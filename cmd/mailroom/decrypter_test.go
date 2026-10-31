// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"testing"

	"mailroom/internal/api"
	"mailroom/internal/pgpwire"
)

// The API trims candidate signer keys to its own copy of the wire's bounds (it
// does not import the wire); a drift would make the crypto process refuse every
// decrypt of mail from a correspondent with many keys.
func TestEmbeddedSignerBoundsMatchTheWire(t *testing.T) {
	if api.MaxEmbeddedSigners != pgpwire.MaxDecryptSigners {
		t.Errorf("api.MaxEmbeddedSigners = %d, pgpwire.MaxDecryptSigners = %d", api.MaxEmbeddedSigners, pgpwire.MaxDecryptSigners)
	}
	if api.MaxEmbeddedSignerBytes != pgpwire.MaxDecryptSignerBytes {
		t.Errorf("api.MaxEmbeddedSignerBytes = %d, pgpwire.MaxDecryptSignerBytes = %d", api.MaxEmbeddedSignerBytes, pgpwire.MaxDecryptSignerBytes)
	}
	for _, s := range []struct{ api, wire string }{
		{api.EmbeddedVerified, string(pgpwire.EmbeddedVerified)},
		{api.EmbeddedBad, string(pgpwire.EmbeddedBad)},
		{api.EmbeddedNoKey, string(pgpwire.EmbeddedNoKey)},
	} {
		if s.api != s.wire {
			t.Errorf("status %q in the API is %q on the wire", s.api, s.wire)
		}
	}
}

func TestEmbeddedFromWire(t *testing.T) {
	got := embeddedFromWire(pgpwire.EmbeddedSignature{Signed: true, Status: pgpwire.EmbeddedVerified,
		IssuerFingerprint: "AB", SignerFingerprint: "CD"})
	if got != (api.EmbeddedSignature{Signed: true, Status: api.EmbeddedVerified, Issuer: "AB", SignerFingerprint: "CD"}) {
		t.Errorf("verified: %+v", got)
	}
	// No fingerprint subpacket: the key id names the issuer.
	got = embeddedFromWire(pgpwire.EmbeddedSignature{Signed: true, Status: pgpwire.EmbeddedNoKey, IssuerKeyID: "0123456789ABCDEF"})
	if got.Issuer != "0123456789ABCDEF" || got.Status != api.EmbeddedNoKey {
		t.Errorf("key id: %+v", got)
	}
	if got := embeddedFromWire(pgpwire.EmbeddedSignature{}); got != (api.EmbeddedSignature{}) {
		t.Errorf("unsigned: %+v", got)
	}
}
