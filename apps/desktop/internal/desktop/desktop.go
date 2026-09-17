// Package desktop is the shell's flow: hot key, capture, stream, confirm,
// replace.
//
// None of it is OS-specific. The OS-specific half is the Platform interface at
// the bottom of this file, implemented once per display server, and the split
// is deliberate rather than tidy-minded: the two guarantees this product makes
// about other people's documents are enforced here, in one place, instead of
// being a rule that three platform backends each have to remember.
//
// Those guarantees, from the brief:
//
//   - Never replace text silently. The user sees the rewrite and accepts it
//     before anything is written back.
//   - Always restore the original clipboard, including on every error path.
//
// The second is why Capture returns a Restore function rather than leaving
// each backend to undo its own mess. A backend that forgets is a backend that
// eats somebody's copied password; a single deferred call in Trigger is a thing
// a test can prove, and TestTheClipboardIsRestoredOnEveryPath does.
package desktop

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"

	"github.com/srikary12/starch/apps/desktop/internal/client"
)

// Rect is a position on screen, in the display server's own coordinates. Used
// only to place the overlay near the text it is rewriting.
type Rect struct{ X, Y, W, H int }

// Empty reports whether the rectangle carries no useful position, in which case
// the overlay picks its own.
func (r Rect) Empty() bool { return r.W == 0 && r.H == 0 }

// Capture is text taken from the focused application, and everything needed to
// put a replacement back where it came from.
type Capture struct {
	// Text is what the user had selected.
	Text string

	// Anchor is opaque platform state identifying where Text came from — an
	// accessibility element, a window handle — so that Replace can target the
	// same place even if focus has moved since. Backends that cannot offer one
	// leave it nil and rely on focus still being where it was.
	Anchor any

	// Bounds is where the text is on screen, for placing the overlay. Zero when
	// the platform could not say.
	Bounds Rect

	// ViaClipboard records that this came from the clipboard fallback rather
	// than an accessibility API. Kept because it changes what Replace can
	// promise: a clipboard round trip cannot know whether the application
	// honoured the copy, so an empty result is ambiguous in a way an
	// accessibility read is not.
	ViaClipboard bool

	// Restore puts the clipboard back as it was. It is called exactly once, on
	// every path out of Trigger, including panics in the platform layer.
	//
	// Backends that never touched the clipboard return nil, and a nil Restore
	// is not an error — it means there was nothing to undo.
	Restore func() error
}

// Decision is what the user did with a rewrite they were shown.
type Decision int

const (
	// Cancelled is Escape, and the default for anything ambiguous. Choosing it
	// as the zero value is deliberate: a Decision that was never set must never
	// read as consent to write into a document.
	Cancelled Decision = iota
	// Accepted is Enter.
	Accepted
)

func (d Decision) String() string {
	if d == Accepted {
		return "accepted"
	}
	return "cancelled"
}

// Overlay is the window showing the rewrite as it arrives.
type Overlay interface {
	// Delta appends a fragment as it streams in.
	Delta(string)

	// Done reports that the stream finished, with the complete text. Until this
	// is called the rewrite is partial, and an implementation must not let the
	// user accept one — see the comment on Trigger.
	Done(full string)

	// Fail shows an error in place of a rewrite. The overlay stays up so the
	// user can read it, and Await keeps working so Escape still dismisses it.
	Fail(error)

	// Await blocks until the user accepts or cancels, or ctx ends. Returning
	// Cancelled on a ctx that ended is correct and expected.
	Await(ctx context.Context) (Decision, error)

	// Close takes it off screen. Called exactly once, on every path.
	Close()
}

// Platform is the OS-specific half of the shell.
//
// Small on purpose. Everything that can be decided without asking the operating
// system is decided in Trigger, so that a new display server is a matter of
// four methods rather than a second copy of the product's behaviour.
type Platform interface {
	// Capture reads the selection in whatever application has focus.
	//
	// An empty Text is a normal outcome, not an error: the user pressed the hot
	// key with nothing selected.
	Capture(ctx context.Context) (Capture, error)

	// Replace writes replacement over the captured selection.
	//
	// It must land as a single undoable edit where the platform allows it:
	// Cmd-Z or Ctrl-Z in the host application has to put the original back in
	// one step, not character by character.
	Replace(ctx context.Context, c Capture, replacement string) error

	// Overlay opens the window for a capture.
	Overlay(ctx context.Context, c Capture) (Overlay, error)

	// Notify says something the user needs to know when there is no overlay to
	// say it in — nothing selected, or a capture that failed outright.
	Notify(title, body string)
}

// Streamer runs one rewrite, calling onDelta as fragments arrive, and returns
// the finished text.
//
// An interface rather than a *client.Client so that the flow can be tested
// without a daemon, and so that cancelling through ctx is the only way to stop
// a rewrite. That last part is a billing guarantee, not a style choice: the
// contract is explicit that a client which merely stops reading has cancelled
// nothing and is still being paid for.
type Streamer interface {
	Stream(ctx context.Context, req client.RewriteRequest, onDelta func(string)) (string, error)
}

// Shell is the flow. Construct one per process and call Trigger per hot key.
type Shell struct {
	Platform Platform
	Streamer Streamer

	// Preset is the rewrite style, from settings.
	Preset string

	// running guards against a second rewrite starting while one is in
	// progress. See Trigger.
	running atomic.Bool
}

