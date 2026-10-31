// SPDX-License-Identifier: AGPL-3.0-only

package census

import (
	"context"
	"fmt"
	"io"
	"math"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"mailroom/internal/mime"
)

// HasAttachment reports whether p — a message's top-level BODYSTRUCTURE, as
// mapped into census's own Part tree — has at least one attachment leaf.
//
// ⚠ THIS MUST MATCH how ingest sets app.messages.has_attachments
// (internal/mime/walk.go: walkLeaf classifies with mime.IsAttachmentLeaf,
// and walkBody dispatches message/rfc822 to walkNestedMessage instead of
// walkLeaf, so a nested message is never itself counted as an attachment)
// or the census number means nothing — it would report the number of
// something-or-other, not the number the operator is actually deciding
// whether to trust ingest with (CLAUDE.md: a measurement is a claim about
// the code that produced it).
//
// The rule, precisely: a multipart container is never itself an attachment,
// so it recurses into its children. A message/rfc822 leaf is never an
// attachment either — walkBody routes message/rfc822 to walkNestedMessage,
// which appends the NESTED message to Message.Nested rather than the
// PARENT's Attachments, so from has_attachments' point of view a forwarded
// message's own container part simply isn't one — and this holds whether
// the message/rfc822 part is nested inside a multipart or is the WHOLE
// message's single top-level part (an unusual shape, but the same rule
// applies at the root as everywhere else in the tree). Every other leaf
// defers to mime.IsAttachmentLeaf — the one place that rule is allowed to
// live (internal/mime/partpath.go) — never re-derived here.
func HasAttachment(p *Part) bool {
	if p == nil {
		return false
	}
	if p.MediaType == "message/rfc822" {
		return false
	}
	if strings.HasPrefix(p.MediaType, "multipart/") {
		for _, c := range p.Children {
			if HasAttachment(c) {
				return true
			}
		}
		return false
	}
	return mime.IsAttachmentLeaf(p.MediaType, p.Disposition)
}

// trackedCapabilities are the capability names Report.Supports reports on.
// Each gets BOTH an Advertised bit (was this name literally in the
// server's CAPABILITY response, case-insensitively) and an Effective bit
// (per imap.go's CapabilitiesEffective, i.e. go-imap's own CapSet.Has,
// which is what internal/sync's sync engine actually queries) — the two
// can differ, because IMAP4rev2 implies UIDPLUS/MOVE/IDLE/… and QRESYNC
// implies CONDSTORE (RFC 9051/RFC 7162), so "not advertised" can still
// mean "Mailroom will use it".
var trackedCapabilities = []string{
	"IMAP4REV2", "UIDPLUS", "SPECIAL-USE", "CONDSTORE", "QRESYNC", "IDLE", "MOVE",
}

// censusRoles are the RFC 6154 SPECIAL-USE roles Report.Roles enumerates,
// keyed the same way internal/store/jmap_mailbox.go's roleForSpecialUse
// does (lowercase, no leading backslash), so a reader can cross-reference
// the two directly. \Sent \Drafts \Trash \Junk \Archive are the roles named
// by the task that produced this file; \All and \Flagged are ALSO roles
// Mailroom recognizes there (both map to a role name, and both mark a
// mailbox VIRTUAL below) so they are worth reporting even though day-one
// setup deliberately excludes them from anything actionable
// (internal/store/setup_census.go, setupCensusRoleIncluded). \Inbox is
// intentionally NOT in this list: RFC 6154 defines no `\Inbox` SPECIAL-USE
// attribute at all, and roleForMailbox recognizes the inbox by the reserved
// IMAP name "INBOX" instead — an attribute-keyed table has no way to name
// it, so it is reported implicitly via the mailbox actually named INBOX in
// Report.Mailboxes.
var censusRoles = []string{"sent", "drafts", "trash", "junk", "archive", "all", "flagged"}

const (
	flagSeen  = "\\Seen"
	flagDraft = "\\Draft"
)

