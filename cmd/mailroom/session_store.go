// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"context"
	"fmt"
	"time"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/auth"
	"mailroom/internal/crypto"
	"mailroom/internal/store"
	"mailroom/internal/xoauth2"
)

// sessionTokenInfoV1 is the HKDF label for the key that seals a session's
// provider refresh token and ID token (ADR-048 section 2). The key is derived
// per user from the master key, as oauthmail.SealFlowVerifier does, so no per-row
// key is stored. Never change it for existing rows.
const sessionTokenInfoV1 = "mailroom-session-v1"

// What a sealed session token is, bound into its associated data. Both columns
// are sealed under the same key and ids, so without this a refresh_ct copied
// into id_token_ct would open as an "ID token" and be handed to the browser as
// logout's id_token_hint: a decryption oracle for anyone who can write the row
// but does not hold the master key.
const (
	sessionPurposeRefresh = "refresh"
	sessionPurposeIDToken = "id_token"
)

// sessionStore adapts internal/store's session methods to auth.SessionStore. It
// owns the sealing, so the store layer never sees a token and the auth package
// never sees the database.
//
// ⚠ Tokens are bearer credentials: nothing here puts one in an error string, and
// the sealed forms are bound to the user and session ids as associated data, so
// a ciphertext copied between rows does not open.
type sessionStore struct {
	st   *store.Store
	keys crypto.KeyProvider
}

var _ auth.SessionStore = (*sessionStore)(nil)

func newSessionStore(st *store.Store, keys crypto.KeyProvider) *sessionStore {
	return &sessionStore{st: st, keys: keys}
}

func (s *sessionStore) Lookup(ctx context.Context, hash [32]byte) (auth.SessionRef, bool, error) {
	ref, ok, err := s.st.LookupSession(ctx, hash)
	if err != nil || !ok {
		return auth.SessionRef{}, false, err
	}
	return auth.SessionRef{ID: ref.ID, UserID: ref.UserID, LastCheckedAt: ref.LastCheckedAt}, true, nil
}

func (s *sessionStore) Create(ctx context.Context, in auth.NewSessionInput) (ulid.ULID, error) {
	id := store.NewULID()
	var refreshCT, idCT []byte
	if in.RefreshToken != "" || in.IDToken != "" {
		master, err := s.keys.MasterKey(ctx)
		if err != nil {
			return ulid.ULID{}, fmt.Errorf("main: session master key: %w", err)
		}
		defer crypto.Zero(master)
		if in.RefreshToken != "" {
			if refreshCT, err = sealSessionToken(master, in.UserID, id, sessionPurposeRefresh, in.RefreshToken); err != nil {
				return ulid.ULID{}, err
			}
		}
		if in.IDToken != "" {
			if idCT, err = sealSessionToken(master, in.UserID, id, sessionPurposeIDToken, in.IDToken); err != nil {
				return ulid.ULID{}, err
			}
		}
	}
	err := s.st.UserTx(ctx, in.UserID, func(tx *store.UserTx) error {
		return tx.CreateSession(ctx, store.NewSession{
			ID: id, TokenHash: in.TokenHash, RefreshCT: refreshCT, IDTokenCT: idCT,
			UserAgent: in.UserAgent, Addr: in.Addr,
		})
	})
	if err != nil {
		return ulid.ULID{}, err
	}
	return id, nil
}

func (s *sessionStore) Touch(ctx context.Context, ref auth.SessionRef, addr string, at time.Time) error {
	return s.st.UserTx(ctx, ref.UserID, func(tx *store.UserTx) error {
		return tx.TouchSession(ctx, ref.ID, addr, at)
	})
}

func (s *sessionStore) Revoke(ctx context.Context, ref auth.SessionRef, reason string) error {
	return s.st.UserTx(ctx, ref.UserID, func(tx *store.UserTx) error {
		_, err := tx.RevokeSession(ctx, ref.ID, reason)
		return err
	})
}

func (s *sessionStore) IDToken(ctx context.Context, ref auth.SessionRef) (string, error) {
	var ct []byte
	if err := s.st.UserTx(ctx, ref.UserID, func(tx *store.UserTx) error {
		var err error
		ct, err = tx.SessionIDToken(ctx, ref.ID)
		return err
	}); err != nil {
		return "", err
	}
	if len(ct) == 0 {
		return "", nil
	}
	master, err := s.keys.MasterKey(ctx)
	if err != nil {
		return "", fmt.Errorf("main: session master key: %w", err)
	}
	defer crypto.Zero(master)
	return openSessionToken(master, ref.UserID, ref.ID, sessionPurposeIDToken, ct)
}

func (s *sessionStore) RefreshToken(ctx context.Context, ref auth.SessionRef) (xoauth2.Secret, error) {
	var ct []byte
	if err := s.st.UserTx(ctx, ref.UserID, func(tx *store.UserTx) error {
		var err error
		ct, err = tx.SessionRefreshToken(ctx, ref.ID)
		return err
	}); err != nil {
		return "", err
	}
	return s.openRefresh(ctx, ref.UserID, ref.ID, ct)
}

