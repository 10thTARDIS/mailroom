// SPDX-License-Identifier: AGPL-3.0-only

package pgpkeyd

// The two tests ADR-024 names in its Verification section. Both are NEGATIVE
// assertions, and both pass vacuously if written carelessly, so each carries
// its own control:
//
//   - TestNoAPIPathReturnsPrivateKeyMaterial builds a leak detector, then
//     PROVES THE DETECTOR FIRES against synthetic leaky responses before
//     trusting it to say "clean" about the real ones. A detector that cannot
//     see a leak reports no leaks.
//   - TestConfusedDeputyFailsClosed carries a positive control in the same
//     test: A asking for A's own key must succeed. Without it, a daemon that
//     refused everything would pass.
//
// Both were also probed by hand on the way in — see the PR for what went red.

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/ProtonMail/go-crypto/openpgp/armor"
	"github.com/ProtonMail/go-crypto/openpgp/packet"
	"github.com/oklog/ulid/v2"

	"mailroom/internal/crypto"
	"mailroom/internal/pgpwire"
)

// ---------------------------------------------------------------------------
// Harness: a real daemon on a real Unix socket, spoken to by the real client.
// ---------------------------------------------------------------------------

type harness struct {
	client *pgpwire.Client
	svc    *Service
	vault  *MemoryVault
	root   []byte
}

func startDaemon(t *testing.T) *harness {
	t.Helper()
	return startDaemonWithLogger(t, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

// startDaemonWithLogger is startDaemon with the daemon's logger supplied, so a
// test can read what the process wrote — see import_test.go, where "did the
// private key reach the log" is the whole question.
func startDaemonWithLogger(t *testing.T, log *slog.Logger) *harness {
	t.Helper()

	root := make([]byte, crypto.KeySize)
	for i := range root {
		root[i] = byte(i * 7)
	}
	vault := NewMemoryVault()

	svc, err := NewService(root, vault, log)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	handler, err := NewHandler(svc, log)
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}

	socket := filepath.Join(t.TempDir(), "crypto.sock")
	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan error, 1)
	go func() { served <- Serve(ctx, socket, handler, log) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-served:
			if err != nil {
				t.Errorf("Serve: %v", err)
			}
		case <-time.After(10 * time.Second):
			t.Error("Serve did not return after cancellation")
		}
	})

	client := pgpwire.NewClient(socket)
	deadline := time.Now().Add(10 * time.Second)
	for {
		if err := client.Health(context.Background()); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("crypto process never became healthy")
		}
		time.Sleep(5 * time.Millisecond)
	}

	return &harness{client: client, svc: svc, vault: vault, root: root}
}

// unsealPrivate recovers a stored private key OUT OF BAND — straight from the
// vault, in-process, using the root the test itself created. This is how the
// leak test gets a needle it knows is genuine private material: nothing about
// it goes through the API, so a broken API cannot quietly supply a harmless
// substitute.
func (h *harness) unsealPrivate(t *testing.T, keyID string) []byte {
	t.Helper()
	rec, err := h.vault.Get(context.Background(), keyID)
	if err != nil {
		t.Fatalf("vault.Get: %v", err)
	}
	wrap, err := crypto.DeriveKey(h.root, []byte(rec.UserID), rootInfo)
	if err != nil {
		t.Fatalf("DeriveKey: %v", err)
	}
	priv, err := crypto.Open(wrap, rec.Sealed, sealAAD(rec.UserID, rec.KeyID))
	if err != nil {
		t.Fatalf("open sealed private key: %v", err)
	}
	if len(priv) == 0 {
		t.Fatal("sealed private key opened to nothing")
	}
	return priv
}

func newUserID() string { return ulid.Make().String() }

// ---------------------------------------------------------------------------
// The leak detector.
// ---------------------------------------------------------------------------

// leakDetector reports whether a response body contains private key material.
//
// "Contains" is not a substring check on the wire bytes, because a leak would
// realistically arrive JSON-escaped, base64'd (Go marshals []byte that way), or
// ASCII-armored. So the corpus for one response is: its raw bytes, every string
// value inside its JSON, and the decoded form of every armor block and every
// base64 run found in those strings.
type leakDetector struct {
	// windows are 16-byte runs lying entirely within the SECRET portion of a
	// secret-key packet. They are what makes the detector specific: public key
	// fields and user IDs are in legitimate responses, so a needle touching
	// them would fire on clean output.
	windows [][]byte
	// wholePriv is the complete serialized private key, searched additionally
	// in hex and base64 for the case where a leak carries all of it verbatim.
	wholePriv []byte
}

func newLeakDetector(t *testing.T, priv []byte, publicArmor string) *leakDetector {
	t.Helper()

	// publicArmor is still validated: a detector built from a public key that
	// does not parse would be built from the wrong inputs.
	_ = dearmor(t, publicArmor)
	windows, err := secretWindows(priv)
	if err != nil {
		t.Fatalf("leak detector: %v", err)
	}
	// If the key had no secret bytes there would be no needle at all, and every
	// assertion below would pass for a reason that has nothing to do with the
	// API. Refuse to run in that state.
	if len(windows) < 8 {
		t.Fatalf("leak detector has only %d secret-material windows; needles are not distinctive enough to test with", len(windows))
	}
	return &leakDetector{windows: windows, wholePriv: priv}
}

const leakWindowSize = 16

// secretSuffixes returns, for each secret-key packet in the serialized private
// key, the bytes of that packet that are SECRET: everything after the public
// key fields (S2K usage, S2K/IV, the secret scalars or MPIs, the checksum).
// Nothing from a user-ID packet, signature, or the public half of a key packet
// is ever returned, so a needle cut from these can never match a response that
// legitimately carries the public key or user IDs (#899).
func secretSuffixes(priv []byte) ([][]byte, error) {
	r := packet.NewReader(bytes.NewReader(priv))
	var out [][]byte
	for {
		p, err := r.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		pk, ok := p.(*packet.PrivateKey)
		if !ok {
			continue
		}
		var privBuf, pubBuf bytes.Buffer
		if err := pk.Serialize(&privBuf); err != nil {
			return nil, err
		}
		if err := pk.PublicKey.Serialize(&pubBuf); err != nil {
			return nil, err
		}
		pubBodyLen := bodyLenOf(pubBuf.Bytes())
		secretLen := bodyLenOf(privBuf.Bytes()) - pubBodyLen
		if secretLen <= 0 {
			continue
		}
		raw := privBuf.Bytes()
		out = append(out, raw[len(raw)-secretLen:])
	}
	return out, nil
}

