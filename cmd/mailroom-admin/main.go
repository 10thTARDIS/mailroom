// SPDX-License-Identifier: AGPL-3.0-only

// Command mailroom-admin is the operator CLI for 0.5 "config-level accounts"
// (spec §16 onboarding). It connects as the application role (mailroom_app)
// via store.Open, so every statement runs under RLS.
//
// Subcommands:
//
//	add-account   --user-id --name --imap-host [--imap-port] --imap-user
//	              (--imap-pass | --imap-pass-file) --smtp-host [--smtp-port]
//	              [--smtp-user] [--smtp-pass|--smtp-pass-file] [--provider-profile]
//	              [--ingest-only]   (ADR-028: create the account already
//	              shadowed — it syncs but Mailroom never mutates its remote
//	              mailbox until writeback is enabled)
//	list-accounts --user-id
//	accounts-report --user-id [--accounts-file FILE] [--fail-on-drift]
//	                   (#538: the accounts file is reconciled ADDITIVELY at boot —
//	                   an existing account's endpoints/credentials/sync_enabled are
//	                   never touched, only identities are still added — so this
//	                   reports every place the database has drifted from what the
//	                   file now declares, and what (if anything) applies the fix.
//	                   Read-only; --fail-on-drift exits non-zero when it finds any.)
//	enable-account  --user-id --account-id [--force]   (mailroom#435: switch sync back on.
//	                Dials IMAP with the STORED credentials first and refuses on failure
//	                unless --force. The only supported way out of sync_enabled=false —
//	                boot never re-validates an account it already has.)
//	disable-account --user-id --account-id             (mailroom#435: pause an account)
//	set-account-credentials --user-id --account-id
//	                   (--password-file F | --from-accounts-file --account NAME)
//	                   [--username U] [--smtp-too]
//	                   (mailroom#571: the automation path for a password rotation
//	                   accounts-report can only SEE (`password: DIFFERS`) — #538
//	                   deliberately never applies one at boot. Validates the new
//	                   credential against IMAP FIRST, through the exact same
//	                   onboarding.ValidateAccount call Account/set's connection
//	                   change makes, and re-seals it ONLY on success; a rejected
//	                   credential leaves the stored one byte-identical and this
//	                   exits non-zero. A credential that already matches what is
//	                   stored is reported and exits 0 without writing. Never
//	                   accepts a password on the command line. #655: with
//	                   --password-file, an account whose stored SMTP login
//	                   differs from its IMAP login KEEPS the SMTP pair unless
//	                   --smtp-too; the report before the write says which of
//	                   IMAP/SMTP will change, and any SMTP credential being
//	                   written is first checked by an AUTH-only SMTP dial.)
//	sync-once     --user-id
//	gmail-census  --user-id --account-id [--limit N] [--top N] [--skip-dedup]
//	              [--budget-mb N]
//	              (#52, ADR-047: READ-ONLY report before a Gmail account's first
//	              import — messages and bytes in All Mail/Spam/Trash, label
//	              mapping, years, days at the daily budget, dedup preview. No
//	              body is downloaded and no label folder is selected; it runs
//	              while the Gmail sync hold stands.)
//	resnippet     --user-id [--apply] [--batch N]
//	rescore       --user-id [--apply] [--batch N]   (mailroom#157 signals + scores)
//	train-labels  --user-id [--apply]               (mailroom#169 per-label classifier report)
//	priority-report --user-id [--model N] [--since DAYS]
//	                   (mailroom#842: how engaged mail ranks under the cold start and the
//	                   trained model at the 0.6 line, per signal source, with the verdict
//	                   the promotion gate would reach today. Read-only; counts only.)
//	priority-reply-backfill --user-id [--apply]
//	                   (mailroom#842: record the reply signal for replies sent from ANY
//	                   client. Report-only by DEFAULT; once per original, so idempotent.)
//	priority-train  --user-id [--apply]
//	                   (mailroom#842: run the daily priority pass now. Report-only by
//	                   DEFAULT: the gate's verdict and whether it would promote, keep the
//	                   cold start, or revert the active model; --apply runs the pass.)
//	priority-rescore --user-id [--since DAYS] [--apply]
//	                   (mailroom#842: re-score recent mail with the model in service. Dry
//	                   run by DEFAULT: how many change band and become important.)
//	classify      --user-id                         (mailroom#175 kind distribution report)
//	repair-sent-drafts --user-id [--apply] [--batch N]   (mailroom#146 cleanup)
//	provision-identity --user-id (--domain D | --address A) [--account-id] [--apply]
//	backfill-alias     --user-id [--apply] [--batch N] [--samples N]  (mailroom#177)
//	repair-contacts    --user-id [--apply] [--batch N]                (mailroom#177)
//	backfill-function-signals --user-id [--apply] [--batch N]         (mailroom#156)
//	induce             --user-id [--limit N] [--top N]   (what Rule/induce would propose)
//	suggest-report     --user-id [--limit N]   (mailroom#242 rule-derived label suggestions, read-only)
//	suggest-rules      --user-id [--limit N] [--min-covers N] [--max-collateral N] [--apply]
//	                   [--priority N] [--backfill] [--max-mutations N]
//	                   (mailroom#242: persist rule-derived suggestions; --backfill writes
//	                   suggestion rows for EXISTING unlabelled mail, not just new arrivals.
//	                   RUN accept-rules FIRST, then this for the remainder — mailroom#294)
//	pgp-report         --user-id [--batch N] [--keys-only]   (mailroom#47 step 0: signing/encryption
//	                   prevalence; mailroom#795: own and held keys by expiry bucket. Read-only)
//	pgp-verify         --user-id [--batch N] [--show N]   (mailroom#47 6a: do signatures check out, read-only)
//	pgp-generate       --user-id --email [--name] [--expires-days N] [--print-public] [--socket]
//	                   (mailroom#47 step 2; keys expire after 730 days by default, 0 = never, mailroom#795)
//	pgp-import         --user-id --file PATH [--passphrase-file F] [--socket]                      (mailroom#47 step 2)
//	pgp-list           --user-id                                               (mailroom#47 step 2, read-only)
//	pgp-bind           --user-id --key-id --identity-id [--socket]             (mailroom#47 step 2)
//	pgp-publish        --user-id --key-id      (mailroom#48: put a bound key in the WKD)
//	pgp-unpublish      --user-id --key-id      (mailroom#48: withdraw it again)
//	pgp-retire         --user-id --key-id [--unbind]   (mailroom#795: decrypt-only, unpublished;
//	                   refuses an identity's bound default unless --unbind)
//	pgp-extend         --user-id --key-id --days N [--socket]   (mailroom#795: re-sign with expiry
//	                   now+N days; same fingerprint)
//	pgp-revoke         --user-id --key-id --reason compromised|superseded|retired --confirm LAST8 [--print-public]
//	                   [--socket]   (mailroom#795: real OpenPGP revocation. Irreversible.)
//	kind-sweep         --user-id [--limit N]   (mailroom#175 threshold sweep over real mail)
//	untangle-labels    --user-id --plan FILE [--apply] [--batch N]   (mailroom#219 batch-triage clean-up)
//	tabs               --user-id [--batch N]   (mailroom#219 category-tab distribution, read-only)
//	area-report        --user-id [--plan FILE] [--threshold 0.5] [--batch N] [--verbose]
//	                   (#442 increment 0: how every live message would resolve into an
//	                   area under a proposed plan. Read-only; there is no --apply.)
//	area-plan          --user-id --plan FILE [--apply]   (#442 increment 3: load an area
//	                   plan through the one place that can until the desk client exists —
//	                   diffs the file against the STORED areas and creates/updates; never
//	                   deletes a stored area the plan omits.)
//	materialize-area-labels --user-id [--apply] [--batch N] [--verbose]
//	                   (#442 increment 3: write an area's own label onto existing mail
//	                   that only landed there via its tab verdict — "areas are labels".
//	                   Report-only by default; --apply labels the would-label population.)
//	tab-eval           --user-id [--batch N]   (mailroom#219 tab classifier replay + acceptance test)
//	map-label-tabs     --user-id --plan FILE [--apply]   (mailroom#219 point labels at tabs)
//	classify-tabs      --user-id [--apply] [--batch N]   (mailroom#228 fill in stored tab verdicts)
//	blob-report        [--user U] [--verbose]   (mailroom#300 blob/file reconciliation, read-only)
//	mailbox-roles      [--user U] [--verbose]   (#351: which SPECIAL-USE roles a provider
//	                   actually advertises. Read-only; EXITS NON-ZERO when a required role
//	                   (drafts, sent) is missing, so it can gate a provider cutover.)
//	attachment-report  [--user U] [--verbose] [--apply]   (#333 increment 0: measure the
//	                   attachment corpus. Report-only by DEFAULT; --apply repairs only
//	                   has_attachments rows that disagree with their own bytes, #377.)
//	backfill-model-blobs [--user U] [--apply] [--verbose]
//	                   (mailroom#316: the app.blobs row model artifacts written before the fix
//	                   never got. REQUIRED alongside that fix, not cleanup — Prune now refuses
//	                   to delete bytes with no row, so an un-backfilled artifact leaks forever.)
//	accept-rules       --user-id [--limit N] [--min-covers N] [--max-collateral N]
//	                   [--priority N] [--apply]   (mailroom#294 accept induced rules)
//	apply-rules        --user-id [--limit N] [--rule R] [--apply] [--undo]
//	                   [--max-mutations N] [--show-conflicts]   (mailroom#191/#193)
//	delete-label       --user-id --name N [--apply]
//	diff-duplicates    --user-id [--groups N] [--show N] [--context N]   (mailroom#206)
//	backfill-dedup-hash --user-id [--apply] [--dry-run] [--batch N] [--recompute]   (mailroom#206)
//	collapse-duplicates --user-id [--apply] [--dry-run] [--limit N]   (mailroom#206)
//	collapse-provenance --user-id [--apply] [--show N]   (mailroom#859 label attribution
//	                   lost by past collapses: report per evidence kind and applied_by,
//	                   plus the bound it cannot see. Report-only by DEFAULT; --apply
//	                   re-attributes 'user' rows carrying a rule id to 'rule', one Email
//	                   change row per message.)
//	auth-report        --user-id [--batch N] [--examples N]   (mailroom#55 sender auth, read-only)
//	                   [--authserv]: per authserv-id (anywhere / topmost / trusted) and per
//	                   received month (no header, trusted, trusted on top, untrusted only)
//	css-report         --user-id [--top N] [--batch N]   (mailroom#55 CSS sanitizer corpus)
//	pixel-report       --user-id [--top N] [--batch N]   (mailroom#55 tracking pixels)
//	proxy-report       --user-id [--top N] [--batch N] [--resolve] [--fetch] [--fetch-max N]
//	                   (mailroom#55 remote-image hosts, read-only)
//	sender-report      --user [--verbose]   (mailroom#397 sender warnings)
//	contact-dates-report --user-id [--sample N] [--batch N]   (mailroom#816 stored vs true
//	                   first/last mail per contact address, read-only)
//	contact-dates-backfill --user-id [--apply] [--list] [--contacts-per-batch N]
//	                   (mailroom#816 sets first/last seen to the true mail dates and saves
//	                   written-to contacts. Report-only by DEFAULT; one Contact change row
//	                   per changed contact.)
//	contact-names-clean --user-id [--apply] [--sample N]   (mailroom#831 strips bidi/control/
//	                   zero-width characters from learned contact display names. Report-only
//	                   by DEFAULT; one Contact change row per changed contact.)
//	export-report      --user-id   (mailroom#54 per-table export disposition, read-only)
//	delete-user        --user-id [--apply] [--confirm-user-id U] [--actor A] [--accept-key-loss]
//	                   (mailroom#54 hard delete. Report-only by DEFAULT; the extra
//	                   confirmations exist because this is irreversible.)
//	backfill-attachments --user [--apply]   (mailroom#396 attachment rows)
//	quarantine         --user U [--release msgID:part] [--revoke msgID:part] [--batch N]
//	                   (ADR-027 §8: what the download gate refuses, and the two
//	                   decisions that change it. Read-only with neither flag.)
//	thread-report      --user U [--apply]   (mailroom#210: conversations split apart by a
//	                   shared Message-ID. Read-only by default; --apply merges them.)
//	key-discover       --user U [--apply] [--limit N]   (mailroom#48: look up correspondents'
//	                   published keys over WKD. Read-only by default.)
//	pgp-export-wrapped --user-id --key-id [--passphrase-file F] [--out F]
//	                   (mailroom#54: the ONLY route to a private key; without it the
//	                   user's encrypted mail is permanently unreadable.)
//	day-one            --user-id [--account-id A] [--window 30] [--plan FILE] [--apply]
//	                   [--undo RUN_ID] [--runs] [--batch N]
//	                   (mailroom#446: turn a folder-sorted mailbox into a desk. Prints the
//	                   FOLDER CENSUS and then the preview of the plan — per folder and in
//	                   total — and writes nothing. --apply creates the run and drives its
//	                   chunks inline; --undo reverses one from its journal; --runs lists
//	                   them. NOTHING MOVES ON THE SERVER except a folder the plan explicitly
//	                   marks bringToDesk.)
//	day-one-server-archive --user-id [--apply] [--limit N] [--batch N]
//	                   (mailroom#934: move day-one's Mailroom-only archives that the server
//	                   still shows in INBOX to the account's Archive folder, as a user
//	                   archive would. Read-only by default; --limit caps an apply.)
//	sieve-report       --user-id [--account NAME] [--show-active] [--host H] [--port P]
//	                   [--implicit-tls] [--timeout D]
//	                   (mailroom#50: what each account's ManageSieve server holds before
//	                   Mailroom writes anything — capabilities (vacation? include?), every
//	                   script with its size and which is active, and a STRUCTURAL summary
//	                   of the active one. Never prints a body unless --show-active. The
//	                   client can only send read verbs; there is no --apply.)
//	from-field-report  --user-id [--batch N] [--examples N]
//	                   (mailroom#754: how many messages carry no From, several From fields,
//	                   an empty one beside a real one, a group or several mailboxes — and,
//	                   per shape, how often learned placement acted on them and the reading
//	                   pane showed an auth notice or a trusted DMARC pass. Counts and
//	                   message ids only; read-only.)
//	nudge-report       --user-id [--since 180d] [--examples]
//	                   (mailroom#58: how accurate follow-up nudges would be on the
//	                   user's own history — per-rule precision with its Wilson lower
//	                   bound, recall, and nudges per week, inbound and outbound.
//	                   Read-only; message ids only, with --examples.)
//	placement-report   --user-id [--reset-gate AREA_ID]
//	                   (mailroom#714: learned placement. Confident keys per area, every
//	                   placement's outcome, precision with its Wilson lower bound, the
//	                   correction gate per area, abstentions by reason, and what a
//	                   backfill of existing mail WOULD move (an upper bound), and
//	                   arrival vs backfill outcomes. Counts only, read-only;
//	                   --reset-gate re-enables an area the gate stopped. No --apply.)
//	placement-explain  --user-id (--message-id M | --sender ADDRESS [--limit N]) [--as-of]
//	                   (mailroom#840: why learned placement did or did not place a
//	                   message. Re-runs the arrival decision read-only with the very
//	                   functions ingest uses and prints the sender parse, the keys, the
//	                   evidence per key, the decision, every sender check with its
//	                   inputs (the trusted Authentication-Results clauses and their
//	                   header.from against the key domain, warnings, the tab verdict)
//	                   and the first check that refused. Evidence is as of NOW unless
//	                   --as-of (filings made before the message was ingested). Read-only
//	                   transaction, never committed; prints addresses, never subjects.)
//	placement-backfill --user-id [--exclude ADDRESS-or-DOMAIN ...] [--apply] [--undo BATCH_ID]
//	                   (mailroom#714: apply learned placement to mail already on the
//	                   desk. Report only by default: totals, area pairs, a line per
//	                   key, abstentions by reason, EXACT because every mover's headers
//	                   pass the arrival-time checks. --apply performs that plan as a
//	                   batch; --undo reverses one batch exactly, skipping mail the user
//	                   has filed since. Prints sender addresses, never subjects.)
//	drawer-report      --user-id [--apply] [--batch N] [--enable] [--disable]
//	                   (mailroom#443: preview of the drawer auto-archive sweep — what
//	                   it would move to Archive right now, and why the rest is
//	                   excluded, and whether the login-level master switch is
//	                   armed. Report-only by default; --apply performs the move
//	                   regardless of the switch; --enable/--disable flips the
//	                   switch the hourly job checks before archiving anything.)
//	writeback-ledger   --user-id --account-id [--apply]
//	                   (#521, ADR-028 §4: the ingest-only ledger — writeback
//	                   intents an account accumulated as `suppressed` while
//	                   shadowed, broken down per op class. Read-only by default;
//	                   --apply replays them to `pending` and REFUSES while the
//	                   account is still shadowed: nothing re-suppresses an
//	                   already-pending row, so a premature replay would sit
//	                   invisible to this report (which counts only
//	                   `suppressed`) until both switches lift, then fire
//	                   UNREVIEWED on the next drain.)
//	compact-changelog  --user-id [--retention 2160h] [--apply]
//	                   (#526, ADR-003: change_log rows older than the
//	                   retention window (default 90 days, 7-day minimum),
//	                   per object type. Report-only by default, showing the
//	                   TRUE eligible total; --apply drains it in the same
//	                   batches the weekly job uses and raises change_floor
//	                   to the highest seq it deleted.)
//	consolidate-folders --user-id --account-id --from PATTERN --into FOLDER
//	                   [--delete-empty PATTERN]... [--max-folders N] [--apply]
//	                   [--despite-ingest-only]
//	                   (#647: move every message in the folders matching --from
//	                   into --into, then delete the --delete-empty folders once
//	                   empty — ON THE PROVIDER, over IMAP. Report-only by default;
//	                   --apply verifies each move and each delete by STATUS and
//	                   stops on a mismatch. Writes nothing to Mailroom's database:
//	                   sync observes the changes like any other client's.)
//
// ⚠ EVERY SUBCOMMAND ABOVE IS ALSO A KEY IN `subcommands` BELOW, AND A TEST
// ENFORCES THAT IN BOTH DIRECTIONS (mailroom#350). This list is prose — it
// carries each command's flags, which nothing can derive — but its MEMBERSHIP is
// checked, because it had silently drifted by 18 entries before anyone compared
// the two.
//
// Configuration (shared with the server, internal/config + Docker-secret _FILE
// convention):
//
//	MAILROOM_DATABASE_URL[_FILE]       mailroom_app DSN (required)
//	MAILROOM_DATABASE_PASSWORD_FILE    the DSN's password, injected in memory (#537)
//	MAILROOM_MASTER_KEY_FILE           envelope master key (default /run/secrets/mailroom_master_key)
//	MAILROOM_BLOB_ROOT                 blob store root for sync-once (default /var/lib/mailroom/blobs)
//
// The operator user named on the command line must already exist;
// mailroom-admin itself never creates one. It is provisioned one of two ways,
// neither of which is this CLI: at BOOT, for the server's configured operator
// subject and the optional declared-accounts operator (store.ProvisionUser,
// cmd/mailroom); or at LOGIN, under OIDC, the first time an identity that
// completes the auth flow is seen (store.ResolveLogin) — so a user IS a side
// effect of someone logging in, just never of running this tool. enable-account
// (above) is the separate, narrower re-enable for one account's sync_enabled;
// the equivalent for the account's OWNER — re-enabling a disabled user — is not
// a mailroom-admin subcommand either. Account/set with syncEnabled: true
// (urn:mailroom:accounts) is the same re-enable enable-account performs, from
// the JMAP surface instead of this CLI.
package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"math"
	"os"
	"os/signal"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/blob"
	"mailroom/internal/config"
	"mailroom/internal/crypto"
	"mailroom/internal/ingest"
	"mailroom/internal/kind"
	"mailroom/internal/mailsync"
	"mailroom/internal/mime"
	"mailroom/internal/ml"
	"mailroom/internal/nudge"
	"mailroom/internal/oauthmail"
	"mailroom/internal/onboarding"
	"mailroom/internal/server"
	"mailroom/internal/store"
	"mailroom/internal/sync"
	"mailroom/internal/syncrunner"
)

