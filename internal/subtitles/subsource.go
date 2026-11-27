package subtitles

// SubSource (subsource.net) subtitle provider.
//
// Docs: https://subsource.net/api-docs
//   Base URL: https://api.subsource.net/api/v1
//   Auth: X-API-Key header (never placed in URLs or logs).
//   1. GET /movies/search
//        searchType=imdb&imdb=tt... [&season=N]
//        Fallback: searchType=text&q=<title>&type=movie|series [&year=Y] [&season=N]
//        Returns {success, data:[{movieId, title, alternateTitle, type,
//        releaseYear, imdbId, tmdbId, season, subtitleCount}]}.
//        TV series have one movieId per season.
//   2. GET /subtitles
//        movieId=<id>&language=<subsource_lang>&limit=100&sort=popular [&page=N]
//        Returns {success, data:[{subtitleId, movieId, language, releaseInfo[],
//        files, hearingImpaired, foreignParts, framerate, productionType,
//        releaseType, downloads, rating:{good,bad,total}, uploaderId,
//        contributors:[{id, displayname}]}], pagination:{page,limit,total,pages}}.
//        SubSource's releaseInfo query parameter filters strictly by exact text,
//        so Ether fetches the movie/season listing and ranks against releaseInfo[]
//        locally (and filters TV season listings to the requested episode).
//   3. GET /subtitles/{id}/download
//        Authenticated with X-API-Key. Returns a zip archive (or raw subtitle);
//        unzipSubtitleCandidate picks the matching subtitle entry.
//   Limits: Free 60/min, 1,800/hr, 7,200/day; Pro 300/min, 10,000/hr, 100,000/day.
//        Responses include X-RateLimit-* headers (X-RateLimit-Remaining,
//        X-RateLimit-Reset); 429 may also include Retry-After or JSON retryAfter.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/logredact"
	"github.com/braedonsaunders/ether/internal/releaseparse"
	"github.com/braedonsaunders/ether/internal/retryafter"
)

const (
	subsourceBaseURL         = "https://api.subsource.net/api/v1"
	subsourceMaxLimit        = 100
	subsourceMaxEpisodePages = 3
	subsourceDefaultCooldown = time.Minute
	subsourceMaxCooldown     = 24 * time.Hour
)

type subsourceMovieCacheEntry struct {
	movieID int64 // 0 means negative (title not found on SubSource)
	expires time.Time
}

// subsourceEnabled reports whether SubSource can search and download.
func (s *Service) subsourceEnabled() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.subsource.Enabled && strings.TrimSpace(s.subsource.APIKey) != "" && s.dir != ""
}

// subsourceFlexString unmarshals a JSON string, number, or null into a string.
type subsourceFlexString string

func (f *subsourceFlexString) UnmarshalJSON(b []byte) error {
	b = bytes.TrimSpace(b)
	if len(b) == 0 || bytes.Equal(b, []byte("null")) {
		*f = ""
		return nil
	}
	if b[0] == '"' {
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			return err
		}
		*f = subsourceFlexString(s)
		return nil
	}
	*f = subsourceFlexString(strings.TrimSpace(string(b)))
	return nil
}

// subsourceFlexStrings unmarshals a JSON []string, single string, or null.
type subsourceFlexStrings []string

func (f *subsourceFlexStrings) UnmarshalJSON(b []byte) error {
	b = bytes.TrimSpace(b)
	if len(b) == 0 || bytes.Equal(b, []byte("null")) {
		*f = nil
		return nil
	}
	if b[0] == '[' {
		var list []string
		if err := json.Unmarshal(b, &list); err != nil {
			return err
		}
		*f = subsourceFlexStrings(list)
		return nil
	}
	var single string
	if err := json.Unmarshal(b, &single); err != nil {
		return err
	}
	if single != "" {
		*f = subsourceFlexStrings{single}
	}
	return nil
}

// subsourceFlexBool unmarshals a JSON bool, numeric 0/1, or null.
type subsourceFlexBool bool

func (f *subsourceFlexBool) UnmarshalJSON(b []byte) error {
	b = bytes.TrimSpace(b)
	if len(b) == 0 || bytes.Equal(b, []byte("null")) {
		*f = false
		return nil
	}
	var v bool
	if err := json.Unmarshal(b, &v); err == nil {
		*f = subsourceFlexBool(v)
		return nil
	}
	var n int
	if err := json.Unmarshal(b, &n); err == nil {
		*f = subsourceFlexBool(n != 0)
		return nil
	}
	return nil
}

type subsourceMovie struct {
	MovieID        int64               `json:"movieId"`
	Title          string              `json:"title"`
	AlternateTitle string              `json:"alternateTitle"`
	Type           string              `json:"type"`
	ReleaseYear    int                 `json:"releaseYear"`
	IMDBID         string              `json:"imdbId"`
	TMDBID         subsourceFlexString `json:"tmdbId"`
	Season         int                 `json:"season"`
	SubtitleCount  int                 `json:"subtitleCount"`
}

