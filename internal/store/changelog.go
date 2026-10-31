// SPDX-License-Identifier: AGPL-3.0-only

package store

import (
	"context"
	"fmt"

	"github.com/oklog/ulid/v2"
)

// ObjectType mirrors the app.object_type enum: the JMAP-visible types whose
// mutations are sequenced through the change log (spec §13/§14).
type ObjectType string

const (
	ObjectEmail           ObjectType = "Email"
	ObjectMailbox         ObjectType = "Mailbox"
	ObjectThread          ObjectType = "Thread"
	ObjectIdentity        ObjectType = "Identity"
	ObjectEmailSubmission ObjectType = "EmailSubmission"
	ObjectContact         ObjectType = "Contact"
	ObjectLabel           ObjectType = "Label"
	ObjectRule            ObjectType = "Rule"
	ObjectKey             ObjectType = "Key"
	// ObjectAccount is the user's mail account (app.accounts). It became a
	// JMAP-visible object with in-UI account management (mailroom#98/#45): the
	// urn:mailroom:accounts extension's Account/set appends its change_log rows.
	// The app.object_type enum carries 'Account' as of migration 00009.
	ObjectAccount ObjectType = "Account"
	// ObjectRemoteAllow is a remembered "show remote content" decision (#202,
	// app.remote_allows). JMAP-visible so a grant made on one device reaches the
	// others through /changes — a per-sender allow that did not sync would be a
	// setting the user has to make again on every browser. The app.object_type enum
	// carries 'RemoteAllow' as of migration 00021.
	ObjectRemoteAllow ObjectType = "RemoteAllow"
	// ObjectComposeSnippet is a saved signature or template (#56,
	// app.compose_snippets). JMAP-visible so a signature written on one device is
	// available on the others — compose text that lived in one browser would be
	// retyped in the next. The app.object_type enum carries 'ComposeSnippet' as of
	// migration 00022.
	ObjectComposeSnippet ObjectType = "ComposeSnippet"
	// ObjectSavedSearch is a named query (#57, app.saved_searches), shown in the
	// sidebar when pinned — the matrix's "smart folder". JMAP-visible for the same
	// reason as a signature: a folder you arranged on the laptop that the phone knows
	// nothing about is a folder you stop trusting. The app.object_type enum carries
	// 'SavedSearch' as of migration 00024.
	ObjectSavedSearch ObjectType = "SavedSearch"
	// ObjectCategoryTab is one Gmail-style category tab's per-user state — whether
	// it is enabled and where it sits in the strip (#219, app.category_tabs).
	// JMAP-visible for the same reason as the two above: a tab switched off on the
	// laptop that the phone still shows is a setting the user makes twice. The
	// app.object_type enum carries 'CategoryTab' as of migration 00028.
	ObjectCategoryTab ObjectType = "CategoryTab"
	// ObjectArea is one of the desk's areas — the partition every message
	// resolves into, fed by tabs and/or labels and owning a label of its own
	// (#442, app.areas). JMAP-visible so an area created or rearranged on one
	// device reaches the others; a feed change appends the change_log row of the
	// AREA IT BELONGS TO, never a row of its own (area_feeds carries no object
	// type). The app.object_type enum carries 'Area' as of migration 00046.
	ObjectArea ObjectType = "Area"
	// ObjectDeskPreferences is the desk's per-user settings singleton — the
	// Surface/Drawer score threshold and the drawer/tray dwell periods (#442,
	// app.desk_preferences). Its change_log object_id is the USER's id, not a
	// manufactured row id, because there is exactly one desk per user in 0.5.
	// The app.object_type enum carries 'DeskPreferences' as of migration 00046.
	ObjectDeskPreferences ObjectType = "DeskPreferences"
	// ObjectClientPreferences is one client family's per-user display
	// preferences — theme, density, pane split (#442, app.client_preferences).
	// Not a singleton like DeskPreferences: a phone and a browser want different
	// defaults, so each (user, family) pair gets its own row and its own change
	// rows. The app.object_type enum carries 'ClientPreferences' as of migration
	// 00046.
	ObjectClientPreferences ObjectType = "ClientPreferences"
	// ObjectSetupRun is one day-one setup run — the chunked bulk import that
	// labels and archives a folder-sorted mailbox onto the desk (#446,
	// app.setup_runs). JMAP-visible because the CLIENT POLLS IT: the wizard
	// starts the run with Setup/apply and then watches status and counters move
	// while a background job drains the chunks, and a second device opened
	// mid-run must see the same thing. The app.object_type enum carries
	// 'SetupRun' as of migration 00050. The run's JOURNAL
	// (app.setup_journal) is deliberately NOT an object type: it is the run's
	// internal per-message record, and what a client syncs is the run plus the
	// Emails the run touched.
	ObjectSetupRun ObjectType = "SetupRun"
)