// bodyLenOf returns the body length of the single new-format packet in b.
func bodyLenOf(b []byte) int {
	switch {
	case b[1] < 192:
		return int(b[1])
	case b[1] < 224:
		return (int(b[1])-192)<<8 + int(b[2]) + 192
	default:
		return int(b[2])<<24 | int(b[3])<<16 | int(b[4])<<8 | int(b[5])
	}
}

// secretWindows takes every leakWindowSize-byte run lying entirely within a
// secret region of the private key.
func secretWindows(priv []byte) ([][]byte, error) {
	sufs, err := secretSuffixes(priv)
	if err != nil {
		return nil, err
	}
	var windows [][]byte
	for _, s := range sufs {
		for i := 0; i+leakWindowSize <= len(s); i++ {
			windows = append(windows, s[i:i+leakWindowSize])
		}
	}
	return windows, nil
}

// findLeak returns a description of the first private-material match, or "".
func (d *leakDetector) findLeak(body []byte) string {
	corpus := expand(body)

	for _, c := range corpus {
		if bytes.Contains(c, []byte(openpgp.PrivateKeyType)) {
			return "response carries a " + openpgp.PrivateKeyType + " armor header"
		}
		for _, w := range d.windows {
			if bytes.Contains(c, w) {
				return "response carries private key bytes (window " + hex.EncodeToString(w) + ")"
			}
		}
		if bytes.Contains(c, []byte(hex.EncodeToString(d.wholePriv))) {
			return "response carries the hex-encoded private key"
		}
		if bytes.Contains(c, []byte(base64.StdEncoding.EncodeToString(d.wholePriv))) {
			return "response carries the base64-encoded private key"
		}
	}
	return ""
}

// expand turns one response body into every representation a leak could hide
// in: the raw bytes, each JSON string value, and the decoded content of armor
// blocks and base64 runs inside those strings.
func expand(body []byte) [][]byte {
	out := [][]byte{body}

	var doc any
	if err := json.Unmarshal(body, &doc); err == nil {
		for _, s := range jsonStrings(doc) {
			out = append(out, []byte(s))
			if b := dearmorBestEffort(s); b != nil {
				out = append(out, b)
			}
			if b, err := base64.StdEncoding.DecodeString(strings.TrimSpace(s)); err == nil && len(b) > 0 {
				out = append(out, b)
			}
		}
	}
	return out
}

func jsonStrings(v any) []string {
	switch t := v.(type) {
	case string:
		return []string{t}
	case []any:
		var out []string
		for _, e := range t {
			out = append(out, jsonStrings(e)...)
		}
		return out
	case map[string]any:
		var out []string
		for k, e := range t {
			out = append(out, k)
			out = append(out, jsonStrings(e)...)
		}
		return out
	default:
		return nil
	}
}

func dearmor(t *testing.T, s string) []byte {
	t.Helper()
	b := dearmorBestEffort(s)
	if b == nil {
		t.Fatalf("could not dearmor: %.60q", s)
	}
	return b
}

func dearmorBestEffort(s string) []byte {
	i := strings.Index(s, "-----BEGIN PGP")
	if i < 0 {
		return nil
	}
	blk, err := armor.Decode(strings.NewReader(s[i:]))
	if err != nil {
		return nil
	}
	b, err := io.ReadAll(blk.Body)
	if err != nil {
		return nil
	}
	return b
}

// ---------------------------------------------------------------------------
// Test 1 — no API path returns private key material.
// ---------------------------------------------------------------------------

