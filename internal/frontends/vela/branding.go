package vela

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"image"
	"image/png"
	"io"
	"math"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/braedonsaunders/ether/internal/frontend"
	"golang.org/x/image/webp"
)

const (
	MaxAppNameChars   = 40
	MaxWordmarkBytes  = 512 << 10
	MaxWordmarkWidth  = 1024
	MaxWordmarkHeight = 256
)

var (
	ErrWordmarkTooLarge  = errors.New("wordmark exceeds 512 KB")
	brandingURLPattern   = regexp.MustCompile(`^/vela/v0/branding/([0-9a-f]{64})\.(png|svg|webp)$`)
	brandingAssetPattern = regexp.MustCompile(`^([0-9a-f]{64})\.(png|svg|webp)$`)
)

func init() {
	frontend.RegisterMigration("vela_wordmarks", 1, `CREATE TABLE vela_wordmarks (digest TEXT PRIMARY KEY, ext TEXT NOT NULL, mime TEXT NOT NULL, width INTEGER NOT NULL, height INTEGER NOT NULL, data BLOB NOT NULL, created_at INTEGER NOT NULL);`)
}

// WordmarkValidationError reports an invalid wordmark image or dimension check.
type WordmarkValidationError struct {
	Message string
}

func (e *WordmarkValidationError) Error() string { return e.Message }

// Wordmark is a validated, content-addressed owner wordmark asset.
type Wordmark struct {
	URL    string `json:"url"`
	Width  int    `json:"width"`
	Height int    `json:"height"`
	Digest string `json:"digest"`
}

type wordmarkSetting struct {
	URL    string
	Width  int
	Height int
	Dark   string
}

type brandingBlob struct {
	mime string
	data []byte
}

// UploadWordmark validates a PNG, SVG, or WebP wordmark (<= 512 KB, <= 1024x256 px)
// and stores it by SHA-256 digest for unauthenticated immutable serving under /vela/v0/branding/.
func UploadWordmark(ctx context.Context, db *sql.DB, r io.Reader) (Wordmark, error) {
	if db == nil {
		return Wordmark{}, errors.New("wordmark storage unavailable")
	}
	if r == nil {
		return Wordmark{}, &WordmarkValidationError{Message: "wordmark file is empty"}
	}
	data, err := io.ReadAll(io.LimitReader(r, MaxWordmarkBytes+1))
	if err != nil {
		return Wordmark{}, err
	}
	if len(data) == 0 {
		return Wordmark{}, &WordmarkValidationError{Message: "wordmark file is empty"}
	}
	if len(data) > MaxWordmarkBytes {
		return Wordmark{}, ErrWordmarkTooLarge
	}
	ext, mimeType, width, height, err := validateWordmarkBytes(data)
	if err != nil {
		return Wordmark{}, err
	}
	sum := sha256.Sum256(data)
	digest := hex.EncodeToString(sum[:])
	_, err = db.ExecContext(
		ctx,
		`INSERT INTO vela_wordmarks(digest, ext, mime, width, height, data, created_at) VALUES(?, ?, ?, ?, ?, ?, ?) ON CONFLICT(digest) DO NOTHING`,
		digest, ext, mimeType, width, height, data, time.Now().Unix(),
	)
	if err != nil {
		return Wordmark{}, err
	}
	return Wordmark{
		URL:    "/vela/v0/branding/" + digest + "." + ext,
		Width:  width,
		Height: height,
		Digest: digest,
	}, nil
}

// PruneWordmarks removes unreferenced wordmarks older than an hour so an
// uploaded draft is never deleted mid-edit while old assets do not accumulate.
func PruneWordmarks(ctx context.Context, db *sql.DB, settings map[string]any) {
	if db == nil {
		return
	}
	cutoff := time.Now().Add(-time.Hour).Unix()
	keep := referencedWordmarkDigests(settings)
	if len(keep) == 0 {
		_, _ = db.ExecContext(ctx, `DELETE FROM vela_wordmarks WHERE created_at < ?`, cutoff)
		return
	}
	args := make([]any, 0, 1+len(keep))
	args = append(args, cutoff)
	for d := range keep {
		args = append(args, d)
	}
	_, _ = db.ExecContext(ctx, `DELETE FROM vela_wordmarks WHERE created_at < ? AND digest NOT IN (`+frontend.SQLPlaceholders(len(keep))+`)`, args...)
}

