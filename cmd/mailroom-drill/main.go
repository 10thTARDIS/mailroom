// SPDX-License-Identifier: AGPL-3.0-only

// Command mailroom-drill is the proving half of the restore drill (spec §11,
// ADR-005): "a backup that hasn't restored is a hypothesis".
//
// scripts/restore-drill.sh restores an encrypted backup artifact into a scratch
// Postgres and a scratch blob store, then runs THIS tool against it to prove the
// restore is ALIVE rather than merely present:
//
//	verify  --user-id U   1. decrypt one of the user's messages with the
//	                         ESCROWED master key and re-check its content hash;
//	                      2. open that user's sealed account credentials (the
//	                         wrapped key store, which lives in Postgres);
//	                      3. rebuild the user's search index FROM POSTGRES;
//	                      4. run one search and require a hit;
//	                      5. prove the SECOND escrowed root is present, well
//	                         formed, not the master key again, matches this
//	                         database's OWN recorded check value
//	                         (app.pgp_root_check, migration 00055, #525) if it
//	                         has one, and — for every PGP private key this user
//	                         has stored — actually unseals it and checks the
//	                         fingerprint (ADR-024).
//
// Step 5 exists because steps 1-4 all turn on one secret. Since the crypto
// process arrived there are two, and the second one's loss is invisible to
// every other check here: the mailbox restores, opens, searches and reads
// perfectly, and only the mail encrypted to the user's PGP keys is gone. A
// drill that stopped at step 4 would print a clean bill of health over exactly
// that hole — which is the class of thing this drill exists to prevent.
//
//	seed    --subject S   Fixture mode only: provision a user, an account with
//	                      REAL sealed credentials, a mailbox, and N REAL
//	                      envelope-encrypted messages, through the same
//	                      ingest/blob path the sync engine uses. Never point this
//	                      at a real database.
//
//	                      --pgp-root-key-file (or MAILROOM_PGP_ROOT_KEY_FILE) is
//	                      OPTIONAL: if given, seed also mints a real OpenPGP key
//	                      for the user, sealed exactly the way the crypto process
//	                      seals one, and stores it in app.user_keys — AND records
//	                      app.pgp_root_check's one row (migration 00055, #525),
//	                      exactly as the crypto process's own first boot against a
//	                      database would. Without it, seed creates neither — a
//	                      fixture with zero PGP keys and no recorded check value
//	                      means `verify` step 5 can only report the escrow leg
//	                      UNPROVEN, so the self-test's own fixture must pass this
//	                      flag or it never exercises the unseal OR the check-value
//	                      match code at all.
//
// Why a separate binary rather than a cmd/mailroom-admin subcommand: `verify`
// needs the Tantivy engine (CGO, -tags tantivy) to rebuild a real index, and the
// Dockerfile deliberately builds mailroom-admin CGO-free so a broken search build
// can never block a migration. Keeping the drill out of the shipped image also
// keeps `seed` — which writes synthetic mail — off the production box entirely.
// It is still part of the module, so `go build ./...` and the linters hold it to
// the same bar as everything else, and an incompatible change to internal/ingest
// or internal/search breaks the drill at BUILD time rather than at 3am.
//
// Configuration (shared with the server, internal/config):
//
//	MAILROOM_DATABASE_URL              mailroom_app DSN (required)
//	MAILROOM_DATABASE_PASSWORD_FILE    the DSN's password, injected in memory (#537)
//	MAILROOM_MASTER_KEY_FILE           the ESCROWED master key (ADR-005)
//	MAILROOM_PGP_ROOT_KEY_FILE         the ESCROWED PGP root — a DIFFERENT secret
//	                                   (ADR-024); --no-pgp-root skips it and says so
//	MAILROOM_BLOB_ROOT                 restored blob store
//	MAILROOM_SEARCH_ROOT               scratch index root (rebuilt, never restored)
//
// Every check runs as mailroom_app under RLS, through store.UserTx — the drill
// proves the restore is usable BY THE APPLICATION, not merely readable by a
// superuser.
package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
	"unicode"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/ProtonMail/go-crypto/openpgp/packet"
	"github.com/oklog/ulid/v2"

	"mailroom/internal/blob"
	"mailroom/internal/config"
	"mailroom/internal/crypto"
	"mailroom/internal/ingest"
	"mailroom/internal/onboarding"
	"mailroom/internal/pgdsn"
	"mailroom/internal/pgpkeyd"
	"mailroom/internal/search"
	"mailroom/internal/search/tantivy"
	"mailroom/internal/store"
	"mailroom/internal/syncrunner"
)

// blobPathInfo is the HKDF label for the blob store's per-user path-derivation
// key (an HMAC key, not an encryption key). It MUST match cmd/mailroom and
// cmd/mailroom-admin: a restore that derived different paths would find no blobs
// at all, which is exactly the kind of silent break this drill exists to catch.
const blobPathInfo = "mailroom-blob-path-v1"

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "mailroom-drill: "+err.Error())
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: mailroom-drill <seed|verify> [flags]")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	switch args[0] {
	case "seed":
		return runSeed(ctx, args[1:])
	case "verify":
		return runVerify(ctx, args[1:])
	default:
		return fmt.Errorf("unknown subcommand %q; want seed|verify", args[0])
	}
}

// settings is the drill's own slice of the server's configuration.
//
// It deliberately does NOT use config.Load(): that validates the SERVER's auth
// block and fails without an OIDC issuer or a dev-auth subject. Demanding an
// identity provider before you are allowed to decrypt a backup would be an
// absurd thing to discover at 3am, and a restore target has no auth config at
// all. The env NAMES and defaults are still config's, so there is one source of
// truth for them.
type settings struct {
	databaseURL string
	// masterKeyFile is the first root: it covers the blobs, the sealed
	// credentials, and the blob PATHS.
	masterKeyFile string
	// pgpRootKeyFile is the SECOND root (ADR-024), covering the PGP private
	// keys. It is a separate secret on purpose, and its loss is invisible to
	// every other check in this drill.
	pgpRootKeyFile string
	blobRoot       string
	searchRoot     string
}

// env reads NAME, honoring the Docker-secret NAME_FILE indirection the rest of
// Mailroom uses.
func env(name, def string) (string, error) {
	if path := strings.TrimSpace(os.Getenv(name + "_FILE")); path != "" {
		b, err := os.ReadFile(path)
		if err != nil {
			return "", fmt.Errorf("read %s_FILE (%s): %w", name, path, err)
		}
		return strings.TrimSpace(string(b)), nil
	}
	if v := strings.TrimSpace(os.Getenv(name)); v != "" {
		return v, nil
	}
	return def, nil
}

