package plex

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
	"encoding/xml"
	"image"
	"image/jpeg"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/store"
	"github.com/braedonsaunders/ether/internal/transcode"
)

var tautulliHeaders = []string{
	"X-Plex-Product", "Tautulli",
	"X-Plex-Version", "2.15.0",
	"X-Plex-Client-Identifier", "tautulli-audit-client",
	"X-Plex-Platform", "Linux",
	"X-Plex-Device", "PC",
	"X-Plex-Device-Name", "Tautulli",
}

func TestTautulliTranscodeSessionHardwareProgressAndThrottle(t *testing.T) {
	f := newFixture()
	defer f.srv.Close()
	f.trans.decision = core.Decision{
		Kind:        core.DecisionTranscode,
		VideoCopy:   false,
		VideoCodec:  "h264",
		MaxHeight:   720,
		BitrateKbps: 4000,
		AudioCopy:   false,
		AudioCodec:  "aac",
		Container:   "mpegts",
	}
	headers := []string{
		"X-Plex-Client-Identifier", "tautulli-player-1",
		"X-Plex-Session-Identifier", "tautulli-play-1",
		"X-Plex-Product", "Plex Web",
		"X-Plex-Platform", "Chrome",
		"X-Plex-Device", "OSX",
	}
	start := "/video/:/transcode/universal/start.m3u8?path=%2Flibrary%2Fmetadata%2F1000000000000550&protocol=hls&directPlay=0&directStream=1&session=hw-sess"
	if rec := f.get(t, decisionPath(start), headers...); rec.Code != http.StatusOK {
		t.Fatalf("decision: %d %s", rec.Code, rec.Body.String())
	}
	if rec := f.get(t, start, headers...); rec.Code != http.StatusOK {
		t.Fatalf("start: %d %s", rec.Code, rec.Body.String())
	}
	if rec := f.get(t, "/:/timeline?ratingKey=1000000000000550&key=%2Flibrary%2Fmetadata%2F1000000000000550&state=playing&time=60000&duration=8340000", headers...); rec.Code != http.StatusOK {
		t.Fatalf("timeline: %d %s", rec.Code, rec.Body.String())
	}

	hs := f.trans.sessions["hw-sess"]
	if hs == nil {
		t.Fatal("missing started HLS session")
	}

	type wireTS struct {
		Key                      string  `xml:"key,attr"`
		Throttled                string  `xml:"throttled,attr"`
		Complete                 string  `xml:"complete,attr"`
		Progress                 float64 `xml:"progress,attr"`
		Speed                    float64 `xml:"speed,attr"`
		TranscodeHwRequested     string  `xml:"transcodeHwRequested,attr"`
		TranscodeHwDecoding      string  `xml:"transcodeHwDecoding,attr"`
		TranscodeHwDecodingTitle string  `xml:"transcodeHwDecodingTitle,attr"`
		TranscodeHwEncoding      string  `xml:"transcodeHwEncoding,attr"`
		TranscodeHwEncodingTitle string  `xml:"transcodeHwEncodingTitle,attr"`
		TranscodeHwFullPipeline  string  `xml:"transcodeHwFullPipeline,attr"`
		MinOffsetAvailable       float64 `xml:"minOffsetAvailable,attr"`
		MaxOffsetAvailable       float64 `xml:"maxOffsetAvailable,attr"`
	}
	readTS := func(t *testing.T) wireTS {
		t.Helper()
		rec := f.get(t, "/status/sessions", tautulliHeaders...)
		if rec.Code != http.StatusOK {
			t.Fatalf("/status/sessions: %d %s", rec.Code, rec.Body.String())
		}
		var doc struct {
			Videos []struct {
				TS *wireTS `xml:"TranscodeSession"`
			} `xml:"Video"`
		}
		if err := xml.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
			t.Fatalf("unmarshal: %v\n%s", err, rec.Body.String())
		}
		if len(doc.Videos) != 1 || doc.Videos[0].TS == nil {
			t.Fatalf("missing TranscodeSession: %s", rec.Body.String())
		}
		return *doc.Videos[0].TS
	}

	// 1. Active GPU full pipeline (running).
	hs.encoder = &core.EncoderStatus{
		Decision:           f.trans.decision,
		Processor:          "gpu",
		Filters:            "gpu",
		Phase:              "running",
		Speed:              4.26,
		OutputTime:         2085 * time.Second,
		ReadyUntil:         2085 * time.Second, // 25% of 8340s
		MinOffsetAvailable: 60 * time.Second,
		HWDecoder:          "videotoolbox",
		HWEncoder:          "videotoolbox",
	}
	ts := readTS(t)
	if ts.Throttled != "0" || ts.Complete != "0" || ts.Progress != 25 || ts.Speed != 4.3 ||
		ts.TranscodeHwRequested != "1" || ts.TranscodeHwFullPipeline != "1" ||
		ts.TranscodeHwDecoding != "videotoolbox" || ts.TranscodeHwDecodingTitle != "Apple VideoToolbox" ||
		ts.TranscodeHwEncoding != "videotoolbox" || ts.TranscodeHwEncodingTitle != "Apple VideoToolbox" ||
		ts.MinOffsetAvailable != 60 || ts.MaxOffsetAvailable != 2085 {
		t.Fatalf("running GPU TranscodeSession = %+v", ts)
	}

	// 2. Throttled (paused) GPU run.
	hs.encoder.Phase = "paused"
	ts = readTS(t)
	if ts.Throttled != "1" || ts.Complete != "0" {
		t.Fatalf("paused TranscodeSession = %+v, want throttled=1 complete=0", ts)
	}

	// 3. Finished run reports complete=1 and progress=100.
	hs.encoder.Phase = "finished"
	ts = readTS(t)
	if ts.Throttled != "0" || ts.Complete != "1" || ts.Progress != 100 {
		t.Fatalf("finished TranscodeSession = %+v, want complete=1 progress=100", ts)
	}

	// 4. Software fallback clears HW attributes.
	hs.encoder = &core.EncoderStatus{
		Decision:   f.trans.decision,
		Processor:  "cpu",
		Filters:    "cpu",
		Phase:      "running",
		Speed:      1.5,
		ReadyUntil: 834 * time.Second, // 10%
	}
	ts = readTS(t)
	if ts.TranscodeHwRequested != "0" || ts.TranscodeHwDecoding != "" || ts.TranscodeHwEncoding != "" ||
		ts.TranscodeHwFullPipeline != "" || ts.Progress != 10 {
		t.Fatalf("CPU TranscodeSession = %+v", ts)
	}
}

