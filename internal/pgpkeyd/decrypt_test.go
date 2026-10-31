// SPDX-License-Identifier: AGPL-3.0-only

package pgpkeyd

// Decrypt (#47 increment 6b, ADR-037), against a real daemon on a real Unix
// socket, spoken to by the real client — the boundary_test.go harness.
//
// Each test names the 2026-08-21 checklist item it pins. Every NEGATIVE
// assertion has its positive control beside it: a refusal test passes just as
// well against a daemon that refuses everything, so each one first shows the
// same request shape succeeding.

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/ProtonMail/go-crypto/openpgp/packet"
	"github.com/oklog/ulid/v2"

	"mailroom/internal/pgpwire"
)

const secretEntity = "Content-Type: text/plain; charset=utf-8\r\n\r\nthe quarterly numbers are in the attached sheet\r\n"

func newMessageID() string { return ulid.Make().String() }

// decryptAs calls Decrypt over the socket.
func decryptAs(t *testing.T, h *harness, user string, ct []byte) (*pgpwire.DecryptResponse, error) {
	t.Helper()
	return h.client.Decrypt(context.Background(), pgpwire.DecryptRequest{
		UserID: user, MessageID: newMessageID(), Purpose: pgpwire.DecryptForBody, Ciphertext: ct,
	})
}

// assertOpaque requires err to be EXACTLY the one opaque decrypt failure.
func assertOpaque(t *testing.T, label string, resp *pgpwire.DecryptResponse, err error) {
	t.Helper()
	if err == nil {
		t.Errorf("%s: decrypt SUCCEEDED (%d bytes) — it must fail", label, len(resp.Entity))
		return
	}
	var werr *pgpwire.Error
	if !errors.As(err, &werr) {
		t.Errorf("%s: not a wire error: %v", label, err)
		return
	}
	if werr.Code != pgpwire.CodeDecryptFailed || werr.Message != pgpwire.DecryptFailedMessage {
		t.Errorf("%s: got {%s %q}, want exactly {%s %q}", label, werr.Code, werr.Message,
			pgpwire.CodeDecryptFailed, pgpwire.DecryptFailedMessage)
	}
	if resp != nil {
		t.Errorf("%s: a response body accompanied the failure", label)
	}
}

func generate(t *testing.T, h *harness, user, email string) *pgpwire.GenerateResponse {
	t.Helper()
	g, err := h.client.Generate(context.Background(), pgpwire.GenerateRequest{UserID: user, Email: email})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	return g
}

// ── The happy path, and the audit row it leaves (items 4 and 7's input). ──

func TestDecryptReturnsTheEntityAndRecordsIt(t *testing.T) {
	h := startDaemon(t)
	user := newUserID()
	g := generate(t, h, user, "reader@example.com")
	ct := encryptTo(t, pubRing(t, g.Key.PublicKey), []byte(secretEntity), nil)

	msgID := newMessageID()
	resp, err := h.client.Decrypt(context.Background(), pgpwire.DecryptRequest{
		UserID:    strings.ToLower(user), // a case variant must canonicalise (#301 C1)
		MessageID: msgID, Purpose: pgpwire.DecryptForBody, Ciphertext: ct,
	})
	if err != nil {
		t.Fatalf("Decrypt: %v", err)
	}
	if string(resp.Entity) != secretEntity {
		t.Errorf("entity = %q, want %q", resp.Entity, secretEntity)
	}

	// Binary (dearmored) ciphertext is accepted too.
	if resp, err := decryptAs(t, h, user, dearmorMessage(t, ct)); err != nil || string(resp.Entity) != secretEntity {
		t.Errorf("binary ciphertext: %v", err)
	}

	audits := h.vault.Audits()
	if len(audits) != 2 {
		t.Fatalf("audit rows = %d, want one per call (2)", len(audits))
	}
	a := audits[0]
	if !a.Decrypted || a.UserID != user || a.MessageID != msgID || a.Purpose != "body" {
		t.Errorf("audit row = %+v", a)
	}
	if a.Fingerprint != g.Key.Fingerprint {
		t.Errorf("audit fingerprint = %q, want the decrypting key's primary %q", a.Fingerprint, g.Key.Fingerprint)
	}
}

// ── Item 1: one opaque error, identical across every cryptographic failure. ──

