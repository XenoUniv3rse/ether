package personalize

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"time"

	"github.com/braedonsaunders/ether/internal/contentrating"
	"github.com/braedonsaunders/ether/internal/core"
)

// Owner previews of Recommendations pages: the same assembly and
// arrangement the apps get, for a viewer (or the typical viewer), a device
// and a moment, under the saved page or an unsaved draft. Nothing is
// stored, memoized or recorded as an impression.

type previewPageKey struct{}

// previewKey returns the page a preview asks for, when it is a rows tab of
// the section's library (a library's second rows tab is not its default).
func previewKey(ctx context.Context) (core.PageKey, bool) {
	k, ok := ctx.Value(previewPageKey{}).(core.PageKey)
	return k, ok
}

// PreviewPage assembles a page for a viewer at a moment under a draft (or
// the saved page), without changing anything persisted.
func (e *Engine) PreviewPage(ctx context.Context, viewer *core.User, key core.PageKey, req core.PagePreviewRequest) (*core.PagePreview, error) {
	if !key.Valid() {
		return nil, &ValidationError{Problems: []Problem{{"page", "must be home or library:<id>/<tab>"}}}
	}
	ctx, user, hc, err := e.previewContext(ctx, viewer, key, req.Page, req.Device, req.At)
	if err != nil {
		return nil, err
	}
	count := req.Count
	if count <= 0 {
		count = 12
	}
	rows, err := e.previewRows(ctx, user, hc, key, count)
	if err != nil {
		return nil, err
	}
	pl := e.planFor(ctx, user.ID, sectionOfKey(key))
	library, _, _ := key.Library()
	libs := e.presentationLibraries(ctx)
	out := &core.PagePreview{Rows: []core.HomeRow{}, Picks: []core.PreviewPick{}, NotShown: []core.NotShownRow{}, GeneratedAt: e.now(), Context: hc}
	for _, r := range rows {
		switch {
		case !r.Hidden:
			r.Present = presentationFor(r, libs, int(library))
			out.Rows = append(out.Rows, e.withAccessFlags(ctx, r, user))
		case r.RowID != "":
			ns := core.NotShownRow{RowID: r.RowID, Title: r.Title, Reason: r.HiddenReason}
			if rr := pl.byRow[r.RowID]; rr != nil {
				e.notShownHint(&ns, rr, user, hc)
			}
			out.NotShown = append(out.NotShown, ns)
		case r.HiddenCode == "page_full":
			out.Picks = append(out.Picks, core.PreviewPick{Kind: r.Kind, Title: r.Title, Personal: personalBadge(r.Kind), Why: "Ether may use it when there's room"})
		}
	}
	def := e.opts.MaxHomeRows
	if key != core.PageHome {
		def = e.opts.MaxSectionRows
	}
	length := pl.maxRows(hc.Device, def)
	if length <= 0 {
		length = 14 // arrangePage's own fallback
	}
	out.Summary = core.PreviewCounts{Length: length, Picks: len(out.Picks), PicksOn: pl.picks}
	if pl.curated {
		out.Summary.Length = 0
	}
	for _, r := range out.Rows {
		// App-only rows (plugin surfaces Vela draws) don't count toward
		// the page length; the summary says so separately.
		if r.Present.Surface != nil {
			out.Summary.AppOnly++
			continue
		}
		out.Summary.Rows++
		if r.RowID != "" {
			out.Summary.Chosen++
		}
		switch r.Mode {
		case core.PlacementPinned:
			out.Summary.Pinned++
		case core.PlacementShow:
			out.Summary.Shown++
		default:
			out.Summary.Auto++
		}
	}
	out.Access = e.viewerAccess(ctx, viewer)
	return out, nil
}

// previewContext prepares a preview: the draft page (validated), the page
// key, the viewer (nil is the typical viewer) and the request context.
func (e *Engine) previewContext(ctx context.Context, viewer *core.User, key core.PageKey, draft *core.RowPage, device string, at time.Time) (context.Context, *core.User, core.HubContext, error) {
	if draft != nil {
		p := *draft
		if p.Key == "" {
			p.Key = key
		}
		if p.Key != key {
			return ctx, nil, core.HubContext{}, &ValidationError{Problems: []Problem{{"page.key", "doesn't match the page"}}}
		}
		norm, err := e.NormalizePage(ctx, p)
		if err != nil {
			return ctx, nil, core.HubContext{}, err
		}
		ctx = withDraftPage(ctx, norm)
	}
	ctx = context.WithValue(ctx, previewPageKey{}, key)
	user := viewer
	if user == nil {
		user = &core.User{}
	}
	if device == "" {
		device = core.DeviceTV
	}
	if !validDevices[device] {
		return ctx, nil, core.HubContext{}, &ValidationError{Problems: []Problem{{"device", "must be tv, mobile, web or desktop"}}}
	}
	if at.IsZero() {
		at = e.now()
	}
	at = at.In(e.loc)
	hc := core.HubContext{Device: device, Now: at, Daypart: core.DaypartAt(at), Weekend: core.IsWeekend(at)}
	return ctx, user, hc, nil
}

