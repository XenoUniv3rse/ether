package onair

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/braedonsaunders/ether/internal/artwork"
	"github.com/go-chi/chi/v5"
	"golang.org/x/image/draw"
	"golang.org/x/image/font"
	"golang.org/x/image/font/opentype"
	"golang.org/x/image/math/fixed"
	_ "golang.org/x/image/webp"
)

type LibraryUse struct {
	Kind  string `json:"kind"`
	ID    string `json:"id"`
	Name  string `json:"name"`
	Scene string `json:"scene,omitempty"`
}
type LibraryAsset struct {
	Asset
	MIME      string       `json:"mime"`
	UsedBy    []LibraryUse `json:"usedBy"`
	CreatedAt int64        `json:"createdAt"`
}

func (s *Service) Library(ctx context.Context) ([]LibraryAsset, error) {
	assets, e := s.Assets(ctx)
	if e != nil {
		return nil, e
	}
	out := []LibraryAsset{}
	for _, a := range assets {
		if a.Kind == "reel" {
			continue
		}
		v, e := s.libraryInfo(ctx, a)
		if e != nil {
			return nil, e
		}
		out = append(out, v)
	}
	return out, nil
}
func (s *Service) libraryInfo(ctx context.Context, a Asset) (LibraryAsset, error) {
	v := LibraryAsset{Asset: a, MIME: a.MIME, UsedBy: []LibraryUse{}}
	if a.Status != "" && a.Status != "ready" {
		v.CreatedAt = s.now().Unix()
		return v, nil
	}
	if e := s.db.QueryRowContext(ctx, "SELECT created_at FROM onair_assets WHERE digest=?", a.Digest).Scan(&v.CreatedAt); e != nil {
		return v, e
	}
	for _, entry := range []struct{ table, kind string }{{"onair_looks", "style"}, {"onair_reels", "reel"}, {"livetv_channels", "channel"}} {
		if entry.kind == "channel" {
			ok, e := s.hasChannelAssets(ctx)
			if e != nil {
				return v, e
			}
			if !ok {
				continue
			}
		}
		query := "SELECT id,data FROM " + entry.table + " WHERE data LIKE ?"
		if entry.kind == "reel" {
			query = "SELECT id,data FROM onair_reels WHERE id IN (SELECT reel_id FROM onair_reel_clips WHERE source_digest=?)"
		}
		arg := "%" + a.Digest + "%"
		if entry.kind == "reel" {
			arg = a.Digest
		}
		rows, e := s.db.QueryContext(ctx, query, arg)
		if e != nil {
			return v, e
		}
		for rows.Next() {
			var id, data string
			if e = rows.Scan(&id, &data); e != nil {
				rows.Close()
				return v, e
			}
			var record struct {
				Name   string            `json:"name"`
				Scenes []json.RawMessage `json:"scenes"`
			}
			_ = json.Unmarshal([]byte(data), &record)
			name := record.Name
			if name == "" {
				name = id
			}
			v.UsedBy = append(v.UsedBy, LibraryUse{Kind: entry.kind, ID: id, Name: name})
			for _, raw := range record.Scenes {
				if strings.Contains(string(raw), a.Digest) {
					var sc struct{ ID, Name string }
					_ = json.Unmarshal(raw, &sc)
					v.UsedBy = append(v.UsedBy, LibraryUse{Kind: "style", ID: id, Name: sc.Name, Scene: sc.ID})
				}
			}

		}
		e = rows.Err()
		rows.Close()
		if e != nil {
			return v, e
		}
	}
	return v, nil
}
func (s *Service) libraryAssetRow(ctx context.Context, d string) (Asset, error) {
	var a Asset
	a.Digest = d
	e := s.db.QueryRowContext(ctx, "SELECT name,kind,size,license,mime,source,attribution,COALESCE(duration_ms,0),width,height FROM onair_assets WHERE digest=?", d).Scan(&a.Name, &a.Kind, &a.Size, &a.License, &a.MIME, &a.Source, &a.Attribution, &a.DurationMS, &a.Width, &a.Height)
	if e != nil {
		return Asset{}, ErrNotFound
	}
	return a, nil
}
func (s *Service) libraryAsset(ctx context.Context, d string) (LibraryAsset, error) {
	a, e := s.libraryAssetRow(ctx, d)
	if e != nil {
		return LibraryAsset{}, e
	}
	return s.libraryInfo(ctx, a)
}

