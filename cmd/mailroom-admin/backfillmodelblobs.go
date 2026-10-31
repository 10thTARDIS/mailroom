// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"context"
	"encoding/hex"
	"flag"
	"fmt"
	"io"
	"strings"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/blob"
	"mailroom/internal/crypto"
	"mailroom/internal/ml"
	"mailroom/internal/store"
)

// `mailroom-admin backfill-model-blobs` — give the model artifacts written
// before #316 the app.blobs row they never had.
//
// WHY IT EXISTS. `Registry.snapshot` wrote the artifact FILE and the
// app.ml_models row and never an app.blobs row, so every model artifact this
// instance ever produced was a file no query could reach: invisible to blob
// accounting, invisible to quota, and — the part that made it a correctness
// problem rather than a tidiness one — invisible to the reclaim guard #300 added
// to protect exactly these bytes. The registry now writes the row. This is the
// other half: the artifacts already on disk.
//
// ⚠ IT IS NOT OPTIONAL CLEANUP, WHICH IS UNUSUAL FOR A BACKFILL. Prune used to
// delete an artifact's file whenever no surviving model row referenced it,
// asking nothing else; it now goes through the one shared reclaim guard, which
// deletes a file only when it removed that file's app.blobs row. So an artifact
// with no row is one Prune will now correctly refuse to delete — its ml_models
// row goes and its bytes stay, forever. Without this backfill the #316 fix would
// make the existing leak worse rather than better.
//
// REPORT FIRST, --apply SECOND (#177). The default run writes nothing. That rule
// has already earned itself twice on this exact issue: the leak was found by a
// read-only report, and the issue's own stated hypothesis about what produced it
// was wrong — the report is what corrected it.
//
// ⚠ EVERY COUNT IS PER SIGNAL, NEVER ONE TOTAL. "4 backfilled" cannot distinguish
// the outcomes below, and they are not the same news:
//
//   - MISSING is the ordinary case: bytes on disk, readable, no row. Backfillable.
//   - NO-FILE is a model row pointing at bytes that are GONE. It is strictly
//     worse than the leak this command exists for — that model cannot load — and
//     no row is written for it, because a row claiming bytes exist when they do
//     not is what turns a silent leak into a broken read.
//   - UNREADABLE is a file present that will not decrypt under the model DEK.
//     Also not backfilled, and it means something other than this scheme wrote it.
//
// A run that finds nothing prints those zeros against the number of hashes it
// examined, so "nothing to do" cannot be read off a report that examined nothing.
func runBackfillModelBlobs(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("backfill-model-blobs", flag.ContinueOnError)
	apply := fs.Bool("apply", false, "write the missing app.blobs rows (default: report only)")
	verbose := fs.Bool("verbose", false, "list every hash and its outcome, not just the counts")
	userFlag := fs.String("user", "", "restrict to one user id (default: every active user)")
	if err := fs.Parse(args); err != nil {
		return err
	}

	d, err := openDeps(ctx)
	if err != nil {
		return err
	}
	defer d.close()

	blobs, err := openBlobStore(ctx, d)
	if err != nil {
		return err
	}

	users, err := d.store.ListActiveUserIDs(ctx)
	if err != nil {
		return fmt.Errorf("backfill-model-blobs: list users: %w", err)
	}
	if *userFlag != "" {
		u, perr := ulid.Parse(*userFlag)
		if perr != nil {
			return fmt.Errorf("backfill-model-blobs: parse -user: %w", perr)
		}
		users = []ulid.ULID{u}
	}

	master, err := d.keys.MasterKey(ctx)
	if err != nil {
		return fmt.Errorf("backfill-model-blobs: master key: %w", err)
	}
	defer crypto.Zero(master)

	fmt.Printf("%-28s %7s %7s %8s %8s %8s %10s %9s\n",
		"USER", "MODELS", "HASHES", "HAVE-ROW", "MISSING", "NO-FILE", "UNREADABLE", "WRITTEN")

	var tot totals
	for _, uid := range users {
		u, details, err := backfillOneUser(ctx, d.store, blobs, master, uid, *apply, *verbose)
		if err != nil {
			return err
		}
		fmt.Print(userBlock(uid.String(), u, details))
		tot.add(u)
	}

	fmt.Printf("%-28s %7d %7d %8d %8d %8d %10d %9d\n",
		"TOTAL", tot.models, tot.hashes, tot.haveRow, tot.missing, tot.noFile, tot.unreadable, tot.written)

	// The one line that says what this run DID, in the tense it did it. A report
	// that reads like an apply is how a dry run gets mistaken for the fix.
	switch {
	case !*apply && tot.missing > 0:
		fmt.Printf("\nREPORT ONLY — nothing was written. Re-run with --apply to insert %d row(s).\n", tot.missing)
	case !*apply:
		fmt.Printf("\nREPORT ONLY — nothing was written, and nothing is missing.\n")
	default:
		fmt.Printf("\nAPPLIED — %d app.blobs row(s) written.\n", tot.written)
	}
	if tot.noFile > 0 || tot.unreadable > 0 {
		fmt.Printf("⚠ %d artifact(s) have no file and %d will not decrypt; NO row was written for "+
			"either. A model row pointing at absent or unreadable bytes cannot load — that is a "+
			"separate and worse problem than the missing rows this command fixes.\n",
			tot.noFile, tot.unreadable)
	}
	return nil
}

