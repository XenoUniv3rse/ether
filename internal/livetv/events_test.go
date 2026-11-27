package livetv

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/events"
	"github.com/braedonsaunders/ether/internal/store"
)

func TestLiveTVEmitsAllEvents(t *testing.T) {
	ctx := context.Background()
	db, err := store.OpenMemory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	baseNow := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	var clockNano atomic.Int64
	clockNano.Store(baseNow.UnixNano())
	now := func() time.Time { return time.Unix(0, clockNano.Load()).UTC() }
	bus, rec := events.NewTestBus(t)
	reg := events.NewBuiltinRegistry()

	// HTTP test server serving a tiny M3U playlist + failing/auth endpoints.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/ok.m3u":
			w.Header().Set("Content-Type", "audio/x-mpegurl")
			_, _ = w.Write([]byte("#EXTM3U\n#EXTINF:-1 tvg-id=\"espn\" tvg-chno=\"206\",ESPN HD\nhttp://provider.invalid/live/espn.ts\n"))
		case "/auth.m3u":
			w.WriteHeader(http.StatusUnauthorized)
		default:
			w.WriteHeader(http.StatusBadGateway)
		}
	}))
	defer srv.Close()

	s := historyService(t, db, func(o *Options) {
		o.Now = now
		o.Events = bus
		o.HTTPClient = srv.Client()
	})
	defer s.Close()

	// 1. livetv.guide.refreshed via importSourceLogged.
	imported, err := s.importSourceLogged(ctx, Source{
		ID:          "prov_1",
		Name:        "Primary IPTV",
		Kind:        "m3u",
		PlaylistURL: srv.URL + "/ok.m3u",
	})
	if err != nil {
		t.Fatalf("importSourceLogged ok: %v", err)
	}
	evGuideOK := rec.WaitFor(t, events.EventLiveTVGuideRefreshed, time.Second)
	if err := reg.ValidatePayload(evGuideOK.Type, evGuideOK.Payload); err != nil {
		t.Fatalf("guide refreshed invalid: %v", err)
	}
	gOK, err := events.DecodePayload[events.LiveTVGuideRefreshedPayload](evGuideOK)
	if err != nil {
		t.Fatal(err)
	}
	if gOK.SourceID != imported.ID || gOK.Channels != 1 {
		t.Fatalf("unexpected guide refreshed: %+v", gOK)
	}

	// 2. livetv.provider.auth_failed + livetv.guide.failed via 401 on importSourceLogged.
	prevUpdated := baseNow.Add(-6 * time.Hour)
	if _, err := s.importSourceLogged(ctx, Source{
		ID:          "prov_auth",
		Name:        "Locked IPTV",
		Kind:        "m3u",
		PlaylistURL: srv.URL + "/auth.m3u",
		UpdatedAt:   prevUpdated,
	}); err == nil {
		t.Fatal("expected 401 import failure")
	}
	evAuth := rec.WaitFor(t, events.EventLiveTVProviderAuthFailed, time.Second)
	if err := reg.ValidatePayload(evAuth.Type, evAuth.Payload); err != nil {
		t.Fatalf("provider auth failed invalid: %v", err)
	}
	authP, err := events.DecodePayload[events.LiveTVProviderAuthFailedPayload](evAuth)
	if err != nil {
		t.Fatal(err)
	}
	if authP.ProviderID != "prov_auth" || authP.StatusCode != 401 {
		t.Fatalf("unexpected provider auth failed: %+v", authP)
	}

	evGuideFail := rec.WaitFor(t, events.EventLiveTVGuideFailed, time.Second)
	if err := reg.ValidatePayload(evGuideFail.Type, evGuideFail.Payload); err != nil {
		t.Fatalf("guide failed invalid: %v", err)
	}
	gFail, err := events.DecodePayload[events.LiveTVGuideFailedPayload](evGuideFail)
	if err != nil {
		t.Fatal(err)
	}
	if gFail.SourceID != "prov_auth" || gFail.StaleHours < 5.9 {
		t.Fatalf("unexpected guide failed: %+v", gFail)
	}

	// Populate channel snapshot with primary + backup stream sources.
	ch := Channel{
		ID:       "ch_espn",
		Name:     "ESPN HD",
		Number:   "206",
		Kind:     "iptv",
		Enabled:  true,
		SourceID: "prov_1",
		StreamSources: []StreamSource{
			{ID: "src_primary", URL: "http://provider.invalid/primary.ts", Enabled: true},
			{ID: "src_backup", URL: "http://provider.invalid/backup.ts", Enabled: true},
		},
	}
	if _, err := s.SaveChannel(ctx, ch); err != nil {
		t.Fatal(err)
	}
	if _, err := s.allChannels(ctx); err != nil {
		t.Fatal(err)
	}

	// 3. livetv.source.cooldown via recordSourceHealth.
	s.recordSourceHealth(ch.ID, ch.StreamSources[0], context.DeadlineExceeded, false)
	evCool := rec.WaitFor(t, events.EventLiveTVSourceCooldown, time.Second)
	if err := reg.ValidatePayload(evCool.Type, evCool.Payload); err != nil {
		t.Fatalf("source cooldown invalid: %v", err)
	}
	cool, err := events.DecodePayload[events.LiveTVSourceCooldownPayload](evCool)
	if err != nil {
		t.Fatal(err)
	}
	if cool.ChannelID != ch.ID || cool.SourceID != "src_primary" || cool.Failures != 1 || cool.ErrorClass != "timeout" || cool.RetryAfter == nil {
		t.Fatalf("unexpected source cooldown: %+v", cool)
	}

	// 4. livetv.source.degraded via noteSourceEvidence (edge-triggered).
	s.noteSourceEvidence(ch.ID, ch.StreamSources[0], SourceEvidence{
		Media:        10 * time.Minute,
		DamageEvents: 4,
		Stalls:       2,
		KeyframeGap:  6 * time.Second,
	})
	evDeg := rec.WaitFor(t, events.EventLiveTVSourceDegraded, time.Second)
	if err := reg.ValidatePayload(evDeg.Type, evDeg.Payload); err != nil {
		t.Fatalf("source degraded invalid: %v", err)
	}
	deg, err := events.DecodePayload[events.LiveTVSourceDegradedPayload](evDeg)
	if err != nil {
		t.Fatal(err)
	}
	if deg.ChannelID != ch.ID || deg.SourceID != "src_primary" || deg.DamagePerHour < 1 {
		t.Fatalf("unexpected source degraded: %+v", deg)
	}

	// 5-7. livetv.input.stalled, livetv.input.recovered, livetv.input.lost via inputGrant.notify.
	grant := &inputGrant{
		service:     s,
		histChannel: ch.ID,
		histSrc:     ch.StreamSources[0],
		source:      "prov_1",
	}
	grant.notify(UpstreamEvent{
		Kind: InputStalled,
		At:   baseNow,
		Health: UpstreamHealth{
			Kind:         "ts",
			DataAge:      8500 * time.Millisecond,
			ReserveDepth: 2 * time.Second,
			StallAfter:   8 * time.Second,
			Reconnects:   1,
		},
	})
	evStall := rec.WaitFor(t, events.EventLiveTVInputStalled, time.Second)
	if err := reg.ValidatePayload(evStall.Type, evStall.Payload); err != nil {
		t.Fatalf("input stalled invalid: %v", err)
	}

	grant.notify(UpstreamEvent{
		Kind: InputRecovered,
		At:   baseNow.Add(4200 * time.Millisecond),
		Health: UpstreamHealth{
			Kind:       "ts",
			Reconnects: 1,
		},
	})
	evRec := rec.WaitFor(t, events.EventLiveTVInputRecovered, time.Second)
	if err := reg.ValidatePayload(evRec.Type, evRec.Payload); err != nil {
		t.Fatalf("input recovered invalid: %v", err)
	}
	recP, err := events.DecodePayload[events.LiveTVInputRecoveredPayload](evRec)
	if err != nil {
		t.Fatal(err)
	}
	if recP.DowntimeMS != 4200 {
		t.Fatalf("downtimeMs = %d, want 4200", recP.DowntimeMS)
	}

	grant.notify(UpstreamEvent{
		Kind: InputFailed,
		At:   baseNow.Add(10 * time.Second),
		Health: UpstreamHealth{
			Kind:       "ts",
			Reconnects: 3,
		},
		Err: errors.New("upstream EOF after reconnect limit"),
	})
	evLost := rec.WaitFor(t, events.EventLiveTVInputLost, time.Second)
	if err := reg.ValidatePayload(evLost.Type, evLost.Payload); err != nil {
		t.Fatalf("input lost invalid: %v", err)
	}
	lostP, err := events.DecodePayload[events.LiveTVInputLostPayload](evLost)
	if err != nil {
		t.Fatal(err)
	}
	if !lostP.HasBackup || lostP.Reconnects != 3 {
		t.Fatalf("unexpected input lost: %+v", lostP)
	}

	// 8. livetv.source.failover.
	s.emitSourceFailover(ch, "src_primary", "src_backup", "input stalled", "broadcast")
	evFO := rec.WaitFor(t, events.EventLiveTVSourceFailover, time.Second)
	if err := reg.ValidatePayload(evFO.Type, evFO.Payload); err != nil {
		t.Fatalf("source failover invalid: %v", err)
	}
	foP, err := events.DecodePayload[events.LiveTVSourceFailoverPayload](evFO)
	if err != nil {
		t.Fatal(err)
	}
	if foP.FromSourceID != "src_primary" || foP.ToSourceID != "src_backup" {
		t.Fatalf("unexpected source failover: %+v", foP)
	}

	// 9. livetv.provider.limit_reached via viewer admission.
	rel1, err := s.holdProviderInput("prov_1", "b1", 1)
	if err != nil {
		t.Fatalf("first reservation failed: %v", err)
	}
	defer rel1()
	s.setSourceEntry("prov_1", authEntry{limit: 1, at: time.Now()})
	if err := s.withProviderCapacity(ctx, Channel{ID: "ch_other", SourceID: "prov_1"}, StreamSource{}, func(context.Context) error {
		_, err := s.holdProviderInput("prov_1", "b2", 1)
		return err
	}); !errors.Is(err, ErrProviderBusy) {
		t.Fatalf("expected ErrProviderBusy, got %v", err)
	}
	evLim := rec.WaitFor(t, events.EventLiveTVProviderLimitReached, time.Second)
	if err := reg.ValidatePayload(evLim.Type, evLim.Payload); err != nil {
		t.Fatalf("provider limit invalid: %v", err)
	}
	limP, err := events.DecodePayload[events.LiveTVProviderLimitReachedPayload](evLim)
	if err != nil {
		t.Fatal(err)
	}
	if limP.ProviderID != "prov_1" || limP.ActiveConnections != 1 || limP.MaxConnections != 1 {
		t.Fatalf("unexpected provider limit: %+v", limP)
	}

	// 10-11. livetv.channel.tuned + livetv.channel.stopped (via session lifecycle).
	ss := &session{
		ready:       true,
		bytesServed: 2048,
		inputSource: ch.StreamSources[0],
		user:        core.User{ID: 42, Username: "alex"},
		caps:        core.ClientCaps{Product: "Vela tvOS", ClientID: "atv-1"},
		info: SessionInfo{
			ID:             "live-sess-1",
			UserID:         42,
			Channel:        ch,
			ActiveSourceID: "src_primary",
			StartedAt:      baseNow,
		},
	}
	s.emitChannelTuned(ss.info, ss.inputSource, ss.user, ss.caps, "broadcast")
	evTuned := rec.WaitFor(t, events.EventLiveTVChannelTuned, time.Second)
	if err := reg.ValidatePayload(evTuned.Type, evTuned.Payload); err != nil {
		t.Fatalf("channel tuned invalid: %v", err)
	}
	tunedP, err := events.DecodePayload[events.LiveTVChannelTunedPayload](evTuned)
	if err != nil {
		t.Fatal(err)
	}
	if tunedP.ChannelID != ch.ID || tunedP.Username != "alex" || tunedP.Client != "Vela tvOS" {
		t.Fatalf("unexpected channel tuned: %+v", tunedP)
	}

	clockNano.Store(baseNow.Add(30 * time.Second).UnixNano())
	ss.close(s)
	evStopped := rec.WaitFor(t, events.EventLiveTVChannelStopped, time.Second)
	if err := reg.ValidatePayload(evStopped.Type, evStopped.Payload); err != nil {
		t.Fatalf("channel stopped invalid: %v", err)
	}
	stopP, err := events.DecodePayload[events.LiveTVChannelStoppedPayload](evStopped)
	if err != nil {
		t.Fatal(err)
	}
	if stopP.ChannelID != ch.ID || stopP.WatchedMS != 30000 || stopP.BytesServed != 2048 {
		t.Fatalf("unexpected channel stopped: %+v", stopP)
	}
}
