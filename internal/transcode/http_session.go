package transcode

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

const (
	audioSpoolMaxDuration = 30 * time.Minute
	audioSpoolMaxBytes    = 64 << 20
)

type audioSpool struct {
	mu       sync.Mutex
	cond     *sync.Cond
	path     string
	written  int64
	done     bool
	err      error
	overflow bool
	proc     Process
	cancel   context.CancelFunc
}

func (sp *audioSpool) stop() {
	sp.mu.Lock()
	proc, cancel := sp.proc, sp.cancel
	sp.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if proc != nil {
		proc.Stop()
	}
}

// httpSession is one protocol=http conversion: ffmpeg writes streamable
// Matroska (video) or the target audio container (audio-only) to stdout.
type httpSession struct {
	m    *Manager
	id   string
	dir  string
	req  core.HLSRequest
	life *encoderLifetime
	log  *slog.Logger

	vaapiToneLogged sync.Once
	mu              sync.Mutex
	proc            Process
	pipe            *io.PipeReader
	spool           *audioSpool
	requestSeq      uint64
	closed          bool
	softwareDecode  bool
	forceCPU        bool
	delivered       atomic.Bool
	lastAccess      time.Time
	lastMediaAccess time.Time
}

var _ core.HTTPSession = (*httpSession)(nil)
var _ core.OwnedEncoderSession = (*httpSession)(nil)
var _ core.MediaAccessTracker = (*httpSession)(nil)

func (m *Manager) startHTTP(ctx context.Context, req core.HLSRequest, life *encoderLifetime) (core.HLSSession, error) {
	id := req.SessionID
	if id == "" {
		id = randomHex(12)
		req.SessionID = id
	}
	if req.InputLease != nil {
		if err := life.installInput(req.InputLease); err != nil {
			return nil, err
		}
	}
	if err := m.claimEncoderID(ctx, id, life); err != nil {
		return nil, err
	}
	encoding := req.Info.Video() != nil && !req.Decision.VideoCopy
	if err := m.admitStartEncoding(ctx, req, life, encoding); err != nil {
		return nil, err
	}
	// Attach uses dir for cleanup and any staged external sidecars.
	dir := filepath.Join(m.tempDir, "http-"+unsafeID.ReplaceAllString(id, "_")+"-"+randomHex(4))
	stub := &Session{
		id:              id,
		proto:           core.ProtocolHTTP,
		m:               m,
		dir:             dir,
		log:             m.log.With("session", id, "protocol", core.ProtocolHTTP),
		created:         time.Now(),
		encoder:         life,
		sessionInput:    sessionInput{liveInput: req.LiveInput, noVideoTranscode: req.NoVideoTranscode},
		sessionDelivery: sessionDelivery{changed: make(chan struct{})},
	}
	if !life.attach(stub) {
		return nil, errors.New("transcode: session closed")
	}
	req.Info = m.stageExternalSubtitles(dir, req.Info, req.ReleaseID)
	h := &httpSession{
		m:    m,
		id:   id,
		dir:  dir,
		req:  req,
		life: life,

		log:        stub.log,
		lastAccess: time.Now(),
	}
	// This ticket retains responsibility for HTTP registry/pipe detachment even
	// when Start is canceled before the lazy HTTP session is returned.
	detach, err := life.reserve(1)
	if err != nil {
		return nil, err
	}
	go func() {
		<-life.cancel
		h.detachOwnedHTTP()
		detach[0].finish()
	}()
	life.mu.Lock()
	defer life.mu.Unlock()
	m.mu.Lock()
	if m.closed || ctx.Err() != nil || life.sealed {
		m.mu.Unlock()
		return nil, errors.New("transcode: manager closed or start canceled")
	}
	if m.httpSessions == nil {
		m.httpSessions = map[string]*httpSession{}
	}
	m.httpSessions[id] = h
	m.mu.Unlock()
	h.log.Info("http session starting", "kind", req.Decision.Kind, "offset", req.StartOffset)
	return h, nil
}

func (s *httpSession) ID() string { return s.id }

func (s *httpSession) ServePlaylist(w http.ResponseWriter, r *http.Request) {
	http.Error(w, "not a playlist", http.StatusNotFound)
}

