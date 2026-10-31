// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/pgpkeyd"
	"mailroom/internal/pgpwire"
	"mailroom/internal/store"
	"mailroom/internal/wkd"
)

// `pgp-generate` / `pgp-import` / `pgp-list` / `pgp-bind` / `pgp-publish` /
// `pgp-unpublish` are the operator's only way to reach the crypto process's
// key custody (ADR-024, #47) and the Web Key Directory (#48). They speak
// pgpwire over its Unix socket for everything except listing and publication,
// which read and write app.user_keys directly — the same split GetUserKey's
// doc comment draws: the server (and this CLI, which authenticates as it) may
// read the PUBLIC half of a stored key and decide whether to publish it, but
// only the crypto process may create, import or touch a private one.

// pgpSocketPath resolves --socket, then MAILROOM_PGP_SOCKET, then the
// daemon's own compiled-in default — the same three-way precedence
// pgpkeyd.Config.SocketPath uses on the other end of the socket, so an
// operator who has already set the env var for the daemon does not also have
// to pass it on every CLI invocation.
func pgpSocketPath(flagVal string) string {
	if flagVal != "" {
		return flagVal
	}
	if v := strings.TrimSpace(os.Getenv(pgpkeyd.EnvSocketPath)); v != "" {
		return v
	}
	return pgpkeyd.DefaultSocketPath
}

// addSocketFlag registers --socket with a usage string naming its default,
// out of a helper so the four subcommands below cannot drift on the wording.
func addSocketFlag(fs *flag.FlagSet) *string {
	return fs.String("socket", "",
		"crypto process Unix socket (default $"+pgpkeyd.EnvSocketPath+" or "+pgpkeyd.DefaultSocketPath+")")
}

// cryptoClient dials the crypto process and confirms it actually answers
// before any subcommand touches a flag it parsed correctly. There is no
// fallback for a missing crypto process — ADR-024 keeps private key custody
// out of this binary entirely — so failing here, plainly, is the whole of
// what this CLI can do about it.
func cryptoClient(ctx context.Context, socketPath string) (*pgpwire.Client, error) {
	c := pgpwire.NewClient(socketPath)
	if err := c.Health(ctx); err != nil {
		return nil, fmt.Errorf(
			"the crypto process is not running (or not reachable) at %s — key operations need it "+
				"and there is no fallback: %w", socketPath, err)
	}
	return c, nil
}

// wireCallError turns a failed pgpwire call into an operator-facing message.
//
// A *pgpwire.Error is the daemon's own considered refusal (CodeOf reads its
// code); pgpwire.Error.Message is already written to be safe to print — the
// import path in particular is tested to never quote the bytes it could not
// parse (pgpkeyd/import_test.go) — so this only adds the per-code framing an
// operator needs, rather than the bare "crypto process: invalid_request: …"
// wire format. Anything else reaching here is a transport failure that
// survived the Health probe in cryptoClient (the daemon died mid-call); it is
// passed through rather than reinterpreted.
func wireCallError(op string, err error) error {
	var werr *pgpwire.Error
	if !errors.As(err, &werr) {
		return fmt.Errorf("%s: %w", op, err)
	}
	switch werr.Code {
	case pgpwire.CodeDenied:
		return fmt.Errorf("%s: denied — that key does not belong to this user", op)
	case pgpwire.CodeNotFound:
		return fmt.Errorf("%s: not found — no such key or identity", op)
	case pgpwire.CodeInvalidRequest:
		return fmt.Errorf("%s: rejected: %s", op, werr.Message)
	default:
		return fmt.Errorf("%s: crypto process error: %s", op, werr.Message)
	}
}

// parseULIDFlag parses a required ULID-valued flag, naming it in both the
// missing- and invalid-value cases the way parseUserID does for --user-id.
func parseULIDFlag(flagName, s string) (ulid.ULID, error) {
	if s == "" {
		return ulid.ULID{}, fmt.Errorf("--%s is required", flagName)
	}
	id, err := ulid.Parse(s)
	if err != nil {
		return ulid.ULID{}, fmt.Errorf("--%s %q: not a valid ULID: %w", flagName, s, err)
	}
	return id, nil
}

