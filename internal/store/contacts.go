// SPDX-License-Identifier: AGPL-3.0-only

package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/oklog/ulid/v2"
)

// This file holds the auto-derived internal Contacts layer (spec §10a, #37).
//
// Contacts are NOT hand-maintained: they are a by-product of ingest. Every
// message that lands teaches the address book who the user corresponds with,
// what name that correspondent goes by, and — the alias story (spec §5) —
// which of the USER'S own addresses that correspondent reached them at, which
// is what lets a reply default to the right From. In the auto-derived model
// each correspondent address is its own contact (there is no cross-address
// person-merging yet; that is a manual, v1.x feature), so contact_addresses'
// UNIQUE (user_id, address) is the natural identity of an auto-contact.
//
// A Contact IS a JMAP-visible object (changelog.go ObjectContact), so its
// CLIENT-VISIBLE state changes append exactly one Contact change_log row in the
// SAME UserTx as the mutation (the load-bearing invariant — CLAUDE.md).
//
// THE CHANGE-LOG DISCIPLINE (deliberate, and the whole reason this file is
// careful): the SYNCED state of a Contact is (existence, its address set, its
// display name, its reached_me_at alias). seen_count and last_seen_at are
// INTERNAL bookkeeping — they move on literally every message and are NOT part
// of the synced object. So a change_log row is appended ONLY on:
//
//   - CREATE (a new contact + its first address), or
//   - a display_name filled in (empty -> non-empty), or
//   - a reached_me_at alias that actually changed.
//
// A pure re-sighting (bump seen_count / last_seen_at, nothing else) appends NO
// row. Emitting one per message would turn the auto-contact stream into a
// firehose that pushes a StateChange to every connected client on every single
// mail delivery — exactly the flood this discipline exists to prevent. The
// re-sighting bump still happens (the data is kept current); it just does not
// advertise itself as a synced-state change.

// Contact is the auto-derived correspondent projection for the JMAP Contact
// extension. In the auto-derived model Addresses holds a single address, but
// the shape carries a slice so a future manual-merge feature can attach more.
type Contact struct {
	ID          ulid.ULID
	DisplayName string
	Addresses   []ContactAddress
	// VIP and Blocked are the user's own markers on the contact (#59, spec
	// §10a) — the only USER-AUTHORED state an auto-derived contact carries.
	// BlockDestination is where new mail from a blocked contact goes ("junk"
	// or "trash"); BlockedAt is when it was blocked (nil while not blocked).
	// All four are synced Contact state: changing any appends a Contact
	// change_log row (contact_flags.go).
	VIP              bool
	Blocked          bool
	BlockDestination string
	BlockedAt        *time.Time
	// SavedAt is non-nil while the contact is SAVED (#816, ADR-041): listed
	// on the Contacts page. Set by sending to it, by isSaved, by isVip.
	SavedAt *time.Time
	// UserDisplayName is the user's own name for the contact (#816). Ingest
	// never writes it; DisplayedName prefers it over DisplayName.
	UserDisplayName *string
	// Notes is the user's free text (at most MaxContactNotes characters).
	Notes string
}

// DisplayedName is the name a client shows: the user's own name for the
// contact when they set one, else the name learned from mail (which may be "").
func (c Contact) DisplayedName() string {
	if c.UserDisplayName != nil && *c.UserDisplayName != "" {
		return *c.UserDisplayName
	}
	return c.DisplayName
}

// MailCounts sums the contact's addresses: outbound is how many messages the
// user sent to them; inbound is every other sighting (mail from them, or mail
// they were a co-recipient of), i.e. seen_count less the sightings that were
// the user's own sent mail. Statistics, not synced state (ADR-041 §3).
func (c Contact) MailCounts() (inbound, outbound int) {
	for _, a := range c.Addresses {
		outbound += a.OutboundCount
		if n := a.SeenCount - a.OutboundCount; n > 0 {
			inbound += n
		}
	}
	return inbound, outbound
}

