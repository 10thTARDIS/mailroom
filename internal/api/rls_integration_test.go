// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package api

// The API-layer RLS negative-test suite — data-model §5, "test 4": authenticated
// as user A, naming user B's object ids returns notFound / 404 / empty, NEVER
// B's data, and for /set never mutates B's objects. It is the one-level-up
// counterpart to internal/store/rls_test.go's store-layer suite: that one proves
// the SQL policies isolate rows; this one proves the JMAP surface built on top of
// them never opens a door around the policies (a request-supplied account id, a
// content-addressed blobId, an id echoed into a query).
//
// It runs under REAL RLS: the API's store is the mailroom_app pool
// (scripts/integration-test.sh), so every assertion below is enforced by the same
// policies production runs.
//
// EXHAUSTIVENESS is not a matter of diligence here — it is enforced. crossUser
// Probes must cover the server's entire method table (checked below, and again
// without a database in rls_forcing_test.go). Adding a JMAP method without a
// cross-user probe is a red build.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/oklog/ulid/v2"

	"mailroom/internal/attachscan"
	"mailroom/internal/auth"
	"mailroom/internal/ml"
	"mailroom/internal/outofoffice"
	"mailroom/internal/search"
	"mailroom/internal/store"
)

// rlsUser is a fixture user that owns an object of EVERY JMAP-visible type: the
// email/mailbox/thread/label/blob seedAPIUser gives it, plus an identity and a
// submission seeded here.
type rlsUser struct {
	apiFixture
	IdentityID    ulid.ULID
	SubmissionID  ulid.ULID
	RuleID        ulid.ULID
	RemoteAllowID ulid.ULID // a remembered per-sender remote-content grant (#202)
	// RemoteAllowValue is B's trusted sender address. Distinctive per user because
	// the leak that matters here is A learning WHICH senders B trusts, not merely
	// that a grant exists.
	RemoteAllowValue string
	SnippetID        ulid.ULID // a saved signature (#56)
	// SnippetName is B's signature name. Distinctive per user because a snippet's
	// content is the user's own words, not a setting.
	SnippetName string
	SavedID     ulid.ULID // a saved search / smart folder (#57)
	// SavedQuery is B's saved query text. Distinctive per user because THE QUERY IS
	// THE LEAK: it says what B goes looking for in their own mail — who they check on,
	// what they worry about — which is more revealing than the fact a folder exists.
	SavedQuery string
	SavedName  string
	// CategoryTabID is B's row for one tab (#219). Unlike the objects above it
	// carries no free text to leak — only whether a tab is on and where it sits —
	// so the id alone is the whole read-side identifier. The write side is where
	// this one bites: disabling B's tab would move a class of their mail into
	// Primary without altering a single message.
	CategoryTabID  ulid.ULID
	ContactID      ulid.ULID
	ModelVersionID ulid.ULID // the seeded priority-model row id (#34 Triage probes)
	ContactAddr    string    // an auto-derived correspondent address — distinctive per user
	BlobID         string    // the message blob's content hash, hex — a client's blobId
	AccountName    string    // the mail account's display name — distinctive per user so a
	// name-level leak (EmailSubmission/getSendQuota) is catchable, not masked by a
	// shared default like "primary".
	// AreaID is B's one seeded area (#442), fed by B's own label ID — reusing
	// LabelID rather than minting a second one, exactly as CategoryTabID is a
	// single seeded row rather than left to lazy materialisation, so naming
	// this id in a probe names a row that really exists. It also carries a
	// TAB feed ("updates") shared with A's own area — the sharp case Area/set
	// exists to probe: RLS scopes the tab-uniqueness index by user_id, so A
	// claiming the same tab for A's own area must succeed without A ever
	// learning B already holds it.
	AreaID ulid.ULID
	// ClientPrefWebID is B's seeded `web`-family ClientPreferences row (#442),
	// seeded directly (not left to lazy materialisation) for the same reason
	// as AreaID/CategoryTabID.
	ClientPrefWebID ulid.ULID
	// UserKeyID is B's own seeded PGP key (#448/#374 increment 1, spec §9).
	// UserKeyFingerprint is per-user distinctive, exactly like RemoteAllowValue,
	// so a leak of the FINGERPRINT (without the id) is caught too.
	UserKeyID          ulid.ULID
	UserKeyFingerprint string
	// SetupRunID is B's one finished day-one setup run (#446), seeded with a
	// journal row against B's own message and label so the sharp probe has real
	// state to fail to touch: Setup/undo names a run id and then DELETES LABELS
	// and MOVES MAIL from its journal, so a run that resolved across users would
	// be A stripping B's filing and enqueueing IMAP moves on B's account. A run
	// with an empty journal would make that probe pass vacuously.
	SetupRunID ulid.ULID
	// SetupLabelID is the label that run "wrote", attributed 'setup'. Separate
	// from LabelID because the two attributions are the whole point: undo removes
	// only a setup-applied label, and Rule/induce learns only from a user-applied
	// one.
	SetupLabelID ulid.ULID
	// OOFSubject is B's saved out-of-office subject (#50): the user's own words
	// (and often where they are), so a leak of it is a leak of content.
	OOFSubject string
}

// identifiers is every id/atom that is B's and must NEVER surface in A's
// responses. Used by the leak assertion for read methods.
func (u rlsUser) identifiers() []string {
	return []string{
		u.MessageID.String(), u.ThreadID.String(), u.InboxID.String(), u.TrashID.String(),
		u.LabelID.String(), u.LabelAtom, u.IdentityID.String(), u.SubmissionID.String(),
		u.RuleID.String(), u.ContactID.String(), u.ModelVersionID.String(), u.ContactAddr,
		u.RemoteAllowID.String(), u.RemoteAllowValue,
		u.SnippetID.String(), u.SnippetName,
		u.SavedID.String(), u.SavedQuery, u.SavedName, u.CategoryTabID.String(),
		u.BlobID, u.UserID.String(), u.AccountID.String(), u.AccountName,
		u.AreaID.String(), u.ClientPrefWebID.String(),
		u.UserKeyID.String(), u.UserKeyFingerprint,
		u.SetupRunID.String(), u.SetupLabelID.String(),
		u.OOFSubject,
	}
}

// rlsHarness is the compose harness (real store, composer, blob store) with a
// search service wired in, so the FULL method table — including SearchSnippet/get
// and the search-aware Email/query — is registered and therefore probed.
type rlsHarness struct {
	*composeHarness
	// custody is the real crypto service over the store (#796), counting calls
	// so a probe can show a cross-user request never reached it.
	custody *inProcCustody
}

func newRLSHarness(t *testing.T) *rlsHarness {
	t.Helper()
	h := newComposeHarness(t)
	ix, err := search.NewIndexer(t.TempDir(), search.MemoryFactory, search.NewStoreSource(h.store), nil)
	if err != nil {
		t.Fatalf("search indexer: %v", err)
	}
	h.api.SetSearch(search.NewService(ix, search.NewStoreResolver(h.store)))
	// Wire the priority-model registry over the harness's real store + blob store so
	// the Triage control probes exercise the actual reset/rollback/correct paths
	// under RLS (a nil registry would fail-closed before reaching any user data,
	// which is not the isolation claim under test).
	h.api.registry = ml.NewRegistry(h.store, h.blobs, fakeMasterKeys{})
	// Out of office (#50) over the real store. No credentials opener: a probe
	// that reached a dial would have resolved B's account, which is the leak.
	h.api.outOfOffice = outofoffice.NewManager(h.store, nil)
	// Own-key management (#796) over the real crypto service and store, so the
	// UserKey probes exercise the real paths rather than failing closed on a
	// missing custody.
	c := newInProcCustody(t, h.store, nil)
	h.api.custody = c
	h.api.wkdDomain = "example.com"
	return &rlsHarness{composeHarness: h, custody: c}
}

func seedRLSTwo(t *testing.T, super *pgxpool.Pool) (a, b rlsUser) {
	t.Helper()
	run := store.NewULID().String()
	return seedRLSUser(t, super, "a-"+run), seedRLSUser(t, super, "b-"+run)
}

// seedRLSUser extends seedAPIUser (email/mailbox/thread/label/blob) with a
// verified identity and a scheduled submission, so B genuinely owns one of every
// JMAP-visible type — naming B's identity/submission id is then a real cross-user
// probe, not a probe for an id that never existed.
func seedRLSUser(t *testing.T, super *pgxpool.Pool, tag string) rlsUser {
	t.Helper()
	f := seedAPIUser(t, super, tag)
	ctx := context.Background()
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := super.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("seed rls %s: %v", tag, err)
		}
	}

	idID := store.NewULID()
	exec(`INSERT INTO app.identities (id, user_id, account_id, kind, address, display_name, enabled)
	      VALUES ($1,$2,$3,'fixed',$4,$5,true)`,
		idID.String(), f.UserID.String(), f.AccountID.String(), tag+"-me@example.com", "Owner "+tag)

	subID := store.NewULID()
	exec(`INSERT INTO app.submissions
	        (id, user_id, account_id, identity_id, message_id, from_address, envelope_to, state)
	      VALUES ($1,$2,$3,$4,$5,$6,$7::citext[],'scheduled')`,
		subID.String(), f.UserID.String(), f.AccountID.String(), idID.String(), f.MessageID.String(),
		tag+"-me@example.com", []string{"friend@example.net"})

	// A T0 rule (mailroom#32): B genuinely owns a Rule object, so naming B's rule
	// id, or trying to rename/destroy it, is a real cross-user probe. Its name is
	// per-user distinctive so a leak is catchable.
	ruleID := store.NewULID()
	exec(`INSERT INTO app.rules (id, user_id, name, priority, conditions, actions, enabled)
	      VALUES ($1,$2,$3,100,$4::jsonb,$5::jsonb,true)`,
		ruleID.String(), f.UserID.String(), "rule-"+tag,
		`{"field":"from","op":"equals","value":"`+tag+`-corr@example.net"}`,
		`[{"type":"addLabel","label":"`+f.LabelID.String()+`"}]`)

	// A correction against that rule (#170). Seeded for BOTH users on purpose:
	// without it, Rule/get's corrections count and Rule/tighten's window would
	// read zero for A whether isolation held or not, and a probe that passes on
	// an empty table proves nothing.
	exec(`INSERT INTO app.rule_corrections (user_id, rule_id, message_id, label_id)
	      VALUES ($1,$2,$3,$4)`,
		f.UserID.String(), ruleID.String(), f.MessageID.String(), f.LabelID.String())

	// Rename the account to a per-user distinctive value (seedAPIUser names every
	// account "primary"): a getSendQuota leak of B's account NAME is then visible,
	// where a shared "primary" would mask it.
	acctName := "acct-" + tag
	exec(`UPDATE app.accounts SET name = $1 WHERE user_id = $2`, acctName, f.UserID.String())

	// An auto-derived contact (as ingest would create): naming B's contact id, or
	// filtering Contact/query by B's correspondent address, is then a real probe.
	contactID := store.NewULID()
	contactAddr := tag + "-corr@example.net"
	exec(`INSERT INTO app.contacts (id, user_id, display_name, created_at)
	      VALUES ($1,$2,$3, now())`,
		contactID.String(), f.UserID.String(), "Corr "+tag)
	exec(`INSERT INTO app.contact_addresses
	        (id, user_id, contact_id, address, first_seen_at, last_seen_at, seen_count)
	      VALUES ($1,$2,$3,$4, now(), now(), 1)`,
		store.NewULID().String(), f.UserID.String(), contactID.String(), contactAddr)

	// An active priority-model version (#34/#35): B genuinely owns a model row, so a
	// Triage/get that leaked would surface B's lineage, and a Triage/set that leaked
	// would touch B's active row. The artifact_hash is a placeholder — Triage/get is
	// a pure lineage read that never loads the artifact — while Triage/set{reset} in
	// A's own probe writes a real baseline blob through the registry.
	modelID := store.NewULID()
	exec(`INSERT INTO app.ml_models
	        (id, user_id, model_kind, version, parent_id, artifact_hash, active, created_at)
	      VALUES ($1,$2,'priority',1,NULL,$3,true, now())`,
		modelID.String(), f.UserID.String(), make([]byte, 32))

	var hash []byte
	if err := super.QueryRow(ctx,
		`SELECT content_hash FROM app.blobs WHERE user_id=$1 LIMIT 1`,
		f.UserID.String()).Scan(&hash); err != nil {
		t.Fatalf("seed rls %s: read blob hash: %v", tag, err)
	}

	// A remembered remote-content grant (#202). Per-user distinctive VALUE as well as
	// id: this row decides whether a stranger's images load, so the leak that matters
	// is A learning WHICH senders B trusts, not just that a row exists.
	allowID := store.NewULID()
	allowValue := tag + "-trusted@example.net"
	exec(`INSERT INTO app.remote_allows (id, user_id, kind, value)
	      VALUES ($1, $2, 'address', $3)`,
		allowID.String(), f.UserID.String(), allowValue)

	// A saved signature (#56), bound to this user's identity so the probe covers the
	// identity-linked form. Its NAME is per-user distinctive: a snippet body is text
	// the user wrote, so a leak here is closer to reading their draft than their
	// preferences.
	snippetID := store.NewULID()
	snippetName := "sig-" + tag
	exec(`INSERT INTO app.compose_snippets
	        (id, user_id, kind, identity_id, name, body_html, body_text)
	      VALUES ($1, $2, 'signature', $3, $4, '', $5)`,
		snippetID.String(), f.UserID.String(), idID.String(), snippetName, "regards, "+tag)

	savedID := store.NewULID()
	savedName := "smart-" + tag
	savedQuery := "from:" + tag + "-watched@example.net is:unread"
	exec(`INSERT INTO app.saved_searches (id, user_id, name, query, in_sidebar, position)
	      VALUES ($1, $2, $3, $4, true, 0)`,
		savedID.String(), f.UserID.String(), savedName, savedQuery)

	// ONE category tab row (#219), seeded rather than left to lazy materialisation
	// so naming this id in a probe names a row that really exists — the same
	// reason this fixture seeds an identity and a submission. Deliberately NOT
	// primary and deliberately in a non-default state (disabled, position 7): if a
	// cross-user write did land, "unchanged" is then a claim with content instead
	// of a value that happens to match the default the writer would have set.
	tabID := store.NewULID()
	exec(`INSERT INTO app.category_tabs (id, user_id, tab, enabled, position)
	      VALUES ($1, $2, 'promotions', false, 7)`,
		tabID.String(), f.UserID.String())

	// The desk (#442): a seeded DeskPreferences row, ONE area (fed by this
	// user's own label — the Area/set probe adds a TAB feed to B's area
	// specifically, to set up its own sharp case), and ONE ClientPreferences
	// row. All seeded directly rather than left to lazy materialisation, for
	// CategoryTabID's own reason.
	exec(`INSERT INTO app.desk_preferences (user_id, line_threshold, drawer_default_days, tray_nag_days)
	      VALUES ($1, 0.42, 21, 5)`, f.UserID.String())

	areaID := store.NewULID()
	exec(`INSERT INTO app.areas (id, user_id, name, position, label_id, hidden_from_rail)
	      VALUES ($1, $2, $3, 7, $4, false)`,
		areaID.String(), f.UserID.String(), "area-"+tag, f.LabelID.String())
	exec(`INSERT INTO app.area_feeds (id, user_id, area_id, label_id)
	      VALUES ($1, $2, $3, $4)`,
		store.NewULID().String(), f.UserID.String(), areaID.String(), f.LabelID.String())

	clientPrefID := store.NewULID()
	exec(`INSERT INTO app.client_preferences (id, user_id, family, theme, density, message_opens, pane_split_pct)
	      VALUES ($1, $2, 'web', 'dark', 'compact', 'focused', 33)`,
		clientPrefID.String(), f.UserID.String())

	// A PGP key (#448/#374 increment 1, spec §9): B genuinely owns a UserKey
	// row, seeded directly (bypassing InsertUserKey, which requires a real
	// crypto-process-sealed private half this fixture has no business minting)
	// so UserKey/get has a real id to be probed against. The fingerprint is
	// per-user distinctive, like RemoteAllowValue, so a leak of the value
	// itself (not just the id) is caught. private_key_ct is a fake, non-empty
	// ciphertext so a "does the response leak it" probe means something.
	userKeyID := store.NewULID()
	userKeyFingerprint := "FPR-" + tag
	exec(`INSERT INTO app.user_keys
	        (id, user_id, kind, fingerprint, public_key, private_key_ct, envelope_ver, decrypt_only, wkd_published)
	      VALUES ($1, $2, 'pgp', $3, $4, $5, 1, false, true)`,
		userKeyID.String(), f.UserID.String(), userKeyFingerprint,
		[]byte("pub-"+tag), []byte("privct-"+tag))

	// A FINISHED day-one setup run with a journal row (#446). 'done' on purpose:
	// that is the one status Setup/undo accepts, so the probe exercises the path
	// that would actually write — a 'running' run would be refused for a reason
	// that has nothing to do with isolation, and the probe would pass without
	// ever reaching the journal.
	// The label the run "wrote", attributed to the setup, and the archive mark it
	// set — so an undo that leaked across users would have something real to
	// destroy, and "unchanged" is a claim with content.
	//
	// ⚠ A SECOND LABEL, not a re-attribution of the fixture's existing one. The
	// first attempt reused f.LabelID and set applied_by='setup' on it, which made
	// the undo probe real and broke the Rule/induce probe — that one needs the
	// SAME row to say 'user', because induction learns only from the user's own
	// labelling. Both were measured as red tests, and they are the point of
	// applied_by: a run's label and a person's label are different facts about
	// different rows, and undo must be able to tell them apart.
	//
	// It is created BEFORE the journal row below, which has an FK onto it.
	setupLabelID := store.NewULID()
	exec(`INSERT INTO app.labels (id, user_id, name) VALUES ($1,$2,$3)`,
		setupLabelID.String(), f.UserID.String(), "setup-"+tag)
	exec(`INSERT INTO app.message_labels (user_id, message_id, label_id, applied_by)
	      VALUES ($1, $2, $3, 'setup')`,
		f.UserID.String(), f.MessageID.String(), setupLabelID.String())
	exec(`UPDATE app.messages SET desk_archived_at = now() WHERE user_id = $1 AND id = $2`,
		f.UserID.String(), f.MessageID.String())

	setupRunID := store.NewULID()
	exec(`INSERT INTO app.setup_runs (id, user_id, kind, status, window_days, plan,
	        planned_labels, labels_written, planned_archived, archived, started_at, finished_at)
	      VALUES ($1, $2, 'import', 'done', 30, $3::jsonb, 1, 1, 1, 1, now(), now())`,
		setupRunID.String(), f.UserID.String(),
		`{"windowDays":30,"folders":[{"mailboxId":"`+f.InboxID.String()+`","action":"labelArchive"}]}`)
	exec(`INSERT INTO app.setup_journal (id, user_id, run_id, message_id, label_id, desk_archived)
	      VALUES ($1, $2, $3, $4, $5, true)`,
		store.NewULID().String(), f.UserID.String(), setupRunID.String(),
		f.MessageID.String(), setupLabelID.String())

	// An out-of-office row (#50), enabled with a distinctive subject, so the
	// OutOfOffice probes have real state to fail to read or overwrite.
	oofSubject := "oof-away-" + tag
	exec(`INSERT INTO app.out_of_office (user_id, account_id, enabled, subject, body, server_state)
	      VALUES ($1, $2, true, $3, 'gone fishing', 'active')`,
		f.UserID.String(), f.AccountID.String(), oofSubject)

	return rlsUser{apiFixture: f, IdentityID: idID, SubmissionID: subID, RuleID: ruleID,
		OOFSubject:    oofSubject,
		RemoteAllowID: allowID, RemoteAllowValue: allowValue,
		SnippetID: snippetID, SnippetName: snippetName,
		SavedID: savedID, SavedQuery: savedQuery, SavedName: savedName,
		CategoryTabID: tabID,
		ContactID:     contactID, ModelVersionID: modelID, ContactAddr: contactAddr,
		BlobID: hex.EncodeToString(hash), AccountName: acctName,
		AreaID: areaID, ClientPrefWebID: clientPrefID,
		UserKeyID: userKeyID, UserKeyFingerprint: userKeyFingerprint,
		SetupRunID: setupRunID, SetupLabelID: setupLabelID}
}

