// SPDX-License-Identifier: AGPL-3.0-only

package main

// The adapter that lets the composer sign without knowing the crypto process
// exists (#47 increment 5, ADR-024).
//
// internal/compose declares a one-method Signer interface and nothing else; the
// pgpwire client lives here. That keeps the compose package free of the crypto
// client, its socket, and its error taxonomy — and means a test of the send path
// substitutes a function rather than standing up a daemon.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/compose"
	"mailroom/internal/pgpwire"
	"mailroom/internal/store"
)

// cryptoSigner adapts *pgpwire.Client to compose.Signer.
type cryptoSigner struct {
	client *pgpwire.Client
	log    *slog.Logger
}

// newCryptoSigner returns a signer for the crypto process at socketPath, or nil
// when no socket is configured.
//
// ⚠ Returning nil is a real answer, not a failure: an unconfigured socket means
// "do not sign", and compose treats a nil Signer as exactly that. Constructing a
// client that would fail on first use instead would turn an operator choice into
// a broken send path discovered by a user.
func newCryptoSigner(socketPath string, log *slog.Logger) *cryptoSigner {
	if socketPath == "" {
		return nil
	}
	return &cryptoSigner{client: pgpwire.NewClient(socketPath), log: log}
}

// Sign asks the crypto process for a detached signature.
//
// ⚠ ERROR CLASSIFICATION LIVES HERE, and it is not the requeue/fail split the
// original plan described. That plan assumed signing happened inside SendWorker,
// where a claim has already moved the submission out of `scheduled` and quota is
// reserved, so only a requeue could release both. Signing now happens at submit
// time, BEFORE any of that exists — there is nothing to requeue and nothing to
// release, so the classification that survives is about what to TELL the caller
// and what to shout about:
//
//   - CodeDenied is logged LOUDLY at Error. It means the crypto process refused
//     a key that is not this user's, which cannot happen through any legitimate
//     path: the key id comes from SigningKeyForIdentity, scoped to the user's own
//     rows under RLS. A denial therefore means a confused deputy in OUR code, and
//     it is the one outcome here that is a security signal rather than a fault.
//   - CodeNotFound and CodeInvalidRequest are permanent for this message —
//     a key that has vanished or been retired between binding and sending. The
//     send fails and the client is told; retrying identical bytes would fail
//     identically.
//   - A transport error or CodeInternal is transient: the crypto process is
//     down, restarting, or briefly broken. The submission was never created, so
//     the client's retry IS the retry.
//
// Every case returns an error, because at submit time there is no half-state to
// clean up. What differs is what the operator sees in the log.
func (s *cryptoSigner) Sign(ctx context.Context, userID, keyID ulid.ULID, data []byte) (string, string, error) {
	callCtx, cancel := context.WithTimeout(ctx, pgpwire.DefaultTimeout)
	defer cancel()

	res, err := s.client.Sign(callCtx, pgpwire.SignRequest{
		UserID: userID.String(),
		KeyID:  keyID.String(),
		Data:   data,
	})
	if err != nil {
		code, ok := pgpwire.CodeOf(err)
		switch {
		case ok && code == pgpwire.CodeDenied:
			// ⚠ Not a normal failure. See above.
			s.log.Error("CONFUSED DEPUTY: the crypto process refused a signing key as not "+
				"belonging to the requesting user — the key id came from that user's own "+
				"rows under RLS, so this is a bug in Mailroom, not a user error",
				"user_id", userID.String(), "key_id", keyID.String(), "err", err)
		case ok && (code == pgpwire.CodeNotFound || code == pgpwire.CodeInvalidRequest):
			s.log.Warn("the signing key is gone or no longer usable; this send cannot be signed",
				"user_id", userID.String(), "key_id", keyID.String(), "code", string(code), "err", err)
		default:
			// Transport, or CodeInternal. Transient by nature.
			s.log.Error("the crypto process could not sign this message; it may be down",
				"user_id", userID.String(), "key_id", keyID.String(), "err", err)
		}
		return "", "", fmt.Errorf("crypto process: sign: %w", err)
	}
	return res.Signature, res.MicAlg, nil
}