func referencedWordmarkDigests(settings map[string]any) map[string]bool {
	out := map[string]bool{}
	wm, ok := parseWordmarkSetting(settings["wordmark"])
	if !ok {
		return out
	}
	if m := brandingURLPattern.FindStringSubmatch(wm.URL); m != nil {
		out[m[1]] = true
	}
	if m := brandingURLPattern.FindStringSubmatch(wm.Dark); m != nil {
		out[m[1]] = true
	}
	return out
}

// SignInMethods holds the owner's enabled sign-in methods for Vela viewers.
// Each method defaults to enabled when absent from persisted settings.
type SignInMethods struct {
	Password     bool
	Plex         bool
	QuickConnect bool
}

// SignInMethodsFromSettings returns the effective Vela sign-in method toggles.
func SignInMethodsFromSettings(s map[string]any) SignInMethods {
	out := SignInMethods{Password: true, Plex: true, QuickConnect: true}
	if s == nil {
		return out
	}
	if v, ok := s["passwordSignIn"].(bool); ok {
		out.Password = v
	}
	if v, ok := s["plexSignIn"].(bool); ok {
		out.Plex = v
	}
	if v, ok := s["quickConnect"].(bool); ok {
		out.QuickConnect = v
	}
	return out
}

// CapabilitiesChanged reports whether two Vela settings maps produce different
// advertised capabilities.
func CapabilitiesChanged(before, after map[string]any) bool {
	return SignInMethodsFromSettings(before) != SignInMethodsFromSettings(after) ||
		trailerPlaybackMode(before) != trailerPlaybackMode(after)
}

// NormalizeSettings trims appName, omits unset branding keys, and clones wordmark/icon maps.
func NormalizeSettings(s frontend.Settings) frontend.Settings {
	out := make(frontend.Settings, len(s))
	for k, v := range s {
		switch k {
		case "appName":
			if name, ok := v.(string); ok {
				if trimmed := strings.TrimSpace(name); trimmed != "" {
					out["appName"] = trimmed
				}
			}
		case "wordmark":
			if wm, ok := parseWordmarkSetting(v); ok {
				m := map[string]any{
					"url":    wm.URL,
					"width":  wm.Width,
					"height": wm.Height,
				}
				if wm.Dark != "" {
					m["dark"] = wm.Dark
				}
				out["wordmark"] = m
			}
		case "livePreview":
			if mode, ok := v.(string); ok && ValidLivePreviewMode(mode) {
				out["livePreview"] = mode
			}
		case "appIcon":
			if icon, ok := parseAppIconSetting(v); ok {
				out["appIcon"] = map[string]any{
					"url":    icon.URL,
					"width":  icon.Width,
					"height": icon.Height,
				}
			}
		default:
			out[k] = v
		}
	}
	return out
}

// AppearanceChanged reports whether two Vela settings maps produce a different
// appearance/branding snapshot.
func AppearanceChanged(before, after map[string]any) bool {
	return canonicalAppearanceState(before) != canonicalAppearanceState(after)
}

func canonicalAppearanceState(s map[string]any) string {
	skin := "cinematic"
	if id, ok := s["defaultSkinId"].(string); ok && ValidSkinID(id) {
		skin = id
	}
	force, _ := s["force"].(bool)
	name := ""
	if raw, ok := s["appName"].(string); ok {
		if trimmed := strings.TrimSpace(raw); trimmed != "" && utf8.RuneCountInString(trimmed) <= MaxAppNameChars && !hasControlRune(trimmed) {
			name = trimmed
		}
	}
	wmKey := ""
	if wm, ok := parseWordmarkSetting(s["wordmark"]); ok {
		wmKey = fmt.Sprintf("%s|%d|%d|%s", wm.URL, wm.Width, wm.Height, wm.Dark)
	}
	iconKey := ""
	if icon, ok := parseAppIconSetting(s["appIcon"]); ok {
		iconKey = fmt.Sprintf("%s|%d|%d", icon.URL, icon.Width, icon.Height)
	}
	return fmt.Sprintf("%s\x00%t\x00%s\x00%s\x00%s", skin, force, name, wmKey, iconKey)
}