type subsourceMoviesResponse struct {
	Success bool             `json:"success"`
	Data    []subsourceMovie `json:"data"`
}

type subsourceContributor struct {
	ID          int64  `json:"id"`
	DisplayName string `json:"displayname"`
}

type subsourceSubtitle struct {
	SubtitleID      int64                `json:"subtitleId"`
	MovieID         int64                `json:"movieId"`
	Language        string               `json:"language"`
	ReleaseInfo     subsourceFlexStrings `json:"releaseInfo"`
	Commentary      string               `json:"commentary"`
	Files           int                  `json:"files"`
	Size            int64                `json:"size"`
	HearingImpaired subsourceFlexBool    `json:"hearingImpaired"`
	ForeignParts    subsourceFlexBool    `json:"foreignParts"`
	Framerate       subsourceFlexString  `json:"framerate"`
	ProductionType  string               `json:"productionType"`
	ReleaseType     string               `json:"releaseType"`
	Downloads       int                  `json:"downloads"`
	Rating          struct {
		Good  int `json:"good"`
		Bad   int `json:"bad"`
		Total int `json:"total"`
	} `json:"rating"`
	UploaderID   int64                  `json:"uploaderId"`
	Contributors []subsourceContributor `json:"contributors"`
}

type subsourceSubtitlesResponse struct {
	Success    bool                `json:"success"`
	Data       []subsourceSubtitle `json:"data"`
	Pagination struct {
		Page  int `json:"page"`
		Limit int `json:"limit"`
		Total int `json:"total"`
		Pages int `json:"pages"`
	} `json:"pagination"`
}

// subsourceRequest builds an authenticated SubSource API request.
// The API key travels exclusively in the X-API-Key header.
func subsourceRequest(ctx context.Context, base, path, key, agent string, q url.Values) (*http.Request, error) {
	u := strings.TrimRight(base, "/") + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, logredact.TransportError(err, "subsource", nil)
	}
	req.Header.Set("X-API-Key", strings.TrimSpace(key))
	req.Header.Set("User-Agent", agent)
	req.Header.Set("Accept", "application/json")
	return req, nil
}

// subsourceMoviesSearchRequest is shared by live searches and the console
// connection test so both exercise the same authentication and endpoint.
func subsourceMoviesSearchRequest(ctx context.Context, base, key, agent string, q url.Values) (*http.Request, error) {
	return subsourceRequest(ctx, base, "/movies/search", key, agent, q)
}

func readSubSourceMoviesSearch(resp *http.Response) (subsourceMoviesResponse, error) {
	var sr subsourceMoviesResponse
	if err := checkSubSourceStatus(resp.StatusCode, "search"); err != nil {
		return sr, err
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return sr, logredact.TransportError(err, "subsource", nil)
	}
	if err := json.Unmarshal(body, &sr); err != nil {
		return sr, fmt.Errorf("subsource search decode: %w", err)
	}
	if !sr.Success {
		return sr, errors.New("subsource search: request rejected")
	}
	return sr, nil
}

func readSubSourceSubtitles(resp *http.Response) (subsourceSubtitlesResponse, error) {
	var sr subsourceSubtitlesResponse
	if err := checkSubSourceStatus(resp.StatusCode, "subtitles"); err != nil {
		return sr, err
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return sr, logredact.TransportError(err, "subsource", nil)
	}
	if err := json.Unmarshal(body, &sr); err != nil {
		return sr, fmt.Errorf("subsource subtitles decode: %w", err)
	}
	if !sr.Success {
		return sr, errors.New("subsource subtitles: request rejected")
	}
	return sr, nil
}

func checkSubSourceStatus(code int, op string) error {
	// Never include response bodies in errors.
	switch code {
	case http.StatusOK:
		return nil
	case http.StatusUnauthorized, http.StatusForbidden:
		return errors.New("SubSource rejected the API key")
	case http.StatusTooManyRequests:
		return errors.New("SubSource rate limit exceeded; try again shortly")
	default:
		return fmt.Errorf("subsource %s: HTTP %d", op, code)
	}
}

// subsourceRateLimitReset extracts the rate-limit reset time from SubSource
// response headers (X-RateLimit-Reset, Retry-After) and, on 429 responses,
// an optional JSON {"retryAfter": <seconds>} body.
func subsourceRateLimitReset(h http.Header, body []byte, now time.Time) (time.Time, bool) {
	if raw := strings.TrimSpace(h.Get("X-RateLimit-Reset")); raw != "" {
		if t, err := time.Parse(time.RFC3339Nano, raw); err == nil {
			return t, true
		}
		if t, err := time.Parse(time.RFC3339, raw); err == nil {
			return t, true
		}
		if secs, err := strconv.ParseFloat(raw, 64); err == nil && !math.IsNaN(secs) && !math.IsInf(secs, 0) {
			if secs > 1_000_000_000 {
				return time.Unix(int64(secs), 0).UTC(), true
			}
			if secs >= 0 {
				d := time.Duration(math.Ceil(secs)) * time.Second
				return now.Add(min(d, subsourceMaxCooldown)), true
			}
		}
	}
	if d, ok := retryafter.Parse(h.Get("Retry-After"), now, subsourceMaxCooldown); ok {
		return now.Add(d), true
	}
	if len(body) > 0 {
		var rl struct {
			RetryAfter *float64 `json:"retryAfter"`
		}
		if json.Unmarshal(body, &rl) == nil && rl.RetryAfter != nil &&
			!math.IsNaN(*rl.RetryAfter) && !math.IsInf(*rl.RetryAfter, 0) && *rl.RetryAfter > 0 {
			d := time.Duration(math.Ceil(*rl.RetryAfter)) * time.Second
			return now.Add(min(d, subsourceMaxCooldown)), true
		}
	}
	return time.Time{}, false
}

