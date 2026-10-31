// SPDX-License-Identifier: AGPL-3.0-only

// Package gmailtest is a scripted, in-process fake of Gmail's IMAP server for
// tests (#52, ADR-047). It speaks real IMAP over TCP, so a test drives the
// production sync.Dial, the go-imap client and the vendored X-GM-EXT-1 parser
// (third_party/go-imap) end to end, and it records what it was asked: every
// SELECT/EXAMINE by mailbox name, every FETCH's items, and every body byte it
// served — so "no label folder was ever selected" and "the import stopped at
// the budget" are assertions about the wire, not about our own bookkeeping.
//
// It models what the Gmail sync path depends on, and nothing else:
//
//   - CAPABILITY with X-GM-EXT-1 (switchable) and CONDSTORE (switchable);
//   - LIST with SPECIAL-USE attributes (\All, \Junk, \Trash, \Sent, ...);
//   - SELECT/EXAMINE, optionally (CONDSTORE), with UIDVALIDITY, UIDNEXT,
//     EXISTS and HIGHESTMODSEQ;
//   - UID FETCH of UID, FLAGS, INTERNALDATE, RFC822.SIZE, ENVELOPE (its
//     Message-ID only), MODSEQ, BODY.PEEK[], X-GM-MSGID, X-GM-THRID and
//     X-GM-LABELS, with (CHANGEDSINCE n);
//   - UID SEARCH (ALL, UID n, X-GM-MSGID n, HEADER Message-Id x), NOOP, ID,
//     ENABLE, UNSELECT, CLOSE, LOGOUT;
//   - the writeback side (write.go): UID STORE ±FLAGS and ±X-GM-LABELS
//     (silent or echoing), UID MOVE / UID COPY with Gmail's label semantics,
//     UID EXPUNGE / EXPUNGE, and CREATE — every command line recorded, so
//     "no EXPUNGE ever reached the server" is an assertion about the wire.
//
// Labels are written on the wire the way Gmail writes them: a system label
// as a backslash atom (\Inbox) and a user label as a quoted string in
// modified UTF-7 — or, per message, verbatim from Message.WireLabels.
package gmailtest

import (
	"bufio"
	"encoding/base64"
	"fmt"
	"net"
	"sort"
	"strconv"
	"strings"
	gosync "sync"
	"testing"
	"time"
	"unicode/utf16"

	"mailroom/internal/egress/egresstest"
	"mailroom/internal/sync"
)

// Message is one message in a fake mailbox.
type Message struct {
	UID          uint32
	InternalDate time.Time
	Flags        []string
	MsgID        uint64 // X-GM-MSGID
	ThrID        uint64 // X-GM-THRID
	// Labels are the message's labels, decoded (`\Inbox`, "Work", "été").
	Labels []string
	// WireLabels, when non-empty, is written verbatim inside X-GM-LABELS (…)
	// instead of encoding Labels — for real-shaped responses a test pins.
	WireLabels string
	// LabelFault, when set, is how this message's X-GM-LABELS goes wrong on
	// the wire whenever it is asked for (#52 review H1):
	//   - LabelsOmitted:  the item is simply not in the response;
	//   - LabelsSplit:    the response carries everything else, and a SECOND
	//                     untagged FETCH for the same message carries the labels;
	//   - LabelsTruncated: the response stops in the middle of the label list
	//                     and the connection is dropped.
	LabelFault string
	// FlagFault, when set, is how this message's FLAGS goes wrong on the wire
	// whenever it is asked for (#909), with the same three values:
	//   - LabelsOmitted:   FLAGS is not in the response;
	//   - LabelsSplit:     a SECOND untagged FETCH for the message carries it;
	//   - LabelsTruncated: the response stops before FLAGS and the connection
	//                      is dropped.
	FlagFault string
	// MessageID is the Message-ID the ENVELOPE reports ("" = NIL).
	MessageID string
	Body      []byte
	// Size overrides RFC822.SIZE (0 = len(Body)).
	Size   int64
	ModSeq uint64
}

func (m *Message) size() int64 {
	if m.Size > 0 {
		return m.Size
	}
	return int64(len(m.Body))
}