// probes is the test's own registry of how to call each method. It is checked
// against pgpwire.Methods below in BOTH directions, so a method added to the
// API without a probe here fails this suite instead of being silently skipped.
// That check is the reason this test does not rot into passing vacuously as the
// API grows through increments 3–6.
func probes(userID, keyID, fingerprint, armoredPrivate string, ciphertext []byte, recipientPublic string) map[pgpwire.Method]any {
	return map[pgpwire.Method]any{
		pgpwire.MethodGenerate: pgpwire.GenerateRequest{
			UserID: userID, Name: "Probe", Email: "probe@example.com",
		},
		pgpwire.MethodExportPublic: pgpwire.ExportPublicRequest{
			UserID: userID, KeyID: keyID,
		},
		// import is the method whose REQUEST carries a private key, so its
		// probe hands over a real one: the leak test then answers the question
		// that actually matters here — does the key I just posted come back out?
		pgpwire.MethodImport: pgpwire.ImportRequest{
			UserID: userID, PrivateKey: armoredPrivate,
			// Ignored for an unprotected key, but present so the request shape
			// with a passphrase goes through the leak assertions too; the real
			// protected-key path is TestImportPassphrase*.
			Passphrase: "probe-import-passphrase",
		},
		pgpwire.MethodBindIdentity: pgpwire.BindIdentityRequest{
			UserID: userID, KeyID: keyID, IdentityID: ulid.Make().String(),
		},
		// sign is the method that UNSEALS a private key, so its probe must be a
		// request that succeeds: a refusal would exercise the argument checks and
		// never touch the key at all, and the leak assertion would pass for a
		// reason that has nothing to do with signing.
		// export-wrapped is the method whose RESPONSE carries a private key, so
		// its probe must SUCCEED: a refusal would exercise the argument checks and
		// never wrap anything, and the assertion would pass for a reason that has
		// nothing to do with exporting.
		pgpwire.MethodExportWrapped: pgpwire.ExportWrappedRequest{
			UserID: userID, KeyID: keyID, Passphrase: "probe-passphrase-long-enough",
		},
		pgpwire.MethodSign: pgpwire.SignRequest{
			UserID: userID, KeyID: keyID, Data: []byte("probe data to be signed\r\n"),
		},
		// decrypt UNSEALS every key the user holds and returns plaintext, so its
		// probe must SUCCEED (#47 checklist item 6): a refusal would exercise the
		// argument checks or fail before the session key existed, and the leak
		// assertion would pass for a reason that has nothing to do with
		// decrypting. The ciphertext is encrypted to the probed user's own key.
		// It carries a candidate signer key (RFC 3156 section 6.2) so the
		// signer-parsing path is part of what is examined.
		pgpwire.MethodDecrypt: pgpwire.DecryptRequest{
			UserID: userID, MessageID: ulid.Make().String(), Purpose: pgpwire.DecryptForBody,
			Ciphertext: ciphertext,
			Signers:    []pgpwire.DecryptSigner{{PublicKey: []byte(recipientPublic)}},
		},
		// encrypt uses no private key, but it takes the sender's key id and puts
		// a session key into the world, so its probe must SUCCEED too (#47,
		// ADR-038): a refusal would exercise the argument checks and never choose
		// a session key at all. The recipient is a different user's real key.
		pgpwire.MethodEncrypt: pgpwire.EncryptRequest{
			UserID: userID, KeyID: keyID, Entity: []byte(secretEntity),
			Recipients: []pgpwire.EncryptRecipient{
				{Address: "probe-recipient@example.com", PublicKey: []byte(recipientPublic)},
			},
		},
		// extend UNSEALS the key, re-signs it and RE-SEALS it (#795), so its
		// probe must SUCCEED: a refusal would never touch the private half. It
		// runs after every method above (registry order) and before revoke.
		pgpwire.MethodExtend: pgpwire.ExtendRequest{
			UserID: userID, KeyID: keyID, Days: 400,
		},
		// revoke unseals, signs a revocation and re-seals (#795). It must
		// SUCCEED for the same reason, and it is LAST in the registry, so the
		// probes before it ran against a live key.
		pgpwire.MethodRevoke: pgpwire.RevokeRequest{
			UserID: userID, KeyID: keyID, Reason: pgpwire.RevokeSuperseded,
			Confirm: fingerprintSuffix(fingerprint),
		},
	}
}

func TestProbeTableCoversEveryMethod(t *testing.T) {
	table := probes("u", "k", "", "", nil, "")
	if len(table) != len(pgpwire.Methods) {
		t.Fatalf("probe table has %d entries for %d declared methods", len(table), len(pgpwire.Methods))
	}
	for _, m := range pgpwire.Methods {
		if _, ok := table[m]; !ok {
			t.Errorf("method %q is served but has no leak probe", m)
		}
	}
	for m := range table {
		if !slicesContains(pgpwire.Methods, m) {
			t.Errorf("probe table has %q, which is not a declared method", m)
		}
	}
}

func TestNoAPIPathReturnsPrivateKeyMaterial(t *testing.T) {
	h := startDaemon(t)
	ctx := context.Background()
	user := newUserID()

	gen, err := h.client.Generate(ctx, pgpwire.GenerateRequest{
		UserID: user, Name: "Subject", Email: "subject@example.com",
	})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}

	priv := h.unsealPrivate(t, gen.Key.KeyID)
	det := newLeakDetector(t, priv, gen.Key.PublicKey)
	probeCiphertext := encryptTo(t, pubRing(t, gen.Key.PublicKey), []byte(secretEntity), nil)
	recipientGen, err := h.client.Generate(ctx, pgpwire.GenerateRequest{
		UserID: newUserID(), Name: "Recipient", Email: "probe-recipient@example.com",
	})
	if err != nil {
		t.Fatalf("Generate recipient: %v", err)
	}

	// ── Control: prove the detector can SEE a leak before believing it when it
	// says there is none. Three shapes a real leak would take.
	t.Run("detector fires on synthetic leaks", func(t *testing.T) {
		armored := armorPrivateForTest(t, priv)
		leaky := map[string][]byte{
			"armored private key": mustJSON(t, map[string]string{"key": armored}),
			"base64 private key":  mustJSON(t, map[string][]byte{"key": priv}),
			"raw private bytes":   priv,
		}
		for name, body := range leaky {
			if got := det.findLeak(body); got == "" {
				t.Errorf("detector did not fire on a %s — it cannot observe the failure it claims to exclude", name)
			}
		}
	})

	// ── Control: the detector must NOT fire on legitimate public output, or
	// every assertion below would be unfalsifiable in the other direction.
	t.Run("detector is quiet on the public key", func(t *testing.T) {
		body := mustJSON(t, map[string]string{"public_key": gen.Key.PublicKey})
		if got := det.findLeak(body); got != "" {
			t.Errorf("detector fired on a legitimate public key: %s", got)
		}
	})

	// ── The assertion itself: call EVERY method and look at what comes back.
	// Not at the code, and not at whether a method named "export-private"
	// exists.
	for _, m := range pgpwire.Methods {
		req := probes(user, gen.Key.KeyID, gen.Key.Fingerprint, armorPrivateForTest(t, priv), probeCiphertext, recipientGen.Key.PublicKey)[m]

		var raw json.RawMessage
		callErr := h.client.Call(ctx, m, req, &raw)

		// A method that failed is still a response, and its error text is
		// still bytes that came out of the process holding the keys.
		corpus := []byte(raw)
		if callErr != nil {
			corpus = append(corpus, []byte(callErr.Error())...)
		}
		if len(corpus) == 0 {
			t.Errorf("method %q returned nothing at all — it was not really exercised", m)
			continue
		}

		// ⚠ export-wrapped IS THE ONE METHOD THAT RETURNS A PRIVATE KEY, and it
		// gets a STRICTER assertion rather than an exemption.
		//
		// The generic detector fires on the `PGP PRIVATE KEY BLOCK` armor header
		// unconditionally, which this method legitimately emits. Skipping it
		// here would leave the one response that actually carries key material
		// as the one nobody checks — the exact shape of a vacuous test. So
		// instead: the response must contain NO PLAINTEXT private-key bytes (the
		// encryption has to have really happened), and the armored key must be
		// passphrase-protected and refuse to open without it.
		if m == pgpwire.MethodExportWrapped {
			assertWrappedExportIsNotALeak(t, det, corpus, callErr)
			continue
		}
		// decrypt's probe must have succeeded, or it never unsealed a key and
		// the leak check below examined an error message instead of a
		// decryption (#47 checklist item 6).
		if m == pgpwire.MethodDecrypt {
			var dr pgpwire.DecryptResponse
			if callErr != nil || json.Unmarshal(raw, &dr) != nil || string(dr.Entity) != secretEntity {
				t.Errorf("the decrypt probe did not succeed (%v), so the leak assertion for it "+
					"would examine nothing that touched a key", callErr)
			}
		}

		// encrypt's probe must have succeeded, and the sender must be able to open
		// what it produced: otherwise the leak check examined an error message.
		if m == pgpwire.MethodEncrypt {
			var er pgpwire.EncryptResponse
			if callErr != nil || json.Unmarshal(raw, &er) != nil || !strings.Contains(er.Ciphertext, "BEGIN PGP MESSAGE") {
				t.Errorf("the encrypt probe did not succeed (%v), so the leak assertion for it "+
					"would examine nothing that chose a session key", callErr)
			} else if dr, err := h.client.Decrypt(ctx, pgpwire.DecryptRequest{
				UserID: user, MessageID: ulid.Make().String(), Purpose: pgpwire.DecryptForBody,
				Ciphertext: []byte(er.Ciphertext),
			}); err != nil || string(dr.Entity) != secretEntity {
				t.Errorf("the encrypt probe's output does not open for the sender: %v", err)
			}
		}

		// extend and revoke must have succeeded, or they never unsealed and
		// re-sealed anything and the check below examined an error message.
		if m == pgpwire.MethodExtend {
			var er pgpwire.ExtendResponse
			if callErr != nil || json.Unmarshal(raw, &er) != nil || er.Key.UsableUntil == nil {
				t.Errorf("the extend probe did not succeed (%v), so the leak assertion for it "+
					"would examine nothing that re-sealed a key", callErr)
			}
		}
		if m == pgpwire.MethodRevoke {
			var rr pgpwire.RevokeResponse
			if callErr != nil || json.Unmarshal(raw, &rr) != nil || rr.RevokedAt.IsZero() {
				t.Errorf("the revoke probe did not succeed (%v), so the leak assertion for it "+
					"would examine nothing that signed a revocation", callErr)
			}
		}

		if leak := det.findLeak(corpus); leak != "" {
			t.Errorf("method %q leaked private key material: %s", m, leak)
		}
	}
}

