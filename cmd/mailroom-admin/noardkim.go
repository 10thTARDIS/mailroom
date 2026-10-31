// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"context"
	"sort"
	"strings"

	"mailroom/internal/dkimverify"
	"mailroom/internal/ml"
	"mailroom/internal/sieve"
	"mailroom/internal/store"
)

// `auth-report --authserv --no-ar --dkim` — would local DKIM verification
// authenticate the mail that carries no Authentication-Results? (v1.90.6, #776)
//
// READ-ONLY. It verifies in-process (internal/dkimverify) the messages the
// --no-ar walk already classifies, in the SAME corpus walk: no second pass over
// the blobs. Nothing is stored and no behaviour changes.
//
//	NOAR     every message in the NOAR population is verified;
//	CONTROL  every Nth CONTROL message (--dkim-sample, default 20) is verified
//	         as a CALIBRATION sample, and the local outcome is set beside the
//	         provider's own dkim= verdict from the message's TRUSTED
//	         Authentication-Results (ml.TrustedAt + ml.TrustedAuthResults, the
//	         same trusted-parse path the rest of the report uses). If the local
//	         verifier and the provider disagree where both have a verdict, the
//	         local verifier is not trusted, and the matrix says so.
//
// The sample takes every Nth CONTROL message over the whole walk (the walk
// cannot know which months NOAR occurs in until it ends), so the calibration
// covers CONTROL months the tables above leave out.
//
// WHAT TO READ INTO IT. Keys are fetched at REPORT time, not at receipt. Old
// mail whose key was rotated reads key_not_found or fail: that is the honest
// answer to "would this verify today", not a defect.
//
// PRIVACY: statuses, d= domains, From domains and Received hosts are printed.
// No selector table, key, signature, subject or full address is.

const (
	dkimTopDomains = noARTopDomains // From domain table
	dkimTopRoutes  = noARTopHosts   // topmost Received `from` table
)

// dkimChecker is what the report needs of dkimverify.Verifier, so a test can
// substitute a verifier with a fake resolver (or a broken one).
type dkimChecker interface {
	Verify(ctx context.Context, raw []byte) []dkimverify.Result
	Stats() dkimverify.Stats
}

// dkimConfig turns the --dkim measurement on for a walk.
type dkimConfig struct {
	verifier dkimChecker
	sample   int // every Nth CONTROL message is verified; >= 1
}

// Message-level outcome columns, in print order.
const (
	colPassAligned   = "pass aligned"
	colPassUnaligned = "pass unaligned"
	colFail          = "fail"
	colKeyGone       = "key not found/revoked"
	colTemp          = "temperror"
	colOther         = "perm/unsupp."
	colNone          = "no signature"
)

var dkimCols = []string{colPassAligned, colPassUnaligned, colFail, colKeyGone, colTemp, colOther, colNone}

// Agreement-matrix columns (local outcome) and rows (provider verdict).
var (
	matrixCols = []string{"pass", "fail", "key nf/rev", "temperror", "perm/unsupp.", "no signature"}
	matrixRows = []string{"pass", "fail", "none", "other"}
)

// orgDomain approximates the organisational domain as the last two labels. That
// is WRONG for multi-label public suffixes (example.co.uk reads as co.uk); the
// report says so wherever it prints an alignment number.
func orgDomain(d string) string {
	d = strings.ToLower(strings.Trim(strings.TrimSpace(d), "."))
	labels := strings.Split(d, ".")
	if len(labels) <= 2 {
		return d
	}
	return strings.Join(labels[len(labels)-2:], ".")
}

// statusPrecedence orders the non-pass statuses for the message-level verdict:
// when no signature passes, the message is reported at the first of these any
// of its signatures has.
var statusPrecedence = []dkimverify.Status{
	dkimverify.StatusFailBody, dkimverify.StatusFailSignature, dkimverify.StatusKeyRevoked,
	dkimverify.StatusKeyNotFound, dkimverify.StatusTempError, dkimverify.StatusPermError,
	dkimverify.StatusUnsupported,
}

const (
	statusPassAligned   = "pass, d= aligned with From"
	statusPassUnaligned = "pass, d= not aligned"
	statusNoSignature   = "no signature"
)