// blobPathInfo is the HKDF label for the blob store's per-user path-derivation
// key (HMAC key, not an encryption key). Derived from the master key so
// mailroom-admin needs no extra secret.
const blobPathInfo = "mailroom-blob-path-v1"

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "mailroom-admin:", err)
		os.Exit(1)
	}
}

// subcommands IS the dispatcher's vocabulary — the single place it is written
// down, and the thing `run` actually dispatches on.
//
// ⚠ IT USED TO BE TWO PLACES AND WAS REALLY THREE (mailroom#350). A `switch`
// dispatched, a hand-maintained `subcommandList` constant rendered the usage
// line, and the package doc above listed the commands with their flags. The
// constant's own comment claimed to be "the one place the dispatcher's
// vocabulary is written down" while the switch was a second, unchecked list —
// and the drift it recorded (`repair-sent-drafts` in neither) was exactly a
// switch/list disagreement, so the mechanism that had already failed once was
// still unguarded.
//
// Measured when this was fixed: the switch and the constant agreed at 51 each,
// but the PACKAGE DOC had drifted to 33 — eighteen subcommands dispatched and
// undocumented, including `quarantine`, `delete-user` and `pgp-export-wrapped`.
// Nobody had ever compared those two.
//
// Now the usage line is DERIVED from this map, so that pair cannot drift at all,
// and TestPackageDocDocumentsEverySubcommand checks the doc comment's membership
// against it in both directions. The doc stays prose — it carries each command's
// flags, which nothing can derive — but it can no longer go quietly stale.
//
// Why it matters more than tidiness: the export archive's manifest.json tells a
// reader to run `mailroom-admin pgp-export-wrapped` to retrieve the private key
// WITHOUT WHICH THEIR ENCRYPTED MAIL IS PERMANENTLY UNREADABLE. A rename that
// missed one list degrades from "a command is missing from --help" to "the only
// documented route to an unrecoverable key points at nothing".
// internal/exporter/limits_test.go guards the archive's half of that string and
// says in its own comment that it cannot guard the command's existence. This is
// the other half.
var subcommands = map[string]func(context.Context, []string) error{
	"add-account":               runAddAccount,
	"list-accounts":             runListAccounts,
	"accounts-report":           runAccountsReport,
	"enable-account":            runEnableAccount,
	"disable-account":           runDisableAccount,
	"set-account-credentials":   runSetAccountCredentials,
	"sync-once":                 runSyncOnce,
	"gmail-census":              runGmailCensus,
	"resnippet":                 runResnippet,
	"rescore":                   runRescore,
	"train-labels":              runTrainLabels,
	"priority-report":           runPriorityReport,
	"priority-reply-backfill":   runPriorityReplyBackfill,
	"priority-rescore":          runPriorityRescore,
	"priority-train":            runPriorityTrain,
	"classify":                  runClassify,
	"repair-sent-drafts":        runRepairSentDrafts,
	"provision-identity":        runProvisionIdentity,
	"backfill-alias":            runBackfillAlias,
	"repair-contacts":           runRepairContacts,
	"induce":                    runInduce,
	"suggest-report":            runSuggestReport,
	"suggest-rules":             runSuggestRules,
	"pgp-report":                runPGPReport,
	"pgp-verify":                runPGPVerify,
	"pgp-generate":              runPGPGenerate,
	"pgp-export-wrapped":        runPGPExportWrapped,
	"pgp-import":                runPGPImport,
	"pgp-list":                  runPGPList,
	"pgp-bind":                  runPGPBind,
	"pgp-publish":               runPGPPublish,
	"pgp-unpublish":             runPGPUnpublish,
	"pgp-retire":                runPGPRetire,
	"pgp-extend":                runPGPExtend,
	"pgp-revoke":                runPGPRevoke,
	"key-discover":              runKeyDiscover,
	"kind-sweep":                runKindSweep,
	"accept-rules":              runAcceptRules,
	"apply-rules":               runApplyRules,
	"diff-duplicates":           runDiffDuplicates,
	"collapse-duplicates":       runCollapseDuplicates,
	"collapse-provenance":       runCollapseProvenance,
	"blob-report":               runBlobReport,
	"backfill-model-blobs":      runBackfillModelBlobs,
	"css-report":                runCSSReport,
	"auth-report":               runAuthReport,
	"sender-report":             runSenderReport,
	"contact-dates-report":      runContactDatesReport,
	"contact-dates-backfill":    runContactDatesBackfill,
	"contact-names-clean":       runContactNamesClean,
	"pixel-report":              runPixelReport,
	"proxy-report":              runProxyReport,
	"export-report":             runExportReport,
	"mailbox-roles":             runMailboxRoles,
	"attachment-report":         runAttachmentReport,
	"backfill-attachments":      runBackfillAttachments,
	"quarantine":                runQuarantine,
	"thread-report":             runThreadReport,
	"delete-user":               runDeleteUser,
	"backfill-dedup-hash":       runBackfillDedupHash,
	"delete-label":              runDeleteLabel,
	"backfill-function-signals": runBackfillFunctionSignals,
	"untangle-labels":           runUntangleLabels,
	"tabs":                      runTabs,
	"area-report":               runAreaReport,
	"area-plan":                 runAreaPlan,
	"materialize-area-labels":   runMaterializeAreaLabels,
	"classify-tabs":             runClassifyTabs,
	"map-label-tabs":            runMapLabelTabs,
	"tab-eval":                  runTabEval,
	"drawer-report":             runDrawerReport,
	"placement-report":          runPlacementReport,
	"placement-explain":         runPlacementExplain,
	"nudge-report":              runNudgeReport,
	"from-field-report":         runFromFieldReport,
	"placement-backfill":        runPlacementBackfill,
	"day-one":                   runDayOne,
	"day-one-server-archive":    runDayOneServerArchive,
	"writeback-ledger":          runWritebackLedger,
	"compact-changelog":         runCompactChangelog,
	"consolidate-folders":       runConsolidateFolders,
	"sieve-report":              runSieveReport,
}

