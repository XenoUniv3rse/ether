package plex

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// A Plex player that opens no port (Plex for Samsung) long-polls its server at
// /player/proxy/poll, and controllers send /player/* commands to the server
// with X-Plex-Target-Client-Identifier. The wire shapes below follow a player
// implementation measured against PMS: commands arrive as <Command> elements
// with the controller's arguments prefixed "query", the player answers with
// POST /player/proxy/response?commandID=N, and that body reaches the
// controller unchanged.

const companionTV = "tv-companion-1"

func companionPlayerHeaders(token string) map[string]string {
	return map[string]string{
		"X-Plex-Token":             token,
		"X-Plex-Client-Identifier": companionTV,
		"X-Plex-Product":           "Plex for Samsung",
		"X-Plex-Device-Name":       "Living Room TV",
		"X-Plex-Platform":          "Tizen",
		"X-Plex-Platform-Version":  "7.0",
		"X-Plex-Version":           "5.94.1",
		"X-Plex-Provides":          "player",
		"Accept":                   "application/xml",
	}
}

func companionFixture(t *testing.T) *fixture {
	t.Helper()
	f := newFixture()
	t.Cleanup(func() { _ = f.srv.Close() })
	f.srv.companion.pollHold = 2 * time.Second
	f.srv.companion.commandWait = 2 * time.Second
	return f
}

// startPoll issues a player poll in the background and returns its result.
func startPoll(t *testing.T, f *fixture, token string) <-chan *httptest.ResponseRecorder {
	t.Helper()
	out := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		out <- f.request(t, http.MethodGet, "/player/proxy/poll?deviceClass=tv&protocolCapabilities=timeline,playback,navigation,playqueues&protocolVersion=3&commandID=0", companionPlayerHeaders(token))
	}()
	return out
}

func waitForClient(t *testing.T, f *fixture, token string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		r := f.request(t, http.MethodGet, "/clients", map[string]string{"X-Plex-Token": token, "Accept": "application/xml"})
		if r.Code == http.StatusOK && strings.Contains(r.Body.String(), `machineIdentifier="`+companionTV+`"`) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("player %s never listed in /clients", companionTV)
}

func TestCompanionRelayDeliversCommandAndReturnsPlayerAnswer(t *testing.T) {
	f := companionFixture(t)
	const controllerToken = "other-owner-credential"
	f.srv.testAuth().users[controllerToken] = ownerUser
	poll := startPoll(t, f, ownerToken)
	waitForClient(t, f, ownerToken)

	clients := f.request(t, http.MethodGet, "/clients", map[string]string{"Accept": "application/xml", "X-Plex-Token": controllerToken}).Body.String()
	for _, want := range []string{`name="Living Room TV"`, `product="Plex for Samsung"`, `protocolCapabilities="timeline,playback,navigation,playqueues"`, `deviceClass="tv"`} {
		if !strings.Contains(clients, want) {
			t.Errorf("/clients missing %s: %s", want, clients)
		}
	}
	if r := f.request(t, http.MethodGet, "/player/proxy/poll", companionPlayerHeaders(controllerToken)); r.Code != http.StatusNotFound {
		t.Fatalf("poll using another credential for the viewer returned %d", r.Code)
	}

	ctrl := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		ctrl <- f.request(t, http.MethodGet, "/player/playback/playMedia?key=%2Flibrary%2Fmetadata%2F550&offset=0&type=video&commandID=12", map[string]string{
			"X-Plex-Token":                    controllerToken,
			"X-Plex-Client-Identifier":        "phone-1",
			"X-Plex-Target-Client-Identifier": companionTV,
		})
	}()

	var got *httptest.ResponseRecorder
	select {
	case got = <-poll:
	case <-time.After(3 * time.Second):
		t.Fatal("poll was not released by the command")
	}
	body := got.Body.String()
	if got.Code != http.StatusOK || !strings.Contains(body, `<Command `) || !strings.Contains(body, `path="/player/playback/playMedia"`) ||
		!strings.Contains(body, `queryKey="/library/metadata/550"`) || !strings.Contains(body, `queryOffset="0"`) || !strings.Contains(body, `queryType="video"`) {
		t.Fatalf("poll answer %d: %s", got.Code, body)
	}
	if strings.Contains(body, "X-Plex") || strings.Contains(body, "queryCommandID") {
		t.Fatalf("poll leaked transport arguments: %s", body)
	}
	id := attrValue(body, "commandID")
	if id == "" {
		t.Fatalf("command has no commandID: %s", body)
	}

	answer := `<?xml version="1.0" encoding="utf-8"?>` + "\n" + `<Response code="200" status="OK" />`
	if r := f.requestBody(t, http.MethodPost, "/player/proxy/response?commandID="+id, []byte(answer), companionPlayerHeaders(controllerToken)); r.Code != http.StatusNotFound {
		t.Fatalf("response using another credential for the viewer returned %d", r.Code)
	}
	if r := f.requestBody(t, http.MethodPost, "/player/proxy/response?commandID="+id, []byte(answer), companionPlayerHeaders(ownerToken)); r.Code != http.StatusOK {
		t.Fatalf("player response refused: %d", r.Code)
	}
	select {
	case c := <-ctrl:
		if c.Code != http.StatusOK || c.Body.String() != answer {
			t.Fatalf("controller got %d %q, want the player's answer verbatim", c.Code, c.Body.String())
		}
	case <-time.After(3 * time.Second):
		t.Fatal("controller never released")
	}
}

