// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/consolidate"
	"mailroom/internal/store"
	"mailroom/internal/sync"
	"mailroom/internal/syncrunner"
)

// consolidateFoldersHelp is the command's --help preamble. It states the three
// things an operator must know before --apply: the report comes first, the moved
// mail is downloaded again, and the provider-side backup is the only undo.
const consolidateFoldersHelp = `usage: mailroom-admin consolidate-folders --user-id U --account-id A
         --from PATTERN --into FOLDER [--delete-empty PATTERN]... [--max-folders N]
         [--apply [--despite-ingest-only]]

Reshape the account's folder tree ON THE PROVIDER (#647): move every message in
each folder matching --from into --into, then delete the folders matching
--delete-empty once they are empty. Patterns are IMAP LIST patterns: '*' matches
anything including the hierarchy delimiter, '%' anything except it. So
--from 'INBOX/*' is every folder below INBOX; '%' stops at one level.

REPORT FIRST. Without --apply this changes nothing anywhere: it prints, per
folder, the server's STATUS (MESSAGES, UIDNEXT), the number Mailroom holds for
it, and what --apply would do — move N, delete, or REFUSE with the reason — with
totals per category. Read it, then add --apply.

--apply works one folder at a time. Each move is a single 'UID MOVE 1:*',
checked by STATUS before and after: the source must read 0 and the destination
exactly what it held plus what moved, or the run STOPS and says where. A server
without MOVE is refused; it never falls back to COPY + EXPUNGE. Deletions run
after every move, leaves before parents, and each re-reads STATUS immediately
before DELETE and deletes only at 0. Re-running continues a stopped or bounded
run: every decision is re-read from the server.

Never touched: INBOX, any folder with a SPECIAL-USE attribute (Sent, Drafts,
Trash, Junk, Archive, All, Flagged), and any folder Mailroom maps to a role.

It writes NOTHING to Mailroom's database. Mailroom's sync sees the moves like any
other client's: moved mail appears as NEW mail in the destination and its body
is DOWNLOADED AGAIN, then deduplicated by content hash onto the message Mailroom
already has. Expect the next sync passes to fetch as many bodies as you moved.

TAKE A FRESH PROVIDER-SIDE BACKUP FIRST. Mailroom's own backup is of Mailroom's
copy, not of the provider's folder tree, and it cannot undo a DELETE.

On an ingest-only account (writeback_enabled=false, or
MAILROOM_WRITEBACK_DISABLED set) --apply also needs --despite-ingest-only: this
command writes to the provider directly, outside the writeback path that
ingest-only mode shuts (ADR-028).

Flags:
`

// deleteEmptyFlag collects repeated --delete-empty values.
type deleteEmptyFlag []string

func (f *deleteEmptyFlag) String() string { return strings.Join(*f, ",") }

func (f *deleteEmptyFlag) Set(v string) error {
	*f = append(*f, v)
	return nil
}

