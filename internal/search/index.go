// SPDX-License-Identifier: AGPL-3.0-only

package search

import (
	"context"
	crand "crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/oklog/ulid/v2"
)

// buildID names a rebuild/replaced index directory: unique and time-sortable, so
// a leftover directory says when it was created. It is not an object id — it
// never leaves the filesystem.
func buildID() string {
	return ulid.MustNew(ulid.Timestamp(time.Now()), crand.Reader).String()
}

// The indexer is a CHANGE-LOG CONSUMER (spec §7, ADR-003):
//
//	"Index updates consume the same per-user per-type change log that feeds JMAP
//	 /changes — one ordered stream, one cursor per consumer. Crash between DB
//	 commit and index write heals by cursor replay (at-least-once + idempotent
//	 upsert by message id)."
//
// So: read a page of Email changes after our cursor, upsert the created/updated
// documents, delete the destroyed ones, THEN persist the new cursor. That order
// is the whole crash-safety argument — a crash anywhere before the cursor write
// replays the same page, and replay is harmless because Upsert is idempotent. A
// crash AFTER the index write but before the cursor write costs one duplicate
// upsert, which is a no-op. There is no ordering that loses a document.
//
// Where the cursor lives: in a meta.json INSIDE the user's index directory, not
// in Postgres. This is deliberate and load-bearing. The index is a disposable
// cache; if it is deleted, the cursor MUST die with it. A cursor in Postgres
// would survive an index that did not, and the indexer would then happily resume
// from a state that claims "caught up" while the index is empty — permanently
// missing every message older than the cursor, with nothing to detect it. Keeping
// the two in one directory makes that failure unrepresentable: lose the index,
// lose the cursor, rebuild from zero.

// metaFile is the name of the cursor/version file inside a user's index dir.
//
// It must NOT be "meta.json": Tantivy stores its own segment metadata under that
// exact name in the same directory, and writeMeta renames over it atomically —
// so a cursor write silently destroyed the index, and every subsequent open
// failed with `missing field "segments"`. The name is namespaced to keep the
// cursor beside the index (see the invariant above) without colliding with the
// engine's own files.
const metaFile = "mailroom-index.json"

// schemaVersion is the index schema version (spec §7: "versioned index +
// rebuild-and-swap"). BUMP IT whenever the fields a Document maps to change:
// every user's index is then rebuilt in the background on next start, the old one
// serving until the swap.
//
// v2 (#712) added the from_domain / to_domain fields. Documents indexed under v1
// have no values in them, so `from:shop.example` would silently match nothing on
// existing mail until reindexed — hence the bump.
//
// v3 (#722/#723) added list (List-Id), size (larger:/smaller:), filename,
// deliveredto / deliveredto_domain (alias:), and the flag atoms unsubscribe and
// the attachment families (has:pdf, …). Until a user's index is rebuilt, a query
// that needs any of them gets ErrIndexUpgrading (RequiredSchemaVersion) while
// every older kind of query keeps being served by the old index.
const schemaVersion = 3

// Default indexer tuning.
const (
	DefaultBatch    = 500             // change-log rows (and messages) per round trip
	DefaultInterval = 5 * time.Second // catch-up period when idle
	dirMode         = 0o700           // per-user index dirs: owner only (spec §7)
)

// Changes is one page of the Email change stream, already collapsed per object
// by the store (data-model §3.2). Upsert holds created ∪ updated ids; Delete
// holds destroyed ids.
type Changes struct {
	NewState int64
	HasMore  bool
	Upsert   []ulid.ULID
	Delete   []ulid.ULID
}

