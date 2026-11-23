// SPDX-License-Identifier: AGPL-3.0-only

package store

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/placement"
	"mailroom/internal/senderwarn"
)

// MEASURING LEARNED SORTING BEFORE CHANGING IT (#28, #169): the data layer of
// `mailroom-admin placement-measure`.
//
// REPORT ONLY. Every method here reads; nothing writes, and nothing it computes
// is read by the ingest path. The arithmetic is internal/placement's (measure.go,
// pure and unit-tested); this file gathers the rows it needs and decides, in
// one place per section, what a row MEANS. Each such decision is stated where
// it is made, because a number is only as good as the definition behind it
// (CLAUDE.md: a measurement is a claim about the code that produced it).
//
// WHAT THE STORED DATA CAN AND CANNOT SAY. Labels and flags are CURRENT STATE,
// not a history: when the user re-files a message the desk's label is deleted
// and the user's is inserted, so the original placement of a moved message is
// gone. What does carry a time is each label row's created_at, and the
// feedback_events the web client records (an `open` after the reading-pane
// dwell, an `archive_unread` when unread mail is archived). The sections below
// are built from exactly those, and each one that has to leave something out
// says how much it left out.

// PlacementMeasureOptions configures the measurement.
type PlacementMeasureOptions struct {
	// Now is the clock every age is judged against (the caller's, so a test
	// can pin it).
	Now time.Time
	// WindowDays bounds the messages sections 4 and 5 read (created in the
	// last WindowDays). Sections 3 and 6 read the user's filings and are not
	// windowed; section 2 reads the mailbox, bounded by AuthScanMax.
	WindowDays int
	// SplitDays places the split day D: events judged before it teach, and only
	// messages that arrived on or after it are scored (no fixed point).
	SplitDays int
	// RecentDays is the window of "messages received lately" in section 3.
	RecentDays int
	// AuthScanMax caps how many messages section 2 reads the raw headers of.
	AuthScanMax int
	// Senders turns on EXACT mode for section 2 (the arrival-time checks on each
	// message's own headers, as placement-backfill does). Nil reads the stored
	// auth_fail only, which is a LOWER bound.
	Senders PlacementSenderFunc
}

// Defaults for PlacementMeasureOptions.
const (
	MeasureWindowDays  = 365
	MeasureSplitDays   = 45
	MeasureRecentDays  = 30
	MeasureAuthScanMax = 3000
	// MeasureLeftAloneDays is how long a placement must sit unmoved before it
	// counts as "left alone": the gate's own KeptAfter, so the two agree.
	MeasureLeftAloneDays = 7
)

// AuthBucket is section 2's counts for one verdict of the sender's key.
type AuthBucket struct {
	// Messages is every message from keys with this verdict that the section
	// judged (read in exact mode, tested against the stored flag otherwise).
	Messages int
	// OK passed every arrival-time check; AuthOnly was refused ONLY for
	// authentication; AuthAlso was refused for authentication AND something
	// else (an ambiguous From or a sender warning); OtherOnly was refused for
	// something else alone. Exact mode only: otherwise the unread rest is OK.
	OK, AuthOnly, AuthAlso, OtherOnly int
	// AuthOnlyKeys are the distinct decision keys behind AuthOnly, and Covered
	// those an enabled rule already files into the key's area (an UPPER bound:
	// a rule that merely names the address or domain counts).
	AuthOnlyKeys, CoveredKeys, CoveredMessages int
}

// AuthRefusals is section 2: filed senders refused for authentication alone.
type AuthRefusals struct {
	// Exact: each message was judged on its own raw headers. False: the stored
	// auth_fail only, a lower bound on authentication refusals (it cannot see a
	// missing trusted DMARC pass).
	Exact bool
	// Population is every message from a key whose evidence is confident or
	// thin and that no label already placed; Read how many were judged (newest
	// first, capped by AuthScanMax); Unreadable how many bytes could not be read.
	Population, Read, Unreadable int
	Truncated                    bool
	Confident, Thin              AuthBucket
}

