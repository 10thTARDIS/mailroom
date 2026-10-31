// SPDX-License-Identifier: AGPL-3.0-only

package api

import (
	"net/url"
	"strconv"
	"strings"

	"golang.org/x/net/html"
)

// Per-image facts collected while the REAL sanitizer runs (#55 increment 2), so
// a corpus report can measure tracking pixels without re-parsing anything.
//
// ⚠ IT RECORDS FACTS AND CLASSIFIES NOTHING. What counts as a "tracking pixel"
// is a judgement with no single right answer — 1×1, "tiny", hidden by CSS, no
// dimensions at all — and the whole point of the report is to show how the
// populations differ under each rule. Deciding here would collapse that into one
// number before anybody had seen the alternatives, which is the mistake the
// stored auth_fail boolean made in increment 0.
//
// ⚠ IT PIGGYBACKS ON THE CSS COLLECTOR, and the type is still called cssStats.
// That name is now narrower than what it carries. It is deliberately NOT renamed
// here: the rename touches 33 sites across the sanitizer, and this change should
// stay small enough to review as a security diff. Worth doing on its own.
//
// WHY THE SANITIZER AND NOT A SECOND PARSER. The alternative — walk the HTML
// again in cmd/ and look for <img> — would be a measurement of code written by
// the same hand to agree with the same assumptions (#169, #220), and it would
// drift the moment the sanitizer's idea of "remote" changed. This sees exactly
// what the shipped filter saw, and TestMeasuredImagesAreIdenticalToSanitizeHTML
// pins that the instrumented path still produces byte-identical output.

// RemoteImage is one <img> whose URL the sanitizer PARKED for the "show remote
// content" affordance — i.e. one that would fetch from the sender's server if
// the user allowed it. Images that fetch nothing (cid:, data:) are absent, and
// so are the ones dropped outright, because neither can track anybody.
type RemoteImage struct {
	// Host is the parked URL's host, lowercased, or "" when it did not parse.
	// Only the host: the path and query of a tracking URL are an identifier for
	// the recipient, and a report that printed them would be copying the tracking
	// token into a terminal and a paste buffer.
	Host string
	// Scheme is the parked URL's scheme, lowercased. It is "" for a
	// protocol-relative URL (//host/p.gif), which is a THIRD case and not a
	// missing value: such a URL inherits the page's scheme, so in a client
	// served over https it is already https and was never part of ADR-026's
	// decision A. Non-identifying, so unlike URL it is safe to print.
	Scheme string
	// URL is the parked URL, verbatim.
	//
	// ⚠ NEVER PRINT THIS, AND NEVER PUT IT IN AN AGGREGATE. The path and query
	// of a tracking URL are an identifier for the recipient — printing one
	// copies the tracking token into a terminal, a scrollback and a paste
	// buffer. It exists because the proxy resolves the URL from the stored
	// message (ADR-026 §3b) and because proxy-report's --fetch has to fetch
	// something. Host and Scheme are what reports are allowed to show.
	URL string
	// FromSrcset is true when the URL was parked from srcset rather than src.
	// Counted apart because a responsive image is a different thing from a
	// beacon, and lumping them together would inflate the pixel count.
	FromSrcset bool

	// The declaring attributes, VERBATIM and unparsed. The report decides what
	// they mean; empty string means the attribute was absent, which is itself a
	// signal and is not the same as a zero value.
	Width  string
	Height string
	Style  string
	Alt    string
	// HasAlt distinguishes alt="" (a deliberate "this image is decorative",
	// which a tracking pixel very often carries) from no alt attribute at all.
	HasAlt bool
}

// SilentImageForms counts image references the sanitizer drops outright, so
// they never become a parked image and never reach the proxy (#674). Facts
// only: the sanitizer's output is unchanged.
type SilentImageForms struct {
	BackgroundAttr    int // a background= attribute naming a remote URL
	PictureSource     int // a <source> (as in <picture>) whose src or first srcset candidate is remote
	CidPercentEncoded int // an <img src="cid:…"> containing '%', which never matches its part
}

