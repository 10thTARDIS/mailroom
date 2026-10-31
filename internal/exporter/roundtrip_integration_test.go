// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package exporter_test

// The gate #54 names for the exporter: export, read it back through the REAL
// import reader, and assert the message identities are unchanged.
//
// ⚠ WHY IT ASSERTS ON content_hash AND NOT ON "the files look right". Identity
// in this system IS the SHA-256 of the raw RFC 5322 bytes (ADR-001). An export
// that changed one byte — a CRLF normalised, a trailing newline added, an mbox
// "From " line unstuffed wrongly — would produce files that open fine in a mail
// client and are a DIFFERENT MESSAGE to this system: re-importing them would
// duplicate rather than deduplicate. Byte-exactness is not a quality bar here,
// it is the correctness condition, and the hash is the only thing that tests it.
//
// ⚠ AND IT READS THE ARCHIVE WITH internal/importer, not with its own directory
// walk. The property under test is that the two halves AGREE — that what the
// exporter writes is what the importer understands, including maildir detection
// and the flag letters in the filename. A test that re-implemented the reader
// would be testing this file's idea of maildir against itself.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/oklog/ulid/v2"

	"mailroom/internal/blob"
	"mailroom/internal/crypto"
	"mailroom/internal/exporter"
	"mailroom/internal/exportplan"
	"mailroom/internal/importer"
	"mailroom/internal/store"
	"mailroom/internal/syncrunner"
)

const blobPathInfo = "mailroom-blob-path-v1"

// TestExportRoundTripsByteExactThroughTheImportReader is the gate.
func TestExportRoundTripsByteExactThroughTheImportReader(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	// Four messages with deliberately awkward bytes and a distinct flag set
	// each, so a format that mangles content or loses a flag cannot pass.
	seeds := []struct {
		label                   string
		raw                     []byte
		seen, flagged, answered bool
	}{
		{label: "plain",
			raw: []byte("Subject: plain\r\nMessage-ID: <a@example.com>\r\n\r\nhello\r\n")},
		// ⚠ A LINE THAT LOOKS LIKE AN mbox SEPARATOR. In an mbox export this
		// would be escaped to ">From " and, unless unescaped identically coming
		// back, the hash changes — and with it the message's identity. Maildir
		// applies no such transformation; this is what asserts that.
		{label: "mbox-ish", seen: true,
			raw: []byte("Subject: mboxish\r\nMessage-ID: <b@example.com>\r\n\r\n" +
				"From here to there\r\n>From already escaped\r\n")},
		// Bare LF among CRLF, and a final line with no terminator at all.
		{label: "mixed-endings", seen: true, flagged: true,
			raw: []byte("Subject: mixed\r\nMessage-ID: <c@example.com>\r\n\r\nline\nline\r\nno trailing newline")},
		// Eight-bit and NUL bytes in the body.
		{label: "binary-ish", answered: true,
			raw: append([]byte("Subject: bytes\r\nMessage-ID: <d@example.com>\r\n\r\n"),
				0x00, 0xff, 0xfe, '\r', '\n')},
	}

	want := map[string]seedFacts{}
	for _, s := range seeds {
		h.seedMessage(t, s.raw, s.seen, s.flagged, s.answered)
		sum := sha256.Sum256(s.raw)
		want[hex.EncodeToString(sum[:])] = seedFacts{
			raw: s.raw, seen: s.seen, flagged: s.flagged, answered: s.answered, label: s.label,
		}
	}

	out := filepath.Join(t.TempDir(), "archive")
	res, err := exporter.Export(ctx, h.st, h.blobs, h.dek, exporter.Options{
		UserID: h.userID, OutDir: out,
	})
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	if len(res.Unreadable) > 0 {
		t.Fatalf("export reported %d unreadable messages: %+v", len(res.Unreadable), res.Unreadable)
	}
	if res.Messages != int64(len(seeds)) {
		t.Fatalf("exported %d messages, seeded %d", res.Messages, len(seeds))
	}

	// The archive must be recognised by the import path's OWN detector, not by
	// this test's opinion of what a maildir looks like.
	root := filepath.Join(out, "maildir")
	kind, err := importer.Detect(root)
	if err != nil {
		t.Fatalf("the exported archive is not a recognisable archive: %v", err)
	}
	if kind != importer.KindMaildir {
		t.Fatalf("exported archive detected as %q, want maildir", kind)
	}

	src, err := importer.Open(root, kind, 64*1024*1024)
	if err != nil {
		t.Fatalf("open exported archive: %v", err)
	}
	defer func() { _ = src.Close() }()

	got := map[string]bool{}
	for {
		item, err := src.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("read exported archive: %v", err)
		}
		sum := sha256.Sum256(item.Raw)
		hash := hex.EncodeToString(sum[:])
		facts, ok := want[hash]
		if !ok {
			t.Errorf("the archive holds a message hashing to %s, which was never seeded — "+
				"the exported bytes are not the stored bytes", hash)
			continue
		}
		got[hash] = true
		if !bytes.Equal(item.Raw, facts.raw) {
			t.Errorf("%s: bytes differ despite a matching hash", facts.label)
		}
		assertFlags(t, facts, item.Flags)
	}

	for hash, facts := range want {
		if !got[hash] {
			t.Errorf("%s (%s) did not come back out of the archive", facts.label, hash)
		}
	}
}

