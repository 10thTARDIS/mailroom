// SPDX-License-Identifier: AGPL-3.0-only

package writeback

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"mailroom/internal/blob"
	"mailroom/internal/store"
	imapsync "mailroom/internal/sync"

	"github.com/oklog/ulid/v2"
)

// --- fakes ------------------------------------------------------------------

// writerCall records one IMAP write the engine issued, so tests can assert the
// exact server-facing effect of an intent.
type writerCall struct {
	kind    string // "store" | "copy" | "move" | "expunge" | "append"
	mailbox string
	uid     imapsync.UID
	add     []imapsync.Flag
	remove  []imapsync.Flag
	dest    string
	flags   []imapsync.Flag
	body    []byte
}

// fakeWriter is an in-memory IMAPWriter. failKinds maps an op kind to an error
// so a test can make a specific op fail; the call is still recorded (the
// attempt happened) before the error is returned.
type fakeWriter struct {
	calls        []writerCall
	supportsMove bool
	failKinds    map[string]error
	// searchHits is what a Message-ID SEARCH finds, keyed by Message-ID. An
	// absent key means "not in the mailbox".
	//
	// ⚠ It is deliberately NOT updated by Append. That is not a simplification,
	// it is the bug's habitat: on a real provider a SEARCH issued milliseconds after
	// our own APPEND returned nothing, and a fake whose SEARCH always sees its
	// own writes could never host that test (#268).
	searchHits map[string][]imapsync.UID
	// appendUID is the APPENDUID the fake server hands back, incremented per
	// append. Zero it to model a server with no UIDPLUS, which answers "I am not
	// telling you" — and which the engine must not mistake for a real UID.
	appendUID imapsync.UID
	noUIDPlus bool
	// expungeHolds, when non-nil, is the set of coordinates this fake server
	// ACTUALLY still holds. A UID EXPUNGE naming anything else succeeds and
	// removes nothing — which is what a real server does, and the reason
	// Result.Expunged cannot be inferred from "the command did not error"
	// (#296). nil means the permissive default: every expunge removes one
	// message.
	expungeHolds map[coord]bool
	// failCoords fails a write at ONE specific coordinate. failKinds fails a
	// whole op type, which cannot express the case #296 turns on: a message with
	// several copies in one account where one copy fails and its siblings are
	// perfectly writable.
	failCoords map[coord]error
	// expungeForceZero makes Expunge report a removal count of ZERO for this
	// coordinate WITHOUT consulting or mutating expungeHolds — modelling the
	// #611 incident: the server actually still holds the message, but a UID
	// EXPUNGE naming it reports nothing removed anyway (not strictly
	// read-your-writes). expungeHolds alone cannot express this: a coordinate
	// missing from that map means "never held it", which is a genuinely
	// different, unsurprising zero.
	expungeForceZero map[coord]bool
	// uidPresentCalls records every UIDPresent probe, so a test can assert the
	// probe ran — or, just as importantly, that it did NOT: expungeOne must
	// only ask when Expunge itself reported removing nothing (#611).
	uidPresentCalls []coord
	// uidStillPresent controls what UIDPresent answers for a coordinate. A
	// coordinate absent from the map answers false ("not present"), the safe
	// default for tests that don't care — deliberately independent of
	// expungeHolds/expungeForceZero, because the whole point of the probe is
	// to answer a question Expunge's own return value could not, so a test
	// must be able to make the two disagree.
	uidStillPresent map[coord]bool
}

// coord identifies one message copy on the fake server.
type coord struct {
	mailbox string
	uid     imapsync.UID
}

func (w *fakeWriter) record(c writerCall) error {
	w.calls = append(w.calls, c)
	if err, ok := w.failKinds[c.kind]; ok {
		return err
	}
	if err, ok := w.failCoords[coord{mailbox: c.mailbox, uid: c.uid}]; ok {
		return err
	}
	return nil
}

func (w *fakeWriter) StoreFlags(_ context.Context, mailbox string, uid imapsync.UID, add, remove []imapsync.Flag) error {
	return w.record(writerCall{kind: "store", mailbox: mailbox, uid: uid, add: add, remove: remove})
}
func (w *fakeWriter) Copy(_ context.Context, mailbox string, uid imapsync.UID, dest string) error {
	return w.record(writerCall{kind: "copy", mailbox: mailbox, uid: uid, dest: dest})
}
func (w *fakeWriter) Move(_ context.Context, mailbox string, uid imapsync.UID, dest string) error {
	return w.record(writerCall{kind: "move", mailbox: mailbox, uid: uid, dest: dest})
}
func (w *fakeWriter) Expunge(_ context.Context, mailbox string, uid imapsync.UID) (int, error) {
	if err := w.record(writerCall{kind: "expunge", mailbox: mailbox, uid: uid}); err != nil {
		return 0, err
	}
	c := coord{mailbox: mailbox, uid: uid}
	if w.expungeForceZero[c] {
		return 0, nil
	}
	if w.expungeHolds == nil {
		return 1, nil
	}
	if w.expungeHolds[c] {
		delete(w.expungeHolds, c)
		return 1, nil
	}
	// The command succeeded; the server had nothing by that UID to remove.
	return 0, nil
}
func (w *fakeWriter) UIDPresent(_ context.Context, mailbox string, uid imapsync.UID) (bool, error) {
	c := coord{mailbox: mailbox, uid: uid}
	w.uidPresentCalls = append(w.uidPresentCalls, c)
	if err, ok := w.failKinds["uidpresent"]; ok {
		return false, err
	}
	return w.uidStillPresent[c], nil
}
func (w *fakeWriter) Append(_ context.Context, mailbox string, flags []imapsync.Flag, body []byte) (imapsync.UID, error) {
	if err := w.record(writerCall{kind: "append", mailbox: mailbox, flags: flags, body: body}); err != nil {
		return 0, err
	}
	if w.noUIDPlus {
		return 0, nil
	}
	w.appendUID++
	return w.appendUID, nil
}
func (w *fakeWriter) SearchMessageID(_ context.Context, mailbox string, messageID string) ([]imapsync.UID, error) {
	w.calls = append(w.calls, writerCall{kind: "search", mailbox: mailbox, dest: messageID})
	if err, ok := w.failKinds["search"]; ok {
		return nil, err
	}
	return w.searchHits[messageID], nil
}
func (w *fakeWriter) SupportsMove() bool { return w.supportsMove }

func (w *fakeWriter) kinds() []string {
	out := make([]string, len(w.calls))
	for i, c := range w.calls {
		out[i] = c.kind
	}
	return out
}

// fakeStore is an in-memory IntentStore that faithfully simulates the intent
// state machine (claim increments attempts and flips pending->inflight;
// MarkIntentFailed with an empty deadReason returns the row to pending), so a
// test can drive retry-then-dead by calling ApplyPending in a loop.
type fakeStore struct {
	intents map[ulid.ULID]*store.Intent
	order   []ulid.ULID
	// prov holds EVERY live copy of a message in an account, because that is
	// what the real store returns (#296) — a fake that could only express one
	// copy per account could not host the test for this bug at all.
	prov    map[string][]store.ProvenanceTarget
	provErr error
	markErr error // when set, every mark call fails (infra-failure simulation)
	// eligible controls the #555 apply-time re-check: nil means "still
	// eligible", matching the pre-#555 behaviour every existing test was
	// written under, so only a test that opts in exercises the guard.
	eligible    map[string]bool
	eligibleErr error
	withdrawn   []ulid.ULID
	// vanishBeforeMark deletes the intent at the start of every mark call,
	// standing in for a message delete whose cascade removes the intent while
	// the pass is still working on it (#691).
	vanishBeforeMark bool
	// recorded is every RecordStoredFlags call, in order (#576); recordErr
	// fails them all.
	recorded  []recordedFlags
	recordErr error
	// gmailRecorded is every RecordGmailLabels call, in order (#52).
	gmailRecorded []recordedFlags
}

