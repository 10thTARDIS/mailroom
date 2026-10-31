// SPDX-License-Identifier: AGPL-3.0-only

package importer

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// ErrBadItem marks a per-message failure the reader RECOVERED from: the item is
// unreadable (truncated, absurdly large, an unreadable file) but the source is
// still positioned and the run continues. Next returns it alongside a non-nil
// Item carrying the cursor to skip past, so the caller counts it and moves on.
// A nil Item with a non-nil error is fatal (the archive itself is unreadable).
var ErrBadItem = errors.New("importer: unusable message")

// Position is the durable cursor into an archive. It is deliberately
// coarse-grained and format-specific:
//
//   - mbox: Source is the mbox file (relative to the archive root) and Offset is
//     the byte offset of the START of the next message. Resume is a seek, so a
//     20 GB archive resumes in O(1) rather than by re-reading and re-hashing it.
//   - maildir: Source is the folder and Name is the filename of the last message
//     COMPLETED. Resume skips folders already done and, within the current folder,
//     every name lexicographically <= Name. Maildir names are stable and the walk
//     order is sorted, so that is exact — and it costs a directory listing, not a
//     read of the messages.
type Position struct {
	Source string `json:"source"`
	Offset int64  `json:"offset,omitempty"`
	Name   string `json:"name,omitempty"`
}

// Item is one message read out of an archive.
type Item struct {
	// Raw is the message's bytes, VERBATIM as the archive stored them (the mbox
	// `From ` separator removed and mboxrd `>From ` unescaped — neither is part of
	// the message). They are what gets content-hashed and encrypted, so what comes
	// back out of the blob store is exactly what the archive held.
	Raw []byte
	// Folder is the archive's own folder for this message ("INBOX", "Work/Old").
	// A Gmail label mapping (gmail.go) may override it.
	Folder string
	// Flags are IMAP-style flag tokens (`\Seen`, `\Flagged`, ...) recovered from
	// the archive; ingest maps them with the same mapFlags the sync path uses.
	Flags []string
	// Labels are label names to attach (Gmail's X-Gmail-Labels, minus the ones
	// that mean a folder or a flag).
	Labels []string
	// ReceivedAt is the archive's evidence of delivery time; zero when it has none
	// (ingest then falls back to the Date header).
	ReceivedAt time.Time
	// Bytes is how much of the archive this item consumed (progress accounting,
	// not the message size).
	Bytes int64
	// After is the cursor to persist once this item has been COMMITTED.
	After Position
}

// Source streams messages out of an archive. Implementations hold at most one
// message in memory at a time.
type Source interface {
	// Next returns the next item. It returns (nil, io.EOF) at the end,
	// (item, err) wrapping ErrBadItem for a recoverable per-message failure, and
	// (nil, err) for a fatal one.
	Next() (*Item, error)
	// Seek positions the source just after pos (resume). A position the source
	// cannot honor is not an error: it restarts from the beginning, which is
	// slower but never wrong (dedup carries correctness, not the cursor).
	Seek(pos Position) error
	// Size is the archive's total size in bytes, for progress/ETA; 0 = unknown.
	Size() int64
	// Done is how many bytes of the archive have been consumed so far.
	Done() int64
	Close() error
}

// Kind is the archive layout.
type Kind string

const (
	KindAuto    Kind = "auto"
	KindMbox    Kind = "mbox"    // one mbox file, or a directory tree of them
	KindMaildir Kind = "maildir" // a maildir (or a tree of maildirs)
)

// Detect works out what an archive is. A directory that contains cur/ and new/
// (or any descendant that does) is a maildir; anything else is treated as
// mbox-shaped, which is also what a Google Takeout is once unpacked
// (Takeout/Mail/*.mbox).
func Detect(root string) (Kind, error) {
	fi, err := os.Stat(root)
	if err != nil {
		return "", fmt.Errorf("importer: stat archive: %w", err)
	}
	if !fi.IsDir() {
		return KindMbox, nil
	}
	maildir := false
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil //nolint:nilerr // an unreadable subtree is not a detection failure
		}
		if d.IsDir() && isMaildir(path) {
			maildir = true
			return filepath.SkipAll
		}
		return nil
	})
	if err != nil {
		return "", fmt.Errorf("importer: detect archive: %w", err)
	}
	if maildir {
		return KindMaildir, nil
	}
	return KindMbox, nil
}

// Open opens an archive of the given kind (KindAuto detects it).
func Open(root string, kind Kind, maxMessageBytes int64) (Source, error) {
	if kind == "" || kind == KindAuto {
		k, err := Detect(root)
		if err != nil {
			return nil, err
		}
		kind = k
	}
	switch kind {
	case KindMbox:
		return openMboxTree(root, maxMessageBytes)
	case KindMaildir:
		return openMaildirTree(root, maxMessageBytes)
	default:
		return nil, fmt.Errorf("importer: unknown archive kind %q", kind)
	}
}

// isMaildir reports whether dir is a maildir folder (it has cur/ and new/).
func isMaildir(dir string) bool {
	for _, sub := range []string{"cur", "new"} {
		fi, err := os.Stat(filepath.Join(dir, sub))
		if err != nil || !fi.IsDir() {
			return false
		}
	}
	return true
}

// mboxExts are the file extensions treated as mbox files when walking a
// directory. A Google Takeout's Mail/ directory holds `*.mbox`; hand-rolled
// archives are often extensionless or `.mbox.txt`.
var mboxExts = map[string]bool{".mbox": true, ".mbx": true, "": true}

// listMboxFiles finds the mbox files under root, in a STABLE (sorted) order —
// the order is part of the cursor's meaning, so it must not depend on the
// filesystem's whims. Zero-length and obviously-non-mbox files are skipped.
func listMboxFiles(root string) ([]string, error) {
	fi, err := os.Stat(root)
	if err != nil {
		return nil, fmt.Errorf("importer: stat archive: %w", err)
	}
	if !fi.IsDir() {
		return []string{root}, nil
	}
	var out []string
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, werr error) error {
		if werr != nil {
			return werr
		}
		if d.IsDir() {
			return nil
		}
		if !mboxExts[strings.ToLower(filepath.Ext(path))] {
			return nil
		}
		info, ierr := d.Info()
		if ierr != nil || info.Size() == 0 {
			return nil //nolint:nilerr // an empty or vanished file has nothing to import
		}
		out = append(out, path)
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("importer: walk archive: %w", err)
	}
	sort.Strings(out)
	return out, nil
}

// relSlash renders path relative to root with forward slashes — the stable,
// platform-independent identity a Position and a mailbox name are built from.
func relSlash(root, path string) string {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		rel = filepath.Base(path)
	}
	return filepath.ToSlash(rel)
}
