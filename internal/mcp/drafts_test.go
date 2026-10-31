// SPDX-License-Identifier: AGPL-3.0-only

package mcp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"mailroom/internal/mcp/mcptest"
)

// THE DRAFT TOOLS' WORDING, THE HIDDEN-TOOL REFUSAL AND MARKDOWN (#872),
// against a fake backend. The server-side halves (collapse, replace, discard)
// are in internal/api/mcp_drafts_integration_test.go.

// draftBackend answers an Email/set create with a new draft (or the canned
// answer), and records every call.
func draftBackend(mode SendMode, sendAllowed bool) *fakeBackend {
	f := &fakeBackend{mode: mode, allowed: map[string]bool{"Email/set": true, "Email/get": true,
		"Identity/get": true, "PendingAction/get": true, "EmailSubmission/set": sendAllowed}}
	f.answer = func(c Invocation) Invocation {
		switch c.Name {
		case "Email/set":
			return inv("Email/set", c.CallID, map[string]any{"created": map[string]any{"d": map[string]any{"id": "D1"}}})
		case "EmailSubmission/set":
			return inv("error", c.CallID, map[string]any{"type": "forbidden",
				"description": "this key does not have the scope mail:send that EmailSubmission/set needs here"})
		}
		return inv("error", c.CallID, map[string]any{"type": "unknownMethod"})
	}
	return f
}

func TestDraftWordingFollowsSendMode(t *testing.T) {
	ctx := context.Background()
	draftArgs := map[string]any{"to": []string{"bob@example.net"}, "subject": "Lunch", "body": "Thursday?"}
	for _, c := range []struct {
		name        string
		mode        SendMode
		sendAllowed bool
		wantSend    bool
		wantApprove bool
	}{
		{"no send", SendNone, false, false, false},
		{"send", SendDirect, true, true, false},
		{"send with approval", SendWithApproval, true, true, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := connect(t, draftBackend(c.mode, c.sendAllowed))
			tools, err := s.Tools(ctx)
			if err != nil {
				t.Fatal(err)
			}
			listed, _ := json.Marshal(tools)
			drafts := 0
			for _, tl := range tools {
				if !strings.HasPrefix(tl.Name, "draft_") || tl.Name == "draft_discard" {
					continue
				}
				drafts++
				mentions := strings.Contains(tl.Description, "send_draft")
				if mentions != c.wantSend {
					t.Errorf("%s description mentions send_draft = %v, want %v: %s", tl.Name, mentions, c.wantSend, tl.Description)
				}
				if says := strings.Contains(tl.Description, cannotSend); says == c.wantSend {
					t.Errorf("%s description says the key cannot send = %v: %s", tl.Name, says, tl.Description)
				}
				if approve := strings.Contains(tl.Description, "sends wait for the account owner's approval"); c.wantSend && approve != c.wantApprove {
					t.Errorf("%s description mentions approval = %v, want %v", tl.Name, approve, c.wantApprove)
				}
			}
			if drafts != 3 {
				t.Fatalf("listed %d of draft_new, draft_reply, draft_update", drafts)
			}
			// Nowhere in a no-send key's whole tools/list: not a description,
			// not a property's.
			if !c.wantSend && strings.Contains(string(listed), "send_draft") {
				t.Errorf("a key that cannot send was shown send_draft: %s", listed)
			}

			res := mcpOK(t, s, "draft_new", draftArgs)
			if mentions := strings.Contains(res.Text, "send_draft"); mentions != c.wantSend {
				t.Errorf("draft_new result mentions send_draft = %v, want %v: %s", mentions, c.wantSend, res.Text)
			}
			if says := strings.Contains(res.Text, cannotSend); says == c.wantSend {
				t.Errorf("draft_new result says the key cannot send = %v: %s", says, res.Text)
			}
			if o := res.Object(); o["status"] != "draft" || o["emailId"] != "D1" {
				t.Errorf("draft_new result %v", o)
			}
		})
	}
}

func mcpOK(t *testing.T, s *mcptest.Session, name string, args any) mcptest.Result {
	t.Helper()
	res, err := s.Call(context.Background(), name, args)
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	if res.IsError {
		t.Fatalf("%s: tool error %s", name, res.Text)
	}
	return res
}