// Source is the canonical store as the indexer sees it. Implemented by
// StoreSource over *store.Store; a fake satisfies it in tests, which is why the
// indexer's own tests need no database.
type Source interface {
	// EmailState is the current Email change state for the user (the head of the
	// stream). A reindex reads this BEFORE walking, so nothing committed during
	// the walk can be missed.
	EmailState(ctx context.Context, userID ulid.ULID) (int64, error)

	// EmailChanges returns the collapsed Email changes after `since`, at most
	// max objects. It must return ErrCursorTooOld when `since` is below the
	// change_log compaction floor — the signal to reindex from scratch.
	EmailChanges(ctx context.Context, userID ulid.ULID, since int64, max int) (Changes, error)

	// Messages returns the canonical projections for ids. Ids that are absent
	// (destroyed between the change row and this read) are simply not returned.
	Messages(ctx context.Context, userID ulid.ULID, ids []ulid.ULID) ([]Message, error)

	// MessageIDsAfter walks every message id for the user in ascending id order —
	// the reindex scan. after is the exclusive low bound (zero to start).
	MessageIDsAfter(ctx context.Context, userID ulid.ULID, after ulid.ULID, limit int) ([]ulid.ULID, error)
}

// ErrCursorTooOld is returned by Source.EmailChanges when the cursor is below the
// change-log compaction floor: the deltas needed to catch up no longer exist, so
// the only correct move is a full reindex. (This is the indexer's version of
// JMAP's cannotCalculateChanges — same stream, same floor, same fallback.)
var ErrCursorTooOld = errors.New("search: index cursor is below the change-log floor")

// Content fetches the decrypted raw RFC 5322 bytes of a message. The body and the
// recipient list exist nowhere else (see Message).
type Content interface {
	Raw(ctx context.Context, userID ulid.ULID, contentHash []byte) ([]byte, error)
}

// meta is the persisted state of one user's index: the change-log cursor plus the
// schema version the index was built with.
type meta struct {
	SchemaVersion int       `json:"schema_version"`
	Cursor        int64     `json:"cursor"`
	UpdatedAt     time.Time `json:"updated_at"`
	// Rebuilding marks an index whose reindex was interrupted. It is never
	// trusted for serving cursors: a set flag forces another reindex.
	Rebuilding bool `json:"rebuilding,omitempty"`
}

// userIndex is one user's open index: the backend, its cursor, and the lock that
// serializes writes against searches.
type userIndex struct {
	mu      sync.RWMutex
	dir     string
	backend SearchBackend
	meta    meta
}

// Indexer owns every user's index directory and drives them from the change log.
type Indexer struct {
	root    string
	open    BackendFactory
	src     Source
	content Content
	log     *slog.Logger
	// syncSink, when set, receives each catch-up pass Progress (see WithSyncSink).
	syncSink func(userID ulid.ULID, p Progress)
	batch    int
	interval time.Duration

	mu    sync.Mutex
	users map[ulid.ULID]*userIndex
}

// Option configures an Indexer.
type Option func(*Indexer)

// WithLogger sets the logger (default slog.Default()).
func WithLogger(l *slog.Logger) Option {
	return func(ix *Indexer) {
		if l != nil {
			ix.log = l
		}
	}
}

// WithBatch sets how many changes/messages are processed per round trip.
func WithBatch(n int) Option {
	return func(ix *Indexer) {
		if n > 0 {
			ix.batch = n
		}
	}
}

// WithInterval sets the idle catch-up period of Run.
func WithInterval(d time.Duration) Option {
	return func(ix *Indexer) {
		if d > 0 {
			ix.interval = d
		}
	}
}

// NewIndexer creates the index root (0700) and returns an Indexer over it.
// `open` chooses the engine: tantivy.Open in production, MemoryFactory in tests
// and in a build without the tantivy tag.
func NewIndexer(root string, open BackendFactory, src Source, content Content, opts ...Option) (*Indexer, error) {
	if root == "" {
		return nil, errors.New("search: index root is required")
	}
	if open == nil || src == nil {
		return nil, errors.New("search: a backend factory and a source are required")
	}
	if err := os.MkdirAll(root, dirMode); err != nil {
		return nil, fmt.Errorf("search: create index root %s: %w", root, err)
	}
	// MkdirAll honors umask and does nothing to an existing directory, so state
	// the mode explicitly: the index holds the plaintext of every message,
	// including mail that arrived encrypted (spec §7).
	if err := os.Chmod(root, dirMode); err != nil {
		return nil, fmt.Errorf("search: secure index root %s: %w", root, err)
	}

	ix := &Indexer{
		root:     root,
		open:     open,
		src:      src,
		content:  content,
		log:      slog.Default(),
		batch:    DefaultBatch,
		interval: DefaultInterval,
		users:    make(map[ulid.ULID]*userIndex),
	}
	for _, o := range opts {
		o(ix)
	}
	return ix, nil
}