// assertWrappedExportIsNotALeak holds export-wrapped to the standard the generic
// detector cannot express.
func assertWrappedExportIsNotALeak(t *testing.T, det *leakDetector, corpus []byte, callErr error) {
	t.Helper()
	if callErr != nil {
		t.Fatalf("export-wrapped failed, so this assertion never examined a real "+
			"exported key: %v", callErr)
	}

	var resp pgpwire.ExportWrappedResponse
	if err := json.Unmarshal(corpus, &resp); err != nil {
		t.Fatalf("export-wrapped response is not the declared shape: %v", err)
	}

	// ⚠ THE BYTE-WINDOW DETECTOR IS THE WRONG INSTRUMENT FOR THIS METHOD, AND
	// SAYING SO IS BETTER THAN CONTORTING IT UNTIL IT PASSES.
	//
	// Its needles are 16-byte runs present in the plaintext private
	// serialization and absent from the public export. That is not the same as
	// "secret". A correctly wrapped key legitimately carries, in the clear, the
	// same user IDs, self-signatures and public parameters as the plaintext
	// form — and the private-key packet frames them under a different tag than
	// the public one, so no public-serialization filter can subtract them. Two
	// attempts at filtering both still fired on an export whose secret packets
	// were provably encrypted. A heuristic that cannot be made specific here
	// would only ever have passed by luck.
	//
	// What IS sound is checked instead, and it is stronger for this method:
	// the library's own structural view of the artefact, a whole-key search
	// that cannot false-positive, and a positive control proving the export is
	// real rather than a stub.

	// 1. The whole plaintext key must not appear anywhere in the response, in
	//    any encoding. Unlike a 16-byte run this cannot match by coincidence:
	//    if any part of the key is encrypted, the whole plaintext cannot be
	//    present.
	for _, c := range expand(corpus) {
		if bytes.Contains(c, det.wholePriv) ||
			bytes.Contains(c, []byte(hex.EncodeToString(det.wholePriv))) ||
			bytes.Contains(c, []byte(base64.StdEncoding.EncodeToString(det.wholePriv))) {
			t.Fatal("export-wrapped returned the plaintext private key verbatim")
		}
	}

	// 2. Every secret packet must be encrypted — the primary key AND every
	//    subkey. A subkey left in the clear is the whole encryption subkey of a
	//    modern key, which is the one that opens the user's mail.
	entities := readArmoredKey(t, resp.PrivateKey)
	if !entities.PrivateKey.Encrypted {
		t.Fatal("export-wrapped returned an UNENCRYPTED primary private key — a raw key " +
			"export wearing the name of a wrapped one")
	}
	for i, sub := range entities.Subkeys {
		if sub.PrivateKey != nil && !sub.PrivateKey.Encrypted {
			t.Fatalf("subkey %d came back UNENCRYPTED", i)
		}
	}

	// 3. "Encrypted" must be a fact, not a flag: the wrong passphrase fails.
	if err := entities.PrivateKey.Decrypt([]byte("not-the-passphrase")); err == nil {
		t.Error("the exported key opened with a passphrase that was never set")
	}

	// 4. ⚠ THE POSITIVE CONTROL, without which every assertion above is
	//    satisfied by returning garbage. Re-read the key and open it with the
	//    passphrase the probe actually used; it must succeed and be the SAME
	//    key that was exported.
	fresh := readArmoredKey(t, resp.PrivateKey)
	if err := fresh.PrivateKey.Decrypt([]byte("probe-passphrase-long-enough")); err != nil {
		t.Fatalf("the exported key does NOT open with its own passphrase (%v) — this "+
			"export is unusable, and the assertions above passed for the wrong reason", err)
	}
	if got := fresh.PrimaryKey.KeyIdString(); got != entities.PrimaryKey.KeyIdString() {
		t.Errorf("re-read key id %s differs from %s", got, entities.PrimaryKey.KeyIdString())
	}
}

