// SPDX-License-Identifier: AGPL-3.0-only

// Package mcptest drives an MCP endpoint with the MCP SDK's own client, for
// tests outside internal/mcp (which is the only package that imports the SDK).
// Its surface is plain Go: tool names, and results as JSON.
package mcptest

import (
	"context"
	"encoding/json"
	"net/http"
	"sort"
	"strings"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// Session is a connected MCP client session.
type Session struct{ s *sdk.ClientSession }

// bearer adds the API key to every request the SDK client makes.
type bearer struct {
	token string
	next  http.RoundTripper
}

func (b bearer) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+b.token)
	return b.next.RoundTrip(r)
}

// Connect initializes a session with the endpoint at url, authenticating with
// token (none when empty). base is the HTTP client to use (nil: default).
func Connect(ctx context.Context, url, token string, base *http.Client) (*Session, error) {
	hc := &http.Client{Transport: http.DefaultTransport}
	if base != nil {
		c := *base
		hc = &c
		if hc.Transport == nil {
			hc.Transport = http.DefaultTransport
		}
	}
	if token != "" {
		hc.Transport = bearer{token: token, next: hc.Transport}
	}
	client := sdk.NewClient(&sdk.Implementation{Name: "mailroom-mcptest", Version: "test"}, nil)
	s, err := client.Connect(ctx, &sdk.StreamableClientTransport{Endpoint: url, HTTPClient: hc, MaxRetries: -1}, nil)
	if err != nil {
		return nil, err
	}
	return &Session{s: s}, nil
}

// Close ends the session.
func (s *Session) Close() error { return s.s.Close() }

// ToolNames is tools/list's names, sorted.
func (s *Session) ToolNames(ctx context.Context) ([]string, error) {
	res, err := s.s.ListTools(ctx, nil)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, t := range res.Tools {
		out = append(out, t.Name)
	}
	sort.Strings(out)
	return out, nil
}

// Tool is one listed tool's public description.
type Tool struct {
	Name        string
	Description string
	InputSchema json.RawMessage
}

// Tools is tools/list's full entries.
func (s *Session) Tools(ctx context.Context) ([]Tool, error) {
	res, err := s.s.ListTools(ctx, nil)
	if err != nil {
		return nil, err
	}
	var out []Tool
	for _, t := range res.Tools {
		schema, _ := json.Marshal(t.InputSchema)
		out = append(out, Tool{Name: t.Name, Description: t.Description, InputSchema: schema})
	}
	return out, nil
}

// Result is one tools/call result.
type Result struct {
	IsError bool
	// Structured is structuredContent as JSON ("null" when absent).
	Structured json.RawMessage
	// Text is every text content block, joined by newlines.
	Text string
}

// Object is Structured decoded as a JSON object.
func (r Result) Object() map[string]any {
	var m map[string]any
	_ = json.Unmarshal(r.Structured, &m)
	return m
}

// Call calls a tool. A protocol-level failure is an error; a tool error is a
// Result with IsError set.
func (s *Session) Call(ctx context.Context, name string, args any) (Result, error) {
	res, err := s.s.CallTool(ctx, &sdk.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		return Result{}, err
	}
	structured, _ := json.Marshal(res.StructuredContent)
	var texts []string
	for _, c := range res.Content {
		if t, ok := c.(*sdk.TextContent); ok {
			texts = append(texts, t.Text)
		}
	}
	return Result{IsError: res.IsError, Structured: structured, Text: strings.Join(texts, "\n")}, nil
}
