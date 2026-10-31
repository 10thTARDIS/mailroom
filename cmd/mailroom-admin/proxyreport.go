// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/api"
	"mailroom/internal/blob"
	"mailroom/internal/crypto"
	"mailroom/internal/egress"
	"mailroom/internal/mime"
	"mailroom/internal/store"
	"mailroom/internal/syncrunner"
)

// `mailroom-admin proxy-report` — what an image proxy would actually do on this
// mailbox, measured before the endpoint exists (#55 increment 3, ADR-026 §6).
//
// THE POPULATION IS NARROWER THAN "REMOTE IMAGES", and the narrowing is the
// design. The proxy fetches only what the client would have fetched anyway
// (§3c), so a message whose sender is not allowed contributes nothing; and a
// beacon has no proxy reference and cannot acquire one (§3d), so proxying one
// would reintroduce exactly the tracking v1.42.0 removed. Both exclusions are
// COUNTED rather than silently applied — an excluded population that is never
// printed is indistinguishable from one that is empty.
//
// ⚠ IT USES THE PROXY'S OWN MATCH, NOT THE CLIENT'S. store.RemoteAllowsMatch
// mirrors the client deliberately, because a report normally asks "what would
// the client do". This report asks "what would the PROXY do", so it uses
// store.ProxyRemoteAllowsMatch — the stricter, fail-closed rule of §3c. The two
// differ on senders a stranger can choose, and the report prints how many
// messages fall in the gap, because a fail-closed rule that costs real mail is
// worth knowing about before it ships.
//
// THE ROW THIS PROJECT WOULD REGRET SKIPPING is "resolves to a non-public
// address today". A guard that has never been observed to refuse anything is a
// guard nobody has evidence works, and that number is the difference between
// the SSRF guard defending against real mail and against the hypothetical.
//
// READ-ONLY with respect to Mailroom's data: it writes nothing and proposes
// nothing.
//
// THE PREFETCH SECTION (#922) is the report that comes before the arrival
// prefetch runs: for mail received inside the window it counts the images the
// prefetch WOULD fetch — the same list the job walks (api.ProxyEligibleImages:
// parked by the shipped sanitizer, beacons excluded, per-message cap applied),
// for the same senders (the proxy's own rule) — and how many of those are
// already cached, plus the cache's current size split into referenced (kept
// while a message is kept, bounded by no eviction) and unreferenced. It
// fetches nothing.
//
// ⚠ BUT --fetch IS NOT READ-ONLY WITH RESPECT TO THE OUTSIDE WORLD, which is
// why it is off by default. Fetching every proxied image tells every one of
// those senders that this mailbox is live and that their mail was processed —
// for the whole corpus at once, including mail from months ago that was never
// opened. That is the exact harm this epic exists to reduce, so it is opt-in,
// with the count printed before anything leaves.
func runProxyReport(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("proxy-report", flag.ContinueOnError)
	userFlag := fs.String("user-id", "", "user whose corpus to measure (required)")
	top := fs.Int("top", 15, "show this many hosts / parameters (0 = all)")
	batch := fs.Int("batch", 500, "messages per page of the keyset walk")
	resolve := fs.Bool("resolve", true,
		"resolve each host and report how many are non-public (DNS only; no sender learns anything)")
	fetch := fs.Bool("fetch", false,
		"ALSO fetch each image to measure sizes. ⚠ This contacts every sender's server.")
	fetchMax := fs.Int("fetch-max", 300, "with --fetch, stop after this many images (0 = no limit)")
	prefetchWindow := fs.Duration("prefetch-window", 7*24*time.Hour,
		"count what the arrival prefetch would fetch for mail received within this window "+
			"(match MAILROOM_IMAGE_PROXY_PREFETCH_MAX_AGE; 0 skips the section). Fetches nothing.")
	if err := fs.Parse(args); err != nil {
		return err
	}
	userID, err := parseUserID(*userFlag)
	if err != nil {
		return err
	}

	d, err := openDeps(ctx)
	if err != nil {
		return err
	}
	defer d.close()

	blobs, err := openBlobStore(ctx, d)
	if err != nil {
		return err
	}
	master, err := d.keys.MasterKey(ctx)
	if err != nil {
		return fmt.Errorf("proxy-report: master key: %w", err)
	}
	dek, err := crypto.DeriveKey(master, userID[:], syncrunner.MailBlobInfoV1)
	crypto.Zero(master)
	if err != nil {
		return fmt.Errorf("proxy-report: derive mail dek: %w", err)
	}
	defer crypto.Zero(dek)

	// Read the grants ONCE: a report that re-read them per message could
	// describe two different states in one table.
	var allows []store.RemoteAllow
	if err := d.store.UserTx(ctx, userID, func(tx *store.UserTx) error {
		var e error
		allows, e = tx.ListRemoteAllows(ctx)
		return e
	}); err != nil {
		return fmt.Errorf("proxy-report: list remote allows: %w", err)
	}

	agg, err := scanCorpusProxy(ctx, d, blobs, userID, dek, allows, *batch, *prefetchWindow)
	if err != nil {
		return err
	}
	agg.print(allows, *top)
	if *prefetchWindow > 0 {
		if err := agg.measurePrefetchCache(ctx, d, userID); err != nil {
			return err
		}
		agg.printPrefetch(*prefetchWindow)
	} else {
		rpln("")
		rpln("prefetch                     : NOT MEASURED (--prefetch-window=0)")
	}

	if *resolve {
		agg.resolveHosts(ctx)
		agg.printResolution(*top)
	} else {
		rpln("")
		rpln("resolution                   : NOT MEASURED (--resolve=false)")
		rpln("  ⚠ The row that says whether the SSRF guard would ever fire on real")
		rpln("    mail is therefore absent. Absent is not zero.")
	}

	if *fetch {
		agg.fetchSizes(ctx, d, *fetchMax)
		agg.printSizes()
	} else {
		rpln("")
		rpln("size distribution            : NOT MEASURED (--fetch is off by default)")
		rpln("  Fetching would contact every sender's server for the whole corpus at")
		rpln("  once, which is the harm this epic exists to reduce. Re-run with")
		rpln("  --fetch to set the byte cap and cache budget from evidence.")
	}
	return nil
}

