// SPDX-License-Identifier: AGPL-3.0-only

package search

import (
	"fmt"
	"math"
	"strconv"
	"strings"
)

// Size filtering (`larger:` / `smaller:`, #722) without numeric fields.
//
// The binding has no numeric field and no range query (see daterange.go, which
// has the same problem with dates), so a size is indexed as PREFIX TOKENS and a
// range is matched as the OR of the aligned blocks that cover it exactly — the
// segment-tree decomposition of an interval.
//
// Index time: the size in bytes, clamped to [0, 2^40), contributes one token
// per hexadecimal digit position — its value shifted right by 0, 4, 8 … 36
// bits:
//
//	b00:2a3f1   the exact size (0x2a3f1 = 172,017 bytes)
//	b04:2a3f    the 16-byte block it is in
//	…
//	b36:0       the 64 GiB block it is in
//
// Query time: [lo, hi) is covered greedily by the LARGEST aligned block that
// fits (lo a multiple of 2^s and lo + 2^s ≤ hi), each block being one token.
// The cover is exact to the byte: no over-fetch, no post-filter, no lost recall
// — unlike a fixed set of size buckets, which would round every boundary.
//
// Why base 16 rather than binary or calendar-like units: it trades index size
// against query size. Ten tokens per message (binary would need forty); a
// one-sided range like `larger:10M` costs at most 15 tokens per digit
// position, about 150 in the worst case and typically far fewer — an OR the
// engine evaluates over posting lists with a handful of entries each.
//
// Why not filter sizes in Postgres instead, where size_bytes already is: a size
// term can sit anywhere in the tree (`(larger:5M OR has:pdf) -from:me`), and the
// index answers the whole tree before its hit limit applies. A size condition
// applied afterwards in SQL would cut down an already-capped candidate list —
// `invoice larger:5M` would return the large ones among the 500 best invoice
// matches instead of the 500 best large invoices.

const (
	sizeBits  = 40 // sizes are clamped below 2^40 bytes (1 TiB)
	sizeStep  = 4  // one token per hex digit
	sizeLimit = int64(1) << sizeBits
)

func sizeToken(shift uint, v int64) string { return fmt.Sprintf("b%02d:%x", shift, v>>shift) }

// sizeTokens returns the prefix tokens a document of this size carries.
func sizeTokens(size int64) []string {
	if size < 0 {
		size = 0
	}
	if size >= sizeLimit {
		size = sizeLimit - 1
	}
	out := make([]string, 0, sizeBits/sizeStep)
	for s := uint(0); s < sizeBits; s += sizeStep {
		out = append(out, sizeToken(s, size))
	}
	return out
}

// sizeCover returns the minimal aligned-block tokens covering [lo, hi), or nil
// when the range is empty. Bounds are clamped to [0, 2^40).
func sizeCover(lo, hi int64) []string {
	if lo < 0 {
		lo = 0
	}
	if hi > sizeLimit {
		hi = sizeLimit
	}
	var out []string
	for lo < hi {
		s := uint(sizeBits - sizeStep)
		for s > 0 {
			block := int64(1) << s
			if lo%block == 0 && lo+block <= hi {
				break
			}
			s -= sizeStep
		}
		out = append(out, sizeToken(s, lo))
		lo += int64(1) << s
	}
	return out
}

// sizeRangeNode compiles a SizeRange into a token Or. An empty range compiles
// to None (matches nothing) rather than being dropped.
func sizeRangeNode(r SizeRange) Node {
	hi := r.Max
	if hi <= 0 {
		hi = sizeLimit
	}
	tokens := sizeCover(r.Min, hi)
	if len(tokens) == 0 {
		return None{}
	}
	branches := make([]Node, 0, len(tokens))
	for _, tok := range tokens {
		branches = append(branches, Term{Field: FieldSize, Value: tok, Match: MatchExact, Boost: 1})
	}
	if len(branches) == 1 {
		return branches[0]
	}
	return Or{Nodes: branches}
}

// parseSize parses a size as users write it: a number of bytes, or a number
// with a K, M or G suffix (binary: 1K = 1024 bytes, the unit the web client
// shows sizes in), optionally followed by B and optionally with a fraction —
// 1500, 10K, 2.5M, 1GB.
func parseSize(v string) (int64, bool) {
	v = strings.ToLower(strings.TrimSpace(v))
	v = strings.TrimSuffix(v, "b")
	mult := 1.0
	switch {
	case strings.HasSuffix(v, "k"):
		mult = 1 << 10
	case strings.HasSuffix(v, "m"):
		mult = 1 << 20
	case strings.HasSuffix(v, "g"):
		mult = 1 << 30
	}
	if mult != 1 {
		v = v[:len(v)-1]
	}
	if v == "" || strings.ContainsAny(v, "+-eE") {
		return 0, false
	}
	n, err := strconv.ParseFloat(v, 64)
	if err != nil || math.IsNaN(n) || math.IsInf(n, 0) || n < 0 {
		return 0, false
	}
	b := n * mult
	if b >= float64(sizeLimit) {
		return sizeLimit, true
	}
	return int64(math.Round(b)), true
}

// sizeNode compiles larger:/smaller:. "larger than X" is size > X and "smaller
// than X" is size < X, so the two never overlap and only X itself is in
// neither. An empty value (mid-keystroke) matches nothing without an error, as
// an empty from: does.
func sizeNode(op, raw string) (Node, error) {
	if strings.TrimSpace(raw) == "" {
		return None{}, nil
	}
	n, ok := parseSize(raw)
	if !ok {
		return nil, &SyntaxError{Operator: op, Value: raw,
			Reason: "not a size; use a number of bytes or a number with K, M or G, e.g. 500K or 10M"}
	}
	if op == "larger" {
		return SizeRange{Min: n + 1}, nil
	}
	if n <= 0 {
		return None{}, nil // nothing is smaller than nothing (and Max 0 would mean open-ended)
	}
	return SizeRange{Max: n}, nil
}
