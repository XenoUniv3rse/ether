package pluginitems

import (
	"bytes"
	"encoding/json"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/libraries"
	"github.com/braedonsaunders/ether/internal/plugins"
)

// EffectiveCard resolves a plugin library's card shape: the owner's CardShape
// choice first, then the plugin's declared library.card ("poster" by default).
func EffectiveCard(cardShape string, decl plugins.KindLibraryDecl) string {
	switch cardShape {
	case core.CardPoster, core.CardLandscape, core.CardSquare:
		return cardShape
	default:
		return decl.EffectiveCard()
	}
}

// TextResolver looks up a plugin i18n key (without the "$t:" prefix) in the
// plugin's strings file for the default locale.
type TextResolver func(key string) (string, bool)

// Project maps a plugin KindItem into a core.Title for legacy Plex/Jellyfin/Emby
// frontends per §6.3.
//
// Mapping rules:
//   - Title.Key = item.Key (a PluginKey(seq))
//   - Title.Type:
//   - Single-level playable kind -> TypeMovie
//   - 2-level hierarchy (e.g. channel -> video) -> root = TypeShow, leaf = TypeEpisode
//   - 3-level hierarchy (e.g. series -> season -> episode) -> TypeShow / TypeSeason / TypeEpisode
//   - Title.Title = item.Title
//   - Title.SortTitle = SortTitle(item.Title) or FoldSortTitle(item.SortTitle)
//   - Title.Summary = item.Summary + formatted declared non-empty fields at the end
//   - Title.Year = item.Year
//   - Title.Released = time.Unix(item.Released, 0)
//   - Title.Runtime = duration field (e.g. "duration" / "runtime") when present
//   - Title.Genres = item.Fields["genre"/"genres"/"tags"] + facet fields
//   - Title.PosterPath = art["poster"] (falling back to art["thumb"])
//   - Title.BackdropPath = art["backdrop"]
//   - Title.StillPath = art["thumb"] (for episode-like items)
//   - Title.LogoPath = art["logo"]
//
// Any "$t:" field labels without a text resolver fall back to the field key so
// unresolved i18n tokens are never emitted.
func Project(item core.KindItem, decl plugins.MediaKindMeta, present core.Presentations, cardShape ...string) core.Title {
	return ProjectWithText(item, decl, present, nil, cardShape...)
}

// ResolveMediaClassAndRole derives the effective MediaClass and per-level
// MediaRole for a declared kind and level via libraries.ResolveKindPresentations.
func ResolveMediaClassAndRole(level string, decl plugins.MediaKindMeta, present core.Presentations) (core.MediaClass, core.MediaRole) {
	class, role, _ := resolveLevelPresentation(level, decl, present)
	return class, role
}

func resolveLevelPresentation(level string, decl plugins.MediaKindMeta, present core.Presentations) (core.MediaClass, core.MediaRole, string) {
	levels := make([]libraries.KindPresentationLevel, len(decl.Levels))
	for i, lv := range decl.Levels {
		levels[i] = libraries.KindPresentationLevel{
			Key:       lv.Key,
			Parent:    lv.Parent,
			Media:     lv.Media,
			Role:      lv.Role,
			Playable:  lv.Playable,
			Container: lv.Container,
			Members:   lv.Members,
		}
	}
	res := libraries.ResolveKindPresentations(libraries.KindPresentationInput{
		Media:   decl.Media,
		Levels:  levels,
		Present: present,
	}, core.PresentationOverrides{})
	class := decl.EffectiveLevelMedia(level, res.MediaClass)
	role := res.LevelRoles[level]
	if role == "" && class != "" {
		role = core.MediaRoleItem
	}
	mbItemType := ""
	if present.MediaBrowser != nil {
		if len(present.MediaBrowser.ItemTypes) > 0 {
			mbItemType = present.MediaBrowser.ItemTypes[level]
		} else if res.Effective.MediaBrowser != nil {
			mbItemType = res.Effective.MediaBrowser.ItemTypes[level]
		}
	}
	return class, role, mbItemType
}

