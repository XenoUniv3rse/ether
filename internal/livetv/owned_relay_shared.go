package livetv

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

type relayOutputKey struct {
	channel, decision string
	video, audio      string
	height, bitrate   int
	copyOnly, hdr, dv bool
	cut               time.Duration
	viewer            int64 // personal channels are private to a viewer
}

type sharedRelay struct {
	ready    chan struct{}
	stopping chan struct{}
	refs     int
	p        *relayProducer
	stop     func()
}

// acquireOwnedRelay shares the selected output without sharing viewer state.
// Its input grant and encoder lifetime belong to the output, so any viewer
// can leave first without interrupting the remaining viewers.
func (s *Service) acquireOwnedRelay(ctx context.Context, ss *session, info core.MediaInfo, caps core.ClientCaps, dec core.Decision, cut time.Duration) (*ownedLease, error) {
	decision := dec
	decision.Reason = ""
	encoded, _ := json.Marshal(decision)
	ss.mu.Lock()
	key := relayOutputKey{
		channel: ss.info.Channel.ID, decision: string(encoded), cut: cut,
		video: strings.Join(caps.VideoCodecs, ","), audio: strings.Join(caps.AudioCodecs, ","),
		height: caps.MaxHeight, bitrate: caps.MaxBitrateKbps,
		copyOnly: caps.NoVideoTranscode, hdr: caps.HDR10, dv: caps.DolbyVision,
	}
	if ss.info.Channel.Personal != nil {
		key.viewer = ss.info.UserID
	}
	ss.mu.Unlock()

	state := &s.direct
	for {
		state.mu.Lock()
		if err := ctx.Err(); err != nil {
			state.mu.Unlock()
			return nil, err
		}
		sh := state.relays[key]
		if sh == nil {
			sh = &sharedRelay{ready: make(chan struct{})}
			if state.relays == nil {
				state.relays = make(map[relayOutputKey]*sharedRelay)
			}
			state.relays[key] = sh
			state.mu.Unlock()

			p, stop, err := func() (p *relayProducer, stop func(), err error) {
				defer func() {
					if recover() != nil {
						err = core.ErrNotAvailable
					}
				}()
				return s.openSharedRelay(ctx, ss, info, caps, dec, cut)
			}()
			state.mu.Lock()
			if err != nil {
				delete(state.relays, key)
			} else {
				sh.p, sh.stop, sh.refs = p, stop, 1
			}
			close(sh.ready)
			state.mu.Unlock()
			if err != nil {
				return nil, err
			}
			return s.sharedRelayLease(key, sh), nil
		}
		if sh.p != nil && sh.stopping == nil {
			sh.refs++
			state.mu.Unlock()
			s.revokeInput(ss) // discard the joiner's probe grant
			return s.sharedRelayLease(key, sh), nil
		}
		ready := sh.ready
		if sh.stopping != nil {
			ready = sh.stopping
		}
		state.mu.Unlock()
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-ready:
		}
	}
}

func (s *Service) openSharedRelay(ctx context.Context, viewer *session, info core.MediaInfo, caps core.ClientCaps, dec core.Decision, cut time.Duration) (*relayProducer, func(), error) {
	lifetime, cancel := context.WithCancel(s.serviceContext())
	viewer.mu.Lock()
	if viewer.closed || viewer.ctx != nil && viewer.ctx.Err() != nil {
		viewer.mu.Unlock()
		cancel()
		return nil, nil, core.ErrNotFound
	}
	input := &session{
		info: viewer.info, caps: viewer.caps, user: viewer.user, ctx: lifetime,
		inputSource: viewer.inputSource, inputURL: viewer.inputURL, inputHeaders: viewer.inputHeaders,
		sniffedManifest: viewer.sniffedManifest, steadyRefused: viewer.steadyRefused,
	}
	s.transferInput(viewer, input)
	viewer.mu.Unlock()
	started := false
	defer func() {
		if !started {
			cancel()
			s.revokeInput(input)
		}
	}()
	p := s.newRelayProducer(input, info, caps, dec, cut)
	p.shared = true
	if err := p.start(ctx, info, dec, cut, false); err != nil {
		cancel()
		viewer.mu.Lock()
		if !viewer.closed && (viewer.ctx == nil || viewer.ctx.Err() == nil) {
			s.transferInput(input, viewer)
		}
		viewer.mu.Unlock()
		s.revokeInput(input)
		return nil, nil, err
	}
	s.enableOwnedTimeshift(p.tl, input.info.Channel)
	go p.tl.run(lifetime)
	var stopOnce sync.Once
	stop := func() {
		stopOnce.Do(func() {
			cancel()
			defer s.revokeInput(input)
			p.close()
		})
	}
	go func() {
		defer stop()
		defer func() {
			if recover() != nil {
				p.tl.fail(core.ErrNotAvailable)
			}
		}()
		p.run(lifetime)
	}()
	started = true
	return p, stop, nil
}

func (s *Service) sharedRelayLease(key relayOutputKey, sh *sharedRelay) *ownedLease {
	return &ownedLease{tl: sh.p.tl, relay: sh.p, provider: sh.p.servingProvider, release: func() {
		state := &s.direct
		state.mu.Lock()
		sh.refs--
		last := sh.refs == 0
		if last {
			sh.stopping = make(chan struct{})
		}
		state.mu.Unlock()
		if last {
			defer func() {
				state.mu.Lock()
				delete(state.relays, key)
				close(sh.stopping)
				state.mu.Unlock()
			}()
			sh.stop()
		}
	}}
}

// publishOwnedLocked updates the viewer's lease and the input readers' view
// together. The grant itself stays owned by the producer.
func (ss *session) publishOwnedLocked(lease *ownedLease) {
	ss.owned = lease
	var owner *session
	if lease != nil && lease.relay != nil && lease.relay.shared {
		owner = lease.relay.sess.Load()
	}
	ss.sharedInput.Store(owner)
}
