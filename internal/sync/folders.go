// SPDX-License-Identifier: AGPL-3.0-only

package sync

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/emersion/go-imap/v2"
)

// FolderAdmin is the FOLDER-LEVEL write seam: whole-folder moves, STATUS and
// DELETE. It exists for exactly one caller, `mailroom-admin
// consolidate-folders` (#647), an operator command that reshapes the provider's
// folder tree, and it is deliberately NOT part of SyncSession.
//
// ⚠ WHY A SEPARATE INTERFACE. SyncSession is what the sync engine, the IDLE
// watcher and the writeback drain are handed, and ADR-028 rests on the fact that
// none of them can create, delete or rename a remote folder. Widening
// SyncSession with DELETE would hand that ability to every one of them. A
// FolderAdmin is only ever obtained from DialFolderAdmin, and the one production
// caller is an operator command that reports first and acts only on --apply.
//
// Every method takes and returns this package's own types; go-imap stays behind
// the seam like everywhere else in this package.
type FolderAdmin interface {
	// ListMailboxes is SyncSession.ListMailboxes: every mailbox the account can
	// see, with SPECIAL-USE attributes.
	ListMailboxes(ctx context.Context) ([]Mailbox, error)

	// Status issues STATUS (RFC 9051 §6.3.11) for MESSAGES, UIDNEXT and
	// UIDVALIDITY. It never SELECTs. When mailbox is this connection's
	// currently selected mailbox it UNSELECTs first where the server supports
	// UNSELECT, because RFC 9051 advises against STATUS on the selected
	// mailbox.
	//
	// A server that answers STATUS WITHOUT a MESSAGES item is an ERROR, never a
	// zero: the consolidation deletes a folder only when this says 0, so an
	// omitted count read as 0 would delete a folder nobody had counted.
	Status(ctx context.Context, mailbox string) (FolderStatus, error)

	// MoveAll moves EVERY message in source to dest with one `UID MOVE 1:*`
	// (RFC 6851), through SyncSession.Move.
	//
	// ⚠ IT REFUSES, with ErrMoveUnsupported, when the server does not
	// advertise MOVE — it NEVER falls back. SyncSession.Move lets go-imap
	// degrade to COPY + STORE \Deleted + EXPUNGE, which is acceptable for one
	// writeback intent and not for a whole folder: a failure between the COPY
	// and the EXPUNGE leaves every message in BOTH folders, and the plain
	// EXPUNGE fallback (no UIDPLUS) removes every \Deleted message in the
	// folder, not only the ones this command marked.
	//
	// The caller must not call it on an empty source (`1:*` on an empty
	// mailbox is not well defined across servers); consolidate reads STATUS
	// first and skips N=0.
	MoveAll(ctx context.Context, source, dest string) error

	// DeleteMailbox issues DELETE (RFC 9051 §6.3.5). It does NOT check that
	// the folder is empty — the caller must, with Status, immediately before —
	// but it does deselect the folder first if this connection has it
	// selected: some servers (Dovecot among them) end the whole session when
	// the selected mailbox is deleted.
	DeleteMailbox(ctx context.Context, mailbox string) error

	// SupportsMove reports whether the server advertised MOVE (RFC 6851), or
	// IMAP4rev2, which includes it. MoveAll refuses when this is false.
	SupportsMove() bool

	// Logout sends LOGOUT; Close drops the connection. As SyncSession.
	Logout(ctx context.Context) error
	Close() error
}

// FolderStatus is one STATUS answer.
type FolderStatus struct {
	Name        string
	Messages    uint32
	UIDNext     UID
	UIDValidity uint32
}

// ErrMoveUnsupported is MoveAll's refusal on a server without MOVE.
var ErrMoveUnsupported = errors.New("the server does not advertise MOVE (RFC 6851); refusing to fall back to COPY + EXPUNGE for a whole folder")

// DialFolderAdmin connects exactly as Dial does — the same egress-guarded dial
// (#625 §3), the same Config — and returns the folder-level seam over that
// connection. Build cfg the way every other account connection is built
// (syncrunner.CredentialResolver), so MAILROOM_ALLOW_PRIVATE_ACCOUNT_HOSTS and
// the SEARCH limiter apply identically.
func DialFolderAdmin(ctx context.Context, cfg Config) (FolderAdmin, error) {
	s, err := Dial(ctx, cfg)
	if err != nil {
		return nil, err
	}
	sess, ok := s.(*session)
	if !ok {
		_ = s.Close() // best-effort: we're returning an error anyway
		return nil, &Error{Kind: ErrorKindUnknown, Op: "dial", Err: fmt.Errorf("sync: unexpected session type %T", s)}
	}
	return &folderAdmin{s: sess}, nil
}