// ProjectWithText behaves like Project and resolves "$t:" field labels through
// resolve (falling back to the field key when unresolved).
func ProjectWithText(item core.KindItem, decl plugins.MediaKindMeta, present core.Presentations, resolve TextResolver, cardShape ...string) core.Title {
	var ownerCard string
	if len(cardShape) > 0 {
		ownerCard = cardShape[0]
	}
	mediaType := projectMediaType(item.Level, decl, present)
	mediaClass, mediaRole, mbItemType := resolveLevelPresentation(item.Level, decl, present)

	sortTitle := core.EffectiveSortTitle(item.Title, item.SortTitle)

	dateDisplay := ""
	if decl.Library.EffectiveDateDisplay() == core.DateDisplayDate {
		dateDisplay = core.DateDisplayDate
	}

	reading := extractTitleReading(item)
	effIndex, indexDisplay := extractIndexAndDisplay(item)
	seriesField := SeriesFacetField(decl)
	seriesParents := synthesizeSeriesParents(item, seriesField, mediaRole, effIndex, indexDisplay)

	var primaryChildLevel string
	if len(decl.ChildLevels(item.Level)) > 1 {
		if primary, ok := decl.PrimaryChildLevel(item.Level, mediaClass); ok {
			primaryChildLevel = primary.Key
		}
	}

	// ChildCount for tallied parent levels comes strictly from stored parent
	// tallies (plugin_item_parent_tally), ignoring plugin-supplied ChildCount.
	childCount := item.ChildCount
	if len(decl.Levels) >= 2 && decl.HasChildLevels(item.Level) {
		childCount = 0
	}

	level, _, declared := decl.Level(item.Level)
	t := core.Title{
		Key:               item.Key,
		Type:              mediaType,
		MBItemType:        mbItemType,
		LyricsDeclared:    decl.Lyrics && declared && level.Playable && item.Playable && item.Unavailable == nil,
		RecordingID:       item.Recording,
		KindLevel:         item.Level,
		OrderedList:       declared && level.Members,
		PrimaryChildLevel: primaryChildLevel,
		ItemIndex:         effIndex,
		PluginAddedAt:     item.AddedAt,
		LandscapeCard:     EffectiveCard(ownerCard, decl.Library) == core.CardLandscape,
		DateDisplay:       dateDisplay,
		MediaClass:        mediaClass,
		MediaRole:         mediaRole,
		IndexDisplay:      indexDisplay,
		Disc:              extractDiscNumber(item),
		MediaParents:      seriesParents,
		MediaReading:      reading,
		Title:             item.Title,
		SortTitle:         sortTitle,
		Summary:           strings.TrimSpace(item.Summary),
		Year:              item.Year,
		Rating:            item.Rating,
		Popularity:        extractPopularity(item),
		ChildCount:        childCount,
		LeafCount:         item.LeafCount,
	}

	if item.Released > 0 {
		t.Released = time.Unix(item.Released, 0).UTC()
		if t.Year == 0 {
			t.Year = t.Released.Year()
		}
	}

	// Hierarchy indices.
	switch mediaType {
	case core.TypeSeason:
		t.Season = item.Index
	case core.TypeEpisode:
		t.Episode = item.Index
		if decl.LevelDepth(item.Level) == 1 {
			t.Season = 1
		}
	}

	// Artwork roles.
	if len(item.Art) > 0 {
		var thumb string
		for _, a := range item.Art {
			u := strings.TrimSpace(a.URL)
			if u == "" {
				continue
			}
			switch a.Role {
			case "poster":
				if t.PosterPath == "" {
					t.PosterPath = u
				}
			case "backdrop":
				if t.BackdropPath == "" {
					t.BackdropPath = u
				}
			case "logo":
				if t.LogoPath == "" {
					t.LogoPath = u
				}
			case "thumb":
				if thumb == "" {
					thumb = u
				}
			}
		}
		t.ThumbPath = thumb
		if mediaType == core.TypeEpisode {
			t.StillPath = thumb
			if t.PosterPath == "" {
				t.PosterPath = thumb
			}
		} else if t.PosterPath == "" {
			t.PosterPath = thumb
		}
	}

	// Extract runtime from any declared "duration" field or well-known duration keys.
	if sec := extractDurationSeconds(item.Fields, decl); sec > 0 {
		t.Runtime = time.Duration(sec) * time.Second
	}

	// Genres, studios, and people from declared fields.
	t.Genres = extractGenresAndFacets(item, decl)
	if studios := extractStringsFromFields(item.Fields, "studio", "studios", "channel", "publisher", "author"); len(studios) > 0 {
		t.Studios = studios
	}
	cast, directors, writers, crew, credits := extractProjectedPeople(item.Fields, decl)
	if len(cast) > 0 {
		t.Cast = cast
	}
	if len(directors) > 0 {
		t.Directors = directors
	}
	if len(writers) > 0 {
		t.Writers = writers
	}
	if len(crew) > 0 {
		t.Crew = crew
	}
	if len(credits) > 0 {
		t.MediaCredits = credits
	}
	t.MediaFacts = extractMediaFacts(item, decl, resolve, reading != nil, seriesField, credits)

	return t
}

