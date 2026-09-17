package win32

import (
	"context"
	"fmt"
	"sync/atomic"
	"unsafe"

	"github.com/srikary12/starch/apps/desktop/internal/desktop"
	"golang.org/x/sys/windows"
)

var (
	gdi32 = windows.NewLazySystemDLL("gdi32.dll")

	procShowWindow      = user32.NewProc("ShowWindow")
	procInvalidateRect  = user32.NewProc("InvalidateRect")
	procBeginPaint      = user32.NewProc("BeginPaint")
	procEndPaint        = user32.NewProc("EndPaint")
	procFillRect        = user32.NewProc("FillRect")
	procDrawText        = user32.NewProc("DrawTextW")
	procSetLayeredAttrs = user32.NewProc("SetLayeredWindowAttributes")
	procGetSystemMetric = user32.NewProc("GetSystemMetrics")
	procSetWindowsHook  = user32.NewProc("SetWindowsHookExW")
	procUnhookWindows   = user32.NewProc("UnhookWindowsHookEx")
	procCallNextHook    = user32.NewProc("CallNextHookEx")
	procGetClientRect   = user32.NewProc("GetClientRect")

	procCreateFont   = gdi32.NewProc("CreateFontW")
	procSelectObject = gdi32.NewProc("SelectObject")
	procSetTextColor = gdi32.NewProc("SetTextColor")
	procSetBkMode    = gdi32.NewProc("SetBkMode")
	procCreateBrush  = gdi32.NewProc("CreateSolidBrush")
	procDeleteObject = gdi32.NewProc("DeleteObject")
)

const (
	wmPaint = 0x000F

	wsPopup       = 0x80000000
	wsExLayered   = 0x00080000
	wsExTopmost   = 0x00000008
	wsExNoActive  = 0x08000000
	wsExToolWin   = 0x00000080
	swShowNoAct   = 8 // SW_SHOWNA: show without activating
	lwaAlpha      = 0x00000002
	transparentBk = 1

	dtWordBreak = 0x0010
	dtNoPrefix  = 0x0800

	whKeyboardLL  = 13
	wmKeyDown     = 0x0100
	wmSysKeyDown  = 0x0104
	hookSwallowed = 1

	// Padding inside the window. The rest of the layout lives in place.go,
	// which is untagged so the clamping can be tested.
	overlayPadding = 16
)

type rect struct{ left, top, right, bottom int32 }

type paintStruct struct {
	hdc         uintptr
	erase       int32
	paint       rect
	restore     int32
	incUpdate   int32
	rgbReserved [32]byte
}

type kbdHook struct {
	vkCode    uint32
	scanCode  uint32
	flags     uint32
	time      uint32
	extraInfo uintptr
}

// Overlay is the window showing a rewrite as it streams.
//
// Layered, topmost, and above all non-activating: WS_EX_NOACTIVATE keeps focus
// in the application the text came from. That is not cosmetic. Take focus away
// and the selection stops being a selection in many editors, and the paste that
// follows lands wherever focus went — which could be this window.
//
// The cost of never taking focus is that no key message ever arrives, so the
// keys come from a low-level keyboard hook instead. The macOS shell has it
// easier here: AppKit's non-activating panel can hold key focus without
// activating its application, and Win32 has no equivalent.
type Overlay struct {
	ui    *UIThread
	state *overlayState
	hwnd  uintptr
	hook  uintptr

	closed atomic.Bool
}

// activeOverlay is the overlay the keyboard hook feeds.
//
// A package-level pointer for the same reason as `active`: a hook procedure is
// a bare callback. There is at most one overlay at a time — the flow is one
// rewrite at a time by construction — so this is a description rather than a
// restriction.
var activeOverlay atomic.Pointer[Overlay]

// Delta, Done, Fail and Await satisfy desktop.Overlay by way of the state,
// repainting where the visible text changes.
func (o *Overlay) Delta(fragment string) { o.state.Delta(fragment); o.repaint() }
func (o *Overlay) Done(full string)      { o.state.Done(full); o.repaint() }
func (o *Overlay) Fail(err error)        { o.state.Fail(err); o.repaint() }

func (o *Overlay) Await(ctx context.Context) (desktop.Decision, error) {
	return o.state.Await(ctx)
}

