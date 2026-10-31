// SPDX-License-Identifier: AGPL-3.0-only

package wkd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"time"

	"mailroom/internal/egress"
	"mailroom/internal/pgpkeys"
)

// This file is the FETCHING side of WKD (#48): looking up someone ELSE's key.
// wkd.go is the serving side (our own address's path/hash mapping) and stays
// pure — no network calls, nothing to guard. This file is the opposite: every
// URL it builds is derived from an email address, i.e. chosen by whoever wrote
// to the user, which is exactly the SSRF shape internal/egress exists for. See
// the package doc's "What is deliberately not here" note, now filled in.

// ErrNoKey means both the advanced and direct methods answered 404 — the
// ORDINARY "this address has not published a WKD key" outcome, not a failure.
// Callers count it separately from a fetch failure so a report can tell "nobody
// publishes a key" apart from "the fetcher never fires" (CLAUDE.md, #155/#176).
var ErrNoKey = errors.New("wkd: no key published at either WKD location")

// fetchTimeout bounds one whole lookup (both the advanced and direct attempt
// share the parent context; each individual HTTP round trip gets this much).
// A WKD host is a stranger's server chosen by whoever wrote to the user, so a
// slow or hanging one must not stall a discovery sweep over many contacts.
const fetchTimeout = 10 * time.Second

// doer is the minimal seam Fetch needs. *egress.Client satisfies it, and that
// is the ONLY production implementation — see NewFetcher. Tests substitute a
// fake that never leaves the process, because egress correctly refuses to
// dial the loopback address httptest.NewTLSServer listens on.
type doer interface {
	Do(req *http.Request) (*http.Response, error)
}

// Fetcher looks up a correspondent's key over WKD.
type Fetcher struct {
	do  doer
	log *slog.Logger
}

// NewFetcher builds a Fetcher that fetches EXCLUSIVELY through client, the
// SSRF-guarded egress client (#55, ADR referenced there). There is no
// constructor that accepts a bare http.RoundTripper or *http.Client for
// production use, and that is deliberate: a WKD host name comes from an
// address an attacker can choose, so there is no "trusted" caller who gets to
// skip the guard.
func NewFetcher(client *egress.Client, log *slog.Logger) *Fetcher {
	if log == nil {
		log = slog.Default()
	}
	return &Fetcher{do: client, log: log}
}

// newFetcherWithDoer is the TEST-ONLY seam. It exists so lookup_test.go can
// point Fetch at an httptest.NewTLSServer — a loopback address egress
// CORRECTLY refuses to dial — without weakening NewFetcher. It is unexported:
// nothing outside this package's own tests can reach it, so there is no flag
// or code path by which production can bypass the guard.
func newFetcherWithDoer(do doer, log *slog.Logger) *Fetcher {
	if log == nil {
		log = slog.Default()
	}
	return &Fetcher{do: do, log: log}
}

// fetchTransportError marks a failure that never got a definitive HTTP
// response at all — DNS, TLS, connection refused, timeout, or the egress
// guard itself refusing the address. Fetch uses this distinction (via
// errors.As) to decide whether the advanced method's failure should fall
// through to the direct method: a host that could not be reached tells us
// nothing about whether a key exists there, so trying direct is reasonable.
// A host that WAS reached and gave a real (if wrong) answer is a different,
// more informative case — see the fall-through comment in Fetch.
type fetchTransportError struct {
	url string
	err error
}

func (e *fetchTransportError) Error() string {
	return fmt.Sprintf("wkd: %s: %v", e.url, e.err)
}

func (e *fetchTransportError) Unwrap() error { return e.err }

// Fetch looks up addr's key over WKD, advanced method first, per
// draft-koch-openpgp-webkey-service revision DraftRevision (wkd.go). It
// returns the RAW key bytes exactly as served — parsing is
// internal/pgpkeys.Parse's job, deliberately not done here, so this stays a
// transport and its own tests need no key material.
//
// url is the location that answered, for the caller's report.
func (f *Fetcher) Fetch(ctx context.Context, addr string) (key []byte, sourceURL string, err error) {
	localPart, domain, err := SplitAddress(addr)
	if err != nil {
		return nil, "", fmt.Errorf("wkd: %w", err)
	}

	query := "?l=" + url.QueryEscape(localPart)
	advancedURL := "https://openpgpkey." + domain + AdvancedPath(localPart, domain) + query
	directURL := "https://" + domain + DirectPath(localPart) + query

	body, aerr := f.fetchOne(ctx, advancedURL)
	if aerr == nil {
		return body, advancedURL, nil
	}

	var terr *fetchTransportError
	if !errors.Is(aerr, ErrNoKey) && !errors.As(aerr, &terr) {
		// ⚠ NO FALL-THROUGH HERE. The advanced host was REACHED and gave a
		// definitive but unusable answer (an unexpected status, or a body over
		// the size cap). wkd.go's package doc already names this trap: a
		// half-configured openpgpkey subdomain that resolves but serves the
		// wrong thing "breaks ALL lookups, including ones the direct method
		// would have answered" — because falling through here would silently
		// paper over exactly that misconfiguration instead of surfacing it.
		return nil, "", aerr
	}
	f.log.DebugContext(ctx, "wkd: advanced method failed, trying direct",
		"addr", addr, "err", aerr)

	body, derr := f.fetchOne(ctx, directURL)
	if derr != nil {
		return nil, "", derr
	}
	return body, directURL, nil
}

// fetchOne performs one GET, capped and guarded.
func (f *Fetcher) fetchOne(ctx context.Context, rawURL string) ([]byte, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("wkd: %q: not a valid URL: %w", rawURL, err)
	}
	// ⚠ HTTPS ONLY, checked here even though Fetch only ever constructs
	// https:// URLs above. This is the belt to that suspenders: it is the
	// control that proves the rule is enforced rather than merely intended by
	// construction, and it is what lookup_test.go exercises directly.
	if u.Scheme != "https" {
		return nil, fmt.Errorf("wkd: refusing non-https URL %s", rawURL)
	}

	reqCtx, cancel := context.WithTimeout(ctx, fetchTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, fmt.Errorf("wkd: %s: building request: %w", rawURL, err)
	}
	req.Header.Set("User-Agent", "Mailroom-WKD/1")
	req.Header.Set("Accept", "application/octet-stream")

	resp, err := f.do.Do(req)
	if err != nil {
		return nil, &fetchTransportError{url: rawURL, err: err}
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == http.StatusNotFound {
		return nil, fmt.Errorf("%w: %s", ErrNoKey, rawURL)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("wkd: %s: unexpected HTTP status %s", rawURL, resp.Status)
	}

	// ⚠ CAPPED AT MaxKeyBytes+1, and a body that reaches the cap is an ERROR,
	// not a truncated success. Returning the first MaxKeyBytes of an oversize
	// body would hand pgpkeys.Parse a byte stream that is not the key at all —
	// a truncated OpenPGP packet stream can still parse into SOMETHING, which
	// would be worse than refusing outright.
	limited := io.LimitReader(resp.Body, int64(pgpkeys.MaxKeyBytes)+1)
	body, err := io.ReadAll(limited)
	if err != nil {
		return nil, fmt.Errorf("wkd: %s: reading response body: %w", rawURL, err)
	}
	if int64(len(body)) > int64(pgpkeys.MaxKeyBytes) {
		return nil, fmt.Errorf("wkd: %s: response body exceeds the %d byte cap", rawURL, pgpkeys.MaxKeyBytes)
	}
	return body, nil
}
