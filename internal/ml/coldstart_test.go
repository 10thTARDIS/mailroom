// SPDX-License-Identifier: AGPL-3.0-only

package ml

import (
	"sort"
	"strings"
	"testing"
)

// The cold-start heuristic must RANK mail sensibly out of the box (spec §8): a
// known sender writing directly to you outranks a plain list message, which
// outranks auth-failing bulk; an unknown-but-direct message lands in between. We
// assert the ORDERING and the side of ImportantThreshold, never exact values —
// the weights are hand-tuned and free to move as long as the ranking holds.
func TestColdStartRanking(t *testing.T) {
	m := newColdStart()
	const threshold = 0.6 // internal/api ImportantThreshold (not imported: api is out of bounds)

	// Built through the real featuresFromFacts so the test scores exactly what the
	// serve path would produce for these messages.
	knownDirect := m.Score(featuresFromFacts(MessageFacts{
		HasCorresponded: true, OutboundCount: 8, ToCcCount: 1,
	}))
	unknownDirect := m.Score(featuresFromFacts(MessageFacts{
		ToCcCount: 1,
	}))
	plainList := m.Score(featuresFromFacts(MessageFacts{
		IsListMail: true, HasListUnsubscribe: true, ToCcCount: 50,
	}))
	authFailBulk := m.Score(featuresFromFacts(MessageFacts{
		IsListMail: true, HasListUnsubscribe: true, ToCcCount: 50, AuthFail: true,
	}))

	// Every score is a valid probability.
	for name, s := range map[string]float64{
		"knownDirect": knownDirect, "unknownDirect": unknownDirect,
		"plainList": plainList, "authFailBulk": authFailBulk,
	} {
		if s < 0 || s > 1 {
			t.Errorf("%s score %v out of [0,1]", name, s)
		}
	}

	// Known + direct is clearly important; plain list and auth-failing bulk are not.
	if knownDirect <= threshold {
		t.Errorf("known+direct score %v, want > %v (should be important from day one)", knownDirect, threshold)
	}
	if plainList >= threshold {
		t.Errorf("plain list score %v, want < %v (list mail should not be important by default)", plainList, threshold)
	}
	if authFailBulk >= threshold {
		t.Errorf("auth-failing bulk score %v, want < %v", authFailBulk, threshold)
	}

	// The ordering the spec's cold-start line demands.
	if !(authFailBulk < plainList && plainList < unknownDirect && unknownDirect < knownDirect) {
		t.Errorf("ranking violated: authFailBulk=%v plainList=%v unknownDirect=%v knownDirect=%v",
			authFailBulk, plainList, unknownDirect, knownDirect)
	}

	// "auth-failing bulk sinks": adding AuthFail to an otherwise-identical list
	// message must strictly lower the score.
	if authFailBulk >= plainList {
		t.Errorf("AuthFail did not sink the score: with=%v without=%v", authFailBulk, plainList)
	}
}

// THE MESSAGE THAT BROKE IT (mailroom#157). A marketing mail is addressed to you
// ALONE, carries List-Unsubscribe but usually NO List-Id, and — because it mails
// you every day — was the best-known "contact" in the address book. Under the old
// weights that combination scored 0.964 and put a delivery-preview sender at the top
// of the priority inbox, with nearly all mail above the line.
//
// Two things have to be true for that not to recur, and neither is about tuning:
// mail from someone you have never written to must not collect a relationship
// bonus however often they write to you, and List-Unsubscribe must disqualify the
// direct-address bonus rather than merely offset it.
func TestColdStartDoesNotPromoteFrequentBulkSenders(t *testing.T) {
	m := newColdStart()
	const threshold = 0.6

	// A daily newsletter: to you alone, unsubscribe header, no List-Id, and you
	// have never once written back.
	newsletter := m.Score(featuresFromFacts(MessageFacts{
		SenderAddress:      "preview@email.delivery.example",
		HasListUnsubscribe: true,
		ToCcCount:          1,
	}))
	if newsletter >= threshold {
		t.Errorf("a daily newsletter scored %v (>= %v): frequent bulk senders must not be important", newsletter, threshold)
	}

	// The same shape from someone you DO write to is important — the signal is
	// correspondence, not the absence of an unsubscribe link.
	fromAFriend := m.Score(featuresFromFacts(MessageFacts{
		SenderAddress:   "friend@example.com",
		HasCorresponded: true, OutboundCount: 4,
		ToCcCount: 1,
	}))
	if fromAFriend <= threshold {
		t.Errorf("mail from someone you correspond with scored %v (<= %v)", fromAFriend, threshold)
	}

	// And a stranger writing to you personally still clears the line: never having
	// heard from someone is not a reason to bury them.
	stranger := m.Score(featuresFromFacts(MessageFacts{
		SenderAddress: "new.person@example.com",
		ToCcCount:     1,
	}))
	if stranger <= threshold {
		t.Errorf("a stranger writing to you directly scored %v (<= %v): first contact must not be buried", stranger, threshold)
	}
	if stranger >= fromAFriend {
		t.Errorf("a stranger (%v) outranked someone you correspond with (%v)", stranger, fromAFriend)
	}
}

