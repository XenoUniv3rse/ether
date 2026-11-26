package personalize

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/braedonsaunders/ether/internal/core"
)

// The final, shared composition pass for every app: the page's rules
// applied to the rows the engine and the app built. On a Personalized page,
// pinned and shown rows are never dropped by the page length or slot
// conflicts; Auto rows and Ether's picks stay within the length and their
// position bounds. A Curated page is exactly its rows, in its order.

type arrangeCandidate struct {
	r              core.HomeRow
	p              *rule
	rank           float64
	explicitAlways bool
}

// arrangePage arranges rows under a page plan. def is the length when the
// owner set none.
func arrangePage(rows []core.HomeRow, pl *pagePlan, user int64, hc core.HubContext, def int) []core.HomeRow {
	if pl != nil && pl.curated {
		return arrangeCurated(rows, pl, user, hc)
	}
	maxRows := pl.maxRows(hc.Device, def)
	if maxRows <= 0 {
		maxRows = 14
	}
	var activeRows []core.HomeRow
	for _, row := range rows {
		p := pl.match(row)
		if p == nil && pl.suppressed(row) {
			continue
		}
		if code, _ := p.hiddenReason(user, hc); code != "" {
			continue
		}
		activeRows = append(activeRows, row)
	}
	// Rows no page rule covers get the engine's defaults: Continue Watching
	// first, then top picks, new releases and trending discovery.
	_, defaults := leadDefaults(activeRows, pl, maxRows)
	var pool []arrangeCandidate
	hidden := []core.HomeRow{}
	counts := map[string]int{}
	for _, row := range rows {
		row = resetPlacement(row)
		p := pl.match(row)
		if p == nil && pl.suppressed(row) {
			continue
		}
		if p == nil && pl != nil && !pl.picks && !isAppAnchor(row) {
			row.Hidden, row.HiddenCode, row.HiddenReason = true, "picks_off", "This page is Curated"
			hidden = append(hidden, row)
			continue
		}
		applyRule(&row, p)
		if code, why := p.hiddenReason(user, hc); why != "" {
			row.Hidden, row.HiddenCode, row.HiddenReason = true, code, why
			hidden = append(hidden, row)
			continue
		}
		explicitAlways := p != nil && p.Status == "always"
		cp := p
		if cp == nil {
			if cp = defaults[row.HubID]; cp != nil {
				// Worded from its real neighbour once placed (etherPlacedWhy).
				row.Why = whyFitLeads
			}
		}
		pool = append(pool, arrangeCandidate{row, cp, 0, explicitAlways})
	}
	// Explanation-only hidden rows are absent from the apps' candidate list.
	// Compute ranking from the eligible pool so their presence in a preview
	// cannot change the relative effect of owner priority weights.
	for i := range pool {
		c := &pool[i]
		c.rank = float64(len(pool) - i)
		if c.p != nil {
			if c.p.Priority > 0 {
				c.rank *= c.p.Priority
			}
			if c.p.Status == "always" {
				c.rank += 10000
			}
			if c.p.Slot > 0 && c.p.Layer == core.RowLayerViewer {
				c.rank += 100000
			}
		}
	}
	sort.SliceStable(pool, func(i, j int) bool { return pool[i].rank > pool[j].rank })
	out := []core.HomeRow{}
	var placed []*rule
	used := make([]bool, len(pool))
	needsExplicitAlways := func(c arrangeCandidate, tally map[string]int) bool {
		if !c.explicitAlways || c.p == nil {
			return false
		}
		if c.p.MaxPerPage > 0 && tally[c.p.Row] >= c.p.MaxPerPage {
			return false
		}
		if c.p.Slot > 0 && tally[c.p.Row] >= 1 {
			return false
		}
		return true
	}
	remainingAlways := func() int {
		sim := make(map[string]int, len(counts))
		for k, n := range counts {
			sim[k] = n
		}
		rem := 0
		for i, c := range pool {
			if used[i] || !needsExplicitAlways(c, sim) {
				continue
			}
			sim[c.p.Row]++
			rem++
		}
		return rem
	}
	for slot := 1; slot <= maxRows || remainingAlways() > 0; slot++ {
		remAlways := remainingAlways()
		allowOptional := slot <= maxRows && len(out)+remAlways < maxRows
		pick := -1
		displacedPick := false
		deadline := 101
		for i, c := range pool {
			if used[i] {
				continue
			}
			must := needsExplicitAlways(c, counts)
			if !must && !allowOptional {
				continue
			}
			p := c.p
			if p != nil {
				if p.MaxPerPage > 0 && counts[p.Row] >= p.MaxPerPage {
					continue
				}
				if p.Slot > 0 {
					if !must && p.Slot != slot {
						continue
					}
					if must && p.Slot > slot {
						continue
					}
				}
				if p.MinSlot > slot {
					continue
				}
				if p.MaxSlot > 0 && p.MaxSlot < slot && !must {
					continue
				}
				if p.Slot == slot {
					pick = i
					break
				}
				if must && p.Slot > 0 && p.Slot < slot {
					if !displacedPick {
						pick = i
						displacedPick = true
					}
					continue
				}
				if !displacedPick && p.MaxSlot > 0 && p.MaxSlot < deadline {
					pick = i
					deadline = p.MaxSlot
					continue
				}
			}
			if pick < 0 {
				pick = i
			}
		}
		if pick < 0 {
			if remAlways == 0 {
				break
			}
			bestTarget := 1000
			for i, c := range pool {
				if used[i] || !needsExplicitAlways(c, counts) {
					continue
				}
				target := max(c.p.Slot, c.p.MinSlot)
				if target == 0 {
					target = slot
				}
				if pick < 0 || target < bestTarget {
					pick = i
					bestTarget = target
				}
			}
			if pick < 0 {
				break
			}
		}
		used[pick] = true
		c := pool[pick]
		c.r.Slot = slot
		if c.p != nil {
			counts[c.p.Row]++
		}
		out = append(out, c.r)
		placed = append(placed, c.p)
	}
	orderFanOuts(out, placed)
	for i, p := range placed {
		if p != nil && p.Mode == core.PlacementPinned && p.Slot > 0 && p.Slot != i+1 {
			out[i].Why = pinMovedWhy(p, i+1, out)
		}
	}
	etherPlacedWhy(out)
	for i, c := range pool {
		if !used[i] {
			c.r.Hidden = true
			c.r.HiddenCode = "page_full"
			c.r.HiddenReason = "The page is full: it shows up to its length, pinned and shown rows first"
			hidden = append(hidden, c.r)
		}
	}
	return append(out, hidden...)
}

