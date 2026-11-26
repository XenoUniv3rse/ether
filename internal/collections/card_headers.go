package collections

import (
	"cmp"
	"context"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

type cardSampleSource interface {
	CollectionCardSample(context.Context, core.Filter, int64, string, string, *core.FilterGroup, string, int) (core.Title, error)
}

type cardMemberSource interface {
	CollectionCardMembers(context.Context, core.Filter, int64, string, string, *core.FilterGroup, string, int, int64, int) ([]core.CollectionCardMember, error)
}

func cardFacet(spec core.CollectionSpec) (facet, value string, local bool) {
	if path, ok := localFacetSpec(spec); ok {
		_, rest, _ := strings.Cut(path, "|")
		facet, value, _ = strings.Cut(rest, "|")
		return facet, value, true
	}
	return "", "", false
}

func cardTypes(spec core.CollectionSpec) []core.MediaType {
	if spec.MediaType == MediaMixed {
		return []core.MediaType{core.TypeMovie, core.TypeShow}
	}
	return []core.MediaType{core.MediaType(spec.MediaType)}
}

// WithCollectionCardFilter installs the same indexed viewer predicates used
// for library browsing, including the viewer's share and availability rules.
func WithCollectionCardFilter(filter func(context.Context, *core.User, core.MediaType) (core.Filter, bool, error)) Option {
	return func(s *Service) { s.cardFilter = filter }
}

// CollectionCardHeaders reads enabled shared collection metadata and proves it
// has visible content without building or materializing its members.
func (s *Service) CollectionCardHeaders(ctx context.Context, user *core.User, keys []core.RatingKey) (map[core.RatingKey]*core.Collection, error) {
	out := make(map[core.RatingKey]*core.Collection, len(keys))
	probe, ok := s.cat.(cardSampleSource)
	if !ok || s.cardFilter == nil || user == nil || len(keys) == 0 {
		return out, nil
	}
	ctx = core.WithCatalogViewer(ctx, user)
	ids := make([]int64, 0, len(keys))
	for _, key := range keys {
		base, section := key.CollectionSection()
		if base.Kind() == core.KindSmart && section == "" && !key.IsEphemeral() {
			ids = append(ids, base.TMDB())
		}
	}
	rows, err := s.rowsByIDs(ctx, ids)
	if err != nil {
		return nil, err
	}
	type policy struct {
		filter core.Filter
		fits   bool
	}
	filters := make(map[core.MediaType]policy, 2)
	for _, row := range rows {
		if !visible(row, user) || row.owner != 0 {
			continue
		}
		facet, value, local := cardFacet(row.spec)
		if !local && (row.itemCount == 0 || row.buildHash == "") {
			continue
		}
		for _, typ := range cardTypes(row.spec) {
			p, ok := filters[typ]
			if !ok {
				p.filter, p.fits, err = s.cardFilter(ctx, user, typ)
				if err != nil {
					return nil, err
				}
				filters[typ] = p
			}
			if !p.fits {
				continue
			}
			filter := p.filter
			var anyPersonal *core.FilterGroup
			filter.Group, anyPersonal = cardPersonalRules(row.spec, user.ID)
			sample, err := probe.CollectionCardSample(ctx, filter, row.id, facet, value, anyPersonal, row.spec.ReleaseMode, row.spec.LookaheadDays)
			if err != nil {
				return nil, err
			}
			if sample.Key == 0 {
				continue
			}
			c := s.collectionWithArt(ctx, row, "", nil, nil)
			c.Type = sample.Type
			// The cached count is a hint for the owner. For other
			// viewers the probe only proves one visible member.
			c.ChildCount = 1
			if user.IsOwner {
				c.ChildCount = max(1, row.itemCount)
			}
			if c.RankedTop10 {
				c.ChildCount = min(c.ChildCount, 10)
			}
			if c.PosterPath == "" {
				c.PosterPath = sample.PosterPath
			}
			if c.BackdropPath == "" {
				c.BackdropPath = sample.BackdropPath
			}
			out[c.Key] = &c
			break
		}
	}
	return out, nil
}

// CollectionCardPage reads only the requested window of a shared Home card.
// The mixed page merges movie and show visibility in the stored member order.
func (s *Service) CollectionCardPage(ctx context.Context, user *core.User, key core.RatingKey, after int64, offset, limit int) ([]core.CollectionCardMember, string, error) {
	source, ok := s.cat.(cardMemberSource)
	base, section := key.CollectionSection()
	if !ok || s.cardFilter == nil || user == nil || base.Kind() != core.KindSmart || section != "" || key.IsEphemeral() {
		return nil, "", core.ErrNotFound
	}
	ctx = core.WithCatalogViewer(ctx, user)
	row, err := s.rowByKey(ctx, base)
	if err != nil {
		return nil, "", err
	}
	if !visible(row, user) || row.owner != 0 {
		return nil, "", core.ErrNotFound
	}
	facet, value, local := cardFacet(row.spec)
	if !local && (row.itemCount == 0 || row.buildHash == "") {
		return nil, "", core.ErrNotFound
	}
	limit = min(limit, 101)
	if cap := row.spec.Limit; cap > 0 {
		limit = min(limit, cap-offset)
	}
	if row.spec.RankedTop10 {
		limit = min(limit, 10-offset)
	}
	revision := row.buildHash + ":" + strconv.FormatInt(row.stamp().UnixNano(), 10)
	if limit <= 0 {
		return nil, revision, nil
	}
	var members []core.CollectionCardMember
	for _, typ := range cardTypes(row.spec) {
		filter, fits, err := s.cardFilter(ctx, user, typ)
		if err != nil {
			return nil, "", err
		}
		if !fits {
			continue
		}
		var anyPersonal *core.FilterGroup
		filter.Group, anyPersonal = cardPersonalRules(row.spec, user.ID)
		window, err := source.CollectionCardMembers(ctx, filter, row.id, facet, value, anyPersonal, row.spec.ReleaseMode, row.spec.LookaheadDays, after, limit)
		if err != nil {
			return nil, "", err
		}
		members = append(members, window...)
	}
	slices.SortFunc(members, func(a, b core.CollectionCardMember) int { return cmp.Compare(a.Position, b.Position) })
	return members[:min(len(members), limit)], revision, nil
}

func cardPersonalRules(spec core.CollectionSpec, viewer int64) (required, anyPersonal *core.FilterGroup) {
	var all, any []core.FilterGroup
	for _, rules := range []struct {
		rules   []core.CollectionRule
		exclude bool
	}{{spec.Rules, false}, {spec.Exclude, true}} {
		compiled, _ := compileRules(rules.rules)
		for _, rule := range compiled {
			if !rule.personal() {
				continue
			}
			match := rule.boolVal != ((rule.op == "isNot") != rules.exclude)
			watched := core.FilterGroup{UserID: viewer, StateField: "viewCount", StateOperator: ">"}
			zeroCount := watched
			zeroCount.StateOperator = "="
			noHistory := false
			unwatched := core.FilterGroup{Operator: "or", Children: []core.FilterGroup{
				zeroCount, {UserID: viewer, History: &noHistory},
			}}
			g := unwatched
			if match {
				g = watched
			}
			if rule.field == "inProgress" {
				offset := core.FilterGroup{UserID: viewer, StateField: "viewOffset", StateOperator: ">"}
				if match {
					g = core.FilterGroup{Operator: "and", Children: []core.FilterGroup{offset, unwatched}}
				} else {
					offset.StateOperator = "<="
					g = core.FilterGroup{Operator: "or", Children: []core.FilterGroup{offset, watched}}
				}
			}
			if spec.Match == MatchAny && !rules.exclude {
				any = append(any, g)
			} else {
				all = append(all, g)
			}
		}
	}
	if len(all) > 0 {
		required = &core.FilterGroup{Operator: "and", Children: all}
	}
	if len(any) > 0 {
		anyPersonal = &core.FilterGroup{Operator: "or", Children: any}
	}
	return
}

// CustomCardArtwork is the owner console's preview of assigned artwork. It
// shares the image policy and sizing of Collection Studio.
func (s *Service) CustomCardArtwork(ctx context.Context, id string, width int) ([]byte, string, time.Time, error) {
	row, err := s.rowByID(ctx, id)
	if err != nil {
		return nil, "", time.Time{}, err
	}
	if row.owner != 0 || !row.enabled || row.spec.Artwork.Mode != "custom" || row.spec.Artwork.URL == "" {
		return nil, "", time.Time{}, core.ErrNotFound
	}
	return s.artworkFor(ctx, row, nil, width)
}
