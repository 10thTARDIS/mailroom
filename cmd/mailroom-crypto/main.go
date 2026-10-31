// SPDX-License-Identifier: AGPL-3.0-only

// Command mailroom-crypto is the crypto process (ADR-024): the only place in
// Mailroom that holds PGP private key material.
//
// It is a SEPARATE BINARY from the server, with its own root secret, and it
// does not link internal/config — it cannot read the application's master key,
// because the code that knows how to find it is not in this binary.
//
// ⚠ CORRECTION TO WHAT THIS COMMENT SAID IN INCREMENT 2. It used to claim the
// binary linked neither internal/config NOR internal/store, and that it could
// not reach the database. The second half stopped being true in increment 3:
// keys are persisted in app.user_keys, internal/store is the only package
// permitted to touch the database (CLAUDE.md), so this binary links it and
// holds a DSN. What did NOT change is the part that matters — it still has its
// own root secret and the server still does not.
//
// The database is not the boundary. It connects as mailroom_app, exactly like
// the server, and both are subject to the same RLS. The server can read
// private_key_ct and gets ciphertext it cannot open, because it does not have
// the root that seals it. Giving the crypto process its own DB role was
// considered and declined: every RLS policy in the schema is written
// FOR ALL TO mailroom_app, so a second role would need new policies on every
// table for no gain against the threat this design is actually about.
//
//	mailroom-crypto                  # verify the root, then serve on $MAILROOM_PGP_SOCKET
//	mailroom-crypto -healthcheck     # dial the socket, exit 0 if healthy
//	mailroom-crypto -version
//
// ⚠ THE SOCKET EXISTS ONLY ONCE THE ROOT CHECK HAS PASSED (#525). run() compares
// the PGP root with the value app.pgp_root_check recorded for this database and
// returns before listening on a mismatch. So the healthcheck, which only dials,
// fails — and nothing can ask this process to sign with a root that opens no
// stored key, or to seal a NEW key under a root the escrow does not hold.
//
// The application talks to it through internal/pgpwire over a Unix socket.
// There is no network listener and no configuration that could create one.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"mailroom/internal/crypto"
	"mailroom/internal/pgpkeyd"
	"mailroom/internal/pgpwire"
	"mailroom/internal/store"
)

// version is stamped at build time (-ldflags "-X main.version=..."). An
// unstamped build must not claim to be a release.
var version = "0.0.0-dev"

func main() {
	showVersion := flag.Bool("version", false, "print the version and exit")
	healthcheck := flag.Bool("healthcheck", false, "dial the socket, report health, exit")
	flag.Parse()

	if *showVersion {
		fmt.Println(version)
		return
	}

	cfg, err := pgpkeyd.FromEnv()
	if err != nil {
		fmt.Fprintf(os.Stderr, "mailroom-crypto: %v\n", err)
		os.Exit(2)
	}

	if *healthcheck {
		// Its own mode rather than a shell one-liner in the compose file:
		// nothing in a minimal image speaks HTTP over a Unix socket, and the
		// alternative is installing a tool that can — more surface in the one
		// container that should have least.
		if err := pgpwire.NewClient(cfg.SocketPath).Health(context.Background()); err != nil {
			fmt.Fprintf(os.Stderr, "mailroom-crypto: unhealthy: %v\n", err)
			os.Exit(1)
		}
		fmt.Println("ok")
		return
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	err = run(ctx, cfg)
	stop()
	if err != nil {
		fmt.Fprintf(os.Stderr, "mailroom-crypto: %v\n", err)
		os.Exit(1)
	}
}

// run is the whole process between configuration and exit: read the root,
// verify it against the database, serve until ctx is cancelled.
//
// It takes its context from the caller, rather than installing the signal
// handler itself, so that the boot test can drive the same function the
// container's entrypoint calls (main_integration_test.go).
func run(ctx context.Context, cfg pgpkeyd.Config) error {
	log := newLogger(cfg.LogFormat, cfg.LogLevel)

	// The root secret has exactly one lifetime and one owner: read here,
	// handed to the service, zeroized here. It is never logged, never written,
	// and never leaves this process.
	root, err := crypto.ReadKeyFile(cfg.RootKeyFile, "PGP root key")
	if err != nil {
		return err
	}
	defer crypto.Zero(root)

	db, err := store.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer db.Close()

	vault, err := pgpkeyd.NewStoreVault(db)
	if err != nil {
		return err
	}
	svc, err := pgpkeyd.NewService(root, vault, log)
	if err != nil {
		return err
	}
	handler, err := pgpkeyd.NewHandler(svc, log)
	if err != nil {
		return err
	}

	log.Info("mailroom-crypto starting",
		"version", version,
		"socket", cfg.SocketPath,
		"root_key_file", cfg.RootKeyFile,
		"storage", "app.user_keys (PostgreSQL, under RLS)")

	// ─── THE ROOT CHECK (#525) ──────────────────────────────────────────────
	// BEFORE the socket exists, and fatal. This process used to read its root
	// here and check nothing until someone signed or decrypted, so a restore
	// paired with the wrong root booted, reported healthy, and served for as
	// long as nobody used a key. Returning now means the healthcheck fails —
	// it dials the socket, and there is no socket.
	//
	// It is also why nothing below this line can seal a key under a root the
	// database has not accepted: Generate is only reachable through Serve.
	check, err := pgpkeyd.NewStoreRootCheck(db)
	if err != nil {
		return err
	}
	if _, err := svc.VerifyRoot(ctx, check); err != nil {
		return err
	}
	// ────────────────────────────────────────────────────────────────────────

	if err := pgpkeyd.Serve(ctx, cfg.SocketPath, handler, log); err != nil {
		return err
	}
	log.Info("mailroom-crypto stopped")
	return nil
}

// newLogger is a five-line copy of internal/server.NewLogger rather than an
// import of it: internal/server pulls in the whole HTTP application, and the
// point of this binary is that it links as little as possible.
func newLogger(format, level string) *slog.Logger {
	opts := &slog.HandlerOptions{Level: parseLevel(level)}
	var h slog.Handler = slog.NewJSONHandler(os.Stdout, opts)
	if format == "text" {
		h = slog.NewTextHandler(os.Stdout, opts)
	}
	return slog.New(h)
}

func parseLevel(level string) slog.Level {
	switch level {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}