func (s *httpSession) ServeSegment(w http.ResponseWriter, r *http.Request, n int) {
	http.Error(w, "not a segment", http.StatusNotFound)
}

func (s *httpSession) SwapInput(ctx context.Context, inputURL string, info core.MediaInfo, at time.Duration) error {
	info = s.m.stageExternalSubtitles(s.dir, info, "")
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return errors.New("transcode: session closed")
	}
	s.requestSeq++
	if s.pipe != nil {
		_ = s.pipe.CloseWithError(errors.New("transcode: input changed"))
		s.pipe = nil
	}
	if s.spool != nil {
		s.spool.stop()
		s.spool = nil
	}
	s.req.InputURL = inputURL
	s.req.Info = info
	s.req.StartOffset = at
	if s.proc != nil {
		s.proc.Stop()
		s.proc = nil
	}
	return nil
}

func (s *httpSession) LastAccess() time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastAccess
}

func (s *httpSession) LastMediaAccess() time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastMediaAccess
}

func (s *httpSession) Touch() {
	s.mu.Lock()
	s.lastAccess = s.m.now()
	s.mu.Unlock()
}

func (s *httpSession) touchMediaAccess() {
	now := s.m.now()
	s.mu.Lock()
	s.lastAccess = now
	s.lastMediaAccess = now
	s.mu.Unlock()
}

func (s *httpSession) Closed() bool {
	s.mu.Lock()
	closed, life := s.closed, s.life
	s.mu.Unlock()
	return closed || (life != nil && life.isSealed())
}

func (s *httpSession) busy() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.proc != nil
}

// rangeStartsAfterZero reports whether a Range header asks for bytes
// after the start of the body (including suffix ranges). Unparsable
// headers are ignored, as HTTP allows.
func rangeStartsAfterZero(rng string) bool {
	spec, ok := strings.CutPrefix(strings.TrimSpace(rng), "bytes=")
	if !ok {
		return false
	}
	first, _, _ := strings.Cut(spec, ",")
	start, _, ok := strings.Cut(strings.TrimSpace(first), "-")
	if !ok {
		return false
	}
	if start == "" {
		return true // suffix range: the last N bytes of a stream with no end yet
	}
	n, err := strconv.ParseInt(start, 10, 64)
	return err == nil && n > 0
}

// rangeNeedsFullSpool reports whether a Range request requires the complete
// spooled file before responding: a nonzero start offset (seek or suffix
// range) or an explicit end byte (such as Safari/AVPlayer's bytes=0-1 probe).
// Open-ended bytes=0- requests from browsers stream progressively as soon as
// initial bytes are available instead of blocking on full track transcode.
func rangeNeedsFullSpool(rng string) bool {
	if rng == "" {
		return false
	}
	if rangeStartsAfterZero(rng) {
		return true
	}
	spec, ok := strings.CutPrefix(strings.TrimSpace(rng), "bytes=")
	if !ok {
		return false
	}
	first, _, _ := strings.Cut(spec, ",")
	_, end, ok := strings.Cut(strings.TrimSpace(first), "-")
	return ok && strings.TrimSpace(end) != ""
}

func (s *httpSession) httpContentType() string {
	audioOnly := s.req.Info.Video() == nil
	if !audioOnly {
		return "video/x-matroska"
	}
	switch normalizeHTTPAudioContainer(s.req.Decision.Container) {
	case "mp3":
		return "audio/mpeg"
	case "aac":
		return "audio/aac"
	case "mp4":
		return "audio/mp4"
	case "ogg", "opus":
		return "audio/ogg"
	case "webm":
		return "audio/webm"
	case "flac":
		return "audio/flac"
	case "wav":
		return "audio/wav"
	case ContainerMPEGTS:
		return "video/mp2t"
	default:
		return "audio/x-matroska"
	}
}

func (s *httpSession) isSpoolableAudio() bool {
	if s.req.LiveInput || s.req.Info.Video() != nil || s.req.StartOffset > 0 {
		return false
	}
	if s.req.Info.Duration > audioSpoolMaxDuration {
		return false
	}
	return true
}

