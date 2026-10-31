// SPDX-License-Identifier: AGPL-3.0-only

package consolidate

import (
	"bytes"
	"context"
	"errors"
	"reflect"
	"sort"
	"strings"
	"testing"

	"mailroom/internal/sync"
)

// fakeSession is an in-memory IMAP account: folders, message counts, and a log
// of every state-changing call, so a test can assert both what happened and
// what did NOT.
type fakeSession struct {
	boxes       []sync.Mailbox
	msgs        map[string]uint32
	uidnext     map[string]uint32
	noMove      bool
	leaveBehind map[string]uint32   // MoveAll leaves this many in the source
	statusSeq   map[string][]uint32 // successive STATUS answers, overriding msgs
	writes      []string            // "move A->B", "delete A"
	statusCalls map[string]int
}

func newFake(boxes ...sync.Mailbox) *fakeSession {
	return &fakeSession{
		boxes: boxes, msgs: map[string]uint32{}, uidnext: map[string]uint32{},
		leaveBehind: map[string]uint32{}, statusSeq: map[string][]uint32{}, statusCalls: map[string]int{},
	}
}

func box(name string, attrs ...sync.MailboxAttr) sync.Mailbox {
	return sync.Mailbox{Name: name, Delimiter: '/', Attrs: attrs}
}

func (f *fakeSession) with(name string, n uint32) *fakeSession {
	f.msgs[name] = n
	f.uidnext[name] = n + 1
	return f
}

func (f *fakeSession) ListMailboxes(context.Context) ([]sync.Mailbox, error) {
	return append([]sync.Mailbox(nil), f.boxes...), nil
}

func (f *fakeSession) has(name string) bool {
	for _, b := range f.boxes {
		if b.Name == name {
			return true
		}
	}
	return false
}

func (f *fakeSession) Status(_ context.Context, name string) (sync.FolderStatus, error) {
	if !f.has(name) {
		return sync.FolderStatus{}, errors.New("no such mailbox")
	}
	f.statusCalls[name]++
	n := f.msgs[name]
	if seq := f.statusSeq[name]; len(seq) > 0 {
		n, f.statusSeq[name] = seq[0], seq[1:]
	}
	return sync.FolderStatus{Name: name, Messages: n, UIDNext: sync.UID(f.uidnext[name] + 1), UIDValidity: 7}, nil
}

func (f *fakeSession) MoveAll(_ context.Context, src, dst string) error {
	if f.noMove {
		return errors.New("fake: MOVE called on a server without MOVE")
	}
	f.writes = append(f.writes, "move "+src+"->"+dst)
	left := f.leaveBehind[src]
	moved := f.msgs[src] - left
	f.msgs[dst] += moved
	f.uidnext[dst] += moved
	f.msgs[src] = left
	return nil
}

func (f *fakeSession) DeleteMailbox(_ context.Context, name string) error {
	f.writes = append(f.writes, "delete "+name)
	for i, b := range f.boxes {
		if b.Name == name {
			f.boxes = append(f.boxes[:i], f.boxes[i+1:]...)
			return nil
		}
	}
	return errors.New("no such mailbox")
}

func (f *fakeSession) SupportsMove() bool { return !f.noMove }

// theIssueMailbox is #647's shape in miniature: per-sender folders under
// INBOX, a dated archive under the special-use Archive, and empties.
func theIssueMailbox() *fakeSession {
	return newFake(
		box("INBOX"), box("INBOX/a"), box("INBOX/b"), box("INBOX/c"),
		box("Archive", sync.MailboxAttrArchive), box("Archive/2025", sync.MailboxAttrNoSelect),
		box("Archive/2025/01"), box("Old"), box("Sent", sync.MailboxAttrSent),
		box("Trash", sync.MailboxAttrTrash), box("Trash/x"),
	).with("INBOX", 10).with("INBOX/a", 3).with("INBOX/b", 2).with("INBOX/c", 0).
		with("Archive", 4).with("Archive/2025/01", 2).with("Old", 0).with("Sent", 6).
		with("Trash", 1).with("Trash/x", 0)
}