// TestExportRefusesANonEmptyDirectory: merging into a populated directory would
// produce an archive nobody could describe.
func TestExportRefusesANonEmptyDirectory(t *testing.T) {
	h := newHarness(t)
	out := t.TempDir()
	if err := os.WriteFile(filepath.Join(out, "somebody-elses-file"), []byte("x"), 0o600); err != nil {
		t.Fatalf("seed dir: %v", err)
	}
	if _, err := exporter.Export(context.Background(), h.st, h.blobs, h.dek,
		exporter.Options{UserID: h.userID, OutDir: out}); err == nil {
		t.Fatal("export into a non-empty directory succeeded; it must refuse")
	}
}

// TestExportReportsAMessageWhoseBytesAreMissing is the negative control for the
// whole command.
//
// ⚠ A message row whose blob is gone must be REPORTED. An archive silently
// missing a message, with a successful exit, is the failure this project keeps
// paying for — and the row-with-no-bytes shape is exactly what #316 produced for
// real.
func TestExportReportsAMessageWhoseBytesAreMissing(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	h.seedMessage(t, []byte("Subject: fine\r\nMessage-ID: <ok@example.com>\r\n\r\nbody\r\n"),
		true, false, false)
	h.seedRowWithoutBlob(t, []byte("Subject: gone\r\nMessage-ID: <gone@example.com>\r\n\r\nbody\r\n"))

	out := filepath.Join(t.TempDir(), "archive")
	res, err := exporter.Export(ctx, h.st, h.blobs, h.dek,
		exporter.Options{UserID: h.userID, OutDir: out})
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	if res.Messages != 1 {
		t.Errorf("wrote %d messages, want 1 (the readable one)", res.Messages)
	}
	if len(res.Unreadable) != 1 {
		t.Fatalf("reported %d unreadable, want 1 — a message whose bytes are gone must "+
			"be named, not silently omitted", len(res.Unreadable))
	}
	// The archive must describe its own incompleteness to someone who was not
	// watching the console.
	b, err := os.ReadFile(filepath.Join(out, "manifest.json"))
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	if !bytes.Contains(b, []byte(res.Unreadable[0].MessageID)) {
		t.Error("manifest.json does not name the message that could not be exported")
	}
}

type seedFacts struct {
	raw                     []byte
	seen, flagged, answered bool
	label                   string
}