func TestHiddenToolNamesTheMissingScope(t *testing.T) {
	f := draftBackend(SendNone, false)
	f.lacking = []string{"mail:send"}
	s := connect(t, f)
	res, err := s.Call(context.Background(), "send_draft", map[string]any{"emailId": "D1"})
	if err != nil {
		t.Fatal(err)
	}
	want := "send_draft is not available: this key does not have permission to send email (mail:send)"
	if !res.IsError || !strings.Contains(res.Text, want) {
		t.Fatalf("send_draft without mail:send: %+v, want a tool error saying %q", res, want)
	}
	e, _ := res.Object()["error"].(map[string]any)
	detail, _ := e["detail"].(map[string]any)
	if e["type"] != "forbidden" || detail["serverError"] == nil {
		t.Fatalf("the refusal should be forbidden and keep the server's error as detail: %v", e)
	}
	// The call still went to the backend, whose gate refuses and audits it.
	if len(f.calls) != 1 || f.calls[0].Name != "EmailSubmission/set" {
		t.Fatalf("calls %v: the hidden tool's JMAP call must still reach the dispatcher", f.calls)
	}

	if got := notAvailable("draft_reply", []string{"mail:draft", "mail:read"}); got !=
		"draft_reply is not available: this key does not have permission to write drafts or read mail (mail:draft, mail:read)" {
		t.Errorf("two scopes: %q", got)
	}
	if got := notAvailable("find_contacts", []string{LacksRestriction}); !strings.Contains(got, "restricted") {
		t.Errorf("restriction: %q", got)
	}
}

// A LISTED tool's errors are untouched: only a hidden one is reworded.
func TestListedToolErrorIsNotReworded(t *testing.T) {
	f := draftBackend(SendDirect, true)
	s := connect(t, f)
	res, err := s.Call(context.Background(), "send_draft", map[string]any{"emailId": "D1"})
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError || strings.Contains(res.Text, "is not available") {
		t.Fatalf("a listed tool's error was reworded: %s", res.Text)
	}
}

func TestDraftResultsReadTheServer(t *testing.T) {
	ctx := context.Background()
	f := draftBackend(SendNone, false)
	var created map[string]any
	f.answer = func(c Invocation) Invocation {
		return inv("Email/set", c.CallID, map[string]any{"created": map[string]any{"d": created},
			"discarded": []any{"D1"}})
	}
	s := connect(t, f)

	created = map[string]any{"id": "D1", "alreadySaved": true}
	res, err := s.Call(ctx, "draft_new", map[string]any{"to": []string{"bob@example.net"}, "subject": "s", "body": "b"})
	if err != nil {
		t.Fatal(err)
	}
	o := res.Object()
	if res.IsError || o["status"] != "already_saved" || o["emailId"] != "D1" ||
		!strings.Contains(res.Text, "already saved") || !strings.Contains(res.Text, "nothing new was written") {
		t.Fatalf("a collapsed repeat: %+v", res)
	}

	created = map[string]any{"id": "D2", "replacedEmailId": "D1"}
	res, err = s.Call(ctx, "draft_update", map[string]any{"emailId": "D1", "to": []string{"bob@example.net"},
		"subject": "s", "body": "b2"})
	if err != nil {
		t.Fatal(err)
	}
	if o := res.Object(); res.IsError || o["emailId"] != "D2" || o["replacedEmailId"] != "D1" {
		t.Fatalf("draft_update: %+v", res)
	}
	var sent map[string]any
	_ = json.Unmarshal(f.calls[len(f.calls)-1].Args, &sent)
	d, _ := sent["create"].(map[string]any)["d"].(map[string]any)
	if d["replacesEmailId"] != "D1" {
		t.Fatalf("draft_update must send ONE create naming replacesEmailId: %v", sent)
	}

	res, err = s.Call(ctx, "draft_discard", map[string]any{"emailId": "D1"})
	if err != nil {
		t.Fatal(err)
	}
	if o := res.Object(); res.IsError || o["status"] != "discarded" {
		t.Fatalf("draft_discard: %+v", res)
	}
	_ = json.Unmarshal(f.calls[len(f.calls)-1].Args, &sent)
	if ids, _ := sent["discardDrafts"].([]any); len(ids) != 1 || ids[0] != "D1" || sent["destroy"] != nil {
		t.Fatalf("draft_discard must use discardDrafts, never destroy: %v", sent)
	}
}

// ---- Markdown -----------------------------------------------------------------

