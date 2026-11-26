package livetv

import (
	"context"
	"encoding/json"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
)

// The console lineup is 157k channels on a large IPTV source, so the page no
// longer downloads every row or decodes every programme. This file keeps a
// small in-process index of channel identity, refreshed only when the lineup
// changes, and serves pages of full channels from it.

const (
	channelPageDefault = 200
	channelPageMax     = 500
	channelPageChunk   = 400 // ids per IN (...) keeps SQLite under its variable limit
	guideBatch         = 400 // channel pairs per guide query
)

// ChannelSummary is the console lineup identity: everything the list, search
// and guide rows need, without stream URLs, items or programming.
type ChannelSummary struct {
	ID            string   `json:"id"`
	Number        string   `json:"number"`
	Name          string   `json:"name"`
	Kind          string   `json:"kind"`
	Private       bool     `json:"private,omitempty"`
	Enabled       bool     `json:"enabled"`
	HiddenApps    []string `json:"hiddenApps,omitempty"`
	ShownApps     []string `json:"shownApps,omitempty"`
	Favorite      bool     `json:"favorite"`
	Rating        string   `json:"rating,omitempty"`
	Logo          string   `json:"logo,omitempty"`
	SourceID      string   `json:"sourceId,omitempty"`
	GuideID       string   `json:"guideId,omitempty"`
	GuideSourceID string   `json:"guideSourceId,omitempty"`
	Group         string   `json:"group,omitempty"`
	MergedInto    string   `json:"mergedInto,omitempty"`
	ArchiveDays   int      `json:"archiveDays,omitempty"`

	rawHiddenApps []string
	numberKey     numberKey
	sweepKey      string
	styleID       string
	reelID        string
}

type ChannelCounts struct {
	All       int `json:"all"`
	Live      int `json:"live"`
	Favorites int `json:"favorites"`
	Virtual   int `json:"virtual"`
	IPTV      int `json:"iptv"`
	Off       int `json:"off"`
	Missing   int `json:"missing"`
}

type ChannelGroup struct {
	Key      string `json:"key"`
	SourceID string `json:"sourceId,omitempty"`
	Kind     string `json:"kind"`
	Count    int    `json:"count"`
	Start    int    `json:"start"`
}

type ChannelPageQuery struct {
	Limit  int
	Offset int
	Q      string
	Filter string
	Source string
	// Group keeps only channels in this provider lineup group (exact name).
	Group string
	// OwnerGroup keeps only channels in this Your-groups id (or "none" for ungrouped).
	OwnerGroup string
	// App keeps only channels published to this app (or hidden from it when prefixed with "!").
	App string
	// Sort orders matching channels by "number", "name", "id", or "source".
	Sort string
}

type ChannelPage struct {
	Channels   []Channel       `json:"channels"`
	Total      int             `json:"total"`
	Counts     ChannelCounts   `json:"counts"`
	Groups     []ChannelGroup  `json:"groups"`
	Coverage   map[string]bool `json:"coverage"`
	NextNumber string          `json:"nextNumber"`
}

// ChannelSummaries returns the whole lineup in display order. The result is
// shared and must be treated as read-only.
func (s *Service) ChannelSummaries(ctx context.Context) ([]ChannelSummary, error) {
	all, err := s.channelSummaries(ctx)
	if err != nil {
		return nil, err
	}
	hasMerged := false
	for i := range all {
		if all[i].MergedInto != "" {
			hasMerged = true
			break
		}
	}
	if !hasMerged {
		return all, nil
	}
	out := make([]ChannelSummary, 0, len(all))
	for _, c := range all {
		if c.MergedInto == "" {
			out = append(out, c)
		}
	}
	return out, nil
}

