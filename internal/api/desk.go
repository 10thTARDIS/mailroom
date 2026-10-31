// SPDX-License-Identifier: AGPL-3.0-only

package api

// THE DESK'S CLOSED VOCABULARIES (#442, extended by #514) — client families,
// themes, densities, message-open behaviours, text sizes and row layouts —
// defined ONCE here rather than derived from
// internal/store/desk_preferences.go's CHECK constraints (spec §2's explicit
// instruction). The store's CHECKs stay the LAST line of defence — the
// database refuses a bad value even if some future caller bypasses this
// package — but the FIRST line has to live here, in Go, so a rejection names
// the offending PROPERTY (a typed SetError) instead of surfacing whatever text
// Postgres attaches to a constraint violation.
//
// deskCapability (session.go) advertises these same slices, so a client never
// has to hard-code them and the two cannot drift: there is exactly one place
// that says what a theme, a density, a message-open mode or a client family
// is.
var (
	// deskClientFamilies mirrors app.client_family (migration 00046).
	deskClientFamilies = []string{"web", "phone", "desktop"}
	// deskThemes mirrors client_preferences.theme's CHECK.
	deskThemes = []string{"system", "light", "dark"}
	// deskDensities mirrors client_preferences.density's CHECK.
	deskDensities = []string{"compact", "normal", "relaxed"}
	// deskMessageOpens mirrors client_preferences.message_opens's CHECK.
	deskMessageOpens = []string{"beside", "focused"}
	// deskTextSizes mirrors client_preferences.text_size's CHECK (#514,
	// migration 00052). FIVE STEPS, SMALLEST FIRST, and the ORDER IS PART OF
	// THE VOCABULARY: a client renders these as a five-step slider, so the
	// index of a value in this slice is its position on that control. "m" is
	// the middle and the size the client has always drawn at.
	deskTextSizes = []string{"xs", "s", "m", "l", "xl"}
	// deskRowLayouts mirrors client_preferences.row_layout's CHECK (#514).
	// What a line CONTAINS is the client's business; the server stores which
	// of the two shapes was chosen.
	deskRowLayouts = []string{"oneLine", "multiLine"}
)

// inVocabulary reports whether v is one of vocab's closed set.
func inVocabulary(vocab []string, v string) bool {
	for _, c := range vocab {
		if c == v {
			return true
		}
	}
	return false
}