// MailSpan is the earliest first_seen_at and the latest last_seen_at over the
// contact's addresses (zero times when it has none). After
// contact-dates-backfill these are message dates; before it they may be
// ingest times (#816).
func (c Contact) MailSpan() (first, last time.Time) {
	for _, a := range c.Addresses {
		if !a.FirstSeen.IsZero() && (first.IsZero() || a.FirstSeen.Before(first)) {
			first = a.FirstSeen
		}
		if a.LastSeen.After(last) {
			last = a.LastSeen
		}
	}
	return first, last
}

// ContactAddress is one known address of a contact, with its sighting stats and
// the alias the correspondent reached the user at (nil when none is known — a
// To/Cc-only correspondent, or one only ever seen on outbound mail).
type ContactAddress struct {
	// ID is the contact_addresses row id: what Contact/separate names (#816).
	ID          ulid.ULID
	Address     string
	ReachedMeAt *string
	FirstSeen   time.Time
	LastSeen    time.Time
	SeenCount   int
	// OutboundCount is how many messages the user sent to this address.
	OutboundCount int
	// MergedFrom is the home contact (now a tombstone) this address was merged
	// in from; nil while it is in its home contact (ADR-041).
	MergedFrom *ulid.ULID
}

// ContactParticipant is one To/Cc recipient observed on an ingested message.
type ContactParticipant struct {
	Address string
	Name    string
}

// ContactSourceMessage is the correspondent evidence extracted from one
// ingested message, passed to UpsertContactsFromMessage inside the ingest
// transaction.
//
// It deliberately carries NO explicit "sent vs received" flag: direction is
// fully determined by the me-exclusion below. On a received message From is the
// correspondent (and carries AddressedAddress as their reached_me_at); on a
// sent copy From is the user, which the verified-identity check excludes, so
// reached_me_at is only ever recorded from genuinely inbound mail. To/Cc are
// correspondents in both directions and never carry a reached_me_at.
type ContactSourceMessage struct {
	FromName    string
	FromAddress string
	// Recipients are the To + Cc participants (address + display name if any).
	Recipients []ContactParticipant
	// AddressedAddress is the alias the FROM sender reached the user at — the
	// already-computed reply-from alias for this message (ingest.deriveAddressedIdentity).
	AddressedAddress string
	// ReceivedAt is the message's received_at (INTERNALDATE): the clock the
	// contact-dates report and backfill use, and the one a sighting is stamped
	// with (#829). Zero means "no message behind this sighting" (a key import, a
	// compose recipient), and the sighting is stamped with now().
	ReceivedAt time.Time
	// Draft marks a draft. Drafts are excluded from the report and the backfill,
	// so a draft must not move an EXISTING address's dates either (it still counts
	// as a sighting, as it always has). A NEW address from a draft starts at the
	// draft's date: the columns are NOT NULL and that is the best evidence there is.
	Draft bool
	// DateUntrusted means ReceivedAt is not provider or importer evidence (it fell
	// back to the sender-controlled Date header, or is absent). The sighting is
	// stamped now() for a new address and moves nothing on an existing one.
	DateUntrusted bool
}

