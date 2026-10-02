package win32

// The tray icon's data structure, and the menu model, in an untagged file so
// both are checked here.
//
// NOTIFYICONDATAW is size-sensitive in the same way INPUT is, and worse: the
// structure has grown across Windows versions, and Shell_NotifyIcon decides
// which version it is being handed by looking at cbSize. Get it wrong and the
// call fails with no icon and no explanation, or — if it happens to match an
// older version — succeeds while silently ignoring half the fields.

import "unsafe"

// guid is GUID, declared here rather than taken from golang.org/x/sys/windows
// so that this file builds everywhere and the size assertion below can run off
// Windows. Same 16-byte layout, which the test checks rather than assumes.
type guid struct {
	data1 uint32
	data2 uint16
	data3 uint16
	data4 [8]byte
}

// notifyIconData is NOTIFYICONDATAW, in its current form.
type notifyIconData struct {
	cbSize           uint32
	hWnd             uintptr
	uID              uint32
	uFlags           uint32
	uCallbackMessage uint32
	hIcon            uintptr
	szTip            [128]uint16
	dwState          uint32
	dwStateMask      uint32
	szInfo           [256]uint16
	uVersion         uint32
	szInfoTitle      [64]uint16
	dwInfoFlags      uint32
	guidItem         guid
	hBalloonIcon     uintptr
}

var notifyIconDataSize = uint32(unsafe.Sizeof(notifyIconData{}))

// MenuItem is one line of the tray menu.
type MenuItem struct {
	// Label is what the user reads. Empty means a separator.
	Label string
	// Checked marks the current choice, for a group like the presets.
	Checked bool
	// Disabled greys it out. Used for the status line, which is information
	// rather than an action.
	Disabled bool
	// Do runs when it is chosen. nil is allowed and means the item does
	// nothing, which is what a status line wants.
	Do func()
}

// Separator is a divider in the menu.
func Separator() MenuItem { return MenuItem{} }

func (m MenuItem) isSeparator() bool { return m.Label == "" }

// selectable reports whether choosing this item should do anything.
func (m MenuItem) selectable() bool {
	return !m.isSeparator() && !m.Disabled && m.Do != nil
}

// menuCommandID turns a position in the menu into the command identifier
// Windows sends back in WM_COMMAND.
//
// Offset by one because zero is what TrackPopupMenu returns when the user
// dismisses the menu without choosing anything, and treating a dismissal as a
// click on the first item would mean the tray menu quietly changing settings
// whenever somebody pressed Escape.
func menuCommandID(index int) uint32 { return uint32(index) + 1 }

// menuIndex is the reverse, returning false for the dismissal.
func menuIndex(command uint32) (int, bool) {
	if command == 0 {
		return 0, false
	}
	return int(command) - 1, true
}