// creditRoleForField maps a declared plugin people field key to its canonical
// singular MediaCredit role (artist, albumArtist, author, narrator, composer,
// illustrator, translator, host, director, writer, creator, producer, actor,
// or the trimmed field key when unrecognized).
func creditRoleForField(fieldKey string) string {
	k := strings.TrimSpace(fieldKey)
	switch strings.ToLower(k) {
	case "artist", "artists":
		return "artist"
	case "albumartist", "albumartists", "album_artist", "album_artists":
		return "albumArtist"
	case "author", "authors":
		return "author"
	case "narrator", "narrators":
		return "narrator"
	case "composer", "composers":
		return "composer"
	case "illustrator", "illustrators":
		return "illustrator"
	case "translator", "translators":
		return "translator"
	case "host", "hosts":
		return "host"
	case "director", "directors":
		return "director"
	case "writer", "writers":
		return "writer"
	case "creator", "creators":
		return "creator"
	case "producer", "producers":
		return "producer"
	case "editor", "editors":
		return "editor"
	case "cast", "actor", "actors", "performer", "performers":
		return "actor"
	default:
		return k
	}
}

func peopleFieldBucket(fieldKey string) string {
	switch creditRoleForField(fieldKey) {
	case "director", "creator", "host":
		return "director"
	case "writer", "author":
		return "writer"
	case "composer", "illustrator", "translator", "producer", "editor":
		return "crew"
	default:
		return "actor"
	}
}

func defaultRoleLabel(role string) string {
	switch role {
	case "author":
		return "Author"
	case "narrator":
		return "Narrator"
	case "composer":
		return "Composer"
	case "illustrator":
		return "Illustrator"
	case "translator":
		return "Translator"
	case "host":
		return "Host"
	case "producer":
		return "Producer"
	case "editor":
		return "Editor"
	case "creator":
		return "Creator"
	default:
		return ""
	}
}