// userBlock renders ONE user's summary row followed by that user's detail lines.
//
// ⚠ THE ORDER IS THE POINT, AND IT WAS WRONG IN v1.34.7. The details were
// printed while the user was being processed and the summary row only after, so
// on a multi-user instance every user's hashes appeared ABOVE their own row —
// i.e. directly beneath the PREVIOUS user's. Read against the operator's own
// instance, five model artifacts belonging to the second user sat under the
// first user's row, which showed zero models. Nothing was miscounted; the
// attribution was simply unreadable, which in a report whose entire job is to
// say which bytes belong to whom is not a cosmetic problem.
//
// This is also the order `blob-report` already uses. Rendering the block as one
// string rather than printing in two places is what lets a test hold it to that
// without capturing stdout.
func userBlock(uid string, u userTotals, details []string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%-28s %7d %7d %8d %8d %8d %10d %9d\n",
		uid, u.models, u.hashes, u.haveRow, u.missing, u.noFile, u.unreadable, u.written)
	for _, d := range details {
		b.WriteString(d)
		b.WriteString("\n")
	}
	return b.String()
}

// userTotals is one user's per-signal outcome. Fields, not a map, so a new
// outcome cannot be added without a column being added to print it.
type userTotals struct {
	models     int // app.ml_models rows
	hashes     int // DISTINCT artifact hashes (byte-identical snapshots share one)
	haveRow    int // already accounted in app.blobs
	missing    int // no row, file present and readable — backfillable
	noFile     int // no row, and no file on disk either
	unreadable int // no row, file present, will not decrypt
	written    int // rows actually inserted (0 unless --apply)
}

type totals = userTotals

func (t *totals) add(u userTotals) {
	t.models += u.models
	t.hashes += u.hashes
	t.haveRow += u.haveRow
	t.missing += u.missing
	t.noFile += u.noFile
	t.unreadable += u.unreadable
	t.written += u.written
}

