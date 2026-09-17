package win32

import (
	"context"
	"fmt"
	"runtime"
	"sync"
	"sync/atomic"
	"unsafe"

	"golang.org/x/sys/windows"
)

// The one OS thread that every window in this process lives on.
//
// Win32 ties windows to the thread that created them: messages for a window are
// delivered to that thread's queue and nowhere else, and RegisterHotKey targets
// a window or a thread rather than a process. So there is exactly one of these,
// it is pinned with runtime.LockOSThread, and everything that touches a window
// runs on it.
//
// The work queue is the part that matters. A hot key press starts a rewrite
// that takes seconds, and doing that work inline would stop the message loop —
// which is the same loop that has to be drawing the overlay the rewrite just
// opened. So the handler runs elsewhere and posts back here for anything that
// touches a window. This is the same arrangement AppKit imposes on the macOS
// shell; Win32 just does not enforce it for you.

var (
	user32              = windows.NewLazySystemDLL("user32.dll")
	procRegisterClassEx = user32.NewProc("RegisterClassExW")
	procCreateWindowEx  = user32.NewProc("CreateWindowExW")
	procDestroyWindow   = user32.NewProc("DestroyWindow")
	procDefWindowProc   = user32.NewProc("DefWindowProcW")
	procGetMessage      = user32.NewProc("GetMessageW")
	procTranslateMsg    = user32.NewProc("TranslateMessage")
	procDispatchMessage = user32.NewProc("DispatchMessageW")
	procPostMessage     = user32.NewProc("PostMessageW")
	procPostQuitMessage = user32.NewProc("PostQuitMessage")
)

const (
	wmDestroy = 0x0002
	wmHotKey  = 0x0312
	wmApp     = 0x8000

	// Posted by Post to drain the work queue on this thread.
	wmRunWork = wmApp + 1
	// Posted by Run's watcher when the context ends.
	wmShutdown = wmApp + 2

	// HWND_MESSAGE. A message-only window: never shown, never in the task bar,
	// never in Alt-Tab, but it owns a message queue, which is all that is
	// wanted.
	hwndMessage = ^uintptr(2) // (HWND)-3
)

type msg struct {
	hwnd    uintptr
	message uint32
	wParam  uintptr
	lParam  uintptr
	time    uint32
	pt      struct{ x, y int32 }
	private uint32
}

type wndClassExW struct {
	size       uint32
	style      uint32
	wndProc    uintptr
	clsExtra   int32
	wndExtra   int32
	instance   windows.Handle
	icon       windows.Handle
	cursor     windows.Handle
	background windows.Handle
	menuName   *uint16
	className  *uint16
	iconSm     windows.Handle
}

// UIThread owns the message loop and the windows on it.
type UIThread struct {
	hwnd uintptr

	// work is drained on this thread. Its own type, in an untagged file, so the
	// one part of this that can deadlock is tested off Windows.
	work workQueue

	mu      sync.Mutex
	hotkeys map[int32]func()
	nextID  int32
	// pending is the rewrite advertised on the clipboard but not yet handed
	// over. See paste_windows.go.
	pending *pendingPaste

	// ready closes once hwnd is usable, so Post from another goroutine cannot
	// race the window into existence.
	ready   chan struct{}
	started sync.Once
}

// NewUIThread creates one. It does nothing until Run is called.
func NewUIThread() *UIThread {
	return &UIThread{
		hotkeys: make(map[int32]func()),
		nextID:  1,
		ready:   make(chan struct{}),
	}
}

// Post runs fn on the UI thread, and returns without waiting for it.
//
// Safe from any goroutine, and the only safe way to touch a window from one.
// Calls made before Run has a window are held until it does rather than
// dropped, because the hot key can in principle be pressed during startup.
func (u *UIThread) Post(fn func()) {
	u.work.add(fn)

	select {
	case <-u.ready:
		postMessage(u.hwnd, wmRunWork)
	default:
		// Run drains the queue once it is up.
	}
}

// Run creates the message-only window and pumps messages until ctx ends.
//
// It locks the calling goroutine to its thread for its whole life and never
// unlocks it: every window created from Post belongs to this thread, and
// letting the Go runtime move the goroutine afterwards would leave those
// windows owned by a thread with no message loop.
func (u *UIThread) Run(ctx context.Context) error {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	if err := u.createWindow(); err != nil {
		return err
	}
	defer procDestroyWindow.Call(u.hwnd)

	u.started.Do(func() { close(u.ready) })
	// Anything posted before the window existed.
	u.work.drain()

	watching, stopWatching := context.WithCancel(ctx)
	defer stopWatching()
	go func() {
		<-watching.Done()
		postMessage(u.hwnd, wmShutdown)
	}()

	return u.pump()
}