func extractProjectedPeople(fields map[string]json.RawMessage, decl plugins.MediaKindMeta) (cast, directors, writers, crew []core.Person, credits []core.MediaCredit) {
	if len(fields) == 0 {
		return nil, nil, nil, nil, nil
	}
	type personKey struct {
		id   int64
		role string
	}
	appendUnique := func(dst *[]core.Person, seen map[personKey]int, list []core.Person) {
		for _, p := range list {
			if p.TMDB <= 0 || p.Name == "" {
				continue
			}
			k := personKey{id: p.TMDB, role: p.Role}
			if idx, ok := seen[k]; ok {
				if (*dst)[idx].ThumbPath == "" && p.ThumbPath != "" {
					(*dst)[idx].ThumbPath = p.ThumbPath
				}
				continue
			}
			seen[k] = len(*dst)
			*dst = append(*dst, p)
		}
	}
	seenCast := map[personKey]int{}
	seenDir := map[personKey]int{}
	seenWri := map[personKey]int{}
	seenCrew := map[personKey]int{}
	seenCredits := map[personKey]int{}

	addCredits := func(fieldKey string, list []core.Person) {
		baseRole := creditRoleForField(fieldKey)
		generic := strings.EqualFold(fieldKey, "credits") || strings.EqualFold(fieldKey, "people")
		bucket := peopleFieldBucket(fieldKey)
		defLabel := defaultRoleLabel(baseRole)
		bucketed := make([]core.Person, 0, len(list))
		for _, p := range list {
			if p.TMDB <= 0 || p.Name == "" {
				continue
			}
			role := baseRole
			if generic && strings.TrimSpace(p.Role) != "" {
				role = creditRoleForField(p.Role)
			}
			ck := personKey{id: p.TMDB, role: role}
			if idx, ok := seenCredits[ck]; ok {
				if credits[idx].ThumbPath == "" && p.ThumbPath != "" {
					credits[idx].ThumbPath = p.ThumbPath
				}
			} else {
				seenCredits[ck] = len(credits)
				credits = append(credits, core.MediaCredit{
					Name:      p.Name,
					Role:      role,
					TMDB:      p.TMDB,
					ThumbPath: p.ThumbPath,
				})
			}
			cp := p
			if cp.Role == "" && defLabel != "" {
				cp.Role = defLabel
			}
			bucketed = append(bucketed, cp)
		}
		switch bucket {
		case "director":
			appendUnique(&directors, seenDir, bucketed)
		case "writer":
			appendUnique(&writers, seenWri, bucketed)
		case "crew":
			appendUnique(&crew, seenCrew, bucketed)
		default:
			appendUnique(&cast, seenCast, bucketed)
		}
	}

	if len(decl.Fields) > 0 {
		for _, fd := range decl.Fields {
			if !isPeopleIndexedField(fd) {
				continue
			}
			list := extractPeopleFromFields(fields, fd.Key)
			if len(list) == 0 {
				continue
			}
			addCredits(fd.Key, list)
		}
		return cast, directors, writers, crew, credits
	}

	for _, fk := range []string{
		"albumArtist", "albumArtists", "album_artist", "album_artists",
		"artist", "artists",
		"author", "authors",
		"narrator", "narrators",
		"composer", "composers",
		"illustrator", "illustrators",
		"translator", "translators",
		"host", "hosts",
		"director", "directors", "creator", "creators",
		"writer", "writers",
		"producer", "producers",
		"cast", "actors",
	} {
		if list := extractPeopleFromFields(fields, fk); len(list) > 0 {
			addCredits(fk, list)
		}
	}
	return cast, directors, writers, crew, credits
}

func projectMediaType(level string, decl plugins.MediaKindMeta, present core.Presentations) core.MediaType {
	if decl.IsVideoTitleLevel(level) {
		return core.TypeMovie
	}
	if present.MediaBrowser != nil && len(present.MediaBrowser.ItemTypes) > 0 {
		switch present.MediaBrowser.ItemTypes[level] {
		case "Movie", "Video":
			return core.TypeMovie
		case "Series":
			return core.TypeShow
		case "Season":
			return core.TypeSeason
		case "Episode":
			return core.TypeEpisode
		}
	}
	if present.Vela != nil && present.Vela.Type == "movies" {
		return core.TypeMovie
	}
	if present.Plex != nil && present.Plex.Base == "movie" {
		return core.TypeMovie
	}

	levels := decl.Levels
	if len(levels) <= 1 {
		return core.TypeMovie
	}

	depth := decl.LevelDepth(level)
	isLeaf := decl.IsLeafLevel(level)
	maxDepth := 0
	for _, lv := range levels {
		if d := decl.LevelDepth(lv.Key); d > maxDepth {
			maxDepth = d
		}
	}

	if maxDepth <= 0 {
		return core.TypeMovie
	}
	if maxDepth == 1 {
		if depth == 0 {
			return core.TypeShow
		}
		return core.TypeEpisode
	}

	if depth == 0 {
		return core.TypeShow
	}
	if isLeaf {
		return core.TypeEpisode
	}
	return core.TypeSeason
}

func extractDiscNumber(item core.KindItem) int {
	for _, key := range []string{"disc", "discNumber", "disc_number"} {
		if raw, ok := item.Fields[key]; ok && len(raw) > 0 {
			if n, err := parseJSONInt64(raw); err == nil && n > 0 {
				return int(n)
			}
		}
	}
	if len(item.Hints) > 0 {
		var h struct {
			Disc       int `json:"disc"`
			DiscNumber int `json:"discNumber"`
		}
		if json.Unmarshal(item.Hints, &h) == nil {
			if h.Disc > 0 {
				return h.Disc
			}
			if h.DiscNumber > 0 {
				return h.DiscNumber
			}
		}
	}
	return 0
}

