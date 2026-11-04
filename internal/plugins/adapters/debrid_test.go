package adapters

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/debrid"
	"github.com/braedonsaunders/ether/internal/plugins"
)

type debridCallFunc func(context.Context, string, plugins.Request) (plugins.Response, error)

func (f debridCallFunc) Call(c context.Context, id string, r plugins.Request) (plugins.Response, error) {
	return f(c, id, r)
}
func TestNativeDebridSelectsWantedEpisodeAndKeepsCallbacksInHost(t *testing.T) {
	p := &debridProvider{id: "test", mgr: debridCallFunc(func(_ context.Context, id string, r plugins.Request) (plugins.Response, error) {
		if id != "test" || r.Method != "POST" || r.Path != "/ether/debrid/acquire" {
			t.Fatalf("request: %s %+v", id, r)
		}
		var q map[string]any
		if err := json.Unmarshal(r.Body, &q); err != nil {
			t.Fatal(err)
		}
		if q["cachedOnly"] != true || q["infoHash"] != "abc" || len(q) != 4 {
			t.Fatalf("wire request: %s", r.Body)
		}
		return plugins.Response{Status: 200, Body: json.RawMessage(`{"torrentId":"T1","ready":true,"added":false,"files":[{"id":"1","path":"Show.S01E01.mkv","size":10000},{"id":"2","path":"Show.S01E02.mkv","size":9000}]}`)}, nil
	})}
	got, err := p.Acquire(context.Background(), debrid.AcquireRequest{InfoHash: "abc", CachedOnly: true, Choose: func(f []debrid.File) (debrid.File, error) {
		return debrid.ChooseFile(f, &core.ReleaseTarget{Season: 1, Episode: 2})
	}})
	if err != nil || got.File.ID != "2" || got.Added {
		t.Fatalf("got=%+v err=%v", got, err)
	}
}
func TestNativeDebridOnlyRemovesNewUncachedTransfersWhenAllowed(t *testing.T) {
	for _, added := range []bool{false, true} {
		for _, allowed := range []bool{false, true} {
			removed := 0
			p := &debridProvider{id: "test", mgr: debridCallFunc(func(_ context.Context, _ string, r plugins.Request) (plugins.Response, error) {
				if strings.HasSuffix(r.Path, "/remove") {
					removed++
					return plugins.Response{Status: 200}, nil
				}
				b, _ := json.Marshal(map[string]any{"torrentId": "T", "ready": false, "added": added})
				return plugins.Response{Status: 200, Body: b}, nil
			})}
			_, err := p.Acquire(context.Background(), debrid.AcquireRequest{CachedOnly: true, RemoveUncached: true, MayRemove: func(context.Context, string) bool { return allowed }})
			if !errors.Is(err, core.ErrNotCached) || removed != map[bool]int{false: 0, true: 1}[added && allowed] {
				t.Fatalf("added=%v allowed=%v removed=%d err=%v", added, allowed, removed, err)
			}
		}
	}
}
func TestNativeDebridErrorsAndUnsafeLinks(t *testing.T) {
	for status, want := range map[int]error{401: debrid.ErrAuth, 403: debrid.ErrAuth, 404: debrid.ErrGone, 409: core.ErrNotCached, 429: debrid.ErrRateLimited, 501: debrid.ErrUnsupported} {
		p := &debridProvider{mgr: debridCallFunc(func(context.Context, string, plugins.Request) (plugins.Response, error) {
			return plugins.Response{Status: status, Body: json.RawMessage(`{"error":"secret-token"}`)}, nil
		})}
		_, err := p.Account(context.Background())
		if !errors.Is(err, want) || strings.Contains(err.Error(), "secret-token") {
			t.Fatalf("status %d: %v", status, err)
		}
	}
	for _, u := range []string{"http://127.0.0.1/private", "http://169.254.169.254/metadata", "file:///etc/passwd"} {
		p := &debridProvider{mgr: debridCallFunc(func(context.Context, string, plugins.Request) (plugins.Response, error) {
			b, _ := json.Marshal(map[string]string{"url": u})
			return plugins.Response{Status: 200, Body: b}, nil
		})}
		if _, err := p.Link(context.Background(), debrid.Handle{}); err == nil {
			t.Fatalf("accepted %s", u)
		}
	}
}