// previewRows are the page's rows for the preview: served rows in order,
// then the hidden ones with reasons. Plugin library pages arrange the
// library's candidate shelves.
func (e *Engine) previewRows(ctx context.Context, user *core.User, hc core.HubContext, key core.PageKey, count int) ([]core.HomeRow, error) {
	id, tab, isLib := key.Library()
	if isLib {
		lib, ok := e.pageLibraryFor(ctx, id)
		if ok && lib.Plugin != "" {
			var cands []core.HomeRow
			if e.opts.PageCandidates != nil {
				cands = e.opts.PageCandidates(ctx, user, id, tab)
			}
			pl := e.pluginDefaultPlan(e.planFor(ctx, user.ID, string(key)), cands)
			hc.SectionID = int(id)
			rows := arrangePage(cands, pl, user.ID, hc, e.opts.MaxSectionRows)
			return e.appendAbsentPageRows(ctx, rows, pl, user, hc, "", map[string]bool{}), nil
		}
	}
	p, err := e.PreviewHome(ctx, user, hc, sectionOfKey(key), count)
	if err != nil {
		return nil, err
	}
	if key == core.PageHome {
		pl := e.planFor(ctx, user.ID, surfHome)
		var visible []core.HomeRow
		for _, r := range p.Rows {
			if !r.Hidden {
				visible = append(visible, r)
			}
		}
		withStubs := e.withSurfaceRows(ctx, visible, pl, user.ID, hc, map[string]core.NeutralHomeRow{})
		hidden := []core.HomeRow{}
		for _, r := range p.Rows {
			if r.Hidden {
				hidden = append(hidden, r)
			}
		}
		for i := range withStubs {
			withStubs[i].Slot = i + 1
		}
		return append(withStubs, hidden...), nil
	}
	return p.Rows, nil
}

func sectionOfKey(key core.PageKey) string {
	if id, _, ok := key.Library(); ok {
		return librarySectionName(int(id))
	}
	return surfHome
}

func (e *Engine) pageLibraryFor(ctx context.Context, id core.LibraryID) (core.PageLibrary, bool) {
	for _, l := range e.pageLibraries(ctx) {
		if l.ID == id {
			return l, true
		}
	}
	return core.PageLibrary{}, false
}

// notShownHint points at where a not-shown row would show: a schedule's
// next matching day, or a viewer the row is for.
func (e *Engine) notShownHint(ns *core.NotShownRow, r *rule, user *core.User, hc core.HubContext) {
	if r.Gate == "audience" && r.row != nil && r.row.Placement.Audience.Mode == core.AudienceOnly && len(r.row.Placement.Audience.Viewers) > 0 {
		ns.PreviewAs = r.row.Placement.Audience.Viewers[0]
		return
	}
	if r.When.IsZero() || r.When.Allows(hc.Now) {
		return
	}
	// The next moment within a year (checked at 9 PM each day) the
	// schedule lets the row show.
	for d := 1; d <= 366; d++ {
		t := time.Date(hc.Now.Year(), hc.Now.Month(), hc.Now.Day()+d, 21, 0, 0, 0, hc.Now.Location())
		if r.When.Allows(t) {
			ns.PreviewAt = &t
			return
		}
	}
}

// withAccessFlags flags a row the previewed viewer's limits can't vouch
// for: unrated titles, a plugin surface's declared rating.
func (e *Engine) withAccessFlags(ctx context.Context, r core.HomeRow, user *core.User) core.HomeRow {
	if user == nil || user.MaxContentRating == "" {
		return r
	}
	if r.Present.Surface != nil {
		rating := ""
		if e.opts.Surfaces != nil {
			for _, s := range e.opts.Surfaces(ctx) {
				if s.WireID == r.Present.Surface.Surface {
					rating = s.ContentRating
				}
			}
		}
		if rating == "" {
			r.Flags = append(r.Flags, "Rating not known: the plugin declares none")
		} else {
			r.Flags = append(r.Flags, r.Title+" · "+rating+" (declared by the plugin)")
		}
		return r
	}
	unrated := 0
	for _, t := range r.Items {
		if t.ContentRating == "" {
			unrated++
		}
	}
	if unrated > 0 {
		r.Flags = append(r.Flags, fmt.Sprintf("%d unrated %s", unrated, map[bool]string{true: "title", false: "titles"}[unrated == 1]))
	}
	return r
}

