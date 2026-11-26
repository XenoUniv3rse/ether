package vela

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
)

// grantRecorder issues a grant only for plugin images of org.allowed and for
// channel ch-1, so the test sees both the signed and the dropped path.
type grantRecorder struct {
	frontend.ImageGrants
	plugins []string
}

func (g *grantRecorder) Issue(_ context.Context, req frontend.ImageGrantRequest) (string, error) {
	if !strings.HasPrefix(req.Image.Path, frontend.PluginImagePrefix) || req.Image.Subject != "org.allowed" {
		return "", core.ErrUnauthorized
	}
	g.plugins = append(g.plugins, req.Image.Subject)
	return "sealed-plugin", nil
}

func (g *grantRecorder) IssueChannelLogo(_ context.Context, _ string, _ core.Device, channelID string) (string, error) {
	if channelID != "ch-1" {
		return "", core.ErrUnauthorized
	}
	return "sealed-channel", nil
}

// The security invariant: no payload leaves with an upstream image URL. A
// logo Ether signs becomes a grant URL; one it cannot sign is removed.
func TestLiveImagesNeverLeakUpstreamURLs(t *testing.T) {
	grants := &grantRecorder{}
	i := &instance{host: frontend.Host{ImageGrants: grants}}
	r := httptest.NewRequest(http.MethodGet, "/vela/v0/live/events", nil)
	signer := i.newLiveImages(r, "token")

	payload := map[string]any{
		"events": []any{
			map[string]any{
				"id":     "org.allowed:sports:401",
				"status": map[string]any{"state": "in"},
				"league": map[string]any{"id": "nfl", "logo": "https://cdn.example.com/nfl.png"},
				"competitors": []any{
					map[string]any{"id": "a", "logo": "https://cdn.example.com/a.png"},
				},
				"watch": []any{
					map[string]any{"kind": "channel", "channelId": "ch-1", "logo": "http://provider.example/logo.png"},
					map[string]any{"kind": "channel", "channelId": "ch-hidden", "logo": "http://provider.example/x.png"},
					map[string]any{"kind": "addon", "logo": "https://addon.example/y.png"},
				},
			},
			map[string]any{
				"id":          "org.other:sports:9",
				"competitors": []any{map[string]any{"id": "b", "logo": "https://cdn.example.com/b.png"}},
			},
		},
		"entities": []any{map[string]any{"key": "org.allowed:sports:team:1", "logo": "https://cdn.example.com/t.png"}},
	}
	raw, err := signer.sign(payload, "")
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	if strings.Contains(text, "http://") || strings.Contains(text, "https://") {
		t.Fatalf("upstream URL leaked: %s", text)
	}
	var out struct {
		Events []struct {
			League      map[string]any   `json:"league"`
			Competitors []map[string]any `json:"competitors"`
			Watch       []map[string]any `json:"watch"`
		} `json:"events"`
		Entities []map[string]any `json:"entities"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	first := out.Events[0]
	if first.League["logo"] != "/vela/v0/images/sealed-plugin" || first.Competitors[0]["logo"] != "/vela/v0/images/sealed-plugin" {
		t.Fatalf("plugin logos not signed: %+v", first)
	}
	if first.Watch[0]["logo"] != "/vela/v0/images/sealed-channel" {
		t.Fatalf("channel logo not signed: %+v", first.Watch[0])
	}
	if _, ok := first.Watch[1]["logo"]; ok {
		t.Fatalf("logo of a channel the viewer cannot see survived: %+v", first.Watch[1])
	}
	if _, ok := first.Watch[2]["logo"]; ok {
		t.Fatalf("add-on target logo survived: %+v", first.Watch[2])
	}
	if _, ok := out.Events[1].Competitors[0]["logo"]; ok {
		t.Fatalf("logo of an unauthorized plugin survived: %+v", out.Events[1])
	}
	if out.Entities[0]["logo"] != "/vela/v0/images/sealed-plugin" {
		t.Fatalf("entity logo not signed: %+v", out.Entities[0])
	}
	// One grant per distinct crest per response.
	if len(grants.plugins) != 3 {
		t.Fatalf("issued %d plugin grants, want 3 (league, competitor, entity)", len(grants.plugins))
	}
}

type liveChannelSelectionFixture struct {
	frontend.LiveTV
	revision frontend.LiveChannelRevision
	channel  frontend.LiveChannel
	err      error
}

func (f *liveChannelSelectionFixture) ChannelSelectionRevision(context.Context) (frontend.LiveChannelRevision, error) {
	return f.revision, f.err
}

func (f *liveChannelSelectionFixture) ReadChannelSelection(_ context.Context, _ *frontend.Viewer, ids []string) ([]frontend.LiveChannel, error) {
	if f.channel.ID == "" || len(ids) != 1 || ids[0] != f.channel.ID {
		return nil, nil
	}
	return []frontend.LiveChannel{f.channel}, nil
}

func TestStreamChannelCardsFollowPolicyAndLineupRevisions(t *testing.T) {
	now := time.Unix(100, 0)
	live := &liveChannelSelectionFixture{
		revision: frontend.LiveChannelRevision{Policy: 1, Lineup: 1},
		channel:  frontend.LiveChannel{ID: "channel", Name: "Original"},
	}
	i := &instance{host: frontend.Host{LiveTV: live, Now: func() time.Time { return now }}}
	images := i.newLiveImages(httptest.NewRequest(http.MethodGet, "/", nil), "")
	images.viewer = &frontend.Viewer{ID: 1}
	images.channelCards = &streamChannelCards{}
	readName := func() string {
		_, cards := images.ChannelCards(t.Context(), []string{"channel"}, "")
		card, _ := cards["channel"].(object)
		name, _ := card["name"].(string)
		return name
	}
	checkName := func(want string) {
		t.Helper()
		if got := readName(); got != want {
			t.Fatalf("channel card name = %q, want %q", got, want)
		}
	}
	checkName("Original")

	live.channel.Name = "Updated"
	checkName("Original")
	live.revision.Lineup++
	now = now.Add(channelCardRevisionInterval)
	checkName("Updated")

	live.channel = frontend.LiveChannel{}
	live.revision.Policy++
	now = now.Add(channelCardRevisionInterval)
	checkName("")

	live.channel = frontend.LiveChannel{ID: "channel", Name: "Restored"}
	live.revision.Policy++
	now = now.Add(channelCardRevisionInterval)
	checkName("Restored")

	live.err = errors.New("revision unavailable")
	now = now.Add(channelCardRevisionInterval)
	checkName("")
}