// LeftAloneCoverage says how much the left-alone measurement could and could
// not see, so a small number is not read as a total.
type LeftAloneCoverage struct {
	// DeskPlaced counts messages the desk placed (label applied_by 'desk') that
	// were old enough to be judged before the split; Opened how many of those
	// the user opened within the left-alone window.
	DeskPlaced, Opened int
	// EarliestOpen is the first `open` event the user has at all (zero: none).
	// The signal does not exist before it: older mail can never count as
	// opened, whatever the user did.
	EarliestOpen time.Time
	// UnobservableMoved counts messages the user filed more than the window
	// after they arrived, before the split. Each WAS left alone for the window
	// and then moved, but the desk's label (which named the original area) was
	// deleted by the move, so the message cannot be an event. The events are
	// missing the failures that happened late, which can only flatter the
	// signal.
	UnobservableMoved int
	// ExcludedRule and ExcludedLearned count later messages left out of the
	// outcome because a rule or learned placement had decided their area.
	ExcludedRule, ExcludedLearned int
	// Outcomes is how many later messages were scored.
	Outcomes int
}

// ArchivedCoverage is the same for the archived-unread measurement.
type ArchivedCoverage struct {
	// EventsNoArea counts archive-unread events dropped because the message
	// carries no area label to name its origin.
	EventsNoArea int
	// PrimaryOutcomes is how many later messages in Primary were scored.
	PrimaryOutcomes int
}

// PlacementMeasure is everything placement-measure prints.
type PlacementMeasure struct {
	Now, Split, WindowStart time.Time
	Options                 PlacementMeasureOptions
	AreaNames               map[ulid.ULID]string
	// Primary is the area section 5 scores later mail in (the area named
	// Primary, else the first area); zero when the user has no areas.
	Primary     ulid.ULID
	PrimaryName string

	Abstentions AbstentionTally
	Auth        AuthRefusals

	RecencyAddress, RecencyDomain placement.Recency
	RecentDays                    int

	// LeftAlone is the signal as defined (opened and not moved); LeftAloneAny
	// drops the opened condition (merely not moved) as a contrast.
	LeftAlone, LeftAloneAny placement.LeftAloneResult
	LeftAloneCover          LeftAloneCoverage
	// UserEvidence is how many filings of the user's own the split leaves as
	// existing evidence (before D).
	UserEvidence int

	Archived      placement.ArchiveResult
	ArchivedCover ArchivedCoverage

	ListID placement.ListIDCeiling
	// ListIDAddresses counts sending addresses whose mail carries more than one
	// List-Id; ListIDCensus is how many messages have their List-Id key filled.
	ListIDAddresses int
	ListIDCensus    RecipientCensus
}

// measureLabel is one area-own-label row on a message.
type measureLabel struct {
	Area ulid.ULID
	By   string
	At   time.Time
}

// measureMessage is one message's facts for sections 4 and 5.
type measureMessage struct {
	From       string
	Created    time.Time
	Seen       bool
	Archived   bool
	Trashed    bool
	Labels     []measureLabel
	OpenedAt   *time.Time
	ArchUnread *time.Time
}

// PlacementMeasure computes the report. It writes nothing.
func (tx *UserTx) PlacementMeasure(ctx context.Context, opts PlacementMeasureOptions) (PlacementMeasure, error) {
	if opts.Now.IsZero() {
		opts.Now = time.Now()
	}
	if opts.WindowDays <= 0 {
		opts.WindowDays = MeasureWindowDays
	}
	if opts.SplitDays <= 0 {
		opts.SplitDays = MeasureSplitDays
	}
	if opts.RecentDays <= 0 {
		opts.RecentDays = MeasureRecentDays
	}
	if opts.AuthScanMax <= 0 {
		opts.AuthScanMax = MeasureAuthScanMax
	}
	now := opts.Now.UTC()
	m := PlacementMeasure{
		Now: now, Options: opts, RecentDays: opts.RecentDays,
		Split:       now.AddDate(0, 0, -opts.SplitDays),
		WindowStart: now.AddDate(0, 0, -opts.WindowDays),
	}
	var err error
	if m.AreaNames, m.Primary, m.PrimaryName, err = tx.measureAreas(ctx); err != nil {
		return m, err
	}
	own, err := tx.ownIdentityDomains(ctx)
	if err != nil {
		return m, err
	}

	// 1. Why arrivals were not placed.
	if m.Abstentions, err = tx.AbstentionTally(ctx, 30, now); err != nil {
		return m, err
	}

	// The user's own filings feed sections 2, 3, 4 and 6.
	filings, err := tx.placementFilings(ctx, "", time.Time{})
	if err != nil {
		return m, err
	}

	// 2. Authentication-only refusals.
	if m.Auth, err = tx.measureAuth(ctx, filings, opts); err != nil {
		return m, err
	}

	// 3. Recency.
	if m.RecencyAddress, m.RecencyDomain, err = tx.measureRecency(ctx, filings, own, now, opts.RecentDays); err != nil {
		return m, err
	}

	// 4 and 5.
	msgs, earliestOpen, err := tx.measureMessages(ctx, m.WindowStart)
	if err != nil {
		return m, err
	}
	var userEvents []placement.Event
	for _, f := range filings {
		if f.At.Before(m.Split) {
			userEvents = append(userEvents, placement.Event{From: f.From, Area: f.AreaID, At: f.At})
		}
	}
	m.UserEvidence = len(userEvents)
	m.LeftAlone, m.LeftAloneAny, m.LeftAloneCover = measureLeftAlone(msgs, userEvents, own, m.Split, now, earliestOpen)
	m.Archived, m.ArchivedCover = measureArchivedUnread(msgs, own, m.Split, now, m.Primary)

	// 6. List-Id.
	if m.ListID, m.ListIDAddresses, m.ListIDCensus, err = tx.measureListID(ctx); err != nil {
		return m, err
	}
	return m, nil
}