// openRefresh opens a sealed refresh token ("" for none). The error never
// contains the token.
func (s *sessionStore) openRefresh(ctx context.Context, userID, sessionID ulid.ULID, ct []byte) (xoauth2.Secret, error) {
	if len(ct) == 0 {
		return "", nil
	}
	master, err := s.keys.MasterKey(ctx)
	if err != nil {
		return "", fmt.Errorf("main: session master key: %w", err)
	}
	defer crypto.Zero(master)
	tok, err := openSessionToken(master, userID, sessionID, sessionPurposeRefresh, ct)
	if err != nil {
		return "", err
	}
	return xoauth2.Secret(tok), nil
}

// sessionAAD binds a sealed token to its user, its session and what it is.
func sessionAAD(userID, sessionID ulid.ULID, purpose string) []byte {
	aad := make([]byte, 0, 32+1+len(purpose))
	aad = append(aad, userID[:]...)
	aad = append(aad, sessionID[:]...)
	aad = append(aad, 0)
	return append(aad, purpose...)
}

func sealSessionToken(master []byte, userID, sessionID ulid.ULID, purpose, token string) ([]byte, error) {
	key, err := crypto.DeriveKey(master, userID[:], sessionTokenInfoV1)
	if err != nil {
		return nil, fmt.Errorf("main: derive session key: %w", err)
	}
	defer crypto.Zero(key)
	ct, err := crypto.Seal(key, nil, []byte(token), sessionAAD(userID, sessionID, purpose))
	if err != nil {
		return nil, fmt.Errorf("main: seal session token: %w", err)
	}
	return ct, nil
}

func openSessionToken(master []byte, userID, sessionID ulid.ULID, purpose string, ct []byte) (string, error) {
	key, err := crypto.DeriveKey(master, userID[:], sessionTokenInfoV1)
	if err != nil {
		return "", fmt.Errorf("main: derive session key: %w", err)
	}
	defer crypto.Zero(key)
	pt, err := crypto.Open(key, ct, sessionAAD(userID, sessionID, purpose))
	if err != nil {
		return "", fmt.Errorf("main: open session token: %w", err)
	}
	defer crypto.Zero(pt)
	return string(pt), nil
}

// Renew runs one renewal under the store's per-session lock (ADR-048 section 4).
// It opens the stored refresh token for the callback and seals whatever the
// callback returns; the store layer only ever sees ciphertext.
//
// An error from here (the key provider, a token that will not open, the
// database) is a failure to renew, not a verdict on the session: the auth
// package treats it as transient and serves within the outage allowance.
func (s *sessionStore) Renew(ctx context.Context, ref auth.SessionRef, staleBefore time.Time,
	do func(ctx context.Context, cur auth.RenewCurrent) auth.RenewDecision) (auth.RenewResult, error) {
	var res auth.RenewResult
	err := s.st.UserTx(ctx, ref.UserID, func(tx *store.UserTx) error {
		state, ran, err := tx.RenewSessionLocked(ctx, ref.ID, staleBefore,
			func(cur store.SessionRenewState) (store.SessionRenewWrite, error) {
				master, err := s.keys.MasterKey(ctx)
				if err != nil {
					return store.SessionRenewWrite{}, fmt.Errorf("main: session master key: %w", err)
				}
				defer crypto.Zero(master)
				var refresh xoauth2.Secret
				if len(cur.RefreshCT) > 0 {
					tok, err := openSessionToken(master, ref.UserID, ref.ID, sessionPurposeRefresh, cur.RefreshCT)
					if err != nil {
						return store.SessionRenewWrite{}, err
					}
					refresh = xoauth2.Secret(tok)
				}
				d := do(ctx, auth.RenewCurrent{
					RefreshToken: refresh, Subject: cur.Subject, LastCheckedAt: cur.LastCheckedAt,
				})
				switch d.Kind {
				case auth.RenewRefuse:
					return store.SessionRenewWrite{Action: store.SessionRenewRefuse}, nil
				case auth.RenewRotate:
					w := store.SessionRenewWrite{Action: store.SessionRenewRotate}
					if !d.RefreshToken.Empty() {
						if w.RefreshCT, err = sealSessionToken(master, ref.UserID, ref.ID, sessionPurposeRefresh, d.RefreshToken.Reveal()); err != nil {
							return store.SessionRenewWrite{}, err
						}
					}
					if !d.IDToken.Empty() {
						if w.IDTokenCT, err = sealSessionToken(master, ref.UserID, ref.ID, sessionPurposeIDToken, d.IDToken.Reveal()); err != nil {
							return store.SessionRenewWrite{}, err
						}
					}
					return w, nil
				default:
					return store.SessionRenewWrite{}, nil
				}
			})
		if err != nil {
			return err
		}
		res = auth.RenewResult{Ran: ran, Revoked: state.Revoked, LastCheckedAt: state.LastCheckedAt, Generation: state.Generation}
		return nil
	})
	if err != nil {
		return auth.RenewResult{}, err
	}
	return res, nil
}

// Confirm stamps the session confirmed (ADR-048 section 4, phase C), in its own
// short transaction outside the renewal lock.
func (s *sessionStore) Confirm(ctx context.Context, ref auth.SessionRef, generation int64) (time.Time, bool, bool, error) {
	var at time.Time
	var ok, revoked bool
	err := s.st.UserTx(ctx, ref.UserID, func(tx *store.UserTx) error {
		var err error
		at, ok, revoked, err = tx.ConfirmSession(ctx, ref.ID, generation)
		return err
	})
	return at, ok, revoked, err
}
