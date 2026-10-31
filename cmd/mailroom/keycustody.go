// SPDX-License-Identifier: AGPL-3.0-only

package main

// The adapter that lets the JMAP own-key methods (#796, internal/api
// user_key_manage.go) reach the crypto process's key verbs without internal/api
// importing the wire, mirroring signer.go and decrypter.go.
//
// ⚠ NOTHING HERE LOGS A REQUEST. Import carries a private key and a passphrase
// in, export carries a passphrase in and a wrapped key out; this file only moves
// them between two structs.

import (
	"context"
	"errors"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/api"
	"mailroom/internal/pgpwire"
)

// custodyWire is the slice of *pgpwire.Client the adapter calls. An interface so
// the error mapping is tested against a fake without a socket.
type custodyWire interface {
	Generate(ctx context.Context, in pgpwire.GenerateRequest) (*pgpwire.GenerateResponse, error)
	Import(ctx context.Context, in pgpwire.ImportRequest) (*pgpwire.ImportResponse, error)
	Extend(ctx context.Context, in pgpwire.ExtendRequest) (*pgpwire.ExtendResponse, error)
	Revoke(ctx context.Context, in pgpwire.RevokeRequest) (*pgpwire.RevokeResponse, error)
	ExportWrapped(ctx context.Context, in pgpwire.ExportWrappedRequest) (*pgpwire.ExportWrappedResponse, error)
}

// cryptoCustody adapts the crypto process to api.KeyCustody.
type cryptoCustody struct{ w custodyWire }

// newCryptoCustody returns key custody for the crypto process at socketPath, or
// nil when no socket is configured (the API then answers
// keyManagementUnavailable, and the capability says keyManagement: false).
func newCryptoCustody(socketPath string) *cryptoCustody {
	if socketPath == "" {
		return nil
	}
	return &cryptoCustody{w: pgpwire.NewClient(socketPath)}
}

// custodyErr turns a crypto-process refusal into *api.CustodyError (code and
// message only); a transport failure passes through for the API to report as a
// fixed "could not be reached".
func custodyErr(err error) error {
	var werr *pgpwire.Error
	if errors.As(err, &werr) {
		return &api.CustodyError{Code: string(werr.Code), Message: werr.Message}
	}
	return err
}

func custodyKey(k pgpwire.KeyInfo) api.CustodyKey {
	return api.CustodyKey{KeyID: k.KeyID, Fingerprint: k.Fingerprint}
}

func (c *cryptoCustody) GenerateKey(ctx context.Context, userID ulid.ULID, name, email string, exp api.KeyExpiry) (api.CustodyKey, error) {
	callCtx, cancel := context.WithTimeout(ctx, pgpwire.DefaultTimeout)
	defer cancel()
	res, err := c.w.Generate(callCtx, pgpwire.GenerateRequest{
		UserID: userID.String(), Name: name, Email: email, ExpiresDays: exp.Days, ExpiresAt: exp.At,
	})
	if err != nil {
		return api.CustodyKey{}, custodyErr(err)
	}
	return custodyKey(res.Key), nil
}

func (c *cryptoCustody) ImportKey(ctx context.Context, userID ulid.ULID, armoredPrivate, passphrase string) (api.CustodyKey, bool, error) {
	callCtx, cancel := context.WithTimeout(ctx, pgpwire.DefaultTimeout)
	defer cancel()
	res, err := c.w.Import(callCtx, pgpwire.ImportRequest{
		UserID: userID.String(), PrivateKey: armoredPrivate, Passphrase: passphrase,
	})
	if err != nil {
		return api.CustodyKey{}, false, custodyErr(err)
	}
	return custodyKey(res.Key), res.AlreadyStored, nil
}

func (c *cryptoCustody) ExtendKey(ctx context.Context, userID, keyID ulid.ULID, exp api.KeyExpiry) (api.CustodyKey, error) {
	callCtx, cancel := context.WithTimeout(ctx, pgpwire.DefaultTimeout)
	defer cancel()
	res, err := c.w.Extend(callCtx, extendRequest(userID, keyID, exp))
	if err != nil {
		return api.CustodyKey{}, custodyErr(err)
	}
	return custodyKey(res.Key), nil
}

func (c *cryptoCustody) RevokeKey(ctx context.Context, userID, keyID ulid.ULID, reason, confirm string) (api.CustodyKey, error) {
	callCtx, cancel := context.WithTimeout(ctx, pgpwire.DefaultTimeout)
	defer cancel()
	res, err := c.w.Revoke(callCtx, pgpwire.RevokeRequest{
		UserID: userID.String(), KeyID: keyID.String(), Reason: reason, Confirm: confirm,
	})
	if err != nil {
		return api.CustodyKey{}, custodyErr(err)
	}
	return custodyKey(res.Key), nil
}

func (c *cryptoCustody) ExportKey(ctx context.Context, userID, keyID ulid.ULID, passphrase string) (api.CustodyKey, string, error) {
	callCtx, cancel := context.WithTimeout(ctx, pgpwire.DefaultTimeout)
	defer cancel()
	res, err := c.w.ExportWrapped(callCtx, pgpwire.ExportWrappedRequest{
		UserID: userID.String(), KeyID: keyID.String(), Passphrase: passphrase,
	})
	if err != nil {
		return api.CustodyKey{}, "", custodyErr(err)
	}
	return custodyKey(res.Key), res.PrivateKey, nil
}

// extendRequest maps an api.KeyExpiry onto the wire's two forms: days from now,
// or an exact instant (#813).
func extendRequest(userID, keyID ulid.ULID, exp api.KeyExpiry) pgpwire.ExtendRequest {
	r := pgpwire.ExtendRequest{UserID: userID.String(), KeyID: keyID.String(), ExpiresAt: exp.At}
	if exp.Days != nil {
		r.Days = *exp.Days
	}
	return r
}
