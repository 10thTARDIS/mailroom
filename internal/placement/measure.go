// SPDX-License-Identifier: AGPL-3.0-only

package placement

// MEASURING BEFORE CHANGING (#28, #169): the pure arithmetic behind
// `mailroom-admin placement-measure`.
//
// Nothing here is a decision about where mail goes. These functions only count,
// over rows internal/store gathers, what four candidate changes WOULD have done
// if they had been switched on: a recency rule for mixed senders, "the user
// opened it and left it" as weak evidence, "the user archived it unread" as weak
// evidence, and keying by List-Id. They are pure (no database, no clock of
// their own) so each can be tested against hand-computed numbers, and so the
// store cannot quietly change what a number means.
//
// TWO RULES THESE FUNCTIONS HOLD, from CLAUDE.md's measurement discipline:
//
//   - NO FIXED POINT. A signal built from the desk's own decisions can only
//     look right about itself. Evidence and outcome are therefore split in
//     time: events judged before a day D teach, and only messages that arrived
//     on or after D are scored. The caller chooses D and every report prints it.
//   - THE SAME GATE MATHS AS THE LIVE FEATURE. A would-have-been precision is
//     Gate(kept, corrected): the same 95% Wilson lower bound and the same
//     resolved-count floor learned placement's correction gate uses, so a number
//     here can be read against the gate's thresholds rather than a point
//     estimate.

import (
	"sort"
	"time"

	"github.com/oklog/ulid/v2"
)

// ── recency for mixed keys ───────────────────────────────────────────────────

// TimedFiling is one of the user's own filings of a key's mail into an area.
type TimedFiling struct {
	Area ulid.ULID
	At   time.Time
}

// RecencyRuns are the run lengths the recency measurement reports.
var RecencyRuns = []int{2, 3, 5}

// TailRun is the key's NEWEST consecutive run: the area of its newest filing
// and how many filings into that area are strictly newer than the newest
// filing into any other area.
//
// STRICTLY NEWER, so the answer never depends on an order the data does not
// have. A bulk session files many messages in one instant; ordering those by id
// would make "the last three went to Updates" an accident of the ulid sort.
// With ties against the other area never counting toward the run, a key whose
// newest instant holds filings into two areas has no tail area (run 0), and a
// tied filing never lengthens a run it cannot be proved to belong to.
func TailRun(fs []TimedFiling) (area ulid.ULID, run int) {
	if len(fs) == 0 {
		return ulid.ULID{}, 0
	}
	var newest time.Time
	for _, f := range fs {
		if f.At.After(newest) {
			newest = f.At
		}
	}
	areasAtNewest := map[ulid.ULID]bool{}
	for _, f := range fs {
		if f.At.Equal(newest) {
			areasAtNewest[f.Area] = true
		}
	}
	if len(areasAtNewest) != 1 {
		return ulid.ULID{}, 0
	}
	for a := range areasAtNewest {
		area = a
	}
	var lastOther time.Time
	for _, f := range fs {
		if f.Area != area && f.At.After(lastOther) {
			lastOther = f.At
		}
	}
	for _, f := range fs {
		if f.Area == area && f.At.After(lastOther) {
			run++
		}
	}
	return area, run
}

// RecencyRow is one run length's answer.
type RecencyRow struct {
	N int
	// Keys is the number of MIXED keys whose newest filings are a run of at
	// least N into one area.
	Keys int
	// Recent is how many messages those keys received in the recent window.
	Recent int
}

// Recency is the whole recency measurement for one key kind.
type Recency struct {
	// Keys is every key with at least one filing; Mixed those filed into more
	// than one area (the keys a recency rule is about).
	Keys, Mixed int
	// MixedRecent is the recent-window mail of ALL mixed keys: the denominator
	// the rows' Recent figures read against.
	MixedRecent int
	Rows        []RecencyRow
}

