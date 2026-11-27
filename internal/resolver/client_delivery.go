package resolver

import (
	"slices"
	"sync"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

const (
	clientSampleInterval = 2 * time.Second
	clientSampleLifetime = 90 * time.Second
	clientSampleCount    = 5
	maxClientEstimates   = 1024
)

type clientDeliveryKey struct {
	viewerID int64
	device   string
	address  string
}

type clientDeliverySample struct {
	at  time.Time
	bps float64
}

type clientDeliveryState struct {
	mu      sync.Mutex
	samples map[clientDeliveryKey][clientSampleCount]clientDeliverySample
}

func clientDeliveryIdentity(viewerID int64, caps core.ClientCaps) (clientDeliveryKey, bool) {
	return clientDeliveryKey{viewerID, caps.ClientID, caps.ClientIP},
		caps.Protocol == "vela" && !caps.NoPlaybackTelemetry && !caps.Download &&
			viewerID > 0 && caps.ClientID != "" && caps.ClientIP != ""
}

func (s *Service) ObserveClientThroughput(viewerID int64, caps core.ClientCaps, bps float64) {
	key, ok := clientDeliveryIdentity(viewerID, caps)
	if !ok || !core.ValidClientThroughput(bps) {
		return
	}
	now := s.now()
	state := &s.clientDelivery
	state.mu.Lock()
	defer state.mu.Unlock()
	recent, exists := state.samples[key]
	if exists && now.Sub(recent[0].at) < clientSampleInterval {
		return
	}
	if !exists && len(state.samples) >= maxClientEstimates {
		for k, v := range state.samples {
			if now.Sub(v[0].at) >= clientSampleLifetime {
				delete(state.samples, k)
			}
		}
		if len(state.samples) >= maxClientEstimates {
			return
		}
	}
	if state.samples == nil {
		state.samples = make(map[clientDeliveryKey][clientSampleCount]clientDeliverySample)
	}
	copy(recent[1:], recent[:len(recent)-1])
	recent[0] = clientDeliverySample{at: now, bps: bps}
	state.samples[key] = recent
}

func (s *Service) ClientDeliveryBudgetKbps(viewerID int64, caps core.ClientCaps) int {
	key, ok := clientDeliveryIdentity(viewerID, caps)
	if !ok {
		return 0
	}
	state := &s.clientDelivery
	state.mu.Lock()
	recent := state.samples[key]
	state.mu.Unlock()
	now := s.now()
	var rates [clientSampleCount]float64
	n := 0
	for _, sample := range recent {
		if !sample.at.IsZero() && now.Sub(sample.at) < clientSampleLifetime {
			rates[n] = sample.bps
			n++
		}
	}
	if n < 3 {
		return 0
	}
	slices.Sort(rates[:n])
	// Use the lower median for an even window, reserve 25% for bitrate
	// bursts, and bucket the budget for stable resolver/detail cache keys.
	return max(1, int(rates[(n-1)/2]*.75/500_000)*500)
}

func (s *Service) withClientDeliveryBudget(req core.ResolveRequest) core.ResolveRequest {
	if req.User != nil && !req.PreferredOnly && core.IsAutoVersionSelector(req.Prefer) {
		req.ClientDeliveryBudgetKbps = s.ClientDeliveryBudgetKbps(req.User.ID, req.Client)
	}
	return req
}

func applyClientDeliveryFit(bandwidth int, bps float64, budgetKbps int) int {
	if budgetKbps > 0 && bps > float64(budgetKbps)*1000 {
		return -1
	}
	return bandwidth
}

// Mark a connection preference only when a candidate that would otherwise
// outrank this pick was demoted by delivery evidence. Compare the same policy,
// language and quality keys so a fast but independently preferred pick does
// not get a bitrate explanation.
func markAutoDeliveryReasons(cands []candidate, title *core.Title) {
	withoutDelivery := func(c candidate) candidate {
		c.bandwidth = c.preDeliveryBandwidth
		return c
	}
	var demoted *candidate
	for n := range cands {
		c := &cands[n]
		if c.deliveryDemotion != "" && (demoted == nil || compareCandidates(withoutDelivery(*c), withoutDelivery(*demoted))) {
			demoted = c
		}
	}
	if demoted == nil {
		return
	}
	demotedRate := releaseBitsPerSecond(sizedForEpisode(demoted.rel), title)
	for n := range cands {
		c := &cands[n]
		if c.bandwidth >= 0 && compareCandidates(withoutDelivery(*demoted), withoutDelivery(*c)) &&
			compareCandidates(*c, *demoted) {
			rate := releaseBitsPerSecond(sizedForEpisode(c.rel), title)
			if rate > 0 && rate < demotedRate {
				c.autoReason = demoted.deliveryDemotion
			}
		}
	}
}
