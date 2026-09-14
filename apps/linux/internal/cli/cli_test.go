package cli

import (
	"bytes"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/srikary12/starch/apps/linux/internal/secret"
	"github.com/srikary12/starch/apps/linux/internal/settings"
	"github.com/srikary12/starch/internal/catalog"
)

type fakeSecrets struct {
	values  map[string]string
	openErr error
	closed  bool
}

func newFakeSecrets() *fakeSecrets { return &fakeSecrets{values: map[string]string{}} }

func (f *fakeSecrets) Set(account, value string) error {
	f.values[account] = value
	return nil
}

func (f *fakeSecrets) Get(account string) (string, bool, error) {
	value, ok := f.values[account]
	return value, ok, nil
}

func (f *fakeSecrets) Has(account string) (bool, error) {
	_, ok := f.values[account]
	return ok, nil
}

func (f *fakeSecrets) Delete(account string) error {
	delete(f.values, account)
	return nil
}

func (f *fakeSecrets) open() (SecretStore, func() error, error) {
	if f.openErr != nil {
		return nil, func() error { return nil }, f.openErr
	}
	return f, func() error { f.closed = true; return nil }, nil
}

type harness struct {
	env     Env
	out     *bytes.Buffer
	errOut  *bytes.Buffer
	secrets *fakeSecrets
	typed   string
}

func newHarness(t *testing.T) *harness {
	t.Helper()

	h := &harness{
		out:     &bytes.Buffer{},
		errOut:  &bytes.Buffer{},
		secrets: newFakeSecrets(),
		typed:   "sk-typed-at-the-prompt",
	}
	h.env = Env{
		Stdout:      h.out,
		Stderr:      h.errOut,
		Store:       settings.NewStore(filepath.Join(t.TempDir(), settings.FileName)),
		Catalog:     catalog.Builtin(),
		OpenSecrets: h.secrets.open,
		ReadSecret:  func(string) (string, error) { return h.typed, nil },
	}
	return h
}

func (h *harness) run(t *testing.T, args ...string) error {
	t.Helper()
	h.out.Reset()
	h.errOut.Reset()
	return Run(args, h.env)
}

func (h *harness) prefs(t *testing.T) settings.Preferences {
	t.Helper()
	prefs, err := h.env.Store.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return prefs
}

func TestConfigWithNoFlagsPrintsTheConfiguration(t *testing.T) {
	h := newHarness(t)
	if err := h.run(t); err != nil {
		t.Fatalf("Run: %v", err)
	}

	out := h.out.String()
	prefs := settings.Default()
	for _, want := range []string{prefs.Provider, prefs.Model, prefs.BaseURL, prefs.HotKey.String()} {
		if !strings.Contains(out, want) {
			t.Errorf("output does not mention %q:\n%s", want, out)
		}
	}
	// Where the file is, because the next thing someone wants to do is edit it.
	if !strings.Contains(out, h.env.Store.Path()) {
		t.Errorf("output does not say where settings live:\n%s", out)
	}
}

// A stored key is reported as present and never printed. This is the test that
// would fail if someone ever reached for Get here instead of Has.
func TestConfigNeverPrintsTheKey(t *testing.T) {
	h := newHarness(t)
	h.secrets.values[secret.Account(settings.Default().Provider)] = "sk-super-secret"

	if err := h.run(t); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if strings.Contains(h.out.String(), "sk-super-secret") {
		t.Fatalf("the API key was printed:\n%s", h.out.String())
	}
	if !strings.Contains(h.out.String(), "saved in the keyring") {
		t.Errorf("output does not say a key is stored:\n%s", h.out.String())
	}
}

func TestConfigSetsProviderAndAdoptsItsEndpoint(t *testing.T) {
	h := newHarness(t)
	built := catalog.Builtin()
	if len(built.Providers) < 2 {
		t.Skip("need two providers")
	}
	target := built.Providers[1]

	if err := h.run(t, "--provider", target.ID); err != nil {
		t.Fatalf("Run: %v", err)
	}
	prefs := h.prefs(t)
	if prefs.Provider != target.ID {
		t.Errorf("provider = %q", prefs.Provider)
	}
	if prefs.BaseURL != target.Endpoints[0].URL {
		t.Errorf("endpoint = %q, want the new provider's own %q", prefs.BaseURL, target.Endpoints[0].URL)
	}
	if prefs.Model != target.Endpoints[0].DefaultModel {
		t.Errorf("model = %q", prefs.Model)
	}
}

func TestConfigRejectsAnUnknownProvider(t *testing.T) {
	h := newHarness(t)
	err := h.run(t, "--provider", "altavista")
	if err == nil {
		t.Fatal("an unknown provider was accepted; the daemon would reject it with a 400")
	}
	for _, provider := range catalog.Builtin().Providers {
		if !strings.Contains(err.Error(), provider.ID) {
			t.Errorf("error %q does not offer %q", err, provider.ID)
		}
	}
}

