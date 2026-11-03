// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

func TestMedianDuration(t *testing.T) {
	ms := time.Millisecond
	for _, c := range []struct {
		in   []time.Duration
		want time.Duration
	}{
		{nil, 0},
		{[]time.Duration{7 * ms}, 7 * ms},
		{[]time.Duration{9 * ms, 1 * ms, 5 * ms}, 5 * ms},
		{[]time.Duration{4 * ms, 10 * ms, 2 * ms, 8 * ms}, 6 * ms},
	} {
		if got := medianDuration(c.in); got != c.want {
			t.Errorf("median(%v) = %v, want %v", c.in, got, c.want)
		}
	}
	in := []time.Duration{3 * ms, 1 * ms}
	medianDuration(in)
	if in[0] != 3*ms {
		t.Error("medianDuration reordered its input")
	}
}

func TestPlanVerdictThresholds(t *testing.T) {
	ms := time.Millisecond
	for _, c := range []struct {
		name            string
		generic, custom time.Duration
		want            bool
	}{
		{"both needed, met", 100 * ms, 10 * ms, true},
		{"exactly 2x and exactly 5ms slower", 10 * ms, 5 * ms, true},
		{"ratio met, gap under 5ms", 8 * ms, 4 * ms, false},
		{"gap met, ratio under 2x", 120 * ms, 100 * ms, false},
		{"equal", 50 * ms, 50 * ms, false},
		{"generic faster", 5 * ms, 50 * ms, false},
		{"custom zero, generic large", 20 * ms, 0, true},
	} {
		if got, _ := planVerdict(c.generic, c.custom); got != c.want {
			t.Errorf("%s: candidate(%v, %v) = %v, want %v", c.name, c.generic, c.custom, got, c.want)
		}
	}
	if _, r := planVerdict(30*ms, 10*ms); r != 3 {
		t.Errorf("ratio = %v, want 3", r)
	}
}

func TestPrintPlanCheck(t *testing.T) {
	ms := time.Millisecond
	var b bytes.Buffer
	printPlanCheck(&b, []planPathResult{
		{Name: "slow path", Generic: []time.Duration{90 * ms, 100 * ms, 110 * ms}, Custom: []time.Duration{9 * ms, 10 * ms, 11 * ms}, Rows: 4, Source: "stored"},
		{Name: "same path", Generic: []time.Duration{10 * ms}, Custom: []time.Duration{10 * ms}, Rows: 1, Source: "resolver"},
		{Name: "missing path", Skipped: "no data"},
		// Plans its own statements: a gap is noise, never a candidate.
		{Name: planEmailPath, Generic: []time.Duration{300 * ms}, Custom: []time.Duration{30 * ms}, Rows: 50, Source: "stored"},
	}, 3)
	out := b.String()
	for _, want := range []string{
		"slow path", "generic plan is slower by 10.0x - candidate for planForValues",
		"no plan-mode effect",
		"not exercised: no data",
		"slow path                                  stored\n",
		"same path                                  resolver\n",
		planEmailPath + "      plans its own statements (custom), so the modes do not apply\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
	if strings.Count(out, "candidate for planForValues") != 1 {
		t.Errorf("only the slow path is a candidate:\n%s", out)
	}
	if !strings.HasSuffix(out, "READ ONLY: nothing was written (rolled back).\n") {
		t.Error("output must end with the read-only line")
	}
}

func TestPlanExecTime(t *testing.T) {
	plan := "Seq Scan on t (actual time=0.01..0.02 rows=1 loops=1)\nPlanning Time: 0.1 ms\nExecution Time: 12.500 ms\n"
	if got := planExecTime(plan); got != 12500*time.Microsecond {
		t.Errorf("planExecTime = %v, want 12.5ms", got)
	}
	if got := planExecTime("no timing"); got != 0 {
		t.Errorf("planExecTime without a line = %v, want 0", got)
	}
}
