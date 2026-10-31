// SPDX-License-Identifier: AGPL-3.0-only

package api

// Unit tests (no database) for the Label JMAP extension rendering: the Label
// object shape, the local-only color/nesting metadata, the read-only keyword
// atom, and the requested-property subset.

import (
	"testing"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/store"
)

func TestLabelObjectFull(t *testing.T) {
	id := ulid.MustParse("00000000000000000000000001")
	parent := ulid.MustParse("00000000000000000000000002")
	lv := store.LabelView{
		ID:           id,
		Name:         "Invoices",
		Color:        "#ff8800",
		ParentID:     &parent,
		MirrorToIMAP: true,
		KeywordAtom:  "MailroomLabel-invoices",
	}
	obj := labelObject(lv, nil)

	if obj["id"] != id.String() {
		t.Errorf("id = %v", obj["id"])
	}
	if obj["name"] != "Invoices" {
		t.Errorf("name = %v", obj["name"])
	}
	if obj["color"] != "#ff8800" {
		t.Errorf("color = %v", obj["color"])
	}
	if obj["parentId"] != parent.String() {
		t.Errorf("parentId = %v", obj["parentId"])
	}
	if obj["mirrorToImap"] != true {
		t.Errorf("mirrorToImap = %v", obj["mirrorToImap"])
	}
	if obj["keyword"] != "MailroomLabel-invoices" {
		t.Errorf("keyword = %v", obj["keyword"])
	}
}

// A local-only label (no atom, unset color/parent) renders those as JSON null,
// not empty strings — the documented non-roundtrip caveat surfaces as null.
func TestLabelObjectLocalOnly(t *testing.T) {
	id := ulid.MustParse("00000000000000000000000003")
	lv := store.LabelView{ID: id, Name: "Personal", MirrorToIMAP: false}
	obj := labelObject(lv, nil)

	for _, k := range []string{"color", "parentId", "keyword"} {
		if v, ok := obj[k]; !ok || v != nil {
			t.Errorf("%s = %v (ok=%v), want nil", k, v, ok)
		}
	}
	if obj["mirrorToImap"] != false {
		t.Errorf("mirrorToImap = %v, want false", obj["mirrorToImap"])
	}
}

func TestLabelObjectPropertySubset(t *testing.T) {
	id := ulid.MustParse("00000000000000000000000004")
	lv := store.LabelView{ID: id, Name: "Work", Color: "blue", KeywordAtom: "MailroomLabel-work"}
	obj := labelObject(lv, map[string]bool{"name": true})

	if _, ok := obj["color"]; ok {
		t.Error("color should be excluded when not requested")
	}
	if obj["id"] != id.String() {
		t.Error("id must always be present")
	}
	if obj["name"] != "Work" {
		t.Errorf("name = %v", obj["name"])
	}
}
