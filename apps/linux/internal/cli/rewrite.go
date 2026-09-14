package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"

	"github.com/srikary12/starch/apps/linux/internal/client"
	"github.com/srikary12/starch/apps/linux/internal/rewrite"
	"github.com/srikary12/starch/apps/linux/internal/secret"
	"github.com/srikary12/starch/apps/linux/internal/settings"
	"github.com/srikary12/starch/internal/catalog"
)

// maxSelection is the daemon's own cap, checked here so an accidental `starch
// rewrite < some.iso` is refused locally rather than after a megabyte of
// upload.
const maxSelection = 1 << 20

// RewriteEnv is everything `starch rewrite` touches.
type RewriteEnv struct {
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer

	Store       *settings.Store
	OpenSecrets func() (SecretStore, func() error, error)
	// Connect starts a daemon and returns a client for it plus a shutdown
	// function. Injected so the tests can supply a daemon of their own.
	Connect func(ctx context.Context, prefs settings.Preferences) (*client.Client, func(), error)
}

// RunRewrite implements `starch rewrite`: read a selection on stdin, stream the
// rewrite to stdout.
//
// This is the whole product without the desktop: the same daemon, session,
// keyring and streaming path the hot key will use. Having it before any X11
// code exists means the loop can be proven end to end, and it stands on its
// own as something to pipe text through.
func RunRewrite(ctx context.Context, args []string, env RewriteEnv) error {
	flags := flag.NewFlagSet("starch rewrite", flag.ContinueOnError)
	flags.SetOutput(env.Stderr)
	preset := flags.String("preset", "", "rewriting style; defaults to the configured one")
	hint := flags.String("hint", "", "a freeform steer for this rewrite only")
	flags.Usage = func() {
		fmt.Fprintf(env.Stderr, "usage: starch rewrite [flags] < text\n\n")
		flags.PrintDefaults()
	}
	if err := flags.Parse(args); err != nil {
		return err
	}

	text, err := io.ReadAll(io.LimitReader(env.Stdin, maxSelection+1))
	if err != nil {
		return fmt.Errorf("reading the text: %w", err)
	}
	if len(text) > maxSelection {
		return errors.New("that is too long to rewrite in one go")
	}
	if strings.TrimSpace(string(text)) == "" {
		return errors.New("there is no text to rewrite")
	}

	prefs, err := env.Store.Load()
	if err != nil {
		fmt.Fprintf(env.Stderr, "warning: %v\n", err)
	}
	if prefs.Provider == "" || prefs.Model == "" {
		return rewrite.ErrNoProvider
	}

	key, err := apiKey(env, prefs)
	if err != nil {
		return err
	}

	c, shutdown, err := env.Connect(ctx, prefs)
	if err != nil {
		return err
	}
	defer shutdown()

	chosen := prefs.PresetID
	if *preset != "" {
		chosen = *preset
	}

	result, err := rewrite.Run(ctx, c,
		rewrite.Config{Preferences: prefs, APIKey: key},
		client.RewriteRequest{Text: string(text), Preset: chosen, Hint: *hint},
		func(delta string) { fmt.Fprint(env.Stdout, delta) },
	)
	if err != nil {
		// Deltas have already been printed, so end the line before the error
		// lands on the same one.
		if result.Full != "" {
			fmt.Fprintln(env.Stdout)
		}
		return err
	}
	fmt.Fprintln(env.Stdout)
	return nil
}

// apiKey reads the configured provider's key from the keyring.
//
// A local endpoint needs none, and it does not even open the keyring in that
// case: demanding one there would put a password dialog in front of exactly
// the people who chose Ollama so that nothing leaves the machine.
func apiKey(env RewriteEnv, prefs settings.Preferences) (string, error) {
	provider := settings.FindProvider(catalog.Builtin(), prefs.Provider)
	needed := provider == nil || provider.RequiresKey

	store, closeStore, err := env.OpenSecrets()
	if err != nil {
		if !needed {
			return "", nil
		}
		return "", err
	}
	defer func() { _ = closeStore() }()

	key, found, err := store.Get(secret.Account(prefs.Provider))
	if err != nil {
		return "", err
	}
	if (!found || key == "") && needed {
		return "", fmt.Errorf("no API key is stored for %s. Run `starch config --key`", prefs.Provider)
	}
	return key, nil
}
