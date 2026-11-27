package experience

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/braedonsaunders/ether/internal/contentrating"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/reqmemo"
)

// PluginBrowsePage is one keyset-paged window of projected plugin items.
type PluginBrowsePage struct {
	Items      []core.Title
	Total      int
	TotalExact bool
	NextCursor string
}

// PluginItemView is a projected plugin item with its owning library, raw art
// references and playability flag.
type PluginItemView struct {
	Library      core.Library
	Title        core.Title
	Images       []frontend.Image
	Chapters     []core.Chapter
	Playable     bool
	SkipContinue bool
	Unavailable  *core.ItemUnavailable
}

// PluginLibraries supplies unified library metadata (builtin, filter, and
// plugin) and store-backed plugin item operations to Service without coupling
// package experience to plugins, pluginitems, or libraries.
type PluginLibraries interface {
	Libraries(ctx context.Context) ([]core.Library, error)
	Library(ctx context.Context, id core.LibraryID) (core.Library, error)
	IsPluginLibraryActive(ctx context.Context, lib core.Library) bool
	RootMediaType(ctx context.Context, lib core.Library) core.MediaType
	Count(ctx context.Context, lib core.Library) (int, error)
	Browse(ctx context.Context, lib core.Library, q frontend.ItemQuery, cursor string, limit int) (PluginBrowsePage, error)
	Item(ctx context.Context, key core.RatingKey) (PluginItemView, error)
	ItemsMany(ctx context.Context, keys []core.RatingKey) (map[core.RatingKey]PluginItemView, error)
	Children(ctx context.Context, parent core.RatingKey, recursive bool, cursor string, limit int) (PluginBrowsePage, error)
	Ancestors(ctx context.Context, key core.RatingKey) ([]core.Title, core.Library, error)
	Facets(ctx context.Context, lib core.Library, facet string) ([]core.FacetValue, error)
	Search(ctx context.Context, lib core.Library, query string, q frontend.ItemQuery, limit int) ([]core.Title, error)
	// MemberOf reports whether key is a shared member of a collection
	// library's collection. It is one indexed read, never a walk.
	MemberOf(ctx context.Context, lib core.Library, key core.RatingKey) (bool, error)
}

// PluginBatchBrowseLibraries is an optional capability for plugin library
// adapters that execute multiple SQL-ready browse queries for one library
// together (sharing kind/tally reads, shelf ref queries, and item hydration).
type PluginBatchBrowseLibraries interface {
	BrowseBatch(ctx context.Context, lib core.Library, queries []frontend.ItemQuery) ([]PluginBrowsePage, error)
}

// PluginShelf is one resolved manifest-declared Recommended shelf for a plugin library.
type PluginShelf struct {
	Key       string
	Label     string
	Level     string
	Sort      core.SortOrder
	Direction string // "desc" (default) or "asc"
	Filters   map[string][]string
	TabFilter *core.TabFilter
	// Purpose and Layout are the shelf's declared row purpose
	// (recentContexts, liked) and default look (tracks, tiles, …).
	Purpose frontend.RowPurpose
	Layout  string
}

// PluginPresentation carries manifest-declared Vela library presentation metadata.
type PluginPresentation struct {
	Media        core.MediaClass
	Card         string
	DateDisplay  string
	Experience   string // declared library experience (empty: the class default)
	Tabs         []frontend.ViewTab
	Sorts        []string
	Facets       []frontend.ViewFacet
	Shelves      []PluginShelf
	TabShelves   map[string][]PluginShelf // per-tab shelves keyed by recommended tab key
	ListLevels   []string
	RootLevel    string                     // root hierarchy level key (index 0)
	LevelMedia   map[string]core.MediaClass // effective media class per declared level
	LevelParents map[string]string          // effective parent level key per non-root level
	LevelRoles   map[string]string          // declared semantic role per level (album, artist, playlist, track…)
}

// roleLevel returns the first declared level with a role.
func (p PluginPresentation) roleLevel(role string) string {
	for lvl, r := range p.LevelRoles {
		if r == role {
			return lvl
		}
	}
	return ""
}

// PluginFilteredChildrenLibraries is an optional extension of PluginLibraries
// that pushes child level and role filtering directly into the store query so
// filtered /children paging stays O(page).
type PluginFilteredChildrenLibraries interface {
	ChildrenFiltered(ctx context.Context, parent core.RatingKey, recursive bool, level, role, cursor string, limit int) (PluginBrowsePage, error)
}

// PluginQueryChildrenLibraries is an optional extension of PluginLibraries
// that pushes child level, role, and type filtering (Types, UnservedTypes,
// ExcludedTypes) directly into the store query so typed child listings (such
// as MusicArtist -> MusicAlbum or Video with Recursive=true) stay O(page)
// without walking or mixing unrelated branches.
type PluginQueryChildrenLibraries interface {
	ChildrenQuery(ctx context.Context, parent core.RatingKey, q frontend.ItemQuery, cursor string, limit int) (PluginBrowsePage, error)
}

// PluginFacetPerson is one distinct person entry for a plugin library's people tab.
type PluginFacetPerson struct {
	PersonID    int64
	Name        string
	Count       int
	HasThumb    bool
	SampleTitle *core.Title
}

// PluginFacetPeoplePage is one page of people for a plugin library's people tab.
type PluginFacetPeoplePage struct {
	Items []PluginFacetPerson
	Total int
	More  bool
}

// PluginSliceExpander is implemented by plugin library adapters that can
// trigger non-blocking background facet/person expansions on viewer browse.
type PluginSliceExpander interface {
	MaybeExpand(ctx context.Context, lib core.Library, q frontend.ItemQuery) bool
}

// PluginPresentationLibraries is implemented by plugin library adapters that
// expose card/tabs/sorts/facets presentation metadata, facet collections, and
// facet people tabs.
type PluginPresentationLibraries interface {
	Presentation(ctx context.Context, lib core.Library) (PluginPresentation, bool)
	FacetBuckets(ctx context.Context, lib core.Library, field string) ([]frontend.FacetBucket, error)
	FacetCollections(ctx context.Context, lib core.Library, field, order, query string, offset, limit int) (frontend.FacetCollectionPage, error)
	FacetPeople(ctx context.Context, lib core.Library, field, order, query string, offset, limit int) (PluginFacetPeoplePage, error)
}

// PluginLevelFacetLibraries is an optional extension of PluginLibraries /
// PluginPresentationLibraries supporting hierarchy-level-scoped facets, facet
// collections, and people lists.
type PluginLevelFacetLibraries interface {
	FacetsAtLevel(ctx context.Context, lib core.Library, level, facet string) ([]core.FacetValue, error)
	FacetBucketsAtLevel(ctx context.Context, lib core.Library, level, field string) ([]frontend.FacetBucket, error)
	FacetCollectionsAtLevel(ctx context.Context, lib core.Library, level, field, order, query string, offset, limit int) (frontend.FacetCollectionPage, error)
	FacetPeopleAtLevel(ctx context.Context, lib core.Library, level, field, order, query string, offset, limit int) (PluginFacetPeoplePage, error)
}

type catalogTabFacetReader interface {
	LibraryFacetCollections(context.Context, core.CatalogFacetCollectionsQuery) (core.CatalogFacetCollectionsPage, error)
	LibraryFacetPeople(context.Context, core.CatalogFacetPeopleQuery) (core.CatalogFacetPeoplePage, error)
}

var (
	_ frontend.LibraryFacetBuckets        = (*Service)(nil)
	_ frontend.LibraryFacetBucketsAtLevel = (*Service)(nil)
	_ frontend.LevelFacetReader           = (*Service)(nil)
	_ frontend.LevelFacetPager            = (*Service)(nil)
	_ frontend.LibraryFacetCollections    = (*Service)(nil)
	_ frontend.LibraryFacetPeople         = (*Service)(nil)
	_ frontend.PluginLibraryRowsReader    = (*Service)(nil)
)

const maxPluginCursorEntries = 512

type pluginCursorCache struct {
	mu    sync.Mutex
	order []string
	items map[string]string
}

func newPluginCursorCache() *pluginCursorCache {
	return &pluginCursorCache{
		items: make(map[string]string),
	}
}

func (c *pluginCursorCache) get(key string) (string, bool) {
	if c == nil {
		return "", false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	v, ok := c.items[key]
	return v, ok
}

func (c *pluginCursorCache) put(key, cursor string) {
	if c == nil || key == "" || cursor == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, exists := c.items[key]; !exists {
		if len(c.order) >= maxPluginCursorEntries {
			evict := c.order[0]
			c.order = c.order[1:]
			delete(c.items, evict)
		}
		c.order = append(c.order, key)
	}
	c.items[key] = cursor
}

func filterMediaType(lib core.Library) core.MediaType {
	if lib.MediaType != "" {
		return lib.MediaType
	}
	return core.TypeMovie
}

func isZeroPresentations(p core.Presentations) bool {
	return !p.AdapterOnly && p.Vela == nil && p.Plex == nil && p.MediaBrowser == nil && p.Stremio == nil && p.Xtream == nil && p.Silo == nil
}

func (s *Service) publishedLibrary(ctx context.Context, lib core.Library) (core.MediaType, string, bool) {
	if lib.Hidden {
		return "", "", false
	}
	switch lib.Source {
	case core.LibraryBuiltin:
		mt := lib.MediaType
		if mt == "" {
			if lib.ID == core.LibraryShows {
				mt = core.TypeShow
			} else {
				mt = core.TypeMovie
			}
		}
		if isZeroPresentations(lib.Present) {
			return mt, s.builtinNative(mt), true
		}
		return s.presentLibrary(lib.Present, mt)
	case core.LibraryCatalog:
		return s.presentLibrary(lib.Present, filterMediaType(lib))
	case core.LibraryCollection:
		return s.presentLibrary(lib.Present, filterMediaType(lib))
	case core.LibraryPlugin:
		if s.d.PluginLibraries == nil || !s.d.PluginLibraries.IsPluginLibraryActive(ctx, lib) {
			return "", "", false
		}
		return s.presentLibrary(lib.Present, s.d.PluginLibraries.RootMediaType(ctx, lib))
	default:
		return "", "", false
	}
}

var defaultCatalogueSortIDs = []string{"title", "added", "released", "year", "rating", "duration", "popularity", "lastViewed"}

func libraryTabToView(tab core.LibraryTab) frontend.ViewTab {
	sortID := tab.Sort
	if sortID != "" && core.IsLastViewedSort(sortID) {
		sortID = string(core.SortLastViewed)
	}
	return frontend.ViewTab{
		ID:                tab.Key,
		Label:             tab.Label,
		Kind:              tab.Kind,
		Level:             tab.Level,
		Facet:             tab.Facet,
		Order:             tab.Order,
		Sort:              sortID,
		Direction:         tab.Direction,
		Card:              tab.EffectiveCard(),
		Layout:            tab.EffectiveLayout(),
		Search:            tab.EffectiveSearch(),
		SearchPlaceholder: tab.SearchPlaceholder,
		Filters:           tab.Filters.Clone(),
	}
}

func libraryTabsToViewTabs(tabs []core.LibraryTab, sorts []string, isCatalogue bool) []frontend.ViewTab {
	out := make([]frontend.ViewTab, 0, len(tabs))
	for _, tab := range tabs {
		if tab.Hidden {
			continue
		}
		vt := libraryTabToView(tab)
		switch tab.Kind {
		case core.TabCollections:
			if isCatalogue && vt.Facet != "" {
				if canon, ok := core.NormalizeCatalogueTabFacet(core.TabCollections, vt.Facet); ok {
					vt.Facet = canon
				}
			}
			if vt.Facet != "" {
				vt.Order = cmp.Or(vt.Order, "count")
				effSort := vt.Sort
				if effSort == "" {
					if slices.Contains(sorts, "released") {
						effSort = "released"
					} else if len(sorts) > 0 {
						effSort = sorts[0]
					} else {
						effSort = "server"
					}
				}
				effDir := vt.Direction
				if effDir == "" {
					switch effSort {
					case "server", "title", "index":
						effDir = "asc"
					default:
						effDir = "desc"
					}
				}
				vt.Sort = effSort
				vt.Direction = effDir
			}
		case core.TabPeople:
			if isCatalogue && vt.Facet != "" {
				if canon, ok := core.NormalizeCatalogueTabFacet(core.TabPeople, vt.Facet); ok {
					vt.Facet = canon
				}
			}
			if vt.Facet != "" {
				vt.Order = cmp.Or(vt.Order, "name")
			}
		}
		out = append(out, vt)
	}
	return out
}

// PublishedViews enumerates published libraries directly, without inventing a viewer.
func (s *Service) PublishedViews(ctx context.Context) ([]frontend.View, error) {
	if s.d.PluginLibraries == nil {
		return nil, nil
	}
	return s.storedViews(ctx, nil)
}

func (s *Service) storedViews(ctx context.Context, v *core.User) ([]frontend.View, error) {
	libs, err := s.d.PluginLibraries.Libraries(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]frontend.View, 0, len(libs))
	for _, lib := range libs {
		if v != nil && !v.AllowsLibrary(lib.ID) {
			continue
		}
		mt, native, ok := s.publishedLibrary(ctx, lib)
		if !ok {
			continue
		}
		revision := ""
		if !lib.UpdatedAt.IsZero() {
			revision = lib.UpdatedAt.UTC().Format("2006-01-02T15:04:05.999999999Z07:00")
		}
		view := frontend.View{
			Revision:       revision,
			ID:             strconv.FormatInt(int64(lib.ID), 10),
			LibraryID:      lib.ID,
			Name:           lib.Name,
			Type:           mt,
			Virtual:        true,
			KindKey:        lib.KindKey,
			PluginID:       lib.PluginID,
			Native:         native,
			Subset:         libraryIsRuleSubset(lib),
			TabsCustomized: lib.TabsCustomized,
		}
		if lib.Source == core.LibraryPlugin {
			if pres, hasPres := s.d.PluginLibraries.(PluginPresentationLibraries); hasPres {
				if p, ok := pres.Presentation(ctx, lib); ok {
					view.Media = p.Media
					view.Card = lib.EffectiveCardShape(p.Card)
					view.DateDisplay = p.DateDisplay
					view.Experience = core.EffectiveLibraryExperience(p.Experience, p.Media)
					view.Tabs = p.Tabs
					view.Sorts = p.Sorts
					view.Facets = p.Facets
					view.ListLevels = p.ListLevels
				}
			}
			if view.Card == "" && lib.CardShape != "" {
				view.Card = lib.EffectiveCardShape("")
			}
			if lib.TabsCustomized && len(view.Tabs) == 0 && len(lib.Tabs) > 0 {
				view.Tabs = libraryTabsToViewTabs(lib.Tabs, view.Sorts, false)
			}
		} else {
			view.Card = lib.EffectiveCardShape("")
			srcTabs := lib.Tabs
			if !lib.TabsCustomized {
				srcTabs = core.DefaultCatalogueTabs()
			}
			view.Tabs = libraryTabsToViewTabs(srcTabs, defaultCatalogueSortIDs, true)
		}
		for _, tab := range lib.Tabs {
			if tab.Hidden && tab.Kind == core.TabLibrary {
				view.HiddenBrowseTabs = append(view.HiddenBrowseTabs, libraryTabToView(tab))
			}
		}
		out = append(out, view)
	}
	return out, nil
}