// MeasureRecency counts, for each run length in ns, the mixed keys that end in a
// run of that length, and the recent mail they received. filings is keyed by key
// value; recent is the recent-window message count by key value (a key absent
// from it received none).
func MeasureRecency(filings map[string][]TimedFiling, recent map[string]int, ns []int) Recency {
	out := Recency{}
	for _, n := range ns {
		out.Rows = append(out.Rows, RecencyRow{N: n})
	}
	keys := make([]string, 0, len(filings))
	for k := range filings {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fs := filings[k]
		if len(fs) == 0 {
			continue
		}
		out.Keys++
		areas := map[ulid.ULID]bool{}
		for _, f := range fs {
			areas[f.Area] = true
		}
		if len(areas) < 2 {
			continue
		}
		out.Mixed++
		out.MixedRecent += recent[k]
		_, run := TailRun(fs)
		for i := range out.Rows {
			if run >= out.Rows[i].N {
				out.Rows[i].Keys++
				out.Rows[i].Recent += recent[k]
			}
		}
	}
	return out
}

// ── events, and the keys they would teach ────────────────────────────────────

// Event is one thing a signal observed about a sender's message: the From
// address, the area it points at, and when it became known.
type Event struct {
	From string
	Area ulid.ULID
	At   time.Time
}

// AggregateEvents folds events into per-address and per-domain Evidence, the
// same shape learned placement builds from the user's filings, so Judge and
// Decide read them unchanged.
func AggregateEvents(events []Event) (byAddr, byDomain map[string]Evidence) {
	byAddr = map[string]Evidence{}
	byDomain = map[string]Evidence{}
	domainAddrs := map[string]map[string]bool{}
	add := func(m map[string]Evidence, key string, e Event) {
		ev := m[key]
		if ev.Filings == nil {
			ev.Filings = map[ulid.ULID]int{}
		}
		ev.Filings[e.Area]++
		if e.At.After(ev.LatestFiling) {
			ev.LatestFiling = e.At
		}
		m[key] = ev
	}
	for _, e := range events {
		a, d, ok := KeysFor(e.From)
		if !ok {
			continue
		}
		add(byAddr, a.Value, e)
		add(byDomain, d.Value, e)
		if domainAddrs[d.Value] == nil {
			domainAddrs[d.Value] = map[string]bool{}
		}
		domainAddrs[d.Value][a.Value] = true
	}
	for k, ev := range byAddr {
		ev.Addresses = 1
		byAddr[k] = ev
	}
	for k, ev := range byDomain {
		ev.Addresses = len(domainAddrs[k])
		byDomain[k] = ev
	}
	return byAddr, byDomain
}

// Teacher is a set of events read exactly as learned placement reads the user's
// filings: the thresholds (MinFilings, MinDomainAddresses), the agreement rule,
// the address-before-domain precedence and the freemail/own-domain block are
// Judge's and Decide's own.
type Teacher struct {
	addr, dom map[string]Evidence
	own       map[string]bool
}

// NewTeacher builds a Teacher from events. own is the user's own domains.
func NewTeacher(events []Event, own map[string]bool) Teacher {
	a, d := AggregateEvents(events)
	return Teacher{addr: a, dom: d, own: own}
}

// Decide is placement.Decide for the message's From, over the teacher's
// evidence. ok is false when the From has no usable key.
func (t Teacher) Decide(from string) (Decision, bool) {
	a, d, ok := KeysFor(from)
	if !ok {
		return Decision{}, false
	}
	return Decide(a, d, t.addr[a.Value], t.dom[d.Value], t.own), true
}

// HasEvidence reports whether the teacher holds any event for either of the
// From's keys.
func (t Teacher) HasEvidence(from string) bool {
	a, d, ok := KeysFor(from)
	if !ok {
		return false
	}
	return t.addr[a.Value].Total() > 0 || t.dom[d.Value].Total() > 0
}

// KeyCounts is keys by kind.
type KeyCounts struct{ Addresses, Domains int }

// Total is both kinds together.
func (k KeyCounts) Total() int { return k.Addresses + k.Domains }

