// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"context"
	"flag"
	"fmt"
	"sort"
	"strings"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/store"
)

// `mailroom-admin mailbox-roles` — what SPECIAL-USE roles a provider actually
// advertises, asked BEFORE real mail depends on the answer (#351).
//
// WHY IT EXISTS. Mailroom detects mailbox roles once, from RFC 6154 SPECIAL-USE,
// at sync. Everything downstream trusts that detection and refuses to guess a
// folder name when it is absent — deliberately, because a guessed folder is how
// mail ends up somewhere the user will never look. The consequence is that a
// provider which does not advertise \Drafts or \Sent produces no error at
// onboarding, no error at sync, and no error at boot: it produces a failed
// action, once, at the moment a person tries to save a draft or send a message.
//
// Migrating to a new provider is exactly when that gets discovered the hard way,
// so this is the question made answerable in advance.
//
// READ-ONLY, AND THERE IS NOTHING TO APPLY. The fix for a missing role is not in
// this codebase — it is tagging the folder on the mail server, or choosing a
// provider that tags it. Printing the truth is the whole job.
//
// ⚠ THE EXIT CODE IS PART OF THE REPORT, so this can gate a cutover rather than
// being something someone remembers to read. A missing REQUIRED role exits
// non-zero; a missing degraded-but-usable one is printed and exits 0. The
// difference is not a matter of taste, and it is written down in
// mailboxRoleRequirements below with the call site that breaks.

// roleRequirement records what actually stops working when a role is absent, so
// the report's verdict is grounded in a call site rather than in an opinion.
type roleRequirement struct {
	role     string
	required bool
	// consequence is printed verbatim when the role is missing. It names the
	// user-visible failure, not the internal symbol.
	consequence string
}

// The roles Mailroom resolves, and what each one costs when the server does not
// advertise it. Ordered most-severe first so a truncated read still shows the
// blockers.
var mailboxRoleRequirements = []roleRequirement{
	{"drafts", true, "drafts cannot be saved at all — compose returns \"no Drafts mailbox\" (internal/compose/compose.go)"},
	{"sent", true, "a sent message cannot be filed; the copy the user expects to find is not written (internal/compose/send.go)"},
	{"trash", false, "deleting falls back to the local-only trash state; no server-side move happens"},
	{"archive", false, "the archive action and any archive rule have no destination (internal/store/rules.go)"},
	{"junk", false, "marking junk, and the \"not junk\" undo, cannot move the message on the server"},
	{"inbox", false, "returning a message to the inbox has no destination — note INBOX is matched by NAME, so this being missing means the account has no INBOX at all"},
}

