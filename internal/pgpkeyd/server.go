// SPDX-License-Identifier: AGPL-3.0-only

package pgpkeyd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"mailroom/internal/crypto"
	"mailroom/internal/pgpwire"
)

// handlerFunc decodes, dispatches and encodes one method.
type handlerFunc func(context.Context, *Service, []byte) (any, error)

// handlers is the implementation side of pgpwire.Methods. NewHandler refuses to
// build a router unless these two agree exactly, in both directions — a method
// declared but not implemented would 404 at runtime, and a method implemented
// but not declared would be served while the leak test, which walks the
// registry, never called it. Neither is allowed to be a runtime discovery.
var handlers = map[pgpwire.Method]handlerFunc{
	pgpwire.MethodGenerate: func(ctx context.Context, s *Service, body []byte) (any, error) {
		var req pgpwire.GenerateRequest
		if err := decode(body, &req); err != nil {
			return nil, err
		}
		return s.Generate(ctx, req)
	},
	pgpwire.MethodExportPublic: func(ctx context.Context, s *Service, body []byte) (any, error) {
		var req pgpwire.ExportPublicRequest
		if err := decode(body, &req); err != nil {
			return nil, err
		}
		return s.ExportPublic(ctx, req)
	},
	pgpwire.MethodImport: func(ctx context.Context, s *Service, body []byte) (any, error) {
		var req pgpwire.ImportRequest
		if err := decode(body, &req); err != nil {
			return nil, err
		}
		// The decoded request holds a private key. Zeroizing a Go string is not
		// possible, so this only shortens the window on the raw body — which is
		// still worth doing, because `body` is the copy most likely to end up
		// somewhere by accident.
		defer crypto.Zero(body)
		return s.Import(ctx, req)
	},
	pgpwire.MethodBindIdentity: func(ctx context.Context, s *Service, body []byte) (any, error) {
		var req pgpwire.BindIdentityRequest
		if err := decode(body, &req); err != nil {
			return nil, err
		}
		return s.BindIdentity(ctx, req)
	},
	pgpwire.MethodExportWrapped: func(ctx context.Context, s *Service, body []byte) (any, error) {
		var req pgpwire.ExportWrappedRequest
		if err := decode(body, &req); err != nil {
			return nil, err
		}
		return s.ExportWrapped(ctx, req)
	},
	pgpwire.MethodSign: func(ctx context.Context, s *Service, body []byte) (any, error) {
		var req pgpwire.SignRequest
		if err := decode(body, &req); err != nil {
			return nil, err
		}
		return s.Sign(ctx, req)
	},
	pgpwire.MethodDecrypt: func(ctx context.Context, s *Service, body []byte) (any, error) {
		var req pgpwire.DecryptRequest
		if err := decode(body, &req); err != nil {
			return nil, err
		}
		return s.Decrypt(ctx, req)
	},
	pgpwire.MethodEncrypt: func(ctx context.Context, s *Service, body []byte) (any, error) {
		var req pgpwire.EncryptRequest
		if err := decode(body, &req); err != nil {
			return nil, err
		}
		return s.Encrypt(ctx, req)
	},
	pgpwire.MethodExtend: func(ctx context.Context, s *Service, body []byte) (any, error) {
		var req pgpwire.ExtendRequest
		if err := decode(body, &req); err != nil {
			return nil, err
		}
		return s.Extend(ctx, req)
	},
	pgpwire.MethodRevoke: func(ctx context.Context, s *Service, body []byte) (any, error) {
		var req pgpwire.RevokeRequest
		if err := decode(body, &req); err != nil {
			return nil, err
		}
		return s.Revoke(ctx, req)
	},
}

// NewHandler builds the daemon's HTTP router from the method registry.
func NewHandler(svc *Service, log *slog.Logger) (http.Handler, error) {
	if svc == nil {
		return nil, errors.New("pgpkeyd: nil service")
	}
	if log == nil {
		return nil, errors.New("pgpkeyd: nil logger")
	}
	if len(handlers) != len(pgpwire.Methods) {
		return nil, fmt.Errorf("pgpkeyd: %d handlers for %d declared methods", len(handlers), len(pgpwire.Methods))
	}

	mux := http.NewServeMux()
	// Unconditional, and that is correct only because of WHEN it can be reached:
	// cmd/mailroom-crypto verifies the PGP root before it listens (#525), so a
	// process able to answer this has already passed that check. Nothing here
	// re-checks it, because the root cannot change while the process runs.
	mux.HandleFunc("GET "+pgpwire.HealthPath, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = io.WriteString(w, "ok\n")
	})

	for _, m := range pgpwire.Methods {
		h, ok := handlers[m]
		if !ok {
			return nil, fmt.Errorf("pgpkeyd: method %q declared but not implemented", m)
		}
		mux.Handle("POST "+m.Path(), methodHandler(svc, log, m, h))
	}
	return mux, nil
}