// measureAreas reads the areas directly (ListAreas would materialise a fresh
// account's default plan) and picks the one section 5 calls Primary.
func (tx *UserTx) measureAreas(ctx context.Context) (names map[ulid.ULID]string, primary ulid.ULID, primaryName string, err error) {
	rows, err := tx.tx.Query(ctx,
		`SELECT id, name FROM app.areas WHERE user_id = $1 ORDER BY position, id`, tx.userID.String())
	if err != nil {
		return nil, primary, "", fmt.Errorf("store: placement measure (areas): %w", err)
	}
	defer rows.Close()
	names = map[ulid.ULID]string{}
	var first ulid.ULID
	for rows.Next() {
		var idStr, name string
		if err := rows.Scan(&idStr, &name); err != nil {
			return nil, primary, "", fmt.Errorf("store: placement measure (scan area): %w", err)
		}
		id, err := ulid.Parse(idStr)
		if err != nil {
			return nil, primary, "", fmt.Errorf("store: placement measure: area id %q: %w", idStr, err)
		}
		names[id] = name
		if first == (ulid.ULID{}) {
			first, primaryName = id, name
		}
		if strings.EqualFold(strings.TrimSpace(name), "primary") {
			primary, primaryName = id, name
		}
	}
	if primary == (ulid.ULID{}) {
		primary = first // the first area by position; primaryName already names it
	}
	return names, primary, primaryName, rows.Err()
}

// ── section 2 ────────────────────────────────────────────────────────────────

