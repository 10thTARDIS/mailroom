// SPDX-License-Identifier: AGPL-3.0-only

package api

import (
	"bytes"
	"testing"
	"time"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/ProtonMail/go-crypto/openpgp/packet"

	"mailroom/internal/pgpverify"
	"mailroom/internal/store"
)

// classifySignature is the decision table; the integration tests prove the facts
// it is given are gathered correctly, this proves what it does with them.
func TestClassifySignature(t *testing.T) {
	const (
		oldFp = "AAAA"
		newFp = "BBBB"
	)
	verified := pgpverify.Result{Verdict: pgpverify.VerdictVerified, SignerFingerprint: newFp}
	held := func(source string, auth, refused, names bool) *heldSigner {
		return &heldSigner{key: store.ContactKey{Fingerprint: newFp, Source: source, Authenticated: auth, Refused: refused}, names: names}
	}
	cases := []struct {
		name      string
		f         sigFacts
		want      string // "" = nil
		wantPrev  string
		pinFirst  bool
		wantRefus bool
	}{
		{"unsigned", sigFacts{res: pgpverify.Result{Verdict: pgpverify.VerdictNotSigned}}, "", "", false, false},
		{"bad", sigFacts{res: pgpverify.Result{Verdict: pgpverify.VerdictBadSignature}}, sigBad, "", false, false},
		{"malformed is bad", sigFacts{res: pgpverify.Result{Verdict: pgpverify.VerdictMalformed}}, sigBad, "", false, false},
		{"no key", sigFacts{res: pgpverify.Result{Verdict: pgpverify.VerdictNoKey, IssuerFingerprint: "CCCC"}}, sigUnknownKey, "", false, false},
		{"unknown verdict is never good", sigFacts{res: pgpverify.Result{Verdict: "future"}}, sigBad, "", false, false},
		{"own key", sigFacts{res: verified, signer: &heldSigner{own: true, names: true}}, sigVerified, "", false, false},
		{"own key for another address", sigFacts{res: verified, signer: &heldSigner{own: true}}, sigUnknownKey, "", false, false},
		{"eligible, no pin: verified and pin it",
			sigFacts{res: verified, signer: held(store.ContactKeyWKD, false, false, true)}, sigVerified, "", true, false},
		{"eligible, pinned to it",
			sigFacts{res: verified, signer: held(store.ContactKeyWKD, false, false, true), pin: newFp, hasPin: true}, sigVerified, "", false, false},
		{"does not name the address, even if pinned and imported",
			sigFacts{res: verified, signer: held(store.ContactKeyManual, false, false, false), pin: newFp, hasPin: true}, sigUnknownKey, "", false, false},
		{"differs from the pin",
			sigFacts{res: verified, signer: held(store.ContactKeyWKD, false, false, true), pin: oldFp, hasPin: true}, sigKeyChanged, oldFp, false, false},
		{"differs from the pin but imported",
			sigFacts{res: verified, signer: held(store.ContactKeyManual, false, false, true), pin: oldFp, hasPin: true}, sigVerified, "", false, false},
		{"unauthenticated autocrypt, nothing to compare",
			sigFacts{res: verified, signer: held(store.ContactKeyAutocrypt, false, false, true)}, sigUnknownKey, "", false, false},
		{"unauthenticated autocrypt against an eligible key",
			sigFacts{res: verified, signer: held(store.ContactKeyAutocrypt, false, false, true), other: oldFp}, sigKeyChanged, oldFp, false, false},
		{"refused, with a pin",
			sigFacts{res: verified, signer: held(store.ContactKeyWKD, false, true, true), pin: oldFp, hasPin: true}, sigKeyChanged, oldFp, false, true},
		{"refused, nothing to compare",
			sigFacts{res: verified, signer: held(store.ContactKeyWKD, false, true, true)}, sigUnknownKey, "", false, false},
		{"no held key found for a verified fingerprint", sigFacts{res: verified}, sigUnknownKey, "", false, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, pinFirst := classifySignature(c.f)
			if c.want == "" {
				if got != nil {
					t.Fatalf("got %+v, want nil", got)
				}
				return
			}
			if got == nil || got.State != c.want {
				t.Fatalf("got %+v, want state %s", got, c.want)
			}
			if got.PreviousFingerprint != c.wantPrev {
				t.Errorf("previous = %q, want %q", got.PreviousFingerprint, c.wantPrev)
			}
			if pinFirst != c.pinFirst {
				t.Errorf("pinFirst = %v, want %v", pinFirst, c.pinFirst)
			}
			if got.Refused != c.wantRefus {
				t.Errorf("refused = %v, want %v", got.Refused, c.wantRefus)
			}
		})
	}
}

