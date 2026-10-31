// SPDX-License-Identifier: AGPL-3.0-only

package api

import (
	"strings"

	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

// SERVER-SIDE HOSTILE-HTML SANITIZATION (spec §11, Forgejo #39).
//
// Every HTML message body is attacker-controlled input, authored by a stranger,
// and it is rendered inside the operator's own origin next to their mail. The
// full defence is documented in docs/hostile-content.md; this file is ONE of its
// two independent layers, and the layers are deliberately not the same kind of
// thing:
//
//	LAYER 1 (client, primary): web/ renders an HTML body in a sandboxed iframe
//	  (opaque origin, no scripts) whose document carries its own restrictive CSP.
//	  It is a bet on the browser's origin isolation, which is the strongest bet
//	  available. It is NOT superseded by this file.
//
//	LAYER 2 (server, this file): the bytes a client receives for htmlBody have
//	  already had the dangerous constructs REMOVED. This exists because layer 1
//	  is a property of ONE client, and because layer 1 has holes a CSP cannot
//	  close:
//
//	    * <meta http-equiv="refresh" content="0;url=https://tracker/…"> —
//	      a sandboxed iframe may navigate ITSELF (sandbox gates only TOP
//	      navigation), and no CSP directive governs a document navigating itself
//	      (`navigate-to` was dropped from CSP3 and never shipped). A meta refresh
//	      in a message body is therefore a working tracking beacon AND a working
//	      phishing redirect through the strictest CSP we can write.
//	    * <link rel="dns-prefetch"> / rel="preconnect"> — these perform DNS
//	      resolution and a TCP+TLS connect. They fetch no resource, so no CSP
//	      directive applies to them at all. A per-recipient hostname
//	      (<token>.tracker.example) makes DNS itself the read receipt.
//
//	  Neither is hypothetical, and neither is stopped by `default-src 'none'`.
//	  A server-side sanitizer is the only place they can be removed.
//
// It also means a NON-BROWSER client (aerc over JMAP, the future MCP surface, an
// export) is not the one thing standing between a phishing email and its user.
//
// ── WHY THIS IS A PARSE-FILTER-RESERIALIZE, NOT A REGEX ──────────────────────
//
// The classic way to lose is to pattern-match on a string: the browser's parser
// and the sanitizer's parser then disagree about what the bytes MEAN, and the
// gap between them is mutation XSS. So:
//
//  1. Parse with golang.org/x/net/html — the same HTML5 tree-construction
//     algorithm browsers implement.
//  2. Filter the TREE against an allowlist. An attribute that is not on the list
//     does not exist; an element that is not on the list does not exist.
//  3. Re-serialize with html.Render, which escapes text and attribute values.
//
// What we emit is therefore, by construction, a rendering of a tree we fully
// inspected — not a substring of the attacker's bytes.
//
// SCRIPTING IS PARSED AS DISABLED (html.ParseOptionEnableScripting(false)), and
// that is load-bearing rather than cosmetic. The HTML5 parser has TWO modes, and
// they disagree about <noscript>: with scripting ENABLED its contents are raw
// text; with scripting DISABLED they are markup. Our sandboxed iframe has no
// scripting, so the BROWSER parses <noscript> contents as markup — and a
// sanitizer that parsed them as text would have waved a live <img onerror=…>
// straight through as an inert string. We parse in the same mode the renderer
// uses, and then unwrap <noscript> entirely so the question cannot be asked
// again by a client that renders with scripting on.
//
// ── REMOTE CONTENT ───────────────────────────────────────────────────────────
//
// Blocking remote content is the default (spec §11, feature matrix §2/§8).
// Every attribute that would make a renderer fetch from the network is
// REMOVED. A remote <img> src/srcset is preserved as data-mailroom-remote-src /
// -srcset and the element is marked data-mailroom-blocked, which is what gives
// the web client its "Show remote content" affordance something to restore.
// With the image proxy on (the default, ADR-026, MAILROOM_IMAGE_PROXY) the
// preserved src is this server's own /jmap/proxy/ URL (px below), so restoring
// an image fetches it through the SSRF-guarded proxy and the client never
// learns the sender's URL. Two exceptions, both below: a tracking beacon is
// never rewritten (it is parked under a name nothing restores), and srcset is
// dropped rather than parked. With the proxy off the preserved src is the
// sender's URL and the browser fetches it directly.
//
// Everything else that fetches — CSS url() and @import, <link>, <style
// src>, background=, poster=, <video>/<audio>/<source>, <object>/<embed> — is
// removed outright and is NOT restorable. Those are the channels whose only use
// in mail is tracking; a decorative CSS background is not worth an origin.
//
// "Removed outright" is worth one more sentence, because for CSS it was not true
// until Forgejo #297: a url() spelled with a CSS escape (`\75 rl(…)`) walked past
// the regex that was supposed to remove it and reached the client live. Worse
// than a surviving tracker, an escaped-CSS-only body carries no
// data-mailroom-blocked marker either, so the client shows NEITHER banner — the
// user is not told anything was blocked and is not told anything was loaded. The
// CSS filter is an allowlist of properties now (sanitize_css.go); "removed
// outright" is a fact about a token stream rather than about a pattern.

// remoteSrcAttr / remoteSrcsetAttr carry a blocked remote image URL through to
// the client, which may restore it into src/srcset if — and only if — the user
// explicitly asks for remote content. blockedAttr marks the element so the
// client can say "this message tried to load remote content" without re-parsing.
// pixelSrcAttr / pixelSrcsetAttr park a TRACKING PIXEL's URL — an image whose
// declared size means it is a beacon rather than a picture (#55 increment 2).
//
// ⚠ THE SEPARATE NAME IS THE WHOLE MECHANISM. The client restores remote content
// by replacing the two `remote` attribute names with `src`/`srcset`; it has no
// list of exceptions and needs none, because a beacon is never given a name it
// restores. Suppression is therefore structural: there is no code path, and no
// bug in the client, that can un-park one. Marking beacons with an extra
// attribute instead would have required the client to parse the DOM and decide,
// which is both more code and more ways to be wrong.
//
// The SERVER decides what IS a beacon (a fact about the image), and does not
// decide whether to load it — sanitize.go never branches on a preference, which
// is what keeps it the only path. What the client does with a permanently parked
// URL is the client's business; today it counts them and says so.
const (
	remoteSrcAttr    = "data-mailroom-remote-src"
	remoteSrcsetAttr = "data-mailroom-remote-srcset"
	blockedAttr      = "data-mailroom-blocked"
	pixelSrcAttr     = "data-mailroom-pixel-src"
	pixelSrcsetAttr  = "data-mailroom-pixel-srcset"
)

// SanitizeHTML returns a safe rendering of one HTML message body on the READ
// path — a body authored by a stranger and rendered next to the user's mail.
//
// It is total: hostile, malformed and truncated input all produce a (possibly
// empty) safe document rather than an error, because a message body that cannot
// be sanitized must still be a message the user can open.
func SanitizeHTML(in string) string {
	return sanitizeFragment(in, false, nil, nil)
}

// SanitizeOutboundHTML sanitizes user-authored HTML on the SEND path — the rich
// body a client composes, before it becomes the text/html part of the RFC 5322
// bytes Mailroom transmits and files (internal/compose/build.go writeBody). It
// is called from the API trust boundary (internal/api/draft.go) rather than in
// the compose package, because the compose package is imported BY this one and a
// reverse import would be a cycle; draft.go is the sole path that fills
// compose.DraftRequest.HTML, so every outbound HTML body passes through here.
//
// WHY SANITIZE OUR OWN USER'S HTML. The rich-text editor is a UI convenience,
// not a security control: a client is free to send any bytes it likes to
// Email/set, and "the editor would never produce a <script>" is an assumption
// about ONE client that the server must not make. What we transmit is also what
// we FILE in Sent and re-render on the read path, and mail we originate should
// carry no active or fetching content we would strip from mail we receive.
//
// HOW IT DIFFERS FROM THE READ PATH. Same parse-filter-reserialize machinery,
// same element/attribute allowlists, same event-handler and CSS stripping — but
// two behaviours are the read path's rendering policy, not a property of the
// bytes, and are turned off for outbound mail (see sanitizeAttrs):
//
//   - A remote <img> src is DROPPED, not parked in data-mailroom-remote-src and
//     marked data-mailroom-blocked. Those are read-path affordances for the web
//     client's "show remote content" button; they are meaningless — and leaky —
//     in sent mail. The compose editor only ever inserts INLINE images (cid: /
//     data:), which ARE preserved and are what make an embedded image render for
//     the recipient. A remote image only ever arrives by pasted markup, and not
//     embedding it means mail we send carries no silent third-party fetch (a
//     tracking beacon aimed at the recipient). Every surviving <img> src is thus
//     self-contained, the same strong property the read path guarantees.
//   - Anchors are NOT rewritten with target="_blank" rel="noopener…": that is
//     for links opened inside OUR sandboxed message iframe, not something to bake
//     into a message other people read. The javascript:/scheme validation on
//     href still applies.
func SanitizeOutboundHTML(in string) string {
	return sanitizeFragment(in, true, nil, nil)
}

// sanitizeFragment is the shared implementation. outbound selects the send-path
// policy described on SanitizeOutboundHTML; false is the read-path default.
func sanitizeFragment(in string, outbound bool, st *cssStats, px proxyURLFn) string {
	// The body is parsed as a FRAGMENT in body context. A message body is not a
	// document — a <html>/<head> wrapper in a mail part is noise — and fragment
	// parsing keeps us from inventing structure the sender did not write.
	nodes, err := html.ParseFragmentWithOptions(
		strings.NewReader(in),
		&html.Node{Type: html.ElementNode, Data: "body", DataAtom: atom.Body},
		html.ParseOptionEnableScripting(false),
	)
	if err != nil {
		// x/net/html only errors on a malformed *context* node, which is a
		// constant above, so this is unreachable. Fail closed anyway: an
		// unparseable body renders as nothing rather than as itself.
		return ""
	}

	var b strings.Builder
	for _, n := range nodes {
		sanitizeNode(n, &b, outbound, st, px)
	}
	return b.String()
}

// sanitizeNode filters one node and writes its safe rendering to b. outbound
// selects the send-path attribute policy (see SanitizeOutboundHTML).
func sanitizeNode(n *html.Node, b *strings.Builder, outbound bool, st *cssStats, px proxyURLFn) {
	switch n.Type {
	case html.TextNode:
		// html.EscapeString is what makes text inert: an attacker's "<script>"
		// arrives here as a TEXT node (the parser already decided that) and
		// leaves as "&lt;script&gt;".
		b.WriteString(html.EscapeString(n.Data))
		return
	case html.ElementNode:
		// fall through
	default:
		// Comments, doctypes, and the document node itself carry nothing a
		// message body needs. A comment in particular is a classic mXSS carrier
		// (conditional comments, "-->" tricks) and there is no reason to keep one.
		return
	}

	name := strings.ToLower(n.Data)

	// <style> is the one element whose children are RAW TEXT and that we still
	// keep, so it is decided first and by its own function.
	if name == "style" {
		writeStyleElement(n, b, st)
		return
	}

	if droppedSubtrees[name] {
		// Measurement only (#674): <video>/<audio> are dropped whole before their
		// children are visited, so a <source> reached here is a <picture> one or
		// a stray, and a remote src or first srcset candidate on it never
		// becomes a parked image or reaches the proxy. Counted once per element.
		// ⚠ GUARDED HERE, not below — the render path passes a nil *cssStats
		// (see the srcset branch in sanitizeAttrs).
		if st != nil && name == "source" {
			remote := false
			for _, a := range n.Attr {
				switch strings.ToLower(a.Key) {
				case "src":
					if classifyImageSrc(a.Val) == srcRemote {
						remote = true
					}
				case "srcset":
					if classifyImageSrc(firstSrcsetURL(a.Val)) == srcRemote {
						remote = true
					}
				}
				if remote {
					break
				}
			}
			if remote {
				st.Silent.PictureSource++
			}
		}
		// The element AND its children are gone. This is the set whose CONTENT is
		// dangerous or is parsed in a foreign/raw-text mode: script, the SVG and
		// MathML foreign-content trees (a rich source of parser confusion and of
		// <svg><script>), and the framing/plugin elements.
		return
	}

	if !allowedElements[name] {
		// UNWRAP: the element disappears, its children are kept and sanitized.
		// A <form> or an unknown <x-widget> carries no meaning we honor, but the
		// TEXT inside it is the message the user is trying to read.
		sanitizeChildren(n, b, outbound, st, px)
		return
	}

	attrs := sanitizeAttrs(name, n.Attr, outbound, st, px)

	b.WriteByte('<')
	b.WriteString(name)
	for _, a := range attrs {
		b.WriteByte(' ')
		b.WriteString(a.Key)
		b.WriteString(`="`)
		b.WriteString(html.EscapeString(a.Val))
		b.WriteByte('"')
	}
	b.WriteByte('>')

	if voidElements[name] {
		return
	}
	sanitizeChildren(n, b, outbound, st, px)
	b.WriteString("</")
	b.WriteString(name)
	b.WriteByte('>')
}

func sanitizeChildren(n *html.Node, b *strings.Builder, outbound bool, st *cssStats, px proxyURLFn) {
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		sanitizeNode(c, b, outbound, st, px)
	}
}

