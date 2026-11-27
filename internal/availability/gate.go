package availability

import (
	"cmp"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"maps"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/releaseparse"
	"github.com/braedonsaunders/ether/internal/store"
)

const gateMigration = `
CREATE TABLE availability_gate (
 policy TEXT NOT NULL,
 rating_key INTEGER NOT NULL,
 first_playable_at INTEGER NOT NULL,
 source_id TEXT NOT NULL DEFAULT '',
 updated_at INTEGER NOT NULL,
 PRIMARY KEY(policy, rating_key)
);
CREATE TABLE availability_gate_policy (
 policy TEXT PRIMARY KEY,
 seeded_at INTEGER NOT NULL
);
ALTER TABLE availability_title_checks ADD COLUMN kind TEXT NOT NULL DEFAULT 'movie';
ALTER TABLE availability_title_checks ADD COLUMN anchor_ts INTEGER NOT NULL DEFAULT 0;
ALTER TABLE availability_title_checks ADD COLUMN window_end_ts INTEGER NOT NULL DEFAULT 0;
ALTER TABLE availability_title_checks ADD COLUMN priority REAL NOT NULL DEFAULT 0;
CREATE INDEX availability_title_checks_due ON availability_title_checks(next_check, priority DESC, rating_key, kind, anchor_ts, window_end_ts) WHERE next_check > 0;
`

func init() {
	store.RegisterAfter("availability_gate", "availability", "availability_hidden_intervals", "availability_coverage", "availability_checks_retention")
	store.Register("availability_gate", 1, gateMigration)
}

// retryOffsets defines the bounded retry cadence from a title's official
// release/air/digital anchor before falling back to daily checks until the
// evidence window ends.
var retryOffsets = []time.Duration{
	0,
	30 * time.Minute,
	1 * time.Hour,
	90 * time.Minute,
	2 * time.Hour,
	150 * time.Minute,
	3 * time.Hour,
	4 * time.Hour,
	8 * time.Hour,
}

const firstLookPriorityBoost = 1_000_000.0

// nextRetryAfter returns the smallest scheduled retry timestamp strictly after
// nowTS and strictly before windowEndTS (0 when the window has ended).
func nextRetryAfter(anchorTS, windowEndTS, nowTS int64) int64 {
	if anchorTS <= 0 {
		return 0
	}
	if windowEndTS > 0 && nowTS >= windowEndTS {
		return 0
	}
	for _, d := range retryOffsets {
		cand := anchorTS + int64(d.Seconds())
		if cand > nowTS {
			if windowEndTS > 0 && cand >= windowEndTS {
				return 0
			}
			return cand
		}
	}
	elapsed := nowTS - anchorTS
	days := elapsed/86400 + 1
	cand := anchorTS + days*86400
	if windowEndTS > 0 && cand >= windowEndTS {
		return 0
	}
	return cand
}

// Followed episodes need retries during the evening too: the generic cadence
// ends at 08:00 UTC, well before many networks broadcast. Keep these targeted
// checks hourly within the existing evidence window and indexer budgets.
func nextEpisodeRetry(anchorTS, windowEndTS, nowTS int64, followed bool) int64 {
	next := nextRetryAfter(anchorTS, windowEndTS, nowTS)
	if followed && nowTS >= anchorTS-int64((72*time.Hour).Seconds()) && nowTS < anchorTS {
		return min(anchorTS, nowTS+int64((6*time.Hour).Seconds()))
	}
	if followed && nowTS >= anchorTS && nowTS < windowEndTS {
		hourly := anchorTS + ((nowTS-anchorTS)/3600+1)*3600
		if hourly < windowEndTS && (next == 0 || hourly < next) {
			return hourly
		}
	}
	return next
}

// computeGatePolicy hashes the profile, resolver, provider usability, and feed
// source generations that govern whether an accepted release opens the gate.
// Window lengths, poll budgets, and upcoming-movie flags are applied in SQL at
// read time so changing them never invalidates existing gate rows.
func computeGatePolicy(policy string, usenet, torrent bool, feeds []core.ReleaseFeed) string {
	h := sha256.New()
	h.Write([]byte("gate-v1:"))
	h.Write([]byte(policy))
	if usenet {
		h.Write([]byte(":u1"))
	} else {
		h.Write([]byte(":u0"))
	}
	if torrent {
		h.Write([]byte(":t1"))
	} else {
		h.Write([]byte(":t0"))
	}
	if len(feeds) == 1 {
		f := feeds[0]
		h.Write([]byte{'|'})
		h.Write([]byte(f.ID))
		h.Write([]byte{'='})
		h.Write([]byte(f.Generation))
		h.Write([]byte{'/'})
		h.Write([]byte(f.Protocol))
	} else if len(feeds) > 1 {
		order := make([]int, len(feeds))
		for i := range order {
			order[i] = i
		}
		sort.Slice(order, func(i, j int) bool {
			if feeds[order[i]].ID != feeds[order[j]].ID {
				return feeds[order[i]].ID < feeds[order[j]].ID
			}
			return feeds[order[i]].Generation < feeds[order[j]].Generation
		})
		for _, idx := range order {
			f := feeds[idx]
			h.Write([]byte{'|'})
			h.Write([]byte(f.ID))
			h.Write([]byte{'='})
			h.Write([]byte(f.Generation))
			h.Write([]byte{'/'})
			h.Write([]byte(f.Protocol))
		}
	}
	var sum [sha256.Size]byte
	return hex.EncodeToString(h.Sum(sum[:0]))
}