// arrangeCurated is a Curated page: the page's rows in its order (a
// one-row-per-library row's libraries in their order, a row with several
// seeds in the engine's order), nothing else. Rows with no titles are
// skipped; there is no length.
func arrangeCurated(rows []core.HomeRow, pl *pagePlan, user int64, hc core.HubContext) []core.HomeRow {
	type placed struct {
		row        core.HomeRow
		order, sub int
	}
	var keep []placed
	hidden := []core.HomeRow{}
	count := map[string]int{}
	for i, row := range rows {
		row = resetPlacement(row)
		p := pl.match(row)
		if p == nil {
			// An app's own anchor row (Continue Watching) the page doesn't
			// list stays off the page like any other unlisted row.
			row.Hidden, row.HiddenCode, row.HiddenReason = true, "not_on_page", "Not on this page"
			if !pl.suppressed(row) {
				hidden = append(hidden, row)
			}
			continue
		}
		applyRule(&row, p)
		if code, why := p.hiddenReason(user, hc); why != "" {
			row.Hidden, row.HiddenCode, row.HiddenReason = true, code, why
			hidden = append(hidden, row)
			continue
		}
		if p.MaxPerPage > 0 && count[p.RowID] >= p.MaxPerPage {
			continue
		}
		if !rowHasItems(row) && row.Kind != KindContinue {
			row.Hidden, row.HiddenCode, row.HiddenReason = true, "empty", "No titles right now"
			hidden = append(hidden, row)
			continue
		}
		count[p.RowID]++
		if p.Mode != core.PlacementPinned || p.Slot <= 0 {
			row.Why = "In your order"
		}
		keep = append(keep, placed{row: row, order: p.Order, sub: pl.libraryIndex(p.row, p.Library)*1000 + i})
	}
	sort.SliceStable(keep, func(i, j int) bool {
		if keep[i].order != keep[j].order {
			return keep[i].order < keep[j].order
		}
		return keep[i].sub < keep[j].sub
	})
	out := make([]core.HomeRow, 0, len(keep)+len(hidden))
	for _, k := range keep {
		out = append(out, k.row)
	}
	out = pinToPositions(out, pl)
	for i := range out {
		out[i].Slot = i + 1
	}
	return append(out, hidden...)
}

// pinToPositions moves the rows pinned to a position onto it, counting every
// row shown (plugin surface rows too), lowest position first; the other
// rows keep their order around them.
func pinToPositions(rows []core.HomeRow, pl *pagePlan) []core.HomeRow {
	type pin struct {
		row core.HomeRow
		at  int
	}
	var pins []pin
	rest := make([]core.HomeRow, 0, len(rows))
	for _, r := range rows {
		at := 0
		if r.Present.Surface != nil {
			if p := pl.byRow[r.RowID]; p != nil && p.Mode == core.PlacementPinned {
				at = p.Slot
			}
		} else if p := pl.match(r); p != nil && p.Mode == core.PlacementPinned && !r.Hidden {
			at = p.Slot
		}
		if at > 0 {
			pins = append(pins, pin{r, at})
			continue
		}
		rest = append(rest, r)
	}
	if len(pins) == 0 {
		return rows
	}
	sort.SliceStable(pins, func(i, j int) bool { return pins[i].at < pins[j].at })
	for _, p := range pins {
		at := min(p.at-1, len(rest))
		rest = append(rest[:at], append([]core.HomeRow{p.row}, rest[at:]...)...)
	}
	return rest
}

