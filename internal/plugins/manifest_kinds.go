package plugins

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/libraryfilter"
)

var (
	kindKeyRe      = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)
	kindFieldRe    = regexp.MustCompile(`^[a-z][a-zA-Z0-9_]{0,39}$`)
	kindArtRe      = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)
	kindShelfKeyRe = regexp.MustCompile(`^[a-z][a-z0-9-]{0,39}$`)
)

// Envelope keys a declared field may not shadow (spec §2.1).
var kindEnvelopeKeys = map[string]bool{
	"id": true, "title": true, "sortTitle": true, "year": true,
	"released": true, "summary": true, "rating": true, "index": true,
	"parentId": true, "level": true,
}

var kindFieldTypes = map[string]bool{
	"string": true, "int": true, "float": true, "bool": true, "date": true,
	"duration": true, "enum": true, "strings": true, "people": true,
}

var kindSorts = map[string]bool{
	"title": true, "added": true, "released": true, "year": true, "rating": true, "index": true, "lastViewed": true,
}

// importSiteName is a playlist link site a kind reads ("deezer").
var importSiteName = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)

var kindLibraryCards = map[string]bool{
	"poster": true, "landscape": true, "square": true,
}

var kindLibraryDateDisplays = map[string]bool{
	"year": true, "date": true,
}

var kindLibraryTabKinds = map[string]bool{
	"recommended": true, "library": true, "collections": true, "people": true, "view": true, "live": true,
}

var kindLibraryTabLayouts = map[string]bool{
	"grid": true, "list": true, "timeline": true,
}

// KindRuleFields maps built-in rule facts (genre, studio, runtime, ...) onto
// declared, indexed plugin field keys. Shared by manifest validation and
// pluginitems SQL compilation so both agree on which built-in facts exist.
func KindRuleFields(fields []KindField) map[string]string {
	out := map[string]string{}
	aliases := map[string][]string{
		"genre":      {"genre", "genres", "tags", "tag"},
		"studio":     {"studio", "studios"},
		"country":    {"country", "countries"},
		"keyword":    {"keyword", "keywords"},
		"language":   {"language"},
		"runtime":    {"runtime", "duration", "length"},
		"votes":      {"votes"},
		"popularity": {"popularity"},
	}
	for field, keys := range aliases {
		wantNumeric := field == "runtime" || field == "votes" || field == "popularity"
		for _, key := range keys {
			for _, f := range fields {
				text := f.Type == "string" || f.Type == "strings" || f.Type == "enum" || f.Type == "people"
				numeric := f.Type == "int" || f.Type == "float" || f.Type == "duration"
				if f.Key == key && f.Indexed && ((wantNumeric && numeric) || (!wantNumeric && text)) {
					out[field] = key
					break
				}
			}
			if out[field] != "" {
				break
			}
		}
	}
	return out
}

var mediaBrowserCollectionTypes = map[string]bool{
	"movies": true, "tvshows": true, "music": true, "audiobooks": true,
	"books": true, "photos": true, "homevideos": true,
}

var mediaBrowserItemTypes = map[string]bool{
	"Movie": true, "Series": true, "Season": true, "Episode": true, "Video": true,
	"MusicArtist": true, "MusicAlbum": true, "Audio": true, "AudioBook": true,
	"Book": true, "PhotoAlbum": true, "Photo": true,
}

// mediaBrowserItemTypeList names the closed itemTypes set for error messages.
const mediaBrowserItemTypeList = "Movie, Series, Season, Episode, Video, MusicArtist, MusicAlbum, Audio, AudioBook, Book, PhotoAlbum or Photo"

func validateMediaKinds(m Manifest, add func(string, string), label func(string, string, int)) {
	e := m.Ether
	kinds := e.Contributes.MediaKinds
	hasProvider := m.HasCapability(CapCatalogProvider)
	hasResolver := m.HasCapability(CapKindResolver)
	if e.Kind == KindContent && !hasProvider && !hasResolver {
		add("ether.capabilities", "a content plugin needs catalog_provider or kind_resolver")
	}
	if (hasProvider || hasResolver) && len(kinds) == 0 {
		add("ether.contributes.mediaKinds", "is required for catalog_provider and kind_resolver")
	}
	if !hasProvider && !hasResolver && len(kinds) > 0 {
		add("ether.contributes.mediaKinds", "needs the catalog_provider or kind_resolver capability")
	}
	seen := map[string]bool{}
	for i, k := range kinds {
		base := fmt.Sprintf("ether.contributes.mediaKinds[%d]", i)
		validateMediaKind(m, k, base, hasProvider, add, label)
		if seen[k.Key] {
			add(base+".key", "is listed twice")
		}
		seen[k.Key] = true
	}
}