// writeStyleElement emits a <style> block whose CSS has been filtered.
//
// <style> is kept because real mail uses it (media queries, class rules) and a
// client that drops it renders a lot of legitimate mail as a wall of unstyled
// text. It is the one element whose children html.Render would emit as RAW text,
// so it gets two guards rather than one:
//
//   - the CSS is filtered against a property allowlist (sanitize_css.go), so no
//     surviving declaration can name an external resource or a script hook;
//   - if a '<' survives that filter, the whole element is dropped. There is no
//     CSS we care about enough to risk a "</style>" escape.
//
// ⚠ THE SECOND GUARD REFUSES '<' ONLY, AS OF #324. It used to refuse '<' or '>',
// which forced cssPreludeDelims to exclude '>' upstream — because one child
// combinator reaching the output would have cost the entire stylesheet. That
// upstream exclusion was measured on a corpus of real messages as thousands of rules and many emptied stylesheets
// (#305), so the blunt guard was not free; it was merely paid for somewhere the
// original trade could not see.
//
// A <style> element is RAW TEXT. Nothing terminates it but '</style' — a bare
// '>' is literal text there and always was. So refusing '<' is both necessary and
// sufficient, and it lets the child combinator through.
//
// The guard should still be unreachable, and now for a reason that is written
// down and tested rather than asserted: cssDelim is the only token kind
// cssWriteTokens emits as a raw rune, neither delim allowlist contains '<', and
// every other path (identifiers, hash names, strings, numbers, function names)
// escapes it or draws it from our own tables. See cssPreludeDelims for the full
// enumeration, TestOutputCannotContainALessThan for the check, and
// hostile-content-e2e.sh for the browser-verified breakout attempt.
//
// It stays because "should be unreachable" is a claim about today's code, and
// this is the one element where being wrong about it costs an origin. The
// corpus report counts every time it fires; a non-zero there is a finding.
func writeStyleElement(n *html.Node, b *strings.Builder, st *cssStats) {
	var raw strings.Builder
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		if c.Type == html.TextNode {
			raw.WriteString(c.Data)
		}
	}
	before := st.markRuleDrops()
	css := sanitizeStylesheetStats(raw.String(), st)
	// '<' only: '</style' is the sole terminator of a raw-text element, and a
	// bare '>' is literal text there (#324).
	markupGuard := css != "" && strings.ContainsRune(css, '<')
	st.styleElement(css == "", markupGuard, before)
	if css == "" || markupGuard {
		return
	}
	b.WriteString("<style>")
	b.WriteString(css)
	b.WriteString("</style>")
}

