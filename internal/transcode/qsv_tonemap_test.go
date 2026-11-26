package transcode

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/store"
)

type staticQSVToneMapper struct {
	m qsvToneMapMethod
}

func (s staticQSVToneMapper) method(core.HWAccel, features, bool) qsvToneMapMethod { return s.m }
func (s staticQSVToneMapper) status(core.HWAccel, features) (bool, bool) {
	return s.m != qsvToneMapNone, true
}

func qsvHDRTestFeatures(m qsvToneMapper) features {
	f := testFeatures()
	f.hwaccels = map[string]bool{"qsv": true, "vaapi": true, "opencl": true}
	for _, name := range []string{"vpp_qsv", "hwmap", "tonemap_opencl", "zscale", "tonemap", "tonemapx", "scale", "format"} {
		f.filters[name] = true
	}
	for _, enc := range []string{"h264_qsv", "hevc_qsv", "libx264", "libx265"} {
		f.encoders[enc] = true
	}
	f.vppQSVTonemap = true
	f.qsvToneMap = m
	return f
}

func TestQSVHardwareToneMapArgsVPPAndOpenCL(t *testing.T) {
	info := mustParse(t, "mkv_hevc_hdr10.json")
	opts := allow()
	opts.MaxHeight = 720
	dec := Decide(info, plexWeb, opts, Policy{})
	if dec.VideoCopy || !dec.ToneMap || dec.VideoCodec != "h264" || dec.MaxHeight != 720 {
		t.Fatalf("unexpected base decision: %+v", dec)
	}
	hw := core.HWAccel{
		Name:            AccelQSV,
		Device:          "/dev/dri/renderD128",
		Encoders:        []string{"h264_qsv", "hevc_qsv"},
		QSVQualityModes: map[string][]string{"h264_qsv": {qsvLA}},
		QSVBitrateModes: map[string][]string{"h264_qsv": {qsvLA}},
	}

	t.Run("vpp_qsv_tonemap", func(t *testing.T) {
		s := baseSpec(info, dec)
		s.Accel = hw
		s.Features = qsvHDRTestFeatures(staticQSVToneMapper{m: qsvToneMapVPP})
		args := buildArgs(s)
		t.Logf("QSV (a) vpp_qsv args: ffmpeg %s", strings.Join(args, " "))
		mustContain(t, args,
			"-qsv_device /dev/dri/renderD128",
			"-hwaccel qsv -hwaccel_output_format qsv",
			"vpp_qsv=w=1280:h=720:format=nv12:tonemap=1",
			"-c:v h264_qsv",
		)
		mustNotContain(t, args, "-init_hw_device", "tonemap_opencl=", "tonemapx=", "zscale=", "hwdownload")
	})

	t.Run("vaapi_opencl_qsv_interop", func(t *testing.T) {
		s := baseSpec(info, dec)
		s.Accel = hw
		s.Features = qsvHDRTestFeatures(staticQSVToneMapper{m: qsvToneMapOpenCL})
		args := buildArgs(s)
		t.Logf("QSV (b) tonemap_opencl interop args: ffmpeg %s", strings.Join(args, " "))
		mustContain(t, args,
			"-init_hw_device vaapi=va:/dev/dri/renderD128",
			"-init_hw_device qsv=qs@va",
			"-init_hw_device opencl=ocl@va",
			"-filter_hw_device ocl",
			"-hwaccel qsv -hwaccel_output_format qsv",
			"vpp_qsv=w=1280:h=720:format=p010le,hwmap=derive_device=opencl,tonemap_opencl=tonemap=hable:desat=0:format=nv12:t=bt709:m=bt709:p=bt709,hwmap=derive_device=qsv:reverse=1:extra_hw_frames=16",
			"-c:v h264_qsv",
		)
		mustNotContain(t, args, "-qsv_device", "tonemapx=", "zscale=", "hwdownload")
	})
}