// userRoot is the per-user directory; live is the index inside it. A user's
// index NEVER shares a directory with another's — that is the isolation control
// (spec §7), and the 0700 mode is what enforces it against everything but this
// process.
func (ix *Indexer) userRoot(userID ulid.ULID) string { return filepath.Join(ix.root, userID.String()) }
func (ix *Indexer) liveDir(userID ulid.ULID) string {
	return filepath.Join(ix.userRoot(userID), "live")
}

// index returns the user's open index, opening (and creating) it on first use.
// A zero user id is rejected: fail closed, exactly as store.UserTx does — no
// identity, no index.
func (ix *Indexer) index(ctx context.Context, userID ulid.ULID) (*userIndex, error) {
	if userID == (ulid.ULID{}) {
		return nil, errors.New("search: a user identity is required")
	}

	ix.mu.Lock()
	if ui, ok := ix.users[userID]; ok {
		ix.mu.Unlock()
		return ui, nil
	}
	ix.mu.Unlock()

	dir := ix.liveDir(userID)
	if err := mkdirSecure(dir); err != nil {
		return nil, err
	}
	m, err := readMeta(dir)
	if err != nil {
		return nil, err
	}
	backend, err := ix.open(ctx, dir)
	if err != nil {
		return nil, fmt.Errorf("search: open index for %s: %w", userID, err)
	}
	ui := &userIndex{dir: dir, backend: backend, meta: m}

	ix.mu.Lock()
	defer ix.mu.Unlock()
	// Lost a race: another goroutine opened it first. Close ours and use theirs.
	if existing, ok := ix.users[userID]; ok {
		_ = backend.Close()
		return existing, nil
	}
	ix.users[userID] = ui
	return ui, nil
}

// Progress reports what a catch-up or reindex did.
type Progress struct {
	Indexed int   // documents upserted
	Deleted int   // documents removed
	Cursor  int64 // the change state the index is now at
	Rebuilt bool  // a full reindex happened

	// BodiesMissing counts documents indexed from HEADERS ONLY because their
	// blob could not be read (#351 B6). It is the difference between "the index
	// was rebuilt" and "the mail came back".
	//
	// ⚠ WITHOUT IT THE RESTORE DRILL PROVED ONE MESSAGE. The drill decrypts the
	// single oldest message directly, and otherwise relies on the reindex — which
	// walks every message and reads every blob. But an unreadable blob degraded
	// silently to a headers-only document and was still counted in Indexed, so a
	// restore that lost all but the oldest message still printed its document
	// count and THE RESTORE IS ALIVE. A count that cannot distinguish those two
	// outcomes is the light that is always on.
	BodiesMissing int
}

