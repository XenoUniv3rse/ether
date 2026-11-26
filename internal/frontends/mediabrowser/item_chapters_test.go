package mediabrowser

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/frontendtest"
	"github.com/braedonsaunders/ether/internal/frontend/kit/ids"
)

// A multi-part audiobook knows its chapters from its parts before any source
// is probed. The Jellyfin item detail must list them: apps read chapter names
// and start positions from the item, not from a resolved version.
func TestJellyfinItemDetailCarriesItemLevelChapters(t *testing.T) {
	viewer := &frontend.Viewer{ID: 1}
	bookKey := core.PluginKey(501)
	exp := frontendtest.Experience{
		ItemFunc: func(context.Context, frontend.ItemRequest) (frontend.ItemDetail, error) {
			return frontend.ItemDetail{
				Title:               frontend.Title{Key: bookKey, Type: frontend.TypeMovie, Title: "Adventures of Sherlock Holmes", MediaClass: core.MediaClassAudiobook, MBItemType: "AudioBook"},
				ImagesAuthoritative: true,
				Images:              []frontend.Image{{Kind: frontend.ImagePoster, Path: "/cover"}},
				Chapters: []core.Chapter{
					{ID: 0, Start: 0, End: 50 * time.Minute},
					{ID: 1, Title: "The Red-Headed League", Start: 50 * time.Minute, End: 95 * time.Minute},
				},
			}, nil
		},
	}
	grants := frontendtest.ImageGrants{
		IssueFunc: func(_ context.Context, req frontend.ImageGrantRequest) (string, error) {
			if req.Item != bookKey || req.Image.Path != "/cover" || req.Credential != "grant" {
				return "", frontend.ErrNotFound
			}
			return "cover-tag", nil
		},
		ServeFunc: func(w http.ResponseWriter, r *http.Request, tag string) {
			if tag != "cover-tag" {
				http.NotFound(w, r)
				return
			}
			_, _ = w.Write([]byte("book cover"))
		},
	}
	ident := frontendtest.Identity{ResolveFunc: func(context.Context, string) (*frontend.Viewer, error) { return viewer, nil }}
	inst, err := (Jellyfin{MachineID: "test"}).Mount(context.Background(), frontend.Host{
		Identity:     ident,
		Experience:   exp,
		ImageGrants:  grants,
		Capabilities: frontendtest.Capabilities{},
		SurfaceURL:   func(context.Context, *http.Request) (string, error) { return "https://ether.example/jellyfin", nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	itemPath := "/jellyfin/Items/" + ids.RatingKeyGUID(bookKey)
	req := httptest.NewRequest(http.MethodGet, itemPath, nil)
	req.Header.Set("X-Emby-Token", "grant")
	rec := httptest.NewRecorder()
	inst.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("item detail = (%d) %s", rec.Code, rec.Body.String())
	}
	var got struct {
		ImageTags map[string]string
		Chapters  []struct {
			Name               *string
			StartPositionTicks int64
			ImageDateModified  *string
			ImageTag           string
		}
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Chapters) != 2 {
		t.Fatalf("chapters = %d, want 2: %s", len(got.Chapters), rec.Body.String())
	}
	second := got.Chapters[1]
	if second.Name == nil || *second.Name != "The Red-Headed League" || second.StartPositionTicks != (50*time.Minute).Nanoseconds()/100 || second.ImageDateModified == nil {
		t.Fatalf("second chapter = %+v", second)
	}
	if got.Chapters[0].Name != nil {
		t.Fatal("unnamed chapter carries a Name")
	}
	cover := got.ImageTags["Primary"]
	for _, chapter := range got.Chapters {
		if cover == "" || chapter.ImageTag != cover {
			t.Fatal("chapter does not carry the primary cover tag")
		}
	}
	for _, query := range []string{"", "?tag=" + url.QueryEscape(second.ImageTag)} {
		req := httptest.NewRequest(http.MethodGet, itemPath+"/Images/Chapter/1"+query, nil)
		req.Header.Set("X-Emby-Token", "grant")
		rec := httptest.NewRecorder()
		inst.Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusOK || rec.Body.String() != "book cover" {
			t.Fatalf("chapter image = (%d) %s", rec.Code, rec.Body.String())
		}
	}
}
