package win32

import "github.com/srikary12/starch/apps/desktop/internal/desktop"

// Where the overlay goes, as arithmetic rather than as a syscall.
//
// Untagged so it is tested here. The failure it guards against is not subtle
// once it happens and is invisible until it does: a selection near the right or
// bottom edge of the screen puts the overlay half off it, and the user is asked
// to approve a rewrite they cannot read.

const (
	// Layout, in pixels at 96 DPI.
	overlayWidth  = 460
	overlayHeight = 160
	// Gap between the selection and the overlay, and the minimum from any
	// screen edge.
	overlayGap = 8
)

// placeOverlay positions the overlay near the captured text, kept on screen.
//
// Below the selection when its position is known, above it when there is no
// room below, and centred when nothing reported a position at all — which is
// the clipboard capture path, where there is no position to report.
func placeOverlay(near desktop.Rect, screenWidth, screenHeight int32) (x, y, width, height int32) {
	width, height = overlayWidth, overlayHeight

	// Never wider than the screen it has to fit on.
	if width > screenWidth-2*overlayGap {
		width = screenWidth - 2*overlayGap
	}
	if height > screenHeight-2*overlayGap {
		height = screenHeight - 2*overlayGap
	}

	if near.Empty() {
		// A third of the way down rather than centred: text being rewritten is
		// usually in the upper half, and covering it is the one thing the
		// placement must not do.
		return centre(screenWidth, width), screenHeight / 3, width, height
	}

	x = int32(near.X)
	y = int32(near.Y+near.H) + overlayGap

	if x+width > screenWidth-overlayGap {
		x = screenWidth - width - overlayGap
	}
	if x < overlayGap {
		x = overlayGap
	}

	if y+height > screenHeight-overlayGap {
		// No room below, so go above the selection instead of hanging off the
		// bottom.
		y = int32(near.Y) - height - overlayGap
	}
	if y < overlayGap {
		y = overlayGap
	}
	return x, y, width, height
}

func centre(available, size int32) int32 {
	x := (available - size) / 2
	if x < overlayGap {
		return overlayGap
	}
	return x
}
