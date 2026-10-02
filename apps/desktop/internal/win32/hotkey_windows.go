package win32

import (
	"errors"
	"fmt"

	"github.com/srikary12/starch/apps/desktop/internal/settings"
	"golang.org/x/sys/windows"
)

var (
	procRegisterHotKey   = user32.NewProc("RegisterHotKey")
	procUnregisterHotKey = user32.NewProc("UnregisterHotKey")
)

// ERROR_HOTKEY_ALREADY_REGISTERED. Windows returns this whether the shortcut is
// held by another application, reserved by the shell, or taken by a vendor
// utility, and offers no way to find out which — hence HotKeyConflictHelp.
const errorHotKeyAlreadyRegistered = windows.Errno(1409)

// ErrHotKeyTaken reports a shortcut the operating system would not give us.
var ErrHotKeyTaken = errors.New("that shortcut is already registered by something else")

// RegisterHotKey binds a global shortcut, calling onPress each time it is hit.
//
// Must be called from the UI thread — pass it to Post — because Windows
// delivers WM_HOTKEY to the thread that registered the key, and registering
// from anywhere else means a shortcut that fires into a queue nobody reads.
//
// onPress runs on its own goroutine, not on the UI thread: a rewrite takes
// seconds and the message loop has to keep running throughout.
//
// The returned function unregisters it.
func (u *UIThread) RegisterHotKey(h settings.HotKey, onPress func()) (func(), error) {
	binding, err := TranslateHotKey(h)
	if err != nil {
		return nil, err
	}

	u.mu.Lock()
	id := u.nextID
	u.nextID++
	u.hotkeys[id] = onPress
	u.mu.Unlock()

	unbind := func() {
		u.mu.Lock()
		delete(u.hotkeys, id)
		u.mu.Unlock()
		procUnregisterHotKey.Call(u.hwnd, uintptr(id))
	}

	result, _, callErr := procRegisterHotKey.Call(
		u.hwnd,
		uintptr(id),
		uintptr(binding.Modifiers),
		uintptr(binding.VirtualKey),
	)
	if result == 0 {
		u.mu.Lock()
		delete(u.hotkeys, id)
		u.mu.Unlock()

		if errors.Is(callErr, errorHotKeyAlreadyRegistered) {
			// The message matters more than the error here: this is the one
			// failure a user is likely to hit and the least self-explanatory.
			return nil, fmt.Errorf("%w: %s", ErrHotKeyTaken, HotKeyConflictHelp(h))
		}
		return nil, fmt.Errorf("registering %s: %w", h, callErr)
	}

	return unbind, nil
}
