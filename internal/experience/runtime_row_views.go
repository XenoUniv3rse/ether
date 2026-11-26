package experience

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"slices"
	"strconv"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
)

// RuntimeRowViews binds persistence, snapshot construction and current policy
// revalidation inside Experience. The app supplies source capabilities only.
type RuntimeRowViews struct {
	DB                  *sql.DB
	PendingWatch        func(int64) uint64
	CurrentAvailable    func(context.Context, []core.Title) []core.Title
	Protocol            core.Protocol
	Receipts            *Receipts
	Visibility          Visibility
	Libraries           []Library
	LiveLibraries       func() []Library
	Active              func(context.Context, int64) (*core.User, error)
	CachedGet           func(context.Context, core.RatingKey) (*core.Title, error)
	CachedGetMany       func(context.Context, []core.RatingKey) (map[core.RatingKey]core.Title, error)
	CachedVirtualSeason func(context.Context, *core.User, core.RatingKey) (*core.Title, error)
	Collections         core.Collections
	Playlists           core.Playlists
	Live                SnapshotLiveSource
	Situation           func(frontend.RowRequest) core.HomeSituation
	Snapshot            func(context.Context, frontend.RowRequest, core.HomeSituation) ([]frontend.Row, error)
}

// rowViewRevision reads watch, preference and layout versions, so snapshots
// created by another node fail closed after a relevant change.
func (d RuntimeRowViews) rowViewRevision(ctx context.Context, user *core.User) (string, error) {
	if proof := bootstrapProofFor(ctx, user.ID); proof != nil {
		proof.mu.Lock()
		defer proof.mu.Unlock()
		if proof.revision != "" {
			return proof.revision, nil
		}
		rev, err := d.readRowViewRevision(ctx, user)
		if err == nil {
			proof.revision = rev
		}
		return rev, err
	}
	return d.readRowViewRevision(ctx, user)
}
func (d RuntimeRowViews) readRowViewRevision(ctx context.Context, user *core.User) (string, error) {
	var layout string
	var favorites, exclusions, rules int64
	var watch WatchRevision
	if d.PendingWatch != nil {
		watch.Pending = d.PendingWatch(user.ID)
	}
	err := d.DB.QueryRowContext(ctx, `SELECT `+watchRevisionSQL+`, COALESCE((SELECT updated_at FROM favorites_mirror WHERE viewer_id=?),0), COALESCE((SELECT revision FROM notinterested_revisions WHERE viewer_id=?),0), COALESCE((SELECT revision FROM catalog_viewer_rules WHERE viewer_id=?),0)`, user.ID, user.ID, user.ID, user.ID).Scan(&watch.Persisted, &favorites, &exclusions, &rules)
	if err != nil {
		return "", err
	}
	// Saved Recommendations pages (one row per page, a handful at most).
	rows, err := d.DB.QueryContext(ctx, `SELECT page, revision FROM personalize_pages ORDER BY page`)
	if err != nil {
		return "", err
	}
	for rows.Next() {
		var page string
		var revision int64
		if err := rows.Scan(&page, &revision); err != nil {
			rows.Close()
			return "", err
		}
		layout += page + ":" + strconv.FormatInt(revision, 10) + ","
	}
	if err := rows.Close(); err != nil {
		return "", err
	}
	if err := rows.Err(); err != nil {
		return "", err
	}
	b, err := json.Marshal([]any{user.ID, user.IsOwner, user.MaxContentRating, user.AllowedLibraries, layout, favorites, exclusions, rules, watch})
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}

type rowRevisionKey struct{}
type rowRevisionProof struct {
	viewer   *core.User
	revision string
}

type rowCardHeadersKey struct{}

type rowCollectionBatchKey struct{}
type rowCollectionBatchProof struct {
	viewer  *core.User
	cols    map[core.RatingKey]*core.Collection
	matched map[core.RatingKey]bool
	want    map[core.RatingKey][]core.RatingKey
}

// RowViewCollectionBatchChecker is an optional extension on core.Collections
// that validates visibility and membership for multiple collection shelves in
// one policy pass (bounded queries regardless of shelf count).
type RowViewCollectionBatchChecker interface {
	CheckMembersBatch(context.Context, *core.User, []core.RatingKey, map[core.RatingKey][]core.RatingKey, int) (map[core.RatingKey]*core.Collection, map[core.RatingKey]bool, error)
}

