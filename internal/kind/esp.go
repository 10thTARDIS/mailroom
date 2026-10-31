// SPDX-License-Identifier: AGPL-3.0-only

package kind

import "strings"

// Sending-infrastructure identification: which email service provider sent this,
// and what that provider is mostly used FOR (mailroom#175).
//
// The idea: a retailer's marketing and its receipts routinely leave the same
// brand on different infrastructure — promotions through a campaign platform,
// order confirmations through a transactional API — and the DKIM d= domain or
// the envelope Return-Path says which, on the FIRST message ever seen from that
// sender.
//
// ⚠ HOW MUCH OF TODAY'S MAIL THIS CAN ACTUALLY REACH — MEASURED, NOT ASSUMED.
// #175 called this "the highest-value untapped signal". On a real mailbox
// it reaches almost none of it. Of the highest-volume unrecognised
// bulk senders, most sign with their OWN domain:
//
//	news.shop.example          dkim=news.shop.example
//	mail.store.example         dkim=mail.store.example
//	email.theatre.example      dkim=email.theatre.example
//	...
//
// and only a few name a provider at all (a SendGrid
// signature alongside the brand's own, say).
//
// The cause is the same regulation #175 cites as making bulk mail
// self-identifying. DMARC alignment requires d= to align with the From domain,
// and the 2024 Gmail/Yahoo rules made DMARC compulsory for bulk senders — so
// every ESP now offers custom signing domains, and the more compliant a sender
// is, the LESS their headers reveal about who sends for them. Sender-identifying
// and ESP-identifying pull in opposite directions.
//
// So this table is a long-tail helper, not a primary signal, and it is bounded
// by something no amount of curation fixes: you cannot fingerprint a provider
// that has deliberately made itself invisible. Mail from a brand's own aligned
// subdomain needs a different discriminator — the marketing-subdomain SHAPE
// (`e.`, `eml.`, `news.`, `email.` on a brand's registrable domain), which is
// observable without any table and does not need per-retailer maintenance.
//
// WHY THIS IS LEGITIMATE under the no-corpus rule. Spec §8 forbids training on
// other users' mail and permits global priors built from PUBLIC data. Nothing
// here came from anybody's mailbox: these are published product facts about
// public companies, of the same kind as SpamAssassin's static ruleset. No
// message content, no addresses, no behaviour.
//
// HOW IT IS MATCHED. On the DKIM d= domain and the Return-Path domain, never on
// the From domain — the whole point is that From says who the mail is FROM while
// these say who SENT it. A retailer's From is always the retailer.
//
// ACCURACY, HONESTLY. Providers move between categories, big senders use several,
// and a platform's "transactional" tier can carry marketing if a customer
// misuses it. So these weights are moderate rather than decisive, and the mixed
// class deliberately says only "machine-sent at volume". The table is meant to
// be extended from what a mailbox turns up — `mailroom-admin classify`
// reports unmatched high-volume sending domains for exactly that purpose.

// ESPClass is what a message's sending infrastructure is predominantly used FOR.
//
// EXPORTED for the category-tab classifier (mailroom#219), which needs the same
// answer this package's own signal table needs. It is a FACT about public
// sending infrastructure, not a verdict: the tab layer reads it and decides for
// itself, exactly as the classification architecture requires — separate the
// kind layer's FACTS (which have always worked) from its AGGREGATE (which is
// where every failure lived). Nothing about the table, the matching or the
// weights changed with the export.
type ESPClass int

const (
	// ESPNone is "not recognised", which is the common case: DMARC alignment has
	// made most bulk senders' infrastructure invisible (see the file header).
	ESPNone ESPClass = iota
	// ESPMarketing is infrastructure used predominantly for campaigns.
	ESPMarketing
	// ESPTransactional is infrastructure used predominantly for receipts,
	// confirmations, password resets and other machine mail the user triggered.
	ESPTransactional
	// ESPNewsletter is publishing platforms — subscribed editorial rather than
	// retail marketing.
	ESPNewsletter
	// ESPMixed is general-purpose sending: says volume, says nothing about
	// purpose.
	ESPMixed
)

