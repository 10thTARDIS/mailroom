// SPDX-License-Identifier: AGPL-3.0-only

package main

// `mailroom-admin diff-duplicates` — WHY does the same message occupy several rows? (#206)
//
// THIS COMMAND EXISTS TO ANSWER ONE QUESTION AND IT WRITES NOTHING. A real
// inbox shows some messages 3–4 times. They are real, separate rows: same Message-ID,
// same subject, same tracking token, arriving seconds apart, differing by 1–6 bytes out
// of ~19KB. ADR-001 hashes the raw bytes as identity and says a transport that mutates
// bytes creates a second message, so re-delivery is invisible to dedup by design.
//
// The fix depends entirely on WHERE those few bytes differ:
//
//   * in a per-delivery TRACE HEADER (Received:, Return-Path:, …) — then a dedup hash
//     computed over a trace-stripped copy collapses them, and that is a clean fix;
//   * in the BODY (a per-copy tracking token) — then stripping headers would ship and
//     change nothing at all.
//
// Guessing which costs a release. So this decrypts the copies and reports the answer,
// per the standing lesson: build the report before the fix whenever "it ran and changed
// nothing" is a plausible outcome (#177).
//
// It is a DIAGNOSTIC, not a repair. There is no -apply, no write path, and no store
// method it calls can mutate. Deliberately: a tool that can also fix invites being run
// before its own output has been read.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"flag"
	"fmt"
	"net/mail"
	"sort"
	"strings"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/crypto"
	"mailroom/internal/ingest"
	"mailroom/internal/store"
	"mailroom/internal/syncrunner"
)

// traceHeaders are the fields a mail path rewrites or adds PER DELIVERY. They are the
// candidate explanation for two copies of one message differing by a few bytes, and the
// set a dedup hash would strip.
//
// Named explicitly rather than pattern-matched on `X-`: plenty of X- headers are part of
// the message the sender composed (X-Mailer, X-Campaign-Id) and stripping those would
// merge messages that genuinely differ.
var traceHeaders = []string{
	"Received",
	"Return-Path",
	"Delivered-To",
	"Envelope-To",
	"X-Original-To",
	"X-Forwarded-To",
	"X-Forwarded-For",
	"X-Envelope-To",
	"X-Envelope-From",
	"X-Delivered-To",
	"Authentication-Results",
	"ARC-Authentication-Results",
	"ARC-Message-Signature",
	"ARC-Seal",
	"Received-SPF",
	"X-Spam-Status",
	"X-Spam-Score",
	"X-Spam-Level",
	"X-Spam-Checker-Version",
	"X-Virus-Scanned",
}

