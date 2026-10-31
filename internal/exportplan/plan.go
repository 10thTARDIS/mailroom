// SPDX-License-Identifier: AGPL-3.0-only

// Package exportplan is the decision, written down, about what a per-user export
// carries and what it leaves behind (#54, spec §10).
//
// # Why this is a package and not a switch inside the exporter
//
// Spec §10 defines the export as "mbox or maildir + JSON sidecars for
// labels/scores/contacts". That sentence names four things and the schema has
// forty-three tables. The gap between those two numbers is the whole problem:
// an export that quietly carries four tables' worth of a user's data, while
// reporting success, is the portability promise failing in exactly the way this
// project's discipline is written to catch — it ran, it produced output, and
// what it produced was incomplete in a way no error surfaced.
//
// So every user-scoped table gets an explicit disposition here, with a reason,
// and the report prints it. Deciding is the point; the exporter merely obeys.
//
// # The disposition a new table gets is UNHANDLED, and that is deliberate
//
// A table absent from this map is not silently dropped. Unhandled is its own
// loud state, because the failure this guards against is not a wrong decision —
// it is a decision nobody made. #316 is what an unmaintained list costs: one
// producer forgot one INSERT for the life of the project. The table set is
// derived from pg_catalog at run time (store.UserRowCensus) and checked against
// this map, so the 44th table announces itself the day it is created.
//
// # WITHHELD is separated from DROPPED on purpose
//
// "We chose not to carry your rules" and "we will not hand you a file containing
// your IMAP password" are different statements, and collapsing them into one
// "not exported" bucket would hide the second. An export is a file that leaves
// the machine; anything in it should be there because someone decided it should
// be, and credentials are the category where a mistake is not recoverable by
// re-running the export.
package exportplan

import "sort"

// Disposition is what an export does with one table.
type Disposition string

const (
	// Exported: the data leaves with the user, in the form named by Detail.
	Exported Disposition = "exported"
	// Dropped: deliberately not carried, for the reason given. The user loses
	// it on migration and that is an accepted, stated cost.
	Dropped Disposition = "dropped"
	// Withheld: MUST NOT appear in an export because it is credential material
	// or another user's business. Distinct from Dropped so that "we decided not
	// to" can never be confused with "it would be unsafe to".
	Withheld Disposition = "withheld"
)

// Entry is one table's decision.
type Entry struct {
	Disposition Disposition
	// Detail names the output for Exported entries (the file the data lands in),
	// and is empty otherwise.
	Detail string
	// Reason is why. Required for Dropped and Withheld — a disposition without a
	// reason is a decision nobody has to defend.
	Reason string
}