func (s *Service) setSubSourceBlockedUntil(until time.Time) {
	s.mu.Lock()
	if until.After(s.subsourceBlockedUntil) {
		s.subsourceBlockedUntil = until
	}
	s.mu.Unlock()
}

// subsourceDo sends a SubSource HTTP request while respecting the provider's
// X-RateLimit-* headers, Retry-After, and 429 cooldowns.
//
// Short rate limits (within s.maxWait) are waited out once inline; longer
// window resets fail fast with a temporary error so searchProviders falls
// through to the next provider without stalling playback.
func (s *Service) subsourceDo(ctx context.Context, mk func() (*http.Request, error)) (*http.Response, error) {
	for attempt := 0; ; attempt++ {
		s.mu.Lock()
		blockedUntil := s.subsourceBlockedUntil
		s.mu.Unlock()
		now := s.now()
		if now.Before(blockedUntil) {
			wait := blockedUntil.Sub(now)
			if attempt > 0 || automaticBudget(ctx) != nil || wait > s.maxWait {
				return nil, fmt.Errorf("subsource: rate limited (HTTP 429)")
			}
			t := time.NewTimer(wait)
			select {
			case <-t.C:
			case <-ctx.Done():
				t.Stop()
				return nil, ctx.Err()
			}
		}

		req, err := mk()
		if err != nil {
			return nil, err
		}
		resp, err := s.http.Do(req)
		if err != nil {
			return nil, logredact.TransportError(err, "subsource", nil)
		}

		now = s.now()
		if resp.StatusCode == http.StatusTooManyRequests {
			body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
			resp.Body.Close()
			resetAt, ok := subsourceRateLimitReset(resp.Header, body, now)
			if !ok {
				resetAt = now.Add(subsourceDefaultCooldown)
			} else if !resetAt.After(now) {
				// Server still refused past its own reset timestamp: wait at least 1s.
				resetAt = now.Add(time.Second)
			}
			if maxAt := now.Add(subsourceMaxCooldown); resetAt.After(maxAt) {
				resetAt = maxAt
			}
			s.setSubSourceBlockedUntil(resetAt)
			wait := resetAt.Sub(now)
			if attempt == 0 && automaticBudget(ctx) == nil && wait <= s.maxWait {
				t := time.NewTimer(wait)
				select {
				case <-t.C:
					continue
				case <-ctx.Done():
					t.Stop()
					return nil, ctx.Err()
				}
			}
			return nil, fmt.Errorf("subsource: rate limited (HTTP 429)")
		}

		// Proactively honour X-RateLimit-Remaining: 0 for subsequent calls.
		if rem := strings.TrimSpace(resp.Header.Get("X-RateLimit-Remaining")); rem != "" {
			if n, err := strconv.Atoi(rem); err == nil && n <= 0 {
				if resetAt, ok := subsourceRateLimitReset(resp.Header, nil, now); ok && resetAt.After(now) {
					if maxAt := now.Add(subsourceMaxCooldown); resetAt.After(maxAt) {
						resetAt = maxAt
					}
					s.setSubSourceBlockedUntil(resetAt)
				}
			}
		}

		return resp, nil
	}
}

func normalizeIMDBID(raw string) string {
	s := strings.ToLower(strings.TrimSpace(raw))
	if s == "" {
		return ""
	}
	if strings.HasPrefix(s, "tt") {
		if len(s) <= 2 {
			return ""
		}
		return s
	}
	if _, err := strconv.ParseInt(s, 10, 64); err == nil {
		return "tt" + s
	}
	return ""
}

func parsePositiveInt64(raw string) int64 {
	n, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
	if err != nil || n <= 0 {
		return 0
	}
	return n
}

func isSubSourceEpisode(title *core.Title) bool {
	return title.Type == core.TypeEpisode || (title.Type == "" && title.Key.Kind() == core.KindEpisode)
}

