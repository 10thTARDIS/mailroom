// SPDX-License-Identifier: AGPL-3.0-only

package wkd

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"mailroom/internal/egress"
	"mailroom/internal/pgpkeys"
)

// newTestFetcher points a Fetcher at an httptest.NewTLSServer while still
// letting Fetch build its ordinary "https://openpgpkey.<domain>/..." and
// "https://<domain>/..." URLs: the transport's DialTLSContext ignores the
// address it was asked to dial and always connects to the test server's real
// listener, so the request's Host/SNI can be anything (openpgpkey.example.com,
// example.com) while the bytes actually go to srv. Certificate hostname
// verification is switched off because that is net/http's job (and egress's,
// tested in internal/egress) — this suite is about Fetch's own fallback and
// size-cap logic, not TLS validation.
func newTestFetcher(srv *httptest.Server) *Fetcher {
	dialer := &tls.Dialer{Config: &tls.Config{InsecureSkipVerify: true}} //nolint:gosec // test double, see comment above
	transport := &http.Transport{
		DialTLSContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return dialer.DialContext(ctx, network, srv.Listener.Addr().String())
		},
	}
	client := &http.Client{Transport: transport, Timeout: 5 * time.Second}
	return newFetcherWithDoer(client, slog.Default())
}

// pathRecorder is a small thread-safe log of request paths, since the
// httptest server runs each request's handler on its own goroutine.
type pathRecorder struct {
	mu    sync.Mutex
	paths []string
}

func (r *pathRecorder) record(p string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.paths = append(r.paths, p)
}

func (r *pathRecorder) snapshot() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, len(r.paths))
	copy(out, r.paths)
	return out
}

// 1. The advanced method is tried first, and a hit there is the whole story:
// no direct request happens at all.
func TestFetchAdvancedHit(t *testing.T) {
	const addr, localPart, domain = "alice@example.com", "alice", "example.com"
	keyBytes := []byte("fake-advanced-key-bytes")
	advPath := AdvancedPath(localPart, domain)

	rec := &pathRecorder{}
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.record(r.URL.Path)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(keyBytes)
	}))
	defer srv.Close()

	f := newTestFetcher(srv)
	body, gotURL, err := f.Fetch(context.Background(), addr)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if !bytes.Equal(body, keyBytes) {
		t.Errorf("body = %q, want %q", body, keyBytes)
	}
	wantURL := "https://openpgpkey." + domain + advPath + "?l=" + localPart
	if gotURL != wantURL {
		t.Errorf("url = %q, want %q", gotURL, wantURL)
	}
	if paths := rec.snapshot(); len(paths) != 1 || paths[0] != advPath {
		t.Fatalf("requests = %v, want exactly [%s] (no direct request on an advanced hit)", paths, advPath)
	}
}

