package desktop

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// The copy round trip: save the clipboard, press Copy, read what landed, put
// the clipboard back.
//
// Neutral rather than per-platform because the thing that makes it correct is
// not a syscall, it is a guarantee — and it is the guarantee most likely to be
// got wrong, in a way that quietly corrupts somebody's document.
//
// The hazard: if the focused application ignores the synthetic Ctrl-C, the
// clipboard still holds whatever the user last copied. Read it and you have a
// perfectly plausible-looking selection that has nothing to do with what is on
// screen. The user then sees a rewrite of their last copied text, presses
// Enter, and it replaces a selection it was never derived from.
//
// So the clipboard's serial number is checked rather than its contents. No
// change means the application did not copy, which is reported as an empty
// selection — never as the text that happened to be sitting there.

// Clipboard is the platform's clipboard, as the round trip needs it.
type Clipboard interface {
	// Serial changes whenever the clipboard's contents change. On Windows this
	// is GetClipboardSequenceNumber; every platform has some equivalent, and a
	// platform without one cannot implement this safely.
	Serial() (uint64, error)

	// Text returns the clipboard's text, or "" when it holds none.
	Text() (string, error)

	// Save captures the current contents and returns a function restoring
	// them. The returned function is called on every path, including those
	// where the copy failed.
	Save() (restore func() error, err error)
}

// Keyboard synthesizes the keystrokes the round trip needs.
type Keyboard interface {
	// Copy sends the platform's copy chord to the focused application.
	Copy(ctx context.Context) error
	// Paste sends the paste chord.
	Paste(ctx context.Context) error
}

// ErrClipboardUnchanged reports an application that did not answer the copy.
//
// Its own error because it is not a failure in the ordinary sense: the shortcut
// worked, the application simply does not let other programs read its
// selection, which is true of a great deal of software and is worth saying
// plainly rather than reporting as a timeout.
var ErrClipboardUnchanged = errors.New("the application did not put the selection on the clipboard")

// CopyRoundTrip captures a selection by copying it.
type CopyRoundTrip struct {
	Clipboard Clipboard
	Keyboard  Keyboard

	// Settle is how long to wait for the clipboard to change after Copy.
	// Applications answer a synthetic Ctrl-C asynchronously and some are slow
	// about it, so this cannot be zero; it is also in the user's latency budget
	// for every rewrite, so it cannot be generous.
	Settle time.Duration

	// Interval is how often to look.
	Interval time.Duration

	// Sleep is injected so the tests do not spend real time. nil means the
	// real clock.
	Sleep func(ctx context.Context, d time.Duration) error
}

// Default timings. 300ms is chosen against the 500ms first-token budget: the
// round trip happens before a single byte is requested from the provider, so
// anything spent here is spent twice over in the user's perception.
const (
	DefaultSettle   = 300 * time.Millisecond
	DefaultInterval = 10 * time.Millisecond
)

// Capture performs the round trip.
//
// The returned Capture always carries a Restore, even on the error paths —
// especially on the error paths. A failure part-way through is exactly when the
// clipboard is most likely to be holding something the user did not put there.
func (r CopyRoundTrip) Capture(ctx context.Context) (Capture, error) {
	settle, interval := r.Settle, r.Interval
	if settle <= 0 {
		settle = DefaultSettle
	}
	if interval <= 0 {
		interval = DefaultInterval
	}

	restore, err := r.Clipboard.Save()
	if err != nil {
		return Capture{ViaClipboard: true}, fmt.Errorf("saving the clipboard: %w", err)
	}
	captured := Capture{ViaClipboard: true, Restore: restore}

	before, err := r.Clipboard.Serial()
	if err != nil {
		return captured, fmt.Errorf("reading the clipboard state: %w", err)
	}

	if err := r.Keyboard.Copy(ctx); err != nil {
		return captured, fmt.Errorf("sending the copy shortcut: %w", err)
	}

	changed, err := r.awaitChange(ctx, before, settle, interval)
	if err != nil {
		return captured, err
	}
	if !changed {
		// Deliberately not returning whatever the clipboard holds. It is the
		// user's previous copy, and rewriting that instead of their selection
		// is the worst thing this code could do.
		return captured, ErrClipboardUnchanged
	}

	text, err := r.Clipboard.Text()
	if err != nil {
		return captured, fmt.Errorf("reading the selection: %w", err)
	}
	captured.Text = text
	return captured, nil
}

// awaitChange polls the serial until it moves or the budget runs out.
func (r CopyRoundTrip) awaitChange(ctx context.Context, before uint64, settle, interval time.Duration) (bool, error) {
	sleep := r.Sleep
	if sleep == nil {
		sleep = sleepFor
	}

	for waited := time.Duration(0); ; waited += interval {
		now, err := r.Clipboard.Serial()
		if err != nil {
			return false, fmt.Errorf("reading the clipboard state: %w", err)
		}
		if now != before {
			return true, nil
		}
		if waited >= settle {
			return false, nil
		}
		if err := sleep(ctx, interval); err != nil {
			return false, err
		}
	}
}

func sleepFor(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
