// SPDX-License-Identifier: AGPL-3.0-only

package sync

import (
	"context"
	"errors"
	"time"
)

// Writer adapts a SyncSession's batch (UIDSet) write methods to the
// single-UID method shapes mailroom/internal/writeback.IMAPWriter expects.
// The writeback engine (issue #11) applies one writeback intent — one
// message — at a time, but IMAP's UID COPY/MOVE/EXPUNGE naturally batch, so
// SyncSession exposes the batch form and Writer narrows it back down.
//
// Writer's method set is spelled with exactly the same types
// (context.Context, string, UID, []Flag, []byte, error/bool) that
// writeback.IMAPWriter uses for its own methods (writeback imports this
// package as imapsync and uses imapsync.UID/imapsync.Flag directly), so a
// *Writer satisfies writeback.IMAPWriter structurally — Go interface
// satisfaction never requires an import. This package deliberately does not
// import mailroom/internal/writeback: writeback already imports
// mailroom/internal/sync, so the reverse import would cycle. The caller
// (writeback wiring, or a test) assigns a *sync.Writer wherever a
// writeback.IMAPWriter value is needed; the compiler checks the match at
// that assignment site.
type Writer struct {
	// Session is the underlying SyncSession every method delegates to.
	Session SyncSession
}

// NewWriter wraps s so it can be used as a writeback.IMAPWriter.
func NewWriter(s SyncSession) *Writer {
	return &Writer{Session: s}
}

// StoreFlags delegates directly: SyncSession.StoreFlags already takes a
// single UID.
func (w *Writer) StoreFlags(ctx context.Context, mailbox string, uid UID, add, remove []Flag) error {
	return w.Session.StoreFlags(ctx, mailbox, uid, add, remove)
}

// Copy narrows uid to a one-element UIDSet for SyncSession.Copy.
func (w *Writer) Copy(ctx context.Context, mailbox string, uid UID, dest string) error {
	return w.Session.Copy(ctx, mailbox, UIDSetNum(uid), dest)
}

// Move narrows uid to a one-element UIDSet for SyncSession.Move.
func (w *Writer) Move(ctx context.Context, mailbox string, uid UID, dest string) error {
	return w.Session.Move(ctx, mailbox, UIDSetNum(uid), dest)
}

// Expunge narrows uid to a one-element UIDSet for SyncSession.Expunge and
// passes the server's removal count straight through: 0 means the UID EXPUNGE
// succeeded without the server removing anything, which is the one thing the
// writeback engine's Expunged tally must be able to see (#296).
func (w *Writer) Expunge(ctx context.Context, mailbox string, uid UID) (int, error) {
	return w.Session.Expunge(ctx, mailbox, UIDSetNum(uid))
}

// Append delegates with a zero internalDate (SyncSession.Append lets the server
// assign one in that case) and passes the server's APPENDUID back to the caller.
//
// It used to discard that UID, on the reasoning that a subsequent sync pass
// would learn the message's UID anyway. That is still how PROVENANCE is
// resolved — but "eventually" was not soon enough for the one caller that needs
// it now: the writeback engine expunges a superseded draft moments after
// appending it, and a SEARCH that soon after our own APPEND has been observed to
// come back empty on a real provider (#268). The UID the server volunteered at
// APPEND time is the answer to a question we would otherwise have to ask it
// again. It is 0 when the server does not support UIDPLUS and so never said.
func (w *Writer) Append(ctx context.Context, mailbox string, flags []Flag, body []byte) (UID, error) {
	return w.Session.Append(ctx, mailbox, flags, time.Time{}, body)
}

// SearchMessageID delegates directly. The writeback engine uses it for the Sent
// self-append dedup (is the provider's own copy already there?) and to identify
// the copy of a superseded draft it must expunge — see writeback.IMAPWriter,
// which the engine documents the error semantics on.
func (w *Writer) SearchMessageID(ctx context.Context, mailbox string, messageID string) ([]UID, error) {
	return w.Session.SearchMessageID(ctx, mailbox, messageID)
}

// UIDPresent delegates directly: SyncSession.UIDPresent already takes a
// single UID, the same shape writeback.IMAPWriter needs (#611).
func (w *Writer) UIDPresent(ctx context.Context, mailbox string, uid UID) (bool, error) {
	return w.Session.UIDPresent(ctx, mailbox, uid)
}

// SupportsMove reports Session.Supports(CapabilityMove).
func (w *Writer) SupportsMove() bool {
	return w.Session.Supports(CapabilityMove)
}

// errNoGmailWrite is what the Gmail methods return when the wrapped session
// does not implement GmailWriteSession (a test fake, or a future session
// type). The engine checks SupportsGmailExt first, so reaching it means a
// wiring mistake, and it fails the write rather than guessing.
var errNoGmailWrite = &Error{Kind: ErrorKindProtocol, Op: "gmail",
	Err: errors.New("sync: this session has no Gmail write support")}

// SupportsGmailExt reports Session.Supports(CapabilityGmailExt) AND that the
// session implements GmailWriteSession. The writeback engine needs both
// before it may take its Gmail path (#52, ADR-047 §3).
func (w *Writer) SupportsGmailExt() bool {
	_, ok := w.Session.(GmailWriteSession)
	return ok && w.Session.Supports(CapabilityGmailExt)
}

// SupportsUIDPlus reports Session.Supports(CapabilityUIDPlus). The Gmail
// writeback path refuses to run without it (#911 review), so an EXPUNGE can
// never fall back to a plain one.
func (w *Writer) SupportsUIDPlus() bool {
	return w.Session.Supports(CapabilityUIDPlus)
}

// StoreGmailLabels delegates to the session's GmailWriteSession.
func (w *Writer) StoreGmailLabels(ctx context.Context, mailbox string, uid UID, add, remove []string) ([]string, bool, error) {
	g, ok := w.Session.(GmailWriteSession)
	if !ok {
		return nil, false, errNoGmailWrite
	}
	return g.StoreGmailLabels(ctx, mailbox, uid, add, remove)
}

// FetchGmail delegates to the session's GmailWriteSession.
func (w *Writer) FetchGmail(ctx context.Context, mailbox string, uid UID) (uint64, []string, bool, error) {
	g, ok := w.Session.(GmailWriteSession)
	if !ok {
		return 0, nil, false, errNoGmailWrite
	}
	return g.FetchGmail(ctx, mailbox, uid)
}

// SearchGmailMsgID delegates to the session's GmailWriteSession.
func (w *Writer) SearchGmailMsgID(ctx context.Context, mailbox string, msgID uint64) ([]UID, error) {
	g, ok := w.Session.(GmailWriteSession)
	if !ok {
		return nil, errNoGmailWrite
	}
	return g.SearchGmailMsgID(ctx, mailbox, msgID)
}

// CreateMailbox delegates to the session's GmailWriteSession.
func (w *Writer) CreateMailbox(ctx context.Context, name string) error {
	g, ok := w.Session.(GmailWriteSession)
	if !ok {
		return errNoGmailWrite
	}
	return g.CreateMailbox(ctx, name)
}
