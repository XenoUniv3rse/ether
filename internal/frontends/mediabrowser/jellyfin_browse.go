package mediabrowser

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/kit/authhdr"
	"github.com/braedonsaunders/ether/internal/frontend/kit/ids"
	"github.com/braedonsaunders/ether/internal/frontend/kit/libpath"
	"github.com/braedonsaunders/ether/internal/frontend/kit/route"
)

const (
	specialNone specialItems = iota
	specialBoxSets
	specialPlaylists
)

type specialItems int

// requestedSpecialItems reports a library query that is only box sets or only
// playlists. Mixed type queries stay on the title browse path.
func requestedSpecialItems(r *http.Request) specialItems {
	values := route.Query(r.URL.Query(), "IncludeItemTypes")
	if len(values) == 0 {
		return specialNone
	}
	box, playlist, other := false, false, false
	for _, value := range values {
		for _, typ := range strings.Split(value, ",") {
			switch strings.ToLower(strings.TrimSpace(typ)) {
			case "", "userview":
			case "boxset":
				box = true
			case "playlist":
				playlist = true
			default:
				other = true
			}
		}
	}
	if other || (box && playlist) {
		return specialNone
	}
	if box {
		return specialBoxSets
	}
	if playlist {
		return specialPlaylists
	}
	return specialNone
}

func splitCSV(value string) []string {
	parts := strings.Split(value, ",")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part != "" {
			out = append(out, part)
		}
	}
	return out
}

// firstListValue returns the first non-empty entry of a query list that may
// be comma-joined, repeated, or both.
func firstListValue(values []string) string {
	for _, value := range values {
		if parts := splitCSV(value); len(parts) > 0 {
			return parts[0]
		}
	}
	return ""
}

// guidLike reports whether id parses as a UUID the way the Jellyfin
// Kotlin SDK does: 32 lowercase hex digits, optionally dashed 8-4-4-4-12.
// jellyfin-sdk-kotlin's UUIDSerializer rejects uppercase hex with an
// uncaught IllegalArgumentException.
func guidLike(id string) bool {
	if len(id) == 36 && id[8] == '-' && id[13] == '-' && id[18] == '-' && id[23] == '-' {
		id = id[:8] + id[9:13] + id[14:18] + id[19:23] + id[24:]
	}
	if len(id) != 32 {
		return false
	}
	for k := 0; k < len(id); k++ {
		c := id[k]
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return false
		}
	}
	return true
}

func dashedGUID(id string) string {
	if len(id) != 32 {
		return id
	}
	return id[0:8] + "-" + id[8:12] + "-" + id[12:16] + "-" + id[16:20] + "-" + id[20:]
}

func itemEtag(id string) string {
	sum := sha256.Sum256([]byte("jellyfin-item:" + id))
	return hex.EncodeToString(sum[:16])
}

func setMissing(m map[string]any, key string, value any) {
	if _, ok := m[key]; !ok {
		m[key] = value
	}
}

func fieldSet(r *http.Request) map[string]bool {
	out := map[string]bool{}
	if r == nil {
		return out
	}
	for _, value := range route.Query(r.URL.Query(), "Fields") {
		for _, field := range splitCSV(value) {
			out[strings.ToLower(field)] = true
		}
	}
	return out
}

func jellyfinSeriesStatus(status string) string {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "ended", "canceled", "cancelled":
		return "Ended"
	case "returning series", "in production", "planned", "continuing":
		return "Continuing"
	default:
		return status
	}
}

// jellyfinCollectionFolder fills the CollectionFolder shell Jellyfin 12.4
// returns for every user view. Dates and paths stay absent. Artwork is attached
// separately when a library collage can be issued. ChildCount is the viewer's
// visible library total, set by the views handler.
func (i *instance) jellyfinCollectionFolder(item map[string]any, id string) {
	item["ChannelId"] = nil
	item["BackdropImageTags"] = []string{}
	item["ImageTags"] = map[string]string{}
	item["ImageBlurHashes"] = map[string]any{}
	item["Chapters"] = []any{}
	item["ExternalUrls"] = []any{}
	item["GenreItems"] = []any{}
	item["Genres"] = []string{}
	item["People"] = []any{}
	item["RemoteTrailers"] = []any{}
	item["Studios"] = []any{}
	item["Taglines"] = []string{}
	item["Tags"] = []string{}
	item["LockData"] = false
	item["LockedFields"] = []string{}
	item["LocalTrailerCount"] = 0
	item["SpecialFeatureCount"] = 0
	item["MediaType"] = "Unknown"
	item["LocationType"] = "FileSystem"
	item["PlayAccess"] = "Full"
	item["EnableMediaSourceDisplay"] = true
	item["Etag"] = itemEtag(id)
	data := i.userDataDTO(id, frontend.WatchStatus{})
	delete(data, "PlayedPercentage")
	item["UserData"] = data
}

type jellyfinListFileKey struct{}

// withJellyfinListFiles marks a library, resume or home row. Item detail keeps
// an estimated source without an invented container, path or stream list.
func withJellyfinListFiles(r *http.Request) *http.Request {
	if r == nil || r.Context().Value(jellyfinListFileKey{}) != nil {
		return r
	}
	// Lists join stored artwork facts and let hashing finish behind the read.
	// A 100ms hashing wait alone would consume the entire home-row budget.
	ctx := withListVersionScope(frontend.WithoutSynchronousArtworkMetadata(r.Context()))
	return r.WithContext(context.WithValue(ctx, jellyfinListFileKey{}, true))
}

func jellyfinListFile(r *http.Request) bool {
	if r == nil {
		return false
	}
	on, _ := r.Context().Value(jellyfinListFileKey{}).(bool)
	return on
}

// presentJellyfinFile gives a movie or episode row the file description that
// static clients read before asking for PlaybackInfo.
func (i *instance) presentJellyfinFile(r *http.Request, item, source map[string]any, title frontend.Title) {
	if mbDeclaredNonVideo(title) || (title.Type != "movie" && title.Type != "episode") {
		return
	}
	path, _ := item["Path"].(string)
	i.completeFileSource(r, source, title, path)
	if path == "" {
		item["Path"] = source["Path"]
	}
	item["Container"] = source["Container"]
	source["VideoType"] = "VideoFile"
}

func jellyfinLibraryPath(title frontend.Title, container string) string {
	ext := container
	if ext == "" || strings.ContainsAny(ext, `/\.`) {
		ext = "mkv"
	}
	switch title.Type {
	case "movie":
		name := libpath.Safe(title.Title)
		if title.Year > 0 {
			name = fmt.Sprintf("%s (%d)", name, title.Year)
		}
		return "/Movies/" + name + "/" + name + "." + ext
	case "episode":
		show := libpath.Safe(title.ShowTitle)
		if title.ShowTitle == "" {
			show = "Show"
		}
		file := fmt.Sprintf("%s - S%02dE%02d.%s", show, title.Season, title.Episode, ext)
		return "/TV/" + show + "/" + fmt.Sprintf("Season %02d", title.Season) + "/" + file
	default:
		return ""
	}
}

