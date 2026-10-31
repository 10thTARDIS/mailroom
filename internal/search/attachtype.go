// SPDX-License-Identifier: AGPL-3.0-only

package search

import (
	"path"
	"strings"
)

// Attachment TYPE FAMILIES (#723): has:pdf, has:image, has:document,
// has:spreadsheet, has:presentation.
//
// THIS TABLE IS THE ONLY DEFINITION. The index derives a message's family atoms
// from it (Document.Values(FieldFlag)), the parser accepts exactly its names
// (flagAtom), and attachtype_test.go walks it — so a family cannot be
// searchable under a name the index never writes, or written under one nobody
// can type.
//
// A part's family is decided by its MIME TYPE FIRST and its file-name
// extension only when the type does not decide it. The declared type is what
// the message says the part is; the extension is the fallback for the very
// common `application/octet-stream` (and its kin) that tells us nothing. A part
// whose type names one family and whose name names another is the TYPE's —
// e.g. `image/png` called "scan.pdf" is an image, which is what a viewer will
// open it as.

// Attachment is one attachment part as the index sees it: its declared file
// name and MIME type (app.attachments, the same projection the UI lists).
type Attachment struct {
	Filename    string
	ContentType string
}

// attachmentFamily is one has:<name> family.
type attachmentFamily struct {
	// Name is the flag atom and the has:/is: value. Aliases are other values
	// the parser accepts for it.
	Name    string
	Aliases []string
	// Types are exact MIME types; Prefixes are MIME type prefixes ("image/").
	Types    []string
	Prefixes []string
	// Exts are file-name extensions, lower case, without the dot.
	Exts []string
}

// The family atoms. They share FieldFlag with is:unread and friends — a family
// is a yes/no fact about a message, which is what flag atoms are for — and are
// distinct from every older atom.
const (
	FamilyPDF          = "pdf"
	FamilyImage        = "image"
	FamilyDocument     = "document"
	FamilySpreadsheet  = "spreadsheet"
	FamilyPresentation = "presentation"
)

var attachmentFamilies = []attachmentFamily{
	{
		Name:  FamilyPDF,
		Types: []string{"application/pdf", "application/x-pdf"},
		Exts:  []string{"pdf"},
	},
	{
		Name:     FamilyImage,
		Aliases:  []string{"images", "photo", "photos"},
		Prefixes: []string{"image/"},
		Exts:     []string{"jpg", "jpeg", "png", "gif", "webp", "heic", "heif", "bmp", "tif", "tiff", "svg"},
	},
	{
		Name:    FamilyDocument,
		Aliases: []string{"documents", "doc"},
		Types: []string{
			"application/msword",
			"application/vnd.openxmlformats-officedocument.wordprocessingml.document",
			"application/vnd.oasis.opendocument.text",
			"application/rtf", "text/rtf",
			"text/plain", "text/markdown",
		},
		Exts: []string{"doc", "docx", "odt", "rtf", "txt", "md"},
	},
	{
		Name:    FamilySpreadsheet,
		Aliases: []string{"spreadsheets", "sheet"},
		Types: []string{
			"application/vnd.ms-excel",
			"application/vnd.openxmlformats-officedocument.spreadsheetml.sheet",
			"application/vnd.oasis.opendocument.spreadsheet",
			"text/csv",
		},
		Exts: []string{"xls", "xlsx", "ods", "csv"},
	},
	{
		Name:    FamilyPresentation,
		Aliases: []string{"presentations", "slides"},
		Types: []string{
			"application/vnd.ms-powerpoint",
			"application/vnd.openxmlformats-officedocument.presentationml.presentation",
			"application/vnd.oasis.opendocument.presentation",
		},
		Exts: []string{"ppt", "pptx", "odp", "key"},
	},
}

// familyOf returns the family a part belongs to, or "" for none: by MIME type
// first, then by extension (see the file header).
func familyOf(a Attachment) string {
	ct := strings.ToLower(strings.TrimSpace(a.ContentType))
	if i := strings.IndexByte(ct, ';'); i >= 0 {
		ct = strings.TrimSpace(ct[:i])
	}
	for _, f := range attachmentFamilies {
		for _, t := range f.Types {
			if ct == t {
				return f.Name
			}
		}
		for _, p := range f.Prefixes {
			if strings.HasPrefix(ct, p) {
				return f.Name
			}
		}
	}
	ext := strings.TrimPrefix(strings.ToLower(path.Ext(strings.TrimSpace(a.Filename))), ".")
	if ext == "" {
		return ""
	}
	for _, f := range attachmentFamilies {
		for _, e := range f.Exts {
			if ext == e {
				return f.Name
			}
		}
	}
	return ""
}

// attachmentFamilyAtoms is every family any of the parts belongs to.
func attachmentFamilyAtoms(parts []Attachment) []string {
	var out []string
	for _, a := range parts {
		if f := familyOf(a); f != "" {
			out = append(out, f)
		}
	}
	return dedupe(out)
}

// familyAtom maps a has:/is: value to its family atom (name or alias).
func familyAtom(v string) (string, bool) {
	for _, f := range attachmentFamilies {
		if v == f.Name {
			return f.Name, true
		}
		for _, a := range f.Aliases {
			if v == a {
				return f.Name, true
			}
		}
	}
	return "", false
}
