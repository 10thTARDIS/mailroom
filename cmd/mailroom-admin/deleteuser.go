// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/blob"
	"mailroom/internal/config"
	"mailroom/internal/store"
)

// `mailroom-admin delete-user` — what a hard delete WOULD remove (#54, spec §10).
//
// REPORT-ONLY BY DEFAULT. --apply performs the destructive half, and ALWAYS
// re-runs checkDeletePolicy as its FIRST act, before touching the search
// index, the blob prefix or the database — refusing there, loudly and with
// nothing yet deleted, is what stands in for the "it ran and changed nothing"
// failure this operation cannot afford to have silently. See runApply and
// applyDelete below for the order the destructive half actually follows.
//
// ⚠ AND IT IS NOT HYPOTHETICAL — A DELETE WRITTEN TODAY WOULD DO EXACTLY THAT.
// Measured on the deployed instance and reproduced on a throwaway PostgreSQL 18:
//
//	users_owner               = mailroom_owner   (NOT postgres)
//	rls_forced                = t
//	delete_policies_for_owner = 0
//
//	SET ROLE mailroom_owner;
//	DELETE FROM app.users WHERE id='…';
//	DELETE 0                  <- no error
//	rows_remaining = 1        <- the user is still there
//
// The reasoning that says "the missing DELETE grant would catch it loudly" is
// wrong: app.users is OWNED by mailroom_owner, and an owner holds DELETE
// implicitly, which is why information_schema reports a grant schema.sql never
// writes. Nothing loud stands in the way — only the silent RLS filter.
//
// ⚠ IT DOES REPRODUCE IN THE INTEGRATION SUITE, and an earlier version of this
// comment claimed it did not. scripts/schema-apply.sh applies the schema as
// postgres — that job is blind — but scripts/integration-test.sh applies it as
// mailroom_owner deliberately, so the store suite sees the deployed ownership.
// internal/store/delete_policy_integration_test.go therefore measures the silent
// no-op, and the fix for it, on app.users itself rather than on a model of it.
//
// checkDeletePolicy below re-derives that at run time rather than trusting this
// comment, and the report refuses to describe itself as ready while it holds.
//
// # Why the table set comes from the catalog
//
// store.UserRowCensus reads pg_catalog, so the 43rd table is covered the day it
// is created. A hardcoded list is a promise someone will remember; #316 is what
// that promise is worth — one producer forgot one INSERT for the life of the
// project, and a row-driven sweep would have walked past every model a user
// owned while reporting success.
//
// # Why the blob half walks the PREFIX and not app.blobs
//
// Same reason. A row-driven walk is only as complete as every writer's
// discipline; the derived path prefix does not care who forgot.
func runDeleteUser(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("delete-user", flag.ContinueOnError)
	userFlag := fs.String("user-id", "", "the user to report on (required)")
	apply := fs.Bool("apply", false,
		"DESTROY this user and everything of theirs. Irreversible.")
	confirm := fs.String("confirm-user-id", "",
		"with --apply: the user id again, typed out. The two must match.")
	actor := fs.String("actor", "", "with --apply: who is doing this, for the audit tombstone")
	acceptKeyLoss := fs.Bool("accept-key-loss", false,
		"with --apply: proceed even though a private key will be destroyed that "+
			"exists nowhere else. Read what the report says about it first.")
	if err := fs.Parse(args); err != nil {
		return err
	}
	userID, err := parseUserID(*userFlag)
	if err != nil {
		return err
	}

	d, err := openDeps(ctx)
	if err != nil {
		return err
	}
	defer d.close()

	return deleteUserCore(ctx, d, userID, *apply, *confirm, *actor, *acceptKeyLoss)
}

