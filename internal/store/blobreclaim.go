// SPDX-License-Identifier: AGPL-3.0-only

package store

import (
	"context"
	"encoding/hex"
	"fmt"
)

// This file holds the ONE blob-reclaim guard the store shares (#300).
//
// # Why it is one function and not five copies
//
// Reclaiming a blob is the single most destructive statement in the codebase:
// the app.blobs row carries the dek_id the ciphertext is opened with, so a row
// deleted while something still needs the bytes does not merely leak — it makes
// the file on disk permanently unreadable. Every path that deletes it therefore
// has to ask exactly the same question, and until #300 four paths asked four
// different ones:
//
//	purge.go      message + attachment          (correct as far as it went)
//	collapse.go   message + attachment          (a second copy of the same SQL)
//	drafts.go     nothing at all — the row leaked
//	accounts.go   nothing at all — the row leaked
//
// The audit measured the result: blobs outnumbered messages, with a few
// app.blobs rows referenced by no message, no attachment and no
// model. Half of those were superseded content (ReplaceMessageBytes at
// signing time), the rest came from RetireDraft / DeleteAccount.
//
// # The guard, and the third referent nobody had
//
// Identity is (user_id, content_hash) with a UNIQUE constraint, so at most one
// MESSAGE ever references a blob. The genuine sharing is with ATTACHMENTS (a
// forwarded .eml whose bytes equal another message, or the same attachment on
// several messages — content-addressed dedup, data-model §2) and, the referent
// #300 found missing from BOTH existing copies, with app.ml_models.artifact_hash
// (schema.sql:1212, "blob ref"). That column is a blob reference with NO foreign
// key to enforce it — exactly like attachments — so nothing would have stopped a
// purge from reclaiming the bytes of a live model artifact out from under the
// registry.
//
// ⚠ AND UNTIL #316 THAT REFERENT COULD NOT FIRE AT ALL. The reason recorded here
// was that it had never fired because "model artifacts are not mail and no
// message has ever hashed to one" — luck rather than a guarantee, which was the
// argument for adding it. The real reason was worse: the registry wrote the model
// FILE and the ml_models row and never an app.blobs row, so the clause below
// protects a row that was never created. It was a check that could not fire,
// added in the wave whose subject was checks that cannot fire. The registry now
// records the row (ml/registry.go), which is what makes this guard real.
//
// The reciprocal hazard was open at the same time and is now closed with it:
// PruneModelVersions asked only whether a surviving MODEL still referenced a
// hash, and its caller then deleted the file — so a model-shaped reclaim could
// destroy the bytes of a message that shared them. It comes through this
// function now, like everything else.
//
// # What the caller still owes
//
// This deletes the METADATA ROW ONLY, and returns the hashes it removed so the
// caller can delete the FILE **inside the same transaction, before it commits**.
// That ordering is the race proof spelled out in HardDeleteMessage: the row
// delete is invisible to a concurrent ingest until commit, so the "not found by
// content hash" window a byte-identical re-ingest needs in order to re-Put the
// bytes opens only AFTER the file is already gone, never during. A file-delete
// failure aborts the transaction, so the failure mode is "retry", never a
// canonical row that outlives its bytes.
//
// Returning the hashes rather than deleting the file here is deliberate: the
// store is the only package that touches the DATABASE, and the blob store is a
// filesystem. Making the caller take the second half also makes it impossible to
// half-reclaim by accident — a caller that ignores the return value fails to
// compile.

