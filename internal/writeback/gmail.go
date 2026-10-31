// SPDX-License-Identifier: AGPL-3.0-only

package writeback

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"mailroom/internal/store"
	imapsync "mailroom/internal/sync"
)

// GMAIL WRITEBACK (#52, ADR-047 §3, increment 5b).
//
// A live Gmail OAuth account syncs [Gmail]/All Mail, Spam and Trash only, so
// every copy the engine can address lives in one of those three, and Gmail's
// folders are LABELS on it. The generic engine would treat them as folders —
// a "move to Archive" as a MOVE, a label as a keyword nobody in Gmail sees, a
// delete as STORE \Deleted + EXPUNGE whose effect depends on the person's
// Gmail IMAP settings. In Gmail mode (WithGmail) each intent is translated to
// Gmail's own primitive instead:
//
//	intent                         copy in All Mail                copy in Trash / Spam
//	------------------------------ ------------------------------- ----------------------------
//	±flag (\Seen, \Flagged=Star)   UID STORE ±FLAGS                same
//	±kw carrying a label name      UID STORE ±X-GM-LABELS ("name") same
//	±kw without one (a keyword)    UID STORE ±FLAGS                same
//	move → INBOX                   +X-GM-LABELS (\Inbox)           UID MOVE → INBOX
//	move → All Mail (archive)      -X-GM-LABELS (\Inbox)           UID MOVE → INBOX, checked,
//	                                                               then -X-GM-LABELS (\Inbox)
//	move → Trash / Spam            UID MOVE → Trash / Spam         UID MOVE (no-op if there)
//	move → a user-label view L     +X-GM-LABELS (L) -(\Inbox)      UID MOVE → L
//	move → Sent/Drafts/Starred/... refused (dead-lettered)         refused
//	expunge (hard delete)          refused (dead-lettered)         EXPUNGE there
//	draft retirement (by header)   Drafts view only: must be an unsent draft; MOVE → Trash,
//	                               found there by X-GM-MSGID, then EXPUNGE there
//
// Every MOVE out of Trash or Spam is checked: the message must then be found
// by X-GM-MSGID exactly once in the destination, or the intent dead-letters.
//
// THE RULES THAT MAKE IT SAFE, each enforced here and each with a negative
// control in gmail_test.go:
//
//   - NO EXPUNGE OUTSIDE TRASH AND SPAM, and no STORE \Deleted outside them
//     either (Gmail's default auto-expunge makes \Deleted itself
//     destructive). expungeOne is only ever called with the Trash or Spam
//     mailbox in this file. A server without MOVE is refused rather than
//     letting go-imap fall back to COPY + \Deleted + EXPUNGE.
//   - A LABEL REMOVAL IS A LABEL REMOVAL: -X-GM-LABELS, nothing else.
//   - THE COPY IS FOUND BY X-GM-MSGID, NEVER GUESSED. Before the first write
//     to a copy in a pass, UID SEARCH X-GM-MSGID in the copy's mailbox gives
//     its current UID (it changes when a message leaves All Mail and comes
//     back). Not found is a retry, never a write to the stored UID. A copy
//     with no stored X-GM-MSGID falls back to its stored UID, which the
//     provenance conflict checks still guard.
//   - A LABEL WRITE IS CONFIRMED BY READING IT BACK. The STORE is not silent,
//     so Gmail echoes the labels after the change; with no echo the labels
//     are FETCHed. A label that is not there afterwards is a failure (retried,
//     then dead-lettered), never "applied". Whether STORE creates a missing
//     user label is UNVERIFIED (ADR-047, Consequences), so both are handled:
//     an add that fails or does not stick CREATEs the label and is retried
//     once.
//   - ECHO SUPPRESSION: a confirmed label write moves the copy's gm_labels
//     baseline by exactly that delta (store.RecordGmailLabelDelta), so the
//     next reading of an unchanged Gmail diffs to nothing instead of
//     reporting Mailroom's own write as a change made in Gmail.
//   - WRITES GO ONLY TO THE OWNING ACCOUNT. An intent is per account and its
//     targets are that account's own live copies
//     (MessageProvenanceTargetsForAccount); labels being user-wide does not
//     widen that.

