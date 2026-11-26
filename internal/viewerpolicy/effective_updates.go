package viewerpolicy

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/braedonsaunders/ether/internal/store"
)

// Refresh timestamps and build reports can change without changing access
// or row membership. Compare the fields that affect a viewer's visit.
var effectiveUpdates = []struct {
	table, columns string
}{
	{"auth_users", authPolicyColumns},
	{"collections_specs", "id, slug, builtin, owner_user_id, spec_json, enabled, browse_group, browse_anchor, browse_rank, browse_type, browse_mode"},
	{"identity_links", "id, viewer_id, provider, subject, password_hash"},
	{"auth_plex_defaults", "user_id, plex_id, payload"},
	{"catalog_home_release", "key, checked, earliest_ts, fallback_admitted"},
}

func effectiveUpdate(table, columns string, dialect store.Dialect) string {
	operator := "IS NOT"
	body := "BEGIN " + sqliteBump + " END"
	if dialect == store.Postgres {
		operator = "IS DISTINCT FROM"
		body = "EXECUTE FUNCTION viewer_policy_bump()"
	}
	var changed []string
	for _, c := range strings.Split(columns, ", ") {
		changed = append(changed, "OLD."+c+" "+operator+" NEW."+c)
	}
	return fmt.Sprintf("CREATE TRIGGER viewer_policy_%s_update AFTER UPDATE OF %s ON %s FOR EACH ROW WHEN (%s) %s",
		table, columns, table, strings.Join(changed, " OR "), body)
}

func init() {
	// These lookups feed collection builds. A build publishes any actual
	// membership change through collections_members.
	collectionCaches := []string{"collections_names", "collections_external"}
	store.Register("viewer_policy", 9, "SELECT 1;")
	store.RegisterSQLiteApply("viewer_policy", 9, func(ctx context.Context, tx *sql.Tx) error {
		for _, update := range effectiveUpdates {
			var exists int
			if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name=?`, update.table).Scan(&exists); err != nil {
				return err
			}
			if exists == 0 {
				continue
			}
			name := "viewer_policy_" + update.table + "_update"
			sql := "DROP TRIGGER IF EXISTS " + name + "; " + effectiveUpdate(update.table, update.columns, store.SQLite) + ";"
			if update.table == "catalog_home_release" {
				// An attempt with no outcome is equivalent to no release row.
				sql += `DROP TRIGGER IF EXISTS viewer_policy_catalog_home_release_insert;
DROP TRIGGER IF EXISTS viewer_policy_catalog_home_release_delete;
CREATE TRIGGER viewer_policy_catalog_home_release_insert AFTER INSERT ON catalog_home_release
 WHEN NEW.checked<>0 OR NEW.earliest_ts<>0 OR NEW.fallback_admitted<>0 BEGIN ` + sqliteBump + ` END;
CREATE TRIGGER viewer_policy_catalog_home_release_delete AFTER DELETE ON catalog_home_release
 WHEN OLD.checked<>0 OR OLD.earliest_ts<>0 OR OLD.fallback_admitted<>0 BEGIN ` + sqliteBump + ` END;`
			}
			if _, err := tx.ExecContext(ctx, sql); err != nil {
				return err
			}
		}
		for _, table := range collectionCaches {
			if _, err := tx.ExecContext(ctx, sqliteDropAll(table)); err != nil {
				return err
			}
		}
		return nil
	})
	var pg strings.Builder
	for _, table := range collectionCaches {
		pg.WriteString(pgGuard(table, fmt.Sprintf(`DROP TRIGGER IF EXISTS viewer_policy_%[1]s_insert ON %[1]s;
DROP TRIGGER IF EXISTS viewer_policy_%[1]s_update ON %[1]s;
DROP TRIGGER IF EXISTS viewer_policy_%[1]s_delete ON %[1]s;`, table)))
	}
	for _, update := range effectiveUpdates {
		pg.WriteString(pgGuard(update.table, "DROP TRIGGER IF EXISTS viewer_policy_"+update.table+"_update ON "+update.table+"; "+effectiveUpdate(update.table, update.columns, store.Postgres)))
	}
	// The release table still has its original all-events PostgreSQL
	// trigger. Replace it with per-event triggers on the effective outcome.
	pg.WriteString(pgGuard("catalog_home_release", `DROP TRIGGER IF EXISTS viewer_policy_catalog_home_release ON catalog_home_release;
CREATE TRIGGER viewer_policy_catalog_home_release_insert AFTER INSERT ON catalog_home_release FOR EACH ROW
 WHEN (NEW.checked<>0 OR NEW.earliest_ts<>0 OR NEW.fallback_admitted<>0) EXECUTE FUNCTION viewer_policy_bump();
CREATE TRIGGER viewer_policy_catalog_home_release_delete AFTER DELETE ON catalog_home_release FOR EACH ROW
 WHEN (OLD.checked<>0 OR OLD.earliest_ts<>0 OR OLD.fallback_admitted<>0) EXECUTE FUNCTION viewer_policy_bump();`))
	store.RegisterDialect("viewer_policy", 9, store.Postgres, pg.String())
}
