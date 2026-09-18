// Translating a stored shortcut into what RegisterHotKey wants.
//
// Deliberately in a file with no build tag, so it compiles and is tested on the
// machine this is written on. The syscall that consumes it cannot be, and
// keeping the table on this side of that line is the difference between one
// untested function and one untested package.
package win32

import (
	"fmt"
	"strings"

	"github.com/srikary12/starch/apps/desktop/internal/settings"
)

// Modifier flags for RegisterHotKey.
const (
	modAlt     = 0x0001
	modControl = 0x0002
	modShift   = 0x0004
	modWin     = 0x0008
	// Without this, holding the shortcut down fires it over and over, and each
	// one would start a rewrite and bill for it.
	modNoRepeat = 0x4000
)

// HotKeyBinding is a shortcut in the form RegisterHotKey takes.
type HotKeyBinding struct {
	Modifiers  uint32
	VirtualKey uint32
}

// namedKeys maps the keysym spellings settings.HotKey stores to Windows
// virtual-key codes.
//
// Keysym names rather than Windows names because that is what is already in
// everyone's settings file and what the other platforms look up; a second
// vocabulary would mean a settings file that means different things on
// different machines. The X11 spellings and their more common aliases are both
// accepted, since a hand-edited file will contain whichever the person knew.
var namedKeys = map[string]uint32{
	"space":     0x20,
	"Return":    0x0D,
	"Enter":     0x0D,
	"Tab":       0x09,
	"Escape":    0x1B,
	"BackSpace": 0x08,
	"Delete":    0x2E,
	"Insert":    0x2D,
	"Home":      0x24,
	"End":       0x23,
	"Prior":     0x21, // Page Up, in keysymdef's spelling
	"Page_Up":   0x21,
	"Next":      0x22, // Page Down
	"Page_Down": 0x22,
	"Left":      0x25,
	"Up":        0x26,
	"Right":     0x27,
	"Down":      0x28,
}

// oemKeys are punctuation whose virtual-key code is fixed but whose *character*
// depends on the keyboard layout: VK_OEM_1 is semicolon on a US layout and
// something else elsewhere.
//
// Included because people do bind them, and excluded from the error message's
// suggestions because a shortcut that moves when the layout changes is a poor
// thing to recommend.
var oemKeys = map[string]uint32{
	";":  0xBA, // VK_OEM_1
	"=":  0xBB, // VK_OEM_PLUS
	",":  0xBC, // VK_OEM_COMMA
	"-":  0xBD, // VK_OEM_MINUS
	".":  0xBE, // VK_OEM_PERIOD
	"/":  0xBF, // VK_OEM_2
	"`":  0xC0, // VK_OEM_3
	"[":  0xDB, // VK_OEM_4
	"\\": 0xDC, // VK_OEM_5
	"]":  0xDD, // VK_OEM_6
	"'":  0xDE, // VK_OEM_7
}

// TranslateHotKey turns a stored shortcut into modifiers and a virtual key.
//
// It refuses a shortcut the settings layer would refuse, rather than trusting
// that it was checked earlier: a global registration that succeeds on a
// dangerous binding takes that key away from every other application on the
// machine, with nothing on screen to say what did it.
func TranslateHotKey(h settings.HotKey) (HotKeyBinding, error) {
	if reason := h.InvalidReason(); reason != "" {
		return HotKeyBinding{}, fmt.Errorf("%s", reason)
	}

	key, err := virtualKey(h.Key)
	if err != nil {
		return HotKeyBinding{}, err
	}

	mods := uint32(modNoRepeat)
	for _, m := range []struct {
		bit  settings.Modifier
		flag uint32
	}{
		{settings.ModCtrl, modControl},
		{settings.ModAlt, modAlt},
		{settings.ModShift, modShift},
		{settings.ModSuper, modWin},
	} {
		if h.Mods.Has(m.bit) {
			mods |= m.flag
		}
	}

	return HotKeyBinding{Modifiers: mods, VirtualKey: key}, nil
}

// virtualKey looks up one key name.
func virtualKey(name string) (uint32, error) {
	// Letters and digits are their own ASCII codes, which is the one convenient
	// thing about the virtual-key table.
	if len(name) == 1 {
		c := name[0]
		switch {
		case c >= 'a' && c <= 'z':
			return uint32(c - 'a' + 'A'), nil
		case c >= 'A' && c <= 'Z':
			return uint32(c), nil
		case c >= '0' && c <= '9':
			return uint32(c), nil
		}
		if vk, ok := oemKeys[name]; ok {
			return vk, nil
		}
	}

	if vk, ok := namedKeys[name]; ok {
		return vk, nil
	}

	// F1 through F24. VK_F1 is 0x70 and they run consecutively.
	if n, ok := functionKey(name); ok {
		return 0x70 + n - 1, nil
	}

	return 0, fmt.Errorf("%q is not a key this shell can bind on Windows", name)
}

// functionKey parses "F5" into 5, for F1 to F24.
func functionKey(name string) (uint32, bool) {
	if len(name) < 2 || (name[0] != 'F' && name[0] != 'f') {
		return 0, false
	}
	digits := name[1:]
	var n uint32
	for _, r := range digits {
		if r < '0' || r > '9' {
			return 0, false
		}
		n = n*10 + uint32(r-'0')
	}
	if n < 1 || n > 24 {
		return 0, false
	}
	return n, true
}

// HotKeyConflictHelp is what to tell someone whose shortcut was refused by the
// operating system.
//
// Its own function because ERROR_HOTKEY_ALREADY_REGISTERED is the single most
// likely failure on this platform and the least self-explanatory: Windows hands
// back one error code whether the key is held by another application, reserved
// by the shell, or taken by the machine's own vendor utility, and there is no
// API that says which.
func HotKeyConflictHelp(h settings.HotKey) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s is already taken by something else on this machine, "+
		"so Starch could not register it.", h.String())
	if h.Mods.Has(settings.ModSuper) {
		b.WriteString(" Windows reserves a number of Win-key combinations for " +
			"itself, and some laptop vendors add more.")
	}
	b.WriteString(" Pick a different shortcut with `starch config --hotkey`.")
	return b.String()
}
