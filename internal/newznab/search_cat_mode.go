package newznab

import (
	"context"
	"errors"
	"net/url"
	"slices"
	"time"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
)

// searchCatMode records how default movie/TV category filtering behaves on
// this indexer when the owner left MovieCategories/TVCategories unset.
// Standard Newznab servers expand parent categories (2000 -> 2000..2999,
// 5000 -> 5000..5999). Some custom implementations only match
// explicit subcategories advertised in caps, or require omitting cat=.
type searchCatMode int

const (
	searchCatDefault    searchCatMode = 0 // parent cat=2000 / cat=5000
	searchCatExpanded   searchCatMode = 1 // all caps subcategories for the family
	searchCatUnfiltered searchCatMode = 2 // omit cat= on default movie/TV searches
)

type searchCatState struct {
	identity          string
	mode              searchCatMode
	verified          bool
	checked           time.Time
	probeMisses       int
	probeBackoffUntil time.Time
}

const (
	searchCatProbeMaxMisses = 3
	searchCatProbeBackoff   = time.Hour
)

func (c *Client) loadSearchCatState(ctx context.Context, identity string) searchCatState {
	c.searchModeMu.Lock()
	defer c.searchModeMu.Unlock()
	if c.searchMode.identity == identity {
		return c.searchMode
	}
	st := searchCatState{identity: identity}
	if c.db != nil {
		var mode, verified int
		var checked int64
		err := c.db.QueryRowContext(ctx,
			`SELECT mode, verified, checked_at FROM newznab_search_modes WHERE indexer_id=? AND identity=?`,
			c.indexerID(), identity).Scan(&mode, &verified, &checked)
		if err == nil && mode >= int(searchCatDefault) && mode <= int(searchCatUnfiltered) {
			st.mode = searchCatMode(mode)
			st.verified = verified != 0
			st.checked = time.Unix(checked, 0)
		}
	}
	c.searchMode = st
	return st
}

func (c *Client) saveSearchCatState(ctx context.Context, st searchCatState) {
	c.searchModeMu.Lock()
	c.searchMode = st
	c.searchModeMu.Unlock()
	if c.db == nil {
		return
	}
	v := 0
	if st.verified {
		v = 1
	}
	if _, err := c.db.ExecContext(ctx,
		`INSERT INTO newznab_search_modes(indexer_id, identity, mode, verified, checked_at) VALUES(?,?,?,?,?)
		 ON CONFLICT(indexer_id) DO UPDATE SET identity=excluded.identity, mode=excluded.mode, verified=excluded.verified, checked_at=excluded.checked_at`,
		c.indexerID(), st.identity, int(st.mode), v, st.checked.Unix()); err != nil {
		c.log.Debug("newznab search mode save failed", "indexer", c.Name(), "err", err)
	}
}

func (c *Client) recordSearchCatProbeMiss(identity string) {
	c.searchModeMu.Lock()
	defer c.searchModeMu.Unlock()
	if c.searchMode.identity != identity || c.searchMode.verified {
		return
	}
	c.searchMode.probeMisses++
	if c.searchMode.probeMisses >= searchCatProbeMaxMisses {
		c.searchMode.probeMisses = 0
		c.searchMode.probeBackoffUntil = c.now().Add(searchCatProbeBackoff)
	}
}

func (c *Client) currentSearchCatMode() searchCatMode {
	identity := searchIdentity(c.Config())
	c.searchModeMu.Lock()
	defer c.searchModeMu.Unlock()
	if c.searchMode.identity == identity {
		return c.searchMode.mode
	}
	return searchCatDefault
}

func (c *Client) usesDefaultSearchCats(cfg config.Indexer, req core.SearchRequest) bool {
	if req.Category != "" {
		return false
	}
	if isTVRequest(req) {
		return len(cfg.TVCategories) == 0
	}
	return len(cfg.MovieCategories) == 0
}

// capsSubcategories returns the sorted, deduplicated category IDs in caps for
// the movie (2000..2999) or TV (5000..5999) family when caps advertises at
// least one subcategory beyond the bare parent.
func capsSubcategories(caps *Caps, isTV bool) []int {
	if caps == nil || len(caps.Categories) == 0 {
		return nil
	}
	lo, hi, parent := 2000, 3000, 2000
	if isTV {
		lo, hi, parent = 5000, 6000, 5000
	}
	var out []int
	hasSub := false
	for _, id := range caps.Categories {
		if id >= lo && id < hi {
			if id != parent {
				hasSub = true
			}
			if !slices.Contains(out, id) {
				out = append(out, id)
			}
		}
	}
	if !hasSub {
		return nil
	}
	slices.Sort(out)
	return out
}

