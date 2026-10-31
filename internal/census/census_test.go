// SPDX-License-Identifier: AGPL-3.0-only

package census

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"
)

// --- HasAttachment -----------------------------------------------------------

func TestHasAttachment(t *testing.T) {
	cases := []struct {
		name string
		part *Part
		want bool
	}{
		{
			name: "text/plain only",
			part: &Part{MediaType: "text/plain"},
			want: false,
		},
		{
			name: "multipart/alternative text+html",
			part: &Part{MediaType: "multipart/alternative", Children: []*Part{
				{MediaType: "text/plain"},
				{MediaType: "text/html"},
			}},
			want: false,
		},
		{
			name: "multipart/mixed with application/pdf",
			part: &Part{MediaType: "multipart/mixed", Children: []*Part{
				{MediaType: "text/plain"},
				{MediaType: "application/pdf"},
			}},
			want: true,
		},
		{
			name: "text/plain with disposition attachment",
			part: &Part{MediaType: "text/plain", Disposition: "attachment"},
			want: true,
		},
		{
			name: "inline image/png inside multipart/related",
			part: &Part{MediaType: "multipart/related", Children: []*Part{
				{MediaType: "text/html"},
				{MediaType: "image/png", Disposition: "inline"},
			}},
			want: true,
		},
		{
			name: "multipart/mixed with text + message/rfc822",
			part: &Part{MediaType: "multipart/mixed", Children: []*Part{
				{MediaType: "text/plain"},
				{MediaType: "message/rfc822"},
			}},
			want: false,
		},
		{
			name: "top-level application/pdf single part",
			part: &Part{MediaType: "application/pdf"},
			want: true,
		},
		{
			name: "top-level message/rfc822 (whole message is a forwarded message)",
			part: &Part{MediaType: "message/rfc822"},
			want: false,
		},
		{
			name: "nil structure (server returned none)",
			part: nil,
			want: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := HasAttachment(tc.part); got != tc.want {
				t.Errorf("HasAttachment(%+v) = %v, want %v", tc.part, got, tc.want)
			}
		})
	}
}

// --- specialUse ----------------------------------------------------------------

func TestSpecialUse(t *testing.T) {
	cases := []struct {
		name  string
		attrs []string
		want  string
	}{
		{
			name:  "only structural attributes, lowercased (as imap.go actually produces them) — none are a role",
			attrs: []string{`\hasnochildren`, `\subscribed`, `\noinferiors`},
			want:  "-",
		},
		{
			name:  "no attributes at all",
			attrs: nil,
			want:  "-",
		},
		{
			name:  "one real role among structural attributes",
			attrs: []string{`\hasnochildren`, `\subscribed`, `\sent`},
			want:  `\Sent`,
		},
		{
			name:  "two roles: both shown, in specialUseDisplayRoles' fixed order regardless of input order",
			attrs: []string{`\flagged`, `\hasnochildren`, `\archive`},
			want:  `\Archive,\Flagged`,
		},
		{
			name:  "RFC 8457 \\Important is recognized",
			attrs: []string{`\important`},
			want:  `\Important`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := specialUse(tc.attrs); got != tc.want {
				t.Errorf("specialUse(%v) = %q, want %q", tc.attrs, got, tc.want)
			}
		})
	}
}

// --- writeProbe ------------------------------------------------------------