// Encrypt asks the crypto process to encrypt one MIME entity to the chosen
// recipient keys and to the sender's own (#47, ADR-038). cryptoSigner is also the
// composer's compose.Encrypter: one adapter over one socket.
//
// Errors: a recipient the crypto process cannot encrypt to (CodeNoRecipientKey,
// which can only happen if a key expired or was revoked between the application's
// pre-check and now, or if the two disagree) becomes *compose.MissingKeysError
// naming the addresses; anything else is wrapped and the send fails. There is no
// fallback to plaintext.
func (s *cryptoSigner) Encrypt(ctx context.Context, userID, keyID ulid.ULID, to []compose.EncryptTo, entity []byte) (string, error) {
	callCtx, cancel := context.WithTimeout(ctx, pgpwire.DefaultTimeout)
	defer cancel()

	recipients := make([]pgpwire.EncryptRecipient, 0, len(to))
	for _, t := range to {
		recipients = append(recipients, pgpwire.EncryptRecipient{Address: t.Address, PublicKey: t.PublicKey})
	}
	res, err := s.client.Encrypt(callCtx, pgpwire.EncryptRequest{
		UserID:     userID.String(),
		KeyID:      keyID.String(),
		Recipients: recipients,
		Entity:     entity,
	})
	if err != nil {
		code, ok := pgpwire.CodeOf(err)
		switch {
		case ok && code == pgpwire.CodeNoRecipientKey:
			var addrs []string
			var werr *pgpwire.Error
			if errors.As(err, &werr) {
				if _, list, found := strings.Cut(werr.Message, ": "); found {
					for _, a := range strings.Split(list, ", ") {
						if a = strings.TrimSpace(a); a != "" {
							addrs = append(addrs, a)
						}
					}
				}
			}
			s.log.Warn("the crypto process found no usable encryption key for a recipient",
				"user_id", userID.String(), "addresses", addrs)
			return "", &compose.MissingKeysError{Addresses: addrs}
		case ok && code == pgpwire.CodeDenied:
			s.log.Error("CONFUSED DEPUTY: the crypto process refused the sender's key as not "+
				"belonging to the requesting user",
				"user_id", userID.String(), "key_id", keyID.String(), "err", err)
		default:
			s.log.Error("the crypto process could not encrypt this message",
				"user_id", userID.String(), "key_id", keyID.String(), "code", string(code), "err", err)
		}
		return "", fmt.Errorf("crypto process: encrypt: %w", err)
	}
	for _, t := range res.To {
		s.log.Info("message encrypted to recipient key",
			"user_id", userID.String(), "address", t.Address, "fingerprint", t.Fingerprint)
	}
	return res.Ciphertext, nil
}

