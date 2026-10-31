// SPDX-License-Identifier: AGPL-3.0-only

package nudge

import (
	"math"
	"time"

	"mailroom/internal/kind"
	"mailroom/internal/stats"
)

// Message is one message as the measurement sees it. The row facts come from
// app.messages; the Parsed fields from the message's own headers and body.
type Message struct {
	ID     string // a ULID string: sorting by it is sorting by ingest order
	Thread string // "" = no thread
	At     time.Time
	// Date is the Date header, zero when absent. Audit only: At is the clock.
	Date time.Time
	From string // lower-cased

	// Direction evidence, kept apart so the report can say which signal
	// decided it (per signal, never one total). Outbound is any of the three.
	InSentFolder  bool // provenance in a sent-role mailbox
	ViaSubmission bool // a Mailroom submission reached 'sent'
	FromIdentity  bool // From is one of the user's identities

	MessageID string
	InReplyTo string
	Answered  bool // \Answered: set by whichever client replied, untimed
	Junk      bool
	// ContextOnly marks a message the live nudge reads for its conversation
	// or history but must never nudge itself (it may be trashed, snoozed or
	// outside the window). The report ignores it.
	ContextOnly bool

	// Parsed is false when the blob could not be read or parsed, or was not
	// asked for (inbound mail older than the window). Everything below is the
	// zero value then.
	Parsed   bool
	To       []string
	Cc       []string
	Bcc      []string
	Facts    kind.Facts // HasCorresponded is overwritten by Evaluate (as-of)
	Question bool
	Request  bool
}

// Outbound reports whether the user sent m, on any evidence.
func (m *Message) Outbound() bool { return m.InSentFolder || m.ViaSubmission || m.FromIdentity }

// Config is the measurement's window and horizons.
type Config struct {
	Now   time.Time
	Since time.Time
	// ReplyWithin is the inbound ground-truth horizon: replied within this
	// long counts as "needed a reply". Inbound mail younger than this is
	// censored (its outcome is not known yet) and excluded, and counted.
	ReplyWithin time.Duration
	// InboundNudgeDays are the "haven't replied in N days" delays measured.
	InboundNudgeDays []int
	// OutboundDays are the "no one answered in N days" horizons measured.
	OutboundDays []int
	// LateWithin is how long after a fired outbound nudge an answer still
	// counts as "it really was waiting" (only measured on mail at least this
	// old).
	LateWithin time.Duration
	// Examples is the number of message ids kept per bucket; 0 keeps none.
	Examples int
}

// DefaultConfig is the report's default: 14-day reply horizon, nudges after
// 3 and 7 days inbound, 3/7/14 days outbound, 30-day late-answer horizon.
func DefaultConfig(now time.Time, since time.Duration) Config {
	return Config{
		Now:              now,
		Since:            now.Add(-since),
		ReplyWithin:      14 * day,
		InboundNudgeDays: []int{3, 7},
		OutboundDays:     []int{3, 7, 14},
		LateWithin:       30 * day,
	}
}

const day = 24 * time.Hour

// Rate is Hits out of N.
type Rate struct{ Hits, N int }

// P is the point estimate, NaN when N is zero (no evidence must not read as 0).
func (r Rate) P() float64 {
	if r.N <= 0 {
		return math.NaN()
	}
	return float64(r.Hits) / float64(r.N)
}

// LB is the bottom of the 95% Wilson interval — the number to gate on.
func (r Rate) LB() float64 { return stats.WilsonLowerBound(r.Hits, r.N, stats.Z95) }

// Report is the whole measurement.
type Report struct {
	Since, Now time.Time

	Messages int // every message given
	Outbound int // ...the user sent (any evidence)
	Inbound  int // ...everything else

	// Inbound population accounting: in the window, what was excluded and why.
	InboundInWindow  int
	InboundCensored  int // younger than ReplyWithin: outcome not known yet
	InboundJunk      int
	InboundUnparsed  int // blob unreadable: excluded, so every count is a lower bound
	InboundEvaluable int
	InboundWeeks     float64 // span the evaluable inbound covers, for per-week rates

	InboundVariants []InboundVariant
	// Signals are per-signal reply rates, With and Without, on two
	// populations: every evaluable inbound message, and the "person, to me"
	// population a nudge would actually choose among.
	SignalsAll    []SignalRow
	SignalsPerson []SignalRow

	OutboundInWindow int
	OutboundUnparsed int
	// OutboundAutoOnly counts sent messages whose only answer within
	// ReplyWithin was an automatic one (Auto-Submitted, e.g. a vacation
	// responder) — answers the measurement deliberately does not count.
	OutboundAutoOnly int
	OutboundVariants []OutboundVariant

	Audit Audit
}