func runDiffDuplicates(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("diff-duplicates", flag.ContinueOnError)
	var (
		userIDStr = fs.String("user-id", "", "operator user id (ULID)")
		groups    = fs.Int("groups", 50, "how many duplicate groups to examine")
		show      = fs.Int("show", 3, "how many groups to print a detailed diff for")
		context_  = fs.Int("context", 90, "bytes of context to print around a body difference")
	)
	if err := fs.Parse(args); err != nil {
		return err
	}
	userID, err := parseUserID(*userIDStr)
	if err != nil {
		return err
	}

	d, err := openDeps(ctx)
	if err != nil {
		return err
	}
	defer d.close()

	blobs, err := openBlobStore(ctx, d)
	if err != nil {
		return err
	}
	master, err := d.keys.MasterKey(ctx)
	if err != nil {
		return fmt.Errorf("master key: %w", err)
	}
	dek, err := crypto.DeriveKey(master, userID[:], syncrunner.MailBlobInfoV1)
	crypto.Zero(master)
	if err != nil {
		return fmt.Errorf("derive mail dek: %w", err)
	}
	defer crypto.Zero(dek)

	var found []store.DuplicateGroup
	if err := d.store.UserTx(ctx, userID, func(tx *store.UserTx) error {
		var e error
		found, e = tx.DuplicateMessageIDGroups(ctx, *groups)
		return e
	}); err != nil {
		return fmt.Errorf("find duplicate groups: %w", err)
	}

	if len(found) == 0 {
		fmt.Println("no Message-ID is held by more than one message — nothing to diff")
		return nil
	}

	// The tally that actually answers the question. Every group lands in exactly one
	// bucket, so the buckets are a partition and the numbers can be trusted to add up.
	var (
		copies                   int
		sameAfterStrip           int // identical once trace headers are removed -> FIXABLE
		headersDifferBeyondTrace int // headers differ in a field we would NOT strip
		bodiesDiffer             int // the body itself differs -> NOT fixable this way
		unreadable               int
		differingFields          = map[string]int{}
		beyondTraceFields        = map[string]int{}
		detailed                 int
		// Whether the PROPOSED fix (ingest.DedupHash) actually agrees across each
		// group's real bytes.
		dedupWouldCollapse int
		dedupWouldNot      int
	)

	for _, g := range found {
		copies += len(g.Copies)

		raws := make([][]byte, 0, len(g.Copies))
		ok := true
		for _, c := range g.Copies {
			raw, err := readMailBlob(ctx, blobs, userID, dek, c.ContentHash)
			if err != nil {
				d.log.Warn("diff-duplicates: read blob failed", "message", c.ID, "err", err)
				ok = false
				break
			}
			raws = append(raws, raw)
		}
		if !ok || len(raws) < 2 {
			unreadable++
			continue
		}

		// THE ACTUAL TEST OF THE PROPOSED FIX, against real bytes. The unit tests in
		// internal/ingest use synthetic messages; this asks whether ingest.DedupHash
		// collapses the copies that are really in the database. Without this the fix
		// would ship on the strength of tests I wrote to agree with it.
		if dedupAgrees(raws) {
			dedupWouldCollapse++
		} else {
			dedupWouldNot++
		}

		verdict, fields := classifyGroup(raws)
		switch verdict {
		case verdictSameAfterStrip:
			sameAfterStrip++
			for _, f := range fields {
				differingFields[f]++
			}
		case verdictHeadersBeyondTrace:
			headersDifferBeyondTrace++
			for _, f := range fields {
				beyondTraceFields[f]++
			}
		case verdictBodiesDiffer:
			bodiesDiffer++
		}

		if detailed < *show {
			detailed++
			printGroupDetail(g, raws, verdict, fields, *context_)
		}
	}

	fmt.Printf("\n=== %d duplicate Message-ID groups, %d copies ===\n", len(found), copies)
	fmt.Printf("  identical once trace headers are stripped : %d   <- a dedup hash WOULD collapse these\n", sameAfterStrip)
	fmt.Printf("  headers differ beyond the trace set       : %d\n", headersDifferBeyondTrace)
	fmt.Printf("  bodies genuinely differ                   : %d   <- a dedup hash would NOT help these\n", bodiesDiffer)
	fmt.Printf("  unreadable (blob missing or undecryptable) : %d\n", unreadable)
	fmt.Printf("\n  ingest.DedupHash agrees across the group   : %d   <- the proposed fix, on REAL bytes\n",
		dedupWouldCollapse)
	fmt.Printf("  ingest.DedupHash still disagrees           : %d\n", dedupWouldNot)

	if len(differingFields) > 0 {
		fmt.Println("\nthe trace fields that actually differed between copies:")
		for _, kv := range sortedCounts(differingFields) {
			fmt.Printf("  %-32s %d groups\n", kv.k, kv.v)
		}
	}
	if len(beyondTraceFields) > 0 {
		fmt.Println("\nfields differing OUTSIDE the trace set — each one is a decision to make:")
		for _, kv := range sortedCounts(beyondTraceFields) {
			fmt.Printf("  %-32s %d groups\n", kv.k, kv.v)
		}
	}

	// The verdict, stated so nobody has to interpret the table. It is about
	// ingest.DedupHash rather than about the header classification, because DedupHash
	// is what would actually ship.
	readable := len(found) - unreadable
	fmt.Println()
	switch {
	case dedupWouldCollapse == readable && readable > 0:
		fmt.Printf("VERDICT: ingest.DedupHash collapses ALL %d readable groups on real bytes.\n", readable)
		fmt.Println("         The fix is measured, not assumed. Safe to wire into ingest.")
	case dedupWouldCollapse > 0:
		fmt.Printf("VERDICT: PARTIAL. ingest.DedupHash collapses %d of %d readable groups.\n",
			dedupWouldCollapse, readable)
		fmt.Println("         Read the detail above: the rest need a reason, not a wider hash.")
	default:
		fmt.Println("VERDICT: ingest.DedupHash collapses NOTHING. Shipping it would change nothing.")
		fmt.Println("         Do not wire it in — find out why first.")
	}
	return nil
}