// Sync catches the user's index up to the head of the Email change log and
// returns what it did. It is safe to call concurrently with searches, and safe to
// call again after a crash — replaying a page is a no-op.
//
// A cursor below the compaction floor, a schema-version bump, or an interrupted
// previous rebuild each mean the deltas cannot get us to a correct index: Sync
// falls back to Reindex, which always can (the store is the system of record).
func (ix *Indexer) Sync(ctx context.Context, userID ulid.ULID) (Progress, error) {
	ui, err := ix.index(ctx, userID)
	if err != nil {
		return Progress{}, err
	}

	ui.mu.RLock()
	m := ui.meta
	ui.mu.RUnlock()

	if m.SchemaVersion != schemaVersion || m.Rebuilding {
		ix.log.Info("search: rebuilding index",
			"user_id", userID.String(), "reason", rebuildReason(m),
			"have_version", m.SchemaVersion, "want_version", schemaVersion)
		return ix.Reindex(ctx, userID)
	}

	var prog Progress
	prog.Cursor = m.Cursor
	for {
		if err := ctx.Err(); err != nil {
			return prog, err
		}
		page, err := ix.src.EmailChanges(ctx, userID, prog.Cursor, ix.batch)
		if errors.Is(err, ErrCursorTooOld) {
			ix.log.Warn("search: index cursor below the change-log floor; reindexing",
				"user_id", userID.String(), "cursor", prog.Cursor)
			return ix.Reindex(ctx, userID)
		}
		if err != nil {
			return prog, fmt.Errorf("search: read changes: %w", err)
		}
		if len(page.Upsert) == 0 && len(page.Delete) == 0 && page.NewState == prog.Cursor {
			return prog, nil // already at the head
		}

		n, missing, err := ix.apply(ctx, ui, userID, page.Upsert, page.Delete)
		if err != nil {
			return prog, err
		}
		prog.Indexed += n
		prog.BodiesMissing += missing
		prog.Deleted += len(page.Delete)

		// The cursor is persisted ONLY after the index write landed. See the file
		// header: this ordering is the crash-safety argument.
		if err := ix.setCursor(ui, page.NewState); err != nil {
			return prog, err
		}
		prog.Cursor = page.NewState

		if !page.HasMore {
			return prog, nil
		}
	}
}

func rebuildReason(m meta) string {
	switch {
	case m.Rebuilding:
		return "previous rebuild was interrupted"
	case m.SchemaVersion == 0:
		return "no index metadata (new or discarded index)"
	default:
		return "index schema version bump"
	}
}

// apply upserts the given message ids and deletes the destroyed ones against the
// user's live backend.
func (ix *Indexer) apply(ctx context.Context, ui *userIndex, userID ulid.ULID, upsert, remove []ulid.ULID) (indexed, bodiesMissing int, err error) {
	docs, missing, err := ix.documents(ctx, userID, upsert)
	if err != nil {
		return 0, 0, err
	}

	ui.mu.Lock()
	defer ui.mu.Unlock()
	if len(docs) > 0 {
		if err := ui.backend.Upsert(ctx, docs); err != nil {
			return 0, 0, fmt.Errorf("search: upsert %d documents: %w", len(docs), err)
		}
	}
	if len(remove) > 0 {
		if err := ui.backend.Delete(ctx, remove); err != nil {
			return 0, 0, fmt.Errorf("search: delete %d documents: %w", len(remove), err)
		}
	}
	return len(docs), missing, nil
}

// documents loads the canonical rows for ids and builds their Documents, pulling
// each message's raw bytes for the body and recipients.
//
// An id that no longer resolves is skipped, not an error: the message was
// destroyed between the change row and this read, and the destroy is already in
// the stream behind us.
func (ix *Indexer) documents(ctx context.Context, userID ulid.ULID, ids []ulid.ULID) ([]Document, int, error) {
	if len(ids) == 0 {
		return nil, 0, nil
	}
	msgs, err := ix.src.Messages(ctx, userID, ids)
	if err != nil {
		return nil, 0, fmt.Errorf("search: load messages: %w", err)
	}
	bodiesMissing := 0
	docs := make([]Document, 0, len(msgs))
	for _, m := range msgs {
		var raw []byte
		if ix.content != nil && len(m.ContentHash) > 0 {
			raw, err = ix.content.Raw(ctx, userID, m.ContentHash)
			if err != nil {
				// Degrade, don't wedge: index the headers we have from Postgres.
				// Never log the content or the hash's plaintext meaning — id only.
				ix.log.Warn("search: message body unavailable; indexing headers only",
					"user_id", userID.String(), "message_id", m.ID.String(), "err", err)
				raw = nil
				bodiesMissing++
			}
		}
		docs = append(docs, buildDocument(m, raw))
	}
	return docs, bodiesMissing, nil
}

