package livetv

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"html"
	"math"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/transcode"
)

var dashURL = regexp.MustCompile(`(initialization|media)="([^"]+)"`)

func (s *Service) ServeDASH(w http.ResponseWriter, r *http.Request, id string, u *core.User) error {
	ss, err := s.getSession(r.Context(), id, u)
	if err != nil {
		writeStreamError(w, err)
		return nil
	}
	ss.mu.Lock()
	ss.consumed = true
	shared := ss.broadcast != nil
	owned := ss.owned
	hasDash := ss.dash != nil
	archive := ss.catchUp
	ss.mu.Unlock()
	if archive != nil {
		return s.serveCatchUpDASH(w, r, ss, archive)
	}
	if shared {
		return s.serveBroadcastDASH(w, r, ss)
	}
	if owned != nil && !hasDash && (owned.tl.isFMP4() || broadcastUsesFMP4(ss.caps)) {
		if s.serveOwnedDASH(w, r, ss, owned) {
			return nil
		}
	}
	ds, err := s.ensureDASH(r.Context(), ss, id)
	if err != nil {
		var dashErr *VirtualDASHError
		if errors.As(err, &dashErr) {
			return err
		}
		writeStreamError(w, err)
		return nil
	}
	rec := httptest.NewRecorder()
	ds.ServeManifest(rec, r)
	if rec.Code != 200 {
		http.Error(w, "Live stream unavailable", rec.Code)
		return nil
	}
	prefix := "session/livetv-dash-" + id + "/"
	body := dashURL.ReplaceAllStringFunc(rec.Body.String(), func(attr string) string {
		parts := dashURL.FindStringSubmatch(attr)
		target := strings.Replace(parts[2], prefix, "/livetv/sessions/"+id+"/0/dash/", 1)
		return parts[1] + `="` + html.EscapeString(withAuth(target, ss.streamGrant)) + `"`
	})
	w.Header().Set("Content-Type", "application/dash+xml")
	w.Header().Set("Cache-Control", "no-store")
	if r.Method != "HEAD" {
		fmt.Fprint(w, body)
	}
	return nil
}

