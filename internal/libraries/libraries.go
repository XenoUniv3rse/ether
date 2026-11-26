// Package libraries owns the library declarations. Movies (1) and TV Shows
// (2) are seeded once as ordinary, editable and deletable catalogue libraries.
package libraries

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync/atomic"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/libraryfilter"
	"github.com/braedonsaunders/ether/internal/store"
)

func init() {
	const hideLibraryTab = `ALTER TABLE libraries ADD COLUMN hide_library_tab INTEGER NOT NULL DEFAULT 0;`
	store.Register("libraries", 17, hideLibraryTab)
	store.RegisterDialect("libraries", 17, store.Postgres, hideLibraryTab)
	const metadataSource = `ALTER TABLE libraries ADD COLUMN metadata_source TEXT NOT NULL DEFAULT '';`
	store.Register("libraries", 16, metadataSource)
	store.RegisterDialect("libraries", 16, store.Postgres, metadataSource)
	const mig13 = `
ALTER TABLE libraries ADD COLUMN present_overrides TEXT NOT NULL DEFAULT '{}';
UPDATE libraries SET present_overrides = '{"adapterOnly":true}' WHERE source = 'plugin' AND present LIKE '%"adapterOnly":true%';
`
	store.Register("libraries", 13, mig13)
	store.RegisterDialect("libraries", 13, store.Postgres, mig13)
	store.Register("libraries", 12, `ALTER TABLE libraries ADD COLUMN card_shape TEXT NOT NULL DEFAULT '';`)
	store.RegisterDialect("libraries", 12, store.Postgres, `ALTER TABLE libraries ADD COLUMN card_shape TEXT NOT NULL DEFAULT '';`)
	store.Register("libraries", 10, "SELECT 1;")
	store.RegisterApply("libraries", 10, migrateVelaPresentation)
	store.Register("libraries", 9, "ALTER TABLE libraries ADD COLUMN deleted INTEGER NOT NULL DEFAULT 0;")
	store.RegisterDialect("libraries", 9, store.Postgres, "ALTER TABLE libraries ADD COLUMN deleted INTEGER NOT NULL DEFAULT 0;")
	store.RegisterAfter("libraries", "catalog_viewer_rules")
	libraryfilter.RegisterMediaTypeMigration("libraries", 8, "libraries", "rules", "id")
	store.Register("libraries", 1, `
CREATE TABLE libraries (
	id         INTEGER PRIMARY KEY,
	key        TEXT    NOT NULL UNIQUE,
	name       TEXT    NOT NULL,
	source     TEXT    NOT NULL,
	media_type TEXT    NOT NULL DEFAULT '',
	plugin_id  TEXT    NOT NULL DEFAULT '',
	kind_key   TEXT    NOT NULL DEFAULT '',
	filter     TEXT    NOT NULL DEFAULT '',
	present    TEXT    NOT NULL DEFAULT '{}',
	sort       INTEGER NOT NULL DEFAULT 0,
	hidden     INTEGER NOT NULL DEFAULT 0,
	created_at INTEGER NOT NULL,
	updated_at INTEGER NOT NULL
);
CREATE UNIQUE INDEX libraries_plugin_kind ON libraries(plugin_id, kind_key) WHERE source = 'plugin';
CREATE TABLE libraries_seq (id INTEGER PRIMARY KEY, next INTEGER NOT NULL);
CREATE TABLE libraries_state (k TEXT PRIMARY KEY, v TEXT NOT NULL DEFAULT '');
`)
	store.RegisterDialect("libraries", 1, store.Postgres, `
CREATE TABLE libraries (
	id         BIGINT PRIMARY KEY,
	key        TEXT    NOT NULL UNIQUE,
	name       TEXT    NOT NULL,
	source     TEXT    NOT NULL,
	media_type TEXT    NOT NULL DEFAULT '',
	plugin_id  TEXT    NOT NULL DEFAULT '',
	kind_key   TEXT    NOT NULL DEFAULT '',
	filter     TEXT    NOT NULL DEFAULT '',
	present    TEXT    NOT NULL DEFAULT '{}',
	sort       BIGINT  NOT NULL DEFAULT 0,
	hidden     INTEGER NOT NULL DEFAULT 0,
	created_at BIGINT  NOT NULL,
	updated_at BIGINT  NOT NULL
);
CREATE UNIQUE INDEX libraries_plugin_kind ON libraries(plugin_id, kind_key) WHERE source = 'plugin';
CREATE TABLE libraries_seq (id BIGINT PRIMARY KEY, next BIGINT NOT NULL);
CREATE TABLE libraries_state (k TEXT PRIMARY KEY, v TEXT NOT NULL DEFAULT '');
`)
	store.Register("libraries", 2, `
ALTER TABLE libraries ADD COLUMN rules TEXT NOT NULL DEFAULT '[]';
ALTER TABLE libraries ADD COLUMN visibility_default TEXT NOT NULL DEFAULT '';
`)
	store.RegisterDialect("libraries", 2, store.Postgres, `
ALTER TABLE libraries ADD COLUMN rules TEXT NOT NULL DEFAULT '[]';
ALTER TABLE libraries ADD COLUMN visibility_default TEXT NOT NULL DEFAULT '';
`)
	store.Register("libraries", 3, `
ALTER TABLE libraries ADD COLUMN quality_profile TEXT NOT NULL DEFAULT '';
`)
	store.RegisterDialect("libraries", 3, store.Postgres, `
ALTER TABLE libraries ADD COLUMN quality_profile TEXT NOT NULL DEFAULT '';
`)
	// A collection-source library stores the id of the owner collection
	// whose titles it shows (an MDBList list, a Trakt list, a smart rule…).
	store.Register("libraries", 4, `
ALTER TABLE libraries ADD COLUMN collection_id INTEGER NOT NULL DEFAULT 0;
`)
	store.RegisterDialect("libraries", 4, store.Postgres, `
ALTER TABLE libraries ADD COLUMN collection_id BIGINT NOT NULL DEFAULT 0;
`)
}

