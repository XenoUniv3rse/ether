package personalize

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/reqmemo"
)

// The high end of the SmartKey payload namespace is reserved for virtual
// discovery collections; ordinary SQLite smart-collection IDs start at one.
const reviewCollectionBase int64 = 900_000_000_000_000

func reviewCollectionKey(year int, typ core.MediaType) core.RatingKey {
	kind := int64(1)
	if typ == core.TypeShow {
		kind = 2
	}
	return core.SmartKey(reviewCollectionBase + int64(year)*10 + kind)
}
func reviewCollectionParts(key core.RatingKey) (int, core.MediaType, bool) {
	if key.Kind() != core.KindSmart {
		return 0, "", false
	}
	id := int64(key-core.SmartKey(0)) - reviewCollectionBase
	year := int(id / 10)
	if year < 1900 || year > 9999 {
		return 0, "", false
	}
	switch id % 10 {
	case 1:
		return year, core.TypeMovie, true
	case 2:
		return year, core.TypeShow, true
	}
	return 0, "", false
}

type reviewCollections struct {
	d    *Discovery
	base core.Collections

	// cards keeps each viewer's listing cards briefly and shares a build in
	// flight: one Jellyfin Views or library listing asks once per library,
	// concurrently and outside any request memo.
	mu    sync.Mutex
	cards map[string]*reviewCardEntry
}

type reviewCardEntry struct {
	done chan struct{}
	col  *core.Collection
	ok   bool
	err  error
	at   time.Time
}

// reviewCardTTL bounds how stale a listing's Year in Review card can be. The
// card is last year's review for eleven months of the year.
const reviewCardTTL = 30 * time.Second

// ReviewCollections retains the collection engine as the authority for owner
// collections and adds virtual collections assembled from the viewer's history.
func (d *Discovery) ReviewCollections(base core.Collections) core.Collections {
	return &reviewCollections{d: d, base: base}
}

// review builds a viewer's whole Year in Review collection of typ, for
// opening it (Get, Items, member checks). Listings use reviewCard instead.
func (c *reviewCollections) review(ctx context.Context, u *core.User, year int, typ core.MediaType) (*core.Collection, []core.Title, error) {
	if u == nil {
		return nil, nil, core.ErrUnauthorized
	}
	// Opening a collection reads it and probes its items; share one build
	// per viewer, year and type for the rest of the read request.
	hub, e := reqmemo.Do(ctx, fmt.Sprintf("personalize:review:%p:%d:%s:%#v", c.d, year, typ, u), func() (core.Hub, error) {
		return c.d.yearInReview(ctx, u, year, 10000, false, typ, 0)
	})
	if e != nil {
		return nil, nil, e
	}
	items := hub.Items
	if items == nil {
		items = []core.Title{}
	}
	return reviewCollection(u, year, typ, hub.Title, items, len(items)), items, nil
}

// reviewCardKeys is how many of the viewer's latest completed keys a
// listing's card reads looking for its art. A year whose latest keys are all
// unavailable is not listed; opening the collection still reads all of it.
const reviewCardKeys = 16

// reviewCard is a Year in Review collection as a listing shows it: whether
// it exists, its art from the latest title, and its size from one counting
// statement, without hydrating the rest of the viewer's year.
func (c *reviewCollections) reviewCard(ctx context.Context, u *core.User, year int, typ core.MediaType) (*core.Collection, bool) {
	if u == nil {
		return nil, false
	}
	key := fmt.Sprintf("%d:%s:%#v", year, typ, u)
	c.mu.Lock()
	if e, ok := c.cards[key]; ok && (e.at.IsZero() || time.Since(e.at) < reviewCardTTL) {
		c.mu.Unlock()
		select {
		case <-e.done:
		case <-ctx.Done():
			return nil, false
		}
		if e.err != nil {
			return nil, false
		}
		return e.col, e.ok
	}
	if c.cards == nil || len(c.cards) >= 4096 {
		c.cards = map[string]*reviewCardEntry{}
	}
	e := &reviewCardEntry{done: make(chan struct{})}
	c.cards[key] = e
	c.mu.Unlock()

	e.col, e.ok, e.err = c.buildReviewCard(ctx, u, year, typ)
	c.mu.Lock()
	if e.err != nil {
		delete(c.cards, key)
	} else {
		e.at = time.Now()
	}
	c.mu.Unlock()
	close(e.done)
	if e.err != nil {
		return nil, false
	}
	return e.col, e.ok
}