// backfillOneUser classifies (and, with apply, repairs) one user's artifacts. It
// RETURNS its detail lines rather than printing them, so the caller can put them
// under the right user's row — see userBlock.
//
// The plaintext size is MEASURED by streaming the artifact back out through the
// blob store and counting, not inferred: app.blobs.size_plain means the
// plaintext length, and the ciphertext length on disk is a different number. The
// read doubles as the readability check — a row is written only for bytes this
// command has just proved it can open, so the backfill cannot manufacture a row
// that promises a model which is not there.
func backfillOneUser(
	ctx context.Context,
	st *store.Store,
	blobs blob.BlobStore,
	master []byte,
	uid ulid.ULID,
	apply, verbose bool,
) (userTotals, []string, error) {
	var (
		u       userTotals
		details []string
	)
	detail := func(format string, args ...any) {
		if verbose {
			details = append(details, fmt.Sprintf(format, args...))
		}
	}

	var refs []store.ModelArtifactRef
	if err := st.UserTx(ctx, uid, func(tx *store.UserTx) error {
		var e error
		refs, e = tx.ModelArtifactRefs(ctx)
		return e
	}); err != nil {
		return u, nil, fmt.Errorf("backfill-model-blobs: artifact refs for %s: %w", uid, err)
	}
	u.hashes = len(refs)
	for _, r := range refs {
		u.models += r.Versions
	}
	if len(refs) == 0 {
		return u, nil, nil
	}

	// The same derivation the registry uses (ADR-013): HKDF(master, salt=userID,
	// info=ModelDEKInfoV1). Taken from ml's exported label rather than re-spelled
	// here, so a future scheme change cannot leave this command deriving the old
	// key and reporting every artifact as unreadable.
	dek, err := crypto.DeriveKey(master, uid[:], ml.ModelDEKInfoV1)
	if err != nil {
		return u, nil, fmt.Errorf("backfill-model-blobs: derive model dek for %s: %w", uid, err)
	}
	defer crypto.Zero(dek)

	type pending struct {
		meta store.BlobMeta
		hex  string
	}
	var toWrite []pending

	for _, r := range refs {
		hx := hex.EncodeToString(r.ContentHash)
		if r.HasBlobRow {
			u.haveRow++
			detail("  have-row   %s (%d version(s))", hx, r.Versions)
			continue
		}
		var ch blob.ContentHash
		if len(r.ContentHash) != len(ch) {
			return u, nil, fmt.Errorf("backfill-model-blobs: artifact hash %s is %d bytes, want %d",
				hx, len(r.ContentHash), len(ch))
		}
		copy(ch[:], r.ContentHash)

		storedSize, serr := blobs.StoredSize(ctx, uid, ch)
		if serr != nil {
			u.noFile++
			detail("  NO-FILE    %s (%d version(s)) — %v", hx, r.Versions, serr)
			continue
		}
		plainSize, rerr := plaintextSize(ctx, blobs, uid, dek, ch)
		if rerr != nil {
			u.unreadable++
			detail("  UNREADABLE %s (%d version(s)) — %v", hx, r.Versions, rerr)
			continue
		}
		u.missing++
		detail("  missing    %s (%d version(s), %d plain / %d stored)",
			hx, r.Versions, plainSize, storedSize)
		toWrite = append(toWrite, pending{
			meta: store.BlobMeta{
				ContentHash: r.ContentHash,
				SizePlain:   plainSize,
				SizeStored:  storedSize,
				DEKID:       ml.ModelBlobSchemeV1,
				EnvelopeVer: 1,
			},
			hex: hx,
		})
	}

	if !apply || len(toWrite) == 0 {
		return u, details, nil
	}
	// One transaction for this user's rows: they are the same repair, and a
	// half-applied backfill would leave the next run reporting a different
	// number for no reason anyone could reconstruct.
	if err := st.UserTx(ctx, uid, func(tx *store.UserTx) error {
		for _, p := range toWrite {
			if e := tx.InsertBlobMeta(ctx, p.meta); e != nil {
				return fmt.Errorf("insert blob row for %s: %w", p.hex, e)
			}
		}
		return nil
	}); err != nil {
		return u, details, fmt.Errorf("backfill-model-blobs: apply for %s: %w", uid, err)
	}
	u.written = len(toWrite)
	return u, details, nil
}

// plaintextSize opens an artifact and counts the bytes that come out. It is the
// measurement AND the readability proof; see backfillOneUser.
func plaintextSize(
	ctx context.Context,
	blobs blob.BlobStore,
	uid ulid.ULID,
	dek []byte,
	ch blob.ContentHash,
) (int64, error) {
	rc, err := blobs.Get(ctx, uid, dek, ch)
	if err != nil {
		return 0, err
	}
	defer func() { _ = rc.Close() }()
	n, err := io.Copy(io.Discard, rc)
	if err != nil {
		return 0, err
	}
	return n, nil
}