func validateMediaKind(m Manifest, k MediaKindMeta, base string, hasProvider bool, add func(string, string), label func(string, string, int)) {
	if !kindKeyRe.MatchString(k.Key) {
		add(base+".key", "must be lower case letters, digits and dashes, at most 32 characters")
	}
	label(base+".label", k.Label, 60)
	if k.Media != "" && !core.ValidMediaClass(k.Media) {
		add(base+".media", "must be video, music, audiobook, book, podcast or photo")
	}
	if !k.Library.IsZero() {
		label(base+".library.name", k.Library.Name, 60)
		if k.Library.Icon != "" && !ValidAdapterIconPath(k.Library.Icon) {
			add(base+".library.icon", "must be a package-relative SVG or PNG path")
		}
		if k.Library.Card != "" && !kindLibraryCards[k.Library.Card] {
			add(base+".library.card", "must be poster, landscape or square")
		}
		if k.Library.DateDisplay != "" && !kindLibraryDateDisplays[k.Library.DateDisplay] {
			add(base+".library.dateDisplay", "must be year or date")
		}
		if k.Import != nil {
			if len(k.Import.Links) > 8 {
				add(base+".import.links", "at most 8 sites may be named")
			}
			for i, site := range k.Import.Links {
				if !importSiteName.MatchString(site) {
					add(fmt.Sprintf("%s.import.links[%d]", base, i), "must be a short lowercase site name such as deezer")
				}
			}
			if len(k.Import.Links) > 0 && !k.Import.Identify {
				add(base+".import.identify", "link imports need identify")
			}
		}
		if k.Library.Experience != "" && !core.ValidLibraryExperience(k.Library.Experience) {
			add(base+".library.experience", "must be an experience id such as watch, listen, read, gallery or live")
		}
		if len(k.Library.Tabs) > MaxKindLibraryTabs {
			add(base+".library.tabs", fmt.Sprintf("at most %d tabs may be declared", MaxKindLibraryTabs))
		}
		seenTab := map[string]bool{}
		for j, tab := range k.Library.Tabs {
			tbase := fmt.Sprintf("%s.library.tabs[%d]", base, j)
			switch {
			case !kindKeyRe.MatchString(tab.Key):
				add(tbase+".key", "must be lower case letters, digits and dashes, at most 32 characters")
			case seenTab[tab.Key]:
				add(tbase+".key", "is listed twice")
			}
			seenTab[tab.Key] = true
			label(tbase+".label", tab.Label, 40)
			if !kindLibraryTabKinds[tab.Kind] {
				add(tbase+".kind", "must be recommended, library, collections, people, view or live")
			}
		}
	}

	if len(k.Levels) < 1 || len(k.Levels) > 5 {
		add(base+".levels", "must declare one to five levels")
	}
	levelKeys := map[string]bool{}
	playable := false
	for j, lv := range k.Levels {
		lbase := fmt.Sprintf("%s.levels[%d]", base, j)
		if !kindKeyRe.MatchString(lv.Key) {
			add(lbase+".key", "must be lower case letters, digits and dashes, at most 32 characters")
		}
		if levelKeys[lv.Key] {
			add(lbase+".key", "is listed twice")
		}
		if lv.Members {
			switch {
			case j == 0:
				add(lbase+".members", "the root level cannot be a list level")
			case lv.Parent != "":
				add(lbase+".members", "a list level has no parent")
			case lv.Playable:
				add(lbase+".members", "a list level cannot be playable")
			}
		}
		if lv.Parent != "" && !lv.Members {
			switch {
			case j == 0:
				add(lbase+".parent", "the root level cannot set parent")
			case lv.Parent == lv.Key:
				add(lbase+".parent", "cannot reference itself (cycle)")
			case !levelKeys[lv.Parent]:
				add(lbase+".parent", "must name an earlier level")
			}
		}
		levelKeys[lv.Key] = true
		label(lbase+".label", lv.Label, 40)
		if lv.Media != "" && !core.ValidMediaClass(lv.Media) {
			add(lbase+".media", "must be video, music, audiobook, book, podcast or photo")
		}
		if lv.Role != "" && !validKindLevelRole(lv.Role) {
			add(lbase+".role", "must be trimmed printable text of at most 40 characters")
		}
		if lv.Playable {
			playable = true
		}
	}
	if len(k.Levels) > 0 && !playable {
		add(base+".levels", "at least one level must be playable")
	}
	for j, lv := range k.Levels {
		if lv.Parent != "" {
			if parent, _, ok := k.Level(lv.Parent); ok && parent.Members {
				add(fmt.Sprintf("%s.levels[%d].parent", base, j), "a list level cannot be a parent (its children are its members)")
			}
		}
	}

	fields := map[string]KindField{}
	for j, f := range k.Fields {
		fbase := fmt.Sprintf("%s.fields[%d]", base, j)
		switch {
		case !kindFieldRe.MatchString(f.Key):
			add(fbase+".key", "must start with a lower case letter and use letters, digits and underscores, at most 40 characters")
		case kindEnvelopeKeys[f.Key]:
			add(fbase+".key", "collides with an item envelope key")
		case fields[f.Key].Key != "":
			add(fbase+".key", "is listed twice")
		}
		fields[f.Key] = f
		if !kindFieldTypes[f.Type] {
			add(fbase+".type", "must be string, int, float, bool, date, duration, enum, strings or people")
		}
		label(fbase+".label", f.Label, 40)
		if f.Type == "enum" {
			if len(f.Enum) == 0 {
				add(fbase+".enum", "an enum field needs at least one value")
			}
			seenEnum := map[string]bool{}
			for _, v := range f.Enum {
				if strings.TrimSpace(v) == "" || seenEnum[v] {
					add(fbase+".enum", "values must be non-empty and listed once")
					break
				}
				seenEnum[v] = true
			}
		} else if len(f.Enum) > 0 {
			add(fbase+".enum", "only enum fields list values")
		}
		if len(f.Values) > 0 {
			if f.Type != "strings" {
				add(fbase+".values", "only strings fields may declare values")
			} else {
				seenValues := map[string]bool{}
				for _, v := range f.Values {
					if strings.TrimSpace(v) == "" || seenValues[v] {
						add(fbase+".values", "values must be non-empty and listed once")
						break
					}
					seenValues[v] = true
				}
			}
		}
	}

	seenArt := map[string]bool{}
	for j, a := range k.Art {
		abase := fmt.Sprintf("%s.art[%d]", base, j)
		switch {
		case !kindArtRe.MatchString(a.Role):
			add(abase+".role", "must be lower case letters, digits and dashes, at most 32 characters")
		case BuiltinKindArtRoles[a.Role]:
			add(abase+".role", "is a built-in role and needs no declaration")
		case seenArt[a.Role]:
			add(abase+".role", "is listed twice")
		}
		seenArt[a.Role] = true
		label(abase+".label", a.Label, 40)
	}

	validatePresentations(m, k, base+".present", levelKeys, add)

	seenSort := map[string]bool{}
	for j, s := range k.Browse.Sorts {
		if !kindSorts[s] || seenSort[s] {
			add(fmt.Sprintf("%s.browse.sorts[%d]", base, j), "must be title, added, released, year, rating, index or lastViewed, listed once")
		}
		seenSort[s] = true
	}
	seenFacet := map[string]bool{}
	for j, f := range k.Browse.Facets {
		if seenFacet[f] {
			add(fmt.Sprintf("%s.browse.facets[%d]", base, j), "is listed twice")
		}
		seenFacet[f] = true
		if f == "year" || f == "released" || f == "rating" {
			continue
		}
		field, ok := fields[f]
		if !ok {
			add(fmt.Sprintf("%s.browse.facets[%d]", base, j), "must name a declared field")
		} else if !field.Indexed && field.Type != "people" {
			add(fmt.Sprintf("%s.browse.facets[%d]", base, j), "a facet field must be indexed")
		}
	}
	if k.Browse.Expand != nil {
		ebase := base + ".browse.expand"
		if !hasProvider {
			add(ebase, "expand needs the catalog_provider capability")
		}
		if len(k.Browse.Expand.Facets) == 0 && len(k.Browse.Expand.Levels) == 0 {
			add(ebase, "must list at least one browse facet or container level")
		}
		seenExpand := map[string]bool{}
		for j, f := range k.Browse.Expand.Facets {
			fbase := fmt.Sprintf("%s.facets[%d]", ebase, j)
			if seenExpand[f] {
				add(fbase, "is listed twice")
			}
			seenExpand[f] = true
			if !seenFacet[f] {
				add(fbase, "must name a declared browse facet")
			}
		}
		seenExpandLevel := map[string]bool{}
		for j, lvKey := range k.Browse.Expand.Levels {
			lbase := fmt.Sprintf("%s.levels[%d]", ebase, j)
			if seenExpandLevel[lvKey] {
				add(lbase, "is listed twice")
			}
			seenExpandLevel[lvKey] = true
			lv, _, ok := k.Level(lvKey)
			if !ok {
				add(lbase, "must name a declared level")
			} else if !lv.Container {
				add(lbase, "must name a level with container: true")
			}
		}
		if k.Browse.Expand.TTLHours < 0 {
			add(ebase+".ttlHours", "must be positive")
		}
	}
	if !k.Library.IsZero() {
		declaredIndexed := make(map[string]string, len(k.Fields))
		for _, f := range k.Fields {
			if f.Indexed || f.Type == "people" {
				declaredIndexed[f.Key] = f.Type
			}
		}
		ruleFields := KindRuleFields(k.Fields)
		for j, tab := range k.Library.Tabs {
			tbase := fmt.Sprintf("%s.library.tabs[%d]", base, j)
			if tab.Level != "" {
				switch tab.Kind {
				case "library", "view", "collections", "people":
					if !levelKeys[tab.Level] {
						add(tbase+".level", "must name a declared level")
					}
				default:
					add(tbase+".level", "only library, view, collections or people tabs may set level")
				}
			}
			switch tab.Kind {
			case "collections":
				if tab.Facet != "" && !seenFacet[tab.Facet] {
					add(tbase+".facet", "must name a declared browse facet")
				}
				if tab.Sort != "" {
					if tab.Facet == "" {
						add(tbase+".sort", "requires facet to be set")
					} else if !seenSort[tab.Sort] && !core.IsLastViewedSort(tab.Sort) {
						add(tbase+".sort", "must name a declared browse sort")
					}
				}
				if tab.Direction != "" {
					if tab.Facet == "" {
						add(tbase+".direction", "requires facet to be set")
					} else if tab.Direction != "asc" && tab.Direction != "desc" {
						add(tbase+".direction", "must be asc or desc")
					}
				}
				if tab.Order != "" {
					if tab.Facet == "" {
						add(tbase+".order", "requires facet to be set")
					} else if tab.Order != "count" && tab.Order != "name" {
						add(tbase+".order", "must be count or name")
					}
				}
				if tab.SearchPlaceholder != "" {
					label(tbase+".searchPlaceholder", tab.SearchPlaceholder, 60)
					if tab.Search != nil && !*tab.Search {
						add(tbase+".searchPlaceholder", "requires search to be enabled")
					}
				}
				if tab.Card != "" {
					add(tbase+".card", "only live or recommended tabs may set card")
				}
				if tab.Layout != "" {
					add(tbase+".layout", "only view tabs may set layout")
				}
				if tab.Filters != nil && !tab.Filters.IsEmpty() {
					add(tbase+".filters", "only view tabs may set filters")
				}
				if len(tab.Shelves) > 0 {
					add(tbase+".shelves", "only recommended tabs may set shelves")
				}
			case "people":
				if tab.Facet == "" || !seenFacet[tab.Facet] {
					add(tbase+".facet", "must name a declared browse facet")
				} else if fields[tab.Facet].Type != "people" {
					add(tbase+".facet", "must name a people field")
				}
				if tab.Sort != "" {
					add(tbase+".sort", "only collections or view tabs may set sort")
				}
				if tab.Direction != "" {
					add(tbase+".direction", "only collections or view tabs may set direction")
				}
				if tab.Order != "" && tab.Order != "count" && tab.Order != "name" {
					add(tbase+".order", "must be count or name")
				}
				if tab.SearchPlaceholder != "" {
					label(tbase+".searchPlaceholder", tab.SearchPlaceholder, 60)
					if tab.Search != nil && !*tab.Search {
						add(tbase+".searchPlaceholder", "requires search to be enabled")
					}
				}
				if tab.Card != "" {
					add(tbase+".card", "only live or recommended tabs may set card")
				}
				if tab.Layout != "" {
					add(tbase+".layout", "only view tabs may set layout")
				}
				if tab.Filters != nil && !tab.Filters.IsEmpty() {
					add(tbase+".filters", "only view tabs may set filters")
				}
				if len(tab.Shelves) > 0 {
					add(tbase+".shelves", "only recommended tabs may set shelves")
				}
			case "view":
				if tab.Facet != "" {
					add(tbase+".facet", "only collections or people tabs may set facet")
				}
				if tab.Order != "" {
					add(tbase+".order", "only collections or people tabs may set order")
				}
				if tab.Card != "" {
					add(tbase+".card", "only live or recommended tabs may set card")
				}
				if tab.Layout != "" && !kindLibraryTabLayouts[tab.Layout] {
					add(tbase+".layout", "must be grid, list or timeline")
				}
				if tab.Sort != "" && !seenSort[tab.Sort] && !core.IsLastViewedSort(tab.Sort) {
					add(tbase+".sort", "must name a declared browse sort or lastViewed")
				}
				if tab.Direction != "" && tab.Direction != "asc" && tab.Direction != "desc" {
					add(tbase+".direction", "must be asc or desc")
				}
				if tab.SearchPlaceholder != "" {
					label(tbase+".searchPlaceholder", tab.SearchPlaceholder, 60)
					if tab.Search != nil && !*tab.Search {
						add(tbase+".searchPlaceholder", "requires search to be enabled")
					}
				}
				if tab.Filters != nil {
					if tab.Filters.Match != "" && tab.Filters.Match != "all" && tab.Filters.Match != "any" {
						add(tbase+".filters.match", "must be all or any")
					}
					if tab.Filters.VisibilityDefault != "" && tab.Filters.VisibilityDefault != "show" && tab.Filters.VisibilityDefault != "hide" {
						add(tbase+".filters.visibilityDefault", "must be show or hide")
					}
					if !tab.Filters.IsEmpty() {
						specs, fallback := tab.Filters.ToPolicy("")
						if err := libraryfilter.ValidatePluginManifestPolicy(specs, fallback, declaredIndexed, ruleFields); err != nil {
							add(tbase+".filters", err.Error())
						}
					}
				}
				if len(tab.Shelves) > 0 {
					add(tbase+".shelves", "only recommended tabs may set shelves")
				}
			case "live":
				if tab.Facet != "" {
					add(tbase+".facet", "only collections or people tabs may set facet")
				}
				if tab.Sort != "" {
					add(tbase+".sort", "only collections or view tabs may set sort")
				}
				if tab.Direction != "" {
					add(tbase+".direction", "only collections or view tabs may set direction")
				}
				if tab.Order != "" {
					add(tbase+".order", "only collections or people tabs may set order")
				}
				if tab.Card != "" && !kindLibraryCards[tab.Card] {
					add(tbase+".card", "must be poster, landscape or square")
				}
				if tab.Layout != "" {
					add(tbase+".layout", "only view tabs may set layout")
				}
				if tab.Filters != nil && !tab.Filters.IsEmpty() {
					add(tbase+".filters", "only view tabs may set filters")
				}
				if tab.SearchPlaceholder != "" {
					label(tbase+".searchPlaceholder", tab.SearchPlaceholder, 60)
					if tab.Search != nil && !*tab.Search {
						add(tbase+".searchPlaceholder", "requires search to be enabled")
					}
				}
				if len(tab.Shelves) > 0 {
					add(tbase+".shelves", "only recommended tabs may set shelves")
				}
			case "recommended":
				if tab.Facet != "" {
					add(tbase+".facet", "only collections or people tabs may set facet")
				}
				if tab.Sort != "" {
					add(tbase+".sort", "only collections or view tabs may set sort")
				}
				if tab.Direction != "" {
					add(tbase+".direction", "only collections or view tabs may set direction")
				}
				if tab.Order != "" {
					add(tbase+".order", "only collections or people tabs may set order")
				}
				if tab.Card != "" && !kindLibraryCards[tab.Card] {
					add(tbase+".card", "must be poster, landscape or square")
				}
				if tab.Layout != "" {
					add(tbase+".layout", "only view tabs may set layout")
				}
				if tab.Filters != nil && !tab.Filters.IsEmpty() {
					add(tbase+".filters", "only view tabs may set filters")
				}
				if tab.Search != nil {
					add(tbase+".search", "only collections, people, view or live tabs may set search")
				}
				if tab.SearchPlaceholder != "" {
					add(tbase+".searchPlaceholder", "only collections, people, view or live tabs may set searchPlaceholder")
				}
				validateKindLibraryShelves(tbase+".shelves", tab.Shelves, levelKeys, fields, add, label)
			default:
				if tab.Facet != "" {
					add(tbase+".facet", "only collections or people tabs may set facet")
				}
				if tab.Sort != "" {
					add(tbase+".sort", "only collections or view tabs may set sort")
				}
				if tab.Direction != "" {
					add(tbase+".direction", "only collections or view tabs may set direction")
				}
				if tab.Order != "" {
					add(tbase+".order", "only collections or people tabs may set order")
				}
				if tab.Card != "" {
					add(tbase+".card", "only live or recommended tabs may set card")
				}
				if tab.Layout != "" {
					add(tbase+".layout", "only view tabs may set layout")
				}
				if tab.Filters != nil && !tab.Filters.IsEmpty() {
					add(tbase+".filters", "only view tabs may set filters")
				}
				if tab.Search != nil {
					add(tbase+".search", "only collections, people, view or live tabs may set search")
				}
				if tab.SearchPlaceholder != "" {
					add(tbase+".searchPlaceholder", "only collections, people, view or live tabs may set searchPlaceholder")
				}
				if len(tab.Shelves) > 0 {
					add(tbase+".shelves", "only recommended tabs may set shelves")
				}
			}
		}
	}
	validateKindLibraryShelves(base+".library.shelves", k.Library.Shelves, levelKeys, fields, add, label)
	for j, f := range k.Browse.SearchFields {
		field, ok := fields[f]
		if !ok {
			add(fmt.Sprintf("%s.browse.searchFields[%d]", base, j), "must name a declared field")
		} else if field.Type != "string" && field.Type != "strings" && field.Type != "people" {
			add(fmt.Sprintf("%s.browse.searchFields[%d]", base, j), "a search field must hold text")
		}
	}
	if len(k.Browse.SearchFields) > 0 && !k.Browse.Search {
		add(base+".browse.search", "searchFields need search to be set")
	}
	if k.Browse.LiveSearch && !hasProvider {
		add(base+".browse.liveSearch", "live search needs the catalog_provider capability")
	}

	switch k.Sync.Mode {
	case "":
		if hasProvider && k.HasLibrary() {
			add(base+".sync.mode", "a catalog provider kind must be pull or push")
		}
	case KindSyncPull, KindSyncPush:
		if !hasProvider {
			add(base+".sync.mode", "sync needs the catalog_provider capability")
		}
	case "ondemand":
		add(base+".sync.mode", "ondemand is not supported; kinds sync with pull or push")
	default:
		add(base+".sync.mode", "must be pull or push")
	}
	if k.Sync.Refresh != "" {
		if k.Sync.Mode != KindSyncPull {
			add(base+".sync.refresh", "only pull kinds refresh on a schedule")
		} else if d, err := time.ParseDuration(k.Sync.Refresh); err != nil || d < 15*time.Minute {
			add(base+".sync.refresh", "must be a duration of at least 15m")
		}
	}
	if k.Sync.Undo != "" {
		if k.Sync.Mode != KindSyncPush {
			add(base+".sync.undo", "only push kinds configure sync.undo")
		} else if k.Sync.Undo != KindSyncUndoSnapshots && k.Sync.Undo != KindSyncUndoIDs {
			add(base+".sync.undo", "must be snapshots or ids")
		}
	}
	if k.Cache.MaxBytes < 0 {
		add(base+".cache.maxBytes", "must not be negative")
	}
	if k.Cache.TTL != "" {
		if d, err := time.ParseDuration(k.Cache.TTL); err != nil || d <= 0 {
			add(base+".cache.ttl", "must be a positive duration")
		}
	}
	if k.Match.ThresholdDefault < 0 || k.Match.ThresholdDefault > 1 {
		add(base+".match.thresholdDefault", "must be between 0 and 1")
	}
}

