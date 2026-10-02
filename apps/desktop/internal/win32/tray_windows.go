package win32

import (
	"fmt"
	"sync"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	shell32            = windows.NewLazySystemDLL("shell32.dll")
	procShellNotifyIcn = shell32.NewProc("Shell_NotifyIconW")

	procLoadIcon         = user32.NewProc("LoadIconW")
	procCreatePopupMenu  = user32.NewProc("CreatePopupMenu")
	procAppendMenu       = user32.NewProc("AppendMenuW")
	procTrackPopupMenu   = user32.NewProc("TrackPopupMenu")
	procDestroyMenu      = user32.NewProc("DestroyMenu")
	procGetCursorPos     = user32.NewProc("GetCursorPos")
	procSetForegroundWin = user32.NewProc("SetForegroundWindow")
)

const (
	nimAdd    = 0x00000000
	nimModify = 0x00000001
	nimDelete = 0x00000002

	nifMessage = 0x00000001
	nifIcon    = 0x00000002
	nifTip     = 0x00000004

	mfString    = 0x00000000
	mfGrayed    = 0x00000001
	mfChecked   = 0x00000008
	mfSeparator = 0x00000800

	tpmRightButton = 0x0002
	tpmReturnCmd   = 0x0100

	wmRButtonUp = 0x0205
	wmLButtonUp = 0x0202

	// The message the shell sends us about the icon, and the one the tray menu
	// posts to itself.
	wmTrayCallback = wmApp + 3

	idiApplication = 32512

	trayIconID = 1
)

type point struct{ x, y int32 }

// Tray is the notification-area icon and its menu.
//
// It is not on the path of a rewrite: the flow reports through the overlay,
// where the user is already looking. This is the settings affordance — the
// place to change preset without a terminal, and the place to quit from — which
// is why nothing above it fails if the icon cannot be created.
type Tray struct {
	ui *UIThread

	mu    sync.Mutex
	items []MenuItem
	tip   string

	added bool
}

// activeTray is the tray the window procedure routes messages to, for the same
// reason as `active` and `activeOverlay`: there is one of it, and a window
// procedure has nowhere to hang a receiver.
var activeTray struct {
	sync.Mutex
	tray *Tray
}

// ShowTray puts an icon in the notification area.
//
// menu is what the icon offers when clicked. Rebuild it with SetMenu whenever
// the thing it reflects changes — the checked preset, say — because the menu is
// constructed at the moment it is shown rather than cached.
func (u *UIThread) ShowTray(tip string, menu []MenuItem) (*Tray, error) {
	tray := &Tray{ui: u, items: menu, tip: tip}

	created := make(chan error, 1)
	u.Post(func() { created <- tray.add() })
	if err := <-created; err != nil {
		return nil, err
	}

	activeTray.Lock()
	activeTray.tray = tray
	activeTray.Unlock()
	return tray, nil
}

// SetMenu replaces what the icon offers.
func (t *Tray) SetMenu(menu []MenuItem) {
	t.mu.Lock()
	t.items = menu
	t.mu.Unlock()
}

// Close removes the icon.
//
// Worth doing rather than leaving to process exit: an icon whose process has
// gone lingers in the notification area until the user happens to move the
// mouse over it, which looks exactly like the application failing to quit.
func (t *Tray) Close() {
	activeTray.Lock()
	if activeTray.tray == t {
		activeTray.tray = nil
	}
	activeTray.Unlock()

	done := make(chan struct{})
	t.ui.Post(func() {
		defer close(done)
		t.mu.Lock()
		defer t.mu.Unlock()
		if !t.added {
			return
		}
		data := t.data(0)
		procShellNotifyIcn.Call(nimDelete, uintptr(unsafe.Pointer(&data)))
		t.added = false
	})
	<-done
}

func (t *Tray) add() error {
	// The generic application icon. A bundled one belongs with the packaging
	// work, and an icon that is obviously a placeholder is better than a
	// missing icon that reads as a broken install.
	icon, _, _ := procLoadIcon.Call(0, idiApplication)

	data := t.data(icon)
	result, _, err := procShellNotifyIcn.Call(nimAdd, uintptr(unsafe.Pointer(&data)))
	if result == 0 {
		return fmt.Errorf("adding the notification-area icon: %w", err)
	}

	t.mu.Lock()
	t.added = true
	t.mu.Unlock()
	return nil
}

func (t *Tray) data(icon uintptr) notifyIconData {
	data := notifyIconData{
		cbSize:           notifyIconDataSize,
		hWnd:             t.ui.hwnd,
		uID:              trayIconID,
		uFlags:           nifMessage | nifIcon | nifTip,
		uCallbackMessage: wmTrayCallback,
		hIcon:            icon,
	}

	t.mu.Lock()
	tip := t.tip
	t.mu.Unlock()

	// szTip is a fixed 128 characters including the terminator, so a long tip
	// is truncated here rather than overrunning the field.
	encoded, err := windows.UTF16FromString(tip)
	if err == nil {
		if len(encoded) > len(data.szTip) {
			encoded = encoded[:len(data.szTip)-1]
			encoded = append(encoded, 0)
		}
		copy(data.szTip[:], encoded)
	}
	return data
}

// showMenu builds and runs the popup menu, on the UI thread.
func (t *Tray) showMenu() {
	t.mu.Lock()
	items := append([]MenuItem(nil), t.items...)
	t.mu.Unlock()

	if len(items) == 0 {
		return
	}

	menu, _, _ := procCreatePopupMenu.Call()
	if menu == 0 {
		return
	}
	defer procDestroyMenu.Call(menu)

	for i, item := range items {
		if item.isSeparator() {
			procAppendMenu.Call(menu, mfSeparator, 0, 0)
			continue
		}

		flags := uintptr(mfString)
		if item.Checked {
			flags |= mfChecked
		}
		if !item.selectable() {
			flags |= mfGrayed
		}

		label, err := windows.UTF16PtrFromString(item.Label)
		if err != nil {
			continue
		}
		procAppendMenu.Call(menu, flags,
			uintptr(menuCommandID(i)), uintptr(unsafe.Pointer(label)))
	}

	var cursor point
	procGetCursorPos.Call(uintptr(unsafe.Pointer(&cursor)))

	// Required, and famously so: without it the menu stays on screen after the
	// user clicks elsewhere, because the shell only dismisses a popup owned by
	// the foreground window.
	procSetForegroundWin.Call(t.ui.hwnd)

	chosen, _, _ := procTrackPopupMenu.Call(
		menu, tpmRightButton|tpmReturnCmd,
		uintptr(cursor.x), uintptr(cursor.y),
		0, t.ui.hwnd, 0,
	)

	index, ok := menuIndex(uint32(chosen))
	if !ok || index >= len(items) {
		// Dismissed without choosing. Not a click on the first item.
		return
	}
	if item := items[index]; item.selectable() {
		// Off the UI thread: a menu action may open an overlay or talk to the
		// daemon, and this is running inside the message loop.
		go item.Do()
	}
}

// trayMessage routes the shell's callback.
func trayMessage(event uintptr) {
	if event != wmRButtonUp && event != wmLButtonUp {
		return
	}
	activeTray.Lock()
	tray := activeTray.tray
	activeTray.Unlock()
	if tray != nil {
		tray.showMenu()
	}
}
