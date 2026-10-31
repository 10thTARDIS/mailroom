// SPDX-License-Identifier: AGPL-3.0-only

package api

import (
	"strings"
	"testing"
)

// The remote-image measurement hook (#55 increment 2).
//
// Every test here was probed by breaking the mechanism it covers; the probes are
// named next to the assertions they justify.

// TestMeasuredImagesAreIdenticalToSanitizeHTML is the claim the whole report
// rests on, and it is the reason the hook returns the sanitized string at all.
//
// If the instrumented path ever diverged from the shipped one, every number the
// pixel report prints would describe different code than the renderer runs —
// which is the #169/#220 failure exactly, and it would be invisible because both
// paths would still "work".
//
// Probed: making remoteImage append an attribute to `out` reddens this.
func TestMeasuredImagesAreIdenticalToSanitizeHTML(t *testing.T) {
	inputs := append(append([]string{}, cssSamples...), hostileCorpus()...)
	inputs = append(inputs,
		`<img src="https://tracker.example/open.gif?id=abc" width="1" height="1" alt="">`,
		`<img srcset="https://cdn.example/a.png 1x, https://cdn.example/b.png 2x">`,
		`<img src="cid:inline@example">`,
		`<img src="data:image/png;base64,iVBORw0KGgo=">`,
	)
	for _, in := range inputs {
		want := SanitizeHTML(in)
		got, _ := MeasureHTMLRemoteImages(in)
		if got != want {
			t.Errorf("the instrumented path diverged from the shipped one\n  in:   %s\n  ship: %q\n  meas: %q",
				in, want, got)
		}
	}
}

// TestRemoteImagesOnlyCountsWhatWouldActuallyFetch is the negative control that
// makes every count in the report mean something.
//
// A cid: or data: image fetches nothing and cannot track anybody; an image with
// a javascript: or relative src is dropped outright and is not restorable. If any
// of those were recorded, the report's "remote images" figure would be inflated
// by images that are structurally incapable of contacting anyone — and the number
// would still look plausible.
//
// Probed: recording in the srcInline branch reddens this.
func TestRemoteImagesOnlyCountsWhatWouldActuallyFetch(t *testing.T) {
	for _, in := range []string{
		`<img src="cid:inline@example" width="1" height="1">`,
		`<img src="data:image/png;base64,iVBORw0KGgo=" width="1" height="1">`,
		`<img src="javascript:alert(1)">`,
		`<img src="/relative/pixel.gif" width="1" height="1">`,
		`<p>no images at all</p>`,
		`<div style="background-image:url(https://tracker.example/p.gif)">x</div>`,
	} {
		if _, imgs := MeasureHTMLRemoteImages(in); len(imgs) != 0 {
			t.Errorf("recorded %d remote image(s) for input that fetches nothing: %s\n  got: %+v",
				len(imgs), in, imgs)
		}
	}
	// Positive control: without this, every assertion above would also pass if
	// the hook recorded nothing at all, ever.
	_, imgs := MeasureHTMLRemoteImages(`<img src="https://tracker.example/p.gif">`)
	if len(imgs) != 1 {
		t.Fatalf("a genuinely remote image was not recorded — the assertions above prove nothing; got %+v", imgs)
	}
}

// TestRemoteImageFactsAreCaptured pins the attributes the report classifies on.
// The sanitizer keeps width/height/style/alt, so the facts must survive with it.
//
// Probed: dropping any case from imageFactsFrom's switch reddens this.
func TestRemoteImageFactsAreCaptured(t *testing.T) {
	_, imgs := MeasureHTMLRemoteImages(
		`<img src="https://Tracker.Example/open.gif?id=abc123" width="1" height="1" ` +
			`style="display:none" alt="">`)
	if len(imgs) != 1 {
		t.Fatalf("want 1 image, got %d", len(imgs))
	}
	g := imgs[0]
	if g.Host != "tracker.example" {
		t.Errorf("Host = %q, want the lowercased host", g.Host)
	}
	if g.Width != "1" || g.Height != "1" {
		t.Errorf("Width/Height = %q/%q, want 1/1", g.Width, g.Height)
	}
	if !strings.Contains(g.Style, "display:none") {
		t.Errorf("Style = %q, want the declared style verbatim", g.Style)
	}
	if !g.HasAlt || g.Alt != "" {
		t.Errorf("HasAlt/Alt = %v/%q — alt=\"\" is a deliberate 'decorative' and is not "+
			"the same as no alt at all", g.HasAlt, g.Alt)
	}
	if g.FromSrcset {
		t.Error("FromSrcset is true for a plain src")
	}
}

// TestRemoteImageAbsentAttributesAreEmptyNotZero keeps "the sender declared
// nothing" distinct from "the sender declared 0". They are different senders
// doing different things, and a report that merged them would classify every
// undimensioned banner as a pixel.
//
// Probed: initialising Width/Height to "0" reddens this.
func TestRemoteImageAbsentAttributesAreEmptyNotZero(t *testing.T) {
	_, imgs := MeasureHTMLRemoteImages(`<img src="https://cdn.example/banner.png">`)
	if len(imgs) != 1 {
		t.Fatalf("want 1 image, got %d", len(imgs))
	}
	g := imgs[0]
	if g.Width != "" || g.Height != "" {
		t.Errorf("Width/Height = %q/%q, want empty for absent attributes", g.Width, g.Height)
	}
	if g.HasAlt {
		t.Error("HasAlt is true for an element with no alt attribute")
	}
}

