package settings

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestParseHotKey(t *testing.T) {
	tests := []struct {
		in      string
		want    HotKey
		display string
	}{
		{"Ctrl+Alt+Super+P", HotKey{ModCtrl | ModAlt | ModSuper, "p"}, "Ctrl+Alt+Super+P"},
		// Spelling a user might reach for, from any desktop they came from.
		{"control+alt+win+p", HotKey{ModCtrl | ModAlt | ModSuper, "p"}, "Ctrl+Alt+Super+P"},
		{"Cmd+Shift+K", HotKey{ModSuper | ModShift, "k"}, "Shift+Super+K"},
		{"Super+space", HotKey{ModSuper, "space"}, "Super+space"},
		{"Ctrl+f5", HotKey{ModCtrl, "F5"}, "Ctrl+F5"},
		{"Alt+F12", HotKey{ModAlt, "F12"}, "Alt+F12"},
		// Letters are lowercase keysyms: the shift state is a modifier, not a
		// different key.
		{"Ctrl+Alt+A", HotKey{ModCtrl | ModAlt, "a"}, "Ctrl+Alt+A"},
	}

	for _, tc := range tests {
		t.Run(tc.in, func(t *testing.T) {
			got, err := ParseHotKey(tc.in)
			if err != nil {
				t.Fatalf("ParseHotKey(%q): %v", tc.in, err)
			}
			if got != tc.want {
				t.Errorf("= %+v, want %+v", got, tc.want)
			}
			if got.String() != tc.display {
				t.Errorf("String() = %q, want %q", got.String(), tc.display)
			}
		})
	}
}

// Round-tripping matters because the settings file is meant to be hand-edited:
// what we write has to be something we can read back.
func TestHotKeyRoundTrips(t *testing.T) {
	for _, h := range []HotKey{
		DefaultHotKey,
		{ModCtrl | ModShift, "space"},
		{ModSuper, "F1"},
		{ModAlt, "slash"},
	} {
		parsed, err := ParseHotKey(h.String())
		if err != nil {
			t.Fatalf("ParseHotKey(%q): %v", h.String(), err)
		}
		if parsed != h {
			t.Errorf("%q round-tripped to %+v, want %+v", h.String(), parsed, h)
		}
	}
}

func TestParseHotKeyRejectsNonsense(t *testing.T) {
	for _, in := range []string{"", "Ctrl+", "+P", "Ctrl+Nope+P"} {
		if got, err := ParseHotKey(in); err == nil {
			t.Errorf("ParseHotKey(%q) = %+v, want an error", in, got)
		}
	}
}

// The grab is global, so an unsafe binding does not fail visibly — it takes
// the key away from every other application. These have to be refused before
// they are ever registered.
func TestHotKeyValidity(t *testing.T) {
	tests := []struct {
		in        string
		valid     bool
		mentions  string
		explained string
	}{
		{"Ctrl+Alt+Super+P", true, "", "the default"},
		{"Ctrl+Alt+P", true, "", "two modifiers and a letter"},
		{"P", false, "Ctrl, Alt or Super", "a bare letter would swallow typing"},
		{"Shift+P", false, "Ctrl, Alt or Super", "Shift alone is part of ordinary typing"},
		{"Ctrl+C", false, "every application", "one modifier plus a letter shadows a real shortcut"},
		{"Super+space", true, "", "space carries no character"},
		{"Ctrl+F5", true, "", "function keys shadow nothing"},
	}

	for _, tc := range tests {
		t.Run(tc.in, func(t *testing.T) {
			h, err := ParseHotKey(tc.in)
			if err != nil {
				t.Fatalf("ParseHotKey(%q): %v", tc.in, err)
			}
			if got := h.Valid(); got != tc.valid {
				t.Fatalf("Valid() = %v, want %v (%s); reason %q",
					got, tc.valid, tc.explained, h.InvalidReason())
			}
			if tc.mentions != "" && !strings.Contains(h.InvalidReason(), tc.mentions) {
				t.Errorf("reason %q does not mention %q", h.InvalidReason(), tc.mentions)
			}
		})
	}
}

// Stored as the string a person would type, so the settings file stays
// readable and editable.
func TestHotKeyJSONIsAString(t *testing.T) {
	encoded, err := json.Marshal(DefaultHotKey)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if string(encoded) != `"Ctrl+Alt+Super+P"` {
		t.Fatalf("encoded = %s, want a plain string", encoded)
	}

	var back HotKey
	if err := json.Unmarshal(encoded, &back); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if back != DefaultHotKey {
		t.Errorf("decoded = %+v", back)
	}
}

func TestDefaultHotKeyIsUsable(t *testing.T) {
	if reason := DefaultHotKey.InvalidReason(); reason != "" {
		t.Fatalf("the default shortcut is refused by our own rules: %s", reason)
	}
}
