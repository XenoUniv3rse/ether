package collections

import (
	"context"
	"database/sql"
	"math"
	"strings"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/store"
)

func browseRank(rank int) int64 {
	if rank <= 0 {
		return math.MaxInt64
	}
	return int64(rank) - 1
}

func init() {
	store.Register("collections", 15, `SELECT 1;`)
	store.RegisterApply("collections", 15, migrateBrowseOrder)
	store.RegisterRestore("collections_browse_order", func(context.Context, *sql.Tx) (func(context.Context, *sql.Tx) error, error) {
		return backfillBrowseOrder, nil
	})
	store.RegisterDialect("collections", 16, store.SQLite, `
CREATE TRIGGER collections_browse_insert AFTER INSERT ON collections_specs BEGIN
	UPDATE collections_specs SET browse_anchor=NEW.id WHERE id=NEW.id AND NEW.browse_group='';
	`+refreshBrowseGroup("NEW")+`
END;
CREATE TRIGGER collections_browse_move AFTER UPDATE OF browse_group ON collections_specs
WHEN OLD.browse_group<>NEW.browse_group BEGIN
	UPDATE collections_specs SET browse_anchor=NEW.id WHERE id=NEW.id AND NEW.browse_group='';
	`+refreshBrowseGroup("OLD")+refreshBrowseGroup("NEW")+`
END;
CREATE TRIGGER collections_browse_delete AFTER DELETE ON collections_specs
WHEN OLD.browse_group<>'' AND OLD.id=OLD.browse_anchor BEGIN
	`+refreshBrowseGroup("OLD")+`
END;`)
	store.RegisterDialect("collections", 16, store.Postgres, `
CREATE FUNCTION collections_browse_lock() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
	-- Serialize changes to group minima across writers before row locks.
	PERFORM pg_advisory_xact_lock(186734219, 32);
	RETURN NULL;
END; $$;
CREATE TRIGGER collections_browse_lock BEFORE INSERT OR DELETE OR UPDATE OF browse_group ON collections_specs
FOR EACH STATEMENT EXECUTE FUNCTION collections_browse_lock();

CREATE FUNCTION collections_browse_order() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
	IF TG_OP='UPDATE' AND OLD.browse_group=NEW.browse_group THEN RETURN NULL; END IF;
	IF TG_OP<>'DELETE' THEN
		UPDATE collections_specs SET browse_anchor=NEW.id WHERE id=NEW.id AND NEW.browse_group='';
		`+refreshBrowseGroup("NEW")+`
	END IF;
	IF TG_OP<>'INSERT' AND OLD.id=OLD.browse_anchor THEN
		`+refreshBrowseGroup("OLD")+`
	END IF;
	RETURN NULL;
END; $$;
CREATE TRIGGER collections_browse_order AFTER INSERT OR DELETE OR UPDATE OF browse_group ON collections_specs
FOR EACH ROW EXECUTE FUNCTION collections_browse_order();`)
}

// refreshBrowseGroup updates the affected index range only when its anchor
// changes. Disabled and private specs still participate in the minimum.
func refreshBrowseGroup(ref string) string {
	return `UPDATE collections_specs SET browse_anchor=(
		SELECT MIN(a.id) FROM collections_specs a WHERE a.browse_group=` + ref + `.browse_group AND a.browse_group<>''
	) WHERE browse_group=` + ref + `.browse_group AND browse_group<>'' AND browse_anchor<>(
		SELECT MIN(a.id) FROM collections_specs a WHERE a.browse_group=` + ref + `.browse_group AND a.browse_group<>''
	);`
}

func browseGroup(builtin bool, spec core.CollectionSpec) string {
	if builtin {
		return ""
	}
	if spec.AddonSync != nil && spec.AddonSync.AddonID != "" {
		return "addon:" + spec.AddonSync.AddonID
	}
	if spec.Folder != "" {
		return "folder:" + strings.ToLower(spec.Folder)
	}
	return ""
}

// browseMode is kept beside the display tuple so an exact page can detect
// whether any candidate needs serving-time rule evaluation.
func browseMode(spec core.CollectionSpec) int {
	if !spec.BrowseInApps {
		return 0
	}
	if plainCollectionSpec(spec) {
		return 1
	}
	return 2
}

func migrateBrowseOrder(ctx context.Context, tx *sql.Tx) error {
	if _, err := tx.ExecContext(ctx, `
ALTER TABLE collections_specs ADD COLUMN browse_group TEXT NOT NULL DEFAULT '';
ALTER TABLE collections_specs ADD COLUMN browse_anchor BIGINT NOT NULL DEFAULT 0;
ALTER TABLE collections_specs ADD COLUMN browse_rank BIGINT NOT NULL DEFAULT 9223372036854775807;
ALTER TABLE collections_specs ADD COLUMN browse_type TEXT NOT NULL DEFAULT '';
ALTER TABLE collections_specs ADD COLUMN browse_mode INTEGER NOT NULL DEFAULT -1;
CREATE INDEX collections_specs_browse_mode ON collections_specs (browse_mode,browse_type) WHERE enabled=1;
CREATE INDEX collections_specs_browse_group ON collections_specs (browse_group,id) WHERE browse_group<>'';
CREATE INDEX collections_specs_browse_display ON collections_specs (builtin DESC,browse_anchor,browse_rank,id) WHERE enabled=1;
DROP INDEX collections_specs_browse_order;
DROP INDEX collections_specs_grouped;`); err != nil {
		return err
	}

	return backfillBrowseOrder(ctx, tx)
}

func backfillBrowseOrder(ctx context.Context, tx *sql.Tx) error {
	rows, err := tx.QueryContext(ctx, `SELECT `+specCols+` FROM collections_specs`)
	if err != nil {
		return err
	}
	type position struct {
		id    int64
		group string
		rank  int
		typ   string
		mode  int
	}
	var positions []position
	anchors := make(map[string]int64)
	for rows.Next() {
		row, err := scanSpecRow(rows)
		if row == nil {
			rows.Close()
			return err
		}
		p := position{id: row.id, mode: -1}
		if err == nil {
			p.group, p.rank = browseGroup(row.builtin, row.spec), row.spec.SortOrder
			p.typ, p.mode = row.spec.MediaType, browseMode(row.spec)
		}
		positions = append(positions, p)
		if a, ok := anchors[p.group]; !ok || p.id < a {
			anchors[p.group] = p.id
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, p := range positions {
		anchor := p.id
		if p.group != "" {
			anchor = anchors[p.group]
		}
		if _, err := tx.ExecContext(ctx, `UPDATE collections_specs SET browse_group=?,browse_anchor=?,browse_rank=?,browse_type=?,browse_mode=? WHERE id=?`, p.group, anchor, browseRank(p.rank), p.typ, p.mode, p.id); err != nil {
			return err
		}
	}
	return nil
}
