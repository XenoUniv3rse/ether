package experience

import (
	"context"
	"errors"
	"log/slog"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/viewerpolicy"
)

// RowSource supplies neutral candidate rows for one viewer and device.
type RowSource interface {
	Rows(context.Context, frontend.RowRequest) ([]frontend.Row, error)
}

// RowServiceDeps are host-owned dependencies. AllowRow and AllowEntry must
// recheck current membership and visibility, including non-title entries.
type RowServiceDeps struct {
	Source     RowSource
	Visibility Visibility
	Active     func(context.Context, *core.User) bool
	AllowRow   func(context.Context, *core.User, frontend.Row) bool
	AllowEntry func(context.Context, *core.User, frontend.RowEntry) bool
	Layout     func(context.Context, frontend.RowRequest, []frontend.Row) ([]frontend.Row, error)
	Receipts   *Receipts
	Situation  func(context.Context, frontend.RowRequest) (core.HomeSituation, error)
	// HomeFallback supplies bounded library rows when a neutral Home has no
	// discovery shelves left after policy filtering. Plex does not use it.
	HomeFallback func(context.Context, frontend.RowRequest) ([]frontend.Row, error)
	// PrefetchTitles optionally warms parent shows before per-entry visibility
	// checks so rows containing episodes never trigger per-show catalog reads.
	PrefetchTitles func(context.Context, []core.RatingKey)
	// PrepareRows optionally batches collection and playlist checks for a
	// single row-filtering pass (called fresh before each pass).
	PrepareRows func(context.Context, *core.User, []frontend.Row) context.Context
	// RankedSource disables retries when the source ranks policy-filtered candidates once.
	RankedSource bool
}

// RowsService produces viewer-scoped, policy-checked rows.
type RowsService struct {
	d RowServiceDeps
}

func NewRowsService(d RowServiceDeps) (*RowsService, error) {
	if d.Source == nil || d.Active == nil || d.AllowRow == nil || d.AllowEntry == nil || d.Receipts == nil || d.Situation == nil || d.Visibility.GetTitle == nil || d.Visibility.SectionFor == nil {
		return nil, errors.New("experience: incomplete row dependencies")
	}
	return &RowsService{d: d}, nil
}

// Rows filters each tagged entry before limiting and never accepts client-made
// interaction identifiers. Candidate requests expand to a bounded prefix when
// filtering would otherwise leave a row short.
func (s *RowsService) Rows(ctx context.Context, req frontend.RowRequest, protocol core.Protocol) ([]frontend.Row, error) {
	return s.rows(ctx, req, protocol, true)
}

// SnapshotRows builds and authorizes the same ranked rows without issuing
// impressions; a paged row view issues receipts only for delivered entries.
func (s *RowsService) SnapshotRows(ctx context.Context, req frontend.RowRequest, protocol core.Protocol) ([]frontend.Row, error) {
	return s.rows(ctx, req, protocol, false)
}

