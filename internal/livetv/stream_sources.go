package livetv

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

// StreamSource is one independently replaceable input to a channel. EPG,
// numbering, favourites and viewer policy belong to Channel, never to an input.
type StreamSource struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	URL      string `json:"url"`
	Enabled  bool   `json:"enabled"`
	Priority int    `json:"priority"` // smaller first; equal priorities retain list order
	// AuthSourceID names the source whose provider credentials this input
	// uses; empty means the channel's own source.
	AuthSourceID string `json:"authSourceId,omitempty"`
	// MergedFrom names the imported channel ID whose stream was attached by
	// channel input merge rules; empty on the channel's own or manual inputs.
	MergedFrom string `json:"mergedFrom,omitempty"`
	// AutoFrom names the duplicate channel ID whose primary stream was
	// attached automatically as a backup; AutoNumber records its channel number.
	AutoFrom   string `json:"autoFrom,omitempty"`
	AutoNumber string `json:"autoNumber,omitempty"`
	// Headers are request headers this input needs (a Stremio stream's
	// proxy headers), sent after the source's own.
	Headers map[string]string `json:"headers,omitempty"`
}

// MaxChannelInputs bounds one channel's stream inputs.
const MaxChannelInputs = 16

// maxInputHeaders bounds one input's request headers.
const maxInputHeaders = 16

// CleanInputHeaders keeps the request headers an input may carry, with
// canonical names: no headers the HTTP stack or the proxy manages, no line
// breaks, at most 16. It returns nil when none remain.
func CleanInputHeaders(in map[string]string) map[string]string {
	var out map[string]string
	for k, v := range in {
		name := http.CanonicalHeaderKey(strings.TrimSpace(k))
		if !validInputHeader(name, v) || len(out) == maxInputHeaders {
			continue
		}
		if out == nil {
			out = map[string]string{}
		}
		out[name] = v
	}
	return out
}

func validInputHeader(name, value string) bool {
	return name != "" && len(name) <= 128 && len(value) <= 4096 &&
		!strings.ContainsAny(name, " :\r\n") && !strings.ContainsAny(value, "\r\n") && !forbiddenHeaders[name]
}

type StreamSourceStatus struct {
	ID          string    `json:"id"`
	State       string    `json:"state"`
	Failures    int       `json:"failures"`
	LastChecked time.Time `json:"lastChecked,omitempty"`
	LastPlayed  time.Time `json:"lastPlayed,omitempty"`
	RetryAfter  time.Time `json:"retryAfter,omitempty"`
	LastError   string    `json:"lastError,omitempty"`
	// Learned is what the source's history says about its media.
	Learned *LearnedSourceHealth `json:"learned,omitempty"`
}

type streamSourceHealth struct {
	StreamSourceStatus
}

func validStreamURL(raw string) bool {
	if validHTTP(raw) {
		return true
	}
	u, e := url.Parse(raw)
	return e == nil && u.Scheme == "plugin" && u.Host != "" && strings.Trim(u.Path, "/") != ""
}