func (s *httpSession) writeHTTPHeaders(w http.ResponseWriter) {
	h := w.Header()
	h.Set("Content-Type", s.httpContentType())
	h.Set("Accept-Ranges", "none")
	h.Set("Cache-Control", "no-store")
	h.Set("Connection", "close")
	w.WriteHeader(http.StatusOK)
}

func (s *httpSession) ensureAudioSpoolLocked() (*audioSpool, error) {
	if s.spool != nil {
		return s.spool, nil
	}
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return nil, err
	}
	spoolPath := filepath.Join(s.dir, "audio.stream")
	f, err := os.OpenFile(spoolPath, os.O_CREATE|os.O_RDWR|os.O_TRUNC, 0o600)
	if err != nil {
		return nil, err
	}
	scopeHeaders, err := encoderScopeHeaders(s.req.InputLease, s.req.InputURL)
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	spec := runSpec{
		InputHeaders:      s.req.InputHeaders,
		InputScopeHeaders: scopeHeaders,
		Input:             s.req.InputURL,
		Info:              s.req.Info,
		Decision:          s.req.Decision,
		Start:             s.req.StartOffset,
		Accel:             core.HWAccel{Name: AccelNone},
		Features:          s.m.feat,
		Protocol:          core.ProtocolHTTP,
		InputTimeout:      s.m.opts.stallTimeout,
	}
	pr, pw := io.Pipe()
	stderr := newTailBuffer(40)
	cmd := Command{
		Path:       s.m.ffmpeg,
		Args:       buildArgs(spec),
		Stdout:     pw,
		Stderr:     &lineWriter{fn: stderr.add},
		Background: !s.req.LiveInput,
	}
	work, err := s.life.runWork(false)
	if err != nil {
		_ = f.Close()
		_ = pw.Close()
		_ = pr.Close()
		return nil, err
	}
	proc, err := s.m.runner.Start(cmd)
	if err != nil {
		for _, item := range work {
			item.finish()
		}
		_ = f.Close()
		_ = pw.Close()
		_ = pr.Close()
		return nil, err
	}
	s.life.fenceProcess(proc, work[2])
	s.life.markStarted()
	ctx, cancel := context.WithCancel(context.Background())
	sp := &audioSpool{path: spoolPath, proc: proc, cancel: cancel}
	sp.cond = sync.NewCond(&sp.mu)
	s.spool = sp
	go func() {
		<-proc.Done()
		_ = pw.Close()
		work[0].finish()
	}()
	go func() {
		defer work[1].finish()
		defer f.Close()
		defer pr.Close()
		stopCancel := context.AfterFunc(ctx, func() {
			_ = pr.CloseWithError(context.Canceled)
			proc.Stop()
		})
		defer stopCancel()
		buf := make([]byte, 32*1024)
		var copyErr error
		var written int64
		overflow := false
		for {
			n, rerr := pr.Read(buf)
			if n > 0 {
				if written+int64(n) > audioSpoolMaxBytes {
					overflow = true
					proc.Stop()
					break
				}
				if _, werr := f.Write(buf[:n]); werr != nil {
					copyErr = werr
					proc.Stop()
					break
				}
				written += int64(n)
				sp.mu.Lock()
				sp.written = written
				sp.cond.Broadcast()
				sp.mu.Unlock()
			}
			if rerr != nil {
				if !errors.Is(rerr, io.EOF) {
					copyErr = rerr
				}
				break
			}
		}
		<-proc.Done()
		procErr := proc.Err()
		sp.mu.Lock()
		sp.overflow = overflow
		if copyErr != nil {
			sp.err = copyErr
		} else if !overflow && procErr != nil && sp.written == 0 {
			sp.err = procErr
		}
		sp.done = true
		sp.proc = nil
		sp.cond.Broadcast()
		sp.mu.Unlock()
	}()
	return sp, nil
}