// libraryIsRuleSubset reports whether lib narrows a broader canonical catalogue
// with rules, a hide default, narrower sources, or a collection.
func libraryIsRuleSubset(lib core.Library) bool {
	if core.IsBuiltinLibrary(lib.ID) || lib.Source == core.LibraryPlugin {
		return false
	}
	if lib.Source == core.LibraryCollection {
		return true
	}
	if lib.Source != core.LibraryCatalog {
		return false
	}
	if len(lib.Rules) > 0 || (lib.VisibilityDefault != "" && lib.VisibilityDefault != "show") {
		return true
	}
	if len(lib.Sources) == 0 {
		return false
	}
	kind := filterMediaType(lib)
	for _, src := range lib.Sources {
		if (kind == core.TypeMovie && src == "tmdb-movies") || (kind == core.TypeShow && src == "tmdb-tv") {
			return false
		}
	}
	return true
}

func (s *Service) allowedFilterLibraries(ctx context.Context, v *core.User) []core.Library {
	if s.d.PluginLibraries == nil || v == nil {
		return nil
	}
	libs, err := reqmemo.Do(ctx, "experience:filterlibs:"+strconv.FormatInt(v.ID, 10), func() ([]core.Library, error) {
		all, err := s.d.PluginLibraries.Libraries(ctx)
		if err != nil {
			return nil, err
		}
		var out []core.Library
		for _, lib := range all {
			if !lib.Hidden && (lib.Source == core.LibraryCatalog || lib.Source == core.LibraryCollection && lib.CollectionID > 0) && v.AllowsLibrary(lib.ID) {
				if _, _, ok := s.publishedLibrary(ctx, lib); ok {
					out = append(out, lib)
				}
			}
		}
		return out, nil
	})
	if err != nil {
		return nil
	}
	return libs
}

func withoutSectionCheck(vis Visibility) Visibility {
	vis.SectionFor = nil
	return vis
}

// sectionOrFilterAllowed reports whether v may access a TMDB title via either
// its canonical built-in library or an allowed, published filter library whose
// saved predicate matches the title (or its parent show for seasons/episodes).
func (s *Service) sectionOrFilterAllowed(ctx context.Context, v *core.User, title *core.Title, sectionFor func(core.MediaType) (int, bool)) bool {
	if title == nil || sectionFor == nil {
		return false
	}
	id, ok := sectionFor(title.Type)
	if ok && SectionAllowed(v, id) {
		if s.d.PluginLibraries == nil {
			return true
		}
		// Ensure the canonical built-in library itself has not been hidden on this protocol.
		if lib, err := s.d.PluginLibraries.Library(ctx, core.LibraryID(id)); err == nil {
			_, _, pub := s.publishedLibrary(ctx, lib)
			if pub && lib.Source == core.LibraryBuiltin {
				return true
			}
		}
	}
	filterLibs := s.allowedFilterLibraries(ctx, v)
	if len(filterLibs) == 0 {
		return false
	}
	// A published, unrestricted TMDB source includes remote search summaries
	// as well as stored rows. Requiring a row here both hides those summaries
	// and adds a membership query for every card on Home. Viewer safety is
	// still applied by the caller after this library-admission check.
	kind := title.Type
	if kind == core.TypeEpisode || kind == core.TypeSeason {
		kind = core.TypeShow
	}
	for _, lib := range filterLibs {
		rules := lib.Rules
		if core.IsBuiltinLibrary(lib.ID) {
			rules = core.WithoutLibraryScopedRules(rules)
		}
		if lib.Source != core.LibraryCatalog || filterMediaType(lib) != kind || lib.RulesBroken || len(rules) != 0 || (lib.VisibilityDefault != "" && lib.VisibilityDefault != "show") {
			continue
		}
		if len(lib.Sources) == 0 {
			return true
		}
		for _, source := range lib.Sources {
			if kind == core.TypeMovie && source == "tmdb-movies" || kind == core.TypeShow && source == "tmdb-tv" {
				return true
			}
		}
	}
	target := title
	if (title.Type == core.TypeSeason || title.Type == core.TypeEpisode) && title.ShowKey != 0 && s.d.CachedGet != nil {
		if show, err := s.d.CachedGet(ctx, title.ShowKey); err == nil && show != nil {
			target = show
		} else {
			return false
		}
	}
	for _, lib := range filterLibs {
		wantType := filterMediaType(lib)
		if target.Type != wantType {
			continue
		}
		if lib.Source == core.LibraryCollection {
			// A collection's shared membership is a semi-join, one indexed
			// read per collection library for the title's key.
			ok, err := s.d.PluginLibraries.MemberOf(ctx, lib, target.Key)
			if err != nil {
				continue
			}
			if ok {
				return true
			}
			continue
		}
		// Membership includes sources and the visible policy in one indexed read.
		member, err := s.d.PluginLibraries.MemberOf(ctx, lib, target.Key)
		if err == nil && member {
			return true
		}

	}
	return false
}

// sectionOrFilterAllowedBatch is sectionOrFilterAllowed evaluated across a
// slice of titles with one set-based membership query per custom filter or
// collection library instead of one query per title.
func (s *Service) sectionOrFilterAllowedBatch(ctx context.Context, v *core.User, titles []core.Title, sectionFor func(core.MediaType) (int, bool)) map[core.RatingKey]bool {
	return s.sectionOrFilterAllowedBatchWithParents(ctx, v, titles, sectionFor, s.d.ParentTitles)
}

func (s *Service) sectionOrFilterAllowedBatchWithParents(ctx context.Context, v *core.User, titles []core.Title, sectionFor func(core.MediaType) (int, bool), readParents parentTitleReader) map[core.RatingKey]bool {
	allowed := make(map[core.RatingKey]bool, len(titles))
	if sectionFor == nil || len(titles) == 0 {
		return allowed
	}
	builtinOK := map[core.MediaType]bool{}
	builtinChecked := map[core.MediaType]bool{}
	checkBuiltin := func(typ core.MediaType) bool {
		if builtinChecked[typ] {
			return builtinOK[typ]
		}
		builtinChecked[typ] = true
		id, ok := sectionFor(typ)
		if !ok || !SectionAllowed(v, id) {
			return false
		}
		if s.d.PluginLibraries == nil {
			builtinOK[typ] = true
			return true
		}
		if lib, err := s.d.PluginLibraries.Library(ctx, core.LibraryID(id)); err == nil {
			_, _, pub := s.publishedLibrary(ctx, lib)
			if pub && lib.Source == core.LibraryBuiltin {
				builtinOK[typ] = true
				return true
			}
		}
		return false
	}

	var remaining []int
	for i := range titles {
		if titles[i].Key.IsPluginKey() {
			continue
		}
		if checkBuiltin(titles[i].Type) {
			allowed[titles[i].Key] = true
		} else {
			remaining = append(remaining, i)
		}
	}
	if len(remaining) == 0 {
		return allowed
	}
	filterLibs := s.allowedFilterLibraries(ctx, v)
	if len(filterLibs) == 0 {
		return allowed
	}
	unrestrictedKind := map[core.MediaType]bool{}
	for _, lib := range filterLibs {
		rules := lib.Rules
		if core.IsBuiltinLibrary(lib.ID) {
			rules = core.WithoutLibraryScopedRules(rules)
		}
		if lib.Source != core.LibraryCatalog || lib.RulesBroken || len(rules) != 0 || (lib.VisibilityDefault != "" && lib.VisibilityDefault != "show") {
			continue
		}
		kind := filterMediaType(lib)
		if len(lib.Sources) == 0 {
			unrestrictedKind[kind] = true
			continue
		}
		for _, source := range lib.Sources {
			if kind == core.TypeMovie && source == "tmdb-movies" || kind == core.TypeShow && source == "tmdb-tv" {
				unrestrictedKind[kind] = true
				break
			}
		}
	}

	var needMember []int
	var parentKeys []core.RatingKey
	seenParent := map[core.RatingKey]bool{}
	for _, idx := range remaining {
		t := &titles[idx]
		kind := t.Type
		if kind == core.TypeEpisode || kind == core.TypeSeason {
			kind = core.TypeShow
		}
		if unrestrictedKind[kind] {
			allowed[t.Key] = true
			continue
		}
		needMember = append(needMember, idx)
		if (t.Type == core.TypeSeason || t.Type == core.TypeEpisode) && t.ShowKey != 0 && !seenParent[t.ShowKey] {
			seenParent[t.ShowKey] = true
			parentKeys = append(parentKeys, t.ShowKey)
		}
	}
	if len(needMember) == 0 {
		return allowed
	}

	shows := map[core.RatingKey]core.Title{}
	if len(parentKeys) > 0 {
		switch {
		case readParents != nil:
			if m, err := readParents(ctx, parentKeys); err == nil {
				shows = m
			}
		case s.d.CachedGetMany != nil:
			if m, err := s.d.CachedGetMany(ctx, parentKeys); err == nil {
				shows = m
			}
		case s.d.CachedGet != nil:
			for _, pk := range parentKeys {
				if show, err := s.d.CachedGet(ctx, pk); err == nil && show != nil {
					shows[pk] = *show
				}
			}
		}
	}

	type targetRef struct {
		titleKey   core.RatingKey
		targetKey  core.RatingKey
		targetType core.MediaType
	}
	refs := make([]targetRef, 0, len(needMember))
	for _, idx := range needMember {
		t := &titles[idx]
		tk, tt := t.Key, t.Type
		if (t.Type == core.TypeSeason || t.Type == core.TypeEpisode) && t.ShowKey != 0 {
			if show, ok := shows[t.ShowKey]; ok {
				tk, tt = show.Key, show.Type
			} else if s.d.CachedGet != nil {
				continue
			}
		}
		refs = append(refs, targetRef{titleKey: t.Key, targetKey: tk, targetType: tt})
	}

	batcher, hasBatcher := s.d.PluginLibraries.(interface {
		MembersOf(context.Context, core.Library, []core.RatingKey) (map[core.RatingKey]bool, error)
	})
	for _, lib := range filterLibs {
		wantType := filterMediaType(lib)
		var unchecked []core.RatingKey
		seenTarget := map[core.RatingKey]bool{}
		for _, ref := range refs {
			if allowed[ref.titleKey] || ref.targetType != wantType || seenTarget[ref.targetKey] {
				continue
			}
			seenTarget[ref.targetKey] = true
			unchecked = append(unchecked, ref.targetKey)
		}
		if len(unchecked) == 0 {
			continue
		}
		var members map[core.RatingKey]bool
		if hasBatcher {
			if m, err := batcher.MembersOf(ctx, lib, unchecked); err == nil {
				members = m
			}
		} else if !core.IsBuiltinLibrary(lib.ID) && s.d.LibraryMembers != nil {
			if m, _, ok, err := s.d.LibraryMembers(ctx, lib.ID, unchecked); err == nil && ok {
				members = m
			}
		}
		if members == nil && s.d.PluginLibraries != nil {
			members = make(map[core.RatingKey]bool, len(unchecked))
			for _, k := range unchecked {
				if ok, err := s.d.PluginLibraries.MemberOf(ctx, lib, k); err == nil && ok {
					members[k] = true
				}
			}
		}
		for _, ref := range refs {
			if !allowed[ref.titleKey] && ref.targetType == wantType && members[ref.targetKey] {
				allowed[ref.titleKey] = true
			}
		}
	}
	return allowed
}