// The LabelFault values.
const (
	LabelsOmitted   = "omitted"
	LabelsSplit     = "split"
	LabelsTruncated = "truncated"
)

// Mailbox is one fake mailbox.
type Mailbox struct {
	Name        string
	Attrs       []string // e.g. `\All`, `\HasNoChildren`
	UIDValidity uint32
	UIDNext     uint32
	Messages    []*Message // ascending UID
}

// Server is the fake. All exported fields may be set before the first
// connection; use the methods to mutate state while connected.
type Server struct {
	t testing.TB

	// GmailExt advertises X-GM-EXT-1 and serves the X-GM-* items. Without it
	// a FETCH naming them is answered BAD, as a real non-Gmail server would.
	GmailExt bool
	// CondStore advertises CONDSTORE and serves MODSEQ / HIGHESTMODSEQ.
	CondStore bool
	// LabelChangeBumpsModSeq makes SetLabels bump the message's MODSEQ —
	// the UNVERIFIED Gmail behaviour ADR-047 §4 depends on, switchable so a
	// test can exercise both worlds.
	LabelChangeBumpsModSeq bool

	mu        gosync.Mutex
	mailboxes []*Mailbox
	modseq    uint64

	selects []string
	fetches []string
	// The write side (write.go): every command line the fake received
	// (tag stripped), and every message an EXPUNGE removed.
	commands []string
	expunged []string
	// LabelCreate is how a STORE +X-GM-LABELS naming a user label that does
	// not exist yet behaves — UNVERIFIED on real Gmail (ADR-047), so a test
	// can exercise each world: LabelStoreCreates (the default, ""),
	// LabelStoreNO or LabelStoreIgnores.
	LabelCreate string
	// NoMove withholds the MOVE capability.
	NoMove bool
	// NoUIDPlus withholds UIDPLUS (UID EXPUNGE).
	NoUIDPlus bool
	// MoveDrops makes a MOVE out of Trash or Spam lose the message: the
	// source copy goes and nothing arrives — what a COPY Gmail treated as a
	// no-op would do (#911 review F2).
	MoveDrops bool

	bodyBytes  int64
	bodyCount  int
	sizeChecks int
}

// New returns a fake Gmail with X-GM-EXT-1 and CONDSTORE on and no mailboxes.
func New(t testing.TB) *Server {
	return &Server{t: t, GmailExt: true, CondStore: true, LabelChangeBumpsModSeq: true, modseq: 1000}
}

// StandardMailboxes adds Gmail's usual tree: INBOX and three user-label
// folders (which a Gmail sync must never select), [Gmail]/All Mail (\All),
// Spam (\Junk), Trash (\Trash), Sent (\Sent), Drafts (\Drafts), Starred
// (\Flagged) and Important (\Important).
func (s *Server) StandardMailboxes() {
	add := func(name string, attrs ...string) {
		s.AddMailbox(&Mailbox{Name: name, Attrs: append([]string{`\HasNoChildren`}, attrs...)})
	}
	add("INBOX")
	add("Work")
	add("Receipts")
	add("Été")
	s.AddMailbox(&Mailbox{Name: "[Gmail]", Attrs: []string{`\HasChildren`, `\Noselect`}})
	add("[Gmail]/All Mail", `\All`)
	add("[Gmail]/Drafts", `\Drafts`)
	add("[Gmail]/Important", `\Important`)
	add("[Gmail]/Sent Mail", `\Sent`)
	add("[Gmail]/Spam", `\Junk`)
	add("[Gmail]/Starred", `\Flagged`)
	add("[Gmail]/Trash", `\Trash`)
}

// AddMailbox adds a mailbox (UIDVALIDITY defaults to 1, UIDNEXT to 1).
func (s *Server) AddMailbox(mb *Mailbox) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if mb.UIDValidity == 0 {
		mb.UIDValidity = 1
	}
	if mb.UIDNext == 0 {
		mb.UIDNext = 1
	}
	s.mailboxes = append(s.mailboxes, mb)
}