func subsourceTitleIdentity(title *core.Title) (imdbID string, tmdbID int64, season, episode int, queryTitle string) {
	if isSubSourceEpisode(title) {
		imdbID = normalizeIMDBID(title.ShowIDs.IMDB)
		tmdbID = title.ShowIDs.TMDB
		if tmdbID == 0 && title.Key.Kind() == core.KindEpisode {
			tmdbID = title.Key.TMDB()
		}
		season, episode = title.Season, title.Episode
		if season == 0 && title.Key.Kind() == core.KindEpisode {
			season, episode = title.Key.Season(), title.Key.Episode()
		}
		for _, candidate := range []string{title.ShowTitle, title.ShowEnglishTitle, title.ShowOriginalTitle} {
			if s := strings.TrimSpace(candidate); s != "" {
				queryTitle = s
				break
			}
		}
		return
	}
	imdbID = normalizeIMDBID(title.IDs.IMDB)
	tmdbID = title.IDs.TMDB
	if tmdbID == 0 && title.Key.Kind() == core.KindMovie {
		tmdbID = title.Key.TMDB()
	}
	for _, candidate := range []string{title.Title, title.EnglishTitle, title.OriginalTitle} {
		if s := strings.TrimSpace(candidate); s != "" {
			queryTitle = s
			break
		}
	}
	return
}

func subsourceWantTitles(title *core.Title) []string {
	var raw []string
	if isSubSourceEpisode(title) {
		raw = append(raw, title.ShowTitle, title.ShowEnglishTitle, title.ShowOriginalTitle)
		raw = append(raw, title.ShowAlternativeTitles...)
	} else {
		raw = append(raw, title.Title, title.EnglishTitle, title.OriginalTitle)
		raw = append(raw, title.AlternativeTitles...)
	}
	seen := map[string]bool{}
	var out []string
	for _, s := range raw {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		k := strings.ToLower(s)
		if seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, s)
	}
	return out
}

var subsourceSeasonSuffixRe = regexp.MustCompile(`(?i)\s*(?:[-–—:]\s*)?(?:season\s*\d+|s\d{1,2}|(?:first|second|third|fourth|fifth|sixth|seventh|eighth|ninth|tenth|\d+(?:st|nd|rd|th))\s+season)\s*$`)

func subsourceItemTitlesMatch(m subsourceMovie, wantTitles []string, isEpisode bool) bool {
	cands := make([]string, 0, 4)
	for _, raw := range []string{m.Title, m.AlternateTitle} {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		cands = append(cands, raw)
		if isEpisode {
			if stripped := strings.TrimSpace(subsourceSeasonSuffixRe.ReplaceAllString(raw, "")); stripped != "" && stripped != raw {
				cands = append(cands, stripped)
			}
		}
	}
	for _, cand := range cands {
		for _, want := range wantTitles {
			if releaseparse.TitlesMatch(cand, want) {
				return true
			}
		}
	}
	return false
}

func subsourceYearStrictMatch(m subsourceMovie, title *core.Title, wantSeason int) bool {
	if isSubSourceEpisode(title) {
		showYear, epYear := title.ShowYear, title.Year
		if showYear == 0 && epYear == 0 {
			return true
		}
		if m.ReleaseYear == 0 {
			return false
		}
		if showYear > 0 && m.ReleaseYear == showYear {
			return true
		}
		if epYear > 0 && m.ReleaseYear == epYear {
			return true
		}
		// SubSource may tag Season N with that season's air year when we only
		// know the show's premiere year.
		if wantSeason > 1 && showYear > 0 && epYear == 0 &&
			m.ReleaseYear >= showYear && m.ReleaseYear <= showYear+wantSeason+5 {
			return true
		}
		return false
	}
	if title.Year == 0 {
		return true
	}
	return m.ReleaseYear == title.Year
}

// pickSubSourceMovie selects the best matching SubSource movie/season record.
// Conflicting IMDb/TMDB IDs, wrong content types, or wrong seasons are rejected;
// without an ID match, strict title and year matching is required.
func pickSubSourceMovie(items []subsourceMovie, title *core.Title, wantSeason int, wantIMDB string, wantTMDB int64) int64 {
	isEpisode := isSubSourceEpisode(title)
	wantTitles := subsourceWantTitles(title)

	var bestID int64
	bestScore := 0
	bestSubs := -1

	for _, m := range items {
		if m.MovieID <= 0 {
			continue
		}
		mType := strings.ToLower(strings.TrimSpace(m.Type))
		isSeriesItem := mType == "tvseries" || mType == "series"
		isMovieItem := mType == "movie"
		if isEpisode {
			if isMovieItem && m.Season == 0 {
				continue
			}
			if wantSeason > 0 && m.Season > 0 && m.Season != wantSeason {
				continue
			}
		} else {
			if isSeriesItem || m.Season > 0 {
				continue
			}
		}

		gotIMDB := normalizeIMDBID(m.IMDBID)
		gotTMDB := parsePositiveInt64(string(m.TMDBID))
		if wantIMDB != "" && gotIMDB != "" && wantIMDB != gotIMDB {
			continue
		}
		if wantTMDB > 0 && gotTMDB > 0 && wantTMDB != gotTMDB {
			continue
		}

		imdbMatch := wantIMDB != "" && gotIMDB == wantIMDB
		tmdbMatch := wantTMDB > 0 && gotTMDB == wantTMDB

		score := 0
		switch {
		case imdbMatch || tmdbMatch:
			if isEpisode && wantSeason > 0 && m.Season != wantSeason {
				if m.Season == 0 {
					score = 50
				} else {
					continue
				}
			} else {
				if imdbMatch {
					score += 100
				}
				if tmdbMatch {
					score += 90
				}
			}
		default:
			if isEpisode && wantSeason > 0 && m.Season != wantSeason {
				continue
			}
			if len(wantTitles) == 0 || !subsourceItemTitlesMatch(m, wantTitles, isEpisode) {
				continue
			}
			if !subsourceYearStrictMatch(m, title, wantSeason) {
				continue
			}
			score = 60
		}

		if score > bestScore ||
			(score == bestScore && m.SubtitleCount > bestSubs) ||
			(score == bestScore && m.SubtitleCount == bestSubs && (bestID == 0 || m.MovieID < bestID)) {
			bestID = m.MovieID
			bestScore = score
			bestSubs = m.SubtitleCount
		}
	}
	return bestID
}

