// SPDX-License-Identifier: AGPL-3.0-only

package keypub

import (
	"image"
	"image/color"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/makiuchi-d/gozxing"
	"github.com/makiuchi-d/gozxing/qrcode"
)

var (
	svgViewBox = regexp.MustCompile(`viewBox="0 0 (\d+) (\d+)"`)
	svgPath    = regexp.MustCompile(` d="([^"]*)"`)
	svgRun     = regexp.MustCompile(`M(\d+) (\d+)h(\d+)v1h-(\d+)z`)
)

// decodeQRSVG reads the rendered SVG back the way a scanner would see it: it
// parses the viewBox and the path out of the MARKUP (not out of the encoder),
// paints them into a bitmap at scale pixels per module with a white
// background, and hands that to an independent QR decoder.
func decodeQRSVG(t *testing.T, svg string, scale int) string {
	t.Helper()
	vb := svgViewBox.FindStringSubmatch(svg)
	if vb == nil {
		t.Fatalf("no viewBox in svg: %s", svg)
	}
	size, _ := strconv.Atoi(vb[1])
	pm := svgPath.FindStringSubmatch(svg)
	if pm == nil {
		t.Fatalf("no path data in svg: %s", svg)
	}
	runs := svgRun.FindAllStringSubmatch(pm[1], -1)
	if len(runs) == 0 {
		t.Fatalf("path has no dark runs: %q", pm[1])
	}
	img := image.NewGray(image.Rect(0, 0, size*scale, size*scale))
	for i := range img.Pix {
		img.Pix[i] = 0xff
	}
	for _, r := range runs {
		x, _ := strconv.Atoi(r[1])
		y, _ := strconv.Atoi(r[2])
		w, _ := strconv.Atoi(r[3])
		for px := x * scale; px < (x+w)*scale; px++ {
			for py := y * scale; py < (y+1)*scale; py++ {
				img.SetGray(px, py, color.Gray{Y: 0})
			}
		}
	}
	bin, err := gozxing.NewBinaryBitmapFromImage(img)
	if err != nil {
		t.Fatal(err)
	}
	res, err := qrcode.NewQRCodeReader().Decode(bin, nil)
	if err != nil {
		t.Fatalf("the QR code does not decode: %v", err)
	}
	return res.GetText()
}

func TestQRDecodesToTheExactURI(t *testing.T) {
	const want = "OPENPGP4FPR:" + knownFingerprint
	svg := string(fingerprintQRSVG(knownFingerprint))
	if got := decodeQRSVG(t, svg, 4); got != want {
		t.Errorf("decoded %q, want %q", got, want)
	}
	// A lowercase fingerprint is normalised to the uppercase URI.
	if got := decodeQRSVG(t, string(fingerprintQRSVG(strings.ToLower(knownFingerprint))), 4); got != want {
		t.Errorf("lowercase input decoded %q, want %q", got, want)
	}
}

// TestQRDecodeTestCanFail is the negative control for the test above: a code
// built for a different fingerprint must NOT decode to this one's URI. Without
// it, a decoder that returned a constant would pass.
func TestQRDecodeTestCanFail(t *testing.T) {
	other := "0123456789ABCDEF0123456789ABCDEF01234567"
	got := decodeQRSVG(t, string(fingerprintQRSVG(other)), 4)
	if got == "OPENPGP4FPR:"+knownFingerprint {
		t.Fatalf("a different fingerprint decoded to this one's URI: %q", got)
	}
	if got != "OPENPGP4FPR:"+other {
		t.Errorf("decoded %q, want the other fingerprint's URI", got)
	}
}

func TestQRRefusesNonHex(t *testing.T) {
	for _, f := range []string{"", "ZZZZ", "AAAA 1111", "AAAA\"><script>", "<svg onload=x>"} {
		if got := fingerprintQRSVG(f); got != "" {
			t.Errorf("fingerprintQRSVG(%q) = %q, want no QR code", f, got)
		}
	}
}

func TestQRMarkupIsInertAndLabelled(t *testing.T) {
	svg := string(fingerprintQRSVG(knownFingerprint))
	for _, want := range []string{`role="img"`, `aria-label="QR code of the key fingerprint"`, `viewBox=`} {
		if !strings.Contains(svg, want) {
			t.Errorf("svg lacks %s", want)
		}
	}
	for _, banned := range []string{"<script", "<style", " style=", "href", "onload", "<image", "<foreignObject"} {
		if strings.Contains(strings.ToLower(svg), strings.ToLower(banned)) {
			t.Errorf("svg contains %q", banned)
		}
	}
}

func TestPageShowsQRBesideTheTextFingerprintWithoutScript(t *testing.T) {
	fake := &pageFakeLookup{address: "joe.doe@example.org", fingerprint: knownFingerprint, found: true}
	svc, err := New(fake, "example.org", testLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	rec := httptest.NewRecorder()
	svc.PageHandler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/key/"+validHash1, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	body := rec.Body.String()
	if strings.Count(body, "<svg") != 1 {
		t.Errorf("want exactly one inline svg, body:\n%s", body)
	}
	if !strings.Contains(body, "AAAA 1111 BBBB 2222 CCCC  3333 DDDD 4444 EEEE 5555") {
		t.Error("the grouped text fingerprint is gone")
	}
	for _, banned := range []string{"<script", "javascript:", "http://", " src=", "url("} {
		if strings.Contains(strings.ToLower(body), banned) {
			t.Errorf("page contains %q; it must stay script-free and fetch nothing", banned)
		}
	}
	// And the QR in the served page is the one that decodes to the URI.
	start := strings.Index(body, "<svg")
	end := strings.Index(body, "</svg>") + len("</svg>")
	if got := decodeQRSVG(t, body[start:end], 4); got != "OPENPGP4FPR:"+knownFingerprint {
		t.Errorf("page QR decoded %q", got)
	}
}

func TestPageWithoutAUsableFingerprintHasNoQR(t *testing.T) {
	fake := &pageFakeLookup{address: "joe.doe@example.org", fingerprint: "not hex", found: true}
	svc, _ := New(fake, "example.org", testLogger())
	rec := httptest.NewRecorder()
	svc.PageHandler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/key/"+validHash1, nil))
	if rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), "<svg") {
		t.Errorf("status %d, want 200 and no svg", rec.Code)
	}
}