// A reply within a thread you are part of, and a server-flagged message, both
// rank up relative to a bare unknown message.
func TestColdStartRelationshipSignals(t *testing.T) {
	m := newColdStart()
	bare := m.Score(featuresFromFacts(MessageFacts{ToCcCount: 3}))
	reply := m.Score(featuresFromFacts(MessageFacts{ToCcCount: 3, IsReplyInThread: true}))
	flagged := m.Score(featuresFromFacts(MessageFacts{ToCcCount: 3, RemoteFlagged: true}))
	if reply <= bare {
		t.Errorf("reply-in-thread %v did not outrank bare %v", reply, bare)
	}
	if flagged <= bare {
		t.Errorf("server-flagged %v did not outrank bare %v", flagged, bare)
	}
}

// Score is deterministic and read-only: repeated calls agree, Update is a no-op
// (the heuristic is fixed), and scoring never mutates observable behavior.
func TestColdStartDeterministicAndReadOnly(t *testing.T) {
	m := newColdStart()
	fv := featuresFromFacts(MessageFacts{HasCorresponded: true, OutboundCount: 3, ToCcCount: 1})
	first := m.Score(fv)
	if second := m.Score(fv); second != first {
		t.Errorf("Score not deterministic: %v then %v", first, second)
	}
	// Update must not change what the model scores.
	m.Update(fv, 1.0)
	m.Update(fv, 0.0)
	if after := m.Score(fv); after != first {
		t.Errorf("Update changed the fixed heuristic: %v -> %v", first, after)
	}
	if m.Kind() != KindPriority {
		t.Errorf("Kind() = %q, want %q", m.Kind(), KindPriority)
	}
}

// MarshalBinary/UnmarshalBinary is a stateless round-trip, and UnmarshalModel
// dispatches on the 0x03 type tag — so nothing panics if the framing is ever
// exercised even though the registry never snapshots this model.
func TestColdStartMarshalRoundTrip(t *testing.T) {
	m := newColdStart()
	data, err := m.MarshalBinary()
	if err != nil {
		t.Fatalf("MarshalBinary: %v", err)
	}
	if len(data) != 3 || data[0] != modelMagic || data[1] != typeTagColdStart || data[2] != coldStartFormatV1 {
		t.Fatalf("unexpected framing % x", data)
	}
	got, err := UnmarshalModel(KindPriority, data)
	if err != nil {
		t.Fatalf("UnmarshalModel: %v", err)
	}
	if got.Kind() != KindPriority {
		t.Errorf("Kind after round-trip = %q, want %q", got.Kind(), KindPriority)
	}
	fv := featuresFromFacts(MessageFacts{IsListMail: true})
	if got.Score(fv) != m.Score(fv) {
		t.Errorf("Score after round-trip = %v, want %v", got.Score(fv), m.Score(fv))
	}

	// Corrupt framing is rejected, not silently mis-parsed.
	bad := []byte{modelMagic, typeTagColdStart, coldStartFormatV1 + 1}
	if err := newColdStart().UnmarshalBinary(bad); err == nil {
		t.Error("UnmarshalBinary accepted an unsupported format version")
	}
}

// featuresFromFacts emits authfail:1 exactly when AuthFail is set (and never
// otherwise) — the key the cold-start heuristic reads.
func TestFeaturesEmitAuthFail(t *testing.T) {
	with := featuresFromFacts(MessageFacts{AuthFail: true})
	if _, ok := with["authfail:1"]; !ok {
		t.Error("AuthFail=true did not emit authfail:1")
	}
	without := featuresFromFacts(MessageFacts{})
	if _, ok := without["authfail:1"]; ok {
		t.Error("AuthFail=false emitted authfail:1")
	}
}

// coldStartModel.Score sums over three package-level bucket maps, which is the
// shape #222 had to fix in logisticModel.Score. It is safe here ONLY because
// featuresFromFacts emits exactly one key per bucket family, so at most one term
// per loop is nonzero and the rest add an exact 0.0.
//
// That property lives in features.go, not in coldstart.go, so it is pinned here:
// if a second key from one family is ever emitted, coldStartModel.Score becomes
// order-dependent the way the logistic scorer was, and this goes red first.
func TestColdStartBucketFamiliesAreMutuallyExclusive(t *testing.T) {
	cases := []struct {
		name  string
		build func(*MessageFacts)
	}{
		{"bulk list mail", func(f *MessageFacts) {
			f.IsListMail, f.HasListUnsubscribe, f.ToCcCount, f.InboundCount = true, true, 40, 25
		}},
		{"direct correspondent", func(f *MessageFacts) {
			f.HasCorresponded, f.OutboundCount, f.ToCcCount, f.IsReplyInThread = true, 12, 1, true
		}},
		{"stranger, one-way", func(f *MessageFacts) { f.InboundCount, f.ToCcCount = 3, 0 }},
		{"zero everything", func(*MessageFacts) {}},
	}
	families := []string{"sent:", "tocc:", "oneway:"}

	for _, c := range cases {
		var facts MessageFacts
		facts.SenderAddress = "a@x.test"
		facts.SenderDomain = "x.test"
		c.build(&facts)
		fv := featuresFromFacts(facts)

		for _, fam := range families {
			var hits []string
			for k := range fv {
				if strings.HasPrefix(k, fam) {
					hits = append(hits, k)
				}
			}
			if len(hits) > 1 {
				sort.Strings(hits)
				t.Errorf("%s: family %q emitted %d keys (%v); coldStartModel.Score's map loops are only order-independent at one",
					c.name, fam, len(hits), hits)
			}
		}
	}
}
