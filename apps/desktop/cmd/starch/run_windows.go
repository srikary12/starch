package main

import (
	"context"
	"fmt"
	"os"

	"github.com/srikary12/starch/apps/desktop/internal/client"
	"github.com/srikary12/starch/apps/desktop/internal/desktop"
	"github.com/srikary12/starch/apps/desktop/internal/rewrite"
	"github.com/srikary12/starch/apps/desktop/internal/secret"
	"github.com/srikary12/starch/apps/desktop/internal/settings"
	"github.com/srikary12/starch/apps/desktop/internal/win32"
	"github.com/srikary12/starch/internal/brand"
	"github.com/srikary12/starch/internal/catalog"
)

// runDesktop is the shell proper: press the shortcut anywhere, see a rewrite,
// press Enter.
//
// The order here is the one that gives the user the best first press. The
// daemon is started and handed its credentials before the hot key is
// registered, so the first rewrite pays for a connection that is already warm
// rather than for a cold start — the 500ms budget is measured from a keystroke,
// and process spawn plus a session handshake does not fit inside it.
func runDesktop(ctx context.Context) error {
	store := store()
	prefs, err := store.Load()
	if err != nil {
		// A settings file that will not parse is worth saying out loud, but it
		// is not fatal: Load has already fallen back to the defaults, and a
		// shell that refuses to start is worse than one running on them.
		fmt.Fprintf(os.Stderr, "%s: %v\n", brand.Slug, err)
	}
	if problem := prefs.Problem(catalog.Builtin()); problem != nil {
		return fmt.Errorf("%w\n\nRun `starch config` to set one up.", problem)
	}

	key, err := apiKeyFor(prefs)
	if err != nil {
		return err
	}

	// Quitting from the tray ends the message loop by cancelling this, which
	// returns from ui.Run below and unwinds everything in order.
	ctx, quit := context.WithCancel(ctx)
	defer quit()

	daemonClient, shutdown, err := connect(ctx, prefs)
	if err != nil {
		return err
	}
	defer shutdown()

	config := rewrite.Config{Preferences: prefs, APIKey: key}
	if err := rewrite.EnsureSession(ctx, daemonClient, config); err != nil {
		return err
	}

	ui := win32.NewUIThread()
	shell := &desktop.Shell{
		Platform: win32.NewPlatform(ui),
		Streamer: streamer{client: daemonClient, config: config},
		Preset:   prefs.PresetID,
	}

	// Registering has to happen on the UI thread: Windows delivers WM_HOTKEY to
	// the thread that registered the key.
	registered := make(chan error, 1)
	ui.Post(func() {
		_, err := ui.RegisterHotKey(prefs.HotKey, func() {
			if err := shell.Trigger(ctx); err != nil {
				// The overlay reports anything the user can act on. This is for
				// the rest, and it goes to stderr rather than nowhere.
				fmt.Fprintf(os.Stderr, "%s: %v\n", brand.Slug, err)
			}
		})
		registered <- err
	})
	if err := <-registered; err != nil {
		return err
	}

	// The tray is not on the path of a rewrite -- the overlay reports
	// everything the user needs mid-flow -- so a tray that will not appear is
	// worth saying and not worth stopping for.
	tray, err := showTray(ctx, ui, daemonClient, store, shell, prefs, quit)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s: no notification-area icon: %v\n", brand.Slug, err)
	} else {
		defer tray.Close()
	}

	fmt.Fprintf(os.Stderr, "%s is running. Press %s to rewrite the selected text.\n",
		brand.Name, prefs.HotKey)

	// Blocks until ctx ends, pumping messages. Everything above runs on top of
	// this loop from here on.
	return ui.Run(ctx)
}

// streamer adapts the daemon client to what the flow needs, which is one
// method and no knowledge of sessions.
type streamer struct {
	client *client.Client
	config rewrite.Config
}

func (s streamer) Stream(
	ctx context.Context, req client.RewriteRequest, onDelta func(string),
) (string, error) {
	result, err := rewrite.Run(ctx, s.client, s.config, req, onDelta)
	return result.Full, err
}

// apiKeyFor reads the key from Credential Manager, immediately before use.
//
// Not held anywhere by this process: it goes straight into the session request
// and lives on only in the daemon's memory, which is the arrangement the whole
// design rests on.
func apiKeyFor(prefs settings.Preferences) (string, error) {
	store, closeStore, err := openSecrets()
	if err != nil {
		return "", err
	}
	defer closeStore()

	key, found, err := store.Get(secret.Account(prefs.Provider))
	if err != nil {
		return "", err
	}
	if !found {
		// Local endpoints need no key at all, so an absent one is only a
		// problem if the endpoint wants it.
		return "", nil
	}
	return key, nil
}

// showTray puts the icon in the notification area and builds its menu.
//
// The menu offers what can be enumerated and leaves everything else to `starch
// config`, which is the same division the other shells use: a list of presets
// is a menu, an API key and an endpoint are not.
func showTray(
	ctx context.Context,
	ui *win32.UIThread,
	daemonClient *client.Client,
	store *settings.Store,
	shell *desktop.Shell,
	prefs settings.Preferences,
	quit func(),
) (*win32.Tray, error) {
	tip := fmt.Sprintf("%s — %s", brand.Name, prefs.HotKey)

	tray, err := ui.ShowTray(tip, nil)
	if err != nil {
		return nil, err
	}

	// Rebuilt rather than mutated, so the checked item always reflects what was
	// actually saved rather than what was clicked.
	var rebuild func(chosen string)
	rebuild = func(chosen string) {
		tray.SetMenu(presetMenu(ctx, daemonClient, store, shell, prefs, chosen, rebuild, quit))
	}
	rebuild(prefs.PresetID)
	return tray, nil
}

func presetMenu(
	ctx context.Context,
	daemonClient *client.Client,
	store *settings.Store,
	shell *desktop.Shell,
	prefs settings.Preferences,
	chosen string,
	rebuild func(string),
	quit func(),
) []win32.MenuItem {
	items := []win32.MenuItem{
		{Label: fmt.Sprintf("%s — press %s", brand.Name, prefs.HotKey), Disabled: true},
		win32.Separator(),
	}

	// From the daemon rather than from a built-in list, because a user's own
	// presets file is the thing worth showing and only the daemon has read it.
	set, err := daemonClient.Presets(ctx)
	if err != nil {
		items = append(items, win32.MenuItem{Label: "Styles unavailable", Disabled: true})
	}
	for _, preset := range set.Presets {
		items = append(items, win32.MenuItem{
			Label:   preset.Name,
			Checked: preset.ID == chosen,
			Do: func() {
				// Saved as well as applied: a style picked from the tray should
				// still be the style tomorrow.
				prefs.PresetID = preset.ID
				shell.Preset = preset.ID
				if err := store.Save(prefs); err != nil {
					fmt.Fprintf(os.Stderr, "%s: %v\n", brand.Slug, err)
				}
				rebuild(preset.ID)
			},
		})
	}

	return append(items,
		win32.Separator(),
		win32.MenuItem{Label: "Settings are in `starch config`", Disabled: true},
		win32.Separator(),
		// Cancelling ends the message loop, which returns from runDesktop and
		// takes the daemon with it -- through the job object, which does not
		// depend on this process exiting tidily.
		win32.MenuItem{Label: "Quit " + brand.Name, Do: quit},
	)
}