func (s *RowsService) rows(ctx context.Context, req frontend.RowRequest, protocol core.Protocol, issue bool) ([]frontend.Row, error) {
	if req.Viewer == nil || req.Device.ID == "" || protocol == "" || !s.d.Active(ctx, req.Viewer) {
		return nil, errors.New("experience: invalid row viewer")
	}
	if req.HomeFallbackOnly && (protocol != "vela" || req.Placement != frontend.PlacementHome || req.Purpose != "" || req.ViewID != "" || req.Item != 0 || req.Limit < 1 || req.Limit > 6 || len(req.ExcludeItems) > 6) {
		return nil, core.ErrNotFound
	}
	limit := req.Limit
	if limit <= 0 {
		limit = 50
	}
	fetch := limit
	var out []frontend.Row
	var err error
	for {
		sourceReq := req
		sourceReq.Limit = fetch
		var candidates []frontend.Row
		var e error
		if !req.HomeFallbackOnly {
			candidates, e = s.d.Source.Rows(ctx, sourceReq)
		}
		if e != nil {
			return nil, e
		}
		if req.Purpose != "" {
			kept := make([]frontend.Row, 0, len(candidates))
			for _, row := range candidates {
				if row.Purpose == req.Purpose {
					kept = append(kept, row)
				}
			}
			candidates = kept
		}
		if s.d.PrefetchTitles != nil {
			var parents []core.RatingKey
			seenParent := map[core.RatingKey]bool{}
			for _, row := range candidates {
				for _, entry := range row.Entries {
					if entry.Kind != frontend.RowEntryTitle || entry.Title == nil {
						continue
					}
					show := seriesOfTitle(*entry.Title)
					if show == entry.Title.Key {
						continue
					}
					if show != 0 && !seenParent[show] {
						seenParent[show] = true
						parents = append(parents, show)
					}
				}
			}
			if len(parents) > 0 {
				s.d.PrefetchTitles(ctx, parents)
			}
		}
		passCtx := ctx
		if s.d.PrepareRows != nil {
			passCtx = s.d.PrepareRows(ctx, req.Viewer, candidates)
		}
		out = make([]frontend.Row, 0, len(candidates))
		expand := false
		for _, row := range candidates {
			if !s.d.AllowRow(passCtx, req.Viewer, row) {
				continue
			}
			entries := make([]frontend.RowEntry, 0, len(row.Entries))
			seen := make(map[string]bool, len(row.Entries))
			for _, entry := range row.Entries {
				entry.Handle = ""
				id := entryIdentity(entry)
				key := string(entry.Kind) + ":" + id
				if id != "" && !seen[key] && s.permitted(passCtx, req.Viewer, entry) {
					seen[key] = true
					entries = append(entries, entry)
				}
			}
			if len(entries) < limit && len(row.Entries) >= fetch {
				expand = true
			}
			row.Items = nil
			row.Handle, row.ImpressionID = "", ""
			if len(entries) > limit {
				entries, row.More = entries[:limit], true
			}
			row.Entries = entries
			if len(entries) > 0 {
				out = append(out, row)
			}
		}
		if s.d.RankedSource || !expand || fetch >= 1000 || ctx.Err() != nil {
			break
		}
		fetch = min(fetch*2, 1000)
	}
	if req.Purpose == "" {
		out, err = s.appendHomeFallback(ctx, req, out, limit)
		if err != nil {
			return nil, err
		}
	}
	if s.d.Layout != nil {
		candidates := make([]LayoutCandidate, len(out))
		layoutInput := make([]frontend.Row, len(out))
		for i, row := range out {
			candidates[i] = LayoutCandidate{ID: row.ID, Title: row.Title, Purpose: row.Purpose, ItemCount: len(row.Entries)}
			layoutInput[i] = row
			layoutInput[i].Entries = append([]frontend.RowEntry(nil), row.Entries...)
		}
		var placements []frontend.Row
		indices, e := ArrangeLayout(candidates, func(_ []LayoutCandidate) ([]string, error) {
			var err error
			placements, err = s.d.Layout(ctx, req, layoutInput)
			if err != nil {
				return nil, err
			}
			ids := make([]string, 0, len(placements))
			for _, placement := range placements {
				ids = append(ids, placement.ID)
			}
			return ids, nil
		})
		if e != nil {
			return nil, e
		}
		original := out
		out = make([]frontend.Row, 0, len(placements))
		next := 0
		for _, placement := range placements {
			if IsSurfaceRow(placement) {
				// Plugin surface stubs carry no titles; only Vela draws
				// plugin documents, so other apps never receive them.
				if protocol == "vela" {
					out = append(out, placement)
				}
				continue
			}
			if next >= len(indices) || original[indices[next]].ID != placement.ID {
				continue
			}
			row := original[indices[next]]
			next++
			row.Layout, row.Style, row.Rank = placement.Layout, placement.Style, placement.Rank
			row.PreviewOnFocus = placement.PreviewOnFocus
			row.RankedTop10 = placement.RankedTop10
			out = append(out, row)
		}
	}

	for index := range out {
		out[index].Layout.PreviewOnFocus = out[index].PreviewOnFocus
	}

	situation, err := s.d.Situation(ctx, req)
	if err != nil {
		return nil, err
	}

	// Layout only sees permitted rows; reject injected or altered rows rather than
	// trusting a layout provider to manufacture authorization decisions.
	passCtx := ctx
	if s.d.PrepareRows != nil {
		passCtx = s.d.PrepareRows(ctx, req.Viewer, out)
	}
	var subjects []ReceiptSubject
	var handles []*string
	var receiptRows []int
	omitted := make([]bool, len(out))
	for i := range out {
		row := &out[i]
		row.Situation = situation
		if IsSurfaceRow(*row) {
			continue
		}
		row.Layout.RankedTop10 = row.RankedTop10
		if row.RankedTop10 {
			row.Entries = row.Entries[:min(10, len(row.Entries))]
			row.More = false
		}
		if !s.d.AllowRow(passCtx, req.Viewer, *row) {
			if req.Placement == frontend.PlacementHome {
				slog.WarnContext(ctx, "experience: dropping Home row", "row", row.ID, "reason", "row no longer authorized")
				omitted[i] = true
				continue
			}
			return nil, errors.New("experience: layout returned unauthorized row")
		}
		row.Items = nil
		for j := range row.Entries {
			if entryIdentity(row.Entries[j]) == "" || !s.permitted(passCtx, req.Viewer, row.Entries[j]) {
				if req.Placement == frontend.PlacementHome {
					slog.WarnContext(ctx, "experience: dropping Home row", "row", row.ID, "reason", "entry no longer authorized")
					omitted[i] = true
					break
				}
				return nil, errors.New("experience: layout returned unauthorized entry")
			}
		}
		if !issue || omitted[i] {
			continue
		}
		impression := &core.NeutralImpression{RowID: row.ID, Kind: row.Kind, Title: row.Title, Purpose: row.Purpose, Rank: row.Rank, Situation: situation, Collection: row.Collection, Item: req.Item, Placement: string(req.Placement), ViewID: req.ViewID}
		for _, entry := range row.Entries {
			impression.Entries = append(impression.Entries, core.NeutralEntryRef{Kind: string(entry.Kind), ID: entryIdentity(entry)})
			if entry.Kind == frontend.RowEntryTitle && entry.Title != nil {
				impression.Items = append(impression.Items, entry.Title.Key)
			}
		}
		subjects = append(subjects, ReceiptSubject{Kind: "row", ID: row.ID, Purpose: row.Purpose, Rank: row.Rank, Impression: impression})
		receiptRows = append(receiptRows, i)
		handles = append(handles, &row.Handle)
		for j := range row.Entries {
			entry := &row.Entries[j]
			id := row.ID + "/" + string(entry.Kind) + "/" + entryIdentity(*entry)
			subjects = append(subjects, ReceiptSubject{Kind: string(entry.Kind), ID: id, Purpose: row.Purpose, Rank: row.Rank, Impression: impression})
			handles = append(handles, &entry.Handle)
		}
	}
	if len(subjects) > 0 {
		tokens, e := s.d.Receipts.IssueBatch(ctx, req.Viewer, req.Device, protocol, subjects)
		// IssueBatch already retries one overlapping policy write. Further
		// writes can land between attempts (a catalogue sync, background
		// metadata refreshes); read the epoch again a bounded number of times
		// rather than failing the page.
		for attempt := 0; attempt < rowsFenceRetries && errors.Is(e, viewerpolicy.ErrChanged) && ctx.Err() == nil; attempt++ {
			tokens, e = s.d.Receipts.IssueBatch(ctx, req.Viewer, req.Device, protocol, subjects)
		}
		if errors.Is(e, ErrReceipt) && req.Placement == frontend.PlacementHome && ctx.Err() == nil {
			// A row can lose authorization between layout and receipt admission.
			// Keep the batch on the normal path and isolate rows on rejection.
			start := 0
			for _, index := range receiptRows {
				n := 1 + len(subjects[start].Impression.Entries)
				subject := subjects[start]
				rowTokens, err := s.d.Receipts.IssueBatch(ctx, req.Viewer, req.Device, protocol, subjects[start:start+n])
				if errors.Is(err, ErrReceipt) {
					slog.WarnContext(ctx, "experience: dropping Home row", "row", subject.ID, "reason", "receipt rejected")
					omitted[index] = true
				} else if err != nil {
					return nil, err
				} else {
					for j, token := range rowTokens {
						*handles[start+j] = token
					}
				}
				start += n
			}
		} else if e != nil {
			return nil, e
		} else {
			for i := range handles {
				*handles[i] = tokens[i]
			}
		}
	}
	kept := out[:0]
	for i, row := range out {
		if !omitted[i] {
			kept = append(kept, row)
		}
	}
	return kept, nil
}