func load() (settings, error) {
	var s settings
	var err error
	if s.databaseURL, err = env(config.EnvDatabaseURL, ""); err != nil {
		return s, err
	}
	if s.databaseURL == "" {
		return s, fmt.Errorf("%s is required", config.EnvDatabaseURL)
	}
	// The URL itself carries no password (#537): MAILROOM_DATABASE_PASSWORD_FILE
	// names the mailroom_app password secret, injected here in memory.
	if s.databaseURL, err = pgdsn.WithPasswordFile(s.databaseURL, strings.TrimSpace(os.Getenv(config.EnvDatabasePasswordFile))); err != nil {
		return s, err
	}
	if s.masterKeyFile, err = env(config.EnvMasterKeyFile, config.DefaultMasterKeyFile); err != nil {
		return s, err
	}
	if s.pgpRootKeyFile, err = env(pgpkeyd.EnvRootKeyFile, pgpkeyd.DefaultRootKeyFile); err != nil {
		return s, err
	}
	if s.blobRoot, err = env(config.EnvBlobRoot, config.DefaultBlobRoot); err != nil {
		return s, err
	}
	if s.searchRoot, err = env(config.EnvSearchRoot, config.DefaultSearchRoot); err != nil {
		return s, err
	}
	return s, nil
}

// deps are the runtime handles both subcommands need. The master key is the
// ESCROWED one: nothing here can work without it, which is the whole point.
type deps struct {
	log    *slog.Logger
	cfg    settings
	store  *store.Store
	blobs  blob.BlobStore
	master []byte
	close  func()
}

func open(ctx context.Context) (*deps, error) {
	cfg, err := load()
	if err != nil {
		return nil, err
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))

	master, err := crypto.NewFileKeyProvider(cfg.masterKeyFile).MasterKey(ctx)
	if err != nil {
		// The single most likely 3am failure: the escrow was never retrieved, or
		// the wrong copy was. Say so in those words.
		return nil, fmt.Errorf("master key from %s: %w\n"+
			"  This is what a MISSING escrowed master key looks like (ADR-005).\n"+
			"  The backup artifact never contains it; retrieve it from offline escrow",
			cfg.masterKeyFile, err)
	}

	st, err := store.Open(ctx, cfg.databaseURL)
	if err != nil {
		crypto.Zero(master)
		return nil, fmt.Errorf("open store: %w", err)
	}

	pathKey, err := crypto.DeriveKey(master, nil, blobPathInfo)
	if err != nil {
		st.Close()
		crypto.Zero(master)
		return nil, fmt.Errorf("derive blob path key: %w", err)
	}
	defer crypto.Zero(pathKey)

	blobs, err := blob.NewFSBlobStore(cfg.blobRoot, pathKey)
	if err != nil {
		st.Close()
		crypto.Zero(master)
		return nil, fmt.Errorf("open blob store at %s: %w", cfg.blobRoot, err)
	}

	d := &deps{log: log, cfg: cfg, store: st, blobs: blobs, master: master}
	d.close = func() {
		st.Close()
		crypto.Zero(d.master)
	}
	return d, nil
}

// mailDEK derives the per-user mail-blob key exactly as the sync path does
// (syncrunner.MailBlobInfoV1). Reusing the constant rather than restating it is
// deliberate: a drift between the key mail was WRITTEN with and the key the drill
// READS with would make a healthy backup look broken, or a broken one look fine.
func (d *deps) mailDEK(userID ulid.ULID) ([]byte, error) {
	return crypto.DeriveKey(d.master, userID[:], syncrunner.MailBlobInfoV1)
}

// --- seed --------------------------------------------------------------------

func runSeed(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("seed", flag.ContinueOnError)
	subject := fs.String("subject", "drill-operator", "OIDC subject of the fixture user")
	email := fs.String("email", "operator@example.com", "email of the fixture user")
	account := fs.String("account", "fixture", "account name")
	canary := fs.String("canary", "", "word planted in every message body (required)")
	count := fs.Int("messages", 3, "how many messages to ingest")
	// OPTIONAL, unlike --master-key-file (implicit via MAILROOM_MASTER_KEY_FILE)
	// which seed has always required. seed worked with zero PGP keys before
	// ADR-024 increment 3 and must keep working that way when the caller has no
	// PGP root to hand it — the flag only adds a key when it is actually given.
	pgpRootKeyFile := fs.String("pgp-root-key-file", "",
		"escrowed PGP root key FILE PATH (optional; also honors MAILROOM_PGP_ROOT_KEY_FILE) — "+
			"if set, seed also mints one real OpenPGP key for the user")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if strings.TrimSpace(*canary) == "" {
		return errors.New("seed: --canary is required (the token the drill searches for)")
	}
	if *count < 1 {
		return errors.New("seed: --messages must be at least 1")
	}
	rootFile := strings.TrimSpace(*pgpRootKeyFile)
	if rootFile == "" {
		v, err := env(pgpkeyd.EnvRootKeyFile, "")
		if err != nil {
			return err
		}
		rootFile = v
	}

	d, err := open(ctx)
	if err != nil {
		return err
	}
	defer d.close()

	userID, err := d.store.ProvisionUser(ctx, *subject, *email)
	if err != nil {
		return fmt.Errorf("provision user: %w", err)
	}

	// A REAL sealed credential envelope, so the restored artifact carries real
	// wrapped key material (app.accounts.credentials_ct) for `verify` to open.
	creds, err := onboarding.SealCredentials(d.master, userID, onboarding.Credentials{
		IMAPUsername: *email,
		IMAPPassword: "fixture-not-a-real-password",
	})
	if err != nil {
		return fmt.Errorf("seal credentials: %w", err)
	}

	var acctID, mboxID ulid.ULID
	if err := d.store.UserTx(ctx, userID, func(tx *store.UserTx) error {
		id, err := tx.CreateAccount(ctx, store.AccountRow{
			Name:            *account,
			ProviderProfile: "generic",
			IMAPHost:        "imap.example.com",
			IMAPPort:        993,
			SMTPHost:        "smtp.example.com",
			SMTPPort:        465,
			AuthKind:        "password",
			CredentialsCT:   creds,
			CredentialsDEK:  onboarding.CredentialsSchemeV1,
			EnvelopeVer:     1,
			SyncEnabled:     false, // a fixture must never dial out
		})
		if err != nil {
			return fmt.Errorf("create account: %w", err)
		}
		acctID = id

		mbox, err := tx.UpsertMailbox(ctx, store.MailboxUpsert{
			AccountID:   acctID,
			IMAPName:    "INBOX",
			DisplayName: "Inbox",
		})
		if err != nil {
			return fmt.Errorf("upsert mailbox: %w", err)
		}
		mboxID = mbox.ID
		return nil
	}); err != nil {
		return err
	}

	dek, err := d.mailDEK(userID)
	if err != nil {
		return fmt.Errorf("derive mail dek: %w", err)
	}
	defer crypto.Zero(dek)

	in := ingest.New(d.store, d.blobs)
	base := time.Now().UTC().Add(-time.Duration(*count) * time.Hour)
	for i := 0; i < *count; i++ {
		raw := fixtureMessage(i, *canary, base.Add(time.Duration(i)*time.Hour))
		res, err := in.Ingest(ctx, userID, dek, syncrunner.MailBlobSchemeV1, raw, ingest.Provenance{
			AccountID:    acctID,
			MailboxID:    mboxID,
			UID:          int64(i + 1),
			UIDValidity:  1,
			InternalDate: base.Add(time.Duration(i) * time.Hour),
			RemoteFlags:  []string{"\\Seen"},
		})
		if err != nil {
			return fmt.Errorf("ingest message %d: %w", i, err)
		}
		fmt.Printf("seeded message %s (content-hash %x…, %d bytes, encrypted at rest)\n",
			res.MessageID, res.ContentHash[:6], len(raw))
	}

	// A REAL PGP key, sealed exactly the way the crypto process seals one — so
	// `verify` step 5 has something to unseal — and app.pgp_root_check's row,
	// recorded exactly the way the crypto process's first boot would (#551).
	// Without this the fixture always takes the "0 stored keys, no recorded
	// check value" branch, and neither the unseal code nor the check-value
	// match code that step 5 grew (ADR-024, #525) would ever run in CI at all.
	pgpKeysSeeded := 0
	if rootFile == "" {
		fmt.Printf("no --pgp-root-key-file given; seeded 0 PGP keys and recorded no pgp_root_check " +
			"(verify step 5 will report the escrow leg UNPROVEN)\n")
	} else {
		if err := seedPGPKey(ctx, d, userID, *email, rootFile); err != nil {
			return err
		}
		pgpKeysSeeded = 1
		fmt.Printf("seeded 1 PGP key for %s, and recorded app.pgp_root_check\n", userID)
	}

	fmt.Printf("user_id=%s\n", userID)
	fmt.Printf("account_id=%s\n", acctID)
	fmt.Printf("pgp_keys_seeded=%d\n", pgpKeysSeeded)
	return nil
}

