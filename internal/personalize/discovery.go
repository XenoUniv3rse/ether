package personalize

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/braedonsaunders/ether/internal/contentrating"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/reqmemo"
	"github.com/braedonsaunders/ether/internal/store"
)

func init() {
	store.Register("personalize_discovery", 1, `CREATE TABLE personalize_discovery_hidden(user_id INTEGER NOT NULL,item_key INTEGER NOT NULL,hidden_at INTEGER NOT NULL,PRIMARY KEY(user_id,item_key));`)
}

// Discovery adds viewer-owned utility rows without changing Designer layout.
type Discovery struct {
	db      *sql.DB
	catalog core.Catalog
	state   core.State
	allowed func(*core.User, *core.Title) bool
	now     func() time.Time
	// NotInterested applies only to recommendation rows, never Continue.
	NotInterested func(context.Context, *core.User) ([]NotInterestedSignal, error)
	// AllowedFacts is what allowed reads of a title beyond its row for a
	// viewer (safety.Service.TitleFacts). A light read loads only those;
	// without it, every visibility fact.
	AllowedFacts func(*core.User) core.TitleFacts
}

func NewDiscovery(db *sql.DB, cat core.Catalog, st core.State, allowed func(*core.User, *core.Title) bool) *Discovery {
	return &Discovery{db: db, catalog: cat, state: st, allowed: allowed, now: time.Now}
}
func discoveryIdentity(t *core.Title) core.RatingKey {
	if t.ShowKey != 0 {
		return t.ShowKey
	}
	if t.Type == core.TypeSeason || t.Type == core.TypeEpisode {
		return core.ShowKey(t.Key.TMDB())
	}
	return t.Key
}
func (d *Discovery) visible(u *core.User, t *core.Title) bool {
	if u == nil || t == nil || t.Adult || contentrating.Exceeds(t.ContentRating, u.MaxContentRating) {
		return false
	}
	section := core.LibraryMovies
	if t.Type != core.TypeMovie {
		section = core.LibraryShows
	}
	if len(u.AllowedLibraries) > 0 {
		ok := false
		for _, id := range u.AllowedLibraries {
			ok = ok || id == section
		}
		if !ok {
			return false
		}
	}
	return d.allowed == nil || d.allowed(u, t)
}

