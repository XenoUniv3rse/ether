package frontend

import (
	"context"
	"strconv"
)

type exportCompactIDsKey struct{}

// WithExportCompactIDs carries IDs read alongside one catalogue batch.
func WithExportCompactIDs(ctx context.Context, ids map[RatingKey]int32) context.Context {
	return context.WithValue(ctx, exportCompactIDsKey{}, ids)
}

// ExportCompactID finds an ID already read with the title.
func ExportCompactID(ctx context.Context, key RatingKey) int32 {
	ids, _ := ctx.Value(exportCompactIDsKey{}).(map[RatingKey]int32)
	return ids[key]
}

// WalkCompactTitles resolves identifiers for a bounded window of streamed
// titles. The source may emit across multiple parents into the same window.
func WalkCompactTitles[T any](ctx context.Context, batchSize int,
	compact func(context.Context, []string) (map[string]int32, error),
	walk func(func(RatingKey, int32, T) error) error, emit func(T, int32) error,
) error {
	items := make([]T, 0, batchSize)
	refs := make([]string, 0, batchSize)
	flush := func() error {
		if len(items) == 0 {
			return nil
		}
		ids, err := compact(ctx, refs)
		if err != nil {
			return err
		}
		for j, item := range items {
			if err := emit(item, ids[refs[j]]); err != nil {
				return err
			}
		}
		clear(items)
		items, refs = items[:0], refs[:0]
		return nil
	}
	if err := walk(func(key RatingKey, id int32, item T) error {
		if id > 0 {
			if err := flush(); err != nil {
				return err
			}
			return emit(item, id)
		}
		items = append(items, item)
		refs = append(refs, strconv.FormatInt(int64(key), 10))
		if len(items) == batchSize {
			return flush()
		}
		return nil
	}); err != nil {
		return err
	}
	return flush()
}
