package vela

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/experience"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/frontendtest"
	"github.com/braedonsaunders/ether/internal/frontend/kit/ids"
	"github.com/braedonsaunders/ether/internal/personalize"
	"github.com/braedonsaunders/ether/internal/state"
	"github.com/braedonsaunders/ether/internal/store"
	"github.com/braedonsaunders/ether/internal/viewerpolicy"
)

type idleBootstrapExperience struct {
	frontendtest.Experience
	*experience.RowViewService
	db            *sql.DB
	staleBackfill bool
	revisionReads int
	churn         int
	revisionErr   error
}

func (f *idleBootstrapExperience) HomeRevision(ctx context.Context, _ *frontend.Viewer) (frontend.HomeRevision, bool, error) {
	if f.revisionErr != nil {
		return frontend.HomeRevision{}, false, f.revisionErr
	}
	f.revisionReads++
	epoch, err := viewerpolicy.Read(ctx, f.db)
	if f.churn > 0 {
		epoch += int64(f.churn)
		f.churn--
	}
	return frontend.HomeRevision{Viewer: "idle", Policy: epoch}, true, err
}
func (f *idleBootstrapExperience) ResolveRowView(ctx context.Context, q frontend.RowViewPageRequest) (frontend.RowViewPage, error) {
	if q.Offset > 0 && f.staleBackfill {
		f.staleBackfill = false
		return frontend.RowViewPage{}, frontend.ErrRowViewStale
	}
	return f.RowViewService.ResolveRowView(ctx, q)
}

type idleBootstrapImages struct{ frontend.ImageGrants }

func (idleBootstrapImages) SignPage(_ context.Context, q frontend.ImagePageRequest) ([]frontend.ImageBatchItem, error) {
	var out []frontend.ImageBatchItem
	for _, title := range q.Titles {
		out = append(out, frontend.ImageBatchItem{Item: title.Key, Images: []frontend.GrantedImage{{Secret: "image", Image: frontend.Image{Kind: frontend.ImageBackdrop, Path: fmt.Sprint(title.Key), Tag: "backdrop"}}}})
	}
	return out, nil
}

type watchBootstrapExperience struct{ idleBootstrapExperience }

func (f *watchBootstrapExperience) HomeRevision(ctx context.Context, v *frontend.Viewer) (frontend.HomeRevision, bool, error) {
	watch, err := experience.ReadWatchRevision(ctx, f.db, v.ID, 0)
	return frontend.HomeRevision{Viewer: fmt.Sprint(watch.Persisted)}, true, err
}

