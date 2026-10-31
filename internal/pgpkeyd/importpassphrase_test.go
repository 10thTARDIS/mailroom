// SPDX-License-Identifier: AGPL-3.0-only

package pgpkeyd

// The restore half of pgp-export-wrapped (#626): Import accepts the
// passphrase-protected file the export writes, given its passphrase.

import (
	"context"
	"strings"
	"testing"

	"mailroom/internal/pgpwire"
)

func exportWrappedForTest(t *testing.T, h *harness, user, keyID, pass string) string {
	t.Helper()
	var resp pgpwire.ExportWrappedResponse
	if err := h.client.Call(context.Background(), pgpwire.MethodExportWrapped, pgpwire.ExportWrappedRequest{
		UserID: user, KeyID: keyID, Passphrase: pass,
	}, &resp); err != nil {
		t.Fatalf("ExportWrapped: %v", err)
	}
	return resp.PrivateKey
}

// Generate, export under a passphrase, import under the same passphrase for a
// different user: same fingerprint, and the imported key signs.
func TestImportPassphraseRoundTripsAnExportedKey(t *testing.T) {
	h := startDaemon(t)
	ctx := context.Background()
	origin := newUserID()
	gen, err := h.client.Generate(ctx, pgpwire.GenerateRequest{UserID: origin, Email: "rt@example.com"})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	wrapped := exportWrappedForTest(t, h, origin, gen.Key.KeyID, goodPassphrase)

	restorer := newUserID()
	got, err := h.client.Import(ctx, pgpwire.ImportRequest{UserID: restorer, PrivateKey: wrapped, Passphrase: goodPassphrase})
	if err != nil {
		t.Fatalf("Import of a wrapped key with its passphrase: %v", err)
	}
	if got.Key.Fingerprint != gen.Key.Fingerprint {
		t.Errorf("fingerprint %q, want %q", got.Key.Fingerprint, gen.Key.Fingerprint)
	}

	// The stored form must be unprotected-and-resealed, not the wrapped file.
	if ent := h.entityOf(t, got.Key.KeyID); ent.PrivateKey.Encrypted {
		t.Error("the stored key is still passphrase-encrypted")
	}

	sig, err := h.client.Sign(ctx, pgpwire.SignRequest{UserID: restorer, KeyID: got.Key.KeyID, Data: []byte("restored\r\n")})
	if err != nil {
		t.Fatalf("the restored key cannot sign: %v", err)
	}
	if sig.Fingerprint != gen.Key.Fingerprint || !strings.Contains(sig.Signature, "BEGIN PGP SIGNATURE") {
		t.Errorf("unexpected signature response: %+v", sig)
	}
}

// Without the passphrase the key is refused with a message naming the flag; a
// wrong passphrase gives one opaque error that echoes nothing.
func TestImportPassphraseRefusalsAreOpaque(t *testing.T) {
	h, logged := startDaemonLogging(t)
	ctx := context.Background()
	origin := newUserID()
	gen, err := h.client.Generate(ctx, pgpwire.GenerateRequest{UserID: origin, Email: "rt@example.com"})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	wrapped := exportWrappedForTest(t, h, origin, gen.Key.KeyID, goodPassphrase)

	_, err = h.client.Import(ctx, pgpwire.ImportRequest{UserID: newUserID(), PrivateKey: wrapped})
	if err == nil || !strings.Contains(err.Error(), "--passphrase-file") {
		t.Errorf("no passphrase: err = %v, want a refusal naming --passphrase-file", err)
	}

	const wrong = "this is the wrong passphrase"
	_, err = h.client.Import(ctx, pgpwire.ImportRequest{UserID: newUserID(), PrivateKey: wrapped, Passphrase: wrong})
	if err == nil {
		t.Fatal("a wrong passphrase was accepted")
	}
	if !strings.Contains(err.Error(), "could not unlock the key") {
		t.Errorf("wrong passphrase: err = %v, want the opaque message", err)
	}
	if strings.Contains(err.Error(), wrong) || strings.Contains(err.Error(), goodPassphrase) {
		t.Errorf("an error echoed a passphrase: %v", err)
	}
	// Neither passphrase may reach the daemon's log, and the log must be non-empty
	// so this is not vacuous.
	if logged.Len() == 0 {
		t.Fatal("the daemon logged nothing; the log assertion would be vacuous")
	}
	for _, p := range []string{wrong, goodPassphrase} {
		if strings.Contains(logged.String(), p) {
			t.Errorf("a passphrase reached the log: %q", p)
		}
	}
}
