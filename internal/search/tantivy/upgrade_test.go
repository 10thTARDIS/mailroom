// SPDX-License-Identifier: AGPL-3.0-only

//go:build tantivy && cgo

package tantivy

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"

	tantivygo "github.com/anyproto/tantivy-go"

	"mailroom/internal/search"
)

// The schema-version bump for from_domain / to_domain (#712), against a REAL
// on-disk index written by the previous schema.
//
// Tantivy refuses to open an index with a schema other than the one it was
// written with. Before this was handled, bumping the schema version could never
// trigger the background rebuild it exists for: the indexer could not even open
// the old index to find out it was stale, so every Sync failed and search stayed
// broken until someone deleted the index by hand. This test is that upgrade, end
// to end.

type upgradeSource struct {
	msg search.Message
	raw []byte
}

func (s *upgradeSource) EmailState(context.Context, ulid.ULID) (int64, error) { return 1, nil }
func (s *upgradeSource) EmailChanges(_ context.Context, _ ulid.ULID, since int64, _ int) (search.Changes, error) {
	return search.Changes{NewState: 1}, nil
}
func (s *upgradeSource) Messages(_ context.Context, _ ulid.ULID, ids []ulid.ULID) ([]search.Message, error) {
	return []search.Message{s.msg}, nil
}
func (s *upgradeSource) MessageIDsAfter(_ context.Context, _ ulid.ULID, after ulid.ULID, _ int) ([]ulid.ULID, error) {
	if after.Compare(s.msg.ID) < 0 {
		return []ulid.ULID{s.msg.ID}, nil
	}
	return nil, nil
}
func (s *upgradeSource) Raw(context.Context, ulid.ULID, []byte) ([]byte, error) { return s.raw, nil }