// deleteUserCore is runDeleteUser after flags are parsed and deps are
// resolved: the report, then (with --apply) the destructive half. Split out
// as its own function so an integration test can drive it against a fixture
// *deps built directly against the test database — the same seam
// openStoredAccounts (accounts_report.go) and this package's other
// integration tests use, rather than threading env vars through config.Load
// just to reach this logic.
func deleteUserCore(ctx context.Context, d *deps, userID ulid.ULID, apply bool, confirm, actor string, acceptKeyLoss bool) error {
	var (
		exists bool
		census store.UserCensus
	)
	if err := d.store.UserTx(ctx, userID, func(tx *store.UserTx) error {
		var e error
		if exists, e = tx.SelfExists(ctx); e != nil {
			return e
		}
		census, e = tx.UserRowCensus(ctx)
		return e
	}); err != nil {
		return fmt.Errorf("delete-user: census: %w", err)
	}

	rp("user                         : %s\n", userID)
	if !exists {
		// ⚠ STOP. Every count below would be zero, and "nothing to delete" is
		// the most confident possible way to be wrong about a mistyped id.
		rpln("")
		rpln("⚠ NO SUCH USER — or the row is not visible to this connection.")
		rpln("  Every table below would read zero for ANY id, because an RLS-scoped")
		rpln("  transaction for a user that does not exist is indistinguishable from")
		rpln("  one for a user who owns nothing. Refusing to print a report that")
		rpln("  would read as \"there is nothing here to delete\".")
		return fmt.Errorf("delete-user: user %s not found", userID)
	}

	rpln("")
	rpln("-- rows, per table, discovered from pg_catalog ------------------------")
	rp("tables with a user_id        : %d\n", len(census.Tables))
	rp("rows owned by this user      : %d\n", census.TotalRows)
	if census.Skipped > 0 {
		rp("⚠ tables that could NOT be read: %d — a table this report cannot read is\n", census.Skipped)
		rpln("  a table a delete cannot verify. They are listed below, not counted as zero.")
	}
	nonEmpty := 0
	for _, t := range census.Tables {
		if t.SkipReason != "" {
			rp("  %-34s UNREADABLE: %s\n", t.Table, t.SkipReason)
			continue
		}
		if t.Rows == 0 {
			continue
		}
		nonEmpty++
		rp("  %-34s %8d\n", t.Table, t.Rows)
	}
	rp("  (%d of %d tables hold rows for this user; the rest are empty)\n",
		nonEmpty, len(census.Tables))

	rpln("")
	rpln("-- the row a delete actually targets -----------------------------------")
	switch {
	case len(census.RootCandidates) == 0:
		// ⚠ Not "no root, therefore nothing to do". Every user_id above is a
		// foreign key to something; finding nothing means the query failed to
		// look, and a delete built on that would cascade from nowhere.
		rpln("  ⚠ NO ROOT TABLE FOUND — no single-column user_id foreign key in `app`")
		rpln("    resolves to a target. That is a broken query or a schema this report")
		rpln("    does not understand, NOT a schema with nothing to delete.")
	case census.Root == "":
		// ⚠ Reported, never resolved by picking one. See UserRowCensus: a bare
		// LIMIT 1 here already named the wrong table once.
		rp("  ⚠ AMBIGUOUS ROOT — %d tables are nominated, so this report will not\n",
			len(census.RootCandidates))
		rpln("    guess which one a delete should target:")
		for _, c := range census.RootCandidates {
			rp("      %s\n", c)
		}
	case census.RootSkipReason != "":
		// ⚠ NOT MEASURED, said out loud. A zero here would read as "this user's
		// own row is already gone" rather than "the count failed".
		rp("  app.%s — ⚠ COULD NOT COUNT: %s\n", census.Root, census.RootSkipReason)
		rpln("    NOT MEASURED. That is not the same as \"the row is not there\".")
	default:
		rp("  app.%s, %d row(s) visible to this user\n", census.Root, census.RootRows)
		rpln("  Everything counted above hangs off this row by ON DELETE CASCADE, so a")
		rpln("  delete removes one row and the database removes the rest.")
		rp("  (app.%s has no user_id column of its own — it is scoped by its id. It is\n", census.Root)
		rpln("   named here, and NOT under the uncovered heading below, because listing")
		rpln("   the delete's own target as \"data no delete could find\" would be the")
		rpln("   exact opposite of true.)")
	}

	if len(census.Uncovered) > 0 {
		rpln("")
		rpln("-- tables with NO user_id and NOT the root -----------------------------")
		rpln("  These are not swept by a user_id-driven delete. That is a fact about")
		rpln("  the schema, not an omission by this report — but a new table here")
		rpln("  would hold data no delete could find, so it is listed every run.")
		for _, t := range census.Uncovered {
			rp("  %s\n", t)
		}
	} else {
		rpln("")
		rpln("-- tables with NO user_id and NOT the root -----------------------------")
		rpln("  none — every table in `app` is either user-scoped or the root itself.")
	}

	keys, err := reportKeys(ctx, d, userID)
	if err != nil {
		return err
	}
	if err := reportBlobs(ctx, d, userID); err != nil {
		return err
	}
	reportSearchIndex(userID)
	policyErr := checkDeletePolicy(ctx, d, userID)

	if apply {
		// #625 §4: this MUST be the first thing --apply does. Before this fix
		// --apply deleted the search index and the blob prefix, then ran the
		// database transaction — on a database missing the owner's delete
		// policy that transaction deletes zero rows, so the user's files were
		// already gone by the time anything noticed the row-delete was a no-op.
		if policyErr != nil {
			rpln("")
			rpln("-- REFUSING --apply -----------------------------------------------------")
			rp("  %v\n", policyErr)
			rpln("  Nothing was touched: no search index, no blob file, no database row.")
			return fmt.Errorf("delete-user: refusing --apply: %w", policyErr)
		}
		return runApply(ctx, d, userID, confirm, actor, acceptKeyLoss, keys)
	}

	rpln("")
	rpln("-- what is NOT here ---------------------------------------------------")
	rpln("  Nothing was destroyed: --apply was not given.")
	rpln("  ⚠ Backups are the other half and are outside this process: the restore")
	rpln("    unit is pg_dump plus the blob directory verbatim, so a hard delete")
	rpln("    means gone from the live system NOW and gone from backups only when")
	rpln("    the last pre-deletion artifact ages out. Retention is currently")
	rpln("    unbounded, which makes the honest answer \"indefinitely\".")
	return nil
}