// recordFacts probes media dimensions and duration and stores them on the row.
// b is the asset's bytes when the caller holds them; path is its file.
func (s *Service) recordFacts(ctx context.Context, a Asset, b []byte, path string) Asset {
	a.DurationMS, a.Width, a.Height = 0, 0, 0
	if a.Kind == "image" {
		var r io.Reader
		if b != nil {
			r = bytes.NewReader(b)
		} else if f, e := os.Open(path); e == nil {
			defer f.Close()
			r = f
		}
		if r != nil {
			if c, _, e := image.DecodeConfig(r); e == nil {
				a.Width, a.Height = c.Width, c.Height
			}
		}
	}
	if (a.Kind == "audio" || a.Kind == "video" || a.Kind == "reel") && s.o.Probe != nil {
		if info, e := s.o.Probe(ctx, path); e == nil {
			a.DurationMS = info.Duration.Milliseconds()
			if st := info.Video(); st != nil {
				a.Width, a.Height = st.Width, st.Height
			}
		}
	}
	if _, e := s.db.ExecContext(ctx, "UPDATE onair_assets SET duration_ms=?,width=?,height=? WHERE digest=?", a.DurationMS, a.Width, a.Height, a.Digest); e != nil {
		s.o.Logger.Warn("onair asset facts not saved", "asset", a.Digest, "err", e)
	}
	return a
}

// backfillFacts probes assets stored before media facts existed, one at a time.
func (s *Service) backfillFacts() {
	defer s.wg.Done()
	rows, e := s.db.QueryContext(s.ctx, "SELECT digest,name,kind FROM onair_assets WHERE duration_ms IS NULL")
	if e != nil {
		return
	}
	var pending []Asset
	for rows.Next() {
		var a Asset
		if rows.Scan(&a.Digest, &a.Name, &a.Kind) == nil {
			pending = append(pending, a)
		}
	}
	rows.Close()
	for _, a := range pending {
		if s.ctx.Err() != nil {
			return
		}
		path, e := s.assetPath(s.ctx, a.Digest)
		if e != nil {
			continue
		}
		s.recordFacts(s.ctx, a, nil, path)
	}
}

func encodeLibraryThumb(src image.Image) ([]byte, error) {
	h := max(1, 480*src.Bounds().Dy()/max(1, src.Bounds().Dx()))
	h = min(h, 4096)
	im := image.NewRGBA(image.Rect(0, 0, 480, h))
	draw.CatmullRom.Scale(im, im.Bounds(), src, src.Bounds(), draw.Src, nil)
	var out bytes.Buffer
	if e := jpeg.Encode(&out, im, &jpeg.Options{Quality: 85}); e != nil {
		return nil, e
	}
	return out.Bytes(), nil
}

func (s *Service) reelLocalMediaPath(ctx context.Context, a LibraryAsset) string {
	if a.Source != "" {
		w, h := s.workingSize()
		s.mu.Lock()
		var candidate string
		for _, m := range s.reelClipIndex {
			if m.Link && m.Source == a.Source {
				key := linkCacheKey(m.Source, w, h, m.LoudnessMatch, m.TrimSilence)
				if ent, ok := s.linkCache[key]; ok && ent.Path != "" {
					candidate = ent.Path
					break
				}
			}
		}
		if candidate == "" {
			for _, loudness := range []bool{true, false} {
				for _, trim := range []bool{true, false} {
					key := linkCacheKey(a.Source, w, h, loudness, trim)
					if ent, ok := s.linkCache[key]; ok && ent.Path != "" {
						candidate = ent.Path
						break
					}
				}
				if candidate != "" {
					break
				}
			}
		}
		s.mu.Unlock()
		if candidate != "" {
			if st, err := os.Stat(candidate); err == nil && st.Size() > 0 {
				return candidate
			}
		}
	}
	rows, err := s.db.QueryContext(ctx, "SELECT id FROM onair_reel_clips WHERE source_digest=?", a.Digest)
	if err != nil {
		return ""
	}
	defer rows.Close()
	for rows.Next() {
		var clipID string
		if rows.Scan(&clipID) == nil && clipID != "" {
			p := filepath.Join(s.o.Dir, "reel-"+clipID+".mp4")
			if st, err := os.Stat(p); err == nil && st.Size() > 0 {
				return p
			}
		}
	}
	return ""
}