func normalizeStreamSources(c *Channel) error {
	if len(c.StreamSources) == 0 {
		if !validStreamURL(c.StreamURL) {
			return fmt.Errorf("IPTV needs an HTTP(S) stream source")
		}
		id := stableID(c.ID, "primary")
		if c.SourceID != "" {
			id = stableID(c.ID, "playlist")
		}
		c.StreamSources = []StreamSource{{ID: id, Name: "Primary stream", URL: c.StreamURL, Enabled: true}}
	}
	if len(c.StreamSources) > MaxChannelInputs {
		return fmt.Errorf("a channel supports at most %d stream sources", MaxChannelInputs)
	}
	c.StreamSources = append([]StreamSource(nil), c.StreamSources...)
	ids, urls := map[string]bool{}, map[string]bool{}
	for i := range c.StreamSources {
		src := &c.StreamSources[i]
		src.URL = strings.TrimSpace(src.URL)
		src.Name = strings.TrimSpace(src.Name)
		if !validStreamURL(src.URL) {
			return fmt.Errorf("stream source %d must use HTTP(S)", i+1)
		}
		if src.ID == "" {
			src.ID = stableID(c.ID, src.URL)
		}
		if len(src.ID) > 200 || len(src.Name) > 200 || src.Priority < 0 || ids[src.ID] || urls[src.URL] {
			return fmt.Errorf("stream sources need unique IDs and URLs, valid names and nonnegative priorities")
		}
		if src.Name == "" {
			src.Name = fmt.Sprintf("Stream %d", i+1)
		}
		if len(src.Headers) > maxInputHeaders {
			return fmt.Errorf("stream source %d has more than %d headers", i+1, maxInputHeaders)
		}
		for k, v := range src.Headers {
			if !validInputHeader(http.CanonicalHeaderKey(strings.TrimSpace(k)), v) {
				return fmt.Errorf("stream source %d: header %q can't be set", i+1, k)
			}
		}
		ids[src.ID], urls[src.URL] = true, true
	}
	sort.SliceStable(c.StreamSources, func(i, j int) bool { return c.StreamSources[i].Priority < c.StreamSources[j].Priority })
	c.StreamURL = ""
	for i := range c.StreamSources {
		c.StreamSources[i].Priority = i
		if c.StreamURL == "" && c.StreamSources[i].Enabled {
			c.StreamURL = c.StreamSources[i].URL
		}
	}
	if c.Enabled && c.StreamURL == "" {
		return fmt.Errorf("an enabled IPTV channel needs an enabled stream source")
	}
	return nil
}

func mergeImportedStreams(imported, previous Channel) []StreamSource {
	if len(previous.StreamSources) == 0 {
		return imported.StreamSources
	}
	out := append([]StreamSource(nil), previous.StreamSources...)
	// Only the import-owned entry follows refreshed playlist URLs. Removing that
	// entry is an explicit owner override; refresh must not recreate it.
	for i := range out {
		if out[i].ID == stableID(imported.ID, "playlist") {
			out[i].URL = imported.StreamURL
			for _, src := range imported.StreamSources {
				if src.ID == out[i].ID {
					out[i].Headers = CleanInputHeaders(src.Headers)
					break
				}
			}
		}
	}
	return out
}

func sourceHealthKey(channelID string, src StreamSource) string {
	// A replacement URL starts with fresh health even when its entry ID survives.
	return stableID(channelID, src.ID, src.URL)
}

func (s *Service) sourceStatus(channelID string, src StreamSource) StreamSourceStatus {
	s.sourceMu.Lock()
	h := s.sourceHealth[sourceHealthKey(channelID, src)]
	s.sourceMu.Unlock()
	h.ID = src.ID
	switch {
	case !src.Enabled:
		h.State = "disabled"
	case h.RetryAfter.After(s.o.Now()):
		h.State = "cooling_down"
	case h.LastChecked.IsZero():
		h.State = "unchecked"
	case h.LastError != "":
		h.State = "retry_ready"
	default:
		h.State = "available"
	}
	return h.StreamSourceStatus
}

func (s *Service) autoDonorActive(donorID string) bool {
	if donorID == "" || s == nil {
		return true
	}
	if donor, ok := s.cachedChannelByID(donorID); ok {
		return donor.Enabled && donor.MergedInto == ""
	}
	return true
}

