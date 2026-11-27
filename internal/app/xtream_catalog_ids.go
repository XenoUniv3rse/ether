package app

import "github.com/braedonsaunders/ether/internal/store"

const xtreamCatalogRefs = `SELECT CAST(key AS TEXT) AS ref FROM catalog_titles WHERE type IN ('movie','show','season','episode')`

func init() {
	// Xtream clients require positive signed 32-bit identities for all kinds
	// of VOD item. Assign those IDs as catalogue records arrive so an export
	// can use the same durable mapping as detail and playback without writing.
	store.RegisterAfter("xtream_catalog_ids", "catalog", "frontend_compact_ids")
	backfill := xtreamIDSeed("item", xtreamCatalogRefs)
	store.Register("xtream_catalog_ids", 1, backfill+`
CREATE TRIGGER xtream_catalog_item_id AFTER INSERT ON catalog_titles
 WHEN NEW.type IN ('movie','show','season','episode') AND NOT EXISTS (
   SELECT 1 FROM frontend_compact_ids WHERE surface='xtream' AND kind='item' AND ref=CAST(NEW.key AS TEXT))
BEGIN
 SELECT RAISE(ABORT, 'Xtream item identity namespace exhausted') WHERE EXISTS (
   SELECT 1 FROM frontend_compact_id_counters WHERE surface='xtream' AND kind='item' AND last_id>=2147483647);
 UPDATE frontend_compact_id_counters SET last_id=last_id+1 WHERE surface='xtream' AND kind='item';
 INSERT INTO frontend_compact_ids(surface,kind,ref,id)
 SELECT 'xtream','item',CAST(NEW.key AS TEXT),last_id
 FROM frontend_compact_id_counters WHERE surface='xtream' AND kind='item';
END;
`)
	store.RegisterDialect("xtream_catalog_ids", 1, store.Postgres, backfill+`
CREATE FUNCTION xtream_catalog_item_id() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE allocated BIGINT;
BEGIN
 IF EXISTS (SELECT 1 FROM frontend_compact_ids WHERE surface='xtream' AND kind='item' AND ref=CAST(NEW.key AS TEXT)) THEN
   RETURN NULL;
 END IF;
 UPDATE frontend_compact_id_counters SET last_id=last_id
 WHERE surface='xtream' AND kind='item';
 IF NOT EXISTS (SELECT 1 FROM frontend_compact_ids WHERE surface='xtream' AND kind='item' AND ref=CAST(NEW.key AS TEXT)) THEN
   UPDATE frontend_compact_id_counters SET last_id=last_id+1
   WHERE surface='xtream' AND kind='item' AND last_id<2147483647 RETURNING last_id INTO allocated;
   IF NOT FOUND THEN RAISE EXCEPTION 'Xtream item identity namespace exhausted'; END IF;
   INSERT INTO frontend_compact_ids(surface,kind,ref,id) VALUES('xtream','item',CAST(NEW.key AS TEXT),allocated);
 END IF;
 RETURN NULL;
END $$;
CREATE TRIGGER xtream_catalog_item_id AFTER INSERT ON catalog_titles
 FOR EACH ROW WHEN (NEW.type IN ('movie','show','season','episode')) EXECUTE FUNCTION xtream_catalog_item_id();
`)
}
