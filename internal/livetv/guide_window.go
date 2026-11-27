package livetv

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
)

const (
	MaxGuideWindowChannels   = frontend.MaxLiveGuideWindowChannels
	MaxGuideWindowDuration   = time.Duration(frontend.MaxLiveGuideWindowMinutes) * time.Minute
	MaxGuideWindowProgrammes = frontend.MaxLiveGuideWindowProgrammes
)

type GuideWindowState = frontend.LiveGuideWindowState

const (
	GuideWindowReady       = frontend.LiveGuideWindowReady
	GuideWindowUnavailable = frontend.LiveGuideWindowUnavailable
)

type GuideWindowChannel struct {
	ArchiveDays int
	ChannelID   string
	State       GuideWindowState
	Programmes  []Programme
}

type GuideWindow struct {
	From, Until time.Time
	ValidUntil  time.Time
	Channels    []GuideWindowChannel
}

type guideWindowSourceInfo struct {
	exists              bool
	failed              bool
	hasAnyGuideChannels bool
	src                 Source
}

type guideWindowChannelMeta struct {
	hasGuideChannel bool
	maxEnd          sql.NullInt64
}

func authEntryFromSource(src Source, now time.Time) authEntry {
	if src.Disabled {
		return authEntry{at: now, disabled: true}
	}
	e := authEntry{
		name:          src.Name,
		playlistHost:  providerHost(src.PlaylistURL),
		auth:          src.Auth,
		at:            now,
		perHour:       max(src.TunesPerHour, 0),
		unratedRating: src.UnratedRating,
		adultGroups:   map[string]bool{},
	}
	for _, g := range src.Groups {
		if g.Adult {
			e.adultGroups[normGroup(g.Name)] = true
		}
	}
	if src.LoginPool || len(src.Logins) > 0 {
		e.pool = &Source{
			ID:          src.ID,
			LoginPool:   src.LoginPool,
			Auth:        src.Auth,
			PlaylistURL: src.PlaylistURL,
			Account:     src.Account,
			Logins:      src.Logins,
		}
	}
	if src.Account != nil {
		e.limit = src.Account.MaxConnections
	}
	if src.MaxConnections > 0 && (e.limit == 0 || src.MaxConnections < e.limit) {
		e.limit = src.MaxConnections
	}
	if src.LoginPool || len(src.Logins) > 0 {
		e.limit = loginLimit(src.Account)
		for _, l := range src.Logins {
			if l.Error == "" {
				e.limit += loginLimit(l.Account)
			}
		}
	}
	switch {
	case src.TuneGapSeconds > 0:
		e.gap = time.Duration(src.TuneGapSeconds) * time.Second
	case src.TuneGapSeconds == 0 && e.limit > 0:
		e.gap = defaultTuneGap
	}
	return e
}

func guideSourceFailed(src Source) bool {
	if src.Disabled || src.LastError != "" || src.RefreshFailures > 0 {
		return true
	}
	if src.LastErrorAt != nil && !src.LastErrorAt.Before(src.UpdatedAt) {
		return true
	}
	return false
}

func stepAfter(anchor, now time.Time, step time.Duration) time.Time {
	anchor = anchor.UTC()
	now = now.UTC()
	if step <= 0 {
		return now.Add(time.Minute)
	}
	if anchor.After(now) {
		return anchor
	}
	steps := now.Sub(anchor)/step + 1
	return anchor.Add(steps * step)
}

func newTooDenseError(channels int, dur time.Duration, total int) *frontend.LiveGuideWindowTooDenseError {
	if total <= MaxGuideWindowProgrammes {
		total = MaxGuideWindowProgrammes + 1
	}
	maxCh := max(1, min(MaxGuideWindowChannels, (channels*MaxGuideWindowProgrammes)/total))
	maxMin := max(1, min(frontend.MaxLiveGuideWindowMinutes, int((dur*MaxGuideWindowProgrammes)/(time.Duration(total)*time.Minute))))
	return &frontend.LiveGuideWindowTooDenseError{
		MaxChannels:      maxCh,
		MaxWindowMinutes: maxMin,
	}
}

