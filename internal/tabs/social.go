// SPDX-License-Identifier: AGPL-3.0-only

package tabs

import (
	"strings"

	"mailroom/internal/kind"
)

// THE SOCIAL TAB'S ONE NAME LIST, and the honest account of what it is.
//
// Every other rule in classify.go reads a header semantic: List-Id means list
// traffic because RFC 2919 says so, Auto-Submitted means machine-generated
// because RFC 3834 says so. There is no header that says "this is a social
// network". So Social — alone among the five — needs a list of who the social
// networks ARE.
//
// WHY THAT IS STILL LEGITIMATE under spec §8's isolation rule, which forbids
// training on other users' mail and permits global priors built from PUBLIC
// data: nothing here came from anybody's mailbox. These are published facts
// about public companies, exactly like internal/kind's ESP table, and of the
// same kind as SpamAssassin's static ruleset. No message content, no addresses,
// no behaviour.
//
// ⚠ IT IS ALSO THE RULE MOST EASILY FITTED TO THE CORPUS IT IS SCORED ON, and
// that risk is real rather than theoretical: this list could be written by
// looking up which senders carry a user's `Social` label and adding their
// domains, at which point the acceptance test would be marking its own homework
// — the exact failure #169 spent a month inside. So:
//
//   - This list was written from what a social platform IS, not from the corpus.
//   - `mailroom-admin tab-eval` reports how many DISTINCT SENDERS this rule
//     fired on, alongside its precision. A rule that scores 1.00 across three
//     senders has demonstrated nothing, and the report has to make that
//     visible rather than let a per-tab average imply otherwise.
//
// WHAT COUNTS AS SOCIAL. Gmail's Social tab is messages from social networks,
// media-sharing sites and dating services — places whose product is people
// interacting. Collaboration and developer tools are deliberately EXCLUDED even
// though they have users and notifications: mail from GitHub or Slack is a
// notification about work, which is Updates. That boundary is a judgement, it is
// stated here rather than buried, and the misfilings it produces will be
// visible in the eval as Social/Updates confusion rather than as a mystery.
var socialDomains = map[string]bool{
	// Social networks
	"facebook.com":       true,
	"facebookmail.com":   true,
	"fb.com":             true,
	"instagram.com":      true,
	"mail.instagram.com": true,
	"threads.net":        true,
	"twitter.com":        true,
	"x.com":              true,
	"linkedin.com":       true,
	"tiktok.com":         true,
	"snapchat.com":       true,
	"pinterest.com":      true,
	"tumblr.com":         true,
	"vk.com":             true,
	"bsky.app":           true,
	"nextdoor.com":       true,
	"meetup.com":         true,

	// Community and discussion platforms whose product is people talking to each
	// other. Reddit and Discord sit here; a forum is a social space even when it
	// is not a "network".
	"reddit.com":     true,
	"redditmail.com": true,
	"discord.com":    true,
	"discordapp.com": true,
	"quora.com":      true,

	// Media-sharing and interest communities, which Gmail files here too.
	"youtube.com":   true,
	"twitch.tv":     true,
	"strava.com":    true,
	"goodreads.com": true,
	"flickr.com":    true,
	"vimeo.com":     true,

	// Dating.
	"match.com":   true,
	"okcupid.com": true,
	"bumble.com":  true,
	"hinge.co":    true,
	"tinder.com":  true,
}

// isSocialPlatform reports whether a message came from one of the platforms
// above.
//
// MATCHED ON THE SENDING DOMAIN FIRST — DKIM d= and the Return-Path — and only
// then on the From domain, which is the opposite of internal/kind's ESP table
// and deliberately so. The ESP table asks "who sent this FOR the brand", and the
// From domain is the brand, so consulting it there would be a From-domain rule
// arriving by the back door. Here the platform IS the sender: a message from
// Facebook is from Facebook, and its From domain is the most reliable statement
// of that. DKIM leads anyway because it is cryptographically bound and survives
// forwarding, which matters for a catchall mailbox where mail arrives redirected.
func isSocialPlatform(f kind.Facts) bool {
	for _, d := range f.DKIMDomains {
		if lookupSocial(d) {
			return true
		}
	}
	if rp := returnPathDomain(f.ReturnPath); rp != "" && lookupSocial(rp) {
		return true
	}
	return lookupSocial(f.FromDomain)
}

// lookupSocial matches a domain against the list by suffix, so bulk-sending
// subdomains (mail.instagram.com, e.linkedin.com, maild.linkedin.com) hit
// without needing an entry each.
func lookupSocial(domain string) bool {
	d := strings.TrimSuffix(strings.ToLower(strings.TrimSpace(domain)), ".")
	if d == "" {
		return false
	}
	if socialDomains[d] {
		return true
	}
	for suffix := range socialDomains {
		if strings.HasSuffix(d, "."+suffix) {
			return true
		}
	}
	return false
}

// returnPathDomain is the domain of the envelope sender, or "".
//
// A second copy of internal/kind's unexported helper, which is worth one line of
// justification rather than a shrug: exporting kind's would widen that package's
// surface for one caller, and the alternative — reaching for the From domain
// instead — is precisely the conflation the ESP table exists to avoid. Six lines
// of duplicated string handling is the cheaper mistake.
func returnPathDomain(returnPath string) string {
	rp := strings.Trim(strings.TrimSpace(returnPath), "<>")
	if at := strings.LastIndex(rp, "@"); at > 0 {
		return strings.ToLower(rp[at+1:])
	}
	return ""
}
