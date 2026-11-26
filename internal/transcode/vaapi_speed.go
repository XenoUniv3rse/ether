package transcode

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	vaapiMinimumFactor = 1.2
	vaapiOffloadFactor = 1.5
	vaapiCPUStreams    = 2
	vaapiHighCPU       = 75
)

type vaapiSpeedKey struct {
	vaapiToneKey
	input, output int
}

type vaapiSpeedResult struct {
	method   vaapiToneMethod
	cpu, gpu float64
}

type VAAPIToneDecision struct {
	Route     string  `json:"route"`
	Output    int     `json:"output"`
	CPU       float64 `json:"cpu,omitempty"`
	GPU       float64 `json:"gpu,omitempty"`
	method    vaapiToneMethod
	key       vaapiSpeedKey
	ready     <-chan struct{}
	contended bool
}

type vaapiSpeedEntry struct {
	done   chan struct{}
	result vaapiSpeedResult
}

type vaapiSpeedCache struct {
	m      *Manager
	ctx    context.Context
	cancel context.CancelFunc
	mu     sync.Mutex
	wg     sync.WaitGroup
	work   chan struct{}
	runs   map[vaapiSpeedKey]*vaapiSpeedEntry
	last   *VAAPIToneDecision
}

func newVAAPISpeedCache(m *Manager) *vaapiSpeedCache {
	ctx, cancel := context.WithCancel(context.Background())
	return &vaapiSpeedCache{m: m, ctx: ctx, cancel: cancel,
		work: make(chan struct{}, 1), runs: make(map[vaapiSpeedKey]*vaapiSpeedEntry)}
}

func (c *vaapiSpeedCache) close() {
	c.mu.Lock()
	c.cancel()
	c.mu.Unlock()
	c.wg.Wait()
}

func vaapiResolutionClass(w, h int) int {
	switch {
	case w <= 1280 && h <= 720:
		return 720
	case w <= 1920 && h <= 1080:
		return 1080
	default:
		return 2160
	}
}

// A cache belongs to one FFmpeg build and selected render node for the life
// of the manager, just like the capability probe. Serial measurements avoid
// benchmarking routes against other calibration jobs.
func (c *vaapiSpeedCache) demand(key vaapiSpeedKey) (vaapiSpeedResult, <-chan struct{}) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if entry := c.runs[key]; entry != nil {
		return entry.result, entry.done
	}
	if c.ctx.Err() != nil {
		return vaapiSpeedResult{}, nil
	}
	entry := &vaapiSpeedEntry{done: make(chan struct{})}
	c.runs[key] = entry
	c.wg.Go(func() {
		defer close(entry.done)
		select {
		case c.work <- struct{}{}:
		case <-c.ctx.Done():
			return
		}
		defer func() { <-c.work }()
		result := c.benchmark(key)
		c.mu.Lock()
		entry.result = result
		c.mu.Unlock()
	})
	return vaapiSpeedResult{}, entry.done
}

func vaapiCPUToneFilter(f features, tenBit bool) string {
	if !tenBit && f.hasFilter("tonemapx") {
		return "format=yuv420p10le,tonemapx=tonemap=hable:desat=0:t=bt709:m=bt709:p=bt709:format=yuv420p"
	}
	if f.hasFilter("zscale") && f.hasFilter("tonemap") {
		filter, _ := cpuToneMap(f)
		return filter
	}
	return ""
}