func assertFlags(t *testing.T, f seedFacts, flags []string) {
	t.Helper()
	has := func(tok string) bool {
		for _, g := range flags {
			if g == tok {
				return true
			}
		}
		return false
	}
	for _, c := range []struct {
		want bool
		tok  string
	}{
		{f.seen, `\Seen`}, {f.flagged, `\Flagged`}, {f.answered, `\Answered`},
	} {
		if has(c.tok) != c.want {
			t.Errorf("%s: flag %s present=%v, want %v (flags=%v) — maildir carries these "+
				"in the filename, so this is the format failing to round-trip read state",
				f.label, c.tok, has(c.tok), c.want, flags)
		}
	}
}

// --- harness -----------------------------------------------------------------

type harness struct {
	st     *store.Store
	super  *pgxpool.Pool
	blobs  blob.BlobStore
	dek    []byte
	userID ulid.ULID
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	ctx := context.Background()

	superURL := os.Getenv("MAILROOM_TEST_SUPER_URL")
	appURL := os.Getenv("MAILROOM_TEST_APP_URL")
	if superURL == "" || appURL == "" {
		t.Skip("integration database not configured — run via scripts/integration-test.sh")
	}

	st, err := store.Open(ctx, appURL)
	if err != nil {
		t.Fatalf("open app store: %v", err)
	}
	t.Cleanup(st.Close)

	super, err := pgxpool.New(ctx, superURL)
	if err != nil {
		t.Fatalf("open super pool: %v", err)
	}
	t.Cleanup(super.Close)

	// A REAL blob store over a temp dir, with real encryption: the exporter's
	// whole job is getting bytes back out of one, so a fake would test nothing.
	master := bytes.Repeat([]byte{0x5a}, crypto.KeySize)
	pathKey, err := crypto.DeriveKey(master, nil, blobPathInfo)
	if err != nil {
		t.Fatalf("derive path key: %v", err)
	}
	blobs, err := blob.NewFSBlobStore(t.TempDir(), pathKey)
	if err != nil {
		t.Fatalf("blob store: %v", err)
	}

	h := &harness{st: st, super: super, blobs: blobs}
	h.userID = store.NewULID()
	tag := h.userID.String()[:10]
	if _, err := super.Exec(ctx,
		`INSERT INTO app.users (id, oidc_subject, email, display_name)
		 VALUES ($1, $2, $3, 'Export Probe')`,
		h.userID.String(), "export-"+tag, "export-"+tag+"@example.com"); err != nil {
		t.Fatalf("provision export user: %v", err)
	}
	t.Cleanup(func() {
		_, _ = super.Exec(context.Background(),
			`DELETE FROM app.users WHERE id = $1`, h.userID.String())
	})

	// The SAME derivation production uses, via the same constant — a test that
	// invented its own label would still pass while proving nothing about the
	// key real mail is written under.
	dek, err := crypto.DeriveKey(master, h.userID[:], syncrunner.MailBlobInfoV1)
	if err != nil {
		t.Fatalf("derive mail dek: %v", err)
	}
	h.dek = dek
	return h
}

// seedMessage stores real encrypted bytes AND the rows that point at them.
func (h *harness) seedMessage(t *testing.T, raw []byte, seen, flagged, answered bool) {
	t.Helper()
	hash, err := h.blobs.Put(context.Background(), h.userID, h.dek, bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("put blob: %v", err)
	}
	h.insertRows(t, raw, hash[:], seen, flagged, answered)
}

// seedRowWithoutBlob inserts the rows and NO bytes: the #316 shape.
func (h *harness) seedRowWithoutBlob(t *testing.T, raw []byte) {
	t.Helper()
	sum := sha256.Sum256(raw)
	h.insertRows(t, raw, sum[:], false, false, false)
}

func (h *harness) insertRows(t *testing.T, raw, hash []byte, seen, flagged, answered bool) {
	h.insertRowsReturningID(t, raw, hash, seen, flagged, answered)
}