// movieSectionLibraries classifies how v may access movies via built-in or
// custom filter/collection libraries. unrestricted is true when every visible
// movie is admitted without per-library filtering.
func (s *Service) movieSectionLibraries(ctx context.Context, v *core.User) (unrestricted bool, libs []core.Library) {
	if s.d.Visibility.SectionFor != nil {
		if id, ok := s.d.Visibility.SectionFor(core.TypeMovie); ok && SectionAllowed(v, id) {
			if s.d.PluginLibraries == nil {
				return true, nil
			}
			if lib, err := s.d.PluginLibraries.Library(ctx, core.LibraryID(id)); err == nil {
				_, _, pub := s.publishedLibrary(ctx, lib)
				if pub && lib.Source == core.LibraryBuiltin {
					return true, nil
				}
			}
		}
	}
	for _, lib := range s.allowedFilterLibraries(ctx, v) {
		if filterMediaType(lib) != core.TypeMovie {
			continue
		}
		rules := lib.Rules
		if core.IsBuiltinLibrary(lib.ID) {
			rules = core.WithoutLibraryScopedRules(rules)
		}
		if lib.Source == core.LibraryCatalog && !lib.RulesBroken && len(rules) == 0 && (lib.VisibilityDefault == "" || lib.VisibilityDefault == "show") {
			if len(lib.Sources) == 0 {
				return true, nil
			}
			for _, src := range lib.Sources {
				if src == "tmdb-movies" {
					return true, nil
				}
			}
		}
		libs = append(libs, lib)
	}
	return false, libs
}

func (s *Service) pluginItem(ctx context.Context, v *core.User, key core.RatingKey) (PluginItemView, error) {
	if s.d.PluginLibraries == nil || !key.IsPluginKey() {
		return PluginItemView{}, core.ErrNotFound
	}
	piv, err := s.d.PluginLibraries.Item(ctx, key)
	if err != nil {
		return PluginItemView{}, core.ErrNotFound
	}
	if !s.pluginItemAllowed(ctx, v, piv) {
		return PluginItemView{}, core.ErrNotFound
	}
	return piv, nil
}

func (s *Service) pluginItemAllowed(ctx context.Context, v *core.User, piv PluginItemView) bool {
	return s.pluginItemAllowedCached(ctx, v, piv, nil)
}

func (s *Service) pluginItemAllowedCached(ctx context.Context, v *core.User, piv PluginItemView, pubCache map[core.LibraryID]bool) bool {
	if v == nil || piv.Title.Key == 0 || !v.AllowsLibrary(piv.Library.ID) {
		return false
	}
	if pubCache != nil {
		ok, seen := pubCache[piv.Library.ID]
		if !seen {
			_, _, ok = s.publishedLibrary(ctx, piv.Library)
			pubCache[piv.Library.ID] = ok
		}
		if !ok {
			return false
		}
	} else if _, _, ok := s.publishedLibrary(ctx, piv.Library); !ok {
		return false
	}
	if v.MaxContentRating != "" && contentrating.EnforceExceeds(contentrating.EffectiveEnforcement(piv.Title.EnforcementRating, piv.Title.ContentRating, piv.Title.Adult, piv.Title.Keywords), v.MaxContentRating) {
		return false
	}
	return TitleAllowed(s.d.Visibility.Policy, v, &piv.Title)
}

// sanitizePluginLibraryQuery removes sorts and filters a plugin library has
// not declared so undeclared caller parameters are ignored gracefully instead
// of returning empty pages or errors.
func (s *Service) sanitizePluginLibraryQuery(ctx context.Context, lib core.Library, q frontend.ItemQuery) frontend.ItemQuery {
	pres, ok := s.d.PluginLibraries.(PluginPresentationLibraries)
	if !ok {
		return q
	}
	p, ok := pres.Presentation(ctx, lib)
	if !ok {
		return q
	}
	if q.Sort != "" && !q.Sort.IsLastViewed() && !slices.Contains(p.Sorts, string(q.Sort)) {
		q.Sort, q.Desc = "", false
	}
	declared := make(map[string]bool, len(p.Facets))
	for _, vf := range p.Facets {
		declared[vf.ID] = true
	}
	f := core.Filter{
		Query:       q.Filter.Query,
		Person:      q.Filter.Person,
		TabRules:    q.Filter.TabRules,
		TabFallback: q.Filter.TabFallback,
	}
	if len(q.Filter.Genres) > 0 && (declared["genre"] || declared["genres"] || declared["tags"]) {
		f.Genres = q.Filter.Genres
	}
	if q.Filter.Studio != "" && (declared["studio"] || declared["studios"] || declared["publisher"] || declared["author"] || declared["channel"]) {
		f.Studio = q.Filter.Studio
	}
	for k, vals := range q.Filter.Facets {
		if declared[k] && len(vals) > 0 {
			if f.Facets == nil {
				f.Facets = make(map[string][]string)
			}
			f.Facets[k] = vals
		}
	}
	if declared["year"] && (f.Facets == nil || len(f.Facets["year"]) == 0) {
		if q.Filter.YearFrom > 0 && q.Filter.YearFrom == q.Filter.YearTo {
			if f.Facets == nil {
				f.Facets = make(map[string][]string)
			}
			f.Facets["year"] = []string{strconv.Itoa(q.Filter.YearFrom)}
		} else if len(q.Filter.Years) > 0 {
			if f.Facets == nil {
				f.Facets = make(map[string][]string)
			}
			for _, y := range q.Filter.Years {
				f.Facets["year"] = append(f.Facets["year"], strconv.Itoa(y))
			}
		}
	}
	q.Filter = f
	return q
}

// pluginQuerySQLReady reports whether every predicate in q's shape is one
// the plugin item store applies itself in SQL: sort and keyset order, the
// type-to-level mapping, genres/studio/facets, person, tab and library
// policy, watched state, the content-rating cap and excluded keys. Viewer
// predicates are handled separately by pluginBrowseQuery, which folds them
// into the query that reaches the store.
func pluginQuerySQLReady(q frontend.ItemQuery) bool {
	if q.AdjacentTo != 0 || q.StartAt != 0 || q.Season != nil {
		return false
	}
	if q.Favorite != nil && *q.Favorite {
		// Plugin titles are never favourites; the walk answers empty.
		return false
	}
	f := q.Filter
	return len(f.IncludeKeys) == 0 &&
		f.Type == "" &&
		f.YearFrom == 0 && f.YearTo == 0 && len(f.Years) == 0 && len(f.ExcludeYears) == 0 &&
		len(f.ReleasedFilters) == 0 && f.MinRating == 0 && f.MaxRating == 0 && len(f.ExcludeRatings) == 0 &&
		f.MinRuntime == 0 && f.MaxRuntime == 0 && len(f.ExcludeRuntimes) == 0 &&
		len(f.Languages) == 0 && len(f.Countries) == 0 && len(f.ExcludeCountries) == 0 &&
		len(f.ContentRatings) == 0 && len(f.ExcludeContentRatings) == 0 && f.MinContentRating == "" &&
		len(f.IncludeCollectionNames) == 0 && len(f.ExcludeCollectionNames) == 0 &&
		f.Collection == 0 && len(f.Collections) == 0 && len(f.ExcludeCollections) == 0 &&
		len(f.Credits) == 0 && len(f.StudioFilters) == 0 && len(f.TitleFilters) == 0 &&
		f.FirstCharacter == "" &&
		f.NameStartsWith == "" && f.NameStartsWithOrGreater == "" && f.NameLessThan == "" &&
		f.Query == ""
}

// pluginPolicyPassive reports whether the access policy provably admits
// every plugin title for v: no viewer restriction the policy can state,
// and no catalogue viewer rules that judge titles on their facts. Projected
// plugin titles are never Adult, so the mandatory guard adds nothing here.
// With no ViewerRules source a non-nil policy is treated as judging, so a
// partially wired service keeps the in-process check.
func (s *Service) pluginPolicyPassive(v *core.User) bool {
	if s.d.Visibility.Policy == nil {
		return true
	}
	if ViewerRestricted(s.d.Visibility.Policy, v) {
		return false
	}
	if s.d.ViewerRules == nil {
		return false
	}
	_, judged := s.d.ViewerRules(v)
	return !judged
}

// pluginShareCap folds plex.tv share filters to the one clause the plugin
// store can apply: a content-rating cap, and only when the movie and
// television caps agree (one library page can hold either projected type).
func pluginShareCap(movie, television core.Filter) (string, bool) {
	for _, f := range []core.Filter{movie, television} {
		if len(f.ContentRatings) > 0 || len(f.ExcludeContentRatings) > 0 || f.MinContentRating != "" ||
			len(f.IncludeCollectionNames) > 0 || len(f.ExcludeCollectionNames) > 0 {
			return "", false
		}
	}
	if movie.MaxContentRating != television.MaxContentRating {
		return "", false
	}
	return movie.MaxContentRating, true
}

// pluginBrowseQuery folds the viewer's predicates into q so the store
// applies them in SQL before counting and paging: the content-rating cap
// (the viewer's own plus any share cap) via Filter.MaxContentRating,
// policy-blocked genres via Filter.ExcludeGenres, and hidden keys via
// Filter.ExcludeKeys. ok is false when a predicate has no SQL form
// (fact-judging viewer rules, share rating/label clauses), leaving the
// caller on the in-process walk. Blocked keywords need no push: projected
// plugin titles never carry Keywords, so they can never match.
func (s *Service) pluginBrowseQuery(ctx context.Context, v *core.User, lib core.Library, q frontend.ItemQuery) (frontend.ItemQuery, bool) {
	if v == nil || !pluginQuerySQLReady(q) {
		return q, false
	}
	out := q
	out.Filter.MaxContentRating = tightenRating(q.Filter.MaxContentRating, v.MaxContentRating)
	if s.d.PageExclusions == nil {
		if s.pluginPolicyPassive(v) {
			return out, true
		}
		return q, false
	}
	if s.d.ViewerRules != nil {
		if _, judged := s.d.ViewerRules(v); judged {
			return q, false
		}
	} else if s.d.Visibility.Policy != nil && ViewerRestricted(s.d.Visibility.Policy, v) {
		return q, false
	}
	ex := s.d.PageExclusions(ctx, v, s.d.PluginLibraries.RootMediaType(ctx, lib))
	if ex.Err != nil || len(ex.Keys) > maxIndexedExclusionSet || len(ex.ExcludeGenres) > 64 {
		return q, false
	}
	shareCap, pushable := pluginShareCap(ex.Movie, ex.Television)
	if !pushable {
		return q, false
	}
	out.Filter.MaxContentRating = tightenRating(out.Filter.MaxContentRating, shareCap)
	out.Filter.ExcludeGenres = append(append([]string{}, ex.ExcludeGenres...), q.Filter.ExcludeGenres...)
	out.Filter.ExcludeKeys = append(append([]core.RatingKey{}, q.Filter.ExcludeKeys...), ex.Keys...)
	return out, true
}