// orderedSourceCandidates lists a channel's enabled inputs, best first. Health
// only orders them: a cooling-down source still appears, after every source
// that is not, so recovery never waits out a cooldown while any input remains.
// Unless the owner manually ordered streamSources, the channel's own healthy or
// unchecked primary inputs always precede automatic duplicate backups, and
// automatic backups rank by measured stall history, resolution closeness and
// source affinity. exclude skips source IDs (typically the one that just failed).
func (s *Service) orderedSourceCandidates(c Channel, exclude ...string) []StreamSource {
	if err := normalizeStreamSources(&c); err != nil {
		return nil
	}
	type ranked struct {
		src    StreamSource
		band   int
		retry  time.Time
		played time.Time
	}
	var out []ranked
	for _, src := range c.StreamSources {
		if !src.Enabled || slices.Contains(exclude, src.ID) || !s.autoDonorActive(src.AutoFrom) {
			continue
		}
		st := s.sourceStatus(c.ID, src)
		band := 1
		switch st.State {
		case "available":
			band = 0
			if !c.OwnerStreamSources && src.AutoFrom != "" {
				band = 2
			}
		case "unchecked":
			band = 1
			if !c.OwnerStreamSources && src.AutoFrom != "" {
				band = 2
			}
		case "retry_ready":
			band = 3
		case "cooling_down":
			band = 4
		}
		out = append(out, ranked{src: src, band: band, retry: st.RetryAfter, played: st.LastPlayed})
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].band != out[j].band {
			return out[i].band < out[j].band
		}
		if !c.OwnerStreamSources && out[i].band == 3 && (out[i].src.AutoFrom == "") != (out[j].src.AutoFrom == "") {
			return out[i].src.AutoFrom == ""
		}
		if !c.OwnerStreamSources && out[i].band < 4 && out[i].src.AutoFrom != "" && out[j].src.AutoFrom != "" {
			if cmp := s.compareRuntimeAutoBackups(c, out[i].src, out[j].src); cmp != 0 {
				return cmp < 0
			}
		}
		if out[i].band == 0 && !out[i].played.Equal(out[j].played) {
			return out[i].played.After(out[j].played)
		}
		return out[i].band == 4 && out[i].retry.Before(out[j].retry)
	})
	list := make([]StreamSource, len(out))
	for i, r := range out {
		list[i] = r.src
	}
	return list
}

// StreamSourceStatuses is owner-only health, without source URLs or upstream
// errors that might contain credentials. Transient failures never ban a source.
func (s *Service) StreamSourceStatuses(ctx context.Context, channelID string) ([]StreamSourceStatus, error) {
	c, err := s.Channel(ctx, channelID)
	if err != nil {
		return nil, err
	}
	if c.Kind != "iptv" {
		return []StreamSourceStatus{}, nil
	}
	if err := normalizeStreamSources(&c); err != nil {
		return nil, err
	}
	out := make([]StreamSourceStatus, 0, len(c.StreamSources))
	for _, src := range c.StreamSources {
		st := s.sourceStatus(c.ID, src)
		st.Learned = s.learnedHealth(c.ID, src)
		out = append(out, st)
	}
	return out, nil
}

func (s *Service) recordSourceHealth(channelID string, src StreamSource, err error, played bool) {
	if src.ID == "" || strings.HasPrefix(channelID, directChannelPrefix) || errors.Is(err, context.Canceled) ||
		errors.Is(err, core.ErrStreamLimit) || errors.Is(err, ErrProviderPaced) {
		return
	}
	s.sourceMu.Lock()
	if s.sourceHealth == nil {
		s.sourceHealth = map[string]streamSourceHealth{}
	}
	key := sourceHealthKey(channelID, src)
	h := s.sourceHealth[key]
	h.ID, h.LastChecked = src.ID, s.o.Now()
	if err != nil {
		h.Failures++
		cooldown := 15 * time.Second
		for i := 1; i < h.Failures && cooldown < 2*time.Minute; i++ {
			cooldown *= 2
		}
		if cooldown > 2*time.Minute {
			cooldown = 2 * time.Minute
		}
		h.RetryAfter = s.o.Now().Add(cooldown)
		h.LastError = "source unavailable"
		if errors.Is(err, context.DeadlineExceeded) {
			h.LastError = "source timed out"
		}
	} else {
		h.Failures, h.LastError, h.RetryAfter = 0, "", time.Time{}
		if played {
			h.LastPlayed = s.o.Now()
		}
	}
	if len(s.sourceHealth) >= 4096 {
		// Bound stale entries left by edited/deleted channel URLs.
		for k, v := range s.sourceHealth {
			if v.LastChecked.Before(s.o.Now().Add(-10 * time.Minute)) {
				delete(s.sourceHealth, k)
			}
		}
		if len(s.sourceHealth) >= 4096 {
			var oldest string
			var at time.Time
			for k, v := range s.sourceHealth {
				if oldest == "" || v.LastChecked.Before(at) {
					oldest, at = k, v.LastChecked
				}
			}
			delete(s.sourceHealth, oldest)
		}
	}
	s.sourceHealth[key] = h
	s.noteSourceOpen(channelID, src, err, played, h.RetryAfter)
	failures, retryAfter, checkedAt := h.Failures, h.RetryAfter, h.LastChecked
	s.sourceMu.Unlock()
	s.bumpHealthRevision()
	if err != nil {
		expiry := retryAfter
		if failures >= watchConsecutiveOpenFailures {
			if winEnd := checkedAt.Add(watchRecentFailureWindow); winEnd.After(expiry) {
				expiry = winEnd
			}
		}
		s.noteHealthExpiry(expiry)
		s.emitSourceCooldown(channelID, src.ID, failures, historyErrorClass(err), retryAfter)
	}
}

