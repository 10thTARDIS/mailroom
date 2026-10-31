// SPDX-License-Identifier: AGPL-3.0-only

package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/oklog/ulid/v2"
)

// This file holds the store surface for the model registry (app.ml_models) and
// the feedback-event replay source (app.feedback_events), spec §8, mailroom#35.
//
// Both are OPERATIONAL data, the same category as app.account_sync_status and
// app.outbound_ledger — NOT JMAP-visible objects. A write here appends NO
// change_log row and neither table is a store ObjectType (CLAUDE.md). The model
// registry drives rollback/reset over the blob store; feedback events feed the
// eval harness and future scorer training. Nothing here is ever synced via
// /changes, the indexer, or a client cursor.
//
// Every method runs inside a UserTx, so RLS (app.ml_models / app.feedback_events
// both carry FORCE ROW LEVEL SECURITY) scopes every statement to the caller:
// no identity, no rows (CLAUDE.md, fail closed).

// ErrModelVersionNotFound is returned when a (kind, version) does not resolve for
// the caller — RLS makes another user's row indistinguishable from a missing one.
var ErrModelVersionNotFound = errors.New("store: model version not found")

// VersionInfo is one row of a model's lineage, for ListModelVersions.
type VersionInfo struct {
	ID        ulid.ULID
	Version   int
	ParentID  *ulid.ULID // nil at a lineage root (the first snapshot, or a Reset)
	Active    bool
	CreatedAt time.Time
}

// InsertModelVersion inserts a new (inactive) model snapshot row for
// (caller, kind) and returns its assigned version. The version is
// max(version)+1 over the caller's rows of that kind, computed inside the
// INSERT so it is atomic with the write (no separate MaxModelVersion round-trip
// to race). When linkParent is true the new row's parent_id is the currently
// ACTIVE row's id (lineage), giving rollback a chain to walk; when false it is
// NULL — the Reset path deliberately breaks lineage by rooting a fresh baseline.
//
// The row is always inserted active=false: activation is a separate, explicit
// step (ActivateModelVersion) so a snapshot never silently displaces the live
// model. A concurrent snapshot of the same (user, kind) can collide on
// UNIQUE(user_id, model_kind, version); the caller surfaces that as a retryable
// error (single-user, low-volume — a genuine collision is rare).
func (tx *UserTx) InsertModelVersion(ctx context.Context, kind string, artifactHash []byte, linkParent bool) (int, error) {
	id := NewULID()
	var version int
	err := tx.tx.QueryRow(ctx, `
		INSERT INTO app.ml_models
			(id, user_id, model_kind, version, parent_id, artifact_hash, active)
		VALUES (
			$1, $2::app.ulid, $3,
			COALESCE((SELECT max(version) FROM app.ml_models
			           WHERE user_id = $2::app.ulid AND model_kind = $3), 0) + 1,
			CASE WHEN $5 THEN (SELECT id FROM app.ml_models
			                    WHERE user_id = $2::app.ulid AND model_kind = $3 AND active)
			     ELSE NULL END,
			$4, false)
		RETURNING version`,
		id.String(), tx.userID.String(), kind, artifactHash, linkParent).Scan(&version)
	if err != nil {
		return 0, fmt.Errorf("store: insert model version: %w", err)
	}
	return version, nil
}

// ActiveModelRow returns the artifact hash and version of the caller's active
// model of the given kind. found is false when no row is active (a fresh user,
// or one whose only snapshots have never been activated) — the caller falls back
// to a baseline so the system always has a scorer.
func (tx *UserTx) ActiveModelRow(ctx context.Context, kind string) (artifactHash []byte, version int, found bool, err error) {
	err = tx.tx.QueryRow(ctx, `
		SELECT artifact_hash, version
		  FROM app.ml_models
		 WHERE user_id = $1 AND model_kind = $2 AND active`,
		tx.userID.String(), kind).Scan(&artifactHash, &version)
	if isNoRows(err) {
		return nil, 0, false, nil
	}
	if err != nil {
		return nil, 0, false, fmt.Errorf("store: active model row: %w", err)
	}
	return artifactHash, version, true, nil
}