func jellyfinMediaSourceShape(source map[string]any, name string) {
	if name != "" {
		setMissing(source, "Name", name)
	}
	// A source whose probed streams hold no video (a music track, an
	// audiobook) is not a video file; an unprobed source keeps the stamp.
	if streams, ok := source["MediaStreams"].([]any); !ok || len(streams) == 0 {
		setMissing(source, "VideoType", "VideoFile")
	} else {
		for _, stream := range streams {
			if dto, ok := stream.(map[string]any); ok && dto["Type"] == "Video" {
				setMissing(source, "VideoType", "VideoFile")
				break
			}
		}
	}
	setMissing(source, "UseMostCompatibleTranscodingProfile", false)
	if id, _ := source["Id"].(string); id != "" {
		setMissing(source, "ETag", itemEtag(id))
	}
}

// shapeJellyfinItem adds the list-item shell recorded from Jellyfin 12.4 and
// the optional fields the client asked for. Unknown file facts stay empty
// strings or null rather than invented containers, dates or image tags.
//
// Both protocols' movies and shows name the library that lists them here,
// so every list row that is shaped carries it.
func (i *instance) shapeJellyfinItem(r *http.Request, viewer *frontend.Viewer, item map[string]any, title frontend.Title) {
	if i == nil || item == nil {
		return
	}
	i.setVideoCanDownload(viewer, item, title)
	i.attachTitleLibrary(r, viewer, item, title)
	if i.protocol != "jellyfin" {
		if sources, ok := item["MediaSources"].([]any); ok {
			path, _ := item["Path"].(string)
			for _, raw := range sources {
				i.completeFileSource(r, raw.(map[string]any), title, path)
			}
		}
		return
	}
	setMissing(item, "ChannelId", nil)
	setMissing(item, "BackdropImageTags", []string{})
	setMissing(item, "ImageTags", map[string]string{})
	setMissing(item, "ImageBlurHashes", map[string]any{})
	// The type switches below describe the video hierarchy; a declared
	// music/book/photo item keeps only the generic shell around them.
	nonVideo := mbDeclaredNonVideo(title)
	switch title.Type {
	case "movie", "episode":
		if nonVideo {
			break
		}
		setMissing(item, "VideoType", "VideoFile")
		setMissing(item, "Container", "")
	case "show":
		if nonVideo {
			break
		}
		setMissing(item, "AirDays", []string{})
		setMissing(item, "MediaType", "Unknown")
		if title.Status != "" {
			setMissing(item, "Status", jellyfinSeriesStatus(title.Status))
		}
		if !title.LastAirDate.IsZero() {
			setMissing(item, "EndDate", wireTime(title.LastAirDate))
			setMissing(item, "DateLastMediaAdded", wireTime(title.LastAirDate))
		}
	case "season":
		if nonVideo {
			break
		}
		setMissing(item, "MediaType", "Unknown")
	}
	if !nonVideo && (title.Type == "episode" || title.Type == "season") {
		setMissing(item, "ParentBackdropImageTags", []string{})
		for _, key := range []string{"ParentBackdropItemId", "ParentLogoImageTag", "ParentLogoItemId", "ParentPrimaryImageItemId", "ParentPrimaryImageTag", "SeriesPrimaryImageTag"} {
			setMissing(item, key, nil)
		}
		i.attachParentArtwork(r, viewer, item, title)
	}
	i.attachViewParentArtwork(r, viewer, item)
	if title.ChildCount > 0 {
		setMissing(item, "ChildCount", title.ChildCount)
	}
	if title.LeafCount > 0 {
		setMissing(item, "RecursiveItemCount", title.LeafCount)
	}
	fields := fieldSet(r)
	if fields["etag"] {
		if id, _ := item["Id"].(string); id != "" {
			setMissing(item, "Etag", itemEtag(id))
		}
	}
	if fields["overview"] {
		setMissing(item, "Overview", title.Summary)
	}
	if fields["genres"] || !jellyfinListFile(r) {
		if len(title.Genres) > 0 {
			setMissing(item, "Genres", title.Genres)
		} else {
			setMissing(item, "Genres", []string{})
		}
		setMissing(item, "GenreItems", i.genreItems(r, viewer, title))
	}
	if fields["datecreated"] {
		if _, ok := item["DateCreated"]; !ok {
			if added := title.DateAdded(time.Now()); !added.IsZero() {
				item["DateCreated"] = wireTime(added)
			}
		}
	}
	if fields["childcount"] {
		setMissing(item, "ChildCount", title.ChildCount)
	}
	if fields["recursiveitemcount"] {
		setMissing(item, "RecursiveItemCount", title.LeafCount)
	}
	if sources, ok := item["MediaSources"].([]any); ok {
		for _, source := range sources {
			if dto, ok := source.(map[string]any); ok {
				jellyfinMediaSourceShape(dto, title.Title)
				if jellyfinListFile(r) {
					i.presentJellyfinFile(r, item, dto, title)
				}
			}
		}
	} else if fields["mediasources"] && jellyfinListFile(r) && !mbIsBookItem(title) && (title.Type == "movie" || title.Type == "episode") && r != nil && viewer != nil {
		source := i.defaultMediaSource(r, viewer, authhdr.MediaBrowserToken(r), title.Key)
		jellyfinMediaSourceShape(source, title.Title)
		alternatives := i.listedVersionSources(r, viewer, source, title)
		i.presentJellyfinFile(r, item, source, title)
		item["MediaSources"] = append([]any{source}, alternatives...)
	}
	if title.Type == "season" || title.Type == "show" {
		i.attachUnplayedCount(r, viewer, item, title)
	}
	if len(title.Keywords) > 0 {
		item["Tags"] = title.Keywords
	}
	jellyfinResumeSubtitle(item)
	i.fillSeriesStudio(r, viewer, item, title)
}

// attachUnplayedCount sets UserData.UnplayedItemCount from the viewer's watch
// state of the real child episodes. A failed or non-episode child listing
// leaves the field absent.
func (i *instance) attachUnplayedCount(r *http.Request, viewer *frontend.Viewer, item map[string]any, title frontend.Title) {
	if i == nil || i.protocol != "jellyfin" || r == nil || viewer == nil || i.host.Experience == nil {
		return
	}
	if title.Type != "show" && title.Type != "season" {
		return
	}
	if counts, ok := unplayedCountsFrom(r); ok {
		if counts.absent[title.Key] {
			return
		}
		if n, found := counts.n[title.Key]; found {
			data, ok := item["UserData"].(map[string]any)
			if !ok {
				return
			}
			data["UnplayedItemCount"] = n
			return
		}
	}
	var episodes []frontend.RatingKey
	if local, ok := i.host.Experience.(interface {
		LocalEpisodes(context.Context, *frontend.Viewer, frontend.RatingKey) ([]frontend.Title, bool, error)
	}); ok {
		listed, complete, err := local.LocalEpisodes(r.Context(), viewer, title.Key)
		if err != nil || !complete {
			return
		}
		episodes = make([]frontend.RatingKey, 0, len(listed))
		for _, child := range listed {
			if child.Type == "episode" {
				episodes = append(episodes, child.Key)
			}
		}
		if len(listed) > 0 && len(episodes) == 0 {
			return
		}
	} else {
		q := frontend.ItemQuery{}
		if title.Type == "show" {
			q.Recursive = true
		}
		page, err := i.host.Experience.Children(r.Context(), viewer, device(r), title.Key, q)
		if err != nil || page.Total > len(page.Items) {
			return
		}
		episodes = make([]frontend.RatingKey, 0, len(page.Items))
		for _, child := range page.Items {
			if child.Type == "episode" {
				episodes = append(episodes, child.Key)
			}
		}
		if len(page.Items) > 0 && len(episodes) == 0 {
			return
		}
	}
	states, err := i.host.Experience.UserData(r.Context(), viewer, episodes)
	if err != nil {
		return
	}
	unplayed := 0
	for _, key := range episodes {
		if states[key].ViewCount == 0 {
			unplayed++
		}
	}
	data, ok := item["UserData"].(map[string]any)
	if !ok {
		return
	}
	data["UnplayedItemCount"] = unplayed
}