// reclaimUnreferencedBlobs deletes the app.blobs rows for hashes that nothing
// else references, and returns exactly the hashes it removed. Hashes still
// referenced by a message, an attachment, a model artifact or a CACHED PROXIED
// IMAGE are left alone — silently, because "still referenced" is the ordinary
// case and not an error.
//
// ⚠ THE FOURTH REFERENT IS app.proxy_image_cache (migration 00038, ADR-026), and
// it is here for the reason the third one is. A cache entry names its bytes by
// content hash with NO foreign key, exactly like an attachment and like
// ml_models.artifact_hash. Blobs are content-addressed, so a cached image whose
// plaintext collided with a message's or an attachment's would share ONE row —
// and a purge of that message would take the file with it, leaving the cache
// pointing at bytes that no longer exist and, worse, at a dek_id that no longer
// exists either. Per this file's own opening: that does not leak, it makes the
// ciphertext permanently unreadable.
//
// This is #300's ml_models hazard repeating with a new writer. It was written
// down when the cache was designed rather than found later by a corrupted read.
//
// It is safe to call with hashes that have no blob row at all (a caller that
// does not know whether the bytes were ever stored): those simply do not come
// back.
func (tx *UserTx) reclaimUnreferencedBlobs(ctx context.Context, hashes [][]byte) ([][]byte, error) {
	if len(hashes) == 0 {
		return nil, nil
	}
	rows, err := tx.tx.Query(ctx, `
		DELETE FROM app.blobs b
		 WHERE b.user_id = $1
		   AND b.content_hash = ANY($2::bytea[])
		   AND NOT EXISTS (SELECT 1 FROM app.messages m
		                    WHERE m.user_id = $1 AND m.content_hash = b.content_hash)
		   AND NOT EXISTS (SELECT 1 FROM app.attachments a
		                    WHERE a.user_id = $1 AND a.content_hash = b.content_hash)
		   AND NOT EXISTS (SELECT 1 FROM app.ml_models mm
		                    WHERE mm.user_id = $1 AND mm.artifact_hash = b.content_hash)
		   AND NOT EXISTS (SELECT 1 FROM app.proxy_image_cache pc
		                    WHERE pc.user_id = $1 AND pc.content_hash = b.content_hash)
		RETURNING b.content_hash`,
		tx.userID.String(), hashes)
	if err != nil {
		return nil, fmt.Errorf("store: reclaim unreferenced blobs: %w", err)
	}
	defer rows.Close()
	var out [][]byte
	for rows.Next() {
		var h []byte
		if err := rows.Scan(&h); err != nil {
			return nil, fmt.Errorf("store: scan reclaimed blob: %w", err)
		}
		out = append(out, h)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: reclaim unreferenced blobs: %w", err)
	}
	return out, nil
}

// hashesWithLiveWriteback returns the subset of hashes that a NOT-YET-TERMINAL
// writeback intent still names in its payload — bytes the engine is going to
// read back out of the blob store when it applies that intent.
//
// ⚠ THIS IS WHY ReplaceMessageBytes CANNOT USE THE PLAIN GUARD (#268, #300).
// An APPEND payload names its bytes by content hash and NOTHING ELSE — there is
// no message_id, no foreign key, no row anywhere that ties the intent to the
// blob it is going to write. Signing rewrites a draft's bytes at SUBMIT time,
// which is typically seconds after the draft was saved and MINUTES before the
// writeback pass that files the Drafts copy: at that moment there is normally a
// pending APPEND naming the pre-signature hash. Reclaiming it under the plain
// guard would take the bytes out from under an intent that is still going to
// run, turning every signed send into a dead-lettered "could not file the draft"
// in account health.
//
// So the intent guard is not belt-and-braces here, it is the load-bearing half:
// without it this reclaim would be a regression dressed as a fix.
//
// ⚠ 'suppressed' COUNTS AS LIVE HERE, AND OMITTING IT WAS #557's REAL MECHANISM.
// This doc says "NOT-YET-TERMINAL", but the predicate enumerated states —
// pending and inflight — and migration 00056 then added a third without this
// being revisited. On a shadowed account (ADR-028) a draft's APPEND is born
// 'suppressed', so signing at submit time asked this guard whether anything
// still needed the pre-signature bytes, was told no, and reclaimed them. The
// ledger row survived naming bytes that no longer existed, and at --apply the
// engine resolved the body by content hash, got blob.ErrNotFound and
// dead-lettered it: one writeback FAILURE per signed send during the run.
//
// That is the same "could not file the draft" outcome the paragraph above says
// this guard exists to prevent — reintroduced for shadowed accounts only, by
// the state nobody added to the list. --apply is the documented way a
// suppressed ledger is replayed, so its bytes are needed exactly as much as a
// pending one's; 'suppressed' being terminal for CLAIMING says nothing about
// whether the blob is still referenced.
//
// ⚠ #557 NAMED A DIFFERENT MECHANISM (RetireDraft's pending-only cancel), and
// that one cannot bite: RetireDraft deletes the message row and
// writeback_intents.message_id is ON DELETE CASCADE, so an APPEND there is
// removed by the cascade whatever its state. A probe written against that path
// passed green on unfixed code, which is what sent me here. The unprotected
// path is RetireSentDraftCopy, which does NOT delete the message.
//
// 'dead' and 'applied' stay out: those bytes really are finished with.
func (tx *UserTx) hashesWithLiveWriteback(ctx context.Context, hashes [][]byte) (map[string]bool, error) {
	if len(hashes) == 0 {
		return nil, nil
	}
	wanted := hexHashes(hashes)
	rows, err := tx.tx.Query(ctx, `
		SELECT DISTINCT payload->>'content_hash'
		  FROM app.writeback_intents
		 WHERE user_id = $1
		   AND state IN ('pending', 'inflight', 'suppressed')
		   AND payload->>'content_hash' = ANY($2::text[])`,
		tx.userID.String(), wanted)
	if err != nil {
		return nil, fmt.Errorf("store: blobs named by live writeback: %w", err)
	}
	defer rows.Close()
	held := map[string]bool{}
	for rows.Next() {
		var h string
		if err := rows.Scan(&h); err != nil {
			return nil, fmt.Errorf("store: scan live writeback hash: %w", err)
		}
		held[h] = true
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: blobs named by live writeback: %w", err)
	}
	return held, nil
}