func brandingFromSettings(s map[string]any) (object, string) {
	branding := object{}
	appName := ""
	if raw, ok := s["appName"].(string); ok {
		trimmed := strings.TrimSpace(raw)
		if trimmed != "" && utf8.RuneCountInString(trimmed) <= MaxAppNameChars && !hasControlRune(trimmed) {
			branding["name"] = trimmed
			appName = trimmed
		}
	}
	if wm, ok := parseWordmarkSetting(s["wordmark"]); ok {
		w := object{
			"url":    wm.URL,
			"width":  wm.Width,
			"height": wm.Height,
		}
		if wm.Dark != "" {
			w["dark"] = wm.Dark
		}
		branding["wordmark"] = w
	}
	return branding, appName
}

func hasControlRune(s string) bool {
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return true
		}
	}
	return false
}

func parseWordmarkSetting(v any) (wordmarkSetting, bool) {
	if v == nil {
		return wordmarkSetting{}, false
	}
	m, ok := v.(map[string]any)
	if !ok {
		return wordmarkSetting{}, false
	}
	for k := range m {
		if k != "url" && k != "width" && k != "height" && k != "dark" {
			return wordmarkSetting{}, false
		}
	}
	urlVal, ok := m["url"].(string)
	if !ok || !brandingURLPattern.MatchString(urlVal) {
		return wordmarkSetting{}, false
	}
	width, okW := settingInt(m["width"])
	height, okH := settingInt(m["height"])
	if !okW || !okH || width < 1 || width > MaxWordmarkWidth || height < 1 || height > MaxWordmarkHeight {
		return wordmarkSetting{}, false
	}
	dark := ""
	if darkRaw, exists := m["dark"]; exists && darkRaw != nil {
		d, ok := darkRaw.(string)
		if !ok {
			return wordmarkSetting{}, false
		}
		d = strings.TrimSpace(d)
		if d != "" {
			if !brandingURLPattern.MatchString(d) {
				return wordmarkSetting{}, false
			}
			dark = d
		}
	}
	return wordmarkSetting{URL: urlVal, Width: width, Height: height, Dark: dark}, true
}

func settingInt(v any) (int, bool) {
	switch n := v.(type) {
	case int:
		return n, true
	case int64:
		return int(n), true
	case float64:
		if math.IsNaN(n) || math.IsInf(n, 0) || n != math.Trunc(n) {
			return 0, false
		}
		return int(n), true
	default:
		return 0, false
	}
}

func validateWordmarkInDB(db *sql.DB, wm wordmarkSetting) error {
	if db == nil {
		return nil
	}
	mainMatch := brandingURLPattern.FindStringSubmatch(wm.URL)
	if mainMatch == nil {
		return errors.New("invalid wordmark URL")
	}
	var ext string
	var width, height int
	err := db.QueryRow(`SELECT ext, width, height FROM vela_wordmarks WHERE digest = ?`, mainMatch[1]).Scan(&ext, &width, &height)
	if err != nil || ext != mainMatch[2] || width != wm.Width || height != wm.Height {
		return errors.New("unknown or mismatched wordmark asset")
	}
	if wm.Dark != "" {
		darkMatch := brandingURLPattern.FindStringSubmatch(wm.Dark)
		if darkMatch == nil {
			return errors.New("invalid dark wordmark URL")
		}
		var darkExt string
		if err := db.QueryRow(`SELECT ext FROM vela_wordmarks WHERE digest = ?`, darkMatch[1]).Scan(&darkExt); err != nil || darkExt != darkMatch[2] {
			return errors.New("unknown dark wordmark asset")
		}
	}
	return nil
}

