package prompt

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	return NewStore(t.TempDir())
}

// The file is meant to be edited, so there has to be something to open. A
// hidden default that only materialises once you guess the format is exactly
// what the brief rules out.
func TestFirstLoadWritesTheDefaults(t *testing.T) {
	store := newTestStore(t)

	set := store.Load()
	if len(set.Presets) != len(Defaults) {
		t.Fatalf("got %d presets, want %d", len(set.Presets), len(Defaults))
	}
	if set.Source != SourceFile {
		t.Errorf("source = %q, want %q — the file should have been created", set.Source, SourceFile)
	}
	if set.Problem != "" {
		t.Errorf("unexpected problem: %s", set.Problem)
	}

	raw, err := os.ReadFile(store.Path())
	if err != nil {
		t.Fatalf("file was not written: %v", err)
	}

	var parsed fileFormat
	if err := json.Unmarshal(raw, &parsed); err != nil {
		t.Fatalf("what we wrote does not parse: %v", err)
	}
	if len(parsed.Presets) != len(Defaults) {
		t.Errorf("wrote %d presets", len(parsed.Presets))
	}
	// Indented, because a person is going to open this.
	if !strings.Contains(string(raw), "\n  ") {
		t.Error("written file is not indented")
	}
}

func TestLoadReadsAnEditedFile(t *testing.T) {
	store := newTestStore(t)
	store.Load() // create it

	custom := []Preset{{ID: "shouty", Name: "SHOUTY", Instruction: "Use capitals."}}
	writeRaw(t, store.Path(), custom)

	set := store.Load()
	if len(set.Presets) != 1 || set.Presets[0].ID != "shouty" {
		t.Fatalf("got %+v, want the edited set", set.Presets)
	}
	if set.Source != SourceFile {
		t.Errorf("source = %q", set.Source)
	}
}

// An edit has to take effect without restarting the daemon, or "editable"
// means "editable if you know to quit the app first".
func TestLoadPicksUpChangesWithoutRestart(t *testing.T) {
	store := newTestStore(t)
	store.Load()

	writeRaw(t, store.Path(), []Preset{{ID: "one", Name: "One", Instruction: "First."}})
	if got := store.Load().Presets[0].ID; got != "one" {
		t.Fatalf("first edit not seen: %q", got)
	}

	// Modification time has second-level granularity on some filesystems, so
	// change the size too rather than relying on the clock.
	time.Sleep(10 * time.Millisecond)
	writeRaw(t, store.Path(), []Preset{
		{ID: "two", Name: "Two", Instruction: "Second, and rather longer than the first."},
	})
	if got := store.Load().Presets[0].ID; got != "two" {
		t.Errorf("second edit not seen: %q", got)
	}
}

// Someone mid-sentence should get a rewrite, not an error about a config file.
func TestBrokenFileFallsBackToDefaultsAndSaysWhy(t *testing.T) {
	tests := []struct {
		name     string
		contents string
		wantIn   string
	}{
		{"not JSON at all", "{ this is not json", "not valid JSON"},
		{"truncated", `{"presets": [{"id": "x",`, "not valid JSON"},
		{"empty list", `{"presets": []}`, "at least one preset"},
		{"missing fields", `{"presets": [{"id": "x"}]}`, "needs an id, a name"},
		{"duplicate ids", `{"presets":[{"id":"a","name":"A","instruction":"i"},` +
			`{"id":"a","name":"B","instruction":"j"}]}`, "unique"},
		{"wrong type", `{"presets": "not an array"}`, "not valid JSON"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := newTestStore(t)
			if err := os.WriteFile(store.Path(), []byte(tt.contents), 0o600); err != nil {
				t.Fatal(err)
			}

			set := store.Load()
			if len(set.Presets) != len(Defaults) {
				t.Errorf("got %d presets, want the defaults to stand in", len(set.Presets))
			}
			if set.Source != SourceDefaults {
				t.Errorf("source = %q, want %q", set.Source, SourceDefaults)
			}
			if set.Problem == "" {
				t.Fatal("no problem reported — the user would see no change and no reason")
			}
			if !strings.Contains(set.Problem, tt.wantIn) {
				t.Errorf("problem = %q, want it to mention %q", set.Problem, tt.wantIn)
			}
		})
	}
}

