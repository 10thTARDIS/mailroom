// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package api

// Mailbox.accountId over the wire (#488): folder NAMES collide across
// accounts — every account has an INBOX, a Sent, an Archive — so a client
// cannot tell one account's Inbox from another's by name alone. This pins
// that Mailbox/get reports the OWNING account for each mailbox, that two
// accounts' INBOXes carry different accountIds, and that the property is
// still RLS-scoped like every other Mailbox field.

import (
	"context"
	"testing"

	"mailroom/internal/store"
)

func TestMailboxAccountID(t *testing.T) {
	h := newAPIHarness(t)
	a, b := seedTwo(t, h.super)
	ctx := context.Background()

	// Give user A a SECOND account with its own INBOX, created through the same
	// UpsertMailbox path the sync engine uses to discover mailboxes — not a raw
	// INSERT — so account_id is populated exactly the way it is in production.
	var secondAccountID, secondInboxID string
	if err := h.store.UserTx(ctx, a.UserID, func(tx *store.UserTx) error {
		acctID, err := tx.CreateAccount(ctx, store.AccountRow{
			Name:           "second",
			IMAPHost:       "imap2.example.com",
			SMTPHost:       "smtp2.example.com",
			CredentialsCT:  []byte{0x01},
			CredentialsDEK: store.NewULID(),
			SyncEnabled:    true,
		})
		if err != nil {
			return err
		}
		secondAccountID = acctID.String()
		st, err := tx.UpsertMailbox(ctx, store.MailboxUpsert{
			AccountID:   acctID,
			IMAPName:    "INBOX",
			DisplayName: "Inbox",
		})
		if err != nil {
			return err
		}
		secondInboxID = st.ID.String()
		return nil
	}); err != nil {
		t.Fatalf("seed A's second account: %v", err)
	}

	// Both of A's INBOXes come back with their OWN account id, and the two
	// differ from each other.
	res := call(t, h.api.mailboxGet, a.UserID, map[string]any{
		"ids": []string{a.InboxID.String(), secondInboxID},
	})
	list, _ := res["list"].([]any)
	if len(list) != 2 {
		t.Fatalf("A/Mailbox/get both inboxes: %d objects, want 2: %#v", len(list), res)
	}
	accountOf := map[string]string{}
	for _, item := range list {
		obj, _ := item.(map[string]any)
		id, _ := obj["id"].(string)
		acctID, ok := obj["accountId"].(string)
		if !ok {
			t.Fatalf("mailbox %v has no accountId: %#v", id, obj)
		}
		accountOf[id] = acctID
	}
	if got := accountOf[a.InboxID.String()]; got != a.AccountID.String() {
		t.Errorf("first account's inbox accountId = %q, want %q", got, a.AccountID.String())
	}
	if got := accountOf[secondInboxID]; got != secondAccountID {
		t.Errorf("second account's inbox accountId = %q, want %q", got, secondAccountID)
	}
	if accountOf[a.InboxID.String()] == accountOf[secondInboxID] {
		t.Error("two different accounts' INBOXes carry the SAME accountId — a client could " +
			"not tell them apart without guessing from the name, exactly the unmeasured " +
			"claim #488 was opened to avoid")
	}

	// RLS: user B asking for A's mailboxes (by id, from either of A's accounts)
	// gets notFound, not a leak — indistinguishable from ids that do not exist.
	res = call(t, h.api.mailboxGet, b.UserID, map[string]any{
		"ids": []string{a.InboxID.String(), secondInboxID},
	})
	if lst, _ := res["list"].([]any); len(lst) != 0 {
		t.Errorf("B's Mailbox/get leaked A's mailboxes: %#v", lst)
	}
	notFound, _ := res["notFound"].([]any)
	if len(notFound) != 2 {
		t.Errorf("B's Mailbox/get notFound = %v, want both of A's inbox ids", notFound)
	}
}