// Report is the census's whole output. JSON-tagged so -json can emit it
// directly.
type Report struct {
	GeneratedAt time.Time `json:"generatedAt"`
	// Capabilities is the server's raw advertised capability list.
	Capabilities []string `json:"capabilities"`
	// Supports is the Advertised/Effective view over trackedCapabilities —
	// see CapVerdict.
	Supports map[string]CapVerdict `json:"supports"`

	Mailboxes []MailboxReport `json:"mailboxes"`
	// Roles maps each role in censusRoles to the mailbox name(s) carrying
	// it. An empty (nil) slice means the role is missing entirely.
	Roles map[string][]string `json:"roles"`

	// Totals is computed over selectable, NON-virtual mailboxes only — see
	// MailboxReport.Virtual.
	Totals Totals `json:"totals"`
	// ByYear buckets the same population as Totals by InternalDate in UTC,
	// ascending.
	ByYear []YearCount `json:"byYear"`
	// MessageIDs estimates cross-folder copies over the same population.
	MessageIDs MessageIDStats `json:"messageIds"`

	// Partial is true when at least one mailbox's Examine failed. The
	// report is still returned — CLAUDE.md: a bounded or empty result must
	// report its bound — but Totals/ByYear/MessageIDs exclude whatever that
	// mailbox would have contributed. Reserved for Examine FAILURES; see
	// Warnings for anomalies that are not failures.
	Partial bool `json:"partial"`

	// Warnings collects mailbox-level anomalies worth a reader's attention
	// up front, prefixed with the mailbox name: a streamed-count/EXAMINE-
	// count mismatch (MailboxReport.ExaminedMessages), or a nonzero
	// MailboxReport.NoStructure. Distinct from Partial: nothing here means
	// Examine returned an error, only that something about its answer is
	// worth a second look.
	Warnings []string `json:"warnings,omitempty"`

	// Probe holds the connection-cap probe's result, or nil if it was never
	// run (the CLI's -probe-connections defaults to off). See ProbeResult
	// and WriteText for how "not run" is distinguished from "ran and held
	// 0".
	Probe *ProbeResult `json:"probe,omitempty"`
}

// CapVerdict is one capability's Advertised/Effective verdict — see
// trackedCapabilities and imap.go's CapabilitiesEffective.
type CapVerdict struct {
	// Advertised is true iff the name appeared verbatim (case-insensitive)
	// in the server's CAPABILITY response.
	Advertised bool `json:"advertised"`
	// Effective is true iff go-imap's own imap.CapSet.Has() — the call
	// internal/sync's sync engine actually makes — would report it present,
	// which folds in what OTHER advertised capabilities imply.
	Effective bool `json:"effective"`
}

// MailboxReport is one mailbox's row in Report.Mailboxes.
type MailboxReport struct {
	Name       string   `json:"name"`
	Attributes []string `json:"attributes"`
	// Virtual is true for a mailbox carrying \All or \Flagged: a view over
	// other mailboxes' messages, not a folder of its own, so counting it
	// would double-count mail already counted where it actually lives.
	Virtual bool `json:"virtual"`
	// Selectable is false for \Noselect/\Nonexistent mailboxes (a pure
	// hierarchy node, e.g. a namespace prefix with no messages of its own).
	// Selectable == false mailboxes are never Examined.
	Selectable bool `json:"selectable"`

	Messages          int       `json:"messages,omitempty"`
	Bytes             int64     `json:"bytes,omitempty"`
	AttachmentBearing int       `json:"attachmentBearing,omitempty"`
	Drafts            int       `json:"drafts,omitempty"`
	Seen              int       `json:"seen,omitempty"`
	Oldest            time.Time `json:"oldest,omitempty"`
	Newest            time.Time `json:"newest,omitempty"`

	// ExaminedMessages is what EXAMINE itself reported (Examined.Messages)
	// before any message was streamed. Compared against Messages (what
	// FETCH actually delivered) as a cheap corruption/drop check: if
	// go-imap silently dropped an unparseable FETCH response without
	// surfacing an error, Messages would otherwise be silently short — the
	// "ran without erroring while doing nothing" class of bug (CLAUDE.md).
	// A mismatch does not set Error/Partial (Examine itself succeeded); it
	// is reported in the NOTE column and in Report.Warnings instead.
	ExaminedMessages int `json:"examinedMessages,omitempty"`
	// NoStructure counts messages with no BODYSTRUCTURE at all
	// (Meta.Structure == nil), for which HasAttachment had no verdict to
	// give — so AttachmentBearing's true denominator is Messages minus
	// NoStructure, not Messages.
	NoStructure int `json:"noStructure,omitempty"`

	// Error is set, and every other count field left at its zero value,
	// when Examine failed for this mailbox. A failure here does not stop
	// the census from examining the rest — see Report.Partial.
	Error string `json:"error,omitempty"`
}

