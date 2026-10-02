package win32

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/srikary12/starch/apps/desktop/internal/desktop"
)

func awaited(t *testing.T, s *overlayState) desktop.Decision {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	d, err := s.Await(ctx)
	if err != nil {
		t.Fatalf("Await: %v", err)
	}
	if ctx.Err() != nil {
		t.Fatal("Await timed out rather than being decided by a key")
	}
	return d
}

// The contract desktop.Trigger refuses to violate, enforced here so the refusal
// is a backstop rather than the only guard. A partial rewrite reads exactly
// like a finished one.
func TestEnterDoesNothingUntilTheStreamFinishes(t *testing.T) {
	s := newOverlayState()
	s.Delta("Thanks for")

	if consumed := s.key(vkReturn); !consumed {
		t.Error("Enter was passed through to the application underneath, where it would insert a newline")
	}
	select {
	case d := <-s.decision:
		t.Fatalf("a decision (%v) was made while the rewrite was still streaming", d)
	default:
	}

	s.Done("Thanks for the update.")
	s.key(vkReturn)
	if got := awaited(t, s); got != desktop.Accepted {
		t.Errorf("decision = %v, want Accepted once the stream had finished", got)
	}
}

func TestEscapeCancelsAtAnyPoint(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(*overlayState)
	}{
		{"while streaming", func(s *overlayState) { s.Delta("Tha") }},
		{"after it finished", func(s *overlayState) { s.Done("Thanks.") }},
		{"after a failure", func(s *overlayState) { s.Fail(errors.New("nope")) }},
		{"before anything at all", func(*overlayState) {}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newOverlayState()
			tc.setup(s)

			if consumed := s.key(vkEscape); !consumed {
				t.Error("Escape was not consumed")
			}
			if got := awaited(t, s); got != desktop.Cancelled {
				t.Errorf("decision = %v, want Cancelled", got)
			}
		})
	}
}

// A failed rewrite has nothing to accept, so Enter dismisses rather than
// reporting a replacement that cannot happen.
func TestEnterAfterAFailureCancels(t *testing.T) {
	s := newOverlayState()
	s.Fail(errors.New("the provider refused the key"))

	s.key(vkReturn)
	if got := awaited(t, s); got != desktop.Cancelled {
		t.Errorf("decision = %v, want Cancelled", got)
	}
}

// The keypad's Enter is a different virtual-key code and means the same thing.
func TestTheKeypadEnterWorksToo(t *testing.T) {
	s := newOverlayState()
	s.Done("Thanks.")

	s.key(vkEnter)
	if got := awaited(t, s); got != desktop.Accepted {
		t.Errorf("decision = %v, want Accepted", got)
	}
}

// The hook sees every keystroke on the machine. Consuming anything but the two
// keys the overlay uses would make the shell eat the user's typing everywhere.
func TestEveryOtherKeyIsPassedThrough(t *testing.T) {
	s := newOverlayState()
	s.Done("Thanks.")

	for _, vk := range []uint32{
		'A', 'Z', '0', 0x20 /* space */, 0x09 /* tab */, 0x08, /* backspace */
		0x25 /* left */, 0x70 /* F1 */, 0x11 /* ctrl */, 0x2E, /* delete */
	} {
		if s.key(vk) {
			t.Errorf("the overlay swallowed %#02x, which belongs to the application underneath", vk)
		}
	}
}

// A second Enter must not queue a second replacement.
func TestOnlyTheFirstDecisionCounts(t *testing.T) {
	s := newOverlayState()
	s.Done("Thanks.")

	s.key(vkReturn)
	s.key(vkReturn)
	s.key(vkEscape)

	if got := awaited(t, s); got != desktop.Accepted {
		t.Fatalf("decision = %v, want the first one", got)
	}
	select {
	case d := <-s.decision:
		t.Errorf("a second decision (%v) was queued", d)
	default:
	}
}

// What the window draws, and in particular that it never offers a key that
// would do nothing.
func TestTheHintTellsTheTruthAboutWhatTheKeysDo(t *testing.T) {
	s := newOverlayState()

	s.Delta("Thanks for")
	streaming := s.view()
	if strings.Contains(streaming.Hint, "Enter") {
		t.Errorf("hint = %q while streaming, but Enter does nothing yet", streaming.Hint)
	}
	if streaming.Text != "Thanks for" {
		t.Errorf("text = %q, want what has streamed so far", streaming.Text)
	}

	s.Done("Thanks for the update.")
	finished := s.view()
	if !strings.Contains(finished.Hint, "Enter") {
		t.Errorf("hint = %q once finished, want it to offer Enter", finished.Hint)
	}
	if finished.Text != "Thanks for the update." {
		t.Errorf("text = %q, want the completed rewrite", finished.Text)
	}
	if finished.Failed {
		t.Error("a finished rewrite is marked as failed")
	}

	s.Fail(errors.New("the endpoint refused the key"))
	failed := s.view()
	if !failed.Failed {
		t.Error("a failure is not marked as one")
	}
	if !strings.Contains(failed.Text, "refused the key") {
		t.Errorf("text = %q, want the reason", failed.Text)
	}
	if strings.Contains(failed.Hint, "Enter") {
		t.Errorf("hint = %q after a failure, but there is nothing to accept", failed.Hint)
	}
}

// The window repaints on a signal rather than a timer, and a burst of deltas
// must not block the stream when nothing is repainting yet.
func TestDeltasNeverBlock(t *testing.T) {
	s := newOverlayState()
	done := make(chan struct{})

	go func() {
		for range 1000 {
			s.Delta("x")
		}
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Delta blocked; the rewrite stream would stall behind the overlay")
	}
	if got := len(s.view().Text); got != 1000 {
		t.Errorf("text is %d characters, want 1000", got)
	}
}
