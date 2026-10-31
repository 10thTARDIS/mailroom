// SPDX-License-Identifier: AGPL-3.0-only

package pgpkeyd

// The end-to-end representation test: a key this package actually PRODUCED,
// served through the handler a correspondent's client actually FETCHES.
//
// # Why this test exists, and what its predecessors could not see
//
// Storage held ASCII armor while internal/keypub requires binary. Every
// published key 500'd, and — because unpublished addresses 404 while published
// ones failed — the WKD endpoint became a perfect address oracle, destroying
// the one property that package is built around.
//
// Nothing caught it, and the reason is FIXTURE REALISM. Every existing test on
// both sides of the boundary used a hand-made stand-in: []byte{0x99, 0x01,
// 0x02}, "\x99\x01\x02binary-public-key", "binary". Those bytes agree with
// whatever the test author believed the format was, which is the one thing
// under test. No producer in this codebase emits them, so no fixture built from
// them could disagree with the consumer — the two sides were each internally
// consistent and never met.
//
// So this test takes NO fixture. It generates a real key through the real
// Service, hands the stored bytes to the real HUHandler, and asks the questions
// a mail client asks: 200, application/octet-stream, no armor, and packets that
// parse back to the same fingerprint. It fails if the storage representation is
// reverted to armor — that is its contract, and it has been probed.

import (
	"bytes"
	"context"
	"encoding/hex"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ProtonMail/go-crypto/openpgp"

	"mailroom/internal/crypto"
	"mailroom/internal/keypub"
	"mailroom/internal/pgpwire"
	"mailroom/internal/wkd"
)

// realKeyLookup is the ONLY thing between the crypto process and the WKD
// handler in this test, and it is deliberately a pass-through: it returns
// Record.PublicKey unchanged, because the production path does the same
// (store.LookupWKDKey reads the column verbatim). Anything that re-encoded here
// would be this test supplying the correctness it is meant to be checking.
type realKeyLookup struct {
	key         []byte
	fingerprint string
	hash        string
}

// LookupWKDKeyPage is required by keypub.New. This suite is about the BYTES on
// the WKD path, not the page, so it answers nothing — but the constructor
// refuses a lookup that cannot serve pages, because that mis-wire would
// otherwise surface as a 404 indistinguishable from an unpublished key.
func (l realKeyLookup) LookupWKDKeyPage(_ context.Context, _, _ string) (string, string, bool, error) {
	return "", "", false, nil
}

func (l realKeyLookup) LookupWKDKey(_ context.Context, _, hash string) ([]byte, string, bool, error) {
	if hash != l.hash {
		return nil, "", false, nil
	}
	return l.key, l.fingerprint, true, nil
}

