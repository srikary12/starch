// Package rewrite turns preferences plus a selection into a streamed rewrite.
//
// It sits between the shell's two front ends — the overlay and the terminal —
// and the daemon client, so that the session handshake and its one retry are
// written once. Both front ends need exactly the same behaviour and getting it
// subtly different in two places is how "it works from the command line but
// not from the hot key" happens.
package rewrite

import (
	"context"
	"errors"
	"fmt"

	"github.com/srikary12/starch/apps/linux/internal/client"
	"github.com/srikary12/starch/apps/linux/internal/settings"
)

// Config is everything needed to configure a daemon session.
type Config struct {
	Preferences settings.Preferences
	// APIKey is read from the keyring immediately before use and is never
	// stored anywhere by this process.
	APIKey string
}

func (c Config) sessionRequest() client.SessionRequest {
	return client.SessionRequest{
		Provider:       c.Preferences.Provider,
		Model:          c.Preferences.Model,
		BaseURL:        c.Preferences.BaseURL,
		APIKey:         c.APIKey,
		ThinkingEffort: c.Preferences.ThinkingEffort,
	}
}

// ErrNoProvider reports that nothing has been configured yet.
var ErrNoProvider = errors.New("no provider is configured. Run `starch config` to choose one")

// EnsureSession hands the daemon its credentials.
//
// Called when the shell starts, so the first rewrite does not pay for it, and
// again whenever a setting changes. The session lives only as long as the
// daemon process, which is why Run below can still meet a daemon without one.
func EnsureSession(ctx context.Context, c *client.Client, cfg Config) error {
	if cfg.Preferences.Provider == "" || cfg.Preferences.Model == "" {
		return ErrNoProvider
	}
	if _, err := c.Session(ctx, cfg.sessionRequest()); err != nil {
		return err
	}
	return nil
}

// Run streams a rewrite, establishing a session first if the daemon has none.
//
// A 428 is not an error worth showing anyone: the daemon restarted, or idled
// out, and its session went with it. The contract's instruction is to post
// credentials and retry once, which is what happens here. Retrying more than
// once would turn a genuinely rejected key into a loop.
func Run(
	ctx context.Context,
	c *client.Client,
	cfg Config,
	req client.RewriteRequest,
	onDelta func(string),
) (client.RewriteResult, error) {
	result, err := c.Rewrite(ctx, req, onDelta)
	if !client.IsNoSession(err) {
		return result, err
	}

	if err := EnsureSession(ctx, c, cfg); err != nil {
		return client.RewriteResult{}, fmt.Errorf("configuring the helper: %w", err)
	}
	return c.Rewrite(ctx, req, onDelta)
}