// trackingParams is the familiar strippable set, drawn from LINK tracking.
//
// ⚠ IT IS PRINTED AS A COUNT, NOT ACTED ON. ADR-026 decision D is "strip
// nothing, measure first", precisely because this list comes from link tracking
// and an image URL more often encodes the recipient in the path or a required
// token — stripping there yields a 404, which fails SILENTLY by removing
// content the reader was meant to see. The report also counts EVERY parameter
// name it sees, not just these, so the table shows what is actually in the mail
// rather than only what this list guessed.
var trackingParams = map[string]bool{
	"utm_source": true, "utm_medium": true, "utm_campaign": true,
	"utm_term": true, "utm_content": true, "utm_id": true,
	"fbclid": true, "gclid": true, "dclid": true, "msclkid": true,
	"twclid": true, "igshid": true, "ttclid": true,
	"mc_eid": true, "mc_cid": true, "mkt_tok": true,
	"_hsenc": true, "_hsmi": true, "vero_id": true, "vero_conv": true,
	"oly_enc_id": true, "oly_anon_id": true, "ck_subscriber_id": true,
	"sc_customer": true, "sc_eh": true, "sc_uid": true,
}

type proxyAggregate struct {
	scanned    int
	unreadable int
	withHTML   int

	// Eligibility, counted rather than assumed.
	msgsProxyAllowed  map[ulid.ULID]bool
	msgsClientAllowed map[ulid.ULID]bool
	// ⚠ The gap: messages the CLIENT would load and the PROXY refuses. This is
	// what fail-closed costs on real mail.
	msgsClientOnly map[ulid.ULID]bool

	// The proxied population: parked images in proxy-allowed messages, beacons
	// excluded.
	proxied         int
	beaconsExcluded int
	msgsWithProxied map[ulid.ULID]bool

	// The forms the sanitizer drops before they could ever be parked (#674):
	// the proxy never sees them. Counted per occurrence and per message — a
	// message with several HTML parts counts once.
	silentBackgroundImgs int
	silentBackgroundMsgs int
	silentPictureImgs    int
	silentPictureMsgs    int
	silentCidImgs        int
	silentCidMsgs        int

	// Scheme split. Protocol-relative is a THIRD case, not a missing value.
	bySchemeImgs  map[string]int
	bySchemeHosts map[string]map[string]bool

	hostImgs     map[string]int
	paramImgs    map[string]int // parameter NAME -> images carrying it (never values)
	imgsAnyTP    int            // images carrying at least one KNOWN tracking param
	imgsAnyParam int

	// Populated by --fetch / --resolve. Kept apart from the scan so an
	// unmeasured row can print as unmeasured rather than as zero.
	//
	// ⚠ Full URLs live here transiently and are NEVER printed or aggregated by
	// URL — see the warning on api.RemoteImage.URL.
	fetchURLs []string

	resolved       bool
	hostPublic     map[string]bool
	hostBlocked    map[string]string // host -> the guard's reason
	hostUnresolved map[string]string

	fetched     bool
	fetchTried  int
	fetchOK     int
	fetchFailed map[string]int
	// ⚠ Failures are attributed to a HOST as well as a kind. An unattributed
	// "9 x HTTP 403" is a number nobody can act on; the same nine against one
	// host whose URLs carry a token-time parameter is a finding about signed,
	// expiring URLs — and therefore about what the proxy must do when a fetch
	// legitimately fails.
	fetchFailedHost map[string]int
	sizes           []int64

	// The arrival prefetch (#922), for mail received inside the window. Each
	// row is its own count so "nothing to fetch" and "nothing in the window"
	// read differently.
	pfWindowStart     time.Time
	pfMsgsInWindow    int // any sender
	pfMsgsAllowed     int // the proxy's rule allows the sender
	pfMsgsWithImages  int // ...and at least one eligible image
	pfMsgsCapped      int // the per-message cap cut images off
	pfImages          int // eligible images, distinct within each message
	pfDistinct        map[string]bool
	pfCached          int // of pfDistinct, already in the cache (servable type)
	pfMeasured        bool
	pfCache           store.ProxyCacheSplit
	pfUnreadableInWin int
}