// measureAuth is section 2. It scans the mailbox the way the placement backfill
// does (the same held test, the same key decision over the same evidence) but
// keeps the messages learned placement would have been confident OR thin about,
// and asks of each whether the arrival-time sender checks refuse it, and for
// what. The checks are senderChecks, the one function ingest and the backfill
// use; in exact mode the sender is built from the message's own raw headers
// exactly as placement-backfill builds it.
func (tx *UserTx) measureAuth(ctx context.Context, filings []placementFiling, opts PlacementMeasureOptions) (AuthRefusals, error) {
	out := AuthRefusals{Exact: opts.Senders != nil}
	byAddr, byDomain := aggregatePlacementEvidence(filings)
	corrections, err := tx.placementCorrections(ctx, nil, time.Time{})
	if err != nil {
		return out, err
	}
	own, err := tx.ownIdentityDomains(ctx)
	if err != nil {
		return out, err
	}
	sd, err := tx.SenderDomainsFor(ctx)
	if err != nil {
		return out, err
	}
	warn := senderwarn.NewIndex(sd.Own, sd.Corresponded)
	areaLabel, _, err := tx.areaOwnLabels(ctx)
	if err != nil {
		return out, err
	}

	decided := []string{LabelByDesk, LabelBySetup}
	rows, err := tx.tx.Query(ctx, `
		SELECT m.id, COALESCE(m.from_address::text, ''), m.auth_fail, m.content_hash, m.received_at,
		       `+providerDatedExpr("m")+`
		  FROM app.messages m
		 WHERE m.user_id = $1 AND m.trashed_at IS NULL AND m.vanished_at IS NULL AND NOT m.is_draft
		   AND m.from_address IS NOT NULL
		   AND NOT (m.id IN (SELECT hml.message_id
		                       FROM app.message_labels hml
		                       JOIN app.area_feeds hf ON hf.user_id = hml.user_id AND hf.label_id = hml.label_id
		                      WHERE hml.user_id = $1 AND NOT (hml.applied_by = ANY($2))))
		 ORDER BY m.received_at DESC, m.id DESC`, tx.userID.String(), decided)
	if err != nil {
		return out, fmt.Errorf("store: placement measure (auth scan): %w", err)
	}
	type cand struct {
		c       PlacementCandidate
		dec     placement.Decision
		area    ulid.ULID
		authBad bool
	}
	var queue []cand
	for rows.Next() {
		var (
			idStr, from   string
			authFail      bool
			hash          []byte
			received      time.Time
			providerDated bool
		)
		if err := rows.Scan(&idStr, &from, &authFail, &hash, &received, &providerDated); err != nil {
			rows.Close()
			return out, fmt.Errorf("store: placement measure (auth scan row): %w", err)
		}
		addrKey, domainKey, ok := placement.KeysFor(from)
		if !ok {
			continue
		}
		addrEv, domainEv := byAddr[addrKey.Value], byDomain[domainKey.Value]
		addrEv.LatestCorrection = corrections[addrKey]
		domainEv.LatestCorrection = corrections[domainKey]
		d := placement.Decide(addrKey, domainKey, addrEv, domainEv, own)
		if !d.Act && d.Reason != placement.Thin {
			continue
		}
		area := d.Area
		if !d.Act {
			ev := addrEv
			kind := placement.KeyAddress
			if d.Key.Kind == placement.KeyDomain {
				ev, kind = domainEv, placement.KeyDomain
			}
			v := placement.Judge(kind, ev)
			if v.Area == nil {
				continue
			}
			area = *v.Area
		}
		id, err := ulid.Parse(idStr)
		if err != nil {
			rows.Close()
			return out, fmt.Errorf("store: placement measure: message id %q: %w", idStr, err)
		}
		queue = append(queue, cand{
			c:   PlacementCandidate{MessageID: id, ContentHash: hash, From: from, ReceivedAt: TrustTime(received, providerDated)},
			dec: d, area: area, authBad: !(placement.SenderAuth{AuthFail: authFail}).Authenticated(),
		})
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return out, fmt.Errorf("store: placement measure (auth scan): %w", err)
	}
	out.Population = len(queue)
	if len(queue) > opts.AuthScanMax {
		queue, out.Truncated = queue[:opts.AuthScanMax], true
	}

	type keyArea struct {
		key  placement.Key
		area ulid.ULID
	}
	type tally struct {
		bucket *AuthBucket
		keys   map[keyArea]int
	}
	conf := tally{&out.Confident, map[keyArea]int{}}
	thin := tally{&out.Thin, map[keyArea]int{}}
	for _, q := range queue {
		t := thin
		if q.dec.Act {
			t = conf
		}
		var refusal SenderChecks
		if opts.Senders != nil {
			s, err := opts.Senders(ctx, q.c)
			if err != nil {
				if ctx.Err() != nil {
					return out, ctx.Err()
				}
				out.Unreadable++
				continue
			}
			refusal = senderChecks(s, warn)
		}
		out.Read++
		t.bucket.Messages++
		switch {
		case opts.Senders == nil:
			// Stored flag only: a message whose auth_fail is set is refused for
			// authentication; nothing else can be said without the headers.
			if q.authBad {
				t.bucket.AuthOnly++
				t.keys[keyArea{q.dec.Key, q.area}]++
			} else {
				t.bucket.OK++
			}
		case !refusal.Authenticated && !refusal.Ambiguous && len(refusal.Warnings) == 0:
			t.bucket.AuthOnly++
			t.keys[keyArea{q.dec.Key, q.area}]++
		case !refusal.Authenticated:
			t.bucket.AuthAlso++
		case refusal.Refusal != "":
			t.bucket.OtherOnly++
		default:
			t.bucket.OK++
		}
	}
	for _, t := range []tally{conf, thin} {
		for ka, n := range t.keys {
			t.bucket.AuthOnlyKeys++
			label, ok := areaLabel[ka.area]
			if !ok {
				continue
			}
			covered, err := tx.enabledRuleFiles(ctx, ka.key.Value, label)
			if err != nil {
				return out, err
			}
			if covered {
				t.bucket.CoveredKeys++
				t.bucket.CoveredMessages += n
			}
		}
	}
	return out, nil
}