// seedPGPKey mints a real OpenPGP entity and stores it exactly the way
// internal/pgpkeyd.Service.Generate would: same algorithm choice, same
// unsigned private serialization, same wrapping-key derivation and AAD
// (pgpkeyd.WrappingKey / pgpkeyd.SealAAD — the functions that unseal uses, so
// seeding and unsealing cannot silently drift apart).
func seedPGPKey(ctx context.Context, d *deps, userID ulid.ULID, email, rootKeyFile string) error {
	root, err := crypto.ReadKeyFile(rootKeyFile, "PGP root key")
	if err != nil {
		return fmt.Errorf("seed: pgp root key: %w", err)
	}
	defer crypto.Zero(root)

	// #551: seed never boots the crypto process, so without this a fixture
	// seeded with a real PGP key still leaves app.pgp_root_check empty — the
	// row only the crypto process's own boot (internal/pgpkeyd.Service.VerifyRoot)
	// would otherwise write. Record it here, the same way, so verify's step 5
	// can exercise the MATCHED branch of the check-value comparison rather than
	// only ever seeing ABSENT.
	if err := recordPGPRootCheck(ctx, d, root); err != nil {
		return err
	}

	// Minted BEFORE sealing, and on purpose: SealAAD binds the key id into the
	// ciphertext, so a key sealed against one id and stored under another
	// would fail to unseal — which is exactly the failure this fixture exists
	// to let the drill catch, not to accidentally manufacture.
	keyID := ulid.Make()

	created := time.Now().UTC().Truncate(time.Second)
	cfg := &packet.Config{
		Algorithm: packet.PubKeyAlgoEdDSA,
		Curve:     packet.Curve25519,
		V6Keys:    false,
		Time:      func() time.Time { return created },
	}
	entity, err := openpgp.NewEntity("Drill Fixture", "", email, cfg)
	if err != nil {
		return fmt.Errorf("seed: generate pgp key: %w", err)
	}

	var priv bytes.Buffer
	if err := entity.SerializePrivateWithoutSigning(&priv, nil); err != nil {
		return fmt.Errorf("seed: serialize pgp private key: %w", err)
	}
	defer crypto.Zero(priv.Bytes())

	// BINARY, via the crypto process's own serializer: app.user_keys.public_key
	// is what WKD serves verbatim, so a fixture stored as armor would seed a row
	// no correspondent's client can use — the failure this drill exists to
	// detect, manufactured by the drill itself.
	pub, err := pgpkeyd.SerializePublic(entity)
	if err != nil {
		return fmt.Errorf("seed: serialize pgp public key: %w", err)
	}

	wrap, err := pgpkeyd.WrappingKey(root, userID.String())
	if err != nil {
		return fmt.Errorf("seed: derive pgp wrapping key: %w", err)
	}
	defer crypto.Zero(wrap)

	sealed, err := crypto.Seal(wrap, nil, priv.Bytes(), pgpkeyd.SealAAD(userID.String(), keyID.String()))
	if err != nil {
		return fmt.Errorf("seed: seal pgp private key: %w", err)
	}

	k := store.UserKey{
		ID:           keyID,
		UserID:       userID,
		Kind:         store.KeyPGP,
		Fingerprint:  strings.ToUpper(hex.EncodeToString(entity.PrimaryKey.Fingerprint)),
		PublicKey:    pub,
		PrivateKeyCT: sealed,
		EnvelopeVer:  1,
	}
	if err := d.store.UserTx(ctx, userID, func(tx *store.UserTx) error {
		_, err := tx.InsertUserKey(ctx, k)
		return err
	}); err != nil {
		return fmt.Errorf("seed: store pgp key: %w", err)
	}
	return nil
}

// pgpRootCheckInfo is internal/pgpkeyd's rootCheckInfo, copied rather than
// imported: it is unexported there on purpose (the value is a stored, in the
// clear diagnostic label, not a secret), and this drill is the SECOND
// consumer pgpkeyd.RootCheckValue's own doc comment anticipates — a caller
// that holds the root directly and must compute the same bytes rather than
// keep a copy of the derivation that can drift. The derivation itself
// (RootCheckValue) IS shared; only this label, which never varies, is
// duplicated. MUST match byte for byte, or a genuinely correct root reads as
// a different, unverifiable scheme (see verifyRootCheckValue).
const pgpRootCheckInfo = "mailroom-pgp-root-check-v1"