// reportKeys reports the user's PGP keys BY ORIGIN — the one fact that decides
// whether deleting this user destroys something irreplaceable (#54).
//
// ⚠ THE THREE CASES ARE NOT INTERCHANGEABLE, AND THE DANGEROUS MISTAKE IS
// TREATING "UNKNOWN" AS "IMPORTED".
//
//   - imported  — the user handed us a key they still hold. Deleting our copy
//     costs them nothing.
//   - generated — minted in the crypto process; this is the ONLY copy of the
//     private half. Deleting it is irreversible, and it retroactively makes
//     every PGP-encrypted message the user already exported unreadable. An
//     export taken months ago turns to garbage.
//   - unknown   — the row predates migration 00039 and nothing, not even
//     app.audit_log, can say which it was. This is the CAUTIOUS case: it must
//     be reported separately and --apply must refuse rather than assume.
//
// The count is stated per case, never as one total, because a single number
// cannot distinguish "nothing irreplaceable here" from "we did not look".
func reportKeys(ctx context.Context, d *deps, userID ulid.ULID) ([]store.UserKey, error) {
	rpln("")
	rpln("-- PGP keys, by where the private half came from -----------------------")

	var keys []store.UserKey
	if err := d.store.UserTx(ctx, userID, func(tx *store.UserTx) error {
		var e error
		keys, e = tx.ListUserKeys(ctx)
		return e
	}); err != nil {
		return nil, fmt.Errorf("delete-user: list keys: %w", err)
	}

	var generated, imported, unknown, publicOnly int
	for _, k := range keys {
		if len(k.PrivateKeyCT) == 0 {
			// No private half to destroy; nothing to weigh.
			publicOnly++
			continue
		}
		switch k.Origin {
		case store.KeyGenerated:
			generated++
		case store.KeyImported:
			imported++
		default:
			unknown++
		}
	}

	rp("keys held                    : %d\n", len(keys))
	rp("  public-only (nothing to lose): %d\n", publicOnly)
	rp("  imported (user holds it too) : %d\n", imported)
	rp("  GENERATED (only copy)        : %d\n", generated)
	rp("  origin NOT RECORDED          : %d\n", unknown)

	if generated == 0 && unknown == 0 {
		if len(keys) > 0 {
			rpln("  Nothing irreplaceable: every private key here exists elsewhere too.")
		}
		return keys, nil
	}

	rpln("")
	if generated > 0 {
		rpln("  ⚠ A DELETE WOULD DESTROY THE ONLY COPY OF A PRIVATE KEY.")
		for _, k := range keys {
			if len(k.PrivateKeyCT) > 0 && k.Origin == store.KeyGenerated {
				rp("      %s  (generated here)\n", k.Fingerprint)
			}
		}
		rpln("    Mail encrypted to it becomes permanently unreadable — including mail")
		rpln("    ALREADY EXPORTED, because an export carries the ciphertext and not the")
		rpln("    key. spec §9 lists an `export-wrapped` operation for exactly this and")
		rpln("    ADR-024 defers building it \"until something needs it\". This is that.")
	}
	if unknown > 0 {
		rpln("")
		rpln("  ⚠ ORIGIN NOT RECORDED for a key with private material:")
		for _, k := range keys {
			if len(k.PrivateKeyCT) > 0 && k.Origin == store.KeyOriginUnrecorded {
				rp("      %s  (predates migration 00039)\n", k.Fingerprint)
			}
		}
		rpln("    This is NOT the same as \"imported\". Nothing recorded which it was, so")
		rpln("    whether a delete destroys the only copy is UNKNOWN, and unknown is the")
		rpln("    cautious case. --apply must refuse these rather than guess.")
	}
	return keys, nil
}

