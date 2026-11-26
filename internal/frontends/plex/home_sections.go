package plex

import (
	"context"
	"net/http"
	"strconv"
	"strings"

	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/kit/homefilter"
	"github.com/braedonsaunders/ether/internal/frontends/plex/model"
)

func isVideoSection(sec Section) bool {
	return sec.PluginID == "" && (sec.Type == frontend.TypeMovie || sec.Type == frontend.TypeShow)
}

func isPluginKindSection(sec Section) bool {
	return sec.PluginID != ""
}

func isCustomTVSection(sec Section) bool {
	return sec.Type == frontend.TypeShow && isKindSection(sec) && !isPluginKindSection(sec)
}

func parseContentDirectories(raw string) ([]int, bool) {
	var ids []int
	playlists := false
	for _, token := range strings.Split(raw, ",") {
		token = strings.TrimSpace(token)
		if token == "playlists" {
			playlists = true
			continue
		}
		if n, err := strconv.Atoi(token); err == nil && n > 0 {
			ids = append(ids, n)
		}
	}
	return ids, playlists
}

func (s *Server) allowedContentSections(u *frontend.User, ids []int) []Section {
	var out []Section
	seen := map[int]bool{}
	for _, id := range ids {
		if seen[id] {
			continue
		}
		if sec, ok := s.sectionByID(id); ok && (isVideoSection(sec) || isKindSection(sec)) && s.canSeeSection(u, id) {
			seen[id] = true
			out = append(out, sec)
		}
	}
	return out
}

func (s *Server) allowedSectionIDs(u *frontend.User, ids []int) []int {
	secs := s.allowedContentSections(u, ids)
	out := make([]int, len(secs))
	for i, sec := range secs {
		out[i] = sec.ID
	}
	return out
}

// firstPinnedSectionID resolves the single library section on Home that owns
// cross-library Continue Watching / On Deck when excludeContinueWatching is
// not set. Unknown, non-video, or unauthorized directory tokens are skipped;
// when pinned names none, the viewer's first accessible video section wins.
func (s *Server) firstPinnedSectionID(u *frontend.User, pinned string) int {
	for _, token := range strings.Split(pinned, ",") {
		id, err := strconv.Atoi(strings.TrimSpace(token))
		if err != nil || id <= 0 {
			continue
		}
		if sec, ok := s.sectionByID(id); ok && isVideoSection(sec) && s.canSeeSection(u, id) {
			return sec.ID
		}
	}
	for _, sec := range s.userSections(u) {
		if isVideoSection(sec) {
			return sec.ID
		}
	}
	return 0
}

// libraryHubID is a row's hub id inside one library's block, <rowID>@<lib>,
// so the blocks of two pinned libraries (and their promoted library pages)
// never emit the same hubIdentifier. Cross-library rows keep their id, PMS's
// own section rows (movie.recentlyadded.<section>, …) keep the identity
// clients know them by, and a page row the engine built for one library
// already carries its @lib.
func libraryHubID(id string, sec Section) string {
	if id == "" || id == "home.playlists" || pmsBuiltInHub(id) {
		return id
	}
	if strings.Contains(id, "@") {
		return id
	}
	return id + "@" + strconv.FormatInt(int64(sectionLibraryID(sec)), 10)
}

// rowHubID is the producer's row id behind a library block's hub id.
func rowHubID(id string) string {
	base, _, _ := strings.Cut(id, "@")
	return base
}

// candidateHubIDs returns id followed by its producer row id, so
// /hubs/items continuations still resolve after emittedHubs expires or the
// server restarts.
func candidateHubIDs(id string) []string {
	if base := rowHubID(id); base != id {
		return []string{id, base}
	}
	return []string{id}
}

func matchHubID(hID string, candidates []string) bool {
	for _, c := range candidates {
		if hID == c {
			return true
		}
	}
	return false
}

func (s *Server) handleSectionHubs(w http.ResponseWriter, r *http.Request) {
	s.sectionHubs(w, r, false)
}

func (s *Server) handlePromotedSectionHubs(w http.ResponseWriter, r *http.Request) {
	s.sectionHubs(w, r, true)
}

func (s *Server) sectionHubs(w http.ResponseWriter, r *http.Request, promotedOnly bool) {
	sec, ok := s.sectionParam(w, r)
	if !ok {
		return
	}
	route := "hubs/sections/" + strconv.Itoa(sec.ID)
	if promotedOnly {
		route += "/promoted"
	}
	s.serveHome(w, r, route, func(_ context.Context, r *http.Request) homeBuild {
		return s.buildSectionHubs(r, sec, promotedOnly)
	})
}

