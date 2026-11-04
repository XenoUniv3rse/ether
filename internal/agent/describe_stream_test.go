package agent

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/agent/tools"
	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/llm"
	"github.com/braedonsaunders/ether/internal/store"
)

// A Describe a collection turn must outlive everything that used to end a
// long build: the model client's whole-request timeout, a quiet model, and
// the browser connection that started it. The fake model answers in two
// parts and stays silent in between until the test lets it finish.
func TestDescribeTurnOutlivesClientTimeoutQuietModelAndDisconnect(t *testing.T) {
	release := make(chan struct{})
	modelSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		send := func(v any) {
			b, _ := json.Marshal(v)
			fmt.Fprintf(w, "data: %s\n\n", b)
			w.(http.Flusher).Flush()
		}
		delta := func(text string) map[string]any {
			return map[string]any{"choices": []any{map[string]any{"delta": map[string]any{"content": text}}}}
		}
		send(delta("Looking for the best films of the decade. "))
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		send(delta("Here is your collection."))
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer modelSrv.Close()

	const oldCap = 50 * time.Millisecond // stands in for the old 120 s client cap
	model := llm.NewClient(llm.ClientOptions{
		Config:     config.LLM{Enabled: true, Provider: config.LLMProviderCustom, BaseURL: modelSrv.URL + "/v1", Model: "fake-chat"},
		HTTPClient: &http.Client{Timeout: oldCap},
	})
	const beat = 10 * time.Millisecond
	svc := New(Options{Model: model, Catalogue: tools.NewCatalogue(), Store: NewStore(store.OpenTest(t)), Heartbeat: beat})
	turnReturned := make(chan struct{})
	h := svc.Handler()
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.ServeHTTP(w, r)
		if r.URL.Path == "/turn" {
			close(turnReturned)
		}
	}))
	defer api.Close()

	ctx, disconnect := context.WithCancel(context.Background())
	body := `{"prompt":"top 10 movies of 2010 to 2020","context":{"route":"` + DescribeCollectionRoute + `","mediaType":"movie"}}`
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, api.URL+"/turn", strings.NewReader(body))
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	thread := res.Header.Get("X-Ether-Thread-Id")
	if res.StatusCode != http.StatusOK || thread == "" {
		t.Fatalf("turn: %d thread %q", res.StatusCode, thread)
	}

	// The first words stream in, then keep-alives hold the quiet stream
	// open. Tickers never fire early, so 6 beats outlast the old cap.
	sc := bufio.NewScanner(res.Body)
	sawText, beats := false, 0
	for sc.Scan() && (!sawText || beats < 6) {
		line := sc.Text()
		if strings.Contains(line, `"text-delta"`) {
			sawText = true
		}
		if sawText && line == ": keep-alive" {
			beats++
		}
	}
	if !sawText || beats < 6 {
		t.Fatalf("stream ended early: text %v, keep-alives %d (%v)", sawText, beats, sc.Err())
	}
	if beats*int(beat) <= int(oldCap) {
		t.Fatalf("test did not outlast the old cap")
	}

	// The tab closes mid-build: its request ends, the turn carries on.
	disconnect()
	res.Body.Close()
	<-turnReturned
	if !svc.Running(thread) {
		t.Fatal("turn stopped when its connection closed")
	}
	close(release)

	deadline := time.Now().Add(10 * time.Second)
	var page struct {
		Running  bool      `json:"running"`
		Messages []Message `json:"messages"`
	}
	for {
		r, err := http.Get(api.URL + "/threads/" + thread + "/messages")
		if err != nil {
			t.Fatal(err)
		}
		page.Running, page.Messages = false, nil
		err = json.NewDecoder(r.Body).Decode(&page)
		r.Body.Close()
		if err != nil {
			t.Fatal(err)
		}
		if !page.Running {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("turn never finished")
		}
		time.Sleep(5 * time.Millisecond)
	}
	last := page.Messages[len(page.Messages)-1]
	if last.Role != "assistant" || !strings.Contains(string(last.Parts), "Here is your collection.") {
		t.Fatalf("rejoined transcript is missing the finished answer: %s", last.Parts)
	}
}
