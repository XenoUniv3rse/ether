package livetv

import (
	"context"
	"sort"
	"time"
)

// Usage is the Live TV console's live picture: who is watching which
// channel, and how much of each provider's allowance Ether is using.
type Usage struct {
	// Sessions counts live tunes; Viewers the distinct viewers behind them.
	Sessions int            `json:"sessions"`
	Viewers  int            `json:"viewers"`
	Watching []ChannelUsage `json:"watching"`
	Sources  []SourceUsage  `json:"sources"`
}

// ChannelUsage is one channel being watched.
type ChannelUsage struct {
	ChannelID string `json:"channelId"`
	Viewers   int    `json:"viewers"`
}

// SourceUsage is one source's provider connections and stream starts.
type SourceUsage struct {
	SourceID string `json:"sourceId"`
	// Open is how many provider connections Ether holds now.
	Open int `json:"open"`
	// StartsLastHour counts stream starts reserved in the last hour.
	StartsLastHour int `json:"startsLastHour"`
}

// LiveUsage reports current viewing and provider usage. It reads only
// in-memory state. Source limits come from the console’s existing source snapshot;
// this poll must never deserialize stored playlists or guides.
func (s *Service) LiveUsage(_ context.Context) (Usage, error) {
	u := Usage{Watching: []ChannelUsage{}, Sources: []SourceUsage{}}
	viewers := map[int64]bool{}
	byChannel := map[string]map[int64]bool{}
	s.mu.Lock()
	for _, ss := range s.sessions {
		u.Sessions++
		viewers[ss.info.UserID] = true
		id := ss.info.Channel.ID
		if byChannel[id] == nil {
			byChannel[id] = map[int64]bool{}
		}
		byChannel[id][ss.info.UserID] = true
	}
	s.mu.Unlock()
	u.Viewers = len(viewers)
	for id, vs := range byChannel {
		u.Watching = append(u.Watching, ChannelUsage{ChannelID: id, Viewers: len(vs)})
	}
	sort.Slice(u.Watching, func(i, j int) bool {
		if u.Watching[i].Viewers != u.Watching[j].Viewers {
			return u.Watching[i].Viewers > u.Watching[j].Viewers
		}
		return u.Watching[i].ChannelID < u.Watching[j].ChannelID
	})
	now := s.o.Now()
	reg := &s.inputRelay.taps
	reg.mu.Lock()
	defer reg.mu.Unlock()
	ids := map[string]bool{}
	for id := range reg.bySource {
		ids[id] = true
	}
	for id := range reg.pace {
		ids[id] = true
	}
	for id := range ids {
		su := SourceUsage{SourceID: id, Open: len(reg.bySource[id])}
		if p := reg.pace[id]; p != nil {
			for _, at := range p.opens {
				if now.Sub(at) < time.Hour && !at.After(now) {
					su.StartsLastHour++
				}
			}
		}
		u.Sources = append(u.Sources, su)
	}
	sort.Slice(u.Sources, func(i, j int) bool { return u.Sources[i].SourceID < u.Sources[j].SourceID })
	return u, nil
}