// The catalog is a convenience, never a whitelist. A model released after this
// build has to remain reachable by typing its name.
func TestConfigAcceptsAModelItHasNeverHeardOf(t *testing.T) {
	h := newHarness(t)
	if err := h.run(t, "--model", "claude-sonnet-7"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := h.prefs(t).Model; got != "claude-sonnet-7" {
		t.Errorf("model = %q, want it stored as typed", got)
	}
}

func TestConfigChecksTheEffortAgainstTheModel(t *testing.T) {
	h := newHarness(t)
	prefs := settings.Default()
	model := settings.FindModel(catalog.Builtin(), prefs.Provider, prefs.BaseURL, prefs.Model)
	if model == nil || model.Thinking == nil {
		t.Skip("the default model has no thinking levels to check against")
	}

	// One it does accept.
	level := string(model.Thinking.Levels[len(model.Thinking.Levels)-1])
	if err := h.run(t, "--effort", level); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := h.prefs(t).ThinkingEffort; got != level {
		t.Errorf("effort = %q, want %q", got, level)
	}

	// One it does not. Better refused here than as an endpoint error later.
	err := h.run(t, "--effort", "ludicrous")
	if err == nil {
		t.Fatal("a level the model does not accept was stored")
	}
	for _, accepted := range model.Thinking.Levels {
		if !strings.Contains(err.Error(), string(accepted)) {
			t.Errorf("error %q does not list %q", err, accepted)
		}
	}

	// And clearing it, which is how you ask for the endpoint's own default.
	if err := h.run(t, "--effort", "off"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := h.prefs(t).ThinkingEffort; got != "" {
		t.Errorf("effort = %q, want it cleared", got)
	}
}

// An unsafe shortcut has to be refused here. Once it is grabbed, the key stops
// working in every other application with no clue as to what took it.
func TestConfigRefusesAnUnsafeShortcut(t *testing.T) {
	h := newHarness(t)
	err := h.run(t, "--hotkey", "Ctrl+C")
	if err == nil {
		t.Fatal("Ctrl+C was accepted as a global shortcut")
	}
	if !strings.Contains(err.Error(), "every application") {
		t.Errorf("error = %v, want it to explain the cost", err)
	}
	if got := h.prefs(t).HotKey; got != settings.DefaultHotKey {
		t.Errorf("hotkey = %v, want it unchanged", got)
	}
}

func TestConfigStoresAndClearsTheKey(t *testing.T) {
	h := newHarness(t)
	account := secret.Account(settings.Default().Provider)

	if err := h.run(t, "--key"); err != nil {
		t.Fatalf("Run --key: %v", err)
	}
	if h.secrets.values[account] != h.typed {
		t.Errorf("stored %q, want the typed key", h.secrets.values[account])
	}
	if strings.Contains(h.out.String(), h.typed) {
		t.Error("the key was echoed back to the terminal")
	}

	if err := h.run(t, "--clear-key"); err != nil {
		t.Fatalf("Run --clear-key: %v", err)
	}
	if _, present := h.secrets.values[account]; present {
		t.Error("the key survived --clear-key")
	}
}

func TestConfigRefusesAnEmptyKey(t *testing.T) {
	h := newHarness(t)
	h.typed = "   "
	if err := h.run(t, "--key"); err == nil {
		t.Fatal("an empty key was stored")
	}
}

// Looking at your settings must not require a keyring. Someone diagnosing a
// broken keyring needs this command most.
func TestConfigWorksWithoutAKeyring(t *testing.T) {
	h := newHarness(t)
	h.secrets.openErr = errors.New("no keyring is running")

	if err := h.run(t); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(h.out.String(), "no keyring is running") {
		t.Errorf("output does not explain the keyring problem:\n%s", h.out.String())
	}
	if !strings.Contains(h.out.String(), settings.Default().Model) {
		t.Errorf("the rest of the configuration was not shown:\n%s", h.out.String())
	}
}

// --list is what a dropdown would have been. It has to say that the list is
// not a restriction, or a stale table reads as one.
func TestListPrintsTheCatalog(t *testing.T) {
	h := newHarness(t)
	if err := h.run(t, "--list"); err != nil {
		t.Fatalf("Run: %v", err)
	}

	out := h.out.String()
	for _, provider := range catalog.Builtin().Providers {
		if !strings.Contains(out, provider.ID) {
			t.Errorf("output omits provider %q", provider.ID)
		}
		for _, endpoint := range provider.Endpoints {
			if !strings.Contains(out, endpoint.URL) {
				t.Errorf("output omits endpoint %q", endpoint.URL)
			}
		}
	}
	if !strings.Contains(out, "not a restriction") {
		t.Errorf("the list does not say a typed model is still accepted:\n%s", out)
	}
}

func TestUnknownFlagIsReported(t *testing.T) {
	h := newHarness(t)
	if err := h.run(t, "--nope"); err == nil {
		t.Fatal("an unknown flag was accepted")
	}
}
