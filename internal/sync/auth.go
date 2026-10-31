// SPDX-License-Identifier: AGPL-3.0-only

package sync

import (
	"context"
	"errors"
	"fmt"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"

	"mailroom/internal/xoauth2"
)

// Auth is how Dial authenticates once connected (ADR-046). It is mailroom's own
// type: the only implementations are this package's, built with XOAuth2Auth, and
// no go-imap or go-sasl type crosses the seam.
//
// A nil Config.Auth means password LOGIN with Config.Username/Config.Password,
// EXACTLY as before this type existed. Password accounts never set it.
type Auth interface {
	// mechanism names the SASL mechanism, for errors and tests. Never secret.
	mechanism() string
	validate() error
	authenticate(ctx context.Context, c *imapclient.Client) error
}

// XOAuth2Auth authenticates with AUTHENTICATE XOAUTH2 as user, asking token for
// a current access token at the moment of authentication (not when the Config
// is built, so a Config reused across a long pass never carries a stale token).
//
// A refusal of the token is reported back through token.Reject and the
// authentication is retried ONCE with a fresh token (ADR-046 §5): a cached
// token the provider has already revoked must not decide the grant's fate when
// a refresh would have succeeded.
func XOAuth2Auth(user string, token xoauth2.TokenSource) Auth {
	// A pointer, so a Config holding it stays comparable (a struct holding an
	// arbitrary interface value may not be, and == on it would panic).
	return &xoauth2Auth{user: user, token: token}
}

type xoauth2Auth struct {
	user  string
	token xoauth2.TokenSource
}

func (xoauth2Auth) mechanism() string { return "XOAUTH2" }

func (a xoauth2Auth) validate() error {
	if a.user == "" {
		return errors.New("sync: config: XOAUTH2 user is required")
	}
	if a.token == nil {
		return errors.New("sync: config: XOAUTH2 token source is required")
	}
	return nil
}

// errMechanismUnsupported is returned when the server does not advertise
// AUTH=XOAUTH2. It is a CONFIGURATION refusal (wrong host, or the provider
// turned the mechanism off), never a rejected credential: classifyRefusal maps
// it to ErrorKindConfig.
var errMechanismUnsupported = errors.New("server does not offer AUTH=XOAUTH2")

// errBadXOAuth2Input is an empty token or a user/token that cannot be encoded.
// Nothing was sent; classifyRefusal maps it to ErrorKindConfig.
var errBadXOAuth2Input = errors.New("cannot build the XOAUTH2 response")

// authRefusedError marks a refusal of the AUTHENTICATE command itself. It
// exists so the XOAUTH2 path is classified as ErrorKindAuth by what it IS — the
// server's answer to our credentials — rather than by which Op the caller
// happened to wrap it under (#888 is that bug for LOGIN). challengeStatus is
// the "status" of the server's failure challenge when one was sent, and is the
// only part of that challenge that ever reaches an error string.
//
// token is the refused token, so authenticate can hand it to the source's
// Reject. It is a Held (a pointer, the unexported-field rule on
// xoauth2.Secret) and never part of Error().
type authRefusedError struct {
	challengeStatus string
	err             error
	token           xoauth2.Held
	// retried marks the SECOND refusal: the first token was handed back and
	// the source issued another (a refresh), and the server refused that one
	// too. It is what ErrIMAPAccessRefused means.
	retried bool
}

// ErrIMAPAccessRefused matches an XOAUTH2 refusal of a token the provider had
// just issued in place of a refused one (ADR-046 §12). The grant is evidently
// fine — the token endpoint honoured it — so the likely cause is IMAP itself
// being switched off for the mailbox: Outlook.com's "POP and IMAP" toggle is
// off by default, and a Microsoft 365 administrator can disable IMAP per
// mailbox. Callers turn it into the profile's hint
// (provider.IMAPAccessHint); it is still ErrorKindAuth.
var ErrIMAPAccessRefused = errors.New("sync: IMAP refused a freshly issued token")

// Is reports ErrIMAPAccessRefused for a refusal of the retried token.
func (e *authRefusedError) Is(target error) bool {
	return target == ErrIMAPAccessRefused && e.retried
}

func (e *authRefusedError) Error() string {
	if e.challengeStatus != "" {
		return fmt.Sprintf("XOAUTH2 refused (status %s): %v", e.challengeStatus, e.err)
	}
	return fmt.Sprintf("XOAUTH2 refused: %v", e.err)
}

func (e *authRefusedError) Unwrap() error { return e.err }

