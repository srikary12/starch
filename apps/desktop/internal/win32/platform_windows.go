package win32

import (
	"context"
	"errors"
	"time"

	"github.com/srikary12/starch/apps/desktop/internal/desktop"
)

// Platform is the Windows half of the shell, as desktop.Platform.
//
// Thin on purpose. Everything decidable without asking the operating system is
// decided in internal/desktop, where it is tested; this is the part that cannot
// be, so the less of it there is, the better.
type Platform struct {
	ui *UIThread
}

// NewPlatform wraps a UI thread. The thread must be running — see UIThread.Run
// — before any of these are called.
func NewPlatform(ui *UIThread) *Platform { return &Platform{ui: ui} }

// Capture reads the selection by copying it.
//
// UI Automation would be better where an application supports it: it reads the
// selection without touching the clipboard at all. It is not here yet, so this
// is the clipboard round trip alone, which is the path that works everywhere
// and the one every other platform ends up on for anything rendering web
// content.
func (p *Platform) Capture(ctx context.Context) (desktop.Capture, error) {
	captured, err := desktop.CopyRoundTrip{
		Clipboard: Clipboard{},
		Keyboard:  Keyboard{},
	}.Capture(ctx)

	// An application that ignored the copy is not a failure to report as one.
	// It means we have no selection, which the flow already knows how to say —
	// and says better, because Capture.ViaClipboard tells it to admit that the
	// application may simply not allow this rather than insisting the user
	// select something they have already selected.
	if errors.Is(err, desktop.ErrClipboardUnchanged) {
		captured.Text = ""
		return captured, nil
	}
	return captured, err
}

// Replace pastes the rewrite over the selection.
func (p *Platform) Replace(ctx context.Context, _ desktop.Capture, replacement string) error {
	return p.ui.Paste(ctx, replacement)
}

// Overlay opens the window showing the rewrite.
func (p *Platform) Overlay(_ context.Context, c desktop.Capture) (desktop.Overlay, error) {
	return p.ui.ShowOverlay(c)
}

// noticeDuration is how long a notice stays up when nobody dismisses it.
const noticeDuration = 3 * time.Second

// Notify says something when there is no overlay to say it in.
//
// Through the overlay rather than a tray balloon, which is the obvious
// alternative and a worse one: a balloon needs a tray icon to hang off,
// Windows collapses it into the notification centre where nobody looks, and the
// user is currently staring at the place they just pressed the shortcut. This
// appears there, and goes away by itself.
func (p *Platform) Notify(title, body string) {
	overlay, err := p.ui.ShowOverlay(desktop.Capture{})
	if err != nil {
		// Nowhere left to report it. Swallowing is correct rather than
		// convenient: this is itself the reporting path, and failing it must
		// not take down a rewrite that already went fine.
		return
	}

	overlay.Fail(errors.New(title + " — " + body))

	go func() {
		defer overlay.Close()
		ctx, cancel := context.WithTimeout(context.Background(), noticeDuration)
		defer cancel()
		// Whichever comes first: the user presses a key, or it times out.
		_, _ = overlay.Await(ctx)
	}()
}
