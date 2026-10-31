// SPDX-License-Identifier: AGPL-3.0-only

package pgpkeyd

// #301 C2 — the daemon must never publish an MD5/SHA-1/RIPEMD160 signature.
//
// # The measurement that shaped these tests
//
// The audit's stated vector was an IMPORTED key whose declared hash preferences
// exclude SHA-2, on the strength of Sign's own comment that "go-crypto may
// override [DefaultHash] if the key's own preferences exclude it". Reading
// go-crypto v1.4.1 before writing a fixture showed that comment to be wrong
// about this library: DetachSign → createSignaturePacket →
// selectHashForSigningKey consults acceptableHashesToWrite, a HARD-CODED table
// keyed on the public-key algorithm and curve —
//
//	Ed448 / X448 / P-521 / BP-512 → SHA-512, SHA3-512
//	P-384 / BP-384               → SHA-384, SHA-512, SHA3-512
//	everything else              → SHA-256, SHA-384, SHA-512, SHA3-256, SHA3-512
//
// — and never looks at the key's PreferredHash subpacket at all. With
// DefaultHash = SHA-256 the selected hash is therefore always SHA-256, SHA-384
// or SHA-512, whatever the key says about itself.
//
// So the vector does not exist today, and the fixture the audit asked for
// CANNOT be built to produce a weak signature through the real library. That is
// a finding, not an obstacle, and it is asserted rather than asserted-about:
// TestSignIgnoresLegacyHashPreferences imports a key that declares MD5/SHA-1/
// RIPEMD160 and nothing else, and pins that it still signs with SHA-256. If a
// go-crypto upgrade ever starts honouring declared preferences, that test goes
// red and points here.
//
// A second measurement narrows it further. go-crypto's algorithm.HashById knows
// only {SHA-256, SHA-384, SHA-512, SHA-224, SHA3-256, SHA3-512}, with SHA-1
// reachable through the separate …WithSha1 accessors that v4 signatures use. So
// in this library MD5 and RIPEMD160 cannot be WRITTEN (HashToHashId fails) and
// cannot be READ EITHER — Signature.parse rejects hash ids 1 and 3 outright, so
// micAlgOf would fail rather than return "pgp-md5", and Sign would refuse that
// as internal. Fail-closed by a different door, and the reason the tests below
// exercise SHA-1 only: it is the one broken hash this library can express.
// The md5/ripemd160 entries in weakMicAlgs stay as insurance against a future
// version that re-adds them, and are dead today by measurement, not by hope.
//
// The refusal ships anyway. It costs one map lookup on a path whose output is
// published to a correspondent and cannot be recalled, and it does not depend
// on a dependency's internal policy table staying as it is.
//
// # How an unreachable refusal is still probed
//
// A check nothing can reach is a check nobody can show is load-bearing — this
// project has shipped three tests that passed for reasons unrelated to their
// subject. So the refusal is exercised through the Service.detachSign seam,
// substituting a signer that emits a GENUINE armored SHA-1 detached signature
// over the same data with the same key. Real packets, real hash, read back by
// the real micAlgOf. What the seam does not prove is that openpgp.DetachSign is
// the production signer — that is one line in NewService, and
// TestProductionUsesLibraryDetachSign asserts it.

import (
	"bytes"
	"context"
	stdcrypto "crypto"

	"io"
	"reflect"
	"strings"
	"testing"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/ProtonMail/go-crypto/openpgp/armor"
	"github.com/ProtonMail/go-crypto/openpgp/packet"

	"mailroom/internal/pgpwire"
)

// OpenPGP hash algorithm ids (RFC 9580 §9.5), for the PreferredHash subpacket.
const (
	hashIDMD5       = 1
	hashIDSHA1      = 2
	hashIDRIPEMD160 = 3
	hashIDSHA256    = 8
)

