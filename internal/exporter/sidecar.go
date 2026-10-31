// SPDX-License-Identifier: AGPL-3.0-only

package exporter

import (
	"bufio"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"mailroom/internal/store"
)

// The sidecars carrying what maildir's filename cannot (#54, spec §10).
//
// # Why messages.jsonl and not messages.json
//
// One JSON object per line, written as the maildir pass streams. A single
// top-level array would have to be held whole or hand-assembled with commas,
// and this file has one entry per message — on the archive the importer was
// built to survive, that is millions. JSON Lines is readable by `jq`, appendable
// without rewriting, and costs one message of memory. The export plan's entry
// says `messages.jsonl` for the same reason.
//
// # Why messages are keyed by content hash
//
// The hash IS the message's identity (ADR-001), it is stable across
// deployments, and it is what an importer can match on without trusting
// anything about filenames. The maildir filename is carried too, as a
// convenience for a human reading the archive — but it is the derived value,
// not the key.
//
// ⚠ NOTHING HERE CARRIES A ULID. Every internal id is meaningless in another
// deployment, so labels travel by NAME and the rule that applied one travels by
// its name too. An export full of `01K3F…` would look complete and be useless.

// sidecarMessage is one line of messages.jsonl.
type sidecarMessage struct {
	ContentHash string `json:"content_hash"`
	File        string `json:"file"`
	// Labels carry provenance: #54 names "label provenance dies if the sidecar
	// ignores it" as a specific loss to avoid.
	Labels   []store.ExportLabelRef `json:"labels,omitempty"`
	Keywords []string               `json:"keywords,omitempty"`
	Junk     bool                   `json:"junk,omitempty"`
	// Score is a pointer so "no score" and "a score of zero" are different
	// values in the JSON, as they are different facts in the database.
	Score      *float32 `json:"score,omitempty"`
	ScoreModel string   `json:"score_model,omitempty"`
	// DrawerArchivedAt (#443) is when a drawer sweep moved this message to
	// Archive, as an RFC3339 timestamp — empty when it never did.
	DrawerArchivedAt string `json:"drawer_archived_at,omitempty"`
	// DeskArchivedAt (#446) is when DAY-ONE SETUP took this message off the desk
	// WITHOUT moving anything on the server. Distinct from the field above,
	// which records a real move — see store.ExportMessageMeta.DeskArchivedAt.
	DeskArchivedAt string `json:"desk_archived_at,omitempty"`
	// Attachments is each projected part's scan verdict and release state
	// (#396, ADR-027 §11). ⚠ NOT RECONSTRUCTIBLE FROM THE MAILDIR BYTES: the
	// bytes carry the attachment, not what a scanner said about it, and
	// certainly not the user's own decision to release one. `exportplan` calls
	// attachments "MIME parts inside the raw bytes", which is true of the
	// content and was the reason this was missed.
	Attachments []store.ExportAttachmentScan `json:"attachments,omitempty"`
}

// messageSidecar writes messages.jsonl incrementally.
type messageSidecar struct {
	f   *os.File
	w   *bufio.Writer
	enc *json.Encoder
	n   int64
}

func newMessageSidecar(dir string) (*messageSidecar, error) {
	f, err := os.OpenFile(filepath.Join(dir, "messages.jsonl"),
		os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, fmt.Errorf("exporter: create messages.jsonl: %w", err)
	}
	w := bufio.NewWriter(f)
	return &messageSidecar{f: f, w: w, enc: json.NewEncoder(w)}, nil
}

// write appends one message's line. Messages with nothing to record still get a
// line: the file is then a complete inventory of what the maildir holds, and a
// reader can tell "this message had no labels" from "this message is missing
// from the sidecar", which a sparse file could not.
func (s *messageSidecar) write(hash []byte, file string, meta *store.ExportMessageMeta) error {
	line := sidecarMessage{ContentHash: hex.EncodeToString(hash), File: file}
	if meta != nil {
		line.Labels = meta.Labels
		line.Keywords = meta.Keywords
		line.Junk = meta.Junk
		if meta.Scored {
			score := meta.Score
			line.Score = &score
			line.ScoreModel = meta.ScoreModel
		}
		if meta.DeskArchivedAt != nil {
			line.DeskArchivedAt = meta.DeskArchivedAt.UTC().Format(rfc3339)
		}
		if meta.DrawerArchivedAt != nil {
			line.DrawerArchivedAt = meta.DrawerArchivedAt.UTC().Format(rfc3339)
		}
		line.Attachments = meta.Attachments
	}
	if err := s.enc.Encode(line); err != nil {
		return fmt.Errorf("exporter: write messages.jsonl: %w", err)
	}
	s.n++
	return nil
}

// close flushes and closes, surfacing a flush error rather than losing the tail.
//
// ⚠ A bufio.Writer that is never flushed loses whatever is still buffered, and
// the file looks fine — shorter than it should be, with no error anywhere. That
// is the same silent-truncation shape the exporter refuses for message bytes.
func (s *messageSidecar) close() error {
	if err := s.w.Flush(); err != nil {
		_ = s.f.Close()
		return fmt.Errorf("exporter: flush messages.jsonl: %w", err)
	}
	if err := s.f.Close(); err != nil {
		return fmt.Errorf("exporter: close messages.jsonl: %w", err)
	}
	return nil
}

// sidecarLabel is one entry of labels.json.
type sidecarLabel struct {
	Name string `json:"name"`
	// Parent is the parent label's NAME, not its id.
	Parent       string `json:"parent,omitempty"`
	Color        string `json:"color,omitempty"`
	MirrorToIMAP bool   `json:"mirror_to_imap"`
	// KeywordAtom is the IMAP keyword this label is written to the mail server
	// as (app.label_keyword_map). It is carried because it explains the
	// `MailroomLabel-*` keywords a reader will otherwise find unexplained on
	// their own messages.
	KeywordAtom string `json:"keyword_atom,omitempty"`
	Tab         string `json:"tab,omitempty"`
}

// writeLabels writes labels.json.
func writeLabels(ctx context.Context, st *store.Store, opt Options) (int, error) {
	var labels []store.LabelView
	if err := st.UserTx(ctx, opt.UserID, func(tx *store.UserTx) error {
		var e error
		labels, e = tx.ListLabels(ctx)
		return e
	}); err != nil {
		return 0, fmt.Errorf("exporter: list labels: %w", err)
	}

	// Parents are resolved to names here, where both are in hand.
	byID := make(map[string]string, len(labels))
	for _, l := range labels {
		byID[l.ID.String()] = l.Name
	}

	out := make([]sidecarLabel, 0, len(labels))
	for _, l := range labels {
		e := sidecarLabel{
			Name: l.Name, Color: l.Color, MirrorToIMAP: l.MirrorToIMAP,
			KeywordAtom: l.KeywordAtom, Tab: string(l.Tab),
		}
		if l.ParentID != nil {
			// ⚠ A parent id that names no label we hold is left BLANK rather
			// than written as a ULID. A dangling internal id in an export is
			// worse than an absent field: it looks like data.
			e.Parent = byID[l.ParentID.String()]
		}
		out = append(out, e)
	}
	if err := writeJSON(opt.OutDir, "labels.json", out); err != nil {
		return 0, err
	}
	return len(out), nil
}

// writeJSON writes v as indented JSON to the archive.
func writeJSON(dir, name string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Errorf("exporter: marshal %s: %w", name, err)
	}
	b = append(b, '\n')
	if err := os.WriteFile(filepath.Join(dir, name), b, 0o600); err != nil {
		return fmt.Errorf("exporter: write %s: %w", name, err)
	}
	return nil
}
