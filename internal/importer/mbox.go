// SPDX-License-Identifier: AGPL-3.0-only

package importer

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// This file is the streaming mbox reader. It never holds more than ONE message,
// so a 25 GB Google Takeout costs the same memory as a 25 KB test fixture.
//
// Framing (RFC 4155 in practice, plus what real archives actually do):
//
//   - A message begins at a line starting with "From " that is at the start of the
//     file or preceded by a blank line. That blank line is the SEPARATOR, not part
//     of the message, so it is held back and dropped (heldBlank below).
//   - The `From ` line itself is not part of the message either: it carries an
//     envelope sender and a delivery date, which is often the only evidence of
//     when the message arrived, so we parse it and throw the line away.
//   - Body lines matching ">From ", ">>From " ... are mboxrd escapes: one '>' is
//     stripped, restoring the bytes as they were delivered. Storing the message
//     verbatim matters — the content hash is over exactly these bytes (ADR-001),
//     and what comes back out of the blob store must be what the archive held.
//   - A file that does not start with a `From ` line is not rejected; we assume it
//     begins mid-message and read it anyway. Real archives are like this.

// mboxSep is the message separator prefix.
var mboxSep = []byte("From ")

// mboxTree reads every mbox file under a root, in sorted order. A single mbox
// file is just a tree of one.
type mboxTree struct {
	root  string
	files []string
	sizes []int64
	total int64

	idx      int // index into files of the CURRENT file (== len(files) when done)
	cur      *mboxFile
	priorLen int64 // bytes in the files already completed
	max      int64
}

func openMboxTree(root string, maxMessageBytes int64) (Source, error) {
	files, err := listMboxFiles(root)
	if err != nil {
		return nil, err
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("importer: no mbox files under %s", root)
	}
	// Positions and folder names are relative to the archive DIRECTORY. When the
	// archive is a single mbox file, that is its parent — so the file's own name is
	// the folder ("archive.mbox" -> "archive") rather than the empty relative path.
	base := root
	if fi, err := os.Stat(root); err == nil && !fi.IsDir() {
		base = filepath.Dir(root)
	}
	t := &mboxTree{root: base, files: files, max: maxMessageBytes}
	t.sizes = make([]int64, len(files))
	for i, f := range files {
		if fi, err := os.Stat(f); err == nil {
			t.sizes[i] = fi.Size()
			t.total += fi.Size()
		}
	}
	return t, nil
}

func (t *mboxTree) Size() int64 { return t.total }

func (t *mboxTree) Done() int64 {
	done := t.priorLen
	if t.cur != nil {
		done += t.cur.offset
	}
	return done
}

func (t *mboxTree) Close() error {
	if t.cur != nil {
		err := t.cur.close()
		t.cur = nil
		return err
	}
	return nil
}

// Seek resumes at a saved position. An unrecognizable position (the archive was
// replaced, the file is gone) rewinds to the start rather than failing: the cursor
// is an optimization, and correctness comes from content-hash dedup.
func (t *mboxTree) Seek(pos Position) error {
	if pos.Source == "" {
		return nil
	}
	idx := -1
	for i, f := range t.files {
		if relSlash(t.root, f) == pos.Source {
			idx = i
			break
		}
	}
	if idx < 0 {
		return nil
	}
	if err := t.Close(); err != nil {
		return err
	}
	t.idx = idx
	t.priorLen = 0
	for i := 0; i < idx; i++ {
		t.priorLen += t.sizes[i]
	}
	f, err := openMboxFile(t.files[idx], relSlash(t.root, t.files[idx]), t.max)
	if err != nil {
		return err
	}
	if err := f.seek(pos.Offset); err != nil {
		_ = f.close()
		return err
	}
	t.cur = f
	return nil
}

func (t *mboxTree) Next() (*Item, error) {
	for {
		if t.cur == nil {
			if t.idx >= len(t.files) {
				return nil, io.EOF
			}
			f, err := openMboxFile(t.files[t.idx], relSlash(t.root, t.files[t.idx]), t.max)
			if err != nil {
				return nil, err
			}
			t.cur = f
		}
		item, err := t.cur.next()
		if errors.Is(err, io.EOF) {
			t.priorLen += t.sizes[t.idx]
			t.idx++
			if cerr := t.Close(); cerr != nil {
				return nil, cerr
			}
			continue
		}
		return item, err
	}
}

