// SPDX-License-Identifier: AGPL-3.0-only

// Package exporter writes one user's mail out as a maildir (#54, spec §10).
//
// It is the mirror of internal/importer and is deliberately shaped like it:
// streaming (one message of memory, not one archive), and refusing to treat a
// partial read as data.
//
// # Why maildir, and why that choice carries the flags for free
//
// Identity in this system is SHA-256 of the raw RFC 5322 bytes (ADR-001), so an
// export is only correct if it is BYTE-EXACT. mbox cannot promise that: its
// "From " line stuffing is reversible only under a convention both ends agree
// on, and any CRLF normalisation changes the hash — which means changes the
// message's identity. Maildir stores one message per file, verbatim, and has no
// such transformation.
//
// ⚠ AND MAILDIR ALREADY CARRIES THE READ STATE, which is the problem #54 warns
// about by name: "an export that writes only messages re-imports everything
// unread". RFC 5322 has no read flag, so an mbox export would need a sidecar and
// an importer that consults it. Maildir puts the flags in the FILENAME's info
// section (":2,S" and friends) — and internal/importer/maildir.go already parses
// exactly that, mapping the tokens onto the same IMAP flags the sync path feeds
// to ingest. So seen/answered/flagged/draft survive a round trip through the
// format, with no new agreement between the two halves to get wrong.
//
// The info section's vocabulary is FIXED, though, so everything else needs a
// sidecar: is_junk, arbitrary IMAP keywords, LABELS and their provenance, and
// the triage score all ride in messages.jsonl beside the maildir. See
// sidecar.go for why that file is JSON Lines and keyed by content hash.
package exporter

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/blob"
	"mailroom/internal/store"
)

// pageSize is how many messages' metadata are held at once. The BYTES are
// streamed one message at a time regardless; this only bounds the row buffer.
const pageSize = 500

// Options configures one export.
type Options struct {
	UserID ulid.ULID
	// OutDir must not exist, or must be an empty directory. An export that
	// merged into a populated directory would produce an archive nobody could
	// describe: part this user's, part whatever was already there.
	OutDir string
}

// Result is what an export did.
type Result struct {
	Messages int64
	// Config is a PER-FILE count of the configuration sidecars. Per file, never
	// one total: a single number cannot distinguish "this user has no rules"
	// from "the rules query returned nothing because it is broken".
	Config map[string]int
	// Account is the email the archive belongs to — the durable identifier, used
	// in the manifest INSTEAD of the internal user id.
	Account string
	// Labels is how many labels landed in labels.json.
	Labels int
	Bytes  int64
	// Unreadable are messages whose bytes could not be retrieved or did not
	// verify. They are RETURNED, never skipped silently — see Export.
	Unreadable []Unreadable
}

// Unreadable is one message the export could not write.
type Unreadable struct {
	MessageID   string `json:"message_id"`
	ContentHash string `json:"content_hash"`
	Reason      string `json:"reason"`
}

// manifest is written beside the maildir so the archive describes itself.
type manifest struct {
	Format    string `json:"format"`
	FormatVer int    `json:"format_version"`
	// Account is the EMAIL this archive belongs to, not the internal user id.
	//
	// ⚠ IT USED TO BE THE ULID, AND A NEGATIVE CONTROL OVER EVERY WRITTEN FILE
	// CAUGHT IT. The archive's own rule is that nothing in it may be a pointer
	// into a database the reader does not have — and the manifest was the one
	// file exempting itself. The email is what identifies the account to a human
	// and to any other mail system, which makes it the better answer anyway.
	Account    string         `json:"account"`
	ExportedAt time.Time      `json:"exported_at"`
	Messages   int64          `json:"messages"`
	Labels     int            `json:"labels"`
	Config     map[string]int `json:"config"`
	Bytes      int64          `json:"bytes"`
	Unreadable []Unreadable   `json:"unreadable"`
	// Limits is written INTO the archive rather than only into a changelog,
	// because the person reading this file in three years is the one who needs
	// to know what is not in it.
	Limits []string `json:"limits"`
}