// Reindex rebuilds a user's index from Postgres and atomically swaps it in — the
// first-class operation spec §7 requires ("the index is a disposable cache …
// always rebuildable; reindex-user is a first-class operation").
//
// The sequence matters:
//
//  1. Read the CURRENT Email change state. Everything committed after this point
//     will be replayed by the catch-up in step 5, so nothing can slip through the
//     gap between the scan and the swap.
//  2. Build a complete index in a fresh sibling directory (build-<ulid>, 0700).
//     The live index keeps serving the whole time — stale-but-working beats
//     degraded (spec §7: "rebuild-and-swap").
//  3. Write the new index's cursor = the state from step 1.
//  4. Swap: move live aside, move the build into place, install the new backend,
//     close the old one, and only then delete the old directory. Closing before
//     deleting means no search is ever reading a file we are removing.
//  5. Catch up from the cursor, which pulls in everything that changed during the
//     rebuild.
func (ix *Indexer) Reindex(ctx context.Context, userID ulid.ULID) (Progress, error) {
	ui, err := ix.index(ctx, userID)
	if err != nil {
		return Progress{}, err
	}

	start := time.Now()
	head, err := ix.src.EmailState(ctx, userID) // (1)
	if err != nil {
		return Progress{}, fmt.Errorf("search: read email state: %w", err)
	}

	// Mark the live index as rebuilding, so a crash mid-rebuild is never mistaken
	// for a healthy index with a valid cursor.
	ui.mu.Lock()
	m := ui.meta
	m.Rebuilding = true
	if err := writeMeta(ui.dir, m); err != nil {
		ui.mu.Unlock()
		return Progress{}, err
	}
	ui.meta = m
	ui.mu.Unlock()

	buildDir := filepath.Join(ix.userRoot(userID), "build-"+buildID())
	if err := mkdirSecure(buildDir); err != nil {
		return Progress{}, err
	}
	// A failed build leaves nothing behind. (On success the directory has already
	// been renamed into place, so this is a no-op — hence best-effort.)
	defer func() { _ = os.RemoveAll(buildDir) }()

	fresh, err := ix.open(ctx, buildDir) // (2)
	if err != nil {
		return Progress{}, fmt.Errorf("search: open rebuild index: %w", err)
	}

	prog := Progress{Rebuilt: true, Cursor: head}
	var after ulid.ULID
	for {
		if err := ctx.Err(); err != nil {
			_ = fresh.Close()
			return prog, err
		}
		ids, err := ix.src.MessageIDsAfter(ctx, userID, after, ix.batch)
		if err != nil {
			_ = fresh.Close()
			return prog, fmt.Errorf("search: walk messages: %w", err)
		}
		if len(ids) == 0 {
			break
		}
		docs, missing, err := ix.documents(ctx, userID, ids)
		if err != nil {
			_ = fresh.Close()
			return prog, err
		}
		if len(docs) > 0 {
			if err := fresh.Upsert(ctx, docs); err != nil {
				_ = fresh.Close()
				return prog, fmt.Errorf("search: rebuild upsert: %w", err)
			}
			prog.Indexed += len(docs)
		}
		prog.BodiesMissing += missing
		after = ids[len(ids)-1]
	}

	if err := writeMeta(buildDir, meta{ // (3)
		SchemaVersion: schemaVersion, Cursor: head, UpdatedAt: time.Now().UTC(),
	}); err != nil {
		_ = fresh.Close()
		return prog, err
	}

	if err := ix.swap(ctx, ui, buildDir, fresh, head); err != nil { // (4) closes fresh on error
		return prog, err
	}

	ix.log.Info("search: index rebuilt",
		"user_id", userID.String(), "documents", prog.Indexed,
		"cursor", head, "took", time.Since(start).String())

	catchUp, err := ix.Sync(ctx, userID) // (5)
	if err != nil {
		return prog, err
	}
	prog.Indexed += catchUp.Indexed
	prog.Deleted += catchUp.Deleted
	// The tail catch-up reads blobs too, so its degrades must be carried or a
	// body lost in the window between the scan and the swap would go unreported.
	prog.BodiesMissing += catchUp.BodiesMissing
	prog.Cursor = catchUp.Cursor
	return prog, nil
}

