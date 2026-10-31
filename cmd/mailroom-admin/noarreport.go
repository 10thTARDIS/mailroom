// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"bytes"
	"net"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/sieve"
	"mailroom/internal/store"
)

// `mailroom-admin auth-report --authserv --no-ar` — what is the inbound mail that
// carries no Authentication-Results at all? (v1.90.5, #776)
//
// WHY IT EXISTS. On a real instance the "other inbound" row (no header, not locally
// authored, not in Sent/Drafts, not From an own identity) grew to hold a large
// share of messages, against a few percent before. Before anyone decides what
// that means, the report lays two populations side by side so differences stand
// out rather than being inferred from a total:
//
//	NOAR    exactly the "other inbound" bucket (authservAggregate.add decides it;
//	        nothing here re-derives it);
//	CONTROL inbound mail (not locally authored, not Sent/Drafts) with a header
//	        bearing a trusted id under the configured rule, counted only in the
//	        months NOAR also occurs in.
//
// It reads headers from the same blob the walk already parsed: no second pass.
// It changes nothing: no writes, no behaviour.
//
// PRIVACY: ids, counts, mail-infrastructure hostnames and address DOMAINS are
// printed. No subject, body or full address is. IMAP folder names and account
// names are printed because the operator running the report owns the instance.

const (
	noARTopDomains  = 15 // From domains table
	noARTopHosts    = 10 // Received by / from tables
	noARTopDelivery = 5  // Delivered-To / X-Original-To domains
	noARTopBoxes    = 25 // mailbox table
	noARTopMonthAcc = 60 // month x account table
)

const (
	unparsedHost = "(unparsed)"
	ipHost       = "[ip]"
)

// noARRow counts one table row in each population. examples are NOAR message ids only.
type noARRow struct {
	noar, ctl int
	examples  []string
}

type noARTable map[string]*noARRow

func (t noARTable) bump(key string, noar bool, id string, examples int) {
	r := t[key]
	if r == nil {
		r = &noARRow{}
		t[key] = r
	}
	if noar {
		r.noar++
		if len(r.examples) < examples && id != "" {
			r.examples = append(r.examples, id)
		}
		return
	}
	r.ctl++
}

// mergeCtl adds src's CONTROL counts into t (used to fold the per-month control
// tallies into the months NOAR occurs in).
func (t noARTable) mergeCtl(src noARTable) {
	for k, r := range src {
		d := t[k]
		if d == nil {
			d = &noARRow{}
			t[k] = d
		}
		d.ctl += r.ctl
	}
}

// noARTables is every table of the report for one set of messages.
type noARTables struct {
	noar, ctl               int // messages observed per population
	monthAccount, mailbox   noARTable
	fromDomain, received    noARTable
	recvBy, recvFrom        noARTable
	presence                noARTable
	deliveredTo, originalTo noARTable
	examples                int
}

func newNoARTables(examples int) *noARTables {
	return &noARTables{
		monthAccount: noARTable{}, mailbox: noARTable{}, fromDomain: noARTable{}, received: noARTable{},
		recvBy: noARTable{}, recvFrom: noARTable{}, presence: noARTable{}, deliveredTo: noARTable{},
		originalTo: noARTable{}, examples: examples,
	}
}

// noARMsg is one message reduced to what the report reads.
type noARMsg struct {
	id         string
	month      string   // received month, YYYY-MM (UTC)
	account    string   // display name, or id; "" when no live mailbox names one
	mailboxes  []string // "role" or imap name, one per live mailbox
	fromDomain string
	raw        []byte
}