func (h *harness) insertRowsReturningID(t *testing.T, raw, hash []byte, seen, flagged, answered bool) string {
	t.Helper()
	ctx := context.Background()
	id := store.NewULID()

	if _, err := h.super.Exec(ctx,
		`INSERT INTO app.blobs (user_id, content_hash, size_plain, size_stored, dek_id)
		 VALUES ($1, $2, $3, $4, $5) ON CONFLICT DO NOTHING`,
		h.userID.String(), hash, len(raw), len(raw)+64, store.NewULID().String()); err != nil {
		t.Fatalf("insert blob row: %v", err)
	}
	if _, err := h.super.Exec(ctx,
		`INSERT INTO app.messages (id, user_id, content_hash, subject, from_address,
		    received_at, size_bytes, message_id_hint, is_seen, is_flagged, is_answered)
		 VALUES ($1, $2, $3, 'probe', 'p@example.com', now(), $4, $5, $6, $7, $8)`,
		id.String(), h.userID.String(), hash, len(raw),
		fmt.Sprintf("<%s@example.com>", id), seen, flagged, answered); err != nil {
		t.Fatalf("insert message row: %v", err)
	}
	return id.String()
}

// TestSidecarCarriesLabelsWithProvenanceAndTheStateMaildirCannot.
//
// ⚠ #54 NAMES THIS LOSS BY NAME: "label provenance dies if the sidecar ignores
// it". A label the USER filed and a label a RULE filed look identical once
// applied_by is dropped, and the user can no longer tell their own filing from
// automation's. So the assertion is not "labels are present" — it is that the
// two are DISTINGUISHABLE, and that the rule is named rather than pointed at by
// an id that means nothing outside this deployment.
func TestSidecarCarriesLabelsWithProvenanceAndTheStateMaildirCannot(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	raw := []byte("Subject: filed\r\nMessage-ID: <sidecar@example.com>\r\n\r\nbody\r\n")
	msgID := h.seedMessageReturningID(t, raw, true, false, false)
	h.setKeywordsAndJunk(t, msgID, []string{"$Forwarded", "NonJunk"}, true)

	userLabel := h.seedLabel(t, "hand-filed")
	ruleLabel := h.seedLabel(t, "rule-filed")
	ruleID := h.seedRule(t, "the-invoices-rule")
	h.applyLabel(t, msgID, userLabel, "user", "")
	h.applyLabel(t, msgID, ruleLabel, "rule", ruleID)
	h.seedScore(t, msgID, 0.75, "probe-model-v3")

	out := filepath.Join(t.TempDir(), "archive")
	res, err := exporter.Export(ctx, h.st, h.blobs, h.dek,
		exporter.Options{UserID: h.userID, OutDir: out})
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	if res.Messages != 1 {
		t.Fatalf("exported %d messages, want 1", res.Messages)
	}

	line := readOnlySidecarLine(t, filepath.Join(out, "messages.jsonl"))

	sum := sha256.Sum256(raw)
	if line.ContentHash != hex.EncodeToString(sum[:]) {
		t.Errorf("sidecar content_hash = %s, want %s", line.ContentHash, hex.EncodeToString(sum[:]))
	}
	if line.File == "" {
		t.Error("sidecar line names no maildir file")
	}

	byName := map[string]sidecarLabelRef{}
	for _, l := range line.Labels {
		byName[l.Name] = l
	}
	if len(byName) != 2 {
		t.Fatalf("sidecar carries %d labels, want 2: %+v", len(byName), line.Labels)
	}
	if got := byName["hand-filed"].AppliedBy; got != "user" {
		t.Errorf("hand-filed applied_by = %q, want \"user\"", got)
	}
	if got := byName["rule-filed"].AppliedBy; got != "rule" {
		t.Errorf("rule-filed applied_by = %q, want \"rule\"", got)
	}
	// ⚠ The distinguishing assertion. Both labels are present either way; only
	// this separates "the user filed it" from "a rule did".
	if byName["hand-filed"].AppliedBy == byName["rule-filed"].AppliedBy {
		t.Error("both labels report the same applied_by — provenance is not carried, " +
			"and the user cannot tell their own filing from a rule's")
	}
	if got := byName["rule-filed"].RuleName; got != "the-invoices-rule" {
		t.Errorf("rule-filed rule_name = %q, want the rule's NAME", got)
	}
	if byName["hand-filed"].RuleName != "" {
		t.Errorf("a hand-filed label names a rule (%q)", byName["hand-filed"].RuleName)
	}

	// The state maildir's info section has no vocabulary for.
	if len(line.Keywords) != 2 {
		t.Errorf("sidecar keywords = %v, want the two seeded", line.Keywords)
	}
	if !line.Junk {
		t.Error("is_junk not carried; maildir has no letter for it, so the sidecar is the only place it can go")
	}
	if line.Score == nil {
		t.Fatal("no score in the sidecar")
	}
	if *line.Score < 0.74 || *line.Score > 0.76 {
		t.Errorf("score = %v, want ~0.75", *line.Score)
	}
	if line.ScoreModel != "probe-model-v3" {
		t.Errorf("score_model = %q, want the seeded model version", line.ScoreModel)
	}

	// labels.json must carry both labels, by name, with the keyword atom that
	// explains the MailroomLabel-* keywords a reader finds on their own mail.
	b, err := os.ReadFile(filepath.Join(out, "labels.json"))
	if err != nil {
		t.Fatalf("read labels.json: %v", err)
	}
	var labels []struct {
		Name        string `json:"name"`
		KeywordAtom string `json:"keyword_atom"`
	}
	if err := json.Unmarshal(b, &labels); err != nil {
		t.Fatalf("parse labels.json: %v", err)
	}
	seen := map[string]string{}
	for _, l := range labels {
		seen[l.Name] = l.KeywordAtom
	}
	for _, want := range []string{"hand-filed", "rule-filed"} {
		if _, ok := seen[want]; !ok {
			t.Errorf("labels.json omits %q (has %v)", want, seen)
		}
	}
	if res.Labels != len(labels) {
		t.Errorf("result reports %d labels, labels.json holds %d", res.Labels, len(labels))
	}

	// ⚠ NO INTERNAL IDS ANYWHERE. An archive full of ULIDs would look complete
	// and be useless in another deployment. This is a negative control over the
	// whole sidecar, not over one field.
	for _, f := range []string{"messages.jsonl", "labels.json"} {
		raw, err := os.ReadFile(filepath.Join(out, f))
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		for _, id := range []string{msgID, userLabel, ruleLabel, ruleID} {
			if bytes.Contains(raw, []byte(id)) {
				t.Errorf("%s contains the internal id %s — ids are meaningless in "+
					"another deployment and must not be exported", f, id)
			}
		}
	}
}