// UpsertContactsFromMessage records/refreshes the correspondents of one
// ingested message. It is called inside the ingest UserTx, so its writes and
// any Contact change_log rows commit atomically with the message itself.
//
// The From sender (when it is not the user) is upserted with reached_me_at set
// to the message's derived alias; every external To/Cc recipient is upserted
// as a plain sighting with no alias. The user's own identities/addresses never
// become contacts.
func (tx *UserTx) UpsertContactsFromMessage(ctx context.Context, msg ContactSourceMessage) error {
	var sight contactSighting
	sight.draft = msg.Draft || msg.DateUntrusted
	if !msg.ReceivedAt.IsZero() && !msg.DateUntrusted {
		// Never later than now: a future INTERNALDATE must not pin last_seen_at,
		// because GREATEST never lets it come back down.
		at := msg.ReceivedAt.UTC()
		if now := time.Now().UTC(); at.After(now) {
			at = now
		}
		sight.at = &at
	}
	// A per-message dedup set so a correspondent that appears in several fields
	// (e.g. both From and Cc) is touched — and can emit — at most once.
	seen := map[string]bool{}

	// IS THIS A MESSAGE THE USER SENT? Determined the same way the me-exclusion
	// is — From resolves to an address this user is verified to send as — so it
	// needs no flag from the caller and cannot disagree with the exclusion below.
	//
	// It matters because the recipients of a message you SENT are the people you
	// actually correspond with, and that is the signal priority scoring runs on
	// (mailroom#157). Deriving it from the message rather than from the send path
	// means an IMPORTED Sent folder counts too: a Gmail archive bootstraps years
	// of real correspondence in one pass instead of starting from zero.
	outbound := false
	if _, err := tx.ResolveSendIdentity(ctx, NormalizeAddress(msg.FromAddress)); err == nil {
		outbound = true
	} else if !errors.Is(err, ErrFromNotVerified) {
		return err
	}

	// From first: it is the only field that carries a reached_me_at alias.
	if err := tx.upsertContactAddress(ctx, msg.FromAddress, msg.FromName, msg.AddressedAddress, seen, false, sight); err != nil {
		return err
	}
	for _, p := range msg.Recipients {
		if err := tx.upsertContactAddress(ctx, p.Address, p.Name, "", seen, outbound, sight); err != nil {
			return err
		}
	}
	return nil
}

// upsertContactAddress records one correspondent address. reachedMeAt is the
// alias the correspondent reached the user at ("" when unknown/not applicable).
// It appends a Contact change_log row only when client-visible state changes
// (see the file header's change-log discipline).
// wroteTo is true when this address is a RECIPIENT of a message the user sent —
// the correspondence signal (#157). It bumps outbound_count in addition to the
// ordinary sighting.
func (tx *UserTx) upsertContactAddress(ctx context.Context, rawAddr, name, reachedMeAt string, seen map[string]bool, wroteTo bool, sight contactSighting) error {
	addr := NormalizeAddress(rawAddr)
	// The learned name is sender-controlled text: store it cleaned (#831).
	name = CleanLearnedContactName(name)
	// Skip anything that is not a real address (empty, or malformed) — these
	// tables have no address CHECK, so the gate is here.
	if addr == "" || AddressDomain(addr) == "" {
		return nil
	}
	if seen[addr] {
		return nil
	}
	seen[addr] = true

	// Exclude the user's own identities/addresses. "Me" is defined EXACTLY as the
	// send path defines it — an address this user is verified to send as (a fixed
	// identity, or any address under a catchall domain_wildcard). A catchall
	// therefore excludes every address at that domain, which is correct: those
	// are the user's own aliases, not correspondents.
	if _, err := tx.ResolveSendIdentity(ctx, addr); err == nil {
		return nil // it's the user
	} else if !errors.Is(err, ErrFromNotVerified) {
		return err
	}

	rma := NormalizeAddress(reachedMeAt)

	// Try the re-sighting path first (the overwhelmingly common case once an
	// address is known). It reports whether the address existed and whether any
	// synced state moved.
	contactID, reachedChanged, existed, err := tx.bumpContactSighting(ctx, addr, rma, wroteTo, sight)
	if err != nil {
		return err
	}
	nameChanged := false
	if !existed {
		// New address => new contact (1 address : 1 contact in the auto-derived
		// model). Under a SAVEPOINT so a concurrent ingest that created the same
		// address first surfaces as a recoverable UNIQUE violation rather than
		// poisoning the whole ingest transaction (SQLSTATE 25P02).
		created, cid, cerr := tx.createContact(ctx, addr, name, rma, wroteTo, sight)
		if cerr != nil {
			return cerr
		}
		if created {
			if _, aerr := tx.AppendChange(ctx, ObjectContact, cid, OpCreated); aerr != nil {
				return aerr
			}
			return nil
		}
		// Lost the race: another tx created it between our bump and our insert.
		// Fall back to a re-sighting bump of the now-existing row.
		contactID, reachedChanged, existed, err = tx.bumpContactSighting(ctx, addr, rma, wroteTo, sight)
		if err != nil {
			return err
		}
		if !existed {
			return fmt.Errorf("store: upsert contact %q: row vanished after unique violation", addr)
		}
	}

	// Fill a still-empty display name from this message, if we now have one.
	if name != "" {
		filled, ferr := tx.fillContactName(ctx, contactID, name)
		if ferr != nil {
			return ferr
		}
		nameChanged = filled
	}

	// SAVED ON SEND (#816, ADR-041): writing to an address saves its contact.
	// Only a NULL saved_at is set, so a contact the user un-saved stays
	// un-saved until they write to it again, and a re-send changes nothing.
	savedChanged := false
	if wroteTo {
		s, serr := tx.saveContactOnSend(ctx, contactID)
		if serr != nil {
			return serr
		}
		savedChanged = s
	}

	// A re-sighting owes a change_log row only when synced state actually moved
	// (ADR-014; isSaved/savedAt joined that state in ADR-041). ONE row for the
	// contact however many of these moved: this is the established ingest
	// convention for contacts, a Contact row of its own rather than riding the
	// Email `created` row, because Contact is its own JMAP type with its own
	// state string.
	if reachedChanged || nameChanged || savedChanged {
		cid, perr := ulid.Parse(contactID)
		if perr != nil {
			return fmt.Errorf("store: parse contact id %q: %w", contactID, perr)
		}
		if _, aerr := tx.AppendChange(ctx, ObjectContact, cid, OpUpdated); aerr != nil {
			return aerr
		}
	}
	return nil
}

