// SPDX-License-Identifier: AGPL-3.0-only

package gmailsync

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"mailroom/internal/sync"
	"mailroom/internal/sync/gmailtest"
)

type fakeCensusStore struct {
	labels   map[string]bool
	msgIDs   map[string]bool
	idsAsked int
}

func (f *fakeCensusStore) ExistingLabels(_ context.Context, names []string) (map[string]bool, error) {
	out := map[string]bool{}
	for _, n := range names {
		if f.labels[n] {
			out[n] = true
		}
	}
	return out, nil
}

func (f *fakeCensusStore) CountMessageIDsElsewhere(_ context.Context, ids []string) (int, error) {
	f.idsAsked += len(ids)
	n := 0
	for _, id := range ids {
		if f.msgIDs[id] {
			n++
		}
	}
	return n, nil
}

// wrap gives a Message-ID its angle brackets, as a real ENVELOPE carries it.
func wrap(id string) string {
	if id == "" {
		return ""
	}
	return "<" + id + ">"
}

func at(y int) time.Time { return time.Date(y, 3, 1, 12, 0, 0, 0, time.UTC) }

// censusFixture: a known mailbox whose every number is counted by hand.
//
//	All Mail: 5 messages, 100+200+300+400+500 = 1500 bytes
//	  uid1 2019 \Inbox Work            -> inbox, Work
//	  uid2 2019 Work "Été"             -> archive
//	  uid3 2020 \Sent \Important       -> sent
//	  uid4 2024 \Inbox \Sent           -> sent (Sent beats Inbox)
//	  uid5 2024 (none)                 -> archive
//	  (uids 3, 4 and 5 carry no user label: 3 unlabelled)
//	Spam: 1 message, 50 bytes, 2024
//	Trash: 1 message, 70 bytes, 2023, label Work
func censusFixture(t *testing.T) *gmailtest.Server {
	g := gmailtest.New(t)
	g.StandardMailboxes()
	add := func(mb string, size int, y int, mid string, labels ...string) {
		g.Append(mb, &gmailtest.Message{Body: bytes.Repeat([]byte("x"), size), InternalDate: at(y), MessageID: wrap(mid), Labels: labels})
	}
	add("[Gmail]/All Mail", 100, 2019, "a@example.com", `\Inbox`, "Work")
	add("[Gmail]/All Mail", 200, 2019, "b@example.com", "Work", "Été")
	add("[Gmail]/All Mail", 300, 2020, "c@example.com", `\Sent`, `\Important`)
	add("[Gmail]/All Mail", 400, 2024, "", `\Inbox`, `\Sent`)
	add("[Gmail]/All Mail", 500, 2024, "e@example.com")
	add("[Gmail]/Spam", 50, 2024, "s@example.com")
	add("[Gmail]/Trash", 70, 2023, "t@example.com", "Work")
	return g
}

func dial(t *testing.T, g *gmailtest.Server) sync.SyncSession {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	sess, err := sync.Dial(ctx, g.Listen(sync.TLSInsecure))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = sess.Close() })
	return sess
}

func TestCensusExactOnKnownFixture(t *testing.T) {
	g := censusFixture(t)
	st := &fakeCensusStore{labels: map[string]bool{"Work": true}, msgIDs: map[string]bool{"b@example.com": true, "t@example.com": true}}
	r, err := Census(context.Background(), dial(t, g), st, CensusOptions{DailyBudgetBytes: 1000, PageSize: 2})
	if err != nil {
		t.Fatalf("census: %v", err)
	}
	if r.Messages != 7 || r.Examined != 7 || r.Bytes != 1620 || r.Sampled || r.EstimatedBytes != 1620 {
		t.Errorf("totals: messages=%d examined=%d bytes=%d sampled=%v est=%d", r.Messages, r.Examined, r.Bytes, r.Sampled, r.EstimatedBytes)
	}
	wantMB := map[string][2]int64{"[Gmail]/All Mail": {5, 1500}, "[Gmail]/Spam": {1, 50}, "[Gmail]/Trash": {1, 70}}
	if len(r.Mailboxes) != 3 {
		t.Fatalf("mailboxes = %+v", r.Mailboxes)
	}
	for _, m := range r.Mailboxes {
		w := wantMB[m.Name]
		if int64(m.Messages) != w[0] || m.Bytes != w[1] {
			t.Errorf("%s: messages=%d bytes=%d, want %v", m.Name, m.Messages, m.Bytes, w)
		}
	}
	wantFolders := map[string]int{"inbox": 1, "archive": 2, "sent": 2, "junk": 1, "trash": 1}
	for k, v := range wantFolders {
		if r.Folders[k] != v {
			t.Errorf("folder %s = %d, want %d (all: %v)", k, r.Folders[k], v, r.Folders)
		}
	}
	if r.SystemLabels[`\Inbox`] != 2 || r.SystemLabels[`\Sent`] != 2 || r.SystemLabels[`\Important`] != 1 {
		t.Errorf("system labels = %v", r.SystemLabels)
	}
	if r.DistinctUserLabels != 2 || r.LabelsExisting != 1 || r.LabelsNew != 1 {
		t.Errorf("labels distinct=%d existing=%d new=%d", r.DistinctUserLabels, r.LabelsExisting, r.LabelsNew)
	}
	if r.ApplicationsExisting != 3 || r.ApplicationsNew != 1 || r.UnlabelledMessages != 3 {
		t.Errorf("applications existing=%d new=%d unlabelled=%d", r.ApplicationsExisting, r.ApplicationsNew, r.UnlabelledMessages)
	}
	if len(r.UserLabelsTop) != 2 || r.UserLabelsTop[0].Name != "Work" || r.UserLabelsTop[0].Messages != 3 || r.UserLabelsTop[1].Name != "Été" {
		t.Errorf("top labels = %+v", r.UserLabelsTop)
	}
	if r.ByYear[2019] != 2 || r.ByYear[2020] != 1 || r.ByYear[2023] != 1 || r.ByYear[2024] != 3 {
		t.Errorf("by year = %v", r.ByYear)
	}
	// 1620 bytes at 1000/day = 2 days; at 900/day (history share) = 2 days.
	if r.DaysAtBudget != 2 || r.DaysAtHistoryBudget != 2 {
		t.Errorf("days = %d / %d", r.DaysAtBudget, r.DaysAtHistoryBudget)
	}
	if r.DedupChecked != 6 || r.DedupElsewhere != 2 || r.NoMessageID != 1 {
		t.Errorf("dedup checked=%d elsewhere=%d none=%d", r.DedupChecked, r.DedupElsewhere, r.NoMessageID)
	}

	// THE PROMISES, checked on the wire: no body, no label folder.
	if b, n := g.BodyBytesServed(); b != 0 || n != 0 {
		t.Errorf("the census downloaded %d bodies (%d bytes)", n, b)
	}
	for _, sel := range g.Selects() {
		if sel != "[Gmail]/All Mail" && sel != "[Gmail]/Spam" && sel != "[Gmail]/Trash" {
			t.Errorf("the census selected %q, a label folder", sel)
		}
	}
	for _, f := range g.Fetches() {
		if strings.Contains(f, "BODY") {
			t.Errorf("a census FETCH named a body: %s", f)
		}
	}
	var out bytes.Buffer
	r.Write(&out)
	for _, want := range []string{"READ-ONLY", "role=all", "new=1", "2019  2", "2 day(s)", "2 already exist"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("report text lacks %q:\n%s", want, out.String())
		}
	}
}