func newProxyAggregate() *proxyAggregate {
	return &proxyAggregate{
		msgsProxyAllowed:  map[ulid.ULID]bool{},
		msgsClientAllowed: map[ulid.ULID]bool{},
		msgsClientOnly:    map[ulid.ULID]bool{},
		msgsWithProxied:   map[ulid.ULID]bool{},
		bySchemeImgs:      map[string]int{},
		bySchemeHosts:     map[string]map[string]bool{},
		hostImgs:          map[string]int{},
		paramImgs:         map[string]int{},
		hostPublic:        map[string]bool{},
		hostBlocked:       map[string]string{},
		hostUnresolved:    map[string]string{},
		fetchFailed:       map[string]int{},
		fetchFailedHost:   map[string]int{},
		pfDistinct:        map[string]bool{},
	}
}

// schemeLabel turns a parked URL's scheme into the row it belongs in.
func schemeLabel(s string) string {
	switch s {
	case "https":
		return "https"
	case "http":
		return "http (blocked as mixed content today)"
	case "":
		return "protocol-relative (// — inherits https)"
	default:
		return "other: " + s
	}
}

func (a *proxyAggregate) addImage(id ulid.ULID, img api.RemoteImage) {
	a.proxied++
	a.msgsWithProxied[id] = true

	label := schemeLabel(img.Scheme)
	a.bySchemeImgs[label]++
	if a.bySchemeHosts[label] == nil {
		a.bySchemeHosts[label] = map[string]bool{}
	}
	if img.Host != "" {
		a.bySchemeHosts[label][img.Host] = true
		a.hostImgs[img.Host]++
	}
	a.fetchURLs = append(a.fetchURLs, img.URL)

	// Parameter NAMES only. A value is the recipient's identifier.
	u, err := url.Parse(img.URL)
	if err != nil {
		return
	}
	q := u.Query()
	if len(q) > 0 {
		a.imgsAnyParam++
	}
	known := false
	for name := range q {
		n := strings.ToLower(name)
		a.paramImgs[n]++
		if trackingParams[n] {
			known = true
		}
	}
	if known {
		a.imgsAnyTP++
	}
}