// TestRLSCrossUserEveryMethod is the suite. It first re-checks the forcing
// function against the LIVE method table (so a method with no probe fails even if
// the unit tripwire was somehow bypassed), then runs every probe.
func TestRLSCrossUserEveryMethod(t *testing.T) {
	h := newRLSHarness(t)
	a, b := seedRLSTwo(t, h.super)

	for name := range h.api.methods {
		if _, ok := crossUserProbes[name]; !ok {
			t.Errorf("method %q is registered but has no cross-user probe in crossUserProbes", name)
		}
	}
	for name := range crossUserProbes {
		if _, ok := h.api.methods[name]; !ok {
			t.Errorf("crossUserProbes has %q but it is not a registered method", name)
		}
		if !crossUserProbedMethods[name] {
			t.Errorf("crossUserProbes covers %q but crossUserProbedMethods (the unit tripwire) does not", name)
		}
	}

	for name, probe := range crossUserProbes {
		t.Run(name, func(t *testing.T) { probe(t, h, a, b) })
	}
}

// crossUserProbes drives one method AS USER A, naming USER B's objects. The map
// key is the JMAP method name and must equal the server's registered method set
// (asserted above). Each probe fails loudly on any leak or cross-user mutation.
var crossUserProbes = map[string]func(t *testing.T, h *rlsHarness, a, b rlsUser){
	// Core/echo has no store and no per-user data. The probe proves exactly that:
	// it echoes its argument and can be called with a NIL-store API without
	// panicking — i.e. it never reaches user data at all.
	"Core/echo": func(t *testing.T, h *rlsHarness, a, b rlsUser) {
		res, mErr := invokeAs(t, h, "Core/echo", a.UserID, map[string]any{"x": 1})
		if mErr != nil || res["x"] != float64(1) {
			t.Errorf("Core/echo = %v, %+v", res, mErr)
		}
		if _, err := echo(context.Background(), b.UserID, json.RawMessage(`{"y":2}`)); err != nil {
			t.Errorf("echo touched something it shouldn't: %+v", err)
		}
	},

	// ---- reads: naming B's id yields nothing of B's ------------------------
	"Email/get":   probeGetFindsNothing("Email/get", "ids", func(b rlsUser) string { return b.MessageID.String() }),
	"Mailbox/get": probeGetFindsNothing("Mailbox/get", "ids", func(b rlsUser) string { return b.InboxID.String() }),
	"Thread/get":  probeGetFindsNothing("Thread/get", "ids", func(b rlsUser) string { return b.ThreadID.String() }),
	"Thread/set": func(t *testing.T, h *rlsHarness, a, b rlsUser) {
		// A muting B's thread must neither mute it nor confirm it exists. The
		// second half matters as much as the first: a notFound that only appeared
		// for *absent* ids would turn this method into an existence oracle for
		// other users' threads.
		res, mErr := invokeAs(t, h, "Thread/set", a.UserID, map[string]any{
			"update": map[string]any{b.ThreadID.String(): map[string]any{"isMuted": true}},
		})
		if mErr != nil {
			t.Fatalf("Thread/set: %+v", mErr)
		}
		if !mapHasKey(res, "notUpdated", b.ThreadID.String()) {
			t.Errorf("Thread/set did not refuse B's thread: %v", res)
		}
		// And prove it stayed unmuted, read back AS B — the assertion that would
		// catch a write leaking past RLS while still reporting notUpdated.
		if h.threadMuted(t, b.UserID, b.ThreadID) {
			t.Error("A muted B's thread")
		}
	},
	"Label/get":           probeGetFindsNothing("Label/get", "ids", func(b rlsUser) string { return b.LabelID.String() }),
	"Rule/get":            probeGetFindsNothing("Rule/get", "ids", func(b rlsUser) string { return b.RuleID.String() }),
	"Identity/get":        probeGetFindsNothing("Identity/get", "ids", func(b rlsUser) string { return b.IdentityID.String() }),
	"EmailSubmission/get": probeGetFindsNothing("EmailSubmission/get", "ids", func(b rlsUser) string { return b.SubmissionID.String() }),
	"Account/get":         probeGetFindsNothing("Account/get", "ids", func(b rlsUser) string { return b.AccountID.String() }),
	"Contact/get":         probeGetFindsNothing("Contact/get", "ids", func(b rlsUser) string { return b.ContactID.String() }),

	// ---- queries: A's result never contains B's ids ------------------------
	"Email/query": func(t *testing.T, h *rlsHarness, a, b rlsUser) {
		// Unfiltered: only A's own message.
		res, mErr := invokeAs(t, h, "Email/query", a.UserID, map[string]any{})
		if mErr != nil {
			t.Fatalf("Email/query: %+v", mErr)
		}
		assertNoBLeak(t, "Email/query", res, b)
		// Filtering by B's mailbox selects nothing for A — B's mailbox is invisible,
		// it does not become a window into B's mail.
		res, mErr = invokeAs(t, h, "Email/query", a.UserID, map[string]any{
			"filter": map[string]any{"inMailbox": b.InboxID.String()},
		})
		if mErr != nil {
			t.Fatalf("Email/query inMailbox=B: %+v", mErr)
		}
		if ids := asArr(res["ids"]); len(ids) != 0 {
			t.Errorf("Email/query inMailbox=B returned %d ids, want 0", len(ids))
		}
		// Filtering by B's ACCOUNT (the #26 inAccount filter) is likewise not a
		// window into B's mail — B's provenance rows are RLS-invisible to A.
		res, mErr = invokeAs(t, h, "Email/query", a.UserID, map[string]any{
			"filter": map[string]any{"inAccount": b.AccountID.String()},
		})
		if mErr != nil {
			t.Fatalf("Email/query inAccount=B: %+v", mErr)
		}
		if ids := asArr(res["ids"]); len(ids) != 0 {
			t.Errorf("Email/query inAccount=B returned %d ids, want 0", len(ids))
		}
	},
	"Mailbox/query": probeQueryNoLeak("Mailbox/query"),
	"Label/query":   probeQueryNoLeak("Label/query"),
	"Rule/query":    probeQueryNoLeak("Rule/query"),
	"Account/query": probeQueryNoLeak("Account/query"),
	// Contact/query is the autocomplete surface; drive it AS A with a filter that
	// matches B's correspondent address and assert nothing of B's surfaces.
	// #653: B's fixture submission is SCHEDULED, i.e. in B's Outbox, so each of
	// these three would return it if the query were not scoped to A: unfiltered,
	// the Outbox filter, and a filter naming B's own message.
	"EmailSubmission/query": func(t *testing.T, h *rlsHarness, a, b rlsUser) {
		for _, filter := range []map[string]any{
			nil,
			{"inOutbox": true},
			{"emailIds": []string{b.MessageID.String()}},
		} {
			args := map[string]any{"calculateTotal": true}
			if filter != nil {
				args["filter"] = filter
			}
			res, mErr := invokeAs(t, h, "EmailSubmission/query", a.UserID, args)
			if mErr != nil {
				t.Fatalf("EmailSubmission/query %v: %+v", filter, mErr)
			}
			assertNoBLeak(t, "EmailSubmission/query", res, b)
		}
	},
	"Contact/query": func(t *testing.T, h *rlsHarness, a, b rlsUser) {
		res, mErr := invokeAs(t, h, "Contact/query", a.UserID, map[string]any{
			"filter": map[string]any{"text": b.ContactAddr},
		})
		if mErr != nil {
			t.Fatalf("Contact/query: %+v", mErr)
		}
		assertNoBLeak(t, "Contact/query", res, b)
		// #59's exact-address and marker filters: B's address names nothing
		// for A, and A's blocked/VIP lists hold none of B's contacts even
		// after B marks one.
		superExec(t, h, `UPDATE app.contacts SET vip = true, blocked = true, blocked_at = now() WHERE id = $1`,
			b.ContactID.String())
		for _, f := range []map[string]any{
			{"address": b.ContactAddr},
			{"isVip": true},
			{"isBlocked": true},
		} {
			res, mErr := invokeAs(t, h, "Contact/query", a.UserID, map[string]any{"filter": f})
			if mErr != nil {
				t.Fatalf("Contact/query %v: %+v", f, mErr)
			}
			assertNoBLeak(t, "Contact/query", res, b)
		}
	},

	// #53: A's sender ranking holds none of B's senders, in any grouping.
	"SenderStats/get": func(t *testing.T, h *rlsHarness, a, b rlsUser) {
		if _, mErr := invokeAs(t, h, "SenderStats/get", a.UserID,
			map[string]any{"accountId": b.UserID.String()}); mErr == nil || mErr.Type != errAccountNotFound {
			t.Fatalf("SenderStats/get naming B's account: got %+v, want accountNotFound", mErr)
		}
		superExec(t, h, `UPDATE app.messages SET from_address = $2, list_id = '<b-only.lists.example>' WHERE id = $1`,
			b.MessageID.String(), b.ContactAddr)
		for _, g := range []string{"address", "domain", "list"} {
			res, mErr := invokeAs(t, h, "SenderStats/get", a.UserID, map[string]any{"groupBy": g})
			if mErr != nil {
				t.Fatalf("SenderStats/get %s: %+v", g, mErr)
			}
			assertNoBLeak(t, "SenderStats/get "+g, res, b)
			for _, item := range asArr(res["list"]) {
				if k, _ := item.(map[string]any)["key"].(string); k == "b-only.lists.example" {
					t.Errorf("A's list ranking holds B's list")
				}
			}
		}
		// Positive control: B sees their own sender.
		res, mErr := invokeAs(t, h, "SenderStats/get", b.UserID, map[string]any{})
		if mErr != nil {
			t.Fatalf("SenderStats/get as B: %+v", mErr)
		}
		if len(asArr(res["list"])) == 0 {
			t.Errorf("B's own ranking is empty — the negative above proves nothing: %v", res)
		}
	},

	// #58: follow-up nudges are computed from the caller's mail only. B has a
	// nudge due (an unanswered sent message, ten days old); A sees none of it,
	// and B — the positive control — does.
	"FollowUp/get": func(t *testing.T, h *rlsHarness, a, b rlsUser) {
		if _, mErr := invokeAs(t, h, "FollowUp/get", a.UserID,
			map[string]any{"accountId": b.UserID.String()}); mErr == nil || mErr.Type != errAccountNotFound {
			t.Fatalf("FollowUp/get naming B's account: got %+v, want accountNotFound", mErr)
		}
		bDue := seedFollowUpDue(t, h, b)
		res, mErr := invokeAs(t, h, "FollowUp/get", a.UserID, map[string]any{})
		if mErr != nil {
			t.Fatalf("FollowUp/get as A: %+v", mErr)
		}
		assertNoBLeak(t, "FollowUp/get", res, b)
		for _, item := range asArr(res["list"]) {
			if item.(map[string]any)["emailId"] == bDue.String() {
				t.Fatalf("A's follow-ups hold B's message")
			}
		}
		res, mErr = invokeAs(t, h, "FollowUp/get", b.UserID, map[string]any{})
		if mErr != nil {
			t.Fatalf("FollowUp/get as B: %+v", mErr)
		}
		found := false
		for _, item := range asArr(res["list"]) {
			found = found || item.(map[string]any)["emailId"] == bDue.String()
		}
		if !found {
			t.Fatalf("B's own nudge is not listed — the negative above proves nothing: %v", res)
		}
	},

	// #58: A's Done on B's message is notFound, exactly as a message that
	// never existed, and writes nothing for B.
	"FollowUp/set": func(t *testing.T, h *rlsHarness, a, b rlsUser) {
		if _, mErr := invokeAs(t, h, "FollowUp/set", a.UserID,
			map[string]any{"accountId": b.UserID.String(), "dismiss": map[string]any{}}); mErr == nil || mErr.Type != errAccountNotFound {
			t.Fatalf("FollowUp/set naming B's account: got %+v, want accountNotFound", mErr)
		}
		res, mErr := invokeAs(t, h, "FollowUp/set", a.UserID,
			map[string]any{"dismiss": map[string]any{b.MessageID.String(): "outbound"}})
		if mErr != nil {
			t.Fatalf("FollowUp/set: %+v", mErr)
		}
		nd, _ := res["notDismissed"].(map[string]any)
		if e, _ := nd[b.MessageID.String()].(map[string]any); e == nil || e["type"] != setNotFound {
			t.Fatalf("dismissing B's message: %v, want notFound", res)
		}
		var n int
		superScan(t, h, &n, `SELECT count(*) FROM app.nudge_dismissals WHERE message_id = $1`, b.MessageID.String())
		if n != 0 {
			t.Fatalf("A's dismissal wrote %d rows against B's message", n)
		}
	},

	// #53: the enumeration behind every clean-up action. A naming B's own
	// sender key gets none of B's message ids; B, the positive control, does.
	"SenderStats/emailIds": func(t *testing.T, h *rlsHarness, a, b rlsUser) {
		if _, mErr := invokeAs(t, h, "SenderStats/emailIds", a.UserID,
			map[string]any{"accountId": b.UserID.String(), "key": "x@example.com"}); mErr == nil || mErr.Type != errAccountNotFound {
			t.Fatalf("emailIds naming B's account: got %+v, want accountNotFound", mErr)
		}
		superExec(t, h, `UPDATE app.messages SET from_address = $2 WHERE id = $1`, b.MessageID.String(), b.ContactAddr)
		res, mErr := invokeAs(t, h, "SenderStats/emailIds", a.UserID, map[string]any{"key": strings.ToLower(b.ContactAddr)})
		if mErr != nil {
			t.Fatalf("emailIds as A: %+v", mErr)
		}
		assertNoBLeak(t, "SenderStats/emailIds", res, b)
		if len(asArr(res["ids"])) != 0 {
			t.Errorf("A enumerated ids for B's sender: %v", res)
		}
		res, mErr = invokeAs(t, h, "SenderStats/emailIds", b.UserID, map[string]any{"key": strings.ToLower(b.ContactAddr)})
		if mErr != nil {
			t.Fatalf("emailIds as B: %+v", mErr)
		}
		if !strInArr(res["ids"], b.MessageID.String()) {
			t.Errorf("B's own enumeration lacks B's message — the negative above proves nothing: %v", res)
		}
	},

	// #59: A cannot mark, block or unblock B's contact. The id is B's real
	// contact id, so the only thing standing between A and the write is RLS
	// (plus the store's own user_id predicate).
	"Contact/set": func(t *testing.T, h *rlsHarness, a, b rlsUser) {
		if _, mErr := invokeAs(t, h, "Contact/set", a.UserID,
			map[string]any{"accountId": b.UserID.String()}); mErr == nil || mErr.Type != errAccountNotFound {
			t.Fatalf("set naming B's account: got %+v, want accountNotFound", mErr)
		}
		superExec(t, h, `UPDATE app.contacts SET vip = false, blocked = false, blocked_at = NULL WHERE id = $1`,
			b.ContactID.String())
		res, mErr := invokeAs(t, h, "Contact/set", a.UserID, map[string]any{
			"update": map[string]any{b.ContactID.String(): map[string]any{"isBlocked": true, "isVip": true}},
		})
		if mErr != nil {
			t.Fatalf("Contact/set: %+v", mErr)
		}
		if !mapHasKey(res, "notUpdated", b.ContactID.String()) {
			t.Errorf("A's update of B's contact was not refused: %v", res)
		}
		var vip, blocked bool
		superScan(t, h, &vip, `SELECT vip FROM app.contacts WHERE id=$1`, b.ContactID.String())
		superScan(t, h, &blocked, `SELECT blocked FROM app.contacts WHERE id=$1`, b.ContactID.String())
		if vip || blocked {
			t.Errorf("A's Contact/set reached B's contact: vip=%v blocked=%v", vip, blocked)
		}
	},

	// #448/#374 increment 1: A's own keys list must show nothing of B's, and
	// naming B's key id must not resolve — the same CategoryTab/get shape
	// (a materialising whole-list read plus a by-id foreign lookup). B's
	// FINGERPRINT is in rlsUser.identifiers() too, so a leak of the value
	// (not just the row id) is caught.
	"UserKey/get": func(t *testing.T, h *rlsHarness, a, b rlsUser) {
		res, mErr := invokeAs(t, h, "UserKey/get", a.UserID, map[string]any{})
		if mErr != nil {
			t.Fatalf("UserKey/get: %+v", mErr)
		}
		assertNoBLeak(t, "UserKey/get", res, b)

		res, mErr = invokeAs(t, h, "UserKey/get", a.UserID, map[string]any{
			"ids": []string{b.UserKeyID.String()},
		})
		if mErr != nil {
			t.Fatalf("UserKey/get by id: %+v", mErr)
		}
		assertNoBLeakExcept(t, "UserKey/get by id", res, b, b.UserKeyID.String())
		if !strInArr(res["notFound"], b.UserKeyID.String()) {
			t.Errorf("UserKey/get by a foreign id did not report notFound: %v", res)
		}
	},

	// #47 (ADR-038): both users hold a contact for friend@example.com with a
	// (deliberately unusable) key and an encryption pin; A asking about that
	// address must see A's own state and nothing of B's. wkd:false keeps the
	// probe off the network.
	"RecipientKey/lookup": func(t *testing.T, h *rlsHarness, a, b rlsUser) {
		res, mErr := invokeAs(t, h, "RecipientKey/lookup", a.UserID, map[string]any{
			"addresses": []string{"friend@example.com"}, "wkd": false,
		})
		if mErr != nil {
			t.Fatalf("RecipientKey/lookup: %+v", mErr)
		}
		assertNoBLeak(t, "RecipientKey/lookup", res, b)
	},

	// #47 (ADR-039): A deciding about a key B holds, for B's correspondent address,
	// must be notFound and must not mark B's key refused. The negative control is
	// B making the same call successfully, so a refusal that came from a typo in
	// the probe cannot pass for isolation.
	"ContactKey/decide": func(t *testing.T, h *rlsHarness, a, b rlsUser) {
		fp := "DECIDEPROBE" + strings.ToUpper(strings.ReplaceAll(b.UserID.String(), "-", ""))
		superExec(t, h, `INSERT INTO app.contact_keys (id, user_id, contact_id, kind, fingerprint, public_key, source)
		                 VALUES ($1, $2, $3, 'pgp', $4, $5, 'autocrypt')`,
			store.NewULID().String(), b.UserID.String(), b.ContactID.String(), fp, []byte{0x09})
		args := map[string]any{"address": b.ContactAddr, "fingerprint": fp, "decision": "keep"}

		res, mErr := invokeAs(t, h, "ContactKey/decide", a.UserID, args)
		if mErr == nil {
			t.Fatalf("A's decision about B's key was accepted: %v", res)
		}
		var refused bool
		superScan(t, h, &refused, `SELECT refused FROM app.contact_keys WHERE fingerprint = $1`, fp)
		if refused {
			t.Errorf("A's ContactKey/decide reached B's key")
		}

		if _, mErr = invokeAs(t, h, "ContactKey/decide", b.UserID, args); mErr != nil {
			t.Fatalf("control: B's own decision was refused: %+v", mErr)
		}
		superScan(t, h, &refused, `SELECT refused FROM app.contact_keys WHERE fingerprint = $1`, fp)
		if !refused {
			t.Errorf("control: B's own decision did not take effect, so the probe above proved nothing")
		}
	},

	// #47 (ADR-038): A importing a key for B's correspondent address must not touch
	// B's contact, key rows, pin or change feed (contacts are per user, so A's
	// import lands in A's own contact). The control is B making the same import
	// and it taking effect, so a probe that failed for a typo proves nothing.
	"ContactKey/import": func(t *testing.T, h *rlsHarness, a, b rlsUser) {
		peer := newPeerKey(t, b.ContactAddr, nil)
		key := armoredPub(t, peer)
		args := map[string]any{"address": b.ContactAddr, "publicKey": key}
		// A's import is A's own data, but it names B's correspondent address in
		// its user ID; left in place it would make every LATER probe of A's
		// key reads (ContactKey/get's userIds, #812) report that string as a
		// leak. Remove it when this probe ends (probe order is map order).
		t.Cleanup(func() {
			superExec(t, h, `DELETE FROM app.contact_keys WHERE user_id = $1 AND fingerprint = $2`, a.UserID.String(), hexOf(peer))
		})
		count := func(user rlsUser) (keys, pins, changes int) {
			superScan(t, h, &keys, `SELECT count(*) FROM app.contact_keys WHERE user_id = $1 AND source = 'manual'`, user.UserID.String())
			superScan(t, h, &pins, `SELECT count(*) FROM app.contact_key_pins WHERE user_id = $1`, user.UserID.String())
			superScan(t, h, &changes, `SELECT count(*) FROM app.change_log WHERE user_id = $1 AND object_type = 'Contact'`, user.UserID.String())
			return
		}
		bk, bp, bc := count(b)

		if _, mErr := invokeAs(t, h, "ContactKey/import", a.UserID, args); mErr != nil {
			t.Fatalf("A's own import was refused: %+v", mErr)
		}
		if k, p, c := count(b); k != bk || p != bp || c != bc {
			t.Errorf("A's ContactKey/import reached B: keys %d->%d pins %d->%d contact changes %d->%d", bk, k, bp, p, bc, c)
		}
		var onB int
		superScan(t, h, &onB, `SELECT count(*) FROM app.contact_keys WHERE user_id = $1 AND contact_id = $2 AND source = 'manual'`,
			b.UserID.String(), b.ContactID.String())
		if onB != 0 {
			t.Errorf("A's import landed in B's contact")
		}

		if _, mErr := invokeAs(t, h, "ContactKey/import", b.UserID, args); mErr != nil {
			t.Fatalf("control: B's own import was refused: %+v", mErr)
		}
		superScan(t, h, &onB, `SELECT count(*) FROM app.contact_keys WHERE user_id = $1 AND contact_id = $2 AND source = 'manual'`,
			b.UserID.String(), b.ContactID.String())
		if onB != 1 {
			t.Errorf("control: B's own import did not land in B's contact, so the probe above proved nothing")
		}
	},

	// ---- changes: A's delta feed never carries B's objects -----------------
	"Email/changes":           probeChangesNoLeak("Email/changes"),
	"Mailbox/changes":         probeChangesNoLeak("Mailbox/changes"),
	"Thread/changes":          probeChangesNoLeak("Thread/changes"),
	"Label/changes":           probeChangesNoLeak("Label/changes"),
	"Rule/changes":            probeChangesNoLeak("Rule/changes"),
	"Identity/changes":        probeChangesNoLeak("Identity/changes"),
	"EmailSubmission/changes": probeChangesNoLeak("EmailSubmission/changes"),
	"Account/changes":         probeChangesNoLeak("Account/changes"),
	"Contact/changes":         probeChangesNoLeak("Contact/changes"),

	// Email/queryChanges is a cannotCalculateChanges fallback in 0.5. Even so, a
	// filter naming B's mailbox must not turn into an error that reflects B's data.
	"Email/queryChanges": func(t *testing.T, h *rlsHarness, a, b rlsUser) {
		res, mErr := invokeAs(t, h, "Email/queryChanges", a.UserID, map[string]any{
			"sinceQueryState": "s:0",
			"filter":          map[string]any{"inMailbox": b.InboxID.String()},
		})
		if mErr != nil {
			if mErr.Type != errCannotCalculateChanges {
				t.Errorf("Email/queryChanges: unexpected error %+v", mErr)
			}
			return
		}
		assertNoBLeak(t, "Email/queryChanges", res, b)
	},

	// ---- sets: A cannot mutate B's objects ---------------------------------
	"Email/set": func(t *testing.T, h *rlsHarness, a, b rlsUser) {
		res, mErr := invokeAs(t, h, "Email/set", a.UserID, map[string]any{
			"update":  map[string]any{b.MessageID.String(): map[string]any{"keywords/$seen": true}},
			"destroy": []string{b.MessageID.String()},
		})
		if mErr != nil {
			t.Fatalf("Email/set: %+v", mErr)
		}
		assertSetRefused(t, "Email/set", res, b.MessageID.String())
		// B's message is untouched: not seen, not trashed (read as its owner B).
		if seen, _, trashed := h.canonicalFlags(t, b.UserID, b.MessageID); seen || trashed {
			t.Errorf("A mutated B's message: seen=%v trashed=%v", seen, trashed)
		}
	},
	"Mailbox/set": func(t *testing.T, h *rlsHarness, a, b rlsUser) {
		// Mailbox mutations are notImplemented in 0.5; the point is only that
		// naming B's mailbox never mutates it and never confirms it exists as B's.
		res, mErr := invokeAs(t, h, "Mailbox/set", a.UserID, map[string]any{
			"update":  map[string]any{b.InboxID.String(): map[string]any{"name": rlsSentinel}},
			"destroy": []string{b.InboxID.String()},
		})
		if mErr != nil {
			t.Fatalf("Mailbox/set: %+v", mErr)
		}
		if !mapHasKey(res, "notUpdated", b.InboxID.String()) || !mapHasKey(res, "notDestroyed", b.InboxID.String()) {
			t.Errorf("Mailbox/set did not refuse B's mailbox: %v", res)
		}
		assertRowUnchanged(t, h, "mailboxes", "display_name", b.InboxID)
	},
	"Label/set": func(t *testing.T, h *rlsHarness, a, b rlsUser) {
		res, mErr := invokeAs(t, h, "Label/set", a.UserID, map[string]any{
			"update":  map[string]any{b.LabelID.String(): map[string]any{"name": rlsSentinel}},
			"destroy": []string{b.LabelID.String()},
		})
		if mErr != nil {
			t.Fatalf("Label/set: %+v", mErr)
		}
		assertSetRefused(t, "Label/set", res, b.LabelID.String())
		assertRowExists(t, h, "labels", b.LabelID)            // destroy did not reach it
		assertRowUnchanged(t, h, "labels", "name", b.LabelID) // update did not rename it
	},
	// #202. A read of the allow list must return only A's grants — and the leak to
	// look for is not merely B's row id, it is B's TRUSTED SENDER ADDRESS. "Whose
	// images does this person load" is a list of correspondents they chose to relax
	// a privacy default for, so it discloses a relationship as well as a setting.
	// RemoteAllowValue is in rlsUser.identifiers(), so the leak assertion covers it.
	"RemoteAllow/get": func(t *testing.T, h *rlsHarness, a, b rlsUser) {
		// The whole-list form: what the settings surface calls. Nothing of B's may
		// appear in it at all.
		res, mErr := invokeAs(t, h, "RemoteAllow/get", a.UserID, map[string]any{})
		if mErr != nil {
			t.Fatalf("RemoteAllow/get: %+v", mErr)
		}
		assertNoBLeak(t, "RemoteAllow/get", res, b)

		// The named-id form: the one that takes an id from the REQUEST, and so is
		// where an unscoped lookup would resolve a foreign row. The id itself is
		// echoed back in notFound (it is the caller's own input, not a disclosure),
		// so it is the one identifier excepted — B's VALUE still must not appear.
		res, mErr = invokeAs(t, h, "RemoteAllow/get", a.UserID, map[string]any{
			"ids": []string{b.RemoteAllowID.String()},
		})
		if mErr != nil {
			t.Fatalf("RemoteAllow/get by id: %+v", mErr)
		}
		assertNoBLeakExcept(t, "RemoteAllow/get by id", res, b, b.RemoteAllowID.String())
		// It must be REPORTED not-found rather than silently omitted, so a client can
		// tell "revoked" from "not yours".
		if !strInArr(res["notFound"], b.RemoteAllowID.String()) {
			t.Errorf("RemoteAllow/get by a foreign id did not report notFound: %v", res)
		}
		// ...and it must not have returned it.
		if list, ok := res["list"].([]any); ok && len(list) > 0 {
			t.Errorf("RemoteAllow/get returned a foreign grant: %v", list)
		}
	},

	"RemoteAllow/changes": probeChangesNoLeak("RemoteAllow/changes"),

	// #56. The leak that matters is B's snippet BODY — their own words, which is
	// closer to reading a draft than to reading a preference. SnippetName is in
	// rlsUser.identifiers() so the blunt check covers it.
	"ComposeSnippet/get": func(t *testing.T, h *rlsHarness, a, b rlsUser) {
		res, mErr := invokeAs(t, h, "ComposeSnippet/get", a.UserID, map[string]any{})
		if mErr != nil {
			t.Fatalf("ComposeSnippet/get: %+v", mErr)
		}
		assertNoBLeak(t, "ComposeSnippet/get", res, b)

		res, mErr = invokeAs(t, h, "ComposeSnippet/get", a.UserID, map[string]any{
			"ids": []string{b.SnippetID.String()},
		})
		if mErr != nil {
			t.Fatalf("ComposeSnippet/get by id: %+v", mErr)
		}
		assertNoBLeakExcept(t, "ComposeSnippet/get by id", res, b, b.SnippetID.String())
		if !strInArr(res["notFound"], b.SnippetID.String()) {
			t.Errorf("ComposeSnippet/get by a foreign id did not report notFound: %v", res)
		}
	},

	"ComposeSnippet/changes": probeChangesNoLeak("ComposeSnippet/changes"),

	// A destroy or an update naming B's snippet must not reach it. An update is the
	// sharper case: overwriting someone else's signature would put words in their
	// outgoing mail.
	"ComposeSnippet/set": func(t *testing.T, h *rlsHarness, a, b rlsUser) {
		res, mErr := invokeAs(t, h, "ComposeSnippet/set", a.UserID, map[string]any{
			"update": map[string]any{
				b.SnippetID.String(): map[string]any{
					"kind": "signature", "name": rlsSentinel, "bodyText": rlsSentinel,
				},
			},
			"destroy": []string{b.SnippetID.String()},
		})
		if mErr != nil {
			t.Fatalf("ComposeSnippet/set: %+v", mErr)
		}
		assertSetRefused(t, "ComposeSnippet/set", res, b.SnippetID.String())
		assertRowExists(t, h, "compose_snippets", b.SnippetID)
		assertRowUnchanged(t, h, "compose_snippets", "name", b.SnippetID)
	},

	// #57. THE QUERY IS THE LEAK, not the folder. A saved search says what B goes
	// looking for in their own mail — which correspondent, which worry — and that is
	// more revealing than the mail it would return. SavedQuery and SavedName are both
	// in rlsUser.identifiers() so the blunt check covers them.
	"SavedSearch/get": func(t *testing.T, h *rlsHarness, a, b rlsUser) {
		res, mErr := invokeAs(t, h, "SavedSearch/get", a.UserID, map[string]any{})
		if mErr != nil {
			t.Fatalf("SavedSearch/get: %+v", mErr)
		}
		assertNoBLeak(t, "SavedSearch/get", res, b)

		res, mErr = invokeAs(t, h, "SavedSearch/get", a.UserID, map[string]any{
			"ids": []string{b.SavedID.String()},
		})
		if mErr != nil {
			t.Fatalf("SavedSearch/get by id: %+v", mErr)
		}
		assertNoBLeakExcept(t, "SavedSearch/get by id", res, b, b.SavedID.String())
		if !strInArr(res["notFound"], b.SavedID.String()) {
			t.Errorf("SavedSearch/get by a foreign id did not report notFound: %v", res)
		}
	},

	"SavedSearch/changes": probeChangesNoLeak("SavedSearch/changes"),

	// An update naming B's saved search is the sharp case: REWRITING SOMEONE ELSE'S
	// QUERY would silently change what their smart folder shows them, which is worse
	// than reading it — they would keep trusting a folder that had been altered.
	"SavedSearch/set": func(t *testing.T, h *rlsHarness, a, b rlsUser) {
		res, mErr := invokeAs(t, h, "SavedSearch/set", a.UserID, map[string]any{
			"update": map[string]any{
				b.SavedID.String(): map[string]any{
					"name": rlsSentinel, "query": rlsSentinel, "inSidebar": false,
				},
			},
			"destroy": []string{b.SavedID.String()},
		})
		if mErr != nil {
			t.Fatalf("SavedSearch/set: %+v", mErr)
		}
		assertSetRefused(t, "SavedSearch/set", res, b.SavedID.String())
		assertRowExists(t, h, "saved_searches", b.SavedID)
		assertRowUnchanged(t, h, "saved_searches", "query", b.SavedID)
	},

	// #219. A read must show A their OWN five tabs and nothing of B's. Note the
	// shape peculiar to this object: A's read MATERIALISES A's own rows, so a
	// broken isolation would not be an empty list, it would be a list with a sixth
	// entry — B's — mixed into A's. The blunt id check catches exactly that.
	"CategoryTab/get": func(t *testing.T, h *rlsHarness, a, b rlsUser) {
		res, mErr := invokeAs(t, h, "CategoryTab/get", a.UserID, map[string]any{})
		if mErr != nil {
			t.Fatalf("CategoryTab/get: %+v", mErr)
		}
		assertNoBLeak(t, "CategoryTab/get", res, b)

		res, mErr = invokeAs(t, h, "CategoryTab/get", a.UserID, map[string]any{
			"ids": []string{b.CategoryTabID.String()},
		})
		if mErr != nil {
			t.Fatalf("CategoryTab/get by id: %+v", mErr)
		}
		assertNoBLeakExcept(t, "CategoryTab/get by id", res, b, b.CategoryTabID.String())
		if !strInArr(res["notFound"], b.CategoryTabID.String()) {
			t.Errorf("CategoryTab/get by a foreign id did not report notFound: %v", res)
		}
	},

	"CategoryTab/changes": probeChangesNoLeak("CategoryTab/changes"),

	// The write side is the one that matters here. Re-enabling and re-ordering B's
	// tab would MOVE A CLASS OF THEIR MAIL — everything mapped to that tab leaves
	// Primary — without touching one message, so nothing in B's client would
	// explain it. The assertion is on the row's own values rather than the sentinel
	// helper, which only knows how to check text columns.
	"CategoryTab/set": func(t *testing.T, h *rlsHarness, a, b rlsUser) {
		res, mErr := invokeAs(t, h, "CategoryTab/set", a.UserID, map[string]any{
			"update": map[string]any{
				b.CategoryTabID.String(): map[string]any{"enabled": true, "position": 0},
			},
			"destroy": []string{b.CategoryTabID.String()},
		})
		if mErr != nil {
			t.Fatalf("CategoryTab/set: %+v", mErr)
		}
		assertSetRefused(t, "CategoryTab/set", res, b.CategoryTabID.String())
		assertRowExists(t, h, "category_tabs", b.CategoryTabID)
		var enabled bool
		var position int
		superScan(t, h, &enabled, `SELECT enabled FROM app.category_tabs WHERE id=$1`,
			b.CategoryTabID.String())
		superScan(t, h, &position, `SELECT position FROM app.category_tabs WHERE id=$1`,
			b.CategoryTabID.String())
		if enabled || position != 7 {
			t.Errorf("A mutated B's category tab: enabled=%v position=%d (want false, 7)",
				enabled, position)
		}
	},

	// A destroy naming B's grant must not revoke it. This one matters more than it
	// looks: silently revoking someone else's allow would not lose data, it would
	// change what their mail client fetches from the network.
	// ADR-026 §3c. The method records "the user asked for THIS message", which
	// the image proxy then honours — so noting a request against ANOTHER user's
	// message would be a way to make the server fetch from mail A cannot see.
	// It must refuse, and it must refuse the same way an id that never existed
	// is refused.
	"RemoteContent/request": func(t *testing.T, h *rlsHarness, a, b rlsUser) {
		_, mErr := invokeAs(t, h, "RemoteContent/request", a.UserID, map[string]any{
			"messageId": b.MessageID.String(),
		})
		if mErr == nil {
			t.Fatalf("RemoteContent/request accepted B's message id from A — the proxy " +
				"would then fetch remote content out of a message A cannot read")
		}
		// CONTROL: A's OWN message is accepted, or this passes for a method that
		// refuses everything and the isolation claim is vacuous.
		if _, ownErr := invokeAs(t, h, "RemoteContent/request", a.UserID, map[string]any{
			"messageId": a.MessageID.String(),
		}); ownErr != nil {
			t.Fatalf("RemoteContent/request refused A's OWN message (%+v) — this probe "+
				"cannot tell isolation from a method that never works", ownErr)
		}
	},
	"RemoteAllow/set": func(t *testing.T, h *rlsHarness, a, b rlsUser) {
		res, mErr := invokeAs(t, h, "RemoteAllow/set", a.UserID, map[string]any{
			"destroy": []string{b.RemoteAllowID.String()},
		})
		if mErr != nil {
			t.Fatalf("RemoteAllow/set: %+v", mErr)
		}
		// Only notDestroyed applies here: this object has no update form at all, so
		// assertSetRefused's notUpdated check would look for something the method never
		// produces. The row surviving is the assertion that matters.
		if !mapHasKey(res, "notDestroyed", b.RemoteAllowID.String()) {
			t.Errorf("RemoteAllow/set: B's id not in notDestroyed: %v", res["notDestroyed"])
		}
		if strInArr(res["destroyed"], b.RemoteAllowID.String()) {
			t.Errorf("RemoteAllow/set: A revoked B's grant")
		}
		assertRowExists(t, h, "remote_allows", b.RemoteAllowID)
	},

	"Rule/set": func(t *testing.T, h *rlsHarness, a, b rlsUser) {
		res, mErr := invokeAs(t, h, "Rule/set", a.UserID, map[string]any{
			"update":  map[string]any{b.RuleID.String(): map[string]any{"name": rlsSentinel}},
			"destroy": []string{b.RuleID.String()},
		})
		if mErr != nil {
			t.Fatalf("Rule/set: %+v", mErr)
		}
		assertSetRefused(t, "Rule/set", res, b.RuleID.String())
		assertRowExists(t, h, "rules", b.RuleID)            // destroy did not reach it
		assertRowUnchanged(t, h, "rules", "name", b.RuleID) // update did not rename it
	},

	// Rule/preview (#155): the blast-radius scan. This method's whole job is to
	// COUNT AND SAMPLE a user's messages, so a leak here is not an id appearing
	// in a payload — it is A being told how much of B's mail a rule would match,
	// and shown B's subjects as examples. That makes it the highest-risk read in
	// the rules surface.
	//
	// The condition is deliberately one that matches BOTH users' seeded messages
	// on content — they carry the same subject — so RLS is the ONLY thing holding
	// the answer down to one. A probe using a value unique to B would report zero
	// even if isolation were broken, because nothing of A's matches it either.
	//
	// `total` is asserted too, not just `matched`: it is a separate COUNT(*) over
	// app.messages, so it is its own leak surface, and it is the number the client
	// renders as the denominator ("37 of 1,204").
	"Rule/preview": func(t *testing.T, h *rlsHarness, a, b rlsUser) {
		res, mErr := invokeAs(t, h, "Rule/preview", a.UserID, map[string]any{
			"conditions": map[string]any{
				"field": "subject", "op": "contains", "value": "seed subject",
			},
		})
		if mErr != nil {
			t.Fatalf("Rule/preview: %+v", mErr)
		}
		// Exactly A's own copy. 2 would mean the scan crossed into B's mailbox;
		// 0 would mean the scan is broken and this probe proves nothing.
		if n, _ := res["matched"].(float64); n != 1 {
			t.Errorf("Rule/preview matched %v, want exactly 1 (A's own message; both users' seeds share this subject)", res["matched"])
		}
		if n, _ := res["total"].(float64); n != 1 {
			t.Errorf("Rule/preview total = %v, want 1 — the denominator counted beyond A's own mail", res["total"])
		}
		assertNoBLeak(t, "Rule/preview", res, b)
	},

	// Rule/induce (#168): learns a rule from labelled mail. The leak here is
	// subtler than a payload one — B's mail becoming EVIDENCE would let A be
	// handed a rule derived from a stranger's correspondence, and the rule's own
	// conditions (a sender, a domain, a List-Id) would carry that stranger's data
	// out in a form no id-scan would recognise.
	//
	// Both users' seeded messages share a subject and sender, and both have a
	// hand-applied label. A asks about ITS label: if RLS leaked, B's copy would be
	// a second example and the scan window would be 2.
	"Rule/induce": func(t *testing.T, h *rlsHarness, a, b rlsUser) {
		res, mErr := invokeAs(t, h, "Rule/induce", a.UserID, map[string]any{
			"labelId": a.LabelID.String(),
		})
		if mErr != nil {
			t.Fatalf("Rule/induce: %+v", mErr)
		}
		if n, _ := res["examples"].(float64); n != 1 {
			t.Errorf("Rule/induce found %v examples, want 1 — A's own labelled message only", res["examples"])
		}
		if n, _ := res["scanned"].(float64); n != 1 {
			t.Errorf("Rule/induce scanned %v messages, want 1 — the evidence window crossed into B's mail", res["scanned"])
		}
		if n, _ := res["total"].(float64); n != 1 {
			t.Errorf("Rule/induce total = %v, want 1", res["total"])
		}
		assertNoBLeak(t, "Rule/induce", res, b)

		// Asking about B's LABEL must teach A nothing: not "that label has
		// examples", not a rule shaped like B's mail.
		other, mErr := invokeAs(t, h, "Rule/induce", a.UserID, map[string]any{
			"labelId": b.LabelID.String(),
		})
		if mErr != nil {
			t.Fatalf("Rule/induce (B's label): %+v", mErr)
		}
		if n, _ := other["examples"].(float64); n != 0 {
			t.Errorf("Rule/induce on B's label found %v examples for A", other["examples"])
		}
		if cs, _ := other["candidates"].([]any); len(cs) != 0 {
			t.Errorf("Rule/induce proposed %d rules from another user's label", len(cs))
		}
		assertNoBLeak(t, "Rule/induce (B's label)", other, b)
	},

	// Rule/tighten (#170): the narrowing proposal. It takes a CALLER-SUPPLIED
	// RULE ID and then scans mail, which makes it two leak surfaces in one — A
	// naming B's rule, and the scan behind it crossing into B's mailbox.
	//
	// Both users own an identically-shaped rule with exactly one correction
	// against it, so a leak shows up as a 2 where a 1 belongs rather than as an
	// id in a payload.
	"Rule/tighten": func(t *testing.T, h *rlsHarness, a, b rlsUser) {
		res, mErr := invokeAs(t, h, "Rule/tighten", a.UserID, map[string]any{
			"ruleId": a.RuleID.String(),
		})
		if mErr != nil {
			t.Fatalf("Rule/tighten: %+v", mErr)
		}
		if n, _ := res["corrections"].(float64); n != 1 {
			t.Errorf("Rule/tighten reported %v corrections, want 1 — B's correction was counted against A's rule", res["corrections"])
		}
		if n, _ := res["scanned"].(float64); n != 1 {
			t.Errorf("Rule/tighten scanned %v messages, want 1 — the proposal window crossed into B's mail", res["scanned"])
		}
		if n, _ := res["total"].(float64); n != 1 {
			t.Errorf("Rule/tighten total = %v, want 1", res["total"])
		}
		assertNoBLeak(t, "Rule/tighten", res, b)

		// B's rule id must NOT resolve. "No such rule" is the required answer,
		// not a clean report of zero corrections: answering at all would confirm
		// the id exists and hand A a measurement of a stranger's automation.
		other, mErr := invokeAs(t, h, "Rule/tighten", a.UserID, map[string]any{
			"ruleId": b.RuleID.String(),
		})
		if mErr == nil {
			t.Fatalf("Rule/tighten answered for another user's rule: %v", other)
		}
	},

	// Rule/apply (#191): the sharpest method in the rules surface. It takes a
	// caller-supplied rule id AND WRITES — a leak here does not merely tell A
	// about B's mail, it LABELS it, appending change_log rows another user's
	// clients will sync and IMAP writebacks that reach their real mailbox.
	//
	// Both users own a rule matching a domain, and both have a seeded message.
	// A applies its OWN rule (which must touch only A's mail), then tries B's.
	"Rule/apply": func(t *testing.T, h *rlsHarness, a, b rlsUser) {
		res, mErr := invokeAs(t, h, "Rule/apply", a.UserID, map[string]any{
			"ruleId": a.RuleID.String(),
		})
		if mErr != nil {
			t.Fatalf("Rule/apply: %+v", mErr)
		}
		if n, _ := res["scanned"].(float64); n != 1 {
			t.Errorf("Rule/apply scanned %v messages, want 1 — the backfill crossed into B's mailbox", res["scanned"])
		}
		if n, _ := res["total"].(float64); n != 1 {
			t.Errorf("Rule/apply total = %v, want 1", res["total"])
		}
		assertNoBLeak(t, "Rule/apply", res, b)

		// B's rule must not resolve — and, far more importantly, must not have
		// run. A rule id is a caller-supplied string; if isolation leaked, A would
		// have just relabelled a stranger's mail.
		before := labelCount(t, h, b)
		if _, mErr := invokeAs(t, h, "Rule/apply", a.UserID, map[string]any{
			"ruleId": b.RuleID.String(),
		}); mErr == nil {
			t.Error("Rule/apply accepted another user's rule id")
		}
		if after := labelCount(t, h, b); after != before {
			t.Errorf("A's Rule/apply changed B's labels: %d -> %d", before, after)
		}

		// And the undo, which is also id-driven: A naming B's message must not
		// strip labels off it.
		bLabels := labelCount(t, h, b)
		if _, mErr := invokeAs(t, h, "Rule/apply", a.UserID, map[string]any{
			"ruleId": a.RuleID.String(), "undo": true,
			"messageIds": []string{b.MessageID.String()},
		}); mErr != nil {
			t.Fatalf("Rule/apply undo: %+v", mErr)
		}
		if after := labelCount(t, h, b); after != bLabels {
			t.Errorf("A's undo removed labels from B's mail: %d -> %d", bLabels, after)
		}
	},

	// Email/replyToInvite (#725): A names B's message. A leak here would not
	// disclose data — it would SEND MAIL: an answer to B's meeting, or a reply
	// from A's own address carrying B's event. The method must not resolve B's
	// message at all, and nothing may be written for either user: no draft, no
	// submission, no recorded answer.
	"Email/replyToInvite": func(t *testing.T, h *rlsHarness, a, b rlsUser) {
		count := func(sql string) int {
			var n int
			superScan(t, h, &n, sql)
			return n
		}
		subs := count(`SELECT count(*) FROM app.submissions`)
		drafts := count(`SELECT count(*) FROM app.messages WHERE is_draft`)
		answers := count(`SELECT count(*) FROM app.calendar_responses`)
		if _, mErr := invokeAs(t, h, "Email/replyToInvite", a.UserID, map[string]any{
			"emailId": b.MessageID.String(), "partstat": "accepted",
		}); mErr == nil {
			t.Errorf("Email/replyToInvite succeeded against B's message — it must not resolve at all")
		} else if !strings.Contains(mErr.Description, "no such email") {
			// The refusal must be the RESOLUTION failing, not a later check
			// that happened to stop it: "no such email" is the only answer that
			// proves A's identity could not see the message.
			t.Errorf("refused for the wrong reason: %+v", mErr)
		}
		if count(`SELECT count(*) FROM app.submissions`) != subs ||
			count(`SELECT count(*) FROM app.messages WHERE is_draft`) != drafts ||
			count(`SELECT count(*) FROM app.calendar_responses`) != answers {
			t.Errorf("a refused cross-user reply wrote a draft, a submission or an answer")
		}
	},

	// Email/releaseAttachment (#396): A tries to lift the quarantine on B's
	// flagged attachment.
	//
	// ⚠ THIS IS THE SHARPEST OF THESE PROBES, because a leak here does not
	// disclose data — it DISARMS A SECURITY CONTROL on another user's mailbox.
	// A release is keyed on a CONTENT DIGEST rather than on a row, so the
	// failure mode to exclude is specific: A releasing content that B also holds
	// must not open B's gate. RLS gives each user their own attachment_releases
	// rows, and this proves the store honours that rather than keying the gate
	// on a digest alone.
	//
	// ⚠ THE FIRST VERSION OF THIS PROBE DID NOT TEST THAT, AND THE COMMENT WAS
	// THE WORSE HALF (#416). It seeded a flagged row for B only, then had A call
	// the method naming B's message id. A's call failed at ID RESOLUTION —
	// AttachmentScanState under A's identity finds no such message — so the probe
	// proved "A cannot name B's message id" and said nothing about the
	// digest-keyed failure the comment describes. ⚠ A NEVER HELD THE DIGEST.
	// An implementation that keyed the gate on part_hash alone, with no user_id
	// predicate, would have passed it unchanged.
	//
	// So there are now two arrangements, and the second is the one that matters:
	//
	//  1. A names B's message id — must fail at resolution (kept: still worth
	//     having, and it is the cheaper of the two failures to regress).
	//  2. BOTH users hold a row carrying the SAME digest, and A releases it
	//     LEGITIMATELY in A's own mailbox — a call that must SUCCEED. B's part
	//     must still be refused afterwards. ⚠ Note this requires a success,
	//     which the all-must-fail shape of these probes could not express, and
	//     is probably why it was written the other way.
	"Email/releaseAttachment": func(t *testing.T, h *rlsHarness, a, b rlsUser) {
		ctx := context.Background()
		hash := sha256.Sum256([]byte("shared-attachment-bytes"))
		seedFlagged := func(u rlsUser) {
			t.Helper()
			if _, err := h.super.Exec(ctx, `
				INSERT INTO app.attachments
					(id, user_id, message_id, part_path, content_type, size_bytes,
					 scan_status, part_hash)
				VALUES ($1, $2, $3, '2', 'application/zip', 10, 'flagged', $4)
				ON CONFLICT (message_id, part_path) DO UPDATE
				   SET scan_status = 'flagged', part_hash = EXCLUDED.part_hash`,
				store.NewULID().String(), u.UserID.String(), u.MessageID.String(), hash[:]); err != nil {
				t.Fatalf("seed flagged attachment: %v", err)
			}
		}
		// ⚠ THE SAME BYTES IN BOTH MAILBOXES. That is the whole arrangement:
		// two users legitimately holding one newsletter attachment is ordinary,
		// and it is exactly when a digest-keyed release with no user predicate
		// would leak.
		seedFlagged(a)
		seedFlagged(b)

		// ── 1. A cannot name B's message id. ────────────────────────────────
		if _, mErr := invokeAs(t, h, "Email/releaseAttachment", a.UserID, map[string]any{
			"id": b.MessageID.String(), "partId": "2",
		}); mErr == nil {
			t.Errorf("Email/releaseAttachment succeeded against B's message — it must not resolve at all")
		}

		// ── 2. A releases the SAME CONTENT legitimately, in A's own mailbox. ──
		// This must succeed, or everything below passes for the wrong reason.
		if _, mErr := invokeAs(t, h, "Email/releaseAttachment", a.UserID, map[string]any{
			"id": a.MessageID.String(), "partId": "2",
		}); mErr != nil {
			t.Fatalf("CONTROL FAILED: A could not release content in A's OWN mailbox (%v) — without a "+
				"successful release there is no release row, and the cross-user assertion below "+
				"cannot distinguish isolation from inaction", mErr)
		}
		var aRows, bRows int
		superScan(t, h, &aRows,
			`SELECT count(*) FROM app.attachment_releases WHERE part_hash=$1 AND user_id=$2`,
			hash[:], a.UserID.String())
		superScan(t, h, &bRows,
			`SELECT count(*) FROM app.attachment_releases WHERE part_hash=$1 AND user_id=$2`,
			hash[:], b.UserID.String())
		if aRows != 1 {
			t.Fatalf("CONTROL FAILED: A's own release wrote %d rows, want 1", aRows)
		}
		if bRows != 0 {
			t.Errorf("A's release of shared content wrote %d release row(s) for B — a release is keyed "+
				"on a CONTENT DIGEST, so a missing user_id predicate disarms the quarantine on every "+
				"other mailbox holding the same bytes", bRows)
		}

		// ⚠ AND THE GATE ITSELF, NOT JUST THE ROW COUNT. attachscan.Blocked is
		// what the download route calls; asserting on it is what makes this a
		// statement about whether B's bytes are still refused rather than about
		// a table.
		//
		// ⚠ A'S OWN GATE IS CHECKED FIRST, AND IT IS THE CONTROL THAT MAKES THE
		// ASSERTION BELOW MEAN SOMETHING. #416 proposed probing this by deleting
		// the `r.user_id = a.user_id` predicate from the release lookup and
		// confirming the test goes red. It does NOT — and for a good reason
		// rather than a bad one: app.attachment_releases carries FORCE ROW LEVEL
		// SECURITY with a policy on both USING and WITH CHECK, so inside a
		// UserTx the subquery can only ever see the caller's own rows and an
		// INSERT can only ever write them. That predicate is defence in depth;
		// RLS is the control. The cross-user leak is therefore not reachable by
		// mutating this SQL at all.
		//
		// So the negative control has to come from the other direction: prove
		// the assertion can SEE a release when there is one. A's part must read
		// as released and unblocked here. If it does not, the check below is
		// blind and its "still blocked" verdict is worthless.
		if err := h.store.UserTx(ctx, a.UserID, func(tx *store.UserTx) error {
			st, found, err := tx.AttachmentScanState(ctx, a.MessageID, "2")
			if err != nil {
				return err
			}
			if !found {
				t.Fatalf("CONTROL FAILED: A has no projected part")
			}
			if !st.Released || attachscan.Blocked(st.Status, st.Released) {
				t.Fatalf("CONTROL FAILED: A's own part is not released/unblocked after A released it "+
					"(released %v, status %q) — this assertion cannot observe a release at all, so "+
					"'B is still blocked' below proves nothing", st.Released, st.Status)
			}
			return nil
		}); err != nil {
			t.Fatalf("read A's gate state: %v", err)
		}
		if err := h.store.UserTx(ctx, b.UserID, func(tx *store.UserTx) error {
			st, found, err := tx.AttachmentScanState(ctx, b.MessageID, "2")
			if err != nil {
				return err
			}
			if !found {
				t.Fatalf("CONTROL FAILED: B has no projected part to be refused")
			}
			if st.Released {
				t.Errorf("B's part reads as RELEASED after A released the same content in A's mailbox")
			}
			if !attachscan.Blocked(st.Status, st.Released) {
				t.Errorf("B's part is no longer blocked (status %q, released %v) after A's release of "+
					"the same digest — the gate has been disarmed across mailboxes",
					st.Status, st.Released)
			}
			return nil
		}); err != nil {
			t.Fatalf("read B's gate state: %v", err)
		}
	},

	// Email/dismissCategory (#176): A tries to close the open questions on B's
	// message. This is a DELETE driven by a caller-supplied id, so a leak here is
	// destructive rather than merely disclosing — A would silently wipe another
	// user's review queue, and nothing in B's client would explain where it went.
	//
	// B is seeded with an open suggestion, so the probe can distinguish "RLS held"
	// from "there was nothing to delete anyway".
	"Email/dismissCategory": func(t *testing.T, h *rlsHarness, a, b rlsUser) {
		if _, err := h.super.Exec(context.Background(), `
			INSERT INTO app.label_suggestions (user_id, message_id, label_id, confidence)
			VALUES ($1, $2, $3, 0.5)
			ON CONFLICT (user_id, message_id, label_id) DO NOTHING`,
			b.UserID.String(), b.MessageID.String(), b.LabelID.String()); err != nil {
			t.Fatalf("seed B's suggestion: %v", err)
		}
		res, mErr := invokeAs(t, h, "Email/dismissCategory", a.UserID, map[string]any{
			"ids": []string{b.MessageID.String()},
		})
		if mErr != nil {
			t.Fatalf("Email/dismissCategory: %+v", mErr)
		}
		if strInArr(res["dismissed"], b.MessageID.String()) {
			t.Errorf("Email/dismissCategory claimed to dismiss B's message %q", b.MessageID.String())
		}
		if !strInArr(res["notFound"], b.MessageID.String()) {
			t.Errorf("Email/dismissCategory did not report B's message as notFound: %v", res)
		}
		var n int
		superScan(t, h, &n,
			`SELECT count(*) FROM app.label_suggestions WHERE user_id=$1 AND message_id=$2`,
			b.UserID.String(), b.MessageID.String())
		if n != 1 {
			t.Errorf("Email/dismissCategory deleted B's suggestion: %d rows left, want 1", n)
		}
		// And no change_log row invented against B's message either — a spurious
		// Email/updated would make B's client re-fetch on a change that never was.
		superScan(t, h, &n,
			`SELECT count(*) FROM app.change_log WHERE user_id=$1 AND object_id=$2 AND op='updated'`,
			b.UserID.String(), b.MessageID.String())
		if n != 0 {
			t.Errorf("Email/dismissCategory appended %d change rows against B's message, want 0", n)
		}
		// And no tombstone written against B's message. This is the sharper half:
		// a dismissal row is written under the CALLER's user_id, so a leak here
		// would not be a foreign-row edit that RLS catches — it would be A
		// silently creating a row about a message A cannot see, which is only
		// prevented by the existence check refusing the id in the first place.
		superScan(t, h, &n,
			`SELECT count(*) FROM app.category_dismissals WHERE message_id=$1`,
			b.MessageID.String())
		if n != 0 {
			t.Errorf("Email/dismissCategory wrote %d dismissal rows for B's message, want 0", n)
		}
		// B's id is echoed in notFound because A supplied it — not a leak. Every
		// other identifier of B's must still be absent.
		assertNoBLeakExcept(t, "Email/dismissCategory", res, b, b.MessageID.String())
	},

	// ---- Triage controls (#34): the per-user priority model ----------------
	// Triage/get: A's model state describes only A's model. Both users are seeded
	// with exactly ONE active version (v1); if RLS leaked, A's ListVersions would
	// return B's row too, so A would see >1 version or B's model row id.
	"Triage/get": func(t *testing.T, h *rlsHarness, a, b rlsUser) {
		res, mErr := invokeAs(t, h, "Triage/get", a.UserID, map[string]any{})
		if mErr != nil {
			t.Fatalf("Triage/get: %+v", mErr)
		}
		if av, _ := res["activeVersion"].(float64); av != 1 {
			t.Errorf("Triage/get activeVersion = %v, want 1 (A's own seeded version)", res["activeVersion"])
		}
		if vs := asArr(res["versions"]); len(vs) != 1 {
			t.Errorf("Triage/get returned %d versions, want 1 — a leak would surface B's model too", len(vs))
		}
		assertNoBLeak(t, "Triage/get", res, b)
	},
	// Triage/set: A rolls its OWN model back to v1 — a model-control mutation that
	// runs ActivateModelVersion's deactivate+activate UPDATEs under A's RLS. B's
	// active ml_models row must be entirely untouched (a deactivate that leaked A's
	// user_id predicate would clear B's active flag). rollbackTo:1 (A's seeded
	// active version) is deliberately state-preserving on A, so this probe does not
	// disturb the shared A/B fixture the other probes read.
	"Triage/set": func(t *testing.T, h *rlsHarness, a, b rlsUser) {
		if _, mErr := invokeAs(t, h, "Triage/set", a.UserID, map[string]any{"rollbackTo": 1}); mErr != nil {
			t.Fatalf("Triage/set rollbackTo 1: %+v", mErr)
		}
		assertRowExists(t, h, "ml_models", b.ModelVersionID) // A's op did not delete B's row
		var n int
		superScan(t, h, &n, `SELECT count(*) FROM app.ml_models WHERE user_id=$1`, b.UserID.String())
		if n != 1 {
			t.Errorf("A's rollback changed B's model row count: %d, want 1", n)
		}
		var activeV1 int
		superScan(t, h, &activeV1,
			`SELECT count(*) FROM app.ml_models WHERE user_id=$1 AND active AND version=1`, b.UserID.String())
		if activeV1 != 1 {
			t.Errorf("A's rollback disturbed B's active model: %d active v1 rows, want 1", activeV1)
		}
	},
	// Triage/correct: THE load-bearing probe. A submits a correction for B's message
	// id. B's message is invisible to A under RLS, so it is recorded notCorrected and
	// NO feedback_events row is written for it.
	"Triage/correct": func(t *testing.T, h *rlsHarness, a, b rlsUser) {
		res, mErr := invokeAs(t, h, "Triage/correct", a.UserID, map[string]any{
			"corrections": map[string]any{b.MessageID.String(): "important"},
		})
		if mErr != nil {
			t.Fatalf("Triage/correct: %+v", mErr)
		}
		if !mapHasKey(res, "notCorrected", b.MessageID.String()) {
			t.Errorf("Triage/correct did not refuse B's message: %v", res)
		}
		if strInArr(res["corrected"], b.MessageID.String()) {
			t.Errorf("Triage/correct claimed to correct B's message %q", b.MessageID.String())
		}
		var n int
		superScan(t, h, &n, `SELECT count(*) FROM app.feedback_events WHERE message_id=$1`, b.MessageID.String())
		if n != 0 {
			t.Errorf("Triage/correct emitted feedback for B's message: count=%d, want 0", n)
		}
		// B's id is echoed back in notCorrected (A supplied it — not a leak); every
		// OTHER identifier of B's must still be absent.
		assertNoBLeakExcept(t, "Triage/correct", res, b, b.MessageID.String())
	},
	// Triage/open (#842): A names B's message as opened. Invisible under RLS, so
	// it is notOpened and NO feedback row is written for it.
	"Triage/open": func(t *testing.T, h *rlsHarness, a, b rlsUser) {
		res, mErr := invokeAs(t, h, "Triage/open", a.UserID, map[string]any{
			"emailIds": []string{b.MessageID.String()},
		})
		if mErr != nil {
			t.Fatalf("Triage/open: %+v", mErr)
		}
		if !mapHasKey(res, "notOpened", b.MessageID.String()) {
			t.Errorf("Triage/open did not refuse B's message: %v", res)
		}
		if strInArr(res["opened"], b.MessageID.String()) {
			t.Errorf("Triage/open claimed to record an open on B's message %q", b.MessageID.String())
		}
		var n int
		superScan(t, h, &n, `SELECT count(*) FROM app.feedback_events WHERE message_id=$1`, b.MessageID.String())
		if n != 0 {
			t.Errorf("Triage/open emitted feedback for B's message: count=%d, want 0", n)
		}
		assertNoBLeakExcept(t, "Triage/open", res, b, b.MessageID.String())
	},
	// A cannot rename or destroy B's mail ACCOUNT — a leak here would expose
	// another user's mail-server credentials. The probe drives update+destroy
	// only (no create: create dials IMAP and seals, which the RLS harness does
	// not wire — the credential-sealing + write-only-password + create isolation
	// are proven end-to-end in TestAccountSetCreateSealsAndHidesPassword). Renaming
	// B's account is metadata-only (no dial), so it needs no dialer here.
	"Account/set": func(t *testing.T, h *rlsHarness, a, b rlsUser) {
		res, mErr := invokeAs(t, h, "Account/set", a.UserID, map[string]any{
			"update":  map[string]any{b.AccountID.String(): map[string]any{"name": rlsSentinel}},
			"destroy": []string{b.AccountID.String()},
		})
		if mErr != nil {
			t.Fatalf("Account/set: %+v", mErr)
		}
		assertSetRefused(t, "Account/set", res, b.AccountID.String())
		assertRowExists(t, h, "accounts", b.AccountID)            // destroy did not reach it
		assertRowUnchanged(t, h, "accounts", "name", b.AccountID) // update did not rename it
		// And B's mail survives: destroy must not have cascaded B's message away.
		var n int
		superScan(t, h, &n, `SELECT count(*) FROM app.messages WHERE id=$1`, b.MessageID.String())
		if n != 1 {
			t.Errorf("Account/set destroy reached B's message: count=%d, want 1", n)
		}
	},
	"Identity/set": func(t *testing.T, h *rlsHarness, a, b rlsUser) {
		res, mErr := invokeAs(t, h, "Identity/set", a.UserID, map[string]any{
			"update":  map[string]any{b.IdentityID.String(): map[string]any{"name": rlsSentinel}},
			"destroy": []string{b.IdentityID.String()},
		})
		if mErr != nil {
			t.Fatalf("Identity/set: %+v", mErr)
		}
		assertSetRefused(t, "Identity/set", res, b.IdentityID.String())
		assertRowExists(t, h, "identities", b.IdentityID)
		assertRowUnchanged(t, h, "identities", "display_name", b.IdentityID)
	},
	"EmailSubmission/set": func(t *testing.T, h *rlsHarness, a, b rlsUser) {
		res, mErr := invokeAs(t, h, "EmailSubmission/set", a.UserID, map[string]any{
			// Try to cancel B's send, destroy B's record, and — the sharp one —
			// SEND B's draft as B's identity. None may succeed.
			"create": map[string]any{
				"steal": map[string]any{"emailId": b.MessageID.String(), "identityId": b.IdentityID.String()},
			},
			"update":  map[string]any{b.SubmissionID.String(): map[string]any{"undoStatus": "canceled"}},
			"destroy": []string{b.SubmissionID.String()},
		})
		if mErr != nil {
			t.Fatalf("EmailSubmission/set: %+v", mErr)
		}
		if !mapHasKey(res, "notCreated", "steal") {
			t.Errorf("A was allowed to submit B's draft: %v", res["created"])
		}
		if mapHasKey(res, "updated", b.SubmissionID.String()) {
			t.Errorf("A canceled B's submission: %v", res["updated"])
		}
		// B's submission is still scheduled — A neither canceled nor destroyed it.
		var state string
		superScan(t, h, &state, `SELECT state FROM app.submissions WHERE id=$1`, b.SubmissionID.String())
		if state != "scheduled" {
			t.Errorf("B's submission state = %q, want scheduled (A must not have touched it)", state)
		}
	},

	// ---- getSendQuota: a read-only meter that must never show B's figures ---
	"EmailSubmission/getSendQuota": func(t *testing.T, h *rlsHarness, a, b rlsUser) {
		// (1) Naming B's JMAP account id is rejected outright by resolveAccount,
		// before any tx opens — identity is the auth-context user, not this param.
		// (This is the assertion that goes RED if the handler drops resolveAccount.)
		if _, mErr := invokeAs(t, h, "EmailSubmission/getSendQuota", a.UserID,
			map[string]any{"accountId": b.UserID.String()}); mErr == nil || mErr.Type != errAccountNotFound {
			t.Fatalf("getSendQuota naming B's account: got %+v, want accountNotFound", mErr)
		}
		// (2) A's own meter lists only A's accounts. The blunt total check first:
		// no id/name/atom of B's anywhere in the response.
		res, mErr := invokeAs(t, h, "EmailSubmission/getSendQuota", a.UserID, map[string]any{})
		if mErr != nil {
			t.Fatalf("getSendQuota (own): %+v", mErr)
		}
		assertNoBLeak(t, "EmailSubmission/getSendQuota", res, b)
		// Positive control + explicit per-entry check: the meter really returned A's
		// account (so an empty list can't be hiding a leak), and no entry is B's id
		// or name.
		foundA := false
		for _, item := range asArr(res["list"]) {
			obj, _ := item.(map[string]any)
			if obj["id"] == a.AccountID.String() && obj["name"] == a.AccountName {
				foundA = true
			}
			if obj["id"] == b.AccountID.String() || obj["name"] == b.AccountName {
				t.Errorf("getSendQuota leaked B's account into A's meter: %v", obj)
			}
		}
		if !foundA {
			t.Errorf("getSendQuota did not return A's own account %s/%q: %v", a.AccountID, a.AccountName, res["list"])
		}
	},

	// ---- Account/getHealth: a health panel that must never show B's account --
	"Account/getHealth": func(t *testing.T, h *rlsHarness, a, b rlsUser) {
		// (1) Naming B's JMAP account id is rejected by resolveAccount BEFORE any
		// tx opens — identity is the auth-context user, not this param. (Goes RED
		// if the handler drops resolveAccount.)
		if _, mErr := invokeAs(t, h, "Account/getHealth", a.UserID,
			map[string]any{"accountId": b.UserID.String()}); mErr == nil || mErr.Type != errAccountNotFound {
			t.Fatalf("getHealth naming B's account: got %+v, want accountNotFound", mErr)
		}
		// (2) A's own panel lists only A's accounts: no id/name/atom of B's anywhere.
		res, mErr := invokeAs(t, h, "Account/getHealth", a.UserID, map[string]any{})
		if mErr != nil {
			t.Fatalf("getHealth (own): %+v", mErr)
		}
		assertNoBLeak(t, "Account/getHealth", res, b)
		// Positive control + explicit per-entry check: the panel really returned A's
		// account (so an empty list can't hide a leak), and no entry is B's.
		foundA := false
		for _, item := range asArr(res["list"]) {
			obj, _ := item.(map[string]any)
			if obj["id"] == a.AccountID.String() && obj["name"] == a.AccountName {
				foundA = true
			}
			if obj["id"] == b.AccountID.String() || obj["name"] == b.AccountName {
				t.Errorf("getHealth leaked B's account into A's panel: %v", obj)
			}
		}
		if !foundA {
			t.Errorf("getHealth did not return A's own account %s/%q: %v", a.AccountID, a.AccountName, res["list"])
		}
	},

	// ---- Agent access (#49): a leak exposes another user's keys (names, scopes,
	// which accounts they reach), what their agents did, and what is waiting on
	// their approval. A sees none of it and can decide none of it.
	"ApiKey/get": func(t *testing.T, h *rlsHarness, a, b rlsUser) {
		bk := seedAgentKey(t, h, b)
		res, mErr := invokeAs(t, h, "ApiKey/get", a.UserID, map[string]any{"ids": []string{bk.keyID}})
		if mErr != nil {
			t.Fatalf("ApiKey/get: %+v", mErr)
		}
		if list := asArr(res["list"]); len(list) != 0 {
			t.Errorf("A got %d of B's keys", len(list))
		}
		if !strInArr(res["notFound"], bk.keyID) {
			t.Errorf("B's key not notFound: %v", res["notFound"])
		}
		all, mErr := invokeAs(t, h, "ApiKey/get", a.UserID, map[string]any{})
		if mErr != nil {
			t.Fatalf("ApiKey/get (all): %+v", mErr)
		}
		if b, _ := json.Marshal(all); strings.Contains(string(b), bk.keyID) || strings.Contains(string(b), bk.name) {
			t.Errorf("A's key list carries B's key: %s", b)
		}
	},
	"ApiKey/set": func(t *testing.T, h *rlsHarness, a, b rlsUser) {
		bk := seedAgentKey(t, h, b)
		res, mErr := invokeAs(t, h, "ApiKey/set", a.UserID, map[string]any{
			"update":  map[string]any{bk.keyID: map[string]any{"scopes": []string{"mail:read", "mail:send"}}},
			"destroy": []string{bk.keyID},
		})
		if mErr != nil {
			t.Fatalf("ApiKey/set: %+v", mErr)
		}
		if !mapHasKey(res, "notUpdated", bk.keyID) || !mapHasKey(res, "notDestroyed", bk.keyID) {
			t.Errorf("A's update/revoke of B's key was not refused: %v", res)
		}
		var scopes []string
		var revoked bool
		superScan2(t, h, `SELECT scopes, revoked_at IS NOT NULL FROM app.api_keys WHERE id=$1`, []any{bk.keyID}, &scopes, &revoked)
		if len(scopes) != 1 || scopes[0] != "mail:read" || revoked {
			t.Errorf("A's set reached B's key: scopes %v revoked %v", scopes, revoked)
		}
	},
	"ApiKey/getEvents": func(t *testing.T, h *rlsHarness, a, b rlsUser) {
		bk := seedAgentKey(t, h, b)
		for _, args := range []map[string]any{{}, {"keyId": bk.keyID}} {
			res, mErr := invokeAs(t, h, "ApiKey/getEvents", a.UserID, args)
			if mErr != nil {
				t.Fatalf("ApiKey/getEvents %v: %+v", args, mErr)
			}
			if list := asArr(res["list"]); len(list) != 0 {
				t.Errorf("A read %d of B's key events (%v)", len(list), args)
			}
		}
	},
	"PendingAction/get": func(t *testing.T, h *rlsHarness, a, b rlsUser) {
		bk := seedAgentKey(t, h, b)
		res, mErr := invokeAs(t, h, "PendingAction/get", a.UserID, map[string]any{"ids": []string{bk.pendingID}})
		if mErr != nil {
			t.Fatalf("PendingAction/get: %+v", mErr)
		}
		if list := asArr(res["list"]); len(list) != 0 {
			t.Errorf("A read %d of B's pending requests", len(list))
		}
		if !strInArr(res["notFound"], bk.pendingID) {
			t.Errorf("B's request not notFound: %v", res["notFound"])
		}
		all, mErr := invokeAs(t, h, "PendingAction/get", a.UserID, map[string]any{})
		if mErr != nil {
			t.Fatalf("PendingAction/get (all): %+v", mErr)
		}
		if b, _ := json.Marshal(all); strings.Contains(string(b), bk.pendingID) || strings.Contains(string(b), bk.summary) {
			t.Errorf("A's pending list carries B's request: %s", b)
		}
	},
	"PendingAction/set": func(t *testing.T, h *rlsHarness, a, b rlsUser) {
		bk := seedAgentKey(t, h, b)
		res, mErr := invokeAs(t, h, "PendingAction/set", a.UserID, map[string]any{
			"update": map[string]any{bk.pendingID: map[string]any{"state": "approved"}}})
		if mErr != nil {
			t.Fatalf("PendingAction/set: %+v", mErr)
		}
		if !mapHasKey(res, "notUpdated", bk.pendingID) {
			t.Errorf("A's approval of B's request was not refused: %v", res)
		}
		var state string
		superScan(t, h, &state, `SELECT state FROM app.api_key_pending WHERE id=$1`, bk.pendingID)
		if state != "pending" {
			t.Errorf("A's set decided B's request: state %q", state)
		}
	},

	// ---- WebPushSubscription: a leak exposes another user's device push --------
	// endpoints and encryption keys. A owns none of B's, cannot read them, and
	// cannot destroy them; keys are never returned even for A's own.
	"WebPushSubscription/get": func(t *testing.T, h *rlsHarness, a, b rlsUser) {
		bp := seedPushSub(t, h, b)
		// A naming B's subscription id gets an empty list + a bare notFound echo,
		// and NOTHING of B's (endpoint, keys, id beyond the echo) anywhere.
		res, mErr := invokeAs(t, h, "WebPushSubscription/get", a.UserID,
			map[string]any{"ids": []string{bp.id}})
		if mErr != nil {
			t.Fatalf("WebPushSubscription/get: %+v", mErr)
		}
		if list := asArr(res["list"]); len(list) != 0 {
			t.Errorf("get of B's subscription returned %d objects, want 0", len(list))
		}
		if !strInArr(res["notFound"], bp.id) {
			t.Errorf("B's subscription id not reported notFound: %v", res["notFound"])
		}
		assertNoPushLeak(t, res, bp)
		// A's own (empty) listing likewise carries nothing of B's.
		all, mErr := invokeAs(t, h, "WebPushSubscription/get", a.UserID, map[string]any{})
		if mErr != nil {
			t.Fatalf("WebPushSubscription/get (all): %+v", mErr)
		}
		assertNoPushLeak(t, all, bp)
	},
	"WebPushSubscription/set": func(t *testing.T, h *rlsHarness, a, b rlsUser) {
		bp := seedPushSub(t, h, b)
		// Naming B's JMAP account id is rejected before any row is touched.
		if _, mErr := invokeAs(t, h, "WebPushSubscription/set", a.UserID,
			map[string]any{"accountId": b.UserID.String()}); mErr == nil || mErr.Type != errAccountNotFound {
			t.Fatalf("set naming B's account: got %+v, want accountNotFound", mErr)
		}
		// A cannot destroy B's subscription.
		res, mErr := invokeAs(t, h, "WebPushSubscription/set", a.UserID,
			map[string]any{"destroy": []string{bp.id}})
		if mErr != nil {
			t.Fatalf("WebPushSubscription/set: %+v", mErr)
		}
		if !mapHasKey(res, "notDestroyed", bp.id) {
			t.Errorf("B's subscription not in notDestroyed: %v", res["notDestroyed"])
		}
		// #59: nor change its push mode.
		res, mErr = invokeAs(t, h, "WebPushSubscription/set", a.UserID,
			map[string]any{"update": map[string]any{bp.id: map[string]any{"notifyMode": "vip"}}})
		if mErr != nil {
			t.Fatalf("WebPushSubscription/set update: %+v", mErr)
		}
		if !mapHasKey(res, "notUpdated", bp.id) {
			t.Errorf("A's mode change on B's subscription was not refused: %v", res)
		}
		var mode string
		superScan(t, h, &mode, `SELECT notify_mode FROM app.push_subscriptions WHERE id=$1`, bp.id)
		if mode != "all" {
			t.Errorf("A's set changed B's push mode to %q", mode)
		}
		if strInArr(res["destroyed"], bp.id) {
			t.Errorf("A destroyed B's subscription %q", bp.id)
		}
		// B's row survives.
		var n int
		superScan(t, h, &n, `SELECT count(*) FROM app.push_subscriptions WHERE id=$1`, bp.id)
		if n != 1 {
			t.Errorf("A's set reached B's subscription: count=%d, want 1", n)
		}
	},

	// ---- SignedInDevice (#937): another user's sessions ----------------------
	// A lists only its own sessions (B's id, user agent and address appear
	// nowhere), and a /get naming B's id is notFound.
	"SignedInDevice/get": func(t *testing.T, h *rlsHarness, a, b rlsUser) {
		_, actx := makeSession(t, h.apiHarness, a.UserID, uaFirefoxLinux, "192.0.2.5", nil)
		bsess, _ := makeSession(t, h.apiHarness, b.UserID, "B-secret-agent Firefox/9", "203.0.113.222", nil)
		res := callCtx(t, actx, h.api.signedInDeviceGet, a.UserID, map[string]any{})
		blob, _ := json.Marshal(res)
		for _, leak := range []string{bsess.String(), "B-secret-agent", "203.0.113.222"} {
			if strings.Contains(string(blob), leak) {
				t.Errorf("A's device list contains B's %q: %s", leak, blob)
			}
		}
		if len(asArr(res["list"])) == 0 {
			t.Error("A's own session is missing from A's list (the probe proves nothing without it)")
		}
		res = callCtx(t, actx, h.api.signedInDeviceGet, a.UserID, map[string]any{"ids": []string{bsess.String()}})
		if len(asArr(res["list"])) != 0 || !strInArr(res["notFound"], bsess.String()) {
			t.Errorf("get naming B's session = %v, want notFound", res)
		}
		// Naming B's JMAP account is refused before any row is read.
		if _, mErr := h.api.signedInDeviceGet(actx, a.UserID,
			mustJSON(t, map[string]any{"accountId": b.UserID.String()})); mErr == nil || mErr.Type != errAccountNotFound {
			t.Errorf("get naming B's account: got %+v, want accountNotFound", mErr)
		}
	},
	// A can neither sign B's device out by id nor reach it through *others*.
	"SignedInDevice/set": func(t *testing.T, h *rlsHarness, a, b rlsUser) {
		_, actx := makeSession(t, h.apiHarness, a.UserID, uaFirefoxLinux, "192.0.2.5", nil)
		bsess, _ := makeSession(t, h.apiHarness, b.UserID, uaFirefoxLinux, "203.0.113.1", []byte("sealed-b"))
		var called bool
		h.api.sessionRevoker = func(ulid.ULID, ulid.ULID, []byte) { called = true }
		defer func() { h.api.sessionRevoker = nil }()

		res := callCtx(t, actx, h.api.signedInDeviceSet, a.UserID, map[string]any{"destroy": []string{bsess.String()}})
		if strInArr(res["destroyed"], bsess.String()) || !mapHasKey(res, "notDestroyed", bsess.String()) {
			t.Errorf("A destroying B's session = %v, want notDestroyed", res)
		}
		callCtx(t, actx, h.api.signedInDeviceSet, a.UserID, map[string]any{"destroy": []string{"*others*"}})
		var reason *string
		superScan(t, h, &reason, `SELECT revoked_reason FROM app.sessions WHERE id=$1`, bsess.String())
		if reason != nil {
			t.Errorf("A's set revoked B's session as %q", *reason)
		}
		if called {
			t.Error("the provider-revocation hook ran for B's session")
		}
		if _, mErr := h.api.signedInDeviceSet(actx, a.UserID,
			mustJSON(t, map[string]any{"accountId": b.UserID.String()})); mErr == nil || mErr.Type != errAccountNotFound {
			t.Errorf("set naming B's account: got %+v, want accountNotFound", mErr)
		}
	},

	// ---- SearchSnippet/get: the id may be echoed, the CONTENT never ---------
	"SearchSnippet/get": func(t *testing.T, h *rlsHarness, a, b rlsUser) {
		res, mErr := invokeAs(t, h, "SearchSnippet/get", a.UserID, map[string]any{
			"emailIds": []string{b.MessageID.String()},
			"filter":   map[string]any{"body": "invoice"},
		})
		if mErr != nil {
			t.Fatalf("SearchSnippet/get: %+v", mErr)
		}
		for _, item := range asArr(res["list"]) {
			obj, _ := item.(map[string]any)
			if obj["emailId"] != b.MessageID.String() {
				continue
			}
			// subject/preview null == "nothing of B's message matched or was read".
			if obj["subject"] != nil || obj["preview"] != nil {
				t.Errorf("SearchSnippet/get leaked B's message content: %v", obj)
			}
		}
	},

	// ---- the desk (#442) ---------------------------------------------------

	// Area/get: A's read must show A's OWN area (materialised or, as here,
	// already seeded) and nothing of B's — the same CategoryTab/get shape,
	// including the "a broken isolation looks like an extra entry, not an
	// empty list" note.
	"Area/get": func(t *testing.T, h *rlsHarness, a, b rlsUser) {
		res, mErr := invokeAs(t, h, "Area/get", a.UserID, map[string]any{})
		if mErr != nil {
			t.Fatalf("Area/get: %+v", mErr)
		}
		assertNoBLeak(t, "Area/get", res, b)

		res, mErr = invokeAs(t, h, "Area/get", a.UserID, map[string]any{
			"ids": []string{b.AreaID.String()},
		})
		if mErr != nil {
			t.Fatalf("Area/get by id: %+v", mErr)
		}
		assertNoBLeakExcept(t, "Area/get by id", res, b, b.AreaID.String())
		if !strInArr(res["notFound"], b.AreaID.String()) {
			t.Errorf("Area/get by a foreign id did not report notFound: %v", res)
		}
	},

	"Area/changes": probeChangesNoLeak("Area/changes"),

	// Area/set is TWO probes in one, matching the forcing-function comment:
	//
	//  1. The ordinary refusal — A cannot rename or destroy B's area.
	//  2. THE SHARP CASE — A's own area claims the SAME TAB ("updates") that
	//     B's area already holds. The two per-user unique indexes
	//     (area_feeds_tab_once/area_feeds_label_once) are scoped by user_id,
	//     so this must SUCCEED for A exactly as if B's area did not exist —
	//     a caller cannot even learn there was a "conflict" to avoid — and it
	//     must leave B's own claim on that tab completely undisturbed.
	"Area/set": func(t *testing.T, h *rlsHarness, a, b rlsUser) {
		res, mErr := invokeAs(t, h, "Area/set", a.UserID, map[string]any{
			"update":  map[string]any{b.AreaID.String(): map[string]any{"name": rlsSentinel}},
			"destroy": []string{b.AreaID.String()},
		})
		if mErr != nil {
			t.Fatalf("Area/set: %+v", mErr)
		}
		assertSetRefused(t, "Area/set", res, b.AreaID.String())
		assertRowExists(t, h, "areas", b.AreaID)
		assertRowUnchanged(t, h, "areas", "name", b.AreaID)

		// Give B's area a TAB feed — seeded here, specific to this probe,
		// rather than in seedRLSUser, so A's area genuinely does NOT already
		// hold it: the assertion below has to observe a real claim, not a
		// no-op re-assertion of something A already had.
		if _, err := h.super.Exec(context.Background(), `
			INSERT INTO app.area_feeds (id, user_id, area_id, tab)
			VALUES ($1, $2, $3, 'updates'::app.category_tab)`,
			store.NewULID().String(), b.UserID.String(), b.AreaID.String()); err != nil {
			t.Fatalf("seed B's tab feed: %v", err)
		}

		// THE SHARP CASE: A claims "updates" for A's OWN area, which B
		// already holds. RLS must make this invisible to A rather than
		// merely forbidden.
		res, mErr = invokeAs(t, h, "Area/set", a.UserID, map[string]any{
			"update": map[string]any{a.AreaID.String(): map[string]any{"tabs": []string{"updates"}}},
		})
		if mErr != nil {
			t.Fatalf("Area/set (A claims a tab B already holds): %+v", mErr)
		}
		if !mapHasKey(res, "updated", a.AreaID.String()) {
			t.Errorf("A's own claim on a tab was refused because ANOTHER USER already holds it "+
				"— RLS must scope that uniqueness check by user_id, not leak across users: %v", res)
		}
		var aTabs, bTabs int
		superScan(t, h, &aTabs, `SELECT count(*) FROM app.area_feeds WHERE area_id=$1 AND tab='updates'`,
			a.AreaID.String())
		superScan(t, h, &bTabs, `SELECT count(*) FROM app.area_feeds WHERE area_id=$1 AND tab='updates'`,
			b.AreaID.String())
		if aTabs != 1 {
			t.Errorf("A's area does not carry the 'updates' tab feed after claiming it: count=%d", aTabs)
		}
		if bTabs != 1 {
			t.Errorf("A's claim on 'updates' REMOVED it from B's area: count=%d, want 1 — "+
				"a per-user feed claim must not be able to touch another user's row at all", bTabs)
		}
	},

	// Area/getSummary: A's counts describe only A's own mail and A's own
	// areas — never B's area id, and never B's message folded into A's
	// totals (which RLS's own m.user_id/a.user_id predicates make structurally
	// impossible, but the surface is worth pinning against regression).
	"Area/getSummary": func(t *testing.T, h *rlsHarness, a, b rlsUser) {
		res, mErr := invokeAs(t, h, "Area/getSummary", a.UserID, map[string]any{})
		if mErr != nil {
			t.Fatalf("Area/getSummary: %+v", mErr)
		}
		assertNoBLeak(t, "Area/getSummary", res, b)

		// Naming B's mail account scopes to nothing of A's (a foreign account
		// id matches zero provenance rows for A, by construction — see
		// AreaSummary's own doc comment), rather than erroring.
		res, mErr = invokeAs(t, h, "Area/getSummary", a.UserID, map[string]any{
			"mailAccountId": b.AccountID.String(),
		})
		if mErr != nil {
			t.Fatalf("Area/getSummary mailAccountId=B: %+v", mErr)
		}
		for _, item := range asArr(res["list"]) {
			obj, _ := item.(map[string]any)
			if s, _ := obj["surface"].(float64); s != 0 {
				t.Errorf("Area/getSummary scoped to B's account counted %v surface mail for A", s)
			}
			if d, _ := obj["drawer"].(float64); d != 0 {
				t.Errorf("Area/getSummary scoped to B's account counted %v drawer mail for A", d)
			}
			if u, _ := obj["unscored"].(float64); u != 0 {
				t.Errorf("Area/getSummary scoped to B's account counted %v unscored mail for A", u)
			}
		}
	},

	// Area/getDesk (#665): Area/getSummary's two cases, plus sections. A section
	// naming B's AREA must come back in notFound (echoing the id A sent is the
	// one allowed mention of it) and never as a page of B's mail; A's own
	// sections and Needs sorting must list none of B's messages, scoped or not.
	"Area/getDesk": func(t *testing.T, h *rlsHarness, a, b rlsUser) {
		sections := []map[string]any{
			{"areaId": a.AreaID.String()}, {"areaId": nil}, {"areaId": b.AreaID.String()},
		}
		res, mErr := invokeAs(t, h, "Area/getDesk", a.UserID, map[string]any{"sections": sections})
		if mErr != nil {
			t.Fatalf("Area/getDesk: %+v", mErr)
		}
		assertNoBLeakExcept(t, "Area/getDesk", res, b, b.AreaID.String())
		if !strInArr(res["notFound"], b.AreaID.String()) {
			t.Errorf("Area/getDesk: B's area was not reported notFound: %v", res["notFound"])
		}
		if n := len(asArr(res["sections"])); n != 2 {
			t.Errorf("Area/getDesk answered %d sections, want A's area and Needs sorting only", n)
		}

		res, mErr = invokeAs(t, h, "Area/getDesk", a.UserID, map[string]any{
			"mailAccountId": b.AccountID.String(), "sections": sections[:2],
		})
		if mErr != nil {
			t.Fatalf("Area/getDesk mailAccountId=B: %+v", mErr)
		}
		assertNoBLeakExcept(t, "Area/getDesk mailAccountId=B", res, b, b.AccountID.String())
		for _, item := range asArr(res["sections"]) {
			obj, _ := item.(map[string]any)
			if n, _ := obj["total"].(float64); n != 0 || len(asArr(obj["ids"])) != 0 {
				t.Errorf("Area/getDesk scoped to B's account listed %v messages for A: %v", n, obj)
			}
		}
		for _, item := range asArr(res["list"]) {
			obj, _ := item.(map[string]any)
			for _, k := range []string{"surface", "drawer", "unscored"} {
				if v, _ := obj[k].(float64); v != 0 {
					t.Errorf("Area/getDesk scoped to B's account counted %v %s mail for A", v, k)
				}
			}
		}
	},

	"OutOfOffice/get": func(t *testing.T, h *rlsHarness, a, b rlsUser) {
		probeGetFindsNothing("OutOfOffice/get", "ids", func(b rlsUser) string { return b.AccountID.String() })(t, h, a, b)
		// With no ids: every account of A's and nothing of B's.
		res, mErr := invokeAs(t, h, "OutOfOffice/get", a.UserID, map[string]any{})
		if mErr != nil {
			t.Fatalf("OutOfOffice/get: %+v", mErr)
		}
		assertNoBLeak(t, "OutOfOffice/get (all)", res, b)
		if len(asArr(res["list"])) == 0 {
			t.Errorf("OutOfOffice/get (all) returned nothing for A: the probe would pass vacuously")
		}
	},
	"OutOfOffice/check": probeGetFindsNothing("OutOfOffice/check", "ids",
		func(b rlsUser) string { return b.AccountID.String() }),
	"OutOfOffice/set": func(t *testing.T, h *rlsHarness, a, b rlsUser) {
		res, mErr := invokeAs(t, h, "OutOfOffice/set", a.UserID, map[string]any{
			"update": map[string]any{b.AccountID.String(): map[string]any{
				"isEnabled": false, "fromDate": nil, "toDate": nil, "subject": "hijacked",
				"textBody": "hijacked", "replyDays": 3, "fromAddress": "", "contactsOnly": false,
			}},
		})
		if mErr != nil {
			t.Fatalf("OutOfOffice/set: %+v", mErr)
		}
		if !mapHasKey(res, "notUpdated", b.AccountID.String()) || mapHasKey(res, "updated", b.AccountID.String()) {
			t.Errorf("OutOfOffice/set did not refuse B's account: %v", res)
		}
		assertNoBLeakExcept(t, "OutOfOffice/set", res, b, b.AccountID.String())
		var subject string
		var enabled bool
		superScan(t, h, &subject, `SELECT subject FROM app.out_of_office WHERE account_id=$1`, b.AccountID.String())
		superScan(t, h, &enabled, `SELECT enabled FROM app.out_of_office WHERE account_id=$1`, b.AccountID.String())
		if subject != b.OOFSubject || !enabled {
			t.Errorf("A's OutOfOffice/set changed B's reply: subject %q enabled %v", subject, enabled)
		}
	},

	"DeskPreferences/get": probeGetFindsNothing("DeskPreferences/get", "ids",
		func(b rlsUser) string { return b.UserID.String() }),
	"DeskPreferences/changes": probeChangesNoLeak("DeskPreferences/changes"),

	// DeskPreferences/set's only valid update id is the CALLER's own user id
	// (the singleton's wire id). Naming B's user id must refuse exactly as an
	// id that never existed would, and must not touch B's threshold.
	"DeskPreferences/set": func(t *testing.T, h *rlsHarness, a, b rlsUser) {
		res, mErr := invokeAs(t, h, "DeskPreferences/set", a.UserID, map[string]any{
			"update": map[string]any{b.UserID.String(): map[string]any{"lineThreshold": 0.99}},
		})
		if mErr != nil {
			t.Fatalf("DeskPreferences/set: %+v", mErr)
		}
		if !mapHasKey(res, "notUpdated", b.UserID.String()) {
			t.Errorf("DeskPreferences/set did not refuse B's user id: %v", res)
		}
		if mapHasKey(res, "updated", b.UserID.String()) {
			t.Errorf("DeskPreferences/set updated B's preferences: %v", res["updated"])
		}
		// float32, not float64: line_threshold is a Postgres `real` (4-byte
		// float), and reading it into a float64 destination widens the
		// stored bit pattern without fixing its precision — 0.42 stored as a
		// real reads back as 0.41999998688697815 through a float64
		// destination, which is not what "unchanged" means here. Comparing
		// at the column's own precision is the correct check.
		var threshold float32
		superScan(t, h, &threshold, `SELECT line_threshold FROM app.desk_preferences WHERE user_id=$1`,
			b.UserID.String())
		if threshold != 0.42 {
			t.Errorf("A changed B's lineThreshold: %v, want 0.42 (the seeded value)", threshold)
		}
	},

	"ClientPreferences/get": probeGetFindsNothing("ClientPreferences/get", "ids",
		func(b rlsUser) string { return b.ClientPrefWebID.String() }),
	"ClientPreferences/changes": probeChangesNoLeak("ClientPreferences/changes"),

	// ClientPreferences/set: naming B's row id must not resolve to any family
	// under A's call at all (famByID is built from A's OWN three rows), so a
	// rewrite of B's theme can never land.
	"ClientPreferences/set": func(t *testing.T, h *rlsHarness, a, b rlsUser) {
		res, mErr := invokeAs(t, h, "ClientPreferences/set", a.UserID, map[string]any{
			"update": map[string]any{b.ClientPrefWebID.String(): map[string]any{"theme": "light"}},
		})
		if mErr != nil {
			t.Fatalf("ClientPreferences/set: %+v", mErr)
		}
		if !mapHasKey(res, "notUpdated", b.ClientPrefWebID.String()) {
			t.Errorf("ClientPreferences/set did not refuse B's row id: %v", res)
		}
		if mapHasKey(res, "updated", b.ClientPrefWebID.String()) {
			t.Errorf("ClientPreferences/set updated B's preferences: %v", res["updated"])
		}
		assertRowExists(t, h, "client_preferences", b.ClientPrefWebID)
		var theme string
		superScan(t, h, &theme, `SELECT theme FROM app.client_preferences WHERE id=$1`, b.ClientPrefWebID.String())
		if theme != "dark" {
			t.Errorf("A changed B's theme: %q, want \"dark\" (the seeded value)", theme)
		}
	},

	// Day-one setup (#446). Three of the six are reports and are probed for
	// LEAKAGE; the other three take an id and are probed for the WRITE.
	//
	// Setup/census is the read that would leak the most if it leaked at all: it
	// names every folder, its message counts, and how concentrated its senders
	// are — a description of somebody's whole filing system.
	"Setup/census": probeQueryNoLeak("Setup/census"),
	"Setup/get": probeGetFindsNothing("Setup/get", "ids",
		func(b rlsUser) string { return b.SetupRunID.String() }),
	"Setup/changes": probeChangesNoLeak("Setup/changes"),

	// Setup/preview: A naming B'S MAILBOX in a plan must be refused, not
	// silently counted over B's mail. RLS makes B's mailbox invisible to A's
	// census, so the plan resolves to "not a folder day-one can act on" — the
	// SAME answer a mailbox id that never existed gets, which is the fail-closed
	// property (a caller cannot use the error to learn B's mailbox exists).
	"Setup/preview": func(t *testing.T, h *rlsHarness, a, b rlsUser) {
		_, mErr := invokeAs(t, h, "Setup/preview", a.UserID, map[string]any{
			"plan": map[string]any{
				"windowDays": 30,
				"folders": []any{map[string]any{
					"mailboxId": b.InboxID.String(), "action": "labelArchive", "labelName": rlsSentinel,
				}},
			},
		})
		if mErr == nil {
			t.Fatalf("Setup/preview accepted a plan naming B's mailbox %s", b.InboxID)
		}
		if mErr.Type != errInvalidArguments {
			t.Errorf("Setup/preview on B's mailbox: error type %q, want %q", mErr.Type, errInvalidArguments)
		}
	},

	// Setup/apply: the same refusal, plus the checks that matter — no run, no
	// journal row and no label may appear under B, and A must not have created a
	// run either (a refused plan writes nothing at all).
	"Setup/apply": func(t *testing.T, h *rlsHarness, a, b rlsUser) {
		_, mErr := invokeAs(t, h, "Setup/apply", a.UserID, map[string]any{
			"plan": map[string]any{
				"windowDays": 30,
				"folders": []any{map[string]any{
					"mailboxId": b.InboxID.String(), "action": "labelArchive", "labelName": rlsSentinel,
				}},
			},
		})
		if mErr == nil {
			t.Fatalf("Setup/apply accepted a plan naming B's mailbox %s", b.InboxID)
		}
		if mErr.Type != errInvalidArguments {
			t.Errorf("Setup/apply on B's mailbox: error type %q, want %q", mErr.Type, errInvalidArguments)
		}
		var runsForA int
		superScan(t, h, &runsForA, `SELECT count(*) FROM app.setup_runs WHERE user_id=$1`, a.UserID.String())
		if runsForA != 1 {
			t.Errorf("A has %d setup runs after a REFUSED apply, want 1 (only the seeded one) — "+
				"a refused plan must write nothing", runsForA)
		}
		var labelled int
		superScan(t, h, &labelled, `SELECT count(*) FROM app.labels WHERE user_id=$1 AND name=$2`,
			b.UserID.String(), rlsSentinel)
		if labelled != 0 {
			t.Errorf("A minted %d label(s) named %q under B", labelled, rlsSentinel)
		}
	},

	// Setup/undo is the sharpest of the six: it DELETES LABELS, CLEARS THE
	// ARCHIVE MARK and MOVES MAIL from a run's journal. A naming B's run id must
	// be refused, and B's journal, B's setup-applied label and B's archive mark
	// must all still be exactly there afterwards.
	"Setup/undo": func(t *testing.T, h *rlsHarness, a, b rlsUser) {
		_, mErr := invokeAs(t, h, "Setup/undo", a.UserID, map[string]any{
			"runId": b.SetupRunID.String(),
		})
		if mErr == nil {
			t.Fatalf("Setup/undo accepted B's run id %s", b.SetupRunID)
		}
		if mErr.Type != errInvalidArguments {
			t.Errorf("Setup/undo on B's run: error type %q, want %q", mErr.Type, errInvalidArguments)
		}
		assertRowExists(t, h, "setup_runs", b.SetupRunID)
		var status string
		superScan(t, h, &status, `SELECT status FROM app.setup_runs WHERE id=$1`, b.SetupRunID.String())
		if status != "done" {
			t.Errorf("A moved B's run to %q, want \"done\" (the seeded value)", status)
		}
		var journal int
		superScan(t, h, &journal, `SELECT count(*) FROM app.setup_journal WHERE run_id=$1`,
			b.SetupRunID.String())
		if journal != 1 {
			t.Errorf("B's setup journal has %d row(s) after A's undo attempt, want 1", journal)
		}
		var stillLabelled int
		superScan(t, h, &stillLabelled, `SELECT count(*) FROM app.message_labels
			 WHERE user_id=$1 AND message_id=$2 AND label_id=$3 AND applied_by='setup'`,
			b.UserID.String(), b.MessageID.String(), b.SetupLabelID.String())
		if stillLabelled != 1 {
			t.Errorf("A removed B's setup-applied label: count=%d, want 1", stillLabelled)
		}
		var archived int
		superScan(t, h, &archived, `SELECT count(*) FROM app.messages
			 WHERE user_id=$1 AND id=$2 AND desk_archived_at IS NOT NULL`,
			b.UserID.String(), b.MessageID.String())
		if archived != 1 {
			t.Errorf("A cleared B's desk_archived_at: count=%d, want 1", archived)
		}
	},
}