func (a xoauth2Auth) authenticate(ctx context.Context, c *imapclient.Client) error {
	// Settle the greeting first, so a server that greets with NO/BYE or drops
	// the connection is reported as that, not as a refused token below. Dial
	// already did (#888), and WaitGreeting is idempotent; this keeps the
	// mechanism correct on its own.
	if err := c.WaitGreeting(); err != nil {
		return err
	}
	if !c.Caps().Has(imap.Cap("AUTH=XOAUTH2")) {
		return errMechanismUnsupported
	}
	err := a.attempt(ctx, c)
	var refused *authRefusedError
	if !errors.As(err, &refused) {
		return err
	}
	// The server refused the token. If it was a cached one the provider has
	// since revoked or expired early, a refresh may well succeed, so drop it
	// (Reject) and try exactly once more on the same connection — a failed
	// AUTHENTICATE leaves the session in the not-authenticated state, where
	// another AUTHENTICATE is legal. The second answer is final: no loop. If
	// the refresh itself fails, that error (needs reauth, operator error,
	// transient) is the truer verdict and is what the caller classifies.
	a.token.Reject(refused.token.Get())
	err = a.attempt(ctx, c)
	// The final refusal is handed back too (security review L1 of #897, the
	// SMTP finding applied here as well): a token refused twice must not stay
	// cached for the next connection to be refused with again.
	if errors.As(err, &refused) {
		a.token.Reject(refused.token.Get())
		refused.retried = true
	}
	return err
}

// attempt is one AUTHENTICATE XOAUTH2 with a token fresh from the source.
func (a xoauth2Auth) attempt(ctx context.Context, c *imapclient.Client) error {
	tok, err := a.token.Token(ctx)
	if err != nil {
		// Already one of xoauth2's sentinels (oauthmail wraps them), which
		// classifyRefusal reads directly. Never the token itself: there is none.
		return fmt.Errorf("access token: %w", err)
	}
	ir, err := xoauth2.InitialResponse(a.user, tok)
	if err != nil {
		return fmt.Errorf("%w: %v", errBadXOAuth2Input, err)
	}
	sc := &saslXOAuth2{ir: ir}
	if err := c.Authenticate(sc); err != nil {
		var imapErr *imap.Error
		switch {
		case sc.challenged:
			// The server sent the failure challenge: it has looked at the token
			// and refused it, whatever it answers (NO, or BAD to the empty
			// response some servers dislike). That is a credential verdict.
			return &authRefusedError{challengeStatus: sc.status, err: err, token: xoauth2.Hold(tok)}
		case errors.As(err, &imapErr) && imapErr.Type == imap.StatusResponseTypeNo:
			// Microsoft's documented refusal: a bare `NO AUTHENTICATE failed.`
			// with no response code and no challenge. A NO to AUTHENTICATE is
			// the server's answer to the credentials.
			return &authRefusedError{err: err, token: xoauth2.Hold(tok)}
		}
		// BAD without a challenge (mechanism syntax), a dropped connection or a
		// timeout: not a credential verdict. Classified as usual.
		return err
	}
	return nil
}

// saslXOAuth2 is the go-sasl client for XOAUTH2. It is unexported: go-sasl
// stays inside this package like go-imap does.
type saslXOAuth2 struct {
	ir         []byte
	challenged bool
	status     string
}

// The initial response contains the token, so the client never renders it.
// (It is a local in authenticate and is never stored in another struct, so
// the unexported-field rule on xoauth2.Secret does not reach it.)
func (s *saslXOAuth2) String() string             { return "saslXOAuth2{[redacted]}" }
func (s *saslXOAuth2) GoString() string           { return s.String() }
func (s *saslXOAuth2) Format(f fmt.State, _ rune) { _, _ = f.Write([]byte(s.String())) }

// Start implements sasl.Client.
func (s *saslXOAuth2) Start() (string, []byte, error) { return "XOAUTH2", s.ir, nil }

// Next implements sasl.Client. The only challenge XOAUTH2 defines is the
// failure challenge, and the protocol requires an EMPTY response to it; the
// server then completes the command with its refusal. Returning an error here
// instead would leave go-imap's AUTHENTICATE waiting on a server that is
// waiting on us.
func (s *saslXOAuth2) Next(challenge []byte) ([]byte, error) {
	s.challenged = true
	if st, ok := xoauth2.ChallengeStatus(challenge); ok {
		s.status = st
	}
	return []byte{}, nil
}