func (s *Service) channelSummaries(ctx context.Context) ([]ChannelSummary, error) {
	prevGroupRev, prevGroupIdx := s.getOwnerGroupCacheState()
	var rev int64
	var rawGroups string
	if err := s.o.DB.QueryRowContext(ctx,
		`SELECT generation, CASE WHEN generation=? THEN '' ELSE COALESCE((SELECT data FROM livetv_owner_groups WHERE id=1),'[]') END FROM livetv_channel_epoch WHERE id=1`,
		prevGroupRev,
	).Scan(&rev, &rawGroups); err != nil {
		return nil, err
	}
	s.lineupMu.Lock()
	if s.lineupIndex != nil && s.lineupRev == rev {
		cached := s.lineupIndex
		s.lineupMu.Unlock()
		if prevGroupIdx == nil || rev != prevGroupRev {
			decoded, err := s.loadOwnerGroupIndexWithTargets(ctx, rev, rawGroups, targetsFromSummaries(cached))
			if err != nil {
				return nil, err
			}
			s.setOwnerGroupCacheState(rev, decoded)
		}
		return cached, nil
	}
	defer s.lineupMu.Unlock()
	rows, err := s.o.DB.QueryContext(ctx, `SELECT data FROM livetv_channels`)
	if err != nil {
		return nil, err
	}
	out := make([]ChannelSummary, 0, 1024)
	for rows.Next() {
		var b []byte
		if err = rows.Scan(&b); err != nil {
			rows.Close()
			return nil, err
		}
		var c ChannelSummary
		if err = json.Unmarshal(b, &c); err != nil {
			rows.Close()
			return nil, err
		}
		c.rawHiddenApps = c.HiddenApps
		var stream struct {
			Look          *ChannelLook    `json:"look"`
			StreamURL     string          `json:"streamUrl"`
			StreamSources []StreamSource  `json:"streamSources"`
			Archive       *ChannelArchive `json:"archive"`
		}
		if err = json.Unmarshal(b, &stream); err != nil {
			rows.Close()
			return nil, err
		}
		if stream.Look != nil {
			c.styleID = stream.Look.LookID
			c.reelID = stream.Look.ReelID
		}
		c.ArchiveDays = (Channel{Archive: stream.Archive}).ArchiveDays()
		c.sweepKey = sweepSignature(Channel{StreamURL: stream.StreamURL, StreamSources: stream.StreamSources})
		c.numberKey = parseNumberKey(c.Number)
		out = append(out, c)
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()

	groupIdx := prevGroupIdx
	if groupIdx == nil || rev != prevGroupRev {
		decoded, err := s.loadOwnerGroupIndexWithTargets(ctx, rev, rawGroups, targetsFromSummaries(out))
		if err != nil {
			return nil, err
		}
		groupIdx = decoded
		s.setOwnerGroupCacheState(rev, groupIdx)
	}
	for i := range out {
		out[i].HiddenApps = groupIdx.effectiveHiddenAppsFor(out[i].SourceID, out[i].Group, out[i].ID, out[i].rawHiddenApps, out[i].ShownApps)
	}

	// Natural channel number order, matching AllowedChannels across every
	// frontend, then source and ID.
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if cmp := a.numberKey.compare(b.numberKey); cmp != 0 {
			return cmp < 0
		}
		if a.SourceID != b.SourceID {
			return a.SourceID < b.SourceID
		}
		return a.ID < b.ID
	})
	// Cache numeric/name/id/source orders once per lineup generation. Paging
	// never sorts 10,000 channels, and equal keys keep a stable ID tie-break.
	orders := map[string][]int{}
	for _, kind := range []string{"number", "source", "name", "id"} {
		order := make([]int, len(out))
		for i := range order {
			order[i] = i
		}
		switch kind {
		case "", "number":
			sort.SliceStable(order, func(i, j int) bool {
				a, b := out[order[i]], out[order[j]]
				if cmp := a.numberKey.compare(b.numberKey); cmp != 0 {
					return cmp < 0
				}
				return strings.ToLower(a.Name) < strings.ToLower(b.Name)
			})
		case "source":
			sort.SliceStable(order, func(i, j int) bool {
				a, b := out[order[i]], out[order[j]]
				if a.SourceID != b.SourceID {
					return a.SourceID < b.SourceID
				}
				return a.numberKey.compare(b.numberKey) < 0
			})
		case "name":
			sort.SliceStable(order, func(i, j int) bool {
				a, b := out[order[i]], out[order[j]]
				na, nb := strings.ToLower(a.Name), strings.ToLower(b.Name)
				if na != nb {
					return na < nb
				}
				return a.numberKey.compare(b.numberKey) < 0
			})
		case "id":
			sort.SliceStable(order, func(i, j int) bool {
				return out[order[i]].ID < out[order[j]].ID
			})
		}
		orders[kind] = order
	}
	s.lineupOrders = orders
	s.lineupIndex = out
	s.lineupRev = rev
	return out, nil
}

func (s *Service) guideCoverageStats(ctx context.Context) (map[guideKey]guideStat, error) {
	now := s.o.Now()
	var rev int64
	if err := s.o.DB.QueryRowContext(ctx, `SELECT generation FROM livetv_channel_epoch WHERE id=1`).Scan(&rev); err != nil {
		return nil, err
	}
	s.coverageMu.Lock()
	defer s.coverageMu.Unlock()
	if s.coverageStats != nil && s.coverageRev == rev && now.Sub(s.coverageAt) < 30*time.Second {
		return s.coverageStats, nil
	}
	stats, err := s.guideStats(ctx, now, now.Add(coverageWindow))
	if err != nil {
		return nil, err
	}
	s.coverageStats, s.coverageAt, s.coverageRev = stats, now, rev
	return stats, nil
}