func pluginCursorPrefix(scope string, q frontend.ItemQuery) string {
	watched := "any"
	if q.Watched != nil {
		if *q.Watched {
			watched = "played"
		} else {
			watched = "unplayed"
		}
	}
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s|%s|%s|%s|%v|%v|%v|%v|%v|%s|%+v", scope, q.Level, q.Role, q.Sort, q.Desc, q.Recursive, q.Types, q.UnservedTypes, q.ExcludedTypes, watched, q.Filter)))
	return hex.EncodeToString(sum[:16]) + "|"
}

func (s *Service) pluginLibraryItems(ctx context.Context, v *core.User, lib core.Library, q frontend.ItemQuery) (core.Page, error) {
	// A studio or performer opened for the first time fills its catalogue
	// before the page is read (MaybeExpand waits, bounded, on the first page),
	// so the viewer sees every title the first time.
	expanding := false
	if !q.SuppressExpand && strings.TrimSpace(q.Filter.Query) == "" {
		if exp, ok := s.d.PluginLibraries.(PluginSliceExpander); ok {
			expanding = exp.MaybeExpand(ctx, lib, q)
		}
	}
	page, err := s.readPluginLibraryItems(ctx, v, lib, q)
	if err != nil {
		return core.Page{}, err
	}
	page.Expanding = expanding
	return page, nil
}

func (s *Service) sortTitlesByLastViewed(ctx context.Context, v *core.User, items []core.Title, desc bool) {
	if len(items) < 2 || v == nil || s.d.State == nil {
		return
	}
	keys := make([]core.RatingKey, len(items))
	for i := range items {
		keys[i] = items[i].Key
	}
	states, err := s.d.State.GetMany(ctx, v.ID, keys)
	if err != nil {
		return
	}
	sort.SliceStable(items, func(i, j int) bool {
		ti := states[items[i].Key].LastViewedAt
		tj := states[items[j].Key].LastViewedAt
		if ti.IsZero() != tj.IsZero() {
			return !ti.IsZero()
		}
		if !ti.IsZero() && !ti.Equal(tj) {
			if desc {
				return ti.After(tj)
			}
			return ti.Before(tj)
		}
		return false
	})
}

// pluginSearchWindow is how many ranked search hits a library search pages
// through.
const pluginSearchWindow = 500

func (s *Service) readPluginLibraryItems(ctx context.Context, v *core.User, lib core.Library, q frontend.ItemQuery) (core.Page, error) {
	if v != nil && core.CatalogViewerID(ctx) == 0 {
		ctx = core.WithCatalogViewer(ctx, v)
	}
	if query := strings.TrimSpace(q.Filter.Query); query != "" {
		limit := q.Limit
		if limit <= 0 {
			limit = 200
		}
		// One fixed candidate window for every page: a window that grew with
		// the offset ranked a different set per page, so pages repeated and
		// skipped titles. Searches deeper than the window end with it.
		window := pluginSearchWindow
		subQ := q
		subQ.Filter.Query = ""
		hits, err := s.d.PluginLibraries.Search(ctx, lib, query, subQ, window)
		if err != nil {
			return core.Page{}, err
		}
		filtered, err := s.filterPluginTitles(ctx, v, hits, subQ)
		if err != nil {
			return core.Page{}, err
		}
		if ppl, ok := s.d.PluginLibraries.(PluginPeopleLibraries); ok && len(subQ.Filter.Facets) == 0 && len(subQ.Filter.TabRules) == 0 && subQ.Filter.TabFallback == "" {
			_, peopleTitles, pErr := s.searchPluginPeopleInLibraries(ctx, v, []core.Library{lib}, ppl, query, subQ, 20)
			if pErr != nil && errors.Is(pErr, context.Canceled) {
				return core.Page{}, pErr
			}
			if len(peopleTitles) > 0 {
				seen := make(map[core.RatingKey]bool, len(filtered)+len(peopleTitles))
				for _, item := range filtered {
					seen[item.Key] = true
				}
				for _, item := range peopleTitles {
					if !seen[item.Key] {
						seen[item.Key] = true
						filtered = append(filtered, item)
					}
				}
			}
		}
		if q.Sort.IsLastViewed() {
			s.sortTitlesByLastViewed(ctx, v, filtered, q.Desc)
		} else if q.Sort != "" {
			if err := s.sortTitles(ctx, filtered, q); err != nil {
				return core.Page{}, err
			}
		} else {
			sort.SliceStable(filtered, func(i, j int) bool {
				if filtered[i].SearchScore != filtered[j].SearchScore {
					return filtered[i].SearchScore > filtered[j].SearchScore
				}
				return filtered[i].Key < filtered[j].Key
			})
		}
		return slicePluginTitles(filtered, subQ)
	}

	offset := max(q.Offset, 0)
	limit := q.Limit
	if limit <= 0 {
		limit = 200
	}

	// Fast O(limit) keyset path when the store applies every predicate in
	// SQL, viewer predicates included (pluginBrowseQuery folded them in).
	if sqlQ, ok := s.pluginBrowseQuery(ctx, v, lib, q); ok && q.Limit > 0 {
		q = sqlQ
		viewerID := int64(0)
		if v != nil {
			viewerID = v.ID
		}
		prefix := pluginCursorPrefix(fmt.Sprintf("lib:%d:u:%d", lib.ID, viewerID), q)
		cursor := ""
		curOffset := 0
		if offset > 0 && s.pluginCursors != nil {
			if cached, ok := s.pluginCursors.get(prefix + strconv.Itoa(offset)); ok {
				cursor = cached
				curOffset = offset
			}
		}
		// Advance keyset cursor up to offset without retaining skipped pages.
		stepQ := q
		stepQ.NoTotal = true
		stepQ.CheapTotal = false
		for curOffset < offset {
			step := min(offset-curOffset, 500)
			page, err := s.d.PluginLibraries.Browse(ctx, lib, stepQ, cursor, step)
			if err != nil {
				return core.Page{}, err
			}
			if len(page.Items) == 0 || page.NextCursor == "" {
				return core.Page{Items: []core.Title{}, Total: curOffset + len(page.Items), TotalExact: true}, nil
			}
			curOffset += len(page.Items)
			cursor = page.NextCursor
			if s.pluginCursors != nil {
				s.pluginCursors.put(prefix+strconv.Itoa(curOffset), cursor)
			}
		}
		page, err := s.d.PluginLibraries.Browse(ctx, lib, q, cursor, limit)
		if err != nil {
			return core.Page{}, err
		}
		if page.NextCursor != "" && s.pluginCursors != nil {
			s.pluginCursors.put(prefix+strconv.Itoa(offset+len(page.Items)), page.NextCursor)
		}
		if page.Items == nil {
			page.Items = []core.Title{}
		}
		return core.Page{Items: page.Items, Total: page.Total, TotalExact: page.TotalExact}, nil
	}

	// In-process keyset scan for the predicates with no SQL form (anchors,
	// fact-judging viewer rules, share rating/label clauses). It runs to
	// exhaustion so the total is the true filtered count, never a capped
	// count presented as the total; NoTotal callers still stop once the
	// page is filled.
	var permitted []core.Title
	cursor := ""
	needAll := q.AdjacentTo != 0 || q.StartAt != 0 || q.Limit <= 0
	wantEnd := offset + limit
	totalMatched := 0
	exhausted := false
	scanQ := q
	scanQ.NoTotal = true
	scanQ.CheapTotal = false
	for scanned := 0; ; {
		step := 200
		if q.PluginScanLimit > 0 {
			step = min(step, q.PluginScanLimit-scanned)
		}
		page, err := s.d.PluginLibraries.Browse(ctx, lib, scanQ, cursor, step)
		if err != nil {
			return core.Page{}, err
		}
		batch, err := s.filterPluginTitles(ctx, v, page.Items, q)
		if err != nil {
			return core.Page{}, err
		}
		for _, item := range batch {
			if needAll || (totalMatched >= offset && len(permitted) < limit) {
				permitted = append(permitted, item)
			}
			totalMatched++
		}
		if page.NextCursor == "" || len(page.Items) == 0 || page.NextCursor == cursor {
			exhausted = true
			break
		}
		scanned += len(page.Items)
		if q.PluginScanLimit > 0 && scanned >= q.PluginScanLimit {
			break
		}
		if !needAll && totalMatched >= wantEnd && q.NoTotal {
			break
		}
		cursor = page.NextCursor
	}
	if !needAll {
		if permitted == nil {
			permitted = []core.Title{}
		}
		return core.Page{Items: permitted, Total: totalMatched, TotalExact: exhausted}, nil
	}
	return slicePluginTitles(permitted, q)
}

func (s *Service) pluginChildren(ctx context.Context, v *core.User, parent core.RatingKey, q frontend.ItemQuery) (core.Page, error) {
	piv, err := s.pluginItem(core.WithNoContainerExpand(ctx), v, parent)
	if err != nil {
		return core.Page{}, err
	}
	if q.SuppressExpand {
		ctx = core.WithNoContainerExpand(ctx)
	}
	// Album and audiobook track/part lists naturally follow disc/track order,
	// including Jellyfin Web's Album Play query (SortBy=SortName ascending).
	// A list (a playlist) keeps its own order: the store returns its members in
	// list order, numbered by position. The default, title and index orders all
	// mean that order (apps that open it as an album ask for index or title);
	// sorting by index here would group members by their own albums.
	if piv.Title.MediaRole == core.MediaRolePlaylist &&
		(q.Sort == "" || q.Sort == core.SortIndex || (q.Sort == core.SortTitle && !q.Desc)) {
		q.Sort = ""
	} else if (piv.Title.MediaRole == core.MediaRoleAlbum || piv.Title.MBItemType == "MusicAlbum" ||
		piv.Title.MediaRole == core.MediaRoleAudiobook || piv.Title.MBItemType == "AudioBook") &&
		(q.Sort == "" || (q.Sort == core.SortTitle && !q.Desc)) {
		q.Sort = core.SortIndex
	}
	offset := max(q.Offset, 0)
	limit := q.Limit
	if limit <= 0 {
		limit = 200
	}

	// Children carries no ItemQuery to the store, so its SQL page can only
	// be trusted when no viewer predicate or type/level filter would filter it in process.
	queryReader, hasQueryChildren := s.d.PluginLibraries.(PluginQueryChildrenLibraries)
	filteredReader, hasFilteredChildren := s.d.PluginLibraries.(PluginFilteredChildrenLibraries)
	fetchChildren := func(cur string, lim int) (PluginBrowsePage, error) {
		if hasQueryChildren {
			return queryReader.ChildrenQuery(ctx, parent, q, cur, lim)
		}
		if hasFilteredChildren && (q.Level != "" || q.Role != "") {
			return filteredReader.ChildrenFiltered(ctx, parent, q.Recursive, q.Level, q.Role, cur, lim)
		}
		return s.d.PluginLibraries.Children(ctx, parent, q.Recursive, cur, lim)
	}
	levelRoleSQLReady := (q.Level == "" && q.Role == "") || hasQueryChildren || hasFilteredChildren
	if v != nil && pluginQuerySQLReady(q) && levelRoleSQLReady && len(q.Types) == 0 && len(q.UnservedTypes) == 0 && len(q.ExcludedTypes) == 0 &&
		q.Watched == nil && v.MaxContentRating == "" && q.Filter.MaxContentRating == "" &&
		len(q.Filter.ExcludeKeys) == 0 && len(q.Filter.ExcludeGenres) == 0 && s.pluginPolicyPassive(v) && q.Sort == "" && q.Limit > 0 {
		prefix := pluginCursorPrefix("child:"+parent.String(), q)
		cursor := ""
		curOffset := 0
		if offset > 0 && s.pluginCursors != nil {
			if cached, ok := s.pluginCursors.get(prefix + strconv.Itoa(offset)); ok {
				cursor = cached
				curOffset = offset
			}
		}
		for curOffset < offset {
			step := min(offset-curOffset, 500)
			page, err := fetchChildren(cursor, step)
			if err != nil {
				return core.Page{}, err
			}
			if len(page.Items) == 0 || page.NextCursor == "" {
				return core.Page{Items: []core.Title{}, Total: page.Total}, nil
			}
			curOffset += len(page.Items)
			cursor = page.NextCursor
			if s.pluginCursors != nil {
				s.pluginCursors.put(prefix+strconv.Itoa(curOffset), cursor)
			}
		}
		page, err := fetchChildren(cursor, limit)
		if err != nil {
			return core.Page{}, err
		}
		if page.NextCursor != "" && s.pluginCursors != nil {
			s.pluginCursors.put(prefix+strconv.Itoa(offset+len(page.Items)), page.NextCursor)
		}
		if page.Items == nil {
			page.Items = []core.Title{}
		}
		return core.Page{Items: page.Items, Total: page.Total}, nil
	}

	if q.ChildScanLimit > 0 {
		if !hasQueryChildren {
			return core.Page{}, nil
		}
		page, err := fetchChildren("", q.ChildScanLimit)
		if err != nil || page.NextCursor != "" {
			return core.Page{}, err
		}
		items, err := s.filterPluginTitles(ctx, v, page.Items, q)
		if err != nil {
			return core.Page{}, err
		}
		return slicePluginTitles(items, q)
	}

	var all []core.Title
	cursor := ""
	filterQ := q
	if (hasQueryChildren || hasFilteredChildren) && (q.Level != "" || q.Role != "") {
		// Already filtered in SQL using the manifest's exact level/role mapping.
		filterQ.Level = ""
		filterQ.Role = ""
	}
	for {
		page, err := fetchChildren(cursor, 200)
		if err != nil {
			return core.Page{}, err
		}
		batch, err := s.filterPluginTitles(ctx, v, page.Items, filterQ)
		if err != nil {
			return core.Page{}, err
		}
		all = append(all, batch...)
		if page.NextCursor == "" || len(page.Items) == 0 || page.NextCursor == cursor {
			break
		}
		cursor = page.NextCursor
	}
	if q.Sort != "" {
		if err := s.sortTitles(ctx, all, q); err != nil {
			return core.Page{}, err
		}
	}
	return slicePluginTitles(all, q)
}