func TestVelaBootstrapRevalidatesCachedRowIDs(t *testing.T) {
	ctx := t.Context()
	db := store.OpenTest(t)
	now := time.Now().Truncate(time.Second)
	clock := func() time.Time { return now }
	active := func(_ context.Context, id int64) (*core.User, error) {
		return &core.User{ID: id, Username: "viewer"}, nil
	}
	receipts, err := experience.NewReceipts(experience.ReceiptOptions{DB: db, TTL: 30 * time.Minute, Now: clock, MaxPerDevice: 500, Active: active,
		Allowed: func(context.Context, *core.User, core.Device, core.Protocol, experience.ReceiptSubject) bool {
			return true
		},
		AllowedAction: func(context.Context, *core.User, core.Device, core.Protocol, experience.ReceiptSubject, frontend.RowInteractionKind) bool {
			return true
		},
		Record: func(context.Context, *core.User, core.Device, core.Protocol, experience.ReceiptSubject, frontend.RowInteractionKind) error {
			return nil
		}})
	if err != nil {
		t.Fatal(err)
	}
	views, err := experience.NewRowViewService(experience.RowViewOptions{DB: db, TTL: 15 * time.Minute, Now: clock, Active: active, Receipts: receipts,
		Build: func(ctx context.Context, q frontend.RowViewRequest) ([]experience.RowViewSnapshot, error) {
			watch, err := experience.ReadWatchRevision(ctx, db, q.Viewer.ID, 0)
			snap := experience.RowViewSnapshot{Placement: q.Placement, Revision: fmt.Sprint(watch.Persisted),
				Row: frontend.Row{ID: "new", Kind: "trending", Title: "New & Noteworthy", Purpose: frontend.RowRecommendation}}
			for n := int64(1); n <= 10; n++ {
				snap.Entries = append(snap.Entries, core.NeutralEntryRef{Kind: "title", ID: core.MovieKey(n).String()})
			}
			return []experience.RowViewSnapshot{snap}, err
		},
		ValidRow: func(ctx context.Context, v *core.User, _ core.Device, _ core.Protocol, snap experience.RowViewSnapshot) bool {
			watch, err := experience.ReadWatchRevision(ctx, db, v.ID, 0)
			return err == nil && snap.Revision == fmt.Sprint(watch.Persisted)
		},
		ResolveEntry: func(_ context.Context, _ *core.User, _ core.Device, _ core.Protocol, ref core.NeutralEntryRef) (frontend.RowEntry, error) {
			key, err := core.ParseRatingKey(ref.ID)
			return frontend.RowEntry{Kind: frontend.RowEntryTitle, Title: &core.Title{Key: key, Title: fmt.Sprint(key)}}, err
		}})
	if err != nil {
		t.Fatal(err)
	}
	source := &watchBootstrapExperience{idleBootstrapExperience{RowViewService: views, db: db}}
	mounted, err := (Frontend{DB: db, ReplayKey: make([]byte, 32)}).Mount(ctx, frontend.Host{Identity: identityFake{}, Experience: source, Sessions: frontendtest.Sessions{}, ImageGrants: idleBootstrapImages{}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = mounted.Close(context.Background()) })
	get := func(path, etag string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/vela/v0/"+path, nil)
		req.Header.Set("Authorization", "Bearer token")
		req.Header.Set("X-Vela-Device-Id", "device")
		req.Header.Set("If-None-Match", etag)
		out := httptest.NewRecorder()
		mounted.Handler().ServeHTTP(out, req)
		return out
	}
	home := func(etag, previous string) (id, tag string) {
		t.Helper()
		out := get("bootstrap", etag)
		if out.Code != http.StatusOK {
			t.Fatalf("bootstrap status = %d, want 200", out.Code)
		}
		var wire struct{ Rows []struct{ ID string } }
		if err := json.Unmarshal(out.Body.Bytes(), &wire); err != nil || len(wire.Rows) != 1 || wire.Rows[0].ID == "" || wire.Rows[0].ID == previous {
			t.Fatalf("bootstrap did not publish a new row id: %v", err)
		}
		id, tag = wire.Rows[0].ID, out.Header().Get("ETag")
		if tag == "" || tag == etag {
			t.Fatal("bootstrap did not publish a new ETag")
		}
		if row := get("rows/"+id, ""); row.Code != http.StatusOK {
			t.Fatalf("published row status = %d, want 200", row.Code)
		}
		return id, tag
	}
	rejected := func(id string) {
		t.Helper()
		out := get("rows/"+id, "")
		if out.Code != http.StatusConflict || !strings.Contains(out.Body.String(), `"code":"cursor_expired"`) {
			t.Fatalf("old row status = %d, want 409 cursor_expired", out.Code)
		}
	}

	id, tag := home("", "")
	now = now.Add(12 * time.Minute)
	watch := state.New(db, state.Options{Now: clock})
	if err := watch.Scrobble(ctx, 1, core.MovieKey(1)); err != nil {
		t.Fatal(err)
	}
	rejected(id)
	// Library lists the same logical shelf, with its own placement-scoped visit.
	if out := get("rows?placement=library", ""); out.Code != http.StatusOK {
		t.Fatalf("library status = %d", out.Code)
	}
	id, tag = home(tag, id)
	if out := get("bootstrap", tag); out.Code != http.StatusNotModified {
		t.Fatalf("current cached bootstrap status = %d, want 304", out.Code)
	}

	// Advance storage past the visit deadline while the wire cache is still
	// warm, then verify a refresh within the same watch revision too.
	now = now.Add(15 * time.Minute)
	rejected(id)
	id, tag = home(tag, id)
	if _, err := views.ListRowViews(ctx, frontend.RowViewRequest{Viewer: &core.User{ID: 1}, Device: core.Device{ID: "device"}, Protocol: "vela", Placement: frontend.PlacementHome, Refresh: true}); err != nil {
		t.Fatal(err)
	}
	rejected(id)
	_, tag = home(tag, id)
	if out := get("bootstrap", tag); out.Code != http.StatusNotModified {
		t.Fatalf("replacement bootstrap status = %d, want 304", out.Code)
	}
}

