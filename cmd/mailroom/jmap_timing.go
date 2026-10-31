// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"mailroom/internal/api"
	"mailroom/internal/metrics"
)

// slowJMAPRequest is the total at which a request's per-method breakdown is
// logged (#665). Below it the histograms are the only record.
const slowJMAPRequest = time.Second

// jmapTimingObserver is the api.WithRequestTiming observer: it feeds both JMAP
// duration histograms and logs one line for a request at or over
// slowJMAPRequest. A nil m skips the histograms (tests build the handler
// without a registry). It logs counts and names only, never arguments or mail.
func jmapTimingObserver(m *metrics.Metrics, logger *slog.Logger) func(api.RequestTiming) {
	return func(t api.RequestTiming) {
		if m != nil {
			m.JMAPRequestDuration.Observe(t.Total.Seconds())
			for _, c := range t.Methods {
				m.JMAPMethodDuration.WithLabelValues(c.Name).Observe(c.Duration.Seconds())
			}
		}
		if t.Total >= slowJMAPRequest {
			logger.Info("slow jmap request",
				"user_id", t.UserID.String(),
				"total_ms", t.Total.Milliseconds(),
				"calls", len(t.Methods),
				"methods", summariseMethodTimings(t.Methods))
		}
	}
}

// summariseMethodTimings aggregates calls by method name, slowest total first,
// as "Email/query×13=8200ms Email/get×13=410ms". Ties break on name so the line
// is stable.
func summariseMethodTimings(calls []api.MethodTiming) string {
	type agg struct {
		name  string
		n     int
		total time.Duration
	}
	byName := map[string]*agg{}
	var order []*agg
	for _, c := range calls {
		g := byName[c.Name]
		if g == nil {
			g = &agg{name: c.Name}
			byName[c.Name] = g
			order = append(order, g)
		}
		g.n++
		g.total += c.Duration
	}
	sort.SliceStable(order, func(i, j int) bool {
		if order[i].total != order[j].total {
			return order[i].total > order[j].total
		}
		return order[i].name < order[j].name
	})
	parts := make([]string, len(order))
	for i, g := range order {
		parts[i] = fmt.Sprintf("%s×%d=%dms", g.name, g.n, g.total.Milliseconds())
	}
	return strings.Join(parts, " ")
}