func (c *reviewCollections) buildReviewCard(ctx context.Context, u *core.User, year int, typ core.MediaType) (*core.Collection, bool, error) {
	hub, e := c.d.yearInReview(ctx, u, year, 1, false, typ, reviewCardKeys)
	if e != nil || len(hub.Items) == 0 {
		return nil, false, e
	}
	count := 1
	if counter, ok := c.d.state.(core.CompletedHistoryStore); ok {
		start := time.Date(year, 1, 1, 0, 0, 0, 0, c.d.now().Location())
		movies, shows, e := counter.CompletedCounts(ctx, u.ID, start, start.AddDate(1, 0, 0))
		if e != nil {
			return nil, false, e
		}
		if typ == core.TypeMovie {
			count = max(count, movies)
		} else {
			count = max(count, shows)
		}
	}
	return reviewCollection(u, year, typ, hub.Title, hub.Items, count), true, nil
}

func reviewCollection(u *core.User, year int, typ core.MediaType, title string, items []core.Title, count int) *core.Collection {
	col := &core.Collection{Key: reviewCollectionKey(year, typ), Title: title, Summary: "Titles you completed this year, based on your private watch history.", Kind: "user", Type: typ, OwnerUserID: u.ID, ChildCount: count}
	if len(items) > 0 {
		col.PosterPath = items[0].PosterPath
		col.BackdropPath = items[0].BackdropPath
		col.UpdatedAt = time.Date(year+1, 1, 1, 0, 0, 0, 0, time.UTC)
	}
	return col
}
func (c *reviewCollections) List(ctx context.Context, u *core.User, typ core.MediaType) ([]core.Collection, error) {
	var out []core.Collection
	if c.base != nil {
		var e error
		out, e = c.base.List(ctx, u, typ)
		if e != nil {
			return nil, e
		}
	}
	return c.withReviews(ctx, u, typ, out), nil
}

// ListOwned forwards the engine's listing without derived franchises (Plex
// collection search), plus the viewer's Year in Review collections.
func (c *reviewCollections) ListOwned(ctx context.Context, u *core.User, typ core.MediaType) ([]core.Collection, error) {
	var out []core.Collection
	if c.base != nil {
		var e error
		if owned, ok := c.base.(core.OwnedCollectionLister); ok {
			out, e = owned.ListOwned(ctx, u, typ)
		} else {
			out, e = c.base.List(ctx, u, typ)
		}
		if e != nil {
			return nil, e
		}
	}
	return c.withReviews(ctx, u, typ, out), nil
}

// ListOwnedMatching sends the title predicate to the base listing before
// resolving member data, then includes matching viewer review collections.
func (c *reviewCollections) ListOwnedMatching(ctx context.Context, u *core.User, typ core.MediaType, foldedName string, match func(string) bool) ([]core.Collection, error) {
	base, ok := c.base.(interface {
		ListOwnedMatching(context.Context, *core.User, core.MediaType, string, func(string) bool) ([]core.Collection, error)
	})
	if !ok {
		return c.ListOwned(ctx, u, typ)
	}
	out, err := base.ListOwnedMatching(ctx, u, typ, foldedName, match)
	if err != nil {
		return nil, err
	}
	if match(reviewTitle(c.latestReviewYear())) {
		out = c.withReviews(ctx, u, typ, out)
	}
	return out, nil
}

// ListComplete forwards the engine's complete listing (errors included, never
// a partial result) and adds the viewer's Year in Review collections, so
// consumers that advertise exact totals (Jellyfin BoxSets) see every
// collection. Without it the wrapper hid ListComplete and those consumers
// answered with an empty list (#454).
func (c *reviewCollections) ListComplete(ctx context.Context, u *core.User, typ core.MediaType) ([]core.Collection, error) {
	var out []core.Collection
	if c.base != nil {
		complete, ok := c.base.(interface {
			ListComplete(context.Context, *core.User, core.MediaType) ([]core.Collection, error)
		})
		if !ok {
			return nil, core.ErrNotAvailable
		}
		var e error
		out, e = complete.ListComplete(ctx, u, typ)
		if e != nil {
			return nil, e
		}
	}
	return c.withReviews(ctx, u, typ, out), nil
}