func (c *vaapiSpeedCache) benchmark(key vaapiSpeedKey) vaapiSpeedResult {
	m := c.m
	method := m.feat.vaapiToneMap(key.vaapiToneKey)
	d := hwDetector{runner: m.runner, ffmpeg: m.ffmpeg, log: m.log, background: true}
	ctx, cancel := context.WithTimeout(c.ctx, 24*time.Second)
	defer cancel()
	dir, err := os.MkdirTemp(m.tempDir, "vaapi-speed-")
	if err != nil {
		return vaapiSpeedResult{}
	}
	defer os.RemoveAll(dir)
	const seconds = 3
	sample := filepath.Join(dir, "hdr.mkv")
	prefix := []string{"-hide_banner", "-nostdin", "-loglevel", "error", "-xerror", "-abort_on", "empty_output"}
	src := vaapiHDRSample(key.vaapiToneKey, key.input*16/9, key.input, seconds)
	args := append(prefix, "-f", "lavfi", "-i", src, "-c:v", "libx265", "-preset", "ultrafast",
		"-x265-params", "pools=4:frame-threads=2:log-level=error"+vaapiHDRMetadata(key.source), sample)
	if d.testRun(ctx, args) != nil {
		return vaapiSpeedResult{}
	}

	w, h := key.output*16/9, key.output
	pic := picturePlan{w: w, h: h, tenBit: key.tenBit}
	spec := runSpec{Accel: m.hw, Rate: rateControl{TargetKbps: 5000}}
	encode := planEncodeArgs(spec, pic, key.encoder, strings.TrimSuffix(key.encoder, "_vaapi"), true)
	measure := func(input []string, chain string, software bool) float64 {
		args := append(append([]string{}, prefix...), input...)
		if software {
			threads := min(4, softwareThreadBudget(w, h, runtime.GOMAXPROCS(0)))
			args = append(args, "-filter_threads", strconv.Itoa(threads))
		}
		args = append(args, "-i", sample, "-vf", chain, "-an")
		args = append(args, encode...)
		routeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		start := time.Now()
		err := d.testRun(routeCtx, append(args, "-f", "null", "-"))
		elapsed := time.Since(start)
		if err == nil || (ctx.Err() == nil && routeCtx.Err() == context.DeadlineExceeded) {
			// On timeout this is an upper bound, already below admission.
			return seconds / elapsed.Seconds()
		}
		return 0
	}
	pixel := "nv12"
	if key.tenBit {
		pixel = "p010le"
	}
	cpuInput := []string{"-init_hw_device", "vaapi=va:" + m.hw.Device, "-filter_hw_device", "va", "-hwaccel", "vaapi", "-hwaccel_device", "va"}
	cpuFilter := vaapiCPUToneFilter(m.feat, key.tenBit)
	if key.input != key.output {
		scale := fmt.Sprintf("scale=%d:%d", w, h)
		if !key.tenBit && m.feat.hasFilter("tonemapx") && key.input > key.output {
			cpuFilter = scale + "," + cpuFilter
		} else {
			cpuFilter += "," + scale
		}
	}
	cpuChain := cpuFilter + ",format=" + pixel + ",hwupload"
	result := vaapiSpeedResult{method: method, cpu: measure(cpuInput, cpuChain, true)}
	if method != "" {
		input, chain, _ := vaapiToneGraph(method, m.hw.Device, w, h, true, key.tenBit, false)
		result.gpu = measure(input, strings.Join(chain, ","), false)
	}
	return result
}

func (r vaapiSpeedResult) choose(output int, cpuBusy bool, activeCPU, activeGPU int) VAAPIToneDecision {
	d := VAAPIToneDecision{Route: "cpu", Output: output, CPU: r.cpu, GPU: r.gpu}
	// The finite frame-copy and memory bandwidth is shared by both
	// paths. Reserve headroom against existing work before admitting more.
	cpu := r.cpu / (1 + 0.18*float64(activeCPU) + 0.55*float64(activeGPU))
	gpu := r.gpu / (1 + 0.40*float64(activeGPU) + 0.20*float64(activeCPU))
	switch {
	case gpu >= vaapiMinimumFactor && (gpu > cpu || cpu < vaapiMinimumFactor || cpuBusy && gpu >= vaapiOffloadFactor):
		d.Route, d.method = "gpu", r.method
	case cpu < vaapiMinimumFactor && (r.cpu > 0 || r.gpu > 0):
		d.Route = "unavailable"
		d.contended = r.cpu >= vaapiMinimumFactor || r.gpu >= vaapiMinimumFactor
	}
	return d
}

