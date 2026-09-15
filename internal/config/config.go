// Package config resolves the daemon's runtime configuration from the
// environment the native shell spawns it with.
//
// Nothing here is tied to one platform. Where the daemon's files belong does
// differ per platform, and paths.go answers that in one place; a shell that
// disagrees can override every path with an environment variable rather than
// the daemon growing a build tag.
package config

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"time"

	"github.com/srikary12/starch/internal/brand"
)

// Environment variables the daemon reads. The shell sets these at spawn.
var (
	// EnvSocket overrides the Unix domain socket path.
	EnvSocket = brand.EnvPrefix + "SOCKET"
	// EnvToken carries the handshake token. Required.
	EnvToken = brand.EnvPrefix + "TOKEN"
	// EnvIdleTimeout overrides how long the daemon survives without a
	// request, as a Go duration ("30s"). Zero disables idle exit.
	EnvIdleTimeout = brand.EnvPrefix + "IDLE_TIMEOUT"
	// EnvDebug enables verbose timing logs when set to a true-ish value.
	EnvDebug = brand.EnvPrefix + "DEBUG"
)

// DefaultIdleTimeout is how long the daemon runs without hearing from a shell
// before exiting. The shell heartbeats well inside this window, so reaching it
// means the shell is gone and the daemon is holding an API key nobody wants.
const DefaultIdleTimeout = 30 * time.Second

// Config is the daemon's resolved runtime configuration.
type Config struct {
	// SocketPath is the Unix domain socket the daemon listens on.
	SocketPath string
	// Token is the handshake token every request must present.
	Token string
	// IdleTimeout is the no-request window after which the daemon exits.
	// Zero disables idle exit.
	IdleTimeout time.Duration
	// Debug enables verbose timing logs.
	Debug bool
}

// ErrNoToken reports that the required handshake token was not supplied.
var ErrNoToken = errors.New("no handshake token: " + EnvToken + " must be set by the parent process")

// Load resolves configuration from the process environment.
func Load() (Config, error) {
	cfg := Config{
		SocketPath:  os.Getenv(EnvSocket),
		Token:       os.Getenv(EnvToken),
		IdleTimeout: DefaultIdleTimeout,
		Debug:       truthy(os.Getenv(EnvDebug)),
	}

	if cfg.SocketPath == "" {
		p, err := DefaultSocketPath()
		if err != nil {
			return Config{}, err
		}
		cfg.SocketPath = p
	}

	if cfg.Token == "" {
		return Config{}, ErrNoToken
	}

	if raw := os.Getenv(EnvIdleTimeout); raw != "" {
		d, err := time.ParseDuration(raw)
		if err != nil {
			return Config{}, fmt.Errorf("parsing %s=%q: %w", EnvIdleTimeout, raw, err)
		}
		if d < 0 {
			return Config{}, fmt.Errorf("%s must not be negative, got %s", EnvIdleTimeout, d)
		}
		cfg.IdleTimeout = d
	}

	return cfg, nil
}

func truthy(s string) bool {
	if s == "" {
		return false
	}
	b, err := strconv.ParseBool(s)
	if err != nil {
		// Treat any other non-empty value as on, so STARCH_DEBUG=yes works.
		return true
	}
	return b
}
