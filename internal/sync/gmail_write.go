// SPDX-License-Identifier: AGPL-3.0-only

package sync

import (
	"context"
	"fmt"
	"strings"

	"github.com/emersion/go-imap/v2"
)

// GmailWriteSession is the Gmail X-GM-EXT-1 WRITE surface of a session (#52,
// ADR-047 §3, increment 5b). It is deliberately NOT part of SyncSession: only
// the writeback engine's Gmail path needs it, and every fake SyncSession in
// the tree would otherwise have to grow five methods it never uses. The real
// session implements it; a caller asserts for it and checks
// Supports(CapabilityGmailExt) before relying on it.
//
// Labels are DECODED names throughout, in both directions: a system label
// keeps its backslash (`\Inbox`), a user label is the exact name ("été").
// The vendored go-imap patch (third_party/go-imap/MAILROOM-PATCHES.md, patch
// 3) does the modified UTF-7 and the quoting on the wire.
type GmailWriteSession interface {
	// StoreGmailLabels adds and then removes Gmail labels on one message
	// (UID STORE +X-GM-LABELS, then UID STORE -X-GM-LABELS; one command when
	// only one side is non-empty, none when both are empty). The STOREs are
	// NOT silent, so Gmail echoes the message's labels after the change:
	// labels is the last echo for uid and echoed says whether there was one.
	// No echo is what a UID STORE naming a UID the mailbox no longer holds
	// looks like (the command succeeds and touches nothing), so a caller must
	// not read echoed=false as success.
	StoreGmailLabels(ctx context.Context, mailbox string, uid UID, add, remove []string) (labels []string, echoed bool, err error)
	// FetchGmail reads one message's X-GM-MSGID and X-GM-LABELS. found is
	// false when the mailbox holds no such UID. labels is nil when the
	// response carried no X-GM-LABELS reading at all (#52 review H1: nil is
	// "no reading", never "no labels").
	FetchGmail(ctx context.Context, mailbox string, uid UID) (msgID uint64, labels []string, found bool, err error)
	// SearchGmailMsgID returns the UIDs in mailbox of the message with this
	// X-GM-MSGID (UID SEARCH X-GM-MSGID n). Gmail's id is unique per message,
	// so the answer is zero or one UID; an empty answer means the message is
	// not in that mailbox.
	SearchGmailMsgID(ctx context.Context, mailbox string, msgID uint64) ([]UID, error)
	// CreateMailbox issues CREATE. On Gmail that creates a label.
	CreateMailbox(ctx context.Context, name string) error
}

var _ GmailWriteSession = (*session)(nil)

func (s *session) StoreGmailLabels(ctx context.Context, mailbox string, uid UID, add, remove []string) ([]string, bool, error) {
	if len(add) == 0 && len(remove) == 0 {
		return nil, false, nil
	}
	if err := validGmailLabels(add); err != nil {
		return nil, false, &Error{Kind: ErrorKindProtocol, Op: "store", Err: err}
	}
	if err := validGmailLabels(remove); err != nil {
		return nil, false, &Error{Kind: ErrorKindProtocol, Op: "store", Err: err}
	}
	var (
		labels []string
		echoed bool
	)
	err := s.withContext(ctx, "store", func() error {
		if err := s.ensureWritableSelect(mailbox); err != nil {
			return err
		}
		numSet := uidSetToIMAP(UIDSetNum(uid))
		run := func(op imap.StoreFlagsOp, names []string) error {
			if len(names) == 0 {
				return nil
			}
			msgs, err := s.client.StoreGmailLabels(numSet, &imap.StoreGmailLabels{
				Op: op, Labels: names,
			}, nil).Collect()
			if err != nil {
				return err
			}
			// Each STORE's own echo decides: an add that echoed followed by a
			// remove that did not means the message went away in between.
			echoed = false
			for _, m := range msgs {
				if m.UID == imap.UID(uid) && m.GmailLabels != nil {
					labels, echoed = m.GmailLabels, true
				}
			}
			return nil
		}
		if err := run(imap.StoreFlagsAdd, add); err != nil {
			return err
		}
		if len(add) > 0 && len(remove) > 0 {
			// THE REMOVE WAITS FOR THE ADD (#911 review F5). A move into a
			// label view is +label then -\Inbox; if the label did not stick,
			// taking \Inbox off would archive the message with nothing to
			// show where it went. So the add's echo — or, with none, a FETCH
			// — must show every added label before the remove is sent. If
			// it does not, the add's state is returned and the caller's
			// read-back check reports the missing label.
			if !echoed {
				msgs, err := s.client.Fetch(numSet, &imap.FetchOptions{UID: true, GmailLabels: true}).Collect()
				if err != nil {
					return err
				}
				for _, m := range msgs {
					if m.UID == imap.UID(uid) && m.GmailLabels != nil {
						labels, echoed = m.GmailLabels, true
					}
				}
			}
			if !echoed || !containsAllFold(labels, add) {
				return nil
			}
		}
		return run(imap.StoreFlagsDel, remove)
	})
	return labels, echoed, err
}