func scanCorpusProxy(
	ctx context.Context,
	d *deps,
	blobs blob.BlobStore,
	userID ulid.ULID,
	dek []byte,
	allows []store.RemoteAllow,
	batch int,
	prefetchWindow time.Duration,
) (*proxyAggregate, error) {
	agg := newProxyAggregate()
	after := ulid.ULID{}
	agg.pfWindowStart = time.Now().Add(-prefetchWindow)

	for {
		var page []store.MessageAuthRef
		if err := d.store.UserTx(ctx, userID, func(tx *store.UserTx) error {
			var e error
			page, e = tx.MessageAuthRefsAfter(ctx, after, batch)
			return e
		}); err != nil {
			return nil, fmt.Errorf("proxy-report: walk messages: %w", err)
		}
		if len(page) == 0 {
			break
		}
		for _, ref := range page {
			after = ref.ID
			agg.scanned++

			// Both rules, on every message, so the gap between them is measured
			// rather than assumed.
			proxyOK := store.ProxyRemoteAllowsMatch(ref.FromAddress, allows)
			clientOK := store.RemoteAllowsMatch(ref.FromAddress, allows)
			if proxyOK {
				agg.msgsProxyAllowed[ref.ID] = true
			}
			if clientOK {
				agg.msgsClientAllowed[ref.ID] = true
			}
			if clientOK && !proxyOK {
				agg.msgsClientOnly[ref.ID] = true
			}
			inWindow := prefetchWindow > 0 && !ref.ReceivedAt.Before(agg.pfWindowStart)
			if inWindow {
				agg.pfMsgsInWindow++
				if proxyOK {
					agg.pfMsgsAllowed++
				}
			}
			if !proxyOK {
				continue
			}

			raw, err := readMailBlob(ctx, blobs, userID, dek, ref.ContentHash)
			if err != nil {
				// NOT silently skipped: a body we could not read is a message
				// this report did not measure.
				agg.unreadable++
				if inWindow {
					agg.pfUnreadableInWin++
				}
				d.log.Warn("proxy-report: read blob failed", "message", ref.ID, "err", err)
				continue
			}
			msg, err := mime.ParseBytes(raw)
			if err != nil {
				agg.unreadable++
				if inWindow {
					agg.pfUnreadableInWin++
				}
				d.log.Warn("proxy-report: parse failed", "message", ref.ID, "err", err)
				continue
			}
			if inWindow {
				agg.addPrefetch(msg)
			}
			sawHTML := false
			silentBG, silentPict, silentCid := false, false, false
			for _, p := range msg.Parts {
				if p.ContentType != "text/html" {
					continue
				}
				sawHTML = true
				_, imgs, silent := api.MeasureHTMLImages(string(p.Data))
				// #674: the forms the sanitizer drops before they could be
				// parked — the proxy never sees them. Occurrences sum over this
				// message's HTML parts; the message itself counts once.
				if silent.BackgroundAttr > 0 {
					agg.silentBackgroundImgs += silent.BackgroundAttr
					silentBG = true
				}
				if silent.PictureSource > 0 {
					agg.silentPictureImgs += silent.PictureSource
					silentPict = true
				}
				if silent.CidPercentEncoded > 0 {
					agg.silentCidImgs += silent.CidPercentEncoded
					silentCid = true
				}
				for _, img := range imgs {
					// §3d: a beacon has no proxy reference and cannot acquire
					// one. Counted, not silently dropped.
					if api.IsTrackingPixel(img) {
						agg.beaconsExcluded++
						continue
					}
					agg.addImage(ref.ID, img)
				}
			}
			if silentBG {
				agg.silentBackgroundMsgs++
			}
			if silentPict {
				agg.silentPictureMsgs++
			}
			if silentCid {
				agg.silentCidMsgs++
			}
			if sawHTML {
				agg.withHTML++
			}
		}
		if len(page) < batch {
			break
		}
	}
	return agg, nil
}

