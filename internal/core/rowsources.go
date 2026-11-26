package core

import (
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"
)

// RowSourceInfo describes one row source the Recommendations designer
// offers under "What fills this row?": built-in rows, collections, plugin
// lists and shelves. A page row names it by Kind (Row.Source.Kind).
type RowSourceInfo struct {
	Kind        string `json:"kind"`
	Title       string `json:"title"`
	Description string `json:"description"`
	// Group is the picker section: keep, trending, new, foryou, top,
	// browse, collections, plugins or live.
	Group string `json:"group"`
	// Personal sources show each viewer different titles; the others are
	// the same list for everyone.
	Personal bool `json:"personal"`
	// Needs are library capabilities the source draws on (all required);
	// see LibraryCapabilities.
	Needs []string `json:"needs,omitempty"`
	// HomeOnly sources fit Home only.
	HomeOnly bool `json:"homeOnly,omitempty"`
	// Split sources can be one row per library.
	Split  bool       `json:"split,omitempty"`
	Params []RowParam `json:"params,omitempty"`
	// Purpose tells apps what the row is for (continue, nextUp, live, …).
	Purpose RowPurpose `json:"purpose,omitempty"`
	// Look holds the source's default look.
	Look RowLook `json:"look"`
	// Plugin names the plugin that provides it (display name), for
	// "From plugins › <name>".
	Plugin string `json:"plugin,omitempty"`
	// Apps lists the apps that can draw it (nil: every app); plugin
	// surfaces are ["vela"].
	Apps []string `json:"apps,omitempty"`
	// Library pins a plugin shelf to its library.
	Library LibraryID `json:"library,omitempty"`
}

const MaxCollectionCards = 50

// Row parameter types.
const (
	ParamChoice         = "choice" // one of Choices
	ParamBool           = "bool"
	ParamCount          = "count"      // an integer Min..Max
	ParamFacetValue     = "facetValue" // a value of the row library's Facet ("" = Ether picks)
	ParamCollectionList = "collectionList"
	ParamText           = "text"
	ParamRef            = "ref" // an id the console picks from a list (a collection, a shelf)
)

// RowParam is one option of a source, drawn as a plain control.
type RowParam struct {
	Key     string        `json:"key"`
	Label   string        `json:"label"`
	Type    string        `json:"type"`
	Choices []ParamOption `json:"choices,omitempty"`
	Facet   string        `json:"facet,omitempty"`
	Default string        `json:"default,omitempty"`
	Min     int           `json:"min,omitempty"`
	Max     int           `json:"max,omitempty"`
	// Required parameters must be set (a collection row's collection).
	Required bool `json:"required,omitempty"`
}

// ParamOption is one choice of a choice parameter.
type ParamOption struct {
	Value string `json:"value"`
	Label string `json:"label"`
}

// ValidateParams checks params against the declared parameters.
func (s RowSourceInfo) ValidateParams(params map[string]string) error {
	declared := map[string]RowParam{}
	for _, p := range s.Params {
		declared[p.Key] = p
	}
	for k, v := range params {
		p, ok := declared[k]
		if !ok {
			return fmt.Errorf("%s has no option %q", s.Title, k)
		}
		if v == "" {
			continue
		}
		switch p.Type {
		case ParamChoice:
			found := false
			for _, c := range p.Choices {
				found = found || c.Value == v
			}
			if !found {
				return fmt.Errorf("%s: %q is not a choice for %s", s.Title, v, p.Label)
			}
		case ParamBool:
			if v != "true" && v != "false" {
				return fmt.Errorf("%s: %s must be true or false", s.Title, p.Label)
			}
		case ParamCount:
			n, err := strconv.Atoi(v)
			if err != nil || n < p.Min || p.Max > 0 && n > p.Max {
				return fmt.Errorf("%s: %s must be %d to %d", s.Title, p.Label, p.Min, p.Max)
			}
		case ParamCollectionList:
			ids := strings.Split(v, ",")
			if len(ids) > p.Max {
				return fmt.Errorf("%s accepts up to %d collections", s.Title, p.Max)
			}
			for _, id := range ids {
				key, err := ParseRatingKey(id)
				if err != nil || key.Kind() != KindSmart || key.IsEphemeral() {
					return fmt.Errorf("%s has an invalid collection", s.Title)
				}
			}
		case ParamText:
			if utf8.RuneCountInString(v) > p.Max || strings.ContainsAny(v, "\n\r") {
				return fmt.Errorf("%s is not valid", p.Label)
			}
		case ParamFacetValue, ParamRef:
			if len(v) > 200 || strings.ContainsAny(v, "\n\r") {
				return fmt.Errorf("%s: %s is not valid", s.Title, p.Label)
			}
		}
	}
	for _, p := range s.Params {
		if p.Required && params[p.Key] == "" {
			return fmt.Errorf("%s needs %s", s.Title, strings.ToLower(p.Label))
		}
	}
	return nil
}

// Library capabilities (RowSourceInfo.Needs). A library advertises what it
// has; the designer offers a source on a page only when the page's library
// (on Home: any library the viewer can see) has every capability it needs.
const (
	CapCharts     = "charts"     // TMDB-backed titles: trending, popular, top rated, upcoming
	CapTaste      = "taste"      // the taste model's films and series rows
	CapEpisodes   = "episodes"   // series with episodes: Up Next, airing, TV lenses
	CapMovies     = "movies"     // films: film-only lenses
	CapProgress   = "progress"   // resumable items: Continue
	CapAdded      = "added"      // Recently added
	CapReleased   = "released"   // New (by release date)
	CapShelves    = "shelves"    // a plugin library's declared shelves
	CapPopularity = "popularity" // Popular on this server
	CapLive       = "live"       // Live TV
	CapCatalogue  = "catalogue"  // owner collections (catalogue titles)
)

// CapRole is the capability of a library with a level of the given role
// ("role:playlist"); CapFacet of one with the given browse facet.
func CapRole(role string) string   { return "role:" + role }
func CapFacet(facet string) string { return "facet:" + facet }

// Level roles the row sources draw on.
const RolePlaylist = "playlist"
