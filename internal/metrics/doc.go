// SPDX-License-Identifier: AGPL-3.0-only

// Package metrics defines Mailroom's Prometheus observability surface
// (spec §16 "Observability"): per-account sync lag/IDLE state/action-queue
// depth/writeback failures, plus global index cursor lag, job-queue
// depth/dead-letters, model version/feedback counts, and outbound ledger
// levels.
//
// Metrics are registered on a private *prometheus.Registry (never the
// global DefaultRegisterer) so tests stay isolated and callers control
// exactly what's exposed on /metrics.
package metrics