func TestTautulliSessionPlayerSecureRemoteAndLocalUserID(t *testing.T) {
	localOwner := core.User{ID: 1, PlexID: 0, Username: "localadmin", Title: "Local Admin", IsOwner: true}
	f := newFixture(func(d *testServerDeps) {
		setDepsAuth(d, &fakeAuth{users: map[string]core.User{
			ownerToken:          ownerUser,
			friendToken:         friendUser,
			"local-owner-token": localOwner,
		}})
	})
	defer f.srv.Close()

	// Remote HTTPS client via loopback reverse proxy.
	req := httptest.NewRequest(http.MethodGet, "/:/timeline?ratingKey=1000000000000550&state=playing&time=10000&duration=8340000", nil)
	req.RemoteAddr = "127.0.0.1:45000"
	req.Header.Set("X-Forwarded-For", "198.51.100.42")
	req.Header.Set("X-Forwarded-Proto", "https")
	req.Header.Set("X-Plex-Token", "local-owner-token")
	req.Header.Set("X-Plex-Client-Identifier", "wan-client")
	req.Header.Set("X-Plex-Session-Identifier", "wan-sess")
	req.Header.Set("X-Plex-Product", "Plex Web")
	rec := httptest.NewRecorder()
	f.srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("wan timeline: %d", rec.Code)
	}

	// Untrusted peer spoofing X-Forwarded-Proto must not report secure=1.
	untrusted := httptest.NewRequest(http.MethodGet, "/:/timeline?ratingKey=1000000000000550&state=playing&time=20000&duration=8340000", nil)
	untrusted.RemoteAddr = "203.0.113.9:50000"
	untrusted.Header.Set("X-Forwarded-Proto", "https")
	untrusted.Header.Set("X-Plex-Token", friendToken)
	untrusted.Header.Set("X-Plex-Client-Identifier", "spoof-client")
	untrusted.Header.Set("X-Plex-Session-Identifier", "spoof-sess")
	rec = httptest.NewRecorder()
	f.srv.ServeHTTP(rec, untrusted)
	if rec.Code != http.StatusOK {
		t.Fatalf("spoof timeline: %d", rec.Code)
	}

	// Direct TLS connection on LAN reports secure=1 with no remotePublicAddress.
	tlsReq := httptest.NewRequest(http.MethodGet, "/:/timeline?ratingKey=1000000000000550&state=playing&time=30000&duration=8340000", nil)
	tlsReq.RemoteAddr = "192.168.1.90:51000"
	tlsReq.TLS = &tls.ConnectionState{}
	tlsReq.Header.Set("X-Plex-Token", ownerToken)
	tlsReq.Header.Set("X-Plex-Client-Identifier", "tls-lan-client")
	tlsReq.Header.Set("X-Plex-Session-Identifier", "tls-lan-sess")
	rec = httptest.NewRecorder()
	f.srv.ServeHTTP(rec, tlsReq)
	if rec.Code != http.StatusOK {
		t.Fatalf("tls timeline: %d", rec.Code)
	}

	sessionsRec := f.get(t, "/status/sessions", tautulliHeaders...)
	if sessionsRec.Code != http.StatusOK {
		t.Fatalf("/status/sessions: %d %s", sessionsRec.Code, sessionsRec.Body.String())
	}
	var doc struct {
		Videos []struct {
			User struct {
				ID    string `xml:"id,attr"`
				Title string `xml:"title,attr"`
			} `xml:"User"`
			Player struct {
				MachineID           string `xml:"machineIdentifier,attr"`
				Address             string `xml:"address,attr"`
				RemotePublicAddress string `xml:"remotePublicAddress,attr"`
				Local               string `xml:"local,attr"`
				Secure              string `xml:"secure,attr"`
				UserID              string `xml:"userID,attr"`
			} `xml:"Player"`
		} `xml:"Video"`
	}
	if err := xml.Unmarshal(sessionsRec.Body.Bytes(), &doc); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, sessionsRec.Body.String())
	}

	byClient := map[string]int{}
	for i, v := range doc.Videos {
		byClient[v.Player.MachineID] = i
	}
	wan := doc.Videos[byClient["wan-client"]]
	if wan.Player.Local != "0" || wan.Player.Secure != "1" ||
		wan.Player.Address != "198.51.100.42" || wan.Player.RemotePublicAddress != "198.51.100.42" ||
		wan.User.ID != "1" || wan.Player.UserID != "1" {
		t.Fatalf("wan player/user = %+v", wan)
	}
	spoof := doc.Videos[byClient["spoof-client"]]
	if spoof.Player.Local != "0" || spoof.Player.Secure != "0" ||
		spoof.Player.Address != "203.0.113.9" || spoof.Player.RemotePublicAddress != "203.0.113.9" {
		t.Fatalf("spoof player = %+v", spoof)
	}
	lanTLS := doc.Videos[byClient["tls-lan-client"]]
	if lanTLS.Player.Local != "1" || lanTLS.Player.Secure != "1" ||
		lanTLS.Player.Address != "192.168.1.90" || lanTLS.Player.RemotePublicAddress != "" {
		t.Fatalf("lan tls player = %+v", lanTLS)
	}
}