// TestWriteProbeRenderings pins the four renderings the #529 review named:
// not run, held-all (a lower bound, no refusal), refused after holding some,
// and the Held==0 "first connection failed" case — which must NOT render as
// "refused at connection 1 after holding 0" (that phrasing reads as a
// measured cap of zero, when nothing was actually measured: a wrong
// password or a DNS failure looks identical to a real cap-of-zero until the
// error text is read).
func TestWriteProbeRenderings(t *testing.T) {
	cases := []struct {
		name string
		p    *ProbeResult
		want []string // every substring must appear
		deny []string // no substring may appear
	}{
		{
			name: "not run: nil",
			p:    nil,
			want: []string{"not run"},
			deny: []string{"scope:"},
		},
		{
			name: "not run: Attempted false",
			p:    &ProbeResult{Attempted: false},
			want: []string{"not run"},
			deny: []string{"scope:"},
		},
		{
			name: "held all requested, no refusal: a LOWER BOUND",
			p:    &ProbeResult{Attempted: true, Max: 5, Held: 5},
			want: []string{"held 5 of max 5", "at least 5 sessions fit", "lower bound", "scope:"},
		},
		{
			name: "refused after holding some",
			p: &ProbeResult{Attempted: true, Max: 15, Held: 10, Refused: true,
				RefusalError: "too many connections"},
			want: []string{"refused at connection 11 after holding 10", "too many connections", "scope:"},
			deny: []string{"FIRST connection failed"},
		},
		{
			name: "the first connection failed: Held == 0 must not read as a measured cap of zero",
			p: &ProbeResult{Attempted: true, Max: 15, Held: 0, Refused: true,
				RefusalError: "authentication failed"},
			want: []string{"the FIRST connection failed", "no cap was measured", "authentication failed", "scope:"},
			deny: []string{"refused at connection 1 after holding 0"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			writeProbe(&buf, tc.p)
			out := buf.String()
			for _, want := range tc.want {
				if !strings.Contains(out, want) {
					t.Errorf("writeProbe(%+v) output =\n%s\nwant it to contain %q", tc.p, out, want)
				}
			}
			for _, deny := range tc.deny {
				if strings.Contains(out, deny) {
					t.Errorf("writeProbe(%+v) output =\n%s\nwant it to NOT contain %q", tc.p, out, deny)
				}
			}
		})
	}
}

// --- percentile / sizeDistribution --------------------------------------------

func TestPercentile(t *testing.T) {
	// A known set: 10..100 in steps of 10 (10 values), so nearest-rank
	// percentiles land on exact, easily hand-verified indices.
	sorted := []int64{10, 20, 30, 40, 50, 60, 70, 80, 90, 100}

	cases := []struct {
		p    float64
		want int64
	}{
		{0.50, 50},  // ceil(0.5*10)=5 -> index 4 -> 50
		{0.90, 90},  // ceil(0.9*10)=9 -> index 8 -> 90
		{0.99, 100}, // ceil(0.99*10)=10 -> index 9 -> 100
	}
	for _, tc := range cases {
		if got := percentile(sorted, tc.p); got != tc.want {
			t.Errorf("percentile(sorted, %v) = %d, want %d", tc.p, got, tc.want)
		}
	}
}

func TestSizeDistribution(t *testing.T) {
	dist := sizeDistribution([]int64{10, 20, 30, 40, 50, 60, 70, 80, 90, 100})
	if dist.Mean != 55 {
		t.Errorf("Mean = %v, want 55", dist.Mean)
	}
	if dist.P50 != 50 || dist.P90 != 90 || dist.P99 != 100 {
		t.Errorf("P50/P90/P99 = %d/%d/%d, want 50/90/100", dist.P50, dist.P90, dist.P99)
	}
	if dist.Max != 100 {
		t.Errorf("Max = %d, want 100", dist.Max)
	}

	if got := sizeDistribution(nil); got != (SizeDistribution{}) {
		t.Errorf("sizeDistribution(nil) = %+v, want the zero value", got)
	}
}

// --- Run, over a fake Reader ---------------------------------------------------

// fakeReader is a Reader built entirely from in-memory stubs, for Run's unit
// tests. It tracks which mailboxes Examine was actually called for, so a
// test can assert a \Noselect mailbox was never touched at all — not merely
// that its report row is empty.
type fakeReader struct {
	caps      []string
	mailboxes []Mailbox
	stubs     map[string]func(fn func(Meta) error) (Examined, error)
	called    map[string]bool
}

func (f *fakeReader) Capabilities(context.Context) ([]string, error) { return f.caps, nil }
func (f *fakeReader) ListMailboxes(context.Context) ([]Mailbox, error) {
	return f.mailboxes, nil
}
func (f *fakeReader) Examine(_ context.Context, name string, fn func(Meta) error) (Examined, error) {
	if f.called == nil {
		f.called = map[string]bool{}
	}
	f.called[name] = true
	stub, ok := f.stubs[name]
	if !ok {
		return Examined{}, fmt.Errorf("fakeReader: unexpected Examine(%q); no stub registered", name)
	}
	return stub(fn)
}
func (f *fakeReader) Close() error { return nil }

var _ Reader = (*fakeReader)(nil)

func textPart() *Part { return &Part{MediaType: "text/plain"} }