func TestRenderMarkdown(t *testing.T) {
	for _, c := range []struct {
		name, in  string
		want, not []string
	}{
		{"bold", "**bold** and ~~gone~~", []string{"<strong>bold</strong>", "<del>gone</del>"}, nil},
		{"plain lines keep their breaks", "Hi Bob,\nThursday works.\nSee you", []string{"Hi Bob,<br>", "Thursday works.<br>"}, nil},
		{"paragraphs", "one\n\ntwo", []string{"<p>one</p>", "<p>two</p>"}, nil},
		{"script block is text", "<script>alert(1)</script>", []string{"&lt;script&gt;"}, []string{"<script"}},
		{"inline html is text", "Hi <b onclick=\"x()\">Bob</b>", []string{"&lt;b onclick="}, []string{"<b "}},
		{"javascript link", "[x](javascript:alert(1))", []string{"x"}, []string{"href", "javascript"}},
		{"obfuscated scheme", "[x](java&#115;cript:alert(1))", nil, []string{"href", "javascript:"}},
		{"entity first letter", "[x](&#106;avascript:alert(1))", nil, []string{"href", "javascript:"}},
		{"named entity colon", "[x](javascript&colon;alert(1))", nil, []string{"href", "javascript:"}},
		{"escaped colon", `[x](javascript\:alert(1))`, nil, []string{"href", "javascript:"}},
		{"reference link", "[x][r]\n\n[r]: java&#115;cript:alert(1)", nil, []string{"href", "javascript:"}},
		{"data link", "[x](data:text/html,hi)", nil, []string{"href"}},
		{"relative link", "[x](/settings)", nil, []string{"href"}},
		{"autolink scheme", "<javascript:alert(1)>", nil, []string{"href"}},
		{"http link", "[site](https://example.com/a)", []string{`<a href="https://example.com/a">site</a>`}, nil},
		{"mailto link", "[me](mailto:you@example.com)", []string{`href="mailto:you@example.com"`}, nil},
		{"bare url", "see https://example.com", []string{`<a href="https://example.com">`}, nil},
		{"email autolink", "<you@example.com>", []string{`href="mailto:you@example.com"`}, nil},
		{"image is its alt text", "![a cat](https://example.com/cat.png)", []string{"a cat"}, []string{"<img", "cat.png"}},
		{"linked image", "[![logo](https://example.com/l.png)](https://example.com)", []string{"logo"}, []string{"<img"}},
		{"table", "| a | b |\n|---|---|\n| 1 | 2 |", []string{"<table>", "<td>1</td>"}, nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			got := renderMarkdown(c.in)
			for _, w := range c.want {
				if !strings.Contains(got, w) {
					t.Errorf("render(%q) = %q, want it to contain %q", c.in, got, w)
				}
			}
			for _, n := range c.not {
				if strings.Contains(strings.ToLower(got), strings.ToLower(n)) {
					t.Errorf("render(%q) = %q, must not contain %q", c.in, got, n)
				}
			}
		})
	}
	if renderMarkdown("  \n ") != "" {
		t.Error("a blank body must stay plain text")
	}
}

// draft_new sends the body twice: text/plain exactly as written, and the
// rendered HTML, as one create.
func TestDraftBodyIsTextPlusRenderedHTML(t *testing.T) {
	f := draftBackend(SendNone, false)
	s := connect(t, f)
	body := "Hi **Bob**,\nsee [this](javascript:alert(1)) <script>x</script>\n"
	if _, err := s.Call(context.Background(), "draft_new", map[string]any{"to": []string{"bob@example.net"},
		"subject": "s", "body": body}); err != nil {
		t.Fatal(err)
	}
	var sent struct {
		Create map[string]struct {
			TextBody   []map[string]string          `json:"textBody"`
			HTMLBody   []map[string]string          `json:"htmlBody"`
			BodyValues map[string]map[string]string `json:"bodyValues"`
		} `json:"create"`
	}
	if err := json.Unmarshal(f.calls[len(f.calls)-1].Args, &sent); err != nil {
		t.Fatal(err)
	}
	d := sent.Create["d"]
	if len(d.TextBody) != 1 || len(d.HTMLBody) != 1 {
		t.Fatalf("want one text and one html part: %+v", d)
	}
	text := d.BodyValues[d.TextBody[0]["partId"]]["value"]
	html := d.BodyValues[d.HTMLBody[0]["partId"]]["value"]
	if text != body {
		t.Errorf("text/plain = %q, want the body exactly %q", text, body)
	}
	if !strings.Contains(html, "<strong>Bob</strong>") || strings.Contains(html, "javascript") ||
		strings.Contains(html, "<script") {
		t.Errorf("text/html = %q", html)
	}
}
