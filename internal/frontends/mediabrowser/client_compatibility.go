package mediabrowser

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/braedonsaunders/ether/internal/contentrating"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/kit/ids"
	"github.com/braedonsaunders/ether/internal/frontend/kit/route"
	kitsort "github.com/braedonsaunders/ether/internal/frontend/kit/sort"
)

func (i *instance) clientCompatibilityRoutes() error {
	for _, entry := range []struct {
		path    string
		handler http.HandlerFunc
	}{
		{"/Items/Suggestions", i.suggestions},
		{"/Users/{userId}/Items/Suggestions", i.suggestions},
		{"/Items/Filters", i.queryFilters}, {"/Items/Filters2", i.queryFilters},
		{"/Shows/Upcoming", i.upcomingEpisodes},
		{"/Videos/{id}/AdditionalParts", i.additionalParts},
		{"/Items/{id}/theme.m4a", i.themeStream},
		{"/Items/{id}/ThemeMedia", i.themeMedia}, {"/Items/{id}/ThemeSongs", i.themeMedia}, {"/Items/{id}/ThemeVideos", i.themeMedia},
		{"/Items/{id}/PlaybackInfo", i.playbackInfo},
		{"/Localization/Options", i.emptyOptionalArray},
		{"/System/WakeOnLanInfo", i.emptyOptionalArray},
		{"/Users/{userId}/HomeSections", i.homeSections},
		{"/Artists", i.artists(false)}, {"/Artists/AlbumArtists", i.artists(true)},
		{"/Artists/{name}", i.artistByName},
		{"/Items/{id}/InstantMix", i.emptyItemCompanions},
		{"/LiveTv/Timers", i.emptyOptionalItems}, {"/LiveTv/SeriesTimers", i.emptyOptionalItems},
		{"/LiveTv/Recordings/Folders", i.emptyOptionalItems},
		{"/Years", i.years},
		{"/Years/{year}", i.year},
		{"/UserItems/{id}/UserData", i.userItemData},
	} {
		if err := i.router.Handle("GET", entry.path, entry.handler); err != nil {
			return err
		}
	}
	if i.protocol == "jellyfin" {
		if err := i.router.Handle("GET", "/System/Configuration/Encoding", http.HandlerFunc(i.encodingOptions)); err != nil {
			return err
		}
		if err := i.router.Handle("POST", "/ClientLog/Document", http.HandlerFunc(i.clientLogDocument)); err != nil {
			return err
		}
	}
	return i.router.Handle("DELETE", "/Videos/ActiveEncodings", http.HandlerFunc(i.stopEncoding))
}