func methodHandler(svc *Service, log *slog.Logger, m pgpwire.Method, h handlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Per-method limit: sign carries a whole message, everything else carries
		// identifiers. One global bound would have to be the larger of the two,
		// which would hand the import path — the one whose body is a private
		// key — an allowance it has no use for.
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, m.MaxBytes()))
		if err != nil {
			writeError(w, log, pgpwire.Errorf(pgpwire.CodeInvalidRequest, "request body unreadable"))
			return
		}
		result, err := h(r.Context(), svc, body)
		if err != nil {
			writeError(w, log, err)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(result); err != nil {
			log.Error("crypto process: encode response", "method", string(m), "err", err)
		}
	})
}

func decode(body []byte, into any) error {
	if len(body) == 0 {
		return pgpwire.Errorf(pgpwire.CodeInvalidRequest, "empty request body")
	}
	if err := json.Unmarshal(body, into); err != nil {
		return pgpwire.Errorf(pgpwire.CodeInvalidRequest, "malformed JSON")
	}
	return nil
}

func statusFor(code pgpwire.ErrorCode) int {
	switch code {
	case pgpwire.CodeInvalidRequest:
		return http.StatusBadRequest
	case pgpwire.CodeNotFound:
		return http.StatusNotFound
	case pgpwire.CodeDenied:
		return http.StatusForbidden
	case pgpwire.CodeDecryptFailed, pgpwire.CodeNoRecipientKey:
		return http.StatusUnprocessableEntity
	case pgpwire.CodeRateLimited:
		return http.StatusTooManyRequests
	default:
		return http.StatusInternalServerError
	}
}

func writeError(w http.ResponseWriter, log *slog.Logger, err error) {
	var werr *pgpwire.Error
	if !errors.As(err, &werr) {
		// An error that is not a wire error escaped a handler. Do not relay
		// its text: it is the one thing here that has never been reviewed for
		// what it might contain.
		log.Error("crypto process: unclassified error", "err", err)
		werr = pgpwire.Errorf(pgpwire.CodeInternal, "internal error")
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusFor(werr.Code))
	_ = json.NewEncoder(w).Encode(pgpwire.ErrorBody{Error: werr})
}

// Serve listens on the Unix socket and serves h until ctx is cancelled.
//
// The socket is the boundary's access control: it is created 0600 and owned by
// the process's own uid, so WHO MAY CONNECT is enforced by the kernel rather
// than by anything in this file. There is no network listener, and there is no
// configuration that could create one.
func Serve(ctx context.Context, socketPath string, h http.Handler, log *slog.Logger) error {
	if err := prepareSocketPath(socketPath); err != nil {
		return err
	}

	ln, err := net.Listen("unix", socketPath)
	if err != nil {
		return fmt.Errorf("pgpkeyd: listen %q: %w", socketPath, err)
	}
	// Listen honours the process umask, which is not ours to assume. Narrow it
	// explicitly, and fail rather than serve on a socket anyone could reach.
	if err := os.Chmod(socketPath, 0o600); err != nil {
		_ = ln.Close()
		return fmt.Errorf("pgpkeyd: chmod socket %q: %w", socketPath, err)
	}

	srv := &http.Server{
		Handler:           h,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
		ErrorLog:          nil,
	}

	done := make(chan error, 1)
	go func() { done <- srv.Serve(ln) }()

	log.Info("crypto process listening", "socket", socketPath)

	select {
	case <-ctx.Done():
		shutCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutCtx); err != nil {
			log.Warn("crypto process: shutdown", "err", err)
		}
		<-done
		return nil
	case err := <-done:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return fmt.Errorf("pgpkeyd: serve: %w", err)
	}
}

// prepareSocketPath creates the socket's directory and removes a stale socket
// left behind by an unclean exit. It refuses to unlink anything that is not a
// socket — a misconfigured path must not delete a file.
func prepareSocketPath(socketPath string) error {
	if socketPath == "" {
		return errors.New("pgpkeyd: empty socket path")
	}
	if err := os.MkdirAll(filepath.Dir(socketPath), 0o700); err != nil {
		return fmt.Errorf("pgpkeyd: create socket directory: %w", err)
	}
	info, err := os.Stat(socketPath)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil
	case err != nil:
		return fmt.Errorf("pgpkeyd: stat socket path %q: %w", socketPath, err)
	case info.Mode()&fs.ModeSocket == 0:
		return fmt.Errorf("pgpkeyd: %q exists and is not a socket", socketPath)
	}
	if err := os.Remove(socketPath); err != nil {
		return fmt.Errorf("pgpkeyd: remove stale socket %q: %w", socketPath, err)
	}
	return nil
}
