// SPDX-License-Identifier: AGPL-3.0-only

package importer

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/ingest"
	"mailroom/internal/store"
)

// DefaultMaxMessageBytes bounds the one message held in memory at a time. It
// matches mime.MaxTotalSize, the parser's own ceiling: a message the parser will
// refuse to fully walk is not worth buffering either. Gmail's own send limit is
// 25 MB (≈34 MB base64-encoded), so this admits every real message and rejects
// the archive corruption that claims to be a 4 GB one.
const DefaultMaxMessageBytes int64 = 64 << 20

// Default checkpoint cadence: whichever comes first. Frequent enough that a crash
// costs seconds of re-reading, rare enough that the fsync is noise next to the
// per-message blob write.
const (
	defaultCheckpointEvery    = 500
	defaultCheckpointInterval = 5 * time.Second
	defaultMaxConsecFailures  = 50
	defaultProgressInterval   = 2 * time.Second
	failureLogBudget          = 20 // distinct failures logged in full; the rest are counted
)

// Options configure an import.
type Options struct {
	UserID ulid.ULID

	// AccountName is the local account imported mail is filed under. It is created
	// with sync_enabled=false and never dials anything (see ensureAccount).
	AccountName string
	// MailboxPrefix namespaces the imported mailboxes ("Imported/Inbox"), so an
	// import can never be confused with — or collide with — a real IMAP folder.
	MailboxPrefix string
	// CredentialsCT/CredentialsDEK are a sealed (empty) credential envelope for the
	// import account. The account has no credentials, but the column is NOT NULL
	// and the restore drill opens accounts[0]'s envelope to prove the key store
	// survived (cmd/mailroom-drill), so it must be a REAL envelope, not zero bytes.
	CredentialsCT  []byte
	CredentialsDEK ulid.ULID

	// DEK/DEKID are the user's mail-blob key and its scheme sentinel, resolved by
	// the caller exactly as the sync path resolves them (syncrunner.MailBlobInfoV1).
	DEK   []byte
	DEKID ulid.ULID

	MaxMessageBytes        int64
	MaxConsecutiveFailures int

	// StatePath is where the resume cursor lives. Empty disables checkpointing.
	StatePath string
	Resume    bool
	DryRun    bool

	Logger           *slog.Logger
	Progress         func(Progress)
	ProgressInterval time.Duration
}

// Stats is the running (and final) tally of an import.
type Stats struct {
	// Items is every message the archive yielded, including the bad ones.
	Items int64 `json:"items"`
	// Imported is new canonical messages created.
	Imported int64 `json:"imported"`
	// Deduped is messages already in the store from an earlier import — the
	// re-run/resume case. Nothing was duplicated.
	Deduped int64 `json:"deduped"`
	// LeftAlone is archived messages whose bytes IMAP sync already owns. They are
	// not touched (see ingest.IngestArchive).
	LeftAlone int64 `json:"leftAlone"`
	// Skipped is items the ARCHIVE could not yield usably: truncated, empty,
	// oversized, unreadable. Counted, never fatal.
	Skipped int64 `json:"skipped"`
	// Failed is items that reached ingest and were rejected (bytes Postgres will
	// not store, a blob write that failed). Counted, never fatal.
	Failed int64 `json:"failed"`
	// Bytes is how much of the archive has been consumed.
	Bytes int64 `json:"bytes"`
}

// Progress is one progress report.
type Progress struct {
	Stats
	BytesTotal  int64
	Elapsed     time.Duration
	MessagesSec float64
	ETA         time.Duration // 0 when unknown (a maildir has no byte total)
}

// Importer bulk-loads an archive through the canonical ingest path.
type Importer struct {
	store *store.Store
	ing   *ingest.Ingestor
	opt   Options
	log   *slog.Logger

	accountID ulid.ULID
	mailboxes map[string]ulid.ULID
	labels    map[string]ulid.ULID

	// Dry-run tallies: what the import WOULD do, per folder and per label.
	folderTally map[string]int64
	labelTally  map[string]int64

	// lastHash is the content hash (hex) of the most recently COMMITTED message.
	// The checkpoint carries it so a resume can prove, against the database, that
	// the cursor still describes reality (Checkpoint.Validate).
	lastHash string

	stats     Stats
	consecErr int
	logged    int
}

