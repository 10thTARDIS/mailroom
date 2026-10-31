// SPDX-License-Identifier: AGPL-3.0-only

package mcp

import (
	"context"
	"encoding/json"
	"go/parser"
	"go/token"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"mailroom/internal/mcp/mcptest"
)

// THE TOOL NAMES ARE A PUBLIC CONTRACT (clients and people's prompts name
// them). This list changes only on purpose.
var publicToolNames = []string{
	"apply_label", "archive", "delete_message", "draft_discard", "draft_new", "draft_reply", "draft_update",
	"find_contacts", "get_contact",
	"get_pending", "list_areas", "list_labels", "list_mailboxes", "mark_read", "mark_unread", "move_to_area",
	"move_to_mailbox", "read_message", "remove_label", "search_mail", "send_draft",
}

func TestToolNamesAreTheContract(t *testing.T) {
	var got []string
	for _, ti := range Catalog() {
		got = append(got, ti.Name)
	}
	slices.Sort(got)
	if !slices.Equal(got, publicToolNames) {
		t.Fatalf("tool names changed:\n got %v\nwant %v", got, publicToolNames)
	}
}

func TestEveryToolIsWellFormed(t *testing.T) {
	name := regexp.MustCompile(`^[a-z][a-z_]*$`)
	for _, tl := range catalog() {
		if !name.MatchString(tl.name) {
			t.Errorf("%s: not a snake_case name", tl.name)
		}
		if len(tl.uses) == 0 {
			t.Errorf("%s: declares no JMAP use, so tools/list cannot decide on it", tl.name)
		}
		if tl.input["type"] != "object" || tl.input["additionalProperties"] != false {
			t.Errorf("%s: input schema must be a closed object: %v", tl.name, tl.input)
		}
		if tl.description == "" || tl.title == "" {
			t.Errorf("%s: missing description or title", tl.name)
		}
		// Arrays and strings in the schema are bounded.
		props, _ := tl.input["properties"].(map[string]any)
		for p, v := range props {
			s, _ := v.(map[string]any)
			switch s["type"] {
			case "string":
				if _, ok := s["maxLength"]; !ok {
					t.Errorf("%s.%s: unbounded string", tl.name, p)
				}
			case "array":
				if _, ok := s["maxItems"]; !ok {
					t.Errorf("%s.%s: unbounded array", tl.name, p)
				}
			case "integer":
				if _, ok := s["maximum"]; !ok {
					t.Errorf("%s.%s: unbounded integer", tl.name, p)
				}
			}
		}
		// Every tool that returns message content says it is data.
		if slices.ContainsFunc(tl.uses, func(u Use) bool { return u.Method == "Email/get" || u.Method == "Contact/get" }) &&
			tl.readOnly && !strings.Contains(tl.description, "untrusted_content") {
			t.Errorf("%s returns third-party content but its description does not say so", tl.name)
		}
	}
}