func (i *instance) brandingAsset(w http.ResponseWriter, r *http.Request) {
	if i.db == nil {
		http.NotFound(w, r)
		return
	}
	asset := r.PathValue("asset")
	var digest, ext, cacheKey, query string
	if m := brandingAssetPattern.FindStringSubmatch(asset); m != nil {
		digest, ext = m[1], m[2]
		cacheKey = digest + "." + ext
		query = `SELECT mime, data FROM vela_wordmarks WHERE digest = ? AND ext = ?`
	} else if m := appIconAssetPattern.FindStringSubmatch(asset); m != nil {
		digest, ext = m[1], m[2]
		cacheKey = "icon-" + digest + "." + ext
		query = `SELECT mime, data FROM vela_app_icons WHERE digest = ? AND ext = ?`
	} else {
		http.NotFound(w, r)
		return
	}

	i.brandingMu.Lock()
	blob, ok := i.brandingBlobs[cacheKey]
	i.brandingMu.Unlock()
	if !ok {
		var mimeType string
		var data []byte
		err := i.db.QueryRowContext(r.Context(), query, digest, ext).Scan(&mimeType, &data)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		blob = brandingBlob{mime: mimeType, data: data}
		i.brandingMu.Lock()
		if i.brandingBlobs == nil {
			i.brandingBlobs = make(map[string]brandingBlob)
		}
		if len(i.brandingBlobs) >= 16 {
			clear(i.brandingBlobs)
		}
		i.brandingBlobs[cacheKey] = blob
		i.brandingMu.Unlock()
	}

	etag := `"` + digest + `"`
	w.Header().Set("Content-Type", blob.mime)
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	w.Header().Set("ETag", etag)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if blob.mime == "image/svg+xml" {
		w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'")
	}
	for _, tag := range strings.Split(r.Header.Get("If-None-Match"), ",") {
		tag = strings.TrimSpace(tag)
		if tag == "*" || strings.TrimPrefix(tag, "W/") == etag {
			w.WriteHeader(http.StatusNotModified)
			return
		}
	}
	w.Header().Set("Content-Length", strconv.Itoa(len(blob.data)))
	if r.Method == http.MethodHead {
		w.WriteHeader(http.StatusOK)
		return
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(blob.data)
}

func validateWordmarkBytes(data []byte) (ext, mimeType string, width, height int, err error) {
	switch {
	case len(data) >= 8 && string(data[:8]) == "\x89PNG\r\n\x1a\n":
		cfg, err := png.DecodeConfig(bytes.NewReader(data))
		if err != nil {
			return "", "", 0, 0, &WordmarkValidationError{Message: "invalid PNG image"}
		}
		if err := checkWordmarkDimensions(cfg.Width, cfg.Height); err != nil {
			return "", "", 0, 0, err
		}
		img, err := png.Decode(bytes.NewReader(data))
		if err != nil || img.Bounds().Dx() != cfg.Width || img.Bounds().Dy() != cfg.Height {
			return "", "", 0, 0, &WordmarkValidationError{Message: "corrupt PNG image"}
		}
		return "png", "image/png", cfg.Width, cfg.Height, nil

	case len(data) >= 12 && string(data[:4]) == "RIFF" && string(data[8:12]) == "WEBP":
		cfg, err := webp.DecodeConfig(bytes.NewReader(data))
		if err != nil {
			return "", "", 0, 0, &WordmarkValidationError{Message: "invalid WebP image"}
		}
		if err := checkWordmarkDimensions(cfg.Width, cfg.Height); err != nil {
			return "", "", 0, 0, err
		}
		img, err := webp.Decode(bytes.NewReader(data))
		if err != nil || img.Bounds().Dx() != cfg.Width || img.Bounds().Dy() != cfg.Height {
			return "", "", 0, 0, &WordmarkValidationError{Message: "corrupt WebP image"}
		}
		return "webp", "image/webp", cfg.Width, cfg.Height, nil

	case looksLikeSVG(data):
		w, h, err := validateWordmarkSVG(data)
		if err != nil {
			return "", "", 0, 0, err
		}
		return "svg", "image/svg+xml", w, h, nil

	default:
		// Ensure image package stays referenced cleanly alongside format decoders.
		_ = image.Config{}
		return "", "", 0, 0, &WordmarkValidationError{Message: "wordmark must be a PNG, SVG, or WebP image"}
	}
}

func checkWordmarkDimensions(width, height int) error {
	if width < 1 || height < 1 {
		return &WordmarkValidationError{Message: "wordmark image must have positive dimensions"}
	}
	if width > MaxWordmarkWidth || height > MaxWordmarkHeight {
		return &WordmarkValidationError{Message: fmt.Sprintf("wordmark exceeds maximum dimensions (%dx%d px)", MaxWordmarkWidth, MaxWordmarkHeight)}
	}
	return nil
}

func looksLikeSVG(data []byte) bool {
	trimmed := bytes.TrimPrefix(data, []byte("\xef\xbb\xbf"))
	trimmed = bytes.TrimSpace(trimmed)
	if len(trimmed) < 5 {
		return false
	}
	prefix := strings.ToLower(string(trimmed[:min(len(trimmed), 256)]))
	return strings.HasPrefix(prefix, "<svg") || strings.HasPrefix(prefix, "<?xml") || strings.HasPrefix(prefix, "<!--")
}

func validateWordmarkSVG(data []byte) (int, int, error) {
	trimmed := bytes.TrimPrefix(data, []byte("\xef\xbb\xbf"))
	if !utf8.Valid(trimmed) {
		return 0, 0, &WordmarkValidationError{Message: "SVG must be valid UTF-8"}
	}
	dec := xml.NewDecoder(bytes.NewReader(trimmed))
	root := false
	depth, elements, styleDepth := 0, 0, 0
	var rootWidthAttr, rootHeightAttr, rootViewBox string

	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return 0, 0, &WordmarkValidationError{Message: "invalid SVG XML"}
		}
		switch node := tok.(type) {
		case xml.Directive:
			return 0, 0, &WordmarkValidationError{Message: "SVG directives and entities are not allowed"}
		case xml.ProcInst:
			if node.Target != "xml" || root {
				return 0, 0, &WordmarkValidationError{Message: "SVG processing instructions are not allowed"}
			}
		case xml.StartElement:
			depth++
			elements++
			if depth > 64 || elements > 2048 {
				return 0, 0, &WordmarkValidationError{Message: "SVG is too complex"}
			}
			name := strings.ToLower(node.Name.Local)
			if !root {
				if name != "svg" || (node.Name.Space != "" && node.Name.Space != "http://www.w3.org/2000/svg") {
					return 0, 0, &WordmarkValidationError{Message: "root element must be <svg>"}
				}
				root = true
				for _, attr := range node.Attr {
					if attr.Name.Space != "" {
						continue
					}
					switch strings.ToLower(attr.Name.Local) {
					case "width":
						rootWidthAttr = attr.Value
					case "height":
						rootHeightAttr = attr.Value
					case "viewbox":
						rootViewBox = attr.Value
					}
				}
			}
			switch name {
			case "script", "foreignobject", "iframe", "frame", "object", "embed", "audio", "video", "canvas", "animate", "set", "animatemotion", "animatetransform", "feimage", "image", "a":
				return 0, 0, &WordmarkValidationError{Message: fmt.Sprintf("SVG <%s> element is not allowed", name)}
			case "style":
				styleDepth++
			}
			for _, attr := range node.Attr {
				key := strings.ToLower(attr.Name.Local)
				val := strings.TrimSpace(attr.Value)
				if len(val) > 16384 {
					return 0, 0, &WordmarkValidationError{Message: "SVG attribute is too long"}
				}
				if strings.HasPrefix(key, "on") {
					return 0, 0, &WordmarkValidationError{Message: "SVG event handlers are not allowed"}
				}
				if key == "href" {
					if !isLocalSVGFragment(val) {
						return 0, 0, &WordmarkValidationError{Message: "SVG external references are not allowed"}
					}
					continue
				}
				if key == "style" {
					if !safeSVGCSS(val) {
						return 0, 0, &WordmarkValidationError{Message: "SVG style contains disallowed references"}
					}
					continue
				}
				if !safeSVGAttrValue(val) {
					return 0, 0, &WordmarkValidationError{Message: "SVG attribute contains disallowed references"}
				}
			}
		case xml.CharData:
			if styleDepth > 0 && !safeSVGCSS(string(node)) {
				return 0, 0, &WordmarkValidationError{Message: "SVG stylesheet contains disallowed references"}
			}
		case xml.EndElement:
			if strings.EqualFold(node.Name.Local, "style") && styleDepth > 0 {
				styleDepth--
			}
			depth--
		}
	}
	if !root || depth != 0 {
		return 0, 0, &WordmarkValidationError{Message: "incomplete SVG document"}
	}
	w, h, ok := resolveSVGDimensions(rootWidthAttr, rootHeightAttr, rootViewBox)
	if !ok {
		return 0, 0, &WordmarkValidationError{Message: "SVG must specify pixel width and height or a viewBox"}
	}
	if err := checkWordmarkDimensions(w, h); err != nil {
		return 0, 0, err
	}
	return w, h, nil
}

