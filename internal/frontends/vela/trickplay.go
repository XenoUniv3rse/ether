package vela

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/braedonsaunders/ether/internal/frontend"
)

const (
	defaultTrickplayPollInterval    = time.Second
	defaultTrickplayPublishInterval = 10 * time.Second
)

type trickplayOfferKey struct {
	present         bool
	width, height   int
	intervalMs      int64
	count           int
	cols, rows      int
	coverageUntilMs int64
	revision        uint64
}

func trickplayKeyFromOffer(offer frontend.TrickplayOffer) trickplayOfferKey {
	intervalMs := offer.Interval.Milliseconds()
	if offer.Width <= 0 || offer.Height <= 0 || offer.Columns <= 0 || offer.Rows <= 0 || offer.Count <= 0 || intervalMs <= 0 {
		return trickplayOfferKey{}
	}
	coverageUntilMs := offer.CoverageUntil.Milliseconds()
	if coverageUntilMs < 0 {
		coverageUntilMs = 0
	}
	return trickplayOfferKey{
		present:         true,
		width:           offer.Width,
		height:          offer.Height,
		intervalMs:      intervalMs,
		count:           offer.Count,
		cols:            offer.Columns,
		rows:            offer.Rows,
		coverageUntilMs: coverageUntilMs,
		revision:        offer.Revision,
	}
}

func (k trickplayOfferKey) wire() object {
	return object{
		"width":           k.width,
		"height":          k.height,
		"intervalMs":      k.intervalMs,
		"count":           k.count,
		"cols":            k.cols,
		"rows":            k.rows,
		"coverageUntilMs": k.coverageUntilMs,
		"revision":        k.revision,
	}
}

func (i *instance) trickplayPollEvery() time.Duration {
	if i.trickplayPollInterval > 0 {
		return i.trickplayPollInterval
	}
	return defaultTrickplayPollInterval
}

func (i *instance) trickplayPublishThrottle() time.Duration {
	if i.trickplayPublishInterval > 0 {
		return i.trickplayPublishInterval
	}
	return defaultTrickplayPublishInterval
}

func (s *playSession) isTrickplayEligibleLocked() bool {
	if s == nil || !s.ended.IsZero() || s.channelID != "" || s.plan == nil {
		return false
	}
	if s.plan["purpose"] == "preview" || s.plan["media"] == "audio" {
		return false
	}
	if _, extra := s.plan["extraId"]; extra {
		return false
	}
	if _, live := s.plan["live"]; live {
		return false
	}
	return true
}

func (s *playSession) applyTrickplayLocked(key trickplayOfferKey) {
	s.trickplayCurrent = key
	if key.present {
		s.plan["trickplay"] = key.wire()
		return
	}
	delete(s.plan, "trickplay")
}

func (s *playSession) resetTrickplayForSourceLocked() {
	delete(s.plan, "trickplay")
	s.trickplayCurrent = trickplayOfferKey{}
	s.trickplayPublished = trickplayOfferKey{}
	s.trickplayLastPublish = time.Time{}
	s.trickplayGenerating = false
}

func (s *playSession) shouldPublishTrickplayLocked(wasGenerating, generating bool, now time.Time, throttle time.Duration) bool {
	if s.trickplayCurrent == s.trickplayPublished {
		return false
	}
	if s.trickplayCurrent.present != s.trickplayPublished.present {
		return true
	}
	if wasGenerating && !generating {
		return true
	}
	return s.trickplayLastPublish.IsZero() || now.Sub(s.trickplayLastPublish) >= throttle
}

// queryTrickplay inspects the session's current trickplay metadata without
// starting generation. ok is true when the broker returned a definitive
// present or absent state, and false on transient/non-NotFound errors.
func (i *instance) queryTrickplay(ctx context.Context, access frontend.SessionAccess, sessionID string) (trickplayOfferKey, bool, bool) {
	tp, ok := i.host.Sessions.(frontend.SessionTrickplay)
	if !ok {
		return trickplayOfferKey{}, false, false
	}
	offer, err := tp.SessionTrickplayOffer(ctx, access, sessionID)
	if err != nil {
		if errors.Is(err, frontend.ErrNotFound) {
			return trickplayOfferKey{}, false, true
		}
		return trickplayOfferKey{}, false, false
	}
	return trickplayKeyFromOffer(offer), offer.Generating, true
}

func (i *instance) refreshInitialTrickplay(ctx context.Context, access frontend.SessionAccess, sessionID string, s *playSession) {
	if !s.isTrickplayEligibleLocked() {
		return
	}
	key, generating, ok := i.queryTrickplay(ctx, access, sessionID)
	if !ok {
		return
	}
	s.trickplayGenerating = generating
	s.applyTrickplayLocked(key)
	if key.present {
		s.trickplayPublished = key
		s.trickplayLastPublish = time.Now()
	}
}

func (i *instance) refreshSessionTrickplay(ctx context.Context, access frontend.SessionAccess, sessionID string, s *playSession) {
	s.mu.Lock()
	if !s.isTrickplayEligibleLocked() {
		s.mu.Unlock()
		return
	}
	source := s.source
	s.mu.Unlock()

	key, generating, ok := i.queryTrickplay(ctx, access, sessionID)
	if !ok {
		return
	}

	s.mu.Lock()
	if s.source == source && s.isTrickplayEligibleLocked() {
		s.trickplayGenerating = generating
		s.applyTrickplayLocked(key)
	}
	s.mu.Unlock()
}

