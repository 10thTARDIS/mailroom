// SPDX-License-Identifier: AGPL-3.0-only

package mime

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const corpusDir = "testdata/corpus"

// TestCorpusNoPanic is the blanket smoke test over the whole golden corpus:
// every fixture must parse without panicking and must yield a non-nil
// Message, whether or not Parse also returns an error. This is the
// "hostile input must never crash the parser" contract from
// docs/mime-policy.md, independent of what each fixture is specifically
// exercising.
func TestCorpusNoPanic(t *testing.T) {
	files, err := filepath.Glob(filepath.Join(corpusDir, "*.eml"))
	if err != nil {
		t.Fatalf("glob corpus: %v", err)
	}
	if len(files) == 0 {
		t.Fatalf("no fixtures found under %s", corpusDir)
	}

	for _, f := range files {
		f := f
		t.Run(filepath.Base(f), func(t *testing.T) {
			data, err := os.ReadFile(f)
			if err != nil {
				t.Fatalf("reading fixture: %v", err)
			}

			var msg *Message
			var parseErr error
			var panicked any
			func() {
				defer func() { panicked = recover() }()
				msg, parseErr = ParseBytes(data)
			}()

			if panicked != nil {
				t.Fatalf("ParseBytes panicked on hostile input (must never happen): %v", panicked)
			}
			if msg == nil {
				t.Fatalf("ParseBytes returned a nil Message (want a best-effort Message even on error); err = %v", parseErr)
			}
		})
	}
}

