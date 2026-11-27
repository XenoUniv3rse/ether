package app

import (
	"fmt"
	"strings"

	"github.com/braedonsaunders/ether/internal/store"
)

func init() {
	store.RegisterAfter("xtream_live_ids", "livetv", "frontend_compact_ids")
	const sqliteGroup = `trim(COALESCE(json_extract(%s.data,'$.group'),''))`
	const pgGroup = `btrim(COALESCE(CAST(%s.data AS jsonb)->>'group',''))`
	const sqliteOwnerGroups = `SELECT 'owner'||char(10)||json_extract(value,'$.id') AS ref FROM %sjson_each(%s.data)`
	const pgOwnerGroups = `SELECT 'owner'||chr(10)||(g->>'id') AS ref FROM %sjsonb_array_elements(CAST(%s.data AS jsonb)) AS g`
	seed := func(groupExpr, ownerGroups string) string {
		return xtreamIDSeed("channel", `SELECT id AS ref FROM livetv_channels`) +
			xtreamIDSeed("live-group", `SELECT `+fmt.Sprintf(groupExpr, "c")+` AS ref FROM livetv_channels c UNION `+
				fmt.Sprintf(ownerGroups, "livetv_owner_groups o, ", "o"))
	}
	sqlite := seed(sqliteGroup, sqliteOwnerGroups)
	postgres := seed(pgGroup, pgOwnerGroups)
	sqliteChannel := xtreamIDsFromRefs("channel", `SELECT NEW.id AS ref`)
	sqliteGroupIDs := xtreamIDsFromRefs("live-group", `SELECT `+fmt.Sprintf(sqliteGroup, "NEW")+` AS ref`)
	sqliteOwnerIDs := xtreamIDsFromRefs("live-group", fmt.Sprintf(sqliteOwnerGroups, "", "NEW"))
	for _, event := range []string{"INSERT", "UPDATE"} {
		var changed string
		if event == "UPDATE" {
			changed = " WHEN NEW.id<>OLD.id OR " + fmt.Sprintf(sqliteGroup, "NEW") + "<>" + fmt.Sprintf(sqliteGroup, "OLD")
		}
		sqlite += "CREATE TRIGGER xtream_channel_ids_" + strings.ToLower(event) + " AFTER " + event + " ON livetv_channels" + changed + " BEGIN\n" +
			sqliteChannel + sqliteGroupIDs + "END;\n"
		sqlite += "CREATE TRIGGER xtream_owner_group_ids_" + strings.ToLower(event) + " AFTER " + event + " ON livetv_owner_groups BEGIN\n" +
			sqliteOwnerIDs + "END;\n"
	}
	postgres += `
CREATE FUNCTION xtream_allocate_live_id(namespace TEXT, reference TEXT) RETURNS void LANGUAGE plpgsql AS $$
DECLARE allocated BIGINT;
BEGIN
 IF reference IS NULL OR reference='' OR EXISTS (
   SELECT 1 FROM frontend_compact_ids WHERE surface='xtream' AND kind=namespace AND ref=reference
 ) THEN RETURN; END IF;
 UPDATE frontend_compact_id_counters SET last_id=last_id
 WHERE surface='xtream' AND kind=namespace;
 IF EXISTS (SELECT 1 FROM frontend_compact_ids WHERE surface='xtream' AND kind=namespace AND ref=reference) THEN RETURN; END IF;
 UPDATE frontend_compact_id_counters SET last_id=last_id+1
 WHERE surface='xtream' AND kind=namespace AND last_id<2147483645 RETURNING last_id INTO allocated;
 IF NOT FOUND THEN RAISE EXCEPTION 'Xtream live identity namespace exhausted'; END IF;
 INSERT INTO frontend_compact_ids(surface,kind,ref,id) VALUES('xtream',namespace,reference,allocated);
END $$;
CREATE FUNCTION xtream_channel_ids() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF TG_OP='UPDATE' THEN
   IF NEW.id=OLD.id AND ` + fmt.Sprintf(pgGroup, "NEW") + `=` + fmt.Sprintf(pgGroup, "OLD") + ` THEN RETURN NULL; END IF;
 END IF;
 PERFORM xtream_allocate_live_id('channel', NEW.id);
 PERFORM xtream_allocate_live_id('live-group', ` + fmt.Sprintf(pgGroup, "NEW") + `);
 RETURN NULL;
END $$;
CREATE FUNCTION xtream_owner_group_ids() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 PERFORM xtream_allocate_live_id('live-group', refs.ref) FROM (` + fmt.Sprintf(pgOwnerGroups, "", "NEW") + `) refs;
 RETURN NULL;
END $$;
CREATE TRIGGER xtream_channel_ids AFTER INSERT OR UPDATE OF data ON livetv_channels
 FOR EACH ROW EXECUTE FUNCTION xtream_channel_ids();
CREATE TRIGGER xtream_owner_group_ids AFTER INSERT OR UPDATE OF data ON livetv_owner_groups
 FOR EACH ROW EXECUTE FUNCTION xtream_owner_group_ids();
`
	store.Register("xtream_live_ids", 1, sqlite)
	store.RegisterDialect("xtream_live_ids", 1, store.Postgres, postgres)
}

func xtreamIDSeed(kind, refs string) string {
	return fmt.Sprintf(`
INSERT INTO frontend_compact_id_counters(surface,kind,last_id) VALUES('xtream','%[1]s',0)
 ON CONFLICT(surface,kind) DO NOTHING;
UPDATE frontend_compact_id_counters SET last_id=last_id WHERE surface='xtream' AND kind='%[1]s';
`, kind) + xtreamIDsFromRefs(kind, refs)
}

func xtreamIDsFromRefs(kind, refs string) string {
	return fmt.Sprintf(`
INSERT INTO frontend_compact_ids(surface,kind,ref,id)
 SELECT 'xtream','%[1]s',r.ref,c.last_id+ROW_NUMBER() OVER (ORDER BY r.ref)
 FROM (SELECT DISTINCT ref FROM (%[2]s) AS candidates WHERE ref<>'') r
 CROSS JOIN frontend_compact_id_counters c
 WHERE c.surface='xtream' AND c.kind='%[1]s'
 AND NOT EXISTS (SELECT 1 FROM frontend_compact_ids i WHERE i.surface='xtream' AND i.kind='%[1]s' AND i.ref=r.ref);
UPDATE frontend_compact_id_counters SET last_id=COALESCE(
 (SELECT MAX(id) FROM frontend_compact_ids WHERE surface='xtream' AND kind='%[1]s'),0)
 WHERE surface='xtream' AND kind='%[1]s' AND last_id<COALESCE(
 (SELECT MAX(id) FROM frontend_compact_ids WHERE surface='xtream' AND kind='%[1]s'),0);
`, kind, refs)
}
