// SPDX-License-Identifier: AGPL-3.0-only

package ingest

import (
	"strings"
	"testing"
)

func TestSnippet(t *testing.T) {
	tests := []struct {
		name string
		text string
		html string
		want string
	}{
		{
			name: "plain text unchanged",
			text: "Hello there, this is a normal message.",
			html: "",
			want: "Hello there, this is a normal message.",
		},
		{
			name: "collapses whitespace",
			text: "Hello\n\n  there\tworld   \n",
			html: "",
			want: "Hello there world",
		},
		{
			name: "style block in text/plain is stripped entirely",
			text: "<style type=\"text/css\"> .Roll2:hover{color:red} </style>Real preview text",
			html: "",
			want: "Real preview text",
		},
		{
			name: "script block in text/plain is stripped entirely",
			text: "<script>var a = 1; alert('x');</script>After script",
			html: "",
			want: "After script",
		},
		{
			name: "HTML-only message derives non-empty text from html part",
			text: "",
			html: "<html><head><style>.a{color:blue}</style><title>Ignore me</title></head>" +
				"<body><p>Welcome to our newsletter</p></body></html>",
			want: "Welcome to our newsletter",
		},
		{
			name: "HTML-only: style/script contents never appear",
			text: "",
			html: "<style>.Roll2:hover{background:#fff}</style>" +
				"<script>trackPixel();</script><div>Visible body</div>",
			want: "Visible body",
		},
		{
			name: "entities decoded",
			text: "",
			html: "<p>Tom &amp; Jerry &lt;3 caf&#233;s it&#39;s great</p>",
			want: "Tom & Jerry <3 cafés it's great",
		},
		{
			name: "block boundaries keep words apart (divs)",
			text: "",
			html: "<div>Hello</div><div>World</div>",
			want: "Hello World",
		},
		{
			name: "table cells keep words apart",
			text: "",
			html: "<table><tr><td>Hello</td><td>World</td></tr></table>",
			want: "Hello World",
		},
		{
			name: "inline elements do not split words",
			text: "",
			html: "<p>e<b>mail</b></p>",
			want: "email",
		},
		{
			name: "text part preferred over html part",
			text: "The plain preview",
			html: "<p>The HTML preview</p>",
			want: "The plain preview",
		},
		{
			name: "empty both",
			text: "",
			html: "",
			want: "",
		},
		{
			name: "whitespace-only text falls back to html",
			text: "   \n\t  ",
			html: "<p>From HTML</p>",
			want: "From HTML",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := snippet(tc.text, tc.html)
			if got != tc.want {
				t.Errorf("snippet(%q, %q) = %q, want %q", tc.text, tc.html, got, tc.want)
			}
			if strings.Contains(got, "<style") || strings.Contains(got, "<script") ||
				strings.Contains(got, "hover{") || strings.Contains(got, "alert(") {
				t.Errorf("snippet leaked markup/script/style: %q", got)
			}
		})
	}
}

func TestSnippetTruncatesAtSnippetLen(t *testing.T) {
	long := strings.Repeat("a", snippetLen+50)
	got := snippet(long, "")
	if runes := []rune(got); len(runes) != snippetLen {
		t.Errorf("expected truncation to %d runes, got %d", snippetLen, len(runes))
	}
}

func TestSnippetTruncationCountsRunesNotBytes(t *testing.T) {
	// Multibyte runes: snippetLen runes, not bytes.
	long := strings.Repeat("é", snippetLen+10)
	got := snippet(long, "")
	if runes := []rune(got); len(runes) != snippetLen {
		t.Errorf("expected %d runes, got %d", snippetLen, len(runes))
	}
}

func TestSnippetLongHTMLBodyTruncated(t *testing.T) {
	body := "<p>" + strings.Repeat("word ", 200) + "</p>"
	got := snippet("", body)
	if runes := []rune(got); len(runes) > snippetLen {
		t.Errorf("expected <= %d runes, got %d", snippetLen, len(runes))
	}
	if got == "" {
		t.Error("expected non-empty snippet from long HTML body")
	}
}

// SnippetFromRaw parses a whole RFC 5322 message and runs the SAME snippet
// generator over it — the path the mailroom-admin `resnippet` backfill uses.
// The key case is an HTML-only message whose <style> block must be dropped
// entirely (the pre-v0.23.1 bug this backfill repairs).
func TestSnippetFromRaw(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want string
	}{
		{
			name: "html-only with style block strips markup",
			raw: "From: Alice <alice@example.com>\r\n" +
				"Subject: Hi\r\n" +
				"MIME-Version: 1.0\r\n" +
				"Content-Type: text/html; charset=utf-8\r\n" +
				"\r\n" +
				"<html><head><style>.Roll2:hover{color:red}</style>" +
				"<title>ignore me</title></head>" +
				"<body><p>Real preview text</p></body></html>\r\n",
			want: "Real preview text",
		},
		{
			name: "plain text passes through",
			raw: "From: Alice <alice@example.com>\r\n" +
				"Subject: Hi\r\n" +
				"Content-Type: text/plain; charset=utf-8\r\n" +
				"\r\n" +
				"Just a normal note.\r\n",
			want: "Just a normal note.",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := SnippetFromRaw([]byte(tc.raw))
			if got != tc.want {
				t.Errorf("SnippetFromRaw() = %q, want %q", got, tc.want)
			}
			if strings.Contains(got, "<style") || strings.Contains(got, "hover{") ||
				strings.Contains(got, "ignore me") {
				t.Errorf("SnippetFromRaw leaked markup/style/head: %q", got)
			}
		})
	}
}

func TestSnippetFromRawEmptyInput(t *testing.T) {
	if got := SnippetFromRaw(nil); got != "" {
		t.Errorf("SnippetFromRaw(nil) = %q, want empty", got)
	}
	if got := SnippetFromRaw([]byte("")); got != "" {
		t.Errorf("SnippetFromRaw(empty) = %q, want empty", got)
	}
}