func TestQSVHardwareToneMapDolbyVisionProfile5KeepsCPURPU(t *testing.T) {
	info := mustParse(t, "mkv_hevc_hdr10.json")
	v := info.Video()
	v.DolbyVision = true
	v.DolbyVisionProfile = 5
	opts := allow()
	opts.MaxHeight = 720
	dec := Decide(info, plexWeb, opts, Policy{})
	dec.Kind, dec.VideoCopy, dec.VideoCodec, dec.MaxHeight, dec.ToneMap = core.DecisionTranscode, false, "h264", 720, true

	s := baseSpec(info, dec)
	s.Accel = core.HWAccel{
		Name:     AccelQSV,
		Device:   "/dev/dri/renderD128",
		Encoders: []string{"h264_qsv", "hevc_qsv"},
	}
	s.Features = qsvHDRTestFeatures(staticQSVToneMapper{m: qsvToneMapVPP})

	args := buildArgs(s)
	mustContain(t, args, "-hwaccel qsv", "tonemapx=", "apply_dovi=1", "scale=1280:720", "-c:v h264_qsv")
	mustNotContain(t, args, "-hwaccel_output_format", "vpp_qsv=", "tonemap_opencl=")
}

func TestQSVToneMapLazyValidationFallbackAndPersistence(t *testing.T) {
	const sdrProbeJSON = `{"streams":[{"codec_name":"h264","pix_fmt":"yuv420p","bits_per_raw_sample":"8","color_space":"bt709","color_transfer":"bt709","color_primaries":"bt709"}]}`
	const untouchedPQJSON = `{"streams":[{"codec_name":"h264","pix_fmt":"yuv420p","bits_per_raw_sample":"8","color_space":"bt2020nc","color_transfer":"smpte2084","color_primaries":"bt2020"}]}`

	t.Run("no_eager_probe_on_new_and_persists_vpp", func(t *testing.T) {
		db := store.OpenTest(t)
		binDir := t.TempDir()
		ffBin := filepath.Join(binDir, "ffmpeg")
		fpBin := filepath.Join(binDir, "ffprobe")
		if err := os.WriteFile(ffBin, []byte("ffmpeg-qsv-test-build-1"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(fpBin, []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		probeRuns := 0
		fr := newFakeRunner()
		fr.hwaccels = "Hardware acceleration methods:\nqsv\nvaapi\nopencl\n"
		fr.filters += " ... vpp_qsv           V->V       Quick Sync Video VPP.\n ... hwmap             V->V       Map hardware frames.\n ... tonemap_opencl    V->V       OpenCL tone map.\n"
		fr.encoders += " V..... h264_qsv             H.264 QSV\n V..... hevc_qsv             HEVC QSV\n"
		fr.runFunc = func(_ context.Context, cmd Command) error {
			joinedArgs := strings.Join(cmd.Args, " ")
			switch {
			case strings.Contains(joinedArgs, "sample-hdr10.mkv") && hasArg(cmd.Args, "lavfi"):
				probeRuns++
				return nil
			case strings.Contains(joinedArgs, "vpp_qsv=w=160:h=90:format=nv12:tonemap=1"):
				probeRuns++
				return nil
			case strings.HasSuffix(cmd.Path, "ffprobe") && strings.Contains(joinedArgs, "out-vpp.mkv"):
				probeRuns++
				_, _ = cmd.Stdout.Write([]byte(sdrProbeJSON))
				return nil
			}
			return errUnhandled
		}

		previousDRM := sysfsDRM
		sysfsDRM = t.TempDir()
		t.Cleanup(func() { sysfsDRM = previousDRM })
		intelDevice := filepath.Join(sysfsDRM, "renderD128", "device")
		if err := os.MkdirAll(intelDevice, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(intelDevice, "vendor"), []byte("0x8086"), 0o644); err != nil {
			t.Fatal(err)
		}
		m, err := New(config.Transcode{FFmpegPath: ffBin, FFprobePath: fpBin, HWAccel: AccelQSV, VAAPIDevice: DefaultVAAPIDevice, TempDir: t.TempDir()}, testLogger(t), WithRunner(fr), withFinder(finder{goos: "linux", isExec: isExecutable}), WithDecodeStore(db))
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		defer func() { _ = m.Close() }()

		if probeRuns != 0 {
			t.Fatalf("expected zero QSV tone-map validation runs during New, got %d", probeRuns)
		}

		cap := m.Capability()
		if cap.HardwareToneMap == nil || !*cap.HardwareToneMap {
			t.Fatalf("expected HardwareToneMap=true after lazy VPP validation, got %+v", cap)
		}
		if !slices.Contains(cap.ToneMap, "qsv") {
			t.Fatalf("expected qsv in ToneMap paths, got %v", cap.ToneMap)
		}
		if probeRuns == 0 {
			t.Fatal("expected lazy validation probe to run on Capability()")
		}

		// A restarted cache on the same FFmpeg binary and SQLite DB must reuse
		// the persisted capability without spawning ffmpeg/ffprobe again.
		failRunner := &fakeRunner{runFunc: func(context.Context, Command) error {
			t.Error("unexpected subprocess after SQLite cache hit")
			return errors.New("unexpected run")
		}}
		reloaded := newQSVToneMapCache(failRunner, ffBin, fpBin, t.TempDir(), m.feat.encoders)
		reloaded.db = db
		defer reloaded.close()
		if got := reloaded.method(m.hw, m.feat, true); got != qsvToneMapVPP {
			t.Fatalf("reloaded method = %q, want %q", got, qsvToneMapVPP)
		}
	})

	t.Run("vpp_untouched_pq_falls_back_to_opencl", func(t *testing.T) {
		fr := &fakeRunner{runFunc: func(_ context.Context, cmd Command) error {
			joinedArgs := strings.Join(cmd.Args, " ")
			switch {
			case strings.Contains(joinedArgs, "sample-hdr10.mkv") && hasArg(cmd.Args, "lavfi"):
				return nil
			case strings.Contains(joinedArgs, "vpp_qsv=w=160:h=90:format=nv12:tonemap=1"):
				return nil
			case strings.HasSuffix(cmd.Path, "ffprobe") && strings.Contains(joinedArgs, "out-vpp.mkv"):
				_, _ = cmd.Stdout.Write([]byte(untouchedPQJSON))
				return nil
			case strings.Contains(joinedArgs, "tonemap_opencl=tonemap=hable"):
				return nil
			case strings.HasSuffix(cmd.Path, "ffprobe") && strings.Contains(joinedArgs, "out-opencl.mkv"):
				_, _ = cmd.Stdout.Write([]byte(sdrProbeJSON))
				return nil
			}
			return errors.New("unexpected command: " + joinedArgs)
		}}
		f := qsvHDRTestFeatures(nil)
		cache := newQSVToneMapCache(fr, "ffmpeg", "ffprobe", t.TempDir(), f.encoders)
		defer cache.close()
		f.qsvToneMap = cache
		hw := core.HWAccel{Name: AccelQSV, Device: "/dev/dri/renderD128", Encoders: []string{"h264_qsv"}}

		if got := cache.method(hw, f, true); got != qsvToneMapOpenCL {
			t.Fatalf("method = %q, want %q when vpp_qsv leaves PQ untouched", got, qsvToneMapOpenCL)
		}
	})

	t.Run("failed_validation_keeps_software_and_logs_once", func(t *testing.T) {
		var logBuf bytes.Buffer
		logger := slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelInfo}))
		fr := &fakeRunner{runFunc: func(_ context.Context, cmd Command) error {
			joinedArgs := strings.Join(cmd.Args, " ")
			switch {
			case strings.Contains(joinedArgs, "sample-hdr10.mkv") && hasArg(cmd.Args, "lavfi"):
				return nil
			case strings.Contains(joinedArgs, "vpp_qsv=w=160:h=90:format=nv12:tonemap=1"):
				_, _ = cmd.Stderr.Write([]byte("MFX_ERR_UNSUPPORTED"))
				return errors.New("exit status 1")
			case strings.Contains(joinedArgs, "tonemap_opencl=tonemap=hable"):
				_, _ = cmd.Stderr.Write([]byte("Failed to map frame to OpenCL"))
				return errors.New("exit status 1")
			}
			return errors.New("unexpected command: " + joinedArgs)
		}}
		f := qsvHDRTestFeatures(nil)
		cache := newQSVToneMapCache(fr, "ffmpeg", "ffprobe", t.TempDir(), f.encoders)
		cache.log = logger
		defer cache.close()
		f.qsvToneMap = cache

		info := mustParse(t, "mkv_hevc_hdr10.json")
		opts := allow()
		opts.MaxHeight = 720
		dec := Decide(info, plexWeb, opts, Policy{})
		hw := core.HWAccel{Name: AccelQSV, Device: "/dev/dri/renderD128", Encoders: []string{"h264_qsv"}}

		s := baseSpec(info, dec)
		s.Accel = hw
		s.Features = f
		s.waitDecode = true

		args := buildArgs(s)
		mustContain(t, args, "-qsv_device /dev/dri/renderD128", "-hwaccel qsv", "tonemapx=", "scale=1280:720", "-c:v h264_qsv")
		mustNotContain(t, args, "-hwaccel_output_format", "vpp_qsv=", "tonemap_opencl=")

		// Repeated calls must not emit duplicate warning lines.
		_ = cache.method(hw, f, true)
		if count := strings.Count(logBuf.String(), "QSV hardware HDR tone-mapping unavailable"); count != 1 {
			t.Fatalf("expected exactly 1 warning log, got %d:\n%s", count, logBuf.String())
		}
		if !strings.Contains(logBuf.String(), "MFX_ERR_UNSUPPORTED") {
			t.Fatalf("expected log to include failure reason, got:\n%s", logBuf.String())
		}
		if qsvHardwareToneMapAvailable(f, hw) {
			t.Fatal("qsvHardwareToneMapAvailable reported true after failed validation")
		}
		if slices.Contains(toneMapPaths(f, hw), "qsv") {
			t.Fatalf("toneMapPaths unexpectedly included qsv: %v", toneMapPaths(f, hw))
		}
	})
}

