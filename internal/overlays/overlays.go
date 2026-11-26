// Package overlays creates truthful, versioned poster variants in pure Go.
// Quality badges require observed release facts; metadata estimates are never
// treated as evidence of 4K/HDR/Dolby Vision/Atmos availability.
package overlays

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/braedonsaunders/ether/internal/artwork"
	"github.com/braedonsaunders/ether/internal/contentrating"
	"github.com/braedonsaunders/ether/internal/core"
	kitimages "github.com/braedonsaunders/ether/internal/frontend/kit/images"
	"github.com/braedonsaunders/ether/internal/ratings"
	"github.com/braedonsaunders/ether/internal/reqmemo"
	"github.com/braedonsaunders/ether/internal/store"
	xdraw "golang.org/x/image/draw"
	"golang.org/x/image/font/gofont/gobold"
	"golang.org/x/image/font/opentype"
	"image"
	"image/draw"
	"image/jpeg"
	_ "image/png"
	"net/url"
	"strings"
	"sync"
	"time"
)

func init() {
	store.Register("overlays", 2, `CREATE TABLE IF NOT EXISTS plugin_poster_badges(plugin_id TEXT NOT NULL,set_key TEXT NOT NULL,title_key TEXT NOT NULL,labels_json TEXT NOT NULL,updated_at INTEGER NOT NULL,PRIMARY KEY(plugin_id,set_key,title_key));CREATE INDEX IF NOT EXISTS plugin_poster_badges_title_key ON plugin_poster_badges(title_key);`)
	store.Register("overlays", 1, `CREATE TABLE overlays_settings(id INTEGER PRIMARY KEY CHECK(id=1),value TEXT NOT NULL);CREATE TABLE overlays_artwork(item_key INTEGER PRIMARY KEY,value TEXT NOT NULL);`)
}

type Settings struct {
	Enabled           bool     `json:"enabled"`
	EpisodeBadges     *bool    `json:"episodeBadges"`
	Status            bool     `json:"status"`
	Quality           bool     `json:"quality"`
	Ratings           bool     `json:"ratings"`
	RatingStyle       string   `json:"ratingStyle"`
	Details           bool     `json:"details"`
	Personalize       bool     `json:"personalize"`
	Localize          bool     `json:"localize"`
	Style             string   `json:"style"`
	Position          string   `json:"position"`
	Accent            string   `json:"accent"`
	Size              string   `json:"size"`
	HideDenominator   bool     `json:"hideDenominator"`
	HideDot           bool     `json:"hideDot"`
	DisabledBadgeSets []string `json:"disabledBadgeSets,omitempty"`
}

func (v Settings) BadgesOnEpisodes() bool { return v.EpisodeBadges == nil || *v.EpisodeBadges }

type Variant struct {
	Path     string   `json:"path"`
	Language string   `json:"language,omitempty"`
	Tags     []string `json:"tags,omitempty"`
}
type Artwork struct {
	Posters        []Variant `json:"posters"`
	Backdrops      []Variant `json:"backdrops"`
	PinnedPoster   string    `json:"pinnedPoster,omitempty"`
	PinnedBackdrop string    `json:"pinnedBackdrop,omitempty"`
}
type Facts struct {
	NewEpisode  bool
	NewSeason   bool
	Returning   bool
	LastChance  time.Time
	Width       int
	Height      int
	HDR         bool
	DolbyVision bool
	Atmos       bool
}
type ImageSource interface {
	Get(context.Context, string, int) ([]byte, string, error)
}
type posterSource interface {
	GetPoster(context.Context, *core.Title, string, int) ([]byte, string, error)
	PosterVersion() string
}

func (s *Service) CustomPostersEnabled() bool { return s.posterVersion() != "" }
func (s *Service) posterVersion() string {
	if p, ok := s.o.Images.(posterSource); ok {
		return p.PosterVersion()
	}
	return ""
}
func (s *Service) PosterGeneration() uint64 {
	if p, ok := s.o.Images.(interface{ PosterGeneration() uint64 }); ok {
		return p.PosterGeneration()
	}
	return 0
}