// recordedFlags is one RecordStoredFlags call on the fake store.
type recordedFlags struct {
	prov        ulid.ULID
	add, remove []string
}

func newFakeStore() *fakeStore {
	return &fakeStore{intents: map[ulid.ULID]*store.Intent{}, prov: map[string][]store.ProvenanceTarget{}}
}

func provKey(messageID, accountID ulid.ULID) string {
	return messageID.String() + "/" + accountID.String()
}

func (s *fakeStore) add(in store.Intent) {
	cp := in
	s.intents[in.ID] = &cp
	s.order = append(s.order, in.ID)
}

func (s *fakeStore) ClaimPendingIntents(_ context.Context, accountID ulid.ULID, limit int) ([]store.Intent, error) {
	var out []store.Intent
	for _, id := range s.order {
		if len(out) >= limit {
			break
		}
		in := s.intents[id]
		if in.State == store.IntentPending && in.AccountID == accountID {
			in.State = store.IntentInflight
			in.Attempts++
			out = append(out, *in)
		}
	}
	return out, nil
}

func (s *fakeStore) MessageProvenanceTargetsForAccount(_ context.Context, messageID, accountID ulid.ULID) ([]store.ProvenanceTarget, error) {
	if s.provErr != nil {
		return nil, s.provErr
	}
	return s.prov[provKey(messageID, accountID)], nil
}

func (s *fakeStore) MarkIntentApplied(_ context.Context, id ulid.ULID) (bool, error) {
	if s.markErr != nil {
		return false, s.markErr
	}
	if s.vanishBeforeMark {
		delete(s.intents, id)
	}
	if in := s.intents[id]; in != nil && in.State == store.IntentInflight {
		in.State = store.IntentApplied
		return true, nil
	}
	return false, nil
}

func (s *fakeStore) MarkIntentFailed(_ context.Context, id ulid.ULID, errMsg, deadReason string) (bool, error) {
	if s.markErr != nil {
		return false, s.markErr
	}
	if s.vanishBeforeMark {
		delete(s.intents, id)
	}
	in := s.intents[id]
	if in == nil || in.State != store.IntentInflight {
		return false, nil
	}
	in.LastError = errMsg
	if deadReason != "" {
		in.State = store.IntentDead
		in.DeadReason = deadReason
	} else {
		in.State = store.IntentPending
	}
	return true, nil
}

// fakeBlobReader is an in-memory BlobReader keyed by hex content hash, so a
// test can drive appendBody's blob-by-hash path with no database and no real
// blob store.
type fakeBlobReader struct {
	blobs map[string][]byte // hex content hash -> raw bytes
	err   error             // when set, every Raw call fails with this
}

func (b *fakeBlobReader) Raw(_ context.Context, _ ulid.ULID, contentHash []byte) ([]byte, error) {
	if b.err != nil {
		return nil, b.err
	}
	raw, ok := b.blobs[hex.EncodeToString(contentHash)]
	if !ok {
		return nil, blob.ErrNotFound
	}
	return raw, nil
}

// --- helpers ----------------------------------------------------------------

func mustPayload(t *testing.T, p intentPayload) []byte {
	t.Helper()
	b, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	return b
}

// seedIntent registers a pending intent (with an optional live provenance
// target) and returns it.
func seedIntent(s *fakeStore, op store.IntentOp, payload []byte, target *store.ProvenanceTarget) store.Intent {
	if target == nil {
		return seedIntentTargets(s, op, payload)
	}
	return seedIntentTargets(s, op, payload, *target)
}

// seedIntentTargets is seedIntent for the multi-copy case: a message with N
// live provenance rows in ONE account, which is the shape #296 is about.
func seedIntentTargets(s *fakeStore, op store.IntentOp, payload []byte, targets ...store.ProvenanceTarget) store.Intent {
	acct := store.NewULID()
	msg := store.NewULID()
	in := store.Intent{
		ID:             store.NewULID(),
		MessageID:      &msg,
		AccountID:      acct,
		Op:             op,
		Payload:        payload,
		IdempotencyKey: "idem-" + string(op),
		State:          store.IntentPending,
	}
	s.add(in)
	if len(targets) > 0 {
		s.prov[provKey(msg, acct)] = targets
	}
	return in
}

func liveTarget() store.ProvenanceTarget {
	return store.ProvenanceTarget{
		ProvenanceID: store.NewULID(),
		MailboxID:    store.NewULID(),
		IMAPName:     "INBOX",
		UID:          42,
		UIDValidity:  100,
		State:        store.ProvenanceLive,
	}
}

// liveTargetAt is liveTarget at explicit coordinates, so a test can build two
// distinct live copies of one message inside one account.
func liveTargetAt(mailbox string, uid int64) store.ProvenanceTarget {
	t := liveTarget()
	t.IMAPName = mailbox
	t.UID = uid
	return t
}

// --- tests ------------------------------------------------------------------

// Each op type applies to the expected IMAP write(s) and lands the intent in
// 'applied'.
func TestApplyEachOpType(t *testing.T) {
	tests := []struct {
		name         string
		op           store.IntentOp
		payload      intentPayload
		supportsMove bool
		wantKinds    []string
		needsMsg     bool
	}{
		{"add flag", store.IntentAddFlag, intentPayload{Flag: `\Seen`}, false, []string{"store"}, true},
		{"remove flag", store.IntentRemoveFlag, intentPayload{Flag: `\Flagged`}, false, []string{"store"}, true},
		{"add keyword", store.IntentAddKeyword, intentPayload{Keyword: "MailroomLabel-x"}, false, []string{"store"}, true},
		{"remove keyword", store.IntentRemoveKeyword, intentPayload{Keyword: "MailroomLabel-x"}, false, []string{"store"}, true},
		{"expunge", store.IntentExpunge, intentPayload{}, false, []string{"store", "expunge"}, true},
		{"move via MOVE", store.IntentMove, intentPayload{Mailbox: "Archive"}, true, []string{"move"}, true},
		{"move via copy", store.IntentMove, intentPayload{Mailbox: "Archive"}, false, []string{"copy", "store", "expunge"}, true},
		{"append", store.IntentAppend, intentPayload{Mailbox: "Drafts", Body: []byte("raw")}, false, []string{"append"}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := newFakeStore()
			var target *store.ProvenanceTarget
			if tc.needsMsg {
				tg := liveTarget()
				target = &tg
			}
			in := seedIntent(s, tc.op, mustPayload(t, tc.payload), target)
			if !tc.needsMsg {
				// append needs no message; drop it to prove that path.
				s.intents[in.ID].MessageID = nil
			}
			w := &fakeWriter{supportsMove: tc.supportsMove}
			e := NewEngine(s)

			res, err := e.ApplyPending(context.Background(), in.AccountID, w)
			if err != nil {
				t.Fatalf("ApplyPending: %v", err)
			}
			if res.Applied != 1 {
				t.Errorf("Applied = %d, want 1 (res %+v)", res.Applied, res)
			}
			if got := s.intents[in.ID].State; got != store.IntentApplied {
				t.Errorf("intent state = %q, want applied", got)
			}
			if got := w.kinds(); !equalStrings(got, tc.wantKinds) {
				t.Errorf("writer calls = %v, want %v", got, tc.wantKinds)
			}
		})
	}
}

