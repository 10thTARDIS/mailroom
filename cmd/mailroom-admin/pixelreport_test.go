// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"strings"
	"testing"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/api"
	"mailroom/internal/store"
)

// pixel-report's classification and its OUTPUT (#55 increment 2).
//
// The output half exists for the reason authreport_test.go does: a report that
// computes a figure and prints it nowhere errors on nothing and looks complete,
// and a test on the counters passes right through it.
//
// Every test here was probed by breaking the mechanism it covers.

func img(width, height, style string) api.RemoteImage {
	return api.RemoteImage{Host: "tracker.example", Width: width, Height: height, Style: style}
}

// TestClassifyPixelSeparatesTheRules is the report's whole reason for existing:
// the candidate rules select different populations, so they must not collapse
// into one another.
//
// Probed: dropping the `wOK && hOK` guard makes a 1-wide divider read as 1×1.
func TestClassifyPixelSeparatesTheRules(t *testing.T) {
	cases := []struct {
		name string
		in   api.RemoteImage
		want pixelSignals
	}{
		{"the classic beacon", img("1", "1", ""), pixelSignals{oneByOne: true}},
		{"zero-sized is also 1x1 by this rule", img("0", "0", ""), pixelSignals{oneByOne: true}},
		{"small but not 1x1", img("2", "3", ""), pixelSignals{tiny: true}},
		// ⚠ A 1×600 image is a divider, not a beacon. It matches `tiny` because
		// the rule is one-dimension-based, and that generosity is exactly what the
		// report is for: it is visible as a row rather than hidden in a verdict.
		{"a tall thin divider matches tiny, deliberately", img("1", "600", ""), pixelSignals{tiny: true}},
		{"hidden by display", img("640", "480", "display:none"), pixelSignals{hidden: true}},
		{"hidden AND 1x1 counts as both", img("1", "1", "visibility:hidden"),
			pixelSignals{oneByOne: true, hidden: true}},
		{"no dimensions at all", img("", "", ""), pixelSignals{noDims: true}},
		// A percentage width is a real, responsive image. If it parsed as a number
		// it would read as a 50-pixel image; it must instead read as "not declared".
		{"percentage width is not a dimension", img("50%", "", ""), pixelSignals{noDims: true}},
		{"an ordinary banner matches nothing", img("600", "200", ""), pixelSignals{}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := classifyPixel(c.in)
			if got != c.want {
				t.Errorf("classifyPixel(%+v) = %+v, want %+v", c.in, got, c.want)
			}
		})
	}
}

// TestStyleHidesDoesNotFireOnPartialOpacity guards the substring shortcut. A
// half-transparent image is a design choice, not a beacon, and matching it would
// put ordinary mail in a security report.
//
// Probed: removing the boundaryAfter guard reddens this.
func TestStyleHidesDoesNotFireOnPartialOpacity(t *testing.T) {
	hidden := []string{
		"display:none", "DISPLAY : NONE", "visibility:hidden", "opacity:0",
		"width:0;height:0", "max-height:0", "border:0;opacity:0",
	}
	for _, s := range hidden {
		if !styleHides(s) {
			t.Errorf("styleHides(%q) = false, want true", s)
		}
	}
	visible := []string{
		"", "opacity:0.5", "opacity:0.85", "width:0.5em", "display:block",
		"visibility:visible", "width:600px;height:200px",
	}
	for _, s := range visible {
		if styleHides(s) {
			t.Errorf("styleHides(%q) = true — an ordinary style must not read as hidden", s)
		}
	}
}

func TestParseDim(t *testing.T) {
	ok := map[string]int{"1": 1, "0": 0, "600": 600, " 12 ": 12, "3px": 3, "12PX": 12}
	for in, want := range ok {
		if got, o := parseDim(in); !o || got != want {
			t.Errorf("parseDim(%q) = %d,%v want %d,true", in, got, o, want)
		}
	}
	// ⚠ Absent and unparseable both mean "the sender declared no pixel count",
	// and must NOT come back as 0 — a zero would classify as a beacon.
	for _, in := range []string{"", "  ", "50%", "auto", "-1", "1.5", "100%"} {
		if got, o := parseDim(in); o {
			t.Errorf("parseDim(%q) = %d,true — want not-a-dimension", in, got)
		}
	}
}