func (s *Service) resolveSubSourceMovieID(ctx context.Context, title *core.Title, key string) (int64, error) {
	imdbID, tmdbID, season, _, queryTitle := subsourceTitleIdentity(title)
	cacheKey := fmt.Sprintf("type=%s|imdb=%s|tmdb=%d|q=%s|sy=%d|y=%d|s=%d",
		title.Type, imdbID, tmdbID, strings.ToLower(queryTitle), title.ShowYear, title.Year, season)

	s.mu.Lock()
	if e, ok := s.subsourceMovies[cacheKey]; ok {
		if s.now().Before(e.expires) {
			s.mu.Unlock()
			return e.movieID, nil
		}
		delete(s.subsourceMovies, cacheKey)
	}
	s.mu.Unlock()

	search := func(q url.Values) ([]subsourceMovie, error) {
		resp, err := s.subsourceDo(ctx, func() (*http.Request, error) {
			return subsourceMoviesSearchRequest(ctx, s.subsourceBase, key, s.agent, q)
		})
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		sr, err := readSubSourceMoviesSearch(resp)
		if err != nil {
			return nil, err
		}
		return sr.Data, nil
	}

	isEpisode := isSubSourceEpisode(title)
	var movieID int64
	// 1. Prefer IMDb ID search when available.
	if imdbID != "" {
		q := url.Values{
			"searchType": {"imdb"},
			"imdb":       {imdbID},
		}
		if isEpisode && season > 0 {
			q.Set("season", strconv.Itoa(season))
		}
		items, err := search(q)
		if err != nil {
			return 0, err
		}
		movieID = pickSubSourceMovie(items, title, season, imdbID, tmdbID)
	}

	// 2. Fallback to text search with strict TMDB / title+year matching.
	if movieID == 0 && queryTitle != "" {
		q := url.Values{
			"searchType": {"text"},
			"q":          {queryTitle},
		}
		if isEpisode {
			q.Set("type", "series")
			if season > 0 {
				q.Set("season", strconv.Itoa(season))
			}
		} else {
			q.Set("type", "movie")
			if title.Year > 0 {
				q.Set("year", strconv.Itoa(title.Year))
			}
		}
		items, err := search(q)
		if err != nil {
			return 0, err
		}
		movieID = pickSubSourceMovie(items, title, season, imdbID, tmdbID)
	}

	if ctx.Err() == nil {
		s.mu.Lock()
		now := s.now()
		if s.subsourceMovies == nil {
			s.subsourceMovies = map[string]subsourceMovieCacheEntry{}
		}
		for k, e := range s.subsourceMovies {
			if !now.Before(e.expires) {
				delete(s.subsourceMovies, k)
			}
		}
		if len(s.subsourceMovies) >= 4096 {
			var oldest string
			expiry := now.Add(s.negTTL + time.Hour)
			for k, e := range s.subsourceMovies {
				if e.expires.Before(expiry) {
					oldest, expiry = k, e.expires
				}
			}
			delete(s.subsourceMovies, oldest)
		}
		s.subsourceMovies[cacheKey] = subsourceMovieCacheEntry{
			movieID: movieID,
			expires: now.Add(s.negTTL),
		}
		s.mu.Unlock()
	}

	return movieID, nil
}

// subsourceMatchesEpisode reports whether a SubSource subtitle entry belongs to
// the requested TV episode and is not a full-season or multi-episode pack.
func subsourceMatchesEpisode(releases []string, files, season, episode int) bool {
	if len(releases) == 0 {
		return false
	}
	matchedTarget := false
	hasOtherEpisode := false
	for _, rel := range releases {
		rel = strings.TrimSpace(rel)
		if rel == "" {
			continue
		}
		parsed := releaseparse.Parse(rel)
		if parsed.FullSeason {
			return false
		}
		if len(parsed.Episodes) > 2 {
			return false
		}
		if releaseparse.EpisodeMatches(parsed, season, episode) {
			matchedTarget = true
		} else if len(parsed.Episodes) > 0 {
			hasOtherEpisode = true
		}
	}
	if !matchedTarget {
		return false
	}
	// Multi-file archive referencing other episodes is a batch pack;
	// unzipSubtitleCandidate does not filter archive members by episode.
	if files > 1 && hasOtherEpisode {
		return false
	}
	return true
}

