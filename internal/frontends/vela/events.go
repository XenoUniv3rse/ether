package vela

import (
	"context"
	"errors"
	"github.com/braedonsaunders/ether/internal/frontend"
	"time"
)

// playingReleasePollEvery paces the live read that notices an in-session
// failover to another release.
const playingReleasePollEvery = 2 * time.Second

// Subscribe only after a viewer opens playback. The broker owns stop and remote
// control events; disabling the frontend cancels every subscription.
func (i *instance) watchSession(viewer *frontend.Viewer, device frontend.Device, id, scope string, s *playSession) {
	base := i.ctx
	if base == nil {
		base = context.Background()
	}
	ctx, cancel := context.WithCancel(base)
	var events <-chan frontend.Event
	if s.archive == nil {
		events, _ = i.host.Sessions.Events(ctx, frontend.EventScope{Viewer: viewer, Protocol: "vela", SessionID: id, Device: device})
	}
	go func() {
		defer cancel()
		ticker := time.NewTicker(15 * time.Second)
		defer ticker.Stop()

		var liveEventC <-chan time.Time
		if i.host.LiveEventLink != nil {
			s.mu.Lock()
			isLiveChannel := s.channelID != ""
			s.mu.Unlock()
			if isLiveChannel {
				lt := time.NewTicker(i.liveEventPollInterval())
				defer lt.Stop()
				liveEventC = lt.C
			}
		}

		var trickplayC <-chan time.Time
		if _, ok := i.host.Sessions.(frontend.SessionTrickplay); ok {
			s.mu.Lock()
			eligible := s.isTrickplayEligibleLocked()
			s.mu.Unlock()
			if eligible {
				tt := time.NewTicker(i.trickplayPollEvery())
				defer tt.Stop()
				trickplayC = tt.C
			}
		}

		// An in-session failover swaps the release under the same session;
		// a cheap live read notices it so the menu names what plays now.
		var playingC <-chan time.Time
		if _, ok := i.host.Sessions.(frontend.PlayingReleaseReader); ok && i.versionsAvailable() {
			s.mu.Lock()
			vod := s.item != 0 && s.channelID == ""
			s.mu.Unlock()
			if vod {
				pt := time.NewTicker(playingReleasePollEvery)
				defer pt.Stop()
				playingC = pt.C
			}
		}

		t := time.NewTicker(250 * time.Millisecond)
		defer t.Stop()
		output := t.C
		for {
			select {

			case <-liveEventC:
				i.refreshSessionLiveEvent(ctx, viewer, s)

			case <-playingC:
				i.pollPlayingRelease(ctx, viewer, device, id, s)

			case <-trickplayC:
				i.pollSessionTrickplay(ctx, viewer, device, id, s)

			case <-output:
				s.mu.Lock()
				if !s.ended.IsZero() {
					s.mu.Unlock()
					return
				}
				if s.commandActive {
					s.mu.Unlock()
					continue
				}
				copy, cursor := s.outputSnapshot(), s.cursor
				s.mu.Unlock()
				changed := i.refreshOutput(ctx, id, copy)
				s.mu.Lock()
				if changed && !s.commandActive && s.cursor == cursor && s.source == copy.source && s.seek == copy.seek && s.ended.IsZero() {
					s.applyOutput(copy)
					alignUnobservedLivePosition(s)
					s.publish("plan")
				}
				s.mu.Unlock()
			case <-ctx.Done():
				return
			case <-ticker.C:
				resolver, ok := i.host.Identity.(frontend.GrantScopes)
				if !ok {
					return
				}
				_, err := resolver.GrantScopeCurrent(ctx, scope, device)
				if err != nil && !errors.Is(err, frontend.ErrUnauthorized) && !errors.Is(err, frontend.ErrNotFound) {
					continue
				}
				if err != nil {
					_ = i.stopPlayback(ctx, frontend.SessionAccess{Viewer: viewer, Device: device, Protocol: "vela"}, id, s)
				}
				s.mu.Lock()
				var archiveTune string
				if s.archive != nil {
					archiveTune = s.archive.tuneID
				}
				s.mu.Unlock()
				if err == nil && archiveTune != "" {
					_, lookupErr := i.host.LiveTV.Session(ctx, archiveTune, viewer)
					if errors.Is(lookupErr, frontend.ErrNotFound) {
						s.mu.Lock()
						if s.archive.tuneID == archiveTune && !s.commandActive {
							err = lookupErr
						}
						s.mu.Unlock()
					}
				}
				s.mu.Lock()
				ended := !s.ended.IsZero()
				if err != nil && !ended {
					s.ended = time.Now()
					s.clearDisconnectTimerLocked()
					s.plan["state"] = "ended"
					s.publish("ended")
				}
				s.mu.Unlock()
				if err != nil || ended {
					return
				}
			case e, ok := <-events:
				if !ok {
					events = nil
					continue
				}
				if e.SessionID != id {
					continue
				}
				if e.Type == "SessionOriginalChanged" {
					i.pollOriginalRecovery(ctx, viewer, device, id, s)
					continue
				}
				s.mu.Lock()
				if !s.ended.IsZero() {
					s.mu.Unlock()
					return
				}
				switch e.Type {
				case "SessionStopped":
					s.plan["state"] = "ended"
					s.ended = time.Now()
					s.clearDisconnectTimerLocked()
					s.publish("ended")
					s.mu.Unlock()
					return
				case "SessionCommand":
					if e.Command != nil {
						switch e.Command.Kind {
						case frontend.CommandPause:
							s.plan["state"] = "paused"
							s.publish("plan")
						case frontend.CommandUnpause:
							s.plan["state"] = "playing"
							s.publish("plan")
						}
					}
				}
				s.mu.Unlock()
			}
		}
	}()
}
