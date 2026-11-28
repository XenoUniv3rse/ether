package favorites

import (
	"context"
	"database/sql"
	"encoding/base64"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/store"
)

// A title favorite records what it is (its role: "movie", "show", "track",
// "album", "artist", "book" …) when it is set, so a viewer's likes list by
// role from one index range: Liked Songs never reads a movie, and nothing
// reads the library.
func init() {
	store.Register("favorites", 2, `
ALTER TABLE favorites_items ADD COLUMN role TEXT NOT NULL DEFAULT '';
CREATE INDEX favorites_role ON favorites_items(viewer_id,kind,role,sort_order,added_at DESC,subject_id);
`)
}

// MaxPageLimit bounds one page of a role list.
const MaxPageLimit = 200

// PageQuery asks for one page of a viewer's favorites of one role, in the
// viewer's order (newest first unless they reordered their Favorites).
type PageQuery struct {
	Kind   Kind
	Role   string
	Cursor string
	Limit  int
}

// Page is one page of favorites. Total counts the viewer's favorites of the
// role (an index range bounded by the viewer's own likes), Next continues.
type Page struct {
	Entries []Entry
	Next    string
	Total   int
}

var ErrBadCursor = errors.New("favorites: bad cursor")

func roleOf(ctx context.Context, o Options, subject Subject) string {
	if subject.Kind != KindTitle || o.TitleRole == nil {
		return ""
	}
	key, err := core.ParseRatingKey(subject.ID)
	if err != nil {
		return ""
	}
	return o.TitleRole(ctx, key)
}

// BackfillRoles assigns roles to title favorites written before roles were
// recorded. It runs during startup, after stored media kinds are available.
func (s *Service) BackfillRoles(ctx context.Context) error {
	if s.o.TitleRole == nil {
		return nil
	}
	const batch = 200
	after := ""
	for {
		rows, err := s.o.DB.QueryContext(ctx, `SELECT DISTINCT subject_id FROM favorites_items
			WHERE kind='title' AND role='' AND subject_id>? ORDER BY subject_id LIMIT ?`, after, batch)
		if err != nil {
			return err
		}
		var ids []string
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return err
			}
			ids = append(ids, id)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		for _, id := range ids {
			role := roleOf(ctx, s.o, Subject{Kind: KindTitle, ID: id})
			if role == "" {
				continue
			}
			if _, err := s.o.DB.ExecContext(ctx, `UPDATE favorites_items SET role=? WHERE kind='title' AND subject_id=? AND role=''`, role, id); err != nil {
				return err
			}
		}
		if len(ids) < batch {
			return nil
		}
		after = ids[len(ids)-1]
	}
}

type cursor struct {
	order int64
	added int64
	id    string
}

func (c cursor) String() string {
	return base64.RawURLEncoding.EncodeToString([]byte(strconv.FormatInt(c.order, 10) + "|" + strconv.FormatInt(c.added, 10) + "|" + c.id))
}

func parseCursor(raw string) (cursor, bool, error) {
	if raw == "" {
		return cursor{}, false, nil
	}
	b, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		return cursor{}, false, ErrBadCursor
	}
	parts := strings.SplitN(string(b), "|", 3)
	if len(parts) != 3 {
		return cursor{}, false, ErrBadCursor
	}
	order, err1 := strconv.ParseInt(parts[0], 10, 64)
	added, err2 := strconv.ParseInt(parts[1], 10, 64)
	if err1 != nil || err2 != nil || parts[2] == "" {
		return cursor{}, false, ErrBadCursor
	}
	return cursor{order, added, parts[2]}, true, nil
}

// Page lists one page of the viewer's favorites of one role. Each page reads
// at most a few index ranges of the viewer's own favorites of that role and
// checks only the subjects it returns.
func (s *Service) Page(ctx context.Context, viewer *core.User, q PageQuery) (Page, error) {
	if q.Kind == "" {
		q.Kind = KindTitle
	}
	if viewer == nil || viewer.ID <= 0 || q.Kind != KindTitle || q.Role == "" {
		return Page{}, ErrForbidden
	}
	if q.Limit <= 0 {
		q.Limit = 50
	}
	q.Limit = min(q.Limit, MaxPageLimit)
	active, err := s.o.Active(ctx, viewer.ID)
	if err != nil || active == nil || active.ID != viewer.ID {
		return Page{}, ErrForbidden
	}
	at, has, err := parseCursor(q.Cursor)
	if err != nil {
		return Page{}, err
	}
	var out Page
	if err := s.o.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM favorites_items WHERE viewer_id=? AND kind=? AND role=?`, viewer.ID, string(q.Kind), q.Role).Scan(&out.Total); err != nil {
		return Page{}, err
	}
	// Hidden subjects (a library the viewer lost) are skipped; a few more
	// reads fill the page, never more than four.
	for round := 0; round < 4 && len(out.Entries) < q.Limit; round++ {
		want := q.Limit - len(out.Entries)
		var rows *sql.Rows
		if has {
			rows, err = s.o.DB.QueryContext(ctx, `SELECT subject_id,added_at,sort_order FROM favorites_items WHERE viewer_id=? AND kind=? AND role=?
				AND (sort_order>? OR (sort_order=? AND (added_at<? OR (added_at=? AND subject_id>?))))
				ORDER BY sort_order ASC,added_at DESC,subject_id ASC LIMIT ?`,
				viewer.ID, string(q.Kind), q.Role, at.order, at.order, at.added, at.added, at.id, want+1)
		} else {
			rows, err = s.o.DB.QueryContext(ctx, `SELECT subject_id,added_at,sort_order FROM favorites_items WHERE viewer_id=? AND kind=? AND role=?
				ORDER BY sort_order ASC,added_at DESC,subject_id ASC LIMIT ?`, viewer.ID, string(q.Kind), q.Role, want+1)
		}
		if err != nil {
			return Page{}, err
		}
		var read []cursor
		for rows.Next() {
			var c cursor
			if err := rows.Scan(&c.id, &c.added, &c.order); err != nil {
				rows.Close()
				return Page{}, err
			}
			read = append(read, c)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return Page{}, err
		}
		more := len(read) > want
		if more {
			read = read[:want]
		}
		ids := make([]string, len(read))
		for i, c := range read {
			ids[i] = c.id
		}
		s.preloadTitles(ctx, ids)
		for _, c := range read {
			subject := Subject{Kind: q.Kind, ID: c.id}
			if s.o.Allowed(ctx, active, subject) {
				out.Entries = append(out.Entries, Entry{Subject: subject, AddedAt: time.Unix(0, c.added).UTC()})
			}
		}
		if len(read) > 0 {
			at, has = read[len(read)-1], true
		}
		if !more {
			return out, nil
		}
		if len(out.Entries) >= q.Limit {
			out.Next = at.String()
			return out, nil
		}
	}
	if has {
		out.Next = at.String()
	}
	return out, nil
}