// A byte offset is useless to someone editing a file by hand.
func TestSyntaxErrorsCarryLineAndColumn(t *testing.T) {
	store := newTestStore(t)
	contents := "{\n  \"presets\": [\n    {\"id\": \"a\" \"name\": \"A\"}\n  ]\n}\n"
	if err := os.WriteFile(store.Path(), []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}

	problem := store.Load().Problem
	if !strings.Contains(problem, "line 3") {
		t.Errorf("problem = %q, want it to name line 3", problem)
	}
}

// A broken file must not be overwritten just because it was read. Someone's
// work-in-progress edit is more valuable than our defaults.
func TestBrokenFileIsNotOverwritten(t *testing.T) {
	store := newTestStore(t)
	const contents = "{ broken but mine"
	if err := os.WriteFile(store.Path(), []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}

	store.Load()

	raw, err := os.ReadFile(store.Path())
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != contents {
		t.Errorf("the file was rewritten: %q", raw)
	}
}

func TestSaveWritesAndTakesEffect(t *testing.T) {
	store := newTestStore(t)
	store.Load()

	custom := []Preset{
		{ID: "terse", Name: "Terse", Instruction: "Fewer words."},
		{ID: "formal", Name: "Formal", Instruction: "More formal."},
	}
	if err := store.Save(custom); err != nil {
		t.Fatalf("Save: %v", err)
	}

	set := store.Load()
	if len(set.Presets) != 2 || set.Presets[0].ID != "terse" {
		t.Errorf("got %+v", set.Presets)
	}

	// And it is on disk for the next process.
	reloaded := NewStore(filepath.Dir(store.Path())).Load()
	if len(reloaded.Presets) != 2 {
		t.Errorf("disk has %d presets", len(reloaded.Presets))
	}
}

func TestSaveRejectsBadSets(t *testing.T) {
	tests := []struct {
		name    string
		presets []Preset
	}{
		{"empty", nil},
		{"missing instruction", []Preset{{ID: "a", Name: "A"}}},
		{"missing name", []Preset{{ID: "a", Instruction: "i"}}},
		{"blank id", []Preset{{ID: "   ", Name: "A", Instruction: "i"}}},
		{"duplicates", []Preset{
			{ID: "a", Name: "A", Instruction: "i"},
			{ID: "a", Name: "B", Instruction: "j"},
		}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := newTestStore(t)
			store.Load()
			before := store.Load().Presets

			if err := store.Save(tt.presets); err == nil {
				t.Fatal("expected a rejection")
			}
			// A rejected save must leave the working set alone.
			if len(store.Load().Presets) != len(before) {
				t.Error("a rejected save changed the active presets")
			}
		})
	}
}

func TestSaveRejectsAnAbsurdlyLargeSet(t *testing.T) {
	store := newTestStore(t)
	huge := make([]Preset, MaxPresets+1)
	for i := range huge {
		huge[i] = Preset{ID: string(rune('a'+i%26)) + itoa(i), Name: "n", Instruction: "i"}
	}
	if err := store.Save(huge); err == nil {
		t.Error("expected a rejection")
	}
}

// The store hands out copies; a caller mutating what it got must not reach
// back into the cache.
func TestLoadReturnsACopy(t *testing.T) {
	store := newTestStore(t)
	set := store.Load()
	set.Presets[0].Name = "clobbered"

	if store.Load().Presets[0].Name == "clobbered" {
		t.Error("mutating a returned set changed the store's cache")
	}
}

func writeRaw(t *testing.T, path string, presets []Preset) {
	t.Helper()
	raw, err := json.Marshal(fileFormat{Presets: presets})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}