// Totals is a population summary over selectable, non-virtual mailboxes.
type Totals struct {
	Messages          int   `json:"messages"`
	Bytes             int64 `json:"bytes"`
	AttachmentBearing int   `json:"attachmentBearing"`
	Drafts            int   `json:"drafts"`
	// NoStructure sums MailboxReport.NoStructure over the same population.
	NoStructure int              `json:"noStructure,omitempty"`
	Size        SizeDistribution `json:"size"`
}

// SizeDistribution is computed exactly (not estimated/sampled) from every
// message size collected: at the sizes this tool is meant to run against
// (tens of thousands of messages, each an int64), holding every size in
// memory to sort it once is cheap, and an approximation would be a strange
// thing to ship in a tool whose entire purpose is to be trusted before a
// cutover decision.
type SizeDistribution struct {
	Mean float64 `json:"mean"`
	P50  int64   `json:"p50"`
	P90  int64   `json:"p90"`
	P99  int64   `json:"p99"`
	Max  int64   `json:"max"`
}

// YearCount is one bucket of Report.ByYear.
type YearCount struct {
	Year     int   `json:"year"`
	Messages int   `json:"messages"`
	Bytes    int64 `json:"bytes"`
}

// MessageIDStats estimates cross-folder copies (the same message
// re-delivered, or filed, into more than one counted mailbox).
type MessageIDStats struct {
	// Distinct is the number of distinct non-empty Message-IDs seen.
	Distinct int `json:"distinct"`
	// Missing counts messages with no Message-ID at all (ENVELOPE returned
	// none) — reported rather than silently folded into Distinct, since a
	// message with no Message-ID cannot be checked for duplication at all.
	Missing int `json:"missing"`
	// SharedMessages counts every message whose Message-ID appears on more
	// than one message across the counted mailboxes (the messages
	// themselves, not the groups).
	SharedMessages int `json:"sharedMessages"`
	// SharedGroups counts the distinct Message-IDs that are shared by more
	// than one message.
	SharedGroups int `json:"sharedGroups"`
}

// yearAccum is Run's working accumulator for one mailbox's contribution to
// one calendar year, kept local until that mailbox's Examine has succeeded
// — see Run's doc for why.
type yearAccum struct {
	messages int
	bytes    int64
}

