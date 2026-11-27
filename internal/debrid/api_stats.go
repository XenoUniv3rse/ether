package debrid

import (
	"context"
	"maps"
	"net/http"
	"strings"
	"sync/atomic"
)

type APICallStats struct {
	Attempts       int64            `json:"attempts"`
	Endpoints      map[string]int64 `json:"endpoints"`
	Retries        int64            `json:"retries"`
	Limited        int64            `json:"limited"`
	HTTP429        int64            `json:"http429"`
	ListHits       int64            `json:"listHits"`
	ReuseHits      int64            `json:"reuseHits"`
	RememberedHits int64            `json:"rememberedHits"`
	FirstPlays     int64            `json:"firstPlays"`
	FirstPlayCalls int64            `json:"firstPlayCalls"`
}

type apiPlayKey struct{}

type apiPlayCalls struct {
	account *accountPacer
	calls   atomic.Int64
}

func endpointClass(r apiRequest) string {
	switch r.path {
	case "/torrents/addMagnet", "/v4/magnet/upload", "/seedbox/add", "/torrents/createtorrent", "/transfer/create", "/cloud":
		return "add"
	case "/unrestrict/link", "/v4/link/unlock", "/torrents/requestdl", "/transfer/directdl":
		return "link"
	case "/torrents/checkcached", "/cache/check", "/cache/info":
		return "availability"
	case "/user", "/v4/user", "/user/me", "/account/info", "/account/infos":
		return "account"
	case "/torrents", "/cloud/history":
		return "list"
	case "/seedbox/list", "/torrents/mylist", "/v4.1/magnet/status":
		if r.query.Get("id") != "" || r.query.Get("ids") != "" {
			return "info"
		}
		return "list"
	case "/v4/magnet/files", "/cloud/status":
		return "info"
	case "/v4/magnet/delete", "/cloud/remove", "/torrents/controltorrent":
		return "delete"
	}
	switch {
	case strings.HasPrefix(r.path, "/torrents/selectFiles/"):
		return "select"
	case strings.HasPrefix(r.path, "/torrents/info/"), strings.HasPrefix(r.path, "/cloud/explore/"):
		return "info"
	case r.method == http.MethodDelete:
		return "delete"
	default:
		return "other"
	}
}

func (p *accountPacer) noteAttempt(ctx context.Context, endpoint string, retry bool) {
	p.mu.Lock()
	if p.api.Endpoints == nil {
		p.api.Endpoints = make(map[string]int64)
	}
	p.api.Endpoints[endpoint]++
	p.api.Attempts++
	if retry {
		p.api.Retries++
	}
	p.mu.Unlock()
	if play, _ := ctx.Value(apiPlayKey{}).(*apiPlayCalls); play != nil && play.account == p {
		play.calls.Add(1)
	}
}

func (p *accountPacer) noteLimit(http429 bool) {
	p.mu.Lock()
	p.api.Limited++
	if http429 {
		p.api.HTTP429++
	}
	p.mu.Unlock()
}

func (p *accountPacer) noteRemembered() {
	p.mu.Lock()
	p.api.RememberedHits++
	p.mu.Unlock()
}

func (p *accountPacer) apiStats() APICallStats {
	p.mu.Lock()
	defer p.mu.Unlock()
	stats := p.api
	stats.Endpoints = maps.Clone(stats.Endpoints)
	return stats
}