// whyFitLeads and whyFitAfter word where a row the page leaves to the
// viewer's taste landed, from the row really above it.
const (
	whyFitLeads = "First: the best fit for what this viewer plays"
	whyFitAfter = ": the next best fit for what this viewer plays"
)

// etherPlacedWhy rewords the "Why here" of rows placed by fit from their
// real neighbour, once the order is final.
func etherPlacedWhy(rows []core.HomeRow) {
	prev := ""
	for i := range rows {
		if rows[i].Hidden {
			continue
		}
		if rows[i].Why == whyFitLeads || strings.HasSuffix(rows[i].Why, whyFitAfter) {
			if prev == "" {
				rows[i].Why = whyFitLeads
			} else {
				rows[i].Why = "After " + prev + whyFitAfter
			}
		}
		prev = rows[i].Title
	}
}

// orderFanOuts keeps the rows of a one-row-per-library row in the order
// of its libraries, within the places the arrangement gave them.
func orderFanOuts(out []core.HomeRow, placed []*rule) {
	groups := map[string][]int{}
	for i, p := range placed {
		if p != nil && p.row != nil && p.row.Scope.PerLibrary && out[i].Library != 0 {
			groups[p.RowID] = append(groups[p.RowID], i)
		}
	}
	for _, idx := range groups {
		if len(idx) < 2 {
			continue
		}
		rows := make([]core.HomeRow, len(idx))
		rules := make([]*rule, len(idx))
		for j, i := range idx {
			rows[j], rules[j] = out[i], placed[i]
		}
		order := make([]int, len(idx))
		for j := range order {
			order[j] = j
		}
		row := rules[0].row
		sort.SliceStable(order, func(a, b int) bool {
			return fanOutIndex(row, rows[order[a]].Library) < fanOutIndex(row, rows[order[b]].Library)
		})
		for j, i := range idx {
			out[i], placed[i] = rows[order[j]], rules[order[j]]
			out[i].Slot = i + 1
		}
	}
}

// fanOutIndex is a library's place in a one-row-per-library row: its
// chosen order, or library order when the row draws from any library.
func fanOutIndex(row *core.Row, lib core.LibraryID) int {
	if len(row.Scope.Libraries) > 0 {
		if i := slices.Index(row.Scope.Libraries, lib); i >= 0 {
			return i
		}
		return len(row.Scope.Libraries) + int(lib)
	}
	return int(lib)
}

// pinMovedWhy explains a pinned row that isn't at its position: which row
// holds the place and why, or that the page has fewer rows above it.
func pinMovedWhy(p *rule, at int, rows []core.HomeRow) string {
	base := p.why(p.Slot)
	if p.Slot >= 1 && p.Slot <= len(rows) && p.Slot != at {
		taker := rows[p.Slot-1]
		switch {
		case taker.Pinned && taker.Layer == core.RowLayerViewer && p.Layer != core.RowLayerViewer:
			return fmt.Sprintf("%s; %s is pinned there just for this viewer, so it's at #%d", base, taker.Title, at)
		case taker.Pinned:
			return fmt.Sprintf("%s; %s is pinned there too, so it's at #%d", base, taker.Title, at)
		default:
			return fmt.Sprintf("%s; %s holds #%d, so it's at #%d", base, taker.Title, p.Slot, at)
		}
	}
	if at <= 1 {
		return fmt.Sprintf("%s; nothing else shows above it, so it's at #1", base)
	}
	return fmt.Sprintf("%s; only %d rows show above it, so it's at #%d", base, at-1, at)
}

// isAppAnchor reports a row an app builds and places itself (Continue
// Watching); with picks off it still shows unless the page hides it.
func isAppAnchor(row core.HomeRow) bool { return row.Kind == KindContinue }

func resetPlacement(row core.HomeRow) core.HomeRow {
	row.Hidden, row.HiddenCode, row.HiddenReason = false, "", ""
	row.Slot, row.Pinned = 0, false
	row.Layer, row.Mode, row.RowID, row.Why = core.RowLayerEther, "", "", "Fits what this viewer plays"
	return row
}

