package plex

import (
	"context"
	"encoding/json"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontends/plex/model"
	"github.com/go-chi/chi/v5"
)

const liveDiscoveryItemsMax = 200
const liveDiscoveryBucketsMax = 32

type liveDiscoveryBucket struct {
	id, title, key string
	items          []frontend.LiveTVProgramme
}
type liveDiscoverySnapshot struct {
	buckets  []liveDiscoveryBucket
	channels map[string]frontend.LiveTVChannel
	genres   map[string]model.StringList
}
type liveDiscoveryEntry struct {
	key      string
	built    time.Time
	expires  time.Time
	snapshot *liveDiscoverySnapshot
}
type liveDiscoveryCache struct {
	mu               sync.Mutex
	closed, building bool
	entries          map[int64]liveDiscoveryEntry
	ready            chan struct{}
	idleTimer        *time.Timer
}

// Discovery is demand-only. A single cancellable worker prepares bounded shelves;
// requests never expand the whole guide or sort the lineup.
// At most eight viewers are retained, refreshed on demand after five minutes.
func (s *Server) liveDiscoverySnapshot(ctx context.Context, viewer *frontend.User) *liveDiscoverySnapshot {
	if !s.liveTVEnabled() || viewer == nil {
		return nil
	}
	id := int64(0)
	if viewer != nil {
		id = viewer.ID
	}
	raw, _ := json.Marshal(viewer)
	key := string(raw) + s.svc().liveTV.LineupChangedFor(viewer).String()
	now := s.now()
	c := &s.liveDiscovery
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil
	}
	if c.idleTimer == nil {
		c.idleTimer = time.AfterFunc(15*time.Minute, func() { c.mu.Lock(); defer c.mu.Unlock(); c.entries = nil })
	} else {
		c.idleTimer.Reset(15 * time.Minute)
	}
	entry := c.entries[id]
	if entry.key == key && now.Before(entry.expires) && !now.Before(entry.built) {
		c.mu.Unlock()
		return entry.snapshot
	}
	if !c.building {
		c.building = true
		c.ready = make(chan struct{})
		var user *frontend.User
		if viewer != nil {
			copy := *viewer
			user = &copy
		}
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			workerCtx, cancel := context.WithTimeout(s.ctx, 2*time.Minute)
			defer cancel()
			snapshot, err := s.buildLiveDiscovery(workerCtx, user)
			c.mu.Lock()
			defer c.mu.Unlock()
			c.building = false
			if !c.closed {
				if c.entries == nil {
					c.entries = map[int64]liveDiscoveryEntry{}
				}
				if len(c.entries) >= 8 { // Evict the oldest viewer, never grow with the user count.
					var oldestID int64
					var oldest time.Time
					for uid, e := range c.entries {
						if oldest.IsZero() || e.built.Before(oldest) {
							oldestID, oldest = uid, e.built
						}
					}
					delete(c.entries, oldestID)
				}
				expires := now.Add(5 * time.Minute)
				if err != nil {
					expires = now.Add(5 * time.Second)
				} else {
					for _, b := range snapshot.buckets {
						for _, p := range b.items {
							for _, boundary := range []time.Time{p.Start, p.End} {
								if boundary.After(now) && boundary.Before(expires) {
									expires = boundary
								}
							}
						}
					}
				}
				c.entries[id] = liveDiscoveryEntry{key: key, built: now, expires: expires, snapshot: snapshot}
			}
			close(c.ready)
		}()
	}
	ready := c.ready
	c.mu.Unlock()
	// A fixed short join keeps small lineups responsive on first use, independent
	// of library size. A large/cold lineup returns Retry-After while it prepares.
	timer := time.NewTimer(25 * time.Millisecond)
	defer timer.Stop()
	select {
	case <-ready:
	case <-timer.C:
	case <-ctx.Done():
	}
	c.mu.Lock()
	entry = c.entries[id]
	c.mu.Unlock()
	if entry.key == key {
		return entry.snapshot
	}
	return nil
}