func validatePresentations(m Manifest, k MediaKindMeta, base string, levelKeys map[string]bool, add func(string, string)) {
	p := k.Present
	protocols := 0
	if p.Vela != nil {
		protocols++
	}
	if p.Plex != nil {
		protocols++
	}
	if p.MediaBrowser != nil {
		protocols++
	}
	if p.Stremio != nil {
		protocols++
	}
	if p.Xtream != nil {
		protocols++
	}
	if p.Silo != nil {
		protocols++
	}
	if p.AdapterOnly {
		if protocols > 0 {
			add(base, "adapterOnly cannot be combined with a protocol presentation")
		}
		if !m.HasCapability(CapFrontendAdapter) {
			add(base+".adapterOnly", "needs a frontend adapter")
		}
		return
	}
	if protocols == 0 {
		// Present is optional: Ether infers per-app presentations from the
		// kind's media class, level roles, and hierarchy shape.
		return
	}
	levels := len(k.Levels)
	if p.Vela != nil {
		switch p.Vela.Type {
		case "":
			if p.Vela.Default == nil {
				add(base+".vela.type", "must be movies or shows")
			}
		case "movies":
		case "shows":
			if levels < 2 {
				add(base+".vela.type", "shows needs at least two levels")
			}
		default:
			add(base+".vela.type", "must be movies or shows")
		}
	}
	if p.Plex != nil {
		switch p.Plex.Base {
		case "":
			if p.Plex.Default == nil {
				add(base+".plex.base", "must be movie, show, artist or photo")
			}
		case "movie", "photo", "artist":
			// A single-level artist kind is a flat album list: its items
			// present as albums (a level named artist/track presents as
			// that instead), never as artists.
		case "show":
			if levels < 2 {
				add(base+".plex.base", p.Plex.Base+" needs at least two levels")
			}
		default:
			add(base+".plex.base", "must be movie, show, artist or photo")
		}
	}
	if p.MediaBrowser != nil {
		if p.MediaBrowser.CollectionType == "" {
			if p.MediaBrowser.Default == nil && len(p.MediaBrowser.ItemTypes) == 0 {
				add(base+".mediaBrowser.collectionType", "must be movies, tvshows, music, audiobooks, books, photos or homevideos")
			}
		} else if !mediaBrowserCollectionTypes[p.MediaBrowser.CollectionType] {
			add(base+".mediaBrowser.collectionType", "must be movies, tvshows, music, audiobooks, books, photos or homevideos")
		}
		for level, itemType := range p.MediaBrowser.ItemTypes {
			if !levelKeys[level] {
				add(base+".mediaBrowser.itemTypes", "unknown level "+level)
			}
			if !mediaBrowserItemTypes[itemType] {
				add(base+".mediaBrowser.itemTypes", "unknown item type "+itemType+" for level "+level+" (must be "+mediaBrowserItemTypeList+")")
			}
		}
	}
	if p.Stremio != nil {
		switch p.Stremio.Type {
		case "":
			if p.Stremio.Default == nil {
				add(base+".stremio.type", "must be movie, series, channel or tv")
			}
		case "movie", "channel", "tv":
		case "series":
			if levels < 2 {
				add(base+".stremio.type", "series needs at least two levels")
			}
		default:
			add(base+".stremio.type", "must be movie, series, channel or tv")
		}
	}
	if p.Xtream != nil {
		switch p.Xtream.Category {
		case "":
			if p.Xtream.Default == nil {
				add(base+".xtream.category", "must be movie, series or live")
			}
		case "movie", "live":
		case "series":
			if levels < 2 {
				add(base+".xtream.category", "series needs at least two levels")
			}
		default:
			add(base+".xtream.category", "must be movie, series or live")
		}
	}
	if p.Silo != nil {
		if p.Silo.Type == "" {
			if p.Silo.Default == nil {
				add(base+".silo.type", "must be movies, tvshows, or a MediaBrowser item type such as Movie or Series")
			}
		} else if !mediaBrowserItemTypes[p.Silo.Type] && !mediaBrowserCollectionTypes[p.Silo.Type] {
			add(base+".silo.type", "must be movies, tvshows, or a MediaBrowser item type such as Movie or Series")
		}
	}
}