func (s *httpSession) serveAudioSpool(w http.ResponseWriter, r *http.Request) bool {
	s.mu.Lock()
	if s.closed || s.life.isSealed() {
		s.mu.Unlock()
		http.Error(w, "session closed", http.StatusGone)
		return true
	}
	s.lastAccess = time.Now()
	sp, err := s.ensureAudioSpoolLocked()
	s.mu.Unlock()
	if err != nil {
		return false
	}
	stopWake := context.AfterFunc(r.Context(), func() {
		sp.mu.Lock()
		sp.cond.Broadcast()
		sp.mu.Unlock()
	})
	defer stopWake()

	needFull := rangeNeedsFullSpool(r.Header.Get("Range")) || r.Method == http.MethodHead
	sp.mu.Lock()
	if needFull {
		for !sp.done && r.Context().Err() == nil {
			sp.cond.Wait()
		}
	} else {
		for !sp.done && sp.written == 0 && r.Context().Err() == nil {
			sp.cond.Wait()
		}
	}
	done, written, overflow, spoolErr := sp.done, sp.written, sp.overflow, sp.err
	sp.mu.Unlock()

	if r.Context().Err() != nil {
		return true
	}
	if overflow {
		return false
	}
	if done && (spoolErr != nil || written == 0) {
		http.Error(w, "transcode failed", http.StatusBadGateway)
		return true
	}
	if done {
		f, err := os.Open(sp.path)
		if err != nil {
			http.Error(w, "transcode failed", http.StatusBadGateway)
			return true
		}
		defer f.Close()
		w.Header().Set("Content-Type", s.httpContentType())
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Accept-Ranges", "bytes")
		s.delivered.Store(true)
		s.touchMediaAccess()
		http.ServeContent(w, r, "", time.Time{}, f)
		return true
	}
	// Open-ended GET while audio transcode is still writing: stream bytes as
	// they arrive without blocking on full track completion.
	f, err := os.Open(sp.path)
	if err != nil {
		http.Error(w, "transcode failed", http.StatusBadGateway)
		return true
	}
	defer f.Close()
	s.writeHTTPHeaders(w)
	fw := &httpWriteTracker{writer: flushWriter{ResponseWriter: w, s: s, timeout: s.m.opts.httpWriteTimeout}}
	var off int64
	buf := make([]byte, 32*1024)
	for {
		sp.mu.Lock()
		for off >= sp.written && !sp.done && r.Context().Err() == nil {
			sp.cond.Wait()
		}
		avail, finished, ov := sp.written, sp.done, sp.overflow
		sp.mu.Unlock()
		if r.Context().Err() != nil || ov {
			return true
		}
		for off < avail {
			n, rerr := f.ReadAt(buf, off)
			if n > 0 {
				if _, werr := fw.Write(buf[:n]); werr != nil {
					return true
				}
				off += int64(n)
			}
			if rerr != nil && !errors.Is(rerr, io.EOF) {
				return true
			}
			if n == 0 {
				break
			}
		}
		if finished && off >= avail {
			return true
		}
	}
}

