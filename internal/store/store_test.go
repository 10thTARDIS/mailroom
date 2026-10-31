// SPDX-License-Identifier: AGPL-3.0-only

package store

import (
	"context"
	"errors"
	"regexp"
	"testing"

	"github.com/oklog/ulid/v2"
)

// UserTx must fail closed on a zero identity BEFORE touching the database —
// verified here with a nil pool: reaching the pool would panic.
func TestUserTxRejectsZeroIdentity(t *testing.T) {
	s := NewFromPool(nil)
	err := s.UserTx(context.Background(), ulid.ULID{}, func(tx *UserTx) error {
		t.Fatal("callback must not run without an identity")
		return nil
	})
	if !errors.Is(err, ErrNoIdentity) {
		t.Fatalf("got %v, want ErrNoIdentity", err)
	}
}

// NewULID output must satisfy the app.ulid domain CHECK (26-char Crockford
// base32, no I/L/O/U) and be unique.
func TestNewULIDMatchesSchemaDomain(t *testing.T) {
	domain := regexp.MustCompile(`^[0-9A-HJKMNP-TV-Z]{26}$`) // db/schema.sql app.ulid
	seen := make(map[string]bool)
	for i := 0; i < 1000; i++ {
		id := NewULID().String()
		if !domain.MatchString(id) {
			t.Fatalf("ULID %q violates the app.ulid domain pattern", id)
		}
		if seen[id] {
			t.Fatalf("duplicate ULID generated: %q", id)
		}
		seen[id] = true
	}
}