func attachmentPart() *Part {
	return &Part{MediaType: "multipart/mixed", Children: []*Part{
		textPart(),
		{MediaType: "application/pdf"},
	}}
}

func TestRun(t *testing.T) {
	est := time.FixedZone("EST", -5*3600)

	fake := &fakeReader{
		// Deliberately no "IMAP4rev2": that capability's IMPLICATIONS
		// (UIDPLUS/MOVE/IDLE/…) are exercised by
		// TestBuildSupportsAdvertisedVsEffective instead, so this test's own
		// Supports assertions can stay a simple "advertised == effective"
		// check without the two ever needing to differ here.
		caps: []string{"CONDSTORE", "MOVE"},
		mailboxes: []Mailbox{
			{Name: "INBOX"},
			{Name: "Sent", Attributes: []string{"\\sent"}},
			{Name: "All Mail", Attributes: []string{"\\all"}},
			{Name: "Noselect Folder", Attributes: []string{"\\noselect"}},
			{Name: "Broken", Attributes: nil},
			{Name: "Mismatch", Attributes: nil},
		},
		stubs: map[string]func(fn func(Meta) error) (Examined, error){
			"INBOX": func(fn func(Meta) error) (Examined, error) {
				msgs := []Meta{
					{
						// 2024-12-31T23:30:00-05:00 == 2025-01-01T04:30:00Z:
						// the whole point of this fixture is that the UTC
						// year (2025) differs from the naive local year
						// (2024), and ByYear must bucket by the UTC one.
						UID:          1,
						Flags:        []string{"\\Seen"},
						InternalDate: time.Date(2024, 12, 31, 23, 30, 0, 0, est),
						Size:         100,
						MessageID:    "<a@x>",
						Structure:    textPart(),
					},
					{
						UID:          2,
						Flags:        []string{"\\Seen", "\\Draft"},
						InternalDate: time.Date(2025, 6, 1, 12, 0, 0, 0, time.UTC),
						Size:         200,
						MessageID:    "<a@x>", // same Message-ID as UID 1: a shared pair
						Structure:    attachmentPart(),
					},
					{
						UID:          3,
						InternalDate: time.Date(2023, 1, 1, 0, 0, 0, 0, time.UTC),
						Size:         300,
						MessageID:    "", // no Message-ID at all
						Structure:    nil,
					},
				}
				for _, m := range msgs {
					if err := fn(m); err != nil {
						return Examined{}, err
					}
				}
				return Examined{Messages: uint32(len(msgs))}, nil
			},
			"Sent": func(func(Meta) error) (Examined, error) {
				return Examined{}, nil // empty mailbox
			},
			"All Mail": func(fn func(Meta) error) (Examined, error) {
				// A virtual mailbox message with a Message-ID that WOULD
				// collide with INBOX's pair, and a size that WOULD move
				// every aggregate if counted — proving it is excluded, not
				// merely small enough not to matter.
				if err := fn(Meta{
					UID:          99,
					InternalDate: time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC),
					Size:         999_999,
					MessageID:    "<a@x>",
				}); err != nil {
					return Examined{}, err
				}
				return Examined{Messages: 1}, nil
			},
			"Broken": func(fn func(Meta) error) (Examined, error) {
				// Streams ONE message before failing, so the test can prove
				// that partial data from a failed mailbox does not leak
				// into the totals.
				_ = fn(Meta{UID: 1, Size: 555_555, InternalDate: time.Now()})
				return Examined{}, errors.New("boom: connection reset")
			},
			"Mismatch": func(fn func(Meta) error) (Examined, error) {
				// Streams ONE message but reports EXAMINE saw 5 — the
				// "go-imap silently dropped a FETCH item" scenario item 4
				// (#529 review) exists to catch. The one message streamed
				// also has no Structure, exercising NoStructure at the same
				// time; its Message-ID is deliberately empty too.
				if err := fn(Meta{
					UID:          1,
					InternalDate: time.Date(2025, 3, 1, 0, 0, 0, 0, time.UTC),
					Size:         50,
					Structure:    nil,
				}); err != nil {
					return Examined{}, err
				}
				return Examined{Messages: 5}, nil
			},
			// Deliberately NO stub for "Noselect Folder": Run must never
			// call Examine on it at all.
		},
	}

	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	rep, err := Run(context.Background(), fake, now)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	if fake.called["Noselect Folder"] {
		t.Error("Run() called Examine on a \\Noselect mailbox; it must never be examined")
	}

	if !rep.Partial {
		t.Error("Partial = false, want true (Broken's Examine failed)")
	}
	var brokenErr string
	for _, mb := range rep.Mailboxes {
		if mb.Name == "Broken" {
			brokenErr = mb.Error
		}
	}
	if brokenErr == "" {
		t.Error("Mailboxes[Broken].Error is empty, want the Examine error")
	}

	// Totals: INBOX + Mismatch. All Mail (virtual) and Broken (failed) must
	// contribute NOTHING, even though both streamed a message that would
	// have been very visible in the aggregate if it leaked in.
	if rep.Totals.Messages != 4 {
		t.Errorf("Totals.Messages = %d, want 4 (3 from INBOX + 1 from Mismatch)", rep.Totals.Messages)
	}
	if rep.Totals.Bytes != 650 {
		t.Errorf("Totals.Bytes = %d, want 650 (100+200+300+50; NOT +999999 or +555555)", rep.Totals.Bytes)
	}
	if rep.Totals.AttachmentBearing != 1 {
		t.Errorf("Totals.AttachmentBearing = %d, want 1", rep.Totals.AttachmentBearing)
	}
	if rep.Totals.Drafts != 1 {
		t.Errorf("Totals.Drafts = %d, want 1", rep.Totals.Drafts)
	}
	if rep.Totals.NoStructure != 2 {
		t.Errorf("Totals.NoStructure = %d, want 2 (INBOX's UID 3 + Mismatch's UID 1)", rep.Totals.NoStructure)
	}

	// ByYear: UID1 lands in UTC 2025 (not 2024), UID2 in 2025, UID3 in 2023,
	// Mismatch's message in 2025. All Mail's 2025-dated message must NOT add
	// a fourth to the 2025 bucket.
	byYear := map[int]YearCount{}
	for _, y := range rep.ByYear {
		byYear[y.Year] = y
	}
	if y := byYear[2025]; y.Messages != 3 || y.Bytes != 350 {
		t.Errorf("ByYear[2025] = %+v, want {Messages:3 Bytes:350}", y)
	}
	if y := byYear[2023]; y.Messages != 1 || y.Bytes != 300 {
		t.Errorf("ByYear[2023] = %+v, want {Messages:1 Bytes:300}", y)
	}
	if _, ok := byYear[2024]; ok {
		t.Error("ByYear has a 2024 bucket; UID1's local-time year must not have been used")
	}

	// Message-IDs: UID1+UID2 share "<a@x>" (2 messages, 1 group); UID3 and
	// Mismatch's message have none (missing). All Mail's colliding
	// Message-ID must not inflate this.
	if rep.MessageIDs.Distinct != 1 {
		t.Errorf("MessageIDs.Distinct = %d, want 1", rep.MessageIDs.Distinct)
	}
	if rep.MessageIDs.Missing != 2 {
		t.Errorf("MessageIDs.Missing = %d, want 2", rep.MessageIDs.Missing)
	}
	if rep.MessageIDs.SharedMessages != 2 {
		t.Errorf("MessageIDs.SharedMessages = %d, want 2", rep.MessageIDs.SharedMessages)
	}
	if rep.MessageIDs.SharedGroups != 1 {
		t.Errorf("MessageIDs.SharedGroups = %d, want 1", rep.MessageIDs.SharedGroups)
	}

	// Roles: Sent maps to "Sent"; everything else in censusRoles is missing.
	if got := rep.Roles["sent"]; len(got) != 1 || got[0] != "Sent" {
		t.Errorf(`Roles["sent"] = %v, want ["Sent"]`, got)
	}
	for _, role := range []string{"drafts", "trash", "junk", "archive", "flagged"} {
		if got := rep.Roles[role]; len(got) != 0 {
			t.Errorf("Roles[%q] = %v, want empty (missing)", role, got)
		}
	}
	// "all" IS present (All Mail carries \All) — it is a recognized role,
	// just one day-one setup excludes from anything actionable; the census
	// still reports it.
	if got := rep.Roles["all"]; len(got) != 1 || got[0] != "All Mail" {
		t.Errorf(`Roles["all"] = %v, want ["All Mail"]`, got)
	}

	// Virtual/Selectable flags on the mailbox rows themselves.
	byName := map[string]MailboxReport{}
	for _, mb := range rep.Mailboxes {
		byName[mb.Name] = mb
	}
	if !byName["All Mail"].Virtual {
		t.Error(`Mailboxes["All Mail"].Virtual = false, want true`)
	}
	if byName["INBOX"].Virtual {
		t.Error(`Mailboxes["INBOX"].Virtual = true, want false`)
	}
	if byName["Noselect Folder"].Selectable {
		t.Error(`Mailboxes["Noselect Folder"].Selectable = true, want false`)
	}

	// Supports: tracked capabilities reflect exactly what fake.caps
	// advertised, on BOTH Advertised and Effective (nothing in fake.caps
	// here implies anything else — see TestBuildSupportsAdvertisedVsEffective
	// for the cases where the two genuinely differ).
	for _, c := range []string{"CONDSTORE", "MOVE"} {
		if v := rep.Supports[c]; !v.Advertised || !v.Effective {
			t.Errorf("Supports[%q] = %+v, want {Advertised:true Effective:true}", c, v)
		}
	}
	for _, c := range []string{"IMAP4REV2", "QRESYNC", "IDLE", "UIDPLUS", "SPECIAL-USE"} {
		if v := rep.Supports[c]; v.Advertised || v.Effective {
			t.Errorf("Supports[%q] = %+v, want {Advertised:false Effective:false}", c, v)
		}
	}

	// ExaminedMessages/NoStructure per mailbox.
	if got := byName["INBOX"].ExaminedMessages; got != 3 {
		t.Errorf("Mailboxes[INBOX].ExaminedMessages = %d, want 3 (matches streamed; no mismatch)", got)
	}
	if got := byName["INBOX"].NoStructure; got != 1 {
		t.Errorf("Mailboxes[INBOX].NoStructure = %d, want 1 (UID 3 had Structure == nil)", got)
	}
	if got := byName["Mismatch"].ExaminedMessages; got != 5 {
		t.Errorf("Mailboxes[Mismatch].ExaminedMessages = %d, want 5", got)
	}
	if got := byName["Mismatch"].Messages; got != 1 {
		t.Errorf("Mailboxes[Mismatch].Messages = %d, want 1", got)
	}
	if got := byName["Mismatch"].NoStructure; got != 1 {
		t.Errorf("Mailboxes[Mismatch].NoStructure = %d, want 1", got)
	}

	// All Mail's own fixture message also has no Structure set — NoStructure
	// is tracked per mailbox REGARDLESS of Virtual (only the totals/warnings
	// question of whether it counts toward the aggregate is virtual-gated),
	// so it gets a warning too even though it never touches Totals.
	if got := byName["All Mail"].NoStructure; got != 1 {
		t.Errorf("Mailboxes[All Mail].NoStructure = %d, want 1", got)
	}

	// Warnings: exactly the count-mismatch and NoStructure findings, each
	// naming the mailbox it came from — Broken (an Examine FAILURE) must
	// not appear here, only in Partial/Error.
	wantSubstrings := []string{
		"Mismatch: EXAMINE reported 5, FETCH returned 1",
		"Mismatch: 1 message(s) had no BODYSTRUCTURE",
		"INBOX: 1 message(s) had no BODYSTRUCTURE",
		"All Mail: 1 message(s) had no BODYSTRUCTURE",
	}
	for _, want := range wantSubstrings {
		found := false
		for _, w := range rep.Warnings {
			if strings.Contains(w, want) {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("Warnings = %v, want an entry containing %q", rep.Warnings, want)
		}
	}
	if len(rep.Warnings) != len(wantSubstrings) {
		t.Errorf("len(Warnings) = %d, want exactly %d: %v", len(rep.Warnings), len(wantSubstrings), rep.Warnings)
	}
}

