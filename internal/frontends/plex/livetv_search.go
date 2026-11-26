package plex

import (
	"context"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontends/plex/model"
)

func (s *Server) liveProviderSource() string {
	return "server://" + s.identity().MachineIdentifier + "/" + frontend.LiveTVProvider
}

func liveProgrammeOnAir(p frontend.LiveTVProgramme, now time.Time) bool {
	return !now.Before(p.Start) && now.Before(p.End)
}

func (s *Server) liveSearchProgrammeMetadata(r *http.Request, c frontend.LiveTVChannel, p frontend.LiveTVProgramme, now time.Time, source string) (model.Metadata, bool) {
	md := s.liveProgrammeMetadata(c, p)
	s.applyGuideChannelThumb(r, &md, c)
	onAir := liveProgrammeOnAir(p, now)
	if onAir && len(md.Media) > 0 {
		md.Media[0].Extra = append(md.Media[0].Extra, model.A("onAir", true))
	}
	if source != "" {
		md.Extra = append(md.Extra, model.A("source", source))
	}
	return md, onAir
}

// liveChannelMatchAiringMetadata shapes the on-now airing Metadata for a
// channel-name/call-sign match on /library/search. It keeps the PMS guide
// Media[0] channel attributes (channelTitle, channelCallSign,
// channelIdentifier, channelVcn, channelThumb, beginsAt, endsAt, onAir) and
// episodic grandparentTitle without inventing non-PMS attributes. Because
// Plex for iOS and Apple TV ignore SearchResult.Channel and render only
// Metadata.title (plus grandparentTitle for episodes) on search rows, title
// carries the matched channel name while the programme title is preserved in
// tagline/summary (and grandparentTitle for episodes).
func (s *Server) liveChannelMatchAiringMetadata(r *http.Request, c frontend.LiveTVChannel, p frontend.LiveTVProgramme, now time.Time, source string) (model.Metadata, bool) {
	md, onAir := s.liveSearchProgrammeMetadata(r, c, p, now, source)
	if !onAir || c.Name == "" {
		return md, onAir
	}
	progTitle := strings.TrimSpace(md.Title)
	showTitle := strings.TrimSpace(md.GrandparentTitle)
	if md.Type == "episode" {
		if (showTitle == "" || strings.EqualFold(showTitle, c.Name)) && progTitle != "" && !strings.EqualFold(progTitle, c.Name) {
			md.GrandparentTitle = progTitle
		}
	}
	progLabel := progTitle
	if showTitle != "" && !strings.EqualFold(showTitle, c.Name) {
		if progTitle != "" && !strings.EqualFold(showTitle, progTitle) {
			progLabel = showTitle + " — " + progTitle
		} else {
			progLabel = showTitle
		}
	}
	md.Title = c.Name
	if progLabel != "" && !strings.EqualFold(progLabel, c.Name) {
		if md.Type == "movie" && md.Tagline == "" {
			md.Tagline = progLabel
		}
		summary := strings.TrimSpace(md.Summary)
		if summary == "" {
			md.Summary = progLabel
		} else if !strings.HasPrefix(strings.ToLower(summary), strings.ToLower(progLabel)) {
			md.Summary = progLabel + " — " + summary
		}
	}
	if md.Thumb == "" {
		md.Thumb = s.liveGuideLogo(r, c)
	}
	return md, true
}

func (s *Server) canSearchLiveTV(ctx context.Context, pr *Request) (frontend.LiveTVSearch, bool) {
	if !s.liveTVEnabled() || pr == nil {
		return nil, false
	}
	if !s.svc().liveTV.VisibleTo(ctx, pr.User) {
		return nil, false
	}
	if p, ok := s.policy().(frontend.PlaybackAccessPolicy); ok && !p.CanPlay(pr.User, pr.Remote, s.now()) {
		return nil, false
	}
	search, ok := s.svc().liveTV.(frontend.LiveTVSearch)
	return search, ok
}