// ActivateModelVersion makes (kind, version) the single active model of that
// kind for the caller, enforcing the app.ml_models_one_active DB invariant
// (exactly one active row per (user, kind)). It runs in the CALLER's UserTx and
// does two writes in order — deactivate the current active row, then activate
// the target — so the partial unique index is never momentarily violated (both
// statements commit atomically with the surrounding transaction).
//
// The target's existence is checked FIRST: a missing version returns
// ErrModelVersionNotFound and touches nothing, so a bad rollback request can
// never leave the caller with zero active models. Rollback is exactly this call
// against an older version; there is no delete, so lineage is preserved.
func (tx *UserTx) ActivateModelVersion(ctx context.Context, kind string, version int) error {
	var targetID string
	err := tx.tx.QueryRow(ctx, `
		SELECT id FROM app.ml_models
		 WHERE user_id = $1 AND model_kind = $2 AND version = $3`,
		tx.userID.String(), kind, version).Scan(&targetID)
	if isNoRows(err) {
		return ErrModelVersionNotFound
	}
	if err != nil {
		return fmt.Errorf("store: activate model (lookup): %w", err)
	}

	if _, err := tx.tx.Exec(ctx, `
		UPDATE app.ml_models SET active = false
		 WHERE user_id = $1 AND model_kind = $2 AND active`,
		tx.userID.String(), kind); err != nil {
		return fmt.Errorf("store: activate model (deactivate current): %w", err)
	}
	if _, err := tx.tx.Exec(ctx, `
		UPDATE app.ml_models SET active = true
		 WHERE user_id = $1 AND id = $2`,
		tx.userID.String(), targetID); err != nil {
		return fmt.Errorf("store: activate model (activate target): %w", err)
	}
	return nil
}

// ListModelVersions returns the caller's lineage for a kind, oldest version
// first — the input to ListVersions / a rollback UI.
func (tx *UserTx) ListModelVersions(ctx context.Context, kind string) ([]VersionInfo, error) {
	rows, err := tx.tx.Query(ctx, `
		SELECT id, version, parent_id, active, created_at
		  FROM app.ml_models
		 WHERE user_id = $1 AND model_kind = $2
		 ORDER BY version`,
		tx.userID.String(), kind)
	if err != nil {
		return nil, fmt.Errorf("store: list model versions: %w", err)
	}
	defer rows.Close()

	var out []VersionInfo
	for rows.Next() {
		var (
			v      VersionInfo
			idStr  string
			parent *string
		)
		if err := rows.Scan(&idStr, &v.Version, &parent, &v.Active, &v.CreatedAt); err != nil {
			return nil, fmt.Errorf("store: scan model version: %w", err)
		}
		if v.ID, err = ulid.Parse(idStr); err != nil {
			return nil, fmt.Errorf("store: parse model id %q: %w", idStr, err)
		}
		if parent != nil {
			p, perr := ulid.Parse(*parent)
			if perr != nil {
				return nil, fmt.Errorf("store: parse model parent id %q: %w", *parent, perr)
			}
			v.ParentID = &p
		}
		out = append(out, v)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list model versions rows: %w", err)
	}
	return out, nil
}

