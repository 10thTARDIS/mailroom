// SPDX-License-Identifier: AGPL-3.0-only

package store

import (
	"context"
	"fmt"

	"github.com/oklog/ulid/v2"
)

// This file is the store surface for the #156 function signals (migration
// 00027): the rest of internal/kind's header-only Facts that migration 00016
// left uncaptured because they live only in the message BYTES —
// List-Unsubscribe-Post, Auto-Submitted, Precedence, Feedback-ID, the DKIM d=
// domains, and Return-Path.
//
// Like the #157 bulk signals in ingest.go/triage.go, these are an ENVELOPE
// PROJECTION, not a JMAP-visible Email property: no client ever reads a DKIM
// domain or a Precedence header off Email/get, so a write here appends NO
// change_log row of its own. At ingest the values ride along inside the same
// INSERT the Email `created` change already covers (ingestNew appends that
// change AFTER InsertMessage, in the same transaction — see
// internal/ingest/ingest.go). The backfill below is a pure re-derivation from
// already-stored bytes, exactly like UpdateMessageSignals (triage.go), and for
// the identical reason appends nothing.

// FunctionSignals is the write-side bundle for the #156 signals, shared by
// ingest.FunctionSignalsFromHeader (the ONE extraction both ingest and the
// backfill command call, so they cannot derive these two different ways) and
// UpdateMessageFunctionSignals below.
//
// A STRUCT HERE, UNLIKE UpdateMessageSignals's four scalar parameters: five of
// these six fields are strings, and a positional call with five string
// arguments is exactly the shape a caller transposes by accident without a
// compiler complaint. A named struct makes that mistake require typing the
// wrong field name, which is a much smaller failure mode. Same shape as
// MessageRow, ContactSourceMessage and the other multi-field store write
// bundles in this package.
type FunctionSignals struct {
	// ListUnsubscribePost is List-Unsubscribe-Post (RFC 8058 one-click).
	ListUnsubscribePost string
	// AutoSubmitted is Auto-Submitted (RFC 3834).
	AutoSubmitted string
	// Precedence is the pre-standard bulk/list marker.
	Precedence string
	// FeedbackID is Gmail's feedback-loop header (or the X-Feedback-ID some
	// ESPs emit instead) — presence alone means a high-volume ESP sender.
	FeedbackID string
	// DKIMDomains are the d= values from every DKIM-Signature header on the
	// message — the sending infrastructure, kind.go's own "highest-value
	// signal available".
	DKIMDomains []string
	// ReturnPath is the envelope sender, the VERP/bounce-tracking tell.
	ReturnPath string
}

// UpdateMessageFunctionSignals backfills the function signals migration 00027
// added, from headers re-parsed out of the stored bytes (#156, mirroring
// UpdateMessageSignals for #157's four in triage.go). It reports whether
// anything actually changed, so a repeat run is a cheap no-op.
//
// NULLIF/COALESCE, not a Go-side nil interface: every parameter is a concrete,
// never-nil value (dkimDomains defaults to an empty, non-nil slice below), and
// the NULL-collapsing happens entirely in SQL — the same shape as
// UpdateMessageSignals and labels.go's UpdateLabel, chosen over binding a bare
// nil so pgx never has to infer an array element type from nothing.
//
// It appends NO change_log row — see the file header.
func (tx *UserTx) UpdateMessageFunctionSignals(ctx context.Context, id ulid.ULID, s FunctionSignals) (bool, error) {
	dkim := s.DKIMDomains
	if dkim == nil {
		dkim = []string{}
	}
	var touched string
	err := tx.tx.QueryRow(ctx, `
		UPDATE app.messages
		   SET list_unsubscribe_post = NULLIF($3, ''),
		       auto_submitted        = NULLIF($4, ''),
		       precedence            = NULLIF($5, ''),
		       feedback_id           = NULLIF($6, ''),
		       dkim_domains          = NULLIF($7::text[], '{}'::text[]),
		       return_path           = NULLIF($8, '')
		 WHERE user_id = $1 AND id = $2
		   AND (COALESCE(list_unsubscribe_post, ''), COALESCE(auto_submitted, ''),
		        COALESCE(precedence, ''), COALESCE(feedback_id, ''),
		        COALESCE(dkim_domains, '{}'::text[]), COALESCE(return_path, ''))
		       IS DISTINCT FROM ($3, $4, $5, $6, $7::text[], $8)
		RETURNING id`,
		tx.userID.String(), id.String(),
		s.ListUnsubscribePost, s.AutoSubmitted, s.Precedence, s.FeedbackID,
		dkim, s.ReturnPath).Scan(&touched)
	if isNoRows(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("store: update message function signals: %w", err)
	}
	return true, nil
}
