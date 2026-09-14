package secret

import (
	"errors"
	"strings"
	"testing"
)

func openStore(t *testing.T) (*Store, *fakeKeyring) {
	t.Helper()

	address := startBus(t)
	keyring := publish(t, dial(t, address))

	// A second connection, so the client is a real bus peer of the service
	// rather than reaching an object inside its own process.
	store, err := OpenOn(dial(t, address))
	if err != nil {
		t.Fatalf("OpenOn: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store, keyring
}

func TestSetGetDelete(t *testing.T) {
	store, _ := openStore(t)
	account := Account("anthropic")

	if _, found, err := store.Get(account); err != nil || found {
		t.Fatalf("Get before Set = found %v, err %v", found, err)
	}

	if err := store.Set(account, "sk-ant-secret"); err != nil {
		t.Fatalf("Set: %v", err)
	}

	value, found, err := store.Get(account)
	if err != nil || !found {
		t.Fatalf("Get = %q, found %v, err %v", value, found, err)
	}
	if value != "sk-ant-secret" {
		t.Errorf("value = %q", value)
	}

	has, err := store.Has(account)
	if err != nil || !has {
		t.Errorf("Has = %v, err %v", has, err)
	}

	if err := store.Delete(account); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, found, _ := store.Get(account); found {
		t.Error("the key survived Delete")
	}
	// Deleting what is not there is how a settings command clears a key it
	// cannot know the state of. It must not fail.
	if err := store.Delete(account); err != nil {
		t.Errorf("second Delete: %v", err)
	}
}

// Re-entering a key must replace the entry rather than add another. A keyring
// accumulating one item per attempt is how "which of these is current?"
// becomes a support question.
func TestSetTwiceReplaces(t *testing.T) {
	store, keyring := openStore(t)
	account := Account("gemini")

	if err := store.Set(account, "first"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if err := store.Set(account, "second"); err != nil {
		t.Fatalf("Set again: %v", err)
	}

	if n := keyring.itemCount(); n != 1 {
		t.Errorf("keyring holds %d items, want 1", n)
	}
	if value, _, _ := store.Get(account); value != "second" {
		t.Errorf("value = %q, want the newer one", value)
	}
}

// One entry per provider, so configuring a second one does not throw away the
// first one's key. Switching back should not mean finding the key again.
func TestProvidersDoNotClobberEachOther(t *testing.T) {
	store, _ := openStore(t)

	if err := store.Set(Account("anthropic"), "sk-ant"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if err := store.Set(Account("gemini"), "goog-key"); err != nil {
		t.Fatalf("Set: %v", err)
	}

	for account, want := range map[string]string{
		Account("anthropic"): "sk-ant",
		Account("gemini"):    "goog-key",
	} {
		got, found, err := store.Get(account)
		if err != nil || !found {
			t.Fatalf("Get(%s): found %v, err %v", account, found, err)
		}
		if got != want {
			t.Errorf("Get(%s) = %q, want %q", account, got, want)
		}
	}
}

// A login keyring is locked on a fresh session, which is the state a first
// launch actually meets. Both writing and reading have to drive the unlock
// prompt rather than failing in a way that reads as a bug.
func TestALockedKeyringIsUnlocked(t *testing.T) {
	store, keyring := openStore(t)
	account := Account("anthropic")

	keyring.lock(false)
	if err := store.Set(account, "sk-locked"); err != nil {
		t.Fatalf("Set against a locked keyring: %v", err)
	}
	if keyring.promptCount() == 0 {
		t.Error("no prompt was shown, so the write went into a locked collection")
	}

	keyring.lock(false)
	value, found, err := store.Get(account)
	if err != nil || !found {
		t.Fatalf("Get against a locked keyring: found %v, err %v", found, err)
	}
	if value != "sk-locked" {
		t.Errorf("value = %q", value)
	}
}

// Someone who closes the password dialog has declined, and that is not the
// same as an empty key or a broken keyring. It has to be said plainly.
func TestADismissedPromptIsReportedAsSuch(t *testing.T) {
	store, keyring := openStore(t)
	keyring.lock(true)

	err := store.Set(Account("anthropic"), "sk-never-stored")
	if !errors.Is(err, ErrDismissed) {
		t.Fatalf("Set = %v, want ErrDismissed", err)
	}
}

// Has answers without unlocking, so a settings command can say "key saved"
// without putting a password dialog in front of someone who only wanted to
// look at their configuration.
func TestHasDoesNotUnlock(t *testing.T) {
	store, keyring := openStore(t)
	account := Account("anthropic")

	if err := store.Set(account, "sk-ant"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	keyring.lock(true) // Any prompt would now be dismissed, and so would fail.

	has, err := store.Has(account)
	if err != nil {
		t.Fatalf("Has: %v", err)
	}
	if !has {
		t.Error("Has = false for a key that is in the keyring, merely locked")
	}
}

// A machine with no keyring at all gets a sentence telling it what to install,
// not a D-Bus error nobody can act on.
func TestNoKeyringIsAnExplanation(t *testing.T) {
	_, err := OpenOn(dial(t, startBus(t)))
	if !errors.Is(err, ErrNoService) {
		t.Fatalf("OpenOn with no service = %v, want ErrNoService", err)
	}
	for _, name := range []string{"GNOME Keyring", "KWallet", "KeePassXC"} {
		if !strings.Contains(err.Error(), name) {
			t.Errorf("error %q does not mention %s", err, name)
		}
	}
}

func TestAccountNamesAreProviderScoped(t *testing.T) {
	if Account("anthropic") == Account("gemini") {
		t.Fatal("two providers share one account name")
	}
	if !strings.Contains(Account("anthropic"), "anthropic") {
		t.Errorf("Account = %q, want it to name the provider", Account("anthropic"))
	}
}
