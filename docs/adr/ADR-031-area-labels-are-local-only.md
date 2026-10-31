# ADR-031: Areas' own labels are local-only, and local labels are applied by id

**Status:** Accepted (a deliberate deviation from spec §6)

## Context

Spec §6 makes labels hybrid and IMAP-sync by default: "by default each label
mirrors to IMAP keywords so other clients and backups see them … Default = sync;
local-only is the exception, not a per-label chore."

Every desk area owns a label (placing mail in an area is applying that
label). The labels Mailroom mints for areas are minted **local-only**: no
`label_keyword_map` row and `mirror_to_imap = false`. That covers the default
areas (`store.labelForAreaName`, used when the default set — Primary, Social,
Promotions, Updates, Forums, Notes — is materialised), a new area
(`store.CreateArea`), and day-one setup's folder labels
(`store.setupEnsureLabelAndArea`). On a long-used instance tens of thousands of
messages can carry such labels, most of them applied by the classifier.

Before this decision the JMAP surface knew labels only as keyword atoms. Email/get joined
labels through `label_keyword_map` with an inner join, and Email/set routed a
label only by `keywords/<atom>`. A local-only label was therefore on the
message, deciding its area, and impossible for any client to see or change. The
web client showed every area label as disabled ("local only") in the label
menu, and the desk's Re-file refused them, so mail could not be moved from
Updates to Promotions at all.

There were two ways out: give the area labels atoms, or make local labels
applicable.

## Decision

Decision (2026-10-15): **keep areas' own labels local-only** and
make local labels **applicable inside Mailroom**:

1. **No atoms for area labels.** Nothing assigns a keyword atom to an area's
   label, and nothing writes one to the mail server. Whether a label mirrors is
   decided by whether it has an atom (`store.MirrorKeyword`, and the writeback
   arm of `ApplyEmailUpdate` via `labelKeywordAtom`), not by the
   `mirror_to_imap` flag. So "fixing" the menu by assigning atoms would have
   started writing keywords onto the user's mail.
2. **`Email.labelIds`** (urn:mailroom:labels, opt-in): the id-set
   `{labelId: true}` of EVERY label on the message, mirrored and local alike.
   It is read in the same query as the keyword atoms (`attachLabelKeywords`,
   now a LEFT JOIN that adds to `keywords` and `labelProvenance` only when an
   atom exists, so the output for mirrored labels is unchanged).
3. **`Email/set` patch `labelIds/<id>`**: `true` applies the label, and `false`
   or `null` removes it. The id must be one of the caller's labels (RLS makes
   another user's id indistinguishable from a missing one), otherwise the
   update is refused with `invalidPatch` naming the patch key. The patch goes
   to the same `EmailUpdate.AddLabelIDs/RemoveLabelIDs` fields as the keyword
   path, so it gets the same change_log row, suggestion clearing, rule
   corrections and, **for a label that has an atom**, the same writeback.
   A whole-object `labelIds` replacement is refused, because it would need a
   diff against labels the client may never have fetched.
4. **Re-file is a swap.** The area resolver picks the label-fed area by
   position (`areas_resolve.go`), so the client's re-file removes the other
   areas' own labels the message carries and adds the target's. Its undo
   reverses exactly what changed for each message.

## Why not mirror them

- **Volume on someone else's server.** Mirroring would write keywords onto
  every message in the user's mailbox, tens of thousands on a long-used one. It would also write one per
  classifier decision from then on, for a sorting scheme that only means
  something inside Mailroom. Other clients would show `MailroomLabel-Updates`
  atoms that the user never asked for.
- **Reconciliation risk.** Once a label is mirrored, the server's copy becomes
  an input. A keyword dropped or rewritten by another client, a server that
  refuses user keywords in some mailboxes (spec §6's PERMANENTFLAGS rule), or a
  restore from backup would all have to be reconciled against the desk's
  placement. "Remote wins" in any form could move mail between areas without
  the user doing anything. Keeping the label local means the desk has one
  source of truth.
- **Area placement is Mailroom's own view**, in the same spirit as ADR-029's
  mute. It says where Mailroom shows a message, not something the user files
  on the server.

## Consequences

- Area labels do not appear in other mail clients or in server-side backups.
  The label menu says so with a non-blocking hint ("stays in Mailroom"). A
  Mailroom export still carries them, because it reads `message_labels`.
- An area that reuses an EXISTING label (`labelForAreaName` matches by name)
  keeps that label's mirroring. Only the labels Mailroom mints for areas are
  local by this decision.
- A client that wants every label on a message must read `labelIds`.
  `keywords` and `labelProvenance` still describe only mirrored labels.
  `labelProvenance` is unchanged byte for byte, so a local label has no
  provenance entry on the wire yet.
- The web client applies a mirrored label through its keyword atom, as before,
  and a local label through `labelIds/<id>`. A mirrored label sent by id
  writes back the same intents as the keyword path, and a test pins that. The
  client keeps the atom path for mirrored labels because its keyword overlay
  is what the chips and label lists read.
- Re-file does not remove a non-area label that also feeds an area
  (`area_feeds`). If such a label puts a message in an earlier area, the
  message stays there.
- Spec §6 records this as an exception alongside ADR-029.

## Correction

The Context above lists "Notes" as one of the default areas. It is not. The
default set is five areas, one per category tab (`areas.Default()` over
`tabs.All`, `internal/tabs`): Primary, Social, Promotions, Updates, Forums.
"Notes" exists only where a user created it. The decision is unaffected: it
covers every area's label whatever the area is called.

## References

- Spec §6 (label model), §13 (labels are keywords).
- ADR-029 (the precedent for Mailroom-only behaviour).
- `internal/store/jmap_email.go` (`attachLabelKeywords`, `ApplyEmailUpdate`),
  `internal/api/email.go` (`applyLabelIDPatches`), `web/src/core/refile.ts`.
