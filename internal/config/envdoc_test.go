// SPDX-License-Identifier: AGPL-3.0-only

package config

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// internalOnlyEnv lists MAILROOM_* constants this package defines that are
// deliberately NOT expected to appear in deploy/compose.yml or
// deploy/.env.example, with the reason each is exempt. It is empty today —
// every MAILROOM_* constant internal/config currently defines turned out, on
// inspection, to be something an operator of the shipped deployment should
// be able to set (#543) — but the mechanism exists so a genuinely dev/test-
// only switch added later has somewhere to be listed instead of silently
// failing TestEveryMailroomEnvVarIsDocumented forever. Verify a candidate's
// actual purpose in code before adding it here; do not add one just to make
// a failure go away.
var internalOnlyEnv = map[string]string{}

// mailroomEnvTokenPattern extracts whole MAILROOM_* tokens from YAML/shell
// text. "Whole" matters: without the trailing \b, MAILROOM_IMAP_IDLE would
// read as "documented" merely because MAILROOM_IMAP_IDLE_TIMEOUT appears
// somewhere in the file — a plain substring search gets that wrong.
var mailroomEnvTokenPattern = regexp.MustCompile(`\bMAILROOM_[A-Za-z0-9_]*\b`)

// TestEveryMailroomEnvVarIsDocumented parses this package's own Go source
// with go/parser + go/ast — not grep, deliberately: a `//` comment that
// happens to quote an env var name must not count as documentation, and one
// already does, in this very file's neighbour (config.go's comment on the
// "MAILROOM_DATABASE_URL (or _FILE) is required" problem string). It finds
// every MAILROOM_* environment variable this package defines as a string
// constant, then checks that each one is settable from deploy/.env.example:
// it appears as a whole token in deploy/compose.yml or deploy/.env.example,
// either under its own name or with the "_FILE" Docker-secrets-indirection
// suffix (fileSuffix) — MAILROOM_SESSION_KEY, for example, is only ever
// written as MAILROOM_SESSION_KEY_FILE in the shipped deployment, and that
// counts.
//
// This is #543's guard. The gap it closes (15+ MAILROOM_* vars readable by
// the code but unreachable from deploy/.env.example, discovered by hand) had
// nothing stopping it from reopening the moment the next constant was added
// to this package without a matching deploy/ entry. Now something does.
func TestEveryMailroomEnvVarIsDocumented(t *testing.T) {
	names := mailroomEnvConstants(t)
	if len(names) == 0 {
		t.Fatal("found zero MAILROOM_* constants in internal/config — the AST walk itself is broken, " +
			"not the thing it is supposed to check")
	}

	composeTokens := mailroomTokens(t, "../../deploy/compose.yml")
	envExampleTokens := mailroomTokens(t, "../../deploy/.env.example")

	for _, name := range names {
		documented := composeTokens[name] || envExampleTokens[name] ||
			composeTokens[name+fileSuffix] || envExampleTokens[name+fileSuffix]
		reason, internal := internalOnlyEnv[name]

		switch {
		case internal && documented:
			t.Errorf("%s is listed in internalOnlyEnv (reason: %q) but ALSO appears in "+
				"deploy/compose.yml or deploy/.env.example — that exception is stale now that it "+
				"is documented. Remove it from internalOnlyEnv in internal/config/envdoc_test.go.",
				name, reason)
		case internal:
			// Deliberately excluded, and correctly not (yet) documented.
		case !documented:
			t.Errorf("%s is a MAILROOM_* setting internal/config defines, but it appears in "+
				"neither deploy/compose.yml nor deploy/.env.example (as itself or as %s%s). Add it "+
				"to the mailroom (or crypto) service's environment: block in deploy/compose.yml as "+
				"${%s:-} (#543 decision: no baked-in default there) and document its real default "+
				"in deploy/.env.example — or, only if it genuinely should not be operator-settable, "+
				"add it to internalOnlyEnv in internal/config/envdoc_test.go with a reason.",
				name, name, fileSuffix, name)
		}
	}
}

