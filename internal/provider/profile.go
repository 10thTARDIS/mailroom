// SPDX-License-Identifier: AGPL-3.0-only

package provider

import "strings"

// PROVIDER PROFILES BEYOND THE POLL FLOOR (spec §16, #52 increment 6,
// ADR-046 §12). Each table here is a STATED mapping, not a guess: a row exists
// because the provider documents (or a probe showed) the behaviour, and every
// row that rests on something unverified says so. A profile with no row gets
// the generic behaviour.

// ProfileMicrosoft is the provider_profile of an Outlook.com / Microsoft 365
// account connected through OAuth (oauthmail's registry).
const ProfileMicrosoft = "microsoft"

// folderSpecialUse maps a profile's well-known folder NAMES to the RFC 6154
// SPECIAL-USE attribute they stand for, for a server that lists them without
// one. SPECIAL-USE always wins when the server sends it; this is only the
// fallback.
//
// microsoft: Exchange Online and Outlook.com name the standard folders Sent
// Items, Drafts, Deleted Items, Junk Email (older tenants: Junk E-mail) and
// Archive; Inbox is INBOX by name everywhere. Whether Exchange sends
// SPECIAL-USE in LIST is UNVERIFIED (a live test settles it). The
// names are the ENGLISH ones: a mailbox whose language is not English has
// localised folder names, which this map does not cover — SPECIAL-USE is the
// only thing that would, and the folder then simply has no role.
var folderSpecialUse = map[string]map[string]string{
	ProfileMicrosoft: {
		"sent items":    `\Sent`,
		"drafts":        `\Drafts`,
		"deleted items": `\Trash`,
		"junk email":    `\Junk`,
		"junk e-mail":   `\Junk`,
		"archive":       `\Archive`,
	},
}

// FolderSpecialUse returns the SPECIAL-USE attribute a top-level folder name
// stands for under profile, or "" when the profile states none for it. Only
// top-level names match (a "Sent Items" nested under another folder is just a
// folder); the comparison is case-insensitive.
func FolderSpecialUse(profile, name string, delim rune) string {
	m := folderSpecialUse[profile]
	if m == nil || isNested(name, delim) {
		return ""
	}
	return m[strings.ToLower(name)]
}

// nonMailFolders are folders a profile lists over IMAP that hold no mail
// (Exchange exposes its calendar, contacts, notes and tasks stores as IMAP
// folders, plus Outlook's local Outbox and conflict folders). Mailroom marks
// them NOT SYNCED (sync_scope='off') the first time it sees them, so they are
// neither polled nor shown as live mail folders; a user can switch one on.
var nonMailFolders = map[string]map[string]bool{
	ProfileMicrosoft: {
		"calendar":             true,
		"contacts":             true,
		"journal":              true,
		"notes":                true,
		"tasks":                true,
		"outbox":               true,
		"sync issues":          true,
		"conversation history": true,
		"rss feeds":            true,
		"rss subscriptions":    true,
	},
}

// NonMailFolder reports whether name (or the top-level folder it is nested
// under) is one of profile's non-mail folders.
func NonMailFolder(profile, name string, delim rune) bool {
	m := nonMailFolders[profile]
	if m == nil {
		return false
	}
	top := name
	if delim != 0 {
		if i := strings.IndexRune(name, delim); i >= 0 {
			top = name[:i]
		}
	}
	return m[strings.ToLower(top)]
}

func isNested(name string, delim rune) bool {
	return delim != 0 && strings.ContainsRune(name, delim)
}

// outboundDailyQuota is a profile's default daily send cap
// (app.accounts.outbound_daily_quota) for a newly connected account; a profile
// with no row takes the schema default.
//
// microsoft: 100 submissions a day. Microsoft's real limits are not verified
// here (the design spike marks them UNVERIFIED) and differ by account type —
// Exchange Online counts recipients per day in the thousands, consumer
// Outlook.com is lower and stricter for new accounts — and exceeding them
// locks the account's sending for a day or more, everywhere, the user's own
// Outlook included. 100 is conservative against every figure in circulation
// even at several recipients per message. It equals the generic schema
// default today; it is stated here so the Microsoft value is a decision of
// its own and does not move if the generic default ever does.
var outboundDailyQuota = map[string]int{
	ProfileMicrosoft: 100,
}

// OutboundDailyQuota returns profile's default daily send cap, or 0 for "the
// schema default".
func OutboundDailyQuota(profile string) int {
	return outboundDailyQuota[profile]
}

// IMAPAccessHint is the explanation to give when a provider issues a fresh
// OAuth token and its IMAP server still refuses it (sync.ErrIMAPAccessRefused):
// the grant is fine, so the person does not need to reconnect — IMAP itself is
// switched off for the mailbox.
func IMAPAccessHint(profile string) string {
	switch profile {
	case ProfileMicrosoft:
		return "IMAP access is off for this mailbox. On Outlook.com, turn it on in Outlook " +
			"settings (Mail, Forwarding and IMAP); on a work or school account, ask your " +
			"administrator to enable IMAP for your mailbox. You do not need to reconnect the account"
	case "gmail":
		return "Gmail refused IMAP access for this account although Google issued a fresh " +
			"sign-in; a Google Workspace administrator may have turned IMAP off"
	}
	return "the mail server refused IMAP access although the provider issued a fresh sign-in; " +
		"IMAP may be switched off for this mailbox"
}
