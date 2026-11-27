package playback

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/langtag"
	"github.com/braedonsaunders/ether/internal/nzb"
	"github.com/braedonsaunders/ether/internal/releaseparse"
	"github.com/braedonsaunders/ether/internal/store"
	"github.com/braedonsaunders/ether/internal/transcode"
	"github.com/braedonsaunders/ether/internal/vault"
)

func init() {
	// Durable cross-viewer, cross-indexer container probe cache keyed by
	// release content identity (W116 nzb:<sha256>, bt:<infoHash>, pre:...).
	store.Register("playback_release_probe", 1, `CREATE TABLE release_probes (
	content_key TEXT PRIMARY KEY,
	info_json TEXT NOT NULL,
	updated_at BIGINT NOT NULL
);
CREATE INDEX release_probes_updated ON release_probes(updated_at);`)
}

const (
	maxReleaseProbeCache   = 8192
	versionProbeCooldown   = 5 * time.Minute
	versionProbePerTimeout = 10 * time.Second
)

type versionProbeState struct {
	running bool
	doneAt  time.Time
}

func releaseProbeContentKeys(rel core.Release, rawPayload []byte, contentIDOf func(string) string) []string {
	if rel.Info.FullSeason && !rel.HasFileOrEpisodeScope() {
		return nil
	}
	var out []string
	add := func(k string) {
		if k == "" {
			return
		}
		for _, existing := range out {
			if existing == k {
				return
			}
		}
		out = append(out, k)
	}
	if len(rawPayload) > 0 && !bytes.HasPrefix(rawPayload, []byte(debridPayloadPrefix)) {
		if cid := nzb.ContentID(rawPayload); cid != "" {
			add(rel.ScopedContentID(cid))
		}
	}
	if vk := vault.KeyOf(rel); vk != "" {
		add(rel.ScopedContentID(string(vk)))
	}
	if contentIDOf != nil && rel.ID != "" {
		if cid := contentIDOf(rel.ID); cid != "" {
			// resolver.ContentIDForRelease already returns a scoped key for
			// episode-targeted packs (hex TargetReleaseID); only scope raw
			// unscoped nzb:/pre: keys so episode keys are never double-hashed.
			if strings.HasPrefix(cid, nzb.ContentIDPrefix) || strings.HasPrefix(cid, core.PreFetchContentIDPrefix) {
				add(rel.ScopedContentID(cid))
			} else {
				add(cid)
			}
		}
	}
	for _, k := range rel.ProbeContentKeys() {
		add(k)
	}
	return out
}

func vaultTrackTarget(rel core.Release, rawPayload []byte, contentIDOf func(string) string) (vault.Key, int, int) {
	if rel.HasExplicitFileTarget() || (rel.Info.FullSeason && !rel.HasFileOrEpisodeScope()) {
		return "", 0, 0
	}
	var vk vault.Key
	switch {
	case vault.KeyOf(rel) != "":
		vk = vault.KeyOf(rel)
	case strings.HasPrefix(rel.ContentID, nzb.ContentIDPrefix):
		vk = vault.Key(rel.ContentID)
	case len(rawPayload) > 0 && !bytes.HasPrefix(rawPayload, []byte(debridPayloadPrefix)):
		if cid := nzb.ContentID(rawPayload); cid != "" {
			vk = vault.Key(cid)
		}
	case strings.TrimSpace(rel.InfoHash) != "":
		vk = vault.TorrentKey(strings.ToLower(strings.TrimSpace(rel.InfoHash)))
	case contentIDOf != nil && rel.ID != "":
		if cid := contentIDOf(rel.ID); strings.HasPrefix(cid, nzb.ContentIDPrefix) {
			vk = vault.Key(cid)
		}
	}
	if vk == "" {
		return "", 0, 0
	}
	if season, episode, ok := rel.EpisodeScope(); ok {
		return vk, season, episode
	}
	return vk, 0, 0
}

func (m *Manager) preloadReleaseProbes() {
	if m.deps.DB == nil {
		return
	}
	rows, err := m.deps.DB.Query(`SELECT content_key, info_json FROM release_probes ORDER BY updated_at DESC LIMIT ?`, maxReleaseProbeCache+1)
	if err != nil {
		return
	}
	defer rows.Close()
	loaded := make(map[string]core.MediaInfo)
	count := 0
	for rows.Next() {
		var key, raw string
		if err := rows.Scan(&key, &raw); err != nil {
			continue
		}
		count++
		if count > maxReleaseProbeCache {
			break
		}
		if len(raw) > maxPersistedProbeBytes {
			continue
		}
		if info, ok := decodePersistedProbe(raw); ok {
			if facts, ok := persistedProbeFacts(info); ok {
				loaded[key] = facts
			}
		}
	}
	if rows.Err() != nil {
		return
	}
	m.releaseProbeMu.Lock()
	m.releaseProbes = loaded
	m.releaseProbesLoaded = count <= maxReleaseProbeCache
	m.releaseProbeMu.Unlock()
}