// rlsSentinel is the recognizable value A tries to write onto B's objects; a
// probe then proves it never landed.
const rlsSentinel = "PWNED-BY-A"

// --- probe builders ---------------------------------------------------------

// probeGetFindsNothing builds a /get probe: A naming B's id gets an empty list
// and B's id back in notFound, and no identifier of B's anywhere in the response.
func probeGetFindsNothing(method, idKey string, bID func(rlsUser) string) func(*testing.T, *rlsHarness, rlsUser, rlsUser) {
	return func(t *testing.T, h *rlsHarness, a, b rlsUser) {
		id := bID(b)
		res, mErr := invokeAs(t, h, method, a.UserID, map[string]any{idKey: []string{id}})
		if mErr != nil {
			t.Fatalf("%s: %+v", method, mErr)
		}
		if list := asArr(res["list"]); len(list) != 0 {
			t.Errorf("%s of B's id returned %d objects, want 0", method, len(list))
		}
		if !strInArr(res["notFound"], id) {
			t.Errorf("%s: B's id %q not reported notFound: %v", method, id, res["notFound"])
		}
		// The requested id is echoed back in notFound — that is not a leak (the
		// client supplied it). But NOTHING ELSE of B's may appear: an empty list
		// plus a bare notFound echo, and no other id, atom, or hash of B's.
		assertNoBLeakExcept(t, method, res, b, id)
	}
}

