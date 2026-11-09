// SPDX-License-Identifier: AGPL-3.0-only

package api

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"mailroom/internal/mime"
)

func TestParseHeaderProperty(t *testing.T) {
	ok := map[string]headerSpec{
		"header:Subject":                 {prop: "header:Subject", lname: "subject", form: formRaw},
		"header:subject:asText":          {prop: "header:subject:asText", lname: "subject", form: formText},
		"header:To:asAddresses:all":      {prop: "header:To:asAddresses:all", lname: "to", form: formAddr, all: true},
		"header:Received:all":            {prop: "header:Received:all", lname: "received", form: formRaw, all: true},
		"header:References:asMessageIds": {prop: "header:References:asMessageIds", lname: "references", form: formMsgIDs},
		"header:List-Post:asURLs":        {prop: "header:List-Post:asURLs", lname: "list-post", form: formURLs},
		"header:Resent-Date:asDate":      {prop: "header:Resent-Date:asDate", lname: "resent-date", form: formDate},
		"header:Cc:asGroupedAddresses":   {prop: "header:Cc:asGroupedAddresses", lname: "cc", form: formGrouped},
		"header:X-Anything:asRaw":        {prop: "header:X-Anything:asRaw", lname: "x-anything", form: formRaw},
	}
	for p, want := range ok {
		got, why := parseHeaderProperty(p)
		if why != "" || got != want {
			t.Errorf("%s: got %+v %q, want %+v", p, got, why, want)
		}
	}
	bad := []string{
		"header:",                       // empty name
		"header",                        // no name
		"header:Subject:asNonsense",     // unknown form
		"header:Subject:all:asText",     // all must be last
		"header:Subject:asText:asRaw",   // two forms
		"header:Subject:asText:all:all", // two alls
		"header:Subject:asAddresses",    // form not defined for the header
		"header:From:asDate",            // form not defined for the header
		"header:Date:asMessageIds",      // form not defined for the header
		"header:Sub ject",               // space in a name
		"header:Subject:",               // empty part
	}
	for _, p := range bad {
		if _, why := parseHeaderProperty(p); why == "" {
			t.Errorf("%q was accepted", p)
		}
	}
}

func TestEmailPropertySpecsRefusesWhatItDoesNotKnow(t *testing.T) {
	if _, mErr := emailPropertySpecs([]string{"id", "subject", "header:From:asAddresses", "headers"}); mErr != nil {
		t.Fatalf("known properties refused: %+v", mErr)
	}
	for _, props := range [][]string{
		{"id", "subjct"},
		{"id", "Subject"}, // names are case-sensitive
		{"header:From:asDate"},
		{"headers:all"},
	} {
		_, mErr := emailPropertySpecs(props)
		if mErr == nil || mErr.Type != errInvalidArguments {
			t.Errorf("%v: got %+v, want invalidArguments", props, mErr)
		}
	}
	over := make([]string, 0, maxHeaderProperties+1)
	for i := 0; i <= maxHeaderProperties; i++ {
		over = append(over, "header:X-"+strings.Repeat("a", i+1))
	}
	if _, mErr := emailPropertySpecs(over); mErr == nil || mErr.Type != errInvalidArguments {
		t.Errorf("over the property cap: %+v", mErr)
	}
}

// The known set must hold every property emailGet can answer, or a working
// property becomes invalidArguments. The literals are read out of the source so
// a property added later without a registration is a red test here.
func TestKnownEmailPropertiesCoverEveryPropertyEmailGetAnswers(t *testing.T) {
	known := knownEmailProperties()
	for p := range defaultEmailProperties {
		if !known[p] {
			t.Errorf("default property %q is not known", p)
		}
	}
	for p := range rawProperties {
		if !known[p] {
			t.Errorf("raw property %q is not known", p)
		}
	}
	re := regexp.MustCompile(`props\["([A-Za-z]+)"\]`)
	files, _ := filepath.Glob("*.go")
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		b, _ := os.ReadFile(f)
		for _, m := range re.FindAllStringSubmatch(string(b), -1) {
			if !known[m[1]] {
				t.Errorf("%s reads props[%q], which Email/get would refuse as unknown", f, m[1])
			}
		}
	}
}

func fieldsOf(t *testing.T, block string) []mime.Field {
	t.Helper()
	f, _ := mime.SplitHeaderFields([]byte(block+"\r\n\r\n"), 0)
	return f
}