// rowsFenceRetries bounds the extra receipt attempts after the policy epoch
// moved under a page being built.
const rowsFenceRetries = 4

// appendHomeFallback adds policy-checked library shelves when Home is only
// Continue Watching and Next Up. The host supplies the fallback for each surface.
func (s *RowsService) appendHomeFallback(ctx context.Context, req frontend.RowRequest, out []frontend.Row, limit int) ([]frontend.Row, error) {
	if req.Placement != frontend.PlacementHome || s.d.HomeFallback == nil {
		return out, nil
	}
	for _, row := range out {
		if !req.HomeFallbackOnly && row.Purpose != frontend.RowContinue && row.Purpose != frontend.RowNextUp {
			return out, nil
		}
	}
	fallback, err := s.d.HomeFallback(ctx, req)
	if err != nil {
		return nil, err
	}
	for _, row := range fallback {
		if !s.d.AllowRow(ctx, req.Viewer, row) {
			continue
		}
		entries := make([]frontend.RowEntry, 0, min(len(row.Entries), limit))
		seen := map[string]bool{}
		for _, entry := range row.Entries {
			id := string(entry.Kind) + ":" + entryIdentity(entry)
			if entryIdentity(entry) == "" || seen[id] || !s.permitted(ctx, req.Viewer, entry) {
				continue
			}
			seen[id] = true
			entry.Handle = ""
			if len(entries) == limit {
				row.More = true
				break
			}
			entries = append(entries, entry)
		}
		row.Items, row.Handle, row.ImpressionID, row.Entries = nil, "", "", entries
		if len(entries) > 0 {
			out = append(out, row)
		}
	}
	return out, nil
}

