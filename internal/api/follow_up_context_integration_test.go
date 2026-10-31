// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package api

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"mailroom/internal/store"
)

// The two places the API must hand nudge.Due more than the candidates' ids
// (#58): (1) the store's CONTEXT — sent mail read only for "wrote to them
// before", which may be trashed — is never itself nudged; (2) a Done on the
// newer message of a conversation keeps the older one from taking its place.
func TestFollowUpContextAndConversationDone(t *testing.T) {
	h := newComposeHarness(t)
	ctx := context.Background()
	x := seedRLSUser(t, h.super, "fu2-"+strings.ToLower(store.NewULID().String()))
	f := x.apiFixture
	var me string
	if err := h.super.QueryRow(ctx, `SELECT address FROM app.identities WHERE id = $1`, x.IdentityID.String()).Scan(&me); err != nil {
		t.Fatal(err)
	}
	call(t, h.api.deskPreferencesSet, f.UserID, map[string]any{
		"update": map[string]any{f.UserID.String(): map[string]any{
			"followUp": map[string]any{"outbound": true, "inbound": true, "inboundDays": 3},
		}},
	})

	trashed := fuMail(t, h, f, fuThread(t, h, f), me, "kim@example.org", "Binned", 10, "")
	if _, err := h.super.Exec(ctx, `UPDATE app.messages SET trashed_at = now() WHERE id = $1`, trashed.String()); err != nil {
		t.Fatal(err)
	}
	conv := fuThread(t, h, f)
	older := fuMail(t, h, f, conv, "eve@example.org", me, "Re: Plans", 6, "In-Reply-To: <a@example.test>\r\n")
	var olderMsgID string
	if err := h.super.QueryRow(ctx, `SELECT message_id_hint FROM app.messages WHERE id = $1`, older.String()).Scan(&olderMsgID); err != nil {
		t.Fatal(err)
	}
	newer := fuMail(t, h, f, conv, "eve@example.org", me, "Re: Plans", 5, "In-Reply-To: "+olderMsgID+"\r\n")

	ids, _, _ := fuList(t, h, f.UserID)
	if fmt.Sprint(ids) != fmt.Sprint([]string{newer.String() + ":inbound"}) {
		t.Fatalf("listed %v, want only the newer message of the conversation (the trashed sent mail is context)", ids)
	}
	call(t, h.api.followUpSet, f.UserID, map[string]any{"dismiss": map[string]any{newer.String(): "inbound"}})
	if ids, _, _ = fuList(t, h, f.UserID); len(ids) != 0 {
		t.Fatalf("after Done on the newer message: %v, want nothing (the older must not take its place)", ids)
	}
}