func (s *session) FetchGmail(ctx context.Context, mailbox string, uid UID) (uint64, []string, bool, error) {
	var (
		msgID  uint64
		labels []string
		found  bool
	)
	err := s.withContext(ctx, "fetch", func() error {
		if err := s.ensureWritableSelect(mailbox); err != nil {
			return err
		}
		msgs, err := s.client.Fetch(uidSetToIMAP(UIDSetNum(uid)), &imap.FetchOptions{
			UID: true, GmailMsgID: true, GmailLabels: true,
		}).Collect()
		if err != nil {
			return err
		}
		for _, m := range msgs {
			if m.UID != imap.UID(uid) {
				continue
			}
			found = true
			if m.GmailMsgID != 0 {
				msgID = m.GmailMsgID
			}
			if m.GmailLabels != nil {
				labels = m.GmailLabels
			}
		}
		return nil
	})
	return msgID, labels, found, err
}

func (s *session) SearchGmailMsgID(ctx context.Context, mailbox string, msgID uint64) ([]UID, error) {
	if msgID == 0 {
		return nil, &Error{Kind: ErrorKindProtocol, Op: "search", Err: fmt.Errorf("sync: search: zero X-GM-MSGID")}
	}
	var uids []UID
	err := s.withContext(ctx, "search", func() error {
		if err := s.ensureWritableSelect(mailbox); err != nil {
			return err
		}
		// The same per-account SEARCH budget as every other SEARCH (#638).
		if err := s.awaitSearchSlot(ctx); err != nil {
			return err
		}
		data, err := s.client.UIDSearch(&imap.SearchCriteria{GmailMsgID: []uint64{msgID}}, nil).Wait()
		if err != nil {
			return err
		}
		for _, u := range data.AllUIDs() {
			uids = append(uids, UID(u))
		}
		return nil
	})
	return uids, err
}

func (s *session) CreateMailbox(ctx context.Context, name string) error {
	return s.withContext(ctx, "create", func() error {
		return s.client.Create(name, nil).Wait()
	})
}

// ValidGmailLabel reports whether a decoded label name can be written to
// Gmail unambiguously. A system label is a backslash followed by an atom
// (`\Inbox`). A user label must be non-empty and must not begin with a
// backslash: Gmail reads a leading backslash as a system label, so such a
// name cannot round-trip (the FETCH parser itself unquotes "\\Important" to
// `\Important`).
func ValidGmailLabel(label string) bool {
	if label == "" || label == `\` {
		return false
	}
	if !strings.HasPrefix(label, `\`) {
		return true
	}
	for i := 1; i < len(label); i++ {
		switch c := label[i]; {
		case c <= 0x20, c >= 0x7f, strings.IndexByte(`(){%*"\]`, c) >= 0:
			return false
		}
	}
	return true
}

// containsAllFold reports whether have contains every label of want, case-
// insensitively (Gmail label names are).
func containsAllFold(have, want []string) bool {
	for _, w := range want {
		found := false
		for _, h := range have {
			if strings.EqualFold(h, w) {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

func validGmailLabels(labels []string) error {
	for _, l := range labels {
		if !ValidGmailLabel(l) {
			return fmt.Errorf("sync: %q cannot be written as a Gmail label", l)
		}
	}
	return nil
}
