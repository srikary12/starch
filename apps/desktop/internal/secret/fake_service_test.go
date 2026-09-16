//go:build !windows

package secret

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/godbus/dbus/v5"
)

// A private session bus with a stand-in Secret Service on it.
//
// The real thing is GNOME Keyring or KWallet, neither of which exists on a CI
// runner or on the machine this was written on. What can be checked without
// them is the part that is actually ours: the call sequence, the attribute
// matching, and above all the locked-keyring path, which is the one a user
// meets on a fresh login and the one that is easiest to get wrong.

// busConfig is a minimal session bus. The stock session.conf is not present on
// every machine, so the test brings its own rather than depending on the
// desktop it happens to be running on.
const busConfig = `<!DOCTYPE busconfig PUBLIC "-//freedesktop//DTD D-BUS Bus Configuration 1.0//EN"
 "http://www.freedesktop.org/standards/dbus/1.0/busconfig.dtd">
<busconfig>
  <type>session</type>
  <listen>unix:tmpdir=/tmp</listen>
  <policy context="default">
    <allow send_destination="*"/>
    <allow own="*"/>
    <allow receive_sender="*"/>
  </policy>
</busconfig>
`

// startBus runs a private dbus-daemon and returns its address.
func startBus(t *testing.T) string {
	t.Helper()

	daemon, err := exec.LookPath("dbus-daemon")
	if err != nil {
		t.Skip("dbus-daemon is not installed; this test needs a session bus to talk to")
	}

	dir, err := os.MkdirTemp("", "st")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })

	configPath := filepath.Join(dir, "bus.conf")
	if err := os.WriteFile(configPath, []byte(busConfig), 0o600); err != nil {
		t.Fatalf("writing the bus config: %v", err)
	}

	cmd := exec.Command(daemon, "--config-file="+configPath, "--print-address", "--nofork")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("StdoutPipe: %v", err)
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting dbus-daemon: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})

	address, err := bufio.NewReader(stdout).ReadString('\n')
	if err != nil {
		t.Fatalf("reading the bus address: %v", err)
	}
	return strings.TrimSpace(address)
}