// summarize reduces a message's signature results to one status row and one
// outcome column. A message passes if ANY signature passes; it is "aligned" if
// any PASSING signature's d= shares the From domain's (approximate)
// organisational domain.
func summarize(rs []dkimverify.Result, fromDomain string) (status, col string) {
	if len(rs) == 0 {
		return statusNoSignature, colNone
	}
	from := orgDomain(fromDomain)
	passed, aligned := false, false
	have := map[dkimverify.Status]bool{}
	for _, r := range rs {
		have[r.Status] = true
		if r.Status == dkimverify.StatusPass {
			passed = true
			if r.Domain != "" && orgDomain(r.Domain) == from {
				aligned = true
			}
		}
	}
	switch {
	case aligned:
		return statusPassAligned, colPassAligned
	case passed:
		return statusPassUnaligned, colPassUnaligned
	}
	for _, s := range statusPrecedence {
		if !have[s] {
			continue
		}
		switch s {
		case dkimverify.StatusFailBody, dkimverify.StatusFailSignature:
			return string(s), colFail
		case dkimverify.StatusKeyRevoked, dkimverify.StatusKeyNotFound:
			return string(s), colKeyGone
		case dkimverify.StatusTempError:
			return string(s), colTemp
		}
		return string(s), colOther
	}
	return string(rs[0].Status), colOther // unreachable: every Status is listed above
}

// matrixCol folds an outcome column into the agreement matrix's local columns.
func matrixCol(col string) string {
	switch col {
	case colPassAligned, colPassUnaligned:
		return "pass"
	case colFail:
		return "fail"
	case colKeyGone:
		return "key nf/rev"
	case colTemp:
		return "temperror"
	case colOther:
		return "perm/unsupp."
	}
	return "no signature"
}

// dkimVerdict is the provider's own dkim= result for a message, from its
// TRUSTED Authentication-Results: pass (any signature passed), fail (none
// passed, one failed), none (no dkim= result, or only "none"), other (neutral,
// policy, temperror, permerror).
func dkimVerdict(ar ml.AuthResults) string {
	set := ar.Results["dkim"]
	has := func(k string) bool {
		for r := range set {
			if strings.EqualFold(r, k) {
				return true
			}
		}
		return false
	}
	switch {
	case has("pass"):
		return "pass"
	case has("fail"):
		return "fail"
	case len(set) == 0 || (len(set) == 1 && has("none")):
		return "none"
	}
	return "other"
}

// providerDKIMVerdict reads the verdict the way the rest of the report reads a
// trusted header: the ids trusted at THIS message's received time, the topmost
// trusted authserv-id's headers only.
func providerDKIMVerdict(arValues, trusted []string, ref store.MessageAuthRef) string {
	return dkimVerdict(ml.TrustedAuthResults(arValues, ml.TrustedAt(trusted, ref.TrustTime())))
}

// ---- tables ----

// dkimGrid counts messages per row across named columns.
type dkimGrid struct {
	cols    []string
	rows    map[string][]int
	noTotal bool     // columns are not a partition: no TOTAL column
	order   []string // preset rows, printed first and in this order when mode is byOrder
}

func newDKIMGrid(cols []string, rows ...string) *dkimGrid {
	g := &dkimGrid{cols: cols, rows: map[string][]int{}, order: rows}
	for _, r := range rows {
		g.rows[r] = make([]int, len(cols))
	}
	return g
}

func (g *dkimGrid) inc(row, col string) {
	c := make([]int, len(g.cols))
	if cur, ok := g.rows[row]; ok {
		c = cur
	}
	for i, name := range g.cols {
		if name == col {
			c[i]++
		}
	}
	g.rows[row] = c
}

func (g *dkimGrid) cell(row, col string) int {
	for i, name := range g.cols {
		if name == col && g.rows[row] != nil {
			return g.rows[row][i]
		}
	}
	return 0
}

func sum(c []int) (t int) {
	for _, n := range c {
		t += n
	}
	return t
}

// print writes at most n rows (all of them when n <= 0) and says how many it cut.
func (g *dkimGrid) print(title string, n int, mode sortMode) {
	rpln()
	rpln(title)
	keys := make([]string, 0, len(g.rows))
	for k := range g.rows {
		keys = append(keys, k)
	}
	pos := map[string]int{}
	for i, k := range g.order {
		pos[k] = i
	}
	sort.Slice(keys, func(i, j int) bool {
		if mode == byOrder {
			pi, iok := pos[keys[i]]
			pj, jok := pos[keys[j]]
			if iok != jok {
				return iok
			}
			if iok && pi != pj {
				return pi < pj
			}
		}
		if mode == byCount {
			if a, b := sum(g.rows[keys[i]]), sum(g.rows[keys[j]]); a != b {
				return a > b
			}
		}
		return keys[i] < keys[j]
	})
	shown := keys
	if n > 0 && len(shown) > n {
		shown = shown[:n]
	}
	rp("  %-34s", "ROW")
	for _, c := range g.cols {
		rp(" %*s", colWidth(c), c)
	}
	if !g.noTotal {
		rp(" %7s", "TOTAL")
	}
	rpln()
	for _, k := range shown {
		// ⚠ k can come from a header the SENDER wrote (From domain, Received host).
		rp("  %-34s", sieve.Printable(k))
		for i, c := range g.cols {
			rp(" %*d", colWidth(c), g.rows[k][i])
		}
		if !g.noTotal {
			rp(" %7d", sum(g.rows[k]))
		}
		rpln()
	}
	if cut := len(keys) - len(shown); cut > 0 {
		m := 0
		for _, k := range keys[len(shown):] {
			m += sum(g.rows[k])
		}
		rp("  +%d more rows, %d messages (table shows top %d of %d rows)\n", cut, m, n, len(keys))
	} else if n > 0 {
		rp("  (all %d rows shown; limit %d)\n", len(keys), n)
	} else {
		rp("  (all %d rows shown)\n", len(keys))
	}
}