func normalizeWindowProgrammes(ps []Programme) []Programme {
	if len(ps) == 0 {
		return []Programme{}
	}
	sort.Slice(ps, func(i, j int) bool {
		if !ps[i].Start.Equal(ps[j].Start) {
			return ps[i].Start.Before(ps[j].Start)
		}
		return ps[i].ID < ps[j].ID
	})
	out := ps[:0]
	for _, p := range ps {
		if len(out) > 0 && out[len(out)-1].ID == p.ID {
			continue
		}
		out = append(out, p)
	}
	return out
}

func (s *Service) allowedWindowProgramme(ctx context.Context, u *core.User, c Channel, p *Programme, titleMemo map[core.RatingKey]*core.Title) bool {
	if p != nil && p.policyTag != nil {
		tag := p.policyTag
		var lookup func(context.Context, core.RatingKey) (*core.Title, error)
		if tag.catalog {
			lookup = func(context.Context, core.RatingKey) (*core.Title, error) { return tag.title, nil }
		}
		return s.allowedWithLookup(ctx, u, c, p, lookup, false)
	}
	var lookup func(context.Context, core.RatingKey) (*core.Title, error)
	if s.o.Catalog != nil {
		base := s.o.Catalog.Get
		if c.Personal != nil {
			if cached, ok := s.o.Catalog.(interface {
				CachedGet(context.Context, core.RatingKey) (*core.Title, error)
			}); ok {
				base = cached.CachedGet
			}
		}
		lookup = func(ctx context.Context, key core.RatingKey) (*core.Title, error) {
			if titleMemo != nil {
				if t, ok := titleMemo[key]; ok {
					return t, nil
				}
			}
			t, err := base(ctx, key)
			if err == nil && titleMemo != nil && t != nil {
				titleMemo[key] = t
			}
			return t, err
		}
	}
	return s.allowedWithLookup(ctx, u, c, p, lookup, false)
}

