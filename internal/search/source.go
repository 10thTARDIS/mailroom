// SPDX-License-Identifier: AGPL-3.0-only

package search

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/blob"
	"mailroom/internal/crypto"
	"mailroom/internal/store"
)

// The adapters that bind the engine-neutral indexer to Mailroom's real store and
// blob store. Everything above this file speaks in search's own types, so the
// indexer, the AST, and the backends are all testable with fakes and no database.

// StoreSource is the production Source: the canonical Postgres store, read
// through UserTx so RLS scopes every statement to the user being indexed. The
// indexer therefore inherits the database's fail-closed posture for free — a
// zero user id never reaches a query, and a user id can only ever read its own
// rows.
type StoreSource struct{ st *store.Store }

// NewStoreSource wraps a *store.Store as the indexer's Source.
func NewStoreSource(st *store.Store) *StoreSource { return &StoreSource{st: st} }

// EmailState returns the head of the user's Email change stream.
func (s *StoreSource) EmailState(ctx context.Context, userID ulid.ULID) (int64, error) {
	var state int64
	err := s.st.UserTx(ctx, userID, func(tx *store.UserTx) error {
		var e error
		state, e = tx.ChangeState(ctx, store.ObjectEmail)
		return e
	})
	return state, err
}

// EmailChanges reads one collapsed page of the Email change log after `since`.
// It reuses the SAME store method JMAP Foo/changes uses (UserTx.Changes), which
// is the point of spec §7's "one ordered stream, one cursor per consumer": the
// indexer is just another consumer of the stream clients sync against, with the
// same collapse rules and the same compaction floor.
//
// A `since` below the floor surfaces as ErrCursorTooOld, the indexer's signal to
// reindex — the same situation in which a client is told cannotCalculateChanges
// and refetches.
func (s *StoreSource) EmailChanges(ctx context.Context, userID ulid.ULID, since int64, max int) (Changes, error) {
	var out Changes
	err := s.st.UserTx(ctx, userID, func(tx *store.UserTx) error {
		page, err := tx.Changes(ctx, store.ObjectEmail, since, max)
		if err != nil {
			return err
		}
		out.NewState = page.NewState
		out.HasMore = page.HasMore
		out.Upsert = append(append([]ulid.ULID{}, page.Created...), page.Updated...)
		out.Delete = page.Destroyed
		return nil
	})
	if errors.Is(err, store.ErrCannotCalculateChanges) {
		return Changes{}, ErrCursorTooOld
	}
	if err != nil {
		return Changes{}, err
	}
	return out, nil
}

// Messages loads the canonical index projections for ids.
func (s *StoreSource) Messages(ctx context.Context, userID ulid.ULID, ids []ulid.ULID) ([]Message, error) {
	var out []Message
	err := s.st.UserTx(ctx, userID, func(tx *store.UserTx) error {
		rows, err := tx.IndexMessages(ctx, ids)
		if err != nil {
			return err
		}
		out = make([]Message, 0, len(rows))
		for _, r := range rows {
			out = append(out, messageFrom(r))
		}
		return nil
	})
	return out, err
}

// MessageIDsAfter walks the user's messages in ascending id order (the reindex
// scan). ULIDs sort by creation time, so this is a stable keyset walk that costs
// nothing extra.
func (s *StoreSource) MessageIDsAfter(ctx context.Context, userID ulid.ULID, after ulid.ULID, limit int) ([]ulid.ULID, error) {
	var out []ulid.ULID
	err := s.st.UserTx(ctx, userID, func(tx *store.UserTx) error {
		var e error
		out, e = tx.MessageIDsAfter(ctx, after, limit)
		return e
	})
	return out, err
}

// messageFrom converts the store's projection into the indexer's.
func messageFrom(r store.IndexMessage) Message {
	m := Message{
		ID:            r.ID,
		ThreadID:      r.ThreadID,
		ContentHash:   r.ContentHash,
		Subject:       r.Subject,
		FromName:      r.FromName,
		FromAddress:   r.FromAddress,
		Snippet:       r.Snippet,
		ReceivedAt:    r.ReceivedAt,
		Size:          r.SizeBytes,
		HasAttachment: r.HasAttachments,
		Trashed:       r.Trashed,
		Seen:          r.Seen,
		Flagged:       r.Flagged,
		Answered:      r.Answered,
		Draft:         r.Draft,
		Junk:          r.Junk,
		Keywords:      r.Keywords,
		LabelIDs:      r.LabelIDs,
		MailboxIDs:    r.MailboxIDs,
		AccountIDs:    r.AccountIDs,

		ListID:           r.ListID,
		ListUnsubscribe:  r.ListUnsubscribe,
		AddressedAddress: r.AddressedAddress,
	}
	for _, a := range r.Attachments {
		m.Attachments = append(m.Attachments, Attachment{Filename: a.Filename, ContentType: a.ContentType})
	}
	if r.SentAt != nil {
		m.SentAt = *r.SentAt
	}
	return m
}

