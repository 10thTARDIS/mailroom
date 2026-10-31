// SPDX-License-Identifier: AGPL-3.0-only

package store

import (
	"context"
	"fmt"
	"time"

	"github.com/oklog/ulid/v2"
)

// The reads a CONFIGURATION export needs that nothing else in the store
// provides (#54, spec §10).
//
// Most of the export plan's sidecars are served by accessors that already
// existed — ListLabels, ListIdentities, ListRules, ListComposeSnippets,
// ListSavedSearches, ListCategoryTabs, ListRemoteAllows, ListContacts. These
// three are the gaps.
//
// ⚠ app.contact_keys HAD NO READER AT ALL. Nothing in internal/store selected
// from it before this, which is worth noticing rather than quietly fixing: the
// export plan promises "contacts.json (public keys only)", and a promise about a
// table nothing can read is not a promise anyone could have kept. It holds zero
// rows on a typical instance, so the gap had never surfaced.

// ExportAccount is the user's own account record.
//
// ⚠ THE OIDC SUBJECT IS NOT SELECTED, and its absence is the point. It is an
// identifier minted by THIS deployment's identity provider; it names the user to
// that provider and is a stable correlator anywhere else. The export plan says
// so, and the query is the enforcement — a field never read cannot be
// accidentally serialised later.
type ExportAccount struct {
	Email       string
	DisplayName string
	CreatedAt   time.Time
}

// ExportAccountRecord reads the calling user's own row.
func (tx *UserTx) ExportAccountRecord(ctx context.Context) (ExportAccount, error) {
	var a ExportAccount
	err := tx.tx.QueryRow(ctx, `
		SELECT email, display_name, created_at FROM app.users`).
		Scan(&a.Email, &a.DisplayName, &a.CreatedAt)
	if err != nil {
		return a, fmt.Errorf("store: export account record: %w", err)
	}
	return a, nil
}

// ExportContactKey is one correspondent's PUBLIC key.
type ExportContactKey struct {
	ContactID   ulid.ULID
	Kind        string
	Fingerprint string
	Source      string
	FetchedAt   time.Time
}

// ExportContactKeys returns every held correspondent key, grouped by contact.
// A key the user removed (#815: removed_at set, kept for its history) is not
// exported: the export carries the keys the user holds, not this instance's
// record of the ones they discarded.
//
// ⚠ public_key IS NOT SELECTED. The bytes are recoverable from WKD or the
// correspondent themselves, they bloat a JSON sidecar with base64, and the
// FINGERPRINT is what a human verifies and what another client needs to look the
// key up. Carrying the fingerprint and the source is the useful half; carrying
// the packets is bulk.
func (tx *UserTx) ExportContactKeys(ctx context.Context) (map[ulid.ULID][]ExportContactKey, error) {
	rows, err := tx.tx.Query(ctx, `
		SELECT contact_id, kind, fingerprint, source, fetched_at
		  FROM app.contact_keys
		 WHERE removed_at IS NULL
		 ORDER BY contact_id, fingerprint`)
	if err != nil {
		return nil, fmt.Errorf("store: export contact keys: %w", err)
	}
	defer rows.Close()

	out := map[ulid.ULID][]ExportContactKey{}
	for rows.Next() {
		var cid string
		var k ExportContactKey
		if err := rows.Scan(&cid, &k.Kind, &k.Fingerprint, &k.Source, &k.FetchedAt); err != nil {
			return nil, fmt.Errorf("store: scan contact key: %w", err)
		}
		id, err := ulid.Parse(cid)
		if err != nil {
			return nil, fmt.Errorf("store: contact key contact id %q: %w", cid, err)
		}
		k.ContactID = id
		out[id] = append(out[id], k)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: export contact keys: %w", err)
	}
	return out, nil
}

// ExportSubmission is one send attempt, as an export records it.
//
// ⚠ IT CARRIES THE MESSAGE'S CONTENT HASH, NOT ITS ULID. That makes the send
// record point at a FILE IN THE EXPORTED MAILDIR — the archive becomes
// self-referential in a way another system can follow, instead of holding a
// pointer into a database it does not have. account_id, identity_id, thread_id,
// job_id and the idempotency key are not carried at all: instance bookkeeping,
// and in the key's case a client's private retry token.
type ExportSubmission struct {
	MessageHash  string
	FromAddress  string
	EnvelopeTo   []string
	State        string
	SMTPResponse string
	SendAt       time.Time
	SentAt       *time.Time
}

// ExportSubmissions returns every send attempt, joined to the message's hash.
func (tx *UserTx) ExportSubmissions(ctx context.Context) ([]ExportSubmission, error) {
	rows, err := tx.tx.Query(ctx, `
		SELECT encode(m.content_hash, 'hex'), s.from_address, s.envelope_to,
		       s.state, COALESCE(s.smtp_response, ''), s.send_at, s.sent_at
		  FROM app.submissions s
		  JOIN app.messages m ON m.id = s.message_id
		 ORDER BY s.created_at`)
	if err != nil {
		return nil, fmt.Errorf("store: export submissions: %w", err)
	}
	defer rows.Close()

	var out []ExportSubmission
	for rows.Next() {
		var s ExportSubmission
		if err := rows.Scan(&s.MessageHash, &s.FromAddress, &s.EnvelopeTo,
			&s.State, &s.SMTPResponse, &s.SendAt, &s.SentAt); err != nil {
			return nil, fmt.Errorf("store: scan export submission: %w", err)
		}
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: export submissions: %w", err)
	}
	return out, nil
}