func validYouTubeID(id string) bool {
	if len(id) < 6 || len(id) > 20 {
		return false
	}
	for i := 0; i < len(id); i++ {
		c := id[i]
		if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '-' || c == '_') {
			return false
		}
	}
	return true
}

func linkThumbURL(raw, archiveOrigin string) (string, bool) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") {
		return "", false
	}
	host := strings.ToLower(u.Hostname())
	archiveHost := ""
	if au, err := url.Parse(archiveOrigin); err == nil {
		archiveHost = strings.ToLower(au.Hostname())
	}
	if host == "archive.org" || host == "www.archive.org" || (archiveHost != "" && host == archiveHost) {
		parts := strings.Split(strings.Trim(u.Path, "/"), "/")
		if len(parts) >= 2 && (parts[0] == "details" || parts[0] == "download") && parts[1] != "" {
			return archiveOrigin + "/services/img/" + url.PathEscape(parts[1]), archiveOrigin != "https://archive.org"
		}
	}
	if host == "youtu.be" {
		id := strings.Split(strings.Trim(u.Path, "/"), "/")[0]
		if validYouTubeID(id) {
			return "https://i.ytimg.com/vi/" + url.PathEscape(id) + "/hqdefault.jpg", false
		}
	}
	if host == "youtube.com" || host == "www.youtube.com" || host == "m.youtube.com" {
		id := strings.TrimSpace(u.Query().Get("v"))
		if id == "" {
			parts := strings.Split(strings.Trim(u.Path, "/"), "/")
			if len(parts) >= 2 && (parts[0] == "shorts" || parts[0] == "embed") {
				id = parts[1]
			}
		}
		if validYouTubeID(id) {
			return "https://i.ytimg.com/vi/" + url.PathEscape(id) + "/hqdefault.jpg", false
		}
	}
	return "", false
}

func (s *Service) fetchLinkPoster(ctx context.Context, raw string) ([]byte, error) {
	thumbURL, customArchive := linkThumbURL(raw, s.archiveOrigin())
	if thumbURL == "" {
		return nil, ErrNotFound
	}
	tctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	client, closeIdle := s.importHTTPClient()
	defer closeIdle()
	var (
		resp *http.Response
		err  error
	)
	if customArchive {
		req, reqErr := http.NewRequestWithContext(tctx, http.MethodGet, thumbURL, nil)
		if reqErr != nil {
			return nil, reqErr
		}
		resp, err = client.Do(req)
		if err != nil {
			return nil, err
		}
		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			return nil, ErrNotFound
		}
	} else {
		resp, err = importGETValidated(tctx, client, thumbURL, nil, s.validateImportURL)
		if err != nil {
			return nil, ErrNotFound
		}
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil || len(data) == 0 {
		return nil, ErrNotFound
	}
	src, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, ErrNotFound
	}
	return encodeLibraryThumb(src)
}