// ── section 3 ────────────────────────────────────────────────────────────────

// measureRecency is section 3: per key kind, over the user's own filings (the
// same rows learned placement's evidence is made of), the mixed keys whose
// newest filings are a run into one area.
func (tx *UserTx) measureRecency(ctx context.Context, filings []placementFiling, own map[string]bool, now time.Time, recentDays int) (addr, dom placement.Recency, err error) {
	byAddr := map[string][]placement.TimedFiling{}
	byDom := map[string][]placement.TimedFiling{}
	for _, f := range filings {
		a, d, ok := placement.KeysFor(f.From)
		if !ok {
			continue
		}
		tf := placement.TimedFiling{Area: f.AreaID, At: f.At}
		byAddr[a.Value] = append(byAddr[a.Value], tf)
		if !placement.DomainFallbackBlocked(d.Value, own) {
			byDom[d.Value] = append(byDom[d.Value], tf)
		}
	}
	// Messages received in the window, by address and by domain.
	rows, err := tx.tx.Query(ctx, `
		SELECT lower(btrim(from_address::text)), count(*)
		  FROM app.messages
		 WHERE user_id = $1 AND trashed_at IS NULL AND NOT is_draft AND from_address IS NOT NULL
		   AND received_at >= $2 AND received_at <= $3
		 GROUP BY 1`, tx.userID.String(), now.AddDate(0, 0, -recentDays), now)
	if err != nil {
		return addr, dom, fmt.Errorf("store: placement measure (recent): %w", err)
	}
	defer rows.Close()
	recentAddr, recentDom := map[string]int{}, map[string]int{}
	for rows.Next() {
		var from string
		var n int
		if err := rows.Scan(&from, &n); err != nil {
			return addr, dom, fmt.Errorf("store: placement measure (recent scan): %w", err)
		}
		a, d, ok := placement.KeysFor(from)
		if !ok {
			continue
		}
		recentAddr[a.Value] += n
		recentDom[d.Value] += n
	}
	if err := rows.Err(); err != nil {
		return addr, dom, fmt.Errorf("store: placement measure (recent): %w", err)
	}
	return placement.MeasureRecency(byAddr, recentAddr, placement.RecencyRuns),
		placement.MeasureRecency(byDom, recentDom, placement.RecencyRuns), nil
}

// ── sections 4 and 5: the per-message facts ──────────────────────────────────

