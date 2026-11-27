package extras

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/store"
)

func init() {
	store.Register("extras", 1, `
CREATE TABLE extras_lists (
	cache_key   TEXT PRIMARY KEY,
	parent_key  INTEGER NOT NULL,
	payload     TEXT NOT NULL,
	updated_at  INTEGER NOT NULL,
	expires_at  INTEGER NOT NULL
);
CREATE INDEX extras_lists_expires ON extras_lists(expires_at);
CREATE INDEX extras_lists_parent ON extras_lists(parent_key);
CREATE TABLE extras_items (
	extra_key   INTEGER PRIMARY KEY,
	parent_key  INTEGER NOT NULL,
	payload     TEXT NOT NULL,
	expires_at  INTEGER NOT NULL
);
CREATE INDEX extras_items_expires ON extras_items(expires_at);
`)
}

// storedExtra is the durable JSON representation of an Extra list entry.
// It carries clip metadata only — never video bytes or resolved signed URLs.
type storedExtra struct {
	Key         core.RatingKey `json:"k,omitempty"`
	ParentKey   core.RatingKey `json:"pk"`
	ParentTitle string         `json:"pt,omitempty"`
	Title       string         `json:"t"`
	Subtype     string         `json:"st"`
	ExtraType   int            `json:"et"`
	Site        string         `json:"s,omitempty"`
	SiteKey     string         `json:"sk,omitempty"`
	DiscoverKey string         `json:"dk,omitempty"`
	Language    string         `json:"l,omitempty"`
	Official    bool           `json:"o,omitempty"`
	PublishedAt int64          `json:"pa,omitempty"`
	Size        int            `json:"sz,omitempty"`
	DurationMs  int64          `json:"dm,omitempty"`
	Source      string         `json:"src"`
	Identity    string         `json:"id"`
}

func toStoredExtra(e Extra) storedExtra {
	var pub int64
	if !e.PublishedAt.IsZero() {
		pub = e.PublishedAt.Unix()
	}
	return storedExtra{
		Key:         e.Key,
		ParentKey:   e.ParentKey,
		ParentTitle: e.ParentTitle,
		Title:       e.Title,
		Subtype:     e.Subtype,
		ExtraType:   e.ExtraType,
		Site:        e.Site,
		SiteKey:     e.SiteKey,
		DiscoverKey: e.DiscoverKey,
		Language:    e.Language,
		Official:    e.Official,
		PublishedAt: pub,
		Size:        e.Size,
		DurationMs:  e.Duration.Milliseconds(),
		Source:      e.Source,
		Identity:    e.identity,
	}
}

func (se storedExtra) toExtra() Extra {
	var pub time.Time
	if se.PublishedAt > 0 {
		pub = time.Unix(se.PublishedAt, 0).UTC()
	}
	id := se.Identity
	if id == "" {
		if se.Source == SourcePlexDiscover && se.DiscoverKey != "" {
			id = fmt.Sprintf("%d|d|%s", int64(se.ParentKey), se.DiscoverKey)
		} else {
			id = fmt.Sprintf("%d|c|%s|%s", int64(se.ParentKey), strings.ToLower(se.Site), se.SiteKey)
		}
	}
	return Extra{
		Key:         se.Key,
		ParentKey:   se.ParentKey,
		ParentTitle: se.ParentTitle,
		Title:       se.Title,
		Subtype:     se.Subtype,
		ExtraType:   se.ExtraType,
		Site:        se.Site,
		SiteKey:     se.SiteKey,
		DiscoverKey: se.DiscoverKey,
		Language:    se.Language,
		Official:    se.Official,
		PublishedAt: pub,
		Size:        se.Size,
		Duration:    time.Duration(se.DurationMs) * time.Millisecond,
		Source:      se.Source,
		identity:    id,
	}
}

func (s *Service) loadListDB(ctx context.Context, cacheKey string) ([]Extra, time.Time, bool) {
	if s.db == nil {
		return nil, time.Time{}, false
	}
	var payload string
	var expiresAt int64
	err := s.db.QueryRowContext(ctx, `SELECT payload, expires_at FROM extras_lists WHERE cache_key=?`, cacheKey).Scan(&payload, &expiresAt)
	if err != nil {
		return nil, time.Time{}, false
	}
	expires := time.Unix(expiresAt, 0).UTC()
	if !s.now().Before(expires) {
		return nil, time.Time{}, false
	}
	var raw []storedExtra
	if err := json.Unmarshal([]byte(payload), &raw); err != nil {
		return nil, time.Time{}, false
	}
	out := make([]Extra, 0, len(raw))
	for _, item := range raw {
		out = append(out, item.toExtra())
	}
	return out, expires, true
}

func (s *Service) saveListDB(ctx context.Context, cacheKey string, parentKey core.RatingKey, items []Extra, now, expires time.Time) {
	if s.db == nil {
		return
	}
	stored := make([]storedExtra, 0, len(items))
	for _, item := range items {
		stored = append(stored, toStoredExtra(item))
	}
	payload, err := json.Marshal(stored)
	if err != nil {
		return
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO extras_lists(cache_key, parent_key, payload, updated_at, expires_at) VALUES(?,?,?,?,?)
		 ON CONFLICT(cache_key) DO UPDATE SET parent_key=excluded.parent_key, payload=excluded.payload, updated_at=excluded.updated_at, expires_at=excluded.expires_at`,
		cacheKey, int64(parentKey), string(payload), now.Unix(), expires.Unix(),
	); err != nil {
		return
	}
	for _, item := range stored {
		if item.Key == 0 || item.Source == SourceClientYouTube {
			continue
		}
		itemJSON, err := json.Marshal(item)
		if err != nil {
			continue
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO extras_items(extra_key, parent_key, payload, expires_at) VALUES(?,?,?,?)
			 ON CONFLICT(extra_key) DO UPDATE SET parent_key=excluded.parent_key, payload=excluded.payload, expires_at=excluded.expires_at`,
			int64(item.Key), int64(item.ParentKey), string(itemJSON), expires.Unix(),
		); err != nil {
			return
		}
	}
	_ = tx.Commit()
}

func (s *Service) lookupItemDB(key core.RatingKey) (Extra, bool) {
	if s.db == nil || key == 0 {
		return Extra{}, false
	}
	ctx, cancel := context.WithTimeout(s.themeCtx, 2*time.Second)
	defer cancel()
	var payload string
	if err := s.db.QueryRowContext(ctx, `SELECT payload FROM extras_items WHERE extra_key=?`, int64(key)).Scan(&payload); err != nil {
		return Extra{}, false
	}
	var item storedExtra
	if err := json.Unmarshal([]byte(payload), &item); err != nil {
		return Extra{}, false
	}
	e := item.toExtra()
	e.Key = key
	return e, true
}

// Prune removes expired persisted extras list and item metadata rows.
func (s *Service) Prune(ctx context.Context) (int64, error) {
	if s == nil || s.db == nil {
		return 0, nil
	}
	cutoff := s.now().Unix()
	var total int64
	for _, q := range []string{
		`DELETE FROM extras_lists WHERE expires_at <= ?`,
		`DELETE FROM extras_items WHERE expires_at <= ?`,
	} {
		res, err := s.db.ExecContext(ctx, q, cutoff)
		if err != nil {
			return total, err
		}
		if n, err := res.RowsAffected(); err == nil {
			total += n
		}
	}
	return total, nil
}