// applyRule stamps a page rule's look and placement onto a row.
func applyRule(row *core.HomeRow, p *rule) {
	if row.Kind == KindTop10 || row.Kind == KindTop10Today {
		row.Hub.RankedTop10 = true
	}
	row.Personal = personalBadge(row.Kind) || row.Present.Personal
	if p == nil {
		return
	}
	row.RowID, row.Library, row.Mode, row.Layer = p.RowID, p.Library, p.Mode, p.Layer
	row.Look = p.Look
	row.Why = p.why(p.Slot)
	if p.Slot > 0 && p.Status == "always" {
		row.Pinned = true
	}
	if p.Look.Trailer != nil {
		row.Hub.PreviewOnFocus = *p.Look.Trailer
	}
	if p.Look.Numbered != nil {
		row.Hub.RankedTop10 = *p.Look.Numbered
	}
	if p.Name != "" && p.Derived {
		row.Title = p.Name
		row.Hub.Title = p.Name
	}
}

// SetExtraHomeRows supplies the additional production rows for owner previews.
// Wire once during startup, before serving requests.
func (e *Engine) SetExtraHomeRows(fn func(context.Context, *core.User, core.HubContext, string, int) []core.Hub) {
	e.extraHomeRows = fn
}

func hubKind(id string) string {
	switch {
	case strings.Contains(id, "continue"), strings.Contains(id, "inprogress"):
		return KindContinue
	case strings.Contains(id, "recentlyadded"):
		return KindRecentlyAdded
	case strings.Contains(id, "playlist"):
		return KindPlaylists
	}
	// An engine or collection row from before a restart ("movie.cluster.…", "collection.…").
	if k := parseHubKind(id); k != "other" && (strings.HasPrefix(id, "home.") || strings.HasPrefix(id, "movie.") || strings.HasPrefix(id, "tv.") || strings.HasPrefix(id, "collection.")) {
		return k
	}
	return id
}

func rowsFromHubs(hubs []core.Hub) []core.HomeRow {
	rows := make([]core.HomeRow, 0, len(hubs))
	for _, h := range hubs {
		rows = append(rows, core.HomeRow{HubID: h.ID, Title: h.Title, Kind: hubKind(h.ID), Source: core.RowSourcePlex, Items: h.Items, Hub: h})
	}
	return rows
}

// FinalizeHome arranges the complete Plex row list after all producers ran.
// Unknown/custom hubs with no catalogue items are deliberately preserved.
func (e *Engine) FinalizeHome(ctx context.Context, user *core.User, hc core.HubContext, section string, hubs []core.Hub) ([]core.Hub, error) {
	ctx = core.WithCatalogViewer(ctx, user)
	rows := rowsFromHubs(hubs)
	rows = e.finalizeHomeRows(ctx, user, hc, section, rows)
	out := make([]core.Hub, 0, len(rows))
	for _, row := range rows {
		out = append(out, row.Hub)
	}
	return out, nil
}

// finalizeHomeRows is shared by both wire-shaped and neutral adapters.
func (e *Engine) finalizeHomeRows(ctx context.Context, user *core.User, hc core.HubContext, section string, rows []core.HomeRow) []core.HomeRow {
	return e.finalizeHomeRowsWithDefault(ctx, user, hc, section, rows, 0)
}

// pageSection resolves the section an app asks for: "" or "home", a
// library ("library:<id>", its first rows tab), a rows tab
// ("library:<id>/<tab>"), or a section the app knows only by id.
func (e *Engine) pageSection(section string, sectionID int) string {
	if section == "" || section == surfHome {
		if sectionID > 0 {
			return surfHome
		}
		return surfHome
	}
	if core.PageKey(section).Valid() {
		return section
	}
	if _, ok := parseLibrarySection(section); ok {
		return section
	}
	if sectionID > 0 {
		return librarySectionName(sectionID)
	}
	return surfHome
}

