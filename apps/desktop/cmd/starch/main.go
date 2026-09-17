// Command starch is the Linux shell: hot key, text capture, overlay and
// replacement, with starchd doing everything that is not OS-specific.
//
// It is the counterpart of the macOS app in apps/macos and speaks the same
// wire contract, documented in api/README.md. Nothing here is a second daemon:
// cmd/starchd is spawned as a child and remains the only process that ever
// holds an API key or calls a provider.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/srikary12/starch/apps/desktop/internal/cli"
	"github.com/srikary12/starch/apps/desktop/internal/client"
	"github.com/srikary12/starch/apps/desktop/internal/daemon"
	"github.com/srikary12/starch/apps/desktop/internal/secret"
	"github.com/srikary12/starch/apps/desktop/internal/settings"
	"github.com/srikary12/starch/internal/brand"
	"github.com/srikary12/starch/internal/catalog"
	"github.com/srikary12/starch/internal/config"
)

// version is stamped at build time with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	// Ctrl-C during a rewrite must close the connection, not just stop
	// printing: the contract is explicit that a client which merely stops
	// reading has cancelled nothing and is still being billed.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := run(ctx, os.Args[1:]); err != nil {
		if errors.Is(err, context.Canceled) {
			os.Exit(130)
		}
		fmt.Fprintf(os.Stderr, "%s: %v\n", brand.Slug, err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string) error {
	if len(args) == 0 {
		usage()
		return runDesktop(ctx)
	}

	switch args[0] {
	case "run":
		return runDesktop(ctx)
	case "config":
		return cli.Run(args[1:], configEnv())
	case "rewrite":
		return cli.RunRewrite(ctx, args[1:], rewriteEnv())
	case "version", "--version", "-v":
		fmt.Printf("%s %s\n", brand.Slug, version)
		return nil
	case "help", "--help", "-h":
		usage()
		return nil
	}
	usage()
	return fmt.Errorf("unknown command %q", args[0])
}

func usage() {
	fmt.Fprintf(os.Stderr, `%s — rewrite selected text, without leaving the app you are in.

  starch run               the shell: a global shortcut that rewrites
                           whatever text is selected
  starch config            show or change the configuration
  starch config --list     the providers, endpoints and models on offer
  starch config --key      store an API key in the keyring
  starch rewrite < file    rewrite text on stdin, streaming to stdout
  starch version

`, brand.Name)
}

func store() *settings.Store {
	path, err := settings.DefaultPath()
	if err != nil {
		// Only reachable with no home directory at all, where nothing else
		// would work either. A relative path keeps the failure legible.
		path = settings.FileName
	}
	return settings.NewStore(path)
}

func configEnv() cli.Env {
	return cli.Env{
		Stdout:      os.Stdout,
		Stderr:      os.Stderr,
		Store:       store(),
		Catalog:     catalog.Builtin(),
		OpenSecrets: openSecrets,
		ReadSecret:  cli.ReadSecretFromTerminal,
	}
}

func rewriteEnv() cli.RewriteEnv {
	return cli.RewriteEnv{
		Stdin:       os.Stdin,
		Stdout:      os.Stdout,
		Stderr:      os.Stderr,
		Store:       store(),
		OpenSecrets: openSecrets,
		Connect:     connect,
	}
}

func openSecrets() (cli.SecretStore, func() error, error) {
	store, err := secret.Open()
	if err != nil {
		return nil, func() error { return nil }, err
	}
	return store, store.Close, nil
}

// connect spawns a daemon of our own and waits for it to answer.
//
// On its own socket, named for this process, rather than the one a running
// desktop shell would own. Two daemons must never contend for one socket — the
// second refuses to start rather than stealing the address — and a one-shot
// command has no way to authenticate to a daemon someone else spawned, because
// the handshake token only exists in that parent's memory.
func connect(ctx context.Context, prefs settings.Preferences) (*client.Client, func(), error) {
	executable, err := daemon.Locate()
	if err != nil {
		return nil, nil, err
	}
	dir, err := config.RuntimeDir()
	if err != nil {
		return nil, nil, err
	}
	socket := filepath.Join(dir, fmt.Sprintf("cli-%d.sock", os.Getpid()))

	level := slog.LevelError
	if prefs.Debug {
		level = slog.LevelDebug
	}
	supervisor, err := daemon.New(daemon.Options{
		Executable: executable,
		SocketPath: socket,
		Debug:      prefs.Debug,
		Logger:     slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level})),
	})
	if err != nil {
		return nil, nil, err
	}

	running, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = supervisor.Run(running)
	}()
	shutdown := func() {
		cancel()
		<-done
	}

	ready, cancelReady := context.WithTimeout(running, 10*time.Second)
	defer cancelReady()
	if err := supervisor.WaitHealthy(ready); err != nil {
		shutdown()
		return nil, nil, err
	}
	return supervisor.Client(), shutdown, nil
}
