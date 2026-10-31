// SPDX-License-Identifier: AGPL-3.0-only

package metrics

import (
	"net/http"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// namespace prefixes every metric name (mailroom_...).
const namespace = "mailroom"

// Metrics is Mailroom's typed Prometheus registry (spec §16). All metrics
// are registered on a private *prometheus.Registry, never the global
// DefaultRegisterer, so callers (and tests) control exactly what's exposed
// and stay isolated from each other.
//
// ⚠ EVERY FIELD HERE MUST HAVE A CALL SITE THAT SETS IT, and that is enforced
// by TestEveryDeclaredMetricIsSetSomewhere rather than left to review. For most
// of this file's life it was not true: 8 of 10 metrics were declared,
// registered and served while nothing ever set them, so /metrics emitted a
// constant 0 for each — not a missing dashboard but one that actively asserted
// everything was fine (#524).
//
// ModelVersion was DELETED rather than wired in that pass. It described itself
// as a stub ("internal/ml TBD") and its labels were (account, model), while the
// real model registry keys versions per USER. Wiring a per-user number onto an
// account label would have been a metric fed by the wrong thing, which is worse
// than one that is absent because it reads as authoritative. If an ml metric is
// wanted, it should be declared to match what ml actually knows.
type Metrics struct {
	registry *prometheus.Registry

	// Per-account (spec §16 "Per-account").

	// SyncLagSeconds is how far behind an account's IMAP sync is.
	SyncLagSeconds *prometheus.GaugeVec // labels: account

	// IdleConnState reports the IMAP IDLE connection state per account
	// (e.g. 0=down, 1=connecting, 2=idling — the sync engine owns the
	// exact encoding).
	IdleConnState *prometheus.GaugeVec // labels: account

	// ActionQueueDepth is how many writeback actions (label, move, delete, ...)
	// are still OWED to the server for an account: pending plus inflight.
	// Terminal states are excluded — 'applied' is done and 'dead' is surfaced
	// separately in account health, and counting either would make this a
	// running total that never falls. See store.CountOutstandingIntents.
	ActionQueueDepth *prometheus.GaugeVec // labels: account

	// WritebackFailuresTotal counts failed writeback attempts, the basis
	// for the writeback failure rate.
	WritebackFailuresTotal *prometheus.CounterVec // labels: account

	// Global (spec §16 "Global").

	// IndexCursorLagSeconds is how far the search index consumer is
	// behind the change log, across all users.
	IndexCursorLagSeconds prometheus.Gauge

	// JobQueueDepth is the BACKLOG of the durable job table (spec §16 "Durable
	// job model"): jobs that are pending and already due, across all users.
	//
	// ⚠ "Due", not merely pending, and the wording was corrected when this was
	// first wired (#524). app.jobs also holds pending rows scheduled far in the
	// future — a snooze for next week — and counting those would make the gauge
	// climb with ordinary use and mean nothing when it did. Jobs stranded in
	// 'running' are excluded too: migration 00041's lease returns them to the
	// claimable set on their own.
	JobQueueDepth prometheus.Gauge

	// JobDeadLettersTotal counts jobs that exhausted their retry budget
	// and landed in the dead-letter state, by job kind.
	JobDeadLettersTotal *prometheus.CounterVec // labels: kind

	// OutboundLedgerLevel is how many sends the account has drawn from TODAY'S
	// budget (UTC), sampled on each sync pass so it resets at midnight without
	// waiting for the day's first send. See store.OutboundSentToday.
	OutboundLedgerLevel *prometheus.GaugeVec // labels: account

	// SyncDisabledAccounts is how many of a user's accounts have sync switched
	// off — by a failed validation or by the operator (mailroom#435). Mail is
	// not arriving for these; any value above zero is an operator condition,
	// and the point of the gauge is that it can be alerted on, because the log
	// line that used to be the only signal scrolled past once at boot.
	SyncDisabledAccounts *prometheus.GaugeVec // labels: user

	// ScannerSignaturePublishedTimestamp is WHEN the ClamAV signature database
	// clamd has loaded was published, as Unix seconds, from clamd's own VERSION
	// reply (ADR-027 §5c, #641). Alert on `time() - this`.
	//
	// ⚠ A TIMESTAMP, NOT AN AGE, because the value is read as seldom as once a
	// day (the re-scan worker's idle cadence). An age gauge is true only at the
	// instant it is set, so between reads it would stand still while the real
	// age grew, and an alert on it would fire up to a day late. `time() - ts`
	// keeps growing on its own with no read at all.
	//
	// ⚠ LEFT AT ZERO WHEN clamd REPORTS NO PUBLICATION TIME, deliberately:
	// `time() - 0` is enormous, so the alert FIRES, which agrees with
	// clamd.Version.Stale, where an unknown age is stale. A daemon that will
	// not say how old its signatures are is not one to call fresh.
	ScannerSignaturePublishedTimestamp prometheus.Gauge

	// ScannerSignatureVersion is the loaded daily database's version number
	// (e.g. 28132). It moves only when freshclam publishes something new and
	// clamd reloads it; a flat line is the frozen-database failure (#641), and
	// it is what the re-scan tiers gate on.
	ScannerSignatureVersion prometheus.Gauge

	// JMAPMethodDuration is how long each JMAP method call takes, by method
	// name (#665). It answers "which method is slow". A desk load is one POST
	// carrying ~30 calls run serially, so a request-level number cannot say.
	// The label is bounded: an unknown or undeclared method is "unknown",
	// never the client's string.
	JMAPMethodDuration *prometheus.HistogramVec // labels: method

	// JMAPRequestDuration is how long a whole POST /jmap request spends in
	// method dispatch (#665). It answers "how slow is a request, and how much
	// of it is the one method JMAPMethodDuration points at".
	JMAPRequestDuration prometheus.Histogram

	// ProxyRefusalsTotal counts image-proxy requests that did not deliver an
	// image, by closed reason label (#674). It exists to say how often each
	// refusal happens on real mail, so nothing in the proxy is prioritised on a
	// guess. The labels are api.reason* constants, never client text.
	ProxyRefusalsTotal *prometheus.CounterVec // labels: reason

	// LocalDKIMTotal counts the outcomes of Mailroom's own DKIM check at ingest
	// (#776, ADR-036), one increment per newly ingested message that reached
	// the check. The label is a closed set (ingest.LocalDKIMResults), and it
	// includes skipped_trusted_header (a trusted Authentication-Results header
	// exists, nothing was verified) and not_checked (a DNS failure or a spent
	// budget, nothing was stored) so the bound is visible.
	LocalDKIMTotal *prometheus.CounterVec // labels: result

	// ProxyFetchesTotal counts image-proxy requests that delivered an image,
	// result="served" (fetched from the sender) or "cached" (no outbound request).
	// With ProxyRefusalsTotal it gives a refusal RATE rather than a bare count.
	ProxyFetchesTotal *prometheus.CounterVec // labels: result

	// ProxyPrefetchTotal counts images the arrival prefetch handled (#922):
	// result="fetched" (fetched from the sender and cached), "already_cached"
	// (only a reference was added), "refused" (the fetch failed; the log line
	// says why and names the registrable domain) or "empty" (200 with no
	// bytes); and, per RUN rather than per image, "cap_reached" (nothing was
	// fetched because the user's kept images reached the referenced-bytes
	// ceiling). Kept apart from ProxyFetchesTotal and
	// ProxyRefusalsTotal so those stay statements about READS.
	ProxyPrefetchTotal *prometheus.CounterVec // labels: result

	// ProxyCacheBytes and ProxyCacheEntries are one user's image cache, split
	// by kind="referenced" (a live message keeps it: exempt from every
	// eviction, #922) and kind="unreferenced" (bounded by the cache cap and
	// TTL). Referenced images are bounded by no eviction at all, so these are
	// how their growth is seen. Set after each eviction pass and prefetch run,
	// and once per user at start — never per cache hit.
	ProxyCacheBytes   *prometheus.GaugeVec // labels: user, kind
	ProxyCacheEntries *prometheus.GaugeVec // labels: user, kind

	// OnDemandScanTotal counts the verdicts of attachments of DECRYPTED messages
	// scanned at download time (#47): such parts are never scanned at ingest (the
	// ciphertext is opaque), so this is the only record that the on-demand scan
	// ran. Label is a closed set (api.OnDemandVerdicts): clean, flagged, unknown,
	// and unscanned (no scanner configured).
	OnDemandScanTotal *prometheus.CounterVec // labels: verdict

	// SearchDuration is how long one search-index read took, kind="search"
	// (Email/query's candidate set) or kind="snippet" (SearchSnippet/get's
	// highlighting), #724. It is the engine's time alone — what the web
	// client's live-search debounce (SEARCH_DEBOUNCE_MS) has to be weighed
	// against — and it is the number to read before changing that constant.
	// The label is a closed set of two (search.TimingKind), never query text.
	SearchDuration *prometheus.HistogramVec // labels: kind

	// LearnedPlacementTotal counts learned placement's arrival decisions, one
	// increment per new message that reached it, by the reason it did or did
	// not place (#840): placed, or why not (thin, mixed, unauthenticated,
	// warned, ambiguous, ...), and error when the decision failed. The label is
	// a closed set (store.LearnedPlacementLabels, anything else "other"), never
	// a sender, an area or any text from the mail. Before it, every abstention
	// but "gated" was silent.
	LearnedPlacementTotal *prometheus.CounterVec // labels: reason

	// The sync engine's correctness counters (mailsync.Observer).
	//
	// FlagRefreshAbortedTotal counts flag readings a refresh applied in a
	// FETCH command that then failed (#909's report: before the fix, any of
	// them could have been an empty FLAGS cut short). path: refresh, gmail,
	// ingest_reread.
	FlagRefreshAbortedTotal *prometheus.CounterVec // labels: path
	// FetchFlagsAbsentTotal counts messages whose FETCH response lacked the
	// FLAGS it asked for (#909) — none read as "no flags" any more. path:
	// ingest, refresh, gmail, sweep, ingest_reread.
	FetchFlagsAbsentTotal *prometheus.CounterVec // labels: path
	// SyncRecoveredArrivalsTotal counts NEW messages the recovery paths
	// ingested (#908's report), by path (gap, flag_fallback) and outcome
	// (notify, muted, blocked).
	SyncRecoveredArrivalsTotal *prometheus.CounterVec // labels: path, outcome
	// FlagSweepMessagesTotal counts the CONDSTORE-less flag sweep's work
	// (ADR-046 §12): result=read (messages re-read) and applied (whose
	// server flags differed and were applied).
	FlagSweepMessagesTotal *prometheus.CounterVec // labels: result
}

// searchDurationBuckets resolves the range a debounce decision turns on — a
// few milliseconds to a few hundred — and still bounds a pathological search.
var searchDurationBuckets = []float64{0.001, 0.0025, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5}

// jmapDurationBuckets spans a cached read (a few ms) to the request a phone
// gives up on (tens of seconds).
var jmapDurationBuckets = []float64{0.005, 0.025, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30}

// New builds a Metrics with every field registered on a fresh, private
// registry. It never touches prometheus.DefaultRegisterer.
func New() *Metrics {
	registry := prometheus.NewRegistry()
	factory := promauto.With(registry)

	return &Metrics{
		registry: registry,

		SyncLagSeconds: factory.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: namespace,
			Name:      "sync_lag_seconds",
			Help:      "Seconds by which an account's IMAP sync lags behind the mailbox.",
		}, []string{"account"}),

		IdleConnState: factory.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: namespace,
			Name:      "idle_connection_state",
			Help:      "IMAP IDLE connection state per account (sync-engine-defined encoding).",
		}, []string{"account"}),

		ActionQueueDepth: factory.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: namespace,
			Name:      "action_queue_depth",
			Help:      "Writeback actions still owed to the server per account (pending plus inflight).",
		}, []string{"account"}),

		WritebackFailuresTotal: factory.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "writeback_failures_total",
			Help:      "Total failed writeback attempts per account.",
		}, []string{"account"}),

		IndexCursorLagSeconds: factory.NewGauge(prometheus.GaugeOpts{
			Namespace: namespace,
			Name:      "index_cursor_lag_seconds",
			Help:      "Seconds by which the search index consumer lags behind the change log.",
		}),

		JobQueueDepth: factory.NewGauge(prometheus.GaugeOpts{
			Namespace: namespace,
			Name:      "job_queue_depth",
			Help:      "Jobs that are pending and already due across all users (backlog).",
		}),

		JobDeadLettersTotal: factory.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "job_dead_letters_total",
			Help:      "Total jobs that exhausted retries and landed in the dead-letter state, by kind.",
		}, []string{"kind"}),

		OutboundLedgerLevel: factory.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: namespace,
			Name:      "outbound_ledger_level",
			Help:      "Sends drawn from today (UTC) outbound budget, per account.",
		}, []string{"account"}),

		SyncDisabledAccounts: factory.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: namespace,
			Name:      "sync_disabled_accounts",
			Help:      "Accounts per user whose sync is switched off (failed validation or operator pause); mail is not arriving for them.",
		}, []string{"user"}),

		ScannerSignaturePublishedTimestamp: factory.NewGauge(prometheus.GaugeOpts{
			Namespace: namespace,
			Name:      "scanner_signature_published_timestamp_seconds",
			Help:      "Unix time the loaded ClamAV signature database was published; alert on time() minus this. Zero when the daemon reports no publication time.",
		}),

		ScannerSignatureVersion: factory.NewGauge(prometheus.GaugeOpts{
			Namespace: namespace,
			Name:      "scanner_signature_version",
			Help:      "Version number of the loaded ClamAV daily signature database.",
		}),

		JMAPMethodDuration: factory.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: namespace,
			Name:      "jmap_method_duration_seconds",
			Help:      "Seconds one JMAP method call took, by method name; unknown or undeclared methods are labelled unknown.",
			Buckets:   jmapDurationBuckets,
		}, []string{"method"}),

		JMAPRequestDuration: factory.NewHistogram(prometheus.HistogramOpts{
			Namespace: namespace,
			Name:      "jmap_request_duration_seconds",
			Help:      "Seconds a whole JMAP request spent running its method calls.",
			Buckets:   jmapDurationBuckets,
		}),

		ProxyRefusalsTotal: factory.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "proxy_refusals_total",
			Help:      "Image-proxy requests that delivered no image, by reason.",
		}, []string{"reason"}),

		LocalDKIMTotal: factory.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "local_dkim_total",
			Help:      "Outcomes of Mailroom's own DKIM check at ingest, by result (includes skipped_trusted_header and not_checked).",
		}, []string{"result"}),

		ProxyFetchesTotal: factory.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "proxy_fetches_total",
			Help:      "Image-proxy requests that delivered an image: served (fetched from the sender) or cached.",
		}, []string{"result"}),

		ProxyPrefetchTotal: factory.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "proxy_prefetch_total",
			Help:      "Images handled by the image prefetch at arrival (fetched, already_cached, refused, empty), and runs skipped at the kept-images ceiling (cap_reached).",
		}, []string{"result"}),

		ProxyCacheBytes: factory.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: namespace,
			Name:      "proxy_cache_bytes",
			Help:      "Bytes in a user's image cache, by kind: referenced (kept while a message is kept) or unreferenced (capped).",
		}, []string{"user", "kind"}),

		ProxyCacheEntries: factory.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: namespace,
			Name:      "proxy_cache_entries",
			Help:      "Images in a user's image cache, by kind: referenced or unreferenced.",
		}, []string{"user", "kind"}),

		OnDemandScanTotal: factory.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "ondemand_scan_total",
			Help:      "Verdicts of the download-time scan of attachments of decrypted messages, by verdict.",
		}, []string{"verdict"}),

		SearchDuration: factory.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: namespace,
			Name:      "search_duration_seconds",
			Help:      "Seconds one search-index read took: kind=search (a query's matches) or kind=snippet (highlighting a page).",
			Buckets:   searchDurationBuckets,
		}, []string{"kind"}),

		LearnedPlacementTotal: factory.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "learned_placement_total",
			Help:      "Learned placement arrival decisions by reason: placed, or why not (closed set; error when the decision failed).",
		}, []string{"reason"}),

		FlagRefreshAbortedTotal: factory.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "flag_refresh_aborted_total",
			Help:      "Flag readings applied by a FETCH command that then failed, by path.",
		}, []string{"path"}),

		FetchFlagsAbsentTotal: factory.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "fetch_flags_absent_total",
			Help:      "Messages whose FETCH response lacked the requested FLAGS item (treated as no reading), by path.",
		}, []string{"path"}),

		SyncRecoveredArrivalsTotal: factory.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "sync_recovered_arrivals_total",
			Help:      "New messages ingested by the gap backfill or the flag-pass body fallback, by path and outcome (notify, muted, blocked).",
		}, []string{"path", "outcome"}),

		FlagSweepMessagesTotal: factory.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "flag_sweep_messages_total",
			Help:      "Messages the CONDSTORE-less flag sweep re-read (result=read) and applied a changed reading for (result=applied).",
		}, []string{"result"}),
	}
}

// Registry returns the private registry every metric is registered on.
func (m *Metrics) Registry() *prometheus.Registry { return m.registry }

// Handler returns the /metrics HTTP handler backed by this Metrics'
// private registry.
func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.registry, promhttp.HandlerOpts{Registry: m.registry})
}