// PruneModelVersions deletes the caller's OLDEST snapshots of a kind, keeping the
// newest keepN versions and ALWAYS the active one (even if it is older than the
// newest keepN — the active model is never pruned).
//
// It returns the number of rows deleted and the artifact hashes whose app.blobs
// row this call REMOVED — the ones whose file the caller must now delete, inside
// this same transaction (blobreclaim.go). Distinct snapshots have distinct
// serialized bytes and therefore distinct content hashes; the only way two rows
// share a hash is byte-identical models (e.g. two un-trained resets), and the
// guard keeps a shared blob alive until its last referent is gone.
//
// ⚠ THE REF CHECK HERE USED TO BE A FIFTH COPY, AND IT ASKED HALF THE QUESTION
// (#316). It asked only "does a surviving ml_models row still reference this
// hash?" and the caller then deleted the FILE unconditionally — so a model
// artifact whose bytes a MESSAGE or ATTACHMENT happened to share had that
// message's bytes destroyed, leaving app.blobs and app.messages intact and the
// message unopenable. That is the exact mirror of the hazard #300 fixed in the
// other direction, where a message-shaped reclaim could destroy a live model
// artifact; #300 rejected "it has never happened" as a guarantee for that side,
// so it is rejected for this one. Both directions now ask the one shared
// question in blobreclaim.go, which is the whole point of there being one.
//
// Lineage is a self-FK (parent_id -> ml_models.id, NO ACTION), so a SURVIVING
// child pointing at a pruned parent would block the delete. Before deleting we
// therefore NULL the parent pointer of any row that references a victim — the
// pruned row was that child's lineage root, and trimming history is exactly what
// Prune does, so a broken pointer becomes a clean root. The whole thing runs in
// the caller's UserTx: identify victims, sever inbound pointers, delete, then
// ref-check (separate statements so each sees the prior one's effects — a
// data-modifying CTE's writes are invisible to the rest of the same query).
func (tx *UserTx) PruneModelVersions(ctx context.Context, kind string, keepN int) (deleted int, freed [][]byte, err error) {
	if keepN < 0 {
		keepN = 0
	}
	// 1. Identify victims: non-active rows outside the newest keepN.
	rows, err := tx.tx.Query(ctx, `
		SELECT id, artifact_hash FROM app.ml_models m
		 WHERE m.user_id = $1 AND m.model_kind = $2
		   AND NOT m.active
		   AND m.id NOT IN (
		       SELECT id FROM app.ml_models
		        WHERE user_id = $1 AND model_kind = $2
		        ORDER BY version DESC
		        LIMIT $3)`,
		tx.userID.String(), kind, keepN)
	if err != nil {
		return 0, nil, fmt.Errorf("store: prune model versions (scan victims): %w", err)
	}
	var (
		victimIDs    []string
		victimHashes [][]byte
	)
	for rows.Next() {
		var (
			id string
			h  []byte
		)
		if err := rows.Scan(&id, &h); err != nil {
			rows.Close()
			return 0, nil, fmt.Errorf("store: scan pruned model row: %w", err)
		}
		victimIDs = append(victimIDs, id)
		victimHashes = append(victimHashes, h)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return 0, nil, fmt.Errorf("store: prune model versions rows: %w", err)
	}
	rows.Close()
	if len(victimIDs) == 0 {
		return 0, nil, nil
	}

	// 2. Sever inbound lineage pointers so the delete cannot trip the self-FK.
	if _, err := tx.tx.Exec(ctx, `
		UPDATE app.ml_models SET parent_id = NULL
		 WHERE user_id = $1 AND parent_id = ANY($2::text[])`,
		tx.userID.String(), victimIDs); err != nil {
		return 0, nil, fmt.Errorf("store: prune model versions (sever lineage): %w", err)
	}

	// 3. Delete the victims.
	if _, err := tx.tx.Exec(ctx, `
		DELETE FROM app.ml_models
		 WHERE user_id = $1 AND id = ANY($2::text[])`,
		tx.userID.String(), victimIDs); err != nil {
		return 0, nil, fmt.Errorf("store: prune model versions (delete): %w", err)
	}
	deleted = len(victimIDs)

	// 4. De-duplicate victim hashes and hand them to the ONE shared guard, which
	// runs AFTER the delete above so it sees the surviving ml_models rows — a
	// hash another version still references simply does not come back.
	seen := make(map[string]struct{}, len(victimHashes))
	candidates := make([][]byte, 0, len(victimHashes))
	for _, h := range victimHashes {
		if _, dup := seen[string(h)]; dup {
			continue
		}
		seen[string(h)] = struct{}{}
		candidates = append(candidates, h)
	}
	// Hashes with no app.blobs row at all come back empty rather than as an
	// error, which is the documented behaviour of the guard — and it is the
	// reason `backfill-model-blobs` exists: an artifact written before #316 has
	// no row, so pruning it here removes its ml_models row and correctly refuses
	// to delete bytes this transaction cannot prove it owns. Those files are
	// reported by `blob-report` as files-with-no-row until they are backfilled.
	freed, err = tx.reclaimUnreferencedBlobs(ctx, candidates)
	if err != nil {
		return 0, nil, err
	}
	return deleted, freed, nil
}

