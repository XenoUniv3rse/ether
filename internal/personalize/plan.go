package personalize

import (
	"context"
	"fmt"
	"hash/fnv"
	"slices"
	"strings"

	"github.com/braedonsaunders/ether/internal/core"
)

// A pagePlan is one Recommendations page (core.RowPage) resolved for one
// viewer: the viewer's variant applied, the audience gate applied, every
// row compiled into the rule the arrangement and assembly consult. Rules
// bind to served rows exactly: rows built for the page by their row id
// ("<rowID>" or "<rowID>@<library>"), and rows the engine or an app builds
// by their source ("trending", "collection|12", "shelf|charts").

// rule is one page row (or one library's row of a one-row-per-library row)
// as the arrangement sees it.
type rule struct {
	// Row is the tally key: "<rowID>" or "<rowID>@<library>".
	Row     string
	RowID   string
	Library core.LibraryID
	Source  string // the source kind
	Mode    string // pinned | show | auto | hide
	// Status is always (pinned, show), eligible (auto) or off (hide).
	Status         string
	Slot           int
	MinSlot        int
	MaxSlot        int
	Priority       float64 // ranking multiplier; 0 is 1
	MaxPerPage     int
	MaxDaysPerWeek int
	When           *core.Schedule
	Devices        []string
	Look           core.RowLook
	// Layer is everyone or viewer (who set the placement).
	Layer string
	// Gate is a reason the viewer never sees the row ("audience").
	Gate string
	// Order is the row's place in the page's own order (Curated pages).
	Order int
	// audience is who the row is for (the "Why here" wording).
	audience core.Audience
	// Name is the owner's row title ("" = the source's own).
	Name string
	// Derived rows are built for the page (parameters or libraries the
	// engine's own row doesn't have); bound by row id only.
	Derived bool
	row     *core.Row
}

type pagePlan struct {
	key     core.PageKey
	page    core.RowPage // effective page (variant mode/length applied)
	saved   bool
	viewer  int64
	curated bool
	picks   bool
	rules   []*rule
	byRow   map[string]*rule // "<rowID>" and "<rowID>@<lib>"
	bySrc   map[string]*rule // plain binding: sourceKey → rule
	listed  map[string]bool  // source kinds listed for this viewer
	derived []*rule          // rules built as page rows
	// libraries are the page library's (or Home's) libraries, by id.
	libraries map[core.LibraryID]core.PageLibrary
}

// sourceKey identifies a source with the parameter that tells its rows
// apart ("collection|12", "shelf|charts", "trending").
func sourceKey(kind, param string) string {
	if param == "" {
		return kind
	}
	return kind + "|" + param
}

// planFor resolves the page for a viewer. section is an engine section
// ("home", "library:<id>", "library:<id>/<tab>").
func (e *Engine) planFor(ctx context.Context, viewer int64, section string) *pagePlan {
	key := e.pageKeyFor(ctx, section)
	if pk, ok := previewKey(ctx); ok && pk != key {
		// A preview of a library's other rows tab: same library, that tab.
		if want, _, okW := pk.Library(); okW {
			if have, _, okH := key.Library(); okH && have == want {
				key = pk
			}
		}
	}
	p, saved := e.pageForContext(ctx, key)
	return e.compilePlan(ctx, p, saved, viewer)
}

type draftPagesKey struct{}

// withDraftPage returns ctx carrying an unsaved page for previews.
func withDraftPage(ctx context.Context, p core.RowPage) context.Context {
	return context.WithValue(ctx, draftPagesKey{}, p)
}

// pageForContext returns the draft page in ctx when it is this page, else
// the saved page, else the default.
func (e *Engine) pageForContext(ctx context.Context, key core.PageKey) (core.RowPage, bool) {
	if d, ok := ctx.Value(draftPagesKey{}).(core.RowPage); ok && d.Key == key {
		return d, true
	}
	return e.Page(ctx, key)
}

