// SPDX-License-Identifier: AGPL-3.0-only

package ingest

import (
	"testing"
	"time"

	"mailroom/internal/mime"
)

// A forged, untrusted `dmarc=pass` above a trusted `dmarc=fail` cancels the
// all-headers failure flag (AuthResultsFail), but the TRUSTED header's failure
// must still be seen, because a trusted sender's waiver is refused on it.
func TestLearnedSenderSeesATrustedFailBesideAForgedPass(t *testing.T) {
	raw := "Authentication-Results: evil.example; dmarc=pass header.from=example.org; dkim=pass header.d=example.org\r\n" +
		"Authentication-Results: mx.example; dmarc=fail header.from=example.org; spf=pass\r\n" +
		"From: robot@example.org\r\nTo: you@example.net\r\nSubject: s\r\n\r\nbody\r\n"
	parsed, err := mime.ParseBytes([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	s := LearnedSender("robot@example.org", parsed, []string{"mx.example"}, time.Now())
	a := s.Auth()
	if !a.TrustedFail {
		t.Fatalf("auth = %+v, want TrustedFail (the trusted header says dmarc=fail)", a)
	}
	// Documents WHY the all-headers flag is not enough: the forged pass cancels it.
	if a.AuthFail {
		t.Logf("AuthFail is set here; the regression is still guarded by TrustedFail")
	}
	if a.Authenticated() {
		t.Fatalf("a trusted dmarc=fail must not read as authenticated: %+v", a)
	}

	// Control: the same message with a trusted pass has no TrustedFail.
	ok := "Authentication-Results: mx.example; dmarc=pass header.from=example.org\r\n" +
		"From: robot@example.org\r\n\r\nbody\r\n"
	p2, _ := mime.ParseBytes([]byte(ok))
	if a2 := LearnedSender("robot@example.org", p2, []string{"mx.example"}, time.Now()).Auth(); a2.TrustedFail || !a2.DMARCPass {
		t.Fatalf("control auth = %+v, want a pass and no TrustedFail", a2)
	}
	// Both SPF and DKIM failing in the trusted header counts too.
	both := "Authentication-Results: mx.example; spf=fail; dkim=fail header.d=example.org\r\nFrom: robot@example.org\r\n\r\nb\r\n"
	p3, _ := mime.ParseBytes([]byte(both))
	if a3 := LearnedSender("robot@example.org", p3, []string{"mx.example"}, time.Now()).Auth(); !a3.TrustedFail {
		t.Fatalf("spf+dkim fail: %+v, want TrustedFail", a3)
	}
}
