package plex

import (
	"context"
	"strings"

	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontends/plex/model"
)

// homeLayoutFacts is the renderer's layout report: identity, title, type,
// context and the painted item count. Metadata, keys and programme sidecars
// stay on the wire row. Callers record the report when they paint the row.
func homeLayoutFacts(row model.Hub) (frontend.Hub, frontend.NeutralHomeRow) {
	count := max(row.Size, len(row.Metadata)+len(row.Directory))
	hub := frontend.Hub{ID: row.HubIdentifier, Title: row.Title, Type: frontend.MediaType(row.Type), Context: row.Context, ItemCount: count, PreviewOnFocus: row.PreviewOnFocus, RankedTop10: row.RankedTop10}
	neutral := frontend.NeutralHomeRow{ID: row.HubIdentifier, Title: row.Title, Type: frontend.MediaType(row.Type), Purpose: frontend.HomePurpose(frontend.Hub{ID: row.HubIdentifier, Type: frontend.MediaType(row.Type)}), ItemCount: count, PreviewOnFocus: row.PreviewOnFocus, RankedTop10: row.RankedTop10}
	return hub, neutral
}

// prepareHomeContinueAndOnDeck builds the Home Continue Watching and On Deck
// projections ahead of HomeAssembly so On Deck participates in layout slot
// placement, Off rules, and page row budgets while suppressing duplicate
// resume shelves.
func (s *Server) prepareHomeContinueAndOnDeck(ctx context.Context, pr *Request, hc frontend.HubContext, hubs []frontend.Hub, selected map[string]bool, includeContinue bool, count int) ([]frontend.Hub, *model.Hub, hubSpec, model.Hub) {
	stripOnDeck := func(in []frontend.Hub) []frontend.Hub {
		out := make([]frontend.Hub, 0, len(in))
		for _, h := range in {
			if h.ID != "home.ondeck" {
				out = append(out, h)
			}
		}
		return out
	}
	if !includeContinue {
		return stripOnDeck(hubs), nil, hubSpec{}, model.Hub{}
	}
	cwSpec := s.continueHub(ctx, pr, cwHome, Section{}, count)
	cwRendered := s.renderHub(ctx, pr, cwSpec, hubCtx{})
	if selected != nil && !selected["home.ondeck"] {
		return stripOnDeck(hubs), nil, cwSpec, cwRendered
	}
	spec := s.continueHub(ctx, pr, cwGlobal, Section{}, count)
	episodes := spec.Items[:0]
	for _, item := range spec.Items {
		if item.Type == frontend.TypeEpisode {
			episodes = append(episodes, item)
		}
	}
	spec.Items = episodes
	spec.ID, spec.Context, spec.Title, spec.Style, spec.key = "home.ondeck", "hub.home.ondeck", "On Deck", "shelf", "/hubs/home/onDeck"
	spec.included, spec.noPromoted, spec.plain = false, false, false
	ondeck := s.renderHub(ctx, pr, spec, hubCtx{})

	same := len(ondeck.Metadata) > 0 && len(ondeck.Metadata) == len(cwRendered.Metadata)
	keys := make(map[string]bool, len(cwRendered.Metadata))
	for _, item := range cwRendered.Metadata {
		keys[item.RatingKey] = true
	}
	for _, item := range ondeck.Metadata {
		same = same && keys[item.RatingKey]
	}
	cwShown := selected == nil || selected["home.continue"]
	if cwShown {
		if layout, ok := s.recommender().(frontend.HomeRowLayoutGate); ok && !layout.HomeRowEnabled(ctx, pr.User, hc, "home.continue") {
			cwShown = false
		}
	}
	if len(ondeck.Metadata) == 0 || (same && cwShown) {
		return stripOnDeck(hubs), nil, cwSpec, cwRendered
	}
	out := make([]frontend.Hub, 0, len(hubs)+1)
	replaced := false
	for _, h := range hubs {
		if h.ID == "home.ondeck" {
			if !replaced {
				out = append(out, spec.Hub)
				replaced = true
			}
			continue
		}
		out = append(out, h)
	}
	if !replaced {
		out = append(out, spec.Hub)
	}
	return out, &ondeck, cwSpec, cwRendered
}