// mboxFile is one open mbox file, positioned at a message boundary.
type mboxFile struct {
	path string
	rel  string
	f    *os.File
	br   *bufio.Reader
	max  int64

	offset int64 // byte offset of the next byte to be read
	// sep is the `From ` separator line that begins the NEXT message: reading it
	// is how we discover the current one ended, so it is carried over.
	sep       []byte
	sepOffset int64
	sepValid  bool

	line []byte       // reusable line buffer, bounded by max
	msg  bytes.Buffer // the message under construction, bounded by max
	base string       // folder name derived from the file
	eof  bool
}

func openMboxFile(path, rel string, max int64) (*mboxFile, error) {
	f, err := os.Open(path) //nolint:gosec // the archive path is an operator argument
	if err != nil {
		return nil, fmt.Errorf("importer: open mbox %s: %w", path, err)
	}
	return &mboxFile{
		path: path, rel: rel, f: f,
		br:   bufio.NewReaderSize(f, 64<<10),
		max:  max,
		base: folderForMbox(rel),
	}, nil
}

// folderForMbox names the mailbox a plain mbox file maps to: its path within the
// archive, minus the extension. A Google Takeout's "All mail Including Spam and
// Trash.mbox" only ever supplies a FALLBACK here — every message in it carries
// X-Gmail-Labels, and the label mapping (gmail.go) overrides the folder.
func folderForMbox(rel string) string {
	name := strings.TrimSuffix(rel, filepath.Ext(rel))
	name = strings.Trim(name, "/")
	if name == "" || name == "." {
		return "Archive"
	}
	return name
}

func (m *mboxFile) close() error {
	if err := m.f.Close(); err != nil {
		return fmt.Errorf("importer: close mbox %s: %w", m.path, err)
	}
	return nil
}

func (m *mboxFile) seek(off int64) error {
	if off <= 0 {
		return nil
	}
	if _, err := m.f.Seek(off, io.SeekStart); err != nil {
		return fmt.Errorf("importer: seek mbox %s: %w", m.path, err)
	}
	m.br.Reset(m.f)
	m.offset = off
	m.sepValid = false
	return nil
}

// readLine reads one line including its trailing newline, bounded: a single line
// longer than max is truncated in memory (the tail is counted and discarded), so
// a file that is one enormous line cannot exhaust memory. It returns the line and
// how many bytes of the file it consumed.
func (m *mboxFile) readLine() (line []byte, n int64, err error) {
	m.line = m.line[:0]
	for {
		chunk, rerr := m.br.ReadSlice('\n')
		n += int64(len(chunk))
		if int64(len(m.line)) < m.max {
			room := m.max - int64(len(m.line))
			if int64(len(chunk)) > room {
				chunk = chunk[:room]
			}
			m.line = append(m.line, chunk...)
		}
		if rerr == nil {
			return m.line, n, nil
		}
		if errors.Is(rerr, bufio.ErrBufferFull) {
			continue // a long line: keep pulling
		}
		if errors.Is(rerr, io.EOF) {
			if len(m.line) == 0 && n == 0 {
				return nil, 0, io.EOF
			}
			return m.line, n, nil // a final line with no newline
		}
		return nil, n, fmt.Errorf("importer: read mbox %s: %w", m.path, rerr)
	}
}

// isSeparator reports whether line begins a new message.
func isSeparator(line []byte) bool { return bytes.HasPrefix(line, mboxSep) }