// encodingOptions exposes Jellyfin's encoding configuration contract with
// software defaults. Ether manages its encoders through its own settings.
func (i *instance) encodingOptions(w http.ResponseWriter, r *http.Request) {
	if v, _, err := i.viewer(r); err != nil || v == nil {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	jsonReply(w, map[string]any{
		"EncodingThreadCount":                -1,
		"EnableFallbackFont":                 false,
		"EnableAudioVbr":                     false,
		"DownMixAudioBoost":                  2,
		"DownMixStereoAlgorithm":             "None",
		"MaxMuxingQueueSize":                 2048,
		"EnableThrottling":                   false,
		"ThrottleDelaySeconds":               180,
		"EnableSegmentDeletion":              false,
		"SegmentKeepSeconds":                 720,
		"HardwareAccelerationType":           "none",
		"EnableTonemapping":                  false,
		"EnableVppTonemapping":               false,
		"EnableVideoToolboxTonemapping":      false,
		"TonemappingAlgorithm":               "bt2390",
		"TonemappingMode":                    "auto",
		"TonemappingRange":                   "auto",
		"TonemappingDesat":                   0,
		"TonemappingPeak":                    100,
		"TonemappingParam":                   0,
		"VppTonemappingBrightness":           16,
		"VppTonemappingContrast":             1,
		"H264Crf":                            23,
		"H265Crf":                            28,
		"DeinterlaceDoubleRate":              false,
		"DeinterlaceMethod":                  "yadif",
		"EnableDecodingColorDepth10Hevc":     true,
		"EnableDecodingColorDepth10Vp9":      true,
		"EnableDecodingColorDepth10HevcRext": false,
		"EnableDecodingColorDepth12HevcRext": false,
		"EnableEnhancedNvdecDecoder":         true,
		"PreferSystemNativeHwDecoder":        true,
		"EnableIntelLowPowerH264HwEncoder":   false,
		"EnableIntelLowPowerHevcHwEncoder":   false,
		"EnableHardwareEncoding":             false,
		"AllowHevcEncoding":                  false,
		"AllowAv1Encoding":                   false,
		"EnableSubtitleExtraction":           true,
		"HardwareDecodingCodecs":             []string{},
		"AllowOnDemandMetadataBasedKeyframeExtractionForExtensions": []string{"mkv"},
	})
}

// clientLogDocument accepts a client log upload the way Jellyfin 10.10
// does: authenticated, at most 1,000,000 bytes, answered with the stored
// file name. Ether keeps no client logs, so the body is drained and
// discarded; the name follows Jellyfin's client-log naming so clients
// that surface it show something sensible.
func (i *instance) clientLogDocument(w http.ResponseWriter, r *http.Request) {
	v, _, err := i.viewer(r)
	if err != nil || v == nil {
		http.Error(w, "Unauthorized", 401)
		return
	}
	const maxClientLogDocument = 1_000_000
	if r.ContentLength > maxClientLogDocument {
		http.Error(w, "Payload must be less than 1,000,000 bytes", http.StatusRequestEntityTooLarge)
		return
	}
	if _, err := io.Copy(io.Discard, http.MaxBytesReader(w, r.Body, maxClientLogDocument)); err != nil {
		http.Error(w, "Payload must be less than 1,000,000 bytes", http.StatusRequestEntityTooLarge)
		return
	}
	client := device(r).Client
	if client == "" {
		client = "unknown-client"
	}
	name := "clientlog-" + clientLogNamePart(client) + "-" + time.Now().UTC().Format("20060102150405") + ".log"
	jsonReply(w, map[string]any{"FileName": name})
}

func clientLogNamePart(client string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(client) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case b.Len() > 0:
			b.WriteByte('-')
		}
		if b.Len() >= 40 {
			break
		}
	}
	if s := strings.Trim(b.String(), "-"); s != "" {
		return s
	}
	return "client"
}

func (i *instance) queryFilters(w http.ResponseWriter, r *http.Request) {
	v, _, err := i.viewer(r)
	if err != nil || v == nil {
		http.Error(w, "Unauthorized", 401)
		return
	}
	if i.host.Experience == nil {
		http.Error(w, "Unavailable", 503)
		return
	}
	// A cold aggregate or unavailable index must not hold a library page.
	// All five indexed lookups share one small request budget.
	ctx, cancel := context.WithTimeout(r.Context(), 250*time.Millisecond)
	defer cancel()
	// Read each facet independently: an absent optional facet must not hide
	// the filters that are available. Production implements the indexed reader;
	// embedded hosts can still supply their bounded Facets implementation.
	read := func(kind string) []frontend.FacetValue {
		var values []frontend.FacetValue
		var err error
		if reader, ok := i.host.Experience.(frontend.FilterFacetReader); ok {
			values, err = reader.FilterFacets(ctx, v, device(r), facetView(r), kind)
		} else {
			values, err = i.host.Experience.Facets(ctx, v, device(r), facetView(r), kind)
		}
		if err != nil {
			i.logFacetFailure(kind, err)
			return nil
		}
		return values
	}
	genres, studios := read("genre"), read("studio")
	genreNames, studioNames, tags, ratings := []string{}, []string{}, []string{}, []string{}
	genreItems, studioItems := []any{}, []any{}
	for _, value := range genres {
		genreNames = append(genreNames, value.Title)
		genreItems = append(genreItems, map[string]any{"Name": value.Title, "Id": value.ID})
	}
	for _, value := range studios {
		studioNames = append(studioNames, value.Title)
		studioItems = append(studioItems, map[string]any{"Name": value.Title, "Id": value.ID})
	}
	for _, value := range read("tag") {
		tags = append(tags, value.Title)
	}
	for _, value := range read("contentRating") {
		ratings = append(ratings, contentrating.Prefixed(value.Title))
	}
	years := []int{}
	for _, value := range read("year") {
		if n, err := strconv.Atoi(value.Title); err == nil {
			years = append(years, n)
		}
	}
	result := map[string]any{"Genres": genreNames, "Studios": studioNames, "Tags": tags, "OfficialRatings": ratings, "Years": years}
	if strings.HasSuffix(strings.ToLower(r.URL.Path), "/items/filters2") {
		result["Genres"], result["Studios"] = genreItems, studioItems
		result["AudioLanguages"], result["SubtitleLanguages"] = []any{}, []any{}
	}
	jsonReply(w, result)
}