type pluginSectionPaint struct {
	pages  []hubRender
	layout []frontend.NeutralHomeRow
	served map[string]frontend.Hub
}

func (p pluginSectionPaint) rows(painted []model.Hub) []model.Hub {
	var out []model.Hub
	for i, row := range painted {
		if row.Size > 0 {
			out = append(out, presentHub(row, p.layout[i]))
		}
	}
	return out
}

func (s *Server) pluginSectionHubs(ctx context.Context, pr *Request, sec Section, count int) ([]model.Hub, map[string]frontend.Hub) {
	plan := s.preparePluginSectionHubs(ctx, pr, sec, count)
	return plan.rows(s.paintHubPages(ctx, pr, plan.pages)), plan.served
}

// preparePluginSectionHubs prepares a plugin library's first rows tab,
// already arranged by that page (order, length, Look), with their own row
// ids, drawn the way each row's Present asks.
func (s *Server) preparePluginSectionHubs(ctx context.Context, pr *Request, sec Section, visibleCount int) pluginSectionPaint {
	exp := s.exp()
	reader, ok := exp.(frontend.PluginLibraryRowsReader)
	if !ok || visibleCount <= 0 {
		return pluginSectionPaint{}
	}
	rows, err := reader.PluginLibraryRows(ctx, pr.User, plexDevice(pr), sec.IDString(), "")
	if err != nil {
		s.log.Warn("plugin section rows", "section", sec.ID, "err", err)
	}
	secCopy := sec
	vis := s.sectionVisibility(sec)
	plan := pluginSectionPaint{served: map[string]frontend.Hub{}}
	for _, row := range rows {
		if row.Layout.Present.Surface != nil {
			continue
		}
		rawItems := row.Items
		if len(rawItems) == 0 {
			for _, e := range row.Entries {
				if e.Title != nil {
					rawItems = append(rawItems, *e.Title)
				}
			}
		}
		if len(rawItems) == 0 {
			continue
		}
		items := rawItems
		hasExtra := len(items) > visibleCount
		if hasExtra {
			items = items[:visibleCount]
		}
		plan.layout = append(plan.layout, frontend.NeutralHomeRow{Present: row.Layout.Present})
		plan.pages = append(plan.pages, hubRender{titles: items, paint: func(ctx context.Context) model.Hub {
			byKey := make(map[string]*frontend.Title, len(items))
			for i := range items {
				byKey[items[i].Key.String()] = &items[i]
			}
			md := s.renderTitles(ctx, pr, items, renderOpts{media: true, storedOnly: true, vis: &vis, section: &secCopy})
			if len(md) == 0 {
				return model.Hub{}
			}
			keys := make([]string, 0, len(md))
			for i := range md {
				md[i].Type = kindTypeForBase(string(sec.Type), md[i].Type, byKey[md[i].RatingKey])
				keys = append(keys, md[i].RatingKey)
			}
			fh := frontend.Hub{ID: row.ID, Title: row.Title, Type: sec.Type, Items: rawItems, Promoted: true}
			s.rememberHub(ctx, pr, hubSpec{Hub: fh}, hubCtx{section: sec.ID})
			total := len(rawItems)
			if hasExtra && total <= len(md) {
				total = len(md) + 1
			}
			mh := model.Hub{
				HubKey:        "/library/metadata/" + strings.Join(keys, ","),
				Key:           hubItemsKey(fh, hubCtx{section: sec.ID}),
				Title:         row.Title,
				Type:          string(sec.Type),
				HubIdentifier: row.ID,
				Context:       "hub." + string(sec.Type) + "." + pluginRowContext(row.ID),
				Size:          len(md),
				TotalSize:     model.Ptr(total),
				More:          hasExtra || total > len(md),
				Style:         "shelf",
				Promoted:      model.Ptr(true),
				Metadata:      md,
			}
			plan.served[row.ID] = fh
			return mh
		}})
	}
	return plan
}

// pluginRowContext is the Plex context suffix of a plugin library row
// ("plugin:<view>:shelf:<key>" → "shelf"); the prefix is the library's Plex
// base type, which Plex clients read.
func pluginRowContext(id string) string {
	parts := strings.Split(id, ":")
	if len(parts) < 3 || parts[2] == "" {
		return "recentlyadded"
	}
	return strings.ToLower(parts[2])
}

