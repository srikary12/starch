//go:build !windows

// The freedesktop Secret Service implementation, used on Linux and on any other
// Unix with a keyring on its session bus. Windows has its own file.
//
// org.freedesktop.secrets is provided by GNOME Keyring, by KWallet, and by
// KeePassXC, which covers essentially every desktop. There is no fallback to a
// file: a key sitting in a dotfile would quietly break the guarantee the README
// makes, and a machine with no keyring is better told so.
//
// The session is opened with the "plain" algorithm, so the key crosses the
// session bus unencrypted. That bus is a Unix socket owned by this user, and
// the threat model in the README is explicit that nothing here defends against
// code already running as the user — such code can read the daemon's
// environment or attach to its process. The Secret Service also offers a
// Diffie-Hellman session that would close the narrower window where another of
// the user's own processes is monitoring the bus during the transfer; it is
// worth adding if that window ever matters.
package secret

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/godbus/dbus/v5"
	"github.com/srikary12/starch/internal/brand"
)

const (
	serviceName = "org.freedesktop.secrets"

	servicePath    = dbus.ObjectPath("/org/freedesktop/secrets")
	defaultAliased = dbus.ObjectPath("/org/freedesktop/secrets/aliases/default")

	serviceInterface    = "org.freedesktop.Secret.Service"
	collectionInterface = "org.freedesktop.Secret.Collection"
	itemInterface       = "org.freedesktop.Secret.Item"
	promptInterface     = "org.freedesktop.Secret.Prompt"
)

// callTimeout bounds an ordinary Secret Service call. Unlocking is excluded:
// that one waits for a person to type a password.
const callTimeout = 10 * time.Second

// promptTimeout bounds how long we wait for the user to answer a keyring
// prompt. Long, because typing a passphrase is exactly what it is waiting for,
// but not unbounded — a prompt nobody ever sees would hang the shell.
const promptTimeout = 2 * time.Minute

// ErrNoService reports that nothing on the session bus provides a keyring.
var ErrNoService = errors.New(
	"no keyring is running. GNOME Keyring, KWallet and KeePassXC all provide one; " +
		"starting yours and trying again is the fix")

// ErrDismissed reports that the user closed a keyring prompt without answering.
var ErrDismissed = errors.New("the keyring prompt was dismissed")

// Store is a connection to the desktop's Secret Service.
type Store struct {
	conn    *dbus.Conn
	closes  bool
	session dbus.ObjectPath
}

// Open connects to the session bus and starts a Secret Service session.
func Open() (*Store, error) {
	conn, err := dbus.SessionBus()
	if err != nil {
		return nil, fmt.Errorf("connecting to the session bus: %w", err)
	}
	store, err := open(conn, false)
	if err != nil {
		return nil, err
	}
	return store, nil
}

// OpenOn starts a session on an existing connection. Tests use this to reach a
// private bus; nothing else should need it.
func OpenOn(conn *dbus.Conn) (*Store, error) { return open(conn, false) }

func open(conn *dbus.Conn, closes bool) (*Store, error) {
	s := &Store{conn: conn, closes: closes}

	ctx, cancel := context.WithTimeout(context.Background(), callTimeout)
	defer cancel()

	var output dbus.Variant
	var session dbus.ObjectPath
	err := s.service().CallWithContext(ctx, serviceInterface+".OpenSession", 0,
		"plain", dbus.MakeVariant("")).Store(&output, &session)
	if err != nil {
		if isUnknownService(err) {
			return nil, ErrNoService
		}
		return nil, fmt.Errorf("opening a keyring session: %w", err)
	}

	s.session = session
	return s, nil
}

// Close releases the Secret Service session.
func (s *Store) Close() error {
	if s.session != "" && s.session != "/" {
		ctx, cancel := context.WithTimeout(context.Background(), callTimeout)
		defer cancel()
		_ = s.conn.Object(serviceName, s.session).
			CallWithContext(ctx, "org.freedesktop.Secret.Session.Close", 0).Err
		s.session = ""
	}
	if s.closes {
		return s.conn.Close()
	}
	return nil
}

func (s *Store) service() dbus.BusObject { return s.conn.Object(serviceName, servicePath) }