// recordPGPRootCheck writes app.pgp_root_check's one row exactly the way
// internal/pgpkeyd.Service.VerifyRoot does at the crypto process's first boot
// against a database (migration 00055, #525): EnsureRootCheck only inserts
// when the table is empty and never overwrites a recorded value, so calling
// it here from a fixture that has never run the crypto process is the same
// trust-on-first-use step that boot would have taken — not a new behaviour.
//
// seed never boots the crypto process, so without this call every fixture's
// app.pgp_root_check stays empty forever, and `verify` step 5 could never
// observe anything but ABSENT for the check-value comparison — the MATCHED
// branch, and the mismatch it exists to catch, would be untested by the
// drill's own self-test.
func recordPGPRootCheck(ctx context.Context, d *deps, root []byte) error {
	value, err := pgpkeyd.RootCheckValue(root)
	if err != nil {
		return fmt.Errorf("seed: derive pgp root check value: %w", err)
	}
	check, err := pgpkeyd.NewStoreRootCheck(d.store)
	if err != nil {
		return fmt.Errorf("seed: pgp root check store: %w", err)
	}
	if _, _, err := check.EnsureRootCheck(ctx, store.PGPRootCheck{Info: pgpRootCheckInfo, Value: value}); err != nil {
		return fmt.Errorf("seed: record pgp root check: %w", err)
	}
	return nil
}

// fixtureMessage builds a real RFC 5322 message.
//
// The canary goes in the BODY AND NOWHERE ELSE — deliberately. The subject, the
// sender and the snippet all survive in Postgres as plaintext projections, so a
// search that matched one of THOSE would prove nothing about the blob store: it
// would hit even if every encrypted blob had been lost. The body exists only
// inside the AEAD-encrypted blob. A search that finds the canary therefore proves
// the whole chain — blob restored, master key correct, blob decrypted, body
// parsed, index rebuilt from it.
func fixtureMessage(i int, canary string, at time.Time) []byte {
	var b bytes.Buffer
	fmt.Fprintf(&b, "From: Sender %d <sender%d@example.com>\r\n", i, i)
	fmt.Fprintf(&b, "To: Operator <operator@example.com>\r\n")
	fmt.Fprintf(&b, "Subject: Drill fixture %d\r\n", i)
	fmt.Fprintf(&b, "Date: %s\r\n", at.Format(time.RFC1123Z))
	fmt.Fprintf(&b, "Message-ID: <drill-%d-%d@example.com>\r\n", i, at.UnixNano())
	fmt.Fprintf(&b, "MIME-Version: 1.0\r\n")
	fmt.Fprintf(&b, "Content-Type: text/plain; charset=utf-8\r\n")
	fmt.Fprintf(&b, "\r\n")
	fmt.Fprintf(&b, "This message exists only to be restored from a backup.\r\n")
	fmt.Fprintf(&b, "The word the drill searches for is %s, and it appears in this\r\n", canary)
	fmt.Fprintf(&b, "body and nowhere else -- not in the subject, not in any column.\r\n")
	fmt.Fprintf(&b, "A backup that hasn't restored is a hypothesis.\r\n")
	return b.Bytes()
}

// --- verify ------------------------------------------------------------------