func (s *Server) buildSectionHubs(r *http.Request, sec Section, promotedOnly bool) homeBuild {
	batch := &shelfBatch{}
	ctx := withStoredPolicy(context.WithValue(r.Context(), shelfBatchKey{}, batch))
	defer func() { s.enqueueShelves(batch.records) }()
	pr := FromContext(ctx)
	visibleCount := countParam(r, 12)
	count := 200
	mc := hubContainer()
	mc.LibrarySectionID = model.Ptr(sec.ID)
	mc.LibrarySectionTitle = sec.Title
	mc.LibrarySectionUUID = sec.UUID
	if isPluginKindSection(sec) {
		hubs, served := s.pluginSectionHubs(ctx, pr, sec, visibleCount)
		mc.Hub = ownerHeroHubs(stableHomeHubs(pr, hubs))
		mc.Size = len(mc.Hub)
		return homeBuild{mc: mc, served: served, section: sec.ID}
	}
	if !isVideoSection(sec) {
		return homeBuild{mc: mc, section: sec.ID}
	}
	secCopy := sec
	hc := hubCtx{section: sec.ID, visible: visibleCount, renderSec: &secCopy}
	pc := s.hubContext(pr, sec.ID)
	vis := s.sectionVisibility(sec)
	served := map[string]frontend.Hub{}
	var layoutHubs []frontend.Hub
	var layoutNeutral []frontend.NeutralHomeRow
	var specs []hubSpec

	if !promotedOnly && !metadataOptionOn(r.URL.Query().Get("excludeContinueWatching")) && (!isKindSection(sec) || isCustomTVSection(sec)) {
		cw := s.continueHub(ctx, pr, cwSection, sec, visibleCount)
		if !isCustomTVSection(sec) {
			// Built-in Movies/TV keep their hide rules (#719); custom TV
			// libraries are scoped inside continueHub.
			cw.Hub.Items = s.filterSectionMembers(ctx, sec, cw.Hub.Items)
		}
		specs = append(specs, cw)
	}
	if !isKindSection(sec) {
		if ra, err := s.recentlyAddedHub(ctx, pr, sec, count); err == nil {
			specs = append(specs, ra)
		} else {
			s.log.Warn("recently added hub", "section", sec.ID, "err", err)
		}
	}
	var sectionPlaylist *model.Hub
	if !promotedOnly && r.URL.Query().Get("includeLibraryPlaylists") == "1" {
		if h, ok := s.sectionPlaylistsHub(ctx, pr, sec.ID, visibleCount); ok {
			sectionPlaylist = &h
		}
	}
	provisional := false
	if s.recommender() != nil {
		hubs, err := s.recommendSectionFor(ctx, pr, sec, pc, s.saferFetchCount(pr.User, count))
		if err != nil {
			s.log.Warn("section hubs", "section", sec.ID, "err", err)
		}
		provisional = !hasRecommendedRows(hubs)
		for _, h := range hubs {
			if view, ok := frontend.SectionView(h, sec.Type); ok {
				if promotedOnly {
					view.Promoted = true
				}
				specs = append(specs, hubSpec{Hub: view})
			}
		}
	}
	if !isKindSection(sec) {
		// Mixed discovery rows (watchlist, Rediscover) keep only this section's titles.
		for _, hs := range s.discoveryHubs(ctx, pr, count) {
			if h, ok := frontend.SectionView(hs.Hub, sec.Type); ok {
				h.Items = s.filterSectionMembers(ctx, sec, h.Items)
				if len(h.Items) == 0 {
					continue
				}
				if promotedOnly {
					h.Promoted = true
				}
				hs.Hub = h
				specs = append(specs, hs)
			}
		}
	}
	pages := make([]hubRender, 0, len(specs))
	for _, spec := range specs {
		pages = append(pages, s.prepareHub(ctx, pr, spec, hc, vis))
	}
	mc.Hub = s.paintHubPages(ctx, pr, pages)
	for i, row := range mc.Hub {
		hub, neutral := homeLayoutFacts(row)
		layoutHubs = append(layoutHubs, hub)
		layoutNeutral = append(layoutNeutral, neutral)
		served[row.HubIdentifier] = specs[i].Hub
	}
	sectionName := "library:" + strconv.Itoa(sec.ID)
	mc.Hub = ownerHeroHubs(stableHomeHubs(pr, s.arrangeHomeRows(ctx, pr, pc, sectionName, mc.Hub, layoutHubs, layoutNeutral)))
	if promotedOnly {
		for i := range mc.Hub {
			oldID := mc.Hub[i].HubIdentifier
			newID := libraryHubID(oldID, sec)
			if newID == oldID {
				continue
			}
			mc.Hub[i].HubIdentifier = newID
			if h, ok := served[oldID]; ok {
				served[newID] = h
				rem := h
				rem.ID = newID
				s.rememberHub(ctx, pr, hubSpec{Hub: rem}, hc)
				if strings.HasPrefix(mc.Hub[i].Key, "/hubs/items/") {
					mc.Hub[i].Key = hubItemsKey(rem, hubCtx{section: sec.ID})
				}
			}
		}
	}
	if sectionPlaylist != nil {
		at := 0
		for i, h := range mc.Hub {
			if strings.Contains(h.HubIdentifier, ".recentlyadded.") {
				at = i + 1
				break
			}
			if isCustomTVSection(sec) && h.HubIdentifier == "tv.inprogress."+sec.IDString() {
				at = i + 1
			}
		}
		mc.Hub = append(mc.Hub, model.Hub{})
		copy(mc.Hub[at+1:], mc.Hub[at:])
		mc.Hub[at] = *sectionPlaylist
	}

	s.log.Debug("section hubs rendered", "section", sec.ID, "user", pr.User.ID, "count", count, "rows", len(mc.Hub))
	mc.Size = len(mc.Hub)
	return homeBuild{mc: mc, served: served, section: sec.ID, provisional: provisional}
}