// The add-flag op stores exactly the right flag on the right coordinates.
func TestApplyAddFlagCoordinates(t *testing.T) {
	s := newFakeStore()
	tg := liveTarget()
	in := seedIntent(s, store.IntentAddFlag, mustPayload(t, intentPayload{Flag: `\Seen`}), &tg)
	w := &fakeWriter{}
	if _, err := NewEngine(s).ApplyPending(context.Background(), in.AccountID, w); err != nil {
		t.Fatal(err)
	}
	if len(w.calls) != 1 {
		t.Fatalf("want 1 call, got %d", len(w.calls))
	}
	c := w.calls[0]
	if c.mailbox != "INBOX" || c.uid != 42 {
		t.Errorf("coordinates = %s/%d, want INBOX/42", c.mailbox, c.uid)
	}
	if len(c.add) != 1 || c.add[0] != imapsync.FlagSeen {
		t.Errorf("add = %v, want [%q]", c.add, imapsync.FlagSeen)
	}
	if len(c.remove) != 0 {
		t.Errorf("remove = %v, want none", c.remove)
	}
}

// Re-applying: once an intent is applied it is no longer pending, so a second
// pass claims nothing and issues no IMAP writes — the state machine makes the
// pass idempotent.
func TestIdempotentReapply(t *testing.T) {
	s := newFakeStore()
	tg := liveTarget()
	in := seedIntent(s, store.IntentAddFlag, mustPayload(t, intentPayload{Flag: `\Seen`}), &tg)
	w := &fakeWriter{}
	e := NewEngine(s)

	if _, err := e.ApplyPending(context.Background(), in.AccountID, w); err != nil {
		t.Fatal(err)
	}
	res, err := e.ApplyPending(context.Background(), in.AccountID, w)
	if err != nil {
		t.Fatal(err)
	}
	if res != (Result{}) {
		t.Errorf("second pass result = %+v, want zero", res)
	}
	if len(w.calls) != 1 {
		t.Errorf("writer called %d times across two passes, want 1", len(w.calls))
	}
}

// Conflict paths: a provenance divergence dead-letters the intent WITHOUT
// touching the server (provider-wins), with a surfaced dead_reason.
func TestConflictProvenanceMismatch(t *testing.T) {
	uidv := func(v int64) *int64 { return &v }
	tests := []struct {
		name    string
		target  *store.ProvenanceTarget // nil => no live provenance
		payload intentPayload
	}{
		{
			name:    "message gone (no live provenance)",
			target:  nil,
			payload: intentPayload{Flag: `\Seen`},
		},
		{
			name: "uidvalidity bumped",
			target: func() *store.ProvenanceTarget {
				tg := liveTarget() // UIDValidity 100
				return &tg
			}(),
			payload: intentPayload{Flag: `\Seen`, UIDValidity: uidv(99)},
		},
		{
			name: "stale uidvalidity state",
			target: func() *store.ProvenanceTarget {
				tg := liveTarget()
				tg.State = store.ProvenanceStaleUIDValidity
				return &tg
			}(),
			payload: intentPayload{Flag: `\Seen`},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := newFakeStore()
			in := seedIntent(s, store.IntentAddFlag, mustPayload(t, tc.payload), tc.target)
			w := &fakeWriter{}
			res, err := NewEngine(s).ApplyPending(context.Background(), in.AccountID, w)
			if err != nil {
				t.Fatal(err)
			}
			if res.Conflicts != 1 || res.DeadLettered != 1 {
				t.Errorf("res = %+v, want 1 conflict / 1 dead-lettered", res)
			}
			if len(w.calls) != 0 {
				t.Errorf("server was mutated on conflict: %v", w.kinds())
			}
			got := s.intents[in.ID]
			if got.State != store.IntentDead {
				t.Errorf("state = %q, want dead", got.State)
			}
			if got.DeadReason == "" {
				t.Error("dead_reason is empty — a conflict must be surfaced")
			}
		})
	}
}

// An intent cascaded away mid-pass is counted as Vanished, never as applied,
// retried or dead-lettered (#691). A real pass logged dead_lettered=1
// with no dead intent anywhere, and the count sent a read looking for a failure
// that did not exist. Conflicts is a subset of DeadLettered, so it must not
// count a vanished row either.
func TestVanishedIntentIsNotCountedAsAnOutcome(t *testing.T) {
	uidv := func(v int64) *int64 { return &v }
	tests := []struct {
		name        string
		payload     intentPayload
		failKinds   map[string]error
		maxAttempts int
	}{
		{name: "applied", payload: intentPayload{Flag: `\Seen`}},
		{name: "retried", payload: intentPayload{Flag: `\Seen`},
			failKinds: map[string]error{"store": errors.New("connection reset")}, maxAttempts: 3},
		{name: "dead-lettered", payload: intentPayload{Flag: `\Seen`},
			failKinds: map[string]error{"store": errors.New("connection reset")}, maxAttempts: 1},
		{name: "conflict", payload: intentPayload{Flag: `\Seen`, UIDValidity: uidv(99)}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := newFakeStore()
			s.vanishBeforeMark = true
			tg := liveTarget()
			in := seedIntent(s, store.IntentAddFlag, mustPayload(t, tc.payload), &tg)
			opts := []Option{}
			if tc.maxAttempts > 0 {
				opts = append(opts, WithMaxAttempts(tc.maxAttempts))
			}
			w := &fakeWriter{failKinds: tc.failKinds}
			res, err := NewEngine(s, opts...).ApplyPending(context.Background(), in.AccountID, w)
			if err != nil {
				t.Fatal(err)
			}
			if want := (Result{Vanished: 1, Claimed: 1}); res != want {
				t.Errorf("res = %+v, want %+v", res, want)
			}
		})
	}
}

// A transient IMAP failure retries until maxAttempts, then dead-letters with a
// surfaced reason — bounded retries, never a silent drop.
func TestRetryThenDeadLetter(t *testing.T) {
	s := newFakeStore()
	tg := liveTarget()
	in := seedIntent(s, store.IntentAddFlag, mustPayload(t, intentPayload{Flag: `\Seen`}), &tg)
	w := &fakeWriter{failKinds: map[string]error{"store": errors.New("connection reset")}}
	e := NewEngine(s, WithMaxAttempts(3))

	// Passes 1 and 2 retry; pass 3 (attempts==3) dead-letters.
	for pass := 1; pass <= 3; pass++ {
		res, err := e.ApplyPending(context.Background(), in.AccountID, w)
		if err != nil {
			t.Fatalf("pass %d: %v", pass, err)
		}
		switch pass {
		case 1, 2:
			if res.Retried != 1 || res.DeadLettered != 0 {
				t.Errorf("pass %d res = %+v, want 1 retried", pass, res)
			}
			if got := s.intents[in.ID].State; got != store.IntentPending {
				t.Errorf("pass %d state = %q, want pending", pass, got)
			}
		case 3:
			if res.DeadLettered != 1 || res.Retried != 0 {
				t.Errorf("pass %d res = %+v, want 1 dead-lettered", pass, res)
			}
		}
	}
	got := s.intents[in.ID]
	if got.State != store.IntentDead {
		t.Fatalf("final state = %q, want dead", got.State)
	}
	if got.DeadReason == "" {
		t.Error("dead_reason is empty — dead-letter must be surfaced")
	}
	if got.LastError == "" {
		t.Error("last_error is empty — the underlying cause must be recorded")
	}
	if n := len(w.calls); n != 3 {
		t.Errorf("writer attempted %d times, want 3", n)
	}
}