// Limits are the things this export does not carry, stated in the archive
// itself. Kept here rather than in the command so the manifest and the operator
// cannot drift apart.
var Limits = []string{
	"PGP-encrypted mail is exported as ciphertext. Reading it needs the private " +
		"key, which is NOT in this archive by design — the key and the ciphertext " +
		"in one directory would be protected by nothing. Get it separately with " +
		"`mailroom-admin pgp-export-wrapped`, which writes it under a passphrase " +
		"you choose (spec §9, ADR-024 §4). Without that file this mail is " +
		"permanently unreadable, including by the system it came from.",
	"Your sign-in identifier is not carried: it names you to the identity provider of " +
		"the system this came from, and is meaningless — and correlatable — elsewhere.",
	"Correspondents' public keys are listed by FINGERPRINT and source, not as key " +
		"bytes: the fingerprint is what a person verifies, and the key itself is " +
		"refetchable from its published source.",
	"Not carried at all, because they describe the system rather than you: sync " +
		"bookkeeping, IMAP folder coordinates, conversation groupings (rebuilt from the " +
		"messages), the work queue, model artifacts, and suggestion/training records.",
	"Trashed messages carry maildir's 'T'. Mailroom's own importer treats that as " +
		"a lifecycle flag and drops it, so re-importing this archive returns them untrashed.",
	"THE MAIL CAN BE READ BACK; THE SIDECARS CANNOT. The message files are a " +
		"maildir, which Mailroom's importer reads (as it reads mbox), recovering " +
		"seen/answered/flagged/draft from the filename. But messages.jsonl and the " +
		"configuration files are a RECORD, not a restore path: nothing imports them, " +
		"here or anywhere else. Labels, custom keywords, junk marks, triage scores, " +
		"rules, saved searches, tabs and identities come back only by a person reading " +
		"them. If the goal is to move a whole instance rather than to hold your own " +
		"mail, this is the wrong tool — that is a database restore (scripts/backup.sh " +
		"plus the restore drill), which carries all of it.",
}

// ⚠ THE LAST LIMIT ABOVE IS A DECISION, NOT A TODO (#418 item 4).
//
// The export is an ARCHIVE FORMAT — the answer to "can I get my own mail out"
// (spec §18, #54) — and deliberately not a migration path. The recovery route
// for an instance is scripts/backup.sh plus pg_restore, which is whole-database
// and is drilled as a required check; the export is drilled only for the mail
// itself, by TestExportRoundTripsByteExactThroughTheImportReader.
//
// ⚠ IT HAS A CONSEQUENCE WORTH SEEING. #396's argument for exporting scan
// verdicts was that "an export that ships a flagged file's bytes with no verdict
// resurrects the problem on the next instance" — reasoning that presumes the
// archive IS how you move to a next instance. Under this decision that presumption
// is wrong, and the verdicts in messages.jsonl are there so a HUMAN can see what
// was quarantined, not so an importer can re-apply it. Both are worth having;
// they are not the same claim, and building an importer later would change which
// one is true.

