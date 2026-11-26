package vela

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/braedonsaunders/ether/internal/frontend"
)

func (i *instance) liveETag(r *http.Request, payload any) string {
	return `"` + hashJSON([]any{
		payload,
		// The cursor bucket (cursorExpiry) changes the tag before a cursor in
		// a cached page can expire.
		time.Now().Unix() / 120,
		requestDevice(r),
		r.Header.Get("Authorization"),
		r.Header.Get("X-Vela-Device-Id"),
		r.URL.RequestURI(),
	}) + `"`
}

func liveNotModified(w http.ResponseWriter, r *http.Request, tag string) bool {
	for _, candidate := range strings.Split(r.Header.Get("If-None-Match"), ",") {
		trimmed := strings.TrimSpace(candidate)
		if trimmed == tag || trimmed == "*" {
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Cache-Control", "private, max-age=0, must-revalidate")
			w.Header().Set("ETag", tag)
			w.Header().Set("Vary", "Authorization, X-Vela-Device-Id")
			w.WriteHeader(http.StatusNotModified)
			return true
		}
	}
	return false
}

func writeLiveCached(w http.ResponseWriter, r *http.Request, tag string, v any) {
	b, err := json.Marshal(v)
	if err != nil {
		fail(w, err, r)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "private, max-age=0, must-revalidate")
	w.Header().Set("ETag", tag)
	w.Header().Set("Vary", "Authorization, X-Vela-Device-Id")
	_, _ = w.Write(b)
}

func (i *instance) channels(w http.ResponseWriter, r *http.Request) {
	v, token, e := i.viewer(r)
	if e != nil {
		fail(w, e, r)
		return
	}
	if i.host.LiveTV == nil {
		i.unavailable(w, r)
		return
	}
	scope, e := i.livePageScope(r, v, token)
	if e != nil {
		fail(w, e, r)
		return
	}
	cursor, e := i.readLiveCursor(r, scope, hashJSON([]any{"channels", limit(r)}))
	if e != nil {
		fail(w, e, r)
		return
	}
	channels, e := i.host.LiveTV.Channels(r.Context(), v)
	if e != nil {
		fail(w, e, r)
		return
	}
	snapshot := hashJSON(channels)
	if cursor.Snapshot != "" && cursor.Snapshot != snapshot {
		fail(w, frontend.ErrLiveGuideStale)
		return
	}
	start := min(cursor.Offset, len(channels))
	end := start + min(limit(r), len(channels)-start)
	tag := i.liveETag(r, []any{"channels", snapshot, start, end, end < len(channels)})
	if liveNotModified(w, r, tag) {
		return
	}
	selected := channels[start:end]
	if batch, ok := i.host.ImageGrants.(frontend.ChannelArtworkGrantBatch); ok {
		var ids []string
		for _, c := range selected {
			if len(c.Images) > 0 {
				ids = append(ids, c.ID)
			}
		}
		ctx, err := batch.PrepareChannelArtwork(r.Context(), token, ids)
		if err != nil {
			fail(w, err, r)
			return
		}
		r = r.WithContext(ctx)
	}
	out := []object{}
	memo := newLiveArtCache()
	for _, c := range selected {
		art, err := i.liveArtworkCached(r, token, 0, c.ID, c.Images, memo)
		if err != nil {
			fail(w, err, r)
			return
		}
		channel := object{"id": c.ID, "number": c.Number, "name": c.Name, "kind": c.Kind, "artwork": art, "favorite": c.Favorite, "rating": c.Rating}
		if c.ArchiveDays > 0 {
			channel["archiveDays"] = c.ArchiveDays
		}
		if c.Groups != nil {
			// The Vela protocol names an owner's channel groups "guides"
			// (Channel.guides, LiveGuide); Vela builds its guide tabs from it.
			guides := []object{}
			for _, group := range c.Groups {
				guides = append(guides, object{"id": group.ID, "name": group.Name})
			}
			channel["guides"] = guides
		}
		if c.Group != "" {
			channel["group"] = c.Group
		}
		out = append(out, channel)
	}
	page := object{"items": out, "continuation": "exhausted"}
	if end < len(channels) {
		next, err := i.nextLiveCursor(cursor, end, snapshot)
		if err != nil {
			fail(w, err, r)
			return
		}
		page["continuation"], page["nextCursor"] = "more", next
	}
	writeLiveCached(w, r, tag, page)
}