func TestDecryptFailuresAreIndistinguishable(t *testing.T) {
	h := startDaemon(t)
	user := newUserID()
	g := generate(t, h, user, "reader@example.com")
	mine := pubRing(t, g.Key.PublicKey)
	stranger := newLocalEntity(t, "stranger@example.com")

	good := encryptTo(t, mine, []byte(secretEntity), nil)

	// CONTROL: the shape of request used below succeeds when nothing is wrong.
	if _, err := decryptAs(t, h, user, good); err != nil {
		t.Fatalf("CONTROL FAILED: a well-formed message to the user's key did not decrypt: %v", err)
	}

	// A signed-but-not-encrypted OpenPGP message: valid packets, no encryption.
	var signedOnly bytes.Buffer
	w, err := openpgp.Sign(&signedOnly, stranger, &openpgp.FileHints{IsBinary: true}, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = w.Write([]byte(secretEntity))
	_ = w.Close()

	noKeysUser := newUserID()

	cases := map[string]struct {
		user string
		ct   []byte
	}{
		"wrong key":             {user, encryptTo(t, openpgp.EntityList{stranger}, []byte(secretEntity), nil)},
		"tampered MDC":          {user, tamperTail(t, good, 3)},
		"tampered body":         {user, tamperTail(t, good, 40)},
		"SED (no MDC)":          {user, armorMessage(t, craftedMessage(t, mine[0], sessionKey(t), []byte(secretEntity), true, 0))},
		"garbage armor":         {user, []byte("-----BEGIN PGP MESSAGE-----\n\nnot base64 at all!!\n-----END PGP MESSAGE-----\n")},
		"wrong armor type":      {user, []byte(g.Key.PublicKey)},
		"not OpenPGP at all":    {user, []byte("hello, this is plain text")},
		"truncated packets":     {user, dearmorMessage(t, good)[:40]},
		"signed, not encrypted": {user, armorMessage(t, signedOnly.Bytes())},
		"user holds no key":     {noKeysUser, good},
	}

	var first string
	for name, tc := range cases {
		resp, err := decryptAs(t, h, tc.user, tc.ct)
		assertOpaque(t, name, resp, err)
		if err == nil {
			continue
		}
		// The WHOLE error, as the client renders it from the wire, must be the
		// same string in every case — code, message, everything.
		if first == "" {
			first = err.Error()
		} else if err.Error() != first {
			t.Errorf("%s: error %q differs from %q — a distinguishable failure is an oracle", name, err.Error(), first)
		}
	}

	// Every failure was audited, as a failure, with no key named.
	var failed int
	for _, a := range h.vault.Audits() {
		if !a.Decrypted {
			failed++
			if a.Fingerprint != "" {
				t.Errorf("a failed decrypt recorded a key fingerprint: %+v", a)
			}
		}
	}
	if failed != len(cases) {
		t.Errorf("failed audit rows = %d, want %d (one per failure)", failed, len(cases))
	}
}

// ── Item 2: SED is refused outright — and the fixture is proven genuine. ──

func TestDecryptRefusesSED(t *testing.T) {
	h := startDaemon(t)
	user := newUserID()

	// A key the test can also hold locally, so it can prove the fixture opens.
	local := newLocalEntity(t, "sed@example.com")
	if _, err := h.client.Import(context.Background(), pgpwire.ImportRequest{
		UserID: user, PrivateKey: armorPrivateEntity(t, local),
	}); err != nil {
		t.Fatalf("Import: %v", err)
	}
	sed := craftedMessage(t, local, sessionKey(t), []byte(secretEntity), true, 0)

	// ── CONTROL: the fixture IS a decryptable SED message. go-crypto opens it
	// when, and only when, its insecure flag is set. Without this, "refused"
	// could mean "the fixture was garbage".
	md, err := openpgp.ReadMessage(bytes.NewReader(sed), openpgp.EntityList{local}, nil,
		&packet.Config{InsecureAllowUnauthenticatedMessages: true})
	if err != nil {
		t.Fatalf("CONTROL FAILED: the SED fixture does not open even with the insecure flag: %v", err)
	}
	got, err := io.ReadAll(md.UnverifiedBody)
	if err != nil || string(got) != secretEntity {
		t.Fatalf("CONTROL FAILED: SED fixture decrypted to %q (%v)", got, err)
	}
	// And go-crypto with the flag OFF refuses it — the line decryptConfig pins.
	if _, err := openpgp.ReadMessage(bytes.NewReader(sed), openpgp.EntityList{local}, nil, decryptConfig(1<<20)); err == nil {
		t.Fatal("decryptConfig() lets go-crypto read an SED message")
	}

	// THE ASSERTION: the daemon refuses it, opaquely, armored or binary.
	resp, err := decryptAs(t, h, user, sed)
	assertOpaque(t, "SED binary", resp, err)
	resp, err = decryptAs(t, h, user, armorMessage(t, sed))
	assertOpaque(t, "SED armored", resp, err)

	// Positive control on the same key: the integrity-protected twin decrypts.
	seipd := craftedMessage(t, local, sessionKey(t), []byte(secretEntity), false, 0)
	if resp, err := decryptAs(t, h, user, seipd); err != nil || string(resp.Entity) != secretEntity {
		t.Errorf("the SEIPD twin of the refused message did not decrypt: %v", err)
	}
}

// ── Item 3: bounds. Output cap (compressed and not), nesting, request size. ──

func TestNewServiceUsesTheWireDecryptCap(t *testing.T) {
	h := startDaemon(t)
	if h.svc.maxPlaintext != pgpwire.MaxDecryptedBytes {
		t.Errorf("maxPlaintext = %d, want pgpwire.MaxDecryptedBytes (%d)", h.svc.maxPlaintext, pgpwire.MaxDecryptedBytes)
	}
	if pgpwire.MethodDecrypt.MaxBytes() != pgpwire.MaxDecryptRequestBytes {
		t.Errorf("decrypt request bound = %d, want MaxDecryptRequestBytes", pgpwire.MethodDecrypt.MaxBytes())
	}
	if pgpwire.MethodDecrypt.MaxResponseBytes() < pgpwire.MaxDecryptedBytes*4/3 {
		t.Errorf("the client would truncate a maximal decrypt response")
	}
	if pgpwire.MethodImport.MaxBytes() != pgpwire.MaxRequestBytes {
		t.Error("import inherited a larger request bound")
	}
}

func TestDecryptCapsPlaintext(t *testing.T) {
	h := startDaemon(t)
	const capBytes = 64 << 10
	h.svc.maxPlaintext = capBytes
	user := newUserID()
	local := newLocalEntity(t, "bomb@example.com")
	if _, err := h.client.Import(context.Background(), pgpwire.ImportRequest{
		UserID: user, PrivateKey: armorPrivateEntity(t, local),
	}); err != nil {
		t.Fatalf("Import: %v", err)
	}
	msg := func(plain []byte, compressed bool) []byte {
		layers := 0
		if compressed {
			layers = 1
		}
		return craftedMessage(t, local, sessionKey(t), plain, false, layers)
	}
	atCap := bytes.Repeat([]byte("a"), capBytes)
	over := bytes.Repeat([]byte("a"), capBytes+1)
	bomb := make([]byte, 50*capBytes) // zeros: compresses to a few hundred bytes

	// CONTROL: exactly at the cap is fine, compressed or not.
	for _, compressed := range []bool{false, true} {
		if resp, err := decryptAs(t, h, user, msg(atCap, compressed)); err != nil || len(resp.Entity) != capBytes {
			t.Fatalf("CONTROL FAILED: a plaintext exactly at the cap (compressed=%v) was refused: %v", compressed, err)
		}
	}

	// UNCOMPRESSED one byte over: no decompression limit applies, so only the
	// daemon's own read cap can refuse this — the case the output-cap probe
	// turns red.
	resp, err := decryptAs(t, h, user, msg(over, false))
	assertOpaque(t, "uncompressed, one byte over the cap", resp, err)

	// A decompression bomb: tiny ciphertext, 50x the cap of zeros. Refused by
	// go-crypto's per-layer limit AND by the read cap.
	ct := msg(bomb, true)
	if len(ct) > capBytes/4 {
		t.Fatalf("the bomb fixture is %d bytes of ciphertext; it is meant to be tiny", len(ct))
	}
	resp, err = decryptAs(t, h, user, ct)
	assertOpaque(t, "decompression bomb", resp, err)
}

func TestDecryptRefusesDeepCompressionNesting(t *testing.T) {
	h := startDaemon(t)
	user := newUserID()
	local := newLocalEntity(t, "nest@example.com")
	if _, err := h.client.Import(context.Background(), pgpwire.ImportRequest{
		UserID: user, PrivateKey: armorPrivateEntity(t, local),
	}); err != nil {
		t.Fatalf("Import: %v", err)
	}
	// CONTROL: a few layers decrypt.
	shallow := craftedMessage(t, local, sessionKey(t), []byte(secretEntity), false, 3)
	if resp, err := decryptAs(t, h, user, shallow); err != nil || string(resp.Entity) != secretEntity {
		t.Fatalf("CONTROL FAILED: 3 nested compression layers did not decrypt: %v", err)
	}
	deep := craftedMessage(t, local, sessionKey(t), []byte(secretEntity), false, 40)
	resp, err := decryptAs(t, h, user, deep)
	assertOpaque(t, "40 nested compression layers", resp, err)
}

// TestDecryptRefusesAnOversizedRequest posts one byte more than the decrypt
// bound, streamed, straight at the socket.
func TestDecryptRefusesAnOversizedRequest(t *testing.T) {
	h := startDaemon(t)
	hc := &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", h.client.SocketPath())
		},
	}}
	body := io.LimitReader(repeatReader('A'), pgpwire.MaxDecryptRequestBytes+1)
	req, err := http.NewRequest(http.MethodPost, "http://crypto"+pgpwire.MethodDecrypt.Path(), body)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := hc.Do(req)
	if err != nil {
		// The server may close mid-upload once the limit trips; that is a refusal too.
		return
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusBadRequest || !strings.Contains(string(raw), string(pgpwire.CodeInvalidRequest)) {
		t.Errorf("oversized request: status %d body %q, want 400 invalid_request", resp.StatusCode, raw)
	}
}

