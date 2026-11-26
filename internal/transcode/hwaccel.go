package transcode

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

// Hardware accelerator names, as used in config.Transcode.HWAccel and
// core.HWAccel.Name.
const (
	AccelNone         = "none"
	AccelVideoToolbox = "videotoolbox"
	AccelNVENC        = "nvenc"
	AccelQSV          = "qsv"
	AccelVAAPI        = "vaapi"
)

// DefaultVAAPIDevice is used by specs without an assigned VAAPI render node.
const DefaultVAAPIDevice = "/dev/dri/renderD128"

// features is what the ffmpeg build offers: encoders, hwaccels, filters.
type features struct {
	decode        decodeCapabilities
	qsvToneMap    qsvToneMapper
	vaapiToneMap  func(vaapiToneKey) vaapiToneMethod
	vaapiMeasured bool
	// A one-frame device probe confirms the installed CUDA filter accepts
	// P010 output. A filter name or version alone does not establish this.
	cudaP010ToneMap bool
	// scaleVTFormat is a build capability, not an FFmpeg version guess:
	// upstream builds can expose scale_vt without its format option.
	scaleVTFormat bool
	// vppQSVTonemap is true when -h filter=vpp_qsv advertises the tonemap option.
	vppQSVTonemap bool
	// Jellyfin patches the segment muxer's origin and first-entry duration.
	jellyfinSegmentClock bool
	// Optional startup-only inventory. Unknown lists deny distributed admission
	// without changing standalone encoder behavior.
	muxers, demuxers, decoders, bsfs, inputProtocols map[string]bool
	encoders                                         map[string]bool
	hwaccels                                         map[string]bool
	filters                                          map[string]bool
	// filtersProbed is true only after -filters parsed at least one filter.
	// A nil or empty map means those filters are unavailable, never that
	// every filter exists.
	filtersProbed bool
	probeDetail   string
}

func cudaP010ProbeArgs() []string {
	return []string{"-hide_banner", "-loglevel", "error", "-f", "lavfi", "-i", "color=size=64x64:rate=1:duration=1,format=p010le,setparams=color_primaries=bt2020:color_trc=smpte2084:colorspace=bt2020nc", "-vf", "hwupload_cuda,tonemap_cuda=tonemap=hable:format=p010le:t=bt709:m=bt709:p=bt709,hwdownload,format=p010le", "-frames:v", "1", "-f", "null", "-"}
}

func probeCUDAP010(ctx context.Context, d hwDetector, hw core.HWAccel, f features) bool {
	if hw.Name != AccelNVENC || !f.hwaccels["cuda"] || !f.hasFilter("tonemap_cuda") || !f.hasFilter("hwupload_cuda") || !f.hasFilter("scale_cuda") {
		return false
	}
	return d.testRun(ctx, cudaP010ProbeArgs()) == nil
}

func (f features) hasEncoder(n string) bool { return f.encoders[n] }
func (f features) hasFilter(n string) bool  { return f.filters[n] }

var (
	encoderLineRE = regexp.MustCompile(`^\s*([VASD][A-Z.]{5})\s+(\S+)`)
	filterLineRE  = regexp.MustCompile(`^\s*[A-Z.|]{2,3}\s+(\S+)\s+\S*->\S*`)
)

// parseEncoders parses `ffmpeg -hide_banner -encoders`.
func parseEncoders(out []byte) map[string]bool {
	m := map[string]bool{}
	sc := bufio.NewScanner(bytes.NewReader(out))
	started := false
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(strings.TrimSpace(line), "------") {
			started = true
			continue
		}
		if !started {
			continue
		}
		if mm := encoderLineRE.FindStringSubmatch(line); mm != nil {
			m[mm[2]] = true
		}
	}
	return m
}

// parseHWAccels parses `ffmpeg -hide_banner -hwaccels`.
func parseHWAccels(out []byte) map[string]bool {
	m := map[string]bool{}
	sc := bufio.NewScanner(bytes.NewReader(out))
	started := false
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if strings.HasPrefix(line, "Hardware acceleration methods") {
			started = true
			continue
		}
		if started && line != "" && !strings.Contains(line, " ") {
			m[line] = true
		}
	}
	return m
}

// parseFilters parses `ffmpeg -hide_banner -filters`.
func parseFilters(out []byte) map[string]bool {
	m := map[string]bool{}
	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		if mm := filterLineRE.FindStringSubmatch(sc.Text()); mm != nil {
			m[mm[1]] = true
		}
	}
	return m
}

