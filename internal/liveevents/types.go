package liveevents

import (
	"context"
	"errors"
	"sort"
	"strings"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/plugins"
)

var (
	// ErrNotFound reports an unknown feed or live event.
	ErrNotFound = errors.New("liveevents: not found")
	// ErrUnavailable reports that live events or the backing plugin feed is unavailable.
	ErrUnavailable = errors.New("liveevents: unavailable")
	// ErrInvalidRequest reports a malformed event ID, feed reference, or query.
	ErrInvalidRequest = errors.New("liveevents: invalid request")
	// ErrTooManySubscribers reports that the hub's subscriber cap has been reached.
	ErrTooManySubscribers = errors.New("liveevents: too many subscribers")
	// ErrRateLimited reports that the backing plugin returned HTTP 429 or hit host rate limits.
	ErrRateLimited = errors.New("liveevents: rate limited")
)

// FeedRef uniquely identifies one feed contributed by one plugin.
type FeedRef struct {
	PluginID string
	FeedID   string
}

// String returns the canonical "pluginID:feedID" key.
func (r FeedRef) String() string {
	if r.PluginID == "" || r.FeedID == "" {
		return ""
	}
	return r.PluginID + ":" + r.FeedID
}

// ParseFeedRef splits a canonical "pluginID:feedID" key.
func ParseFeedRef(key string) (FeedRef, bool) {
	p, f, ok := strings.Cut(strings.TrimSpace(key), ":")
	if !ok || p == "" || f == "" || strings.Contains(f, ":") {
		return FeedRef{}, false
	}
	return FeedRef{PluginID: p, FeedID: f}, true
}

// FeedInfo describes one active live event feed contributed by an enabled plugin.
type FeedInfo struct {
	PluginID       string
	PluginName     string
	FeedID         string
	Title          string
	MinIntervalSec int
	EntityKinds    []string
}

// Ref returns the FeedRef for this feed.
func (f FeedInfo) Ref() FeedRef {
	return FeedRef{PluginID: f.PluginID, FeedID: f.FeedID}
}

// Key returns the canonical "pluginID:feedID" key.
func (f FeedInfo) Key() string {
	return f.Ref().String()
}

// EventQuery scopes a plugin GET /ether/v1/live/{feed}/events call.
type EventQuery struct {
	From   string
	To     string
	League string
	// IDs are plugin-local event IDs (not namespaced).
	IDs []string
}

func (q EventQuery) canonicalKey(pluginID, feedID string) string {
	var b strings.Builder
	b.WriteString(pluginID)
	b.WriteByte('\x00')
	b.WriteString(feedID)
	b.WriteByte('\x00')
	b.WriteString(strings.TrimSpace(q.From))
	b.WriteByte('\x00')
	b.WriteString(strings.TrimSpace(q.To))
	b.WriteByte('\x00')
	b.WriteString(strings.TrimSpace(q.League))
	b.WriteByte('\x00')
	if len(q.IDs) > 0 {
		ids := make([]string, 0, len(q.IDs))
		for _, id := range q.IDs {
			id = strings.TrimSpace(id)
			if id != "" {
				ids = append(ids, id)
			}
		}
		sort.Strings(ids)
		prev := ""
		first := true
		for _, id := range ids {
			if id == prev {
				continue
			}
			prev = id
			if !first {
				b.WriteByte(',')
			}
			first = false
			b.WriteString(id)
		}
	}
	return b.String()
}

func (q EventQuery) queryParams() map[string]string {
	params := map[string]string{}
	if v := strings.TrimSpace(q.From); v != "" {
		params["from"] = v
	}
	if v := strings.TrimSpace(q.To); v != "" {
		params["to"] = v
	}
	if v := strings.TrimSpace(q.League); v != "" {
		params["league"] = v
	}
	if len(q.IDs) > 0 {
		ids := make([]string, 0, len(q.IDs))
		seen := make(map[string]struct{}, len(q.IDs))
		for _, id := range q.IDs {
			id = strings.TrimSpace(id)
			if id == "" {
				continue
			}
			if _, ok := seen[id]; ok {
				continue
			}
			seen[id] = struct{}{}
			ids = append(ids, id)
		}
		sort.Strings(ids)
		if len(ids) > 0 {
			params["ids"] = strings.Join(ids, ",")
		}
	}
	if len(params) == 0 {
		return nil
	}
	return params
}

// PluginSource is the minimal plugin host surface required by Hub.
// Production wires NewManagerSource(pluginsMgr); tests inject a fake.
type PluginSource interface {
	ActiveLiveFeeds(ctx context.Context) []FeedInfo
	Call(ctx context.Context, pluginID string, req plugins.Request) (plugins.Response, error)
}