// Close takes the overlay off screen and removes the hook.
//
// Idempotent, and the hook removal is the part that matters: a low-level
// keyboard hook left installed would go on swallowing Escape across the whole
// machine for as long as this process lived.
func (o *Overlay) Close() {
	if !o.closed.CompareAndSwap(false, true) {
		return
	}
	activeOverlay.CompareAndSwap(o, nil)

	done := make(chan struct{})
	o.ui.Post(func() {
		defer close(done)
		if o.hook != 0 {
			procUnhookWindows.Call(o.hook)
			o.hook = 0
		}
		if o.hwnd != 0 {
			procDestroyWindow.Call(o.hwnd)
			o.hwnd = 0
		}
	})
	<-done
}

func (o *Overlay) repaint() {
	if o.closed.Load() {
		return
	}
	o.ui.Post(func() {
		if o.hwnd != 0 {
			procInvalidateRect.Call(o.hwnd, 0, 1)
		}
	})
}

// ShowOverlay opens an overlay near the captured text.
func (u *UIThread) ShowOverlay(c desktop.Capture) (*Overlay, error) {
	overlay := &Overlay{ui: u, state: newOverlayState()}

	created := make(chan error, 1)
	u.Post(func() { created <- overlay.create(c.Bounds) })
	if err := <-created; err != nil {
		return nil, err
	}

	activeOverlay.Store(overlay)
	return overlay, nil
}

func (o *Overlay) create(near desktop.Rect) error {
	name, err := windows.UTF16PtrFromString(
		fmt.Sprintf("StarchOverlay.%d", windows.GetCurrentProcessId()))
	if err != nil {
		return fmt.Errorf("building a window class name: %w", err)
	}

	var instance windows.Handle
	if err := windows.GetModuleHandleEx(0, nil, &instance); err != nil {
		return fmt.Errorf("locating this module: %w", err)
	}

	class := wndClassExW{
		size:      uint32(unsafe.Sizeof(wndClassExW{})),
		wndProc:   windows.NewCallback(overlayWndProc),
		instance:  instance,
		className: name,
	}
	// A duplicate class is not an error here: a second rewrite in the same
	// process reuses it.
	procRegisterClassEx.Call(uintptr(unsafe.Pointer(&class)))

	x, y, width, height := o.place(near)
	hwnd, _, callErr := procCreateWindowEx.Call(
		wsExLayered|wsExTopmost|wsExNoActive|wsExToolWin,
		uintptr(unsafe.Pointer(name)), uintptr(unsafe.Pointer(name)),
		wsPopup,
		uintptr(x), uintptr(y), uintptr(width), uintptr(height),
		0, 0, uintptr(instance), 0,
	)
	if hwnd == 0 {
		return fmt.Errorf("creating the overlay window: %w", callErr)
	}
	o.hwnd = hwnd

	// Slightly translucent, so what is underneath stays legible — the user is
	// comparing a rewrite against the text it came from.
	procSetLayeredAttrs.Call(hwnd, 0, 244, lwaAlpha)

	// SW_SHOWNA rather than SW_SHOW: showing must not activate, or the
	// WS_EX_NOACTIVATE above is undone at the moment it matters.
	procShowWindow.Call(hwnd, swShowNoAct)

	hook, _, hookErr := procSetWindowsHook.Call(
		whKeyboardLL, windows.NewCallback(keyboardHook), 0, 0,
	)
	if hook == 0 {
		procDestroyWindow.Call(hwnd)
		o.hwnd = 0
		return fmt.Errorf("watching for Enter and Escape: %w", hookErr)
	}
	o.hook = hook
	return nil
}

// place asks the screen how big it is and hands the arithmetic to placeOverlay,
// which is in an untagged file so the clamping is tested.
func (o *Overlay) place(near desktop.Rect) (x, y, width, height int32) {
	screenWidth, _, _ := procGetSystemMetric.Call(0)  // SM_CXSCREEN
	screenHeight, _, _ := procGetSystemMetric.Call(1) // SM_CYSCREEN
	return placeOverlay(near, int32(screenWidth), int32(screenHeight))
}

