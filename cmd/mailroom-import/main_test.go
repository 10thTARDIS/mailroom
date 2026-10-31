// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"mailroom/internal/config"
)

func clearImportEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{
		config.EnvDatabaseURL, config.EnvDatabaseURL + "_FILE",
		config.EnvDatabasePasswordFile,
		config.EnvMasterKeyFile, config.EnvBlobRoot,
	} {
		t.Setenv(k, "")
	}
}

// TestLoadDatabasePasswordFile pins the #537 path: a plain database URL with
// no password, plus MAILROOM_DATABASE_PASSWORD_FILE, ends up with the file's
// password injected into settings.databaseURL.
func TestLoadDatabasePasswordFile(t *testing.T) {
	clearImportEnv(t)
	t.Setenv(config.EnvDatabaseURL, "postgres://mailroom_app@127.0.0.1:5432/mailroom?sslmode=disable")

	pwPath := filepath.Join(t.TempDir(), "app_password")
	const want = "p@ss/wo:rd%'# x"
	if err := os.WriteFile(pwPath, []byte(want+"\n"), 0o644); err != nil {
		t.Fatalf("os.WriteFile: %v", err)
	}
	t.Setenv(config.EnvDatabasePasswordFile, pwPath)

	s, err := load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if !strings.Contains(s.databaseURL, "mailroom_app:") {
		t.Fatalf("databaseURL = %q, does not look like it carries a password", s.databaseURL)
	}
}

func TestLoadDatabaseURLRequired(t *testing.T) {
	clearImportEnv(t)
	if _, err := load(); err == nil {
		t.Fatal("load() error = nil, want error for missing database URL")
	}
}