func (s *Service) filterPluginTitles(ctx context.Context, v *core.User, items []core.Title, q frontend.ItemQuery) ([]core.Title, error) {
	items = s.dropHiddenCopies(ctx, v, items)
	if len(items) == 0 {
		return nil, nil
	}
	var watched map[core.RatingKey]core.WatchStatus
	if q.Watched != nil {
		keys := make([]core.RatingKey, len(items))
		for i := range items {
			keys[i] = items[i].Key
		}
		var err error
		watched, err = s.d.State.GetMany(ctx, v.ID, keys)
		if err != nil {
			return nil, err
		}
	}
	// Plugin library titles are not favourites in this build: they are not in
	// the catalogue the favourites policy reads, so favouriting one is refused
	// and none is ever stored. Honour the filter directly rather than asking
	// the favourites store, whose policy check answers ErrForbidden for keys it
	// cannot resolve and would fail the whole page (Jellyfin answered 503 to
	// `Filters=IsFavorite` on a plugin library).
	if q.Favorite != nil && *q.Favorite {
		return []core.Title{}, nil
	}
	sub := q
	sub.Watched = nil
	sub.Favorite = nil
	out := make([]core.Title, 0, len(items))
	for _, t := range items {
		if v.MaxContentRating != "" && contentrating.EnforceExceeds(contentrating.EffectiveEnforcement(t.EnforcementRating, t.ContentRating, t.Adult, t.Keywords), v.MaxContentRating) {
			continue
		}
		if !TitleAllowed(s.d.Visibility.Policy, v, &t) {
			continue
		}
		if q.Watched != nil && (watched[t.Key].ViewCount > 0) != *q.Watched {
			continue
		}
		ok, err := s.matchesUserQuery(ctx, v, t, sub)
		if err != nil {
			return nil, err
		}
		if ok {
			out = append(out, t)
		}
	}
	return out, nil
}

func slicePluginTitles(items []core.Title, q frontend.ItemQuery) (core.Page, error) {
	if q.AdjacentTo != 0 {
		return adjacentTitles(items, q)
	}
	if q.StartAt != 0 {
		return startingTitles(items, q)
	}
	total := len(items)
	start := min(max(q.Offset, 0), total)
	end := total
	if q.Limit > 0 {
		end = min(start+q.Limit, total)
	}
	slice := items[start:end]
	if slice == nil {
		slice = []core.Title{}
	}
	return core.Page{Items: slice, Total: total}, nil
}

func (s *Service) pluginAncestors(ctx context.Context, v *core.User, key core.RatingKey) ([]core.Title, error) {
	if _, err := s.pluginItem(core.WithNoContainerExpand(ctx), v, key); err != nil {
		return nil, err
	}
	ancestors, lib, err := s.d.PluginLibraries.Ancestors(ctx, key)
	if err != nil {
		return nil, err
	}
	out := make([]core.Title, 0, len(ancestors))
	for _, a := range ancestors {
		piv := PluginItemView{Library: lib, Title: a}
		if !s.pluginItemAllowed(ctx, v, piv) {
			return nil, core.ErrNotFound
		}
		out = append(out, a)
	}
	return out, nil
}

func (s *Service) pluginLibraryFacets(ctx context.Context, viewer *core.Viewer, lib core.Library, name string) ([]core.FacetValue, error) {
	return s.pluginLibraryFacetsAtLevel(ctx, viewer, lib, "", name)
}

func (s *Service) pluginLibraryFacetsAtLevel(ctx context.Context, viewer *core.Viewer, lib core.Library, level, name string) ([]core.FacetValue, error) {
	v, err := s.viewer(ctx, viewer)
	if err != nil {
		return nil, err
	}
	if !v.AllowsLibrary(lib.ID) {
		return nil, core.ErrNotFound
	}
	if _, _, ok := s.publishedLibrary(ctx, lib); !ok {
		return nil, core.ErrNotFound
	}
	if level != "" {
		if lf, ok := s.d.PluginLibraries.(PluginLevelFacetLibraries); ok {
			return lf.FacetsAtLevel(ctx, lib, level, name)
		}
	}
	return s.d.PluginLibraries.Facets(ctx, lib, name)
}

func (s *Service) filterLibraryFacets(ctx context.Context, viewer *core.Viewer, lib core.Library, name string) ([]core.FacetValue, bool, error) {
	if s.d.FacetFilter == nil || s.d.PageExclusions == nil {
		return nil, false, nil
	}
	sqlFacet := sqlFacets[name]
	if sqlFacet == "" {
		return nil, false, nil
	}
	v, err := s.viewer(ctx, viewer)
	if err != nil {
		return nil, false, err
	}
	if !v.AllowsLibrary(lib.ID) {
		return nil, false, core.ErrNotFound
	}
	mt, _, ok := s.publishedLibrary(ctx, lib)
	if !ok {
		return nil, false, core.ErrNotFound
	}
	ex := s.d.PageExclusions(ctx, v, mt)
	if ex.Err != nil {
		return nil, false, ex.Err
	}
	base := s.libraryBrowseFilter(ctx, v, lib, core.Filter{Type: mt})
	filter, fits, none := viewerBrowseFilter(base, v, ex, mt)
	if !fits {
		return nil, false, nil
	}
	if none {
		return []core.FacetValue{}, true, nil
	}
	vals, err := s.d.FacetFilter(ctx, filter, sqlFacet)
	if err != nil {
		return nil, false, err
	}
	for i := range vals {
		vals[i].ID = facetID(name, vals[i].Key)
	}
	return vals, true, nil
}

// pluginSearchConcurrency bounds the plugin libraries one search reads at
// once.
const pluginSearchConcurrency = 8

// pluginSearchItems searches the viewer's published plugin libraries for an
// unscoped search, every library at once. Libraries an app was opted into
// (PluginOptInLibraries) and searches for a catalogue media class
// (ItemQuery.CatalogueOnly) leave plugin items out; scoped library searches
// go through readPluginLibraryItems instead. Scores are placed on the
// catalogue's scale by unscopedPluginScore; best is the highest score
// before that, on the plugin's own scale.
func (s *Service) pluginSearchItems(ctx context.Context, v *core.User, query string, q frontend.ItemQuery, window int) (items []core.Title, best float64, err error) {
	if s.d.PluginLibraries == nil || strings.TrimSpace(query) == "" || q.CatalogueOnly {
		return nil, 0, nil
	}
	libs, err := s.d.PluginLibraries.Libraries(ctx)
	if err != nil {
		return nil, 0, err
	}
	subQ := q
	subQ.Filter.Query = ""
	var targets []core.Library
	for _, lib := range libs {
		if lib.Source != core.LibraryPlugin || !v.AllowsLibrary(lib.ID) || s.optInLibrary(ctx, lib) {
			continue
		}
		if _, _, ok := s.publishedLibrary(ctx, lib); !ok {
			continue
		}
		targets = append(targets, lib)
	}
	hits := make([][]core.Title, len(targets)+1)
	errs := make([]error, len(targets)+1)
	var wg sync.WaitGroup
	sem := make(chan struct{}, pluginSearchConcurrency)
	for i, lib := range targets {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			found, err := s.d.PluginLibraries.Search(ctx, lib, query, subQ, window)
			if err != nil {
				if errors.Is(err, context.Canceled) {
					errs[i] = err
				}
				return
			}
			hits[i], errs[i] = s.filterPluginTitles(ctx, v, found, subQ)
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, peopleTitles, err := s.searchPluginPeopleForUser(ctx, v, query, subQ, 20)
		if err != nil && errors.Is(err, context.Canceled) {
			errs[len(targets)] = err
		}
		hits[len(targets)] = peopleTitles
	}()
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			return nil, 0, err
		}
	}
	var out []core.Title
	seen := make(map[core.RatingKey]bool)
	for _, group := range hits {
		for _, item := range group {
			if !seen[item.Key] {
				seen[item.Key] = true
				best = max(best, item.SearchScore)
				item.SearchScore = unscopedPluginScore(item.SearchScore)
				out = append(out, item)
			}
		}
	}
	if len(out) > 1 {
		sort.SliceStable(out, func(i, j int) bool {
			if out[i].SearchScore != out[j].SearchScore {
				return out[i].SearchScore > out[j].SearchScore
			}
			return out[i].Popularity > out[j].Popularity
		})
	}
	return out, best, nil
}

// unscopedPluginScore places a plugin library's relevance (1.0 for its
// exact title, 0.90 a prefix, 0.60 a loose or cast match, plus up to 0.08
// of the plugin's own popularity) on the catalogue's scale for a search
// across every library. Plugin items carry no catalogue standing, so the
// films and series the viewer most likely meant come first: an exact or
// franchise title, a person's filmography ("tom hanks" is Tom Hanks's films
// before books called Tom Hanks), a title being typed, and a well-known
// title holding the query's words in order ("breaking bad": El Camino
// before songs called Breaking Bad). A plugin's exact title still outranks
// loose catalogue matches.
func unscopedPluginScore(score float64) float64 {
	return min(score, 1) * 0.82
}

// PluginOptInLibraries is an optional PluginLibraries capability: OptInLibrary
// reports a library whose kind starts unpublished in protocol's app (its
// manifest marks the presentation default:false), private content an owner
// opted the app into. Browsing it is unchanged; search reaches it only when
// scoped to it, never in an unscoped or media-class search.
type PluginOptInLibraries interface {
	OptInLibrary(ctx context.Context, lib core.Library, protocol core.Protocol) bool
}

func (s *Service) optInLibrary(ctx context.Context, lib core.Library) bool {
	o, ok := s.d.PluginLibraries.(PluginOptInLibraries)
	return ok && o.OptInLibrary(ctx, lib, s.d.Protocol)
}

func (s *Service) builtinNative(mt core.MediaType) string {
	if s.d.BuiltinNative != nil {
		return s.d.BuiltinNative(mt)
	}
	return string(mt)
}