// Search shares the indexed, policy-filtered Live TV search with other apps.
func (s *Server) handleLiveChannelSearch(w http.ResponseWriter, r *http.Request) {
	limit, ok := searchLimit(w, r, 10, 50)
	if !ok {
		return
	}
	rawQuery := r.URL.Query().Get("query")
	query := strings.ToLower(strings.TrimSpace(rawQuery))
	if utf8.RuneCountInString(query) > 256 {
		writeHTTPError(w, http.StatusBadRequest)
		return
	}
	mc := &model.MediaContainer{Identifier: frontend.LiveTVProvider}
	if query == "" {
		s.write(w, r, mc)
		return
	}
	r, leave := s.enterSearch(r, rawQuery)
	defer leave()
	search, ok := s.svc().liveTV.(frontend.LiveTVSearch)
	if !ok {
		s.write(w, r, mc)
		return
	}
	results, err := search.SearchLiveTV(r.Context(), FromContext(r.Context()).User, query, limit)
	if err != nil {
		s.writeSearchErr(w, r, err, mc)
		return
	}
	source := s.liveProviderSource()
	for _, c := range results.Channels {
		mc.SearchResult = append(mc.SearchResult, model.SearchResult{Score: 1, Channel: s.liveSearchChannel(r, c)})
	}
	parents := map[string]frontend.LiveTVChannel{}
	for _, c := range results.ProgrammeChannels {
		parents[c.ID] = c
	}
	now := s.now()
	for _, p := range results.Programmes {
		c := parents[p.ChannelID]
		md, _ := s.liveSearchProgrammeMetadata(r, c, p, now, source)
		mc.SearchResult = append(mc.SearchResult, model.SearchResult{Score: 0.9, Metadata: &md})
	}
	mc.Size = len(mc.SearchResult)
	s.write(w, r, mc)
}

func (s *Server) liveSearchChannel(r *http.Request, c frontend.LiveTVChannel) *model.SearchChannel {
	categories := model.StringList{}
	if c.Group != "" {
		categories = append(categories, c.Group)
	}
	logo := s.liveGuideLogo(r, c)
	return &model.SearchChannel{
		ID:                 c.ID,
		Identifier:         c.ID,
		ChannelIdentifier:  c.ID,
		Slug:               c.ID,
		Key:                liveProviderPath + "/channels/" + url.PathEscape(c.ID),
		Type:               "channel",
		GridKey:            c.ID,
		VCN:                c.Number,
		Title:              c.Name,
		ShortTitle:         c.Name,
		CallSign:           c.Name,
		Thumb:              logo,
		CoverPoster:        logo,
		Language:           "eng",
		ProviderIdentifier: frontend.LiveTVProvider,
		Source:             s.liveProviderSource(),
		Categories:         categories,
	}
}

const (
	// Channel-name/call-sign matches outrank library title matches (max raw
	// ~1.16) so a matched channel's on-now airing lands first on /library/search.
	liveChannelMatchRawBase  = 1.25
	liveChannelMatchRawFloor = 1.18
)

type liveLibrarySearchResult struct {
	results           []model.SearchResult
	scores            []float64
	hasChannelMatches bool
}