// IndexedOwnedCollections forwards indexed owned paging and appends the
// viewer's review collections in the same order as VisitOwned.
func (c *reviewCollections) IndexedOwnedCollections(ctx context.Context, u *core.User, typ core.MediaType, filter *core.Filter, offset, limit int,
	evaluate func([]core.Collection) ([]core.Collection, error),
) ([]core.Collection, int, bool, error) {
	pager, ok := c.base.(core.IndexedOwnedCollectionPager)
	if !ok {
		return nil, 0, false, nil
	}
	page, total, handled, err := pager.IndexedOwnedCollections(ctx, u, typ, filter, offset, limit, evaluate)
	if err != nil || !handled {
		return page, total, handled, err
	}
	reviews := c.withReviews(ctx, u, typ, nil)
	if evaluate != nil {
		reviews, err = evaluate(reviews)
		if err != nil {
			return nil, 0, true, err
		}
	}
	start, left := max(0, offset-total), limit-len(page)
	if left > 0 && start < len(reviews) {
		page = append(page, reviews[start:min(start+left, len(reviews))]...)
	}
	return page, total + len(reviews), true, nil
}

// ListOwnedComplete forwards the engine's non-franchise listing plus Year in
// Review collections and reports whether TMDB franchises belong to typ, so
// page-first collection endpoints never materialize every TMDB franchise.
func (c *reviewCollections) ListOwnedComplete(ctx context.Context, u *core.User, typ core.MediaType) ([]core.Collection, bool, error) {
	var out []core.Collection
	var includeFranchises bool
	if c.base != nil {
		owned, ok := c.base.(interface {
			ListOwnedComplete(context.Context, *core.User, core.MediaType) ([]core.Collection, bool, error)
		})
		if !ok {
			return nil, false, core.ErrNotAvailable
		}
		var err error
		out, includeFranchises, err = owned.ListOwnedComplete(ctx, u, typ)
		if err != nil {
			return nil, false, err
		}
	}
	return c.withReviews(ctx, u, typ, out), includeFranchises, nil
}

func (c *reviewCollections) IncludesFranchises(typ core.MediaType) bool {
	base, ok := c.base.(interface{ IncludesFranchises(core.MediaType) bool })
	return ok && base.IncludesFranchises(typ)
}

// VisitOwned forwards bounded owner batches, followed by the viewer's review.
func (c *reviewCollections) VisitOwned(ctx context.Context, u *core.User, typ core.MediaType, query string, batchSize int, after core.RatingKey, visit func([]core.Collection) (bool, error)) (bool, error) {
	base, ok := c.base.(interface {
		VisitOwned(context.Context, *core.User, core.MediaType, string, int, core.RatingKey, func([]core.Collection) (bool, error)) (bool, error)
	})
	if !ok {
		return false, core.ErrNotAvailable
	}
	stopped := false
	franchises, err := base.VisitOwned(ctx, u, typ, query, batchSize, after, func(cols []core.Collection) (bool, error) {
		stop, err := visit(cols)
		stopped = stop
		return stop, err
	})
	if err != nil || stopped {
		return false, err
	}
	_, err = visit(c.withReviews(ctx, u, typ, nil))
	return franchises, err
}