// InboundVariant is one candidate "needs reply" rule and its accuracy.
type InboundVariant struct {
	Name string
	Rule string
	// Precision: of the candidates, how many the user replied to (a reply in
	// the thread, or to its Message-ID, within ReplyWithin — or \Answered).
	Precision Rate
	// The two ground-truth signals behind Precision.Hits, separately.
	TimedReplies int // a reply message found within ReplyWithin
	AnsweredOnly int // \Answered with no reply message found in time
	// Recall: of every evaluable inbound message the user replied to, how many
	// this rule would have flagged.
	Recall Rate
	// PerWeek is candidates per week — the rule's raw volume, before any nudge
	// delay takes the replied-to ones out.
	PerWeek  float64
	Nudges   []InboundNudge
	Examples map[string][]string
}

// InboundNudge is what a "you haven't replied in N days" nudge on a rule
// would have done.
type InboundNudge struct {
	AfterDays int
	// Fired: candidates with no reply message within N days and not superseded.
	Fired int
	// Superseded: a later candidate message in the same thread arrived within
	// N days, so the thread's one nudge belongs to that later message.
	Superseded int
	// Of Fired: the user replied later, within ReplyWithin (the nudge pointed
	// at something they did answer — the useful case)...
	LateReplied int
	// ...\Answered with no reply message found, so whether the reply came
	// before day N is unknown...
	AnsweredUntimed int
	// ...or no reply at all. This is the noise bucket — with the caveat that
	// "never replied" also contains the mail the user forgot, which is the
	// mail a nudge is FOR. Behaviour cannot separate the two.
	NoReply int
	PerWeek float64
}

// SignalRow is one signal's reply rate where it is present and where it is
// absent.
type SignalRow struct {
	Name          string
	With, Without Rate
}

// OutboundVariant is one candidate "no one answered" rule.
type OutboundVariant struct {
	Name     string
	Rule     string
	InWindow int
	ByDays   []OutboundDays
	Examples map[string][]string
}

// OutboundDays is an outbound rule at one horizon.
type OutboundDays struct {
	Days int
	// Answered: of the sent messages at least Days old, how many had a human
	// answer (not the user, not automatic) within Days.
	Answered Rate
	// Fired: no answer within Days and not superseded by the user's own later
	// message in the thread within Days. PerWeek is the alert volume.
	Fired      int
	Superseded int
	PerWeek    float64
	// Of the fired ones old enough to observe (LateWithin), how many were
	// answered after day Days but within LateWithin: the thread really was
	// waiting on someone.
	LateObserved int
	LateAnswered int
}

// Audit is how sent mail is represented, and whether threading can be
// trusted as ground truth.
type Audit struct {
	OutboundSentFolder, OutboundSubmission, OutboundIdentity int
	SentFolderNotIdentity                                    int // identity gap: From not a known identity
	IdentityNotSentFolder                                    int // self-sent, or a send not (yet) in Sent
	// The clock check: outbound whose arrival time (the clock every timing
	// here uses) is more than a day from its own Date header — a Sent folder
	// imported in bulk — and outbound with no Date at all.
	OutboundClockSkewOver1d, OutboundNoDate            int
	OutboundInWindow                                   int
	OutboundThreadedWithInbound                        int // shares a thread with an inbound message
	OutboundUnthreaded                                 int
	OutboundWithInReplyTo                              int
	ParentSameThread, ParentOtherThread, ParentMissing int
	// \Answered among evaluable inbound, and whether a reply message exists.
	AnsweredInbound, AnsweredWithReply, AnsweredNoReply int
	// Replies found ONLY through In-Reply-To, not the thread: threading
	// missed them, and a thread-only ground truth would have too.
	RepliesByInReplyToOnly int
}

type inRule struct {
	name, rule string
	pred       func(Features) bool
}

