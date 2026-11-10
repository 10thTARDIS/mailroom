// SPDX-License-Identifier: AGPL-3.0-only

package api

import (
	"strings"
	"testing"
)

const bgURL = "https://news.example/img/bg.png?u=TOKEN"

// A remote background= on a table cell is parked under its own name, never kept
// as background= and never turned into CSS (#26).
func TestBackgroundAttrIsParkedNotKept(t *testing.T) {
	for _, in := range []string{
		`<table background="` + bgURL + `"><tr><td>x</td></tr></table>`,
		`<table><tr><td background="` + bgURL + `">x</td></tr></table>`,
		`<table><tr><th background="` + bgURL + `">x</th></tr></table>`,
		`<table><tr background="` + bgURL + `"><td>x</td></tr></table>`,
	} {
		out := SanitizeHTML(in)
		if !strings.Contains(out, remoteBackgroundAttr+`="`+bgURL+`"`) {
			t.Errorf("not parked:\n in: %s\nout: %s", in, out)
		}
		if !strings.Contains(out, blockedAttr) {
			t.Errorf("not marked blocked: %s", out)
		}
		assertNoLiveBackground(t, out)
	}
}

// assertNoLiveBackground: nothing in the output could fetch by itself.
func assertNoLiveBackground(t *testing.T, out string) {
	t.Helper()
	for _, bad := range []string{` background=`, "url(", "style="} {
		if strings.Contains(out, bad) {
			t.Errorf("output holds %q, which could load an image on its own: %s", bad, out)
		}
	}
}

func TestBackgroundAttrThroughTheProxyNeverShowsTheSendersURL(t *testing.T) {
	acct, msg := testIDs()
	in := `<table><tr><td background="` + bgURL + `">x</td></tr></table>`
	out := sanitizeHTMLWithProxy(in, proxyRewriterFor(acct, msg))
	if strings.Contains(out, "news.example") || strings.Contains(out, "TOKEN") {
		t.Errorf("sender URL reached the client: %s", out)
	}
	if want := proxyPath(acct, msg, ProxyTokenFor(bgURL)); !strings.Contains(out, remoteBackgroundAttr+`="`+want+`"`) {
		t.Errorf("proxy path not parked under %s: %s", remoteBackgroundAttr, out)
	}
	// The resolver finds it: it is proxy-eligible, i.e. the token resolves.
	_, imgs := MeasureHTMLRemoteImages(in)
	if len(imgs) != 1 || imgs[0].URL != bgURL {
		t.Errorf("background not among the remote images the proxy may serve: %+v", imgs)
	}
}

func TestBackgroundAttrNegativeControls(t *testing.T) {
	acct, msg := testIDs()
	px := proxyRewriterFor(acct, msg)
	cases := map[string]string{
		"beacon cell":     `<table><tr><td width="1" height="1" background="` + bgURL + `">x</td></tr></table>`,
		"javascript":      `<table><tr><td background="javascript:alert(1)">x</td></tr></table>`,
		"data html":       `<table><tr><td background="data:text/html,x">x</td></tr></table>`,
		"relative":        `<table><tr><td background="/img/bg.png">x</td></tr></table>`,
		"not a carrier":   `<div background="` + bgURL + `">x</div>`,
		"span carrier":    `<p><span background="` + bgURL + `">x</span></p>`,
		"forged parked":   `<table><tr><td data-mailroom-remote-background="` + bgURL + `">x</td></tr></table>`,
		"forged restored": `<table><tr><td data-mailroom-remote-src="` + bgURL + `">x</td></tr></table>`,
	}
	for name, in := range cases {
		for _, out := range []string{SanitizeHTML(in), sanitizeHTMLWithProxy(in, px)} {
			if strings.Contains(out, remoteBackgroundAttr) || strings.Contains(out, remoteSrcAttr) {
				t.Errorf("%s: a restorable parking was produced: %s", name, out)
			}
			assertNoLiveBackground(t, out)
		}
	}
	// Outbound: dropped.
	out := SanitizeOutboundHTML(`<table><tr><td background="` + bgURL + `">x</td></tr></table>`)
	if strings.Contains(out, "news.example") || strings.Contains(out, "background") {
		t.Errorf("outbound kept a background: %s", out)
	}
}

// A beacon cell's background is parked under the pixel name, which nothing
// restores, and is never rewritten to the proxy.
func TestBackgroundAttrBeaconIsParkedUnderThePixelName(t *testing.T) {
	acct, msg := testIDs()
	in := `<table><tr><td width="1" height="1" background="` + bgURL + `">x</td></tr></table>`
	out := sanitizeHTMLWithProxy(in, proxyRewriterFor(acct, msg))
	if !strings.Contains(out, pixelSrcAttr+`="`+bgURL+`"`) {
		t.Errorf("beacon background not under the pixel name: %s", out)
	}
	if strings.Contains(out, "/jmap/proxy/") {
		t.Errorf("a beacon was proxied: %s", out)
	}
}

// <picture> (#26): the <source> elements are dropped, so no remote <source
// srcset> can be fetched by anything; the fallback <img> goes the normal way.
func TestPictureKeepsTheImgAndDropsEverySource(t *testing.T) {
	acct, msg := testIDs()
	in := `<picture><source srcset="https://news.example/a.webp 1x, https://news.example/b.webp 2x" type="image/webp">` +
		`<source src="https://news.example/c.png"><img src="https://news.example/a.png" alt="pic"></picture>`
	for _, out := range []string{SanitizeHTML(in), sanitizeHTMLWithProxy(in, proxyRewriterFor(acct, msg))} {
		if strings.Contains(out, "<source") || strings.Contains(out, "srcset") || strings.Contains(out, "a.webp") || strings.Contains(out, "b.webp") {
			t.Errorf("a <source> survived: %s", out)
		}
		if !strings.Contains(out, remoteSrcAttr+`="`) || !strings.Contains(out, `alt="pic"`) {
			t.Errorf("the fallback <img> did not go through the normal path: %s", out)
		}
	}
}