// noteReleaseProbeMemory updates the in-memory content-ID probe cache without
// performing any disk or database I/O (safe on the play path).
func (m *Manager) noteReleaseProbeMemory(rel core.Release, rawPayload []byte, info core.MediaInfo) (core.MediaInfo, []string, bool) {
	facts, ok := persistedProbeFacts(info)
	if !ok {
		return core.MediaInfo{}, nil, false
	}
	keys := releaseProbeContentKeys(rel, rawPayload, m.deps.ContentIDOf)
	if len(keys) == 0 {
		return core.MediaInfo{}, nil, false
	}
	m.releaseProbeMu.Lock()
	if m.releaseProbes == nil {
		m.releaseProbes = make(map[string]core.MediaInfo)
	}
	if m.releaseProbeKeys == nil {
		m.releaseProbeKeys = make(map[string]string)
	}
	for _, k := range keys {
		if _, exists := m.releaseProbes[k]; !exists && len(m.releaseProbes) >= maxReleaseProbeCache {
			m.releaseProbesLoaded = false
			continue
		}
		m.releaseProbes[k] = core.CloneMediaInfo(facts)
	}
	if rel.ID != "" && len(m.releaseProbeKeys) < maxReleaseProbeCache {
		if aliasID := rel.ScopedContentID(rel.ID); aliasID != "" {
			m.releaseProbeKeys[aliasID] = keys[0]
		}
	}
	m.releaseProbeMu.Unlock()
	return facts, keys, true
}

// persistReleaseProbe writes sanitized probe facts durably to release_probes
// and, when vaulted, to vault.TrackStore.
func (m *Manager) persistReleaseProbe(ctx context.Context, rel core.Release, rawPayload []byte, info core.MediaInfo) {
	facts, keys, ok := m.noteReleaseProbeMemory(rel, rawPayload, info)
	if !ok {
		return
	}
	if m.deps.DB != nil {
		b, err := json.Marshal(probeFactsRecord{FactsVersion: core.ProbeFactsVersion, Info: facts})
		if err == nil && len(b) <= maxPersistedProbeBytes {
			now := m.now().UnixMilli()
			dbCtx := context.WithoutCancel(ctx)
			for _, k := range keys {
				_, _ = m.deps.DB.ExecContext(dbCtx, `INSERT INTO release_probes(content_key, info_json, updated_at) VALUES(?, ?, ?)
ON CONFLICT(content_key) DO UPDATE SET info_json=excluded.info_json, updated_at=excluded.updated_at`,
					k, string(b), now)
			}
		}
	}
	if ts, ok := m.deps.Vault.(vault.TrackStore); ok {
		if vk, season, episode := vaultTrackTarget(rel, rawPayload, m.deps.ContentIDOf); vk != "" {
			_ = ts.SetTracks(context.WithoutCancel(ctx), vk, season, episode, facts)
		}
	}
}

// RecordReleaseProbe caches probed container facts in memory and durably by
// content identity (and into the NZB vault's TrackStore when vaulted).
func (m *Manager) RecordReleaseProbe(ctx context.Context, rel core.Release, rawPayload []byte, info core.MediaInfo) {
	m.persistReleaseProbe(ctx, rel, rawPayload, info)
	if m.deps.OnVersionProbed != nil {
		m.deps.OnVersionProbed(0, rel)
	}
}

// CachedProbeByKeys returns probed MediaInfo for the first matching durable
// release_probes content key, consulting the preloaded in-memory cache first
// and falling back to SQLite only when the table exceeded maxReleaseProbeCache.
func (m *Manager) CachedProbeByKeys(keys []string) (core.MediaInfo, bool) {
	if m == nil || len(keys) == 0 {
		return core.MediaInfo{}, false
	}
	m.releaseProbeMu.RLock()
	for _, k := range keys {
		if k == "" {
			continue
		}
		if info, ok := m.releaseProbes[k]; ok {
			m.releaseProbeMu.RUnlock()
			return core.CloneMediaInfo(info), true
		}
	}
	complete := m.releaseProbesLoaded
	m.releaseProbeMu.RUnlock()
	if !complete && m.deps.DB != nil {
		for _, k := range keys {
			if k == "" {
				continue
			}
			var raw string
			if err := m.deps.DB.QueryRow(`SELECT info_json FROM release_probes WHERE content_key=?`, k).Scan(&raw); err == nil && len(raw) <= maxPersistedProbeBytes {
				if info, ok := decodePersistedProbe(raw); ok {
					if facts, ok := persistedProbeFacts(info); ok {
						return facts, true
					}
				}
			}
		}
	}
	return core.MediaInfo{}, false
}

