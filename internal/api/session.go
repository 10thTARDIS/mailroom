// SPDX-License-Identifier: AGPL-3.0-only

package api

import (
	"net/http"
	"time"

	"mailroom/internal/pgpkeys"
	"mailroom/internal/store"
	"mailroom/internal/tabs"
)

// The JMAP Session resource (RFC 8620 §2): capabilities, the accounts the user
// can access, primary accounts per capability, and the URLs a client uses for
// the API, blob download/upload, and push. Served at GET /jmap/session and the
// well-known alias. Authenticated: the account is the caller's own user, taken
// from the token context, never a parameter.

// Session is the RFC 8620 §2 session object.
type Session struct {
	Capabilities    map[string]any     `json:"capabilities"`
	Accounts        map[string]Account `json:"accounts"`
	PrimaryAccounts map[string]string  `json:"primaryAccounts"`
	Username        string             `json:"username"`
	APIURL          string             `json:"apiUrl"`
	DownloadURL     string             `json:"downloadUrl"`
	UploadURL       string             `json:"uploadUrl"`
	// PreviewURL is a MAILROOM EXTENSION, not an RFC 8620 field (#131). It names
	// the one route that serves an attachment for inline rendering, and it is
	// advertised separately from downloadUrl precisely because the two are
	// different doors with different rules — a client that wants to render must ask
	// the route that will only ever hand back something renderable. Deriving it by
	// rewriting downloadUrl client-side would couple the two templates and make a
	// server-side change to either one silently wrong on the client.
	PreviewURL     string `json:"previewUrl"`
	EventSourceURL string `json:"eventSourceUrl"`
	State          string `json:"state"`
}

// Account is one entry in Session.accounts (RFC 8620 §2).
type Account struct {
	Name                string         `json:"name"`
	IsPersonal          bool           `json:"isPersonal"`
	IsReadOnly          bool           `json:"isReadOnly"`
	AccountCapabilities map[string]any `json:"accountCapabilities"`
}

// coreCapability advertises the RFC 8620 core limits (§2). The values are
// conservative, honest bounds for the 0.5 server, and each one is the SAME const
// the server actually enforces (maxSizeUpload / maxSizeRequestObject).
func coreCapability() map[string]any {
	return map[string]any{
		"maxSizeUpload":         maxUploadBytes,
		"maxConcurrentUpload":   4,
		"maxSizeRequestObject":  maxRequestBytes,
		"maxConcurrentRequests": 4,
		"maxCallsInRequest":     maxCallsInRequest,
		"maxObjectsInGet":       500,
		"maxObjectsInSet":       500,
		"collationAlgorithms":   []string{"i;ascii-casemap", "i;octet"},
	}
}

// mailCapability advertises the RFC 8621 mail limits (§1.4). The daily-driver
// subset keeps these modest.
func mailCapability() map[string]any {
	return map[string]any{
		"maxMailboxesPerEmail":       nil,
		"maxMailboxDepth":            nil,
		"maxSizeMailboxName":         255,
		"maxSizeAttachmentsPerEmail": maxAttachmentBytes,
		"emailQuerySortOptions":      []string{"receivedAt", "sentAt"},
		"mayCreateTopLevelMailbox":   false,
	}
}

// maxDelayedSend is the Undo-Send / Send-Later horizon: submissions are durable
// jobs (spec §5/§16), so the only real bound is how far ahead we are willing to
// hold one.
//
// It is a CONST because it is both advertised and enforced, and those must be the
// same number. Until send-later had a UI (#56) nothing checked it, so a client
// could schedule a send for the year 3000 and the server would write a durable job
// for it — the same disagreement maxSizeUpload's comment warns about, from the
// other end. submission.go rejects a sendAt beyond this.
const maxDelayedSend = 30 * 24 * time.Hour