// arrangeHomeRows reorders painted rows from facts captured at paint time.
// A missing provider or a layout error leaves the original rows in place.
// Empty and duplicate rows are dropped by the hubs handlers after this returns.
func (s *Server) arrangeHomeRows(ctx context.Context, pr *Request, hc frontend.HubContext, section string, rows []model.Hub, hubs []frontend.Hub, neutral []frontend.NeutralHomeRow) []model.Hub {
	if len(rows) != len(hubs) || len(rows) != len(neutral) {
		if s.log != nil {
			s.log.Warn("Home layout unavailable", "error", "paint facts diverged")
		}
		return rows
	}
	if _, ok := s.recommender().(frontend.NeutralHomeLayoutSource); !ok {
		if _, layout := s.recommender().(frontend.HomeLayoutSource); !layout {
			return rows
		}
	}
	if source, ok := s.recommender().(frontend.NeutralHomeLayoutSource); ok {
		sit := frontend.HomeSituation{Device: hc.Device, Daypart: hc.Daypart, Weekend: hc.Weekend, Now: hc.Now, LibraryID: hc.SectionID, Product: hc.Product, Platform: hc.Platform, SessionID: hc.SessionID, ClientID: hc.ClientID}
		ordered, err := source.FinalizeNeutralHome(ctx, pr.User, sit, section, neutral)
		if err != nil {
			return rows
		}
		byID := make(map[string]int, len(neutral))
		for i, row := range neutral {
			byID[row.ID] = i
		}
		out := make([]model.Hub, 0, len(ordered))
		for _, row := range ordered {
			if row.Present.Surface != nil {
				// A plugin surface row is drawn from the plugin's document;
				// Plex has no renderer for it.
				continue
			}
			if index, ok := byID[row.ID]; ok {
				out = append(out, presentHub(rows[index], row))
			}
		}
		return out
	}
	indices, err := s.exp().ArrangeHome(ctx, s.recommender(), pr.User, hc, section, hubs, neutral)
	if err != nil {
		if s.log != nil {
			s.log.Warn("Home layout unavailable", "error", err)
		}
		return rows
	}
	out := make([]model.Hub, 0, len(indices))
	for _, i := range indices {
		if i < 0 || i >= len(rows) {
			if s.log != nil {
				s.log.Warn("Home layout unavailable", "error", "index out of range")
			}
			return rows
		}
		out = append(out, rows[i])
	}
	return out
}

// presentHub draws a painted hub the way its page row asks (Present).
// Plex hub styles are shelf and hero: Hero is a hero hub, Wide a shelf of
// landscape art, Standard, Tiles and Track list a shelf. Numbered paints
// Top 10 rank art and Trailer on focus landscape art. The card follows the
// library. Plex rebuilds items itself, so it applies Hide watched from the
// viewer's state already painted on each item: no lookup.
func presentHub(h model.Hub, row frontend.NeutralHomeRow) model.Hub {
	p := row.Present
	if p.HideWatched {
		h = hideWatched(h)
	}
	h.PreviewOnFocus = p.Trailer
	h.RankedTop10 = p.Numbered
	h.Library = int64(p.Library)
	h.Layout = p.Layout
	if h.Type == "collection" && p.Card == "landscape" {
		for n := range h.Directory {
			if h.Directory[n].Art != "" {
				h.Directory[n].Thumb = h.Directory[n].Art
			}
		}
	}
	switch h.Layout {
	case "":
	case frontend.RowStyleHero:
		h.Style = "hero"
	default:
		h.Style = "shelf"
	}
	return h
}

// hideWatched drops the viewer's watched titles. A row with every title
// watched comes back empty, and the Home passes drop it.
func hideWatched(h model.Hub) model.Hub {
	if len(h.Metadata) == 0 {
		return h
	}
	kept, ok := keepHubMetadata(h, func(m model.Metadata) bool { return !plexWatched(m) })
	if !ok {
		kept.Metadata, kept.Size = nil, 0
	}
	return kept
}

// plexWatched reports a painted title the viewer has fully watched: a movie
// or episode with a view, a show or season with every episode viewed.
func plexWatched(m model.Metadata) bool {
	switch m.Type {
	case "show", "season":
		return m.LeafCount != nil && *m.LeafCount > 0 && m.ViewedLeafCount != nil && *m.ViewedLeafCount >= *m.LeafCount
	}
	return m.ViewCount > 0
}

// stableHomeHubs drops rows a client would draw empty, and keeps the first
// copy when the same hub was produced twice (promoted and section builders,
// or a recommender repeating a row the front end already built).
func stableHomeHubs(pr *Request, rows []model.Hub) []model.Hub {
	seen := map[string]bool{}
	out := make([]model.Hub, 0, len(rows))
	for _, h := range rows {
		if len(h.Metadata) == 0 && len(h.Directory) == 0 {
			continue
		}
		if h.HubIdentifier != "" {
			if seen[h.HubIdentifier] {
				continue
			}
			seen[h.HubIdentifier] = true
		}
		if h.PreviewOnFocus || h.Layout == frontend.RowStyleWide {
			h.Metadata = append([]model.Metadata(nil), h.Metadata...)
			landscapeMetadata(h.Metadata)
		}
		if h.RankedTop10 {
			h.Metadata = append([]model.Metadata(nil), h.Metadata...)
			h.Metadata = h.Metadata[:min(10, len(h.Metadata))]
			rankedMetadata(h.Metadata)
			h.Size = len(h.Metadata)
			h.TotalSize = model.Ptr(h.Size)
			h.More = false
			h.Meta = rankedDisplayImageMeta(pr, h.Type)
		}
		out = append(out, h)
	}
	return out
}

