// SPDX-License-Identifier: AGPL-3.0-only

package thread

import (
	crand "crypto/rand"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"
)

// testEntropy mirrors internal/store's NewULID entropy source, kept local
// so this package's tests stay independent of internal/store.
var testEntropy = &ulid.LockedMonotonicReader{MonotonicReader: ulid.Monotonic(crand.Reader, 0)}

func newULID(t *testing.T) ulid.ULID {
	t.Helper()
	id, err := ulid.New(ulid.Timestamp(time.Now()), testEntropy)
	if err != nil {
		t.Fatalf("newULID: %v", err)
	}
	return id
}

// msg builds a Message with a fresh app ID for the given header fields.
// base is a reference time; offset is added to it to produce Date, so
// tests can express chronological order compactly and construct input
// slices in a different order than the intended chronology.
func msg(t *testing.T, base time.Time, offset time.Duration, messageID, subject string, refs, inReplyTo []string) Message {
	t.Helper()
	return Message{
		ID:         newULID(t),
		MessageID:  messageID,
		References: refs,
		InReplyTo:  inReplyTo,
		Subject:    subject,
		Date:       base.Add(offset),
	}
}

// threadOf finds the thread containing m's ID and fails the test if m is
// not in any thread.
func threadOf(t *testing.T, res Result, m Message) ThreadResult {
	t.Helper()
	idx, ok := res.ThreadOf[m.ID]
	if !ok {
		t.Fatalf("message %s (Message-ID %q) not assigned to any thread", m.ID, m.MessageID)
	}
	return res.Threads[idx]
}

// assertSameThread fails unless every given message ends up in one thread.
func assertSameThread(t *testing.T, res Result, msgs ...Message) {
	t.Helper()
	if len(msgs) == 0 {
		return
	}
	want := threadOf(t, res, msgs[0])
	for _, m := range msgs[1:] {
		got := threadOf(t, res, m)
		if got.RootMessageID != want.RootMessageID || len(got.Members) != len(want.Members) {
			t.Fatalf("expected %s and %s in the same thread; got roots %q and %q",
				msgs[0].MessageID, m.MessageID, want.RootMessageID, got.RootMessageID)
		}
	}
}

// assertDistinctThreads fails unless every given message ends up in its own
// (mutually different) thread.
func assertDistinctThreads(t *testing.T, res Result, msgs ...Message) {
	t.Helper()
	seen := make(map[int]ulid.ULID)
	for _, m := range msgs {
		idx, ok := res.ThreadOf[m.ID]
		if !ok {
			t.Fatalf("message %s (Message-ID %q) not assigned to any thread", m.ID, m.MessageID)
		}
		if prior, dup := seen[idx]; dup {
			t.Fatalf("expected %s to be in a distinct thread, but shares a thread with %s", m.MessageID, prior)
		}
		seen[idx] = m.ID
	}
}

// assertOrder fails unless th.Members equals the app IDs of msgs, in order.
func assertOrder(t *testing.T, th ThreadResult, msgs ...Message) {
	t.Helper()
	if len(th.Members) != len(msgs) {
		t.Fatalf("thread has %d members, want %d", len(th.Members), len(msgs))
	}
	for i, m := range msgs {
		if th.Members[i] != m.ID {
			t.Fatalf("member %d = %s, want %s (Message-ID %q)", i, th.Members[i], m.ID, m.MessageID)
		}
	}
}

func TestLinearReplyChain(t *testing.T) {
	base := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	a := msg(t, base, 0*time.Hour, "a@x", "Meeting notes", nil, nil)
	b := msg(t, base, 1*time.Hour, "b@x", "Re: Meeting notes", []string{"a@x"}, nil)
	c := msg(t, base, 2*time.Hour, "c@x", "Re: Meeting notes", []string{"a@x", "b@x"}, nil)

	// Feed them out of chronological order to exercise the date sort.
	res := Thread([]Message{c, a, b})

	if len(res.Threads) != 1 {
		t.Fatalf("got %d threads, want 1", len(res.Threads))
	}
	assertSameThread(t, res, a, b, c)
	th := threadOf(t, res, a)
	if !th.HasRoot || th.Root != a.ID {
		t.Fatalf("expected root %s, got HasRoot=%v Root=%s", a.ID, th.HasRoot, th.Root)
	}
	assertOrder(t, th, a, b, c)
}