func NewRuntimeRowViews(d RuntimeRowViews) (*RowViewService, error) {
	protocol, receipts, visibility := d.Protocol, d.Receipts, d.Visibility
	currentLibraries := func() []Library {
		if d.LiveLibraries != nil {
			return d.LiveLibraries()
		}
		return d.Libraries
	}
	// A row snapshot is already drawn from the local catalogue. Revalidation
	// must never trigger a per-title network refresh while a client pages it.
	cacheVisibility := visibility
	readTitle := func(ctx context.Context, key core.RatingKey) (*core.Title, error) {
		if d.CachedGetMany == nil {
			return d.CachedGet(ctx, key)
		}
		titles, err := d.CachedGetMany(ctx, []core.RatingKey{key})
		if err != nil {
			return nil, err
		}
		title, ok := titles[key]
		if !ok {
			return nil, core.ErrNotFound
		}
		return &title, nil
	}
	cacheVisibility.GetTitle = readTitle
	getCached := func(ctx context.Context, user *core.User, key core.RatingKey) (*core.Title, error) {
		title, err := readTitle(ctx, key)
		if errors.Is(err, core.ErrNotFound) && d.CachedVirtualSeason != nil && key.Kind() == core.KindSeason {
			return d.CachedVirtualSeason(ctx, user, key)
		}
		return title, err
	}
	options := RowViewOptions{
		DB: d.DB, TTL: 15 * time.Minute, Active: d.Active, Receipts: receipts,
		PreparePolicy: func(ctx context.Context, u *core.User) (context.Context, error) {
			revision, err := d.rowViewRevision(ctx, u)
			return context.WithValue(ctx, rowRevisionKey{}, rowRevisionProof{u, revision}), err
		},
		PrepareSnapshots: func(ctx context.Context, u *core.User, snaps []RowViewSnapshot) (context.Context, error) {
			checker, ok := d.Collections.(RowViewCollectionBatchChecker)
			if !ok {
				return ctx, nil
			}
			var keys []core.RatingKey
			want := map[core.RatingKey][]core.RatingKey{}
			for _, snap := range snaps {
				if snap.Row.Collection == 0 {
					continue
				}
				if _, seen := want[snap.Row.Collection]; seen {
					continue
				}
				w, valid := rowViewMemberKeys(snap.Entries)
				if !valid {
					continue
				}
				keys = append(keys, snap.Row.Collection)
				want[snap.Row.Collection] = w
			}
			if len(keys) == 0 {
				return ctx, nil
			}
			cols, matched, err := checker.CheckMembersBatch(ctx, u, keys, want, rowViewMemberScanLimit)
			if err != nil {
				return nil, err
			}
			return context.WithValue(ctx, rowCollectionBatchKey{}, rowCollectionBatchProof{
				viewer:  u,
				cols:    cols,
				matched: matched,
				want:    want,
			}), nil
		},
		Build: func(ctx context.Context, request frontend.RowViewRequest) ([]RowViewSnapshot, error) {
			if request.Protocol != protocol {
				return nil, core.ErrUnauthorized
			}
			before, err := d.rowViewRevision(ctx, request.Viewer)
			if err != nil {
				return nil, err
			}
			fresh, err := bootstrapActive(ctx, d.Active, request.Viewer.ID)
			if err != nil || fresh == nil {
				return nil, ErrRowViewStale
			}
			request.Viewer = fresh
			r := frontend.RowRequest{Viewer: request.Viewer, Device: request.Device, Placement: request.Placement, ViewID: request.ViewID, Item: request.Item, Limit: 50}
			situation := d.Situation(r)
			result, err := d.Snapshot(ctx, r, situation)
			if err != nil {
				return nil, err
			}
			after, err := d.rowViewRevision(ctx, request.Viewer)
			if err != nil {
				return nil, err
			}
			if before != after {
				return nil, ErrRowViewStale
			}
			out := make([]RowViewSnapshot, 0, len(result))
			for _, row := range result {
				s := RowViewSnapshot{Row: row, Situation: situation, Placement: request.Placement, ViewID: request.ViewID, Item: request.Item, Revision: after, Truncated: row.More || len(row.Entries) >= 50}
				for _, entry := range row.Entries {
					s.Entries = append(s.Entries, core.NeutralEntryRef{Kind: string(entry.Kind), ID: RowEntryIdentity(entry)})
				}
				out = append(out, s)
			}
			return out, nil
		},
		ValidRow: func(ctx context.Context, u *core.User, _ core.Device, p core.Protocol, s RowViewSnapshot) bool {
			if p != protocol {
				return false
			}
			proof, shared := ctx.Value(rowRevisionKey{}).(rowRevisionProof)
			var revision string
			var err error
			if shared && proof.viewer == u {
				revision = proof.revision
			} else {
				revision, err = d.rowViewRevision(ctx, u)
			}
			if err != nil || revision != s.Revision {
				return false
			}
			switch s.Placement {
			case frontend.PlacementHome:
			case frontend.PlacementLibrary:
				id, err := strconv.Atoi(s.ViewID)
				valid := false
				for _, lib := range currentLibraries() {
					if lib.ID == id {
						valid = true
						break
					}
				}
				if err != nil || !valid || !SectionAllowed(u, id) {
					return false
				}
			case frontend.PlacementDetail:
				if s.Item == 0 {
					return false
				}
				title, err := getCached(ctx, u, s.Item)
				if err != nil {
					return false
				}
				if preflight, ok := ctx.Value(rowViewPolicyPreflightKey{}).(*rowViewPolicyPreflight); ok && d.CurrentAvailable != nil {
					if !cacheVisibility.Playable(ctx, u, title) {
						return false
					}
					preflight.titles = append(preflight.titles, *title)
				} else if !cacheVisibility.Visible(ctx, u, title) {
					return false
				}
			default:
				return false
			}
			if s.Row.Collection != 0 {
				if d.Collections == nil {
					return false
				}
				if checker, ok := d.Collections.(RowViewCollectionChecker); ok {
					want, valid := rowViewMemberKeys(s.Entries)
					if !valid {
						return false
					}
					if batch, ok := ctx.Value(rowCollectionBatchKey{}).(rowCollectionBatchProof); ok && batch.viewer == u {
						if preWant, checked := batch.want[s.Row.Collection]; checked && slices.Equal(preWant, want) {
							c := batch.cols[s.Row.Collection]
							if c == nil || !batch.matched[s.Row.Collection] || !visibility.CollectionAllowed(u, c) {
								return false
							}
							return true
						}
					}
					c, matched, err := checker.CheckMembers(ctx, u, s.Row.Collection, want, rowViewMemberScanLimit)
					if err != nil || c == nil || !matched || !visibility.CollectionAllowed(u, c) {
						return false
					}
				} else {
					c, err := d.Collections.Get(ctx, u, s.Row.Collection)
					if err != nil || c == nil || !visibility.CollectionAllowed(u, c) {
						return false
					}
					if !RowViewCollectionMembers(ctx, d.Collections, u, s.Row.Collection, s.Entries) {
						return false
					}
				}
			}
			return true
		},
		ResolveEntry: func(ctx context.Context, u *core.User, _ core.Device, p core.Protocol, ref core.NeutralEntryRef) (frontend.RowEntry, error) {
			if p != protocol {
				return frontend.RowEntry{}, core.ErrNotFound
			}
			switch frontend.RowEntryKind(ref.Kind) {
			case frontend.RowEntryTitle:
				key, err := core.ParseRatingKey(ref.ID)
				if err != nil {
					return frontend.RowEntry{}, core.ErrNotFound
				}
				t, err := getCached(ctx, u, key)
				if err != nil || t == nil || t.Key != key || !cacheVisibility.Visible(ctx, u, t) {
					return frontend.RowEntry{}, core.ErrNotFound
				}
				return frontend.RowEntry{Kind: frontend.RowEntryTitle, Title: t}, nil
			case frontend.RowEntryCollection:
				key, err := core.ParseRatingKey(ref.ID)
				if err != nil || d.Collections == nil {
					return frontend.RowEntry{}, core.ErrNotFound
				}
				var c *core.Collection
				if checker, ok := d.Collections.(collectionCardHeaders); ok {
					if cards, ok := ctx.Value(rowCardHeadersKey{}).(map[core.RatingKey]*core.Collection); ok {
						c = cards[key]
					} else {
						c = readCollectionCard(ctx, checker, u, key)
					}
				} else {
					c, err = d.Collections.Get(ctx, u, key)
				}
				if err != nil || c == nil || c.Key != key || !visibility.CollectionAllowed(u, c) {
					return frontend.RowEntry{}, core.ErrNotFound
				}
				return frontend.RowEntry{Kind: frontend.RowEntryCollection, Collection: c}, nil
			case frontend.RowEntryPlaylist:
				key, err := core.ParseRatingKey(ref.ID)
				if err != nil || d.Playlists == nil {
					return frontend.RowEntry{}, core.ErrNotFound
				}
				var pl *core.Playlist
				if hdr, ok := d.Playlists.(playlistHeaderSource); ok {
					pl, err = hdr.GetHeader(ctx, u.ID, key)
				} else {
					pl, err = d.Playlists.Get(ctx, u.ID, key)
				}
				if err != nil || pl == nil || pl.UserID != u.ID || pl.Key != key {
					return frontend.RowEntry{}, core.ErrNotFound
				}
				copy := *pl
				copy.Items = nil
				return frontend.RowEntry{Kind: frontend.RowEntryPlaylist, Playlist: &copy}, nil
			case frontend.RowEntryLiveChannel:
				if d.Live == nil {
					return frontend.RowEntry{}, core.ErrNotFound
				}
				entries, err := snapshotLiveEntries(ctx, u, d.Live, []core.NeutralEntryRef{ref})
				if err != nil {
					if errors.Is(err, core.ErrPreparing) || errors.Is(err, core.ErrNotAvailable) {
						return frontend.RowEntry{}, core.ErrNotFound
					}
					return frontend.RowEntry{}, err
				}
				for _, entry := range entries {
					if entry.LiveChannel != nil && entry.LiveChannel.ID == ref.ID {
						return entry, nil
					}
				}
			}
			return frontend.RowEntry{}, core.ErrNotFound
		},
	}
	options.ResolveEntries = func(ctx context.Context, u *core.User, device core.Device, p core.Protocol, refs []core.NeutralEntryRef) ([]frontend.RowEntry, error) {
		if p != protocol {
			return nil, core.ErrNotFound
		}
		if checker, ok := d.Collections.(collectionCardHeaders); ok {
			var keys []core.RatingKey
			for _, ref := range refs {
				if ref.Kind == string(frontend.RowEntryCollection) {
					if key, err := core.ParseRatingKey(ref.ID); err == nil {
						keys = append(keys, key)
					}
				}
			}
			if len(keys) > 0 {
				cards, err := checker.CollectionCardHeaders(ctx, u, keys)
				if err != nil {
					return nil, err
				}
				ctx = context.WithValue(ctx, rowCardHeadersKey{}, cards)
			}
		}
		resolutionVisibility := cacheVisibility
		preflight, current := ctx.Value(rowViewPolicyPreflightKey{}).(*rowViewPolicyPreflight)
		if current && d.CurrentAvailable != nil {
			resolutionVisibility.FilterAvailable = nil
		}
		// Batch playlist header lookups for this single policy pass so a shelf
		// of playlists reads state_playlists once without loading playlist items.
		resolveOne := options.ResolveEntry
		if batchPl, ok := d.Playlists.(playlistHeadersBatchSource); ok {
			var plKeys []core.RatingKey
			for _, ref := range refs {
				if ref.Kind == string(frontend.RowEntryPlaylist) {
					if k, err := core.ParseRatingKey(ref.ID); err == nil {
						plKeys = append(plKeys, k)
					}
				}
			}
			if len(plKeys) > 1 {
				if headers, err := batchPl.GetHeaders(ctx, u.ID, plKeys); err == nil {
					resolveOne = func(ctx context.Context, u *core.User, device core.Device, p core.Protocol, ref core.NeutralEntryRef) (frontend.RowEntry, error) {
						if ref.Kind == string(frontend.RowEntryPlaylist) {
							key, err := core.ParseRatingKey(ref.ID)
							if err != nil {
								return frontend.RowEntry{}, core.ErrNotFound
							}
							pl, found := headers[key]
							if !found || pl.UserID != u.ID || pl.Key != key {
								return frontend.RowEntry{}, core.ErrNotFound
							}
							pl.Items = nil
							return frontend.RowEntry{Kind: frontend.RowEntryPlaylist, Playlist: &pl}, nil
						}
						return options.ResolveEntry(ctx, u, device, p, ref)
					}
				}
			}
		}
		entries, err := RowViewResolveBatchTitles(ctx, u, device, p, refs, resolveOne, d.CachedGetMany, getCached, resolutionVisibility, func(ctx context.Context, viewer *core.User) ([]frontend.RowEntry, error) {
			if d.Live == nil {
				return nil, core.ErrNotFound
			}
			live, err := snapshotLiveEntries(ctx, viewer, d.Live, refs)
			if errors.Is(err, core.ErrPreparing) || errors.Is(err, core.ErrNotAvailable) {
				return nil, nil
			}
			return live, err
		})
		if err != nil {
			return nil, err
		}
		if current && d.CurrentAvailable != nil {
			titles := append([]core.Title(nil), preflight.titles...)
			for _, entry := range entries {
				if entry.Title != nil {
					titles = append(titles, *entry.Title)
				}
			}
			allowed := map[core.RatingKey]bool{}
			freshVisibility := cacheVisibility
			freshVisibility.FilterAvailable = d.CurrentAvailable
			for _, title := range freshVisibility.Available(ctx, u, titles) {
				allowed[title.Key] = true
			}
			for _, title := range titles {
				if !allowed[title.Key] {
					return nil, ErrRowViewStale
				}
			}
		}
		return entries, nil
	}
	return NewRowViewService(options)
}