func validateFrontendAdapters(m Manifest, add func(string, string), label func(string, string, int)) {
	adapters := m.Ether.Contributes.FrontendAdapters
	if m.HasCapability(CapFrontendAdapter) && len(adapters) == 0 {
		add("ether.contributes.frontendAdapters", "is required for frontend_adapter")
	}
	if !m.HasCapability(CapFrontendAdapter) && len(adapters) > 0 {
		add("ether.contributes.frontendAdapters", "needs the frontend_adapter capability")
	}
	seen := map[string]bool{}
	for i, a := range adapters {
		base := fmt.Sprintf("ether.contributes.frontendAdapters[%d]", i)
		if !kindKeyRe.MatchString(a.Key) || seen[a.Key] {
			add(base+".key", "must be a unique lower case key, at most 32 characters")
		}
		seen[a.Key] = true
		label(base+".label", a.Label, 60)
		if !map[string]bool{"opds": true, "subsonic": true, "custom": true}[a.ProtocolHint] {
			add(base+".protocolHint", "must be opds, subsonic or custom")
		}
		if !strings.HasPrefix(a.PathPrefix, "/") || a.PathPrefix == "/" {
			add(base+".pathPrefix", "must be a path below the shared listener root, such as /opds")
		}
		if a.DefaultPort < 0 || a.DefaultPort > 65535 {
			add(base+".defaultPort", "must be a port number")
		}
		if a.Auth != "" && !map[string]bool{"ether_token": true, "basic": true, "none_lan": true, "query_credentials": true}[a.Auth] {
			add(base+".auth", "must be ether_token, basic, none_lan or query_credentials")
		}
		seenLib := map[string]bool{}
		for j, key := range a.Libraries {
			if !kindKeyRe.MatchString(key) || seenLib[key] {
				add(fmt.Sprintf("%s.libraries[%d]", base, j), "must name a declared media kind key")
			}
			seenLib[key] = true
			if _, ok := m.MediaKindByKey(key); !ok {
				add(fmt.Sprintf("%s.libraries[%d]", base, j), "must name a media kind declared by this plugin")
			}
		}
		if len(a.SearchPaths) > MaxAdapterSearchPaths {
			add(base+".searchPaths", fmt.Sprintf("must declare at most %d search paths", MaxAdapterSearchPaths))
		}
		seenSearch := map[string]bool{}
		for j, sp := range a.SearchPaths {
			norm, ok := normalizeAdapterSearchPath(sp)
			if !ok {
				add(fmt.Sprintf("%s.searchPaths[%d]", base, j), "must be a clean subpath starting with /, without ?, #, whitespace, or ..")
				continue
			}
			if seenSearch[norm] {
				add(fmt.Sprintf("%s.searchPaths[%d]", base, j), "must not repeat a search path")
				continue
			}
			seenSearch[norm] = true
		}
		if len(a.UploadPaths) > maxAdapterUploadPaths {
			add(base+".uploadPaths", fmt.Sprintf("must declare at most %d upload paths", maxAdapterUploadPaths))
		}
		seenUploads := map[string]bool{}
		for j, upload := range a.UploadPaths {
			field := fmt.Sprintf("%s.uploadPaths[%d]", base, j)
			norm, ok := normalizeAdapterSearchPath(upload.Path)
			if _, err := path.Match(norm, ""); !ok || err != nil || len(norm) > 256 {
				add(field+".path", "must be a clean subpath glob starting with /, at most 256 characters")
			} else if seenUploads[norm] {
				add(field+".path", "must not repeat an upload path")
			}
			seenUploads[norm] = true
			if upload.MaxBytes < 1 || upload.MaxBytes > MaxAdapterUploadBytes {
				add(field+".maxBytes", fmt.Sprintf("must be between 1 and %d bytes", MaxAdapterUploadBytes))
			}
		}
		if a.Icon != "" && !ValidAdapterIconPath(a.Icon) {
			add(base+".icon", "must be an SVG or PNG file inside the package")
		}
	}
}

