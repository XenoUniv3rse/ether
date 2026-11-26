package collections

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/braedonsaunders/ether/internal/core"
)

// LegacyPlacement is where a collection was published before
// Recommendations pages (its old "targets"), for the one-time rows-pages-v1
// migration (internal/app). Collections are content only now.
type LegacyPlacement struct {
	ID         string // public id
	MediaType  string // movie | show | mixed
	Enabled    bool
	HomeRow    bool
	SectionRow bool
	// Viewers is the old audience: nil for everyone.
	Viewers []int64
	// When and Devices are the old collection schedule.
	When    *core.Schedule
	Devices []string
}

// legacyTargets is the stored shape of the old targets.
type legacyTargets struct {
	CollectionsTab bool            `json:"collectionsTab"`
	HomeRow        bool            `json:"homeRow"`
	SectionRow     bool            `json:"sectionRow"`
	Users          json.RawMessage `json:"users"`
	Schedule       *struct {
		Days      []int    `json:"days"`
		FromHour  *int     `json:"fromHour"`
		ToHour    *int     `json:"toHour"`
		Devices   []string `json:"devices"`
		Dayparts  []string `json:"dayparts"`
		Months    []int    `json:"months"`
		StartDate string   `json:"startDate"`
		EndDate   string   `json:"endDate"`
	} `json:"schedule"`
}

// TakeLegacyPlacements reads every collection's old targets, rewrites the
// stored spec without them (browseInApps = the old Collections tab switch)
// and returns them. It is idempotent: a spec already rewritten has none.
func (s *Service) TakeLegacyPlacements(ctx context.Context) ([]LegacyPlacement, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, slug, spec_json, enabled FROM collections_specs`)
	if err != nil {
		return nil, err
	}
	type stored struct {
		id      int64
		slug    string
		raw     string
		enabled bool
	}
	var all []stored
	for rows.Next() {
		var st stored
		var enabled int
		if err := rows.Scan(&st.id, &st.slug, &st.raw, &enabled); err != nil {
			rows.Close()
			return nil, err
		}
		st.enabled = enabled != 0
		all = append(all, st)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	var out []LegacyPlacement
	for _, st := range all {
		var doc map[string]json.RawMessage
		if err := json.Unmarshal([]byte(st.raw), &doc); err != nil {
			continue
		}
		rawTargets, ok := doc["targets"]
		if !ok {
			continue
		}
		var t legacyTargets
		_ = json.Unmarshal(rawTargets, &t)
		var mediaType string
		_ = json.Unmarshal(doc["mediaType"], &mediaType)
		pid := st.slug
		if pid == "" {
			pid = fmt.Sprint(st.id)
		}
		p := LegacyPlacement{ID: pid, MediaType: mediaType, Enabled: st.enabled, HomeRow: t.HomeRow, SectionRow: t.SectionRow}
		var users any
		if len(t.Users) > 0 && json.Unmarshal(t.Users, &users) == nil {
			if list, ok := users.([]any); ok {
				p.Viewers = []int64{}
				for _, v := range list {
					switch n := v.(type) {
					case float64:
						p.Viewers = append(p.Viewers, int64(n))
					case string:
						var id int64
						if _, err := fmt.Sscan(n, &id); err == nil {
							p.Viewers = append(p.Viewers, id)
						}
					}
				}
			}
		}
		if sc := t.Schedule; sc != nil {
			when := &core.Schedule{Days: sc.Days, FromHour: sc.FromHour, ToHour: sc.ToHour, Dayparts: sc.Dayparts, Months: sc.Months}
			if strings.TrimSpace(sc.StartDate) != "" || strings.TrimSpace(sc.EndDate) != "" {
				when.Dates = []core.DateRange{{From: strings.TrimSpace(sc.StartDate), To: strings.TrimSpace(sc.EndDate)}}
			}
			if !when.IsZero() {
				p.When = when
			}
			p.Devices = sc.Devices
		}
		out = append(out, p)
		delete(doc, "targets")
		b, _ := json.Marshal(t.CollectionsTab)
		doc["browseInApps"] = b
		js, err := json.Marshal(doc)
		if err != nil {
			return nil, err
		}
		var spec core.CollectionSpec
		if err := json.Unmarshal(js, &spec); err != nil {
			return nil, err
		}
		Normalize(&spec)
		if _, err := s.db.ExecContext(ctx, `UPDATE collections_specs SET spec_json=?,browse_mode=?,browse_type=? WHERE id=?`, string(js), browseMode(spec), spec.MediaType, st.id); err != nil {
			return nil, err
		}
	}
	s.forgetRowMemo(ctx)
	return out, nil
}

// DeleteRetired removes built-in collections that became built-in row
// sources (the Ether-managed discovery collections), with their members.
func (s *Service) DeleteRetired(ctx context.Context, slugs []string) error {
	if len(slugs) == 0 {
		return nil
	}
	args := make([]any, len(slugs))
	marks := make([]string, len(slugs))
	for i, id := range slugs {
		args[i], marks[i] = id, "?"
	}
	in := strings.Join(marks, ",")
	if _, err := s.db.ExecContext(ctx, `DELETE FROM collections_members WHERE spec_id IN (SELECT id FROM collections_specs WHERE builtin=1 AND slug IN (`+in+`))`, args...); err != nil {
		return err
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM collections_specs WHERE builtin=1 AND slug IN (`+in+`)`, args...); err != nil {
		return err
	}
	s.forgetRowMemo(ctx)
	return nil
}