// A structurally-invalid payload can never succeed, so it dead-letters
// immediately without a server call and without consuming retries.
func TestInvalidPayloadDeadLetters(t *testing.T) {
	tests := []struct {
		name    string
		op      store.IntentOp
		payload intentPayload
	}{
		{"flag missing", store.IntentAddFlag, intentPayload{}},
		{"keyword missing", store.IntentAddKeyword, intentPayload{}},
		{"move missing mailbox", store.IntentMove, intentPayload{}},
		{"append missing mailbox", store.IntentAppend, intentPayload{Body: []byte("x")}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := newFakeStore()
			tg := liveTarget()
			in := seedIntent(s, tc.op, mustPayload(t, tc.payload), &tg)
			w := &fakeWriter{}
			res, err := NewEngine(s).ApplyPending(context.Background(), in.AccountID, w)
			if err != nil {
				t.Fatal(err)
			}
			if res.DeadLettered != 1 {
				t.Errorf("res = %+v, want 1 dead-lettered", res)
			}
			if len(w.calls) != 0 {
				t.Errorf("server mutated on invalid payload: %v", w.kinds())
			}
			if got := s.intents[in.ID]; got.State != store.IntentDead || got.DeadReason == "" {
				t.Errorf("state=%q dead_reason=%q, want dead + surfaced reason", got.State, got.DeadReason)
			}
		})
	}
}

// A message-targeted op with a nil message_id is unrecoverable — dead-letter.
func TestMissingMessageIDDeadLetters(t *testing.T) {
	s := newFakeStore()
	in := seedIntent(s, store.IntentAddFlag, mustPayload(t, intentPayload{Flag: `\Seen`}), nil)
	s.intents[in.ID].MessageID = nil
	w := &fakeWriter{}
	res, err := NewEngine(s).ApplyPending(context.Background(), in.AccountID, w)
	if err != nil {
		t.Fatal(err)
	}
	if res.DeadLettered != 1 || len(w.calls) != 0 {
		t.Errorf("res = %+v, calls = %v; want dead-lettered, no calls", res, w.kinds())
	}
}

// A provenance READ error is infrastructure, not a lost delta: the intent is
// retried (returned to pending), not dead-lettered.
func TestProvenanceReadErrorRetries(t *testing.T) {
	s := newFakeStore()
	tg := liveTarget()
	in := seedIntent(s, store.IntentAddFlag, mustPayload(t, intentPayload{Flag: `\Seen`}), &tg)
	s.provErr = errors.New("db down")
	w := &fakeWriter{}
	res, err := NewEngine(s).ApplyPending(context.Background(), in.AccountID, w)
	if err != nil {
		t.Fatal(err)
	}
	if res.Retried != 1 || res.DeadLettered != 0 {
		t.Errorf("res = %+v, want 1 retried", res)
	}
	if got := s.intents[in.ID].State; got != store.IntentPending {
		t.Errorf("state = %q, want pending", got)
	}
}

