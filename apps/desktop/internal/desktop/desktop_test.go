package desktop

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/srikary12/starch/apps/desktop/internal/client"
)

// The point of this package is that the promises the product makes about other
// people's documents are testable without a display server. These are those
// promises. None of this needs X11, Wayland, Win32 or a Mac.

type fakeOverlay struct {
	mu sync.Mutex

	deltas  []string
	full    string
	failed  error
	closed  int
	doneAt  time.Time
	done    chan struct{}
	decide  chan Decision
	awaitAt chan struct{}

	// waitForDone makes Await hold until the stream has finished, which is what
	// a real overlay does by not offering Enter before then.
	waitForDone bool
	awaitErr    error
}

func newOverlay() *fakeOverlay {
	return &fakeOverlay{
		done:    make(chan struct{}),
		decide:  make(chan Decision, 1),
		awaitAt: make(chan struct{}, 1),
	}
}

func (o *fakeOverlay) Delta(s string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.deltas = append(o.deltas, s)
}

func (o *fakeOverlay) Done(full string) {
	o.mu.Lock()
	o.full, o.doneAt = full, time.Now()
	o.mu.Unlock()
	close(o.done)
}

func (o *fakeOverlay) Fail(err error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.failed = err
}

func (o *fakeOverlay) Await(ctx context.Context) (Decision, error) {
	o.awaitAt <- struct{}{}
	if o.waitForDone {
		select {
		case <-o.done:
		case <-ctx.Done():
			return Cancelled, nil
		}
	}
	select {
	case d := <-o.decide:
		return d, o.awaitErr
	case <-ctx.Done():
		return Cancelled, nil
	}
}

func (o *fakeOverlay) Close() {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.closed++
}

func (o *fakeOverlay) closeCount() int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.closed
}

type fakePlatform struct {
	mu sync.Mutex

	capture    Capture
	captureErr error
	overlay    *fakeOverlay
	overlayErr error
	replaceErr error

	restores  int
	replaced  []string
	notices   []string
	overlayed int
}

func (p *fakePlatform) Capture(context.Context) (Capture, error) {
	c := p.capture
	c.Restore = func() error {
		p.mu.Lock()
		defer p.mu.Unlock()
		p.restores++
		return nil
	}
	return c, p.captureErr
}

func (p *fakePlatform) Replace(_ context.Context, _ Capture, replacement string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.replaced = append(p.replaced, replacement)
	return p.replaceErr
}

func (p *fakePlatform) Overlay(context.Context, Capture) (Overlay, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.overlayed++
	if p.overlayErr != nil {
		return nil, p.overlayErr
	}
	return p.overlay, nil
}

// setOverlay swaps in the next overlay, under the lock, because a test that
// triggers twice does so from a different goroutine than the flow reads from.
func (p *fakePlatform) setOverlay(o *fakeOverlay) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.overlay = o
}

func (p *fakePlatform) Notify(title, _ string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.notices = append(p.notices, title)
}

func (p *fakePlatform) counts() (restores int, replaced []string, notices []string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.restores, append([]string(nil), p.replaced...), append([]string(nil), p.notices...)
}

type streamFunc func(context.Context, client.RewriteRequest, func(string)) (string, error)

func (f streamFunc) Stream(ctx context.Context, req client.RewriteRequest, onDelta func(string)) (string, error) {
	return f(ctx, req, onDelta)
}

// streams emits fragments and returns them joined, the way a real rewrite does.
func streams(pieces ...string) streamFunc {
	return func(_ context.Context, _ client.RewriteRequest, onDelta func(string)) (string, error) {
		for _, piece := range pieces {
			onDelta(piece)
		}
		return strings.Join(pieces, ""), nil
	}
}

func shellWith(p *fakePlatform, s Streamer) *Shell {
	return &Shell{Platform: p, Streamer: s, Preset: "professional"}
}