// planRowKind maps a page row's source to the engine kind its served rows
// carry (for the bandit's priors and kind caps).
func planRowKind(src core.RowSource) string {
	switch src.Kind {
	case SourceCollection:
		return KindCollectionPrefix + src.Param("collection", "")
	}
	return src.Kind
}

// plainParams reports whether a row's parameters are ones the engine's own
// row already serves (so the row binds to it instead of being built).
func plainParams(src core.RowSource) bool {
	for k, v := range src.Params {
		if v == "" {
			continue
		}
		switch {
		case k == "count", k == "collection", k == "shelf":
		case k == "window" && src.Kind == KindTrending && v == "week":
		case k == "window" && src.Kind == KindAiringToday && v == "day":
		default:
			return false
		}
	}
	return true
}

func (e *Engine) compilePlan(ctx context.Context, p core.RowPage, saved bool, viewer int64) *pagePlan {
	libs := map[core.LibraryID]core.PageLibrary{}
	for _, l := range e.pageLibraries(ctx) {
		libs[l.ID] = l
	}
	return compilePage(p, saved, viewer, libs)
}

// compilePage compiles a page for a viewer (pure).
func compilePage(p core.RowPage, saved bool, viewer int64, libs map[core.LibraryID]core.PageLibrary) *pagePlan {
	pl := &pagePlan{key: p.Key, saved: saved, viewer: viewer, byRow: map[string]*rule{}, bySrc: map[string]*rule{}, listed: map[string]bool{},
		libraries: libs}
	v := p.VariantFor(viewer)
	if v != nil {
		if v.Mode != "" {
			p.Mode = v.Mode
		}
		if v.Length != nil {
			p.Length = *v.Length
		}
	}
	pl.page = p
	pl.curated = p.Mode == core.PageCurated
	pl.picks = !pl.curated && p.Picks
	for i := range p.Rows {
		row := p.Rows[i]
		look, place, layer := row.Look, row.Placement, core.RowLayerEveryone
		if v != nil {
			for _, o := range v.Rows {
				if o.Row != row.ID {
					continue
				}
				if o.Look != nil {
					look = *o.Look
				}
				if o.Placement != nil {
					place = *o.Placement
				}
				layer = core.RowLayerViewer
			}
		}
		r := &rule{Row: row.ID, RowID: row.ID, Source: row.Source.Kind, Mode: place.Mode, Slot: place.Position,
			MinSlot: place.NotAbove, MaxSlot: place.NotBelow, MaxDaysPerWeek: place.DaysPerWeek, When: place.When,
			Devices: place.Devices, Look: look, Layer: layer, Order: i, Name: row.Name, row: &p.Rows[i], audience: place.Audience}
		switch place.Mode {
		case core.PlacementPinned, core.PlacementShow:
			r.Status = "always"
		case core.PlacementHide:
			r.Status = "off"
		default:
			r.Status = "eligible"
		}
		if pl.curated && r.Status != "off" {
			// A Curated page shows its rows in its order; a pinned row
			// keeps its position.
			r.Status = "always"
			if place.Mode != core.PlacementPinned {
				r.Slot = 0
			}
		}
		switch place.Priority {
		case 1:
			r.Priority = 2
		case -1:
			r.Priority = 0.5
		}
		if n, err := fmtAtoi(row.Source.Param("count", "")); err == nil && n > 0 {
			r.MaxPerPage = n
		}
		// Keep the row addressable by ID for previews. Only rows in the
		// viewer's audience can bind or suppress an engine row by source.
		pl.rules = append(pl.rules, r)
		pl.byRow[row.ID] = r
		if !place.Audience.Includes(viewer) {
			r.Gate = "audience"
			continue
		}
		pl.listed[row.Source.Kind] = true
		_, param := sourceParam(row.Source)
		skey := sourceKey(row.Source.Kind, param)
		plain := plainParams(row.Source) && len(row.Scope.Libraries) == 0 && !row.Scope.PerLibrary
		if strings.HasPrefix(row.Source.Kind, "plugin:") {
			// A plugin list row comes back from LayoutRow under its full row
			// id, options included ("plugin:x/y?genre=Action").
			skey = core.PluginListRowID(row.Source.Kind, row.Source.Params)
			plain = len(row.Scope.Libraries) == 0 && !row.Scope.PerLibrary
		}
		if plain && pl.bySrc[skey] == nil {
			pl.bySrc[skey] = r
		} else {
			r.Derived = true
			pl.derived = append(pl.derived, r)
		}
	}
	return pl
}

