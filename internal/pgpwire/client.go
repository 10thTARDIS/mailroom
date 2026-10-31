// SPDX-License-Identifier: AGPL-3.0-only

package pgpwire

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"
)

// DefaultTimeout bounds a single call. Key generation is the slow one and is
// still well under a second on any machine that can run a mail server; the
// timeout exists so that a wedged crypto process shows up as a failed request
// rather than a hung mail send.
const DefaultTimeout = 30 * time.Second

// Client calls the crypto process over its Unix socket.
//
// It can only ever speak to a Unix socket: the dialer is hardcoded, so no
// configuration mistake can point the application's key operations at a
// network address.
type Client struct {
	socketPath string
	http       *http.Client
}

// NewClient returns a client for the daemon listening at socketPath.
func NewClient(socketPath string) *Client {
	return &Client{
		socketPath: socketPath,
		http: &http.Client{
			Timeout: DefaultTimeout,
			Transport: &http.Transport{
				DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
					var d net.Dialer
					return d.DialContext(ctx, "unix", socketPath)
				},
				MaxIdleConns:    4,
				IdleConnTimeout: 90 * time.Second,
			},
		},
	}
}

// SocketPath is where this client dials.
func (c *Client) SocketPath() string { return c.socketPath }

// Health reports whether the crypto process is answering. A nil error means it
// is up; it says nothing about whether any particular key exists.
func (c *Client) Health(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://crypto"+HealthPath, nil)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("crypto process: health: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("crypto process: health: status %d", resp.StatusCode)
	}
	return nil
}

// Call invokes one method. It is the single transport path — Generate and
// ExportPublic below are typed wrappers over it, and the leak test drives it
// directly so that it can walk the whole Methods registry generically rather
// than needing a hand-written call per method.
//
// A failure carried by the daemon arrives as *Error; use errors.As to read its
// Code. A transport failure arrives as an ordinary wrapped error.
func (c *Client) Call(ctx context.Context, m Method, in, out any) error {
	body, err := json.Marshal(in)
	if err != nil {
		return fmt.Errorf("crypto process: %s: encode request: %w", m, err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://crypto"+m.Path(), bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("crypto process: %s: %w", m, err)
	}
	defer func() { _ = resp.Body.Close() }()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, m.MaxResponseBytes()))
	if err != nil {
		return fmt.Errorf("crypto process: %s: read response: %w", m, err)
	}

	if resp.StatusCode != http.StatusOK {
		var eb ErrorBody
		if err := json.Unmarshal(raw, &eb); err == nil && eb.Error != nil {
			return eb.Error
		}
		return fmt.Errorf("crypto process: %s: status %d", m, resp.StatusCode)
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("crypto process: %s: decode response: %w", m, err)
	}
	return nil
}

// Generate creates a fresh key pair for a user and returns its public half.
func (c *Client) Generate(ctx context.Context, in GenerateRequest) (*GenerateResponse, error) {
	var out GenerateResponse
	if err := c.Call(ctx, MethodGenerate, in, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ExportPublic returns the armored public key of a stored key the calling user
// owns. A key belonging to someone else fails with CodeDenied.
func (c *Client) ExportPublic(ctx context.Context, in ExportPublicRequest) (*ExportPublicResponse, error) {
	var out ExportPublicResponse
	if err := c.Call(ctx, MethodExportPublic, in, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Import hands an existing armored private key to the crypto process.
//
// ⚠ The caller is holding private key material at this point and should have
// obtained it from the user, used it here, and dropped it. Do not log in.
// Do not persist it. It does not belong on this side of the socket.
func (c *Client) Import(ctx context.Context, in ImportRequest) (*ImportResponse, error) {
	var out ImportResponse
	if err := c.Call(ctx, MethodImport, in, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// BindIdentity makes a stored key the default signing key for one address.
func (c *Client) BindIdentity(ctx context.Context, in BindIdentityRequest) (*BindIdentityResponse, error) {
	var out BindIdentityResponse
	if err := c.Call(ctx, MethodBindIdentity, in, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Sign returns a detached signature over in.Data made with a stored key the
// calling user owns.
//
// A key belonging to someone else fails with CodeDenied; a retired key
// (decrypt-only or revoked) fails with CodeInvalidRequest — it may still open
// old mail, but it must never put its name on anything new.
func (c *Client) Sign(ctx context.Context, in SignRequest) (*SignResponse, error) {
	var out SignResponse
	if err := c.Call(ctx, MethodSign, in, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// CodeOf reports the wire error code carried by err, if any.
func CodeOf(err error) (ErrorCode, bool) {
	var werr *Error
	if errors.As(err, &werr) {
		return werr.Code, true
	}
	return "", false
}

// ExportWrapped asks for the user's private key, encrypted under a passphrase
// THEY chose.
//
// ⚠ THIS IS THE ONE CALL WHOSE RESPONSE CARRIES PRIVATE KEY MATERIAL, and the
// one whose REQUEST carries the only thing protecting it. Neither belongs
// anywhere a process list or a log can reach: callers read the passphrase from
// a file the operator controls (never argv, never the environment — both are
// world-readable via /proc for the life of the process), and write the result
// straight to a file rather than to stdout.
func (c *Client) ExportWrapped(ctx context.Context, in ExportWrappedRequest) (*ExportWrappedResponse, error) {
	var out ExportWrappedResponse
	if err := c.Call(ctx, MethodExportWrapped, in, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Decrypt opens one PGP/MIME encrypted part with the user's keys.
//
// Every cryptographic failure arrives as CodeDecryptFailed with
// DecryptFailedMessage; a spent budget as CodeRateLimited. Callers must not
// try to tell failures apart — the crypto process deliberately makes that
// impossible — and must never persist Entity (ADR-037).
func (c *Client) Decrypt(ctx context.Context, in DecryptRequest) (*DecryptResponse, error) {
	var out DecryptResponse
	if err := c.Call(ctx, MethodDecrypt, in, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Encrypt encrypts one MIME entity to the given recipients' public keys and to
// the sender's own key. A recipient with no usable encryption key fails with
// CodeNoRecipientKey, whose message names the addresses.
func (c *Client) Encrypt(ctx context.Context, in EncryptRequest) (*EncryptResponse, error) {
	var out EncryptResponse
	if err := c.Call(ctx, MethodEncrypt, in, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Extend re-signs a stored key with a later expiry (#795).
func (c *Client) Extend(ctx context.Context, in ExtendRequest) (*ExtendResponse, error) {
	var out ExtendResponse
	if err := c.Call(ctx, MethodExtend, in, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Revoke revokes a stored key (#795). Irreversible.
func (c *Client) Revoke(ctx context.Context, in RevokeRequest) (*RevokeResponse, error) {
	var out RevokeResponse
	if err := c.Call(ctx, MethodRevoke, in, &out); err != nil {
		return nil, err
	}
	return &out, nil
}
