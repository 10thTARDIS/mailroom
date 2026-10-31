// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package api

// Encrypted sends, driven through the real JMAP methods against a real database
// and the REAL crypto-process service (#47, ADR-038).
//
// The crypto side is pgpkeyd.Service called in-process (no socket): sign and
// encrypt are the daemon's own code, so what is joined here is the application's
// half — key eligibility and ranking, the pin, the Bcc refusal, the stored
// rewrite — to the daemon's, which is the join that shipped inert before (#48).

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/ProtonMail/go-crypto/openpgp/armor"
	"github.com/ProtonMail/go-crypto/openpgp/packet"
	"github.com/oklog/ulid/v2"

	"mailroom/internal/blob"
	"mailroom/internal/compose"
	"mailroom/internal/pgpkeyd"
	"mailroom/internal/pgpkeys"
	"mailroom/internal/pgpmime"
	"mailroom/internal/pgpverify"
	"mailroom/internal/pgpwire"
	"mailroom/internal/store"
	"mailroom/internal/wkd"
)

// inProcCrypto is the composer's Signer and Encrypter, backed by the daemon's
// service called directly.
type inProcCrypto struct{ svc *pgpkeyd.Service }

func (c inProcCrypto) Sign(ctx context.Context, userID, keyID ulid.ULID, data []byte) (string, string, error) {
	r, err := c.svc.Sign(ctx, pgpwire.SignRequest{UserID: userID.String(), KeyID: keyID.String(), Data: data})
	if err != nil {
		return "", "", err
	}
	return r.Signature, r.MicAlg, nil
}

func (c inProcCrypto) Encrypt(ctx context.Context, userID, keyID ulid.ULID, to []compose.EncryptTo, entity []byte) (string, error) {
	rs := make([]pgpwire.EncryptRecipient, 0, len(to))
	for _, t := range to {
		rs = append(rs, pgpwire.EncryptRecipient{Address: t.Address, PublicKey: t.PublicKey})
	}
	r, err := c.svc.Encrypt(ctx, pgpwire.EncryptRequest{
		UserID: userID.String(), KeyID: keyID.String(), Recipients: rs, Entity: entity,
	})
	if err != nil {
		return "", err
	}
	return r.Ciphertext, nil
}

type encHarness struct {
	*composeHarness
	blobs   blob.BlobStore
	svc     *pgpkeyd.Service
	fetcher *fakeFetcher
	a       apiFixture
	// senderPub is the sender's binary public key; senderFpr its fingerprint.
	senderPub []byte
	senderFpr string
	senderKey string
}

type fetchResult struct {
	key []byte
	err error
}

type fakeFetcher struct {
	mu      sync.Mutex
	results map[string]fetchResult
	calls   []string
}

func (f *fakeFetcher) Fetch(_ context.Context, addr string) ([]byte, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, addr)
	r, ok := f.results[addr]
	if !ok {
		return nil, "", wkd.ErrNoKey
	}
	return r.key, "https://example.test/wkd", r.err
}