// Export writes the user's mail to opt.OutDir as a maildir.
//
// ⚠ A MESSAGE WHOSE BYTES DO NOT VERIFY IS REPORTED, NEVER WRITTEN. blob.Get's
// contract is that authentication and content-hash failures surface on the
// TERMINATING read, so a caller that streams straight to a file and stops
// caring has already written unverified bytes by the time it finds out. This
// hashes what it writes and compares against the identity the database
// recorded; a mismatch removes the file and records the message as unreadable.
// An export that quietly contained one corrupt message would be worse than one
// that failed.
func Export(ctx context.Context, st *store.Store, blobs blob.BlobStore, dek []byte, opt Options) (Result, error) {
	var res Result

	if opt.OutDir == "" {
		return res, errors.New("exporter: an output directory is required")
	}
	if err := prepareDir(opt.OutDir); err != nil {
		return res, err
	}
	cur := filepath.Join(opt.OutDir, "maildir", "cur")
	for _, d := range []string{
		filepath.Join(opt.OutDir, "maildir", "tmp"),
		filepath.Join(opt.OutDir, "maildir", "new"),
		cur,
	} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return res, fmt.Errorf("exporter: create %s: %w", d, err)
		}
	}

	side, err := newMessageSidecar(opt.OutDir)
	if err != nil {
		return res, err
	}
	sidecarClosed := false
	defer func() {
		if !sidecarClosed {
			_ = side.close()
		}
	}()

	var after ulid.ULID
	for {
		var (
			page []store.ExportMessage
			meta map[ulid.ULID]*store.ExportMessageMeta
		)
		if err := st.UserTx(ctx, opt.UserID, func(tx *store.UserTx) error {
			var e error
			if page, e = tx.ExportMessagePage(ctx, after, pageSize); e != nil {
				return e
			}
			ids := make([]ulid.ULID, len(page))
			for i, m := range page {
				ids[i] = m.ID
			}
			meta, e = tx.ExportMessageMetaFor(ctx, ids)
			return e
		}); err != nil {
			return res, fmt.Errorf("exporter: read page: %w", err)
		}
		if len(page) == 0 {
			break
		}
		for _, m := range page {
			name := maildirName(m)
			n, err := writeOne(ctx, blobs, dek, opt.UserID, cur, m, name)
			if err != nil {
				res.Unreadable = append(res.Unreadable, Unreadable{
					MessageID:   m.ID.String(),
					ContentHash: hex.EncodeToString(m.ContentHash),
					Reason:      err.Error(),
				})
				continue
			}
			// ⚠ The sidecar line is written ONLY for a message whose bytes
			// landed. A line describing a file that is not there would make the
			// archive claim more than it holds.
			if err := side.write(m.ContentHash, name, meta[m.ID]); err != nil {
				return res, err
			}
			res.Messages++
			res.Bytes += n
		}
		after = page[len(page)-1].ID

		if err := ctx.Err(); err != nil {
			return res, err
		}
	}

	if err := side.close(); err != nil {
		return res, err
	}
	sidecarClosed = true
	if side.n != res.Messages {
		// Not reachable by construction, and asserted anyway: a sidecar with a
		// different number of lines than the maildir has files is an archive
		// whose two halves disagree, and silence would let that ship.
		return res, fmt.Errorf("exporter: messages.jsonl has %d lines but %d messages "+
			"were written; the archive's halves disagree", side.n, res.Messages)
	}

	labels, err := writeLabels(ctx, st, opt)
	if err != nil {
		return res, err
	}
	res.Labels = labels

	cfg, accountEmail, err := writeConfig(ctx, st, opt)
	if err != nil {
		return res, err
	}
	res.Config = cfg
	res.Account = accountEmail

	if err := writeManifest(opt, res); err != nil {
		return res, err
	}
	return res, nil
}

// prepareDir refuses anything but a missing or empty directory.
func prepareDir(dir string) error {
	entries, err := os.ReadDir(dir)
	switch {
	case os.IsNotExist(err):
		return os.MkdirAll(dir, 0o700)
	case err != nil:
		return fmt.Errorf("exporter: read %s: %w", dir, err)
	case len(entries) > 0:
		return fmt.Errorf("exporter: %s is not empty; refusing to merge an export "+
			"into an existing directory", dir)
	}
	return nil
}