func TestTautulliTerminateStopsTranscodeAndNotifiesImmediately(t *testing.T) {
	f := newFixture()
	defer f.srv.Close()
	f.trans.decision = core.Decision{
		Kind:       core.DecisionTranscode,
		VideoCopy:  false,
		VideoCodec: "h264",
		MaxHeight:  720,
		AudioCopy:  true,
		Container:  "mpegts",
	}
	playerHdr := []string{
		"X-Plex-Token", friendToken,
		"X-Plex-Client-Identifier", "kill-client",
		"X-Plex-Session-Identifier", "kill-session-1",
		"X-Plex-Product", "Plex Web",
	}
	start := "/video/:/transcode/universal/start.m3u8?path=%2Flibrary%2Fmetadata%2F1000000000000550&protocol=hls&directPlay=0&directStream=1&session=kill-ts"
	if rec := f.get(t, decisionPath(start), playerHdr...); rec.Code != http.StatusOK {
		t.Fatalf("decision: %d %s", rec.Code, rec.Body.String())
	}
	if rec := f.get(t, start, playerHdr...); rec.Code != http.StatusOK {
		t.Fatalf("start: %d %s", rec.Code, rec.Body.String())
	}
	if rec := f.get(t, "/:/timeline?ratingKey=1000000000000550&key=%2Flibrary%2Fmetadata%2F1000000000000550&state=playing&time=45000&duration=8340000", playerHdr...); rec.Code != http.StatusOK {
		t.Fatalf("timeline: %d", rec.Code)
	}

	sub, ok := f.srv.notifier.subscribe([]string{"playing"}, &ownerUser)
	if !ok {
		t.Fatal("subscribe failed")
	}
	defer f.srv.notifier.unsubscribe(sub)

	termRec := f.get(t, "/status/sessions/terminate?sessionId=kill-session-1&reason="+url.QueryEscape("No 4K transcodes"), tautulliHeaders...)
	if termRec.Code != http.StatusOK {
		t.Fatalf("terminate: %d %s", termRec.Code, termRec.Body.String())
	}

	// Encoder must be stopped and removed from /transcode/sessions.
	if tsList := f.get(t, "/transcode/sessions", tautulliHeaders...).Body.String(); strings.Contains(tsList, "kill-ts") {
		t.Fatalf("terminated transcode still listed: %s", tsList)
	}
	stopped := false
	for _, id := range f.trans.stopped {
		if id == "kill-ts" {
			stopped = true
			break
		}
	}
	if !stopped {
		t.Fatalf("transcoder Stop was not called for kill-ts: %v", f.trans.stopped)
	}

	// Session disappears from /status/sessions immediately.
	if sessList := f.get(t, "/status/sessions", tautulliHeaders...).Body.String(); strings.Contains(sessList, "kill-session-1") {
		t.Fatalf("terminated session still in /status/sessions: %s", sessList)
	}

	// Owner notification stream receives stopped PlaySessionStateNotification.
	select {
	case msg := <-sub.ch:
		var env struct {
			NotificationContainer struct {
				Type                         string `json:"type"`
				PlaySessionStateNotification []struct {
					State string `json:"state"`
				} `json:"PlaySessionStateNotification"`
			} `json:"NotificationContainer"`
		}
		if err := json.Unmarshal(msg.data, &env); err != nil {
			t.Fatalf("unmarshal ws payload: %v\n%s", err, msg.data)
		}
		if env.NotificationContainer.Type != "playing" || len(env.NotificationContainer.PlaySessionStateNotification) != 1 || env.NotificationContainer.PlaySessionStateNotification[0].State != "stopped" {
			t.Fatalf("unexpected notification: %s", msg.data)
		}
	default:
		t.Fatal("expected immediate stopped PlaySessionStateNotification on terminate")
	}

	// Client's next timeline still receives PMS terminationCode/Text.
	nextTL := f.get(t, "/:/timeline?ratingKey=1000000000000550&key=%2Flibrary%2Fmetadata%2F1000000000000550&state=playing&time=50000&duration=8340000", playerHdr...)
	if !strings.Contains(nextTL.Body.String(), `terminationCode="2006" terminationText="No 4K transcodes"`) {
		t.Fatalf("missing termination message on next timeline: %s", nextTL.Body.String())
	}
}