func isPluginStreamURL(raw string) bool {
	return strings.HasPrefix(strings.TrimSpace(raw), "plugin://")
}

// expandChannelForTune expands any lazy multi-candidate plugin:// inputs
// (such as event-based add-on channels) into ordered HTTP(S) StreamSources
// on this tune's local Channel copy, leaving the stored channel untouched.
func (s *Service) expandChannelForTune(ctx context.Context, c Channel) (Channel, *Programme, map[string]Programme, error) {
	r := s.kindRegistry()
	r.mu.RLock()
	expander := r.streamExpander
	r.mu.RUnlock()
	if expander == nil {
		return c, nil, nil, nil
	}
	if err := normalizeStreamSources(&c); err != nil {
		return c, nil, nil, nil
	}
	hasPlugin := false
	for _, src := range c.StreamSources {
		if src.Enabled && isPluginStreamURL(src.URL) {
			hasPlugin = true
			break
		}
	}
	if !hasPlugin {
		return c, nil, nil, nil
	}
	expanded := make([]StreamSource, 0, len(c.StreamSources))
	seenURL := map[string]bool{}
	var progByInput map[string]Programme
	var activeProg *Programme
	var lastErr error
	anyExpanded := false
	for _, src := range c.StreamSources {
		if !src.Enabled {
			expanded = append(expanded, src)
			continue
		}
		if !isPluginStreamURL(src.URL) {
			if !seenURL[src.URL] {
				seenURL[src.URL] = true
				expanded = append(expanded, src)
			}
			continue
		}
		subs, byID, prog, handled, err := expander(ctx, s, c, src)
		if !handled {
			expanded = append(expanded, src)
			continue
		}
		anyExpanded = true
		if err != nil {
			lastErr = err
			continue
		}
		if activeProg == nil && prog != nil {
			activeProg = prog
		}
		for _, sub := range subs {
			if len(expanded) >= MaxChannelInputs {
				break
			}
			if !sub.Enabled || sub.URL == "" || seenURL[sub.URL] {
				continue
			}
			seenURL[sub.URL] = true
			sub.Priority = len(expanded)
			expanded = append(expanded, sub)
			if byID != nil {
				if p, ok := byID[sub.ID]; ok {
					if progByInput == nil {
						progByInput = map[string]Programme{}
					}
					progByInput[sub.ID] = p
				}
			}
		}
	}
	if !anyExpanded {
		return c, nil, nil, nil
	}
	firstURL := ""
	for _, src := range expanded {
		if src.Enabled {
			firstURL = src.URL
			break
		}
	}
	if firstURL == "" {
		if lastErr != nil {
			return c, nil, nil, lastErr
		}
		return c, nil, nil, core.ErrNotAvailable
	}
	c.StreamSources = expanded
	c.StreamURL = firstURL
	return c, activeProg, progByInput, nil
}

