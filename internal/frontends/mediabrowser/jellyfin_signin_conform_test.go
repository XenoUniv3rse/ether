package mediabrowser

import (
	"context"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/conform"
	mbconform "github.com/braedonsaunders/ether/internal/conform/mediabrowser"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/frontendtest"
	"github.com/braedonsaunders/ether/internal/frontend/kit/ids"
)

// TestJellyfinSignInRecordedCorpus replays the recorded Jellyfin Android TV
// connect, password sign-in and Quick Connect flows against Ether and compares
// each answer with the Jellyfin 12.4 recording. Fixture-only steps are replayed
// here too: the fake identity stands in for the out-of-band approval.
func TestJellyfinSignInRecordedCorpus(t *testing.T) {
	const client, token, secret = "jellyfin-androidtv", "signed-in-token", "approved-secret"
	p, err := mbconform.Load("jellyfin", filepath.Join("..", "..", "conform", "profiles", "jellyfin"))
	if err != nil {
		t.Fatal(err)
	}
	viewer := &frontend.Viewer{ID: 7, Username: "viewer"}
	userID, _ := ids.GUID(ids.User, viewer.ID)
	vars := map[string]string{"username": "viewer", "password": "password", "device_id": "conform-device", "user_id": userID, "quick_connect_secret": secret}
	created := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	identity := frontendtest.Identity{
		ResolveFunc: func(_ context.Context, credential string) (*frontend.Viewer, error) {
			if credential != token {
				return nil, frontend.ErrIdentityUnavailable
			}
			return viewer, nil
		},
		ViewerTimesFunc: func(context.Context, string, int64) (frontend.ViewerTimes, error) {
			return frontend.ViewerTimes{LastLogin: created, LastActivity: created}, nil
		},
		SignInFunc: func(context.Context, string, string, frontend.Device) (string, error) { return token, nil },
		InitiateFunc: func(context.Context, frontend.Device) (string, string, error) {
			return "123456", "pending-secret", nil
		},
		DetailsFunc: func(_ context.Context, s string) (frontend.QuickConnectDetails, error) {
			return frontend.QuickConnectDetails{Approved: s == secret, Code: "123456", Secret: s, Device: frontend.Device{ID: vars["device_id"], Client: "Jellyfin Android TV", DeviceName: "Android TV", Version: "0.18.2"}, CreatedAt: created}, nil
		},
		PollFunc: func(_ context.Context, s string) (string, error) {
			if s != secret {
				return "", frontend.ErrIdentityUnavailable
			}
			return token, nil
		},
	}
	inst, err := mount(context.Background(), frontend.Host{Identity: identity, Sessions: frontendtest.Sessions{}}, "conform", "jellyfin")
	if err != nil {
		t.Fatal(err)
	}
	expand := func(s string) string {
		for key, value := range vars {
			s = strings.ReplaceAll(s, "${"+key+"}", value)
		}
		return s
	}
	compared := map[string]bool{}
	for _, req := range p.Corpus(client) {
		flow := req.Name
		if !strings.HasPrefix(flow, "connect_") && !strings.HasPrefix(flow, "sign_in_") && !strings.HasPrefix(flow, "quick_connect_") {
			continue
		}
		name := p.FixtureName(client, req, true)
		reference, err := conform.LoadFixture(filepath.Join("..", "..", "..", "testdata", "conform", "jellyfin", client, name+".json"))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		r := httptest.NewRequest(req.Method, expand(req.Path), strings.NewReader(expand(string(req.Body))))
		for key, values := range req.Headers[client+"-json"] {
			for _, value := range values {
				r.Header.Add(key, expand(value))
			}
		}
		r.Header.Set("Authorization", r.Header.Get("Authorization")+`, Token="`+token+`"`)
		w := httptest.NewRecorder()
		inst.Handler().ServeHTTP(w, r)
		target := p.Redact(conform.Response{Status: w.Code, Headers: w.Header(), Body: w.Body.Bytes()})
		target = conform.RedactSecrets(target, token, secret, vars["device_id"], vars["user_id"], vars["username"])
		for _, d := range p.Compare(reference, target).Differences {
			if d.Blocking {
				t.Errorf("%s %s: %s %s (reference %v, ether %v)", req.Method, req.Path, d.Kind, d.Path, d.Reference, d.Target)
			}
		}
		compared[req.Method+" "+req.Path] = true
	}
	// A profile edit must not silently drop the flows #308 depends on.
	for _, want := range []string{"POST /Users/AuthenticateByName", "GET /Users/Me", "POST /QuickConnect/Initiate", "GET /QuickConnect/Connect?secret=${quick_connect_secret}", "POST /Users/AuthenticateWithQuickConnect"} {
		if !compared[want] {
			t.Errorf("recorded corpus lacks %s", want)
		}
	}
}