// subcommandList renders the vocabulary for the usage line and the
// unknown-subcommand error. DERIVED from subcommands, never maintained: that is
// what makes "the usage line lists a command that does not dispatch" impossible
// to express rather than merely tested for. Sorted so the output is stable.
func subcommandList() string {
	names := make([]string, 0, len(subcommands))
	for n := range subcommands {
		names = append(names, n)
	}
	sort.Strings(names)
	return strings.Join(names, "|")
}

func run(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: mailroom-admin <" + subcommandList() + "> [flags]")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cmd, rest := args[0], args[1:]
	fn, ok := subcommands[cmd]
	if !ok {
		return fmt.Errorf("unknown subcommand %q; want %s", cmd, subcommandList())
	}
	return fn(ctx, rest)
}

// deps bundles the shared runtime dependencies.
type deps struct {
	cfg   *config.Config
	log   *slog.Logger
	keys  crypto.KeyProvider
	store *store.Store
	close func()
	// allowPrivateAccountHosts is MAILROOM_ALLOW_PRIVATE_ACCOUNT_HOSTS (#527),
	// threaded through to onboarding.WithAllowPrivateHosts on every path here
	// that validates an account's IMAP/SMTP endpoints (add-account,
	// enable-account's re-validation, #624 §2). Without it this CLI's egress
	// guard was permanently stuck at the safe default no matter what the
	// operator configured for the server — a LAN mail server that Account/set
	// and the server's own onboarding could reach was refused here regardless.
	allowPrivateAccountHosts bool
	// imapSearchRate is MAILROOM_IMAP_SEARCH_RATE (#638), threaded through to
	// the sync runner's credential resolver for the same reason
	// allowPrivateAccountHosts is: a one-shot `sync` from this CLI issues the
	// very same UID SEARCHes the server's pass does, against the very same
	// per-account budget, so an unpaced CLI run would be refused exactly as the
	// unpaced server was.
	imapSearchRate int
}

// openDeps loads config, builds a logger, and opens the store.
func openDeps(ctx context.Context) (*deps, error) {
	cfg, err := config.Load()
	if err != nil {
		return nil, err
	}
	logger := server.NewLogger(cfg.LogFormat, cfg.LogLevel)
	st, err := store.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return nil, fmt.Errorf("open store: %w", err)
	}
	// The instance-level ingest-only kill switch (ADR-028 §2,
	// MAILROOM_WRITEBACK_DISABLED) is only honoured by whichever process calls
	// Store.SetWritebackDisabled — it is not read from the environment inside
	// EnqueueIntent itself. Before this call this CLI's UserTx never saw it, so
	// apply-rules/untangle-labels/etc. would enqueue 'pending' (not
	// 'suppressed') on an account shadowed only by the instance flag, and the
	// whole backlog would fire the instant the flag is dropped at cutover —
	// exactly the accumulate-then-fire hazard ADR-028 exists to prevent.
	st.SetWritebackDisabled(cfg.WritebackDisabled)
	return &deps{
		cfg:                      cfg,
		log:                      logger,
		keys:                     crypto.NewFileKeyProvider(cfg.MasterKeyFile),
		store:                    st,
		close:                    st.Close,
		allowPrivateAccountHosts: cfg.AllowPrivateAccountHosts,
		imapSearchRate:           cfg.IMAPSearchRate,
	}, nil
}

// addAccountOnboardingOptions is add-account's full onboarding.Option set,
// built from *deps in ONE place (#624 §2). Before this existed, add-account
// passed onboarding.WithLogger and nothing else — MAILROOM_ALLOW_PRIVATE_
// ACCOUNT_HOSTS was parsed by config.Load and never reached this CLI's own
// egress check, so a LAN account this operator explicitly allowed for the
// server (cmd/mailroom) and Account/set (internal/api) was refused here
// regardless. A seam function rather than the options written inline at the
// call site so the exact thing runAddAccount passes is also the exact thing
// TestAddAccountHonorsAllowPrivateAccountHosts drives.
func addAccountOnboardingOptions(d *deps) []onboarding.Option {
	return []onboarding.Option{
		onboarding.WithLogger(d.log),
		onboarding.WithAllowPrivateHosts(d.allowPrivateAccountHosts),
	}
}