// GmailWriter is the Gmail write surface the engine's Gmail mode needs, on
// top of IMAPWriter. *imapsync.Writer implements it.
type GmailWriter interface {
	// SupportsGmailExt reports that the server advertised X-GM-EXT-1 and the
	// session can write Gmail labels.
	SupportsGmailExt() bool
	// SupportsUIDPlus reports UIDPLUS: Gmail mode refuses to run without it
	// rather than let an EXPUNGE fall back to a plain one that sweeps every
	// \Deleted message in the mailbox.
	SupportsUIDPlus() bool
	// StoreGmailLabels adds then removes labels on one message and returns
	// the labels Gmail echoed afterwards (echoed=false: no echo).
	StoreGmailLabels(ctx context.Context, mailbox string, uid imapsync.UID, add, remove []string) (labels []string, echoed bool, err error)
	// FetchGmail reads one message's X-GM-MSGID and X-GM-LABELS (labels nil
	// = no reading; found=false = no such UID).
	FetchGmail(ctx context.Context, mailbox string, uid imapsync.UID) (msgID uint64, labels []string, found bool, err error)
	// SearchGmailMsgID returns the UIDs in mailbox with this X-GM-MSGID.
	SearchGmailMsgID(ctx context.Context, mailbox string, msgID uint64) ([]imapsync.UID, error)
	// CreateMailbox issues CREATE (a Gmail label).
	CreateMailbox(ctx context.Context, name string) error
}

// gmailInbox is Gmail's system label for the inbox.
const gmailInbox = `\Inbox`

// WithGmail puts the engine in Gmail mode for one Gmail OAuth account
// (ADR-047 §3). folders names the account's Gmail mailboxes. The runner sets
// it from the account row; nothing else should.
func WithGmail(folders store.GmailFolders) Option {
	return func(e *Engine) {
		e.gmail = true
		e.folders = folders
	}
}

// WithGmailWritebackHold replaces the runner's Gmail writeback hold check
// (default store.GmailWritebackHeld, which holds every Gmail OAuth account
// while store.GmailWritebackReady is false). It exists so tests can exercise
// the Gmail path with the hold lifted, as syncrunner.WithSyncHold does for
// sync; production never sets it.
func WithGmailWritebackHold(f func(authKind, providerProfile string) bool) Option {
	return func(e *Engine) {
		e.gmailHold = f
	}
}

// gmailKind is what a mailbox is on a Gmail account.
type gmailKind int

const (
	gmailOther    gmailKind = iota // a Gmail system view we never write into (Sent, Drafts, Starred, ...)
	gmailInboxBox                  // INBOX
	gmailAll                       // [Gmail]/All Mail
	gmailTrash                     // [Gmail]/Trash
	gmailSpam                      // [Gmail]/Spam
	gmailLabel                     // a user-label view
)

func (e *Engine) gmailKindOf(name string) gmailKind {
	f := e.folders
	switch {
	case name == "":
		return gmailOther
	case strings.EqualFold(name, "INBOX") || (f.Inbox != "" && name == f.Inbox):
		return gmailInboxBox
	case f.All != "" && name == f.All:
		return gmailAll
	case f.Trash != "" && name == f.Trash:
		return gmailTrash
	case f.Spam != "" && name == f.Spam:
		return gmailSpam
	case f.Reserved[name]:
		return gmailOther
	case strings.HasPrefix(name, "[Gmail]") || strings.HasPrefix(name, "[Google Mail]"):
		// The \Noselect parent, or a system view the last LIST did not mark:
		// never a label to invent.
		return gmailOther
	}
	return gmailLabel
}

// gmailKey identifies a message in a mailbox by X-GM-MSGID, for the pass's
// re-resolution cache.
type gmailKey struct {
	mailbox string
	msgID   uint64
}

// gmailCapable reports whether w can take the Gmail path, and the GmailWriter.
func gmailCapable(w IMAPWriter) (GmailWriter, bool) {
	g, ok := w.(GmailWriter)
	if !ok || !g.SupportsGmailExt() || !g.SupportsUIDPlus() {
		return nil, false
	}
	return g, true
}