func (s *Server) buildLiveDiscovery(ctx context.Context, viewer *frontend.User) (*liveDiscoverySnapshot, error) {
	live := s.svc().liveTV
	if live == nil || !live.VisibleTo(ctx, viewer) {
		return nil, frontend.ErrNotAvailable
	}
	cs, err := live.AllowedChannels(ctx, viewer)
	if err != nil {
		return nil, err
	}
	sort.SliceStable(cs, func(i, j int) bool {
		if cs[i].Number != cs[j].Number {
			return compareChannelNum(cs[i].Number, cs[j].Number)
		}
		return cs[i].ID < cs[j].ID
	})
	now := s.now()
	// Tonight is a real evening window in the server's configured local clock;
	// its More link uses exactly the same bucket as the shelf.
	evening := time.Date(now.Year(), now.Month(), now.Day(), 18, 0, 0, 0, now.Location())
	nightEnd := evening.Add(8 * time.Hour)
	if !now.Before(nightEnd) {
		evening = evening.AddDate(0, 0, 1)
		nightEnd = evening.Add(8 * time.Hour)
	}
	out := &liveDiscoverySnapshot{channels: map[string]frontend.LiveTVChannel{}, genres: map[string]model.StringList{}}
	bucketIndex := map[string]int{}
	addBucket := func(id, title, key string) {
		if _, ok := bucketIndex[id]; ok || len(out.buckets) >= liveDiscoveryBucketsMax {
			return
		}
		bucketIndex[id] = len(out.buckets)
		out.buckets = append(out.buckets, liveDiscoveryBucket{id: id, title: title, key: key})
	}
	addBucket("all", "On Now", liveProviderPath+"/watchnow/all")
	for _, f := range liveCategoryGuideFilters() {
		addBucket(f.Identifier, f.Title, liveProviderPath+"/watchnow/categories/"+f.Identifier)
	}
	addBucket("tonight", "On Tonight", liveProviderPath+"/watchnow/tonight")
	// Each batch temporarily holds at most 32 channels' guide. Retained entries
	// and buckets have hard caps; a 10,000-channel guide is never materialized.
	for start := 0; start < len(cs); start += 32 {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		chunk := cs[start:min(start+32, len(cs))]
		ids := make([]string, len(chunk))
		channels := map[string]frontend.LiveTVChannel{}
		for i, c := range chunk {
			ids[i] = c.ID
			channels[c.ID] = c
		}
		ps, e := live.GuideForChannels(ctx, viewer, now, now.Add(26*time.Hour), ids)
		if e != nil {
			return nil, e
		}
		found := map[string]bool{}
		appendItem := func(id string, p frontend.LiveTVProgramme) {
			if at, ok := bucketIndex[id]; ok {
				if id != "all" && id != "tonight" {
					key := "ether.category." + id
					found := false
					for _, existing := range out.genres[p.ChannelID] {
						if existing == key {
							found = true
							break
						}
					}
					if !found {
						out.genres[p.ChannelID] = append(out.genres[p.ChannelID], key)
					}
				}
				out.buckets[at].items = append(out.buckets[at].items, p)
			}
		}
		for _, p := range ps {
			c, ok := channels[p.ChannelID]
			if !ok || !p.End.After(now) {
				continue
			}
			if !p.Start.After(now) {
				appendItem("all", p)
				found[c.ID] = true
			}
			for _, f := range liveCategoryGuideFilters() {
				if matchesLiveCategory(p.Item.Genres, f.Identifier, p.Item.MediaType) {
					appendItem(f.Identifier, p)
				}
			}
			for _, genre := range p.Item.Genres {
				genre = strings.TrimSpace(genre)
				if genre == "" || len(genre) > 256 {
					continue
				}
				canonical := false
				for _, f := range liveCategoryGuideFilters() {
					if frontend.LiveCategory([]string{genre}, f.Identifier, "") {
						canonical = true
						break
					}
				}
				if canonical {
					continue
				}
				id := liveGenreID(genre)
				addBucket(id, genre, liveProviderPath+"/watchnow/categories/"+id)
				appendItem(id, p)
			}
			if p.End.After(evening) && p.Start.Before(nightEnd) {
				appendItem("tonight", p)
			}
		}
		for _, c := range chunk {
			if !found[c.ID] {
				appendItem("all", frontend.LiveTVProgramme{ChannelID: c.ID})
			}
		}
		// Merge each batch into bounded, deterministic shelves, on-air first.
		for i := range out.buckets {
			b := &out.buckets[i]
			sort.SliceStable(b.items, func(i, j int) bool {
				a, z := b.items[i], b.items[j]
				al, zl := a.ID != "" && !a.Start.After(now), z.ID != "" && !z.Start.After(now)
				if al != zl {
					return al
				}
				if !al && a.ID != "" && z.ID != "" && !a.Start.Equal(z.Start) {
					return a.Start.Before(z.Start)
				}
				ac, ok := channels[a.ChannelID]
				if !ok {
					ac = out.channels[a.ChannelID]
				}
				zc, ok := channels[z.ChannelID]
				if !ok {
					zc = out.channels[z.ChannelID]
				}
				if ac.Number != zc.Number {
					return compareChannelNum(ac.Number, zc.Number)
				}
				if a.ChannelID != z.ChannelID {
					return a.ChannelID < z.ChannelID
				}
				return a.Start.Before(z.Start)
			})
			seen := map[string]bool{}
			kept := make([]frontend.LiveTVProgramme, 0, min(len(b.items), liveDiscoveryItemsMax))
			for _, p := range b.items {
				dedup := p.ChannelID
				if b.id != "all" && p.ID != "" {
					dedup = p.ChannelID + "|" + p.Item.Title
				}
				if seen[dedup] {
					continue
				}
				seen[dedup] = true
				kept = append(kept, p)
				if len(kept) == liveDiscoveryItemsMax {
					break
				}
			}
			b.items = kept
		}
		// Keep metadata only for retained programmes, including earlier batches.
		retained := map[string]frontend.LiveTVChannel{}
		for _, b := range out.buckets {
			for _, p := range b.items {
				c, ok := channels[p.ChannelID]
				if !ok {
					c = out.channels[p.ChannelID]
				}
				retained[c.ID] = c
			}
		}
		out.channels = retained
	}
	return out, nil
}

