// SPDX-License-Identifier: AGPL-3.0-only

package store

import (
	"context"
	"fmt"
	"time"

	"github.com/oklog/ulid/v2"
)

// The per-message state that maildir CANNOT carry (#54, spec §10).
//
// The filename's info section holds seen/answered/flagged/draft/trashed and
// nothing else — it has a fixed vocabulary. Everything here is the remainder,
// and every field is something a user would notice losing:
//
//   - LABELS, with WHO applied them. `applied_by_rule_id` distinguishes the
//     user's own filing from a rule's, and #54 names it explicitly: "label
//     provenance dies if the sidecar ignores it".
//   - CUSTOM KEYWORDS, which have no maildir letter.
//   - is_junk, likewise.
//   - the TRIAGE SCORE, which spec §10 names as one of the three sidecars.
//
// ⚠ LABELS ARE CARRIED BY NAME, NOT BY ID. A ULID means nothing in another
// deployment; the label "invoices" means the same thing everywhere. The same
// goes for the rule that applied one — the rule's NAME is the durable fact, and
// exporting `applied_by_rule_id` would carry a pointer into a table this export
// does not contain.

// ExportLabelRef is one label applied to a message, as an export records it.
type ExportLabelRef struct {
	Name string `json:"name"`
	// AppliedBy is 'user' | 'rule' | 'ml' | 'agent' | 'desk' (#442: the desk's
	// own area label, applied because the message's tab verdict placed it
	// there — see LabelByDesk).
	AppliedBy string `json:"applied_by"`
	// RuleName is the rule that applied it, when AppliedBy is "rule" and the
	// rule still exists. Empty otherwise.
	//
	// ⚠ EMPTY DOES NOT MEAN "no rule was involved". app.rules deletes SET NULL,
	// so a label applied by a rule that has since been deleted keeps
	// applied_by='rule' with nothing to name. AppliedBy is the reliable field;
	// this one is a convenience that can legitimately be blank.
	RuleName string `json:"rule_name,omitempty"`
}

// ExportMessageMeta is one message's non-maildir state.
type ExportMessageMeta struct {
	Labels   []ExportLabelRef
	Keywords []string
	Junk     bool
	// Score is the triage score, and Scored says whether there is one at all.
	// A zero score and no score are different facts, and a bare float cannot
	// tell them apart.
	Score      float32
	Scored     bool
	ScoreModel string
	// DrawerArchivedAt (#443) is when a drawer sweep archived this message, nil
	// if never. Folded into the per-message sidecar rather than given its own
	// file, the same reasoning triage_scores' Score/Scored pair above gives: a
	// second file keyed by message id is a join waiting to disagree.
	DrawerArchivedAt *time.Time
	// DeskArchivedAt (#446) is when DAY-ONE SETUP archived this message IN
	// MAILROOM — took it off the desk without moving anything on the server —
	// nil if it never did.
	//
	// ⚠ A DIFFERENT FACT FROM DrawerArchivedAt above, and the archive must carry
	// both. The drawer mark says the bytes were MOVED to the Archive folder,
	// which is visible in the maildir the export writes beside this file; this
	// one says the message was filed away while the bytes stayed exactly where
	// they were, which nothing in the maildir can express. Drop it and the
	// importing side puts thousands of messages back on the desk.
	DeskArchivedAt *time.Time
	// Attachments carries each projected part's SCAN VERDICT and RELEASE state
	// (#396, ADR-027 §11).
	//
	// ⚠ THIS IS NOT REDUNDANT WITH THE MAILDIR BYTES, and the exporter's own
	// plan says so wrongly: `internal/exportplan` disposes attachments as "MIME
	// parts inside the raw bytes", which is true of the CONTENT and false of the
	// verdict. §11 required verdicts and release state to join this sidecar "in
	// the same increment that first writes the table" — they did not, and the
	// consequence it predicted was live: an export shipped a flagged file's
	// bytes with no verdict, so the silently-waiting attachment came back on the
	// next instance with a clean slate.
	//
	// It is also the answer to the raw-download door. §8's gate covers the part
	// route; the whole message is deliberately still fetchable (the message must
	// stay readable), so the durable record of "one of these parts is
	// quarantined" has to travel with the archive rather than only in a header.
	Attachments []ExportAttachmentScan
}