// searchLiveTVLibrary runs indexed, policy-filtered Live TV search for
// /library/search. When the query matches channels by name/call sign/number,
// each matched channel emits its on-now airing Metadata (ranked first, with
// the channel name visible for clients like Plex for iOS/Apple TV that skip
// SearchResult.Channel) alongside its Channel element, and omits future
// airings of that channel. Matches on programme titles keep normal ranking
// and future airings.
func (s *Server) searchLiveTVLibrary(r *http.Request, pr *Request, query string, limit int) liveLibrarySearchResult {
	query = strings.TrimSpace(query)
	if query == "" || utf8.RuneCountInString(query) > 256 {
		return liveLibrarySearchResult{}
	}
	ctx := r.Context()
	search, ok := s.canSearchLiveTV(ctx, pr)
	if !ok {
		return liveLibrarySearchResult{}
	}
	results, err := search.SearchLiveTV(ctx, pr.User, query, limit)
	if err != nil {
		if !frontend.SearchWasSuperseded(ctx) && ctx.Err() == nil {
			s.log.Warn("live search unavailable", "error", err)
		}
		return liveLibrarySearchResult{}
	}
	source := s.liveProviderSource()
	now := s.now()
	var out liveLibrarySearchResult

	matchedByID := make(map[string]frontend.LiveTVChannel, len(results.Channels))
	matchedIDs := make([]string, 0, len(results.Channels))
	for _, c := range results.Channels {
		if _, dup := matchedByID[c.ID]; dup {
			continue
		}
		matchedByID[c.ID] = c
		matchedIDs = append(matchedIDs, c.ID)
	}

	onNowCap := min(len(matchedIDs), min(max(limit, 1), 20))
	onNowByChannel := make(map[string]frontend.LiveTVProgramme, onNowCap)
	for _, p := range results.Programmes {
		if _, matched := matchedByID[p.ChannelID]; !matched {
			continue
		}
		if liveProgrammeOnAir(p, now) {
			if _, exists := onNowByChannel[p.ChannelID]; !exists {
				onNowByChannel[p.ChannelID] = p
			}
		}
	}
	if onNowCap > 0 {
		missing := make([]string, 0, onNowCap)
		for _, id := range matchedIDs[:onNowCap] {
			if _, exists := onNowByChannel[id]; !exists {
				missing = append(missing, id)
			}
		}
		if len(missing) > 0 {
			if onNow, err := s.svc().liveTV.GuideForChannels(ctx, pr.User, now, now.Add(time.Second), missing); err == nil {
				for _, p := range onNow {
					if liveProgrammeOnAir(p, now) {
						if _, exists := onNowByChannel[p.ChannelID]; !exists {
							onNowByChannel[p.ChannelID] = p
						}
					}
				}
			}
		}
	}

	for i, id := range matchedIDs {
		c := matchedByID[id]
		out.hasChannelMatches = true
		airRaw := max(liveChannelMatchRawFloor+0.001, liveChannelMatchRawBase-float64(i)*0.0005)
		chanRaw := airRaw - 0.0001
		wireScore := math.Round(max(0.99, 1.0-float64(i)*0.0001)*10000) / 10000
		if i < onNowCap {
			if p, ok := onNowByChannel[c.ID]; ok {
				if md, onAir := s.liveChannelMatchAiringMetadata(r, c, p, now, source); onAir {
					out.results = append(out.results, model.SearchResult{
						Score:    wireScore,
						Metadata: &md,
					})
					out.scores = append(out.scores, airRaw)
				}
			} else {
				md := s.liveChannelMetadata(r, c)
				md.Extra = append(md.Extra, model.A("source", source))
				out.results = append(out.results, model.SearchResult{
					Score:    wireScore,
					Metadata: &md,
				})
				out.scores = append(out.scores, airRaw)
			}
		}
		out.results = append(out.results, model.SearchResult{
			Score:   wireScore,
			Channel: s.liveSearchChannel(r, c),
		})
		out.scores = append(out.scores, chanRaw)
	}

	parents := make(map[string]frontend.LiveTVChannel, len(results.ProgrammeChannels)+len(results.Channels))
	for _, c := range results.Channels {
		parents[c.ID] = c
	}
	for _, c := range results.ProgrammeChannels {
		parents[c.ID] = c
	}
	progRank := 0
	for _, p := range results.Programmes {
		if _, matched := matchedByID[p.ChannelID]; matched {
			// Channel-name matches return only the on-now airing above, not
			// future airings of that channel.
			continue
		}
		c := parents[p.ChannelID]
		md, onAir := s.liveSearchProgrammeMetadata(r, c, p, now, source)
		sc := 0.72
		if onAir {
			sc = 0.82
		}
		sc = max(0.1, sc-float64(progRank)*0.001)
		progRank++
		out.results = append(out.results, model.SearchResult{
			Score:    math.Round(sc*10000) / 10000,
			Metadata: &md,
		})
		out.scores = append(out.scores, sc)
	}
	// A programme's past airings rank just above its upcoming ones: they can
	// be watched now.
	for i, md := range s.catchUpSearch(r, query, limit) {
		sc := max(0.1, 0.78-float64(i)*0.001)
		out.results = append(out.results, model.SearchResult{
			Score:    math.Round(sc*10000) / 10000,
			Metadata: &md,
		})
		out.scores = append(out.scores, sc)
	}
	return out
}