const defaultHomeRowBudget = 20

func (s *Server) homeRowBudget(ctx context.Context, pr *Request, hc frontend.HubContext) int {
	if src, ok := s.recommender().(frontend.HomeRowBudgetSource); ok {
		if n := src.HomeRowBudget(ctx, pr.User, hc); n > 0 {
			return n
		}
	}
	return defaultHomeRowBudget
}

func isCrossLibraryHub(id string) bool {
	switch id {
	case "home.continue", "home.ondeck", "home.playlists":
		return true
	default:
		return false
	}
}

func sectionSubsetParents(secs []Section) map[int]int {
	if len(secs) < 2 {
		return nil
	}
	views := make([]frontend.View, len(secs))
	for i, sec := range secs {
		views[i] = frontend.View{
			ID:        strconv.Itoa(sec.ID),
			LibraryID: sectionLibraryID(sec),
			Name:      sec.Title,
			Type:      sec.Type,
			PluginID:  sec.PluginID,
			KindKey:   sec.KindKey,
			Subset:    sec.Subset,
		}
	}
	raw := frontend.SubsetParents(views)
	if len(raw) == 0 {
		return nil
	}
	out := make(map[int]int, len(raw))
	for childStr, parentStr := range raw {
		c, err1 := strconv.Atoi(childStr)
		p, err2 := strconv.Atoi(parentStr)
		if err1 == nil && err2 == nil {
			out[c] = p
		}
	}
	return out
}

func hubItemKeys(h model.Hub) []string {
	if len(h.Metadata) > 0 {
		keys := make([]string, 0, len(h.Metadata))
		for _, m := range h.Metadata {
			k := m.RatingKey
			if m.GrandparentRatingKey != "" {
				k = m.GrandparentRatingKey
			} else if m.Type == "season" && m.ParentRatingKey != "" {
				k = m.ParentRatingKey
			}
			if k != "" {
				keys = append(keys, k)
			}
		}
		return keys
	}
	if len(h.Directory) > 0 {
		keys := make([]string, 0, len(h.Directory))
		for _, d := range h.Directory {
			k := d.Key
			if k == "" {
				k = d.ID
			}
			if k != "" {
				keys = append(keys, k)
			}
		}
		return keys
	}
	return nil
}

type parentHubIndex struct {
	all   map[string]bool
	byRow map[string]map[string]bool
}

func indexParentHubRows(rows []model.Hub, secID int, maxRows int) parentHubIndex {
	idx := parentHubIndex{
		all:   map[string]bool{},
		byRow: map[string]map[string]bool{},
	}
	counted := 0
	for _, mh := range rows {
		if isCrossLibraryHub(mh.HubIdentifier) {
			continue
		}
		if maxRows > 0 && counted >= maxRows {
			break
		}
		counted++
		stem := rowHubID(mh.HubIdentifier)
		rowSet := idx.byRow[stem]
		if rowSet == nil {
			rowSet = map[string]bool{}
			idx.byRow[stem] = rowSet
		}
		for _, k := range hubItemKeys(mh) {
			idx.all[k] = true
			rowSet[k] = true
		}
	}
	return idx
}

