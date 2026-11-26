package personalize

import (
	"strconv"
	"strings"

	"github.com/braedonsaunders/ether/internal/core"
)

// Row sources that are not engine generators: rows the front ends build
// (Continue Watching on a library page, Recently Added, Live), owner
// collections, plugin shelves, and chart rows built only for a page.
const (
	SourceCollectionCards = "collection-cards"
	SourceCollection      = "collection" // params: collection (public id)
	SourceShelf           = "shelf"      // params: shelf (a plugin library's declared shelf key)
	SourceSurface         = "surface"    // params: surface ("<pluginID>:<surfaceID>"); a stub Vela draws
	// KindLibraryPlaylists is a library's own playlists: the items of its
	// playlist level (listen libraries).
	KindLibraryPlaylists = "library-playlists"
	KindUpcoming         = "upcoming"
	KindLive             = "live"
	KindWatchlist        = "watchlist"
	KindRediscover       = "rediscover"
	KindNewlyAvail       = "newly-available"
	KindYearReview       = "year-in-review"
	kindPluginFacet      = "plugin-facet" // a plugin library's automatic facet rows (Ether's picks only)
)

// Source picker groups (core.RowSourceInfo.Group).
const (
	groupKeep            = "keep"
	groupTrending        = "trending"
	groupNew             = "new"
	groupForYou          = "foryou"
	groupTop             = "top"
	groupBrowse          = "browse"
	groupYourCollections = "collections"
	groupPlugins         = "plugins"
	groupLive            = "live"
)

var (
	paramGenre  = core.RowParam{Key: "genre", Label: "Genre", Type: core.ParamFacetValue, Facet: "genre"}
	paramWindow = core.RowParam{Key: "window", Label: "Window", Type: core.ParamChoice, Default: "week",
		Choices: []core.ParamOption{{Value: "day", Label: "Today"}, {Value: "week", Label: "This week"}}}
	paramAiring = core.RowParam{Key: "window", Label: "Window", Type: core.ParamChoice, Default: "day",
		Choices: []core.ParamOption{{Value: "day", Label: "Today"}, {Value: "week", Label: "This week"}}}
)

func paramCount(def int) core.RowParam {
	return core.RowParam{Key: "count", Label: "How many rows", Type: core.ParamCount, Min: 1, Max: 3, Default: strconv.Itoa(def)}
}

// sourceTitles are the designer's plain names for engine kinds whose row
// titles are long or viewer-specific.
var sourceTitles = map[string]string{
	KindContinue:      "Continue",
	KindTop10:         "Top 10 for You",
	KindTop10Today:    "Top 10 Today",
	KindShowcase:      "Acclaimed in a Genre",
	KindBecause:       "Because You Watched",
	KindPerson:        "More from a Person",
	KindTrending:      "Trending",
	KindNewReleases:   "New",
	KindTopRated:      "Top Rated",
	KindAiringToday:   "Airing",
	KindRecentlyAdded: "Recently Added",
	KindPlaylists:     "Your Playlists",
	KindTasteCluster:  "Taste Clusters",
	KindEra:           "Best of a Decade",
	KindWorld:         "Cinema in a Language",
	KindTopGenre:      "Top Rated in a Genre",
	KindBridge:        "Where Two Favourites Meet",
	KindStudio:        "More from a Studio",
	KindNetwork:       "More from a Network",
	KindPopularServer: "Popular on This Server",
}

// sourceGroups place engine kinds in the picker.
var sourceGroups = map[string]string{
	KindContinue: groupKeep, KindUpNext: groupKeep, KindFavorites: groupKeep, KindTraktWatch: groupKeep, KindPlaylists: groupKeep,
	KindTrending: groupTrending, KindPopular: groupTrending, KindTop10Today: groupTrending, KindPopularServer: groupTrending,
	KindNewReleases: groupNew, KindRecentlyAdded: groupNew, KindAiringToday: groupNew, KindNewSeasons: groupNew,
	KindTopRated: groupTop, KindAcclaimed: groupTop, KindTopGenre: groupTop,
	KindShowcase: groupBrowse, KindPerson: groupBrowse, KindStudio: groupBrowse, KindNetwork: groupBrowse, KindEra: groupBrowse, KindWorld: groupBrowse,
}

