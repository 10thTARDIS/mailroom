// SPDX-License-Identifier: AGPL-3.0-only

package importer

import (
	"slices"
	"testing"
)

func TestClassifyGmail(t *testing.T) {
	for _, tc := range []struct {
		name       string
		header     string
		wantFolder string
		wantFlags  []string
		wantLabels []string
		wantOK     bool
	}{
		{
			name:       "no header at all",
			header:     "",
			wantOK:     false,
			wantFolder: "",
		},
		{
			// The single most common shape in a Takeout: no folder-ish label at
			// all. That IS Gmail's archive.
			name:       "archived and read",
			header:     "Work",
			wantFolder: "Archive",
			wantFlags:  []string{`\Seen`},
			wantLabels: []string{"Work"},
			wantOK:     true,
		},
		{
			// Unread is ASSERTED. Its absence means read — getting this backwards
			// hands the user an archive of 20,000 unread messages.
			name:       "unread inbox mail",
			header:     "Inbox,Unread,Important",
			wantFolder: "Inbox",
			wantFlags:  nil,
			wantLabels: []string{"Important"},
			wantOK:     true,
		},
		{
			name:       "starred",
			header:     "Inbox,Starred",
			wantFolder: "Inbox",
			wantFlags:  []string{`\Flagged`, `\Seen`},
			wantOK:     true,
		},
		{
			name:       "spam is a folder and a flag",
			header:     "Spam",
			wantFolder: "Spam",
			wantFlags:  []string{"junk", `\Seen`},
			wantOK:     true,
		},
		{
			// A message you sent to yourself carries both. local_mailbox_id holds
			// exactly ONE mailbox, so priority decides: Sent beats Inbox.
			name:       "inbox and sent",
			header:     "Inbox,Sent",
			wantFolder: "Sent",
			wantFlags:  []string{`\Seen`},
			wantOK:     true,
		},
		{
			name:       "trash outranks everything",
			header:     "Inbox,Trash,Work",
			wantFolder: "Trash",
			wantFlags:  []string{`\Seen`},
			wantLabels: []string{"Work"},
			wantOK:     true,
		},
		{
			name:       "categories and nested labels stay labels",
			header:     "Category Promotions,Work/Projects,Unread",
			wantFolder: "Archive",
			wantLabels: []string{"Category Promotions", "Work/Projects"},
			wantOK:     true,
		},
		{
			name:       "drafts",
			header:     "Draft",
			wantFolder: "Drafts",
			wantFlags:  []string{`\Draft`, `\Seen`},
			wantOK:     true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			folder, flags, labels, ok := classifyGmail(tc.header)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if folder != tc.wantFolder {
				t.Errorf("folder = %q, want %q", folder, tc.wantFolder)
			}
			slices.Sort(flags)
			want := slices.Clone(tc.wantFlags)
			slices.Sort(want)
			if !slices.Equal(flags, want) {
				t.Errorf("flags = %v, want %v", flags, want)
			}
			if !slices.Equal(labels, tc.wantLabels) {
				t.Errorf("labels = %v, want %v", labels, tc.wantLabels)
			}
		})
	}
}

func TestHeaderValue(t *testing.T) {
	raw := []byte("Subject: hi\r\n" +
		"X-Gmail-Labels: Inbox,Work,\r\n" +
		" Category Personal\r\n" +
		"Status: RO\r\n" +
		"\r\n" +
		"X-Gmail-Labels: not a header, this is the body\r\n")

	if got, want := headerValue(raw, "X-Gmail-Labels"), "Inbox,Work, Category Personal"; got != want {
		t.Errorf("folded header = %q, want %q", got, want)
	}
	if got, want := headerValue(raw, "status"), "RO"; got != want {
		t.Errorf("case-insensitive lookup = %q, want %q", got, want)
	}
	if got := headerValue(raw, "Nope"); got != "" {
		t.Errorf("absent header = %q, want empty", got)
	}
	// A header-looking line in the BODY must not be picked up.
	if got := headerValue([]byte("Subject: x\n\nX-Gmail-Labels: Inbox\n"), "X-Gmail-Labels"); got != "" {
		t.Errorf("body line read as a header: %q", got)
	}
	// A message with no body separator at all (truncated) is all header.
	if got, want := headerValue([]byte("Subject: x\nStatus: R"), "Status"), "R"; got != want {
		t.Errorf("truncated message header = %q, want %q", got, want)
	}
}

func TestStatusFlags(t *testing.T) {
	for _, tc := range []struct {
		status, xstatus string
		want            []string
	}{
		{"RO", "", []string{`\Seen`}},
		{"O", "", nil},
		{"R", "AF", []string{`\Seen`, `\Answered`, `\Flagged`}},
		{"", "T", []string{`\Draft`}},
	} {
		got := statusFlags(tc.status, tc.xstatus)
		slices.Sort(got)
		want := slices.Clone(tc.want)
		slices.Sort(want)
		if !slices.Equal(got, want) {
			t.Errorf("statusFlags(%q,%q) = %v, want %v", tc.status, tc.xstatus, got, want)
		}
	}
}

// TestClassifyPrefersGmailLabelsOverTheFile is the Takeout case: the archive is one
// flat "All mail" mbox, so the FILE tells us nothing and the header tells us
// everything.
func TestClassifyPrefersGmailLabelsOverTheFile(t *testing.T) {
	im, err := New(nil, nil, Options{UserID: newTestULID(), DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	item := &Item{
		Folder: "All mail Including Spam and Trash",
		Raw:    []byte("X-Gmail-Labels: Inbox,Unread,Work\r\nSubject: x\r\n\r\nbody\r\n"),
	}
	folder, flags, labels := im.classify(item)
	if folder != "Inbox" {
		t.Errorf("folder = %q, want Inbox (from the label, not the file)", folder)
	}
	if len(flags) != 0 {
		t.Errorf("flags = %v, want none (Unread)", flags)
	}
	if !slices.Equal(labels, []string{"Work"}) {
		t.Errorf("labels = %v, want [Work]", labels)
	}

	// A plain mbox with no Gmail header keeps its file's folder and reads its
	// flags from Status:.
	plain := &Item{
		Folder: "old-mail",
		Raw:    []byte("Status: RO\r\nX-Status: F\r\nSubject: x\r\n\r\nbody\r\n"),
	}
	folder, flags, labels = im.classify(plain)
	if folder != "old-mail" {
		t.Errorf("folder = %q, want old-mail", folder)
	}
	slices.Sort(flags)
	if !slices.Equal(flags, []string{`\Flagged`, `\Seen`}) {
		t.Errorf("flags = %v, want [\\Flagged \\Seen]", flags)
	}
	if len(labels) != 0 {
		t.Errorf("labels = %v, want none", labels)
	}
}