type repeatReader byte

func (r repeatReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = byte(r)
	}
	return len(p), nil
}

// ── Item 4: the rate limit, and the audit that fails closed. ──

func TestDecryptRateLimitPerUser(t *testing.T) {
	h := startDaemon(t)
	h.svc.decryptLimit = newDecryptLimiter(2, 0) // two, and no refill
	alice, bob := newUserID(), newUserID()
	ga := generate(t, h, alice, "a@example.com")
	gb := generate(t, h, bob, "b@example.com")
	cta := encryptTo(t, pubRing(t, ga.Key.PublicKey), []byte(secretEntity), nil)
	ctb := encryptTo(t, pubRing(t, gb.Key.PublicKey), []byte(secretEntity), nil)

	for i := 0; i < 2; i++ {
		if _, err := decryptAs(t, h, alice, cta); err != nil {
			t.Fatalf("call %d inside the budget failed: %v", i+1, err)
		}
	}
	_, err := decryptAs(t, h, alice, cta)
	if code, _ := pgpwire.CodeOf(err); code != pgpwire.CodeRateLimited {
		t.Errorf("third call: err = %v, want %s", err, pgpwire.CodeRateLimited)
	}
	// Per USER: bob's budget is his own.
	if _, err := decryptAs(t, h, bob, ctb); err != nil {
		t.Errorf("bob was limited by alice's spending: %v", err)
	}
	// The refused call touched no key, so it left no audit row (it is logged).
	if n := len(h.vault.Audits()); n != 3 {
		t.Errorf("audit rows = %d, want 3 (the refused call is not a decryption)", n)
	}
}

