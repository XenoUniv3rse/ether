package collections

import (
	"context"

	"github.com/braedonsaunders/ether/internal/core"
)

// CollectionPosterCandidates returns poster-bearing titles within the same
// first 20 released items used by single-collection artwork resolution.
// A present, empty map entry means the collection has no poster candidates.
func (s *Service) CollectionPosterCandidates(ctx context.Context, user *core.User, keys []core.RatingKey) (map[core.RatingKey][]core.Title, error) {
	const limit = 20
	var ids []int64
	for _, key := range keys {
		base, _ := key.CollectionSection()
		if !key.IsEphemeral() && base.Kind() == core.KindSmart {
			ids = append(ids, base.TMDB())
		}
	}
	rows, err := s.rowsByIDs(ctx, ids)
	if err != nil {
		return nil, err
	}
	var directIDs []int64
	var directKeys []core.RatingKey
	var batchRows []*specRow
	var batchKeys []core.RatingKey
	plainViewer := s.plainMemberViewer(user)
	for _, key := range keys {
		base, section := key.CollectionSection()
		row := rows[base.TMDB()]
		if key.IsEphemeral() || base.Kind() != core.KindSmart || row == nil || !visible(row, user) ||
			row.spec.ReleaseMode == "upcoming" || row.spec.ReleaseMode == "mixed" ||
			section != "" && row.spec.MediaType != MediaMixed {
			continue
		}
		if plainViewer && s.plainPersistedMembers(row) {
			directIDs = append(directIDs, row.id)
			directKeys = append(directKeys, key)
		} else {
			batchRows = append(batchRows, row)
			batchKeys = append(batchKeys, key)
		}
	}
	heads, err := s.mirror.CollectionMemberHeads(ctx, directIDs, limit, true)
	if err != nil {
		return nil, err
	}
	perCollection := make(map[core.RatingKey][]core.RatingKey, len(keys))
	for i, key := range directKeys {
		perCollection[key] = heads[directIDs[i]].Keys
	}
	if len(batchRows) > 0 {
		served, err := s.servedMembersBatch(ctx, batchRows, user)
		if err != nil {
			return nil, err
		}
		valid, err := s.mirror.ValidKeys(ctx, uniqueMemberKeys(served), false)
		if err != nil {
			return nil, err
		}
		validSet := make(map[core.RatingKey]bool, len(valid))
		for _, key := range valid {
			validSet[key] = true
		}
		for i, key := range batchKeys {
			_, section := key.CollectionSection()
			var prefix []core.RatingKey
			for _, member := range sectionKeys(served[i], section) {
				if validSet[member] {
					prefix = append(prefix, member)
					if len(prefix) == limit {
						break
					}
				}
			}
			perCollection[key] = prefix
		}
	}
	prefixes := make([][]core.RatingKey, 0, len(perCollection))
	for _, prefix := range perCollection {
		prefixes = append(prefixes, prefix)
	}
	titles, err := s.mirror.Titles(ctx, uniqueMemberKeys(prefixes))
	if err != nil {
		return nil, err
	}
	posters := make(map[core.RatingKey]core.Title, len(titles))
	for _, title := range titles {
		if title.PosterPath != "" {
			posters[title.Key] = title
		}
	}
	out := make(map[core.RatingKey][]core.Title, len(perCollection))
	for key, prefix := range perCollection {
		out[key] = nil
		for _, member := range prefix {
			if title, ok := posters[member]; ok {
				out[key] = append(out[key], title)
			}
		}
	}
	return out, nil
}

func uniqueMemberKeys(groups [][]core.RatingKey) []core.RatingKey {
	seen := make(map[core.RatingKey]bool)
	var keys []core.RatingKey
	for _, group := range groups {
		for _, key := range group {
			if !seen[key] {
				seen[key] = true
				keys = append(keys, key)
			}
		}
	}
	return keys
}