func TestTautulliRecentlyAddedPersonalExcluded(t *testing.T) {
	f := newFixture()
	defer f.srv.Close()

	// Tautulli queries type=1 (movies), type=2 (shows), type=8 (music), and
	// type=1&personal=1 (other_video) and concatenates the results.
	moviesRec := f.get(t, "/hubs/home/recentlyAdded?X-Plex-Container-Start=0&X-Plex-Container-Size=50&type=1", tautulliHeaders...)
	if moviesRec.Code != http.StatusOK || !strings.Contains(moviesRec.Body.String(), `type="movie"`) || !strings.Contains(moviesRec.Body.String(), `librarySectionID="1"`) {
		t.Fatalf("movies recentlyAdded: %d %s", moviesRec.Code, moviesRec.Body.String())
	}
	showsRec := f.get(t, "/hubs/home/recentlyAdded?X-Plex-Container-Start=0&X-Plex-Container-Size=50&type=2", tautulliHeaders...)
	if showsRec.Code != http.StatusOK || !strings.Contains(showsRec.Body.String(), `type="show"`) {
		t.Fatalf("shows recentlyAdded: %d %s", showsRec.Code, showsRec.Body.String())
	}
	for _, query := range []string{
		"X-Plex-Container-Start=0&X-Plex-Container-Size=50&type=8",
		"X-Plex-Container-Start=0&X-Plex-Container-Size=50&type=1&personal=1",
	} {
		rec := f.get(t, "/hubs/home/recentlyAdded?"+query, tautulliHeaders...)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: status %d", query, rec.Code)
		}
		var mc struct {
			Size int `xml:"size,attr"`
		}
		if err := xml.Unmarshal(rec.Body.Bytes(), &mc); err != nil {
			t.Fatalf("%s: unmarshal: %v", query, err)
		}
		if mc.Size != 0 {
			t.Fatalf("%s: size=%d want 0:\n%s", query, mc.Size, rec.Body.String())
		}
	}
}