// syncResolverOptions is the one-shot `sync` command's credential-resolver
// option set, built from *deps in ONE place for the same reason
// addAccountOnboardingOptions is: MAILROOM_ALLOW_PRIVATE_ACCOUNT_HOSTS must
// reach the IMAP dial this CLI makes, not only the validation check (#625 §3).
// Since the sync dial is guarded, forgetting it here would refuse a LAN account
// the operator explicitly allowed; TestSyncResolverHonorsAllowPrivateAccountHosts
// drives this exact function.
func syncResolverOptions(d *deps) []syncrunner.ResolverOption {
	return []syncrunner.ResolverOption{
		syncrunner.WithSearchRate(d.imapSearchRate),
		syncrunner.WithAllowPrivateHosts(d.allowPrivateAccountHosts),
		// The Gmail sync model's budget and sweep (#52): a Gmail account synced
		// from this CLI is held to the same daily budget as the server's.
		syncrunner.WithGmailSync(d.gmailBudgetBytes(), d.gmailLabelSweep()),
	}
}

// gmailBudgetBytes is the configured Gmail daily download budget in bytes
// (the default when no configuration was loaded, as in unit tests).
func (d *deps) gmailBudgetBytes() int64 {
	if d.cfg == nil || d.cfg.GmailDailyDownloadMB <= 0 {
		return int64(config.DefaultGmailDailyDownloadMB) * 1000 * 1000
	}
	return int64(d.cfg.GmailDailyDownloadMB) * 1000 * 1000
}

// gmailLabelSweep is the configured Gmail label-sweep interval.
func (d *deps) gmailLabelSweep() time.Duration {
	if d.cfg == nil {
		return config.DefaultGmailLabelSweepInterval
	}
	return d.cfg.GmailLabelSweepInterval
}

func runAddAccount(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("add-account", flag.ContinueOnError)
	var (
		userIDStr    = fs.String("user-id", "", "operator user id (ULID); must already exist")
		name         = fs.String("name", "", "account name, unique per user")
		imapHost     = fs.String("imap-host", "", "IMAP host (e.g. imap.example.com)")
		imapPort     = fs.Int("imap-port", 993, "IMAP port (implicit TLS)")
		imapUser     = fs.String("imap-user", "", "IMAP username (e.g. you@example.com)")
		imapPass     = fs.String("imap-pass", "", "IMAP/SMTP password (prefer --imap-pass-file)")
		imapPassFile = fs.String("imap-pass-file", "", "file holding the password (Docker-secret style)")
		smtpHost     = fs.String("smtp-host", "", "SMTP host (e.g. smtp.example.com)")
		smtpPort     = fs.Int("smtp-port", 465, "SMTP port (implicit TLS)")
		smtpUser     = fs.String("smtp-user", "", "SMTP username (default: --imap-user)")
		smtpPass     = fs.String("smtp-pass", "", "SMTP password (default: IMAP password)")
		smtpPassFile = fs.String("smtp-pass-file", "", "file holding the SMTP password")
		profile      = fs.String("provider-profile", "generic", "provider profile (generic|migadu|gmail|...)")
		ingestOnly   = fs.Bool("ingest-only", false, "create the account already shadowed (ADR-028): it syncs normally but Mailroom never mutates its remote mailbox")
	)
	if err := fs.Parse(args); err != nil {
		return err
	}
	userID, err := parseUserID(*userIDStr)
	if err != nil {
		return err
	}
	pass, err := resolveSecretFlag(*imapPass, *imapPassFile)
	if err != nil {
		return fmt.Errorf("imap password: %w", err)
	}
	if pass == "" {
		return errors.New("add-account: --imap-pass or --imap-pass-file is required")
	}
	smtpPassword, err := resolveSecretFlag(*smtpPass, *smtpPassFile)
	if err != nil {
		return fmt.Errorf("smtp password: %w", err)
	}

	d, err := openDeps(ctx)
	if err != nil {
		return err
	}
	defer d.close()

	// --ingest-only is a POINTER, not a plain bool, for the same reason
	// store.AccountRow.WritebackEnabled is: the pointer's zero value (nil) must
	// mean "schema default (true, normal writeback)", not "shadowed". Leaving
	// it nil when the flag is absent lets CreateAccount's COALESCE apply the
	// schema default rather than this CLI hard-coding true.
	var writebackEnabled *bool
	if *ingestOnly {
		v := false
		writebackEnabled = &v
	}

	res, err := onboarding.AddAccount(ctx, onboarding.NewStoreAdapter(d.store), d.keys, userID,
		onboarding.AccountSpec{
			Name:             *name,
			ProviderProfile:  *profile,
			IMAPHost:         *imapHost,
			IMAPPort:         *imapPort,
			SMTPHost:         *smtpHost,
			SMTPPort:         *smtpPort,
			Username:         *imapUser,
			Password:         pass,
			SMTPUsername:     *smtpUser,
			SMTPPassword:     smtpPassword,
			WritebackEnabled: writebackEnabled,
			TLS:              sync.TLSImplicit,
		},
		addAccountOnboardingOptions(d)...,
	)
	if err != nil {
		// mailroom#435: say which state the account was LEFT in, because the
		// two need different things from the operator.
		if res.AccountID != (ulid.ULID{}) {
			if res.SyncEnabled {
				fmt.Printf("account %s stored; validation failed (%s) but sync stays ENABLED — the sync loop will retry with backoff\n",
					res.AccountID, onboarding.FailureKind(err))
			} else {
				fmt.Printf("account %s stored but sync DISABLED: the server rejected it (%s); fix the credentials, then enable-account\n",
					res.AccountID, onboarding.FailureKind(err))
			}
		}
		return err
	}
	fmt.Printf("account %s added and validated (%d mailboxes discovered)\n", res.AccountID, res.MailboxesSeen)
	if *ingestOnly {
		fmt.Println("  created SHADOWED (--ingest-only): it will sync but Mailroom will never mutate its remote mailbox until writeback is enabled (Account/set writebackEnabled=true)")
	}
	return nil
}

// runEnableAccount switches an account's sync back on (mailroom#435). It is the
// supported way out of sync_enabled=false, which until now meant editing the
// row in SQL: boot reconciles declared accounts BY NAME and never re-validates
// one it already has, so an account that a first-boot failure switched off
// stayed off through every restart, container replacement and deploy.
//
// It dials IMAP with the STORED credentials first and refuses to enable on a
// failure, naming the kind, unless --force is given: an operator running this
// is present and can act on "the server still rejects the password", whereas
// enabling blindly would only move the failure into the sync loop's log.
// --force is for the case where the operator knows better — the provider is
// down right now, say — and wants the loop's backoff to pick the account up
// when it can.
func runEnableAccount(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("enable-account", flag.ContinueOnError)
	userIDStr := fs.String("user-id", "", "operator user id (ULID)")
	accountIDStr := fs.String("account-id", "", "account id (ULID; see list-accounts)")
	force := fs.Bool("force", false, "enable even if IMAP validation fails now; the sync loop will retry with backoff")
	if err := fs.Parse(args); err != nil {
		return err
	}
	userID, err := parseUserID(*userIDStr)
	if err != nil {
		return err
	}
	accountID, err := ulid.Parse(*accountIDStr)
	if err != nil {
		return fmt.Errorf("enable-account: --account-id: %w", err)
	}

	d, err := openDeps(ctx)
	if err != nil {
		return err
	}
	defer d.close()

	var acct store.Account
	if err := d.store.UserTx(ctx, userID, func(tx *store.UserTx) error {
		var e error
		acct, e = tx.GetAccount(ctx, accountID)
		return e
	}); err != nil {
		return fmt.Errorf("enable-account: %w", err)
	}
	if acct.SyncEnabled {
		fmt.Printf("account %s (%s) already has sync enabled; nothing to do\n", acct.ID, acct.Name)
		return nil
	}
	// The Gmail sync hold (#52, oauthmail.SyncHold): not even --force lifts
	// it; the switch is a code constant that the Gmail sync model flips.
	if code, msg, held := oauthmail.SyncHold(acct.AuthKind, acct.ProviderProfile); held {
		return fmt.Errorf("enable-account: account %s (%s): sync cannot be switched on yet: %s (%s)",
			acct.ID, acct.Name, msg, code)
	}

	// Network I/O, so OUTSIDE any transaction (onboarding.ValidateAccount's contract).
	n, verr := validateStoredAccount(ctx, d.keys, userID, acct, d.allowPrivateAccountHosts)
	switch {
	case verr == nil:
		fmt.Printf("account %s (%s): IMAP validation succeeded (%d mailboxes)\n", acct.ID, acct.Name, n)
	case *force:
		fmt.Printf("account %s (%s): IMAP validation FAILED (%s): %v\n  enabling anyway (--force); the sync loop will retry with backoff and log each failure\n",
			acct.ID, acct.Name, onboarding.FailureKind(verr), verr)
	default:
		fmt.Printf("account %s (%s): IMAP validation FAILED (%s): %v\n  NOT enabled. Fix the credentials or endpoint, or pass --force to enable anyway.\n",
			acct.ID, acct.Name, onboarding.FailureKind(verr), verr)
		return fmt.Errorf("enable-account: validation failed: %w", verr)
	}

	if err := d.store.UserTx(ctx, userID, func(tx *store.UserTx) error {
		return tx.SetAccountSyncEnabled(ctx, accountID, true)
	}); err != nil {
		return fmt.Errorf("enable-account: %w", err)
	}
	fmt.Printf("account %s (%s): sync ENABLED\n", acct.ID, acct.Name)
	return nil
}