// Run walks every mailbox r reports and builds a Report. now is the
// generation timestamp (a parameter, not time.Now(), so the report is
// deterministic and testable).
//
// A mailbox whose Examine fails is recorded with its Error and does NOT
// stop the census: the remaining mailboxes are still examined (CLAUDE.md:
// build the report before the fix — a partial answer beats none). To keep
// that failure from silently corrupting the aggregates, every per-message
// fact this function collects is accumulated LOCALLY to the mailbox being
// examined and merged into Report.Totals/ByYear/MessageIDs only once that
// mailbox's Examine has returned with no error — a mailbox that fails
// halfway through streaming must contribute NOTHING to the totals, not the
// partial prefix it managed to stream before failing.
func Run(ctx context.Context, r Reader, now time.Time) (*Report, error) {
	caps, err := r.Capabilities(ctx)
	if err != nil {
		return nil, fmt.Errorf("census: capabilities: %w", err)
	}
	mailboxes, err := r.ListMailboxes(ctx)
	if err != nil {
		return nil, fmt.Errorf("census: list mailboxes: %w", err)
	}

	rep := &Report{
		GeneratedAt:  now,
		Capabilities: caps,
		Supports:     buildSupports(caps),
		Roles:        rolesMap(mailboxes),
	}

	var (
		allSizes    []int64
		byYear      = map[int]*YearCount{}
		msgIDCounts = map[string]int{}
		missingIDs  int
	)

	for _, mb := range mailboxes {
		mr := MailboxReport{
			Name:       mb.Name,
			Attributes: mb.Attributes,
			Virtual:    isVirtual(mb),
			Selectable: isSelectable(mb),
		}
		if !mr.Selectable {
			rep.Mailboxes = append(rep.Mailboxes, mr)
			continue
		}

		var (
			count, attachBearing, drafts, seenCount, noStructure int
			totalBytes                                           int64
			oldest, newest                                       time.Time
			localSizes                                           []int64
			localByYear                                          = map[int]*yearAccum{}
			localMsgIDs                                          = map[string]int{}
			localMissing                                         int
		)
		examined, examErr := r.Examine(ctx, mb.Name, func(m Meta) error {
			count++
			totalBytes += m.Size
			if m.Structure == nil {
				noStructure++
			}
			if HasAttachment(m.Structure) {
				attachBearing++
			}
			if hasFlag(m.Flags, flagDraft) {
				drafts++
			}
			if hasFlag(m.Flags, flagSeen) {
				seenCount++
			}
			d := m.InternalDate.UTC()
			if oldest.IsZero() || d.Before(oldest) {
				oldest = d
			}
			if newest.IsZero() || d.After(newest) {
				newest = d
			}
			if !mr.Virtual {
				localSizes = append(localSizes, m.Size)
				yr := d.Year()
				ya := localByYear[yr]
				if ya == nil {
					ya = &yearAccum{}
					localByYear[yr] = ya
				}
				ya.messages++
				ya.bytes += m.Size
				if m.MessageID == "" {
					localMissing++
				} else {
					localMsgIDs[m.MessageID]++
				}
			}
			return nil
		})
		if examErr != nil {
			mr.Error = examErr.Error()
			rep.Partial = true
			rep.Mailboxes = append(rep.Mailboxes, mr)
			continue
		}

		mr.Messages = count
		mr.Bytes = totalBytes
		mr.AttachmentBearing = attachBearing
		mr.Drafts = drafts
		mr.Seen = seenCount
		mr.Oldest = oldest
		mr.Newest = newest
		mr.ExaminedMessages = int(examined.Messages)
		mr.NoStructure = noStructure

		// Both are anomalies, not failures: Examine itself returned no
		// error, so Partial stays false, but a reader deciding whether to
		// trust these numbers should see them without hunting through every
		// row — collected here, rendered at the top by WriteText.
		if int(examined.Messages) != count {
			rep.Warnings = append(rep.Warnings, fmt.Sprintf(
				"%s: EXAMINE reported %d, FETCH returned %d (mail delivered during the run can cause this; re-run to tell)",
				mb.Name, examined.Messages, count))
		}
		if noStructure > 0 {
			rep.Warnings = append(rep.Warnings, fmt.Sprintf(
				"%s: %d message(s) had no BODYSTRUCTURE (no attachment verdict possible)", mb.Name, noStructure))
		}

		rep.Mailboxes = append(rep.Mailboxes, mr)

		if mr.Virtual {
			continue
		}
		rep.Totals.Messages += count
		rep.Totals.Bytes += totalBytes
		rep.Totals.AttachmentBearing += attachBearing
		rep.Totals.Drafts += drafts
		rep.Totals.NoStructure += noStructure
		allSizes = append(allSizes, localSizes...)
		for yr, ya := range localByYear {
			acc := byYear[yr]
			if acc == nil {
				acc = &YearCount{Year: yr}
				byYear[yr] = acc
			}
			acc.Messages += ya.messages
			acc.Bytes += ya.bytes
		}
		for id, n := range localMsgIDs {
			msgIDCounts[id] += n
		}
		missingIDs += localMissing
	}

	rep.Totals.Size = sizeDistribution(allSizes)
	rep.ByYear = sortedYears(byYear)
	rep.MessageIDs = messageIDStats(msgIDCounts, missingIDs)

	return rep, nil
}

