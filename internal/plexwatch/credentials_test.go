package plexwatch

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

const fixtureJWT = "eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJzdWIiOiI0MiIsImV4cCI6NDEwMjQ0NDgwMH0.b2ZmbGluZS10ZXN0LXNpZ25hdHVyZS0zMi1ieXRlcyEhIQ"
const fixtureEnvelope = "qrnFIbOHg+dJLsOFPqyqsJ3QXpuvHl54a3raN2nHSnov1LalA7Trl+R7ekbV2TuKpzWTLIYes4HccLg1RYIJ501Fr6ZKv6Dm5xKODrZ5qhyubO66YN+yMEPRjLIh0E0uf8M3jpfwHpB4gup6gE6Lt2dGMdTEpk1n5lxIk/1QavA="

func TestServiceCredentialTransport(t *testing.T) {
	claim := "claim-one"
	lookups := 0
	posts := 0
	refuse := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v2/server/users/services" {
			lookups++
			if r.Header.Get("X-Plex-Product") != "Plex Media Server" {
				t.Error("Plex filters server services by product")
			}
			if r.Header.Get("X-Plex-Token") != claim || r.Header.Get("Authorization") != "" {
				t.Error("wrong service-lookup credential")
			}
			json.NewEncoder(w).Encode([]any{map[string]any{"id": 42, "services": []any{map[string]any{"identifier": "scrobbling", "endpoint": scrobbleOrigin, "status": "online", "token": fixtureEnvelope}}}})
			return
		}
		if r.Header.Get("Authorization") != "Bearer "+fixtureJWT || r.Header.Get("X-Plex-Token") != "" {
			t.Error("state request must use service Bearer only")
		}
		if r.Method == "POST" {
			posts++
		}
		if refuse {
			w.WriteHeader(401)
			return
		}
		if r.Method == "POST" {
			w.WriteHeader(201)
			return
		}
		w.Write([]byte(`{"items":[],"pageInfo":{}}`))
	}))
	defer server.Close()
	c, err := newClient(server.URL, "ether-id", nil, func() time.Time { return time.Date(2026, 11, 4, 0, 0, 0, 0, time.UTC) })
	if err != nil {
		t.Fatal(err)
	}
	c.serverIdentity = func() (string, string) { return claim, "ether-id" }
	for i := 0; i < 2; i++ {
		if _, err = c.fetchState(context.Background(), "viewer-pin", 42, ""); err != nil {
			t.Fatal(err)
		}
	}
	if lookups != 1 {
		t.Fatalf("lookups %d", lookups)
	}
	if _, err = c.fetchState(context.Background(), "viewer-pin", 99, ""); !errors.Is(err, ErrCredentialForbidden) {
		t.Fatalf("cross-account %v", err)
	}
	claim = "claim-two"
	if _, err = c.fetchState(context.Background(), "viewer-pin", 42, ""); err != nil {
		t.Fatal(err)
	}
	if lookups != 3 {
		t.Fatalf("claim failed to invalidate cache: %d", lookups)
	}
	refuse = true
	err = c.postState(context.Background(), "viewer-pin", 42, postItem{GUID: "plex://movie/offline"})
	if !errors.Is(err, ErrCredentialForbidden) || errors.Is(err, ErrRevoked) || posts != 1 {
		t.Fatalf("service rejection must neither revoke PIN nor replay POST: %v, posts %d", err, posts)
	}
	refuse = false
	if _, err = c.fetchState(context.Background(), "viewer-pin", 42, ""); err != nil {
		t.Fatal(err)
	}
	if lookups != 4 {
		t.Fatal("refused JWT reused")
	}
}
func TestServiceEnvelope(t *testing.T) {
	token, expiry, err := unwrapServiceToken(fixtureEnvelope)
	if err != nil || token != fixtureJWT || expiry.Unix() != 4102444800 {
		t.Fatal("fixture decode failed", err)
	}
	for _, bad := range []string{"", "!", "AAAA", fixtureEnvelope[:len(fixtureEnvelope)-4]} {
		if _, _, err := unwrapServiceToken(bad); err == nil {
			t.Fatal("accepted bad envelope")
		}
	}
}
