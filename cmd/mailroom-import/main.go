// SPDX-License-Identifier: AGPL-3.0-only

// Command mailroom-import bulk-loads an existing mail archive — mbox, maildir, or
// a Google Takeout — into Mailroom (spec §4, mailroom#27).
//
//	mailroom-import --user-id U /path/to/Takeout/Mail
//	mailroom-import --subject alice --dry-run ~/mail/archive.mbox
//
// It streams: a 25 GB Takeout costs the memory of ONE message, not of the archive.
// It is resumable: interrupt it and re-run it, and it picks up from its cursor
// instead of re-reading twenty gigabytes. It is idempotent even without the cursor,
// because a message's identity is the SHA-256 of its bytes (ADR-001), so importing
// the same archive twice deduplicates rather than duplicating. And a broken message
// — real archives are full of them — is counted and skipped, never fatal.
//
// IMPORTED MAIL IS NEVER WRITTEN BACK TO IMAP. It is filed under a local account
// created with sync_enabled=false and it gets no message_provenance row, which is
// what structurally prevents writeback: every writeback intent is enqueued per LIVE
// ACCOUNT, and a message with no provenance has none. See internal/importer's
// package doc.
//
// Why a separate binary, not a `mailroom-admin` subcommand: like cmd/mailroom-drill
// (the restore drill), this is an OPERATOR tool that writes mail in bulk, and it is
// deliberately kept out of the shipped image — the archive lives on the operator's
// machine, not in the container, and a tool that can write a million messages has no
// business sitting on the production box. It is still part of the module, so
// `go build ./...`, `go vet` and the linters hold it to the same bar, and a breaking
// change to internal/ingest breaks it at BUILD time rather than mid-import.
//
// Configuration (shared with the server, internal/config — but NOT config.Load(),
// which validates the server's OIDC block that an import has no use for):
//
//	MAILROOM_DATABASE_URL              mailroom_app DSN (required)
//	MAILROOM_DATABASE_PASSWORD_FILE    the DSN's password, injected in memory (#537)
//	MAILROOM_MASTER_KEY_FILE           the master key the mail-blob DEK is derived from
//	MAILROOM_BLOB_ROOT                 the blob store
//	MAILROOM_CLAMD_SOCKET              the malware scanner (optional; unset = no scanning)
//
// ⚠ THE SCANNER IS READ FROM THE SAME VARIABLE THE SERVER USES, and that is the
// point rather than a convenience (#394, ADR-027 §11). An archive is years of
// mail nothing has ever looked at, which makes an import the single largest
// intake of unscanned attachments this system will ever see. Leaving it unset
// is a real choice with a real consequence: every imported attachment is stored
// 'unscanned', and nothing looks at it before it lands. The server's re-scan job
// does pick those rows up later — a never-scanned row counts as stale, and one
// with no content digest is due whatever its age (rescanDueSQL in
// internal/store/rescan.go) — but only on a server that HAS a scanner, and only
// once the mail is already stored. Scanning here is the only check before that.
//
// It is fail-closed like the server's: if the daemon stops answering the import
// STOPS, at the first message, rather than marking mail imported-and-unchecked.
// The checkpoint has not advanced past it, so re-running once the scanner is
// back resumes exactly where it stopped.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/blob"
	"mailroom/internal/clamd"
	"mailroom/internal/config"
	"mailroom/internal/crypto"
	"mailroom/internal/importer"
	"mailroom/internal/ingest"
	"mailroom/internal/onboarding"
	"mailroom/internal/pgdsn"
	"mailroom/internal/store"
	"mailroom/internal/syncrunner"
)

// blobPathInfo is the HKDF label for the blob store's per-user path-derivation key
// (an HMAC key, not an encryption key). It MUST match cmd/mailroom,
// cmd/mailroom-admin and cmd/mailroom-drill: an importer that derived different
// paths would write its blobs where the server will never look for them.
const blobPathInfo = "mailroom-blob-path-v1"

func main() {
	if err := run(os.Args[1:]); err != nil {
		if errors.Is(err, context.Canceled) {
			// Ctrl-C during a multi-hour import is an ordinary outcome, not a fault:
			// the cursor is flushed and a re-run resumes.
			fmt.Fprintln(os.Stderr, "mailroom-import: interrupted — re-run to resume")
			os.Exit(130)
		}
		fmt.Fprintln(os.Stderr, "mailroom-import: "+err.Error())
		os.Exit(1)
	}
}

