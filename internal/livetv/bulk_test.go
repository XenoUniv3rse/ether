package livetv

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestChannelBatchAtomicAndPreservesFields(t *testing.T) {
	s := testService(t, Options{})
	ctx := context.Background()
	ids := make([]string, 701)
	tx, err := s.o.DB.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	for i := range ids {
		ids[i] = fmt.Sprintf("channel-%04d", i)
		c := testChannel()
		c.ID = ids[i]
		c.StreamURL = "https://provider.invalid/private"
		c.Private = true
		b, _ := json.Marshal(c)
		if _, err := tx.ExecContext(ctx, "INSERT INTO livetv_channels(id,data) VALUES(?,?)", c.ID, b); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	fav := true
	// The missing ID is in the final SQL chunk. Earlier chunks must roll back.
	if _, err := s.UpdateChannels(ctx, ChannelBatch{IDs: append(append([]string{}, ids...), "zzz-missing"), Favorite: &fav}); err == nil {
		t.Fatal("missing channel accepted")
	}
	c, err := s.Channel(ctx, ids[0])
	if err != nil || c.Favorite {
		t.Fatalf("partial update: %+v %v", c, err)
	}
	bumps := 0
	s.SetOnChange(func() { bumps++ })
	n, err := s.UpdateChannels(ctx, ChannelBatch{IDs: append(append([]string{}, ids...), ids[0]), Favorite: &fav})
	if err != nil || n != len(ids) || bumps != 1 {
		t.Fatalf("batch: n=%d bumps=%d err=%v", n, bumps, err)
	}
	for _, id := range []string{ids[0], ids[700]} {
		c, err := s.Channel(ctx, id)
		if err != nil || !c.Favorite || !c.Enabled || !c.Private || c.StreamURL != "https://provider.invalid/private" || len(c.Items) != 2 {
			t.Fatalf("lost fields: %+v %v", c, err)
		}
	}
	off := false
	if _, err := s.UpdateChannels(ctx, ChannelBatch{IDs: ids[:2], Enabled: &off, Favorite: &off}); err != nil {
		t.Fatal(err)
	}
	c, _ = s.Channel(ctx, ids[0])
	if c.Enabled || c.Favorite {
		t.Fatal("false patch ignored")
	}
	c, _ = s.Channel(ctx, ids[2])
	if !c.Enabled || !c.Favorite {
		t.Fatal("unselected channel changed")
	}
}

func TestChannelBatchGroupsSurviveImport(t *testing.T) {
	s := testService(t, Options{})
	ctx := context.Background()
	src := Source{ID: "provider", Name: "Provider", Playlist: "#EXTM3U\n#EXTINF:-1 tvg-id=\"news\" group-title=\"Provider news\",News\nhttps://provider.invalid/news.ts\n"}
	if _, err := s.ImportSource(ctx, src); err != nil {
		t.Fatal(err)
	}
	cs, _ := s.Channels(ctx)
	if len(cs) != 1 {
		t.Fatal("fixture channels", len(cs))
	}
	group := "  My news  "
	if _, err := s.UpdateChannels(ctx, ChannelBatch{IDs: []string{cs[0].ID}, Group: &group}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ImportSource(ctx, src); err != nil {
		t.Fatal(err)
	}
	c, _ := s.Channel(ctx, cs[0].ID)
	if c.Group != "My news" || !c.CustomGroup {
		t.Fatalf("custom group lost: %+v", c)
	}
}

func TestChannelBatchRejectsInvalidAndUnknownFields(t *testing.T) {
	s := testService(t, Options{})
	ctx := context.Background()
	c := testChannel()
	s.SaveChannel(ctx, c)
	fav := true
	long := strings.Repeat("x", 201)
	for _, body := range []ChannelBatch{{IDs: []string{c.ID}}, {Favorite: &fav}, {IDs: []string{""}, Favorite: &fav}, {IDs: []string{c.ID}, Group: &long}} {
		if _, err := s.UpdateChannels(ctx, body); err == nil {
			t.Fatal("accepted invalid batch")
		}
	}
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPatch, "/channels/bulk", strings.NewReader(`{"ids":["test"],"streamUrl":"https://wrong.invalid"}`)))
	if rec.Code != http.StatusBadRequest {
		t.Fatal("unexpected status", rec.Code)
	}
	rec = httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPatch, "/channels/bulk", strings.NewReader(`{"ids":["test"],"favorite":true}`)))
	if rec.Code != http.StatusOK {
		t.Fatal("patch route", rec.Code, rec.Body.String())
	}
}