// TestBuildSupportsAdvertisedVsEffective pins the three cases the #529
// review named explicitly: a capability can be EFFECTIVE (go-imap's own
// imap.CapSet.Has(), which is what internal/sync's engine actually queries)
// without being ADVERTISED verbatim, because IMAP4rev2 implies UIDPLUS and
// QRESYNC implies CONDSTORE (RFC 9051/RFC 7162) — while IMAP4rev1 implies
// neither.
func TestBuildSupportsAdvertisedVsEffective(t *testing.T) {
	cases := []struct {
		name string
		raw  []string
		cap  string
		want CapVerdict
	}{
		{"IMAP4rev2 implies UIDPLUS even though UIDPLUS is not advertised",
			[]string{"IMAP4rev2"}, "UIDPLUS", CapVerdict{Advertised: false, Effective: true}},
		{"QRESYNC implies CONDSTORE",
			[]string{"QRESYNC"}, "CONDSTORE", CapVerdict{Advertised: false, Effective: true}},
		{"IMAP4rev1 does NOT imply UIDPLUS (only rev2 does)",
			[]string{"IMAP4rev1", "IDLE"}, "UIDPLUS", CapVerdict{Advertised: false, Effective: false}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := buildSupports(tc.raw)[tc.cap]
			if got != tc.want {
				t.Errorf("buildSupports(%v)[%q] = %+v, want %+v", tc.raw, tc.cap, got, tc.want)
			}
		})
	}
}

