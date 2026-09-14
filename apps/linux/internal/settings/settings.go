// Package settings holds the shell's non-secret preferences.
//
// Nothing sensitive belongs here: this is a JSON file in the user's config
// directory. The API key lives in the Secret Service, in package secret.
package settings

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/srikary12/starch/internal/catalog"
	"github.com/srikary12/starch/internal/config"
)

// FileName is the settings file, alongside the daemon's presets.json.
const FileName = "settings.json"

// DefaultPreset is what a rewrite uses when nothing else is chosen. The daemon
// falls back to the same one, because a user mid-sentence should get a rewrite
// rather than an error about configuration.
const DefaultPreset = "professional"

// Preferences is everything the shell remembers between runs.
type Preferences struct {
	Provider string `json:"provider"`
	Model    string `json:"model"`
	BaseURL  string `json:"base_url"`
	// ThinkingEffort is the level to ask the model for, in the provider's own
	// vocabulary. Empty means do not ask, leaving the endpoint's own default.
	//
	// A plain string rather than an enum: which levels a model accepts comes
	// from the catalog and differs per model, so an enum here would be a
	// second copy of that list, going stale and silently dropping a level the
	// catalog was offering.
	ThinkingEffort string `json:"thinking_effort"`
	HotKey         HotKey `json:"hotkey"`
	PresetID       string `json:"preset"`
	Debug          bool   `json:"debug"`
}

// Default returns the preferences a fresh install starts from.
//
// The provider, endpoint and model come from the linked catalog rather than
// from constants written out here. The macOS shell has to keep its own copy of
// those and it went stale — it was still naming a model two generations old.
// A Go shell in the same repository has no such excuse: this is the same table
// the daemon serves from GET /v1/models, so the seed cannot disagree with it.
func Default() Preferences {
	p := Preferences{
		HotKey:   DefaultHotKey,
		PresetID: DefaultPreset,
	}
	built := catalog.Builtin()
	if len(built.Providers) > 0 {
		p.Provider = built.Providers[0].ID
		p.BaseURL, p.Model = EndpointDefaults(built, p.Provider)
	}
	return p
}

// FindProvider returns the named provider from cat, or nil.
func FindProvider(cat catalog.Catalog, id string) *catalog.Provider {
	for i := range cat.Providers {
		if cat.Providers[i].ID == id {
			return &cat.Providers[i]
		}
	}
	return nil
}

// EndpointDefaults returns the endpoint and model to adopt when a provider is
// chosen.
//
// Always the new provider's own, never the previous one's. Carrying a custom
// endpoint across a provider switch sounds protective and is not: pointing the
// Anthropic provider at api.openai.com cannot work, and the failure arrives
// later as an unexplained error from the wrong host.
func EndpointDefaults(cat catalog.Catalog, providerID string) (baseURL, model string) {
	provider := FindProvider(cat, providerID)
	if provider == nil || len(provider.Endpoints) == 0 {
		return "", ""
	}
	endpoint := provider.Endpoints[0]
	return endpoint.URL, endpoint.DefaultModel
}

// FindModel returns the named model within a provider's endpoint, or nil. A
// typed-in model that is not in the table is not an error — the list is a
// convenience, never a whitelist.
func FindModel(cat catalog.Catalog, providerID, baseURL, modelID string) *catalog.Model {
	provider := FindProvider(cat, providerID)
	if provider == nil {
		return nil
	}
	for i := range provider.Endpoints {
		endpoint := &provider.Endpoints[i]
		if baseURL != "" && endpoint.URL != baseURL {
			continue
		}
		for j := range endpoint.Models {
			if endpoint.Models[j].ID == modelID {
				return &endpoint.Models[j]
			}
		}
	}
	return nil
}

// UseProvider switches to a provider, adopting its endpoint and model and
// dropping a thinking level the new model may not accept.
func (p *Preferences) UseProvider(cat catalog.Catalog, providerID string) {
	p.Provider = providerID
	p.BaseURL, p.Model = EndpointDefaults(cat, providerID)
	p.ThinkingEffort = ""
	p.UseDefaultEffort(cat)
}

// UseDefaultEffort preselects the current model's default thinking level, or
// clears it if the model has no reasoning controls.
func (p *Preferences) UseDefaultEffort(cat catalog.Catalog) {
	model := FindModel(cat, p.Provider, p.BaseURL, p.Model)
	if model == nil || model.Thinking == nil {
		p.ThinkingEffort = ""
		return
	}
	p.ThinkingEffort = string(model.Thinking.Default)
}

// Store reads and writes Preferences as one JSON file.
//
// One file rather than a key per setting, so adding a setting cannot leave a
// half-migrated state behind, and so the whole thing is legible and editable
// by hand — which on Linux people will do whatever we provide.
type Store struct {
	path string
}

// NewStore returns a store backed by the file at path.
func NewStore(path string) *Store { return &Store{path: path} }

// DefaultPath is the settings file in the user's config directory.
func DefaultPath() (string, error) {
	dir, err := config.SupportDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, FileName), nil
}

// Path is where this store reads and writes.
func (s *Store) Path() string { return s.path }

// Load returns the stored preferences.
//
// The returned Preferences are always usable, even when err is non-nil: a file
// that cannot be read or parsed yields the defaults and an error describing
// why. Losing settings beats refusing to start, and the caller is expected to
// report the problem rather than treat it as fatal.
func (s *Store) Load() (Preferences, error) {
	// Start from the defaults and decode over them, so a field added in a
	// later version is simply absent rather than zero — the mistake that
	// silently reverts everyone's settings on upgrade.
	prefs := Default()

	raw, err := os.ReadFile(s.path)
	if errors.Is(err, fs.ErrNotExist) {
		return prefs, nil
	}
	if err != nil {
		return prefs, fmt.Errorf("reading %s: %w", s.path, err)
	}

	if err := json.Unmarshal(raw, &prefs); err != nil {
		return Default(), fmt.Errorf("reading %s: %w", s.path, err)
	}

	if reason := prefs.HotKey.InvalidReason(); reason != "" {
		// A stored shortcut can predate the current rules or come from a hand
		// edit. Grabbing an unsafe one would break that key everywhere, so
		// fall back rather than honour it.
		stored := prefs.HotKey
		prefs.HotKey = DefaultHotKey
		return prefs, fmt.Errorf("shortcut %s cannot be used: %s", stored, reason)
	}
	return prefs, nil
}

// Save writes the preferences.
//
// Written to a temporary file and renamed, so an interrupted write leaves the
// previous settings intact rather than a truncated file that reverts everything
// to defaults on the next start.
func (s *Store) Save(prefs Preferences) error {
	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("creating %s: %w", dir, err)
	}

	encoded, err := json.MarshalIndent(prefs, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding settings: %w", err)
	}
	encoded = append(encoded, '\n')

	temp, err := os.CreateTemp(dir, FileName+".*")
	if err != nil {
		return fmt.Errorf("creating a temporary file in %s: %w", dir, err)
	}
	tempName := temp.Name()
	defer os.Remove(tempName) // No-op once the rename has succeeded.

	if err := temp.Chmod(0o600); err != nil {
		temp.Close()
		return fmt.Errorf("securing %s: %w", tempName, err)
	}
	if _, err := temp.Write(encoded); err != nil {
		temp.Close()
		return fmt.Errorf("writing %s: %w", tempName, err)
	}
	if err := temp.Close(); err != nil {
		return fmt.Errorf("closing %s: %w", tempName, err)
	}
	if err := os.Rename(tempName, s.path); err != nil {
		return fmt.Errorf("replacing %s: %w", s.path, err)
	}
	return nil
}