func TestTautulliRefreshReachabilityAndWebSocket(t *testing.T) {
	f := newFixture(func(d *testServerDeps) {
		setDepsConfig(d, func() config.Config {
			c := config.Config{}
			c.Server.Port = 32400
			c.Server.FriendlyName = "Ether Test"
			c.Server.MachineIdentifier = "0123456789abcdef0123456789abcdef01234567"
			c.Plex.Claimed = true
			c.Plex.RemoteAccess = true
			c.Plex.OwnerUsername = "owner"
			return c
		})
	})
	defer f.srv.Close()

	// Non-owners cannot trigger reachability refresh.
	if rec := f.request(t, http.MethodPut, "/myplex/refreshReachability", map[string]string{"X-Plex-Token": friendToken}); rec.Code != http.StatusForbidden {
		t.Fatalf("friend refreshReachability: %d want 403", rec.Code)
	}

	sub, ok := f.srv.notifier.subscribe([]string{"reachability"}, &ownerUser)
	if !ok {
		t.Fatal("subscribe failed")
	}
	defer f.srv.notifier.unsubscribe(sub)

	if rec := f.request(t, http.MethodPut, "/myplex/refreshReachability", map[string]string{"X-Plex-Product": "Tautulli"}); rec.Code != http.StatusOK {
		t.Fatalf("owner refreshReachability: %d %s", rec.Code, rec.Body.String())
	}
	select {
	case msg := <-sub.ch:
		if !bytes.Contains(msg.data, []byte(`"reachability"`)) {
			t.Fatalf("missing reachability token: %s", msg.data)
		}
		var env struct {
			NotificationContainer struct {
				Type                     string `json:"type"`
				Size                     int    `json:"size"`
				ReachabilityNotification []struct {
					Reachability bool `json:"reachability"`
				} `json:"ReachabilityNotification"`
			} `json:"NotificationContainer"`
		}
		if err := json.Unmarshal(msg.data, &env); err != nil {
			t.Fatalf("unmarshal: %v\n%s", err, msg.data)
		}
		if env.NotificationContainer.Type != "reachability" ||
			len(env.NotificationContainer.ReachabilityNotification) != 1 ||
			!env.NotificationContainer.ReachabilityNotification[0].Reachability {
			t.Fatalf("unexpected reachability payload: %s", msg.data)
		}
	default:
		t.Fatal("expected reachability websocket notification")
	}
}

func TestTautulliSectionLabels(t *testing.T) {
	f := newFixture()
	defer f.srv.Close()

	rec := f.get(t, "/library/sections/1/label", tautulliHeaders...)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /library/sections/1/label: %d %s", rec.Code, rec.Body.String())
	}
	var facet struct {
		Size   int    `xml:"size,attr"`
		Title2 string `xml:"title2,attr"`
	}
	if err := xml.Unmarshal(rec.Body.Bytes(), &facet); err != nil {
		t.Fatalf("unmarshal label facet: %v", err)
	}
	if facet.Size != 0 || facet.Title2 != "By Label" {
		t.Fatalf("label facet = %+v", facet)
	}

	// Filtering by a label key returns 0 items instead of ignoring the filter.
	filtered := f.get(t, "/library/sections/1/all?label=42", tautulliHeaders...)
	if filtered.Code != http.StatusOK {
		t.Fatalf("label filter: %d %s", filtered.Code, filtered.Body.String())
	}
	var list struct {
		Size int `xml:"size,attr"`
	}
	if err := xml.Unmarshal(filtered.Body.Bytes(), &list); err != nil {
		t.Fatalf("unmarshal label filter: %v", err)
	}
	if list.Size != 0 {
		t.Fatalf("label=42 size=%d want 0", list.Size)
	}

	// Secondary subpath /library/sections/1/label/42 also returns 0 items.
	subRec := f.get(t, "/library/sections/1/label/42", tautulliHeaders...)
	if subRec.Code != http.StatusOK {
		t.Fatalf("label subpath: %d %s", subRec.Code, subRec.Body.String())
	}
	if err := xml.Unmarshal(subRec.Body.Bytes(), &list); err != nil || list.Size != 0 {
		t.Fatalf("label subpath list=%+v err=%v", list, err)
	}

	// Negative label filter keeps items.
	neg := f.get(t, "/library/sections/1/all?label!=42", tautulliHeaders...)
	if neg.Code != http.StatusOK {
		t.Fatalf("label!=42: %d", neg.Code)
	}
	if err := xml.Unmarshal(neg.Body.Bytes(), &list); err != nil || list.Size == 0 {
		t.Fatalf("label!=42 expected items, got %+v err=%v", list, err)
	}
}