type Options struct {
	// CacheDir contains disposable generated variants only. Empty disables disk caching.
	CacheDir string
	// Zero selects defaults (16 MiB memory, 256 MiB disk); negative disables a tier.
	MemoryCacheBytes int64
	DiskCacheBytes   int64
	// RenderConcurrency bounds CPU-heavy cold work (default 2, maximum 16).
	RenderConcurrency int
	Images            ImageSource
	Allowed           func(*core.User, *core.Title) bool
	Facts             func(context.Context, *core.User, *core.Title) Facts
	FactsFor          func(context.Context, *core.User, *core.Title, bool, bool) Facts
	PreparePage       func(context.Context, *core.User, []core.Title, bool, bool) context.Context
	Ratings           *ratings.Service
	Taste             func(context.Context, *core.User) []string
	Now               func() time.Time
	Variants          func(context.Context, *core.Title, string) (Artwork, error)
	PluginBadges      func(context.Context) []PluginBadgeSet
}
type Service struct {
	db         *sql.DB
	o          Options
	generated  *posterCache
	parsedFont *opentype.Font
	fontErr    error
	sampleOnce sync.Once
	sampleData []byte
	sampleErr  error
}

func New(db *sql.DB, o Options) *Service {
	if o.Now == nil {
		o.Now = time.Now
	}
	f, err := opentype.Parse(gobold.TTF)
	return &Service{db: db, o: o, generated: newPosterCache(o), parsedFont: f, fontErr: err}
}

type previewSettingsKey struct{}

func (s *Service) Settings(ctx context.Context) (Settings, error) {
	if v, ok := ctx.Value(previewSettingsKey{}).(Settings); ok {
		return normalizeSettings(v), nil
	}
	if snap, ok := ctx.Value(settingsSnapshotKey{s}).(settingsSnapshot); ok {
		return snap.value, snap.err
	}
	var v Settings
	// Artwork for each listed item asks: read the row once per request.
	row, e := reqmemo.Do(ctx, "overlays:settings", func() (*string, error) {
		var raw string
		err := s.db.QueryRowContext(ctx, `SELECT value FROM overlays_settings WHERE id=1`).Scan(&raw)
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return &raw, err
	})
	if e != nil {
		return v, e
	}
	if row == nil {
		return normalizeSettings(v), nil
	}
	raw := *row
	e = json.Unmarshal([]byte(raw), &v)
	return normalizeSettings(v), e
}
func (s *Service) SaveSettings(ctx context.Context, v Settings) error {
	reqmemo.Forget(ctx, "overlays:settings")
	v = normalizeSettings(v)
	if err := validateSettings(v); err != nil {
		return err
	}
	raw, e := json.Marshal(v)
	if e != nil {
		return e
	}
	_, e = s.db.ExecContext(ctx, `INSERT INTO overlays_settings(id,value) VALUES(1,?) ON CONFLICT(id) DO UPDATE SET value=excluded.value`, string(raw))
	return e
}

type artworkPageKey struct{}

func (s *Service) withArtworkOnly(ctx context.Context, keys []core.RatingKey) (context.Context, error) {
	if s == nil {
		return ctx, nil
	}
	ctx = s.WithSettingsSnapshot(ctx)
	if prev, ok := ctx.Value(artworkPageKey{}).(map[core.RatingKey]Artwork); ok {
		var missing []core.RatingKey
		seen := make(map[core.RatingKey]bool, len(keys))
		for _, k := range keys {
			if k == 0 || seen[k] {
				continue
			}
			seen[k] = true
			if _, have := prev[k]; !have {
				missing = append(missing, k)
			}
		}
		if len(missing) == 0 {
			return ctx, nil
		}
		batch, err := s.ArtworkMany(ctx, missing)
		if err != nil {
			return ctx, err
		}
		merged := make(map[core.RatingKey]Artwork, len(prev)+len(batch))
		for k, v := range prev {
			merged[k] = v
		}
		for k, v := range batch {
			merged[k] = v
		}
		return context.WithValue(ctx, artworkPageKey{}, merged), nil
	}
	batch, err := s.ArtworkMany(ctx, keys)
	if err != nil {
		return ctx, err
	}
	return context.WithValue(ctx, artworkPageKey{}, batch), nil
}

// WithArtworkPage reads saved variants and active plugin badges for a grid once
// and keeps settings on the context so each tile does not query them again.
func (s *Service) WithArtworkPage(ctx context.Context, keys []core.RatingKey) (context.Context, error) {
	if s == nil {
		return ctx, nil
	}
	var err error
	ctx, err = s.withArtworkOnly(ctx, keys)
	if err != nil {
		return ctx, err
	}
	ctx = s.withPluginBadgesForKeys(ctx, keys)
	return ctx, nil
}

