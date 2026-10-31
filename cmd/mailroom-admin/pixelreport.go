// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"context"
	"flag"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/api"
	"mailroom/internal/blob"
	"mailroom/internal/crypto"
	"mailroom/internal/mime"
	"mailroom/internal/store"
	"mailroom/internal/syncrunner"
)

// `mailroom-admin pixel-report` — how much tracking is actually reachable in
// this mailbox, and for whom (#55 increment 2).
//
// WHY A REPORT AND NOT A DETECTOR. #55 asks for "tracking-pixel detection
// independent of full remote-image blocking". Detection implies a rule, and
// there is no agreed rule: 1×1, "small", hidden by CSS, and "no dimensions at
// all" select overlapping and very differently sized populations. Writing the
// rule first and counting afterwards produces a number the rule was fitted to
// (#177, #191, #206). So this counts every candidate rule SEPARATELY and leaves
// the choice to a person reading the table.
//
// ⚠ THE POPULATION THAT MATTERS IS NOT "MESSAGES WITH A PIXEL". Remote content
// is blocked by default, so a beacon in a blocked message never fires — it is
// inert markup. The number that says what increment 2 would actually buy is
// pixels in messages whose sender the user has ALREADY allowed, because those
// are the ones that fetch on open. The report leads with the whole corpus for
// context and then narrows to that, and the two are never added together.
//
// ⚠ IT MEASURES THE SHIPPED SANITIZER. Counts come from api.MeasureHTMLRemoteImages,
// which threads a collector through the real sanitizeFragment; a test in
// internal/api pins its output byte-identical to SanitizeHTML. Re-walking the
// HTML here with a second parser would be a measurement of code written by the
// same hand to agree with the same assumptions (#169, #220), and would drift the
// moment the sanitizer's idea of "remote" changed.
//
// ⚠ ONLY <img> CAN DO THIS AT ALL, which is worth stating because it bounds the
// whole question. Every other way a message could fetch something — background,
// poster, CSS url(), <link>, <iframe>, <video> — is DROPPED outright by the
// sanitizer rather than parked, so it is not restorable by any affordance and
// cannot track anyone even if the user allows remote content. A "tracking pixel"
// in Mailroom is therefore exactly a parked <img>, and this report is complete
// over that set rather than being a sample of a larger one.
//
// READ-ONLY. There is no --apply: it writes nothing and proposes nothing.
func runPixelReport(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("pixel-report", flag.ContinueOnError)
	userFlag := fs.String("user-id", "", "user whose corpus to measure (required)")
	top := fs.Int("top", 15, "show this many hosts (0 = all)")
	batch := fs.Int("batch", 500, "messages per page of the keyset walk")
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
		return fmt.Errorf("pixel-report: master key: %w", err)
	}
	dek, err := crypto.DeriveKey(master, userID[:], syncrunner.MailBlobInfoV1)
	crypto.Zero(master)
	if err != nil {
		return fmt.Errorf("pixel-report: derive mail dek: %w", err)
	}
	defer crypto.Zero(dek)

	// The grants as they stand right now. They are read ONCE: a report that
	// re-read them per message could describe two different states in one table.
	var allows []store.RemoteAllow
	if err := d.store.UserTx(ctx, userID, func(tx *store.UserTx) error {
		var e error
		allows, e = tx.ListRemoteAllows(ctx)
		return e
	}); err != nil {
		return fmt.Errorf("pixel-report: list remote allows: %w", err)
	}

	agg, err := scanCorpusPixels(ctx, d, blobs, userID, dek, allows, *batch)
	if err != nil {
		return err
	}
	agg.print(allows, *top)
	return nil
}

// pixelSignals is one image's classification under every candidate rule. A image
// can satisfy several at once, which is why they are booleans on one struct
// rather than a single category — collapsing them to a category would force an
// ordering nobody has agreed on.
type pixelSignals struct {
	oneByOne bool // both dimensions declared and <= 1
	tiny     bool // either declared dimension <= 3, excluding oneByOne
	hidden   bool // inline style that renders it invisible
	noDims   bool // neither dimension declared at all
}

// any reports whether this image matched any rule at all.
func (s pixelSignals) any() bool { return s.oneByOne || s.tiny || s.hidden || s.noDims }