// ListPage forwards page-first collection paging (Plex /collections,
// /collection, /all?type=18) and appends the viewer's Year in Review
// collections after the base listing without materializing every franchise.
func (c *reviewCollections) ListPage(ctx context.Context, u *core.User, typ core.MediaType, offset, limit int) ([]core.Collection, int, error) {
	if offset < 0 {
		offset = 0
	}
	if limit < 0 {
		limit = 500
	}
	reviews := c.withReviews(ctx, u, typ, nil)
	if c.base == nil {
		if limit == 0 || offset >= len(reviews) {
			return nil, len(reviews), nil
		}
		end := min(offset+limit, len(reviews))
		return reviews[offset:end], len(reviews), nil
	}
	if pager, ok := c.base.(interface {
		ListPage(context.Context, *core.User, core.MediaType, int, int) ([]core.Collection, int, error)
	}); ok {
		page, baseTotal, err := pager.ListPage(ctx, u, typ, offset, limit)
		if err != nil {
			return nil, 0, err
		}
		total := baseTotal + len(reviews)
		if limit > 0 && len(page) < limit && len(reviews) > 0 {
			start := max(0, offset-baseTotal)
			if start < len(reviews) {
				rem := limit - len(page)
				end := min(start+rem, len(reviews))
				page = append(page, reviews[start:end]...)
			}
		}
		return page, total, nil
	}
	out, err := c.base.List(ctx, u, typ)
	if err != nil {
		return nil, 0, err
	}
	out = append(out, reviews...)
	total := len(out)
	if limit == 0 || offset >= total {
		return nil, total, nil
	}
	end := min(offset+limit, total)
	return out[offset:end], total, nil
}

// FindVisible forwards existence probes to the underlying engine before
// checking Year in Review collections, avoiding full franchise materialization.
func (c *reviewCollections) FindVisible(ctx context.Context, u *core.User, typ core.MediaType, accept func(core.Collection) (bool, error)) (bool, error) {
	if c.base != nil {
		if finder, ok := c.base.(interface {
			FindVisible(context.Context, *core.User, core.MediaType, func(core.Collection) (bool, error)) (bool, error)
		}); ok {
			found, err := finder.FindVisible(ctx, u, typ, accept)
			if err != nil || found {
				return found, err
			}
		}
	}
	for _, rev := range c.withReviews(ctx, u, typ, nil) {
		if accept == nil {
			return true, nil
		}
		yes, err := accept(rev)
		if err != nil || yes {
			return yes, err
		}
	}
	return false, nil
}

func (c *reviewCollections) latestReviewYear() int {
	now := c.d.now()
	if now.Month() < time.December {
		return now.Year() - 1
	}
	return now.Year()
}

func (c *reviewCollections) withReviews(ctx context.Context, u *core.User, typ core.MediaType, out []core.Collection) []core.Collection {
	year := c.latestReviewYear()
	types := []core.MediaType{typ}
	if typ == "" {
		types = []core.MediaType{core.TypeMovie, core.TypeShow}
	}
	for _, t := range types {
		if t != core.TypeMovie && t != core.TypeShow {
			continue
		}
		if col, ok := c.reviewCard(ctx, u, year, t); ok {
			out = append(out, *col)
		}
	}
	return out
}
func (c *reviewCollections) Get(ctx context.Context, u *core.User, key core.RatingKey) (*core.Collection, error) {
	if year, typ, ok := reviewCollectionParts(key); ok {
		// The collection itself is its card; only its items need the year.
		if u == nil {
			return nil, core.ErrUnauthorized
		}
		if card, ok := c.reviewCard(ctx, u, year, typ); ok {
			col := *card
			return &col, nil
		}
		return reviewCollection(u, year, typ, fmt.Sprintf("Your %d in Review", year), nil, 0), nil
	}
	if c.base == nil {
		return nil, core.ErrNotFound
	}
	return c.base.Get(ctx, u, key)
}

// ProbeVisible answers a listing's visibility probe for a Year in Review
// collection from its card: the card already found a title the viewer can
// see, and counted the rest, so the listing does not build the whole year.
func (c *reviewCollections) ProbeVisible(ctx context.Context, u *core.User, col core.Collection) (bool, int, bool, error) {
	year, typ, ok := reviewCollectionParts(col.Key)
	if !ok {
		if p, ok := c.base.(interface {
			ProbeVisible(context.Context, *core.User, core.Collection) (bool, int, bool, error)
		}); ok {
			return p.ProbeVisible(ctx, u, col)
		}
		return false, 0, false, nil
	}
	card, ok := c.reviewCard(ctx, u, year, typ)
	if !ok {
		return false, 0, true, nil
	}
	return true, card.ChildCount, true, nil
}