// sanitizeAttrs applies the attribute allowlist to one element, and applies the
// remote-content policy. outbound selects the send-path policy: a remote image
// is DROPPED (only inline cid:/data: images travel in mail we send), and anchors
// are not rewritten with target/rel — see SanitizeOutboundHTML.
func sanitizeAttrs(elem string, in []html.Attribute, outbound bool, st *cssStats, px proxyURLFn) []html.Attribute {
	out := make([]html.Attribute, 0, len(in))
	blocked := false

	for _, a := range in {
		// Namespaced attributes (xlink:href, xml:*) only reach us on foreign
		// content, which is dropped wholesale — but an attribute with a namespace
		// is never something we understand, so it is never something we keep.
		if a.Namespace != "" {
			continue
		}
		key := strings.ToLower(a.Key)

		// Event handlers, unconditionally, on every element. This is the one rule
		// that must never grow an exception.
		if strings.HasPrefix(key, "on") {
			continue
		}

		switch key {
		case "src", "srcset":
			// The remote-content decision. Only <img> keeps an image at all —
			// every other element that could carry a src is in droppedSubtrees or
			// is not on the allowlist.
			if elem != "img" {
				continue
			}
			if key == "srcset" {
				if outbound {
					// The compose editor never produces a srcset, and the read
					// path's data-mailroom-* rewrite has no meaning to a recipient.
					// Drop it rather than ship a responsive-image blocker.
					continue
				}
				if strings.TrimSpace(a.Val) != "" {
					// ⚠ WITH THE PROXY ON, srcset IS DROPPED RATHER THAN PARKED,
					// and dropping is the only safe option of the three.
					//
					// Parking the original candidates would be fail-OPEN: the
					// client restores srcset by name and the browser would fetch
					// every candidate straight from the sender, defeating the
					// proxy for exactly the images it is meant to cover.
					// Rewriting each candidate needs the srcset grammar parsed,
					// and a candidate list is comma-separated while a URL may
					// itself contain a comma — so a parser that guessed wrong
					// would either drop a real image or emit a malformed list.
					//
					// Dropping costs the responsive VARIANTS only: `src` is
					// still parked and proxied on the same element, so the image
					// renders, just not at the highest available resolution.
					// That is a visible degradation rather than a silent
					// removal, and it is bounded — a follow-up can proxy the
					// candidates once someone has measured how many images here
					// actually carry a srcset.
					if px != nil && !isTrackingPixelAttrs(in) {
						blocked = true
						if st != nil {
							st.remoteImage(imageFactsFrom(in, firstSrcsetURL(a.Val), true))
						}
						continue
					}
					// ⚠ srcset is parked under the pixel name too when the element
					// is a beacon. Handling only `src` would leave a hole: a 1×1
					// carrying a srcset would keep a restorable name and load.
					parkAs := remoteSrcsetAttr
					if isTrackingPixelAttrs(in) {
						parkAs = pixelSrcsetAttr
					}
					out = append(out, html.Attribute{Key: parkAs, Val: a.Val})
					blocked = true
					// Measurement only (#55): records what was parked, changes
					// nothing about the parking. srcset is a candidate list, so
					// the host recorded is the FIRST candidate's — enough to say
					// who would be contacted, and the report counts srcset
					// separately from src for exactly that imprecision.
					// ⚠ GUARDED HERE, not inside remoteImage. Go evaluates the
					// argument first, so a nil check inside the method would
					// still have parsed a URL and built a struct for every
					// remote image on the RENDER path, where nothing is being
					// measured. Same shape as the cssStats counters' documented
					// trap, and the render path is every message every user opens.
					if st != nil {
						st.remoteImage(imageFactsFrom(in, firstSrcsetURL(a.Val), true))
					}
				}
				continue
			}
			switch classifyImageSrc(a.Val) {
			case srcInline:
				// data:image/… and cid: — bytes that are already in the message.
				// They fetch nothing, so they are not remote content. On the send
				// path this is what preserves an inline image the user inserted:
				// the <img src="cid:…"> keeps pointing at the inline attachment.
				// Measurement only (#674): a cid: that carries a '%' can never
				// match its part — the part name is compared verbatim — so the
				// reference is silently dead. Counted, and nothing appended
				// changes. ⚠ GUARDED HERE, not after the checks — the render path
				// passes a nil *cssStats (see the srcset branch above).
				if st != nil {
					if strings.HasPrefix(normalizeURL(a.Val), "cid:") && strings.Contains(a.Val, "%") {
						st.Silent.CidPercentEncoded++
					}
				}
				out = append(out, html.Attribute{Key: "src", Val: strings.TrimSpace(a.Val)})
			case srcRemote:
				if !outbound {
					// Received mail: park the URL for the "show remote content"
					// button and mark the element as blocked.
					// A beacon is parked under a name the client never restores,
					// so it stays inert even for a sender the user has allowed
					// (#55 increment 2). Still `blocked`, because it IS remote
					// content that was withheld — and the client's line about how
					// many trackers it blocked is what explains a "show remote
					// content" click that then loads nothing.
					parkAs := remoteSrcAttr
					beacon := isTrackingPixelAttrs(in)
					if beacon {
						parkAs = pixelSrcAttr
					}
					parked := strings.TrimSpace(a.Val)
					// ⚠ THE PROXY REWRITE (ADR-026 §3b/§3d), and note what it is
					// NOT: it does not park the sender's URL and add a proxy
					// alternative beside it. It parks the PROXY's URL INSTEAD, so
					// the sender's URL never reaches the client at all. A client
					// that cannot see the real URL cannot be made to fetch it by
					// any later bug — the same structural argument v1.42.0 used
					// for beacons, rather than a branch that could be written the
					// wrong way.
					//
					// A BEACON IS NEVER REWRITTEN. §3d: proxying a beacon would
					// reintroduce exactly the tracking increment 2 removed, just
					// from a different address. It keeps the pixel name, which
					// nothing restores.
					// ⚠ UNCONDITIONAL ASSIGNMENT, and that is the fail-closed
					// choice. A rewriter that returned "" would park an empty
					// src — a broken image, which is visible. Keeping the
					// sender's URL on "the rewriter did not like it" would park
					// the real URL under a name the client restores, and the
					// browser would fetch the sender DIRECTLY. That is the
					// fail-open decision C forbids, and it would happen at
					// exactly the moment the user is told they are protected.
					if px != nil && !beacon {
						parked = px(parked)
					}
					out = append(out, html.Attribute{Key: parkAs, Val: parked})
					blocked = true
					// Measurement only (#55): records what was parked. It does
					// not decide anything and does not change what is parked —
					// the corpus report reads these to count tracking pixels.
					// Guarded, not nil-checked inside — see the srcset branch above.
					if st != nil {
						st.remoteImage(imageFactsFrom(in, a.Val, false))
					}
				}
				// Outbound: dropped — mail we send embeds no third-party fetch.
				// Inline (cid:/data:) images survive above; a remote one only
				// arrives by pasted markup and is not restorable.
			default:
				// javascript:, blob:, file:, about:, a data: that is not an image,
				// anything unparseable: gone, and not restorable.
			}
			continue

		case "href":
			// A link is user-initiated navigation, which is the one remote thing a
			// message body is allowed to do — and only after a click.
			u, ok := safeLinkURL(a.Val)
			if !ok {
				continue
			}
			out = append(out, html.Attribute{Key: "href", Val: u})
			continue

		case "style":
			css := sanitizeStyleAttrStats(a.Val, st)
			st.styleAttr(css == "")
			if css == "" {
				continue
			}
			out = append(out, html.Attribute{Key: "style", Val: css})
			continue

		case "target", "rel":
			// Forced below on anchors; never taken from the message.
			continue

		// Attributes that name a URL and have no legitimate use in mail we serve.
		case "background", "poster", "action", "formaction", "cite", "data",
			"longdesc", "profile", "usemap", "ping", "srcdoc", "http-equiv",
			"lowsrc", "dynsrc", "codebase", "archive", "manifest":
			// Measurement only (#674): a background= naming a remote URL is
			// dropped here and never reaches the proxy. ⚠ GUARDED HERE, not
			// after the classify — the render path passes a nil *cssStats.
			if st != nil {
				if key == "background" && classifyImageSrc(a.Val) == srcRemote {
					st.Silent.BackgroundAttr++
				}
			}
			continue
		}

		if !allowedAttrs[key] {
			continue
		}
		out = append(out, html.Attribute{Key: key, Val: a.Val})
	}

	if elem == "a" && !outbound {
		out = forceLinkSafety(out)
	}
	if blocked {
		out = append(out, html.Attribute{Key: blockedAttr, Val: ""})
	}
	return out
}

