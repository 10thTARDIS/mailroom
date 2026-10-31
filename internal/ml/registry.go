// SPDX-License-Identifier: AGPL-3.0-only

package ml

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"time"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/blob"
	"mailroom/internal/crypto"
	"mailroom/internal/onboarding"
	"mailroom/internal/store"
)

// ModelDEKInfoV1 is the HKDF domain-separation label for the per-user model
// data-encryption key (ADR-013, the same derive-don't-store pattern as the
// account-credentials and mail-blob keys). Model artifacts are stored as
// ENCRYPTED blobs under this key; versioning the label lets a future scheme
// derive distinct material without colliding with v1.
const ModelDEKInfoV1 = "mailroom-ml-model-v1"

// ModelBlobSchemeV1 is the sentinel DEKID recorded on the app.blobs row of a
// model artifact: like syncrunner.MailBlobSchemeV1 it names the DERIVATION
// SCHEME, because ADR-013's scheme derives the key and stores no key row for
// dek_id to point at. It is what tells a reader of app.blobs which key opens
// these bytes — mail blobs and model artifacts are in one content-addressed
// namespace and are NOT interchangeable, since they are sealed under different
// HKDF labels.
var ModelBlobSchemeV1 = onboarding.SchemeULID("ml-model-v1")

// DefaultKeepN is the recommended retention for Prune: keep the newest 10
// snapshots per (user, kind) plus the active one. Model artifacts are tiny, so
// history is cheap; 10 is plenty of rollback depth. Callers pass their own N —
// this is the documented default to reach for.
const DefaultKeepN = 10

// Registry is the per-user model registry (spec §8, mailroom#35): it snapshots
// models into the encrypted blob store, tracks lineage and the single active
// version in app.ml_models, and offers rollback / reset / prune. Every operation
// goes through store.UserTx, so RLS scopes it to one user; no identity, no rows.
type Registry struct {
	st    *store.Store
	blobs blob.BlobStore
	keys  crypto.KeyProvider
}

// NewRegistry builds a registry over the store, blob store and master-key
// provider. The blob store MUST be the same one the rest of the app uses (model
// artifacts share the per-user content-addressed namespace).
func NewRegistry(st *store.Store, blobs blob.BlobStore, keys crypto.KeyProvider) *Registry {
	return &Registry{st: st, blobs: blobs, keys: keys}
}

// modelDEK derives the per-user model DEK from the master key:
// HKDF(master, salt=userID, info=ModelDEKInfoV1). The caller owns the returned
// key and must crypto.Zero it when done.
func (r *Registry) modelDEK(ctx context.Context, userID ulid.ULID) ([]byte, error) {
	master, err := r.keys.MasterKey(ctx)
	if err != nil {
		return nil, fmt.Errorf("ml: master key: %w", err)
	}
	defer crypto.Zero(master)
	dek, err := crypto.DeriveKey(master, userID[:], ModelDEKInfoV1)
	if err != nil {
		return nil, fmt.Errorf("ml: derive model dek: %w", err)
	}
	return dek, nil
}

// Snapshot serializes m, stores it as an encrypted blob, and records a new
// INACTIVE version row whose parent is the currently-active version (lineage).
// It returns the assigned version. Snapshotting never changes which model is
// active — call Activate for that.
func (r *Registry) Snapshot(ctx context.Context, userID ulid.ULID, m Model) (int, error) {
	return r.snapshot(ctx, userID, m, true)
}

