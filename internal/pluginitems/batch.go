package pluginitems

import (
	"context"
	"strings"

	"github.com/braedonsaunders/ether/internal/core"
)

// KeyedItem pairs a stored KindItem with its owning LibraryID.
type KeyedItem struct {
	Item      core.KindItem
	LibraryID core.LibraryID
}

type prefixScanner struct {
	sc     rowScanner
	prefix []any
}

func (p prefixScanner) Scan(dest ...any) error {
	all := make([]any, 0, len(p.prefix)+len(dest))
	all = append(all, p.prefix...)
	all = append(all, dest...)
	return p.sc.Scan(all...)
}

// GetByKeys fetches stored KindItems and their hydrated artwork in bounded SQL
// batches rather than issuing one query per key.
func (s *Service) GetByKeys(ctx context.Context, keys []core.RatingKey) (map[core.RatingKey]KeyedItem, error) {
	out := make(map[core.RatingKey]KeyedItem, len(keys))
	seqs := make([]any, 0, len(keys))
	seen := make(map[int64]bool, len(keys))
	for _, key := range keys {
		if seq := key.PluginSeq(); seq > 0 && !seen[seq] {
			seen[seq] = true
			seqs = append(seqs, seq)
		}
	}
	err := s.readItemsBatch(ctx, "seq", seqs, func(item KeyedItem) {
		out[item.Item.Key] = item
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// GetByRefs reads plugin items and artwork for a page of external references.
func (s *Service) GetByRefs(ctx context.Context, refs []core.ItemRef) (map[core.ItemRef]KeyedItem, error) {
	out := make(map[core.ItemRef]KeyedItem, len(refs))
	values := make([]any, 0, len(refs))
	seen := make(map[core.ItemRef]bool, len(refs))
	for _, ref := range refs {
		if ref != "" && !seen[ref] {
			seen[ref] = true
			values = append(values, string(ref))
		}
	}
	err := s.readItemsBatch(ctx, "item_ref", values, func(item KeyedItem) {
		out[item.Item.Ref] = item
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

func (s *Service) readItemsBatch(ctx context.Context, column string, values []any, accept func(KeyedItem)) error {
	if s == nil || s.db == nil {
		return nil
	}
	const chunkSize = 500
	for start := 0; start < len(values); start += chunkSize {
		chunk := values[start:min(start+chunkSize, len(values))]
		placeholders := make([]string, len(chunk))
		args := make([]any, len(chunk))
		for i, value := range chunk {
			placeholders[i] = "?"
			args[i] = value
		}
		rows, err := s.db.QueryContext(ctx,
			`SELECT `+itemCols+` FROM plugin_items WHERE `+column+` IN (`+strings.Join(placeholders, ",")+`)`,
			args...)
		if err != nil {
			return err
		}
		var items []core.KindItem
		var libs []core.LibraryID
		for rows.Next() {
			it, libID, _, scanErr := scanItemRow(rows)
			if scanErr != nil {
				rows.Close()
				return scanErr
			}
			items = append(items, it)
			libs = append(libs, libID)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return err
		}
		rows.Close()
		if len(items) == 0 {
			continue
		}
		if err := s.hydrateArtSlice(ctx, items); err != nil {
			return err
		}
		for i, it := range items {
			accept(KeyedItem{Item: it, LibraryID: libs[i]})
		}
	}
	return nil
}

// FilterVisibleRefs evaluates lib's visibility policy for refs in one batched
// SQL query per chunk, returning the subset of refs that are NOT hidden.
func (s *Service) FilterVisibleRefs(ctx context.Context, lib core.Library, refs []core.ItemRef) (map[core.ItemRef]bool, error) {
	visible := make(map[core.ItemRef]bool, len(refs))
	if s == nil || s.db == nil || len(refs) == 0 {
		return visible, nil
	}
	policy, err := s.LibraryPolicy(ctx, lib)
	if err != nil {
		return nil, err
	}
	if policy.Passive() {
		for _, r := range refs {
			if r != "" {
				visible[r] = true
			}
		}
		return visible, nil
	}
	q, pArgs, err := s.policySQL(ctx, lib.ID, policy)
	if err != nil {
		return nil, err
	}
	uniq := make([]core.ItemRef, 0, len(refs))
	seen := make(map[core.ItemRef]bool, len(refs))
	for _, r := range refs {
		if r != "" && !seen[r] {
			seen[r] = true
			uniq = append(uniq, r)
		}
	}
	const chunkSize = 500
	for start := 0; start < len(uniq); start += chunkSize {
		chunk := uniq[start:min(start+chunkSize, len(uniq))]
		placeholders := make([]string, len(chunk))
		args := make([]any, 0, 1+len(chunk)+len(pArgs))
		args = append(args, int64(lib.ID))
		for i, r := range chunk {
			placeholders[i] = "?"
			args[i] = string(r)
		}
		args = append(args, pArgs...)
		rows, err := s.db.QueryContext(ctx,
			`SELECT i.item_ref FROM plugin_items i WHERE i.library_id = ? AND i.item_ref IN (`+strings.Join(placeholders, ",")+`) AND NOT (`+q+`)`,
			args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var refStr string
			if err := rows.Scan(&refStr); err != nil {
				rows.Close()
				return nil, err
			}
			visible[core.ItemRef(refStr)] = true
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, err
		}
		rows.Close()
	}
	return visible, nil
}

// FilterVisibleIDs evaluates lib's visibility policy for externalIDs belonging
// to (pluginID, kindKey) in bounded SQL chunks, returning the subset of stored
// external IDs allowed by lib's LibraryPolicy in deduplicated input order.
// Unknown or syntactically invalid IDs are omitted even when the policy is
// passive so behavior stays identical whether or not Rules are configured.
func (s *Service) FilterVisibleIDs(ctx context.Context, pluginID, kindKey string, lib core.Library, externalIDs []string) ([]string, error) {
	if s == nil || s.db == nil || len(externalIDs) == 0 {
		return []string{}, nil
	}
	policy, err := s.LibraryPolicy(ctx, lib)
	if err != nil {
		return nil, err
	}
	var (
		q     string
		pArgs []any
	)
	if !policy.Passive() {
		q, pArgs, err = s.policySQL(ctx, lib.ID, policy)
		if err != nil {
			return nil, err
		}
	}
	uniq := make([]string, 0, len(externalIDs))
	seen := make(map[string]bool, len(externalIDs))
	for _, id := range externalIDs {
		if core.ValidExternalID(id) && !seen[id] {
			seen[id] = true
			uniq = append(uniq, id)
		}
	}
	if len(uniq) == 0 {
		return []string{}, nil
	}
	allowed := make(map[string]bool, len(uniq))
	const chunkSize = 500
	for start := 0; start < len(uniq); start += chunkSize {
		chunk := uniq[start:min(start+chunkSize, len(uniq))]
		placeholders := make([]string, len(chunk))
		args := make([]any, 0, 1+len(chunk)+len(pArgs))
		args = append(args, int64(lib.ID))
		for i, id := range chunk {
			placeholders[i] = "?"
			args = append(args, string(core.NewItemRef(pluginID, kindKey, id)))
		}
		sqlQuery := `SELECT i.external_id FROM plugin_items i WHERE i.library_id = ? AND i.item_ref IN (` + strings.Join(placeholders, ",") + `)`
		if !policy.Passive() {
			sqlQuery += ` AND NOT (` + q + `)`
			args = append(args, pArgs...)
		}
		rows, err := s.db.QueryContext(ctx, sqlQuery, args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var extID string
			if err := rows.Scan(&extID); err != nil {
				rows.Close()
				return nil, err
			}
			allowed[extID] = true
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, err
		}
		rows.Close()
	}
	out := make([]string, 0, len(allowed))
	for _, id := range uniq {
		if allowed[id] {
			out = append(out, id)
		}
	}
	return out, nil
}