// forceLinkSafety puts target/rel on an anchor OURSELVES rather than honoring
// what the message said.
//
// The message iframe is sandboxed with allow-popups (docs/hostile-content.md:
// without it, no link in any HTML mail works at all) and
// allow-popups-to-escape-sandbox (or the opened page loads into an opaque origin
// and is broken). `noopener` is what keeps the opened page from getting a handle
// on the frame that opened it, and `noreferrer` is what keeps the sender from
// learning anything about where the click came from.
func forceLinkSafety(attrs []html.Attribute) []html.Attribute {
	hasHref := false
	for _, a := range attrs {
		if a.Key == "href" {
			hasHref = true
		}
	}
	if !hasHref {
		return attrs
	}
	return append(attrs,
		html.Attribute{Key: "target", Val: "_blank"},
		html.Attribute{Key: "rel", Val: "noopener noreferrer nofollow"},
	)
}

// --- URLs --------------------------------------------------------------------

type srcKind int

const (
	srcNone srcKind = iota
	srcInline
	srcRemote
)

// classifyImageSrc decides what an <img src> is.
func classifyImageSrc(raw string) srcKind {
	s := normalizeURL(raw)
	switch {
	case s == "":
		return srcNone
	case strings.HasPrefix(s, "cid:"):
		return srcInline
	case strings.HasPrefix(s, "data:image/"):
		// A data: URL is bytes, not a fetch — but only an IMAGE one. data:text/html
		// and data:image/svg+xml are scripting contexts wearing an <img> costume.
		if strings.HasPrefix(s, "data:image/svg+xml") {
			return srcNone
		}
		return srcInline
	case strings.HasPrefix(s, "http://"), strings.HasPrefix(s, "https://"),
		strings.HasPrefix(s, "//"):
		return srcRemote
	case hasScheme(s):
		// A scheme we do not recognize (javascript:, vbscript:, blob:, file:,
		// about:, feed:, jar:, …). Never.
		return srcNone
	default:
		// A relative URL. In a message body there is nothing for it to be relative
		// TO (the document is a srcdoc with no base), so it can only resolve
		// against whatever origin renders it — which is exactly what we refuse.
		return srcNone
	}
}