func applyDefaultSearchCats(v url.Values, cfg config.Indexer, caps *Caps, isTV bool, mode searchCatMode) {
	var configured []int
	parent := 2000
	if isTV {
		configured = cfg.TVCategories
		parent = 5000
	} else {
		configured = cfg.MovieCategories
	}
	if len(configured) > 0 {
		v.Set("cat", joinCats(configured))
		return
	}
	switch mode {
	case searchCatUnfiltered:
		v.Del("cat")
	case searchCatExpanded:
		if sub := capsSubcategories(caps, isTV); len(sub) > 0 {
			v.Set("cat", joinCats(sub))
			return
		}
		v.Set("cat", joinCats([]int{parent}))
	default:
		v.Set("cat", joinCats([]int{parent}))
	}
}

// isVideoCompatibleCategory rejects obvious non-video standard Newznab
// categories (Console 1xxx, Audio 3xxx, PC 4xxx, XXX 6xxx, Books 7xxx) when
// an unfiltered search omits cat=, while keeping movies (2xxx), TV/anime
// (5xxx), unknown (0) and custom/other categories (>=8000).
func isVideoCompatibleCategory(cat int) bool {
	if cat == 0 || cat >= 8000 {
		return true
	}
	return (cat >= 2000 && cat < 3000) || (cat >= 5000 && cat < 6000)
}

func filterVideoReleases(rs []core.Release) []core.Release {
	if len(rs) == 0 {
		return rs
	}
	filtered := rs[:0]
	for _, r := range rs {
		if isVideoCompatibleCategory(r.Category) {
			filtered = append(filtered, r)
		}
	}
	return filtered
}

func cloneURLValues(v url.Values) url.Values {
	out := make(url.Values, len(v))
	for k, vs := range v {
		out[k] = append([]string(nil), vs...)
	}
	return out
}

// selectCatProbeTemplates picks up to two representative queries from attempts:
// the primary query, plus the first keyword (t=search&q=...) fallback when the
// primary query is ID-only. That covers both indexers that answer ID searches
// once cat= is fixed and indexers that match only keyword queries.
func selectCatProbeTemplates(attempts []url.Values) []url.Values {
	if len(attempts) == 0 {
		return nil
	}
	out := []url.Values{attempts[0]}
	if attempts[0].Get("q") == "" {
		for _, a := range attempts[1:] {
			if a.Get("t") == "search" && a.Get("q") != "" {
				out = append(out, a)
				break
			}
		}
	}
	return out
}

// probeBroaderSearchCats retries representative queries using caps-expanded
// subcategories first, then without cat=, when an unverified indexer returned
// empty on default parent categories (cat=2000 / cat=5000). The first mode
// that yields video-compatible releases is persisted for this indexer identity.
func (c *Client) probeBroaderSearchCats(ctx context.Context, req core.SearchRequest, caps *Caps, attempts []url.Values, id config.Identity, media config.IdentityMediaType, identity string) ([]core.Release, searchCatMode, error) {
	templates := selectCatProbeTemplates(attempts)
	if len(templates) == 0 {
		return nil, searchCatDefault, nil
	}
	isTV := isTVRequest(req)
	type candidate struct {
		mode searchCatMode
		cat  string // "" deletes cat
	}
	var cands []candidate
	if sub := capsSubcategories(caps, isTV); len(sub) > 0 {
		cands = append(cands, candidate{mode: searchCatExpanded, cat: joinCats(sub)})
	}
	cands = append(cands, candidate{mode: searchCatUnfiltered, cat: ""})

	for _, cand := range cands {
		probeCtx := ctx
		if cand.mode == searchCatUnfiltered {
			if emit, ok := ctx.Value(searchArrivalKey{}).(func([]core.Release)); ok {
				probeCtx = context.WithValue(ctx, searchArrivalKey{}, func(rs []core.Release) {
					if kept := filterVideoReleases(rs); len(kept) > 0 {
						emit(kept)
					}
				})
			}
		}
		for _, tmpl := range templates {
			if err := ctx.Err(); err != nil {
				return nil, searchCatDefault, err
			}
			q := cloneURLValues(tmpl)
			if cand.cat == "" {
				q.Del("cat")
			} else {
				q.Set("cat", cand.cat)
			}
			rs, err := c.doSearch(probeCtx, q, id, media)
			if err != nil {
				if errors.Is(err, core.ErrBudgetExhausted) || transientFailure(err) {
					return nil, searchCatDefault, err
				}
				continue
			}
			if cand.mode == searchCatUnfiltered {
				rs = filterVideoReleases(rs)
			}
			if len(rs) > 0 {
				c.saveSearchCatState(ctx, searchCatState{
					identity: identity,
					mode:     cand.mode,
					verified: true,
					checked:  c.now(),
				})
				return rs, cand.mode, nil
			}
		}
	}
	c.recordSearchCatProbeMiss(identity)
	return nil, searchCatDefault, nil
}