// attachCumulativeRuntime sums the runtimes of a complete episode list.
// A truncated page or an episode without a runtime leaves the field absent.
func (i *instance) attachCumulativeRuntime(r *http.Request, viewer *frontend.Viewer, item map[string]any, title frontend.Title) {
	if i == nil || i.protocol != "jellyfin" || r == nil || viewer == nil || i.host.Experience == nil || title.Type != "show" {
		return
	}
	if _, ok := item["CumulativeRunTimeTicks"]; ok {
		return
	}
	if local, ok := i.host.Experience.(interface {
		LocalEpisodes(context.Context, *frontend.Viewer, frontend.RatingKey) ([]frontend.Title, bool, error)
	}); ok {
		listed, complete, err := local.LocalEpisodes(r.Context(), viewer, title.Key)
		if err != nil || !complete || len(listed) == 0 {
			return
		}
		var ticks int64
		for _, child := range listed {
			if child.Type != "episode" || child.Runtime <= 0 {
				return
			}
			ticks += child.Runtime.Nanoseconds() / 100
		}
		item["CumulativeRunTimeTicks"] = ticks
		return
	}
	page, err := i.host.Experience.Children(r.Context(), viewer, device(r), title.Key, frontend.ItemQuery{Recursive: true})
	if err != nil || page.Total > len(page.Items) || len(page.Items) == 0 {
		return
	}
	var ticks int64
	for _, child := range page.Items {
		if child.Type != "episode" || child.Runtime <= 0 {
			return
		}
		ticks += child.Runtime.Nanoseconds() / 100
	}
	item["CumulativeRunTimeTicks"] = ticks
}

// jellyfinExternalURLs links the provider ids the title already carries.
// Episode pages use the show id plus the season and episode numbers, which
// is the TMDB URL Jellyfin builds.
func jellyfinExternalURLs(t frontend.Title) []any {
	urls := []any{}
	if id := t.IDs.IMDB; len(id) > 2 && strings.HasPrefix(id, "tt") && strings.Trim(id[2:], "0123456789") == "" {
		urls = append(urls, map[string]string{"Name": "IMDb", "Url": "https://www.imdb.com/title/" + id})
	}
	switch t.Type {
	case "movie":
		if t.IDs.TMDB > 0 {
			urls = append(urls, map[string]string{"Name": "TMDB", "Url": "https://www.themoviedb.org/movie/" + strconv.FormatInt(t.IDs.TMDB, 10)})
		}
	case "show":
		if t.IDs.TMDB > 0 {
			urls = append(urls, map[string]string{"Name": "TMDB", "Url": "https://www.themoviedb.org/tv/" + strconv.FormatInt(t.IDs.TMDB, 10)})
		}
	case "episode":
		if t.ShowIDs.TMDB > 0 && t.Episode > 0 {
			urls = append(urls, map[string]string{"Name": "TMDB", "Url": "https://www.themoviedb.org/tv/" + strconv.FormatInt(t.ShowIDs.TMDB, 10) + "/season/" + strconv.Itoa(t.Season) + "/episode/" + strconv.Itoa(t.Episode)})
		}
	}
	return urls
}

func (i *instance) genreItems(r *http.Request, viewer *frontend.Viewer, title frontend.Title) []any {
	out := []any{}
	if r == nil || viewer == nil || i.host.Experience == nil || title.Key == 0 || len(title.Genres) == 0 {
		return out
	}
	names := make([]string, 0, min(len(title.Genres), 256))
	seen := map[string]bool{}
	for _, name := range title.Genres {
		if name == "" || seen[name] || len(names) == 256 {
			continue
		}
		seen[name] = true
		names = append(names, name)
	}
	authorized, err := i.host.Experience.NamedFacetIDs(r.Context(), viewer, device(r), title.Key, "genre", names)
	if err != nil {
		return out
	}
	for _, name := range names {
		id := authorized[name]
		if id == "" {
			continue
		}
		wireID, ok := i.namedFacetWireID(r.Context(), "genre", id)
		if ok {
			out = append(out, map[string]any{"Id": wireID, "Name": name})
		}
	}
	return out
}

func (i *instance) writeBoxSets(w http.ResponseWriter, r *http.Request, viewer *frontend.Viewer, q frontend.ItemQuery) {
	if i.host.Experience == nil {
		http.Error(w, "Unavailable", http.StatusServiceUnavailable)
		return
	}
	if slices.Contains(q.ExcludedTypes, frontend.MediaType("boxset")) {
		jsonReply(w, i.queryResult([]any{}, 0, q.Offset))
		return
	}
	page, err := i.host.Experience.Collections(r.Context(), viewer, device(r), frontend.CollectionQuery{Query: q.Filter.Query, Offset: q.Offset, Limit: q.Limit})
	if errors.Is(err, frontend.ErrNotAvailable) {
		jsonReply(w, i.queryResult([]any{}, 0, q.Offset))
		return
	}
	if err != nil {
		http.Error(w, "Unavailable", http.StatusServiceUnavailable)
		return
	}
	r = withJellyfinListFiles(withBrowseMemo(r))
	titles := make([]frontend.Title, 0, len(page.Items))
	for _, collection := range page.Items {
		titles = append(titles, frontend.Title{Key: collection.Key, Type: "boxset"})
	}
	images := i.gridListImages(r, titles)
	items := make([]any, 0, len(page.Items))
	for _, collection := range page.Items {
		dto := i.boxSetDTO(collection)
		for key, value := range images[collection.Key] {
			dto[key] = value
		}
		items = append(items, dto)
	}
	jsonReply(w, i.queryResult(items, page.Total, q.Offset))
}

