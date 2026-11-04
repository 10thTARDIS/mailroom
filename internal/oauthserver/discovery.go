// SPDX-License-Identifier: AGPL-3.0-only

package oauthserver

import (
	"encoding/json"
	"net/http"
)

// Discovery documents (ADR-050 section 2): unauthenticated GETs serving the
// same document to everyone.

func writeDoc(w http.ResponseWriter, doc map[string]any) {
	b, _ := json.Marshal(doc)
	h := w.Header()
	h.Set("Content-Type", "application/json")
	h.Set("Cache-Control", "public, max-age=300")
	// Metadata is public by design; a browser-based MCP client may read it.
	h.Set("Access-Control-Allow-Origin", "*")
	_, _ = w.Write(b)
}

// ProtectedResource serves RFC 9728 metadata for /mcp, at both well-known paths.
func (s *Server) ProtectedResource() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeDoc(w, map[string]any{
			"resource":                 s.resource,
			"authorization_servers":    []string{s.issuer},
			"bearer_methods_supported": []string{"header"},
			"scopes_supported":         s.policy.DefaultScopes,
			"resource_name":            "Mailroom",
		})
	})
}

// AuthorizationServer serves RFC 8414 metadata. registration_endpoint is
// present only when Dynamic Client Registration is on.
func (s *Server) AuthorizationServer() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		doc := map[string]any{
			"issuer":                                         s.issuer,
			"authorization_endpoint":                         s.issuer + "/oauth/authorize",
			"token_endpoint":                                 s.issuer + "/oauth/token",
			"revocation_endpoint":                            s.issuer + "/oauth/revoke",
			"response_types_supported":                       []string{"code"},
			"grant_types_supported":                          []string{"authorization_code", "refresh_token"},
			"code_challenge_methods_supported":               []string{"S256"},
			"token_endpoint_auth_methods_supported":          []string{"none"},
			"scopes_supported":                               s.policy.Scopes,
			"client_id_metadata_document_supported":          true,
			"authorization_response_iss_parameter_supported": true,
		}
		if s.dcr {
			doc["registration_endpoint"] = s.issuer + "/oauth/register"
		}
		writeDoc(w, doc)
	})
}
