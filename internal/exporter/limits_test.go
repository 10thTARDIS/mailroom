package exporter

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"mailroom/internal/exportplan"
)

// The archive's manifest.json is the only place a person reading this directory
// in three years learns what is NOT in it. Nothing tested that list, and it went
// stale for a whole release wave: it said the private key "is not contained
// here (spec §9 export-wrapped, ADR-024)" — a citation to a design document —
// for the entire period between the export shipping and
// `mailroom-admin pgp-export-wrapped` being built for #54. Every word was true.
// It just told the reader what was missing without telling them how to get it,
// which for the one limit that is PERMANENT AND UNRECOVERABLE is half an answer.
//
// ⚠ WHAT THIS TEST DOES NOT GUARD. It cannot check that the command it names
// still exists: the dispatch table is a switch in package main of
// cmd/mailroom-admin and is not importable from here. Renaming the subcommand
// without editing this string would leave the manifest pointing at nothing and
// this test green. That is a real gap, stated rather than papered over — what
// it does catch is the failure that actually happened, a limit that describes
// an absence and offers the reader no way out of it.
func TestTheUnreadableMailLimitTellsTheReaderHowToGetTheKey(t *testing.T) {
	if len(Limits) == 0 {
		t.Fatal("Limits is empty: the manifest would promise nothing and omit everything")
	}

	var pgp string
	for _, l := range Limits {
		if l == "" {
			t.Error("an empty limit would render as a blank bullet in the archive")
		}
		if strings.Contains(l, "PGP-encrypted mail") {
			pgp = l
		}
	}
	if pgp == "" {
		t.Fatal("no limit mentions PGP-encrypted mail; exported ciphertext that " +
			"nobody is told is ciphertext is the worst version of this feature")
	}

	// The substantive assertion: it names the escape hatch, not just the gap.
	if !strings.Contains(pgp, "pgp-export-wrapped") {
		t.Errorf("the PGP limit does not name the command that retrieves the key.\n"+
			"This is the exact drift that went unnoticed for a release wave.\ngot: %s", pgp)
	}

	// And says the loss is permanent. A reader who thinks the key can be
	// re-derived later has been told the opposite of the truth: for a GENERATED
	// key the private half exists nowhere else, which is why delete-user
	// refuses without --accept-key-loss.
	if !strings.Contains(pgp, "permanently unreadable") {
		t.Errorf("the PGP limit does not say the loss is permanent.\ngot: %s", pgp)
	}
}

// TestNothingImportsTheSidecarsTheManifestSaysAreOneWay pins the claim added for
// #418 item 4: the mail round-trips, the sidecars do not.
//
// That limit is a statement about a DIFFERENT package, which is exactly the kind
// of sentence that goes quietly false. The day someone teaches internal/importer
// to read messages.jsonl or rules.json, the archive's manifest starts telling
// readers to do a database restore for something the tool in their hand can now
// do — the same drift as the PGP limit above, in the opposite direction.
//
// ⚠ THE FILE LIST IS DERIVED FROM exportplan, not typed here. A hardcoded list
// checked against a hardcoded scan proves only that someone typed the same thing
// twice (exportplan.Unhandled says so about its own map). Adding a new sidecar
// to the plan extends this test for free.
func TestNothingImportsTheSidecarsTheManifestSaysAreOneWay(t *testing.T) {
	// The limit must actually be there; otherwise this test guards a promise
	// the archive never makes.
	var claimed bool
	for _, l := range Limits {
		if strings.Contains(l, "THE SIDECARS CANNOT") {
			claimed = true
		}
	}
	if !claimed {
		t.Fatal("no limit states that the sidecars have no importer; either restore it " +
			"or delete this test — a guard for an absent claim is worse than neither")
	}

	sidecarFile := regexp.MustCompile(`[a-z_]+\.jsonl?`)
	sidecars := map[string]bool{}
	for _, r := range exportplan.Rows() {
		for _, f := range sidecarFile.FindAllString(r.Entry.Detail, -1) {
			sidecars[f] = true
		}
	}
	// ⚠ The empty-side trap (#350): a scan for nothing finds nothing and reports
	// a confident pass. If the plan's Detail wording ever stops naming files,
	// fail here rather than silently guarding zero names.
	if len(sidecars) < 5 {
		t.Fatalf("derived only %d sidecar filenames from exportplan (%v) — the plan's "+
			"Detail format changed and this test is scanning for almost nothing",
			len(sidecars), sidecars)
	}

	dirs := []string{"../importer", "../../cmd/mailroom-import"}
	var scanned int
	for _, dir := range dirs {
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatalf("read %s: %v", dir, err)
		}
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") {
				continue
			}
			path := filepath.Join(dir, e.Name())
			src, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read %s: %v", path, err)
			}
			scanned++
			for f := range sidecars {
				if strings.Contains(string(src), f) {
					t.Errorf("%s references %q, but the export archive's manifest.json tells "+
						"its reader that the sidecars have NO importer and that recovering an "+
						"instance means a database restore. One of the two is now wrong: either "+
						"this reference is not an import path (narrow it), or the limit in "+
						"exporter.Limits needs rewriting.", path, f)
				}
			}
		}
	}
	if scanned == 0 {
		t.Fatal("scanned ZERO Go files: the import side moved and this test proves nothing")
	}
	t.Logf("scanned %d Go files across %v for %d sidecar filenames", scanned, dirs, len(sidecars))
}
