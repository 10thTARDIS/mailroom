// SPDX-License-Identifier: AGPL-3.0-only

package store

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/oklog/ulid/v2"
)

// THE CENSUS — step 1 of day-one setup (#446): every folder the provider
// sorted mail into, with the numbers a person needs to decide what it is.
//
// This file WRITES NOTHING. It is the report that has to exist before the bulk
// import does (CLAUDE.md: "build the report before the fix"), and it is the
// only thing the wizard's first screen renders. That constraint is why it does
// not call ListAreas or ListLabels: both MATERIALISE — ListAreas creates the
// default area plan, and its change_log rows, on first contact — and a census
// that quietly created five areas the first time somebody looked at it would
// be a report with a side effect, which is the thing the discipline exists to
// prevent. It reads app.areas and app.labels directly instead.
//
// EVERY NUMBER STATES ITS DENOMINATOR. Live is the population; InWindow and
// Older partition it; Unclassified (no From address at all) is counted rather
// than hidden, because a folder that is 40% unclassifiable is a folder whose
// shape word means much less than it looks like it does.

// FolderCensus is one folder, with everything the wizard needs to propose an
// action for it.
type FolderCensus struct {
	MailboxID ulid.ULID
	AccountID ulid.ULID
	// Name is the display name — and, by default, the LABEL day-one proposes
	// for this folder's mail. The folder's own name is the label: the user
	// already chose that word.
	Name string
	// Role is "" for a plain folder and "inbox" for INBOX, which IS a row (step
	// 2 applies to the inbox too — a mailbox arriving from another client has
	// years of unactioned inbox mail, and it is the largest single pile).
	// Special-use folders never appear at all: they are "mapped without asking".
	Role      string
	FeedsDesk bool

	// Live is the population: messages with LIVE provenance in this folder that
	// are not junk, trashed, vanished or drafts.
	Live int
	// InWindow is how many of Live arrived within the window (default 30 days).
	// THE WINDOW IS UNTOUCHED BY DESIGN — day-one never labels, archives or
	// moves anything in it, so this column is what the user reads to see how
	// much stays on the desk.
	InWindow int
	// Older is Live - InWindow: the mail day-one would act on.
	Older int
	// Unclassified is how many of Live carry no From address, so nothing here
	// can say anything about their sender, domain or shape. Counted, never
	// hidden.
	Unclassified int

	DistinctSenders int
	DistinctDomains int
	// SendersFor90 is the fewest distinct senders whose messages cover at least
	// 90% of Live — the concentration figure the "a handful of senders" shape
	// word is derived from, and the one that survives a long tail (a folder of
	// 200 messages from 6 senders plus 30 one-offs reads as fewSenders here and
	// as 36 distinct senders in the column beside it).
	SendersFor90 int
	// BulkShare is the share of Live carrying List-Unsubscribe, in [0,1].
	BulkShare float64
	// Shape is classifyShape's word for this folder.
	Shape string

	// ExistingLabelID is a label whose name already equals this folder's name
	// (case-insensitively), so day-one reuses it rather than minting a second
	// label meaning the same thing. Nil when there is none.
	ExistingLabelID *ulid.ULID
	// ExistingAreaID is an area whose name already equals this folder's name.
	// Nil when there is none — in which case the plan may create one.
	ExistingAreaID *ulid.ULID
}