// RecordProbeByKeys caches probed container facts in memory and durably in
// release_probes under each non-empty key in keys.
func (m *Manager) RecordProbeByKeys(ctx context.Context, keys []string, info core.MediaInfo) {
	if m == nil || len(keys) == 0 {
		return
	}
	facts, ok := persistedProbeFacts(info)
	if !ok {
		return
	}
	var uniq []string
	for _, k := range keys {
		if k == "" {
			continue
		}
		dup := false
		for _, existing := range uniq {
			if existing == k {
				dup = true
				break
			}
		}
		if !dup {
			uniq = append(uniq, k)
		}
	}
	if len(uniq) == 0 {
		return
	}
	m.releaseProbeMu.Lock()
	if m.releaseProbes == nil {
		m.releaseProbes = make(map[string]core.MediaInfo)
	}
	for _, k := range uniq {
		if _, exists := m.releaseProbes[k]; !exists && len(m.releaseProbes) >= maxReleaseProbeCache {
			m.releaseProbesLoaded = false
			continue
		}
		m.releaseProbes[k] = core.CloneMediaInfo(facts)
	}
	m.releaseProbeMu.Unlock()
	if m.deps.DB != nil {
		b, err := json.Marshal(probeFactsRecord{FactsVersion: core.ProbeFactsVersion, Info: facts})
		if err == nil && len(b) <= maxPersistedProbeBytes {
			now := m.now().UnixMilli()
			dbCtx := context.WithoutCancel(ctx)
			for _, k := range uniq {
				_, _ = m.deps.DB.ExecContext(dbCtx, `INSERT INTO release_probes(content_key, info_json, updated_at) VALUES(?, ?, ?)
ON CONFLICT(content_key) DO UPDATE SET info_json=excluded.info_json, updated_at=excluded.updated_at`,
					k, string(b), now)
			}
		}
	}
}

func itemProbeContentKey(key core.RatingKey) string {
	if key == 0 {
		return ""
	}
	return "item:" + key.String()
}

// noteItemProbeMemory caches probed container facts for a plugin item in
// memory without disk or database I/O.
func (m *Manager) noteItemProbeMemory(key core.RatingKey, info core.MediaInfo) (core.MediaInfo, string, bool) {
	if m == nil || !key.IsPluginKey() {
		return core.MediaInfo{}, "", false
	}
	facts, ok := persistedProbeFacts(info)
	if !ok {
		return core.MediaInfo{}, "", false
	}
	ck := itemProbeContentKey(key)
	m.releaseProbeMu.Lock()
	if m.releaseProbes == nil {
		m.releaseProbes = make(map[string]core.MediaInfo)
	}
	if _, exists := m.releaseProbes[ck]; exists || len(m.releaseProbes) < maxReleaseProbeCache {
		m.releaseProbes[ck] = core.CloneMediaInfo(facts)
	} else {
		m.releaseProbesLoaded = false
	}
	m.releaseProbeMu.Unlock()
	return facts, ck, true
}

// persistItemProbe durably stores probed container facts (including streams
// and chapters) for a plugin item in release_probes so detail pages can list
// probed chapters even across restarts without re-probing or calling /resolve.
func (m *Manager) persistItemProbe(ctx context.Context, key core.RatingKey, info core.MediaInfo) {
	facts, ck, ok := m.noteItemProbeMemory(key, info)
	if !ok || m.deps.DB == nil {
		return
	}
	b, err := json.Marshal(probeFactsRecord{FactsVersion: core.ProbeFactsVersion, Info: facts})
	if err != nil || len(b) > maxPersistedProbeBytes {
		return
	}
	now := m.now().UnixMilli()
	_, _ = m.deps.DB.ExecContext(context.WithoutCancel(ctx), `INSERT INTO release_probes(content_key, info_json, updated_at) VALUES(?, ?, ?)
ON CONFLICT(content_key) DO UPDATE SET info_json=excluded.info_json, updated_at=excluded.updated_at`,
		ck, string(b), now)
}

