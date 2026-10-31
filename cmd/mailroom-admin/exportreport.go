// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"context"
	"flag"
	"fmt"

	"mailroom/internal/exportplan"
	"mailroom/internal/store"
)

// `mailroom-admin export-report` — what a per-user export would carry, what it
// would leave behind, and what nobody has decided about (#54, spec §10).
//
// READ-ONLY, and it exists before the exporter for the reason this project keeps
// re-learning: the dangerous export failure is not a crash, it is a file that is
// produced successfully and is quietly incomplete. Spec §10 describes the export
// in one sentence naming four things — "mbox or maildir + JSON sidecars for
// labels/scores/contacts" — against a schema of forty-three tables. Writing the
// exporter first would mean deciding that gap implicitly, one omission at a
// time, with nothing printing what was decided.
//
// So: internal/exportplan holds an explicit disposition for every user-scoped
// table, and this command prints it against the live corpus with row counts.
// The decisions become reviewable BEFORE any of them is load-bearing.
//
// ⚠ THE CHECK THAT MATTERS MOST IS THE ONE FOR TABLES NOBODY DECIDED ABOUT.
// The table set comes from pg_catalog at run time, so a table added tomorrow is
// compared against the plan the day it exists and reported as UNHANDLED rather
// than silently omitted from a user's data. #316 is the cost of the other
// approach.
func runExportReport(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("export-report", flag.ContinueOnError)
	userFlag := fs.String("user-id", "", "the user to report on (required)")
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

	var (
		exists  bool
		census  store.UserCensus
		survey  store.ExportSurvey
		userErr error
	)
	if err := d.store.UserTx(ctx, userID, func(tx *store.UserTx) error {
		if exists, userErr = tx.SelfExists(ctx); userErr != nil {
			return userErr
		}
		if census, userErr = tx.UserRowCensus(ctx); userErr != nil {
			return userErr
		}
		survey, userErr = tx.SurveyExportable(ctx)
		return userErr
	}); err != nil {
		return fmt.Errorf("export-report: %w", err)
	}

	rp("user                         : %s\n", userID)
	if !exists {
		// Same refusal as delete-user, for the same reason: every count below
		// would be zero for ANY id, and a page of zeroes reads as "there is
		// nothing here to export".
		rpln("")
		rpln("⚠ NO SUCH USER — or the row is not visible to this connection.")
		rpln("  An RLS-scoped transaction for a user that does not exist reads zero from")
		rpln("  every table, which is indistinguishable from a user who owns nothing.")
		return fmt.Errorf("export-report: user %s not found", userID)
	}

	reportUnhandled(census)
	reportDispositions(census)
	reportMailShape(survey)
	reportExportLimits(survey)
	return nil
}

// reportUnhandled is the load-bearing check, so it runs FIRST and before any
// reassuring per-table detail.
func reportUnhandled(census store.UserCensus) {
	live := make([]string, 0, len(census.Tables)+1)
	for _, t := range census.Tables {
		live = append(live, t.Table)
	}
	// The root is a real table with a decision to make about it, even though it
	// carries no user_id column of its own.
	if census.Root != "" {
		live = append(live, census.Root)
	}
	missing, stale := exportplan.Unhandled(live)

	rpln("")
	rpln("-- tables nobody has decided about -------------------------------------")
	switch {
	case len(missing) == 0 && len(stale) == 0:
		rp("  none — all %d live tables have an explicit disposition.\n", len(live))
	default:
		for _, t := range missing {
			rp("  ⚠ UNHANDLED: app.%s exists and the export plan does not mention it.\n", t)
			rpln("    An export would omit it silently. Decide, then add it to")
			rpln("    internal/exportplan.")
		}
		for _, t := range stale {
			rp("  ⚠ STALE: the plan decides app.%s, which is not in the schema.\n", t)
			rpln("    Harmless to an export, but the plan has drifted — and a plan that has")
			rpln("    drifted once is not evidence for what else it claims.")
		}
	}
}

// reportDispositions prints the decision for every table, grouped, with the
// live row count beside it.
//
// ⚠ THE COUNTS ARE WHAT MAKE THIS MORE THAN A LIST. "rules: dropped" is a
// design note; "rules: dropped, 127 rows" is a statement about what this user
// actually loses.
func reportDispositions(census store.UserCensus) {
	rows := make(map[string]int64, len(census.Tables))
	unreadable := make(map[string]bool, len(census.Tables))
	for _, t := range census.Tables {
		rows[t.Table] = t.Rows
		if t.SkipReason != "" {
			unreadable[t.Table] = true
		}
	}
	if census.Root != "" {
		rows[census.Root] = census.RootRows
	}

	groups := []struct {
		d      exportplan.Disposition
		title  string
		legend string
	}{
		{exportplan.Exported, "carried by the export", ""},
		{exportplan.Dropped, "deliberately NOT carried", "the user loses these on migration"},
		{exportplan.Withheld, "WITHHELD — must not appear in an export", "credentials or admin record"},
	}

	for _, g := range groups {
		rpln("")
		if g.legend != "" {
			rp("-- %s (%s) ---\n", g.title, g.legend)
		} else {
			rp("-- %s ---------------------------------------------\n", g.title)
		}
		var carried, empty int
		for _, r := range exportplan.Rows() {
			if r.Entry.Disposition != g.d {
				continue
			}
			n, known := rows[r.Table]
			switch {
			case unreadable[r.Table]:
				// ⚠ Not counted as zero. A table this report cannot read is one
				// the export cannot verify either.
				rp("  %-22s   UNREADABLE — not measured\n", r.Table)
			case !known:
				rp("  %-22s   (not in this schema)\n", r.Table)
			case n == 0:
				empty++
			default:
				carried++
				rp("  %-22s %8d\n", r.Table, n)
			}
			if r.Entry.Detail != "" && known && n > 0 {
				rp("      -> %s\n", r.Entry.Detail)
			}
			if r.Entry.Reason != "" && known && n > 0 {
				// An exported table's reason is a caveat about what is carried
				// WITHIN it; a dropped or withheld one's is the justification.
				// Same field, different sentence — so the prefix has to differ
				// or an exported table reads as if it were being excused.
				if r.Entry.Disposition == exportplan.Exported {
					rp("      note: %s\n", r.Entry.Reason)
				} else {
					rp("      because %s\n", r.Entry.Reason)
				}
			}
		}
		rp("  (%d tables hold rows for this user; %d more are empty)\n", carried, empty)
	}
}