// A sampled census states its bound and labels its extrapolations.
func TestCensusSampleReportsItsBound(t *testing.T) {
	g := censusFixture(t)
	r, err := Census(context.Background(), dial(t, g), &fakeCensusStore{}, CensusOptions{Limit: 2, DailyBudgetBytes: 1000, SkipDedup: true})
	if err != nil {
		t.Fatalf("census: %v", err)
	}
	// Newest 2 of All Mail are uids 5 and 4 (900 bytes); Spam and Trash have 1.
	if !r.Sampled || r.Examined != 4 || r.Messages != 7 {
		t.Errorf("sampled=%v examined=%d messages=%d", r.Sampled, r.Examined, r.Messages)
	}
	if r.Bytes != 1020 || r.EstimatedBytes != 900*5/2+50+70 {
		t.Errorf("bytes=%d est=%d", r.Bytes, r.EstimatedBytes)
	}
	var out bytes.Buffer
	r.Write(&out)
	if !strings.Contains(out.String(), "SAMPLED: only the newest 2") || !strings.Contains(out.String(), "ESTIMATE") {
		t.Errorf("a sampled report must say so:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "dedup preview: SKIPPED") {
		t.Errorf("a skipped dedup must say so")
	}
	for _, f := range g.Fetches() {
		if strings.Contains(f, "ENVELOPE") {
			t.Errorf("--skip-dedup still fetched ENVELOPE: %s", f)
		}
	}
}

func TestCensusRefusals(t *testing.T) {
	g := gmailtest.New(t)
	g.AddMailbox(&gmailtest.Mailbox{Name: "INBOX"})
	if _, err := Census(context.Background(), dial(t, g), &fakeCensusStore{}, CensusOptions{}); !errors.Is(err, ErrAllMailHidden) {
		t.Errorf("no \\All: err = %v", err)
	}
	if len(g.Selects()) != 0 {
		t.Errorf("a refused census selected %v", g.Selects())
	}
	g2 := censusFixture(t)
	g2.GmailExt = false
	if _, err := Census(context.Background(), dial(t, g2), &fakeCensusStore{}, CensusOptions{}); !errors.Is(err, ErrNotGmail) {
		t.Errorf("no X-GM-EXT-1: err = %v", err)
	}
}

// A message whose response carries no X-GM-LABELS is NOT counted as an
// unlabelled, archived message (#52 review H1): it is counted on its own.
func TestCensusMissingLabelsAreNoReading(t *testing.T) {
	g := censusFixture(t)
	g.SetLabelFault("[Gmail]/All Mail", 5, gmailtest.LabelsOmitted) // uid 5: no labels anyway
	g.SetLabelFault("[Gmail]/All Mail", 1, gmailtest.LabelsOmitted) // uid 1: \Inbox Work
	r, err := Census(context.Background(), dial(t, g), &fakeCensusStore{}, CensusOptions{SkipDedup: true})
	if err != nil {
		t.Fatal(err)
	}
	if r.NoLabelReading != 2 {
		t.Errorf("NoLabelReading = %d, want 2", r.NoLabelReading)
	}
	if r.Folders["inbox"] != 0 || r.Folders["archive"] != 1 || r.UnlabelledMessages != 2 {
		t.Errorf("missing readings leaked into the counts: folders=%v unlabelled=%d", r.Folders, r.UnlabelledMessages)
	}
}
