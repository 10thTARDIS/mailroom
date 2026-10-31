// SPDX-License-Identifier: AGPL-3.0-only

package ml

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"unicode"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/kind"
	"mailroom/internal/store"
)

// This file is the feature layer for the T1 priority scorer (mailroom#33) — and
// its single most important property is that there is ONE hashing point,
// featuresFromFacts, feeding BOTH serve-time scoring and (future) replay-time
// training. A model is only as good as the guarantee that the features it was
// trained on are computed identically to the features it is scored on; the way
// this code makes that guarantee STRUCTURAL, not a matter of remembering, is:
//
//   - MessageFacts is the raw per-message signal, buildable from EITHER the
//     in-hand ingest data (serve path, internal/ingest) OR a stored row (replay
//     path, StoreExtractor below);
//   - featuresFromFacts(MessageFacts) is the ONLY place a fact becomes a hashed
//     feature key. Both paths call it. So the two can differ in where the FACTS
//     come from, but never in how a fact becomes a feature — there is no second
//     copy of the hashing scheme to drift.

// MessageFacts is the raw, un-hashed per-message signal the priority model
// reads. It is deliberately a plain data struct with no behavior: whoever builds
// it (ingest, in hand; StoreExtractor, from the row) is free to fill each field
// from whatever source it has, and featuresFromFacts turns the whole thing into
// the sparse feature vector.
//
// A field a given path cannot observe is left at its zero value. The replay path
// in particular cannot see the fields that are NOT projected onto app.messages
// (they live in the message blob): List-Id / List-Unsubscribe, the To+Cc count,
// and the Authentication-Results verdict (AuthFail). Those default to false/0
// there. That is a facts-SOURCE asymmetry, not a hashing skew — both paths still
// run the one featuresFromFacts.
type MessageFacts struct {
	// SenderAddress is the From address, normalized (lowercased/trimmed).
	SenderAddress string
	// SenderDomain is the From address's domain, lowercased.
	SenderDomain string
	// HasCorresponded is true when the USER has written to this address — the
	// signal that actually means a relationship (mailroom#157).
	//
	// It replaces IsKnownContact, which meant "is in the auto-derived address
	// book" and therefore only ever meant "has mailed you". On real mail that
	// made a daily newsletter the best-known correspondent in the mailbox and
	// handed it the largest UP weight in the model: nearly everything scored
	// important, with a transactional notification sender at the top. Being mailed at is not
	// a relationship; replying is.
	HasCorresponded bool
	// OutboundCount is how many times the user has written to this address,
	// bucketed into a feature; 0 when never. The counterpart to the old
	// ContactSeenCount, counting the direction that means something.
	OutboundCount int
	// InboundCount is how many times this address has written to the USER. On its
	// own it means nothing — that was the original bug — but combined with
	// OutboundCount == 0 it is evidence of BROADCAST: someone who has mailed you
	// twenty-six times and never once been answered is not a correspondent, they
	// are a mailing list that forgot its unsubscribe header (mailroom#157).
	InboundCount int
	// IsListMail is true when the message carries a List-Id header (bulk/list
	// mail). Projected onto the row by 00016, so BOTH paths see it (#157).
	IsListMail bool
	// HasListUnsubscribe is true when the message carries List-Unsubscribe — in
	// practice the most reliable bulk marker there is, since transactional mail
	// deliberately omits it (an unsubscribe must not stop a receipt).
	HasListUnsubscribe bool
	// Subject is the raw subject; featuresFromFacts tokenizes it.
	Subject string
	// ToCcCount is the number of To+Cc recipients. Projected by 00016 (#157).
	ToCcCount int
	// IsReplyInThread is true when the message has an In-Reply-To header (it is a
	// reply, not a thread root). Both paths derive it from In-Reply-To presence,
	// so it is skew-free.
	IsReplyInThread bool
	// HourOfDayUTC is the received hour (0..23) in UTC.
	HourOfDayUTC int
	// HasAttachments mirrors app.messages.has_attachments.
	HasAttachments bool
	// AddressedAlias is the alias the message was delivered to (spec §5), the
	// same value ingest derives and stores as addressed_address. Normalized.
	AddressedAlias string
	// RemoteFlagged is true when the server already marked the message important
	// or junk (\Flagged / junk) — a strong prior the model should not ignore.
	RemoteFlagged bool
	// AuthFail is true when the message's authentication CLEARLY failed
	// (Authentication-Results: DMARC=fail, or SPF=fail AND DKIM=fail — see
	// AuthResultsFail). It is the "auth-failing bulk sinks" signal (spec §8).
	// The VERDICT is projected onto the row by 00016 (#157), so both paths see
	// it without re-implementing the parser in SQL.
	AuthFail bool
}