// KeyTally is the keys a teacher would act on, per area, and why the rest would
// not, per kind.
type KeyTally struct {
	// Confident counts keys Judge calls Confident, by the area they point at.
	// A domain on the freemail/own block is never counted, as in the report.
	Confident map[ulid.ULID]KeyCounts
	// New is the subset of Confident with no event in the other teacher passed
	// to Tally (keys the other source had nothing on).
	New map[ulid.ULID]KeyCounts
	// Reasons counts every key by verdict, per kind.
	Reasons map[KeyKind]map[Reason]int
}

// Tally counts the teacher's keys. other, when given, is a second teacher whose
// evidence marks a key as not new: a key is NEW when other holds no event on it.
func (t Teacher) Tally(other *Teacher) KeyTally {
	out := KeyTally{
		Confident: map[ulid.ULID]KeyCounts{}, New: map[ulid.ULID]KeyCounts{},
		Reasons: map[KeyKind]map[Reason]int{KeyAddress: {}, KeyDomain: {}},
	}
	bump := func(m map[ulid.ULID]KeyCounts, a ulid.ULID, kind KeyKind) {
		c := m[a]
		if kind == KeyAddress {
			c.Addresses++
		} else {
			c.Domains++
		}
		m[a] = c
	}
	one := func(kind KeyKind, evs map[string]Evidence, otherEvs map[string]Evidence) {
		for k, ev := range evs {
			if kind == KeyDomain && DomainFallbackBlocked(k, t.own) {
				out.Reasons[kind][DomainBlocked]++
				continue
			}
			v := Judge(kind, ev)
			out.Reasons[kind][v.Reason]++
			if v.Reason != Confident {
				continue
			}
			bump(out.Confident, *v.Area, kind)
			if other == nil || otherEvs[k].Total() == 0 {
				bump(out.New, *v.Area, kind)
			}
		}
	}
	var oa, od map[string]Evidence
	if other != nil {
		oa, od = other.addr, other.dom
	}
	one(KeyAddress, t.addr, oa)
	one(KeyDomain, t.dom, od)
	return out
}

// ── "the user opened it and left it alone" ───────────────────────────────────

// LeftAloneOutcome is one LATER message (arrived on or after the split day,
// old enough for its outcome to be known): who sent it and where the user filed
// it by hand, if they did.
type LeftAloneOutcome struct {
	From    string
	FiledTo *ulid.ULID
}

// AreaPrecision is a would-have-been precision for one area: of the later
// messages the signal's keys would have placed there, how many the user left
// (kept) and how many they filed into a different area (corrected). Gate is the
// live correction gate's own arithmetic over the pair.
type AreaPrecision struct {
	Messages, Kept, Corrected int
	Gate                      GateStatus
}

func precisionOf(kept, corrected int) AreaPrecision {
	return AreaPrecision{Messages: kept + corrected, Kept: kept, Corrected: corrected,
		Gate: Gate(GateCounts{Kept: kept, Corrected: corrected})}
}

// LeftAloneResult is the whole left-alone measurement for one event set.
type LeftAloneResult struct {
	// Events is how many events taught (EventsByArea: per area they point
	// at), and Keys what they would teach.
	Events       int
	EventsByArea map[ulid.ULID]int
	Keys         KeyTally
	// Areas is the would-have-been precision per taught area, over later mail
	// from NEW keys only (keys the user's own filings, before the split, had
	// nothing on): reach the live feature does not already have.
	Areas map[ulid.ULID]AreaPrecision
	// Total is Areas summed: the one figure per variant.
	Total AreaPrecision
	// Served counts later messages whose sender the user's own filings already
	// decide or have evidence on: excluded from precision, not from view.
	Served int
	// Untaught is every other later message: the base rate.
	Untaught int
	// MovedAnyTaught / MovedAnyUntaught: how many later messages of each group
	// the user filed into ANY area by hand. The base rate of moving at all:
	// a signal that does not beat it has told us nothing.
	MovedAnyTaught, MovedAnyUntaught int
}