// CachedItemProbe returns probed MediaInfo (including Chapters and Streams)
// for a plugin item from live sessions, the preloaded in-memory release_probes
// cache, or release_probes on disk without any network or plugin /resolve I/O.
func (m *Manager) CachedItemProbe(key core.RatingKey) (core.MediaInfo, bool) {
	if m == nil || !key.IsPluginKey() {
		return core.MediaInfo{}, false
	}
	var liveInfo core.MediaInfo
	var liveFound bool
	var liveAt time.Time
	m.mu.Lock()
	var matching []*session
	for _, s := range m.sessions {
		if s != nil && !s.closed && s.ident.key == key {
			matching = append(matching, s)
		}
	}
	m.mu.Unlock()
	for _, s := range matching {
		s.mu.Lock()
		g, closed, started := s.cur, s.closed, s.startedAt
		s.mu.Unlock()
		if g == nil || closed {
			continue
		}
		if facts, ok := persistedProbeFacts(g.infoSnapshot()); ok {
			if !liveFound || len(facts.Chapters) > len(liveInfo.Chapters) || (len(facts.Chapters) == len(liveInfo.Chapters) && started.After(liveAt)) {
				liveInfo = facts
				liveFound = true
				liveAt = started
			}
		}
	}
	if liveFound && len(liveInfo.Chapters) > 0 {
		return core.CloneMediaInfo(liveInfo), true
	}

	ck := itemProbeContentKey(key)
	m.releaseProbeMu.RLock()
	if info, ok := m.releaseProbes[ck]; ok {
		m.releaseProbeMu.RUnlock()
		if !liveFound || len(info.Chapters) > len(liveInfo.Chapters) {
			return core.CloneMediaInfo(info), true
		}
		return core.CloneMediaInfo(liveInfo), true
	}
	complete := m.releaseProbesLoaded
	m.releaseProbeMu.RUnlock()

	if !complete && m.deps.DB != nil {
		var raw string
		if err := m.deps.DB.QueryRow(`SELECT info_json FROM release_probes WHERE content_key=?`, ck).Scan(&raw); err == nil && len(raw) <= maxPersistedProbeBytes {
			if info, ok := decodePersistedProbe(raw); ok {
				if facts, ok := persistedProbeFacts(info); ok {
					if !liveFound || len(facts.Chapters) > len(liveInfo.Chapters) {
						return facts, true
					}
				}
			}
		}
	}
	if liveFound {
		return core.CloneMediaInfo(liveInfo), true
	}
	return core.MediaInfo{}, false
}

// CachedReleaseProbe returns probed MediaInfo for rel from in-memory caches
// (durable release_probes preload and vault TrackStore) with zero request-path
// database or network I/O.
func (m *Manager) CachedReleaseProbe(rel core.Release) (core.MediaInfo, bool) {
	if m == nil {
		return core.MediaInfo{}, false
	}
	keys := releaseProbeContentKeys(rel, nil, m.deps.ContentIDOf)
	m.releaseProbeMu.RLock()
	for _, k := range keys {
		if info, ok := m.releaseProbes[k]; ok {
			m.releaseProbeMu.RUnlock()
			return core.CloneMediaInfo(info), true
		}
	}
	if rel.ID != "" && !(rel.Info.FullSeason && !rel.HasFileOrEpisodeScope()) {
		if aliasKey := m.releaseProbeKeys[rel.ScopedContentID(rel.ID)]; aliasKey != "" {
			if info, ok := m.releaseProbes[aliasKey]; ok {
				m.releaseProbeMu.RUnlock()
				return core.CloneMediaInfo(info), true
			}
		}
	}
	complete := m.releaseProbesLoaded
	m.releaseProbeMu.RUnlock()

	if ts, ok := m.deps.Vault.(vault.TrackStore); ok {
		if vk, season, episode := vaultTrackTarget(rel, nil, m.deps.ContentIDOf); vk != "" {
			if info, hit, err := ts.Tracks(context.Background(), vk, season, episode); err == nil && hit {
				return core.CloneMediaInfo(info), true
			}
		}
	}
	if !complete && m.deps.DB != nil && len(keys) > 0 {
		for _, k := range keys {
			var raw string
			if err := m.deps.DB.QueryRow(`SELECT info_json FROM release_probes WHERE content_key=?`, k).Scan(&raw); err == nil && len(raw) <= maxPersistedProbeBytes {
				if info, ok := decodePersistedProbe(raw); ok {
					if facts, ok := persistedProbeFacts(info); ok {
						return facts, true
					}
				}
			}
		}
	}
	return core.MediaInfo{}, false
}

