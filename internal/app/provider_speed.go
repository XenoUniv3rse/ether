package app

import (
	"context"
	"encoding/json"
	"log/slog"
	"slices"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/nzb"
	"github.com/braedonsaunders/ether/internal/speedtest"
	"github.com/braedonsaunders/ether/internal/store"
	"github.com/braedonsaunders/ether/internal/vault"
)

func init() {
	store.Register("provider_speed", 1, `
CREATE TABLE provider_speed_results (
    kind TEXT NOT NULL,
    provider_id TEXT NOT NULL,
    completed_at BIGINT NOT NULL,
    result TEXT NOT NULL,
    PRIMARY KEY(kind,provider_id)
);
`)
}

func (a *App) testProviderSpeed(ctx context.Context, kind, id string) speedtest.Result {
	r := speedtest.Result{Status: "unavailable"}
	cfg := a.cfg.Get()
	enabled := false
	switch kind {
	case "usenet":
		for _, p := range cfg.Usenet.Providers {
			enabled = enabled || (p.ID == id && p.Enabled)
		}
	case "debrid":
		for _, p := range cfg.Debrid.Providers {
			enabled = enabled || (p.ID == id && p.Enabled)
		}
	}
	if id == "" || !enabled {
		return r
	}
	key := [2]string{kind, id}
	if _, loaded := a.providerSpeedRuns.LoadOrStore(key, struct{}{}); loaded {
		r.Status = "already_running"
		return r
	}
	defer a.providerSpeedRuns.Delete(key)
	ctx, cancel := context.WithTimeout(ctx, 16*time.Second)
	defer cancel()
	playing, active := a.providerSpeedPlayback()
	switch kind {
	case "usenet":
		r = a.testUsenetSpeed(ctx, id, active)
	case "debrid":
		if a.media != nil {
			r = a.media.SpeedTest(ctx, id, active)
		}
	}
	r.At = time.Now().UTC()
	r.StreamsPlaying = playing
	if r.Status == "ok" {
		r.TypicalMbps, _ = a.playback.ProviderSpeedEvidence()
	}
	// Keep the last completed attempt, including a failed connection test.
	// Admission failures and missing samples do not replace a measurement.
	if r.Seconds > 0 && r.Status != "busy" && ctx.Err() == nil {
		a.saveSpeedResult(ctx, kind, id, r)
	}
	return r
}

// providerSpeedPlayback samples current playback before provider I/O begins.
func (a *App) providerSpeedPlayback() (playing int, active bool) {
	if a.playback == nil {
		return 0, false
	}
	for _, s := range a.playback.Sessions() {
		if s.PreparedOnly {
			continue
		}
		switch core.PlaySessionLiveness(s) {
		case core.SessionLivenessPlaying:
			playing++
			active = true
		case core.SessionLivenessStarting:
			active = true
		}
	}
	return playing, active
}

func (a *App) testUsenetSpeed(ctx context.Context, id string, active bool) speedtest.Result {
	r := speedtest.Result{Status: "unavailable"}
	sampler, ok := a.vault.(interface {
		SpeedSampleKeys(context.Context) ([]vault.Key, error)
	})
	if !ok || a.fetcher == nil {
		return r
	}
	prep, cancel := context.WithTimeout(ctx, 5*time.Second)
	keys, err := sampler.SpeedSampleKeys(prep)
	if err != nil {
		cancel()
		return r
	}
	if len(keys) == 0 {
		cancel()
		r.Status = "empty_vault"
		return r
	}
	type sample struct {
		doc    *nzb.NZB
		posted time.Time
	}
	var samples []sample
	for _, key := range keys {
		if prep.Err() != nil {
			break
		}
		record, err := a.vault.Get(prep, key)
		if err != nil || record.NZB == nil {
			continue
		}
		var posted time.Time
		for _, file := range record.NZB.Files {
			if file != nil && file.Date.After(posted) {
				posted = file.Date
			}
		}
		samples = append(samples, sample{record.NZB, posted})
	}
	cancel()
	if len(samples) == 0 {
		r.Status = "no_readable_nzb"
		return r
	}
	// Prefer older posts within the bounded vault sample, with undated
	// releases last. Availability is always checked against this provider.
	slices.SortStableFunc(samples, func(left, right sample) int {
		switch {
		case left.posted.IsZero() && !right.posted.IsZero():
			return 1
		case right.posted.IsZero() && !left.posted.IsZero():
			return -1
		default:
			return left.posted.Compare(right.posted)
		}
	})
	docs := make([]*nzb.NZB, 0, len(samples))
	for _, sample := range samples {
		docs = append(docs, sample.doc)
	}
	return a.fetcher.SpeedTest(ctx, id, docs, active)
}

func (a *App) saveSpeedResult(ctx context.Context, kind, id string, r speedtest.Result) {
	if a.db == nil {
		return
	}
	raw, err := json.Marshal(r)
	if err == nil {
		_, err = a.db.ExecContext(ctx, `INSERT INTO provider_speed_results (kind,provider_id,completed_at,result)
VALUES (?,?,?,?) ON CONFLICT(kind,provider_id) DO UPDATE SET completed_at=excluded.completed_at,result=excluded.result
WHERE provider_speed_results.completed_at <= excluded.completed_at`, kind, id, r.At.UnixMilli(), string(raw))
	}
	if err != nil {
		a.log.Log(ctx, slog.LevelWarn, "could not save provider speed test result")
	}
}

func (a *App) supportProviderSpeed(kind string) map[string]speedtest.Result {
	cfg := a.cfg.Get()
	var ids []string
	if kind == "usenet" {
		for _, p := range cfg.Usenet.Providers {
			ids = append(ids, p.ID)
		}
	} else {
		for _, p := range cfg.Debrid.Providers {
			ids = append(ids, p.ID)
		}
	}
	results := make(map[string]speedtest.Result)
	if a.db == nil {
		return results
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	for _, id := range ids {
		if ctx.Err() != nil {
			break
		}
		var raw string
		var result speedtest.Result
		if err := a.db.QueryRowContext(ctx, `SELECT result FROM provider_speed_results WHERE kind=? AND provider_id=?`, kind, id).Scan(&raw); err == nil &&
			json.Unmarshal([]byte(raw), &result) == nil {
			results[id] = result
		}
	}
	return results
}

func (a *App) supportUsenetSpeed() (map[string]speedtest.Result, []speedtest.Observation) {
	_, observed := a.playback.ProviderSpeedEvidence()
	return a.supportProviderSpeed("usenet"), observed
}