// contactSighting is when a sighting happened. at is nil for "now" (no message
// behind it); draft stops an existing address's dates moving (see
// ContactSourceMessage.Draft).
type contactSighting struct {
	at    *time.Time
	draft bool
}

// bumpContactSighting refreshes an existing address's sighting: it increments
// seen_count, moves first_seen_at/last_seen_at to cover the sighting's date (LEAST/GREATEST, so
// out-of-order and bulk-imported mail never move them the wrong way; #829), and refreshes reached_me_at when this message
// carries one. It reports the owning contact id, whether reached_me_at actually
// changed, and whether the address existed at all (existed == false leaves the
// caller to create it).
//
// The `cur` CTE snapshots the pre-update reached_me_at (a data-modifying WITH
// cannot see its own effects and RETURNING sees only the NEW row) so the change
// detection is honest — the same snapshot trick UpsertProvenance uses.
func (tx *UserTx) bumpContactSighting(ctx context.Context, addr, rma string, wroteTo bool, sight contactSighting) (contactID string, reachedChanged, existed bool, err error) {
	outbound := 0
	if wroteTo {
		outbound = 1
	}
	err = tx.tx.QueryRow(ctx, `
		WITH cur AS (
			SELECT reached_me_at
			  FROM app.contact_addresses
			 WHERE user_id = $1 AND address = $2
		)
		UPDATE app.contact_addresses ca
		   SET seen_count     = ca.seen_count + 1,
		       outbound_count = ca.outbound_count + $4,
		       first_seen_at  = CASE WHEN $6::boolean THEN ca.first_seen_at
		                             ELSE LEAST(ca.first_seen_at, LEAST(COALESCE($5::timestamptz, now()), now())) END,
		       last_seen_at   = CASE WHEN $6::boolean THEN ca.last_seen_at
		                             ELSE GREATEST(ca.last_seen_at, LEAST(COALESCE($5::timestamptz, now()), now())) END,
		       reached_me_at  = COALESCE(NULLIF($3::text, '')::citext, ca.reached_me_at)
		  FROM cur
		 WHERE ca.user_id = $1 AND ca.address = $2
		RETURNING ca.contact_id,
		          ( NULLIF($3::text, '')::citext IS NOT NULL
		            AND NULLIF($3::text, '')::citext IS DISTINCT FROM cur.reached_me_at ) AS reached_changed`,
		tx.userID.String(), addr, rma, outbound, sight.at, sight.draft).Scan(&contactID, &reachedChanged)
	if err != nil {
		if isNoRows(err) {
			return "", false, false, nil
		}
		return "", false, false, fmt.Errorf("store: bump contact sighting: %w", err)
	}
	return contactID, reachedChanged, true, nil
}