func (s *Service) currentGatePolicy() string {
	policy, _, usenet, torrent := s.discoverySnapFull()
	var feeds []core.ReleaseFeed
	if s.d.Feeds != nil {
		feeds = s.d.Feeds.FeedSources()
	}
	return computeGatePolicy(policy, usenet, torrent, feeds)
}

type gateOpening struct {
	key    core.RatingKey
	source string
}

// openGate permanently opens the release gate for (policy, key). Once opened,
// a gate row is never deleted or closed by Invalidate or time passing.
func (s *Service) openGate(ctx context.Context, policy string, key core.RatingKey, sourceID string, at int64) error {
	if !s.hasGateTable || policy == "" || key == 0 {
		return nil
	}
	if err := s.recordPlayableEpisode(ctx, key, time.Unix(at, 0)); err != nil {
		return err
	}
	res, err := s.db.ExecContext(ctx, `INSERT INTO availability_gate(policy,rating_key,first_playable_at,source_id,updated_at) VALUES(?,?,?,?,?) ON CONFLICT(policy,rating_key) DO NOTHING`,
		policy, int64(key), at, sourceID, at)
	if err != nil {
		return err
	}
	rows, _ := res.RowsAffected()
	if rows > 0 {
		_, _ = s.db.ExecContext(ctx, `UPDATE availability_title_checks SET next_check=0,state='found',reason='' WHERE rating_key=? AND (next_check<>0 OR state<>'found')`, int64(key))
		s.gateChanged()
	}
	return nil
}

func (s *Service) gateChanged() {
	s.gateRev.Add(1)
	s.clearVisibility()
	if c, ok := s.d.Catalog.(interface{ ConsoleGateChanged() }); ok {
		c.ConsoleGateChanged()
	}
}

func (s *Service) recordPlayableEpisode(ctx context.Context, key core.RatingKey, at time.Time) error {
	if key.Kind() == core.KindEpisode {
		if c, ok := s.d.Catalog.(interface {
			RecordPlayableEpisode(context.Context, core.RatingKey, time.Time) error
		}); ok {
			return c.RecordPlayableEpisode(ctx, key, at)
		}
	}
	return nil
}

func (s *Service) recordPlayableEpisodes(ctx context.Context, keys []core.RatingKey, at time.Time) error {
	var eps []core.RatingKey
	for _, k := range keys {
		if k.Kind() == core.KindEpisode {
			eps = append(eps, k)
		}
	}
	if len(eps) == 0 {
		return nil
	}
	if batch, ok := s.d.Catalog.(interface {
		RecordPlayableEpisodes(context.Context, []core.RatingKey, time.Time) error
	}); ok {
		return batch.RecordPlayableEpisodes(ctx, eps, at)
	}
	for _, k := range eps {
		if err := s.recordPlayableEpisode(ctx, k, at); err != nil {
			return err
		}
	}
	return nil
}

func insertGateBatch(ctx context.Context, tx *sql.Tx, policy string, openings []gateOpening, nowTS int64) (int64, error) {
	if len(openings) == 0 || policy == "" {
		return 0, nil
	}
	seen := make(map[core.RatingKey]bool, len(openings))
	dedup := make([]gateOpening, 0, len(openings))
	for _, op := range openings {
		if op.key > 0 && !seen[op.key] {
			seen[op.key] = true
			dedup = append(dedup, op)
		}
	}
	var added int64
	const batch = 500
	for chunk := range store.Chunks(dedup, batch) {
		var sb strings.Builder
		sb.Grow(128 + len(chunk)*12)
		sb.WriteString(`INSERT INTO availability_gate(policy,rating_key,first_playable_at,source_id,updated_at) VALUES `)
		args := make([]any, 0, len(chunk)*5)
		for i, op := range chunk {
			if i > 0 {
				sb.WriteByte(',')
			}
			sb.WriteString("(?,?,?,?,?)")
			args = append(args, policy, int64(op.key), nowTS, op.source, nowTS)
		}
		sb.WriteString(` ON CONFLICT(policy,rating_key) DO NOTHING`)
		res, err := tx.ExecContext(ctx, sb.String(), args...)
		if err != nil {
			return added, err
		}
		if n, _ := res.RowsAffected(); n > 0 {
			added += n
		}
	}
	return added, nil
}