// featuresFromFacts is THE single hashing point (see the file header): it turns
// a MessageFacts into a SPARSE FeatureVector of presence-1.0 keys. It is pure
// and deterministic — the same facts always yield the same keys — which is what
// lets the eval harness replay a model and get the exact score serve-time
// produced.
//
// Emitted keys:
//
//	from:<addr>     dom:<domain>     corr:1      sent:<bucket>
//	list:1          unsub:1          subtok:<tok> (per distinct subject token)
//	tocc:<bucket>   reply:1          hour:<0-23>  attach:1
//	alias:<addr>    flagged:1        authfail:1
//
// sent:, tocc: and hour: are CATEGORICAL and always emitted (every message has a
// value); the rest are BOOLEAN and emitted only when present. Everything is
// lowercased here too, so a caller that forgets to normalize cannot split one
// real feature across two keys.
// onewayMin is how much unanswered mail from one address counts as broadcast
// rather than as someone who simply has not been replied to yet. Six is
// deliberately forgiving: a colleague you owe five replies is not a newsletter.
const onewayMin = 6

// isNoReplyAddress reports whether an address advertises that it does not accept
// replies — no-reply@, noreply@, donotreply@ and the usual spellings. It is a
// property of the LOCAL PART only, so a person at a domain that also runs a
// no-reply sender is unaffected.
func isNoReplyAddress(addr string) bool {
	local, _, ok := strings.Cut(strings.ToLower(strings.TrimSpace(addr)), "@")
	if !ok {
		return false
	}
	// Strip separators so no-reply / no_reply / noreply / no.reply all collapse
	// to one spelling before matching.
	stripped := strings.NewReplacer("-", "", "_", "", ".", "").Replace(local)
	return strings.HasPrefix(stripped, "noreply") ||
		strings.HasPrefix(stripped, "donotreply") ||
		strings.HasPrefix(stripped, "dontreply")
}

func featuresFromFacts(f MessageFacts) FeatureVector {
	fv := FeatureVector{}
	if a := strings.ToLower(strings.TrimSpace(f.SenderAddress)); a != "" {
		fv["from:"+a] = 1.0
	}
	if d := strings.ToLower(strings.TrimSpace(f.SenderDomain)); d != "" {
		fv["dom:"+d] = 1.0
	}
	// corr:/sent: replace the old known:/seen: pair, which counted mail RECEIVED
	// and so ranked bulk senders as the closest correspondents (#157). The keys
	// are NEW names rather than reused ones on purpose: a model trained on the
	// old meaning must not silently inherit those weights under a new one.
	if f.HasCorresponded {
		fv["corr:1"] = 1.0
	}
	fv["sent:"+countBucket(f.OutboundCount)] = 1.0
	// ONE-WAY: they write, you never do. Emitted only above a threshold of
	// received mail, so a stranger's first message is never penalised — it is
	// sustained unanswered volume that means broadcast, not a single arrival.
	if f.OutboundCount == 0 && f.InboundCount >= onewayMin {
		fv["oneway:"+countBucket(f.InboundCount)] = 1.0
	}
	// Nobody writes to you personally from an address that refuses replies.
	if isNoReplyAddress(f.SenderAddress) {
		fv["noreply:1"] = 1.0
	}
	if f.IsListMail {
		fv["list:1"] = 1.0
	}
	if f.HasListUnsubscribe {
		fv["unsub:1"] = 1.0
	}
	for _, tok := range subjectTokens(f.Subject) {
		fv["subtok:"+tok] = 1.0
	}
	// CURATED SUBJECT VOCABULARY, alongside the raw tokens rather than instead of
	// them (#169).
	//
	// The raw tokens are the model's only subject signal, and they are partly
	// sender identity in disguise: `subtok:verge`, `subtok:patreon`. Hold a
	// sender out — which is the only honest way to ask whether a label model has
	// generalised — and its vocabulary goes with it. Grouped 5-fold CV over a set of
	// hand-labelled messages had every label but one swinging 0.5–0.7 AUC on
	// which senders happened to be held out.
	//
	// These classes are the abstraction the tokens lack: "order-confirmation
	// language", "campaign language", whoever wrote it. They come from
	// internal/kind's curated lexicons, so there is ONE definition of what
	// promotional language means rather than two that can drift apart.
	//
	// Only the SUBJECT rules are used, because the subject is a stored column and
	// therefore identical on the ingest and replay paths. kind's other signals
	// live in the message bytes, and a feature the training path cannot see is
	// worse than no feature — that is the train/serve gap #157 existed to close.
	for _, c := range kind.SubjectClasses(f.Subject) {
		fv["subjclass:"+string(c)] = 1.0
	}
	fv["tocc:"+countBucket(f.ToCcCount)] = 1.0
	if f.IsReplyInThread {
		fv["reply:1"] = 1.0
	}
	fv["hour:"+strconv.Itoa(clampHour(f.HourOfDayUTC))] = 1.0
	if f.HasAttachments {
		fv["attach:1"] = 1.0
	}
	if a := strings.ToLower(strings.TrimSpace(f.AddressedAlias)); a != "" {
		fv["alias:"+a] = 1.0
	}
	if f.RemoteFlagged {
		fv["flagged:1"] = 1.0
	}
	if f.AuthFail {
		fv["authfail:1"] = 1.0
	}
	return fv
}

