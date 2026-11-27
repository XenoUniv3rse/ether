package mediabrowser

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/frontendtest"
	"github.com/braedonsaunders/ether/internal/frontend/kit/ids"
)

// The TV detail screen decodes several independent item lists and standalone
// DTOs. Exercise them through the router against the pinned Jellyfin schema.
func TestJellyfinSeriesDetailConformance(t *testing.T) {
	showKey := core.ShowKey(496)
	specials := core.SeasonKey(496, 0)
	show := frontend.Title{
		Key: showKey, Type: "show", Title: "Series", ChildCount: 2,
		Cast: []core.Person{{TMDB: 42, Name: "Cast member without portrait"}},
	}
	episode := frontend.Title{Key: core.EpisodeKey(496, 0, 1), Type: "episode", Title: "Special", ShowKey: showKey, ParentKey: specials}
	first := frontend.Title{Key: core.EpisodeKey(496, 1, 1), Type: "episode", Title: "First episode", ShowKey: showKey, Season: 1, Episode: 1}
	upcoming := frontend.Title{
		Key: core.EpisodeKey(496, 1, 2), Type: "episode", Title: "Upcoming", ShowKey: showKey, Season: 1, Episode: 2,
		Released: time.Date(2099, 1, 1, 0, 0, 0, 0, time.UTC),
	}
	personID, _ := ids.GUID(ids.Person, 42)
	exp := issue759Exp{
		people: []frontend.PersonDetail{{ID: personID, Name: "Cast member without portrait"}},
		Experience: frontendtest.Experience{
			ViewsFunc: func(context.Context, *frontend.Viewer) ([]frontend.View, error) {
				return []frontend.View{{ID: "4", Type: "show", Native: "audiobooks", Name: "Audiobooks"}}, nil
			},
			ItemFunc: func(_ context.Context, r frontend.ItemRequest) (frontend.ItemDetail, error) {
				switch r.Key {
				case showKey:
					return frontend.ItemDetail{Title: show, Extras: []frontend.Extra{
						{Key: core.ExtraKey(496), ParentKey: showKey, Kind: "trailer", Title: "Trailer"},
						{Key: core.ExtraKey(497), ParentKey: showKey, Kind: "featurette", Title: "Featurette"},
					}}, nil
				case episode.Key:
					return frontend.ItemDetail{Title: episode}, nil
				case first.Key:
					return frontend.ItemDetail{Title: first}, nil
				}
				return frontend.ItemDetail{}, core.ErrNotFound
			},
			ChildrenFunc: func(_ context.Context, _ *frontend.Viewer, _ frontend.Device, key frontend.RatingKey, q frontend.ItemQuery) (core.Page, error) {
				switch {
				case key == specials:
					return core.Page{Items: []frontend.Title{episode}, Total: 1}, nil
				case len(q.Filter.ReleasedFilters) > 0:
					return core.Page{Items: []frontend.Title{upcoming}, Total: 1}, nil
				case q.Recursive:
					return core.Page{Items: []frontend.Title{first}, Total: 1}, nil
				default:
					return core.Page{Items: []frontend.Title{{Key: specials, ShowKey: showKey, Type: "season", Title: "Specials"}}, Total: 1}, nil
				}
			},
			SimilarFunc: func(context.Context, *frontend.Viewer, frontend.Device, frontend.RatingKey, frontend.ItemQuery) (frontend.SearchPage, error) {
				return frontend.SearchPage{Page: core.Page{Items: []frontend.Title{{Key: core.PluginKey(497), Type: "show", MBItemType: "Series", Title: "Another series"}}, Total: 1}}, nil
			},
			PersonFacetIDsFunc: func(context.Context, *frontend.Viewer, frontend.Device, frontend.RatingKey, []int64) (map[int64]string, error) {
				return map[int64]string{42: personID}, nil
			},
		},
	}
	inst, err := (Jellyfin{MachineID: "series-conformance"}).Mount(t.Context(), frontend.Host{
		Identity: frontendtest.Identity{ResolveFunc: func(context.Context, string) (*frontend.Viewer, error) {
			return &frontend.Viewer{ID: 1}, nil
		}},
		Experience: exp,
		Themes:     &themeCache{},
		ThemeTitle: func(context.Context, *frontend.Viewer, frontend.RatingKey) (*frontend.Title, error) {
			return &show, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = inst.Close(context.Background()) })

	id := ids.RatingKeyGUID(showKey)
	userID, _ := ids.GUID(ids.User, 1)
	detail := "/Items/" + id
	fields := "CanDelete,ChannelInfo,Chapters,ChildCount,CumulativeRunTimeTicks,DateCreated,DisplayPreferencesId,Genres,ItemCounts,MediaSourceCount,MediaSources,MediaStreams,Overview,Path,PrimaryImageAspectRatio,Taglines,Trickplay"
	for _, path := range []string{
		detail, "/Users/" + userID + detail,
		"/Shows/" + id + "/Seasons?Fields=" + fields,
		"/Shows/" + id + "/Episodes?Fields=" + fields + "&SeasonId=" + ids.RatingKeyGUID(specials),
		"/Shows/NextUp?Fields=" + fields + "&SeriesId=" + id,
		"/Shows/Upcoming?Fields=" + fields + "&ParentId=" + id,
		detail + "/Similar?Fields=" + fields + "&Limit=20",
		detail + "/SpecialFeatures", detail + "/LocalTrailers",
		detail + "/ThemeMedia", detail + "/ThemeSongs", detail + "/ThemeVideos",
		"/Persons", "/UserViews",
	} {
		t.Run(path, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, path, nil)
			r.Header.Set("Authorization", `MediaBrowser Client="Jellyfin Android TV", Device="Shield", DeviceId="shield", Version="0.19.10", Token="grant"`)
			w := httptest.NewRecorder()
			inst.Handler().ServeHTTP(w, r)
			if w.Code != http.StatusOK {
				t.Fatalf("series request failed: %d %s", w.Code, w.Body.String())
			}
			schemaPath := path
			if strings.HasPrefix(path, "/Users/") {
				schemaPath = detail // Both routes serve BaseItemDto.
			}
			requireJellyfinSpec(t, r.Method, schemaPath, w)

			var body any
			if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			checkSeriesKotlinDTO(t, "$", body)
		})
	}
}