// dedupAgrees reports whether ingest.DedupHash is identical across every copy — i.e.
// whether the proposed fix would in fact treat them as one message.
func dedupAgrees(raws [][]byte) bool {
	first := ingest.DedupHash(raws[0])
	for i := 1; i < len(raws); i++ {
		if ingest.DedupHash(raws[i]) != first {
			return false
		}
	}
	return true
}

type verdict int

const (
	verdictSameAfterStrip verdict = iota
	verdictHeadersBeyondTrace
	verdictBodiesDiffer
)

// classifyGroup decides why the copies differ, and names the header fields involved.
//
// Checked in this order because it is the order of consequence: a body difference cannot
// be fixed by header stripping at all, so it is decided first; only then does it matter
// which headers differ.
func classifyGroup(raws [][]byte) (verdict, []string) {
	bodies := make([][]byte, len(raws))
	headers := make([]mail.Header, len(raws))
	for i, raw := range raws {
		h, body := splitMessage(raw)
		bodies[i] = body
		headers[i] = h
	}
	for i := 1; i < len(bodies); i++ {
		if !bytes.Equal(bodies[0], bodies[i]) {
			return verdictBodiesDiffer, nil
		}
	}

	// Bodies match. Which header fields differ?
	differing := map[string]bool{}
	for i := 1; i < len(headers); i++ {
		for _, f := range fieldsDiffering(headers[0], headers[i]) {
			differing[f] = true
		}
	}
	var trace, beyond []string
	for f := range differing {
		if isTraceHeader(f) {
			trace = append(trace, f)
		} else {
			beyond = append(beyond, f)
		}
	}
	sort.Strings(trace)
	sort.Strings(beyond)
	if len(beyond) > 0 {
		return verdictHeadersBeyondTrace, beyond
	}
	return verdictSameAfterStrip, trace
}

// fieldsDiffering names every header field whose values are not identical between two
// messages. Compares the FULL value list per field, so a repeated header (several
// Received: lines) counts as different when the lines differ or the count does.
func fieldsDiffering(a, b mail.Header) []string {
	seen := map[string]bool{}
	for f := range a {
		seen[f] = true
	}
	for f := range b {
		seen[f] = true
	}
	var out []string
	for f := range seen {
		if !equalStrings(a[f], b[f]) {
			out = append(out, f)
		}
	}
	return out
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

func isTraceHeader(field string) bool {
	for _, t := range traceHeaders {
		if strings.EqualFold(field, t) {
			return true
		}
	}
	return false
}

// splitMessage separates the header block from the body, tolerating both CRLF and bare
// LF separators. A message that will not parse yields empty headers and the whole thing
// as the body, so it is classified as a body difference — the conservative answer, since
// a message we cannot parse is one whose headers we cannot claim to have compared.
func splitMessage(raw []byte) (mail.Header, []byte) {
	m, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		return mail.Header{}, raw
	}
	body := new(bytes.Buffer)
	if _, err := body.ReadFrom(m.Body); err != nil {
		return m.Header, raw
	}
	return m.Header, body.Bytes()
}

