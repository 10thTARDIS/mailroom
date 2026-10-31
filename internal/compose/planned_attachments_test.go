// SPDX-License-Identifier: AGPL-3.0-only

package compose

import (
	"errors"
	"testing"
)

// A held send's attachment list counts every part Build attached, whatever
// its type or disposition (#49 review, F4 follow-up). The display
// classification (attachproj.Rows) leaves an inline text part with a
// Content-ID out, which would show the approver "no attachments" while the
// blob still went out.
//
// RED PROBE: return attachproj.Rows of the parsed bytes from Attachments (the
// first version) and the inline text case lists nothing.
func TestPlannedAttachmentsCountEveryAttachedPart(t *testing.T) {
	plan := func(t *testing.T, d Draft) PlannedSubmit {
		t.Helper()
		d.From = Address{Email: "me@example.com"}
		d.To = []Address{{Email: "you@example.net"}}
		d.Subject = "s"
		built, err := Build(d)
		if err != nil {
			t.Fatal(err)
		}
		return PlannedSubmit{raw: built.Raw}
	}
	secret := []byte("account,balance\nalice,100\n")
	for _, tc := range []struct {
		name string
		d    Draft
		want []string
	}{
		{"none, text only", Draft{TextBody: "hi"}, nil},
		{"none, text and html", Draft{TextBody: "hi", HTMLBody: "<p>hi</p>"}, nil},
		{"pdf attachment", Draft{TextBody: "hi", Attachments: []Attachment{
			{Name: "q3.pdf", Type: "application/pdf", Data: []byte("%PDF-1.4")}}}, []string{"q3.pdf"}},
		{"inline text with a cid", Draft{TextBody: "hi", HTMLBody: "<p>hi</p>", Attachments: []Attachment{
			{Name: "notes.txt", Type: "text/plain", Data: secret, Inline: true, ContentID: "x1@example.com"}}},
			[]string{"notes.txt"}},
		{"nameless inline html", Draft{TextBody: "hi", Attachments: []Attachment{
			{Type: "text/html", Data: secret, Inline: true}}}, []string{""}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rows, err := plan(t, tc.d).Attachments()
			if err != nil {
				t.Fatal(err)
			}
			if len(rows) != len(tc.want) {
				t.Fatalf("listed %d attachments %+v, want %d", len(rows), rows, len(tc.want))
			}
			for i, r := range rows {
				if r.Filename != tc.want[i] || r.SizeBytes <= 0 {
					t.Errorf("attachment %d = %+v, want name %q and a size", i, r, tc.want[i])
				}
			}
			if tc.name == "inline text with a cid" && rows[0].SizeBytes != int64(len(secret)) {
				t.Errorf("size = %d, want %d", rows[0].SizeBytes, len(secret))
			}
		})
	}
	// A blob declared as a container cannot be listed honestly: refused.
	_, err := plan(t, Draft{TextBody: "hi", Attachments: []Attachment{
		{Name: "x", Type: "multipart/mixed", Data: secret}}}).Attachments()
	if !errors.Is(err, ErrUnlistableAttachments) {
		t.Errorf("a multipart-declared attachment: err = %v, want ErrUnlistableAttachments", err)
	}
}
