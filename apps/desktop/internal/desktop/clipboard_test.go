package desktop

import (
	"context"
	"errors"
	"testing"
	"time"
)

type fakeClipboard struct {
	serial     uint64
	text       string
	serialErr  error
	textErr    error
	saveErr    error
	restores   int
	serialRead int

	// onSerialRead lets a test move the clipboard part-way through the wait,
	// the way a slow application does.
	onSerialRead func(c *fakeClipboard, nth int)
}

func (c *fakeClipboard) Serial() (uint64, error) {
	c.serialRead++
	if c.onSerialRead != nil {
		c.onSerialRead(c, c.serialRead)
	}
	return c.serial, c.serialErr
}

func (c *fakeClipboard) Text() (string, error) { return c.text, c.textErr }

func (c *fakeClipboard) Save() (func() error, error) {
	if c.saveErr != nil {
		return nil, c.saveErr
	}
	return func() error { c.restores++; return nil }, nil
}

type fakeKeyboard struct {
	copies   int
	pastes   int
	copyErr  error
	onCopy   func()
	pasteErr error
}

func (k *fakeKeyboard) Copy(context.Context) error {
	k.copies++
	if k.onCopy != nil {
		k.onCopy()
	}
	return k.copyErr
}

func (k *fakeKeyboard) Paste(context.Context) error { k.pastes++; return k.pasteErr }

// noSleep runs the poll loop without spending real time.
func noSleep(context.Context, time.Duration) error { return nil }

func roundTrip(c Clipboard, k Keyboard) CopyRoundTrip {
	return CopyRoundTrip{
		Clipboard: c, Keyboard: k,
		Settle: 100 * time.Millisecond, Interval: 10 * time.Millisecond,
		Sleep: noSleep,
	}
}

func TestACopiedSelectionIsRead(t *testing.T) {
	clip := &fakeClipboard{serial: 7, text: "old"}
	keys := &fakeKeyboard{onCopy: func() {
		clip.serial, clip.text = 8, "the selection"
	}}

	got, err := roundTrip(clip, keys).Capture(context.Background())
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}
	if got.Text != "the selection" {
		t.Errorf("text = %q", got.Text)
	}
	if !got.ViaClipboard {
		t.Error("ViaClipboard = false, but this is the clipboard path")
	}
	if keys.copies != 1 {
		t.Errorf("sent the copy chord %d times, want 1", keys.copies)
	}
}

// The reason this file exists.
//
// An application that ignores the synthetic copy leaves the clipboard holding
// whatever the user last copied. Returning that would hand the flow a
// plausible-looking selection derived from nothing on screen — the user sees a
// rewrite of their last copied text and presses Enter, and it replaces a
// selection it was never taken from.
func TestAnIgnoredCopyIsNeverMistakenForASelection(t *testing.T) {
	clip := &fakeClipboard{serial: 7, text: "a password the user copied earlier"}
	keys := &fakeKeyboard{} // the application does nothing

	got, err := roundTrip(clip, keys).Capture(context.Background())

	if !errors.Is(err, ErrClipboardUnchanged) {
		t.Fatalf("err = %v, want ErrClipboardUnchanged", err)
	}
	if got.Text != "" {
		t.Errorf("text = %q, want empty — that is the user's previous clipboard, not their selection", got.Text)
	}
}

// Whatever happens, the clipboard goes back. Enumerated rather than assumed,
// because the error paths are exactly when it is holding something the user
// did not put there.
func TestTheClipboardIsAlwaysRestored(t *testing.T) {
	boom := errors.New("boom")

	tests := []struct {
		name string
		clip *fakeClipboard
		keys *fakeKeyboard
	}{
		{"the copy worked", &fakeClipboard{serial: 1}, &fakeKeyboard{}},
		{"the copy chord failed", &fakeClipboard{serial: 1}, &fakeKeyboard{copyErr: boom}},
		{"the application ignored it", &fakeClipboard{serial: 1}, &fakeKeyboard{}},
		{"reading the text failed", &fakeClipboard{serial: 1, textErr: boom}, &fakeKeyboard{}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			clip, keys := tc.clip, tc.keys
			if tc.name == "the copy worked" || tc.name == "reading the text failed" {
				keys.onCopy = func() { clip.serial++ }
			}

			got, _ := roundTrip(clip, keys).Capture(context.Background())
			if got.Restore == nil {
				t.Fatal("no Restore was returned, so the flow has nothing to defer")
			}
			if err := got.Restore(); err != nil {
				t.Fatalf("Restore: %v", err)
			}
			if clip.restores != 1 {
				t.Errorf("restored %d times, want 1", clip.restores)
			}
		})
	}
}