func probeQueryNoLeak(method string) func(*testing.T, *rlsHarness, rlsUser, rlsUser) {
	return func(t *testing.T, h *rlsHarness, a, b rlsUser) {
		res, mErr := invokeAs(t, h, method, a.UserID, map[string]any{})
		if mErr != nil {
			t.Fatalf("%s: %+v", method, mErr)
		}
		assertNoBLeak(t, method, res, b)
	}
}

func probeChangesNoLeak(method string) func(*testing.T, *rlsHarness, rlsUser, rlsUser) {
	return func(t *testing.T, h *rlsHarness, a, b rlsUser) {
		res, mErr := invokeAs(t, h, method, a.UserID, map[string]any{"sinceState": "s:0"})
		if mErr != nil {
			t.Fatalf("%s: %+v", method, mErr)
		}
		assertNoBLeak(t, method, res, b)
	}
}

// --- assertions -------------------------------------------------------------

// assertNoBLeak fails if any of B's identifiers appears anywhere in A's response.
// It is the blunt, total check for READ methods: a get/query/changes response
// that A receives must contain no id, atom, or hash of B's — not in a list, not
// in a notFound echo (a read method never echoes an id it was not given), not
// anywhere.
func assertNoBLeak(t *testing.T, label string, res map[string]any, b rlsUser) {
	t.Helper()
	assertNoBLeakExcept(t, label, res, b, "")
}