// mailroomEnvConstants walks this package's Go source (every non-test .go
// file in the directory) and returns every distinct string constant value
// that starts with "MAILROOM_", sorted.
//
// Deliberately go/parser.ParseFile per file rather than the more convenient
// parser.ParseDir: ParseDir is deprecated (since Go 1.25) precisely because
// it does not consider build tags when associating files with packages,
// which is exactly the kind of silent gap #543 exists to stop making.
func mailroomEnvConstants(t *testing.T) []string {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read internal/config: %v", err)
	}

	fset := token.NewFileSet()
	seen := map[string]bool{}
	var names []string
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		for _, decl := range file.Decls {
			gd, ok := decl.(*ast.GenDecl)
			if !ok || gd.Tok != token.CONST {
				continue
			}
			for _, spec := range gd.Specs {
				vs, ok := spec.(*ast.ValueSpec)
				if !ok {
					continue
				}
				for _, val := range vs.Values {
					lit, ok := val.(*ast.BasicLit)
					if !ok || lit.Kind != token.STRING {
						continue
					}
					v, err := strconv.Unquote(lit.Value)
					if err != nil || !strings.HasPrefix(v, "MAILROOM_") {
						continue
					}
					if !seen[v] {
						seen[v] = true
						names = append(names, v)
					}
				}
			}
		}
	}
	sort.Strings(names)
	return names
}

// mailroomTokens reads path and returns the set of whole MAILROOM_* tokens
// it contains.
func mailroomTokens(t *testing.T, path string) map[string]bool {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	out := map[string]bool{}
	for _, tok := range mailroomEnvTokenPattern.FindAllString(string(b), -1) {
		out[tok] = true
	}
	return out
}

// TestLoadTreatsEmptyPassthroughVarsAsUnset covers every tunable #543 added
// to deploy/compose.yml's mailroom service as ${VAR:-} (empty default, no
// baked-in default value — see the decision in envdoc_test.go's package
// comment above). When deploy/.env leaves one of these unset, Compose does
// NOT omit the variable from the container's environment: with ":-" and an
// empty default, it sets the variable to the EMPTY STRING. Passing every
// setting through this way is only safe if every parser below treats ""
// exactly like "not set" — proven here by actually loading both ways and
// comparing, not assumed from reading the parsing code once.
//
// MAILROOM_AUTHSERV_IDS is deliberately NOT in this list: it is read
// directly by cmd/mailroom (os.Getenv, never through internal/config.Load()
// or the Config struct), so there is no Config field for this test to
// compare. Its own empty-vs-unset handling — strings.Split on "," then
// dropping blank parts — was checked by reading cmd/mailroom/main.go's
// trustedAuthservIDs: splitting "" yields one empty element, which the
// blank-part filter drops, leaving zero trusted ids either way.
func TestLoadTreatsEmptyPassthroughVarsAsUnset(t *testing.T) {
	passthroughVars := []string{
		EnvSyncMaxBackoff, EnvIMAPIdle, EnvIMAPIdleTimeout, EnvIMAPPool, EnvIMAPPoolIdleTimeout,
		EnvImageProxy, EnvTrashRetention, EnvPurgeInterval, EnvChangeLogRetention, EnvClamdMaxBytes,
		EnvIndexInterval, EnvSearchDisabled, EnvWritebackDisabled, EnvAllowPrivateAccountHosts,
		EnvUndoSendWindow, EnvRateLimitPerSecond, EnvRateLimitBurst, EnvWKDRateLimitPerSecond,
		EnvWKDRateLimitBurst, EnvImageProxyPrefetch, EnvImageProxyPrefetchMaxAge, EnvImageProxyPrefetchMaxBytes,
		EnvTrustedProxies, EnvJobRetention, EnvDeadJobRetention,
	}

	base := func(t *testing.T) {
		t.Helper()
		clearAuthEnv(t)
		t.Setenv(EnvDatabaseURL, "postgres://mailroom_app@localhost/mailroom")
		t.Setenv(EnvDevAuthSubject, "dev-operator")
		t.Setenv(EnvDevAuthAck, DevAuthAckPhrase)
	}

	base(t)
	unset, err := Load()
	if err != nil {
		t.Fatalf("Load() with every passthrough var unset: %v", err)
	}

	base(t)
	for _, v := range passthroughVars {
		t.Setenv(v, "")
	}
	empty, err := Load()
	if err != nil {
		t.Fatalf("Load() with every passthrough var explicitly set to \"\" (what Compose actually "+
			"produces in the container when deploy/.env leaves them unset): %v", err)
	}

	if !reflect.DeepEqual(unset, empty) {
		t.Errorf("Load() differs between vars left unset and the same vars explicitly set to \"\", "+
			"which Compose treats as identical: at least one parser is reading \"\" as something "+
			"other than the default.\nunset:  %+v\nempty:  %+v", unset, empty)
	}
}