func run(args []string) error {
	fs := flag.NewFlagSet("mailroom-import", flag.ContinueOnError)
	userIDStr := fs.String("user-id", "", "the user to import into (ULID)")
	subject := fs.String("subject", "", "the user's OIDC subject (alternative to --user-id)")
	kind := fs.String("kind", "auto", "archive layout: auto|mbox|maildir")
	account := fs.String("account", "Archive (imported)", "local account imported mail is filed under")
	prefix := fs.String("mailbox-prefix", "Imported", "mailbox name prefix for imported folders")
	statePath := fs.String("state", "", "resume-cursor file (default: <archive>.mailroom-import.json)")
	noResume := fs.Bool("no-resume", false, "ignore any saved cursor and import from the beginning")
	dryRun := fs.Bool("dry-run", false, "read the archive and report the mapping; touch nothing")
	maxBytes := fs.Int64("max-message-bytes", importer.DefaultMaxMessageBytes,
		"skip (and count) any message larger than this")
	quiet := fs.Bool("quiet", false, "no progress output")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: mailroom-import [flags] <archive path>")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		fs.Usage()
		return errors.New("exactly one archive path is required")
	}
	archive := fs.Arg(0)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	d, err := open(ctx)
	if err != nil {
		return err
	}
	defer d.close()

	userID, err := resolveUser(ctx, d, *userIDStr, *subject)
	if err != nil {
		return err
	}

	k := importer.Kind(*kind)
	if k == importer.KindAuto {
		if k, err = importer.Detect(archive); err != nil {
			return err
		}
	}
	fp, err := importer.FingerprintArchive(archive, k)
	if err != nil {
		return err
	}
	src, err := importer.Open(archive, k, *maxBytes)
	if err != nil {
		return err
	}
	defer func() { _ = src.Close() }()

	opt := importer.Options{
		UserID:          userID,
		AccountName:     *account,
		MailboxPrefix:   *prefix,
		MaxMessageBytes: *maxBytes,
		StatePath:       statePathFor(*statePath, archive),
		Resume:          !*noResume,
		DryRun:          *dryRun,
		Logger:          log,
	}
	if !*quiet {
		opt.Progress = printProgress
	}

	if !*dryRun {
		dek, err := crypto.DeriveKey(d.master, userID[:], syncrunner.MailBlobInfoV1)
		if err != nil {
			return fmt.Errorf("derive mail dek: %w", err)
		}
		defer crypto.Zero(dek)
		opt.DEK = dek
		opt.DEKID = syncrunner.MailBlobSchemeV1

		// A REAL sealed envelope for the local archive account: it holds no
		// credentials (there is nothing to log in to), but app.accounts.credentials_ct
		// is NOT NULL and the restore drill proves the key store survived a backup by
		// OPENING accounts[0]'s envelope — which, ordered by name, may well be this
		// one. Zero bytes there would turn a healthy restore into a red drill.
		creds, err := onboarding.SealCredentials(d.master, userID, onboarding.Credentials{})
		if err != nil {
			return fmt.Errorf("seal archive account credentials: %w", err)
		}
		opt.CredentialsCT = creds
		opt.CredentialsDEK = onboarding.CredentialsSchemeV1
	}

	ingestOpts := []ingest.Option{}
	if sock := strings.TrimSpace(os.Getenv(config.EnvClamdSocket)); sock != "" {
		fmt.Fprintf(os.Stderr, "attachment scanning ON via %s; the import STOPS if the scanner stops answering\n", sock)
		ingestOpts = append(ingestOpts, ingest.WithScanner(&clamd.Client{Network: "unix", Address: sock}))
	} else {
		fmt.Fprintf(os.Stderr, "attachment scanning OFF (%s unset): imported attachments are stored 'unscanned' "+
			"until the server's re-scan job (which needs a scanner) reaches them\n",
			config.EnvClamdSocket)
	}

	im, err := importer.New(d.store, ingest.New(d.store, d.blobs, ingestOpts...), opt)
	if err != nil {
		return err
	}

	fmt.Fprintf(os.Stderr, "importing %s (%s) into user %s%s\n",
		archive, k, userID, dryRunSuffix(*dryRun))

	stats, runErr := im.Run(ctx, src, fp)
	printSummary(stats, im, *dryRun)
	return runErr
}