func TestDecryptLimiterRefills(t *testing.T) {
	l := newDecryptLimiter(1, 1)
	t0 := time.Unix(1000, 0)
	if !l.allow("u", t0) || l.allow("u", t0) {
		t.Fatal("burst of 1 not honoured")
	}
	if !l.allow("u", t0.Add(1100*time.Millisecond)) {
		t.Error("no refill after one second")
	}
}

func TestDecryptFailsClosedWhenTheAuditCannotBeWritten(t *testing.T) {
	h := startDaemon(t)
	user := newUserID()
	g := generate(t, h, user, "audit@example.com")
	ct := encryptTo(t, pubRing(t, g.Key.PublicKey), []byte(secretEntity), nil)
	// CONTROL: it decrypts while the trail works.
	if _, err := decryptAs(t, h, user, ct); err != nil {
		t.Fatalf("CONTROL FAILED: %v", err)
	}
	h.vault.mu.Lock()
	h.vault.auditErr = errors.New("disk full")
	h.vault.mu.Unlock()
	resp, err := decryptAs(t, h, user, ct)
	if err == nil {
		t.Fatalf("decrypt returned %d bytes of plaintext with no audit row written", len(resp.Entity))
	}
	if code, _ := pgpwire.CodeOf(err); code != pgpwire.CodeInternal {
		t.Errorf("code = %v, want internal", code)
	}
}