// ChangeOp mirrors the app.change_op enum.
type ChangeOp string

const (
	OpCreated   ChangeOp = "created"
	OpUpdated   ChangeOp = "updated"
	OpDestroyed ChangeOp = "destroyed"
)

// ⚠ THE TWO STATEMENTS THE INVARIANT IS MADE OF, DEFINED ONCE (#299).
//
// dsn.go needs to append change rows through a SAVEPOINT executor rather than
// through *UserTx (its writes are isolated from the surrounding ingest
// transaction), so it cannot call AppendChange — and it therefore held a second,
// hand-copied version of both statements, byte-equivalent today with nothing
// tying it to these. The store has already paid for that shape once, with the ml
// payload struct that got a pinning test precisely because a second definition
// had drifted.
//
// A shared const is better than a test that watches for drift: the two callers
// cannot disagree, because there is only one text. Anything that needs a third
// caller (a future savepoint-scoped writer) gets these too.
const (
	// nextChangeSeqSQL allocates the next per-(user, type) sequence.
	// $1 = user id, $2 = object type. Returns the allocated sequence.
	nextChangeSeqSQL = `
		INSERT INTO app.change_seq (user_id, object_type, last_seq)
		VALUES ($1, $2, 1)
		ON CONFLICT (user_id, object_type)
		DO UPDATE SET last_seq = change_seq.last_seq + 1
		RETURNING last_seq`

	// insertChangeLogSQL appends the row itself.
	// $1 = user id, $2 = object type, $3 = seq, $4 = object id, $5 = op.
	insertChangeLogSQL = `
		INSERT INTO app.change_log (user_id, object_type, seq, object_id, op)
		VALUES ($1, $2, $3, $4, $5)`
)

// NextChangeSeq allocates the next per-(user, type) sequence (data-model §3).
// The row lock taken by the UPDATE serializes writers per (user, type) — the
// known per-user hot row, a non-issue at the target scale. The upsert form
// lazily creates the counter row on a type's first-ever mutation with the
// same locking semantics as the data model's plain UPDATE … RETURNING.
func (tx *UserTx) NextChangeSeq(ctx context.Context, t ObjectType) (int64, error) {
	var seq int64
	err := tx.tx.QueryRow(ctx, nextChangeSeqSQL, tx.userID.String(), t).Scan(&seq)
	if err != nil {
		return 0, fmt.Errorf("store: allocate change seq (%s): %w", t, err)
	}
	return seq, nil
}

// AppendChange allocates a sequence and appends the change_log row that
// every mutation of a JMAP-visible object MUST write in the same
// transaction as the mutation (the load-bearing invariant — CLAUDE.md,
// ADR-003). It returns the allocated sequence, which is the new JMAP state
// for the type.
func (tx *UserTx) AppendChange(ctx context.Context, t ObjectType, objectID ulid.ULID, op ChangeOp) (int64, error) {
	seq, err := tx.NextChangeSeq(ctx, t)
	if err != nil {
		return 0, err
	}
	if _, err := tx.tx.Exec(ctx, insertChangeLogSQL,
		tx.userID.String(), t, seq, objectID.String(), op,
	); err != nil {
		return 0, fmt.Errorf("store: append change log (%s %s %s): %w", t, objectID, op, err)
	}
	return seq, nil
}

