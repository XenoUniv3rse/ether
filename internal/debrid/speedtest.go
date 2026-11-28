package debrid

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptrace"
	"sync"
	"sync/atomic"
	"time"

	"github.com/braedonsaunders/ether/internal/speedtest"
)

type playbackSpeedSample struct {
	at time.Time
	id string
	p  Payload
}

func (e *entry) rememberSpeedSample(at time.Time, id string, p Payload) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if p.Provider == e.cfg.ID && !p.IsDirect() && !p.IsPluginProvider() && !at.Before(e.lastSpeedSample.at) {
		e.lastSpeedSample = playbackSpeedSample{at: at, id: id, p: p}
	}
}

// SpeedTest reads one range from a previously played file through the
// existing CDN transport and admission gate. It never adds a torrent.
func (e *Engine) SpeedTest(ctx context.Context, id string, playback bool) speedtest.Result {
	r := speedtest.Result{Status: "unavailable", SpareOnly: playback}
	account := e.svc.entryByID(id)
	if account == nil {
		return r
	}
	if account.blocked(e.now()) != nil {
		r.Status = "paused"
		return r
	}
	account.mu.Lock()
	sample := account.lastSpeedSample
	account.mu.Unlock()
	if sample.at.IsZero() || e.now().Sub(sample.at) > 7*24*time.Hour {
		r.Status = "no_recent_file"
		return r
	}

	p := sample.p
	// Reuse the active file's current link and connection budget, if it
	// still represents the same file on the same account.
	e.mu.Lock()
	f := e.files[sample.id]
	e.mu.Unlock()
	if f != nil {
		current, _ := f.payload()
		if current.Provider != id || current.InfoHash != p.InfoHash || current.fileTag() != p.fileTag() {
			f = nil
		} else {
			p = current
		}
	}
	if p.URL == "" || (!p.Expires.IsZero() && p.Expires.Before(e.now().Add(speedtest.Duration))) {
		if !account.sem.tryAcquire(0) {
			r.Status, r.SpareOnly = "busy", true
			return r
		}
		refresh, cancel := context.WithTimeout(ctx, 4*time.Second)
		dl, err := account.p.Link(refresh, p.handle())
		if err == nil || refresh.Err() == nil {
			e.svc.observe(account, err)
		}
		cancel()
		account.release()
		if err != nil {
			if errors.Is(err, ErrGone) || errors.Is(err, ErrBlocked) {
				r.Status = "no_recent_file"
			}
			return r
		}
		p.URL, p.OnReplayEdge = dl.URL, false
	}

	test, cancel := context.WithTimeout(ctx, speedtest.Duration)
	defer cancel()
	var yielded atomic.Bool
	stopForPlayback := context.CancelFunc(func() {
		yielded.Store(true)
		cancel()
	})
	if f != nil {
		r.SpareOnly = r.SpareOnly || f.playback.Load() > 0 || len(f.conns) > 0
		if _, loaded := e.speedChecks.LoadOrStore(f.conns, stopForPlayback); loaded {
			r.Status, r.SpareOnly = "busy", true
			return r
		}
		defer e.speedChecks.Delete(f.conns)
		select {
		case f.conns <- struct{}{}:
			defer func() { <-f.conns }()
		default:
			r.Status, r.SpareOnly = "busy", true
			return r
		}
	}
	reserve := 0
	if r.SpareOnly {
		reserve = 1
	}
	var releases []func()
	defer func() {
		for _, release := range releases {
			release()
		}
	}()
	admitted := make(map[string]bool)
	admit := func(raw string) bool {
		addr := dialAddr(raw)
		if addr == "" {
			return false
		}
		if e.checkMediaPause(id, raw) != nil || e.backoff.down(urlHost(raw)) {
			r.Status = "paused"
			return false
		}
		if admitted[addr] {
			return true
		}
		release, spare := e.gate.trySpeedTest(addr, stopForPlayback, reserve)
		r.SpareOnly = r.SpareOnly || spare
		if release == nil {
			r.Status = "busy"
			return false
		}
		releases = append(releases, release)
		admitted[addr] = true
		return true
	}
	if !admit(p.URL) {
		return r
	}
	client := *e.hc
	redirectPolicy := client.CheckRedirect
	client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if p.OnReplayEdge || len(via) > 3 {
			return http.ErrUseLastResponse
		}
		if req.URL.User != nil || (req.URL.Scheme != "https" && req.URL.Scheme != "http") ||
			(via[len(via)-1].URL.Scheme == "https" && req.URL.Scheme != "https") {
			return http.ErrUseLastResponse
		}
		if redirectPolicy != nil {
			if err := redirectPolicy(req, via); err != nil {
				return err
			}
		}
		if !admit(req.URL.String()) {
			return http.ErrUseLastResponse
		}
		// A signed playback URL can appear as Go's default Referer.
		req.Header.Del("Referer")
		return nil
	}
	test = context.WithValue(test, speedDialKey{}, true)

	var traceMu sync.Mutex
	used := make(map[net.Conn]bool)
	test = httptrace.WithClientTrace(test, &httptrace.ClientTrace{
		GotConn: func(info httptrace.GotConnInfo) {
			traceMu.Lock()
			used[info.Conn] = used[info.Conn] || !info.Reused
			traceMu.Unlock()
		},
	})
	start := time.Now()
	fileSize := p.streamSize()
	from, to, size := p.wireRange(fileSize/8, fileSize-1)
	resp, _, err := e.send(test, &client, p.URL, from, to, nil)
	switch {
	case err != nil:
		if errors.Is(err, errSpeedDialBusy) {
			r.Status, r.SpareOnly = "busy", true
		} else if isConnRefused(err) {
			r.Refused = 1
		}
	case resp.StatusCode == http.StatusPartialContent && checkContentRange(resp.Header.Get("Content-Range"), from, to, size) == nil:
		r.Bytes, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, to-from+1))
	case resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode == http.StatusServiceUnavailable:
		wait := retryAfter(resp.Header.Get("Retry-After"))
		if resp.StatusCode == http.StatusTooManyRequests {
			r.Refused = 1
			e.reduceHostCap(resp.Request.URL.String())
		}
		if wait > 0 || resp.StatusCode == http.StatusTooManyRequests {
			e.pauseMedia(id, resp.Request.URL.String(), resp.StatusCode, wait)
		}
	case resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusGone:
		r.Status = "no_recent_file"
	}
	if resp != nil {
		resp.Body.Close()
	}
	traceMu.Lock()
	r.Connections = len(used)
	for _, fresh := range used {
		if fresh {
			r.Opened++
		}
	}
	traceMu.Unlock()
	if resp != nil {
		r.Connections = max(1, r.Connections)
	}
	if yielded.Load() {
		r.Status, r.SpareOnly = "busy", true
	}
	r.Measure(start)
	return r
}