func TestCompanionRelayIdlePollAnswersEmptyContainer(t *testing.T) {
	f := companionFixture(t)
	f.srv.companion.pollHold = 50 * time.Millisecond
	r := <-startPoll(t, f, ownerToken)
	if r.Code != http.StatusOK || !strings.Contains(r.Body.String(), `size="0"`) {
		t.Fatalf("idle poll %d: %s", r.Code, r.Body.String())
	}
}

func TestCompanionRelayKeepsUsersApart(t *testing.T) {
	f := companionFixture(t)
	f.srv.companion.pollHold = 300 * time.Millisecond
	poll := startPoll(t, f, friendToken)
	waitForClient(t, f, friendToken)

	// The owner's controller must not reach a player polled by another user,
	// even with the same client identifier, and must not see it listed.
	if body := f.request(t, http.MethodGet, "/clients", map[string]string{"Accept": "application/xml"}).Body.String(); strings.Contains(body, companionTV) {
		t.Fatalf("owner sees another user's player: %s", body)
	}
	r := f.request(t, http.MethodGet, "/player/playback/pause?type=video", map[string]string{"X-Plex-Target-Client-Identifier": companionTV})
	if r.Code != http.StatusNotFound {
		t.Fatalf("cross-user command = %d, want 404", r.Code)
	}
	if got := <-poll; strings.Contains(got.Body.String(), "<Command") {
		t.Fatalf("friend's player received the owner's command: %s", got.Body.String())
	}
}

func TestCompanionRelayCommandWithoutAnswerTimesOut(t *testing.T) {
	f := companionFixture(t)
	f.srv.companion.commandWait = 100 * time.Millisecond
	poll := startPoll(t, f, ownerToken)
	waitForClient(t, f, ownerToken)
	r := f.request(t, http.MethodGet, "/player/playback/stop?type=video", map[string]string{"X-Plex-Target-Client-Identifier": companionTV})
	if r.Code != http.StatusGatewayTimeout {
		t.Fatalf("unanswered command = %d, want 504", r.Code)
	}
	<-poll
}

func TestCompanionRelayTimelineReachesController(t *testing.T) {
	f := companionFixture(t)
	f.srv.companion.pollHold = 100 * time.Millisecond
	<-startPoll(t, f, ownerToken)
	timeline := `<MediaContainer location="fullScreenVideo"><Timeline type="video" state="playing" time="1000" duration="5000" ratingKey="550" /><Timeline type="music" state="stopped" /><Timeline type="photo" state="stopped" /></MediaContainer>`
	if r := f.requestBody(t, http.MethodPost, "/player/proxy/timeline?commandID=0", []byte(timeline), companionPlayerHeaders(ownerToken)); r.Code != http.StatusOK {
		t.Fatalf("timeline refused: %d", r.Code)
	}
	r := f.request(t, http.MethodGet, "/player/timeline/poll?wait=0&commandID=4", map[string]string{
		"X-Plex-Target-Client-Identifier": companionTV,
		"Origin":                          "https://app.plex.tv",
	})
	if r.Code != http.StatusOK || !strings.Contains(r.Body.String(), `state="playing"`) || !strings.Contains(r.Body.String(), `time="1000"`) {
		t.Fatalf("timeline poll %d: %s", r.Code, r.Body.String())
	}
	if r.Header().Get("X-Plex-Client-Identifier") != companionTV ||
		!strings.Contains(r.Header().Get("Access-Control-Expose-Headers"), "X-Plex-Client-Identifier") {
		t.Fatal("timeline does not expose the receiver identity")
	}
}

func TestCompanionRelayPlayerEndpointsNeedAUser(t *testing.T) {
	f := companionFixture(t)
	h := companionPlayerHeaders("")
	delete(h, "X-Plex-Token")
	h["X-Plex-Token"] = "not-a-token"
	if r := f.request(t, http.MethodGet, "/player/proxy/poll", h); r.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous poll = %d, want 401", r.Code)
	}
	// An answer for a command nobody sent is refused rather than accepted.
	if r := f.requestBody(t, http.MethodPost, "/player/proxy/response?commandID=99", []byte(`<Response code="200" status="OK" />`), companionPlayerHeaders(ownerToken)); r.Code != http.StatusNotFound {
		t.Fatalf("stray response = %d, want 404", r.Code)
	}
}

func attrValue(body, name string) string {
	i := strings.Index(body, " "+name+`="`)
	if i < 0 {
		return ""
	}
	rest := body[i+len(name)+3:]
	if j := strings.IndexByte(rest, '"'); j >= 0 {
		return rest[:j]
	}
	return ""
}