// classifyPixel applies every candidate rule to one image's declared attributes.
//
// ⚠ IT IS DELIBERATELY GENEROUS AND THE REPORT SAYS SO. `noDims` in particular
// is a weak signal — most ordinary content images in mail declare no width or
// height either — and it is counted so a reader can see how much the "anything
// undimensioned is suspicious" rule would sweep up, not because it is a good
// rule. Presenting it beside the strict ones is the point.
func classifyPixel(img api.RemoteImage) pixelSignals {
	w, wOK := parseDim(img.Width)
	h, hOK := parseDim(img.Height)

	var s pixelSignals
	switch {
	// ⚠ THE 1x1 ROW IS THE SHIPPED RULE, not a copy of it. api.IsTrackingPixel is
	// what the sanitizer uses to decide which images to park where the client
	// cannot restore them (#55 increment 2), so this row says what the product
	// actually does. The rows below it are candidates that were CONSIDERED and NOT
	// adopted, and keeping that difference visible is the report's job — a reader
	// comparing them is comparing the shipped rule against its alternatives.
	case api.IsTrackingPixel(img):
		s.oneByOne = true
	case (wOK && w <= 3) || (hOK && h <= 3):
		s.tiny = true
	}
	s.hidden = styleHides(img.Style)
	s.noDims = !wOK && !hOK
	return s
}

// parseDim reads an HTML dimension attribute. Returns ok=false when the
// attribute was absent or is not a plain number — "50%" is a real width and is
// emphatically not a pixel, so it must not read as one.
func parseDim(v string) (int, bool) {
	t := strings.TrimSpace(strings.ToLower(v))
	if t == "" {
		return 0, false
	}
	t = strings.TrimSuffix(t, "px")
	n, err := strconv.Atoi(strings.TrimSpace(t))
	if err != nil || n < 0 {
		return 0, false
	}
	return n, true
}

// styleHides reports whether an inline style makes the image invisible.
//
// Deliberately a substring test over a whitespace-stripped copy rather than a
// CSS parse: the sanitizer has already decided what survives, and the question
// here is only "did the sender ask for this to be invisible". A false positive
// costs one row in a report; a CSS parser in a report costs a second
// implementation of the thing the sanitizer already does.
func styleHides(style string) bool {
	s := strings.ToLower(style)
	s = strings.NewReplacer(" ", "", "\t", "", "\n", "", "\r", "").Replace(s)
	for _, needle := range []string{
		"display:none", "visibility:hidden", "opacity:0", "opacity:0.0",
		"width:0", "height:0", "max-height:0", "max-width:0",
	} {
		if strings.Contains(s, needle) {
			// opacity:0 must not match opacity:0.5 — check the character after.
			if strings.HasPrefix(needle, "opacity:0") || strings.HasSuffix(needle, ":0") {
				if !boundaryAfter(s, needle) {
					continue
				}
			}
			return true
		}
	}
	return false
}

// boundaryAfter reports whether every occurrence of needle in s is followed by
// something that ends the value — so `width:0` matches `width:0;` and `width:0`
// at the end, but not `width:0.5em` or `width:00`.
func boundaryAfter(s, needle string) bool {
	from := 0
	for {
		i := strings.Index(s[from:], needle)
		if i < 0 {
			return false
		}
		end := from + i + len(needle)
		if end >= len(s) {
			return true
		}
		if c := s[end]; c == ';' || c == '}' || c == '"' || c == '\'' {
			return true
		}
		from = end
	}
}

// pixelAggregate accumulates the corpus.
//
// Every population is held as BOTH an image count and a set of message ids,
// because "four hundred beacons" and "four hundred messages carrying one" call
// for different responses and one number cannot tell them apart — the same
// separation css-report keeps between DECLS and MESSAGES.
type pixelAggregate struct {
	scanned       int
	unreadable    int
	withHTML      int
	htmlParts     int
	nestedSkipped int

	// Whole corpus.
	images       int
	imagesSrcset int
	msgsWithImg  map[ulid.ULID]bool
	sig          map[string]int
	sigMsgs      map[string]map[ulid.ULID]bool
	hostMsgs     map[string]map[ulid.ULID]bool

	// The exposed subset: messages whose sender is ALREADY allowed, so their
	// remote content loads on open without asking.
	exposedMsgs    map[ulid.ULID]bool
	exposedImages  int
	exposedSig     map[string]int
	exposedSigMsgs map[string]map[ulid.ULID]bool
	// perSender is keyed by the grant that allowed it, so the operator can see
	// which remembered decision is carrying the exposure.
	perSenderMsgs map[string]map[ulid.ULID]bool
	perSenderPix  map[string]int
}