// folderAdmin wraps the SAME *session type every other connection uses, so
// MoveAll's move is SyncSession.Move rather than a second copy of it. It is a
// wrapper rather than extra methods on *session so that nothing holding a
// SyncSession can type-assert its way to DeleteMailbox.
type folderAdmin struct {
	s *session
}

var _ FolderAdmin = (*folderAdmin)(nil)

func (f *folderAdmin) ListMailboxes(ctx context.Context) ([]Mailbox, error) {
	return f.s.ListMailboxes(ctx)
}

func (f *folderAdmin) SupportsMove() bool { return f.s.Supports(CapabilityMove) }

func (f *folderAdmin) Logout(ctx context.Context) error { return f.s.Logout(ctx) }

func (f *folderAdmin) Close() error { return f.s.Close() }

func (f *folderAdmin) Status(ctx context.Context, mailbox string) (FolderStatus, error) {
	var out FolderStatus
	err := f.s.withContext(ctx, "status", func() error {
		if _, err := f.s.deselect(mailbox, false); err != nil {
			return err
		}
		data, err := f.s.client.Status(mailbox, &imap.StatusOptions{
			NumMessages: true,
			UIDNext:     true,
			UIDValidity: true,
		}).Wait()
		if err != nil {
			return err
		}
		if data.NumMessages == nil {
			return fmt.Errorf("sync: status %q: the server omitted MESSAGES", mailbox)
		}
		out = FolderStatus{
			Name:        mailbox,
			Messages:    *data.NumMessages,
			UIDNext:     UID(data.UIDNext),
			UIDValidity: data.UIDValidity,
		}
		return nil
	})
	return out, err
}

func (f *folderAdmin) MoveAll(ctx context.Context, source, dest string) error {
	// The SAME predicate go-imap's Move uses to choose between MOVE and its
	// COPY fallback (Caps().Has(CapMove), which also answers for IMAP4rev2), so
	// passing this check means the fallback cannot be taken.
	if !f.SupportsMove() {
		return &Error{Kind: ErrorKindProtocol, Op: "move", Err: ErrMoveUnsupported}
	}
	// UIDSetRange(1, 0) is `1:*`.
	return f.s.Move(ctx, source, UIDSetRange(1, 0), dest)
}

func (f *folderAdmin) DeleteMailbox(ctx context.Context, mailbox string) error {
	return f.s.withContext(ctx, "delete", func() error {
		if _, err := f.s.deselect(mailbox, true); err != nil {
			return err
		}
		return f.s.client.Delete(mailbox).Wait()
	})
}

// deselect leaves mailbox if it is this connection's selected mailbox, and
// reports whether it did. It must be called from inside a withContext closure.
//
// UNSELECT (RFC 3691, part of IMAP4rev2) when the server has it. Without it,
// and only when force is set, it EXAMINEs INBOX instead: RFC 9051 §6.3.2 has
// SELECT/EXAMINE close the current mailbox WITHOUT an expunge. CLOSE is never
// used — it silently expunges every \Deleted message in the mailbox.
func (s *session) deselect(mailbox string, force bool) (bool, error) {
	sel := s.client.Mailbox()
	if sel == nil || !sameMailboxName(sel.Name, mailbox) {
		return false, nil
	}
	switch {
	case s.client.Caps().Has(imap.CapUnselect):
		if err := s.client.Unselect().Wait(); err != nil {
			return false, err
		}
	case force:
		if _, err := s.client.Select("INBOX", &imap.SelectOptions{ReadOnly: true}).Wait(); err != nil {
			return false, err
		}
	default:
		return false, nil
	}
	s.writeSelected = ""
	return true, nil
}

// sameMailboxName compares two mailbox names the way IMAP does: exactly, except
// that INBOX is case-insensitive (RFC 9051 §5.1).
func sameMailboxName(a, b string) bool {
	if strings.EqualFold(a, "INBOX") && strings.EqualFold(b, "INBOX") {
		return true
	}
	return a == b
}