// ChangeState reads the current state (last allocated sequence) for a type;
// 0 means the type has never been mutated. Read inside the same transaction
// as a data read, state and data are mutually consistent (data-model §3.1).
func (tx *UserTx) ChangeState(ctx context.Context, t ObjectType) (int64, error) {
	var seq int64
	err := tx.tx.QueryRow(ctx, `
		SELECT COALESCE(
			(SELECT last_seq FROM app.change_seq
			  WHERE user_id = $1 AND object_type = $2), 0)`,
		tx.userID.String(), t,
	).Scan(&seq)
	if err != nil {
		return 0, fmt.Errorf("store: read change state (%s): %w", t, err)
	}
	return seq, nil
}

// AppendChanges appends one change row per object id, in ONE pair of statements
// rather than one pair per id (#417).
//
// ⚠ IT EXISTS BECAUSE THE LOOP WAS A LOCK-HOLD, NOT BECAUSE IT WAS SLOW. Every
// AppendChange takes the per-(user, type) `change_seq` row lock — the known hot
// row — and that lock is held until the transaction commits. So a loop of N
// calls does not merely cost 2N round trips: it holds a lock that SERIALISES
// EVERY OTHER Email WRITER FOR THAT USER across all of them.
//
// ⚠ THE CASE THAT FORCED IT, MEASURED RATHER THAN IMAGINED. A quarantine flag
// propagates to every copy of the same content (ADR-027 §8), and the largest
// content group on the corpus this was designed against is a few dozen rows. On a real
// mailbox a newsletter logo sits on TEN THOUSAND messages — so one `flagged`
// verdict, including a ClamAV heuristic false positive (#412), became ~20,000
// statements holding that lock, INSIDE the ingest transaction. A stranger's
// attachment could stall every Email write for the account for tens of seconds,
// and the blast radius scaled with how popular the file was.
//
// ⚠ IT IS NOT A CAP, AND MUST NOT BECOME ONE. §8 requires a flag to reach EVERY
// copy — a release or a quarantine that hit some of them is the
// writeback-hit-1-of-N-copies bug, which this codebase has already shipped once.
// The fix is to make the whole set cheap, never to do less of it.
//
// The sequences are allocated as one contiguous RANGE, so they stay strictly
// increasing per (user, type) exactly as the one-at-a-time path produces — a
// client's `/changes` cursor cannot tell the two apart.
func (tx *UserTx) AppendChanges(ctx context.Context, t ObjectType, objectIDs []ulid.ULID, op ChangeOp) error {
	if len(objectIDs) == 0 {
		return nil
	}
	n := int64(len(objectIDs))
	// One statement for the whole range. RETURNING gives the LAST sequence, so
	// the block is (last-n, last].
	var last int64
	if err := tx.tx.QueryRow(ctx, `
		INSERT INTO app.change_seq (user_id, object_type, last_seq)
		VALUES ($1, $2, $3)
		ON CONFLICT (user_id, object_type)
		DO UPDATE SET last_seq = change_seq.last_seq + $3
		RETURNING last_seq`,
		tx.userID.String(), t, n).Scan(&last); err != nil {
		return fmt.Errorf("store: allocate %d change seqs (%s): %w", n, t, err)
	}
	ids := make([]string, 0, len(objectIDs))
	for _, id := range objectIDs {
		ids = append(ids, id.String())
	}
	// And one statement for the rows: generate_series pairs each id with its
	// sequence, so the ordering of the block matches the ordering of the slice.
	if _, err := tx.tx.Exec(ctx, `
		INSERT INTO app.change_log (user_id, object_type, seq, object_id, op)
		SELECT $1, $2, $3::bigint - $4::bigint + i, id, $5
		  FROM unnest($6::text[]) WITH ORDINALITY AS t(id, i)`,
		tx.userID.String(), t, last, n, op, ids); err != nil {
		return fmt.Errorf("store: append %d change rows (%s): %w", n, t, err)
	}
	return nil
}