func plan(t *testing.T, s Session, mr map[string]MailroomCount, o Options) *Plan {
	t.Helper()
	p, err := BuildPlan(context.Background(), s, mr, o)
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}
	return p
}

func actions(fs []*Folder) map[string]string {
	out := map[string]string{}
	for _, f := range fs {
		a := string(f.Action)
		if f.Refusal != nil {
			a += " " + f.Refusal.Class
		}
		out[f.Name] = a
	}
	return out
}

func TestPlanPerFolder(t *testing.T) {
	s := theIssueMailbox()
	p := plan(t, s, map[string]MailroomCount{"INBOX/a": {Live: 3}, "INBOX/b": {Live: 1}}, Options{
		From: "INBOX/*", Into: "INBOX", DeleteEmpty: []string{"INBOX/*", "Old", "Trash/*"},
	})
	if len(s.writes) != 0 {
		t.Fatalf("BuildPlan WROTE to the server: %v", s.writes)
	}
	if got, want := actions(p.Moves), map[string]string{
		"INBOX/a": "move", "INBOX/b": "move", "INBOX/c": "nothing to move",
	}; !reflect.DeepEqual(got, want) {
		t.Errorf("moves = %v, want %v", got, want)
	}
	if got, want := actions(p.Deletes), map[string]string{
		"INBOX/a": "delete after its move", "INBOX/b": "delete after its move",
		"INBOX/c": "delete", "Old": "delete", "Trash/x": "delete",
	}; !reflect.DeepEqual(got, want) {
		t.Errorf("deletes = %v, want %v", got, want)
	}
	tot := p.Totals()
	if tot.MoveFolders != 2 || tot.MoveMessages != 5 || tot.NothingToMove != 1 || tot.DeleteNow != 3 ||
		tot.DeleteAfterMove != 2 || tot.RefusedMoves != 0 || tot.RefusedDeletes != 0 {
		t.Errorf("totals = %+v", tot)
	}
	// INBOX/b: server 2, Mailroom 1 -> differs; INBOX/c, Old, Trash/x: no row.
	if tot.MailroomDiffers != 1 || tot.MailroomMissing != 3 {
		t.Errorf("Mailroom columns: differs %d (want 1), missing %d (want 3)", tot.MailroomDiffers, tot.MailroomMissing)
	}

	var buf bytes.Buffer
	if err := WriteReport(&buf, p); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"move 3", "move 2", "nothing to move", "2 folders", "5 messages", "INBOX would go from 10 to 15"} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("report lacks %q:\n%s", want, buf.String())
		}
	}
}

func TestArchiveFlattenPlan(t *testing.T) {
	s := theIssueMailbox()
	p := plan(t, s, nil, Options{From: "Archive/*", Into: "Archive", DeleteEmpty: []string{"Archive/*"}})
	if got, want := actions(p.Moves), map[string]string{
		"Archive/2025": "not selectable", "Archive/2025/01": "move",
	}; !reflect.DeepEqual(got, want) {
		t.Errorf("moves = %v, want %v", got, want)
	}
	// Leaves before parents.
	var order []string
	for _, f := range p.Deletes {
		order = append(order, f.Name+"="+string(f.Action))
	}
	if want := []string{"Archive/2025/01=delete after its move", "Archive/2025=delete"}; !reflect.DeepEqual(order, want) {
		t.Errorf("delete order = %v, want %v", order, want)
	}
}