// reportBlobs walks the user's derived on-disk prefix.
func reportBlobs(ctx context.Context, d *deps, userID ulid.ULID) error {
	rpln("")
	rpln("-- blobs, walked by DERIVED PREFIX (not by app.blobs rows) -------------")
	blobs, err := openBlobStore(ctx, d)
	if err != nil {
		return fmt.Errorf("delete-user: open blob store: %w", err)
	}
	enum, ok := blobs.(*blob.FSBlobStore)
	if !ok {
		// ⚠ Refuse rather than skip. A report that silently omitted the on-disk
		// half would print a clean row census and read as "this is everything",
		// which is the failure this whole command exists to prevent.
		return fmt.Errorf("delete-user: this blob backend (%T) cannot enumerate, so the "+
			"on-disk half cannot be computed. Refusing to print a half-report that "+
			"would read as a complete one", blobs)
	}
	hashes, unparsed, err := enum.UserHashes(ctx, userID)
	if err != nil {
		return fmt.Errorf("delete-user: walk blobs: %w", err)
	}
	rp("derived prefix               : %s\n", enum.UserPrefix(userID))
	rp("blob files under it          : %d\n", len(hashes))
	if len(unparsed) > 0 {
		// ⚠ Named, not merely counted. These are files a HASH-driven sweep would
		// walk straight past — the exact failure the prefix walk exists to avoid —
		// so a delete has to be told they are there.
		rp("⚠ unparsable filenames        : %d — a hash-driven sweep would miss these\n",
			len(unparsed))
		for _, n := range unparsed {
			rp("    %s\n", n)
		}
	}
	return nil
}

// reportSearchIndex reports the per-user index directory.
//
// ⚠ NOTHING TEARS THIS DOWN TODAY, and it is not a cache of harmless metadata:
// the index holds full body text, INCLUDING the plaintext of PGP-encrypted
// mail. An account-level delete also cascades change_log away before the
// indexer could consume the destroys, so the change feed can never clean it —
// the directory has to be removed directly.
func reportSearchIndex(userID ulid.ULID) {
	rpln("")
	rpln("-- search index -------------------------------------------------------")
	root := os.Getenv(config.EnvSearchRoot)
	if root == "" {
		rpln("  search root not configured in this environment (MAILROOM_SEARCH_ROOT unset)")
		rpln("  ⚠ NOT MEASURED — that is not the same as \"there is no index\". Run this")
		rpln("    where the server runs to get a real answer.")
		return
	}
	dir := filepath.Join(root, userID.String())
	size, files, err := dirSize(dir)
	switch {
	case os.IsNotExist(err):
		rp("  %s\n", dir)
		rpln("  no index directory for this user")
	case err != nil:
		rp("  %s\n", dir)
		rp("  ⚠ could not measure: %v\n", err)
	default:
		rp("  %s\n", dir)
		rp("  %d files, %s\n", files, humanBytes(size))
		rpln("  ⚠ This holds body text, including the plaintext of PGP-encrypted mail,")
		rpln("    and no code path removes it. A delete must unlink it directly.")
	}
}