var _ Source = (*StoreSource)(nil)

// --- Content -----------------------------------------------------------------

// DEKFunc resolves a user's unwrapped mail-blob data-encryption key. The caller
// owns key policy (which HKDF label, which scheme version); this package only
// ever borrows the bytes and scrubs them.
type DEKFunc func(ctx context.Context, userID ulid.ULID) ([]byte, error)

// BlobContent reads a message's raw RFC 5322 bytes out of the encrypted blob
// store. It is the ONLY place the indexer touches ciphertext, and the DEK it
// borrows is zeroed the moment the read is done.
type BlobContent struct {
	blobs blob.BlobStore
	dek   DEKFunc
}

// NewBlobContent binds a blob store and a DEK resolver as the indexer's Content.
func NewBlobContent(blobs blob.BlobStore, dek DEKFunc) *BlobContent {
	return &BlobContent{blobs: blobs, dek: dek}
}

// Raw decrypts and returns the message's raw bytes. blob.BlobStore verifies the
// AEAD tag and the content hash as the reader drains, so a truncated or tampered
// blob surfaces as an error here rather than as silently wrong index content.
func (c *BlobContent) Raw(ctx context.Context, userID ulid.ULID, contentHash []byte) ([]byte, error) {
	if len(contentHash) != 32 {
		return nil, fmt.Errorf("search: bad content hash length %d", len(contentHash))
	}
	var h blob.ContentHash
	copy(h[:], contentHash)

	dek, err := c.dek(ctx, userID)
	if err != nil {
		return nil, fmt.Errorf("search: resolve mail dek: %w", err)
	}
	defer crypto.Zero(dek)

	rc, err := c.blobs.Get(ctx, userID, dek, h)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rc.Close() }()

	raw, err := io.ReadAll(rc)
	if err != nil {
		return nil, fmt.Errorf("search: read blob: %w", err)
	}
	return raw, nil
}

var _ Content = (*BlobContent)(nil)

// --- Resolver ----------------------------------------------------------------

// Resolver turns the NAMES a user types (label:Work, account:personal) into the
// ids the index actually stores. It needs the database, which is why it is a seam
// and not part of the parser.
//
// Indexing label/account IDS rather than names is a deliberate choice with two
// consequences worth stating: renaming a label costs NO reindex (the id is
// unchanged — and store.DeleteLabel explicitly does not fan out per-Email change
// rows, so a name-based index would go stale with no event to catch it), and a
// deleted label's id simply stops resolving, so `label:Deleted` returns nothing
// instead of matching leftovers.
type Resolver interface {
	// LabelIDByName resolves a label name (exact, case-insensitive).
	LabelIDByName(ctx context.Context, userID ulid.ULID, name string) (ulid.ULID, bool, error)
	// AccountIDByName resolves an account name (exact, case-insensitive).
	AccountIDByName(ctx context.Context, userID ulid.ULID, name string) (ulid.ULID, bool, error)
}

// StoreResolver is the production Resolver.
type StoreResolver struct{ st *store.Store }

// NewStoreResolver wraps a *store.Store as a Resolver.
func NewStoreResolver(st *store.Store) *StoreResolver { return &StoreResolver{st: st} }

// LabelIDByName resolves a label name for the user.
func (r *StoreResolver) LabelIDByName(ctx context.Context, userID ulid.ULID, name string) (ulid.ULID, bool, error) {
	var (
		id    ulid.ULID
		found bool
	)
	err := r.st.UserTx(ctx, userID, func(tx *store.UserTx) error {
		var e error
		id, found, e = tx.LabelIDByName(ctx, name)
		return e
	})
	return id, found, err
}

// AccountIDByName resolves an account name for the user.
func (r *StoreResolver) AccountIDByName(ctx context.Context, userID ulid.ULID, name string) (ulid.ULID, bool, error) {
	var (
		id    ulid.ULID
		found bool
	)
	err := r.st.UserTx(ctx, userID, func(tx *store.UserTx) error {
		var e error
		id, found, e = tx.AccountIDByName(ctx, name)
		return e
	})
	return id, found, err
}

var _ Resolver = (*StoreResolver)(nil)