// inboundRules are the candidate "needs reply" rules, broadest first. The
// SHIPPED rule is WaitingOnYou itself, not a restatement of it
// (features_test.go asserts the identity).
var inboundRules = []inRule{
	{"all", "every inbound message (base rate)", func(Features) bool { return true }},
	{"person", "no List-Id, no bulk markers, not Auto-Submitted, not a no-reply sender", Features.Person},
	{"person+to-me", "person, and the user is in To (not only Cc)", func(f Features) bool { return f.Person() && f.MeInTo }},
	{"person+to-me+not-machine", "…and the kind classifier does not call it machine mail", func(f Features) bool { return f.Person() && f.MeInTo && f.NotMachine }},
	{"person+to-me+kind-personal", "…and the kind classifier decides personal", func(f Features) bool { return f.Person() && f.MeInTo && f.Personal }},
	{ShippedInboundRule, "SHIPPED (opt-in): person, to me, and (kind-personal OR a reply)", WaitingOnYou},
	{"person+to-me+corresponded", "person, to me, from someone the user wrote to BEFORE this message", func(f Features) bool { return f.Person() && f.MeInTo && f.Corresponded }},
	{"person+to-me+asks", "person, to me, with a question mark or request phrasing", func(f Features) bool { return f.Person() && f.MeInTo && f.Asks() }},
	{"person+to-me+not-machine+asks", "person, to me, not machine, and asks", func(f Features) bool { return f.Person() && f.MeInTo && f.NotMachine && f.Asks() }},
	{"person+to-me+corresponded+asks", "person, to me, corresponded before, and asks", func(f Features) bool { return f.Person() && f.MeInTo && f.Corresponded && f.Asks() }},
}

// ShippedInboundRule and ShippedOutboundRule name the report variants that
// ARE the live nudges, so the report's output can be read against them.
const (
	ShippedInboundRule  = "person+to-me+(kind-personal|is-reply)"
	ShippedOutboundRule = "to-people"
)

var inboundSignals = []struct {
	name string
	get  func(Features) bool
}{
	{"no-list-id", func(f Features) bool { return f.NoListID }},
	{"not-bulk", func(f Features) bool { return f.NotBulk }},
	{"not-auto-submitted", func(f Features) bool { return f.NotAutomated }},
	{"not-no-reply", func(f Features) bool { return f.NotNoReply }},
	{"me-in-to", func(f Features) bool { return f.MeInTo }},
	{"me-cc-only", func(f Features) bool { return f.MeCcOnly }},
	{"kind-personal", func(f Features) bool { return f.Personal }},
	{"kind-not-machine", func(f Features) bool { return f.NotMachine }},
	{"corresponded-before", func(f Features) bool { return f.Corresponded }},
	{"is-reply", func(f Features) bool { return f.IsReply }},
	{"question-mark", func(f Features) bool { return f.Question }},
	{"request-phrasing", func(f Features) bool { return f.Request }},
}

type outRule struct {
	name, rule string
	pred       func(OutFeatures) bool
}

// outboundRules: the SHIPPED rule is NoOneAnswered itself.
var outboundRules = []outRule{
	{"all", "every message the user sent (base rate)", func(OutFeatures) bool { return true }},
	{ShippedOutboundRule, "SHIPPED: not only to the user, no no-reply recipient, no list/bulk/automated mail in the conversation", NoOneAnswered},
	{"to-people+started", "to-people, and it started the thread", func(f OutFeatures) bool { return f.ToPeople() && f.Started }},
	{"to-people+continued", "to-people, and it continued a thread", func(f OutFeatures) bool { return f.ToPeople() && !f.Started }},
	{"to-people+asks", "to-people, with a question mark or request phrasing", func(f OutFeatures) bool { return f.ToPeople() && f.Asks }},
}

// Evaluate measures every candidate rule against what the user (and their
// correspondents) actually did next. msgs may be in any order.
func Evaluate(msgs []Message, me Me, cfg Config) Report {
	ix := newIndex(msgs, me)
	rep := Report{Since: cfg.Since, Now: cfg.Now, Messages: len(ix.ms)}
	for _, m := range ix.ms {
		if !m.Outbound() {
			rep.Inbound++
			continue
		}
		rep.Outbound++
		if m.InSentFolder {
			rep.Audit.OutboundSentFolder++
		}
		if m.ViaSubmission {
			rep.Audit.OutboundSubmission++
		}
		if m.FromIdentity {
			rep.Audit.OutboundIdentity++
		}
		if m.InSentFolder && !m.FromIdentity {
			rep.Audit.SentFolderNotIdentity++
		}
		if m.FromIdentity && !m.InSentFolder {
			rep.Audit.IdentityNotSentFolder++
		}
		switch {
		case m.Date.IsZero():
			rep.Audit.OutboundNoDate++
		case m.At.Sub(m.Date) > day || m.Date.Sub(m.At) > day:
			rep.Audit.OutboundClockSkewOver1d++
		}
	}
	evalInbound(&rep, ix, me, cfg)
	evalOutbound(&rep, ix, me, cfg)
	return rep
}