type sidecarLabelRef struct {
	Name      string `json:"name"`
	AppliedBy string `json:"applied_by"`
	RuleName  string `json:"rule_name"`
}

type sidecarLine struct {
	ContentHash string            `json:"content_hash"`
	File        string            `json:"file"`
	Labels      []sidecarLabelRef `json:"labels"`
	Keywords    []string          `json:"keywords"`
	Junk        bool              `json:"junk"`
	Score       *float32          `json:"score"`
	ScoreModel  string            `json:"score_model"`
}

// readOnlySidecarLine reads messages.jsonl and requires exactly one line.
func readOnlySidecarLine(t *testing.T, path string) sidecarLine {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read messages.jsonl: %v", err)
	}
	lines := bytes.Split(bytes.TrimSpace(b), []byte("\n"))
	if len(lines) != 1 {
		t.Fatalf("messages.jsonl has %d lines, want 1", len(lines))
	}
	var out sidecarLine
	if err := json.Unmarshal(lines[0], &out); err != nil {
		t.Fatalf("parse messages.jsonl: %v", err)
	}
	return out
}

// --- sidecar fixtures ---------------------------------------------------------

func (h *harness) seedMessageReturningID(t *testing.T, raw []byte, seen, flagged, answered bool) string {
	t.Helper()
	hash, err := h.blobs.Put(context.Background(), h.userID, h.dek, bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("put blob: %v", err)
	}
	return h.insertRowsReturningID(t, raw, hash[:], seen, flagged, answered)
}