// WithTitlePage prepares settings, saved variants, cached ratings, viewer
// overlay facts, and active plugin badges once for a page of titles so per-tile
// versioning and selection perform no per-item database queries.
func (s *Service) WithTitlePage(ctx context.Context, viewer *core.User, titles []core.Title) (context.Context, error) {
	if s == nil {
		return ctx, nil
	}
	keys := make([]core.RatingKey, len(titles))
	for i := range titles {
		keys[i] = titles[i].Key
	}
	var err error
	ctx, err = s.withArtworkOnly(ctx, keys)
	if err != nil {
		return ctx, err
	}
	cfg, err := s.Settings(ctx)
	if err != nil || !cfg.Enabled || len(titles) == 0 {
		return ctx, nil
	}
	ctx = s.withPluginBadgesForTitles(ctx, cfg, titles)
	if cfg.Ratings && s.o.Ratings != nil {
		ctx = s.o.Ratings.WithPage(ctx, titles)
	}
	if (cfg.Status || cfg.Quality) && s.o.PreparePage != nil {
		ctx = s.o.PreparePage(ctx, viewer, titles, cfg.Status, cfg.Quality)
	}
	return ctx, nil
}

func (s *Service) ArtworkMany(ctx context.Context, keys []core.RatingKey) (map[core.RatingKey]Artwork, error) {
	out := make(map[core.RatingKey]Artwork, len(keys))
	for _, key := range keys {
		out[key] = Artwork{}
	}
	if s == nil || s.db == nil || len(keys) == 0 {
		return out, nil
	}
	{
		numeric := make([]int64, len(keys))
		for n, k := range keys {
			numeric[n] = int64(k)
		}
		data, err := json.Marshal(numeric)
		if err != nil {
			return nil, err
		}
		source := "SELECT value FROM json_each(?)"
		if store.DialectOf(s.db) == store.Postgres {
			source = "SELECT CAST(value AS BIGINT) FROM json_array_elements_text(CAST(? AS JSON))"
		}
		rows, err := s.db.QueryContext(ctx, `SELECT item_key, value FROM overlays_artwork WHERE item_key IN (`+source+`)`, string(data))
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var key int64
			var raw string
			if err = rows.Scan(&key, &raw); err != nil {
				rows.Close()
				return nil, err
			}
			var art Artwork
			if err = json.Unmarshal([]byte(raw), &art); err != nil {
				rows.Close()
				return nil, err
			}
			out[core.RatingKey(key)] = art
		}
		if err = rows.Err(); err != nil {
			rows.Close()
			return nil, err
		}
		rows.Close()
	}
	return out, nil
}

func (s *Service) Artwork(ctx context.Context, key core.RatingKey) (Artwork, error) {
	if batch, ok := ctx.Value(artworkPageKey{}).(map[core.RatingKey]Artwork); ok {
		return batch[key], nil
	}
	var raw string
	var v Artwork
	e := s.db.QueryRowContext(ctx, `SELECT value FROM overlays_artwork WHERE item_key=?`, key).Scan(&raw)
	if errors.Is(e, sql.ErrNoRows) {
		return v, nil
	}
	if e != nil {
		return v, e
	}
	e = json.Unmarshal([]byte(raw), &v)
	return v, e
}
func validVariantPath(path string) bool {
	if artwork.ValidPath(path) {
		return true
	}
	u, err := url.Parse(path)
	return err == nil && u.Scheme == "https" && u.Host == "artworks.thetvdb.com" &&
		u.User == nil && !u.ForceQuery && u.RawQuery == "" && u.Fragment == "" && u.Path != ""
}

func (s *Service) SaveArtwork(ctx context.Context, key core.RatingKey, v Artwork) error {
	if len(v.Posters) > 50 || len(v.Backdrops) > 50 {
		return errors.New("maximum 50 artwork variants")
	}
	for _, list := range [][]Variant{v.Posters, v.Backdrops} {
		for _, a := range list {
			if !validVariantPath(a.Path) || len(a.Tags) > 20 {
				return errors.New("invalid artwork variant")
			}
		}
	}
	for _, p := range []string{v.PinnedPoster, v.PinnedBackdrop} {
		if p != "" && !validVariantPath(p) {
			return errors.New("invalid pinned artwork")
		}
	}
	raw, e := json.Marshal(v)
	if e != nil {
		return e
	}
	_, e = s.db.ExecContext(ctx, `INSERT INTO overlays_artwork(item_key,value) VALUES(?,?) ON CONFLICT(item_key) DO UPDATE SET value=excluded.value`, key, string(raw))
	return e
}
func choose(variants []Variant, pinned, fallback, lang string, taste []string) string {
	if pinned != "" {
		return pinned
	}
	best := fallback
	score := -1
	for _, v := range variants {
		n := 0
		if v.Language == strings.Split(lang, "-")[0] {
			n += 100
		} else if v.Language != "" {
			continue
		}
		for _, tag := range v.Tags {
			for _, want := range taste {
				if strings.EqualFold(tag, want) {
					n += 10
				}
			}
		}
		if n > score {
			best = v.Path
			score = n
		}
	}
	return best
}