// GuideWindowForChannels returns complete overlap coverage of [from, until) for
// up to 128 unique channels in exact request order, using bounded indexed reads.
// Hidden, unknown, unmapped, or failed-source channels are reported as
// "unavailable" with an empty programme list and never reveal listings.
func (s *Service) GuideWindowForChannels(ctx context.Context, u *core.User, from, until time.Time, ids []string) (GuideWindow, error) {
	if !until.After(from) || until.Sub(from) > MaxGuideWindowDuration {
		return GuideWindow{}, fmt.Errorf("guide window must be positive and at most %s", MaxGuideWindowDuration)
	}
	if len(ids) == 0 || len(ids) > MaxGuideWindowChannels {
		return GuideWindow{}, fmt.Errorf("guide window requires 1..%d channels", MaxGuideWindowChannels)
	}
	seenIDs := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		if id == "" {
			return GuideWindow{}, fmt.Errorf("empty channel id")
		}
		if _, dup := seenIDs[id]; dup {
			return GuideWindow{}, fmt.Errorf("duplicate channel id %q", id)
		}
		seenIDs[id] = struct{}{}
	}

	now := time.Now().UTC()
	if s != nil && s.o.Now != nil {
		now = s.o.Now().UTC()
	}

	out := GuideWindow{
		From:     from,
		Until:    until,
		Channels: make([]GuideWindowChannel, len(ids)),
	}
	for i, id := range ids {
		out.Channels[i] = GuideWindowChannel{
			ChannelID:  id,
			State:      GuideWindowUnavailable,
			Programmes: []Programme{},
		}
	}
	if s == nil || !s.Enabled() || u == nil {
		out.ValidUntil = now.Add(60 * time.Second).Truncate(time.Second)
		return out, nil
	}

	var pol Policy
	var err error
	ctx, pol, err = s.withPolicy(ctx, u)
	if err != nil {
		return GuideWindow{}, err
	}

	selected, err := s.channelsByIDs(ctx, ids)
	if err != nil {
		return GuideWindow{}, err
	}

	present := make([]Channel, 0, len(selected))
	presentIdx := make([]int, 0, len(selected))
	for i, id := range ids {
		if c, ok := selected[id]; ok {
			present = append(present, c)
			presentIdx = append(presentIdx, i)
		}
	}

	sources, err := s.loadGuideWindowSources(ctx, present, now)
	if err != nil {
		return GuideWindow{}, err
	}

	var virtualChannels []Channel
	var virtualPositions []int
	var iptvChannels []Channel
	var iptvPositions []int
	for k, c := range present {
		if !s.audienceAllowed(ctx, u, c) {
			continue
		}
		reqIdx := presentIdx[k]
		if c.Kind == "virtual" {
			virtualChannels = append(virtualChannels, c)
			virtualPositions = append(virtualPositions, reqIdx)
		} else {
			iptvChannels = append(iptvChannels, c)
			iptvPositions = append(iptvPositions, reqIdx)
		}
	}

	if len(virtualChannels) > 0 {
		if err := s.warmGuideWindowVirtual(ctx, virtualChannels, now); err != nil {
			return GuideWindow{}, err
		}
	}

	titleMemo := make(map[core.RatingKey]*core.Title)

	rated := strings.TrimSpace(u.MaxContentRating) != "" || strings.TrimSpace(pol.MaxRating) != ""
	if rated {
		if len(iptvChannels) > 0 {
			airing, err := s.programmesAt(ctx, iptvChannels, now)
			if err != nil {
				return GuideWindow{}, err
			}
			keptC := iptvChannels[:0]
			keptP := iptvPositions[:0]
			for i, c := range iptvChannels {
				var current *Programme
				if p, ok := airing[i]; ok {
					p.ChannelID = c.ID
					p.ID = fmt.Sprintf("%s-%d", c.ID, p.Start.UnixMilli())
					current = &p
				}
				if s.allowedWindowProgramme(ctx, u, c, current, titleMemo) {
					keptC = append(keptC, c)
					keptP = append(keptP, iptvPositions[i])
				}
			}
			iptvChannels = keptC
			iptvPositions = keptP
		}
		if len(virtualChannels) > 0 {
			keptC := virtualChannels[:0]
			keptP := virtualPositions[:0]
			for i, c := range virtualChannels {
				pc := s.personalChannel(ctx, c, u)
				if pc.Personal == nil {
					s.virtualGuide.mu.RLock()
					entry := s.virtualGuide.entries[c.ID]
					s.virtualGuide.mu.RUnlock()
					if entry == nil || now.Before(entry.from) || now.Add(time.Second).After(entry.until) {
						continue
					}
				}
				var current *Programme
				if ps := s.viewerSchedule(ctx, pc, now, now.Add(time.Second)); len(ps) > 0 {
					current = &ps[0]
				}
				if s.allowedWindowProgramme(ctx, u, pc, current, titleMemo) {
					keptC = append(keptC, c)
					keptP = append(keptP, virtualPositions[i])
				}
			}
			virtualChannels = keptC
			virtualPositions = keptP
		}
	}
	for n, c := range iptvChannels {
		out.Channels[iptvPositions[n]].ArchiveDays = c.ArchiveDays()
	}

	var earliestNext time.Time
	noteNext := func(candidate time.Time) {
		if candidate.IsZero() {
			return
		}
		if earliestNext.IsZero() || candidate.Before(earliestNext) {
			earliestNext = candidate
		}
	}

	totalProgrammes := 0
	dur := until.Sub(from)

	for i, c := range virtualChannels {
		reqIdx := virtualPositions[i]
		pc := s.personalChannel(ctx, c, u)
		var raw []Programme
		var nextChange time.Time
		if pc.Personal != nil {
			ps, err := s.personalProgrammes(ctx, pc, u, from, until)
			if err != nil {
				if ctx.Err() != nil {
					return GuideWindow{}, ctx.Err()
				}
				continue
			}
			raw = ps
			anchor := time.Unix(0, 0).UTC()
			s.virtualGuide.mu.RLock()
			if entry := s.virtualGuide.entries[c.ID]; entry != nil && !entry.built.IsZero() {
				anchor = entry.built
			}
			s.virtualGuide.mu.RUnlock()
			nextChange = stepAfter(anchor, now, virtualGuideIdleWindow)
		} else {
			s.virtualGuide.mu.RLock()
			entry := s.virtualGuide.entries[c.ID]
			s.virtualGuide.mu.RUnlock()
			if entry == nil || from.Before(entry.from) || until.After(entry.until) {
				if s.serviceContext() != nil {
					s.noteVirtualGuideDemand(false)
				} else {
					s.wakeVirtualGuide()
				}
				continue
			}
			raw = s.virtualProgrammes(c, from, until, "guide")
			nextChange = stepAfter(entry.built, now, time.Hour)
		}
		filtered := make([]Programme, 0, len(raw))
		for _, p := range raw {
			if !p.End.After(from) || !p.Start.Before(until) {
				continue
			}
			if s.allowedWindowProgramme(ctx, u, pc, &p, titleMemo) {
				filtered = append(filtered, p)
			}
		}
		filtered = normalizeWindowProgrammes(filtered)
		totalProgrammes += len(filtered)
		if totalProgrammes > MaxGuideWindowProgrammes {
			return GuideWindow{}, newTooDenseError(len(ids), dur, totalProgrammes)
		}
		out.Channels[reqIdx] = GuideWindowChannel{
			ChannelID:  c.ID,
			State:      GuideWindowReady,
			Programmes: filtered,
		}
		noteNext(nextChange)
	}

	type channelSlot struct {
		reqIdx int
		ch     Channel
	}
	byKey := make(map[guideKey][]channelSlot, len(iptvChannels))
	var keyOrder []guideKey
	for i, c := range iptvChannels {
		srcID := guideSource(c)
		if c.GuideID == "" || srcID == "" {
			continue
		}
		k := guideKey{source: srcID, guide: c.GuideID}
		if _, ok := byKey[k]; !ok {
			keyOrder = append(keyOrder, k)
		}
		byKey[k] = append(byKey[k], channelSlot{reqIdx: iptvPositions[i], ch: c})
	}

	if len(keyOrder) > 0 {
		meta, err := s.loadGuideWindowChannelMeta(ctx, keyOrder)
		if err != nil {
			return GuideWindow{}, err
		}

		readyKeys := make([]guideKey, 0, len(keyOrder))
		readySet := make(map[guideKey]bool, len(keyOrder))
		horizons := make(map[string]int64)
		uncovered := make(map[string]bool)
		readySources := make(map[string]bool)

		for _, k := range keyOrder {
			srcInfo := sources[k.source]
			if srcInfo.exists && srcInfo.failed {
				continue
			}
			chMeta := meta[k]
			if !chMeta.hasGuideChannel && (srcInfo.exists || srcInfo.hasAnyGuideChannels || !chMeta.maxEnd.Valid) {
				continue
			}
			readyKeys = append(readyKeys, k)
			readySet[k] = true
			readySources[k.source] = true
			if !chMeta.maxEnd.Valid || chMeta.maxEnd.Int64 <= now.UnixMilli() {
				uncovered[k.source] = true
			}
			if srcInfo.exists && chMeta.maxEnd.Valid && chMeta.maxEnd.Int64 > srcInfo.src.UpdatedAt.UnixMilli() {
				if prev, ok := horizons[k.source]; !ok || chMeta.maxEnd.Int64 < prev {
					horizons[k.source] = chMeta.maxEnd.Int64
				}
			}
		}

		for srcID := range readySources {
			var horizon *time.Time
			if h, ok := horizons[srcID]; ok {
				horizon = msTime(h)
			}
			noteNext(s.guideSourceNextChange(sources[srcID], horizon, uncovered[srcID], now))
		}

		if len(readyKeys) > 0 {
			type keyProgramme struct {
				key guideKey
				p   Programme
			}
			perChannel := make(map[int][]Programme, len(iptvChannels))
			fromMS := from.UnixMilli()
			untilMS := until.UnixMilli()
			if until.UnixNano()%int64(time.Millisecond) != 0 && until.UnixNano() > 0 {
				untilMS++
			}
			rawDelivered := 0
			for start := 0; start < len(readyKeys); start += guideBatch {
				end := min(start+guideBatch, len(readyKeys))
				marks := make([]string, 0, end-start)
				args := make([]any, 0, (end-start)*2+2)
				for _, k := range readyKeys[start:end] {
					marks = append(marks, "(?,?)")
					args = append(args, k.source, k.guide)
				}
				args = append(args, fromMS, untilMS)
				rows, err := s.o.DB.QueryContext(ctx, `SELECT source_id, channel_id, data FROM livetv_programmes WHERE (source_id,channel_id) IN (`+strings.Join(marks, ",")+`) AND end_ms>? AND start_ms<?`, args...)
				if err != nil {
					return GuideWindow{}, err
				}
				var scanned []keyProgramme
				for rows.Next() {
					var k guideKey
					var b []byte
					if err = rows.Scan(&k.source, &k.guide, &b); err != nil {
						rows.Close()
						return GuideWindow{}, err
					}
					if !readySet[k] {
						continue
					}
					slots := byKey[k]
					if rawDelivered+len(scanned) > MaxGuideWindowProgrammes*4 {
						extra := len(slots)
						for extra < 100000 && rows.Next() {
							extra += max(1, len(slots))
						}
						rows.Close()
						return GuideWindow{}, newTooDenseError(len(ids), dur, totalProgrammes+rawDelivered+len(scanned)+extra)
					}
					var p Programme
					if err = json.Unmarshal(b, &p); err != nil {
						rows.Close()
						return GuideWindow{}, err
					}
					if !p.End.After(from) || !p.Start.Before(until) {
						continue
					}
					scanned = append(scanned, keyProgramme{key: k, p: p})
				}
				if err = rows.Err(); err != nil {
					rows.Close()
					return GuideWindow{}, err
				}
				rows.Close()

				for _, entry := range scanned {
					for _, slot := range byKey[entry.key] {
						q := entry.p
						q.ChannelID = slot.ch.ID
						q.ID = fmt.Sprintf("%s-%d", slot.ch.ID, q.Start.UnixMilli())
						if s.allowedWindowProgramme(ctx, u, slot.ch, &q, titleMemo) {
							perChannel[slot.reqIdx] = append(perChannel[slot.reqIdx], q)
							rawDelivered++
						}
					}
				}
			}

			for _, k := range readyKeys {
				for _, slot := range byKey[k] {
					ps := normalizeWindowProgrammes(perChannel[slot.reqIdx])
					totalProgrammes += len(ps)
					out.Channels[slot.reqIdx] = GuideWindowChannel{
						ChannelID:   slot.ch.ID,
						ArchiveDays: slot.ch.ArchiveDays(),
						State:       GuideWindowReady,
						Programmes:  ps,
					}
				}
			}
			if totalProgrammes > MaxGuideWindowProgrammes {
				return GuideWindow{}, newTooDenseError(len(ids), dur, totalProgrammes)
			}
		}
	}

	hasUnavailable := false
	for _, ch := range out.Channels {
		if ch.State != GuideWindowReady {
			hasUnavailable = true
			break
		}
	}
	validUntil := earliestNext
	if hasUnavailable {
		retryCap := now.Add(60 * time.Second).Truncate(time.Second)
		if validUntil.IsZero() || retryCap.Before(validUntil) {
			validUntil = retryCap
		}
	}
	if validUntil.IsZero() {
		validUntil = stepAfter(time.Unix(0, 0).UTC(), now, defaultRefreshInterval)
	}
	out.ValidUntil = validUntil.UTC().Truncate(time.Second)
	return out, nil
}

