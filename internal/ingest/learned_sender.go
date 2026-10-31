// SPDX-License-Identifier: AGPL-3.0-only

package ingest

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/blob"
	"mailroom/internal/mime"
	"mailroom/internal/ml"
	"mailroom/internal/placement"
	"mailroom/internal/store"
)

// LearnedSender builds THE identity learned placement authenticates and acts
// on (#714, ADR-032 §7b) from a parsed message: `from` (the stored From
// address, messages.from_address), checked against the raw From headers, with
// authentication evaluated by placement.NewSender for that address's own
// domain.
//
// ONE FUNCTION, TWO CALLERS: the ingest path (Ingestor.learnedSender, on the
// message arriving) and the placement backfill (BackfillSenders, on a stored
// message's blob). So a backfilled message passes exactly the checks it would
// have had to pass on arrival.
//
// `trusted` is the raw MAILROOM_AUTHSERV_IDS entries and received the
// message's received_at (INTERNALDATE as stored); ml.TrustedAt resolves date-
// bounded entries against it (#766). TrustConfigured stays "the deployment
// names any entry", so a message past every bound is refused, not treated as
// unconfigured.
//
// Authentication is read from the TOPMOST Authentication-Results header with
// a trusted authserv-id (ml.TopmostTrustedAuthResults; see its doc for why
// one), through the shared resinfo tokenizer, with RELAXED DMARC alignment
// (ml.DMARCRelaxed, #846: header.from may be the From domain or exactly its
// organisational domain). The unconfigured floor is the
// conservative failure flag, ml.AuthResultsFail over every
// Authentication-Results header: the same expression ingest stores as
// messages.auth_fail (projectionFrom), evaluated here from the headers rather
// than read back, so a stored value from an older parser cannot vouch for a
// message today's parser would refuse.
func LearnedSender(from string, parsed *mime.Message, trusted []string, received time.Time) placement.Sender {
	var mailboxes []string
	for _, a := range parsed.Header.From() {
		mailboxes = append(mailboxes, a.Address)
	}
	authValues := parsed.Header.Values("Authentication-Results")
	top := ml.TopmostTrustedAuthResults(authValues, ml.TrustedAt(trusted, received))
	authFail := ml.AuthResultsFail(authValues)
	return placement.NewSender(from,
		placement.FromHeaders{Fields: parsed.Header.FieldCount("From"), Raw: parsed.Header.Values("From"), Parsed: mailboxes},
		func(domain string) placement.SenderAuth {
			// RELAXED alignment (#846, ADR-032 §7b amendment): a trusted pass
			// whose header.from is the domain's organisational domain counts
			// for a subdomain sender. Every other check is unchanged.
			match := top.DMARCMatchFor(domain, ml.DMARCRelaxed)
			return placement.SenderAuth{
				TrustConfigured: len(trusted) > 0,
				DMARCPass:       match != ml.DMARCNoMatch,
				DMARCMatch:      string(match),
				AffirmativePass: top.AffirmativelyPassed(),
				AuthFail:        authFail,
			}
		})
}

// ErrUnreadableMessage marks a candidate whose stored bytes could not be read
// or parsed. The backfill counts it as its own skip reason; it never moves.
var ErrUnreadableMessage = errors.New("ingest: message bytes unreadable")

// BackfillSenders is the placement backfill's sender source (#714 increment
// 2a): for each candidate the planner would move, read its raw message from
// the encrypted blob store (blob.BlobStore.Get, the reader the search indexer
// and the admin blob scans use; it verifies the AEAD tag and the content hash
// as it drains), parse it with the ingest parser, and build its sender with
// LearnedSender, the arrival-time function.
//
// It touches no database. dek is the user's mail-blob key; the caller owns
// and zeroes it.
func BackfillSenders(blobs blob.BlobStore, userID ulid.ULID, dek []byte, trusted []string) store.PlacementSenderFunc {
	return func(ctx context.Context, c store.PlacementCandidate) (placement.Sender, error) {
		raw, err := readCandidateBytes(ctx, blobs, userID, dek, c.ContentHash)
		if err != nil {
			return placement.Sender{}, err
		}
		parsed, err := mime.ParseBytes(raw)
		if err != nil || parsed == nil {
			return placement.Sender{}, fmt.Errorf("%w: parse: %v", ErrUnreadableMessage, err)
		}
		return LearnedSender(c.From, parsed, trusted, c.ReceivedAt), nil // ReceivedAt is the trust time (zero for archive rows)
	}
}

// readCandidateBytes reads a stored message's raw bytes through the blob
// reader (blob.BlobStore.Get verifies the AEAD tag and the content hash as it
// drains). Every failure wraps ErrUnreadableMessage.
func readCandidateBytes(ctx context.Context, blobs blob.BlobStore, userID ulid.ULID, dek []byte, contentHash []byte) ([]byte, error) {
	var h blob.ContentHash
	if len(contentHash) != len(h) {
		return nil, fmt.Errorf("%w: content hash is %d bytes", ErrUnreadableMessage, len(contentHash))
	}
	copy(h[:], contentHash)
	rc, err := blobs.Get(ctx, userID, dek, h)
	if err != nil {
		return nil, fmt.Errorf("%w: open blob: %v", ErrUnreadableMessage, err)
	}
	raw, err := io.ReadAll(rc)
	_ = rc.Close()
	if err != nil {
		return nil, fmt.Errorf("%w: read blob: %v", ErrUnreadableMessage, err)
	}
	return raw, nil
}