// signalNames is the fixed row order. A stable order matters because this table
// is read across runs to see whether anything moved.
var signalNames = []string{"1x1", "tiny (<=3px)", "hidden by style", "no dimensions", "ANY of the above"}

func newPixelAggregate() *pixelAggregate {
	a := &pixelAggregate{
		msgsWithImg:    map[ulid.ULID]bool{},
		sig:            map[string]int{},
		sigMsgs:        map[string]map[ulid.ULID]bool{},
		hostMsgs:       map[string]map[ulid.ULID]bool{},
		exposedMsgs:    map[ulid.ULID]bool{},
		exposedSig:     map[string]int{},
		exposedSigMsgs: map[string]map[ulid.ULID]bool{},
		perSenderMsgs:  map[string]map[ulid.ULID]bool{},
		perSenderPix:   map[string]int{},
	}
	for _, n := range signalNames {
		a.sigMsgs[n] = map[ulid.ULID]bool{}
		a.exposedSigMsgs[n] = map[ulid.ULID]bool{}
	}
	return a
}

func mark(m map[string]map[ulid.ULID]bool, key string, id ulid.ULID) {
	if m[key] == nil {
		m[key] = map[ulid.ULID]bool{}
	}
	m[key][id] = true
}

// add folds one image into the corpus and, when its sender is allowed, into the
// exposed subset as well.
func (a *pixelAggregate) add(id ulid.ULID, img api.RemoteImage, exposedBy string) {
	a.images++
	if img.FromSrcset {
		a.imagesSrcset++
	}
	a.msgsWithImg[id] = true
	if img.Host != "" {
		mark(a.hostMsgs, img.Host, id)
	}

	s := classifyPixel(img)
	exposed := exposedBy != ""
	if exposed {
		a.exposedImages++
		a.exposedMsgs[id] = true
		a.perSenderPix[exposedBy]++
		mark(a.perSenderMsgs, exposedBy, id)
	}
	for name, on := range map[string]bool{
		signalNames[0]: s.oneByOne,
		signalNames[1]: s.tiny,
		signalNames[2]: s.hidden,
		signalNames[3]: s.noDims,
		signalNames[4]: s.any(),
	} {
		if !on {
			continue
		}
		a.sig[name]++
		a.sigMsgs[name][id] = true
		if exposed {
			a.exposedSig[name]++
			a.exposedSigMsgs[name][id] = true
		}
	}
}

