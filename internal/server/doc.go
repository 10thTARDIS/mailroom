// SPDX-License-Identifier: AGPL-3.0-only

// Package server is the operational spine the JMAP API and sync engine
// hang off (spec §16): an HTTP server exposing liveness/readiness probes
// and Prometheus metrics, structured logging via log/slog, and a graceful
// start/stop lifecycle driven by a cancellable context.
//
// It deliberately does not import internal/crypto or internal/sync — those
// subsystems aren't ready yet — and instead accepts small function/interface
// collaborators (ReadinessFunc, an http.Handler for /metrics) so callers can
// wire them in from cmd/mailroom without this package growing hard
// dependencies on work in progress.
package server