// snapshot is the shared body of Snapshot and Reset. linkParent chooses whether
// the new row's parent is the active version (normal snapshot, lineage) or NULL
// (Reset, a deliberate lineage break rooting a fresh baseline).
func (r *Registry) snapshot(ctx context.Context, userID ulid.ULID, m Model, linkParent bool) (int, error) {
	data, err := m.MarshalBinary()
	if err != nil {
		return 0, fmt.Errorf("ml: marshal model: %w", err)
	}
	dek, err := r.modelDEK(ctx, userID)
	if err != nil {
		return 0, err
	}
	defer crypto.Zero(dek)

	hash, err := r.blobs.Put(ctx, userID, dek, bytes.NewReader(data))
	if err != nil {
		return 0, fmt.Errorf("ml: put model artifact: %w", err)
	}

	// ⚠ THE app.blobs ROW IS NOT OPTIONAL BOOKKEEPING (#316).
	//
	// This wrote the FILE and recorded the MODEL and, until #316, nothing else —
	// so every model artifact this instance has ever produced was a file with no
	// app.blobs row. Nothing broke, which is why it survived: ActiveModel reads
	// through blobs.Get, which is content-addressed on disk and never consults
	// app.blobs. The absence of an error proved nothing (CLAUDE.md), and three
	// things were quietly false the whole time:
	//
	//   - the bytes were invisible to blob accounting — no size, no dek_id, no
	//     envelope_ver, and unreachable by the reconciliation report, so they
	//     leaked one file per distinct version forever;
	//   - reclaimUnreferencedBlobs' ml_models guard (#300) COULD NOT FIRE, since
	//     it protects an app.blobs row from being reclaimed and no such row was
	//     ever created — a check that cannot fire, added during the wave whose
	//     subject was checks that cannot fire;
	//   - a hard-delete sweep driven off app.blobs (#54) would have walked past
	//     every model artifact the user owns.
	//
	// StoredSize is read BEFORE the transaction opens: it stats the file we just
	// wrote, and holding a database transaction open across filesystem I/O is
	// what the rest of the store deliberately avoids.
	storedSize, err := r.blobs.StoredSize(ctx, userID, hash)
	if err != nil {
		return 0, fmt.Errorf("ml: size of model artifact: %w", err)
	}

	var version int
	err = r.st.UserTx(ctx, userID, func(tx *store.UserTx) error {
		// Same transaction as the model row, deliberately: the pair is what makes
		// the reclaim guard real, and a blobs row without its ml_models row would
		// read as an unreferenced blob — i.e. as something safe to reclaim.
		//
		// InsertBlobMeta is ON CONFLICT DO NOTHING and blobs are content-addressed,
		// so re-snapshotting an unchanged model (this instance has three such
		// versions sharing one hash) reuses the one row rather than colliding.
		//
		// NOT quota-gated, unlike ingest: StorageQuota sums app.blobs.size_stored,
		// so these bytes now COUNT toward the user's quota (correctly — they are
		// real disk), but a training run must not fail to record a model it has
		// already written because the mailbox is full. A few KB per version is not
		// worth a failure mode that loses the model.
		if e := tx.InsertBlobMeta(ctx, store.BlobMeta{
			ContentHash: hash[:],
			SizePlain:   int64(len(data)),
			SizeStored:  storedSize,
			DEKID:       ModelBlobSchemeV1,
			EnvelopeVer: 1,
		}); e != nil {
			return e
		}
		var e error
		version, e = tx.InsertModelVersion(ctx, m.Kind(), hash[:], linkParent)
		return e
	})
	if err != nil {
		return 0, err
	}
	return version, nil
}

// Activate makes version the single active model of kind for the user. A missing
// version returns store.ErrModelVersionNotFound and changes nothing.
func (r *Registry) Activate(ctx context.Context, userID ulid.ULID, kind string, version int) error {
	return r.st.UserTx(ctx, userID, func(tx *store.UserTx) error {
		return tx.ActivateModelVersion(ctx, kind, version)
	})
}

// Rollback re-activates an older version — it is exactly Activate against a
// version that already exists (validated), and never deletes a row, so the model
// you rolled away from is still there to roll forward to.
func (r *Registry) Rollback(ctx context.Context, userID ulid.ULID, kind string, toVersion int) error {
	return r.Activate(ctx, userID, kind, toVersion)
}

// Reset is the "reset my model" safety valve: it snapshots a FRESH baseline with
// its lineage deliberately broken (parent_id NULL) and activates it, so the user
// starts over from a clean scorer while every prior version remains on record for
// audit / manual rollback. It returns the new active version.
func (r *Registry) Reset(ctx context.Context, userID ulid.ULID, kind string) (int, error) {
	version, err := r.snapshot(ctx, userID, NewBaseline(kind), false)
	if err != nil {
		return 0, err
	}
	if err := r.Activate(ctx, userID, kind, version); err != nil {
		return 0, err
	}
	return version, nil
}

