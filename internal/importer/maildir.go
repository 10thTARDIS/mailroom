// SPDX-License-Identifier: AGPL-3.0-only

package importer

import (
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// This file is the maildir reader (Maildir, and Maildir++ subfolders).
//
//   - cur/ and new/ are the SAME mailbox. new/ means "never seen by a client", so
//     its messages are unseen; cur/ carries per-message flags in the filename.
//   - tmp/ is skipped: it holds deliveries in progress, which are by definition
//     not yet mail.
//   - A filename's info section (":2,FRS") is the flag set — D draft, F flagged,
//     P passed, R replied, S seen, T trashed. They map onto the same IMAP flag
//     tokens the sync path feeds to ingest.mapFlags, so a maildir's flags and an
//     IMAP server's flags mean the same thing by the time they reach the store.
//   - A Maildir++ subfolder is a sibling directory named ".Work.Projects"; its
//     logical name is "Work/Projects". A plain tree of maildirs is also handled:
//     any directory with cur/ and new/ is a folder, named by its path.
//
// Memory: one folder's FILENAMES (not its messages) are held while it is read,
// because the walk order has to be stable for the cursor to mean anything. That
// is ~50 bytes per message — 50 MB for a folder with a million messages in it,
// which no real maildir has. The messages themselves are read one at a time.

// maildirTree reads every maildir folder under a root, in sorted order.
type maildirTree struct {
	root    string
	folders []string // absolute paths, sorted
	max     int64

	fi    int // index of the current folder
	files []maildirEntry
	mi    int // index into files

	doneBytes int64
}

// maildirEntry is one message file: its path, the flags encoded in its name, and
// the name itself (which is the cursor within a folder).
type maildirEntry struct {
	path  string
	name  string
	isNew bool
}

func openMaildirTree(root string, maxMessageBytes int64) (Source, error) {
	var folders []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, werr error) error {
		if werr != nil {
			return werr
		}
		if !d.IsDir() {
			return nil
		}
		switch filepath.Base(path) {
		case "cur", "new", "tmp":
			return filepath.SkipDir
		}
		if isMaildir(path) {
			folders = append(folders, path)
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("importer: walk maildir: %w", err)
	}
	if len(folders) == 0 {
		return nil, fmt.Errorf("importer: no maildir folders under %s", root)
	}
	sort.Strings(folders)
	t := &maildirTree{root: root, folders: folders, max: maxMessageBytes, fi: -1}
	return t, nil
}

// Size returns 0: a maildir's total is only knowable by stat-ing every message,
// which for a large archive is a slow pass we refuse to make the operator wait
// for. Progress is reported in messages, not bytes (progress.go).
func (t *maildirTree) Size() int64 { return 0 }

func (t *maildirTree) Done() int64 { return t.doneBytes }

func (t *maildirTree) Close() error { return nil }

// Seek resumes after pos: skip the folders already done, and inside the current
// folder skip every name <= pos.Name. Both are directory listings, not reads.
func (t *maildirTree) Seek(pos Position) error {
	if pos.Source == "" {
		return nil
	}
	for i, f := range t.folders {
		if maildirFolderName(t.root, f) != pos.Source {
			continue
		}
		if err := t.openFolder(i); err != nil {
			return err
		}
		for t.mi < len(t.files) && t.files[t.mi].name <= pos.Name {
			t.mi++
		}
		return nil
	}
	return nil // unknown folder: start from the beginning (dedup keeps it correct)
}

// openFolder lists one folder's cur/ and new/ in a stable order.
func (t *maildirTree) openFolder(i int) error {
	t.fi = i
	t.mi = 0
	t.files = nil
	dir := t.folders[i]
	for _, sub := range []string{"cur", "new"} {
		names, err := readDirNames(filepath.Join(dir, sub))
		if err != nil {
			return err
		}
		for _, n := range names {
			t.files = append(t.files, maildirEntry{
				path: filepath.Join(dir, sub, n), name: n, isNew: sub == "new",
			})
		}
	}
	// One order for both subdirectories: the cursor is a NAME, and a name must
	// mean the same position no matter which of cur/new it came from.
	sort.Slice(t.files, func(a, b int) bool { return t.files[a].name < t.files[b].name })
	return nil
}

func (t *maildirTree) Next() (*Item, error) {
	for {
		if t.fi < 0 || t.mi >= len(t.files) {
			next := t.fi + 1
			if next >= len(t.folders) {
				return nil, io.EOF
			}
			if err := t.openFolder(next); err != nil {
				return nil, err
			}
			continue
		}
		e := t.files[t.mi]
		t.mi++

		folder := maildirFolderName(t.root, t.folders[t.fi])
		item := &Item{
			Folder: folder,
			Flags:  maildirFlags(e.name, e.isNew),
			After:  Position{Source: folder, Name: e.name},
		}

		fi, err := os.Stat(e.path)
		if err != nil {
			return item, fmt.Errorf("%w: stat %s: %v", ErrBadItem, e.path, err)
		}
		if fi.Size() > t.max {
			t.doneBytes += fi.Size()
			item.Bytes = fi.Size()
			return item, fmt.Errorf("%w: %s is %d bytes, over the %d-byte limit",
				ErrBadItem, e.path, fi.Size(), t.max)
		}
		raw, err := os.ReadFile(e.path) //nolint:gosec // path comes from the operator's archive
		if err != nil {
			return item, fmt.Errorf("%w: read %s: %v", ErrBadItem, e.path, err)
		}
		t.doneBytes += int64(len(raw))
		item.Bytes = int64(len(raw))
		if len(raw) == 0 {
			return item, fmt.Errorf("%w: %s is empty", ErrBadItem, e.path)
		}
		item.Raw = raw
		item.ReceivedAt = maildirTime(e.name, fi.ModTime())
		return item, nil
	}
}

// readDirNames lists a directory's file names. A missing subdirectory (a maildir
// with no new/) is empty, not an error.
func readDirNames(dir string) ([]string, error) {
	f, err := os.Open(dir) //nolint:gosec // path comes from the operator's archive
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("importer: open %s: %w", dir, err)
	}
	defer func() { _ = f.Close() }()
	names, err := f.Readdirnames(-1)
	if err != nil {
		return nil, fmt.Errorf("importer: list %s: %w", dir, err)
	}
	out := names[:0]
	for _, n := range names {
		if strings.HasPrefix(n, ".") {
			continue // dotfiles are maildir bookkeeping (dovecot-uidlist et al.)
		}
		out = append(out, n)
	}
	sort.Strings(out)
	return out, nil
}