// assertNoBLeakExcept is assertNoBLeak with one allowed identifier — the id a
// method legitimately echoes back to the caller (a /get's notFound). Every OTHER
// identifier of B's is still forbidden.
func assertNoBLeakExcept(t *testing.T, label string, res map[string]any, b rlsUser, except string) {
	t.Helper()
	blob, err := json.Marshal(res)
	if err != nil {
		t.Fatalf("%s: re-marshal response: %v", label, err)
	}
	s := string(blob)
	for _, id := range b.identifiers() {
		if id == "" || id == except {
			continue
		}
		if strings.Contains(s, id) {
			t.Errorf("%s: A's response LEAKED B's identifier %q:\n%s", label, id, s)
		}
	}
}

// assertSetRefused fails unless B's id was refused (notUpdated/notDestroyed) and
// never appears among the mutations A's /set actually performed.
func assertSetRefused(t *testing.T, label string, res map[string]any, bID string) {
	t.Helper()
	if !mapHasKey(res, "notUpdated", bID) {
		t.Errorf("%s: B's id %q not in notUpdated: %v", label, bID, res["notUpdated"])
	}
	if mapHasKey(res, "updated", bID) {
		t.Errorf("%s: A updated B's object %q: %v", label, bID, res["updated"])
	}
	if strInArr(res["destroyed"], bID) {
		t.Errorf("%s: A destroyed B's object %q", label, bID)
	}
}