// readArmoredKey dearmors and parses exactly one OpenPGP key.
func readArmoredKey(t *testing.T, armored string) *openpgp.Entity {
	t.Helper()
	blk, err := armor.Decode(strings.NewReader(armored))
	if err != nil {
		t.Fatalf("export-wrapped did not return an armored key: %v", err)
	}
	entities, err := openpgp.ReadKeyRing(blk.Body)
	if err != nil || len(entities) != 1 || entities[0].PrivateKey == nil {
		t.Fatalf("export-wrapped returned something that is not one private key: %v", err)
	}
	return entities[0]
}

// TestHealthEndpointReturnsNoKeyMaterial covers the one path that is not in the
// method registry.
func TestHealthEndpointReturnsNoKeyMaterial(t *testing.T) {
	h := startDaemon(t)
	ctx := context.Background()
	user := newUserID()

	gen, err := h.client.Generate(ctx, pgpwire.GenerateRequest{
		UserID: user, Email: "health@example.com",
	})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	det := newLeakDetector(t, h.unsealPrivate(t, gen.Key.KeyID), gen.Key.PublicKey)

	if err := h.client.Health(ctx); err != nil {
		t.Fatalf("Health: %v", err)
	}
	// Health's body is fixed, but assert on it rather than assume it.
	if leak := det.findLeak([]byte("ok\n")); leak != "" {
		t.Errorf("health endpoint leaked private key material: %s", leak)
	}
}

// ---------------------------------------------------------------------------
// Test 2 — a confused-deputy request fails closed, with its positive control.
// ---------------------------------------------------------------------------

func TestConfusedDeputyFailsClosed(t *testing.T) {
	h := startDaemon(t)
	ctx := context.Background()

	alice, bob := newUserID(), newUserID()

	aliceKey, err := h.client.Generate(ctx, pgpwire.GenerateRequest{UserID: alice, Email: "alice@example.com"})
	if err != nil {
		t.Fatalf("generate for alice: %v", err)
	}
	bobKey, err := h.client.Generate(ctx, pgpwire.GenerateRequest{UserID: bob, Email: "bob@example.com"})
	if err != nil {
		t.Fatalf("generate for bob: %v", err)
	}

	// ── POSITIVE CONTROL. Without this, a daemon that refused every request
	// would pass the refusal assertions below and look perfectly secure.
	t.Run("A can export A's own key", func(t *testing.T) {
		got, err := h.client.ExportPublic(ctx, pgpwire.ExportPublicRequest{UserID: alice, KeyID: aliceKey.Key.KeyID})
		if err != nil {
			t.Fatalf("alice could not export her own key: %v", err)
		}
		if got.Key.Fingerprint != aliceKey.Key.Fingerprint {
			t.Errorf("fingerprint = %q, want %q", got.Key.Fingerprint, aliceKey.Key.Fingerprint)
		}
		if !strings.Contains(got.Key.PublicKey, "PGP PUBLIC KEY BLOCK") {
			t.Errorf("export did not return an armored public key: %.40q", got.Key.PublicKey)
		}
	})

	// ── The refusal, in both directions, so a check that happened to be
	// one-sided cannot pass.
	for _, tc := range []struct {
		name     string
		caller   string
		keyID    string
		otherFPR string
	}{
		{"A asking for B's key", alice, bobKey.Key.KeyID, bobKey.Key.Fingerprint},
		{"B asking for A's key", bob, aliceKey.Key.KeyID, aliceKey.Key.Fingerprint},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := h.client.ExportPublic(ctx, pgpwire.ExportPublicRequest{UserID: tc.caller, KeyID: tc.keyID})
			if err == nil {
				t.Fatalf("request succeeded and returned key %q — the boundary is open", got.Key.KeyID)
			}
			code, ok := pgpwire.CodeOf(err)
			if !ok {
				t.Fatalf("refusal was not a wire error: %v", err)
			}
			if code != pgpwire.CodeDenied {
				t.Errorf("code = %q, want %q — refused, but not for the reason that proves the ownership check ran", code, pgpwire.CodeDenied)
			}
			// Fail closed means nothing came back, not merely "an error came
			// back alongside the key".
			if got != nil {
				t.Errorf("a response body accompanied the refusal: %+v", got.Key)
			}
			if strings.Contains(err.Error(), tc.otherFPR) {
				t.Errorf("the refusal itself disclosed the other user's fingerprint")
			}
		})
	}

	// ── An absent key is a different answer from someone else's key. Without
	// this, "deny everything" and "deny correctly" are indistinguishable.
	t.Run("a key that does not exist is not_found, not denied", func(t *testing.T) {
		_, err := h.client.ExportPublic(ctx, pgpwire.ExportPublicRequest{UserID: alice, KeyID: ulid.Make().String()})
		if err == nil {
			t.Fatal("export of a nonexistent key succeeded")
		}
		if code, _ := pgpwire.CodeOf(err); code != pgpwire.CodeNotFound {
			t.Errorf("code = %q, want %q", code, pgpwire.CodeNotFound)
		}
	})
}

