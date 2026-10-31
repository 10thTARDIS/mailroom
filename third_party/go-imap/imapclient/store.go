package imapclient

import (
	"fmt"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/internal/imapwire"
)

// Store sends a STORE command.
//
// Unless StoreFlags.Silent is set, the server will return the updated values.
//
// A nil options pointer is equivalent to a zero options value.
func (c *Client) Store(numSet imap.NumSet, store *imap.StoreFlags, options *imap.StoreOptions) *FetchCommand {
	cmd := &FetchCommand{
		numSet: numSet,
		msgs:   make(chan *FetchMessageData, 128),
	}
	enc := c.beginCommand(uidCmdName("STORE", imapwire.NumSetKind(numSet)), cmd)
	enc.SP().NumSet(numSet).SP()
	if options != nil && options.UnchangedSince != 0 {
		enc.Special('(').Atom("UNCHANGEDSINCE").SP().ModSeq(options.UnchangedSince).Special(')').SP()
	}
	switch store.Op {
	case imap.StoreFlagsSet:
		// nothing to do
	case imap.StoreFlagsAdd:
		enc.Special('+')
	case imap.StoreFlagsDel:
		enc.Special('-')
	default:
		panic(fmt.Errorf("imapclient: unknown store flags op: %v", store.Op))
	}
	enc.Atom("FLAGS")
	if store.Silent {
		enc.Atom(".SILENT")
	}
	enc.SP().List(len(store.Flags), func(i int) {
		enc.Flag(store.Flags[i])
	})
	enc.end()
	return cmd
}

// StoreGmailLabels sends a STORE command that alters Gmail labels:
// `STORE <set> [(UNCHANGEDSINCE n)] ±X-GM-LABELS[.SILENT] (<labels>)`.
// This requires the X-GM-EXT-1 extension. Mailroom patch, see
// MAILROOM-PATCHES.md.
//
// Unless store.Silent is set, Gmail answers with an untagged FETCH carrying
// the message's labels after the change (X-GM-LABELS), which the returned
// FetchCommand delivers like any FETCH response. Each label is written by
// imapwire.Encoder.GmailLabel: system labels as atoms, user labels as quoted
// modified UTF-7.
//
// A nil options pointer is equivalent to a zero options value.
func (c *Client) StoreGmailLabels(numSet imap.NumSet, store *imap.StoreGmailLabels, options *imap.StoreOptions) *FetchCommand {
	cmd := &FetchCommand{
		numSet: numSet,
		msgs:   make(chan *FetchMessageData, 128),
	}
	enc := c.beginCommand(uidCmdName("STORE", imapwire.NumSetKind(numSet)), cmd)
	enc.SP().NumSet(numSet).SP()
	if options != nil && options.UnchangedSince != 0 {
		enc.Special('(').Atom("UNCHANGEDSINCE").SP().ModSeq(options.UnchangedSince).Special(')').SP()
	}
	switch store.Op {
	case imap.StoreFlagsSet:
		// nothing to do
	case imap.StoreFlagsAdd:
		enc.Special('+')
	case imap.StoreFlagsDel:
		enc.Special('-')
	default:
		panic(fmt.Errorf("imapclient: unknown store flags op: %v", store.Op))
	}
	enc.Atom("X-GM-LABELS")
	if store.Silent {
		enc.Atom(".SILENT")
	}
	enc.SP().List(len(store.Labels), func(i int) {
		enc.GmailLabel(store.Labels[i])
	})
	enc.end()
	return cmd
}