// dial opens one peer connection to the bus at address.
func dial(t *testing.T, address string) *dbus.Conn {
	t.Helper()
	conn, err := dbus.Connect(address)
	if err != nil {
		t.Fatalf("connecting to %s: %v", address, err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

// fakeKeyring is a stand-in org.freedesktop.secrets.
type fakeKeyring struct {
	t    *testing.T
	conn *dbus.Conn

	mu      sync.Mutex
	items   map[dbus.ObjectPath]*fakeItem
	next    int
	locked  bool
	dismiss bool
	prompts int
	pending []dbus.ObjectPath
}

type fakeItem struct {
	path       dbus.ObjectPath
	keyring    *fakeKeyring
	attributes map[string]string
	value      []byte
}

const promptPath = dbus.ObjectPath("/org/freedesktop/secrets/prompt/p1")

// publish exports the stand-in on conn and claims the well-known name.
func publish(t *testing.T, conn *dbus.Conn) *fakeKeyring {
	t.Helper()

	k := &fakeKeyring{t: t, conn: conn, items: map[dbus.ObjectPath]*fakeItem{}}

	if err := conn.Export(k, servicePath, serviceInterface); err != nil {
		t.Fatalf("exporting the service: %v", err)
	}
	if err := conn.Export(k, defaultAliased, collectionInterface); err != nil {
		t.Fatalf("exporting the collection: %v", err)
	}
	if err := conn.Export(k, promptPath, promptInterface); err != nil {
		t.Fatalf("exporting the prompt: %v", err)
	}

	reply, err := conn.RequestName(serviceName, dbus.NameFlagDoNotQueue)
	if err != nil {
		t.Fatalf("requesting %s: %v", serviceName, err)
	}
	if reply != dbus.RequestNameReplyPrimaryOwner {
		t.Fatalf("could not own %s (reply %v)", serviceName, reply)
	}
	return k
}

// lock makes the keyring behave like a login keyring nobody has unlocked yet.
func (k *fakeKeyring) lock(dismiss bool) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.locked, k.dismiss = true, dismiss
}

func (k *fakeKeyring) promptCount() int {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.prompts
}

func (k *fakeKeyring) itemCount() int {
	k.mu.Lock()
	defer k.mu.Unlock()
	return len(k.items)
}

// OpenSession implements org.freedesktop.Secret.Service.OpenSession.
func (k *fakeKeyring) OpenSession(algorithm string, input dbus.Variant) (dbus.Variant, dbus.ObjectPath, *dbus.Error) {
	if algorithm != "plain" {
		return dbus.Variant{}, "/", dbus.NewError("org.freedesktop.DBus.Error.NotSupported",
			[]any{"only plain is implemented here"})
	}
	return dbus.MakeVariant(""), "/org/freedesktop/secrets/session/s1", nil
}

// SearchItems implements org.freedesktop.Secret.Service.SearchItems.
func (k *fakeKeyring) SearchItems(query map[string]string) ([]dbus.ObjectPath, []dbus.ObjectPath, *dbus.Error) {
	k.mu.Lock()
	defer k.mu.Unlock()

	var found []dbus.ObjectPath
	for path, item := range k.items {
		if matches(item.attributes, query) {
			found = append(found, path)
		}
	}
	if k.locked {
		return nil, found, nil
	}
	return found, nil, nil
}

func matches(have, query map[string]string) bool {
	for name, want := range query {
		if have[name] != want {
			return false
		}
	}
	return true
}

// Unlock implements org.freedesktop.Secret.Service.Unlock.
func (k *fakeKeyring) Unlock(objects []dbus.ObjectPath) ([]dbus.ObjectPath, dbus.ObjectPath, *dbus.Error) {
	k.mu.Lock()
	locked := k.locked
	k.mu.Unlock()

	if !locked {
		return objects, "/", nil
	}
	// A real keyring answers with a prompt the user has to satisfy.
	k.rememberUnlock(objects)
	return nil, promptPath, nil
}

func (k *fakeKeyring) rememberUnlock(objects []dbus.ObjectPath) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.pending = objects
}

// Prompt implements org.freedesktop.Secret.Prompt.Prompt.
func (k *fakeKeyring) Prompt(windowID string) *dbus.Error {
	k.mu.Lock()
	k.prompts++
	dismissed := k.dismiss
	pending := k.pending
	if !dismissed {
		k.locked = false
	}
	k.mu.Unlock()

	// Answered asynchronously, as a real prompt is: the reply to Prompt()
	// comes back immediately and the outcome arrives as a signal.
	go func() {
		time.Sleep(10 * time.Millisecond)
		if err := k.conn.Emit(promptPath, promptInterface+".Completed",
			dismissed, dbus.MakeVariant(pending)); err != nil {
			k.t.Errorf("emitting Completed: %v", err)
		}
	}()
	return nil
}

// CreateItem implements org.freedesktop.Secret.Collection.CreateItem.
func (k *fakeKeyring) CreateItem(properties map[string]dbus.Variant, secret secretValue, replace bool) (dbus.ObjectPath, dbus.ObjectPath, *dbus.Error) {
	attrs, _ := properties[itemInterface+".Attributes"].Value().(map[string]string)

	k.mu.Lock()
	if replace {
		for path, item := range k.items {
			if matches(item.attributes, attrs) {
				item.value = secret.Value
				k.mu.Unlock()
				return path, "/", nil
			}
		}
	}
	k.next++
	path := dbus.ObjectPath(fmt.Sprintf("/org/freedesktop/secrets/collection/login/i%d", k.next))
	item := &fakeItem{path: path, keyring: k, attributes: attrs, value: secret.Value}
	k.items[path] = item
	k.mu.Unlock()

	if err := k.conn.Export(item, path, itemInterface); err != nil {
		return "/", "/", dbus.MakeFailedError(err)
	}
	return path, "/", nil
}

// GetSecret implements org.freedesktop.Secret.Item.GetSecret.
func (i *fakeItem) GetSecret(session dbus.ObjectPath) (secretValue, *dbus.Error) {
	i.keyring.mu.Lock()
	defer i.keyring.mu.Unlock()
	return secretValue{
		Session:     session,
		Value:       i.value,
		ContentType: "text/plain; charset=utf8",
	}, nil
}

// Delete implements org.freedesktop.Secret.Item.Delete.
func (i *fakeItem) Delete() (dbus.ObjectPath, *dbus.Error) {
	i.keyring.mu.Lock()
	delete(i.keyring.items, i.path)
	i.keyring.mu.Unlock()
	return "/", nil
}
