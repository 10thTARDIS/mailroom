// SPDX-License-Identifier: AGPL-3.0-only

package pgpkeyd

// The publish paths of a certified key (ADR-054 section 1): the certification is
// in the STORED bytes from the first byte, and every way a correspondent can get
// the key serves those bytes. Together with the merge exception these are the
// end-to-end proof that a Mailroom-to-Mailroom rotation is vouched: the bytes a
// correspondent receives, fed to pgpkeys.Merge with the old key as certifier, keep
// the certification; fed with no certifier, they lose it.
//
// The key page's download link IS the WKD path (keypub/page.go), so the page test
// follows the link it renders and asserts what the WKD handler serves there.

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"mailroom/internal/autocrypt"
	"mailroom/internal/keypub"
	"mailroom/internal/pgpkeys"
	"mailroom/internal/wkd"
)

type fixedKeyLookup struct {
	key  []byte
	addr string
	fpr  string
}

func (f fixedKeyLookup) LookupWKDKey(context.Context, string, string) ([]byte, string, bool, error) {
	return f.key, f.fpr, true, nil
}

func (f fixedKeyLookup) LookupWKDKeyPage(context.Context, string, string) (string, string, bool, error) {
	return f.addr, f.fpr, true, nil
}

// certificationSurvivesOnlyWithTheOldKey is the shared assertion: got carries the
// certification, it survives the merge when the old key is a certifier, and is
// dropped when no certifier is given.
func certificationSurvivesOnlyWithTheOldKey(t *testing.T, what string, got, oldPub []byte, addr string) {
	t.Helper()
	if !pgpkeys.CertifiedBy(got, oldPub, addr) {
		t.Errorf("%s: carries no certification verifying under the old key", what)
		return
	}
	// A correspondent already holds the new key without the certification (say,
	// from an earlier sighting) and now meets this copy.
	bare, err := bareCopy(got)
	if err != nil {
		t.Fatal(err)
	}
	merged, _, err := pgpkeys.Merge(bare, got, oldPub)
	if err != nil || !pgpkeys.CertifiedBy(merged, oldPub, addr) {
		t.Errorf("%s: the certification did not survive a merge with the old key as certifier (err=%v)", what, err)
	}
	merged, _, err = pgpkeys.Merge(bare, got)
	if err != nil || pgpkeys.CertifiedBy(merged, oldPub, addr) {
		t.Errorf("%s: with NO certifier the certification should have been dropped (err=%v)", what, err)
	}
}

// bareCopy is the key without third-party certifications: Merge with itself and
// no certifiers normalises exactly that.
func bareCopy(pub []byte) ([]byte, error) {
	m, _, err := pgpkeys.Merge(pub, pub)
	return m, err
}

func TestCertificationIsOnEveryPublishPath(t *testing.T) {
	h := startDaemon(t)
	user := newUserID()
	const addr = "rot@example.com"
	old := certifyGen(t, h, user, addr, "")
	res := certifyGen(t, h, user, addr, old.Key.KeyID)
	if !res.Certified {
		t.Fatal("fixture: not certified")
	}
	stored := storedPublic(t, h, res.Key.KeyID)
	oldPub := storedPublic(t, h, old.Key.KeyID)

	t.Run("stored bytes", func(t *testing.T) {
		certificationSurvivesOnlyWithTheOldKey(t, "stored", stored, oldPub, addr)
	})

	svc, err := keypub.New(fixedKeyLookup{key: stored, addr: addr, fpr: res.Key.Fingerprint}, "example.com",
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	hu := func() []byte {
		rec := httptest.NewRecorder()
		svc.HUHandler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, wkd.DirectPath("rot"), nil))
		if rec.Code != 200 {
			t.Fatalf("WKD status %d", rec.Code)
		}
		return rec.Body.Bytes()
	}

	t.Run("WKD body", func(t *testing.T) {
		certificationSurvivesOnlyWithTheOldKey(t, "WKD body", hu(), oldPub, addr)
	})

	t.Run("outgoing Autocrypt header, decoded", func(t *testing.T) {
		hdr := autocrypt.Header(addr, stored, true)
		peer, ok, err := autocrypt.Parse([]string{strings.TrimPrefix(hdr, "Autocrypt: ")})
		if err != nil || !ok {
			t.Fatalf("our own header does not parse: ok=%v err=%v\n%s", ok, err, hdr)
		}
		certificationSurvivesOnlyWithTheOldKey(t, "Autocrypt header", peer.KeyData, oldPub, addr)
	})

	t.Run("key page download link", func(t *testing.T) {
		rec := httptest.NewRecorder()
		svc.PageHandler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/key/"+wkd.HashLocalPart("rot"), nil))
		m := regexp.MustCompile(`class="download" href="([^"]+)"`).FindStringSubmatch(rec.Body.String())
		if m == nil {
			t.Fatalf("no download link on the key page:\n%s", rec.Body.String())
		}
		if m[1] != wkd.DirectPath("rot") {
			t.Fatalf("the download link is %q, not the WKD path", m[1])
		}
		// Following the link is the WKD handler's answer.
		certificationSurvivesOnlyWithTheOldKey(t, "key page download", hu(), oldPub, addr)
	})
}