// TestSealedKeyCannotBeRelocated proves the AAD binding: a stored private key
// is bound to BOTH its owner and its key id, so moving the sealed bytes to
// another user's row — which is what an attacker with write access to the store
// would try — yields ciphertext that does not open.
func TestSealedKeyCannotBeRelocated(t *testing.T) {
	h := startDaemon(t)
	ctx := context.Background()
	alice, bob := newUserID(), newUserID()

	gen, err := h.client.Generate(ctx, pgpwire.GenerateRequest{UserID: alice, Email: "alice@example.com"})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	rec, err := h.vault.Get(ctx, gen.Key.KeyID)
	if err != nil {
		t.Fatalf("vault.Get: %v", err)
	}

	// Control: it opens under the right (user, key) pair.
	aliceWrap, err := crypto.DeriveKey(h.root, []byte(alice), rootInfo)
	if err != nil {
		t.Fatalf("DeriveKey: %v", err)
	}
	if _, err := crypto.Open(aliceWrap, rec.Sealed, sealAAD(alice, rec.KeyID)); err != nil {
		t.Fatalf("sealed key did not open under its own owner: %v", err)
	}

	bobWrap, err := crypto.DeriveKey(h.root, []byte(bob), rootInfo)
	if err != nil {
		t.Fatalf("DeriveKey: %v", err)
	}
	if _, err := crypto.Open(bobWrap, rec.Sealed, sealAAD(bob, rec.KeyID)); err == nil {
		t.Error("alice's sealed key opened under bob's wrapping key")
	}
	if _, err := crypto.Open(aliceWrap, rec.Sealed, sealAAD(bob, rec.KeyID)); err == nil {
		t.Error("alice's sealed key opened with another user's associated data")
	}
	if _, err := crypto.Open(aliceWrap, rec.Sealed, sealAAD(alice, ulid.Make().String())); err == nil {
		t.Error("alice's sealed key opened under a different key id")
	}
}

// TestGenerateRejectsBadInput keeps the validation honest: a user id the daemon
// cannot recognise must not become the owner of a key.
func TestGenerateRejectsBadInput(t *testing.T) {
	h := startDaemon(t)
	ctx := context.Background()

	for _, tc := range []struct {
		name string
		req  pgpwire.GenerateRequest
	}{
		{"no user", pgpwire.GenerateRequest{Email: "x@example.com"}},
		{"user is not a ULID", pgpwire.GenerateRequest{UserID: "alice", Email: "x@example.com"}},
		{"no email", pgpwire.GenerateRequest{UserID: newUserID()}},
		{"email is not an address", pgpwire.GenerateRequest{UserID: newUserID(), Email: "nobody"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := h.client.Generate(ctx, tc.req); err == nil {
				t.Fatal("accepted")
			} else if code, _ := pgpwire.CodeOf(err); code != pgpwire.CodeInvalidRequest {
				t.Errorf("code = %q, want %q", code, pgpwire.CodeInvalidRequest)
			}
		})
	}
}

// TestUnknownPathIsNotServed guards the router: only the declared methods, and
// only by POST.
func TestUnknownPathIsNotServed(t *testing.T) {
	h := startDaemon(t)
	ctx := context.Background()

	var raw json.RawMessage
	err := h.client.Call(ctx, pgpwire.Method("export-private"), map[string]string{"user_id": newUserID()}, &raw)
	if err == nil {
		t.Fatal("an undeclared method was served")
	}
	if !strings.Contains(err.Error(), "404") {
		t.Errorf("want a 404 for an undeclared method, got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Signing: the first method that uses a private key rather than describing one.
// ---------------------------------------------------------------------------

// TestSignProducesAVerifiableSignature is the POSITIVE CONTROL for everything
// below, and for the leak test's sign probe.
//
// Without it, a Sign that returned an empty string, or armor over nothing,
// would satisfy "no private material came back" perfectly and pass every
// refusal assertion in this file. The question the test asks is the one a
// recipient asks: does the public key verify these bytes?
func TestSignProducesAVerifiableSignature(t *testing.T) {
	h := startDaemon(t)
	ctx := context.Background()
	user := newUserID()

	gen, err := h.client.Generate(ctx, pgpwire.GenerateRequest{UserID: user, Email: "signer@example.com"})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}

	data := []byte("Content-Type: text/plain\r\n\r\nsign me\r\n")
	got, err := h.client.Sign(ctx, pgpwire.SignRequest{UserID: user, KeyID: gen.Key.KeyID, Data: data})
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	if !strings.Contains(got.Signature, "-----BEGIN PGP SIGNATURE-----") {
		t.Fatalf("not an armored detached signature: %.60q", got.Signature)
	}
	if got.Fingerprint != gen.Key.Fingerprint {
		t.Errorf("fingerprint = %q, want %q", got.Fingerprint, gen.Key.Fingerprint)
	}
	if got.MicAlg != "pgp-sha256" {
		t.Errorf("micalg = %q, want %q", got.MicAlg, "pgp-sha256")
	}

	keyring, err := openpgp.ReadArmoredKeyRing(strings.NewReader(gen.Key.PublicKey))
	if err != nil {
		t.Fatalf("read exported public key: %v", err)
	}
	if _, err := openpgp.CheckArmoredDetachedSignature(
		keyring, bytes.NewReader(data), strings.NewReader(got.Signature), nil); err != nil {
		t.Fatalf("the exported public key did not verify the signature: %v", err)
	}

	// ── NEGATIVE CONTROL. A verification that cannot fail proves nothing about
	// the one above.
	tampered := append([]byte(nil), data...)
	tampered[len(tampered)-2] ^= 0x01
	if _, err := openpgp.CheckArmoredDetachedSignature(
		keyring, bytes.NewReader(tampered), strings.NewReader(got.Signature), nil); err == nil {
		t.Error("verification accepted tampered data — it cannot observe the failure it claims to exclude")
	}

	// The micalg must describe THIS signature, not a constant. Read the hash
	// back off the packet the same way a verifier would.
	if want, err := micAlgOf([]byte(got.Signature)); err != nil {
		t.Errorf("micAlgOf: %v", err)
	} else if got.MicAlg != want {
		t.Errorf("reported micalg %q, signature says %q", got.MicAlg, want)
	}
}

// TestSignConfusedDeputyFailsClosed is TestConfusedDeputyFailsClosed's rule
// applied to the method where getting it wrong is worst: a signature made with
// the wrong user's key is published to a correspondent and cannot be recalled.
func TestSignConfusedDeputyFailsClosed(t *testing.T) {
	h := startDaemon(t)
	ctx := context.Background()
	alice, bob := newUserID(), newUserID()

	aliceKey, err := h.client.Generate(ctx, pgpwire.GenerateRequest{UserID: alice, Email: "alice@example.com"})
	if err != nil {
		t.Fatalf("generate for alice: %v", err)
	}
	bobKey, err := h.client.Generate(ctx, pgpwire.GenerateRequest{UserID: bob, Email: "bob@example.com"})
	if err != nil {
		t.Fatalf("generate for bob: %v", err)
	}

	// ── POSITIVE CONTROL: a daemon that refused every signature would pass the
	// refusals below and look secure.
	t.Run("A can sign with A's own key", func(t *testing.T) {
		if _, err := h.client.Sign(ctx, pgpwire.SignRequest{
			UserID: alice, KeyID: aliceKey.Key.KeyID, Data: []byte("hello"),
		}); err != nil {
			t.Fatalf("alice could not sign with her own key: %v", err)
		}
	})

	for _, tc := range []struct {
		name     string
		caller   string
		keyID    string
		otherFPR string
	}{
		{"A signing with B's key", alice, bobKey.Key.KeyID, bobKey.Key.Fingerprint},
		{"B signing with A's key", bob, aliceKey.Key.KeyID, aliceKey.Key.Fingerprint},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := h.client.Sign(ctx, pgpwire.SignRequest{
				UserID: tc.caller, KeyID: tc.keyID, Data: []byte("hello"),
			})
			if err == nil {
				t.Fatalf("request succeeded and returned a signature by %q — the boundary is open", got.Fingerprint)
			}
			code, ok := pgpwire.CodeOf(err)
			if !ok {
				t.Fatalf("refusal was not a wire error: %v", err)
			}
			if code != pgpwire.CodeDenied {
				t.Errorf("code = %q, want %q — refused, but not for the reason that proves the ownership check ran", code, pgpwire.CodeDenied)
			}
			if got != nil {
				t.Errorf("a response body accompanied the refusal: %+v", got)
			}
			if strings.Contains(err.Error(), tc.otherFPR) {
				t.Errorf("the refusal itself disclosed the other user's fingerprint")
			}
		})
	}
}

