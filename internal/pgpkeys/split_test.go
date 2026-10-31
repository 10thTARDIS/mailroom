// SPDX-License-Identifier: AGPL-3.0-only

package pgpkeys

import (
	"bytes"
	"errors"
	"testing"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/ProtonMail/go-crypto/openpgp/packet"
)

func TestSplitKeys(t *testing.T) {
	gen := func(email string) *openpgp.Entity {
		e, err := openpgp.NewEntity("K", "", email, &packet.Config{Algorithm: packet.PubKeyAlgoEdDSA, Curve: packet.Curve25519})
		if err != nil {
			t.Fatal(err)
		}
		return e
	}
	pub := func(e *openpgp.Entity) []byte {
		var b bytes.Buffer
		if err := e.Serialize(&b); err != nil {
			t.Fatal(err)
		}
		return b.Bytes()
	}
	a, b := gen("a@example.com"), gen("b@example.com")
	blob := append(pub(a), pub(b)...)
	parts, err := SplitKeys(blob)
	if err != nil || len(parts) != 2 {
		t.Fatalf("SplitKeys = %d parts, %v; want 2", len(parts), err)
	}
	for i, want := range []*openpgp.Entity{a, b} {
		k, err := Parse(parts[i])
		if err != nil {
			t.Fatal(err)
		}
		pa, _ := Parse(pub(want))
		if k.Fingerprint != pa.Fingerprint {
			t.Errorf("part %d is %s, want %s", i, k.Fingerprint, pa.Fingerprint)
		}
		// Each part merges with the original: it is the same key.
		if _, _, err := Merge(pub(want), parts[i]); err != nil {
			t.Errorf("part %d does not merge with its key: %v", i, err)
		}
	}
	var sec bytes.Buffer
	if err := a.SerializePrivate(&sec, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := SplitKeys(append(pub(b), sec.Bytes()...)); !errors.Is(err, ErrHasPrivateKey) {
		t.Errorf("a blob with a secret key: %v, want ErrHasPrivateKey", err)
	}
	if _, err := SplitKeys([]byte("not a key")); !errors.Is(err, ErrMalformed) {
		t.Errorf("garbage: %v, want ErrMalformed", err)
	}
}