func (u *UIThread) pump() error {
	var m msg
	for {
		// GetMessageW returns 0 for WM_QUIT and -1 for an error, which is why
		// the result is signed and why a plain "not zero" test would loop
		// forever on a broken window handle.
		result, _, err := procGetMessage.Call(
			uintptr(unsafe.Pointer(&m)), 0, 0, 0,
		)
		switch int32(result) {
		case -1:
			return fmt.Errorf("reading the message queue: %w", err)
		case 0:
			return nil
		}

		switch m.message {
		case wmRunWork:
			u.work.drain()
			continue
		case wmShutdown:
			return nil
		case wmHotKey:
			u.fireHotKey(int32(m.wParam))
			continue
		}

		procTranslateMsg.Call(uintptr(unsafe.Pointer(&m)))
		procDispatchMessage.Call(uintptr(unsafe.Pointer(&m)))
	}
}

func (u *UIThread) fireHotKey(id int32) {
	u.mu.Lock()
	fn := u.hotkeys[id]
	u.mu.Unlock()
	if fn == nil {
		return
	}
	// In a goroutine, deliberately. A rewrite takes seconds and this thread has
	// to keep pumping messages throughout, not least for the overlay the
	// rewrite is about to open.
	go fn()
}

func (u *UIThread) createWindow() error {
	// The module handle for this executable. x/sys exposes only the Ex form,
	// and a nil name asks for the process's own image, which is what a window
	// class wants.
	var instance windows.Handle
	if err := windows.GetModuleHandleEx(0, nil, &instance); err != nil {
		return fmt.Errorf("locating this module: %w", err)
	}

	// Named for the product and this process, so two running copies cannot
	// collide on the class name — RegisterClassEx fails on a duplicate.
	name, err := windows.UTF16PtrFromString(
		fmt.Sprintf("StarchMessageWindow.%d", windows.GetCurrentProcessId()))
	if err != nil {
		return fmt.Errorf("building a window class name: %w", err)
	}

	class := wndClassExW{
		size:      uint32(unsafe.Sizeof(wndClassExW{})),
		wndProc:   windows.NewCallback(wndProc),
		instance:  instance,
		className: name,
	}
	if atom, _, err := procRegisterClassEx.Call(uintptr(unsafe.Pointer(&class))); atom == 0 {
		return fmt.Errorf("registering the window class: %w", err)
	}

	hwnd, _, err := procCreateWindowEx.Call(
		0, uintptr(unsafe.Pointer(name)), uintptr(unsafe.Pointer(name)),
		0, 0, 0, 0, 0,
		hwndMessage, 0, uintptr(instance), 0,
	)
	if hwnd == 0 {
		return fmt.Errorf("creating the message window: %w", err)
	}
	u.hwnd = hwnd
	active.Store(u)
	return nil
}

// active is the process's one UI thread.
//
// A package-level pointer because a window procedure is a bare C callback with
// nowhere to hang a receiver, and the alternative — stashing the pointer in the
// window's user data and reading it back through GetWindowLongPtr — buys
// nothing here. There is exactly one UI thread by design: Win32 ties windows to
// the thread that made them, so a second one would be a second set of windows
// nothing could talk to.
var active atomic.Pointer[UIThread]

func wndProc(hwnd uintptr, message uint32, wParam, lParam uintptr) uintptr {
	switch message {
	case wmDestroy:
		procPostQuitMessage.Call(0)
		return 0

	case wmRenderFormat:
		// An application is pasting and has asked for the data we advertised.
		if u := active.Load(); u != nil {
			u.render(uint32(wParam))
		}
		return 0

	case wmRenderAllFormats:
		// The clipboard is being handed on while we still owe it data, which
		// happens when this process is closing. Render so the user's paste is
		// not lost.
		if u := active.Load(); u != nil {
			u.render(cfUnicodeText)
		}
		return 0

	case wmTrayCallback:
		// The shell packs the mouse event into lParam.
		trayMessage(lParam)
		return 0

	case wmDestroyClipboard:
		// Someone else took ownership before pasting ours. Whatever was waiting
		// is never going to be rendered, so release it rather than let it sit
		// out the full timeout.
		if u := active.Load(); u != nil {
			u.clearPending()
		}
		return 0
	}

	result, _, _ := procDefWindowProc.Call(hwnd, uintptr(message), wParam, lParam)
	return result
}

func postMessage(hwnd uintptr, message uint32) {
	procPostMessage.Call(hwnd, uintptr(message), 0, 0)
}