// New builds an Importer. It does not touch the database.
func New(st *store.Store, ing *ingest.Ingestor, opt Options) (*Importer, error) {
	if opt.UserID == (ulid.ULID{}) {
		return nil, errors.New("importer: a user id is required")
	}
	if !opt.DryRun && len(opt.DEK) == 0 {
		return nil, errors.New("importer: a mail-blob DEK is required")
	}
	if opt.AccountName == "" {
		opt.AccountName = "Archive (imported)"
	}
	if opt.MailboxPrefix == "" {
		opt.MailboxPrefix = "Imported"
	}
	if opt.MaxMessageBytes <= 0 {
		opt.MaxMessageBytes = DefaultMaxMessageBytes
	}
	if opt.MaxConsecutiveFailures <= 0 {
		opt.MaxConsecutiveFailures = defaultMaxConsecFailures
	}
	if opt.ProgressInterval <= 0 {
		opt.ProgressInterval = defaultProgressInterval
	}
	log := opt.Logger
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return &Importer{
		store: st, ing: ing, opt: opt, log: log,
		mailboxes:   map[string]ulid.ULID{},
		labels:      map[string]ulid.ULID{},
		folderTally: map[string]int64{},
		labelTally:  map[string]int64{},
	}, nil
}

// Tallies returns the per-folder and per-label message counts a dry run produced,
// each sorted by descending count. It is what lets an operator see the mapping a
// 25 GB import WOULD make before committing three hours to it.
func (im *Importer) Tallies() (folders, labels []Tally) {
	return sortTally(im.folderTally), sortTally(im.labelTally)
}

// Tally is one row of a dry run's mapping report.
type Tally struct {
	Name  string
	Count int64
}

func sortTally(m map[string]int64) []Tally {
	out := make([]Tally, 0, len(m))
	for k, v := range m {
		out = append(out, Tally{Name: k, Count: v})
	}
	sort.Slice(out, func(a, b int) bool {
		if out[a].Count != out[b].Count {
			return out[a].Count > out[b].Count
		}
		return out[a].Name < out[b].Name
	})
	return out
}

// Run imports every message src yields. It returns the final stats; a returned
// error means the RUN failed (the database went away, the archive is unreadable),
// never that a message did — bad messages are counted in Stats.Skipped/Failed.
//
// Interrupting it (ctx cancellation — Ctrl-C, the OOM killer's warning shot, a
// laptop lid) is a supported outcome: the checkpoint is flushed and the stats are
// returned alongside ctx.Err(), and re-running resumes from there.
func (im *Importer) Run(ctx context.Context, src Source, fp Fingerprint) (Stats, error) {
	started := time.Now()

	if !im.opt.DryRun {
		if err := im.prepare(ctx); err != nil {
			return im.stats, err
		}
	}

	cp := &Checkpoint{
		UserID:  im.opt.UserID.String(),
		Account: im.opt.AccountName,
		Archive: fp,
	}
	if err := im.resume(ctx, src, cp, fp); err != nil {
		return im.stats, err
	}

	var (
		lastReport     = started
		lastCheckpoint = time.Now()
		sinceSaved     int
	)
	flush := func() error {
		if im.opt.StatePath == "" || im.opt.DryRun {
			return nil
		}
		cp.Stats = im.stats
		if err := cp.Save(im.opt.StatePath); err != nil {
			return err
		}
		sinceSaved = 0
		lastCheckpoint = time.Now()
		return nil
	}

	for {
		if err := ctx.Err(); err != nil {
			_ = flush() // best effort: we are already unwinding
			return im.stats, err
		}

		item, err := src.Next()
		switch {
		case errors.Is(err, io.EOF):
			if ferr := flush(); ferr != nil {
				return im.stats, ferr
			}
			im.report(started, src, true)
			return im.stats, nil
		case err != nil && item == nil:
			// The archive itself is unreadable. That IS fatal — unlike a bad
			// message, there is nothing to skip TO.
			_ = flush()
			return im.stats, err
		case err != nil:
			im.stats.Items++
			im.stats.Skipped++
			im.stats.Bytes += item.Bytes
			im.note("skipping an unusable archive item", err)
			cp.Position = item.After
			sinceSaved++
		default:
			im.stats.Items++
			im.stats.Bytes += item.Bytes
			if err := im.ingestOne(ctx, item); err != nil {
				if ctx.Err() != nil {
					_ = flush()
					return im.stats, ctx.Err()
				}
				// ⚠ A SCANNER OUTAGE IS NOT A MESSAGE THAT COULD NOT BE STORED
				// (ADR-027 §11). Falling through to the counter below would burn
				// MaxConsecutiveFailures perfectly good messages, mark them
				// Failed, and then stop with "the database or blob store is
				// probably gone" — pointing the operator at the two components
				// that are working. Fail fast, and name the scanner.
				//
				// It is also the right thing for the mail: the messages are
				// still in the archive file, the checkpoint has not advanced
				// past them, and re-running the import once the scanner is back
				// picks up exactly where this stopped.
				if errors.Is(err, ingest.ErrScannerUnavailable) {
					_ = flush()
					return im.stats, fmt.Errorf("importer: stopping at the first message: %w", err)
				}
				im.stats.Failed++
				im.consecErr++
				im.note("skipping a message ingest could not store", err)
				if im.consecErr >= im.opt.MaxConsecutiveFailures {
					_ = flush()
					return im.stats, fmt.Errorf(
						"importer: %d consecutive failures — stopping (the database or blob store is probably gone, not the mail): %w",
						im.consecErr, err)
				}
			} else {
				im.consecErr = 0
				cp.Position = item.After
				cp.LastContentHash = im.lastHash
				sinceSaved++
			}
		}

		if sinceSaved >= defaultCheckpointEvery || time.Since(lastCheckpoint) >= defaultCheckpointInterval {
			if err := flush(); err != nil {
				return im.stats, err
			}
		}
		if time.Since(lastReport) >= im.opt.ProgressInterval {
			im.report(started, src, false)
			lastReport = time.Now()
		}
	}
}