func probeFeatures(ctx context.Context, r Runner, ffmpeg string) (features, error) {
	run := func(args ...string) ([]byte, error) {
		ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
		defer cancel()
		var out bytes.Buffer
		err := r.Run(ctx, Command{Path: ffmpeg, Args: append([]string{"-hide_banner"}, args...), Stdout: &out, Stderr: &out})
		if err != nil {
			return nil, fmt.Errorf("transcode: ffmpeg %v: %w", args, err)
		}
		return out.Bytes(), nil
	}
	f := features{
		encoders: map[string]bool{},
		hwaccels: map[string]bool{},
		filters:  map[string]bool{},
	}
	enc, err := probeList(run, "-encoders", parseEncoders)
	if err != nil {
		return f, fmt.Errorf("transcode: ffmpeg encoders: %w", err)
	}
	f.encoders = enc
	if hw, hwErr := probeList(run, "-hwaccels", parseHWAccels); hwErr != nil {
		f.probeDetail = hwErr.Error()
	} else {
		f.hwaccels = hw
	}
	filters, filtersErr := probeList(run, "-filters", parseFilters)
	if filtersErr != nil {
		// Degrade: an unreadable filter list is empty, not "everything".
		if f.probeDetail != "" {
			f.probeDetail += "; "
		}
		f.probeDetail += filtersErr.Error()
	} else {
		f.filters = filters
		f.filtersProbed = true
	}
	if f.hasFilter("scale_vt") {
		// Optional probing cannot make a formerly usable build fail startup.
		if help, helpErr := run("-h", "filter=scale_vt"); helpErr == nil {
			f.scaleVTFormat = filterHasOption(help, "format")
		}
	}
	if f.hasFilter("vpp_qsv") {
		if help, helpErr := run("-h", "filter=vpp_qsv"); helpErr == nil {
			f.vppQSVTonemap = filterHasOption(help, "tonemap")
		}
	}
	probeDistributedFeatures(ctx, r, ffmpeg, &f)
	return f, nil
}

// probeList runs one ffmpeg query up to three times. An empty parse is a
// failed read, not an empty feature set that callers may treat as unknown.
func probeList(run func(...string) ([]byte, error), flag string, parse func([]byte) map[string]bool) (map[string]bool, error) {
	var last error
	for attempt := 0; attempt < 3; attempt++ {
		b, err := run(flag)
		if err != nil {
			last = err
			continue
		}
		if m := parse(b); len(m) > 0 {
			return m, nil
		}
		last = fmt.Errorf("ffmpeg %s produced no entries", flag)
	}
	if last == nil {
		last = fmt.Errorf("ffmpeg %s produced no entries", flag)
	}
	return nil, last
}

func filterHasOption(help []byte, option string) bool {
	for _, line := range strings.Split(string(help), "\n") {
		fields := strings.Fields(line)
		if len(fields) > 1 && fields[0] == option && strings.HasPrefix(fields[1], "<") {
			return true
		}
	}
	return false
}

// accelCandidate is one hardware encoder family.
type accelCandidate struct {
	name     string
	h264     string
	hevc     string
	platform []string
}

var accelCandidates = []accelCandidate{
	{name: AccelVideoToolbox, h264: "h264_videotoolbox", hevc: "hevc_videotoolbox", platform: []string{"darwin"}},
	{name: AccelNVENC, h264: "h264_nvenc", hevc: "hevc_nvenc", platform: []string{"linux", "windows"}},
	{name: AccelQSV, h264: "h264_qsv", hevc: "hevc_qsv", platform: []string{"linux", "windows"}},
	{name: AccelVAAPI, h264: "h264_vaapi", hevc: "hevc_vaapi", platform: []string{"linux"}},
}

// testEncodeArgs is a 1-frame lavfi encode that proves an encoder works on
// this machine (drivers present, device usable).
func testEncodeArgs(accel, encoder string) []string {
	args := []string{"-hide_banner", "-nostdin", "-loglevel", "error"}
	vf := ""
	switch accel {
	case AccelQSV:
		vf = "format=nv12"
	case AccelNVENC, AccelVideoToolbox:
		vf = "format=yuv420p"
	}
	args = append(args, "-f", "lavfi", "-i", "testsrc=size=320x240:rate=1", "-frames:v", "1")
	if vf != "" {
		args = append(args, "-vf", vf)
	}
	return append(args, "-c:v", encoder, "-f", "null", "-")
}

