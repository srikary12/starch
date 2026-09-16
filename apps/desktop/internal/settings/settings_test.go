package settings

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/srikary12/starch/internal/catalog"
)

func store(t *testing.T) *Store {
	t.Helper()
	return NewStore(filepath.Join(t.TempDir(), FileName))
}

// The macOS shell's compiled-in fallback went stale and named a model two
// generations old, because it was a separate copy of this table. A Go shell in
// the same repository links the real one, so that cannot happen here — this
// test is what keeps it linked rather than copied.
func TestDefaultsComeFromTheRealCatalog(t *testing.T) {
	prefs := Default()
	built := catalog.Builtin()

	provider := FindProvider(built, prefs.Provider)
	if provider == nil {
		t.Fatalf("default provider %q is not in the catalog", prefs.Provider)
	}
	if prefs.BaseURL != provider.Endpoints[0].URL {
		t.Errorf("base URL = %q, want the provider's own endpoint %q",
			prefs.BaseURL, provider.Endpoints[0].URL)
	}
	if FindModel(built, prefs.Provider, prefs.BaseURL, prefs.Model) == nil {
		t.Errorf("default model %q is not one this endpoint serves", prefs.Model)
	}
	if prefs.PresetID != DefaultPreset {
		t.Errorf("preset = %q", prefs.PresetID)
	}
}

func TestSaveAndLoadRoundTrip(t *testing.T) {
	s := store(t)
	want := Default()
	want.Model = "some-model-released-after-this-build"
	want.ThinkingEffort = "high"
	want.HotKey = HotKey{ModCtrl | ModAlt, "j"}
	want.Debug = true

	if err := s.Save(want); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, err := s.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got != want {
		t.Errorf("loaded %+v, want %+v", got, want)
	}
}

func TestLoadWithNoFileIsDefaults(t *testing.T) {
	got, err := store(t).Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got != Default() {
		t.Errorf("loaded %+v, want the defaults", got)
	}
}