// plan is the decision for every user-scoped table in schema app.
//
// ⚠ ADDING A TABLE TO THE SCHEMA AND NOT TO THIS MAP IS A REPORTED FAILURE, not
// a silent drop. See Unhandled.
var plan = map[string]Entry{
	// ---- the account record ----------------------------------------------
	//
	// ⚠ THIS ENTRY EXISTS BECAUSE THE COVERAGE TEST DEMANDED IT. app.users is
	// the delete's root and carries no user_id column, so an earlier version of
	// this map simply did not mention it — and the export would have omitted
	// the user's own account record while reporting success. Exactly the class
	// of omission the catalog check is for.
	"users": {Exported, "account.json (email address and display name)",
		"the OIDC subject is NOT carried: it is an identifier minted by THIS " +
			"deployment's identity provider, it names the user to that provider, and it " +
			"is meaningless — and correlatable — anywhere else"},

	// ---- the mail itself -------------------------------------------------
	"messages": {Exported, "maildir/ (one file per message, raw RFC 5322 bytes)",
		""},
	"blobs": {Exported, "maildir/ — the blob bytes ARE the message files",
		""},
	// ⚠ THE SENTENCE THAT USED TO STAND HERE WAS HALF TRUE, AND THE FALSE HALF
	// COST SOMETHING. "attachments are MIME parts inside the raw bytes" is true
	// of the CONTENT and false of everything app.attachments exists to record:
	// the scan verdict and the user's release of a quarantine (#396, ADR-027
	// §11). Read as a complete disposition it said "nothing to carry", so an
	// export shipped a flagged file's bytes with no verdict — resurrecting the
	// silently-waiting attachment on the next instance, which is precisely what
	// §11 predicted and what this entry made easy to miss.
	// ⚠ EXPORTED AS A FLAG ON THE PART, NOT AS ITS OWN FILE. A release is keyed
	// on a content digest, and a digest is a pointer into THIS deployment's
	// bytes — carrying the table verbatim would produce a file of hex nobody on
	// the importing side could join to anything. Denormalising it onto each
	// part in messages.jsonl carries the same fact where it can be used: "the
	// user accepted these bytes." It is the one field in that record a re-scan
	// on the importing side could not reproduce, because it is a decision
	// rather than a measurement.
	"attachment_releases": {Exported, "messages.jsonl (as `released` on each attachment record)",
		"not carried as a table: the digest it keys on names bytes in this deployment, " +
			"so the decision travels attached to the part it is about"},
	"attachments": {Exported, "maildir/ for the bytes; messages.jsonl for the scan verdict and release",
		"the verdict is NOT reconstructible from the bytes — it is what a scanner " +
			"said at a moment, and the release is a decision a human made"},

	// ---- sidecars named by spec §10 --------------------------------------
	"labels": {Exported, "labels.json", ""},
	"message_labels": {Exported, "messages.jsonl (per message, with applied_by and the rule's NAME)",
		"the rule is carried by name, not by applied_by_rule_id: a ULID is a pointer " +
			"into a table this export does not contain"},
	// ⚠ NOT a separate scores.json, though spec §10's sentence names one. A file
	// keyed by message id, sitting beside a file keyed by message id, is two
	// places for the same join to go wrong — and the score is per-message state
	// exactly like the labels next to it.
	"triage_scores": {Exported, "messages.jsonl (per message, with its model version)",
		"folded into the per-message sidecar rather than given its own file: a second " +
			"file keyed the same way is a join waiting to disagree"},
	"contacts": {Exported, "contacts.json", ""},
	"contact_addresses": {Exported, "contacts.json (nested under their contact)",
		""},
	"contact_keys": {Exported, "contacts.json (public keys only)", ""},
	// Encryption pins (#47, migration 00068): which key was last encrypted to for
	// an address. Re-derivable trust state on this instance, not the user's data.
	"contact_key_pins": {Dropped, "",
		"the fingerprint last encrypted to for each address, a trust-on-first-use record " +
			"for this instance; it is rebuilt by the next encrypted send and carries nothing " +
			"the exported keys do not"},
	// Key history (#815, migration 00071): when a correspondent's key was first
	// seen, renewed, pinned, refused or removed on this instance.
	"contact_key_events": {Dropped, "",
		"this instance's record of when it saw, renewed, trusted or removed a correspondent's " +
			"key; it describes this server's sightings, not the user's data, and the exported " +
			"keys carry the keys themselves"},

	// Merge records (#834, migration 00073): what each Contact/merge borrowed,
	// so Contact/separate can give it back.
	"contact_merges": {Dropped, "",
		"this instance's bookkeeping for undoing a merge: the exported contacts carry their " +
			"current state and addresses, and an import starts with no merges to undo"},

	// ---- user-authored configuration, beyond §10's four nouns ------------
	//
	// §10 names labels, scores and contacts. These are none of those, and they
	// are also things the user WROTE — losing them is losing work, not losing
	// derived state. Carried deliberately, and listed here so the widening is
	// visible rather than assumed.
	"identities":        {Exported, "identities.json", ""},
	"rules":             {Exported, "rules.json", ""},
	"compose_snippets":  {Exported, "snippets.json (signatures and templates)", ""},
	"saved_searches":    {Exported, "saved_searches.json", ""},
	"category_tabs":     {Exported, "tabs.json", ""},
	"label_keyword_map": {Exported, "labels.json (nested)", ""},
	"remote_allows":     {Exported, "remote_allows.json", ""},
	// The desk's areas, feeds, and its two settings surfaces (#442 increment 2)
	// are all USER-AUTHORED configuration by the same reasoning as
	// tabs.json/saved_searches.json above: losing them on migration is losing
	// work, not losing derived state.
	//
	// areas.json carries NAME, tabs and label NAMES rather than ids — like
	// rules.json's action.label (config_sidecar.go), an area's own label and
	// its additional label feeds are resolved to the names they mean, since a
	// ULID would be a dangling pointer into a database the importing side
	// does not have.
	"areas": {Exported, "areas.json", ""},
	"area_feeds": {Exported, "areas.json (nested feeds under their area)",
		"part of the Area object above; same file, same reasoning"},
	"desk_preferences":   {Exported, "desk_preferences.json", ""},
	"client_preferences": {Exported, "client_preferences.json", ""},
	// Out of office (#50, migration 00065): the user's own reply text and
	// window, per account by name. The server-side state does not travel.
	"out_of_office": {Exported, "out_of_office.json", ""},
	// The drawer auto-archive job's audit table (#443, migration 00048):
	// folded into the per-message sidecar as `drawer_archived_at`, the same
	// reasoning triage_scores above gives — a per-message fact keyed by
	// message id belongs beside the other per-message facts, not in a second
	// file that could disagree about which message it describes. The area
	// NAME already travels on the row itself (app.drawer_archives.area_name
	// survives the area's own deletion); an area id would be a dangling
	// pointer into a database the importing side does not have.
	"drawer_archives": {Exported, "messages.jsonl (as drawer_archived_at on each message)",
		"folded into the per-message sidecar rather than a separate table: a per-message " +
			"fact keyed the same way as labels and scores is a join waiting to disagree"},

	// Day-one setup (#446, migration 00050). Both DROPPED, and the reason is the
	// same one message_provenance gets: they describe THIS deployment's
	// relationship with a provider's folder layout, in this deployment's ids.
	//
	// A setup run is a one-shot operator decision about a mailbox that has
	// already been made and already applied — its lasting effect is the LABELS it
	// wrote and the desk_archived_at marks it set, both of which travel (labels in
	// labels.json, the mark below). Carrying the run itself would export a plan
	// naming mailbox ids that mean nothing on the importing side, and the run's
	// counters are a record of an event, not of the user's mail.
	//
	// The JOURNAL is the sharper case and is dropped for a reason worth stating:
	// it exists ONLY to make an undo possible on THIS instance. Exporting it would
	// carry an undo journal for an operation nothing on the other side can
	// reverse — a promise the archive cannot keep.
	"setup_runs": {Dropped, "",
		"a one-shot operator decision about this deployment's folder layout, already " +
			"applied; its lasting effect is the labels it wrote (labels.json) and the " +
			"desk_archived_at mark (messages.jsonl), both of which are exported"},
	"setup_journal": {Dropped, "",
		"the per-message undo journal for a run on THIS instance only; an importing " +
			"side has nothing to reverse, so carrying it would be a promise the archive " +
			"cannot keep"},

	// Learned placement (#714, migration 00060). Both DROPPED. The placement
	// log is a MEASUREMENT of this instance's decisions (which message it
	// filed, under which key, and whether the user kept it), not something
	// the user made; its lasting effect, the area label, travels with the
	// message's labels, and the EVIDENCE it learns from is the user's own
	// filings, which travel too, so an importing side re-learns the same
	// memory from them. The gate resets are operator actions on this
	// instance's measurement and mean nothing without it.
	"learned_placements": {Dropped, "",
		"a measurement of this instance's placements and their outcomes; the labels it " +
			"wrote travel with each message, and the user filings it learns from are exported"},
	"learned_placement_gate_resets": {Dropped, "",
		"operator re-enables of this instance's correction gate, meaningless without its log"},

	// Calendar invitations (#725, migration 00063). Both DROPPED, because both
	// are re-derivable from mail that IS exported. The index is a projection of
	// the invitation messages' own bytes (UID, SEQUENCE, METHOD, ORGANIZER),
	// rebuilt by the importing side's ingest. The answers are what the user
	// SENT, and each one left as an iTIP REPLY message that travels in the
	// mail export with its text/calendar part intact; the row is this
	// instance's pointer from that send to the card.
	"calendar_invites": {Dropped, "",
		"an index of invitation messages by event UID, re-derived from their exported bytes on import"},
	"calendar_responses": {Dropped, "",
		"the user's answers; each one was sent as an iTIP REPLY message, which is exported with the mail"},

	// Follow-up nudges (#58, migration 00064). DROPPED: a dismissal hides a
	// nudge that expires on its own 30 days after the message (nudge.MaxAge),
	// so on any import later than that it would hide nothing; the settings
	// that DO matter travel in desk_preferences.json.
	"nudge_dismissals": {Dropped, "",
		"\"Done\" on a follow-up nudge, which expires by itself 30 days after its message; " +
			"the follow-up settings travel in desk_preferences.json"},

	// Local DKIM verdicts (#776, migration 00066). DROPPED: a verdict about
	// signatures checked at THIS instance's ingest, against DNS keys of that
	// moment; the exported bytes still carry the signatures themselves.
	"message_local_auth": {Dropped, "",
		"the result of this instance's own DKIM check at ingest, made against the DNS keys of " +
			"that moment and unreproducible later (x= expiry, rotated keys); the signed bytes travel with the mail"},

	// ---- derivable from what is exported ---------------------------------
	"threads": {Dropped, "",
		"rebuilt on import from References/In-Reply-To in the raw bytes; exporting " +
			"thread ids would carry THIS instance's ULIDs, which mean nothing elsewhere"},
	"message_provenance": {Dropped, "",
		"IMAP coordinates (account, mailbox, UID) of this instance's connection to a " +
			"provider; meaningless in another deployment, and re-derived by the first sync"},
	"mailboxes": {Dropped, "",
		"the provider's folder layout, re-discovered on first sync. The user-meaningful " +
			"half is labels, which IS exported"},

	// ---- operational bookkeeping -----------------------------------------
	"change_seq":   {Dropped, "", "sync cursor bookkeeping; per-instance by construction"},
	"change_log":   {Dropped, "", "the JMAP change feed; a log of this instance's mutations, not user data"},
	"change_floor": {Dropped, "", "change-feed compaction watermark"},
	"jobs":         {Dropped, "", "the work queue; in-flight operations of this instance"},
	"sync_jobs":    {Dropped, "", "per-account sync scheduling state"},
	"writeback_intents": {Dropped, "",
		"pending IMAP writes for this instance's accounts"},
	"account_sync_status": {Dropped, "", "per-account sync health and cursors"},
	"superseded_content":  {Dropped, "", "draft-replacement bookkeeping (#268); internal"},
	"proxy_image_cache":   {Dropped, "", "a cache of remote images; refetchable, and not the user's data"},
	"proxy_image_refs":    {Dropped, "", "which messages keep a cached remote image alive (#922); internal"},

	// ---- send history ----------------------------------------------------
	//
	// The sent MAIL is in `messages` and exports normally. These three are the
	// DELIVERY RECORD around it, and they were reviewed together and split
	// (decided 2026-08-28) rather than dropped as a block.
	//
	// ⚠ A HYPOTHESIS THAT DROVE THE FIRST VERSION OF THIS BLOCK, CHECKED AND
	// REFUTED — recorded so nobody re-raises it. The argument for keeping
	// `submissions` was that Bcc recipients live only in `envelope_to` and would
	// be lost. They are not: compose.Build deliberately KEEPS the Bcc header in
	// the STORED bytes and compose.SendPayload strips it only from the
	// transmitted copy, so an export (which carries stored bytes) carries the
	// blind recipients too.
	//
	// What survives that refutation is what `submissions` is exported for:
	//
	//  1. A FAILED OR CANCELLED SEND LEAVES NO OTHER TRACE. state='failed' means
	//     the message exports looking like an ordinary unsent draft, with
	//     nothing recording that a send was attempted and what the server said.
	//  2. THE ENVELOPE CAN DIVERGE FROM THE HEADERS. JMAP EmailSubmission/set
	//     accepts an explicit envelope.rcptTo (internal/api/submission.go), so a
	//     client can address recipients who appear in no header at all. When
	//     that happens this table is the only record of who was written to.
	//
	// ⚠ Exported as ADDRESSES AND OUTCOME, never as the stored row: account_id,
	// identity_id, message_id, job_id and idempotency_key are this instance's
	// ULIDs and its retry bookkeeping, meaningless in another deployment and —
	// for the idempotency key — a client's private token rather than the user's
	// data.
	"submissions": {Exported,
		"submissions.json (from/to addresses, state, SMTP response, sent time)",
		"the internal ids and the idempotency key are NOT carried: they are this " +
			"instance's bookkeeping, not the user's record of what was sent"},

	// The other two stay dropped, and for DIFFERENT reasons than each other.
	"outbound_ledger": {Dropped, "",
		"a per-account, per-day SENT COUNT and nothing else — rate-limit bookkeeping " +
			"that says nothing about any individual message, and is derivable from the " +
			"submissions that ARE exported"},
	"dsn_events": {Dropped, "",
		"parsed bounce notifications. The DSN is itself a message and exports normally; " +
			"RFC 3464 puts the original Message-ID in its body, so the link back to what " +
			"bounced is re-derivable from the exported mail rather than lost"},

	// ---- machine-learning state ------------------------------------------
	"ml_models": {Dropped, "",
		"per-user model artifacts; large, binary, and reproducible by retraining on the " +
			"exported mail. Carrying them would export a derived artifact in place of the " +
			"data it was derived from"},
	"label_suggestions":   {Dropped, "", "model/rule output awaiting a decision; regenerated after import"},
	"category_dismissals": {Dropped, "", "tombstones for suggestions the user dismissed"},
	"rule_corrections":    {Dropped, "", "training signal derived from the user's corrections"},
	"feedback_events":     {Dropped, "", "training signal derived from user actions"},

	// ---- withheld: credentials and admin record --------------------------
	"accounts": {Withheld, "",
		"holds the user's IMAP/SMTP credentials (encrypted at rest). An export is a file " +
			"that leaves the machine; provider passwords must not be in it. The account's " +
			"non-secret shape is re-entered when the user reconnects"},
	"api_keys": {Withheld, "",
		"hashed API credentials for this instance; useless elsewhere and not to be circulated"},
	"api_key_events": {Withheld, "",
		"what this instance's API keys did; the keys themselves are not carried, so their " +
			"audit has nothing to attach to elsewhere"},
	"api_key_drafts": {Withheld, "",
		"which drafts this instance's API keys wrote; the keys do not travel"},
	"api_key_suppressed_refusals": {Withheld, "",
		"counts of refused calls by this instance's API keys; the keys do not travel"},
	"api_key_pending": {Withheld, "",
		"requests from this instance's API keys waiting for approval; they expire within a day " +
			"and name keys that do not travel"},
	"ai_providers": {Withheld, "",
		"operator-configured provider endpoints and API keys"},
	"push_subscriptions": {Withheld, "",
		"browser push endpoints and their VAPID material, bound to this deployment"},
	"user_keys": {Withheld, "",
		"PGP keys. The PUBLIC halves are safe and are carried in contacts.json; the PRIVATE " +
			"halves are not carried by THIS export — `mailroom-admin pgp-export-wrapped` " +
			"(spec §9's `export-wrapped`, ADR-024) exports them separately, passphrase-wrapped, " +
			"as its own deliberate act. Without running that too, exported PGP-encrypted mail " +
			"is unreadable, which this report states rather than discovers later"},
	"audit_log": {Withheld, "",
		"the administrative record of actions taken on this instance; an accountability " +
			"trail, not the user's own data"},
	// The decrypt trail (#47, migration 00067). WITHHELD for audit_log's
	// reason: it records what the crypto process on THIS instance did, and it
	// is the accountability half of ADR-037's threat model, not mail.
	// Mail-provider OAuth (#52, migration 00078). The grant is credential
	// state: it names the provider account the refresh token (sealed in
	// accounts, itself withheld) belongs to, and is useless without it. A
	// pending flow carries a sealed PKCE verifier and lives ten minutes.
	"oauth_grants": {Withheld, "",
		"the state of the OAuth sign-in to a Gmail or Microsoft account (provider, account " +
			"subject, scopes); bound to this instance's OAuth client and to a token that is not " +
			"carried. The account is reconnected after a move"},
	"oauth_flows": {Withheld, "",
		"a sign-in to a mail provider in progress, holding a sealed one-time verifier; it " +
			"expires within minutes"},
	// Browser sessions (#937, migration 00082). Credentials: each row is a hashed
	// cookie secret plus a sealed provider refresh token for THIS instance.
	"sessions": {Withheld, "",
		"signed-in browsers of this instance (a hashed cookie secret and a sealed sign-in " +
			"provider token); credentials that are useless elsewhere. Everyone signs in again " +
			"after a move"},
	"pgp_decrypt_audit": {Withheld, "",
		"the crypto process's record of each decryption on this instance (message id, " +
			"outcome, key fingerprint, no content); an accountability trail, not the user's own data"},
}