func (s *Service) guideSourceNextChange(info guideWindowSourceInfo, horizon *time.Time, uncovered bool, now time.Time) time.Time {
	if !info.exists {
		return stepAfter(time.Unix(0, 0).UTC(), now, defaultRefreshInterval)
	}
	src := info.src
	if next, ok := nextRefresh(src, horizon); ok {
		if s.sourceGuideFeatures(src).Remote && (src.LastErrorAt == nil || !src.LastErrorAt.After(src.UpdatedAt)) && uncovered {
			if earlier := src.UpdatedAt.Add(minRefreshInterval); earlier.Before(next) {
				next = earlier
			}
		}
		if src.ActivityRefreshMinutes > 0 {
			cooldown := time.Duration(max(30, src.ActivityRefreshMinutes)) * time.Minute
			if act := src.UpdatedAt.Add(cooldown); act.Before(next) {
				next = act
			}
		}
		if !next.After(now) {
			return stepAfter(next, now, time.Minute)
		}
		return next.UTC()
	}
	anchor := src.UpdatedAt
	if anchor.IsZero() {
		anchor = time.Unix(0, 0).UTC()
	}
	return stepAfter(anchor, now, defaultRefreshInterval)
}

func (s *Service) loadGuideWindowSources(ctx context.Context, cs []Channel, now time.Time) (map[string]guideWindowSourceInfo, error) {
	seen := make(map[string]bool, len(cs))
	var ids []string
	add := func(id string) {
		if id != "" && !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
	}
	for _, c := range cs {
		add(c.SourceID)
		add(guideSource(c))
		for _, stream := range c.StreamSources {
			add(stream.AuthSourceID)
		}
	}
	out := make(map[string]guideWindowSourceInfo, len(ids))
	if len(ids) == 0 {
		return out, nil
	}

	s.authCache.mu.Lock()
	authGen := s.authCache.gen
	s.authCache.mu.Unlock()

	for start := 0; start < len(ids); start += channelPageChunk {
		end := min(start+channelPageChunk, len(ids))
		batch := ids[start:end]
		values := make([]string, len(batch))
		args := make([]any, len(batch))
		for i, id := range batch {
			values[i] = "(?)"
			args[i] = id
		}
		rows, err := s.o.DB.QueryContext(ctx, `WITH src(id) AS (VALUES `+strings.Join(values, ",")+`)
SELECT src.id, s.data, r.updated_ms, EXISTS(SELECT 1 FROM livetv_guide_channels g WHERE g.source_id=src.id)
FROM src
LEFT JOIN livetv_sources s ON s.id=src.id
LEFT JOIN livetv_source_refresh r ON r.id=src.id`, args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var id string
			var raw []byte
			var updatedMS sql.NullInt64
			var hasGuide bool
			if err = rows.Scan(&id, &raw, &updatedMS, &hasGuide); err != nil {
				rows.Close()
				return nil, err
			}
			info := guideWindowSourceInfo{hasAnyGuideChannels: hasGuide}
			var entry authEntry
			if raw != nil {
				src, _, derr := s.decodeSource(raw)
				if derr != nil {
					rows.Close()
					return nil, derr
				}
				if src.ID == "" {
					src.ID = id
				}
				if updatedMS.Valid && updatedMS.Int64 > src.UpdatedAt.UnixMilli() {
					src.UpdatedAt = time.UnixMilli(updatedMS.Int64).UTC()
				}
				info.exists = true
				info.src = src
				info.failed = guideSourceFailed(src)
				entry = authEntryFromSource(src, now)
			} else {
				entry = authEntry{at: now, adultGroups: map[string]bool{}}
			}
			out[id] = info
			s.authCache.mu.Lock()
			if s.authCache.gen == authGen {
				if cur, exists := s.authCache.m[id]; !exists || now.Sub(cur.at) >= authCacheTTL {
					if s.authCache.m == nil {
						s.authCache.m = map[string]authEntry{}
					}
					s.authCache.m[id] = entry
				}
			}
			s.authCache.mu.Unlock()
		}
		if err = rows.Err(); err != nil {
			rows.Close()
			return nil, err
		}
		rows.Close()
	}
	return out, nil
}