// saveContactOnSend sets saved_at on a contact that is not saved, and reports
// whether it did. A tombstone cannot be reached here: its addresses moved.
func (tx *UserTx) saveContactOnSend(ctx context.Context, contactID string) (bool, error) {
	tag, err := tx.tx.Exec(ctx, `
		UPDATE app.contacts SET saved_at = now()
		 WHERE user_id = $1 AND id = $2 AND saved_at IS NULL`,
		tx.userID.String(), contactID)
	if err != nil {
		return false, fmt.Errorf("store: save contact on send: %w", err)
	}
	return tag.RowsAffected() > 0, nil
}

// fillContactName sets a contact's display name from a freshly observed one,
// but ONLY when the stored name is still empty (we never overwrite a name the
// user or an earlier, better message established). It reports whether a row was
// actually written (i.e. whether the name changed).
func (tx *UserTx) fillContactName(ctx context.Context, contactID, name string) (bool, error) {
	var id string
	err := tx.tx.QueryRow(ctx, `
		UPDATE app.contacts
		   SET display_name = $3
		 WHERE user_id = $1 AND id = $2 AND display_name = '' AND $3 <> ''
		RETURNING id`,
		tx.userID.String(), contactID, name).Scan(&id)
	if err != nil {
		if isNoRows(err) {
			return false, nil
		}
		return false, fmt.Errorf("store: fill contact name: %w", err)
	}
	return true, nil
}

// createContact inserts a new contact and its first address under a SAVEPOINT.
// created is false (with no error) when the address was concurrently created by
// another transaction — a UNIQUE (user_id, address) violation the caller
// recovers from by re-running the sighting bump. The savepoint began before
// BOTH inserts, so a conflict on the address rolls the contact back too: no
// orphan contact is ever left behind.
func (tx *UserTx) createContact(ctx context.Context, addr, name, rma string, wroteTo bool, sight contactSighting) (created bool, id ulid.ULID, err error) {
	id = NewULID()
	addrID := NewULID()
	// A contact first seen as the RECIPIENT of a message the user sent starts at
	// outbound_count 1, not 0: that message is itself the first correspondence
	// (#157). Missing it would mean the first person you write to only counts
	// from your second message onward.
	outbound := 0
	if wroteTo {
		outbound = 1
	}

	sp, err := tx.tx.Begin(ctx) // SAVEPOINT
	if err != nil {
		return false, id, fmt.Errorf("store: create contact (savepoint): %w", err)
	}
	// A contact first seen as the recipient of the user's own mail is SAVED
	// from the start (#816); its `created` change row covers it.
	if _, err = sp.Exec(ctx, `
		INSERT INTO app.contacts (id, user_id, display_name, saved_at)
		VALUES ($1, $2, $3, CASE WHEN $4::boolean THEN now() END)`,
		id.String(), tx.userID.String(), name, wroteTo); err != nil {
		_ = sp.Rollback(ctx)
		return false, id, fmt.Errorf("store: create contact: %w", err)
	}
	if _, err = sp.Exec(ctx, `
		INSERT INTO app.contact_addresses (id, user_id, contact_id, address, reached_me_at, outbound_count,
		                                   first_seen_at, last_seen_at)
		VALUES ($1, $2, $3, $4, NULLIF($5::text, '')::citext, $6,
		        LEAST(COALESCE($7::timestamptz, now()), now()), LEAST(COALESCE($7::timestamptz, now()), now()))`,
		addrID.String(), tx.userID.String(), id.String(), addr, rma, outbound, sight.at); err != nil {
		_ = sp.Rollback(ctx) // ROLLBACK TO SAVEPOINT — undoes the contact insert too
		if isUniqueViolation(err) {
			return false, id, nil // lost the race; caller re-bumps
		}
		return false, id, fmt.Errorf("store: create contact address: %w", err)
	}
	if err = sp.Commit(ctx); err != nil { // RELEASE SAVEPOINT
		return false, id, fmt.Errorf("store: create contact (release): %w", err)
	}
	return true, id, nil
}