func checkSeriesKotlinDTO(t *testing.T, path string, body any) {
	t.Helper()
	switch data := body.(type) {
	case []any:
		for _, item := range data {
			checkSeriesKotlinDTO(t, path+"[]", item)
		}
	case map[string]any:
		// Kotlin's generated constructors require fields which OpenAPI
		// leaves optional. The schema above still validates types and enums.
		var required map[string]string
		switch {
		case strings.HasSuffix(path, ".People[]"), strings.HasSuffix(path, ".GenreItems[]"), strings.HasSuffix(path, ".Studios[]"):
			required = map[string]string{"Id": "uuid"}
		case strings.HasSuffix(path, ".MediaSources[]"):
			required = map[string]string{"Protocol": "string", "Type": "string", "TranscodingSubProtocol": "string"}
			for _, field := range []string{
				"IsRemote", "ReadAtNativeFramerate", "IgnoreDts", "IgnoreIndex", "GenPtsInput",
				"SupportsTranscoding", "SupportsDirectStream", "SupportsDirectPlay", "IsInfiniteStream",
				"RequiresOpening", "RequiresClosing", "RequiresLooping", "SupportsProbing", "HasSegments",
			} {
				required[field] = "bool"
			}
		case strings.HasSuffix(path, ".MediaStreams[]"):
			required = map[string]string{"Type": "string", "Index": "int"}
			for _, field := range []string{
				"IsInterlaced", "IsDefault", "IsForced", "IsHearingImpaired",
				"IsExternal", "IsTextSubtitleStream", "SupportsExternalStream",
			} {
				required[field] = "bool"
			}
		case strings.HasSuffix(path, ".Chapters[]"):
			required = map[string]string{"ImageDateModified": "date", "StartPositionTicks": "long"}
		case strings.HasSuffix(path, ".UserData"):
			required = map[string]string{"Key": "string", "ItemId": "uuid", "PlayCount": "int", "IsFavorite": "bool", "Played": "bool", "PlaybackPositionTicks": "long"}
		case strings.HasSuffix(path, "Result"):
			required = map[string]string{"Items": "list", "TotalRecordCount": "int", "StartIndex": "int", "OwnerId": "uuid"}
		case data["Items"] != nil:
			required = map[string]string{"Items": "list", "TotalRecordCount": "int", "StartIndex": "int"}
		case data["ThemeSongsResult"] != nil:
			// The three result objects are checked when visited below.
		case path == "$", path == "$[]", strings.HasSuffix(path, ".Items[]"):
			required = map[string]string{"Id": "uuid", "Type": "string"}
		}
		if required != nil {
			(&kotlinModel{required: required}).check(t, path, data)
		}
		for k, value := range data {
			checkSeriesKotlinDTO(t, path+"."+k, value)
		}
	}
}
