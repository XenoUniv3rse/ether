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

const CatchUpLibraryName = "Catch-up"
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
}

func CatchUpGroupID(kind MediaType, title string) string {
	sum := sha256.Sum256([]byte(strings.ToLower(strings.TrimSpace(title))))
	return fmt.Sprintf("catchup-%s-%x", kind, sum[:16])
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

// CatchUpAiringLayout names an airing by when it started, in the server's
// local time: "Wed Nov 25, 6:30 PM".
const CatchUpAiringLayout = "Mon Jan 2, 3:04 PM"

// CatchUpEpisodeTitle names an archived airing. Guide entries for news, sport
// and talk shows usually carry no episode title, and the same programme often
// airs several times a day, so an airing without one is named by when it
// started; otherwise every airing of the programme shows the same name.
func CatchUpEpisodeTitle(p LiveTVProgramme) string {
	if p.Item.EpisodeTitle != "" {
		return p.Item.EpisodeTitle
	}
	return p.Start.In(time.Local).Format(CatchUpAiringLayout)
}
