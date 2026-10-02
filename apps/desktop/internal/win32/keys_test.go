package win32

import (
	"strings"
	"testing"

	"github.com/srikary12/starch/apps/desktop/internal/settings"
)

// No build tag, so this runs on the machine the code is written on. The
// syscall that consumes a HotKeyBinding cannot be tested here; the table that
// produces one is the part that gets a value wrong, and it can be.

func TestTranslateTheDefaultShortcut(t *testing.T) {
	got, err := TranslateHotKey(settings.DefaultHotKey)
	if err != nil {
		t.Fatalf("TranslateHotKey(%s): %v", settings.DefaultHotKey, err)
	}

	// Ctrl+Alt+Super+P.
	want := uint32(modNoRepeat | modControl | modAlt | modWin)
	if got.Modifiers != want {
		t.Errorf("modifiers = %#04x, want %#04x", got.Modifiers, want)
	}
	if got.VirtualKey != 'P' {
		t.Errorf("virtual key = %#02x, want %#02x", got.VirtualKey, 'P')
	}
}

// Holding a shortcut down must fire once. Without MOD_NOREPEAT each repeat
// starts a rewrite, and each rewrite is billed.
func TestEveryBindingRefusesKeyRepeat(t *testing.T) {
	for _, spelling := range []string{
		"Ctrl+Alt+Super+P", "Ctrl+Shift+space", "Alt+F5", "Ctrl+Alt+Delete",
	} {
		h, err := settings.ParseHotKey(spelling)
		if err != nil {
			t.Fatalf("ParseHotKey(%q): %v", spelling, err)
		}
		binding, err := TranslateHotKey(h)
		if err != nil {
			t.Fatalf("TranslateHotKey(%q): %v", spelling, err)
		}
		if binding.Modifiers&modNoRepeat == 0 {
			t.Errorf("%q does not set MOD_NOREPEAT, so holding it would rewrite repeatedly", spelling)
		}
	}
}

func TestVirtualKeys(t *testing.T) {
	tests := []struct {
		key  string
		want uint32
	}{
		// Letters are the uppercase ASCII code whatever case they are stored
		// in; the shift state is a modifier, not a different key.
		{"p", 'P'},
		{"P", 'P'},
		{"a", 'A'},
		{"z", 'Z'},
		{"0", '0'},
		{"9", '9'},
		{"space", 0x20},
		{"Return", 0x0D},
		{"Escape", 0x1B},
		{"Delete", 0x2E},
		// keysymdef spells these Prior and Next; everyone else says Page Up and
		// Page Down, and a hand-edited settings file will have either.
		{"Prior", 0x21},
		{"Page_Up", 0x21},
		{"Next", 0x22},
		{"Page_Down", 0x22},
		{"Left", 0x25},
		{"Down", 0x28},
		{"F1", 0x70},
		{"F5", 0x74},
		{"F12", 0x7B},
		{"F24", 0x87},
		{".", 0xBE},
		{";", 0xBA},
	}

	for _, tc := range tests {
		got, err := virtualKey(tc.key)
		if err != nil {
			t.Errorf("virtualKey(%q): %v", tc.key, err)
			continue
		}
		if got != tc.want {
			t.Errorf("virtualKey(%q) = %#02x, want %#02x", tc.key, got, tc.want)
		}
	}
}

// F13 to F24 exist and F25 does not. Worth pinning because the codes run
// consecutively from F1 and an off-by-one here binds the wrong key silently.
func TestFunctionKeyRange(t *testing.T) {
	if _, err := virtualKey("F24"); err != nil {
		t.Errorf("F24 should be bindable: %v", err)
	}
	for _, key := range []string{"F0", "F25", "F99", "Fx", "F1x"} {
		if _, err := virtualKey(key); err == nil {
			t.Errorf("virtualKey(%q) succeeded, want it refused", key)
		}
	}

	// "F" on its own is the letter, not a malformed function key, and must
	// resolve as one — Ctrl+Alt+F is a perfectly ordinary shortcut.
	if got, err := virtualKey("F"); err != nil || got != 'F' {
		t.Errorf("virtualKey(\"F\") = %#02x, %v; want the letter F (%#02x)", got, err, 'F')
	}
}

func TestAnUnknownKeyIsRefusedByName(t *testing.T) {
	_, err := virtualKey("Hyper_L")
	if err == nil {
		t.Fatal("an unknown keysym was accepted")
	}
	if !strings.Contains(err.Error(), "Hyper_L") {
		t.Errorf("error = %v, want it to name the key it could not bind", err)
	}
}

// The settings layer's rules are about a global grab taking a key away from
// every application on the machine. That is no less true here, so translation
// enforces them rather than assuming somebody already did.
func TestADangerousShortcutIsRefusedHereToo(t *testing.T) {
	tests := []struct {
		name string
		key  settings.HotKey
	}{
		{"no modifiers at all", settings.HotKey{Key: "p"}},
		{"one modifier on a character key", settings.HotKey{Mods: settings.ModCtrl, Key: "c"}},
		{"shift alone, which is ordinary typing", settings.HotKey{Mods: settings.ModShift, Key: "p"}},
		{"no key", settings.HotKey{Mods: settings.ModCtrl | settings.ModAlt}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := TranslateHotKey(tc.key); err == nil {
				t.Fatalf("TranslateHotKey(%v) succeeded, want it refused", tc.key)
			}
		})
	}
}

// One modifier is enough for a key that carries no character, on Windows as
// everywhere else.
func TestASingleModifierIsFineOnAFunctionKey(t *testing.T) {
	h := settings.HotKey{Mods: settings.ModAlt, Key: "F5"}
	binding, err := TranslateHotKey(h)
	if err != nil {
		t.Fatalf("TranslateHotKey(%s): %v", h, err)
	}
	if binding.Modifiers != uint32(modNoRepeat|modAlt) {
		t.Errorf("modifiers = %#04x", binding.Modifiers)
	}
}

// ERROR_HOTKEY_ALREADY_REGISTERED is the likeliest failure on this platform and
// says nothing useful by itself, so the message has to.
func TestTheConflictMessageIsActionable(t *testing.T) {
	help := HotKeyConflictHelp(settings.DefaultHotKey)

	if !strings.Contains(help, settings.DefaultHotKey.String()) {
		t.Errorf("help = %q, want it to name the shortcut", help)
	}
	if !strings.Contains(help, "starch config") {
		t.Errorf("help = %q, want it to say how to change it", help)
	}
	// The Win key is the common cause, and only worth mentioning when it is
	// actually involved.
	if !strings.Contains(help, "Win-key") {
		t.Errorf("help = %q, want it to mention the reserved Win combinations", help)
	}
	if strings.Contains(HotKeyConflictHelp(settings.HotKey{
		Mods: settings.ModCtrl | settings.ModAlt, Key: "p",
	}), "Win-key") {
		t.Error("the Win-key note appears for a shortcut that does not use it")
	}
}