// attributes identify one of our items. They are also what SearchItems matches
// on, so they have to be exact rather than descriptive.
func attributes(account string) map[string]string {
	return map[string]string{
		"application": brand.Slug,
		"account":     account,
		// Tells GNOME Keyring what kind of item this is. Without it the item
		// still works but is shown in Seahorse without a type.
		"xdg:schema": "org.freedesktop.Secret.Generic",
	}
}

// secretValue is the Secret Service's (session, parameters, value, type)
// struct. Field order is the wire order and must not be rearranged.
type secretValue struct {
	Session     dbus.ObjectPath
	Parameters  []byte
	Value       []byte
	ContentType string
}

// Set stores or replaces the secret for account.
func (s *Store) Set(account, value string) error {
	ctx, cancel := context.WithTimeout(context.Background(), callTimeout)
	defer cancel()

	collection, err := s.unlockedDefaultCollection(ctx)
	if err != nil {
		return err
	}

	properties := map[string]dbus.Variant{
		itemInterface + ".Label":      dbus.MakeVariant(brand.Name + " API key (" + account + ")"),
		itemInterface + ".Attributes": dbus.MakeVariant(attributes(account)),
	}
	secret := secretValue{
		Session:     s.session,
		Value:       []byte(value),
		ContentType: "text/plain; charset=utf8",
	}

	var item, prompt dbus.ObjectPath
	// replace=true: an in-place replacement keeps the item's identity, so the
	// keyring does not accumulate one entry per time the key was re-entered.
	err = s.conn.Object(serviceName, collection).
		CallWithContext(ctx, collectionInterface+".CreateItem", 0, properties, secret, true).
		Store(&item, &prompt)
	if err != nil {
		return fmt.Errorf("storing the key in the keyring: %w", err)
	}
	if _, err := s.resolve(prompt); err != nil {
		return err
	}
	return nil
}

// Get returns the secret for account. The boolean reports whether one exists,
// which is different from an empty one.
func (s *Store) Get(account string) (string, bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), callTimeout)
	defer cancel()

	item, err := s.find(ctx, account)
	if err != nil || item == "" {
		return "", false, err
	}

	var secret secretValue
	err = s.conn.Object(serviceName, item).
		CallWithContext(ctx, itemInterface+".GetSecret", 0, s.session).Store(&secret)
	if err != nil {
		return "", false, fmt.Errorf("reading the key from the keyring: %w", err)
	}
	return string(secret.Value), true, nil
}

// Has reports whether a secret exists for account, without reading it.
//
// The settings command uses this to say "key saved" without unlocking
// anything, which on a locked keyring is the difference between a status line
// and a password prompt.
func (s *Store) Has(account string) (bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), callTimeout)
	defer cancel()

	var unlocked, locked []dbus.ObjectPath
	err := s.service().CallWithContext(ctx, serviceInterface+".SearchItems", 0, attributes(account)).
		Store(&unlocked, &locked)
	if err != nil {
		return false, fmt.Errorf("searching the keyring: %w", err)
	}
	return len(unlocked)+len(locked) > 0, nil
}

// Delete removes the secret for account. Absent is not an error.
func (s *Store) Delete(account string) error {
	ctx, cancel := context.WithTimeout(context.Background(), callTimeout)
	defer cancel()

	item, err := s.find(ctx, account)
	if err != nil || item == "" {
		return err
	}

	var prompt dbus.ObjectPath
	if err := s.conn.Object(serviceName, item).
		CallWithContext(ctx, itemInterface+".Delete", 0).Store(&prompt); err != nil {
		return fmt.Errorf("removing the key from the keyring: %w", err)
	}
	_, err = s.resolve(prompt)
	return err
}

// find returns the item holding account's secret, unlocking it if necessary.
func (s *Store) find(ctx context.Context, account string) (dbus.ObjectPath, error) {
	var unlocked, locked []dbus.ObjectPath
	err := s.service().CallWithContext(ctx, serviceInterface+".SearchItems", 0, attributes(account)).
		Store(&unlocked, &locked)
	if err != nil {
		return "", fmt.Errorf("searching the keyring: %w", err)
	}
	if len(unlocked) > 0 {
		return unlocked[0], nil
	}
	if len(locked) == 0 {
		return "", nil
	}

	opened, err := s.unlock(ctx, locked[:1])
	if err != nil {
		return "", err
	}
	if len(opened) == 0 {
		return "", ErrDismissed
	}
	return opened[0], nil
}

