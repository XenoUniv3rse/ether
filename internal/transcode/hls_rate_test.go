package transcode

import (
	"fmt"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
)

func TestHLSRateFitsAdvertisedPeak(t *testing.T) {
	for _, audio := range []int{0, 192, 1500} {
		for _, nominal := range []int{800, 8000} {
			for _, fps := range []float64{10, 60} {
				info := ladderInfo(1080, nominal)
				info.Streams[0].FrameRate = fps
				info.Streams[1].BitrateKbps = audio
				d := core.Decision{VideoCodec: "h264", BitrateKbps: nominal + audio, AudioCopy: true, AudioStream: 1}
				if audio == 0 {
					d.AudioStream = -1
				}
				peak := peakKbps(nominal + audio)
				for _, old := range []rateControl{{}, {Quality: true, MaxrateKbps: peak, BufsizeKbps: peak * 8}, {Quality: true, MaxrateKbps: 300, BufsizeKbps: 2400}} {
					rate := boundedHLSRate(info, d, old, peak, 2*time.Second)
					if got := encodedHLSPeakKbps(info, d, rate, 2*time.Second); got > peak {
						t.Fatalf("audio=%d nominal=%d fps=%v rate=%+v computed=%d advertised=%d", audio, nominal, fps, rate, got, peak)
					}
					if rate.TargetKbps > nominal || (old.MaxrateKbps > 0 && rate.MaxrateKbps > old.MaxrateKbps) {
						t.Fatalf("raised existing budget: %+v", rate)
					}
					if float64(rate.BufsizeKbps) < float64(rate.MaxrateKbps)/fps {
						t.Fatalf("less than one frame: %+v fps=%v", rate, fps)
					}
				}
			}
		}
	}
}

func TestHLSRateAppliedToRampSteadyAndRung(t *testing.T) {
	for _, vbv := range []bool{false, true} {
		t.Run(fmt.Sprint(vbv), func(t *testing.T) {
			fr := newFakeRunner()
			m := newTestManager(t, fr, config.Transcode{FastStart: true, ABRLadder: true, VBVReservoir: vbv})
			req := remuxRequest(t, "rate-contract", time.Minute)
			req.Info = ladderInfo(2160, 40000)
			req.Decision = core.Decision{Kind: core.DecisionTranscode, Container: ContainerMPEGTS, VideoCodec: "h264", BitrateKbps: 8000, MaxHeight: 1080, AudioCopy: true, AudioStream: 1}
			s := startSession(t, m, req)
			s.mu.Lock()
			defer s.mu.Unlock()
			if len(s.rungs) < 2 {
				t.Fatal("missing ladder")
			}
			for _, step := range []struct{ track, seg int }{{0, 0}, {0, 2}, {1, 2}} {
				s.wantTrack = step.track
				if err := s.startRunLocked(step.seg, false); err != nil {
					t.Fatal(err)
				}
				spec := s.cur.spec
				if got := encodedHLSPeakKbps(s.info, spec.Decision, spec.Rate, s.hlsPeakWindowLocked()); got > s.rungPeakKbpsLocked(step.track) {
					t.Fatalf("step=%+v spec=%+v peak=%d", step, spec.Rate, got)
				}
				if argValue(fr.lastStart(), "-bufsize") != fmt.Sprintf("%dk", spec.Rate.BufsizeKbps) {
					t.Fatal("rate not passed to encoder")
				}
			}
		})
	}
}

func TestSourceOutputBudgetSurvivesRunRestart(t *testing.T) {
	fr := newFakeRunner()
	m := newTestManager(t, fr, config.Transcode{})
	req := remuxRequest(t, "source-budget", time.Minute)
	req.Info = ladderInfo(1080, 12000)
	req.Decision = core.Decision{
		Kind: core.DecisionRemux, Container: ContainerFMP4,
		VideoCopy: true, AudioCopy: true, AudioStream: 1,
	}
	s := startSession(t, m, req)
	const budget = 2500
	d, err := s.ReduceOutputBitrate(t.Context(), req.InputURL, budget)
	if err != nil || d.Kind != core.DecisionTranscode || d.VideoCopy {
		t.Fatalf("output reduction: decision=%+v err=%v", d, err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	check := func() {
		t.Helper()
		spec := s.cur.spec
		if spec.Input != req.InputURL || spec.Decision.VideoCopy || spec.Target == nil || spec.Target.width != req.Info.Video().Width {
			t.Fatalf("lost input or picture format: decision=%+v contract=%v", spec.Decision, spec.Target)
		}
		if peak := encodedHLSPeakKbps(s.info, spec.Decision, spec.Rate, s.hlsPeakWindowLocked()); peak > budget {
			t.Fatalf("encoded peak = %d kb/s, budget = %d kb/s", peak, budget)
		}
	}
	check()
	if err := s.startRunForLocked(4, false, "seek"); err != nil {
		t.Fatal(err)
	}
	check()
}
