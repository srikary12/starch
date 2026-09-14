package settings

import (
	"fmt"
	"strings"
)

// Modifier is a set of modifier keys, as a bitmask.
//
// Named for what a Linux user calls them rather than for X11's Mod1..Mod5,
// which are a keyboard-map indirection and mean different things on different
// layouts. Turning these into X modifier masks is the X layer's job, done once
// against the live keyboard map.
type Modifier uint8

const (
	ModCtrl Modifier = 1 << iota
	ModAlt
	ModShift
	ModSuper
)

// Has reports whether every modifier in m is present.
func (m Modifier) Has(other Modifier) bool { return m&other == other }

// count is how many of Ctrl, Alt and Super are set. Shift does not count: it
// cannot carry a shortcut on its own because it is part of ordinary typing.
func (m Modifier) count() int {
	n := 0
	for _, mod := range []Modifier{ModCtrl, ModAlt, ModSuper} {
		if m.Has(mod) {
			n++
		}
	}
	return n
}

// modifierNames maps what a user might write to the bit it means. Several
// spellings each, because "Super", "Win", "Cmd" and "Meta" all name the key
// with the logo on it depending on who is describing it.
var modifierNames = map[string]Modifier{
	"ctrl": ModCtrl, "control": ModCtrl,
	"alt": ModAlt, "option": ModAlt, "opt": ModAlt,
	"shift": ModShift,
	"super": ModSuper, "win": ModSuper, "meta": ModSuper,
	"cmd": ModSuper, "command": ModSuper, "mod4": ModSuper,
}

// HotKey is a global shortcut: a set of modifiers and one key.
//
// The key is an X keysym name as spelled in keysymdef.h — "p", "space", "F5" —
// because that is what the X layer has to look up, and storing anything else
// would mean a translation table that can disagree with the server's.
type HotKey struct {
	Mods Modifier
	Key  string
}

// DefaultHotKey is Ctrl+Alt+Super+P, the direct counterpart of the macOS
// shell's ⌃⌥⌘P. Three modifiers is deliberate: the binding is global, so a
// collision does not merely fail — it silently breaks that shortcut in every
// other application.
var DefaultHotKey = HotKey{Mods: ModCtrl | ModAlt | ModSuper, Key: "p"}

// safeSingleModifier are keys that carry no character and shadow nothing
// anyone types, so one modifier is enough for them.
var safeSingleModifier = map[string]bool{
	"space": true,
	"F1":    true, "F2": true, "F3": true, "F4": true, "F5": true, "F6": true,
	"F7": true, "F8": true, "F9": true, "F10": true, "F11": true, "F12": true,
}

// String renders the shortcut the way a desktop settings panel would.
func (h HotKey) String() string {
	var parts []string
	// A fixed order, so the same shortcut always reads the same way.
	for _, m := range []struct {
		bit  Modifier
		name string
	}{{ModCtrl, "Ctrl"}, {ModAlt, "Alt"}, {ModShift, "Shift"}, {ModSuper, "Super"}} {
		if h.Mods.Has(m.bit) {
			parts = append(parts, m.name)
		}
	}
	parts = append(parts, displayKey(h.Key))
	return strings.Join(parts, "+")
}

func displayKey(key string) string {
	if len(key) == 1 {
		return strings.ToUpper(key)
	}
	return key
}

// ParseHotKey reads a shortcut written the way a desktop settings panel shows
// it: "Ctrl+Alt+Super+P".
func ParseHotKey(s string) (HotKey, error) {
	fields := strings.Split(s, "+")
	var h HotKey

	for i, field := range fields {
		field = strings.TrimSpace(field)
		if field == "" {
			return HotKey{}, fmt.Errorf("%q has an empty part", s)
		}
		if mod, ok := modifierNames[strings.ToLower(field)]; ok && i < len(fields)-1 {
			h.Mods |= mod
			continue
		}
		if i != len(fields)-1 {
			return HotKey{}, fmt.Errorf("%q: %q is not a modifier", s, field)
		}
		h.Key = canonicalKey(field)
	}

	if h.Key == "" {
		return HotKey{}, fmt.Errorf("%q names no key", s)
	}
	return h, nil
}

// canonicalKey normalises a key name to its keysym spelling. Letters are
// lowercase keysyms — the shift state is a modifier, not a different key — and
// function keys are uppercase.
func canonicalKey(key string) string {
	if len(key) == 1 {
		return strings.ToLower(key)
	}
	if len(key) >= 2 && (key[0] == 'f' || key[0] == 'F') {
		if rest := key[1:]; isDigits(rest) {
			return "F" + rest
		}
	}
	return key
}

func isDigits(s string) bool {
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return s != ""
}

// InvalidReason explains why this shortcut cannot be used, or returns "".
//
// Stricter than it looks, on purpose. A global grab that collides does not
// fail visibly: it takes the key away from every other application, with no
// clue as to what did it.
func (h HotKey) InvalidReason() string {
	if h.Key == "" {
		return "A shortcut needs a key."
	}
	switch h.Mods.count() {
	case 0:
		return "A shortcut needs Ctrl, Alt or Super. Without one it would swallow " +
			"ordinary typing in every application."
	case 1:
		if !safeSingleModifier[h.Key] {
			return h.String() + " would take that shortcut away from every application — " +
				"Ctrl+C would stop copying everywhere. Add a second modifier."
		}
	}
	return ""
}

// Valid reports whether the shortcut is safe to grab globally.
func (h HotKey) Valid() bool { return h.InvalidReason() == "" }

// MarshalText stores the shortcut as the string a person would type, so the
// settings file stays hand-editable.
func (h HotKey) MarshalText() ([]byte, error) { return []byte(h.String()), nil }

// UnmarshalText parses that string back.
func (h *HotKey) UnmarshalText(text []byte) error {
	parsed, err := ParseHotKey(string(text))
	if err != nil {
		return err
	}
	*h = parsed
	return nil
}