// PathBound is implemented by a backend whose open handle is tied to the
// directory it was opened on (Tantivy writes each new segment under that path).
// Such a handle cannot be carried across the rebuild's rename; see swap.
type PathBound interface {
	PathBound() bool
}

// swap installs a freshly built index as the user's live index. The old backend
// is closed BEFORE its directory is removed, so no search is ever reading files
// we are deleting; and the old directory is moved aside first, so the window in
// which "live" does not exist is a single rename. swap owns fresh: on an error
// it has been closed.
//
// ⚠ A PATH-BOUND BACKEND IS REOPENED AT THE LIVE PATH, never carried over from
// the build. A Tantivy handle opened on build-<id>/ commits into a directory the
// rename has removed, and every catch-up after a rebuild failed with "No such
// file or directory" until the process restarted
// (TestIndexerIndexesNewMailAfterARebuild). The in-memory backend keeps no
// files, and reopening it would come back empty, so its handle is kept.
func (ix *Indexer) swap(ctx context.Context, ui *userIndex, buildDir string, fresh SearchBackend, cursor int64) error {
	ui.mu.Lock()
	defer ui.mu.Unlock()

	reopen := false
	if pb, ok := fresh.(PathBound); ok && pb.PathBound() {
		reopen = true
		// Everything the build wrote is committed; closing releases its lock.
		if err := fresh.Close(); err != nil {
			return fmt.Errorf("search: close rebuilt index: %w", err)
		}
	}

	oldDir := filepath.Join(filepath.Dir(ui.dir), "old-"+buildID())
	if err := os.Rename(ui.dir, oldDir); err != nil {
		if !reopen {
			_ = fresh.Close()
		}
		return fmt.Errorf("search: move old index aside: %w", err)
	}
	if err := os.Rename(buildDir, ui.dir); err != nil {
		// Put the old one back rather than leaving the user with no index at all.
		_ = os.Rename(oldDir, ui.dir)
		if !reopen {
			_ = fresh.Close()
		}
		return fmt.Errorf("search: install rebuilt index: %w", err)
	}
	if reopen {
		var err error
		if fresh, err = ix.open(ctx, ui.dir); err != nil {
			// Same rule: the old index goes back and keeps serving.
			_ = os.Rename(ui.dir, buildDir)
			_ = os.Rename(oldDir, ui.dir)
			return fmt.Errorf("search: open rebuilt index: %w", err)
		}
	}

	old := ui.backend
	ui.backend = fresh
	ui.meta = meta{SchemaVersion: schemaVersion, Cursor: cursor, UpdatedAt: time.Now().UTC()}

	if old != nil {
		if err := old.Close(); err != nil {
			ix.log.Warn("search: closing the replaced index failed", "err", err)
		}
	}
	if err := os.RemoveAll(oldDir); err != nil {
		ix.log.Warn("search: removing the replaced index directory failed", "dir", oldDir, "err", err)
	}
	return nil
}

// setCursor persists the cursor after a successful index write.
func (ix *Indexer) setCursor(ui *userIndex, cursor int64) error {
	ui.mu.Lock()
	defer ui.mu.Unlock()
	m := meta{SchemaVersion: schemaVersion, Cursor: cursor, UpdatedAt: time.Now().UTC()}
	if err := writeMeta(ui.dir, m); err != nil {
		return err
	}
	ui.meta = m
	return nil
}

