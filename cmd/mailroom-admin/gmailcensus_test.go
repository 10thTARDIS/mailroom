// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"mailroom/internal/gmailsync"
	"mailroom/internal/sync"
	"mailroom/internal/sync/gmailtest"
)

type nopCensusStore struct{}

func (nopCensusStore) ExistingLabels(context.Context, []string) (map[string]bool, error) {
	return map[string]bool{}, nil
}
func (nopCensusStore) CountMessageIDsElsewhere(context.Context, []string) (int, error) {
	return 0, nil
}

// The command's seam: dial, census, print — over the real sync.Dial against
// the scripted fake Gmail. The census's numbers are internal/gmailsync's
// tests; this proves the command reaches them and reads no mail.
func TestGmailCensusCommandSeam(t *testing.T) {
	g := gmailtest.New(t)
	g.StandardMailboxes()
	g.Append("[Gmail]/All Mail", &gmailtest.Message{Body: []byte("Subject: a\r\n\r\nhello"), Labels: []string{`\Inbox`, "Work"}})
	var out bytes.Buffer
	if err := gmailCensus(context.Background(), &out, sync.Dial, g.Listen(sync.TLSInsecure), nopCensusStore{},
		gmailsync.CensusOptions{DailyBudgetBytes: 1000}); err != nil {
		t.Fatalf("census: %v", err)
	}
	if !strings.Contains(out.String(), "READ-ONLY") || !strings.Contains(out.String(), "messages=1") {
		t.Errorf("output:\n%s", out.String())
	}
	if b, n := g.BodyBytesServed(); b != 0 || n != 0 {
		t.Errorf("the census command downloaded %d bodies", n)
	}
}
