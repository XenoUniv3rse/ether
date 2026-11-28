// Package pluginitems owns the storage, keyset browse, full-text search,
// sync engine, Title projection, matching and resolution cache for plugin
// content kinds (spec §2.2-2.5, §3.1-3.6, §4.4, §6.3, §7).
//
// Plugin items never enter catalog_titles; every query is scoped by
// library_id first, and item identity is the namespaced core.ItemRef
// ("pk:<pluginID>:<kindKey>:<externalID>") paired with a stored,
// never-reused RatingKey sequence number (core.PluginKey).
package pluginitems

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/plugins"
	"github.com/braedonsaunders/ether/internal/store"
)

const currentFTSVersion int64 = 1

var pluginItemsRestoreGen atomic.Uint64

func init() {
	store.Register("pluginitems", 26, `ALTER TABLE plugin_sync_state ADD COLUMN failure_json TEXT NOT NULL DEFAULT '';`)
	// search_tsv exists only on PostgreSQL (SQLite searches plugin_item_fts),
	// so a cluster's dump restored on a standalone server skips it.
	store.RegisterLocalColumn("plugin_items", "search_tsv")
	// Logical restores never carry SQLite's virtual plugin_item_fts table,
	// and a SQLite backup restored onto PostgreSQL lacks plugin_items.search_tsv
	// (which defaults to ''::tsvector). Mark affected kinds pending so startup
	// rebuilds the engine's search index in bounded, resumable keyset batches.
	store.RegisterRestore("pluginitems_search", func(ctx context.Context, tx *sql.Tx) (func(context.Context, *sql.Tx) error, error) {
		return func(ctx context.Context, tx *sql.Tx) error {
			pluginItemsRestoreGen.Add(1)
			if store.RestoreDialect(ctx) == store.Postgres {
				_, err := tx.ExecContext(ctx, `
UPDATE plugin_kinds
SET fts_version = 0
WHERE EXISTS (
	SELECT 1
	FROM plugin_items i
	WHERE i.plugin_id = plugin_kinds.plugin_id
	  AND i.kind_key = plugin_kinds.kind_key
	  AND i.search_tsv = ''::tsvector
)`)
				return err
			}
			_, err := tx.ExecContext(ctx, `UPDATE plugin_kinds SET fts_version = 0`)
			return err
		}, nil
	})
	// Migration 9: persist each item's content-rating enforcement level
	// (contentrating.Level, 0 = unrated) so a viewer's MaxContentRating is
	// applied in SQL by Browse instead of by walking the library in memory.
	store.Register("pluginitems", 9, `ALTER TABLE plugin_items ADD COLUMN rating_level INTEGER NOT NULL DEFAULT 0;`)
	store.RegisterApply("pluginitems", 9, applyMigration9)
	// Migration 10 (#646): the content hash of the canonical stored item,
	// so a republish can skip items that have not changed instead of
	// rewriting their row, field rows, art and FTS entry. Existing rows
	// start empty and are rewritten (and hashed) on their next upsert.
	// (Migration 9 is taken by plugin_items.rating_level on the preview
	// branch.)
	store.Register("pluginitems", 10, `
ALTER TABLE plugin_items ADD COLUMN content_hash TEXT NOT NULL DEFAULT '';
`)
	// Migration 11: persist the full /resolve candidate list alongside the
	// winning source so version menus and version picks reuse the resolution
	// cache without re-running plugin /resolve on every detail or play call.
	store.Register("pluginitems", 11, `
ALTER TABLE plugin_resolutions ADD COLUMN candidates TEXT NOT NULL DEFAULT '';
`)
	// Migration 12: backfill plugin_items.sort_title with the shared sort-key
	// rules (leading non-alphanumerics stripped before leading English article,
	// diacritic/ligature folding, and numeric collation) while preserving
	// genuine custom source sort titles.
	store.Register("pluginitems", 12, `SELECT 1;`)
	store.RegisterApply("pluginitems", 12, applyMigration12)
	// Migration 14: track items pinned by live search, facet/container
	// expansion, or viewer opens so missed-walk pruning never drops them, and
	// persist structured 50%-delete-guard metadata on plugin_sync_state so the
	// console can present a plain-language keep-or-remove choice.
	store.Register("pluginitems", 14, `
ALTER TABLE plugin_items ADD COLUMN pinned INTEGER NOT NULL DEFAULT 0;
ALTER TABLE plugin_sync_state ADD COLUMN guard_json TEXT NOT NULL DEFAULT '';
`)
	// Migration 15: maintain per-parent child and descendant-leaf tallies for
	// multi-level plugin kinds so containers (series, seasons, albums) report
	// ChildCount and LeafCount in O(1) without walking descendants.
	store.Register("pluginitems", 15, migration15SQLiteDDL)
	store.RegisterDialect("pluginitems", 15, store.Postgres, migration15PostgresDDL)
	store.RegisterSQLiteApply("pluginitems", 15, applyMigration15SQLite)
	store.RegisterPostgresApply("pluginitems", 15, applyMigration15Postgres)
	// Migration 16: partition plugin_item_parent_tally by child_level so
	// branching kinds report per-role/per-level counts in O(1), and index
	// (library_id, parent_ref, level, idx, item_ref) for O(page) filtered
	// children paging.
	store.Register("pluginitems", 16, migration16SQLiteDDL)
	store.RegisterDialect("pluginitems", 16, store.Postgres, migration16PostgresDDL)
	store.RegisterSQLiteApply("pluginitems", 16, applyMigration16SQLite)
	store.RegisterPostgresApply("pluginitems", 16, applyMigration16Postgres)

	// Migration 17: extend plugin_item_field_tally with case-folded search and
	// name-order columns/indexes, and add plugin_item_person_tally keyed by
	// v_int so people and collections tabs read bounded indexed pages from the
	// tallies without scanning plugin_item_fields.
	store.Register("pluginitems", 17, migration17SQLiteDDL)
	store.RegisterDialect("pluginitems", 17, store.Postgres, migration17PostgresDDL)
	store.RegisterSQLiteApply("pluginitems", 17, applyMigration17SQLite)
	store.RegisterPostgresApply("pluginitems", 17, applyMigration17Postgres)

	// Migration 18: persist the newest representative item_ref (and person
	// portrait-thumb item_ref) on plugin_item_field_tally and
	// plugin_item_person_tally and cover them in the tally sort indexes so
	// collections and people tabs resolve artwork in O(1) per bucket without
	// scanning plugin_item_fields or probing plugin_items_released.
	store.Register("pluginitems", 18, migration18SQLiteDDL)
	store.RegisterDialect("pluginitems", 18, store.Postgres, migration18PostgresDDL)
	store.RegisterSQLiteApply("pluginitems", 18, applyMigration18SQLite)
	store.RegisterPostgresApply("pluginitems", 18, applyMigration18Postgres)

	// Migration 19 (#768): bound PostgreSQL btree index entries on
	// plugin_item_fields, plugin_item_field_tally, and plugin_item_person_tally
	// with md5(...) / left(..., 256) expressions so any validated field value
	// up to MaxFieldRunes (4,000 runes) fits well under PostgreSQL's 2,704-byte
	// btree tuple limit. SQLite has no btree tuple cap and keeps its covering
	// indexes unchanged.
	store.Register("pluginitems", 19, `SELECT 1;`)
	store.RegisterDialect("pluginitems", 19, store.Postgres, migration19PostgresDDL)

	// Migration 8 (#623): restrict the numeric field lookup indexes to rows
	// their queries read so text-heavy libraries do not store and maintain
	// (field, 0, item_ref) entries for every text value. Both SQLite and
	// PostgreSQL support these partial index predicates verbatim.
	store.Register("pluginitems", 8, `
DROP INDEX IF EXISTS plugin_item_fields_lookup_real;
CREATE INDEX plugin_item_fields_lookup_real ON plugin_item_fields(field, v_real, item_ref) WHERE v_text = '';
DROP INDEX IF EXISTS plugin_item_fields_lookup_int;
CREATE INDEX plugin_item_fields_lookup_int ON plugin_item_fields(field, v_int, item_ref) WHERE v_text = '' OR v_int <> 0;
`)

	store.Register("pluginitems", 7, `
CREATE TABLE plugin_item_field_tally (
	library_id INTEGER NOT NULL,
	level      TEXT    NOT NULL,
	field      TEXT    NOT NULL,
	v_text     TEXT    NOT NULL,
	cnt        INTEGER NOT NULL,
	PRIMARY KEY (library_id, level, field, v_text)
);
INSERT INTO plugin_item_field_tally(library_id, level, field, v_text, cnt)
SELECT i.library_id, i.level, f.field, f.v_text, COUNT(*)
FROM plugin_item_fields f
JOIN plugin_items i ON i.item_ref = f.item_ref
WHERE f.v_text <> '' AND i.level <> ''
GROUP BY i.library_id, i.level, f.field, f.v_text;
INSERT INTO plugin_item_field_tally(library_id, level, field, v_text, cnt)
SELECT library_id, '', field, v_text, CAST(SUM(cnt) AS INTEGER)
FROM plugin_item_field_tally
WHERE level <> ''
GROUP BY library_id, field, v_text;
CREATE INDEX plugin_item_field_tally_count
	ON plugin_item_field_tally(library_id, level, field, cnt DESC, v_text);
`)

	store.Register("pluginitems", 5, `
CREATE TABLE plugin_facet_expansions (
	plugin_id   TEXT    NOT NULL,
	kind_key    TEXT    NOT NULL,
	library_id  INTEGER NOT NULL,
	facet       TEXT    NOT NULL,
	value_key   TEXT    NOT NULL,
	state       TEXT    NOT NULL DEFAULT '',
	started_at  INTEGER NOT NULL DEFAULT 0,
	finished_at INTEGER NOT NULL DEFAULT 0,
	items_added INTEGER NOT NULL DEFAULT 0,
	error       TEXT    NOT NULL DEFAULT '',
	PRIMARY KEY (plugin_id, kind_key, facet, value_key)
);
CREATE INDEX plugin_facet_expansions_library ON plugin_facet_expansions(library_id, state);
`)
	store.RegisterDialect("pluginitems", 5, store.Postgres, `
CREATE TABLE plugin_facet_expansions (
	plugin_id   TEXT   NOT NULL,
	kind_key    TEXT   NOT NULL,
	library_id  BIGINT NOT NULL,
	facet       TEXT   NOT NULL,
	value_key   TEXT   NOT NULL,
	state       TEXT   NOT NULL DEFAULT '',
	started_at  BIGINT NOT NULL DEFAULT 0,
	finished_at BIGINT NOT NULL DEFAULT 0,
	items_added BIGINT NOT NULL DEFAULT 0,
	error       TEXT   NOT NULL DEFAULT '',
	PRIMARY KEY (plugin_id, kind_key, facet, value_key)
);
CREATE INDEX plugin_facet_expansions_library ON plugin_facet_expansions(library_id, state);
`)

	store.Register("pluginitems", 6, `
CREATE INDEX IF NOT EXISTS plugin_item_fields_lookup_real ON plugin_item_fields(field, v_real, item_ref);
`)
	store.RegisterApply("pluginitems", 6, applyMigration6)

	store.Register("pluginitems", 4, `
ALTER TABLE plugin_item_fields ADD COLUMN v_fold TEXT NOT NULL DEFAULT '';
CREATE INDEX plugin_item_fields_fold ON plugin_item_fields(field, v_fold, v_text, v_int, item_ref);
`)
	store.RegisterApply("pluginitems", 4, applyMigration4)

	store.Register("pluginitems", 3, `ALTER TABLE plugin_kinds ADD COLUMN fts_version INTEGER NOT NULL DEFAULT 0;`)
	store.RegisterDialect("pluginitems", 3, store.Postgres, `ALTER TABLE plugin_kinds ADD COLUMN fts_version BIGINT NOT NULL DEFAULT 0;`)
	store.RegisterSQLiteApply("pluginitems", 3, applyMigration3SQLite)
	store.RegisterPostgresApply("pluginitems", 3, applyMigration3Postgres)

	store.Register("pluginitems", 2, `ALTER TABLE plugin_items ADD COLUMN unavailable TEXT NOT NULL DEFAULT '';`)
	store.Register("pluginitems", 1, `
CREATE TABLE plugin_kinds (
	plugin_id      TEXT    NOT NULL,
	kind_key       TEXT    NOT NULL,
	library_id     INTEGER NOT NULL,
	decl           TEXT    NOT NULL,
	decl_hash      TEXT    NOT NULL,
	schema_version INTEGER NOT NULL DEFAULT 1,
	PRIMARY KEY (plugin_id, kind_key)
);
CREATE INDEX plugin_kinds_library ON plugin_kinds(library_id);

CREATE TABLE plugin_items_seq (
	id   INTEGER PRIMARY KEY,
	next INTEGER NOT NULL
);
INSERT INTO plugin_items_seq(id, next) VALUES(1, 1);

CREATE TABLE plugin_items (
	item_ref     TEXT    PRIMARY KEY,
	seq          INTEGER NOT NULL UNIQUE,
	plugin_id    TEXT    NOT NULL,
	kind_key     TEXT    NOT NULL,
	library_id   INTEGER NOT NULL,
	external_id  TEXT    NOT NULL,
	parent_ref   TEXT    NOT NULL DEFAULT '',
	level        TEXT    NOT NULL,
	idx          INTEGER NOT NULL DEFAULT 0,
	title        TEXT    NOT NULL,
	sort_title   TEXT    NOT NULL,
	summary      TEXT    NOT NULL DEFAULT '',
	year         INTEGER NOT NULL DEFAULT 0,
	released     INTEGER NOT NULL DEFAULT 0,
	rating       REAL    NOT NULL DEFAULT 0,
	playable     INTEGER NOT NULL DEFAULT 0,
	child_count  INTEGER NOT NULL DEFAULT 0,
	leaf_count   INTEGER NOT NULL DEFAULT 0,
	hints        TEXT    NOT NULL DEFAULT '{}',
	fields       TEXT    NOT NULL DEFAULT '{}',
	etag         TEXT    NOT NULL DEFAULT '',
	added_at     INTEGER NOT NULL,
	updated_at   INTEGER NOT NULL,
	seen_at      INTEGER NOT NULL,
	missed_walks INTEGER NOT NULL DEFAULT 0,
	UNIQUE (plugin_id, kind_key, external_id)
);
CREATE INDEX plugin_items_browse       ON plugin_items(library_id, parent_ref, sort_title, item_ref);
CREATE INDEX plugin_items_level_sort   ON plugin_items(library_id, level, sort_title, item_ref);
CREATE INDEX plugin_items_parent       ON plugin_items(parent_ref, idx, sort_title, item_ref);
CREATE INDEX plugin_items_added        ON plugin_items(library_id, added_at DESC, item_ref);
CREATE INDEX plugin_items_released     ON plugin_items(library_id, released DESC, item_ref);
CREATE INDEX plugin_items_year         ON plugin_items(library_id, year DESC, item_ref);
CREATE INDEX plugin_items_rating       ON plugin_items(library_id, rating DESC, item_ref);
CREATE INDEX plugin_items_idx          ON plugin_items(library_id, idx ASC, item_ref);
CREATE INDEX plugin_items_search_title ON plugin_items(library_id, sort_title);

CREATE TABLE plugin_item_fields (
	item_ref TEXT    NOT NULL,
	field    TEXT    NOT NULL,
	v_text   TEXT    NOT NULL DEFAULT '',
	v_int    INTEGER NOT NULL DEFAULT 0,
	v_real   REAL    NOT NULL DEFAULT 0,
	PRIMARY KEY (item_ref, field, v_text, v_int)
);
CREATE INDEX plugin_item_fields_lookup     ON plugin_item_fields(field, v_text, item_ref);
CREATE INDEX plugin_item_fields_lookup_int ON plugin_item_fields(field, v_int, item_ref);

CREATE TABLE plugin_item_art (
	item_ref TEXT    NOT NULL,
	role     TEXT    NOT NULL,
	url      TEXT    NOT NULL,
	width    INTEGER NOT NULL DEFAULT 0,
	height   INTEGER NOT NULL DEFAULT 0,
	PRIMARY KEY (item_ref, role)
);

CREATE TABLE plugin_item_tally (
	library_id INTEGER NOT NULL,
	level      TEXT    NOT NULL,
	n          INTEGER NOT NULL,
	PRIMARY KEY (library_id, level)
);

CREATE TABLE plugin_sync_state (
	plugin_id    TEXT    NOT NULL,
	kind_key     TEXT    NOT NULL,
	cursor       TEXT    NOT NULL DEFAULT '',
	generation   INTEGER NOT NULL DEFAULT 0,
	last_started INTEGER NOT NULL DEFAULT 0,
	last_ok      INTEGER NOT NULL DEFAULT 0,
	last_error   TEXT    NOT NULL DEFAULT '',
	items        INTEGER NOT NULL DEFAULT 0,
	PRIMARY KEY (plugin_id, kind_key)
);

CREATE TABLE plugin_resolutions (
	item_ref   TEXT    PRIMARY KEY,
	pattern    TEXT    NOT NULL DEFAULT '',
	source     TEXT    NOT NULL DEFAULT '{}',
	negative   INTEGER NOT NULL DEFAULT 0,
	expires_at INTEGER NOT NULL
);
CREATE INDEX plugin_resolutions_expires ON plugin_resolutions(expires_at);

CREATE TABLE plugin_matches (
	item_ref      TEXT    NOT NULL,
	candidate_ref TEXT    NOT NULL,
	score         REAL    NOT NULL,
	reasons       TEXT    NOT NULL DEFAULT '[]',
	source        TEXT    NOT NULL DEFAULT 'auto',
	chosen        INTEGER NOT NULL DEFAULT 0,
	updated_at    INTEGER NOT NULL,
	PRIMARY KEY (item_ref, candidate_ref)
);
CREATE INDEX plugin_matches_chosen ON plugin_matches(item_ref) WHERE chosen = 1;

CREATE VIRTUAL TABLE plugin_item_fts USING fts5(item_ref UNINDEXED, library_id UNINDEXED, text);
`)

	store.RegisterDialect("pluginitems", 1, store.Postgres, `
CREATE TABLE plugin_kinds (
	plugin_id      TEXT   NOT NULL,
	kind_key       TEXT   NOT NULL,
	library_id     BIGINT NOT NULL,
	decl           TEXT   NOT NULL,
	decl_hash      TEXT   NOT NULL,
	schema_version BIGINT NOT NULL DEFAULT 1,
	PRIMARY KEY (plugin_id, kind_key)
);
CREATE INDEX plugin_kinds_library ON plugin_kinds(library_id);

CREATE TABLE plugin_items_seq (
	id   BIGINT PRIMARY KEY,
	next BIGINT NOT NULL
);
INSERT INTO plugin_items_seq(id, next) VALUES(1, 1);

CREATE TABLE plugin_items (
	item_ref     TEXT COLLATE "C" PRIMARY KEY,
	seq          BIGINT           NOT NULL UNIQUE,
	plugin_id    TEXT             NOT NULL,
	kind_key     TEXT             NOT NULL,
	library_id   BIGINT           NOT NULL,
	external_id  TEXT COLLATE "C" NOT NULL,
	parent_ref   TEXT COLLATE "C" NOT NULL DEFAULT '',
	level        TEXT COLLATE "C" NOT NULL,
	idx          BIGINT           NOT NULL DEFAULT 0,
	title        TEXT             NOT NULL,
	sort_title   TEXT COLLATE "C" NOT NULL,
	summary      TEXT             NOT NULL DEFAULT '',
	year         BIGINT           NOT NULL DEFAULT 0,
	released     BIGINT           NOT NULL DEFAULT 0,
	rating       DOUBLE PRECISION NOT NULL DEFAULT 0,
	playable     INTEGER          NOT NULL DEFAULT 0,
	child_count  BIGINT           NOT NULL DEFAULT 0,
	leaf_count   BIGINT           NOT NULL DEFAULT 0,
	hints        TEXT             NOT NULL DEFAULT '{}',
	fields       TEXT             NOT NULL DEFAULT '{}',
	etag         TEXT             NOT NULL DEFAULT '',
	added_at     BIGINT           NOT NULL,
	updated_at   BIGINT           NOT NULL,
	seen_at      BIGINT           NOT NULL,
	missed_walks INTEGER          NOT NULL DEFAULT 0,
	search_tsv   TSVECTOR         NOT NULL DEFAULT ''::tsvector,
	UNIQUE (plugin_id, kind_key, external_id)
);
CREATE INDEX plugin_items_browse       ON plugin_items(library_id, parent_ref, sort_title, item_ref);
CREATE INDEX plugin_items_level_sort   ON plugin_items(library_id, level, sort_title, item_ref);
CREATE INDEX plugin_items_parent       ON plugin_items(parent_ref, idx, sort_title, item_ref);
CREATE INDEX plugin_items_added        ON plugin_items(library_id, added_at DESC, item_ref);
CREATE INDEX plugin_items_released     ON plugin_items(library_id, released DESC, item_ref);
CREATE INDEX plugin_items_year         ON plugin_items(library_id, year DESC, item_ref);
CREATE INDEX plugin_items_rating       ON plugin_items(library_id, rating DESC, item_ref);
CREATE INDEX plugin_items_idx          ON plugin_items(library_id, idx ASC, item_ref);
CREATE INDEX plugin_items_search_title ON plugin_items(library_id, sort_title);
CREATE INDEX plugin_items_search_tsv   ON plugin_items USING gin (search_tsv);

CREATE TABLE plugin_item_fields (
	item_ref TEXT COLLATE "C" NOT NULL,
	field    TEXT             NOT NULL,
	v_text   TEXT             NOT NULL DEFAULT '',
	v_int    BIGINT           NOT NULL DEFAULT 0,
	v_real   DOUBLE PRECISION NOT NULL DEFAULT 0,
	PRIMARY KEY (item_ref, field, v_text, v_int)
);
CREATE INDEX plugin_item_fields_lookup     ON plugin_item_fields(field, v_text, item_ref);
CREATE INDEX plugin_item_fields_lookup_int ON plugin_item_fields(field, v_int, item_ref);

CREATE TABLE plugin_item_art (
	item_ref TEXT COLLATE "C" NOT NULL,
	role     TEXT             NOT NULL,
	url      TEXT             NOT NULL,
	width    BIGINT           NOT NULL DEFAULT 0,
	height   BIGINT           NOT NULL DEFAULT 0,
	PRIMARY KEY (item_ref, role)
);

CREATE TABLE plugin_item_tally (
	library_id BIGINT NOT NULL,
	level      TEXT   NOT NULL,
	n          BIGINT NOT NULL,
	PRIMARY KEY (library_id, level)
);

CREATE TABLE plugin_sync_state (
	plugin_id    TEXT   NOT NULL,
	kind_key     TEXT   NOT NULL,
	cursor       TEXT   NOT NULL DEFAULT '',
	generation   BIGINT NOT NULL DEFAULT 0,
	last_started BIGINT NOT NULL DEFAULT 0,
	last_ok      BIGINT NOT NULL DEFAULT 0,
	last_error   TEXT   NOT NULL DEFAULT '',
	items        BIGINT NOT NULL DEFAULT 0,
	PRIMARY KEY (plugin_id, kind_key)
);

CREATE TABLE plugin_resolutions (
	item_ref   TEXT COLLATE "C" PRIMARY KEY,
	pattern    TEXT             NOT NULL DEFAULT '',
	source     TEXT             NOT NULL DEFAULT '{}',
	negative   INTEGER          NOT NULL DEFAULT 0,
	expires_at BIGINT           NOT NULL
);
CREATE INDEX plugin_resolutions_expires ON plugin_resolutions(expires_at);

CREATE TABLE plugin_matches (
	item_ref      TEXT COLLATE "C" NOT NULL,
	candidate_ref TEXT             NOT NULL,
	score         DOUBLE PRECISION NOT NULL,
	reasons       TEXT             NOT NULL DEFAULT '[]',
	source        TEXT             NOT NULL DEFAULT 'auto',
	chosen        INTEGER          NOT NULL DEFAULT 0,
	updated_at    BIGINT           NOT NULL,
	PRIMARY KEY (item_ref, candidate_ref)
);
CREATE INDEX plugin_matches_chosen ON plugin_matches(item_ref) WHERE chosen = 1;
`)
}

