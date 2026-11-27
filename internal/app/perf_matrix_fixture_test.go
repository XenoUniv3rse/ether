package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/livetv"
)

func seedPerfMatrixCatalogue(ctx context.Context, db *sql.DB, live bool) error {
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
	if err := seedPerfRemotePlugin(ctx, tx, now.Unix()); err != nil {
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