// SeriesFacetField returns the declared string/string[] field key that acts as
// a series facet for decl (such as "series" on 1-level book/audiobook kinds),
// or "" when the kind declares no such facet.
func SeriesFacetField(decl plugins.MediaKindMeta) string {
	strFields := make(map[string]plugins.KindField, len(decl.Fields))
	for _, fd := range decl.Fields {
		if fd.Type == "string" || fd.Type == "strings" || fd.Type == "string[]" {
			strFields[fd.Key] = fd
		}
	}
	isSeriesName := func(k string) bool {
		switch strings.ToLower(strings.TrimSpace(k)) {
		case "series", "bookseries", "book_series":
			return true
		default:
			return false
		}
	}
	for _, fk := range decl.Browse.Facets {
		if isSeriesName(fk) {
			if _, ok := strFields[fk]; ok {
				return fk
			}
		}
	}
	for _, tab := range decl.Library.Tabs {
		if tab.Kind == "collections" && tab.Facet != "" && (isSeriesName(tab.Key) || isSeriesName(tab.Facet)) {
			if _, ok := strFields[tab.Facet]; ok {
				return tab.Facet
			}
		}
	}
	for _, fd := range decl.Fields {
		if fd.Indexed && isSeriesName(fd.Key) {
			if _, ok := strFields[fd.Key]; ok {
				return fd.Key
			}
		}
	}
	return ""
}

func synthesizeSeriesParents(item core.KindItem, seriesField string, role core.MediaRole, idx int, idxDisplay string) []core.TitleParent {
	if seriesField == "" || role == core.MediaRoleSeries || len(item.Fields) == 0 {
		return nil
	}
	raw, ok := item.Fields[seriesField]
	if !ok || len(raw) == 0 {
		return nil
	}
	var vals []string
	var single string
	if json.Unmarshal(raw, &single) == nil {
		if s := strings.TrimSpace(single); s != "" {
			vals = []string{s}
		}
	} else {
		var list []string
		if json.Unmarshal(raw, &list) == nil {
			for _, v := range list {
				if s := strings.TrimSpace(v); s != "" {
					vals = append(vals, s)
				}
			}
		}
	}
	if len(vals) == 0 {
		return nil
	}
	seen := make(map[string]bool, len(vals))
	out := make([]core.TitleParent, 0, len(vals))
	for _, name := range vals {
		k := strings.ToLower(name)
		if seen[k] {
			continue
		}
		seen[k] = true
		p := core.TitleParent{
			FacetID: "facet:" + seriesField + ":" + name,
			Title:   name,
			Role:    core.MediaRoleSeries,
		}
		if len(out) == 0 {
			p.Index = idx
			p.IndexDisplay = idxDisplay
		}
		out = append(out, p)
	}
	return out
}

func parseLeadingPositiveInt(raw string) int {
	s := strings.TrimSpace(raw)
	if s == "" {
		return 0
	}
	if n, err := strconv.Atoi(s); err == nil && n > 0 {
		return n
	}
	if f, err := strconv.ParseFloat(s, 64); err == nil && f >= 1 {
		return int(f)
	}
	return 0
}

func extractIndexAndDisplay(item core.KindItem) (int, string) {
	effIndex := item.Index
	var rawDisplay string
	pick := func(raw string) bool {
		s := strings.TrimSpace(raw)
		if s == "" {
			return false
		}
		rawDisplay = s
		return true
	}
	for _, key := range []string{"indexDisplay", "seriesNumber", "series_number", "bookNumber", "book_number", "trackNumber", "track_number", "track", "number"} {
		raw, ok := item.Fields[key]
		if !ok || len(raw) == 0 {
			continue
		}
		trimmed := bytes.TrimSpace(raw)
		var s string
		if json.Unmarshal(trimmed, &s) == nil {
			if pick(s) {
				break
			}
			continue
		}
		var f float64
		if json.Unmarshal(trimmed, &f) == nil && f > 0 {
			if pick(strconv.FormatFloat(f, 'f', -1, 64)) {
				break
			}
		}
	}
	if rawDisplay == "" && len(item.Hints) > 0 {
		var h struct {
			IndexDisplay string `json:"indexDisplay"`
			SeriesNumber string `json:"seriesNumber"`
		}
		if json.Unmarshal(item.Hints, &h) == nil {
			if !pick(h.IndexDisplay) {
				pick(h.SeriesNumber)
			}
		}
	}
	if effIndex <= 0 && rawDisplay != "" {
		effIndex = parseLeadingPositiveInt(rawDisplay)
	}
	if rawDisplay != "" && effIndex > 0 && rawDisplay == strconv.Itoa(effIndex) {
		rawDisplay = ""
	}
	return effIndex, rawDisplay
}

