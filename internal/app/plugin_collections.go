package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"

	"github.com/braedonsaunders/ether/internal/collections"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/plugins"
	"github.com/braedonsaunders/ether/internal/store"
)

func init() {
	store.Register("plugin_collections", 1, `CREATE TABLE plugin_collection_links(plugin TEXT NOT NULL, local_id TEXT NOT NULL, collection_id TEXT NOT NULL, PRIMARY KEY(plugin,local_id));`)
}

// Serialize local writes through mapping creation and collection mutation. The
// unique database key also prevents a second node taking over a plugin's link.
var pluginCollectionMu sync.Mutex

type pluginCollectionDocument struct {
	core.CollectionSpec
	Items   *[]string `json:"items"`
	ItemIDs *[]string `json:"itemIds"`
}

func decodePluginCollection(raw json.RawMessage) (pluginCollectionDocument, string, error) {
	var doc pluginCollectionDocument
	if len(raw) > 1<<20 {
		return doc, "", collectionInput("collection document is too large")
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return doc, "", collectionInput("collection document must be valid JSON")
	}
	id := strings.TrimSpace(doc.ID)
	if id == "" {
		id = strings.TrimSpace(doc.Name)
	}
	if id == "" || len(id) > 256 {
		return doc, "", collectionInput("collection needs a stable id or name (up to 256 characters)")
	}
	if doc.Items != nil && doc.ItemIDs != nil {
		return doc, "", collectionInput("use either items or itemIds")
	}
	return doc, id, nil
}

func (a *App) pluginCollectionUpsert(ctx context.Context, raw json.RawMessage) (json.RawMessage, error) {
	plugin := plugins.CollectionPluginID(ctx)
	if plugin == "" {
		return nil, plugins.ErrPermissionDenied
	}
	doc, localID, err := decodePluginCollection(raw)
	if err != nil {
		return nil, err
	}
	if a.collections == nil {
		return nil, plugins.ErrUnavailable
	}
	items := doc.Items
	if items == nil {
		items = doc.ItemIDs
	}
	if items != nil {
		if len(*items) > 200 {
			return nil, collectionInput("a plugin collection accepts at most 200 items per write")
		}
		keys := make([]string, 0, len(*items))
		for _, id := range *items {
			key, err := a.pluginCollectionTitle(ctx, id)
			if err != nil {
				return nil, collectionInput(fmt.Sprintf("Collection item %q is invalid or not in the local catalogue. Use a catalogue source for titles that are not cached.", id))
			}
			keys = append(keys, key)
		}
		doc.Sources = []core.CollectionSource{{Kind: "titles", Keys: keys}}
		if doc.MediaType == "" {
			doc.MediaType = "mixed"
		}
		if doc.Sort == "" {
			doc.Sort = "source"
		}
		// The compact SDK form describes a published collection.
		var fields map[string]json.RawMessage
		_ = json.Unmarshal(raw, &fields)
		if _, explicit := fields["enabled"]; !explicit {
			doc.Enabled = true
		}
		if _, explicit := fields["targets"]; !explicit {
			doc.Targets.CollectionsTab = true
			doc.Targets.Users = core.AllUsers()
		}
	} else if len(doc.Sources) == 0 {
		return nil, collectionInput("collection must declare items, itemIds, or sources")
	}
	doc.OwnerUserID = 0
	pluginCollectionMu.Lock()
	defer pluginCollectionMu.Unlock()
	var serverID string
	err = a.db.QueryRowContext(ctx, `SELECT collection_id FROM plugin_collection_links WHERE plugin=? AND local_id=?`, plugin, localID).Scan(&serverID)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	if serverID != "" {
		previous, err := a.collections.GetSpec(ctx, serverID)
		if errors.Is(err, core.ErrNotFound) {
			if _, err = a.db.ExecContext(ctx, `DELETE FROM plugin_collection_links WHERE plugin=? AND local_id=?`, plugin, localID); err != nil {
				return nil, err
			}
		} else if err != nil {
			return nil, err
		} else {
			// Journal the actual previous collection, including owner edits, using the
			// plugin-local identifier so undo cannot target another plugin's collection.
			previous.ID = localID
			inverse, err := json.Marshal(previous)
			if err != nil {
				return nil, err
			}
			doc.ID = serverID
			_, err = a.collections.UpdateSpec(ctx, serverID, doc.CollectionSpec)
			return inverse, collectionServiceError(err)
		}
	}
	doc.ID = ""
	created, err := a.collections.CreateSpec(ctx, doc.CollectionSpec)
	if err != nil {
		return nil, collectionServiceError(err)
	}
	_, err = a.db.ExecContext(ctx, `INSERT INTO plugin_collection_links(plugin,local_id,collection_id) VALUES(?,?,?)`, plugin, localID, created.ID)
	if err != nil {
		cleanupErr := a.collections.DeleteSpec(ctx, created.ID)
		return nil, errors.Join(err, cleanupErr)
	}
	return nil, nil
}