func (a *proxyAggregate) print(allows []store.RemoteAllow, top int) {
	rp("scanned                      : %d messages\n", a.scanned)
	rp("remote-content grants        : %d\n", len(allows))
	rp("unreadable                   : %d  (blob or MIME failure — NOT counted as clean)\n", a.unreadable)
	rpln("")
	rpln("-- who the proxy would fetch for (ADR-026 §3c) ------------------------")
	rp("messages the CLIENT allows   : %d\n", len(a.msgsClientAllowed))
	rp("messages the PROXY allows    : %d  (the stricter, fail-closed rule)\n", len(a.msgsProxyAllowed))
	rp("  client-only (the gap)      : %d  <- what fail-closed costs on real mail\n", len(a.msgsClientOnly))
	if len(a.msgsClientOnly) == 0 {
		rpln("     Zero here is a finding, not an empty result: on this corpus the")
		rpln("     stricter rule refuses nothing the client would have loaded.")
	}
	rpln("")
	rpln("-- what would be proxied ----------------------------------------------")
	rp("messages with proxied images : %d\n", len(a.msgsWithProxied))
	rp("images that would be proxied : %d\n", a.proxied)
	rp("beacons EXCLUDED (§3d)       : %d  (parked un-restorably in v1.42.0; the proxy never sees them)\n",
		a.beaconsExcluded)
	rp("distinct hosts               : %d\n", len(a.hostImgs))
	rpln("")
	rpln("-- images the proxy never sees (#674) ---------------------------------")
	rp("background= attribute (remote)  : %d in %d messages\n", a.silentBackgroundImgs, a.silentBackgroundMsgs)
	rp("<picture> <source> (remote)     : %d in %d messages\n", a.silentPictureImgs, a.silentPictureMsgs)
	rp("cid: with %%-encoding (unmatched): %d in %d messages\n", a.silentCidImgs, a.silentCidMsgs)
	rpln("  CSS url() backgrounds are dropped by the CSS allowlist: see css-report")
	rpln("  (background / background-image rows) for those.")
	if a.proxied == 0 {
		rpln("")
		rpln("⚠ NOTHING WOULD BE PROXIED. That is a design finding rather than an empty")
		rpln("  result: with no eligible image, the endpoint has nothing to serve and")
		rpln("  the rows below describe an empty set.")
		return
	}
	rpln("")
	rpln("-- scheme (ADR-026 decision A) ----------------------------------------")
	for _, label := range sortedByCount(a.bySchemeImgs) {
		rp("%-42s: %5d images across %d hosts\n",
			label, a.bySchemeImgs[label], len(a.bySchemeHosts[label]))
	}
	http := a.bySchemeImgs[schemeLabel("http")]
	rpln("")
	rp("  The http row is the count of images that render ONLY because the proxy\n")
	rp("  exists: the client restores a parked URL into src= and the browser\n")
	rp("  blocks plain http on an https page. Here that is %d image(s).\n", http)
	if http == 0 {
		rpln("  ⚠ ZERO. Decision A's widening buys nothing measurable on this corpus,")
		rpln("    and could be withdrawn — the ADR says so explicitly.")
	}
	rpln("")
	rpln("-- tracking parameters (ADR-026 decision D) ---------------------------")
	rp("images with any query param  : %d of %d\n", a.imgsAnyParam, a.proxied)
	rp("images with a KNOWN tracking param: %d\n", a.imgsAnyTP)
	rpln("parameter names actually present (names only — a value is the recipient's id):")
	names := sortedByCount(a.paramImgs)
	if top > 0 && len(names) > top {
		rp("  (showing the top %d of %d)\n", top, len(names))
		names = names[:top]
	}
	for _, n := range names {
		mark := " "
		if trackingParams[n] {
			mark = "*"
		}
		rp("  %s %-32s %5d images\n", mark, n, a.paramImgs[n])
	}
	rpln("  * = on the familiar link-tracking list. Stripping is DEFERRED (decision D):")
	rpln("    for images the recipient is usually in the path or a required token, so")
	rpln("    an over-eager rule 404s the picture and fails silently.")
	rpln("")
	rpln("-- hosts --------------------------------------------------------------")
	hosts := sortedByCount(a.hostImgs)
	shown := hosts
	if top > 0 && len(shown) > top {
		shown = shown[:top]
		rp("(showing the top %d of %d)\n", top, len(hosts))
	}
	for _, h := range shown {
		rp("  %-52s %5d images\n", h, a.hostImgs[h])
	}
}