func weeks(from, to time.Time) float64 {
	w := to.Sub(from).Hours() / (24 * 7)
	if w <= 0 {
		return math.NaN()
	}
	return w
}

func perWeek(n int, w float64) float64 {
	if math.IsNaN(w) {
		return math.NaN()
	}
	return float64(n) / w
}

func addExample(ex map[string][]string, bucket, id string, limit int) {
	if limit <= 0 || len(ex[bucket]) >= limit {
		return
	}
	ex[bucket] = append(ex[bucket], id)
}

func evalInbound(rep *Report, ix *index, me Me, cfg Config) {
	end := cfg.Now.Add(-cfg.ReplyWithin)
	rep.InboundWeeks = weeks(cfg.Since, end)

	type cand struct {
		m        *Message
		f        Features
		replyAt  time.Time // first reply message, zero if none
		timed    bool      // reply message within ReplyWithin
		replied  bool      // timed || \Answered
		viaIRTOn bool      // the reply was found only through In-Reply-To
	}
	var pop []*cand
	byID := map[*Message]*cand{}
	for _, m := range ix.ms {
		if m.Outbound() || m.At.Before(cfg.Since) {
			continue
		}
		rep.InboundInWindow++
		switch {
		case m.At.After(end):
			rep.InboundCensored++
			continue
		case m.Junk:
			rep.InboundJunk++
			continue
		case !m.Parsed:
			rep.InboundUnparsed++
			continue
		}
		c := &cand{m: m, f: InboundFeatures(m, me, ix.correspondedBefore(m))}
		for _, o := range ix.related(m) {
			if !o.Outbound() {
				continue
			}
			c.replyAt = o.At
			c.viaIRTOn = m.Thread == "" || o.Thread != m.Thread
			break
		}
		c.timed = !c.replyAt.IsZero() && c.replyAt.Sub(m.At) <= cfg.ReplyWithin
		c.replied = c.timed || m.Answered
		if c.timed && c.viaIRTOn {
			rep.Audit.RepliesByInReplyToOnly++
		}
		if m.Answered {
			rep.Audit.AnsweredInbound++
			if c.replyAt.IsZero() {
				rep.Audit.AnsweredNoReply++
			} else {
				rep.Audit.AnsweredWithReply++
			}
		}
		pop = append(pop, c)
		byID[m] = c
	}
	rep.InboundEvaluable = len(pop)

	repliedTotal := 0
	for _, c := range pop {
		if c.replied {
			repliedTotal++
		}
	}

	signalRows := func(filter func(Features) bool) []SignalRow {
		var rows []SignalRow
		for _, s := range inboundSignals {
			row := SignalRow{Name: s.name}
			for _, c := range pop {
				if !filter(c.f) {
					continue
				}
				r := &row.Without
				if s.get(c.f) {
					r = &row.With
				}
				r.N++
				if c.replied {
					r.Hits++
				}
			}
			rows = append(rows, row)
		}
		return rows
	}
	rep.SignalsAll = signalRows(func(Features) bool { return true })
	rep.SignalsPerson = signalRows(func(f Features) bool { return f.Person() && f.MeInTo })

	for _, rule := range inboundRules {
		v := InboundVariant{Name: rule.name, Rule: rule.rule, Examples: map[string][]string{}}
		v.Recall.N = repliedTotal
		for _, c := range pop {
			if !rule.pred(c.f) {
				if c.replied {
					addExample(v.Examples, "missed (replied, not flagged)", c.m.ID, cfg.Examples)
				}
				continue
			}
			v.Precision.N++
			if c.replied {
				v.Precision.Hits++
				v.Recall.Hits++
				if c.timed {
					v.TimedReplies++
				} else {
					v.AnsweredOnly++
				}
				addExample(v.Examples, "flagged, replied", c.m.ID, cfg.Examples)
			} else {
				addExample(v.Examples, "flagged, not replied", c.m.ID, cfg.Examples)
			}
		}
		v.PerWeek = perWeek(v.Precision.N, rep.InboundWeeks)

		for _, n := range cfg.InboundNudgeDays {
			after := time.Duration(n) * day
			nu := InboundNudge{AfterDays: n}
			for _, c := range pop {
				if !rule.pred(c.f) {
					continue
				}
				if !c.replyAt.IsZero() && c.replyAt.Sub(c.m.At) <= after {
					continue // replied before the nudge was due
				}
				superseded := false
				for _, o := range ix.related(c.m) {
					if o.At.Sub(c.m.At) > after {
						break
					}
					if oc, ok := byID[o]; ok && rule.pred(oc.f) {
						superseded = true
						break
					}
				}
				if superseded {
					nu.Superseded++
					continue
				}
				nu.Fired++
				switch {
				case c.timed:
					nu.LateReplied++
				case c.m.Answered:
					nu.AnsweredUntimed++
				default:
					nu.NoReply++
				}
				if n == cfg.InboundNudgeDays[0] {
					addExample(v.Examples, "nudged", c.m.ID, cfg.Examples)
				}
			}
			nu.PerWeek = perWeek(nu.Fired, rep.InboundWeeks)
			v.Nudges = append(v.Nudges, nu)
		}
		rep.InboundVariants = append(rep.InboundVariants, v)
	}
}