func (i *instance) boxSetDTO(collection frontend.Collection) map[string]any {
	id := ids.RatingKeyGUID(collection.Key)
	name := collection.Title
	item := map[string]any{
		"Id": id, "Name": name, "Type": "BoxSet", "IsFolder": true, "ChildCount": collection.ChildCount,
		"Overview": collection.Summary, "ServerId": i.info.ID, "SortName": name, "CanDelete": false, "CanDownload": false,
		"LocationType": "FileSystem", "ProviderIds": map[string]string{},
	}
	if i.protocol == "jellyfin" {
		i.jellyfinCollectionFolder(item, id)
		item["Type"] = "BoxSet"
		item["ChildCount"] = collection.ChildCount
		item["Overview"] = collection.Summary
		delete(item, "CollectionType")
	}
	if i.protocol == "emby" {
		i.embyItemDTO(item, frontend.Title{Key: collection.Key, Title: name, Summary: collection.Summary}, frontend.WatchStatus{})
	}
	return item
}

// fillSeriesTitles copies each episode or season's show title onto the row
// when the list item did not carry it. One lookup serves each distinct show.
func (i *instance) fillSeriesTitles(r *http.Request, viewer *frontend.Viewer, titles []frontend.Title) {
	if i == nil || r == nil || viewer == nil || i.host.Experience == nil {
		return
	}
	names := map[frontend.RatingKey]string{}
	for n := range titles {
		title := &titles[n]
		if title.ShowTitle != "" || (title.Type != "episode" && title.Type != "season") {
			continue
		}
		show := parentShowKey(*title)
		if show == 0 {
			continue
		}
		name, ok := names[show]
		if !ok {
			name = i.showTitle(r, viewer, show)
			names[show] = name
		}
		if name == "" {
			continue
		}
		title.ShowTitle = name
		if title.ShowKey == 0 {
			title.ShowKey = show
		}
	}
}

func (i *instance) storedListTitle(r *http.Request, viewer *frontend.Viewer, key frontend.RatingKey) (frontend.Title, bool) {
	if i == nil || i.host.Experience == nil || r == nil || viewer == nil || key == 0 {
		return frontend.Title{}, false
	}
	req := frontend.ItemRequest{Viewer: viewer, Device: device(r), Key: key}
	type stored interface {
		StoredItem(context.Context, frontend.ItemRequest) (frontend.ItemDetail, error)
	}
	if source, ok := i.host.Experience.(stored); ok {
		detail, err := source.StoredItem(r.Context(), req)
		if err != nil || detail.Title.Key != key {
			return frontend.Title{}, false
		}
		return detail.Title, true
	}
	detail, err := i.host.Experience.Item(r.Context(), req)
	if err != nil || detail.Title.Key != key {
		return frontend.Title{}, false
	}
	return detail.Title, true
}

func (i *instance) showTitle(r *http.Request, viewer *frontend.Viewer, show frontend.RatingKey) string {
	title, ok := i.storedListTitle(r, viewer, show)
	if !ok || title.Title == "" || (title.Type != "" && title.Type != "show") {
		return ""
	}
	i.rememberSeriesStudio(r, show, title.Studios)
	return title.Title
}

// nameChildSeries copies the parent show's title onto children that do not
// already carry it. Season rows often omit the series label that detail fills.
func (i *instance) nameChildSeries(r *http.Request, viewer *frontend.Viewer, parent frontend.RatingKey, page *frontend.Page) {
	if i == nil || r == nil || viewer == nil || page == nil || i.host.Experience == nil || parent.Kind() != frontend.KindShow {
		return
	}
	missing := false
	for _, title := range page.Items {
		if title.ShowTitle == "" && (title.Type == "season" || title.Type == "episode") {
			missing = true
			break
		}
	}
	if !missing {
		return
	}
	detail, err := i.host.Experience.Item(r.Context(), frontend.ItemRequest{Viewer: viewer, Device: device(r), Key: parent})
	if err != nil || detail.Title.Title == "" {
		return
	}
	if detail.Title.Type != "" && detail.Title.Type != "show" {
		return
	}
	if detail.Title.Key != 0 && detail.Title.Key != parent {
		return
	}
	name := detail.Title.Title
	for n := range page.Items {
		if page.Items[n].ShowTitle == "" && (page.Items[n].Type == "season" || page.Items[n].Type == "episode") {
			page.Items[n].ShowTitle = name
			if page.Items[n].ShowKey == 0 {
				page.Items[n].ShowKey = parent
			}
		}
	}
}

// attachParentIdentity writes the show and season ids and names the title
// already carries. An episode key encodes its season when ShowKey was left
// empty. For episodes with an independent key, a canonical parent season
// also identifies the show and season.
func attachParentIdentity(m map[string]any, t frontend.Title) {
	if mbDeclaredNonVideo(t) {
		// A declared music/book/photo item names its immediate parent in
		// the kind's hierarchy (track -> album -> artist), never a series
		// or season. Jellyfin reads a track's album from Album/AlbumId.
		if t.ParentKey != 0 {
			parentID := ids.RatingKeyGUID(t.ParentKey)
			m["ParentId"] = parentID
			if t.MBItemType == "Audio" {
				m["AlbumId"] = parentID
				if t.ParentTitle != "" {
					m["Album"] = t.ParentTitle
				}
			}
		}
		if t.ItemIndex > 0 {
			m["IndexNumber"] = t.ItemIndex
		}
		if t.MBItemType == "Audio" {
			disc := t.Disc
			if disc <= 0 && t.ItemIndex > 0 {
				disc = 1
			}
			if disc > 0 {
				m["ParentIndexNumber"] = disc
			}
		}
		return
	}
	show := parentShowKey(t)
	seasonKey := t.ParentKey
	if seasonKey == 0 && t.Type == frontend.TypeEpisode {
		seasonKey = t.Key.Parent()
	}
	if show == 0 && t.MBItemType == "Video" && t.ParentKey != 0 {
		m["ParentId"] = ids.RatingKeyGUID(t.ParentKey)
		if t.ItemIndex > 0 {
			m["IndexNumber"] = t.ItemIndex
		}
	}
	if show != 0 {
		m["SeriesId"] = ids.RatingKeyGUID(show)
		if t.ShowTitle != "" {
			m["SeriesName"] = t.ShowTitle
		}
	}
	seasonNumber, episodeNumber := t.Season, t.Episode
	labeled := t
	labeled.Season = seasonNumber
	// Lab season rows put "Season N" in Name and omit SeasonName. Episodes
	// carry SeasonName alongside the episode title.
	if t.Type == "season" && t.Title == "" {
		if name := seasonDisplayName(labeled); name != "" {
			m["Name"] = name
		}
	}
	if t.Type == "episode" {
		if name := seasonDisplayName(labeled); name != "" {
			m["SeasonName"] = name
		}
	}
	if t.Type == "episode" {
		if seasonKey != 0 && seasonKey != show {
			id := ids.RatingKeyGUID(seasonKey)
			m["SeasonId"] = id
			m["ParentId"] = id
		}
		m["ParentIndexNumber"] = seasonNumber
		m["IndexNumber"] = episodeNumber
	}
	if t.Type == "season" {
		parent := t.ParentKey
		if parent == 0 {
			parent = show
		}
		if parent != 0 {
			m["ParentId"] = ids.RatingKeyGUID(parent)
		}
		m["IndexNumber"] = seasonNumber
	}
}