func (i *instance) guide(w http.ResponseWriter, r *http.Request) {
	v, token, e := i.viewer(r)
	if e != nil {
		fail(w, e, r)
		return
	}
	if i.host.LiveTV == nil {
		i.unavailable(w, r)
		return
	}
	q := r.URL.Query()
	from, e1 := time.Parse(time.RFC3339, q.Get("from"))
	until, e2 := time.Parse(time.RFC3339, q.Get("until"))
	if e1 != nil || e2 != nil || !until.After(from) || until.Sub(from) > 24*time.Hour {
		problem(w, 400, "request", "invalid_guide_window")
		return
	}
	channels := []string{}
	if s := q.Get("channelIds"); s != "" {
		channels = strings.Split(s, ",")
	}
	if len(channels) > 100 {
		problem(w, 400, "request", "too_many_channels")
		return
	}
	scope, e := i.livePageScope(r, v, token)
	if e != nil {
		fail(w, e, r)
		return
	}
	cursor, e := i.readLiveCursor(r, scope, hashJSON([]any{"guide", from.UTC(), until.UTC(), channels, limit(r)}))
	if e != nil {
		fail(w, e, r)
		return
	}
	pager, ok := i.host.LiveTV.(frontend.LiveGuidePager)
	if !ok {
		i.unavailable(w, r)
		return
	}
	programmes, e := pager.GuidePage(i.host.Experience.NoProviderWait(r.Context()), v, frontend.LiveGuidePageQuery{LiveGuideQuery: frontend.LiveGuideQuery{Channels: channels, From: from, Until: until, Limit: limit(r), Device: requestDevice(r)}, Offset: cursor.Offset, Snapshot: cursor.Snapshot})
	if e != nil {
		fail(w, e, r)
		return
	}
	if programmes.More && (len(programmes.Items) == 0 || programmes.Snapshot == "") {
		fail(w, frontend.ErrLiveGuideStale)
		return
	}
	now := i.catchUpNow()
	tag := i.liveETag(r, []any{
		"guide",
		from.UTC().Format(time.RFC3339),
		until.UTC().Format(time.RFC3339),
		cursor.Offset,
		programmes.Snapshot,
		programmes.More,
		programmes.Items,
		archiveFlagFingerprint(programmes.Items, 0, now),
	})
	if liveNotModified(w, r, tag) {
		return
	}
	out := []object{}
	memo := newLiveArtCache()
	i.liveProgrammeTitleArt(r, token, programmes.Items, memo)
	for _, p := range programmes.Items {
		programme, err := i.liveProgrammeWire(r, token, p, memo)
		if err != nil {
			fail(w, err, r)
			return
		}
		addArchiveFlags(programme, p.ArchiveDays, p.Start, p.End, now)
		out = append(out, programme)
	}
	page := object{"items": out, "from": from.UTC().Format(time.RFC3339), "until": until.UTC().Format(time.RFC3339), "continuation": "exhausted"}
	if programmes.More {
		next, err := i.nextLiveCursor(cursor, cursor.Offset+len(out), programmes.Snapshot)
		if err != nil {
			fail(w, err, r)
			return
		}
		page["continuation"], page["nextCursor"] = "more", next
	}
	writeLiveCached(w, r, tag, page)
}

func (i *instance) liveGuideWindowReader() (frontend.LiveGuideWindowReader, bool) {
	if i.host.LiveTV == nil {
		return nil, false
	}
	_, live := i.host.LiveTV.(frontend.LiveGuidePager)
	reader, ok := i.host.LiveTV.(frontend.LiveGuideWindowReader)
	if !live || !ok || !reader.LiveGuideWindowAvailable() {
		return nil, false
	}
	return reader, true
}

func (i *instance) liveProgrammeTitleArt(r *http.Request, token string, programmes []frontend.LiveProgramme, memo *liveArtCache) {
	signer, ok := i.host.ImageGrants.(frontend.ImagePageSigner)
	if !ok || memo == nil {
		return
	}
	var titles []frontend.Title
	seenTitles := map[frontend.RatingKey]bool{}
	seenKinds := map[string]bool{}
	var kinds []string
	for _, p := range programmes {
		if p.Item == 0 {
			continue
		}
		for _, img := range p.Images {
			if img.Subject == p.ID || img.Path == "" {
				continue
			}
			switch img.Kind {
			case frontend.ImagePoster, frontend.ImageBackdrop, frontend.ImageLogo, frontend.ImageThumb, frontend.ImageBanner, frontend.ImageStill:
				if !seenKinds[img.Kind] {
					seenKinds[img.Kind] = true
					kinds = append(kinds, img.Kind)
				}
				if !seenTitles[p.Item] {
					seenTitles[p.Item] = true
					titles = append(titles, frontend.Title{Key: p.Item})
				}
			}
		}
	}
	if len(titles) == 0 || len(kinds) == 0 {
		return
	}
	signed, err := signer.SignPage(r.Context(), frontend.ImagePageRequest{
		Credential: token,
		Device:     requestDevice(r),
		Titles:     titles,
		Kinds:      kinds,
	})
	if err != nil {
		return
	}
	for _, batch := range signed {
		for _, g := range batch.Images {
			if g.Image.Path == "" || g.Secret == "" {
				continue
			}
			key := fmt.Sprintf("%d\x00\x00%s\x00%s\x00%s", batch.Item, g.Image.Kind, g.Image.Path, g.Image.Tag)
			memo.issued[key] = object{"id": hash(g.Image.Path), "kind": g.Image.Kind, "tag": g.Image.Tag, "sizes": []object{imageSize(g.Image, g.Secret)}}
		}
	}
}

