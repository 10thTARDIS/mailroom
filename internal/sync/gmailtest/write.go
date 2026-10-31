// SPDX-License-Identifier: AGPL-3.0-only

package gmailtest

// THE WRITE SIDE OF THE FAKE GMAIL (#52, ADR-047 §3, increment 5b).
//
// It models the Gmail behaviour the writeback engine depends on, from
// Google's IMAP extension documentation, and records every command so a test
// can assert on what reached the wire:
//
//   - UID STORE ±FLAGS[.SILENT] and ±X-GM-LABELS[.SILENT]. A non-silent STORE
//     echoes the message's state (`* n FETCH (X-GM-LABELS (...) UID u)`), as
//     Gmail does. A UID the mailbox does not hold is an OK with no echo.
//   - Labels on a message in All Mail are its Labels; a label view (INBOX,
//     "Work") is a mailbox a test fills itself, because the sync never reads
//     one.
//   - UID MOVE / UID COPY with Gmail's meaning: into Trash or Spam the message
//     leaves All Mail and every view (one message, many labels); out of Trash
//     or Spam into INBOX, All Mail or a label view it comes back to All Mail
//     at a NEW UID (with \Inbox or the label added). The X-GM-MSGID stays.
//   - UID EXPUNGE / EXPUNGE remove only messages flagged \Deleted, and every
//     removal is recorded with its mailbox (Expunged).
//   - CREATE adds a label view.
//
// What a user label that does not exist yet does on STORE +X-GM-LABELS is
// UNVERIFIED on real Gmail, so LabelCreate picks a world.

import (
	"fmt"
	"strconv"
	"strings"
)

// The LabelCreate worlds.
const (
	LabelStoreCreates = ""       // STORE creates the label (the default)
	LabelStoreNO      = "no"     // STORE answers NO [NONEXISTENT]
	LabelStoreIgnores = "ignore" // STORE answers OK and does not apply it
	// LabelStoreNever: STORE answers OK and never applies a user label, even
	// one that exists — a server whose writes do not stick, for the test
	// that such a write is never recorded as done.
	LabelStoreNever = "never"
)

// Commands returns every command line the fake received, tag stripped, in
// order (`UID STORE 3 -X-GM-LABELS (\Inbox)`).
func (s *Server) Commands() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.commands...)
}

// MutatingCommands returns the commands that change the mailbox: STORE,
// COPY, MOVE, EXPUNGE, CREATE, APPEND, DELETE, RENAME (UID forms included).
func (s *Server) MutatingCommands() []string {
	var out []string
	for _, c := range s.Commands() {
		f := strings.Fields(strings.ToUpper(c))
		if len(f) == 0 {
			continue
		}
		name := f[0]
		if name == "UID" && len(f) > 1 {
			name = f[1]
		}
		switch name {
		case "STORE", "COPY", "MOVE", "EXPUNGE", "CREATE", "APPEND", "DELETE", "RENAME":
			out = append(out, c)
		}
	}
	return out
}

// Expunged returns "mailbox uid" for every message an EXPUNGE removed.
func (s *Server) Expunged() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.expunged...)
}

// Message returns a copy of the message at uid in mailbox (nil if none).
func (s *Server) Message(mailbox string, uid uint32) *Message {
	s.mu.Lock()
	defer s.mu.Unlock()
	mb := s.find(mailbox)
	if mb == nil {
		return nil
	}
	for _, m := range mb.Messages {
		if m.UID == uid {
			cp := *m
			cp.Labels = append([]string(nil), m.Labels...)
			cp.Flags = append([]string(nil), m.Flags...)
			return &cp
		}
	}
	return nil
}

// FindMsgID returns the UIDs in mailbox holding the message with this
// X-GM-MSGID.
func (s *Server) FindMsgID(mailbox string, msgID uint64) []uint32 {
	s.mu.Lock()
	defer s.mu.Unlock()
	mb := s.find(mailbox)
	if mb == nil {
		return nil
	}
	var out []uint32
	for _, m := range mb.Messages {
		if m.MsgID == msgID {
			out = append(out, m.UID)
		}
	}
	return out
}

// HasMailbox reports whether a mailbox (label view) exists.
func (s *Server) HasMailbox(name string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.find(name) != nil
}

// MoveMessage moves a message between mailboxes the way Gmail's own apps
// would (for a test simulating "trashed in Gmail, then restored"), giving it
// a new UID in the destination.
func (s *Server) MoveMessage(from string, uid uint32, to string) uint32 {
	s.mu.Lock()
	defer s.mu.Unlock()
	src := s.find(from)
	if src == nil {
		panic("gmailtest: no mailbox " + from)
	}
	for _, m := range src.Messages {
		if m.UID == uid {
			return s.gmailMove(src, m, to, true)
		}
	}
	panic(fmt.Sprintf("gmailtest: no uid %d in %s", uid, from))
}