func (h *harness) setKeywordsAndJunk(t *testing.T, msgID string, kw []string, junk bool) {
	t.Helper()
	if _, err := h.super.Exec(context.Background(),
		`UPDATE app.messages SET keywords = $2, is_junk = $3 WHERE id = $1`,
		msgID, kw, junk); err != nil {
		t.Fatalf("set keywords: %v", err)
	}
}

func (h *harness) seedLabel(t *testing.T, name string) string {
	t.Helper()
	id := store.NewULID().String()
	if _, err := h.super.Exec(context.Background(),
		`INSERT INTO app.labels (id, user_id, name) VALUES ($1, $2, $3)`,
		id, h.userID.String(), name); err != nil {
		t.Fatalf("seed label %q: %v", name, err)
	}
	if _, err := h.super.Exec(context.Background(),
		`INSERT INTO app.label_keyword_map (user_id, label_id, keyword_atom)
		 VALUES ($1, $2, $3)`,
		h.userID.String(), id, "MailroomLabel-"+name); err != nil {
		t.Fatalf("seed keyword atom: %v", err)
	}
	return id
}

// seedRule creates a rule whose action REFERENCES A LABEL BY ID, which is what a
// real rule holds (`{"type":"addLabel","label":"01K…"}`).
//
// ⚠ THE FIXTURE USED TO PASS actions='[]' AND THAT IS WHY THE LEAK SHIPPED PAST
// the negative control: an action list with no reference in it cannot leak one.
// The live run found 12 dangling label ids in rules.json; this is the fixture
// that would have caught them.
func (h *harness) seedRule(t *testing.T, name string) string {
	return h.seedRuleWithLabel(t, name, "")
}

func (h *harness) seedRuleWithLabel(t *testing.T, name, labelID string) string {
	t.Helper()
	id := store.NewULID().String()
	actions := `[]`
	if labelID != "" {
		actions = fmt.Sprintf(`[{"type":"addLabel","label":%q}]`, labelID)
	}
	if _, err := h.super.Exec(context.Background(),
		`INSERT INTO app.rules (id, user_id, name, conditions, actions)
		 VALUES ($1, $2, $3,
		         '{"all":[{"op":"equals","field":"from","value":"probe@example.com"}]}'::jsonb,
		         $4::jsonb)`,
		id, h.userID.String(), name, actions); err != nil {
		t.Fatalf("seed rule %q: %v", name, err)
	}
	return id
}

func (h *harness) applyLabel(t *testing.T, msgID, labelID, by, ruleID string) {
	t.Helper()
	var rule *string
	if ruleID != "" {
		rule = &ruleID
	}
	if _, err := h.super.Exec(context.Background(),
		`INSERT INTO app.message_labels (user_id, message_id, label_id, applied_by, applied_by_rule_id)
		 VALUES ($1, $2, $3, $4, $5)`,
		h.userID.String(), msgID, labelID, by, rule); err != nil {
		t.Fatalf("apply label: %v", err)
	}
}

func (h *harness) seedScore(t *testing.T, msgID string, score float32, model string) {
	t.Helper()
	if _, err := h.super.Exec(context.Background(),
		`INSERT INTO app.triage_scores (user_id, message_id, score, model_version)
		 VALUES ($1, $2, $3, $4)`,
		h.userID.String(), msgID, score, model); err != nil {
		t.Fatalf("seed score: %v", err)
	}
}