func (i *instance) liveProgrammeWire(r *http.Request, token string, p frontend.LiveProgramme, memo *liveArtCache) (object, error) {
	art := []object{}
	for _, img := range p.Images {
		item, channel := p.Item, ""
		if img.Subject == p.ID {
			item, channel = 0, p.ChannelID
		}
		images, err := i.liveArtworkCached(r, token, item, channel, []frontend.Image{img}, memo)
		if err != nil {
			return nil, err
		}
		art = append(art, images...)
	}
	programme := object{
		"id":            p.ID,
		"channelId":     p.ChannelID,
		"name":          p.Title,
		"summary":       p.Summary,
		"start":         p.Start.UTC().Format(time.RFC3339),
		"end":           p.End.UTC().Format(time.RFC3339),
		"episodeTitle":  p.EpisodeTitle,
		"seasonNumber":  p.Season,
		"episodeNumber": p.Episode,
		"artwork":       art,
		"rating":        p.Rating,
	}
	if len(p.Genres) > 0 {
		programme["genres"] = p.Genres
	}
	return programme, nil
}

func rawRequestBody(w http.ResponseWriter, r *http.Request) (json.RawMessage, bool) {
	if b, ok := r.Context().Value(bodyKey{}).(json.RawMessage); ok {
		return b, true
	}
	var raw json.RawMessage
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	if dec.Decode(&raw) != nil {
		return nil, false
	}
	var trailing any
	if dec.Decode(&trailing) != io.EOF {
		return nil, false
	}
	return raw, true
}

