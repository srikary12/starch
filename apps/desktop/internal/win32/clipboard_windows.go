package win32

import (
	"context"
	"fmt"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	kernel32 = windows.NewLazySystemDLL("kernel32.dll")

	procGetClipboardSequence = user32.NewProc("GetClipboardSequenceNumber")
	procOpenClipboard        = user32.NewProc("OpenClipboard")
	procCloseClipboard       = user32.NewProc("CloseClipboard")
	procEmptyClipboard       = user32.NewProc("EmptyClipboard")
	procGetClipboardData     = user32.NewProc("GetClipboardData")
	procSetClipboardData     = user32.NewProc("SetClipboardData")
	procSendInput            = user32.NewProc("SendInput")

	procGlobalAlloc  = kernel32.NewProc("GlobalAlloc")
	procGlobalLock   = kernel32.NewProc("GlobalLock")
	procGlobalUnlock = kernel32.NewProc("GlobalUnlock")
	procGlobalSize   = kernel32.NewProc("GlobalSize")
)

const (
	cfUnicodeText = 13
	gmemMoveable  = 0x0002
)

// Clipboard is the Windows clipboard, as the capture round trip needs it.
//
// Text only, deliberately and with a cost. A faithful save and restore of every
// format is not possible for any clipboard client: delayed-rendering formats
// are produced on demand by the owning application and cannot be read without
// destroying them, and owner-drawn formats are drawn by a window that will not
// be asked again. So an image or a spreadsheet range on the clipboard when the
// hot key is pressed does not survive the rewrite.
//
// It would not have survived anyway — the copy this shell synthesizes is
// performed by the focused application, and that is what overwrites the
// clipboard. What this type controls is only whether the user's *text* comes
// back, and whether the captured selection is left sitting there afterwards. It
// is documented in MANUAL_TESTS.md rather than left for someone to discover.
type Clipboard struct{}

// Serial is GetClipboardSequenceNumber: it changes whenever the contents do.
//
// This is what makes the capture safe. Comparing contents would not do: an
// application that ignores the copy leaves the previous contents in place, and
// they would read as a perfectly good selection.
func (Clipboard) Serial() (uint64, error) {
	serial, _, _ := procGetClipboardSequence.Call()
	// Documented to return zero only when the process cannot access the
	// clipboard at all, which is a station-isolation case rather than an
	// ordinary error.
	if serial == 0 {
		return 0, fmt.Errorf("this process cannot observe the clipboard")
	}
	return uint64(serial), nil
}

// Text returns the clipboard's Unicode text, or "" when it holds none.
func (c Clipboard) Text() (string, error) {
	var text string
	err := c.withClipboard(func() error {
		handle, _, _ := procGetClipboardData.Call(cfUnicodeText)
		if handle == 0 {
			// No text on the clipboard is not an error: an image is a normal
			// thing for it to hold.
			return nil
		}
		var err error
		text, err = readGlobalString(handle)
		return err
	})
	return text, err
}

// Save captures the clipboard's text and returns a function restoring it.
//
// When there was no text, the restore *empties* the clipboard rather than
// leaving things as it finds them. That is the deliberate choice: by then the
// clipboard holds the selection this shell caused to be copied, and leaving it
// there would mean the hot key silently replaced whatever the user had — which
// is the behaviour this whole round trip exists to avoid.
func (c Clipboard) Save() (func() error, error) {
	saved, err := c.Text()
	if err != nil {
		return nil, err
	}

	return func() error {
		return c.withClipboard(func() error {
			if result, _, err := procEmptyClipboard.Call(); result == 0 {
				return fmt.Errorf("clearing the clipboard: %w", err)
			}
			if saved == "" {
				return nil
			}
			handle, err := newGlobalString(saved)
			if err != nil {
				return err
			}
			// SetClipboardData takes ownership of the block on success, so it
			// must not be freed here; on failure it is leaked rather than
			// risking a double free of memory the system may already hold.
			if result, _, err := procSetClipboardData.Call(cfUnicodeText, handle); result == 0 {
				return fmt.Errorf("putting the clipboard back: %w", err)
			}
			return nil
		})
	}, nil
}

