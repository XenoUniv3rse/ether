package identity_test

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/frontendtest"
	"github.com/braedonsaunders/ether/internal/frontend/kit/authhdr"
	"github.com/braedonsaunders/ether/internal/frontend/kit/ids"
	"github.com/braedonsaunders/ether/internal/identity"
	"github.com/braedonsaunders/ether/internal/store"
)

type channelArtworkFixture struct {
	frontendtest.Artwork
	visible       map[string][]frontend.Image
	blockedViewer map[int64]bool
}

func (f *channelArtworkFixture) List(_ context.Context, req frontend.ArtworkRequest) ([]frontend.Image, bool, error) {
	if req.Channel == "" {
		return nil, false, core.ErrNotFound
	}
	if req.Viewer != nil && f.blockedViewer[req.Viewer.ID] {
		return nil, false, core.ErrUnauthorized
	}
	images, ok := f.visible[req.Channel]
	if !ok {
		return nil, false, core.ErrUnauthorized
	}
	if req.Programme != "" {
		var filtered []frontend.Image
		for _, img := range images {
			if img.Subject == req.Programme {
				filtered = append(filtered, img)
			}
		}
		return filtered, true, nil
	}
	return images, true, nil
}

func (f *channelArtworkFixture) Read(_ context.Context, req frontend.ArtworkRequest, img frontend.Image, _ frontend.ArtworkTransform) ([]byte, string, error) {
	return []byte("<svg xmlns=\"http://www.w3.org/2000/svg\"/>"), "image/svg+xml", nil
}