// ── Item 5: retired keys STILL decrypt. The positive control the checklist asks for. ──

func TestRevokedKeyStillDecrypts(t *testing.T) {
	h := startDaemon(t)
	user := newUserID()

	// A key that encrypted mail while current...
	local := newLocalEntity(t, "old@example.com")
	oldMail := encryptTo(t, openpgp.EntityList{local}, []byte(secretEntity), nil)
	// ...then was REVOKED with a real OpenPGP revocation signature...
	if err := local.RevokeKey(packet.KeyCompromised, "rotated", nil); err != nil {
		t.Fatalf("RevokeKey: %v", err)
	}
	if !local.Revoked(time.Now()) {
		t.Fatal("fixture: the entity does not report itself revoked")
	}
	imp, err := h.client.Import(context.Background(), pgpwire.ImportRequest{
		UserID: user, PrivateKey: armorPrivateEntity(t, local),
	})
	if err != nil {
		t.Fatalf("Import of a revoked key: %v", err)
	}
	// ...and retired in Mailroom's own terms: decrypt_only AND revoked_at.
	now := time.Now()
	if !h.vault.retire(imp.Key.KeyID, true, &now) {
		t.Fatal("could not retire the imported key")
	}

	// CONTROL: the retirement is REAL — Sign refuses this key. Without this, a
	// green decrypt below could mean the retirement never applied.
	_, err = h.client.Sign(context.Background(), pgpwire.SignRequest{UserID: user, KeyID: imp.Key.KeyID, Data: []byte("x\r\n")})
	if code, _ := pgpwire.CodeOf(err); code != pgpwire.CodeInvalidRequest {
		t.Fatalf("CONTROL FAILED: Sign with the retired key: err = %v, want invalid_request — "+
			"the key is not actually retired, so the assertion below would prove nothing", err)
	}

	// THE ASSERTION: old mail still opens.
	resp, err := decryptAs(t, h, user, oldMail)
	if err != nil {
		t.Fatalf("a revoked, decrypt_only key did NOT decrypt mail encrypted to it before "+
			"revocation — retirement has orphaned the user's archive: %v", err)
	}
	if string(resp.Entity) != secretEntity {
		t.Errorf("entity = %q", resp.Entity)
	}
}

// ── Ownership: a user's request is offered only that user's keys. ──

func TestDecryptConfusedDeputyFailsClosed(t *testing.T) {
	h := startDaemon(t)
	alice, bob := newUserID(), newUserID()
	generate(t, h, alice, "alice@example.com")
	gb := generate(t, h, bob, "bob@example.com")
	toBob := encryptTo(t, pubRing(t, gb.Key.PublicKey), []byte(secretEntity), nil)

	// POSITIVE CONTROL: bob opens his own mail.
	if _, err := decryptAs(t, h, bob, toBob); err != nil {
		t.Fatalf("CONTROL FAILED: bob cannot decrypt mail to his own key: %v", err)
	}
	// MemoryVault returns EVERY user's records, so only the service's ownership
	// filter stands between alice's request and bob's key.
	resp, err := decryptAs(t, h, alice, toBob)
	assertOpaque(t, "alice asking for bob's mail", resp, err)
}

// ── Item 6: no session key and no private key bytes in any response or log. ──

