// SPDX-License-Identifier: AGPL-3.0-only

package pgpkeyd

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"time"

	"mailroom/internal/crypto"
	"mailroom/internal/store"
)

// rootCheckInfo is the HKDF label for the boot-time root check (#525).
//
// Versioned for the same reason rootInfo is, and DIFFERENT from rootInfo on
// purpose: the check value is stored in the clear, so it has to be an output
// that no other derivation from this root ever produces.
const rootCheckInfo = "mailroom-pgp-root-check-v1"

// ErrRootMismatch means the process was given a PGP root that is not the one
// this database recorded. It must not serve.
var ErrRootMismatch = errors.New("pgpkeyd: PGP root mismatch")

// RootCheckValue derives the check value for a root:
//
//	pgp root ──HKDF(salt=none, info="mailroom-pgp-root-check-v1")──▶ check value
//
// One-way, and under a label nothing else uses, so storing it discloses nothing
// about the root or about any wrapping key (salt=userID, info="mailroom-pgp-v1").
//
// Exported for the reason WrappingKey is: a second consumer that holds the root
// directly — the restore drill — must compute the same bytes, not keep a copy of
// the derivation that can drift.
func RootCheckValue(root []byte) ([]byte, error) {
	return crypto.DeriveKey(root, nil, rootCheckInfo)
}

// RootCheckStore is where the check value lives. It is an interface for one
// reason: the comparison is the part worth unit-testing, and the unit suite has
// no database. StoreRootCheck is the real one.
type RootCheckStore interface {
	// EnsureRootCheck returns the recorded check, recording candidate first if
	// there is none. It must never replace a recorded value.
	EnsureRootCheck(ctx context.Context, candidate store.PGPRootCheck) (recorded store.PGPRootCheck, inserted bool, err error)
}

// RootCheckOutcome names which of the two healthy states a boot reached. They
// look identical from outside the process and do not mean the same thing, so
// the boot log says which.
type RootCheckOutcome string

const (
	// RootRecorded: the database had no recorded root, and now this one is.
	// NOTHING WAS VERIFIED. On a fresh install that is correct; on a database
	// that predates migration 00055 it is the one-time trust-on-first-use step
	// the migration describes.
	RootRecorded RootCheckOutcome = "recorded"
	// RootVerified: the recorded value matched. This is the root the database
	// was built with.
	RootVerified RootCheckOutcome = "verified"
)

// VerifyRoot compares this process's root with the one the database recorded,
// recording it if there is none, and returns an error wrapping ErrRootMismatch
// if they differ (#525).
//
// ⚠ ON ERROR THE CALLER MUST NOT SERVE — not log and carry on. Carrying on was
// the defect: the root was read at boot and nothing checked it until somebody
// signed or decrypted, so a wrong root reported healthy for as long as nobody
// did. cmd/mailroom-crypto returns before it listens, so the healthcheck, which
// only dials the socket, fails.
//
// ⚠ WHAT A MATCH PROVES: that this is the root the database recorded. Not that
// it opens the stored keys. For a restore those coincide, and the restore
// drill's step 5 is what proves the second.
func (s *Service) VerifyRoot(ctx context.Context, st RootCheckStore) (RootCheckOutcome, error) {
	if st == nil {
		return "", errors.New("pgpkeyd: nil root check store")
	}
	value, err := RootCheckValue(s.root)
	if err != nil {
		return "", fmt.Errorf("pgpkeyd: derive root check: %w", err)
	}

	recorded, inserted, err := st.EnsureRootCheck(ctx, store.PGPRootCheck{Info: rootCheckInfo, Value: value})
	if err != nil {
		return "", fmt.Errorf("pgpkeyd: read or record the root check: %w", err)
	}
	at := recorded.RecordedAt.UTC().Format(time.RFC3339)

	if inserted {
		s.log.Info("pgp root recorded for this database",
			"outcome", string(RootRecorded), "recorded_at", at,
			"meaning", "no root had been recorded, so this one is now required at every boot; "+
				"it was NOT verified against keys stored before now — the restore drill's step 5 does that")
		return RootRecorded, nil
	}

	// A different scheme is not a different root and must not be reported as
	// one — the operator's remedy for each is different. It is refused all the
	// same, because a root that cannot be verified has not been.
	if recorded.Info != rootCheckInfo {
		s.log.Error("pgp root check was recorded under a scheme this binary does not compute; refusing to start",
			"outcome", "unverifiable", "recorded_scheme", recorded.Info, "this_scheme", rootCheckInfo,
			"recorded_at", at)
		return "", fmt.Errorf("pgpkeyd: the recorded PGP root check uses scheme %q and this binary computes %q, "+
			"so this root cannot be verified; refusing to start", recorded.Info, rootCheckInfo)
	}

	if subtle.ConstantTimeCompare(recorded.Value, value) != 1 {
		s.log.Error("PGP ROOT MISMATCH: refusing to start", "outcome", "mismatch", "recorded_at", at)
		return "", fmt.Errorf("%w: this is not the PGP root this database recorded on %s. With it every stored "+
			"PGP private key is unopenable, and any key created now would be sealed under a root your escrow "+
			"does not hold. Install the escrowed PGP root (docs/backup-restore-runbook.md, step 5)",
			ErrRootMismatch, at)
	}

	s.log.Info("pgp root verified", "outcome", string(RootVerified), "recorded_at", at)
	return RootVerified, nil
}

// StoreRootCheck is the real RootCheckStore: app.pgp_root_check, read and
// written through a SystemTx — the only context its policies admit.
type StoreRootCheck struct {
	store *store.Store
}

// NewStoreRootCheck wraps a Store.
func NewStoreRootCheck(s *store.Store) (*StoreRootCheck, error) {
	if s == nil {
		return nil, errors.New("pgpkeyd: nil store")
	}
	return &StoreRootCheck{store: s}, nil
}

// EnsureRootCheck implements RootCheckStore.
func (r *StoreRootCheck) EnsureRootCheck(ctx context.Context, candidate store.PGPRootCheck) (store.PGPRootCheck, bool, error) {
	var (
		rec      store.PGPRootCheck
		inserted bool
	)
	err := r.store.SystemTx(ctx, func(tx *store.SystemTx) error {
		var err error
		rec, inserted, err = tx.EnsurePGPRootCheck(ctx, candidate)
		return err
	})
	return rec, inserted, err
}