func normalizeReadingFormat(raw string) string {
	s := strings.ToLower(strings.TrimSpace(raw))
	s = strings.TrimPrefix(s, ".")
	switch s {
	case "epub", "application/epub+zip":
		return "epub"
	case "pdf", "application/pdf":
		return "pdf"
	case "cbz", "application/vnd.comicbook+zip", "application/x-cbz":
		return "cbz"
	default:
		return ""
	}
}

func extractTitleReading(item core.KindItem) *core.TitleReading {
	var format string
	for _, key := range []string{"readingFormat", "format", "fileFormat", "file_format"} {
		raw, ok := item.Fields[key]
		if !ok || len(raw) == 0 {
			continue
		}
		var s string
		if json.Unmarshal(raw, &s) == nil {
			if f := normalizeReadingFormat(s); f != "" {
				format = f
				break
			}
		}
	}
	var pages int
	for _, key := range []string{"pages", "pageCount", "page_count"} {
		raw, ok := item.Fields[key]
		if !ok || len(raw) == 0 {
			continue
		}
		if n, err := parseJSONInt64(raw); err == nil && n > 0 {
			pages = int(n)
			break
		}
	}
	if len(item.Hints) > 0 && (format == "" || pages == 0) {
		var h struct {
			Format string `json:"format"`
			MIME   string `json:"mime"`
			Pages  int    `json:"pages"`
		}
		if json.Unmarshal(item.Hints, &h) == nil {
			if format == "" {
				if f := normalizeReadingFormat(h.Format); f != "" {
					format = f
				} else if f := normalizeReadingFormat(h.MIME); f != "" {
					format = f
				}
			}
			if pages == 0 && h.Pages > 0 {
				pages = h.Pages
			}
		}
	}
	if format == "" {
		return nil
	}
	return &core.TitleReading{Format: format, Pages: pages}
}

func extractPopularity(item core.KindItem) float64 {
	if len(item.Fields) == 0 {
		return 0
	}
	for _, key := range []string{"popularity", "fans", "listeners"} {
		raw, ok := item.Fields[key]
		if !ok || len(raw) == 0 {
			continue
		}
		trimmed := bytes.TrimSpace(raw)
		var f float64
		if json.Unmarshal(trimmed, &f) == nil && f > 0 {
			return f
		}
		if n, err := parseJSONInt64(trimmed); err == nil && n > 0 {
			return float64(n)
		}
	}
	return 0
}

func isBlockedFactField(fd plugins.KindField, hasReading bool) bool {
	if isPeopleIndexedField(fd) || fd.Type == "duration" {
		return true
	}
	rawKey := strings.TrimSpace(fd.Key)
	low := strings.ToLower(rawKey)
	if low == "id" || strings.HasSuffix(low, "_id") || strings.HasSuffix(rawKey, "Id") || strings.HasSuffix(rawKey, "ID") {
		return true
	}
	switch low {
	case "genre", "genres", "tag", "tags",
		"duration", "runtime", "length", "durationsec", "duration_sec",
		"contentrating", "certification", "mpaa",
		"parts",
		"disc", "discnumber", "disc_number", "discs", "disccount", "disc_count",
		"tracks", "trackcount", "track_count", "childcount", "child_count",
		"indexdisplay", "seriesnumber", "series_number", "booknumber", "book_number",
		"popularity", "fans", "listeners", "score", "rank",
		"upc", "ean", "isrc", "asin", "mbid", "tmdb", "imdb", "tvdb", "olid",
		"curation", "featured", "decade", "era",
		"artist", "artists", "albumartist", "album_artist", "album", "author", "authors", "narrator", "narrators", "series":
		return true
	case "format", "readingformat", "fileformat", "file_format", "pages", "pagecount", "page_count":
		return hasReading
	default:
		return false
	}
}