// For reports a stable order beats map iteration.
type Row struct {
	Table string
	Entry Entry
}

// Rows returns every decision, sorted by table.
func Rows() []Row {
	out := make([]Row, 0, len(plan))
	for t, e := range plan {
		out = append(out, Row{Table: t, Entry: e})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Table < out[j].Table })
	return out
}

// Lookup returns the decision for a table, and whether one exists.
func Lookup(table string) (Entry, bool) {
	e, ok := plan[table]
	return e, ok
}

// Unhandled returns the tables present in the live schema that this map has no
// decision for, and the entries in the map that no longer exist in the schema.
//
// ⚠ BOTH DIRECTIONS MATTER, and only one of them is obvious. A table in the
// schema and not in the map is data an export would silently omit. A table in
// the map and not in the schema is a decision about something that no longer
// exists — harmless to the export, but it means this map has drifted, and a map
// that has drifted once is not evidence for anything else it claims.
//
// live is the catalog-derived table set (store.UserRowCensus), not a constant:
// a hardcoded list checked against a hardcoded map proves only that someone
// typed the same thing twice.
func Unhandled(live []string) (missing, stale []string) {
	inLive := make(map[string]bool, len(live))
	for _, t := range live {
		inLive[t] = true
		if _, ok := plan[t]; !ok {
			missing = append(missing, t)
		}
	}
	for t := range plan {
		if !inLive[t] {
			stale = append(stale, t)
		}
	}
	sort.Strings(missing)
	sort.Strings(stale)
	return missing, stale
}
