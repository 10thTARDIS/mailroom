// SPDX-License-Identifier: AGPL-3.0-only

package importer

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// drain reads every item a source yields, returning the good ones and the count of
// recoverable per-item failures — the same split Run makes.
func drain(t *testing.T, src Source) (items []*Item, bad int) {
	t.Helper()
	for {
		item, err := src.Next()
		switch {
		case errors.Is(err, io.EOF):
			return items, bad
		case err != nil && item == nil:
			t.Fatalf("fatal source error: %v", err)
		case err != nil:
			bad++
		default:
			items = append(items, item)
		}
		if len(items)+bad > 10000 {
			t.Fatal("source does not terminate")
		}
	}
}

func writeFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

const twoMessages = "From alice@example.com Thu Jan  1 00:00:00 1970\r\n" +
	"From: Alice <alice@example.com>\r\n" +
	"Subject: one\r\n" +
	"\r\n" +
	"body one\r\n" +
	"\r\n" +
	"From bob@example.com Fri Jan  2 03:04:05 1970\r\n" +
	"From: Bob <bob@example.com>\r\n" +
	"Subject: two\r\n" +
	"\r\n" +
	"body two\r\n"

func TestMboxSplitsMessagesAndDropsTheSeparator(t *testing.T) {
	dir := t.TempDir()
	path := writeFile(t, dir, "archive.mbox", twoMessages)

	src, err := Open(path, KindMbox, DefaultMaxMessageBytes)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = src.Close() }()

	items, bad := drain(t, src)
	if bad != 0 {
		t.Fatalf("bad items = %d, want 0", bad)
	}
	if len(items) != 2 {
		t.Fatalf("messages = %d, want 2", len(items))
	}
	for i, want := range []string{"Subject: one", "Subject: two"} {
		raw := string(items[i].Raw)
		if strings.HasPrefix(raw, "From ") {
			t.Errorf("message %d still carries the mbox separator line:\n%q", i, raw)
		}
		if !strings.Contains(raw, want) {
			t.Errorf("message %d does not contain %q:\n%q", i, want, raw)
		}
		// The blank separator line before the next `From ` is not part of the
		// message: a message that grows a trailing blank line every time it is
		// exported and re-imported would get a NEW content hash each round trip and
		// dedup would stop working.
		if strings.HasSuffix(raw, "\n\n") || strings.HasSuffix(raw, "\r\n\r\n") {
			t.Errorf("message %d kept the separator's blank line:\n%q", i, raw)
		}
	}
	if got, want := items[0].ReceivedAt.UTC(), time.Date(1970, 1, 1, 0, 0, 0, 0, time.UTC); !got.Equal(want) {
		t.Errorf("message 0 received_at = %s, want %s (from the `From ` line)", got, want)
	}
	if got, want := items[1].ReceivedAt.UTC(), time.Date(1970, 1, 2, 3, 4, 5, 0, time.UTC); !got.Equal(want) {
		t.Errorf("message 1 received_at = %s, want %s", got, want)
	}
	if items[0].Folder != "archive" {
		t.Errorf("folder = %q, want %q", items[0].Folder, "archive")
	}
}

// TestMboxPreservesBytesVerbatim is the identity contract. A message's identity IS
// its bytes (ADR-001), so the reader may remove the mbox framing — the `From `
// line and the blank line that separates messages — and NOTHING else. An earlier
// version of this reader rebuilt held-back blank lines as "\n", which silently
// rewrote the header/body separator of every CRLF message: the content hash then
// no longer matched the same message synced from IMAP (so dedup failed), and the
// blob stored bytes the archive never contained.
func TestMboxPreservesBytesVerbatim(t *testing.T) {
	dir := t.TempDir()
	msg := "From: Alice <alice@example.com>\r\n" +
		"Subject: crlf\r\n" +
		"\r\n" + // the header/body separator: a CRLF blank line
		"first paragraph\r\n" +
		"\r\n" + // an interior blank line, part of the body
		"\r\n" + // two in a row
		"second paragraph\r\n"
	content := "From alice@example.com Thu Jan  1 00:00:00 1970\r\n" + msg + "\r\n" +
		"From bob@example.com Thu Jan  1 00:00:00 1970\r\n" + msg
	path := writeFile(t, dir, "crlf.mbox", content)

	src, err := Open(path, KindMbox, DefaultMaxMessageBytes)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = src.Close() }()
	items, bad := drain(t, src)
	if bad != 0 || len(items) != 2 {
		t.Fatalf("items=%d bad=%d, want 2/0", len(items), bad)
	}
	for i, it := range items {
		if string(it.Raw) != msg {
			t.Errorf("message %d is not byte-identical to what the archive held.\n got: %q\nwant: %q",
				i, it.Raw, msg)
		}
	}
}

func TestMboxUnescapesFromLines(t *testing.T) {
	dir := t.TempDir()
	body := "From alice@example.com Thu Jan  1 00:00:00 1970\n" +
		"Subject: quoting\n" +
		"\n" +
		">From the desk of Alice\n" +
		">>From deeper still\n" +
		"not >From escaped\n"
	path := writeFile(t, dir, "q.mbox", body)

	src, err := Open(path, KindMbox, DefaultMaxMessageBytes)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = src.Close() }()
	items, _ := drain(t, src)
	if len(items) != 1 {
		t.Fatalf("messages = %d, want 1", len(items))
	}
	raw := string(items[0].Raw)
	if !strings.Contains(raw, "\nFrom the desk of Alice\n") {
		t.Errorf("mboxrd escape not reversed:\n%q", raw)
	}
	if !strings.Contains(raw, "\n>From deeper still\n") {
		t.Errorf(">>From should unescape to >From:\n%q", raw)
	}
	if !strings.Contains(raw, "not >From escaped") {
		t.Errorf("a mid-line >From must not be touched:\n%q", raw)
	}
}