// detachSignWith returns a substitute for openpgp.DetachSign that signs with a
// chosen hash instead of letting go-crypto choose one.
//
// It builds the signature packet by hand — the same sequence
// openpgp.detachSign runs, minus selectHashForSigningKey — so the bytes it
// writes are a genuine detached signature by the daemon's own key, over the
// caller's data, differing from a production one only in the hash. Nothing here
// touches or reveals the private key beyond handing it to the library's own
// Sign, exactly as production does.
func detachSignWith(h stdcrypto.Hash) func(io.Writer, *openpgp.Entity, io.Reader, *packet.Config) error {
	return func(w io.Writer, signer *openpgp.Entity, message io.Reader, cfg *packet.Config) error {
		priv := signer.PrivateKey
		// ⚠ ONE deviation from production's config, and it is forced: go-crypto
		// randomizes v4 signatures with a salt notation by default, and
		// SignatureSaltForHash has no salt length for MD5 or SHA-1 — the very
		// hashes this fixture exists to produce. Turning that off is the only
		// way to make the artefact at all. It changes a notation subpacket and
		// nothing about the hash, which is the only field under test.
		off := false
		weakCfg := &packet.Config{
			Time:                                  cfg.Time,
			NonDeterministicSignaturesViaNotation: &off,
		}
		sig := &packet.Signature{
			Version:           priv.Version,
			SigType:           packet.SigTypeBinary,
			PubKeyAlgo:        priv.PubKeyAlgo,
			Hash:              h,
			CreationTime:      weakCfg.Now(),
			IssuerKeyId:       &priv.KeyId,
			IssuerFingerprint: priv.Fingerprint,
		}
		hasher, err := sig.PrepareSign(weakCfg)
		if err != nil {
			return err
		}
		if _, err := io.Copy(hasher, message); err != nil {
			return err
		}
		if err := sig.Sign(hasher, priv, weakCfg); err != nil {
			return err
		}
		return sig.Serialize(w)
	}
}

// TestSignRefusesWeakHash is the refusal itself, in the shape of the retired-key
// refusal it mirrors: invalid-request, nothing in the response, and a POSITIVE
// CONTROL proving the same key, same data and same request succeed when the
// hash is acceptable.
func TestSignRefusesWeakHash(t *testing.T) {
	for _, tc := range []struct {
		name   string
		hash   stdcrypto.Hash
		micalg string
	}{
		// SHA-1 only — see this file's header: go-crypto v1.4.1 can neither
		// write nor read an MD5 or RIPEMD160 signature packet, so there is no
		// honest fixture for those arms of the refusal. Their door is a parse
		// failure in micAlgOf, which Sign already refuses as internal.
		{"sha1", stdcrypto.SHA1, "pgp-sha1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := startDaemon(t)
			ctx := context.Background()
			user := newUserID()
			data := []byte("Content-Type: text/plain\r\n\r\nweak hash\r\n")

			gen, err := h.client.Generate(ctx, pgpwire.GenerateRequest{UserID: user, Email: "weak@example.com"})
			if err != nil {
				t.Fatalf("Generate: %v", err)
			}

			// ── POSITIVE CONTROL: unmodified, this exact request signs. Without
			// it a Sign broken for any other reason would look like the refusal.
			before, err := h.client.Sign(ctx, pgpwire.SignRequest{
				UserID: user, KeyID: gen.Key.KeyID, Data: data,
			})
			if err != nil {
				t.Fatalf("the key would not sign at all: %v", err)
			}
			if before.MicAlg != "pgp-sha256" {
				t.Fatalf("control signature micalg = %q, want pgp-sha256", before.MicAlg)
			}

			// Now make the daemon produce the weak-hash artefact it would produce
			// if go-crypto ever selected one.
			h.svc.detachSign = detachSignWith(tc.hash)

			// ── CONTROL ON THE FIXTURE: the substitute must really emit that
			// hash, read back the way a verifier would. If it silently produced
			// SHA-256 the refusal below would never be reached and the test would
			// pass by not testing anything.
			var probe bytes.Buffer
			aw, err := armor.Encode(&probe, openpgp.SignatureType, nil)
			if err != nil {
				t.Fatalf("armor.Encode: %v", err)
			}
			entity := h.entityOf(t, gen.Key.KeyID)
			if err := h.svc.detachSign(aw, entity, bytes.NewReader(data), &packet.Config{}); err != nil {
				t.Fatalf("the substitute signer failed: %v", err)
			}
			if err := aw.Close(); err != nil {
				t.Fatalf("armor close: %v", err)
			}
			if got, err := micAlgOf(probe.Bytes()); err != nil {
				t.Fatalf("micAlgOf on the fixture: %v", err)
			} else if got != tc.micalg {
				t.Fatalf("the fixture signs with %q, not %q — it cannot exercise the refusal", got, tc.micalg)
			}

			got, err := h.client.Sign(ctx, pgpwire.SignRequest{
				UserID: user, KeyID: gen.Key.KeyID, Data: data,
			})
			if err == nil {
				t.Fatalf("a %s signature was published: %.60q", tc.micalg, got.Signature)
			}
			if code, _ := pgpwire.CodeOf(err); code != pgpwire.CodeInvalidRequest {
				t.Errorf("code = %q, want %q", code, pgpwire.CodeInvalidRequest)
			}
			if got != nil {
				t.Errorf("a response body accompanied the refusal: %+v", got)
			}
			// The refusal must SAY which hash, or an operator cannot tell it from
			// the retired-key refusal that shares its code.
			if !strings.Contains(err.Error(), tc.micalg) {
				t.Errorf("the refusal does not name the hash it refused: %v", err)
			}
			// Fail closed means nothing leaked out with the refusal either.
			if strings.Contains(err.Error(), "BEGIN PGP") {
				t.Errorf("the refusal carried signature bytes: %v", err)
			}
		})
	}
}