const (
	DefaultAdapterUploadBytes int64 = 64 << 20
	MaxAdapterUploadBytes     int64 = 512 << 20
	maxAdapterUploadPaths           = 16
)

// UploadLimit returns the smallest limit of the declared globs matching a
// normalized adapter subpath, or zero for ordinary inline requests.
func (a FrontendAdapterMeta) UploadLimit(subpath string) int64 {
	if len(a.UploadPaths) == 0 {
		return 0
	}
	subpath, ok := normalizeAdapterSearchPath(subpath)
	if !ok {
		return 0
	}
	var limit int64
	for _, upload := range a.UploadPaths {
		pattern, ok := normalizeAdapterSearchPath(upload.Path)
		if !ok || upload.MaxBytes < 1 || upload.MaxBytes > MaxAdapterUploadBytes {
			continue
		}
		if match, _ := path.Match(pattern, subpath); match && (limit == 0 || upload.MaxBytes < limit) {
			limit = upload.MaxBytes
		}
	}
	return limit
}

// ValidAdapterIconPath reports whether p names an SVG or PNG icon inside
// the package; the conformance kit checks the file itself.
func ValidAdapterIconPath(p string) bool {
	lower := strings.ToLower(p)
	return !unsafePath(p) && !strings.ContainsAny(p, "\\:%?#\r\n\t") && (strings.HasSuffix(lower, ".svg") || strings.HasSuffix(lower, ".png"))
}