// GetContacts returns the contact projections for the requested ids (RLS-scoped).
// Ids not returned were not found or belong to another user — indistinguishable
// by design. Result order is unspecified; the caller re-associates by id.
func (tx *UserTx) GetContacts(ctx context.Context, ids []ulid.ULID) ([]Contact, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	rows, err := tx.tx.Query(ctx, `
		SELECT `+contactColumns+`
		  FROM app.contacts c
		  LEFT JOIN app.contact_addresses ca
		    ON ca.user_id = c.user_id AND ca.contact_id = c.id
		 WHERE c.user_id = $1 AND c.id = ANY($2) AND c.merged_into IS NULL
		 ORDER BY c.id, ca.seen_count DESC, ca.address`,
		tx.userID.String(), ulidStrings(ids))
	if err != nil {
		return nil, fmt.Errorf("store: get contacts: %w", err)
	}
	defer rows.Close()
	return scanContacts(rows)
}

// ListContacts returns every contact for the caller — the "all contacts" form
// Contact/get serves for ids:null. Each contact carries its address set; result
// order is grouped by contact id (the caller re-associates by id). RLS scopes
// it to the caller.
func (tx *UserTx) ListContacts(ctx context.Context) ([]Contact, error) {
	rows, err := tx.tx.Query(ctx, `
		SELECT `+contactColumns+`
		  FROM app.contacts c
		  LEFT JOIN app.contact_addresses ca
		    ON ca.user_id = c.user_id AND ca.contact_id = c.id
		 WHERE c.user_id = $1 AND c.merged_into IS NULL
		 ORDER BY c.id, ca.seen_count DESC, ca.address`,
		tx.userID.String())
	if err != nil {
		return nil, fmt.Errorf("store: list contacts: %w", err)
	}
	defer rows.Close()
	return scanContacts(rows)
}

// contactColumns is the projection scanContacts reads (c = app.contacts, ca =
// app.contact_addresses, LEFT JOINed). A merged-away contact (a tombstone,
// merged_into set) is excluded by every caller's WHERE: it was reported
// destroyed (ADR-041).
const contactColumns = `c.id, c.display_name, c.vip, c.blocked, c.block_destination, c.blocked_at,
		       c.saved_at, c.user_display_name, c.notes,
		       ca.id, ca.address, ca.reached_me_at, ca.first_seen_at, ca.last_seen_at, ca.seen_count,
		       ca.outbound_count, ca.merged_from_contact`