func extractMediaFacts(item core.KindItem, decl plugins.MediaKindMeta, resolve TextResolver, hasReading bool, seriesField string, credits []core.MediaCredit) []core.TitleFact {
	if len(item.Fields) == 0 || len(decl.Fields) == 0 {
		return nil
	}
	titleNorm := strings.ToLower(strings.TrimSpace(item.Title))
	creditNames := make(map[string]bool, len(credits))
	for _, c := range credits {
		if n := strings.ToLower(strings.TrimSpace(c.Name)); n != "" {
			creditNames[n] = true
		}
	}
	var facts []core.TitleFact
	for _, fd := range decl.Fields {
		// Opt-in only: surface fields explicitly marked as facts by the plugin.
		if fd.Fact == nil || !*fd.Fact {
			continue
		}
		if isBlockedFactField(fd, hasReading) || (seriesField != "" && fd.Key == seriesField) {
			continue
		}
		raw, ok := item.Fields[fd.Key]
		if !ok || len(raw) == 0 {
			continue
		}
		val := formatRawField(fd, raw)
		if val == "" {
			continue
		}
		valNorm := strings.ToLower(strings.TrimSpace(val))
		if (titleNorm != "" && valNorm == titleNorm) || creditNames[valNorm] {
			continue
		}
		label := plugins.ResolveTextValue(fd.Label, fd.Key, resolve)
		facts = append(facts, core.TitleFact{Label: label, Value: val})
	}
	return facts
}

func formatRawField(fd plugins.KindField, raw json.RawMessage) string {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return ""
	}
	switch fd.Type {
	case "string", "enum":
		var s string
		if json.Unmarshal(trimmed, &s) == nil {
			return strings.TrimSpace(s)
		}
	case "date":
		if n, err := parseJSONInt64(trimmed); err == nil {
			if n > 86400 {
				return time.Unix(n, 0).UTC().Format("2006-01-02")
			}
			return strconv.FormatInt(n, 10)
		}
		var s string
		if json.Unmarshal(trimmed, &s) == nil {
			return strings.TrimSpace(s)
		}
	case "int", "duration":
		if n, err := parseJSONInt64(trimmed); err == nil {
			return strconv.FormatInt(n, 10)
		}
	case "float":
		var v float64
		if json.Unmarshal(trimmed, &v) == nil {
			return strconv.FormatFloat(v, 'f', -1, 64)
		}
	case "bool":
		var b bool
		if json.Unmarshal(trimmed, &b) == nil {
			if b {
				return "Yes"
			}
			return "No"
		}
	case "strings":
		var arr []string
		if json.Unmarshal(trimmed, &arr) == nil {
			var clean []string
			for _, s := range arr {
				if s = strings.TrimSpace(s); s != "" {
					clean = append(clean, s)
				}
			}
			return strings.Join(clean, ", ")
		}
	case "people":
		var arr []string
		if json.Unmarshal(trimmed, &arr) == nil {
			return strings.Join(arr, ", ")
		}
		var objs []struct {
			Name string `json:"name"`
		}
		if json.Unmarshal(trimmed, &objs) == nil {
			var names []string
			for _, o := range objs {
				if n := strings.TrimSpace(o.Name); n != "" {
					names = append(names, n)
				}
			}
			return strings.Join(names, ", ")
		}
	}
	return ""
}

func extractDurationSeconds(fields map[string]json.RawMessage, decl plugins.MediaKindMeta) int64 {
	if len(fields) == 0 {
		return 0
	}
	for _, fd := range decl.Fields {
		if fd.Type == "duration" {
			if raw, ok := fields[fd.Key]; ok {
				if n, err := parseJSONInt64(raw); err == nil && n > 0 {
					return n
				}
			}
		}
	}
	for _, key := range []string{"duration", "runtime", "length"} {
		if raw, ok := fields[key]; ok {
			if n, err := parseJSONInt64(raw); err == nil && n > 0 {
				return n
			}
		}
	}
	return 0
}

