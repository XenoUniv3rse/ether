package app

import (
	"archive/zip"
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontends/vela"
	"github.com/braedonsaunders/ether/internal/livetv"
	"github.com/braedonsaunders/ether/internal/personalize"
)

// Give viewers without a generated household playlist a real item and queue
// to open while leaving their watch history and personalization cold.
func seedPerfMatrixPlaylistCoverage(ctx context.Context, db *sql.DB) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := time.Now().UnixNano()
	if _, err := tx.ExecContext(ctx, `INSERT INTO state_playlists(user_id,title,smart,updated_at,added_at,guid)
		SELECT u.id,'First playlist',0,?,?,'matrix-first-' || CAST(u.id AS TEXT)
		FROM auth_users u WHERE u.active=1 AND NOT EXISTS(SELECT 1 FROM state_playlists p WHERE p.user_id=u.id)`, now, now); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO state_playlist_items(playlist_id,position,rating_key,added_at)
		SELECT p.id,0,t.key,? FROM state_playlists p
		CROSS JOIN (SELECT key FROM catalog_titles WHERE type='movie' AND adult=0 AND browsable=1 ORDER BY key LIMIT 1) t
		WHERE p.guid LIKE 'matrix-first-%'`, now); err != nil {
		return err
	}
	return tx.Commit()
}

const matrixAvatarURL = "https://plex.tv/matrix/fixture-avatar.jpg"

func seedPerfMatrixCatalogue(ctx context.Context, db *sql.DB, live, exercisePluginHost bool) error {
	now := time.Now().UTC()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	for _, q := range []string{
		"UPDATE auth_users SET restricted=1, allowed_libraries='[1,2]', max_content_rating='PG' WHERE username='viewer02'",
		"INSERT INTO auth_users(username,title,plex_id,is_home,origin,active,created_at,updated_at) VALUES('home-member','Home member',9100,1,'plex',1,?,?)",
		"UPDATE auth_users SET plex_id=9101, is_home=1 WHERE is_owner=1",
		"UPDATE catalog_titles SET released_ts=?, primary_released_ts=?, status='Post Production' WHERE type='movie' AND tmdb_id<=100",
		"UPDATE catalog_titles SET released_ts=?, primary_released_ts=? WHERE type='episode' AND season=3 AND episode=6 AND tmdb_id<=100",
		"UPDATE catalog_credits SET thumb='/p.jpg' WHERE kind='cast' AND key IN (SELECT key FROM catalog_titles WHERE type='movie' AND adult=0 ORDER BY popularity DESC, key LIMIT 100)",
	} {
		var args []any
		if strings.Contains(q, "?") {
			at := now
			if strings.HasPrefix(q, "UPDATE catalog_titles") {
				at = at.AddDate(0, 0, 7)
			}
			args = []any{at.Unix(), at.Unix()}
		}
		if _, err := tx.ExecContext(ctx, q, args...); err != nil {
			return err
		}
	}
	if err := seedPerfRemotePlugin(ctx, tx, now.Unix(), exercisePluginHost); err != nil {
		return err
	}
	if err := seedPerfMatrixCollections(ctx, tx, now); err != nil {
		return err
	}
	if live {
		if err := seedPerfMatrixLive(ctx, tx, now); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func seedPerfMatrixCollections(ctx context.Context, tx *sql.Tx, now time.Time) error {
	for i := range 240 {
		kind, media := "movie", core.TypeMovie
		if i%4 == 0 {
			kind, media = "show", core.TypeShow
		}
		spec := core.CollectionSpec{
			Name: fmt.Sprintf("Fixture collection %03d", i), MediaType: kind,
			Sort: "source", Refresh: "weekly", Enabled: true, BrowseInApps: true,
			Sources: []core.CollectionSource{{Kind: "titles"}},
		}
		if i%3 != 0 {
			spec.Folder = fmt.Sprintf("Fixture folder %02d", i/12)
			spec.SortOrder = 1 + i%12
		}
		if i%3 == 2 {
			spec.AddonSync = &core.CollectionAddonSync{
				AddonID: "community.example.fixture", AddonName: "Fixture add-on",
				CatalogType: kind, CatalogKey: fmt.Sprintf("fixture-%d", i), CatalogID: fmt.Sprintf("fixture-%d", i),
			}
		}
		keys := make([]core.RatingKey, 48)
		for j := range keys {
			id := int64(101 + (i*19+j)%5_000)
			key := core.MovieKey(id)
			if media == core.TypeShow {
				key = core.ShowKey(id)
			}
			keys[j] = key
			spec.Sources[0].Keys = append(spec.Sources[0].Keys, fmt.Sprint(key))
		}
		if i%7 == 0 {
			spec.Limit = len(keys)
		}
		doc, err := json.Marshal(spec)
		if err != nil {
			return err
		}
		group, rank := "", int64(math.MaxInt64)
		if spec.Folder != "" {
			group, rank = "folder:"+strings.ToLower(spec.Folder), int64(spec.SortOrder-1)
		}
		if spec.AddonSync != nil {
			group = "addon:" + spec.AddonSync.AddonID
		}
		// Mix ready-made collections with request-time member filtering.
		mode := 1
		if i%7 == 0 {
			mode = 2
		}
		var id int64
		err = tx.QueryRowContext(ctx, `INSERT INTO collections_specs(spec_json, created_at, updated_at, last_built_at, changed_at, item_count, browse_group, browse_rank, browse_type, browse_mode, build_hash)
			VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?) RETURNING id`, string(doc), now.Unix(), now.Unix(), now.Unix(), now.Unix(), len(keys), group, rank, kind, mode, fmt.Sprintf("matrix-build-%d", i)).Scan(&id)
		if err != nil {
			return err
		}
		for pos, key := range keys {
			if _, err := tx.ExecContext(ctx, "INSERT INTO collections_members(spec_id,pos,key) VALUES(?,?,?)", id, pos, key); err != nil {
				return err
			}
		}
	}
	return nil
}

func seedPerfMatrixLive(ctx context.Context, tx *sql.Tx, now time.Time) error {
	const sourceID = "matrix-guide"
	source := livetv.Source{ID: sourceID, Name: "Fixture guide", UpdatedAt: now, RefreshHours: -1}
	doc, err := json.Marshal(source)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, "INSERT INTO livetv_sources(id,data) VALUES (?,?)", sourceID, string(doc)); err != nil {
		return err
	}
	start := now.Truncate(time.Hour).Add(-time.Hour)
	for ch := range 300 {
		id := fmt.Sprintf("matrix-%03d", ch)
		channel := livetv.Channel{
			ID: id, GuideID: id, SourceID: sourceID, Kind: "iptv", Enabled: true,
			Name: fmt.Sprintf("Fixture channel %03d", ch), Number: fmt.Sprint(ch + 1),
			StreamURL: "http://127.0.0.1:1/fixture.ts", Group: "Entertainment",
		}
		if ch%6 == 0 {
			channel.Group = "Sports"
		}
		doc, err := json.Marshal(channel)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, "INSERT INTO livetv_channels(id,data) VALUES (?,?)", id, string(doc)); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, "INSERT INTO livetv_guide_channels(source_id,guide_id,name) VALUES (?,?,?)", sourceID, id, channel.Name); err != nil {
			return err
		}
		for hour := range 7 * 24 {
			at := start.Add(time.Duration(hour) * time.Hour)
			p := livetv.Programme{
				ID: fmt.Sprintf("%s-%d", id, hour), ChannelID: id, SourceID: sourceID, Start: at, End: at.Add(time.Hour),
				Item: livetv.Item{Title: fmt.Sprintf("Fixture programme %03d", hour), Genres: []string{channel.Group}, Live: channel.Group == "Sports", DurationMS: time.Hour.Milliseconds()},
			}
			doc, err := json.Marshal(p)
			if err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, "INSERT INTO livetv_programmes(source_id,channel_id,start_ms,end_ms,data) VALUES (?,?,?,?,?)",
				sourceID, id, p.Start.UnixMilli(), p.End.UnixMilli(), string(doc)); err != nil {
				return err
			}
		}
	}
	return nil
}

func seedPerfMatrixRows(ctx context.Context, a *App) error {
	for _, key := range []core.PageKey{core.PageHome, core.LibraryPage(core.LibraryMovies, "recommendations"), core.LibraryPage(core.LibraryShows, "recommendations")} {
		sources, err := a.personalize.PageSources(ctx, key)
		if err != nil {
			return err
		}
		page, _ := a.personalize.Page(ctx, key)
		page.Mode = core.PageCurated
		page.Rows = nil
		for _, source := range sources.Sources {
			if !source.Available {
				continue
			}
			row := core.Row{Source: core.RowSource{Kind: source.Kind, Params: map[string]string{}}, Placement: core.RowPlacement{Mode: core.PlacementShow}}
			if source.Library != 0 {
				row.Scope.Libraries = []core.LibraryID{source.Library}
			}
			valid := true
			for _, param := range source.Params {
				value := param.Default
				switch {
				case param.Type == core.ParamRef || param.Type == core.ParamCollectionList:
					if len(source.Refs) > 0 {
						value = source.Refs[0].Value
					}
				case param.Type == core.ParamFacetValue && param.Key == "genre":
					value = "Drama"
				}
				if value != "" {
					row.Source.Params[param.Key] = value
				} else if param.Required {
					valid = false
				}
			}
			if valid {
				page.Rows = append(page.Rows, row)
			}
		}
		if _, err := a.personalize.SavePage(ctx, page); err != nil {
			return fmt.Errorf("save %s: %w", key, err)
		}
	}
	return nil
}

func seedPerfMatrixCollectionRow(ctx context.Context, a *App) error {
	page, _ := a.personalize.Page(ctx, core.PageHome)
	row := core.Row{
		Name:      "Matrix collection cards",
		Source:    core.RowSource{Kind: personalize.SourceCollectionCards, Params: map[string]string{"match": "Fixture collection 001"}},
		Placement: core.RowPlacement{Mode: core.PlacementShow},
	}
	page.Rows = append([]core.Row{row}, page.Rows...)
	_, err := a.personalize.SavePage(ctx, page)
	return err
}

// Install the same archive format used by the console so the public asset
// requests hit real installed documents and files in the per-case copy.
func seedPerfMatrixSkins(ctx context.Context, a *App, preview []byte) error {
	for _, manifest := range []string{
		`{"schema":1,"id":"matrix-pack","name":"Matrix pack","version":"1.0.0","engine":"cinematic","swatches":["#10141c","#f4f7fb","#38bdf8"],"images":{"preview":"assets/preview.jpg"},"palette":{"canvas":"#10141c","surface":"#131720","ink":"#f4f7fb","accent":"#38bdf8"}}`,
		`{"schema":1,"renderer":1,"id":"matrix-document","name":"Matrix document","version":"1.0.0","swatches":["#10141c","#f4f7fb","#38bdf8"],"preview":"assets/preview.jpg","ui":{"density":"compact"},"routes":{"home":{"template":"grid"}}}`,
	} {
		var data bytes.Buffer
		archive := zip.NewWriter(&data)
		for _, file := range []struct {
			name string
			data []byte
		}{{"skin.json", []byte(manifest)}, {"assets/preview.jpg", preview}} {
			dst, err := archive.Create(file.name)
			if err != nil {
				return err
			}
			if _, err := dst.Write(file.data); err != nil {
				return err
			}
		}
		if err := archive.Close(); err != nil {
			return err
		}
		if _, err := a.velaSkinsStore().InstallArchive(ctx, data.Bytes()); err != nil {
			return err
		}
	}
	return nil
}

func seedPerfMatrixBranding(ctx context.Context, a *App) error {
	const svg = `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 160 40"><rect width="160" height="40" fill="#202838"/></svg>`
	wordmark, err := vela.UploadWordmark(ctx, a.db, strings.NewReader(svg))
	if err != nil {
		return err
	}
	return a.cfg.Update(func(cfg *config.Config) error {
		surface := cfg.Frontends["vela"]
		if surface.Settings == nil {
			surface.Settings = make(map[string]any)
		}
		surface.Settings["wordmark"] = map[string]any{"url": wordmark.URL, "width": wordmark.Width, "height": wordmark.Height}
		cfg.Frontends["vela"] = surface
		return nil
	})
}