// TestEveryFileTheExportPlanPromisesIsActuallyWritten.
//
// ⚠ THE PLAN IS A PROMISE, AND THIS IS WHAT MAKES IT ONE. internal/exportplan
// names, for every exported table, the file its data lands in. Nothing
// previously required those files to EXIST — the plan could promise
// `saved_searches.json` forever while the exporter wrote nothing, and the
// export-report would keep printing the promise as though it were a fact.
//
// The expected set is derived FROM THE PLAN, not typed here, so adding an
// exported table with a new output file makes this test demand that file.
func TestEveryFileTheExportPlanPromisesIsActuallyWritten(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	// A minimal but non-empty user: one message, so the maildir pass runs.
	h.seedMessage(t, []byte("Subject: p\r\nMessage-ID: <p@example.com>\r\n\r\nb\r\n"), false, false, false)

	out := filepath.Join(t.TempDir(), "archive")
	if _, err := exporter.Export(ctx, h.st, h.blobs, h.dek,
		exporter.Options{UserID: h.userID, OutDir: out}); err != nil {
		t.Fatalf("export: %v", err)
	}

	want := map[string]bool{}
	for _, r := range exportplan.Rows() {
		if r.Entry.Disposition != exportplan.Exported {
			continue
		}
		// Detail reads like "labels.json" or "maildir/ (one file per message…)"
		// or "contacts.json (nested under their contact)". The file is the first
		// token; anything after a space is prose.
		name := r.Entry.Detail
		if i := strings.IndexByte(name, ' '); i >= 0 {
			name = name[:i]
		}
		name = strings.TrimSuffix(name, "/")
		if name == "" {
			t.Errorf("app.%s is exported but its Detail names no file", r.Table)
			continue
		}
		want[name] = true
	}
	if len(want) < 8 {
		t.Fatalf("only %d promised files derived from the plan — the parse is wrong and "+
			"this test would pass vacuously", len(want))
	}

	for name := range want {
		if _, err := os.Stat(filepath.Join(out, name)); err != nil {
			t.Errorf("the export plan promises %q and the exporter did not write it: %v",
				name, err)
		}
	}
}

// TestNoConfigurationSidecarCarriesAnInternalID extends the negative control
// over every file the export writes, not just the two the mail uses.
//
// ⚠ The references BETWEEN sidecars are where a ULID hides most easily: a
// signature bound to an identity, a send record naming a message. Both are
// resolved to durable values (an address, a content hash); this is what proves
// it for the whole archive rather than for the fields someone remembered.
func TestNoConfigurationSidecarCarriesAnInternalID(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	raw := []byte("Subject: sent\r\nMessage-ID: <sent@example.com>\r\n\r\nb\r\n")
	msgID := h.seedMessageReturningID(t, raw, true, false, false)
	acctID, identID := h.seedAccountAndIdentity(t)
	snippetID := h.seedSignature(t, identID, "my-sig")
	subID := h.seedSubmission(t, acctID, identID, msgID)
	labelID := h.seedLabel(t, "cfg-label")
	// ⚠ A rule whose action REFERENCES THE LABEL BY ID — the shape a real rule has,
	// and the one that leaked ULIDs into rules.json on a real corpus.
	ruleID := h.seedRuleWithLabel(t, "cfg-rule", labelID)

	out := filepath.Join(t.TempDir(), "archive")
	if _, err := exporter.Export(ctx, h.st, h.blobs, h.dek,
		exporter.Options{UserID: h.userID, OutDir: out}); err != nil {
		t.Fatalf("export: %v", err)
	}

	entries, err := os.ReadDir(out)
	if err != nil {
		t.Fatalf("read archive: %v", err)
	}
	ids := []string{msgID, acctID, identID, snippetID, subID, labelID, ruleID, h.userID.String()}
	var checked int
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		b, err := os.ReadFile(filepath.Join(out, e.Name()))
		if err != nil {
			t.Fatalf("read %s: %v", e.Name(), err)
		}
		checked++
		for _, id := range ids {
			if bytes.Contains(b, []byte(id)) {
				t.Errorf("%s contains the internal id %s", e.Name(), id)
			}
		}
	}
	if checked < 8 {
		t.Fatalf("only %d files checked; the archive should hold every promised sidecar, "+
			"so this negative control is not covering what it claims", checked)
	}

	// And the resolved references must actually be THERE — an archive that
	// simply omitted them would pass the id check trivially.
	// The rule action must name the LABEL, not its id.
	rulesJSON, err := os.ReadFile(filepath.Join(out, "rules.json"))
	if err != nil {
		t.Fatalf("read rules.json: %v", err)
	}
	if !bytes.Contains(rulesJSON, []byte(`"label": "cfg-label"`)) {
		t.Errorf("rules.json does not name the label its action applies; got: %s", rulesJSON)
	}

	snips, err := os.ReadFile(filepath.Join(out, "snippets.json"))
	if err != nil {
		t.Fatalf("read snippets.json: %v", err)
	}
	if !bytes.Contains(snips, []byte("probe-identity@example.com")) {
		t.Error("snippets.json does not name the identity's ADDRESS — the binding was " +
			"dropped rather than resolved, which passes the id check for the wrong reason")
	}
	subs, err := os.ReadFile(filepath.Join(out, "submissions.json"))
	if err != nil {
		t.Fatalf("read submissions.json: %v", err)
	}
	sum := sha256.Sum256(raw)
	if !bytes.Contains(subs, []byte(hex.EncodeToString(sum[:]))) {
		t.Error("submissions.json does not name the message's CONTENT HASH — the send " +
			"record points at nothing a reader of this archive could follow")
	}
}