func (e *Engine) finalizeHomeRowsWithDefault(ctx context.Context, user *core.User, hc core.HubContext, section string, rows []core.HomeRow, defaultMaxRows int) []core.HomeRow {
	ctx = core.WithCatalogViewer(ctx, user)
	if user == nil {
		user = &core.User{}
	}
	if hc.Now.IsZero() {
		hc.Now = e.now()
	}
	hc.Now = hc.Now.In(e.loc)
	if section != "" && section != surfHome && !core.PageKey(section).Valid() && !strings.HasPrefix(section, "library:") {
		section = e.pageSection(section, hc.SectionID)
	}
	if hc.HomeScoped {
		section = surfHome
	}
	pl := e.planFor(ctx, user.ID, section)
	def := e.opts.MaxHomeRows
	if pl.key != core.PageHome {
		def = e.opts.MaxSectionRows
	}
	if defaultMaxRows > 0 {
		def = defaultMaxRows
	}
	pl = e.pluginDefaultPlan(pl, rows)
	for i := range rows {
		e.hubMu.RLock()
		kind := e.hubKinds[rows[i].HubID]
		e.hubMu.RUnlock()
		if kind != "" {
			rows[i].Kind = kind
		}
	}
	// Recently Added is the catalogue's newest additions, not picks: keep
	// only titles this viewer could plausibly want (toppicks.go), unless the
	// page shows it (pinned or Show) and so wants any available additions.
	var taste *Taste
	if m := e.modelFor(ctx, user, false); m != nil {
		taste = m.taste
	}
	kept := rows[:0]
	for _, r := range rows {
		if r.Kind == KindRecentlyAdded {
			p := pl.match(r)
			always := p != nil && p.Status == "always" && p.active(user.ID, hc)
			curated := curateFeed(r, taste)
			if curated.Hidden {
				if always {
					if len(curated.Items) > 0 {
						curated.Hidden = false
						curated.HiddenCode = ""
						curated.HiddenReason = ""
						curated.Hub.Items = curated.Items
						kept = append(kept, curated)
					} else if rowHasItems(r) {
						kept = append(kept, r)
					}
				}
				continue
			}
			r = curated
			r.Hub.Items = curated.Items
		}
		kept = append(kept, r)
	}
	rows = kept
	out := []core.HomeRow{}
	for _, row := range arrangePage(rows, pl, user.ID, hc, def) {
		if !row.Hidden {
			out = append(out, row)
		}
	}
	return out
}

// pluginDefaultPlan is a plugin library page the owner never saved: its
// declared shelves (and Continue) in declared order, Curated. A page with
// no declared shelves keeps the Personalized default over its automatic
// rows.
func (e *Engine) pluginDefaultPlan(pl *pagePlan, rows []core.HomeRow) *pagePlan {
	if pl == nil || pl.saved || pl.key == core.PageHome {
		return pl
	}
	// Only plugin libraries declare shelves, so their rows tell the page
	// apart from a catalogue library's.
	page := core.RowPage{Key: pl.key, Mode: core.PageCurated, Rows: []core.Row{}}
	shelves := false
	for _, r := range rows {
		src, param := sourceOf(r.HubID, r.Kind)
		switch src {
		case SourceShelf:
			shelves = true
			page.Rows = append(page.Rows, core.Row{ID: stableRowID(pl.key, src+param), Source: core.RowSource{Kind: SourceShelf, Params: map[string]string{"shelf": param}},
				Placement: core.RowPlacement{Mode: core.PlacementShow, Audience: core.Audience{Mode: core.AudienceEveryone}}})
		case KindContinue:
			page.Rows = append(page.Rows, core.Row{ID: stableRowID(pl.key, src), Source: core.RowSource{Kind: KindContinue},
				Placement: core.RowPlacement{Mode: core.PlacementShow, Audience: core.Audience{Mode: core.AudienceEveryone}}})
		}
	}
	if !shelves {
		return pl
	}
	return compilePage(page, false, pl.viewer, pl.libraries)
}

// stableRowID derives a row id for a default page's row, so previews and
// apps see the same id until the owner saves the page.
func stableRowID(key core.PageKey, s string) string {
	const alphabet = "abcdefghijklmnopqrstuvwxyz234567"
	var h uint64 = 1469598103934665603
	for _, c := range string(key) + "|" + s {
		h ^= uint64(c)
		h *= 1099511628211
	}
	out := []byte{'r'}
	for i := 0; i < 8; i++ {
		out = append(out, alphabet[h%32])
		h /= 32
	}
	return string(out)
}

// HomeRowBudget returns the Home page length for a viewer and device.
func (e *Engine) HomeRowBudget(ctx context.Context, user *core.User, hc core.HubContext) int {
	ctx = core.WithCatalogViewer(ctx, user)
	pl := e.planFor(ctx, userID(user), surfHome)
	maxRows := pl.maxRows(hc.Device, e.opts.MaxHomeRows)
	if maxRows <= 0 {
		maxRows = 20
	}
	return maxRows
}

// HomeRowEnabled checks whether Home shows an app-built row (Continue
// Watching) for the viewer now, without slot or length constraints.
func (e *Engine) HomeRowEnabled(ctx context.Context, user *core.User, hc core.HubContext, id string) bool {
	ctx = core.WithCatalogViewer(ctx, user)
	if user == nil {
		user = &core.User{}
	}
	if hc.Now.IsZero() {
		hc.Now = e.now()
	}
	hc.Now = hc.Now.In(e.loc)
	pl := e.planFor(ctx, user.ID, surfHome)
	row := core.HomeRow{HubID: id, Kind: hubKind(id)}
	p := pl.match(row)
	if p == nil {
		return !pl.curated || isAppAnchor(row) && !pl.listed[KindContinue]
	}
	return p.active(user.ID, hc)
}

func (e *Engine) FinalizeNeutralHome(ctx context.Context, user *core.User, situation core.HomeSituation, section string, candidates []core.NeutralHomeRow) ([]core.NeutralHomeRow, error) {
	return e.finalizeNeutralHome(ctx, user, situation, section, candidates, false)
}