func (s *Service) presentLibrary(p core.Presentations, mt core.MediaType) (core.MediaType, string, bool) {
	if s.d.LibraryPresentation == nil {
		return "", "", false
	}
	return s.d.LibraryPresentation(p, mt)
}

func (s *Service) publishedLibraryByViewID(ctx context.Context, v *core.User, viewID string) (core.Library, core.MediaType, error) {
	if s.d.PluginLibraries == nil || v == nil {
		return core.Library{}, "", core.ErrNotFound
	}
	idNum, err := strconv.ParseInt(strings.TrimSpace(viewID), 10, 64)
	if err != nil || idNum <= 0 {
		return core.Library{}, "", core.ErrNotFound
	}
	lib, err := s.d.PluginLibraries.Library(ctx, core.LibraryID(idNum))
	if err != nil || !v.AllowsLibrary(lib.ID) {
		return core.Library{}, "", core.ErrNotFound
	}
	mt, _, ok := s.publishedLibrary(ctx, lib)
	if !ok {
		return core.Library{}, "", core.ErrNotFound
	}
	return lib, mt, nil
}

func (s *Service) pluginLibraryByViewID(ctx context.Context, v *core.User, viewID string) (core.Library, PluginPresentationLibraries, PluginPresentation, error) {
	lib, _, err := s.publishedLibraryByViewID(ctx, v, viewID)
	if err != nil || lib.Source != core.LibraryPlugin {
		return core.Library{}, nil, PluginPresentation{}, core.ErrNotFound
	}
	pres, ok := s.d.PluginLibraries.(PluginPresentationLibraries)
	if !ok {
		return lib, nil, PluginPresentation{}, nil
	}
	p, _ := pres.Presentation(ctx, lib)
	return lib, pres, p, nil
}

func (s *Service) catalogueFacetQueryFilter(ctx context.Context, v *core.User, lib core.Library, mt core.MediaType) (core.Filter, bool, error) {
	base := s.libraryBrowseFilter(ctx, v, lib, core.Filter{Type: mt})
	if s.d.PageExclusions != nil {
		ex := s.d.PageExclusions(ctx, v, mt)
		if ex.Err != nil {
			return core.Filter{}, false, ex.Err
		}
		filter, fits, none := viewerBrowseFilter(base, v, ex, mt)
		if none {
			return filter, true, nil
		}
		if fits {
			return filter, false, nil
		}
	}
	if v != nil {
		base.CatalogViewerID = v.ID
		base.MaxContentRating = tightenRating(base.MaxContentRating, v.MaxContentRating)
	}
	return base, false, nil
}

// LibraryFacetBuckets implements frontend.LibraryFacetBuckets.
func (s *Service) LibraryFacetBuckets(ctx context.Context, viewer *core.Viewer, dev core.Device, viewID, field string) ([]frontend.FacetBucket, error) {
	return s.LibraryFacetBucketsAtLevel(ctx, viewer, dev, viewID, "", field)
}

// LibraryFacetBucketsAtLevel implements frontend.LibraryFacetBucketsAtLevel.
func (s *Service) LibraryFacetBucketsAtLevel(ctx context.Context, viewer *core.Viewer, _ core.Device, viewID, level, field string) ([]frontend.FacetBucket, error) {
	ctx = core.WithCatalogViewer(ctx, viewer)
	v, err := s.viewer(ctx, viewer)
	if err != nil {
		return nil, err
	}
	lib, pres, p, err := s.pluginLibraryByViewID(ctx, v, viewID)
	if err != nil || pres == nil {
		return nil, core.ErrNotFound
	}
	declared := false
	for _, f := range p.Facets {
		if f.ID == field {
			declared = true
			break
		}
	}
	if !declared {
		return nil, core.ErrNotFound
	}
	if level != "" {
		if lf, ok := s.d.PluginLibraries.(PluginLevelFacetLibraries); ok {
			return lf.FacetBucketsAtLevel(ctx, lib, level, field)
		}
	}
	return pres.FacetBuckets(ctx, lib, field)
}

// LibraryFacetCollections implements frontend.LibraryFacetCollections.
func (s *Service) LibraryFacetCollections(ctx context.Context, viewer *core.Viewer, _ core.Device, viewID, field, order string, page frontend.PageQuery) (frontend.FacetCollectionPage, error) {
	ctx = core.WithCatalogViewer(ctx, viewer)
	v, err := s.viewer(ctx, viewer)
	if err != nil {
		return frontend.FacetCollectionPage{}, err
	}
	lib, mt, err := s.publishedLibraryByViewID(ctx, v, viewID)
	if err != nil {
		return frontend.FacetCollectionPage{}, core.ErrNotFound
	}
	if lib.Source == core.LibraryPlugin {
		pres, ok := s.d.PluginLibraries.(PluginPresentationLibraries)
		if !ok || pres == nil {
			return frontend.FacetCollectionPage{}, core.ErrNotFound
		}
		p, _ := pres.Presentation(ctx, lib)
		declared := false
		for _, f := range p.Facets {
			if f.ID == field {
				declared = true
				break
			}
		}
		if !declared {
			return frontend.FacetCollectionPage{}, core.ErrNotFound
		}
		var out frontend.FacetCollectionPage
		if page.Level != "" {
			if lf, ok := s.d.PluginLibraries.(PluginLevelFacetLibraries); ok {
				out, err = lf.FacetCollectionsAtLevel(ctx, lib, page.Level, field, order, page.Query, page.Offset, page.Limit)
			} else {
				out, err = pres.FacetCollections(ctx, lib, field, order, page.Query, page.Offset, page.Limit)
			}
		} else {
			out, err = pres.FacetCollections(ctx, lib, field, order, page.Query, page.Offset, page.Limit)
		}
		if err != nil {
			return frontend.FacetCollectionPage{}, err
		}
		publication := map[core.LibraryID]bool{lib.ID: true}
		for i := range out.Items {
			if out.Items[i].NewestItem != nil {
				piv := PluginItemView{Library: lib, Title: *out.Items[i].NewestItem}
				if !s.pluginItemAllowedCached(ctx, v, piv, publication) {
					out.Items[i].NewestItem = nil
				}
			}
		}
		return out, nil
	}

	canonFacet, ok := core.NormalizeCatalogueTabFacet(core.TabCollections, field)
	if !ok || canonFacet == "" {
		return frontend.FacetCollectionPage{}, core.ErrNotFound
	}
	catReader, ok := s.d.Catalog.(catalogTabFacetReader)
	if !ok || catReader == nil {
		return frontend.FacetCollectionPage{}, core.ErrNotFound
	}
	filter, none, err := s.catalogueFacetQueryFilter(ctx, v, lib, mt)
	if err != nil {
		return frontend.FacetCollectionPage{}, err
	}
	if none {
		return frontend.FacetCollectionPage{Items: []frontend.FacetCollection{}}, nil
	}
	raw, err := catReader.LibraryFacetCollections(ctx, core.CatalogFacetCollectionsQuery{
		Filter: filter,
		Facet:  canonFacet,
		Order:  order,
		Query:  page.Query,
		Offset: page.Offset,
		Limit:  page.Limit,
	})
	if err != nil {
		return frontend.FacetCollectionPage{}, err
	}
	var samples []core.Title
	for _, entry := range raw.Items {
		if entry.NewestItem != nil {
			samples = append(samples, *entry.NewestItem)
		}
	}
	visible, err := s.allowTitles(ctx, v, samples)
	if err != nil {
		return frontend.FacetCollectionPage{}, err
	}
	allowed := make(map[core.RatingKey]bool, len(visible))
	for _, title := range visible {
		allowed[title.Key] = true
	}
	items := make([]frontend.FacetCollection, 0, len(raw.Items))
	for _, entry := range raw.Items {
		var newest *core.Title
		if entry.NewestItem != nil && allowed[entry.NewestItem.Key] {
			newest = entry.NewestItem
		}
		items = append(items, frontend.FacetCollection{
			ID:         entry.ID,
			Name:       entry.Name,
			ItemCount:  entry.ItemCount,
			NewestItem: newest,
		})
	}
	return frontend.FacetCollectionPage{Items: items, More: raw.More}, nil
}

// isArtistStringFacet reports whether a plugin view facet carries artist
// credits using string/strings type (e.g. Deezer's "artists" facet) rather
// than type:"people".
func isArtistStringFacet(f frontend.ViewFacet) bool {
	if f.Type != "string" && f.Type != "strings" {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(f.ID)) {
	case "artist", "artists", "album_artist", "album_artists", "albumartist", "albumartists":
		return true
	default:
		return false
	}
}

// LibraryFacetPeople implements frontend.LibraryFacetPeople.
func (s *Service) LibraryFacetPeople(ctx context.Context, viewer *core.Viewer, _ core.Device, viewID, field, order string, page frontend.PageQuery) (frontend.FacetPersonPage, error) {
	ctx = core.WithCatalogViewer(ctx, viewer)
	v, err := s.viewer(ctx, viewer)
	if err != nil {
		return frontend.FacetPersonPage{}, err
	}
	lib, mt, err := s.publishedLibraryByViewID(ctx, v, viewID)
	if err != nil {
		return frontend.FacetPersonPage{}, core.ErrNotFound
	}
	if lib.Source == core.LibraryPlugin {
		pres, ok := s.d.PluginLibraries.(PluginPresentationLibraries)
		if !ok || pres == nil {
			return frontend.FacetPersonPage{}, core.ErrNotFound
		}
		p, _ := pres.Presentation(ctx, lib)
		declaredPeople := false
		for _, f := range p.Facets {
			if f.ID == field && (f.Type == "people" || isArtistStringFacet(f)) {
				declaredPeople = true
				break
			}
		}
		if !declaredPeople {
			return frontend.FacetPersonPage{}, core.ErrNotFound
		}
		var raw PluginFacetPeoplePage
		if page.Level != "" {
			if lf, ok := s.d.PluginLibraries.(PluginLevelFacetLibraries); ok {
				raw, err = lf.FacetPeopleAtLevel(ctx, lib, page.Level, field, order, page.Query, page.Offset, page.Limit)
			} else {
				raw, err = pres.FacetPeople(ctx, lib, field, order, page.Query, page.Offset, page.Limit)
			}
		} else {
			raw, err = pres.FacetPeople(ctx, lib, field, order, page.Query, page.Offset, page.Limit)
		}
		if err != nil {
			return frontend.FacetPersonPage{}, err
		}
		publication := map[core.LibraryID]bool{lib.ID: true}
		items := make([]frontend.FacetPerson, 0, len(raw.Items))
		for _, entry := range raw.Items {
			if !core.IsPluginPersonID(entry.PersonID) || strings.TrimSpace(entry.Name) == "" || entry.Count <= 0 {
				continue
			}
			var artKey core.RatingKey
			var images []frontend.Image
			if entry.SampleTitle != nil {
				piv := PluginItemView{Library: lib, Title: *entry.SampleTitle}
				if s.pluginItemAllowedCached(ctx, v, piv, publication) {
					artKey = entry.SampleTitle.Key
					if entry.HasThumb {
						for _, group := range [][]core.Person{entry.SampleTitle.Cast, entry.SampleTitle.Directors, entry.SampleTitle.Writers, entry.SampleTitle.Crew} {
							for _, p := range group {
								if p.TMDB == entry.PersonID && p.ThumbPath != "" {
									images = append(images, frontend.Image{
										Kind:    frontend.ImagePerson,
										Path:    p.ThumbPath,
										Subject: "tmdb:" + strconv.FormatInt(p.TMDB, 10),
									})
									break
								}
							}
							if len(images) > 0 {
								break
							}
						}
					}
					if entry.SampleTitle.PosterPath != "" {
						images = append(images, frontend.Image{
							Kind: frontend.ImagePoster,
							Path: entry.SampleTitle.PosterPath,
						})
					}
				}
			}
			items = append(items, frontend.FacetPerson{
				Person: frontend.PersonDetail{
					ID:          personFacetID(entry.PersonID),
					Name:        entry.Name,
					TMDB:        entry.PersonID,
					ArtworkItem: artKey,
					Images:      images,
				},
				ItemCount: entry.Count,
			})
		}
		return frontend.FacetPersonPage{Items: items, Total: raw.Total, More: raw.More}, nil
	}

	canonFacet, ok := core.NormalizeCatalogueTabFacet(core.TabPeople, field)
	if !ok || canonFacet == "" {
		return frontend.FacetPersonPage{}, core.ErrNotFound
	}
	catReader, ok := s.d.Catalog.(catalogTabFacetReader)
	if !ok || catReader == nil {
		return frontend.FacetPersonPage{}, core.ErrNotFound
	}
	filter, none, err := s.catalogueFacetQueryFilter(ctx, v, lib, mt)
	if err != nil {
		return frontend.FacetPersonPage{}, err
	}
	if none {
		return frontend.FacetPersonPage{Items: []frontend.FacetPerson{}}, nil
	}
	raw, err := catReader.LibraryFacetPeople(ctx, core.CatalogFacetPeopleQuery{
		Filter: filter,
		Facet:  canonFacet,
		Order:  order,
		Query:  page.Query,
		Offset: page.Offset,
		Limit:  page.Limit,
	})
	if err != nil {
		return frontend.FacetPersonPage{}, err
	}
	items := make([]frontend.FacetPerson, 0, len(raw.Items))
	for _, entry := range raw.Items {
		if entry.PersonTMDB <= 0 || strings.TrimSpace(entry.Name) == "" || entry.ItemCount <= 0 {
			continue
		}
		items = append(items, frontend.FacetPerson{
			Person: frontend.PersonDetail{
				ID:          personFacetID(entry.PersonTMDB),
				Name:        entry.Name,
				TMDB:        entry.PersonTMDB,
				ArtworkItem: entry.SampleKey,
			},
			ItemCount: entry.ItemCount,
		})
	}
	return frontend.FacetPersonPage{Items: items, More: raw.More}, nil
}