// dirSize totals a directory tree.
func dirSize(dir string) (int64, int, error) {
	if _, err := os.Stat(dir); err != nil {
		return 0, 0, err
	}
	var total int64
	var files int
	err := filepath.Walk(dir, func(_ string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() {
			total += info.Size()
			files++
		}
		return nil
	})
	return total, files, err
}

// checkDeletePolicy re-derives, at run time, whether a hard delete could
// actually remove the app.users row — rather than trusting this file's comment.
// It prints the report either way, and returns a non-nil error describing why
// --apply must refuse when the answer is anything but a clean "yes": runDeleteUser
// treats that error as a gate, checked BEFORE --apply touches the search index,
// the blob prefix, or the database (#625 §4) — a --apply that deleted files
// first and only then discovered the row-delete would no-op left a user's mail
// gone from disk with the account row still standing.
//
// ⚠ THE ANSWER IS READ FROM pg_catalog, NOT ASSUMED. The whole point is that the
// dangerous state is invisible from schema.sql alone (ownership supplies the
// privilege) and does not reproduce in CI (where postgres owns the tables). A
// check that only re-stated the source would be describing the wrong database.
func checkDeletePolicy(ctx context.Context, d *deps, userID ulid.ULID) error {
	rpln("")
	rpln("-- can a delete actually remove the app.users row? ---------------------")
	var (
		owner    string
		forced   bool
		policies int
	)
	err := d.store.UserTx(ctx, userID, func(tx *store.UserTx) error {
		var e error
		owner, forced, policies, e = tx.DeletePolicyStatus(ctx)
		return e
	})
	if err != nil {
		rp("  ⚠ could not determine: %v\n", err)
		rpln("  NOT MEASURED. Absent is not \"fine\".")
		return fmt.Errorf("delete policy status could not be determined (NOT MEASURED, which is not \"fine\"): %w", err)
	}
	rp("app.users owner              : %s\n", owner)
	rp("FORCE ROW LEVEL SECURITY     : %v\n", forced)
	rp("DELETE policies for the owner : %d\n", policies)
	if forced && policies == 0 {
		rpln("")
		rpln("  ⚠ A HARD DELETE WOULD SILENTLY DO NOTHING.")
		rpln("    The owner holds DELETE implicitly, so there is no permission error to")
		rpln("    catch it — FORCE RLS filters the statement to zero rows and DELETE")
		rpln("    reports success. --apply REFUSES until app.users has a FOR DELETE")
		rpln("    policy for its owner (migration 00040's users_owner_delete).")
		return errors.New("a hard delete would silently remove ZERO rows: app.users is FORCE RLS " +
			"with no DELETE policy for its owner")
	}
	rpln("  A delete would reach the row.")
	return nil
}

// --- the destructive half -----------------------------------------------------