// The main and private transcoders share the same host resources.
var vaapiToneAdmission struct {
	sync.Mutex
	cpu int
	gpu map[string]int
}

func (m *Manager) admitVAAPITone(s *runSpec) func(Process) {
	cache := m.vaapiSpeed
	if cache == nil || s.Accel.Name != AccelVAAPI || s.Accel.NoGPUTonemap || !s.Decision.EncodesVideo(s.Info) {
		return nil
	}
	v := s.Info.Video()
	encoder, hw := s.videoEncoder()
	if !hw {
		return nil
	}
	pic := planPicture(*s, v, strings.TrimSuffix(encoder, "_vaapi"))
	source := encodedRange(v)
	if !pic.tonemap || pic.convert != "" || (source != rangeHDR10 && source != rangeHLG) ||
		vaapiCPUToneFilter(m.feat, pic.tenBit) == "" || pic.w <= 0 || pic.h <= 0 {
		return nil
	}
	key := vaapiSpeedKey{
		vaapiToneKey: vaapiToneKey{source: source, encoder: encoder, tenBit: pic.tenBit, full: pictureOf(v).full},
		input:        vaapiResolutionClass(v.Width, v.Height), output: vaapiResolutionClass(pic.w, pic.h),
	}
	result, ready := cache.demand(key)
	hostBusy := m.opts.cpuLoad != nil && m.opts.cpuLoad() >= vaapiHighCPU
	admission := &vaapiToneAdmission
	device := vaapiDevice(s.Accel)
	admission.Lock()
	choice := result.choose(key.output, hostBusy || admission.cpu >= vaapiCPUStreams, admission.cpu, admission.gpu[device])
	choice.key, choice.ready = key, ready
	s.vaapiToneDecision = &choice
	switch choice.Route {
	case "cpu":
		admission.cpu++
	case "gpu":
		if admission.gpu == nil {
			admission.gpu = make(map[string]int)
		}
		admission.gpu[device]++
	default:
		admission.Unlock()
		return nil
	}
	admission.Unlock()
	release := func() {
		admission.Lock()
		if choice.Route == "cpu" {
			admission.cpu--
		} else {
			admission.gpu[device]--
			if admission.gpu[device] == 0 {
				delete(admission.gpu, device)
			}
		}
		admission.Unlock()
	}
	return func(p Process) {
		if p == nil {
			release()
			return
		}
		go func() { <-p.Done(); release() }()
	}
}

func (m *Manager) recordVAAPITone(d *VAAPIToneDecision, log *slog.Logger, once *sync.Once) {
	if d == nil {
		return
	}
	m.vaapiSpeed.mu.Lock()
	m.vaapiSpeed.last = d
	m.vaapiSpeed.mu.Unlock()
	once.Do(func() {
		report := func() {
			result := m.vaapiSpeed.withFactors(*d)
			log.Info("VAAPI tone-map route", "route", result.Route, "gpuMethod", result.method,
				"output", result.Output, "cpuFactor", result.CPU, "gpuFactor", result.GPU,
				"calibratingAtStart", d.CPU == 0 && d.GPU == 0)
		}
		if d.ready == nil {
			report()
			return
		}
		select {
		case <-d.ready:
			report()
		default:
			go func() { <-d.ready; report() }()
		}
	})
}

func (c *vaapiSpeedCache) withFactors(d VAAPIToneDecision) VAAPIToneDecision {
	c.mu.Lock()
	defer c.mu.Unlock()
	if entry := c.runs[d.key]; entry != nil {
		d.CPU, d.GPU = entry.result.cpu, entry.result.gpu
	}
	return d
}

func (m *Manager) VAAPIToneDecision() *VAAPIToneDecision {
	if m.vaapiSpeed == nil {
		return nil
	}
	m.vaapiSpeed.mu.Lock()
	last := m.vaapiSpeed.last
	m.vaapiSpeed.mu.Unlock()
	if last == nil {
		return nil
	}
	decision := m.vaapiSpeed.withFactors(*last)
	return &decision
}
