package win32

import (
	"testing"
	"unsafe"
)

// SendInput checks the size it is told each element is and does nothing if it
// disagrees — returning zero, with nothing else to go on. So a layout that is
// wrong by a byte is a shell where the hot key fires, the overlay opens, and
// the selection is never copied, discovered only on a real Windows desktop.
//
// These are the documented sizes on 64-bit Windows. They are fixed by the
// architecture rather than the OS, which is why this can run here at all.
func TestTheInputStructuresAreTheSizeWindowsExpects(t *testing.T) {
	if unsafe.Sizeof(uintptr(0)) != 8 {
		t.Skip("the documented sizes below are the 64-bit ones")
	}

	if got := unsafe.Sizeof(keybdInput{}); got != 24 {
		t.Errorf("KEYBDINPUT = %d bytes, want 24", got)
	}
	if got := unsafe.Sizeof(input{}); got != 40 {
		t.Errorf("INPUT = %d bytes, want 40; SendInput will refuse every call", got)
	}
	if int(inputSize) != 40 {
		t.Errorf("inputSize = %d, want 40", inputSize)
	}

	// The union has to start after the type field and its padding, or every
	// key event carries a virtual-key code read from the wrong offset.
	if got := unsafe.Offsetof(input{}.ki); got != 8 {
		t.Errorf("the union begins at byte %d, want 8", got)
	}
}

// Modifier down, key down, key up, modifier up. Releasing the modifier first
// would have applications see a bare keystroke; not releasing it would apply it
// to whatever the user types next.
func TestAChordPressesAndReleasesInOrder(t *testing.T) {
	events := copyChord()

	if len(events) != 4 {
		t.Fatalf("%d events, want 4", len(events))
	}

	want := []struct {
		vk    uint16
		up    bool
		label string
	}{
		{vkControl, false, "Ctrl down"},
		{vkC, false, "C down"},
		{vkC, true, "C up"},
		{vkControl, true, "Ctrl up"},
	}

	for i, w := range want {
		got := events[i]
		if got.kind != inputKeyboard {
			t.Errorf("event %d (%s) is not a keyboard event", i, w.label)
		}
		if got.ki.vk != w.vk {
			t.Errorf("event %d (%s) has vk %#02x, want %#02x", i, w.label, got.ki.vk, w.vk)
		}
		if up := got.ki.flags&keyEventKeyUp != 0; up != w.up {
			t.Errorf("event %d (%s): key-up = %v, want %v", i, w.label, up, w.up)
		}
	}
}

func TestCopyAndPasteDifferOnlyInTheKey(t *testing.T) {
	c, v := copyChord(), pasteChord()

	if c[1].ki.vk != vkC || v[1].ki.vk != vkV {
		t.Fatalf("copy uses %#02x and paste %#02x", c[1].ki.vk, v[1].ki.vk)
	}
	if c[0].ki.vk != vkControl || v[0].ki.vk != vkControl {
		t.Error("both chords should be held with Ctrl")
	}
	if len(c) != len(v) {
		t.Errorf("copy has %d events and paste %d", len(c), len(v))
	}
}