// viewerAccess is the Viewer access strip for a viewer with limits.
func (e *Engine) viewerAccess(ctx context.Context, viewer *core.User) *core.ViewerAccess {
	if viewer == nil || viewer.ID <= 0 || viewer.IsOwner || viewer.MaxContentRating == "" && len(viewer.AllowedLibraries) == 0 {
		return nil
	}
	libs := e.pageLibraries(ctx)
	a := &core.ViewerAccess{Viewer: viewer.ID, Name: firstNonEmptyString(viewer.Title, viewer.Username), AgeLimit: viewer.MaxContentRating, Unrated: "shown", LibrariesTotal: len(libs)}
	if viewer.MaxContentRating != "" && !contentrating.Allowed("", viewer.MaxContentRating) {
		a.Unrated = "hidden"
	}
	for _, l := range libs {
		if viewer.AllowsLibrary(l.ID) {
			a.LibrariesSeen++
		} else {
			a.HiddenLibraries = append(a.HiddenLibraries, l.Name)
		}
	}
	return a
}

// PreviewPageRow returns one draft row's titles (one entry per library for
// a one-row-per-library row) for the drawer's Content tab.
func (e *Engine) PreviewPageRow(ctx context.Context, viewer *core.User, key core.PageKey, req core.RowPreviewRequest) (*core.RowPreview, error) {
	if !key.Valid() {
		return nil, &ValidationError{Problems: []Problem{{"page", "must be home or library:<id>/<tab>"}}}
	}
	base, _ := e.pageForContext(ctx, key)
	row := req.Row
	if row.ID == "" {
		row.ID = stableRowID(key, "preview")
	}
	// The row alone, pinned first on a Personalized copy of the page.
	probe := core.RowPage{Key: key, Mode: core.PagePersonalized, Picks: false, Length: core.PageLength{Rows: core.MaxRowsPerLibraryRow + 1}, SkipHome: base.SkipHome}
	row.Placement = core.RowPlacement{Mode: core.PlacementPinned, Position: 1, Audience: core.Audience{Mode: core.AudienceEveryone}}
	probe.Rows = []core.Row{row}
	pp, err := e.PreviewPage(ctx, viewer, key, core.PagePreviewRequest{Page: &probe, Device: req.Device, At: req.At, Count: max(req.Count, 24)})
	if err != nil {
		return nil, err
	}
	out := &core.RowPreview{Rows: []core.HomeRow{}}
	for _, r := range pp.Rows {
		if r.RowID == row.ID {
			out.Rows = append(out.Rows, r)
			out.Total += len(r.Items) + len(r.Hub.Collections)
		}
	}
	return out, nil
}

// PageImpact counts the viewers a draft page changes and the viewers who
// have changes of their own in it. A change to the page itself reaches
// every viewer its rows' audiences include; a change made just for one
// viewer reaches only that viewer.
func (e *Engine) PageImpact(ctx context.Context, key core.PageKey, draft core.RowPage, device string, at time.Time) (core.PageImpact, error) {
	if !key.Valid() {
		return core.PageImpact{}, &ValidationError{Problems: []Problem{{"page", "must be home or library:<id>/<tab>"}}}
	}
	if e.opts.Users == nil {
		return core.PageImpact{}, nil
	}
	users, err := e.opts.Users.Users(ctx)
	if err != nil {
		return core.PageImpact{}, err
	}
	saved, _ := e.pageForContext(ctx, key)
	shared := func(p core.RowPage) string {
		p.Variants, p.Revision, p.UpdatedAt, p.Offered = nil, 0, time.Time{}, nil
		b, _ := json.Marshal(p)
		return string(b)
	}
	baseChanged := shared(saved) != shared(draft)
	variant := func(p core.RowPage, viewer int64) string {
		v := p.VariantFor(viewer)
		if v == nil || len(v.Rows) == 0 && v.Mode == "" && v.Length == nil {
			return ""
		}
		b, _ := json.Marshal(v)
		return string(b)
	}
	out := core.PageImpact{}
	for i := range users {
		id := users[i].ID
		mine := variant(draft, id)
		if mine != "" {
			out.WithChanges++
		}
		if baseChanged || mine != variant(saved, id) {
			out.Viewers++
		}
	}
	return out, nil
}

// defaultPage is the page served before the owner saves one: Home places
// the published plugin Home-row surfaces from position 2, as before.
func (e *Engine) defaultPage(ctx context.Context, key core.PageKey) core.RowPage {
	p := DefaultPage(key)
	if key != core.PageHome || e.opts.Surfaces == nil {
		return p
	}
	for i, s := range e.opts.Surfaces(ctx) {
		id := s.Plugin + ":" + s.ID
		p.Rows = append(p.Rows, core.Row{ID: stableRowID(key, "surface:"+id), Source: core.RowSource{Kind: SourceSurface, Params: map[string]string{"surface": id}},
			Placement: core.RowPlacement{Mode: core.PlacementPinned, Position: 2 + i, Audience: core.Audience{Mode: core.AudienceEveryone}}})
		p.Offered = append(p.Offered, "surface:"+id)
	}
	slices.SortStableFunc(p.Rows, func(a, b core.Row) int { return a.Placement.Position - b.Placement.Position })
	return p
}

func firstNonEmptyString(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
