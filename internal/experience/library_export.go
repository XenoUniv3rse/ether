package experience

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/reqmemo"
)

// ExportLibraryPage uses the regular catalogue and discovery policy. It does
// not resolve a source, issue a session, or materialize a library in memory.
func (s *Service) ExportLibraryPage(ctx context.Context, viewer *core.Viewer, device core.Device, viewID string, after core.RatingKey, limit int) (frontend.LibraryExportPage, error) {
	ctx = core.WithCatalogViewer(ctx, viewer)
	var viewerID int64
	if viewer != nil {
		viewerID = viewer.ID
	}
	scope, err := reqmemo.Do(ctx, fmt.Sprintf("library:export:%s:%d:%s", s.d.Protocol, viewerID, viewID), func() (exportScope, error) {
		return s.exportScope(ctx, viewer, viewID)
	})
	if err != nil {
		return frontend.LibraryExportPage{}, err
	}
	v, filter := scope.viewer, scope.filter
	if scope.none {
		return frontend.LibraryExportPage{Done: true}, nil
	}
	limit = min(max(limit, 1), 250)
	// An export row shows list facts (genres, directors, three cast) and
	// carries what this viewer's policy reads; a title availability judges
	// on its facts comes back in full. Detail stays with one-title reads.
	q := core.BrowseQuery{Filter: filter, Sort: core.SortKey, AfterKey: after, Limit: limit, NoTotal: true}
	if detail, ok := s.d.Visibility.FactJudge(ctx); ok {
		facts := s.d.Visibility.TitleFacts(v)
		lean := frontend.ExportTitlesOnly(ctx)
		if !lean {
			facts |= core.FactGenres
		}
		q.ListRows = &core.ListRows{Facts: facts, Detail: detail, OmitCredits: lean}
	}
	page, err := s.d.Browse(ctx, q)
	if err != nil {
		return frontend.LibraryExportPage{}, err
	}
	result := frontend.LibraryExportPage{Done: len(page.Items) < limit, Next: after, Items: []core.Title{}}
	if len(page.Items) > 0 {
		result.Next = page.Items[len(page.Items)-1].Key
	}
	candidates := s.d.Visibility.Available(ctx, v, page.Items)
	for _, t := range candidates {
		if s.d.Visibility.TitleAllowed(v, &t) && s.d.Visibility.Playable(ctx, v, &t) {
			result.Items = append(result.Items, t)
		}
	}
	return result, nil
}

// exportScope is what an export of viewID reads besides the library: the
// active viewer, the view's library, its exclusions and the folded filter.
type exportScope struct {
	viewer  *core.User
	library Library
	ex      LibraryPageExclusions
	filter  core.Filter
	none    bool
}

func (s *Service) exportScope(ctx context.Context, viewer *core.Viewer, viewID string) (exportScope, error) {
	v, err := s.viewer(ctx, viewer)
	if err != nil {
		return exportScope{}, err
	}
	id, err := strconv.Atoi(viewID)
	if err != nil || s.d.Browse == nil {
		return exportScope{}, core.ErrNotFound
	}
	var out exportScope
	if s.d.PluginLibraries != nil {
		stored, err := s.d.PluginLibraries.Library(ctx, core.LibraryID(id))
		if err != nil {
			return exportScope{}, core.ErrNotFound
		}
		mt, _, ok := s.publishedLibrary(ctx, stored)
		if !ok || !v.AllowsLibrary(stored.ID) || stored.Source == core.LibraryPlugin {
			return exportScope{}, core.ErrNotFound
		}
		if stored.Source == core.LibraryCollection && stored.CollectionID <= 0 {
			return exportScope{viewer: v, library: Library{ID: int(stored.ID), Name: stored.Name, Type: mt, Virtual: true}, none: true}, nil
		}
		lib := Library{ID: int(stored.ID), Name: stored.Name, Type: mt, Virtual: !core.IsBuiltinLibrary(stored.ID)}
		f := s.libraryBrowseFilter(ctx, v, stored, core.Filter{Type: mt, MaxContentRating: v.MaxContentRating})
		f.Type = mt
		out = exportScope{viewer: v, library: lib, filter: f}
	} else {
		if !SectionAllowed(v, id) {
			return exportScope{}, core.ErrNotFound
		}
		var library *Library
		libs := s.libraries()
		for j := range libs {
			if libs[j].ID == id {
				library = &libs[j]
				break
			}
		}
		if library == nil {
			return exportScope{}, core.ErrNotFound
		}
		out = exportScope{viewer: v, library: *library, filter: core.Filter{Type: library.Type, MaxContentRating: v.MaxContentRating}}
	}
	if s.d.PageExclusions != nil {
		out.ex = s.d.PageExclusions(ctx, v, out.library.Type)
		if out.ex.Err != nil {
			return exportScope{}, out.ex.Err
		}
		if folded, fits, none := viewerBrowseFilter(out.filter, v, out.ex, out.library.Type); fits {
			out.filter, out.none = folded, none
		}
	}
	return out, nil
}

