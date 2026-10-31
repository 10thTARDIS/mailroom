// SPDX-License-Identifier: AGPL-3.0-only

package server

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func testLogger() *slog.Logger { return NewLoggerWriter("json", "error", io.Discard) }

func TestHealthzAlwaysOK(t *testing.T) {
	srv := New(":0", testLogger(), nil, nil)

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()
	srv.httpServer.Handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
}

func TestReadyzNilFuncIsReady(t *testing.T) {
	srv := New(":0", testLogger(), nil, nil)

	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	rec := httptest.NewRecorder()
	srv.httpServer.Handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
}

func TestReadyzReportsReady(t *testing.T) {
	ready := func(ctx context.Context) error { return nil }
	srv := New(":0", testLogger(), nil, ready)

	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	rec := httptest.NewRecorder()
	srv.httpServer.Handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
}

func TestReadyzReportsNotReady(t *testing.T) {
	wantErr := errors.New("db unreachable")
	ready := func(ctx context.Context) error { return wantErr }
	srv := New(":0", testLogger(), nil, ready)

	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	rec := httptest.NewRecorder()
	srv.httpServer.Handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusServiceUnavailable)
	}
	if !strings.Contains(rec.Body.String(), "db unreachable") {
		t.Errorf("body = %q, want it to mention the readiness error", rec.Body.String())
	}
}

func TestMetricsHandlerMounted(t *testing.T) {
	stub := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("stub-metrics-output"))
	})
	srv := New(":0", testLogger(), stub, nil)

	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	rec := httptest.NewRecorder()
	srv.httpServer.Handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	if rec.Body.String() != "stub-metrics-output" {
		t.Errorf("body = %q, want stub output", rec.Body.String())
	}
}

func TestMetricsAbsentWhenHandlerNil(t *testing.T) {
	srv := New(":0", testLogger(), nil, nil)

	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	rec := httptest.NewRecorder()
	srv.httpServer.Handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d (no handler mounted)", rec.Code, http.StatusNotFound)
	}
}

// denyAll stands in for an auth middleware that rejects every request. Anything
// it wraps is, by construction, unreachable without authentication.
func denyAll(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	})
}

func TestAppHandlerMountedAtRoot(t *testing.T) {
	app := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("app:" + r.URL.Path))
	})
	srv := New(":0", testLogger(), nil, nil, WithAppHandler(app))

	for _, path := range []string{"/", "/jmap", "/inbox/42", "/anything/at/all"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		rec := httptest.NewRecorder()
		srv.httpServer.Handler.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("%s: status = %d, want 200", path, rec.Code)
		}
		if rec.Body.String() != "app:"+path {
			t.Errorf("%s: body = %q, want the app handler to have served it", path, rec.Body.String())
		}
	}
}

// The probes are operational endpoints: a health checker or a Prometheus scraper
// carries no session, so they MUST stay in front of the (auth-wrapped) app
// handler and MUST stay unauthenticated. This is the test that stops a future
// "just wrap everything" refactor from silently breaking liveness.
func TestProbesWinOverAppHandlerAndStayUnauthenticated(t *testing.T) {
	app := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("app handler served %s — a probe route leaked into the app", r.URL.Path)
	})
	metrics := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("stub-metrics-output"))
	})
	ready := func(ctx context.Context) error { return nil }

	// The app is behind an auth middleware that denies everything.
	srv := New(":0", testLogger(), metrics, ready, WithAppHandler(denyAll(app)))

	for _, path := range []string{"/healthz", "/readyz", "/metrics"} {
		req := httptest.NewRequest(http.MethodGet, path, nil) // no credentials
		rec := httptest.NewRecorder()
		srv.httpServer.Handler.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Errorf("%s: status = %d, want 200 — probes must not require auth", path, rec.Code)
		}
	}

	// ...while everything else does hit the auth-wrapped app.
	req := httptest.NewRequest(http.MethodGet, "/jmap", nil)
	rec := httptest.NewRecorder()
	srv.httpServer.Handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("/jmap: status = %d, want 401 (the app handler IS behind auth)", rec.Code)
	}
}

func TestNoAppHandlerLeavesRootUnmounted(t *testing.T) {
	srv := New(":0", testLogger(), nil, nil)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	srv.httpServer.Handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (nothing mounted at /)", rec.Code)
	}
}

func TestWithShutdownTimeoutOption(t *testing.T) {
	srv := New(":0", testLogger(), nil, nil, WithShutdownTimeout(3*time.Second))
	if srv.shutdownTimeout != 3*time.Second {
		t.Fatalf("shutdownTimeout = %v, want 3s", srv.shutdownTimeout)
	}
}

func TestRunStartsAndShutsDownGracefully(t *testing.T) {
	srv := New("127.0.0.1:0", testLogger(), nil, nil)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.Run(ctx) }()

	// Give the listener a moment to come up before triggering shutdown.
	time.Sleep(100 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run() error = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run() did not return after context cancellation")
	}
}

func TestRunReturnsErrorOnBindFailure(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen: %v", err)
	}
	defer func() { _ = ln.Close() }()

	srv := New(ln.Addr().String(), testLogger(), nil, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := srv.Run(ctx); err == nil {
		t.Fatal("Run() error = nil, want bind failure error")
	}
}
