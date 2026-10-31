// SPDX-License-Identifier: AGPL-3.0-only

// Command mailroom-export writes one user's mail out as a maildir (#54, spec §10).
//
//	mailroom-export --user-id U   /path/to/archive
//	mailroom-export --subject alice /path/to/archive
//
// It is the mirror of cmd/mailroom-import and is deliberately its twin:
// streaming (one message of memory, not one archive), and refusing to write
// bytes it has not verified.
//
// Why a separate binary rather than a `mailroom-admin` subcommand: the same
// reason mailroom-import is one. This is an OPERATOR tool that moves mail in
// bulk to the operator's own disk, and it is kept out of the shipped image —
// the archive belongs on their machine, not inside the container, and a tool
// that can read out a million messages has no business sitting on the
// production box. It is still part of the module, so `go build ./...`, vet and
// the linters hold it to the same bar.
//
// ⚠ WHAT THIS WRITES. The MAIL as byte-exact message files, with
// seen/answered/flagged/draft/trashed in the maildir filename where
// internal/importer already reads them back; messages.jsonl for the per-message
// state a filename cannot hold (labels WITH their provenance, custom keywords,
// junk, the triage score); and the configuration sidecars the export plan names
// — labels, contacts, identities, rules, signatures and templates, saved
// searches, category tabs, remote-image allowances, send history, and the
// account record.
//
// ⚠ AND WHAT READS IT BACK — WHICH IS LESS THAN WHAT IS WRITTEN. This is an
// ARCHIVE FORMAT, not a migration path. The MAIL returns: cmd/mailroom-import
// reads maildirs, so the bytes and the filename flags round-trip, and an
// integration test holds that byte-exact. THE SIDECARS DO NOT. Nothing reads
// messages.jsonl or the configuration files back — not this project, and there
// is no format anyone else reads either. They are a RECORD, for a person or a
// script someone writes on the day they need one.
//
// So if the goal is "stand this instance up somewhere else", this is the wrong
// tool: that is scripts/backup.sh plus pg_restore, which is whole-database and
// is drilled as a required check. If the goal is "my mail is mine and I can
// leave", this is exactly the tool. The manifest says so in the archive itself.
//
// It deliberately does not carry the private key that makes PGP-encrypted mail
// readable. That key leaves by a separate, explicit act — `mailroom-admin
// pgp-export-wrapped`, spec §9, built for #54 — under a passphrase the user
// chooses, because bundling it here would put the ciphertext and the thing that
// opens it in one directory and protect the pair with nothing.
//
// The archive says so in its own manifest.json rather than only in a changelog,
// and names the command: the person opening that directory in three years is
// the one who needs to know, and telling them what is missing without telling
// them how to get it is only half an answer.
//
// Configuration (shared with the server and the importer, internal/config):
//
//	MAILROOM_DATABASE_URL              mailroom_app DSN (required; honours _FILE)
//	MAILROOM_DATABASE_PASSWORD_FILE    the DSN's password, injected in memory (#537)
//	MAILROOM_MASTER_KEY_FILE           the master key the mail-blob DEK is derived from
//	MAILROOM_BLOB_ROOT                 the blob store (honours _FILE)
//
// Every setting honours the Docker-secrets _FILE indirection the deployment
// actually uses. The DSN itself carries no password (#537): the deployment
// sets MAILROOM_DATABASE_URL to a plain postgres:// URL and
// MAILROOM_DATABASE_PASSWORD_FILE to the mailroom_app password secret —
// the SAME file the role-bootstrap step used to set that role's password —
// and this binary assembles the two in memory (internal/pgdsn) right
// before connecting, exactly like the server and every other DB-touching
// command.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"sort"
	"strings"
	"syscall"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/blob"
	"mailroom/internal/config"
	"mailroom/internal/crypto"
	"mailroom/internal/exporter"
	"mailroom/internal/pgdsn"
	"mailroom/internal/store"
	"mailroom/internal/syncrunner"
)

// blobPathInfo is the HKDF label for the blob store's per-user path-derivation
// key. It MUST match cmd/mailroom, cmd/mailroom-admin, cmd/mailroom-drill and
// cmd/mailroom-import: a tool that derived different paths would look for blobs
// where nothing was ever written and report an empty mailbox.
const blobPathInfo = "mailroom-blob-path-v1"

func main() {
	if err := run(os.Args[1:]); err != nil {
		if errors.Is(err, context.Canceled) {
			fmt.Fprintln(os.Stderr, "mailroom-export: interrupted — the partial archive is incomplete, delete it")
			os.Exit(130)
		}
		fmt.Fprintln(os.Stderr, "mailroom-export: "+err.Error())
		os.Exit(1)
	}
}