func TestQSVHDRPreservingDownscaleAndHEVCProbe(t *testing.T) {
	// Startup QSV probe uses an 8-bit nv12 test frame (outRange == "") and must
	// tag SDR BT.709 rather than PQ BT.2020 so drivers do not reject hevc_qsv.
	probe := qsvProbeArgs("/dev/dri/renderD128", "hevc_qsv", qsvICQ)
	mustContain(t, probe, "-color_primaries bt709 -color_trc bt709 -colorspace bt709")
	mustNotContain(t, probe, "smpte2084", "bt2020")

	info := mustParse(t, "mkv_hevc_hdr10.json")
	v := info.Video()
	v.DolbyVision = true
	v.DolbyVisionProfile = 8
	v.DolbyVisionCompatibilityID = 1

	opts := allow()
	opts.DirectPlay = false
	opts.MaxHeight = 720
	caps := appleTV
	caps.HDR10 = true
	caps.VideoCodecs = []string{"h264", "hevc"}
	caps.TranscodeVideoCodecs = []string{"h264", "hevc"}

	dec := Decide(info, caps, opts, Policy{HEVCEncoder: true, HEVCHardware: true})
	if dec.VideoCopy || dec.ToneMap || dec.VideoCodec != "hevc" || dec.MaxHeight != 720 {
		t.Fatalf("expected 720p HDR-preserving HEVC transcode, got %+v", dec)
	}

	s := baseSpec(info, dec)
	s.Accel = core.HWAccel{
		Name:            AccelQSV,
		Device:          "/dev/dri/renderD128",
		Encoders:        []string{"h264_qsv", "hevc_qsv"},
		QSVQualityModes: map[string][]string{"hevc_qsv": {qsvICQ}},
		QSVBitrateModes: map[string][]string{"hevc_qsv": {qsvICQ}},
	}
	s.Features = qsvHDRTestFeatures(staticQSVToneMapper{m: qsvToneMapVPP})

	args := buildArgs(s)
	mustContain(t, args,
		"-qsv_device /dev/dri/renderD128",
		"-hwaccel qsv -hwaccel_output_format qsv",
		"vpp_qsv=w=1280:h=720:format=p010le",
		"-c:v hevc_qsv",
		"-profile:v main10",
	)
	mustNotContain(t, args, "tonemap", "zscale=", "format=nv12")
}
