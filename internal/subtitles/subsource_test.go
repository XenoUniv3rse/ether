package subtitles

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
)

func makeSubSourceZip(name, text string) []byte {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	f, _ := zw.Create(name)
	_, _ = f.Write([]byte(text))
	_ = zw.Close()
	return buf.Bytes()
}

// TestSubSourceSearchDownloadRank verifies X-API-Key header auth (never in URL),
// IMDb title lookup, subtitle listing + release-name ranking, and authenticated zip download.
func TestSubSourceSearchDownloadRank(t *testing.T) {
	const secretKey = "subsource-super-secret-key"
	subText := "1\n00:00:01,000 --> 00:00:02,000\nBonjour tout le monde\n\n"

	mux := http.NewServeMux()
	mux.HandleFunc("/movies/search", func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.RawQuery, secretKey) {
			t.Errorf("API key leaked into URL query: %s", r.URL.RawQuery)
		}
		if r.Header.Get("X-API-Key") != secretKey {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		q := r.URL.Query()
		if q.Get("searchType") == "text" && q.Get("q") == "inception" {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"success": true,
				"data": []any{map[string]any{
					"movieId": 1001, "title": "Inception", "type": "movie", "releaseYear": 2010, "imdbId": "tt1375666",
				}},
			})
			return
		}
		if q.Get("searchType") != "imdb" || q.Get("imdb") != "tt1375666" {
			t.Errorf("unexpected movie search query: %s", r.URL.RawQuery)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"success": true,
			"data": []any{map[string]any{
				"movieId":       1001,
				"title":         "Inception",
				"type":          "movie",
				"releaseYear":   2010,
				"imdbId":        "tt1375666",
				"tmdbId":        "27205",
				"subtitleCount": 42,
			}},
		})
	})
	mux.HandleFunc("/subtitles", func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.RawQuery, secretKey) {
			t.Errorf("API key leaked into URL query: %s", r.URL.RawQuery)
		}
		if r.Header.Get("X-API-Key") != secretKey {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		q := r.URL.Query()
		if q.Get("movieId") != "1001" || q.Get("language") != "french" {
			t.Errorf("unexpected subtitles query: %s", r.URL.RawQuery)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"success": true,
			"data": []any{
				map[string]any{
					"subtitleId":      9002,
					"movieId":         1001,
					"language":        "french",
					"releaseInfo":     []string{"Some.Other.Edition.720p"},
					"files":           1,
					"hearingImpaired": true,
					"foreignParts":    false,
					"framerate":       "23.976",
					"downloads":       500,
					"rating":          map[string]any{"good": 9, "bad": 1, "total": 10},
				},
				map[string]any{
					"subtitleId":      9001,
					"movieId":         1001,
					"language":        "french",
					"releaseInfo":     []string{"Inception.2010.720p.BluRay", "Inception.2010.1080p.BluRay.x264-GRP"},
					"files":           1,
					"hearingImpaired": false,
					"foreignParts":    false,
					"framerate":       "23.976",
					"downloads":       120,
					"rating":          map[string]any{"good": 10, "bad": 0, "total": 10},
					"uploaderId":      7,
					"contributors":    []any{map[string]any{"id": 7, "displayname": "cinephile"}},
				},
			},
			"pagination": map[string]any{"page": 1, "limit": 100, "total": 2, "pages": 1},
		})
	})
	mux.HandleFunc("/subtitles/9001/download", func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.RawQuery, secretKey) {
			t.Errorf("API key leaked into download URL: %s", r.URL.RawQuery)
		}
		if r.Header.Get("X-API-Key") != secretKey {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		_, _ = w.Write(makeSubSourceZip("Inception.2010.1080p.BluRay.x264-GRP.fr.srt", subText))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	if err := testSubSource(context.Background(), srv.Client(), srv.URL, config.SubSource{APIKey: secretKey}); err != nil {
		t.Fatalf("connection test: %v", err)
	}
	if err := testSubSource(context.Background(), srv.Client(), srv.URL, config.SubSource{APIKey: "wrong-key"}); err == nil {
		t.Fatal("expected bad key error")
	} else if strings.Contains(err.Error(), "wrong-key") {
		t.Fatalf("error leaked API key: %v", err)
	}

	svc := New(Options{
		SubSource:    config.SubSource{Enabled: true, APIKey: secretKey},
		CacheDir:     t.TempDir(),
		SubSourceURL: srv.URL,
	})
	if svc.SearchNeedsHash() {
		t.Fatal("SubSource-only configuration must not request media hashing")
	}

	title := &core.Title{
		Key:   core.MovieKey(27205),
		Type:  core.TypeMovie,
		Title: "Inception",
		Year:  2010,
		IDs:   core.IDs{TMDB: 27205, IMDB: "tt1375666"},
	}

	cands, err := svc.SearchOnDemand(context.Background(), title, "fr", "Inception.2010.1080p.BluRay.x264-GRP.mkv")
	if err != nil || len(cands) != 2 {
		t.Fatalf("search: %+v %v", cands, err)
	}
	if cands[0].Provider != "SubSource" || cands[0].ProviderID != config.SubtitleProviderSubSource ||
		cands[0].Release != "Inception.2010.1080p.BluRay.x264-GRP" || cands[0].Uploader != "cinephile" {
		t.Fatalf("top candidate: %+v", cands[0])
	}

	st, err := svc.DownloadOnDemand(context.Background(), title, cands[0].FileID)
	if err != nil {
		t.Fatal(err)
	}
	if st.Language != "fr" || st.ProviderTitle != "SubSource" {
		t.Fatalf("pick: %+v", st)
	}
	rc, format, err := svc.OpenFormat(st.ExternalURL, "srt")
	if err != nil || format != "srt" {
		t.Fatalf("open: %v %s", err, format)
	}
	defer rc.Close()
	got := make([]byte, 256)
	n, _ := rc.Read(got)
	if !bytes.Contains(got[:n], []byte("Bonjour tout le monde")) {
		t.Fatalf("downloaded body: %q", got[:n])
	}
}

