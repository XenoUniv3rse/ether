package vela

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/braedonsaunders/ether/internal/frontend"
)

const defaultRowItemLimit = 20

// The collection's limit counts rows; only a single-row request uses it as
// an item limit. Keep bootstrap independent of the number of shelves requested.
func rowItemLimit(r *http.Request) int {
	n := defaultRowItemLimit
	if r.PathValue("id") != "" {
		if requested, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && requested > 0 {
			n = min(requested, 50)
		}
	}
	return n
}

type rowCursor struct {
	Row, Scope string
	Exclude    []string
	Offset     int
	Expires    int64
}

func (i *instance) cursorScope(r *http.Request, v *frontend.Viewer) string {
	return hashJSON([]any{v, requestDevice(r).ID})
}
func (i *instance) rowPage(r *http.Request, v *frontend.Viewer, id string, offset, n int) (frontend.Row, error) {
	views, ok := i.host.Experience.(frontend.RowViews)
	if !ok {
		return frontend.Row{}, frontend.ErrNotAvailable
	}
	page, e := views.ResolveRowView(r.Context(), frontend.RowViewPageRequest{Viewer: v, Device: requestDevice(r), Protocol: "vela", ID: id, Offset: offset, Limit: n})
	if e != nil {
		return frontend.Row{}, e
	}
	noteBootstrapDeadline(r, page.ExpiresAt)
	return rowFromPage(id, page), nil
}

func rowFromPage(id string, page frontend.RowViewPage) frontend.Row {
	kind := ""
	if page.Truncated {
		kind = "truncated"
	}
	return frontend.Row{RankedTop10: page.RankedTop10, Kind: kind, ID: id, Entries: page.Entries, Handle: page.ImpressionHandle, More: page.More}
}
func (i *instance) rowToken(r *http.Request, v *frontend.Viewer, id string, offset int, exclude []string) string {
	return i.sealRowCursor(rowCursor{Row: id, Scope: i.cursorScope(r, v), Offset: offset, Exclude: exclude, Expires: cursorExpiry(time.Now())})
}

func (i *instance) sealRowCursor(c rowCursor) string {
	b, _ := json.Marshal(c)
	nonce, e := i.cursorNonce("vela-row", b)
	if e != nil {
		return ""
	}
	return base64.RawURLEncoding.EncodeToString(i.seal.Seal(nonce, nonce, b, []byte("vela-row")))
}
func (i *instance) readRowToken(r *http.Request, v *frontend.Viewer, token string) (rowCursor, error) {
	var c rowCursor
	if len(token) > 4096 {
		return c, frontend.ErrRowViewStale
	}
	b, e := base64.RawURLEncoding.DecodeString(token)
	if e != nil || len(b) < i.seal.NonceSize() {
		return c, frontend.ErrRowViewStale
	}
	b, e = i.seal.Open(nil, b[:i.seal.NonceSize()], b[i.seal.NonceSize():], []byte("vela-row"))
	if e != nil || json.Unmarshal(b, &c) != nil || c.Expires < time.Now().Unix() || c.Scope != i.cursorScope(r, v) || c.Offset < 0 || c.Offset > 10000 || len(c.Exclude) > 6 {
		return c, frontend.ErrRowViewStale
	}
	for _, id := range c.Exclude {
		if len(id) > 256 {
			return rowCursor{}, frontend.ErrRowViewStale
		}
	}
	return c, nil
}

// backfillShelves consumes only the missing slots from the same immutable
// visit. Each round batches independent shelves so they can share one
// authorization check and receipt transaction.
func (i *instance) backfillShelves(r *http.Request, v *frontend.Viewer, token string, shelves []object, wanted int) []error {
	errs := make([]error, len(shelves))
	done := make([]bool, len(shelves))
	for attempt := 0; attempt < 6; attempt++ {
		var indexes []int
		var cursors []rowCursor
		var reqs []frontend.RowViewPageRequest
		for n, shelf := range shelves {
			if done[n] || errs[n] != nil || shelf["continuation"] != "more" {
				continue
			}
			missing := wanted - len(shelf["items"].([]object))
			if missing <= 0 {
				continue
			}
			next, _ := shelf["nextCursor"].(string)
			if next == "" {
				continue
			}
			cursor, err := i.readRowToken(r, v, next)
			if err != nil {
				errs[n] = err
				continue
			}
			if len(cursor.Exclude) == 0 {
				continue
			}
			indexes = append(indexes, n)
			cursors = append(cursors, cursor)
			reqs = append(reqs, frontend.RowViewPageRequest{Viewer: v, Device: requestDevice(r), Protocol: "vela", ID: cursor.Row, Offset: cursor.Offset, Limit: missing})
		}
		if len(reqs) == 0 {
			break
		}
		var pages []frontend.RowViewPage
		if batch, ok := i.host.Experience.(frontend.RowViewPages); ok && len(reqs) > 1 {
			var err error
			pages, err = batch.ResolveRowViews(r.Context(), reqs)
			if retryBootstrap(err) || r.Context().Err() != nil {
				errs[indexes[0]] = errors.Join(err, r.Context().Err())
				return errs
			}
			if err != nil || len(pages) != len(reqs) {
				pages = nil
			}
		}
		for k, n := range indexes {
			q, cursor, shelf := reqs[k], cursors[k], shelves[n]
			var row frontend.Row
			if pages != nil {
				noteBootstrapDeadline(r, pages[k].ExpiresAt)
				row = rowFromPage(q.ID, pages[k])
			} else {
				var err error
				row, err = i.rowPage(r, v, q.ID, q.Offset, q.Limit)
				if err != nil {
					errs[n] = err
					continue
				}
			}
			page, err := i.rowWire(r, v, token, row, cursor.Offset, cursor.Exclude, true)
			if err != nil {
				errs[n] = err
				continue
			}
			shelf["items"] = append(shelf["items"].([]object), page["items"].([]object)...)
			shelf["continuation"] = page["continuation"]
			delete(shelf, "nextCursor")
			if next, ok := page["nextCursor"]; ok {
				shelf["nextCursor"] = next
			}
			done[n] = len(row.Entries) == 0
		}
	}
	return errs
}

func (i *instance) backfillShelf(r *http.Request, v *frontend.Viewer, token string, shelf object, wanted int) error {
	return i.backfillShelves(r, v, token, []object{shelf}, wanted)[0]
}
