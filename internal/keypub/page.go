// SPDX-License-Identifier: AGPL-3.0-only

package keypub

import (
	"bytes"
	"context"
	"html/template"
	"net/http"
	"strings"

	"mailroom/internal/wkd"
)

// PageLookup answers the question the key page needs that Lookup does not:
// WHOSE key this is. WKD returns bytes to a machine; the page shows an address
// and a fingerprint to a person who is checking them against another channel.
//
// It is a second interface rather than a widening of Lookup so that the WKD
// path — the one a stranger reaches most often — asks for strictly less.
// keypub.New requires an implementation to satisfy BOTH, so a lookup that
// cannot serve pages is a startup failure rather than a page that 404s at
// request time and looks exactly like an unpublished key.
type PageLookup interface {
	LookupWKDKeyPage(ctx context.Context, domain, hash string) (address, fingerprint string, found bool, err error)
}

// pageTemplate is parsed once at package init and PANICS on a syntax error —
// a template that fails to parse is a build-time bug, not something that
// should surface as a 500 to whoever visits first.
var pageTemplate = template.Must(template.New("keypage").Parse(pageHTML))

// pageData is everything the template is allowed to see. Every field reaches
// HTML through html/template, so escaping is structural rather than
// remembered: there is no path from an address or fingerprint fetched out of
// the database straight into a byte stream, the way text/template or string
// concatenation would allow.
type pageData struct {
	Address     string
	Fingerprint string
	AdvancedURL string
	DirectURL   string
	DownloadURL string
}

// pageHTML has NO external resource of any kind — no CDN, no web font, no
// remote image, no XHR — and no JavaScript. It is served on the same origin
// as the mail client to callers who supply no session at all, so anything it
// fetched or executed would be a request a stranger got to shape. Inline CSS
// is the only styling mechanism, deliberately.
const pageHTML = `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<title>OpenPGP key — {{.Address}}</title>
<style>
  body{font-family:system-ui,sans-serif;max-width:38em;margin:2rem auto;padding:0 1rem;color:#1a1a1a;line-height:1.5}
  h1{font-size:1.15rem}
  .fingerprint{font-family:ui-monospace,Consolas,"Courier New",monospace;font-size:1.05rem;letter-spacing:.03em;background:#f2f2f2;padding:.6rem .8rem;border-radius:.3rem;word-break:break-all}
  .hint{color:#444}
  .urls{font-family:ui-monospace,Consolas,"Courier New",monospace;font-size:.85rem;word-break:break-all;padding-left:1.2rem}
  a.download{display:inline-block;margin-top:1rem}
</style>
</head>
<body>
<h1>OpenPGP key for {{.Address}}</h1>
<p class="fingerprint">{{.Fingerprint}}</p>
<p class="hint">A fingerprint is what you check over a different channel — a phone call, a chat you already trust — to confirm this key really belongs to {{.Address}} before you rely on it.</p>
<p>A correspondent's mail client will look for this key at, in order:</p>
<ul class="urls">
<li>{{.AdvancedURL}}</li>
<li>{{.DirectURL}}</li>
</ul>
<p><a class="download" href="{{.DownloadURL}}">Download the key</a></p>
</body>
</html>
`

// PageHandler serves /key/{hash} — a shareable, human-readable page for a
// PUBLISHED key, addressed by the same z-base-32 hash WKD uses so one
// identifier answers both the machine lookup and the page a person actually
// reads.
//
// ⚠ This handler sets ONLY Content-Type. CSP, X-Content-Type-Options,
// X-Frame-Options and Cache-Control belong to the route gate that mounts this
// handler — setting any of them here would either duplicate or silently
// contradict whatever the gate decides, and the gate is where every route's
// security headers are reviewed together. Do not "helpfully" add them back.
func (s *Service) PageHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.Enabled() {
			http.NotFound(w, r)
			return
		}

		hash := strings.TrimPrefix(r.URL.Path, "/key/")
		if !validHash(hash) {
			// Refused before the database is touched, exactly like the WKD
			// endpoint: a malformed path is the cheapest thing an anonymous
			// caller can send, and it stays cheap.
			http.NotFound(w, r)
			return
		}
		if s.throttled(w, r) {
			return
		}

		pl, ok := s.lookup.(PageLookup)
		if !ok {
			// The lookup this Service was built with does not answer the page's
			// question. That is a wiring bug, not "no key here" — logged loudly,
			// but the response to the anonymous caller stays the same generic
			// 404 either way, so it carries no information about which is true.
			s.log.Error("keypub: lookup does not implement PageLookup; key page cannot render")
			http.NotFound(w, r)
			return
		}

		address, fingerprint, found, err := pl.LookupWKDKeyPage(r.Context(), s.domain, hash)
		if err != nil {
			// Do not relay the error: this response goes to anyone.
			s.log.Error("keypub: key page lookup failed", "err", err)
			http.Error(w, "lookup failed", http.StatusInternalServerError)
			return
		}
		if !found {
			// Byte-identical to the WKD miss: an unpublished address and a
			// nonexistent one must stay indistinguishable here too, or this page
			// becomes the enumeration oracle the WKD handler is careful to avoid.
			http.NotFound(w, r)
			return
		}

		localPart, _, err := wkd.SplitAddress(address)
		if err != nil {
			// A published row with an address that doesn't parse is a data bug
			// upstream, not something to explain to an anonymous caller.
			s.log.Error("keypub: published address does not parse", "err", err)
			http.Error(w, "lookup failed", http.StatusInternalServerError)
			return
		}

		directPath := wkd.DirectPath(localPart)
		data := pageData{
			Address:     address,
			Fingerprint: groupFingerprint(fingerprint),
			AdvancedURL: "https://openpgpkey." + s.domain + wkd.AdvancedPath(localPart, s.domain),
			DirectURL:   "https://" + s.domain + directPath,
			// The download link IS the WKD path, not a page-only route: there is
			// exactly one way to get the bytes, and it is the one every real
			// client already uses. A relative path also keeps this correct
			// regardless of what host actually served this page.
			DownloadURL: directPath,
		}

		// Render fully into a buffer before touching the response, so a
		// template failure can never ship a half-written page under a 200
		// that has already been committed to the wire.
		var buf bytes.Buffer
		if err := pageTemplate.Execute(&buf, data); err != nil {
			s.log.Error("keypub: key page render failed", "err", err)
			http.Error(w, "render failed", http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		if _, err := w.Write(buf.Bytes()); err != nil {
			s.log.Warn("keypub: write key page response", "err", err)
		}
	})
}

// groupFingerprint renders a hex fingerprint the way `gpg --fingerprint`
// does: 4-character groups, with an extra space at the midpoint, so someone
// reading it aloud has a natural place to pause and whoever is checking on
// the other end has a natural place to confirm they're still in sync. The
// standard OpenPGP v4 fingerprint is 40 hex characters (10 groups); anything
// else still groups by four, just without the extra gap, since there is no
// well-defined middle for it.
func groupFingerprint(fpr string) string {
	var groups []string
	for i := 0; i < len(fpr); i += 4 {
		end := i + 4
		if end > len(fpr) {
			end = len(fpr)
		}
		groups = append(groups, fpr[i:end])
	}
	if len(groups) == 0 || len(groups)%2 != 0 {
		return strings.Join(groups, " ")
	}
	mid := len(groups) / 2
	return strings.Join(groups[:mid], " ") + "  " + strings.Join(groups[mid:], " ")
}