func TestDecryptLeaksNeitherSessionKeyNorPrivateKey(t *testing.T) {
	var logBuf syncBuffer
	h := startDaemonWithLogger(t, slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	user := newUserID()

	local := newLocalEntity(t, "leak@example.com")
	imp, err := h.client.Import(context.Background(), pgpwire.ImportRequest{
		UserID: user, PrivateKey: armorPrivateEntity(t, local),
	})
	if err != nil {
		t.Fatal(err)
	}
	priv := h.unsealPrivate(t, imp.Key.KeyID)
	det := newLeakDetector(t, priv, mustExportPublic(t, h, user, imp.Key.KeyID))

	sk := sessionKey(t)
	ct := craftedMessage(t, local, sk, []byte(secretEntity), false, 1)

	var corpus []byte
	// A SUCCEEDING decrypt — a refusal would never touch the key or session key.
	var raw json.RawMessage
	if err := h.client.Call(context.Background(), pgpwire.MethodDecrypt, pgpwire.DecryptRequest{
		UserID: user, MessageID: newMessageID(), Purpose: pgpwire.DecryptForAttachment, Ciphertext: ct,
	}, &raw); err != nil {
		t.Fatalf("the probe decrypt failed, so nothing below examined a real decryption: %v", err)
	}
	var dr pgpwire.DecryptResponse
	if err := json.Unmarshal(raw, &dr); err != nil || string(dr.Entity) != secretEntity {
		t.Fatalf("probe decrypt returned %q (%v)", dr.Entity, err)
	}
	corpus = append(corpus, raw...)
	// And failures, whose bodies and logs are also output.
	for _, bad := range [][]byte{tamperTail(t, armorMessage(t, ct), 3), []byte("junk")} {
		_, err := decryptAs(t, h, user, bad)
		if err != nil {
			corpus = append(corpus, err.Error()...)
		}
	}

	logs := logBuf.Bytes()
	for name, body := range map[string][]byte{"responses": corpus, "daemon log": logs} {
		if leak := det.findLeak(body); leak != "" {
			t.Errorf("%s: private key material: %s", name, leak)
		}
		if sessionKeyIn(body, sk) {
			t.Errorf("%s: the SESSION KEY appears", name)
		}
		// And no library error text reaches either.
		if libText := goCryptoErrorText(body); libText != "" {
			t.Errorf("%s: go-crypto error text %q leaked", name, libText)
		}
	}

	// CONTROL on the instrument: the same search FIRES on a synthetic response
	// carrying the session key the way a leak would (JSON []byte is base64).
	if !sessionKeyIn(mustJSON(t, map[string][]byte{"k": sk}), sk) || !sessionKeyIn([]byte(hex.EncodeToString(sk)), sk) {
		t.Fatal("the session-key search cannot see a session key; every assertion above is vacuous")
	}
	// CONTROL: the log did record the decryption, so "no leak" is a statement
	// about a log that was written, not an empty one.
	if !strings.Contains(string(logs), "pgp message decrypted") {
		t.Fatalf("the daemon log never recorded the decrypt; the log assertions are vacuous:\n%s", logs)
	}
}

// sessionKeyIn searches every representation expand() knows for the key, raw,
// hex (both cases) and base64 (padded and not).
func sessionKeyIn(body, sk []byte) bool {
	encs := [][]byte{sk, []byte(hex.EncodeToString(sk)), []byte(strings.ToUpper(hex.EncodeToString(sk))),
		[]byte(base64.StdEncoding.EncodeToString(sk)), []byte(base64.RawStdEncoding.EncodeToString(sk))}
	for _, c := range expand(body) {
		for _, e := range encs {
			if bytes.Contains(c, e) {
				return true
			}
		}
	}
	return false
}

func mustExportPublic(t *testing.T, h *harness, user, keyID string) string {
	t.Helper()
	got, err := h.client.ExportPublic(context.Background(), pgpwire.ExportPublicRequest{UserID: user, KeyID: keyID})
	if err != nil {
		t.Fatalf("ExportPublic: %v", err)
	}
	return got.Key.PublicKey
}

// syncBuffer is a goroutine-safe log sink: the daemon logs from its handler
// goroutines while the test reads.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) Bytes() []byte {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]byte(nil), b.buf.Bytes()...)
}

// ── Argument validation is NOT a cryptographic failure, and says so. ──

func TestDecryptValidatesItsArguments(t *testing.T) {
	h := startDaemon(t)
	user := newUserID()
	for name, req := range map[string]pgpwire.DecryptRequest{
		"no user":        {MessageID: newMessageID(), Purpose: pgpwire.DecryptForBody, Ciphertext: []byte("x")},
		"bad message id": {UserID: user, MessageID: "nope", Purpose: pgpwire.DecryptForBody, Ciphertext: []byte("x")},
		"bad purpose":    {UserID: user, MessageID: newMessageID(), Purpose: "index", Ciphertext: []byte("x")},
		"no ciphertext":  {UserID: user, MessageID: newMessageID(), Purpose: pgpwire.DecryptForBody},
	} {
		_, err := h.client.Decrypt(context.Background(), req)
		if code, _ := pgpwire.CodeOf(err); code != pgpwire.CodeInvalidRequest {
			t.Errorf("%s: err = %v, want invalid_request", name, err)
		}
	}
	if n := len(h.vault.Audits()); n != 0 {
		t.Errorf("invalid requests wrote %d audit rows", n)
	}
}