// runConsolidateFolders is `mailroom-admin consolidate-folders` (#647). See
// consolidateFoldersHelp and package internal/consolidate.
//
// Database reads go through Store.UserTx (the account row and
// UserTx.MailboxLiveCounts); nothing is written. The IMAP connection is built by
// the SAME resolver every other account connection uses
// (syncrunner.CredentialResolver with syncResolverOptions), so the #625 egress
// guard and MAILROOM_ALLOW_PRIVATE_ACCOUNT_HOSTS apply exactly as they do to sync.
func runConsolidateFolders(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("consolidate-folders", flag.ContinueOnError)
	var deleteEmpty deleteEmptyFlag
	var (
		userIDStr    = fs.String("user-id", "", "operator user id (ULID)")
		accountIDStr = fs.String("account-id", "", "account id (ULID; see list-accounts)")
		from         = fs.String("from", "", "IMAP LIST pattern of folders to empty into --into (e.g. 'INBOX/*')")
		into         = fs.String("into", "", "destination folder; must already exist")
		apply        = fs.Bool("apply", false, "perform the moves and deletions (default: report only)")
		maxFolders   = fs.Int("max-folders", 0, "bound one --apply run to N folder operations (0 = no bound)")
		despite      = fs.Bool("despite-ingest-only", false, "allow --apply on an ingest-only account (ADR-028)")
	)
	fs.Var(&deleteEmpty, "delete-empty", "IMAP LIST pattern of folders to delete once empty (repeatable)")
	fs.Usage = func() {
		_, _ = io.WriteString(fs.Output(), consolidateFoldersHelp)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return err
	}
	userID, err := parseUserID(*userIDStr)
	if err != nil {
		return err
	}
	accountID, err := ulid.Parse(*accountIDStr)
	if err != nil {
		return fmt.Errorf("consolidate-folders: --account-id: %w", err)
	}
	opts := consolidate.Options{From: *from, Into: *into, DeleteEmpty: deleteEmpty, MaxFolders: *maxFolders}
	if err := opts.Validate(); err != nil {
		return fmt.Errorf("consolidate-folders: %w", err)
	}

	d, err := openDeps(ctx)
	if err != nil {
		return err
	}
	defer d.close()

	var (
		acct   store.Account
		counts []store.MailboxLiveCount
	)
	if err := d.store.UserTx(ctx, userID, func(tx *store.UserTx) error {
		var e error
		if acct, e = tx.GetAccount(ctx, accountID); e != nil {
			return e
		}
		counts, e = tx.MailboxLiveCounts(ctx, accountID)
		return e
	}); err != nil {
		return fmt.Errorf("consolidate-folders: %w", err)
	}

	shadowed := !acct.WritebackEnabled || d.store.WritebackDisabled()
	if err := ingestOnlyGate(acct, d.store.WritebackDisabled(), *apply, *despite); err != nil {
		return err
	}

	// Network I/O, so OUTSIDE any transaction.
	resolver := syncrunner.NewCredentialResolver(d.keys, syncResolverOptions(d)...)
	req, err := resolver.Prepare(ctx, userID, acct)
	if err != nil {
		return fmt.Errorf("consolidate-folders: %w", err)
	}
	sess, err := sync.DialFolderAdmin(ctx, req.Config)
	if err != nil {
		return fmt.Errorf("consolidate-folders: connect: %w", err)
	}
	defer func() { _ = sess.Close() }()

	mode := "REPORT ONLY — nothing on the provider or in Mailroom is changed; add --apply to act"
	if *apply {
		mode = "APPLY — the report below is followed by the moves and deletions"
	}
	fmt.Printf("consolidate-folders: account %s (%s) at %s:%d, ingest-only=%t\n%s\n\n",
		acct.ID, acct.Name, acct.IMAPHost, req.Config.Port, shadowed, mode)

	if err := consolidateFolders(ctx, os.Stdout, sess, liveCountMap(counts), opts, *apply); err != nil {
		return err
	}
	_ = sess.Logout(ctx) // best-effort: every change is already verified by STATUS
	return nil
}

// consolidateFolders is the command after its dependencies are resolved: plan,
// report, and (with apply) act. Split out so the whole flow is testable over a
// fake session without a database.
func consolidateFolders(ctx context.Context, w io.Writer, sess consolidate.Session,
	counts map[string]consolidate.MailroomCount, opts consolidate.Options, apply bool) error {
	p, err := consolidate.BuildPlan(ctx, sess, counts, opts)
	if err != nil {
		return fmt.Errorf("consolidate-folders: %w", err)
	}
	if err := consolidate.WriteReport(w, p); err != nil {
		return err
	}
	if !apply {
		return nil
	}
	_, _ = fmt.Fprintf(w, "\nAPPLYING\n")
	res, err := consolidate.Apply(ctx, sess, p, w)
	_, _ = fmt.Fprintf(w, "\nDONE: moved %d folders (%d messages), deleted %d folders, kept %d at the moment of deleting, %d already gone",
		len(res.MovedFolders), res.MovedMessages, len(res.Deleted), len(res.NotDeleted), res.AlreadyGone)
	if res.Bounded {
		_, _ = fmt.Fprintf(w, "; stopped at --max-folders %d — re-run to continue", opts.MaxFolders)
	}
	_, _ = fmt.Fprintln(w)
	if err != nil {
		return fmt.Errorf("consolidate-folders: %w", err)
	}
	return nil
}

// liveCountMap keys store.MailboxLiveCounts by IMAP name for the plan.
func liveCountMap(rows []store.MailboxLiveCount) map[string]consolidate.MailroomCount {
	m := make(map[string]consolidate.MailroomCount, len(rows))
	for _, r := range rows {
		m[r.IMAPName] = consolidate.MailroomCount{Live: r.LiveProvenance, Role: r.Role}
	}
	return m
}

// ingestOnlyGate refuses --apply on an ingest-only account unless the operator
// says --despite-ingest-only. ADR-028 made remote writes on a shadowed account
// structurally impossible through the writeback path; this command does not go
// through that path, so the switch has to be honoured here, explicitly, or it
// would be the one way round it nobody decided on. Both halves count, as
// writeback-ledger reads them: the account's own column and the instance flag.
func ingestOnlyGate(acct store.Account, instanceDisabled, apply, despite bool) error {
	if !apply || despite || (acct.WritebackEnabled && !instanceDisabled) {
		return nil
	}
	return fmt.Errorf("consolidate-folders: account %s (%s) is INGEST-ONLY (writeback_enabled=%t, "+
		"MAILROOM_WRITEBACK_DISABLED=%t); --apply writes to the provider directly, outside the writeback "+
		"path ingest-only mode shuts, so it also needs --despite-ingest-only. Run without --apply first "+
		"to read the report", acct.ID, acct.Name, acct.WritebackEnabled, instanceDisabled)
}