// ExamineVerdicts is a minimal, test-oriented hook: it Examines mailbox and
// returns each message's UID mapped to HasAttachment's verdict on its
// BODYSTRUCTURE. Run only returns aggregates, which cannot support the
// per-message comparison the integration suite needs (census's verdict
// against internal/mime's parse of the same raw bytes, message by
// message) — this exists so that comparison has something to call without
// Run growing a detail mode nothing else needs.
func ExamineVerdicts(ctx context.Context, r Reader, mailbox string) (map[uint32]bool, error) {
	out := map[uint32]bool{}
	if _, err := r.Examine(ctx, mailbox, func(m Meta) error {
		out[m.UID] = HasAttachment(m.Structure)
		return nil
	}); err != nil {
		return nil, err
	}
	return out, nil
}

// MessageRow is one message as ListMessages reports it: exactly the keys
// needed to join a server message to a store's provenance row (mailbox,
// UIDVALIDITY, UID) plus the one measured fact, RFC822.SIZE. Deliberately
// nothing else — no Message-ID, flags, dates or structure — because this
// output exists to attribute a byte difference, and a per-message listing of
// someone's mailbox should carry no more than that job needs.
type MessageRow struct {
	Mailbox     string
	UIDValidity uint32
	UID         uint32
	Size        int64
}

// ListMessages streams one MessageRow per message in every selectable,
// non-virtual mailbox — the same population Report.Totals counts, so the rows
// sum to Totals.Bytes. It exists because a per-mailbox total can say THAT a
// store disagrees with the server but never WHICH message does (#529's
// calibration left a 1,029-byte Archive difference unattributable for exactly
// that reason).
//
// Rows for a mailbox are emitted only after its Examine succeeds, because
// UIDVALIDITY is known only then; a failing mailbox emits nothing and its error
// is returned, since a listing with a silently missing mailbox would read as a
// store holding messages the server does not.
func ListMessages(ctx context.Context, r Reader, fn func(MessageRow) error) error {
	mailboxes, err := r.ListMailboxes(ctx)
	if err != nil {
		return fmt.Errorf("census: list mailboxes: %w", err)
	}
	for _, mb := range mailboxes {
		if !isSelectable(mb) || isVirtual(mb) {
			continue
		}
		var rows []MessageRow
		ex, err := r.Examine(ctx, mb.Name, func(m Meta) error {
			rows = append(rows, MessageRow{Mailbox: mb.Name, UID: m.UID, Size: m.Size})
			return nil
		})
		if err != nil {
			return fmt.Errorf("census: examine %q: %w", mb.Name, err)
		}
		for _, row := range rows {
			row.UIDValidity = ex.UIDValidity
			if err := fn(row); err != nil {
				return err
			}
		}
	}
	return nil
}

func hasFlag(flags []string, want string) bool {
	for _, f := range flags {
		if strings.EqualFold(f, want) {
			return true
		}
	}
	return false
}

func hasAttr(attrs []string, want string) bool {
	for _, a := range attrs {
		if strings.EqualFold(a, want) {
			return true
		}
	}
	return false
}

// isVirtual reports whether mb is a view over other mailboxes' messages
// (\All or \Flagged) rather than a folder of its own.
func isVirtual(mb Mailbox) bool {
	return hasAttr(mb.Attributes, `\All`) || hasAttr(mb.Attributes, `\Flagged`)
}

// isSelectable reports whether mb can be EXAMINEd at all.
func isSelectable(mb Mailbox) bool {
	return !hasAttr(mb.Attributes, `\Noselect`) && !hasAttr(mb.Attributes, `\Nonexistent`)
}

// buildSupports computes the Advertised/Effective verdict for every
// trackedCapabilities name. Advertised is a simple case-insensitive
// membership check against the raw list; Effective delegates to imap.go's
// CapabilitiesEffective, which is the only place this package builds an
// imap.CapSet.
func buildSupports(caps []string) map[string]CapVerdict {
	have := make(map[string]bool, len(caps))
	for _, c := range caps {
		have[strings.ToUpper(c)] = true
	}
	effective := CapabilitiesEffective(caps, trackedCapabilities)

	out := make(map[string]CapVerdict, len(trackedCapabilities))
	for _, c := range trackedCapabilities {
		out[c] = CapVerdict{Advertised: have[strings.ToUpper(c)], Effective: effective[c]}
	}
	return out
}