func (i *instance) guideWindow(w http.ResponseWriter, r *http.Request) {
	v, token, e := i.viewer(r)
	if e != nil {
		fail(w, e, r)
		return
	}
	reader, ok := i.liveGuideWindowReader()
	if !ok {
		i.unavailable(w, r)
		return
	}
	raw, ok := rawRequestBody(w, r)
	if !ok {
		problem(w, 400, "request", "invalid_json")
		return
	}
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		problem(w, 400, "request", "invalid_request")
		return
	}
	var req struct {
		ChannelIDs     []string `json:"channelIds"`
		From           string   `json:"from"`
		Until          string   `json:"until"`
		IdempotencyKey *string  `json:"idempotencyKey"`
	}
	if json.Unmarshal(raw, &req) != nil {
		problem(w, 400, "request", "invalid_request")
		return
	}
	if req.IdempotencyKey != nil && (*req.IdempotencyKey == "" || len(*req.IdempotencyKey) > 256) {
		problem(w, 400, "request", "invalid_request")
		return
	}
	from, e1 := time.Parse(time.RFC3339Nano, req.From)
	until, e2 := time.Parse(time.RFC3339Nano, req.Until)
	maxWindow := time.Duration(frontend.MaxLiveGuideWindowMinutes) * time.Minute
	if e1 != nil || e2 != nil || !until.After(from) || until.Sub(from) > maxWindow {
		problem(w, 400, "request", "invalid_guide_window")
		return
	}
	if len(req.ChannelIDs) == 0 {
		problem(w, 400, "request", "invalid_request")
		return
	}
	if len(req.ChannelIDs) > frontend.MaxLiveGuideWindowChannels {
		problem(w, 400, "request", "too_many_channels")
		return
	}
	seen := make(map[string]struct{}, len(req.ChannelIDs))
	for _, id := range req.ChannelIDs {
		if id == "" {
			problem(w, 400, "request", "invalid_request")
			return
		}
		if _, dup := seen[id]; dup {
			problem(w, 400, "request", "invalid_request")
			return
		}
		seen[id] = struct{}{}
	}

	ctx := r.Context()
	if i.host.Experience != nil {
		ctx = i.host.Experience.NoProviderWait(ctx)
	}
	window, err := reader.GuideWindowBatch(ctx, v, frontend.LiveGuideWindowQuery{
		Channels: req.ChannelIDs,
		From:     from,
		Until:    until,
		Device:   requestDevice(r),
	})
	if err != nil {
		var dense *frontend.LiveGuideWindowTooDenseError
		if errors.As(err, &dense) {
			send(w, 400, object{
				"error": object{
					"class":   "request",
					"code":    "guide_window_too_dense",
					"message": "guide window too dense",
					"retry":   object{"allowed": false},
					"suggest": object{
						"maxChannels":      dense.MaxChannels,
						"maxWindowMinutes": dense.MaxWindowMinutes,
					},
				},
			})
			return
		}
		fail(w, err, r)
		return
	}

	now := i.catchUpNow()
	flags := make([][]byte, len(window.Channels))
	for index, ch := range window.Channels {
		flags[index] = archiveFlagFingerprint(ch.Programmes, ch.ArchiveDays, now)
	}
	tag := i.liveETag(r, []any{
		"guide-window",
		req.ChannelIDs,
		from.UTC().Format(time.RFC3339),
		until.UTC().Format(time.RFC3339),
		window.Revision,
		window.ValidUntil.UTC().Format(time.RFC3339),
		window.Channels,
		flags,
	})
	if liveNotModified(w, r, tag) {
		return
	}

	channels := make([]object, len(window.Channels))
	memo := newLiveArtCache()
	var allProgrammes []frontend.LiveProgramme
	for _, ch := range window.Channels {
		for _, p := range ch.Programmes {
			if p.Item != 0 {
				allProgrammes = append(allProgrammes, ch.Programmes...)
				break
			}
		}
	}
	if len(allProgrammes) > 0 {
		i.liveProgrammeTitleArt(r, token, allProgrammes, memo)
	}
	for idx, ch := range window.Channels {
		programmes := make([]object, len(ch.Programmes))
		for j, p := range ch.Programmes {
			programme, err := i.liveProgrammeWire(r, token, p, memo)
			if err != nil {
				fail(w, err, r)
				return
			}
			addArchiveFlags(programme, ch.ArchiveDays, p.Start, p.End, now)
			programmes[j] = programme
		}
		channels[idx] = object{
			"channelId":  ch.ChannelID,
			"state":      string(ch.State),
			"programmes": programmes,
		}
		if ch.ArchiveDays > 0 {
			channels[idx]["archiveDays"] = ch.ArchiveDays
		}
	}
	out := object{
		"from":       from.UTC().Format(time.RFC3339),
		"until":      until.UTC().Format(time.RFC3339),
		"revision":   window.Revision,
		"validUntil": window.ValidUntil.UTC().Format(time.RFC3339),
		"channels":   channels,
	}
	writeLiveCached(w, r, tag, out)
}

// Issue signed, current-policy image capabilities for this response page.
// Source URLs and credentials never enter the Vela wire model.
//
// One response reuses a signed URL for an image it already issued (a channel's
// logo repeats on every programme).
func (i *instance) liveArtwork(r *http.Request, token string, item frontend.RatingKey, channel string, images []frontend.Image) ([]object, error) {
	return i.liveArtworkCached(r, token, item, channel, images, nil)
}

type liveArtCache struct {
	issued map[string]object
}

func newLiveArtCache() *liveArtCache { return &liveArtCache{issued: map[string]object{}} }

func (i *instance) liveArtworkCached(r *http.Request, token string, item frontend.RatingKey, channel string, images []frontend.Image, memo *liveArtCache) ([]object, error) {
	out := []object{}
	if i.host.ImageGrants == nil || (item == 0 && channel == "") {
		return out, nil
	}
	for _, img := range images {
		key := fmt.Sprintf("%d\x00%s\x00%s\x00%s\x00%s", item, channel, img.Kind, img.Path, img.Tag)
		if memo != nil {
			if art, ok := memo.issued[key]; ok {
				out = append(out, art)
				continue
			}
		}
		secret, err := i.host.ImageGrants.Issue(r.Context(), frontend.ImageGrantRequest{Credential: token, Device: requestDevice(r), Item: item, Channel: channel, Image: img})
		if err != nil {
			return nil, err
		}
		art := object{"id": hash(img.Path), "kind": img.Kind, "tag": img.Tag, "sizes": []object{imageSize(img, secret)}}
		if memo != nil {
			memo.issued[key] = art
		}
		out = append(out, art)
	}
	return out, nil
}
