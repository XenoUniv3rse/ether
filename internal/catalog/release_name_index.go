package catalog

import (
	"context"
	"database/sql"
	"math"

	"github.com/braedonsaunders/ether/internal/releaseparse"
	"github.com/braedonsaunders/ether/internal/store"
)

// Release matching needs exactly the same normalization as releaseparse; the
// browse sort key has different article, punctuation and original-title rules.
func init() {
	store.Register("catalog", 32, `SELECT 1;`)
	store.RegisterApply("catalog", 32, migrateReleaseNames)
}

func migrateReleaseNames(ctx context.Context, tx *sql.Tx) error {
	if _, err := tx.ExecContext(ctx, `CREATE TABLE catalog_release_names (name TEXT NOT NULL, key BIGINT NOT NULL, PRIMARY KEY(name,key))`); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `CREATE INDEX catalog_release_names_key ON catalog_release_names(key)`); err != nil {
		return err
	}
	last := int64(math.MinInt64)
	for {
		rows, err := tx.QueryContext(ctx, `SELECT key,title,original_title FROM catalog_titles WHERE key>? AND type IN ('movie','show') ORDER BY key LIMIT 500`, last)
		if err != nil {
			return err
		}
		type row struct {
			key             int64
			title, original string
		}
		var batch []row
		for rows.Next() {
			var r row
			if err = rows.Scan(&r.key, &r.title, &r.original); err != nil {
				rows.Close()
				return err
			}
			batch = append(batch, r)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		if len(batch) == 0 {
			return nil
		}
		for _, r := range batch {
			if err = insertReleaseNames(ctx, tx, r.key, r.title, r.original); err != nil {
				return err
			}
			last = r.key
		}
	}
}

func insertReleaseNames(ctx context.Context, tx *sql.Tx, key int64, names ...string) error {
	for _, name := range names {
		name = releaseparse.NormalizeTitle(name)
		if name == "" {
			continue
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO catalog_release_names(name,key) VALUES(?,?) ON CONFLICT(name,key) DO NOTHING`, name, key); err != nil {
			return err
		}
	}
	return nil
}

func updateReleaseNames(ctx context.Context, tx *sql.Tx, key int64) error {
	var typ, title, original string
	if err := tx.QueryRowContext(ctx, `SELECT type,title,original_title FROM catalog_titles WHERE key=?`, key).Scan(&typ, &title, &original); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM catalog_release_names WHERE key=?`, key); err != nil {
		return err
	}
	if typ != "movie" && typ != "show" {
		return nil
	}
	return insertReleaseNames(ctx, tx, key, title, original)
}