// Append adds msg to the named mailbox. A zero UID takes UIDNEXT; a zero
// ModSeq takes the next server modseq. It panics on an unknown mailbox.
func (s *Server) Append(mailbox string, msg *Message) *Message {
	s.mu.Lock()
	defer s.mu.Unlock()
	mb := s.find(mailbox)
	if mb == nil {
		panic("gmailtest: no mailbox " + mailbox)
	}
	if msg.UID == 0 {
		msg.UID = mb.UIDNext
	}
	if msg.UID >= mb.UIDNext {
		mb.UIDNext = msg.UID + 1
	}
	if msg.ModSeq == 0 {
		s.modseq++
		msg.ModSeq = s.modseq
	}
	if msg.InternalDate.IsZero() {
		msg.InternalDate = time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	}
	mb.Messages = append(mb.Messages, msg)
	sort.Slice(mb.Messages, func(i, j int) bool { return mb.Messages[i].UID < mb.Messages[j].UID })
	return msg
}

// SetWireLabels pins a message's X-GM-LABELS wire form (see Message).
func (s *Server) SetWireLabels(mailbox string, uid uint32, wire string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.message(mailbox, uid).WireLabels = wire
}

// SetLabelFault sets a message's LabelFault (see Message).
func (s *Server) SetLabelFault(mailbox string, uid uint32, fault string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.message(mailbox, uid).LabelFault = fault
}

// SetFlagFault sets a message's FlagFault (see Message).
func (s *Server) SetFlagFault(mailbox string, uid uint32, fault string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.message(mailbox, uid).FlagFault = fault
}

// SetFlagsQuietly replaces a message's flags WITHOUT bumping its MODSEQ — a
// change on a server that has no CONDSTORE to report it with.
func (s *Server) SetFlagsQuietly(mailbox string, uid uint32, flags ...string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.message(mailbox, uid).Flags = flags
}

// SetLabels replaces a message's labels (a label change made in Gmail's own
// apps). With LabelChangeBumpsModSeq it also bumps the message's MODSEQ.
func (s *Server) SetLabels(mailbox string, uid uint32, labels ...string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	m := s.message(mailbox, uid)
	m.Labels = labels
	m.WireLabels = ""
	if s.LabelChangeBumpsModSeq {
		s.modseq++
		m.ModSeq = s.modseq
	}
}

// SetFlags replaces a message's flags and bumps its MODSEQ.
func (s *Server) SetFlags(mailbox string, uid uint32, flags ...string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	m := s.message(mailbox, uid)
	m.Flags = flags
	s.modseq++
	m.ModSeq = s.modseq
}

// Expunge removes a message from a mailbox.
func (s *Server) Expunge(mailbox string, uid uint32) {
	s.mu.Lock()
	defer s.mu.Unlock()
	mb := s.find(mailbox)
	for i, m := range mb.Messages {
		if m.UID == uid {
			mb.Messages = append(mb.Messages[:i], mb.Messages[i+1:]...)
			s.modseq++
			return
		}
	}
	panic(fmt.Sprintf("gmailtest: no uid %d in %s", uid, mailbox))
}

func (s *Server) message(mailbox string, uid uint32) *Message {
	mb := s.find(mailbox)
	if mb == nil {
		panic("gmailtest: no mailbox " + mailbox)
	}
	for _, m := range mb.Messages {
		if m.UID == uid {
			return m
		}
	}
	panic(fmt.Sprintf("gmailtest: no uid %d in %s", uid, mailbox))
}

func (s *Server) find(name string) *Mailbox {
	for _, mb := range s.mailboxes {
		if mb.Name == name || (strings.EqualFold(name, "INBOX") && strings.EqualFold(mb.Name, "INBOX")) {
			return mb
		}
	}
	return nil
}

// Selects returns every mailbox name SELECTed or EXAMINEd, in order.
func (s *Server) Selects() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.selects...)
}

// Fetches returns every UID FETCH command's arguments, in order.
func (s *Server) Fetches() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.fetches...)
}

// BodyBytesServed is the total size of every BODY[] this server sent, and
// BodiesServed how many it sent.
func (s *Server) BodyBytesServed() (bytes int64, bodies int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.bodyBytes, s.bodyCount
}

