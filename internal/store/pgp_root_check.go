// SPDX-License-Identifier: AGPL-3.0-only

package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// PGPRootCheck is the one row of app.pgp_root_check (migration 00055, #525): a
// one-way value derived from the crypto process's PGP root, recorded the first
// time that process boots against this database.
//
// It is not a secret and not key material. The question it answers is narrow —
// is the root the crypto process was just given the same root this database
// was built with? internal/pgpkeyd owns the derivation and the verdict; this
// package only stores the bytes.
type PGPRootCheck struct {
	// Info is the derivation's HKDF label. It is stored so that a future change
	// of scheme reads as "a different scheme", never as "a different root".
	Info       string
	Value      []byte
	RecordedAt time.Time
}

// EnsurePGPRootCheck returns the recorded check, recording candidate first if
// there is none. inserted reports which of the two happened.
//
// ⚠ IT NEVER REPLACES A RECORDED VALUE, and could not if it tried: mailroom_app
// holds SELECT and INSERT on the table and nothing else. A recorded value that
// disagrees with candidate is handed back unchanged for the caller to refuse.
// Deciding what a mismatch means is the crypto process's job, and overwriting
// here would be the one answer that is always wrong.
//
// SystemTx only, and not by convention: the table's policies admit its row
// solely where app.current_user_id() IS NULL. Under a user's identity the row
// is invisible and the INSERT is refused — which is why UserTx has no such
// method.
func (tx *SystemTx) EnsurePGPRootCheck(ctx context.Context, candidate PGPRootCheck) (PGPRootCheck, bool, error) {
	rec, found, err := tx.readPGPRootCheck(ctx)
	if err != nil || found {
		return rec, false, err
	}

	err = tx.tx.QueryRow(ctx, `
		INSERT INTO app.pgp_root_check (info, check_value)
		VALUES ($1, $2)
		ON CONFLICT (singleton) DO NOTHING
		RETURNING info, check_value, recorded_at`,
		candidate.Info, candidate.Value,
	).Scan(&rec.Info, &rec.Value, &rec.RecordedAt)
	if err == nil {
		return rec, true, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return PGPRootCheck{}, false, fmt.Errorf("store: record pgp root check: %w", err)
	}

	// DO NOTHING fired: another boot recorded a value between the read above and
	// the insert. That value is the recorded one, whatever candidate was.
	rec, found, err = tx.readPGPRootCheck(ctx)
	if err != nil {
		return PGPRootCheck{}, false, err
	}
	if !found {
		return PGPRootCheck{}, false, errors.New(
			"store: pgp root check: the insert conflicted with a row this transaction cannot read")
	}
	return rec, false, nil
}

func (tx *SystemTx) readPGPRootCheck(ctx context.Context) (PGPRootCheck, bool, error) {
	var rec PGPRootCheck
	err := tx.tx.QueryRow(ctx,
		`SELECT info, check_value, recorded_at FROM app.pgp_root_check`,
	).Scan(&rec.Info, &rec.Value, &rec.RecordedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return PGPRootCheck{}, false, nil
	}
	if err != nil {
		return PGPRootCheck{}, false, fmt.Errorf("store: read pgp root check: %w", err)
	}
	return rec, true, nil
}