// Paths returns owner-approved variants, selected by language and actual tags.
func (s *Service) Paths(ctx context.Context, u *core.User, t *core.Title, lang string) (string, string) {
	poster, backdrop := t.PosterPath, t.BackdropPath
	cfg, e := s.Settings(ctx)
	if e != nil || !cfg.Enabled {
		return poster, backdrop
	}
	v, e := s.Artwork(ctx, t.Key)
	if e != nil {
		return poster, backdrop
	}
	var taste []string
	if cfg.Personalize && s.o.Taste != nil {
		taste = s.o.Taste(ctx, u)
	}
	if !cfg.Localize {
		lang = ""
	}
	return choose(v.Posters, v.PinnedPoster, poster, lang, taste), choose(v.Backdrops, v.PinnedBackdrop, backdrop, "", taste)
}
func (s *Service) Labels(ctx context.Context, u *core.User, t *core.Title) []string {
	report, err := s.Inspect(ctx, u, t)
	if err != nil {
		return nil
	}
	return report.Labels
}
func (s *Service) Version(ctx context.Context, u *core.User, t *core.Title, lang string) string {
	cfg, _ := s.Settings(ctx)
	if t.Type == core.TypeEpisode && !cfg.BadgesOnEpisodes() {
		hash := sha256.Sum256([]byte(t.StillPath))
		return hex.EncodeToString(hash[:8])
	}
	ctx = context.WithValue(ctx, previewSettingsKey{}, cfg)
	p, b := s.Paths(ctx, u, t, lang)
	if t.Type == core.TypeEpisode {
		p = t.StillPath
	}
	labels := visibleLabels(s.presentation(ctx, u, t, cfg).Labels)
	// Viewer identity and unrelated catalogue updates do not change pixels.
	parts := []any{s.posterVersion(), p, b, t.LogoPath, labels, pixelStyle(cfg, len(labels) > 0), "compositor-v5"}
	if gen := s.PosterGeneration(); gen > 0 {
		parts = append(parts, gen)
	}
	raw, _ := json.Marshal(parts)
	hash := sha256.Sum256(raw)
	return hex.EncodeToString(hash[:8])
}

func (s *Service) metadataImage(ctx context.Context, path string, width int) ([]byte, string, error) {
	if strings.HasPrefix(path, "https://") || strings.HasPrefix(path, "http://") {
		if images, ok := s.o.Images.(interface {
			GetUntrustedURL(context.Context, string) ([]byte, string, error)
		}); ok {
			return images.GetUntrustedURL(ctx, path)
		}
		return nil, "", core.ErrNotAvailable
	}
	return s.o.Images.Get(ctx, path, width)
}

// Poster decodes bounded images and uses a bounded generated-image cache.
func (s *Service) Poster(ctx context.Context, u *core.User, t *core.Title, lang string) ([]byte, string, error) {
	if s.o.Images == nil {
		return nil, "", errors.New("artwork unavailable")
	}
	cfg, err := s.Settings(ctx)
	if err != nil {
		return nil, "", err
	}
	if t.Type == core.TypeEpisode && !cfg.BadgesOnEpisodes() {
		return s.metadataImage(ctx, t.StillPath, 500)
	}
	ctx = context.WithValue(ctx, previewSettingsKey{}, cfg)
	path, _ := s.Paths(ctx, u, t, lang)
	if t.Type == core.TypeEpisode {
		path = t.StillPath
	}
	var data []byte
	var ct string
	if p, ok := s.o.Images.(posterSource); ok {
		data, ct, err = p.GetPoster(ctx, t, path, 500)
	} else {
		data, ct, err = s.metadataImage(ctx, path, 500)
	}
	if err != nil {
		return nil, "", err
	}
	labels := s.presentation(ctx, u, t, cfg).Labels
	if len(labels) == 0 {
		return data, ct, nil
	}
	return s.compositeContext(ctx, data, labels, cfg)
}