// withClipboard opens the clipboard, runs fn, and closes it.
//
// Opening fails while another process holds it, which is ordinary rather than
// exceptional — clipboard managers and remote desktop agents both poll it — so
// this retries briefly instead of giving up on the first refusal.
func (Clipboard) withClipboard(fn func() error) error {
	const attempts = 10
	var lastErr error
	for range attempts {
		// A null window handle associates the clipboard with the current task,
		// which is what a console or message-only process wants.
		if result, _, err := procOpenClipboard.Call(0); result != 0 {
			defer procCloseClipboard.Call()
			return fn()
		} else {
			lastErr = err
		}
		time.Sleep(10 * time.Millisecond)
	}
	return fmt.Errorf("the clipboard is held by another program: %w", lastErr)
}

// globalPointer turns the address GlobalLock returns into a pointer.
//
// This is a deliberate escape from go vet's unsafeptr check, and the reason is
// worth stating rather than hiding behind the helper. That check exists because
// a uintptr holding the address of *Go-managed* memory can go stale: the
// collector may move or free the object while only an integer refers to it, and
// converting back later then yields a pointer to nothing.
//
// None of that applies here. GlobalAlloc allocated this block, the Windows heap
// owns it, GlobalLock pins it for as long as we hold the lock, and the Go
// collector neither knows nor cares about it. The check has no way to tell
// those two cases apart — golang.org/x/sys ships no typed wrapper for the
// Global heap, so there is no vetted path to this memory at all.
//
// It is done once, here, so the reasoning lives in one place instead of being
// re-argued at four call sites.
func globalPointer(address uintptr) unsafe.Pointer {
	return *(*unsafe.Pointer)(unsafe.Pointer(&address))
}

func readGlobalString(handle uintptr) (string, error) {
	pointer, _, err := procGlobalLock.Call(handle)
	if pointer == 0 {
		return "", fmt.Errorf("reading the clipboard: %w", err)
	}
	defer procGlobalUnlock.Call(handle)

	size, _, _ := procGlobalSize.Call(handle)
	if size == 0 {
		return "", nil
	}
	// The block is UTF-16 and NUL-terminated; UTF16PtrToString stops at the
	// NUL, so a block padded beyond its string does not leak the padding.
	return windows.UTF16PtrToString((*uint16)(globalPointer(pointer))), nil
}

func newGlobalString(s string) (uintptr, error) {
	encoded, err := windows.UTF16FromString(s)
	if err != nil {
		return 0, fmt.Errorf("the saved clipboard text cannot be encoded: %w", err)
	}

	size := uintptr(len(encoded) * 2)
	handle, _, allocErr := procGlobalAlloc.Call(gmemMoveable, size)
	if handle == 0 {
		return 0, fmt.Errorf("allocating for the clipboard: %w", allocErr)
	}

	pointer, _, lockErr := procGlobalLock.Call(handle)
	if pointer == 0 {
		return 0, fmt.Errorf("allocating for the clipboard: %w", lockErr)
	}
	copy(unsafe.Slice((*uint16)(globalPointer(pointer)), len(encoded)), encoded)
	procGlobalUnlock.Call(handle)

	return handle, nil
}

// Keyboard synthesizes keystrokes into whatever application has focus.
type Keyboard struct{}

// Copy sends Ctrl-C.
func (k Keyboard) Copy(ctx context.Context) error { return k.send(ctx, copyChord(), "copy") }

// Paste sends Ctrl-V.
func (k Keyboard) Paste(ctx context.Context) error { return k.send(ctx, pasteChord(), "paste") }

func (Keyboard) send(ctx context.Context, events []input, what string) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	sent, _, err := procSendInput.Call(
		uintptr(len(events)),
		uintptr(unsafe.Pointer(&events[0])),
		uintptr(inputSize),
	)
	if int(sent) != len(events) {
		// The likeliest cause by far is UIPI: a process at a lower integrity
		// level cannot send input to an elevated window, and Windows reports it
		// by silently accepting fewer events rather than by failing. Saying so
		// is the difference between a puzzling no-op and an obvious one.
		return fmt.Errorf("sending %s reached %d of %d keystrokes; the focused "+
			"window may be running as administrator, which blocks input from "+
			"programs that are not: %w", what, sent, len(events), err)
	}
	return nil
}