// Every refusal, each proven to fire in the plan AND to hold in Apply: a
// refused folder is never moved or deleted.
func TestRefusals(t *testing.T) {
	for _, tc := range []struct {
		name     string
		fake     func() *fakeSession
		mailroom map[string]MailroomCount
		opts     Options
		folder   string
		deletion bool
		class    string
	}{
		{"special-use source", theIssueMailbox, nil,
			Options{From: "Sent", Into: "INBOX"}, "Sent", false, RefuseSpecialUse},
		{"special-use deletion", func() *fakeSession {
			// No Trash/x here: with a child, has-children would refuse Trash too
			// and this case could not tell which guard held.
			return newFake(box("INBOX"), box("Trash", sync.MailboxAttrTrash)).with("Trash", 0)
		}, nil,
			Options{DeleteEmpty: []string{"Trash"}}, "Trash", true, RefuseSpecialUse},
		{"INBOX source", theIssueMailbox, nil,
			Options{From: "INBOX", Into: "Old"}, "INBOX", false, RefuseInbox},
		{"INBOX deletion", func() *fakeSession { return theIssueMailbox().with("INBOX", 0) }, nil,
			Options{DeleteEmpty: []string{"inbox"}}, "INBOX", true, RefuseInbox},
		{"Mailroom role on a provider without SPECIAL-USE",
			func() *fakeSession { return newFake(box("INBOX"), box("Sent Items")).with("Sent Items", 4) },
			map[string]MailroomCount{"Sent Items": {Live: 4, Role: "sent"}},
			Options{From: "Sent Items", Into: "INBOX"}, "Sent Items", false, RefuseMailroomRole},
		{"source is the destination", theIssueMailbox, nil,
			Options{From: "INBOX/a", Into: "INBOX/a"}, "INBOX/a", false, RefuseIsDestination},
		{"source contains the destination",
			func() *fakeSession {
				return newFake(box("INBOX"), box("Projects"), box("Projects/All")).with("Projects", 5).with("Projects/All", 1)
			}, nil,
			Options{From: "Projects", Into: "Projects/All"}, "Projects", false, RefuseContainsDestination},
		{"parent with a child that is not being deleted",
			func() *fakeSession {
				return newFake(box("INBOX"), box("Parent"), box("Parent/kid")).with("Parent", 0).with("Parent/kid", 0)
			}, nil,
			Options{DeleteEmpty: []string{"Parent"}}, "Parent", true, RefuseHasChildren},
		{"destination does not exist", theIssueMailbox, nil,
			Options{From: "INBOX/*", Into: "Nowhere"}, "INBOX/a", false, RefuseDestination},
		{"destination is \\Noselect", theIssueMailbox, nil,
			Options{From: "INBOX/*", Into: "Archive/2025"}, "INBOX/a", false, RefuseDestination},
		{"server without MOVE", func() *fakeSession { f := theIssueMailbox(); f.noMove = true; return f }, nil,
			Options{From: "INBOX/*", Into: "INBOX"}, "INBOX/a", false, RefuseNoMove},
		{"deletion target that is not empty", func() *fakeSession { return theIssueMailbox().with("Old", 4) }, nil,
			Options{DeleteEmpty: []string{"Old"}}, "Old", true, RefuseNotEmpty},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := tc.fake()
			p := plan(t, s, tc.mailroom, tc.opts)
			list := p.Moves
			if tc.deletion {
				list = p.Deletes
			}
			var got *Folder
			for _, f := range list {
				if f.Name == tc.folder {
					got = f
				}
			}
			if got == nil {
				t.Fatalf("%s is not in the plan at all: moves %v deletes %v", tc.folder, actions(p.Moves), actions(p.Deletes))
			}
			if got.Action != ActionRefuse || got.Refusal == nil || got.Refusal.Class != tc.class {
				t.Fatalf("%s planned as %q (%v), want REFUSE %s", tc.folder, got.Action, got.Refusal, tc.class)
			}
			var out bytes.Buffer
			if _, err := Apply(context.Background(), s, p, &out); err != nil {
				t.Fatalf("Apply: %v", err)
			}
			for _, w := range s.writes {
				if strings.HasSuffix(w, " "+tc.folder) || strings.HasPrefix(w, "move "+tc.folder+"->") {
					t.Errorf("Apply acted on a REFUSED folder: %s (all writes: %v)", w, s.writes)
				}
			}
		})
	}
}