// categoryTabsCapability advertises the tab VOCABULARY (#219), so a client can
// render the strip — and label a mapping control — without hard-coding five
// strings it would then have to keep in step with the server's enum.
//
// The order is Gmail's own, which is also the default `position` a newly
// materialised set of tabs gets; a user rearranges from there, and the per-tab
// `position` is what a client should actually sort on.
func categoryTabsCapability() map[string]any {
	names := make([]string, 0, len(tabs.All))
	for _, t := range tabs.All {
		names = append(names, string(t))
	}
	return map[string]any{"tabs": names}
}

// deskCapability advertises the desk's (#442, #514) closed vocabularies —
// client families, themes, densities, message-open behaviours, text sizes and
// row layouts — for
// categoryTabsCapability's own reason: a client can render its settings UI
// without hard-coding strings it would then have to keep in step with the
// server's enum. The slices themselves live in desk.go, defined once so the
// api's own validators (DeskPreferences/set, ClientPreferences/set) check
// against EXACTLY what this advertises rather than a second, hand-copied list.
func deskCapability() map[string]any {
	return map[string]any{
		"clientFamilies": deskClientFamilies,
		"themes":         deskThemes,
		"densities":      deskDensities,
		"messageOpens":   deskMessageOpens,
		// #514. Advertised for the same reason as the four above, and with one
		// extra property worth stating: textSizes is ORDERED smallest to
		// largest, so a client can render the five-step control from this
		// slice without deciding for itself which end is which.
		"textSizes":  deskTextSizes,
		"rowLayouts": deskRowLayouts,
		// #665. Area/getDesk exists. A boolean rather than a version: a client
		// that sees it sends one Area/getDesk for the whole desk, and one that
		// does not (an older server) keeps sending Area/getSummary plus a
		// per-section Email/query — the only way to tell the two apart without
		// a failed call, since both servers advertise this same URN.
		"getDesk": true,
	}
}

// pgpCapability advertises what THIS INSTANCE can do with PGP (#492).
//
// `signing` answers the one question a compose window cannot answer for itself:
// can this server produce a signature AT ALL? Signing needs the crypto process
// (ADR-024 — the private half never exists on this side of that boundary), so an
// instance with keys bound and no crypto socket sends the key and no signature,
// which is a materially different promise to make in a UI. It is derived from
// the SAME field prepareOutbound consults before signing (compose.Composer's
// signer, via CanSign), so it cannot describe a server other than this one.
//
// ⚠ IT IS SERVER-WIDE, NOT PER-IDENTITY. The other half of "Will sign" is
// Identity.signingKeyId, and a client needs BOTH — this true AND that non-null.
// Either one alone is a badge that will be wrong for somebody.
//
// `encrypt` (#47, ADR-038) is the same kind of fact for encryption: can this server
// encrypt outgoing mail at all (a crypto process wired for signing AND encrypting,
// compose.Composer.CanEncrypt)? It is server-wide; per-recipient availability is
// RecipientKey/lookup, and per-identity it still needs Identity.signingKeyId.
func pgpCapability(signing, encrypt, keyManagement bool) map[string]any {
	return map[string]any{
		"signing": signing, "encrypt": encrypt,
		// #796: whether UserKey/generate, /import, /extend, /revoke and /export
		// can be called (a crypto process is wired for key custody).
		"keyManagement": keyManagement,
		// #795: the ONE warning threshold (pgpkeys.ExpiryWarningDays) the
		// server's expiringSoon flags use, so a client's own wording ("expires
		// in 12 days") agrees with them.
		"keyExpiryWarningDays": pgpkeys.ExpiryWarningDays,
	}
}

// submissionCapability advertises the RFC 8621 §7 EmailSubmission limits.
func submissionCapability() map[string]any {
	return map[string]any{
		"maxDelayedSend":       int(maxDelayedSend.Seconds()),
		"submissionExtensions": map[string]any{},
	}
}