// linkSchemes are the schemes an <a href> may carry. `cid:` is not among them:
// a link to a message part is a download, and a client gets those from
// attachments[].blobId, not from a body link the sender wrote.
var linkSchemes = map[string]bool{
	"http": true, "https": true, "mailto": true, "tel": true,
}

// safeLinkURL validates an <a href>. It returns the value to emit, and whether
// the link survives at all.
func safeLinkURL(raw string) (string, bool) {
	s := normalizeURL(raw)
	if s == "" {
		return "", false
	}
	// An in-message anchor: harmless, and the only relative form worth keeping.
	if strings.HasPrefix(s, "#") {
		return strings.TrimSpace(raw), true
	}
	if strings.HasPrefix(s, "//") {
		// A protocol-relative URL resolves against OUR scheme. Pin it to https
		// rather than letting a message decide to go out in cleartext.
		return "https:" + strings.TrimSpace(raw), true
	}
	scheme, ok := splitScheme(s)
	if !ok || !linkSchemes[scheme] {
		return "", false
	}
	return strings.TrimSpace(raw), true
}

// normalizeURL lowercases and strips the bytes an HTML parser ignores when it
// looks for a scheme. `java&Tab;script:` and "\x01javascript:" both reach us as
// literal control characters (x/net/html has already decoded the entities), and
// a browser will happily strip them before deciding the URL is javascript:. So
// we strip them first, and decide on the same string the browser will.
func normalizeURL(raw string) string {
	var b strings.Builder
	for _, r := range raw {
		if r <= 0x20 || r == 0x7f {
			continue
		}
		b.WriteRune(r)
	}
	return strings.ToLower(b.String())
}