func seasonDisplayName(t frontend.Title) string {
	if t.SeasonTitle != "" {
		return t.SeasonTitle
	}
	if t.Season > 0 && (t.Type == "episode" || t.Type == "season") {
		return fmt.Sprintf("Season %d", t.Season)
	}
	return ""
}

// jellyfinListFields drops list keys Jellyfin leaves off unless Fields asked
// for them. Detail responses keep the item DTO. A season also drops runtime
// unless media sources were requested.
// jellyfinListFacts publishes subtitle and unplayed facts a library row can
// know without embedding media sources. Resume movies omit the zero count
// the season and series rows carry. A false subtitle flag stays absent.
func (i *instance) jellyfinListFacts(r *http.Request, item map[string]any, title frontend.Title) {
	if i == nil || i.protocol != "jellyfin" || item == nil {
		return
	}
	if jellyfinProbedSubtitles(r, title.Key) {
		item["HasSubtitles"] = true
	}
	if title.Type != "movie" || r == nil || strings.Contains(r.URL.Path, "/Resume") {
		return
	}
	data, ok := item["UserData"].(map[string]any)
	if !ok {
		return
	}
	if _, exists := data["UnplayedItemCount"]; !exists {
		data["UnplayedItemCount"] = 0
	}
}

func jellyfinProbedSubtitles(r *http.Request, key frontend.RatingKey) bool {
	if r == nil {
		return false
	}
	cached, _ := r.Context().Value(editionPageKey{}).(map[frontend.RatingKey][]frontend.Version)
	for _, version := range cached[key] {
		if version.Media.Estimated || len(version.Media.Streams) == 0 {
			continue
		}
		if mediaHasSubtitles(version.Media) {
			return true
		}
	}
	return false
}

func jellyfinRemoteTrailers(d frontend.ItemDetail) []any {
	var out []any
	for _, extra := range d.Extras {
		if extra.Kind != "trailer" && extra.Kind != "teaser" {
			continue
		}
		url, ok := remoteTrailerURL(extra.Source, extra.SourceID)
		if !ok {
			continue
		}
		name := extra.Title
		if name == "" {
			name = "Trailer"
		}
		out = append(out, map[string]string{"Name": name, "Url": url})
	}
	return out
}

func remoteTrailerURL(site, key string) (string, bool) {
	site = strings.ToLower(strings.TrimSpace(site))
	key = strings.TrimSpace(key)
	if key == "" || strings.Contains(key, "/") || strings.Contains(key, "?") {
		return "", false
	}
	switch site {
	case "youtube":
		return "https://www.youtube.com/watch?v=" + key, true
	case "vimeo":
		return "https://vimeo.com/" + key, true
	}
	return "", false
}

func (i *instance) jellyfinListFields(r *http.Request, item map[string]any, title frontend.Title) {
	if i == nil || i.protocol != "jellyfin" {
		return
	}
	fields := fieldSet(r)
	if !fields["overview"] {
		delete(item, "Overview")
	}
	if !fields["datecreated"] {
		delete(item, "DateCreated")
	}
	if !fields["sortname"] {
		delete(item, "SortName")
	}
	// Similar rows carry provider ids even when the request does not ask.
	if !fields["providerids"] && (r == nil || !strings.Contains(r.URL.Path, "/Similar")) {
		delete(item, "ProviderIds")
	}
	if !fields["genres"] {
		delete(item, "Genres")
		delete(item, "GenreItems")
	}
	if !fields["taglines"] {
		delete(item, "Taglines")
	}
	if !fields["tags"] {
		delete(item, "Tags")
	}
	if !fields["productionlocations"] {
		delete(item, "ProductionLocations")
	}
	if !fields["originaltitle"] {
		delete(item, "OriginalTitle")
	}
	// Blurhash maps repeat the image grant as a key. Library rows keep the
	// empty shell Jellyfin sends and leave the hash for item detail.
	item["ImageBlurHashes"] = map[string]any{}
	if title.Type == "season" && !fields["mediasources"] {
		delete(item, "RunTimeTicks")
	}
	delete(item, "CanDelete")
	delete(item, "CanDownload")
}

// jellyfinResumeFields drops keys a Jellyfin resume row does not carry unless
// Fields asked for them. Parent artwork and series identity stay.
func (i *instance) jellyfinResumeFields(r *http.Request, item map[string]any) {
	if i == nil || i.protocol != "jellyfin" || item == nil {
		return
	}
	fields := fieldSet(r)
	if !fields["overview"] {
		delete(item, "Overview")
	}
	if !fields["datecreated"] {
		delete(item, "DateCreated")
	}
	if !fields["sortname"] {
		delete(item, "SortName")
	}
	if !fields["providerids"] {
		delete(item, "ProviderIds")
	}
	if !fields["genres"] {
		delete(item, "Genres")
		delete(item, "GenreItems")
	}
	if !fields["mediasources"] {
		delete(item, "MediaSources")
		if has, ok := item["HasSubtitles"].(bool); !ok || !has {
			delete(item, "HasSubtitles")
		}
	}
	// Resume rows do not carry folder counts or the show's last-added date.
	delete(item, "ChildCount")
	delete(item, "RecursiveItemCount")
	delete(item, "DateLastMediaAdded")
	if !fields["path"] {
		delete(item, "Path")
	}
	if !fields["tags"] {
		delete(item, "Tags")
	}
	delete(item, "CanDelete")
	delete(item, "CanDownload")
	delete(item, "OriginalTitle")
	// Infuse asks for ParentId on resume. Other resume queries omit the field,
	// and those rows stay without it.
	if !fields["parentid"] {
		delete(item, "ParentId")
	}
	delete(item, "SeriesStudio")
	delete(item, "ProductionLocations")
}

// jellyfinSeasonListFields is the season row trim used by resume, next-up, and
// ancestors. Those responses keep overview and genres that a folder listing omits.
func (i *instance) jellyfinSeasonListFields(r *http.Request, item map[string]any, title frontend.Title) {
	if i == nil || i.protocol != "jellyfin" || title.Type != "season" {
		return
	}
	fields := fieldSet(r)
	if !fields["overview"] {
		delete(item, "Overview")
	}
	if !fields["datecreated"] {
		delete(item, "DateCreated")
	}
	if !fields["sortname"] {
		delete(item, "SortName")
	}
	if !fields["providerids"] {
		delete(item, "ProviderIds")
	}
	if !fields["mediasources"] {
		delete(item, "RunTimeTicks")
	}
	delete(item, "CanDelete")
	delete(item, "CanDownload")
}

// jellyfinDetailShell fills keys that a Jellyfin item detail response always
// carries. Empty collections stand in for facts this library does not have.
// List rows do not get this shell.
func (i *instance) jellyfinDetailShell(item map[string]any) {
	if i.protocol != "jellyfin" {
		return
	}
	setMissing(item, "Chapters", []any{})
	setMissing(item, "ExternalUrls", []any{})
	setMissing(item, "LockData", false)
	setMissing(item, "LockedFields", []any{})
	setMissing(item, "People", []any{})
	setMissing(item, "RemoteTrailers", []any{})
	setMissing(item, "Taglines", []any{})
	setMissing(item, "Tags", []any{})
	setMissing(item, "LocalTrailerCount", 0)
	setMissing(item, "SpecialFeatureCount", 0)
	if id, _ := item["Id"].(string); id != "" {
		setMissing(item, "Etag", itemEtag(id))
		setMissing(item, "DisplayPreferencesId", id)
	}
}