// note logs a per-message failure — the ordinal and the source position, never
// the message. A 20 GB archive of garbage would otherwise write a 20 GB log.
func (im *Importer) note(msg string, err error) {
	if im.logged >= failureLogBudget {
		return
	}
	im.logged++
	im.log.Warn(msg, "item", im.stats.Items, "err", err)
	if im.logged == failureLogBudget {
		im.log.Warn("further per-message failures will be counted but not logged",
			"budget", failureLogBudget)
	}
}

func (im *Importer) report(started time.Time, src Source, final bool) {
	if im.opt.Progress == nil {
		return
	}
	elapsed := time.Since(started)
	p := Progress{
		Stats:      im.stats,
		BytesTotal: src.Size(),
		Elapsed:    elapsed,
	}
	if secs := elapsed.Seconds(); secs > 0 {
		p.MessagesSec = float64(im.stats.Items) / secs
	}
	// ETA needs a byte total (mbox has one; a maildir does not — see Size).
	if !final && p.BytesTotal > 0 && im.stats.Bytes > 0 && elapsed > 0 {
		rate := float64(im.stats.Bytes) / elapsed.Seconds()
		if rate > 0 {
			remaining := float64(p.BytesTotal - im.stats.Bytes)
			if remaining > 0 {
				p.ETA = time.Duration(remaining/rate) * time.Second
			}
		}
	}
	im.opt.Progress(p)
}

// resume positions src at the saved cursor, if there is a trustworthy one.
func (im *Importer) resume(ctx context.Context, src Source, cp *Checkpoint, fp Fingerprint) error {
	if !im.opt.Resume || im.opt.StatePath == "" || im.opt.DryRun {
		return nil
	}
	saved, err := LoadCheckpoint(im.opt.StatePath)
	if err != nil {
		// A corrupt or foreign checkpoint is not fatal: it is an optimization we
		// decline to use. Say so loudly and start from the top.
		im.log.Warn("ignoring the saved import cursor", "path", im.opt.StatePath, "err", err)
		return nil
	}
	if saved == nil {
		return nil
	}
	if ok, why := saved.Validate(ctx, im.store, im.opt.UserID, fp); !ok {
		im.log.Warn("ignoring the saved import cursor", "path", im.opt.StatePath, "reason", why)
		return nil
	}
	if err := src.Seek(saved.Position); err != nil {
		return err
	}
	cp.Position = saved.Position
	cp.LastContentHash = saved.LastContentHash
	im.stats = saved.Stats
	im.log.Info("resuming import",
		"source", saved.Position.Source, "offset", saved.Position.Offset,
		"name", saved.Position.Name, "already_imported", saved.Stats.Imported)
	return nil
}

