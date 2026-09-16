package secret

import (
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

// These talk to the real Credential Manager, because there is nothing to stand
// in for it: it is part of the OS, there is no service to fake and no bus to
// intercept. That is fine on a CI runner and fine on a developer's machine as
// long as the entries are unmistakably ours and cleaned up, which is what the
// provider name below is for.
//
// It needs no desktop session, no unlock and no prompt, which is why this is
// one of the few Windows paths CI can genuinely verify.

func testAccount(t *testing.T) string {
	t.Helper()
	provider := fmt.Sprintf("starch-test-%d-%d", os.Getpid(), time.Now().UnixNano())
	account := Account(provider)
	t.Cleanup(func() {
		store, err := Open()
		if err != nil {
			t.Errorf("Open during cleanup: %v", err)
			return
		}
		defer store.Close()
		if err := store.Delete(account); err != nil {
			t.Errorf("leaving %s behind in Credential Manager: %v", target(account), err)
		}
	})
	return account
}

func openStore(t *testing.T) *Store {
	t.Helper()
	store, err := Open()
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func TestSetGetDelete(t *testing.T) {
	store := openStore(t)
	account := testAccount(t)

	if _, found, err := store.Get(account); err != nil || found {
		t.Fatalf("Get before Set = found %v, err %v", found, err)
	}
	if has, err := store.Has(account); err != nil || has {
		t.Fatalf("Has before Set = %v, err %v", has, err)
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
	if has, err := store.Has(account); err != nil || !has {
		t.Errorf("Has = %v, err %v", has, err)
	}

	if err := store.Delete(account); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, found, _ := store.Get(account); found {
		t.Error("the key survived Delete")
	}
	// Deleting what is not there is how a settings command clears a key whose
	// state it cannot know. It must not fail.
	if err := store.Delete(account); err != nil {
		t.Errorf("second Delete: %v", err)
	}
}

// Re-entering a key must replace the entry rather than add another, or the
// control panel fills up with entries nobody can tell apart.
func TestSetTwiceReplaces(t *testing.T) {
	store := openStore(t)
	account := testAccount(t)

	if err := store.Set(account, "first"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if err := store.Set(account, "second"); err != nil {
		t.Fatalf("Set again: %v", err)
	}

	value, found, err := store.Get(account)
	if err != nil || !found {
		t.Fatalf("Get: found %v, err %v", found, err)
	}
	if value != "second" {
		t.Errorf("value = %q, want the newer one", value)
	}
}

// One entry per provider, so configuring a second does not throw away the
// first one's key.
func TestProvidersDoNotClobberEachOther(t *testing.T) {
	store := openStore(t)
	first, second := testAccount(t), testAccount(t)

	if err := store.Set(first, "sk-ant"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if err := store.Set(second, "goog-key"); err != nil {
		t.Fatalf("Set: %v", err)
	}

	for account, want := range map[string]string{first: "sk-ant", second: "goog-key"} {
		got, found, err := store.Get(account)
		if err != nil || !found {
			t.Fatalf("Get(%s): found %v, err %v", account, found, err)
		}
		if got != want {
			t.Errorf("Get(%s) = %q, want %q", account, got, want)
		}
	}
}

// A key over the API's blob limit otherwise fails inside advapi32 with a
// message about nothing in particular. Not a realistic key, but a realistic
// paste of the wrong thing into the prompt.
func TestAnOversizedKeyIsRefusedWithAReadableMessage(t *testing.T) {
	store := openStore(t)
	account := testAccount(t)

	err := store.Set(account, strings.Repeat("x", credMaxBlobSize+1))
	if err == nil {
		t.Fatal("an oversized key was accepted")
	}
	if !strings.Contains(err.Error(), "Credential Manager") {
		t.Errorf("error = %v, want it to name what refused it", err)
	}
	if has, _ := store.Has(account); has {
		t.Error("the oversized key was stored anyway")
	}
}

// The name in the control panel is what someone sees when they go looking for
// what Starch stored, so it names the product rather than only the account.
func TestTargetNamesTheProduct(t *testing.T) {
	got := target(Account("anthropic"))
	if !strings.HasPrefix(got, "Starch/") {
		t.Errorf("target = %q, want it to start with the product name", got)
	}
	if !strings.Contains(got, "anthropic") {
		t.Errorf("target = %q, want it to name the provider", got)
	}
}
