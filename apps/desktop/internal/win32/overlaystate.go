package win32

import (
	"context"
	"strings"
	"sync"

	"github.com/srikary12/starch/apps/desktop/internal/desktop"
)

// The overlay's state, separated from the window that draws it.
//
// Untagged so it is tested here, and worth separating for a reason beyond
// that: this is where the flow's contract is kept. desktop.Trigger refuses a
// rewrite accepted before its stream finished — a partial rewrite reads exactly
// like a complete one, and writing one into a document is the worst thing this
// product can do — and the refusal is a backstop, not the mechanism. The
// mechanism is here: Enter does nothing until Done has been called.
//
// The keys arrive from a low-level keyboard hook rather than from window
// messages, because the overlay never takes focus. That is not a detail: the
// host application has to stay active, or the selection stops being a selection
// and the paste lands somewhere else entirely.

// Virtual-key codes the overlay answers to.
const (
	vkReturn = 0x0D
	vkEscape = 0x1B
	vkEnter  = 0x6E // numeric keypad Enter is a different code and means the same
)

// overlayState is what the overlay knows and what the keys do to it.
type overlayState struct {
	mu       sync.Mutex
	streamed strings.Builder
	full     string
	done     bool
	failed   error
	decided  bool

	decision chan desktop.Decision
	// changed is signalled whenever the visible text changes, so the window can
	// repaint without polling.
	changed chan struct{}
}

func newOverlayState() *overlayState {
	return &overlayState{
		decision: make(chan desktop.Decision, 1),
		changed:  make(chan struct{}, 1),
	}
}

// Delta appends a streamed fragment.
func (s *overlayState) Delta(fragment string) {
	s.mu.Lock()
	s.streamed.WriteString(fragment)
	s.mu.Unlock()
	s.notify()
}

// Done records that the stream finished. Only after this does Enter mean
// anything.
func (s *overlayState) Done(full string) {
	s.mu.Lock()
	s.full, s.done = full, true
	s.mu.Unlock()
	s.notify()
}

// Fail shows an error instead of a rewrite.
//
// The overlay stays up so the message can be read, and every key then means the
// same thing, because there is nothing to accept.
func (s *overlayState) Fail(err error) {
	s.mu.Lock()
	s.failed = err
	s.mu.Unlock()
	s.notify()
}

// Await blocks until a key decides, or ctx ends.
func (s *overlayState) Await(ctx context.Context) (desktop.Decision, error) {
	select {
	case d := <-s.decision:
		return d, nil
	case <-ctx.Done():
		return desktop.Cancelled, nil
	}
}

// key handles a virtual-key code, reporting whether the overlay consumed it.
//
// A consumed key is swallowed by the hook and never reaches the application
// underneath. That has to be exact: swallowing Enter while the overlay is up is
// the point, and swallowing anything else would make the shell eat the user's
// typing in every application on the machine.
func (s *overlayState) key(vk uint32) (consumed bool) {
	switch vk {
	case vkEscape:
		s.decide(desktop.Cancelled)
		return true

	case vkReturn, vkEnter:
		s.mu.Lock()
		ready, failed := s.done, s.failed != nil
		s.mu.Unlock()

		switch {
		case failed:
			// Nothing to accept. Dismiss, and treat it as the cancellation it
			// is rather than reporting a replacement that cannot happen.
			s.decide(desktop.Cancelled)
			return true
		case ready:
			s.decide(desktop.Accepted)
			return true
		default:
			// Still streaming. Swallowed rather than passed through: the user
			// is looking at this overlay, and letting Enter insert a newline
			// into the document behind it would be worse than doing nothing.
			return true
		}
	}
	return false
}

// decide records the first decision and ignores the rest.
//
// Once only, because a second Enter after an accepted rewrite must not queue a
// second replacement.
func (s *overlayState) decide(d desktop.Decision) {
	s.mu.Lock()
	if s.decided {
		s.mu.Unlock()
		return
	}
	s.decided = true
	s.mu.Unlock()

	s.decision <- d
}

// view is what the window should draw.
type view struct {
	Text string
	// Hint is the line under the text telling the user what the keys do. It
	// changes with the state, because offering Enter while it does nothing
	// would be a lie.
	Hint string
	// Failed marks an error, which the window draws differently.
	Failed bool
}

func (s *overlayState) view() view {
	s.mu.Lock()
	defer s.mu.Unlock()

	switch {
	case s.failed != nil:
		return view{Text: s.failed.Error(), Hint: "Esc to dismiss", Failed: true}
	case s.done:
		return view{Text: s.full, Hint: "Enter to replace · Esc to cancel"}
	default:
		return view{Text: s.streamed.String(), Hint: "Esc to cancel"}
	}
}

func (s *overlayState) notify() {
	select {
	case s.changed <- struct{}{}:
	default:
	}
}