func (s *Service) loadGuideWindowChannelMeta(ctx context.Context, keys []guideKey) (map[guideKey]guideWindowChannelMeta, error) {
	out := make(map[guideKey]guideWindowChannelMeta, len(keys))
	for start := 0; start < len(keys); start += guideBatch {
		end := min(start+guideBatch, len(keys))
		batch := keys[start:end]
		values := make([]string, len(batch))
		args := make([]any, 0, len(batch)*2)
		for i, k := range batch {
			values[i] = "(?,?)"
			args = append(args, k.source, k.guide)
		}
		rows, err := s.o.DB.QueryContext(ctx, `WITH ch(source_id, channel_id) AS (VALUES `+strings.Join(values, ",")+`)
SELECT ch.source_id, ch.channel_id,
	EXISTS(SELECT 1 FROM livetv_guide_channels g WHERE g.source_id=ch.source_id AND g.guide_id=ch.channel_id),
	(SELECT MAX(p.end_ms) FROM livetv_programmes p WHERE p.source_id=ch.source_id AND p.channel_id=ch.channel_id)
FROM ch`, args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var k guideKey
			var m guideWindowChannelMeta
			if err = rows.Scan(&k.source, &k.guide, &m.hasGuideChannel, &m.maxEnd); err != nil {
				rows.Close()
				return nil, err
			}
			out[k] = m
		}
		if err = rows.Err(); err != nil {
			rows.Close()
			return nil, err
		}
		rows.Close()
	}
	return out, nil
}

