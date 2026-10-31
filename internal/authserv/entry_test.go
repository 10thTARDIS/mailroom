// SPDX-License-Identifier: AGPL-3.0-only

package authserv

import (
	"testing"
	"time"
)

func TestParse(t *testing.T) {
	d := func(s string) time.Time { v, _ := time.Parse("2006-01-02", s); return v }
	cases := []struct {
		in      string
		want    Entry
		ok      bool
		wantErr bool
	}{
		{"mx.provider.example", Entry{ID: "mx.provider.example"}, true, false},
		{"  MX.Provider.Example  ", Entry{ID: "mx.provider.example"}, true, false},
		{"old.provider.example<2026-07-01", Entry{ID: "old.provider.example", Bound: d("2026-07-01"), Bounded: true}, true, false},
		{" Old.Provider.Example < 2026-07-01 ", Entry{ID: "old.provider.example", Bound: d("2026-07-01"), Bounded: true}, true, false},
		{"", Entry{}, false, false},
		{"   ", Entry{}, false, false},
		{"<2026-07-01", Entry{}, false, true},
		{"old.provider.example<", Entry{}, false, true},
		{"old.provider.example<2026-13-01", Entry{}, false, true},
		{"old.provider.example<2026-7-1", Entry{}, false, true},
		{"old.provider.example<yesterday", Entry{}, false, true},
		{"old.provider.example<2026-07-01<2026-08-01", Entry{}, false, true},
	}
	for _, c := range cases {
		got, ok, err := Parse(c.in)
		if (err != nil) != c.wantErr || ok != c.ok || got != c.want {
			t.Errorf("Parse(%q) = %+v, %v, %v; want %+v, %v, err=%v", c.in, got, ok, err, c.want, c.ok, c.wantErr)
		}
	}
}