func dryRunSuffix(dry bool) string {
	if dry {
		return " — DRY RUN, nothing will be written"
	}
	return ""
}

// statePathFor puts the cursor next to the archive by default: it describes THAT
// archive on THIS machine (see internal/importer/checkpoint.go), so that is where
// it belongs and where a second run will look for it.
func statePathFor(explicit, archive string) string {
	if explicit != "" {
		return explicit
	}
	abs, err := filepath.Abs(archive)
	if err != nil {
		abs = archive
	}
	return abs + ".mailroom-import.json"
}

// --- progress ----------------------------------------------------------------

func printProgress(p importer.Progress) {
	line := fmt.Sprintf("  %s  imported=%d deduped=%d skipped=%d failed=%d  %s  %.0f msg/s",
		p.Elapsed.Round(time.Second), p.Imported, p.Deduped, p.Skipped, p.Failed,
		bytesDone(p), p.MessagesSec)
	if p.ETA > 0 {
		line += "  eta " + p.ETA.Round(time.Second).String()
	}
	fmt.Fprintln(os.Stderr, line)
}

func bytesDone(p importer.Progress) string {
	if p.BytesTotal > 0 {
		return fmt.Sprintf("%s/%s (%.1f%%)", humanBytes(p.Bytes), humanBytes(p.BytesTotal),
			100*float64(p.Bytes)/float64(p.BytesTotal))
	}
	return humanBytes(p.Bytes)
}

func printSummary(s importer.Stats, im *importer.Importer, dry bool) {
	fmt.Fprintln(os.Stderr, "----")
	if dry {
		folders, labels := im.Tallies()
		fmt.Fprintln(os.Stderr, "mailboxes this import WOULD create:")
		for _, t := range folders {
			fmt.Fprintf(os.Stderr, "  %-40s %d\n", t.Name, t.Count)
		}
		if len(labels) > 0 {
			fmt.Fprintln(os.Stderr, "labels this import WOULD create:")
			for _, t := range labels {
				fmt.Fprintf(os.Stderr, "  %-40s %d\n", t.Name, t.Count)
			}
		}
	}
	fmt.Fprintf(os.Stderr, "items=%d imported=%d deduped=%d left-alone=%d skipped=%d failed=%d bytes=%s\n",
		s.Items, s.Imported, s.Deduped, s.LeftAlone, s.Skipped, s.Failed, humanBytes(s.Bytes))
	if rss := peakRSS(); rss > 0 {
		fmt.Fprintf(os.Stderr, "peak rss=%s\n", humanBytes(rss))
	}
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return strconv.FormatInt(n, 10) + " B"
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

// peakRSS reports the process's high-water resident set size. It reads VmHWM from
// /proc, which is the kernel's own accounting — the number that decides whether the
// OOM killer takes an interest in a 25 GB import. Returns 0 where /proc is absent.
func peakRSS() int64 {
	b, err := os.ReadFile("/proc/self/status")
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(string(b), "\n") {
		if !strings.HasPrefix(line, "VmHWM:") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			return 0
		}
		kb, err := strconv.ParseInt(fields[1], 10, 64)
		if err != nil {
			return 0
		}
		return kb * 1024
	}
	return 0
}

// --- wiring ------------------------------------------------------------------

// settings is the importer's slice of the server's configuration. Like the restore
// drill, it deliberately does NOT call config.Load(): that validates the SERVER's
// auth block and fails without an OIDC issuer, and an import has no more use for an
// identity provider than a restore does. The env NAMES are still config's, so there
// is one source of truth for them.
type settings struct {
	databaseURL   string
	masterKeyFile string
	blobRoot      string
}

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
	if s.blobRoot, err = env(config.EnvBlobRoot, config.DefaultBlobRoot); err != nil {
		return s, err
	}
	return s, nil
}

type deps struct {
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
	master, err := crypto.NewFileKeyProvider(cfg.masterKeyFile).MasterKey(ctx)
	if err != nil {
		return nil, fmt.Errorf("master key from %s: %w", cfg.masterKeyFile, err)
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
	d := &deps{cfg: cfg, store: st, blobs: blobs, master: master}
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