func TestTautulliPhotoTranscodeBIFFrameForActiveStream(t *testing.T) {
	otherFriend := core.User{ID: 99, PlexID: 9900, Username: "other", Title: "Other"}
	f := newFixture(func(d *testServerDeps) {
		setDepsAuth(d, &fakeAuth{users: map[string]core.User{
			ownerToken:    ownerUser,
			friendToken:   friendUser,
			"other-token": otherFriend,
		}})
	})
	defer f.srv.Close()
	var encoded bytes.Buffer
	if err := jpeg.Encode(&encoded, image.NewRGBA(image.Rect(0, 0, 160, 90)), nil); err != nil {
		t.Fatal(err)
	}
	frame := encoded.Bytes()
	bif, err := transcode.MarshalBIF([]transcode.BIFFrame{{Time: 10 * time.Second, JPEG: frame}})
	if err != nil {
		t.Fatal(err)
	}
	f.srv.setTrickplay(&fakeTrickplay{bif: bif, frame: frame, available: true})

	// Friend is actively playing fightClub on their own client.
	f.playback.mu.Lock()
	f.playback.sessions["friend-active"] = &core.PlaySession{
		ID:         "friend-active",
		UserID:     friendUser.ID,
		ClientID:   "friend-apple-tv",
		Key:        fightClub.Key,
		Resolution: &core.Resolution{Release: core.Release{ID: "rel-bif"}},
		StartedAt:  fixedNow,
	}
	f.playback.mu.Unlock()

	inner := "http://127.0.0.1:32400/library/parts/" + fightClub.Key.String() + "/indexes/sd/10000?X-Plex-Token=" + ownerToken
	path := "/photo/:/transcode?url=" + url.QueryEscape(inner) + "&width=300&height=450&format=png"

	// Owner running Tautulli with its own client ID can fetch the active stream's BIF thumb.
	ownerTautulli := append(append([]string(nil), tautulliHeaders...), "X-Plex-Token", ownerToken)
	rec := f.get(t, path, ownerTautulli...)
	if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "image/jpeg" || !bytes.Equal(rec.Body.Bytes(), frame) {
		t.Fatalf("owner BIF transcode: status=%d ct=%q len=%d", rec.Code, rec.Header().Get("Content-Type"), rec.Body.Len())
	}

	// A different non-owner client cannot fetch another viewer's active BIF thumb.
	denied := f.get(t, path, "X-Plex-Token", "other-token", "X-Plex-Client-Identifier", "other-client")
	if denied.Code != http.StatusNotFound {
		t.Fatalf("non-owner BIF transcode: %d want 404", denied.Code)
	}
}

func TestTautulliLiveTVDVRFields(t *testing.T) {
	f, _, _ := liveFixture(t)
	rec := f.get(t, "/livetv/dvrs", tautulliHeaders...)
	if rec.Code != http.StatusOK {
		t.Fatalf("/livetv/dvrs: %d %s", rec.Code, rec.Body.String())
	}
	var doc struct {
		DVRs []struct {
			Key           string `xml:"key,attr"`
			UUID          string `xml:"uuid,attr"`
			Language      string `xml:"language,attr"`
			Lineup        string `xml:"lineup,attr"`
			LineupTitle   string `xml:"lineupTitle,attr"`
			Country       string `xml:"country,attr"`
			RefreshedAt   int64  `xml:"refreshedAt,attr"`
			EPGIdentifier string `xml:"epgIdentifier,attr"`
		} `xml:"Dvr"`
	}
	if err := xml.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, rec.Body.String())
	}
	if len(doc.DVRs) != 1 {
		t.Fatalf("expected 1 Dvr, got %d", len(doc.DVRs))
	}
	d := doc.DVRs[0]
	if d.Key == "" || d.UUID == "" || d.Language == "" || d.Lineup == "" ||
		d.LineupTitle == "" || d.Country == "" || d.RefreshedAt <= 0 || d.EPGIdentifier == "" {
		t.Fatalf("incomplete Dvr attributes: %+v", d)
	}
}