// RemoveContinue stores a sticky show-level dismissal, so the next episode or
// a repeated timeline cannot immediately put a dismissed show back on Home.
func (d *Discovery) RemoveContinue(ctx context.Context, uid int64, key core.RatingKey) error {
	t, e := d.catalog.Get(ctx, key)
	if e != nil {
		return e
	}
	_, e = d.db.ExecContext(ctx, `INSERT INTO personalize_discovery_hidden(user_id,item_key,hidden_at) VALUES(?,?,?) ON CONFLICT(user_id,item_key) DO UPDATE SET hidden_at=excluded.hidden_at`, uid, discoveryIdentity(t), d.now().Unix())
	return e
}
func (d *Discovery) RestoreContinue(ctx context.Context, uid int64, key core.RatingKey) error {
	t, e := d.catalog.Get(ctx, key)
	if e != nil {
		return e
	}
	_, e = d.db.ExecContext(ctx, `DELETE FROM personalize_discovery_hidden WHERE user_id=? AND item_key=?`, uid, discoveryIdentity(t))
	return e
}
func (d *Discovery) ContinueAllowed(ctx context.Context, uid int64, t *core.Title) bool {
	if t == nil {
		return false
	}
	var n int
	e := d.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM personalize_discovery_hidden WHERE user_id=? AND item_key=?`, uid, discoveryIdentity(t)).Scan(&n)
	return e == nil && n == 0
}

// FilterContinue deduplicates episodes by their parent show's TMDB identity.
func (d *Discovery) FilterContinue(ctx context.Context, u *core.User, titles []core.Title) []core.Title {
	out := []core.Title{}
	if u == nil {
		return out
	}
	// The viewer's hidden identities among the row's, in one read per 400
	// titles instead of one per title.
	hidden := map[core.RatingKey]bool{}
	ids := make([]any, 0, len(titles))
	for i := range titles {
		ids = append(ids, int64(discoveryIdentity(&titles[i])))
	}
	for start := 0; start < len(ids); start += 400 {
		part := ids[start:min(start+400, len(ids))]
		rows, e := d.db.QueryContext(ctx, `SELECT item_key FROM personalize_discovery_hidden WHERE user_id=? AND item_key IN (?`+strings.Repeat(",?", len(part)-1)+`)`, append([]any{u.ID}, part...)...)
		if e != nil {
			return []core.Title{}
		}
		for rows.Next() {
			var k int64
			if rows.Scan(&k) == nil {
				hidden[core.RatingKey(k)] = true
			}
		}
		e = rows.Err()
		rows.Close()
		if e != nil {
			return []core.Title{}
		}
	}
	seen := map[core.RatingKey]bool{}
	for _, t := range titles {
		k := discoveryIdentity(&t)
		if seen[k] || !d.visible(u, &t) || hidden[k] {
			continue
		}
		seen[k] = true
		out = append(out, t)
	}
	return out
}
func (d *Discovery) YearInReview(ctx context.Context, u *core.User, year, limit int) (core.Hub, error) {
	return d.yearInReview(ctx, u, year, limit, false, "", 0)
}

// reviewPage is how many completed keys a Year in Review build reads at a
// time; a build stops reading once it holds limit titles.
const reviewPage = 256

// reviewKinds are the key kinds that can become a title of want ("" is
// every kind); plugin keys only tell their type once hydrated.
func reviewKinds(want core.MediaType) []core.KeyKind {
	switch want {
	case core.TypeMovie:
		return []core.KeyKind{core.KindMovie, core.KindLocal}
	case core.TypeShow:
		return []core.KeyKind{core.KindShow, core.KindSeason, core.KindEpisode, core.KindLocal}
	}
	return nil
}

func reviewTitle(year int) string {
	return fmt.Sprintf("Your %d in Review", year)
}

// yearInReview applies exclusions only when this history is suggested as a
// discovery row; direct collection access continues to show viewing history.
// want limits it to films or shows ("" is both). maxKeys > 0 stops the build
// after that many completed keys even if it holds fewer than limit titles
// (a listing's card reads only the latest few).
func (d *Discovery) yearInReview(ctx context.Context, u *core.User, year, limit int, recommendation bool, want core.MediaType, maxKeys int) (core.Hub, error) {
	hub := core.Hub{ID: fmt.Sprintf("discovery.review.%d", year), Title: reviewTitle(year), Context: "hub.home.yearinreview", Promoted: true}
	if u == nil || d.state == nil {
		return hub, nil
	}
	if year < 1900 || year > d.now().Year() {
		return hub, fmt.Errorf("invalid review year")
	}
	if limit <= 0 {
		limit = 20
	}
	var excluded excludedTitles
	if recommendation {
		var err error
		excluded, _, err = loadExclusions(ctx, u, d.NotInterested)
		if err != nil {
			return hub, err
		}
	}
	// The year is the server's calendar year (d.now's zone), not UTC's: a
	// film finished on New Year's Eve evening belongs to that year.
	start := time.Date(year, 1, 1, 0, 0, 0, 0, d.now().Location())
	end := start.AddDate(1, 0, 0)
	// One entry per key completed inside the year, from an aggregate over
	// only this viewer's completed rows in it. A newest-first history page
	// (capped at 500 rows) dropped the start of the year, or all of it, for
	// active viewers once later and started-only rows filled the page (#799).
	completed, ok := d.state.(core.CompletedHistoryStore)
	if !ok {
		return hub, nil
	}
	seen := map[core.RatingKey]bool{}
	// Read the year a page at a time and hydrate a window at a time. The
	// build stops at limit, so the rest of a long year is neither read nor
	// loaded onto a Home or listing request.
	kinds := reviewKinds(want)
	for offset := 0; len(hub.Items) < limit && (maxKeys <= 0 || offset < maxKeys); offset += reviewPage {
		size := reviewPage
		if maxKeys > 0 {
			size = min(size, maxKeys-offset)
		}
		key := fmt.Sprintf("discovery:review:%d:%d:%v:%d:%d", u.ID, start.Unix(), kinds, offset, size)
		page, e := reqmemo.Do(ctx, key, func() ([]core.HistoryEntry, error) {
			return completed.CompletedBetween(ctx, u.ID, start, end, kinds, offset, size)
		})
		if e != nil {
			return hub, e
		}
		d.reviewWindows(ctx, u, page, limit, want, maxKeys > 0, excluded, seen, &hub)
		if len(page) < size {
			break
		}
	}
	return hub, nil
}

// reviewWindows hydrates entries a window at a time (64, or fewer for a
// small build such as a listing's card) into hub until it holds limit
// titles of want. A light build (a listing's card) reads only the stored
// rows a visibility decision needs, never credits or trailers, and skips a
// key the catalogue does not hold instead of fetching it.
func (d *Discovery) reviewWindows(ctx context.Context, u *core.User, entries []core.HistoryEntry, limit int, want core.MediaType, light bool, excluded excludedTitles, seen map[core.RatingKey]bool, hub *core.Hub) {
	size := min(64, max(8, limit))
	for startAt := 0; startAt < len(entries) && len(hub.Items) < limit; startAt += size {
		window := entries[startAt:min(startAt+size, len(entries))]
		keys := make([]core.RatingKey, 0, len(window)*2)
		for _, h := range window {
			if !seen[tasteKey(h.Key)] {
				keys = append(keys, h.Key, tasteKey(h.Key))
			}
		}
		var cached map[core.RatingKey]core.Title
		rows, lightRead := d.catalog.(visibilityRowSource)
		lightRead = light && lightRead
		if lightRead {
			var err error
			facts := core.VisibilityFacts
			if d.AllowedFacts != nil {
				facts = d.AllowedFacts(u)
			}
			if cached, err = rows.CachedVisibilityRows(ctx, keys, facts); err != nil {
				return
			}
		} else {
			cached = d.reviewTitles(ctx, u.ID, keys)
		}
		title := func(k core.RatingKey) (*core.Title, error) {
			if lightRead {
				if t, ok := cached[k]; ok {
					return &t, nil
				}
				return nil, core.ErrNotFound
			}
			return historyTitle(ctx, d.catalog, cached, k)
		}
		for _, h := range window {
			// Episode keys already identify their show. Once that show is in the
			// row, do not hydrate every remaining watched episode just to discard it.
			if seen[tasteKey(h.Key)] {
				continue
			}
			t, e := title(h.Key)
			if e != nil || !d.visible(u, t) || excluded.contains(t) {
				continue
			}
			k := discoveryIdentity(t)
			if seen[k] {
				continue
			}
			if k != t.Key {
				t, e = title(k)
				if e != nil || !d.visible(u, t) || excluded.contains(t) {
					continue
				}
			}
			if want != "" && t.Type != want {
				continue
			}
			seen[k] = true
			hub.Items = append(hub.Items, *t)
			if len(hub.Items) >= limit {
				break
			}
		}
	}
}

type reviewProjectionKey struct{}

// WithReviewProjection requests card and policy facts for a Home projection
// whose wire format does not display full credits or trailers.
func WithReviewProjection(ctx context.Context, projection core.ListRows) context.Context {
	return context.WithValue(ctx, reviewProjectionKey{}, projection)
}

func (d *Discovery) loadReviewTitles(ctx context.Context, keys []core.RatingKey) map[core.RatingKey]core.Title {
	if projection, ok := ctx.Value(reviewProjectionKey{}).(core.ListRows); ok {
		if cat, ok := d.catalog.(interface {
			CachedReviewTitles(context.Context, []core.RatingKey, core.ListRows) ([]core.Title, error)
		}); ok {
			titles, err := cat.CachedReviewTitles(ctx, keys, projection)
			if err == nil {
				out := make(map[core.RatingKey]core.Title, len(titles))
				for _, title := range titles {
					out[title.Key] = title
				}
				return out
			}
		}
	}
	return mirroredTitles(ctx, d.catalog, keys)
}

type reviewTitleMemo struct {
	mu     sync.Mutex
	titles map[core.RatingKey]core.Title
}

// reviewTitles keeps hydrated history candidates across successive widening
// passes. The windows can overlap even when their exact key lists differ.
func (d *Discovery) reviewTitles(ctx context.Context, viewerID int64, keys []core.RatingKey) map[core.RatingKey]core.Title {
	if !reqmemo.Active(ctx) {
		return d.loadReviewTitles(ctx, keys)
	}
	memo, _ := reqmemo.Do(ctx, fmt.Sprintf("discovery:review-titles:%p:%d", d, viewerID), func() (*reviewTitleMemo, error) {
		return &reviewTitleMemo{titles: make(map[core.RatingKey]core.Title)}, nil
	})
	memo.mu.Lock()
	defer memo.mu.Unlock()
	out := make(map[core.RatingKey]core.Title, len(keys))
	var missing []core.RatingKey
	for _, k := range keys {
		if title, ok := memo.titles[k]; ok {
			out[k] = title
		} else {
			missing = append(missing, k)
		}
	}
	for key, title := range d.loadReviewTitles(ctx, missing) {
		out[key] = title
		if len(memo.titles) < 4096 {
			memo.titles[key] = title
		}
	}
	return out
}

// visibilityRowSource is the catalogue's batched read of the stored rows a
// visibility decision needs (catalog.CachedVisibilityRows).
type visibilityRowSource interface {
	CachedVisibilityRows(context.Context, []core.RatingKey, core.TitleFacts) (map[core.RatingKey]core.Title, error)
}

func (d *Discovery) Rediscover(ctx context.Context, u *core.User, limit int) (core.Hub, error) {
	hub := core.Hub{ID: "discovery.rediscover", Title: "Rediscover", Context: "hub.home.rediscover", Promoted: true}
	if u == nil || d.state == nil {
		return hub, nil
	}
	if limit <= 0 {
		limit = 12
	}
	excluded, _, err := loadExclusions(ctx, u, d.NotInterested)
	if err != nil {
		return hub, err
	}
	entries, e := d.state.InProgress(ctx, u.ID, 200)
	if e != nil {
		return hub, e
	}
	seen := map[core.RatingKey]bool{}
	// Match review rows: hydrate eligible history in bounded windows, including
	// parent shows, so a cold mirror does not issue a detail read per title.
	for start := 0; start < len(entries) && len(hub.Items) < limit; start += 64 {
		window := entries[start:min(start+64, len(entries))]
		keys := make([]core.RatingKey, 0, len(window)*2)
		for _, st := range window {
			if !st.LastViewedAt.IsZero() && d.now().Sub(st.LastViewedAt) >= 30*24*time.Hour && st.ViewCount <= 0 && !seen[tasteKey(st.Key)] {
				keys = append(keys, st.Key, tasteKey(st.Key))
			}
		}
		cached := mirroredTitles(ctx, d.catalog, keys)
		for _, st := range window {
			if st.LastViewedAt.IsZero() || d.now().Sub(st.LastViewedAt) < 30*24*time.Hour || st.ViewCount > 0 {
				continue
			}
			if seen[tasteKey(st.Key)] {
				continue
			}
			t, e := historyTitle(ctx, d.catalog, cached, st.Key)
			if e != nil || !d.visible(u, t) || excluded.contains(t) {
				continue
			}
			k := discoveryIdentity(t)
			if seen[k] {
				continue
			}
			if k != t.Key {
				t, e = historyTitle(ctx, d.catalog, cached, k)
				if e != nil || !d.visible(u, t) || excluded.contains(t) {
					continue
				}
			}
			seen[k] = true
			hub.Items = append(hub.Items, *t)
			if len(hub.Items) >= limit {
				break
			}
		}
	}
	return hub, nil
}
func (d *Discovery) Hubs(ctx context.Context, u *core.User, limit int) ([]core.Hub, error) {
	var out []core.Hub
	rediscover, e := d.Rediscover(ctx, u, limit)
	if e != nil {
		return nil, e
	}
	if len(rediscover.Items) > 0 {
		out = append(out, rediscover)
	}
	year := d.now().Year()
	if d.now().Month() < time.December {
		year--
	}
	review, e := d.yearInReview(ctx, u, year, limit, true, "", 0)
	if e != nil {
		return out, e
	}
	if len(review.Items) > 0 {
		out = append(out, review)
	}
	return out, nil
}