// Caller invokes one plugin route (satisfied by *plugins.Manager).
type Caller interface {
	Call(ctx context.Context, pluginID string, req plugins.Request) (plugins.Response, error)
}

// HealthReporter records plugin health issues discovered by sync or item
// validation (satisfied by *plugins.Manager via RecordFailure, or custom hooks).
type HealthReporter interface {
	RecordFailure(ctx context.Context, pluginID, reason string)
}

// SyncJobHandle receives progress and completion updates for one tracked sync run.
type SyncJobHandle interface {
	SetProgress(progress string)
	Finish(state, code, outcome, errMsg string, failure *plugins.RouteFailure)
}

// SyncJobTracker mirrors active and completed library syncs into the plugin's
// background jobs list so Activity and Overview show live progress and outcome.
type SyncJobTracker interface {
	BeginSyncJob(pluginID, kindKey, routePath, label string, cancel context.CancelFunc) SyncJobHandle
	UpdateSyncJob(pluginID, kindKey, state, code, outcome, errMsg string)
}

// Options configures the pluginitems Service.
type Options struct {
	Logger         *slog.Logger
	Now            func() time.Time
	Caller         Caller
	Health         HealthReporter
	Jobs           SyncJobTracker
	IsLeader       func() bool
	MaxPluginItems int
	// MaxPluginItemsFor returns the live owner-configured stored-item ceiling
	// for pluginID when set, overriding MaxPluginItems.
	MaxPluginItemsFor func(pluginID string) int
	// OnAttention is an optional hook notified when a sync guard or
	// contract error flags a plugin for owner attention (e.g. 50%-delete
	// guard, unknown fields dropped, auth required).
	OnAttention func(ctx context.Context, pluginID, reason string)
	// OnSyncEvent is an optional non-blocking observer for plugin library sync lifecycle.
	OnSyncEvent func(SyncEvent)
}