func (s *Server) special(attr string) *Mailbox {
	for _, mb := range s.mailboxes {
		for _, a := range mb.Attrs {
			if strings.EqualFold(a, attr) {
				return mb
			}
		}
	}
	return nil
}

func hasAttr(mb *Mailbox, attr string) bool {
	for _, a := range mb.Attrs {
		if strings.EqualFold(a, attr) {
			return true
		}
	}
	return false
}

// add appends a copy of m to mb at UIDNEXT (lock held).
func (s *Server) add(mb *Mailbox, m *Message) *Message {
	cp := *m
	cp.Labels = append([]string(nil), m.Labels...)
	cp.Flags = append([]string(nil), m.Flags...)
	cp.UID = mb.UIDNext
	mb.UIDNext++
	s.modseq++
	cp.ModSeq = s.modseq
	mb.Messages = append(mb.Messages, &cp)
	return &cp
}

// removeMsgID takes every copy of a message out of every mailbox except keep
// (lock held): Gmail's "one message, many labels".
func (s *Server) removeMsgID(msgID uint64, keep *Mailbox) {
	if msgID == 0 {
		return
	}
	for _, mb := range s.mailboxes {
		if mb == keep {
			continue
		}
		kept := mb.Messages[:0]
		for _, m := range mb.Messages {
			if m.MsgID != msgID {
				kept = append(kept, m)
			}
		}
		mb.Messages = kept
	}
	s.modseq++
}

// gmailMove applies a MOVE (remove=true) or COPY of m from src to the named
// mailbox with Gmail's meaning, and returns the new UID (lock held).
func (s *Server) gmailMove(src *Mailbox, m *Message, to string, remove bool) uint32 {
	dst := s.find(to)
	if dst == nil {
		return 0
	}
	all := s.special(`\All`)
	fromBin := hasAttr(src, `\Trash`) || hasAttr(src, `\Junk`)
	if s.MoveDrops && remove && fromBin {
		s.removeUID(src, m.UID)
		return 0
	}
	switch {
	case hasAttr(dst, `\Trash`) || hasAttr(dst, `\Junk`):
		// Out of All Mail and every view, into Trash/Spam. (A copy already
		// in the destination is left alone, so a test can plant a stray.)
		moved := *m
		s.removeMsgID(m.MsgID, dst)
		if remove {
			s.removeUID(src, m.UID)
		}
		return s.add(dst, &moved).UID
	case fromBin:
		// Back into All Mail at a new UID, labelled by where it went, and
		// into that view (INBOX or the label) too.
		back := *m
		back.Labels = append([]string(nil), m.Labels...)
		switch {
		case strings.EqualFold(dst.Name, "INBOX"):
			back.Labels = append(back.Labels, `\Inbox`)
		case dst == all:
		default:
			back.Labels = append(back.Labels, dst.Name)
		}
		if remove {
			s.removeUID(src, m.UID)
		}
		if all == nil {
			return s.add(dst, &back).UID
		}
		if dst != all {
			s.add(dst, &back)
		}
		return s.add(all, &back).UID
	default:
		// Between All Mail and views: Gmail adds the destination's label.
		if strings.EqualFold(dst.Name, "INBOX") {
			m.Labels = append(m.Labels, `\Inbox`)
		} else if dst != all {
			m.Labels = append(m.Labels, dst.Name)
		}
		return m.UID
	}
}

// findFold finds a mailbox by name case-insensitively (lock held).
func (s *Server) findFold(name string) *Mailbox {
	for _, mb := range s.mailboxes {
		if strings.EqualFold(mb.Name, name) {
			return mb
		}
	}
	return nil
}

// dropMsgID removes a message from one view (lock held).
func (s *Server) dropMsgID(mb *Mailbox, msgID uint64) {
	if msgID == 0 {
		return
	}
	kept := mb.Messages[:0]
	for _, m := range mb.Messages {
		if m.MsgID != msgID {
			kept = append(kept, m)
		}
	}
	mb.Messages = kept
}

// AddToTrash puts a stray copy with this X-GM-MSGID in Trash (for the test
// of a Trash search that matches more than one message).
func (s *Server) AddToTrash(msgID uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if t := s.special(`\Trash`); t != nil {
		s.add(t, &Message{MsgID: msgID, Labels: []string{}, Body: []byte("x")})
	}
}

func (s *Server) removeUID(mb *Mailbox, uid uint32) {
	kept := mb.Messages[:0]
	for _, m := range mb.Messages {
		if m.UID != uid {
			kept = append(kept, m)
		}
	}
	mb.Messages = kept
}