func newNoARMsg(ref store.MessageAuthRef, raw []byte, live []ulid.ULID,
	labels map[ulid.ULID]store.MailboxLabel, accounts map[ulid.ULID]string) noARMsg {
	m := noARMsg{id: ref.ID.String(), month: ref.ReceivedAt.UTC().Format("2006-01"), raw: raw,
		fromDomain: addrDomain(ref.FromAddress)}
	seenAcc := map[string]bool{}
	for _, b := range live {
		l, ok := labels[b]
		if !ok {
			m.mailboxes = append(m.mailboxes, "(unknown mailbox)")
			continue
		}
		name := l.Role
		if name == "" {
			name = l.IMAPName
		}
		m.mailboxes = append(m.mailboxes, name)
		acc := accounts[l.AccountID]
		if acc == "" {
			acc = l.AccountID.String()
		}
		if !seenAcc[acc] {
			seenAcc[acc] = true
			if m.account == "" {
				m.account = acc
			} else {
				m.account += "+" + acc
			}
		}
	}
	return m
}

func addrDomain(addr string) string {
	i := strings.LastIndexByte(addr, '@')
	if i < 0 || i == len(addr)-1 {
		return "(none)"
	}
	return strings.ToLower(strings.Trim(strings.TrimSpace(addr[i+1:]), "<>. "))
}

// presenceOrder is the header-presence table, in print order.
var presenceOrder = []string{
	"DKIM-Signature", "ARC-Seal", "ARC-Authentication-Results", "List-Id", "Delivered-To",
	"X-Original-To", "X-Forwarded-*", "Resent-*", "Return-Path empty or <>",
}

func (t *noARTables) observe(m noARMsg, noar bool) {
	n := func(tb noARTable, key string) { tb.bump(key, noar, m.id, t.examples) }
	// Examples (ids) are kept for tables 1, 2 and 5 only.
	plain := func(tb noARTable, key string) { tb.bump(key, noar, "", 0) }
	if noar {
		t.noar++
	} else {
		t.ctl++
	}
	acc := m.account
	if acc == "" {
		acc = "(no live mailbox)"
	}
	n(t.monthAccount, m.month+"  "+acc)
	if len(m.mailboxes) == 0 {
		n(t.mailbox, "(no live mailbox)")
	}
	seenBox := map[string]bool{}
	for _, b := range m.mailboxes {
		if !seenBox[b] {
			seenBox[b] = true
			n(t.mailbox, b)
		}
	}
	plain(t.fromDomain, m.fromDomain)

	fields := parseHeaderFields(m.raw)
	var received []string
	has := map[string]bool{}
	delivered, original := map[string]bool{}, map[string]bool{}
	for _, f := range fields {
		lname := strings.ToLower(f.name)
		switch {
		case lname == "received":
			received = append(received, f.value)
		case lname == "dkim-signature":
			has["DKIM-Signature"] = true
		case lname == "arc-seal":
			has["ARC-Seal"] = true
		case lname == "arc-authentication-results":
			has["ARC-Authentication-Results"] = true
		case lname == "list-id":
			has["List-Id"] = true
		case lname == "delivered-to":
			has["Delivered-To"] = true
			delivered[addrDomain(f.value)] = true
		case lname == "x-original-to":
			has["X-Original-To"] = true
			original[addrDomain(f.value)] = true
		case strings.HasPrefix(lname, "x-forwarded-"):
			has["X-Forwarded-*"] = true
		case strings.HasPrefix(lname, "resent-"):
			has["Resent-*"] = true
		case lname == "return-path":
			if v := strings.TrimSpace(f.value); v == "" || v == "<>" {
				has["Return-Path empty or <>"] = true
			}
		}
	}
	for _, k := range presenceOrder {
		if has[k] {
			plain(t.presence, k)
		}
	}
	for d := range delivered {
		plain(t.deliveredTo, d)
	}
	for d := range original {
		plain(t.originalTo, d)
	}
	if c := len(received); c >= 4 {
		plain(t.received, "4+")
	} else {
		plain(t.received, strconv.Itoa(c))
	}
	if len(received) > 0 {
		by, from := receivedHosts(received[0])
		n(t.recvBy, by)
		n(t.recvFrom, from)
	} else {
		n(t.recvBy, "(no Received header)")
		n(t.recvFrom, "(no Received header)")
	}
}