func (s *Server) liveDiscoveryItems(r *http.Request, id string, limit int) ([]frontend.LiveTVProgramme, map[string]frontend.LiveTVChannel, error) {
	snapshot := s.liveDiscoverySnapshot(r.Context(), FromContext(r.Context()).User)
	if snapshot == nil {
		return nil, nil, frontend.ErrNotAvailable
	}
	for _, b := range snapshot.buckets {
		if b.id == id {
			policy, ok := s.svc().liveTV.(frontend.LiveTVDiscoveryPolicy)
			if !ok {
				return nil, nil, frontend.ErrNotAvailable
			}
			// Recheck current policy, including future programme ratings, before every
			// response. Cached viewer data never grants access after a policy edit.
			items, channels, err := policy.FilterDiscovery(r.Context(), FromContext(r.Context()).User, b.items[:min(limit, len(b.items))])
			byID := map[string]frontend.LiveTVChannel{}
			for _, c := range channels {
				byID[c.ID] = c
			}
			return items, byID, err
		}
	}
	return nil, nil, frontend.ErrNotFound
}
func (s *Server) liveDiscoveryToMetadata(r *http.Request, ps []frontend.LiveTVProgramme, cs map[string]frontend.LiveTVChannel) []model.Metadata {
	r = s.withProgrammeGuideLogos(r, ps, cs)
	out := make([]model.Metadata, 0, len(ps))
	for _, p := range ps {
		c := cs[p.ChannelID]
		if p.ID == "" {
			out = append(out, s.liveChannelMetadata(r, c))
			continue
		}
		md := s.liveProgrammeMetadata(c, p)
		s.applyGuideChannelThumb(r, &md, c)
		out = append(out, md)
	}
	return out
}
func (s *Server) liveDiscoveryMetadata(r *http.Request, captured *[]frontend.LiveTVProgramme) ([]model.Metadata, error) {
	id := "all"
	if strings.HasSuffix(r.URL.Path, "/tonight") {
		id = "tonight"
	}
	if raw := chi.URLParam(r, "category"); raw != "" {
		var ok bool
		id, ok = normalizeLiveCategory(raw)
		if !ok {
			return nil, frontend.ErrNotFound
		}
	}
	ps, cs, err := s.liveDiscoveryItems(r, id, liveDiscoveryItemsMax)
	if err != nil {
		return nil, err
	}
	if captured != nil {
		*captured = ps
	}
	return s.liveDiscoveryToMetadata(r, ps, cs), nil
}
