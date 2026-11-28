package resolver

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/gob"
	"encoding/json"
	"strings"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

// A detail view may label already configured choices, but must not run release
// ranking, namesake lookups or provider checks on the request goroutine. These
// snapshots are populated only when viewed, use no persistent writes, and share
// the resolver's cancellable background lifetime. Admission still revalidates
// every selected source through the normal resolver.
type detailVariantSnapshot struct {
	variants []core.ReleaseVariant
	until    time.Time
	pending  bool
}

type detailVariantTitleKey struct {
	Key       core.RatingKey
	ShowKey   core.RatingKey
	Runtime   time.Duration
	Language  string
	Countries []string
	Search    core.SearchRequest
}

type detailVariantUserKey struct {
	ID                int64
	Username          string
	IsOwner           bool
	AudioLanguages    []string
	PreferOriginal    bool
	SubtitleLanguages []string
}

type detailVariantCacheKey struct {
	Title          detailVariantTitleKey
	User           *detailVariantUserKey
	Client         core.ClientCaps
	PlayedRelease  string
	PlayedSelector string
	Prefer         string
	Exclude        []string
	Limit          int
	Slots          bool

	ClientDeliveryBudgetKbps int
}

func (s *Service) detailVariantKey(req core.ResolveRequest, limit int, slots bool) ([32]byte, error) {
	ck := detailVariantCacheKey{
		ClientDeliveryBudgetKbps: req.ClientDeliveryBudgetKbps,
		Title: detailVariantTitleKey{
			Key:       req.Title.Key,
			ShowKey:   req.Title.ShowKey,
			Runtime:   req.Title.Runtime,
			Language:  req.Title.Language,
			Countries: req.Title.Countries,
			Search:    searchRequestFor(req.Title),
		},
		Client:         req.Client,
		PlayedRelease:  req.PlayedRelease,
		PlayedSelector: req.PlayedSelector,
		Prefer:         req.Prefer,
		Exclude:        req.Exclude,
		Limit:          limit,
		Slots:          slots,
	}
	if req.User != nil {
		ck.User = &detailVariantUserKey{
			ID:                req.User.ID,
			Username:          req.User.Username,
			IsOwner:           req.User.IsOwner,
			AudioLanguages:    req.User.AudioLanguages,
			PreferOriginal:    req.User.PreferOriginal,
			SubtitleLanguages: req.User.SubtitleLanguages,
		}
	}
	var keyBuf bytes.Buffer
	if err := gob.NewEncoder(&keyBuf).Encode(ck); err != nil {
		return [32]byte{}, err
	}
	encoded, err := json.Marshal([]any{keyBuf.Bytes(), s.qualityCfg(), s.resolverCfg()})
	if err != nil {
		return [32]byte{}, err
	}
	return sha256.Sum256(encoded), nil
}

func (s *Service) detailVariantTTL(versions []core.ReleaseVariant) time.Duration {
	hasConcrete := false
	for _, v := range versions {
		if v.Release.ID != "" && !strings.HasPrefix(v.Release.ID, core.VersionSelectorPrefix) {
			hasConcrete = true
			break
		}
	}
	if !hasConcrete {
		return 15 * time.Second
	}
	ttl := 5 * time.Minute
	if sttl := s.searchTTL(); sttl > 0 && sttl < ttl {
		ttl = sttl
	}
	if ttl < 15*time.Second {
		ttl = 15 * time.Second
	}
	return ttl
}

func (s *Service) cacheDetailVariants(req core.ResolveRequest, limit int, slots bool, versions []core.ReleaseVariant) {
	if s == nil || req.Title == nil || len(versions) == 0 {
		return
	}
	key, err := s.detailVariantKey(req, limit, slots)
	if err != nil {
		return
	}
	now := s.now()
	ttl := s.detailVariantTTL(versions)
	s.bgMu.Lock()
	defer s.bgMu.Unlock()
	if s.bgClosed {
		return
	}
	if s.detailVariants == nil {
		s.detailVariants = make(map[[32]byte]*detailVariantSnapshot)
	}
	if _, exists := s.detailVariants[key]; !exists && len(s.detailVariants) >= 128 {
		for k, v := range s.detailVariants {
			if !v.pending && !now.Before(v.until) {
				delete(s.detailVariants, k)
			}
		}
		if len(s.detailVariants) >= 128 {
			return
		}
	}
	s.detailVariants[key] = &detailVariantSnapshot{
		variants: cloneDetailVariants(versions),
		until:    now.Add(ttl),
	}
}