// TestWeakMicAlgsCoversEveryBrokenHashThisPackageCanName pins the two tables to
// each other. micAlgNames deliberately still names the broken hashes so the
// refusal can report one; if a name is added there and not classified here, a
// hash would be publishable by omission.
func TestWeakMicAlgsCoversEveryBrokenHashThisPackageCanName(t *testing.T) {
	for h, name := range micAlgNames {
		broken := h == stdcrypto.MD5 || h == stdcrypto.SHA1 || h == stdcrypto.RIPEMD160
		if broken && !weakMicAlgs[name] {
			t.Errorf("%v is named %q and would be published", h, name)
		}
		if !broken && weakMicAlgs[name] {
			t.Errorf("%v (%q) is refused but is not a broken hash", h, name)
		}
	}
	for name := range weakMicAlgs {
		found := false
		for _, n := range micAlgNames {
			if n == name {
				found = true
			}
		}
		if !found {
			t.Errorf("weakMicAlgs refuses %q, which micAlgOf can never produce — the entry is dead", name)
		}
	}
}

// TestProductionUsesLibraryDetachSign is the one assertion the seam cannot make
// about itself: that a Service built the production way signs with go-crypto,
// not with anything a test left behind.
func TestProductionUsesLibraryDetachSign(t *testing.T) {
	svc, err := NewService(make([]byte, 32), NewMemoryVault(), discardLogger())
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	want := reflect.ValueOf(openpgp.DetachSign).Pointer()
	if got := reflect.ValueOf(svc.detachSign).Pointer(); got != want {
		t.Error("NewService did not wire openpgp.DetachSign as the detached signer")
	}
}