// TestRemoteImageSrcsetIsRecordedSeparately — a responsive image is not a
// beacon, and the report counts them apart so a mailbox full of srcset banners
// cannot masquerade as a mailbox full of trackers.
//
// Probed: passing false for fromSrcset at the srcset call site reddens this.
func TestRemoteImageSrcsetIsRecordedSeparately(t *testing.T) {
	_, imgs := MeasureHTMLRemoteImages(
		`<img srcset="https://cdn.example/a.png 1x, https://other.example/b.png 2x">`)
	if len(imgs) != 1 {
		t.Fatalf("want 1 image, got %d", len(imgs))
	}
	if !imgs[0].FromSrcset {
		t.Error("FromSrcset is false for a srcset-parked image")
	}
	if imgs[0].Host != "cdn.example" {
		t.Errorf("Host = %q, want the FIRST candidate's host", imgs[0].Host)
	}
}

// TestRemoteImageStatsNilNeverPanics — the render path passes a nil *cssStats,
// so the measurement must cost it nothing and must not crash it. Every user's
// mail goes through this path. The silent-forms inputs (#674) sit on this same
// path and run through the same loop.
func TestRemoteImageStatsNilNeverPanics(t *testing.T) {
	inputs := []string{
		`<img src="https://tracker.example/p.gif" width="1" height="1">`,
		`<img srcset="https://cdn.example/a.png 1x">`,
		`<img src="cid:x@y">`,
	}
	for _, tc := range silentFormCases {
		inputs = append(inputs, tc.in)
	}
	for _, in := range inputs {
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("SanitizeHTML panicked on %s: %v", in, r)
				}
			}()
			_ = SanitizeHTML(in)
		}()
	}
}

// silentFormCases is the mail the silent-forms recording sees (#674): each
// form it counts, each near-miss that must not count, and a plain remote img
// that must count nowhere. The counting test, the byte-identity test and the
// nil-path test all run this same list, so a divergence in one shows up next
// to the count it would corrupt.
var silentFormCases = []struct {
	in         string
	remoteWant *int // parked remote images returned; nil = not asserted
	want       SilentImageForms
}{
	{
		in:   `<table><tr><td background="https://x.example/bg.png">t</td></tr></table>`,
		want: SilentImageForms{BackgroundAttr: 1},
	},
	{
		in:   `<table><tr><td background="cid:x">t</td></tr></table>`,
		want: SilentImageForms{},
	},
	{
		in:         `<picture><source srcset="https://x.example/a.webp 1x"><img src="https://x.example/a.png"></picture>`,
		remoteWant: silentTestPtr(1),
		want:       SilentImageForms{PictureSource: 1},
	},
	{
		in:   `<picture><source srcset="data:image/png;base64,AAAA"></picture>`,
		want: SilentImageForms{},
	},
	{
		in:   `<img src="cid:a%40b">`,
		want: SilentImageForms{CidPercentEncoded: 1},
	},
	{
		in:   `<img src="cid:a@b">`,
		want: SilentImageForms{},
	},
	{
		in:   `<img src="https://x.example/a.png">`,
		want: SilentImageForms{},
	},
}

// silentTestPtr is the table's pointer helper: a nil remoteWant means the
// parked-image count is not asserted for that input.
func silentTestPtr(n int) *int { return &n }

// TestSilentImageFormsAreCounted — the forms the sanitizer drops before they
// could be parked (#674) are counted, and the near-misses are not.
//
// Probed: removing any of the three recording sites in sanitize.go reddens the
// case that exercises it; recording a near-miss reddens its zero.
func TestSilentImageFormsAreCounted(t *testing.T) {
	for _, tc := range silentFormCases {
		_, imgs, got := MeasureHTMLImages(tc.in)
		if tc.remoteWant != nil && len(imgs) != *tc.remoteWant {
			t.Errorf("%s: parked remote images = %d, want %d", tc.in, len(imgs), *tc.remoteWant)
		}
		if got != tc.want {
			t.Errorf("SilentImageForms for %s = %+v, want %+v", tc.in, got, tc.want)
		}
	}
}

// TestMeasuredImagesWithSilentFormsAreIdenticalToSanitizeHTML — the
// silent-forms counters must not change a single byte of the output. That is
// the "measurement only" claim of #674 made checkable the same way
// TestMeasuredImagesAreIdenticalToSanitizeHTML makes it for parked images.
//
// Probed: any of the three recording sites appending to the output reddens
// this.
func TestMeasuredImagesWithSilentFormsAreIdenticalToSanitizeHTML(t *testing.T) {
	for _, tc := range silentFormCases {
		in := tc.in
		want := SanitizeHTML(in)
		got, _, _ := MeasureHTMLImages(in)
		if got != want {
			t.Errorf("the instrumented path diverged from the shipped one\n  in:   %s\n  ship: %q\n  meas: %q",
				in, want, got)
		}
	}
}
