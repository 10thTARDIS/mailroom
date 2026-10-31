// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package store

import (
	"context"
	"crypto/sha256"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// testUID returns an IMAP UID that no other call in this test process has
// returned (#550).
//
// ⚠ IT REPLACES `NewULID().Time() % 1_000_000_000`, WHICH WAS THE CURRENT
// MILLISECOND. provenance_live_uq is (mailbox_id, uidvalidity, uid), and ULIDs
// minted within one millisecond share their time field by design, so two
// fixture rows placed in one mailbox inside a millisecond collided. It surfaced
// as TestSetupRunsAreUserIsolated dying in its own seed on a fast machine. The
// clock survives only as the starting point, so separate test processes against
// one database still begin far apart.
func testUID() int64 { return testUIDs.Add(1) }

var testUIDs = func() *atomic.Int64 {
	var n atomic.Int64
	n.Store(time.Now().UnixMilli() % 1_000_000_000)
	return &n
}()

// appTables is the complete list of tables in schema app. The RLS suite
// cross-checks it against pg_tables so that adding a table without fixture
// coverage (and therefore without isolation coverage) is a red build.
var appTables = []string{
	"users", "change_seq", "change_log", "change_floor",
	"accounts", "identities", "mailboxes",
	"blobs", "messages", "threads", "message_provenance", "attachments",
	"attachment_releases",
	"labels", "message_labels", "label_keyword_map", "category_tabs",
	"areas", "area_feeds", "desk_preferences", "client_preferences", "drawer_archives",
	"setup_runs", "setup_journal",
	"learned_placements", "learned_placement_gate_resets",
	"calendar_invites", "calendar_responses",
	"nudge_dismissals",
	"out_of_office",
	"message_local_auth",
	"pgp_decrypt_audit",
	"contact_key_pins",
	"contact_key_events",
	"contact_merges",
	"writeback_intents", "jobs",
	"submissions", "outbound_ledger", "dsn_events", "account_sync_status",
	"triage_scores", "feedback_events", "ml_models", "rules", "label_suggestions",
	"category_dismissals", "rule_corrections", "remote_allows", "compose_snippets",
	"saved_searches", "superseded_content",
	"proxy_image_cache", "proxy_image_refs",
	"contacts", "contact_addresses", "contact_keys",
	"user_keys", "api_keys", "api_key_events", "api_key_pending", "api_key_drafts", "api_key_suppressed_refusals", "push_subscriptions",
	"audit_log", "ai_providers", "sync_jobs",
	"oauth_grants", "oauth_flows",
	"sessions",
}

// systemTables are the tables in schema app that belong to the INSTANCE rather
// than to any user: no user_id column, and the inverse of "no identity, no
// rows" — their rows exist only where app.current_user_id() IS NULL. They
// cannot join appTables, whose suites seed per-user rows and query user_id, so
// they are listed here instead. TableInventoryComplete counts them, test 6 pins
// the shape that makes the exemption safe, and each has its own behavioural
// isolation test:
//
//	pgp_root_check   pgp_root_check_integration_test.go (#525)
var systemTables = []string{"pgp_root_check"}

// userScopeColumn names the column that scopes a table to a user.
// Everything is "user_id" except users itself.
func userScopeColumn(table string) string {
	if table == "users" {
		return "id"
	}
	return "user_id"
}

// fixtureUser is one seeded user with a full object graph: at least one row
// in every app table.
type fixtureUser struct {
	Tag       string
	ID        string
	AccountID string
	MessageID string
	// MailboxID is the seeded INBOX — needed by anything that places a message at an
	// IMAP coordinate (provenance).
	MailboxID string
	// IdentityID is the fixed (non-wildcard) identity — needed by anything that binds
	// an object to an address, e.g. a per-identity signature (#56).
	IdentityID string
}

// seedUser inserts a complete per-user object graph as the superuser
// (bypasses RLS — seeding only; assertions always run as app/admin roles).
// tag is embedded in every globally-unique value so seeding is repeatable
// within one database.
func seedUser(t *testing.T, super *pgxpool.Pool, tag string) fixtureUser {
	t.Helper()
	ctx := context.Background()

	u := fixtureUser{Tag: tag, ID: NewULID().String()}
	hash := sha256.Sum256([]byte("blob-" + tag))
	tokenHash := sha256.Sum256([]byte("api-token-" + tag))
	artifactHash := sha256.Sum256([]byte("model-" + tag))

	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := super.Exec(ctx, sql, args...); err != nil {
			// Include the first line of the statement for fast diagnosis.
			line := strings.SplitN(strings.TrimSpace(sql), "\n", 2)[0]
			t.Fatalf("seed %s: %s: %v", tag, line, err)
		}
	}

	exec(`INSERT INTO app.users (id, oidc_subject, email, display_name)
	      VALUES ($1, $2, $3, $4)`,
		u.ID, "oidc-"+tag, tag+"@example.com", "User "+tag)

	// Change machinery: counter, two log entries, floor.
	exec(`INSERT INTO app.change_seq (user_id, object_type, last_seq) VALUES ($1, 'Email', 2)`, u.ID)
	exec(`INSERT INTO app.change_floor (user_id, object_type, floor_seq) VALUES ($1, 'Email', 0)`, u.ID)

	u.AccountID = NewULID().String()
	exec(`INSERT INTO app.accounts (id, user_id, name, imap_host, smtp_host,
	        credentials_ct, credentials_dek)
	      VALUES ($1, $2, 'primary', 'mail.example.com', 'smtp.example.com', $3, $4)`,
		u.AccountID, u.ID, []byte{0x01}, NewULID().String())

	// Operational sync-health telemetry row (mailroom#43) — one per account, so
	// the RLS inventory/isolation suite covers app.account_sync_status.
	exec(`INSERT INTO app.account_sync_status (account_id, user_id, last_attempt_at, last_success_at)
	      VALUES ($1, $2, now(), now())`, u.AccountID, u.ID)

	identityID := NewULID().String()
	u.IdentityID = identityID
	exec(`INSERT INTO app.identities (id, user_id, account_id, kind, address)
	      VALUES ($1, $2, $3, 'fixed', $4)`,
		identityID, u.ID, u.AccountID, tag+"@example.com")

	mailboxID := NewULID().String()
	u.MailboxID = mailboxID
	exec(`INSERT INTO app.mailboxes (id, user_id, account_id, imap_name, display_name)
	      VALUES ($1, $2, $3, 'INBOX', 'Inbox')`,
		mailboxID, u.ID, u.AccountID)

	exec(`INSERT INTO app.blobs (user_id, content_hash, size_plain, size_stored, dek_id)
	      VALUES ($1, $2, 100, 132, $3)`,
		u.ID, hash[:], NewULID().String())

	threadID := NewULID().String()
	exec(`INSERT INTO app.threads (id, user_id, subject) VALUES ($1, $2, 'seed thread')`,
		threadID, u.ID)

	u.MessageID = NewULID().String()
	exec(`INSERT INTO app.messages (id, user_id, content_hash, thread_id, subject,
	        from_address, received_at, size_bytes, message_id_hint)
	      VALUES ($1, $2, $3, $4, 'seed message', $5, now(), 100, $6)`,
		u.MessageID, u.ID, hash[:], threadID, "friend@example.com", "<seed-"+tag+"@example.com>")

	// The two change_log rows reference the message.
	exec(`INSERT INTO app.change_log (user_id, object_type, seq, object_id, op)
	      VALUES ($1, 'Email', 1, $2, 'created'), ($1, 'Email', 2, $2, 'updated')`,
		u.ID, u.MessageID)

	exec(`INSERT INTO app.message_provenance (id, user_id, message_id, account_id,
	        mailbox_id, uid, uidvalidity)
	      VALUES ($1, $2, $3, $4, $5, 1, 1)`,
		NewULID().String(), u.ID, u.MessageID, u.AccountID, mailboxID)

	// The part digest is seeded too, because the quarantine's identity is the
	// CONTENT (#396, ADR-027 §8): a release row keys on it, so an attachment
	// fixture with a NULL part_hash could carry no release and the isolation
	// suite would be asserting over an empty table — a check that cannot fire.
	//
	// ⚠ The digest is per-USER, derived from tag, so the two fixture users do
	// NOT share content. Two users holding the same digest is a legitimate real
	// situation (a newsletter both subscribe to), but as a fixture it would make
	// the isolation assertions ambiguous: "B can see this release row" and "B
	// has an identical row of their own" look the same from the outside.
	partHash := sha256.Sum256([]byte("attachment-" + tag))
	exec(`INSERT INTO app.attachments (id, user_id, message_id, part_path, size_bytes, part_hash)
	      VALUES ($1, $2, $3, '2', 42, $4)`,
		NewULID().String(), u.ID, u.MessageID, partHash[:])
	exec(`INSERT INTO app.attachment_releases (user_id, part_hash, released_from_message_id, released_from_part)
	      VALUES ($1, $2, $3, '2')`,
		u.ID, partHash[:], u.MessageID)

	labelID := NewULID().String()
	exec(`INSERT INTO app.labels (id, user_id, name) VALUES ($1, $2, $3)`,
		labelID, u.ID, "label-"+tag)
	exec(`INSERT INTO app.message_labels (user_id, message_id, label_id) VALUES ($1, $2, $3)`,
		u.ID, u.MessageID, labelID)
	// An open classifier question (#169), so the RLS inventory has a
	// label_suggestions row to probe. A leak here would tell one user which
	// categories another user's mail is being considered for.
	exec(`INSERT INTO app.label_suggestions (user_id, message_id, label_id, confidence, model_version)
	      VALUES ($1, $2, $3, 0.5, 1)`,
		u.ID, u.MessageID, labelID)
	// A "this does not need a category" tombstone (#176), so the RLS inventory
	// has a category_dismissals row to probe. A leak here is worse than a
	// disclosure: the surface DELETES by caller-supplied id, so a hole would let
	// one user silently empty another's review queue.
	exec(`INSERT INTO app.category_dismissals (user_id, message_id) VALUES ($1, $2)`,
		u.ID, u.MessageID)
	// A superseded content hash (#260), so the RLS inventory has a
	// superseded_content row to probe. ⚠ A leak here would be a MESSAGE-LOSS
	// bug rather than a disclosure: ingest treats a hit as "we already hold
	// this", so one user's tombstone visible to another would make that other
	// user's genuinely new mail vanish into a message they cannot see. The
	// hash is deliberately derived from the user's own tag so the two users'
	// rows differ, which is what makes a cross-user hit observable at all.
	exec(`INSERT INTO app.superseded_content (user_id, content_hash, message_id)
	      VALUES ($1, sha256($3::bytea), $2)`,
		u.ID, u.MessageID, []byte("superseded-"+tag))
	// A cached proxied image (ADR-026), so the RLS inventory has a
	// proxy_image_cache row to probe. ⚠ A leak here discloses WHICH IMAGES
	// ANOTHER USER HAS BEEN SENT AND OPENED — the row exists only because they
	// opened a message containing it — and would let one user read another's
	// cached bytes. The url_hash is derived from the user's own tag so the two
	// users' rows differ, which is what makes a cross-user hit observable at
	// all rather than looking like a shared cache entry.
	exec(`INSERT INTO app.proxy_image_cache
	             (user_id, url_hash, content_hash, content_type, size_bytes)
	      VALUES ($1, sha256($2::bytea), sha256($3::bytea), 'image/png', 42)`,
		u.ID, []byte("proxy-url-"+tag), []byte("proxy-bytes-"+tag))
	// The message that keeps that cached image alive (#922), so the RLS
	// inventory has a proxy_image_refs row to probe. ⚠ A leak here discloses
	// which images arrived in another user's mail; a cross-user DELETE would
	// release their prefetched images to eviction, and a signed URL that has
	// expired since cannot be fetched again. Same per-user url_hash as above.
	exec(`INSERT INTO app.proxy_image_refs (user_id, url_hash, message_id)
	      VALUES ($1, sha256($2::bytea), $3)`,
		u.ID, []byte("proxy-url-"+tag), u.MessageID)
	exec(`INSERT INTO app.label_keyword_map (user_id, label_id, keyword_atom)
	      VALUES ($1, $2, $3)`,
		u.ID, labelID, "MailroomLabel-"+tag)

	exec(`INSERT INTO app.writeback_intents (id, user_id, message_id, account_id, op,
	        payload, idempotency_key)
	      VALUES ($1, $2, $3, $4, '+flag', '{"flag":"\\Seen"}'::jsonb, $5)`,
		NewULID().String(), u.ID, u.MessageID, u.AccountID, "wb-"+tag)

	// Fixture job fires far in the future so dequeue tests never claim it.
	exec(`INSERT INTO app.jobs (id, user_id, kind, fire_at, payload, idempotency_key)
	      VALUES ($1, $2, 'snooze_wake', now() + interval '1 hour', '{}'::jsonb, $3)`,
		NewULID().String(), u.ID, "job-"+tag)

	submissionID := NewULID().String()
	exec(`INSERT INTO app.submissions (id, user_id, account_id, identity_id, message_id,
	        from_address, envelope_to, send_at)
	      VALUES ($1, $2, $3, $4, $5, $6, ARRAY['friend@example.com']::citext[],
	              now() + interval '1 hour')`,
		submissionID, u.ID, u.AccountID, identityID, u.MessageID, tag+"@example.com")

	// One indexed invitation and one answer to it (#725, migration 00063), so
	// the RLS inventory probes both. A leak of either tells one user which
	// meetings another was invited to, by whom, and what they answered.
	exec(`INSERT INTO app.calendar_invites (user_id, message_id, uid, sequence, method, organizer)
	      VALUES ($1, $2, $3, 0, 'REQUEST', $4)`,
		u.ID, u.MessageID, "event-"+tag+"@example.com", "organizer-"+tag+"@example.net")
	exec(`INSERT INTO app.calendar_responses (id, user_id, message_id, uid, organizer, sequence,
	        partstat, submission_id)
	      VALUES ($1, $2, $3, $4, $5, 0, 'ACCEPTED', $6)`,
		NewULID().String(), u.ID, u.MessageID, "event-"+tag+"@example.com",
		"organizer-"+tag+"@example.net", submissionID)

	exec(`INSERT INTO app.outbound_ledger (account_id, user_id, day, sent_count)
	      VALUES ($1, $2, CURRENT_DATE, 1)`,
		u.AccountID, u.ID)

	exec(`INSERT INTO app.dsn_events (id, user_id, origin_message_id, recipient, action,
	        status_code)
	      VALUES ($1, $2, $3, 'friend@example.com', 'failed', '5.1.1')`,
		NewULID().String(), u.ID, u.MessageID)

	exec(`INSERT INTO app.triage_scores (user_id, message_id, score, model_version)
	      VALUES ($1, $2, 0.5, 'v0')`,
		u.ID, u.MessageID)

	exec(`INSERT INTO app.feedback_events (id, user_id, message_id, action)
	      VALUES ($1, $2, $3, 'open')`,
		NewULID().String(), u.ID, u.MessageID)

	exec(`INSERT INTO app.ml_models (id, user_id, model_kind, version, artifact_hash, active)
	      VALUES ($1, $2, 'priority', 1, $3, true)`,
		NewULID().String(), u.ID, artifactHash[:])

	// A valid, DISABLED rule: valid so RulesForEval/List can parse conditions
	// (object) + actions (array); disabled so it never perturbs an enabled-rules
	// count. It exists only so the RLS inventory has a rules row to probe.
	ruleID := NewULID().String()
	exec(`INSERT INTO app.rules (id, user_id, name, conditions, actions, enabled)
	      VALUES ($1, $2, $3, '{"field":"from","op":"exists"}'::jsonb, '[]'::jsonb, false)`,
		ruleID, u.ID, "rule-"+tag)
	// A correction the user made to that rule (#170), so the RLS inventory has a
	// rule_corrections row to probe. A leak here would tell one user which of
	// another's automations they had disagreed with, and on which messages.
	exec(`INSERT INTO app.rule_corrections (user_id, rule_id, message_id, label_id)
	      VALUES ($1, $2, $3, $4)`,
		u.ID, ruleID, u.MessageID, labelID)

	// A remembered "always load this sender's images" grant (#202), so the RLS
	// inventory has a remote_allows row to probe. A leak here would tell one user
	// WHICH senders another has decided to trust with a tracking pixel — a list of
	// correspondents, not merely a setting.
	exec(`INSERT INTO app.remote_allows (id, user_id, kind, value)
	      VALUES ($1, $2, 'address', $3)`,
		NewULID().String(), u.ID, tag+"-trusted@example.net")

	// A saved signature (#56), so the RLS inventory has a compose_snippets row. Its
	// body is text the user wrote, so a leak is closer to reading a draft.
	exec(`INSERT INTO app.compose_snippets (id, user_id, kind, name, body_text)
	      VALUES ($1, $2, 'signature', $3, $4)`,
		NewULID().String(), u.ID, "sig-"+tag, "regards, "+tag)

	// A saved search (#57), so the RLS inventory has a saved_searches row. The QUERY is
	// the sensitive part: it records what this user goes looking for in their own mail,
	// which is more revealing than the mail it would return.
	exec(`INSERT INTO app.saved_searches (id, user_id, name, query)
	      VALUES ($1, $2, $3, $4)`,
		NewULID().String(), u.ID, "smart-"+tag, "from:"+tag+"-watched@example.net is:unread")

	// One category tab row (#219), so the RLS inventory covers app.category_tabs.
	// Seeded rather than left to store.ListCategoryTabs's lazy materialisation: the
	// isolation suite reads these tables directly and must find a row that exists
	// before any code path runs, not one a later test happens to create.
	exec(`INSERT INTO app.category_tabs (id, user_id, tab, enabled, position)
	      VALUES ($1, $2, 'promotions', true, 2)`,
		NewULID().String(), u.ID)

	// One area (#442, migration 00046), owning the same label seeded above, so
	// the RLS inventory has areas and area_feeds rows to probe. The feed is the
	// area's OWN label — "areas are labels" — rather than a tab, so this fixture
	// also proves a label-fed area is visible to isolation coverage without
	// needing a second label.
	areaID := NewULID().String()
	exec(`INSERT INTO app.areas (id, user_id, name, position, label_id)
	      VALUES ($1, $2, $3, 0, $4)`,
		areaID, u.ID, "Area-"+tag, labelID)
	exec(`INSERT INTO app.area_feeds (id, user_id, area_id, label_id)
	      VALUES ($1, $2, $3, $4)`,
		NewULID().String(), u.ID, areaID, labelID)

	// The desk's settings singleton (#442): defaults are enough to give the RLS
	// inventory a desk_preferences row.
	exec(`INSERT INTO app.desk_preferences (user_id) VALUES ($1)`, u.ID)

	// One client family's display preferences (#442), so the RLS inventory has
	// a client_preferences row. 'web' is arbitrary; the point is one row exists.
	exec(`INSERT INTO app.client_preferences (id, user_id, family)
	      VALUES ($1, $2, 'web')`,
		NewULID().String(), u.ID)

	// One drawer-archive record (#443, migration 00048), so the RLS inventory
	// has a drawer_archives row. u.MessageID and areaID already exist above —
	// this fixture does not claim the message was actually swept, only that
	// the row exists for isolation coverage.
	exec(`INSERT INTO app.drawer_archives (user_id, message_id, area_id, area_name, period_days)
	      VALUES ($1, $2, $3, $4, 30)`,
		u.ID, u.MessageID, areaID, "Area-"+tag)

	// One learned placement and one gate reset (#714, migration 00060), so the
	// RLS inventory probes both. A leak of the first tells one user which of
	// another's correspondents Mailroom has learned to sort, and where.
	exec(`INSERT INTO app.learned_placements (user_id, message_id, key_kind, key_value, area_id)
	      VALUES ($1, $2, 'address', $3, $4)`,
		u.ID, u.MessageID, tag+"-sender@example.net", areaID)
	exec(`INSERT INTO app.learned_placement_gate_resets (user_id, area_id) VALUES ($1, $2)`,
		u.ID, areaID)

	// One follow-up nudge dismissal (#58, migration 00064): a leak would tell
	// one user which of another's conversations they have closed.
	exec(`INSERT INTO app.nudge_dismissals (user_id, message_id, kind) VALUES ($1, $2, 'outbound')`,
		u.ID, u.MessageID)

	// One out-of-office reply (#50, migration 00065), so the RLS inventory
	// probes it. A leak of it is a leak of the user's own words and, usually,
	// of when they are away.
	exec(`INSERT INTO app.out_of_office (user_id, account_id, enabled, subject, body)
	      VALUES ($1, $2, true, $3, 'away until Monday')`,
		u.ID, u.AccountID, "Away "+tag)

	// One local DKIM verdict (#776, migration 00066), so the RLS inventory
	// probes it. A leak would tell one user which of another's senders
	// authenticate.
	exec(`INSERT INTO app.message_local_auth (message_id, user_id, method, result, domain, aligned)
	      VALUES ($1, $2, 'dkim', 'pass', 'example.com', true)`,
		u.MessageID, u.ID)

	// One decrypt-trail row (#47, migration 00067), so the RLS inventory probes
	// it. A leak would tell one user which of another's messages were
	// encrypted and when they were opened. Its append-only grant is pinned by
	// pgp_decrypt_audit_integration_test.go.
	exec(`INSERT INTO app.pgp_decrypt_audit (id, user_id, message_id, purpose, outcome, key_fingerprint)
	      VALUES ($1, $2, $3, 'body', 'decrypted', 'ABCDEF0123456789')`,
		NewULID().String(), u.ID, u.MessageID)

	// One day-one setup run and its journal row (#446, migration 00050), so the
	// RLS inventory has both tables to probe. 'done' rather than 'queued' on
	// purpose: that is the status Setup/undo accepts, so the API-layer cross-user
	// probe reaches the code that would actually write.
	setupRunID := NewULID().String()
	exec(`INSERT INTO app.setup_runs (id, user_id, kind, status, window_days, plan)
	      VALUES ($1, $2, 'import', 'done', 30, $3::jsonb)`,
		setupRunID, u.ID,
		`{"windowDays":30,"folders":[{"mailboxId":"`+mailboxID+`","action":"labelArchive"}]}`)
	exec(`INSERT INTO app.setup_journal (id, user_id, run_id, message_id, label_id, desk_archived)
	      VALUES ($1, $2, $3, $4, $5, true)`,
		NewULID().String(), u.ID, setupRunID, u.MessageID, labelID)

	contactID := NewULID().String()
	exec(`INSERT INTO app.contacts (id, user_id, display_name) VALUES ($1, $2, 'Friend')`,
		contactID, u.ID)
	exec(`INSERT INTO app.contact_addresses (id, user_id, contact_id, address, reached_me_at)
	      VALUES ($1, $2, $3, 'friend@example.com', $4)`,
		NewULID().String(), u.ID, contactID, tag+"@example.com")
	contactKeyID := NewULID().String()
	exec(`INSERT INTO app.contact_keys (id, user_id, contact_id, kind, fingerprint,
	        public_key, source)
	      VALUES ($1, $2, $3, 'pgp', $4, $5, 'manual')`,
		contactKeyID, u.ID, contactID, "fp-contact-"+tag, []byte{0x02})
	// A REMOVED key (#815, migration 00071: soft delete) and one history event,
	// so the inventory probes both: a removed key is still the user's, and a
	// leak of either would tell one user which correspondents another dropped.
	removedKeyID := NewULID().String()
	exec(`INSERT INTO app.contact_keys (id, user_id, contact_id, kind, fingerprint,
	        public_key, source, removed_at)
	      VALUES ($1, $2, $3, 'pgp', $4, $5, 'wkd', now())`,
		removedKeyID, u.ID, contactID, "fp-removed-"+tag, []byte{0x04})
	exec(`INSERT INTO app.contact_key_events (id, user_id, contact_key_id, kind, source)
	      VALUES ($1, $2, $3, 'removed', 'user')`,
		NewULID().String(), u.ID, removedKeyID)

	// One merge record (#834, migration 00073) with its tombstone: a leak would
	// tell one user whom another merged, and the names and notes they held.
	tombstoneID := NewULID().String()
	exec(`INSERT INTO app.contacts (id, user_id, display_name, notes, merged_into)
	      VALUES ($1, $2, 'Merged Friend', 'merged notes', $3)`,
		tombstoneID, u.ID, contactID)
	exec(`INSERT INTO app.contact_merges (id, user_id, into_contact, from_contact, addresses, before_state, after_state)
	      VALUES ($1, $2, $3, $4, ARRAY[$5::text]::app.ulid[], '{"displayName":"Friend"}', '{"displayName":"Friend","notes":"merged notes"}')`,
		NewULID().String(), u.ID, contactID, tombstoneID, NewULID().String())

	// One encryption pin (#47, migration 00068): a leak would tell one user which
	// correspondents another encrypts to, and with which keys.
	exec(`INSERT INTO app.contact_key_pins (user_id, address, fingerprint, source)
	      VALUES ($1, 'friend@example.com', $2, 'manual')`,
		u.ID, "fp-pin-"+tag)

	exec(`INSERT INTO app.user_keys (id, user_id, kind, fingerprint, public_key)
	      VALUES ($1, $2, 'pgp', $3, $4)`,
		NewULID().String(), u.ID, "fp-user-"+tag, []byte{0x03})

	apiKeyID := NewULID().String()
	exec(`INSERT INTO app.api_keys (id, user_id, name, token_hash, scopes)
	      VALUES ($1, $2, 'seed key', $3, ARRAY['mail:read'])`,
		apiKeyID, u.ID, tokenHash[:])
	// What the key did and what it asked a human to approve (#49, migration
	// 00074): a leak would tell one user what another's agents read, sent and
	// deleted, and which of their mail is waiting on a decision.
	exec(`INSERT INTO app.api_key_events (id, user_id, key_id, method, outcome, cost, summary)
	      VALUES ($1, $2, $3, 'Email/get', 'ok', 1, '{"ids":1}')`,
		NewULID().String(), u.ID, apiKeyID)
	exec(`INSERT INTO app.api_key_drafts (user_id, message_id, key_id) VALUES ($1, $2, $3)`,
		u.ID, u.MessageID, apiKeyID)
	exec(`INSERT INTO app.api_key_suppressed_refusals (user_id, key_id, minute, outcome, method, reason, count)
	      VALUES ($1, $2, date_trunc('minute', now()), 'denied', 'Contact/get', 'scope', 3)`,
		u.ID, apiKeyID)
	exec(`INSERT INTO app.api_key_pending (id, user_id, key_id, kind, method, payload, summary, expires_at)
	      VALUES ($1, $2, $3, 'delete', 'Email/set', '{"destroy":[]}', 'Move 1 message to Trash', now() + interval '1 day')`,
		NewULID().String(), u.ID, apiKeyID)

	exec(`INSERT INTO app.push_subscriptions (id, user_id, endpoint, keys_p256dh, keys_auth)
	      VALUES ($1, $2, $3, 'p256dh', 'auth')`,
		NewULID().String(), u.ID, "https://push.example.com/"+tag)

	exec(`INSERT INTO app.audit_log (user_id, actor, action, object_type, object_id)
	      VALUES ($1, 'user', 'test.seed', 'Email', $2)`,
		u.ID, u.MessageID)

	exec(`INSERT INTO app.ai_providers (id, user_id, name, base_url)
	      VALUES ($1, $2, 'local', 'http://ollama.example.com')`,
		NewULID().String(), u.ID)

	exec(`INSERT INTO app.sync_jobs (id, user_id, account_id, kind, source)
	      VALUES ($1, $2, $3, 'bulk_import', 'mbox')`,
		NewULID().String(), u.ID, u.AccountID)

	// Mail-provider OAuth (#52, migration 00078): a grant on the seeded account
	// and a pending flow, so the RLS suite proves B can neither see nor consume
	// A's (oauth_grants_rls_integration_test.go drives the store methods too).
	exec(`INSERT INTO app.oauth_grants (account_id, user_id, provider, subject, address, scopes)
	      VALUES ($1, $2, 'google', $3, $4, ARRAY['https://mail.google.com/'])`,
		u.AccountID, u.ID, "sub-"+tag, tag+"@example.com")
	flowState := sha256.Sum256([]byte("oauth-state-" + tag))
	exec(`INSERT INTO app.oauth_flows (id, user_id, state_hash, provider, verifier_ct, nonce, expires_at)
	      VALUES ($1, $2, $3, 'google', $4, $5, now() + interval '10 minutes')`,
		NewULID().String(), u.ID, flowState[:], []byte{0x02}, "nonce-"+tag)

	// A browser session (#937, migration 00082): the RLS suite proves B can
	// neither read, touch nor revoke it.
	sessionHash := sha256.Sum256([]byte("session-secret-" + tag))
	exec(`INSERT INTO app.sessions (id, user_id, token_hash, user_agent)
	      VALUES ($1, $2, $3, 'Seed Browser')`,
		NewULID().String(), u.ID, sessionHash[:])

	return u
}

// seedTwoUsers seeds users A and B with fresh globally-unique tags and
// verifies every app table holds at least one row for each — the
// precondition that makes the negative assertions meaningful.
func seedTwoUsers(t *testing.T, super *pgxpool.Pool) (a, b fixtureUser) {
	t.Helper()
	ctx := context.Background()
	run := NewULID().String() // unique per seeding, so reseeding never collides
	a = seedUser(t, super, "a-"+run)
	b = seedUser(t, super, "b-"+run)

	for _, table := range appTables {
		col := userScopeColumn(table)
		for _, u := range []fixtureUser{a, b} {
			var n int
			err := super.QueryRow(ctx,
				`SELECT count(*) FROM app.`+table+` WHERE `+col+` = $1`, u.ID).Scan(&n)
			if err != nil {
				t.Fatalf("fixture count %s: %v", table, err)
			}
			if n == 0 {
				t.Fatalf("fixture gap: table app.%s has no rows for user %s — extend seedUser", table, u.Tag)
			}
		}
	}
	return a, b
}