// A failure in the store mark call is an infrastructure error surfaced from
// ApplyPending (aggregated), not swallowed.
func TestMarkFailureSurfacesError(t *testing.T) {
	s := newFakeStore()
	tg := liveTarget()
	in := seedIntent(s, store.IntentAddFlag, mustPayload(t, intentPayload{Flag: `\Seen`}), &tg)
	s.markErr = errors.New("mark failed")
	w := &fakeWriter{}
	_, err := NewEngine(s).ApplyPending(context.Background(), in.AccountID, w)
	if err == nil {
		t.Fatal("expected an aggregated infra error, got nil")
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// --- Sent self-append dedup (spec §4, ADR-001) ------------------------------

// Most providers do not file a copy of a message you submit over SMTP, so
// Mailroom APPENDs one to Sent. Some providers DO (Gmail). Appending ours on top
// of theirs would give the user two of every sent message — so the engine asks
// the server, by Message-ID, whether the message is already there, and skips the
// append if it is.
func TestAppendSkipsWhenProviderAlreadyFiledIt(t *testing.T) {
	s := newFakeStore()
	in := seedIntent(s, store.IntentAppend, mustPayload(t, intentPayload{
		Mailbox: "Sent", Body: []byte("raw"), MessageIDHeader: "sent-1@example.com",
	}), nil)
	w := &fakeWriter{searchHits: map[string][]imapsync.UID{"sent-1@example.com": {7}}}

	res, err := NewEngine(s).ApplyPending(context.Background(), in.AccountID, w)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if res.Deduped != 1 || res.Applied != 1 {
		t.Errorf("result = %+v, want 1 deduped and 1 applied (the intent is DONE, not retried)", res)
	}
	for _, k := range w.kinds() {
		if k == "append" {
			t.Fatal("the message was already in Sent: appending it again would duplicate the user's sent mail")
		}
	}
	if got := s.intents[in.ID].State; got != store.IntentApplied {
		t.Errorf("intent state = %q, want applied", got)
	}
}

// The provider did NOT file a copy (the common case): we append ours.
func TestAppendProceedsWhenTheMessageIsNotThere(t *testing.T) {
	s := newFakeStore()
	in := seedIntent(s, store.IntentAppend, mustPayload(t, intentPayload{
		Mailbox: "Sent", Body: []byte("raw"), MessageIDHeader: "sent-2@example.com",
	}), nil)
	w := &fakeWriter{searchHits: map[string][]imapsync.UID{}}

	res, err := NewEngine(s).ApplyPending(context.Background(), in.AccountID, w)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if res.Applied != 1 || res.Deduped != 0 {
		t.Errorf("result = %+v, want 1 applied and 0 deduped", res)
	}
	if !contains(w.kinds(), "append") {
		t.Fatal("the Sent copy was not filed")
	}
}

// A move whose account has no mailbox for the role is DEAD-LETTERED with a
// reason the user can act on — never silently dropped (spec §4a). The store
// enqueues such an intent deliberately, carrying the role and no mailbox.
func TestMoveWithUnresolvableRoleIsDeadLetteredWithAReason(t *testing.T) {
	s := newFakeStore()
	target := liveTarget()
	in := seedIntent(s, store.IntentMove, mustPayload(t, intentPayload{
		TargetRole: "archive",
	}), &target)

	res, err := NewEngine(s).ApplyPending(context.Background(), in.AccountID, &fakeWriter{})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if res.DeadLettered != 1 {
		t.Fatalf("result = %+v, want the move dead-lettered", res)
	}
	reason := s.intents[in.ID].DeadReason
	if reason == "" || !strings.Contains(reason, "archive") {
		t.Errorf("dead_reason = %q, want it to name the missing role so the user knows what is wrong", reason)
	}
}

func contains(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}

// A superseded draft's copy is expunged by MESSAGE-ID, not by a stored UID: the
// message row is gone (store.RetireDraft hard-deletes it, and the intent's
// message_id FK is ON DELETE CASCADE), and a stale UID could point at whatever
// the server has since given that number to. The engine re-identifies the target
// on the server immediately before destroying it.
func TestHeaderExpungeResolvesTheUIDBySearch(t *testing.T) {
	s := newFakeStore()
	acct := store.NewULID()
	in := store.Intent{
		ID: store.NewULID(), AccountID: acct, Op: store.IntentExpunge,
		Payload: mustPayload(t, intentPayload{
			Mailbox: "Drafts", MessageIDHeader: "draft-v1@example.com",
		}),
		IdempotencyKey: "expunge-1", State: store.IntentPending,
	}
	s.add(in)
	w := &fakeWriter{searchHits: map[string][]imapsync.UID{"draft-v1@example.com": {17}}}

	res, err := NewEngine(s).ApplyPending(context.Background(), acct, w)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if res.Applied != 1 {
		t.Fatalf("result = %+v, want the expunge applied", res)
	}
	// Applied alone cannot tell you mail was destroyed — that was the whole of
	// #268. Expunged can.
	if res.Expunged != 1 {
		t.Fatalf("Expunged = %d, want 1", res.Expunged)
	}
	var expunged bool
	for _, c := range w.calls {
		if c.kind == "expunge" {
			expunged = true
			if c.uid != 17 {
				t.Errorf("expunged UID %d, want the one SEARCH returned (17)", c.uid)
			}
			if c.mailbox != "Drafts" {
				t.Errorf("expunged in %q, want Drafts", c.mailbox)
			}
		}
	}
	if !expunged {
		t.Fatal("the superseded draft was not expunged from the server")
	}
}

// --- APPEND body resolution by content hash (mailroom#89) -------------------

// A CURRENT append intent carries no body — only ContentHash. The engine must
// resolve the actual bytes from the blob store before writing them to IMAP.
func TestAppendResolvesBodyFromBlobStoreByContentHash(t *testing.T) {
	s := newFakeStore()
	raw := []byte("the actual message bytes, potentially megabytes of attachment")
	sum := sha256.Sum256(raw)
	hashHex := hex.EncodeToString(sum[:])

	in := seedIntent(s, store.IntentAppend, mustPayload(t, intentPayload{
		Mailbox: "Drafts", ContentHash: hashHex,
	}), nil)
	s.intents[in.ID].MessageID = nil

	blobs := &fakeBlobReader{blobs: map[string][]byte{hashHex: raw}}
	w := &fakeWriter{}
	e := NewEngine(s, WithBlobReader(blobs, store.NewULID()))

	res, err := e.ApplyPending(context.Background(), in.AccountID, w)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if res.Applied != 1 {
		t.Fatalf("result = %+v, want 1 applied", res)
	}
	var appended []byte
	for _, c := range w.calls {
		if c.kind == "append" {
			appended = c.body
		}
	}
	if string(appended) != string(raw) {
		t.Errorf("appended bytes = %q, want the blob store's bytes %q", appended, raw)
	}
}

// A payload with no inline body and no BlobReader configured can never resolve
// its bytes — that is a wiring problem, not a transient one, so it must be
// dead-lettered (surfaced) rather than retried forever or panicking.
func TestAppendWithNoBlobReaderConfiguredDeadLetters(t *testing.T) {
	s := newFakeStore()
	in := seedIntent(s, store.IntentAppend, mustPayload(t, intentPayload{
		Mailbox: "Drafts", ContentHash: "aabbccdd",
	}), nil)
	s.intents[in.ID].MessageID = nil
	w := &fakeWriter{}

	res, err := NewEngine(s).ApplyPending(context.Background(), in.AccountID, w) // no WithBlobReader
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if res.DeadLettered != 1 {
		t.Fatalf("result = %+v, want dead-lettered", res)
	}
	if len(w.calls) != 0 {
		t.Errorf("server was mutated with no resolvable body: %v", w.kinds())
	}
	if got := s.intents[in.ID].DeadReason; got == "" {
		t.Error("dead_reason is empty — an unresolvable append must be surfaced")
	}
}

// A blob that has permanently ceased to exist (blob.ErrNotFound) is
// unrecoverable: retrying can never produce the bytes back, so the intent is
// dead-lettered with a reason, not retried forever.
func TestAppendDeadLettersWhenBlobIsPermanentlyGone(t *testing.T) {
	s := newFakeStore()
	sum := sha256.Sum256([]byte("gone"))
	in := seedIntent(s, store.IntentAppend, mustPayload(t, intentPayload{
		Mailbox: "Drafts", ContentHash: hex.EncodeToString(sum[:]),
	}), nil)
	s.intents[in.ID].MessageID = nil
	blobs := &fakeBlobReader{blobs: map[string][]byte{}} // nothing stored
	w := &fakeWriter{}

	res, err := NewEngine(s, WithBlobReader(blobs, store.NewULID())).ApplyPending(context.Background(), in.AccountID, w)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if res.DeadLettered != 1 {
		t.Fatalf("result = %+v, want dead-lettered (the blob is permanently gone)", res)
	}
	if got := s.intents[in.ID].DeadReason; got == "" {
		t.Error("dead_reason is empty — a permanently missing blob must be surfaced")
	}
}

// A TRANSIENT blob-store failure (disk hiccup, a key provider that could not
// open the master key this pass) is retried — it is not the same failure mode
// as a permanently missing blob, and must not be dead-lettered on the first
// try.
func TestAppendRetriesOnTransientBlobReadError(t *testing.T) {
	s := newFakeStore()
	in := seedIntent(s, store.IntentAppend, mustPayload(t, intentPayload{
		Mailbox: "Drafts", ContentHash: "aabbcc",
	}), nil)
	s.intents[in.ID].MessageID = nil
	blobs := &fakeBlobReader{err: errors.New("disk temporarily unavailable")}
	w := &fakeWriter{}

	res, err := NewEngine(s, WithBlobReader(blobs, store.NewULID())).ApplyPending(context.Background(), in.AccountID, w)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if res.Retried != 1 || res.DeadLettered != 0 {
		t.Errorf("result = %+v, want retried, not dead-lettered", res)
	}
	if got := s.intents[in.ID].State; got != store.IntentPending {
		t.Errorf("state = %q, want pending (retry)", got)
	}
}

// A LEGACY append intent — enqueued before mailroom#89 — still carries its
// body inline. The engine must keep honoring that shape so an intent already
// queued at upgrade time is never stranded, even with no BlobReader wired.
func TestAppendHonorsLegacyInlineBodyWithNoBlobReader(t *testing.T) {
	s := newFakeStore()
	in := seedIntent(s, store.IntentAppend, mustPayload(t, intentPayload{
		Mailbox: "Drafts", Body: []byte("a pre-upgrade intent's inline body"),
	}), nil)
	s.intents[in.ID].MessageID = nil
	w := &fakeWriter{}

	res, err := NewEngine(s).ApplyPending(context.Background(), in.AccountID, w) // no BlobReader
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if res.Applied != 1 {
		t.Fatalf("result = %+v, want the legacy inline-body intent applied", res)
	}
	var appended []byte
	for _, c := range w.calls {
		if c.kind == "append" {
			appended = c.body
		}
	}
	if string(appended) != "a pre-upgrade intent's inline body" {
		t.Errorf("appended bytes = %q, want the legacy inline body", appended)
	}
}

// ⚠ THIS TEST USED TO ASSERT THE BUG (#268).
//
// It was called TestHeaderExpungeOfAMissingCopyIsSuccess and it demanded
// res.Applied == 1 when the SEARCH matched nothing, on the reasoning that the
// copy must already be gone and the intent's purpose is that it not be there.
// The reasoning was fine; the premise was not. A SEARCH can also return nothing
// when the copy IS there — measured, on a real provider, milliseconds after we
// appended the message ourselves — and the old assertion made those two
// outcomes literally the same green. A superseded draft survived every send and
// nothing anywhere reported a failure.
//
// An expunge is only enqueued now when a copy is believed to exist (see
// store.RetireSentDraftCopy). So finding nothing is a failure: retry, and after
// the attempt budget dead-letter with a reason an operator can read.
func TestHeaderExpungeOfAMissingCopyIsNotSilentSuccess(t *testing.T) {
	s := newFakeStore()
	acct := store.NewULID()
	in := store.Intent{
		ID: store.NewULID(), AccountID: acct, Op: store.IntentExpunge,
		Payload: mustPayload(t, intentPayload{
			Mailbox: "Drafts", MessageIDHeader: "gone@example.com",
		}),
		IdempotencyKey: "expunge-2", State: store.IntentPending,
	}
	s.add(in)
	w := &fakeWriter{searchHits: map[string][]imapsync.UID{}}
	e := NewEngine(s, WithMaxAttempts(3))

	res, err := e.ApplyPending(context.Background(), acct, w)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if res.Applied != 0 || res.Retried != 1 {
		t.Fatalf("result = %+v, want the intent RETRIED — an expunge that removed nothing has not succeeded", res)
	}
	if res.Expunged != 0 {
		t.Errorf("Expunged = %d, want 0: nothing was found, so nothing was destroyed", res.Expunged)
	}
	if contains(w.kinds(), "expunge") {
		t.Fatal("nothing was found, so nothing may be expunged")
	}

	// Drain the attempt budget: the intent must end up DEAD and SURFACED, not
	// quietly retried forever and not quietly applied.
	for i := 0; i < 5 && s.intents[in.ID].State == store.IntentPending; i++ {
		if _, err := e.ApplyPending(context.Background(), acct, w); err != nil {
			t.Fatalf("apply: %v", err)
		}
	}
	if got := s.intents[in.ID].State; got != store.IntentDead {
		t.Fatalf("final state = %q, want %q", got, store.IntentDead)
	}
	reason := s.intents[in.ID].DeadReason
	if !strings.Contains(reason, "gone@example.com") || !strings.Contains(reason, "Drafts") {
		t.Errorf("dead_reason = %q, want it to name the message and the mailbox so the operator can go look", reason)
	}
}

// The same empty SEARCH, but this pass APPENDED the message itself and the
// server returned an APPENDUID. That is the incident's exact shape: a draft's
// APPEND and the EXPUNGE that supersedes it land in one pass, milliseconds
// apart, and the SEARCH between them comes back blind. We do not have to ask the
// server which UID it is — it told us when it took the message (#268).
func TestHeaderExpungeUsesTheAppendUIDWhenSearchIsBlindToOurOwnAppend(t *testing.T) {
	s := newFakeStore()
	acct := store.NewULID()
	const msgID = "draft@example.com"
	raw := []byte("From: me@example.com\r\nMessage-ID: <draft@example.com>\r\n\r\nhi\r\n")
	sum := sha256.Sum256(raw)
	hashHex := hex.EncodeToString(sum[:])

	// Ordered exactly as the live queue was: the APPEND first, then the EXPUNGE
	// that supersedes it.
	s.add(store.Intent{
		ID: store.NewULID(), AccountID: acct, Op: store.IntentAppend,
		Payload: mustPayload(t, intentPayload{
			Mailbox: "Drafts", ContentHash: hashHex, MessageIDHeader: msgID,
		}),
		IdempotencyKey: "append-1", State: store.IntentPending,
	})
	s.add(store.Intent{
		ID: store.NewULID(), AccountID: acct, Op: store.IntentExpunge,
		Payload: mustPayload(t, intentPayload{
			Mailbox: "Drafts", MessageIDHeader: msgID,
		}),
		IdempotencyKey: "expunge-appenduid", State: store.IntentPending,
	})

	// searchHits stays empty throughout: this fake server never lets a SEARCH
	// see the message it just accepted, which is the whole point.
	w := &fakeWriter{searchHits: map[string][]imapsync.UID{}}
	blobs := &fakeBlobReader{blobs: map[string][]byte{hashHex: raw}}

	res, err := NewEngine(s, WithBlobReader(blobs, store.NewULID())).
		ApplyPending(context.Background(), acct, w)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if res.Applied != 2 {
		t.Fatalf("result = %+v, want both intents applied", res)
	}
	if res.Expunged != 1 {
		t.Fatalf("Expunged = %d, want 1 — the draft copy must actually be destroyed", res.Expunged)
	}
	var expungedUID imapsync.UID
	for _, c := range w.calls {
		if c.kind == "expunge" {
			expungedUID = c.uid
		}
	}
	if expungedUID != 1 {
		t.Errorf("expunged UID = %d, want 1 (the APPENDUID this pass was given)", expungedUID)
	}
}

// A server with no UIDPLUS never says which UID it assigned, so there is no UID
// to fall back on. The engine must NOT invent one — expunging UID 0, or a
// guessed number, destroys somebody else's mail. It fails and retries instead.
func TestHeaderExpungeNeverGuessesAUIDWhenTheServerDidNotGiveOne(t *testing.T) {
	s := newFakeStore()
	acct := store.NewULID()
	const msgID = "draft@example.com"
	raw := []byte("From: me@example.com\r\nMessage-ID: <draft@example.com>\r\n\r\nhi\r\n")
	sum := sha256.Sum256(raw)
	hashHex := hex.EncodeToString(sum[:])

	s.add(store.Intent{
		ID: store.NewULID(), AccountID: acct, Op: store.IntentAppend,
		Payload: mustPayload(t, intentPayload{
			Mailbox: "Drafts", ContentHash: hashHex, MessageIDHeader: msgID,
		}),
		IdempotencyKey: "append-2", State: store.IntentPending,
	})
	s.add(store.Intent{
		ID: store.NewULID(), AccountID: acct, Op: store.IntentExpunge,
		Payload: mustPayload(t, intentPayload{
			Mailbox: "Drafts", MessageIDHeader: msgID,
		}),
		IdempotencyKey: "expunge-nouidplus", State: store.IntentPending,
	})

	w := &fakeWriter{searchHits: map[string][]imapsync.UID{}, noUIDPlus: true}
	blobs := &fakeBlobReader{blobs: map[string][]byte{hashHex: raw}}

	res, err := NewEngine(s, WithBlobReader(blobs, store.NewULID())).
		ApplyPending(context.Background(), acct, w)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if res.Applied != 1 || res.Retried != 1 {
		t.Fatalf("result = %+v, want the append applied and the expunge retried", res)
	}
	if contains(w.kinds(), "expunge") {
		t.Fatal("the server never returned an APPENDUID, so there was no UID to expunge — it must not guess")
	}
}

// The APPENDUID is remembered per (mailbox, Message-ID), not globally: an
// expunge for a DIFFERENT message must not inherit the UID of the one we
// appended. Getting this wrong deletes the wrong mail, which is unrecoverable.
func TestHeaderExpungeDoesNotBorrowAnotherMessagesAppendUID(t *testing.T) {
	s := newFakeStore()
	acct := store.NewULID()
	raw := []byte("From: me@example.com\r\nMessage-ID: <mine@example.com>\r\n\r\nhi\r\n")
	sum := sha256.Sum256(raw)
	hashHex := hex.EncodeToString(sum[:])

	s.add(store.Intent{
		ID: store.NewULID(), AccountID: acct, Op: store.IntentAppend,
		Payload: mustPayload(t, intentPayload{
			Mailbox: "Drafts", ContentHash: hashHex, MessageIDHeader: "mine@example.com",
		}),
		IdempotencyKey: "append-3", State: store.IntentPending,
	})
	s.add(store.Intent{
		ID: store.NewULID(), AccountID: acct, Op: store.IntentExpunge,
		Payload: mustPayload(t, intentPayload{
			Mailbox: "Drafts", MessageIDHeader: "somebody-elses@example.com",
		}),
		IdempotencyKey: "expunge-other", State: store.IntentPending,
	})

	w := &fakeWriter{searchHits: map[string][]imapsync.UID{}}
	blobs := &fakeBlobReader{blobs: map[string][]byte{hashHex: raw}}

	res, err := NewEngine(s, WithBlobReader(blobs, store.NewULID())).
		ApplyPending(context.Background(), acct, w)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if contains(w.kinds(), "expunge") {
		t.Fatalf("a different message's APPENDUID was used to destroy mail: %v", w.kinds())
	}
	if res.Retried != 1 {
		t.Fatalf("result = %+v, want the unrelated expunge retried, not applied", res)
	}
}

// A failed SEARCH on the expunge path must RETRY, never guess: without the
// search we do not know which UID is ours, and deleting the wrong message is
// unrecoverable.
func TestHeaderExpungeRetriesWhenSearchFails(t *testing.T) {
	s := newFakeStore()
	acct := store.NewULID()
	in := store.Intent{
		ID: store.NewULID(), AccountID: acct, Op: store.IntentExpunge,
		Payload: mustPayload(t, intentPayload{
			Mailbox: "Drafts", MessageIDHeader: "x@example.com",
		}),
		IdempotencyKey: "expunge-3", State: store.IntentPending,
	}
	s.add(in)
	w := &fakeWriter{failKinds: map[string]error{"search": errors.New("connection reset")}}

	res, err := NewEngine(s).ApplyPending(context.Background(), acct, w)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if res.Retried != 1 {
		t.Fatalf("result = %+v, want the intent retried", res)
	}
	if contains(w.kinds(), "expunge") {
		t.Fatal("an unanswerable SEARCH must never lead to a blind expunge")
	}
}

// --- #611: an expunge that removed nothing must be re-observed -------------
//
// Three call sites share expungeOne (engine.go): the header-addressed expunge
// (applyHeaderExpunge), the provenance-addressed expunge
// (applyToTarget/IntentExpunge), and the no-MOVE move fallback
// (applyToTarget/IntentMove). The incident that prompted this shipped at only
// one of an equivalent pair, so each site gets its own three cases here rather
// than one shared table: still-present (transient, retried, NOT applied),
// absent (applied, but NOT counted as a destruction), and the server actually
// removing it (applied, counted, and the probe never runs at all — it exists
// only to interpret a zero, not to double-check a one).

// The header-addressed path: a superseded draft's copy, found by SEARCH.
func TestHeaderExpungeReobservesWhenTheServerReportsZero(t *testing.T) {
	newIntent := func(t *testing.T, s *fakeStore, acct ulid.ULID) store.Intent {
		t.Helper()
		in := store.Intent{
			ID: store.NewULID(), AccountID: acct, Op: store.IntentExpunge,
			Payload: mustPayload(t, intentPayload{
				Mailbox: "Drafts", MessageIDHeader: "reobserve@example.com",
			}),
			IdempotencyKey: "expunge-reobserve", State: store.IntentPending,
		}
		s.add(in)
		return in
	}

	t.Run("still present: transient, not applied", func(t *testing.T) {
		s := newFakeStore()
		acct := store.NewULID()
		in := newIntent(t, s, acct)
		w := &fakeWriter{
			searchHits:       map[string][]imapsync.UID{"reobserve@example.com": {37}},
			expungeForceZero: map[coord]bool{{mailbox: "Drafts", uid: 37}: true},
			uidStillPresent:  map[coord]bool{{mailbox: "Drafts", uid: 37}: true},
		}
		e := NewEngine(s, WithMaxAttempts(3))

		res, err := e.ApplyPending(context.Background(), acct, w)
		if err != nil {
			t.Fatalf("apply: %v", err)
		}
		if res.Applied != 0 || res.Retried != 1 {
			t.Fatalf("result = %+v, want retried, not applied — the server kept the message", res)
		}
		if res.Expunged != 0 {
			t.Errorf("Expunged = %d, want 0: nothing was actually destroyed", res.Expunged)
		}
		if got := s.intents[in.ID].State; got != store.IntentPending {
			t.Errorf("state = %q, want pending (retry)", got)
		}
		if len(w.uidPresentCalls) != 1 {
			t.Fatalf("UIDPresent calls = %d, want exactly 1", len(w.uidPresentCalls))
		}
		if last := s.intents[in.ID].LastError; !strings.Contains(last, "37") || !strings.Contains(last, "Drafts") {
			t.Errorf("last_error = %q, want it to name the surviving uid and mailbox", last)
		}
	})

	t.Run("absent: applied, not counted as destroyed", func(t *testing.T) {
		s := newFakeStore()
		acct := store.NewULID()
		newIntent(t, s, acct)
		w := &fakeWriter{
			searchHits:       map[string][]imapsync.UID{"reobserve@example.com": {37}},
			expungeForceZero: map[coord]bool{{mailbox: "Drafts", uid: 37}: true},
			// uidStillPresent left nil: UIDPresent answers false (genuinely gone).
		}

		res, err := NewEngine(s).ApplyPending(context.Background(), acct, w)
		if err != nil {
			t.Fatalf("apply: %v", err)
		}
		if res.Applied != 1 {
			t.Fatalf("result = %+v, want applied — the message is genuinely gone", res)
		}
		if res.Expunged != 0 {
			t.Errorf("Expunged = %d, want 0: the server never confirmed destroying anything", res.Expunged)
		}
		if len(w.uidPresentCalls) != 1 {
			t.Fatalf("UIDPresent calls = %d, want exactly 1", len(w.uidPresentCalls))
		}
	})

	t.Run("server removed it: applied, counted, no probe", func(t *testing.T) {
		s := newFakeStore()
		acct := store.NewULID()
		newIntent(t, s, acct)
		w := &fakeWriter{searchHits: map[string][]imapsync.UID{"reobserve@example.com": {37}}}

		res, err := NewEngine(s).ApplyPending(context.Background(), acct, w)
		if err != nil {
			t.Fatalf("apply: %v", err)
		}
		if res.Applied != 1 || res.Expunged != 1 {
			t.Fatalf("result = %+v, want 1 applied and 1 expunged", res)
		}
		if len(w.uidPresentCalls) != 0 {
			t.Fatalf("UIDPresent calls = %d, want 0 — the probe is only for a zero count", len(w.uidPresentCalls))
		}
	})

	// FAIL CLOSED: a probe that could not be answered is not permission to move
	// on. Read as "absent", it would retire the intent on exactly the zero count
	// #611 was — the one case the probe exists to question.
	t.Run("probe fails: transient, not applied", func(t *testing.T) {
		s := newFakeStore()
		acct := store.NewULID()
		in := newIntent(t, s, acct)
		w := &fakeWriter{
			searchHits:       map[string][]imapsync.UID{"reobserve@example.com": {37}},
			expungeForceZero: map[coord]bool{{mailbox: "Drafts", uid: 37}: true},
			failKinds:        map[string]error{"uidpresent": errors.New("connection reset")},
		}

		res, err := NewEngine(s, WithMaxAttempts(3)).ApplyPending(context.Background(), acct, w)
		if err != nil {
			t.Fatalf("apply: %v", err)
		}
		if res.Applied != 0 || res.Retried != 1 {
			t.Fatalf("result = %+v, want retried, not applied — nobody could say the copy was gone", res)
		}
		if got := s.intents[in.ID].State; got != store.IntentPending {
			t.Errorf("state = %q, want pending (retry)", got)
		}
	})
}

// The provenance-addressed path: applyToTarget's IntentExpunge case.
func TestProvenanceExpungeReobservesWhenTheServerReportsZero(t *testing.T) {
	target := func() store.ProvenanceTarget { return liveTargetAt("INBOX", 37) }

	t.Run("still present: transient, not applied", func(t *testing.T) {
		s := newFakeStore()
		tg := target()
		in := seedIntent(s, store.IntentExpunge, mustPayload(t, intentPayload{}), &tg)
		w := &fakeWriter{
			expungeForceZero: map[coord]bool{{mailbox: "INBOX", uid: 37}: true},
			uidStillPresent:  map[coord]bool{{mailbox: "INBOX", uid: 37}: true},
		}
		e := NewEngine(s, WithMaxAttempts(3))

		res, err := e.ApplyPending(context.Background(), in.AccountID, w)
		if err != nil {
			t.Fatalf("apply: %v", err)
		}
		if res.Applied != 0 || res.Retried != 1 {
			t.Fatalf("result = %+v, want retried, not applied", res)
		}
		if res.Expunged != 0 {
			t.Errorf("Expunged = %d, want 0", res.Expunged)
		}
		if len(w.uidPresentCalls) != 1 {
			t.Fatalf("UIDPresent calls = %d, want exactly 1", len(w.uidPresentCalls))
		}
	})

	t.Run("absent: applied, not counted as destroyed", func(t *testing.T) {
		s := newFakeStore()
		tg := target()
		in := seedIntent(s, store.IntentExpunge, mustPayload(t, intentPayload{}), &tg)
		w := &fakeWriter{expungeForceZero: map[coord]bool{{mailbox: "INBOX", uid: 37}: true}}

		res, err := NewEngine(s).ApplyPending(context.Background(), in.AccountID, w)
		if err != nil {
			t.Fatalf("apply: %v", err)
		}
		if res.Applied != 1 {
			t.Fatalf("result = %+v, want applied", res)
		}
		if res.Expunged != 0 {
			t.Errorf("Expunged = %d, want 0", res.Expunged)
		}
		if len(w.uidPresentCalls) != 1 {
			t.Fatalf("UIDPresent calls = %d, want exactly 1", len(w.uidPresentCalls))
		}
	})

	t.Run("server removed it: applied, counted, no probe", func(t *testing.T) {
		s := newFakeStore()
		tg := target()
		in := seedIntent(s, store.IntentExpunge, mustPayload(t, intentPayload{}), &tg)
		w := &fakeWriter{}

		res, err := NewEngine(s).ApplyPending(context.Background(), in.AccountID, w)
		if err != nil {
			t.Fatalf("apply: %v", err)
		}
		if res.Applied != 1 || res.Expunged != 1 {
			t.Fatalf("result = %+v, want 1 applied and 1 expunged", res)
		}
		if len(w.uidPresentCalls) != 0 {
			t.Fatalf("UIDPresent calls = %d, want 0", len(w.uidPresentCalls))
		}
	})
}

// The no-MOVE fallback: applyToTarget's IntentMove case when SupportsMove is
// false (Copy, then expungeOne on the source). destroyed is always false for
// a move — the message was relocated, not destroyed — so Expunged must stay
// 0 in every subtest here, including the one where the source really is gone.
func TestMoveFallbackReobservesWhenTheSourceExpungeReportsZero(t *testing.T) {
	target := func() store.ProvenanceTarget { return liveTargetAt("INBOX", 37) }

	t.Run("source still present: transient, not applied", func(t *testing.T) {
		s := newFakeStore()
		tg := target()
		in := seedIntent(s, store.IntentMove, mustPayload(t, intentPayload{Mailbox: "Archive"}), &tg)
		w := &fakeWriter{
			expungeForceZero: map[coord]bool{{mailbox: "INBOX", uid: 37}: true},
			uidStillPresent:  map[coord]bool{{mailbox: "INBOX", uid: 37}: true},
		}
		e := NewEngine(s, WithMaxAttempts(3))

		res, err := e.ApplyPending(context.Background(), in.AccountID, w)
		if err != nil {
			t.Fatalf("apply: %v", err)
		}
		if res.Applied != 0 || res.Retried != 1 {
			t.Fatalf("result = %+v, want retried, not applied — the source is still there", res)
		}
		if !contains(w.kinds(), "copy") {
			t.Error("the destination copy was never attempted")
		}
		if len(w.uidPresentCalls) != 1 {
			t.Fatalf("UIDPresent calls = %d, want exactly 1", len(w.uidPresentCalls))
		}
	})

	t.Run("source absent: applied, not counted as destroyed", func(t *testing.T) {
		s := newFakeStore()
		tg := target()
		in := seedIntent(s, store.IntentMove, mustPayload(t, intentPayload{Mailbox: "Archive"}), &tg)
		w := &fakeWriter{expungeForceZero: map[coord]bool{{mailbox: "INBOX", uid: 37}: true}}

		res, err := NewEngine(s).ApplyPending(context.Background(), in.AccountID, w)
		if err != nil {
			t.Fatalf("apply: %v", err)
		}
		if res.Applied != 1 {
			t.Fatalf("result = %+v, want applied", res)
		}
		if res.Expunged != 0 {
			t.Errorf("Expunged = %d, want 0 — a move never counts as a destruction", res.Expunged)
		}
		if len(w.uidPresentCalls) != 1 {
			t.Fatalf("UIDPresent calls = %d, want exactly 1", len(w.uidPresentCalls))
		}
	})

	t.Run("server removed the source: applied, no probe", func(t *testing.T) {
		s := newFakeStore()
		tg := target()
		in := seedIntent(s, store.IntentMove, mustPayload(t, intentPayload{Mailbox: "Archive"}), &tg)
		w := &fakeWriter{}

		res, err := NewEngine(s).ApplyPending(context.Background(), in.AccountID, w)
		if err != nil {
			t.Fatalf("apply: %v", err)
		}
		if res.Applied != 1 {
			t.Fatalf("result = %+v, want applied", res)
		}
		if res.Expunged != 0 {
			t.Errorf("Expunged = %d, want 0 — a move never counts as a destruction even when the source really is removed", res.Expunged)
		}
		if len(w.uidPresentCalls) != 0 {
			t.Fatalf("UIDPresent calls = %d, want 0", len(w.uidPresentCalls))
		}
	})
}

// PurgeExpungeStillEligible fakes the #555 apply-time re-check.
//
// ⚠ THE DEFAULT IS "STILL ELIGIBLE". A nil map means every purge expunge is
// still due, which is the behaviour every test written before #555 assumed —
// so adding the guard does not silently change what those tests exercise. A
// test that wants the stale case says so explicitly.
func (s *fakeStore) PurgeExpungeStillEligible(_ context.Context, messageID, accountID ulid.ULID) (bool, store.ExpungeSkip, error) {
	if s.eligibleErr != nil {
		return false, "", s.eligibleErr
	}
	if s.eligible == nil {
		return true, store.ExpungeSkipNone, nil
	}
	ok, present := s.eligible[provKey(messageID, accountID)]
	if !present {
		return true, store.ExpungeSkipNone, nil
	}
	if ok {
		return true, store.ExpungeSkipNone, nil
	}
	return false, store.ExpungeSkipUnlanded, nil
}

// WithdrawIntent records the withdrawal and removes the row, as the real store
// does — the intent is GONE, not moved to a terminal state, so a test asserting
// on s.intents sees the same thing production would.
func (s *fakeStore) RecordStoredFlags(_ context.Context, prov ulid.ULID, add, remove []string) error {
	if s.recordErr != nil {
		return s.recordErr
	}
	s.recorded = append(s.recorded, recordedFlags{prov: prov, add: add, remove: remove})
	return nil
}

// RecordGmailLabels records the confirmed Gmail label delta (#52), as the
// real store moves the copy's gm_labels baseline.
func (s *fakeStore) RecordGmailLabels(_ context.Context, prov ulid.ULID, add, remove []string) error {
	if s.recordErr != nil {
		return s.recordErr
	}
	s.gmailRecorded = append(s.gmailRecorded, recordedFlags{prov: prov, add: add, remove: remove})
	return nil
}

func (s *fakeStore) WithdrawIntent(_ context.Context, id ulid.ULID) error {
	s.withdrawn = append(s.withdrawn, id)
	delete(s.intents, id)
	return nil
}