// remoteImage records one parked image. Nil-receiver safe: the render path
// passes a nil *cssStats and must not pay for, or panic on, any of this.
func (s *cssStats) remoteImage(img RemoteImage) {
	if s == nil {
		return
	}
	s.RemoteImages = append(s.RemoteImages, img)
}

// imageFactsFrom builds a RemoteImage from the element's ORIGINAL attributes.
//
// It reads the attributes as the sender wrote them rather than as they survive
// the filter, and for these four that is the same thing: width and height are on
// the attribute allowlist, and display/visibility/opacity are on the CSS property
// allowlist, so none of them is stripped. Reading the originals keeps this honest
// if that ever stops being true — the report would then be describing what the
// sender asked for, which is the question it is actually asking.
func imageFactsFrom(in []html.Attribute, rawURL string, fromSrcset bool) RemoteImage {
	raw := strings.TrimSpace(rawURL)
	img := RemoteImage{
		Host:       urlHost(raw),
		Scheme:     urlScheme(raw),
		URL:        raw,
		FromSrcset: fromSrcset,
	}
	for _, a := range in {
		switch strings.ToLower(a.Key) {
		case "width":
			img.Width = a.Val
		case "height":
			img.Height = a.Val
		case "style":
			img.Style = a.Val
		case "alt":
			img.Alt = a.Val
			img.HasAlt = true
		}
	}
	return img
}

// urlHost is the lowercased host of a parked URL, or "" if it will not parse.
//
// Protocol-relative URLs (//tracker.example/p.gif) are ordinary in mail and
// url.Parse handles them, but only because the leading // makes it host-relative
// — a bare tracker.example/p.gif parses as a PATH with no host, and returning ""
// for it is correct rather than a gap.
func urlHost(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return ""
	}
	return strings.ToLower(u.Hostname())
}

// urlScheme is the lowercased scheme of a parked URL.
//
// ⚠ "" IS A REAL ANSWER, NOT A FAILURE. A protocol-relative URL
// (//tracker.example/p.gif) has no scheme of its own and takes the page's, so
// in a client served over https it is https. Reporting it as a third category
// rather than folding it into "http" is what keeps ADR-026's decision A
// measurable: the images that only a proxy can ever render are the ones written
// explicitly as http, and a protocol-relative URL is not one of them.
func urlScheme(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return ""
	}
	return strings.ToLower(u.Scheme)
}

// MeasureHTMLRemoteImages runs the shipped sanitizer over one HTML fragment and
// returns its output alongside every remote image it parked.
//
// The returned string is the sanitizer's real output and is not decoration:
// TestMeasuredImagesAreIdenticalToSanitizeHTML compares it against SanitizeHTML
// on the same inputs, which is what makes "this measures the shipped filter" a
// checked claim instead of a comment.
func MeasureHTMLRemoteImages(in string) (string, []RemoteImage) {
	st := newCSSStats()
	out := sanitizeFragment(in, false, st, nil)
	return out, st.RemoteImages
}

// MeasureHTMLImages is MeasureHTMLRemoteImages plus the forms the sanitizer
// drops outright, which never become parked images and never reach the proxy
// (#674). The returned string is, as there, the sanitizer's real output and
// not decoration:
// TestMeasuredImagesWithSilentFormsAreIdenticalToSanitizeHTML compares it
// against SanitizeHTML on the same inputs.
func MeasureHTMLImages(in string) (string, []RemoteImage, SilentImageForms) {
	st := newCSSStats()
	out := sanitizeFragment(in, false, st, nil)
	return out, st.RemoteImages, st.Silent
}

