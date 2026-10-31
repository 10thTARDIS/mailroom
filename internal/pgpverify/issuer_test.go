// SPDX-License-Identifier: AGPL-3.0-only

package pgpverify

import (
	"strings"
	"testing"
)

// The display of "signed with a key you have not seen" names the key the
// SIGNATURE says made it, which is the only thing known when no key is held.
func TestVerifyNamesTheIssuerWhenNoKeyIsHeld(t *testing.T) {
	raw, signer := newSignedMessage(t, "hello\r\n")
	other, otherPub := newSignedMessage(t, "another\r\n")
	_ = other

	res := Verify(raw, []PublicKey{otherPub})
	if res.Verdict != VerdictNoKey {
		t.Fatalf("verdict = %q, want no_key", res.Verdict)
	}
	// newSignedMessage's PublicKey.Fingerprint is not hex, so compare through a
	// verified run: the issuer on the no_key result must name the same key.
	ok := Verify(raw, []PublicKey{signer})
	if ok.Verdict != VerdictVerified {
		t.Fatalf("control: verdict = %q", ok.Verdict)
	}
	if res.IssuerFingerprint == "" {
		t.Fatal("no_key result names no issuer")
	}
	if !strings.HasSuffix(ok.SignerFingerprint, res.IssuerFingerprint[len(res.IssuerFingerprint)-16:]) {
		t.Errorf("issuer %q is not the signing key %q", res.IssuerFingerprint, ok.SignerFingerprint)
	}
	if ok.IssuerFingerprint != res.IssuerFingerprint {
		t.Errorf("the same signature reports two issuers: %q and %q", ok.IssuerFingerprint, res.IssuerFingerprint)
	}
}