func runVerify(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("verify", flag.ContinueOnError)
	userIDStr := fs.String("user-id", "", "user to prove (ULID)")
	query := fs.String("query", "", "search to run; default: a token from the decrypted body")
	showContent := fs.Bool("show-content", false, "print subject/query in clear (FIXTURE data only — never real mail)")
	allowMemory := fs.Bool("allow-memory-index", false, "accept the in-memory backend (NOT a real drill)")
	noPGPRoot := fs.Bool("no-pgp-root", false,
		"this instance has no PGP root to escrow; skip step 5 and accept that the drill then proves NOTHING about encrypted mail")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *userIDStr == "" {
		return errors.New("verify: --user-id is required")
	}
	userID, err := ulid.Parse(*userIDStr)
	if err != nil {
		return fmt.Errorf("verify: --user-id %q is not a ULID: %w", *userIDStr, err)
	}
	inClear := *showContent

	d, err := open(ctx)
	if err != nil {
		return err
	}
	defer d.close()

	fmt.Printf("== user %s\n", userID)

	// (1) DECRYPT ONE MESSAGE with the escrowed master key.
	msg, raw, err := decryptOne(ctx, d, userID)
	if err != nil {
		return err
	}
	fmt.Printf("  [1/5] decrypt   OK  message %s, %d bytes, content-hash verified (%x…)\n",
		msg.ID, len(raw), msg.ContentHash[:6])
	if inClear {
		fmt.Printf("               subject: %q\n", msg.Subject)
	}

	// (2) OPEN THE WRAPPED KEY STORE. Mailroom's wrapped key material lives in
	// Postgres (app.accounts.credentials_ct), so this proves that half of the
	// restore unit survived the dump AND that the escrowed master key opens it.
	if err := openCredentials(ctx, d, userID); err != nil {
		return err
	}

	// (3) REBUILD THE INDEX FROM POSTGRES. The index is never backed up (spec
	// §7/§11): a restored-but-stale index is worse than none, so the drill has to
	// prove it can be regenerated.
	svc, closeIx, err := openSearch(d, *allowMemory)
	if err != nil {
		return err
	}
	defer closeIx()

	prog, err := svc.Reindex(ctx, userID)
	if err != nil {
		return fmt.Errorf("[3/5] REINDEX FAILED: %w\n"+
			"  The index rebuilds from Postgres + the blob store. A failure here means one\n"+
			"  of them did not come back intact", err)
	}
	if prog.Indexed == 0 {
		return errors.New("[3/5] REINDEX FAILED: rebuilt the index and it contains ZERO documents\n" +
			"  The restore is structurally present but has no mail in it")
	}
	// ⚠ THE WHOLE-CORPUS CHECK (#351 B6). Step 1 decrypts ONE message — the oldest
	// — and step 4 searches for a token taken from that same message. The reindex
	// is the only step that touches every blob, and until now it could not report
	// what it found: an unreadable body degraded to a headers-only document and
	// was still counted in Indexed. So a restore that lost every blob but the
	// oldest passed with a document count and "THE RESTORE IS ALIVE".
	//
	// A bound is not optional here. "N documents" without this is a statement
	// about Postgres, not about the mail.
	if prog.BodiesMissing > 0 {
		return fmt.Errorf("[3/5] REINDEX FAILED: %d of %d document(s) had NO READABLE BODY and were "+
			"indexed from headers alone\n"+
			"  Postgres came back and the blob store did not. Those messages exist as rows\n"+
			"  and are searchable by subject, but their content is gone — which is the\n"+
			"  failure a restore is supposed to catch, not survive",
			prog.BodiesMissing, prog.Indexed)
	}
	fmt.Printf("  [3/5] reindex   OK  %d document(s) rebuilt from Postgres (cursor %d), "+
		"every body read from the blob store\n",
		prog.Indexed, prog.Cursor)
	// (4) SEARCH, AND REQUIRE A HIT.
	q := strings.TrimSpace(*query)
	derived := false
	if q == "" {
		q = searchToken(raw)
		derived = true
		if q == "" {
			return errors.New("[4/5] SEARCH FAILED: could not derive a query token from the decrypted message; pass --query")
		}
	}
	shown := q
	if derived && !inClear {
		shown = "<derived from subject, redacted>"
	}

	res, err := svc.Search(ctx, userID, q, search.SearchOptions{Limit: 10})
	if err != nil {
		return fmt.Errorf("[4/5] SEARCH FAILED: %w", err)
	}
	if len(res.Hits) == 0 {
		return fmt.Errorf("[4/5] SEARCH FAILED: query %s returned NO HITS\n"+
			"  The index rebuilt but does not answer. A restore that cannot be searched is\n"+
			"  not a restore anyone can use", shown)
	}
	fmt.Printf("  [4/5] search    OK  query %s -> %d hit(s), top %s\n", shown, len(res.Hits), res.Hits[0].ID)

	// (5) THE SECOND ROOT. Steps 1–4 all turn on the master key. Since ADR-024
	// there is a second escrowed secret whose loss is invisible to every one of
	// them: the mailbox restores, opens, searches and reads perfectly, and only
	// the PGP-encrypted mail is gone. A drill that stopped at step 4 would
	// print "ALIVE" over exactly that hole.
	pgp, err := verifyPGPRoot(ctx, d, userID, *noPGPRoot)
	if err != nil {
		return err
	}

	switch {
	case !pgp.present:
		fmt.Printf("== user %s is ALIVE for everything the MASTER KEY covers\n", userID)
		fmt.Printf("== ⚠ THE PGP ROOT WAS NOT CHECKED. This run did not prove that mail\n")
		fmt.Printf("==   encrypted to your PGP keys is recoverable. That is a different\n")
		fmt.Printf("==   secret, and losing it fails silently — see docs/key-escrow.md.\n")
	case pgp.stored == 0 && pgp.rootCheckMatched:
		// #551: this database recorded a check value (app.pgp_root_check,
		// migration 00055) and the escrowed root matches it — PROVEN to be the
		// root this database was built with, even with nothing to unseal. That
		// is a narrower claim than "opens a key", so it is worded as one.
		fmt.Printf("== user %s is ALIVE, and the second escrowed root is PROVEN: it matches this\n", userID)
		fmt.Printf("==   database's own recorded root check (app.pgp_root_check)\n")
		fmt.Printf("== ⚠ this user has 0 stored PGP keys, so step 5 unsealed nothing. Matching the\n")
		fmt.Printf("==   recorded root is real evidence, but it is not the same claim as \"this root\n")
		fmt.Printf("==   opens a key\" — re-run against a user with PGP keys for that, stronger, check.\n")
	case pgp.stored == 0:
		fmt.Printf("== user %s is ALIVE, and the second escrowed root is present — but UNPROVEN\n", userID)
		fmt.Printf("== ⚠ this user has 0 stored PGP keys, so step 5 had nothing to unseal, and this\n")
		fmt.Printf("==   database has no recorded root check either (app.pgp_root_check is empty: it\n")
		fmt.Printf("==   predates migration 00055, or its crypto process has never booted). That is\n")
		fmt.Printf("==   NOT a pass: re-run against a user with PGP keys before trusting this leg.\n")
	default:
		fmt.Printf("== user %s is ALIVE, and both escrowed roots are present — %d PGP key(s)\n", userID, pgp.unsealed)
		fmt.Printf("==   unsealed and fingerprint-verified\n")
		if pgp.rootCheckMatched {
			fmt.Printf("==   (the root also matches this database's own recorded check, app.pgp_root_check)\n")
		}
	}
	return nil
}

// pgpRootResult records what step 5 actually established, so the verdict can
// state its own bound rather than implying more than it proved.
type pgpRootResult struct {
	// present is true once the escrowed secret itself passed the
	// present/well-formed/distinct-from-master checks. It says nothing about
	// whether there was any stored key to open with it — see stored/unsealed.
	present bool
	// rootCheckMatched is true iff this database recorded a check value
	// (app.pgp_root_check, migration 00055, #525) AND it equals
	// pgpkeyd.RootCheckValue(root). It is the narrower of step 5's two proofs:
	// "this is the root the database was built with", not "this root opens a
	// key" (rootCheckMatched can be true with stored == 0). A mismatch is
	// never recorded here — verifyPGPRoot returns a hard error instead, before
	// stored/unsealed are ever populated.
	rootCheckMatched bool
	// stored counts this user's app.user_keys rows with a non-empty sealed
	// private key: the population step 5 attempted to open.
	stored int
	// unsealed counts how many of those actually opened AND had their
	// fingerprint verified against the stored column. A hard failure is
	// returned (not recorded here) the moment any one of them does not, so a
	// non-error pgpRootResult always has unsealed == stored.
	unsealed int
}

