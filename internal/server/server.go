package server

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/srikary12/starch/internal/config"
	"github.com/srikary12/starch/internal/prompt"
	"github.com/srikary12/starch/internal/provider"
)

// ErrIdle is returned by Serve when the daemon shut itself down because no
// request arrived within the idle timeout. It means the shell is gone.
var ErrIdle = errors.New("idle timeout reached")

// shutdownGrace bounds how long Serve waits for in-flight requests during a
// graceful stop before forcing the listener closed. A stuck SSE stream must
// not keep a process holding an API key alive.
const shutdownGrace = 2 * time.Second

// Options configures a Server.
type Options struct {
	// Token is the handshake token every request must present. Required.
	Token string
	// IdleTimeout is the no-request window after which Serve returns ErrIdle.
	// Zero disables idle exit.
	IdleTimeout time.Duration
	// Version is reported by /healthz.
	Version string
	// Logger receives daemon logs. Defaults to slog.Default().
	Logger *slog.Logger
	// Now is the clock, overridable in tests. Defaults to time.Now.
	Now func() time.Time
	// PresetDir holds presets.json. Defaults to the support directory; tests
	// point it at a temporary one.
	PresetDir string
	// HTTPClient talks to model endpoints. Defaults to provider.NewHTTPClient.
	// One client is shared across every rewrite on purpose: keeping the
	// connection warm is most of why a daemon beats spawning per request.
	HTTPClient *http.Client
}

// Server serves the wire contract over a net.Listener, normally a Unix domain
// socket. It is transport-agnostic on purpose: the listener is supplied by the
// caller so tests can use httptest and a future shell can use a named pipe.
type Server struct {
	opts    Options
	log     *slog.Logger
	now     func() time.Time
	started time.Time

	// lastActive is the Unix-nano timestamp of the most recent request
	// boundary. inFlight counts requests currently being served; a long SSE
	// stream must not look idle just because it started a while ago.
	lastActive atomic.Int64
	inFlight   atomic.Int64

	httpClient *http.Client
	presets    *prompt.Store

	// mu guards session. Rewrites read it; /v1/session replaces it.
	mu      sync.RWMutex
	session *session
}

// New builds a Server. It returns an error rather than panicking on a missing
// token so that a misconfigured spawn fails with a readable message.
func New(opts Options) (*Server, error) {
	if opts.Token == "" {
		return nil, errors.New("server: token is required")
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Version == "" {
		opts.Version = "dev"
	}
	if opts.HTTPClient == nil {
		opts.HTTPClient = provider.NewHTTPClient()
	}
	if opts.PresetDir == "" {
		dir, err := config.SupportDir()
		if err != nil {
			return nil, fmt.Errorf("server: resolving the support directory: %w", err)
		}
		opts.PresetDir = dir
	}

	s := &Server{
		opts:       opts,
		log:        opts.Logger,
		now:        opts.Now,
		started:    opts.Now(),
		httpClient: opts.HTTPClient,
		presets:    prompt.NewStore(opts.PresetDir),
	}
	s.touch()
	return s, nil
}

// Handler returns the routed, authenticated HTTP handler.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	// Routes are registered without a method pattern and checked by only() so
	// that a method mismatch produces the same JSON error envelope as every
	// other failure. A shell that gets plain text back has hit a bug.
	mux.HandleFunc("/healthz", only(http.MethodGet, s.handleHealthz))
	mux.HandleFunc("/"+APIVersion+"/session", only(http.MethodPost, s.handleSession))
	mux.HandleFunc("/"+APIVersion+"/rewrite", only(http.MethodPost, s.handleRewrite))
	mux.HandleFunc("/"+APIVersion+"/models", only(http.MethodGet, s.handleModels))
	mux.HandleFunc("/"+APIVersion+"/presets", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			s.handleGetPresets(w, r)
		case http.MethodPut:
			s.handlePutPresets(w, r)
		default:
			w.Header().Set("Allow", "GET, PUT")
			writeError(w, http.StatusMethodNotAllowed, ErrMethodNotAllowed,
				"This endpoint accepts GET and PUT.")
		}
	})

	// Catch-all. Anything unrouted, including the /v1 endpoints not yet
	// implemented, lands here as a JSON 404.
	mux.HandleFunc("/", s.handleNotFound)

	return s.withActivity(s.withAuth(mux))
}