// addPrefetch counts one in-window, proxy-allowed message the way the prefetch
// job would walk it: the SAME list (api.ProxyEligibleImages), the same cap, and
// one fetch per distinct image within the message.
func (a *proxyAggregate) addPrefetch(msg *mime.Message) {
	urls, capped := api.ProxyEligibleImages(msg, api.DefaultProxyPerMessage)
	if capped {
		a.pfMsgsCapped++
	}
	seen := map[string]bool{}
	for _, u := range urls {
		k := string(api.ProxyURLHash(u))
		if seen[k] {
			continue
		}
		seen[k] = true
		a.pfImages++
		a.pfDistinct[k] = true
	}
	if len(seen) > 0 {
		a.pfMsgsWithImages++
	}
}

// measurePrefetchCache asks the cache which of the would-be-prefetched images it
// already holds, and reads its current size split. Read-only: the lookup does
// not advance last_used_at.
func (a *proxyAggregate) measurePrefetchCache(ctx context.Context, d *deps, userID ulid.ULID) error {
	err := d.store.UserTx(ctx, userID, func(tx *store.UserTx) error {
		for k := range a.pfDistinct {
			if _, found, e := tx.ProxyImageCachedType(ctx, []byte(k)); e != nil {
				return e
			} else if found {
				a.pfCached++
			}
		}
		var e error
		a.pfCache, e = tx.ProxyCacheBreakdown(ctx)
		return e
	})
	if err != nil {
		return fmt.Errorf("proxy-report: prefetch cache lookup: %w", err)
	}
	a.pfMeasured = true
	return nil
}

func (a *proxyAggregate) printPrefetch(window time.Duration) {
	rpln("")
	rpln("-- the arrival prefetch (#922; MAILROOM_IMAGE_PROXY_PREFETCH) --------")
	rp("window                       : mail received since %s (%s)\n",
		a.pfWindowStart.UTC().Format(time.RFC3339), window)
	rp("messages in the window       : %d  (any sender)\n", a.pfMsgsInWindow)
	rp("  from senders the PROXY allows: %d\n", a.pfMsgsAllowed)
	rp("  ...with an image to fetch  : %d\n", a.pfMsgsWithImages)
	rp("  ...cut off by the %d-image cap: %d\n", api.DefaultProxyPerMessage, a.pfMsgsCapped)
	rp("  unreadable (NOT counted)   : %d\n", a.pfUnreadableInWin)
	rp("images (distinct per message): %d  (beacons never included, §3d)\n", a.pfImages)
	rp("distinct images              : %d\n", len(a.pfDistinct))
	rp("  already cached             : %d  (a reference is added; no fetch)\n", a.pfCached)
	rp("  WOULD BE FETCHED           : %d\n", len(a.pfDistinct)-a.pfCached)
	if a.pfMsgsInWindow == 0 {
		rpln("  ⚠ Nothing arrived inside the window, so zero here says nothing about")
		rpln("    the prefetch — only that there is no recent mail to measure.")
	} else if a.pfMsgsAllowed == 0 {
		rpln("  Zero allowed senders in the window: the prefetch would fetch nothing")
		rpln("  until a sender is allowed. That is the rule working, not a fault.")
	}
	rpln("  This counts what a prefetch run over the window WOULD fetch; live mail")
	rpln("  is prefetched as it arrives. Nothing here contacted any sender.")
	if a.pfMeasured {
		rpln("")
		rpln("cache now (bytes are the images' plain size):")
		rp("  referenced   (kept while a message is kept): %6d images, %s\n",
			a.pfCache.ReferencedEntries, humanBytes(a.pfCache.ReferencedBytes))
		rp("  unreferenced (cap + 30-day unread TTL)     : %6d images, %s\n",
			a.pfCache.UnreferencedEntries, humanBytes(a.pfCache.UnreferencedBytes))
		rpln("  ⚠ Referenced images are evicted by NOTHING but the deletion of the")
		rpln("    last message that shows them; the cap bounds the unreferenced row only.")
	}
}