// Run drives the user's index continuously: one catch-up immediately, then every
// interval, until ctx is cancelled. Failures are logged and retried on the next
// tick — a search index that cannot keep up must degrade (stale results), never
// take the process down.
func (ix *Indexer) Run(ctx context.Context, userID ulid.ULID) error {
	if userID == (ulid.ULID{}) {
		return errors.New("search: run: a user identity is required")
	}
	ix.log.Info("search: indexer starting",
		"user_id", userID.String(), "interval", ix.interval.String(), "root", ix.root)

	if prog, err := ix.Sync(ctx, userID); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		ix.log.Error("search: initial catch-up failed", "user_id", userID.String(), "err", err)
	} else if prog.Indexed > 0 || prog.Deleted > 0 {
		ix.log.Info("search: caught up", "user_id", userID.String(),
			"indexed", prog.Indexed, "deleted", prog.Deleted, "cursor", prog.Cursor)
	}

	t := time.NewTicker(ix.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			ix.log.Info("search: indexer stopping", "user_id", userID.String())
			return ctx.Err()
		case <-t.C:
			prog, err := ix.Sync(ctx, userID)
			if err != nil {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				ix.log.Error("search: catch-up failed", "user_id", userID.String(), "err", err)
				continue
			}
			// Reported on EVERY pass, including one that indexed nothing: a
			// caught-up consumer must be able to report zero lag, or the gauge
			// keeps its last value and stays stuck at its high-water mark.
			ix.reportSync(userID, prog)
			if prog.Indexed > 0 || prog.Deleted > 0 {
				ix.log.Debug("search: indexed", "user_id", userID.String(),
					"indexed", prog.Indexed, "deleted", prog.Deleted, "cursor", prog.Cursor)
			}
		}
	}
}

// Search runs a query against ONE user's index. There is no other entry point,
// and there is no query that spans users: the caller's id selects the index, and
// a query cannot name another one (spec §7's isolation control, mirrored on the
// RLS posture the database has).
func (ix *Indexer) Search(ctx context.Context, userID ulid.ULID, q Query) (Result, error) {
	ui, err := ix.index(ctx, userID)
	if err != nil {
		return Result{}, err
	}
	ui.mu.RLock()
	res, err := ui.backend.Search(ctx, q)
	ui.mu.RUnlock()
	if err != nil {
		return Result{}, err
	}
	RankHits(res.Hits)
	return res, nil
}

// RankHits puts hits in Mailroom's ONE relevance order: score descending, then
// newest first, then id — a total order, so the same set of hits always comes
// back in the same sequence (#724).
//
// Email/query's "relevance" sort pages through this sequence by position
// (store.EmailQuery.SortByRank), re-running the search for every page. An engine
// is free to break score ties however it likes — Tantivy breaks them by document
// address, which a segment merge between two page requests changes — and a tie
// order that moved between page 1 and page 2 would show one message twice and
// skip another. Ties are common: a one-word search over subjects scores every
// subject hit identically. The memory backend already orders this way; this
// makes it the contract rather than a property of one backend.
func RankHits(hits []Hit) {
	sort.SliceStable(hits, func(i, j int) bool {
		a, b := hits[i], hits[j]
		switch {
		case a.Score != b.Score:
			return a.Score > b.Score
		case !a.ReceivedAt.Equal(b.ReceivedAt):
			return a.ReceivedAt.After(b.ReceivedAt)
		default:
			return a.ID.Compare(b.ID) < 0
		}
	})
}

// Stats reports the health of a user's index: how far behind the change log it
// is, and how many documents it holds. Lag is what surfaces spec §7's "search may
// be N minutes stale".
type Stats struct {
	Cursor    int64 // the change state the index is at
	Head      int64 // the current change state in Postgres
	Lag       int64 // Head - Cursor: how many change sequences behind
	Documents uint64
}

// Stats reads the user's index health.
func (ix *Indexer) Stats(ctx context.Context, userID ulid.ULID) (Stats, error) {
	ui, err := ix.index(ctx, userID)
	if err != nil {
		return Stats{}, err
	}
	head, err := ix.src.EmailState(ctx, userID)
	if err != nil {
		return Stats{}, fmt.Errorf("search: read email state: %w", err)
	}
	ui.mu.RLock()
	cursor := ui.meta.Cursor
	backend := ui.backend
	ui.mu.RUnlock()

	n, err := backend.NumDocs(ctx)
	if err != nil {
		return Stats{}, fmt.Errorf("search: count documents: %w", err)
	}
	return Stats{Cursor: cursor, Head: head, Lag: head - cursor, Documents: n}, nil
}