type managerSource struct {
	mgr *plugins.Manager
}

// NewManagerSource adapts *plugins.Manager to PluginSource.
func NewManagerSource(mgr *plugins.Manager) PluginSource {
	if mgr == nil {
		return nil
	}
	return managerSource{mgr: mgr}
}

func (s managerSource) ActiveLiveFeeds(ctx context.Context) []FeedInfo {
	if s.mgr == nil {
		return nil
	}
	active := s.mgr.ActiveLiveFeeds(ctx)
	if len(active) == 0 {
		return nil
	}
	out := make([]FeedInfo, 0, len(active))
	for _, f := range active {
		out = append(out, FeedInfo{
			PluginID:       f.PluginID,
			PluginName:     f.PluginName,
			FeedID:         f.FeedID,
			Title:          f.Title,
			MinIntervalSec: f.MinIntervalSec,
			EntityKinds:    append([]string(nil), f.EntityKinds...),
		})
	}
	return out
}

func (s managerSource) Call(ctx context.Context, pluginID string, req plugins.Request) (plugins.Response, error) {
	if s.mgr == nil {
		return plugins.Response{}, ErrUnavailable
	}
	return s.mgr.Call(ctx, pluginID, req)
}

// WatchEnricher attaches viewer-scoped watch targets (A5) to live events
// before they are returned to a viewer. Implementations should mutate only
// ev.Watch in place.
type WatchEnricher func(ctx context.Context, viewer *core.Viewer, events []core.LiveEvent)

// ChangeHandler observes raw (unmasked) event transitions detected by the hub.
// prev is nil when the event is seen for the first time.
type ChangeHandler func(prev *core.LiveEvent, next core.LiveEvent)

// FeedPollHandler observes completion of a feed poll attempt (success or error)
// so console observers can refresh cached health and events without extra polls.
type FeedPollHandler func(pluginID, feedID string)

// FeedHealth reports the hub's observed polling health for one live event feed.
type FeedHealth struct {
	LastPollAt        time.Time `json:"lastPollAt,omitempty"`
	LastOkAt          time.Time `json:"lastOkAt,omitempty"`
	LastError         string    `json:"lastError,omitempty"`
	ConsecutiveErrors int       `json:"consecutiveErrors"`
	NextPollSec       int       `json:"nextPollSec,omitempty"`
	NextPollAt        time.Time `json:"nextPollAt,omitempty"`
	Subscribers       int       `json:"subscribers"`
	Polling           bool      `json:"polling"`
}

// FeedLeagueSummary summarizes one league present in a feed's cached state.
type FeedLeagueSummary struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Abbr       string `json:"abbr,omitempty"`
	EventCount int    `json:"eventCount"`
}

// FeedOverview summarizes one feed's cached events, leagues, entity counts,
// and observed polling health without triggering plugin calls.
type FeedOverview struct {
	FeedID         string              `json:"id"`
	Title          string              `json:"title"`
	MinIntervalSec int                 `json:"minIntervalSec"`
	EntityKinds    []string            `json:"entityKinds,omitempty"`
	Active         bool                `json:"active"`
	Health         FeedHealth          `json:"health"`
	EventCount     int                 `json:"eventCount"`
	LiveCount      int                 `json:"liveCount"`
	UpcomingCount  int                 `json:"upcomingCount"`
	Leagues        []FeedLeagueSummary `json:"leagues,omitempty"`
	EntityCounts   map[string]int      `json:"entityCounts,omitempty"`
	EntityTotal    int                 `json:"entityTotal"`
}

// SubscribeRequest configures a hub subscription.
type SubscribeRequest struct {
	// EventIDs are namespaced event IDs ("pluginID:feedID:eventID").
	EventIDs []string
	// Feeds are feed references ("pluginID:feedID") or bare feed IDs ("scores").
	Feeds []string
	// Viewer scopes spoiler filtering, watch enrichment, screen resolution, and alerts.
	Viewer *core.Viewer
	// ViewerID is an optional fallback when Viewer is nil (e.g. background alert subscribers).
	ViewerID int64
	// FilterSpoilers masks scores, probability, situation, ranking and plays
	// according to the viewer's spoilerFree preference and per-event reveals.
	FilterSpoilers bool
	// InvalidateOnly sends spoiler-filtered updates without resolving watch targets.
	InvalidateOnly bool
	// Alerts subscribes to in-app alerts published for Viewer/ViewerID.
	Alerts bool
}

func (r SubscribeRequest) viewerID() int64 {
	if r.Viewer != nil && r.Viewer.ID > 0 {
		return r.Viewer.ID
	}
	if r.ViewerID > 0 {
		return r.ViewerID
	}
	return 0
}