// FinalizeNeutralHomeFill applies the page to a minimal hero fill. Shelf
// sparsity and taste curation must not discard the one or two missing slots.
func (e *Engine) FinalizeNeutralHomeFill(ctx context.Context, user *core.User, situation core.HomeSituation, section string, candidates []core.NeutralHomeRow) ([]core.NeutralHomeRow, error) {
	return e.finalizeNeutralHome(ctx, user, situation, section, candidates, true)
}

func (e *Engine) finalizeNeutralHome(ctx context.Context, user *core.User, situation core.HomeSituation, section string, candidates []core.NeutralHomeRow, fill bool) ([]core.NeutralHomeRow, error) {
	ctx = core.WithCatalogViewer(ctx, user)
	hc := core.HubContext{Device: situation.Device, Daypart: situation.Daypart, Weekend: situation.Weekend, Now: situation.Now, SectionID: situation.LibraryID, Product: situation.Product, Platform: situation.Platform, SessionID: situation.SessionID, ClientID: situation.ClientID}
	rows := make([]core.HomeRow, 0, len(candidates))
	byID := make(map[string]core.NeutralHomeRow, len(candidates))
	for _, candidate := range candidates {
		kind := candidate.Kind
		if kind == "" {
			kind = hubKind(candidate.ID)
		}
		h := core.Hub{ID: candidate.ID, Title: candidate.Title, Type: candidate.Type, Items: candidate.Items, ItemCount: candidate.ItemCount, PreviewOnFocus: candidate.PreviewOnFocus, RankedTop10: candidate.RankedTop10}
		rows = append(rows, core.HomeRow{HubID: candidate.ID, Title: candidate.Title, Kind: kind, Source: core.RowSourcePrecomposed, Items: candidate.Items, Hub: h})
		byID[candidate.ID] = candidate
	}
	var arranged []core.HomeRow
	if fill {
		if user == nil {
			return nil, core.ErrUnauthorized
		}
		hc.Now = hc.Now.In(e.loc)
		// Fill is a band, so shelf-only default slots (Recently Added at
		// slot eight) do not apply. The page's own rules still do, and Ether
		// may use any candidate the page doesn't cover.
		pl := e.planFor(ctx, user.ID, section)
		fillPlan := *pl
		fillPlan.picks, fillPlan.curated = true, false
		// Every candidate the page doesn't cover is an Auto row of the band,
		// so the default lead slots (Recently Added from #8) never apply.
		fillPlan.bySrc = make(map[string]*rule, len(pl.bySrc)+len(rows))
		for k, v := range pl.bySrc {
			fillPlan.bySrc[k] = v
		}
		for _, row := range rows {
			if fillPlan.match(row) == nil && !fillPlan.suppressed(row) {
				src, param := sourceOf(row.HubID, row.Kind)
				fillPlan.bySrc[sourceKey(src, param)] = &rule{Row: row.HubID, Source: src, Mode: core.PlacementAuto, Status: "eligible", Layer: core.RowLayerEther}
			}
		}
		for _, row := range arrangePage(rows, &fillPlan, user.ID, hc, e.opts.MaxHomeRows) {
			if !row.Hidden {
				arranged = append(arranged, row)
			}
		}
	} else {
		arranged = e.finalizeHomeRowsWithDefault(ctx, user, hc, section, rows, situation.DefaultMaxRows)
	}
	libs := map[core.LibraryID]core.PageLibrary{}
	for _, l := range e.pageLibraries(ctx) {
		libs[l.ID] = l
	}
	if !fill && user != nil {
		arranged = e.withSurfaceRows(ctx, arranged, e.planFor(ctx, user.ID, section), user.ID, hc, byID)
	}
	out := make([]core.NeutralHomeRow, 0, len(arranged))
	for _, row := range arranged {
		candidate, ok := byID[row.HubID]
		if !ok {
			if row.Kind != SourceSurface {
				continue
			}
			candidate = core.NeutralHomeRow{ID: row.HubID, Title: row.Title, Kind: SourceSurface}
		}
		candidate.PreviewOnFocus = row.Hub.PreviewOnFocus
		candidate.PreviewOnFocusPolicy = row.Look.Trailer
		candidate.RankedTop10 = row.Hub.RankedTop10
		candidate.Always = row.Mode == core.PlacementPinned || row.Mode == core.PlacementShow
		candidate.Style = row.Look.Style
		candidate.HideWatched = row.Look.HideWatched
		candidate.Title = row.Title
		own := candidate.Present
		candidate.Present = presentationFor(row, libs, situation.LibraryID)
		// The row's own default look (a plugin shelf's declared layout)
		// applies when the page leaves the look to the source.
		if candidate.Present.Layout == "" {
			candidate.Present.Layout = own.Layout
		}
		// A shelf that is the viewer's own (Jump back in, Liked, Recently
		// played) is personal whatever the page says.
		candidate.Present.Personal = candidate.Present.Personal || own.Personal || candidate.Purpose == core.PurposeRecentContexts || candidate.Purpose == core.PurposeLiked
		out = append(out, candidate)
	}
	return out, nil
}