// EvaluateLeftAlone scores a left-alone event set.
//
// events are the signal's events (judged before the split); userFilings the
// user's own filings before the split, as events (the evidence learned
// placement already has); outcomes the later messages.
func EvaluateLeftAlone(events, userFilings []Event, outcomes []LeftAloneOutcome, own map[string]bool) LeftAloneResult {
	sig := NewTeacher(events, own)
	usr := NewTeacher(userFilings, own)
	res := LeftAloneResult{Events: len(events), Keys: sig.Tally(&usr), Areas: map[ulid.ULID]AreaPrecision{},
		EventsByArea: map[ulid.ULID]int{}}
	for _, e := range events {
		res.EventsByArea[e.Area]++
	}
	kept, corrected := map[ulid.ULID]int{}, map[ulid.ULID]int{}
	for _, o := range outcomes {
		sd, ok := sig.Decide(o.From)
		if usr.HasEvidence(o.From) {
			res.Served++
			continue
		}
		if !ok || !sd.Act {
			res.Untaught++
			if o.FiledTo != nil {
				res.MovedAnyUntaught++
			}
			continue
		}
		if o.FiledTo != nil {
			res.MovedAnyTaught++
		}
		if o.FiledTo != nil && *o.FiledTo != sd.Area {
			corrected[sd.Area]++
		} else {
			kept[sd.Area]++
		}
	}
	totalK, totalC := 0, 0
	for a := range kept {
		res.Areas[a] = precisionOf(kept[a], corrected[a])
	}
	for a := range corrected {
		res.Areas[a] = precisionOf(kept[a], corrected[a])
	}
	for a := range res.Areas {
		totalK += res.Areas[a].Kept
		totalC += res.Areas[a].Corrected
	}
	res.Total = precisionOf(totalK, totalC)
	return res
}

// ── "the user archived it unread" ────────────────────────────────────────────

// ArchiveClass is what became of a LATER message from a key the signal taught.
type ArchiveClass int

const (
	// ArchivedUnread: the user archived it without opening it again.
	ArchivedUnread ArchiveClass = iota
	// Opened: the user opened it (an open event, or it is marked seen).
	Opened
	// KeptUnopened: still where the desk put it, unopened, not archived.
	KeptUnopened
	// ArchivedOther: archived or trashed with no archive-unread event and no
	// sign it was opened (another client, an API key, a rule).
	ArchivedOther
)

// ArchiveOutcome is one later message in the area that matters (Primary).
type ArchiveOutcome struct {
	From  string
	Class ArchiveClass
}

// ArchiveAreaRow is one area of origin's line.
type ArchiveAreaRow struct {
	Messages                                            int
	ArchivedUnread, Opened, KeptUnopened, ArchivedOther int
	// Strict reads "how often the user later opened or kept a message from the
	// key in Primary": ArchivedUnread / Messages. Engaged reads only the
	// messages the user showed a decision about: ArchivedUnread / (ArchivedUnread
	// + Opened). Both through the gate's Wilson arithmetic.
	Strict, Engaged GateStatus
}

// ArchiveResult is the whole archived-unread measurement.
type ArchiveResult struct {
	Events int
	// Keys is what the events would teach: Confident per area of ORIGIN. A key
	// whose events came from two areas is Mixed and teaches nothing.
	Keys KeyTally
	// Areas is the later-mail outcome per area of origin.
	Areas map[ulid.ULID]ArchiveAreaRow
	// Untaught counts later messages from keys the signal did not teach.
	Untaught int
}