func nonEmptyUniqueHubs(rows []model.Hub) []model.Hub {
	seen := make(map[string]bool, len(rows))
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
		out = append(out, h)
	}
	return out
}

func trimSectionLibraryRows(rows []model.Hub, cap int) []model.Hub {
	out := make([]model.Hub, 0, len(rows))
	used := 0
	for _, mh := range rows {
		if isCrossLibraryHub(mh.HubIdentifier) {
			out = append(out, mh)
			continue
		}
		if used >= cap {
			continue
		}
		out = append(out, mh)
		used++
	}
	return out
}

// buildScopedHomeHubs builds PMS-shaped Home responses when contentDirectoryID
// scopes /hubs or /hubs/promoted to one or more pinned directories. Each
// library contributes only its own rows and titles of its own media type and
// visibility; cross-library Continue Watching / On Deck appears at most once
// on the first pinned library when excludeContinueWatching is not set.
// inLibraryBlock reports whether a row belongs in sec's block of a scoped
// Home: any combined row (already narrowed to sec's titles), or the row a
// one-row-per-library page row built for sec's library.
func inLibraryBlock(mh model.Hub, sec Section) bool {
	return mh.Library == 0 || mh.Library == int64(sectionLibraryID(sec))
}

func (s *Server) buildScopedHomeHubs(r *http.Request, promotedOnly bool) homeBuild {
	ctx := withStoredPolicy(r.Context())
	pr := FromContext(ctx)
	q := r.URL.Query()
	count := countParam(r, 12)
	rawDir := strings.TrimSpace(q.Get("contentDirectoryID"))
	dirIDs, wantPlaylists := parseContentDirectories(rawDir)
	allowedSecs := s.allowedContentSections(pr.User, dirIDs)
	if len(allowedSecs) == 0 && !wantPlaylists {
		return homeBuild{mc: hubContainer()}
	}
	perDirectory := !strings.Contains(rawDir, ",")
	shelves := promotedOnly && perDirectory && len(allowedSecs) == 1 && isVideoSection(allowedSecs[0]) && !wantPlaylists && s.wantsCollectionShelves(pr)
	rawPinned := strings.TrimSpace(q.Get("pinnedContentDirectoryID"))
	pinnedIDs, _ := parseContentDirectories(rawPinned)
	explicitPinnedSecs := s.allowedContentSections(pr.User, pinnedIDs)
	pinned := rawPinned
	if shelves && pinned == "" {
		pinned = s.defaultPinnedDirectories(pr.User)
	}
	var selected map[string]bool
	if ids := q["identifier"]; len(ids) > 0 {
		selected = map[string]bool{}
		for _, v := range ids {
			for _, id := range strings.Split(v, ",") {
				selected[id] = true
			}
		}
	}
	firstSec := s.firstPinnedSectionID(pr.User, pinned)
	includeContinue := false
	if !metadataOptionOn(q.Get("excludeContinueWatching")) && firstSec != 0 {
		for _, sec := range allowedSecs {
			if sec.ID == firstSec {
				includeContinue = true
				break
			}
		}
	}
	if includeContinue {
		if layout, ok := s.recommender().(frontend.HomeRowLayoutGate); ok && !layout.HomeRowEnabled(ctx, pr.User, s.hubContext(pr, 0), "home.continue") {
			includeContinue = false
		}
	}
	var parentPool []Section
	if len(allowedSecs) > 1 {
		parentPool = allowedSecs
	} else if len(allowedSecs) == 1 && len(explicitPinnedSecs) > 1 {
		parentPool = explicitPinnedSecs
	}
	parentOf := sectionSubsetParents(parentPool)

	needDiscovery := false
	for _, sec := range allowedSecs {
		if !isKindSection(sec) {
			needDiscovery = true
			break
		}
	}
	if !needDiscovery && selected == nil && len(parentOf) > 0 {
		for _, sec := range allowedSecs {
			if parentID := parentOf[sec.ID]; parentID != 0 {
				if pSec, ok := s.sectionByID(parentID); ok && !isKindSection(pSec) {
					needDiscovery = true
					break
				}
			}
		}
	}
	var sharedDisc []frontend.Hub
	if needDiscovery {
		disc, filter, err := s.sharedDiscovery(ctx, pr, s.hubContext(pr, 0), rawDir, pinned, count)
		if filter != nil {
			ctx = homefilter.With(ctx, filter)
		}
		if err != nil {
			s.log.Warn("home discovery hubs", "err", err)
		}
		sharedDisc = disc
	}

	type finishSection func([]model.Hub) ([]model.Hub, map[string]frontend.Hub, bool)
	var pages []hubRender
	prepareSection := func(sec Section, withContinue bool, sel map[string]bool) finishSection {
		secServed := map[string]frontend.Hub{}
		if isPluginKindSection(sec) {
			plan := s.preparePluginSectionHubs(ctx, pr, sec, count)
			start := len(pages)
			pages = append(pages, plan.pages...)
			return func(painted []model.Hub) ([]model.Hub, map[string]frontend.Hub, bool) {
				kindHubs := plan.rows(painted[start : start+len(plan.pages)])
				var out []model.Hub
				for _, mh := range kindHubs {
					if !inLibraryBlock(mh, sec) {
						continue
					}
					id := libraryHubID(mh.HubIdentifier, sec)
					if sel != nil && !sel[mh.HubIdentifier] && !sel[id] {
						continue
					}
					if src, ok := plan.served[mh.HubIdentifier]; ok {
						secServed[id] = src
					}
					mh.HubIdentifier = id
					if strings.EqualFold(mh.Style, "hero") {
						mh.Style = "shelf"
					}
					out = append(out, mh)
				}
				return nonEmptyUniqueHubs(out), secServed, false
			}
		}
		secCopy := sec
		renderHC := hubCtx{visible: count, renderSec: &secCopy}
		pc := s.hubContext(pr, sec.ID)
		pc.HomeScoped = true
		vis := s.sectionVisibility(sec)
		progress := s.homeProgressSpecs(ctx, pr, sel, withContinue && sec.ID == firstSec, count)
		secProv := false
		var specs []hubSpec

		if !isKindSection(sec) {
			if ra, err := s.recentlyAddedHub(ctx, pr, sec, count); err == nil {
				specs = append(specs, ra)
			}
		}
		if s.recommender() != nil {
			sectionHubs, err := s.recommendSectionFor(ctx, pr, sec, pc, s.saferFetchCount(pr.User, count))
			if err != nil {
				s.log.Warn("section home hubs", "section", sec.ID, "err", err)
			}
			if !hasRecommendedRows(sectionHubs) {
				secProv = true
			}
			for _, h := range sectionHubs {
				view, ok := frontend.SectionView(h, sec.Type)
				if !ok {
					continue
				}
				if promotedOnly {
					view.Promoted = true
				}
				scopedID := libraryHubID(view.ID, sec)
				rem := view
				rem.ID = scopedID
				s.rememberHub(ctx, pr, hubSpec{Hub: rem}, hubCtx{section: sec.ID})
				specs = append(specs, hubSpec{Hub: view, key: hubItemsKey(rem, hubCtx{section: sec.ID})})
			}
		}
		if !isKindSection(sec) {
			for _, dh := range sharedDisc {
				view, ok := frontend.SectionView(dh, sec.Type)
				if !ok {
					continue
				}
				view.Items = s.filterSectionMembers(ctx, sec, view.Items)
				if len(view.Items) == 0 {
					continue
				}
				if promotedOnly {
					view.Promoted = true
				}
				scopedID := libraryHubID(view.ID, sec)
				rem := view
				rem.ID = scopedID
				s.rememberHub(ctx, pr, hubSpec{Hub: rem}, hubCtx{section: sec.ID})
				specs = append(specs, hubSpec{Hub: view, key: hubItemsKey(rem, hubCtx{section: sec.ID})})
			}
		}
		pageStart := len(pages)
		for _, spec := range progress {
			pages = append(pages, s.prepareHub(ctx, pr, spec, hubCtx{}, s.visibility()))
		}
		for _, spec := range specs {
			pages = append(pages, s.prepareHub(ctx, pr, spec, renderHC, vis))
		}
		pageEnd := len(pages)
		return func(rows []model.Hub) ([]model.Hub, map[string]frontend.Hub, bool) {
			var secRows []model.Hub
			var layoutHubs []frontend.Hub
			var layoutNeutral []frontend.NeutralHomeRow
			origByID := map[string]frontend.Hub{}
			appendRow := func(mh model.Hub, src frontend.Hub, record bool) {
				scoped := libraryHubID(mh.HubIdentifier, sec)
				if sel != nil && !sel[mh.HubIdentifier] && !sel[scoped] {
					return
				}
				secRows = append(secRows, mh)
				hFact, nFact := homeLayoutFacts(mh)
				layoutHubs = append(layoutHubs, hFact)
				layoutNeutral = append(layoutNeutral, nFact)
				if record {
					origByID[mh.HubIdentifier] = src
				}
			}
			painted := rows[pageStart:pageEnd]
			if len(progress) > 0 {
				appendRow(painted[0], progress[0].Hub, true)
				_, ondeckHub := s.placeHomeOnDeck(ctx, pr, pc, nil, sel, progress, painted[:len(progress)])
				if ondeckHub != nil {
					appendRow(*ondeckHub, frontend.Hub{ID: "home.ondeck", Title: ondeckHub.Title, Type: frontend.TypeEpisode, Promoted: true}, true)
				}
			}
			for i, row := range painted[len(progress):] {
				appendRow(row, specs[i].Hub, true)
			}
			if sec.ID == firstSec {
				if source, ok := s.recommender().(collectionCardHubSource); ok {
					for _, h := range source.CollectionCardHubs(ctx, pr.User, pc, count) {
						appendRow(s.renderCollectionCardHub(ctx, pr, h), h, false)
					}
				}
			}
			secRows = s.arrangeHomeRows(ctx, pr, pc, "home", secRows, layoutHubs, layoutNeutral)
			out := make([]model.Hub, 0, len(secRows))
			for _, mh := range secRows {
				if !inLibraryBlock(mh, sec) {
					continue
				}
				oldID := mh.HubIdentifier
				if mh.Type != "collection" {
					mh.HubIdentifier = libraryHubID(oldID, sec)
				}
				if mh.HubIdentifier != "home.continue" && strings.EqualFold(mh.Style, "hero") {
					mh.Style = "shelf"
				}
				if src, ok := origByID[oldID]; ok {
					secServed[mh.HubIdentifier] = src
				}
				out = append(out, mh)
			}
			return nonEmptyUniqueHubs(out), secServed, secProv
		}
	}

	homeBudget := s.homeRowBudget(ctx, pr, s.hubContext(pr, 0))
	numParents := 0
	for _, sec := range parentPool {
		if parentOf[sec.ID] == 0 {
			numParents++
		}
	}
	parentRefCap := homeBudget
	if len(parentPool) > 1 && numParents > 0 {
		parentRefCap = max(homeBudget/numParents, 1)
	}

	neededParents := map[int]bool{}
	if selected == nil {
		for _, sec := range allowedSecs {
			if pID := parentOf[sec.ID]; pID != 0 {
				neededParents[pID] = true
			}
		}
	}

	// Select each library's pages before painting so pinned libraries share
	// the same metadata reads. Keep parent and subset layout in their usual order.
	finish := make(map[int]finishSection, len(allowedSecs))
	for _, sec := range allowedSecs {
		if selected != nil || parentOf[sec.ID] == 0 {
			finish[sec.ID] = prepareSection(sec, includeContinue, selected)
		}
	}
	parentFinish := map[int]finishSection{}
	for _, sec := range explicitPinnedSecs {
		if neededParents[sec.ID] && finish[sec.ID] == nil {
			parentFinish[sec.ID] = prepareSection(sec, false, nil)
		}
	}
	for _, sec := range allowedSecs {
		if selected == nil && parentOf[sec.ID] != 0 {
			finish[sec.ID] = prepareSection(sec, includeContinue, selected)
		}
	}
	painted := s.paintHubPages(ctx, pr, pages)

	served := map[string]frontend.Hub{}
	secBuilt := make(map[int][]model.Hub, len(allowedSecs))
	parentIdx := map[int]parentHubIndex{}
	provisional := false

	// Build non-subset sections first so subset sections can compare against their
	// parent library's rows regardless of ordering in contentDirectoryID.
	for _, sec := range allowedSecs {
		if selected == nil && parentOf[sec.ID] != 0 {
			continue
		}
		rows, secServed, secProv := finish[sec.ID](painted)
		secBuilt[sec.ID] = rows
		for id, h := range secServed {
			served[id] = h
		}
		if secProv {
			provisional = true
		}
		if neededParents[sec.ID] {
			parentIdx[sec.ID] = indexParentHubRows(rows, sec.ID, parentRefCap)
		}
	}

	// For per-directory requests naming a subset library whose parent is pinned,
	// build the parent's reference rows from the memoised recommender.
	if selected == nil {
		for pID := range neededParents {
			if _, ok := parentIdx[pID]; ok {
				continue
			}
			for _, pSec := range explicitPinnedSecs {
				if pSec.ID == pID {
					pRows, _, _ := parentFinish[pID](painted)
					parentIdx[pID] = indexParentHubRows(pRows, pID, parentRefCap)
					break
				}
			}
		}
	}

	// Build subset sections and suppress rows that do not differ meaningfully from
	// their parent library's Home rows.
	for _, sec := range allowedSecs {
		if selected != nil || parentOf[sec.ID] == 0 {
			continue
		}
		rows, secServed, secProv := finish[sec.ID](painted)
		if secProv {
			provisional = true
		}
		if idx, ok := parentIdx[parentOf[sec.ID]]; ok {
			filtered := make([]model.Hub, 0, len(rows))
			for _, mh := range rows {
				if isCrossLibraryHub(mh.HubIdentifier) {
					filtered = append(filtered, mh)
					continue
				}
				stem := rowHubID(mh.HubIdentifier)
				if frontend.SubsetRowDistinct(hubItemKeys(mh), idx.byRow[stem], idx.all) {
					filtered = append(filtered, mh)
				}
			}
			rows = filtered
		}
		secBuilt[sec.ID] = rows
		for _, mh := range rows {
			if src, ok := secServed[mh.HubIdentifier]; ok {
				served[mh.HubIdentifier] = src
			}
		}
	}

	var playlistRows []model.Hub
	if wantPlaylists {
		if plHub, _, ok := s.playlistsHubProjection(ctx, pr, count); ok && (selected == nil || selected[plHub.HubIdentifier]) {
			hFact, nFact := homeLayoutFacts(plHub)
			arranged := s.arrangeHomeRows(ctx, pr, s.hubContext(pr, 0), "home", []model.Hub{plHub}, []frontend.Hub{hFact}, []frontend.NeutralHomeRow{nFact})
			for _, mh := range arranged {
				if strings.EqualFold(mh.Style, "hero") {
					mh.Style = "shelf"
				}
				playlistRows = append(playlistRows, mh)
			}
			playlistRows = nonEmptyUniqueHubs(playlistRows)
		}
	}

	// Keep the combined Home screen within the effective Home row budget while
	// allocating rows fairly across pinned libraries.
	if selected == nil {
		if len(allowedSecs) > 1 {
			crossCount := len(playlistRows)
			libLengths := make([]int, len(allowedSecs))
			for i, sec := range allowedSecs {
				for _, mh := range secBuilt[sec.ID] {
					if isCrossLibraryHub(mh.HubIdentifier) {
						crossCount++
					} else {
						libLengths[i]++
					}
				}
			}
			caps := frontend.AllocateRowBudget(libLengths, max(homeBudget-crossCount, 0))
			for i, sec := range allowedSecs {
				secBuilt[sec.ID] = trimSectionLibraryRows(secBuilt[sec.ID], caps[i])
			}
		} else if len(allowedSecs) == 1 && len(explicitPinnedSecs) > 1 {
			perDirCap := max(homeBudget/len(explicitPinnedSecs), 1)
			sec := allowedSecs[0]
			secBuilt[sec.ID] = trimSectionLibraryRows(secBuilt[sec.ID], perDirCap)
		}
	}

	var allRows []model.Hub
	for i, sec := range allowedSecs {
		qualify := (len(allowedSecs) > 1 && i > 0) ||
			(len(allowedSecs) == 1 && len(explicitPinnedSecs) > 1 && sec.ID != explicitPinnedSecs[0].ID)
		for _, mh := range secBuilt[sec.ID] {
			if qualify && !isCrossLibraryHub(mh.HubIdentifier) {
				mh.Title = frontend.QualifyLibraryRowTitle(mh.Title, sec.Title)
			}
			allRows = append(allRows, mh)
		}
	}
	allRows = append(allRows, playlistRows...)

	mc := hubContainer()
	mc.Hub = stableHomeHubs(pr, allRows)
	if shelves {
		secID := allowedSecs[0].ID
		var renamed map[string]shelfRef
		mc.Hub, renamed = collectionShelves(mc.Hub, secID)
		s.rememberShelves(renamed)
		for id, ref := range renamed {
			if h, ok := served[ref.hub]; ok {
				served[id] = h
			}
		}
	}
	if pr.HasPage {
		total := len(mc.Hub)
		lo, hi := pr.window(total)
		mc.TotalSize = model.Ptr(total)
		mc.Offset = model.Ptr(lo)
		mc.Hub = mc.Hub[lo:hi]
	}
	mc.Size = len(mc.Hub)
	buildSec := 0
	if len(allowedSecs) == 1 && !wantPlaylists {
		buildSec = allowedSecs[0].ID
	}
	return homeBuild{mc: mc, served: served, section: buildSec, provisional: provisional}
}