// Serve accepts connections on ln until ctx is cancelled or the idle timeout
// elapses, then shuts down gracefully and closes ln.
//
// It returns ErrIdle on idle exit, nil on ctx cancellation, and a non-nil
// error if the listener itself failed.
func (s *Server) Serve(ctx context.Context, ln net.Listener) error {
	httpSrv := &http.Server{
		Handler: s.Handler(),
		// Over a 0600 socket a slowloris is not much of a threat, but an
		// unbounded header read is still a way to wedge the daemon.
		ReadHeaderTimeout: 10 * time.Second,
		ErrorLog:          slog.NewLogLogger(s.log.Handler(), slog.LevelDebug),
	}

	serveErr := make(chan error, 1)
	go func() {
		err := httpSrv.Serve(ln)
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		serveErr <- err
	}()

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	var stopReason error
	select {
	case err := <-serveErr:
		return err
	case <-ctx.Done():
	case <-s.idleSignal(ctx):
		stopReason = ErrIdle
	}

	shutCtx, shutCancel := context.WithTimeout(context.Background(), shutdownGrace)
	defer shutCancel()
	if err := httpSrv.Shutdown(shutCtx); err != nil {
		s.log.Warn("graceful shutdown timed out, closing connections", "error", err)
		_ = httpSrv.Close()
	}
	<-serveErr

	return stopReason
}

// idleSignal returns a channel that closes once the daemon has gone
// IdleTimeout without a request boundary and has nothing in flight.
func (s *Server) idleSignal(ctx context.Context) <-chan struct{} {
	out := make(chan struct{})
	if s.opts.IdleTimeout <= 0 {
		return out // never fires
	}

	go func() {
		defer close(out)
		// Poll at a fraction of the timeout so the overshoot stays small
		// without spinning. Bounded below so a tiny timeout in tests does not
		// produce a pathological tick rate.
		interval := max(s.opts.IdleTimeout/4, time.Millisecond)
		t := time.NewTicker(interval)
		defer t.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if s.inFlight.Load() > 0 {
					continue
				}
				last := time.Unix(0, s.lastActive.Load())
				if s.now().Sub(last) >= s.opts.IdleTimeout {
					s.log.Info("no requests within idle timeout, exiting",
						"idle_timeout", s.opts.IdleTimeout)
					return
				}
			}
		}
	}()
	return out
}

func (s *Server) touch() { s.lastActive.Store(s.now().UnixNano()) }

// withActivity records request boundaries for the idle timer.
func (s *Server) withActivity(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.inFlight.Add(1)
		s.touch()
		defer func() {
			s.touch()
			s.inFlight.Add(-1)
		}()
		next.ServeHTTP(w, r)
	})
}

// withAuth enforces the handshake token.
//
// The socket's 0600 mode already limits reach to this user's processes; the
// token is defence in depth against another of the user's own processes
// stumbling onto the socket. It is not a defence against an attacker who
// already runs as the user — they can read the daemon's environment.
func (s *Server) withAuth(next http.Handler) http.Handler {
	want := []byte(s.opts.Token)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, ok := bearerToken(r)
		// Compare unconditionally so a missing header and a wrong token take
		// the same path, and use a constant-time comparison so the token
		// cannot be recovered a byte at a time.
		match := subtle.ConstantTimeCompare([]byte(got), want) == 1
		if !ok || !match {
			writeError(w, http.StatusUnauthorized, ErrUnauthorized,
				"missing or invalid handshake token")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func bearerToken(r *http.Request) (string, bool) {
	h := r.Header.Get("Authorization")
	const prefix = "Bearer "
	if len(h) <= len(prefix) || !strings.EqualFold(h[:len(prefix)], prefix) {
		return "", false
	}
	return h[len(prefix):], true
}

// only wraps h so that any other method yields a JSON 405 with an Allow header.
func only(method string, h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != method {
			w.Header().Set("Allow", method)
			writeError(w, http.StatusMethodNotAllowed, ErrMethodNotAllowed,
				fmt.Sprintf("%s is not allowed on %s, use %s", r.Method, r.URL.Path, method))
			return
		}
		h(w, r)
	}
}

func (s *Server) handleNotFound(w http.ResponseWriter, r *http.Request) {
	writeError(w, http.StatusNotFound, ErrNotFound,
		fmt.Sprintf("no such endpoint: %s (see api/README.md for the %s contract)", r.URL.Path, APIVersion))
}