// An idle bootstrap reuses an immutable, authorized visit and its receipts.
// Exercise real persistence/issuance, stale manifests and hero backfill through
// the mounted HTTP handler; fake only identity, title source and artwork.
func TestVelaIdleBootstrap(t *testing.T) {
	db := store.OpenTest(t)
	active := func(_ context.Context, id int64) (*core.User, error) {
		return &core.User{ID: id, Username: "viewer"}, nil
	}
	receipts, err := experience.NewReceipts(experience.ReceiptOptions{DB: db, TTL: 30 * time.Minute, MaxPerDevice: 100, Active: active,
		Allowed: func(context.Context, *core.User, core.Device, core.Protocol, experience.ReceiptSubject) bool {
			return true
		},
		AllowedAction: func(context.Context, *core.User, core.Device, core.Protocol, experience.ReceiptSubject, frontend.RowInteractionKind) bool {
			return true
		},
		Record: func(context.Context, *core.User, core.Device, core.Protocol, experience.ReceiptSubject, frontend.RowInteractionKind) error {
			return nil
		}})
	if err != nil {
		t.Fatal(err)
	}
	builds := 0
	valid := true
	views, err := experience.NewRowViewService(experience.RowViewOptions{DB: db, TTL: 15 * time.Minute, Active: active, Receipts: receipts,
		Build: func(_ context.Context, q frontend.RowViewRequest) ([]experience.RowViewSnapshot, error) {
			builds++
			snap := experience.RowViewSnapshot{Row: frontend.Row{ID: "trending", Kind: "trending", Title: "Trending", Purpose: frontend.RowRecommendation}, Placement: q.Placement}
			for n := 1; n <= 30; n++ {
				snap.Entries = append(snap.Entries, core.NeutralEntryRef{Kind: "title", ID: core.MovieKey(int64(n)).String()})
			}
			return []experience.RowViewSnapshot{snap}, nil
		},
		ValidRow: func(context.Context, *core.User, core.Device, core.Protocol, experience.RowViewSnapshot) bool {
			return valid
		},
		ResolveEntry: func(_ context.Context, _ *core.User, _ core.Device, _ core.Protocol, ref core.NeutralEntryRef) (frontend.RowEntry, error) {
			key, err := core.ParseRatingKey(ref.ID)
			return frontend.RowEntry{Kind: frontend.RowEntryTitle, Title: &core.Title{Key: key, Title: fmt.Sprint(key)}}, err
		}})
	if err != nil {
		t.Fatal(err)
	}
	// A live manifest referencing an expired row used to reject a new listing.
	if _, err = views.ListRowViews(context.Background(), frontend.RowViewRequest{Viewer: &core.User{ID: 1}, Device: core.Device{ID: "device"}, Protocol: "vela", Placement: frontend.PlacementHome}); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`UPDATE experience_row_views SET expires=0`); err != nil {
		t.Fatal(err)
	}
	source := &idleBootstrapExperience{RowViewService: views, db: db, staleBackfill: true}
	mounted, err := (Frontend{DB: db, ReplayKey: make([]byte, 32)}).Mount(context.Background(), frontend.Host{Identity: identityFake{}, Experience: source, Sessions: frontendtest.Sessions{}, ImageGrants: idleBootstrapImages{}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = mounted.Close(context.Background()) })
	inst := mounted.(*instance)
	get := func() ([]byte, *store.QueryStats) {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, "/vela/v0/bootstrap", nil)
		req.Header.Set("Authorization", "Bearer token")
		req.Header.Set("X-Vela-Device-Id", "device")
		ctx, stats := store.WithQueryStats(req.Context())
		w := httptest.NewRecorder()
		mounted.Handler().ServeHTTP(w, req.WithContext(ctx))
		if w.Code != 200 {
			t.Fatalf("bootstrap %d: %s", w.Code, w.Body.String())
		}
		var wire struct{ Heroes []json.RawMessage }
		if err := json.Unmarshal(w.Body.Bytes(), &wire); err != nil || len(wire.Heroes) != 6 {
			t.Fatalf("heroes: %s, %v", w.Body, err)
		}
		return append([]byte(nil), w.Body.Bytes()...), stats
	}
	first, _ := get()
	second, warm := get()
	if string(first) != string(second) {
		t.Fatal("idle hero, receipts or cursors changed")
	}
	if warm.Writes() != 0 {
		t.Fatalf("idle writes=%d", warm.Writes())
	}
	// Wire-grant refresh must also reuse live page receipts rather than fill
	// the device quota. The response can carry newly sealed image/cursor grants.
	inst.bootstrapMu.Lock()
	for _, entry := range inst.bootstraps {
		entry.expires = time.Now().Add(-time.Second)
	}
	inst.bootstrapMu.Unlock()
	_, renewed := get()
	if renewed.Writes() != 0 {
		t.Fatalf("page revisit wrote %d statements", renewed.Writes())
	}
	var count int
	if err = db.QueryRow(`SELECT COUNT(*) FROM experience_receipts`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 49 {
		t.Fatalf("idle receipt count=%d, want 21 before stale backfill + 28 on rebuilt visit", count)
	}
	if builds != 3 {
		t.Fatalf("manifest builds=%d want seed, stale replacement, backfill replacement", builds)
	}
	// A durable policy change must invalidate even an otherwise live wire body.
	valid = false
	if _, err = db.Exec(`UPDATE viewer_policy_epoch SET generation=generation+1 WHERE id=1`); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/vela/v0/bootstrap", nil)
	req.Header.Set("Authorization", "Bearer token")
	req.Header.Set("X-Vela-Device-Id", "device")
	denied := httptest.NewRecorder()
	mounted.Handler().ServeHTTP(denied, req)
	// A revoked row is dropped from Home; nothing it carried may be served.
	if body := denied.Body.String(); strings.Contains(body, `"heroes"`) || (denied.Code == 200 && !strings.Contains(body, `"rows":[]`)) {
		t.Fatal("cached Home survived a policy revocation", denied.Code, body)
	}

	// A moving revision is retried locally; continuous churn has a bounded,
	// explicit client retry contract and never publishes a mixed snapshot.
	valid = true
	for _, churn := range []int{2, 100} {
		inst.bootstraps = nil
		source.churn, source.revisionReads = churn, 0
		out := httptest.NewRecorder()
		mounted.Handler().ServeHTTP(out, req)
		if churn == 2 && out.Code != 200 {
			t.Fatalf("settling revision: %d %s", out.Code, out.Body)
		}
		if churn == 100 && (out.Code != 409 || out.Header().Get("Retry-After") != "1" || !strings.Contains(out.Body.String(), `"code":"bootstrap_changed"`) || !strings.Contains(out.Body.String(), `"allowed":true`)) {
			t.Fatalf("continuous revision: %d %s", out.Code, out.Body)
		}
		if source.revisionReads > 2*bootstrapAttempts {
			t.Fatalf("unbounded assembly: %d revision reads", source.revisionReads)
		}
	}
	source.churn = 0

	// Unexpected failures must give the owner one useful, sanitized WARN.
	valid = true
	source.revisionErr = fmt.Errorf("bootstrap revision: %w", errors.New("failed https://example.invalid/private?token=hidden token=hidden Bearer token"))
	var logs bytes.Buffer
	prior := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
	defer slog.SetDefault(prior)
	failed := httptest.NewRecorder()
	mounted.Handler().ServeHTTP(failed, req)
	if failed.Code != 503 {
		t.Fatalf("unexpected failure status=%d", failed.Code)
	}
	line := logs.String()
	if strings.Count(line, `"level":"WARN"`) != 1 || !strings.Contains(line, `GET /vela/v0/bootstrap`) || !strings.Contains(line, `error_class`) || !strings.Contains(line, `error_chain`) {
		t.Fatalf("missing request warning: %s", line)
	}
	if strings.Contains(line, "https://") || strings.Contains(line, "example.invalid") || strings.Contains(line, "hidden") || strings.Contains(line, "Bearer token") {
		t.Fatalf("unsafe warning: %s", line)
	}
}

