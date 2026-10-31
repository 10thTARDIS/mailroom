// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"fmt"
	"math/rand"
)

// Synthetic email-like corpus. Fully self-contained and deterministic (fixed
// RNG seed) and uses only example.com addresses. Rare marker tokens are
// planted on fixed index strides so every query below has an exactly known
// expected hit count — that is how correctness is checked, independent of N.

var wordPool = []string{
	"meeting", "schedule", "update", "project", "deadline", "budget", "review",
	"team", "client", "release", "feature", "bug", "ticket", "server", "deploy",
	"morning", "afternoon", "please", "thanks", "regards", "attached", "document",
	"proposal", "contract", "signature", "payment", "vendor", "shipment", "order",
	"account", "password", "reset", "notice", "reminder", "follow", "up", "call",
	"quarter", "growth", "metric", "dashboard", "roadmap", "sprint", "backlog",
	"design", "prototype", "feedback", "approval", "launch", "campaign", "newsletter",
}

var senders = []string{
	"alice@example.com", "bob@example.com", "carol@example.com",
	"dave@example.com", "notifications@example.com",
}

const alertsSender = "alerts@example.com"

// expectations holds the exact hit count each query must return.
type expectations struct {
	n              int
	termZorbex     int // Term(body,"zorbex")
	prefixZorb     int // Prefix(body,"zorb")  == termZorbex
	phraseQRR      int // Phrase(subject,"quarterly","revenue","report")
	boolInvNotSpam int // Bool(body, must[invoice], mustNot[spam])
	fieldAlerts    int // FieldEquals(from, alertsSender)
}

func genCorpus(n int) ([]Doc, expectations) {
	rng := rand.New(rand.NewSource(42)) // #nosec G404 - synthetic test data only
	docs := make([]Doc, 0, n)
	var exp expectations
	exp.n = n

	for i := 0; i < n; i++ {
		subject := randWords(rng, 4, 8)
		body := randWords(rng, 30, 70)
		from := senders[rng.Intn(len(senders))]

		// Rare body marker "zorbex" every 50 docs (does not stem).
		if i%50 == 0 {
			body = "zorbex " + body
			exp.termZorbex++
			exp.prefixZorb++
		}
		// Boolean setup: "invoice" every 20, "spam" every 40 (spam ⊂ invoice).
		if i%20 == 0 {
			body += " invoice"
		}
		if i%40 == 0 {
			body += " spam"
		}
		if i%20 == 0 && i%40 != 0 {
			exp.boolInvNotSpam++ // invoice present, spam absent
		}
		// Phrase: adjacent "quarterly revenue report" every 100 docs...
		if i%100 == 0 {
			subject = "quarterly revenue report " + subject
			exp.phraseQRR++
		}
		// ...and the SAME three words scattered (non-adjacent) every 100 at
		// offset 50, which a phrase query MUST NOT match.
		if i%100 == 50 {
			subject = "revenue for the report of this quarterly " + subject
		}
		// Exact-field sender marker every 64 docs.
		if i%64 == 0 {
			from = alertsSender
			exp.fieldAlerts++
		}

		docs = append(docs, Doc{
			ID:      fmt.Sprintf("MSG%08d", i),
			From:    from,
			To:      "you@example.com",
			Subject: subject,
			Body:    body,
		})
	}
	return docs, exp
}

func randWords(rng *rand.Rand, min, max int) string {
	count := min + rng.Intn(max-min+1)
	out := make([]byte, 0, count*7)
	for i := 0; i < count; i++ {
		if i > 0 {
			out = append(out, ' ')
		}
		out = append(out, wordPool[rng.Intn(len(wordPool))]...)
	}
	return string(out)
}