func overlayWndProc(hwnd uintptr, message uint32, wParam, lParam uintptr) uintptr {
	if message != wmPaint {
		result, _, _ := procDefWindowProc.Call(hwnd, uintptr(message), wParam, lParam)
		return result
	}

	overlay := activeOverlay.Load()
	if overlay == nil {
		result, _, _ := procDefWindowProc.Call(hwnd, uintptr(message), wParam, lParam)
		return result
	}
	overlay.paint(hwnd)
	return 0
}

func (o *Overlay) paint(hwnd uintptr) {
	var ps paintStruct
	hdc, _, _ := procBeginPaint.Call(hwnd, uintptr(unsafe.Pointer(&ps)))
	if hdc == 0 {
		return
	}
	defer procEndPaint.Call(hwnd, uintptr(unsafe.Pointer(&ps)))

	var client rect
	procGetClientRect.Call(hwnd, uintptr(unsafe.Pointer(&client)))

	background, _, _ := procCreateBrush.Call(colorRef(32, 32, 32))
	defer procDeleteObject.Call(background)
	procFillRect.Call(hdc, uintptr(unsafe.Pointer(&client)), background)

	showing := o.state.view()

	font := newUIFont(15)
	defer procDeleteObject.Call(font)
	previous, _, _ := procSelectObject.Call(hdc, font)
	defer procSelectObject.Call(hdc, previous)

	procSetBkMode.Call(hdc, transparentBk)
	if showing.Failed {
		procSetTextColor.Call(hdc, colorRef(255, 140, 140))
	} else {
		procSetTextColor.Call(hdc, colorRef(240, 240, 240))
	}

	body := rect{
		left:   client.left + overlayPadding,
		top:    client.top + overlayPadding,
		right:  client.right - overlayPadding,
		bottom: client.bottom - overlayPadding - 24,
	}
	drawText(hdc, showing.Text, &body, dtWordBreak|dtNoPrefix)

	hint := rect{
		left:   client.left + overlayPadding,
		top:    client.bottom - overlayPadding - 18,
		right:  client.right - overlayPadding,
		bottom: client.bottom - overlayPadding,
	}
	procSetTextColor.Call(hdc, colorRef(150, 150, 150))
	drawText(hdc, showing.Hint, &hint, dtNoPrefix)
}

func drawText(hdc uintptr, text string, area *rect, format uint32) {
	if text == "" {
		return
	}
	encoded, err := windows.UTF16FromString(text)
	if err != nil {
		return
	}
	procDrawText.Call(
		hdc,
		uintptr(unsafe.Pointer(&encoded[0])),
		uintptr(len(encoded)-1), // without the NUL
		uintptr(unsafe.Pointer(area)),
		uintptr(format),
	)
}

// colorRef is Win32's COLORREF, which is 0x00BBGGRR rather than RGB.
func colorRef(r, g, b uint32) uintptr {
	return uintptr(r | g<<8 | b<<16)
}

func newUIFont(height int32) uintptr {
	face, err := windows.UTF16PtrFromString("Segoe UI")
	if err != nil {
		return 0
	}
	const defaultCharSet = 1
	font, _, _ := procCreateFont.Call(
		uintptr(height), 0, 0, 0,
		400, // FW_NORMAL
		0, 0, 0,
		defaultCharSet,
		0, 0, 0, 0,
		uintptr(unsafe.Pointer(face)),
	)
	return font
}

// keyboardHook is the low-level keyboard hook.
//
// It runs on the UI thread, and Windows silently removes a hook whose procedure
// is slow — LowLevelHooksTimeout, five seconds by default, dropped to a fraction
// of that on some systems. So this does as little as possible: read the virtual
// key, hand it to the state machine, return. Everything the overlay does in
// response happens on another goroutine.
func keyboardHook(code int32, wParam, lParam uintptr) uintptr {
	if code < 0 {
		next, _, _ := procCallNextHook.Call(0, uintptr(code), wParam, lParam)
		return next
	}

	if wParam == wmKeyDown || wParam == wmSysKeyDown {
		if overlay := activeOverlay.Load(); overlay != nil {
			event := (*kbdHook)(globalPointer(lParam))
			if overlay.state.key(event.vkCode) {
				// Swallowed: the application underneath never sees it. Only
				// ever Enter and Escape, and only while an overlay is up.
				return hookSwallowed
			}
		}
	}

	next, _, _ := procCallNextHook.Call(0, uintptr(code), wParam, lParam)
	return next
}
