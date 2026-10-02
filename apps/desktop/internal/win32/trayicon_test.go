package win32

import (
	"testing"
	"unsafe"
)

// Shell_NotifyIcon works out which version of the structure it has been handed
// by reading cbSize. A wrong value fails with no icon and no explanation, or —
// if it happens to match an older version — succeeds while silently ignoring
// the fields beyond it.
//
// The documented size on 64-bit Windows, checked here because nothing on the
// path to a running tray icon would notice.
func TestTheTrayStructureIsTheSizeTheShellExpects(t *testing.T) {
	if unsafe.Sizeof(uintptr(0)) != 8 {
		t.Skip("the documented size below is the 64-bit one")
	}

	if got := unsafe.Sizeof(notifyIconData{}); got != 976 {
		t.Errorf("NOTIFYICONDATAW = %d bytes, want 976", got)
	}
	if notifyIconDataSize != 976 {
		t.Errorf("cbSize would be sent as %d, want 976", notifyIconDataSize)
	}

	if got := unsafe.Sizeof(guid{}); got != 16 {
		t.Errorf("GUID = %d bytes, want 16; the tail of the structure is misplaced", got)
	}

	// The two fields most likely to be misplaced by a padding mistake, because
	// both sit immediately after a run of 32-bit fields.
	if got := unsafe.Offsetof(notifyIconData{}.hIcon); got != 32 {
		t.Errorf("hIcon is at byte %d, want 32", got)
	}
	if got := unsafe.Offsetof(notifyIconData{}.szTip); got != 40 {
		t.Errorf("szTip is at byte %d, want 40", got)
	}
}

// Zero is what TrackPopupMenu returns when the user dismisses the menu without
// choosing anything. Treating that as a click on the first item would mean the
// tray menu changing a setting every time someone pressed Escape.
func TestDismissingTheMenuIsNotAChoice(t *testing.T) {
	if _, ok := menuIndex(0); ok {
		t.Fatal("a dismissal was read as a selection")
	}

	for i := range 10 {
		index, ok := menuIndex(menuCommandID(i))
		if !ok {
			t.Errorf("item %d round-tripped as a dismissal", i)
		}
		if index != i {
			t.Errorf("item %d round-tripped as %d", i, index)
		}
	}
}

func TestWhatCanBeChosen(t *testing.T) {
	tests := []struct {
		name string
		item MenuItem
		want bool
	}{
		{"an ordinary item", MenuItem{Label: "Concise", Do: func() {}}, true},
		{"a separator", Separator(), false},
		{"a disabled status line", MenuItem{Label: "Ready", Disabled: true}, false},
		{"an item with nothing to do", MenuItem{Label: "Ready"}, false},
		{"a checked item is still selectable", MenuItem{Label: "Professional", Checked: true, Do: func() {}}, true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.item.selectable(); got != tc.want {
				t.Errorf("selectable = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestASeparatorIsAnEmptyLabel(t *testing.T) {
	if !Separator().isSeparator() {
		t.Error("Separator() is not one")
	}
	if (MenuItem{Label: "Quit"}).isSeparator() {
		t.Error("a labelled item reads as a separator")
	}
}