// ingestOne classifies one archive item and puts it through canonical ingest.
//
// Each message is its OWN transaction, and that is the answer to the batch-poison
// problem (SQLSTATE 25P02): a message Postgres refuses — a NUL byte smuggled into
// a Subject, say — aborts only its own transaction, so there is no batch for it to
// destroy and no SAVEPOINT to forget. It costs one commit per message, which is
// the same order as the blob fsync ingest already pays, and it buys exact resume
// granularity: whatever is committed is committed.
func (im *Importer) ingestOne(ctx context.Context, item *Item) (err error) {
	// An archive is hostile input by construction. internal/mime already recovers
	// panics on its own (mime/safe.go), but nothing else in the chain promises to,
	// and a panic 400,000 messages into a 20 GB import would be an expensive way to
	// find that out.
	defer func() {
		if rec := recover(); rec != nil {
			err = fmt.Errorf("importer: recovered while ingesting an archive message: %v", rec)
		}
	}()

	folder, flags, labels := im.classify(item)

	if im.opt.DryRun {
		im.folderTally[folder]++
		for _, l := range labels {
			im.labelTally[l]++
		}
		im.stats.Imported++
		return nil
	}

	mailboxID, err := im.mailboxID(ctx, folder)
	if err != nil {
		return err
	}
	labelIDs, err := im.labelIDs(ctx, labels)
	if err != nil {
		return err
	}

	res, err := im.ing.IngestArchive(ctx, im.opt.UserID, im.opt.DEK, im.opt.DEKID, item.Raw, ingest.Archive{
		MailboxID:  mailboxID,
		ReceivedAt: item.ReceivedAt,
		Flags:      flags,
		LabelIDs:   labelIDs,
	})
	if err != nil {
		return err
	}
	im.lastHash = hex.EncodeToString(res.ContentHash[:])
	switch {
	case res.Created:
		im.stats.Imported++
		im.folderTally[folder]++
		for _, l := range labels {
			im.labelTally[l]++
		}
	case res.LeftAlone:
		im.stats.LeftAlone++
	default:
		im.stats.Deduped++
	}
	return nil
}

// classify merges what the archive's LAYOUT said about a message (the reader's
// folder and flags) with what the message's own HEADERS say (Gmail's labels, an
// mbox's Status:). The headers win on folder, because in a Takeout the file is one
// undifferentiated "All mail" and the labels are the only structure there is.
func (im *Importer) classify(item *Item) (folder string, flags, labels []string) {
	folder = item.Folder
	flags = item.Flags

	if gf, gflags, glabels, ok := classifyGmail(headerValue(item.Raw, gmailLabelsHeader)); ok {
		folder = gf
		flags = append(flags, gflags...)
		labels = glabels
	} else if len(flags) == 0 {
		// A plain mbox carries its flags in Status:/X-Status: or not at all.
		flags = statusFlags(headerValue(item.Raw, "Status"), headerValue(item.Raw, "X-Status"))
	}
	return folder, dedupStrings(flags), dedupStrings(labels)
}