// TestSubSourceStrictTitleFallbackAndEpisodes verifies:
//   - Movies without IMDb ID fall back to text search with strict title+year / TMDB matching
//     and reject remakes, conflicting IDs, and TV entries.
//   - Episodes match the target season and filter out wrong episodes, season packs, and trailers.
func TestSubSourceStrictTitleFallbackAndEpisodes(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/movies/search", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		switch {
		case q.Get("searchType") == "text" && q.Get("q") == "Dune":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"success": true,
				"data": []any{
					// Wrong year remake — must be rejected.
					map[string]any{"movieId": 10, "title": "Dune", "type": "movie", "releaseYear": 1984, "subtitleCount": 999},
					// Conflicting TMDB ID with matching year — must be rejected.
					map[string]any{"movieId": 11, "title": "Dune", "type": "movie", "releaseYear": 2021, "tmdbId": "999999", "subtitleCount": 500},
					// Wrong type (TV series) — must be rejected.
					map[string]any{"movieId": 12, "title": "Dune", "type": "tvseries", "releaseYear": 2021, "season": 1, "subtitleCount": 500},
					// Exact match.
					map[string]any{"movieId": 13, "title": "Dune", "type": "movie", "releaseYear": 2021, "tmdbId": "438631", "subtitleCount": 50},
				},
			})
		case q.Get("searchType") == "text" && q.Get("q") == "No Match Movie":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"success": true,
				"data": []any{
					// Different year — strict match rejects.
					map[string]any{"movieId": 20, "title": "No Match Movie", "type": "movie", "releaseYear": 2019},
					// Subtitle/extra words — strict match rejects.
					map[string]any{"movieId": 21, "title": "No Match Movie Returns", "type": "movie", "releaseYear": 2024},
				},
			})
		case q.Get("searchType") == "imdb" && q.Get("imdb") == "tt11280740":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"success": true,
				"data": []any{
					// Wrong season — must be rejected.
					map[string]any{"movieId": 301, "title": "Severance", "type": "tvseries", "imdbId": "tt11280740", "season": 1},
					// Right season.
					map[string]any{"movieId": 302, "title": "Severance", "type": "tvseries", "imdbId": "tt11280740", "season": 2},
				},
			})
		default:
			_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "data": []any{}})
		}
	})
	mux.HandleFunc("/subtitles", func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("movieId") {
		case "13":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"success": true,
				"data": []any{
					map[string]any{"subtitleId": 1301, "movieId": 13, "language": "english", "releaseInfo": []string{"Dune.2021.2160p.WEB-DL"}},
				},
			})
		case "302":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"success": true,
				"data": []any{
					// Trailer — rejected.
					map[string]any{"subtitleId": 3200, "movieId": 302, "language": "english", "releaseType": "trailer", "releaseInfo": []string{"Severance.S02E03.Trailer"}},
					// Wrong episode — rejected.
					map[string]any{"subtitleId": 3201, "movieId": 302, "language": "english", "files": 1, "releaseInfo": []string{"Severance.S02E01.1080p.ATVP.WEB-DL"}},
					// Full season pack — rejected.
					map[string]any{"subtitleId": 3202, "movieId": 302, "language": "english", "files": 10, "releaseInfo": []string{"Severance.S02.1080p.ATVP.WEB-DL"}},
					// Multi-episode batch pack — rejected.
					map[string]any{"subtitleId": 3203, "movieId": 302, "language": "english", "files": 3, "releaseInfo": []string{"Severance.S02E01.WEB", "Severance.S02E02.WEB", "Severance.S02E03.WEB"}},
					// Exact episode match — accepted.
					map[string]any{"subtitleId": 3204, "movieId": 302, "language": "english", "files": 1, "releaseInfo": []string{"Severance.S02E03.1080p.ATVP.WEB-DL"}},
				},
			})
		default:
			t.Errorf("unexpected subtitles lookup for movieId=%s", r.URL.Query().Get("movieId"))
			_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "data": []any{}})
		}
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	svc := New(Options{
		SubSource:    config.SubSource{Enabled: true, APIKey: "key"},
		CacheDir:     t.TempDir(),
		SubSourceURL: srv.URL,
	})
	ctx := context.Background()

	// Movie without IMDb ID matches movieId=13 via text search + strict matching.
	dune := &core.Title{Key: core.MovieKey(438631), Type: core.TypeMovie, Title: "Dune", Year: 2021, IDs: core.IDs{TMDB: 438631}}
	cands, err := svc.SearchOnDemand(ctx, dune, "en", "Dune.2021.2160p.WEB-DL.mkv")
	if err != nil || len(cands) != 1 || cands[0].Release != "Dune.2021.2160p.WEB-DL" {
		t.Fatalf("dune text fallback: %+v %v", cands, err)
	}

	// Non-matching year/title yields no false positive.
	nomatch := &core.Title{Key: core.MovieKey(99999), Type: core.TypeMovie, Title: "No Match Movie", Year: 2024}
	cands, err = svc.SearchOnDemand(ctx, nomatch, "en", "")
	if err != nil || len(cands) != 0 {
		t.Fatalf("expected strict rejection, got %+v %v", cands, err)
	}

	// Episode matches season 2 and keeps only S02E03.
	ep := &core.Title{
		Key:       core.EpisodeKey(95396, 2, 3),
		Type:      core.TypeEpisode,
		ShowTitle: "Severance",
		ShowYear:  2022,
		Year:      2025,
		ShowIDs:   core.IDs{IMDB: "tt11280740", TMDB: 95396},
		Season:    2,
		Episode:   3,
	}
	cands, err = svc.SearchOnDemand(ctx, ep, "en", "Severance.S02E03.1080p.ATVP.WEB-DL.mkv")
	if err != nil || len(cands) != 1 || cands[0].Release != "Severance.S02E03.1080p.ATVP.WEB-DL" {
		t.Fatalf("episode filtering: %+v %v", cands, err)
	}
}