// unlockedDefaultCollection returns the default collection, unlocking it first.
// A login keyring is routinely locked, and writing into a locked collection
// fails in a way that reads as a bug rather than as "type your password".
func (s *Store) unlockedDefaultCollection(ctx context.Context) (dbus.ObjectPath, error) {
	opened, err := s.unlock(ctx, []dbus.ObjectPath{defaultAliased})
	if err != nil {
		return "", err
	}
	if len(opened) == 0 {
		return "", ErrDismissed
	}
	return opened[0], nil
}

// unlock unlocks objects, waiting for the user if the service asks.
func (s *Store) unlock(ctx context.Context, objects []dbus.ObjectPath) ([]dbus.ObjectPath, error) {
	var opened []dbus.ObjectPath
	var prompt dbus.ObjectPath
	err := s.service().CallWithContext(ctx, serviceInterface+".Unlock", 0, objects).
		Store(&opened, &prompt)
	if err != nil {
		return nil, fmt.Errorf("unlocking the keyring: %w", err)
	}
	if len(opened) > 0 {
		return opened, nil
	}

	result, err := s.resolve(prompt)
	if err != nil {
		return nil, err
	}
	if result.Signature().String() == "ao" {
		if paths, ok := result.Value().([]dbus.ObjectPath); ok {
			return paths, nil
		}
	}
	return opened, nil
}

// resolve completes a prompt, if the call returned one. "/" means there was
// nothing to ask.
func (s *Store) resolve(prompt dbus.ObjectPath) (dbus.Variant, error) {
	if prompt == "" || prompt == "/" {
		return dbus.Variant{}, nil
	}

	signals := make(chan *dbus.Signal, 8)
	s.conn.Signal(signals)
	defer s.conn.RemoveSignal(signals)

	match := []dbus.MatchOption{
		dbus.WithMatchObjectPath(prompt),
		dbus.WithMatchInterface(promptInterface),
		dbus.WithMatchMember("Completed"),
	}
	if err := s.conn.AddMatchSignal(match...); err != nil {
		return dbus.Variant{}, fmt.Errorf("watching the keyring prompt: %w", err)
	}
	defer func() { _ = s.conn.RemoveMatchSignal(match...) }()

	ctx, cancel := context.WithTimeout(context.Background(), callTimeout)
	defer cancel()
	// The empty window id means "no parent window". The shell has no
	// addressable X window at the moment a key is being saved from the
	// terminal, and the keyring centres its dialog rather than failing.
	if err := s.conn.Object(serviceName, prompt).
		CallWithContext(ctx, promptInterface+".Prompt", 0, "").Err; err != nil {
		return dbus.Variant{}, fmt.Errorf("showing the keyring prompt: %w", err)
	}

	deadline := time.After(promptTimeout)
	for {
		select {
		case signal := <-signals:
			if signal.Path != prompt || signal.Name != promptInterface+".Completed" {
				continue
			}
			var dismissed bool
			var result dbus.Variant
			if err := dbus.Store(signal.Body, &dismissed, &result); err != nil {
				return dbus.Variant{}, fmt.Errorf("reading the prompt result: %w", err)
			}
			if dismissed {
				return dbus.Variant{}, ErrDismissed
			}
			return result, nil
		case <-deadline:
			return dbus.Variant{}, errors.New("the keyring prompt went unanswered")
		}
	}
}

// isUnknownService reports whether err is the bus saying nothing provides the
// Secret Service, as opposed to the service itself refusing.
func isUnknownService(err error) bool {
	var dbusErr dbus.Error
	if !errors.As(err, &dbusErr) {
		return false
	}
	switch dbusErr.Name {
	case "org.freedesktop.DBus.Error.ServiceUnknown",
		"org.freedesktop.DBus.Error.NameHasNoOwner",
		"org.freedesktop.DBus.Error.Spawn.ServiceNotFound":
		return true
	}
	return false
}
