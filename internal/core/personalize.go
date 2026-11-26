package core

import (
	"context"
	"strings"
	"time"
)

// Device classes for HubContext.Device.
const (
	DeviceTV      = "tv"
	DeviceMobile  = "mobile"
	DeviceWeb     = "web"
	DeviceDesktop = "desktop"
)

// Dayparts for HubContext.Daypart, in the server's local time.
const (
	DaypartMorning   = "morning"   // 05:00–11:59
	DaypartAfternoon = "afternoon" // 12:00–16:59
	DaypartEvening   = "evening"   // 17:00–21:59
	DaypartLate      = "late"      // 22:00–04:59
)

// HubContext is the situation a Plex client asked for rows in: which kind of
// device, when, and for which section (empty = Home).
type HubContext struct {
	Device    string    `json:"device"`
	Daypart   string    `json:"daypart"`
	Weekend   bool      `json:"weekend"`
	Now       time.Time `json:"now"`
	SectionID int       `json:"sectionId,omitempty"`
	// HomeScoped marks a library-scoped Home request (such as Plex
	// /hubs/promoted?contentDirectoryID=N): rows remain scoped to the
	// section's media type and library membership, while layout policies and
	// collection targeting use the Home ("home") surface.
	HomeScoped bool   `json:"-"`
	Language   string `json:"-"`
	Product    string `json:"product,omitempty"`  // X-Plex-Product
	Platform   string `json:"platform,omitempty"` // X-Plex-Platform
	SessionID  string `json:"sessionId,omitempty"`
	ClientID   string `json:"clientId,omitempty"`
}

// ContextualRecommender is implemented by recommenders that adapt rows to
// the request context. internal/plex prefers it over Recommender when
// available.
type ContextualRecommender interface {
	HomeHubsFor(ctx context.Context, user *User, hc HubContext, limit int) ([]Hub, error)
	SectionHubsFor(ctx context.Context, user *User, t MediaType, hc HubContext, limit int) ([]Hub, error)
}

// HubImpression is one row as served to a user, for the personalization
// feedback loop.
type HubImpression struct {
	UserID   int64       `json:"userId"`
	HubID    string      `json:"hubId"`
	Position int         `json:"position"`
	Keys     []RatingKey `json:"keys"`
	Context  HubContext  `json:"context"
	"strings"`
	At time.Time `json:"at"`
}

// ImpressionSink receives served rows. Implementations must not block.
type ImpressionSink interface {
	RecordImpressions(user *User, hc HubContext, hubs []Hub)
}

// CollectionRow is an owner collection (or a plugin list) served as a row
// for a viewer: enabled, visible to them, with their content-rating limit,
// personal rules, order and rotation applied. Where it shows is a row on a
// Recommendations page.
type CollectionRow struct {
	Hub          Hub    `json:"hub"`
	CollectionID string `json:"collectionId"`
}

// PersonalScorer scores titles for a user (higher = better fit). The
// collection engine uses it for the "personal" sort.
type PersonalScorer interface {
	ScoreTitles(ctx context.Context, user *User, titles []Title) (map[RatingKey]float64, error)
}

// SectionView restricts a Home hub to one library section's media type,
// so mixed rows that belong on a section too (the watchlist, Rediscover)
// can appear there with only that section's titles. A typed hub passes
// through when its type matches. Recaps (Year in Review) stay on Home.
// ok is false when nothing of the type remains.
func SectionView(h Hub, t MediaType) (Hub, bool) {
	if h.Context == "hub.home.yearinreview" {
		return h, false
	}
	match := func(x MediaType) bool {
		if t == TypeShow {
			return x == TypeShow || x == TypeSeason || x == TypeEpisode
		}
		return x == t
	}
	if h.Type != "" {
		return h, match(h.Type)
	}
	items := make([]Title, 0, len(h.Items))
	for _, it := range h.Items {
		if match(it.Type) {
			items = append(items, it)
		}
	}
	if len(items) == 0 {
		return h, false
	}
	h.Items = items
	h.Key = SectionCollectionKey(h.Key, t)
	h.Type = t
	return h, true
}

const WatchlistSplitParam = "splitByAvailability"

func IsWatchlistHub(id string) bool {
	return strings.HasPrefix(id, "watchlist.")
}

// WatchlistHubAvailable and WatchlistHubComing identify the split watchlist
// shelves independently of their configured names and translated labels.
func WatchlistHubAvailable(id string) bool {
	return IsWatchlistHub(id) && strings.HasSuffix(id, ".available")
}

func WatchlistHubComing(id string) bool {
	return IsWatchlistHub(id) && strings.HasSuffix(id, ".coming")
}