// withSurfaceRows inserts the page's plugin surface rows (stubs Vela draws
// from the plugin's document) at their page positions. They are not
// counted toward the page length and nothing dedupes or filters them; the
// apps drop the ones a viewer or app may not see.
func (e *Engine) withSurfaceRows(ctx context.Context, rows []core.HomeRow, pl *pagePlan, user int64, hc core.HubContext, existing map[string]core.NeutralHomeRow) []core.HomeRow {
	if pl == nil || e.opts.Surfaces == nil || !pl.listed[SourceSurface] {
		return rows
	}
	byKey := map[string]core.SurfaceInfo{}
	for _, s := range e.opts.Surfaces(ctx) {
		byKey[s.Plugin+":"+s.ID] = s
	}
	for _, r := range pl.rules {
		if r.Source != SourceSurface || r.row == nil || !r.active(user, hc) {
			continue
		}
		key := r.row.Source.Param("surface", "")
		info, ok := byKey[key]
		if !ok {
			continue
		}
		id := "surface:" + key
		if _, dup := existing[id]; dup {
			continue
		}
		title := r.Name
		if title == "" {
			title = info.Title
		}
		row := core.HomeRow{HubID: id, RowID: r.RowID, Kind: SourceSurface, Title: title, Source: core.RowSourceOwner,
			Layer: r.Layer, Mode: r.Mode, Look: r.Look, Why: r.why(r.Slot), Hub: core.Hub{ID: id, Title: title},
			Present: core.RowPresentation{RowID: r.RowID, Surface: &core.RowSurface{Plugin: info.Plugin, PluginName: info.PluginName, Surface: info.WireID}}}
		var at int
		switch {
		case pl.curated:
			at = 0
			for i, x := range rows {
				if p := pl.match(x); p != nil && p.Order < r.Order {
					at = i + 1
				}
			}
		case r.Mode == core.PlacementPinned:
			at = min(max(r.Slot-1, 0), len(rows))
		default:
			at = min(1, len(rows))
		}
		rows = append(rows[:at], append([]core.HomeRow{row}, rows[at:]...)...)
	}
	// Pinned positions count the surface rows too.
	return pinToPositions(rows, pl)
}

// presentationFor is how apps should draw an arranged row.
func presentationFor(row core.HomeRow, libs map[core.LibraryID]core.PageLibrary, pageLibrary int) core.RowPresentation {
	if row.Present.Surface != nil {
		return row.Present
	}
	p := core.RowPresentation{RowID: row.RowID, Layout: row.Look.Style, Card: row.Look.Card, Personal: row.Personal,
		Library: row.Library, Numbered: row.Hub.RankedTop10, Trailer: row.Hub.PreviewOnFocus, HideWatched: row.Look.HideWatched}
	lib := row.Library
	if lib == 0 {
		lib = core.LibraryID(pageLibrary)
	}
	if l, ok := libs[lib]; ok {
		p.Experience = l.Experience
		if p.Card == "" {
			p.Card = l.Card
		}
	}
	return p
}

var _ core.NeutralHomeLayoutSource = (*Engine)(nil)

func (e *Engine) HomeCandidatesFor(ctx context.Context, u *core.User, hc core.HubContext, limit int) ([]core.Hub, error) {
	return toHubs(e.home(ctx, u, hc, "", limit, false).Rows), nil
}

// HomeCandidatesWithPolicy applies per-request visibility before ranking.
func (e *Engine) HomeCandidatesWithPolicy(ctx context.Context, u *core.User, hc core.HubContext, limit int, allow func(context.Context, *core.User, *core.Title) bool) ([]core.Hub, error) {
	if allow == nil {
		return nil, errors.New("personalize: missing visibility policy")
	}
	p := e.homeWithPolicy(ctx, u, hc, "", limit, false, allow)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return toHubs(p.Rows), nil
}

// SectionCandidatesWithPolicy applies viewer visibility before section ranking.
func (e *Engine) SectionCandidatesWithPolicy(ctx context.Context, u *core.User, section core.MediaType, hc core.HubContext, limit int, allow func(context.Context, *core.User, *core.Title) bool) ([]core.Hub, error) {
	if allow == nil {
		return nil, errors.New("personalize: missing visibility policy")
	}
	p := e.homeWithPolicy(ctx, u, hc, section, limit, false, allow)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return toHubs(p.Rows), nil
}