// labelCount is how many labels a user's mail carries, in total. The blunt
// instrument on purpose: a WRITING method that leaked would move this number,
// whatever it did and whichever message it reached.
func labelCount(t *testing.T, h *rlsHarness, u rlsUser) int {
	t.Helper()
	var n int
	if err := h.super.QueryRow(context.Background(),
		`SELECT count(*) FROM app.message_labels WHERE user_id = $1`, u.UserID.String()).Scan(&n); err != nil {
		t.Fatalf("count labels: %v", err)
	}
	return n
}

func assertRowExists(t *testing.T, h *rlsHarness, table string, id ulid.ULID) {
	t.Helper()
	var n int
	superScan(t, h, &n, `SELECT count(*) FROM app.`+table+` WHERE id=$1`, id.String())
	if n != 1 {
		t.Errorf("app.%s row %s: count=%d, want 1 (A must not have destroyed it)", table, id, n)
	}
}

func assertRowUnchanged(t *testing.T, h *rlsHarness, table, col string, id ulid.ULID) {
	t.Helper()
	var n int
	superScan(t, h, &n, `SELECT count(*) FROM app.`+table+` WHERE id=$1 AND `+col+` = $2`,
		id.String(), rlsSentinel)
	if n != 0 {
		t.Errorf("app.%s.%s of row %s was set to the sentinel — A mutated B's object", table, col, id)
	}
}

