// SPDX-License-Identifier: AGPL-3.0-only

//go:build tantivy && cgo

package tantivy_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"mailroom/internal/search"
)

// The index schema v3 fields (#722/#723) against the REAL engine: list:,
// has:unsubscribe, the attachment families, filename: (analysed, with the
// dotted-name phrase), larger:/smaller: (the size-prefix tokens), alias: /
// deliveredto: and their domain view, and OR across them. The in-memory backend
// answers the same cases in internal/search/operators_test.go; these are the
// ones whose meaning depends on the engine — the raw tokenizer's exactness, the
// analyser's split of "report.pdf", the phrase across a multi-valued field, and
// an OR of ~150 size tokens.
func TestTantivyV3Fields(t *testing.T) {
	b := open(t)
	at := time.Date(2026, 7, 12, 9, 0, 0, 0, time.UTC)
	named := map[string]search.Document{
		"list": {ID: mkID(1, "LST"), Subject: "Weekly", ListID: `"Announce" <announce.lists.example.org>`,
			Unsubscribe: true, Size: 4 << 10},
		"other-list": {ID: mkID(2, "OTL"), Subject: "Daily", ListID: "<dev.other.example>", Size: 900},
		"unsub-only": {ID: mkID(3, "UNS"), Subject: "Promo", Unsubscribe: true, Size: 1024},
		"pdf": {ID: mkID(4, "PDF"), Subject: "Invoice", Size: 1025, Attachments: []search.Attachment{
			{Filename: "Invoice 2026-03.pdf", ContentType: "application/pdf"}}},
		"octet-pdf": {ID: mkID(5, "OCT"), Subject: "Scan", Size: 5 << 20, Attachments: []search.Attachment{
			{Filename: "scan.PDF", ContentType: "application/octet-stream"}}},
		"two-files": {ID: mkID(6, "TWO"), Subject: "Two", Size: 12 << 20, Attachments: []search.Attachment{
			{Filename: "report.docx", ContentType: "application/vnd.openxmlformats-officedocument.wordprocessingml.document"},
			{Filename: "photo.pdf", ContentType: "image/jpeg"}}},
		"report-pdf": {ID: mkID(7, "RPT"), Subject: "Report", Size: 30 << 20, Attachments: []search.Attachment{
			{Filename: "report.pdf", ContentType: "application/pdf"}}},
		"alias":       {ID: mkID(8, "ALI"), Subject: "Patreon", DeliveredTo: "sub@me.example.net", Size: 100},
		"alias-sub":   {ID: mkID(9, "ALS"), Subject: "Shop", DeliveredTo: "shop@mail.me.example.net", Size: 100},
		"alias-other": {ID: mkID(10, "ALO"), Subject: "Else", DeliveredTo: "x@notme.example.net", Size: 100},
	}
	var docs []search.Document
	for _, d := range named {
		d.ReceivedAt = at
		docs = append(docs, d)
	}
	// Filler of every size class, so a size range that over-matched would show.
	for i := 0; i < 40; i++ {
		docs = append(docs, search.Document{ID: mkID(int64(100+i), fmt.Sprintf("F%02d", i)),
			Subject: "filler", Size: int64(1) << uint(i%31), ReceivedAt: at})
	}
	if err := b.Upsert(context.Background(), docs); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	cases := []struct {
		q    string
		want []string
	}{
		{"list:announce.lists.example.org", []string{"list"}},
		{"list:announce@lists.example.org", []string{"list"}},
		{"list:example.org", []string{"list"}},
		{"list:lists.example", nil},
		{"has:unsubscribe", []string{"list", "unsub-only"}},
		{"has:pdf", []string{"pdf", "octet-pdf", "report-pdf"}},
		{"has:image", []string{"two-files"}},
		{"has:document", []string{"two-files"}},
		{"has:presentation", nil},
		{"filename:pdf", []string{"pdf", "octet-pdf", "report-pdf", "two-files"}},
		{"filename:invoice", []string{"pdf"}},
		{"filename:report.pdf", []string{"report-pdf"}}, // NOT two-files: report.docx + photo.pdf
		{"filename:report", []string{"two-files", "report-pdf"}},
		{"filename:rep*", []string{"two-files", "report-pdf"}},
		{"alias:sub@me.example.net", []string{"alias"}},
		{"deliveredto:me.example.net", []string{"alias", "alias-sub"}},
		{"alias:mail.me.example.net", []string{"alias-sub"}},
		{"has:pdf OR has:image", []string{"pdf", "octet-pdf", "report-pdf", "two-files"}},
		{"(has:pdf OR has:unsubscribe) -list:example.org", []string{"pdf", "octet-pdf", "report-pdf", "unsub-only"}},
	}
	for _, tc := range cases {
		got := hits(t, b, tc.q)
		if len(got) != len(tc.want) {
			t.Errorf("%q matched %d, want %v", tc.q, len(got), tc.want)
		}
		for _, k := range tc.want {
			if !got[named[k].ID] {
				t.Errorf("%q missed %s", tc.q, k)
			}
		}
	}

	// Size ranges are exact to the byte on the real engine: count every
	// document whose size satisfies the bound and compare.
	for _, tc := range []struct {
		q  string
		in func(int64) bool
	}{
		{"larger:1K", func(s int64) bool { return s > 1024 }},
		{"smaller:1K", func(s int64) bool { return s < 1024 }},
		{"larger:5M", func(s int64) bool { return s > 5<<20 }},
		{"larger:1024 smaller:5M", func(s int64) bool { return s > 1024 && s < 5<<20 }},
		{"larger:20M OR smaller:200", func(s int64) bool { return s > 20<<20 || s < 200 }},
	} {
		want := 0
		for _, d := range docs {
			if tc.in(d.Size) {
				want++
			}
		}
		res, err := b.Search(context.Background(), search.Query{
			Root: search.Parse(tc.q, search.ParseOptions{}).Root, Limit: search.MaxLimit})
		if err != nil {
			t.Fatalf("%q: %v", tc.q, err)
		}
		if res.Total != want || want == 0 {
			t.Errorf("%q matched %d, want %d (a zero want means the fixture does not exercise it)", tc.q, res.Total, want)
		}
	}
}