// THE ANTI-CORRUPTION LAYER, HELD IN PLACE: only internal/mcp (and its test
// helper mcptest) may import the MCP SDK.
func TestOnlyThisPackageImportsTheSDK(t *testing.T) {
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("repository root not found at %s", root)
	}
	checked := 0
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && (d.Name() == "node_modules" || d.Name() == ".git" || d.Name() == "web") {
			return filepath.SkipDir
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") {
			return nil
		}
		f, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
		if err != nil {
			return err
		}
		checked++
		rel, _ := filepath.Rel(root, path)
		rel = filepath.ToSlash(rel)
		for _, imp := range f.Imports {
			p, _ := strconv.Unquote(imp.Path.Value)
			if strings.HasPrefix(p, "github.com/modelcontextprotocol/") && !strings.HasPrefix(rel, "internal/mcp/") {
				t.Errorf("%s imports %s: only internal/mcp may import the MCP SDK", rel, p)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if checked < 100 {
		t.Fatalf("walked only %d Go files: the guard is not looking at the repository", checked)
	}
}

func TestCleanText(t *testing.T) {
	hostile := "Pay\u202Eexe.txt\u202C now\u200B\u200C\u200D\u2060\uFEFF\u00AD\u2066x\u2069\x00\x07\x1b[31m\r\nline2\tok\u2028end\U000E0041\u3164\uFE0F\u034F\U000E0100\u115F\uFFA0\u17B4\u180B"
	got := cleanText(hostile)
	want := "Payexe.txt nowx[31m\nline2\tok\nend"
	if got != want {
		t.Fatalf("cleanText = %q, want %q", got, want)
	}
	for _, r := range got {
		if isBidiControl(r) || (r < 0x20 && r != '\n' && r != '\t') {
			t.Fatalf("cleanText left %U", r)
		}
	}
	if cleanText("plain café, 日本語 ✓") != "plain café, 日本語 ✓" {
		t.Fatal("cleanText changed ordinary text")
	}
}

func TestHTMLText(t *testing.T) {
	in := `<html><head><title>T</title><style>.x{color:red}</style></head><body>
	<p>Hello <b>there</b>,</p><div>second   line</div><ul><li>one</li><li>two</li></ul>
	<blockquote>quoted text</blockquote><script>alert(1)</script>a<br>b</body></html>`
	got := htmlText(in)
	want := "Hello there,\n\nsecond line\n\n- one\n- two\n\nquoted text\na\nb"
	if got != want {
		t.Fatalf("htmlText:\n got %q\nwant %q", got, want)
	}
}

// Text a person cannot see is dropped with its whole subtree (#853 review),
// and the visible text beside it survives (the positive control: a converter
// that dropped everything would pass the first half).
func TestHTMLTextDropsHiddenText(t *testing.T) {
	hidden := []string{
		`<div hidden>H1</div>`,
		`<span aria-hidden="TRUE">H2</span>`,
		`<div style="DISPLAY : none !important">H3 <b>nested</b></div>`,
		`<p style="visibility:hidden">H4</p>`,
		`<p style="color:red; opacity: 0">H5</p>`,
		`<p style="opacity:0.0">H6</p>`,
		`<p style="opacity:.0">H7</p>`,
		`<span style="font-size:0px">H8</span>`,
		`<span style="font-size: 0em">H9</span>`,
		`<span style="font-size:0%">H10</span>`,
		`<span style="font-size:0">H11</span>`,
		`<div style="max-height:0;overflow:hidden">H12</div>`,
		`<div style="width: 0px; overflow: hidden">H13</div>`,
		`<span style="color:transparent">H14</span>`,
		`<span style="color: rgba(0, 0, 0, 0)">H15</span>`,
		`<span style="visibility:collapse">H16</span>`,
	}
	in := "<p>Visible start.</p>" + strings.Join(hidden, "") +
		`<p style="opacity:0.5;font-size:10px;max-height:0">Faint but shown.</p><p>Visible end.</p>`
	got := htmlText(in)
	if regexp.MustCompile(`H[0-9]+`).MatchString(got) {
		t.Errorf("hidden text leaked: %q", got)
	}
	if want := "Visible start.\n\nFaint but shown.\n\nVisible end."; got != want {
		t.Fatalf("htmlText = %q, want %q", got, want)
	}
}

func TestCapUTF8(t *testing.T) {
	s := strings.Repeat("é", 10) // 20 bytes
	got, cut := capUTF8(s, 5)
	if !cut || got != "éé" {
		t.Fatalf("capUTF8 = %q, %v", got, cut)
	}
	if got, cut := capUTF8("abc", 5); cut || got != "abc" {
		t.Fatalf("capUTF8 short = %q, %v", got, cut)
	}
}

// ---- the handler, driven by the SDK's client against a fake backend --------

type fakeBackend struct {
	mu      sync.Mutex
	allowed map[string]bool // methods the "key" may use
	lacking []string        // what KeyLacks reports for a use not allowed (default mail:read)
	mode    SendMode
	calls   []Invocation
	answer  func(inv Invocation) Invocation
}

func (f *fakeBackend) Call(_ context.Context, calls []Invocation) ([]Invocation, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []Invocation
	for _, c := range calls {
		f.calls = append(f.calls, c)
		out = append(out, f.answer(c))
	}
	return out, nil
}

func (f *fakeBackend) KeyMayCall(_ context.Context, uses ...Use) bool {
	for _, u := range uses {
		if !f.allowed[u.Method] {
			return false
		}
	}
	return true
}

func (f *fakeBackend) KeyLacks(ctx context.Context, uses ...Use) []string {
	if f.KeyMayCall(ctx, uses...) {
		return nil
	}
	if f.lacking != nil {
		return f.lacking
	}
	return []string{"mail:read"}
}

func (f *fakeBackend) SendMode(context.Context) SendMode { return f.mode }

func inv(name, callID string, args any) Invocation {
	raw, _ := json.Marshal(args)
	return Invocation{Name: name, Args: raw, CallID: callID}
}

func connect(t *testing.T, b Backend) *mcptest.Session {
	t.Helper()
	srv := httptest.NewServer(NewHandler(b, Options{Version: "test"}))
	t.Cleanup(srv.Close)
	s, err := mcptest.Connect(context.Background(), srv.URL, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestToolsListFollowsKeyMayCall(t *testing.T) {
	f := &fakeBackend{allowed: map[string]bool{"Mailbox/get": true, "Label/get": true}}
	s := connect(t, f)
	got, err := s.ToolNames(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"list_labels", "list_mailboxes"}; !slices.Equal(got, want) {
		t.Fatalf("tools/list = %v, want %v", got, want)
	}
}

func TestMethodErrorIsAToolErrorAndPendingIsAResult(t *testing.T) {
	f := &fakeBackend{allowed: map[string]bool{"Mailbox/get": true, "Email/set": true, "PendingAction/get": true}}
	f.answer = func(c Invocation) Invocation {
		switch c.Name {
		case "Mailbox/get":
			return inv("error", c.CallID, map[string]any{"type": "overQuota", "description": "daily budget spent"})
		case "Email/set":
			return inv("Email/set", c.CallID, map[string]any{"notDestroyed": map[string]any{
				"m1": map[string]any{"type": "pendingApproval", "pendingActionId": "p1"}}})
		case "PendingAction/get":
			return inv("PendingAction/get", c.CallID, map[string]any{"list": []any{map[string]any{
				"id": "p1", "expiresAt": "2026-10-25T00:00:00Z", "summary": "Move 1 message to Trash\u202E"}}})
		}
		return inv("error", c.CallID, map[string]any{"type": "unknownMethod"})
	}
	s := connect(t, f)
	ctx := context.Background()

	// A method error is a tool error carrying the JMAP type.
	res, err := s.Call(ctx, "list_mailboxes", map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	e := res.Object()["error"].(map[string]any)
	if !res.IsError || e["type"] != "overQuota" || !strings.Contains(res.Text, "daily budget spent") {
		t.Fatalf("a method error must be a tool error with its JMAP type: %+v", res)
	}

	res, err = s.Call(ctx, "delete_message", map[string]any{"emailIds": []string{"m1"}})
	if err != nil {
		t.Fatal(err)
	}
	o := res.Object()
	if res.IsError || o["status"] != "pending_approval" || o["pendingId"] != "p1" || o["expiresAt"] != "2026-10-25T00:00:00Z" {
		t.Fatalf("a hold must be a normal pending_approval result: %+v", res)
	}
	if strings.ContainsRune(string(res.Structured), '\u202E') || strings.ContainsRune(res.Text, '\u202E') {
		t.Fatal("the summary's bidi override reached the model")
	}

	// Bounds are the schema's: 51 ids is refused before the backend sees it.
	before := len(f.calls)
	ids := make([]string, maxBulkIDs+1)
	for i := range ids {
		ids[i] = "m" + strconv.Itoa(i)
	}
	res, err = s.Call(ctx, "mark_read", map[string]any{"emailIds": ids})
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError || len(f.calls) != before {
		t.Fatalf("an over-long id list must be refused by the schema without a call: %+v (%d calls)", res, len(f.calls)-before)
	}
}

func TestStatelessTransport(t *testing.T) {
	srv := httptest.NewServer(NewHandler(&fakeBackend{}, Options{}))
	defer srv.Close()
	for _, m := range []string{http.MethodGet, http.MethodDelete} {
		req, _ := http.NewRequest(m, srv.URL, nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusMethodNotAllowed {
			t.Errorf("%s: %d, want 405 (stateless: no SSE stream, no session to delete)", m, resp.StatusCode)
		}
		if resp.Header.Get("Mcp-Session-Id") != "" {
			t.Errorf("%s issued a session id", m)
		}
	}
}