func rolesMap(mailboxes []Mailbox) map[string][]string {
	out := make(map[string][]string, len(censusRoles))
	for _, role := range censusRoles {
		out[role] = nil
		attr := `\` + role
		for _, mb := range mailboxes {
			if hasAttr(mb.Attributes, attr) {
				out[role] = append(out[role], mb.Name)
			}
		}
	}
	return out
}

// sizeDistribution computes the exact mean/percentiles/max over sizes.
// Percentiles use nearest-rank on the sorted, ascending slice — the
// conventional definition for a finite population (as opposed to
// interpolated percentiles, which imply values between observations that
// were never actually measured).
func sizeDistribution(sizes []int64) SizeDistribution {
	if len(sizes) == 0 {
		return SizeDistribution{}
	}
	sorted := append([]int64(nil), sizes...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })

	var sum int64
	for _, s := range sorted {
		sum += s
	}
	return SizeDistribution{
		Mean: float64(sum) / float64(len(sorted)),
		P50:  percentile(sorted, 0.50),
		P90:  percentile(sorted, 0.90),
		P99:  percentile(sorted, 0.99),
		Max:  sorted[len(sorted)-1],
	}
}

// percentile returns the p-th percentile of sorted (ascending) by
// nearest-rank: the smallest value such that at least a fraction p of the
// population is <= it.
func percentile(sorted []int64, p float64) int64 {
	if len(sorted) == 0 {
		return 0
	}
	idx := int(math.Ceil(p*float64(len(sorted)))) - 1
	if idx < 0 {
		idx = 0
	}
	if idx >= len(sorted) {
		idx = len(sorted) - 1
	}
	return sorted[idx]
}

func sortedYears(m map[int]*YearCount) []YearCount {
	years := make([]int, 0, len(m))
	for y := range m {
		years = append(years, y)
	}
	sort.Ints(years)
	out := make([]YearCount, 0, len(years))
	for _, y := range years {
		out = append(out, *m[y])
	}
	return out
}

func messageIDStats(counts map[string]int, missing int) MessageIDStats {
	stats := MessageIDStats{Distinct: len(counts), Missing: missing}
	for _, n := range counts {
		if n > 1 {
			stats.SharedMessages += n
			stats.SharedGroups++
		}
	}
	return stats
}

// WriteText renders rep as an aligned, readable plain-text report.
func WriteText(w io.Writer, rep *Report) error {
	if rep.Partial {
		var failed []string
		for _, mb := range rep.Mailboxes {
			if mb.Error != "" {
				failed = append(failed, mb.Name)
			}
		}
		_, _ = fmt.Fprintf(w, "⚠ PARTIAL REPORT — Examine failed for: %s\n\n", strings.Join(failed, ", "))
	}
	if len(rep.Warnings) > 0 {
		_, _ = fmt.Fprintln(w, "⚠ WARNINGS:")
		for _, warn := range rep.Warnings {
			_, _ = fmt.Fprintf(w, "  - %s\n", warn)
		}
		_, _ = fmt.Fprintln(w)
	}

	_, _ = fmt.Fprintf(w, "generated: %s\n\n", rep.GeneratedAt.Format(time.RFC3339))

	writeCapabilities(w, rep)
	if err := writeMailboxes(w, rep); err != nil {
		return err
	}
	writeRoles(w, rep)
	writeByYear(w, rep)
	writeSizeDistribution(w, rep)
	writeMessageIDs(w, rep)
	writeProbe(w, rep.Probe)
	return nil
}

func writeCapabilities(w io.Writer, rep *Report) {
	_, _ = fmt.Fprintln(w, "CAPABILITIES")
	for _, c := range trackedCapabilities {
		v := rep.Supports[c]
		var status string
		switch {
		case v.Advertised:
			status = "yes"
		case v.Effective:
			status = "yes (implied, not advertised — go-imap CapSet.Has, which Mailroom's sync uses, treats it as present)"
		default:
			status = "no"
		}
		_, _ = fmt.Fprintf(w, "  %-12s %s\n", c, status)
	}
	_, _ = fmt.Fprintf(w, "  raw: %s\n\n", strings.Join(rep.Capabilities, " "))
}

func writeMailboxes(w io.Writer, rep *Report) error {
	_, _ = fmt.Fprintln(w, "MAILBOXES")
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "  NAME\tSPECIAL-USE\tMESSAGES\tSIZE\tBYTES\tATTACH\tDRAFTS\tOLDEST\tNEWEST\tNOTE")
	for _, mb := range rep.Mailboxes {
		_, _ = fmt.Fprintf(tw, "  %s\t%s\t%d\t%s\t%d\t%d\t%d\t%s\t%s\t%s\n",
			mb.Name, specialUse(mb.Attributes), mb.Messages, humanBytes(mb.Bytes), mb.Bytes,
			mb.AttachmentBearing, mb.Drafts, formatDate(mb.Oldest), formatDate(mb.Newest), mailboxNote(mb))
	}
	if err := tw.Flush(); err != nil {
		return fmt.Errorf("census: write report: %w", err)
	}
	_, _ = fmt.Fprintf(w, "  TOTAL (selectable, non-virtual mailboxes only): %d messages, %s (%d bytes), "+
		"%d attachment-bearing, %d drafts\n",
		rep.Totals.Messages, humanBytes(rep.Totals.Bytes), rep.Totals.Bytes,
		rep.Totals.AttachmentBearing, rep.Totals.Drafts)
	if rep.Totals.NoStructure > 0 {
		_, _ = fmt.Fprintf(w, "  %d message(s) had no BODYSTRUCTURE (no attachment verdict possible; "+
			"AttachmentBearing's denominator is Messages minus this)\n", rep.Totals.NoStructure)
	}
	_, _ = fmt.Fprintln(w)
	return nil
}

// mailboxNote renders the MAILBOXES table's NOTE column: every applicable
// condition, joined, rather than a single switch — a non-virtual, selectable
// mailbox with no Error can still have a count mismatch AND a nonzero
// NoStructure at the same time, and both are worth seeing on the row they
// describe (as well as in Report.Warnings, which Run populates for the same
// two conditions).
func mailboxNote(mb MailboxReport) string {
	var parts []string
	if !mb.Selectable {
		parts = append(parts, "not selectable — excluded")
	}
	if mb.Virtual {
		parts = append(parts, "virtual — excluded from totals")
	}
	if mb.Error != "" {
		parts = append(parts, "EXAMINE FAILED: "+mb.Error)
	}
	if mb.Selectable && mb.Error == "" && mb.ExaminedMessages != mb.Messages {
		parts = append(parts, fmt.Sprintf(
			"EXAMINE reported %d, FETCH returned %d (mail delivered during the run can cause this; re-run to tell)",
			mb.ExaminedMessages, mb.Messages))
	}
	if mb.NoStructure > 0 {
		parts = append(parts, fmt.Sprintf("%d message(s) had no BODYSTRUCTURE (no attachment verdict possible)", mb.NoStructure))
	}
	return strings.Join(parts, "; ")
}

// specialUseDisplayRoles is an ALLOWLIST of the RFC 6154 SPECIAL-USE roles
// (plus RFC 8457's \Important) that specialUse() will ever display, in
// display order.
//
// ⚠ THIS MUST BE AN ALLOWLIST, NOT A DENYLIST. Mailbox.Attributes are
// lowercased by imap.go (e.g. "\hasnochildren"), and a denylist written
// against the mixed-case spellings IMAP servers actually send
// (`\HasNoChildren`) never matches a lowercased attribute at all — every
// structural attribute silently fell through as "not denied", and the
// first thing that DID look like a role got shown instead: on a real
// server, INBOX (which reliably carries \HasNoChildren and often
// \Unmarked, never a real role) would have displayed "\hasnochildren" as
// its special-use. An allowlist has no such failure mode: matching is
// case-insensitive (hasAttr), so it is immune to casing on either side.
var specialUseDisplayRoles = []string{
	`\All`, `\Archive`, `\Drafts`, `\Flagged`, `\Junk`, `\Sent`, `\Trash`, `\Important`,
}

// specialUse renders every role in specialUseDisplayRoles that attrs
// carries, comma-joined (RFC 6154 expects at most one in practice, but
// nothing forbids a server from sending more, and hiding a second one would
// be its own silent-wrongness). "-" when none apply.
func specialUse(attrs []string) string {
	var found []string
	for _, role := range specialUseDisplayRoles {
		if hasAttr(attrs, role) {
			found = append(found, role)
		}
	}
	if len(found) == 0 {
		return "-"
	}
	return strings.Join(found, ",")
}

func formatDate(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	return t.Format("2006-01-02")
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%dB", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f%ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

func writeRoles(w io.Writer, rep *Report) {
	_, _ = fmt.Fprintln(w, "ROLES")
	for _, role := range censusRoles {
		names := rep.Roles[role]
		if len(names) == 0 {
			_, _ = fmt.Fprintf(w, "  %-10s MISSING\n", role)
			continue
		}
		_, _ = fmt.Fprintf(w, "  %-10s %s\n", role, strings.Join(names, ", "))
	}
	_, _ = fmt.Fprintln(w)
}

func writeByYear(w io.Writer, rep *Report) {
	_, _ = fmt.Fprintln(w, "BY YEAR (INTERNALDATE, UTC; selectable, non-virtual mailboxes)")
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "  YEAR\tMESSAGES\tBYTES")
	for _, y := range rep.ByYear {
		_, _ = fmt.Fprintf(tw, "  %d\t%d\t%d\n", y.Year, y.Messages, y.Bytes)
	}
	_ = tw.Flush()
	_, _ = fmt.Fprintln(w)
}

func writeSizeDistribution(w io.Writer, rep *Report) {
	s := rep.Totals.Size
	_, _ = fmt.Fprintln(w, "SIZE DISTRIBUTION (selectable, non-virtual mailboxes)")
	_, _ = fmt.Fprintf(w, "  mean %.0f  p50 %d  p90 %d  p99 %d  max %d\n\n", s.Mean, s.P50, s.P90, s.P99, s.Max)
}

func writeMessageIDs(w io.Writer, rep *Report) {
	m := rep.MessageIDs
	_, _ = fmt.Fprintln(w, "MESSAGE-ID DUPLICATES (selectable, non-virtual mailboxes)")
	_, _ = fmt.Fprintf(w, "  distinct %d  missing %d  shared messages %d  shared groups %d\n\n",
		m.Distinct, m.Missing, m.SharedMessages, m.SharedGroups)
}

func writeProbe(w io.Writer, p *ProbeResult) {
	_, _ = fmt.Fprintln(w, "CONNECTION PROBE")
	if p == nil || !p.Attempted {
		_, _ = fmt.Fprintln(w, "  not run")
		return
	}
	switch {
	case p.Refused && p.Held == 0:
		// NOT "refused at connection 1 after holding 0": that phrasing reads
		// as a measured cap of zero, when what actually happened is that the
		// probe never got far enough to measure anything — a wrong
		// password or a DNS failure looks identical to this until the error
		// text is read.
		_, _ = fmt.Fprintf(w, "  the FIRST connection failed, so no cap was measured: %s\n", p.RefusalError)
	case p.Refused:
		_, _ = fmt.Fprintf(w, "  refused at connection %d after holding %d: %s\n", p.Held+1, p.Held, p.RefusalError)
	default:
		_, _ = fmt.Fprintf(w, "  held %d of max %d — at least %d sessions fit (a lower bound; no refusal observed)\n",
			p.Held, p.Max, p.Held)
	}
	_, _ = fmt.Fprintln(w, "  scope: counts only sessions THIS run opened. Other clients connected to the same\n"+
		"    account at the time (a running Mailroom instance, phone/desktop mail apps) are NOT\n"+
		"    counted, so a refusal after N means N MORE sessions fit alongside whatever else was\n"+
		"    already connected — not that the cap is N. A provider may cap per account, per source\n"+
		"    IP, or both; this was measured from one IP only.")
}
