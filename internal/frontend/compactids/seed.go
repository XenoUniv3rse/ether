package compactids

import "fmt"

// SeedXtreamSQL seeds a namespace from a trusted SQL query projecting ref.
func SeedXtreamSQL(kind, refs string) string {
	return fmt.Sprintf(`
INSERT INTO frontend_compact_id_counters(surface,kind,last_id) VALUES('xtream','%[1]s',0)
 ON CONFLICT(surface,kind) DO NOTHING;
UPDATE frontend_compact_id_counters SET last_id=last_id WHERE surface='xtream' AND kind='%[1]s';
`, kind) + AllocateXtreamSQL(kind, refs)
}

// AllocateXtreamSQL allocates missing IDs from a trusted SQL query projecting ref.
// The caller holds the write transaction and serializes namespace allocation.
func AllocateXtreamSQL(kind, refs string) string {
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