type playlistHeaderSource interface {
	GetHeader(context.Context, int64, core.RatingKey) (*core.Playlist, error)
}

type playlistHeadersBatchSource interface {
	GetHeaders(context.Context, int64, []core.RatingKey) (map[core.RatingKey]core.Playlist, error)
}

type RowViewCollectionSource interface {
	Items(context.Context, *core.User, core.RatingKey, int, int) (core.Page, error)
}

// RowViewCollectionChecker is an optional fast path on collection sources that
// checks visibility and key membership without hydrating full item titles
// (credits, keywords, aired counts).
type RowViewCollectionChecker interface {
	CheckMembers(context.Context, *core.User, core.RatingKey, []core.RatingKey, int) (*core.Collection, bool, error)
}

func rowViewMemberKeys(refs []core.NeutralEntryRef) ([]core.RatingKey, bool) {
	if len(refs) == 0 {
		return nil, false
	}
	want := make([]core.RatingKey, 0, len(refs))
	seen := make(map[core.RatingKey]bool, len(refs))
	for _, ref := range refs {
		if ref.Kind != string(frontend.RowEntryTitle) {
			return nil, false
		}
		key, err := core.ParseRatingKey(ref.ID)
		if err != nil {
			return nil, false
		}
		if !seen[key] {
			seen[key] = true
			want = append(want, key)
		}
	}
	return want, len(want) > 0
}