// verifyPGPRoot proves the SECOND escrowed root (ADR-024) is present,
// well-formed, distinct from the master key, matches this database's own
// recorded check value if it has one (app.pgp_root_check, migration 00055,
// #525), and — for every PGP private key this user has stored — actually
// opens it.
//
// The unseal step remains the point of this function even after #551: presence
// and well-formedness alone would have passed even if the escrowed root and
// the stored keys had drifted apart, which is the exact failure mode ADR-024
// exists to catch, and a matching check value does not by itself prove any
// PARTICULAR key opens — see verifyRootCheckValue's doc comment. A user with
// zero stored keys is NOT skipped over silently — see runVerify's verdict,
// which reports that case as unproven (absent) or proven-but-narrower
// (matched) rather than folding it into a full pass or the SKIPPED-by-flag
// path.
func verifyPGPRoot(ctx context.Context, d *deps, userID ulid.ULID, acknowledgedAbsent bool) (pgpRootResult, error) {
	if acknowledgedAbsent {
		fmt.Printf("  [5/5] pgp root  --  SKIPPED at your request (--no-pgp-root)\n")
		return pgpRootResult{}, nil
	}

	root, err := crypto.ReadKeyFile(d.cfg.pgpRootKeyFile, "PGP root key")
	if err != nil {
		return pgpRootResult{}, fmt.Errorf("[5/5] PGP ROOT FAILED: %w\n"+
			"  This is the crypto process's own escrowed secret (ADR-024), not the master key.\n"+
			"  Without it, every message ever encrypted to your PGP keys is unrecoverable — and\n"+
			"  nothing else in this drill would have told you, because steps 1-4 all pass without it.\n"+
			"  Retrieve it from escrow (docs/key-escrow.md) and point %s at it.\n"+
			"  If this instance genuinely has no PGP root, re-run with --no-pgp-root and accept that\n"+
			"  this drill then proves nothing about encrypted mail",
			err, pgpkeyd.EnvRootKeyFile)
	}
	defer crypto.Zero(root)

	// The collapse this catches: pointing both secrets at the same file. It
	// leaves a stack that boots, serves and drills perfectly while having
	// exactly one root — which deletes the property the separate process
	// exists for, with nothing anywhere reporting a problem.
	if bytes.Equal(root, d.master) {
		return pgpRootResult{}, errors.New("[5/5] PGP ROOT FAILED: the escrowed PGP root is BYTE-IDENTICAL to the master key\n" +
			"  These must be two independent secrets (ADR-024). If they are the same, an attacker\n" +
			"  holding the master key can derive every PGP private key, and the crypto process is\n" +
			"  isolation with nothing behind it. Check that the two _FILE variables point at\n" +
			"  different files, then re-escrow whichever one is wrong")
	}

	// #551: THE CHECK VALUE, BEFORE UNSEALING. app.pgp_root_check is a
	// database-wide fact (one singleton row, not per-user), independent of
	// whether THIS user has any PGP keys at all — so it can PROVE the root is
	// the one this database was built with even against a key-less backup,
	// where unsealPGPKeys below has nothing to attempt. A mismatch here is
	// exactly the disaster ADR-024 exists to catch, so it fails HARD and
	// before unsealing runs: unsealing a key with a root already known to be
	// wrong would only repeat the same verdict less precisely.
	check, err := pgpkeyd.NewStoreRootCheck(d.store)
	if err != nil {
		return pgpRootResult{}, fmt.Errorf("[5/5] PGP ROOT FAILED: %w", err)
	}
	checkStatus, err := verifyRootCheckValue(ctx, check, root)
	if err != nil {
		return pgpRootResult{}, fmt.Errorf("[5/5] PGP ROOT FAILED: %w", err)
	}
	if checkStatus == rootCheckMismatched {
		return pgpRootResult{}, errors.New("[5/5] PGP ROOT FAILED: this root does NOT match app.pgp_root_check, the\n" +
			"  value this database recorded the first time ITS OWN crypto process booted (migration\n" +
			"  00055, #525). This is the disaster case ADR-024 exists to catch: the escrowed PGP root\n" +
			"  you were given and the one this database was actually built with have drifted apart.\n" +
			"  Every PGP private key stored here is sealed under the real root, not this one — retrieve\n" +
			"  the correct escrowed root (docs/key-escrow.md) before trusting anything else about it")
	}

	stored, unsealed, err := unsealPGPKeys(ctx, d, userID, root)
	if err != nil {
		return pgpRootResult{}, err
	}

	result, lines := pgpRootVerdict(checkStatus, stored, unsealed)
	for _, l := range lines {
		fmt.Println(l)
	}
	return result, nil
}

// rootCheckStatus is the three-state outcome of comparing the escrowed PGP
// root against app.pgp_root_check (migration 00055, #525): a table that only
// carries a row once SOME crypto process has booted against this database (or
// `seed` has stood in for one, #551).
type rootCheckStatus int

const (
	// rootCheckAbsent: no row exists yet. This backup predates migration
	// 00055, or its crypto process has never booted (nor has `seed` been given
	// --pgp-root-key-file). NOT evidence of anything, and must never be folded
	// into a pass on its own — see pgpRootVerdict and runVerify's switch.
	rootCheckAbsent rootCheckStatus = iota
	// rootCheckMatched: a row exists and equals pgpkeyd.RootCheckValue(root)
	// exactly. This root is PROVEN to be the one this database was built
	// with — narrower than "opens a key", per pgpkeyd's own doc comment on
	// VerifyRoot.
	rootCheckMatched
	// rootCheckMismatched: a row exists and disagrees, or was recorded under a
	// scheme this binary does not compute (which is treated the same way: a
	// root that cannot be verified has not been). FAILED — verifyPGPRoot
	// returns a hard error the moment this is observed and never reaches
	// pgpRootVerdict with it.
	rootCheckMismatched
)

// verifyRootCheckValue compares root's check value against app.pgp_root_check
// through check — the SAME RootCheckStore interface and EnsureRootCheck
// semantics internal/pgpkeyd.Service.VerifyRoot uses at the crypto process's
// own boot — without ever overwriting a value that is already recorded:
// EnsureRootCheck only inserts when the table is empty.
//
// ⚠ IF THE ROW WAS ABSENT, THIS CALL RECORDS IT. That is not a new behaviour
// invented for the drill: it is the exact trust-on-first-use step 00055's own
// comment describes for a database's first boot, reused via the interface
// pgpkeyd already exports for a second consumer. inserted == true is how
// ABSENT is recognised — this call's own insert is what makes it observable,
// the same way a fresh boot's insert is what its log line reports as
// "recorded" rather than "verified" (pgpkeyd.RootRecorded).
func verifyRootCheckValue(ctx context.Context, check pgpkeyd.RootCheckStore, root []byte) (rootCheckStatus, error) {
	value, err := pgpkeyd.RootCheckValue(root)
	if err != nil {
		return rootCheckAbsent, fmt.Errorf("derive root check value: %w", err)
	}
	recorded, inserted, err := check.EnsureRootCheck(ctx, store.PGPRootCheck{Info: pgpRootCheckInfo, Value: value})
	if err != nil {
		return rootCheckAbsent, fmt.Errorf("read app.pgp_root_check: %w", err)
	}
	if inserted {
		return rootCheckAbsent, nil
	}
	// A different scheme is not a different root, but this binary cannot
	// verify it either way, and a root that cannot be verified has not been —
	// see pgpkeyd.VerifyRoot's identical refusal.
	if recorded.Info != pgpRootCheckInfo {
		return rootCheckMismatched, nil
	}
	if subtle.ConstantTimeCompare(recorded.Value, value) != 1 {
		return rootCheckMismatched, nil
	}
	return rootCheckMatched, nil
}

