// SPDX-License-Identifier: AGPL-3.0-only

package pgpverify

import (
	"bytes"
	stdcrypto "crypto"
	"strings"
	"testing"
	"time"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/ProtonMail/go-crypto/openpgp/armor"
	"github.com/ProtonMail/go-crypto/openpgp/packet"

	"mailroom/internal/pgpmime"
)

// signedWithHash builds a real multipart/signed message whose detached signature
// uses hash h. go-crypto's ArmoredDetachSign will not write SHA-1, so the
// signature packet is made with the library's own PrepareSign/Sign, the sequence
// detachSign runs minus its hash selection (salt notation off: it has no salt
// length for SHA-1).
func signedWithHash(t *testing.T, h stdcrypto.Hash) (raw, canonical, armoredSig []byte, pub PublicKey, e *openpgp.Entity) {
	t.Helper()
	e, err := openpgp.NewEntity("Weak", "", "signer@example.com", &packet.Config{Algorithm: packet.PubKeyAlgoEdDSA})
	if err != nil {
		t.Fatal(err)
	}
	msg := []byte("Content-Type: text/plain; charset=utf-8\r\n\r\nsigned with a chosen hash\r\n")
	raw, err = pgpmime.Sign(msg, func(c []byte) (string, string, error) {
		canonical = append([]byte(nil), c...)
		priv := e.PrivateKey
		off := false
		cfg := &packet.Config{NonDeterministicSignaturesViaNotation: &off}
		sig := &packet.Signature{Version: priv.Version, SigType: packet.SigTypeBinary, PubKeyAlgo: priv.PubKeyAlgo,
			Hash: h, CreationTime: time.Now(), IssuerKeyId: &priv.KeyId, IssuerFingerprint: priv.Fingerprint}
		hasher, err := sig.PrepareSign(cfg)
		if err != nil {
			return "", "", err
		}
		hasher.Write(c)
		if err := sig.Sign(hasher, priv, cfg); err != nil {
			return "", "", err
		}
		var out bytes.Buffer
		aw, err := armor.Encode(&out, openpgp.SignatureType, nil)
		if err != nil {
			return "", "", err
		}
		if err := sig.Serialize(aw); err != nil {
			return "", "", err
		}
		if err := aw.Close(); err != nil {
			return "", "", err
		}
		armoredSig = out.Bytes()
		return out.String(), "pgp-sha256", nil
	})
	if err != nil {
		t.Fatalf("pgpmime.Sign: %v", err)
	}
	var b bytes.Buffer
	if err := e.Serialize(&b); err != nil {
		t.Fatal(err)
	}
	return raw, canonical, armoredSig, PublicKey{Fingerprint: strings.ToUpper(string(e.PrimaryKey.Fingerprint)), Binary: b.Bytes()}, e
}

// #301 C2 on the verify side: a SHA-1 signature is not "verified", although
// go-crypto's v1 API would call it good.
func TestVerifyRefusesSHA1(t *testing.T) {
	raw, canonical, armoredSig, pub, e := signedWithHash(t, stdcrypto.SHA1)

	// THE LIBRARY HALF, measured: go-crypto verifies it. If this ever goes red the
	// library has started refusing SHA-1 itself; revisit WeakHashes' comment.
	blk, err := armor.Decode(bytes.NewReader(armoredSig))
	if err != nil {
		t.Fatal(err)
	}
	sig, _, err := openpgp.VerifyDetachedSignature(openpgp.EntityList{e}, bytes.NewReader(canonical), blk.Body, nil)
	if err != nil || sig.Hash != stdcrypto.SHA1 {
		t.Fatalf("fixture: go-crypto did not verify the SHA-1 signature (%v); the refusal below would not be ours", err)
	}

	if res := Verify(raw, []PublicKey{pub}); res.Verdict != VerdictBadSignature {
		t.Errorf("SHA-1 signature: verdict %s, want %s", res.Verdict, VerdictBadSignature)
	}
	// CONTROL: the same construction with SHA-256 verifies.
	raw256, _, _, pub256, _ := signedWithHash(t, stdcrypto.SHA256)
	if res := Verify(raw256, []PublicKey{pub256}); res.Verdict != VerdictVerified {
		t.Errorf("SHA-256 control: verdict %s (%v), want verified", res.Verdict, res.Err)
	}
}