func (s *httpSession) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if s.isSpoolableAudio() && s.serveAudioSpool(w, r) {
		return
	}
	s.mu.Lock()
	if s.closed || s.life.isSealed() {
		s.mu.Unlock()
		http.Error(w, "session closed", http.StatusGone)
		return
	}
	s.lastAccess = time.Now()
	// A byte range cannot resume an encoder's Matroska output: a new encoder
	// writes a different header and its offsets do not describe the old stream.
	// Roku retries a stalled progressive stream with a nonzero Range (#650):
	// answering 200 with a restarted encode corrupts its demuxer, and the
	// restart also supersedes (kills) the connection it is still playing,
	// whose clean EOF then looks like the end of the title. Refuse the
	// range without touching the running encoder; a range from byte 0 is
	// just a fresh GET and is served normally.
	if rng := r.Header.Get("Range"); rng != "" && (s.forceCPU || rangeStartsAfterZero(rng)) {
		s.mu.Unlock()
		http.Error(w, "progressive transcode does not support byte ranges", http.StatusRequestedRangeNotSatisfiable)
		return
	}
	if r.Method == http.MethodHead {
		s.mu.Unlock()
		s.writeHTTPHeaders(w)
		return
	}
	s.requestSeq++
	seq := s.requestSeq
	if s.pipe != nil {
		_ = s.pipe.CloseWithError(errors.New("transcode: superseded request"))
		s.pipe = nil
	}
	if s.proc != nil {
		s.proc.Stop()
		s.proc = nil
	}
	probe := runSpec{LiveInput: s.req.LiveInput, CopyTimestamps: s.req.CopyTimestamps, InputHeaders: s.req.InputHeaders, Input: s.req.InputURL, Info: s.req.Info, Decision: s.req.Decision, Start: s.req.StartOffset, Protocol: core.ProtocolHTTP}
	lease := s.req.InputLease
	s.mu.Unlock()
	if needsCopyKeyframe(probe) {
		if scope, err := encoderScopeHeaders(lease, probe.Input); err == nil {
			probe.InputScopeHeaders = scope
			probe.SeekFrom, probe.HasSeekFrom = s.m.copyKeyframeBefore(r.Context(), probe)
		}
	}
	silenceAudio := false
	for attempt := 0; attempt < 2; attempt++ {
		s.mu.Lock()
		if s.closed || s.life.isSealed() || s.requestSeq != seq || r.Context().Err() != nil {
			s.mu.Unlock()
			return
		}
		accel := s.m.hw
		if s.forceCPU || time.Now().UnixNano() < s.m.httpHWFailedUntil.Load() {
			accel = core.HWAccel{Name: AccelNone}
		}
		spec := runSpec{SoftwareDecode: s.softwareDecode, SilenceAudio: silenceAudio, LiveInput: s.req.LiveInput, CopyTimestamps: s.req.CopyTimestamps, InputHeaders: s.req.InputHeaders, Input: s.req.InputURL, Info: s.req.Info, Decision: s.req.Decision, Start: s.req.StartOffset, Accel: accel, Features: s.m.feat, Protocol: core.ProtocolHTTP, InputTimeout: s.m.opts.stallTimeout}
		scopeHeaders, err := encoderScopeHeaders(s.req.InputLease, s.req.InputURL)
		if err != nil {
			s.mu.Unlock()
			http.Error(w, "input authority unavailable", http.StatusServiceUnavailable)
			return
		}
		spec.InputScopeHeaders = scopeHeaders
		if probe.HasSeekFrom && spec.Input == probe.Input && spec.Start == probe.Start {
			spec.SeekFrom, spec.HasSeekFrom = probe.SeekFrom, true
		}
		finishTone := s.m.admitVAAPITone(&spec)
		if spec.vaapiToneDecision != nil && spec.vaapiToneDecision.Route == "unavailable" {
			s.mu.Unlock()
			s.m.recordVAAPITone(spec.vaapiToneDecision, s.log, &s.vaapiToneLogged)
			http.Error(w, "transcode failed", http.StatusServiceUnavailable)
			return
		}
		pr, pw := io.Pipe()
		stderr := newTailBuffer(40)
		cmd := Command{Path: s.m.ffmpeg, Args: buildArgs(spec), Stdout: pw, Stderr: &lineWriter{fn: stderr.add}}
		encoding := s.req.Info.Video() != nil && !s.req.Decision.VideoCopy
		work, err := s.life.runWork(encoding)
		if err != nil {
			if finishTone != nil {
				finishTone(nil)
			}
			s.mu.Unlock()
			_ = pw.Close()
			_ = pr.Close()
			http.Error(w, "transcode failed", http.StatusServiceUnavailable)
			return
		}
		// Progressive encodes run at normal priority; the output pipe
		// naturally backpressures them when the client has enough media.
		cmd.Background = !s.req.LiveInput && !s.req.Decision.EncodesVideo(s.req.Info)
		proc, err := s.m.runner.Start(cmd)
		if finishTone != nil {
			finishTone(proc)
		}
		if err != nil {
			s.mu.Unlock()
			for _, workItem := range work {
				workItem.finish()
			}
			_ = pw.Close()
			_ = pr.Close()
			s.log.Warn("http ffmpeg start", "err", err)
			http.Error(w, "transcode failed", http.StatusInternalServerError)
			return
		}
		s.m.recordVAAPITone(spec.vaapiToneDecision, s.log, &s.vaapiToneLogged)
		s.life.fenceProcess(proc, work[2])
		s.proc = proc
		s.pipe = pr
		s.life.markStarted()
		s.life.encoding.Store(encoding)
		s.mu.Unlock()
		// Process exit and the serving writer are independent producers. A blocked
		// downstream writer must keep retirement pending even after ffmpeg exits.
		defer work[1].finish()
		go func() {
			<-proc.Done()
			_ = pw.Close()
			work[0].finish()
		}()
		stopCancel := context.AfterFunc(r.Context(), func() { _ = pr.CloseWithError(r.Context().Err()); proc.Stop() })
		var started atomic.Bool
		var timer *time.Timer
		if timeout := s.m.opts.stallTimeout; timeout > 0 {
			timer = time.AfterFunc(timeout, func() {
				if !started.Load() {
					_ = pr.CloseWithError(errors.New("transcode: HTTP startup stalled"))
					proc.Stop()
				}
			})
		}
		buf := make([]byte, 32*1024)
		n, readErr := pr.Read(buf)
		if n > 0 {
			started.Store(true)
		}
		if timer != nil {
			timer.Stop()
		}
		var copyErr error
		clientWriteFailed := false
		if n > 0 {
			s.writeHTTPHeaders(w)
			fw := &httpWriteTracker{writer: flushWriter{ResponseWriter: w, s: s, timeout: s.m.opts.httpWriteTimeout}}
			if spec.usesHW() {
				copyErr = copyHTTPMatroska(fw, io.MultiReader(bytes.NewReader(buf[:n]), pr))
			} else {
				_, copyErr = fw.Write(buf[:n])
				if copyErr == nil && readErr == nil {
					_, copyErr = io.Copy(fw, pr)
				}
			}
			clientWriteFailed = fw.failed
		}
		_ = pr.Close()
		stopCancel()
		if r.Context().Err() != nil || copyErr != nil || n == 0 {
			proc.Stop()
		}
		<-proc.Done()
		procErr := proc.Err()
		s.mu.Lock()
		current := !s.closed && s.requestSeq == seq
		hardwareFailed := current && r.Context().Err() == nil && !clientWriteFailed && spec.usesHW() && procErr != nil && looksLikeEncoderFailure(stderr.String(), spec)
		if hardwareFailed && decodeInitFailure(stderr.String(), spec) {
			s.softwareDecode = true
			if caps := spec.Features.decode; caps != nil {
				caps.reject(spec.Accel, spec.Info.Video())
			}
			s.log.Warn("progressive hardware decode failed; retrying with software decode and hardware encode", "hwaccel", spec.Accel.Name)
		} else if hardwareFailed {
			s.forceCPU = true
			s.m.httpHWFailedUntil.Store(time.Now().Add(5 * time.Minute).UnixNano())
			s.log.Warn("progressive hardware encoder failed; next request will use CPU", "hwaccel", spec.Accel.Name)
		}
		s.lastAccess = time.Now()
		if s.proc == proc {
			s.proc = nil
		}
		if s.pipe == pr {
			s.pipe = nil
		}
		s.mu.Unlock()
		if n > 0 {
			if hardwareFailed {
				// net/http closes the HTTP/1 connection (or resets an HTTP/2
				// stream) without a final chunk. A clean EOF looks like a
				// completed movie to clients and can advance the play queue.
				panic(http.ErrAbortHandler)
			}
			return
		}
		if r.Context().Err() != nil || !current {
			return
		}
		if attempt == 0 {
			if spec.LiveInput && procErr != nil && missingLiveAudio(spec, stderr.String()) {
				silenceAudio = true
			}
			s.log.Warn("http transcode produced no output; retrying")
			continue
		}
		http.Error(w, "transcode failed", http.StatusBadGateway)
		return
	}
}