func (c *conn) create(tag, args string) {
	s := c.s
	name, _ := unquoteArg(args)
	decoded, err := decodeMUTF7(name)
	if err != nil {
		decoded = name
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.find(decoded) != nil {
		c.put("%s NO [ALREADYEXISTS] Duplicate folder name %s (Failure)", tag, name)
		return
	}
	s.mailboxes = append(s.mailboxes, &Mailbox{Name: decoded, Attrs: []string{`\HasNoChildren`}, UIDValidity: 1, UIDNext: 1})
	c.put("%s OK Success", tag)
}

func (c *conn) uidStore(tag, args string) {
	s := c.s
	if c.selected == nil {
		c.put("%s BAD No mailbox selected", tag)
		return
	}
	setStr, rest, _ := strings.Cut(strings.TrimSpace(args), " ")
	item, list, _ := strings.Cut(strings.TrimSpace(rest), " ")
	item = strings.ToUpper(item)
	silent := strings.HasSuffix(item, ".SILENT")
	item = strings.TrimSuffix(item, ".SILENT")
	op := byte(0)
	if strings.HasPrefix(item, "+") || strings.HasPrefix(item, "-") {
		op, item = item[0], item[1:]
	}
	values, ok := parseList(list)
	if !ok || (item != "FLAGS" && item != "X-GM-LABELS") {
		c.put("%s BAD Could not parse command", tag)
		return
	}
	if item == "X-GM-LABELS" && !s.GmailExt {
		c.put("%s BAD Could not parse command", tag)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if item == "X-GM-LABELS" {
		for i, v := range values {
			if !strings.HasPrefix(v, `\`) {
				if d, err := decodeMUTF7(v); err == nil {
					values[i] = d
				}
				// Gmail's label names are case-insensitive: an existing
				// label is applied under its own spelling.
				if mb := s.findFold(values[i]); mb != nil {
					values[i] = mb.Name
				}
			}
		}
		if op == '+' && s.LabelCreate == LabelStoreNO {
			for _, v := range values {
				if !strings.HasPrefix(v, `\`) && s.find(v) == nil {
					c.put("%s NO [NONEXISTENT] No label %s (Failure)", tag, quote(v))
					return
				}
			}
		}
	}
	for seq, m := range c.selected.Messages {
		if !inSet(setStr, m.UID, c.selected.UIDNext) {
			continue
		}
		switch item {
		case "FLAGS":
			m.Flags = applySet(m.Flags, values, op, false)
		case "X-GM-LABELS":
			apply := values
			if op == '+' && s.LabelCreate != LabelStoreNO {
				apply = nil
				for _, v := range values {
					known := s.find(v) != nil && s.LabelCreate != LabelStoreNever
					if strings.HasPrefix(v, `\`) || known || s.LabelCreate == LabelStoreCreates {
						apply = append(apply, v)
						if !strings.HasPrefix(v, `\`) && s.find(v) == nil {
							s.mailboxes = append(s.mailboxes, &Mailbox{Name: v, Attrs: []string{`\HasNoChildren`}, UIDValidity: 1, UIDNext: 1})
						}
					}
				}
			}
			m.Labels = applySet(m.Labels, apply, op, true)
			if op == '-' {
				// Taking \Inbox off takes the message out of the INBOX view.
				for _, v := range apply {
					if strings.EqualFold(v, `\Inbox`) {
						if inbox := s.find("INBOX"); inbox != nil && inbox != c.selected {
							s.dropMsgID(inbox, m.MsgID)
						}
					}
				}
			}
		}
		s.modseq++
		m.ModSeq = s.modseq
		if silent {
			continue
		}
		if item == "FLAGS" {
			c.put("* %d FETCH (FLAGS (%s) UID %d)", seq+1, strings.Join(m.Flags, " "), m.UID)
		} else {
			c.put("* %d FETCH (X-GM-LABELS (%s) UID %d)", seq+1, wireLabels(m), m.UID)
		}
	}
	c.put("%s OK Success", tag)
}

// applySet applies a STORE op to a set; labels compare system labels
// case-insensitively.
func applySet(cur, values []string, op byte, _ bool) []string {
	key := strings.ToLower
	switch op {
	case 0:
		return append([]string(nil), values...)
	case '+':
		have := map[string]bool{}
		for _, v := range cur {
			have[key(v)] = true
		}
		out := append([]string(nil), cur...)
		for _, v := range values {
			if !have[key(v)] {
				out = append(out, v)
				have[key(v)] = true
			}
		}
		return out
	default:
		drop := map[string]bool{}
		for _, v := range values {
			drop[key(v)] = true
		}
		var out []string
		for _, v := range cur {
			if !drop[key(v)] {
				out = append(out, v)
			}
		}
		if out == nil {
			out = []string{}
		}
		return out
	}
}

func (c *conn) uidMoveCopy(tag, verb, args string) {
	s := c.s
	if c.selected == nil {
		c.put("%s BAD No mailbox selected", tag)
		return
	}
	if verb == "MOVE" && s.NoMove {
		c.put("%s BAD Unknown command", tag)
		return
	}
	setStr, rest, _ := strings.Cut(strings.TrimSpace(args), " ")
	name, _ := unquoteArg(rest)
	decoded, err := decodeMUTF7(name)
	if err != nil {
		decoded = name
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.find(decoded) == nil {
		c.put("%s NO [TRYCREATE] No folder %s (Failure)", tag, name)
		return
	}
	var picked []*Message
	for _, m := range c.selected.Messages {
		if inSet(setStr, m.UID, c.selected.UIDNext) {
			picked = append(picked, m)
		}
	}
	for _, m := range picked {
		seq := 0
		for i, x := range c.selected.Messages {
			if x == m {
				seq = i + 1
			}
		}
		s.gmailMove(c.selected, m, decoded, verb == "MOVE")
		if verb == "MOVE" && seq > 0 && !containsMsg(c.selected, m) {
			c.put("* %d EXPUNGE", seq)
		}
	}
	c.put("%s OK Success", tag)
}

func containsMsg(mb *Mailbox, m *Message) bool {
	for _, x := range mb.Messages {
		if x == m {
			return true
		}
	}
	return false
}

func (c *conn) expunge(tag, setStr string) {
	s := c.s
	if c.selected == nil {
		c.put("%s BAD No mailbox selected", tag)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	mb := c.selected
	for i := 0; i < len(mb.Messages); {
		m := mb.Messages[i]
		deleted := false
		for _, f := range m.Flags {
			if strings.EqualFold(f, `\Deleted`) {
				deleted = true
			}
		}
		if deleted && (setStr == "" || inSet(strings.TrimSpace(setStr), m.UID, mb.UIDNext)) {
			mb.Messages = append(mb.Messages[:i], mb.Messages[i+1:]...)
			s.expunged = append(s.expunged, fmt.Sprintf("%s %d", mb.Name, m.UID))
			s.modseq++
			c.put("* %d EXPUNGE", i+1)
			continue
		}
		i++
	}
	c.put("%s OK Success", tag)
}

// parseSearch understands the keys the sync and writeback send: ALL, UID
// <set>, X-GM-MSGID <n>, HEADER <field> <value> (Message-Id only), ANDed.
func parseSearch(args string, uidNext uint32) (func(*Message) bool, bool) {
	toks, ok := tokens(args)
	if !ok {
		return nil, false
	}
	var preds []func(*Message) bool
	for i := 0; i < len(toks); i++ {
		switch strings.ToUpper(toks[i]) {
		case "ALL":
		case "UID":
			if i+1 >= len(toks) {
				return nil, false
			}
			set := toks[i+1]
			i++
			preds = append(preds, func(m *Message) bool { return inSet(set, m.UID, uidNext) })
		case "X-GM-MSGID":
			if i+1 >= len(toks) {
				return nil, false
			}
			n, err := strconv.ParseUint(toks[i+1], 10, 64)
			if err != nil {
				return nil, false
			}
			i++
			preds = append(preds, func(m *Message) bool { return m.MsgID == n })
		case "HEADER":
			if i+2 >= len(toks) || !strings.EqualFold(toks[i+1], "Message-Id") {
				return nil, false
			}
			want := strings.Trim(toks[i+2], "<>")
			i += 2
			preds = append(preds, func(m *Message) bool { return strings.Trim(m.MessageID, "<>") == want })
		default:
			return nil, false
		}
	}
	return func(m *Message) bool {
		for _, p := range preds {
			if !p(m) {
				return false
			}
		}
		return true
	}, true
}

// tokens splits a command's arguments into atoms and (unquoted) quoted
// strings.
func tokens(s string) ([]string, bool) {
	var out []string
	s = strings.TrimSpace(s)
	for s != "" {
		if s[0] == '"' {
			v, rest := unquoteArg(s)
			out = append(out, v)
			s = strings.TrimSpace(rest)
			continue
		}
		v, rest, _ := strings.Cut(s, " ")
		out = append(out, v)
		s = strings.TrimSpace(rest)
	}
	return out, true
}

// parseList parses a parenthesized list of atoms and quoted strings, keeping
// a backslash atom's backslash.
func parseList(s string) ([]string, bool) {
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, "(") || !strings.HasSuffix(s, ")") {
		return nil, false
	}
	toks, ok := tokens(s[1 : len(s)-1])
	if toks == nil {
		toks = []string{}
	}
	return toks, ok
}