func colWidth(c string) int {
	if len(c) < 6 {
		return 6
	}
	return len(c)
}

// noARDKIM accumulates the --dkim measurement.
type noARDKIM struct {
	cfg dkimConfig

	noarSeen, noarVerified int
	ctlSeen, ctlVerified   int // CONTROL messages walked / verified (the sample)

	status    noARTable // message-level status; NOAR vs sampled CONTROL
	sigStatus noARTable // signature-level status; NOAR vs sampled CONTROL
	sigDetail noARTable // non-pass signatures by "status: detail" (the reason, #776)
	sigDomain noARTable // non-pass signatures by signing domain (d=)
	fromDom   *dkimGrid // NOAR by From domain
	route     *dkimGrid // NOAR by topmost Received `from` host
	month     *dkimGrid // NOAR by received month: pass / other
	matrix    *dkimGrid // CONTROL sample: provider verdict x local outcome
	calMonth  *dkimGrid // CONTROL sample by month: sampled / both-verdict / agree
}

func newNoARDKIM(cfg dkimConfig) *noARDKIM {
	d := &noARDKIM{
		cfg: cfg, status: noARTable{}, sigStatus: noARTable{}, sigDetail: noARTable{}, sigDomain: noARTable{},
		fromDom:  newDKIMGrid(dkimCols),
		route:    newDKIMGrid(dkimCols),
		month:    newDKIMGrid([]string{"pass", "other"}),
		matrix:   newDKIMGrid(matrixCols, matrixRows...),
		calMonth: newDKIMGrid([]string{"sampled", "both verdict", "agree"}),
	}
	d.calMonth.noTotal = true
	return d
}

// observe verifies one message of the report's two populations. provider is the
// provider's dkim= verdict, read only for CONTROL.
func (d *noARDKIM) observe(ctx context.Context, m noARMsg, noar bool, provider func() string) {
	if noar {
		d.noarSeen++
	} else {
		d.ctlSeen++
		if d.ctlSeen%d.cfg.sample != 0 {
			return // not in the calibration sample
		}
	}
	rs := d.cfg.verifier.Verify(ctx, m.raw)
	status, col := summarize(rs, m.fromDomain)
	d.status.bump(status, noar, "", 0)
	for _, r := range rs {
		d.sigStatus.bump(string(r.Status), noar, "", 0)
		// WHY a signature did not pass. A status alone cannot tell "the
		// signature is wrong" from "it expired before today" (x= is judged
		// against the current time, so old mail with a short-lived signature
		// is permerror without ever being checked).
		if r.Status != dkimverify.StatusPass {
			d.sigDetail.bump(string(r.Status)+": "+r.Detail, noar, "", 0)
			dom := r.Domain
			if dom == "" {
				dom = "(no d=)"
			}
			d.sigDomain.bump(dom, noar, "", 0)
		}
	}
	if !noar {
		d.ctlVerified++
		p := provider()
		local := matrixCol(col)
		d.matrix.inc(p, local)
		d.calMonth.inc(m.month, "sampled")
		if (p == "pass" || p == "fail") && (local == "pass" || local == "fail") {
			d.calMonth.inc(m.month, "both verdict")
			if p == local {
				d.calMonth.inc(m.month, "agree")
			}
		}
		return
	}
	d.noarVerified++
	d.fromDom.inc(m.fromDomain, col)
	route := "(no Received header)"
	for _, f := range parseHeaderFields(m.raw) {
		if strings.EqualFold(f.name, "received") {
			_, route = receivedHosts(f.value)
			break
		}
	}
	d.route.inc(route, col)
	if col == colPassAligned || col == colPassUnaligned {
		d.month.inc(m.month, "pass")
	} else {
		d.month.inc(m.month, "other")
	}
}

// agreement returns the matrix totals the summary lines need.
func (d *noARDKIM) agreement() (both, agree, provPass, provPassLocalPass, provFail, provFailLocalFail int) {
	for _, p := range []string{"pass", "fail"} {
		for _, l := range []string{"pass", "fail"} {
			n := d.matrix.cell(p, l)
			both += n
			if p == l {
				agree += n
			}
		}
	}
	provPass = sum(d.matrix.rows["pass"])
	provPassLocalPass = d.matrix.cell("pass", "pass")
	provFail = sum(d.matrix.rows["fail"])
	provFailLocalFail = d.matrix.cell("fail", "fail")
	return
}