// hasScheme reports whether s begins with an RFC 3986 scheme.
func hasScheme(s string) bool {
	_, ok := splitScheme(s)
	return ok
}

// splitScheme returns the scheme of s (already normalized: lowercase, no
// control/space characters), and whether it has one.
func splitScheme(s string) (string, bool) {
	i := strings.IndexByte(s, ':')
	if i <= 0 {
		return "", false
	}
	// A colon that appears after a '/', '?' or '#' is part of a path or a query,
	// not a scheme delimiter ("foo/bar:baz").
	if j := strings.IndexAny(s, "/?#"); j >= 0 && j < i {
		return "", false
	}
	for k := 0; k < i; k++ {
		c := s[k]
		switch {
		case c >= 'a' && c <= 'z':
		case c >= '0' && c <= '9', c == '+', c == '-', c == '.':
			if k == 0 {
				return "", false // a scheme must start with a letter
			}
		default:
			return "", false
		}
	}
	return s[:i], true
}

// --- CSS ---------------------------------------------------------------------
//
// The CSS filter lives in sanitize_css.go, and it is a TOKENIZER plus an
// allowlist of PROPERTIES — the same parse-filter-reserialize shape as the HTML
// half above, for the same reason.
//
// It used to be a regular expression (`url\s*\(|@import|expression\s*\(|…`), and
// Forgejo #297 is the record of why that could never have worked: CSS Syntax
// §4.3.1 decodes an identifier's escapes BEFORE anything compares it, so
// `\75 rl(https://tracker.example/x.png)` is an ordinary <url-token> containing
// none of the bytes that regex looked for. It was MEASURED fetching in the pinned
// Chromium image. `image-set()` needed no trick at all.
//
// The two entry points are:
//
//	sanitizeStyleAttr(v)   a style="" attribute value — a declaration list
//	sanitizeStylesheet(v)  a <style> element's contents — a list of rules
//
// Both return "" when nothing survives, and both callers below treat "" as
// "drop the attribute / drop the element".
//
// WHAT THIS BUYS, STATED ACCURATELY — and the previous version of this paragraph
// is the reason to be careful. It said the message CSP "already pins
// img-src/font-src/style-src so a url() cannot fetch", which is true ONLY while
// remote content is blocked. Once the user allows a sender's images (#202), the
// message CSP is img-src `data: https:` and a CSS background url() fetches like
// any other image — MEASURED, in the pinned Chromium image, with the real sandbox
// tokens and the real policy. Layer 1 does not cover this case; the server filter
// is the only thing that does.
//
// So what it buys is: (a) the NON-BROWSER client — aerc over JMAP, the future MCP
// surface, an export — which has no CSP at all, (b) every message of a sender the
// user has ever trusted, and (c) the specific claim that the CSS we serve contains
// no construct that can name an external resource: no <url-token> in any position,
// no image function, no @import, no @font-face.
//
// That is deliberately narrower than the claim this comment used to make ("the
// bytes we serve contain no remote reference at all"), which was false twice over
// — once because of #297, and once because a blocked <img> URL is preserved ON
// PURPOSE in data-mailroom-remote-src so the client can offer to load it. The
// bytes we serve DO carry remote URLs; what they carry is no construct that can
// FETCH one. Within CSS specifically there is no remote URL left at all, because
// a stripped CSS url() is not parked and not restorable — see
// docs/hostile-content.md §2.3.

