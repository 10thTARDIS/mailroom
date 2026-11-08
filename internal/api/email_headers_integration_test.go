// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package api

// Email/get header properties through the real blob store (ADR-052, #114).

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/store"
)

const headerFixture = "Received: from mx1.example.net\r\n by mx2.example.net\r\n" +
	"Received: from client.example.net\r\n" +
	"Subject: =?UTF-8?Q?Caf=C3=A9?= au lait\r\n" +
	"From: \"Alice Example\" <alice@example.com>\r\n" +
	"To: Team: bob@example.com, \"Dave D\" <dave@example.com>;\r\n" +
	"To: late@example.com\r\n" +
	"Date: Fri, 21 Nov 1997 09:55:06 -0600\r\n" +
	"Message-ID: <hdr-1@example.com>\r\n" +
	"List-Unsubscribe: <mailto:u@example.com>, <https://example.com/u>\r\n" +
	"MIME-Version: 1.0\r\nContent-Type: text/plain; charset=utf-8\r\n\r\nbody text\r\n"

func getHeaderProps(t *testing.T, h *composeHarness, userID ulid.ULID, ids []string, props ...string) (map[string]any, *MethodError) {
	t.Helper()
	raw, mErr := h.api.emailGet(context.Background(), userID,
		mustJSON(t, map[string]any{"ids": ids, "properties": append([]string{"id"}, props...)}))
	if mErr != nil {
		return nil, mErr
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	return out, nil
}

func TestEmailGetHeaderPropertiesFromTheStoredMessage(t *testing.T) {
	h := newComposeHarness(t)
	a, b := seedTwo(t, h.super)
	id := seedRawMessage(t, h, a, []byte(headerFixture))

	res, mErr := getHeaderProps(t, h, a.UserID, []string{id.String()},
		"header:Subject:asText", "header:Received:all", "header:From:asAddresses", "header:To:asGroupedAddresses:all",
		"header:Date:asDate", "header:Message-ID:asMessageIds", "header:List-Unsubscribe:asURLs", "header:X-Absent:asText",
		"headers")
	if mErr != nil {
		t.Fatalf("method error: %+v", mErr)
	}
	obj := res["list"].([]any)[0].(map[string]any)
	if obj["header:Subject:asText"] != "Café au lait" {
		t.Errorf("subject = %v", obj["header:Subject:asText"])
	}
	if recv := obj["header:Received:all"].([]any); len(recv) != 2 || !strings.Contains(recv[0].(string), "mx1") ||
		!strings.Contains(recv[0].(string), "\r\n by mx2") {
		t.Errorf("Received:all = %#v (order, folding)", recv)
	}
	if from := obj["header:From:asAddresses"].([]any); len(from) != 1 || from[0].(map[string]any)["name"] != "Alice Example" {
		t.Errorf("from = %#v", from)
	}
	if g := obj["header:To:asGroupedAddresses:all"].([]any); len(g) != 2 {
		t.Errorf("To grouped:all = %#v", g)
	}
	if obj["header:Date:asDate"] != "1997-11-21T09:55:06-06:00" {
		t.Errorf("date = %v", obj["header:Date:asDate"])
	}
	if ids := obj["header:Message-ID:asMessageIds"].([]any); len(ids) != 1 || ids[0] != "hdr-1@example.com" {
		t.Errorf("message-id = %#v", ids)
	}
	if urls := obj["header:List-Unsubscribe:asURLs"].([]any); len(urls) != 2 {
		t.Errorf("urls = %#v", urls)
	}
	if v, present := obj["header:X-Absent:asText"]; !present || v != nil {
		t.Errorf("absent header = %v (present=%v), want null", v, present)
	}
	if hs := obj["headers"].([]any); len(hs) != 11 {
		t.Errorf("headers has %d fields: %v", len(hs), hs)
	}
	if obj["headersTruncated"] != false {
		t.Errorf("headersTruncated = %v", obj["headersTruncated"])
	}

	// Another user's message id answers notFound, header properties or not.
	res, mErr = getHeaderProps(t, h, b.UserID, []string{id.String()}, "header:Subject")
	if mErr != nil {
		t.Fatalf("method error: %+v", mErr)
	}
	if n := len(res["list"].([]any)); n != 0 {
		t.Errorf("another user read A's headers (%d objects)", n)
	}
}

func TestEmailGetRefusesUnknownAndMalformedProperties(t *testing.T) {
	h := newComposeHarness(t)
	a, _ := seedTwo(t, h.super)
	id := seedRawMessage(t, h, a, []byte(headerFixture))
	for _, p := range []string{"notAProperty", "header:From:asDate", "header:", "header:Subject:asBogus", "headers:all"} {
		_, mErr := getHeaderProps(t, h, a.UserID, []string{id.String()}, "subject", p)
		if mErr == nil || mErr.Type != errInvalidArguments || !strings.Contains(mErr.Description, strings.Split(p, ":")[0]) {
			t.Errorf("%q: got %+v, want invalidArguments naming it", p, mErr)
		}
	}
}

func TestEmailGetHeaderBoundsPerMessageAndPerRequest(t *testing.T) {
	h := newComposeHarness(t)
	a, _ := seedTwo(t, h.super)

	// A header block several times the cap: 5,000 fields of ~100 bytes.
	var sb strings.Builder
	sb.WriteString("Subject: kept\r\n")
	for i := 0; i < 5000; i++ {
		fmt.Fprintf(&sb, "X-Pad-%d: %s\r\n", i, strings.Repeat("p", 90))
	}
	sb.WriteString("\r\nbody\r\n")
	if sb.Len() < 3*maxHeaderBlockBytes {
		t.Fatalf("fixture is only %d bytes", sb.Len())
	}
	id := seedRawMessage(t, h, a, []byte(sb.String()))
	res, mErr := getHeaderProps(t, h, a.UserID, []string{id.String()}, "headers", "header:Subject:asText")
	if mErr != nil {
		t.Fatalf("method error: %+v", mErr)
	}
	obj := res["list"].([]any)[0].(map[string]any)
	if obj["headersTruncated"] != true {
		t.Errorf("headersTruncated = %v, want true", obj["headersTruncated"])
	}
	if obj["header:Subject:asText"] != "kept" {
		t.Errorf("a field inside the cap was lost: %v", obj["header:Subject:asText"])
	}
	enc, _ := json.Marshal(obj["headers"])
	if len(enc) > maxHeaderBlockBytes*2 {
		t.Errorf("headers answer is %d bytes for a %d byte cap", len(enc), maxHeaderBlockBytes)
	}
	if n := len(obj["headers"].([]any)); n >= 5000 || n < 100 {
		t.Errorf("headers has %d fields; want a bounded, non-empty prefix", n)
	}

	// The request bound: one id over the cap is requestTooLarge, and it is
	// refused before any blob is read.
	counting := &countingRaw{inner: h.api.raw}
	h.api.raw = counting
	ids := make([]string, maxHeaderPropertyIDs+1)
	for i := range ids {
		ids[i] = store.NewULID().String()
	}
	if _, mErr := getHeaderProps(t, h, a.UserID, ids, "header:Subject"); mErr == nil || mErr.Type != errRequestTooLarge {
		t.Errorf("over the id bound: %+v, want requestTooLarge", mErr)
	}
	if n := counting.reads.Load(); n != 0 {
		t.Errorf("%d blob reads before the refusal", n)
	}
	// At the bound it is accepted (ids that do not exist are notFound).
	if res, mErr := getHeaderProps(t, h, a.UserID, ids[:maxHeaderPropertyIDs], "header:Subject"); mErr != nil {
		t.Errorf("at the bound: %+v", mErr)
	} else if n := len(res["notFound"].([]any)); n != maxHeaderPropertyIDs {
		t.Errorf("notFound = %d", n)
	}
	// Without header properties the bound does not apply.
	if _, mErr := getHeaderProps(t, h, a.UserID, ids, "subject"); mErr != nil {
		t.Errorf("a list-view call was bounded like a header call: %+v", mErr)
	}
}

// A header-only call reads each message once and does not walk its MIME body.
func TestEmailGetHeaderPropertiesReadOneBlobPerMessage(t *testing.T) {
	h := newComposeHarness(t)
	a, _ := seedTwo(t, h.super)
	id := seedRawMessage(t, h, a, []byte(headerFixture))
	counting := &countingRaw{inner: h.api.raw}
	h.api.raw = counting
	if _, mErr := getHeaderProps(t, h, a.UserID, []string{id.String()}, "header:Subject", "header:From:asAddresses", "headers"); mErr != nil {
		t.Fatal(mErr)
	}
	if n := counting.reads.Load(); n != 1 {
		t.Errorf("%d blob reads for one message, want 1", n)
	}
}

// Header properties need exactly mail:read, as bodies do (ADR-052 §Scope).
func TestEmailGetHeaderPropertiesNeedMailRead(t *testing.T) {
	h := newAgentHarness(t)
	a, _ := seedTwo(t, h.super)
	id := seedRawMessage(t, h.composeHarness, a, []byte(headerFixture))
	args := map[string]any{"ids": []string{id.String()}, "properties": []string{"id", "header:Subject:asText"}}

	reader, _ := h.newKey(t, a.UserID, map[string]any{"name": "reader", "scopes": []string{"mail:read"}})
	name, res := h.keyCall(t, reader, "Email/get", args)
	if name != "Email/get" {
		t.Fatalf("mail:read key: %s %v", name, res)
	}
	if list := res["list"].([]any); len(list) != 1 || list[0].(map[string]any)["header:Subject:asText"] != "Café au lait" {
		t.Errorf("mail:read key got %v", res)
	}

	// Negative control: a key WITHOUT mail:read is refused the same call.
	other, _ := h.newKey(t, a.UserID, map[string]any{"name": "labels", "scopes": []string{"labels:read"}})
	name, res = h.keyCall(t, other, "Email/get", args)
	wantForbidden(t, "key without mail:read", name, res)
}