// hexHashes renders raw content hashes the way a writeback payload names bytes.
func hexHashes(hashes [][]byte) []string {
	out := make([]string, 0, len(hashes))
	for _, h := range hashes {
		out = append(out, hex.EncodeToString(h))
	}
	return out
}

// BlobRowCensus is one user's blob-row side of a reconciliation (#300). Every
// field is a COUNT OF A DISTINCT SIGNAL rather than a share of one total,
// because a single "orphans: 4" cannot distinguish "nothing to do" from "the
// thing I measured is always empty" — the failure this project keeps paying for
// (#177, #191, #155).
type BlobRowCensus struct {
	// Rows is every app.blobs row this user has: the denominator for everything
	// below, and the number a "0 orphans" result has to be read against.
	Rows int
	// Referenced counts rows something still points at, by which pointer. A row
	// may be reached more than one way, so these do not sum to Rows.
	ByMessage    int
	ByAttachment int
	ByModel      int
	// ByProxyCache counts rows a cached proxied image names (#418 item 3).
	//
	// ⚠ IT WAS MISSING HERE WHILE THE RECLAIM GUARD CHECKED IT, so every
	// cached-image blob was reported as "unreferenced row (leaks unless
	// reclaimed)" while being correctly referenced and correctly retained. A
	// wrong NUMBER, never a wrong delete — blob-report is read-only and the
	// guard was right all along — but this is an audit surface whose entire
	// value is that its statements can be trusted.
	//
	// This is #300's shape one more time: a referent added to the guard and not
	// to the thing that counts. The two predicates below and in
	// reclaimUnreferencedBlobs must name the SAME four tables.
	ByProxyCache int
	// Unreferenced is the reclaim guard's own predicate: no message, no
	// attachment, no model artifact, no cached proxied image. These are the rows
	// that leak.
	Unreferenced int
	// UnreferencedSuperseded is the subset of Unreferenced whose hash appears in
	// app.superseded_content — bytes a message USED to have, before compose-time
	// PGP signing rewrote them in place. ATTRIBUTION PER PRODUCER PATH is the
	// point: in the audit half of the orphans were this, which named
	// ReplaceMessageBytes as their producer and made the fix findable.
	UnreferencedSuperseded int
	// UnreferencedHeldByIntent is the subset a not-yet-terminal writeback intent
	// still names. These are NOT leaks — the engine is about to read those bytes
	// — and counting them separately is what stops a future reader "cleaning up"
	// a number that is supposed to be non-zero (#268).
	UnreferencedHeldByIntent int
	// Hashes is every hash in app.blobs, for the on-disk half of the
	// reconciliation.
	Hashes [][]byte
	// UnreferencedHashes is Unreferenced, listed, so --apply-style follow-up work
	// (or a human) can act on specific rows rather than a count.
	UnreferencedHashes [][]byte
}