// EvaluateArchivedUnread scores an archived-unread event set. Each event's Area
// is its area of origin.
func EvaluateArchivedUnread(events []Event, outcomes []ArchiveOutcome, own map[string]bool) ArchiveResult {
	sig := NewTeacher(events, own)
	res := ArchiveResult{Events: len(events), Keys: sig.Tally(nil), Areas: map[ulid.ULID]ArchiveAreaRow{}}
	for _, o := range outcomes {
		d, ok := sig.Decide(o.From)
		if !ok || !d.Act {
			res.Untaught++
			continue
		}
		r := res.Areas[d.Area]
		r.Messages++
		switch o.Class {
		case ArchivedUnread:
			r.ArchivedUnread++
		case Opened:
			r.Opened++
		case KeptUnopened:
			r.KeptUnopened++
		default:
			r.ArchivedOther++
		}
		res.Areas[d.Area] = r
	}
	for a, r := range res.Areas {
		r.Strict = Gate(GateCounts{Kept: r.ArchivedUnread, Corrected: r.Messages - r.ArchivedUnread})
		r.Engaged = Gate(GateCounts{Kept: r.ArchivedUnread, Corrected: r.Opened})
		res.Areas[a] = r
	}
	return res
}

// ── List-Id in the key ───────────────────────────────────────────────────────

// ListFiling is one of the user's filings with the message's List-Id key
// (messages.list_id_key). ListID nil means the column is not filled yet (the
// ADR-055 backfill has not reached the message); "" means filled, no List-Id.
type ListFiling struct {
	Address string
	Area    ulid.ULID
	ListID  *string
}

// ListIDCeiling is the most keying by List-Id could do, from filings alone.
type ListIDCeiling struct {
	// Filings is every filing read; Unfilled those whose list_id_key is not
	// filled yet, which this measurement can only treat as "no List-Id" (so the
	// ceiling below is a LOWER bound when Unfilled > 0).
	Filings, Unfilled int
	// MixedAddresses are addresses filed into more than one area.
	MixedAddresses int
	// MultiList is how many of them carry more than one List-Id on their mail
	// (the caller supplies the set: it reads all mail, not only filed mail).
	MultiList int
	// Separated: every (address, List-Id) group is filed into a single area, so
	// a List-Id key would make the address clean. Partial: some groups would be
	// clean and some not. NotHelped: no group is clean.
	Separated, Partial, NotHelped int
	// SubKeysConfident counts the (address, List-Id) groups, among the
	// Separated and Partial addresses, with a List-Id, one area and at least
	// MinFilings filings: the keys List-Id keying would act on.
	SubKeysConfident int
}

// MeasureListID computes the List-Id ceiling. multiList names the addresses
// whose mail carries more than one distinct List-Id.
func MeasureListID(filings []ListFiling, multiList map[string]bool) ListIDCeiling {
	out := ListIDCeiling{Filings: len(filings)}
	type group struct {
		areas map[ulid.ULID]int
	}
	byAddr := map[string]map[string]*group{} // address -> list id -> group
	addrAreas := map[string]map[ulid.ULID]bool{}
	for _, f := range filings {
		id := ""
		if f.ListID == nil {
			out.Unfilled++
		} else {
			id = *f.ListID
		}
		if byAddr[f.Address] == nil {
			byAddr[f.Address] = map[string]*group{}
			addrAreas[f.Address] = map[ulid.ULID]bool{}
		}
		g := byAddr[f.Address][id]
		if g == nil {
			g = &group{areas: map[ulid.ULID]int{}}
			byAddr[f.Address][id] = g
		}
		g.areas[f.Area]++
		addrAreas[f.Address][f.Area] = true
	}
	addrs := make([]string, 0, len(byAddr))
	for a := range byAddr {
		addrs = append(addrs, a)
	}
	sort.Strings(addrs)
	for _, a := range addrs {
		if len(addrAreas[a]) < 2 {
			continue
		}
		out.MixedAddresses++
		if !multiList[a] {
			continue
		}
		out.MultiList++
		pure, total := 0, 0
		for id, g := range byAddr[a] {
			total++
			if len(g.areas) != 1 {
				continue
			}
			pure++
			if id == "" {
				continue
			}
			n := 0
			for _, c := range g.areas {
				n = c
			}
			if n >= MinFilings {
				out.SubKeysConfident++
			}
		}
		switch {
		case pure == total:
			out.Separated++
		case pure > 0:
			out.Partial++
		default:
			out.NotHelped++
		}
	}
	return out
}
