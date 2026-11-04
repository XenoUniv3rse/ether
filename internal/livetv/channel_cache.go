package livetv

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"

	"github.com/braedonsaunders/ether/internal/store"
)

// The channel snapshot keeps every request path off a full table decode.
// A large IPTV lineup is tens of thousands of channels, and every Plex
// lineup, grid, hub and tune request needs the channel list. Requests share
// one decoded, sorted snapshot that is rebuilt only when livetv_channels
// changes. Triggers bump livetv_channel_epoch on every write to the table,
// from any writer and any cluster node, so one indexed read per request
// decides whether the snapshot is current.

const channelEpochSchema = `CREATE TABLE livetv_channel_epoch (id INTEGER PRIMARY KEY CHECK(id=1), generation BIGINT NOT NULL); INSERT INTO livetv_channel_epoch(id,generation) VALUES(1,1);`

func init() {
	sqlite := channelEpochSchema
	for _, event := range []string{"INSERT", "UPDATE", "DELETE"} {
		sqlite += fmt.Sprintf(` CREATE TRIGGER livetv_channel_epoch_%s AFTER %s ON livetv_channels BEGIN UPDATE livetv_channel_epoch SET generation=generation+1 WHERE id=1; END;`, strings.ToLower(event), event)
	}
	store.Register("livetv", 6, sqlite)
	store.RegisterDialect("livetv", 6, store.Postgres, channelEpochSchema+` CREATE FUNCTION livetv_channel_epoch_bump() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN UPDATE livetv_channel_epoch SET generation=generation+1 WHERE id=1; RETURN NULL; END $$;
CREATE TRIGGER livetv_channel_epoch AFTER INSERT OR UPDATE OR DELETE ON livetv_channels FOR EACH STATEMENT EXECUTE FUNCTION livetv_channel_epoch_bump();`)
	store.RegisterRestore("livetv_channel_epoch", restoreChannelEpoch, "livetv_channel_epoch")
}

// restoreChannelEpoch moves the epoch past both the restored and the live
// value, so no node keeps a snapshot of the pre-restore lineup.
func restoreChannelEpoch(ctx context.Context, tx *sql.Tx) (func(context.Context, *sql.Tx) error, error) {
	var before int64
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(generation),0) FROM livetv_channel_epoch`).Scan(&before); err != nil {
		return nil, err
	}
	return func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO livetv_channel_epoch(id,generation) VALUES(1,?)
			ON CONFLICT(id) DO UPDATE SET generation=CASE WHEN livetv_channel_epoch.generation>=excluded.generation THEN livetv_channel_epoch.generation+1 ELSE excluded.generation END`, before+1)
		return err
	}, nil
}

type channelSnapshot struct {
	mu         sync.Mutex
	generation int64
	list       []Channel // sorted by number; never mutated once published
	bySource   map[string][]int
	byID       map[string]int
}

// allChannels returns every channel in number order. The slice is the
// caller's; the channels' nested slices are shared and must not be written
// in place.
func (s *Service) allChannels(ctx context.Context) ([]Channel, error) {
	list, _, err := s.channelSnapshot(ctx)
	if err != nil {
		return nil, err
	}
	return slices.Clone(list), nil
}

func (s *Service) channelSnapshot(ctx context.Context) ([]Channel, *channelSnapshot, error) {
	var gen int64
	if err := s.o.DB.QueryRowContext(ctx, `SELECT generation FROM livetv_channel_epoch WHERE id=1`).Scan(&gen); err != nil {
		return nil, nil, err
	}
	c := &s.chanSnap
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.list != nil && c.generation == gen {
		return c.list, c, nil
	}
	// The epoch is read before the rows: a write in between leaves an older
	// generation on a newer list, which the next request rebuilds.
	list, err := channels(ctx, s.o.DB)
	if err != nil {
		return nil, nil, err
	}
	c.generation, c.list = gen, list
	c.byID = make(map[string]int, len(list))
	c.bySource = map[string][]int{}
	for i, ch := range list {
		c.byID[ch.ID] = i
		c.bySource[ch.SourceID] = append(c.bySource[ch.SourceID], i)
	}
	return list, c, nil
}

// numberKey orders channel numbers naturally: 2 < 10, 4.1 < 4.2 < 5.
type numberKey struct {
	major, minor int
	raw          string
}

func parseNumberKey(n string) numberKey {
	k := numberKey{raw: n}
	major, rest, _ := strings.Cut(strings.TrimSpace(n), ".")
	k.major = leadingInt(major)
	k.minor = leadingInt(rest)
	return k
}

func leadingInt(s string) int {
	end := 0
	for end < len(s) && s[end] >= '0' && s[end] <= '9' {
		end++
	}
	n, _ := strconv.Atoi(s[:end])
	return n
}

func (a numberKey) compare(b numberKey) int {
	if a.major != b.major {
		return a.major - b.major
	}
	if a.minor != b.minor {
		return a.minor - b.minor
	}
	return strings.Compare(a.raw, b.raw)
}

// sortChannels sorts by natural channel number, stable on the input order.
func sortChannels(cs []Channel) {
	keys := make([]numberKey, len(cs))
	idx := make([]int, len(cs))
	for i := range cs {
		keys[i] = parseNumberKey(cs[i].Number)
		idx[i] = i
	}
	slices.SortStableFunc(idx, func(a, b int) int { return keys[a].compare(keys[b]) })
	sorted := make([]Channel, len(cs))
	for i, j := range idx {
		sorted[i] = cs[j]
	}
	copy(cs, sorted)
}

func decodeChannelRows(rows *sql.Rows) ([]Channel, error) {
	defer rows.Close()
	out := []Channel{}
	for rows.Next() {
		var b []byte
		var c Channel
		if err := rows.Scan(&b); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(b, &c); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}