// Shape words. THE THRESHOLDS BELOW ARE THE DESIGN'S OWN WORDS TURNED INTO
// NUMBERS, NOT MEASURED CONSTANTS. #446 describes a folder as holding mail from
// "one sender domain", "a handful of senders", "bulk", or "mixed"; classifyShape
// picks between those four, and every boundary in it (one domain, five senders,
// 80% bulk) is a judgement rather than a finding.
//
// That is acceptable ONLY because the client shows the counts beside the word:
// DistinctSenders, DistinctDomains, SendersFor90 and BulkShare are all on the
// row, so a wrong word is visible rather than authoritative, and the user's
// decision rests on the numbers. Do not add a behaviour that depends on the
// word alone.
const (
	// ShapeEmpty is a folder with no live mail at all — reported, never
	// omitted, because "this folder is empty" is the answer to "why does it
	// propose nothing" (CLAUDE.md: a bounded or empty result must report its
	// bound).
	ShapeEmpty = "empty"
	// ShapeOneDomain is every message from a single sender DOMAIN — the
	// strongest signal there is that the folder is about one relationship.
	ShapeOneDomain = "oneDomain"
	// ShapeFewSenders is "a handful of senders": at most shapeFewSendersMax
	// senders account for 90% of the folder.
	ShapeFewSenders = "fewSenders"
	// ShapeBulk is a folder that is overwhelmingly list mail.
	ShapeBulk = "bulk"
	// ShapeMixed is everything else — the honest answer, not a failure.
	ShapeMixed = "mixed"
)

const (
	// shapeFewSendersMax is what "a handful" means here.
	shapeFewSendersMax = 5
	// shapeBulkShare is what "bulk" means here.
	shapeBulkShare = 0.8
	// shapeCoverage is the share SendersFor90 must reach.
	shapeCoverage = 0.9
)

// classifyShape names a folder's shape. PURE, and unit-tested as such: the
// order of the tests below is the whole definition, and it is deliberate —
// oneDomain outranks fewSenders (a single domain with eight senders is still
// one relationship), and both outrank bulk (a newsletter you get only from one
// domain is better described by the domain than by the fact that it is a
// newsletter).
func classifyShape(c FolderCensus) string {
	switch {
	case c.Live == 0:
		return ShapeEmpty
	case c.DistinctDomains == 1:
		return ShapeOneDomain
	case c.SendersFor90 > 0 && c.SendersFor90 <= shapeFewSendersMax:
		return ShapeFewSenders
	case c.BulkShare >= shapeBulkShare:
		return ShapeBulk
	default:
		return ShapeMixed
	}
}

// sendersForCoverage is the fewest senders (largest first) whose messages cover
// at least `share` of total. Pure, so the arithmetic that produces
// SendersFor90 is testable without a database.
//
// It returns 0 when there is nothing to cover, which classifyShape reads as
// "no answer" rather than "one sender" — a folder whose every message has no
// From address has no sender concentration to report, and reporting 0 senders
// covering 90% of it would be a claim about mail that carries no such fact.
func sendersForCoverage(counts []int, total int, share float64) int {
	if total <= 0 || len(counts) == 0 {
		return 0
	}
	sorted := make([]int, len(counts))
	copy(sorted, counts)
	// Descending, by hand rather than via sort.Slice, so this file needs no
	// import for four lines of insertion sort over a handful of values.
	for i := 1; i < len(sorted); i++ {
		for j := i; j > 0 && sorted[j] > sorted[j-1]; j-- {
			sorted[j], sorted[j-1] = sorted[j-1], sorted[j]
		}
	}
	need := float64(total) * share
	var running float64
	for i, n := range sorted {
		running += float64(n)
		if running >= need {
			return i + 1
		}
	}
	return len(sorted)
}

// setupCensusRoleIncluded reports whether a mailbox with this JMAP role gets a
// census row.
//
// SPECIAL-USE FOLDERS ARE NOT ROWS — "mapped without asking" (#446). Drafts,
// Sent, Trash, Archive and Junk already have a meaning Mailroom knows, and
// proposing "label everything in Sent 'Sent' and archive it" would be asking
// the user to decide something that is not a decision. `all` and `flagged` are
// excluded on the same grounds (an All-mail folder is every message twice
// over, and a virtual Flagged folder is a view, not a filing).
//
// INBOX IS THE ONE ROLE THAT IS INCLUDED, because step 2 applies to it: the
// inbox is where the unactioned backlog actually is.
func setupCensusRoleIncluded(role string) bool {
	return role == "" || role == "inbox"
}