func (d *noARDKIM) print() {
	st := d.cfg.verifier.Stats()
	rpln()
	rpln("LOCAL DKIM VERIFICATION (--dkim, #776)")
	rpln("  Read-only. Keys are fetched NOW, not at receipt: mail whose key has since rotated reads")
	rpln("  key_not_found or fail, which is the honest answer to \"would this verify today\".")
	rp("  NOAR messages verified     : %d of %d\n", d.noarVerified, d.noarSeen)
	rp("  CONTROL calibration sample : %d verified, every %d%s CONTROL message of %d walked (all months,\n",
		d.ctlVerified, d.cfg.sample, ordinal(d.cfg.sample), d.ctlSeen)
	rpln("                               including months with no NOAR)")
	rp("  DNS key lookups            : %d used of a budget of %d; %d answered from the cache\n", st.Lookups, st.Budget, st.CacheHits)
	rp("  temperror from the budget  : %d signature(s) could not be checked because the budget was spent\n", st.BudgetRefused)
	if st.BudgetRefused > 0 {
		rpln("  ⚠ the budget was exhausted: the temperror rows below are partly a bound, not a finding.")
	}
	rpln("  A message passes if ANY of its signatures passes. \"aligned\" = a passing signature's d= has the")
	rpln("  same organisational domain as the From domain, APPROXIMATED as the last two labels (wrong for")
	rpln("  suffixes like co.uk). When no signature passes, a message is reported at the first of:")
	rpln("  fail_body, fail_signature, key_revoked, key_not_found, temperror, permerror, unsupported.")

	nPass := d.status.rowNoAR(statusPassAligned) + d.status.rowNoAR(statusPassUnaligned)
	rp("  NOAR messages that pass    : %d of %d verified (%s); %d with d= aligned to From, %d unaligned\n",
		nPass, d.noarVerified, sharePct(nPass, d.noarVerified),
		d.status.rowNoAR(statusPassAligned), d.status.rowNoAR(statusPassUnaligned))

	printNoARTable("D1. BY STATUS, per message (NOAR / sampled CONTROL; CONTROL here is the calibration sample only)",
		d.status, 20, byCount, false)
	printNoARTable("D1b. BY STATUS, per signature (a message may carry several; NOAR / sampled CONTROL)",
		d.sigStatus, 20, byCount, false)
	printNoARTable("D1c. NON-PASS SIGNATURES BY REASON (status: detail; NOAR / sampled CONTROL)",
		d.sigDetail, 20, byCount, false)
	printNoARTable("D1d. NON-PASS SIGNATURES BY SIGNING DOMAIN d= (NOAR / sampled CONTROL)",
		d.sigDomain, 15, byCount, false)
	d.fromDom.print("D2. NOAR BY FROM DOMAIN x OUTCOME (messages)", dkimTopDomains, byCount)
	d.route.print("D3. NOAR BY TOPMOST Received: `from` HOST x OUTCOME (messages)", dkimTopRoutes, byCount)
	d.month.print("D4. NOAR BY RECEIVED MONTH x pass / other (messages)", 0, byKey)

	both, agree, pp, ppl, pf, pff := d.agreement()
	d.matrix.print("D5. CALIBRATION: provider's trusted dkim= (rows) x local outcome (columns), sampled CONTROL messages", 0, byOrder)
	if both == 0 {
		rpln("  ⚠ no sampled message had a pass/fail verdict on BOTH sides: there is nothing to compare, which")
		rpln("    means the verifier or the sample is broken or empty, not that they agree.")
	} else {
		rp("  agreement where both have a verdict (pass/fail on each side): %d of %d (%s)\n", agree, both, sharePct(agree, both))
	}
	rp("  provider pass -> local pass : %d of %d (%s)   (the rest: key gone, temperror, fail, ...: see the matrix)\n", ppl, pp, sharePct(ppl, pp))
	rp("  provider fail -> local fail : %d of %d (%s)\n", pff, pf, sharePct(pff, pf))
	d.calMonth.print("D5b. CALIBRATION BY RECEIVED MONTH (key rotation shows as agreement falling with age)", 0, byKey)
}

// rowNoAR is the NOAR count of a row (0 when absent).
func (t noARTable) rowNoAR(key string) int {
	if r := t[key]; r != nil {
		return r.noar
	}
	return 0
}

func ordinal(n int) string {
	switch {
	case n%100 >= 11 && n%100 <= 13:
		return "th"
	case n%10 == 1:
		return "st"
	case n%10 == 2:
		return "nd"
	case n%10 == 3:
		return "rd"
	}
	return "th"
}
