// SPDX-License-Identifier: AGPL-3.0-only

package api

import (
	"encoding/json"
	"fmt"
	"strings"
)

// evalPointer evaluates a JMAP result-reference path (RFC 8620 §3.7) against a
// prior method response's arguments. The path is an RFC 6901 JSON Pointer with
// JMAP's extension: a `*` token maps the remainder of the path over every
// element of an array, flattening one level, and the final result MUST be an
// array (the referencing argument is always a list — e.g. Email/query's
// `/ids`, or `/list/*/id` over an Email/get result).
//
// Supported forms cover the common back-references clients actually send; an
// unsupported shape returns an error the caller maps to invalidResultReference.
func evalPointer(doc json.RawMessage, path string) (json.RawMessage, error) {
	if path == "" || path[0] != '/' {
		return nil, fmt.Errorf("api: result reference path %q must start with '/'", path)
	}
	tokens := strings.Split(path[1:], "/")
	return evalTokens(doc, tokens)
}

func evalTokens(doc json.RawMessage, tokens []string) (json.RawMessage, error) {
	if len(tokens) == 0 {
		return doc, nil
	}
	tok := unescapePointerToken(tokens[0])
	rest := tokens[1:]

	if tok == "*" {
		var arr []json.RawMessage
		if err := json.Unmarshal(doc, &arr); err != nil {
			return nil, fmt.Errorf("api: result reference '*' applied to a non-array")
		}
		// Map the remaining path over each element and flatten one level.
		out := make([]json.RawMessage, 0, len(arr))
		for _, el := range arr {
			v, err := evalTokens(el, rest)
			if err != nil {
				return nil, err
			}
			// If the mapped value is itself an array, flatten it in; otherwise
			// append the scalar/object.
			var inner []json.RawMessage
			if json.Unmarshal(v, &inner) == nil && looksLikeArray(v) {
				out = append(out, inner...)
			} else {
				out = append(out, v)
			}
		}
		b, err := json.Marshal(out)
		if err != nil {
			return nil, fmt.Errorf("api: marshal mapped result reference: %w", err)
		}
		return b, nil
	}

	// Object member or array index.
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(doc, &obj); err == nil && looksLikeObject(doc) {
		v, ok := obj[tok]
		if !ok {
			return nil, fmt.Errorf("api: result reference path missing key %q", tok)
		}
		return evalTokens(v, rest)
	}
	return nil, fmt.Errorf("api: result reference token %q does not resolve", tok)
}

// unescapePointerToken applies RFC 6901 unescaping (~1 → /, ~0 → ~).
func unescapePointerToken(t string) string {
	t = strings.ReplaceAll(t, "~1", "/")
	t = strings.ReplaceAll(t, "~0", "~")
	return t
}

func looksLikeArray(b json.RawMessage) bool {
	for _, c := range b {
		switch c {
		case ' ', '\t', '\n', '\r':
			continue
		case '[':
			return true
		default:
			return false
		}
	}
	return false
}

func looksLikeObject(b json.RawMessage) bool {
	for _, c := range b {
		switch c {
		case ' ', '\t', '\n', '\r':
			continue
		case '{':
			return true
		default:
			return false
		}
	}
	return false
}
