// SPDX-License-Identifier: AGPL-3.0-only

// Package importer bulk-loads an existing mail archive — mbox, maildir, or a
// Google Takeout — into the canonical store (spec §4, matrix §1, mailroom#27).
//
// # One ingest, not two
//
// Every message goes through ingest.Ingestor.IngestArchive, which is the SAME
// canonical ingest the IMAP sync path uses: SHA-256-of-the-raw-bytes identity
// (ADR-001), the encrypted blob, incremental threading, and the change_log rows —
// all in one transaction (the load-bearing invariant, CLAUDE.md). This package
// contributes the archive-shaped half only: how to stream messages out of a file
// tree, what folder/label/flag each one carries, and how to resume.
//
// # Imported mail has no IMAP provenance, and that is load-bearing
//
// An archived message never had a (mailbox, uidvalidity, uid) coordinate, so it
// gets no app.message_provenance row. It is placed by local_mailbox_id, the same
// column an un-APPENDed draft uses. Three consequences fall out of that single
// fact, structurally rather than by remembering to check:
//
//   - Writeback can never fire for it. Writeback intents are enqueued once per
//     LIVE ACCOUNT and the live account set is read from message_provenance
//     (store/jmap_email.go liveAccountIDs); an empty set means every enqueue loop
//     runs zero times, for this import and for every later Email/set the user
//     performs on the message. Imported mail is local-only mail: Mailroom must
//     never push a 20-year-old archive at somebody's live IMAP server.
//   - The expunge reaper can never soft-delete it. The reaper only runs against a
//     mailbox it has just polled, the import account is created sync_enabled=false
//     (so it is never polled — store.ListSyncableAccounts), and reapMessage
//     explicitly refuses to vanish a message that has a local placement.
//   - Its mailbox membership is exactly the one folder we filed it in
//     (store/jmap_email.go effectiveMembershipSQL, branch 2).
//
// # Scale
//
// A real Google Takeout is tens of gigabytes. Nothing here ever holds an archive,
// or an mbox, in memory: sources stream, and the only large allocation is the ONE
// message currently being ingested, hard-bounded by Options.MaxMessageBytes.
//
// # Resumability
//
// Idempotence is free: re-presenting bytes already stored deduplicates on the
// content hash. The Checkpoint exists only so that a resumed import does not have
// to re-READ 20 GB to rediscover that. It is written after the ingest commits, so
// it can only ever lag — a crash re-reads a few messages and dedups them — and it
// is validated against the database before it is trusted.
//
// # A bad message is a counted skip, never the end of the run
//
// Real archives contain truncated messages, broken MIME, absurd headers and bytes
// that are not mail at all. internal/mime is already lenient (it recovers panics
// and degrades), and each message is ingested in ITS OWN transaction, so a message
// that Postgres itself rejects (a NUL byte in a header, say) aborts only its own
// transaction. Failures are counted, rate-limited in the log, and reported —
// message 400,000 does not end a 20 GB import.
package importer