// AnswersVisible reports whether ProbeVisible answers for key itself.
func (c *reviewCollections) AnswersVisible(key core.RatingKey) bool {
	_, _, ok := reviewCollectionParts(key)
	return ok
}
func (c *reviewCollections) Items(ctx context.Context, u *core.User, key core.RatingKey, offset, limit int) (core.Page, error) {
	if year, typ, ok := reviewCollectionParts(key); ok {
		_, items, e := c.review(ctx, u, year, typ)
		if e != nil {
			return core.Page{}, e
		}
		total := len(items)
		if offset < 0 {
			offset = 0
		}
		if offset > total {
			offset = total
		}
		if limit <= 0 {
			limit = total
		}
		end := offset + limit
		if end < offset || end > total {
			end = total
		}
		return core.Page{Items: items[offset:end], Total: total}, nil
	}
	if c.base == nil {
		return core.Page{}, core.ErrNotFound
	}
	return c.base.Items(ctx, u, key, offset, limit)
}
func (c *reviewCollections) CheckMembers(ctx context.Context, u *core.User, key core.RatingKey, want []core.RatingKey, scanLimit int) (*core.Collection, bool, error) {
	if len(want) == 0 || scanLimit <= 0 {
		return nil, false, nil
	}
	if year, typ, ok := reviewCollectionParts(key); ok {
		col, items, e := c.review(ctx, u, year, typ)
		if e != nil {
			return nil, false, e
		}
		wantSet := make(map[core.RatingKey]bool, len(want))
		for _, k := range want {
			wantSet[k] = true
		}
		for _, it := range items[:min(len(items), scanLimit)] {
			delete(wantSet, it.Key)
		}
		return col, len(wantSet) == 0, nil
	}
	if c.base == nil {
		return nil, false, core.ErrNotFound
	}
	if checker, ok := c.base.(interface {
		CheckMembers(context.Context, *core.User, core.RatingKey, []core.RatingKey, int) (*core.Collection, bool, error)
	}); ok {
		return checker.CheckMembers(ctx, u, key, want, scanLimit)
	}
	col, err := c.base.Get(ctx, u, key)
	if err != nil {
		return nil, false, err
	}
	wantSet := make(map[core.RatingKey]bool, len(want))
	for _, k := range want {
		wantSet[k] = true
	}
	for offset := 0; offset < scanLimit; {
		page, err := c.base.Items(ctx, u, key, offset, 100)
		if err != nil {
			return col, false, err
		}
		for _, it := range page.Items {
			delete(wantSet, it.Key)
		}
		if len(wantSet) == 0 {
			return col, true, nil
		}
		if len(page.Items) == 0 {
			return col, false, nil
		}
		offset += len(page.Items)
		if offset >= page.Total {
			return col, false, nil
		}
	}
	return col, false, nil
}
func (c *reviewCollections) CheckMembersBatch(ctx context.Context, u *core.User, keys []core.RatingKey, want map[core.RatingKey][]core.RatingKey, scanLimit int) (map[core.RatingKey]*core.Collection, map[core.RatingKey]bool, error) {
	cols := make(map[core.RatingKey]*core.Collection, len(keys))
	matched := make(map[core.RatingKey]bool, len(keys))
	var baseKeys []core.RatingKey
	for _, key := range keys {
		if year, typ, ok := reviewCollectionParts(key); ok {
			w := want[key]
			if len(w) == 0 || scanLimit <= 0 {
				if col, ok := c.reviewCard(ctx, u, year, typ); ok {
					cols[key] = col
				}
				continue
			}
			col, items, e := c.review(ctx, u, year, typ)
			if e != nil || col == nil {
				continue
			}
			cols[key] = col
			wantSet := make(map[core.RatingKey]bool, len(w))
			for _, k := range w {
				wantSet[k] = true
			}
			for _, it := range items[:min(len(items), scanLimit)] {
				delete(wantSet, it.Key)
			}
			matched[key] = len(wantSet) == 0
			continue
		}
		baseKeys = append(baseKeys, key)
	}
	if len(baseKeys) == 0 || c.base == nil {
		return cols, matched, nil
	}
	if batch, ok := c.base.(interface {
		CheckMembersBatch(context.Context, *core.User, []core.RatingKey, map[core.RatingKey][]core.RatingKey, int) (map[core.RatingKey]*core.Collection, map[core.RatingKey]bool, error)
	}); ok {
		bcols, bmatched, err := batch.CheckMembersBatch(ctx, u, baseKeys, want, scanLimit)
		if err != nil {
			return nil, nil, err
		}
		for k, v := range bcols {
			cols[k] = v
		}
		for k, v := range bmatched {
			matched[k] = v
		}
		return cols, matched, nil
	}
	for _, key := range baseKeys {
		w := want[key]
		if len(w) > 0 && scanLimit > 0 {
			col, ok, err := c.CheckMembers(ctx, u, key, w, scanLimit)
			if err == nil && col != nil {
				cols[key] = col
				matched[key] = ok
			}
		} else if col, err := c.base.Get(ctx, u, key); err == nil && col != nil {
			cols[key] = col
		}
	}
	return cols, matched, nil
}
func (c *reviewCollections) CreateSmart(ctx context.Context, col core.Collection) (*core.Collection, error) {
	if c.base == nil {
		return nil, core.ErrNotFound
	}
	return c.base.CreateSmart(ctx, col)
}
func (c *reviewCollections) DeleteSmart(ctx context.Context, key core.RatingKey) error {
	if _, _, ok := reviewCollectionParts(key); ok {
		return core.ErrNotFound
	}
	if c.base == nil {
		return core.ErrNotFound
	}
	return c.base.DeleteSmart(ctx, key)
}
func (c *reviewCollections) CollectionArtwork(ctx context.Context, u *core.User, key core.RatingKey, element string, width int) ([]byte, string, bool, error) {
	if _, _, ok := reviewCollectionParts(key); ok {
		return nil, "", false, nil
	}
	if source, ok := c.base.(core.CollectionArtworkSource); ok {
		return source.CollectionArtwork(ctx, u, key, element, width)
	}
	return nil, "", false, nil
}

