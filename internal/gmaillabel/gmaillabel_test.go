// SPDX-License-Identifier: AGPL-3.0-only

package gmaillabel

import (
	"reflect"
	"testing"
)

func TestClassify(t *testing.T) {
	cases := []struct {
		name   string
		labels []string
		folder string
		user   []string
	}{
		{"archived: no folder-like label", []string{"Work"}, FolderArchive, []string{"Work"}},
		{"inbox", []string{`\Inbox`, `\Important`, "Work", "Été"}, FolderInbox, []string{"Work", "Été"}},
		{"sent to yourself lands in Sent", []string{`\Inbox`, `\Sent`}, FolderSent, nil},
		{"drafts beats sent", []string{`\Sent`, `\Draft`}, FolderDrafts, nil},
		{"Gmail's \\Drafts spelling", []string{`\Drafts`}, FolderDrafts, nil},
		{"starred and important are not labels", []string{`\Starred`, `\Important`}, FolderArchive, nil},
		{"case of the system label", []string{`\INBOX`}, FolderInbox, nil},
		{"duplicates collapse", []string{"A", "A", `\Inbox`, `\Inbox`}, FolderInbox, []string{"A"}},
		{"label with spaces and quotes", []string{`Say "hi"`, "Two words"}, FolderArchive, []string{`Say "hi"`, "Two words"}},
		{"empty", []string{}, FolderArchive, nil},
	}
	for _, c := range cases {
		got := Classify(c.labels)
		if got.Folder != c.folder || !reflect.DeepEqual(got.User, c.user) {
			t.Errorf("%s: got folder=%q user=%q, want %q %q", c.name, got.Folder, got.User, c.folder, c.user)
		}
	}
}

func TestDiff(t *testing.T) {
	add, rm := Diff(nil, []string{`\Inbox`, "A", "B"})
	if !reflect.DeepEqual(add, []string{"A", "B"}) || rm != nil {
		t.Errorf("first observation: add=%q rm=%q", add, rm)
	}
	add, rm = Diff([]string{`\Inbox`, "A", "B"}, []string{"B", "C"})
	if !reflect.DeepEqual(add, []string{"C"}) || !reflect.DeepEqual(rm, []string{"A"}) {
		t.Errorf("change: add=%q rm=%q", add, rm)
	}
	add, rm = Diff([]string{"A"}, []string{"A", `\Starred`})
	if add != nil || rm != nil {
		t.Errorf("a system-label change is not a user-label change: add=%q rm=%q", add, rm)
	}
}

func TestEqual(t *testing.T) {
	if !Equal([]string{"a", "b"}, []string{"b", "a"}) {
		t.Error("order must not matter")
	}
	if Equal(nil, []string{}) {
		t.Error("never observed is not the same as observed empty")
	}
	if Equal([]string{"a"}, []string{"a", "b"}) {
		t.Error("different sets compared equal")
	}
}

func TestSanitize(t *testing.T) {
	if Sanitize(nil) != nil {
		t.Fatal("nil (no reading) must stay nil, never become an empty label set")
	}
	got := Sanitize([]string{"a\x00b", "c\td\u0085", "\x00", `\Inbox`, "\xffok", "été"})
	want := []string{"ab", "cd", `\Inbox`, "\uFFFDok", "été"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Sanitize = %q, want %q", got, want)
	}
	if s := Sanitize([]string{}); s == nil || len(s) != 0 {
		t.Errorf("an empty reading must stay an empty, non-nil reading: %#v", s)
	}
}