// firstSrcsetURL is the URL of a srcset's first candidate.
//
// A srcset is "url [descriptor], url [descriptor], …". Only the first URL is
// taken, and the report says so rather than implying it counted them all: the
// question this answers is "which host would be contacted", and for a beacon
// hidden in a srcset the candidates are the same host anyway. Returns "" for an
// empty or malformed list, which urlHost then renders as an unknown host rather
// than inventing one.
func firstSrcsetURL(srcset string) string {
	first := srcset
	if i := strings.IndexByte(first, ','); i >= 0 {
		first = first[:i]
	}
	first = strings.TrimSpace(first)
	if i := strings.IndexAny(first, " \t\n\r\f"); i >= 0 {
		first = first[:i]
	}
	return first
}

// ---------------------------------------------------------------------------
// The tracking-pixel rule (#55 increment 2)
// ---------------------------------------------------------------------------
//
// ⚠ 1×1 ONLY, AND THAT NARROWNESS IS MEASURED RATHER THAN CAUTIOUS. `pixel-report`
// counted four candidate rules over a real corpus and the exposed
// population — the messages whose sender is already allowed, so their remote
// content actually loads:
//
//	rule              what it catches
//	1×1               the tracking pixels   ← all of the exposed messages
//	tiny (<=3px)      almost nothing further
//	hidden by style   spacers and dark-mode variants
//	no dimensions     most content images
//
// 1×1 alone accounts for **every** exposed message, so no other rule adds
// coverage — it can only add false positives. "hidden by style" in particular
// would catch preheader spacers and dark-mode variants, which are ordinary
// layout, and "no dimensions" would catch most content images in mail.
//
// A wider rule here does not fail loudly: it silently stops a real image from
// loading in mail the user chose to trust. So the rule is the narrowest one that
// covers what was actually measured, and widening it needs new evidence rather
// than an intuition.

// IsTrackingPixel reports whether a parked image is a beacon by the shipped rule.
//
// It is exported so `pixel-report`'s "1x1" row measures THE RULE THAT SHIPS
// rather than a copy of it — the report's other rows are candidates that were
// considered and not adopted, and the difference between those two things has to
// stay visible.
func IsTrackingPixel(img RemoteImage) bool { return oneByOne(img.Width, img.Height) }

// isTrackingPixelAttrs is the same rule, read straight off the element's
// attributes.
//
// ⚠ It exists so the RENDER path never builds a RemoteImage. That struct parses
// the URL to find its host, which the rule does not use and which every message
// every user opens would otherwise pay for, once per remote image — tens of
// thousands of times over a mailbox this size. Measurement may be expensive;
// deciding what to render may not.
func isTrackingPixelAttrs(in []html.Attribute) bool {
	var w, h string
	for _, a := range in {
		switch strings.ToLower(a.Key) {
		case "width":
			w = a.Val
		case "height":
			h = a.Val
		}
	}
	return oneByOne(w, h)
}

// oneByOne is the rule itself, in one place so the two entry points above cannot
// drift: BOTH dimensions declared, and both at most one pixel.
//
// Requiring both is what separates a beacon from a divider. A 1×600 image is a
// rule line and is common in mail; treating it as a tracker would delete a
// visible part of the message. `pixel-report` counts that case ("tiny") as a
// separate row precisely so the difference is legible.
//
// 0 counts: a 0×0 image is no more a picture than a 1×1 one.
func oneByOne(width, height string) bool {
	w, wOK := pixelDim(width)
	h, hOK := pixelDim(height)
	return wOK && hOK && w <= 1 && h <= 1
}

// pixelDim reads an HTML dimension attribute, reporting ok=false when it was
// absent or is not a plain pixel count.
//
// ⚠ "50%" MUST NOT read as 50. A percentage is a responsive image — the opposite
// of a beacon — and a parser that accepted the leading digits would suppress it.
// Anything that is not a whole non-negative number of pixels is "not declared".
func pixelDim(v string) (int, bool) {
	t := strings.TrimSpace(strings.ToLower(v))
	if t == "" {
		return 0, false
	}
	t = strings.TrimSpace(strings.TrimSuffix(t, "px"))
	n, err := strconv.Atoi(t)
	if err != nil || n < 0 {
		return 0, false
	}
	return n, true
}