func TestMboxSkipsOversizedMessageAndKeepsGoing(t *testing.T) {
	dir := t.TempDir()
	huge := strings.Repeat("x", 4096)
	content := "From a@example.com Thu Jan  1 00:00:00 1970\n" +
		"Subject: small\n\nok\n\n" +
		"From b@example.com Thu Jan  1 00:00:00 1970\n" +
		"Subject: huge\n\n" + huge + "\n\n" +
		"From c@example.com Thu Jan  1 00:00:00 1970\n" +
		"Subject: also small\n\nok\n"
	path := writeFile(t, dir, "big.mbox", content)

	src, err := Open(path, KindMbox, 1024) // a 1 KiB ceiling
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = src.Close() }()

	items, bad := drain(t, src)
	if bad != 1 {
		t.Errorf("bad items = %d, want 1 (the oversized message)", bad)
	}
	if len(items) != 2 {
		t.Fatalf("good messages = %d, want 2 — an oversized message must not eat its neighbours", len(items))
	}
	if !strings.Contains(string(items[1].Raw), "also small") {
		t.Errorf("the message AFTER the oversized one was not recovered:\n%q", items[1].Raw)
	}
}

func TestMboxHandlesGarbageAndHeadlessFiles(t *testing.T) {
	dir := t.TempDir()
	// No leading `From ` line (the file starts mid-message) and a truncated final
	// message with no body at all. Both are ordinary in real archives.
	content := "Subject: headless\n\nbody\n\n" +
		"From x@example.com Thu Jan  1 00:00:00 1970\n" +
		"Subject: truncated"
	path := writeFile(t, dir, "odd.mbox", content)

	src, err := Open(path, KindMbox, DefaultMaxMessageBytes)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = src.Close() }()
	items, bad := drain(t, src)
	if bad != 0 {
		t.Errorf("bad = %d, want 0 (both are readable, if odd)", bad)
	}
	if len(items) != 2 {
		t.Fatalf("messages = %d, want 2", len(items))
	}
	if !strings.HasPrefix(string(items[0].Raw), "Subject: headless") {
		t.Errorf("a file starting mid-message must still yield it: %q", items[0].Raw)
	}
	if !strings.Contains(string(items[1].Raw), "truncated") {
		t.Errorf("a header-only final message must still be yielded: %q", items[1].Raw)
	}
}

// TestMboxSeekResumesExactly is the resume contract: replaying from a saved cursor
// must yield exactly the messages that came after it — no gap (lost mail) and no
// overlap beyond what dedup absorbs.
func TestMboxSeekResumesExactly(t *testing.T) {
	dir := t.TempDir()
	var b strings.Builder
	for i := 0; i < 10; i++ {
		b.WriteString("From s@example.com Thu Jan  1 00:00:00 1970\n")
		b.WriteString("Subject: msg\n\n")
		b.WriteString("body\n\n")
	}
	path := writeFile(t, dir, "ten.mbox", b.String())

	src, err := Open(path, KindMbox, DefaultMaxMessageBytes)
	if err != nil {
		t.Fatal(err)
	}
	var after Position
	for i := 0; i < 4; i++ {
		item, err := src.Next()
		if err != nil {
			t.Fatal(err)
		}
		after = item.After
	}
	_ = src.Close()

	src2, err := Open(path, KindMbox, DefaultMaxMessageBytes)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = src2.Close() }()
	if err := src2.Seek(after); err != nil {
		t.Fatal(err)
	}
	rest, bad := drain(t, src2)
	if bad != 0 {
		t.Fatalf("bad = %d after seek", bad)
	}
	if len(rest) != 6 {
		t.Fatalf("after resuming past 4 of 10 messages, got %d, want 6", len(rest))
	}
	for i, it := range rest {
		if !strings.HasPrefix(string(it.Raw), "Subject: msg") {
			t.Fatalf("resumed message %d is not aligned to a message boundary: %q", i, it.Raw)
		}
	}
}

func TestMboxTreeReadsEveryFileInAStableOrder(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "b.mbox", "From a@example.com Thu Jan  1 00:00:00 1970\nSubject: in b\n\nx\n")
	writeFile(t, dir, "a.mbox", "From a@example.com Thu Jan  1 00:00:00 1970\nSubject: in a\n\nx\n")

	src, err := Open(dir, KindMbox, DefaultMaxMessageBytes)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = src.Close() }()
	items, _ := drain(t, src)
	if len(items) != 2 {
		t.Fatalf("messages = %d, want 2", len(items))
	}
	if items[0].Folder != "a" || items[1].Folder != "b" {
		t.Errorf("folders = %q,%q — the walk order must be sorted, or the cursor means nothing",
			items[0].Folder, items[1].Folder)
	}
}

func TestParseFromLineDate(t *testing.T) {
	for _, tc := range []struct {
		line string
		want time.Time
	}{
		{"From alice@example.com Thu Jan  1 00:00:00 1970\n",
			time.Date(1970, 1, 1, 0, 0, 0, 0, time.UTC)},
		{"From 1234567@mail Sat Jan 03 12:30:00 +0000 2015\n",
			time.Date(2015, 1, 3, 12, 30, 0, 0, time.UTC)},
		{"From nobody Mon Feb 29 01:02:03 UTC 2016\n",
			time.Date(2016, 2, 29, 1, 2, 3, 0, time.UTC)},
		{"From garbage\n", time.Time{}},
		{"", time.Time{}},
	} {
		got := parseFromLineDate([]byte(tc.line))
		if !got.Equal(tc.want) {
			t.Errorf("parseFromLineDate(%q) = %s, want %s", tc.line, got, tc.want)
		}
	}
}
