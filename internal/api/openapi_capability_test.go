// SPDX-License-Identifier: AGPL-3.0-only

package api

// TestCapabilityDocMatchesCode pins docs/openapi.yaml's Capabilities schema
// against capabilityObjects() in capability.go, in BOTH directions (Forgejo
// #408). Before this test, five URNs the server advertised — unsubscribe,
// remotecontent, composesnippets, savedsearches, categorytabs — were simply
// absent from the doc's Capabilities properties, silently, because nothing
// checked the two against each other. That is exactly the kind of drift the
// file's own header already warns can happen ("HAND-AUTHORED... deliberately
// conservative" is a promise, not an enforced property).
//
// MEMBERSHIP ONLY. This test must never grow into asserting anything about a
// description's WORDING — that would turn writing a good comment into a
// reason to fight the test, which is how comments get worse rather than
// better. It only ever compares two sets of URN strings.

import (
	"bufio"
	"fmt"
	"os"
	"strings"
	"testing"
)

// openapiPath is relative to this package's directory (internal/api), which
// is where `go test` sets the working directory regardless of where it was
// invoked from.
const openapiPath = "../../docs/openapi.yaml"

// capabilitiesDeliberatelyUndocumented lists URNs that capabilityObjects()
// advertises but which docs/openapi.yaml intentionally omits from the
// Capabilities schema, with the reason why. Forgejo #408 asked that such an
// exception be visible and have to be justified HERE, in code, rather than
// silently swallowed by the comparison below.
//
// EMPTY TODAY: every URN capabilityObjects() advertises is documented, and
// that is the state this test enforces. If a future capability genuinely
// needs to ship undocumented for a stated reason, name it here — do not
// special-case it inline in TestCapabilityDocMatchesCode.
var capabilitiesDeliberatelyUndocumented = map[string]string{}

func TestCapabilityDocMatchesCode(t *testing.T) {
	documented, err := capabilityDocKeys(openapiPath)
	if err != nil {
		t.Fatalf("reading %s: %v", openapiPath, err)
	}

	code := capabilityObjects()

	// Direction 1: the code advertises a URN the doc never mentions.
	for urn := range code {
		if documented[urn] {
			continue
		}
		if reason, exempt := capabilitiesDeliberatelyUndocumented[urn]; exempt {
			t.Logf("%q is advertised by capabilityObjects() but deliberately undocumented: %s", urn, reason)
			continue
		}
		t.Errorf("%q is advertised by capabilityObjects() (internal/api/capability.go) but "+
			"docs/openapi.yaml's Capabilities schema does not list it — a client reading only "+
			"the OpenAPI doc would never learn this capability exists (add it, or add it to "+
			"capabilitiesDeliberatelyUndocumented with a reason)", urn)
	}

	// Direction 2 (the dual): the doc documents a URN the code no longer
	// advertises. One direction alone would let the doc list a capability
	// that was renamed or removed and never notice.
	for urn := range documented {
		if _, ok := code[urn]; ok {
			continue
		}
		t.Errorf("docs/openapi.yaml documents %q under Capabilities, but "+
			"capabilityObjects() (internal/api/capability.go) no longer advertises it — "+
			"the doc describes a capability that does not exist", urn)
	}
}

// capabilityDocKeys reads docs/openapi.yaml and returns the set of property
// keys declared directly under the Capabilities schema's `properties:` map —
// i.e. every URN the doc claims the server advertises.
//
// go.mod carries no YAML library, and this is not the place to add one to read
// one map of scalar keys out of one file — so this is a NARROW SCANNER, not a
// parser. It assumes the following shape, true of docs/openapi.yaml as of
// Forgejo #408 and asserted nowhere else:
//
//  1. A line indented EXACTLY 4 spaces, reading exactly "Capabilities:",
//     opens the schema.
//  2. Somewhere after that (inside the schema's own `type`/`description`
//     keys), a line indented EXACTLY 6 spaces reads exactly "properties:".
//  3. Every line indented EXACTLY 8 spaces from there until the schema ends
//     (the first line at indent <= 6 that is not blank) is one property
//     entry: either `key: value` on one line (typically a $ref) or a bare
//     `key:` opening a nested block (typically `type:`/`description:`).
//  4. Anything indented MORE than 8 spaces — description prose, a nested
//     `properties:`/`items:` block belonging to ONE capability's own inline
//     schema — is a continuation of the property immediately above it and is
//     ignored.
//
// THE KEY MAY ITSELF CONTAIN COLONS, because every key here is a URN (e.g.
// urn:ietf:params:jmap:core). Splitting on the first ':' would truncate it to
// "urn", so this scanner instead splits on ": " (colon THEN space) — the same
// rule YAML itself uses to tell a plain-scalar key from the rest of its own
// text — and falls back to "the whole line minus one trailing colon" for a
// bare `key:` with nothing after it on the line.
//
// This is deliberately NOT robust to the schema being reshaped: a capability
// key containing ": ", the doc being reordered so `properties:` stops being
// the first one found, or the Capabilities schema disappearing entirely all
// make this function return an error rather than silently an incomplete (or
// empty) set — a wrong answer here must be loud, not a quietly-passing test.
func capabilityDocKeys(path string) (map[string]bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()

	const (
		schemaLine     = "Capabilities:"
		schemaIndent   = 4
		propertiesLine = "properties:"
		propsIndent    = 6
		keyIndent      = 8
	)

	// state machine: seekingSchema -> seekingProperties -> inProperties -> done
	state := "seekingSchema"
	keys := map[string]bool{}

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		content := strings.TrimRight(strings.TrimLeft(line, " "), " \t")
		indent := len(line) - len(strings.TrimLeft(line, " "))

		switch state {
		case "seekingSchema":
			if indent == schemaIndent && content == schemaLine {
				state = "seekingProperties"
			}

		case "seekingProperties":
			if content == "" {
				continue
			}
			if indent <= schemaIndent {
				return nil, fmt.Errorf(
					"capabilityDocKeys: hit %q (indent %d) before finding a %q line under "+
						"Capabilities — the schema's shape changed and this scanner's assumptions are stale",
					content, indent, propertiesLine)
			}
			if indent == propsIndent && content == propertiesLine {
				state = "inProperties"
			}

		case "inProperties":
			if content == "" {
				continue // a blank line inside one capability's description block
			}
			if indent < keyIndent {
				state = "done"
				continue
			}
			if indent > keyIndent {
				continue // continuation of the property entry above
			}
			key, ok := parseYAMLPlainKey(content)
			if !ok {
				return nil, fmt.Errorf(
					"capabilityDocKeys: line %q at the property indent does not look like a YAML key", content)
			}
			keys[key] = true

		case "done":
			// Nothing left to do; keep draining the scanner to EOF.
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	switch state {
	case "seekingSchema":
		return nil, fmt.Errorf("capabilityDocKeys: never found a %q line", schemaLine)
	case "seekingProperties":
		return nil, fmt.Errorf("capabilityDocKeys: found Capabilities: but never found its %q line", propertiesLine)
	}
	if len(keys) == 0 {
		return nil, fmt.Errorf("capabilityDocKeys: found the properties block but read zero keys from it")
	}
	return keys, nil
}

// parseYAMLPlainKey extracts the key from one `key: value` or bare `key:`
// line. See capabilityDocKeys for why ": " (colon-space), not the first ':',
// is the separator this looks for.
func parseYAMLPlainKey(content string) (string, bool) {
	if idx := strings.Index(content, ": "); idx >= 0 {
		return content[:idx], true
	}
	if strings.HasSuffix(content, ":") {
		return content[:len(content)-1], true
	}
	return "", false
}