// ResetCounters forgets the recorded selects, fetches and body totals.
func (s *Server) ResetCounters() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.selects, s.fetches, s.bodyBytes, s.bodyCount = nil, nil, 0, 0
}

// Listen starts serving on 127.0.0.1 and returns a sync.Config that dials
// it (LOGIN; the fake accepts any credentials). The fake speaks PLAINTEXT, so
// the caller passes the plaintext mode itself: production code may never name
// it (cmd/mailroom's TestNoProductionCodeDisablesTLS), and this file is not a
// _test.go file.
func (s *Server) Listen(plaintext sync.TLSMode) sync.Config {
	ln := egresstest.Listen(s.t, s.serve)
	return sync.Config{
		Host: "localhost", Port: ln.Port(),
		Username: "you@example.com", Password: "fake",
		TLS:               plaintext,
		DialTimeout:       5 * time.Second,
		Resolver:          egresstest.NewResolver(s.t, []string{"127.0.0.1"}),
		AllowPrivateHosts: true,
	}
}

func (s *Server) capabilities() string {
	caps := "IMAP4rev1 UNSELECT IDLE NAMESPACE QUOTA ID XLIST CHILDREN ENABLE SPECIAL-USE AUTH=PLAIN"
	if !s.NoUIDPlus {
		caps += " UIDPLUS"
	}
	if !s.NoMove {
		caps += " MOVE"
	}
	if s.GmailExt {
		caps += " X-GM-EXT-1"
	}
	if s.CondStore {
		caps += " CONDSTORE"
	}
	return caps
}

type conn struct {
	s        *Server
	w        *bufio.Writer
	r        *bufio.Reader
	selected *Mailbox
	condOn   bool
}

func (c *conn) put(format string, args ...any) {
	_, _ = fmt.Fprintf(c.w, format, args...)
	_, _ = c.w.WriteString("\r\n")
}

func (s *Server) serve(nc net.Conn) {
	defer func() { _ = nc.Close() }()
	c := &conn{s: s, w: bufio.NewWriter(nc), r: bufio.NewReader(nc)}
	c.put("* OK [CAPABILITY %s] Gimap ready for requests", s.capabilities())
	_ = c.w.Flush()
	for {
		line, err := c.r.ReadString('\n')
		if err != nil {
			return
		}
		line = strings.TrimRight(line, "\r\n")
		tag, rest, _ := strings.Cut(line, " ")
		cmd, args, _ := strings.Cut(rest, " ")
		if !c.handle(tag, strings.ToUpper(cmd), args) {
			_ = c.w.Flush()
			return
		}
		_ = c.w.Flush()
	}
}

func (c *conn) handle(tag, cmd, args string) bool {
	s := c.s
	s.mu.Lock()
	s.commands = append(s.commands, strings.TrimSpace(cmd+" "+args))
	s.mu.Unlock()
	switch cmd {
	case "CREATE":
		c.create(tag, args)
	case "EXPUNGE":
		c.expunge(tag, "")
	case "CAPABILITY":
		c.put("* CAPABILITY %s", s.capabilities())
		c.put("%s OK Thats all she wrote!", tag)
	case "LOGIN", "AUTHENTICATE":
		c.put("%s OK [CAPABILITY %s] you@example.com authenticated (Success)", tag, s.capabilities())
	case "ID":
		c.put(`* ID ("name" "GImap")`)
		c.put("%s OK Success", tag)
	case "ENABLE":
		if strings.Contains(strings.ToUpper(args), "CONDSTORE") && s.CondStore {
			c.condOn = true
			c.put("* ENABLED CONDSTORE")
		}
		c.put("%s OK Success", tag)
	case "NOOP":
		c.put("%s OK Success", tag)
	case "NAMESPACE":
		c.put(`* NAMESPACE (("" "/")) NIL NIL`)
		c.put("%s OK Success", tag)
	case "LIST":
		s.mu.Lock()
		for _, mb := range s.mailboxes {
			c.put(`* LIST (%s) "/" %s`, strings.Join(mb.Attrs, " "), quote(encodeMUTF7(mb.Name)))
		}
		s.mu.Unlock()
		c.put("%s OK Success", tag)
	case "SELECT", "EXAMINE":
		c.selectMailbox(tag, cmd, args)
	case "UNSELECT", "CLOSE":
		c.selected = nil
		c.put("%s OK Returned to authenticated state. (Success)", tag)
	case "UID":
		sub, subArgs, _ := strings.Cut(args, " ")
		switch strings.ToUpper(sub) {
		case "FETCH":
			if !c.uidFetch(tag, subArgs) {
				return false // a truncated response: the connection drops
			}
		case "SEARCH":
			c.uidSearch(tag, subArgs)
		case "STORE":
			c.uidStore(tag, subArgs)
		case "MOVE", "COPY":
			c.uidMoveCopy(tag, strings.ToUpper(sub), subArgs)
		case "EXPUNGE":
			c.expunge(tag, subArgs)
		default:
			c.put("%s BAD Unsupported UID command", tag)
		}
	case "LOGOUT":
		c.put("* BYE LOGOUT Requested")
		c.put("%s OK 73 good day (Success)", tag)
		return false
	default:
		c.put("%s BAD Unknown command", tag)
	}
	return true
}