func TestVelaBootstrapMapsTransientNotAvailableTo503Retryable(t *testing.T) {
	ctx := context.Background()
	db := store.OpenTest(t)
	for _, tc := range []struct {
		name string
		err  error
	}{
		{name: "bare_not_available", err: frontend.ErrNotAvailable},
		{name: "preparing", err: &frontend.PreparingError{RetryAfter: 3 * time.Second, Err: frontend.ErrNotAvailable}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			exp := frontendtest.Experience{
				RowsFunc: func(context.Context, frontend.RowRequest) ([]frontend.Row, error) {
					return nil, tc.err
				},
			}
			mounted, err := (Frontend{DB: db, ReplayKey: make([]byte, 32)}).Mount(ctx, frontend.Host{
				Identity:    identityFake{},
				Experience:  exp,
				Sessions:    frontendtest.Sessions{},
				ImageGrants: idleBootstrapImages{},
			})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = mounted.Close(ctx) })
			req := httptest.NewRequest(http.MethodGet, "/vela/v0/bootstrap", nil)
			req.Header.Set("Authorization", "Bearer token")
			req.Header.Set("X-Vela-Device-Id", "device")
			rec := httptest.NewRecorder()
			mounted.Handler().ServeHTTP(rec, req)
			if rec.Code != http.StatusServiceUnavailable {
				t.Fatalf("status = %d, want 503; body=%s", rec.Code, rec.Body.String())
			}
			if rec.Header().Get("Retry-After") == "" {
				t.Fatalf("missing Retry-After header; body=%s", rec.Body.String())
			}
			var payload struct {
				Error struct {
					Code  string `json:"code"`
					Retry struct {
						Allowed      bool `json:"allowed"`
						AfterSeconds int  `json:"afterSeconds"`
					} `json:"retry"`
				} `json:"error"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
				t.Fatalf("decode error payload: %v (%s)", err, rec.Body.String())
			}
			if !payload.Error.Retry.Allowed || payload.Error.Retry.AfterSeconds <= 0 {
				t.Fatalf("expected retryable error payload, got %+v (%s)", payload.Error, rec.Body.String())
			}
		})
	}
}

type bootstrapCardHeaders struct{ core.Collections }

func (bootstrapCardHeaders) CollectionCardHeaders(_ context.Context, _ *core.User, keys []core.RatingKey) (map[core.RatingKey]*core.Collection, error) {
	out := make(map[core.RatingKey]*core.Collection, len(keys))
	for _, key := range keys {
		out[key] = &core.Collection{Key: key, Title: "Services", ChildCount: 2}
	}
	return out, nil
}

func (bootstrapCardHeaders) CollectionCardPage(_ context.Context, _ *core.User, _ core.RatingKey, after int64, _, limit int) ([]core.CollectionCardMember, string, error) {
	members := []core.CollectionCardMember{{Key: core.MovieKey(1), Position: 0}, {Key: core.ShowKey(1), Position: 1}}
	start := int(after + 1)
	return members[start:min(start+limit, len(members))], "revision", nil
}

type bootstrapCardExperience struct {
	frontendtest.Experience
	service *experience.Service
}

func (s bootstrapCardExperience) OpenCollectionCard(ctx context.Context, viewer *frontend.Viewer, key frontend.RatingKey, after int64, offset, limit int) (frontend.CollectionCardPage, error) {
	return s.service.OpenCollectionCard(ctx, viewer, key, after, offset, limit)
}

type bootstrapCardImages struct {
	frontend.ImageGrants
	source *experience.Service
}

func (s bootstrapCardImages) SignPage(ctx context.Context, q frontend.ImagePageRequest) ([]frontend.ImageBatchItem, error) {
	if !frontend.CollectionCardArtwork(ctx) {
		return nil, nil
	}
	var keys []core.RatingKey
	for _, title := range q.Titles {
		if title.Key == core.SmartKey(3) {
			return nil, frontend.ErrNotAvailable
		}
		keys = append(keys, title.Key)
	}
	headers, err := s.source.PageArtworkSmartCollections(ctx, &core.User{ID: 1}, keys)
	if err != nil {
		return nil, err
	}
	out := make([]frontend.ImageBatchItem, 0, len(headers))
	for key := range headers {
		out = append(out, frontend.ImageBatchItem{Item: key, Images: []frontend.GrantedImage{
			{Image: frontend.Image{Kind: frontend.ImagePoster, Path: "poster"}, Secret: "image-grant"},
		}})
	}
	return out, nil
}

func TestBootstrapCollectionCardsThroughArtworkPolicy(t *testing.T) {
	db := store.OpenTest(t)
	get := func(context.Context, *core.User, core.RatingKey) (*core.Title, error) {
		return nil, core.ErrNotFound
	}
	service, err := experience.NewService(experience.ServiceDeps{
		Protocol: "vela", Active: func(context.Context, int64) (*core.User, error) { return &core.User{ID: 1}, nil },
		Get: get, Children: func(context.Context, *core.User, core.RatingKey) ([]core.Title, error) { return nil, nil },
		State: state.New(db, state.Options{}), Rows: &experience.RowsService{},
		Search: experience.Search{Catalog: func(context.Context, string, []core.MediaType, int) ([]core.Title, error) { return nil, nil }},
		Visibility: experience.Visibility{
			GetTitle:   func(context.Context, core.RatingKey) (*core.Title, error) { return nil, core.ErrNotFound },
			SectionFor: func(core.MediaType) (int, bool) { return 1, true },
		},
		CachedGetMany: func(context.Context, []core.RatingKey) (map[core.RatingKey]core.Title, error) {
			return map[core.RatingKey]core.Title{
				core.MovieKey(1): {Key: core.MovieKey(1), Type: core.TypeMovie, Title: "Movie"},
				core.ShowKey(1):  {Key: core.ShowKey(1), Type: core.TypeShow, Title: "Show"},
			}, nil
		},
		Collections: personalize.NewDiscovery(db, nil, nil, nil).ReviewCollections(bootstrapCardHeaders{}),
	})
	if err != nil {
		t.Fatal(err)
	}
	keys := []core.RatingKey{core.SmartKey(1), core.SmartKey(2)}
	source := bootstrapCardExperience{service: service, Experience: frontendtest.Experience{RowsFunc: func(_ context.Context, q frontend.RowRequest) ([]frontend.Row, error) {
		if q.HomeFallbackOnly {
			return nil, nil
		}
		row := frontend.Row{ID: "home.rsvclogos", Kind: "collection-cards", Title: "Services", Purpose: frontend.RowCollection}
		for _, key := range keys {
			row.Entries = append(row.Entries, frontend.RowEntry{Kind: frontend.RowEntryCollection, Collection: &core.Collection{Key: key, Title: "Services", ChildCount: 2}})
		}
		broken := row
		broken.ID = "home.rbroken"
		broken.Entries = []frontend.RowEntry{{Kind: frontend.RowEntryCollection, Collection: &core.Collection{Key: core.SmartKey(3)}}}
		return []frontend.Row{broken, row}, nil
	}}}
	mounted, err := (Frontend{DB: db, ReplayKey: make([]byte, 32)}).Mount(t.Context(), frontend.Host{
		Identity: identityFake{}, Experience: source, Sessions: frontendtest.Sessions{}, ImageGrants: bootstrapCardImages{source: service},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = mounted.Close(context.Background()) })
	getPage := func(path string, out any) {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, "/vela/v0/"+path, nil)
		req.Header.Set("Authorization", "Bearer token")
		req.Header.Set("X-Vela-Device-Id", "device")
		rec := httptest.NewRecorder()
		mounted.Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", rec.Code)
		}
		if err := json.Unmarshal(rec.Body.Bytes(), out); err != nil {
			t.Fatal(err)
		}
	}
	var wire struct {
		Rows []struct {
			Role  string
			Items []struct {
				TargetID, Name, Kind string
				Artwork              []json.RawMessage
			}
		}
	}
	getPage("bootstrap", &wire)
	if len(wire.Rows) != 1 || wire.Rows[0].Role != "collection" || len(wire.Rows[0].Items) != len(keys) {
		t.Fatal("bootstrap did not publish the collection shelf")
	}
	for n, item := range wire.Rows[0].Items {
		if item.TargetID != ids.RatingKeyGUID(keys[n]) || item.Kind != "collection" || item.Name != "Services" || len(item.Artwork) != 1 {
			t.Fatalf("collection card %d is missing its target, name or artwork", n)
		}
	}
	var opened struct {
		Collection               struct{ ID string }
		Items                    []struct{ ID string }
		Continuation, NextCursor string
	}
	target := wire.Rows[0].Items[1].TargetID
	path := "collections/" + target + "?limit=1"
	getPage(path, &opened)
	if opened.Collection.ID != target || opened.Continuation != "more" || opened.NextCursor == "" ||
		len(opened.Items) != 1 || opened.Items[0].ID != ids.RatingKeyGUID(core.MovieKey(1)) {
		t.Fatal("collection target did not open at its first item")
	}
	getPage(path+"&cursor="+opened.NextCursor, &opened)
	if opened.Continuation != "exhausted" || len(opened.Items) != 1 || opened.Items[0].ID != ids.RatingKeyGUID(core.ShowKey(1)) {
		t.Fatal("collection did not continue to its show")
	}
}