// PluginLibraryCandidateRows builds the candidate shelves (20 items each) for a
// plugin library's Recommended tab before per-library layout policies run.
func (s *Service) PluginLibraryCandidateRows(ctx context.Context, viewer *core.Viewer, viewID, tab string) ([]frontend.Row, error) {
	ctx = core.WithCatalogViewer(ctx, viewer)
	v, err := s.viewer(ctx, viewer)
	if err != nil {
		return nil, err
	}
	rows, _, _, err := s.pluginLibraryCandidateRowsForTab(ctx, v, core.Device{}, viewID, tab)
	return rows, err
}

func pluginFirstRecommendedTabID(tabs []frontend.ViewTab) string {
	for _, t := range tabs {
		if t.Kind == core.TabRecommended {
			return t.ID
		}
	}
	return ""
}

type pluginContinueScope struct {
	allow         bool
	titleMedia    core.MediaClass
	allowedLevels map[string]bool
	allowedMedia  map[core.MediaClass]bool
}

func resolvePluginContinueScope(p PluginPresentation, activeShelves []PluginShelf) pluginContinueScope {
	levelMediaOf := func(lvl string) core.MediaClass {
		if lvl != "" && p.LevelMedia != nil {
			if m, ok := p.LevelMedia[lvl]; ok && m != "" {
				return m
			}
		}
		return p.Media
	}
	isDescendantOf := func(candidate, ancestor string) bool {
		if candidate == ancestor {
			return true
		}
		cur := candidate
		for step := 0; step <= len(p.LevelParents); step++ {
			parent := p.LevelParents[cur]
			if parent == "" {
				return false
			}
			if parent == ancestor {
				return true
			}
			cur = parent
		}
		return false
	}

	allowedLevels := map[string]bool{}
	allowedMedia := map[core.MediaClass]bool{}
	addSameMediaSubtree := func(root string, media core.MediaClass) {
		if media != "" {
			allowedMedia[media] = true
		}
		if root == "" {
			return
		}
		allowedLevels[root] = true
		for lv, lvMedia := range p.LevelMedia {
			if lvMedia == media && isDescendantOf(lv, root) {
				allowedLevels[lv] = true
			}
		}
	}

	if len(activeShelves) > 0 {
		for _, sh := range activeShelves {
			lvl := sh.Level
			if lvl == "" {
				lvl = p.RootLevel
			}
			addSameMediaSubtree(lvl, levelMediaOf(lvl))
		}
	} else {
		addSameMediaSubtree(p.RootLevel, levelMediaOf(p.RootLevel))
	}

	var nonMusicMedia []core.MediaClass
	for m := range allowedMedia {
		if m != core.MediaClassMusic {
			nonMusicMedia = append(nonMusicMedia, m)
		}
	}
	allow := len(nonMusicMedia) > 0 || (len(allowedMedia) == 0 && p.Media != core.MediaClassMusic)
	titleMedia := p.Media
	if len(nonMusicMedia) == 1 {
		titleMedia = nonMusicMedia[0]
	}
	return pluginContinueScope{
		allow:         allow,
		titleMedia:    titleMedia,
		allowedLevels: allowedLevels,
		allowedMedia:  allowedMedia,
	}
}

func (s *Service) pluginLibraryCandidateRowsForTab(ctx context.Context, v *core.User, dev core.Device, viewID, tabID string) ([]frontend.Row, bool, bool, error) {
	lib, pres, p, err := s.pluginLibraryByViewID(ctx, v, viewID)
	if err != nil {
		return nil, false, false, err
	}
	const (
		maxCandidates = 16
		rowLimit      = 20
	)
	tabID = strings.TrimSpace(tabID)
	firstRecTabID := pluginFirstRecommendedTabID(p.Tabs)
	isDefaultTab := tabID == "" || tabID == firstRecTabID
	targetTabID := tabID
	if targetTabID == "" {
		targetTabID = firstRecTabID
	}

	var activeShelves []PluginShelf
	fromTabShelves := false
	if targetTabID != "" && len(p.TabShelves[targetTabID]) > 0 {
		activeShelves = p.TabShelves[targetTabID]
		fromTabShelves = true
	} else if len(p.Shelves) > 0 {
		activeShelves = p.Shelves
	}
	declaredShelves := len(activeShelves) > 0

	// Preserve existing row IDs ("plugin:<viewID>:shelf:<key>") when serving
	// the default recommended tab from library.shelves; per-tab shelves and
	// additional recommended tabs use stable tab-scoped row IDs
	// ("plugin:<viewID>:tab:<tabId>:shelf:<key>").
	rowPrefix := "plugin:" + viewID + ":"
	if (!isDefaultTab || fromTabShelves) && targetTabID != "" {
		rowPrefix = "plugin:" + viewID + ":tab:" + targetTabID + ":"
	}

	var rows []frontend.Row
	appendRow := func(id, title string, purpose frontend.RowPurpose, layout, kind string, items []core.Title) {
		if len(items) == 0 || (!declaredShelves && len(rows) >= maxCandidates) {
			return
		}
		if len(items) > rowLimit {
			items = items[:rowLimit]
		}
		copied := make([]core.Title, len(items))
		copy(copied, items)
		entries := make([]frontend.RowEntry, len(copied))
		for i := range copied {
			entries[i] = frontend.RowEntry{Kind: frontend.RowEntryTitle, Title: &copied[i]}
		}
		row := frontend.Row{
			ID:      id,
			Title:   title,
			Purpose: purpose,
			Kind:    kind,
			Style:   "shelf",
			Entries: entries,
			Items:   copied,
		}
		// A shelf's declared look is its default; a page's look wins.
		row.Layout.Present.Layout = layout
		// Continue, Jump back in and Liked are this viewer's own titles.
		row.Layout.Present.Personal = purpose == frontend.RowContinue || purpose == core.PurposeRecentContexts || purpose == core.PurposeLiked
		rows = append(rows, row)
	}

	// 1. Continue shelf (scoped to this tab's shelf levels and media class;
	// music-only tabs and track-role items are excluded).
	contScope := resolvePluginContinueScope(p, activeShelves)
	if s.d.State != nil && contScope.allow {
		if sts, sErr := s.d.State.InProgress(ctx, v.ID, 200); sErr == nil && len(sts) > 0 {
			var keys []core.RatingKey
			for _, st := range sts {
				if (st.ViewOffset > 0 || (st.Reading != nil && st.Reading.Locator != "" && st.Reading.Fraction > 0 && st.Reading.Fraction < 0.98)) && st.Key.IsPluginKey() {
					keys = append(keys, st.Key)
				}
			}
			if len(keys) > 0 {
				if pivs, pErr := s.d.PluginLibraries.ItemsMany(ctx, keys); pErr == nil {
					var cont []core.Title
					for _, k := range keys {
						piv, ok := pivs[k]
						if !ok || piv.Library.ID != lib.ID || piv.SkipContinue || !s.pluginItemAllowed(ctx, v, piv) {
							continue
						}
						if len(contScope.allowedLevels) > 0 && piv.Title.KindLevel != "" && !contScope.allowedLevels[piv.Title.KindLevel] {
							continue
						}
						if len(contScope.allowedMedia) > 0 && piv.Title.MediaClass != "" && !contScope.allowedMedia[piv.Title.MediaClass] {
							continue
						}
						cont = append(cont, piv.Title)
						if len(cont) >= rowLimit {
							break
						}
					}
					appendRow(rowPrefix+"continue", pluginContinueTitle(contScope.titleMedia, dev.Language), frontend.RowContinue, "", "continue", cont)
				}
			}
		}
	}

	// The library's own rows every page can place (Recently added, New,
	// Playlists): one LIMIT query each, batched with the declared shelves.
	type candidateQuery struct {
		id, title, kind string
		purpose         frontend.RowPurpose
		layout          string
		q               frontend.ItemQuery
	}
	generic := []candidateQuery{
		{id: rowPrefix + "released", title: "Newest", kind: "recentlyReleased", purpose: frontend.RowRecentlyReleased, q: frontend.ItemQuery{Sort: core.SortReleased, Desc: true, Limit: rowLimit, NoTotal: true, SuppressExpand: true}},
		{id: rowPrefix + "added", title: "Recently Added", kind: "recentlyAdded", purpose: frontend.RowRecentlyAdded, q: frontend.ItemQuery{Sort: core.SortAdded, Desc: true, Limit: rowLimit, NoTotal: true, SuppressExpand: true}},
	}
	if lvl := p.roleLevel(core.RolePlaylist); lvl != "" {
		generic = append(generic, candidateQuery{id: rowPrefix + "playlists", title: "Playlists", kind: "playlists", purpose: frontend.RowRecommendation,
			q: frontend.ItemQuery{Level: lvl, Sort: core.SortAdded, Desc: true, Limit: rowLimit, NoTotal: true, SuppressExpand: true}})
	}

	// Plugin-declared shelves (tab-specific or library-level) replace the
	// auto-generated facet shelves and are not clamped to the auto
	// candidate cap.
	if declaredShelves {
		if v != nil && core.CatalogViewerID(ctx) == 0 {
			ctx = core.WithCatalogViewer(ctx, v)
		}
		queries := make([]candidateQuery, 0, len(activeShelves)+len(generic))
		for _, sh := range activeShelves {
			sortOrder := sh.Sort
			if sortOrder == "" {
				sortOrder = core.SortReleased
			}
			desc := sh.Direction != "asc"
			q := frontend.ItemQuery{
				Level:          sh.Level,
				Sort:           sortOrder,
				Desc:           desc,
				Limit:          rowLimit,
				NoTotal:        true,
				SuppressExpand: true,
			}
			if len(sh.Filters) > 0 {
				facets := make(map[string][]string, len(sh.Filters))
				for k, vals := range sh.Filters {
					facets[k] = append([]string(nil), vals...)
				}
				q.Filter.Facets = facets
			}
			if sh.TabFilter != nil && !sh.TabFilter.IsEmpty() {
				q.Filter.TabRules, q.Filter.TabFallback = sh.TabFilter.ToPolicy("")
			}
			// Declared shelves sorted by lastViewed are viewer-history shelves
			// (e.g. "Jump back in", "Recently played"): restrict to items with
			// viewer watch history (rolled up to sh.Level) so unplayed items
			// never fill the shelf and it hides cleanly when empty.
			if sortOrder.IsLastViewed() {
				q.Filter.TabRules, q.Filter.TabFallback = requireViewerHistoryPolicy(q.Filter.TabRules, q.Filter.TabFallback)
			}
			title := sh.Label
			if strings.TrimSpace(title) == "" {
				title = sh.Key
			}
			purpose := sh.Purpose
			if purpose == "" {
				purpose = frontend.RowRecommendation
			}
			queries = append(queries, candidateQuery{id: rowPrefix + "shelf:" + sh.Key, title: title, kind: "shelf", purpose: purpose, layout: sh.Layout, q: q})
		}
		// The library's own rows read first and show after the shelves.
		queries = append(append([]candidateQuery(nil), generic...), queries...)

		var batchPages []PluginBrowsePage
		batchSlot := make([]int, len(queries))
		for i := range batchSlot {
			batchSlot[i] = -1
		}
		if batcher, ok := s.d.PluginLibraries.(PluginBatchBrowseLibraries); ok && len(queries) > 1 {
			var batchQs []frontend.ItemQuery
			for idx, cq := range queries {
				if cq.q.Sort.IsLastViewed() {
					continue
				}
				if sqlQ, sqlOK := s.pluginBrowseQuery(ctx, v, lib, cq.q); sqlOK && sqlQ.Limit > 0 {
					batchSlot[idx] = len(batchQs)
					batchQs = append(batchQs, sqlQ)
				}
			}
			if len(batchQs) > 1 {
				if pages, bErr := batcher.BrowseBatch(ctx, lib, batchQs); bErr == nil && len(pages) == len(batchQs) {
					batchPages = pages
				}
			}
		}

		results := make([][]core.Title, len(queries))
		for idx, cq := range queries {
			if slot := batchSlot[idx]; slot >= 0 && batchPages != nil {
				results[idx] = batchPages[slot].Items
			} else if page, pErr := s.pluginLibraryItems(ctx, v, lib, cq.q); pErr == nil {
				results[idx] = page.Items
			}
		}
		for i := range queries {
			idx := (i + len(generic)) % len(queries) // shelves, then the library's own rows
			cq := queries[idx]
			appendRow(cq.id, cq.title, cq.purpose, cq.layout, cq.kind, results[idx])
			if n := len(rows); n > 0 && rows[n-1].ID == cq.id && cq.q.Sort.IsLastViewed() {
				// A viewer-history shelf (Recently played) is personal too.
				rows[n-1].Layout.Present.Personal = true
			}
		}
		if rows == nil {
			return []frontend.Row{}, true, isDefaultTab, nil
		}
		return rows, true, isDefaultTab, nil
	}

	// 2. Newest, Recently added and Playlists.
	for _, cq := range generic {
		if page, pErr := s.pluginLibraryItems(ctx, v, lib, cq.q); pErr == nil {
			appendRow(cq.id, cq.title, cq.purpose, cq.layout, cq.kind, page.Items)
		}
	}

	// 4. Top rated (when rating is a declared sort).
	if slices.Contains(p.Sorts, "rating") && len(rows) < maxCandidates {
		if page, pErr := s.pluginLibraryItems(ctx, v, lib, frontend.ItemQuery{
			Sort:    core.SortRating,
			Desc:    true,
			Limit:   rowLimit,
			NoTotal: true,
		}); pErr == nil {
			appendRow(rowPrefix+"rating", "Top Rated", frontend.RowRecommendation, "", "topRated", page.Items)
		}
	}

	// 5. Facet shelves: a few from the first declared facet, then a few from
	// the next non-people string facet (such as genres or tags). Buckets come
	// from the precomputed facet tallies behind FacetBuckets; each facet's
	// largest buckets form a small pool and the day's shelves are picked from
	// that pool by a per-library daily seed (see dailyFacetPool), so the
	// shelves rotate daily and equal-size buckets never fall back to A-Z.
	// Probing stays bounded per facet so viewer restrictions never walk
	// hundreds of buckets.
	if pres != nil && len(p.Facets) > 0 && len(rows) < maxCandidates {
		now := time.Now()
		if s.d.Now != nil {
			now = s.d.Now()
		}
		day := now.Local().Format("2006-01-02")
		shelfFacets := []frontend.ViewFacet{p.Facets[0]}
		if facet, ok := nextNonPeopleStringFacet(p.Facets); ok {
			shelfFacets = append(shelfFacets, facet)
		}
		for _, facet := range shelfFacets {
			if len(rows) >= maxCandidates {
				break
			}
			buckets, bErr := pres.FacetBuckets(ctx, lib, facet.ID)
			if bErr != nil || len(buckets) == 0 {
				continue
			}
			pool := dailyFacetPool(buckets, lib.ID, facet.ID, day, pluginFacetPoolSize)
			added := 0
			for tried, b := range pool {
				if len(rows) >= maxCandidates || added >= pluginFacetShelves || tried >= pluginFacetProbes {
					break
				}
				page, pErr := s.pluginLibraryItems(ctx, v, lib, frontend.ItemQuery{
					Sort:           core.SortReleased,
					Desc:           true,
					Limit:          rowLimit,
					NoTotal:        true,
					SuppressExpand: true,
					Filter: core.Filter{
						Facets: map[string][]string{facet.ID: {b.ID}},
					},
				})
				if pErr != nil || len(page.Items) == 0 {
					continue
				}
				title := b.Label
				if isOriginFacetKey(facet.ID) {
					title = "From " + b.Label
				}
				before := len(rows)
				appendRow(rowPrefix+"facet:"+facet.ID+":"+b.ID, title, frontend.RowRecommendation, "", "facet", page.Items)
				if len(rows) > before {
					added++
				}
			}
		}
	}
	if rows == nil {
		return []frontend.Row{}, false, isDefaultTab, nil
	}
	return rows, false, isDefaultTab, nil
}