// enrichEstimatedFromDebridMeta uses the selected debrid file's name and size
// to refine an estimate before a container probe is available.
func enrichEstimatedFromDebridMeta(v *core.ReleaseVariant, title *core.Title, filePath string, size int64) {
	if v == nil || !v.Info.Estimated {
		return
	}
	if size > 0 {
		v.Info.FileSize = size
		if v.Info.Size <= 0 {
			v.Info.Size = size
		}
	}
	if filePath == "" {
		return
	}
	parsed := releaseparse.Parse(filePath)
	if container, codec := core.AudioFileHint(filePath); container != "" && isAudioOnlyTitle(title) {
		v.Info.Container = container
		if size > 0 {
			v.Info.Size = size
			if v.Info.Duration > 0 {
				v.Info.BitrateKbps = int(float64(size) * 8 / v.Info.Duration.Seconds() / 1000)
			}
		}
		if codec == "" {
			codec = parsed.AudioCodec
		}
		if codec != "" {
			// Format evidence from the actual selected file outranks a release
			// name or a client-based estimate. Other stream facts remain unknown.
			v.Info.Streams = []core.StreamInfo{{Type: "audio", Codec: codec}}
		}
	}
	var extra []string
	for _, lang := range parsed.Languages {
		if lang == "" || lang == releaseparse.LanguageMulti || lang == releaseparse.LanguageDual {
			continue
		}
		code := langtag.CanonicalBCP47(lang)
		if code == "" {
			continue
		}
		dup := false
		for _, e := range extra {
			if e == code {
				dup = true
				break
			}
		}
		if !dup {
			extra = append(extra, code)
		}
	}
	if len(extra) == 0 {
		return
	}
	existing := map[string]bool{}
	var template core.StreamInfo
	hasTemplate := false
	maxIdx := -1
	unknownIdx := -1
	for i, st := range v.Info.Streams {
		if st.Index > maxIdx {
			maxIdx = st.Index
		}
		if st.Type != "audio" {
			continue
		}
		if !hasTemplate {
			template = st
			hasTemplate = true
		}
		if st.Language == "" {
			if unknownIdx < 0 {
				unknownIdx = i
			}
			continue
		}
		if c := langtag.CanonicalBCP47(releaseparse.LanguageName(st.Language)); c != "" {
			existing[c] = true
		}
		existing[strings.ToLower(st.Language)] = true
	}
	for _, iso := range extra {
		if existing[iso] {
			continue
		}
		existing[iso] = true
		if unknownIdx >= 0 {
			v.Info.Streams[unknownIdx].Language = iso
			unknownIdx = -1
			continue
		}
		maxIdx++
		st := core.StreamInfo{
			Index:    maxIdx,
			Type:     "audio",
			Language: iso,
		}
		if hasTemplate {
			st.Codec = template.Codec
			st.Channels = template.Channels
			st.ChannelLayout = template.ChannelLayout
			st.Profile = template.Profile
			st.BitrateKbps = template.BitrateKbps
		} else if parsed.AudioCodec != "" {
			st.Codec = parsed.AudioCodec
		}
		v.Info.Streams = append(v.Info.Streams, st)
	}
}

// BackgroundBusy reports how many background version-probe runs are active.
func (m *Manager) BackgroundBusy() int {
	if m == nil {
		return 0
	}
	m.versionProbeMu.Lock()
	defer m.versionProbeMu.Unlock()
	n := 0
	for _, st := range m.versionProbes {
		if st.running {
			n++
		}
	}
	return n
}

func (m *Manager) isBackgroundBusy() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.closed || m.draining || m.hasActivePlaybackLocked()
}

