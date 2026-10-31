// SPDX-License-Identifier: AGPL-3.0-only

// Command mailroom-migrate applies the forward-only schema migrations
// (ADR-008, docs/migrations.md) with goose over the embedded db/migrations.
//
// It must connect with the OWNER DSN (mailroom_owner): migrations create and
// alter objects and must NOT run as the RLS-constrained application role
// (mailroom_app). Supply it via MAILROOM_MIGRATE_DATABASE_URL, or the Docker
// secret indirection MAILROOM_MIGRATE_DATABASE_URL_FILE.
//
// The URL itself carries no password (#537): MAILROOM_MIGRATE_DATABASE_PASSWORD_FILE
// names a file — the SAME secret the role-bootstrap step used to set
// mailroom_owner's password — whose contents are injected into the URL's
// userinfo in memory, via internal/pgdsn, right before connecting.
//
// Usage: mailroom-migrate <up|down|status|version|up-to VERSION|down-to VERSION>
package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"strconv"
	"strings"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"

	"mailroom/db"
	"mailroom/internal/pgdsn"
)

// migrationsDir is the path within the embedded FS (db.Migrations embeds
// "migrations/*.sql", so the goose base directory is "migrations").
const migrationsDir = "migrations"

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "mailroom-migrate:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: mailroom-migrate <up|down|status|version|up-to VERSION|down-to VERSION>")
	}
	command, rest := args[0], args[1:]

	dsn, err := resolveDSN()
	if err != nil {
		return err
	}

	sqldb, err := sql.Open("pgx", dsn)
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer func() { _ = sqldb.Close() }()

	goose.SetBaseFS(db.Migrations)
	if err := goose.SetDialect("postgres"); err != nil {
		return fmt.Errorf("set dialect: %w", err)
	}
	warnIfDatabaseIsAhead(sqldb, os.Stderr)
	if err := goose.RunContext(context.Background(), command, sqldb, migrationsDir, rest...); err != nil {
		return fmt.Errorf("goose %s: %w", command, err)
	}
	return nil
}

// warnIfDatabaseIsAhead reports a database that has been migrated by a NEWER
// build than the one running now.
//
// WHY THIS EXISTS. `goose up` against a database ahead of its own migration set
// finds nothing to apply and says so cheerfully — which is indistinguishable
// from a correct, fully-migrated deploy. That is exactly how a `docker compose
// up` performed while the host was checked out on a feature branch left a
// deployed instance carrying an unreleased table, with the deploy output
// giving no hint (mailroom#177).
//
// It matters beyond tidiness because **goose records applied migrations by
// VERSION NUMBER, not by content**. Once version N is recorded, the release that
// eventually ships version N will SKIP it — so if that migration is edited
// before it lands, the database keeps the older definition permanently and
// nothing anywhere says so.
//
// It WARNS rather than fails: deploying an older tag on purpose (a rollback) is
// legitimate and must not be blocked by its own diagnostics. Failing closed here
// would turn a recoverable situation into an outage.
func warnIfDatabaseIsAhead(sqldb *sql.DB, out io.Writer) {
	// A fresh database has no goose table yet — the overwhelmingly common first
	// deploy, and not a drift condition. Any error that actually matters will be
	// raised by the goose command itself a moment later, so this stays silent.
	dbVersion, err := goose.GetDBVersion(sqldb)
	if err != nil {
		return
	}
	names, err := migrationNames()
	if err != nil {
		return
	}
	if msg := driftWarning(dbVersion, highestVersion(names)); msg != "" {
		_, _ = fmt.Fprint(out, msg)
	}
}

// driftWarning renders the operator-facing warning for a database ahead of this
// build, or "" when there is nothing to report. It is split out from the
// database plumbing so BOTH answers are unit-testable: a warning that fired when
// it should not would cry wolf on every deploy until people learned to ignore
// it, which is the same as not having one.
//
// carried == 0 means no migrations were readable at all; that disables the check
// rather than declaring every database ahead of an empty set.
func driftWarning(dbVersion, carried int64) string {
	if carried == 0 || dbVersion <= carried {
		return ""
	}
	return fmt.Sprintf(`
WARNING: the database is at schema version %d, but this build carries
migrations only up to %d.

The database was migrated by a NEWER build — most likely a feature-branch image
deployed to this instance, or a rollback to an older release. Objects created by
those migrations are still present and this build knows nothing about them.

goose records applied migrations BY VERSION NUMBER, not by content, so it will
SKIP version %d if a later release ships it. If that migration is edited before
it lands, this database keeps the older definition and nothing reports it.
Compare the schema against the migration before relying on it.

`, dbVersion, carried, carried+1)
}

// migrationNames lists the embedded migration filenames.
func migrationNames() ([]string, error) {
	entries, err := fs.ReadDir(db.Migrations, migrationsDir)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() {
			names = append(names, e.Name())
		}
	}
	return names, nil
}

// highestVersion returns the largest goose version among migration filenames
// ("00018_label_suggestions.sql" -> 18), or 0 when none parse.
//
// It applies goose's own rules (NumericComponent): a ".sql" extension, a "_"
// separator, and a numeric prefix before it — a name missing any of those is not
// a migration and is skipped. Agreeing with goose about which version a file IS
// is what keeps the check honest; a parser that disagreed would either warn
// about drift that does not exist or stay silent about drift that does.
func highestVersion(names []string) int64 {
	var max int64
	for _, name := range names {
		if !strings.HasSuffix(name, ".sql") {
			continue
		}
		i := strings.IndexByte(name, '_')
		if i < 0 {
			continue
		}
		v, err := strconv.ParseInt(name[:i], 10, 64)
		if err != nil {
			continue
		}
		if v > max {
			max = v
		}
	}
	return max
}

// resolveDSN reads the owner DSN, honoring the _FILE (Docker secret)
// convention used elsewhere in the project (see internal/config), then
// injects the owner password from MAILROOM_MIGRATE_DATABASE_PASSWORD_FILE if
// set (#537, internal/pgdsn) — the URL itself is expected to carry none.
func resolveDSN() (string, error) {
	const key = "MAILROOM_MIGRATE_DATABASE_URL"
	const pwKey = "MAILROOM_MIGRATE_DATABASE_PASSWORD_FILE"

	var raw string
	switch {
	case os.Getenv(key+"_FILE") != "":
		path := os.Getenv(key + "_FILE")
		b, err := os.ReadFile(path)
		if err != nil {
			return "", fmt.Errorf("read %s_FILE: %w", key, err)
		}
		raw = strings.TrimSpace(string(b))
	case os.Getenv(key) != "":
		raw = os.Getenv(key)
	default:
		return "", fmt.Errorf("%s (or %s_FILE) is required — the mailroom_owner DSN", key, key)
	}

	return pgdsn.WithPasswordFile(raw, strings.TrimSpace(os.Getenv(pwKey)))
}