func (s *Service) isGateOpen(ctx context.Context, policy string, key core.RatingKey) bool {
	if !s.hasGateTable || policy == "" || key == 0 {
		return false
	}
	var ok bool
	err := s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM availability_gate WHERE policy=? AND rating_key=?)`, policy, int64(key)).Scan(&ok)
	return err == nil && ok
}

func (s *Service) openGateKeys(ctx context.Context, policy string, keys []core.RatingKey) map[core.RatingKey]bool {
	out := map[core.RatingKey]bool{}
	if !s.hasGateTable || policy == "" || len(keys) == 0 {
		return out
	}
	for start := 0; start < len(keys); start += 500 {
		batch := keys[start:min(start+500, len(keys))]
		args := make([]any, 0, len(batch)+1)
		args = append(args, policy)
		for _, k := range batch {
			args = append(args, int64(k))
		}
		rows, err := s.db.QueryContext(ctx, `SELECT rating_key FROM availability_gate WHERE policy=? AND rating_key IN (`+store.Placeholders(len(batch))+`)`, args...)
		if err != nil {
			continue
		}
		for rows.Next() {
			var k int64
			if rows.Scan(&k) == nil {
				out[core.RatingKey(k)] = true
			}
		}
		rows.Close()
	}
	return out
}

// officialDatePassed reports whether t's official release date has passed at
// now: digital/physical home release date for movies, air date for episodes.
func (s *Service) officialDatePassed(ctx context.Context, t core.Title, now time.Time) bool {
	if t.Adult || !t.IsReleased(now) {
		return false
	}
	switch t.Type {
	case core.TypeEpisode:
		return !t.Released.IsZero() && !t.Released.After(now)
	case core.TypeMovie:
		if !t.HomeReleased.IsZero() {
			return !t.HomeReleased.After(now)
		}
		if t.HomeReleaseChecked {
			return false
		}
		if h, ok := s.d.Catalog.(interface {
			HomeReleases(context.Context, []core.RatingKey) (map[core.RatingKey]time.Time, error)
		}); ok {
			if got, err := h.HomeReleases(ctx, []core.RatingKey{t.Key}); err == nil {
				at := got[t.Key]
				return !at.IsZero() && !at.After(now)
			}
		}
		return false
	default:
		return false
	}
}

func (s *Service) officialDatesPassed(ctx context.Context, titles map[core.RatingKey]*core.Title, now time.Time) map[core.RatingKey]bool {
	out := map[core.RatingKey]bool{}
	var lookup []core.RatingKey
	for k, t := range titles {
		if t == nil || t.Adult || !t.IsReleased(now) {
			continue
		}
		switch t.Type {
		case core.TypeEpisode:
			if !t.Released.IsZero() && !t.Released.After(now) {
				out[k] = true
			}
		case core.TypeMovie:
			if !t.HomeReleased.IsZero() {
				if !t.HomeReleased.After(now) {
					out[k] = true
				}
			} else if !t.HomeReleaseChecked {
				lookup = append(lookup, k)
			}
		}
	}
	if len(lookup) > 0 {
		if h, ok := s.d.Catalog.(interface {
			HomeReleases(context.Context, []core.RatingKey) (map[core.RatingKey]time.Time, error)
		}); ok {
			if got, err := h.HomeReleases(ctx, lookup); err == nil {
				for _, k := range lookup {
					if at := got[k]; !at.IsZero() && !at.After(now) {
						out[k] = true
					}
				}
			}
		}
	}
	return out
}

// maybeOpenGateFromRelease evaluates a newly recorded release and opens
// availability_gate iff it is verified or the title's official date has passed
// (and no newer negative cooldown applies), and the quality profile accepts it.
func (s *Service) maybeOpenGateFromRelease(ctx context.Context, f core.ReleaseFeed, t core.Title, r core.Release, now time.Time, verified bool) error {
	if !s.hasGateTable || t.Adult || (!t.IsReleased(now) && !(verified && t.Type == core.TypeEpisode)) {
		return nil
	}
	input := s.discoverySnapshot()
	if f.ID != "probe" {
		matched := false
		for _, src := range input.feeds {
			if src.ID == f.ID && src.Generation == f.Generation {
				if r.Protocol == "" {
					r.Protocol = src.Protocol
				}
				matched = true
				break
			}
		}
		if !matched {
			return nil
		}
	}
	if !input.usable(r.Protocol) {
		return nil
	}
	if s.isGateOpen(ctx, input.gatePolicy, t.Key) {
		return nil
	}
	if !verified {
		if !s.officialDatePassed(ctx, t, now) {
			return nil
		}
		if source, ok := s.d.Validator.(interface {
			KnownUnavailableSince(context.Context, []core.RatingKey) map[core.RatingKey]time.Time
		}); ok {
			unavail := source.KnownUnavailableSince(ctx, []core.RatingKey{t.Key})
			if since, has := unavail[t.Key]; has {
				var firstSeen int64
				var state string
				var rev int64
				if err := s.db.QueryRowContext(ctx, `SELECT first_seen,state,revision FROM availability_releases WHERE id=? AND rating_key=? AND source_id=?`, r.ID, int64(t.Key), f.ID).Scan(&firstSeen, &state, &rev); err == nil {
					if state == "unavailable" || (state == "unknown" && rev > 0) || firstSeen <= since.Unix() {
						return nil
					}
				} else if now.Unix() <= since.Unix() {
					return nil
				}
			}
		}
	}
	cand := r
	if cand.Info.Title == "" && cand.Name != "" {
		cand.Info = releaseparse.Parse(cand.Name)
	}
	verdicts := s.judge(offRequest(ctx), []feedPair{{t: &t, r: cand}}, true)
	if len(verdicts) == 1 && verdicts[0] {
		return s.openGate(ctx, input.gatePolicy, t.Key, f.ID, now.Unix())
	}
	return nil
}

// reopenRetainedGates rechecks retained evidence only for titles being used
// or checked. Listing pages and their counts read the published gates in SQL.
func (s *Service) reopenRetainedGates(ctx context.Context, titles map[core.RatingKey]*core.Title, feeds map[string]core.ReleaseFeed, input discoverySnapshot, acceptable map[core.RatingKey]bool) error {
	if len(titles) == 0 {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	keys := make([]core.RatingKey, 0, len(titles))
	for k := range titles {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	type observation struct {
		title     *core.Title
		release   core.Release
		source    string
		firstSeen int64
		verified  bool
	}
	var observations []observation
	for batch := range store.Chunks(keys, 500) {
		args := make([]any, 0, len(batch)+1)
		args = append(args, input.retentionCutoff().Unix())
		for _, k := range batch {
			args = append(args, int64(k))
		}
		rows, err := s.db.QueryContext(ctx, `SELECT rating_key,source_id,generation,payload,state,first_seen
 FROM availability_releases WHERE last_seen>=? AND state<>'unavailable' AND NOT (state='unknown' AND revision>0)
 AND rating_key IN (`+store.Placeholders(len(batch))+`)`, args...)
		if err != nil {
			return err
		}
		for rows.Next() {
			var k core.RatingKey
			var source, generation, payload, state string
			var firstSeen int64
			if err := rows.Scan(&k, &source, &generation, &payload, &state, &firstSeen); err != nil {
				rows.Close()
				return err
			}
			t := titles[k]
			verified := state == "verified"
			if t.Adult || (!t.IsReleased(input.now) && !(verified && t.Type == core.TypeEpisode)) {
				continue
			}
			feed := feeds[source]
			if source != "probe" && (feed.ID == "" || feed.Generation != generation || !input.usable(feed.Protocol)) {
				continue
			}
			r, err := s.decode(payload)
			if err != nil {
				continue
			}
			if r.Protocol == "" {
				r.Protocol = feed.Protocol
			}
			if !input.usable(r.Protocol) {
				continue
			}
			observations = append(observations, observation{t, r, source, firstSeen, verified})
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
	}
	if len(observations) == 0 {
		return nil
	}

	unverified := make(map[core.RatingKey]*core.Title)
	for _, o := range observations {
		if !o.verified {
			unverified[o.title.Key] = o.title
		}
	}
	passed := s.officialDatesPassed(ctx, unverified, input.now)
	unavailable := s.unavailableSince(ctx, slices.Collect(maps.Keys(unverified)))
	var pairs []feedPair
	var sources []string
	for _, o := range observations {
		key := o.title.Key
		if !o.verified {
			since, blocked := unavailable[key]
			if !passed[key] || (blocked && o.firstSeen <= since.Unix()) {
				continue
			}
		}
		r := o.release
		if r.Info.Title == "" && r.Name != "" {
			r.Info = releaseparse.Parse(r.Name)
		}
		pairs = append(pairs, feedPair{t: o.title, r: r})
		sources = append(sources, o.source)
	}

	var openings []gateOpening
	var openKeys []core.RatingKey
	seen := make(map[core.RatingKey]bool)
	for i, ok := range s.judge(ctx, pairs, true) {
		k := pairs[i].t.Key
		if ok && !seen[k] {
			seen[k] = true
			openings = append(openings, gateOpening{key: k, source: sources[i]})
			openKeys = append(openKeys, k)
		}
	}
	if len(openings) == 0 {
		return ctx.Err()
	}
	if err := s.recordPlayableEpisodes(ctx, openKeys, input.now); err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	added, err := insertGateBatch(ctx, tx, input.gatePolicy, openings, input.now.Unix())
	if err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	for _, k := range openKeys {
		acceptable[k] = true
	}
	if added > 0 {
		s.gateChanged()
	}
	return nil
}

func (s *Service) unavailableSince(ctx context.Context, keys []core.RatingKey) map[core.RatingKey]time.Time {
	if len(keys) == 0 {
		return nil
	}
	if current, _ := ctx.Value(currentPolicyKey{}).(bool); current {
		if source, ok := s.d.Validator.(interface {
			KnownUnavailableSinceCurrent(context.Context, []core.RatingKey) map[core.RatingKey]time.Time
		}); ok {
			return source.KnownUnavailableSinceCurrent(ctx, keys)
		}
	}
	if source, ok := s.d.Validator.(interface {
		KnownUnavailableSince(context.Context, []core.RatingKey) map[core.RatingKey]time.Time
	}); ok {
		return source.KnownUnavailableSince(ctx, keys)
	}
	return nil
}

type storedObsState struct {
	gen       string
	state     string
	firstSeen int64
	revision  int64
}

func (s *Service) storedObservationStates(ctx context.Context, titles map[core.RatingKey]*core.Title) map[string]storedObsState {
	keys := make([]any, 0, len(titles))
	for k := range titles {
		keys = append(keys, int64(k))
	}
	out := map[string]storedObsState{}
	for start := 0; start < len(keys); start += 500 {
		batch := keys[start:min(start+500, len(keys))]
		rows, err := s.db.QueryContext(ctx, `SELECT id,rating_key,source_id,generation,state,first_seen,revision FROM availability_releases WHERE rating_key IN (`+store.Placeholders(len(batch))+`)`, batch...)
		if err != nil {
			continue
		}
		for rows.Next() {
			var id, src, gen, state string
			var key, firstSeen, rev int64
			if rows.Scan(&id, &key, &src, &gen, &state, &firstSeen, &rev) == nil {
				out[observationKey(id, core.RatingKey(key), src)] = storedObsState{gen: gen, state: state, firstSeen: firstSeen, revision: rev}
			}
		}
		rows.Close()
	}
	return out
}

// matchRetainedAndOpenGate matches retained availability_feed_items against
// ungated titles. Post-official-date matches open availability_gate immediately;
// pre-official-date matches only queue availability_releases for background
// verification without opening the gate. Repeat reads with unchanged feed
// generations perform zero database writes.
func (s *Service) matchRetainedAndOpenGate(ctx context.Context, titles map[core.RatingKey]*core.Title, feeds map[string]core.ReleaseFeed, input discoverySnapshot, acceptable map[core.RatingKey]bool) {
	if len(titles) == 0 {
		return
	}
	byName := map[string][]*core.Title{}
	for _, t := range titles {
		names := []string{t.Title, t.OriginalTitle}
		if t.Type == core.TypeEpisode {
			names = []string{t.ShowTitle, t.ShowOriginalTitle}
		}
		for _, name := range names {
			n := releaseparse.NormalizeTitle(name)
			if n != "" {
				byName[n] = append(byName[n], t)
			}
		}
	}
	if len(byName) == 0 {
		return
	}
	names := make([]string, 0, len(byName))
	for n := range byName {
		names = append(names, n)
	}
	sort.Strings(names)
	type pending struct {
		source, gen, raw string
		seen             int64
	}
	var work []pending
	for start := 0; start < len(names); start += 500 {
		ns := names[start:min(start+500, len(names))]
		args := make([]any, 0, len(ns)+1)
		marks := make([]string, len(ns))
		for i, n := range ns {
			marks[i] = "?"
			args = append(args, n)
		}
		args = append(args, input.retentionCutoff().Unix())
		rows, err := s.db.QueryContext(ctx, `SELECT source_id,generation,payload,seen_at FROM availability_feed_items WHERE match_title IN (`+strings.Join(marks, ",")+`) AND seen_at>=? ORDER BY seen_at DESC, source_id, id LIMIT 2000`, args...)
		if err != nil {
			continue
		}
		for rows.Next() {
			var p pending
			if rows.Scan(&p.source, &p.gen, &p.raw, &p.seen) == nil {
				work = append(work, p)
			}
		}
		rows.Close()
	}
	if len(work) == 0 {
		return
	}

	type observed struct {
		r    core.Release
		t    *core.Title
		feed core.ReleaseFeed
		p    pending
	}
	var candidates []observed
	var pairs []feedPair
	for _, p := range work {
		f, ok := feeds[p.source]
		if !ok || f.Generation != p.gen || !input.usable(f.Protocol) {
			continue
		}
		r, err := s.decode(p.raw)
		if err != nil {
			continue
		}
		r.Info = releaseparse.Parse(r.Name)
		for _, t := range byName[releaseparse.NormalizeTitle(r.Info.Title)] {
			if t.Type == core.TypeEpisode && !releaseparse.EpisodeMatches(r.Info, t.Season, t.Episode) {
				season := false
				for _, n := range r.Info.Seasons {
					if n == t.Season {
						season = true
					}
				}
				if !r.Info.FullSeason || !season {
					continue
				}
			}
			if t.Type == core.TypeMovie && (len(r.Info.Seasons) > 0 || len(r.Info.Episodes) > 0) {
				continue
			}
			candidates = append(candidates, observed{r: r, t: t, feed: f, p: p})
			pairs = append(pairs, feedPair{t: t, r: r})
		}
	}
	if len(pairs) == 0 {
		return
	}
	var found []observed
	for i, ok := range s.judge(ctx, pairs, false) {
		if ok {
			found = append(found, candidates[i])
		}
	}
	if len(found) == 0 {
		return
	}

	stored := s.storedObservationStates(ctx, titles)
	passed := s.officialDatesPassed(ctx, titles, input.now)
	var keyList []core.RatingKey
	for k := range titles {
		keyList = append(keyList, k)
	}
	unavailable := s.unavailableSince(ctx, keyList)

	var toInsert []observed
	var toOpen []gateOpening
	openedSet := map[core.RatingKey]bool{}
	for _, o := range found {
		okKey := observationKey(o.r.ID, o.t.Key, o.feed.ID)
		st, hasStored := stored[okKey]
		sameGen := hasStored && st.gen == o.feed.Generation
		if !sameGen {
			toInsert = append(toInsert, o)
		}
		firstSeen := o.p.seen
		invalidated := false
		isVerified := false
		if sameGen {
			firstSeen = st.firstSeen
			invalidated = st.state == "unavailable" || (st.state == "unknown" && st.revision > 0)
			isVerified = st.state == "verified"
		}
		if invalidated {
			continue
		}
		if !isVerified {
			if !passed[o.t.Key] {
				continue
			}
			if since, has := unavailable[o.t.Key]; has && firstSeen <= since.Unix() {
				continue
			}
		}
		acceptable[o.t.Key] = true
		if s.hasGateTable && input.gatePolicy != "" && !openedSet[o.t.Key] {
			openedSet[o.t.Key] = true
			toOpen = append(toOpen, gateOpening{key: o.t.Key, source: o.feed.ID})
		}
	}

	if len(toOpen) > 0 {
		openKeys := make([]core.RatingKey, len(toOpen))
		for i, op := range toOpen {
			openKeys[i] = op.key
		}
		if err := s.recordPlayableEpisodes(ctx, openKeys, input.now); err != nil {
			return
		}
	}
	if len(toInsert) == 0 && len(toOpen) == 0 {
		return
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return
	}
	defer tx.Rollback()
	if len(toInsert) > 0 {
		dedup := make([]observed, 0, len(toInsert))
		pos := make(map[string]int, len(toInsert))
		for _, o := range toInsert {
			okKey := observationKey(o.r.ID, o.t.Key, o.feed.ID)
			if idx, exists := pos[okKey]; exists {
				dedup[idx] = o
				continue
			}
			pos[okKey] = len(dedup)
			dedup = append(dedup, o)
		}
		const relBatch = 250
		for chunk := range store.Chunks(dedup, relBatch) {
			var sb strings.Builder
			sb.Grow(256 + len(chunk)*28)
			sb.WriteString(`INSERT INTO availability_releases(id,rating_key,source_id,generation,payload,title,name,protocol,first_seen,last_seen,priority,policy,policy_digest) VALUES `)
			args := make([]any, 0, len(chunk)*13)
			for i, o := range chunk {
				if i > 0 {
					sb.WriteByte(',')
				}
				sb.WriteString("(?,?,?,?,?,?,?,?,?,?,?,?,?)")
				args = append(args, o.r.ID, o.t.Key, o.feed.ID, o.feed.Generation, o.p.raw, o.t.Title, o.r.Name, cmp.Or(o.r.Protocol, o.feed.Protocol), o.p.seen, o.p.seen, o.t.Popularity, input.policy, releasePolicyDigest(o.r))
			}
			sb.WriteString(` ON CONFLICT(id,rating_key,source_id) DO UPDATE SET generation=excluded.generation,payload=excluded.payload,policy_digest=excluded.policy_digest,first_seen=excluded.first_seen,last_seen=excluded.last_seen,state='observed',verified_at=0,first_verified=0,next_check=0,revision=availability_releases.revision+1 WHERE availability_releases.generation<>excluded.generation`)
			if _, err := tx.ExecContext(ctx, sb.String(), args...); err != nil {
				return
			}
		}
	}
	nowTS := input.now.Unix()
	gateAdded, err := insertGateBatch(ctx, tx, input.gatePolicy, toOpen, nowTS)
	if err != nil {
		return
	}
	if gateAdded > 0 {
		const checkBatch = 500
		for chunk := range store.Chunks(toOpen, checkBatch) {
			args := make([]any, len(chunk))
			for i, op := range chunk {
				args[i] = int64(op.key)
			}
			_, _ = tx.ExecContext(ctx, `UPDATE availability_title_checks SET next_check=0,state='found',reason='' WHERE rating_key IN (`+store.Placeholders(len(chunk))+`) AND (next_check<>0 OR state<>'found')`, args...)
		}
	}
	if err := tx.Commit(); err != nil {
		return
	}
	if gateAdded > 0 {
		s.gateRev.Add(1)
		if c, ok := s.d.Catalog.(interface{ ConsoleGateChanged() }); ok {
			c.ConsoleGateChanged()
		}
	}
}

func observationKey(id string, key core.RatingKey, source string) string {
	return id + "\x00" + strconv.FormatInt(int64(key), 10) + "\x00" + source
}

// seedDueChecks registers in-window and near-horizon movies and episodes in
// availability_title_checks whenever the catalogue write generation or gate
// policy changes. On an idle server with no catalogue writes, it returns
// immediately with zero SQL queries and zero heap allocations.
func (s *Service) seedDueChecks(ctx context.Context, cfg config.Availability) error {
	if !s.hasGateTable {
		return nil
	}
	moviePlayable := cfg.ModeFor(hiddenMovies) == config.ModePlayable && !cfg.DisableMovieEvidenceGate
	showPlayable := cfg.ModeFor(hiddenShows) == config.ModePlayable
	if !moviePlayable && !showPlayable {
		return nil
	}
	gp := s.currentGatePolicy()
	var catGen uint64
	hasWriteGen := false
	if wg, ok := s.d.Catalog.(interface{ WriteGeneration() uint64 }); ok {
		catGen = wg.WriteGeneration()
		hasWriteGen = true
	}
	if hasWriteGen && s.checksSeeded && s.checksCatalogPtr == s.d.Catalog && s.checksCatalogGen == catGen && s.checksPolicy == gp && s.d.Watchlist == nil {
		return nil
	}

	now := s.now()
	nowTS := now.Unix()
	movieWindow := time.Duration(cfg.EffectiveMovieEvidenceDays()) * 24 * time.Hour
	var movieSince, epSince time.Time
	if moviePlayable {
		movieSince = now.Add(-movieWindow)
	}
	if showPlayable {
		epSince = now.Add(-episodeEvidence)
	}

	var candidates []core.Title
	if sched, ok := s.d.Catalog.(interface {
		ScheduledGateCandidates(context.Context, time.Time, time.Time, time.Time, time.Time) ([]core.Title, error)
	}); ok {
		var err error
		candidates, err = sched.ScheduledGateCandidates(ctx, movieSince, epSince, now.Add(30*24*time.Hour), now.Add(14*24*time.Hour))
		if err != nil {
			return err
		}
	} else {
		if moviePlayable {
			if rm, ok := s.d.Catalog.(interface {
				RecentMovies(context.Context, time.Time) ([]core.Title, error)
			}); ok {
				ms, err := rm.RecentMovies(ctx, movieSince)
				if err != nil {
					return err
				}
				candidates = append(candidates, ms...)
			}
		}
		if showPlayable {
			if re, ok := s.d.Catalog.(interface {
				RecentEvidenceEpisodes(context.Context, time.Time) ([]core.Title, error)
			}); ok {
				es, err := re.RecentEvidenceEpisodes(ctx, epSince)
				if err != nil {
					return err
				}
				candidates = append(candidates, es...)
			}
		}
	}

	if len(candidates) == 0 {
		s.checksSeeded = true
		s.checksCatalogPtr = s.d.Catalog
		s.checksCatalogGen = catGen
		s.checksPolicy = gp
		return nil
	}

	allKeys := make([]core.RatingKey, len(candidates))
	var movieKeys []core.RatingKey
	for i, t := range candidates {
		allKeys[i] = t.Key
		if t.Type == core.TypeMovie {
			movieKeys = append(movieKeys, t.Key)
		}
	}
	open := s.openGateKeys(ctx, gp, allKeys)

	homes := map[core.RatingKey]time.Time{}
	if len(movieKeys) > 0 {
		if h, ok := s.d.Catalog.(interface {
			HomeReleases(context.Context, []core.RatingKey) (map[core.RatingKey]time.Time, error)
		}); ok {
			if got, err := h.HomeReleases(ctx, movieKeys); err == nil {
				homes = got
			}
		}
	}
	watch := map[core.RatingKey]bool{}
	if s.d.Watchlist != nil {
		if w, err := s.d.Watchlist(ctx); err == nil {
			watch = w
		}
	}

	type existingRow struct {
		checked, next, anchor, windowEnd int64
		state                            string
		priority                         float64
	}
	existing := map[core.RatingKey]existingRow{}
	for start := 0; start < len(allKeys); start += 500 {
		batch := allKeys[start:min(start+500, len(allKeys))]
		args := make([]any, len(batch))
		for i, k := range batch {
			args[i] = int64(k)
		}
		rows, err := s.db.QueryContext(ctx, `SELECT rating_key,checked_at,next_check,anchor_ts,window_end_ts,state,priority FROM availability_title_checks WHERE rating_key IN (`+store.Placeholders(len(batch))+`)`, args...)
		if err != nil {
			continue
		}
		for rows.Next() {
			var k int64
			var er existingRow
			if rows.Scan(&k, &er.checked, &er.next, &er.anchor, &er.windowEnd, &er.state, &er.priority) == nil {
				existing[core.RatingKey(k)] = er
			}
		}
		rows.Close()
	}

	type upsertRow struct {
		key       core.RatingKey
		kind      string
		checked   int64
		next      int64
		anchor    int64
		windowEnd int64
		priority  float64
		state     string
	}
	inProgressMemo := map[core.RatingKey]bool{}
	revealSeasons := map[core.RatingKey]bool{}
	nextFollowed := map[core.RatingKey]core.RatingKey{}
	for _, t := range candidates {
		if t.Type == core.TypeEpisode && t.Season > 0 && t.Episode > 0 && t.Released.After(now) && (watch[t.Key] || watch[t.ShowKey]) {
			sk := releaseSeasonKey(&t)
			if s.isSeasonInProgress(ctx, sk, now, candidates, inProgressMemo) {
				// Replace per-episode 72h-early probes inside an in-progress season
				// with one season-level reveal check; keep 72h early for premieres.
				revealSeasons[sk] = true
				continue
			}
			if k := nextFollowed[t.ShowKey]; k == 0 || t.Key < k {
				nextFollowed[t.ShowKey] = t.Key
			}
		}
	}
	for sk := range revealSeasons {
		s.RevealSeason(ctx, sk)
	}
	var updates []upsertRow
	for _, t := range candidates {
		if t.Adult || t.Released.IsZero() {
			continue
		}
		if open[t.Key] {
			if ex, ok := existing[t.Key]; ok && ex.next != 0 {
				updates = append(updates, upsertRow{key: t.Key, kind: string(t.Type), checked: ex.checked, next: 0, anchor: ex.anchor, windowEnd: ex.windowEnd, priority: ex.priority, state: "found"})
			}
			continue
		}
		var anchorTS, windowEndTS int64
		basePrio := t.Popularity
		switch t.Type {
		case core.TypeMovie:
			if !moviePlayable {
				continue
			}
			windowEndTS = t.Released.Add(movieWindow).Unix()
			if windowEndTS <= nowTS {
				continue
			}
			digital := t.HomeReleased
			if digital.IsZero() {
				digital = homes[t.Key]
			}
			if !digital.IsZero() {
				anchorTS = digital.Unix()
			} else {
				anchorTS = t.Released.Unix()
			}
			if watch[t.Key] {
				basePrio += 1000
			}
			if !digital.IsZero() && !digital.After(now) {
				basePrio += 2000
			} else if !digital.IsZero() && digital.Sub(now) < 72*time.Hour {
				basePrio += 800
			}
		case core.TypeEpisode:
			if !showPlayable {
				continue
			}
			anchorTS = t.Released.Unix()
			windowEndTS = t.Released.Add(episodeEvidence).Unix()
			if windowEndTS <= nowTS {
				continue
			}
			if watch[t.Key] || (t.ShowKey != 0 && watch[t.ShowKey]) {
				if !t.Released.After(now) || nextFollowed[t.ShowKey] == t.Key {
					basePrio += 1000
				}
			}
			if !t.Released.After(now) {
				basePrio += 2000
			}
		default:
			continue
		}

		searchStart := t.Released.Unix()
		followed := t.Type == core.TypeEpisode && (watch[t.Key] || watch[t.ShowKey]) && (!t.Released.After(now) || nextFollowed[t.ShowKey] == t.Key)
		if followed {
			searchStart -= int64((72 * time.Hour).Seconds())
		}
		ex, seen := existing[t.Key]
		if !seen {
			nextAt := nowTS
			if !t.IsReleased(now) {
				nextAt = max(nowTS, searchStart)
			}
			updates = append(updates, upsertRow{
				key: t.Key, kind: string(t.Type), checked: 0, next: nextAt,
				anchor: anchorTS, windowEnd: windowEndTS, priority: basePrio + firstLookPriorityBoost,
			})
			continue
		}
		// Re-arm if a previous policy marked it found without opening the new
		// policy's gate, or if the anchor moved (e.g. digital release date
		// arrived or shifted).
		wantPrio := basePrio
		if ex.checked == 0 {
			wantPrio = basePrio + firstLookPriorityBoost
		}
		nextAt := ex.next
		state := ex.state
		if state == "found" {
			state = ""
			nextAt = nowTS
			if !t.IsReleased(now) {
				nextAt = max(nowTS, searchStart)
			}
		} else if ex.anchor != anchorTS {
			if ex.checked == 0 {
				if t.IsReleased(now) {
					nextAt = nowTS
				} else {
					nextAt = max(nowTS, searchStart)
				}
			} else if anchorTS <= nowTS && ex.checked < anchorTS {
				nextAt = nowTS
			} else {
				nextAt = nextRetryAfter(anchorTS, windowEndTS, nowTS)
			}
		}
		if followed && nowTS >= searchStart && state != "found" {
			// Re-arm exhausted schedules as well as newly followed shows.
			retry := nextEpisodeRetry(anchorTS, windowEndTS, nowTS, true)
			if retry != 0 && (nextAt == 0 || (nextAt > retry && nowTS < anchorTS)) {
				nextAt = retry
			}
		}
		if ex.anchor != anchorTS || ex.windowEnd != windowEndTS || ex.next != nextAt || ex.state != state || ex.priority != wantPrio {
			updates = append(updates, upsertRow{
				key: t.Key, kind: string(t.Type), checked: ex.checked, next: nextAt,
				anchor: anchorTS, windowEnd: windowEndTS, priority: wantPrio, state: state,
			})
		}
	}

	if len(updates) > 0 {
		dedup := make([]upsertRow, 0, len(updates))
		pos := make(map[core.RatingKey]int, len(updates))
		for _, u := range updates {
			if idx, exists := pos[u.key]; exists {
				dedup[idx] = u
				continue
			}
			pos[u.key] = len(dedup)
			dedup = append(dedup, u)
		}
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()
		const batch = 500
		for chunk := range store.Chunks(dedup, batch) {
			var sb strings.Builder
			sb.Grow(192 + len(chunk)*18)
			sb.WriteString(`INSERT INTO availability_title_checks(rating_key,kind,checked_at,next_check,anchor_ts,window_end_ts,priority,state) VALUES `)
			args := make([]any, 0, len(chunk)*8)
			for i, u := range chunk {
				if i > 0 {
					sb.WriteByte(',')
				}
				sb.WriteString("(?,?,?,?,?,?,?,?)")
				args = append(args, int64(u.key), u.kind, u.checked, u.next, u.anchor, u.windowEnd, u.priority, u.state)
			}
			sb.WriteString(` ON CONFLICT(rating_key) DO UPDATE SET kind=excluded.kind,next_check=excluded.next_check,anchor_ts=excluded.anchor_ts,window_end_ts=excluded.window_end_ts,priority=excluded.priority,state=excluded.state`)
			if _, err := tx.ExecContext(ctx, sb.String(), args...); err != nil {
				return err
			}
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}

	s.checksSeeded = true
	s.checksCatalogPtr = s.d.Catalog
	s.checksCatalogGen = catGen
	s.checksPolicy = gp
	return nil
}