func summaryGuideSource(c ChannelSummary) string {
	if c.GuideSourceID != "" {
		return c.GuideSourceID
	}
	return c.SourceID
}

func summaryCovered(c ChannelSummary, stats map[guideKey]guideStat) bool {
	if c.Kind == "virtual" {
		return true
	}
	if c.GuideID == "" {
		return false
	}
	st, ok := stats[guideKey{summaryGuideSource(c), c.GuideID}]
	return ok && st.window > 0
}

func channelCounts(all []ChannelSummary, stats map[guideKey]guideStat) ChannelCounts {
	var n ChannelCounts
	for _, c := range all {
		if c.MergedInto != "" {
			continue
		}
		n.All++
		if c.Enabled {
			n.Live++
		} else {
			n.Off++
		}
		if c.Favorite {
			n.Favorites++
		}
		switch c.Kind {
		case "virtual":
			n.Virtual++
		case "iptv":
			n.IPTV++
			if c.Enabled && !summaryCovered(c, stats) {
				n.Missing++
			}
		}
	}
	return n
}

func channelMatches(c ChannelSummary, q ChannelPageQuery, stats map[guideKey]guideStat, groupIdx *ownerGroupIndex) bool {
	if c.MergedInto != "" {
		return false
	}
	if q.Group != "" && c.Group != q.Group {
		return false
	}
	if q.OwnerGroup != "" && !groupIdx.inGroup(q.OwnerGroup, c.SourceID, c.Group, c.ID) {
		return false
	}
	if q.App != "" {
		if wantHidden, ok := strings.CutPrefix(q.App, "!"); ok {
			if !slices.Contains(c.HiddenApps, wantHidden) {
				return false
			}
		} else if slices.Contains(c.HiddenApps, q.App) {
			return false
		}
	}
	if q.Source != "" {
		if q.Source == "virtual" {
			if c.Kind != "virtual" {
				return false
			}
		} else if c.SourceID != q.Source {
			return false
		}
	}
	switch q.Filter {
	case "live":
		if !c.Enabled {
			return false
		}
	case "favorites":
		if !c.Favorite {
			return false
		}
	case "virtual":
		if c.Kind != "virtual" {
			return false
		}
	case "iptv":
		if c.Kind != "iptv" {
			return false
		}
	case "missing":
		if !c.Enabled || c.Kind != "iptv" || summaryCovered(c, stats) {
			return false
		}
	case "off":
		if !c.Enabled {
			return false
		}
	}
	if needle := strings.ToLower(strings.TrimSpace(q.Q)); needle != "" {
		if !strings.Contains(strings.ToLower(c.Number+" "+c.Name+" "+c.Group), needle) && !channelMatchesText(c.Name, c.Number, c.Group, needle) {
			return false
		}
	}
	return true
}

func channelGroups(all []ChannelSummary, order []int) []ChannelGroup {
	out := []ChannelGroup{}
	for i, at := range order {
		c := all[at]
		key := "virtual"
		kind := "virtual"
		if c.Kind != "virtual" {
			key = "src-" + c.SourceID
			kind = "iptv"
		}
		if len(out) == 0 || out[len(out)-1].Key != key {
			out = append(out, ChannelGroup{Key: key, SourceID: c.SourceID, Kind: kind, Start: i})
		}
		out[len(out)-1].Count++
	}
	return out
}

func nextChannelNumber(all []ChannelSummary) string {
	max := 0
	for _, c := range all {
		if c.MergedInto != "" {
			continue
		}
		if n, err := strconv.ParseFloat(c.Number, 64); err == nil && int(n) > max {
			max = int(n)
		}
	}
	return strconv.Itoa(max + 1)
}