// measureMessages reads the facts of every non-draft message created at or
// after since: its sender, flags, area-own-label rows (who applied each, and
// when) and its first `open` and `archive_unread` events. Three queries joined
// in Go, each plain; the event times are the user's own (the web client's
// session-only signals, never an API key's).
func (tx *UserTx) measureMessages(ctx context.Context, since time.Time) (map[ulid.ULID]*measureMessage, time.Time, error) {
	msgs := map[ulid.ULID]*measureMessage{}
	rows, err := tx.tx.Query(ctx, `
		SELECT m.id, lower(btrim(m.from_address::text)), m.created_at, m.is_seen,
		       (m.moved_mailbox_id IS NOT NULL OR m.desk_archived_at IS NOT NULL), m.trashed_at IS NOT NULL
		  FROM app.messages m
		 WHERE m.user_id = $1 AND NOT m.is_draft AND m.from_address IS NOT NULL AND m.created_at >= $2`,
		tx.userID.String(), since)
	if err != nil {
		return nil, time.Time{}, fmt.Errorf("store: placement measure (messages): %w", err)
	}
	for rows.Next() {
		var idStr string
		mm := &measureMessage{}
		if err := rows.Scan(&idStr, &mm.From, &mm.Created, &mm.Seen, &mm.Archived, &mm.Trashed); err != nil {
			rows.Close()
			return nil, time.Time{}, fmt.Errorf("store: placement measure (message row): %w", err)
		}
		id, err := ulid.Parse(idStr)
		if err != nil {
			rows.Close()
			return nil, time.Time{}, fmt.Errorf("store: placement measure: message id %q: %w", idStr, err)
		}
		msgs[id] = mm
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, time.Time{}, fmt.Errorf("store: placement measure (messages): %w", err)
	}

	// Area own-label rows: only a label that IS an area's own label names where
	// a message sits (a topic label that merely feeds an area does not).
	lrows, err := tx.tx.Query(ctx, `
		SELECT ml.message_id, a.id, ml.applied_by, ml.created_at
		  FROM app.message_labels ml
		  JOIN app.areas a ON a.user_id = ml.user_id AND a.label_id = ml.label_id
		  JOIN app.messages m ON m.user_id = ml.user_id AND m.id = ml.message_id
		 WHERE ml.user_id = $1 AND m.created_at >= $2
		 ORDER BY ml.created_at, a.id`, tx.userID.String(), since)
	if err != nil {
		return nil, time.Time{}, fmt.Errorf("store: placement measure (labels): %w", err)
	}
	for lrows.Next() {
		var mid, aid string
		var l measureLabel
		if err := lrows.Scan(&mid, &aid, &l.By, &l.At); err != nil {
			lrows.Close()
			return nil, time.Time{}, fmt.Errorf("store: placement measure (label row): %w", err)
		}
		m, err1 := ulid.Parse(mid)
		a, err2 := ulid.Parse(aid)
		if err1 != nil || err2 != nil {
			lrows.Close()
			return nil, time.Time{}, fmt.Errorf("store: placement measure: label ids %q/%q", mid, aid)
		}
		l.Area = a
		if mm := msgs[m]; mm != nil {
			mm.Labels = append(mm.Labels, l)
		}
	}
	lrows.Close()
	if err := lrows.Err(); err != nil {
		return nil, time.Time{}, fmt.Errorf("store: placement measure (labels): %w", err)
	}

	// The user's feedback events: the first of each kind per message.
	var earliestOpen time.Time
	frows, err := tx.tx.Query(ctx, `
		SELECT message_id, action, min(created_at)
		  FROM app.feedback_events
		 WHERE user_id = $1 AND message_id IS NOT NULL AND action IN ('open', 'archive_unread')
		 GROUP BY message_id, action`, tx.userID.String())
	if err != nil {
		return nil, time.Time{}, fmt.Errorf("store: placement measure (feedback): %w", err)
	}
	for frows.Next() {
		var mid, action string
		var at time.Time
		if err := frows.Scan(&mid, &action, &at); err != nil {
			frows.Close()
			return nil, time.Time{}, fmt.Errorf("store: placement measure (feedback row): %w", err)
		}
		if action == FeedbackActionOpen && (earliestOpen.IsZero() || at.Before(earliestOpen)) {
			earliestOpen = at
		}
		m, err := ulid.Parse(mid)
		if err != nil {
			frows.Close()
			return nil, time.Time{}, fmt.Errorf("store: placement measure: message id %q: %w", mid, err)
		}
		mm := msgs[m]
		if mm == nil {
			continue
		}
		t := at
		if action == FeedbackActionOpen {
			mm.OpenedAt = &t
		} else {
			mm.ArchUnread = &t
		}
	}
	frows.Close()
	if err := frows.Err(); err != nil {
		return nil, time.Time{}, fmt.Errorf("store: placement measure (feedback): %w", err)
	}
	return msgs, earliestOpen, nil
}

// onlyDeskArea returns the area of a message whose area-own-label rows are all
// the desk's and name exactly one area: a message the desk placed by its own
// verdict, and nobody (user, rule, learned placement, an API key, day-one
// setup) has touched since.
func onlyDeskArea(ls []measureLabel) (measureLabel, bool) {
	if len(ls) == 0 {
		return measureLabel{}, false
	}
	first := ls[0]
	for _, l := range ls {
		if l.By != LabelByDesk || l.Area != first.Area {
			return measureLabel{}, false
		}
	}
	return first, true
}

// currentArea is where a message sits for section 5: its newest area-own-label
// row's area (the user's re-file is the newest), or false for no area label.
func currentArea(ls []measureLabel) (ulid.ULID, bool) {
	if len(ls) == 0 {
		return ulid.ULID{}, false
	}
	return ls[len(ls)-1].Area, true
}

