// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package api

// Cross-user probes for the #796 methods, registered into crossUserProbes so
// TestRLSCrossUserEveryMethod runs them with the rest. Each drives the method AS
// USER A naming USER B's objects, asserts B is untouched (and, for the methods
// that reach the crypto process, that the crypto process was never called),
// then runs B's own call as the positive control.

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/ProtonMail/go-crypto/openpgp/armor"
	"github.com/ProtonMail/go-crypto/openpgp/packet"

	"mailroom/internal/store"
	"mailroom/internal/wkd"
)

func armoredPrivateKey(t *testing.T, email string) string {
	t.Helper()
	e, err := openpgp.NewEntity("Probe", "", email, &packet.Config{Algorithm: packet.PubKeyAlgoEdDSA, Curve: packet.Curve25519})
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	w, err := armor.Encode(&buf, openpgp.PrivateKeyType, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.SerializePrivateWithoutSigning(w, nil); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.String()
}

func userKeyCount(t *testing.T, h *rlsHarness, u rlsUser) (keys, changes int) {
	t.Helper()
	superScan(t, h, &keys, `SELECT count(*) FROM app.user_keys WHERE user_id = $1`, u.UserID.String())
	superScan(t, h, &changes, `SELECT count(*) FROM app.change_log WHERE user_id = $1 AND object_type IN ('Key','Identity')`, u.UserID.String())
	return
}

// probeForeignKeyVerb: A naming B's key id is notFound, writes nothing for B
// and never reaches the crypto process; B's own call does reach it.
func probeForeignKeyVerb(method string, args func(b rlsUser) map[string]any) func(t *testing.T, h *rlsHarness, a, b rlsUser) {
	return func(t *testing.T, h *rlsHarness, a, b rlsUser) {
		k0, c0 := userKeyCount(t, h, b)
		calls := h.custody.count()
		res, mErr := invokeAs(t, h, method, a.UserID, args(b))
		if mErr == nil || mErr.Type != errNotFound {
			t.Fatalf("%s naming B's key as A: %v %+v, want notFound", method, res, mErr)
		}
		if h.custody.count() != calls {
			t.Errorf("%s: A's call about B's key reached the crypto process", method)
		}
		if k, c := userKeyCount(t, h, b); k != k0 || c != c0 {
			t.Errorf("%s: B's keys/changes moved %d/%d -> %d/%d", method, k0, c0, k, c)
		}
		// Control: B's own call gets past isolation and reaches custody (the
		// seeded key's sealed half is a fixture, so custody may then refuse it).
		invokeAs(t, h, method, b.UserID, args(b)) //nolint:errcheck // the outcome is not the point
		if h.custody.count() != calls+1 {
			t.Errorf("control: B's own %s never reached the crypto process, so the probe above proved nothing", method)
		}
	}
}

func init() {
	pass := "rls-probe-passphrase-long"
	crossUserProbes["UserKey/extend"] = probeForeignKeyVerb("UserKey/extend", func(b rlsUser) map[string]any {
		return map[string]any{"id": b.UserKeyID.String(), "days": 30}
	})
	crossUserProbes["UserKey/revoke"] = probeForeignKeyVerb("UserKey/revoke", func(b rlsUser) map[string]any {
		return map[string]any{"id": b.UserKeyID.String(), "reason": "compromised", "confirm": "ABCDEF12"}
	})
	crossUserProbes["UserKey/export"] = probeForeignKeyVerb("UserKey/export", func(b rlsUser) map[string]any {
		return map[string]any{"id": b.UserKeyID.String(), "passphrase": pass}
	})
	// #817: A replacing B's key, the bound signing key of B's address: notFound,
	// nothing of B's moves, no crypto call. B's own replace is the control: it
	// reaches the crypto process and retires the key. The probe seeds its own
	// key (and restores B's identity default afterwards) so the shared fixture
	// key the other probes rely on is left as it was.
	crossUserProbes["UserKey/replace"] = func(t *testing.T, h *rlsHarness, a, b rlsUser) {
		var prevDefault *string
		superScan(t, h, &prevDefault, `SELECT default_key_id FROM app.identities WHERE id = $1`, b.IdentityID.String())
		probeKey := store.NewULID().String()
		superExec(t, h, `INSERT INTO app.user_keys (id, user_id, identity_id, kind, fingerprint, public_key, private_key_ct)
		                 VALUES ($1, $2, $3, 'pgp', $4, $5, $6)`,
			probeKey, b.UserID.String(), b.IdentityID.String(), "REPLACEPROBE"+probeKey, []byte{0x07}, []byte{0x08})
		superExec(t, h, `UPDATE app.identities SET default_key_id = $1 WHERE id = $2`, probeKey, b.IdentityID.String())
		t.Cleanup(func() {
			superExec(t, h, `UPDATE app.identities SET default_key_id = $1 WHERE id = $2`, prevDefault, b.IdentityID.String())
		})
		k0, c0 := userKeyCount(t, h, b)
		calls := h.custody.count()
		if _, mErr := invokeAs(t, h, "UserKey/replace", a.UserID, map[string]any{"id": probeKey}); mErr == nil || mErr.Type != errNotFound {
			t.Fatalf("A replacing B's key: %+v, want notFound", mErr)
		}
		if h.custody.count() != calls {
			t.Error("A's replace of B's key reached the crypto process")
		}
		var retired bool
		superScan(t, h, &retired, `SELECT decrypt_only FROM app.user_keys WHERE id = $1`, probeKey)
		if k, c := userKeyCount(t, h, b); k != k0 || c != c0 || retired {
			t.Errorf("A's replace moved B's keys/changes %d/%d -> %d/%d (retired=%v)", k0, c0, k, c, retired)
		}
		if _, mErr := invokeAs(t, h, "UserKey/replace", b.UserID, map[string]any{"id": probeKey}); mErr != nil {
			t.Fatalf("control: B's own replace: %+v", mErr)
		}
		superScan(t, h, &retired, `SELECT decrypt_only FROM app.user_keys WHERE id = $1`, probeKey)
		if h.custody.count() != calls+1 || !retired {
			t.Error("control: B's own replace did not reach the crypto process and retire the key, so the probe above proved nothing")
		}
	}

	// A generating for B's identity: notFound, nothing for B, no crypto call.
	crossUserProbes["UserKey/generate"] = func(t *testing.T, h *rlsHarness, a, b rlsUser) {
		k0, c0 := userKeyCount(t, h, b)
		calls := h.custody.count()
		args := map[string]any{"identityId": b.IdentityID.String(), "bind": true}
		if _, mErr := invokeAs(t, h, "UserKey/generate", a.UserID, args); mErr == nil || mErr.Type != errNotFound {
			t.Fatalf("generate for B's identity as A: %+v, want notFound", mErr)
		}
		if h.custody.count() != calls {
			t.Error("A's generate for B's identity reached the crypto process")
		}
		if k, c := userKeyCount(t, h, b); k != k0 || c != c0 {
			t.Errorf("B's keys/changes moved %d/%d -> %d/%d", k0, c0, k, c)
		}
		if _, mErr := invokeAs(t, h, "UserKey/generate", b.UserID, args); mErr != nil {
			t.Fatalf("control: B's own generate: %+v", mErr)
		}
		if k, _ := userKeyCount(t, h, b); k != k0+1 {
			t.Errorf("control: B's generate did not add B a key")
		}
	}

	// A importing a key lands in A, never in B.
	crossUserProbes["UserKey/import"] = func(t *testing.T, h *rlsHarness, a, b rlsUser) {
		k0, c0 := userKeyCount(t, h, b)
		args := map[string]any{"privateKey": armoredPrivateKey(t, "probe@example.com")}
		res, mErr := invokeAs(t, h, "UserKey/import", a.UserID, args)
		if mErr != nil {
			t.Fatalf("A's own import: %+v", mErr)
		}
		assertNoBLeak(t, "UserKey/import", res, b)
		if k, c := userKeyCount(t, h, b); k != k0 || c != c0 {
			t.Errorf("A's import reached B: %d/%d -> %d/%d", k0, c0, k, c)
		}
		var owner string
		superScan(t, h, &owner, `SELECT user_id FROM app.user_keys WHERE id = $1`, res["id"].(string))
		if owner != a.UserID.String() {
			t.Errorf("A's imported key is owned by %s", owner)
		}
	}

	// A unpublishing B's (published) key: notFound, B's key still published.
	crossUserProbes["UserKey/set"] = func(t *testing.T, h *rlsHarness, a, b rlsUser) {
		published := func() (p bool) {
			superScan(t, h, &p, `SELECT wkd_published FROM app.user_keys WHERE id = $1`, b.UserKeyID.String())
			return
		}
		if !published() {
			t.Fatal("fixture: B's key is not published, so the probe would prove nothing")
		}
		_, c0 := userKeyCount(t, h, b)
		for _, patch := range []map[string]any{{"wkdPublished": false}, {"decryptOnly": true}, {"identityId": b.IdentityID.String()}} {
			res, mErr := invokeAs(t, h, "UserKey/set", a.UserID, map[string]any{"update": map[string]any{b.UserKeyID.String(): patch}})
			if mErr != nil {
				t.Fatalf("UserKey/set: %+v", mErr)
			}
			nu, _ := res["notUpdated"].(map[string]any)
			if e, _ := nu[b.UserKeyID.String()].(map[string]any); e == nil || e["type"] != setNotFound {
				t.Errorf("A's %v on B's key: %v, want notFound", patch, res)
			}
		}
		var retired bool
		superScan(t, h, &retired, `SELECT decrypt_only FROM app.user_keys WHERE id = $1`, b.UserKeyID.String())
		if !published() || retired {
			t.Errorf("A's UserKey/set changed B's key (published=%v retired=%v)", published(), retired)
		}
		if _, c := userKeyCount(t, h, b); c != c0 {
			t.Errorf("A's UserKey/set wrote %d change rows for B", c-c0)
		}
		if _, mErr := invokeAs(t, h, "UserKey/set", b.UserID, map[string]any{"update": map[string]any{b.UserKeyID.String(): map[string]any{"wkdPublished": false}}}); mErr != nil {
			t.Fatalf("control: %+v", mErr)
		}
		if published() {
			t.Error("control: B's own unpublish did not take effect, so the probe above proved nothing")
		}
	}

	seedBContactKey := func(t *testing.T, h *rlsHarness, b rlsUser) (id, fpr string) {
		id = store.NewULID().String()
		fpr = "CKPROBE" + strings.ToUpper(b.UserID.String()) + id
		superExec(t, h, `INSERT INTO app.contact_keys (id, user_id, contact_id, kind, fingerprint, public_key, source, fetched_at)
		                 VALUES ($1, $2, $3, 'pgp', $4, $5, 'wkd', $6)`,
			id, b.UserID.String(), b.ContactID.String(), fpr, []byte{0x09}, time.Now())
		return id, fpr
	}

	crossUserProbes["ContactKey/get"] = func(t *testing.T, h *rlsHarness, a, b rlsUser) {
		id, fpr := seedBContactKey(t, h, b)
		res, mErr := invokeAs(t, h, "ContactKey/get", a.UserID, map[string]any{})
		if mErr != nil {
			t.Fatalf("ContactKey/get: %+v", mErr)
		}
		assertNoBLeak(t, "ContactKey/get", res, b)
		res, mErr = invokeAs(t, h, "ContactKey/get", a.UserID, map[string]any{"contactIds": []string{b.ContactID.String()}})
		if mErr != nil {
			t.Fatalf("ContactKey/get by id: %+v", mErr)
		}
		assertNoBLeakExcept(t, "ContactKey/get by id", res, b, b.ContactID.String())
		if !strInArr(res["notFound"], b.ContactID.String()) || strings.Contains(mustJSONString(t, res), fpr) {
			t.Errorf("A asking for B's contact: %v", res)
		}
		res, _ = invokeAs(t, h, "ContactKey/get", b.UserID, map[string]any{"contactIds": []string{b.ContactID.String()}})
		if !strings.Contains(mustJSONString(t, res), id) {
			t.Errorf("control: B does not see B's own key: %v", res)
		}
	}

	crossUserProbes["ContactKey/set"] = func(t *testing.T, h *rlsHarness, a, b rlsUser) {
		id, _ := seedBContactKey(t, h, b)
		var c0 int
		superScan(t, h, &c0, `SELECT count(*) FROM app.change_log WHERE user_id = $1 AND object_type = 'Contact'`, b.UserID.String())
		res, mErr := invokeAs(t, h, "ContactKey/set", a.UserID, map[string]any{"destroy": []string{id}})
		if mErr != nil {
			t.Fatalf("ContactKey/set: %+v", mErr)
		}
		nd, _ := res["notDestroyed"].(map[string]any)
		if e, _ := nd[id].(map[string]any); e == nil || e["type"] != setNotFound {
			t.Errorf("A destroying B's key: %v, want notFound", res)
		}
		var n, c int
		superScan(t, h, &n, `SELECT count(*) FROM app.contact_keys WHERE id = $1`, id)
		superScan(t, h, &c, `SELECT count(*) FROM app.change_log WHERE user_id = $1 AND object_type = 'Contact'`, b.UserID.String())
		if n != 1 || c != c0 {
			t.Errorf("A's destroy reached B: rows %d, contact changes %d -> %d", n, c0, c)
		}
		if _, mErr := invokeAs(t, h, "ContactKey/set", b.UserID, map[string]any{"destroy": []string{id}}); mErr != nil {
			t.Fatalf("control: %+v", mErr)
		}
		superScan(t, h, &n, `SELECT count(*) FROM app.contact_keys WHERE id = $1 AND removed_at IS NULL`, id)
		if n != 0 {
			t.Error("control: B's own destroy did not remove the key, so the probe above proved nothing")
		}
	}

	// #818: A refreshing B's key: notFound, B's key unclaimed (checked_at
	// untouched) and the fetcher never called; B's own refresh is the control.
	crossUserProbes["ContactKey/refresh"] = func(t *testing.T, h *rlsHarness, a, b rlsUser) {
		id, _ := seedBContactKey(t, h, b)
		f := &countingFetcher{}
		prev := h.api.keyFetcher
		h.api.keyFetcher = f
		t.Cleanup(func() { h.api.keyFetcher = prev })
		if _, mErr := invokeAs(t, h, "ContactKey/refresh", a.UserID, map[string]any{"id": id}); mErr == nil || mErr.Type != errNotFound {
			t.Fatalf("A refreshing B's key: %+v, want notFound", mErr)
		}
		var checked *time.Time
		superScan(t, h, &checked, `SELECT checked_at FROM app.contact_keys WHERE id = $1`, id)
		if f.calls != 0 || checked != nil {
			t.Errorf("A's refresh of B's key reached the network (%d calls) or claimed it (%v)", f.calls, checked)
		}
		if _, mErr := invokeAs(t, h, "ContactKey/refresh", b.UserID, map[string]any{"id": id}); mErr != nil {
			t.Fatalf("control: B's own refresh: %+v", mErr)
		}
		superScan(t, h, &checked, `SELECT checked_at FROM app.contact_keys WHERE id = $1`, id)
		if f.calls == 0 || checked == nil {
			t.Error("control: B's own refresh never looked the key up, so the probe above proved nothing")
		}
	}

	// #815: A reading B's key history (held, then removed): notFound, nothing
	// of B's in the error; B's own read is the control.
	crossUserProbes["ContactKey/history"] = func(t *testing.T, h *rlsHarness, a, b rlsUser) {
		id, _ := seedBContactKey(t, h, b)
		superExec(t, h, `INSERT INTO app.contact_key_events (id, user_id, contact_key_id, kind, source)
		                 VALUES ($1, $2, $3, 'first_seen', 'wkd')`, store.NewULID().String(), b.UserID.String(), id)
		for _, removed := range []bool{false, true} {
			if removed {
				superExec(t, h, `UPDATE app.contact_keys SET removed_at = now() WHERE id = $1`, id)
			}
			res, mErr := invokeAs(t, h, "ContactKey/history", a.UserID, map[string]any{"id": id})
			if mErr == nil || mErr.Type != errNotFound {
				t.Errorf("A reading B's history (removed=%v): %v %+v, want notFound", removed, res, mErr)
			}
			res, mErr = invokeAs(t, h, "ContactKey/history", b.UserID, map[string]any{"id": id})
			if mErr != nil || len(res["list"].([]any)) != 1 {
				t.Errorf("control: B's own history (removed=%v) = %v %+v", removed, res, mErr)
			}
		}
	}
}

// countingFetcher answers every lookup "no key" and counts the calls.
type countingFetcher struct{ calls int }

func (f *countingFetcher) Fetch(context.Context, string) ([]byte, string, error) {
	f.calls++
	return nil, "", wkd.ErrNoKey
}