// SyncEvent describes a plugin library sync lifecycle transition.
type SyncEvent struct {
	Phase       string // "started", "completed", "failed"
	PluginID    string
	KindKey     string
	LibraryID   int64
	LibraryName string
	Trigger     string
	Added       int
	Updated     int
	Removed     int
	DurationMS  int64
	Error       string
}

const maxPinCacheEntries = 16384

// Service manages plugin kinds, items, browse, search, sync, projections,
// matches and resolutions.
type Service struct {
	db                *sql.DB
	log               *slog.Logger
	now               func() time.Time
	caller            Caller
	health            HealthReporter
	jobs              SyncJobTracker
	isLeader          func() bool
	onAttention       func(ctx context.Context, pluginID, reason string)
	onSyncEvent       func(SyncEvent)
	maxPluginItems    int
	maxPluginItemsFor func(pluginID string) int

	syncGen  atomic.Uint64
	writeGen atomic.Uint64

	kindCacheMu  sync.RWMutex
	kindCacheGen uint64
	kindByLib    map[core.LibraryID]KindRecord
	kindByKey    map[string]KindRecord

	partsState atomic.Int32 // 0 = unknown, 1 = empty, 2 = non-empty

	countCacheMu sync.RWMutex
	countCache   map[string]cachedBrowseCount

	locksMu   sync.Mutex
	kindLocks map[string]*sync.Mutex

	syncLocksMu sync.Mutex
	syncLocks   map[string]*sync.Mutex

	bgCtx    context.Context
	bgCancel context.CancelFunc

	workersMu    sync.Mutex
	workers      map[string]*syncWorker
	manualSyncs  map[string]*syncWorker
	draining     map[string]chan struct{}
	reindexers   map[string]*syncWorker
	syncProgress map[string]syncProgress
	lastOutcome  map[string]SyncOutcome

	pinMu   sync.Mutex
	pinSeen map[core.ItemRef]struct{}
	pinCh   chan core.ItemRef
	pinDone chan struct{}

	stats *statsRefresher
}