func (i *instance) pollSessionTrickplay(ctx context.Context, viewer *frontend.Viewer, device frontend.Device, sessionID string, s *playSession) {
	s.mu.Lock()
	if !s.ended.IsZero() || s.commandActive || !s.isTrickplayEligibleLocked() {
		s.mu.Unlock()
		return
	}
	source, seek, cursor := s.source, s.seek, s.cursor
	accessViewer, accessDevice := s.accessViewer, s.accessDevice
	if accessViewer == nil {
		accessViewer = viewer
	}
	if accessDevice.ID == "" {
		accessDevice = device
	}
	s.mu.Unlock()

	key, generating, ok := i.queryTrickplay(ctx, frontend.SessionAccess{Viewer: accessViewer, Device: accessDevice, Protocol: "vela"}, sessionID)
	if !ok {
		return
	}

	s.mu.Lock()
	if !s.ended.IsZero() || s.commandActive || s.source != source || s.seek != seek || s.cursor != cursor || !s.isTrickplayEligibleLocked() {
		s.mu.Unlock()
		return
	}
	wasGenerating := s.trickplayGenerating
	s.trickplayGenerating = generating
	s.applyTrickplayLocked(key)
	if s.shouldPublishTrickplayLocked(wasGenerating, generating, time.Now(), i.trickplayPublishThrottle()) {
		s.publish("plan")
	}
	s.mu.Unlock()
}

func (i *instance) trickplaySheet(w http.ResponseWriter, r *http.Request) {
	v, s := i.session(w, r)
	if s == nil {
		return
	}
	width, ok := parsePositiveDecimalInt(r.PathValue("width"))
	if !ok {
		problem(w, http.StatusNotFound, "not-found", "trickplay_unavailable")
		return
	}
	rawSheet, ok := strings.CutSuffix(r.PathValue("sheet"), ".jpg")
	if !ok {
		problem(w, http.StatusNotFound, "not-found", "trickplay_unavailable")
		return
	}
	index, ok := parseNonNegativeDecimalInt(rawSheet)
	if !ok {
		problem(w, http.StatusNotFound, "not-found", "trickplay_unavailable")
		return
	}
	s.mu.Lock()
	eligible := s.isTrickplayEligibleLocked()
	sourceGen := s.source
	s.mu.Unlock()
	if !eligible {
		problem(w, http.StatusNotFound, "not-found", "trickplay_unavailable")
		return
	}
	tp, ok := i.host.Sessions.(frontend.SessionTrickplay)
	if !ok {
		problem(w, http.StatusNotFound, "not-found", "trickplay_unavailable")
		return
	}
	id := r.PathValue("id")
	inm := r.Header.Values("If-None-Match")
	var etag string
	res, err := tp.SessionTrickplaySheet(r.Context(), i.access(r, v), id, width, index, func(offer frontend.TrickplayOffer) bool {
		etag = trickplayETag(id, sourceGen, offer.Revision, offer.Width, index)
		return etagHeaderMatches(inm, etag)
	})
	if err != nil {
		if errors.Is(err, frontend.ErrNotFound) {
			problem(w, http.StatusNotFound, "not-found", "trickplay_unavailable")
			return
		}
		i.failFor(w, v, err, r)
		return
	}
	s.mu.Lock()
	stillCurrent := s.isTrickplayEligibleLocked() && s.source == sourceGen
	s.mu.Unlock()
	if !stillCurrent {
		problem(w, http.StatusNotFound, "not-found", "trickplay_unavailable")
		return
	}
	if etag == "" {
		etag = trickplayETag(id, sourceGen, res.Offer.Revision, res.Offer.Width, index)
	}
	w.Header().Set("Cache-Control", "private, no-cache")
	w.Header().Set("ETag", etag)
	w.Header().Set("Vary", "Authorization, X-Vela-Device-Id")
	if res.Fallback {
		w.Header().Set("X-Ether-Trickplay-Fallback", "true")
	} else {
		w.Header().Set("X-Ether-Trickplay-Fallback", "false")
	}
	if res.NotModified {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.Header().Set("Content-Type", "image/jpeg")
	w.Header().Set("Content-Length", strconv.Itoa(len(res.Body)))
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodHead {
		return
	}
	_, _ = w.Write(res.Body)
}

func trickplayETag(sessionID string, sourceGen, revision uint64, width, sheet int) string {
	return fmt.Sprintf(`"trickplay-%s-%d-%d-%d-%d"`, hash(sessionID)[:16], sourceGen, revision, width, sheet)
}

func etagHeaderMatches(values []string, tag string) bool {
	for _, header := range values {
		for _, candidate := range strings.Split(header, ",") {
			trimmed := strings.TrimSpace(candidate)
			if trimmed == "*" || trimmed == tag || strings.TrimPrefix(trimmed, "W/") == tag {
				return true
			}
		}
	}
	return false
}

func parsePositiveDecimalInt(raw string) (int, bool) {
	if len(raw) == 0 || raw[0] < '1' || raw[0] > '9' {
		return 0, false
	}
	for idx := 1; idx < len(raw); idx++ {
		if raw[idx] < '0' || raw[idx] > '9' {
			return 0, false
		}
	}
	v, err := strconv.Atoi(raw)
	return v, err == nil && v > 0
}

func parseNonNegativeDecimalInt(raw string) (int, bool) {
	if raw == "0" {
		return 0, true
	}
	return parsePositiveDecimalInt(raw)
}