// PluginLibraryRows implements frontend.PluginLibraryRowsReader: the plugin
// library's candidate shelves for tab, arranged by the Recommendations page
// "library:<id>/<tab>" exactly like a catalogue library's page (Curated or
// Personalized, length, devices, Look), with the page's Hide watched
// applied. tab "" is the first rows tab.
func (s *Service) PluginLibraryRows(ctx context.Context, viewer *core.Viewer, dev core.Device, viewID, tab string) ([]frontend.Row, error) {
	ctx = core.WithCatalogViewer(ctx, viewer)
	v, err := s.viewer(ctx, viewer)
	if err != nil {
		return nil, err
	}
	rows, declaredShelves, isDefaultTab, err := s.pluginLibraryCandidateRowsForTab(ctx, v, dev, viewID, tab)
	if err != nil {
		return nil, err
	}
	if s.d.Layout == nil {
		return rows, nil
	}
	if isDefaultTab {
		tab = ""
	}
	id, _ := strconv.Atoi(viewID)
	now := time.Now()
	if s.d.Now != nil {
		now = s.d.Now()
	}
	hc := core.NewHubContext(now.Local(), dev.Product, dev.Platform, dev.DeviceName)
	req := frontend.RowRequest{
		Viewer:    v,
		Device:    dev,
		Placement: frontend.PlacementLibrary,
		ViewID:    viewID,
		Tab:       strings.TrimSpace(tab),
	}
	sit := core.HomeSituation{
		LibraryID: id,
		Device:    hc.Device,
		Daypart:   hc.Daypart,
		Weekend:   hc.Weekend,
		Now:       hc.Now,
		Product:   dev.Product,
		Platform:  dev.Platform,
		ClientID:  dev.ID,
	}
	// The page's own length applies; an unsaved page shows its declared
	// shelves (Curated) or up to eight automatic rows.
	sit.DefaultMaxRows = 8
	if declaredShelves && len(rows) > 0 {
		sit.DefaultMaxRows = len(rows)
	}
	arranged, err := LayoutRows(ctx, s.d.Layout, nil, req, sit, rows)
	if err != nil {
		return nil, err
	}
	return DropWatched(ctx, v, arranged, s.titlesWatched), nil
}

const (
	// pluginFacetPoolSize caps how many of a facet's largest buckets are
	// eligible for a day's Recommended shelves.
	pluginFacetPoolSize = 24
	// pluginFacetShelves caps the shelves taken from one facet.
	pluginFacetShelves = 4
	// pluginFacetProbes caps bucket probes per facet, so buckets emptied by
	// viewer restrictions are skipped without walking the whole pool.
	pluginFacetProbes = 12
)

// dailyFacetPool returns up to poolSize of the largest buckets in the order
// they should be probed for shelves on day (a "2006-01-02" date). The pool is
// formed by bucket count, with a hash of (library, day, facet, bucket)
// breaking count ties, and the pool itself is ordered by that same hash. The
// result is stable within a day, differs between libraries, rotates daily,
// and never depends on bucket labels, so many equal-size buckets (such as
// developers with similar catalogue sizes) are not always won by the labels
// that sort first.
func dailyFacetPool(buckets []frontend.FacetBucket, libID core.LibraryID, facetID, day string, poolSize int) []frontend.FacetBucket {
	if len(buckets) == 0 || poolSize <= 0 {
		return nil
	}
	type keyed struct {
		bucket frontend.FacetBucket
		key    string
	}
	keyedBuckets := make([]keyed, len(buckets))
	for i, b := range buckets {
		id := b.ID
		if id == "" {
			id = b.Label
		}
		sum := sha256.Sum256([]byte(fmt.Sprintf("%d|%s|%s|%s", int64(libID), day, facetID, id)))
		keyedBuckets[i] = keyed{bucket: b, key: hex.EncodeToString(sum[:])}
	}
	sort.SliceStable(keyedBuckets, func(i, j int) bool {
		if keyedBuckets[i].bucket.Count != keyedBuckets[j].bucket.Count {
			return keyedBuckets[i].bucket.Count > keyedBuckets[j].bucket.Count
		}
		return keyedBuckets[i].key < keyedBuckets[j].key
	})
	if len(keyedBuckets) > poolSize {
		keyedBuckets = keyedBuckets[:poolSize]
	}
	sort.SliceStable(keyedBuckets, func(i, j int) bool {
		return keyedBuckets[i].key < keyedBuckets[j].key
	})
	out := make([]frontend.FacetBucket, len(keyedBuckets))
	for i, k := range keyedBuckets {
		out[i] = k.bucket
	}
	return out
}

// nextNonPeopleStringFacet returns the first facet after facets[0] whose type
// is a string list (genres, tags and similar), skipping people and
// numeric/date facets whose buckets would not read as shelves.
func nextNonPeopleStringFacet(facets []frontend.ViewFacet) (frontend.ViewFacet, bool) {
	if len(facets) < 2 {
		return frontend.ViewFacet{}, false
	}
	for _, facet := range facets[1:] {
		if facet.Type == "strings" || facet.Type == "string" {
			return facet, true
		}
	}
	return frontend.ViewFacet{}, false
}

func isOriginFacetKey(key string) bool {
	switch strings.ToLower(strings.TrimSpace(key)) {
	case "developer", "developers", "publisher", "publishers", "channel", "channels", "network", "networks", "author", "authors":
		return true
	default:
		return false
	}
}

func rulesHaveViewerCondition(rules []core.LibraryExclusion) bool {
	for _, g := range rules {
		for _, r := range g.Rules {
			if r.Field == "viewer" {
				return true
			}
		}
	}
	return false
}

// requireViewerHistoryPolicy ensures a declared sort:lastViewed shelf only
// returns items with viewer watch history (rolled up across container levels),
// unless the shelf's tab filter already specifies a viewer condition.
func requireViewerHistoryPolicy(rules []core.LibraryExclusion, fallback string) ([]core.LibraryExclusion, string) {
	if rulesHaveViewerCondition(rules) {
		return rules, fallback
	}
	historyCond := core.CollectionRule{
		Field: "viewer",
		Op:    "is",
		Value: json.RawMessage(`{"history":true}`),
	}
	if len(rules) == 0 && fallback == "" {
		return []core.LibraryExclusion{{
			Action:    "show",
			MediaType: "mixed",
			Match:     "all",
			Rules:     []core.CollectionRule{historyCond},
		}}, "hide"
	}
	if len(rules) == 1 && rules[0].Action == "show" && rules[0].Match != "any" && fallback == "hide" {
		g := rules[0]
		g.Rules = append(append([]core.CollectionRule(nil), g.Rules...), historyCond)
		return []core.LibraryExclusion{g}, "hide"
	}
	notHistory := core.LibraryExclusion{
		Action:    "hide",
		MediaType: "mixed",
		Match:     "all",
		Rules: []core.CollectionRule{{
			Field: "viewer",
			Op:    "isNot",
			Value: json.RawMessage(`{"history":true}`),
		}},
	}
	out := make([]core.LibraryExclusion, 0, len(rules)+1)
	out = append(out, notHistory)
	out = append(out, rules...)
	return out, fallback
}