// SessionHandler serves the Session resource for the authenticated user.
func (a *API) SessionHandler(w http.ResponseWriter, r *http.Request) {
	userID, ok := contextUserID(r.Context())
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	var profile store.UserProfile
	err := a.store.UserTx(r.Context(), userID, func(tx *store.UserTx) error {
		p, found, err := tx.Profile(r.Context())
		if err != nil {
			return err
		}
		if !found {
			return errSentinel("api: session: user not found")
		}
		profile = p
		return nil
	})
	if err != nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	// Every capability appears in BOTH maps, and both come from the ONE
	// declaration in capability.go: a client may only `using` what is in the
	// top-level `capabilities` set, so a URN advertised per-account alone leaves
	// its methods unreachable (service.go), and a URN we advertise but do not
	// accept in `using` is the same bug from the other end.
	// An account-restricted API key's session names none of the user's
	// addresses (#49 review): the login address may be one of the accounts the
	// key is NOT allowed, so the key's own name stands in for it.
	if p := principalFrom(r.Context()); p.restricted() {
		profile.Email = "API key " + safeText(p.key.Name, 60)
	}

	accountID := userID.String()
	capabilities := capabilityObjects()
	// The push capability advertises this server's VAPID applicationServerKey — an
	// instance value, not a constant — so the browser can subscribe against us
	// (ADR-016). capabilityObjects() carries an empty placeholder; fill in the live
	// key here (empty when push is not wired).
	capabilities[capPush] = map[string]any{"vapidPublicKey": a.vapidPublicKey}
	// Whether remote images are fetched BY THIS SERVER or by the reader's
	// browser (ADR-026). The client needs it because the two are a materially
	// different bargain: "Show remote content" means "let the sender see your
	// address and browser" in one case and "let your server fetch it" in the
	// other, and a banner that said the same thing in both would be wrong in
	// one of them.
	capabilities[capRemoteContent] = map[string]any{"imageProxy": a.proxy != nil}
	// Whether outgoing mail can be signed at all (#492) — read off the composer's
	// own signer, the field prepareOutbound consults, so there is no second source
	// of truth to drift.
	//
	// ⚠ IN BOTH MAPS, unlike push and the image proxy above. `signing` is a fact
	// about this account's sending, and a client that reads accountCapabilities
	// (which is where RFC 8620 puts per-account semantics) would otherwise find
	// the placeholder `false` on a server that signs — an advertised capability
	// object quietly disagreeing with the server it describes.
	signingCap := pgpCapability(a.composer.CanSign(), a.composer.CanEncrypt(), a.custody != nil)
	capabilities[capPGP] = signingCap
	acctCaps := accountCapabilityObjects()
	acctCaps[capPGP] = signingCap
	// Which providers a mailbox can be connected through by signing in there
	// (#52, ADR-046 §3): instance configuration, so filled in live, in BOTH
	// maps for capPGP's reason. Empty when none is configured, and the
	// client then shows no Connect button at all.
	accountsCap := map[string]any{"oauthProviders": a.oauthProviders()}
	capabilities[capAccounts] = accountsCap
	acctCaps[capAccounts] = accountsCap
	acct := Account{
		Name:                profile.Email,
		IsPersonal:          true,
		IsReadOnly:          false,
		AccountCapabilities: acctCaps,
	}
	primary := make(map[string]string, len(capabilities))
	for urn := range capabilities {
		primary[urn] = accountID
	}
	session := Session{
		Capabilities:    capabilities,
		Accounts:        map[string]Account{accountID: acct},
		PrimaryAccounts: primary,
		Username:        profile.Email,
		APIURL:          "/jmap",
		DownloadURL:     "/jmap/download/{accountId}/{blobId}/{name}?type={type}",
		UploadURL:       "/jmap/upload/{accountId}/",
		// No {type} placeholder, deliberately: the preview route chooses the type
		// itself from its own short allowlist and refuses everything else, so there
		// is nothing for a client to ask for.
		PreviewURL:     "/jmap/preview/{accountId}/{blobId}/{name}",
		EventSourceURL: "/jmap/eventsource/?types={types}&closeafter={closeafter}&ping={ping}",
		State:          a.sessionState,
	}
	writeJSON(w, http.StatusOK, session)
}