// resolveHosts answers §6's last row: would the SSRF guard ever fire on real
// mail, or only on the hypothetical.
func (a *proxyAggregate) resolveHosts(ctx context.Context) {
	a.resolved = true
	res := net.DefaultResolver
	for host := range a.hostImgs {
		addrs, err := res.LookupNetIP(ctx, "ip", host)
		if err != nil || len(addrs) == 0 {
			reason := "no addresses"
			if err != nil {
				reason = err.Error()
			}
			a.hostUnresolved[host] = reason
			continue
		}
		blocked := ""
		for _, ad := range addrs {
			if cerr := egress.CheckAddress(ad); cerr != nil {
				blocked = cerr.Error()
				break
			}
		}
		if blocked != "" {
			a.hostBlocked[host] = blocked
			continue
		}
		a.hostPublic[host] = true
	}
}

func (a *proxyAggregate) printResolution(top int) {
	rpln("")
	rpln("-- would the SSRF guard ever fire? (ADR-026 §6, the row not to skip) ---")
	rp("hosts resolved, all public   : %d\n", len(a.hostPublic))
	rp("hosts with a NON-PUBLIC address: %d\n", len(a.hostBlocked))
	rp("hosts that did not resolve   : %d  (⚠ NOT counted as safe — unknown, not public)\n",
		len(a.hostUnresolved))

	if len(a.hostBlocked) == 0 {
		rpln("")
		rpln("⚠ ZERO HOSTS RESOLVE SOMEWHERE NON-PUBLIC TODAY. Read this precisely:")
		rpln("  it means the guard has not been OBSERVED to refuse real mail, not that")
		rpln("  it is unnecessary. The guard defends against a URL an attacker chooses")
		rpln("  and can change after this measurement — a sender controls both the")
		rpln("  hostname and the DNS record behind it. This number is a statement")
		rpln("  about today's corpus, and it is a lower bound on nothing.")
	} else {
		i := 0
		for host, why := range a.hostBlocked {
			if top > 0 && i >= top {
				rp("  ... and %d more\n", len(a.hostBlocked)-i)
				break
			}
			rp("  %-40s %d images — %s\n", host, a.hostImgs[host], why)
			i++
		}
	}
	if len(a.hostUnresolved) > 0 {
		rp("\n  unresolved hosts are %d of %d (%.1f%%) — dead senders, or DNS from here\n",
			len(a.hostUnresolved), len(a.hostImgs),
			100*float64(len(a.hostUnresolved))/float64(len(a.hostImgs)))
	}
}

// fetchSizes sets the byte cap and cache budget from evidence, through the real
// egress guard — its first exercise against the open internet.
func (a *proxyAggregate) fetchSizes(ctx context.Context, d *deps, max int) {
	a.fetched = true
	client := egress.New(egress.Policy{
		// Both schemes: decision A. The point is to measure what the proxy
		// would do, and the proxy takes http.
		Schemes:  []string{"https", "http"},
		Timeout:  15 * time.Second,
		MaxBytes: 16 << 20,
	})
	rp("\nfetching up to %d images through internal/egress...\n", max)
	for _, raw := range a.fetchURLs {
		if max > 0 && a.fetchTried >= max {
			break
		}
		if ctx.Err() != nil {
			break
		}
		a.fetchTried++
		failHost := hostOf(raw)
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, raw, nil)
		if err != nil {
			a.fetchFailed["bad request"]++
			a.fetchFailedHost[failHost]++
			continue
		}
		resp, err := client.Do(req)
		if err != nil {
			a.fetchFailed[classifyFetchErr(err)]++
			a.fetchFailedHost[failHost]++
			continue
		}
		n, rerr := io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		if rerr != nil {
			a.fetchFailed[classifyFetchErr(rerr)]++
			a.fetchFailedHost[failHost]++
			continue
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			a.fetchFailed[fmt.Sprintf("HTTP %d", resp.StatusCode)]++
			a.fetchFailedHost[failHost]++
			continue
		}
		a.fetchOK++
		a.sizes = append(a.sizes, n)
	}
}