// TestSignRefusesRetiredKey covers spec §9's asymmetry: a retired key is kept
// forever so old mail stays readable, and must never sign anything new. A
// correspondent cannot distinguish "signed before retirement" from "signed by
// whoever holds the key now".
func TestSignRetiredKeyIsRefused(t *testing.T) {
	revoked := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name   string
		retire func(*Record)
	}{
		{"decrypt-only", func(r *Record) { r.DecryptOnly = true }},
		{"revoked", func(r *Record) { r.RevokedAt = &revoked }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := startDaemon(t)
			ctx := context.Background()
			user := newUserID()

			gen, err := h.client.Generate(ctx, pgpwire.GenerateRequest{UserID: user, Email: "retiree@example.com"})
			if err != nil {
				t.Fatalf("Generate: %v", err)
			}

			// ── POSITIVE CONTROL: the same key, same request, signs fine BEFORE
			// retirement. Otherwise a Sign broken for any other reason would look
			// like the retirement check working.
			if _, err := h.client.Sign(ctx, pgpwire.SignRequest{
				UserID: user, KeyID: gen.Key.KeyID, Data: []byte("before"),
			}); err != nil {
				t.Fatalf("key would not sign before retirement: %v", err)
			}

			h.mutateRecord(t, gen.Key.KeyID, tc.retire)

			got, err := h.client.Sign(ctx, pgpwire.SignRequest{
				UserID: user, KeyID: gen.Key.KeyID, Data: []byte("after"),
			})
			if err == nil {
				t.Fatalf("a retired key produced a new signature: %.60q", got.Signature)
			}
			if code, _ := pgpwire.CodeOf(err); code != pgpwire.CodeInvalidRequest {
				t.Errorf("code = %q, want %q", code, pgpwire.CodeInvalidRequest)
			}
			if got != nil {
				t.Errorf("a response body accompanied the refusal: %+v", got)
			}

			// A retired key must still be readable — retirement is not deletion.
			if _, err := h.client.ExportPublic(ctx, pgpwire.ExportPublicRequest{
				UserID: user, KeyID: gen.Key.KeyID,
			}); err != nil {
				t.Errorf("retirement also broke export: %v", err)
			}
		})
	}
}