// TestSignIgnoresLegacyHashPreferences is the audit's stated vector, built and
// measured: a key that declares MD5/SHA-1/RIPEMD160 and NOTHING ELSE.
//
// It signs with SHA-256 anyway, because go-crypto v1.4.1 does not consult a
// key's declared hash preferences when choosing a detached-signature hash (see
// this file's header). The test therefore documents a NEGATIVE result — the
// vector is closed by the library, not by us — and is the canary for that
// changing: if a future go-crypto starts honouring the preferences, this goes
// red and the refusal in Sign becomes the thing keeping the promise.
func TestSignIgnoresLegacyHashPreferences(t *testing.T) {
	h := startDaemon(t)
	ctx := context.Background()
	user := newUserID()

	armored := legacyPrefsPrivateKey(t)

	imp, err := h.client.Import(ctx, pgpwire.ImportRequest{UserID: user, PrivateKey: armored})
	if err != nil {
		t.Fatalf("Import: %v", err)
	}

	// ── CONTROL ON THE FIXTURE: the preferences must have survived the armor,
	// the wire, the sealing and the unseal. A key that arrived with default
	// preferences would make the assertion below vacuous.
	stored := h.entityOf(t, imp.Key.KeyID)
	for _, id := range stored.Identities {
		prefs := id.SelfSignature.PreferredHash
		if len(prefs) == 0 {
			t.Fatalf("the stored key declares no hash preferences at all — the fixture did not survive")
		}
		for _, p := range prefs {
			if p == hashIDSHA256 {
				t.Fatalf("the stored key still prefers SHA-256 (%v) — the fixture does not exclude SHA-2", prefs)
			}
		}
	}

	got, err := h.client.Sign(ctx, pgpwire.SignRequest{
		UserID: user, KeyID: imp.Key.KeyID, Data: []byte("legacy prefs\r\n"),
	})
	if err != nil {
		t.Fatalf("Sign with a legacy-preference key: %v — if this is now the weak-hash "+
			"refusal, go-crypto has started honouring declared preferences and #301 C2's "+
			"vector is live", err)
	}
	if got.MicAlg != "pgp-sha256" {
		t.Errorf("micalg = %q, want pgp-sha256", got.MicAlg)
	}
	if weakMicAlgs[got.MicAlg] {
		t.Errorf("a weak-hash signature was published as %q", got.MicAlg)
	}
}

// legacyPrefsPrivateKey builds an armored private key whose self-signature
// declares only MD5, SHA-1 and RIPEMD160 as acceptable hashes.
//
// The self-signature is re-made after the subpacket is rewritten, because a
// preference nobody signed is a preference no implementation reads.
func legacyPrefsPrivateKey(t *testing.T) string {
	t.Helper()
	cfg := &packet.Config{
		Algorithm: packet.PubKeyAlgoEdDSA,
		Curve:     packet.Curve25519,
		V6Keys:    false,
	}
	entity, err := openpgp.NewEntity("Legacy", "", "legacy@example.com", cfg)
	if err != nil {
		t.Fatalf("NewEntity: %v", err)
	}
	for name, id := range entity.Identities {
		id.SelfSignature.PreferredHash = []uint8{hashIDMD5, hashIDSHA1, hashIDRIPEMD160}
		if err := id.SelfSignature.SignUserId(name, entity.PrimaryKey, entity.PrivateKey, cfg); err != nil {
			t.Fatalf("re-sign user id: %v", err)
		}
	}
	var buf bytes.Buffer
	if err := entity.SerializePrivateWithoutSigning(&buf, nil); err != nil {
		t.Fatalf("SerializePrivateWithoutSigning: %v", err)
	}
	return armorPrivateForTest(t, buf.Bytes())
}

// entityOf unseals a stored key IN PROCESS, so a test can inspect the key the
// daemon would actually sign with. It never returns or logs key material — the
// entity stays inside the test binary, exactly as it stays inside the daemon.
func (h *harness) entityOf(t *testing.T, keyID string) *openpgp.Entity {
	t.Helper()
	rec, err := h.vault.Get(context.Background(), keyID)
	if err != nil {
		t.Fatalf("vault.Get: %v", err)
	}
	entity, err := h.svc.unseal(rec)
	if err != nil {
		t.Fatalf("unseal: %v", err)
	}
	return entity
}