// runPGPGenerate asks the crypto process for a fresh key pair (#47 increment
// 2). The private half never reaches this process — pgpwire.KeyInfo has no
// field that could carry it — so there is nothing here to protect beyond not
// printing the public half unless asked.
func runPGPGenerate(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("pgp-generate", flag.ContinueOnError)
	var (
		userIDStr   = fs.String("user-id", "", "operator user id (ULID); must already exist")
		email       = fs.String("email", "", "address the key is bound to (required)")
		name        = fs.String("name", "", "display name for the key's User ID packet")
		printPublic = fs.Bool("print-public", false, "print the armored public key (safe to hand out)")
		expiresDays = fs.Int("expires-days", pgpwire.DefaultKeyLifetimeDays,
			"days until the key (primary and encryption subkey) expires; 0 = never")
	)
	socket := addSocketFlag(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	userID, err := parseUserID(*userIDStr)
	if err != nil {
		return err
	}
	if strings.TrimSpace(*email) == "" {
		return errors.New("pgp-generate: --email is required")
	}
	if *expiresDays < 0 || *expiresDays > pgpwire.MaxKeyLifetimeDays {
		return fmt.Errorf("pgp-generate: --expires-days must be between 0 (never) and %d", pgpwire.MaxKeyLifetimeDays)
	}

	callCtx, cancel := context.WithTimeout(ctx, pgpwire.DefaultTimeout)
	defer cancel()
	client, err := cryptoClient(callCtx, pgpSocketPath(*socket))
	if err != nil {
		return err
	}

	resp, err := client.Generate(callCtx, pgpwire.GenerateRequest{
		UserID:      userID.String(),
		Name:        *name,
		Email:       *email,
		ExpiresDays: expiresDays,
	})
	if err != nil {
		return wireCallError("pgp-generate", err)
	}

	fmt.Printf("key %s generated for user %s\n", resp.Key.KeyID, userID)
	fmt.Printf("  fingerprint: %s\n", resp.Key.Fingerprint)
	fmt.Printf("  created:     %s\n", resp.Key.CreatedAt.Format(time.RFC3339))
	fmt.Printf("  expires:     %s\n", usableUntilText(resp.Key.UsableUntil))
	if *printPublic {
		fmt.Println()
		fmt.Println(resp.Key.PublicKey)
	}
	return nil
}

// runPGPImport hands an existing armored PRIVATE key to the crypto process
// (#47 increment 2, pgpwire.MethodImport).
func runPGPImport(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("pgp-import", flag.ContinueOnError)
	userIDStr := fs.String("user-id", "", "operator user id (ULID); must already exist")
	// A FILE PATH ONLY. Deliberately no flag or env var that carries the key
	// material itself: argv is readable by any process on the box via
	// /proc/<pid>/cmdline (or `ps -ef`), and the environment is readable the
	// same way via /proc/<pid>/environ — both survive for as long as this
	// process runs, well past the moment the key was used. A file the operator
	// controls the permissions of, read once and used, does not have that
	// exposure window.
	file := fs.String("file", "", "path to an ASCII-armored PRIVATE key")
	passFile := fs.String("passphrase-file", "",
		"path to a file holding the passphrase of a protected key (e.g. a pgp-export-wrapped file)")
	socket := addSocketFlag(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	userID, err := parseUserID(*userIDStr)
	if err != nil {
		return err
	}
	if strings.TrimSpace(*file) == "" {
		return errors.New("pgp-import: --file is required")
	}

	raw, err := os.ReadFile(*file)
	if err != nil {
		// The PATH is fine to name; the CONTENTS never are, so this wraps err
		// (an os.PathError naming *file) and nothing else.
		return fmt.Errorf("pgp-import: read %s: %w", *file, err)
	}
	key := string(raw)
	// Best-effort cleanup: this zeroes this process's one extra copy of the
	// key bytes once they are folded into the request. It is not a real
	// guarantee — the []byte->string conversion above may already have been
	// copied by the runtime, and `key` itself is an immutable Go string that
	// cannot be zeroed — which is exactly why generate, not import, is the
	// normal path (pgpwire.Client.Import's doc comment).
	for i := range raw {
		raw[i] = 0
	}

	// Same rules as pgp-export-wrapped: from a file, never argv, trailing
	// newlines stripped. No minimum length on import: the key already exists.
	pass := ""
	if strings.TrimSpace(*passFile) != "" {
		praw, err := os.ReadFile(*passFile)
		if err != nil {
			return fmt.Errorf("pgp-import: read %s: %w", *passFile, err)
		}
		pass = strings.TrimRight(string(praw), "\r\n")
		for i := range praw {
			praw[i] = 0
		}
	}

	callCtx, cancel := context.WithTimeout(ctx, pgpwire.DefaultTimeout)
	defer cancel()
	client, err := cryptoClient(callCtx, pgpSocketPath(*socket))
	if err != nil {
		return err
	}

	resp, err := client.Import(callCtx, pgpwire.ImportRequest{
		UserID:     userID.String(),
		PrivateKey: key,
		Passphrase: pass,
	})
	if err != nil {
		return wireCallError("pgp-import", err)
	}
	if resp.AlreadyStored {
		// Not an error: importing a key twice is a no-op the operator should be
		// able to hear as a plain statement, not a failure (ImportResponse doc
		// comment).
		fmt.Printf("key %s: that key is already held (fingerprint %s); nothing changed\n",
			resp.Key.KeyID, resp.Key.Fingerprint)
		return nil
	}
	fmt.Printf("key %s imported for user %s\n", resp.Key.KeyID, userID)
	fmt.Printf("  fingerprint: %s\n", resp.Key.Fingerprint)
	return nil
}

// runPGPList reads the user's keys from the STORE, not the crypto process —
// the public half and metadata are things the application role may see
// directly (store/user_keys.go's package doc comment), and going through the
// socket for a listing would only add a hop for no extra safety.
func runPGPList(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("pgp-list", flag.ContinueOnError)
	userIDStr := fs.String("user-id", "", "operator user id (ULID)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	userID, err := parseUserID(*userIDStr)
	if err != nil {
		return err
	}

	d, err := openDeps(ctx)
	if err != nil {
		return err
	}
	defer d.close()

	var keys []store.UserKey
	if err := d.store.UserTx(ctx, userID, func(tx *store.UserTx) error {
		var e error
		keys, e = tx.ListUserKeys(ctx)
		return e
	}); err != nil {
		return fmt.Errorf("pgp-list: %w", err)
	}
	if len(keys) == 0 {
		fmt.Println("no keys")
		return nil
	}
	// k.PrivateKeyCT is populated by ListUserKeys (it is the sealed ciphertext
	// this process has no way to open) and is DELIBERATELY never referenced
	// below — not even len(k.PrivateKeyCT), which alone would tell an onlooker
	// how large the sealed material is.
	for _, k := range keys {
		bound := "-"
		if k.IdentityID != nil {
			bound = k.IdentityID.String()
		}
		status := "unpublished"
		if k.WKDPublished {
			status = "published"
		}
		if k.DecryptOnly {
			status += ",decrypt-only"
		}
		if k.RevokedAt != nil {
			status += ",revoked"
		}
		fmt.Printf("%s  fpr=%s  identity=%-26s  created=%s  expires=%s  %s\n",
			k.ID, k.Fingerprint, bound, k.CreatedAt.Format(time.RFC3339), keyExpiryText(k.PublicKey), status)
	}
	return nil
}

// runPGPBind makes a stored key the default signing key for one of the
// user's addresses (pgpwire.MethodBindIdentity). The crypto process, not this
// CLI, decides whether the key belongs to the user — see
// TestBindIdentityRefusesAnotherUsersKey in pgpkeyd.
func runPGPBind(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("pgp-bind", flag.ContinueOnError)
	userIDStr := fs.String("user-id", "", "operator user id (ULID)")
	keyIDStr := fs.String("key-id", "", "key id (ULID) to bind, from pgp-list")
	identityIDStr := fs.String("identity-id", "", "identity (address) id (ULID) to bind the key to")
	socket := addSocketFlag(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	userID, err := parseUserID(*userIDStr)
	if err != nil {
		return err
	}
	keyID, err := parseULIDFlag("key-id", *keyIDStr)
	if err != nil {
		return err
	}
	identityID, err := parseULIDFlag("identity-id", *identityIDStr)
	if err != nil {
		return err
	}

	callCtx, cancel := context.WithTimeout(ctx, pgpwire.DefaultTimeout)
	defer cancel()
	client, err := cryptoClient(callCtx, pgpSocketPath(*socket))
	if err != nil {
		return err
	}

	resp, err := client.BindIdentity(callCtx, pgpwire.BindIdentityRequest{
		UserID:     userID.String(),
		KeyID:      keyID.String(),
		IdentityID: identityID.String(),
	})
	if err != nil {
		return wireCallError("pgp-bind", err)
	}
	fmt.Printf("key %s bound to identity %s\n", resp.Key.KeyID, identityID)
	return nil
}

// runPGPPublish puts a bound key in the Web Key Directory (#48).
//
// It goes to the STORE, not the crypto process: publication touches no private
// key material at all — it flips app.user_keys.wkd_published and records the
// identity's WKD hash — so routing it through the socket would widen that
// process's job for nothing.
//
// ⚠ THE HASH IS COMPUTED HERE, FROM THE BOUND IDENTITY'S OWN ADDRESS, and the
// path built from it is printed. Publication's characteristic failure is not an
// error, it is silence: a key published under the wrong directory name commits
// cleanly, reports success, and is simply never found by anyone. The operator
// cannot check that from a "published" message — they can check it by fetching
// the URL below. So the URL is the output, and store.PublishKey independently
// re-derives the same hash and refuses a mismatch.
func runPGPPublish(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("pgp-publish", flag.ContinueOnError)
	userIDStr := fs.String("user-id", "", "operator user id (ULID)")
	keyIDStr := fs.String("key-id", "", "key id (ULID) to publish, from pgp-list")
	if err := fs.Parse(args); err != nil {
		return err
	}
	userID, err := parseUserID(*userIDStr)
	if err != nil {
		return err
	}
	keyID, err := parseULIDFlag("key-id", *keyIDStr)
	if err != nil {
		return err
	}

	d, err := openDeps(ctx)
	if err != nil {
		return err
	}
	defer d.close()

	var (
		address     string
		localPart   string
		domain      string
		fingerprint string
		hash        string
	)
	// ONE transaction: the identity read and the publish must see the same row,
	// or the path printed could describe a binding that changed underneath.
	if err := d.store.UserTx(ctx, userID, func(tx *store.UserTx) error {
		key, found, err := tx.GetUserKey(ctx, keyID)
		if err != nil {
			return err
		}
		if !found {
			return fmt.Errorf("no key %s for user %s (pgp-list shows this user's keys)", keyID, userID)
		}
		fingerprint = key.Fingerprint
		if key.IdentityID == nil {
			return errors.New("that key is not bound to an identity — WKD publishes a key AT an " +
				"address, so run pgp-bind first")
		}

		ids, err := tx.GetIdentities(ctx, []ulid.ULID{*key.IdentityID})
		if err != nil {
			return err
		}
		if len(ids) == 0 {
			return fmt.Errorf("the key is bound to identity %s, which this user does not have", key.IdentityID)
		}
		ident := ids[0]
		if ident.Kind != store.IdentityFixed || ident.Address == "" {
			// PublishKey refuses this too; refusing here as well means the
			// operator gets the reason without a failed write in the log.
			return fmt.Errorf("identity %s is a domain wildcard (%s) — it stands for unboundedly "+
				"many derived aliases, and WKD names a key per address, so there is no single "+
				"path to publish it at", ident.ID, ident.Email())
		}
		address = ident.Address

		localPart, domain, err = wkd.SplitAddress(address)
		if err != nil {
			return fmt.Errorf("the bound address %q does not parse: %w", address, err)
		}
		hash = wkd.HashLocalPart(localPart)

		ok, err := tx.PublishKey(ctx, keyID, hash)
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("no key %s for user %s", keyID, userID)
		}
		return nil
	}); err != nil {
		if errors.Is(err, store.ErrNotPublishable) {
			// Already a sentence naming the reason; framing it as a Go error
			// chain would bury that under "store: publish key:".
			return fmt.Errorf("pgp-publish: %s", strings.TrimPrefix(
				err.Error(), store.ErrNotPublishable.Error()+": "))
		}
		return fmt.Errorf("pgp-publish: %w", err)
	}

	// The DOMAIN COMES FROM THE ADDRESS, not from configuration, because that is
	// where a correspondent's client gets it. If these URLs name a domain this
	// instance does not answer for, that mismatch IS the finding — and printing
	// the configured domain instead would hide it.
	fmt.Printf("key %s published for %s\n", keyID, address)
	fmt.Printf("  fingerprint: %s\n", fingerprint)
	fmt.Printf("  wkd hash:    %s\n", hash)
	fmt.Printf("  a correspondent's client will request, in this order:\n")
	fmt.Printf("    https://openpgpkey.%s%s\n", domain, wkd.AdvancedPath(localPart, domain))
	fmt.Printf("    https://%s%s\n", domain, wkd.DirectPath(localPart))
	fmt.Printf("  this instance answers the DIRECT form. Fetch it: a working publication returns\n")
	fmt.Printf("  200 with binary OpenPGP packets (application/octet-stream), never an armored\n")
	fmt.Printf("  block. ⚠ Clients try the ADVANCED form FIRST, so an openpgpkey.%s that\n", domain)
	fmt.Printf("  resolves but serves the wrong thing breaks lookups the direct form would have\n")
	fmt.Printf("  answered — that subdomain must be absent or fully correct.\n")
	return nil
}

// runPGPUnpublish withdraws a key from the Web Key Directory (#48).
//
// ⚠ It cannot un-publish what a correspondent already fetched and cached, and
// says so: withdrawal stops NEW lookups. "Stop trusting this key" is revocation,
// a different act with a different mechanism.
func runPGPUnpublish(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("pgp-unpublish", flag.ContinueOnError)
	userIDStr := fs.String("user-id", "", "operator user id (ULID)")
	keyIDStr := fs.String("key-id", "", "key id (ULID) to withdraw, from pgp-list")
	if err := fs.Parse(args); err != nil {
		return err
	}
	userID, err := parseUserID(*userIDStr)
	if err != nil {
		return err
	}
	keyID, err := parseULIDFlag("key-id", *keyIDStr)
	if err != nil {
		return err
	}

	d, err := openDeps(ctx)
	if err != nil {
		return err
	}
	defer d.close()

	var found bool
	if err := d.store.UserTx(ctx, userID, func(tx *store.UserTx) error {
		var e error
		found, e = tx.UnpublishKey(ctx, keyID)
		return e
	}); err != nil {
		return fmt.Errorf("pgp-unpublish: %w", err)
	}
	if !found {
		return fmt.Errorf("pgp-unpublish: no key %s for user %s", keyID, userID)
	}

	fmt.Printf("key %s withdrawn from WKD; the address is no longer visible to anonymous lookups\n", keyID)
	fmt.Printf("  this does not reach copies a correspondent already fetched — revoke the key if\n")
	fmt.Printf("  it must stop being trusted.\n")
	return nil
}

// runPGPExportWrapped writes the user's private key out, encrypted under a
// passphrase they choose (#54, spec §9, ADR-024 §4 as amended).
//
// ⚠ WHY THIS EXISTS AT ALL. Until now nothing could get a private key out of
// Mailroom, by design. #54 is what changed that: a hard delete destroys the only
// copy of a GENERATED key, and an export carries a user's PGP-encrypted mail as
// ciphertext with no key to open it. `delete-user --apply` refuses to destroy an
// irreplaceable key and points here.
//
// ⚠ THE PASSPHRASE COMES FROM A FILE, LIKE pgp-import'S KEY, AND FOR THE SAME
// REASON. argv is readable by any process on the box via /proc/<pid>/cmdline,
// and the environment via /proc/<pid>/environ, both for as long as this process
// lives. A `--passphrase` flag would publish the one secret protecting the file
// this command is about to write.
//
// ⚠ AND THE KEY GOES TO A FILE, NEVER TO STDOUT. Stdout gets redirected into
// logs, scrollback and CI artefacts by people who are not thinking about it.
func runPGPExportWrapped(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("pgp-export-wrapped", flag.ContinueOnError)
	userIDStr := fs.String("user-id", "", "the key's owner (ULID)")
	keyID := fs.String("key-id", "", "which key to export (ULID; see pgp-list)")
	passFile := fs.String("passphrase-file", "",
		"path to a file holding the passphrase to encrypt the exported key under")
	out := fs.String("out", "", "path to write the armored, passphrase-encrypted key to")
	socket := addSocketFlag(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	userID, err := parseUserID(*userIDStr)
	if err != nil {
		return err
	}
	if strings.TrimSpace(*keyID) == "" {
		return errors.New("pgp-export-wrapped: --key-id is required")
	}
	if strings.TrimSpace(*passFile) == "" {
		return errors.New("pgp-export-wrapped: --passphrase-file is required")
	}
	if strings.TrimSpace(*out) == "" {
		return errors.New("pgp-export-wrapped: --out is required")
	}

	// ⚠ REFUSE TO OVERWRITE. The output is the only copy of a key that may exist
	// nowhere else; clobbering a previous export is not a mistake anyone can
	// take back. O_EXCL makes the check and the create one atomic step, so a
	// stat-then-create race cannot slip between them.
	f, err := os.OpenFile(*out, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("pgp-export-wrapped: create %s (refusing to overwrite): %w", *out, err)
	}
	wrote := false
	defer func() {
		_ = f.Close()
		if !wrote {
			// A partial file here is a truncated private key that looks like a
			// backup. Better to leave nothing.
			_ = os.Remove(*out)
		}
	}()

	raw, err := os.ReadFile(*passFile)
	if err != nil {
		// The path is fine to name; the contents never are.
		return fmt.Errorf("pgp-export-wrapped: read %s: %w", *passFile, err)
	}
	// ⚠ TRAILING NEWLINES ARE STRIPPED, AND A USER HAS TO KNOW THAT. A file
	// written with `echo secret > f` ends in "\n"; keeping it would make the
	// real passphrase differ by one invisible byte from what the person typed,
	// and they would discover that when GnuPG refused the key months later.
	// Leading whitespace is KEPT — it is a legitimate passphrase character and
	// trimming it would create the same problem in the other direction.
	pass := strings.TrimRight(string(raw), "\r\n")
	for i := range raw {
		raw[i] = 0
	}
	if len(pass) < pgpwire.MinExportPassphrase {
		return fmt.Errorf("pgp-export-wrapped: the passphrase in %s is %d characters; "+
			"at least %d are required. It is the only thing protecting the exported key, "+
			"and nothing in Mailroom can help you open it later",
			*passFile, len(pass), pgpwire.MinExportPassphrase)
	}

	callCtx, cancel := context.WithTimeout(ctx, pgpwire.DefaultTimeout)
	defer cancel()
	client, err := cryptoClient(callCtx, pgpSocketPath(*socket))
	if err != nil {
		return err
	}
	resp, err := client.ExportWrapped(callCtx, pgpwire.ExportWrappedRequest{
		UserID: userID.String(), KeyID: *keyID, Passphrase: pass,
	})
	if err != nil {
		return wireCallError("pgp-export-wrapped", err)
	}

	if _, err := f.WriteString(resp.PrivateKey); err != nil {
		return fmt.Errorf("pgp-export-wrapped: write %s: %w", *out, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("pgp-export-wrapped: close %s: %w", *out, err)
	}
	wrote = true

	fmt.Printf("key %s exported for user %s\n", resp.Key.KeyID, userID)
	fmt.Printf("  fingerprint : %s\n", resp.Key.Fingerprint)
	fmt.Printf("  written to  : %s (mode 0600)\n", *out)
	fmt.Println()
	fmt.Println("⚠ This file is a PRIVATE KEY. It is encrypted under the passphrase you")
	fmt.Println("  supplied, and nothing in Mailroom can open it or recover that passphrase.")
	fmt.Println("  Lose the passphrase and the key is gone with it.")
	fmt.Println()
	fmt.Println("  Check it opens BEFORE relying on it:")
	fmt.Printf("    gpg --import %s\n", *out)
	return nil
}