// A file written by an older build is missing whatever was added since. Zeroing
// those fields is the mistake that silently reverts everyone's settings on
// upgrade, so absent means "keep the default", not "empty".
func TestOlderFilesKeepTheDefaultsForWhatTheyLack(t *testing.T) {
	s := store(t)
	if err := os.MkdirAll(filepath.Dir(s.Path()), 0o700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(s.Path(), []byte(`{"model":"claude-opus-5"}`), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	got, err := s.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.Model != "claude-opus-5" {
		t.Errorf("model = %q, want the stored one", got.Model)
	}
	if got.HotKey != DefaultHotKey {
		t.Errorf("hotkey = %v, want the default rather than nothing", got.HotKey)
	}
	if got.PresetID != DefaultPreset {
		t.Errorf("preset = %q, want the default rather than empty", got.PresetID)
	}
	if got.Provider != Default().Provider {
		t.Errorf("provider = %q, want the default", got.Provider)
	}
}

// Corrupt settings must not stop the shell starting. They are reported and
// replaced, because a user who cannot launch cannot fix anything.
func TestCorruptSettingsStillYieldUsableDefaults(t *testing.T) {
	s := store(t)
	if err := os.MkdirAll(filepath.Dir(s.Path()), 0o700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(s.Path(), []byte(`{"model": `), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	got, err := s.Load()
	if err == nil {
		t.Fatal("Load reported no problem for a truncated file")
	}
	if got != Default() {
		t.Errorf("loaded %+v, want usable defaults alongside the error", got)
	}
}

// A shortcut from a hand edit or an older rule set must not be grabbed.
func TestAnUnsafeStoredShortcutIsRefused(t *testing.T) {
	s := store(t)
	if err := os.MkdirAll(filepath.Dir(s.Path()), 0o700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(s.Path(), []byte(`{"hotkey":"Ctrl+C"}`), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	got, err := s.Load()
	if err == nil {
		t.Fatal("Load accepted Ctrl+C, which would stop copy working everywhere")
	}
	if !strings.Contains(err.Error(), "Ctrl+C") {
		t.Errorf("error = %v, want it to name the shortcut", err)
	}
	if got.HotKey != DefaultHotKey {
		t.Errorf("hotkey = %v, want the default", got.HotKey)
	}
}

// The settings file sits next to presets.json in the user's config directory
// and is written with the same care: an interrupted save must not truncate it.
func TestSaveIsAtomicAndPrivate(t *testing.T) {
	s := store(t)
	if err := s.Save(Default()); err != nil {
		t.Fatalf("Save: %v", err)
	}

	assertOwnerOnly(t, s.Path())

	// Saving again must leave nothing behind: a stray temporary file in the
	// config directory is the visible symptom of a save that did not finish.
	if err := s.Save(Default()); err != nil {
		t.Fatalf("Save: %v", err)
	}
	entries, err := os.ReadDir(filepath.Dir(s.Path()))
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(entries) != 1 {
		names := make([]string, len(entries))
		for i, e := range entries {
			names[i] = e.Name()
		}
		t.Errorf("directory holds %v, want only the settings file", names)
	}
}

// Switching provider adopts that provider's own endpoint. Keeping the previous
// one sounds protective and is not: pointing Anthropic at an OpenAI host
// cannot work, and the failure arrives later from the wrong place.
func TestUseProviderAdoptsTheNewEndpoint(t *testing.T) {
	built := catalog.Builtin()
	if len(built.Providers) < 2 {
		t.Skip("need two providers to switch between")
	}

	prefs := Default()
	first, second := built.Providers[0], built.Providers[1]
	prefs.BaseURL = "https://a-gateway-i-typed-myself.example"

	prefs.UseProvider(built, second.ID)
	if prefs.BaseURL != second.Endpoints[0].URL {
		t.Errorf("base URL = %q, want %q", prefs.BaseURL, second.Endpoints[0].URL)
	}
	if prefs.Model != second.Endpoints[0].DefaultModel {
		t.Errorf("model = %q, want %q", prefs.Model, second.Endpoints[0].DefaultModel)
	}

	prefs.UseProvider(built, first.ID)
	if prefs.BaseURL != first.Endpoints[0].URL {
		t.Errorf("switching back gave %q, want %q", prefs.BaseURL, first.Endpoints[0].URL)
	}
}

// An effort level belongs to a model. Carrying one across to a model that does
// not take it earns a rejection from the endpoint, in its own words, later.
func TestSwitchingProviderDropsAnEffortTheNewModelMayNotAccept(t *testing.T) {
	built := catalog.Builtin()
	prefs := Default()
	prefs.ThinkingEffort = "xhigh"

	for _, provider := range built.Providers {
		prefs.UseProvider(built, provider.ID)
		model := FindModel(built, prefs.Provider, prefs.BaseURL, prefs.Model)
		switch {
		case model == nil || model.Thinking == nil:
			if prefs.ThinkingEffort != "" {
				t.Errorf("%s: effort = %q, want none for a model with no reasoning controls",
					provider.ID, prefs.ThinkingEffort)
			}
		default:
			if !containsEffort(model.Thinking.Levels, prefs.ThinkingEffort) {
				t.Errorf("%s: effort %q is not one %q accepts (%v)",
					provider.ID, prefs.ThinkingEffort, prefs.Model, model.Thinking.Levels)
			}
		}
	}
}

func containsEffort(levels []catalog.Effort, want string) bool {
	for _, l := range levels {
		if string(l) == want {
			return true
		}
	}
	return false
}

// A model the user typed is used exactly as typed. The catalog is a
// convenience, never a whitelist, or a stale table locks someone out of a
// model their endpoint serves.
func TestATypedModelIsNotRejected(t *testing.T) {
	built := catalog.Builtin()
	prefs := Default()
	prefs.Model = "a-model-shipped-after-this-build"

	if FindModel(built, prefs.Provider, prefs.BaseURL, prefs.Model) != nil {
		t.Fatal("the fixture model is somehow in the catalog")
	}
	prefs.UseDefaultEffort(built)
	if prefs.ThinkingEffort != "" {
		t.Errorf("effort = %q, want none for a model we know nothing about", prefs.ThinkingEffort)
	}

	s := store(t)
	if err := s.Save(prefs); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, err := s.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.Model != prefs.Model {
		t.Errorf("model = %q, want it stored exactly as typed", got.Model)
	}
}

func TestDefaultPathIsInTheConfigDirectory(t *testing.T) {
	path, err := DefaultPath()
	if err != nil {
		t.Fatalf("DefaultPath: %v", err)
	}
	if filepath.Base(path) != FileName {
		t.Errorf("path = %q", path)
	}
}

// Two different situations with two different fixes. Collapsing them sends
// someone who has chosen Ollama back to re-pick the provider they just picked.
func TestProblemTellsTheTwoMissingPiecesApart(t *testing.T) {
	built := catalog.Builtin()

	if err := Default().Problem(built); err != nil {
		t.Errorf("the defaults are not usable: %v", err)
	}

	var nothing Preferences
	if err := nothing.Problem(built); !errors.Is(err, ErrNoProvider) {
		t.Errorf("Problem = %v, want ErrNoProvider", err)
	}

	prefs := Default()
	prefs.UseProvider(built, "openai_compatible")
	if prefs.Model != "" {
		t.Skip("this endpoint now carries a default model")
	}
	err := prefs.Problem(built)
	if !errors.Is(err, ErrNoModel) {
		t.Fatalf("Problem = %v, want ErrNoModel", err)
	}
	if !strings.Contains(err.Error(), prefs.BaseURL) {
		t.Errorf("%q does not name the endpoint", err)
	}
	// The catalog already holds the sentence written for this moment.
	endpoint := FindEndpoint(built, prefs.Provider, prefs.BaseURL)
	if endpoint == nil || endpoint.Note == "" {
		t.Fatal("the endpoint has neither a default model nor a note")
	}
	if !strings.Contains(err.Error(), endpoint.Note) {
		t.Errorf("%q does not pass on the endpoint's own guidance %q", err, endpoint.Note)
	}
}

// The invariant that makes the message above possible. An endpoint may
// truthfully have no default model — a local Ollama serves whatever that
// machine has pulled — but then it owes the user a sentence saying what to
// type, or choosing it leads to a dead end.
func TestEveryEndpointHasADefaultModelOrSaysWhatToType(t *testing.T) {
	for _, provider := range catalog.Builtin().Providers {
		for _, endpoint := range provider.Endpoints {
			if endpoint.DefaultModel == "" && endpoint.Note == "" {
				t.Errorf("%s / %s has no default model and no note explaining what to type",
					provider.ID, endpoint.URL)
			}
		}
	}
}

func TestFindEndpointFallsBackToTheFirst(t *testing.T) {
	built := catalog.Builtin()
	provider := built.Providers[0]

	if got := FindEndpoint(built, provider.ID, provider.Endpoints[0].URL); got == nil ||
		got.URL != provider.Endpoints[0].URL {
		t.Errorf("exact match = %+v", got)
	}
	// A hand-typed gateway still belongs to the provider that will be asked to
	// reach it, so its note and identity are the provider's first endpoint's.
	if got := FindEndpoint(built, provider.ID, "https://a-gateway.example/v1"); got == nil {
		t.Error("an unlisted endpoint resolved to nothing")
	}
	if got := FindEndpoint(built, "altavista", ""); got != nil {
		t.Errorf("unknown provider resolved to %+v", got)
	}
}