// ModelArtifactRef is one distinct artifact hash the caller's model registry
// references, and whether app.blobs already accounts for it — the input to
// `mailroom-admin backfill-model-blobs` (#316).
type ModelArtifactRef struct {
	// ContentHash is the artifact's hash: app.ml_models.artifact_hash, which is
	// the same content-addressed identity app.blobs.content_hash uses.
	ContentHash []byte
	// Versions is how many ml_models rows reference this hash. It is >1 for
	// byte-identical snapshots (an unchanged model re-snapshotted), which is
	// exactly why the report counts DISTINCT HASHES separately from model rows:
	// on a real instance more versions than files existed, and a report that
	// conflated the two would have named the wrong number of missing rows.
	Versions int
	// HasBlobRow is whether app.blobs already accounts for these bytes. Before
	// #316 this was false for every model artifact ever written.
	HasBlobRow bool
}

// ModelArtifactRefs lists the caller's distinct model artifact hashes with their
// app.blobs status, ordered by hash so a report is stable across runs.
//
// It is READ-ONLY. The backfill it feeds writes nothing without --apply, which
// is the order #177 established: on a real instance the producer of these
// orphans turned out not to be what the issue's hypothesis said it was, and the
// report is what established that.
func (tx *UserTx) ModelArtifactRefs(ctx context.Context) ([]ModelArtifactRef, error) {
	rows, err := tx.tx.Query(ctx, `
		SELECT m.artifact_hash,
		       count(*)::int,
		       EXISTS (SELECT 1 FROM app.blobs b
		                WHERE b.user_id = m.user_id AND b.content_hash = m.artifact_hash)
		  FROM app.ml_models m
		 WHERE m.user_id = $1
		 GROUP BY m.user_id, m.artifact_hash
		 ORDER BY m.artifact_hash`, tx.userID.String())
	if err != nil {
		return nil, fmt.Errorf("store: model artifact refs: %w", err)
	}
	defer rows.Close()

	var out []ModelArtifactRef
	for rows.Next() {
		var r ModelArtifactRef
		if err := rows.Scan(&r.ContentHash, &r.Versions, &r.HasBlobRow); err != nil {
			return nil, fmt.Errorf("store: scan model artifact ref: %w", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: model artifact refs rows: %w", err)
	}
	return out, nil
}

// FeedbackEvent is one app.feedback_events row: an observed user action on a
// message, the raw signal the triage model learns from (spec §8).
type FeedbackEvent struct {
	ID        ulid.ULID
	MessageID *ulid.ULID // nullable: an action not tied to a specific message
	Action    string
	Context   json.RawMessage
	CreatedAt time.Time
}

// ListFeedbackEvents returns the caller's feedback events in [since, until),
// oldest first (created_at, then id as a stable tie-break). The time window lets
// a caller build a TRAIN/EVAL split — train on older events, hold out newer ones
// — while this method stays composable: it just returns the ordered slice and
// takes no view on the split. RLS scopes the read to the caller's own events.
func (tx *UserTx) ListFeedbackEvents(ctx context.Context, since, until time.Time) ([]FeedbackEvent, error) {
	rows, err := tx.tx.Query(ctx, `
		SELECT id, message_id, action, context, created_at
		  FROM app.feedback_events
		 WHERE user_id = $1 AND created_at >= $2 AND created_at < $3
		 ORDER BY created_at, id`,
		tx.userID.String(), since.UTC(), until.UTC())
	if err != nil {
		return nil, fmt.Errorf("store: list feedback events: %w", err)
	}
	defer rows.Close()

	var out []FeedbackEvent
	for rows.Next() {
		var (
			e      FeedbackEvent
			idStr  string
			msgStr *string
			ctxRaw []byte
		)
		if err := rows.Scan(&idStr, &msgStr, &e.Action, &ctxRaw, &e.CreatedAt); err != nil {
			return nil, fmt.Errorf("store: scan feedback event: %w", err)
		}
		if e.ID, err = ulid.Parse(idStr); err != nil {
			return nil, fmt.Errorf("store: parse feedback id %q: %w", idStr, err)
		}
		if msgStr != nil {
			m, merr := ulid.Parse(*msgStr)
			if merr != nil {
				return nil, fmt.Errorf("store: parse feedback message id %q: %w", *msgStr, merr)
			}
			e.MessageID = &m
		}
		if ctxRaw != nil {
			e.Context = json.RawMessage(append([]byte(nil), ctxRaw...))
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list feedback events rows: %w", err)
	}
	return out, nil
}