// next reads the next message. It returns io.EOF at the end of the file, and
// (item, ErrBadItem) for a message that is too large or empty — the file stays
// positioned at the next message, so the run continues.
func (m *mboxFile) next() (*Item, error) {
	if m.eof {
		return nil, io.EOF
	}

	// Find the `From ` line that starts this message. On the very first call it is
	// the first line of the file — unless the file begins mid-message, in which
	// case we read from where we are and keep whatever is there.
	m.msg.Reset()
	var (
		fromLine []byte
		start    = m.offset
	)
	switch {
	case m.sepValid:
		fromLine = append([]byte(nil), m.sep...)
		start = m.sepOffset
		m.sepValid = false
	default:
		line, n, err := m.readLine()
		if errors.Is(err, io.EOF) {
			m.eof = true
			return nil, io.EOF
		}
		if err != nil {
			return nil, err
		}
		m.offset += n
		if isSeparator(line) {
			fromLine = append([]byte(nil), line...)
		} else {
			// Not a separator: this file starts (or resumes) mid-message. Keep the
			// line — it is the first line of a real message.
			m.msg.Write(unescapeFrom(line))
		}
	}

	receivedAt := parseFromLineDate(fromLine)

	// held is the blank line most recently read, VERBATIM (its own bytes, CRLF or
	// LF — never a synthesized newline). A blank line is only a separator artifact
	// if a `From ` line follows it; otherwise it belongs to the message and must go
	// back in EXACTLY as it was. Reconstructing it as "\n" would rewrite every CRLF
	// message's header/body separator, changing the bytes — and the bytes are the
	// message's identity (ADR-001), so it would change its content hash, break
	// dedup against the same message synced over IMAP, and store a blob that is not
	// what the archive held.
	var (
		held     []byte
		hasHeld  bool
		oversize bool
	)
	flushHeld := func() {
		if hasHeld {
			m.msg.Write(held)
			hasHeld = false
		}
	}
	for {
		line, n, err := m.readLine()
		if errors.Is(err, io.EOF) {
			m.eof = true
			break
		}
		if err != nil {
			return nil, err
		}

		// A `From ` line preceded by a blank line ends the current message and
		// begins the next one. Carry it over rather than un-reading it; the held
		// blank was the separator's and is dropped.
		if hasHeld && isSeparator(line) {
			m.sep = append(m.sep[:0], line...)
			m.sepOffset = m.offset
			m.sepValid = true
			m.offset += n
			break
		}
		m.offset += n

		if isBlankLine(line) {
			// A run of blank lines: all but the last belong to the message.
			flushHeld()
			held = append(held[:0], line...)
			hasHeld = true
			continue
		}
		flushHeld()
		if oversize || int64(m.msg.Len())+int64(len(line)) > m.max {
			oversize = true
			continue // keep scanning for the separator; do not accumulate
		}
		m.msg.Write(unescapeFrom(line))
	}

	item := &Item{
		Folder:     m.base,
		ReceivedAt: receivedAt,
		After:      Position{Source: m.rel, Offset: m.offset},
	}
	if m.sepValid {
		// The next message's separator has been read but not consumed: the cursor
		// must point AT it, so a resume re-reads it as that message's `From ` line.
		item.After.Offset = m.sepOffset
	}
	item.Bytes = item.After.Offset - start
	switch {
	case oversize:
		return item, fmt.Errorf("%w: message at %s:%d exceeds the %d-byte limit",
			ErrBadItem, m.rel, start, m.max)
	case m.msg.Len() == 0:
		return item, fmt.Errorf("%w: empty message at %s:%d", ErrBadItem, m.rel, start)
	}
	item.Raw = append([]byte(nil), m.msg.Bytes()...)
	return item, nil
}

// isBlankLine reports whether line is empty apart from its line terminator.
func isBlankLine(line []byte) bool {
	return len(bytes.TrimRight(line, "\r\n")) == 0
}

// unescapeFrom reverses the mboxrd escape: ">From " -> "From ", ">>From " ->
// ">From ". Everything else is returned untouched.
func unescapeFrom(line []byte) []byte {
	i := 0
	for i < len(line) && line[i] == '>' {
		i++
	}
	if i > 0 && bytes.HasPrefix(line[i:], mboxSep) {
		return line[1:]
	}
	return line
}

// fromLineLayouts are the shapes a `From ` line's date takes in the wild:
// asctime (the classic), asctime with a zone name, and the numeric-offset form
// Google Takeout writes.
var fromLineLayouts = []string{
	"Mon Jan _2 15:04:05 2006",
	"Mon Jan _2 15:04:05 MST 2006",
	"Mon Jan _2 15:04:05 -0700 2006",
	"Mon Jan _2 15:04:05 2006 MST",
	"Mon Jan _2 15:04:05 2006 -0700",
}

// parseFromLineDate recovers the delivery time from an mbox `From ` line
// ("From sender@example.com Thu Jan  1 00:00:00 1970"). It is frequently the only
// evidence of when a message arrived — a Date: header is attacker-supplied and
// often absent — so it is worth the parse. A line we cannot read yields the zero
// time, and ingest falls back to the Date header.
func parseFromLineDate(fromLine []byte) time.Time {
	if len(fromLine) == 0 {
		return time.Time{}
	}
	s := strings.TrimRight(string(fromLine), "\r\n")
	s = strings.TrimPrefix(s, "From ")
	// Drop the envelope sender: the date is everything after the first space.
	sp := strings.IndexByte(s, ' ')
	if sp < 0 {
		return time.Time{}
	}
	s = strings.TrimSpace(s[sp+1:])
	for _, layout := range fromLineLayouts {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC()
		}
	}
	return time.Time{}
}