// --- helpers ----------------------------------------------------------------

// invokeAs dispatches a method through the SAME table the request handler uses
// (h.api.methods), as user `as`, returning the decoded result or the method
// error. Driving via the table (not the typed handler funcs) ties every probe to
// the exact thing dispatch calls.
func invokeAs(t *testing.T, h *rlsHarness, method string, as ulid.ULID, args any) (map[string]any, *MethodError) {
	t.Helper()
	fn, ok := h.api.methods[method]
	if !ok {
		t.Fatalf("no such method %q", method)
	}
	raw, mErr := fn(context.Background(), as, mustJSON(t, args))
	if mErr != nil {
		return nil, mErr
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("%s: decode result: %v", method, err)
	}
	return out, nil
}

func superExec(t *testing.T, h *rlsHarness, query string, args ...any) {
	t.Helper()
	if _, err := h.super.Exec(context.Background(), query, args...); err != nil {
		t.Fatalf("super exec %q: %v", query, err)
	}
}

func superScan(t *testing.T, h *rlsHarness, dst any, query string, args ...any) {
	t.Helper()
	if err := h.super.QueryRow(context.Background(), query, args...).Scan(dst); err != nil {
		t.Fatalf("super query %q: %v", query, err)
	}
}

func asArr(v any) []any {
	a, _ := v.([]any)
	return a
}