func (s *Service) compositeContext(ctx context.Context, data []byte, labels []string, settings Settings) ([]byte, string, error) {
	settings = normalizeSettings(settings)
	if err := validateSettings(settings); err != nil {
		return nil, "", err
	}
	if err := ctx.Err(); err != nil {
		return nil, "", err
	}
	labels = visibleLabels(labels)
	if kitimages.NoStore(ctx) {
		result, err := s.renderPoster(ctx, data, labels, settings)
		return result, "image/jpeg", err
	}
	// Hash incrementally; never duplicate the source image on a warm request.
	hash := sha256.New()
	_, _ = hash.Write(data)
	_ = json.NewEncoder(hash).Encode([]any{labels, pixelStyle(settings, len(labels) > 0), "compositor-v5"})
	key := hex.EncodeToString(hash.Sum(nil))
	result, err := s.generated.get(ctx, key, func(workCtx context.Context) ([]byte, error) {
		return s.renderPoster(workCtx, data, labels, settings)
	})
	return result, "image/jpeg", err
}

// Only visual inputs belong in a generated-image key. Localization, taste and
// evidence toggles have already resolved to source bytes and visible labels.
func pixelStyle(v Settings, hasLabels bool) [5]string {
	if !hasLabels {
		return [5]string{}
	}
	return [5]string{v.Style, v.Position, v.Accent, v.Size, fmt.Sprint(v.HideDot)}
}
func visibleLabels(labels []string) []string {
	out := make([]string, min(len(labels), 6))
	for i := range out {
		out[i] = strings.Map(func(r rune) rune {
			if r < 32 {
				return -1
			}
			return r
		}, labels[i])
	}
	return out
}

func (s *Service) renderPoster(ctx context.Context, data []byte, labels []string, settings Settings) ([]byte, error) {
	cfg, _, e := image.DecodeConfig(bytes.NewReader(data))
	if e != nil || cfg.Width <= 0 || cfg.Height <= 0 || int64(cfg.Width)*int64(cfg.Height) > 20_000_000 {
		return nil, errors.New("poster dimensions invalid")
	}
	if e = ctx.Err(); e != nil {
		return nil, e
	}
	src, _, e := image.Decode(bytes.NewReader(data))
	if e != nil {
		return nil, e
	}
	if e = ctx.Err(); e != nil {
		return nil, e
	}
	w := 500
	h := max(1, min(src.Bounds().Dy()*w/src.Bounds().Dx(), 1500))
	dst := image.NewRGBA(image.Rect(0, 0, w, h))
	if src.Bounds().Dx() == w && src.Bounds().Dy() == h {
		draw.Draw(dst, dst.Bounds(), src, src.Bounds().Min, draw.Src)
	} else {
		xdraw.CatmullRom.Scale(dst, dst.Bounds(), src, src.Bounds(), draw.Src, nil)
	}
	if e = ctx.Err(); e != nil {
		return nil, e
	}
	if e = s.paintBadges(dst, labels, settings); e != nil {
		return nil, e
	}
	if e = ctx.Err(); e != nil {
		return nil, e
	}
	var out bytes.Buffer
	if e = jpeg.Encode(&out, dst, &jpeg.Options{Quality: 88}); e != nil {
		return nil, e
	}
	return out.Bytes(), nil
}

func (s *Service) Backdrop(ctx context.Context, u *core.User, t *core.Title, lang string) ([]byte, string, error) {
	if s.o.Images == nil {
		return nil, "", errors.New("artwork unavailable")
	}
	_, path := s.Paths(ctx, u, t, lang)
	return s.metadataImage(ctx, path, 1280)
}

func (s *Service) Logo(ctx context.Context, t *core.Title) ([]byte, string, error) {
	if s.o.Images == nil {
		return nil, "", errors.New("artwork unavailable")
	}
	return s.metadataImage(ctx, t.LogoPath, 500)
}

func (s *Service) visible(u *core.User, t *core.Title) bool {
	if t == nil || t.Adult {
		return false
	}
	if u != nil {
		if contentrating.Exceeds(t.ContentRating, u.MaxContentRating) {
			return false
		}
		if len(u.AllowedLibraries) > 0 {
			section := core.LibraryMovies
			if t.Type != core.TypeMovie {
				section = core.LibraryShows
			}
			found := false
			for _, id := range u.AllowedLibraries {
				found = found || id == section
			}
			if !found {
				return false
			}
		}
	}
	return s.o.Allowed == nil || s.o.Allowed(u, t)
}
