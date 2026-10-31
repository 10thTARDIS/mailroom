// SPDX-License-Identifier: AGPL-3.0-only

package config

import (
	"strings"
	"testing"
)

func TestParseTrustedProxies(t *testing.T) {
	ok := []struct{ in, want string }{
		{"", ""},
		{"  ", ""},
		{",,", ""},
		{"10.0.0.0/8", "10.0.0.0/8"},
		{"10.1.2.3/8", "10.0.0.0/8"}, // masked
		{"172.18.0.5", "172.18.0.5/32"},
		{"fd00::1", "fd00::1/128"},
		{"::ffff:10.0.0.1", "10.0.0.1/32"},
		{" 10.0.0.0/8 , 192.0.2.1 ,fd00::/8", "10.0.0.0/8 192.0.2.1/32 fd00::/8"},
	}
	for _, tc := range ok {
		got, err := ParseTrustedProxies(tc.in)
		if err != nil {
			t.Errorf("ParseTrustedProxies(%q): %v", tc.in, err)
			continue
		}
		var ss []string
		for _, p := range got {
			ss = append(ss, p.String())
		}
		if strings.Join(ss, " ") != tc.want {
			t.Errorf("ParseTrustedProxies(%q) = %v, want %q", tc.in, ss, tc.want)
		}
	}
	for _, bad := range []string{"proxy.example.com", "10.0.0.0/33", "10.0.0.1:8080", "10.0.0.0/8, nope", "fe80::1%eth0", "*"} {
		_, err := ParseTrustedProxies(bad)
		if err == nil || !strings.Contains(err.Error(), EnvTrustedProxies) {
			t.Errorf("ParseTrustedProxies(%q) error = %v, want one naming %s", bad, err, EnvTrustedProxies)
		}
	}
}

// A malformed entry stops startup rather than being dropped, and an unset or
// empty variable (what Compose passes) trusts nobody.
func TestLoadTrustedProxies(t *testing.T) {
	base := func(t *testing.T) {
		t.Helper()
		clearAuthEnv(t)
		t.Setenv(EnvDatabaseURL, "postgres://mailroom_app@localhost/mailroom")
		t.Setenv(EnvDevAuthSubject, "dev-operator")
		t.Setenv(EnvDevAuthAck, DevAuthAckPhrase)
	}
	base(t)
	t.Setenv(EnvTrustedProxies, "")
	cfg, err := Load()
	if err != nil || len(cfg.TrustedProxies) != 0 {
		t.Fatalf("empty: err=%v proxies=%v, want none", err, cfg.TrustedProxies)
	}
	base(t)
	t.Setenv(EnvTrustedProxies, "10.0.0.0/8,192.0.2.7")
	cfg, err = Load()
	if err != nil || len(cfg.TrustedProxies) != 2 {
		t.Fatalf("two entries: err=%v proxies=%v", err, cfg.TrustedProxies)
	}
	base(t)
	t.Setenv(EnvTrustedProxies, "10.0.0.0/8,oops")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), EnvTrustedProxies) {
		t.Fatalf("a malformed entry: err=%v, want a startup error naming %s", err, EnvTrustedProxies)
	}
}