// writeOldIndex writes a one-message index with an OLDER schema version and the
// metadata file a build of that version would have left beside it. fields are
// the values to index (only fields that version has).
func writeOldIndex(t *testing.T, dir string, version int, id ulid.ULID, fields map[string]string) {
	t.Helper()
	// Library init happens in Open; run it once on a throwaway directory.
	warm, err := Open(context.Background(), t.TempDir())
	if err != nil {
		t.Fatalf("warm up: %v", err)
	}
	_ = warm.Close()

	schema, err := buildSchema(version)
	if err != nil {
		t.Fatal(err)
	}
	tc, err := tantivygo.NewTantivyContextWithSchema(dir, schema)
	if err != nil {
		t.Fatalf("create v%d index: %v", version, err)
	}
	for _, f := range []func() error{
		func() error { return tc.RegisterTextAnalyzerSimple(tokText, textLimit, tantivygo.English) },
		func() error { return tc.RegisterTextAnalyzerRaw(tokRaw) },
		func() error { return tc.RegisterTextAnalyzerNgram(tokNgram, ngramMin, ngramMax, false) },
	} {
		if err := f(); err != nil {
			t.Fatal(err)
		}
	}
	td := tantivygo.NewDocument()
	for field, val := range fields {
		if err := td.AddField(val, tc, field); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := tc.BatchAddAndDeleteDocumentsWithOpstamp([]*tantivygo.Document{td}, string(search.FieldID), []string{id.String()}); err != nil {
		t.Fatal(err)
	}
	if err := tc.Close(); err != nil {
		t.Fatal(err)
	}
	meta := fmt.Sprintf(`{"schema_version":%d,"cursor":1,"updated_at":"%s"}`, version, time.Now().UTC().Format(time.RFC3339))
	if err := os.WriteFile(filepath.Join(dir, "mailroom-index.json"), []byte(meta), 0o600); err != nil {
		t.Fatal(err)
	}
}

// writeV1Index writes a one-message index with the v1 schema (no domain fields).
func writeV1Index(t *testing.T, dir string, msg search.Message) {
	t.Helper()
	writeOldIndex(t, dir, 1, msg.ID, map[string]string{
		string(search.FieldID):      msg.ID.String(),
		string(search.FieldFrom):    msg.FromAddress,
		string(search.FieldSubject): msg.Subject,
		string(search.FieldPeople):  msg.FromAddress,
	})
}

func TestSchemaV1IndexIsServedThenRebuiltWithDomainFields(t *testing.T) {
	ctx := context.Background()
	user := ulid.Make()
	src := &upgradeSource{
		msg: search.Message{
			ID: ulid.Make(), Subject: "Quarterly invoice",
			FromAddress: "orders@mail.shop.example", ReceivedAt: time.Now().UTC(),
		},
		raw: []byte("From: orders@mail.shop.example\r\nSubject: Quarterly invoice\r\n\r\nbody\r\n"),
	}
	root := t.TempDir()
	live := filepath.Join(root, user.String(), "live")
	if err := os.MkdirAll(live, 0o700); err != nil {
		t.Fatal(err)
	}
	writeV1Index(t, live, src.msg)

	// 1. The old index opens (with its own schema), serves what it can, and says
	//    so for what it cannot — an error, never an empty list.
	old, err := Open(ctx, live)
	if err != nil {
		t.Fatalf("a v1 index must still open under the current schema: %v", err)
	}
	res, err := old.Search(ctx, search.Parse("invoice", search.ParseOptions{}))
	if err != nil || len(res.Hits) != 1 {
		t.Fatalf("the old index should still answer ordinary queries: hits=%d err=%v", len(res.Hits), err)
	}
	res, err = old.Search(ctx, search.Parse("from:orders@mail.shop.example", search.ParseOptions{}))
	if err != nil || len(res.Hits) != 1 {
		t.Fatalf("an exact address still works on the old index: hits=%d err=%v", len(res.Hits), err)
	}
	if _, err := old.Search(ctx, search.Parse("from:shop.example", search.ParseOptions{})); !errors.Is(err, search.ErrIndexUpgrading) {
		t.Fatalf("a domain query on a v1 index = %v, want ErrIndexUpgrading", err)
	}
	if err := old.Close(); err != nil {
		t.Fatal(err)
	}

	// 2. The indexer notices the stale version and rebuilds it.
	ix, err := search.NewIndexer(root, Open, src, src)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ix.Close() }()
	prog, err := ix.Sync(ctx, user)
	if err != nil {
		t.Fatalf("Sync over a v1 index: %v", err)
	}
	if !prog.Rebuilt {
		t.Fatal("the schema-version bump did not trigger a rebuild")
	}

	// 3. After the rebuild the domain search finds the mail, subdomain included.
	for _, q := range []string{"from:shop.example", "from:*.shop.example", "from:mail.shop.example"} {
		res, err := ix.Search(ctx, user, search.Parse(q, search.ParseOptions{}))
		if err != nil || res.Total != 1 {
			t.Errorf("%q after the rebuild: total=%d err=%v, want 1", q, res.Total, err)
		}
	}
	if res, err := ix.Search(ctx, user, search.Parse("from:notshop.example", search.ParseOptions{})); err != nil || res.Total != 0 {
		t.Errorf("a lookalike domain matched after the rebuild: total=%d err=%v", res.Total, err)
	}
}