func printGroupDetail(g store.DuplicateGroup, raws [][]byte, v verdict, fields []string, ctxBytes int) {
	fmt.Printf("\n--- Message-ID %s (%d copies)\n", trimID(g.MessageID), len(g.Copies))
	for i, c := range g.Copies {
		hdr, body := splitMessage(raws[i])
		fmt.Printf("    %s  %s  %6d bytes  body %6d  hdr-sha %s  thread %s\n",
			c.ID.String()[:10], c.ReceivedAt.UTC().Format("15:04:05"), c.Size, len(body),
			shortSHA(headerBytes(raws[i])), trimID(c.ThreadID))
		_ = hdr
	}
	switch v {
	case verdictSameAfterStrip:
		fmt.Printf("    => IDENTICAL after stripping trace headers. Differing: %s\n",
			strings.Join(fields, ", "))
	case verdictHeadersBeyondTrace:
		fmt.Printf("    => bodies match, but these NON-trace fields differ: %s\n",
			strings.Join(fields, ", "))
	case verdictBodiesDiffer:
		fmt.Println("    => THE BODIES DIFFER. Header stripping cannot collapse these.")
		printFirstBodyDifference(raws, ctxBytes)
	}
}

// printFirstBodyDifference shows where two bodies first diverge, with context, so a
// human can see whether it is a tracking token, a timestamp, or something else. This is
// the whole point of the command in the case that matters most.
func printFirstBodyDifference(raws [][]byte, ctxBytes int) {
	_, a := splitMessage(raws[0])
	for i := 1; i < len(raws); i++ {
		_, b := splitMessage(raws[i])
		if bytes.Equal(a, b) {
			continue
		}
		at := firstDifference(a, b)
		fmt.Printf("       first body difference at byte %d of %d/%d:\n", at, len(a), len(b))
		fmt.Printf("         copy 1: %s\n", window(a, at, ctxBytes))
		fmt.Printf("         copy %d: %s\n", i+1, window(b, at, ctxBytes))
		return
	}
}

func firstDifference(a, b []byte) int {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	for i := 0; i < n; i++ {
		if a[i] != b[i] {
			return i
		}
	}
	return n
}

// window renders bytes around an offset as printable text, so a token is readable rather
// than a hex dump. Control bytes become '.' — a diagnostic that scrambles the terminal is
// one nobody runs twice.
func window(b []byte, at, size int) string {
	lo := at - size/2
	if lo < 0 {
		lo = 0
	}
	hi := lo + size
	if hi > len(b) {
		hi = len(b)
	}
	var sb strings.Builder
	for _, c := range b[lo:hi] {
		if c < 0x20 || c > 0x7e {
			sb.WriteByte('.')
			continue
		}
		sb.WriteByte(c)
	}
	return sb.String()
}

func headerBytes(raw []byte) []byte {
	if i := bytes.Index(raw, []byte("\r\n\r\n")); i >= 0 {
		return raw[:i]
	}
	if i := bytes.Index(raw, []byte("\n\n")); i >= 0 {
		return raw[:i]
	}
	return raw
}

func shortSHA(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:4])
}

func trimID(s string) string {
	s = strings.Trim(s, "<>")
	if len(s) > 34 {
		return s[:34] + "…"
	}
	if s == "" {
		return "(none)"
	}
	return s
}

type kv struct {
	k string
	v int
}

func sortedCounts(m map[string]int) []kv {
	out := make([]kv, 0, len(m))
	for k, v := range m {
		out = append(out, kv{k, v})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].v != out[j].v {
			return out[i].v > out[j].v
		}
		return out[i].k < out[j].k
	})
	return out
}

var _ = ulid.ULID{} // keep the import honest if the detail printer changes