func isLocalSVGFragment(s string) bool {
	return strings.HasPrefix(s, "#") && len(s) > 1 && !strings.ContainsAny(s, " \t\r\n\"'()<>\\")
}

func safeSVGAttrValue(s string) bool {
	lower := strings.ToLower(s)
	if strings.Contains(lower, "javascript:") || strings.Contains(lower, "vbscript:") || strings.Contains(lower, "data:") {
		return false
	}
	return safeSVGURLRefs(lower)
}

func safeSVGCSS(s string) bool {
	lower := strings.ToLower(s)
	for _, bad := range []string{"@import", "javascript:", "vbscript:", "data:", "expression(", "-moz-binding", "behavior:"} {
		if strings.Contains(lower, bad) {
			return false
		}
	}
	return safeSVGURLRefs(lower)
}

func safeSVGURLRefs(lower string) bool {
	rest := lower
	for {
		idx := strings.Index(rest, "url(")
		if idx < 0 {
			return true
		}
		rest = rest[idx+4:]
		end := strings.IndexByte(rest, ')')
		if end < 0 {
			return false
		}
		inner := strings.TrimSpace(rest[:end])
		inner = strings.Trim(inner, `"'`)
		if !isLocalSVGFragment(inner) {
			return false
		}
		rest = rest[end+1:]
	}
}