// heroReadyItem reports whether a painted title has the artwork iOS needs
// to draw a Home hero tile: a backdrop and a poster.
func heroReadyItem(m model.Metadata) bool {
	art, thumb := strings.TrimSpace(m.Art), strings.TrimSpace(m.Thumb)
	for _, img := range m.Image {
		switch img.Type {
		case "background":
			if art == "" {
				art = strings.TrimSpace(img.URL)
			}
		case "coverPoster":
			if thumb == "" {
				thumb = strings.TrimSpace(img.URL)
			}
		}
	}
	if art == "" {
		art = strings.TrimSpace(m.GrandparentArt)
	}
	if thumb == "" {
		thumb = strings.TrimSpace(m.GrandparentThumb)
	}
	return art != "" && thumb != ""
}

// paintHeroItems keeps titles that can fill a hero tile and rewrites Size
// and HubKey to match. A hub with no such titles is omitted so the client
// never reserves the slot.
func paintHeroItems(h model.Hub) (model.Hub, bool) {
	return keepHubMetadata(h, heroReadyItem)
}

// keepHubMetadata keeps the titles keep accepts and rewrites Size and HubKey
// to match. ok is false when nothing was kept; the hub is then unchanged.
func keepHubMetadata(h model.Hub, keep func(model.Metadata) bool) (model.Hub, bool) {
	kept := make([]model.Metadata, 0, len(h.Metadata))
	keys := make([]string, 0, len(h.Metadata))
	for _, m := range h.Metadata {
		if !keep(m) {
			continue
		}
		kept = append(kept, m)
		if m.RatingKey != "" {
			keys = append(keys, m.RatingKey)
		}
	}
	if len(kept) == 0 {
		return h, false
	}
	if len(kept) != len(h.Metadata) {
		if len(keys) > 0 {
			h.HubKey = "/library/metadata/" + strings.Join(keys, ",")
		} else {
			h.HubKey = ""
		}
	}
	h.Metadata = kept
	h.Size = len(kept)
	return h, true
}

// heroReadyHomeHubs is the Home-only pass after stableHomeHubs. Plex iOS
// draws the first hub, and any style=hero hub, as the large spotlight. A
// cold or not-yet-warm row with no backdrop must not be sent: either the
// next already-painted, art-ready shelf is promoted into that slot, or
// the hero is omitted. Later shelves stay as they were.
//
// The owner's row style wins where it is set: a lead row the owner made
// Standard or Wide keeps that style (it still needs hero art, as iOS
// spotlights the lead hub), and an owner Hero row with no art-ready title
// falls back to a shelf for this response instead of disappearing. Such a
// row never takes the lead slot: it follows the first art-ready row.
func heroReadyHomeHubs(rows []model.Hub) []model.Hub {
	out := make([]model.Hub, 0, len(rows))
	var waiting []model.Hub
	hasLead := false
	for _, h := range rows {
		if h.Type == "collection" && len(h.Directory) > 0 {
			h.Style = "shelf"
			out = append(out, h)
			continue
		}
		lead := !hasLead
		if lead || strings.EqualFold(h.Style, "hero") {
			painted, ok := paintHeroItems(h)
			if !ok {
				if h.Layout == frontend.RowStyleHero {
					h.Style = "shelf"
					if lead {
						waiting = append(waiting, h)
					} else {
						out = append(out, h)
					}
				}
				continue
			}
			if h.Layout == "" || h.Layout == frontend.RowStyleHero {
				painted.Style = "hero"
			}
			out = append(out, painted)
			hasLead = true
		} else {
			out = append(out, h)
		}
		if len(waiting) > 0 {
			out = append(out, waiting...)
			waiting = nil
		}
	}
	return out
}

// ownerHeroHubs is the library page pass: an owner Hero row keeps only titles
// with hero art, and falls back to a shelf when none has it yet.
func ownerHeroHubs(rows []model.Hub) []model.Hub {
	for i, h := range rows {
		if h.Layout != frontend.RowStyleHero {
			continue
		}
		if painted, ok := paintHeroItems(h); ok {
			rows[i] = painted
		} else {
			rows[i].Style = "shelf"
		}
	}
	return rows
}

// landscapeMetadata paints only response copies, retaining movie/show identities.
func landscapeMetadata(items []model.Metadata) {
	for i := range items {
		m := &items[i]
		art := m.Art
		if art == "" {
			art = m.GrandparentArt
		}
		if art != "" {
			m.Thumb = art
			m.Image = append([]model.Image(nil), m.Image...)
			for n := range m.Image {
				if m.Image[n].Type == "coverPoster" {
					m.Image[n].URL = art
				}
			}
		}
	}
}
