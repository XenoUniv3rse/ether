package pluginitems

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/braedonsaunders/ether/internal/plugins"
)

type syncFailureRuntime struct {
	reply plugins.Response
	err   error
}

func (r *syncFailureRuntime) Call(_ context.Context, req plugins.Request) (plugins.Response, error) {
	if strings.HasSuffix(req.Path, "/browse") {
		return r.reply, r.err
	}
	return plugins.Response{Status: http.StatusOK}, nil
}

func (*syncFailureRuntime) Close() error { return nil }

type syncFailureJobs struct{ manager *plugins.Manager }

func (j syncFailureJobs) BeginSyncJob(pluginID, kindKey, route, label string, cancel context.CancelFunc) SyncJobHandle {
	return j.manager.BeginHostJob(pluginID, "sync:"+kindKey, route, label, kindKey, cancel)
}

func (j syncFailureJobs) UpdateSyncJob(pluginID, kindKey, state, code, outcome, message string) {
	j.manager.UpdateHostJob(pluginID, "sync:"+kindKey, state, code, outcome, message)
}

// A route failure must survive both inspector persistence and sync persistence
// with the same redacted diagnostic, even when the runtime cancels the job.
func TestSyncRouteFailureSurvivesCallAndJob(t *testing.T) {
	ctx := context.Background()
	s, db := openTestService(t, Options{})
	m, err := plugins.New(plugins.Deps{DB: db})
	if err != nil {
		t.Fatal(err)
	}
	rt := &syncFailureRuntime{}
	m.RegisterRuntime("hosted", func(plugins.Installed) (plugins.Runtime, error) { return rt, nil })
	s.SetCaller(m)
	s.SetJobTracker(syncFailureJobs{m})
	const id = "example.failure"
	preview, err := m.PreviewInstall(ctx, plugins.Source{
		Kind: "studio",
		Manifest: json.RawMessage(`{"id":"example.failure","version":"1.0.0","name":"Example",
			"ether":{"protocol":1,"kind":"extension","summary":"Reports failures","runtime":{"kind":"hosted","entry":"plugin.js"},
			"permissions":{"secrets":["api_token"]},"settings":{"fields":[{"key":"api_token","kind":"secret","label":"API token"}]}}}`),
		Files: map[string][]byte{"plugin.js": []byte("export function handle() {}")},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Install(ctx, preview, plugins.InstallOptions{Settings: map[string]any{"api_token": "saved-fixture-value"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpsertKind(ctx, id, 20, musicAlbumDecl(), false); err != nil {
		t.Fatal(err)
	}

	const route = "/ether/v1/kinds/albums/browse"
	ownerJSON := func(t *testing.T, path string, target any) {
		t.Helper()
		w := httptest.NewRecorder()
		m.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/"+id+"/"+path, nil))
		if w.Code != http.StatusOK {
			t.Fatalf("%s status = %d", path, w.Code)
		}
		if err := json.Unmarshal(w.Body.Bytes(), target); err != nil {
			t.Fatal(err)
		}
		for _, credential := range []string{"saved-fixture-value", "path-fixture-value", "text-fixture-value", "json-fixture-value"} {
			if strings.Contains(w.Body.String(), credential) {
				t.Fatalf("%s leaked a credential", path)
			}
		}
	}
	for _, tc := range []struct {
		name, code, body, message string
		status, retry             int
		err                       error
		headers                   map[string]string
	}{
		{name: "reported auth", code: "auth", status: 502, message: "Catalogue rejected saved-fixture-value",
			body: `{"error":{"code":"auth","message":"Catalogue rejected saved-fixture-value"},"url":"https://example.invalid/path-fixture-value","token":"json-fixture-value"}`},
		{name: "upstream status", code: "auth", status: 403},
		{name: "rate limit header", code: "rate_limited", status: 429, retry: 300, headers: map[string]string{"Retry-After": "300"}},
		{name: "rate limit envelope", code: "rate_limited", status: 502, retry: 360,
			body:    `{"error":{"code":"rate_limited","message":"Catalogue is rate limiting requests","retryAfterSeconds":360}}`,
			message: "Catalogue is rate limiting requests"},
		{name: "unavailable", code: "upstream_unavailable", status: 502,
			body: `{"error":{"code":"upstream_unavailable","message":"Catalogue is unavailable"}}`, message: "Catalogue is unavailable"},
		{name: "missing", code: "not_found", status: 404, body: `{"error":{"code":"not_found","message":"Catalogue is missing"}}`, message: "Catalogue is missing"},
		{name: "settings", code: "invalid_settings", status: 400,
			body: `{"error":{"code":"invalid_settings","message":"Choose a catalogue"}}`, message: "Choose a catalogue"},
		{name: "text", code: "rejected", status: 500, body: "Catalogue error with password: text-fixture-value",
			message: "Catalogue error with password=[redacted]"},
		{name: "timeout", code: "timeout", err: context.DeadlineExceeded},
		{name: "malformed", code: "malformed", status: 200, body: `{"items":15}`},
		{name: "crash", code: "crashed", err: errors.New("plugin trapped")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rt.reply = plugins.Response{Status: tc.status, Body: json.RawMessage(tc.body), Headers: tc.headers}
			rt.err = tc.err
			if _, err := s.SyncKind(ctx, id, "albums", SyncOptions{}); err == nil {
				t.Fatal("sync accepted a failed route")
			}
			state, err := s.SyncStateOf(ctx, id, "albums")
			if err != nil || state.Failure == nil {
				t.Fatalf("sync state missing failure: %v", err)
			}
			f := state.Failure
			if f.Code != tc.code || f.Route != route || f.Status != tc.status || f.RetryAfterSeconds != tc.retry {
				t.Errorf("stored route classification = %s, HTTP %d, retry %d", f.Code, f.Status, f.RetryAfterSeconds)
			}
			if f.Message != strings.ReplaceAll(tc.message, "saved-fixture-value", "[redacted]") {
				t.Error("stored failure message differs after redaction")
			}
			if (f.TimeoutSeconds > 0) != (tc.code == "timeout") {
				t.Error("incorrect timeout metadata")
			}
			var history struct{ Calls []plugins.CallRecord }
			ownerJSON(t, "calls", &history)
			if len(history.Calls) == 0 || history.Calls[0].Failure == nil || *history.Calls[0].Failure != *f {
				t.Error("call inspector and sync persisted different diagnostics")
			}
			var jobs []plugins.ActionJobStatus
			ownerJSON(t, "jobs", &jobs)
			if len(jobs) != 1 || jobs[0].State != "error" || jobs[0].Failure == nil || *jobs[0].Failure != *f {
				t.Error("sync job lost the route failure")
			}
		})
	}

	rt.reply = plugins.Response{Status: http.StatusOK, Body: json.RawMessage(`{"items":[]}`)}
	rt.err = nil
	if _, err := s.SyncKind(ctx, id, "albums", SyncOptions{}); err != nil {
		t.Fatal(err)
	}
	state, err := s.SyncStateOf(ctx, id, "albums")
	if err != nil || state.Failure != nil || state.LastError != "" {
		t.Fatal("successful sync did not clear the failure")
	}
}