// migrateVelaPresentation populates present.vela once on existing stored
// libraries from MediaBrowser (movies/tvshows), else Plex (movie/show), so
// every library Vela showed before upgrade stays published to Vela.
func migrateVelaPresentation(ctx context.Context, tx *sql.Tx) error {
	rows, err := tx.QueryContext(ctx, `SELECT id, present FROM libraries`)
	if err != nil {
		return err
	}
	type edit struct {
		id      int64
		present string
	}
	var edits []edit
	for rows.Next() {
		var id int64
		var raw string
		if err := rows.Scan(&id, &raw); err != nil {
			rows.Close()
			return err
		}
		if strings.TrimSpace(raw) == "" {
			continue
		}
		var p core.Presentations
		if err := json.Unmarshal([]byte(raw), &p); err != nil {
			rows.Close()
			return err
		}
		velaType := inferVelaType(p)
		if velaType == "" {
			continue
		}
		p.Vela = &core.VelaPresentation{Type: velaType}
		b, err := json.Marshal(p)
		if err != nil {
			rows.Close()
			return err
		}
		edits = append(edits, edit{id: id, present: string(b)})
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, e := range edits {
		if _, err := tx.ExecContext(ctx, `UPDATE libraries SET present=? WHERE id=?`, e.present, e.id); err != nil {
			return err
		}
	}
	return nil
}

func inferVelaType(p core.Presentations) string {
	if p.AdapterOnly || p.Vela != nil {
		return ""
	}
	if p.MediaBrowser != nil {
		switch p.MediaBrowser.CollectionType {
		case "movies":
			return "movies"
		case "tvshows":
			return "shows"
		}
	}
	if p.Plex != nil {
		switch p.Plex.Base {
		case "movie":
			return "movies"
		case "show":
			return "shows"
		}
	}
	return ""
}

var (
	// ErrDeleted means the owner must explicitly restore this plugin/kind.
	ErrDeleted = errors.New("libraries: library deleted by owner")
	// ErrBuiltinLibrary is retained for compatibility with older callers.
	// Delete no longer returns it.
	ErrBuiltinLibrary = errors.New("libraries: built-in library cannot be deleted")
	// ErrInvalid is a rejected declaration: empty name, an unslugifiable
	// name, or a plugin library without both ids.
	ErrInvalid = errors.New("libraries: invalid library")
)

// PluginLibraryDecl is what a plugin kind asks for at install (§2.5):
// the default display name and presentation. Owner edits made after
// install win: an upsert of an existing (plugin, kind) never
// overwrites the stored name or presentation.
type PluginLibraryDecl struct {
	Name    string
	Present core.Presentations
}

// Options configures the Service.
type Options struct {
	CompileCataloguePolicy func([]core.LibraryExclusion, string) (*libraryfilter.Filter, error)
	CompilePluginPolicy    func(context.Context, core.LibraryID, []core.LibraryExclusion, string) (*libraryfilter.Filter, error)
	Logger                 *slog.Logger
	Now                    func() time.Time
}

// Service is the libraries store. List/Get are plain indexed reads of
// a table that holds one row per library, never per item, so no call
// scales with library size.
type Service struct {
	compileCataloguePolicy func([]core.LibraryExclusion, string) (*libraryfilter.Filter, error)
	compilePluginPolicy    func(context.Context, core.LibraryID, []core.LibraryExclusion, string) (*libraryfilter.Filter, error)
	db                     *sql.DB
	log                    *slog.Logger
	now                    func() time.Time
	gen                    atomic.Uint64
	snap                   atomic.Pointer[snapshot]
}

// New seeds the initial catalogue libraries once per database and returns
// the service. db must already be migrated (store.Migrate), which is
// how every component table is opened.
func New(ctx context.Context, db *sql.DB, o Options) (*Service, error) {
	if db == nil {
		return nil, errors.New("libraries: db required")
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	s := &Service{compileCataloguePolicy: o.CompileCataloguePolicy, compilePluginPolicy: o.CompilePluginPolicy, db: db, log: o.Logger.With("component", "libraries"), now: o.Now}
	if err := s.seedBuiltins(ctx); err != nil {
		return nil, fmt.Errorf("libraries: seed builtins: %w", err)
	}
	if err := s.convertPredicates(ctx); err != nil {
		return nil, err
	}
	libs, err := s.listDB(ctx)
	if err != nil {
		return nil, err
	}
	for _, lib := range libs {
		if lib.RulesBroken {
			s.log.Warn("broken library rules", "library_id", lib.ID, "name", lib.Name)
		}
		data, _ := json.Marshal(lib.Sources)
		if _, err = s.db.ExecContext(ctx, `UPDATE libraries SET sources=? WHERE id=? AND sources='[]'`, string(data), int64(lib.ID)); err != nil {
			return nil, err
		}
	}
	return s, nil
}

// Generation changes on every library mutation; it feeds
// frontend.PageFreshness (§6.4).
func (s *Service) Generation() uint64 {
	if s == nil {
		return 0
	}
	return s.gen.Load()
}

func (s *Service) seedBuiltins(ctx context.Context) error {
	now := s.now().Unix()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// Claim initialization in the same transaction as the rows. An empty table
	// after this marker exists is an intentional owner choice.
	claimed, err := tx.ExecContext(ctx, `INSERT INTO libraries_state(k,v) VALUES('defaults_seeded','1') ON CONFLICT(k) DO NOTHING`)
	if err != nil {
		return err
	}
	n, err := claimed.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return tx.Commit()
	}
	for _, b := range []struct {
		id        core.LibraryID
		key, name string
		mediaType core.MediaType
		sort      int
	}{
		{core.LibraryMovies, "movies", "Movies", core.TypeMovie, 1},
		{core.LibraryShows, "shows", "TV Shows", core.TypeShow, 2},
	} {
		if _, err := tx.ExecContext(ctx, `INSERT INTO libraries(id, key, name, source, media_type, sort, created_at, updated_at)
VALUES(?, ?, ?, 'builtin', ?, ?, ?, ?) ON CONFLICT(id) DO NOTHING`,
			int64(b.id), b.key, b.name, string(b.mediaType), b.sort, now, now); err != nil {
			return err
		}
	}
	// The next free id, past whatever already exists; INSERT of a
	// fresh row only, so a later open never rewinds the counter. The
	// WHERE keeps SQLite from reading ON CONFLICT as a join's ON.
	if _, err := tx.ExecContext(ctx, `INSERT INTO libraries_seq(id, next)
SELECT 1, COALESCE(MAX(id), 2) + 1 FROM libraries WHERE 1=1 ON CONFLICT(id) DO NOTHING`); err != nil {
		return err
	}
	// Preserve explicit owner presentations; materialize only the legacy
	// implicit all-apps default before removing the builtin source distinction.
	rows, err := tx.QueryContext(ctx, `SELECT id, media_type, present FROM libraries WHERE source='builtin'`)
	if err != nil {
		return err
	}
	type upgrade struct {
		id      int64
		present string
	}
	var upgrades []upgrade
	for rows.Next() {
		var id int64
		var mt, raw string
		if err := rows.Scan(&id, &mt, &raw); err != nil {
			rows.Close()
			return err
		}
		var p core.Presentations
		if err := json.Unmarshal([]byte(raw), &p); err != nil {
			rows.Close()
			return err
		}
		if !p.AdapterOnly && p.Vela == nil && p.Plex == nil && p.MediaBrowser == nil && p.Stremio == nil && p.Xtream == nil && p.Silo == nil {
			raw = defaultPresentation(core.MediaType(mt))
		}
		upgrades = append(upgrades, upgrade{id, raw})
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, u := range upgrades {
		if _, err := tx.ExecContext(ctx, `UPDATE libraries SET source='catalogue', present=? WHERE id=?`, u.present, u.id); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// nextID allocates a never-reused library id inside tx (§1.3: a
// libraries_seq row in the same component, no AUTOINCREMENT
// divergence between dialects).
func nextID(ctx context.Context, tx *sql.Tx) (core.LibraryID, error) {
	if _, err := tx.ExecContext(ctx, `UPDATE libraries_seq SET next = next + 1 WHERE id = 1`); err != nil {
		return 0, err
	}
	var next int64
	if err := tx.QueryRowContext(ctx, `SELECT next FROM libraries_seq WHERE id = 1`).Scan(&next); err != nil {
		return 0, err
	}
	return core.LibraryID(next - 1), nil
}

// compileCatalogueRules uses the exact bound serving compiler when wired by
// the app. Standalone stores can still validate the portable rule language.
func (s *Service) compileCatalogueRules(rules []core.LibraryExclusion, fallback string) (*libraryfilter.Filter, error) {
	if s.compileCataloguePolicy != nil {
		return s.compileCataloguePolicy(rules, fallback)
	}
	return libraryfilter.CompilePolicy(rules, fallback)
}

func defaultPresentation(mt core.MediaType) string {
	if mt == core.TypeShow {
		return `{"vela":{"type":"shows"},"plex":{"base":"show"},"mediaBrowser":{"collectionType":"tvshows"},"stremio":{"type":"series"},"xtream":{"category":"series"},"silo":{"type":"tvshows"}}`
	}
	return `{"vela":{"type":"movies"},"plex":{"base":"movie"},"mediaBrowser":{"collectionType":"movies"},"stremio":{"type":"movie"},"xtream":{"category":"movie"},"silo":{"type":"movies"}}`
}