func capturePixelReport(a *pixelAggregate, allows []store.RemoteAllow, top int) string {
	var sb strings.Builder
	old := reportOut
	reportOut = &sb
	defer func() { reportOut = old }()
	a.print(allows, top)
	return sb.String()
}

func aggWith(t *testing.T, exposedBy string, imgs ...api.RemoteImage) *pixelAggregate {
	t.Helper()
	a := newPixelAggregate()
	a.scanned = 10
	a.withHTML = 4
	id := ulid.MustParse("01ARZ3NDEKTSV4RRFFQ69G5FAV")
	for _, i := range imgs {
		a.add(id, i, exposedBy)
	}
	return a
}

// TestPixelReportPrintsTheExposedPopulation — the exposed subset is the number
// the increment turns on. If it were computed and not printed, the report would
// look complete and answer a different question than the one asked.
//
// Probed: deleting the exposed-population block reddens this on the named strings.
func TestPixelReportPrintsTheExposedPopulation(t *testing.T) {
	allows := []store.RemoteAllow{{Kind: store.RemoteAllowDomain, Value: "news.example"}}
	out := capturePixelReport(aggWith(t, "domain news.example", img("1", "1", "")), allows, 5)

	for _, want := range []string{
		"the population that can ACTUALLY fetch",
		"messages from allowed senders carrying a remote image : 1",
		"domain news.example",
		"1x1",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the report never printed %q\n--- got ---\n%s", want, out)
		}
	}
}

// TestPixelReportSaysWhenNothingCanFetch — with no grants the exposed population
// is empty BY CONSTRUCTION, and a column of zeros would read as "no tracking
// here". The report has to say which of the two it is (#155: a bounded result
// must report its bound).
//
// Probed: removing the no-grants branch reddens this.
func TestPixelReportSaysWhenNothingCanFetch(t *testing.T) {
	out := capturePixelReport(aggWith(t, "", img("1", "1", "")), nil, 5)
	if !strings.Contains(out, "NO REMOTE-CONTENT GRANTS AT ALL") {
		t.Errorf("with no grants the report must say the population is empty by construction, "+
			"not print zeros\n--- got ---\n%s", out)
	}
	// And it must still have reported the corpus-wide finding, which is real.
	if !strings.Contains(out, "1x1") {
		t.Errorf("the corpus-wide table vanished when there were no grants\n--- got ---\n%s", out)
	}
}

// TestPixelReportReportsItsOwnEmptiness — zero remote images and "the walk found
// nothing to look at" produce the same table of zeros, and only one of them says
// anything about tracking.
//
// Probed: removing the a.images == 0 branch reddens this.
func TestPixelReportReportsItsOwnEmptiness(t *testing.T) {
	out := capturePixelReport(newPixelAggregate(), nil, 5)
	if !strings.Contains(out, "NOT ONE remote image") {
		t.Errorf("an empty corpus must announce its own emptiness\n--- got ---\n%s", out)
	}
}

// TestPixelReportCountsImagesAndMessagesSeparately — one message carrying forty
// beacons and forty messages carrying one are different problems, and a single
// number cannot tell them apart.
//
// Probed: making sigMsgs count images reddens this.
func TestPixelReportCountsImagesAndMessagesSeparately(t *testing.T) {
	out := capturePixelReport(
		aggWith(t, "", img("1", "1", ""), img("1", "1", ""), img("1", "1", "")), nil, 5)
	// three images, one message
	if !strings.Contains(out, "1x1                       3          1") {
		t.Errorf("want 3 images / 1 message on the 1x1 row\n--- got ---\n%s", out)
	}
}
