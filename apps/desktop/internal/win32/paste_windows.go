package win32

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// Pasting the rewrite, and knowing when it was actually taken.
//
// The hazard is the mirror of the one in clipboard_windows.go. SendInput
// delivers Ctrl-V and returns; the target application reads the clipboard on
// its own schedule, some milliseconds later. Put the rewrite on the clipboard,
// paste, and restore the user's clipboard straight away, and a slow
// application reads the restored contents — pasting the user's previous
// clipboard into their document in place of the rewrite they just approved.
//
// The macOS shell handles this by waiting 120ms and calling it the price of the
// clipboard path, which is honest and is a guess. Windows offers something
// better, and the message loop needed for it already exists for the hot key:
// delayed rendering. The format is published with a null handle, and the
// application that pastes causes WM_RENDERFORMAT to arrive here, at which point
// the data is supplied and the paste is known to have consumed it.
//
// So the wait below is bounded but rarely reached: normally the render message
// ends it, which is a fact rather than an estimate.

const (
	wmRenderFormat     = 0x0305
	wmRenderAllFormats = 0x0306
	wmDestroyClipboard = 0x0307
)

// pendingPaste is the rewrite waiting to be rendered.
type pendingPaste struct {
	text string
	// rendered closes when the target application asks for the data.
	rendered chan struct{}
	once     sync.Once
}

func (p *pendingPaste) done() {
	p.once.Do(func() { close(p.rendered) })
}

// PasteTimeout bounds the wait for an application that never reads the
// clipboard — one where the paste shortcut did nothing at all. It is long
// enough not to cut off a slow editor and short enough that the user is not
// left staring at a finished overlay.
const PasteTimeout = 2 * time.Second

// Paste puts text on the clipboard, sends Ctrl-V, and returns once the target
// application has taken the data or the wait runs out.
//
// The caller restores the user's clipboard afterwards — the flow does it in a
// defer — and it is safe to do so the moment this returns, which is the whole
// point of waiting here.
func (u *UIThread) Paste(ctx context.Context, text string) error {
	if text == "" {
		return fmt.Errorf("nothing to paste")
	}

	pending := &pendingPaste{text: text, rendered: make(chan struct{})}

	// Publishing has to happen on the UI thread: it makes this window the
	// clipboard owner, and a window can only own the clipboard from the thread
	// that created it.
	published := make(chan error, 1)
	u.Post(func() { published <- u.publish(pending) })

	select {
	case err := <-published:
		if err != nil {
			return err
		}
	case <-ctx.Done():
		return ctx.Err()
	}

	if err := (Keyboard{}).Paste(ctx); err != nil {
		u.clearPending()
		return err
	}

	timer := time.NewTimer(PasteTimeout)
	defer timer.Stop()

	select {
	case <-pending.rendered:
		return nil
	case <-timer.C:
		u.clearPending()
		// Not an error the user needs to see as a failure of ours: the
		// keystroke was delivered and the application did not act on it.
		return fmt.Errorf("the application did not accept the paste within %v", PasteTimeout)
	case <-ctx.Done():
		u.clearPending()
		return ctx.Err()
	}
}

// publish claims the clipboard and advertises the format without the data.
func (u *UIThread) publish(pending *pendingPaste) error {
	u.mu.Lock()
	u.pending = pending
	u.mu.Unlock()

	// Our own window handle this time, rather than null: EmptyClipboard makes
	// the opener the clipboard owner, and only the owner is sent
	// WM_RENDERFORMAT.
	result, _, err := procOpenClipboard.Call(u.hwnd)
	if result == 0 {
		u.clearPending()
		return fmt.Errorf("the clipboard is held by another program: %w", err)
	}
	defer procCloseClipboard.Call()

	if result, _, err := procEmptyClipboard.Call(); result == 0 {
		u.clearPending()
		return fmt.Errorf("claiming the clipboard: %w", err)
	}

	// A null handle is the request to be asked later. This is what makes the
	// paste observable.
	if result, _, err := procSetClipboardData.Call(cfUnicodeText, 0); result == 0 {
		u.clearPending()
		return fmt.Errorf("advertising the rewrite on the clipboard: %w", err)
	}
	return nil
}

func (u *UIThread) clearPending() {
	u.mu.Lock()
	pending := u.pending
	u.pending = nil
	u.mu.Unlock()
	if pending != nil {
		pending.done()
	}
}

// render answers WM_RENDERFORMAT with the data we promised.
//
// Called on the UI thread from inside the window procedure, which is why it
// must not open or close the clipboard: the requesting application already has
// it open, and doing so here would deadlock the paste it is in the middle of.
func (u *UIThread) render(format uint32) {
	if format != cfUnicodeText {
		return
	}

	u.mu.Lock()
	pending := u.pending
	u.mu.Unlock()
	if pending == nil {
		return
	}

	handle, err := newGlobalString(pending.text)
	if err != nil {
		// Nothing useful to do here — the application is waiting on a message
		// reply, not on us. Releasing the waiter is better than hanging it.
		pending.done()
		return
	}
	procSetClipboardData.Call(cfUnicodeText, handle)

	// The data has been handed over, so the paste has consumed it and the
	// user's clipboard can safely be put back.
	pending.done()
}