// ActiveModel loads and reconstructs the user's active model of kind, returning
// it and its version. When the user has NO active model (fresh user, or only
// never-activated snapshots) it returns the fallback at version 0 — the
// deterministic cold-start heuristic (#36) for the priority kind, a fresh
// NewBaseline for any other kind — so the caller always gets a usable scorer and
// never has to special-case "no model yet". A stored-but-active model whose
// artifact is missing or corrupt is a real error (not silently masked).
func (r *Registry) ActiveModel(ctx context.Context, userID ulid.ULID, kind string) (Model, int, error) {
	var (
		hash    []byte
		version int
		found   bool
	)
	err := r.st.UserTx(ctx, userID, func(tx *store.UserTx) error {
		var e error
		hash, version, found, e = tx.ActiveModelRow(ctx, kind)
		return e
	})
	if err != nil {
		return nil, 0, err
	}
	if !found {
		// No trained model yet. For the priority kind, hand back the deterministic
		// cold-start heuristic (#36) so mail is sensibly prioritized from day one
		// (direct/known up, list/auth-failing-bulk down) instead of the baseline's
		// flat 0.5; other kinds keep the feature-blind baseline. Version stays 0:
		// modelVersionString(0) documents 0 as the "no trained model" fallback, and
		// a heuristic at v0 keeps that honest — v0 now means "heuristic cold-start,
		// no trained model yet." Once #33's trainer activates a real version this
		// branch is not taken and that model is returned instead.
		if kind == KindPriority {
			return newColdStart(), 0, nil
		}
		return NewBaseline(kind), 0, nil
	}

	m, err := r.loadArtifact(ctx, userID, kind, version, hash)
	if err != nil {
		return nil, 0, err
	}
	return m, version, nil
}

// loadArtifact reads, decrypts and decodes one model artifact by hash.
func (r *Registry) loadArtifact(ctx context.Context, userID ulid.ULID, kind string, version int, hash []byte) (Model, error) {
	ch, err := contentHash(hash)
	if err != nil {
		return nil, err
	}
	dek, err := r.modelDEK(ctx, userID)
	if err != nil {
		return nil, err
	}
	defer crypto.Zero(dek)

	rc, err := r.blobs.Get(ctx, userID, dek, ch)
	if err != nil {
		return nil, fmt.Errorf("ml: get model artifact (kind %q v%d): %w", kind, version, err)
	}
	defer func() { _ = rc.Close() }()
	data, err := io.ReadAll(rc)
	if err != nil {
		return nil, fmt.Errorf("ml: read model artifact (kind %q v%d): %w", kind, version, err)
	}
	m, err := UnmarshalModel(kind, data)
	if err != nil {
		return nil, fmt.Errorf("ml: decode model artifact (kind %q v%d): %w", kind, version, err)
	}
	return m, nil
}

// ActiveVersion reports the user's active version of kind, or 0 when the
// built-in fallback (the cold start, for priority) is serving. It reads the row
// only — no artifact load — so the training pass can learn "is a trained model
// in service" without paying for, or failing on, the blob.
func (r *Registry) ActiveVersion(ctx context.Context, userID ulid.ULID, kind string) (int, error) {
	var version int
	err := r.st.UserTx(ctx, userID, func(tx *store.UserTx) error {
		_, v, found, e := tx.ActiveModelRow(ctx, kind)
		if found {
			version = v
		}
		return e
	})
	return version, err
}

// LoadVersion loads one stored version WITHOUT activating it, for a report
// that scores a specific version side by side with the one in service. Version
// 0 is the built-in fallback. createdAt is the snapshot time (zero for v0).
func (r *Registry) LoadVersion(ctx context.Context, userID ulid.ULID, kind string, version int) (Model, time.Time, error) {
	if version == 0 {
		if kind == KindPriority {
			return newColdStart(), time.Time{}, nil
		}
		return NewBaseline(kind), time.Time{}, nil
	}
	var (
		hash      []byte
		createdAt time.Time
		found     bool
	)
	err := r.st.UserTx(ctx, userID, func(tx *store.UserTx) error {
		var e error
		hash, _, createdAt, found, e = tx.ModelVersionRow(ctx, kind, version)
		return e
	})
	if err != nil {
		return nil, time.Time{}, err
	}
	if !found {
		return nil, time.Time{}, store.ErrModelVersionNotFound
	}
	m, err := r.loadArtifact(ctx, userID, kind, version, hash)
	return m, createdAt, err
}