// The guarantee: "Always restore the original clipboard, including on every
// error path." Every path means every path, so this enumerates them rather
// than testing the happy one and trusting the rest.
func TestTheClipboardIsRestoredOnEveryPath(t *testing.T) {
	boom := errors.New("boom")

	tests := []struct {
		name  string
		build func() (*fakePlatform, Streamer, func(*fakeOverlay))
	}{
		{"the rewrite is accepted", func() (*fakePlatform, Streamer, func(*fakeOverlay)) {
			p := &fakePlatform{capture: Capture{Text: "thx"}, overlay: newOverlay()}
			p.overlay.waitForDone = true
			return p, streams("Thanks."), func(o *fakeOverlay) { o.decide <- Accepted }
		}},
		{"the rewrite is cancelled", func() (*fakePlatform, Streamer, func(*fakeOverlay)) {
			p := &fakePlatform{capture: Capture{Text: "thx"}, overlay: newOverlay()}
			return p, streams("Thanks."), func(o *fakeOverlay) { o.decide <- Cancelled }
		}},
		{"nothing was selected", func() (*fakePlatform, Streamer, func(*fakeOverlay)) {
			p := &fakePlatform{capture: Capture{Text: "   "}, overlay: newOverlay()}
			return p, streams("unused"), nil
		}},
		{"capture itself failed", func() (*fakePlatform, Streamer, func(*fakeOverlay)) {
			p := &fakePlatform{capture: Capture{Text: "thx"}, captureErr: boom, overlay: newOverlay()}
			return p, streams("Thanks."), nil
		}},
		{"the overlay would not open", func() (*fakePlatform, Streamer, func(*fakeOverlay)) {
			p := &fakePlatform{capture: Capture{Text: "thx"}, overlayErr: boom, overlay: newOverlay()}
			return p, streams("Thanks."), nil
		}},
		{"the stream failed", func() (*fakePlatform, Streamer, func(*fakeOverlay)) {
			p := &fakePlatform{capture: Capture{Text: "thx"}, overlay: newOverlay()}
			failing := streamFunc(func(context.Context, client.RewriteRequest, func(string)) (string, error) {
				return "", boom
			})
			return p, failing, func(o *fakeOverlay) { o.decide <- Cancelled }
		}},
		{"replacing failed", func() (*fakePlatform, Streamer, func(*fakeOverlay)) {
			p := &fakePlatform{capture: Capture{Text: "thx"}, replaceErr: boom, overlay: newOverlay()}
			p.overlay.waitForDone = true
			return p, streams("Thanks."), func(o *fakeOverlay) { o.decide <- Accepted }
		}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p, streamer, drive := tc.build()
			if drive != nil {
				drive(p.overlay)
			}

			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = shellWith(p, streamer).Trigger(ctx)

			restores, _, _ := p.counts()
			if restores != 1 {
				t.Errorf("clipboard restored %d times, want exactly 1", restores)
			}
		})
	}
}

// The other guarantee: "Never replace text silently."
func TestNothingIsReplacedWithoutAcceptance(t *testing.T) {
	p := &fakePlatform{capture: Capture{Text: "thx"}, overlay: newOverlay()}
	p.overlay.decide <- Cancelled

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := shellWith(p, streams("Thanks.")).Trigger(ctx); err != nil {
		t.Fatalf("Trigger: %v", err)
	}

	if _, replaced, _ := p.counts(); len(replaced) != 0 {
		t.Errorf("replaced %q after a cancellation", replaced)
	}
}

func TestAnAcceptedRewriteIsWrittenBackWhole(t *testing.T) {
	p := &fakePlatform{capture: Capture{Text: "thx for the update"}, overlay: newOverlay()}
	p.overlay.waitForDone = true
	p.overlay.decide <- Accepted

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := shellWith(p, streams("Thanks ", "for the ", "update.")).Trigger(ctx); err != nil {
		t.Fatalf("Trigger: %v", err)
	}

	_, replaced, _ := p.counts()
	if len(replaced) != 1 || replaced[0] != "Thanks for the update." {
		t.Fatalf("replaced %q, want the whole rewrite once", replaced)
	}
	if p.overlay.closeCount() != 1 {
		t.Errorf("overlay closed %d times, want 1", p.overlay.closeCount())
	}
}