// pgpRootVerdict combines the check-value comparison and the unsealed-key
// count into the pgpRootResult runVerify's banner switches on, and the [5/5]
// diagnostic lines step 5 prints for each reachable combination.
//
// mismatched is NOT a case here: verifyPGPRoot returns a hard error the
// moment verifyRootCheckValue reports it, before unsealPGPKeys or this
// function ever run. This function only ever sees "matched" or "absent" — the
// two states step 5 can still report evidence for.
func pgpRootVerdict(status rootCheckStatus, stored, unsealed int) (pgpRootResult, []string) {
	matched := status == rootCheckMatched
	if stored == 0 {
		if matched {
			return pgpRootResult{present: true, rootCheckMatched: true}, []string{
				"  [5/5] pgp root  OK  matches this database's own recorded root check (app.pgp_root_check)",
				"               note: this user has 0 stored PGP keys, so there was nothing to unseal.",
				"               The root is PROVEN to be the one this database was built with — a",
				"               narrower claim than \"opens a key\", but a real proof, not UNPROVEN.",
			}
		}
		return pgpRootResult{present: true}, []string{
			"  [5/5] pgp root  OK  present, well-formed, and distinct from the master key",
			"               note: this user has 0 stored PGP keys, so there was nothing to",
			"               unseal — the SECRET checked out, but that is not evidence any",
			"               key actually opens with it. This database also has no recorded",
			"               root check (app.pgp_root_check is empty: it predates migration 00055,",
			"               or its crypto process has never booted) — re-run against a user with",
			"               PGP keys, or after a crypto process has booted once, to prove this leg.",
		}
	}
	lines := []string{
		fmt.Sprintf("  [5/5] pgp root  OK  present, well-formed, distinct from the master key; %d/%d", unsealed, stored),
		"               stored PGP key(s) unsealed and fingerprint-verified",
	}
	if matched {
		lines = append(lines, "               (the root also matches this database's own recorded check, app.pgp_root_check)")
	}
	return pgpRootResult{present: true, rootCheckMatched: matched, stored: stored, unsealed: unsealed}, lines
}

// unsealPGPKeys opens every one of the user's stored PGP private keys with
// the escrowed root, exactly the way internal/pgpkeyd sealed them
// (pgpkeyd.WrappingKey / pgpkeyd.SealAAD — the same functions, not a second
// copy of the derivation), and checks the opened key's fingerprint against the
// stored column.
//
// Any single failure here returns an error rather than a partial count: a
// root that opens 4 of 5 keys is not "80% recovered", it is the disaster case
// ADR-024 warns about — the escrowed secret and the stored keys have drifted
// apart, and every key past the first failure is equally suspect.
func unsealPGPKeys(ctx context.Context, d *deps, userID ulid.ULID, root []byte) (stored, unsealed int, err error) {
	var keys []store.UserKey
	if err := d.store.UserTx(ctx, userID, func(tx *store.UserTx) error {
		var e error
		keys, e = tx.ListUserKeys(ctx)
		return e
	}); err != nil {
		return 0, 0, fmt.Errorf("[5/5] PGP ROOT FAILED: list stored keys: %w", err)
	}

	for _, k := range keys {
		if len(k.PrivateKeyCT) == 0 {
			continue
		}
		stored++

		wrap, err := pgpkeyd.WrappingKey(root, userID.String())
		if err != nil {
			return stored, unsealed, fmt.Errorf("[5/5] PGP ROOT FAILED: derive wrapping key for key %s: %w", k.ID, err)
		}
		plain, err := crypto.Open(wrap, k.PrivateKeyCT, pgpkeyd.SealAAD(userID.String(), k.ID.String()))
		crypto.Zero(wrap)
		if err != nil {
			return stored, unsealed, fmt.Errorf("[5/5] PGP ROOT FAILED: key %s (fingerprint %s) did NOT unseal: %w\n"+
				"  The escrowed PGP root does not open a key this user actually has stored. This\n"+
				"  is the disaster case ADR-024 exists to catch: the escrowed secret and the stored\n"+
				"  keys have drifted apart, so mail encrypted to this user's PGP keys is\n"+
				"  UNRECOVERABLE. Steps 1-4 passing tells you nothing about this — they do not\n"+
				"  touch this secret at all",
				k.ID, k.Fingerprint, err)
		}

		entity, perr := openpgp.ReadEntity(packet.NewReader(bytes.NewReader(plain)))
		crypto.Zero(plain)
		if perr != nil {
			return stored, unsealed, fmt.Errorf("[5/5] PGP ROOT FAILED: key %s unsealed to bytes that are not a readable OpenPGP key: %w\n"+
				"  The AEAD tag verified but the plaintext is not a private key. That should be\n"+
				"  impossible unless the stored ciphertext itself is corrupt", k.ID, perr)
		}
		got := strings.ToUpper(hex.EncodeToString(entity.PrimaryKey.Fingerprint))
		if got != k.Fingerprint {
			return stored, unsealed, fmt.Errorf("[5/5] PGP ROOT FAILED: key %s unsealed to the WRONG KEY: stored fingerprint\n"+
				"  %s, opened key fingerprint %s.\n"+
				"  Opening to plausible-looking bytes is not proof of anything — this run only\n"+
				"  trusts a key whose fingerprint matches what was stored", k.ID, k.Fingerprint, got)
		}
		unsealed++
	}
	return stored, unsealed, nil
}