// ResolveStreamSource resolves a channel's stream input at tune time. HTTP(S)
// URLs return directly; plugin:// inputs delegate to the installed stream
// opener under its Open deadline.
func (s *Service) ResolveStreamSource(ctx context.Context, c Channel, src StreamSource) (string, http.Header, error) {
	effSourceID := src.AuthSourceID
	if effSourceID == "" {
		effSourceID = c.SourceID
	}
	if !s.channelSourceEnabled(ctx, effSourceID) {
		return "", nil, core.ErrNotAvailable
	}
	headers, err := s.upstreamHeaders(ctx, c, src)
	if err != nil {
		return "", nil, err
	}
	if !strings.HasPrefix(src.URL, "plugin://") {
		return src.URL, headers, nil
	}
	r := s.kindRegistry()
	r.mu.RLock()
	opener := r.streamOpener
	r.mu.RUnlock()
	if opener == nil {
		return "", nil, core.ErrNotAvailable
	}
	resolved, extra, err := opener(ctx, c, src)
	if err != nil {
		return "", nil, err
	}
	if !validHTTP(resolved) {
		return "", nil, core.ErrNotAvailable
	}
	for k, vals := range extra {
		for _, v := range vals {
			headers.Add(k, v)
		}
	}
	return resolved, headers, nil
}

var errSilentInput = errors.New("live input has no audio")

func liveInputHasAudio(info *core.MediaInfo) bool {
	if info != nil {
		for _, st := range info.Streams {
			if st.Type == "audio" {
				return true
			}
		}
	}
	return false
}

