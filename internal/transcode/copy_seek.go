package transcode

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"time"
)

// A video copy restart must begin on the keyframe before its seek point:
// that is where the run's first segment (and a progressive stream's first
// picture) decodes from. Containers with a keyframe index (Matroska cues,
// MP4 sample tables) seek there by themselves. MPEG transport and program
// streams carry none: their timestamp seek lands on whichever packet holds
// the seek time, usually inside a GOP, and stream copy then discards
// pictures up to the next keyframe. Mid-title that starts the picture up to
// a GOP late; inside a title's last GOP there is no next keyframe and the
// copy has no video at all.
//
// Without an Ether keyframe index, copyKeyframeBefore reads the packet
// headers of a short window before the seek point and returns the last
// keyframe in it, so the run's input seeks there instead. Audio keeps its
// own cut at the seek point (buildArgs).
var copyKeyframeWindows = []time.Duration{6 * time.Second, 20 * time.Second}

const copyKeyframeProbeTimeout = 3 * time.Second

// needsCopyKeyframe reports whether s is a video copy restart whose input
// cannot seek to a keyframe by itself.
func needsCopyKeyframe(s runSpec) bool {
	if s.Start <= 0 || s.HasSeekFrom || s.CopyOrigin || !s.Decision.VideoCopy || s.LiveInput || s.LoopInput || s.RTXTimeline || len(s.Info.Keyframes) > 0 || s.Info.Video() == nil || !s.titleClock() {
		return false
	}
	if _, _, ok := pceSeekInput(s); ok {
		return false
	}
	switch s.Info.Container {
	case "mpegts", "mpeg":
		return true
	}
	return false
}

// copyKeyframeBefore is the title time of the last video keyframe at or
// before s.Start, when one can be found quickly.
func (m *Manager) copyKeyframeBefore(ctx context.Context, s runSpec) (time.Duration, bool) {
	if !needsCopyKeyframe(s) || m.ffprobe == "" {
		return 0, false
	}
	ctx, cancel := context.WithTimeout(ctx, copyKeyframeProbeTimeout)
	defer cancel()
	v := s.Info.Video()
	for _, window := range copyKeyframeWindows {
		from := max(0, s.Start-window)
		args := []string{"-hide_banner", "-v", "error"}
		args = append(args, inputSafetyArgs(s.Input, false)...)
		args = append(args, httpInputArgs(s.Input, false)...)
		args = appendInputHeaders(args, s.commandHeaders())
		// A leading + is relative to the input's start time: the clock
		// -start_at_zero gives the run.
		args = append(args, "-select_streams", strconv.Itoa(v.Index),
			"-read_intervals", "+"+secs(from)+"%+"+secs(s.Start-from+time.Millisecond),
			"-show_entries", "packet=pts_time,flags:format=start_time", "-of", "csv", "-i", s.Input)
		var out cappedProbeBuffer
		if m.runner.Run(ctx, Command{Path: m.ffprobe, Args: args, Stdout: &out}) != nil {
			return 0, false
		}
		if at, ok := lastKeyframeAtOrBefore(out.buf.Bytes(), s.Start); ok {
			return at, true
		}
		if from == 0 {
			break
		}
	}
	return 0, false
}

// lastKeyframeAtOrBefore reads ffprobe's CSV packet listing (absolute
// times: "packet,<pts>,<flags>" lines and one "format,<start>") into title
// time.
func lastKeyframeAtOrBefore(listing []byte, at time.Duration) (time.Duration, bool) {
	best, found := copyKeyframePresentation(listing, at)
	return max(0, best.Truncate(time.Millisecond)-time.Millisecond), found
}

func copyKeyframeTimes(listing []byte) []time.Duration {
	var keys []float64
	origin, haveOrigin := 0.0, false
	for line := range strings.Lines(string(listing)) {
		f := strings.Split(strings.TrimSpace(line), ",")
		switch {
		case len(f) >= 3 && f[0] == "packet" && copyProbeKey(f):
			if pts, err := strconv.ParseFloat(f[1], 64); err == nil {
				keys = append(keys, pts)
			}
		case len(f) >= 2 && f[0] == "format":
			if start, err := strconv.ParseFloat(f[1], 64); err == nil {
				origin, haveOrigin = start, true
			}
		}
	}
	if !haveOrigin {
		return nil
	}
	times := make([]time.Duration, 0, len(keys))
	for _, pts := range keys {
		times = append(times, time.Duration((pts-origin)*float64(time.Second)))
	}
	return times
}

