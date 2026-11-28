package personalize

import (
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
)

// RecordNeutralImpression records a validated server-issued row snapshot using
// the existing impression and ranker paths.
func (e *Engine) RecordNeutralImpression(user *core.User, imp core.NeutralImpression) {
	if user == nil || !validNeutralImpression(imp) {
		return
	}
	entry := neutralDelivery(user, imp)
	if entry.at.IsZero() {
		entry.at = e.now()
		entry.hc.Now = entry.at
	}
	entry.rows = []servedRow{neutralServedRow(imp)}
	e.queueImpression(user, entry)

	hub := core.Hub{ID: imp.RowID, Title: imp.Title, Items: make([]core.Title, len(imp.Items))}
	for i, key := range imp.Items {
		hub.Items[i].Key = key
	}
	e.rankerRecordServeNeutral(user, entry.hc, []core.Hub{hub}, entry.at, map[string]string{imp.RowID: imp.Kind}, imp.Rank)
}

// RecordDeliveredRows records one native response that has already passed
// its surface's viewer policy. The response may have come from a page cache.
func (e *Engine) RecordDeliveredRows(user *core.User, rows []core.NeutralImpression) {
	if user == nil || len(rows) == 0 {
		return
	}
	entry := neutralDelivery(user, rows[0])
	entry.at = e.now()
	entry.analyticsOnly = true
	entry.rows = make([]servedRow, 0, len(rows))
	for _, row := range rows {
		if validNeutralImpression(row) && len(row.Items) > 0 {
			entry.rows = append(entry.rows, neutralServedRow(row))
		}
	}
	if len(entry.rows) > 0 {
		e.rec.record(entry)
	}
}

func validNeutralImpression(imp core.NeutralImpression) bool {
	return imp.RowID != "" && imp.Kind != "" && imp.Rank >= 0
}

func neutralDelivery(user *core.User, imp core.NeutralImpression) impression {
	s := imp.Situation
	hc := core.HubContext{Device: s.Device, Daypart: s.Daypart, Weekend: s.Weekend, Now: s.Now, SectionID: s.LibraryID, Product: s.Product, Platform: s.Platform, SessionID: s.SessionID, ClientID: s.ClientID}
	return impression{userID: user.ID, app: recommendationApp(imp.Surface), home: imp.Placement == "home", at: hc.Now, observedAt: imp.ObservedAt, hc: hc}
}

func neutralServedRow(imp core.NeutralImpression) servedRow {
	return servedRow{hubID: imp.RowID, kind: imp.Kind, title: imp.Title, position: imp.Rank,
		skipAnalytics: !recommendationPurpose(imp.Purpose), keys: append([]core.RatingKey(nil), imp.Items...)}
}

var _ core.NeutralImpressionSink = (*Engine)(nil)

// SemanticRowKind returns the registered producer kind for a ranked row. The
// fallback matches the existing impression classification for external rows.
func (e *Engine) SemanticRowKind(id string) string { return e.kindOfHub(id) }

// RecordNeutralEntryImpression records only the card actually reported visible.
// It neither counts another shelf serve nor marks its offscreen peers shown.
func (e *Engine) RecordNeutralEntryImpression(user *core.User, imp core.NeutralImpression, entry core.NeutralEntryRef) {
	if user == nil || !validNeutralImpression(imp) || entry.Kind != "title" {
		return
	}
	index := -1
	for n, ref := range imp.Entries {
		if ref == entry {
			index = n
			break
		}
	}
	if index < 0 {
		return
	}
	key, err := core.ParseRatingKey(entry.ID)
	if err != nil {
		return
	}
	member := false
	for _, item := range imp.Items {
		if item == key {
			member = true
			break
		}
	}
	if !member {
		return
	}
	delivery := neutralDelivery(user, imp)
	if delivery.at.IsZero() {
		delivery.at = e.now()
		delivery.hc.Now = delivery.at
	}
	imp.Items = []core.RatingKey{key}
	row := neutralServedRow(imp)
	row.itemsOnly, row.itemOffset = true, index
	delivery.rows = []servedRow{row}
	e.queueImpression(user, delivery)
}

func recommendationPurpose(purpose core.RowPurpose) bool {
	switch purpose {
	case frontend.RowContinue, frontend.RowNextUp, frontend.RowWatchlist, frontend.RowLiked, frontend.RowRecentContexts, frontend.RowLive:
		return false
	default:
		return true
	}
}