func TestMissingIntermediateMessage(t *testing.T) {
	base := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	a := msg(t, base, 0, "a@x", "Deploy plan", nil, nil)
	// "m@x" (the reply to A) is never delivered to this store; c only
	// exists because References accumulates the whole ancestor chain.
	c := msg(t, base, 2*time.Hour, "c@x", "Re: Deploy plan", []string{"a@x", "m@x"}, nil)

	res := Thread([]Message{a, c})

	if len(res.Threads) != 1 {
		t.Fatalf("got %d threads, want 1", len(res.Threads))
	}
	th := threadOf(t, res, a)
	if !th.HasRoot || th.Root != a.ID {
		t.Fatalf("expected root %s (missing container spliced out), got HasRoot=%v Root=%s", a.ID, th.HasRoot, th.Root)
	}
	assertOrder(t, th, a, c)
}

// TestDuplicateMessageIDsShareOneThread REPLACES an earlier
// TestDuplicateMessageIDsDoNotMerge, which asserted the opposite. This is a
// deliberate reversal of a tested decision, not a gap being filled (#210).
//
// The old behaviour isolated a duplicate Message-ID into its own thread, citing
// spoofing. That defence was never load-bearing: an attacker who knows a
// Message-ID can already join its thread by naming it in References or
// In-Reply-To, which is how replies work and which this threader honours. So
// isolating duplicates prevented no attack and broke the common legitimate
// case — RFC 5322 §3.6.4 makes a Message-ID globally unique, so two messages
// bearing one are the same message.
//
// Measured on a real mailbox when this changed: the duplicate groups were all
// split across separate threads.
func TestDuplicateMessageIDsShareOneThread(t *testing.T) {
	base := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	// Same Message-ID, no References between them — the shape bulk mail
	// produces (data-model.md case 3), and the one the old behaviour split.
	m1 := msg(t, base, 0, "dup@x", "Invoice #1", nil, nil)
	m2 := msg(t, base, 1*time.Hour, "dup@x", "Totally unrelated", nil, nil)

	res := Thread([]Message{m1, m2})

	if len(res.Threads) != 1 {
		t.Fatalf("got %d threads, want 1 (one Message-ID is one message, so one thread)", len(res.Threads))
	}
	assertSameThread(t, res, m1, m2)

	// Neither message may be dropped on the way — the duplicate keeps its own
	// container precisely so nothing is lost.
	if got := len(res.Threads[0].Members); got != 2 {
		t.Fatalf("thread has %d members, want 2 (no message may be lost to the merge)", got)
	}
}

// TestDuplicateMessageIDCannotDisplaceTheOriginal is the security control the
// merge above requires (#210). Joining a thread is allowed — References already
// permits it. REMOVING someone else's message from its thread, stealing its
// position as root, or re-parenting it is not.
//
// ⚠ Without this, "duplicates merge" could be implemented by letting the later
// message take over the id_table entry, which would silently re-home every
// message that references that id — a strictly worse defect than the one being
// fixed, and one the merge test above would still pass.
func TestDuplicateMessageIDCannotDisplaceTheOriginal(t *testing.T) {
	base := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

	// A genuine conversation: root, and a reply that references it.
	root := msg(t, base, 0, "root@x", "Quarterly report", nil, nil)
	reply := msg(t, base, 1*time.Hour, "reply@x", "Re: Quarterly report", []string{"root@x"}, nil)

	// An attacker sends a message claiming the ROOT's Message-ID, and points
	// its own References somewhere else entirely to try to drag the thread off.
	forged := msg(t, base, 2*time.Hour, "root@x", "You have won", []string{"elsewhere@evil"}, nil)

	res := Thread([]Message{root, reply, forged})

	// The forged message may join — that is unavoidable and was already
	// possible via References — but everyone stays in ONE thread. In
	// particular the reply must NOT have been dragged to "elsewhere@evil".
	assertSameThread(t, res, root, reply, forged)

	th := threadOf(t, res, root)

	// The genuine root still roots the thread. If the forged container had
	// claimed the id_table entry, or been made the parent instead of the
	// child, this is what would flip.
	if !th.HasRoot {
		t.Fatalf("thread lost its real root to a forged duplicate")
	}
	if th.Root != root.ID {
		t.Fatalf("thread root is %s, want the genuine root %s (a forged duplicate displaced it)", th.Root, root.ID)
	}

	// And nothing was removed: all three are still present.
	if got := len(th.Members); got != 3 {
		t.Fatalf("thread has %d members, want 3 (a forged duplicate removed a message)", got)
	}
}