func resolveSVGDimensions(widthAttr, heightAttr, viewBoxAttr string) (int, int, bool) {
	wPx, hasW := parseSVGPixels(widthAttr)
	hPx, hasH := parseSVGPixels(heightAttr)
	vbW, vbH, hasVB := parseSVGViewBox(viewBoxAttr)

	var wf, hf float64
	switch {
	case hasW && hasH:
		wf, hf = wPx, hPx
	case hasW && hasVB:
		wf = wPx
		hf = wPx * vbH / vbW
	case hasH && hasVB:
		hf = hPx
		wf = hPx * vbW / vbH
	case hasVB:
		wf, hf = vbW, vbH
	default:
		return 0, 0, false
	}
	if math.IsNaN(wf) || math.IsInf(wf, 0) || math.IsNaN(hf) || math.IsInf(hf, 0) {
		return 0, 0, false
	}
	if wf < 1 || hf < 1 || wf > MaxWordmarkWidth || hf > MaxWordmarkHeight {
		// Return out-of-range integers when > max so checkWordmarkDimensions gives a clear error.
		if wf > MaxWordmarkWidth || hf > MaxWordmarkHeight {
			return int(math.Ceil(wf)), int(math.Ceil(hf)), true
		}
		return 0, 0, false
	}
	return int(math.Round(wf)), int(math.Round(hf)), true
}

func parseSVGPixels(raw string) (float64, bool) {
	s := strings.TrimSpace(strings.ToLower(raw))
	if s == "" {
		return 0, false
	}
	s = strings.TrimSuffix(s, "px")
	s = strings.TrimSpace(s)
	val, err := strconv.ParseFloat(s, 64)
	if err != nil || math.IsNaN(val) || math.IsInf(val, 0) || val <= 0 {
		return 0, false
	}
	return val, true
}

func parseSVGViewBox(raw string) (float64, float64, bool) {
	fields := strings.FieldsFunc(raw, func(r rune) bool {
		return r == ',' || r == ' ' || r == '\t' || r == '\r' || r == '\n'
	})
	if len(fields) != 4 {
		return 0, 0, false
	}
	nums := make([]float64, 4)
	for idx, f := range fields {
		v, err := strconv.ParseFloat(f, 64)
		if err != nil || math.IsNaN(v) || math.IsInf(v, 0) {
			return 0, 0, false
		}
		nums[idx] = v
	}
	if nums[2] <= 0 || nums[3] <= 0 {
		return 0, 0, false
	}
	return nums[2], nums[3], true
}
