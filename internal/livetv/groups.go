package livetv

import (
	"context"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// Group rules shape a large provider lineup before anything is stored: the
// owner's excluded groups, adult groups when hidden, and groups that first
// appear on a refresh when new groups are not included automatically. Left
// out channels are never imported, so they cost no storage, no guide
// listings and no lineup work on any client request.

// adultGroupName is the fallback for providers without an adult flag: a
// group named for adult content. Xtream panels flag it themselves.
var adultGroupName = regexp.MustCompile(`(?i)(^|[^a-z])(adult|xxx|porn|18\+)([^a-z]|$)`)

func normGroup(name string) string { return strings.ToLower(strings.TrimSpace(name)) }

// sourceGroups lists the groups of cs in first-appearance order.
func sourceGroups(cs []Channel) []SourceGroup {
	var out []SourceGroup
	at := map[string]int{}
	for _, c := range cs {
		key := normGroup(c.Group)
		i, ok := at[key]
		if !ok {
			i = len(out)
			at[key] = i
			out = append(out, SourceGroup{Name: strings.TrimSpace(c.Group)})
		}
		out[i].Channels++
		if c.adult {
			out[i].Adult = true
		}
	}
	for i := range out {
		if !out[i].Adult && adultGroupName.MatchString(out[i].Name) {
			out[i].Adult = true
		}
	}
	return out
}

// applyGroupRules records the provider's groups on src and returns the
// channels the owner's rules keep, renumbered when the owner orders groups.
func (s *Service) applyGroupRules(ctx context.Context, src *Source, cs []Channel) []Channel {
	groups := sourceGroups(cs)
	known := map[string]bool{}
	if old, err := s.storedSourceByID(ctx, src.ID); err == nil {
		for _, g := range old.Groups {
			known[normGroup(g.Name)] = true
		}
	}
	excluded := map[string]bool{}
	for _, name := range src.ExcludeGroups {
		excluded[normGroup(name)] = true
	}
	// New groups are only recognisable once a lineup has been seen.
	if src.NewGroups == "exclude" && len(known) > 0 {
		for _, g := range groups {
			if key := normGroup(g.Name); !known[key] && !excluded[key] {
				excluded[key] = true
				src.ExcludeGroups = append(src.ExcludeGroups, g.Name)
			}
		}
	}
	drop := map[string]bool{}
	for i := range groups {
		key := normGroup(groups[i].Name)
		if excluded[key] || src.HideAdult && groups[i].Adult {
			groups[i].Excluded = true
			drop[key] = true
		}
	}
	src.Groups = groups
	if len(drop) > 0 {
		kept := cs[:0]
		for _, c := range cs {
			if !drop[normGroup(c.Group)] {
				kept = append(kept, c)
			}
		}
		cs = kept
	}
	return orderGroups(src.GroupOrder, cs)
}

// orderGroups numbers channels 1… down the owner's group order, keeping
// the provider's order inside each group and for unlisted groups. An empty
// order leaves the provider's numbers.
func orderGroups(order []string, cs []Channel) []Channel {
	if len(order) == 0 {
		return cs
	}
	rank := map[string]int{}
	for i, name := range order {
		if _, ok := rank[normGroup(name)]; !ok {
			rank[normGroup(name)] = i
		}
	}
	first := map[string]int{}
	for i, c := range cs {
		if _, ok := first[normGroup(c.Group)]; !ok {
			first[normGroup(c.Group)] = i
		}
	}
	type entry struct {
		c          Channel
		rank, pos  int
		groupFirst int
	}
	entries := make([]entry, len(cs))
	for i, c := range cs {
		r, ok := rank[normGroup(c.Group)]
		if !ok {
			r = len(order)
		}
		entries[i] = entry{c: c, rank: r, pos: i, groupFirst: first[normGroup(c.Group)]}
	}
	slices.SortStableFunc(entries, func(a, b entry) int {
		if a.rank != b.rank {
			return a.rank - b.rank
		}
		if a.groupFirst != b.groupFirst {
			return a.groupFirst - b.groupFirst
		}
		return a.pos - b.pos
	})
	for i := range entries {
		cs[i] = entries[i].c
		cs[i].Number = strconv.Itoa(i + 1)
		cs[i].ProviderNumber = cs[i].Number
		cs[i].numberMissing = false
	}
	return cs
}

// sourceChannels returns the stored channels of one source.
func (s *Service) sourceChannels(ctx context.Context, sourceID string) ([]Channel, error) {
	all, err := s.allChannels(ctx)
	if err != nil {
		return nil, err
	}
	var out []Channel
	for _, c := range all {
		if c.SourceID == sourceID {
			out = append(out, c)
		}
	}
	return out, nil
}