// A partial rewrite reads exactly like a finished one. An overlay that offers
// Enter before the stream ends is a bug in that backend, and the flow must
// refuse it rather than write half a sentence into a document.
func TestAPartialRewriteIsNeverWrittenBack(t *testing.T) {
	p := &fakePlatform{capture: Capture{Text: "thx"}, overlay: newOverlay()}
	p.overlay.decide <- Accepted // before Done, which no real overlay should do

	blocked := make(chan struct{})
	slow := streamFunc(func(ctx context.Context, _ client.RewriteRequest, onDelta func(string)) (string, error) {
		onDelta("Thanks")
		<-blocked
		return "Thanks.", nil
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := shellWith(p, slow).Trigger(ctx)
	close(blocked)

	if !errors.Is(err, errAcceptedTooEarly) {
		t.Fatalf("err = %v, want it to refuse an early acceptance", err)
	}
	if _, replaced, _ := p.counts(); len(replaced) != 0 {
		t.Errorf("replaced %q from a stream that had not finished", replaced)
	}
}

// Escape has to reach the provider, not just the screen. Cancelling the
// context is what closes the connection to the daemon, and a client that
// merely stops reading has cancelled nothing and is still being billed.
func TestEscapeCancelsTheStreamItself(t *testing.T) {
	p := &fakePlatform{capture: Capture{Text: "thx"}, overlay: newOverlay()}

	cancelled := make(chan struct{})
	watching := streamFunc(func(ctx context.Context, _ client.RewriteRequest, onDelta func(string)) (string, error) {
		onDelta("Tha")
		<-ctx.Done()
		close(cancelled)
		return "", ctx.Err()
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- shellWith(p, watching).Trigger(ctx) }()

	<-p.overlay.awaitAt // the overlay is up and waiting
	p.overlay.decide <- Cancelled

	select {
	case <-cancelled:
	case <-time.After(5 * time.Second):
		t.Fatal("the stream was never cancelled; the provider would still be generating")
	}

	if err := <-done; err != nil {
		t.Fatalf("Trigger: %v", err)
	}
	if _, replaced, _ := p.counts(); len(replaced) != 0 {
		t.Errorf("replaced %q after Escape", replaced)
	}
}

// Pressing the hot key with nothing selected is an ordinary thing to do, so it
// gets a notification rather than an error, and no overlay at all.
func TestNothingSelectedIsNotAnError(t *testing.T) {
	p := &fakePlatform{capture: Capture{Text: "  \n "}, overlay: newOverlay()}

	if err := shellWith(p, streams("unused")).Trigger(context.Background()); err != nil {
		t.Fatalf("Trigger: %v", err)
	}

	_, _, notices := p.counts()
	if len(notices) != 1 {
		t.Fatalf("notices = %v, want exactly one", notices)
	}
	p.mu.Lock()
	overlayed := p.overlayed
	p.mu.Unlock()
	if overlayed != 0 {
		t.Errorf("opened %d overlays for an empty selection, want none", overlayed)
	}
}

// The clipboard fallback cannot tell "nothing was selected" from "the
// application refused to copy", and saying the wrong one wastes the user's
// time. So the message differs.
func TestTheEmptyMessageDependsOnHowCaptureWorked(t *testing.T) {
	direct := nothingSelectedBody(Capture{})
	fallback := nothingSelectedBody(Capture{ViaClipboard: true})

	if direct == fallback {
		t.Fatal("the same message for both, but a clipboard capture cannot rule out a refusal")
	}
	if !strings.Contains(fallback, "may not") {
		t.Errorf("fallback message = %q, want it to admit the ambiguity", fallback)
	}
}

// A Decision that was never set must not read as consent.
func TestTheZeroDecisionIsCancelled(t *testing.T) {
	var d Decision
	if d != Cancelled {
		t.Fatalf("the zero Decision is %v, which would let an unset value write to a document", d)
	}
}

func TestAShellWithoutAPlatformSaysSo(t *testing.T) {
	if err := (&Shell{}).Trigger(context.Background()); !errors.Is(err, ErrNoPlatform) {
		t.Fatalf("err = %v, want ErrNoPlatform", err)
	}
}

// The hot key is global and cheap to press. A second press while the first
// rewrite is still streaming would open a second overlay over the first, start
// a second generation the user pays for, and leave two of them racing to paste
// into the same document.
//
// Found by writing the manual test for it rather than by the code: nothing
// stopped it.
func TestASecondPressWhileRewritingIsIgnored(t *testing.T) {
	p := &fakePlatform{capture: Capture{Text: "thx"}, overlay: newOverlay()}
	p.overlay.waitForDone = true

	streaming := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	slow := streamFunc(func(context.Context, client.RewriteRequest, func(string)) (string, error) {
		// Once, because the last press in this test runs the same streamer
		// again after the first rewrite has finished.
		once.Do(func() { close(streaming) })
		<-release
		return "Thanks.", nil
	})

	shell := shellWith(p, slow)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	first := make(chan error, 1)
	go func() { first <- shell.Trigger(ctx) }()
	<-streaming

	// The second press, while the first is mid-stream.
	if err := shell.Trigger(ctx); err != nil {
		t.Fatalf("the second press returned an error rather than being ignored: %v", err)
	}

	p.mu.Lock()
	overlays := p.overlayed
	p.mu.Unlock()
	if overlays != 1 {
		t.Errorf("opened %d overlays, want 1 — the second press started another rewrite", overlays)
	}

	close(release)
	p.overlay.decide <- Accepted
	if err := <-first; err != nil {
		t.Fatalf("the first rewrite: %v", err)
	}

	// And once it is done, the shortcut works again.
	next := newOverlay()
	next.waitForDone = true
	next.decide <- Cancelled
	p.setOverlay(next)
	if err := shell.Trigger(ctx); err != nil {
		t.Fatalf("a press after the first finished: %v", err)
	}
	p.mu.Lock()
	overlays = p.overlayed
	p.mu.Unlock()
	if overlays != 2 {
		t.Errorf("opened %d overlays in total, want 2 — the shortcut stopped working", overlays)
	}
}