// New opens the pluginitems Service over a migrated database.
func New(ctx context.Context, db *sql.DB, o Options) (*Service, error) {
	if db == nil {
		return nil, errors.New("pluginitems: db required")
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	if o.Now == nil {
		o.Now = func() time.Time { return time.Now().UTC() }
	}
	if o.MaxPluginItems <= 0 {
		o.MaxPluginItems = DefaultMaxPluginItems
	}
	bgCtx, bgCancel := context.WithCancel(context.Background())
	s := &Service{
		db:                db,
		log:               o.Logger.With("component", "pluginitems"),
		now:               o.Now,
		caller:            o.Caller,
		health:            o.Health,
		jobs:              o.Jobs,
		isLeader:          o.IsLeader,
		onAttention:       o.OnAttention,
		onSyncEvent:       o.OnSyncEvent,
		maxPluginItems:    o.MaxPluginItems,
		maxPluginItemsFor: o.MaxPluginItemsFor,
		kindCacheGen:      pluginItemsRestoreGen.Load(),
		kindByLib:         map[core.LibraryID]KindRecord{},
		kindByKey:         map[string]KindRecord{},
		countCache:        map[string]cachedBrowseCount{},
		kindLocks:         map[string]*sync.Mutex{},
		syncLocks:         map[string]*sync.Mutex{},
		bgCtx:             bgCtx,
		bgCancel:          bgCancel,
		workers:           map[string]*syncWorker{},
		manualSyncs:       map[string]*syncWorker{},
		draining:          map[string]chan struct{}{},
		reindexers:        map[string]*syncWorker{},
		syncProgress:      map[string]syncProgress{},
		lastOutcome:       map[string]SyncOutcome{},
		pinSeen:           map[core.ItemRef]struct{}{},
		pinCh:             make(chan core.ItemRef, 256),
		pinDone:           make(chan struct{}),
	}
	// Seed the sequence row if a test opened the table without data, and
	// load the highest stored sync generation so PageFreshness never rewinds.
	if _, err := db.ExecContext(ctx, `INSERT INTO plugin_items_seq(id, next) VALUES(1, 1) ON CONFLICT(id) DO NOTHING`); err != nil {
		bgCancel()
		return nil, fmt.Errorf("pluginitems: seed seq: %w", err)
	}
	if err := s.reindexPendingKinds(ctx); err != nil {
		bgCancel()
		return nil, fmt.Errorf("pluginitems: reindex pending kinds: %w", err)
	}
	s.scheduleStaleArtistReindexes(ctx)
	s.ensureParentTallies(ctx)
	var maxGen int64
	if err := db.QueryRowContext(ctx, `SELECT COALESCE(MAX(generation), 0) FROM plugin_sync_state`).Scan(&maxGen); err != nil {
		bgCancel()
		return nil, fmt.Errorf("pluginitems: load sync generation: %w", err)
	}
	if maxGen > 0 {
		s.syncGen.Store(uint64(maxGen))
	}
	s.stats = newStatsRefresher(ctx, db, s.log, s.now, s.leader)
	go s.runPinWorker()
	return s, nil
}

// Now returns the service clock's current UTC time.
func (s *Service) Now() time.Time {
	if s == nil || s.now == nil {
		return time.Now().UTC()
	}
	return s.now()
}

// SetCaller sets or replaces the plugin route caller after construction.
func (s *Service) SetCaller(c Caller) {
	s.workersMu.Lock()
	s.caller = c
	s.workersMu.Unlock()
}

// SetHealth sets or replaces the health reporter after construction.
func (s *Service) SetHealth(h HealthReporter) {
	s.workersMu.Lock()
	s.health = h
	s.workersMu.Unlock()
}

// SetJobTracker installs or replaces the background job tracker used to mirror
// library sync runs into the plugin's Activity / Background jobs list.
func (s *Service) SetJobTracker(t SyncJobTracker) {
	s.workersMu.Lock()
	s.jobs = t
	s.workersMu.Unlock()
}

func (s *Service) jobTracker() SyncJobTracker {
	if s == nil {
		return nil
	}
	s.workersMu.Lock()
	t := s.jobs
	s.workersMu.Unlock()
	return t
}

func (s *Service) syncLock(pluginID, kindKey string) *sync.Mutex {
	key := workerKey(pluginID, kindKey)
	s.syncLocksMu.Lock()
	defer s.syncLocksMu.Unlock()
	if s.syncLocks == nil {
		s.syncLocks = map[string]*sync.Mutex{}
	}
	mu := s.syncLocks[key]
	if mu == nil {
		mu = &sync.Mutex{}
		s.syncLocks[key] = mu
	}
	return mu
}

// PinItem marks one stored plugin item as pinned so missed-walk pruning never
// removes it simply because a subsequent /browse walk omitted it.
func (s *Service) PinItem(ctx context.Context, ref core.ItemRef) error {
	if s == nil || s.db == nil || ref == "" {
		return nil
	}
	if _, err := s.db.ExecContext(ctx,
		`UPDATE plugin_items SET pinned = 1 WHERE item_ref = ? AND pinned = 0`,
		string(ref)); err != nil {
		return err
	}
	s.rememberPinned(ref)
	return nil
}

// PinItemAsync records that a viewer opened or resolved ref, persisting
// pinned=1 on a single background worker with zero read-path latency.
func (s *Service) PinItemAsync(ref core.ItemRef) {
	if s == nil || s.db == nil || ref == "" || s.pinCh == nil {
		return
	}
	s.pinMu.Lock()
	if _, ok := s.pinSeen[ref]; ok {
		s.pinMu.Unlock()
		return
	}
	if len(s.pinSeen) >= maxPinCacheEntries {
		s.pinSeen = map[core.ItemRef]struct{}{}
	}
	s.pinSeen[ref] = struct{}{}
	s.pinMu.Unlock()

	select {
	case s.pinCh <- ref:
	default:
		// Queue full; drop cache entry so a later open retries.
		s.pinMu.Lock()
		delete(s.pinSeen, ref)
		s.pinMu.Unlock()
	}
}

func (s *Service) rememberPinned(ref core.ItemRef) {
	if s == nil || ref == "" {
		return
	}
	s.pinMu.Lock()
	if s.pinSeen == nil || len(s.pinSeen) >= maxPinCacheEntries {
		s.pinSeen = map[core.ItemRef]struct{}{}
	}
	s.pinSeen[ref] = struct{}{}
	s.pinMu.Unlock()
}

func (s *Service) forgetPinnedRefs(refs []core.ItemRef) {
	if s == nil || len(refs) == 0 {
		return
	}
	s.pinMu.Lock()
	for _, r := range refs {
		delete(s.pinSeen, r)
	}
	s.pinMu.Unlock()
}

func (s *Service) clearPinnedCache() {
	if s == nil {
		return
	}
	s.pinMu.Lock()
	s.pinSeen = map[core.ItemRef]struct{}{}
	s.pinMu.Unlock()
}

func (s *Service) runPinWorker() {
	defer close(s.pinDone)
	for {
		select {
		case <-s.bgCtx.Done():
			return
		case ref := <-s.pinCh:
			if ref == "" || !s.leader() {
				continue
			}
			ctx, cancel := context.WithTimeout(s.bgCtx, 5*time.Second)
			_, err := s.db.ExecContext(ctx,
				`UPDATE plugin_items SET pinned = 1 WHERE item_ref = ? AND pinned = 0`,
				string(ref))
			cancel()
			if err != nil {
				s.pinMu.Lock()
				delete(s.pinSeen, ref)
				s.pinMu.Unlock()
			}
		}
	}
}

// SetOnSyncEvent installs an optional observer for plugin library sync lifecycle.
func (s *Service) SetOnSyncEvent(fn func(SyncEvent)) {
	s.workersMu.Lock()
	s.onSyncEvent = fn
	s.workersMu.Unlock()
}

func (s *Service) emitSyncEvent(ev SyncEvent) {
	s.workersMu.Lock()
	fn := s.onSyncEvent
	s.workersMu.Unlock()
	if fn != nil {
		fn(ev)
	}
}

// Generation returns the monotonic sync generation across all plugin kinds
// (spec §3.5, §6.4: feeds frontend.PageFreshness.KindSync).
func (s *Service) Generation() uint64 {
	if s == nil {
		return 0
	}
	return s.syncGen.Load()
}

// SyncGeneration is an alias for Generation (feeds PageFreshness.KindSync).
func (s *Service) SyncGeneration() uint64 {
	return s.Generation()
}

// BumpGeneration increments and returns the monotonic sync generation so
// cached frontend ETag/revision scopes observe completed background expansions
// even when zero new items were added.
func (s *Service) BumpGeneration() uint64 {
	if s == nil {
		return 0
	}
	return s.syncGen.Add(1)
}

// MaxItemsFor returns the effective stored-item cap across all libraries owned
// by pluginID, consulting MaxPluginItemsFor when wired.
func (s *Service) MaxItemsFor(pluginID string) int {
	if s == nil {
		return DefaultMaxPluginItems
	}
	if s.maxPluginItemsFor != nil {
		if n := s.maxPluginItemsFor(pluginID); n > 0 {
			return n
		}
	}
	if s.maxPluginItems > 0 {
		return s.maxPluginItems
	}
	return DefaultMaxPluginItems
}

// KindBackends returns a plugins.KindBackends struct with KindUpsert,
// KindDelete, KindItems, KindItemsByID and KindItemCount wired to this store (spec §3.6, §12 slice C).
func (s *Service) KindBackends() plugins.KindBackends {
	return plugins.KindBackends{
		KindUpsert:      s.KindUpsert,
		KindUpsertStats: s.KindUpsertWithStats,
		KindUpsertUndo:  s.KindUpsertWithUndo,
		KindDelete:      s.KindDelete,
		KindItems:       s.KindItems,
		KindItemsByID:   s.KindItemsByID,
		KindItemCount:   s.PluginItemCount,
	}
}

func (s *Service) kindLock(pluginID, kindKey string) *sync.Mutex {
	key := pluginID + "\x00" + kindKey
	s.locksMu.Lock()
	defer s.locksMu.Unlock()
	mu, ok := s.kindLocks[key]
	if !ok {
		mu = &sync.Mutex{}
		s.kindLocks[key] = mu
	}
	return mu
}

const maxCountCacheEntries = 1024

type cachedBrowseCount struct {
	total  int
	gen    uint64
	minute int64
}

// WriteGen returns a monotonically increasing counter bumped on every item
// write or purge so request-scoped caches can key safely on store state.
func (s *Service) WriteGen() uint64 {
	if s == nil {
		return 0
	}
	return s.writeGen.Load()
}

func (s *Service) noteWrite() {
	if s != nil {
		s.writeGen.Add(1)
	}
}

func (s *Service) countMinuteBucket() int64 {
	return s.Now().Unix() / 60
}

func (s *Service) countCacheGet(key string, gen uint64, minute int64) (int, bool) {
	if s == nil {
		return 0, false
	}
	s.countCacheMu.RLock()
	ent, ok := s.countCache[key]
	s.countCacheMu.RUnlock()
	if !ok || ent.gen != gen || ent.minute != minute {
		return 0, false
	}
	return ent.total, true
}

func (s *Service) countCachePut(key string, total int, gen uint64, minute int64) {
	if s == nil || s.writeGen.Load() != gen {
		return
	}
	s.countCacheMu.Lock()
	if s.countCache == nil || len(s.countCache) >= maxCountCacheEntries {
		s.countCache = map[string]cachedBrowseCount{}
	}
	s.countCache[key] = cachedBrowseCount{total: total, gen: gen, minute: minute}
	s.countCacheMu.Unlock()
}

func browseCountCacheKey(prefix, where string, args []any) string {
	var b strings.Builder
	b.Grow(len(prefix) + len(where) + len(args)*16)
	b.WriteString(prefix)
	b.WriteByte(0)
	b.WriteString(where)
	for _, a := range args {
		b.WriteByte(0)
		fmt.Fprintf(&b, "%T:%v", a, a)
	}
	return b.String()
}

func (s *Service) noteAttention(ctx context.Context, pluginID, reason string) {
	s.workersMu.Lock()
	att := s.onAttention
	hlth := s.health
	s.workersMu.Unlock()
	if att != nil {
		att(ctx, pluginID, reason)
	}
	if hlth != nil {
		hlth.RecordFailure(ctx, pluginID, reason)
	}
}

func (s *Service) leader() bool {
	if s.isLeader == nil {
		return true
	}
	return s.isLeader()
}
