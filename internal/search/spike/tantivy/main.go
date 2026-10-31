// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"flag"
	"fmt"
	"os"
	"sort"
	"time"
)

func main() {
	n := flag.Int("n", 500, "number of synthetic documents to index")
	dir := flag.String("dir", "/tmp/mailroom-spike-index", "index directory")
	iters := flag.Int("iters", 300, "latency iterations per query")
	flag.Parse()

	if err := run(*n, *dir, *iters); err != nil {
		fmt.Fprintln(os.Stderr, "SPIKE FAILED:", err)
		os.Exit(1)
	}
}

func run(n int, dir string, iters int) error {
	fmt.Printf("=== tantivy-go spike: n=%d dir=%s iters=%d ===\n", n, dir, iters)

	be, err := newTantivyBackend(dir)
	if err != nil {
		return fmt.Errorf("open backend: %w", err)
	}
	defer be.Close()

	docs, exp := genCorpus(n)

	// --- Index (measure throughput) ---
	t0 := time.Now()
	const batch = 1000
	for i := 0; i < len(docs); i += batch {
		end := i + batch
		if end > len(docs) {
			end = len(docs)
		}
		if err := be.Add(docs[i:end]...); err != nil {
			return fmt.Errorf("add batch %d: %w", i, err)
		}
	}
	indexDur := time.Since(t0)

	got, err := be.NumDocs()
	if err != nil {
		return err
	}
	fmt.Printf("indexed %d docs in %s (%.0f docs/s); NumDocs=%d\n",
		len(docs), indexDur.Round(time.Millisecond),
		float64(len(docs))/indexDur.Seconds(), got)
	if int(got) != n {
		return fmt.Errorf("NumDocs mismatch: got %d want %d", got, n)
	}

	// --- Correctness ---
	fmt.Println("\n--- correctness ---")
	ok := true
	ok = check("Term(body,zorbex)", exp.termZorbex, func() ([]Hit, error) { return be.Term(fBody, "zorbex") }) && ok
	ok = check("Prefix(body,zorb)", exp.prefixZorb, func() ([]Hit, error) { return be.Prefix(fBody, "zorb") }) && ok
	ok = check("Phrase(subject,quarterly revenue report)", exp.phraseQRR,
		func() ([]Hit, error) { return be.Phrase(fSubject, "quarterly", "revenue", "report") }) && ok
	ok = check("Bool(body, +invoice -spam)", exp.boolInvNotSpam,
		func() ([]Hit, error) { return be.Bool(fBody, []string{"invoice"}, []string{"spam"}) }) && ok
	ok = check("FieldEquals(from,alerts@example.com)", exp.fieldAlerts,
		func() ([]Hit, error) { return be.FieldEquals(fFrom, alertsSender) }) && ok

	// Phrase must be strictly less than the bag-of-words match for the same
	// words (proves position awareness, not just term presence).
	revHits, err := be.Term(fSubject, "revenue")
	if err != nil {
		return err
	}
	fmt.Printf("  [info] Term(subject,revenue)=%d vs Phrase=%d (phrase must be < term)\n",
		len(revHits), exp.phraseQRR)
	if len(revHits) <= exp.phraseQRR {
		fmt.Println("  [warn] phrase did not narrow vs bag-of-words; check tokenizer positions")
	}

	// --- Incremental delete ---
	fmt.Println("\n--- incremental delete ---")
	zorbexHits, _ := be.Term(fBody, "zorbex")
	ids := make([]string, len(zorbexHits))
	for i, h := range zorbexHits {
		ids[i] = h.ID
	}
	if err := be.Delete(ids...); err != nil {
		return fmt.Errorf("delete: %w", err)
	}
	after, _ := be.Term(fBody, "zorbex")
	nd, _ := be.NumDocs()
	fmt.Printf("  deleted %d zorbex docs; Term(body,zorbex) now=%d; NumDocs=%d\n", len(ids), len(after), nd)
	ok = expect("delete removes hits", 0, len(after)) && ok
	ok = expect("delete lowers NumDocs", n-len(ids), int(nd)) && ok

	// --- Latency (p50/p95/p99) ---
	// Production top-K (fetch first page, limit=50) — the number the p95<100ms
	// target actually applies to.
	fmt.Println("\n--- latency: production top-K (limit=50) ---")
	measure("Term(body,invoice)", iters, func() error { return be.benchTerm(fBody, "invoice") })
	measure("Phrase(subject,quarterly revenue report)", iters, func() error {
		return be.benchPhrase(fSubject, "quarterly", "revenue", "report")
	})
	measure("Bool(body,+invoice -spam)", iters, func() error {
		return be.benchBool(fBody, []string{"invoice"}, []string{"spam"})
	})
	measure("Prefix(body,dep)", iters, func() error { return be.benchPrefix(fBody, "dep") })

	// Full materialization (limit=100000 + per-hit JSON) — NOT the target metric;
	// shown to isolate result-marshalling cost from engine cost.
	fmt.Println("\n--- latency: full materialization (all hits, diagnostic) ---")
	measure("Term(body,invoice)", iters, func() error { _, e := be.Term(fBody, "invoice"); return e })
	measure("Prefix(body,dep)", iters, func() error { _, e := be.Prefix(fBody, "dep"); return e })

	fmt.Println()
	if !ok {
		return fmt.Errorf("one or more correctness checks FAILED")
	}
	fmt.Println("ALL CORRECTNESS CHECKS PASSED")
	return nil
}

func check(name string, want int, fn func() ([]Hit, error)) bool {
	hits, err := fn()
	if err != nil {
		fmt.Printf("  [FAIL] %-45s error: %v\n", name, err)
		return false
	}
	return expect(name, want, len(hits))
}

func expect(name string, want, got int) bool {
	status := "ok  "
	pass := want == got
	if !pass {
		status = "FAIL"
	}
	fmt.Printf("  [%s] %-45s got=%d want=%d\n", status, name, got, want)
	return pass
}

func measure(name string, iters int, fn func() error) {
	ds := make([]time.Duration, 0, iters)
	for i := 0; i < iters; i++ {
		t := time.Now()
		if err := fn(); err != nil {
			fmt.Printf("  %-45s error: %v\n", name, err)
			return
		}
		ds = append(ds, time.Since(t))
	}
	sort.Slice(ds, func(i, j int) bool { return ds[i] < ds[j] })
	p := func(q float64) time.Duration { return ds[int(float64(len(ds)-1)*q)] }
	fmt.Printf("  %-45s p50=%-10v p95=%-10v p99=%-10v max=%v\n",
		name, p(0.50).Round(time.Microsecond), p(0.95).Round(time.Microsecond),
		p(0.99).Round(time.Microsecond), ds[len(ds)-1].Round(time.Microsecond))
}