// MediaKindByKey returns the declared kind with this key, if any.
func (m Manifest) MediaKindByKey(key string) (MediaKindMeta, bool) {
	if e := m.EffectiveEther(); e != nil {
		for _, k := range e.Contributes.MediaKinds {
			if k.Key == key {
				return k, true
			}
		}
	}
	return MediaKindMeta{}, false
}

// HasLibrary reports whether this media kind declares a library block.
// Creating a library is optional per kind: a kind without a library block
// (for example, a resolver-only kind) creates no library.
func (k MediaKindMeta) HasLibrary() bool {
	return strings.TrimSpace(k.Library.Name) != ""
}

// Level returns the declared level with this key and its position.
func (k MediaKindMeta) Level(key string) (KindLevel, int, bool) {
	for i, lv := range k.Levels {
		if lv.Key == key {
			return lv, i, true
		}
	}
	return KindLevel{}, -1, false
}

// LevelParent returns the effective parent level key for key: the declared
// KindLevel.Parent when set, or the preceding level's key by default. The root
// level (index 0) and unknown keys return "".
func (k MediaKindMeta) LevelParent(key string) string {
	lv, idx, ok := k.Level(key)
	if !ok || idx == 0 || lv.Members {
		return ""
	}
	if lv.Parent != "" {
		return lv.Parent
	}
	return k.Levels[idx-1].Key
}