// serveOwnedDASH exposes an fMP4 ownedTimeline directly as a dynamic MPEG-DASH
// manifest with separate video/audio AdaptationSets, avoiding a second ffmpeg
// process and matching the exact fMP4 media timestamps of every copied segment.
func (s *Service) serveOwnedDASH(w http.ResponseWriter, r *http.Request, ss *session, lease *ownedLease) bool {
	lease.tl.mu.Lock()
	target := max(lease.tl.target, lease.tl.targetFloor, s.latencyPolicy().SegmentDuration())
	lease.tl.mu.Unlock()
	ctx, cancel := context.WithTimeout(r.Context(), target)
	defer cancel()
	var err error
	if lease.relay != nil {
		err = waitOwnedRelayStart(r.Context(), ctx, lease)
	} else {
		err = lease.tl.waitStarted(ctx)
	}
	if err != nil {
		return false
	}
	now := s.o.Now()
	origin, tlTarget, periods, ok := lease.tl.dashSnapshot(now)
	if !ok {
		return false
	}
	policy := s.latencyPolicy()
	segDur := max(tlTarget, policy.SegmentDuration())
	minBufSec := max(2, int(math.Ceil(segDur.Seconds())))
	windowSec := max(minBufSec*3, int(math.Ceil(policy.PlaylistWindow().Seconds())))
	if lease.tl.dvrWindow > 0 {
		first := periods[0].segs[0]
		last := periods[len(periods)-1].segs
		windowSec = max(windowSec, int(math.Ceil((last[len(last)-1].start + last[len(last)-1].dur - first.start).Seconds())))
	}
	delaySec := max(minBufSec, int(math.Ceil(policy.TargetLatency().Seconds())))

	videoBW, audioBW := ownedDASHBandwidths(ss)
	var b strings.Builder
	fmt.Fprintf(&b, `<?xml version="1.0" encoding="utf-8"?><MPD xmlns="urn:mpeg:dash:schema:mpd:2011" type="dynamic" profiles="urn:mpeg:dash:profile:isoff-live:2011" availabilityStartTime="%s" publishTime="%s" minimumUpdatePeriod="PT2S" minBufferTime="PT%dS" timeShiftBufferDepth="PT%dS" suggestedPresentationDelay="PT%dS">`,
		origin.UTC().Format(time.RFC3339Nano), now.UTC().Format(time.RFC3339Nano), minBufSec, windowSec, delaySec)
	for _, period := range periods {
		startAttr := "PT0S"
		if period.start > 0 {
			startAttr = fmt.Sprintf("PT%.3fS", period.start.Seconds())
		}
		fmt.Fprintf(&b, `<Period id="%d" start="%s">`, period.id, startAttr)
		for rep, tr := range period.tracks {
			bw := videoBW
			if tr.Kind == "audio" {
				bw = audioBW
			}
			scale := tr.Timescale
			if scale == 0 {
				scale = 1000
			}
			pto := int64(math.Round(period.originSec * float64(scale)))
			fmt.Fprintf(&b, `<AdaptationSet contentType="%s" mimeType="%s/mp4" segmentAlignment="true"><Representation id="%d" codecs="%s" bandwidth="%d"`,
				tr.Kind, tr.Kind, rep, html.EscapeString(tr.Codec), bw)
			if tr.Kind == "audio" {
				rate := tr.SampleRate
				if rate <= 0 {
					rate = 48000
				}
				ch := tr.Channels
				if ch <= 0 {
					ch = 2
				}
				fmt.Fprintf(&b, ` audioSamplingRate="%d"><AudioChannelConfiguration schemeIdUri="urn:mpeg:dash:23003:3:audio_channel_configuration:2011" value="%d"/>`, rate, ch)
			} else {
				fmt.Fprintf(&b, ` width="%d" height="%d">`, tr.Width, tr.Height)
			}
			initURL := withAuth(fmt.Sprintf("/livetv/sessions/%s/0/dash/%d/header?map=%s", ss.info.ID, rep, url.QueryEscape(period.mapRes)), ss.streamGrant)
			mediaURL := withAuth(fmt.Sprintf("/livetv/sessions/%s/0/dash/%d/$Number$.m4s", ss.info.ID, rep), ss.streamGrant)
			fmt.Fprintf(&b, `<SegmentTemplate timescale="%d" presentationTimeOffset="%d" startNumber="%d" initialization="%s" media="%s"><SegmentTimeline>`,
				scale, pto, period.segs[0].seq, html.EscapeString(initURL), html.EscapeString(mediaURL))
			for _, seg := range period.segs {
				start, dur, has := seg.trackTime(tr)
				if !has {
					start = pto + int64(math.Round((seg.start-period.start).Seconds()*float64(scale)))
					dur = max(int64(1), int64(math.Round(seg.dur.Seconds()*float64(scale))))
				}
				if start < pto {
					start = pto
				}
				fmt.Fprintf(&b, `<S t="%d" d="%d"/>`, start, dur)
			}
			b.WriteString(`</SegmentTimeline></SegmentTemplate></Representation></AdaptationSet>`)
		}
		b.WriteString(`</Period>`)
	}
	b.WriteString(`</MPD>`)
	w.Header().Set("Content-Type", "application/dash+xml")
	w.Header().Set("Cache-Control", "no-store")
	if r.Method != http.MethodHead {
		fmt.Fprint(w, b.String())
	}
	return true
}

func ownedDASHBandwidths(ss *session) (videoBW, audioBW int) {
	videoBW, audioBW = 4_000_000, 128_000
	ss.mu.Lock()
	info := ss.relayInfo
	if info == nil {
		info = ss.info.MediaInfo
	}
	ss.mu.Unlock()
	if info == nil {
		return videoBW, audioBW
	}
	if v := info.Video(); v != nil && v.BitrateKbps > 0 {
		videoBW = v.BitrateKbps * 1000
	} else if info.BitrateKbps > 0 {
		videoBW = info.BitrateKbps * 1000
	}
	for _, st := range info.Streams {
		if st.Type == "audio" && st.BitrateKbps > 0 {
			audioBW = st.BitrateKbps * 1000
			break
		}
	}
	return videoBW, audioBW
}