// ---- leniently parsed header block ----

type headerField struct{ name, value string }

// parseHeaderFields splits the header block of a raw message into unfolded
// fields, topmost first. It is deliberately lenient: a line with no colon is
// skipped, a bare-LF message works, and nothing is decoded. It reads the bytes
// directly because the shipped mime.Header offers named lookups, not the list
// of names that "any header starting X-Forwarded-" needs.
func parseHeaderFields(raw []byte) []headerField {
	end := len(raw)
	for _, sep := range [][]byte{[]byte("\r\n\r\n"), []byte("\n\n")} {
		if i := bytes.Index(raw, sep); i >= 0 && i < end {
			end = i
		}
	}
	// The header block is sender-controlled: bound it, and unfold each field by
	// collecting its parts and joining once (repeated += on one field's value
	// is quadratic in the number of continuation lines).
	if end > maxNoARHeaderBytes {
		end = maxNoARHeaderBytes
	}
	block := strings.ReplaceAll(string(raw[:end]), "\r\n", "\n")
	var out []headerField
	var parts [][]string
	for _, line := range strings.Split(block, "\n") {
		if line == "" {
			continue
		}
		if (line[0] == ' ' || line[0] == '\t') && len(out) > 0 {
			parts[len(parts)-1] = append(parts[len(parts)-1], strings.TrimSpace(line))
			continue
		}
		i := strings.IndexByte(line, ':')
		if i <= 0 || strings.ContainsAny(line[:i], " \t") {
			continue
		}
		out = append(out, headerField{name: line[:i]})
		parts = append(parts, []string{strings.TrimSpace(line[i+1:])})
	}
	for i := range out {
		out[i].value = strings.Join(parts[i], " ")
	}
	return out
}

// maxNoARHeaderBytes bounds how much of a message's header block the --no-ar
// report parses. Real header blocks are a few KB; a block larger than this is
// counted from its first 256 KiB.
const maxNoARHeaderBytes = 256 << 10

var (
	recvFromRE = regexp.MustCompile(`(?i)(?:^|\s)from\s+(\S+)`)
	recvByRE   = regexp.MustCompile(`(?i)(?:^|\s)by\s+(\S+)`)
)

// stripComments removes RFC 5322 parenthesised comments (nested, with
// backslash escapes), which is where MTAs put the reverse-DNS name and IP.
func stripComments(s string) string {
	var b strings.Builder
	depth := 0
	for i := 0; i < len(s); i++ {
		switch c := s[i]; {
		case c == '\\' && i+1 < len(s):
			if depth == 0 {
				b.WriteByte(c)
				b.WriteByte(s[i+1])
			}
			i++
		case c == '(':
			depth++
		case c == ')' && depth > 0:
			depth--
			if depth == 0 {
				b.WriteByte(' ')
			}
		case depth == 0:
			b.WriteByte(c)
		}
	}
	return b.String()
}

// receivedHosts extracts the `by` and `from` hosts of one Received value,
// lowercased, IP literals bucketed as [ip], anything it cannot read as
// (unparsed). A trace header is free text; this never fails, it classifies.
func receivedHosts(value string) (by, from string) {
	v := stripComments(value)
	if i := strings.LastIndexByte(v, ';'); i >= 0 {
		v = v[:i] // the date after the last semicolon
	}
	return hostToken(recvByRE, v), hostToken(recvFromRE, v)
}

func hostToken(re *regexp.Regexp, v string) string {
	m := re.FindStringSubmatch(v)
	if m == nil {
		return unparsedHost
	}
	h := strings.ToLower(strings.Trim(m[1], "<>;,"))
	h = strings.TrimSuffix(h, ".")
	if h == "" {
		return unparsedHost
	}
	ip := strings.Trim(strings.TrimPrefix(h, "ipv6:"), "[]")
	if net.ParseIP(ip) != nil {
		return ipHost
	}
	if strings.HasPrefix(h, "[") { // a bracketed literal that is not an IP
		return unparsedHost
	}
	return h
}