// measureLeftAlone is section 4. DEFINITIONS, as built:
//
//   - EVENT: a message the desk placed by its own verdict (onlyDeskArea) whose
//     placement is at least MeasureLeftAloneDays old by the split day, which
//     the user OPENED (an `open` event) no earlier than the placement and no
//     later than the window after it, and which is STILL where the desk put it
//     (a move would have swapped the label). It teaches at placement + window.
//     LeftAloneAny drops the opened condition.
//   - OUTCOME: a message that arrived on or after the split day and is old
//     enough to be judged by now. Filed-to is the area of the user's own
//     filing of it, if any; a rule's or learned placement's label excludes it
//     (counted), because the real feature would not have acted on it.
//   - EXISTING EVIDENCE: the user's own filings before the split, so "new
//     reach" means a key the user's filings did not already cover.
func measureLeftAlone(msgs map[ulid.ULID]*measureMessage, userEvents []placement.Event, own map[string]bool,
	split, now, earliestOpen time.Time) (opened, notMoved placement.LeftAloneResult, cov LeftAloneCoverage) {
	window := time.Duration(MeasureLeftAloneDays) * 24 * time.Hour
	cov.EarliestOpen = earliestOpen
	var evOpened, evAny []placement.Event
	var outcomes []placement.LeftAloneOutcome
	ids := make([]ulid.ULID, 0, len(msgs))
	for id := range msgs {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i].Compare(ids[j]) < 0 })
	for _, id := range ids {
		mm := msgs[id]
		if mm.Trashed {
			continue
		}
		// Evidence side.
		if l, ok := onlyDeskArea(mm.Labels); ok && !l.At.Add(window).After(split) {
			cov.DeskPlaced++
			ev := placement.Event{From: mm.From, Area: l.Area, At: l.At.Add(window)}
			evAny = append(evAny, ev)
			if mm.OpenedAt != nil && !mm.OpenedAt.Before(l.At) && !mm.OpenedAt.After(l.At.Add(window)) {
				cov.Opened++
				evOpened = append(evOpened, ev)
			}
		} else if !mm.Created.Add(window).After(split) {
			for _, l := range mm.Labels {
				if l.By == LabelByUser && l.At.Sub(mm.Created) > window {
					cov.UnobservableMoved++
					break
				}
			}
		}
		// Outcome side: arrived on or after the split, and old enough to judge.
		if mm.Created.Before(split) || mm.Created.Add(window).After(now) {
			continue
		}
		var o placement.LeftAloneOutcome
		o.From = mm.From
		excluded := false
		for _, l := range mm.Labels {
			switch l.By {
			case LabelByRule:
				cov.ExcludedRule++
				excluded = true
			case LabelByLearned:
				cov.ExcludedLearned++
				excluded = true
			case LabelByUser:
				a := l.Area
				o.FiledTo = &a
			}
			if excluded {
				break
			}
		}
		if excluded {
			continue
		}
		outcomes = append(outcomes, o)
	}
	cov.Outcomes = len(outcomes)
	opened = placement.EvaluateLeftAlone(evOpened, userEvents, outcomes, own)
	notMoved = placement.EvaluateLeftAlone(evAny, userEvents, outcomes, own)
	return opened, notMoved, cov
}