func (s *Server) appendLiveSearchHubs(r *http.Request, mc *model.MediaContainer, query string, limit int) {
	if r.URL.Query().Get("sectionId") != "" {
		return
	}
	pr := FromContext(r.Context())
	search, ok := s.canSearchLiveTV(r.Context(), pr)
	if !ok {
		return
	}
	results, err := search.SearchLiveTV(r.Context(), pr.User, query, limit)
	if err != nil {
		if !frontend.SearchWasSuperseded(r.Context()) && r.Context().Err() == nil {
			s.log.Warn("live search unavailable", "error", err)
		}
		return
	}
	source := s.liveProviderSource()
	var liveHubs []model.Hub
	if len(results.Channels) > 0 {
		hub := model.Hub{
			Title:         "Live TV",
			Type:          "clip",
			HubIdentifier: "livetv",
			Context:       "hub.livetv",
			Style:         "shelf",
			Source:        source,
			Identifier:    frontend.LiveTVProvider,
		}
		for i, c := range results.Channels {
			md := s.liveChannelMetadata(r, c)
			md.Score = strconv.FormatFloat(reportedScore(searchScore(i, len(results.Channels))), 'f', 5, 64)
			md.Extra = append(md.Extra, model.A("source", source))
			hub.Metadata = append(hub.Metadata, md)
		}
		hub.Size = len(hub.Metadata)
		liveHubs = append(liveHubs, hub)
	}
	parents := map[string]frontend.LiveTVChannel{}
	for _, c := range results.ProgrammeChannels {
		parents[c.ID] = c
	}
	if len(results.Programmes) > 0 {
		now := s.now()
		hub := model.Hub{
			Title:         "On TV",
			Type:          "episode",
			HubIdentifier: "tv.plex.provider.epg",
			Context:       "hub.tv.plex.provider.epg",
			Style:         "shelf",
			Source:        source,
			Identifier:    frontend.LiveTVProvider,
		}
		for i, p := range results.Programmes {
			c := parents[p.ChannelID]
			md, _ := s.liveSearchProgrammeMetadata(r, c, p, now, source)
			md.Score = strconv.FormatFloat(reportedScore(searchScore(i, len(results.Programmes))), 'f', 5, 64)
			hub.Metadata = append(hub.Metadata, md)
		}
		hub.Size = len(hub.Metadata)
		liveHubs = append(liveHubs, hub)
	}
	if shows := s.catchUpSearch(r, query, limit); len(shows) > 0 {
		hub := model.Hub{
			Title:         shows[0].LibrarySectionTitle,
			Type:          "show",
			HubIdentifier: "catchup",
			Context:       "hub.catchup",
			Style:         "shelf",
		}
		for i := range shows {
			shows[i].Score = strconv.FormatFloat(reportedScore(searchScore(i, len(shows))), 'f', 5, 64)
		}
		hub.Metadata, hub.Size = shows, len(shows)
		liveHubs = append(liveHubs, hub)
	}
	if len(liveHubs) == 0 {
		return
	}
	insertAt := 0
	for i, h := range mc.Hub {
		if h.Size > 0 {
			insertAt = i + 1
		}
	}
	merged := make([]model.Hub, 0, len(mc.Hub)+len(liveHubs))
	merged = append(merged, mc.Hub[:insertAt]...)
	merged = append(merged, liveHubs...)
	merged = append(merged, mc.Hub[insertAt:]...)
	mc.Hub = merged
}
