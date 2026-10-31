// SPDX-License-Identifier: AGPL-3.0-only

package search

import (
	"reflect"
	"testing"
)

// The attachment-type families (#723), table-tested against the one table that
// defines them (attachtype.go).

func TestFamilyOf(t *testing.T) {
	tests := []struct {
		name, filename, contentType, want string
	}{
		// By MIME type.
		{"pdf by type", "x", "application/pdf", FamilyPDF},
		{"pdf type with parameters", "", "Application/PDF; name=\"x.pdf\"", FamilyPDF},
		{"any image/*", "", "image/heic", FamilyImage},
		{"docx by type", "", "application/vnd.openxmlformats-officedocument.wordprocessingml.document", FamilyDocument},
		{"odt by type", "", "application/vnd.oasis.opendocument.text", FamilyDocument},
		{"plain text is a document", "notes", "text/plain", FamilyDocument},
		{"csv is a spreadsheet", "", "text/csv", FamilySpreadsheet},
		{"xlsx by type", "", "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet", FamilySpreadsheet},
		{"pptx by type", "", "application/vnd.openxmlformats-officedocument.presentationml.presentation", FamilyPresentation},
		// The type wins over the name when it decides.
		{"type beats extension", "scan.pdf", "image/png", FamilyImage},
		// The extension decides when the type does not.
		{"octet-stream pdf", "Scan.PDF", "application/octet-stream", FamilyPDF},
		{"no type, docx", "letter.docx", "", FamilyDocument},
		{"md", "README.md", "application/octet-stream", FamilyDocument},
		{"ods", "q3.ods", "application/octet-stream", FamilySpreadsheet},
		{"key", "deck.key", "application/octet-stream", FamilyPresentation},
		{"jpeg", "photo.jpeg", "application/octet-stream", FamilyImage},
		{"unknown type, unknown extension", "archive.zip", "application/zip", ""},
		{"no extension", "README", "application/octet-stream", ""},
		{"dot only", "file.", "application/octet-stream", ""},
		{"nothing at all", "", "", ""},
	}
	for _, tc := range tests {
		if got := familyOf(Attachment{Filename: tc.filename, ContentType: tc.contentType}); got != tc.want {
			t.Errorf("%s: familyOf(%q, %q) = %q, want %q", tc.name, tc.filename, tc.contentType, got, tc.want)
		}
	}
}

// Every family is reachable under its own name, has some way to be detected,
// and its names never collide with another family's or with a flag atom the
// index already carries — a collision would make has:<x> mean two things.
func TestFamiliesAreWellFormed(t *testing.T) {
	seen := map[string]string{}
	older := []string{FlagRead, FlagUnread, FlagStarred, FlagAnswered, FlagDraft, FlagJunk, FlagTrashed,
		FlagAttachment, FlagUnsubscribe}
	for _, a := range older {
		seen[a] = "flag " + a
	}
	if len(attachmentFamilies) != 5 {
		t.Errorf("%d families, want the five #723 names", len(attachmentFamilies))
	}
	for _, f := range attachmentFamilies {
		if len(f.Types)+len(f.Prefixes) == 0 || len(f.Exts) == 0 {
			t.Errorf("family %q must be detectable both by type and by extension", f.Name)
		}
		for _, n := range append([]string{f.Name}, f.Aliases...) {
			if prev, dup := seen[n]; dup {
				t.Errorf("%q names family %q and %s", n, f.Name, prev)
			}
			seen[n] = "family " + f.Name
			atom, ok := flagAtom(n)
			if !ok || atom != f.Name {
				t.Errorf("has:%s parses to %q (%v), want the family atom %q", n, atom, ok, f.Name)
			}
		}
		if flagSchemaVersion(f.Name) != 3 {
			t.Errorf("family %q is not marked schema v3: an old index would answer has:%s with nothing", f.Name, f.Name)
		}
	}
}

func TestDocumentFlagValuesCarryFamilies(t *testing.T) {
	d := Document{
		Flags:       Flags(true, false, false, false, false, false, true),
		Unsubscribe: true,
		Attachments: []Attachment{
			{Filename: "a.pdf", ContentType: "application/pdf"},
			{Filename: "b.pdf", ContentType: "application/pdf"},
			{Filename: "c.png", ContentType: "image/png"},
			{Filename: "d.zip", ContentType: "application/zip"},
		},
	}
	want := []string{FlagAttachment, FamilyImage, FamilyPDF, FlagRead, FlagUnsubscribe}
	if got := d.Values(FieldFlag); !reflect.DeepEqual(got, want) {
		t.Errorf("flag values = %v, want %v", got, want)
	}
}