// TestSubSourceRateLimitAndFallthrough verifies:
//   - HTTP 429 with X-RateLimit-Reset beyond MaxRetryAfter fails fast, falls through
//     to the next provider (SubDL), and respects the cooldown until the reset time.
//   - X-RateLimit-Remaining: 0 on a 200 response proactively pauses subsequent requests
//     until X-RateLimit-Reset.
//   - Rate-limit errors are never negatively cached, so SubSource recovers after reset.
func TestSubSourceRateLimitAndFallthrough(t *testing.T) {
	now := time.Date(2026, 11, 21, 14, 0, 0, 0, time.UTC)
	var ssMode atomic.Int32 // 0 = 429 with +45s reset, 1 = 200 with Remaining=0 +30s reset, 2 = 200 normal
	var ssHits atomic.Int32

	ssSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ssHits.Add(1)
		mode := ssMode.Load()
		if mode == 0 {
			w.Header().Set("X-RateLimit-Reset", now.Add(45*time.Second).Format(time.RFC3339))
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"success":false,"retryAfter":45}`))
			return
		}
		if mode == 1 && r.URL.Path == "/subtitles" {
			w.Header().Set("X-RateLimit-Remaining", "0")
			w.Header().Set("X-RateLimit-Reset", now.Add(30*time.Second).Format(time.RFC3339))
		}
		if r.URL.Path == "/movies/search" {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"success": true,
				"data":    []any{map[string]any{"movieId": 500, "title": "Matrix", "type": "movie", "releaseYear": 1999, "imdbId": "tt0133093"}},
			})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"success": true,
			"data": []any{map[string]any{
				"subtitleId":  5001,
				"movieId":     500,
				"language":    "english",
				"releaseInfo": []string{"The.Matrix.1999.1080p.BluRay"},
			}},
		})
	}))
	defer ssSrv.Close()

	subdlSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status": true,
			"subtitles": []any{map[string]any{
				"release_name": "The.Matrix.1999.1080p.BluRay",
				"name":         "matrix.en.zip",
				"url":          "/subtitle/1-2.zip",
				"language":     "English",
			}},
		})
	}))
	defer subdlSrv.Close()

	svc := New(Options{
		SubSource:     config.SubSource{Enabled: true, APIKey: "ss-key"},
		SubDL:         config.SubDL{Enabled: true, APIKey: "dl-key"},
		Providers:     []config.SubtitleProvider{{ID: "subsource", Kind: config.SubtitleProviderSubSource, Enabled: true}, {ID: "subdl", Kind: config.SubtitleProviderSubDL, Enabled: true}},
		CacheDir:      t.TempDir(),
		SubSourceURL:  ssSrv.URL,
		SubDLURL:      subdlSrv.URL,
		MaxRetryAfter: 20 * time.Millisecond,
		Now:           func() time.Time { return now },
	})

	title := &core.Title{Key: core.MovieKey(603), Type: core.TypeMovie, Title: "The Matrix", Year: 1999, IDs: core.IDs{IMDB: "tt0133093", TMDB: 603}}
	ctx := context.Background()

	// 1. SubSource returns 429 (+45s): falls through immediately to SubDL.
	cands, err := svc.SearchOnDemand(ctx, title, "en", "The.Matrix.1999.1080p.BluRay")
	if err != nil || len(cands) != 1 || cands[0].ProviderID != config.SubtitleProviderSubDL {
		t.Fatalf("expected fallthrough to SubDL on 429, got %+v %v", cands, err)
	}
	hitsAfter429 := ssHits.Load()

	// 2. Within cooldown (+10s), SubSource is skipped without issuing another HTTP request.
	now = now.Add(10 * time.Second)
	cands, err = svc.SearchOnDemand(ctx, title, "en", "The.Matrix.1999.1080p.BluRay")
	if err != nil || len(cands) != 1 || cands[0].ProviderID != config.SubtitleProviderSubDL {
		t.Fatalf("expected cooldown fallthrough to SubDL, got %+v %v", cands, err)
	}
	if ssHits.Load() != hitsAfter429 {
		t.Fatalf("SubSource should not have been called during cooldown: hits %d -> %d", hitsAfter429, ssHits.Load())
	}

	// 3. Past reset (+46s total), SubSource resumes and returns Remaining=0 with +30s reset.
	ssMode.Store(1)
	now = now.Add(36 * time.Second)
	cands, err = svc.SearchOnDemand(ctx, title, "en", "The.Matrix.1999.1080p.BluRay")
	if err != nil || len(cands) != 2 || cands[0].ProviderID != config.SubtitleProviderSubSource {
		t.Fatalf("expected SubSource recovery after reset, got %+v %v", cands, err)
	}
	hitsAfterRemainingZero := ssHits.Load()

	// 4. Proactive X-RateLimit-Remaining: 0 skips SubSource until its reset window elapses.
	now = now.Add(5 * time.Second)
	cands, err = svc.SearchOnDemand(ctx, title, "en", "The.Matrix.1999.1080p.BluRay")
	if err != nil || len(cands) != 1 || cands[0].ProviderID != config.SubtitleProviderSubDL {
		t.Fatalf("expected proactive cooldown after Remaining=0, got %+v %v", cands, err)
	}
	if ssHits.Load() != hitsAfterRemainingZero {
		t.Fatalf("SubSource called despite X-RateLimit-Remaining: 0")
	}

	// 5. Rotating the API key clears the rate-limit cooldown immediately.
	ssMode.Store(2)
	svc.UpdateSubSource(config.SubSource{Enabled: true, APIKey: "ss-pro-key"})
	cands, err = svc.SearchOnDemand(ctx, title, "en", "The.Matrix.1999.1080p.BluRay")
	if err != nil || len(cands) != 2 || cands[0].ProviderID != config.SubtitleProviderSubSource {
		t.Fatalf("expected immediate recovery after key update, got %+v %v", cands, err)
	}
}

func TestSubSourceRateLimitResetParsing(t *testing.T) {
	now := time.Date(2026, 11, 21, 14, 0, 0, 0, time.UTC)

	// X-RateLimit-Reset ISO-8601 takes precedence over body retryAfter.
	h := http.Header{}
	h.Set("X-RateLimit-Reset", "2026-11-21T14:00:26Z")
	got, ok := subsourceRateLimitReset(h, []byte(`{"retryAfter":3}`), now)
	if !ok || !got.Equal(now.Add(26*time.Second)) {
		t.Fatalf("ISO header: got %v (%v)", got, ok)
	}

	// JSON retryAfter fallback when headers are absent.
	got, ok = subsourceRateLimitReset(http.Header{}, []byte(`{"retryAfter":12.2}`), now)
	if !ok || !got.Equal(now.Add(13*time.Second)) {
		t.Fatalf("body retryAfter: got %v (%v)", got, ok)
	}

	// Retry-After header fallback.
	ra := http.Header{}
	ra.Set("Retry-After", "15")
	got, ok = subsourceRateLimitReset(ra, nil, now)
	if !ok || !got.Equal(now.Add(15*time.Second)) {
		t.Fatalf("Retry-After: got %v (%v)", got, ok)
	}

	// Unparsable CDN body returns false so caller applies default cooldown.
	if _, ok = subsourceRateLimitReset(http.Header{}, []byte("too many requests"), now); ok {
		t.Fatal("expected false for unparsable CDN 429")
	}
}

func TestSubSourceLanguagesAndDisabled(t *testing.T) {
	svc := New(Options{CacheDir: t.TempDir(), SubSource: config.SubSource{Enabled: true}})
	if svc.Enabled() || svc.subsourceEnabled() {
		t.Fatal("SubSource without an API key must stay disabled")
	}

	// Distinct regional variants map cleanly both ways.
	if got := subsourceQueryLangs("pt-BR,pt-PT,zh-CN,zh-TW,fa,nb"); strings.Join(got, ",") !=
		"brazilian_portuguese,portuguese,chinese_bg_code,big_5_code,farsi_persian,norwegian" {
		t.Fatalf("query langs = %v", got)
	}
	if !matchesSearchLanguages("pt-BR", subsourceParseLang("brazilian_portuguese")) {
		t.Fatal("brazilian_portuguese must match pt-BR")
	}
	if matchesSearchLanguages("pt-BR", subsourceParseLang("portuguese")) {
		t.Fatal("european portuguese must not match pt-BR")
	}
	if !matchesSearchLanguages("pt", subsourceParseLang("portuguese")) {
		t.Fatal("portuguese must match generic pt")
	}
	if !matchesSearchLanguages("zh-CN", subsourceParseLang("chinese_bg_code")) {
		t.Fatal("chinese_bg_code must match zh-CN")
	}
	if !matchesSearchLanguages("zh-TW", subsourceParseLang("big_5_code")) {
		t.Fatal("big_5_code must match zh-TW")
	}
	if !matchesSearchLanguages("nb", subsourceParseLang("norwegian")) {
		t.Fatal("norwegian must match nb")
	}
}