func (s *RowsService) permitted(ctx context.Context, viewer *core.User, entry frontend.RowEntry) bool {
	ctx = core.WithCatalogViewer(ctx, viewer)
	if !s.d.AllowEntry(ctx, viewer, entry) {
		return false
	}
	switch entry.Kind {
	case frontend.RowEntryTitle:
		return s.d.Visibility.Visible(ctx, viewer, entry.Title)
	case frontend.RowEntryCollection:
		return s.d.Visibility.CollectionAllowed(viewer, entry.Collection)
	case frontend.RowEntryPlaylist:
		return entry.Playlist.UserID == viewer.ID
	default:
		return true
	}
}

func entryIdentity(e frontend.RowEntry) string {
	n := 0
	for _, present := range []bool{e.Title != nil, e.Collection != nil, e.Playlist != nil, e.LiveChannel != nil, e.Directory != nil} {
		if present {
			n++
		}
	}
	if n != 1 {
		return ""
	}
	switch e.Kind {
	case frontend.RowEntryTitle:
		if e.Title != nil && e.Title.Key != 0 {
			return e.Title.Key.String()
		}
	case frontend.RowEntryCollection:
		if e.Collection != nil && e.Collection.Key != 0 {
			return e.Collection.Key.String()
		}
	case frontend.RowEntryPlaylist:
		if e.Playlist != nil && e.Playlist.Key != 0 {
			return e.Playlist.Key.String()
		}
	case frontend.RowEntryLiveChannel:
		if e.LiveChannel != nil {
			return e.LiveChannel.ID
		}
	case frontend.RowEntryDirectory:
		if e.Directory != nil {
			return e.Directory.ID
		}
	}
	return ""
}

// RowEntryIdentity returns the stable neutral identity of a well-formed entry.
func RowEntryIdentity(e frontend.RowEntry) string { return entryIdentity(e) }

// IssueRowReceipts authorizes only the rows delivered by a server surface. It
// writes one receipt per row, instead of rebuilding and issuing all of Home.
func (s *RowsService) IssueRowReceipts(ctx context.Context, req frontend.RowRequest, protocol core.Protocol, rows []frontend.Row) ([]frontend.Row, error) {
	if len(rows) == 0 {
		return nil, nil
	}
	if len(rows) > 12 || req.Placement != frontend.PlacementHome {
		return nil, ErrReceipt
	}
	out := append([]frontend.Row(nil), rows...)
	subjects := make([]ReceiptSubject, 0, len(out))
	for n := range out {
		row := &out[n]
		if len(row.Entries) == 0 || len(row.Entries) > 50 {
			return nil, ErrReceipt
		}
		imp := &core.NeutralImpression{RowID: row.ID, Kind: row.Kind, Title: row.Title, Purpose: row.Purpose, Rank: row.Rank, Collection: row.Collection, Placement: string(req.Placement), Situation: row.Situation}
		for _, entry := range row.Entries {
			imp.Entries = append(imp.Entries, core.NeutralEntryRef{Kind: string(entry.Kind), ID: entryIdentity(entry)})
			if entry.Kind == frontend.RowEntryTitle && entry.Title != nil {
				imp.Items = append(imp.Items, entry.Title.Key)
			}
		}
		subjects = append(subjects, ReceiptSubject{Kind: "row", ID: row.ID, Purpose: row.Purpose, Rank: row.Rank, Impression: imp})
	}
	tokens, err := s.d.Receipts.IssueBatch(ctx, req.Viewer, req.Device, protocol, subjects)
	if err != nil {
		return nil, err
	}
	for n := range out {
		out[n].Handle = tokens[n]
	}
	return out, nil
}