func runMailboxRoles(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("mailbox-roles", flag.ContinueOnError)
	verbose := fs.Bool("verbose", false, "list every mailbox and its SPECIAL-USE attribute, not just the roles")
	userFlag := fs.String("user", "", "restrict the report to one user id (default: every active user)")
	if err := fs.Parse(args); err != nil {
		return err
	}

	d, err := openDeps(ctx)
	if err != nil {
		return err
	}
	defer d.close()

	users, err := d.store.ListActiveUserIDs(ctx)
	if err != nil {
		return fmt.Errorf("mailbox-roles: list users: %w", err)
	}
	if *userFlag != "" {
		u, perr := ulid.Parse(*userFlag)
		if perr != nil {
			return fmt.Errorf("mailbox-roles: parse -user: %w", perr)
		}
		users = []ulid.ULID{u}
	}

	// ⚠ COUNTED PER ROLE, NEVER AS ONE TOTAL. "3 roles missing" cannot
	// distinguish one account missing three from three accounts each missing
	// \Drafts, and those call for opposite responses.
	missingByRole := map[string]int{}
	accounts := 0
	blocked := 0

	for _, uid := range users {
		var accts []store.Account
		if err := d.store.UserTx(ctx, uid, func(tx *store.UserTx) error {
			var e error
			accts, e = tx.ListAccounts(ctx)
			return e
		}); err != nil {
			return fmt.Errorf("mailbox-roles: list accounts for %s: %w", uid, err)
		}

		for _, a := range accts {
			var rows []store.MailboxRoleRow
			if err := d.store.UserTx(ctx, uid, func(tx *store.UserTx) error {
				var e error
				rows, e = tx.MailboxRolesForAccount(ctx, a.ID)
				return e
			}); err != nil {
				return fmt.Errorf("mailbox-roles: roles for account %s: %w", a.ID, err)
			}
			accounts++

			// ⚠ FIRST WINS, AND THAT IS NOT A DETAIL. A server can advertise the
			// same SPECIAL-USE on two folders, and store.MailboxForRole — the
			// function the running server actually resolves through — orders by
			// imap_name and takes the FIRST match. MailboxRolesForAccount returns
			// rows in that same order, so keeping the first here makes this report
			// name the folder mail would really go to. Overwriting would name the
			// last one, and a report that disagrees with the code it describes is
			// worse than no report.
			byRole := map[string]string{}
			for _, r := range rows {
				if r.Role == "" {
					continue
				}
				if _, seen := byRole[r.Role]; !seen {
					byRole[r.Role] = r.IMAPName
				}
			}

			fmt.Printf("\nuser %s  account %s (%s)  — %d mailboxes\n",
				uid, a.Name, a.ID, len(rows))

			// ⚠ ZERO MAILBOXES IS NOT "NO ROLES MISSING". An account that has
			// never completed a sync has nothing to report roles from, and
			// printing six MISSING lines for it would be a measurement of the
			// sync state dressed up as a measurement of the provider.
			if len(rows) == 0 {
				fmt.Println("  NOT YET SYNCED — no mailboxes are stored for this account, so this")
				fmt.Println("  report can say NOTHING about what the provider advertises. Run a sync first.")
				continue
			}

			acctBlocked := false
			for _, req := range mailboxRoleRequirements {
				name, ok := byRole[req.role]
				switch {
				case ok:
					fmt.Printf("  %-8s ok       %s\n", req.role, name)
				case req.required:
					acctBlocked = true
					missingByRole[req.role]++
					fmt.Printf("  %-8s MISSING  BLOCKS: %s\n", req.role, req.consequence)
				default:
					missingByRole[req.role]++
					fmt.Printf("  %-8s missing  degraded: %s\n", req.role, req.consequence)
				}
			}
			if acctBlocked {
				blocked++
			}

			if *verbose {
				fmt.Println("  every mailbox, as stored:")
				for _, r := range rows {
					su := r.SpecialUse
					if su == "" {
						su = "(none advertised)"
					}
					role := r.Role
					if role == "" {
						role = "-"
					}
					fmt.Printf("    %-32s %-20s -> %s\n", r.IMAPName, su, role)
				}
			}
		}
	}

	fmt.Printf("\n%d account(s) examined across %d user(s).\n", accounts, len(users))
	if accounts == 0 {
		// A bounded/empty result reporting its bound: no accounts is a finding
		// about this deployment, not a clean bill of health for a provider.
		fmt.Println("NO ACCOUNTS FOUND — this report proved nothing about any provider.")
		return nil
	}

	var missing []string
	for role, n := range missingByRole {
		missing = append(missing, fmt.Sprintf("%s=%d", role, n))
	}
	sort.Strings(missing)
	if len(missing) == 0 {
		fmt.Println("Every role resolved on every account.")
	} else {
		fmt.Printf("Accounts missing each role: %s\n", strings.Join(missing, " "))
	}

	if blocked > 0 {
		return fmt.Errorf("%d of %d account(s) are missing a REQUIRED role; "+
			"drafts and sending will fail for them. Tag the folders on the mail server "+
			"(RFC 6154 SPECIAL-USE) and re-sync, then re-run this", blocked, accounts)
	}
	return nil
}