func (h *harness) seedAccountAndIdentity(t *testing.T) (accountID, identityID string) {
	t.Helper()
	ctx := context.Background()
	accountID = store.NewULID().String()
	identityID = store.NewULID().String()
	if _, err := h.super.Exec(ctx,
		`INSERT INTO app.accounts (id, user_id, name, imap_host, imap_port,
		    smtp_host, smtp_port, credentials_ct, credentials_dek, sync_enabled)
		 VALUES ($1, $2, 'probe', 'imap.example.com', 993,
		    'smtp.example.com', 465, '\x00'::bytea, $3, false)`,
		accountID, h.userID.String(), store.NewULID().String()); err != nil {
		t.Fatalf("seed account: %v", err)
	}
	if _, err := h.super.Exec(ctx,
		`INSERT INTO app.identities (id, user_id, account_id, kind, address, display_name, enabled)
		 VALUES ($1, $2, $3, 'fixed', $4, 'Probe', true)`,
		identityID, h.userID.String(), accountID, "probe-identity@example.com"); err != nil {
		t.Fatalf("seed identity: %v", err)
	}
	return accountID, identityID
}

func (h *harness) seedSignature(t *testing.T, identityID, name string) string {
	t.Helper()
	id := store.NewULID().String()
	if _, err := h.super.Exec(context.Background(),
		`INSERT INTO app.compose_snippets (id, user_id, kind, identity_id, name, body_text)
		 VALUES ($1, $2, 'signature', $3, $4, 'regards')`,
		id, h.userID.String(), identityID, name); err != nil {
		t.Fatalf("seed signature: %v", err)
	}
	return id
}

func (h *harness) seedSubmission(t *testing.T, accountID, identityID, msgID string) string {
	t.Helper()
	id := store.NewULID().String()
	if _, err := h.super.Exec(context.Background(),
		`INSERT INTO app.submissions (id, user_id, account_id, identity_id, message_id,
		    from_address, envelope_to, state, smtp_response, sent_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, 'sent', 'accepted', now())`,
		id, h.userID.String(), accountID, identityID, msgID,
		"probe-identity@example.com", []string{"friend@example.com"}); err != nil {
		t.Fatalf("seed submission: %v", err)
	}
	return id
}