// A clipboard that could not even be saved must not be read from either: the
// round trip would overwrite contents it cannot put back.
func TestACapturedClipboardThatCannotBeSavedIsNotTouched(t *testing.T) {
	clip := &fakeClipboard{serial: 1, saveErr: errors.New("access denied")}
	keys := &fakeKeyboard{}

	_, err := roundTrip(clip, keys).Capture(context.Background())
	if err == nil {
		t.Fatal("Capture succeeded despite being unable to save the clipboard")
	}
	if keys.copies != 0 {
		t.Error("the copy chord was sent anyway, overwriting a clipboard we cannot restore")
	}
}

// Applications answer asynchronously and some are slow. The wait has to
// tolerate that without giving up on the first look.
func TestASlowApplicationIsStillHeard(t *testing.T) {
	clip := &fakeClipboard{serial: 3, text: "old"}
	clip.onSerialRead = func(c *fakeClipboard, nth int) {
		// Answers on the fifth poll, well inside the budget.
		if nth == 5 {
			c.serial, c.text = 4, "eventually"
		}
	}

	got, err := roundTrip(clip, &fakeKeyboard{}).Capture(context.Background())
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}
	if got.Text != "eventually" {
		t.Errorf("text = %q, want the late answer", got.Text)
	}
}

// The wait is inside the user's latency budget for every single rewrite, so it
// has to actually end.
func TestTheWaitIsBounded(t *testing.T) {
	clip := &fakeClipboard{serial: 1}
	var slept time.Duration

	trip := CopyRoundTrip{
		Clipboard: clip, Keyboard: &fakeKeyboard{},
		Settle: 100 * time.Millisecond, Interval: 10 * time.Millisecond,
		Sleep: func(_ context.Context, d time.Duration) error { slept += d; return nil },
	}

	if _, err := trip.Capture(context.Background()); !errors.Is(err, ErrClipboardUnchanged) {
		t.Fatalf("err = %v, want it to give up with ErrClipboardUnchanged", err)
	}
	if slept > 150*time.Millisecond {
		t.Errorf("waited %v for a 100ms budget", slept)
	}
}

// Escape during the wait has to be honoured; the whole point of the hot key
// flow is that nothing blocks uninterruptibly.
func TestCancellingDuringTheWait(t *testing.T) {
	clip := &fakeClipboard{serial: 1}
	ctx, cancel := context.WithCancel(context.Background())

	trip := CopyRoundTrip{
		Clipboard: clip, Keyboard: &fakeKeyboard{},
		Settle: time.Hour, Interval: time.Millisecond,
		Sleep: func(ctx context.Context, _ time.Duration) error {
			cancel()
			return ctx.Err()
		},
	}

	_, err := trip.Capture(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

// Zero timings must not mean "poll forever with no pause".
func TestUnsetTimingsFallBackToTheDefaults(t *testing.T) {
	clip := &fakeClipboard{serial: 1}
	polls := 0
	trip := CopyRoundTrip{
		Clipboard: clip, Keyboard: &fakeKeyboard{},
		Sleep: func(context.Context, time.Duration) error { polls++; return nil },
	}

	if _, err := trip.Capture(context.Background()); !errors.Is(err, ErrClipboardUnchanged) {
		t.Fatalf("err = %v", err)
	}
	want := int(DefaultSettle / DefaultInterval)
	if polls < want-2 || polls > want+2 {
		t.Errorf("polled %d times, want about %d (the default budget)", polls, want)
	}
}