// ---- the report over both populations ----

type noARReport struct {
	examples int
	noar     *noARTables
	ctlMonth map[string]*noARTables // CONTROL, per received month; folded at print time
	dkim     *noARDKIM              // --dkim (v1.90.6); nil unless asked for
}

func newNoARReport(examples int) *noARReport {
	return &noARReport{examples: examples, noar: newNoARTables(examples), ctlMonth: map[string]*noARTables{}}
}

func (r *noARReport) observe(m noARMsg, noar bool) {
	if noar {
		r.noar.observe(m, true)
		return
	}
	t := r.ctlMonth[m.month]
	if t == nil {
		t = newNoARTables(0)
		r.ctlMonth[m.month] = t
	}
	t.observe(m, false)
}

// noarMonths returns the months NOAR occurs in, from the month x account table keys.
func (r *noARReport) noarMonths() map[string]bool {
	out := map[string]bool{}
	for k, row := range r.noar.monthAccount {
		if row.noar > 0 {
			out[strings.SplitN(k, "  ", 2)[0]] = true
		}
	}
	return out
}

// combined folds the CONTROL months NOAR also occurs in into one set of tables,
// and reports how many CONTROL messages fell in months with no NOAR (left out).
func (r *noARReport) combined() (t *noARTables, ctlOutside int) {
	// A fresh set each call (print may be called more than once): copy NOAR in,
	// then fold CONTROL months on top.
	t = newNoARTables(r.examples)
	t.noar = r.noar.noar
	t.copyNoAR(r.noar)
	months := r.noarMonths()
	for mo, c := range r.ctlMonth {
		if !months[mo] {
			ctlOutside += c.ctl
			continue
		}
		t.ctl += c.ctl
		t.monthAccount.mergeCtl(c.monthAccount)
		t.mailbox.mergeCtl(c.mailbox)
		t.fromDomain.mergeCtl(c.fromDomain)
		t.received.mergeCtl(c.received)
		t.recvBy.mergeCtl(c.recvBy)
		t.recvFrom.mergeCtl(c.recvFrom)
		t.presence.mergeCtl(c.presence)
		t.deliveredTo.mergeCtl(c.deliveredTo)
		t.originalTo.mergeCtl(c.originalTo)
	}
	return t, ctlOutside
}

// copyNoAR copies src's rows (counts and examples) into t's empty tables.
func (t *noARTables) copyNoAR(src *noARTables) {
	pairs := [][2]noARTable{
		{t.monthAccount, src.monthAccount}, {t.mailbox, src.mailbox}, {t.fromDomain, src.fromDomain},
		{t.received, src.received}, {t.recvBy, src.recvBy}, {t.recvFrom, src.recvFrom},
		{t.presence, src.presence}, {t.deliveredTo, src.deliveredTo}, {t.originalTo, src.originalTo},
	}
	for _, p := range pairs {
		for k, r := range p[1] {
			p[0][k] = &noARRow{noar: r.noar, examples: append([]string(nil), r.examples...)}
		}
	}
}

type sortMode int

const (
	byCount sortMode = iota // NOAR desc, then CONTROL desc, then key
	byKey                   // key ascending (months read chronologically)
	byOrder                 // preset rows first, in their given order (dkimGrid only)
)

