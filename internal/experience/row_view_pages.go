package experience

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/store"
	"github.com/braedonsaunders/ether/internal/viewerpolicy"
)

// ResolveRowViews shares hydration, current-policy proofs and receipt admission
// across a bounded home page. It never scans titles outside these visits.
func (s *RowViewService) ResolveRowViews(ctx context.Context, reqs []frontend.RowViewPageRequest) ([]frontend.RowViewPage, error) {
	pages, err := s.resolveRowViews(ctx, reqs, true)
	if errors.Is(err, viewerpolicy.ErrChanged) {
		return nil, ErrRowViewStale
	}
	return pages, err
}

func (s *RowViewService) ValidateRowViews(ctx context.Context, reqs []frontend.RowViewPageRequest) error {
	_, err := s.resolveRowViews(ctx, reqs, false)
	return err
}

func (s *RowViewService) resolveRowViews(ctx context.Context, reqs []frontend.RowViewPageRequest, issueReceipts bool) ([]frontend.RowViewPage, error) {
	if len(reqs) == 0 {
		return []frontend.RowViewPage{}, nil
	}
	if len(reqs) > 100 {
		return nil, ErrRowViewNotFound
	}
	first := reqs[0]
	u, err := s.viewer(ctx, first.Viewer, first.Device, first.Protocol)
	if err != nil {
		return nil, err
	}
	ids := make([]any, len(reqs))
	seen := map[string]bool{}
	for n, q := range reqs {
		if !reflect.DeepEqual(q.Viewer, first.Viewer) || q.Device != first.Device || q.Protocol != first.Protocol || q.ID == "" || q.Offset < 0 || q.Limit < 1 || q.Limit > 100 || seen[q.ID] {
			return nil, ErrRowViewNotFound
		}
		seen[q.ID] = true
		ids[n] = normalizeRowViewID(q.ID)
	}
	type record struct {
		snapshot RowViewSnapshot
		expires  int64
	}
	load := func(tx *sql.Tx) (map[string]record, error) {
		args := []any{u.ID, first.Device.ID, string(first.Protocol)}
		args = append(args, ids...)
		query := `SELECT snapshot,expires,payload FROM experience_row_views WHERE viewer_id=? AND device_id=? AND protocol=? AND snapshot IN (` + store.Placeholders(len(ids)) + `)`
		if tx != nil && store.DialectOf(s.o.DB) == store.Postgres {
			query += ` FOR SHARE`
		}
		var rows *sql.Rows
		var e error
		if tx == nil {
			rows, e = s.o.DB.QueryContext(ctx, query, args...)
		} else {
			rows, e = tx.QueryContext(ctx, query, args...)
		}
		if e != nil {
			return nil, e
		}
		defer rows.Close()
		out := make(map[string]record, len(ids))
		for rows.Next() {
			var id, payload string
			var rec record
			if e = rows.Scan(&id, &rec.expires, &payload); e != nil {
				return nil, e
			}
			if rec.expires <= s.o.Now().UnixMilli() || json.Unmarshal([]byte(payload), &rec.snapshot) != nil {
				return nil, ErrRowViewStale
			}
			out[id] = rec
		}
		if e = rows.Err(); e != nil {
			return nil, e
		}
		if len(out) != len(ids) {
			return nil, ErrRowViewStale
		}
		return out, nil
	}
	records, err := load(nil)
	if err != nil {
		return nil, err
	}
	var refs []core.NeutralEntryRef
	spans := make([]int, len(reqs)+1)
	for n, id := range ids {
		refs = append(refs, records[id.(string)].snapshot.Entries...)
		spans[n+1] = len(refs)
	}
	validate := func(active *core.User, current bool) ([]frontend.RowEntry, error) {
		policyCtx := ctx
		var e error
		if s.o.PreparePolicy != nil {
			policyCtx, e = s.o.PreparePolicy(policyCtx, active)
			if e != nil {
				return nil, e
			}
		}
		if current {
			policyCtx = context.WithValue(policyCtx, rowViewPolicyPreflightKey{}, &rowViewPolicyPreflight{})
		}
		if s.o.PrepareSnapshots != nil && len(ids) > 0 {
			snaps := make([]RowViewSnapshot, len(ids))
			for i, id := range ids {
				snaps[i] = records[id.(string)].snapshot
			}
			policyCtx, e = s.o.PrepareSnapshots(policyCtx, active, snaps)
			if e != nil {
				return nil, e
			}
		}
		for _, id := range ids {
			if !s.o.ValidRow(policyCtx, active, first.Device, first.Protocol, records[id.(string)].snapshot) {
				return nil, ErrRowViewStale
			}
		}
		return s.resolveAll(policyCtx, active, first.Device, first.Protocol, refs)
	}
	if !issueReceipts {
		if _, err := validate(u, true); err != nil {
			return nil, err
		}
		_, err := load(nil)
		return nil, err
	}
	resolved, err := validate(u, false)
	if err != nil {
		return nil, err
	}
	pages := make([]frontend.RowViewPage, len(reqs))
	var subjects []ReceiptSubject
	offsets := make([]int, len(reqs))
	for n, q := range reqs {
		rec := records[ids[n].(string)]
		snap := rec.snapshot
		entries := make([]frontend.RowEntry, 0, spans[n+1]-spans[n])
		for _, entry := range resolved[spans[n]:spans[n+1]] {
			allowed := len(q.Kinds) == 0
			for _, kind := range q.Kinds {
				if kind == entry.Kind {
					allowed = true
				}
			}
			if allowed {
				entry.Handle = ""
				entries = append(entries, entry)
			}
		}
		start := min(q.Offset, len(entries))
		end := min(start+q.Limit, len(entries))
		page := frontend.RowViewPage{ExpiresAt: time.UnixMilli(rec.expires), RankedTop10: snap.Row.RankedTop10 || snap.Row.Layout.RankedTop10, Total: len(entries), TotalExact: !snap.Truncated, Truncated: snap.Truncated, More: end < len(entries), Entries: entries[start:end]}
		pages[n] = page
		offsets[n] = len(subjects)
		if len(page.Entries) == 0 {
			continue
		}
		imp := &core.NeutralImpression{RowID: snap.Row.ID, Kind: snap.Row.Kind, Title: snap.Row.Title, Purpose: snap.Row.Purpose, Rank: snap.Row.Rank, Situation: snap.Situation, Collection: snap.Row.Collection, Placement: string(snap.Placement), ViewID: snap.ViewID, Item: snap.Item}
		for _, entry := range page.Entries {
			imp.Entries = append(imp.Entries, core.NeutralEntryRef{Kind: string(entry.Kind), ID: entryIdentity(entry)})
			if entry.Title != nil {
				imp.Items = append(imp.Items, entry.Title.Key)
			}
		}
		subjects = append(subjects, ReceiptSubject{Kind: "row", ID: snap.Row.ID, Purpose: snap.Row.Purpose, Rank: snap.Row.Rank, Impression: imp})
		for _, entry := range page.Entries {
			subjects = append(subjects, ReceiptSubject{Kind: string(entry.Kind), ID: fmt.Sprintf("%s/%s/%s", snap.Row.ID, entry.Kind, entryIdentity(entry)), Purpose: snap.Row.Purpose, Rank: snap.Row.Rank, Impression: imp})
		}
	}
	current := func() bool {
		if _, e := load(nil); e != nil {
			return false
		}
		active, e := s.viewer(ctx, u, first.Device, first.Protocol)
		if e != nil {
			return false
		}
		if _, e = validate(active, true); e != nil {
			return false
		}
		_, e = load(nil)
		return e == nil
	}
	if len(subjects) == 0 {
		if !current() {
			return nil, ErrRowViewStale
		}
		return pages, nil
	}
	expiry := time.Unix(s.o.Receipts.opts.Now().Add(s.o.Receipts.opts.TTL).Unix(), 0)
	// Use exactly the single-page keys, so a wire refresh or later row page
	// keeps its live receipts. The existing 512-page admission bound applies.
	reuse := first.Protocol == "vela" && s.o.Receipts.opts.Generation == nil
	keys := make([][32]byte, len(pages))
	var epoch int64
	var tokens []string
	if reuse {
		epoch, err = viewerpolicy.Read(ctx, s.o.DB)
		if err != nil {
			return nil, err
		}
		cachedTokens := make([]string, 0, len(subjects))
		allCached := true
		for n, q := range reqs {
			if len(pages[n].Entries) == 0 {
				continue
			}
			end := offsets[n] + len(pages[n].Entries) + 1
			b, e := json.Marshal([]any{u, q.Device.ID, q.Protocol, q.ID, q.Offset, q.Limit, q.Kinds, subjects[offsets[n]:end]})
			if e != nil {
				return nil, e
			}
			keys[n] = sha256.Sum256(b)
			s.receiptMu.Lock()
			cached, found := s.pageReceipts[keys[n]]
			s.receiptMu.Unlock()
			if !found || cached.expires <= s.o.Receipts.opts.Now().Unix() || cached.epoch != epoch {
				allCached = false
				continue
			}
			cachedTokens = append(cachedTokens, cached.tokens...)
			if time.Unix(cached.expires, 0).Before(expiry) {
				expiry = time.Unix(cached.expires, 0)
			}
		}
		if allCached {
			if !current() {
				return nil, ErrRowViewStale
			}
			after, e := viewerpolicy.Read(ctx, s.o.DB)
			if e != nil {
				return nil, e
			}
			if after != epoch {
				return nil, ErrRowViewStale
			}
			live, e := s.o.Receipts.receiptsLive(ctx, u.ID, first.Device.ID, first.Protocol, cachedTokens)
			if e != nil {
				return nil, e
			}
			if live {
				tokens = cachedTokens
			}
		}
	}
	if tokens == nil {
		expiry = time.Unix(s.o.Receipts.opts.Now().Add(s.o.Receipts.opts.TTL).Unix(), 0)
		tokens, err = s.o.Receipts.issueBatchChecked(ctx, u, first.Device, first.Protocol, subjects, current, func(tx *sql.Tx) error { _, e := load(tx); return e })
		if errors.Is(err, viewerpolicy.ErrChanged) {
			return nil, ErrRowViewStale
		}
		if err != nil {
			return nil, err
		}
		if reuse {
			s.receiptMu.Lock()
			if s.pageReceipts == nil {
				s.pageReceipts = make(map[[32]byte]rowPageReceipts)
			}
			for key, cached := range s.pageReceipts {
				if cached.expires <= s.o.Receipts.opts.Now().Unix() {
					delete(s.pageReceipts, key)
				}
			}
			for n := range pages {
				if len(pages[n].Entries) == 0 {
					continue
				}
				if _, exists := s.pageReceipts[keys[n]]; !exists && len(s.pageReceipts) >= 512 {
					continue
				}
				end := offsets[n] + len(pages[n].Entries) + 1
				s.pageReceipts[keys[n]] = rowPageReceipts{tokens: append([]string(nil), tokens[offsets[n]:end]...), expires: min(pages[n].ExpiresAt.Unix(), expiry.Unix()), epoch: epoch}
			}
			s.receiptMu.Unlock()
		}
	}

	for n := range pages {
		page := &pages[n]
		if len(page.Entries) == 0 {
			continue
		}
		page.ImpressionHandle = tokens[offsets[n]]
		for j := range page.Entries {
			page.Entries[j].Handle = tokens[offsets[n]+j+1]
		}
		if expiry.Before(page.ExpiresAt) {
			page.ExpiresAt = expiry
		}
	}
	return pages, nil
}