// --- The lists ---------------------------------------------------------------

// droppedSubtrees: the element and everything inside it is removed.
//
// Two kinds of thing are here, and both are here for a reason that is about the
// PARSER rather than about the element:
//
//   - raw-text / escapable-raw-text elements (script, textarea, title, xmp,
//     plaintext, noembed, noframes, iframe): their children are not markup, so
//     re-serializing them is a chance to break out of the element.
//   - foreign content (svg, math): the HTML parser switches to XML-ish rules
//     inside them, where <svg><script> runs, where attribute names are
//     case-sensitive, and where the tree builder's error recovery has produced a
//     long line of mXSS. Mail does not need inline SVG.
//
// …plus the plugin/framing elements (object, embed, applet, frame, frameset),
// <base> and <link> and <meta> (a document-level directive a MESSAGE must never
// get to set — this is where <meta http-equiv="refresh"> dies), and the media
// elements, which exist in a mail body only to phone home.
//
// <style> is NOT here (it is kept, with its CSS filtered — writeStyleElement),
// and neither is <noscript> (it is UNWRAPPED — see the scripting-mode note at
// the top of this file).
var droppedSubtrees = map[string]bool{
	"script":    true,
	"iframe":    true,
	"frame":     true,
	"frameset":  true,
	"object":    true,
	"embed":     true,
	"applet":    true,
	"base":      true,
	"link":      true,
	"meta":      true,
	"title":     true,
	"template":  true,
	"svg":       true,
	"math":      true,
	"canvas":    true,
	"audio":     true,
	"video":     true,
	"source":    true,
	"track":     true,
	"portal":    true,
	"xmp":       true,
	"plaintext": true, "listing": true,
	"noembed": true, "noframes": true,
	"textarea": true,
	"marquee":  true,
	"blink":    true,
}