func (i *instance) upcomingEpisodes(w http.ResponseWriter, r *http.Request) {
	v, _, err := i.viewer(r)
	if err != nil || v == nil {
		http.Error(w, "Unauthorized", 401)
		return
	}
	if i.host.Experience == nil {
		http.Error(w, "Unavailable", 503)
		return
	}
	q := itemQuery(r)
	q.Types = []frontend.MediaType{"episode"}
	q.Recursive = true
	q.Sort, _ = kitsort.Order("PremiereDate")
	q.Desc = false
	now := time.Now().UTC().Truncate(24 * time.Hour)
	q.Filter.ReleasedFilters = []frontend.DateFilter{{Op: ">=", Unix: now.AddDate(0, 0, -1).Unix()}}
	if len(route.Query(r.URL.Query(), "Limit")) == 0 || q.Limit <= 0 {
		q.Limit = upcomingEpisodesMax
	} else if q.Limit > upcomingEpisodesMax {
		q.Limit = upcomingEpisodesMax
	}
	var page frontend.Page
	if parent := route.Query(r.URL.Query(), "parentId"); len(parent) > 0 {
		if len(parent) != 1 || parent[0] == "" {
			http.Error(w, "Bad Request", 400)
			return
		}
		// Jellyfin clients (Android TV's series page) scope Upcoming to the
		// series item id. A view id scopes it to that library instead.
		if key, e := ids.ParseRatingKeyGUID(parent[0]); e == nil {
			page, err = i.host.Experience.Children(r.Context(), v, device(r), key, q)
			if err == nil {
				i.nameChildSeries(r, v, key, &page)
			}
		} else if n, e := ids.ParseGUID(ids.View, parent[0]); e == nil {
			page, err = i.host.Experience.LibraryItems(r.Context(), v, device(r), ids.Decimal(n), q)
		} else {
			http.NotFound(w, r)
			return
		}
	} else {
		page, err = i.host.Experience.LibraryItems(r.Context(), v, device(r), "", q)
	}
	if err != nil {
		if errors.Is(err, frontend.ErrNotFound) {
			i.replyItems(w, r, i.queryResult([]any{}, 0, q.Offset))
			return
		}
		http.Error(w, "Unavailable", 503)
		return
	}
	i.page(w, r, v, page, q.Offset)
}

// upcomingEpisodesMax bounds one /Shows/Upcoming page. The premiere-date
// filter and sort run on the catalogue's release index, so the bound is a
// page size, not a scan limit.
const upcomingEpisodesMax = 100

func (i *instance) additionalParts(w http.ResponseWriter, r *http.Request) {
	if _, ok := i.authorizedItem(w, r); !ok {
		return
	}
	// Ether sources are complete editions, with no stacked additional files.
	jsonReply(w, i.queryResult([]any{}, 0, 0))
}

func (i *instance) themeStream(w http.ResponseWriter, r *http.Request) {
	d, ok := i.authorizedTheme(w, r)
	if !ok {
		return
	}
	if i.host.Themes == nil {
		http.NotFound(w, r)
		return
	}
	i.host.Themes.ServeTheme(w, r, d.Title)
}

// themeStreamPath names the static stream with the cached container's
// extension (mp3 from Plex's theme service, m4a from the fallback).
func themeStreamPath(themeID, container string) string {
	return "/Audio/" + themeID + "/stream." + container
}