// ChildLevels returns all declared levels whose effective parent is parentKey,
// in manifest declaration order.
func (k MediaKindMeta) ChildLevels(parentKey string) []KindLevel {
	if parentKey == "" {
		return nil
	}
	var out []KindLevel
	for _, lv := range k.Levels {
		if k.LevelParent(lv.Key) == parentKey {
			out = append(out, lv)
		}
	}
	return out
}

// PrimaryChildLevel returns the primary direct child level of parentKey.
// For a branching container (more than one direct child level), it prefers
// the first child level whose effective media class matches parentKey's own
// effective media class, falling back to the first declared child level.
func (k MediaKindMeta) PrimaryChildLevel(parentKey string, kindMedia core.MediaClass) (KindLevel, bool) {
	children := k.ChildLevels(parentKey)
	if len(children) == 0 {
		return KindLevel{}, false
	}
	if len(children) == 1 {
		return children[0], true
	}
	parentMedia := k.EffectiveLevelMedia(parentKey, kindMedia)
	for _, lv := range children {
		if k.EffectiveLevelMedia(lv.Key, kindMedia) == parentMedia {
			return lv, true
		}
	}
	return children[0], true
}

// HasChildLevels reports whether any declared level has key as its effective parent.
func (k MediaKindMeta) HasChildLevels(key string) bool {
	if key == "" {
		return false
	}
	for _, lv := range k.Levels {
		if k.LevelParent(lv.Key) == key {
			return true
		}
	}
	return false
}

// IsLeafLevel reports whether key is a declared level with no child levels.
func (k MediaKindMeta) IsLeafLevel(key string) bool {
	_, _, ok := k.Level(key)
	return ok && !k.HasChildLevels(key)
}

// LeafLevels returns the set of all declared level keys that have no child levels.
func (k MediaKindMeta) LeafLevels() map[string]bool {
	out := make(map[string]bool, len(k.Levels))
	for _, lv := range k.Levels {
		if !k.HasChildLevels(lv.Key) {
			out[lv.Key] = true
		}
	}
	return out
}

// LevelDepth returns the zero-based tree depth of key (0 for the root level,
// 1 for direct children of root, etc.), or -1 if key is not declared.
func (k MediaKindMeta) LevelDepth(key string) int {
	if _, _, ok := k.Level(key); !ok {
		return -1
	}
	depth := 0
	cur := key
	for step := 0; step < len(k.Levels); step++ {
		p := k.LevelParent(cur)
		if p == "" {
			return depth
		}
		depth++
		cur = p
	}
	return depth
}

// EffectiveLevelMedia returns the effective media class for level key: the
// level's explicit Media override when declared, else the kind's Media, else
// fallback (or core.MediaClassVideo when fallback is empty).
func (k MediaKindMeta) EffectiveLevelMedia(key string, fallback core.MediaClass) core.MediaClass {
	if lv, _, ok := k.Level(key); ok && lv.Media != "" && core.ValidMediaClass(lv.Media) {
		return core.MediaClass(lv.Media)
	}
	if k.Media != "" && core.ValidMediaClass(k.Media) {
		return core.MediaClass(k.Media)
	}
	if fallback != "" {
		return fallback
	}
	return core.MediaClassVideo
}

// IsVideoTitleLevel reports whether key is a playable level that explicitly
// declares media: "video", projecting as a video title (kind: "movie") inside
// any library regardless of the library's own media class.
func (k MediaKindMeta) IsVideoTitleLevel(key string) bool {
	lv, _, ok := k.Level(key)
	return ok && lv.Playable && lv.Media == string(core.MediaClassVideo)
}