func TestListMessages(t *testing.T) {
	fake := &fakeReader{
		mailboxes: []Mailbox{
			{Name: "INBOX"},
			{Name: "All Mail", Attributes: []string{`\all`}},
			{Name: "[Gmail]", Attributes: []string{`\noselect`}},
			{Name: "Archive", Attributes: []string{`\archive`}},
		},
		stubs: map[string]func(fn func(Meta) error) (Examined, error){
			"INBOX": func(fn func(Meta) error) (Examined, error) {
				for _, m := range []Meta{{UID: 3, Size: 100}, {UID: 9, Size: 250}} {
					if err := fn(m); err != nil {
						return Examined{}, err
					}
				}
				return Examined{Messages: 2, UIDValidity: 7}, nil
			},
			"Archive": func(fn func(Meta) error) (Examined, error) {
				return Examined{Messages: 1, UIDValidity: 8}, fn(Meta{UID: 1, Size: 42})
			},
		},
	}

	var got []MessageRow
	if err := ListMessages(context.Background(), fake, func(r MessageRow) error {
		got = append(got, r)
		return nil
	}); err != nil {
		t.Fatalf("ListMessages() error = %v", err)
	}
	want := []MessageRow{
		{Mailbox: "INBOX", UIDValidity: 7, UID: 3, Size: 100},
		{Mailbox: "INBOX", UIDValidity: 7, UID: 9, Size: 250},
		{Mailbox: "Archive", UIDValidity: 8, UID: 1, Size: 42},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ListMessages() rows = %+v, want %+v", got, want)
	}
	// Virtual and non-selectable mailboxes are the population Totals excludes;
	// examining them would double-count (\All) or fail (\Noselect).
	for _, name := range []string{"All Mail", "[Gmail]"} {
		if fake.called[name] {
			t.Errorf("ListMessages examined %q, which is excluded from the counted population", name)
		}
	}
}