// measureArchivedUnread is section 5. DEFINITIONS, as built:
//
//   - EVENT: an `archive_unread` event (the user archived mail they had not
//     read; the web client's own, never an API key's) dated before the split, on
//     a message that carries an area label naming its AREA OF ORIGIN (the
//     message's newest area-own-label row now: archiving does not remove it).
//   - OUTCOME: a message that arrived on or after the split day, is old enough
//     to be judged, and sits in Primary. Its class: archived unread again; opened
//     (an `open` event, or marked seen); archived or trashed with neither; or
//     kept (still there, unopened).
func measureArchivedUnread(msgs map[ulid.ULID]*measureMessage, own map[string]bool, split, now time.Time,
	primary ulid.ULID) (placement.ArchiveResult, ArchivedCoverage) {
	var cov ArchivedCoverage
	window := time.Duration(MeasureLeftAloneDays) * 24 * time.Hour
	var events []placement.Event
	var outcomes []placement.ArchiveOutcome
	ids := make([]ulid.ULID, 0, len(msgs))
	for id := range msgs {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i].Compare(ids[j]) < 0 })
	for _, id := range ids {
		mm := msgs[id]
		area, hasArea := currentArea(mm.Labels)
		if mm.ArchUnread != nil && mm.ArchUnread.Before(split) {
			if !hasArea {
				cov.EventsNoArea++
			} else {
				events = append(events, placement.Event{From: mm.From, Area: area, At: *mm.ArchUnread})
			}
		}
		if mm.Created.Before(split) || mm.Created.Add(window).After(now) || !hasArea || area != primary || primary == (ulid.ULID{}) {
			continue
		}
		cls := placement.KeptUnopened
		switch {
		case mm.ArchUnread != nil:
			cls = placement.ArchivedUnread
		case mm.OpenedAt != nil || mm.Seen:
			cls = placement.Opened
		case mm.Archived || mm.Trashed:
			cls = placement.ArchivedOther
		}
		outcomes = append(outcomes, placement.ArchiveOutcome{From: mm.From, Class: cls})
	}
	cov.PrimaryOutcomes = len(outcomes)
	return placement.EvaluateArchivedUnread(events, outcomes, own), cov
}

// ── section 6 ────────────────────────────────────────────────────────────────

// measureListID is section 6: how many sending addresses carry more than one
// List-Id, and how many of the user's mixed addresses a List-Id key would
// separate. list_id_key is ADR-055's stored form; a message the recipients
// backfill has not reached has NULL there, and the ceiling is then a lower
// bound (the census says how many are filled).
func (tx *UserTx) measureListID(ctx context.Context) (placement.ListIDCeiling, int, RecipientCensus, error) {
	var zero placement.ListIDCeiling
	census, err := tx.RecipientCensus(ctx)
	if err != nil {
		return zero, 0, census, err
	}
	rows, err := tx.tx.Query(ctx, `
		SELECT lower(btrim(from_address::text))
		  FROM app.messages
		 WHERE user_id = $1 AND trashed_at IS NULL AND NOT is_draft AND from_address IS NOT NULL
		   AND COALESCE(list_id_key, '') <> ''
		 GROUP BY 1
		HAVING count(DISTINCT list_id_key) > 1`, tx.userID.String())
	if err != nil {
		return zero, 0, census, fmt.Errorf("store: placement measure (list ids): %w", err)
	}
	multi := map[string]bool{}
	for rows.Next() {
		var a string
		if err := rows.Scan(&a); err != nil {
			rows.Close()
			return zero, 0, census, fmt.Errorf("store: placement measure (list id scan): %w", err)
		}
		multi[a] = true
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return zero, 0, census, fmt.Errorf("store: placement measure (list ids): %w", err)
	}

	frows, err := tx.tx.Query(ctx, `
		SELECT lower(btrim(m.from_address::text)), a.id, m.list_id_key
		  FROM app.message_labels ml
		  JOIN app.areas a ON a.user_id = ml.user_id AND a.label_id = ml.label_id
		  JOIN app.messages m ON m.user_id = ml.user_id AND m.id = ml.message_id
		 WHERE ml.user_id = $1 AND ml.applied_by = $2 AND m.trashed_at IS NULL AND m.from_address IS NOT NULL`,
		tx.userID.String(), LabelByUser)
	if err != nil {
		return zero, 0, census, fmt.Errorf("store: placement measure (list filings): %w", err)
	}
	defer frows.Close()
	var fs []placement.ListFiling
	for frows.Next() {
		var from, aid string
		var list *string
		if err := frows.Scan(&from, &aid, &list); err != nil {
			return zero, 0, census, fmt.Errorf("store: placement measure (list filing scan): %w", err)
		}
		a, err := ulid.Parse(aid)
		if err != nil {
			return zero, 0, census, fmt.Errorf("store: placement measure: area id %q: %w", aid, err)
		}
		fs = append(fs, placement.ListFiling{Address: from, Area: a, ListID: list})
	}
	if err := frows.Err(); err != nil {
		return zero, 0, census, fmt.Errorf("store: placement measure (list filings): %w", err)
	}
	return placement.MeasureListID(fs, multi), len(multi), census, nil
}
