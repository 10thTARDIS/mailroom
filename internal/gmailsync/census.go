// SPDX-License-Identifier: AGPL-3.0-only

// Package gmailsync holds the Gmail read path's REPORT-FIRST tool: the
// census (#52, ADR-047 §6, increment 5a). Before a Gmail account's first real
// import, it reads — and only reads — what that import would do: how many
// messages and bytes, in which of All Mail, Spam and Trash, how the labels
// would map onto Mailroom's folders and labels, how many years of mail, how
// many days the import would take at the daily download budget, and how many
// messages Mailroom already holds from another account.
//
// It NEVER downloads a body. Every FETCH names only RFC822.SIZE,
// INTERNALDATE, X-GM-LABELS (and ENVELOPE, for the Message-ID, when the dedup
// preview is on); every SELECT is an EXAMINE; it never touches a label folder.
// Each number is reported per signal, never folded into one total, and a
// sampled run says so and states its bound (CLAUDE.md: a bounded result must
// report its bound).
package gmailsync

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"

	"mailroom/internal/gmaillabel"
	"mailroom/internal/sync"
)

// CensusStore is the read-only store surface the census needs.
type CensusStore interface {
	// ExistingLabels reports which of names are already Mailroom labels of
	// the user (exact match).
	ExistingLabels(ctx context.Context, names []string) (map[string]bool, error)
	// CountMessageIDsElsewhere counts how many of the Message-IDs the user
	// already has a message for outside this account.
	CountMessageIDsElsewhere(ctx context.Context, messageIDs []string) (int, error)
}

// CensusOptions bounds and shapes a census.
type CensusOptions struct {
	// Limit, when > 0, examines only the newest Limit messages of each
	// mailbox (by UID). The report then says it sampled, and extrapolates
	// bytes and days explicitly as estimates.
	Limit int
	// TopLabels is how many user labels to list by count (default 20).
	TopLabels int
	// DailyBudgetBytes is the budget the days-to-import estimate uses.
	DailyBudgetBytes int64
	// PageSize is how many UIDs one metadata FETCH names (default 1000).
	PageSize int
	// SkipDedup turns off the Message-ID dedup preview, and with it the
	// ENVELOPE in each FETCH (the largest metadata item: a few hundred bytes a
	// message).
	SkipDedup bool
}

// MailboxCensus is one synced mailbox's numbers.
type MailboxCensus struct {
	Name     string
	Role     string // "all", "junk" or "trash"
	Messages int    // EXISTS, as the EXAMINE reported it — exact
	Examined int    // messages whose metadata was read (Messages unless sampled)
	Bytes    int64  // total RFC822.SIZE of the examined messages
}

// LabelCount is a label and how many examined messages carry it.
type LabelCount struct {
	Name     string
	Messages int
	Existing bool // already a Mailroom label of this name
}

// Report is the census.
type Report struct {
	Mailboxes []MailboxCensus
	// NotSelected are the listed folders the Gmail sync never selects (label
	// views of All Mail), so the operator can see they were recognised.
	NotSelected []string

	Messages int   // sum of EXISTS — exact
	Examined int   // messages whose metadata was read
	Bytes    int64 // total RFC822.SIZE of the examined messages
	Sampled  bool  // Limit cut at least one mailbox short
	Limit    int
	// EstimatedBytes is Bytes when not sampled; when sampled, Bytes scaled by
	// Messages/Examined per mailbox — an ESTIMATE, labelled as one.
	EstimatedBytes int64

	// Folders counts where each examined message would land: All Mail copies
	// by their labels (inbox / sent / drafts / archive), Spam and Trash
	// copies by their mailbox.
	Folders map[string]int
	// SystemLabels counts every Gmail system label seen (\Inbox, \Important,
	// \Starred, ...), imported or not.
	SystemLabels map[string]int
	// UserLabelsTop is the TopLabels most-used user labels.
	UserLabelsTop []LabelCount
	// DistinctUserLabels is how many different user labels were seen; of
	// them, LabelsExisting already exist as Mailroom labels and LabelsNew
	// would be created. Applications* count (message, label) pairs the same way.
	DistinctUserLabels   int
	LabelsExisting       int
	LabelsNew            int
	ApplicationsExisting int
	ApplicationsNew      int
	// UnlabelledMessages are examined All Mail messages with no user label.
	UnlabelledMessages int
	// NoLabelReading are examined messages whose response carried no
	// X-GM-LABELS at all; they are in no folder or label count.
	NoLabelReading int

	// ByYear counts examined messages by INTERNALDATE year (UTC).
	ByYear map[int]int

	DailyBudgetBytes int64
	// DaysAtBudget is ceil(EstimatedBytes / DailyBudgetBytes): the days the
	// newest-first import would take at the history share of the budget is
	// slightly more (the budget keeps a tenth for new mail); it is printed
	// beside this one.
	DaysAtBudget        int
	DaysAtHistoryBudget int

	// The dedup preview: of DedupChecked examined messages with a
	// Message-ID, DedupElsewhere already exist in Mailroom outside this
	// account. NoMessageID had none to check.
	DedupSkipped   bool
	DedupChecked   int
	DedupElsewhere int
	NoMessageID    int
}

