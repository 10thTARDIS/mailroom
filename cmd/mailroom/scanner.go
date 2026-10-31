// SPDX-License-Identifier: AGPL-3.0-only

package main

// The adapter that lets ingest scan without knowing clamd exists (#394,
// ADR-027 §4), mirroring signer.go: internal/ingest declares a two-method
// Scanner interface and nothing else, and the client is constructed here.

import (
	"log/slog"

	"mailroom/internal/clamd"
	"mailroom/internal/config"
)

// newScanner returns a malware scanner for the clamd at cfg.ClamdSocket, or nil
// when none is configured.
//
// ⚠ Returning nil is a real answer, not a failure — the same shape as
// newCryptoSigner. An unconfigured socket means "do not scan", and ingest treats
// a nil Scanner as exactly that: rows keep app.attachments' 'unscanned' state,
// which §5d's aggregate reports rather than hides. Constructing a client that
// would fail on first use instead would turn an operator choice into an account
// whose mail silently stops arriving.
//
// ⚠ AND THE LOG LINES ARE NOT DECORATION. Enabling this makes ingest
// fail-closed: a scanner that stops answering pauses every account's sync until
// it answers again. An operator who cannot tell from the boot log whether
// scanning is on cannot diagnose that pause, and "mail stopped arriving" is the
// symptom they will actually be looking at.
func newScanner(cfg *config.Config, log *slog.Logger) *clamd.Client {
	if cfg.ClamdSocket == "" {
		log.Info("attachment scanning is OFF: no scanner socket configured; attachment verdicts stay 'unscanned'",
			"env", config.EnvClamdSocket)
		return nil
	}
	max := cfg.ClamdMaxBytes
	if max == 0 {
		max = clamd.MaxStreamBytes
	}
	log.Info("attachment scanning is ON; a scanner outage will PAUSE sync and leave mail on the provider",
		"clamd_socket", cfg.ClamdSocket, "max_bytes", max)
	return &clamd.Client{
		Network: "unix",
		Address: cfg.ClamdSocket,
		// ⚠ The client's own timeouts, left at their defaults on purpose: they
		// are generous (90 s) because clamd stalls for seconds while reloading
		// its signature database, and treating a reload as an outage would
		// refuse mail for no reason. See internal/clamd's timeout block.
		MaxBytes: max,
	}
}