// espDomains maps a sending domain suffix to what it is predominantly used for.
// Matched as a suffix so subdomains (mail.mcsv.net, bounce.sendgrid.net) hit.
var espDomains = map[string]ESPClass{
	// -- campaign / marketing platforms --
	// Observed in real mail, which is the only reason any entry
	// belongs here. These three are the ONLY senders in that sample whose
	// headers still name an ESP at all — see the aligned-DKIM note in the file
	// header for why that number is so small.
	"informz.net":      ESPMarketing, // Informz (association/nonprofit marketing)
	"mktdns.com":       ESPMarketing, // Marketo / Adobe
	"en25.com":         ESPMarketing, // Oracle Eloqua
	"mcsv.net":         ESPMarketing, // Mailchimp
	"mcdlv.net":        ESPMarketing, // Mailchimp
	"mailchimpapp.net": ESPMarketing,
	"klaviyomail.com":  ESPMarketing,
	"klaviyo.com":      ESPMarketing,
	"braze.com":        ESPMarketing,
	"iterable.com":     ESPMarketing,
	"marketo.com":      ESPMarketing,
	"mktomail.com":     ESPMarketing, // Marketo
	"hubspotemail.net": ESPMarketing,
	// hubspot.com / salesforce.com are the vendors' OWN corporate domains, not
	// the ones their platforms send customer mail from (hubspotemail.net,
	// exacttarget.com / mcsv.net). Reviewed as "brands, not providers" and left
	// in deliberately: what they actually match is HubSpot's and Salesforce's own
	// marketing to you, which IS marketing — the entry is mislabelled as
	// infrastructure rather than wrong about the mail.
	//
	// The sharper version of that review point stands and is worth keeping in
	// view: with DMARC alignment a platform's d= equals its From domain, so any
	// entry for a domain that sends its own mail is a From-domain rule arriving
	// by the back door. TestESPIsNotMatchedOnTheFromDomain still passes — it
	// pins that ESPClassOf reads d= and Return-Path, never FromDomain — so this
	// is a conceptual leak, not a broken invariant. Removing the platform
	// entries (substack, patreon, medium) would also lose genuine signal, since
	// those domains really do carry many different writers' mail. Not changed
	// without a measurement that shows which way it cuts.
	"hubspot.com":         ESPMarketing,
	"exacttarget.com":     ESPMarketing, // Salesforce Marketing Cloud
	"salesforce.com":      ESPMarketing,
	"attentivemobile.com": ESPMarketing,
	"constantcontact.com": ESPMarketing,
	"ctctemail.com":       ESPMarketing, // Constant Contact
	"cheetahmail.com":     ESPMarketing,
	"responsys.net":       ESPMarketing,
	"epsilon.com":         ESPMarketing,
	"listrakbi.com":       ESPMarketing,
	"bluecore.com":        ESPMarketing,
	"omnisend.com":        ESPMarketing,
	"drip.com":            ESPMarketing,
	"activecampaign.com":  ESPMarketing,
	"emarsys.net":         ESPMarketing,
	"sailthru.com":        ESPMarketing,
	"dotdigital.com":      ESPMarketing,
	"getresponse.com":     ESPMarketing,
	"aweber.com":          ESPMarketing,
	"campaignmonitor.com": ESPMarketing,
	"createsend.com":      ESPMarketing, // Campaign Monitor
	"sendinblue.com":      ESPMarketing,
	"brevo.com":           ESPMarketing,

	// -- transactional platforms --
	"postmarkapp.com":    ESPTransactional,
	"mtasv.net":          ESPTransactional, // Postmark
	"sparkpostmail.com":  ESPMixed,
	"mailgun.org":        ESPMixed,
	"mailgun.net":        ESPMixed,
	"sendgrid.net":       ESPMixed,
	"sendgrid.com":       ESPMixed,
	"amazonses.com":      ESPMixed,
	"mandrillapp.com":    ESPTransactional,
	"customeriomail.com": ESPMixed,
	"courier.com":        ESPTransactional,
	"resend.com":         ESPTransactional,
	"loops.so":           ESPMarketing,

	// -- newsletter / publishing platforms --
	"substack.com":     ESPNewsletter,
	"substackcdn.com":  ESPNewsletter,
	"beehiiv.com":      ESPNewsletter,
	"ghost.io":         ESPNewsletter,
	"buttondown.email": ESPNewsletter,
	"convertkit.com":   ESPNewsletter,
	"ck.page":          ESPNewsletter, // ConvertKit
	"revue.email":      ESPNewsletter,
	"tinyletter.com":   ESPNewsletter,
	"patreon.com":      ESPNewsletter,
	"medium.com":       ESPNewsletter,
}

