package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

// Lab credentials must remain on the explicitly selected Ether origin.
func TestLabOriginBoundary(t *testing.T) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		t.Fatal(err)
	}
	token := hex.EncodeToString(raw[:])
	var otherHits atomic.Int32
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { otherHits.Add(1) }))
	defer other.Close()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Plex-Token") != token {
			t.Error("missing lab credential")
		}
		switch r.URL.Path {
		case "/ether/api/health":
			fmt.Fprint(w, `{"ok":true,"plexEnabled":true,"setupComplete":true}`)
		case "/redirect":
			http.Redirect(w, r, other.URL, http.StatusFound)
		default:
			w.WriteHeader(200)
		}
	}))
	defer server.Close()
	lab, err := newLabClient(context.Background(), server.URL, token)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = lab.get(context.Background(), other.URL); err == nil {
		t.Fatal("accepted cross-origin media URL")
	}
	if _, _, err = lab.get(context.Background(), "/redirect"); err == nil {
		t.Fatal("accepted cross-origin redirect")
	}
	if otherHits.Load() != 0 {
		t.Fatal("contacted another origin")
	}
	pms := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "<MediaContainer />") }))
	defer pms.Close()
	if _, err = newLabClient(context.Background(), pms.URL, token); err == nil {
		t.Fatal("accepted a non-Ether target")
	}
}