func (m *Manager) cancelVersionProbes() {
	m.versionProbeMu.Lock()
	cancel := m.versionProbeCancel
	m.versionProbeMu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func (m *Manager) closeVersionProbes() {
	m.versionProbeMu.Lock()
	m.versionProbeClosed = true
	cancel := m.versionProbeCancel
	m.versionProbeMu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// scheduleVersionProbes launches a single low-priority, bounded container
// header probe pass for a title's Play Version menu entries when a viewer opens
// the title's details page. It never searches indexers, never grabs NZBs, runs
// at most one probe at a time per title (and globally), and yields immediately
// to active playback.
func (m *Manager) scheduleVersionProbes(user *core.User, key core.RatingKey, caps core.ClientCaps) {
	if m == nil || user == nil || key == 0 {
		return
	}
	if m.deps.OpenVaulted == nil && m.deps.OpenCachedDebrid == nil {
		return
	}
	if m.deps.Catalog == nil || m.deps.Resolver == nil {
		return
	}
	if _, ok := m.deps.Resolver.(core.VariantResolver); !ok {
		return
	}
	cfg := m.config().Cache
	if cfg.DisableSpeculation || cfg.Mode == "off" || cfg.DetailWarmup == "off" {
		return
	}
	if m.isBackgroundBusy() {
		return
	}

	now := m.now()
	m.versionProbeMu.Lock()
	if m.versionProbeClosed {
		m.versionProbeMu.Unlock()
		return
	}
	if m.versionProbeSem == nil {
		m.versionProbeSem = make(chan struct{}, 1)
	}
	if m.versionProbes == nil {
		m.versionProbes = make(map[core.RatingKey]*versionProbeState)
	}
	st := m.versionProbes[key]
	if st != nil && (st.running || (!st.doneAt.IsZero() && now.Sub(st.doneAt) < versionProbeCooldown)) {
		m.versionProbeMu.Unlock()
		return
	}
	if st == nil {
		st = &versionProbeState{}
		m.versionProbes[key] = st
	}
	st.running = true
	m.versionProbeWG.Add(1)
	m.versionProbeMu.Unlock()

	u := *user
	go func() {
		defer m.versionProbeWG.Done()
		completed := false
		defer func() {
			m.versionProbeMu.Lock()
			if cur := m.versionProbes[key]; cur != nil {
				cur.running = false
				if completed {
					cur.doneAt = m.now()
				}
			}
			m.versionProbeMu.Unlock()
		}()
		completed = m.runVersionProbes(&u, key, caps)
	}()
}

type versionSlotResolver interface {
	VersionSlots(context.Context, core.ResolveRequest, int, bool) ([]core.ReleaseVariant, error)
}

type versionSlotWatcher interface {
	WatchVersionSlots(context.Context, core.ResolveRequest, int, <-chan struct{}, func([]core.ReleaseVariant)) error
}

func (m *Manager) versionProbeRequest(ctx context.Context, user *core.User, title *core.Title, caps core.ClientCaps) core.ResolveRequest {
	req := core.ResolveRequest{
		Title:  title,
		User:   user,
		Client: caps,
	}
	if _, rel, sel, ok := m.ResumedVersion(ctx, user, title.Key, caps); ok {
		req.PlayedRelease = rel.ID
		req.PlayedSelector = sel
		relCopy := rel
		req.PlayedReleaseRecord = &relCopy
	}
	return req
}

func (m *Manager) offeredVariantsForProbing(ctx context.Context, user *core.User, title *core.Title, caps core.ClientCaps) []core.ReleaseVariant {
	req := m.versionProbeRequest(ctx, user, title, caps)
	var variants []core.ReleaseVariant
	var err error
	if slots, ok := m.deps.Resolver.(versionSlotResolver); ok {
		variants, err = slots.VersionSlots(ctx, req, core.MaxVariants, true)
	} else if vr, ok := m.deps.Resolver.(core.VariantResolver); ok {
		variants, err = vr.Variants(ctx, req, core.MaxVariants)
	} else {
		return nil
	}
	if err != nil || len(variants) == 0 {
		return nil
	}
	if len(variants) > core.MaxVariants {
		variants = variants[:core.MaxVariants]
	}
	return variants
}

var _ core.VersionHeaderProber = (*Manager)(nil)

// ProbeVersionHeaders runs bounded, parallel container header probes on any
// unprobed non-Auto offered versions of key that can be opened without indexer
// searches or NZB grabs (already-cached debrid torrents, direct HTTP streams,
// or vaulted Usenet NZBs). When the resolver supports live slot observation,
// each version's header probe starts as soon as its slot fills from the search
// cache or arriving search batches instead of waiting for the full indexer
// fleet to finish (#678, #715).
func (m *Manager) ProbeVersionHeaders(ctx context.Context, user *core.User, key core.RatingKey, caps core.ClientCaps) error {
	if m == nil || user == nil || key == 0 {
		return nil
	}
	if m.deps.OpenVaulted == nil && m.deps.OpenCachedDebrid == nil {
		return nil
	}
	if m.deps.Catalog == nil || m.deps.Resolver == nil {
		return nil
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	title, err := m.deps.Catalog.Get(core.WithLocalResponse(ctx), key)
	if err != nil || title == nil {
		return nil
	}

	budget := m.deps.HeaderProbeBudget
	if budget <= 0 {
		budget = transcode.DefaultHeaderProbeBudget
	}
	sem := make(chan struct{}, 4)
	var (
		wg             sync.WaitGroup
		mu             sync.Mutex
		inFlight       = make(map[string]bool)
		inFlightCached = make(map[string]bool)
		retryCached    = make(map[string]core.Release)
		triedUncached  = make(map[string]bool)
		done           = make(map[string]bool)
	)
	schedule := func(variants []core.ReleaseVariant) {
		if ctx.Err() != nil {
			return
		}
		if len(variants) > core.MaxVariants {
			variants = variants[:core.MaxVariants]
		}
		for _, v := range variants {
			// Auto is resolved and probed by ProbePlayMetadata (or already exact);
			// skipping it prevents duplicate work when Auto and version probes run
			// concurrently.
			if v.Auto {
				continue
			}
			// Scope each version to this title's episode so pack episodes never
			// share one probe (#714).
			rel := probeReleaseForTitle(v.Release, title)
			if rel.ID == "" || strings.HasPrefix(rel.ID, core.VersionSelectorPrefix) {
				continue
			}
			id := rel.ScopedContentID(rel.ID)
			knownCached := v.Availability == core.VersionCached ||
				v.Availability == core.VersionInstant ||
				len(rel.DebridCached) > 0

			mu.Lock()
			if done[id] {
				mu.Unlock()
				continue
			}
			if inFlight[id] {
				if knownCached && !inFlightCached[id] && rel.Protocol == core.ProtocolTorrent {
					retryCached[id] = rel
				}
				mu.Unlock()
				continue
			}
			if !knownCached && triedUncached[id] {
				mu.Unlock()
				continue
			}
			mu.Unlock()

			if _, hit := m.CachedReleaseProbe(rel); hit {
				mu.Lock()
				done[id] = true
				mu.Unlock()
				continue
			}
			if _, hit := m.CachedVariantInfo(ctx, user.ID, key, rel.ID); hit {
				mu.Lock()
				done[id] = true
				mu.Unlock()
				continue
			}

			mu.Lock()
			if done[id] {
				mu.Unlock()
				continue
			}
			if inFlight[id] {
				if knownCached && !inFlightCached[id] && rel.Protocol == core.ProtocolTorrent {
					retryCached[id] = rel
				}
				mu.Unlock()
				continue
			}
			if !knownCached && triedUncached[id] {
				mu.Unlock()
				continue
			}
			inFlight[id] = true
			inFlightCached[id] = knownCached
			mu.Unlock()

			wg.Add(1)
			go func(r core.Release, id string, cached bool) {
				defer wg.Done()
				acquired := false
				select {
				case sem <- struct{}{}:
					acquired = true
					defer func() { <-sem }()
				case <-ctx.Done():
				}
				if !acquired || ctx.Err() != nil {
					mu.Lock()
					delete(inFlight, id)
					delete(inFlightCached, id)
					delete(retryCached, id)
					mu.Unlock()
					return
				}
				for {
					probed := m.probeOneVersionHeaderWithYield(ctx, key, r, budget, m.isShuttingDown)
					mu.Lock()
					if !probed && ctx.Err() == nil {
						if r2, ok := retryCached[id]; ok {
							delete(retryCached, id)
							inFlightCached[id] = true
							r = r2
							cached = true
							mu.Unlock()
							continue
						}
					}
					delete(inFlight, id)
					delete(inFlightCached, id)
					delete(retryCached, id)
					if probed || cached || r.Protocol != core.ProtocolTorrent {
						done[id] = true
					} else {
						triedUncached[id] = true
					}
					mu.Unlock()
					return
				}
			}(rel, id, knownCached)
		}
	}

	if watcher, ok := m.deps.Resolver.(versionSlotWatcher); ok {
		req := m.versionProbeRequest(ctx, user, title, caps)
		_ = watcher.WatchVersionSlots(
			core.WithoutLocalResponse(ctx),
			req,
			core.MaxVariants,
			core.VersionProbeSearchWait(ctx),
			schedule,
		)
	} else {
		schedule(m.offeredVariantsForProbing(core.WithoutLocalResponse(ctx), user, title, caps))
	}
	wg.Wait()
	return ctx.Err()
}

func (m *Manager) isShuttingDown() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.closed || m.draining
}

func (m *Manager) runVersionProbes(user *core.User, key core.RatingKey, caps core.ClientCaps) bool {
	// Acquire the single global background header-probe slot, yielding if
	// playback starts or the manager closes while waiting.
	select {
	case m.versionProbeSem <- struct{}{}:
		defer func() { <-m.versionProbeSem }()
	case <-m.ctx.Done():
		return false
	default:
		// Another title's header probe is already running; do not queue a
		// backlog of background probes.
		return false
	}
	if m.isBackgroundBusy() {
		return false
	}

	runCtx, cancel := context.WithCancel(m.ctx)
	m.versionProbeMu.Lock()
	m.versionProbeCancel = cancel
	m.versionProbeMu.Unlock()
	defer func() {
		cancel()
		m.versionProbeMu.Lock()
		m.versionProbeCancel = nil
		m.versionProbeMu.Unlock()
	}()

	// Check once more after publishing cancel so a concurrent reserveLocked
	// cannot race past us.
	if m.isBackgroundBusy() {
		return false
	}

	localCtx := core.WithLocalResponse(runCtx)
	title, err := m.deps.Catalog.Get(localCtx, key)
	if err != nil || title == nil {
		return false
	}
	variants := m.offeredVariantsForProbing(runCtx, user, title, caps)
	if len(variants) == 0 {
		return false
	}

	budget := m.deps.HeaderProbeBudget
	if budget <= 0 {
		budget = transcode.DefaultHeaderProbeBudget
	}

	for _, v := range variants {
		if runCtx.Err() != nil || m.isBackgroundBusy() {
			return false
		}
		rel := probeReleaseForTitle(v.Release, title)
		if rel.ID == "" || strings.HasPrefix(rel.ID, core.VersionSelectorPrefix) {
			continue
		}
		if _, hit := m.CachedReleaseProbe(rel); hit {
			continue
		}
		if _, hit := m.CachedVariantInfo(runCtx, user.ID, key, rel.ID); hit {
			continue
		}
		if !m.probeOneVersionHeader(runCtx, key, rel, budget) {
			if runCtx.Err() != nil || m.isBackgroundBusy() {
				return false
			}
		}
	}
	return true
}

func (m *Manager) probeOneVersionHeader(ctx context.Context, key core.RatingKey, rel core.Release, budget int) bool {
	return m.probeOneVersionHeaderWithYield(ctx, key, rel, budget, m.isBackgroundBusy)
}

func (m *Manager) probeOneVersionHeaderWithYield(ctx context.Context, key core.RatingKey, rel core.Release, budget int, yield func() bool) bool {
	bgCtx := ctx
	if !core.IsBackgroundCheck(bgCtx) {
		bgCtx = core.WithBackgroundBudget(bgCtx, func(context.Context, string, string, int64) error { return nil })
	}
	probeCtx, cancel := context.WithTimeout(bgCtx, versionProbePerTimeout)
	defer cancel()

	var (
		src        core.MediaSource
		rawPayload []byte
		err        error
	)
	switch rel.Protocol {
	case core.ProtocolTorrent, core.ProtocolHTTP:
		if m.deps.OpenCachedDebrid == nil {
			return false
		}
		src, err = m.deps.OpenCachedDebrid(probeCtx, rel)
	case core.ProtocolUsenet:
		if m.deps.OpenVaulted == nil {
			return false
		}
		src, rawPayload, err = m.deps.OpenVaulted(probeCtx, rel)
	default:
		return false
	}
	if err != nil || src == nil {
		return false
	}
	defer func() { _ = src.Close() }()

	if probeCtx.Err() != nil || (yield != nil && yield()) {
		return false
	}
	size := src.Size()
	if size <= 0 {
		return false
	}
	reader := &budgetedHeaderReader{
		src:   src,
		yield: yield,
	}
	reader.remaining.Store(int64(budget))

	info, err := transcode.ProbeHeader(probeCtx, reader, size, budget)
	if err != nil || info == nil || probeCtx.Err() != nil || (yield != nil && yield()) {
		return false
	}
	mi := core.CloneMediaInfo(*info)
	if mi.Size <= 0 {
		mi.Size = size
	}
	if mi.Container == "" {
		mi.Container = containerFromName(src.Name())
	}
	mi.Estimated = false

	m.persistReleaseProbe(ctx, rel, rawPayload, mi)
	if m.deps.OnVersionProbed != nil {
		m.deps.OnVersionProbed(key, rel)
	}
	return true
}

type prefetchReaderAt interface {
	ReadAtPrefetchContext(ctx context.Context, p []byte, off int64) (int, error)
}

type metadataReaderAt interface {
	ReadMetadataAtContext(ctx context.Context, p []byte, off int64) (int, error)
}

type budgetedHeaderReader struct {
	src       core.MediaSource
	remaining atomic.Int64
	yield     func() bool
}

func (r *budgetedHeaderReader) ReadMetadataAtContext(ctx context.Context, p []byte, off int64) (int, error) {
	if ctx.Err() != nil {
		return 0, ctx.Err()
	}
	if r.yield != nil && r.yield() {
		return 0, context.Canceled
	}
	if r.remaining.Add(-int64(len(p))) < 0 {
		return 0, transcode.ErrProbeIncomplete
	}
	var (
		n   int
		err error
	)
	if pr, ok := r.src.(prefetchReaderAt); ok {
		n, err = pr.ReadAtPrefetchContext(ctx, p, off)
	} else if mr, ok := r.src.(metadataReaderAt); ok {
		n, err = mr.ReadMetadataAtContext(ctx, p, off)
	} else {
		n, err = r.src.ReadAt(p, off)
	}
	if r.yield != nil && r.yield() {
		return 0, context.Canceled
	}
	if err != nil && !errors.Is(err, context.Canceled) {
		return n, err
	}
	return n, err
}

func (r *budgetedHeaderReader) ReadAt(p []byte, off int64) (int, error) {
	return r.ReadMetadataAtContext(context.Background(), p, off)
}