// reportUnsignedDowngrade is the boot-time answer to "is anything being sent
// unsigned that was supposed to be signed?" (#301 C3).
//
// # Why silence here was the wrong default
//
// A nil signer is a legitimate operator choice and the common case, so
// newCryptoSigner treats it as an answer rather than a failure and
// compose.signSubmission makes it a no-op. But the no-op fires EVEN WHEN
// SigningKeyForIdentity would have returned a bound key: the send succeeds,
// unsigned, and the log cannot be distinguished from "this user has no key".
// Meanwhile the correspondent was told through WKD and Autocrypt to expect a
// signature, so the visible effect lands entirely at the far end, where nobody
// running this instance will hear about it.
//
// That made it the ONE quiet degradation in a boundary whose every other
// failure is loud by design: the crypto process being down fails the send
// inside the transaction (ADR-024's no-fallback rule), a foreign key is refused
// and shouted about, a retired key is refused. Unsetting one environment
// variable should not be the exception.
//
// # Why it counts instead of asserting
//
// "You have PGP configured somewhere" is not the question; "how many addresses
// stop being signed for" is. The count is built from the SAME predicate the
// send path uses — ListIdentities, then SigningKeyForIdentity, then the
// Kind == pgp filter compose applies — so N is the number of identities that
// would actually have produced a signature, not a proxy for it. A zero is
// reported too, next to the number of users scanned: "0 identities across 4
// users" and "0 identities across 0 users" are different findings, and a lone
// silence cannot tell them apart (CLAUDE.md — a bounded or empty result must
// report its bound).
//
// # What it does not cover
//
// It is a BOOT-time reading. Binding a key on a running instance that has no
// socket configured stays silent until the next restart. Closing that would
// mean a SigningKeyForIdentity lookup on every send of every unsigned instance
// — the default deployment — to catch a case that requires the operator to
// have run the bind CLI against a crypto process the server was not told
// about. Judged not worth the hot-path cost; if this ever bites, the honest fix
// is a warning inside signSubmission, not a periodic poll.
//
// Never fatal: this is a diagnostic, and an instance must not fail to boot
// because it could not count. A failure to read is itself reported, because
// "the check could not run" and "the check found nothing" must not look alike.
func reportUnsignedDowngrade(ctx context.Context, st *store.Store, log *slog.Logger) {
	identities, users, err := countBoundSigningKeys(ctx, st)
	if err != nil {
		log.Warn("could not determine whether any identity has a bound signing key; "+
			"if one does, its mail is now going out unsigned without further notice",
			"err", err)
		return
	}
	if identities == 0 {
		log.Info("no identity has a bound signing key, so nothing is being sent unsigned "+
			"that would otherwise have been signed",
			"identities_with_bound_keys", 0, "users_scanned", users)
		return
	}
	log.Warn("SENDING UNSIGNED MAIL FOR IDENTITIES THAT HAVE A SIGNING KEY: "+
		"these addresses have a bound PGP key and no crypto socket is configured, so their "+
		"outgoing mail goes out unsigned while correspondents were told (via WKD/Autocrypt) "+
		"to expect a signature",
		"identities_with_bound_keys", identities, "users_scanned", users,
		"hint", "set MAILROOM_PGP_SOCKET to the crypto process's socket, or unbind the keys")
}

// countBoundSigningKeys returns how many identities across all active users
// have a usable PGP signing key bound, and how many users were scanned.
//
// It is composed from the existing per-user reads rather than a new cross-user
// query on purpose. app.identities is user-scoped under RLS and a SystemTx has
// no identity at all (CLAUDE.md: fail closed — no identity, no rows), so a
// single count would need a new SECURITY DEFINER function — a permanent widening
// of the identity-less surface for a boot-time diagnostic. ListActiveUserIDs is
// the narrow definer that already exists (user IDS only, never mail), and the
// per-user reads then run under each user's own identity, exactly as the send
// path does.
//
// The cost is one transaction per active user, once, and only on instances that
// have NO signer configured — an instance that is signing never runs this.
func countBoundSigningKeys(ctx context.Context, st *store.Store) (identities, users int, err error) {
	ids, err := st.ListActiveUserIDs(ctx)
	if err != nil {
		return 0, 0, fmt.Errorf("list active users: %w", err)
	}
	for _, userID := range ids {
		if err := st.UserTx(ctx, userID, func(tx *store.UserTx) error {
			rows, err := tx.ListIdentities(ctx)
			if err != nil {
				return err
			}
			for _, ident := range rows {
				key, ok, err := tx.SigningKeyForIdentity(ctx, ident.ID)
				if err != nil {
					return err
				}
				// The Kind filter mirrors compose.signSubmission's: an S/MIME
				// key bound to an identity (#69, 2.x) is not something the PGP
				// path would have signed with, so counting it would overstate
				// what is being lost.
				if ok && key.Kind == store.KeyPGP {
					identities++
				}
			}
			return nil
		}); err != nil {
			return 0, 0, fmt.Errorf("count bound signing keys for user %s: %w", userID, err)
		}
	}
	return identities, len(ids), nil
}