// RevertToColdStart deactivates the user's model of kind, so ActiveModel
// returns the built-in fallback (the cold-start heuristic, for priority) at
// version 0. Nothing is deleted: the reverted version stays in the lineage and
// can be re-activated deliberately. It is what the promotion gate does when a
// trained model in service no longer earns its place (#842, ADR-043).
//
// Unlike Reset, which ACTIVATES a fresh feature-blind baseline: that model
// scores every message exactly 0.5, below the 0.6 line, so a reset user sees
// nothing important until a pass promotes something — the same symptom #842
// reports, reached by a different road. The daily gate now reverts such a
// baseline to the cold start on its next pass.
func (r *Registry) RevertToColdStart(ctx context.Context, userID ulid.ULID, kind string) error {
	return r.st.UserTx(ctx, userID, func(tx *store.UserTx) error {
		_, err := tx.DeactivateModelKind(ctx, kind)
		return err
	})
}

// ListVersions returns the user's lineage for kind, oldest first.
func (r *Registry) ListVersions(ctx context.Context, userID ulid.ULID, kind string) ([]store.VersionInfo, error) {
	var out []store.VersionInfo
	err := r.st.UserTx(ctx, userID, func(tx *store.UserTx) error {
		var e error
		out, e = tx.ListModelVersions(ctx, kind)
		return e
	})
	return out, err
}

// Prune deletes the user's oldest snapshots of kind, keeping the newest keepN
// versions and ALWAYS the active one, then reclaims the blob artifacts that no
// surviving row references. It returns the number of rows deleted.
//
// ⚠ THIS DOC COMMENT USED TO DESCRIBE THE OPPOSITE DESIGN, AND IT WAS LOAD-
// BEARING (#316). It said model artifacts are "recorded ONLY in ml_models (not
// app.blobs) — ml_models IS their ref registry — so there is no metadata row to
// reconcile and quota does not count them". That was a deliberate choice, written
// down here, and it is what makes this more than an oversight: #300 later added
// an ml_models referent to the shared reclaim guard, which only means anything if
// a model artifact HAS an app.blobs row. Two decisions, in two files, assuming
// opposite worlds, and every check written on top of them was vacuous.
//
// It is resolved in favour of recording the row, because the alternative — keep
// artifacts out of app.blobs and delete #300's guard as inapplicable — leaves
// their bytes permanently invisible to the only reconciliation that exists
// (blob-report walks disk against app.blobs, which is how #316 was found at all),
// leaves #54's hard-delete sweep walking past every model a user owns, and leaves
// the shared content-addressed namespace with one class of blob that no guard on
// either side can protect. The cost is that models now count toward the storage
// quota, which is the honest answer anyway: the bytes are real disk.
//
// Blob reclaim is safe by the ONE shared guard, not by faith: the store deletes
// the ml_models rows, then asks whether any message, attachment or surviving
// model still references those bytes, and reports only the app.blobs rows it
// actually removed.
//
// The files are deleted INSIDE the transaction, before it commits — the ordering
// blobreclaim.go spells out. This function used to delete them after the tx
// returned, which was harmless only because there was no row whose disappearance
// could race an ingest; now there is one, so it observes the same rule as every
// other reclaim path. A file-delete failure aborts the transaction, so the
// failure mode is "nothing was pruned, retry", never an ml_models row that
// outlives bytes or bytes that outlive their row.
func (r *Registry) Prune(ctx context.Context, userID ulid.ULID, kind string, keepN int) (int, error) {
	var deleted int
	err := r.st.UserTx(ctx, userID, func(tx *store.UserTx) error {
		d, freed, e := tx.PruneModelVersions(ctx, kind, keepN)
		if e != nil {
			return e
		}
		deleted = d
		for _, h := range freed {
			ch, cerr := contentHash(h)
			if cerr != nil {
				return cerr
			}
			if derr := r.blobs.Delete(ctx, userID, ch); derr != nil {
				return fmt.Errorf("ml: prune reclaim blob: %w", derr)
			}
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return deleted, nil
}

// contentHash narrows a 32-byte hash slice to the blob store's array type.
func contentHash(b []byte) (blob.ContentHash, error) {
	var h blob.ContentHash
	if len(b) != len(h) {
		return h, fmt.Errorf("ml: artifact hash is %d bytes, want %d", len(b), len(h))
	}
	copy(h[:], b)
	return h, nil
}
