// SPDX-License-Identifier: AGPL-3.0-only

package ingest

import (
	"reflect"
	"testing"

	"mailroom/internal/mime"
	"mailroom/internal/store"
)

// mustParseForTest parses a raw RFC 5322 message for a test and fails fast on
// a parse error — every fixture below is well-formed, so an error means the
// fixture itself is wrong, not the code under test.
func mustParseForTest(t *testing.T, raw string) *mime.Message {
	t.Helper()
	m, err := mime.ParseBytes([]byte(raw))
	if err != nil {
		t.Fatalf("parse fixture: %v", err)
	}
	return m
}

// TestFunctionSignalsFromHeader covers the #156 extraction (migration 00027)
// against the header shapes real mail actually presents.
func TestFunctionSignalsFromHeader(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want store.FunctionSignals
	}{
		{
			name: "no such headers at all",
			raw: "From: alice@example.com\r\n" +
				"Subject: hi\r\n" +
				"\r\n" +
				"body\r\n",
			want: store.FunctionSignals{},
		},
		{
			name: "headers present but empty/whitespace-only",
			raw: "From: alice@example.com\r\n" +
				"Precedence: \r\n" +
				"Auto-Submitted:    \r\n" +
				"Subject: hi\r\n" +
				"\r\n" +
				"body\r\n",
			want: store.FunctionSignals{},
		},
		{
			name: "one-click unsubscribe, auto-submitted, precedence and feedback-id present",
			raw: "From: alice@example.com\r\n" +
				"List-Unsubscribe-Post: One-Click\r\n" +
				"Auto-Submitted: auto-generated\r\n" +
				"Precedence: bulk\r\n" +
				"Feedback-ID: 1:2:3:SES\r\n" +
				"Return-Path: <bounce+abc123@bounces.example.com>\r\n" +
				"Subject: hi\r\n" +
				"\r\n" +
				"body\r\n",
			want: store.FunctionSignals{
				ListUnsubscribePost: "One-Click",
				AutoSubmitted:       "auto-generated",
				Precedence:          "bulk",
				FeedbackID:          "1:2:3:SES",
				ReturnPath:          "<bounce+abc123@bounces.example.com>",
			},
		},
		{
			name: "Feedback-ID falls back to X-Feedback-ID when Feedback-ID is absent",
			raw: "From: alice@example.com\r\n" +
				"X-Feedback-ID: campaign-42:sendgrid\r\n" +
				"Subject: hi\r\n" +
				"\r\n" +
				"body\r\n",
			want: store.FunctionSignals{FeedbackID: "campaign-42:sendgrid"},
		},
		{
			name: "Feedback-ID present takes priority over X-Feedback-ID",
			raw: "From: alice@example.com\r\n" +
				"Feedback-ID: real:one\r\n" +
				"X-Feedback-ID: ignored:two\r\n" +
				"Subject: hi\r\n" +
				"\r\n" +
				"body\r\n",
			want: store.FunctionSignals{FeedbackID: "real:one"},
		},
		{
			name: "a single folded DKIM-Signature header unfolds to one value and yields one domain",
			raw: "From: alice@example.com\r\n" +
				"DKIM-Signature: v=1; a=rsa-sha256; c=relaxed/relaxed; d=example.com;\r\n" +
				" s=selector1; h=from:to:subject; bh=abc123; b=def456\r\n" +
				"Subject: hi\r\n" +
				"\r\n" +
				"body\r\n",
			want: store.FunctionSignals{DKIMDomains: []string{"example.com"}},
		},
		{
			name: "multiple DKIM-Signature headers yield one domain each, in order, deduplicated",
			raw: "From: alice@example.com\r\n" +
				"DKIM-Signature: v=1; a=rsa-sha256; d=esp.example; s=s1; b=aaa\r\n" +
				"DKIM-Signature: v=1; a=rsa-sha256; d=brand.example; s=s2; b=bbb\r\n" +
				"DKIM-Signature: v=1; a=rsa-sha256; d=esp.example; s=s3; b=ccc\r\n" +
				"Subject: hi\r\n" +
				"\r\n" +
				"body\r\n",
			want: store.FunctionSignals{DKIMDomains: []string{"esp.example", "brand.example"}},
		},
		{
			name: "a header appearing twice (not DKIM) takes the FIRST occurrence, matching Header.Get",
			raw: "From: alice@example.com\r\n" +
				"Precedence: bulk\r\n" +
				"Precedence: list\r\n" +
				"Return-Path: <first@example.com>\r\n" +
				"Return-Path: <second@example.com>\r\n" +
				"Subject: hi\r\n" +
				"\r\n" +
				"body\r\n",
			want: store.FunctionSignals{
				Precedence: "bulk",
				ReturnPath: "<first@example.com>",
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m := mustParseForTest(t, tc.raw)
			got := FunctionSignalsFromHeader(m.Header)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("FunctionSignalsFromHeader() = %#v, want %#v", got, tc.want)
			}
		})
	}
}

// TestProjectionFromCarriesFunctionSignals confirms projectionFrom — the
// function ingestNew actually calls — wires FunctionSignalsFromHeader's result
// onto the projection's flat fields rather than dropping or partially copying
// them.
func TestProjectionFromCarriesFunctionSignals(t *testing.T) {
	raw := "From: alice@example.com\r\n" +
		"List-Unsubscribe-Post: One-Click\r\n" +
		"Auto-Submitted: auto-generated\r\n" +
		"Precedence: bulk\r\n" +
		"Feedback-ID: 1:2:3:SES\r\n" +
		"Return-Path: <bounce@example.com>\r\n" +
		"DKIM-Signature: v=1; a=rsa-sha256; d=example.com; s=s1; b=aaa\r\n" +
		"Subject: hi\r\n" +
		"\r\n" +
		"body\r\n"
	m := mustParseForTest(t, raw)
	p := projectionFrom(m)

	want := projection{
		ListUnsubscribePost: "One-Click",
		AutoSubmitted:       "auto-generated",
		Precedence:          "bulk",
		FeedbackID:          "1:2:3:SES",
		DKIMDomains:         []string{"example.com"},
		ReturnPath:          "<bounce@example.com>",
	}
	if p.ListUnsubscribePost != want.ListUnsubscribePost ||
		p.AutoSubmitted != want.AutoSubmitted ||
		p.Precedence != want.Precedence ||
		p.FeedbackID != want.FeedbackID ||
		p.ReturnPath != want.ReturnPath ||
		!reflect.DeepEqual(p.DKIMDomains, want.DKIMDomains) {
		t.Errorf("projectionFrom() function signals = %+v, want %+v",
			struct {
				ListUnsubscribePost, AutoSubmitted, Precedence, FeedbackID, ReturnPath string
				DKIMDomains                                                            []string
			}{p.ListUnsubscribePost, p.AutoSubmitted, p.Precedence, p.FeedbackID, p.ReturnPath, p.DKIMDomains},
			want)
	}
}

func TestProjectionFromNilMessageHasNoFunctionSignals(t *testing.T) {
	p := projectionFrom(nil)
	if p.DKIMDomains != nil || p.ListUnsubscribePost != "" || p.ReturnPath != "" {
		t.Errorf("projectionFrom(nil) function signals = %+v, want zero value", p)
	}
}