func (c *conn) selectMailbox(tag, cmd, args string) {
	s := c.s
	name, rest := unquoteArg(args)
	decoded, err := decodeMUTF7(name)
	if err != nil {
		decoded = name
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.selects = append(s.selects, decoded)
	mb := s.find(decoded)
	if mb == nil {
		c.put("%s NO [NONEXISTENT] Unknown Mailbox: %s (Failure)", tag, name)
		return
	}
	for _, a := range mb.Attrs {
		if strings.EqualFold(a, `\Noselect`) {
			c.put("%s NO [NONEXISTENT] Unknown Mailbox: %s (Failure)", tag, name)
			return
		}
	}
	if strings.Contains(strings.ToUpper(rest), "CONDSTORE") {
		if !s.CondStore {
			c.put("%s BAD CONDSTORE not supported", tag)
			return
		}
		c.condOn = true
	}
	c.selected = mb
	c.put(`* FLAGS (\Answered \Flagged \Draft \Deleted \Seen $NotPhishing $Phishing)`)
	c.put(`* OK [PERMANENTFLAGS (\Answered \Flagged \Draft \Deleted \Seen $NotPhishing $Phishing \*)] Flags permitted.`)
	c.put("* OK [UIDVALIDITY %d] UIDs valid.", mb.UIDValidity)
	c.put("* %d EXISTS", len(mb.Messages))
	c.put("* 0 RECENT")
	c.put("* OK [UIDNEXT %d] Predicted next UID.", mb.UIDNext)
	if c.condOn {
		hm := uint64(1)
		for _, m := range mb.Messages {
			if m.ModSeq > hm {
				hm = m.ModSeq
			}
		}
		c.put("* OK [HIGHESTMODSEQ %d]", hm)
	}
	mode := "READ-WRITE"
	if cmd == "EXAMINE" {
		mode = "READ-ONLY"
	}
	c.put("%s OK [%s] %s selected. (Success)", tag, mode, name)
}

func (c *conn) uidSearch(tag, args string) {
	if c.selected == nil {
		c.put("%s BAD No mailbox selected", tag)
		return
	}
	s := c.s
	match, ok := parseSearch(args, c.selected.UIDNext)
	if !ok {
		c.put("%s BAD Could not parse command", tag)
		return
	}
	s.mu.Lock()
	var b strings.Builder
	b.WriteString("* SEARCH")
	for _, m := range c.selected.Messages {
		if match(m) {
			fmt.Fprintf(&b, " %d", m.UID)
		}
	}
	s.mu.Unlock()
	c.put("%s", b.String())
	c.put("%s OK SEARCH completed (Success)", tag)
}

func (c *conn) uidFetch(tag, args string) bool {
	s := c.s
	if c.selected == nil {
		c.put("%s BAD No mailbox selected", tag)
		return true
	}
	setStr, rest, _ := strings.Cut(args, " ")
	itemsStr := rest
	var changedSince uint64
	if i := strings.Index(strings.ToUpper(rest), "(CHANGEDSINCE"); i >= 0 {
		itemsStr = strings.TrimSpace(rest[:i])
		f := strings.Fields(strings.Trim(rest[i:], "()"))
		if len(f) == 2 {
			changedSince, _ = strconv.ParseUint(f[1], 10, 64)
		}
	}
	items := strings.Fields(strings.ToUpper(strings.Trim(strings.TrimSpace(itemsStr), "()")))
	want := map[string]bool{}
	for _, it := range items {
		want[it] = true
	}
	if !s.GmailExt && (want["X-GM-MSGID"] || want["X-GM-THRID"] || want["X-GM-LABELS"]) {
		c.put("%s BAD Could not parse command", tag)
		return true
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.fetches = append(s.fetches, c.selected.Name+": "+args)
	if want["RFC822.SIZE"] && !want["BODY.PEEK[]"] {
		s.sizeChecks++
	}
	for seq, m := range c.selected.Messages {
		if !inSet(setStr, m.UID, c.selected.UIDNext) {
			continue
		}
		if changedSince > 0 && m.ModSeq <= changedSince {
			continue
		}
		parts := []string{fmt.Sprintf("UID %d", m.UID)}
		if want["FLAGS"] && m.FlagFault == LabelsTruncated {
			// The response ends before FLAGS, and the connection goes.
			c.put("* %d FETCH (UID %d", seq+1, m.UID)
			return false
		}
		if want["FLAGS"] && m.FlagFault == "" {
			parts = append(parts, "FLAGS ("+strings.Join(m.Flags, " ")+")")
		}
		if want["INTERNALDATE"] {
			parts = append(parts, `INTERNALDATE "`+m.InternalDate.UTC().Format("02-Jan-2006 15:04:05 -0700")+`"`)
		}
		if want["RFC822.SIZE"] {
			parts = append(parts, fmt.Sprintf("RFC822.SIZE %d", m.size()))
		}
		if want["ENVELOPE"] {
			mid := "NIL"
			if m.MessageID != "" {
				mid = quote(m.MessageID)
			}
			parts = append(parts, "ENVELOPE (NIL NIL NIL NIL NIL NIL NIL NIL NIL "+mid+")")
		}
		if want["MODSEQ"] || changedSince > 0 {
			parts = append(parts, fmt.Sprintf("MODSEQ (%d)", m.ModSeq))
		}
		if want["X-GM-MSGID"] {
			parts = append(parts, fmt.Sprintf("X-GM-MSGID %d", m.MsgID))
		}
		if want["X-GM-THRID"] {
			parts = append(parts, fmt.Sprintf("X-GM-THRID %d", m.ThrID))
		}
		if want["X-GM-LABELS"] && m.LabelFault == LabelsTruncated {
			// Half a label list, then the connection goes.
			c.put("* %d FETCH (%s X-GM-LABELS (\\Inbox Wo", seq+1, strings.Join(parts, " "))
			return false
		}
		if want["X-GM-LABELS"] && m.LabelFault == "" {
			parts = append(parts, "X-GM-LABELS ("+wireLabels(m)+")")
		}
		if want["BODY.PEEK[]"] {
			parts = append(parts, fmt.Sprintf("BODY[] {%d}\r\n%s", len(m.Body), m.Body))
			s.bodyBytes += int64(len(m.Body))
			s.bodyCount++
		}
		c.put("* %d FETCH (%s)", seq+1, strings.Join(parts, " "))
		if want["FLAGS"] && m.FlagFault == LabelsSplit {
			c.put("* %d FETCH (UID %d FLAGS (%s))", seq+1, m.UID, strings.Join(m.Flags, " "))
		}
		if want["X-GM-LABELS"] && m.LabelFault == LabelsSplit {
			c.put("* %d FETCH (UID %d X-GM-LABELS (%s))", seq+1, m.UID, wireLabels(m))
		}
	}
	c.put("%s OK Success", tag)
	return true
}

// wireLabels renders X-GM-LABELS the way Gmail does.
func wireLabels(m *Message) string {
	if m.WireLabels != "" {
		return m.WireLabels
	}
	out := make([]string, 0, len(m.Labels))
	for _, l := range m.Labels {
		if strings.HasPrefix(l, `\`) {
			out = append(out, l)
			continue
		}
		out = append(out, quote(encodeMUTF7(l)))
	}
	return strings.Join(out, " ")
}

// inSet reports whether uid is in an IMAP sequence-set of UIDs ("1:5,7,9:*").
func inSet(set string, uid, uidNext uint32) bool {
	top := uidNext - 1
	if top < 1 {
		top = 1
	}
	num := func(s string) uint32 {
		if s == "*" {
			return top
		}
		n, _ := strconv.ParseUint(s, 10, 32)
		return uint32(n)
	}
	for _, r := range strings.Split(set, ",") {
		a, b, isRange := strings.Cut(r, ":")
		lo := num(a)
		hi := lo
		if isRange {
			hi = num(b)
		}
		if lo > hi {
			lo, hi = hi, lo
		}
		// "n:*" includes everything from n up even past UIDNEXT.
		if isRange && b == "*" && uid >= lo {
			return true
		}
		if uid >= lo && uid <= hi {
			return true
		}
	}
	return false
}

func quote(s string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s) + `"`
}

// unquoteArg splits a leading IMAP string argument (quoted or atom) from the
// rest of the line.
func unquoteArg(args string) (string, string) {
	args = strings.TrimSpace(args)
	if !strings.HasPrefix(args, `"`) {
		name, rest, _ := strings.Cut(args, " ")
		return name, rest
	}
	var b strings.Builder
	for i := 1; i < len(args); i++ {
		switch args[i] {
		case '\\':
			if i+1 < len(args) {
				i++
				b.WriteByte(args[i])
			}
		case '"':
			return b.String(), strings.TrimSpace(args[i+1:])
		default:
			b.WriteByte(args[i])
		}
	}
	return b.String(), ""
}

// mUTF7 is RFC 3501's modified base64 alphabet (',' instead of '/').
var mUTF7 = base64.NewEncoding("ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+,").WithPadding(base64.NoPadding)

// encodeMUTF7 encodes s as modified UTF-7 (RFC 3501 §5.1.3).
func encodeMUTF7(s string) string {
	var b strings.Builder
	var run []rune
	flush := func() {
		if len(run) == 0 {
			return
		}
		u := utf16.Encode(run)
		buf := make([]byte, 0, len(u)*2)
		for _, x := range u {
			buf = append(buf, byte(x>>8), byte(x))
		}
		b.WriteByte('&')
		b.WriteString(mUTF7.EncodeToString(buf))
		b.WriteByte('-')
		run = nil
	}
	for _, r := range s {
		if r >= 0x20 && r <= 0x7e {
			flush()
			if r == '&' {
				b.WriteString("&-")
			} else {
				b.WriteRune(r)
			}
			continue
		}
		run = append(run, r)
	}
	flush()
	return b.String()
}

// decodeMUTF7 decodes modified UTF-7.
func decodeMUTF7(s string) (string, error) {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] != '&' {
			b.WriteByte(s[i])
			continue
		}
		j := strings.IndexByte(s[i:], '-')
		if j < 0 {
			return "", fmt.Errorf("gmailtest: unterminated shift in %q", s)
		}
		seg := s[i+1 : i+j]
		i += j
		if seg == "" {
			b.WriteByte('&')
			continue
		}
		raw, err := mUTF7.DecodeString(seg)
		if err != nil || len(raw)%2 != 0 {
			return "", fmt.Errorf("gmailtest: bad base64 in %q", s)
		}
		u := make([]uint16, len(raw)/2)
		for k := range u {
			u[k] = uint16(raw[2*k])<<8 | uint16(raw[2*k+1])
		}
		b.WriteString(string(utf16.Decode(u)))
	}
	return b.String(), nil
}

// EncodeMUTF7 is encodeMUTF7, for tests that pin a wire form.
func EncodeMUTF7(s string) string { return encodeMUTF7(s) }