// BlobRowCensus counts and classifies this user's blob rows for the
// reconciliation report. It is READ-ONLY: it exists so the report can be built
// and read before anything is deleted, which is the order #177 established.
func (tx *UserTx) BlobRowCensus(ctx context.Context) (BlobRowCensus, error) {
	var c BlobRowCensus

	// One pass, classifying every row, rather than five COUNT queries: the
	// classifications must be mutually consistent, and separate statements in
	// the same transaction could not disagree but could easily drift in their
	// predicates — which is how the two copies of the reclaim guard came to
	// disagree about what counts as still-in-use (#300).
	rows, err := tx.tx.Query(ctx, `
		SELECT b.content_hash,
		       EXISTS (SELECT 1 FROM app.messages m
		                WHERE m.user_id = b.user_id AND m.content_hash = b.content_hash),
		       EXISTS (SELECT 1 FROM app.attachments a
		                WHERE a.user_id = b.user_id AND a.content_hash = b.content_hash),
		       EXISTS (SELECT 1 FROM app.ml_models mm
		                WHERE mm.user_id = b.user_id AND mm.artifact_hash = b.content_hash),
		       EXISTS (SELECT 1 FROM app.proxy_image_cache pc
		                WHERE pc.user_id = b.user_id AND pc.content_hash = b.content_hash),
		       EXISTS (SELECT 1 FROM app.superseded_content sc
		                WHERE sc.user_id = b.user_id AND sc.content_hash = b.content_hash)
		  FROM app.blobs b
		 WHERE b.user_id = $1
		 ORDER BY b.content_hash`, tx.userID.String())
	if err != nil {
		return c, fmt.Errorf("store: blob row census: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var (
			h                                             []byte
			byMsg, byAtt, byModel, byProxy, wasSuperseded bool
		)
		if err := rows.Scan(&h, &byMsg, &byAtt, &byModel, &byProxy, &wasSuperseded); err != nil {
			return c, fmt.Errorf("store: scan blob census row: %w", err)
		}
		c.Rows++
		c.Hashes = append(c.Hashes, h)
		if byMsg {
			c.ByMessage++
		}
		if byAtt {
			c.ByAttachment++
		}
		if byModel {
			c.ByModel++
		}
		if byProxy {
			c.ByProxyCache++
		}
		// ⚠ THE SAME FOUR REFERENTS reclaimUnreferencedBlobs CHECKS. Adding one
		// there and not here is exactly the drift #418 item 3 found, and #300
		// before it — a census that calls a correctly-retained row a leak.
		if !byMsg && !byAtt && !byModel && !byProxy {
			c.Unreferenced++
			c.UnreferencedHashes = append(c.UnreferencedHashes, h)
			if wasSuperseded {
				c.UnreferencedSuperseded++
			}
		}
	}
	if err := rows.Err(); err != nil {
		return c, fmt.Errorf("store: blob row census: %w", err)
	}

	// The intent-held subset comes from the same helper the reclaim path uses,
	// deliberately: a report that decided "held by an intent" its own way could
	// disagree with the code that acts on it, and then the report would be
	// describing a system that does not exist.
	if len(c.UnreferencedHashes) > 0 {
		held, err := tx.hashesWithLiveWriteback(ctx, c.UnreferencedHashes)
		if err != nil {
			return c, err
		}
		for _, h := range c.UnreferencedHashes {
			if held[hex.EncodeToString(h)] {
				c.UnreferencedHeldByIntent++
			}
		}
	}
	return c, nil
}