// validateGmailOp is validateOp's Gmail half: what Gmail mode refuses
// outright, before anything reaches the server.
func validateGmailOp(op store.IntentOp, p intentPayload) (string, bool) {
	switch op {
	case store.IntentAddFlag, store.IntentRemoveFlag:
		if strings.EqualFold(p.Flag, string(imapsync.FlagDeleted)) {
			// Gmail auto-expunges a message flagged \Deleted (the default IMAP
			// setting), so \Deleted is itself destructive there. Deletion goes
			// through Trash (ADR-047 §3), never through this flag.
			return fmt.Sprintf("op %q: Gmail writeback never sets or clears \\Deleted; deletion goes through Trash", op), false
		}
	case store.IntentAddKeyword, store.IntentRemoveKeyword:
		if p.Label != "" && (strings.HasPrefix(p.Label, `\`) || !imapsync.ValidGmailLabel(p.Label)) {
			return fmt.Sprintf("label %q cannot be written to Gmail: a name beginning with a backslash is a Gmail system label", p.Label), false
		}
		if p.Label != "" && gmailReservedLabelName(p.Label) {
			return fmt.Sprintf("label %q cannot be written to Gmail: it is the name of a Gmail system folder, which Gmail would read as that folder (adding it could move the message, removing it could take it out of the inbox or Trash)", p.Label), false
		}
	}
	return "", true
}

// gmailSystemNames are Gmail's system folders by name. A Mailroom label
// with one of these names (any case) is never written as an X-GM-LABELS user
// label: Gmail may resolve "Inbox" or "Trash" to the system label itself, so a
// user's label removal could archive a message and a label add could trash
// it (#911 review F6). UNVERIFIED which of them Gmail actually conflates; all
// are refused.
var gmailSystemNames = map[string]bool{
	"inbox": true, "trash": true, "spam": true, "sent": true, "sent mail": true,
	"starred": true, "important": true, "drafts": true, "draft": true, "all mail": true,
	"chats": true, "snoozed": true, "scheduled": true,
}

func gmailReservedLabelName(name string) bool {
	l := strings.ToLower(strings.TrimSpace(name))
	return gmailSystemNames[l] || strings.HasPrefix(l, "[gmail]") || strings.HasPrefix(l, "[google mail]")
}

// applyGmailToEveryCopy is applyToEveryCopy for a Gmail account: the same
// every-copy loop and the same all-or-retry rule (#296), with each copy's
// write translated to Gmail's primitive (applyGmailToTarget).
func (e *Engine) applyGmailToEveryCopy(ctx context.Context, in store.Intent, p intentPayload, targets []store.ProvenanceTarget, w IMAPWriter, g GmailWriter, st *pass) error {
	var failures []error
	for _, t := range targets {
		res, err := e.applyGmailToTarget(ctx, in.Op, p, t, w, g, st)
		if err != nil {
			var dead deadError
			if errors.As(err, &dead) {
				// A refusal decided on THIS copy (a move Gmail cannot
				// represent from where it is): retrying cannot change it.
				return e.deadLetter(ctx, in, st, "", dead.reason)
			}
			failures = append(failures, fmt.Errorf("%s uid %d: %w", t.IMAPName, t.UID, err))
			continue
		}
		if res.flagsAdd != nil || res.flagsRemove != nil {
			if err := e.store.RecordStoredFlags(ctx, t.ProvenanceID, res.flagsAdd, res.flagsRemove); err != nil {
				failures = append(failures, fmt.Errorf("%s uid %d: record stored flags: %w", t.IMAPName, t.UID, err))
				continue
			}
		}
		if len(res.labelsAdd) > 0 || len(res.labelsRemove) > 0 {
			if err := e.store.RecordGmailLabels(ctx, t.ProvenanceID, res.labelsAdd, res.labelsRemove); err != nil {
				failures = append(failures, fmt.Errorf("%s uid %d: record gmail labels: %w", t.IMAPName, t.UID, err))
				continue
			}
		}
		if res.destroyed {
			st.res.Expunged++
		}
	}
	if len(failures) > 0 {
		return e.failTransient(ctx, in, st, fmt.Errorf(
			"applied to %d of %d live copies in this account, %d failed: %w",
			len(targets)-len(failures), len(targets), len(failures), errors.Join(failures...)))
	}
	return e.markApplied(ctx, in, st)
}

// deadError is a per-copy refusal that dead-letters the whole intent.
type deadError struct{ reason string }

func (d deadError) Error() string { return d.reason }

// gmailResult is what one copy's Gmail write confirmed, for the bookkeeping
// the caller records after it.
type gmailResult struct {
	flagsAdd, flagsRemove   []string // a FLAGS STORE (RecordStoredFlags, #576)
	labelsAdd, labelsRemove []string // a confirmed label write (the gm_labels baseline)
	destroyed               bool     // the server said it expunged the message
}

// applyGmailToTarget issues one Gmail-mode write against one copy.
func (e *Engine) applyGmailToTarget(ctx context.Context, op store.IntentOp, p intentPayload, t store.ProvenanceTarget, w IMAPWriter, g GmailWriter, st *pass) (gmailResult, error) {
	var res gmailResult
	src := e.gmailKindOf(t.IMAPName)
	if src != gmailAll && src != gmailTrash && src != gmailSpam {
		// A Gmail account syncs only All Mail, Spam and Trash; a copy
		// anywhere else is not something this model put there. Fail closed.
		return res, deadError{fmt.Sprintf("gmail: the copy is in %q, not All Mail, Spam or Trash; refusing to write to it", t.IMAPName)}
	}
	uid, err := e.gmailResolve(ctx, t, g, st)
	if err != nil {
		return res, err
	}
	switch op {
	case store.IntentAddFlag, store.IntentRemoveFlag:
		add, remove := flagDelta(op == store.IntentAddFlag, p.Flag)
		if err := w.StoreFlags(ctx, t.IMAPName, uid, toFlags(add), toFlags(remove)); err != nil {
			return res, err
		}
		res.flagsAdd, res.flagsRemove = add, remove
		return res, nil

	case store.IntentAddKeyword, store.IntentRemoveKeyword:
		if p.Label == "" {
			// A plain keyword ($Junk, a client's own token) stays a keyword.
			add, remove := flagDelta(op == store.IntentAddKeyword, p.Keyword)
			if err := w.StoreFlags(ctx, t.IMAPName, uid, toFlags(add), toFlags(remove)); err != nil {
				return res, err
			}
			res.flagsAdd, res.flagsRemove = add, remove
			return res, nil
		}
		add, remove := flagDelta(op == store.IntentAddKeyword, p.Label)
		echoedAdd, err := e.gmailStoreLabels(ctx, g, t.IMAPName, uid, add, remove, st)
		if err != nil {
			return res, err
		}
		// The baseline takes Gmail's spelling of an added label ("Work" for a
		// Mailroom "work"), so the next reading diffs to nothing (F7).
		res.labelsAdd, res.labelsRemove = echoedAdd, remove
		return res, nil

	case store.IntentMove:
		return e.gmailMove(ctx, p, t, src, uid, w, g, st)

	case store.IntentExpunge:
		if src != gmailTrash && src != gmailSpam {
			// A message-addressed permanent delete only ever comes from the
			// Trash purge, for a copy IN Trash. One aimed at All Mail is not
			// something Mailroom asks for, and moving a real message to Trash
			// to destroy it is not a guess worth making (#911 review F4).
			return res, deadError{fmt.Sprintf("gmail: refusing a permanent delete of the copy in %q; Mailroom only erases a Gmail message that is already in Trash or Spam", t.IMAPName)}
		}
		destroyed, err := e.gmailDeleteForever(ctx, t.IMAPName, src, uid, t.GmMsgID, w, g, st)
		res.destroyed = destroyed
		return res, err
	}
	return res, fmt.Errorf("writeback: unhandled op %q", op)
}

// gmailMove translates a move intent for one copy (see the file comment's
// table).
func (e *Engine) gmailMove(ctx context.Context, p intentPayload, t store.ProvenanceTarget, src gmailKind, uid imapsync.UID, w IMAPWriter, g GmailWriter, st *pass) (gmailResult, error) {
	var res gmailResult
	dest := e.gmailKindOf(p.Mailbox)
	if dest == gmailOther {
		return res, deadError{fmt.Sprintf("gmail: a message cannot be moved into %q on Gmail; only Inbox, All Mail, Spam, Trash and label views take a move", p.Mailbox)}
	}
	if src == gmailAll {
		switch dest {
		case gmailInboxBox:
			if _, err := e.gmailStoreLabels(ctx, g, t.IMAPName, uid, []string{gmailInbox}, nil, st); err != nil {
				return res, err
			}
			res.labelsAdd = []string{gmailInbox}
			return res, nil
		case gmailAll:
			// ARCHIVE: the \Inbox label comes off. No MOVE, so the copy stays
			// where Mailroom's provenance says it is.
			if _, err := e.gmailStoreLabels(ctx, g, t.IMAPName, uid, nil, []string{gmailInbox}, st); err != nil {
				return res, err
			}
			res.labelsRemove = []string{gmailInbox}
			return res, nil
		case gmailLabel:
			// Gmail's own "Move to": the label goes on and the inbox comes
			// off. Only the \Inbox removal is recorded on the baseline: the
			// label is left for the next reading to report, which adds the
			// matching Mailroom label to the message, as every other message
			// carrying that Gmail label has.
			if _, err := e.gmailStoreLabels(ctx, g, t.IMAPName, uid, []string{p.Mailbox}, []string{gmailInbox}, st); err != nil {
				return res, err
			}
			res.labelsRemove = []string{gmailInbox}
			return res, nil
		}
	}
	if dest == src {
		return res, nil // already there (a move into Trash of a copy in Trash)
	}
	// Into or out of Trash/Spam: a real MOVE, the documented Gmail way.
	if !w.SupportsMove() {
		// go-imap would fall back to COPY + STORE \Deleted + EXPUNGE in the
		// source, an EXPUNGE outside Trash and Spam. Refuse instead.
		return res, errors.New("gmail: the server did not advertise MOVE; refusing the COPY+EXPUNGE fallback")
	}
	if src == gmailAll {
		// Into Trash or Spam from All Mail: the message stays recoverable
		// there whatever happens next.
		if err := w.Move(ctx, t.IMAPName, uid, p.Mailbox); err != nil {
			return res, err
		}
		st.forgetGmail(t.IMAPName, t.GmMsgID)
		return res, nil
	}
	return res, e.gmailMoveOut(ctx, p, t, dest, uid, w, g, st)
}

// gmailMoveOut moves a copy OUT of Trash or Spam (#911 review F2). A MOVE is
// a COPY then an EXPUNGE of the source, so a COPY Gmail treats as a no-op
// would lose the message; every such MOVE is therefore checked — the message
// must be found by X-GM-MSGID in the destination, exactly once — and a copy
// with no X-GM-MSGID to check by is refused before anything is sent.
//
// Restoring to the ARCHIVE is never a MOVE into All Mail (whose COPY
// semantics are UNVERIFIED): it is Gmail's standard untrash, a MOVE to INBOX,
// checked, then -X-GM-LABELS (\Inbox) on the copy in All Mail.
func (e *Engine) gmailMoveOut(ctx context.Context, p intentPayload, t store.ProvenanceTarget, dest gmailKind, uid imapsync.UID, w IMAPWriter, g GmailWriter, st *pass) error {
	if t.GmMsgID == 0 {
		return deadError{fmt.Sprintf("gmail: the copy in %q has no X-GM-MSGID, so a move out of it could not be checked; refusing it", t.IMAPName)}
	}
	target := p.Mailbox
	if dest == gmailAll {
		target = e.folders.Inbox
		if target == "" {
			target = "INBOX"
		}
	}
	if err := w.Move(ctx, t.IMAPName, uid, target); err != nil {
		return err
	}
	st.forgetGmail(t.IMAPName, t.GmMsgID)
	uids, err := g.SearchGmailMsgID(ctx, target, t.GmMsgID)
	if err != nil {
		return deadError{fmt.Sprintf("gmail: moved X-GM-MSGID %d out of %q into %q but could not check it arrived (%v); stopping, check the account in Gmail", t.GmMsgID, t.IMAPName, target, err)}
	}
	if len(uids) != 1 {
		return deadError{fmt.Sprintf("gmail: moved X-GM-MSGID %d out of %q into %q, and %q now holds %d copies of it, not 1; stopping, check the account in Gmail", t.GmMsgID, t.IMAPName, target, target, len(uids))}
	}
	if dest != gmailAll {
		return nil
	}
	all := e.folders.All
	if all == "" {
		return deadError{"gmail: restored to the inbox, but the account lists no All Mail to archive it in"}
	}
	inAll, err := g.SearchGmailMsgID(ctx, all, t.GmMsgID)
	if err != nil {
		return fmt.Errorf("gmail: restored to the inbox; find it in All Mail to archive it: %w", err)
	}
	if len(inAll) != 1 {
		return fmt.Errorf("gmail: restored to the inbox, then X-GM-MSGID %d matched %d messages in All Mail; not archiving", t.GmMsgID, len(inAll))
	}
	_, err = e.gmailStoreLabels(ctx, g, all, inAll[0], nil, []string{gmailInbox}, st)
	return err
}

// gmailDeleteForever permanently deletes one message the Gmail way: EXPUNGE
// only ever runs in Trash or Spam. A copy elsewhere is first moved to Trash
// and found there again by its X-GM-MSGID; with no X-GM-MSGID to find it by,
// it is refused rather than guessed at.
func (e *Engine) gmailDeleteForever(ctx context.Context, mailbox string, src gmailKind, uid imapsync.UID, msgID uint64, w IMAPWriter, g GmailWriter, st *pass) (bool, error) {
	if src == gmailTrash || src == gmailSpam {
		return expungeOne(ctx, w, mailbox, uid)
	}
	trash := e.folders.Trash
	if trash == "" {
		return false, deadError{"gmail: the account lists no Trash, so a message cannot be deleted permanently (Mailroom never expunges outside Trash and Spam)"}
	}
	if e.gmailKindOf(trash) != gmailTrash {
		// The Trash name also names INBOX or All Mail (#911 review F9): the
		// "Trash" this would expunge in is not unambiguously Trash.
		return false, deadError{fmt.Sprintf("gmail: %q is not unambiguously the Trash; refusing a permanent delete", trash)}
	}
	if msgID == 0 {
		return false, deadError{fmt.Sprintf("gmail: the copy in %q has no X-GM-MSGID, so it could not be found in Trash after moving it there; refusing to delete it permanently", mailbox)}
	}
	if !w.SupportsMove() {
		return false, errors.New("gmail: the server did not advertise MOVE; refusing the COPY+EXPUNGE fallback")
	}
	if err := w.Move(ctx, mailbox, uid, trash); err != nil {
		return false, err
	}
	st.forgetGmail(mailbox, msgID)
	uids, err := g.SearchGmailMsgID(ctx, trash, msgID)
	if err != nil {
		return false, fmt.Errorf("gmail: find the message in Trash after moving it there: %w", err)
	}
	if len(uids) != 1 {
		// Moved to Trash but not found (or found twice): it is safe in Trash,
		// and Gmail purges Trash on its own; the retry will look again.
		return false, fmt.Errorf("gmail: moved to Trash, then X-GM-MSGID %d matched %d messages there; not expunging", msgID, len(uids))
	}
	if e.gmailKindOf(trash) != gmailTrash {
		return false, deadError{fmt.Sprintf("gmail: %q is not unambiguously the Trash; refusing to expunge there", trash)}
	}
	return expungeOne(ctx, w, trash, uids[0])
}

// gmailResolve returns the copy's CURRENT UID in its mailbox, found by
// X-GM-MSGID (once per message and mailbox per pass), or the stored UID when
// the copy has no X-GM-MSGID.
func (e *Engine) gmailResolve(ctx context.Context, t store.ProvenanceTarget, g GmailWriter, st *pass) (imapsync.UID, error) {
	if t.GmMsgID == 0 {
		return imapsync.UID(t.UID), nil
	}
	key := gmailKey{mailbox: t.IMAPName, msgID: t.GmMsgID}
	if uid, ok := st.gmailUIDs[key]; ok {
		return uid, nil
	}
	uids, err := g.SearchGmailMsgID(ctx, t.IMAPName, t.GmMsgID)
	if err != nil {
		return 0, fmt.Errorf("gmail: resolve X-GM-MSGID %d in %q: %w", t.GmMsgID, t.IMAPName, err)
	}
	switch len(uids) {
	case 0:
		// Never fall back to the stored UID here: the message has left the
		// mailbox (another client moved or deleted it), and the next sync
		// records where it went. Retry then.
		return 0, fmt.Errorf("gmail: X-GM-MSGID %d is no longer in %q (moved or deleted in Gmail?); the next sync re-resolves it", t.GmMsgID, t.IMAPName)
	case 1:
	default:
		return 0, fmt.Errorf("gmail: X-GM-MSGID %d matched %d messages in %q; refusing to guess", t.GmMsgID, len(uids), t.IMAPName)
	}
	if uids[0] != imapsync.UID(t.UID) {
		st.res.Reresolved++
	}
	if st.gmailUIDs == nil {
		st.gmailUIDs = map[gmailKey]imapsync.UID{}
	}
	st.gmailUIDs[key] = uids[0]
	return uids[0], nil
}

// forgetGmail drops a cached resolution after the message left the mailbox.
func (p *pass) forgetGmail(mailbox string, msgID uint64) {
	delete(p.gmailUIDs, gmailKey{mailbox: mailbox, msgID: msgID})
}

// gmailStoreLabels writes a label delta to one message and CONFIRMS it (see
// the file comment): the echoed (or re-FETCHed) labels must contain every
// added label and none of the removed ones. An add that errors or does not
// stick CREATEs its user labels and is retried once, because whether STORE
// creates a missing label is UNVERIFIED.
func (e *Engine) gmailStoreLabels(ctx context.Context, g GmailWriter, mailbox string, uid imapsync.UID, add, remove []string, st *pass) ([]string, error) {
	var lastErr error
	for attempt := 0; attempt < 2; attempt++ {
		labels, echoed, err := g.StoreGmailLabels(ctx, mailbox, uid, add, remove)
		if err == nil && !echoed {
			var found bool
			_, labels, found, err = g.FetchGmail(ctx, mailbox, uid)
			switch {
			case err != nil:
			case !found:
				err = fmt.Errorf("gmail: uid %d is no longer in %q", uid, mailbox)
			case labels == nil:
				err = fmt.Errorf("gmail: no X-GM-LABELS reading for uid %d in %q to confirm the change", uid, mailbox)
			}
		}
		var missing []string
		if err == nil {
			var present []string
			missing, present = labelCheck(labels, add, remove)
			if len(missing) == 0 && len(present) == 0 {
				return echoedSpelling(labels, add), nil
			}
			err = fmt.Errorf("gmail: after the STORE, %q still lacks %q and still has %q", mailbox, missing, present)
		} else {
			missing = add
		}
		lastErr = err
		toCreate := userLabels(missing)
		if attempt > 0 || len(toCreate) == 0 {
			break
		}
		for _, l := range toCreate {
			if st.gmailCreated[l] {
				continue
			}
			// The CREATE's own answer is not decisive: "already exists" is a
			// NO on most servers, and the retried STORE is what is checked.
			if cerr := g.CreateMailbox(ctx, l); cerr == nil {
				st.res.LabelsCreated++
			}
			if st.gmailCreated == nil {
				st.gmailCreated = map[string]bool{}
			}
			st.gmailCreated[l] = true
		}
	}
	return nil, lastErr
}

// echoedSpelling returns each label of add as Gmail spelled it in got.
func echoedSpelling(got, add []string) []string {
	byKey := map[string]string{}
	for _, l := range got {
		byKey[labelKey(l)] = l
	}
	out := make([]string, 0, len(add))
	for _, l := range add {
		if g, ok := byKey[labelKey(l)]; ok {
			out = append(out, g)
		} else {
			out = append(out, l)
		}
	}
	return out
}

// labelCheck returns the labels of add missing from got and the labels of
// remove still in got. Labels compare case-insensitively: Gmail's label names
// are (it will not hold "Work" and "work" at once), so a Mailroom "work"
// written to a Gmail "Work" is confirmed, not a spurious failure (F7).
func labelCheck(got, add, remove []string) (missing, present []string) {
	has := map[string]bool{}
	for _, l := range got {
		has[labelKey(l)] = true
	}
	for _, l := range add {
		if !has[labelKey(l)] {
			missing = append(missing, l)
		}
	}
	for _, l := range remove {
		if has[labelKey(l)] {
			present = append(present, l)
		}
	}
	return missing, present
}

func labelKey(l string) string {
	return strings.ToLower(l)
}

// userLabels keeps the user labels (not `\System` ones).
func userLabels(ls []string) []string {
	var out []string
	for _, l := range ls {
		if !strings.HasPrefix(l, `\`) {
			out = append(out, l)
		}
	}
	return out
}

func flagDelta(add bool, v string) (a, r []string) {
	if add {
		return []string{v}, nil
	}
	return nil, []string{v}
}

// applyGmailHeaderExpunge is applyHeaderExpunge for a Gmail account: the
// header-addressed EXPUNGE that retires a superseded or sent draft
// (store.RetireDraft / RetireSentDraftCopy).
//
// ⚠ THE GENERIC PATH WOULD DESTROY SENT MAIL ON GMAIL. A Gmail draft's
// provenance is All Mail, so the store addresses the expunge to All Mail by
// Message-ID — and the message Gmail filed when the draft was sent has the
// SAME Message-ID, in All Mail too. A Message-ID SEARCH there finds both. So
// on Gmail the draft is looked for in the Drafts view only (which holds
// drafts and nothing else), identified by its X-GM-MSGID, moved to Trash and
// expunged there: deleting forever without an EXPUNGE outside Trash.
//
// The store enqueues one such intent per mailbox it knows a copy in, and on
// Gmail the All Mail copy and the Drafts copy are the SAME message, so the
// second of the pair finds nothing. Within one pass that is recognised
// (gmailRetired) and applied as done; across passes it is the #268 rule — an
// empty search is a failure — that holds.
func (e *Engine) applyGmailHeaderExpunge(ctx context.Context, in store.Intent, p intentPayload, w IMAPWriter, g GmailWriter, st *pass) error {
	if p.Mailbox == "" {
		return e.deadLetter(ctx, in, st, "", "expunge: payload names a Message-ID but no mailbox")
	}
	switch e.gmailKindOf(p.Mailbox) {
	case gmailTrash, gmailSpam:
		// EXPUNGE is allowed here: the generic path, unchanged.
		return e.applyHeaderExpunge(ctx, in, p, w, st)
	case gmailAll, gmailOther:
		// All Mail, or the Drafts view itself: look in Drafts.
	default:
		return e.deadLetter(ctx, in, st, "", fmt.Sprintf("gmail: a header-addressed expunge in %q is not something Gmail writeback does", p.Mailbox))
	}
	drafts := e.folders.Drafts
	if drafts == "" {
		return e.deadLetter(ctx, in, st, "", "gmail: the account lists no Drafts mailbox, so a draft cannot be retired")
	}
	if p.Mailbox != e.folders.All && p.Mailbox != drafts {
		return e.deadLetter(ctx, in, st, "", fmt.Sprintf("gmail: a header-addressed expunge in %q is not something Gmail writeback does", p.Mailbox))
	}
	uids, err := w.SearchMessageID(ctx, drafts, p.MessageIDHeader)
	if err != nil {
		return e.failTransient(ctx, in, st, fmt.Errorf("expunge: search %q: %w", drafts, err))
	}
	if len(uids) == 0 {
		if st.gmailRetired[p.MessageIDHeader] {
			return e.markApplied(ctx, in, st) // its pair retired it, this pass
		}
		uid, ok := st.appendedThisPass(drafts, p.MessageIDHeader)
		if !ok {
			return e.failTransient(ctx, in, st, fmt.Errorf(
				"expunge: no draft %s found in %q — it may already have been removed by another client, or the server did not return it",
				p.MessageIDHeader, drafts))
		}
		uids = []imapsync.UID{uid}
	}
	for _, uid := range uids {
		msgID, labels, found, err := g.FetchGmail(ctx, drafts, uid)
		if err != nil {
			return e.failTransient(ctx, in, st, fmt.Errorf("expunge: identify draft uid %d: %w", uid, err))
		}
		if !found {
			continue // gone between the SEARCH and now
		}
		// STILL A DRAFT, AND NOT SENT (#911 review F1). The Drafts view is
		// only what Gmail calls a draft at the moment it answers; a message
		// that has since been sent, or that carries no label reading at all,
		// is not destroyed on the strength of a Message-ID match.
		if reason, ok := gmailIsUnsentDraft(labels); !ok {
			return e.deadLetter(ctx, in, st, "", fmt.Sprintf("gmail: draft retirement refused for uid %d in %q: %s", uid, drafts, reason))
		}
		removed, err := e.gmailDeleteForever(ctx, drafts, gmailOther, uid, msgID, w, g, st)
		if err != nil {
			var dead deadError
			if errors.As(err, &dead) {
				return e.deadLetter(ctx, in, st, "", dead.reason)
			}
			return e.failTransient(ctx, in, st, err)
		}
		if removed {
			st.res.Expunged++
		}
	}
	if st.gmailRetired == nil {
		st.gmailRetired = map[string]bool{}
	}
	st.gmailRetired[p.MessageIDHeader] = true
	return e.markApplied(ctx, in, st)
}

// gmailIsUnsentDraft reports whether a label reading says a message is a
// draft and not a sent message: a reading must exist, carry \Draft, and not
// carry \Sent (case-insensitive).
func gmailIsUnsentDraft(labels []string) (string, bool) {
	if labels == nil {
		return "no X-GM-LABELS reading to confirm it is still a draft", false
	}
	draft, sent := false, false
	for _, l := range labels {
		switch strings.ToLower(l) {
		case `\draft`:
			draft = true
		case `\sent`:
			sent = true
		}
	}
	switch {
	case sent:
		return "the message carries \\Sent", false
	case !draft:
		return "the message no longer carries \\Draft", false
	}
	return "", true
}