var _ core.PolicyHomeCandidateSource = (*Engine)(nil)

func (e *Engine) SectionCandidatesFor(ctx context.Context, u *core.User, t core.MediaType, hc core.HubContext, limit int) ([]core.Hub, error) {
	return toHubs(e.home(ctx, u, hc, t, limit, false).Rows), nil
}

func (e *Engine) LayoutDefaults() map[string]int {
	return map[string]int{"home": e.opts.MaxHomeRows, "library": e.opts.MaxSectionRows}
}

// HubItemsFor resolves a specific hub by identifier from the viewer's warm
// model, the generic model, or the page's owner rows when the active page no
// longer includes that row (e.g. a cold-start showcase row after the viewer's
// personal model finishes building, or a shelf dropped under a wider count).
func (e *Engine) HubItemsFor(ctx context.Context, u *core.User, hc core.HubContext, id string, limit int) (core.Hub, bool) {
	if limit <= 0 {
		limit = 12
	}
	if hc.Now.IsZero() {
		hc.Now = e.now().In(e.loc)
	}
	user := core.User{}
	if u != nil {
		user = *u
	}
	var section core.MediaType
	switch {
	case strings.HasPrefix(id, "movie."):
		section = core.TypeMovie
	case strings.HasPrefix(id, "tv."):
		section = core.TypeShow
	}
	if section != "" && hc.SectionID == 0 {
		hc.SectionID = e.firstSectionID(ctx, section)
	}
	pl := e.planFor(ctx, user.ID, e.sectionFor(section, hc))
	owner, _ := e.ownerRows(ctx, &user, hc, section, pl, false)
	e.mu.Lock()
	m := e.models[user.ID]
	gm := e.models[genericID]
	e.mu.Unlock()
	models := []*userModel{}
	if m != nil {
		models = append(models, m)
	}
	if gm != nil && gm != m {
		models = append(models, gm)
	}
	if len(models) == 0 {
		if fm := e.modelFor(ctx, &user, false); fm != nil {
			models = append(models, fm)
		}
	}
	day := dayOf(hc.Now, e.loc)
	intent, _ := e.intentFor(user.ID, hc.Now)
	tune := resolveTuning(pl)
	allowed := e.allowedFor(&user)
	for _, mod := range models {
		a := &assembly{visit: int64(day), plan: pl, model: mod, user: user, hc: hc, section: section, limit: limit,
			owner:   append(append([]*candRow{}, owner...), e.pageRows(ctx, pl, mod, owner, &user, section)...),
			allowed: allowed, day: day, intent: intent, tune: &tune,
			metadataRows: func(kind string) bool { return e.metadataRowAvailable(kind, section) }}
		cands := append(append([]*candRow{}, mod.rows...), a.owner...)
		for _, c := range cands {
			if a.hubID(c) != id {
				continue
			}
			items := a.contextItems(c)
			if len(items) == 0 {
				continue
			}
			if section != "" && hc.SectionID > 0 && e.opts.LibraryMembers != nil {
				keys := make([]core.RatingKey, len(items))
				for i, it := range items {
					keys[i] = it.t.Key
				}
				members, _, scoped, err := e.opts.LibraryMembers(ctx, core.LibraryID(hc.SectionID), keys)
				if err != nil {
					return core.Hub{}, false
				}
				if scoped {
					filtered := items[:0]
					for _, it := range items {
						if members[it.t.Key] {
							filtered = append(filtered, it)
						}
					}
					items = filtered
					if len(items) == 0 {
						continue
					}
				}
			}
			if e.opts.FilterAvailable != nil {
				pool := make([]core.Title, len(items))
				for i, it := range items {
					pool[i] = it.t
				}
				avail := map[core.RatingKey]bool{}
				for _, t := range e.opts.FilterAvailable(ctx, pool) {
					avail[t.Key] = true
				}
				filtered := items[:0]
				for _, it := range items {
					if avail[it.t.Key] {
						filtered = append(filtered, it)
					}
				}
				items = filtered
				if len(items) == 0 {
					continue
				}
			}
			limitN := a.rowCap(c)
			more := len(items) > limitN
			if more {
				items = items[:limitN]
			}
			hr := a.homeRow(&rankedRow{c: c, items: items, hubID: id}, items, more)
			return hr.Hub, true
		}
	}
	return core.Hub{}, false
}

// WatchlistRows returns the Home definitions that can bind this viewer's watchlist.
func (e *Engine) WatchlistRows(ctx context.Context, viewer *core.User) []core.Row {
	pl := e.planFor(core.WithCatalogViewer(ctx, viewer), userID(viewer), surfHome)
	var rows []core.Row
	for _, rule := range pl.rules {
		if rule.Source == KindWatchlist && rule.Gate != "audience" && rule.row != nil {
			rows = append(rows, *rule.row)
		}
	}
	return rows
}