func dedupStrings(in []string) []string {
	if len(in) < 2 {
		return in
	}
	seen := make(map[string]bool, len(in))
	out := in[:0]
	for _, s := range in {
		if seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}

// prepare resolves the import account and primes the label cache.
func (im *Importer) prepare(ctx context.Context) error {
	if err := im.ensureAccount(ctx); err != nil {
		return err
	}
	return im.store.UserTx(ctx, im.opt.UserID, func(tx *store.UserTx) error {
		labels, err := tx.ListLabels(ctx)
		if err != nil {
			return err
		}
		for _, l := range labels {
			im.labels[l.Name] = l.ID
		}
		return nil
	})
}

// ensureAccount finds or creates the local account imported mail is filed under.
//
// An account exists only because app.mailboxes.account_id is NOT NULL — imported
// mail has no IMAP account and never will. It is created with sync_enabled=false,
// so store.ListSyncableAccounts (the only thing the sync runner selects on) never
// returns it: nothing ever dials it, nothing ever polls its mailboxes, and the
// expunge reaper — which only ever runs against a mailbox it has just polled —
// therefore never sees the imported mail at all. Its endpoints are .invalid
// (RFC 2606) so a bug that tried to dial it would fail to resolve rather than
// reach somebody's real server.
func (im *Importer) ensureAccount(ctx context.Context) error {
	return im.store.UserTx(ctx, im.opt.UserID, func(tx *store.UserTx) error {
		accts, err := tx.ListAccounts(ctx)
		if err != nil {
			return err
		}
		for _, a := range accts {
			if a.Name == im.opt.AccountName {
				im.accountID = a.ID
				if a.SyncEnabled {
					return fmt.Errorf("importer: account %q has sync ENABLED — refusing to import into it; "+
						"imported mail must never be written back to an IMAP server", a.Name)
				}
				return nil
			}
		}
		if len(im.opt.CredentialsCT) == 0 {
			return errors.New("importer: a sealed credential envelope is required to create the import account")
		}
		id, err := tx.CreateAccount(ctx, store.AccountRow{
			Name:            im.opt.AccountName,
			ProviderProfile: "generic",
			IMAPHost:        "archive.invalid",
			SMTPHost:        "archive.invalid",
			AuthKind:        "password",
			CredentialsCT:   im.opt.CredentialsCT,
			CredentialsDEK:  im.opt.CredentialsDEK,
			EnvelopeVer:     1,
			SyncEnabled:     false,
		})
		if err != nil {
			return err
		}
		im.accountID = id
		im.log.Info("created the local archive account", "account", im.opt.AccountName, "id", id)
		return nil
	})
}

// mailboxID resolves (and creates on first sight) the local mailbox an archive
// folder maps to. Names are prefixed ("Imported/Inbox") and carry NO SPECIAL-USE:
// giving an imported folder the \Archive or \Trash role would put it in the
// running when Email/set resolves a move by role (store.MailboxForRole), and the
// user's real Archive is where their real archive lives. Imported folders are
// destinations you navigate to, not roles the system routes to.
func (im *Importer) mailboxID(ctx context.Context, folder string) (ulid.ULID, error) {
	if id, ok := im.mailboxes[folder]; ok {
		return id, nil
	}
	name := im.opt.MailboxPrefix + "/" + strings.Trim(folder, "/")
	var id ulid.ULID
	err := im.store.UserTx(ctx, im.opt.UserID, func(tx *store.UserTx) error {
		st, err := tx.UpsertMailbox(ctx, store.MailboxUpsert{
			AccountID:   im.accountID,
			IMAPName:    name,
			DisplayName: name,
		})
		if err != nil {
			return err
		}
		id = st.ID
		return nil
	})
	if err != nil {
		return ulid.ULID{}, err
	}
	im.mailboxes[folder] = id
	return id, nil
}

// labelIDs resolves (and creates on first sight) the Mailroom labels an archive's
// labels map to. A Gmail label becomes a Mailroom label of the same name (spec §6:
// labels live in Postgres and mirror to IMAP keywords) — not a mailbox, because
// that is what it IS: Gmail's folders are the six special ones, and everything
// else the user made is a label.
func (im *Importer) labelIDs(ctx context.Context, names []string) ([]ulid.ULID, error) {
	if len(names) == 0 {
		return nil, nil
	}
	out := make([]ulid.ULID, 0, len(names))
	for _, name := range names {
		if id, ok := im.labels[name]; ok {
			out = append(out, id)
			continue
		}
		var id ulid.ULID
		err := im.store.UserTx(ctx, im.opt.UserID, func(tx *store.UserTx) error {
			lid, err := tx.CreateLabel(ctx, store.LabelRow{Name: name, MirrorToIMAP: true})
			switch {
			case errors.Is(err, store.ErrLabelNameTaken):
				// Someone (a previous run, a client) already has it. Re-read rather
				// than guess: the mapping is by name.
				labels, lerr := tx.ListLabels(ctx)
				if lerr != nil {
					return lerr
				}
				for _, l := range labels {
					im.labels[l.Name] = l.ID
					if l.Name == name {
						id = l.ID
					}
				}
				if id == (ulid.ULID{}) {
					return fmt.Errorf("importer: label %q exists but cannot be found", name)
				}
				return nil
			case err != nil:
				return err
			}
			id = lid
			return nil
		})
		if err != nil {
			return nil, err
		}
		im.labels[name] = id
		out = append(out, id)
	}
	return out, nil
}