// runDisableAccount pauses an account's sync — enable-account's counterpart, so
// the pair is symmetric and neither needs SQL (mailroom#435). The row and its
// mail stay; the sync loop stops listing the account and names it at WARN on
// every pass until it is enabled again.
func runDisableAccount(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("disable-account", flag.ContinueOnError)
	userIDStr := fs.String("user-id", "", "operator user id (ULID)")
	accountIDStr := fs.String("account-id", "", "account id (ULID; see list-accounts)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	userID, err := parseUserID(*userIDStr)
	if err != nil {
		return err
	}
	accountID, err := ulid.Parse(*accountIDStr)
	if err != nil {
		return fmt.Errorf("disable-account: --account-id: %w", err)
	}

	d, err := openDeps(ctx)
	if err != nil {
		return err
	}
	defer d.close()

	var acct store.Account
	if err := d.store.UserTx(ctx, userID, func(tx *store.UserTx) error {
		var e error
		if acct, e = tx.GetAccount(ctx, accountID); e != nil {
			return e
		}
		return tx.SetAccountSyncEnabled(ctx, accountID, false)
	}); err != nil {
		return fmt.Errorf("disable-account: %w", err)
	}
	fmt.Printf("account %s (%s): sync DISABLED; the sync loop will name it at WARN each pass until enable-account\n", acct.ID, acct.Name)
	return nil
}

// validateStoredAccount re-derives the account's credentials key, opens the
// sealed secret, and runs the same IMAP validation onboarding does for a new
// account — the check enable-account gates on. It mirrors
// syncrunner.CredentialResolver.Prepare's view of a stored row (implicit TLS on
// the stored host/port; app.accounts carries no TLS-mode column).
func validateStoredAccount(ctx context.Context, keys crypto.KeyProvider, userID ulid.ULID, acct store.Account, allowPrivate bool) (int, error) {
	master, err := keys.MasterKey(ctx)
	if err != nil {
		return 0, fmt.Errorf("master key: %w", err)
	}
	defer crypto.Zero(master)
	creds, err := onboarding.OpenCredentials(master, userID, acct.CredentialsCT)
	if err != nil {
		return 0, fmt.Errorf("open stored credentials: %w", err)
	}
	return onboarding.ValidateAccount(ctx, nil, onboarding.AccountSpec{
		Name:            acct.Name,
		ProviderProfile: acct.ProviderProfile,
		IMAPHost:        acct.IMAPHost,
		IMAPPort:        acct.IMAPPort,
		SMTPHost:        acct.SMTPHost,
		SMTPPort:        acct.SMTPPort,
		Username:        creds.IMAPUsername,
		Password:        creds.IMAPPassword,
		SMTPUsername:    creds.SMTPUsername,
		SMTPPassword:    creds.SMTPPassword,
		TLS:             sync.TLSImplicit,
	},
		// #624 §2: re-validation must honor the same egress policy add-account
		// used, or a re-enable can refuse a LAN account the operator explicitly
		// allowed in.
		onboarding.WithAllowPrivateHosts(allowPrivate),
	)
}

func runListAccounts(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("list-accounts", flag.ContinueOnError)
	userIDStr := fs.String("user-id", "", "operator user id (ULID)")
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

	var accts []store.Account
	if err := d.store.UserTx(ctx, userID, func(tx *store.UserTx) error {
		var e error
		accts, e = tx.ListAccounts(ctx)
		return e
	}); err != nil {
		return err
	}
	if len(accts) == 0 {
		fmt.Println("no accounts")
		return nil
	}
	for _, a := range accts {
		fmt.Printf("%s  %-24s imap=%s:%d smtp=%s:%d profile=%s sync=%t\n",
			a.ID, a.Name, a.IMAPHost, a.IMAPPort, a.SMTPHost, a.SMTPPort, a.ProviderProfile, a.SyncEnabled)
	}
	return nil
}

func runSyncOnce(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("sync-once", flag.ContinueOnError)
	userIDStr := fs.String("user-id", "", "operator user id (ULID)")
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

	// A live sync ingests mail learned placement may file (#714), so it
	// believes the same Authentication-Results identities the server does.
	// The entries go in raw: ingest resolves date-bounded ones against each
	// message's received time (#766).
	authservEntries, err := trustedAuthservEntries()
	if err != nil {
		return err
	}
	engine := mailsync.New(mailsync.NewStoreAdapter(d.store),
		ingest.New(d.store, blobs, ingest.WithTrustedAuthservIDs(authservEntries)), nil)
	resolver := syncrunner.NewCredentialResolver(d.keys, syncResolverOptions(d)...)
	runner := syncrunner.New(syncrunner.NewStoreLister(d.store), engine, resolver.Prepare,
		syncrunner.WithLogger(d.log))

	sum, err := runner.SyncOnce(ctx, userID)
	if err != nil {
		return err
	}
	fmt.Printf("sync-once: accounts=%d disabled=%d synced=%d failed=%d messages=%d flag_updates=%d bodies_fetched=%d body_bytes_fetched=%d\n",
		sum.Accounts, sum.Disabled, sum.Synced, sum.Failed, sum.MessagesIngested, sum.FlagUpdates,
		sum.BodiesFetched, sum.BodyBytesFetched)
	for _, ae := range sum.Errors {
		fmt.Printf("  FAILED %s (%s): %v\n", ae.AccountID, ae.Name, ae.Err)
	}
	if sum.Failed > 0 {
		return fmt.Errorf("sync-once: %d account(s) failed", sum.Failed)
	}
	return nil
}

// runResnippet regenerates the list-preview snippet for a user's
// already-ingested mail. v0.23.1 fixed the ingest snippet generator (it now
// strips HTML and falls back to the HTML body); rows ingested BEFORE that fix
// keep a stale snippet (raw <style> blocks, or blank for HTML-only mail) that
// incremental sync never re-derives. This command re-derives each snippet from
// the message's stored blob using the SAME generator (ingest.SnippetFromRaw) and
// updates the row.
//
// It defaults to a dry run that only reports how many previews WOULD change;
// -apply (or -dry-run=false) writes. Per-message failures (missing/corrupt blob,
// unreadable bytes) are logged and skipped — one bad message never aborts the
// run. Writing goes through UserTx.UpdateMessageSnippet, which appends an Email
// change_log row only when the value actually changes (the invariant), so a
// re-run is cheap and idempotent.
func runResnippet(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("resnippet", flag.ContinueOnError)
	var (
		userIDStr = fs.String("user-id", "", "operator user id (ULID)")
		apply     = fs.Bool("apply", false, "write regenerated snippets (default: dry-run, report only)")
		dryRun    = fs.Bool("dry-run", true, "report what would change without writing (overridden by -apply)")
		batch     = fs.Int("batch", 500, "message id page size for the keyset scan")
	)
	if err := fs.Parse(args); err != nil {
		return err
	}
	userID, err := parseUserID(*userIDStr)
	if err != nil {
		return err
	}
	// Dry-run is the default; -apply (or an explicit -dry-run=false) switches to
	// writing.
	write := *apply || !*dryRun

	d, err := openDeps(ctx)
	if err != nil {
		return err
	}
	defer d.close()

	blobs, err := openBlobStore(ctx, d)
	if err != nil {
		return err
	}

	// Derive the per-user mail-blob DEK once for the whole run — the same
	// storage-free HKDF derivation (salt=userID, info=MailBlobInfoV1) that the
	// sync and body-serving paths use, so the backfill reads the exact bytes
	// ingest wrote. The key is held for the run and zeroed at the end.
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

	var scanned, changed, unchanged, errCount int
	after := ulid.ULID{}
	for {
		var refs []store.MessageBlobRef
		if err := d.store.UserTx(ctx, userID, func(tx *store.UserTx) error {
			var e error
			refs, e = tx.MessageBlobRefsAfter(ctx, after, *batch)
			return e
		}); err != nil {
			return fmt.Errorf("scan messages: %w", err)
		}
		if len(refs) == 0 {
			break
		}
		for _, ref := range refs {
			after = ref.ID
			scanned++

			raw, err := readMailBlob(ctx, blobs, userID, dek, ref.ContentHash)
			if err != nil {
				errCount++
				d.log.Warn("resnippet: read blob failed", "message", ref.ID, "err", err)
				continue
			}
			newSnippet := ingest.SnippetFromRaw(raw)

			if !write {
				// Dry run: compare against the stored snippet to report what a
				// write WOULD change; the store's guarded UPDATE is the same test.
				if newSnippet != ref.Snippet {
					changed++
				} else {
					unchanged++
				}
				continue
			}

			var didChange bool
			if err := d.store.UserTx(ctx, userID, func(tx *store.UserTx) error {
				var e error
				didChange, e = tx.UpdateMessageSnippet(ctx, ref.ID, newSnippet)
				return e
			}); err != nil {
				errCount++
				d.log.Warn("resnippet: update failed", "message", ref.ID, "err", err)
				continue
			}
			if didChange {
				changed++
			} else {
				unchanged++
			}
		}
		if len(refs) < *batch {
			break
		}
	}

	mode := "dry-run (no writes)"
	if write {
		mode = "apply"
	}
	fmt.Printf("resnippet [%s]: user=%s scanned=%d changed=%d unchanged=%d errors=%d\n",
		mode, userID, scanned, changed, unchanged, errCount)
	return nil
}

// runRescore re-derives the priority signals for existing mail and re-scores it
// (mailroom#157).
//
// Two jobs in one pass, because they need the same expensive thing — the message
// bytes:
//
//  1. BACKFILL. Migration 00016 projected List-Id / List-Unsubscribe / the
//     recipient count / the auth verdict onto the row, but a migration cannot
//     open 500 blobs to populate them. Mail ingested before it therefore reads as
//     "not bulk", which is exactly the wrong default for a corpus that is mostly
//     newsletters.
//  2. RE-SCORE. Scores are computed once at ingest, so mail already in the
//     mailbox keeps whatever the old weights said about it — including the 0.96
//     the broken heuristic gave every daily digest. Re-scoring is also how a
//     weight change gets VALIDATED against real mail instead of intuition.
//
// Dry-run by default, like resnippet. Idempotent: the backfill is a guarded
// UPDATE and the score upserts on (user_id, message_id).
func runRescore(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("rescore", flag.ContinueOnError)
	var (
		userIDStr = fs.String("user-id", "", "operator user id (ULID)")
		apply     = fs.Bool("apply", false, "write the backfilled signals and new scores (default: dry-run)")
		dryRun    = fs.Bool("dry-run", true, "report what would change without writing (overridden by -apply)")
		batch     = fs.Int("batch", 500, "message id page size for the keyset scan")
	)
	if err := fs.Parse(args); err != nil {
		return err
	}
	userID, err := parseUserID(*userIDStr)
	if err != nil {
		return err
	}
	write := *apply || !*dryRun

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

	scorer := ml.NewScorer(ml.NewRegistry(d.store, blobs, d.keys))

	// A histogram is the whole point of the dry run: one number per message tells
	// you nothing, but the SHAPE says whether a threshold can separate anything.
	var (
		scanned, backfilled, rescored, errCount int
		buckets                                 [10]int
		importantAt                             = map[float64]int{0.50: 0, 0.60: 0, 0.70: 0, 0.80: 0}
	)
	after := ulid.ULID{}
	for {
		var refs []store.MessageBlobRef
		if err := d.store.UserTx(ctx, userID, func(tx *store.UserTx) error {
			var e error
			refs, e = tx.MessageBlobRefsAfter(ctx, after, *batch)
			return e
		}); err != nil {
			return fmt.Errorf("scan messages: %w", err)
		}
		if len(refs) == 0 {
			break
		}
		for _, ref := range refs {
			after = ref.ID
			scanned++

			raw, err := readMailBlob(ctx, blobs, userID, dek, ref.ContentHash)
			if err != nil {
				errCount++
				d.log.Warn("rescore: read blob failed", "message", ref.ID, "err", err)
				continue
			}
			parsed, err := mime.Parse(bytes.NewReader(raw))
			if err != nil || parsed == nil {
				errCount++
				d.log.Warn("rescore: parse failed", "message", ref.ID, "err", err)
				continue
			}
			listID := strings.TrimSpace(parsed.Header.Get("List-Id"))
			listUnsub := strings.TrimSpace(parsed.Header.Get("List-Unsubscribe"))
			authFail := ml.AuthResultsFail(parsed.Header.Values("Authentication-Results"))
			recipients := len(parsed.Header.To()) + len(parsed.Header.Cc())

			if err := d.store.UserTx(ctx, userID, func(tx *store.UserTx) error {
				if write {
					changed, e := tx.UpdateMessageSignals(ctx, ref.ID, listID, listUnsub, recipients, authFail)
					if e != nil {
						return e
					}
					if changed {
						backfilled++
					}
				}
				// Facts are rebuilt from the ROW (plus the freshly parsed headers),
				// so this scores exactly what the serve path would score today.
				facts, found, e := tx.MessageMLFacts(ctx, ref.ID)
				if e != nil || !found {
					return e
				}
				sent, received, e := tx.OutboundCountForAddress(ctx, facts.FromAddress)
				if e != nil {
					return e
				}
				score, version, top, e := scorer.ScoreMessage(ctx, userID, ml.MessageFacts{
					SenderAddress:      store.NormalizeAddress(facts.FromAddress),
					SenderDomain:       store.AddressDomain(store.NormalizeAddress(facts.FromAddress)),
					HasCorresponded:    sent > 0,
					OutboundCount:      sent,
					InboundCount:       received,
					Subject:            facts.Subject,
					IsReplyInThread:    strings.TrimSpace(facts.InReplyTo) != "",
					HourOfDayUTC:       facts.ReceivedAt.UTC().Hour(),
					HasAttachments:     facts.HasAttachments,
					AddressedAlias:     store.NormalizeAddress(facts.AddressedAddress),
					RemoteFlagged:      facts.Flagged || facts.Junk,
					IsListMail:         listID != "",
					HasListUnsubscribe: listUnsub != "",
					ToCcCount:          recipients,
					AuthFail:           authFail,
				})
				if e != nil {
					return e
				}
				b := int(score * 10)
				if b > 9 {
					b = 9
				}
				buckets[b]++
				for cut := range importantAt {
					if score >= cut {
						importantAt[cut]++
					}
				}
				if write {
					if e := tx.UpsertTriageScore(ctx, ref.ID, score, version, top); e != nil {
						return e
					}
					rescored++
				}
				return nil
			}); err != nil {
				errCount++
				d.log.Warn("rescore: failed", "message", ref.ID, "err", err)
			}
		}
		if len(refs) < *batch {
			break
		}
	}

	mode := "dry-run (no writes)"
	if write {
		mode = "apply"
	}
	fmt.Printf("rescore [%s]: user=%s scanned=%d backfilled=%d rescored=%d errors=%d\n",
		mode, userID, scanned, backfilled, rescored, errCount)
	fmt.Println("score distribution:")
	for i, n := range buckets {
		fmt.Printf("  %.1f-%.1f  %5d  %s\n", float64(i)/10, float64(i+1)/10, n, strings.Repeat("#", n/5))
	}
	fmt.Println("important at threshold:")
	for _, cut := range []float64{0.50, 0.60, 0.70, 0.80} {
		pct := 0.0
		if scanned > 0 {
			pct = 100 * float64(importantAt[cut]) / float64(scanned)
		}
		fmt.Printf("  >= %.2f  %5d  (%.0f%%)\n", cut, importantAt[cut], pct)
	}
	return nil
}

// runRepairSentDrafts cleans up after mailroom#146: messages that were sent
// while sending still left the draft copy behind in the IMAP Drafts folder.
//
// It does NOT delete anything of the user's. For each stranded message it
// enqueues the expunge of the DRAFTS copy only — the same targeted cleanup the
// send path now performs — and clears the leftover draft flag. The Sent copy,
// and the message itself, are untouched. That is the whole reason this exists
// rather than "just delete them from Drafts": in Mailroom's model both copies
// are ONE message, so destroying it would take the Sent copy with it.
//
// Dry-run by default, like resnippet. Idempotent: once repaired, a message no
// longer matches the query, and the expunge intent's idempotency key collapses a
// repeat anyway.
func runRepairSentDrafts(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("repair-sent-drafts", flag.ContinueOnError)
	var (
		userIDStr = fs.String("user-id", "", "operator user id (ULID)")
		apply     = fs.Bool("apply", false, "queue the expunges (default: dry-run, report only)")
		dryRun    = fs.Bool("dry-run", true, "report what would change without writing (overridden by -apply)")
		batch     = fs.Int("batch", 500, "how many stranded messages to scan")
	)
	if err := fs.Parse(args); err != nil {
		return err
	}
	userID, err := parseUserID(*userIDStr)
	if err != nil {
		return err
	}
	write := *apply || !*dryRun

	d, err := openDeps(ctx)
	if err != nil {
		return err
	}
	defer d.close()

	var stranded []store.StrandedSentDraft
	if err := d.store.UserTx(ctx, userID, func(tx *store.UserTx) error {
		var e error
		stranded, e = tx.StrandedSentDraftIDs(ctx, *batch)
		return e
	}); err != nil {
		return fmt.Errorf("scan stranded sent drafts: %w", err)
	}

	if !write {
		for _, s := range stranded {
			fmt.Printf("  would repair %s\n", s.MessageID)
		}
		fmt.Printf("repair-sent-drafts [dry-run (no writes)]: user=%s stranded=%d\n", userID, len(stranded))
		return nil
	}

	var repaired, flagsCleared, errCount int
	for _, s := range stranded {
		// One transaction per message: a message whose copy cannot be named (no
		// Message-ID) must not roll back the ones that could.
		var copies int
		var cleared bool
		// ONE store call, not the two this used to make (#311). Pairing
		// RetireSentDraftCopy with ClearDraftFlag here meant the change_log
		// invariant depended on the CLI remembering it: on a second run the first
		// drops live Drafts provenance (a `mailboxIds` change) and the second
		// no-ops, leaving a JMAP-visible mutation with no Email row. RepairSentDraft
		// owns that pairing in the store, where the invariant belongs.
		if err := d.store.UserTx(ctx, userID, func(tx *store.UserTx) error {
			var e error
			copies, cleared, e = tx.RepairSentDraft(ctx, s.MessageID, s.AccountID)
			return e
		}); err != nil {
			errCount++
			d.log.Warn("repair-sent-drafts: repair failed", "message", s.MessageID, "err", err)
			continue
		}
		if copies > 0 {
			repaired++
		}
		if cleared {
			flagsCleared++
		}
		d.log.Info("repair-sent-drafts: repaired", "message", s.MessageID, "copies_expunged", copies, "draft_flag_cleared", cleared)
	}

	fmt.Printf("repair-sent-drafts [apply]: user=%s stranded=%d expunges_queued=%d draft_flags_cleared=%d errors=%d\n",
		userID, len(stranded), repaired, flagsCleared, errCount)
	fmt.Println("the expunges are queued as writeback intents; the writeback engine applies them on its next pass")
	return nil
}

// readMailBlob fetches and decrypts a message's raw RFC 5322 bytes from the blob
// store with the caller-supplied per-user mail DEK — the same decrypt path
// compose.ReadRaw / the JMAP body endpoint use. A read error is an AEAD/
// content-hash verification failure: the bytes are NOT trustworthy and are
// surfaced so the caller skips that message rather than writing a wrong preview.
func readMailBlob(ctx context.Context, blobs blob.BlobStore, userID ulid.ULID, dek, contentHash []byte) ([]byte, error) {
	var h blob.ContentHash
	if len(contentHash) != len(h) {
		return nil, fmt.Errorf("content hash is %d bytes, want %d", len(contentHash), len(h))
	}
	copy(h[:], contentHash)

	rc, err := blobs.Get(ctx, userID, dek, h)
	if err != nil {
		return nil, fmt.Errorf("open blob: %w", err)
	}
	defer func() { _ = rc.Close() }()

	var buf bytes.Buffer
	if _, err := buf.ReadFrom(rc); err != nil {
		return nil, fmt.Errorf("read blob: %w", err)
	}
	return buf.Bytes(), nil
}

// openBlobStore builds the FSBlobStore sync-once ingests into. Its per-user
// path key is derived from the master key (blobPathInfo), so no extra secret is
// required; the root comes from MAILROOM_BLOB_ROOT.
func openBlobStore(ctx context.Context, d *deps) (blob.BlobStore, error) {
	master, err := d.keys.MasterKey(ctx)
	if err != nil {
		return nil, fmt.Errorf("master key: %w", err)
	}
	defer crypto.Zero(master)
	pathKey, err := crypto.DeriveKey(master, nil, blobPathInfo)
	if err != nil {
		return nil, fmt.Errorf("derive blob path key: %w", err)
	}
	defer crypto.Zero(pathKey)

	root := os.Getenv("MAILROOM_BLOB_ROOT")
	if root == "" {
		root = "/var/lib/mailroom/blobs"
	}
	blobs, err := blob.NewFSBlobStore(root, pathKey)
	if err != nil {
		return nil, fmt.Errorf("open blob store: %w", err)
	}
	return blobs, nil
}

func parseUserID(s string) (ulid.ULID, error) {
	if s == "" {
		return ulid.ULID{}, errors.New("--user-id is required")
	}
	id, err := ulid.Parse(s)
	if err != nil {
		return ulid.ULID{}, fmt.Errorf("--user-id %q: not a valid ULID: %w", s, err)
	}
	return id, nil
}

// resolveSecretFlag returns the file contents (trimmed) when path is set, else
// the literal value. Both empty yields "".
func resolveSecretFlag(value, path string) (string, error) {
	if path != "" {
		b, err := os.ReadFile(path)
		if err != nil {
			return "", fmt.Errorf("read %s: %w", path, err)
		}
		return strings.TrimSpace(string(b)), nil
	}
	return value, nil
}

// runTrainLabels runs a per-label classifier training pass on demand (#169).
//
// The daily job does this by itself, but "wait a day and read the logs" is not a
// way to find out whether a design works on real mail. This reports each label's
// evidence, the AUC its candidate reached, and whether it was promoted — the
// numbers that say whether a mailbox has enough hand-labelling to classify from,
// and the only honest basis for tuning the abstention band.
//
// DRY RUN BY DEFAULT, like rescore: it trains and evaluates without promoting,
// so the shape can be inspected before any model starts labelling mail.
func runTrainLabels(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("train-labels", flag.ContinueOnError)
	var (
		userIDStr = fs.String("user-id", "", "operator user id (ULID)")
		apply     = fs.Bool("apply", false, "promote models that beat the bar (default: dry-run)")
		folds     = fs.Int("folds", 0, "instead of training, run k-fold grouped cross-validation and report the SPREAD (>=2)")
	)
	if err := fs.Parse(args); err != nil {
		return err
	}
	userID, err := parseUserID(*userIDStr)
	if err != nil {
		return err
	}
	if *folds > 0 && *apply {
		return fmt.Errorf("--folds is a diagnostic and promotes nothing; drop --apply")
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
	registry := ml.NewRegistry(d.store, blobs, d.keys)
	trainer := ml.NewTrainer(d.store, registry)

	if *folds > 0 {
		return reportLabelCV(ctx, d, trainer, userID, *folds)
	}

	results, err := trainer.TrainLabelModels(ctx, userID, !*apply)
	if err != nil {
		return fmt.Errorf("train labels: %w", err)
	}
	if len(results) == 0 {
		fmt.Printf("no label has %d+ hand-applied messages yet — nothing to train\n", ml.MinLabelTrainSamples)
		return nil
	}

	mode := "DRY RUN (nothing promoted)"
	if *apply {
		mode = "APPLIED"
	}
	fmt.Printf("label training — %s\n\n", mode)
	// BOTH AUCs, side by side, because the gap is the finding (#169). AUC is
	// measured over senders the model never trained on — the only question worth
	// asking, since a sender it already knows is one an induced rule handles for
	// free. NAIVE is the same model over the old class-only split, where its own
	// training senders appear on both sides. A large gap means the model has
	// memorised a lookup table of From addresses and generalised nothing, and
	// reporting only the second is how such a model gets promoted.
	// PRECLB IS THE COLUMN THE DECISION IS MADE ON (#169, #220). It is the bottom
	// of a 95% interval on cross-validated precision at the apply threshold — the
	// share of suggestions actually shown that would be right, read at the
	// pessimistic end. AUC and NAIVE are printed beside it as the memorisation
	// diagnostic and decide nothing.
	fmt.Printf("%-24s %8s %8s %7s %7s %7s %7s %9s  %s\n",
		"LABEL", "POSITIVE", "NEGATIVE", "SENDERS", "AUC", "NAIVE", "PRECLB", "PROMOTED", "NOTE")
	for _, r := range results {
		name := r.LabelID.String()
		if err := d.store.UserTx(ctx, userID, func(tx *store.UserTx) error {
			views, e := tx.GetLabels(ctx, []ulid.ULID{r.LabelID})
			if e == nil && len(views) == 1 {
				name = views[0].Name
			}
			return nil
		}); err != nil {
			return err
		}
		fmt.Printf("%-24s %8d %8d %7d %7.3f %7.3f %7s %9v  %s\n",
			truncate(name, 24), r.Positives, r.Negatives, r.Groups,
			r.AUC, r.NaiveAUC, ratioOrDash(r.Gate.PrecisionLCB), r.Promoted, r.Reason)
	}
	// A run in which NOTHING promoted must say so as a sentence, not leave the
	// reader to scan a column of `false`. "0 of 9 cleared the bar" is the finding
	// (#169); a table that merely fails to contain a `true` reads as an empty
	// result rather than a measured refusal.
	promoted := 0
	for _, r := range results {
		if r.Promoted {
			promoted++
		}
	}
	fmt.Printf("\n%d of %d label(s) cleared the promotion gate", promoted, len(results))
	if *apply {
		fmt.Printf(".\n")
	} else {
		fmt.Printf(" — nothing was promoted (dry run; re-run with --apply).\n")
	}
	return nil
}

// reportLabelCV prints grouped k-fold cross-validation per label.
//
// SPREAD IS THE COLUMN TO READ for the AUCs, not MEAN. A mean of 0.75 built
// from folds of 0.60 and 0.90 cannot carry a promotion threshold — whichever
// side of the bar it lands on is an accident of which senders happened to be
// held out. The point of the exercise is to find out which of these numbers
// are stable enough to set a floor against at all.
//
// PRECISION IS THE COLUMN TO READ for the surface (#169). The suggest-only
// risk is wrong suggestions shown, and that is a threshold question: PREC is
// the share of pooled out-of-fold scores at/above the apply threshold that are
// truly the label; COVER is the share of true positives the threshold catches;
// ABSTAIN is the share of everything scored that lands on a human; FALSEREJ is
// the share of true positives confidently rejected — the silent-misfiling
// number. All four are computed over pooled out-of-fold scores, where every
// example is judged exactly once by a model that never saw its sender.
func reportLabelCV(ctx context.Context, d *deps, trainer *ml.Trainer, userID ulid.ULID, folds int) error {
	results, err := trainer.CrossValidateLabels(ctx, userID, folds)
	if err != nil {
		return fmt.Errorf("cross-validate labels: %w", err)
	}
	if len(results) == 0 {
		fmt.Printf("no label has %d+ hand-applied messages yet\n", ml.MinLabelTrainSamples)
		return nil
	}
	fmt.Printf("grouped %d-fold cross-validation — senders are held out whole, nothing is promoted\n\n", folds)
	fmt.Printf("%-24s %8s %7s %6s %7s %7s %7s %8s %6s %6s %7s %6s %7s %8s  %s\n",
		"LABEL", "POSITIVE", "SENDERS", "FOLDS", "MEAN", "MIN", "MAX", "SPREAD",
		"APPLY", "PREC", "PRECLB", "COVER", "ABSTAIN", "FALSEREJ", "NOTE")
	for _, r := range results {
		name := r.LabelID.String()
		if err := d.store.UserTx(ctx, userID, func(tx *store.UserTx) error {
			views, e := tx.GetLabels(ctx, []ulid.ULID{r.LabelID})
			if e == nil && len(views) == 1 {
				name = views[0].Name
			}
			return nil
		}); err != nil {
			return err
		}
		if len(r.AUCs) == 0 {
			fmt.Printf("%-24s %8d %7d %6d %7s %7s %7s %8s %6s %6s %7s %6s %7s %8s  %s\n",
				truncate(name, 24), r.Positives, r.Groups, 0,
				"-", "-", "-", "-", "-", "-", "-", "-", "-", "-", r.Reason)
			continue
		}
		dec := r.Decision
		fmt.Printf("%-24s %8d %7d %6d %7.3f %7.3f %7.3f %8.3f %6d %6s %7s %6s %7s %8s  %s\n",
			truncate(name, 24), r.Positives, r.Groups, len(r.AUCs),
			r.Mean, r.Min, r.Max, r.Spread,
			dec.Applied, ratioOrDash(dec.Precision), ratioOrDash(r.Gate.PrecisionLCB),
			ratioOrDash(dec.Coverage),
			ratioOrDash(dec.AbstainRate), ratioOrDash(dec.FalseReject), r.Reason)
	}

	// The verdict, per label, in words. The table above is the evidence; this is
	// what `--apply` would DO with it, and it is the same LabelGate the promotion
	// path consumes rather than a restatement of it — a preview that can disagree
	// with the engine is worse than none (#155).
	fmt.Printf("\nwhat --apply would do with these numbers:\n")
	wouldPromote := 0
	for _, r := range results {
		name := r.LabelID.String()
		if err := d.store.UserTx(ctx, userID, func(tx *store.UserTx) error {
			views, e := tx.GetLabels(ctx, []ulid.ULID{r.LabelID})
			if e == nil && len(views) == 1 {
				name = views[0].Name
			}
			return nil
		}); err != nil {
			return err
		}
		verdict := "REFUSE"
		if r.Gate.Pass {
			verdict = "PROMOTE"
			wouldPromote++
		}
		fmt.Printf("  %-24s %-8s %s\n", truncate(name, 24), verdict, r.Gate.Reason)
	}
	fmt.Printf("\n%d of %d label(s) would clear the gate (target %.2f, %d+ senders).\n",
		wouldPromote, len(results), ml.LabelPromotionTarget(), ml.LabelPromotionMinSenders())
	return nil
}

// ratioOrDash renders a [0,1] rate, or "-" when it is undefined (NaN) — an
// undefined precision is "nothing cleared the threshold", which must not read
// as either 0.000 (looks catastrophic) or 1.000 (looks perfect).
func ratioOrDash(v float64) string {
	if math.IsNaN(v) {
		return "-"
	}
	return fmt.Sprintf("%.3f", v)
}

// truncate shortens a name to fit the report column.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}

// runClassify reports what the deterministic kind classifier makes of a real
// mailbox (#175).
//
// This is a MEASUREMENT command, not a migration: it writes nothing. Its whole
// purpose is to answer the questions that decide whether the design works, and
// that no fixture can answer honestly:
//
//   - what the kind distribution over real mail actually looks like;
//   - for each of the user's own labels, which kind its hand-labelled messages
//     share — which IS the evidence for a binding ("20 of your 22 Promotions
//     look promotional");
//   - which high-volume sending domains the ESP table does not recognise, so it
//     can be extended from reality rather than from memory.
//
// The weights in internal/kind are hand-set. #157 is the standing reminder of
// what happens when hand-set weights are trusted without being pointed at real
// mail: a plausible-looking one made a daily newsletter the operator's closest
// correspondent.
func runClassify(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("classify", flag.ContinueOnError)
	var (
		userIDStr     = fs.String("user-id", "", "operator user id (ULID)")
		batch         = fs.Int("batch", 500, "message id page size for the keyset scan")
		showUnmatched = fs.Int("show-unmatched", 12, "how many unrecognised sending domains to list")
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

	// Label names, and which messages carry them BY HAND.
	labelName := map[string]string{}
	handLabelled := map[ulid.ULID][]string{}
	if err := d.store.UserTx(ctx, userID, func(tx *store.UserTx) error {
		views, e := tx.ListLabels(ctx)
		if e != nil {
			return e
		}
		for _, v := range views {
			labelName[v.ID.String()] = v.Name
		}
		return nil
	}); err != nil {
		return fmt.Errorf("list labels: %w", err)
	}
	if err := d.store.UserTx(ctx, userID, func(tx *store.UserTx) error {
		applied, e := tx.HandAppliedLabels(ctx)
		if e != nil {
			return e
		}
		for msgID, labelIDs := range applied {
			for _, lid := range labelIDs {
				handLabelled[msgID] = append(handLabelled[msgID], labelName[lid.String()])
			}
		}
		return nil
	}); err != nil {
		return fmt.Errorf("hand-applied labels: %w", err)
	}

	var (
		scanned, parsed, failed int
		byKind                  = map[kind.Kind]int{}
		byLabelKind             = map[string]map[kind.Kind]int{}
		unmatchedSender         = map[string]*senderEvidence{}
	)
	samples := map[kind.Kind][]string{}

	after := ulid.ULID{}
	for {
		var refs []store.MessageBlobRef
		if err := d.store.UserTx(ctx, userID, func(tx *store.UserTx) error {
			var e error
			refs, e = tx.MessageBlobRefsAfter(ctx, after, *batch)
			return e
		}); err != nil {
			return fmt.Errorf("scan messages: %w", err)
		}
		if len(refs) == 0 {
			break
		}
		for _, ref := range refs {
			after = ref.ID
			scanned++
			raw, err := readMailBlob(ctx, blobs, userID, dek, ref.ContentHash)
			if err != nil {
				failed++
				continue
			}
			msg, err := mime.Parse(bytes.NewReader(raw))
			if err != nil || msg == nil {
				failed++
				continue
			}
			parsed++

			facts := factsFromHeader(msg)
			// Correspondence is the user's own behaviour, the per-user half of the
			// prior; look it up the same way the scorer does.
			_ = d.store.UserTx(ctx, userID, func(tx *store.UserTx) error {
				if sent, _, e := tx.OutboundCountForAddress(ctx, facts.FromAddress); e == nil {
					facts.HasCorresponded = sent > 0
				}
				return nil
			})

			v := kind.Classify(facts)
			byKind[v.Kind]++
			if len(samples[v.Kind]) < 8 {
				samples[v.Kind] = append(samples[v.Kind], fmt.Sprintf("%-34s %s", trunc(facts.FromAddress, 34), trunc(facts.Subject, 46)))
			}
			for _, name := range handLabelled[ref.ID] {
				if byLabelKind[name] == nil {
					byLabelKind[name] = map[kind.Kind]int{}
				}
				byLabelKind[name][v.Kind]++
			}
			if kind.ESPUnknown(facts) && facts.ListUnsubscribe != "" {
				recordSenderEvidence(unmatchedSender, kind.SendingDomain(facts), facts)
			}
		}
	}

	fmt.Printf("scanned %d messages (%d parsed, %d unreadable)\n\n", scanned, parsed, failed)

	fmt.Println("KIND DISTRIBUTION")
	for _, k := range []kind.Kind{kind.Personal, kind.Transactional, kind.Promotional,
		kind.Notification, kind.List, kind.Automated, kind.Unknown} {
		n := byKind[k]
		pct := 0.0
		if parsed > 0 {
			pct = 100 * float64(n) / float64(parsed)
		}
		fmt.Printf("  %-14s %5d  %5.1f%%\n", k, n, pct)
	}

	// Samples for the kinds worth eyeballing: the ones that decide nothing
	// (unknown) and the ones that never fire, which is how a dead signal is
	// caught before it is trusted.
	for _, k := range []kind.Kind{kind.Unknown, kind.Personal, kind.Notification, kind.Automated, kind.Transactional} {
		if len(samples[k]) == 0 {
			continue
		}
		fmt.Printf("\nSAMPLE: %s\n", k)
		for _, s := range samples[k] {
			fmt.Printf("  %s\n", s)
		}
	}

	fmt.Println("\nHAND-LABELLED MAIL BY KIND  (this is the binding evidence)")
	labels := make([]string, 0, len(byLabelKind))
	for name := range byLabelKind {
		labels = append(labels, name)
	}
	sort.Strings(labels)
	for _, name := range labels {
		counts := byLabelKind[name]
		total := 0
		for _, n := range counts {
			total += n
		}
		// The dominant kind, and how much of the label it covers.
		bestKind, bestN := kind.Unknown, 0
		for k, n := range counts {
			if n > bestN || (n == bestN && string(k) < string(bestKind)) {
				bestKind, bestN = k, n
			}
		}
		fmt.Printf("  %-14s %2d labelled → %-14s %d/%d (%.0f%%)   %v\n",
			name, total, bestKind, bestN, total, 100*float64(bestN)/float64(total), counts)
	}

	if len(unmatchedSender) > 0 {
		fmt.Println("\nUNRECOGNISED BULK SENDING DOMAINS  (candidates for the ESP table)")
		// The From domain alone is NOT an ESP-table candidate. `e.shop.example` is a
		// brand's own sending subdomain, and adding it would grow the table one
		// retailer at a time forever. What the table can generalise on is the
		// INFRASTRUCTURE behind it — so the evidence that decides whether an entry
		// is possible at all is printed next to each domain: the DKIM d= values
		// and the Return-Path domain actually observed.
		//
		// Modern ESPs offer aligned DKIM, where d= is the brand's own domain
		// precisely so it matches the From. Where that is what these show, no ESP
		// entry can be derived from the headers and the honest answer is that this
		// mail needs a different signal — not a bigger table.
		list := make([]*senderEvidence, 0, len(unmatchedSender))
		for dom, ev := range unmatchedSender {
			if dom != "" {
				list = append(list, ev)
			}
		}
		sort.Slice(list, func(i, j int) bool { return list[i].n > list[j].n })
		for i, e := range list {
			if i >= *showUnmatched {
				break
			}
			fmt.Printf("  %-34s %4d  dkim=%-28s return-path=%s\n",
				trunc(e.domain, 34), e.n,
				trunc(joinSet(e.dkim), 28), trunc(joinSet(e.returnPath), 30))
		}
		fmt.Println("  (aligned = dkim domain matches the sender's own domain: no ESP is")
		fmt.Println("   identifiable from the headers, so the table cannot help that mail)")
	}
	return nil
}

// senderEvidence accumulates what a bulk sender's headers reveal about the
// infrastructure behind it, so ESP-table entries are written from observation
// rather than from memory.
type senderEvidence struct {
	domain     string
	n          int
	dkim       map[string]bool
	returnPath map[string]bool
}

func recordSenderEvidence(m map[string]*senderEvidence, domain string, f kind.Facts) {
	ev := m[domain]
	if ev == nil {
		ev = &senderEvidence{domain: domain, dkim: map[string]bool{}, returnPath: map[string]bool{}}
		m[domain] = ev
	}
	ev.n++
	for _, d := range f.DKIMDomains {
		if d = strings.ToLower(strings.TrimSpace(d)); d != "" {
			ev.dkim[d] = true
		}
	}
	if rp := f.ReturnPath; rp != "" {
		if at := strings.LastIndex(rp, "@"); at >= 0 {
			ev.returnPath[strings.ToLower(strings.Trim(rp[at+1:], "<>. "))] = true
		}
	}
}

// joinSet renders a small set deterministically, commonest form first by name.
func joinSet(m map[string]bool) string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return strings.Join(out, ",")
}

// factsFromHeader projects a parsed message into the kind classifier's inputs.
// It is nudge.FactsFromHeader — ONE projection, so the kind sweep, the
// classify report and the follow-up nudges (#58) all read the same facts (the
// RFC 2047 subject bug this function once had is recorded there).
func factsFromHeader(msg *mime.Message) kind.Facts { return nudge.FactsFromHeader(msg) }

func domainOfAddr(addr string) string {
	if at := strings.LastIndex(addr, "@"); at > 0 {
		return strings.ToLower(addr[at+1:])
	}
	return ""
}

// trunc shortens a field to fit the report.
func trunc(s string, n int) string {
	s = strings.ReplaceAll(strings.TrimSpace(s), "\n", " ")
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}
