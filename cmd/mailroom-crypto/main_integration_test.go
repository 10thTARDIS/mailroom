// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package main

// #525: a crypto process given the wrong PGP root never becomes healthy.
//
// This boots the REAL run() — the function the container's entrypoint calls —
// against the real app.pgp_root_check as mailroom_app, and judges it with the
// call the compose healthcheck makes: `mailroom-crypto -healthcheck` is
// pgpwire.Client.Health on the socket and nothing more. The three cases the
// issue requires run as one sequence, because they are one database's life:
//
//	1. fresh install, nothing recorded → healthy, and this root is recorded
//	2. the same root again            → healthy (the positive control)
//	3. a different root               → run() returns before listening, and
//	                                    the healthcheck fails
//
// ⚠ Case 3 has to be shown able to fail, and the probe that shows it is NOT the
// obvious one. Deleting the VerifyRoot call from run() does turn this red — but
// at case 1, "holds 0 row(s)", because that deletes the recording too, and case
// 3 is never reached. So that probe says nothing about case 3. The one that
// does is the defect's own shape: keep the call and ignore its error, i.e. log
// and carry on. Cases 1 and 2 then pass and case 3 fails with "given the WRONG
// PGP root became healthy" (both measured on #525). Cases 1 and 2 are also what
// make "the healthcheck failed" in case 3 mean anything: the same helper,
// pointed the same way, does get an answer from a process that is up.

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"mailroom/internal/crypto"
	"mailroom/internal/pgpkeyd"
	"mailroom/internal/pgpwire"
)

const bootTimeout = 15 * time.Second

func TestCryptoProcessWithTheWrongRootNeverBecomesHealthy(t *testing.T) {
	superURL := os.Getenv("MAILROOM_TEST_SUPER_URL")
	appURL := os.Getenv("MAILROOM_TEST_APP_URL")
	if superURL == "" || appURL == "" {
		t.Skip("integration database not configured — run via scripts/integration-test.sh")
	}
	ctx := context.Background()
	super, err := pgxpool.New(ctx, superURL)
	if err != nil {
		t.Fatalf("open super pool: %v", err)
	}
	t.Cleanup(super.Close)

	// One row per database: start from none, leave none.
	wipe := func() error {
		_, err := super.Exec(context.Background(), `DELETE FROM app.pgp_root_check`)
		return err
	}
	if err := wipe(); err != nil {
		t.Fatalf("reset app.pgp_root_check: %v", err)
	}
	t.Cleanup(func() {
		if err := wipe(); err != nil {
			t.Errorf("clean up app.pgp_root_check: %v", err)
		}
	})

	rootA, rootB := testRoot(0x11), testRoot(0x77)
	wantA, err := pgpkeyd.RootCheckValue(rootA)
	if err != nil {
		t.Fatal(err)
	}
	// recorded reads the table as the superuser — not through the code under
	// test.
	recorded := func() []byte {
		t.Helper()
		var n int
		if err := super.QueryRow(ctx, `SELECT count(*) FROM app.pgp_root_check`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 1 {
			t.Fatalf("app.pgp_root_check holds %d row(s), want 1", n)
		}
		var v []byte
		if err := super.QueryRow(ctx, `SELECT check_value FROM app.pgp_root_check`).Scan(&v); err != nil {
			t.Fatal(err)
		}
		return v
	}

	// 1. A fresh install: nothing recorded. It must come up, and it must record
	//    THIS root — otherwise case 3 compares against nothing in particular.
	p := boot(t, appURL, rootA)
	if err := p.waitHealthy(t); err != nil {
		t.Fatalf("fresh install: run() exited instead of serving: %v", err)
	}
	p.stop(t)
	if got := recorded(); !bytes.Equal(got, wantA) {
		t.Fatalf("fresh install recorded %x, want this root's check value %x", got, wantA)
	}

	// 2. The same root again: the positive control.
	p = boot(t, appURL, rootA)
	if err := p.waitHealthy(t); err != nil {
		t.Fatalf("the SAME root was refused: %v", err)
	}
	p.stop(t)

	// 3. A different root — a restore paired with the wrong escrow envelope, or
	//    with one freshly generated from deploy/README.md §2.
	p = boot(t, appURL, rootB)
	err = p.waitHealthy(t)
	if err == nil {
		t.Fatal("a crypto process given the WRONG PGP root became healthy — the defect #525 exists to close")
	}
	if !errors.Is(err, pgpkeyd.ErrRootMismatch) {
		t.Fatalf("run() failed, but not with ErrRootMismatch: %v", err)
	}
	if p.healthy() {
		t.Error("run() refused the root, yet the healthcheck still answers")
	}
	if got := recorded(); !bytes.Equal(got, wantA) {
		t.Errorf("the refused root's boot changed the recorded value to %x", got)
	}
}

// process is one run() of the crypto process.
type process struct {
	client *pgpwire.Client
	cancel context.CancelFunc
	done   chan error // run()'s result, received at most once — see exited
	exited bool
}

// boot starts run() with root on a fresh socket, as the container does apart
// from where the configuration comes from.
func boot(t *testing.T, appURL string, root []byte) *process {
	t.Helper()
	dir := t.TempDir()
	rootFile := filepath.Join(dir, "pgp_root_key")
	if err := os.WriteFile(rootFile, []byte(hex.EncodeToString(root)+"\n"), 0o600); err != nil {
		t.Fatalf("write root file: %v", err)
	}
	cfg := pgpkeyd.Config{
		SocketPath:  filepath.Join(dir, "crypto.sock"),
		RootKeyFile: rootFile,
		DatabaseURL: appURL,
		LogLevel:    "info",
		LogFormat:   "text",
	}
	ctx, cancel := context.WithCancel(context.Background())
	p := &process{client: pgpwire.NewClient(cfg.SocketPath), cancel: cancel, done: make(chan error, 1)}
	go func() { p.done <- run(ctx, cfg) }()
	t.Cleanup(func() { p.stop(t) })
	return p
}

// waitHealthy returns nil once the healthcheck answers, or run()'s error if the
// process exits first — whichever happens first — and fails the test if
// neither happens.
func (p *process) waitHealthy(t *testing.T) error {
	t.Helper()
	deadline := time.Now().Add(bootTimeout)
	for time.Now().Before(deadline) {
		select {
		case err := <-p.done:
			p.exited = true
			if err == nil {
				return errors.New("run() returned nil without being cancelled")
			}
			return err
		default:
		}
		if p.healthy() {
			return nil
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("run() neither became healthy nor exited within %v", bootTimeout)
	return nil
}

// healthy is the compose healthcheck, `mailroom-crypto -healthcheck`.
func (p *process) healthy() bool {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	return p.client.Health(ctx) == nil
}

// stop cancels run() and requires it to return cleanly. Safe to call twice.
func (p *process) stop(t *testing.T) {
	t.Helper()
	p.cancel()
	if p.exited {
		return
	}
	select {
	case err := <-p.done:
		p.exited = true
		if err != nil {
			t.Errorf("run() after cancellation: %v", err)
		}
	case <-time.After(bootTimeout):
		t.Error("run() did not return after cancellation")
	}
}

func testRoot(seed byte) []byte {
	root := make([]byte, crypto.KeySize)
	for i := range root {
		root[i] = seed + byte(i*3)
	}
	return root
}