// embeddedVerdict maps the crypto process's section 6.2 result onto the verdicts
// classifySignature decides from; an unknown status is never good news.
func TestEmbeddedVerdict(t *testing.T) {
	cases := []struct {
		in   EmbeddedSignature
		want pgpverify.Verdict
		fp   string
	}{
		{EmbeddedSignature{}, pgpverify.VerdictNotSigned, ""},
		{EmbeddedSignature{Signed: true, Status: EmbeddedVerified, SignerFingerprint: "AAAA", Issuer: "AAAA"}, pgpverify.VerdictVerified, "AAAA"},
		{EmbeddedSignature{Signed: true, Status: EmbeddedVerified}, pgpverify.VerdictBadSignature, ""}, // verified with no signer is not believed
		{EmbeddedSignature{Signed: true, Status: EmbeddedNoKey, Issuer: "CCCC"}, pgpverify.VerdictNoKey, ""},
		{EmbeddedSignature{Signed: true, Status: EmbeddedBad}, pgpverify.VerdictBadSignature, ""},
		{EmbeddedSignature{Signed: true, Status: "future"}, pgpverify.VerdictBadSignature, ""},
		{EmbeddedSignature{Signed: true}, pgpverify.VerdictBadSignature, ""},
	}
	for _, c := range cases {
		got := embeddedVerdict(c.in)
		if got.Verdict != c.want || got.SignerFingerprint != c.fp {
			t.Errorf("embeddedVerdict(%+v) = %+v, want %s %q", c.in, got, c.want, c.fp)
		}
	}
	if r := embeddedVerdict(EmbeddedSignature{Signed: true, Status: EmbeddedNoKey, Issuer: "CCCC"}); r.IssuerFingerprint != "CCCC" {
		t.Errorf("noKey lost its issuer: %+v", r)
	}
	// Through the table: a noKey result is unknownKey naming the issuer.
	sig, _ := classifySignature(sigFacts{res: embeddedVerdict(EmbeddedSignature{Signed: true, Status: EmbeddedNoKey, Issuer: "CCCC"})})
	if sig == nil || sig.State != sigUnknownKey || sig.Fingerprint != "CCCC" {
		t.Errorf("noKey classified as %+v", sig)
	}
}

func testPub(t *testing.T, email string) []byte {
	t.Helper()
	e, err := openpgp.NewEntity("T", "", email, &packet.Config{Algorithm: packet.PubKeyAlgoEdDSA, Curve: packet.Curve25519})
	if err != nil {
		t.Fatal(err)
	}
	var b bytes.Buffer
	if err := e.Serialize(&b); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// embeddedCandidates: the pinned key first, eligible keys by rank, own keys naming
// the address, ineligible keys, other own keys; bounded by count and bytes.
func TestEmbeddedCandidatesOrderAndBounds(t *testing.T) {
	ck := func(fp, source string, auth, refused bool, pub []byte) store.ContactKey {
		return store.ContactKey{Kind: store.KeyPGP, Fingerprint: fp, Source: source, Authenticated: auth, Refused: refused, PublicKey: pub}
	}
	pinned, wkd, manual, unauth, refused := []byte("pinned"), []byte("wkd"), []byte("manual"), []byte("unauth"), []byte("refused")
	ownNaming, ownOther := testPub(t, "alice@example.com"), testPub(t, "me@example.com")
	k := &sigKeys{
		from: "alice@example.com", now: time.Now(), pin: "P", hasPin: true,
		contactKeys: []store.ContactKey{
			ck("U", store.ContactKeyAutocrypt, false, false, unauth),
			ck("W", store.ContactKeyWKD, false, false, wkd),
			ck("R", store.ContactKeyManual, false, true, refused),
			ck("M", store.ContactKeyManual, false, false, manual),
			ck("P", store.ContactKeyAutocrypt, false, false, pinned), // the pin, even though ineligible
		},
		ownKeys: []store.UserKey{{Fingerprint: "O2", PublicKey: ownOther}, {Fingerprint: "O1", PublicKey: ownNaming}},
	}
	got := k.embeddedCandidates()
	want := [][]byte{pinned, manual, wkd, ownNaming, unauth, refused, ownOther}
	if len(got) != len(want) {
		t.Fatalf("got %d candidates, want %d", len(got), len(want))
	}
	for i := range want {
		if !bytes.Equal(got[i], want[i]) {
			t.Errorf("candidate %d = %.12q, want %.12q", i, got[i], want[i])
		}
	}

	// The count bound.
	many := &sigKeys{from: "alice@example.com", now: time.Now()}
	for i := 0; i < MaxEmbeddedSigners+5; i++ {
		many.contactKeys = append(many.contactKeys, ck(string(rune('a'+i)), store.ContactKeyWKD, false, false, []byte{byte(i)}))
	}
	if n := len(many.embeddedCandidates()); n != MaxEmbeddedSigners {
		t.Errorf("count bound: %d candidates, want %d", n, MaxEmbeddedSigners)
	}
	// The byte bound: a key that would overflow is skipped, a later small one kept.
	big := bytes.Repeat([]byte{1}, MaxEmbeddedSignerBytes-10)
	sized := &sigKeys{from: "alice@example.com", now: time.Now(), contactKeys: []store.ContactKey{
		ck("A", store.ContactKeyManual, false, false, big),
		ck("B", store.ContactKeyManual, false, false, bytes.Repeat([]byte{2}, 20)),
		ck("C", store.ContactKeyManual, false, false, []byte("small")),
	}}
	out := sized.embeddedCandidates()
	if len(out) != 2 || !bytes.Equal(out[0], big) || string(out[1]) != "small" {
		t.Errorf("byte bound: got %d candidates", len(out))
	}
}