func evalOutbound(rep *Report, ix *index, me Me, cfg Config) {
	type sent struct {
		m        *Message
		f        OutFeatures
		answerAt time.Time // first human answer, zero if none
	}
	var pop []*sent
	for _, m := range ix.ms {
		if !m.Outbound() || m.At.Before(cfg.Since) {
			continue
		}
		rep.OutboundInWindow++
		rep.Audit.OutboundInWindow++
		if m.Thread == "" {
			rep.Audit.OutboundUnthreaded++
		} else {
			for _, o := range ix.byThread[m.Thread] {
				if !o.Outbound() {
					rep.Audit.OutboundThreadedWithInbound++
					break
				}
			}
		}
		if m.InReplyTo != "" {
			rep.Audit.OutboundWithInReplyTo++
			switch p, ok := ix.byMsgID[m.InReplyTo]; {
			case !ok:
				rep.Audit.ParentMissing++
			case p.Thread != "" && p.Thread == m.Thread:
				rep.Audit.ParentSameThread++
			default:
				rep.Audit.ParentOtherThread++
			}
		}
		if !m.Parsed {
			rep.OutboundUnparsed++
			continue
		}

		s := &sent{m: m, f: ix.outboundFeatures(m, me)}
		autoAnswer := false
		for _, o := range ix.related(m) {
			if IsHumanAnswer(o) {
				s.answerAt = o.At
				break
			}
			if !o.Outbound() && !o.Junk && o.At.Sub(m.At) <= cfg.ReplyWithin {
				autoAnswer = true // an automatic reply, deliberately not an answer
			}
		}
		if autoAnswer && (s.answerAt.IsZero() || s.answerAt.Sub(m.At) > cfg.ReplyWithin) {
			rep.OutboundAutoOnly++
		}
		pop = append(pop, s)
	}

	for _, rule := range outboundRules {
		v := OutboundVariant{Name: rule.name, Rule: rule.rule, Examples: map[string][]string{}}
		for _, s := range pop {
			if rule.pred(s.f) {
				v.InWindow++
			}
		}
		for _, n := range cfg.OutboundDays {
			within := time.Duration(n) * day
			end := cfg.Now.Add(-within)
			lateEnd := cfg.Now.Add(-cfg.LateWithin)
			od := OutboundDays{Days: n}
			for _, s := range pop {
				if !rule.pred(s.f) || s.m.At.After(end) {
					continue
				}
				od.Answered.N++
				if !s.answerAt.IsZero() && s.answerAt.Sub(s.m.At) <= within {
					od.Answered.Hits++
					if n == 7 {
						addExample(v.Examples, "answered within 7d", s.m.ID, cfg.Examples)
					}
					continue
				}
				superseded := false
				for _, o := range ix.related(s.m) {
					if o.At.Sub(s.m.At) > within {
						break
					}
					if o.Outbound() {
						superseded = true
						break
					}
				}
				if superseded {
					od.Superseded++
					continue
				}
				od.Fired++
				if n == 7 {
					addExample(v.Examples, "nudged at 7d", s.m.ID, cfg.Examples)
				}
				if !s.m.At.After(lateEnd) {
					od.LateObserved++
					if !s.answerAt.IsZero() && s.answerAt.Sub(s.m.At) <= cfg.LateWithin {
						od.LateAnswered++
					}
				}
			}
			od.PerWeek = perWeek(od.Fired, weeks(cfg.Since, end))
			v.ByDays = append(v.ByDays, od)
		}
		rep.OutboundVariants = append(rep.OutboundVariants, v)
	}
}