// ProgressiveFailed reports a hardware failure to the timeline owner so a
// late terminal update cannot replace the last reported playhead with EOF.
func (s *httpSession) ProgressiveFailed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.forceCPU
}

func (s *httpSession) Close() error {
	if s.life != nil {
		if err := s.RetireEncoder(); err != nil {
			return err
		}
		// Match Session.Close: wait for the encoder lifetime to drain work and
		// release its input lease before returning, even if detachOwnedHTTP
		// already marked s.closed after RetireEncoder sealed s.life.
		timer := time.NewTimer(s.m.encoderWait())
		defer timer.Stop()
		select {
		case <-s.life.done:
			return s.life.legacyResult()
		case <-timer.C:
			return core.ErrEncoderPending
		}
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	proc := s.proc
	pipe := s.pipe
	spool := s.spool
	s.pipe = nil
	s.proc = nil
	s.spool = nil
	s.mu.Unlock()
	if spool != nil {
		spool.stop()
	}
	if pipe != nil {
		_ = pipe.CloseWithError(errors.New("transcode: session closed"))
	}
	if proc != nil {
		proc.Stop()
		<-proc.Done()
	}
	if s.m != nil {
		s.m.mu.Lock()
		if s.m.httpSessions[s.id] == s {
			delete(s.m.httpSessions, s.id)
		}
		s.m.mu.Unlock()
	}
	return nil
}

type flushWriter struct {
	http.ResponseWriter
	s       *httpSession
	timeout time.Duration
}

type httpWriteTracker struct {
	writer io.Writer
	failed bool
}

func (w *httpWriteTracker) Write(p []byte) (int, error) {
	n, err := w.writer.Write(p)
	if err != nil || n != len(p) {
		w.failed = true
	}
	return n, err
}

func (w flushWriter) Write(p []byte) (int, error) {
	if w.s != nil && len(p) > 0 {
		w.s.touchMediaAccess()
	} else if w.s != nil {
		w.s.Touch()
	}
	// Bound each transport operation, not the lifetime of the movie. Clear
	// the deadline even on error: waiting for the encoder's next bytes must
	// not consume the next write's budget or poison a reused connection.
	rc := http.NewResponseController(w.ResponseWriter)
	timeout := w.timeout
	if timeout <= 0 {
		if w.s != nil && w.s.m != nil {
			timeout = w.s.m.effectivePauseRetention()
			if w.s.m.opts.idleTimeout > 0 && timeout < w.s.m.opts.idleTimeout {
				timeout = w.s.m.opts.idleTimeout
			}
		} else {
			timeout = core.PausedSessionRetention
		}
	}
	now := time.Now
	if w.s != nil && w.s.m != nil {
		now = w.s.m.now
	}
	arm := func() error {
		err := rc.SetWriteDeadline(now().Add(timeout))
		if errors.Is(err, http.ErrNotSupported) {
			return nil // e.g. in-memory recorders
		}
		return err
	}
	defer rc.SetWriteDeadline(time.Time{})
	if err := arm(); err != nil {
		return 0, err
	}
	n, err := w.ResponseWriter.Write(p)
	if n > 0 && w.s != nil {
		w.s.delivered.Store(true)
	}
	if err == nil {
		if err = arm(); err == nil {
			err = rc.Flush()
			if errors.Is(err, http.ErrNotSupported) {
				err = nil
			}
		}
	}
	return n, err
}

// EncoderRetirement covers the exact lazy HTTP execution and its input producers.
func (s *httpSession) EncoderRetirement() core.EncoderRetirement { return s.life }

// RetireEncoder never takes the HTTP mutex (Runner.Start may hold it) or waits
// for process exit. The registered detach producer handles late publication.
func (s *httpSession) RetireEncoder() error {
	if s.life == nil {
		return errors.New("transcode: session has no encoder responsibility")
	}
	s.life.seal()
	return nil
}

func (s *httpSession) detachOwnedHTTP() {
	s.mu.Lock()
	s.closed = true
	s.requestSeq++
	proc, pipe, spool := s.proc, s.pipe, s.spool
	s.proc, s.pipe, s.spool = nil, nil, nil
	s.mu.Unlock()
	if spool != nil {
		spool.stop()
	}
	if pipe != nil {
		_ = pipe.CloseWithError(errors.New("transcode: session retired"))
	}
	if proc != nil {
		proc.Stop()
	}
	s.m.mu.Lock()
	if s.m.httpSessions[s.id] == s {
		delete(s.m.httpSessions, s.id)
	}
	s.m.mu.Unlock()
}

// PlaybackDelivered reports output bytes, not source reads by ffmpeg.
func (s *httpSession) PlaybackDelivered() bool { return s.delivered.Load() }