// jellyfinStudioFacts records studio names the title already has. Named-facet
// ids replace this list when the experience issued them.
func jellyfinStudioFacts(item map[string]any, title frontend.Title) {
	// NameGuidPair.Id is a required UUID in the Jellyfin SDK, so a
	// name-only Studios entry fails decoding of the whole item (#614).
	// Studios only ever carries the authorized facet pairs addNamedFacets
	// issued; the plain names stay available as SeriesStudio below.
	if title.Type == "show" || title.Type == "episode" || title.Type == "season" {
		for _, name := range title.Studios {
			if name != "" {
				setMissing(item, "SeriesStudio", name)
				break
			}
		}
	}
	setMissing(item, "Studios", []any{})
}

// jellyfinRootID is the user root folder. View numbers 1 and 2 are the
// movie and show libraries, so 0 stays the root and does not move when a
// library is renamed.
func jellyfinRootID() string {
	id, _ := ids.GUID(ids.View, 0)
	return id
}

// writeUserRoot serves the folder user views name as ParentId. The lab
// returns this object as a UserRootFolder named "Media Folders" from
// /Items/Root and from /Items/{id}.
func (i *instance) writeUserRoot(w http.ResponseWriter, r *http.Request, viewer *frontend.Viewer) {
	if i.host.Experience == nil {
		http.Error(w, "Unavailable", http.StatusServiceUnavailable)
		return
	}
	views, err := i.host.Experience.Views(r.Context(), viewer)
	if err != nil {
		http.Error(w, "Unavailable", http.StatusServiceUnavailable)
		return
	}
	id := jellyfinRootID()
	item := map[string]any{
		"Id": id, "Name": "Media Folders", "Type": "UserRootFolder", "IsFolder": true,
		"ServerId": i.info.ID, "DisplayPreferencesId": id, "ParentId": nil,
	}
	i.jellyfinCollectionFolder(item, id)
	item["Type"] = "UserRootFolder"
	item["Name"] = "Media Folders"
	item["ParentId"] = nil
	item["ChildCount"] = len(views)
	jsonReply(w, item)
}

// jellyfinLibraryOptions describes this server's libraries. Chapters and
// trickplay come from playback probes, not a library-scan image extractor.
// Subtitles already in the file are served when a client asks; nothing is
// downloaded or written back. Fields Ether does not have keep Jellyfin's
// schema default. PathInfos stays empty: libraries are virtual.
func jellyfinLibraryOptions() map[string]any {
	return map[string]any{
		"Enabled":                                 true,
		"EnablePhotos":                            true,
		"EnableRealtimeMonitor":                   false,
		"EnableInternetProviders":                 false,
		"EnableAutomaticSeriesGrouping":           true,
		"EnableChapterImageExtraction":            false,
		"ExtractChapterImagesDuringLibraryScan":   false,
		"EnableTrickplayImageExtraction":          false,
		"ExtractTrickplayImagesDuringLibraryScan": false,
		"SaveTrickplayWithMedia":                  false,
		"EnableEmbeddedTitles":                    false,
		"EnableEmbeddedEpisodeInfos":              false,
		"EnableEmbeddedExtrasTitles":              false,
		"EnableLUFSScan":                          false,
		"SaveLocalMetadata":                       false,
		"SaveLyricsWithMedia":                     false,
		"SaveSubtitlesWithMedia":                  false,
		"AllowEmbeddedSubtitles":                  "AllowAll",
		"RequirePerfectSubtitleMatch":             true,
		"SkipSubtitlesIfAudioTrackMatches":        true,
		"SkipSubtitlesIfEmbeddedSubtitlesPresent": false,
		"AutomaticRefreshIntervalDays":            0,
		"AutomaticallyAddToCollection":            false,
		"PreferNonstandardArtistsTag":             false,
		"UseCustomTagDelimiters":                  false,
		"SeasonZeroDisplayName":                   "Specials",
		"PathInfos":                               []any{},
		"DisabledLocalMetadataReaders":            []any{},
		"DisabledLyricFetchers":                   []any{},
		"DisabledMediaSegmentProviders":           []any{},
		"DisabledSubtitleFetchers":                []any{},
		"LyricFetcherOrder":                       []any{},
		"MediaSegmentProviderOrder":               []any{},
		"SubtitleFetcherOrder":                    []any{},
		"TypeOptions":                             []any{},
		"DelimiterWhitelist":                      []any{},
		"CustomTagDelimiters":                     []any{"/", "|", ";", "\\"},
	}
}