// printNoARTable prints at most n rows and says how many it cut: a bounded list
// reports its bound. withEx adds the NOAR example ids column.
func printNoARTable(title string, t noARTable, n int, mode sortMode, withEx bool) {
	rpln()
	rp("%s\n", title)
	keys := make([]string, 0, len(t))
	for k := range t {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		a, b := t[keys[i]], t[keys[j]]
		if mode == byCount {
			if a.noar != b.noar {
				return a.noar > b.noar
			}
			if a.ctl != b.ctl {
				return a.ctl > b.ctl
			}
		}
		return keys[i] < keys[j]
	})
	shown := keys
	if n > 0 && len(shown) > n {
		shown = shown[:n]
	}
	rp("  %-48s %8s %8s\n", "ROW (top "+strconv.Itoa(n)+")", "NOAR", "CONTROL")
	for _, k := range shown {
		r := t[k]
		line := ""
		if withEx && len(r.examples) > 0 {
			line = "  e.g. " + strings.Join(r.examples, " ")
		}
		// ⚠ k comes from headers the SENDER wrote (Received hosts, Delivered-To
		// domains, From domains) or from mailbox names the server chose. Strip
		// control characters before they reach the operator's terminal, as the
		// sieve report does for server-supplied names.
		rp("  %-48s %8d %8d%s\n", sieve.Printable(k), r.noar, r.ctl, line)
	}
	if cut := len(keys) - len(shown); cut > 0 {
		cn, cc := 0, 0
		for _, k := range keys[len(shown):] {
			cn += t[k].noar
			cc += t[k].ctl
		}
		rp("  +%d more rows, %d NOAR / %d CONTROL messages (table shows top %d of %d rows)\n", cut, cn, cc, n, len(keys))
	} else {
		rp("  (all %d rows shown; limit %d)\n", len(keys), n)
	}
}

func (r *noARReport) print() {
	t, ctlOutside := r.combined()
	rpln()
	rpln("NO AUTHENTICATION-RESULTS BREAKDOWN (--no-ar, #776)")
	rpln("  NOAR    = inbound with no Authentication-Results at all (the \"other inbound\" row above)")
	rpln("  CONTROL = inbound (not locally authored, not Sent/Drafts) with a header bearing a trusted id,")
	rpln("            in the months NOAR also occurs in")
	rp("  NOAR messages    : %d\n", t.noar)
	rp("  CONTROL messages : %d  (%d more CONTROL messages fall in months with no NOAR and are left out)\n", t.ctl, ctlOutside)
	rpln("  Example ids are NOAR only, up to --examples per row; ids only. A message live in several mailboxes")
	rpln("  appears in each mailbox row; a row's examples are the first ids met in id (creation) order.")

	printNoARTable("1. BY RECEIVED MONTH x ACCOUNT (account of the first live mailbox; \"a+b\" = live in both)",
		t.monthAccount, noARTopMonthAcc, byKey, true)
	printNoARTable("2. BY LIVE MAILBOX (role, else IMAP name)", t.mailbox, noARTopBoxes, byCount, true)
	printNoARTable("3. FROM DOMAIN", t.fromDomain, noARTopDomains, byCount, false)
	printNoARTable("4. Received: HEADER COUNT", t.received, 5, byKey, false)
	printNoARTable("5a. TOPMOST Received: `by` HOST (IP literals as [ip])", t.recvBy, noARTopHosts, byCount, true)
	printNoARTable("5b. TOPMOST Received: `from` HOST (IP literals as [ip])", t.recvFrom, noARTopHosts, byCount, true)
	for _, k := range presenceOrder { // a header absent from both is a finding, not a missing row
		if t.presence[k] == nil {
			t.presence[k] = &noARRow{}
		}
	}
	printNoARTable("6. HEADER PRESENCE (messages carrying it; of NOAR / CONTROL above)", t.presence, len(presenceOrder), byKey, false)
	printNoARTable("6b. Delivered-To DOMAINS (a message counts once per domain)", t.deliveredTo, noARTopDelivery, byCount, false)
	printNoARTable("6c. X-Original-To DOMAINS (a message counts once per domain)", t.originalTo, noARTopDelivery, byCount, false)
	rpln()
	rpln("  A header absent from a population shows no row there; rows are counts of messages, so compare")
	rpln("  each against the NOAR / CONTROL totals above, not against each other.")
	if r.dkim != nil {
		r.dkim.print()
	}
}