func (s *Service) serveOwnedDASHPart(w http.ResponseWriter, r *http.Request, ss *session, lease *ownedLease, rep int, seq int64, init bool) {
	if init {
		data, tracks, ok := lease.tl.dashInit(r.URL.Query().Get("map"))
		if !ok || rep < 0 || rep >= len(tracks) {
			writeStreamError(w, core.ErrNotFound)
			return
		}
		isolated, err := transcode.IsolateDASHTrack(data, tracks[rep].ID)
		if err != nil {
			http.Error(w, "invalid DASH init", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", tracks[rep].Kind+"/mp4")
		w.Header().Set("Cache-Control", "no-store")
		http.ServeContent(w, r, "init.mp4", time.Time{}, bytes.NewReader(isolated))
		return
	}
	data, tracks, ok := lease.tl.dashSegment(seq)
	if !ok || rep < 0 || rep >= len(tracks) {
		writeStreamError(w, core.ErrNotFound)
		return
	}
	isolated, err := transcode.IsolateDASHTrack(data, tracks[rep].ID)
	if err != nil {
		http.Error(w, "invalid DASH segment", http.StatusInternalServerError)
		return
	}
	if r.Method != http.MethodHead {
		s.touchMedia(ss)
	}
	observed := s.observeMedia(w, ss)
	defer observed.done()
	observed.Header().Set("Content-Type", tracks[rep].Kind+"/mp4")
	observed.Header().Set("Cache-Control", "no-store")
	http.ServeContent(observed, r, fmt.Sprintf("%d.m4s", seq), time.Time{}, bytes.NewReader(isolated))
}

// ensureDASH returns the session's DASH transcoder, starting it on first
// use. A session closed since it was looked up gets none: close has already
// run and would never stop a transcoder started now.
func (s *Service) ensureDASH(ctx context.Context, ss *session, id string) (core.DASHSession, error) {
	ss.prepareMu.Lock()
	defer ss.prepareMu.Unlock()
	ctx, cancel := ss.bindContext(ctx)
	defer cancel()
	ss.mu.Lock()
	if ss.closed {
		ss.mu.Unlock()
		return nil, core.ErrNotFound
	}
	ss.consumed = true
	if ss.dash != nil {
		ds := ss.dash
		ss.mu.Unlock()
		return ds, nil
	}
	if ss.info.Channel.Kind != "iptv" || s.transcoder() == nil {
		ss.mu.Unlock()
		return nil, core.ErrNotAvailable
	}
	ss.mu.Unlock()
	created, err := s.prepareLiveOutputInput(ctx, ss)
	if err != nil {
		return nil, err
	}
	var replacedOwned *ownedLease
	started := false
	ss.mu.Lock()
	defer func() {
		ss.mu.Unlock()
		if created && !started {
			s.revokeInput(ss)
		}
		replacedOwned.close()
	}()
	if err := ss.live(ctx); err != nil {
		return nil, err
	}
	info := ss.info.MediaInfo
	if info == nil {
		var err error
		if info, err = s.probeSessionInput(ctx, ss); err != nil {
			return nil, err
		}
		if info == nil {
			return nil, core.ErrNotAvailable
		}
	}
	copy := *info
	info = &copy
	info.Duration = 0
	ss.info.MediaInfo = info
	dec := s.transcoder().Decide(*info, ss.caps, core.DecideOptions{DirectStream: !ss.info.Channel.Transcode, AudioStream: -1, SubtitleStream: -1})
	if dec.Kind == core.DecisionDirectPlay {
		dec.Kind = core.DecisionRemux
		dec.VideoCopy = true
		dec.AudioCopy = true
	}
	input, err := s.relayInput(ss)
	if err != nil {
		return nil, err
	}
	hs, err := s.transcoder().StartHLS(s.serviceContext(), core.HLSRequest{SessionID: "livetv-dash-" + id, InputURL: input, LiveInput: true, Info: *info, Decision: dec, Protocol: core.ProtocolDASH, SegmentDuration: s.latencyPolicy().SegmentDuration()})
	if err != nil {
		return nil, err
	}
	ds, ok := hs.(core.DASHSession)
	if !ok {
		hs.Close()
		return nil, core.ErrNotAvailable
	}
	s.recordOutputProviderLocked(ss)
	// Replace any superseded HLS relay/passthrough so two ffmpeg processes do
	// not compete on the same session.
	if ss.sharedInput.Load() == nil {
		replacedOwned = ss.owned
		ss.publishOwnedLocked(nil)
	}
	ss.dash = ds
	started = true
	return ds, nil
}

func (s *Service) ServeDASHPart(w http.ResponseWriter, r *http.Request, id string, u *core.User, rep, n int, init bool) {
	ss, err := s.getSession(r.Context(), id, u)
	if err != nil {
		writeStreamError(w, err)
		return
	}
	ss.mu.Lock()
	shared := ss.broadcast != nil
	ds := ss.dash
	owned := ss.owned
	archive := ss.catchUp
	ss.mu.Unlock()
	if archive != nil {
		generation := r.URL.Query().Get("archive")
		dash, ok := archive.hls.(core.DASHSession)
		if !ok || !archive.info.DASH || generation != archive.info.MediaGeneration {
			writeStreamError(w, core.ErrNotFound)
			return
		}
		if init {
			dash.ServeInit(w, r, rep)
		} else {
			if r.Method != http.MethodHead {
				s.touchMedia(ss)
			}
			observed := s.observeMedia(w, ss)
			defer observed.done()
			dash.ServeMedia(observed, r, rep, n)
		}
		return
	}
	if shared {
		s.serveBroadcastDASHPart(w, r, ss, rep, int64(n), init)
		return
	}
	if ds == nil && owned != nil {
		s.serveOwnedDASHPart(w, r, ss, owned, rep, int64(n), init)
		return
	}
	if ds == nil {
		writeStreamError(w, core.ErrNotFound)
		return
	}
	if !init && r.Method != http.MethodHead {
		s.touchMedia(ss)
	}
	if init {
		ds.ServeInit(w, r, rep)
	} else {
		observed := s.observeMedia(w, ss)
		defer observed.done()
		ds.ServeMedia(observed, r, rep, n)
	}
}

func (s *Service) serveCatchUpDASH(w http.ResponseWriter, r *http.Request, ss *session, archive *catchUpSession) error {
	dash, ok := archive.hls.(core.DASHSession)
	if !ok || !archive.info.DASH {
		return core.ErrNotAvailable
	}
	rec := httptest.NewRecorder()
	dash.ServeManifest(rec, r)
	if rec.Code != http.StatusOK {
		http.Error(w, "Catch-up stream unavailable", rec.Code)
		return nil
	}
	prefix := "session/" + archive.hls.ID() + "/"
	route := "/livetv/sessions/" + ss.info.ID + "/0/dash/"
	body := dashURL.ReplaceAllStringFunc(rec.Body.String(), func(attr string) string {
		parts := dashURL.FindStringSubmatch(attr)
		child, ok := strings.CutPrefix(parts[2], prefix)
		if !ok {
			return attr
		}
		target := route + child + "?archive=" + url.QueryEscape(archive.info.MediaGeneration)
		return parts[1] + `="` + html.EscapeString(withAuth(target, ss.streamGrant)) + `"`
	})
	w.Header().Set("Content-Type", "application/dash+xml")
	w.Header().Set("Cache-Control", "no-store")
	if r.Method != http.MethodHead {
		_, _ = w.Write([]byte(body))
	}
	return nil
}