// Close releases every open index. Safe to call once, at shutdown.
func (ix *Indexer) Close() error {
	ix.mu.Lock()
	users := ix.users
	ix.users = make(map[ulid.ULID]*userIndex)
	ix.mu.Unlock()

	var firstErr error
	for id, ui := range users {
		ui.mu.Lock()
		if err := ui.backend.Close(); err != nil && firstErr == nil {
			firstErr = fmt.Errorf("search: close index for %s: %w", id, err)
		}
		ui.mu.Unlock()
	}
	return firstErr
}

// --- Directory + metadata helpers -------------------------------------------

// mkdirSecure creates dir (and parents) 0700 and enforces the mode even if it
// already existed with a wider one. A user's index directory holds the plaintext
// of their mail; a mode that lets anything else read it is a security bug, not a
// cosmetic one.
func mkdirSecure(dir string) error {
	if err := os.MkdirAll(dir, dirMode); err != nil {
		return fmt.Errorf("search: create %s: %w", dir, err)
	}
	if err := os.Chmod(dir, dirMode); err != nil {
		return fmt.Errorf("search: secure %s: %w", dir, err)
	}
	// The per-user parent is created by MkdirAll with the umask applied; force it
	// too, so <root>/<user> is not group/world traversable.
	if parent := filepath.Dir(dir); parent != "." && parent != string(filepath.Separator) {
		if err := os.Chmod(parent, dirMode); err != nil {
			return fmt.Errorf("search: secure %s: %w", parent, err)
		}
	}
	return nil
}

// readMeta reads a user index's meta.json. A missing or unreadable file yields
// the zero meta, which forces a rebuild — the safe direction: an index we cannot
// describe is an index we do not trust.
func readMeta(dir string) (meta, error) {
	b, err := os.ReadFile(filepath.Join(dir, metaFile))
	if errors.Is(err, os.ErrNotExist) {
		return meta{}, nil
	}
	if err != nil {
		return meta{}, fmt.Errorf("search: read %s: %w", metaFile, err)
	}
	var m meta
	if err := json.Unmarshal(b, &m); err != nil {
		return meta{}, nil // corrupt metadata → rebuild, not a hard failure
	}
	return m, nil
}

// writeMeta persists a user index's meta.json atomically: write a temp file in
// the same directory, fsync it, then rename over the old one. A torn meta.json
// would be indistinguishable from a corrupt one (and force a needless rebuild),
// so it is worth the fsync.
func writeMeta(dir string, m meta) error {
	b, err := json.Marshal(m)
	if err != nil {
		return fmt.Errorf("search: marshal %s: %w", metaFile, err)
	}
	tmp := filepath.Join(dir, metaFile+".tmp")
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("search: write %s: %w", metaFile, err)
	}
	// On any failure the half-written temp file is removed; a failure to remove
	// it is not worth surfacing over the error that actually matters (and the
	// next write truncates it anyway).
	fail := func(stage string, err error) error {
		_ = f.Close()
		_ = os.Remove(tmp)
		return fmt.Errorf("search: %s %s: %w", stage, metaFile, err)
	}
	if _, err := f.Write(b); err != nil {
		return fail("write", err)
	}
	if err := f.Sync(); err != nil {
		return fail("sync", err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("search: close %s: %w", metaFile, err)
	}
	if err := os.Rename(tmp, filepath.Join(dir, metaFile)); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("search: install %s: %w", metaFile, err)
	}
	return nil
}

// WithSyncSink installs a callback invoked after every catch-up pass in Run,
// with the user and the pass's Progress. It exists so the cursor can be turned
// into mailroom_index_cursor_lag_seconds without the search package importing
// the metrics registry or learning what a Prometheus gauge is (#524).
//
// It is called from Run's goroutine and must not block. Without it Run behaves
// identically.
func WithSyncSink(f func(userID ulid.ULID, p Progress)) Option {
	return func(ix *Indexer) { ix.syncSink = f }
}

// reportSync fires the sync sink if one is installed.
func (ix *Indexer) reportSync(userID ulid.ULID, p Progress) {
	if ix.syncSink != nil {
		ix.syncSink(userID, p)
	}
}