// writeOne streams a single message to disk and returns its byte count.
func writeOne(ctx context.Context, blobs blob.BlobStore, dek []byte, userID ulid.ULID,
	cur string, m store.ExportMessage, name string,
) (int64, error) {
	var h blob.ContentHash
	if len(m.ContentHash) != len(h) {
		return 0, fmt.Errorf("content_hash is %d bytes, want %d", len(m.ContentHash), len(h))
	}
	copy(h[:], m.ContentHash)

	rc, err := blobs.Get(ctx, userID, dek, h)
	if err != nil {
		return 0, fmt.Errorf("open blob: %w", err)
	}
	defer func() { _ = rc.Close() }()

	path := filepath.Join(cur, name)
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return 0, fmt.Errorf("create %s: %w", filepath.Base(path), err)
	}
	// Any failure past this point leaves no file behind: a half-written message
	// that a later import would read as a truncated one is the worst artifact
	// this code could produce.
	remove := true
	defer func() {
		_ = f.Close()
		if remove {
			_ = os.Remove(path)
		}
	}()

	sum := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, sum), rc)
	if err != nil {
		// ⚠ This is where blob.Get reports authentication and hash failures, so
		// it is an integrity error as often as it is an I/O one.
		return 0, fmt.Errorf("read blob: %w", err)
	}
	if err := f.Close(); err != nil {
		return 0, fmt.Errorf("close %s: %w", filepath.Base(path), err)
	}
	if got := sum.Sum(nil); !equalBytes(got, m.ContentHash) {
		return 0, fmt.Errorf("the bytes written hash to %s but the database records %s",
			hex.EncodeToString(got), hex.EncodeToString(m.ContentHash))
	}
	remove = false
	return n, nil
}

func equalBytes(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// maildirName builds `<unixtime>.<content-hash>.mailroom:2,<flags>`.
//
// ⚠ THE INFO SECTION IS THE FLAG CARRIER, and the flag letters must be in
// ASCII order — that is the maildir convention, and internal/importer reads them
// case-insensitively but other clients do not all.
//
// ⚠ THE UNIQUE TOKEN IS THE CONTENT HASH, NOT THE MESSAGE'S ULID, and the change
// was forced by a test. Maildir needs some unique token per file; the first
// version used the ULID, which meant every line of messages.jsonl carried an
// internal id in its `file` field — and the sidecar's own rule is that nothing
// in an archive may be a pointer into a table the archive does not contain.
//
// The hash is better on every axis: it is unique per user by construction
// (identity IS (user_id, content_hash), ADR-001), it means the same thing in
// any deployment, and it makes the filename SELF-DESCRIBING — a reader can
// verify a file against its own name with sha256sum and nothing else.
func maildirName(m store.ExportMessage) string {
	var flags []string
	if m.Draft {
		flags = append(flags, "D")
	}
	if m.Flagged {
		flags = append(flags, "F")
	}
	if m.Answered {
		flags = append(flags, "R")
	}
	if m.Seen {
		flags = append(flags, "S")
	}
	if m.Trashed != nil {
		// ⚠ 'T' is maildir's "deleted". It is written because it is the truthful
		// encoding of a trashed message, and because another mail client reading
		// this archive should see it as trashed. Mailroom's OWN importer maps T
		// to \Deleted and ingest drops it as a lifecycle flag, so a round trip
		// through Mailroom returns the message untrashed. Stated in Limits.
		flags = append(flags, "T")
	}
	sort.Strings(flags)

	name := fmt.Sprintf("%d.%s.mailroom:2,", m.ReceivedAt.UTC().Unix(),
		hex.EncodeToString(m.ContentHash))
	for _, f := range flags {
		name += f
	}
	return name
}

func writeManifest(opt Options, res Result) error {
	man := manifest{
		Format:     "mailroom-export-maildir",
		FormatVer:  1,
		Account:    res.Account,
		ExportedAt: time.Now().UTC(),
		Messages:   res.Messages,
		Labels:     res.Labels,
		Config:     res.Config,
		Bytes:      res.Bytes,
		Unreadable: res.Unreadable,
		Limits:     Limits,
	}
	if man.Unreadable == nil {
		man.Unreadable = []Unreadable{}
	}
	b, err := json.MarshalIndent(man, "", "  ")
	if err != nil {
		return fmt.Errorf("exporter: marshal manifest: %w", err)
	}
	b = append(b, '\n')
	path := filepath.Join(opt.OutDir, "manifest.json")
	if err := os.WriteFile(path, b, 0o600); err != nil {
		return fmt.Errorf("exporter: write manifest: %w", err)
	}
	return nil
}