// chartKinds draw on TMDB charts and can be one row per library.
var chartKinds = map[string]bool{KindTrending: true, KindPopular: true, KindTopRated: true, KindNewReleases: true, KindUpcoming: true}

func builtinSources() []core.RowSourceInfo {
	var out []core.RowSourceInfo
	for _, k := range kinds {
		out = append(out, sourceForKind(kindByName[k.kind]))
	}
	extra := []core.RowSourceInfo{
		{Kind: KindUpcoming, Title: "Upcoming", Description: "Films coming soon to cinemas and streaming, a few months ahead.",
			Group: groupNew, Needs: []string{core.CapCharts, core.CapMovies}, Split: true, Params: []core.RowParam{paramGenre}},
		{Kind: KindLive, Title: "Live Now", Description: "What's on your Live TV channels right now.",
			Group: groupLive, HomeOnly: true, Needs: []string{core.CapLive}, Purpose: core.PurposeLive},
		{Kind: KindWatchlist, Title: "Watchlist", Description: "Titles on the viewer’s watchlist.",
			Group: groupKeep, Personal: true, HomeOnly: true,
			Params: []core.RowParam{{Key: core.WatchlistSplitParam, Label: "Split by availability", Type: core.ParamBool, Default: "false"}}},
		{Kind: KindNewlyAvail, Title: "Newly Available", Description: "Titles that just became playable here.",
			Group: groupNew, HomeOnly: true},
		{Kind: KindRediscover, Title: "Rediscover", Description: "Favourites the viewer hasn't returned to in a long while.",
			Group: groupForYou, Personal: true, HomeOnly: true},
		{Kind: KindYearReview, Title: "Year in Review", Description: "The viewer's year in titles.",
			Group: groupForYou, Personal: true, HomeOnly: true},
		{Kind: SourceCollection, Title: "Collection", Description: "One of your collections.",
			Group: groupYourCollections, Needs: []string{core.CapCatalogue}, Split: true,
			Params: []core.RowParam{{Key: "collection", Label: "Collection", Type: core.ParamRef, Required: true}}},
		{Kind: SourceCollectionCards, Title: "Collections", Description: "Show collections as clickable cards.",
			Group: groupYourCollections, HomeOnly: true, Purpose: core.RowPurpose("collection"),
			Look: core.RowLook{Card: "poster"},
			Params: []core.RowParam{
				{Key: "collections", Label: "Collections", Type: core.ParamCollectionList, Max: core.MaxCollectionCards},
				{Key: "match", Label: "Collection name starts with", Type: core.ParamText, Max: 100},
			}},
		{Kind: SourceSurface, Title: "Plugin Row", Description: "A row a plugin draws in Vela (Sports Tonight). Only in Vela.",
			Group: groupPlugins, HomeOnly: true, Apps: []string{"vela"},
			Params: []core.RowParam{{Key: "surface", Label: "Row", Type: core.ParamRef, Required: true}}},
		{Kind: SourceShelf, Title: "Plugin Shelf", Description: "A shelf this library's plugin declares.",
			Group: groupPlugins, Needs: []string{core.CapShelves},
			Params: []core.RowParam{{Key: "shelf", Label: "Shelf", Type: core.ParamRef, Required: true}}},
		{Kind: KindLibraryPlaylists, Title: "Playlists", Description: "The library's playlists.",
			Group: groupKeep, Needs: []string{core.CapRole(core.RolePlaylist)}, Split: true,
			Look: core.RowLook{Card: "square"}},
	}
	return append(out, extra...)
}

func sourceForKind(k kindInfo) core.RowSourceInfo {
	title := sourceTitles[k.kind]
	if title == "" {
		title = k.title
	}
	group := sourceGroups[k.kind]
	if group == "" {
		group = groupForYou
	}
	info := core.RowSourceInfo{
		Kind: k.kind, Title: title, Description: plainDescription(k.description), Group: group,
		Personal: personalBadge(k.kind), Needs: kindNeeds(k), Split: chartKinds[k.kind] || k.kind == KindPopularServer,
	}
	switch k.kind {
	case KindTrending:
		info.Params = []core.RowParam{paramWindow, paramGenre}
	case KindPopular, KindTopRated, KindNewReleases:
		info.Params = []core.RowParam{paramGenre}
	case KindAiringToday:
		info.Params = []core.RowParam{paramAiring}
	case KindContinue:
		info.Purpose = core.PurposeContinue
	case KindUpNext:
		info.Purpose = core.PurposeNextUp
	case KindTop10, KindTop10Today:
		on := true
		info.Look.Numbered = &on
	}
	if k.dynamic && k.cap > 1 {
		info.Params = append(info.Params, paramCount(k.cap))
	}
	switch k.kind {
	case KindContinue:
		info.Needs = []string{core.CapProgress}
	case KindRecentlyAdded:
		info.Needs = []string{core.CapAdded}
	case KindNewReleases:
		// The newest released titles of any library, plugin libraries too.
		info.Needs = []string{core.CapReleased}
	}
	return info
}