func strInArr(v any, want string) bool {
	for _, x := range asArr(v) {
		if s, _ := x.(string); s == want {
			return true
		}
	}
	return false
}

func mapHasKey(res map[string]any, bucket, key string) bool {
	m, _ := res[bucket].(map[string]any)
	_, ok := m[key]
	return ok
}

// pushSub is a subscription seeded for user B: the distinctive strings that must
// never surface in A's WebPushSubscription responses.
type pushSub struct {
	id       string
	endpoint string
	p256dh   string
	auth     string
}

// seedPushSub inserts a Web Push subscription for user B directly (as the super
// user, bypassing RLS to SET UP the fixture), so a probe can prove A can neither
// read nor destroy it under RLS.
func seedPushSub(t *testing.T, h *rlsHarness, b rlsUser) pushSub {
	t.Helper()
	ps := pushSub{
		id:       store.NewULID().String(),
		endpoint: "https://push.example.com/dev-" + store.NewULID().String(),
		p256dh:   "p256dh-secret-" + b.UserID.String(),
		auth:     "auth-secret-" + b.UserID.String(),
	}
	if _, err := h.super.Exec(context.Background(), `
		INSERT INTO app.push_subscriptions
			(id, user_id, endpoint, keys_p256dh, keys_auth, device_name, important_only)
		VALUES ($1,$2,$3,$4,$5,$6,true)`,
		ps.id, b.UserID.String(), ps.endpoint, ps.p256dh, ps.auth, "B's phone"); err != nil {
		t.Fatalf("seed push subscription for B: %v", err)
	}
	return ps
}

// assertNoPushLeak fails if any of B's subscription's sensitive strings — its
// endpoint or either encryption key — appears anywhere in A's response. Keys are
// write-only and must never be returned even to their owner, so their appearance
// in ANY /get response is a bug.
func assertNoPushLeak(t *testing.T, res map[string]any, bp pushSub) {
	t.Helper()
	blob, err := json.Marshal(res)
	if err != nil {
		t.Fatalf("re-marshal push response: %v", err)
	}
	s := string(blob)
	for _, secret := range []string{bp.endpoint, bp.p256dh, bp.auth} {
		if strings.Contains(s, secret) {
			t.Errorf("WebPushSubscription response leaked %q:\n%s", secret, s)
		}
	}
}

// --- HTTP data endpoints ----------------------------------------------------
//
// These are the routes crossUserProbedRoutes points at. They are driven over the
// REAL mux (API.Handler behind the only thing that injects identity, the auth
// middleware — spec §10), because the isolation claim is about the whole HTTP
// path, not a handler in isolation.

// TestRLSCrossUserDownload: the blob store is content-addressed, so a blobId IS a
// hash. The only thing between "A learned B's blobId" and "A has B's attachment"
// is that DownloadHandler resolves ownership in Postgres under A's RLS — never
// against the hash alone.
func TestRLSCrossUserDownload(t *testing.T) {
	h := newRLSHarness(t)
	a, b := seedRLSTwo(t, h.super)
	handler := auth.DevAuth(h.api.Handler(), a.UserID)

	// (1) B's blob under B's account path: the account is not A's → 404, and the
	// server never even admits the account exists.
	if rec := serve(handler, "GET", "/jmap/download/"+b.UserID.String()+"/"+b.BlobID+"/x.eml"); rec.Code != http.StatusNotFound {
		t.Errorf("download B's blob via B's account: status %d, want 404", rec.Code)
	}
	// (2) THE test: B's blob hash under A's OWN account. The account matches, so
	// this reaches the RLS-scoped ownership lookup — which must not find it.
	rec := serve(handler, "GET", "/jmap/download/"+a.UserID.String()+"/"+b.BlobID+"/x.eml")
	if rec.Code != http.StatusNotFound {
		t.Errorf("download B's blob via A's account: status %d, want 404 (RLS ownership)", rec.Code)
	}
	if strings.Contains(rec.Body.String(), b.UserID.String()) {
		t.Errorf("download response referenced B: %s", rec.Body.String())
	}
}

// TestRLSCrossUserPreview: the preview route is the one door that serves bytes for
// INLINE rendering (#131), so a cross-user read here would be worse than through
// download — the attacker would not merely obtain someone else's attachment, they
// would have it rendered for them. It runs the SAME RLS-scoped ownership lookup,
// and this proves the new door did not skip it.
func TestRLSCrossUserPreview(t *testing.T) {
	h := newRLSHarness(t)
	a, b := seedRLSTwo(t, h.super)
	handler := auth.DevAuth(h.api.Handler(), a.UserID)

	// B's blob under B's account: the account is not A's, so the server answers as
	// if it did not exist.
	if rec := serve(handler, "GET", "/jmap/preview/"+b.UserID.String()+"/"+b.BlobID+".1/x.pdf"); rec.Code != http.StatusNotFound {
		t.Errorf("preview B's part via B's account: status %d, want 404", rec.Code)
	}
	// B's blob hash under A's OWN account — the case that reaches the ownership
	// lookup. A 200 here would be a cross-user read; a 415 would mean the type gate
	// answered BEFORE authorization, which would be a disclosure of its own (it
	// would confirm the part exists and what it is).
	rec := serve(handler, "GET", "/jmap/preview/"+a.UserID.String()+"/"+b.BlobID+".1/x.pdf")
	if rec.Code != http.StatusNotFound {
		t.Errorf("preview B's part via A's account: status %d, want 404 (RLS ownership)", rec.Code)
	}
	if strings.Contains(rec.Body.String(), b.UserID.String()) {
		t.Errorf("preview response referenced B: %s", rec.Body.String())
	}
}

// TestRLSCrossUserUpload: an upload naming B's account is refused. The uploaded
// blob would otherwise be recorded — under WHOSE rows is the question RLS answers,
// and the account in the path must never be the answer (spec §10).
func TestRLSCrossUserUpload(t *testing.T) {
	h := newRLSHarness(t)
	a, b := seedRLSTwo(t, h.super)
	handler := auth.DevAuth(h.api.Handler(), a.UserID)

	req := httptest.NewRequest("POST", "/jmap/upload/"+b.UserID.String()+"/", strings.NewReader("hello"))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("upload to B's account: status %d, want 404 (accountNotFound)", rec.Code)
	}
}

// TestRLSEventSourceIsOwnAccountOnly: the push stream is keyed by the caller's own
// account id and only ever reports the caller's state — it takes no account
// parameter, so it cannot be pointed at B, and its payload must name only A.
func TestRLSEventSourceIsOwnAccountOnly(t *testing.T) {
	h := newRLSHarness(t)
	a, b := seedRLSTwo(t, h.super)

	srv := httptest.NewServer(auth.DevAuth(h.api.Handler(), a.UserID))
	defer srv.Close()

	resp, err := srv.Client().Get(srv.URL + "/jmap/eventsource/?closeafter=state")
	if err != nil {
		t.Fatalf("eventsource: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	buf := make([]byte, 4096)
	n, _ := resp.Body.Read(buf)
	body := string(buf[:n])
	if !strings.Contains(body, a.UserID.String()) {
		t.Errorf("eventsource did not report A's own account: %s", body)
	}
	if strings.Contains(body, b.UserID.String()) {
		t.Errorf("eventsource leaked B's account id: %s", body)
	}
}

// TestRLSSessionIsOwnAccountOnly closes the exemption in exemptCrossUserRoutes:
// the Session resource names the caller's account and no one else's.
func TestRLSSessionIsOwnAccountOnly(t *testing.T) {
	h := newRLSHarness(t)
	a, b := seedRLSTwo(t, h.super)
	handler := auth.DevAuth(h.api.Handler(), a.UserID)

	rec := serve(handler, "GET", "/jmap/session")
	if rec.Code != http.StatusOK {
		t.Fatalf("session: status %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, a.UserID.String()) {
		t.Errorf("session did not name A's account: %s", body)
	}
	if strings.Contains(body, b.UserID.String()) {
		t.Errorf("session leaked B's account id: %s", body)
	}
}

// neverFetcher is a proxy fetcher that fails the test if it is ever used.
//
// ⚠ IT IS THE NEGATIVE CONTROL, and without it the probe below would be
// vacuous. A cross-user proxy request that returns 404 proves isolation ONLY if
// the 404 came from the authorization decision. A 404 produced because the fetch
// happened and failed would look identical — and would mean the server had
// already made an outbound request on another user's message.
type neverFetcher struct{ t *testing.T }

func (f neverFetcher) Do(req *http.Request) (*http.Response, error) {
	f.t.Errorf("the image proxy made an OUTBOUND REQUEST for a cross-user probe: %s\n"+
		"The 404 under test would then be a fetch failure, not an authorization "+
		"refusal, and the server would already have contacted the sender.", req.URL.Host)
	return nil, errors.New("neverFetcher: must not be called")
}

// TestRLSCrossUserImageProxy: A may not cause a fetch for B's message.
//
// The image proxy resolves a URL out of a message's stored bytes (ADR-026 §3b),
// so "which message" is an authorization question and RLS is the answer. A must
// get the same 404 whether it names B's account or its own, and the server must
// not have reached the network in either case.
func TestRLSCrossUserImageProxy(t *testing.T) {
	h := newRLSHarness(t)
	a, b := seedRLSTwo(t, h.super)
	withImageProxyClient(ProxyConfig{Enabled: true}, neverFetcher{t})(h.api)
	handler := auth.DevAuth(h.api.Handler(), a.UserID)

	token := ProxyTokenFor("https://tracker.example/pixel.png")

	// B's message under B's account: the account is not A's, so the server
	// answers as if it did not exist.
	if rec := serve(handler, "GET",
		"/jmap/proxy/"+b.UserID.String()+"/"+b.MessageID.String()+"/"+token); rec.Code != http.StatusNotFound {
		t.Errorf("proxy B's message via B's account: status %d, want 404", rec.Code)
	}
	// B's message id under A's OWN account — the case that reaches the message
	// lookup, where RLS is what refuses.
	rec := serve(handler, "GET",
		"/jmap/proxy/"+a.UserID.String()+"/"+b.MessageID.String()+"/"+token)
	if rec.Code != http.StatusNotFound {
		t.Errorf("proxy B's message via A's account: status %d, want 404 (RLS ownership)", rec.Code)
	}
	if strings.Contains(rec.Body.String(), b.UserID.String()) ||
		strings.Contains(rec.Body.String(), b.MessageID.String()) {
		t.Errorf("proxy response referenced B: %s", rec.Body.String())
	}
	// A's OWN message with a token for a URL that is not in it: also 404, and
	// still no fetch. Same status as the cross-user case ON PURPOSE — the
	// endpoint must not become an oracle that distinguishes "not your message"
	// from "no such image in it".
	if rec := serve(handler, "GET",
		"/jmap/proxy/"+a.UserID.String()+"/"+a.MessageID.String()+"/"+token); rec.Code != http.StatusNotFound {
		t.Errorf("proxy A's own message with an unknown token: status %d, want 404", rec.Code)
	}
}

// agentKeySeed is one user's API key with an audit row and a pending request.
type agentKeySeed struct{ keyID, name, pendingID, summary string }

func seedAgentKey(t *testing.T, h *rlsHarness, u rlsUser) agentKeySeed {
	t.Helper()
	ctx := context.Background()
	keyID := store.NewULID().String()
	s := agentKeySeed{keyID: keyID, name: "agent-" + keyID,
		pendingID: store.NewULID().String(), summary: "Move 1 message to Trash for " + keyID}
	hash := sha256.Sum256([]byte("token-" + s.keyID))
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO app.api_keys (id, user_id, name, token_hash, scopes) VALUES ($1,$2,$3,$4,ARRAY['mail:read'])`,
			[]any{s.keyID, u.UserID.String(), s.name, hash[:]}},
		{`INSERT INTO app.api_key_events (id, user_id, key_id, method, outcome, cost) VALUES ($1,$2,$3,'Email/get','ok',1)`,
			[]any{store.NewULID().String(), u.UserID.String(), s.keyID}},
		{`INSERT INTO app.api_key_pending (id, user_id, key_id, kind, method, payload, summary, expires_at)
		  VALUES ($1,$2,$3,'delete','Email/set','{"destroy":[]}',$4, now() + interval '1 day')`,
			[]any{s.pendingID, u.UserID.String(), s.keyID, s.summary}},
	} {
		if _, err := h.super.Exec(ctx, q.sql, q.args...); err != nil {
			t.Fatalf("seed agent key: %v", err)
		}
	}
	return s
}

func superScan2(t *testing.T, h *rlsHarness, sql string, args []any, dest ...any) {
	t.Helper()
	if err := h.super.QueryRow(context.Background(), sql, args...).Scan(dest...); err != nil {
		t.Fatalf("superScan2: %v", err)
	}
}
