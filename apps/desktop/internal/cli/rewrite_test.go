package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/srikary12/starch/apps/desktop/internal/client"
	"github.com/srikary12/starch/apps/desktop/internal/secret"
	"github.com/srikary12/starch/apps/desktop/internal/settings"
	"github.com/srikary12/starch/internal/catalog"
)

// stubDaemon answers a session and streams a fixed rewrite.
func stubDaemon(t *testing.T) *client.Client {
	t.Helper()

	dir, err := os.MkdirTemp("", "st")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })

	path := filepath.Join(dir, "d.sock")
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("listening: %v", err)
	}

	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/session":
			_ = json.NewEncoder(w).Encode(client.SessionResponse{OK: true})
		case "/v1/rewrite":
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(200)
			_, _ = w.Write([]byte(
				"data: {\"delta\":\"Thanks for \"}\n\n" +
					"data: {\"delta\":\"the update\"}\n\n" +
					"data: {\"done\":true,\"full\":\"Thanks for the update\"}\n\n"))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	})

	return client.New(path, "token")
}

type rewriteHarness struct {
	env       RewriteEnv
	out       *bytes.Buffer
	errOut    *bytes.Buffer
	secrets   *fakeSecrets
	connected bool
}

func newRewriteHarness(t *testing.T, prefs settings.Preferences) *rewriteHarness {
	t.Helper()

	store := settings.NewStore(filepath.Join(t.TempDir(), settings.FileName))
	if err := store.Save(prefs); err != nil {
		t.Fatalf("Save: %v", err)
	}

	h := &rewriteHarness{
		out:     &bytes.Buffer{},
		errOut:  &bytes.Buffer{},
		secrets: newFakeSecrets(),
	}
	daemon := stubDaemon(t)
	h.env = RewriteEnv{
		Stdout:      h.out,
		Stderr:      h.errOut,
		Store:       store,
		OpenSecrets: h.secrets.open,
		Connect: func(context.Context, settings.Preferences) (*client.Client, func(), error) {
			h.connected = true
			return daemon, func() {}, nil
		},
	}
	return h
}

func configured(t *testing.T) settings.Preferences {
	t.Helper()
	prefs := settings.Default()
	if provider := settings.FindProvider(catalog.Builtin(), prefs.Provider); provider == nil || !provider.RequiresKey {
		t.Skip("the default provider needs no key, so the key paths cannot be checked through it")
	}
	return prefs
}

func TestRewriteStreamsToStdout(t *testing.T) {
	prefs := configured(t)
	h := newRewriteHarness(t, prefs)
	h.secrets.values[secret.Account(prefs.Provider)] = "sk-test"
	h.env.Stdin = strings.NewReader("thx for the update")

	if err := RunRewrite(context.Background(), nil, h.env); err != nil {
		t.Fatalf("RunRewrite: %v", err)
	}
	if got := strings.TrimRight(h.out.String(), "\n"); got != "Thanks for the update" {
		t.Errorf("stdout = %q", got)
	}
}

// Nothing to rewrite is not worth spawning a daemon for, let alone calling a
// provider about.
func TestRewriteWithNoTextDoesNothing(t *testing.T) {
	prefs := configured(t)
	h := newRewriteHarness(t, prefs)
	h.secrets.values[secret.Account(prefs.Provider)] = "sk-test"
	h.env.Stdin = strings.NewReader("   \n  ")

	if err := RunRewrite(context.Background(), nil, h.env); err == nil {
		t.Fatal("an empty selection was sent")
	}
	if h.connected {
		t.Error("a daemon was started for an empty selection")
	}
}

// A missing key must be named as such, before anything is spawned, and it must
// say which command fixes it.
func TestRewriteWithoutAKeySaysWhichCommandFixesIt(t *testing.T) {
	prefs := configured(t)
	h := newRewriteHarness(t, prefs)
	h.env.Stdin = strings.NewReader("thx")

	err := RunRewrite(context.Background(), nil, h.env)
	if err == nil {
		t.Fatal("a rewrite was attempted with no key")
	}
	if !strings.Contains(err.Error(), "starch config --key") {
		t.Errorf("error = %v, want it to name the command", err)
	}
	if h.connected {
		t.Error("a daemon was started before the key was checked")
	}
}

// An endpoint that needs no key must not need a keyring either. That is the
// whole point for someone running Ollama so nothing leaves the machine.
func TestALocalEndpointNeedsNeitherKeyNorKeyring(t *testing.T) {
	built := catalog.Builtin()
	var local *catalog.Provider
	for i := range built.Providers {
		if !built.Providers[i].RequiresKey {
			local = &built.Providers[i]
			break
		}
	}
	if local == nil {
		t.Skip("no key-free provider in the catalog")
	}

	prefs := settings.Default()
	prefs.UseProvider(built, local.ID)
	// A local endpoint publishes no model list, so choosing one leaves the
	// model for the user to type. Typing it is part of configuring it.
	prefs.Model = "qwen3"

	h := newRewriteHarness(t, prefs)
	h.secrets.openErr = errors.New("no keyring is running")
	h.env.Stdin = strings.NewReader("thx for the update")

	if err := RunRewrite(context.Background(), nil, h.env); err != nil {
		t.Fatalf("RunRewrite: %v", err)
	}
	if !strings.Contains(h.out.String(), "Thanks for the update") {
		t.Errorf("stdout = %q", h.out.String())
	}
}

// Choosing Ollama and stopping there is a configuration someone will really
// have. The message has to name the missing piece and what to type, not send
// them back to re-pick the provider they just picked.
func TestRewriteWithoutAModelSaysWhatToType(t *testing.T) {
	built := catalog.Builtin()
	prefs := settings.Default()
	prefs.UseProvider(built, "openai_compatible")
	if prefs.Model != "" {
		t.Skip("this endpoint now carries a default model")
	}

	h := newRewriteHarness(t, prefs)
	h.env.Stdin = strings.NewReader("thx")

	err := RunRewrite(context.Background(), nil, h.env)
	if !errors.Is(err, settings.ErrNoModel) {
		t.Fatalf("err = %v, want ErrNoModel", err)
	}
	if !strings.Contains(err.Error(), "--model") {
		t.Errorf("error %q does not say which command sets one", err)
	}
	if !strings.Contains(err.Error(), prefs.BaseURL) {
		t.Errorf("error %q does not name the endpoint", err)
	}
	if h.connected {
		t.Error("a daemon was started for a configuration that cannot be used")
	}
}

func TestRewriteWithNothingConfigured(t *testing.T) {
	h := newRewriteHarness(t, settings.Preferences{HotKey: settings.DefaultHotKey})
	h.env.Stdin = strings.NewReader("thx")

	err := RunRewrite(context.Background(), nil, h.env)
	if err == nil || !strings.Contains(err.Error(), "starch config") {
		t.Fatalf("err = %v, want it to name the command that configures a provider", err)
	}
}
