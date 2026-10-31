// SPDX-License-Identifier: AGPL-3.0-only

package importer

import (
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"testing"
	"time"
)

// buildMaildir lays out a maildir: root INBOX plus a Maildir++ subfolder.
func buildMaildir(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	for _, dir := range []string{
		"cur", "new", "tmp",
		".Work.Projects/cur", ".Work.Projects/new", ".Work.Projects/tmp",
	} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	msg := "From: a@example.com\nSubject: hi\n\nbody\n"
	files := map[string]string{
		"cur/1700000001.M1.host:2,S":               msg,
		"cur/1700000002.M2.host:2,FRS":             msg,
		"new/1700000003.M3.host":                   msg,
		"tmp/1700000004.M4.host":                   msg, // in-progress delivery: skipped
		".Work.Projects/cur/1700000005.M5.host:2,": msg,
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(root, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func TestMaildirReadsCurAndNewButNotTmp(t *testing.T) {
	root := buildMaildir(t)

	kind, err := Detect(root)
	if err != nil {
		t.Fatal(err)
	}
	if kind != KindMaildir {
		t.Fatalf("Detect = %q, want maildir", kind)
	}

	src, err := Open(root, kind, DefaultMaxMessageBytes)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = src.Close() }()

	items, bad := drain(t, src)
	if bad != 0 {
		t.Fatalf("bad = %d, want 0", bad)
	}
	if len(items) != 4 {
		t.Fatalf("messages = %d, want 4 (3 in INBOX cur+new, 1 in the subfolder; tmp/ is skipped)", len(items))
	}

	byFolder := map[string]int{}
	for _, it := range items {
		byFolder[it.Folder]++
	}
	want := map[string]int{"INBOX": 3, "Work/Projects": 1}
	if !reflect.DeepEqual(byFolder, want) {
		t.Errorf("folders = %v, want %v", byFolder, want)
	}

	// Flags come from the filename's info section; new/ is unseen.
	var seen, unseen int
	for _, it := range items {
		if slices.Contains(it.Flags, `\Seen`) {
			seen++
		} else {
			unseen++
		}
	}
	if seen != 2 || unseen != 2 {
		t.Errorf("seen=%d unseen=%d, want 2/2 (cur:2,S and cur:2,FRS seen; new/ and :2, unseen)", seen, unseen)
	}

	// The filename's leading Unix timestamp is the delivery time.
	for _, it := range items {
		if it.ReceivedAt.IsZero() {
			t.Errorf("message in %s has no received_at", it.Folder)
		}
	}
	if got := items[0].ReceivedAt.UTC(); !got.Equal(time.Unix(1700000001, 0).UTC()) {
		t.Errorf("received_at = %s, want the filename's timestamp", got)
	}
}

func TestMaildirFlags(t *testing.T) {
	for _, tc := range []struct {
		name  string
		isNew bool
		want  []string
	}{
		{"1.M.host:2,S", false, []string{`\Seen`}},
		{"1.M.host:2,FRS", false, []string{`\Answered`, `\Flagged`, `\Seen`}},
		{"1.M.host:2,DT", false, []string{`\Draft`, `\Deleted`}},
		{"1.M.host:2,", false, nil},
		{"1.M.host", false, nil},
		{"1.M.host:2,S", true, nil}, // new/ is unseen whatever the name claims
	} {
		got := maildirFlags(tc.name, tc.isNew)
		slices.Sort(got)
		want := slices.Clone(tc.want)
		slices.Sort(want)
		if !slices.Equal(got, want) {
			t.Errorf("maildirFlags(%q, new=%v) = %v, want %v", tc.name, tc.isNew, got, want)
		}
	}
}

func TestMaildirSeekResumesWithinAFolder(t *testing.T) {
	root := buildMaildir(t)
	src, err := Open(root, KindMaildir, DefaultMaxMessageBytes)
	if err != nil {
		t.Fatal(err)
	}
	first, err := src.Next()
	if err != nil {
		t.Fatal(err)
	}
	_ = src.Close()

	src2, err := Open(root, KindMaildir, DefaultMaxMessageBytes)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = src2.Close() }()
	if err := src2.Seek(first.After); err != nil {
		t.Fatal(err)
	}
	rest, bad := drain(t, src2)
	if bad != 0 {
		t.Fatalf("bad = %d", bad)
	}
	if len(rest) != 3 {
		t.Fatalf("after resuming past 1 of 4, got %d, want 3", len(rest))
	}
}

func TestMaildirSkipsOversizedMessage(t *testing.T) {
	root := buildMaildir(t)
	big := make([]byte, 4096)
	for i := range big {
		big[i] = 'x'
	}
	if err := os.WriteFile(filepath.Join(root, "cur", "1700000009.M9.host:2,S"), big, 0o600); err != nil {
		t.Fatal(err)
	}
	src, err := Open(root, KindMaildir, 1024)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = src.Close() }()
	items, bad := drain(t, src)
	if bad != 1 {
		t.Errorf("bad = %d, want 1 (the oversized file)", bad)
	}
	if len(items) != 4 {
		t.Errorf("good = %d, want 4 — the oversized file must not stop the walk", len(items))
	}
}
