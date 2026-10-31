// SPDX-License-Identifier: AGPL-3.0-only

package thread

import (
	"sort"
	"time"

	"github.com/oklog/ulid/v2"
)

// Message is the store-agnostic input to the threader: just enough header
// metadata to run JWZ threading without touching persisted state. ID is our
// application identifier for the message (opaque to this package); the rest
// are RFC 5322 header values as parsed by the ingest layer.
type Message struct {
	// ID is this message's app-level identifier.
	ID ulid.ULID

	// MessageID is the raw RFC 5322 Message-ID (angle brackets stripped or
	// not — this package treats it as an opaque comparison key, matching
	// whatever normalization the caller already applied). May be empty.
	MessageID string

	// References lists the RFC 5322 References header, oldest ancestor
	// first, as parsed into individual Message-IDs.
	References []string

	// InReplyTo lists the RFC 5322 In-Reply-To header's Message-IDs. Used
	// only as the reference of last resort when References is empty.
	InReplyTo []string

	// Subject is the message subject, unmodified.
	Subject string

	// Date is the message's Date header (or receipt time, per the
	// caller's choice); used only to order members within a thread.
	Date time.Time
}

// ThreadResult is one JWZ thread: a root container (possibly synthetic, if
// the true root message was never delivered to this store) plus its member
// messages ordered by Date ascending.
type ThreadResult struct {
	// RootMessageID is the Message-ID of the container acting as the
	// thread's root, if known. It may not correspond to any message in
	// Members when the true root is missing from the input set (an empty
	// container standing in for an absent ancestor) — see HasRoot.
	RootMessageID string

	// Root is the app ID of the root message. Only meaningful when
	// HasRoot is true.
	Root ulid.ULID

	// HasRoot reports whether Root identifies a real input message (true)
	// or the root container is a dummy standing in for a missing/unknown
	// ancestor (false, Root is the zero ULID).
	HasRoot bool

	// Members holds every input message assigned to this thread, ordered
	// by Date ascending. Ties (equal Date) preserve input order.
	Members []ulid.ULID
}

// Result is the output of Thread: the discovered threads, plus a lookup
// from each input message's app ID to the index of its thread in Threads.
type Result struct {
	Threads  []ThreadResult
	ThreadOf map[ulid.ULID]int
}

// Thread groups msgs into conversations using JWZ threading with the
// restricted subject fallback documented in doc.go. It never mutates msgs
// and never fails: malformed or adversarial input (missing, duplicate, or
// self/mutually-referential Message-IDs) degrades to a safe grouping rather
// than erroring, matching JWZ's own defensive design.
func Thread(msgs []Message) Result {
	b := newBuilder(len(msgs))

	containers := make([]*container, len(msgs))
	for i := range msgs {
		containers[i] = b.attachMessage(&msgs[i])
	}
	for i := range msgs {
		linkMessage(b, &msgs[i], containers[i])
	}

	roots := b.rootSet()
	roots = pruneRoots(roots)
	roots = restrictedSubjectFallback(roots)

	return assemble(roots)
}

func assemble(roots []*container) Result {
	res := Result{
		Threads:  make([]ThreadResult, 0, len(roots)),
		ThreadOf: make(map[ulid.ULID]int),
	}
	for _, root := range roots {
		var members []*Message
		collectMessages(root, &members)

		sort.SliceStable(members, func(i, j int) bool {
			return members[i].Date.Before(members[j].Date)
		})

		tr := ThreadResult{
			RootMessageID: root.id,
			Members:       make([]ulid.ULID, len(members)),
		}
		if root.message != nil {
			tr.Root = root.message.ID
			tr.HasRoot = true
		}

		idx := len(res.Threads)
		for i, m := range members {
			tr.Members[i] = m.ID
			res.ThreadOf[m.ID] = idx
		}
		res.Threads = append(res.Threads, tr)
	}
	return res
}

// collectMessages walks c's subtree depth-first, appending every real
// (non-dummy) message found.
func collectMessages(c *container, out *[]*Message) {
	if c.message != nil {
		*out = append(*out, c.message)
	}
	for _, child := range c.children {
		collectMessages(child, out)
	}
}