// A vault without the decrypt capabilities refuses rather than decrypting
// without a trail.
func TestDecryptRequiresAnAuditingVault(t *testing.T) {
	root := make([]byte, 32)
	// struct{ Vault } exposes ONLY the Vault methods: no List, no RecordDecrypt.
	svc, err := NewService(root, struct{ Vault }{NewMemoryVault()}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	_, err = svc.Decrypt(context.Background(), pgpwire.DecryptRequest{
		UserID: newUserID(), MessageID: newMessageID(), Purpose: pgpwire.DecryptForBody, Ciphertext: []byte("x"),
	})
	if code, _ := pgpwire.CodeOf(err); code != pgpwire.CodeInternal {
		t.Errorf("err = %v, want internal (no KeyLister/DecryptAuditor)", err)
	}
}

// Item 3, memory: decrypts in flight are bounded. With every slot taken, a
// call waits — and gives up with its context rather than running anyway.
func TestDecryptConcurrencyIsBounded(t *testing.T) {
	h := startDaemon(t)
	if cap(h.svc.decryptSlots) != DefaultDecryptConcurrency {
		t.Fatalf("decrypt slots = %d, want DefaultDecryptConcurrency", cap(h.svc.decryptSlots))
	}
	user := newUserID()
	g := generate(t, h, user, "slots@example.com")
	ct := encryptTo(t, pubRing(t, g.Key.PublicKey), []byte(secretEntity), nil)
	for i := 0; i < cap(h.svc.decryptSlots); i++ {
		h.svc.decryptSlots <- struct{}{}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	_, err := h.svc.Decrypt(ctx, pgpwire.DecryptRequest{
		UserID: user, MessageID: newMessageID(), Purpose: pgpwire.DecryptForBody, Ciphertext: ct,
	})
	if code, _ := pgpwire.CodeOf(err); code != pgpwire.CodeInternal {
		t.Errorf("with every slot taken: err = %v, want the call to wait and give up", err)
	}
	// Free one slot: the same call now succeeds (the control).
	<-h.svc.decryptSlots
	if _, err := h.svc.Decrypt(context.Background(), pgpwire.DecryptRequest{
		UserID: user, MessageID: newMessageID(), Purpose: pgpwire.DecryptForBody, Ciphertext: ct,
	}); err != nil {
		t.Errorf("CONTROL: with a slot free the decrypt failed: %v", err)
	}
}

// goCryptoErrorText returns the first go-crypto error phrase found in b, or "".
//
// "MDC" is matched as an upper-case WORD, the way the library's errors spell
// it ("openpgp: MDC hash mismatch"), and the phrases case-insensitively. The
// old check lower-cased the whole body and looked for "mdc" anywhere, so it
// went red at random: the daemon log carries message ULIDs (Crockford
// base32, upper case, M, D and C all in its alphabet) and base64, either of
// which spells those three letters by chance (CI run on PR #930, 2026-10-28).
func goCryptoErrorText(b []byte) string {
	lower := strings.ToLower(string(b))
	for _, phrase := range []string{"openpgp:", "integrity protected", "key incorrect", "structural error"} {
		if strings.Contains(lower, phrase) {
			return phrase
		}
	}
	if mdcWord.Match(b) {
		return "MDC"
	}
	return ""
}

var mdcWord = regexp.MustCompile(`\bMDC\b`)

// TestGoCryptoErrorTextIgnoresChanceLettersButCatchesTheLibrary pins both
// sides of goCryptoErrorText: real library wording is caught, and the three
// letters inside an identifier or base64 are not.
func TestGoCryptoErrorTextIgnoresChanceLettersButCatchesTheLibrary(t *testing.T) {
	for _, leak := range []string{
		`{"err":"openpgp: MDC hash mismatch"}`,
		`decrypt failed: MDC packet missing`,
		`message is not integrity protected`,
		`openpgp: incorrect key`,
	} {
		if goCryptoErrorText([]byte(leak)) == "" {
			t.Errorf("missed library text in %q", leak)
		}
	}
	for _, chance := range []string{
		`message_id=01M5ZMDCX8Q2RAB0F7Y3N4KJ9P`,
		`"k":"q8mdcZ0aX1"`,
		`user=01KXMDC0000000000000000000`,
	} {
		if got := goCryptoErrorText([]byte(chance)); got != "" {
			t.Errorf("flagged %q in %q, which is chance letters in an identifier", got, chance)
		}
	}
}