func (i *instance) themeSourceDTO(themeID, container string) map[string]any {
	path := themeStreamPath(themeID, container)
	codec := "aac"
	if container == "mp3" {
		codec = "mp3"
	}
	media := frontend.MediaInfo{
		Container: container,
		Estimated: true,
		Streams: []frontend.StreamInfo{
			{Index: 0, Type: "audio", Codec: codec, Channels: 2, Default: true},
		},
	}
	source := i.mediaSourceDTO(themeID, media)
	source["Path"] = path
	source["Protocol"] = "Http"
	source["SupportsDirectPlay"] = true
	source["SupportsDirectStream"] = true
	source["SupportsTranscoding"] = false
	source["IsRemote"] = false
	if i.protocol == "emby" {
		source["DirectStreamUrl"] = path
		i.embyMediaSourceDTO(source, themeID, media)
		delete(source, "ETag")
	}
	i.mediaSourceDialect(source)
	return source
}

func (i *instance) themeMedia(w http.ResponseWriter, r *http.Request) {
	d, ok := i.authorizedTheme(w, r)
	if !ok {
		return
	}
	ownerID := ids.RatingKeyGUID(d.Title.Key)
	empty := i.queryResult([]any{}, 0, 0)
	empty["OwnerId"] = ownerID
	songs := empty
	container := ""
	if !strings.HasSuffix(strings.ToLower(r.URL.Path), "/themevideos") && i.host.Themes != nil {
		container = i.host.Themes.Theme(d.Title, true)
	}
	if container != "" {
		themeID := themeWireID(d.Title.Key)
		source := i.themeSourceDTO(themeID, container)
		path := themeStreamPath(themeID, container)
		songs = i.queryResult([]any{map[string]any{
			"Id":                themeID,
			"ServerId":          i.info.ID,
			"Name":              d.Title.Title,
			"Type":              "Audio",
			"MediaType":         "Audio",
			"Container":         container,
			"IsFolder":          false,
			"Path":              path,
			"UserData":          i.userDataDTO(themeID, frontend.WatchStatus{}),
			"ImageTags":         map[string]string{},
			"BackdropImageTags": []string{},
			"MediaSources":      []any{source},
			"MediaStreams":      source["MediaStreams"],
		}}, 1, 0)
		songs["OwnerId"] = ownerID
	}
	if strings.HasSuffix(strings.ToLower(r.URL.Path), "/thememedia") {
		jsonReply(w, map[string]any{"ThemeSongsResult": songs, "ThemeVideosResult": empty, "SoundtrackSongsResult": empty})
		return
	}
	jsonReply(w, songs)
}