// TestGeneratedKeyIsServableOverWKD drives a REAL generated key through the
// real WKD handler.
func TestGeneratedKeyIsServableOverWKD(t *testing.T) {
	svc := newTestService(t)
	userID := newUserID()
	const address = "alice@example.com"

	gen, err := svc.Generate(context.Background(), pgpwire.GenerateRequest{
		UserID: userID,
		Name:   "Alice Example",
		Email:  address,
	})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}

	// Straight out of the vault: these are the bytes the store column would
	// hold, not a re-derivation of them.
	rec, err := svc.vaultRecord(t, gen.Key.KeyID)
	if err != nil {
		t.Fatalf("vault.Get: %v", err)
	}
	if len(rec.PublicKey) == 0 {
		t.Fatal("Generate stored no public key at all")
	}

	localPart, _, err := wkd.SplitAddress(address)
	if err != nil {
		t.Fatalf("SplitAddress: %v", err)
	}
	hash := wkd.HashLocalPart(localPart)

	svcPub, err := keypub.New(
		realKeyLookup{key: rec.PublicKey, fingerprint: rec.Fingerprint, hash: hash},
		"example.com",
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)
	if err != nil {
		t.Fatalf("keypub.New: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, wkd.DirectPath(localPart), nil)
	rr := httptest.NewRecorder()
	svcPub.HUHandler().ServeHTTP(rr, req)

	// 200 IS THE ASSERTION THAT WOULD HAVE GONE RED. Armored storage makes this
	// a 500 — keypub refuses to serve armor rather than ship something that
	// looks like it worked.
	if rr.Code != http.StatusOK {
		t.Fatalf("WKD lookup for a freshly generated key returned %d, want 200 — "+
			"a key this instance produced is not servable to the clients it was published for; "+
			"body: %s", rr.Code, strings.TrimSpace(rr.Body.String()))
	}
	if ct := rr.Header().Get("Content-Type"); ct != "application/octet-stream" {
		t.Errorf("Content-Type = %q, want application/octet-stream (required by the draft)", ct)
	}

	body := rr.Body.Bytes()
	if bytes.Contains(body, []byte("-----BEGIN PGP")) {
		t.Fatal("the WKD response is ASCII-armored; every real client will decline it while " +
			"curl reports success")
	}

	// THE POINT OF USING A REAL KEY: the bytes must be a key, not merely
	// un-armored. A fixture of arbitrary non-armor bytes passes every check
	// above and fails this one.
	entities, err := openpgp.ReadKeyRing(bytes.NewReader(body))
	if err != nil {
		t.Fatalf("the served bytes are not a readable OpenPGP key ring: %v", err)
	}
	if len(entities) != 1 {
		t.Fatalf("served %d entities, want exactly 1", len(entities))
	}
	served := entities[0]
	if got := strings.ToUpper(hex.EncodeToString(served.PrimaryKey.Fingerprint)); got != gen.Key.Fingerprint {
		t.Errorf("served key fingerprint %s != generated key fingerprint %s — WKD would hand "+
			"correspondents a different key from the one this user holds", got, gen.Key.Fingerprint)
	}
	// A public key and nothing else. The WKD endpoint reads a column the
	// anonymous role cannot even select private material from, but the
	// serializer is on THIS side of that boundary and this is where a wrong
	// call (SerializePrivate for Serialize) would show up.
	if served.PrivateKey != nil {
		t.Error("the WKD response contains a PRIVATE key packet")
	}

	// And the wire form stays ARMORED, because a person pastes it. The two
	// representations coexist on purpose; asserting only one of them is how
	// they came to disagree.
	if !strings.Contains(gen.Key.PublicKey, "-----BEGIN PGP PUBLIC KEY BLOCK-----") {
		t.Errorf("pgpwire.KeyInfo.PublicKey is not armored: %.40q", gen.Key.PublicKey)
	}
	wireEntities, err := openpgp.ReadArmoredKeyRing(strings.NewReader(gen.Key.PublicKey))
	if err != nil {
		t.Fatalf("the armored wire key does not parse: %v", err)
	}
	if got := strings.ToUpper(hex.EncodeToString(wireEntities[0].PrimaryKey.Fingerprint)); got != gen.Key.Fingerprint {
		t.Errorf("armored wire key fingerprint %s != %s — the wire and the column describe "+
			"different key material", got, gen.Key.Fingerprint)
	}
}

// TestImportedKeyIsServableOverWKD covers the second producer. Import has its
// own serialization call site, so "Generate stores binary" says nothing about
// it — and a key taken into custody from outside is exactly the one an operator
// is most likely to publish.
func TestImportedKeyIsServableOverWKD(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()

	// A real private key, made the same way a user's own would be, then handed
	// in through the armored import path.
	seedUser := newUserID()
	seed, err := svc.Generate(ctx, pgpwire.GenerateRequest{UserID: seedUser, Email: "bob@example.com"})
	if err != nil {
		t.Fatalf("seed Generate: %v", err)
	}
	privArmor := armoredPrivateOf(t, svc, seed.Key.KeyID)

	imp, err := svc.Import(ctx, pgpwire.ImportRequest{UserID: newUserID(), PrivateKey: privArmor})
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	rec, err := svc.vaultRecord(t, imp.Key.KeyID)
	if err != nil {
		t.Fatalf("vault.Get: %v", err)
	}

	if bytes.Contains(rec.PublicKey, []byte("-----BEGIN PGP")) {
		t.Fatal("Import stored ASCII armor; a key imported and then published would 500 at the " +
			"WKD endpoint, turning it into an address oracle")
	}
	entities, err := openpgp.ReadKeyRing(bytes.NewReader(rec.PublicKey))
	if err != nil {
		t.Fatalf("Import stored bytes that are not a readable OpenPGP key: %v", err)
	}
	if got := strings.ToUpper(hex.EncodeToString(entities[0].PrimaryKey.Fingerprint)); got != imp.Key.Fingerprint {
		t.Errorf("stored key fingerprint %s != reported %s", got, imp.Key.Fingerprint)
	}
}

// --- helpers ---------------------------------------------------------------

// newTestService builds a Service over the in-memory vault. No socket and no
// database: the question here is what the SERVICE stores, and the transport is
// covered elsewhere.
func newTestService(t *testing.T) *Service {
	t.Helper()
	root := make([]byte, 32)
	for i := range root {
		root[i] = byte(i*11 + 3)
	}
	svc, err := NewService(root, NewMemoryVault(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	return svc
}

// vaultRecord reads the stored record directly, so the assertions above are
// about what was WRITTEN rather than about what the API chooses to project.
func (s *Service) vaultRecord(t *testing.T, keyID string) (Record, error) {
	t.Helper()
	return s.vault.Get(context.Background(), keyID)
}

// armoredPrivateOf recovers a stored private key as armored text, so the import
// path can be fed a GENUINE key rather than a literal pasted into a test file —
// the same reasoning as the rest of this file.
func armoredPrivateOf(t *testing.T, s *Service, keyID string) string {
	t.Helper()
	rec, err := s.vault.Get(context.Background(), keyID)
	if err != nil {
		t.Fatalf("vault.Get: %v", err)
	}
	wrap, err := crypto.DeriveKey(s.root, []byte(rec.UserID), rootInfo)
	if err != nil {
		t.Fatalf("DeriveKey: %v", err)
	}
	priv, err := crypto.Open(wrap, rec.Sealed, sealAAD(rec.UserID, rec.KeyID))
	if err != nil {
		t.Fatalf("open sealed private key: %v", err)
	}
	return armorPrivateForTest(t, priv)
}
