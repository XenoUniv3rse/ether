package frontend

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"strconv"
	"strings"
	"time"
)

const CatchUpLibraryID = "catchup"

type CatchUpLibrary interface {
	CatchUpLibraryVisible(context.Context, *Viewer) (bool, error)
	CatchUpItems(context.Context, *Viewer, CatchUpQuery) (CatchUpPage, error)
	CatchUpItem(context.Context, *Viewer, string) (CatchUpItem, error)
}

type CatchUpQuery struct {
	Parent        string
	Offset, Limit int
	Recursive     bool
	Sort          CatchUpSort
	// Search matches the title or episode title, ignoring case, spacing and punctuation.
	Search string
	// Rollup fills Watch on listed shows and seasons with the viewer's
	// roll-up of their airings. It reads the airings of the listed groups
	// only, so its cost follows the page. A search gets none (it lists only
	// matching airings).
	Rollup bool
}

// CatchUpSort orders a catch-up listing. The zero value lists the most
// recently aired first.
type CatchUpSort string

const (
	CatchUpSortRecent    CatchUpSort = ""
	CatchUpSortOldest    CatchUpSort = "oldest"
	CatchUpSortTitle     CatchUpSort = "title"
	CatchUpSortTitleDesc CatchUpSort = "title-desc"
)

// CatchUpSortFor maps a library sort to a catch-up order: title, or when a
// programme aired (release, premiere and added dates all mean that here).
func CatchUpSortFor(order SortOrder, desc bool) CatchUpSort {
	switch order {
	case SortTitle:
		if desc {
			return CatchUpSortTitleDesc
		}
		return CatchUpSortTitle
	case SortReleased, SortAdded, SortYear:
		if !desc {
			return CatchUpSortOldest
		}
	}
	return CatchUpSortRecent
}

type CatchUpPage struct {
	Items []CatchUpItem
	Total int
}

type CatchUpItem struct {
	ID, ShowID, SeasonID string
	Type                 MediaType
	Programme            LiveTVProgramme
	Artwork              []Image
	// Watch is the viewer's state: for an episode (an airing) its own state,
	// for a show or season the roll-up of the airings in it when asked for
	// (CatchUpQuery.Rollup, CatchUpWatchState.CatchUpGroupWatch).
	Watch CatchUpWatch
}

// CatchUpWatch is one viewer's watch state for one catch-up airing. It is kept
// while the airing is in the archive and pruned when the archive drops it.
//
// For a show or season, Episodes is how many of its airings the viewer can
// play, Watched how many of those they have watched and InProgress how many
// have a resume point; LastViewedAt is the latest of the airings'. ViewOffset,
// ViewCount, IsFavorite and UserRating are zero there.
type CatchUpWatch struct {
	ViewOffset   time.Duration
	ViewCount    int
	LastViewedAt time.Time
	IsFavorite   bool
	UserRating   float64

	Episodes, Watched, InProgress int
}

// FullyWatched reports whether a show or season has airings and the viewer
// has watched all of them.
func (w CatchUpWatch) FullyWatched() bool { return w.Episodes > 0 && w.Watched >= w.Episodes }

// CatchUpWatchState records watch state for catch-up airings. The id is any
// id CatchUpItem accepts (the library id or the GUID). Progress takes an
// episode; SetWatched also takes a show or season and marks every airing in
// it the viewer can play. State is the timeline state ("playing", "paused",
// "buffering" or "stopped").
type CatchUpWatchState interface {
	CatchUpProgress(ctx context.Context, viewer *Viewer, id string, offset, duration time.Duration, state string) error
	CatchUpSetWatched(ctx context.Context, viewer *Viewer, id string, watched bool) error
	// CatchUpGroupWatch is a show's or season's roll-up for one viewer.
	CatchUpGroupWatch(ctx context.Context, viewer *Viewer, id string) (CatchUpWatch, error)
}

// CatchUpWithRollup fills a show's or season's roll-up when live keeps catch-up
// watch state, for a detail page. Episodes are returned as they are.
func CatchUpWithRollup(ctx context.Context, live any, viewer *Viewer, item CatchUpItem) (CatchUpItem, error) {
	w, ok := live.(CatchUpWatchState)
	if !ok || item.Type == TypeEpisode {
		return item, nil
	}
	watch, err := w.CatchUpGroupWatch(ctx, viewer, item.ID)
	if err != nil {
		return item, err
	}
	item.Watch = watch
	return item, nil
}

func CatchUpGroupID(kind MediaType, title string) string {
	return fmt.Sprintf("catchup-%s-%s", kind, CatchUpTitleKey(title))
}

// CatchUpTitleKey is the key every catch-up show and season id is built from:
// 32 hex characters of the airing title's hash. The guide stores it with each
// airing so a show's airings are one indexed lookup.
func CatchUpTitleKey(title string) string {
	sum := sha256.Sum256([]byte(strings.ToLower(strings.TrimSpace(title))))
	return fmt.Sprintf("%x", sum[:16])
}

// CatchUpGroupKey returns the title key of a show or season id. A show or
// season GUID carries only the key's first 28 characters; it returns that
// prefix with whole false.
func CatchUpGroupKey(id string) (key string, whole bool) {
	for _, prefix := range []string{"catchup-show-", "catchup-season-"} {
		if value, ok := strings.CutPrefix(id, prefix); ok && len(value) == 32 {
			return value, true
		}
	}
	if guid, kind := CatchUpGUIDGroup(id); kind != "" {
		return guid[4:], false
	}
	return "", false
}

func CatchUpEpisodeID(programmeID string) string {
	return "catchup-episode-" + base64.RawURLEncoding.EncodeToString([]byte(programmeID))
}

func IsCatchUpID(id string) bool { return id == CatchUpLibraryID || strings.HasPrefix(id, "catchup-") }

func CatchUpEpisodeRequest(id string) (CatchUpRequest, bool) {
	payload, ok := strings.CutPrefix(id, "catchup-episode-")
	if !ok || len(payload) > 256 {
		return CatchUpRequest{}, false
	}
	raw, err := base64.RawURLEncoding.DecodeString(payload)
	programme := string(raw)
	at := strings.LastIndexByte(programme, '-')
	if err != nil || at <= 0 || CatchUpEpisodeID(programme) != id {
		return CatchUpRequest{}, false
	}
	if _, err := strconv.ParseInt(programme[at+1:], 10, 64); err != nil {
		return CatchUpRequest{}, false
	}
	return CatchUpRequest{ChannelID: programme[:at], ProgrammeID: programme}, true
}

type CatchUpArtworkReader interface {
	CatchUpArtwork(context.Context, *Viewer, string, string) ([]byte, string, error)
}

// CatchUpSessionFinder lets a protocol reuse its existing archive session on
// manifest retries. The lookup is scoped to the viewer, protocol and device.
type CatchUpSessionFinder interface {
	FindCatchUp(context.Context, *Viewer, CatchUpRequest, ClientCaps, string) (CatchUpInfo, bool)
}

// CatchUpSessionReader checks whether a session belongs to this protocol and
// viewer. A nonempty device ID also requires the opening device to match.
type CatchUpSessionReader interface {
	LibraryCatchUpSession(SessionAccess, string) (CatchUpInfo, bool)
}

// CatchUpUserDataPatch updates one airing's watch state and preferences.
type CatchUpUserDataPatch struct {
	ItemPlaybackPatch
	Favorite, Likes *bool
	Rating          *float64
}

type CatchUpUserDataWriter interface {
	CatchUpPatchUserData(context.Context, *Viewer, string, CatchUpUserDataPatch) error
}