// ErrNoPlatform is returned by Trigger on a Shell that was not fully built. It
// exists so the failure is a sentence rather than a nil dereference inside a
// display server binding.
var ErrNoPlatform = errors.New("the shell has no platform or streamer")

// Trigger runs one rewrite, start to finish. It is what the hot key calls.
//
// The order is load-bearing:
//
//  1. Capture, and defer the clipboard restore before anything can fail.
//  2. Nothing selected is a notification, not an error, and not an overlay.
//  3. Open the overlay *before* asking for a single token, so the user sees
//     that the key registered rather than wondering for 400ms.
//  4. Stream and wait concurrently, because Escape has to reach the provider
//     while the text is still arriving — that is the whole point of it.
//  5. Replace only on Accepted, and only with the completed text.
//
// Step 5 deliberately has no "accept what has arrived so far" path. A partial
// rewrite reads exactly like a finished one, and offering it would put half a
// sentence into somebody's document — the same reasoning that makes
// client.ErrIncompleteStream its own error rather than a short result.
func (s *Shell) Trigger(ctx context.Context) error {
	if s.Platform == nil || s.Streamer == nil {
		return ErrNoPlatform
	}

	// One rewrite at a time. The hot key is global and cheap to press, and a
	// second press while the first is still streaming would open a second
	// overlay over the first, start a second generation the user pays for, and
	// leave two things racing to paste into the same document. Ignoring the
	// press is the right answer: the user can see an overlay already open, and
	// Escape is how they stop it.
	if !s.running.CompareAndSwap(false, true) {
		return nil
	}
	defer s.running.Store(false)

	captured, err := s.Platform.Capture(ctx)
	// Deferred before the error check on purpose: a capture that failed
	// part-way may still have put something on the clipboard.
	defer restore(captured)
	if err != nil {
		return fmt.Errorf("reading the selection: %w", err)
	}

	if strings.TrimSpace(captured.Text) == "" {
		s.Platform.Notify(nothingSelectedTitle, nothingSelectedBody(captured))
		return nil
	}

	overlay, err := s.Platform.Overlay(ctx, captured)
	if err != nil {
		return fmt.Errorf("opening the overlay: %w", err)
	}
	defer overlay.Close()

	full, decision, err := s.streamAndAwait(ctx, overlay, captured.Text)
	if err != nil {
		return err
	}
	if decision != Accepted {
		return nil
	}

	if err := s.Platform.Replace(ctx, captured, full); err != nil {
		return fmt.Errorf("replacing the text: %w", err)
	}
	return nil
}

// streamAndAwait runs the rewrite and the user's decision at the same time.
//
// Concurrently because they genuinely race: the rewrite is still arriving while
// the user is deciding, and Escape has to stop it mid-flight. Cancelling the
// stream's context is what closes the connection to the daemon, which is what
// stops the provider generating.
func (s *Shell) streamAndAwait(ctx context.Context, overlay Overlay, text string) (string, Decision, error) {
	streaming, stop := context.WithCancel(ctx)
	defer stop()

	type outcome struct {
		full string
		err  error
	}
	finished := make(chan outcome, 1)
	go func() {
		full, err := s.Streamer.Stream(streaming, client.RewriteRequest{
			Text:   text,
			Preset: s.Preset,
		}, overlay.Delta)
		finished <- outcome{full, err}
	}()

	decided := make(chan Decision, 1)
	awaitErr := make(chan error, 1)
	go func() {
		decision, err := overlay.Await(ctx)
		awaitErr <- err
		decided <- decision
	}()

	var full string
	var done bool
	for {
		select {
		case result := <-finished:
			if result.err != nil {
				// Cancelling is how Escape works, so a context error here is
				// the user having already decided, not a failure.
				if errors.Is(result.err, context.Canceled) && streaming.Err() != nil {
					return "", Cancelled, nil
				}
				overlay.Fail(result.err)
				// Stay up so the message can be read. The user dismisses it,
				// and whatever they press means the same thing: no replacement.
				<-decided
				return "", Cancelled, <-awaitErr
			}
			full, done = result.full, true
			overlay.Done(full)

		case decision := <-decided:
			if err := <-awaitErr; err != nil {
				return "", Cancelled, fmt.Errorf("waiting on the overlay: %w", err)
			}
			if decision == Accepted && !done {
				// An overlay must not offer Enter before Done. One that does is
				// a bug in that backend, and honouring it would write a partial
				// rewrite into a document.
				return "", Cancelled, errAcceptedTooEarly
			}
			return full, decision, nil

		case <-ctx.Done():
			return "", Cancelled, ctx.Err()
		}
	}
}

var errAcceptedTooEarly = errors.New(
	"the overlay accepted a rewrite before it finished streaming, which would " +
		"have written a partial one into the document")

// restore puts the clipboard back, swallowing nothing but reporting nowhere.
//
// Deliberately not returning an error: it runs in a defer on every path,
// including paths that are already returning a more interesting failure, and a
// clipboard that could not be restored must not replace the reason the rewrite
// went wrong.
func restore(c Capture) {
	if c.Restore == nil {
		return
	}
	_ = c.Restore()
}

const nothingSelectedTitle = "Nothing selected"

func nothingSelectedBody(c Capture) string {
	if c.ViaClipboard {
		// Worth distinguishing. With the clipboard fallback an empty result can
		// also mean the application ignored the copy, and telling someone to
		// select text they have already selected is infuriating.
		return "Select some text first. If you did, this application may not " +
			"allow other programs to read the selection."
	}
	return "Select some text first, then press the shortcut again."
}
