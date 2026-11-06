// SPDX-License-Identifier: AGPL-3.0-only

package keypub

import (
	"fmt"
	"html/template"
	"image"
	"strings"

	"github.com/boombuler/barcode"
	"github.com/boombuler/barcode/qr"
)

// qrQuietZone is the blank border, in modules, around the symbol. The QR
// specification asks for four; a scanner that finds less may not lock on.
const qrQuietZone = 4

// fingerprintURI is the string the QR code carries: the OpenPGP4FPR URI that
// OpenKeychain, Kleopatra and other clients recognise. The scheme and the hex
// are both uppercase, which also lets the encoder pick the compact
// alphanumeric mode (a QR code of lowercase hex would need byte mode and a
// bigger symbol). It returns ok=false for anything that is not a plain hex
// fingerprint, so a malformed database value never becomes a QR code that
// scans as something other than what the text beside it says.
func fingerprintURI(fpr string) (string, bool) {
	if fpr == "" {
		return "", false
	}
	up := strings.ToUpper(fpr)
	for i := 0; i < len(up); i++ {
		c := up[i]
		if (c < '0' || c > '9') && (c < 'A' || c > 'F') {
			return "", false
		}
	}
	return "OPENPGP4FPR:" + up, true
}

// fingerprintQRSVG renders the QR code of the key's OPENPGP4FPR URI as one
// inline SVG: a single <path> of horizontal runs of dark modules, on a white
// background with a quiet zone, with role="img" and an accessible name. It
// returns "" when there is nothing safe to encode.
//
// The result is template.HTML on purpose, and safe to be: every byte of it is
// produced here from integers and fixed strings. The fingerprint reaches the
// encoder only, never the markup. An inline <svg> is markup, not script and not
// a style sheet, so the page's CSP is unchanged; colours are SVG presentation
// attributes rather than a style attribute.
func fingerprintQRSVG(fpr string) template.HTML {
	uri, ok := fingerprintURI(fpr)
	if !ok {
		return ""
	}
	code, err := qr.Encode(uri, qr.M, qr.Auto)
	if err != nil {
		return ""
	}
	b := code.Bounds()
	n := b.Dx()
	if n <= 0 || b.Dy() != n {
		return ""
	}
	// qr.Encode returns a bare symbol, one pixel per module.
	size := n + 2*qrQuietZone

	var path strings.Builder
	for y := 0; y < n; y++ {
		for x := 0; x < n; {
			if !dark(code, b, x, y) {
				x++
				continue
			}
			start := x
			for x < n && dark(code, b, x, y) {
				x++
			}
			fmt.Fprintf(&path, "M%d %dh%dv1h-%dz", start+qrQuietZone, y+qrQuietZone, x-start, x-start)
		}
	}

	return template.HTML(fmt.Sprintf(
		`<svg class="qr" role="img" aria-label="QR code of the key fingerprint" viewBox="0 0 %d %d" width="%d" height="%d" shape-rendering="crispEdges">`+
			`<rect width="%d" height="%d" fill="#fff"/><path fill="#000" d="%s"/></svg>`,
		size, size, size*4, size*4, size, size, path.String()))
}

// dark reports whether module (x, y) of the symbol is dark. The encoder's
// barcode is a grayscale image: black modules, white elsewhere.
func dark(c barcode.Barcode, b image.Rectangle, x, y int) bool {
	r, g, bl, _ := c.At(b.Min.X+x, b.Min.Y+y).RGBA()
	return r == 0 && g == 0 && bl == 0
}