func subsourceBestRelease(releases []string, releaseName string, subtitleID int64) string {
	var best string
	bestScore := -1
	for _, rel := range releases {
		rel = strings.TrimSpace(rel)
		if rel == "" {
			continue
		}
		if best == "" {
			best = rel
		}
		if releaseName == "" {
			continue
		}
		score := releaseMatch(releaseName, Candidate{Release: rel})
		if score > bestScore {
			bestScore = score
			best = rel
		}
	}
	if best == "" {
		return fmt.Sprintf("subsource-%d", subtitleID)
	}
	return best
}

func parseSubSourceFPS(raw string) float64 {
	raw = strings.TrimSpace(raw)
	if raw == "" || strings.EqualFold(raw, "n/a") {
		return 0
	}
	v, err := strconv.ParseFloat(strings.ReplaceAll(raw, ",", "."), 64)
	if err != nil || v <= 0 || math.IsNaN(v) || math.IsInf(v, 0) {
		return 0
	}
	return canonicalFPS(v)
}

func subsourceRating(good, total int) float64 {
	if total <= 0 || good < 0 {
		return 0
	}
	return 10.0 * float64(good) / float64(total)
}

func subsourceUploader(id int64, contributors []subsourceContributor) string {
	for _, c := range contributors {
		if id != 0 && c.ID == id && strings.TrimSpace(c.DisplayName) != "" {
			return strings.TrimSpace(c.DisplayName)
		}
	}
	return ""
}

// subsourceFileID derives a stable numeric id from a SubSource subtitleId in
// its own namespace so offers never collide with OpenSubtitles or SubDL IDs.
func subsourceFileID(subtitleID int64) int64 {
	p := "subsource:" + strconv.FormatInt(subtitleID, 10)
	var h uint64 = 1469598103934665603
	for i := 0; i < len(p); i++ {
		h ^= uint64(p[i])
		h *= 1099511628211
	}
	if h == 0 {
		return 1
	}
	return int64(h & 0x7fffffffffffffff)
}

// searchSubSource searches SubSource and maps its results onto Candidates.
func (s *Service) searchSubSource(ctx context.Context, title *core.Title, lang, releaseName string) ([]Candidate, error) {
	if title == nil {
		return nil, fmt.Errorf("nil title")
	}
	queryLangs := subsourceQueryLangs(lang)
	if len(queryLangs) == 0 {
		return nil, nil
	}

	imdbID, tmdbID, season, episode, queryTitle := subsourceTitleIdentity(title)
	cacheKey := fmt.Sprintf("subsource:type=%s|imdb=%s|tmdb=%d|q=%s|y=%d|s=%d|e=%d|langs=%s",
		title.Type, imdbID, tmdbID, strings.ToLower(queryTitle), title.Year, season, episode,
		providerLanguages(lang, func(l string) string { return l }))

	s.mu.Lock()
	until, missed := s.negative[cacheKey]
	if missed && !s.now().Before(until) {
		delete(s.negative, cacheKey)
		missed = false
	}
	key := s.subsource.APIKey
	s.mu.Unlock()
	if missed {
		return nil, nil
	}

	movieID, err := s.resolveSubSourceMovieID(ctx, title, key)
	if err != nil {
		return nil, err
	}
	if movieID == 0 {
		s.recordSubSourceNegative(ctx, cacheKey)
		return nil, nil
	}

	var out []Candidate
	var firstErr error
	seenIDs := map[int64]bool{}
	isEpisode := isSubSourceEpisode(title)
	maxPages := 1
	if isEpisode && season > 0 && episode > 0 {
		maxPages = subsourceMaxEpisodePages
	}

	for _, ssLang := range queryLangs {
		langMatches := 0
		for page := 1; page <= maxPages; page++ {
			q := url.Values{
				"movieId":  {strconv.FormatInt(movieID, 10)},
				"language": {ssLang},
				"limit":    {strconv.Itoa(subsourceMaxLimit)},
				"sort":     {"popular"},
			}
			if page > 1 {
				q.Set("page", strconv.Itoa(page))
			}
			resp, err := s.subsourceDo(ctx, func() (*http.Request, error) {
				return subsourceRequest(ctx, s.subsourceBase, "/subtitles", key, s.agent, q)
			})
			if err != nil {
				if firstErr == nil {
					firstErr = err
				}
				break
			}
			sr, err := readSubSourceSubtitles(resp)
			resp.Body.Close()
			if err != nil {
				if firstErr == nil {
					firstErr = err
				}
				break
			}
			for _, d := range sr.Data {
				if d.SubtitleID <= 0 || seenIDs[d.SubtitleID] {
					continue
				}
				if strings.EqualFold(strings.TrimSpace(d.ReleaseType), "trailer") {
					continue
				}
				actual := subsourceParseLang(d.Language)
				if !matchesSearchLanguages(lang, actual) {
					continue
				}
				releases := make([]string, 0, len(d.ReleaseInfo))
				for _, r := range d.ReleaseInfo {
					if r = strings.TrimSpace(r); r != "" {
						releases = append(releases, r)
					}
				}
				if isEpisode && season > 0 && episode > 0 {
					if !subsourceMatchesEpisode(releases, d.Files, season, episode) {
						continue
					}
				}
				seenIDs[d.SubtitleID] = true
				langMatches++
				bestRel := subsourceBestRelease(releases, releaseName, d.SubtitleID)
				hi := bool(d.HearingImpaired)
				forced := (bool(d.ForeignParts) || strings.EqualFold(strings.TrimSpace(d.ProductionType), "forced")) && !hi
				ai := strings.EqualFold(strings.TrimSpace(d.ProductionType), "machine")
				out = append(out, Candidate{
					FileID:        subsourceFileID(d.SubtitleID),
					FileName:      bestRel,
					Language:      actual,
					Release:       bestRel,
					Releases:      releases,
					FPS:           parseSubSourceFPS(string(d.Framerate)),
					DownloadCount: d.Downloads,
					Ratings:       subsourceRating(d.Rating.Good, d.Rating.Total),
					Uploader:      subsourceUploader(d.UploaderID, d.Contributors),
					HearingImp:    hi,
					AITranslated:  ai,
					Forced:        forced,
					Provider:      "SubSource",
					ProviderID:    config.SubtitleProviderSubSource,
					DownloadURL:   s.subsourceBase + "/subtitles/" + strconv.FormatInt(d.SubtitleID, 10) + "/download",
				})
			}
			if langMatches > 0 || sr.Pagination.Pages <= page || len(sr.Data) < subsourceMaxLimit {
				break
			}
		}
	}

	if len(out) == 0 && firstErr != nil {
		return nil, firstErr
	}
	if len(out) == 0 {
		s.recordSubSourceNegative(ctx, cacheKey)
	}
	return out, nil
}