// ExportAttachmentScan is one part's quarantine record in the sidecar.
type ExportAttachmentScan struct {
	// Part is the MIME part path, which is also JMAP's partId — the one
	// identifier here that means the same thing in another deployment, because
	// it is derived from the message bytes rather than assigned by us.
	Part string `json:"part"`
	// State is 'unscanned'|'clean'|'flagged'|'unknown'.
	//
	// ⚠ `clean` TRAVELS WITHOUT ITS TIMESTAMP AND MUST NOT BE READ AS A
	// GUARANTEE. ADR-027 §1a: a verdict is a claim about what the signatures
	// knew at a moment, and an importer reading an archive months later knows
	// neither the moment nor the signatures. The importing side should re-scan;
	// what this field carries is what WE thought, which is the part that cannot
	// be reconstructed from the bytes.
	State string `json:"state"`
	// Released is the user's own override of a quarantine. It is the one field
	// here a re-scan on the importing side could NOT reproduce — it is a human
	// decision, not a measurement — which is why dropping it would lose the most.
	Released bool `json:"released,omitempty"`
	// Filename as the sender supplied it, for a human reading the archive.
	Filename string `json:"filename,omitempty"`
}

// ExportMessageMetaFor returns the metadata for the given message ids.
//
// It takes a PAGE of ids rather than streaming the whole mailbox, so the
// exporter's memory stays bounded by its page size — the same reason
// ExportMessagePage exists. Ids not present in the result simply have no
// metadata; that is normal (an unlabelled, unscored message).
func (tx *UserTx) ExportMessageMetaFor(ctx context.Context, ids []ulid.ULID) (map[ulid.ULID]*ExportMessageMeta, error) {
	out := make(map[ulid.ULID]*ExportMessageMeta, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	strs := make([]string, len(ids))
	for i, id := range ids {
		strs[i] = id.String()
	}
	get := func(id ulid.ULID) *ExportMessageMeta {
		if m, ok := out[id]; ok {
			return m
		}
		m := &ExportMessageMeta{}
		out[id] = m
		return m
	}

	// Labels, joined to their name and to the applying rule's name.
	rows, err := tx.tx.Query(ctx, `
		SELECT ml.message_id, l.name, ml.applied_by, COALESCE(r.name, '')
		  FROM app.message_labels ml
		  JOIN app.labels l ON l.id = ml.label_id
		  LEFT JOIN app.rules r ON r.id = ml.applied_by_rule_id
		 WHERE ml.message_id = ANY($1::text[])
		 ORDER BY ml.message_id, l.name`, strs)
	if err != nil {
		return nil, fmt.Errorf("store: export message labels: %w", err)
	}
	for rows.Next() {
		var mid, name, by, rule string
		if err := rows.Scan(&mid, &name, &by, &rule); err != nil {
			rows.Close()
			return nil, fmt.Errorf("store: scan export label: %w", err)
		}
		id, err := ulid.Parse(mid)
		if err != nil {
			rows.Close()
			return nil, fmt.Errorf("store: export label message id %q: %w", mid, err)
		}
		m := get(id)
		m.Labels = append(m.Labels, ExportLabelRef{Name: name, AppliedBy: by, RuleName: rule})
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: export message labels: %w", err)
	}

	// Keywords and is_junk come off the message row itself.
	rows, err = tx.tx.Query(ctx, `
		SELECT id, keywords, is_junk
		  FROM app.messages
		 WHERE id = ANY($1::text[])
		   AND (cardinality(keywords) > 0 OR is_junk)`, strs)
	if err != nil {
		return nil, fmt.Errorf("store: export message keywords: %w", err)
	}
	for rows.Next() {
		var mid string
		var kw []string
		var junk bool
		if err := rows.Scan(&mid, &kw, &junk); err != nil {
			rows.Close()
			return nil, fmt.Errorf("store: scan export keywords: %w", err)
		}
		id, err := ulid.Parse(mid)
		if err != nil {
			rows.Close()
			return nil, fmt.Errorf("store: export keyword message id %q: %w", mid, err)
		}
		m := get(id)
		m.Keywords, m.Junk = kw, junk
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: export message keywords: %w", err)
	}

	// Triage scores.
	rows, err = tx.tx.Query(ctx, `
		SELECT message_id, score, model_version
		  FROM app.triage_scores
		 WHERE message_id = ANY($1::text[])`, strs)
	if err != nil {
		return nil, fmt.Errorf("store: export triage scores: %w", err)
	}
	for rows.Next() {
		var mid, model string
		var score float32
		if err := rows.Scan(&mid, &score, &model); err != nil {
			rows.Close()
			return nil, fmt.Errorf("store: scan export score: %w", err)
		}
		id, err := ulid.Parse(mid)
		if err != nil {
			rows.Close()
			return nil, fmt.Errorf("store: export score message id %q: %w", mid, err)
		}
		m := get(id)
		m.Score, m.Scored, m.ScoreModel = score, true, model
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: export triage scores: %w", err)
	}

	// Drawer archive marks (#443): when a sweep moved this message to Archive.
	rows, err = tx.tx.Query(ctx, `
		SELECT message_id, archived_at
		  FROM app.drawer_archives
		 WHERE user_id = $1 AND message_id = ANY($2::text[])`, tx.userID.String(), strs)
	if err != nil {
		return nil, fmt.Errorf("store: export drawer archives: %w", err)
	}
	for rows.Next() {
		var mid string
		var at time.Time
		if err := rows.Scan(&mid, &at); err != nil {
			rows.Close()
			return nil, fmt.Errorf("store: scan export drawer archive: %w", err)
		}
		id, err := ulid.Parse(mid)
		if err != nil {
			rows.Close()
			return nil, fmt.Errorf("store: export drawer archive message id %q: %w", mid, err)
		}
		m := get(id)
		m.DrawerArchivedAt = &at
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: export drawer archives: %w", err)
	}

	// Day-one archive marks (#446): when setup took this message off the desk
	// WITHOUT moving anything. A column on app.messages rather than a table of
	// its own, so this reads the message rows directly.
	rows, err = tx.tx.Query(ctx, `
		SELECT id, desk_archived_at
		  FROM app.messages
		 WHERE user_id = $1 AND id = ANY($2::text[]) AND desk_archived_at IS NOT NULL`,
		tx.userID.String(), strs)
	if err != nil {
		return nil, fmt.Errorf("store: export desk archives: %w", err)
	}
	for rows.Next() {
		var mid string
		var at time.Time
		if err := rows.Scan(&mid, &at); err != nil {
			rows.Close()
			return nil, fmt.Errorf("store: scan export desk archive: %w", err)
		}
		id, err := ulid.Parse(mid)
		if err != nil {
			rows.Close()
			return nil, fmt.Errorf("store: export desk archive message id %q: %w", mid, err)
		}
		m := get(id)
		m.DeskArchivedAt = &at
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: export desk archives: %w", err)
	}

	// Attachment scan verdicts and releases (#396, ADR-027 §11). Scoped by
	// user_id as well as the id list: every other query here is joined through a
	// table RLS already scopes, and this one reads app.attachments directly.
	rows, err = tx.tx.Query(ctx, `
		SELECT a.message_id, a.part_path, a.scan_status, COALESCE(a.filename, ''),
		       EXISTS (SELECT 1 FROM app.attachment_releases r
		                WHERE r.user_id = a.user_id AND r.part_hash = a.part_hash)
		  FROM app.attachments a
		 WHERE a.user_id = $1 AND a.message_id = ANY($2::text[])
		 ORDER BY a.message_id, a.part_path`, tx.userID.String(), strs)
	if err != nil {
		return nil, fmt.Errorf("store: export attachment verdicts: %w", err)
	}
	for rows.Next() {
		var mid string
		var s ExportAttachmentScan
		if err := rows.Scan(&mid, &s.Part, &s.State, &s.Filename, &s.Released); err != nil {
			rows.Close()
			return nil, fmt.Errorf("store: scan export attachment verdict: %w", err)
		}
		id, err := ulid.Parse(mid)
		if err != nil {
			rows.Close()
			return nil, fmt.Errorf("store: export verdict message id %q: %w", mid, err)
		}
		m := get(id)
		m.Attachments = append(m.Attachments, s)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: export attachment verdicts: %w", err)
	}

	return out, nil
}