func copyKeyframePresentation(listing []byte, at time.Duration) (time.Duration, bool) {
	best, found := time.Duration(0), false
	for _, t := range copyKeyframeTimes(listing) {
		if t >= 0 && ((at == 0 && (!found || t < best)) || (t <= at && (!found || t > best))) {
			best, found = t, true
		}
	}
	return best, found
}

// stopContext is a context canceled when stop closes (never, for nil).
func stopContext(stop <-chan struct{}) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		select {
		case <-stop:
			cancel()
		case <-ctx.Done():
		}
	}()
	return ctx, cancel
}

// continuousCopyOrigin measures presentation time, not decode time or the
// requested seek. Bounded packet headers are enough; no frame decoding or
// whole-title index is needed. A failed proof must not publish a false origin.
func (m *Manager) continuousCopyOrigin(ctx context.Context, s runSpec) (copyOpening, error) {
	ctx, cancel := context.WithTimeout(ctx, copyKeyframeProbeTimeout)
	defer cancel()
	var sample copyOpening
	haveMeasured := false
	entries := "packet=pts_time,flags:format=start_time"
	if s.AdaptiveHLS {
		entries = "packet=pts_time,dts_time,flags:format=start_time"
	}
	for _, window := range copyKeyframeWindows {
		from := max(0, s.Start-window)
		args := []string{"-hide_banner", "-v", "error"}
		args = append(args, inputSafetyArgs(s.Input, false)...)
		args = append(args, httpInputArgs(s.Input, false)...)
		args = appendInputHeaders(args, s.commandHeaders())
		// A demuxer may seek earlier than from; allow one extra window so a
		// later keyframe before the target is not accidentally omitted.
		args = append(args, "-select_streams", strconv.Itoa(s.Info.Video().Index),
			"-read_intervals", "+"+secs(from)+"%+"+secs(s.Start-from+window),
			"-show_entries", entries, "-of", "csv", "-i", s.Input)
		var out cappedProbeBuffer
		if err := m.runner.Run(ctx, Command{Path: m.ffprobe, Args: args, Stdout: &out}); err != nil {
			if haveMeasured {
				return sample, nil
			}
			return copyOpening{}, errors.New("transcode: cannot prove copy presentation origin")
		}
		if origin, ok := copyKeyframePresentation(out.buf.Bytes(), s.Start); ok {
			next := time.Duration(0)
			for _, k := range copyKeyframeTimes(out.buf.Bytes()) {
				if k > origin && (next == 0 || k < next) {
					next = k
				}
			}
			if s.AdaptiveHLS {
				sample = inspectCopyOpening(out.buf.Bytes())
			}
			sample.origin, sample.next, haveMeasured = origin, next, true
			if next > 0 {
				return sample, nil
			}
		}
	}
	if haveMeasured {
		if s.Info.Duration > sample.origin && s.Info.Duration-sample.origin <= 20*time.Second {
			sample.next = s.Info.Duration
		}
		return sample, nil
	}
	return copyOpening{}, errors.New("transcode: no keyframe in bounded copy seek window")
}

type copyOpening struct {
	origin, next time.Duration
	closed       bool
	decodeLead   time.Duration
}

func copyProbeKey(fields []string) bool {
	return len(fields) >= 3 && (strings.HasPrefix(fields[2], "K") ||
		(len(fields) >= 4 && strings.HasPrefix(fields[3], "K")))
}

// inspectCopyOpening checks sampled decode order for leading pictures and
// measures the reorder delay at a random access point.
func inspectCopyOpening(listing []byte) copyOpening {
	var key, lead float64
	keys := 0
	for line := range strings.Lines(string(listing)) {
		f := strings.Split(strings.TrimSpace(line), ",")
		if len(f) < 3 || f[0] != "packet" {
			continue
		}
		pts, err := strconv.ParseFloat(f[1], 64)
		if err != nil {
			return copyOpening{}
		}
		if copyProbeKey(f) {
			key = pts
			keys++
			if len(f) >= 4 {
				if dts, err := strconv.ParseFloat(f[2], 64); err == nil && pts-dts <= 1 {
					lead = max(lead, pts-dts)
				}
			}
		} else if keys > 0 && pts < key {
			return copyOpening{}
		}
	}
	return copyOpening{closed: keys >= 2, decodeLead: time.Duration(lead * float64(time.Second))}
}

// Millisecond command precision must not round the keyframe seek backwards
// into the previous GOP. Keep the plan itself at the measured exact PTS.
func copyOriginSeek(at time.Duration) time.Duration {
	return ((at+time.Millisecond-1)/time.Millisecond)*time.Millisecond + time.Millisecond
}