// maildirFolderName renders a folder's logical name. Maildir++ encodes hierarchy
// in a dotted sibling directory (".Work.Projects" -> "Work/Projects"); a plain
// tree of maildirs is named by its relative path. The maildir root itself is
// INBOX, which is the maildir convention.
func maildirFolderName(root, dir string) string {
	rel := relSlash(root, dir)
	if rel == "." || rel == "" {
		return "INBOX"
	}
	if strings.HasPrefix(rel, ".") {
		return strings.ReplaceAll(strings.TrimPrefix(rel, "."), ".", "/")
	}
	return rel
}

// maildirFlags maps a maildir filename's info section to IMAP flag tokens.
// A message in new/ has been delivered but never opened: unseen, no flags.
func maildirFlags(name string, isNew bool) []string {
	if isNew {
		return nil
	}
	i := strings.LastIndex(name, ":2,")
	if i < 0 {
		return nil
	}
	var flags []string
	for _, c := range name[i+3:] {
		switch c {
		case 'S':
			flags = append(flags, `\Seen`)
		case 'R':
			flags = append(flags, `\Answered`)
		case 'F':
			flags = append(flags, `\Flagged`)
		case 'D':
			flags = append(flags, `\Draft`)
		case 'T':
			flags = append(flags, `\Deleted`) // ingest drops it: a lifecycle flag
		case 'P':
			// "Passed" (resent/forwarded) has no IMAP system flag; not worth a
			// keyword nobody will ever query.
		}
	}
	return flags
}

// maildirTime recovers the delivery time from a maildir filename, whose first
// field is the delivery timestamp in Unix seconds ("1699999999.M8P9.host"). It is
// the archive's own record of when the message arrived; the file's mtime is the
// fallback, and it is often wrong (a copy, an rsync, a restore all rewrite it).
func maildirTime(name string, modTime time.Time) time.Time {
	if i := strings.IndexAny(name, ".,:"); i > 0 {
		if secs, err := strconv.ParseInt(name[:i], 10, 64); err == nil && secs > 0 {
			return time.Unix(secs, 0).UTC()
		}
	}
	return modTime.UTC()
}
