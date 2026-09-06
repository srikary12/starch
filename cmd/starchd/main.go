// Command starchd is the local daemon behind the native shells.
//
// It is not a network service: it listens on a Unix domain socket owned by the
// user who spawned it, and the only traffic it ever originates is the call to
// the user's own configured model endpoint. It is spawned by a shell (the
// macOS menu bar app today) which owns its lifetime.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/srikary12/starch/internal/brand"
	"github.com/srikary12/starch/internal/config"
	"github.com/srikary12/starch/internal/server"
)

// version is stamped at build time with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	if err := run(); err != nil {
		// slog is already configured by the time most failures happen, but a
		// config error can precede it, so write to stderr directly.
		fmt.Fprintf(os.Stderr, "%s: %v\n", brand.Daemon, err)
		os.Exit(1)
	}
}

func run() error {
	showVersion := flag.Bool("version", false, "print the version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Printf("%s %s\n", brand.Daemon, version)
		return nil
	}

	cfg, err := config.Load()
	if err != nil {
		return err
	}

	// The shell reads our stderr through a pipe. If the shell is killed, that
	// pipe breaks, and Go's default is to kill the process on SIGPIPE for file
	// descriptors 1 and 2 — which would take us down mid-log, before the
	// orphan watchdog could shut down cleanly and unlink the socket. Dropping
	// log lines into a dead pipe is the correct behaviour instead.
	signal.Ignore(syscall.SIGPIPE)

	level := slog.LevelInfo
	if cfg.Debug {
		level = slog.LevelDebug
	}
	// Logs go to stderr, which the shell pipes into its own log. Nothing
	// derived from an API key or from user text is ever logged.
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
	slog.SetDefault(log)

	srv, err := server.New(server.Options{
		Token:       cfg.Token,
		IdleTimeout: cfg.IdleTimeout,
		Version:     version,
		Logger:      log,
	})
	if err != nil {
		return err
	}

	ln, err := server.Listen(cfg.SocketPath)
	if err != nil {
		return err
	}
	// net.UnixListener unlinks the socket on Close, so a clean exit leaves no
	// stale file behind. A SIGKILL still can, which is why Listen probes.
	defer ln.Close()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Backstop for the case the shell dies without terminating us: the daemon
	// holds an API key in memory and must not outlive its owner.
	ctx = withParentWatch(ctx, log)

	log.Info("listening",
		"socket", cfg.SocketPath,
		"version", version,
		"api", server.APIVersion,
		"idle_timeout", cfg.IdleTimeout,
	)

	switch err := srv.Serve(ctx, ln); {
	case err == nil:
		log.Info("shutting down on signal")
		return nil
	case errors.Is(err, server.ErrIdle):
		// Not a failure: the shell went away and we cleaned up after it.
		return nil
	default:
		return err
	}
}