// countBucket maps a count to a coarse, stable bucket label. Bucketing (rather
// than a raw count feature) keeps the feature space categorical and bounded, so
// "seen twice" and "seen three times" share a bucket instead of each learning
// its own weight from one or two examples.
func countBucket(n int) string {
	switch {
	case n <= 0:
		return "0"
	case n == 1:
		return "1"
	case n == 2:
		return "2"
	case n <= 5:
		return "3-5"
	case n <= 10:
		return "6-10"
	case n <= 25:
		return "11-25"
	default:
		return "26+"
	}
}

// clampHour bounds an hour into 0..23 so a malformed timestamp cannot mint an
// out-of-range feature key.
func clampHour(h int) int {
	if h < 0 {
		return 0
	}
	if h > 23 {
		return 23
	}
	return h
}

// subjectTokens lowercases the subject, splits it on any non-alphanumeric run,
// and returns the DISTINCT tokens in first-seen order. It is deliberately simple
// and deterministic (no stemming, no stop-word list): the model learns which
// tokens matter from the weights, and a fancier tokenizer would be one more
// thing that could differ between train and serve.
func subjectTokens(subject string) []string {
	fields := strings.FieldsFunc(strings.ToLower(subject), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsNumber(r)
	})
	seen := make(map[string]bool, len(fields))
	out := make([]string, 0, len(fields))
	for _, tok := range fields {
		if tok == "" || seen[tok] {
			continue
		}
		seen[tok] = true
		out = append(out, tok)
	}
	return out
}

// StoreExtractor implements FeatureExtractor (eval.go) over the message store:
// given a message id it re-reads the canonical row plus the sender's contact
// stats, rebuilds MessageFacts, and runs featuresFromFacts. It is the REPLAY /
// TRAINING path — used by the eval harness now and PR2's trainer later. The
// SERVE path does NOT use it (ingest already has the facts in hand); this exists
// precisely so replay computes features through the same one function.
//
// It is constructed PER USER because #35's FeatureExtractor.Extract(ctx, msgID)
// carries no user id, but every store read needs one for RLS (fail closed).
// Binding the user id at construction keeps the #35 interface unchanged while
// still routing every read through a correctly-scoped UserTx.
type StoreExtractor struct {
	st     *store.Store
	userID ulid.ULID
}

var _ FeatureExtractor = (*StoreExtractor)(nil)

// NewStoreExtractor builds an extractor scoped to one user.
func NewStoreExtractor(st *store.Store, userID ulid.ULID) *StoreExtractor {
	return &StoreExtractor{st: st, userID: userID}
}

// Extract re-reads the message row (+ the sender's contact stats) under an
// RLS-scoped UserTx, builds MessageFacts, and hashes them. A message the user
// does not own or that no longer exists is an error; the eval harness treats any
// extractor error as "skip this sample", so a purged message never fails a run.
func (e *StoreExtractor) Extract(ctx context.Context, msgID ulid.ULID) (FeatureVector, error) {
	var facts MessageFacts
	err := e.st.UserTx(ctx, e.userID, func(tx *store.UserTx) error {
		row, found, err := tx.MessageMLFacts(ctx, msgID)
		if err != nil {
			return err
		}
		if !found {
			return fmt.Errorf("ml: extract: message %s not found for user", msgID)
		}
		// Correspondence and one-way volume, not "is in the address book" (#157).
		outbound, inbound, err := tx.OutboundCountForAddress(ctx, row.FromAddress)
		if err != nil {
			return err
		}
		facts = factsFromRow(row, outbound, inbound)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return featuresFromFacts(facts), nil
}

// factsFromRow maps the stored message signals (+ contact stats) into
// MessageFacts.
//
// The bulk signals — List-Id, List-Unsubscribe, the recipient count and the
// Authentication-Results verdict — now come off the ROW (migration 00016,
// mailroom#157) rather than being left zero-valued. That closes a train/serve
// gap that was worse than a missing feature: the serve path saw a message as
// bulk while this path saw the same message as ordinary, so the model could
// never learn that bulk mail is unimportant, and the cold-start heuristic's
// relationship weights ran unopposed. Mail ingested before 00016 has no
// projected signals until `mailroom-admin rescore` re-derives them from the
// stored bytes; until then it reads as not-bulk, exactly as it did before.
func factsFromRow(row store.MessageMLFacts, outboundCount, inboundCount int) MessageFacts {
	addr := store.NormalizeAddress(row.FromAddress)
	return MessageFacts{
		SenderAddress:      addr,
		SenderDomain:       store.AddressDomain(addr),
		HasCorresponded:    outboundCount > 0,
		OutboundCount:      outboundCount,
		InboundCount:       inboundCount,
		Subject:            row.Subject,
		IsReplyInThread:    strings.TrimSpace(row.InReplyTo) != "",
		HourOfDayUTC:       row.ReceivedAt.UTC().Hour(),
		HasAttachments:     row.HasAttachments,
		AddressedAlias:     store.NormalizeAddress(row.AddressedAddress),
		RemoteFlagged:      row.Flagged || row.Junk,
		IsListMail:         strings.TrimSpace(row.ListID) != "",
		HasListUnsubscribe: strings.TrimSpace(row.ListUnsubscribe) != "",
		ToCcCount:          row.RecipientCount,
		AuthFail:           row.AuthFail,
	}
}