func newEncHarness(t *testing.T) *encHarness {
	t.Helper()
	base := newAPIHarness(t)
	blobs, err := blob.NewFSBlobStore(t.TempDir()+"/blobs", make([]byte, 32))
	if err != nil {
		t.Fatalf("blob store: %v", err)
	}
	root := make([]byte, 32)
	for i := range root {
		root[i] = byte(i*3 + 1)
	}
	svc, err := pgpkeyd.NewService(root, pgpkeyd.NewMemoryVault(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	cr := inProcCrypto{svc: svc}
	ff := &fakeFetcher{results: map[string]fetchResult{}}
	c := compose.NewComposer(blobs, fakeKeys{}, time.Millisecond).WithSigner(cr).WithEncrypter(cr)
	base.api = New(base.store, WithComposer(c), WithKeyFetcher(ff))
	h := &encHarness{composeHarness: &composeHarness{apiHarness: base, blobs: blobs}, blobs: blobs, svc: svc, fetcher: ff}

	a, _ := seedTwo(t, base.super)
	h.a = a
	seedSendable(t, base.super, a, "me@example.com", "")

	ctx := context.Background()
	gen, err := svc.Generate(ctx, pgpwire.GenerateRequest{UserID: a.UserID.String(), Email: "me@example.com"})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	armored, err := armor.Decode(strings.NewReader(gen.Key.PublicKey))
	if err != nil {
		t.Fatal(err)
	}
	h.senderPub, _ = io.ReadAll(armored.Body)
	h.senderFpr, h.senderKey = gen.Key.Fingerprint, gen.Key.KeyID

	var identityID string
	if err := base.super.QueryRow(ctx, `SELECT id FROM app.identities WHERE user_id = $1 AND address = 'me@example.com'`,
		a.UserID.String()).Scan(&identityID); err != nil {
		t.Fatal(err)
	}
	if _, err := base.super.Exec(ctx, `INSERT INTO app.user_keys
	    (id, user_id, identity_id, kind, fingerprint, public_key, private_key_ct, dek_id)
	    VALUES ($1,$2,$3,'pgp',$4,$5,'\xdeadbeef'::bytea,$6)`,
		h.senderKey, a.UserID.String(), identityID, h.senderFpr, h.senderPub, store.NewULID().String()); err != nil {
		t.Fatalf("seed sender key: %v", err)
	}
	if _, err := base.super.Exec(ctx, `UPDATE app.identities SET default_key_id = $3 WHERE user_id = $1 AND id = $2`,
		a.UserID.String(), identityID, h.senderKey); err != nil {
		t.Fatal(err)
	}
	return h
}

// ---- key fixtures -----------------------------------------------------------

func newPeerKey(t *testing.T, email string, cfg *packet.Config) *openpgp.Entity {
	t.Helper()
	if cfg == nil {
		cfg = &packet.Config{}
	}
	cfg.Algorithm, cfg.Curve, cfg.V6Keys = packet.PubKeyAlgoEdDSA, packet.Curve25519, false
	e, err := openpgp.NewEntity("Peer", "", email, cfg)
	if err != nil {
		t.Fatalf("NewEntity: %v", err)
	}
	return e
}

func pubOf(t *testing.T, e *openpgp.Entity) []byte {
	t.Helper()
	var b bytes.Buffer
	if err := e.Serialize(&b); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// seedKey files pub against the contact for address (creating it), the way the
// ingest / discovery / import paths would leave it.
func (h *encHarness) seedKey(t *testing.T, address string, pub []byte, source string, authenticated bool, fetched time.Time) string {
	t.Helper()
	ctx := context.Background()
	var cid string
	err := h.super.QueryRow(ctx, `SELECT contact_id FROM app.contact_addresses WHERE user_id=$1 AND address=$2`,
		h.a.UserID.String(), address).Scan(&cid)
	if err != nil {
		cid = store.NewULID().String()
		if _, err := h.super.Exec(ctx, `INSERT INTO app.contacts (id, user_id, display_name) VALUES ($1,$2,'Peer')`,
			cid, h.a.UserID.String()); err != nil {
			t.Fatal(err)
		}
		if _, err := h.super.Exec(ctx, `INSERT INTO app.contact_addresses (id, user_id, contact_id, address) VALUES ($1,$2,$3,$4)`,
			store.NewULID().String(), h.a.UserID.String(), cid, address); err != nil {
			t.Fatal(err)
		}
	}
	k, err := pgpkeys.Parse(pub)
	if err != nil {
		t.Fatalf("fixture key does not parse: %v", err)
	}
	if _, err := h.super.Exec(ctx, `INSERT INTO app.contact_keys
	    (id, user_id, contact_id, kind, fingerprint, public_key, source, authenticated, fetched_at)
	    VALUES ($1,$2,$3,'pgp',$4,$5,$6,$7,$8)`,
		store.NewULID().String(), h.a.UserID.String(), cid, k.Fingerprint, pub, source, authenticated, fetched); err != nil {
		t.Fatalf("seed contact key: %v", err)
	}
	return k.Fingerprint
}

// ---- compose helpers --------------------------------------------------------

func (h *encHarness) draft(t *testing.T, to, bcc []string) string {
	t.Helper()
	addrs := func(in []string) []map[string]any {
		var out []map[string]any
		for _, a := range in {
			out = append(out, map[string]any{"email": a})
		}
		return out
	}
	create := map[string]any{
		"from":       []map[string]any{{"email": "me@example.com"}},
		"to":         addrs(to),
		"subject":    "quarterly",
		"bodyValues": map[string]any{"1": map[string]any{"value": "the quarterly numbers are attached\n"}},
		"textBody":   []map[string]any{{"partId": "1"}},
	}
	if len(bcc) > 0 {
		create["bcc"] = addrs(bcc)
	}
	res := call(t, h.api.emailSet, h.a.UserID, map[string]any{"create": map[string]any{"c1": create}})
	obj, ok := res["created"].(map[string]any)["c1"].(map[string]any)
	if !ok {
		t.Fatalf("draft not created: %#v", res)
	}
	return obj["id"].(string)
}

// submit returns the notCreated error (nil when the submission was created).
func (h *encHarness) submit(t *testing.T, emailID string, encrypt bool) map[string]any {
	t.Helper()
	create := map[string]any{"emailId": emailID}
	if encrypt {
		create["encrypt"] = true
	}
	res := call(t, h.api.submissionSet, h.a.UserID, map[string]any{"create": map[string]any{"s1": create}})
	if nc, ok := res["notCreated"].(map[string]any); ok {
		if e, ok := nc["s1"].(map[string]any); ok {
			return e
		}
	}
	if m, _ := res["created"].(map[string]any); m["s1"] == nil {
		t.Fatalf("submission neither created nor refused: %#v", res)
	}
	return nil
}

func (h *encHarness) hash(t *testing.T, emailID string) []byte {
	t.Helper()
	var hh []byte
	if err := h.super.QueryRow(context.Background(),
		`SELECT content_hash FROM app.messages WHERE user_id=$1 AND id=$2`, h.a.UserID.String(), emailID).Scan(&hh); err != nil {
		t.Fatal(err)
	}
	return hh
}

func (h *encHarness) pin(t *testing.T, address string) (fpr string, ok bool) {
	t.Helper()
	err := h.super.QueryRow(context.Background(),
		`SELECT fingerprint FROM app.contact_key_pins WHERE user_id=$1 AND address=$2`, h.a.UserID.String(), address).Scan(&fpr)
	return fpr, err == nil
}

// refused asserts the submission was refused as cannotEncrypt, left the draft's
// bytes alone, and returns the description.
func (h *encHarness) refused(t *testing.T, emailID string, errObj map[string]any, before []byte) string {
	t.Helper()
	if errObj == nil {
		t.Fatal("the send was ACCEPTED; it had to be refused")
	}
	if errObj["type"] != "cannotEncrypt" {
		t.Fatalf("error type = %v, want cannotEncrypt: %v", errObj["type"], errObj)
	}
	if string(h.hash(t, emailID)) != string(before) {
		t.Error("a refused encrypted send REWROTE the stored draft")
	}
	d, _ := errObj["description"].(string)
	return d
}

// ---- the tests --------------------------------------------------------------

func TestEncryptedSubmitEndToEnd(t *testing.T) {
	h := newEncHarness(t)
	ctx := context.Background()
	bob := newPeerKey(t, "bob@example.com", nil)
	h.seedKey(t, "bob@example.com", pubOf(t, bob), store.ContactKeyManual, false, time.Now())
	id := h.draft(t, []string{"bob@example.com"}, nil)
	before := h.hash(t, id)

	var snippetBefore string
	_ = h.super.QueryRow(ctx, `SELECT snippet FROM app.messages WHERE id=$1`, id).Scan(&snippetBefore)
	if !strings.Contains(snippetBefore, "quarterly") {
		t.Fatalf("CONTROL: the draft's snippet is %q; the blanking below would prove nothing", snippetBefore)
	}

	if e := h.submit(t, id, true); e != nil {
		t.Fatalf("encrypted submit refused: %v", e)
	}
	raw := storedBytes(t, h.composeHarness, h.blobs, h.a.UserID, id)
	if !pgpmime.IsEncrypted(raw) {
		t.Fatalf("the STORED message is not multipart/encrypted:\n%.300s", raw)
	}
	if bytes.Contains(raw, []byte("quarterly numbers")) {
		t.Fatal("the body is in the clear in the stored (and so sent) message")
	}
	if string(h.hash(t, id)) == string(before) {
		t.Error("the content hash did not move")
	}
	var snippetAfter string
	_ = h.super.QueryRow(ctx, `SELECT snippet FROM app.messages WHERE id=$1`, id).Scan(&snippetAfter)
	if snippetAfter != "" {
		t.Errorf("the plaintext snippet %q survives beside the ciphertext", snippetAfter)
	}

	ct, err := pgpmime.SplitEncrypted(raw)
	if err != nil {
		t.Fatal(err)
	}
	// Bob's client.
	blk, _ := armor.Decode(bytes.NewReader(ct))
	md, err := openpgp.ReadMessage(blk.Body, openpgp.EntityList{bob}, nil, nil)
	if err != nil {
		t.Fatalf("bob cannot open it: %v", err)
	}
	entity, _ := io.ReadAll(md.UnverifiedBody)
	if !bytes.Contains(entity, []byte("multipart/signed")) {
		t.Errorf("the entity inside is not the signed container:\n%.200s", entity)
	}
	// The sender's Sent copy, through the daemon's decrypt, and the signature.
	dec, err := h.svc.Decrypt(ctx, pgpwire.DecryptRequest{
		UserID: h.a.UserID.String(), MessageID: id, Purpose: pgpwire.DecryptForBody, Ciphertext: ct,
	})
	if err != nil {
		t.Fatalf("the sender cannot read their own Sent copy: %v", err)
	}
	msg, err := pgpmime.Rewrap(raw, dec.Entity)
	if err != nil {
		t.Fatal(err)
	}
	res := pgpverify.Verify(msg, []pgpverify.PublicKey{{Fingerprint: h.senderFpr, Binary: h.senderPub}})
	if res.Verdict != pgpverify.VerdictVerified {
		t.Errorf("signature inside the encrypted message: %s (%v)", res.Verdict, res.Err)
	}
	// The fingerprint used is pinned.
	if fpr, ok := h.pin(t, "bob@example.com"); !ok || fpr != strings.ToUpper(hexOf(bob)) {
		t.Errorf("pin = %q ok=%v, want bob's fingerprint", fpr, ok)
	}

	// Re-sending the same (already encrypted) draft leaves it alone.
	h2 := h.hash(t, id)
	h.submit(t, id, true)
	if string(h.hash(t, id)) != string(h2) {
		t.Error("a re-submit re-encrypted an already encrypted message")
	}
}

func hexOf(e *openpgp.Entity) string {
	var b strings.Builder
	for _, c := range e.PrimaryKey.Fingerprint {
		b.WriteString(strings.ToUpper(string("0123456789abcdef"[c>>4])) + strings.ToUpper(string("0123456789abcdef"[c&15])))
	}
	return b.String()
}

func TestEncryptedSubmitWithoutAskingIsNotEncrypted(t *testing.T) {
	h := newEncHarness(t)
	bob := newPeerKey(t, "bob@example.com", nil)
	h.seedKey(t, "bob@example.com", pubOf(t, bob), store.ContactKeyManual, false, time.Now())
	id := h.draft(t, []string{"bob@example.com"}, nil)
	if e := h.submit(t, id, false); e != nil {
		t.Fatal(e)
	}
	raw := storedBytes(t, h.composeHarness, h.blobs, h.a.UserID, id)
	if pgpmime.IsEncrypted(raw) {
		t.Error("a send that did not ask for encryption was encrypted")
	}
	if _, ok := h.pin(t, "bob@example.com"); ok {
		t.Error("a plaintext send pinned a key")
	}
}

func TestEncryptedSubmitRefusesBcc(t *testing.T) {
	h := newEncHarness(t)
	for _, a := range []string{"bob@example.com", "carol@example.com"} {
		h.seedKey(t, a, pubOf(t, newPeerKey(t, a, nil)), store.ContactKeyManual, false, time.Now())
	}
	id := h.draft(t, []string{"bob@example.com"}, []string{"carol@example.com"})
	before := h.hash(t, id)
	d := h.refused(t, id, h.submit(t, id, true), before)
	if !strings.Contains(d, "Bcc") {
		t.Errorf("the message does not say why: %s", d)
	}
	// CONTROL: the same draft sends fine when encryption is not requested, so the
	// refusal is about Bcc and encryption together.
	if e := h.submit(t, id, false); e != nil {
		t.Errorf("CONTROL FAILED: the Bcc draft cannot be sent plainly either: %v", e)
	}
}

func TestEncryptedSubmitNamesRecipientsWithoutKeys(t *testing.T) {
	h := newEncHarness(t)
	h.seedKey(t, "bob@example.com", pubOf(t, newPeerKey(t, "bob@example.com", nil)), store.ContactKeyManual, false, time.Now())
	id := h.draft(t, []string{"bob@example.com", "nokey@example.com", "alsonone@example.org"}, nil)
	before := h.hash(t, id)
	d := h.refused(t, id, h.submit(t, id, true), before)
	for _, want := range []string{"nokey@example.com", "alsonone@example.org"} {
		if !strings.Contains(d, want) {
			t.Errorf("description does not name %s: %s", want, d)
		}
	}
	if strings.Contains(d, "bob@example.com") {
		t.Errorf("description names a recipient that has a key: %s", d)
	}
}

func TestEncryptedSubmitSkipsRevokedAndExpiredKeys(t *testing.T) {
	h := newEncHarness(t)
	old := time.Now().Add(-72 * time.Hour)

	revoked := newPeerKey(t, "revoked@example.com", nil)
	if err := revoked.RevokeKey(packet.KeyCompromised, "x", nil); err != nil {
		t.Fatal(err)
	}
	expired := newPeerKey(t, "expired@example.com", &packet.Config{
		KeyLifetimeSecs: 3600, Time: func() time.Time { return old },
	})
	h.seedKey(t, "revoked@example.com", pubOf(t, revoked), store.ContactKeyManual, false, time.Now())
	h.seedKey(t, "expired@example.com", pubOf(t, expired), store.ContactKeyManual, false, time.Now())

	id := h.draft(t, []string{"revoked@example.com", "expired@example.com"}, nil)
	d := h.refused(t, id, h.submit(t, id, true), h.hash(t, id))
	for _, want := range []string{"revoked@example.com", "expired@example.com"} {
		if !strings.Contains(d, want) {
			t.Errorf("%s was not skipped: %s", want, d)
		}
	}
}

// ---- layer 1: only authenticated Autocrypt keys ------------------------------

func TestEncryptedSubmitIgnoresUnauthenticatedAutocrypt(t *testing.T) {
	h := newEncHarness(t)
	forged := newPeerKey(t, "alice@example.com", nil)
	h.seedKey(t, "alice@example.com", pubOf(t, forged), store.ContactKeyAutocrypt, false, time.Now())
	id := h.draft(t, []string{"alice@example.com"}, nil)
	d := h.refused(t, id, h.submit(t, id, true), h.hash(t, id))
	if !strings.Contains(d, "alice@example.com") {
		t.Errorf("description = %s", d)
	}

	// CONTROL: the same key, authenticated, IS eligible — so the refusal above was
	// the authentication and nothing else about the key.
	h2 := newEncHarness(t)
	h2.seedKey(t, "alice@example.com", pubOf(t, forged), store.ContactKeyAutocrypt, true, time.Now())
	id2 := h2.draft(t, []string{"alice@example.com"}, nil)
	if e := h2.submit(t, id2, true); e != nil {
		t.Fatalf("CONTROL FAILED: an authenticated Autocrypt key is refused: %v", e)
	}
}

// ---- layer 2: source ranking -------------------------------------------------

func TestEncryptedSubmitSourceRanking(t *testing.T) {
	now := time.Now()
	cases := []struct {
		name    string
		seed    func(h *encHarness) (want string)
		address string
	}{
		{"a newer authenticated Autocrypt key does not displace an imported key", func(h *encHarness) string {
			imported := newPeerKey(t, "bob@example.com", nil)
			fpr := h.seedKey(t, "bob@example.com", pubOf(t, imported), store.ContactKeyManual, false, now.Add(-48*time.Hour))
			h.seedKey(t, "bob@example.com", pubOf(t, newPeerKey(t, "bob@example.com", nil)), store.ContactKeyAutocrypt, true, now)
			return fpr
		}, "bob@example.com"},
		{"a newer WKD key does not displace an imported key", func(h *encHarness) string {
			fpr := h.seedKey(t, "bob@example.com", pubOf(t, newPeerKey(t, "bob@example.com", nil)), store.ContactKeyManual, false, now.Add(-48*time.Hour))
			h.seedKey(t, "bob@example.com", pubOf(t, newPeerKey(t, "bob@example.com", nil)), store.ContactKeyWKD, false, now)
			return fpr
		}, "bob@example.com"},
		{"a newer authenticated Autocrypt key does not displace a WKD key", func(h *encHarness) string {
			fpr := h.seedKey(t, "bob@example.com", pubOf(t, newPeerKey(t, "bob@example.com", nil)), store.ContactKeyWKD, false, now.Add(-48*time.Hour))
			h.seedKey(t, "bob@example.com", pubOf(t, newPeerKey(t, "bob@example.com", nil)), store.ContactKeyAutocrypt, true, now)
			return fpr
		}, "bob@example.com"},
		{"a newer UNAUTHENTICATED Autocrypt key does not displace an imported key", func(h *encHarness) string {
			fpr := h.seedKey(t, "bob@example.com", pubOf(t, newPeerKey(t, "bob@example.com", nil)), store.ContactKeyManual, false, now.Add(-48*time.Hour))
			h.seedKey(t, "bob@example.com", pubOf(t, newPeerKey(t, "bob@example.com", nil)), store.ContactKeyAutocrypt, false, now)
			return fpr
		}, "bob@example.com"},
		{"within one rank the newest wins", func(h *encHarness) string {
			h.seedKey(t, "bob@example.com", pubOf(t, newPeerKey(t, "bob@example.com", nil)), store.ContactKeyWKD, false, now.Add(-48*time.Hour))
			return h.seedKey(t, "bob@example.com", pubOf(t, newPeerKey(t, "bob@example.com", nil)), store.ContactKeyWKD, false, now)
		}, "bob@example.com"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newEncHarness(t)
			want := tc.seed(h)
			id := h.draft(t, []string{tc.address}, nil)
			if e := h.submit(t, id, true); e != nil {
				t.Fatalf("refused: %v", e)
			}
			if got, _ := h.pin(t, tc.address); got != want {
				t.Errorf("encrypted to %s, want %s", got, want)
			}
		})
	}
}

// ---- layer 3: the pin --------------------------------------------------------

func TestEncryptedSubmitRefusesAChangedKey(t *testing.T) {
	h := newEncHarness(t)
	first := newPeerKey(t, "bob@example.com", nil)
	firstFpr := h.seedKey(t, "bob@example.com", pubOf(t, first), store.ContactKeyWKD, false, time.Now().Add(-48*time.Hour))
	id1 := h.draft(t, []string{"bob@example.com"}, nil)
	if e := h.submit(t, id1, true); e != nil {
		t.Fatalf("first send refused: %v", e)
	}
	if got, _ := h.pin(t, "bob@example.com"); got != firstFpr {
		t.Fatalf("first send pinned %q, want %q", got, firstFpr)
	}

	// A different, newer key for the same address (rotated, or planted).
	second := newPeerKey(t, "bob@example.com", nil)
	secondFpr := h.seedKey(t, "bob@example.com", pubOf(t, second), store.ContactKeyWKD, false, time.Now())
	id2 := h.draft(t, []string{"bob@example.com"}, nil)
	d := h.refused(t, id2, h.submit(t, id2, true), h.hash(t, id2))
	if !strings.Contains(d, "changed") || !strings.Contains(d, firstFpr) || !strings.Contains(d, secondFpr) {
		t.Errorf("the refusal does not say the key changed, with both fingerprints: %s", d)
	}
	if got, _ := h.pin(t, "bob@example.com"); got != firstFpr {
		t.Errorf("a REFUSED send moved the pin to %q", got)
	}

	// The lookup exposes the same state.
	res := call(t, h.api.recipientKeyLookup, h.a.UserID, map[string]any{"addresses": []string{"bob@example.com"}, "wkd": false})
	row := res["list"].([]any)[0].(map[string]any)
	if row["keyChanged"] != true || row["usable"] != false || row["pinnedFingerprint"] != firstFpr || row["fingerprint"] != secondFpr {
		t.Errorf("RecipientKey/lookup = %v", row)
	}

	// An explicitly IMPORTED key may replace the pin.
	imported := newPeerKey(t, "bob@example.com", nil)
	importedFpr := h.seedKey(t, "bob@example.com", pubOf(t, imported), store.ContactKeyManual, false, time.Now())
	id3 := h.draft(t, []string{"bob@example.com"}, nil)
	if e := h.submit(t, id3, true); e != nil {
		t.Fatalf("an imported key was refused as a change: %v", e)
	}
	if got, _ := h.pin(t, "bob@example.com"); got != importedFpr {
		t.Errorf("pin = %q, want the imported key %q", got, importedFpr)
	}
}

// ---- layer 4: the key must name the address ---------------------------------

func TestEncryptedSubmitKeyMustNameTheAddress(t *testing.T) {
	h := newEncHarness(t)
	other := newPeerKey(t, "someone-else@example.com", nil)
	h.seedKey(t, "bob@example.com", pubOf(t, other), store.ContactKeyManual, false, time.Now())
	id := h.draft(t, []string{"bob@example.com"}, nil)
	d := h.refused(t, id, h.submit(t, id, true), h.hash(t, id))
	if !strings.Contains(d, "bob@example.com") {
		t.Errorf("description = %s", d)
	}

	// CONTROL: a key that names it (case-insensitively) is fine.
	h2 := newEncHarness(t)
	h2.seedKey(t, "bob@example.com", pubOf(t, newPeerKey(t, "Bob@Example.COM", nil)), store.ContactKeyManual, false, time.Now())
	id2 := h2.draft(t, []string{"bob@example.com"}, nil)
	if e := h2.submit(t, id2, true); e != nil {
		t.Fatalf("CONTROL FAILED: a key naming the address (mixed case) is refused: %v", e)
	}
}

// ---- the lookup method -------------------------------------------------------

func TestRecipientKeyLookup(t *testing.T) {
	h := newEncHarness(t)
	stored := newPeerKey(t, "stored@example.com", nil)
	h.seedKey(t, "stored@example.com", pubOf(t, stored), store.ContactKeyManual, false, time.Now())
	published := newPeerKey(t, "published@example.net", nil)
	revoked := newPeerKey(t, "revoked@example.net", nil)
	if err := revoked.RevokeKey(packet.KeyCompromised, "x", nil); err != nil {
		t.Fatal(err)
	}
	h.fetcher.results["published@example.net"] = fetchResult{key: pubOf(t, published)}
	h.fetcher.results["revoked@example.net"] = fetchResult{key: pubOf(t, revoked)}
	h.fetcher.results["down@example.net"] = fetchResult{err: errors.New("dial: refused")}

	lookup := func(askWKD bool, addrs ...string) map[string]map[string]any {
		res := call(t, h.api.recipientKeyLookup, h.a.UserID, map[string]any{"addresses": addrs, "wkd": askWKD})
		out := map[string]map[string]any{}
		for _, r := range res["list"].([]any) {
			m := r.(map[string]any)
			out[m["address"].(string)] = m
		}
		return out
	}

	// Without the network: stored is usable, the rest are not, and nothing was fetched.
	got := lookup(false, "stored@example.com", "published@example.net")
	if got["stored@example.com"]["usable"] != true || got["stored@example.com"]["source"] != "manual" {
		t.Errorf("stored: %v", got["stored@example.com"])
	}
	if got["published@example.net"]["usable"] != false || got["published@example.net"]["wkd"] != "notAsked" {
		t.Errorf("published without wkd: %v", got["published@example.net"])
	}
	if len(h.fetcher.calls) != 0 {
		t.Errorf("wkd:false still fetched: %v", h.fetcher.calls)
	}
	// No key material on the wire.
	if raw := mustJSON(t, got); bytes.Contains(raw, []byte("BEGIN PGP")) {
		t.Error("the lookup returns key material")
	}

	// With the network.
	got = lookup(true, "stored@example.com", "published@example.net", "revoked@example.net", "down@example.net", "nobody@example.net")
	if got["published@example.net"]["usable"] != true || got["published@example.net"]["wkd"] != "found" ||
		got["published@example.net"]["source"] != "wkd" {
		t.Errorf("published: %v", got["published@example.net"])
	}
	if got["revoked@example.net"]["usable"] != false || got["revoked@example.net"]["wkd"] != "unusable" {
		t.Errorf("revoked: %v", got["revoked@example.net"])
	}
	if got["down@example.net"]["wkd"] != "failed" || got["nobody@example.net"]["wkd"] != "none" {
		t.Errorf("down/none: %v %v", got["down@example.net"], got["nobody@example.net"])
	}
	if got["stored@example.com"]["wkd"] != "skipped" {
		t.Errorf("a stored key still went to the network: %v", got["stored@example.com"])
	}
	for _, c := range h.fetcher.calls {
		if c == "stored@example.com" {
			t.Error("fetched an address that already had a usable key")
		}
	}

	// The WKD key was STORED, so Submit (which never touches the network) uses it.
	var n int
	if err := h.super.QueryRow(context.Background(),
		`SELECT count(*) FROM app.contact_keys WHERE user_id=$1 AND source='wkd'`, h.a.UserID.String()).Scan(&n); err != nil || n != 1 {
		t.Errorf("wkd keys stored = %d (%v), want 1 (only the usable one)", n, err)
	}
	h.fetcher.calls = nil
	id := h.draft(t, []string{"published@example.net", "stored@example.com"}, nil)
	if e := h.submit(t, id, true); e != nil {
		t.Fatalf("encrypted submit to looked-up keys refused: %v", e)
	}
	if len(h.fetcher.calls) != 0 {
		t.Errorf("Submit touched the network: %v", h.fetcher.calls)
	}

	// Bad input.
	if _, mErr := h.api.recipientKeyLookup(context.Background(), h.a.UserID, mustJSON(t, map[string]any{"addresses": []string{"not an address"}})); mErr == nil {
		t.Error("a non-address was accepted")
	}
	if _, mErr := h.api.recipientKeyLookup(context.Background(), h.a.UserID, mustJSON(t, map[string]any{"addresses": []string{}})); mErr == nil {
		t.Error("an empty list was accepted")
	}
}
