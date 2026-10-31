// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package ingest

import (
	"context"
	"errors"
	"testing"

	"mailroom/internal/autocrypt"
	"mailroom/internal/dkimverify/dkimtest"
)

// #47 (ADR-038): an Autocrypt key may be encrypted to only if the message that
// carried it was authenticated for its From domain. This is the ingest half: the
// `authenticated` column on app.contact_keys. The resolution half (ignore the
// rows where it is false) is TestEncryptedSubmitIgnoresUnauthenticatedAutocrypt.
func TestAutocryptKeyRecordsWhetherTheMessageWasAuthenticated(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	key := dkimtest.NewKey(t)
	dns := func(_ context.Context, name string) ([]string, error) {
		if name == dkimtest.Name("example.com", "s1") {
			return []string{key.Record()}, nil
		}
		return nil, errors.New("servfail")
	}
	const trusted = "mx.provider.example"
	ing := New(f.st, f.ing.blobs, WithTrustedAuthservIDs([]string{trusted}), WithLocalDKIM(nil), withDKIMLookup(dns))

	authenticatedOf := func(fpr string) int {
		return f.scalar(t, `SELECT count(*) FROM app.contact_keys WHERE fingerprint=$1 AND authenticated`, fpr)
	}
	stored := func(fpr string) int {
		return f.scalar(t, `SELECT count(*) FROM app.contact_keys WHERE fingerprint=$1 AND source='autocrypt'`, fpr)
	}
	uid := int64(1000)
	send := func(from, name, id string, extra ...string) string {
		t.Helper()
		pub, fpr := autocryptKey(t, name, from)
		hdr := autocrypt.HeaderName + ": " + autocrypt.Header(from, pub, false)
		raw := rawFrom(name+" <"+from+">", "hi "+id, id+"@example.com", append([]string{hdr}, extra...)...)
		uid++
		if _, err := ing.Ingest(ctx, f.userID, f.dek, f.dekID, raw, f.prov(f.mailboxA, uid, nil)); err != nil {
			t.Fatal(err)
		}
		return fpr
	}

	// 1. A forged-From message: no Authentication-Results, no signature. The key
	// is STORED (it is evidence) but NOT authenticated.
	forged := send("alice@forged.example", "Alice", "forged")
	if stored(forged) != 1 || authenticatedOf(forged) != 0 {
		t.Errorf("forged: stored=%d authenticated=%d, want 1 and 0", stored(forged), authenticatedOf(forged))
	}

	// 2. A DMARC pass bound to the From domain under a trusted authserv-id.
	dmarc := send("bob@bank.example", "Bob", "dmarc",
		"Authentication-Results: "+trusted+"; dkim=pass header.d=bank.example; dmarc=pass header.from=bank.example")
	if authenticatedOf(dmarc) != 1 {
		t.Error("a DMARC pass for the From domain under a trusted authserv-id did not authenticate the key")
	}

	// 3. A DMARC pass for ANOTHER domain than the From (the forged-From shape
	// with a genuinely passing unrelated domain) does not.
	otherDomain := send("carol@victim.example", "Carol", "otherdom",
		"Authentication-Results: "+trusted+"; dmarc=pass header.from=attacker.example")
	if stored(otherDomain) != 1 || authenticatedOf(otherDomain) != 0 {
		t.Errorf("a dmarc pass for another domain authenticated the key (stored=%d authenticated=%d)",
			stored(otherDomain), authenticatedOf(otherDomain))
	}

	// 4. The same DMARC pass under an authserv-id nobody trusts does not.
	untrusted := send("dave@bank.example", "Dave", "untrusted",
		"Authentication-Results: evil.example; dmarc=pass header.from=bank.example")
	if authenticatedOf(untrusted) != 0 {
		t.Error("a dmarc pass from an UNTRUSTED authserv-id authenticated the key")
	}

	// 4b. #846 relaxed DMARC alignment for LEARNED PLACEMENT only: a pass for
	// the From domain's organisational domain does NOT authenticate an
	// Autocrypt key (this caller stays ml.DMARCStrict). NEGATIVE CONTROL:
	// autocryptAuthenticated switched to ml.DMARCRelaxed turns this red.
	orgOnly := send("erin@news.bank.example", "Erin", "orgonly",
		"Authentication-Results: "+trusted+"; dkim=pass header.d=bank.example; dmarc=pass header.from=bank.example")
	if stored(orgOnly) != 1 || authenticatedOf(orgOnly) != 0 {
		t.Errorf("an org-domain pass authenticated a subdomain sender's key (stored=%d authenticated=%d), want strict",
			stored(orgOnly), authenticatedOf(orgOnly))
	}

	// 5. An aligned local DKIM pass (no trusted header at all).
	pub, fpr := autocryptKey(t, "Erin", "erin@example.com")
	hdr := autocrypt.HeaderName + ": " + autocrypt.Header("erin@example.com", pub, false)
	signed := key.Sign(t, string(rawFrom("Erin <erin@example.com>", "dkim", "dkim@example.com", hdr)), "example.com", "s1")
	if _, err := ing.Ingest(ctx, f.userID, f.dek, f.dekID, []byte(signed), f.prov(f.mailboxA, 2000, nil)); err != nil {
		t.Fatal(err)
	}
	if stored(fpr) != 1 || authenticatedOf(fpr) != 1 {
		t.Errorf("aligned local DKIM: stored=%d authenticated=%d, want 1 and 1", stored(fpr), authenticatedOf(fpr))
	}

	// 6. A local DKIM pass for a DIFFERENT domain than the From is not aligned.
	pub2, fpr2 := autocryptKey(t, "Frank", "frank@victim.example")
	hdr2 := autocrypt.HeaderName + ": " + autocrypt.Header("frank@victim.example", pub2, false)
	signedOther := key.Sign(t, string(rawFrom("Frank <frank@victim.example>", "dkim2", "dkim2@example.com", hdr2)), "example.com", "s1")
	if _, err := ing.Ingest(ctx, f.userID, f.dek, f.dekID, []byte(signedOther), f.prov(f.mailboxA, 2001, nil)); err != nil {
		t.Fatal(err)
	}
	if stored(fpr2) != 1 || authenticatedOf(fpr2) != 0 {
		t.Errorf("a non-aligned DKIM pass authenticated the key (stored=%d authenticated=%d)", stored(fpr2), authenticatedOf(fpr2))
	}
}