// ExportPolicy stamps every input of viewer's export of viewID except
// catalogue content and availability: the active viewer's restrictions,
// the view's library, hidden titles, blocked facts, the plex.tv share
// predicate, what the access policy reads and the viewer's catalogue rules.
// The host adds its policy generations; equal stamps mean the export
// ExportLibraryPage builds differs only by what the catalogue holds.
func (s *Service) ExportPolicy(ctx context.Context, viewer *core.Viewer, _ core.Device, viewID string) (frontend.ExportPolicy, error) {
	ctx = core.WithCatalogViewer(ctx, viewer)
	scope, err := s.exportScope(ctx, viewer, viewID)
	if err != nil {
		return frontend.ExportPolicy{}, err
	}
	v, ex := scope.viewer, scope.ex
	share := ex.Movie
	if typ := scope.library.Type; typ == core.TypeShow || typ == core.TypeSeason || typ == core.TypeEpisode {
		share = ex.Television
	}
	var rules int64
	rulesJudged := false
	if s.d.ViewerRules != nil {
		rules, rulesJudged = s.d.ViewerRules(v)
	}
	facts := s.d.Visibility.TitleFacts(v)
	sections := append([]core.LibraryID(nil), v.AllowedLibraries...)
	if v.IsOwner {
		sections = nil
	}
	stamp := struct {
		Viewer                      int64
		Owner                       bool
		MaxRating                   string
		Sections                    []core.LibraryID
		Library                     int
		Type                        core.MediaType
		None                        bool
		Hidden                      []core.RatingKey
		Genres, Keywords            []string
		Ratings, NotRatings         []string
		MinRating, ShareMax         string
		Collections, NotCollections []string
		Facts                       core.TitleFacts
		Rules                       int64
		RulesJudged                 bool
		ExtraRules                  []core.LibraryExclusion
		ExtraFallback               string
		CatalogueSources            []string
		CollectionMembers           int64
	}{v.ID, v.IsOwner, v.MaxContentRating, sections, scope.library.ID, scope.library.Type, scope.none, ex.Keys, ex.ExcludeGenres, ex.ExcludeKeywords,
		share.ContentRatings, share.ExcludeContentRatings, share.MinContentRating, share.MaxContentRating, share.IncludeCollectionNames, share.ExcludeCollectionNames, facts, rules, rulesJudged,
		scope.filter.ExtraRules, scope.filter.ExtraFallback, scope.filter.CatalogueSources, scope.filter.CollectionMembers}
	raw, err := json.Marshal(stamp)
	if err != nil {
		return frontend.ExportPolicy{}, err
	}
	sum := sha256.Sum256(raw)
	judged := v.MaxContentRating != "" || len(sections) > 0 || len(ex.ExcludeGenres) > 0 || len(ex.ExcludeKeywords) > 0 || facts != 0 || rulesJudged ||
		len(scope.filter.ExtraRules) > 0 || scope.filter.ExtraFallback != "" || len(scope.filter.CatalogueSources) > 0 || scope.filter.CollectionMembers > 0 ||
		len(share.ContentRatings) > 0 || len(share.ExcludeContentRatings) > 0 || share.MinContentRating != "" || share.MaxContentRating != "" ||
		len(share.IncludeCollectionNames) > 0 || len(share.ExcludeCollectionNames) > 0
	return frontend.ExportPolicy{Stamp: hex.EncodeToString(sum[:]), TitleJudged: judged}, nil
}