// ErrNotGmail is a server without X-GM-EXT-1.
var ErrNotGmail = errors.New("gmailsync: the server does not advertise X-GM-EXT-1; it is not Gmail")

// ErrAllMailHidden is a Gmail whose LIST has no \All mailbox.
var ErrAllMailHidden = errors.New("gmailsync: this Gmail account does not show All Mail to IMAP; " +
	"in Gmail's settings (Labels), turn on \"Show in IMAP\" for All Mail")

// Census reads an authenticated Gmail session and reports what an import
// would do. It never downloads a body and never selects a label folder.
func Census(ctx context.Context, sess sync.SyncSession, st CensusStore, opt CensusOptions) (Report, error) {
	if opt.TopLabels <= 0 {
		opt.TopLabels = 20
	}
	if opt.PageSize <= 0 {
		opt.PageSize = 1000
	}
	r := Report{
		Limit: opt.Limit, Folders: map[string]int{}, SystemLabels: map[string]int{},
		ByYear: map[int]int{}, DailyBudgetBytes: opt.DailyBudgetBytes, DedupSkipped: opt.SkipDedup,
	}
	if !sess.Supports(sync.CapabilityGmailExt) {
		return r, ErrNotGmail
	}
	list, err := sess.ListMailboxes(ctx)
	if err != nil {
		return r, fmt.Errorf("gmailsync: list: %w", err)
	}
	var targets []sync.Mailbox
	hasAll := false
	for _, m := range list {
		if m.HasAttr(sync.MailboxAttrNoSelect) || m.HasAttr(sync.MailboxAttrNonExistent) {
			continue
		}
		switch {
		case m.HasAttr(sync.MailboxAttrAll):
			hasAll = true
			targets = append(targets, m)
		case m.HasAttr(sync.MailboxAttrJunk), m.HasAttr(sync.MailboxAttrTrash):
			targets = append(targets, m)
		default:
			r.NotSelected = append(r.NotSelected, m.Name)
		}
	}
	if !hasAll {
		return r, ErrAllMailHidden
	}

	userCounts := map[string]int{}
	for _, m := range targets {
		mc, err := censusMailbox(ctx, sess, st, opt, m, &r, userCounts)
		if err != nil {
			return r, err
		}
		r.Mailboxes = append(r.Mailboxes, mc)
		r.Messages += mc.Messages
		r.Examined += mc.Examined
		r.Bytes += mc.Bytes
		if mc.Examined < mc.Messages {
			r.Sampled = true
		}
		if mc.Examined > 0 {
			r.EstimatedBytes += mc.Bytes * int64(mc.Messages) / int64(mc.Examined)
		}
	}

	// Labels: existing vs new, by distinct name and by application.
	names := make([]string, 0, len(userCounts))
	for n := range userCounts {
		names = append(names, n)
	}
	existing, err := st.ExistingLabels(ctx, names)
	if err != nil {
		return r, fmt.Errorf("gmailsync: existing labels: %w", err)
	}
	r.DistinctUserLabels = len(names)
	var top []LabelCount
	for _, n := range names {
		c := userCounts[n]
		if existing[n] {
			r.LabelsExisting++
			r.ApplicationsExisting += c
		} else {
			r.LabelsNew++
			r.ApplicationsNew += c
		}
		top = append(top, LabelCount{Name: n, Messages: c, Existing: existing[n]})
	}
	sort.Slice(top, func(i, j int) bool {
		if top[i].Messages != top[j].Messages {
			return top[i].Messages > top[j].Messages
		}
		return top[i].Name < top[j].Name
	})
	if len(top) > opt.TopLabels {
		top = top[:opt.TopLabels]
	}
	r.UserLabelsTop = top

	if opt.DailyBudgetBytes > 0 {
		r.DaysAtBudget = ceilDiv(r.EstimatedBytes, opt.DailyBudgetBytes)
		r.DaysAtHistoryBudget = ceilDiv(r.EstimatedBytes, opt.DailyBudgetBytes*9/10)
	}
	return r, nil
}