func (s *Service) detailVariantRead(req core.ResolveRequest, limit int, slots bool) ([]core.ReleaseVariant, error) {
	if req.Title == nil {
		return nil, nil
	}
	key, err := s.detailVariantKey(req, limit, slots)
	if err != nil {
		return nil, err
	}
	now := s.now()
	s.bgMu.Lock()
	entry := s.detailVariants[key]
	if entry != nil && now.Before(entry.until) {
		result := cloneDetailVariants(entry.variants)
		s.bgMu.Unlock()
		return result, nil
	}
	var stale []core.ReleaseVariant
	if entry != nil && len(entry.variants) > 0 {
		stale = cloneDetailVariants(entry.variants)
	}
	if s.bgClosed || (entry != nil && entry.pending) || s.detailVariantWorkers >= 2 {
		s.bgMu.Unlock()
		return stale, nil
	}
	// Gob retains runtime-only identity facts (including alternative titles)
	// that JSON intentionally omits. Serialize once to own the request's slices
	// before starting a worker.
	var request bytes.Buffer
	if err := gob.NewEncoder(&request).Encode(req); err != nil {
		s.bgMu.Unlock()
		return stale, err
	}
	if s.detailVariants == nil {
		s.detailVariants = make(map[[32]byte]*detailVariantSnapshot)
	}
	if entry == nil && len(s.detailVariants) >= 128 {
		for k, v := range s.detailVariants {
			if !v.pending && !now.Before(v.until) {
				delete(s.detailVariants, k)
			}
		}
		if len(s.detailVariants) >= 128 {
			s.bgMu.Unlock()
			return stale, nil
		}
	}
	if entry == nil {
		entry = &detailVariantSnapshot{pending: true}
		s.detailVariants[key] = entry
	} else {
		entry.pending = true
	}
	s.detailVariantWorkers++
	s.bgWG.Add(1)
	s.bgMu.Unlock()
	var owned core.ResolveRequest
	if err := gob.NewDecoder(bytes.NewReader(request.Bytes())).Decode(&owned); err != nil {
		s.bgMu.Lock()
		entry.pending = false
		if len(entry.variants) == 0 {
			delete(s.detailVariants, key)
		}
		s.detailVariantWorkers--
		s.bgMu.Unlock()
		s.bgWG.Done()
		return stale, err
	}
	go func() {
		defer s.bgWG.Done()
		ctx, cancel := context.WithTimeout(s.bgCtx, 5*time.Second)
		defer cancel()
		var versions []core.ReleaseVariant
		var err error
		if slots {
			versions, err = s.versionSlots(ctx, owned, limit, true)
		} else {
			versions, err = s.variants(ctx, owned, limit)
		}
		ttl := s.detailVariantTTL(versions)
		s.bgMu.Lock()
		entry.pending = false
		s.detailVariantWorkers--
		if err == nil {
			entry.variants = cloneDetailVariants(versions)
			entry.until = s.now().Add(ttl)
		} else if len(entry.variants) == 0 {
			delete(s.detailVariants, key)
		}
		s.bgMu.Unlock()
	}()
	return stale, nil
}

func cloneDetailVariants(in []core.ReleaseVariant) []core.ReleaseVariant {
	if in == nil {
		return nil
	}
	out := append([]core.ReleaseVariant(nil), in...)
	for i := range out {
		out[i].Info = core.CloneMediaInfo(in[i].Info)
		// Release records are immutable cache entries; only presentation media
		// facts are modified by callers when overlaying a persisted probe.
	}
	return out
}

// RefreshDetailVariants marks completed detail-variant snapshots stale so the
// next read keeps concrete releases immediately (allowing callers to overlay
// freshly stored probes) while scheduling a background refresh.
func (s *Service) RefreshDetailVariants() {
	if s == nil {
		return
	}
	s.bgMu.Lock()
	for _, v := range s.detailVariants {
		if !v.pending {
			v.until = time.Time{}
		}
	}
	s.bgMu.Unlock()
}

// InvalidateDetailVariants drops completed cached detail-variant snapshots so
// policy, pin, viewer-choice, or search-cache updates appear on the next read.
func (s *Service) InvalidateDetailVariants() {
	if s == nil {
		return
	}
	s.bgMu.Lock()
	for k, v := range s.detailVariants {
		if !v.pending {
			delete(s.detailVariants, k)
		}
	}
	s.bgMu.Unlock()
}