// applyDelete performs the hard delete, in the order #54 specifies.
//
// ⚠ THE ORDER IS FILES FIRST, DATABASE LAST, AND IT IS NOT ARBITRARY. A crash
// part-way through must leave DETECTABLE residue rather than invisible residue.
// Deleting the rows first would leave blob files and a search index that no
// query can find — orphans nothing reports. Deleting the files first leaves rows
// pointing at missing bytes, which `blob-report` and this command's own re-run
// both surface loudly.
//
//	search index dir  →  blob prefix  →  one transaction (cascade + tombstone)
func applyDelete(ctx context.Context, d *deps, userID ulid.ULID, actor string) error {
	rpln("")
	rpln("== APPLYING ==========================================================")

	// 1. The search index. Nothing else ever removes it, and it holds body text
	//    including the plaintext of PGP-encrypted mail.
	if root := os.Getenv(config.EnvSearchRoot); root != "" {
		dir := filepath.Join(root, userID.String())
		switch _, err := os.Stat(dir); {
		case os.IsNotExist(err):
			rp("search index                 : nothing at %s\n", dir)
		case err != nil:
			return fmt.Errorf("delete-user: stat search index %s: %w", dir, err)
		default:
			if err := os.RemoveAll(dir); err != nil {
				return fmt.Errorf("delete-user: remove search index %s: %w", dir, err)
			}
			// ⚠ Verified, not assumed. RemoveAll returns nil for a path it never
			// touched, so "no error" is not evidence the directory is gone.
			if _, err := os.Stat(dir); !os.IsNotExist(err) {
				return fmt.Errorf("delete-user: search index %s still exists after removal", dir)
			}
			rp("search index                 : removed %s\n", dir)
		}
	} else {
		// ⚠ NOT MEASURED is not "there is nothing there". Refuse rather than
		// leave an index behind while reporting success.
		return fmt.Errorf("delete-user: %s is unset, so the search index cannot be "+
			"removed. Run this where the server runs; refusing to delete a user while "+
			"leaving their indexed body text on disk", config.EnvSearchRoot)
	}

	// 2. The blobs, by DERIVED PREFIX rather than by app.blobs rows — the same
	//    reason the report walks it that way: a row-driven sweep is only as
	//    complete as every producer's discipline, and one producer forgot for
	//    the life of the project (#316).
	blobs, err := openBlobStore(ctx, d)
	if err != nil {
		return fmt.Errorf("delete-user: open blob store: %w", err)
	}
	enum, ok := blobs.(*blob.FSBlobStore)
	if !ok {
		return fmt.Errorf("delete-user: this blob backend (%T) cannot enumerate, so the "+
			"on-disk half cannot be removed. Refusing to delete the rows and strand the "+
			"bytes", blobs)
	}
	removed, err := enum.RemoveUserPrefix(ctx, userID)
	if err != nil {
		return fmt.Errorf("delete-user: remove blob prefix: %w", err)
	}
	rp("blob files                   : removed %d under %s\n", removed, enum.UserPrefix(userID))

	// 3. The rows, in one transaction, with the tombstone.
	if err := d.store.SystemTx(ctx, func(tx *store.SystemTx) error {
		return tx.HardDeleteUser(ctx, userID, actor)
	}); err != nil {
		if errors.Is(err, store.ErrHardDeleteNoRows) {
			return fmt.Errorf("delete-user: the DELETE removed NO ROWS. The files above "+
				"are already gone, so this instance now holds rows pointing at missing "+
				"bytes — which is detectable residue by design. Check that migration "+
				"00040's users_owner_delete policy is present: %w", err)
		}
		return fmt.Errorf("delete-user: %w", err)
	}
	rpln("database rows                : deleted (cascade), tombstone written")
	return nil
}

