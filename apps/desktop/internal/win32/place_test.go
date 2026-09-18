package win32

import (
	"testing"

	"github.com/srikary12/starch/apps/desktop/internal/desktop"
)

// A 1080p screen, as a stand-in for "a screen".
const (
	screenW = 1920
	screenH = 1080
)

// The property that matters, checked over the whole screen rather than at a few
// chosen points: wherever the selection is, the overlay is somewhere the user
// can read it. A rewrite shown half off the edge is a rewrite being approved
// unseen.
func TestTheOverlayIsAlwaysFullyOnScreen(t *testing.T) {
	for _, selection := range []desktop.Rect{
		{X: 0, Y: 0, W: 100, H: 20},                        // top left corner
		{X: screenW - 100, Y: 0, W: 100, H: 20},            // top right
		{X: 0, Y: screenH - 40, W: 100, H: 20},             // bottom left
		{X: screenW - 100, Y: screenH - 40, W: 100, H: 20}, // bottom right
		{X: screenW - 10, Y: screenH / 2, W: 5, H: 20},     // hard against the right edge
		{X: 960, Y: 540, W: 200, H: 20},                    // the easy case, in the middle
		{X: 400, Y: screenH - 200, W: 200, H: 20},          // just too low for room beneath
		{}, // no position reported at all
	} {
		x, y, w, h := placeOverlay(selection, screenW, screenH)

		if x < 0 || y < 0 {
			t.Errorf("selection %+v placed the overlay at (%d,%d), off the top or left", selection, x, y)
		}
		if x+w > screenW {
			t.Errorf("selection %+v placed the overlay %d past the right edge", selection, x+w-screenW)
		}
		if y+h > screenH {
			t.Errorf("selection %+v placed the overlay %d past the bottom edge", selection, y+h-screenH)
		}
		if w <= 0 || h <= 0 {
			t.Errorf("selection %+v gave the overlay no size: %dx%d", selection, w, h)
		}
	}
}

// Below the selection is where it belongs when there is room, because the
// selection is what the rewrite is being compared against.
func TestTheOverlaySitsBelowTheSelectionWhenItFits(t *testing.T) {
	selection := desktop.Rect{X: 300, Y: 200, W: 400, H: 24}
	x, y, _, _ := placeOverlay(selection, screenW, screenH)

	if y <= int32(selection.Y+selection.H) {
		t.Errorf("y = %d, want it below the selection ending at %d", y, selection.Y+selection.H)
	}
	if x != int32(selection.X) {
		t.Errorf("x = %d, want it aligned with the selection at %d", x, selection.X)
	}
}

// When there is no room below, above — not clamped to the bottom edge, which
// would cover the very text being rewritten.
func TestTheOverlayGoesAboveWhenThereIsNoRoomBelow(t *testing.T) {
	selection := desktop.Rect{X: 300, Y: screenH - 60, W: 400, H: 24}
	_, y, _, h := placeOverlay(selection, screenW, screenH)

	if y+h > int32(selection.Y) {
		t.Errorf("the overlay spans to %d, overlapping a selection starting at %d", y+h, selection.Y)
	}
}

// The clipboard capture path reports no position, and that is normal rather
// than an error.
func TestNoKnownPositionIsCentred(t *testing.T) {
	x, y, w, _ := placeOverlay(desktop.Rect{}, screenW, screenH)

	if got, want := x+w/2, int32(screenW/2); got < want-2 || got > want+2 {
		t.Errorf("centred at %d, want about %d", got, want)
	}
	// A third of the way down rather than halfway: text being rewritten is
	// usually in the upper half, and covering it is what this must not do.
	if y >= screenH/2 {
		t.Errorf("y = %d, want it in the upper half", y)
	}
}

// Someone's netbook, or a scaled display reporting a small logical screen. The
// overlay has to shrink rather than hang off both edges at once.
func TestATinyScreenShrinksTheOverlay(t *testing.T) {
	const tinyW, tinyH = 320, 240

	x, y, w, h := placeOverlay(desktop.Rect{X: 100, Y: 100, W: 50, H: 20}, tinyW, tinyH)

	if x < 0 || y < 0 || x+w > tinyW || y+h > tinyH {
		t.Errorf("overlay at (%d,%d) %dx%d does not fit a %dx%d screen", x, y, w, h, tinyW, tinyH)
	}
	if w > tinyW || h > tinyH {
		t.Errorf("overlay %dx%d is larger than the whole screen", w, h)
	}
}