// Stop only the authenticated device's broker session; a client-supplied
// deviceId must never select another viewer's encoder.
func (i *instance) stopEncoding(w http.ResponseWriter, r *http.Request) {
	v, _, err := i.viewer(r)
	if err != nil || v == nil {
		http.Error(w, "Unauthorized", 401)
		return
	}
	dev := device(r)
	devices := route.Query(r.URL.Query(), "deviceId")
	sessions := route.Query(r.URL.Query(), "playSessionId")
	if len(devices) != 1 || devices[0] == "" || len(sessions) != 1 || sessions[0] == "" {
		http.Error(w, "Bad Request", 400)
		return
	}
	// Jellyfin answers 204 whatever deviceId names: it only kills matching
	// jobs. Jellyfin for Android's native player sends an id that differs
	// from its WebView's header, and its SDK crashes the app on a 403. The
	// stop below stays scoped to the caller's own viewer and device.
	if dev.ID == "" {
		http.Error(w, "Forbidden", 403)
		return
	}
	if i.host.Sessions == nil {
		http.Error(w, "Unavailable", 503)
		return
	}
	err = i.host.Sessions.Stop(r.Context(), frontend.SessionAccess{Viewer: v, Device: dev, Protocol: i.protocol}, sessions[0])
	if err != nil && !errors.Is(err, frontend.ErrNotFound) {
		http.Error(w, "Unavailable", 503)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// No music libraries or DVR scheduler are advertised by this surface. These
// optional collection reads still have the standard empty query-result shape.
func (i *instance) emptyOptionalItems(w http.ResponseWriter, r *http.Request) {
	if v, _, err := i.viewer(r); err != nil || v == nil {
		http.Error(w, "Unauthorized", 401)
		return
	}
	jsonReply(w, i.queryResult([]any{}, 0, queryInt(r, "StartIndex", 0)))
}
func (i *instance) emptyItemCompanions(w http.ResponseWriter, r *http.Request) {
	if _, ok := i.authorizedItem(w, r); !ok {
		return
	}
	jsonReply(w, i.queryResult([]any{}, 0, 0))
}

// homeSections is the Emby home layout the native apps request at startup
// (GET /Users/{userId}/HomeSections, Emby Server 4.10+). Ether has no
// per-user section editor, so it answers the standard layout: library
// tiles, Continue Watching, Next Up, then Latest Media per visible library.
// Known section types carry no DisplayText; the apps label them with their
// own localized strings. The viewer scope comes from i.viewer, which also
// rejects a {userId} that is not the caller.
func (i *instance) homeSections(w http.ResponseWriter, r *http.Request) {
	v, _, err := i.viewer(r)
	if err != nil || v == nil {
		http.Error(w, "Unauthorized", 401)
		return
	}
	if i.host.Experience == nil {
		http.Error(w, "Unavailable", 503)
		return
	}
	views, err := i.host.Experience.Views(r.Context(), v)
	if err != nil {
		http.Error(w, "Unavailable", 503)
		return
	}
	items := []any{}
	add := func(sectionType, collectionID string) {
		section := map[string]any{"Id": homeSectionID(sectionType, collectionID), "SectionType": sectionType}
		if collectionID != "" {
			section["CollectionId"] = collectionID
		}
		items = append(items, section)
	}
	var latestExcludedGUIDs map[string]bool
	var redundant map[string]bool
	if stored, ok := i.storedUserConfig(r, v); ok {
		if raw, exists := stored["LatestItemsExcludes"]; exists {
			if list, ok := raw.([]any); ok {
				latestExcludedGUIDs = make(map[string]bool, len(list))
				for _, item := range list {
					if s, ok := item.(string); ok && s != "" {
						latestExcludedGUIDs[strings.ToLower(strings.ReplaceAll(s, "-", ""))] = true
					}
				}
			}
		} else {
			hidePlayed := true
			if hp, ok := stored["HidePlayedInLatest"].(bool); ok {
				hidePlayed = hp
			}
			redundant = i.redundantSubsetViews(r, v, views, hidePlayed)
		}
	} else {
		redundant = i.redundantSubsetViews(r, v, views, true)
	}
	add("SmallLibraryTiles", "")
	add("Resume", "")
	add("NextUp", "")
	for _, view := range views {
		if view.Type == "livetv" || view.Type == "boxsets" {
			continue
		}
		n, err := ids.ParseDecimal(view.ID)
		if err != nil || ids.PersonalView(n) {
			continue
		}
		if view.Type != "movie" && view.Type != "show" && view.KindKey == "" {
			continue
		}
		if redundant[view.ID] {
			continue
		}
		id, err := ids.GUID(ids.View, n)
		if err != nil {
			continue
		}
		if latestExcludedGUIDs[strings.ToLower(strings.ReplaceAll(id, "-", ""))] {
			continue
		}
		add("LatestMedia", id)
	}
	jsonReply(w, i.queryResult(items, len(items), 0))
}

// homeSectionID is a stable GUID-shaped id for one standard section. It is
// not an item id; nothing in Ether resolves it.
func homeSectionID(sectionType, collectionID string) string {
	sum := sha256.Sum256([]byte("ether.homesection." + sectionType + "." + collectionID))
	return hex.EncodeToString(sum[:16])
}

func (i *instance) emptyOptionalArray(w http.ResponseWriter, r *http.Request) {
	if v, _, err := i.viewer(r); err != nil || v == nil {
		http.Error(w, "Unauthorized", 401)
		return
	}
	jsonReply(w, []any{})
}

// years lists the production years a filter can offer, from the same year
// facet /Items/Filters reads. Jellyfin Web and Swiftfin ask GET /Years to build
// their year filter; it must exist, not only the embedded Filters list.
func (i *instance) years(w http.ResponseWriter, r *http.Request) {
	v, _, err := i.viewer(r)
	if err != nil || v == nil {
		http.Error(w, "Unauthorized", 401)
		return
	}
	if i.host.Experience == nil {
		http.Error(w, "Unavailable", 503)
		return
	}
	facets, err := i.host.Experience.Facets(r.Context(), v, device(r), facetView(r), "year")
	if err != nil {
		http.Error(w, "Unavailable", 503)
		return
	}
	offset, limit := queryInt(r, "StartIndex", 0), queryInt(r, "Limit", 50)
	if limit <= 0 {
		limit = 50
	}
	items := make([]any, 0, len(facets))
	for _, facet := range facets {
		n, e := strconv.Atoi(facet.Title)
		if e != nil {
			continue
		}
		id, e := ids.GUID(ids.View, int64(n))
		if e != nil {
			continue
		}
		items = append(items, map[string]any{"Name": facet.Title, "Id": id, "Type": "Year", "ServerId": i.info.ID, "ProductionYear": n})
	}
	start := min(max(offset, 0), len(items))
	end := min(start+limit, len(items))
	jsonReply(w, i.queryResult(items[start:end], len(items), offset))
}

// year answers GET /Years/{year} with the year item a client opened. Its items
// are reachable through /Items?Years=, which clients use for the listing.
func (i *instance) year(w http.ResponseWriter, r *http.Request) {
	v, _, err := i.viewer(r)
	if err != nil || v == nil {
		http.Error(w, "Unauthorized", 401)
		return
	}
	raw := r.PathValue("year")
	n, err := strconv.Atoi(raw)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	id, err := ids.GUID(ids.View, int64(n))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	jsonReply(w, map[string]any{"Name": raw, "Id": id, "Type": "Year", "ServerId": i.info.ID, "ProductionYear": n, "IsFolder": true})
}

// userItemData answers GET /UserItems/{id}/UserData, which Jellyfin Web reads
// for an item's played, favorite and resume state outside a full detail load.
func (i *instance) userItemData(w http.ResponseWriter, r *http.Request) {
	v, _, err := i.viewer(r)
	if err != nil || v == nil {
		http.Error(w, "Unauthorized", 401)
		return
	}
	if i.host.Experience == nil {
		http.Error(w, "Unavailable", 503)
		return
	}
	key, err := ids.ParseRatingKeyGUID(r.PathValue("id"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if _, err := i.host.Experience.Item(i.identityContext(r), frontend.ItemRequest{Viewer: v, Device: device(r), Key: key}); err != nil {
		http.NotFound(w, r)
		return
	}
	idsWire := ids.RatingKeyGUID(key)
	state, err := i.host.Experience.UserData(i.identityContext(r), v, []frontend.RatingKey{key})
	if err != nil {
		http.Error(w, "Unavailable", 503)
		return
	}
	jsonReply(w, i.userDataDTO(idsWire, state[key]))
}

func (i *instance) themePlaybackInfo(w http.ResponseWriter, r *http.Request) {
	wireID := r.PathValue("id")
	r.SetPathValue("id", themeParentID(wireID))
	d, ok := i.authorizedTheme(w, r)
	if !ok {
		return
	}
	container := ""
	if i.host.Themes != nil {
		container = i.host.Themes.Theme(d.Title, false)
	}
	if container == "" {
		http.NotFound(w, r)
		return
	}
	jsonReply(w, map[string]any{"MediaSources": []any{i.themeSourceDTO(wireID, container)}})
}

// Negative item GUIDs reserve a stateless audio identity separate from the
// positive catalogue key, while remaining UUID-shaped for native clients.
func themeWireID(key frontend.RatingKey) string { return ids.RatingKeyGUID(-key) }
func themeParentID(id string) string {
	key, err := ids.ParseRatingKeyGUID(id)
	if err != nil || key >= 0 {
		return ""
	}
	parent := -key
	switch parent.Kind() {
	case frontend.KindShow, frontend.KindSeason, frontend.KindEpisode:
		return ids.RatingKeyGUID(parent)
	}
	return ""
}

func (i *instance) authorizedTheme(w http.ResponseWriter, r *http.Request) (frontend.ItemDetail, bool) {
	viewer, _, err := i.viewer(r)
	if err != nil || viewer == nil {
		http.Error(w, "Unauthorized", 401)
		return frontend.ItemDetail{}, false
	}
	key, err := ids.ParseRatingKeyGUID(r.PathValue("id"))
	if err != nil || i.host.ThemeTitle == nil {
		http.NotFound(w, r)
		return frontend.ItemDetail{}, false
	}
	title, err := i.host.ThemeTitle(i.identityContext(r), viewer, key)
	if err != nil {
		http.NotFound(w, r)
		return frontend.ItemDetail{}, false
	}
	return frontend.ItemDetail{Title: *title}, true
}
