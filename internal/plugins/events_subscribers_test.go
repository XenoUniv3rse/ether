package plugins

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"testing"
)

func TestPublishLogsOnlySubscribedEvents(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()
	count := func(table string) int {
		t.Helper()
		var n int
		if err := env.m.store.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+table).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	env.m.Publish(ctx, Event{Name: EventPlaybackStarted})
	if count("plugins_event_log") != 0 || count("plugins_outbox") != 0 {
		t.Fatal("event without subscribers wrote to storage")
	}
	rec := env.installHello(t, "1.0.0")
	env.m.Publish(ctx, Event{Name: EventPlaybackStarted})
	if count("plugins_event_log") != 1 || count("plugins_outbox") != 1 {
		t.Fatal("subscribed event was not logged and queued")
	}
	if err := env.m.SetEnabled(ctx, rec.ID, false); err != nil {
		t.Fatal(err)
	}
	env.m.Publish(ctx, Event{Name: EventPlaybackStarted})
	if count("plugins_event_log") != 1 || count("plugins_outbox") != 1 {
		t.Fatal("disabled subscriber caused a write")
	}
}

func TestEventSubscriptionsRestoredOnStart(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()
	env.installHello(t, "1.0.0")
	restarted, err := New(Deps{DB: env.m.store.db})
	if err != nil {
		t.Fatal(err)
	}
	if err := restarted.Start(ctx); err != nil {
		t.Fatal(err)
	}
	restarted.Publish(ctx, Event{Name: EventPlaybackStarted})
	var count int
	if err := restarted.store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM plugins_outbox`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("restored subscription: %d %v", count, err)
	}
}

func TestFolderGrantRootsAndPrivateEvents(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()
	install := func(id string, grants Grants) Record {
		t.Helper()
		raw := []byte(fmt.Sprintf(`{"id":%q,"name":"Folders","version":"1.0.0","ether":{"protocol":1,"kind":"extension","runtime":{"kind":"hosted","entry":"plugin.js"},"capabilities":["events"],"permissions":{"folders":"read"},"contributes":{"events":{"subscribe":[%q]}}}}`, id, EventPluginFoldersChanged))
		r, err := env.m.Install(ctx, &Preview{
			Manifest: mustParseTestManifest(t, raw), manifestRaw: raw,
		}, InstallOptions{Grants: grants})
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	other := install("com.example.other", Grants{})
	first, second := t.TempDir(), t.TempDir()
	owner := install("com.example.folders", Grants{Folders: []string{first}})
	h := env.m.HostFor(owner.ID)

	var delivered []FolderRootsChangedPayload
	env.fake.routes["/ether/event"] = func(req Request) (Response, error) {
		var batch struct{ Events []Event }
		if err := json.Unmarshal(req.Body, &batch); err != nil {
			return Response{}, err
		}
		ack := []string{}
		for _, ev := range batch.Events {
			var payload FolderRootsChangedPayload
			if ev.Name != EventPluginFoldersChanged {
				t.Fatalf("event name = %s", ev.Name)
			}
			if err := json.Unmarshal(ev.Payload, &payload); err != nil {
				return Response{}, err
			}
			delivered = append(delivered, payload)
			ack = append(ack, ev.ID)
		}
		body, _ := json.Marshal(map[string][]string{"ack": ack})
		return Response{Status: 200, Body: body}, nil
	}

	check := func(path string, want []string, changed bool) {
		t.Helper()
		roots, err := h.FoldersRoots(WithCallContext(ctx, path, ""))
		if err != nil || roots == nil || !slices.Equal(roots, want) {
			t.Fatalf("roots on %s = %v, %v, want %v", path, roots, err, want)
		}
		delivered = nil
		if err := env.m.DeliverOutbox(ctx, other.ID); err != nil || len(delivered) != 0 {
			t.Fatalf("unrelated subscriber received folder changes: %v, %v", delivered, err)
		}
		if err := env.m.DeliverOutbox(ctx, owner.ID); err != nil {
			t.Fatal(err)
		}
		if changed {
			if len(delivered) != 1 || delivered[0].Roots == nil || !slices.Equal(delivered[0].Roots, want) {
				t.Fatalf("folder events = %v, want one with roots %v", delivered, want)
			}
		} else if len(delivered) != 0 {
			t.Fatalf("unchanged grants queued events: %v", delivered)
		}
	}
	set := func(g Grants) {
		t.Helper()
		if err := env.m.SetGrants(ctx, owner.ID, g); err != nil {
			t.Fatal(err)
		}
	}

	check("/ether/job/scan", []string{first}, true)
	if _, err := h.FoldersRoots(WithCallContext(ctx, "/stream/movie/tmdb:1.json", "")); !errors.Is(err, ErrPermissionDenied) {
		t.Fatalf("client route: %v", err)
	}
	set(Grants{Folders: []string{first, second}})
	check("/ether/action/scan", []string{first, second}, true)
	set(Grants{Folders: []string{first}})
	check("/ether/event", []string{first}, true)
	set(Grants{})
	check("/ether/job/scan", nil, true)
	set(Grants{})
	check("/ether/job/scan", nil, false)
	set(Grants{Folders: []string{second}})
	check("/ether/job/scan", []string{second}, true)
	set(Grants{Folders: []string{second}, Revoked: []string{"folders"}})
	check("/ether/event", nil, true)
}
