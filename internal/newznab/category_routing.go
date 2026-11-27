package newznab

import (
	"net/url"
	"slices"
	"strings"
	"unicode"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
)

const animeCategory = 5070

func namedAnimeSource(cfg config.Indexer) bool {
	const name = "animetosho"
	normalized := strings.NewReplacer(" ", "", "-", "", "_", "", ".", "").Replace(strings.ToLower(cfg.Name))
	u, _ := url.Parse(cfg.URL)
	if u != nil {
		host := strings.ToLower(u.Hostname())
		if host == "animetosho.org" || strings.HasSuffix(host, ".animetosho.org") {
			return true
		}
	}
	return strings.HasPrefix(strings.TrimPrefix(normalized, "prowlarr/"), name)
}

func isAnimeCategoryName(name string) bool {
	return slices.Contains(strings.FieldsFunc(strings.ToLower(name), func(r rune) bool {
		return !unicode.IsLetter(r)
	}), "anime")
}

func animeCategories(caps *Caps) []int {
	if caps == nil {
		return nil
	}
	if len(caps.AnimeCategories) > 0 {
		return caps.AnimeCategories
	}
	if slices.Contains(caps.Categories, animeCategory) {
		return []int{animeCategory}
	}
	return nil
}

func animeOnly(cfg config.Indexer, caps *Caps) bool {
	if namedAnimeSource(cfg) {
		return true
	}
	cats := animeCategories(caps)
	if len(cats) == 0 {
		return false
	}
	for _, cat := range caps.Categories {
		if slices.Contains(cats, cat) {
			continue
		}
		if cat%1000 != 0 || !slices.ContainsFunc(cats, func(c int) bool { return c/1000 == cat/1000 }) {
			return false
		}
	}
	return true
}

// supportedCategories intersects category families with advertised capabilities.
// A bare parent with no listed children advertises the whole family.
func supportedCategories(wanted []int, caps *Caps) []int {
	if caps == nil || len(caps.Categories) == 0 {
		return wanted
	}
	var out []int
	for _, cat := range wanted {
		if slices.Contains(caps.Categories, cat) {
			out = append(out, cat)
			continue
		}
		family := cat / 1000 * 1000
		var children []int
		for _, supported := range caps.Categories {
			if supported != family && supported/1000*1000 == family {
				children = append(children, supported)
			}
		}
		if cat == family {
			out = append(out, children...)
		} else if len(children) == 0 && slices.Contains(caps.Categories, family) {
			out = append(out, cat)
		}
	}
	slices.Sort(out)
	return slices.Compact(out)
}

func isAnimeSearch(req core.SearchRequest) bool {
	cats, _ := core.SearchCategories(req.Category)
	return req.Anime || (len(cats) == 1 && cats[0] == animeCategory && strings.TrimSpace(req.Title) != "")
}

func routedCategories(cfg config.Indexer, caps *Caps, req core.SearchRequest) []int {
	if animeOnly(cfg, caps) {
		if isAnimeSearch(req) {
			if cats := animeCategories(caps); len(cats) > 0 {
				return cats
			}
			// The source's specialization is known even without caps.
			return []int{animeCategory}
		}
		return nil
	}
	wanted, _ := core.SearchCategories(req.Category)
	if req.Category == "" {
		wanted = cfg.MovieCategories
		if len(wanted) == 0 {
			wanted = []int{2000}
		}
		if isTVRequest(req) {
			wanted = cfg.TVCategories
			if len(wanted) == 0 {
				wanted = []int{5000}
			}
		}
		if req.Anime && !isTVRequest(req) && len(cfg.MovieCategories) == 0 {
			wanted = []int{2000, animeCategory}
		}
	}
	return supportedCategories(wanted, caps)
}

func hasTitleQuery(q url.Values) bool {
	for _, k := range []string{"q", "imdbid", "tmdbid", "tvdbid", "tvmazeid", "rid", "traktid"} {
		if strings.TrimSpace(q.Get(k)) != "" {
			return true
		}
	}
	return false
}

func routeSearchAttempts(attempts []url.Values, cats []int, defaults bool, mode searchCatMode) {
	if defaults && mode != searchCatDefault && len(cats) == 1 && (cats[0] == 2000 || cats[0] == 5000) {
		return // the verified compatibility mode already shaped the request
	}
	value := joinCats(cats)
	for _, attempt := range attempts {
		attempt.Set("cat", value)
	}
}