// A parent whose children are all being deleted is deleted too — after them.
func TestParentDeletedAfterItsChildren(t *testing.T) {
	s := newFake(box("INBOX"), box("Parent"), box("Parent/kid"), box("Parent/kid/grandkid")).
		with("Parent", 0).with("Parent/kid", 0).with("Parent/kid/grandkid", 0)
	p := plan(t, s, nil, Options{DeleteEmpty: []string{"Parent*"}})
	if _, err := Apply(context.Background(), s, p, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	want := []string{"delete Parent/kid/grandkid", "delete Parent/kid", "delete Parent"}
	if !reflect.DeepEqual(s.writes, want) {
		t.Errorf("writes = %v, want %v", s.writes, want)
	}
}

func TestApplyEndToEndAndRerunIsNoop(t *testing.T) {
	s := theIssueMailbox()
	o := Options{From: "INBOX/*", Into: "INBOX", DeleteEmpty: []string{"INBOX/*", "Old", "Trash/*"}}
	var out bytes.Buffer
	res, err := Apply(context.Background(), s, plan(t, s, nil, o), &out)
	if err != nil {
		t.Fatalf("Apply: %v\n%s", err, out.String())
	}
	if s.msgs["INBOX"] != 15 || res.MovedMessages != 5 {
		t.Errorf("INBOX = %d, moved %d; want 15 and 5", s.msgs["INBOX"], res.MovedMessages)
	}
	// Every move before any delete.
	firstDelete := -1
	for i, w := range s.writes {
		if strings.HasPrefix(w, "delete ") && firstDelete < 0 {
			firstDelete = i
		}
		if strings.HasPrefix(w, "move ") && firstDelete >= 0 {
			t.Errorf("a move (%s) ran after a delete: %v", w, s.writes)
		}
	}
	sort.Strings(res.Deleted)
	if want := []string{"INBOX/a", "INBOX/b", "INBOX/c", "Old", "Trash/x"}; !reflect.DeepEqual(res.Deleted, want) {
		t.Errorf("deleted = %v, want %v", res.Deleted, want)
	}
	if s.msgs["Sent"] != 6 || s.msgs["Trash"] != 1 || s.msgs["Archive"] != 4 || !s.has("Trash") || !s.has("Sent") {
		t.Errorf("special-use folders were touched: %v", s.msgs)
	}

	before := len(s.writes)
	p := plan(t, s, nil, o)
	tot := p.Totals()
	if n := tot.MoveFolders + tot.DeleteNow + tot.DeleteAfterMove; n != 0 {
		t.Errorf("a re-run plans %d operations, want 0: %+v", n, tot)
	}
	var buf bytes.Buffer
	_ = WriteReport(&buf, p)
	if !strings.Contains(buf.String(), "Nothing to do") {
		t.Errorf("re-run report does not say nothing to do:\n%s", buf.String())
	}
	if _, err := Apply(context.Background(), s, p, &out); err != nil || len(s.writes) != before {
		t.Errorf("re-run Apply wrote %v (err %v)", s.writes[before:], err)
	}
}

// THE POST-MOVE VERIFICATION. The fake's MOVE leaves one message behind in
// INBOX/a: Apply must stop there, say which folder and what the numbers are,
// and do nothing further — no second move, no delete.
func TestApplyStopsWhenAMoveLeavesMailBehind(t *testing.T) {
	s := theIssueMailbox()
	s.leaveBehind["INBOX/a"] = 1
	p := plan(t, s, nil, Options{From: "INBOX/*", Into: "INBOX", DeleteEmpty: []string{"INBOX/*", "Old"}})
	res, err := Apply(context.Background(), s, p, &bytes.Buffer{})
	var stop *StopError
	if !errors.As(err, &stop) {
		t.Fatalf("Apply returned %v, want a StopError (writes: %v)", err, s.writes)
	}
	if stop.Folder != "INBOX/a" || !strings.Contains(err.Error(), "source holds 1 (want 0)") ||
		!strings.Contains(err.Error(), "holds 12 (want 13") {
		t.Errorf("stop names the wrong folder or numbers: %v", err)
	}
	if want := []string{"move INBOX/a->INBOX"}; !reflect.DeepEqual(s.writes, want) {
		t.Errorf("writes after the stop = %v, want only %v", s.writes, want)
	}
	if len(res.MovedFolders) != 0 || len(res.Deleted) != 0 {
		t.Errorf("result claims work it did not verify: %+v", res)
	}
}

// THE DELETE RE-READ. Old is empty when planned, and a message arrives before
// the delete: STATUS immediately before DELETE reads 1, so Old stays.
func TestApplyDeletesOnlyAtZero(t *testing.T) {
	s := theIssueMailbox()
	p := plan(t, s, nil, Options{DeleteEmpty: []string{"Old", "Trash/*"}})
	if actions(p.Deletes)["Old"] != "delete" {
		t.Fatalf("precondition: Old planned as %q", actions(p.Deletes)["Old"])
	}
	s.statusSeq["Old"] = []uint32{1}
	res, err := Apply(context.Background(), s, p, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	for _, w := range s.writes {
		if w == "delete Old" {
			t.Fatalf("DELETED a folder whose STATUS read 1 immediately before: %v", s.writes)
		}
	}
	if len(res.NotDeleted) != 1 || !strings.HasPrefix(res.NotDeleted[0], "Old:") {
		t.Errorf("NotDeleted = %v, want Old", res.NotDeleted)
	}
	if !reflect.DeepEqual(res.Deleted, []string{"Trash/x"}) {
		t.Errorf("the other deletion should proceed: %v", res.Deleted)
	}
}

func TestMaxFoldersBoundsARunAndARerunContinues(t *testing.T) {
	s := theIssueMailbox()
	o := Options{From: "INBOX/*", Into: "INBOX", DeleteEmpty: []string{"INBOX/*"}, MaxFolders: 1}
	res, err := Apply(context.Background(), s, plan(t, s, nil, o), &bytes.Buffer{})
	if err != nil || !res.Bounded || !reflect.DeepEqual(s.writes, []string{"move INBOX/a->INBOX"}) {
		t.Fatalf("first run: bounded %v, err %v, writes %v", res.Bounded, err, s.writes)
	}
	res, err = Apply(context.Background(), s, plan(t, s, nil, o), &bytes.Buffer{})
	if err != nil || !res.Bounded || s.writes[1] != "move INBOX/b->INBOX" || len(s.writes) != 2 {
		t.Fatalf("second run: bounded %v, err %v, writes %v", res.Bounded, err, s.writes)
	}
	for i := 0; i < 5; i++ {
		if _, err := Apply(context.Background(), s, plan(t, s, nil, o), &bytes.Buffer{}); err != nil {
			t.Fatal(err)
		}
	}
	if s.has("INBOX/a") || s.has("INBOX/b") || s.has("INBOX/c") || s.msgs["INBOX"] != 15 {
		t.Errorf("repeated bounded runs did not converge: writes %v", s.writes)
	}
}

func TestMatch(t *testing.T) {
	for _, tc := range []struct {
		pattern, name string
		want          bool
	}{
		{"INBOX/*", "INBOX/a", true},
		{"INBOX/*", "INBOX/a/b", true},
		{"INBOX/*", "INBOX", false},
		{"inbox/*", "INBOX/a", true},
		{"INBOX/%", "INBOX/a", true},
		{"INBOX/%", "INBOX/a/b", false},
		{"Archive/*", "Archive", false},
		{"Archive/*", "Archive/2025/01", true},
		{"Trash/*", "Trash/x", true},
		{"Trash/*", "Trashcan/x", false},
		{"Old", "Old", true},
		{"Old", "old", false},
		{"*", "anything/at/all", true},
		{"%", "top", true},
		{"%", "top/sub", false},
		{"Inboxes/*", "Inboxes/a", true},
	} {
		if got := Match(tc.pattern, tc.name, '/'); got != tc.want {
			t.Errorf("Match(%q, %q) = %v, want %v", tc.pattern, tc.name, got, tc.want)
		}
	}
}

func TestValidate(t *testing.T) {
	for _, o := range []Options{
		{},
		{From: "INBOX/*"},
		{DeleteEmpty: []string{""}},
		{DeleteEmpty: []string{"x"}, MaxFolders: -1},
	} {
		if o.Validate() == nil {
			t.Errorf("%+v validated", o)
		}
	}
}
