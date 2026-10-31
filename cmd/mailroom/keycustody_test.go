// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"context"
	"errors"
	"testing"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/api"
	"mailroom/internal/pgpwire"
)

// The API decides on custody error codes and limits it cannot import from the
// wire (internal/api never imports pgpwire). These pin the two sides together:
// a renamed wire code would otherwise turn a clean refusal into serverFail.
func TestCustodyConstantsMatchTheWire(t *testing.T) {
	pairs := map[string]pgpwire.ErrorCode{
		api.CustodyInvalidRequest: pgpwire.CodeInvalidRequest,
		api.CustodyNotFound:       pgpwire.CodeNotFound,
		api.CustodyDenied:         pgpwire.CodeDenied,
		api.CustodyRateLimited:    pgpwire.CodeRateLimited,
	}
	for a, w := range pairs {
		if a != string(w) {
			t.Errorf("api custody code %q != wire code %q", a, w)
		}
	}
	if api.MinExportPassphrase != pgpwire.MinExportPassphrase {
		t.Errorf("api.MinExportPassphrase = %d, pgpwire.MinExportPassphrase = %d", api.MinExportPassphrase, pgpwire.MinExportPassphrase)
	}
	if api.MinKeyLifetime != pgpwire.MinKeyLifetime {
		t.Errorf("api.MinKeyLifetime = %v, pgpwire.MinKeyLifetime = %v", api.MinKeyLifetime, pgpwire.MinKeyLifetime)
	}
	if api.MaxKeyLifetimeDays != pgpwire.MaxKeyLifetimeDays {
		t.Errorf("api.MaxKeyLifetimeDays = %d, pgpwire.MaxKeyLifetimeDays = %d", api.MaxKeyLifetimeDays, pgpwire.MaxKeyLifetimeDays)
	}
}

type fakeCustodyWire struct{ err error }

func (f fakeCustodyWire) Generate(context.Context, pgpwire.GenerateRequest) (*pgpwire.GenerateResponse, error) {
	return nil, f.err
}

func (f fakeCustodyWire) Import(context.Context, pgpwire.ImportRequest) (*pgpwire.ImportResponse, error) {
	return nil, f.err
}

func (f fakeCustodyWire) Extend(context.Context, pgpwire.ExtendRequest) (*pgpwire.ExtendResponse, error) {
	return nil, f.err
}

func (f fakeCustodyWire) Revoke(context.Context, pgpwire.RevokeRequest) (*pgpwire.RevokeResponse, error) {
	return nil, f.err
}

func (f fakeCustodyWire) ExportWrapped(context.Context, pgpwire.ExportWrappedRequest) (*pgpwire.ExportWrappedResponse, error) {
	return nil, f.err
}

// A crypto-process refusal reaches the API as *api.CustodyError with the wire's
// code and message; a transport failure passes through unchanged (the API
// reports it as a fixed sentence).
func TestCryptoCustodyMapsErrors(t *testing.T) {
	ctx := context.Background()
	u, k := ulid.Make(), ulid.Make()
	refusal := pgpwire.Errorf(pgpwire.CodeInvalidRequest, "days must be between 1 and 3650")
	one := 1
	c := &cryptoCustody{w: fakeCustodyWire{err: refusal}}
	calls := map[string]func() error{
		"generate": func() error { _, err := c.GenerateKey(ctx, u, "n", "me@example.com", api.KeyExpiry{}); return err },
		"import":   func() error { _, _, err := c.ImportKey(ctx, u, "x", "y"); return err },
		"extend":   func() error { _, err := c.ExtendKey(ctx, u, k, api.KeyExpiry{Days: &one}); return err },
		"revoke":   func() error { _, err := c.RevokeKey(ctx, u, k, "retired", "ABCDEF12"); return err },
		"export":   func() error { _, _, err := c.ExportKey(ctx, u, k, "a long passphrase"); return err },
	}
	for name, call := range calls {
		var ce *api.CustodyError
		if err := call(); !errors.As(err, &ce) || ce.Code != api.CustodyInvalidRequest || ce.Message != refusal.Message {
			t.Errorf("%s: %v, want *api.CustodyError{invalid_request, %q}", name, err, refusal.Message)
		}
	}
	transport := errors.New("dial unix: no such file")
	c = &cryptoCustody{w: fakeCustodyWire{err: transport}}
	if _, err := c.GenerateKey(ctx, u, "n", "me@example.com", api.KeyExpiry{}); !errors.Is(err, transport) {
		t.Errorf("transport error = %v, want it passed through", err)
	}
	if newCryptoCustody("") != nil {
		t.Error("no socket must mean no custody (so the API reports keyManagementUnavailable)")
	}
}