// vaapiRCProbeArgs is a short lavfi encode that probes a specific VAAPI
// rate-control mode (QVBR, VBR, CBR, CQP). Some drivers (for example Debian's
// free iHD driver or LP-only EncSliceLP entrypoints) support CQP only and
// reject QVBR/VBR with "Driver does not support QVBR RC mode".
func vaapiRCProbeArgs(encoder, device, rcMode string, main10, scale bool) []string {
	pixel, profile := "nv12", "high"
	if main10 {
		pixel, profile = "p010le", "main10"
	} else if encoder == "hevc_vaapi" {
		profile = "main"
	}
	vf := "format=" + pixel + ",hwupload"
	if scale {
		vf += "," + vaapiScale(160, 120, pixel)
	}
	args := []string{"-hide_banner", "-nostdin", "-loglevel", "error", "-xerror", "-abort_on", "empty_output", "-vaapi_device", device,
		"-f", "lavfi", "-i", "testsrc2=size=320x240:rate=24", "-frames:v", "4",
		"-vf", vf, "-c:v", encoder, "-profile:v", profile}
	switch rcMode {
	case "QVBR":
		args = append(args, "-rc_mode", "QVBR", "-global_quality", "23", "-b:v", "1000k", "-maxrate", "1500k", "-bufsize", "2000k")
	case "VBR":
		args = append(args, "-rc_mode", "VBR", "-b:v", "1000k", "-maxrate", "1500k", "-bufsize", "2000k")
	case "CBR":
		args = append(args, "-rc_mode", "CBR", "-b:v", "1000k", "-maxrate", "1000k", "-bufsize", "2000k")
	case "CQP":
		args = append(args, "-rc_mode", "CQP", "-global_quality", "23")
	}
	return append(args, "-f", "null", "-")
}

// hwDetector confirms hardware encoders.
type hwDetector struct {
	runner     Runner
	ffmpeg     string
	goos       string
	log        *slog.Logger
	background bool
	// statDevice reports whether a VAAPI device node exists.
	statDevice    func(string) bool
	renderDevices func() []string
	vendor        func(string) string
}

func (d hwDetector) testEncode(ctx context.Context, accel, encoder string) error {
	return d.testRun(ctx, testEncodeArgs(accel, encoder))
}

func (d hwDetector) testRun(ctx context.Context, args []string) error {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	var stderr bytes.Buffer
	err := d.runner.Run(ctx, Command{Path: d.ffmpeg, Args: args, Stderr: &stderr, Background: d.background})
	if err != nil {
		msg := strings.TrimSpace(stderr.String())
		if len(msg) > 300 {
			msg = msg[len(msg)-300:]
		}
		return fmt.Errorf("%w: %s", err, msg)
	}
	return nil
}

// probeVAAPIRCModes checks which rate-control modes (QVBR, VBR, CBR, CQP)
// a VAAPI encoder supports on device, returning the best mode for quality
// (VBV reservoir) runs and the best mode for explicit bitrate runs.
func (d hwDetector) probeVAAPIRCModes(ctx context.Context, encoder, device string, main10 bool) (qualityRC, bitrateRC string) {
	supported := map[string]bool{}
	for _, rc := range []string{"QVBR", "VBR", "CBR", "CQP"} {
		if err := d.testRun(ctx, vaapiRCProbeArgs(encoder, device, rc, main10, false)); err == nil {
			supported[rc] = true
		}
	}
	for _, rc := range []string{"QVBR", "VBR", "CBR", "CQP"} {
		if supported[rc] {
			qualityRC = rc
			break
		}
	}
	for _, rc := range []string{"VBR", "CBR", "QVBR", "CQP"} {
		if supported[rc] {
			bitrateRC = rc
			break
		}
	}
	return qualityRC, bitrateRC
}