func TestReferenceLoopDoesNotHang(t *testing.T) {
	base := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	a := msg(t, base, 0, "a@x", "Loop", []string{"b@x"}, nil)
	b := msg(t, base, 1*time.Hour, "b@x", "Re: Loop", []string{"a@x"}, nil)

	// The test itself is the assertion: a buggy implementation would hang
	// or stack-overflow here instead of returning.
	res := Thread([]Message{a, b})

	if len(res.Threads) != 1 {
		t.Fatalf("got %d threads, want 1 (loop should still group both messages)", len(res.Threads))
	}
	assertSameThread(t, res, a, b)
}

func TestDistinctConversationsSharingSubjectNotMerged(t *testing.T) {
	base := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

	// Two unrelated conversations that happen to share a subject line.
	// Each already threads correctly on its own via real References, so
	// each root already has a distinct References-rooted identity.
	a1 := msg(t, base, 0, "a1@x", "Status update", nil, nil)
	b1 := msg(t, base, 1*time.Hour, "b1@x", "Re: Status update", []string{"a1@x"}, nil)

	a2 := msg(t, base, 2*time.Hour, "a2@y", "Status update", nil, nil)
	b2 := msg(t, base, 3*time.Hour, "b2@y", "Re: Status update", []string{"a2@y"}, nil)

	res := Thread([]Message{a1, b1, a2, b2})

	if len(res.Threads) != 2 {
		t.Fatalf("got %d threads, want 2 (shared subject must not merge distinct conversations)", len(res.Threads))
	}
	assertSameThread(t, res, a1, b1)
	assertSameThread(t, res, a2, b2)
	assertDistinctThreads(t, res, a1, a2)
}

func TestSubjectFallbackLinksListStrippedReply(t *testing.T) {
	base := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	a := msg(t, base, 0, "a@x", "Foo", nil, nil)
	b := msg(t, base, 1*time.Hour, "b@x", "Re: Foo", []string{"a@x"}, nil)
	// A mailing list rewrote this reply's headers and dropped
	// References/In-Reply-To entirely, leaving only the subject as a clue.
	c := msg(t, base, 2*time.Hour, "c@list", "Re: Foo", nil, nil)

	res := Thread([]Message{a, b, c})

	if len(res.Threads) != 1 {
		t.Fatalf("got %d threads, want 1 (subject fallback should recover the stripped reply)", len(res.Threads))
	}
	th := threadOf(t, res, a)
	if !th.HasRoot || th.Root != a.ID {
		t.Fatalf("expected root %s, got HasRoot=%v Root=%s", a.ID, th.HasRoot, th.Root)
	}
	assertOrder(t, th, a, b, c)
}

func TestForwardedMessageStaysSeparate(t *testing.T) {
	base := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	a := msg(t, base, 0, "a@x", "Foo", nil, nil)
	b := msg(t, base, 1*time.Hour, "b@x", "Re: Foo", []string{"a@x"}, nil)
	// Forwarded out of the original thread with no References back to it.
	f := msg(t, base, 2*time.Hour, "f@z", "Fwd: Foo", nil, nil)

	res := Thread([]Message{a, b, f})

	if len(res.Threads) != 2 {
		t.Fatalf("got %d threads, want 2 (a forward must not be folded into the original thread by subject)", len(res.Threads))
	}
	assertSameThread(t, res, a, b)
	assertDistinctThreads(t, res, a, f)
}

func TestNormalizeSubject(t *testing.T) {
	cases := []struct {
		subject     string
		wantBase    string
		wantReply   bool
		wantForward bool
	}{
		{"Foo", "foo", false, false},
		{"Re: Foo", "foo", true, false},
		{"re:Foo", "foo", true, false},
		{"RE: RE: Foo", "foo", true, false},
		{"Fwd: Foo", "foo", false, true},
		{"FW: Foo", "foo", false, true},
		{"[list-name] Re: Foo", "foo", true, false},
		{"Re: [list-name] Foo", "foo", true, false},
		{"  Foo   Bar  ", "foo bar", false, false},
		{"Re:", "", true, false},
		{"", "", false, false},
	}
	for _, c := range cases {
		base, isReply, isFwd := normalizeSubject(c.subject)
		if base != c.wantBase || isReply != c.wantReply || isFwd != c.wantForward {
			t.Errorf("normalizeSubject(%q) = (%q, %v, %v), want (%q, %v, %v)",
				c.subject, base, isReply, isFwd, c.wantBase, c.wantReply, c.wantForward)
		}
	}
}