// ChannelPage returns one page of full channel records for the console list
// and guide, plus the totals and groups the page needs to lay out the rest.
func (s *Service) ChannelPage(ctx context.Context, q ChannelPageQuery) (ChannelPage, error) {
	// Builds or refreshes the immutable lineup index read under lineupMu below.
	if _, err := s.channelSummaries(ctx); err != nil {
		return ChannelPage{}, err
	}
	_, groupIdx := s.getOwnerGroupCacheState()
	stats, err := s.guideCoverageStats(ctx)
	if err != nil {
		return ChannelPage{}, err
	}
	if q.Limit <= 0 {
		q.Limit = channelPageDefault
	}
	if q.Limit > channelPageMax {
		q.Limit = channelPageMax
	}
	if q.Offset < 0 {
		q.Offset = 0
	}
	kind := q.Sort
	if kind == "" {
		kind = "number"
	}
	s.lineupMu.Lock()
	candidates := s.lineupOrders[kind]
	// A concurrent generation rebuild may replace the cache after our index
	// read. Re-read the matching immutable index and its order together.
	all := s.lineupIndex
	if candidates == nil {
		candidates = s.lineupOrders["number"]
	}
	s.lineupMu.Unlock()
	order := make([]int, 0, len(candidates))
	for _, i := range candidates {
		if channelMatches(all[i], q, stats, groupIdx) {
			order = append(order, i)
		}
	}
	start := min(q.Offset, len(order))
	end := min(start+q.Limit, len(order))
	page := ChannelPage{
		Channels:   []Channel{},
		Total:      len(order),
		Counts:     channelCounts(all, stats),
		Groups:     channelGroups(all, order),
		Coverage:   map[string]bool{},
		NextNumber: nextChannelNumber(all),
	}
	ids := make([]string, 0, end-start)
	for _, at := range order[start:end] {
		c := all[at]
		ids = append(ids, c.ID)
		page.Coverage[c.ID] = c.Kind == "virtual" || summaryCovered(c, stats)
	}
	full, err := s.channelsByIDs(ctx, ids)
	if err != nil {
		return ChannelPage{}, err
	}
	for _, at := range order[start:end] {
		c := all[at]
		// A concurrent delete can leave a summary without its row. Keep the
		// slice aligned for the client rather than shifting later rows.
		v, ok := full[c.ID]
		if !ok {
			v = channelFromSummary(c)
		}
		v.HiddenApps = groupIdx.effectiveHiddenApps(v)
		page.Channels = append(page.Channels, v)
	}
	return page, nil
}

// channelsByIDs reads the full records for a page, in chunks that SQLite's
// bind-variable limit allows.
func (s *Service) channelsByIDs(ctx context.Context, ids []string) (map[string]Channel, error) {
	out := make(map[string]Channel, len(ids))
	pending := make([]string, 0, len(ids))
	s.virtualGuide.mu.RLock()
	for _, id := range ids {
		if entry := s.virtualGuide.entries[id]; entry != nil {
			out[id] = entry.channel
		} else {
			pending = append(pending, id)
		}
	}
	s.virtualGuide.mu.RUnlock()
	return s.readChannelsByIDs(ctx, pending, out)
}

func (s *Service) readChannelsByIDs(ctx context.Context, ids []string, out map[string]Channel) (map[string]Channel, error) {
	for start := 0; start < len(ids); start += channelPageChunk {
		end := min(start+channelPageChunk, len(ids))
		marks := make([]string, 0, end-start)
		args := make([]any, 0, end-start)
		for _, id := range ids[start:end] {
			marks = append(marks, "?")
			args = append(args, id)
		}
		rows, err := s.o.DB.QueryContext(ctx, `SELECT id, data FROM livetv_channels WHERE id IN (`+strings.Join(marks, ",")+`)`, args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var id string
			var b []byte
			if err = rows.Scan(&id, &b); err != nil {
				rows.Close()
				return nil, err
			}
			var c Channel
			if err = json.Unmarshal(b, &c); err != nil {
				rows.Close()
				return nil, err
			}
			out[id] = c
		}
		if err = rows.Err(); err != nil {
			rows.Close()
			return nil, err
		}
		rows.Close()
	}
	return out, nil
}

// virtualChannels returns only virtual channels' full records. The scheduler
// only needs those; loading 157k IPTV rows every 30 seconds was pure waste.
func (s *Service) virtualChannels(ctx context.Context) ([]Channel, error) {
	all, err := s.channelSummaries(ctx)
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0)
	for _, c := range all {
		if c.Kind == "virtual" {
			ids = append(ids, c.ID)
		}
	}
	if len(ids) == 0 {
		return nil, nil
	}
	full, err := s.channelsByIDs(ctx, ids)
	if err != nil {
		return nil, err
	}
	out := make([]Channel, 0, len(ids))
	for _, c := range all {
		if c.Kind == "virtual" {
			if v, ok := full[c.ID]; ok {
				out = append(out, v)
			}
		}
	}
	return out, nil
}

// StyleChannelIDs reads the invalidated lineup index, never programme payloads.
func (s *Service) StyleChannelIDs(ctx context.Context) (map[string][]string, error) {
	channels, err := s.channelSummaries(ctx)
	if err != nil {
		return nil, err
	}
	out := map[string][]string{}
	for _, c := range channels {
		id := c.styleID
		if id != "" {
			out[id] = append(out[id], c.ID)
		}
	}
	return out, nil
}

func (s *Service) ReelChannelIDs(ctx context.Context) (map[string][]string, error) {
	channels, err := s.channelSummaries(ctx)
	if err != nil {
		return nil, err
	}
	out := map[string][]string{}
	for _, c := range channels {
		if c.reelID != "" {
			out[c.reelID] = append(out[c.reelID], c.ID)
		}
	}
	return out, nil
}