// classifyFetchErr keeps a refusal distinct from a network failure. Folding
// them together would make the guard's own effect invisible in the table.
func classifyFetchErr(err error) string {
	switch {
	case errors.Is(err, egress.ErrBlockedAddress):
		return "REFUSED by the guard: non-public address"
	case errors.Is(err, egress.ErrSchemeNotPermitted):
		return "REFUSED by the guard: scheme"
	case errors.Is(err, egress.ErrTooManyRedirects):
		return "REFUSED by the guard: redirects"
	case errors.Is(err, egress.ErrTooLarge):
		return "REFUSED by the guard: over the byte cap"
	default:
		return "network or server error"
	}
}

func (a *proxyAggregate) printSizes() {
	rpln("")
	rpln("-- size distribution (ADR-026 §6; sets the byte cap and cache budget) --")
	rp("attempted                    : %d of %d proxied images\n", a.fetchTried, a.proxied)
	rp("fetched OK                   : %d\n", a.fetchOK)
	if a.fetchTried < a.proxied {
		rp("⚠ BOUNDED: %d images were not attempted (--fetch-max). This is a SAMPLE,\n",
			a.proxied-a.fetchTried)
		rpln("  not the corpus, and the percentiles below describe only what was fetched.")
	}
	if len(a.fetchFailed) > 0 {
		rpln("failures, by kind:")
		for _, k := range sortedByCount(a.fetchFailed) {
			rp("  %-46s %d\n", k, a.fetchFailed[k])
		}
		rpln("failures, by host:")
		for _, h := range sortedByCount(a.fetchFailedHost) {
			rp("  %-46s %d of %d images\n", h, a.fetchFailedHost[h], a.hostImgs[h])
		}
		rpln("  ⚠ A failure concentrated on ONE host usually means signed, expiring")
		rpln("    URLs rather than a broken proxy — look for a token/expiry parameter")
		rpln("    in that host's row above. It is also the case the endpoint must")
		rpln("    render as a broken image rather than retry or fall back.")
	}
	if len(a.sizes) == 0 {
		rpln("no successful fetches — no size distribution to report (this is not a zero-size corpus)")
		return
	}
	sort.Slice(a.sizes, func(i, j int) bool { return a.sizes[i] < a.sizes[j] })
	rp("min / p50 / p90 / p99 / max  : %s / %s / %s / %s / %s\n",
		humanBytes(a.sizes[0]), humanBytes(pct(a.sizes, 50)), humanBytes(pct(a.sizes, 90)),
		humanBytes(pct(a.sizes, 99)), humanBytes(a.sizes[len(a.sizes)-1]))
	var total int64
	for _, s := range a.sizes {
		total += s
	}
	rp("total fetched                : %s over %d images\n", humanBytes(total), len(a.sizes))
	rp("extrapolated corpus size     : ~%s if all %d behaved like this sample\n",
		humanBytes(total/int64(len(a.sizes))*int64(a.proxied)), a.proxied)
}

func pct(sorted []int64, p int) int64 {
	if len(sorted) == 0 {
		return 0
	}
	i := (len(sorted) - 1) * p / 100
	return sorted[i]
}

func humanBytes(n int64) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MiB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KiB", float64(n)/(1<<10))
	default:
		return fmt.Sprintf("%d B", n)
	}
}

// sortedByCount orders keys by descending count, ties broken by name so a run
// is comparable to the previous one.
func sortedByCount[V int](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if m[keys[i]] != m[keys[j]] {
			return m[keys[i]] > m[keys[j]]
		}
		return keys[i] < keys[j]
	})
	return keys
}

// hostOf is the host of a full URL, for attributing a fetch failure. It never
// returns the path or query — see the warning on api.RemoteImage.URL.
func hostOf(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" {
		return "(unparseable)"
	}
	return strings.ToLower(u.Hostname())
}