// allowedElements is what a message body may actually be made of. Anything not
// here is UNWRAPPED (element gone, text kept) rather than dropped, so a message
// wrapped in a <form> or an unknown custom element is still readable.
//
// Notably absent and unwrapped on purpose: form, input, button, select, option,
// label, fieldset, legend (a message body has nothing to submit — and a form in
// mail is a credential-phishing surface), and <noscript> (see the scripting-mode
// note at the top of this file).
var allowedElements = map[string]bool{
	"a": true, "abbr": true, "acronym": true, "address": true, "article": true,
	"aside": true, "b": true, "bdi": true, "bdo": true, "big": true,
	"blockquote": true, "br": true, "caption": true, "center": true, "cite": true,
	"code": true, "col": true, "colgroup": true, "dd": true, "del": true,
	"details": true, "dfn": true, "div": true, "dl": true, "dt": true,
	"em": true, "figcaption": true, "figure": true, "font": true, "footer": true,
	"h1": true, "h2": true, "h3": true, "h4": true, "h5": true, "h6": true,
	"header": true, "hgroup": true, "hr": true, "i": true, "img": true,
	"ins": true, "kbd": true, "li": true, "main": true, "mark": true,
	"nav": true, "ol": true, "p": true, "pre": true, "q": true,
	"rp": true, "rt": true, "ruby": true, "s": true, "samp": true,
	"section": true, "small": true, "span": true, "strike": true, "strong": true,
	"sub": true, "summary": true, "sup": true, "table": true, "tbody": true,
	"td": true, "tfoot": true, "th": true, "thead": true, "time": true,
	"tr": true, "tt": true, "u": true, "ul": true, "var": true, "wbr": true,
}

// voidElements have no closing tag.
var voidElements = map[string]bool{
	"br": true, "col": true, "hr": true, "img": true, "wbr": true,
}

// allowedAttrs is the global attribute allowlist. src/srcset/href/style are
// handled explicitly in sanitizeAttrs and are NOT here — they are decisions, not
// pass-throughs.
//
// The presentational HTML-4 attributes are here because mail is written in
// HTML 4: a sanitizer that strips bgcolor/align/cellpadding turns every
// newsletter into a broken column of text, and none of them can fetch or execute.
var allowedAttrs = map[string]bool{
	"alt": true, "title": true, "class": true, "id": true, "name": true,
	"dir": true, "lang": true, "role": true, "hidden": true,
	"width": true, "height": true, "align": true, "valign": true,
	"border": true, "cellpadding": true, "cellspacing": true,
	"colspan": true, "rowspan": true, "span": true,
	"bgcolor": true, "color": true, "face": true, "size": true,
	"start": true, "reversed": true, "type": true, "value": true,
	"datetime": true, "abbr": true, "headers": true, "scope": true,
	"nowrap": true, "hspace": true, "vspace": true, "loading": true,
}

// sanitizeHTMLWithProxy is SanitizeHTML with the image proxy's rewrite applied.
//
// px == nil is byte-identical to SanitizeHTML — TestProxyOffIsByteIdenticalToSanitizeHTML
// pins that, because "the proxy changes nothing when it is off" is a claim about
// the render path that every message every user opens depends on.
func sanitizeHTMLWithProxy(in string, px proxyURLFn) string {
	return sanitizeFragment(in, false, nil, px)
}