func (s *Service) recordSubSourceNegative(ctx context.Context, cacheKey string) {
	if ctx.Err() != nil {
		return
	}
	s.mu.Lock()
	now := s.now()
	for k, until := range s.negative {
		if !now.Before(until) {
			delete(s.negative, k)
		}
	}
	if len(s.negative) >= 4096 {
		var oldest string
		expiry := now.Add(s.negTTL + time.Hour)
		for k, until := range s.negative {
			if until.Before(expiry) {
				oldest, expiry = k, until
			}
		}
		delete(s.negative, oldest)
	}
	s.negative[cacheKey] = now.Add(s.negTTL)
	s.mu.Unlock()
}

// downloadSubSource fetches and unzips a SubSource candidate using X-API-Key.
func (s *Service) downloadSubSource(ctx context.Context, c Candidate) ([]byte, error) {
	if c.DownloadURL == "" {
		return nil, fmt.Errorf("subsource: no download url")
	}
	s.mu.Lock()
	key := s.subsource.APIKey
	s.mu.Unlock()
	resp, err := s.subsourceDo(ctx, func() (*http.Request, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.DownloadURL, nil)
		if err != nil {
			return nil, logredact.TransportError(err, "subsource", nil)
		}
		req.Header.Set("X-API-Key", strings.TrimSpace(key))
		req.Header.Set("User-Agent", s.agent)
		return req, nil
	})
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if err := checkSubSourceStatus(resp.StatusCode, "download"); err != nil {
		return nil, err
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxSubtitleBytes+1))
	if err != nil {
		return nil, logredact.TransportError(err, "subsource", nil)
	}
	if len(raw) > maxSubtitleBytes {
		return nil, fmt.Errorf("subtitle download too large")
	}
	return unzipSubtitleCandidate(raw, c)
}