// detect picks the hardware accelerator per the configured mode ("auto",
// "none", or an explicit name). It always succeeds; the CPU fallback is
// AccelNone.
func (d hwDetector) detect(ctx context.Context, mode, vaapiDevice string, f features) core.HWAccel {
	cpu := core.HWAccel{Name: AccelNone}
	for _, e := range []string{"libx264", "libx265"} {
		if f.hasEncoder(e) {
			cpu.Encoders = append(cpu.Encoders, e)
		}
	}
	mode = strings.ToLower(strings.TrimSpace(mode))
	if mode == "" {
		mode = "auto"
	}
	if mode == AccelNone || mode == "cpu" || mode == "software" {
		d.log.Info("hardware acceleration disabled by config")
		return cpu
	}
	devices := []string{vaapiDevice}
	if vaapiDevice == "" {
		devices = nil
		if d.goos == "linux" {
			if d.renderDevices != nil {
				devices = d.renderDevices()
			} else {
				devices, _ = filepath.Glob("/dev/dri/renderD*")
			}
		}
	}
	vendor := d.vendor
	if vendor == nil {
		vendor = GPUVendor
	}
	for _, c := range accelCandidates {
		if mode == "auto" && !contains(c.platform, d.goos) {
			continue
		}
		if mode != "auto" && mode != c.name {
			continue
		}
		if !f.hasEncoder(c.h264) {
			if mode != "auto" {
				d.log.Warn("configured hardware encoder not in this ffmpeg build; using CPU", "hwaccel", c.name, "encoder", c.h264)
			}
			continue
		}
		if c.name == AccelQSV {
			if d.goos != "linux" {
				if hw := d.detectQSV(ctx, "", f); len(hw.Encoders) > 0 {
					return hw
				}
				continue
			}
			for _, node := range devices {
				if vendor(node) != "intel" {
					continue
				}
				if hw := d.detectQSV(ctx, node, f); len(hw.Encoders) > 0 {
					return hw
				}
			}
			continue
		}
		if c.name == AccelVAAPI {
			for _, node := range devices {
				if d.statDevice != nil && !d.statDevice(node) {
					continue
				}
				if hw := d.detectVAAPI(ctx, node, c, f); len(hw.Encoders) > 0 {
					d.log.Info("hardware encoder detected", "hwaccel", hw.Name, "encoders", hw.Encoders, "device", hw.Device, "gpuScale", !hw.NoGPUScale)
					return hw
				}
			}
			continue
		}
		if err := d.testEncode(ctx, c.name, c.h264); err != nil {
			d.log.Info("hardware encoder test failed", "hwaccel", c.name, "encoder", c.h264, "err", err)
			continue
		}
		hw := core.HWAccel{Name: c.name, Encoders: []string{c.h264}}
		if f.hasEncoder(c.hevc) {
			if err := d.testEncode(ctx, c.name, c.hevc); err == nil {
				hw.Encoders = append(hw.Encoders, c.hevc)
			} else {
				d.log.Info("hardware HEVC encoder test failed", "hwaccel", c.name, "encoder", c.hevc, "err", err)
			}
		}
		d.log.Info("hardware encoder detected", "hwaccel", hw.Name, "encoders", hw.Encoders, "device", hw.Device, "gpuScale", !hw.NoGPUScale)
		return hw
	}
	if mode != "auto" {
		d.log.Warn("configured hardware acceleration unavailable; using CPU", "hwaccel", mode)
	} else {
		d.log.Info("no hardware encoder found; using CPU", "encoders", cpu.Encoders)
	}
	return cpu
}

const vaapiMain10 = "hevc_vaapi/main10"

func (d hwDetector) detectVAAPI(ctx context.Context, device string, c accelCandidate, f features) core.HWAccel {
	quality, bitrate := d.probeVAAPIRCModes(ctx, c.h264, device, false)
	if quality == "" {
		d.log.Debug("VAAPI H.264 rate control unavailable", "device", device)
		return core.HWAccel{}
	}
	hw := core.HWAccel{Name: AccelVAAPI, Device: device, Encoders: []string{c.h264}}
	recordRC := func(key, quality, bitrate string) {
		if hw.VAAPIQualityRC == nil {
			hw.VAAPIQualityRC = map[string]string{}
			hw.VAAPIBitrateRC = map[string]string{}
		}
		hw.VAAPIQualityRC[key], hw.VAAPIBitrateRC[key] = quality, bitrate
		d.log.Debug("VAAPI rate control selected", "device", device, "profile", key, "qualityMode", quality, "bitrateMode", bitrate)
	}
	if quality != "QVBR" || bitrate != "VBR" {
		recordRC(c.h264, quality, bitrate)
	}
	hw.NoGPUScale = !f.hasFilter("scale_vaapi") || d.testRun(ctx, vaapiRCProbeArgs(c.h264, device, bitrate, false, true)) != nil
	if !hw.NoGPUScale && f.hasFilter("deinterlace_vaapi") && f.hasFilter("idet") {
		hw.VAAPIDeinterlace = d.probeVAAPIDeinterlace(ctx, device)
		d.log.Debug("VAAPI deinterlace probe", "device", device, "available", hw.VAAPIDeinterlace)
	}
	if f.hasEncoder(c.hevc) {
		if quality, bitrate := d.probeVAAPIRCModes(ctx, c.hevc, device, false); quality != "" {
			hw.Encoders = append(hw.Encoders, c.hevc)
			if quality != "QVBR" || bitrate != "VBR" {
				recordRC(c.hevc, quality, bitrate)
			}
			if quality, bitrate := d.probeVAAPIRCModes(ctx, c.hevc, device, true); quality != "" {
				recordRC(vaapiMain10, quality, bitrate)
				if !hw.NoGPUScale {
					hw.VAAPIMain10Scale = d.testRun(ctx, vaapiRCProbeArgs(c.hevc, device, bitrate, true, true)) == nil
				}
			}
		}
	}
	return hw
}

func deviceExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

func contains(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}
