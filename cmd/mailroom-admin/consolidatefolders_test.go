// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"mailroom/internal/consolidate"
	"mailroom/internal/store"
	"mailroom/internal/sync"
)

// writeCountingSession is a one-folder account that records every write.
type writeCountingSession struct {
	msgs   map[string]uint32
	writes []string
}

func (s *writeCountingSession) ListMailboxes(context.Context) ([]sync.Mailbox, error) {
	var out []sync.Mailbox
	for n := range s.msgs {
		out = append(out, sync.Mailbox{Name: n, Delimiter: '/'})
	}
	return out, nil
}

func (s *writeCountingSession) Status(_ context.Context, n string) (sync.FolderStatus, error) {
	return sync.FolderStatus{Name: n, Messages: s.msgs[n], UIDNext: 99, UIDValidity: 1}, nil
}

func (s *writeCountingSession) MoveAll(_ context.Context, src, dst string) error {
	s.writes = append(s.writes, "move "+src)
	s.msgs[dst] += s.msgs[src]
	s.msgs[src] = 0
	return nil
}

func (s *writeCountingSession) DeleteMailbox(_ context.Context, n string) error {
	s.writes = append(s.writes, "delete "+n)
	delete(s.msgs, n)
	return nil
}

func (s *writeCountingSession) SupportsMove() bool { return true }

// Report-first is the default: without apply, nothing is written — and the
// same inputs with apply do write, so the check is not vacuous.
func TestConsolidateFoldersIsReportOnlyByDefault(t *testing.T) {
	opts := consolidate.Options{From: "INBOX/*", Into: "INBOX", DeleteEmpty: []string{"INBOX/*"}}
	for _, apply := range []bool{false, true} {
		s := &writeCountingSession{msgs: map[string]uint32{"INBOX": 1, "INBOX/a": 2}}
		var out bytes.Buffer
		if err := consolidateFolders(context.Background(), &out, s, nil, opts, apply); err != nil {
			t.Fatalf("apply=%v: %v", apply, err)
		}
		if !strings.Contains(out.String(), "move 2") {
			t.Errorf("apply=%v: report lacks the plan:\n%s", apply, out.String())
		}
		switch {
		case !apply && len(s.writes) != 0:
			t.Errorf("REPORT-ONLY run wrote to the server: %v", s.writes)
		case apply && len(s.writes) != 2:
			t.Errorf("--apply wrote %v, want the move and the delete", s.writes)
		}
	}
}

func TestIngestOnlyGate(t *testing.T) {
	live := store.Account{Name: "a", WritebackEnabled: true}
	shadow := store.Account{Name: "a", WritebackEnabled: false}
	for _, tc := range []struct {
		name                               string
		acct                               store.Account
		instanceOff, apply, despite, refus bool
	}{
		{"report on a shadowed account", shadow, false, false, false, false},
		{"apply on a live account", live, false, true, false, false},
		{"apply on a shadowed account", shadow, false, true, false, true},
		{"apply with the instance switch off", live, true, true, false, true},
		{"apply on a shadowed account, despite", shadow, true, true, true, false},
	} {
		err := ingestOnlyGate(tc.acct, tc.instanceOff, tc.apply, tc.despite)
		if (err != nil) != tc.refus {
			t.Errorf("%s: err = %v, want refusal %v", tc.name, err, tc.refus)
		}
	}
}