// kindNeeds derives the library capabilities an engine kind draws on.
func kindNeeds(k kindInfo) []string {
	var needs []string
	if chartKinds[k.kind] || k.kind == KindTop10Today || k.kind == KindAiringToday {
		needs = append(needs, core.CapCharts)
	} else if k.source == core.RowSourceEngine && k.kind != KindPopularServer {
		needs = append(needs, core.CapTaste)
	}
	if k.kind == KindPopularServer {
		needs = append(needs, core.CapPopularity)
	}
	fits := kindFits(k)
	movies, shows := false, false
	for _, f := range fits {
		movies = movies || f == surfMovies
		shows = shows || f == surfShows
	}
	switch {
	case movies && !shows:
		needs = append(needs, core.CapMovies)
	case shows && !movies:
		needs = append(needs, core.CapEpisodes)
	}
	return needs
}

// plainDescription drops designer-internal wording from kind descriptions.
func plainDescription(s string) string {
	for _, cut := range []string{" Pin, schedule or hide this row with Home policies.", " Off by default; add it to a page to use it.", " Off by default: it is usually one or two titles. Add it to a page to use it."} {
		s = strings.ReplaceAll(s, cut, "")
	}
	return s
}

// sourceOf names the page source a served row comes from: its source
// kind and the parameter that tells rows of that kind apart, when any
// (a collection's id, a shelf's key). Rows built for a page carry their
// row id instead (pageRowID).
func sourceOf(hubID, kind string) (source, param string) {
	switch {
	case strings.HasPrefix(kind, KindCollectionPrefix):
		return SourceCollection, strings.TrimPrefix(kind, KindCollectionPrefix)
	case strings.HasPrefix(hubID, "plugin:") && strings.Contains(hubID, ":shelf:"):
		return SourceShelf, hubID[strings.LastIndex(hubID, ":shelf:")+len(":shelf:"):]
	case strings.HasPrefix(hubID, "plugin:") && strings.Count(hubID, ":") >= 2:
		// A plugin library's automatic rows: plugin:<view>:<row>.
		rest := hubID[strings.Index(hubID[len("plugin:"):], ":")+len("plugin:")+1:]
		if tab, ok := strings.CutPrefix(rest, "tab:"); ok {
			// A rows tab's own rows: plugin:<view>:tab:<tab>:<row>.
			_, rest, _ = strings.Cut(tab, ":")
		}
		switch {
		case rest == "continue":
			return KindContinue, ""
		case rest == "playlists":
			return KindLibraryPlaylists, ""
		case rest == "added":
			return KindRecentlyAdded, ""
		case rest == "released":
			return KindNewReleases, ""
		case rest == "rating":
			return KindTopRated, ""
		default:
			return kindPluginFacet, ""
		}
	case strings.HasPrefix(hubID, "livetv.") || hubID == "live":
		return KindLive, ""
	case strings.HasPrefix(hubID, "watchlist."):
		return KindWatchlist, ""
	case strings.HasPrefix(hubID, "availability."):
		return KindNewlyAvail, ""
	case hubID == "discovery.rediscover":
		return KindRediscover, ""
	case strings.HasPrefix(hubID, "discovery.review."):
		return KindYearReview, ""
	}
	return kind, ""
}

// sourceInfo describes a page row source: a built-in, or a runtime source
// registered with the engine (a plugin list).
func (e *Engine) sourceInfo(kind string) (core.RowSourceInfo, bool) {
	return e.rowKindRegistry().Source(kind)
}

// SourceInfos lists every row source: the built-ins, then runtime sources.
func (e *Engine) SourceInfos() []core.RowSourceInfo { return e.rowKindRegistry().Sources() }
