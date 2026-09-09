package server

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/srikary12/starch/internal/prompt"
)

func getPresets(t *testing.T, c *http.Client) prompt.Set {
	t.Helper()
	resp := do(t, c, http.MethodGet, "/v1/presets", testToken)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var set prompt.Set
	if err := json.NewDecoder(resp.Body).Decode(&set); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	return set
}

func TestGetPresetsReturnsTheDefaultsAndThePath(t *testing.T) {
	dir := t.TempDir()
	c, _, _, _ := startDaemon(t, Options{PresetDir: dir})

	set := getPresets(t, c)
	if len(set.Presets) != len(prompt.Defaults) {
		t.Errorf("got %d presets, want %d", len(set.Presets), len(prompt.Defaults))
	}
	// The shell shows this path so people can find the file; without it they
	// have to be told where it lives, which is the thing the brief rules out.
	if set.Path != filepath.Join(dir, prompt.FileName) {
		t.Errorf("path = %q", set.Path)
	}
	if set.Source != prompt.SourceFile {
		t.Errorf("source = %q — the file should have been created", set.Source)
	}
}

func TestPutPresetsReplacesTheSet(t *testing.T) {
	dir := t.TempDir()
	c, _, _, _ := startDaemon(t, Options{PresetDir: dir})

	body, _ := json.Marshal(presetsBody{Presets: []prompt.Preset{
		{ID: "terse", Name: "Terse", Instruction: "Fewer words."},
	}})
	resp := doBody(t, c, http.MethodPut, "/v1/presets", testToken, body)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}

	set := getPresets(t, c)
	if len(set.Presets) != 1 || set.Presets[0].ID != "terse" {
		t.Errorf("got %+v", set.Presets)
	}
}

// The validation messages are written for a person and must survive the trip.
func TestPutPresetsRejectsBadSetsWithAUsableMessage(t *testing.T) {
	tests := []struct {
		name   string
		body   string
		wantIn string
	}{
		{"empty", `{"presets":[]}`, "at least one preset"},
		{"missing instruction", `{"presets":[{"id":"a","name":"A"}]}`, "needs an id, a name"},
		{
			"duplicate ids",
			`{"presets":[{"id":"a","name":"A","instruction":"i"},{"id":"a","name":"B","instruction":"j"}]}`,
			"appears twice",
		},
		{"not an object", `[]`, "Could not read"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, _, _, _ := startDaemon(t, Options{})
			resp := doBody(t, c, http.MethodPut, "/v1/presets", testToken, []byte(tt.body))
			defer resp.Body.Close()

			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400", resp.StatusCode)
			}
			var envelope ErrorBody
			json.NewDecoder(resp.Body).Decode(&envelope)
			if !strings.Contains(envelope.Error.Message, tt.wantIn) {
				t.Errorf("message = %q, want it to mention %q", envelope.Error.Message, tt.wantIn)
			}
		})
	}
}

// The point of an editable file: edit it, and the next rewrite uses it. No
// restart, no reload button.
func TestAnEditedFileAffectsTheNextRewrite(t *testing.T) {
	dir := t.TempDir()

	var gotSystem string
	upstream := newUpstreamCapturingSystem(t, &gotSystem)

	c, _, _, _ := startDaemon(t, Options{PresetDir: dir})
	configureSession(t, c, upstream.URL)

	// Create the file, then edit it behind the daemon's back.
	getPresets(t, c)
	edited := `{"presets":[{"id":"pirate","name":"Pirate","instruction":"Rewrite it as a pirate would."}]}`
	if err := os.WriteFile(filepath.Join(dir, prompt.FileName), []byte(edited), 0o600); err != nil {
		t.Fatal(err)
	}

	body, _ := json.Marshal(rewriteRequest{Text: "hello", Preset: "pirate"})
	resp := doBody(t, c, http.MethodPut, "/v1/presets", testToken, []byte(edited))
	resp.Body.Close()

	body, _ = json.Marshal(rewriteRequest{Text: "hello", Preset: "pirate"})
	resp = doBody(t, c, http.MethodPost, "/v1/rewrite", testToken, body)
	defer resp.Body.Close()
	readFrames(t, resp.Body)

	if !strings.Contains(gotSystem, "as a pirate would") {
		t.Errorf("the edited instruction did not reach the provider:\n%s", gotSystem)
	}
}

// A broken file must not stop rewriting, and the shell has to be able to say
// why nothing changed.
func TestBrokenPresetFileIsReportedButNotFatal(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, prompt.FileName), []byte("{ nope"), 0o600); err != nil {
		t.Fatal(err)
	}
	c, _, _, _ := startDaemon(t, Options{PresetDir: dir})

	set := getPresets(t, c)
	if set.Source != prompt.SourceDefaults {
		t.Errorf("source = %q, want the defaults to stand in", set.Source)
	}
	if set.Problem == "" {
		t.Error("no problem reported — the user would see no change and no reason")
	}

	// And a rewrite still works.
	var system string
	upstream := newUpstreamCapturingSystem(t, &system)
	configureSession(t, c, upstream.URL)

	body, _ := json.Marshal(rewriteRequest{Text: "hello", Preset: "concise"})
	resp := doBody(t, c, http.MethodPost, "/v1/rewrite", testToken, body)
	defer resp.Body.Close()
	frames := readFrames(t, resp.Body)
	if !frames[len(frames)-1].Done {
		t.Error("a broken presets file broke rewriting")
	}
}

func TestPresetsRejectsOtherMethods(t *testing.T) {
	c, _, _, _ := startDaemon(t, Options{})
	resp := do(t, c, http.MethodDelete, "/v1/presets", testToken)
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405", resp.StatusCode)
	}
	if allow := resp.Header.Get("Allow"); allow != "GET, PUT" {
		t.Errorf("Allow = %q", allow)
	}
}

// newUpstreamCapturingSystem records the system prompt the daemon assembled.
func newUpstreamCapturingSystem(t *testing.T, into *string) *upstream {
	t.Helper()
	u := &upstream{cancelled: make(chan struct{})}
	u.Server = newRecordingServer(t, into)
	t.Cleanup(u.Close)
	return u
}