func TestAddHeaderPropertiesAnswersEveryForm(t *testing.T) {
	body := &emailBody{fields: fieldsOf(t, strings.Join([]string{
		"Received: from a",
		"Received: from b",
		"Subject: =?UTF-8?Q?Caf=C3=A9?=",
		" au lait",
		"From: \"Joe\" <joe@example.com>",
		"To: Team: a@example.com, b@example.com;",
		"To: late@example.com",
		"Message-ID: <m1@example.com>",
		"References: <r1@example.com> <r2@example.com>",
		"Date: Fri, 21 Nov 1997 09:55:06 -0600",
		"List-Unsubscribe: <mailto:u@example.com>, <https://example.com/u>",
	}, "\r\n"))}
	props := map[string]bool{"id": true}
	for _, p := range []string{
		"header:Received", "header:received:all", "header:Subject:asText", "header:Subject", "header:From:asAddresses",
		"header:To:asAddresses", "header:To:asAddresses:all", "header:To:asGroupedAddresses:all",
		"header:Message-ID:asMessageIds", "header:References:asMessageIds", "header:Date:asDate",
		"header:List-Unsubscribe:asURLs", "header:X-Absent", "header:X-Absent:all", "header:X-Absent:asText",
		"header:Subject:asRaw", "headers",
	} {
		props[p] = true
	}
	obj := map[string]any{"id": "x"}
	addHeaderProperties(obj, props, body)
	b, _ := json.Marshal(obj)
	var got map[string]any
	_ = json.Unmarshal(b, &got)

	want := map[string]any{
		"header:Received":         " from b", // the LAST instance without :all
		"header:received:all":     []any{" from a", " from b"},
		"header:Subject:asText":   "Café au lait",
		"header:Subject":          " =?UTF-8?Q?Caf=C3=A9?=\r\n au lait",
		"header:Subject:asRaw":    " =?UTF-8?Q?Caf=C3=A9?=\r\n au lait",
		"header:From:asAddresses": []any{map[string]any{"name": "Joe", "email": "joe@example.com"}},
		"header:To:asAddresses":   []any{map[string]any{"name": nil, "email": "late@example.com"}},
		"header:To:asAddresses:all": []any{
			[]any{map[string]any{"name": nil, "email": "a@example.com"}, map[string]any{"name": nil, "email": "b@example.com"}},
			[]any{map[string]any{"name": nil, "email": "late@example.com"}},
		},
		"header:To:asGroupedAddresses:all": []any{
			[]any{map[string]any{"name": "Team", "addresses": []any{
				map[string]any{"name": nil, "email": "a@example.com"}, map[string]any{"name": nil, "email": "b@example.com"}}}},
			[]any{map[string]any{"name": nil, "addresses": []any{map[string]any{"name": nil, "email": "late@example.com"}}}},
		},
		"header:Message-ID:asMessageIds": []any{"m1@example.com"},
		"header:References:asMessageIds": []any{"r1@example.com", "r2@example.com"},
		"header:Date:asDate":             "1997-11-21T09:55:06-06:00",
		"header:List-Unsubscribe:asURLs": []any{"mailto:u@example.com", "https://example.com/u"},
		"header:X-Absent":                nil,
		"header:X-Absent:all":            []any{},
		"header:X-Absent:asText":         nil,
		"headersTruncated":               false,
	}
	for k, w := range want {
		g, present := got[k]
		if !present {
			t.Errorf("%s missing from %v", k, got)
			continue
		}
		if !reflect.DeepEqual(g, w) {
			t.Errorf("%s = %#v, want %#v", k, g, w)
		}
	}
	hs, _ := got["headers"].([]any)
	if len(hs) != 10 {
		t.Fatalf("headers has %d entries, want 10: %v", len(hs), hs)
	}
	if first := hs[0].(map[string]any); first["name"] != "Received" || first["value"] != " from a" {
		t.Errorf("headers[0] = %v", first)
	}
}

func TestAddHeaderPropertiesLeavesOtherRequestsAlone(t *testing.T) {
	obj := map[string]any{"id": "x"}
	addHeaderProperties(obj, map[string]bool{"id": true, "subject": true}, &emailBody{})
	if len(obj) != 1 {
		t.Fatalf("a request without header properties gained keys: %v", obj)
	}
	// An unreadable message gives no values and says why.
	obj = map[string]any{"id": "x"}
	addHeaderProperties(obj, map[string]bool{"id": true, "header:Subject": true}, &emailBody{unreadable: true})
	if obj["bodyUnavailable"] != true || len(obj) != 2 {
		t.Fatalf("unreadable message: %v", obj)
	}
}

// Spellings that mean the same thing are computed once and share the value;
// the size reported counts every key that is emitted.
func TestAddHeaderPropertiesComputesEachMeaningOnce(t *testing.T) {
	body := &emailBody{fields: fieldsOf(t, "Received: a\r\nReceived: b\r\nSubject: s")}
	props := map[string]bool{"id": true}
	spellings := []string{"header:Received:all", "header:received:all", "header:RECEIVED:all", "header:ReCeIvEd:all",
		"header:Received:asRaw:all", "header:Received", "header:received"}
	for _, p := range spellings {
		props[p] = true
	}
	before := headerValueComputations.Load()
	obj := map[string]any{"id": "x"}
	size := addHeaderProperties(obj, props, body)
	// Two meanings: Received:all (x5 spellings, asRaw is the default form) and
	// Received last-instance (x2).
	if n := headerValueComputations.Load() - before; n != 2 {
		t.Errorf("%d computations for 2 distinct meanings", n)
	}
	for _, p := range spellings[:5] {
		if got, _ := obj[p].([]any); len(got) != 2 {
			t.Errorf("%s = %#v", p, obj[p])
		}
	}
	if size <= 0 {
		t.Error("no size reported")
	}
}

func TestHeaderPropertyCapCountsSpellings(t *testing.T) {
	var ps []string
	for i := 0; i <= maxHeaderProperties; i++ {
		ps = append(ps, "header:"+strings.Repeat("A", 1+i)+":all")
	}
	if _, mErr := emailPropertySpecs(ps[:maxHeaderProperties]); mErr != nil {
		t.Fatalf("at the cap: %+v", mErr)
	}
	if _, mErr := emailPropertySpecs(ps); mErr == nil || mErr.Type != errInvalidArguments {
		t.Fatalf("over the cap: %+v", mErr)
	}
	// An exact duplicate is not a new spelling.
	dup := []string{"header:Subject", "header:Subject", "header:Subject"}
	if specs, mErr := emailPropertySpecs(dup); mErr != nil || len(specs) != 1 {
		t.Fatalf("duplicates: %v %+v", specs, mErr)
	}
}
