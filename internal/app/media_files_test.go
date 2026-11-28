package app

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/braedonsaunders/ether/internal/blobcache"
)

func TestMediaFileExplorerProtectsReadsAndClearsFile(t *testing.T) {
	a := setupTestApp(t, true)
	ns := a.mediaFiles.Namespace("test-games", blobcache.Quota{})
	ref, err := ns.Put(context.Background(), "test/game.iso", strings.NewReader("data"), 4)
	if err != nil {
		t.Fatal(err)
	}
	handler := a.cacheHandler()
	call := func(method, path, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w
	}
	r, err := ns.Open(ref)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	w := call("GET", "/?files=1", "")
	var snap struct {
		Used  int64
		Files []struct {
			Name, Library, Ref string
			InUse              bool
			Size               int64
		}
	}
	if err := json.Unmarshal(w.Body.Bytes(), &snap); err != nil {
		t.Fatal(err)
	}
	if snap.Used < 4 || len(snap.Files) != 1 || snap.Files[0].Ref != string(ref) || snap.Files[0].Name != "game.iso" || snap.Files[0].Library != "test-games" || !snap.Files[0].InUse || snap.Files[0].Size != 4 {
		t.Fatalf("unexpected snapshot %+v", snap)
	}
	body := `{"libraryKey":"test-games","ref":"` + string(ref) + `"}`
	if w := call("POST", "/clear-file", body); w.Code != 409 {
		t.Fatal(w.Code, w.Body.String())
	}
	r.Close()
	if w := call("POST", "/clear-file", body); w.Code != 200 || !strings.Contains(w.Body.String(), `"freedBytes":4`) {
		t.Fatal(w.Code, w.Body.String())
	}
	if _, ok := ns.LookupKey("test/game.iso"); ok {
		t.Fatal("cleared file still present")
	}
}
