// SPDX-License-Identifier: AGPL-3.0-only

package sync

import (
	"testing"
	"time"
)

func TestConfigValidate(t *testing.T) {
	base := func() Config {
		return Config{
			Host:     "mail.example.com",
			Port:     993,
			Username: "user@example.com",
			Password: "hunter2",
			TLS:      TLSImplicit,
		}
	}

	tests := []struct {
		name    string
		mutate  func(c Config) Config
		wantErr bool
	}{
		{"valid", func(c Config) Config { return c }, false},
		{"missing host", func(c Config) Config { c.Host = ""; return c }, true},
		{"missing username", func(c Config) Config { c.Username = ""; return c }, true},
		{"missing password", func(c Config) Config { c.Password = ""; return c }, true},
		{"port zero", func(c Config) Config { c.Port = 0; return c }, true},
		{"port negative", func(c Config) Config { c.Port = -1; return c }, true},
		{"port too large", func(c Config) Config { c.Port = 65536; return c }, true},
		{"port max valid", func(c Config) Config { c.Port = 65535; return c }, false},
		{"invalid TLS mode", func(c Config) Config { c.TLS = TLSMode(99); return c }, true},
		{"starttls mode valid", func(c Config) Config { c.TLS = TLSStartTLS; return c }, false},
		{"insecure mode valid", func(c Config) Config { c.TLS = TLSInsecure; return c }, false},
		{"negative dial timeout", func(c Config) Config { c.DialTimeout = -1; return c }, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := tt.mutate(base())
			err := cfg.Validate()
			if tt.wantErr && err == nil {
				t.Errorf("Validate() = nil, want error")
			}
			if !tt.wantErr && err != nil {
				t.Errorf("Validate() = %v, want nil", err)
			}
		})
	}
}

func TestConfigAddr(t *testing.T) {
	cfg := Config{Host: "mail.example.com", Port: 993}
	if got, want := cfg.addr(), "mail.example.com:993"; got != want {
		t.Errorf("addr() = %q, want %q", got, want)
	}
}

func TestConfigTLSServerName(t *testing.T) {
	cfg := Config{Host: "mail.example.com"}
	if got, want := cfg.tlsServerName(), "mail.example.com"; got != want {
		t.Errorf("tlsServerName() = %q, want %q", got, want)
	}

	cfg.ServerName = "override.example.com"
	if got, want := cfg.tlsServerName(), "override.example.com"; got != want {
		t.Errorf("tlsServerName() with override = %q, want %q", got, want)
	}
}

func TestConfigDialTimeout(t *testing.T) {
	cfg := Config{}
	if got := cfg.dialTimeout(); got != defaultDialTimeout {
		t.Errorf("dialTimeout() = %v, want default %v", got, defaultDialTimeout)
	}

	cfg.DialTimeout = 5 * time.Second
	if got := cfg.dialTimeout(); got != 5*time.Second {
		t.Errorf("dialTimeout() = %v, want 5s", got)
	}
}

func TestTLSModeString(t *testing.T) {
	tests := []struct {
		mode TLSMode
		want string
	}{
		{TLSImplicit, "implicit"},
		{TLSStartTLS, "starttls"},
		{TLSInsecure, "insecure"},
	}
	for _, tt := range tests {
		if got := tt.mode.String(); got != tt.want {
			t.Errorf("TLSMode(%d).String() = %q, want %q", tt.mode, got, tt.want)
		}
	}
	if got := TLSMode(42).String(); got == "" {
		t.Error("String() for unknown TLSMode should not be empty")
	}
}