func scanCorpusPixels(
	ctx context.Context,
	d *deps,
	blobs blob.BlobStore,
	userID ulid.ULID,
	dek []byte,
	allows []store.RemoteAllow,
	batch int,
) (*pixelAggregate, error) {
	agg := newPixelAggregate()
	after := ulid.ULID{}

	for {
		// MessageAuthRefsAfter rather than a third near-identical walk: it
		// already returns exactly id + content hash + from address, which is what
		// this needs. Its other fields are ignored here.
		var page []store.MessageAuthRef
		if err := d.store.UserTx(ctx, userID, func(tx *store.UserTx) error {
			var e error
			page, e = tx.MessageAuthRefsAfter(ctx, after, batch)
			return e
		}); err != nil {
			return nil, fmt.Errorf("pixel-report: walk messages: %w", err)
		}
		if len(page) == 0 {
			break
		}
		for _, ref := range page {
			after = ref.ID
			agg.scanned++

			raw, err := readMailBlob(ctx, blobs, userID, dek, ref.ContentHash)
			if err != nil {
				// NOT silently skipped: a body we could not read is a message
				// this report did not measure, and calling the corpus clean on
				// evidence never seen is the failure mode.
				agg.unreadable++
				d.log.Warn("pixel-report: read blob failed", "message", ref.ID, "err", err)
				continue
			}
			msg, err := mime.ParseBytes(raw)
			if err != nil {
				agg.unreadable++
				d.log.Warn("pixel-report: parse failed", "message", ref.ID, "err", err)
				continue
			}
			if len(msg.Nested) > 0 {
				agg.nestedSkipped++
			}

			exposedBy := ""
			if g := store.MatchingRemoteAllow(ref.FromAddress, allows); g != nil {
				exposedBy = string(g.Kind) + " " + g.Value
			}

			sawHTML := false
			for _, p := range msg.Parts {
				if p.ContentType != "text/html" {
					continue
				}
				sawHTML = true
				agg.htmlParts++
				_, imgs := api.MeasureHTMLRemoteImages(string(p.Data))
				for _, img := range imgs {
					agg.add(ref.ID, img, exposedBy)
				}
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

func (a *pixelAggregate) print(allows []store.RemoteAllow, top int) {
	rp("scanned                      : %d messages\n", a.scanned)
	rp("unreadable                   : %d  (blob or MIME failure — NOT counted as clean)\n", a.unreadable)
	rp("with a text/html part        : %d\n", a.withHTML)
	rp("text/html parts measured     : %d\n", a.htmlParts)
	rp("nested messages NOT walked   : %d  (a forwarded .eml's own images are outside this report)\n",
		a.nestedSkipped)
	rpln()

	rp("remote images parked         : %d  (of which from srcset: %d)\n", a.images, a.imagesSrcset)
	rp("messages carrying one        : %d\n", len(a.msgsWithImg))
	rpln("  ONLY <img> can fetch at all — background, poster, CSS url(), <link>, <iframe>")
	rpln("  and the rest are dropped outright, not parked, so they cannot track anyone.")
	rpln()

	if a.images == 0 {
		rpln("⚠ NOT ONE remote image was found in any readable message. Every zero below is")
		rpln("  that, and nothing else — it is not evidence that this mailbox is untracked.")
		rpln()
		return
	}

	rpln("=== candidate rules, over the WHOLE corpus =================================")
	rpln("  an image can match several rules; rows do not sum")
	rp("  %-18s %8s %10s\n", "RULE", "IMAGES", "MESSAGES")
	for _, n := range signalNames {
		rp("  %-18s %8d %10d\n", n, a.sig[n], len(a.sigMsgs[n]))
	}
	rpln()

	rpln("=== the population that can ACTUALLY fetch =================================")
	rpln("  remote content is blocked by default, so a beacon in a blocked message is")
	rpln("  inert markup. These are messages whose sender is ALREADY allowed.")
	if len(allows) == 0 {
		rpln()
		rpln("  ⚠ THERE ARE NO REMOTE-CONTENT GRANTS AT ALL, so this population is empty by")
		rpln("    construction and the zeros below say nothing about tracking. They say the")
		rpln("    default is intact.")
		rpln()
		return
	}
	rp("  grants in force              : %d\n", len(allows))
	rp("  messages from allowed senders carrying a remote image : %d\n", len(a.exposedMsgs))
	rp("  remote images in them        : %d\n", a.exposedImages)
	rpln()
	rp("  %-18s %8s %10s\n", "RULE", "IMAGES", "MESSAGES")
	for _, n := range signalNames {
		rp("  %-18s %8d %10d\n", n, a.exposedSig[n], len(a.exposedSigMsgs[n]))
	}
	rpln()

	rpln("=== which remembered decision carries the exposure =========================")
	type row struct {
		key  string
		msgs int
		pix  int
	}
	rows := make([]row, 0, len(a.perSenderMsgs))
	for k, m := range a.perSenderMsgs {
		rows = append(rows, row{k, len(m), a.perSenderPix[k]})
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].msgs != rows[j].msgs {
			return rows[i].msgs > rows[j].msgs
		}
		return rows[i].key < rows[j].key
	})
	if len(rows) == 0 {
		rpln("  no allowed sender has sent a message carrying a remote image")
	}
	for _, r := range rows {
		rp("  %-52s %4d msg  %5d img\n", r.key, r.msgs, r.pix)
	}
	rpln()

	rpln("=== hosts that would be contacted ==========================================")
	rpln("  host only — the path and query of a tracking URL identify the recipient")
	type hrow struct {
		host string
		msgs int
	}
	hrows := make([]hrow, 0, len(a.hostMsgs))
	for h, m := range a.hostMsgs {
		hrows = append(hrows, hrow{h, len(m)})
	}
	sort.Slice(hrows, func(i, j int) bool {
		if hrows[i].msgs != hrows[j].msgs {
			return hrows[i].msgs > hrows[j].msgs
		}
		return hrows[i].host < hrows[j].host
	})
	shown := len(hrows)
	if top > 0 && top < shown {
		shown = top
	}
	for _, r := range hrows[:shown] {
		rp("  %-52s %4d msg\n", r.host, r.msgs)
	}
	if shown < len(hrows) {
		rp("  … and %d more host(s) not shown (--top=0 for all)\n", len(hrows)-shown)
	}
}