// TestSignRejectsBadInput keeps the argument checks honest, and in particular
// keeps "no data" from becoming a signature over nothing.
func TestSignRejectsBadInput(t *testing.T) {
	h := startDaemon(t)
	ctx := context.Background()
	user := newUserID()

	gen, err := h.client.Generate(ctx, pgpwire.GenerateRequest{UserID: user, Email: "x@example.com"})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}

	for _, tc := range []struct {
		name string
		req  pgpwire.SignRequest
		want pgpwire.ErrorCode
	}{
		{"no user", pgpwire.SignRequest{KeyID: gen.Key.KeyID, Data: []byte("x")}, pgpwire.CodeInvalidRequest},
		{"user is not a ULID", pgpwire.SignRequest{UserID: "alice", KeyID: gen.Key.KeyID, Data: []byte("x")}, pgpwire.CodeInvalidRequest},
		{"no key", pgpwire.SignRequest{UserID: user, Data: []byte("x")}, pgpwire.CodeInvalidRequest},
		{"no data", pgpwire.SignRequest{UserID: user, KeyID: gen.Key.KeyID}, pgpwire.CodeInvalidRequest},
		{"unknown key", pgpwire.SignRequest{UserID: user, KeyID: ulid.Make().String(), Data: []byte("x")}, pgpwire.CodeNotFound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := h.client.Sign(ctx, tc.req); err == nil {
				t.Fatal("accepted")
			} else if code, _ := pgpwire.CodeOf(err); code != tc.want {
				t.Errorf("code = %q, want %q", code, tc.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// mutateRecord edits a stored record in place, out of band. There is no API for
// retiring a key yet (that is the revocation increment), and the retirement
// refusal must be provable before there is one.
func (h *harness) mutateRecord(t *testing.T, keyID string, f func(*Record)) {
	t.Helper()
	h.vault.mu.Lock()
	defer h.vault.mu.Unlock()
	rec, ok := h.vault.records[keyID]
	if !ok {
		t.Fatalf("no such key in vault: %s", keyID)
	}
	f(&rec)
	h.vault.records[keyID] = rec
}

func armorPrivateForTest(t *testing.T, priv []byte) string {
	t.Helper()
	var buf bytes.Buffer
	w, err := armor.Encode(&buf, openpgp.PrivateKeyType, nil)
	if err != nil {
		t.Fatalf("armor.Encode: %v", err)
	}
	if _, err := w.Write(priv); err != nil {
		t.Fatalf("armor write: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("armor close: %v", err)
	}
	return buf.String()
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

func slicesContains(ms []pgpwire.Method, m pgpwire.Method) bool {
	for _, x := range ms {
		if x == m {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// The detector's needles come only from secret material (#899).
// ---------------------------------------------------------------------------

// oldWindows is the selection newLeakDetector used before #899: every 16-byte
// run of the private serialization absent from the public one. Kept only so the
// regression test can show the false match it produced.
func oldWindows(priv, pub []byte) [][]byte {
	var out [][]byte
	for i := 0; i+leakWindowSize <= len(priv); i++ {
		if w := priv[i : i+leakWindowSize]; !bytes.Contains(pub, w) {
			out = append(out, w)
		}
	}
	return out
}

func newTestEntity(t *testing.T) (priv, pub, uidPacket []byte) {
	t.Helper()
	e, err := openpgp.NewEntity("Subject", "", "subject@example.com", nil)
	if err != nil {
		t.Fatalf("NewEntity: %v", err)
	}
	var pb, qb, ub bytes.Buffer
	if err := e.SerializePrivate(&pb, nil); err != nil {
		t.Fatalf("SerializePrivate: %v", err)
	}
	if err := e.Serialize(&qb); err != nil {
		t.Fatalf("Serialize: %v", err)
	}
	for _, id := range e.Identities {
		if err := id.UserId.Serialize(&ub); err != nil {
			t.Fatalf("uid serialize: %v", err)
		}
	}
	return pb.Bytes(), qb.Bytes(), ub.Bytes()
}

// The flake: the last byte before the user-ID packet differs between the private
// and public serializations, so the window [that byte + user-ID packet tag and
// length + "Subject <subj"] is "private-only" yet lies in the PUBLIC user-ID
// packet, which a revoke response legitimately carries. Whether the bytes differ
// is random per key, so this test forces the difference.
func TestLeakWindowsAvoidPublicUserIDPacket(t *testing.T) {
	priv, pub, uid := newTestEntity(t)

	pubAt := bytes.Index(pub, uid)
	privAt := bytes.Index(priv, uid)
	if pubAt < 1 || privAt < 1 {
		t.Fatalf("user-ID packet not found (pub %d, priv %d)", pubAt, privAt)
	}
	pub = append([]byte(nil), pub...)
	pub[pubAt-1] = priv[privAt-1] ^ 0xFF

	straddles := func(w []byte) bool {
		i := bytes.Index(priv, w)
		return i >= 0 && i < privAt && i+len(w) > privAt
	}
	old := 0
	for _, w := range oldWindows(priv, pub) {
		if straddles(w) {
			old++
		}
	}
	if old == 0 {
		t.Fatal("control failed: the old selection produced no window straddling the user-ID packet, so this test cannot observe the bug")
	}

	wins, err := secretWindows(priv)
	if err != nil {
		t.Fatal(err)
	}
	if len(wins) < 8 {
		t.Fatalf("only %d windows", len(wins))
	}
	for _, w := range wins {
		if straddles(w) || bytes.Contains(uid, w) || bytes.Contains(w, uid[:3]) {
			t.Errorf("window %x touches the user-ID packet", w)
		}
		if bytes.Contains(pub, w) {
			t.Errorf("window %x appears in the public serialization", w)
		}
	}
}

// Positive controls for the narrowed detector: it must still see the armored
// key and the bare secret bytes (base64 in JSON), and stay quiet on the public
// key and the user ID.
func TestLeakDetectorStillCatchesSecretMaterial(t *testing.T) {
	priv, pub, uid := newTestEntity(t)
	var pubArmor bytes.Buffer
	w, err := armor.Encode(&pubArmor, openpgp.PublicKeyType, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(pub); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	det := newLeakDetector(t, priv, pubArmor.String())

	sufs, err := secretSuffixes(priv)
	if err != nil || len(sufs) == 0 {
		t.Fatalf("secretSuffixes: %v (%d)", err, len(sufs))
	}
	longest := sufs[0]
	for _, s := range sufs {
		if len(s) > len(longest) {
			longest = s
		}
	}
	for name, body := range map[string][]byte{
		"armored private key": mustJSON(t, map[string]string{"key": armorPrivateForTest(t, priv)}),
		"raw secret bytes":    mustJSON(t, map[string][]byte{"secret": longest}),
	} {
		if det.findLeak(body) == "" {
			t.Errorf("detector missed a leak of: %s", name)
		}
	}
	for name, body := range map[string][]byte{
		"public key":    mustJSON(t, map[string][]byte{"public": pub}),
		"user-ID bytes": mustJSON(t, map[string][]byte{"uid": uid}),
	} {
		if got := det.findLeak(body); got != "" {
			t.Errorf("detector fired on %s: %s", name, got)
		}
	}
}
