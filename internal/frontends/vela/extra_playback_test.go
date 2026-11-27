package vela

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/frontendtest"
	"github.com/braedonsaunders/ether/internal/frontend/kit/ids"
	"github.com/braedonsaunders/ether/internal/sessions"
)

type trailerPlaybackBackend struct{ mvBrokerPlaybackFake }

func (*trailerPlaybackBackend) KeepAlive(string) bool { return false }

type trailerEventRecorder struct {
	*httptest.ResponseRecorder
	cancel context.CancelFunc
}

func (w trailerEventRecorder) Flush() {
	w.ResponseRecorder.Flush()
	w.cancel()
}

func TestExtraPlaybackMediaAfterEvents(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	clip := filepath.Join(t.TempDir(), "trailer.mp4")
	if err := os.WriteFile(clip, []byte("trailer bytes"), 0600); err != nil {
		t.Fatal(err)
	}
	active := func(_ context.Context, id int64) (*core.Viewer, error) { return &core.Viewer{ID: id}, nil }
	source := func() sessions.PreviewSource {
		return sessions.PreviewSource{MIMEType: "video/mp4", SourceGeneration: "cached-trailer", Open: func() (*os.File, error) { return os.Open(clip) }}
	}
	bus := sessions.NewEventBus(nil, active)
	defer bus.Close()
	extra, parent := core.ExtraKey(1), core.MovieKey(1)
	broker, err := sessions.NewBroker(sessions.BrokerDeps{
		Playback: &trailerPlaybackBackend{}, Progress: mvBrokerProgressFake{}, ActiveViewer: active, Events: bus,
		CanPlay: func(*core.Viewer, bool, time.Time) bool { return true }, BitrateLimit: func(*core.Viewer) int { return 0 },
		OnTimeline: func(*core.Viewer, core.RatingKey, time.Duration, time.Duration, string) {},
		Lookup: func(_ context.Context, _ *core.Viewer, _ core.Device, key core.RatingKey) (*core.Title, error) {
			return &core.Title{Key: key}, nil
		},
		ExtraLookup: func(key core.RatingKey) (frontend.Extra, bool) {
			return frontend.Extra{Key: extra, ParentKey: parent, Kind: "trailer"}, key == extra
		},
		PreviewSource: func(context.Context, *core.Viewer, *core.Title, string) (sessions.PreviewSource, error) {
			return source(), nil
		},
		ExtraSource: func(context.Context, *core.Viewer, *core.Title, core.RatingKey, string) (sessions.PreviewSource, error) {
			return source(), nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer broker.Close(ctx)
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(`CREATE TABLE vela_replays(key TEXT PRIMARY KEY,digest TEXT NOT NULL,response BLOB,expires INTEGER NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	i, err := (Frontend{DB: db, ReplayKey: make([]byte, 32)}).Mount(ctx, frontend.Host{
		Identity: identityFake{}, Experience: frontendtest.Experience{},
		Capabilities: frontendtest.Capabilities{}, Sessions: sessions.BindSurface(broker, "vela"),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer i.Close(ctx)
	handler := i.Handler()
	api := func(method, path, body string) *http.Request {
		r := httptest.NewRequest(method, "/vela/v0"+path, strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer token")
		r.Header.Set("X-Vela-Device-Id", "device")
		return r
	}
	type trailerPlan struct {
		SessionID string
		State     string
		Paths     []struct{ URL string }
	}
	type trailerSnapshot struct{ Plan trailerPlan }
	start := httptest.NewRecorder()
	handler.ServeHTTP(start, api(http.MethodPost, "/playback", `{"extraId":"`+ids.RatingKeyGUID(extra)+`","idempotencyKey":"trailer","client":{"device":{"id":"device"},"protocolVersions":[0],"containers":["mp4"],"videoCodecs":["h264"],"audioCodecs":["aac"]}}`))
	var plan trailerPlan
	if start.Code != http.StatusOK || json.Unmarshal(start.Body.Bytes(), &plan) != nil || plan.SessionID == "" || plan.State != "ready" || len(plan.Paths) != 1 {
		t.Fatalf("trailer start status %d, state %q, paths %d", start.Code, plan.State, len(plan.Paths))
	}
	sessionPath := "/sessions/" + plan.SessionID
	snapshot := httptest.NewRecorder()
	handler.ServeHTTP(snapshot, api(http.MethodGet, sessionPath, ""))
	var snap trailerSnapshot
	if snapshot.Code != http.StatusOK || json.Unmarshal(snapshot.Body.Bytes(), &snap) != nil || snap.Plan.State != "ready" ||
		len(snap.Plan.Paths) != 1 || snap.Plan.Paths[0].URL != plan.Paths[0].URL || !strings.HasPrefix(snap.Plan.Paths[0].URL, "/sessions/media/") {
		t.Fatalf("session status %d, state %q, playable path not preserved", snapshot.Code, snap.Plan.State)
	}
	eventCtx, closeEvents := context.WithCancel(ctx)
	defer closeEvents()
	events := trailerEventRecorder{httptest.NewRecorder(), closeEvents}
	handler.ServeHTTP(events, api(http.MethodGet, sessionPath+"/events", "").WithContext(eventCtx))
	_, data, found := strings.Cut(events.Body.String(), "data: ")
	var event struct{ Snapshot trailerSnapshot }
	if events.Code != http.StatusOK || !found || json.Unmarshal([]byte(strings.TrimSpace(data)), &event) != nil || event.Snapshot.Plan.State != "ready" {
		t.Fatalf("first event status %d, state %q", events.Code, event.Snapshot.Plan.State)
	}
	media := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, snap.Plan.Paths[0].URL, nil)
	req.Header.Set("Range", "bytes=0-6")
	handler.ServeHTTP(media, req)
	if media.Code != http.StatusPartialContent || media.Body.String() != "trailer" {
		t.Fatalf("trailer ranged media GET status %d or wrong bytes", media.Code)
	}
}

func (f *previewBrokerFake) ExtraPlaybackAvailable() bool { return true }
func (f *previewBrokerFake) ExtraPlayback(_ context.Context, req frontend.TrailerPreviewRequest) (frontend.TrailerPreviewResult, error) {
	f.calls++
	return f.result, nil
}

// Cold preparation is retryable under the same mutation key; admission is
// replayed once ready, with independent controls and explicit owner hero policy.
func TestExtraPlaybackContract(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	if _, err = db.Exec(`CREATE TABLE vela_replays(key TEXT PRIMARY KEY,digest TEXT NOT NULL,response BLOB,expires INTEGER NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	seal, err := encryption(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	broker := &previewBrokerFake{result: frontend.TrailerPreviewResult{State: "preparing", RetryAfterSeconds: 1}}
	reports, stops := 0, 0
	broker.previewBaseSessions = frontendtest.Sessions{ReportFunc: func(context.Context, frontend.SessionAccess, string, frontend.Timeline) error { reports++; return nil }, StopFunc: func(context.Context, frontend.SessionAccess, string) error { stops++; return nil }}
	i := &instance{host: frontend.Host{Identity: identityFake{}, Capabilities: frontendtest.Capabilities{}, Sessions: broker}, ctx: context.Background(), mux: http.NewServeMux(), db: db, seal: seal, replays: map[string]*replay{}, sessions: map[string]*playSession{}}
	i.routes()
	request := func(path, dev string, b object) *httptest.ResponseRecorder {
		raw, _ := json.Marshal(b)
		r := httptest.NewRequest("POST", "/vela/v0"+path, strings.NewReader(string(raw)))
		r.Header.Set("Authorization", "Bearer token")
		r.Header.Set("X-Vela-Device-Id", dev)
		w := httptest.NewRecorder()
		i.serve(w, r)
		return w
	}
	extra := ids.RatingKeyGUID(core.ExtraKey(1))
	b := object{"extraId": extra, "idempotencyKey": "play-extra", "client": object{"device": object{"id": "device"}, "protocolVersions": []int{0}, "containers": []string{"mp4"}, "videoCodecs": []string{"h264"}, "audioCodecs": []string{"aac"}}}
	if w := request("/playback", "device", b); w.Code != 202 || w.Header().Get("Retry-After") != "1" || len(i.sessions) != 0 {
		t.Fatal("cold extra promised admission", w.Code, w.Body.String())
	}
	broker.result = frontend.TrailerPreviewResult{State: "ready", Grant: &frontend.TrailerPreviewGrant{SessionID: "full-extra", URL: "/sessions/media/full-extra/grant/preview.mp4", MIMEType: "video/mp4", Purpose: "playback", ExpiresAt: time.Now().Add(time.Hour), SourceGeneration: 1, SeekGeneration: 1}}
	ready := request("/playback", "device", b)
	replay := request("/playback", "device", b)
	if ready.Code != 200 || replay.Body.String() != ready.Body.String() || broker.calls != 2 || len(i.sessions) != 1 {
		t.Fatal("preparing retry/admission replay failed", ready.Code, replay.Code, broker.calls, ready.Body.String())
	}
	s := i.sessions["full-extra"]
	if s.plan["purpose"] != "playback" || s.plan["extraId"] != extra || s.plan["titleId"] != nil || s.plan["positionSeconds"] != 0 {
		t.Fatal("extra acquired library/resume identity", s.plan)
	}
	command := object{"idempotencyKey": "seek", "kind": "seek", "positionSeconds": 15, "sourceGeneration": 1, "seekGeneration": 1}
	if w := request("/sessions/full-extra/commands", "device", command); w.Code != 200 {
		t.Fatal("seek failed", w.Code, w.Body.String())
	}
	command = object{"idempotencyKey": "pause", "kind": "pause", "sourceGeneration": 1, "seekGeneration": 2}
	if w := request("/sessions/full-extra/commands", "device", command); w.Code != 200 || reports != 2 {
		t.Fatal("pause failed", w.Code, w.Body.String(), reports)
	}
	command = object{"idempotencyKey": "stop", "kind": "stop", "sourceGeneration": 1, "seekGeneration": 2}
	if w := request("/sessions/full-extra/commands", "another", command); w.Code != 404 || stops != 0 {
		t.Fatal("cross-device stop", w.Code)
	}
	command["idempotencyKey"] = "stop-correct-device"
	if w := request("/sessions/full-extra/commands", "device", command); w.Code != 200 || stops != 1 {
		t.Fatal("stop failed", w.Code, w.Body.String())
	}
	t.Run("hero default and owner off", func(t *testing.T) {
		row := object{"purpose": frontend.RowRecentlyReleased}
		entry := object{"kind": "title", "receiptToken": "issued", "title": object{"id": "title", "kind": "movie", "artwork": []object{{"kind": "backdrop", "sizes": []object{{"url": "/signed"}}}}}}
		off, on := false, true
		for _, tc := range []struct {
			available bool
			policy    *bool
			want      bool
		}{{true, nil, true}, {true, &off, false}, {true, &on, true}, {false, nil, false}, {false, &on, false}} {
			got, ok := heroFromEntry(row, entry, heroSource{previewDefault: tc.available, previewPolicy: tc.policy})
			enabled, _ := got.hero["previewOnFocus"].(bool)
			if !ok || enabled != tc.want {
				t.Fatal("hero preview ignored availability/owner override", tc, got)
			}
		}
	})
}

type velaExtraStreamsFake struct{ enabled bool }

func (f velaExtraStreamsFake) Lookup(core.RatingKey) (frontend.Extra, bool) {
	return frontend.Extra{}, false
}
func (f velaExtraStreamsFake) Open(context.Context, core.RatingKey) (string, string, map[string]string, error) {
	return "", "", nil, core.ErrNotFound
}
func (f velaExtraStreamsFake) Enabled() bool { return f.enabled }
func (f velaExtraStreamsFake) List(context.Context, *core.Title, string) ([]frontend.ExtraClip, error) {
	return nil, nil
}
func (f velaExtraStreamsFake) Clip(core.RatingKey) (frontend.ExtraClip, bool) {
	return frontend.ExtraClip{}, false
}
func (f velaExtraStreamsFake) Resolve(context.Context, frontend.ExtraClip) (frontend.ResolvedExtra, error) {
	return frontend.ResolvedExtra{}, core.ErrNotFound
}
func (f velaExtraStreamsFake) PrewarmBytes() int64 { return 0 }

func TestCapabilitiesExtrasAvailability(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		inst := &instance{host: frontend.Host{ExtraStreams: velaExtraStreamsFake{enabled: enabled}}}
		got := inst.capabilities()["features"].(object)["extras"].(object)["available"]
		if got != enabled {
			t.Fatalf("extras.available = %v, want %v", got, enabled)
		}
	}
}