func extractGenresAndFacets(item core.KindItem, decl plugins.MediaKindMeta) []string {
	if len(item.Fields) == 0 {
		return nil
	}
	seen := make(map[string]bool)
	var out []string
	add := func(s string) {
		s = strings.TrimSpace(s)
		if s == "" {
			return
		}
		low := strings.ToLower(s)
		if seen[low] {
			return
		}
		seen[low] = true
		out = append(out, s)
	}

	for _, v := range extractStringsFromFields(item.Fields, "genre", "genres", "tags", "tag") {
		add(v)
	}

	// When the kind declares an explicit genre/tag field, keep Title.Genres
	// focused on those rather than mixing in people (artists/authors) or
	// structural enums (e.g. record_type) that happen to be browse facets.
	fieldType := make(map[string]string, len(decl.Fields))
	hasDeclaredGenre := false
	for _, fd := range decl.Fields {
		fieldType[fd.Key] = fd.Type
		switch fd.Key {
		case "genre", "genres", "tags", "tag":
			hasDeclaredGenre = true
		}
	}
	if hasDeclaredGenre {
		return out
	}

	facetKeys := append([]string(nil), decl.Browse.Facets...)
	sort.Strings(facetKeys)
	for _, key := range facetKeys {
		switch fieldType[key] {
		case "people", "int", "float", "date", "duration", "bool":
			continue
		}
		switch strings.ToLower(key) {
		case "studio", "studios", "publisher", "publishers", "network", "artist", "artists", "album_artist", "album_artists", "author", "authors", "performer", "performers":
			continue
		}
		for _, v := range extractStringsFromFields(item.Fields, key) {
			add(v)
		}
	}
	return out
}

func extractStringsFromFields(fields map[string]json.RawMessage, keys ...string) []string {
	if len(fields) == 0 {
		return nil
	}
	var out []string
	for _, k := range keys {
		raw, ok := fields[k]
		if !ok || len(raw) == 0 {
			continue
		}
		var s string
		if err := json.Unmarshal(raw, &s); err == nil {
			if s = strings.TrimSpace(s); s != "" {
				out = append(out, s)
			}
			continue
		}
		var arr []string
		if err := json.Unmarshal(raw, &arr); err == nil {
			for _, elem := range arr {
				if elem = strings.TrimSpace(elem); elem != "" {
					out = append(out, elem)
				}
			}
		}
	}
	return out
}

func cleanPersonThumb(raw string) string {
	u, err := validatePersonThumb(raw)
	if err != nil {
		return ""
	}
	return u
}

func extractPeopleFromFields(fields map[string]json.RawMessage, keys ...string) []core.Person {
	if len(fields) == 0 {
		return nil
	}
	var out []core.Person
	for _, k := range keys {
		raw, ok := fields[k]
		if !ok || len(raw) == 0 {
			continue
		}
		var s string
		if err := json.Unmarshal(raw, &s); err == nil {
			if s = strings.TrimSpace(s); s != "" {
				out = append(out, core.Person{TMDB: core.PluginPersonID(s), Name: s})
			}
			continue
		}
		var arr []string
		if err := json.Unmarshal(raw, &arr); err == nil {
			for _, name := range arr {
				if name = strings.TrimSpace(name); name != "" {
					out = append(out, core.Person{TMDB: core.PluginPersonID(name), Name: name})
				}
			}
			continue
		}
		var objs []struct {
			Name  string `json:"name"`
			Role  string `json:"role,omitempty"`
			Thumb string `json:"thumb,omitempty"`
		}
		if err := json.Unmarshal(raw, &objs); err == nil {
			for _, o := range objs {
				if name := strings.TrimSpace(o.Name); name != "" {
					out = append(out, core.Person{
						TMDB:      core.PluginPersonID(name),
						Name:      name,
						Role:      strings.TrimSpace(o.Role),
						ThumbPath: cleanPersonThumb(o.Thumb),
					})
				}
			}
		}
	}
	return out
}