func ceilDiv(a, b int64) int {
	if b <= 0 || a <= 0 {
		return 0
	}
	return int((a + b - 1) / b)
}

func roleOf(m sync.Mailbox) string {
	switch {
	case m.HasAttr(sync.MailboxAttrAll):
		return "all"
	case m.HasAttr(sync.MailboxAttrJunk):
		return "junk"
	default:
		return "trash"
	}
}

func censusMailbox(ctx context.Context, sess sync.SyncSession, st CensusStore, opt CensusOptions, m sync.Mailbox, r *Report, userCounts map[string]int) (MailboxCensus, error) {
	mc := MailboxCensus{Name: m.Name, Role: roleOf(m)}
	// LiveUIDs is EXAMINE + UID SEARCH ALL: the exact UID list, 4 bytes a
	// message, so a sample can be the NEWEST N and pages can name exact UIDs.
	status, uids, err := sess.LiveUIDs(ctx, m.Name)
	if err != nil {
		return mc, fmt.Errorf("gmailsync: examine %q: %w", m.Name, err)
	}
	mc.Messages = int(status.NumMessages)
	sort.Slice(uids, func(i, j int) bool { return uids[i] > uids[j] }) // newest first
	if opt.Limit > 0 && len(uids) > opt.Limit {
		uids = uids[:opt.Limit]
	}
	items := sync.FetchItems{RFC822Size: true, InternalDate: true, GmailExt: true, Envelope: !opt.SkipDedup}
	for start := 0; start < len(uids); start += opt.PageSize {
		end := start + opt.PageSize
		if end > len(uids) {
			end = len(uids)
		}
		var msgIDs []string
		err := sess.FetchByUID(ctx, sync.UIDSetNum(uids[start:end]...), items, func(msg sync.Message) error {
			if msg.Body != nil {
				// Structurally impossible (BodyFull is never set), and checked
				// anyway: the census's one promise is that it reads no mail.
				return errors.New("gmailsync: a census fetch returned a body")
			}
			mc.Examined++
			mc.Bytes += msg.RFC822Size
			if !msg.InternalDate.IsZero() {
				r.ByYear[msg.InternalDate.UTC().Year()]++
			}
			if msg.GmailLabels == nil {
				// No X-GM-LABELS in the response: NOT "no labels" (#52 review
				// H1). Counted on its own and kept out of every label figure.
				r.NoLabelReading++
				return nil
			}
			c := gmaillabel.Classify(gmaillabel.Sanitize(msg.GmailLabels))
			for _, s := range c.System {
				r.SystemLabels[s]++
			}
			for _, u := range c.User {
				userCounts[u]++
			}
			if mc.Role == "all" {
				r.Folders[c.Folder]++
				if len(c.User) == 0 {
					r.UnlabelledMessages++
				}
			} else {
				r.Folders[map[string]string{"junk": gmaillabel.FolderSpam, "trash": gmaillabel.FolderTrash}[mc.Role]]++
			}
			if !opt.SkipDedup {
				if msg.Envelope != nil && msg.Envelope.MessageID != "" {
					msgIDs = append(msgIDs, msg.Envelope.MessageID)
				} else {
					r.NoMessageID++
				}
			}
			return nil
		})
		if err != nil {
			return mc, fmt.Errorf("gmailsync: fetch %q: %w", m.Name, err)
		}
		if len(msgIDs) > 0 {
			n, err := st.CountMessageIDsElsewhere(ctx, msgIDs)
			if err != nil {
				return mc, fmt.Errorf("gmailsync: dedup preview: %w", err)
			}
			r.DedupChecked += len(msgIDs)
			r.DedupElsewhere += n
		}
	}
	return mc, nil
}

