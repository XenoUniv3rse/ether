package app

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/braedonsaunders/ether/internal/vault"
	"github.com/braedonsaunders/ether/internal/vault/importer"
)

var vaultHealthNames = []string{"unknown", "ok", "dead"}

func vaultJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
func vaultError(w http.ResponseWriter, err error) {
	if errors.Is(err, vault.ErrNZBRetained) {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	if errors.Is(err, vault.ErrNotFound) {
		http.Error(w, "Release not found", 404)
		return
	}
	http.Error(w, "Library unavailable", 503)
}

type vaultProgress struct {
	ID        string `json:"id"`
	State     string `json:"state"`
	Imported  int    `json:"imported"`
	Failed    int    `json:"failed"`
	Completed int    `json:"completed"`
}

func (a *App) registerVaultAdmin(mux interface {
	HandleFunc(string, func(http.ResponseWriter, *http.Request))
}) {
	route := func(pattern string, h http.HandlerFunc) {
		mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
			if a.vault == nil {
				http.Error(w, "Library unavailable", 503)
				return
			}
			h(w, r)
		})
	}
	route("GET /vault", func(w http.ResponseWriter, r *http.Request) {
		s, err := a.vault.Stats(r.Context())
		if err != nil {
			vaultError(w, err)
			return
		}
		vaultJSON(w, 200, map[string]any{"releases": s.Releases, "bytes": s.Bytes, "capBytes": s.CapBytes, "grabsAvoided": s.GrabsAvoided, "searchesAvoided": s.SearchesAvoided})
	})
	route("GET /vault/releases", func(w http.ResponseWriter, r *http.Request) {
		l, ok := a.vault.(vault.Lister)
		if !ok {
			http.Error(w, "Library unavailable", 503)
			return
		}
		q := r.URL.Query()
		limit := 50
		if v := q.Get("limit"); v != "" {
			n, e := strconv.Atoi(v)
			if e != nil || n < 1 || n > 500 {
				http.Error(w, "Invalid limit", 400)
				return
			}
			limit = n
		}
		kind := vault.Kind(q.Get("kind"))
		if kind != "" && kind != vault.KindTorrent && kind != vault.KindUsenet {
			http.Error(w, "Invalid kind", 400)
			return
		}
		var health *vault.Health
		if v := q.Get("health"); v != "" {
			for i, name := range vaultHealthNames {
				if name == v {
					h := vault.Health(i)
					health = &h
					break
				}
			}
			if health == nil {
				http.Error(w, "Invalid health", 400)
				return
			}
		}
		entries, err := l.List(r.Context(), q.Get("q"), kind, health, limit+1, q.Get("cursor"))
		if err != nil {
			vaultError(w, err)
			return
		}
		next := ""
		if len(entries) > limit {
			entries = entries[:limit]
			next = string(entries[limit-1].Key)
		}
		rows := make([]map[string]any, 0, len(entries))
		for _, e := range entries {
			rows = append(rows, map[string]any{"key": e.Key, "kind": e.Kind, "name": e.Name, "size": e.Size, "added": e.Added, "lastUsed": e.LastUsed, "uses": e.Uses, "health": vaultHealthNames[e.Health], "source": e.Source})
		}
		vaultJSON(w, 200, map[string]any{"rows": rows, "nextCursor": next})
	})
	route("DELETE /vault/{key}", func(w http.ResponseWriter, r *http.Request) {
		if err := a.vault.Purge(r.Context(), vault.Key(r.PathValue("key"))); err != nil {
			vaultError(w, err)
			return
		}
		w.WriteHeader(204)
	})
	route("POST /vault/{key}/check", a.checkVaultRecord)
	var mu sync.Mutex
	var jobs []vaultProgress
	importCtx, stopImports := context.WithCancel(context.Background())
	var active sync.WaitGroup
	a.closers = append(a.closers, func() { mu.Lock(); stopImports(); mu.Unlock(); active.Wait() })
	route("GET /vault/import/{id}", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		for _, job := range jobs {
			if job.ID == r.PathValue("id") {
				vaultJSON(w, 200, job)
				return
			}
		}
		http.NotFound(w, r)
	})
	route("POST /vault/import", func(w http.ResponseWriter, r *http.Request) {
		epoch := int64(0)
		if a.cluster != nil {
			epoch = a.cluster.Epoch()
		}
		guard := func() error {
			if a.cluster != nil && (!a.cluster.IsLeader() || a.cluster.Epoch() != epoch) {
				return errors.New("leadership changed")
			}
			return nil
		}
		if guard() != nil {
			http.Error(w, "Import requires leader", 409)
			return
		}
		path, cleanup, err := vaultImportPath(w, r)
		if err != nil {
			http.Error(w, "Invalid import", 400)
			return
		}
		mu.Lock()
		if importCtx.Err() != nil {
			mu.Unlock()
			cleanup()
			http.Error(w, "Server stopping", 503)
			return
		}
		if len(jobs) > 0 && jobs[len(jobs)-1].State == "running" {
			mu.Unlock()
			cleanup()
			http.Error(w, "Import already running", 409)
			return
		}
		id := rand.Text()
		if len(jobs) >= 32 {
			jobs = jobs[1:]
		}
		jobs = append(jobs, vaultProgress{ID: id, State: "running"})
		active.Add(1)
		mu.Unlock()
		go func() {
			defer active.Done()
			defer cleanup()
			err := importer.Run(importCtx, a.vault, path, guard, func(success, completed bool) {
				mu.Lock()
				defer mu.Unlock()
				j := &jobs[len(jobs)-1]
				if success {
					j.Imported++
					if completed {
						j.Completed++
					}
				} else {
					j.Failed++
				}
			})
			mu.Lock()
			defer mu.Unlock()
			state := "done"
			if err != nil {
				state = "failed"
			}
			jobs[len(jobs)-1].State = state
		}()
		vaultJSON(w, 202, map[string]string{"id": id})
	})
}
func vaultImportPath(w http.ResponseWriter, r *http.Request) (string, func(), error) {
	media, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if media != "multipart/form-data" {
		var b struct{ Path string }
		err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16384)).Decode(&b)
		if err == nil && strings.TrimSpace(b.Path) == "" {
			err = errors.New("empty path")
		}
		return b.Path, func() {}, err
	}
	r.Body = http.MaxBytesReader(w, r.Body, 2<<30)
	m, err := r.MultipartReader()
	if err != nil {
		return "", nil, err
	}
	for {
		p, err := m.NextPart()
		if err != nil {
			return "", nil, err
		}
		if p.FileName() == "" {
			continue
		}
		f, err := os.CreateTemp("", "ether-vault-*.zip")
		if err != nil {
			return "", nil, err
		}
		cleanup := func() { _ = os.Remove(f.Name()) }
		_, err = io.Copy(f, p)
		closeErr := f.Close()
		if err = errors.Join(err, closeErr); err != nil {
			cleanup()
			return "", nil, err
		}
		return f.Name(), cleanup, nil
	}
}
func (a *App) checkVaultRecord(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	k := vault.Key(r.PathValue("key"))
	rec, err := a.vault.Get(ctx, k)
	if err != nil {
		vaultError(w, err)
		return
	}
	h := vault.HealthUnknown
	if rec.Torrent != nil {
		if a.debrid != nil {
			hash := rec.Torrent.InfoHash
			cached, def := a.debrid.VerifyCached(ctx, hash)
			if cached {
				h = vault.HealthOK
			} else if def {
				h = vault.HealthDead
			}
		}
	} else {
		v, ok := any(a.fetcher).(vault.Verifier)
		if !ok || a.fetcher == nil {
			http.Error(w, "Verifier unavailable", 503)
			return
		}
		alive, err := vault.VerifyRecord(ctx, rec, v)
		if err != nil {
			vaultError(w, err)
			return
		}
		h = vault.HealthDead
		if alive {
			h = vault.HealthOK
		}
	}
	if err := a.vault.MarkHealth(ctx, k, h); err != nil {
		vaultError(w, err)
		return
	}
	vaultJSON(w, 200, map[string]string{"health": vaultHealthNames[h]})
}

// vaultVerifier is the verified article check library-first relies on:
// the Usenet fetcher once it distrusts lying STAT per provider (W119).
// Without it, library-first only replays releases verified within the hour.
func (a *App) vaultVerifier() vault.Verifier {
	if v, ok := any(a.fetcher).(vault.Verifier); ok {
		return v
	}
	return nil
}