// subsourceLangByTag maps normalized Ether language tags (from searchLanguage)
// to SubSource API v1 language query codes.
var subsourceLangByTag = map[string]string{
	"ab":    "abkhazian",
	"af":    "afrikaans",
	"am":    "amharic",
	"an":    "aragonese",
	"ar":    "arabic",
	"as":    "assamese",
	"ast":   "asturian",
	"az":    "azerbaijani",
	"be":    "belarusian",
	"bg":    "bulgarian",
	"bn":    "bengali",
	"br":    "breton",
	"bs":    "bosnian",
	"ca":    "catalan",
	"cs":    "czech",
	"cy":    "welsh",
	"da":    "danish",
	"de":    "german",
	"el":    "greek",
	"en":    "english",
	"eo":    "espranto",
	"es":    "spanish",
	"et":    "estonian",
	"eu":    "basque",
	"fa":    "farsi_persian",
	"fi":    "finnish",
	"fil":   "filipino",
	"fr":    "french",
	"ga":    "irish",
	"gd":    "gaelic",
	"gl":    "gaelician",
	"he":    "hebrew",
	"hi":    "hindi",
	"hr":    "croatian",
	"hu":    "hungarian",
	"hy":    "armenian",
	"ia":    "interlingua",
	"id":    "indonesian",
	"ig":    "igbo",
	"is":    "icelandic",
	"it":    "italian",
	"ja":    "japanese",
	"ka":    "georgian",
	"kk":    "kazakh",
	"kl":    "greenlandic",
	"km":    "khmer",
	"kn":    "kannada",
	"ko":    "korean",
	"ku":    "kurdish",
	"ky":    "kyrgyz",
	"lb":    "luxembourgish",
	"lt":    "lithuanian",
	"lv":    "latvian",
	"mk":    "macedonian",
	"ml":    "malayalam",
	"mn":    "mongolian",
	"mr":    "marathi",
	"ms":    "malay",
	"my":    "burmese",
	"nb":    "norwegian",
	"ne":    "nepali",
	"nl":    "dutch",
	"nn":    "norwegian",
	"no":    "norwegian",
	"nv":    "navajo",
	"oc":    "occitan",
	"or":    "odia",
	"pl":    "polish",
	"prs":   "dari",
	"ps":    "pashto",
	"pt":    "portuguese",
	"pt-br": "brazilian_portuguese",
	"pt-pt": "portuguese",
	"ro":    "romanian",
	"ru":    "russian",
	"sd":    "sindhi",
	"se":    "northen_sami",
	"si":    "sinhala",
	"sk":    "slovak",
	"sl":    "slovenian",
	"so":    "somali",
	"sq":    "albanian",
	"sr":    "serbian",
	"sv":    "swedish",
	"sw":    "swahili",
	"ta":    "tamil",
	"te":    "telugu",
	"th":    "thai",
	"tk":    "turkmen",
	"tl":    "tagalog",
	"tr":    "turkish",
	"tt":    "tatar",
	"uk":    "ukrainian",
	"ur":    "urdu",
	"uz":    "uzbek",
	"vi":    "vietnamese",
	// SubSource files Chinese under the encoding names, not "chinese_simplified"
	// or "chinese_traditional" (a /subtitles query with those returns nothing).
	"zh":    "chinese_bg_code",
	"zh-cn": "chinese_bg_code",
	"zh-hk": "big_5_code",
	"zh-mo": "big_5_code",
	"zh-sg": "chinese_bg_code",
	"zh-tw": "big_5_code",
}

// subsourceTagByName maps SubSource response language codes and aliases back
// to normalized Ether BCP-47 tags.
var subsourceTagByName = func() map[string]string {
	m := map[string]string{
		"big_5_code":            "zh-tw",
		"brazillian_portuguese": "pt-br",
		"brazilian_portuguese":  "pt-br",
		"chinese":               "zh",
		"chinese_bg_code":       "zh-cn",
		"chinese_bilingual":     "zh",
		"chinese_cantonese":     "zh-tw",
		"chinese_simplified":    "zh-cn",
		"chinese_traditional":   "zh-tw",
		"esperanto":             "eo",
		"espranto":              "eo",
		"farsi_persian":         "fa",
		"farsi/persian":         "fa",
		"filipino":              "tl",
		"french_canada":         "fr",
		"french_france":         "fr",
		"gaelician":             "gl",
		"galician":              "gl",
		"montenegrin":           "sr",
		"northern_sami":         "se",
		"northen_sami":          "se",
		"portuguese":            "pt-pt",
		"pushto":                "ps",
		"sinhalese":             "si",
		"spanish_latin_america": "es",
		"spanish_spain":         "es",
	}
	for tag, code := range subsourceLangByTag {
		if _, exists := m[code]; !exists {
			if tag == "nb" || tag == "nn" {
				m[code] = "no"
				continue
			}
			if tag == "pt" {
				m[code] = "pt-pt"
				continue
			}
			if tag == "zh" || tag == "zh-sg" {
				m[code] = "zh-cn"
				continue
			}
			if tag == "zh-hk" || tag == "zh-mo" {
				m[code] = "zh-tw"
				continue
			}
			m[code] = tag
		}
	}
	return m
}()

// subsourceQueryLangs maps a comma-separated Ether language list to
// deduplicated SubSource API v1 language query codes in preference order.
func subsourceQueryLangs(raw string) []string {
	var out []string
	seen := map[string]bool{}
	for _, l := range searchLanguages(raw) {
		code := subsourceLangByTag[l]
		if code == "" {
			if base, _, ok := strings.Cut(l, "-"); ok {
				code = subsourceLangByTag[base]
			}
		}
		if code != "" && !seen[code] {
			seen[code] = true
			out = append(out, code)
		}
	}
	return out
}

// subsourceParseLang normalizes a SubSource response language value into an
// Ether language tag compatible with matchesSearchLanguages.
func subsourceParseLang(raw string) string {
	s := strings.ToLower(strings.TrimSpace(raw))
	if s == "" {
		return ""
	}
	underscored := strings.ReplaceAll(strings.ReplaceAll(s, " ", "_"), "-", "_")
	if tag, ok := subsourceTagByName[underscored]; ok {
		return tag
	}
	if tag, ok := subsourceTagByName[s]; ok {
		return tag
	}
	return searchLanguage(strings.ReplaceAll(s, "_", " "))
}