// Write prints the report for an operator, one signal per line.
func (r Report) Write(w io.Writer) {
	p := func(format string, a ...any) { _, _ = fmt.Fprintf(w, format+"\n", a...) }
	p("gmail-census: READ-ONLY. No message body was downloaded; every mailbox was EXAMINEd.")
	if r.Sampled {
		p("  SAMPLED: only the newest %d messages of each mailbox were read (%d of %d). Counts below are over the sample; bytes and days are EXTRAPOLATED estimates.",
			r.Limit, r.Examined, r.Messages)
	}
	p("mailboxes synced (bodies would come only from these):")
	for _, m := range r.Mailboxes {
		p("  %-28s role=%-5s messages=%d examined=%d bytes=%d (%s)", m.Name, m.Role, m.Messages, m.Examined, m.Bytes, mb(m.Bytes))
	}
	p("folders never selected (Gmail label views of All Mail): %d", len(r.NotSelected))
	for _, n := range r.NotSelected {
		p("  %s", n)
	}
	p("total: messages=%d examined=%d bytes=%d (%s)", r.Messages, r.Examined, r.Bytes, mb(r.Bytes))
	if r.Sampled {
		p("total bytes (ESTIMATE, extrapolated from the sample): %d (%s)", r.EstimatedBytes, mb(r.EstimatedBytes))
	}
	p("would-be folder (examined messages):")
	for _, f := range []string{gmaillabel.FolderInbox, gmaillabel.FolderArchive, gmaillabel.FolderSent, gmaillabel.FolderDrafts, gmaillabel.FolderSpam, gmaillabel.FolderTrash} {
		p("  %-8s %d", f, r.Folders[f])
	}
	p("system labels (Gmail's own; only \\Inbox \\Sent \\Draft place a message, the rest are not imported):")
	for _, k := range sortedKeys(r.SystemLabels) {
		p("  %-14s %d", k, r.SystemLabels[k])
	}
	p("user labels: distinct=%d existing-in-mailroom=%d new=%d", r.DistinctUserLabels, r.LabelsExisting, r.LabelsNew)
	p("label applications: to-existing=%d to-new=%d; All Mail messages with no user label=%d",
		r.ApplicationsExisting, r.ApplicationsNew, r.UnlabelledMessages)
	if r.NoLabelReading > 0 {
		p("WARNING: %d examined message(s) came back with NO label reading; they are left out of the folder and label counts above", r.NoLabelReading)
	}
	p("top %d user labels by message count:", len(r.UserLabelsTop))
	for _, l := range r.UserLabelsTop {
		state := "new"
		if l.Existing {
			state = "exists"
		}
		p("  %-32s %6d  %s", l.Name, l.Messages, state)
	}
	p("messages per year (INTERNALDATE, UTC):")
	years := make([]int, 0, len(r.ByYear))
	for y := range r.ByYear {
		years = append(years, y)
	}
	sort.Ints(years)
	for _, y := range years {
		p("  %d  %d", y, r.ByYear[y])
	}
	if r.DailyBudgetBytes > 0 {
		est := ""
		if r.Sampled {
			est = " (ESTIMATE)"
		}
		p("import time at the daily budget of %s: %d day(s); at the history share (90%%): %d day(s)%s",
			mb(r.DailyBudgetBytes), r.DaysAtBudget, r.DaysAtHistoryBudget, est)
	}
	if r.DedupSkipped {
		p("dedup preview: SKIPPED (--skip-dedup)")
	} else {
		p("dedup preview: of %d examined messages with a Message-ID, %d already exist in Mailroom outside this account; %d had no Message-ID to check",
			r.DedupChecked, r.DedupElsewhere, r.NoMessageID)
	}
}

func mb(b int64) string { return fmt.Sprintf("%.1f MB", float64(b)/1e6) }

func sortedKeys(m map[string]int) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Slice(out, func(i, j int) bool { return strings.ToLower(out[i]) < strings.ToLower(out[j]) })
	return out
}