func TestListMessagesFailingMailboxEmitsNothingAndReturnsItsError(t *testing.T) {
	fake := &fakeReader{
		mailboxes: []Mailbox{{Name: "INBOX"}, {Name: "Archive"}},
		stubs: map[string]func(fn func(Meta) error) (Examined, error){
			"INBOX": func(fn func(Meta) error) (Examined, error) {
				return Examined{UIDValidity: 7}, fn(Meta{UID: 1, Size: 10})
			},
			// Streams a row, THEN fails: that half-mailbox must not be emitted,
			// or a join against it would report the missing half as vanished.
			"Archive": func(fn func(Meta) error) (Examined, error) {
				if err := fn(Meta{UID: 5, Size: 20}); err != nil {
					return Examined{}, err
				}
				return Examined{}, fmt.Errorf("connection reset")
			},
		},
	}

	var got []MessageRow
	err := ListMessages(context.Background(), fake, func(r MessageRow) error {
		got = append(got, r)
		return nil
	})
	if err == nil || !strings.Contains(err.Error(), `"Archive"`) {
		t.Fatalf("ListMessages() error = %v, want an error naming Archive", err)
	}
	for _, r := range got {
		if r.Mailbox == "Archive" {
			t.Errorf("ListMessages emitted %+v from a mailbox whose Examine failed", r)
		}
	}
}