// decryptOne picks the user's oldest message and decrypts its raw bytes out of
// the restored blob store, re-verifying the content hash end to end.
func decryptOne(ctx context.Context, d *deps, userID ulid.ULID) (store.IndexMessage, []byte, error) {
	var msgs []store.IndexMessage
	if err := d.store.UserTx(ctx, userID, func(tx *store.UserTx) error {
		ids, err := tx.MessageIDsAfter(ctx, ulid.ULID{}, 1)
		if err != nil {
			return err
		}
		if len(ids) == 0 {
			return errors.New("this user has NO MESSAGES; an empty mailbox is a failed restore")
		}
		msgs, err = tx.IndexMessages(ctx, ids)
		return err
	}); err != nil {
		return store.IndexMessage{}, nil, fmt.Errorf("[1/5] DECRYPT FAILED: %w", err)
	}
	if len(msgs) == 0 {
		return store.IndexMessage{}, nil, errors.New("[1/5] DECRYPT FAILED: message row vanished between queries")
	}
	msg := msgs[0]

	if len(msg.ContentHash) != sha256.Size {
		return store.IndexMessage{}, nil, fmt.Errorf("[1/5] DECRYPT FAILED: bad content hash length %d", len(msg.ContentHash))
	}
	var h blob.ContentHash
	copy(h[:], msg.ContentHash)

	dek, err := d.mailDEK(userID)
	if err != nil {
		return store.IndexMessage{}, nil, fmt.Errorf("[1/5] DECRYPT FAILED: derive mail dek: %w", err)
	}
	defer crypto.Zero(dek)

	rc, err := d.blobs.Get(ctx, userID, dek, h)
	if err != nil {
		return store.IndexMessage{}, nil, decryptError(err)
	}
	defer func() { _ = rc.Close() }()

	raw, err := io.ReadAll(rc)
	if err != nil {
		// blob.FSBlobStore verifies the AEAD tag AND the content hash as the
		// reader drains, so a wrong key or a mangled blob surfaces HERE.
		return store.IndexMessage{}, nil, decryptError(err)
	}
	if len(raw) == 0 {
		return store.IndexMessage{}, nil, errors.New("[1/5] DECRYPT FAILED: blob decrypted to zero bytes")
	}
	if sum := sha256.Sum256(raw); !bytes.Equal(sum[:], msg.ContentHash) {
		return store.IndexMessage{}, nil, errors.New("[1/5] DECRYPT FAILED: content hash does not match the decrypted bytes")
	}
	if !bytes.Contains(raw, []byte("\r\n\r\n")) && !bytes.Contains(raw, []byte("\n\n")) {
		return store.IndexMessage{}, nil, errors.New("[1/5] DECRYPT FAILED: decrypted bytes are not an RFC 5322 message")
	}
	return msg, raw, nil
}

// decryptError names the overwhelmingly likely cause. An AEAD failure here is
// almost never a corrupt disk: it is the WRONG MASTER KEY.
func decryptError(err error) error {
	return fmt.Errorf("[1/5] DECRYPT FAILED: %w\n"+
		"  The blob store came back but its contents will not open. The usual cause is a\n"+
		"  WRONG escrowed master key (ADR-005) — the mail-blob key is derived from it, and\n"+
		"  so are the blob paths. Check you retrieved the escrow for THIS deployment", err)
}

// openCredentials proves the wrapped key store restored: it opens one account's
// sealed credential envelope with the escrowed master key. Nothing it recovers is
// ever printed — the point is that it OPENS, not what is inside.
func openCredentials(ctx context.Context, d *deps, userID ulid.ULID) error {
	var accts []store.Account
	if err := d.store.UserTx(ctx, userID, func(tx *store.UserTx) error {
		var e error
		accts, e = tx.ListAccounts(ctx)
		return e
	}); err != nil {
		return fmt.Errorf("[2/5] KEY STORE FAILED: list accounts: %w", err)
	}
	if len(accts) == 0 {
		fmt.Printf("  [2/5] key store --  no accounts for this user; nothing wrapped to open\n")
		return nil
	}
	a := accts[0]
	if len(a.CredentialsCT) == 0 {
		return fmt.Errorf("[2/5] KEY STORE FAILED: account %s restored with an EMPTY credential envelope\n"+
			"  The wrapped key store did not survive the backup", a.ID)
	}
	if _, err := onboarding.OpenCredentials(d.master, userID, a.CredentialsCT); err != nil {
		return fmt.Errorf("[2/5] KEY STORE FAILED: account %s: %w\n"+
			"  The wrapped credentials restored but will not open — wrong escrowed master key", a.ID, err)
	}
	fmt.Printf("  [2/5] key store OK  account %s: sealed credentials opened with the escrowed key\n", a.ID)
	return nil
}

// openSearch builds the indexer over a SCRATCH index root. It REFUSES the
// in-memory backend by default: a drill that quietly proved the fallback engine
// would be proving something the production image does not run (ADR-006).
func openSearch(d *deps, allowMemory bool) (*search.Service, func(), error) {
	factory := tantivy.Open
	switch {
	case tantivy.Available():
		fmt.Printf("  ....  search backend: tantivy (CGO, the engine the image ships)\n")
	case allowMemory:
		factory = search.MemoryFactory
		fmt.Printf("  ....  search backend: IN-MEMORY (--allow-memory-index) — NOT a real drill\n")
	default:
		return nil, nil, errors.New("this binary was built WITHOUT the Tantivy engine, so it cannot prove " +
			"the index the production image actually uses.\n" +
			"  Build it with CGO_ENABLED=1 -tags tantivy (ADR-006), or pass --allow-memory-index to " +
			"accept a weaker proof")
	}

	content := search.NewBlobContent(d.blobs, func(_ context.Context, userID ulid.ULID) ([]byte, error) {
		return d.mailDEK(userID)
	})
	ix, err := search.NewIndexer(d.cfg.searchRoot, factory, search.NewStoreSource(d.store), content,
		search.WithLogger(d.log))
	if err != nil {
		return nil, nil, fmt.Errorf("[3/5] REINDEX FAILED: open index root %s: %w", d.cfg.searchRoot, err)
	}
	closeIx := func() {
		if err := ix.Close(); err != nil {
			fmt.Fprintln(os.Stderr, "mailroom-drill: closing the index:", err)
		}
	}
	return search.NewService(ix, search.NewStoreResolver(d.store)), closeIx, nil
}

// searchToken picks a query out of the message's decrypted BODY: the longest run
// of letters of 5+ characters, after the header/body separator.
//
// The body, not the headers, and not the subject — for the same reason the
// fixture plants its canary there. Subject/from/snippet are plaintext columns in
// Postgres, so a search matching one of them would still succeed on a restore
// whose entire blob store was missing. A body term can only be in the index if a
// blob was really fetched, really decrypted with the escrowed master key, and
// really parsed. That is what makes this a proof rather than a formality.
//
// Deriving it from the restored data (rather than hardcoding a fixture word) is
// what lets `verify` run against a REAL production artifact.
func searchToken(raw []byte) string {
	body := raw
	if i := bytes.Index(raw, []byte("\r\n\r\n")); i >= 0 {
		body = raw[i+4:]
	} else if i := bytes.Index(raw, []byte("\n\n")); i >= 0 {
		body = raw[i+2:]
	}
	best := ""
	for _, f := range strings.FieldsFunc(string(body), func(r rune) bool {
		return !unicode.IsLetter(r)
	}) {
		if len(f) > len(best) {
			best = f
		}
	}
	if len(best) < 5 {
		return ""
	}
	return best
}
