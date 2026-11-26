package personalize

import (
	"context"
	"strings"

	"github.com/braedonsaunders/ether/internal/core"
)

type collectionCardSource interface {
	core.CollectionCardHeaders
	FindCollectionCardKeys(context.Context, string, int) ([]core.RatingKey, error)
}

// CollectionCardHubs resolves only rows requested by the effective Home layout.
// The caller applies the same final placement rules and row budget as other hubs.
func (e *Engine) CollectionCardHubs(ctx context.Context, user *core.User, hc core.HubContext, limit int) []core.Hub {
	source, ok := e.opts.Collections.(collectionCardSource)
	if !ok || user == nil {
		return nil
	}
	if hc.Now.IsZero() {
		hc.Now = e.now().In(e.loc)
	}
	pl := e.planFor(ctx, user.ID, surfHome)
	if !pl.listed[SourceCollectionCards] {
		return nil
	}
	type selection struct {
		rule *rule
		keys []core.RatingKey
	}
	var rows []selection
	var keys []core.RatingKey
	for _, r := range pl.rules {
		if r.Source != SourceCollectionCards || !r.active(user.ID, hc) {
			continue
		}
		var rowKeys []core.RatingKey
		if match := strings.TrimSpace(r.row.Source.Param("match", "")); match != "" {
			var err error
			rowKeys, err = source.FindCollectionCardKeys(ctx, match, core.MaxCollectionCards)
			if err != nil {
				e.log.WarnContext(ctx, "collection card row omitted", "row", r.RowID, "stage", "selection")
				continue
			}
		} else {
			for id := range strings.SplitSeq(r.row.Source.Param("collections", ""), ",") {
				if key, err := core.ParseRatingKey(id); err == nil && key.Kind() == core.KindSmart && !key.IsEphemeral() {
					rowKeys = append(rowKeys, key)
				}
			}
		}
		rows = append(rows, selection{r, rowKeys})
		keys = append(keys, rowKeys...)
	}
	if len(keys) == 0 {
		return nil
	}
	collections, err := source.CollectionCardHeaders(ctx, user, keys)
	if limit <= 0 {
		limit = 12
	}
	limit = min(limit, core.MaxCollectionCards)
	var hubs []core.Hub
	batchFailed := err != nil
	for _, row := range rows {
		headers := collections
		if batchFailed {
			var rowErr error
			headers, rowErr = source.CollectionCardHeaders(ctx, user, row.keys)
			if rowErr != nil {
				e.log.WarnContext(ctx, "collection card row omitted", "row", row.rule.RowID, "stage", "headers")
				continue
			}
		}
		name := strings.TrimSpace(row.rule.Name)
		if name == "" {
			name = e.sourceTitle(SourceCollectionCards)
		}
		hub := core.Hub{ID: "home." + row.rule.RowID, Title: name, Type: "collection", Style: "shelf", Promoted: true}
		seen := map[core.RatingKey]bool{}
		for _, key := range row.keys {
			c := headers[key]
			if c == nil || c.OwnerUserID != 0 || c.ChildCount == 0 || seen[key] {
				continue
			}
			seen[key] = true
			hub.Collections = append(hub.Collections, *c)
		}
		if len(hub.Collections) == 0 {
			continue
		}
		hub.More = len(hub.Collections) > limit
		hub.Collections = hub.Collections[:min(limit, len(hub.Collections))]
		hub.ItemCount = len(hub.Collections)
		e.registerHub(hub.ID, SourceCollectionCards)
		hubs = append(hubs, hub)
	}
	return hubs
}
