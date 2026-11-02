// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package store

import (
	"context"
	"os"
	"testing"
)

// The normal Open installs no query tracer; only OpenRecording does. The
// positive case proves HasQueryTracer can observe a tracer at all.
func TestOpenInstallsNoTracerButOpenRecordingDoes(t *testing.T) {
	ctx := context.Background()
	dsn := os.Getenv("MAILROOM_TEST_APP_URL")
	plain, err := Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer plain.Close()
	if plain.HasQueryTracer() {
		t.Error("store.Open installed a query tracer")
	}
	rec, _, err := OpenRecording(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer rec.Close()
	if !rec.HasQueryTracer() {
		t.Error("OpenRecording did not install a tracer")
	}
}