func TestChannelImageGrantsStatelessDeterministicAndScoped(t *testing.T) {
	ctx := context.Background()
	db := store.OpenTest(t)
	if _, err := db.ExecContext(ctx, `INSERT INTO auth_users(id,plex_id,created_at,updated_at) VALUES(1,123,0,0),(2,456,0,0)`); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 11, 18, 18, 0, 10, 0, time.UTC)
	clock := func() time.Time { return now }
	grants, err := identity.NewGrants(db, members{db}, 48*time.Hour, clock)
	if err != nil {
		t.Fatal(err)
	}
	device := core.Device{ID: "tv-1", Client: "Vela", Product: "Vela"}
	velaCred, err := grants.Issue(ctx, 1, nil, "vela", device)
	if err != nil {
		t.Fatal(err)
	}
	velaViewer2Cred, err := grants.Issue(ctx, 2, nil, "vela", core.Device{ID: "tv-2"})
	if err != nil {
		t.Fatal(err)
	}
	jfCred, err := grants.Issue(ctx, 1, nil, "jellyfin", device)
	if err != nil {
		t.Fatal(err)
	}

	artwork := &channelArtworkFixture{
		visible:       map[string][]frontend.Image{},
		blockedViewer: map[int64]bool{2: true},
	}
	for i := 0; i < 5000; i++ {
		chID := fmt.Sprintf("ch-%d", i)
		artwork.visible[chID] = []frontend.Image{
			{Kind: frontend.ImageChannel, Path: fmt.Sprintf("logo-%d", i), Tag: "v1"},
		}
	}
	artwork.visible["ch-a"] = []frontend.Image{
		{Kind: frontend.ImageChannel, Path: "logo-a", Tag: "v1"},
		{Kind: frontend.ImageStill, Path: "prog-a", Tag: "v1", Subject: "airing-a"},
	}
	artwork.visible["ch-b"] = []frontend.Image{
		{Kind: frontend.ImageChannel, Path: "logo-b", Tag: "v1"},
	}

	makeSvc := func(surface frontend.Protocol) *identity.ImageGrantService {
		svc, err := identity.NewArtworkImageGrantService(
			db,
			surface,
			grants,
			func() frontend.Experience { return frontendtest.Experience{} },
			artwork,
			5*time.Minute,
			clock,
			identity.ImageGrantOptions{Token: authhdr.MediaBrowserToken},
		)
		if err != nil {
			t.Fatal(err)
		}
		svc.SetImageKey([]byte("test-node-image-key"))
		return svc
	}
	velaSvc := makeSvc("vela")
	jfSvc := makeSvc("jellyfin")

	// 1. Issue 5,000 channel grants for one viewer: no cap, no DB rows, table dropped.
	for i := 0; i < 5000; i++ {
		chID := fmt.Sprintf("ch-%d", i)
		secret, err := velaSvc.Issue(ctx, frontend.ImageGrantRequest{
			Credential: velaCred,
			Device:     device,
			Channel:    chID,
			Image:      artwork.visible[chID][0],
		})
		if err != nil || secret == "" {
			t.Fatalf("issue %d: secret=%q err=%v", i, secret, err)
		}
	}
	if _, err := db.ExecContext(ctx, `SELECT 1 FROM frontend_image_grants`); err == nil {
		t.Fatal("retired frontend_image_grants table still exists")
	}
	var headerRows int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM capability_headers WHERE namespace='image'`).Scan(&headerRows); err != nil {
		t.Fatal(err)
	}
	if headerRows != 0 {
		t.Fatalf("expected zero persisted image header rows, got %d", headerRows)
	}

	// 2. Stable URL across time and devices; changes only when image Tag or Path changes.
	reqA := frontend.ImageGrantRequest{
		Credential: velaCred,
		Device:     device,
		Channel:    "ch-a",
		Image:      artwork.visible["ch-a"][0],
	}
	sec1, err := velaSvc.Issue(ctx, reqA)
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(24 * time.Hour)
	reqOtherDevice := reqA
	reqOtherDevice.Device.ID = "mobile-1"
	sec2, err := velaSvc.Issue(ctx, reqOtherDevice)
	if err != nil {
		t.Fatal(err)
	}
	if sec1 != sec2 {
		t.Fatalf("expected stable channel secret across time and devices: %q != %q", sec1, sec2)
	}

	// 3. Served image for visible channel returns 200 with a validator for the mutable logo and a long-lived grant.
	serveReq := func(svc *identity.ImageGrantService, r *http.Request, secret string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		svc.Serve(w, r, secret)
		return w
	}
	serve := func(svc *identity.ImageGrantService, path, secret string) int {
		return serveReq(svc, httptest.NewRequest("GET", path, nil), secret).Code
	}
	resp := serveReq(velaSvc, httptest.NewRequest("GET", "/vela/v0/images/"+sec1, nil), sec1)
	if resp.Code != 200 || resp.Header().Get("Cache-Control") != "private, no-cache" || resp.Header().Get("ETag") == "" {
		t.Fatalf("expected 200 with revalidating cache headers, got %d cc=%q etag=%q", resp.Code, resp.Header().Get("Cache-Control"), resp.Header().Get("ETag"))
	}

	// Changing image tag mints a new URL and denies the old URL.
	artwork.visible["ch-a"][0].Tag = "v2"
	if code := serve(velaSvc, "/vela/v0/images/"+sec1, sec1); code != 404 {
		t.Fatalf("expected 404 when channel logo tag changed, got %d", code)
	}
	reqV2 := reqA
	reqV2.Image = artwork.visible["ch-a"][0]
	secV2, err := velaSvc.Issue(ctx, reqV2)
	if err != nil || secV2 == sec1 {
		t.Fatalf("expected new secret when channel logo tag changed: old=%q new=%q err=%v", sec1, secV2, err)
	}
	if code := serve(velaSvc, "/vela/v0/images/"+secV2, secV2); code != 200 {
		t.Fatalf("expected 200 for updated tag secret, got %d", code)
	}
	artwork.visible["ch-a"][0].Tag = "v1"

	// 4. Grant for channel A fails (404) for channel B's image, unlisted image, tampered HMAC, and another viewer.
	unlisted := reqA
	unlisted.Image = artwork.visible["ch-b"][0]
	if _, err := velaSvc.Issue(ctx, unlisted); !errors.Is(err, core.ErrUnauthorized) {
		t.Fatalf("expected unauthorized issuing channel B image under channel A, got %v", err)
	}
	raw, err := base64.RawURLEncoding.DecodeString(sec1)
	if err != nil {
		t.Fatal(err)
	}
	raw[0] ^= 0xff
	if code := serve(velaSvc, "/vela/v0/images/tampered", base64.RawURLEncoding.EncodeToString(raw)); code != 404 {
		t.Fatalf("expected 404 for tampered HMAC, got %d", code)
	}
	viewer2Req := httptest.NewRequest("GET", "/vela/v0/images/"+sec1, nil)
	viewer2Req.Header.Set("Authorization", "Bearer "+velaViewer2Cred)
	if code := serveReq(velaSvc, viewer2Req, sec1).Code; code != 404 {
		t.Fatalf("expected 404 when viewer 2 requests viewer 1 channel URL, got %d", code)
	}

	jfSecA, err := jfSvc.Issue(ctx, frontend.ImageGrantRequest{
		Credential: jfCred,
		Device:     device,
		Channel:    "ch-a",
		Image:      artwork.visible["ch-a"][0],
	})
	if err != nil {
		t.Fatal(err)
	}
	guidA, _ := ids.StringGUID(ids.Channel, "ch-a")
	guidB, _ := ids.StringGUID(ids.Channel, "ch-b")
	if code := serve(jfSvc, "/Items/"+guidA+"/Images/Primary", jfSecA); code != 200 {
		t.Fatalf("expected 200 on matching channel GUID, got %d", code)
	}
	if code := serve(jfSvc, "/Items/"+guidB+"/Images/Primary", jfSecA); code != 404 {
		t.Fatalf("expected 404 serving channel A grant on channel B path, got %d", code)
	}
	if code := serve(jfSvc, "/Items/"+ids.RatingKeyGUID(core.MovieKey(7))+"/Images/Primary", jfSecA); code != 404 {
		t.Fatalf("expected 404 serving channel A grant on library title path, got %d", code)
	}

	// 5. Channel hidden after issue returns 404.
	savedA := artwork.visible["ch-a"]
	delete(artwork.visible, "ch-a")
	if code := serve(velaSvc, "/vela/v0/images/"+sec1, sec1); code != 404 {
		t.Fatalf("expected 404 when channel hidden after issue, got %d", code)
	}
	artwork.visible["ch-a"] = savedA

	// 6. Advancing past session credential TTL still serves stateless image URL while viewer is active;
	// Revoke is a documented no-op; viewer deactivation returns 404.
	now = now.Add(365 * 24 * time.Hour)
	if err := velaSvc.Revoke(ctx, velaCred, sec1); err != nil {
		t.Fatal(err)
	}
	if code := serve(velaSvc, "/vela/v0/images/"+sec1, sec1); code != 200 {
		t.Fatalf("expected stateless URL to remain valid with no expiry, got %d", code)
	}
	if _, err := db.ExecContext(ctx, `UPDATE auth_users SET active=0 WHERE id=1`); err != nil {
		t.Fatal(err)
	}
	if code := serve(velaSvc, "/vela/v0/images/"+sec1, sec1); code != 404 {
		t.Fatalf("expected 404 after viewer deactivation, got %d", code)
	}
}