// The v2 -> v3 bump (#722/#723), the same way: a REAL index written with the v2
// schema (domain fields, none of list/size/filename/deliveredto) still opens,
// still answers everything v2 could — domain search included — refuses every
// v3 operator with ErrIndexUpgrading rather than "no results", and is rebuilt
// in the background into one that answers them.
//
// The v3 FLAG atoms are the subtle case: the flag field exists in v2, so a v2
// index would happily run has:pdf and return nothing. RequiredSchemaVersion is
// what makes that an ErrIndexUpgrading too.
func TestSchemaV2IndexIsServedThenRebuiltWithV3Fields(t *testing.T) {
	ctx := context.Background()
	user := ulid.Make()
	src := &upgradeSource{
		msg: search.Message{
			ID: ulid.Make(), Subject: "Quarterly invoice",
			FromAddress: "orders@mail.shop.example", ReceivedAt: time.Now().UTC(),
			Size:             3 << 20,
			ListID:           "<billing.lists.shop.example>",
			ListUnsubscribe:  "<mailto:unsub@shop.example>",
			AddressedAddress: "shop@me.example.net",
			Attachments:      []search.Attachment{{Filename: "invoice-q3.pdf", ContentType: "application/pdf"}},
		},
		raw: []byte("From: orders@mail.shop.example\r\nSubject: Quarterly invoice\r\n\r\nbody\r\n"),
	}
	root := t.TempDir()
	live := filepath.Join(root, user.String(), "live")
	if err := os.MkdirAll(live, 0o700); err != nil {
		t.Fatal(err)
	}
	writeOldIndex(t, live, 2, src.msg.ID, map[string]string{
		string(search.FieldID):         src.msg.ID.String(),
		string(search.FieldFrom):       src.msg.FromAddress,
		string(search.FieldFromDomain): "shop.example",
		string(search.FieldSubject):    src.msg.Subject,
		string(search.FieldPeople):     src.msg.FromAddress,
		string(search.FieldFlag):       search.FlagAttachment,
	})

	v3Queries := []string{
		"list:shop.example", "has:unsubscribe", "has:pdf", "-has:pdf", "filename:invoice",
		"larger:1M", "smaller:10M", "alias:shop@me.example.net", "deliveredto:me.example.net",
		"invoice OR has:pdf",
	}

	// 1. The v2 index opens with its own schema and serves what it can.
	old, err := Open(ctx, live)
	if err != nil {
		t.Fatalf("a v2 index must still open under the v3 schema: %v", err)
	}
	for _, q := range []string{"invoice", "from:shop.example", "has:attachment"} {
		res, err := old.Search(ctx, search.Parse(q, search.ParseOptions{}))
		if err != nil || len(res.Hits) != 1 {
			t.Errorf("the v2 index should still answer %q: hits=%d err=%v", q, len(res.Hits), err)
		}
	}
	for _, q := range v3Queries {
		if _, err := old.Search(ctx, search.Parse(q, search.ParseOptions{})); !errors.Is(err, search.ErrIndexUpgrading) {
			t.Errorf("%q on a v2 index = %v, want ErrIndexUpgrading", q, err)
		}
	}
	// Writing to it (the indexer never does — it rebuilds — but a write must
	// not fail on the fields v2 lacks).
	if err := old.Upsert(ctx, []search.Document{{ID: ulid.Make(), Subject: "x", ListID: "<a.b.example>", Size: 5}}); err != nil {
		t.Errorf("upsert into a v2 index: %v", err)
	}
	if err := old.Close(); err != nil {
		t.Fatal(err)
	}

	// 2. The indexer notices the stale version and rebuilds it.
	ix, err := search.NewIndexer(root, Open, src, src)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ix.Close() }()
	prog, err := ix.Sync(ctx, user)
	if err != nil {
		t.Fatalf("Sync over a v2 index: %v", err)
	}
	if !prog.Rebuilt {
		t.Fatal("the schema-version bump did not trigger a rebuild")
	}

	// 3. After the rebuild every v3 operator answers, and correctly.
	want := map[string]int{
		"list:shop.example": 1, "has:unsubscribe": 1, "has:pdf": 1, "-has:pdf": 0,
		"filename:invoice": 1, "larger:1M": 1, "smaller:10M": 1, "alias:shop@me.example.net": 1,
		"deliveredto:me.example.net": 1, "invoice OR has:pdf": 1,
	}
	for _, q := range v3Queries {
		res, err := ix.Search(ctx, user, search.Parse(q, search.ParseOptions{}))
		if err != nil || res.Total != want[q] {
			t.Errorf("%q after the rebuild: total=%d err=%v, want %d", q, res.Total, err, want[q])
		}
	}
	for _, q := range []string{"larger:4M", "list:other.example", "alias:me.example.org"} {
		if res, err := ix.Search(ctx, user, search.Parse(q, search.ParseOptions{})); err != nil || res.Total != 0 {
			t.Errorf("%q after the rebuild: total=%d err=%v, want 0", q, res.Total, err)
		}
	}
}