// ESPClassOf identifies the sending infrastructure from DKIM d= and the
// Return-Path.
//
// DKIM leads because it is cryptographically bound to the sending domain and
// survives forwarding; the Return-Path is the fallback. The From domain is never
// consulted — it names the brand, not the sender, and conflating the two is the
// mistake this whole signal exists to avoid.
//
// A message signed by several domains takes the most SPECIFIC verdict available:
// a retailer that signs with both its own domain and its ESP's should be read as
// the ESP, since that is the part that says how the mail was sent.
func ESPClassOf(f Facts) ESPClass {
	best := ESPNone
	consider := func(domain string) {
		if c := lookupESP(domain); c != ESPNone && (best == ESPNone || best == ESPMixed) {
			// A purposeful class beats mixed; otherwise first match wins.
			if best == ESPNone || c != ESPMixed {
				best = c
			}
		}
	}
	for _, d := range f.DKIMDomains {
		consider(d)
	}
	if rp := returnPathDomain(f.ReturnPath); rp != "" {
		consider(rp)
	}
	return best
}

// bulkStreamPrefixes are the leftmost labels brands use when they segregate a
// bulk sending stream onto a subdomain of their own registrable domain:
// news.shop.example, mail.store.example, email.theatre.example, maild.linkedin.com.
//
// This is a SHAPE, not a fact about a company, which is why it is weaker
// evidence than an ESP-table match and weighted well below one. It exists
// because aligned DKIM has made the ESP itself unidentifiable for most bulk mail
// (see the file header): the provider is hidden, but the fact that the brand
// runs a separate bulk stream is still written on the envelope.
var bulkStreamPrefixes = map[string]bool{
	"e": true, "em": true, "eml": true, "email": true, "emails": true,
	"mail": true, "mailer": true, "mailers": true, "mailing": true,
	"maila": true, "mailb": true, "mailc": true, "maild": true, "maile": true,
	"news": true, "newsletter": true, "newsletters": true,
	"send": true, "sending": true, "sender": true,
	"mktg": true, "marketing": true, "campaign": true, "campaigns": true,
	"links": true, "link": true, "click": true, "clicks": true,
	"o": true, "s": true, "t": true, "m": true, "h5": true,
}

// HasBulkStreamShape reports whether the sender's own domain looks like a
// dedicated bulk-sending subdomain. It requires at least three labels, so a
// bare registrable domain (shop.example, news.example) never matches — those
// send campaigns from their main domain and this signal cannot see them.
func HasBulkStreamShape(fromDomain string) bool {
	d := strings.ToLower(strings.TrimSpace(fromDomain))
	d = strings.TrimSuffix(d, ".")
	labels := strings.Split(d, ".")
	if len(labels) < 3 {
		return false
	}
	return bulkStreamPrefixes[labels[0]]
}

// lookupESP matches a domain against the table by suffix.
func lookupESP(domain string) ESPClass {
	d := strings.ToLower(strings.TrimSpace(domain))
	d = strings.TrimSuffix(d, ".")
	if d == "" {
		return ESPNone
	}
	if c, ok := espDomains[d]; ok {
		return c
	}
	for suffix, c := range espDomains {
		if strings.HasSuffix(d, "."+suffix) {
			return c
		}
	}
	return ESPNone
}

// returnPathDomain is the domain of the envelope sender, or "".
func returnPathDomain(returnPath string) string {
	rp := strings.Trim(strings.TrimSpace(returnPath), "<>")
	if at := strings.LastIndex(rp, "@"); at > 0 {
		return strings.ToLower(rp[at+1:])
	}
	return ""
}

// DKIMDomains extracts the d= values from DKIM-Signature header values.
//
// Exported because the signal is only as good as the parsing, and every caller
// that builds Facts — ingest, the admin report, the backfill — must extract it
// the SAME way. A second implementation is how the classifier ends up disagreeing
// with itself about which mail came from where.
func DKIMDomains(signatures []string) []string {
	var out []string
	seen := map[string]bool{}
	for _, sig := range signatures {
		for _, part := range strings.Split(sig, ";") {
			part = strings.TrimSpace(part)
			if !strings.HasPrefix(part, "d=") {
				continue
			}
			d := strings.ToLower(strings.TrimSpace(strings.TrimPrefix(part, "d=")))
			d = strings.TrimSuffix(d, ".")
			if d != "" && !seen[d] {
				seen[d] = true
				out = append(out, d)
			}
		}
	}
	return out
}

// ESPUnknown reports that the sending infrastructure was not recognised — used
// by the admin report to surface table candidates from real mail.
func ESPUnknown(f Facts) bool { return ESPClassOf(f) == ESPNone }

// SendingDomain is the domain that actually sent the message: the first DKIM d=,
// else the Return-Path domain. This is what the ESP table is keyed on, and what
// the report lists when it matches nothing.
func SendingDomain(f Facts) string {
	if len(f.DKIMDomains) > 0 {
		return f.DKIMDomains[0]
	}
	return returnPathDomain(f.ReturnPath)
}