func (i *instance) virtualFolders(w http.ResponseWriter, r *http.Request) {
	viewer, _, err := i.viewer(r)
	if err != nil || viewer == nil {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	if i.host.Experience == nil {
		http.Error(w, "Unavailable", http.StatusServiceUnavailable)
		return
	}
	// Jellyfin requires elevation. A non-admin gets an empty 403 with no
	// content type, which is what the lab reference returns.
	if i.protocol == "jellyfin" && (viewer.Delegated || !viewer.IsOwner) {
		w.WriteHeader(http.StatusForbidden)
		return
	}
	views, err := i.host.Experience.Views(r.Context(), viewer)
	if err != nil {
		http.Error(w, "Unavailable", http.StatusServiceUnavailable)
		return
	}
	result := make([]any, 0, len(views))
	for _, view := range views {
		viewNum, err := ids.ParseDecimal(view.ID)
		if err != nil {
			continue
		}
		viewID, err := ids.GUID(ids.View, viewNum)
		if err != nil {
			continue
		}
		entry := map[string]any{"Name": view.Name, "ItemId": viewID}
		if i.protocol == "jellyfin" {
			entry["LibraryOptions"] = jellyfinLibraryOptions()
			entry["RefreshStatus"] = "Idle"
			// The library view's collage is that folder, so its item id is the view GUID.
			entry["PrimaryImageItemId"] = viewID
		}
		if i.protocol == "jellyfin" && view.Virtual {
			entry["Locations"] = []string{}
		}
		if ct := mediaBrowserCollectionType(view); ct != "" {
			entry["CollectionType"] = ct
		}
		result = append(result, entry)
	}
	jsonReply(w, result)
}

type listMediaEstimateKey struct{}

func withListMediaEstimate(r *http.Request) *http.Request {
	if r == nil {
		return r
	}
	return r.WithContext(context.WithValue(r.Context(), listMediaEstimateKey{}, true))
}

func listMediaEstimate(r *http.Request) bool {
	if r == nil {
		return false
	}
	set, _ := r.Context().Value(listMediaEstimateKey{}).(bool)
	return set
}

func mediaHasSubtitles(media frontend.MediaInfo) bool {
	for _, stream := range media.Streams {
		if stream.Type == "subtitle" {
			return true
		}
	}
	return false
}

type editionPageKey struct{}

// pageEditions loads probed files when the response will keep MediaSources.
// Both Jellyfin and Emby list rows carry probed MediaSources when the request
// names the field; a list without it deletes the objects after they are built,
// so the probe is skipped then.
func (i *instance) pageEditions(r *http.Request, viewer *frontend.Viewer, keys []frontend.RatingKey) *http.Request {
	if i == nil {
		return r
	}
	return i.withPageEditions(r, viewer, keys)
}

func (i *instance) withPageEditions(r *http.Request, viewer *frontend.Viewer, keys []frontend.RatingKey) *http.Request {
	batch, ok := i.host.Editions.(frontend.EditionBatch)
	if !ok || viewer == nil || r == nil || len(keys) == 0 {
		return r
	}
	// Editions only ever feed MediaSources, so skip the batch when the
	// response will not keep it.
	if !fieldSet(r)["mediasources"] {
		return r
	}
	versions, err := batch.EditionsMany(r.Context(), viewer, device(r), keys)
	if err != nil {
		return r
	}
	return r.WithContext(context.WithValue(r.Context(), editionPageKey{}, versions))
}

type jellyfinEdition struct {
	handle string
	source map[string]any
	media  frontend.MediaInfo
}

// jellyfinEditions returns one probed local copy per file. A single copy keeps
// the title id. Several copies become separate items, matching a library that
// stores each file as its own item. Emby and Jellyfin both embed these on a
// list row when the request asks for MediaSources.
func (i *instance) jellyfinEditions(r *http.Request, viewer *frontend.Viewer, title frontend.Title) []jellyfinEdition {
	if i == nil || i.host.Editions == nil || viewer == nil || r == nil {
		return nil
	}
	if !fieldSet(r)["mediasources"] {
		return nil
	}
	if title.Type != "movie" && title.Type != "episode" {
		return nil
	}
	var versions []frontend.Version
	var err error
	if cached, ok := r.Context().Value(editionPageKey{}).(map[frontend.RatingKey][]frontend.Version); ok {
		versions = cached[title.Key]
	} else {
		versions, err = i.host.Editions.Editions(r.Context(), viewer, device(r), title.Key)
	}
	if err != nil || len(versions) == 0 {
		return nil
	}
	out := make([]jellyfinEdition, 0, len(versions))
	var scope frontend.VersionScope
	var scopeErr error
	scoped := false
	for _, version := range versions {
		if version.Media.Estimated || version.Media.Size <= 0 || version.ID == "" {
			continue
		}
		id := ids.RatingKeyGUID(title.Key)
		if len(versions) > 1 {
			if !scoped {
				scope, scopeErr = i.versionScope(r, viewer)
				scoped = true
			}
			if scopeErr != nil || i.host.VersionHandles == nil {
				continue
			}
			handle, e := i.host.VersionHandles.Issue(i.identityContext(r), scope, title.Key, version.ID)
			if e != nil || handle == "" {
				continue
			}
			id = handle
		}
		source := i.mediaSourceDTO(id, version.Media)
		if i.protocol == "emby" {
			i.versionSourceFacts(source, version)
			i.embyMediaSourceDTO(source, ids.RatingKeyGUID(title.Key), version.Media)
			// Emby list rows omit chapter arrays too. Detail keeps them.
			delete(source, "Chapters")
			out = append(out, jellyfinEdition{handle: id, source: source, media: version.Media})
			continue
		}
		jellyfinMediaSourceShape(source, title.Title)
		i.jellyfinMediaSourceDTO(source, version.Media, false)
		// Jellyfin list rows omit chapter arrays. Detail keeps them.
		delete(source, "Chapters")
		out = append(out, jellyfinEdition{handle: id, source: source, media: version.Media})
	}
	return out
}

func setSourcePath(source map[string]any, itemPath string) {
	if source == nil || itemPath == "" {
		return
	}
	if _, ok := source["Path"]; ok {
		return
	}
	source["Path"] = itemPath
}

func applyJellyfinEditionFacts(item map[string]any, media frontend.MediaInfo) {
	if item == nil || media.Estimated || len(media.Streams) == 0 {
		return
	}
	item["HasSubtitles"] = mediaHasSubtitles(media)
}

func (i *instance) fillSeriesStudio(r *http.Request, viewer *frontend.Viewer, item map[string]any, title frontend.Title) {
	if i == nil || i.protocol != "jellyfin" || item == nil {
		return
	}
	if title.Type != "episode" && title.Type != "season" {
		return
	}
	if _, ok := item["SeriesStudio"]; ok {
		return
	}
	name := ""
	for _, studio := range title.Studios {
		if studio != "" {
			name = studio
			break
		}
	}
	if name == "" {
		name = i.cachedSeriesStudio(r, title.ShowKey)
	}
	// A list that already shaped the show reuses that name. An episode detail
	// has not, so load the show once and keep it for the rest of the request.
	if name == "" && title.ShowKey != 0 && r != nil && viewer != nil && i.host.Experience != nil {
		if show, ok := i.storedListTitle(r, viewer, title.ShowKey); ok {
			i.rememberSeriesStudio(r, title.ShowKey, show.Studios)
			name = i.cachedSeriesStudio(r, title.ShowKey)
		}
	}
	if name != "" {
		item["SeriesStudio"] = name
	}
}

type seriesStudioCacheKey struct{}

func (i *instance) cachedSeriesStudio(r *http.Request, show frontend.RatingKey) string {
	if r == nil || show == 0 {
		return ""
	}
	cache, _ := r.Context().Value(seriesStudioCacheKey{}).(map[frontend.RatingKey]string)
	if cache == nil {
		return ""
	}
	return cache[show]
}

func (i *instance) rememberSeriesStudio(r *http.Request, show frontend.RatingKey, studios []string) {
	if r == nil || show == 0 {
		return
	}
	cache, _ := r.Context().Value(seriesStudioCacheKey{}).(map[frontend.RatingKey]string)
	if cache == nil {
		return
	}
	if _, ok := cache[show]; ok {
		return
	}
	name := ""
	for _, studio := range studios {
		if studio != "" {
			name = studio
			break
		}
	}
	cache[show] = name
}

func withSeriesStudioCache(r *http.Request) *http.Request {
	if r == nil {
		return r
	}
	if _, ok := r.Context().Value(seriesStudioCacheKey{}).(map[frontend.RatingKey]string); !ok {
		r = r.WithContext(context.WithValue(r.Context(), seriesStudioCacheKey{}, map[frontend.RatingKey]string{}))
	}
	if _, ok := r.Context().Value(showArtworkCacheKey{}).(map[frontend.RatingKey]map[string]any); !ok {
		r = r.WithContext(context.WithValue(r.Context(), showArtworkCacheKey{}, map[frontend.RatingKey]map[string]any{}))
	}
	if _, ok := r.Context().Value(libraryCollageCacheKey{}).(map[string]libraryCollageHit); ok {
		return r
	}
	return r.WithContext(context.WithValue(r.Context(), libraryCollageCacheKey{}, map[string]libraryCollageHit{}))
}

type showArtworkCacheKey struct{}

// attachParentArtwork copies the show's issued image tags onto an episode or
// season. The tags are grants for the parent item, not copies of the tag text
// from another server.
func (i *instance) attachParentArtwork(r *http.Request, viewer *frontend.Viewer, item map[string]any, title frontend.Title) {
	if i == nil || r == nil || item == nil {
		return
	}
	show := parentShowKey(title)
	if show == 0 {
		return
	}
	images := i.cachedShowArtwork(r, viewer, show)
	seriesID := ids.RatingKeyGUID(show)
	if tags, _ := images["ImageTags"].(map[string]string); len(tags) > 0 {
		if logo := tags["Logo"]; logo != "" {
			item["ParentLogoImageTag"] = logo
			item["ParentLogoItemId"] = seriesID
		}
		if primary := tags["Primary"]; primary != "" {
			item["ParentPrimaryImageTag"] = primary
			item["ParentPrimaryImageItemId"] = seriesID
			item["SeriesPrimaryImageTag"] = primary
		}
	}
	if backs, _ := images["BackdropImageTags"].([]string); len(backs) > 0 {
		item["ParentBackdropImageTags"] = backs
		item["ParentBackdropItemId"] = seriesID
	}
}

// parentShowKey resolves the series relationship for season and episode rows.
func parentShowKey(title frontend.Title) frontend.RatingKey {
	if title.ShowKey != 0 {
		return title.ShowKey
	}
	switch title.Key.Kind() {
	case frontend.KindSeason:
		if title.ParentKey != 0 {
			return title.ParentKey
		}
		return title.Key.Parent()
	case frontend.KindEpisode:
		return title.Key.Parent().Parent()
	}
	return 0
}

// prefetchParentArtwork signs the series artwork of every episode and
// season in titles in one page signing, so attachParentArtwork reads it
// from the request's cache. Signing each series alone ran the artwork,
// overlay and grant reads once per series on the page.
func (i *instance) prefetchParentArtwork(r *http.Request, titles []frontend.Title) {
	if i == nil || i.protocol != "jellyfin" || r == nil {
		return
	}
	cache, _ := r.Context().Value(showArtworkCacheKey{}).(map[frontend.RatingKey]map[string]any)
	if cache == nil {
		return
	}
	var shows []frontend.Title
	seen := map[frontend.RatingKey]bool{}
	for _, title := range titles {
		if title.Type != "episode" && title.Type != "season" {
			continue
		}
		show := parentShowKey(title)
		if _, cached := cache[show]; show == 0 || cached || seen[show] {
			continue
		}
		seen[show] = true
		shows = append(shows, frontend.Title{Key: show})
	}
	if len(shows) == 0 {
		return
	}
	// A series the batch did not sign is left to cachedShowArtwork, which
	// signs it alone as before.
	for key, art := range i.gridImagesFor(r, shows) {
		cache[key] = art
	}
}

func (i *instance) cachedShowArtwork(r *http.Request, _ *frontend.Viewer, show frontend.RatingKey) map[string]any {
	cache, _ := r.Context().Value(showArtworkCacheKey{}).(map[frontend.RatingKey]map[string]any)
	if cache != nil {
		if art, ok := cache[show]; ok {
			return art
		}
	}
	art := map[string]any{}
	if row := i.gridImagesFor(r, []frontend.Title{{Key: show}})[show]; row != nil {
		art = row
	}
	if cache != nil {
		cache[show] = art
	}
	return art
}

type libraryCollageCacheKey struct{}

type libraryCollageHit struct {
	secret string
	image  frontend.Image
	ok     bool
}

func (i *instance) attachLibraryCollage(r *http.Request, viewer *frontend.Viewer, viewNum int64, viewID string, item map[string]any) {
	secret, img, ok := i.libraryCollageGrant(r, viewer, viewNum, viewID)
	if !ok || item == nil {
		return
	}
	tags, _ := item["ImageTags"].(map[string]string)
	if tags == nil {
		tags = map[string]string{}
		item["ImageTags"] = tags
	}
	tags["Primary"] = secret
	i.addImageMetadata(item, "Primary", secret, img)
}

func (i *instance) attachViewParentArtwork(r *http.Request, viewer *frontend.Viewer, item map[string]any) {
	if item == nil {
		return
	}
	if tag, _ := item["ParentPrimaryImageTag"].(string); tag != "" {
		return
	}
	parent, _ := item["ParentId"].(string)
	if parent == "" || parent == jellyfinRootID() {
		return
	}
	viewNum, err := ids.ParseGUID(ids.View, parent)
	if err != nil {
		return
	}
	secret, _, ok := i.libraryCollageGrant(r, viewer, viewNum, parent)
	if !ok {
		return
	}
	item["ParentPrimaryImageTag"] = secret
	item["ParentPrimaryImageItemId"] = parent
}

func (i *instance) libraryCollageGrant(r *http.Request, viewer *frontend.Viewer, viewNum int64, viewID string) (string, frontend.Image, bool) {
	if i == nil || i.protocol != "jellyfin" || r == nil || viewer == nil || viewNum == 0 || viewID == "" || i.host.Experience == nil || i.host.ImageGrants == nil {
		return "", frontend.Image{}, false
	}
	if cache, _ := r.Context().Value(libraryCollageCacheKey{}).(map[string]libraryCollageHit); cache != nil {
		if hit, ok := cache[viewID]; ok {
			return hit.secret, hit.image, hit.ok
		}
	}
	source, ok := i.host.Experience.(frontend.LibraryCollageSource)
	if !ok {
		i.rememberLibraryCollage(r, viewID, "", frontend.Image{}, false)
		return "", frontend.Image{}, false
	}
	img, err := source.LibraryCollage(r.Context(), viewer, device(r), viewID)
	if err != nil || !strings.HasPrefix(img.Path, frontend.LibraryCollagePrefix) || img.Path != img.Tag || img.Subject != viewID {
		i.rememberLibraryCollage(r, viewID, "", frontend.Image{}, false)
		return "", frontend.Image{}, false
	}
	secret, err := i.host.ImageGrants.Issue(r.Context(), frontend.ImageGrantRequest{
		Credential: authhdr.MediaBrowserToken(r), Device: device(r), Item: frontend.RatingKey(viewNum), Image: img,
	})
	if err != nil || secret == "" {
		i.rememberLibraryCollage(r, viewID, "", frontend.Image{}, false)
		return "", frontend.Image{}, false
	}
	i.rememberLibraryCollage(r, viewID, secret, img, true)
	return secret, img, true
}

func (i *instance) rememberLibraryCollage(r *http.Request, viewID, secret string, img frontend.Image, ok bool) {
	cache, _ := r.Context().Value(libraryCollageCacheKey{}).(map[string]libraryCollageHit)
	if cache == nil {
		return
	}
	cache[viewID] = libraryCollageHit{secret: secret, image: img, ok: ok}
}