// CollectionPosterCandidates shares the persisted collection artwork lookup
// with the base service. Reviews keep their own artwork and membership rules.
func (c *reviewCollections) CollectionPosterCandidates(ctx context.Context, u *core.User, keys []core.RatingKey) (map[core.RatingKey][]core.Title, error) {
	batch, ok := c.base.(interface {
		CollectionPosterCandidates(context.Context, *core.User, []core.RatingKey) (map[core.RatingKey][]core.Title, error)
	})
	if !ok {
		return nil, nil
	}
	var baseKeys []core.RatingKey
	for _, key := range keys {
		if _, _, review := reviewCollectionParts(key); !review {
			baseKeys = append(baseKeys, key)
		}
	}
	return batch.CollectionPosterCandidates(ctx, u, baseKeys)
}

// CollectionCardHeaders keeps the indexed Home card lookup on the persisted
// collection source. Private review collections are never Home cards.
func (c *reviewCollections) CollectionCardHeaders(ctx context.Context, u *core.User, keys []core.RatingKey) (map[core.RatingKey]*core.Collection, error) {
	source, ok := c.base.(core.CollectionCardHeaders)
	if !ok {
		return nil, core.ErrNotAvailable
	}
	return source.CollectionCardHeaders(ctx, u, keys)
}

func (c *reviewCollections) CollectionCardPage(ctx context.Context, u *core.User, key core.RatingKey, after int64, offset, limit int) ([]core.CollectionCardMember, string, error) {
	source, ok := c.base.(core.CollectionCardMembers)
	if !ok {
		return nil, "", core.ErrNotAvailable
	}
	return source.CollectionCardPage(ctx, u, key, after, offset, limit)
}

// VisitCollectionMemberPages forwards catalogue membership without opening
// review collections, which answer their own visibility from viewer history.
func (c *reviewCollections) VisitCollectionMemberPages(ctx context.Context, u *core.User, keys []core.RatingKey, size int, visit func(map[core.RatingKey]core.Page) ([]core.RatingKey, error)) error {
	batch, ok := c.base.(interface {
		VisitCollectionMemberPages(context.Context, *core.User, []core.RatingKey, int, func(map[core.RatingKey]core.Page) ([]core.RatingKey, error)) error
	})
	if !ok {
		return nil
	}
	var baseKeys []core.RatingKey
	for _, key := range keys {
		if _, _, review := reviewCollectionParts(key); !review {
			baseKeys = append(baseKeys, key)
		}
	}
	return batch.VisitCollectionMemberPages(ctx, u, baseKeys, size, visit)
}