// scanContacts groups a contact⋈address result set (ordered by contact id) into
// Contacts. An address-less contact (LEFT JOIN NULL) yields an empty Addresses.
func scanContacts(rows interface {
	Next() bool
	Scan(...any) error
	Err() error
}) ([]Contact, error) {
	var out []Contact
	byID := map[ulid.ULID]int{} // contact id -> index in out
	for rows.Next() {
		var (
			idStr     string
			name      string
			vip       bool
			blocked   bool
			blockDest string
			blockedAt *time.Time
			savedAt   *time.Time
			userName  *string
			notes     string
			addrID    *string
			address   *string
			reached   *string
			firstSeen *time.Time
			lastSeen  *time.Time
			seenCount *int
			outbound  *int
			mergedStr *string
		)
		if err := rows.Scan(&idStr, &name, &vip, &blocked, &blockDest, &blockedAt,
			&savedAt, &userName, &notes,
			&addrID, &address, &reached, &firstSeen, &lastSeen, &seenCount, &outbound, &mergedStr); err != nil {
			return nil, fmt.Errorf("store: scan contact: %w", err)
		}
		id, err := ulid.Parse(idStr)
		if err != nil {
			return nil, fmt.Errorf("store: parse contact id %q: %w", idStr, err)
		}
		idx, ok := byID[id]
		if !ok {
			out = append(out, Contact{ID: id, DisplayName: name,
				VIP: vip, Blocked: blocked, BlockDestination: blockDest, BlockedAt: blockedAt,
				SavedAt: savedAt, UserDisplayName: userName, Notes: notes})
			idx = len(out) - 1
			byID[id] = idx
		}
		if address != nil {
			ca := ContactAddress{Address: *address, ReachedMeAt: reached}
			if firstSeen != nil {
				ca.FirstSeen = *firstSeen
			}
			if lastSeen != nil {
				ca.LastSeen = *lastSeen
			}
			if seenCount != nil {
				ca.SeenCount = *seenCount
			}
			if outbound != nil {
				ca.OutboundCount = *outbound
			}
			if addrID != nil {
				if ca.ID, err = ulid.Parse(*addrID); err != nil {
					return nil, fmt.Errorf("store: parse contact address id %q: %w", *addrID, err)
				}
			}
			if mergedStr != nil {
				m, err := ulid.Parse(*mergedStr)
				if err != nil {
					return nil, fmt.Errorf("store: parse merged-from contact id %q: %w", *mergedStr, err)
				}
				ca.MergedFrom = &m
			}
			out[idx].Addresses = append(out[idx].Addresses, ca)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: scan contacts: %w", err)
	}
	return out, nil
}

// ContactMatch is one autocomplete hit: an address, its owning contact, the
// display name, and the alias the correspondent reached the user at (for a
// compose default-From). Ordered most-corresponded-with first.
type ContactMatch struct {
	ContactID   ulid.ULID
	Address     string
	DisplayName string
	ReachedMeAt *string
}

// maxAutocompleteLimit bounds an autocomplete page; a client asking for more is
// clamped rather than allowed to scan the whole address book per keystroke.
const maxAutocompleteLimit = 100

// defaultAutocompleteLimit is used when the caller passes a non-positive limit.
const defaultAutocompleteLimit = 20

// QueryContactsForAutocomplete returns the correspondents whose address OR
// display name contains prefix (case-insensitive substring), most-corresponded
// -with first (seen_count desc). It is the store surface behind Contact/query
// and the compose recipient autocomplete. An empty prefix returns the top
// contacts overall (a useful "recent correspondents" seed). RLS scopes it to
// the caller.
func (tx *UserTx) QueryContactsForAutocomplete(ctx context.Context, prefix string, limit int) ([]ContactMatch, error) {
	if limit <= 0 {
		limit = defaultAutocompleteLimit
	}
	if limit > maxAutocompleteLimit {
		limit = maxAutocompleteLimit
	}
	pattern := "%" + escapeLike(strings.TrimSpace(prefix)) + "%"

	rows, err := tx.tx.Query(ctx, `
		SELECT ca.contact_id, ca.address, c.display_name, ca.reached_me_at
		  FROM app.contact_addresses ca
		  JOIN app.contacts c ON c.user_id = ca.user_id AND c.id = ca.contact_id
		 WHERE ca.user_id = $1
		   AND ( $2::text = ''
		      OR ca.address ILIKE $3 ESCAPE '\'
		      OR c.display_name ILIKE $3 ESCAPE '\' )
		 ORDER BY ca.seen_count DESC, ca.last_seen_at DESC, ca.address
		 LIMIT $4`,
		tx.userID.String(), strings.TrimSpace(prefix), pattern, limit)
	if err != nil {
		return nil, fmt.Errorf("store: query contacts autocomplete: %w", err)
	}
	defer rows.Close()

	var out []ContactMatch
	for rows.Next() {
		var (
			idStr   string
			m       ContactMatch
			reached *string
		)
		if err := rows.Scan(&idStr, &m.Address, &m.DisplayName, &reached); err != nil {
			return nil, fmt.Errorf("store: scan contact match: %w", err)
		}
		id, err := ulid.Parse(idStr)
		if err != nil {
			return nil, fmt.Errorf("store: parse contact id %q: %w", idStr, err)
		}
		m.ContactID = id
		m.ReachedMeAt = reached
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: query contacts autocomplete: %w", err)
	}
	return out, nil
}

// escapeLike escapes the LIKE/ILIKE metacharacters so a user-typed prefix is
// matched literally (backslash is the ESCAPE char in the queries above).
func escapeLike(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return r.Replace(s)
}