// RowViewCollectionMembers adapts one call's collection source into neutral
// member keys. A nil source stays a nil port and fails closed.
func RowViewCollectionMembers(ctx context.Context, source RowViewCollectionSource, user *core.User, collection core.RatingKey, refs []core.NeutralEntryRef) bool {
	if source == nil || user == nil {
		return false
	}
	if checker, ok := source.(RowViewCollectionChecker); ok {
		want, valid := rowViewMemberKeys(refs)
		if !valid {
			return false
		}
		_, matched, err := checker.CheckMembers(ctx, user, collection, want, rowViewMemberScanLimit)
		return err == nil && matched
	}
	return RowViewMembers(ctx, func(ctx context.Context, viewer *core.User, key core.RatingKey, offset, limit int) (RowViewCollectionPage, error) {
		page, err := source.Items(ctx, viewer, key, offset, limit)
		if err != nil {
			return RowViewCollectionPage{}, err
		}
		keys := make([]core.RatingKey, len(page.Items))
		for i := range page.Items {
			keys[i] = page.Items[i].Key
		}
		return RowViewCollectionPage{Keys: keys, Total: page.Total}, nil
	}, user, collection, refs)
}

// RowViewResolveBatchTitles binds this call's viewer, device and protocol into
// the neutral title hydrator. Nothing in the adapter is reused for a later pass.
func RowViewResolveBatchTitles(ctx context.Context, viewer *core.User, device core.Device, protocol core.Protocol, refs []core.NeutralEntryRef, one func(context.Context, *core.User, core.Device, core.Protocol, core.NeutralEntryRef) (frontend.RowEntry, error), batch func(context.Context, []core.RatingKey) (map[core.RatingKey]core.Title, error), get func(context.Context, *core.User, core.RatingKey) (*core.Title, error), visibility Visibility, live func(context.Context, *core.User) ([]frontend.RowEntry, error)) ([]frontend.RowEntry, error) {
	ctx = core.WithCatalogViewer(ctx, viewer)
	return RowViewHydrateEntries(ctx, viewer, refs, func(ctx context.Context, ref core.NeutralEntryRef) (frontend.RowEntry, error) {
		return one(ctx, viewer, device, protocol, ref)
	}, batch, func(ctx context.Context, key core.RatingKey) (*core.Title, error) {
		return get(ctx, viewer, key)
	}, visibility, func(ctx context.Context) ([]frontend.RowEntry, error) {
		return live(ctx, viewer)
	})
}