// TestCorpusExtractions asserts specific, per-fixture expected extractions,
// pinning down exactly what docs/mime-policy.md promises for each hostile
// scenario in the corpus.
func TestCorpusExtractions(t *testing.T) {
	tests := []struct {
		name  string
		file  string
		check func(t *testing.T, msg *Message, err error)
	}{
		{
			name: "malformed RFC 2047 encoded-word falls back to raw token text",
			file: "malformed_rfc2047_subject.eml",
			check: func(t *testing.T, msg *Message, err error) {
				if err != nil {
					t.Fatalf("Parse: %v", err)
				}
				subject := msg.Header.Subject()
				if !strings.Contains(subject, "Hello") {
					t.Errorf("Subject() = %q, want it to contain the decoded word %q", subject, "Hello")
				}
				if !strings.Contains(subject, "!!!not-base64!!!") {
					t.Errorf("Subject() = %q, want the malformed encoded-word to fall back to its raw text", subject)
				}
				if msg.Truncated {
					t.Errorf("Truncated = true, want false")
				}
			},
		},
		{
			name: "charset declared but unrecognized substitutes U+FFFD",
			file: "unknown_charset.eml",
			check: func(t *testing.T, msg *Message, err error) {
				if err != nil {
					t.Fatalf("Parse: %v", err)
				}
				if !strings.Contains(msg.Text, "Known ASCII text") {
					t.Errorf("Text = %q, want the ASCII portion preserved", msg.Text)
				}
				if !strings.ContainsRune(msg.Text, '�') {
					t.Errorf("Text = %q, want the undecodable byte replaced with U+FFFD", msg.Text)
				}
				var found bool
				for _, w := range msg.Warnings {
					if strings.Contains(w, "x-mailroom-hostile-9000") {
						found = true
					}
				}
				if !found {
					t.Errorf("Warnings = %v, want one mentioning the unsupported charset", msg.Warnings)
				}
			},
		},
		{
			name: "unencoded 8-bit bytes fall back to permissive windows-1252 (not U+FFFD)",
			file: "unencoded_8bit.eml",
			check: func(t *testing.T, msg *Message, err error) {
				if err != nil {
					t.Fatalf("Parse: %v", err)
				}
				if subject := msg.Header.Subject(); !strings.Contains(subject, "Café") {
					t.Errorf("Subject() = %q, want it to contain windows-1252-decoded %q", subject, "Café")
				}
				if !strings.Contains(msg.Text, "café") {
					t.Errorf("Text = %q, want it to contain windows-1252-decoded %q", msg.Text, "café")
				}
				if strings.ContainsRune(msg.Text, '�') {
					t.Errorf("Text = %q, want no replacement characters (unencoded 8-bit uses windows-1252, not U+FFFD)", msg.Text)
				}
			},
		},
		{
			name: "nested message/rfc822 recurses 2-3 levels within the depth cap",
			file: "nested_rfc822.eml",
			check: func(t *testing.T, msg *Message, err error) {
				if err != nil {
					t.Fatalf("Parse: %v", err)
				}
				if !strings.Contains(msg.Text, "Top-level body.") {
					t.Errorf("Text = %q, want it to contain %q", msg.Text, "Top-level body.")
				}
				if len(msg.Nested) != 1 {
					t.Fatalf("len(Nested) = %d, want 1", len(msg.Nested))
				}
				lvl1 := msg.Nested[0]
				if got := lvl1.Header.Subject(); got != "Level 1 nested" {
					t.Errorf("Nested[0].Header.Subject() = %q, want %q", got, "Level 1 nested")
				}
				if !strings.Contains(lvl1.Text, "Level 1 body.") {
					t.Errorf("Nested[0].Text = %q, want it to contain %q", lvl1.Text, "Level 1 body.")
				}
				if len(lvl1.Nested) != 1 {
					t.Fatalf("len(Nested[0].Nested) = %d, want 1", len(lvl1.Nested))
				}
				lvl2 := lvl1.Nested[0]
				if got := lvl2.Header.Subject(); got != "Level 2 nested" {
					t.Errorf("Nested[0].Nested[0].Header.Subject() = %q, want %q", got, "Level 2 nested")
				}
				if !strings.Contains(lvl2.Text, "Level 2 body -- deepest.") {
					t.Errorf("Nested[0].Nested[0].Text = %q, want it to contain the deepest body", lvl2.Text)
				}
				if msg.Truncated {
					t.Errorf("Truncated = true, want false (well within MaxDepth)")
				}
			},
		},
		{
			name: "quoted-printable text and base64 attachment both decode",
			file: "quoted_printable_and_base64.eml",
			check: func(t *testing.T, msg *Message, err error) {
				if err != nil {
					t.Fatalf("Parse: %v", err)
				}
				if !strings.Contains(msg.Text, "€10") {
					t.Errorf("Text = %q, want it to contain the quoted-printable-decoded euro sign", msg.Text)
				}
				if len(msg.Attachments) != 1 {
					t.Fatalf("len(Attachments) = %d, want 1", len(msg.Attachments))
				}
				att := msg.Attachments[0]
				if att.Filename != "hello.txt" {
					t.Errorf("Attachments[0].Filename = %q, want %q", att.Filename, "hello.txt")
				}
				if string(att.Data) != "Hello, attachment!\n" {
					t.Errorf("Attachments[0].Data = %q, want %q", att.Data, "Hello, attachment!\n")
				}
			},
		},
		{
			name: "multipart/alternative yields both text and HTML bodies",
			file: "multipart_alternative.eml",
			check: func(t *testing.T, msg *Message, err error) {
				if err != nil {
					t.Fatalf("Parse: %v", err)
				}
				if !strings.Contains(msg.Text, "Plain body text.") {
					t.Errorf("Text = %q, want it to contain %q", msg.Text, "Plain body text.")
				}
				if !strings.Contains(msg.HTML, "HTML body text.") {
					t.Errorf("HTML = %q, want it to contain %q", msg.HTML, "HTML body text.")
				}
				if len(msg.Parts) != 2 {
					t.Errorf("len(Parts) = %d, want 2", len(msg.Parts))
				}
				if len(msg.Attachments) != 0 {
					t.Errorf("len(Attachments) = %d, want 0", len(msg.Attachments))
				}
			},
		},
		{
			name: "unparsable garbage degrades to a best-effort Message plus a defined error",
			file: "truncated_garbage.eml",
			check: func(t *testing.T, msg *Message, err error) {
				if err == nil {
					t.Fatalf("Parse: want a non-nil error for unparsable garbage, got nil")
				}
				if msg == nil {
					t.Fatalf("msg = nil, want a non-nil best-effort Message even on fatal parse failure")
				}
				if !msg.Truncated {
					t.Errorf("Truncated = false, want true")
				}
				if len(msg.Warnings) == 0 {
					t.Errorf("Warnings is empty, want at least one entry explaining the failure")
				}
			},
		},
		{
			name: "missing headers still yield a usable Message",
			file: "missing_headers.eml",
			check: func(t *testing.T, msg *Message, err error) {
				if err != nil {
					t.Fatalf("Parse: %v", err)
				}
				if got := msg.Header.Subject(); got != "" {
					t.Errorf("Subject() = %q, want empty", got)
				}
				if from := msg.Header.From(); from != nil {
					t.Errorf("From() = %v, want nil", from)
				}
				if _, ok := msg.Header.Date(); ok {
					t.Errorf("Date() ok = true, want false")
				}
				if !strings.Contains(msg.Text, "Just a body, no headers at all.") {
					t.Errorf("Text = %q, want it to contain the body", msg.Text)
				}
			},
		},
		{
			name: "oversized message/rfc822 nesting is truncated at the depth cap",
			file: "oversized_nesting_bomb.eml",
			check: func(t *testing.T, msg *Message, err error) {
				if err != nil {
					t.Fatalf("Parse: %v", err)
				}
				if !msg.Truncated {
					t.Errorf("Truncated = false, want true (25 levels of message/rfc822 exceeds MaxDepth=%d)", MaxDepth)
				}
				depth := 0
				cur := msg
				for len(cur.Nested) > 0 {
					depth++
					cur = cur.Nested[0]
				}
				if depth > MaxDepth {
					t.Errorf("reached nesting depth %d, want <= MaxDepth (%d)", depth, MaxDepth)
				}
			},
		},
	}

	// Every fixture on disk must be exercised by a table entry above, so a
	// new hostile fixture can't silently go unchecked.
	files, err := filepath.Glob(filepath.Join(corpusDir, "*.eml"))
	if err != nil {
		t.Fatalf("glob corpus: %v", err)
	}
	covered := make(map[string]bool, len(tests))
	for _, tc := range tests {
		covered[tc.file] = true
	}
	for _, f := range files {
		if base := filepath.Base(f); !covered[base] {
			t.Errorf("corpus fixture %s has no TestCorpusExtractions table entry", base)
		}
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			data, err := os.ReadFile(filepath.Join(corpusDir, tc.file))
			if err != nil {
				t.Fatalf("reading fixture %s: %v", tc.file, err)
			}
			msg, parseErr := ParseBytes(data)
			tc.check(t, msg, parseErr)
		})
	}
}
