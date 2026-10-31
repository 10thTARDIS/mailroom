// SPDX-License-Identifier: AGPL-3.0-only

package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"
)

// defaultShutdownTimeout bounds how long graceful shutdown waits for
// in-flight requests to drain before giving up.
const defaultShutdownTimeout = 10 * time.Second

// ReadinessFunc reports whether the server is ready to take traffic (e.g.
// by pinging the database pool). It is injected by main so this package
// never depends on internal/store directly.
type ReadinessFunc func(ctx context.Context) error

// Server is the HTTP lifecycle manager: /healthz, /readyz, /metrics, plus
// whatever else is mounted before Run is called.
type Server struct {
	httpServer      *http.Server
	logger          *slog.Logger
	shutdownTimeout time.Duration
	appHandler      http.Handler
}

// Option customizes a Server built by New.
type Option func(*Server)

// WithShutdownTimeout overrides the default graceful-shutdown deadline.
func WithShutdownTimeout(d time.Duration) Option {
	return func(s *Server) { s.shutdownTimeout = d }
}

// WithAppHandler mounts the application (JMAP API, auth routes, web client) at
// "/" — the subtree pattern, so it receives every path the operational probes
// don't claim.
//
// The probes stay in front of it: "/healthz", "/readyz", and "/metrics" are
// registered as explicit (non-wildcard, non-subtree) patterns, and Go 1.22+
// ServeMux precedence gives the most specific pattern the request, so they win
// over "/" regardless of registration order. That is deliberate and load
// bearing: the probes MUST remain unauthenticated (a kubelet/compose healthcheck
// or a Prometheus scraper carries no session), while the app handler passed here
// is expected to be ALREADY wrapped in the caller's auth middleware. This
// package therefore never authenticates anything and never sees a user identity.
func WithAppHandler(h http.Handler) Option {
	return func(s *Server) { s.appHandler = h }
}

// New builds a Server listening on addr, with:
//   - GET /healthz — liveness; always 200 once the server is serving.
//   - GET /readyz — readiness; 200 only when ready(ctx) returns nil.
//     A nil ready func is treated as always-ready.
//   - GET /metrics — metricsHandler, if non-nil (typically
//     (*metrics.Metrics).Handler()).
//   - / — the application handler, if WithAppHandler was passed.
func New(addr string, logger *slog.Logger, metricsHandler http.Handler, ready ReadinessFunc, opts ...Option) *Server {
	if logger == nil {
		logger = slog.Default()
	}

	s := &Server{
		logger:          logger,
		shutdownTimeout: defaultShutdownTimeout,
	}
	for _, opt := range opts {
		opt(s)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", healthzHandler)
	mux.HandleFunc("/readyz", readyzHandler(ready, logger))
	if metricsHandler != nil {
		mux.Handle("/metrics", metricsHandler)
	}
	if s.appHandler != nil {
		mux.Handle("/", s.appHandler)
	}

	// ⚠ TIMEOUTS ON THE PUBLIC SERVER (#351). This had none, while the crypto
	// process — which listens on a 0600 Unix socket that is not network
	// reachable at all — set all four (internal/pgpkeyd/server.go). The exposed
	// listener was the unprotected one. Without ReadHeaderTimeout a client that
	// opens a connection and dribbles headers holds a goroutine and an fd
	// indefinitely, and nothing caps concurrent connections.
	//
	// ⚠ WriteTimeout is deliberately NOT set. /jmap/eventsource/ holds a
	// response open for the life of the subscription by design, and a blanket
	// write deadline would sever every push connection on a timer. Per-write
	// deadlines belong in that handler via http.ResponseController, not here.
	//
	// gosec's G112 covers exactly this and does not run: .golangci.yml selects
	// the `standard` linter set, which excludes gosec.
	s.httpServer = &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    1 << 20,
	}
	return s
}

// Addr returns the configured listen address.
func (s *Server) Addr() string { return s.httpServer.Addr }

// Handler returns the composed HTTP surface: the probes, and the (already
// auth-wrapped) app handler behind them. It exists so a caller can test the
// REAL, whole surface — the exact handler Run serves — rather than a hand-rolled
// mux that resembles it. cmd/mailroom's gate test uses it to prove that an
// unauthenticated request reaches nothing but the sign-in page and the probes.
func (s *Server) Handler() http.Handler { return s.httpServer.Handler }

// Run starts the HTTP server and blocks until ctx is cancelled, then
// performs a graceful shutdown bounded by the configured shutdown timeout.
// It returns a non-nil error only on an unrecoverable startup/shutdown
// failure; context cancellation followed by a clean shutdown returns nil.
func (s *Server) Run(ctx context.Context) error {
	serveErr := make(chan error, 1)
	go func() {
		s.logger.Info("http server listening", "addr", s.httpServer.Addr)
		if err := s.httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serveErr <- fmt.Errorf("server: listen and serve: %w", err)
			return
		}
		serveErr <- nil
	}()

	select {
	case err := <-serveErr:
		// The server exited on its own (bind failure, etc.) before ctx was
		// cancelled.
		return err
	case <-ctx.Done():
	}

	s.logger.Info("shutting down http server", "timeout", s.shutdownTimeout)
	shutdownCtx, cancel := context.WithTimeout(context.Background(), s.shutdownTimeout)
	defer cancel()
	if err := s.httpServer.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("server: graceful shutdown: %w", err)
	}

	// Reap the serve goroutine's result; ListenAndServe returns promptly
	// once Shutdown has closed all listeners.
	<-serveErr
	return nil
}

func healthzHandler(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

func readyzHandler(ready ReadinessFunc, logger *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if ready == nil {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("ok"))
			return
		}
		if err := ready(r.Context()); err != nil {
			logger.Warn("readiness check failed", "error", err)
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = fmt.Fprintf(w, "not ready: %v", err)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	}
}