func TestTautulliHubSearchIncludeCollections(t *testing.T) {
	f := newFixture()
	defer f.srv.Close()

	rec := f.get(t, "/hubs/search?query=Alien&includeCollections=1", tautulliHeaders...)
	if rec.Code != http.StatusOK {
		t.Fatalf("/hubs/search: %d %s", rec.Code, rec.Body.String())
	}
	var doc struct {
		Hubs []struct {
			Type        string `xml:"type,attr"`
			Size        int    `xml:"size,attr"`
			Directories []struct {
				RatingKey string `xml:"ratingKey,attr"`
				Title     string `xml:"title,attr"`
			} `xml:"Directory"`
		} `xml:"Hub"`
	}
	if err := xml.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, rec.Body.String())
	}
	found := false
	for _, h := range doc.Hubs {
		if h.Type != "collection" {
			continue
		}
		for _, d := range h.Directories {
			if d.RatingKey == core.CollectionKey(8091).String() && d.Title == "Alien Collection" {
				found = true
			}
		}
	}
	if !found {
		t.Fatalf("expected Alien Collection in collection hub:\n%s", rec.Body.String())
	}
}

func TestTautulliMetadataBatchMarkersAndLibraryTotals(t *testing.T) {
	db := store.OpenTest(t)
	f := newFixture(func(d *testServerDeps) { d.DB = db })
	defer f.srv.Close()
	f.srv.setMarkers(fakeMarkers{
		{Type: "intro", StartMS: 5000, EndMS: 45000},
		{Type: "credits", StartMS: 8000000, EndMS: 8340000, Final: true},
	})

	// Populate an active probed session so Markers render on detail metadata.
	f.playback.mu.Lock()
	f.playback.sessions["probed"] = &core.PlaySession{
		ID:       "probed",
		UserID:   ownerUser.ID,
		ClientID: "tautulli-audit-client",
		Key:      fightClub.Key,
		Info:     sampleInfo,
	}
	f.playback.mu.Unlock()

	batchPath := "/library/metadata/" + fightClub.Key.String() + "," + alien.Key.String() + "?includeMarkers=1"
	rec := f.get(t, batchPath, tautulliHeaders...)
	if rec.Code != http.StatusOK {
		t.Fatalf("batch metadata: %d %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{
		`ratingKey="` + fightClub.Key.String() + `"`,
		`ratingKey="` + alien.Key.String() + `"`,
		`librarySectionID="1"`,
		`librarySectionTitle="Movies"`,
		`<Media `,
		`<Part `,
		`<Stream `,
		`<Guid id="imdb://tt0137523"`,
		`<Marker id="1" type="intro"`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("batch metadata missing %q:\n%s", want, body)
		}
	}

	// Library totals for TV section: shows (type=2), seasons (type=3), episodes (type=4).
	for _, tc := range []struct {
		typ       string
		viewGroup string
	}{
		{"2", "show"},
		{"3", "season"},
		{"4", "episode"},
	} {
		listRec := f.get(t, "/library/sections/2/all?X-Plex-Container-Start=0&X-Plex-Container-Size=1&type="+tc.typ, tautulliHeaders...)
		if listRec.Code != http.StatusOK {
			t.Fatalf("section 2 type=%s: %d %s", tc.typ, listRec.Code, listRec.Body.String())
		}
		var mc struct {
			Size      int    `xml:"size,attr"`
			TotalSize int    `xml:"totalSize,attr"`
			ViewGroup string `xml:"viewGroup,attr"`
		}
		if err := xml.Unmarshal(listRec.Body.Bytes(), &mc); err != nil {
			t.Fatalf("unmarshal type=%s: %v", tc.typ, err)
		}
		if mc.ViewGroup != tc.viewGroup || mc.TotalSize <= 0 {
			t.Fatalf("section 2 type=%s: mc=%+v want viewGroup=%q totalSize>0", tc.typ, mc, tc.viewGroup)
		}
	}
}