// reportMailShape sizes the maildir and names what is NOT in the raw bytes.
func reportMailShape(s store.ExportSurvey) {
	rpln("")
	rpln("-- the mail itself -----------------------------------------------------")
	rp("messages                     : %d\n", s.Messages)
	rp("raw bytes (maildir size)     : %s\n", humanBytes(s.RawBytes))
	rp("  of which trashed           : %d — carrying a user's trash is a decision;\n", s.Trashed)
	rpln("                               today's plan exports them like any other message")

	rpln("")
	rpln("-- state that is NOT in the raw bytes -----------------------------------")
	rpln("  RFC 5322 has no read flag. These live in columns, so an export that wrote")
	rpln("  only the mail would re-import every one of them in the wrong state.")
	rp("messages carrying a flag     : %d\n", s.FlagBearing)
	rp("  with IMAP keywords         : %d\n", s.Keyworded)
	rp("    of which NOT explained by a label : %d\n", s.KeywordedIndependently)
	// ⚠ The split is the whole point of these two lines, so it is explained
	// whichever way it comes out. The raw keyword count read as a large loss on
	// a real mailbox until it was audited: every keyword there is a
	// `MailroomLabel-*` atom Mailroom wrote to IMAP itself, so labels.json
	// already carries it.
	switch {
	case s.Keyworded == 0:
		rpln("    (no keywords at all, so nothing here to carry or lose)")
	case s.KeywordedIndependently == 0:
		rpln("    ZERO — every keyword on this mailbox is one Mailroom itself wrote for a")
		rpln("    label (app.label_keyword_map), so exporting labels carries them all. The")
		rpln("    keyword count above is NOT a measure of loss on this mailbox.")
	default:
		rpln("    ⚠ These are keywords no label explains — written by another client, or by")
		rpln("      the provider. They exist nowhere in the raw bytes and nowhere in the")
		rpln("      labels, so a sidecar that omits them loses them outright.")
	}
	rp("label applications           : %d\n", s.Labelled)
	rp("  attributed to a rule       : %d\n", s.RuleApplied)
	rp("send attempts                : %d\n", s.Submissions)
	rp("  ending in anything but 'sent' : %d\n", s.SubmissionsUnsent)
	// ⚠ Per-case, never one total. A successful send is already described by the
	// message; only a failed or cancelled one needs this table to be legible at
	// all, so the second number is what says whether carrying it matters here.
	if s.Submissions > 0 && s.SubmissionsUnsent == 0 {
		rpln("    every send on this mailbox succeeded, so the exported mail already")
		rpln("    describes all of them — submissions.json costs little and records")
		rpln("    nothing here that the messages do not. That is a fact about this")
		rpln("    mailbox, not about the format.")
	}
	if s.RuleAppliedOrphan > 0 {
		rp("  ⚠ marked rule-applied but naming no rule: %d\n", s.RuleAppliedOrphan)
		rpln("    (rules delete SET NULL, so the attribution outlives the rule. Counted")
		rpln("     apart so \"provenance carried\" is not overstated.)")
	}
}

// reportExportLimits states the things an export cannot deliver today, in the
// report rather than in a doc nobody reads at the moment it matters.
func reportExportLimits(s store.ExportSurvey) {
	rpln("")
	rpln("-- what an export cannot deliver today ----------------------------------")
	rpln("  ⚠ PGP-ENCRYPTED MAIL EXPORTS UNREADABLE.")
	rpln("    An export carries the message bytes, which for encrypted mail is")
	rpln("    ciphertext — and it cannot carry the key that opens it. spec §9 lists an")
	rpln("    `export-wrapped` operation on the crypto process; ADR-024 defers building")
	rpln("    it, and #54 is the trigger that ends that deferral. Until it exists this")
	rpln("    is a stated limit, not a surprise found later.")
	rpln("    Run `mailroom-admin pgp-report --user-id …` for this mailbox's count.")
	rpln("")
	rpln("  ⚠ THERE IS NO EXPORTER YET. This command decides and measures; it writes")
	rpln("    no files. The round-trip gate for the exporter is stated in #54: export")
	rpln("    then import, and assert content_hash equality — byte-exactness is the")
	rpln("    property, because identity IS the SHA-256 of the raw bytes (ADR-001).")
}