func (s *Service) libraryDerived(ctx context.Context, a LibraryAsset, wave bool) ([]byte, error) {
	suffix := ".jpg"
	if wave {
		suffix = ".wave.json"
	}
	path := filepath.Join(s.o.Dir, "library-"+a.Digest+suffix)
	if b, e := os.ReadFile(path); e == nil && len(b) > 0 {
		return b, nil
	}
	source, e := s.assetPath(ctx, a.Digest)
	if errors.Is(e, ErrNotFound) && a.Kind == "reel" {
		if local := s.reelLocalMediaPath(ctx, a); local != "" {
			source = local
			e = nil
		} else if !wave && a.Source != "" {
			b, posterErr := s.fetchLinkPoster(ctx, a.Source)
			if posterErr == nil && len(b) > 0 {
				_ = os.WriteFile(path, b, 0600)
				return b, nil
			}
		}
	}
	if e != nil {
		return nil, e
	}
	var out bytes.Buffer
	if !wave && (a.Kind == "image" || a.Kind == "font") {
		data, e := os.ReadFile(source)
		if e != nil {
			return nil, e
		}
		var src image.Image
		if a.MIME == "image/svg+xml" {
			var err error
			data, _, err = artwork.NewSVGRasterizer().Rasterize(ctx, a.Digest, data, artwork.RasterOptions{Width: 480})
			if err != nil {
				return nil, err
			}
		}
		if a.Kind == "image" {
			var e error
			src, _, e = image.Decode(bytes.NewReader(data))
			if e != nil {
				return nil, e
			}
		} else {
			f, e := opentype.Parse(data)
			if e != nil {
				return nil, e
			}
			face, e := opentype.NewFace(f, &opentype.FaceOptions{Size: 150, DPI: 72})
			if e != nil {
				return nil, e
			}
			defer face.Close()
			im := image.NewRGBA(image.Rect(0, 0, 480, 270))
			draw.Draw(im, im.Bounds(), image.NewUniform(color.RGBA{24, 28, 42, 255}), image.Point{}, draw.Src)
			d := font.Drawer{Dst: im, Src: image.White, Face: face, Dot: fixed.P(110, 190)}
			d.DrawString("Aa")
			src = im
		}
		encoded, e := encodeLibraryThumb(src)
		if e != nil {
			return nil, e
		}
		out.Write(encoded)
	} else {
		if s.o.FFmpeg == nil {
			return nil, fmt.Errorf("media tools unavailable")
		}
		cctx, cancel := context.WithTimeout(ctx, 35*time.Second)
		defer cancel()
		if wave {
			args := []string{"-nostdin", "-hide_banner", "-loglevel", "error", "-protocol_whitelist", "file,pipe", "-i", source, "-t", "30", "-vn", "-ac", "1", "-ar", "8000", "-f", "s16le", "pipe:1"}
			cmd := exec.CommandContext(cctx, s.o.FFmpeg(), args...)
			cmd.Stdout = &out
			if e = cmd.Run(); e != nil {
				return nil, e
			}
			pcm := out.Bytes()
			peaks := make([]float64, 1000)
			samples := len(pcm) / 2
			for i := 0; i < samples; i++ {
				p := math.Abs(float64(int16(binary.LittleEndian.Uint16(pcm[i*2:])))) / 32768
				bucket := min(999, i*1000/max(1, samples))
				peaks[bucket] = max(peaks[bucket], p)
			}
			b, _ := json.Marshal(map[string]any{"peaks": peaks, "durationMs": a.DurationMS})
			out.Reset()
			out.Write(b)
		} else {
			durSec := float64(a.DurationMS) / 1000
			seek := 0.5
			if durSec > 0 {
				seek = math.Min(3, math.Max(0, math.Min(durSec*0.15, durSec-0.05)))
			}
			extract := func(ss float64) error {
				out.Reset()
				args := []string{"-nostdin", "-hide_banner", "-loglevel", "error", "-ss", fmt.Sprintf("%.2f", ss), "-protocol_whitelist", "file,pipe", "-i", source, "-frames:v", "1", "-vf", "scale=480:-2", "-threads", "1", "-f", "image2pipe", "-vcodec", "mjpeg", "pipe:1"}
				cmd := exec.CommandContext(cctx, s.o.FFmpeg(), args...)
				cmd.Stdout = &out
				return cmd.Run()
			}
			e = extract(seek)
			if (e != nil || out.Len() == 0) && seek > 0 {
				e = extract(0)
			}
			if e != nil {
				return nil, e
			}
			if out.Len() == 0 {
				return nil, fmt.Errorf("video thumbnail unavailable")
			}
		}
	}
	b := out.Bytes()
	if e := os.WriteFile(path, b, 0600); e != nil {
		return nil, e
	}
	return b, nil
}
func (s *Service) LibraryRoutes(r chi.Router) {
	r.Get("/assets/{id}/info", func(w http.ResponseWriter, r *http.Request) {
		v, e := s.libraryAsset(r.Context(), chi.URLParam(r, "id"))
		result(w, v, e)
	})
	for _, part := range []string{"thumb", "waveform"} {
		part := part
		r.Get("/assets/{id}/"+part, func(w http.ResponseWriter, r *http.Request) {
			a, e := s.libraryAssetRow(r.Context(), chi.URLParam(r, "id"))
			if e != nil {
				result(w, nil, e)
				return
			}
			b, e := s.libraryDerived(r.Context(), LibraryAsset{Asset: a, MIME: a.MIME}, part == "waveform")
			if e != nil {
				result(w, nil, e)
				return
			}
			mime := "image/jpeg"
			if part == "waveform" {
				mime = "application/json"
			}
			w.Header().Set("Content-Type", mime)
			w.Header().Set("ETag", `"`+a.Digest+"-"+part+`"`)
			http.ServeContent(w, r, part, time.Time{}, bytes.NewReader(b))
		})
	}
	r.Patch("/assets/{id}", func(w http.ResponseWriter, r *http.Request) {
		var p struct {
			Name        string  `json:"name"`
			License     *string `json:"license"`
			Attribution *string `json:"attribution"`
		}
		if !body(w, r, &p) {
			return
		}
		d := chi.URLParam(r, "id")
		a, e := s.libraryAssetRow(r.Context(), d)
		if e != nil {
			result(w, nil, e)
			return
		}
		if strings.TrimSpace(p.Name) == "" {
			p.Name = a.Name
		}
		license, attribution := a.License, a.Attribution
		if p.License != nil {
			license = *p.License
		}
		if a.Kind == "font" && strings.TrimSpace(license) == "" {
			result(w, nil, fmt.Errorf("font licence required"))
			return
		}
		if p.Attribution != nil {
			attribution = *p.Attribution
		}
		_, e = s.db.ExecContext(r.Context(), "UPDATE onair_assets SET name=?,license=?,attribution=? WHERE digest=?", p.Name, license, attribution, d)
		if e != nil {
			result(w, nil, e)
			return
		}
		v, e := s.libraryAsset(r.Context(), d)
		result(w, v, e)
	})
	r.Post("/assets/{id}/replace", s.replaceLibraryAsset)
	r.Post("/assets/{id}/trim", s.trimLibraryAsset)
}
func (s *Service) replaceLibraryAsset(w http.ResponseWriter, r *http.Request) {
	old, e := s.libraryAsset(r.Context(), chi.URLParam(r, "id"))
	if e != nil {
		result(w, nil, e)
		return
	}
	if !multipart(w, r, assetLimit) {
		return
	}
	defer r.MultipartForm.RemoveAll()
	f, h, e := r.FormFile("file")
	if e != nil {
		result(w, nil, e)
		return
	}
	defer f.Close()
	a, e := s.UploadAsset(r.Context(), h.Filename, old.Kind, old.License, f)
	if e != nil {
		result(w, nil, e)
		return
	}
	if a.Digest == old.Digest {
		result(w, old, nil)
		return
	}
	s.assetMu.Lock()
	locked := true
	defer func() {
		if locked {
			s.assetMu.Unlock()
		}
	}()
	tx, e := s.db.BeginTx(r.Context(), nil)
	if e != nil {
		result(w, nil, e)
		return
	}
	defer tx.Rollback()
	tables := []string{"onair_looks", "onair_families", "onair_reels"}
	if ok, _ := s.hasChannelAssets(r.Context()); ok {
		tables = append(tables, "livetv_channels")
	}
	for _, table := range tables {
		if _, e = tx.ExecContext(r.Context(), "UPDATE "+table+" SET data=REPLACE(data,?,?) WHERE data LIKE ?", old.Digest, a.Digest, "%"+old.Digest+"%"); e != nil {
			result(w, nil, e)
			return
		}
	}
	if _, e = tx.ExecContext(r.Context(), "UPDATE onair_reel_clips SET data=REPLACE(data,?,?) WHERE data LIKE ?", old.Digest, a.Digest, "%"+old.Digest+"%"); e != nil {
		result(w, nil, e)
		return
	}
	for _, q := range []string{"INSERT INTO onair_look_assets(look_id,digest) SELECT look_id,? FROM onair_look_assets WHERE digest=? ON CONFLICT(look_id,digest) DO NOTHING", "UPDATE onair_reel_clips SET source_digest=? WHERE source_digest=?"} {
		if _, e = tx.ExecContext(r.Context(), q, a.Digest, old.Digest); e != nil {
			result(w, nil, e)
			return
		}
	}
	if _, e = tx.ExecContext(r.Context(), "DELETE FROM onair_look_assets WHERE digest=?", old.Digest); e == nil {
		_, e = tx.ExecContext(r.Context(), "DELETE FROM onair_assets WHERE digest=?", old.Digest)
	}
	if e == nil {
		_, e = tx.ExecContext(r.Context(), "DELETE FROM onair_asset_replicas WHERE digest=?", old.Digest)
	}
	if e == nil {
		e = tx.Commit()
	}
	if e != nil {
		result(w, nil, e)
		return
	}
	s.removeUnreferenced(r.Context(), old.Digest)
	s.assetMu.Unlock()
	locked = false
	if s.o.Changed != nil {
		_ = s.o.Changed(r.Context())
	}
	v, e := s.libraryAsset(r.Context(), a.Digest)
	result(w, v, e)
}
func (s *Service) trimLibraryAsset(w http.ResponseWriter, r *http.Request) {
	a, e := s.libraryAssetRow(r.Context(), chi.URLParam(r, "id"))
	if e != nil {
		result(w, nil, e)
		return
	}
	var p struct {
		Start, End float64
		Name       string
	}
	if !body(w, r, &p) {
		return
	}
	if s.o.FFmpeg == nil || (a.Kind != "audio" && a.Kind != "video") || math.IsNaN(p.Start) || math.IsNaN(p.End) || p.Start < 0 || p.End <= p.Start || p.End-p.Start > 30 || p.End > float64(a.DurationMS)/1000 {
		result(w, nil, fmt.Errorf("invalid clip interval"))
		return
	}
	source, e := s.assetPath(r.Context(), a.Digest)
	if e != nil {
		result(w, nil, e)
		return
	}
	f, e := os.CreateTemp(s.o.Dir, "library-trim-*")
	if e != nil {
		result(w, nil, e)
		return
	}
	_ = f.Close()
	os.Remove(f.Name())
	ext := ".mp4"
	if a.Kind == "audio" {
		ext = ".m4a"
	}
	out := f.Name() + ext
	defer os.Remove(out)
	ctx, cancel := context.WithTimeout(r.Context(), 45*time.Second)
	defer cancel()
	args := []string{"-nostdin", "-y", "-loglevel", "error", "-protocol_whitelist", "file,pipe", "-i", source, "-ss", fmt.Sprint(p.Start), "-t", fmt.Sprint(p.End - p.Start), "-threads", "1"}
	if a.Kind == "audio" {
		args = append(args, "-vn", "-c:a", "aac")
	} else {
		args = append(args, "-c:v", "libx264", "-c:a", "aac")
	}
	e = exec.CommandContext(ctx, s.o.FFmpeg(), append(args, out)...).Run()
	if e != nil {
		result(w, nil, e)
		return
	}
	file, e := os.Open(out)
	if e != nil {
		result(w, nil, e)
		return
	}
	defer file.Close()
	name := strings.TrimSpace(p.Name)
	if name == "" {
		name = a.Name
	}
	asset, e := s.UploadAsset(r.Context(), name, a.Kind, a.License, io.LimitReader(file, assetLimit+1))
	if e != nil {
		result(w, nil, e)
		return
	}
	if _, e = s.db.ExecContext(r.Context(), "UPDATE onair_assets SET source=?,attribution=? WHERE digest=?", a.Source, a.Attribution, asset.Digest); e != nil {
		result(w, nil, e)
		return
	}
	v, e := s.libraryAsset(r.Context(), asset.Digest)
	result(w, v, e)
}