func validKindLevelRole(role string) bool {
	if role == "" || strings.TrimSpace(role) != role {
		return false
	}
	if utf8.RuneCountInString(role) > 40 {
		return false
	}
	for _, r := range role {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}

// DeclHash is the upgrade gate for an installed kind (spec §2.3/§2.5):
// the SHA-256 of the declaration's canonical JSON. Go marshals struct
// fields in declaration order and map keys sorted, so equal
// declarations always hash equal.
func (k MediaKindMeta) DeclHash() string {
	raw, _ := json.Marshal(k)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// ValidateMediaKindsUpgrade applies the additive-only upgrade rule
// (spec §2.5): a new manifest may add kinds, fields, trailing levels
// and art roles, and may change labels, presentations, browse, cache
// and match defaults, but it may not drop or rename a kind, drop or
// retype a field, shrink an enum, drop a field's index, or remove,
// reorder or re-flag a level. A caller that gets an error refuses the
// upgrade unless the owner chose to replace the library contents.
func ValidateMediaKindsUpgrade(prev, next []MediaKindMeta) error {
	var f []FieldError
	add := func(field, msg string) { f = append(f, FieldError{field, msg}) }
	nextByKey := map[string]MediaKindMeta{}
	for _, k := range next {
		nextByKey[k.Key] = k
	}
	for _, old := range prev {
		base := "ether.contributes.mediaKinds." + old.Key
		nk, ok := nextByKey[old.Key]
		if !ok {
			add(base, "kind was removed; removing a kind needs the owner to replace its library")
			continue
		}
		for i, lv := range old.Levels {
			lbase := fmt.Sprintf("%s.levels[%d]", base, i)
			if i >= len(nk.Levels) {
				add(lbase, "level "+lv.Key+" was removed")
				continue
			}
			nl := nk.Levels[i]
			if nl.Key != lv.Key {
				add(lbase+".key", "levels cannot be removed or reordered; add new levels at the end")
			}
			if nl.Playable != lv.Playable || nl.Container != lv.Container {
				add(lbase, "level "+lv.Key+" changed its playable or container flag")
			}
			if old.LevelParent(lv.Key) != nk.LevelParent(nl.Key) {
				add(lbase+".parent", "level "+lv.Key+" changed its parent")
			}
		}
		nextFields := map[string]KindField{}
		for _, nf := range nk.Fields {
			nextFields[nf.Key] = nf
		}
		for _, of := range old.Fields {
			fbase := base + ".fields." + of.Key
			nf, ok := nextFields[of.Key]
			if !ok {
				add(fbase, "field was removed")
				continue
			}
			if nf.Type != of.Type {
				add(fbase+".type", "field changed type from "+of.Type+" to "+nf.Type)
			}
			if of.Indexed && !nf.Indexed {
				add(fbase+".indexed", "field lost its index")
			}
			if of.Type == "enum" && nf.Type == "enum" {
				have := map[string]bool{}
				for _, v := range nf.Enum {
					have[v] = true
				}
				for _, v := range of.Enum {
					if !have[v] {
						add(fbase+".enum", "enum value "+v+" was removed")
					}
				}
			}
		}
	}
	if len(f) > 0 {
		return &ValidationError{f}
	}
	return nil
}

func validateKindLibraryShelves(shelvesPath string, shelves []KindLibraryShelf, levelKeys map[string]bool, fields map[string]KindField, add func(string, string), label func(string, string, int)) {
	if len(shelves) == 0 {
		return
	}
	if len(shelves) > MaxKindLibraryShelves {
		add(shelvesPath, fmt.Sprintf("at most %d shelves are allowed", MaxKindLibraryShelves))
	}
	declaredIndexed := make(map[string]string, len(fields))
	fieldList := make([]KindField, 0, len(fields))
	for _, f := range fields {
		fieldList = append(fieldList, f)
		if f.Indexed || f.Type == "people" {
			declaredIndexed[f.Key] = f.Type
		}
	}
	ruleFields := KindRuleFields(fieldList)
	checkFacet := func(fieldPath, fk string) {
		if fk == "year" || fk == "released" || fk == "rating" {
			return
		}
		field, ok := fields[fk]
		if !ok {
			add(fieldPath, "must name a declared field")
		} else if !field.Indexed && field.Type != "people" {
			add(fieldPath, "a shelf facet field must be indexed")
		}
	}
	seenKey := map[string]bool{}
	for j, sh := range shelves {
		sbase := fmt.Sprintf("%s[%d]", shelvesPath, j)
		if !kindShelfKeyRe.MatchString(sh.Key) {
			add(sbase+".key", "must match ^[a-z][a-z0-9-]{0,39}$")
		} else if seenKey[sh.Key] {
			add(sbase+".key", "is listed twice")
		}
		seenKey[sh.Key] = true
		label(sbase+".label", sh.Label, 60)
		if sh.Level != "" && !levelKeys[sh.Level] {
			add(sbase+".level", "must name a declared level")
		}
		if sh.Sort != "" && !kindSorts[sh.Sort] {
			add(sbase+".sort", "must be title, added, released, year, rating, index or lastViewed")
		}
		if sh.Direction != "" && sh.Direction != "asc" && sh.Direction != "desc" {
			add(sbase+".direction", "must be asc or desc")
		}
		if sh.Purpose != "" && sh.Purpose != string(core.PurposeRecentContexts) && sh.Purpose != string(core.PurposeLiked) {
			add(sbase+".purpose", "must be recentContexts or liked")
		}
		if sh.Layout != "" && !core.ValidRowStyle(sh.Layout) {
			add(sbase+".layout", "must be standard, hero, wide, tiles or tracks")
		}
		if sh.Facet != "" && !sh.Filters.IsEmpty() {
			add(sbase+".filters", "cannot be combined with facet")
		}
		if sh.Facet != "" {
			checkFacet(sbase+".facet", sh.Facet)
			switch {
			case sh.Value != "" && len(sh.Values) > 0:
				add(sbase+".values", "cannot be combined with value")
			case sh.Value != "":
				if strings.TrimSpace(sh.Value) == "" {
					add(sbase+".value", "must not be empty")
				}
			case len(sh.Values) > 0:
				for vi, v := range sh.Values {
					if strings.TrimSpace(v) == "" {
						add(fmt.Sprintf("%s.values[%d]", sbase, vi), "must not be empty")
					}
				}
			default:
				add(sbase+".value", "facet requires value or values")
			}
		} else {
			if sh.Value != "" {
				add(sbase+".value", "requires facet to be set")
			}
			if len(sh.Values) > 0 {
				add(sbase+".values", "requires facet to be set")
			}
		}
		if sh.Filters != nil {
			if sh.Filters.Tab != nil {
				tf := sh.Filters.Tab
				if tf.Match != "" && tf.Match != "all" && tf.Match != "any" {
					add(sbase+".filters.match", "must be all or any")
				}
				if tf.VisibilityDefault != "" && tf.VisibilityDefault != "show" && tf.VisibilityDefault != "hide" {
					add(sbase+".filters.visibilityDefault", "must be show or hide")
				}
				if !tf.IsEmpty() {
					specs, fallback := tf.ToPolicy("")
					if err := libraryfilter.ValidatePluginManifestPolicy(specs, fallback, declaredIndexed, ruleFields); err != nil {
						add(sbase+".filters", err.Error())
					}
				}
			}
			if len(sh.Filters.Facets) > 0 {
				fkeys := make([]string, 0, len(sh.Filters.Facets))
				for fk := range sh.Filters.Facets {
					fkeys = append(fkeys, fk)
				}
				sort.Strings(fkeys)
				for _, fk := range fkeys {
					if strings.TrimSpace(fk) == "" {
						add(sbase+".filters", "filter facet key must not be empty")
						continue
					}
					fbase := sbase + ".filters." + fk
					checkFacet(fbase, fk)
					vals := sh.Filters.Facets[fk]
					if len(vals) == 0 {
						add(fbase, "must list at least one value")
						continue
					}
					for vi, v := range vals {
						if strings.TrimSpace(v) == "" {
							add(fmt.Sprintf("%s[%d]", fbase, vi), "must not be empty")
						}
					}
				}
			}
		}
	}
}

// ListLevels are the kind's list levels (members), roots of their own.
func (k MediaKindMeta) ListLevels() []KindLevel {
	var out []KindLevel
	for _, lv := range k.Levels {
		if lv.Members {
			out = append(out, lv)
		}
	}
	return out
}