func run(args []string) error {
	fs := flag.NewFlagSet("mailroom-export", flag.ContinueOnError)
	userID := fs.String("user-id", "", "the user to export (ULID)")
	subject := fs.String("subject", "", "the user's OIDC subject, instead of --user-id")
	fs.Usage = func() {
		_, _ = fmt.Fprintln(fs.Output(), "usage: mailroom-export (--user-id U | --subject S) <output-directory>")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		fs.Usage()
		return errors.New("exactly one output directory is required")
	}
	outDir := fs.Arg(0)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	d, err := open(ctx)
	if err != nil {
		return err
	}
	defer d.close()

	uid, err := resolveUser(ctx, d, *userID, *subject)
	if err != nil {
		return err
	}

	// ⚠ Derived exactly as the sync path writes it (syncrunner.MailBlobInfoV1).
	// This is the THIRD caller of this derivation — the server and the restore
	// drill are the others — and the drill's comment says why the constant is
	// reused rather than restated: a drift between the key mail was WRITTEN with
	// and the key it is READ with makes healthy data look corrupt.
	dek, err := crypto.DeriveKey(d.master, uid[:], syncrunner.MailBlobInfoV1)
	if err != nil {
		return fmt.Errorf("derive mail dek: %w", err)
	}
	defer crypto.Zero(dek)

	res, err := exporter.Export(ctx, d.store, d.blobs, dek, exporter.Options{
		UserID: uid,
		OutDir: outDir,
	})
	// The result is reported even on failure: a partial export's counts are the
	// only thing that says how far it got.
	report(outDir, res)
	if err != nil {
		return err
	}
	if len(res.Unreadable) > 0 {
		// ⚠ NON-ZERO EXIT. An archive missing messages must not be mistaken for a
		// complete one by a script that only checks the status code.
		return fmt.Errorf("%d message(s) could not be exported — see the list above "+
			"and manifest.json; this archive is INCOMPLETE", len(res.Unreadable))
	}
	return nil
}

func report(outDir string, res exporter.Result) {
	fmt.Printf("archive                      : %s\n", outDir)
	fmt.Printf("messages written             : %d\n", res.Messages)
	fmt.Printf("bytes written                : %d\n", res.Bytes)
	fmt.Printf("labels.json                  : %d\n", res.Labels)
	// ⚠ PER FILE, INCLUDING THE ZEROS. One total would hide the difference
	// between "this user has no saved searches" and "the saved-search query
	// returned nothing because it is broken" — and an export is precisely where
	// that distinction has to survive.
	names := make([]string, 0, len(res.Config))
	for k := range res.Config {
		names = append(names, k)
	}
	sort.Strings(names)
	for _, n := range names {
		fmt.Printf("%-29s: %d\n", n, res.Config[n])
	}
	if len(res.Unreadable) > 0 {
		fmt.Printf("⚠ messages NOT written        : %d\n", len(res.Unreadable))
		for _, u := range res.Unreadable {
			fmt.Printf("    %s  %s\n", u.MessageID, u.Reason)
		}
	}
	fmt.Println()
	fmt.Println("what this archive does NOT contain:")
	for _, l := range exporter.Limits {
		fmt.Printf("  - %s\n", l)
	}
}

type deps struct {
	store  *store.Store
	blobs  blob.BlobStore
	master []byte
	close  func()
}

// env resolves a setting, honouring the Docker-secrets `_FILE` indirection:
// if NAME_FILE is set, the file's trimmed contents win over NAME itself.
//
// ⚠ THE FIRST VERSION OF THIS FILE USED os.Getenv AND THE LIVE RUN CAUGHT IT.
// A binary that reads only the plain variable breaks the moment a setting
// arrives as a mounted secret instead — which several of them, including the
// database password (see open, below), always do. Identical to
// cmd/mailroom-import's helper, deliberately.
func env(name, def string) (string, error) {
	if path := strings.TrimSpace(os.Getenv(name + "_FILE")); path != "" {
		b, err := os.ReadFile(path) //nolint:gosec // the path is operator-supplied config
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

func open(ctx context.Context) (*deps, error) {
	databaseURL, err := env(config.EnvDatabaseURL, "")
	if err != nil {
		return nil, err
	}
	if databaseURL == "" {
		return nil, fmt.Errorf("%s (or its _FILE) is required", config.EnvDatabaseURL)
	}
	// The URL itself carries no password (#537): the deployment sets
	// MAILROOM_DATABASE_PASSWORD_FILE to the mailroom_app password secret,
	// and it is injected here, in memory, right before connecting.
	databaseURL, err = pgdsn.WithPasswordFile(databaseURL, strings.TrimSpace(os.Getenv(config.EnvDatabasePasswordFile)))
	if err != nil {
		return nil, err
	}
	masterKeyFile, err := env(config.EnvMasterKeyFile, config.DefaultMasterKeyFile)
	if err != nil {
		return nil, err
	}
	blobRoot, err := env(config.EnvBlobRoot, config.DefaultBlobRoot)
	if err != nil {
		return nil, err
	}

	master, err := crypto.NewFileKeyProvider(masterKeyFile).MasterKey(ctx)
	if err != nil {
		return nil, fmt.Errorf("master key from %s: %w", masterKeyFile, err)
	}
	st, err := store.Open(ctx, databaseURL)
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

	blobs, err := blob.NewFSBlobStore(blobRoot, pathKey)
	if err != nil {
		st.Close()
		crypto.Zero(master)
		return nil, fmt.Errorf("open blob store at %s: %w", blobRoot, err)
	}
	d := &deps{store: st, blobs: blobs, master: master}
	d.close = func() {
		st.Close()
		crypto.Zero(d.master)
	}
	return d, nil
}

func resolveUser(ctx context.Context, d *deps, userIDStr, subject string) (ulid.ULID, error) {
	switch {
	case userIDStr != "":
		id, err := ulid.Parse(userIDStr)
		if err != nil {
			return ulid.ULID{}, fmt.Errorf("--user-id %q is not a ULID: %w", userIDStr, err)
		}
		return id, nil
	case subject != "":
		id, ok, err := d.store.UserIDForSubject(ctx, subject)
		if err != nil {
			return ulid.ULID{}, err
		}
		if !ok {
			return ulid.ULID{}, fmt.Errorf("no user with OIDC subject %q", subject)
		}
		return id, nil
	default:
		return ulid.ULID{}, errors.New("one of --user-id or --subject is required")
	}
}