// warmGuideWindowVirtual prepares cold virtual channels within the normal guide
// wait budget without failing the entire batch when one channel is unavailable.
func (s *Service) warmGuideWindowVirtual(ctx context.Context, cs []Channel, now time.Time) error {
	var epoch int64
	if err := s.o.DB.QueryRowContext(ctx, `SELECT generation FROM livetv_channel_epoch WHERE id=1`).Scan(&epoch); err != nil {
		return err
	}
	if s.serviceContext() != nil {
		s.evictIdleVirtualGuide(now)
	}
	s.virtualGuide.mu.RLock()
	revision := s.virtualGuide.revision
	var missing []Channel
	for _, c := range cs {
		if !c.Enabled {
			continue
		}
		entry := s.virtualGuide.entries[c.ID]
		ok := entry != nil && entry.epoch == epoch && entry.revision == revision
		if s.serviceContext() != nil {
			ok = entryCoversNow(entry, c, epoch, revision, now)
		}
		if !ok {
			missing = append(missing, c)
		}
	}
	s.virtualGuide.mu.RUnlock()
	if len(missing) == 0 {
		return nil
	}
	if s.serviceContext() == nil {
		compiled := &guideCompileCache{}
		for _, c := range missing {
			if err := ctx.Err(); err != nil {
				return err
			}
			_ = s.warmSingleVirtualChannel(ctx, c, epoch, revision, now, compiled, nil)
		}
		return ctx.Err()
	}
	compiled := &guideCompileCache{}
	flights := make([]*virtualGuideFlight, 0, len(missing))
	for _, c := range missing {
		flights = append(flights, s.startChannelPrep(c, epoch, revision, now, compiled, nil))
	}
	s.noteVirtualGuideDemand(false)
	waitTimer := s.newTimer(virtualGuideColdWait)
	defer waitTimer.Stop()
	for _, f := range flights {
		select {
		case <-f.done:
		case <-waitTimer.C:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}