// sourceParam returns the parameter that tells rows of a source apart.
func sourceParam(src core.RowSource) (string, string) {
	switch src.Kind {
	case SourceCollection:
		return "collection", src.Param("collection", "")
	case SourceShelf:
		return "shelf", src.Param("shelf", "")
	}
	return "", ""
}

func fmtAtoi(s string) (int, error) {
	var n int
	_, err := fmt.Sscanf(s, "%d", &n)
	return n, err
}

// pageRowID returns the page row id (and library) a served row was built
// for, from its hub id ("home.rabcdefgh@2" → "rabcdefgh", 2).
func pageRowID(hubID string) (string, core.LibraryID, bool) {
	stem := hubStem(hubID)
	id, lib, _ := strings.Cut(stem, "@")
	if !core.ValidRowID(id) {
		return "", 0, false
	}
	var l core.LibraryID
	if lib != "" {
		n, err := fmtAtoi(lib)
		if err != nil {
			return "", 0, false
		}
		l = core.LibraryID(n)
	}
	return id, l, true
}

// match returns the rule a served row binds to, or nil for one of Ether's
// picks. A row built for the page binds by its row id (a fan-out row gets
// its own rule, positioned after the row's first library).
func (pl *pagePlan) match(row core.HomeRow) *rule {
	if pl == nil {
		return nil
	}
	if id, lib, ok := pageRowID(row.HubID); ok {
		base := pl.byRow[id]
		if base == nil {
			return nil
		}
		if lib == 0 {
			return base
		}
		key := id + "@" + fmt.Sprint(int64(lib))
		if r := pl.byRow[key]; r != nil {
			return r
		}
		r := *base
		r.Row, r.Library = key, lib
		if r.Slot > 0 {
			// The libraries of a pinned fan-out sit together from its slot.
			r.Slot += pl.libraryIndex(base.row, lib)
		}
		pl.byRow[key] = &r
		return &r
	}
	src, param := sourceOf(row.HubID, row.Kind)
	if r := pl.bySrc[sourceKey(src, param)]; r != nil {
		return r
	}
	return nil
}

// suppressed reports whether the engine's own row of a source stays off
// the page: the page lists the source only as rows built for it (another
// window, other libraries), so Ether doesn't add the plain row as a pick.
func (pl *pagePlan) suppressed(row core.HomeRow) bool {
	if pl == nil {
		return false
	}
	if _, _, ok := pageRowID(row.HubID); ok {
		return false
	}
	src, param := sourceOf(row.HubID, row.Kind)
	return pl.listed[src] && pl.bySrc[sourceKey(src, param)] == nil
}

// libraryIndex is a library's position in a one-row-per-library row.
func (pl *pagePlan) libraryIndex(row *core.Row, lib core.LibraryID) int {
	if row == nil {
		return 0
	}
	if i := slices.Index(row.Scope.Libraries, lib); i >= 0 {
		return i
	}
	return 0
}

// maxRows is the page length for a device; def when the owner set none.
func (pl *pagePlan) maxRows(device string, def int) int {
	if pl == nil {
		return def
	}
	if n := pl.page.Length.For(device); n > 0 {
		return n
	}
	return def
}

// skipHome reports whether a library page keeps Home's titles off it.
func (pl *pagePlan) skipHome() bool {
	return pl == nil || pl.page.SkipHome == nil || *pl.page.SkipHome
}

