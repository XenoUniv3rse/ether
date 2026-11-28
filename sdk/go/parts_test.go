package etherplugin_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	etherplugin "github.com/braedonsaunders/ether/sdk/go"
)

func TestStreamPartURLMockAndRemoteHost(t *testing.T) {
	ctx := context.Background()
	mock := etherplugin.NewMockHost()
	pctx := &etherplugin.Context{
		Context: ctx,
		Headers: map[string]string{"X-Ether-Viewer": "11"},
		Ether:   mock,
	}

	u, err := pctx.StreamPartURL("pk:recordings:multi", 2)
	if err != nil || !strings.HasPrefix(u, "/ether/v1/kind-media/") || !strings.HasSuffix(u, "?part=2") {
		t.Fatalf("mock StreamPartURL = %q, err = %v", u, err)
	}
	if mock.LastStreamURLViewer != 11 || mock.LastStreamURLItem != "pk:recordings:multi" || mock.LastStreamURLPart == nil || *mock.LastStreamURLPart != 2 {
		t.Fatalf("mock recorded viewer=%d item=%q part=%v", mock.LastStreamURLViewer, mock.LastStreamURLItem, mock.LastStreamURLPart)
	}
	if _, err := pctx.StreamPartURL("pk:recordings:multi", -1); err == nil {
		t.Fatal("negative partIndex accepted")
	}

	var seenBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/ether/plugin-host/frontend/stream-url" {
			http.NotFound(w, r)
			return
		}
		_ = json.NewDecoder(r.Body).Decode(&seenBody)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"url": "/ether/v1/kind-media/remote-part-ticket"})
	}))
	defer srv.Close()

	remote := etherplugin.NewRemoteHostClient(etherplugin.RemoteHostOptions{
		BaseURL:    srv.URL,
		Credential: "cred-1",
	})
	rctx := &etherplugin.Context{
		Context: ctx,
		Headers: map[string]string{"X-Ether-Viewer": "11"},
		Ether:   remote,
	}
	ru, err := rctx.StreamPartURL("pk:recordings:multi", 2)
	if err != nil || ru != "/ether/v1/kind-media/remote-part-ticket" {
		t.Fatalf("remote StreamPartURL = %q, err = %v", ru, err)
	}
	if seenBody["viewerId"] != float64(11) || seenBody["itemRef"] != "pk:recordings:multi" || seenBody["partIndex"] != float64(2) {
		t.Fatalf("remote payload = %+v", seenBody)
	}

	// Source-scoped options (with and without partIndex) on mock and remote hosts.
	part1 := 1
	su, err := pctx.StreamURLWithOptions("pk:recordings:multi", etherplugin.StreamURLOptions{PartIndex: &part1, Source: "pdf", Fallback: true})
	if err != nil || !strings.HasSuffix(su, "?part=1") {
		t.Fatalf("mock StreamURLWithOptions = %q, err = %v", su, err)
	}
	if !mock.LastStreamURLFallback || mock.LastStreamURLSource != "pdf" || mock.LastStreamURLPart == nil || *mock.LastStreamURLPart != 1 {
		t.Fatalf("mock recorded part=%v source=%q", mock.LastStreamURLPart, mock.LastStreamURLSource)
	}
	if _, err := pctx.StreamURLWithOptions("pk:recordings:multi", etherplugin.StreamURLOptions{Source: "   "}); err == nil {
		t.Fatal("blank source accepted")
	}

	seenBody = nil
	rsu, err := rctx.StreamURLWithOptions("pk:recordings:multi", etherplugin.StreamURLOptions{PartIndex: &part1, Source: "epub", Fallback: true})
	if err != nil || rsu != "/ether/v1/kind-media/remote-part-ticket" {
		t.Fatalf("remote StreamURLWithOptions = %q, err = %v", rsu, err)
	}
	if seenBody["viewerId"] != float64(11) || seenBody["itemRef"] != "pk:recordings:multi" || seenBody["partIndex"] != float64(1) || seenBody["source"] != "epub" || seenBody["fallback"] != true {
		t.Fatalf("remote source payload = %+v", seenBody)
	}
}
