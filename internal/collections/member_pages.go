package collections

import (
	"context"

	"github.com/braedonsaunders/ether/internal/core"
)

// VisitCollectionMemberPages reads the same ordered members and total as Items,
// sharing membership, release validation and title hydration across collections.
// The visitor returns the collections that still need another page.
func (s *Service) VisitCollectionMemberPages(ctx context.Context, user *core.User, keys []core.RatingKey, size int, visit func(map[core.RatingKey]core.Page) ([]core.RatingKey, error)) error {
	var ids []int64
	for _, key := range keys {
		base, _ := key.CollectionSection()
		if !key.IsEphemeral() && base.Kind() == core.KindSmart {
			ids = append(ids, base.TMDB())
		}
	}
	if len(ids) == 0 {
		return nil
	}
	specs, err := s.rowsByIDs(ctx, ids)
	if err != nil {
		return err
	}
	var rows []*specRow
	var requested []core.RatingKey
	seen := make(map[core.RatingKey]bool)
	for _, key := range keys {
		base, section := key.CollectionSection()
		row := specs[base.TMDB()]
		if key.IsEphemeral() || base.Kind() != core.KindSmart || seen[key] ||
			row == nil || !visible(row, user) || section != "" && row.spec.MediaType != MediaMixed {
			continue
		}
		seen[key] = true
		requested = append(requested, key)
		rows = append(rows, row)
	}
	if len(rows) == 0 {
		return nil
	}
	served, err := s.servedMembersBatch(ctx, rows, user)
	if err != nil {
		return err
	}
	byRelease := [2][][]core.RatingKey{}
	groupByKey := make(map[core.RatingKey]int, len(rows))
	for i, row := range rows {
		group := 0
		if row.spec.ReleaseMode == "upcoming" || row.spec.ReleaseMode == "mixed" {
			group = 1
		}
		key := requested[i]
		groupByKey[key] = group
		_, section := key.CollectionSection()
		served[i] = sectionKeys(served[i], section)
		byRelease[group] = append(byRelease[group], served[i])
	}
	var valid [2]map[core.RatingKey]bool
	for group, members := range byRelease {
		keys, err := s.mirror.ValidKeys(ctx, uniqueMemberKeys(members), group == 1)
		if err != nil {
			return err
		}
		valid[group] = make(map[core.RatingKey]bool, len(keys))
		for _, key := range keys {
			valid[group][key] = true
		}
	}
	memberKeys := make(map[core.RatingKey][]core.RatingKey, len(rows))
	for i, key := range requested {
		for _, member := range served[i] {
			if valid[groupByKey[key]][member] {
				memberKeys[key] = append(memberKeys[key], member)
			}
		}
	}
	size = max(size, 1)
	for offset := 0; len(requested) > 0; offset += size {
		pageKeys := [2][][]core.RatingKey{}
		prefixes := make(map[core.RatingKey][]core.RatingKey, len(requested))
		for _, key := range requested {
			members := memberKeys[key]
			prefix := members[min(offset, len(members)):min(offset+size, len(members))]
			prefixes[key] = prefix
			group := groupByKey[key]
			pageKeys[group] = append(pageKeys[group], prefix)
		}
		var hydrated [2]map[core.RatingKey]core.Title
		for group, pages := range pageKeys {
			keys := uniqueMemberKeys(pages)
			var titles []core.Title
			if group == 0 {
				titles, err = s.mirror.Titles(ctx, keys)
			} else {
				titles, err = s.mirror.TitlesIncludingFuture(ctx, keys)
			}
			if err != nil {
				return err
			}
			hydrated[group] = make(map[core.RatingKey]core.Title, len(titles))
			for _, title := range titles {
				hydrated[group][title.Key] = title
			}
		}
		pages := make(map[core.RatingKey]core.Page, len(requested))
		for _, key := range requested {
			page := core.Page{Total: len(memberKeys[key])}
			for _, member := range prefixes[key] {
				if title, ok := hydrated[groupByKey[key]][member]; ok {
					page.Items = append(page.Items, title)
				}
			}
			pages[key] = page
		}
		next, err := visit(pages)
		if err != nil {
			return err
		}
		requested = requested[:0]
		for _, key := range next {
			if offset+size < len(memberKeys[key]) && len(pages[key].Items) != 0 {
				requested = append(requested, key)
			}
		}
	}
	return nil
}