// weekEligible spreads a row shown n days a week across the week.
func weekEligible(user int64, key string, hc core.HubContext, n int) bool {
	if n <= 0 || n >= 7 {
		return true
	}
	h := fnv.New32a()
	fmt.Fprintf(h, "%d:%s", user, key)
	day := (int(hc.Now.Weekday()) + 6) % 7
	return (day+int(h.Sum32()%7))%7 < n
}

// active reports whether a rule lets its row appear for this viewer, device
// and moment.
func (r *rule) active(user int64, hc core.HubContext) bool {
	code, _ := r.hiddenReason(user, hc)
	return code == ""
}

// hiddenReason explains, in plain words, why a rule keeps its row off the
// page right now ("" when it doesn't).
func (r *rule) hiddenReason(user int64, hc core.HubContext) (code, why string) {
	if r == nil {
		return "", ""
	}
	switch {
	case r.Gate == "audience":
		return "audience", "Not for this viewer"
	case r.Status == "off":
		if r.Layer == core.RowLayerViewer {
			return "hidden_viewer", "Hidden for this viewer"
		}
		return "hidden", "Hidden on this page"
	case len(r.Devices) > 0 && hc.Device != "" && !slices.Contains(r.Devices, hc.Device):
		return "device", "Not shown on " + deviceLabel(hc.Device)
	case !r.When.Allows(hc.Now):
		return "outside_schedule", "Outside its schedule (" + ScheduleSummary(r.When, nil) + ")"
	case !weekEligible(user, r.Row, hc, r.MaxDaysPerWeek):
		return "weekly_cap", fmt.Sprintf("Shown %d days a week; not today", r.MaxDaysPerWeek)
	}
	return "", ""
}

func deviceLabel(d string) string {
	switch d {
	case core.DeviceTV:
		return "TV"
	case core.DeviceMobile:
		return "phones and tablets"
	case core.DeviceDesktop:
		return "computers"
	case core.DeviceWeb:
		return "the web"
	}
	return d
}

// why is the "Why here" line for a row a rule placed at slot.
func (r *rule) why(slot int) string {
	who := "for everyone"
	switch {
	case r.Layer == core.RowLayerViewer, r.audience.Mode == core.AudienceOnly && len(r.audience.Viewers) == 1:
		who = "just for this viewer"
	case r.audience.Mode == core.AudienceOnly:
		who = "for the chosen viewers"
	case r.audience.Mode == core.AudienceExcept:
		who = "for everyone but the viewers you left out"
	}
	switch r.Mode {
	case core.PlacementPinned:
		if r.Slot <= 0 {
			return "Pinned " + who
		}
		return fmt.Sprintf("Pinned to #%d %s", r.Slot, who)
	case core.PlacementShow:
		return "Always shown " + who + "; its place follows what this viewer plays"
	case core.PlacementAuto:
		return "Auto: shown when it fits what this viewer plays"
	}
	return "Fits what this viewer plays"
}

// placedCollections lists the collections a page places for a viewer,
// whatever the hour (owner rows are fetched for this set, so a new daypart
// never finds its rows unfetched); live restricts it to the rows active now.
func (pl *pagePlan) placedCollections(user int64, hc *core.HubContext) []string {
	if pl == nil {
		return nil
	}
	var ids []string
	seen := map[string]bool{}
	for _, r := range pl.rules {
		if r.row == nil || r.Status == "off" || r.Gate != "" {
			continue
		}
		var id string
		switch {
		case r.Source == SourceCollection:
			id = r.row.Source.Param("collection", "")
		case strings.HasPrefix(r.Source, "plugin:"):
			id = core.PluginListRowID(r.Source, r.row.Source.Params)
		default:
			continue
		}
		if id == "" || seen[id] {
			continue
		}
		if hc != nil && !r.active(user, *hc) {
			continue
		}
		seen[id] = true
		ids = append(ids, id)
	}
	return ids
}
