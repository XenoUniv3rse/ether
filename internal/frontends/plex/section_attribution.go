package plex

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/kit/libattr"
	"github.com/braedonsaunders/ether/internal/frontend/kit/reqmemo"
	"github.com/braedonsaunders/ether/internal/frontends/plex/model"
)

// A title's section is the library that lists it for the viewer, not the
// first section of its type (#788): two show libraries over one source, split
// by rules, each own the titles their rules show. Every response that names a
// title's section (search, metadata, history, On Deck, playlists) and every
// librarySectionID filter resolves it here, so they agree.

type preferredSectionsKey struct{}

// withPreferredSections names the sections the request browses or filters
// by. A title more than one library lists is attributed to the first of them
// that lists it, so a section filter and the section the rows report agree.
func withPreferredSections(ctx context.Context, ids ...int) context.Context {
	if len(ids) == 0 {
		return ctx
	}
	return context.WithValue(ctx, preferredSectionsKey{}, slices.Clone(ids))
}

func preferredSections(ctx context.Context) []int {
	ids, _ := ctx.Value(preferredSectionsKey{}).([]int)
	return ids
}

type sectionAttributor struct {
	resolver libattr.Resolver
	byLib    map[frontend.LibraryID]Section
}

// sectionAttributor lists the sections a title can be attributed to for the
// request's viewer: the preferred sections first, then the built-in section
// of the type, then the other catalogue sections in published order. Plugin
// sections own their items by key and are not candidates.
func (s *Server) sectionAttributor(ctx context.Context) sectionAttributor {
	u := FromContext(ctx).User
	var uid int64
	if u != nil {
		uid = u.ID
	}
	prefer := preferredSections(ctx)
	key := fmt.Sprintf("plex:sectionattr:%p:%d:%v", s, uid, prefer)
	a, _ := reqmemo.Do(ctx, key, func() (sectionAttributor, error) {
		secs := s.withLibraryTitles(s.getSectionsCtx(ctx))
		a := sectionAttributor{
			resolver: libattr.Resolver{Candidates: map[frontend.MediaType][]frontend.LibraryID{}, Memo: key},
			byLib:    map[frontend.LibraryID]Section{},
		}
		add := func(sec Section) {
			if sec.PluginID != "" || (sec.Type != frontend.TypeMovie && sec.Type != frontend.TypeShow) {
				return
			}
			if u != nil && len(u.AllowedLibraries) > 0 && !u.IsOwner && !frontend.SectionAllowed(u, sec.ID) {
				return
			}
			lib := sectionLibraryID(sec)
			if _, seen := a.byLib[lib]; seen {
				return
			}
			a.byLib[lib] = sec
			a.resolver.Candidates[sec.Type] = append(a.resolver.Candidates[sec.Type], lib)
		}
		for _, id := range prefer {
			for _, sec := range secs {
				if sec.ID == id {
					add(sec)
				}
			}
		}
		for _, sec := range secs {
			if !isKindSection(sec) {
				add(sec)
			}
		}
		for _, sec := range secs {
			add(sec)
		}
		if sr, ok := s.exp().(frontend.LibraryScopeReader); ok {
			a.resolver.Members = sr.LibraryMembers
		}
		return a, nil
	})
	return a
}

// sectionsForTitles returns each title's section, keyed by title key, with
// one membership read per candidate library for the whole page. A title no
// candidate lists keeps the section of its type.
func (s *Server) sectionsForTitles(ctx context.Context, ts []frontend.Title) map[frontend.RatingKey]Section {
	out := make(map[frontend.RatingKey]Section, len(ts))
	if len(ts) == 0 {
		return out
	}
	a := s.sectionAttributor(ctx)
	libs := a.resolver.Resolve(ctx, ts)
	for i := range ts {
		t := &ts[i]
		if _, done := out[t.Key]; done {
			continue
		}
		if t.Key.IsPluginKey() && t.LibraryID != 0 {
			if sec, ok := s.sectionForLibraryCtx(ctx, t.LibraryID); ok {
				out[t.Key] = sec
			}
			continue
		}
		if sec, ok := a.byLib[libs[t.Key]]; ok {
			out[t.Key] = sec
			continue
		}
		if sec, ok := s.sectionForCtx(ctx, t.Type); ok {
			out[t.Key] = sec
		}
	}
	return out
}

// sectionForTitle is sectionsForTitles for one title. A page rendered through
// renderTitles has already resolved its titles; this reads that memo.
func (s *Server) sectionForTitle(ctx context.Context, t *frontend.Title) (Section, bool) {
	if t == nil {
		return Section{}, false
	}
	sec, ok := s.sectionsForTitles(ctx, []frontend.Title{*t})[t.Key]
	return sec, ok
}

// sectionForKey resolves the section of a stored item known only by key (a
// timeline notification).
func (s *Server) sectionForKey(ctx context.Context, key frontend.RatingKey) (Section, bool) {
	sec, ok := s.keySections(ctx, []frontend.RatingKey{key})[key]
	return sec, ok
}

// titleContainer is itemContainer for a title: the container names the
// section that lists it.
func (s *Server) titleContainer(ctx context.Context, t *frontend.Title) *model.MediaContainer {
	mc := libraryContainer()
	if sec, ok := s.sectionForTitle(ctx, t); ok {
		mc.LibrarySectionID = model.Ptr(sec.ID)
		mc.LibrarySectionTitle = sec.Title
		mc.LibrarySectionUUID = sec.UUID
	}
	return mc
}

// keySections attributes stored entries known only by key (playlist items)
// in one pass.
func (s *Server) keySections(ctx context.Context, keys []frontend.RatingKey) map[frontend.RatingKey]Section {
	return s.playlistEntrySections(ctx, keys, s.playlistPluginTitles(ctx, keys))
}

func (s *Server) playlistEntrySections(ctx context.Context, keys []frontend.RatingKey, plugins map[frontend.RatingKey]frontend.Title) map[frontend.RatingKey]Section {
	ts := make([]frontend.Title, 0, len(keys))
	for _, k := range keys {
		if k.IsPluginKey() {
			if title, ok := plugins[k]; ok {
				ts = append(ts, title)
			}
			continue
		}
		t := playlistKeyMediaType(k)
		switch k.Kind() {
		case frontend.KindSeason:
			t = frontend.TypeSeason
		case frontend.KindEpisode:
			t = frontend.TypeEpisode
		}
		ts = append(ts, frontend.Title{Key: k, Type: t})
	}
	return s.sectionsForTitles(ctx, ts)
}

// sectionIDList parses a comma-separated section id filter.
func sectionIDList(raw string) []int {
	var ids []int
	for _, part := range strings.Split(raw, ",") {
		if id, err := strconv.Atoi(strings.TrimSpace(part)); err == nil {
			ids = append(ids, id)
		}
	}
	return ids
}