// verifyGone re-reads the world and reports residue.
//
// ⚠ IT IS A SEPARATE PASS, NOT A CONTINUATION OF THE DELETE. The delete's own
// return value says what it believes it did; this says what is actually there.
// #54 asks for exactly this, and for a NON-ZERO EXIT on residue — an incomplete
// deletion must not be mistaken for a complete one by a script.
func verifyGone(ctx context.Context, d *deps, userID ulid.ULID) error {
	rpln("")
	rpln("-- verification (a fresh look, not the delete's own opinion) -----------")

	var residue []string

	var exists bool
	if err := d.store.UserTx(ctx, userID, func(tx *store.UserTx) error {
		var e error
		exists, e = tx.SelfExists(ctx)
		return e
	}); err != nil {
		return fmt.Errorf("delete-user: verify: %w", err)
	}
	if exists {
		residue = append(residue, "the app.users row is still present")
	}
	rp("app.users row                : %s\n", presentOrGone(exists))

	blobs, err := openBlobStore(ctx, d)
	if err != nil {
		return fmt.Errorf("delete-user: verify: open blob store: %w", err)
	}
	if enum, ok := blobs.(*blob.FSBlobStore); ok {
		hashes, unparsed, err := enum.UserHashes(ctx, userID)
		if err != nil {
			return fmt.Errorf("delete-user: verify: walk blobs: %w", err)
		}
		if n := len(hashes) + len(unparsed); n > 0 {
			residue = append(residue, fmt.Sprintf("%d blob file(s) remain under the derived prefix", n))
		}
		rp("blob files under the prefix  : %d\n", len(hashes)+len(unparsed))
	}

	if root := os.Getenv(config.EnvSearchRoot); root != "" {
		dir := filepath.Join(root, userID.String())
		_, err := os.Stat(dir)
		if err == nil {
			residue = append(residue, "the search index directory still exists")
		}
		rp("search index directory       : %s\n", presentOrGone(err == nil))
	}

	if len(residue) > 0 {
		rpln("")
		rpln("⚠ RESIDUE REMAINS:")
		for _, r := range residue {
			rp("    %s\n", r)
		}
		return fmt.Errorf("delete-user: %d kind(s) of residue remain after the delete", len(residue))
	}
	rpln("")
	rpln("  nothing of this user remains in the live system.")
	rpln("  ⚠ BACKUPS ARE THE OTHER HALF and are outside this process: the restore")
	rpln("    unit is pg_dump plus the blob directory verbatim, so this user returns")
	rpln("    from any restore taken before now. Re-run this delete after a restore,")
	rpln("    and note retention is currently unbounded.")
	return nil
}

func presentOrGone(present bool) string {
	if present {
		return "STILL PRESENT"
	}
	return "gone"
}

// runApply is the gate in front of applyDelete. Every refusal here is cheaper
// than the thing it prevents, because the thing it prevents cannot be undone.
func runApply(ctx context.Context, d *deps, userID ulid.ULID, confirm, actor string,
	acceptKeyLoss bool, keys []store.UserKey,
) error {
	// 1. The id, typed twice. A mistyped --user-id that happens to name a real
	//    user is the failure mode with no recovery, and re-typing is the cheapest
	//    check that catches it.
	if confirm != userID.String() {
		return fmt.Errorf("delete-user: --confirm-user-id does not match --user-id. "+
			"Type the id again exactly: %s", userID)
	}
	if actor == "" {
		return errors.New("delete-user: --actor is required with --apply; the audit " +
			"tombstone has to say who did this")
	}

	// 2. ⚠ THE KEY GATE, and the reason export-wrapped had to exist first.
	//    Destroying a GENERATED key destroys the only copy of the private half,
	//    and it is retroactive: mail the user exported months ago becomes
	//    permanently unreadable, because an export carries ciphertext and not the
	//    key. An UNRECORDED origin is the cautious case, not the safe one —
	//    nothing says whether a copy exists elsewhere.
	var atRisk []store.UserKey
	for _, k := range keys {
		if len(k.PrivateKeyCT) == 0 {
			continue
		}
		if k.Origin == store.KeyGenerated || k.Origin == store.KeyOriginUnrecorded {
			atRisk = append(atRisk, k)
		}
	}
	if len(atRisk) > 0 && !acceptKeyLoss {
		rpln("")
		rpln("⚠ REFUSING: this delete would destroy a private key that may exist nowhere else.")
		for _, k := range atRisk {
			origin := string(k.Origin)
			if origin == "" {
				origin = "NOT RECORDED"
			}
			rp("    %s  (origin: %s)\n", k.Fingerprint, origin)
		}
		rpln("")
		rpln("  Export it first, so the user keeps what only this system holds:")
		rp("    mailroom-admin pgp-export-wrapped --user-id %s --key-id <id> \\n", userID)
		rpln("      --passphrase-file <file> --out <file>")
		rpln("")
		rpln("  Then re-run with --accept-key-loss if the key is genuinely disposable.")
		return errors.New("delete-user: refusing to destroy an irreplaceable private key")
	}
	if len(atRisk) > 0 {
		rpln("")
		rp("⚠ PROCEEDING WITH --accept-key-loss: %d private key(s) will be destroyed.\n", len(atRisk))
	}

	if err := applyDelete(ctx, d, userID, actor); err != nil {
		return err
	}
	return verifyGone(ctx, d, userID)
}