func (a *App) pluginCollectionDelete(ctx context.Context, raw json.RawMessage) error {
	plugin := plugins.CollectionPluginID(ctx)
	if plugin == "" {
		return plugins.ErrPermissionDenied
	}
	_, id, err := decodePluginCollection(raw)
	if err != nil {
		return err
	}
	if a.collections == nil {
		return plugins.ErrUnavailable
	}
	pluginCollectionMu.Lock()
	defer pluginCollectionMu.Unlock()
	var serverID string
	err = a.db.QueryRowContext(ctx, `SELECT collection_id FROM plugin_collection_links WHERE plugin=? AND local_id=?`, plugin, id).Scan(&serverID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := a.collections.DeleteSpec(ctx, serverID); err != nil && !errors.Is(err, core.ErrNotFound) {
		return err
	}
	_, err = a.db.ExecContext(ctx, `DELETE FROM plugin_collection_links WHERE plugin=? AND local_id=?`, plugin, id)
	return err
}

func (a *App) pluginCollectionTitle(ctx context.Context, id string) (string, error) {
	if a.catalog == nil {
		return "", plugins.ErrUnavailable
	}
	id = strings.TrimSpace(id)
	if strings.HasPrefix(id, "tt") || strings.HasPrefix(id, "imdb:") {
		// The catalogue owns catalog_titles: resolve through its lookup, a
		// movie first and then a show, as the stored order did.
		imdb := strings.TrimPrefix(id, "imdb:")
		var found *core.Title
		for _, typ := range []core.MediaType{core.TypeMovie, core.TypeShow} {
			t, err := a.catalog.ByExternalIDForType(ctx, "imdb", imdb, typ)
			if err == nil && t != nil {
				found = t
				break
			}
			if cerr := ctx.Err(); cerr != nil {
				return "", cerr
			}
		}
		if found == nil {
			return "", core.ErrNotFound
		}
		title, err := a.catalog.CachedGet(ctx, found.Key)
		if err != nil {
			return "", err
		}
		if title == nil {
			return "", core.ErrNotFound
		}
		return title.Key.String(), nil
	}
	var key core.RatingKey
	var err error
	if strings.HasPrefix(id, "tmdb:") {
		n, e := strconv.ParseInt(strings.TrimPrefix(id, "tmdb:"), 10, 64)
		if e != nil || n <= 0 {
			return "", errors.New("invalid TMDB id")
		}
		key = core.MovieKey(n)
	} else {
		key, err = core.ParseRatingKey(id)
	}
	if err != nil {
		return "", err
	}
	title, err := a.catalog.CachedGet(ctx, key)
	if err != nil {
		return "", err
	}
	if title == nil {
		return "", core.ErrNotFound
	}
	return title.Key.String(), nil
}

func collectionInput(message string) error { return &plugins.CollectionInputError{Message: message} }

func collectionServiceError(err error) error {
	var invalid *collections.ValidationError
	if errors.As(err, &invalid) {
		return collectionInput(invalid.Error())
	}
	return err
}