// 2. Advanced 404s, so direct is tried next — and BOTH paths must have been
// requested, in that order, not just "a" hit somewhere.
func TestFetchAdvancedNotFoundFallsBackToDirect(t *testing.T) {
	const addr, localPart, domain = "alice@example.com", "alice", "example.com"
	keyBytes := []byte("fake-direct-key-bytes")
	advPath := AdvancedPath(localPart, domain)
	dirPath := DirectPath(localPart)

	rec := &pathRecorder{}
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.record(r.URL.Path)
		switch r.URL.Path {
		case advPath:
			w.WriteHeader(http.StatusNotFound)
		case dirPath:
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(keyBytes)
		default:
			t.Errorf("unexpected request path %q", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	f := newTestFetcher(srv)
	body, gotURL, err := f.Fetch(context.Background(), addr)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if !bytes.Equal(body, keyBytes) {
		t.Errorf("body = %q, want %q", body, keyBytes)
	}
	wantURL := "https://" + domain + dirPath + "?l=" + localPart
	if gotURL != wantURL {
		t.Errorf("url = %q, want %q", gotURL, wantURL)
	}
	if paths := rec.snapshot(); len(paths) != 2 || paths[0] != advPath || paths[1] != dirPath {
		t.Fatalf("request order = %v, want [%s %s] (advanced tried first, then direct)", paths, advPath, dirPath)
	}
}

// 3. Both methods 404 → the sentinel ErrNoKey, not a generic failure, so a
// caller can count "no key published" separately from every other outcome.
func TestFetchBothNotFoundReturnsErrNoKey(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	f := newTestFetcher(srv)
	key, gotURL, err := f.Fetch(context.Background(), "alice@example.com")
	if !errors.Is(err, ErrNoKey) {
		t.Fatalf("err = %v, want ErrNoKey", err)
	}
	if key != nil || gotURL != "" {
		t.Errorf("got (key=%v, url=%q) alongside ErrNoKey, want both zero", key, gotURL)
	}
}

// 4. An oversize body is an ERROR, never a truncated "success". This is also
// the case that proves a resolving-but-wrong advanced host does NOT fall
// through to direct (wkd.go's package doc): the response was definitive, just
// unusable, so only one request should ever be made.
func TestFetchOversizeBodyIsAnErrorNotATruncatedKey(t *testing.T) {
	big := bytes.Repeat([]byte("A"), pgpkeys.MaxKeyBytes+1)
	advPath := AdvancedPath("alice", "example.com")

	rec := &pathRecorder{}
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.record(r.URL.Path)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(big)
	}))
	defer srv.Close()

	f := newTestFetcher(srv)
	key, gotURL, err := f.Fetch(context.Background(), "alice@example.com")
	if err == nil {
		t.Fatal("expected an error for an oversize body, got nil")
	}
	if key != nil {
		t.Fatalf("got a non-nil %d-byte key alongside the error — an oversize body must not be "+
			"silently truncated into a partial success", len(key))
	}
	if gotURL != "" {
		t.Errorf("got url = %q alongside an error, want empty", gotURL)
	}
	if errors.Is(err, ErrNoKey) {
		t.Fatalf("oversize body must not be reported as ErrNoKey (that is the 404 case): %v", err)
	}
	if paths := rec.snapshot(); len(paths) != 1 || paths[0] != advPath {
		t.Fatalf("requests = %v, want exactly [%s] — an oversize (but definitive) advanced "+
			"response must not fall through to direct", paths, advPath)
	}
}

// 5. THE CONTROL: an http:// URL is refused outright. This is what proves the
// HTTPS-only rule is enforced code, not just something Fetch happens to
// produce by construction — fetchOne is handed one directly, with a doer that
// fails the test if it is ever invoked, so a regression that let the request
// through would be caught even though Fetch itself never builds an http: URL.
func TestFetchOneRefusesHTTP(t *testing.T) {
	f := newFetcherWithDoer(doerFunc(func(*http.Request) (*http.Response, error) {
		t.Fatal("fetchOne must refuse an http:// URL before ever calling the doer")
		return nil, nil
	}), slog.Default())

	_, err := f.fetchOne(context.Background(), "http://example.com/.well-known/openpgpkey/hu/abcd")
	if err == nil {
		t.Fatal("expected an http:// URL to be refused")
	}
}

// doerFunc adapts a function to the doer interface, for tests that need a
// stub without a full http.Client.
type doerFunc func(*http.Request) (*http.Response, error)

func (f doerFunc) Do(req *http.Request) (*http.Response, error) { return f(req) }

// TestNewFetcherUsesEgressByDefault is the assertion that the PRODUCTION
// constructor cannot be pointed anywhere except through the SSRF guard. If
// this ever fails, something added a way to construct a Fetcher whose
// requests skip internal/egress — exactly the mistake the whole package
// comment warns against.
func TestNewFetcherUsesEgressByDefault(t *testing.T) {
	f := NewFetcher(egress.New(egress.Policy{}), slog.Default())
	if _, ok := f.do.(*egress.Client); !ok {
		t.Fatalf("NewFetcher's transport is %T, want *egress.Client — production must always go "+
			"through the SSRF guard", f.do)
	}
}
