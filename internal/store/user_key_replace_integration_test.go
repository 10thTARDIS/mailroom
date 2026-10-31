// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package store

// UserKey/replace's store step (#817): one savepoint, so an injected failure at
// any stage leaves the old key exactly as it was, even for a caller that
// recovers from the error and commits (the shape ingest-style callers have).

import (
	"context"
	"errors"
	"testing"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/wkd"
)

type replaceState struct {
	oldIdentity, newIdentity *string
	oldRetired, oldPublished bool
	newPublished             bool
	oldRetiredAt             *string
	oldReplacedBy            *string
	identityDefault          *string
	identityHash             *string
	keyRows, identityRows    int
}

type replaceFixture struct {
	h            *harness
	user         fixtureUser
	oldID, newID ulid.ULID
	hash         string
}

func newReplaceFixture(t *testing.T) *replaceFixture {
	t.Helper()
	h := newHarness(t)
	a, _ := seedTwoUsers(t, h.super)
	f := &replaceFixture{h: h, user: a, oldID: NewULID(), newID: NewULID(), hash: wkd.HashLocalPart(a.Tag)}
	ctx := context.Background()
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := h.super.Exec(ctx, sql, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec(`INSERT INTO app.user_keys (id, user_id, identity_id, kind, fingerprint, public_key, private_key_ct, wkd_published)
	      VALUES ($1, $2, $3, 'pgp', $4, $5, $6, true)`, f.oldID.String(), a.ID, a.IdentityID, "FPOLD"+f.oldID.String(), []byte{1}, []byte{2})
	exec(`INSERT INTO app.user_keys (id, user_id, kind, fingerprint, public_key, private_key_ct)
	      VALUES ($1, $2, 'pgp', $3, $4, $5)`, f.newID.String(), a.ID, "FPNEW"+f.newID.String(), []byte{3}, []byte{4})
	exec(`UPDATE app.identities SET default_key_id = $2, wkd_hash = $3 WHERE id = $1`, a.IdentityID, f.oldID.String(), f.hash)
	return f
}

func (f *replaceFixture) state(t *testing.T) replaceState {
	t.Helper()
	ctx := context.Background()
	var s replaceState
	q := func(sql string, dst ...any) {
		t.Helper()
		if err := f.h.super.QueryRow(ctx, sql, f.oldID.String(), f.newID.String(), f.user.IdentityID, f.user.ID).Scan(dst...); err != nil {
			t.Fatal(err)
		}
	}
	q(`SELECT o.identity_id, o.decrypt_only, o.wkd_published, o.retired_at::text, o.replaced_by,
	          n.identity_id, n.wkd_published, i.default_key_id, i.wkd_hash,
	          (SELECT count(*) FROM app.change_log WHERE user_id = $4 AND object_type = 'Key'),
	          (SELECT count(*) FROM app.change_log WHERE user_id = $4 AND object_type = 'Identity')
	     FROM app.user_keys o, app.user_keys n, app.identities i
	    WHERE o.id = $1 AND n.id = $2 AND i.id = $3`,
		&s.oldIdentity, &s.oldRetired, &s.oldPublished, &s.oldRetiredAt, &s.oldReplacedBy,
		&s.newIdentity, &s.newPublished, &s.identityDefault, &s.identityHash, &s.keyRows, &s.identityRows)
	return s
}

// TestReplaceUserKeyIsAtomic injects a failure after the bind, after the retire
// and after the change rows, and requires that each leaves the old key bound,
// published and unretired, the new key unbound, and no change row — although
// the caller swallows the error and commits.
func TestReplaceUserKeyIsAtomic(t *testing.T) {
	boom := errors.New("injected failure")
	for _, stage := range []string{"bound", "retired", "logged"} {
		t.Run(stage, func(t *testing.T) {
			f := newReplaceFixture(t)
			before := f.state(t)
			replaceFault = func(s string) error {
				if s == stage {
					return boom
				}
				return nil
			}
			t.Cleanup(func() { replaceFault = nil })
			ctx := context.Background()
			if err := f.h.appStore().UserTx(ctx, mustULID(t, f.user.ID), func(tx *UserTx) error {
				if _, err := tx.ReplaceUserKey(ctx, f.oldID, f.newID, true); !errors.Is(err, boom) {
					t.Errorf("ReplaceUserKey = %v, want the injected failure", err)
				}
				return nil // the caller recovers and COMMITS
			}); err != nil {
				t.Fatal(err)
			}
			after := f.state(t)
			if after.oldIdentity == nil || *after.oldIdentity != f.user.IdentityID || after.oldRetired || !after.oldPublished ||
				after.oldRetiredAt != nil || after.oldReplacedBy != nil {
				t.Errorf("the old key moved: %+v", after)
			}
			if after.newIdentity != nil || after.newPublished {
				t.Errorf("the new key was left bound or published: %+v", after)
			}
			if after.identityDefault == nil || *after.identityDefault != f.oldID.String() || after.identityHash == nil || *after.identityHash != f.hash {
				t.Errorf("the identity moved: default %v hash %v", after.identityDefault, after.identityHash)
			}
			if after.keyRows != before.keyRows || after.identityRows != before.identityRows {
				t.Errorf("change rows written by a failed replace: Key %d->%d Identity %d->%d",
					before.keyRows, after.keyRows, before.identityRows, after.identityRows)
			}
		})
	}
}

// TestReplaceUserKeySucceeds is the positive control for the test above, and
// pins the success shape: one change row per object, the new key bound and
// published, the old one retired pointing at it.
func TestReplaceUserKeySucceeds(t *testing.T) {
	f := newReplaceFixture(t)
	before := f.state(t)
	ctx := context.Background()
	var res ReplaceResult
	if err := f.h.appStore().UserTx(ctx, mustULID(t, f.user.ID), func(tx *UserTx) error {
		var err error
		res, err = tx.ReplaceUserKey(ctx, f.oldID, f.newID, true)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	after := f.state(t)
	if !res.Found || !res.Published || res.IdentityID.String() != f.user.IdentityID {
		t.Errorf("result = %+v", res)
	}
	if after.newIdentity == nil || *after.newIdentity != f.user.IdentityID || !after.newPublished {
		t.Errorf("new key not in place: %+v", after)
	}
	if !after.oldRetired || after.oldPublished || after.oldRetiredAt == nil || after.oldReplacedBy == nil || *after.oldReplacedBy != f.newID.String() {
		t.Errorf("old key not retired into its replacement: %+v", after)
	}
	if *after.identityDefault != f.newID.String() || after.identityHash == nil || *after.identityHash != f.hash {
		t.Errorf("identity: default %v hash %v", *after.identityDefault, after.identityHash)
	}
	if after.keyRows-before.keyRows != 2 || after.identityRows-before.identityRows != 1 {
		t.Errorf("change rows: Key +%d Identity +%d, want +2/+1", after.keyRows-before.keyRows, after.identityRows-before.identityRows)
	}
	// A second replace of the (now retired) old key is refused, writing nothing.
	if err := f.h.appStore().UserTx(ctx, mustULID(t, f.user.ID), func(tx *UserTx) error {
		_, err := tx.ReplaceUserKey(ctx, f.oldID, NewULID(), true)
		if !errors.Is(err, ErrKeyRetired) {
			t.Errorf("replacing a retired key = %v, want ErrKeyRetired", err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