// MailboxCensus is the census: one row per non-special folder (plus INBOX) for
// the caller, ordered by display name then id. accountID narrows to one
// account; nil covers every account the caller has. windowDays is the
// "recently arrived" window — mail inside it is what stays on the desk.
//
// READ-ONLY. See the file header for why it does not go through
// ListAreas/ListLabels.
func (tx *UserTx) MailboxCensus(ctx context.Context, accountID *ulid.ULID, windowDays int) ([]FolderCensus, error) {
	if windowDays <= 0 {
		return nil, fmt.Errorf("%w: windowDays must be positive, got %d", ErrSetupPlanInvalid, windowDays)
	}

	rows, err := tx.tx.Query(ctx, `
		SELECT id, account_id, display_name, imap_name, COALESCE(special_use, ''), feeds_desk
		  FROM app.mailboxes
		 WHERE user_id = $1 AND ($2::app.ulid IS NULL OR account_id = $2)
		 ORDER BY display_name, id`,
		tx.userID.String(), ulidArg(accountID))
	if err != nil {
		return nil, fmt.Errorf("store: mailbox census (folders): %w", err)
	}
	defer rows.Close()

	var out []FolderCensus
	for rows.Next() {
		var (
			idStr, acctStr, display, imapName, specialUse string
			feedsDesk                                     bool
		)
		if err := rows.Scan(&idStr, &acctStr, &display, &imapName, &specialUse, &feedsDesk); err != nil {
			return nil, fmt.Errorf("store: scan census folder: %w", err)
		}
		// roleForMailbox, not a hand-rolled special-use test in SQL: the role
		// rules (INBOX by NAME, everything else by SPECIAL-USE, with or without
		// a leading backslash) live in exactly one place (jmap_mailbox.go), and
		// a second copy here is how the census would start disagreeing with
		// Mailbox/get about what a folder is.
		role := roleForMailbox(imapName, specialUse)
		if !setupCensusRoleIncluded(role) {
			continue
		}
		id, perr := ulid.Parse(idStr)
		if perr != nil {
			return nil, fmt.Errorf("store: parse census mailbox id %q: %w", idStr, perr)
		}
		acct, perr := ulid.Parse(acctStr)
		if perr != nil {
			return nil, fmt.Errorf("store: parse census account id %q: %w", acctStr, perr)
		}
		out = append(out, FolderCensus{
			MailboxID: id, AccountID: acct, Name: display, Role: role, FeedsDesk: feedsDesk,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: mailbox census (folders): %w", err)
	}
	if len(out) == 0 {
		return nil, nil
	}

	ids := make([]ulid.ULID, len(out))
	at := make(map[ulid.ULID]int, len(out))
	for i, f := range out {
		ids[i] = f.MailboxID
		at[f.MailboxID] = i
	}
	if err := tx.censusCounts(ctx, out, at, ids, windowDays); err != nil {
		return nil, err
	}
	if err := tx.censusSenderConcentration(ctx, out, at, ids); err != nil {
		return nil, err
	}
	if err := tx.censusExistingNames(ctx, out); err != nil {
		return nil, err
	}
	for i := range out {
		out[i].Shape = classifyShape(out[i])
	}
	return out, nil
}

// censusLiveSQL is the census population, as a CTE body binding $1 = user id
// and $2 = the mailbox id array: one row per (folder, message) for messages
// with LIVE provenance in one of those folders that are not junk, trashed,
// vanished or drafts.
//
// DISTINCT because a message can legitimately hold two live provenance rows in
// the SAME folder (a re-delivery with a second UID, #206) and that is one
// message in the folder, not two.
const censusLiveSQL = `
	SELECT DISTINCT mp.mailbox_id, m.id AS message_id, m.from_address,
	       m.received_at, m.list_unsubscribe
	  FROM app.message_provenance mp
	  JOIN app.messages m ON m.user_id = mp.user_id AND m.id = mp.message_id
	 WHERE mp.user_id = $1 AND mp.state = 'live' AND mp.mailbox_id = ANY($2)
	   AND NOT m.is_junk AND m.trashed_at IS NULL AND m.vanished_at IS NULL
	   AND NOT m.is_draft`

// censusCounts fills in the per-folder tallies in ONE query.
func (tx *UserTx) censusCounts(ctx context.Context, out []FolderCensus, at map[ulid.ULID]int, ids []ulid.ULID, windowDays int) error {
	rows, err := tx.tx.Query(ctx, `
		WITH live AS (`+censusLiveSQL+`)
		SELECT mailbox_id,
		       count(*),
		       count(*) FILTER (WHERE received_at >= now() - ($3::int * interval '1 day')),
		       count(*) FILTER (WHERE from_address IS NULL),
		       count(DISTINCT from_address),
		       count(DISTINCT lower(split_part(from_address::text, '@', 2)))
		         FILTER (WHERE from_address IS NOT NULL AND position('@' in from_address::text) > 0),
		       count(*) FILTER (WHERE list_unsubscribe IS NOT NULL)
		  FROM live
		 GROUP BY mailbox_id`,
		tx.userID.String(), ulidStrings(ids), windowDays)
	if err != nil {
		return fmt.Errorf("store: mailbox census (counts): %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			mbStr                                                string
			live, inWindow, unclassified, senders, domains, bulk int
		)
		if err := rows.Scan(&mbStr, &live, &inWindow, &unclassified, &senders, &domains, &bulk); err != nil {
			return fmt.Errorf("store: scan census counts: %w", err)
		}
		mb, perr := ulid.Parse(mbStr)
		if perr != nil {
			return fmt.Errorf("store: parse census counts mailbox id %q: %w", mbStr, perr)
		}
		i, ok := at[mb]
		if !ok {
			continue
		}
		out[i].Live = live
		out[i].InWindow = inWindow
		out[i].Older = live - inWindow
		out[i].Unclassified = unclassified
		out[i].DistinctSenders = senders
		out[i].DistinctDomains = domains
		if live > 0 {
			out[i].BulkShare = float64(bulk) / float64(live)
		}
	}
	return rows.Err()
}

// censusSenderConcentration fills in SendersFor90 from one per-(folder, sender)
// count query, with the arithmetic done by the pure sendersForCoverage above —
// a cumulative-sum window function would put the same logic in SQL where it
// cannot be unit-tested.
func (tx *UserTx) censusSenderConcentration(ctx context.Context, out []FolderCensus, at map[ulid.ULID]int, ids []ulid.ULID) error {
	rows, err := tx.tx.Query(ctx, `
		WITH live AS (`+censusLiveSQL+`)
		SELECT mailbox_id, count(*)
		  FROM live
		 WHERE from_address IS NOT NULL
		 GROUP BY mailbox_id, from_address`,
		tx.userID.String(), ulidStrings(ids))
	if err != nil {
		return fmt.Errorf("store: mailbox census (senders): %w", err)
	}
	defer rows.Close()
	perFolder := map[ulid.ULID][]int{}
	for rows.Next() {
		var mbStr string
		var n int
		if err := rows.Scan(&mbStr, &n); err != nil {
			return fmt.Errorf("store: scan census sender count: %w", err)
		}
		mb, perr := ulid.Parse(mbStr)
		if perr != nil {
			return fmt.Errorf("store: parse census sender mailbox id %q: %w", mbStr, perr)
		}
		perFolder[mb] = append(perFolder[mb], n)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("store: mailbox census (senders): %w", err)
	}
	for mb, counts := range perFolder {
		i, ok := at[mb]
		if !ok {
			continue
		}
		// The denominator is the folder's whole Live population, INCLUDING
		// messages with no From address: "six senders cover 90% of this folder"
		// must mean 90% of the folder, not 90% of the part that happened to name
		// a sender, or an unclassifiable folder would read as concentrated.
		out[i].SendersFor90 = sendersForCoverage(counts, out[i].Live, shapeCoverage)
	}
	return nil
}

// censusExistingNames fills in ExistingLabelID/ExistingAreaID by
// case-insensitive name match — the same reuse rule area-plan's
// findOrNilLabelByName and materialiseDefaultAreas already apply, for the same
// reason: an operator who has been filing mail by hand for years plausibly
// already has a label of the folder's name, and minting a second one would
// leave two labels meaning the same thing.
func (tx *UserTx) censusExistingNames(ctx context.Context, out []FolderCensus) error {
	labels, err := tx.labelIDsByLowerName(ctx)
	if err != nil {
		return err
	}
	areas, err := tx.areaIDsByLowerName(ctx)
	if err != nil {
		return err
	}
	for i := range out {
		key := strings.ToLower(strings.TrimSpace(out[i].Name))
		if id, ok := labels[key]; ok {
			lid := id
			out[i].ExistingLabelID = &lid
		}
		if id, ok := areas[key]; ok {
			aid := id
			out[i].ExistingAreaID = &aid
		}
	}
	return nil
}

// labelIDsByLowerName reads app.labels directly rather than through ListLabels
// — it needs only (lower(name) -> id) and the projection ListLabels builds
// (keyword atoms, tabs, colours) is work the census never looks at.
func (tx *UserTx) labelIDsByLowerName(ctx context.Context) (map[string]ulid.ULID, error) {
	return tx.idsByLowerName(ctx, `SELECT id, name FROM app.labels WHERE user_id = $1`, "labels")
}

// areaIDsByLowerName reads app.areas DIRECTLY, and deliberately not through
// ListAreas: that call MATERIALISES the default area plan (and appends its
// change_log rows) on first contact, and the census must write nothing at all.
func (tx *UserTx) areaIDsByLowerName(ctx context.Context) (map[string]ulid.ULID, error) {
	return tx.idsByLowerName(ctx, `SELECT id, name FROM app.areas WHERE user_id = $1`, "areas")
}

func (tx *UserTx) idsByLowerName(ctx context.Context, query, what string) (map[string]ulid.ULID, error) {
	rows, err := tx.tx.Query(ctx, query, tx.userID.String())
	if err != nil {
		return nil, fmt.Errorf("store: census names (%s): %w", what, err)
	}
	defer rows.Close()
	out := map[string]ulid.ULID{}
	for rows.Next() {
		var idStr, name string
		if err := rows.Scan(&idStr, &name); err != nil {
			return nil, fmt.Errorf("store: scan census name (%s): %w", what, err)
		}
		id, perr := ulid.Parse(idStr)
		if perr != nil {
			return nil, fmt.Errorf("store: parse census %s id %q: %w", what, idStr, perr)
		}
		// First wins on a case-collision, which cannot happen for areas (a
		// unique index on lower(name)) and is possible-but-pathological for
		// labels; either way the census only ever needs SOME existing label of
		// that name to reuse.
		key := strings.ToLower(strings.TrimSpace(name))
		if _, seen := out[key]; !seen {
			out[key] = id
		}
	}
	return out, rows.Err()
}

// ulidArg renders an optional id for a `$n::app.ulid IS NULL OR ...` predicate:
// pgx maps a nil `any` to SQL NULL, which is how "no narrowing" is expressed
// without building two query texts.
func ulidArg(id *ulid.ULID) any {
	if id == nil {
		return nil
	}
	return id.String()
}

// bindFunc is the parameter-binding closure shape every SQL builder in this
// package takes (see areaJoinSQL): it appends an argument and returns its
// placeholder. Named here because setup.go builds several statements that share
// one argument list.
func bindFunc(args *[]any) func(any) string {
	return func(v any) string {
		*args = append(*args, v)
		return "$" + strconv.Itoa(len(*args))
	}
}