// startIPTV selects one input per viewing session. Probing stays bounded so a
// dead primary fails fast enough to leave budget for backups; once an input
// answers its probe, post-probe stream startup receives its own bounded
// budget. It never substitutes another provider's bytes, keys or media
// sequence into the active HLS resource map.
func (s *Service) startIPTV(ctx context.Context, ss *session) error {
	c := ss.info.Channel
	ordered := c.StreamSources
	if !ss.ephemeral {
		if err := normalizeStreamSources(&c); err != nil {
			return err
		}
		ordered = s.orderedSourceCandidates(c)
	}
	var candidates []StreamSource
	var earliestRetry time.Time
	for _, src := range ordered {
		retry := s.sourceStatus(c.ID, src).RetryAfter
		if retry.After(s.o.Now()) {
			if earliestRetry.IsZero() || retry.Before(earliestRetry) {
				earliestRetry = retry
			}
			continue
		}
		candidates = append(candidates, src)
	}
	var last error = core.ErrNotAvailable
	var failures []TuneInputFailure
	if len(candidates) == 0 && !earliestRetry.IsZero() {
		return &TuneError{Code: "source_cooldown", Stage: "source_selection", RetryAfter: earliestRetry.Sub(s.o.Now()), cause: core.ErrNotAvailable}
	}
	remaining := iptvProbeBudget
	if deadline, ok := ctx.Deadline(); ok {
		remaining = min(remaining, max(time.Until(deadline), 0))
	}
	primaryCount := len(candidates)
	for index := 0; index < len(candidates); index++ {
		src := candidates[index]
		started := time.Now()
		log := s.sessionLog(ss).With("input", src.ID, "attempt", index+1, "candidates", len(candidates))
		log.Debug("live source attempt started")
		if err := ctx.Err(); err != nil {
			return err
		}
		// Share the remaining connect/sniff budget among untried inputs. Fast
		// failures leave their unused time for the next candidate.
		connectBudget := remaining / time.Duration(len(candidates)-index)
		resolveCtx, stopResolve := context.WithTimeout(ctx, connectBudget)
		resolveStarted := time.Now()
		resolvedURL, resolvedRawHdrs, headers, err := s.resolveSessionSource(resolveCtx, ss, c, src)
		resolveElapsed := time.Since(resolveStarted)
		stopResolve()
		if err != nil {
			remaining = max(remaining-min(resolveElapsed, connectBudget), 0)
			last = Diagnose("source_resolve", err)
			failures = append(failures, inputFailure(src.ID, last))
			log.Warn("live source resolve failed", append(Diagnose("source_resolve", err).LogAttrs(), "elapsed", time.Since(started))...)
			if !errors.Is(ctx.Err(), context.Canceled) {
				s.recordSourceHealth(c.ID, src, err, false)
			}
			continue
		}
		sniffBudget := max(connectBudget-resolveElapsed, 0)
		probeBudget := max(remaining-resolveElapsed, 0)
		if len(candidates) > 1 {
			probeBudget = min(max(sniffBudget, steadyProbeBudget-resolveElapsed), max(iptvProbeBudget-resolveElapsed, 0))
			if deadline, ok := ctx.Deadline(); ok {
				probeBudget = min(probeBudget, max(time.Until(deadline), 0))
			}
		}
		grantSrc := src
		grantSrc.URL = resolvedURL
		grantSrc.Headers = resolvedRawHdrs
		ss.inputURL, ss.inputSource = resolvedURL, grantSrc
		ss.inputHeaders = headers.Clone()
		ss.setSniffedManifest(nil)
		ss.info.MediaInfo, ss.relayInfo = nil, nil
		ss.startDeadline = time.Time{}
		// ffmpeg and ffprobe read this input through a loopback grant, never
		// with the provider URL or headers on their command line.
		var spent time.Duration
		err = s.withProviderCapacity(ctx, c, src, func(admitCtx context.Context) error {
			return s.grantInputForContext(admitCtx, ss, grantSrc, headers, true, s.latencyPolicy().TargetLatency())
		})
		// Provider pacing and slot contention have their own budgets.
		if err == nil {
			_, err = s.paceSessionInput(ctx, ss)
		}
		if err == nil {
			probeStarted := time.Now()
			err = s.startIPTVSource(ctx, ss, sniffBudget, probeBudget, primaryCount > 1 && index < primaryCount)
			spent = time.Since(probeStarted)
			if err == nil {
				err = s.awaitOwnedStart(ctx, ss)
			}
		}
		remaining = max(remaining-min(resolveElapsed+spent, connectBudget), 0)
		if err == nil {
			if ss.owned != nil && ss.owned.relay == nil {
				// The passthrough producer owns the input now. A viewer's
				// original probe must not pin it after producer failover.
				s.revokeInput(ss)
			}
			ss.info.ActiveSourceID = src.ID
			if serving := ss.servingProviderLocked(); serving != nil && serving.active.sourceID != "" {
				ss.info.ActiveSourceID = serving.active.sourceID
			}
			if ss.info.ActiveSourceID == src.ID {
				s.recordSourceHealth(c.ID, src, nil, false)
			}
			readyAttrs := []any{"elapsed", time.Since(started)}
			if index > 0 {
				readyAttrs = append(readyAttrs, "from", candidates[0].ID, "to", src.ID)
				readyAttrs = append(readyAttrs, s.duplicateFailoverLogAttrs(c, candidates[0], src)...)
			}
			log.Info("live source ready", readyAttrs...)
			return nil
		}
		ss.owned.close()
		ss.owned = nil
		s.revokeInput(ss)
		if errors.Is(err, errSilentInput) {
			// Retry silent inputs only after every original candidate. No health
			// penalty: they remain usable when all audible inputs are down.
			candidates = append(candidates, src)
			log.Info("live silent source deferred", "reason", "trying other candidates before silent fallback")
			continue
		}
		last = Diagnose("source_start", err)
		failures = append(failures, inputFailure(src.ID, last))
		if !errors.Is(ctx.Err(), context.Canceled) {
			s.recordSourceHealth(c.ID, src, err, false)
		}
		health := s.sourceStatus(c.ID, src)
		attrs := append(Diagnose("source_start", err).LogAttrs(), "elapsed", time.Since(started), "next_candidate", index+1 < len(candidates), "cooldown_until", health.RetryAfter)
		if errors.Is(err, context.Canceled) {
			log.Debug("live source attempt cancelled", attrs...)
		} else {
			log.Warn("live source attempt failed", attrs...)
		}
	}
	detail := *Diagnose("source_start", last)
	detail.Inputs = failures
	return &detail
}

func (s *Service) channelSourceEnabled(ctx context.Context, id string) bool {
	if id == "" {
		return true
	}
	src, err := s.storedSourceByID(ctx, id)
	return err == nil && !src.Disabled
}
