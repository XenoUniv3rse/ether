package newznab

import (
	"encoding/xml"
	"strconv"
	"strings"
)

// Caps is a parsed Newznab capabilities document.
type Caps struct {
	ServerTitle     string   `json:"serverTitle,omitempty"`
	ServerType      string   `json:"serverType,omitempty"`
	LimitDefault    int      `json:"limitDefault,omitempty"`
	LimitMax        int      `json:"limitMax,omitempty"`
	SearchAvail     bool     `json:"searchAvailable"`
	SearchParams    []string `json:"searchParams,omitempty"`
	MovieAvail      bool     `json:"movieSearchAvailable"`
	MovieParams     []string `json:"movieParams,omitempty"`
	TVAvail         bool     `json:"tvSearchAvailable"`
	TVParams        []string `json:"tvParams,omitempty"`
	Categories      []int    `json:"categories,omitempty"`
	AnimeCategories []int    `json:"animeCategories,omitempty"`
	NewznabPath     bool     `json:"newznabPath,omitempty"`
}

type capsXML struct {
	Server struct {
		Title string `xml:"title,attr"`
		Type  string `xml:"type,attr"`
	} `xml:"server"`
	Limits struct {
		Max     string `xml:"max,attr"`
		Default string `xml:"default,attr"`
	} `xml:"limits"`
	Searching struct {
		Search struct {
			Available string `xml:"available,attr"`
			Params    string `xml:"supportedParams,attr"`
		} `xml:"search"`
		Movie struct {
			Available string `xml:"available,attr"`
			Params    string `xml:"supportedParams,attr"`
		} `xml:"movie-search"`
		TV struct {
			Available string `xml:"available,attr"`
			Params    string `xml:"supportedParams,attr"`
		} `xml:"tv-search"`
	} `xml:"searching"`
	Categories struct {
		Category []struct {
			ID   string `xml:"id,attr"`
			Name string `xml:"name,attr"`
			Sub  []struct {
				ID   string `xml:"id,attr"`
				Name string `xml:"name,attr"`
			} `xml:"subcat"`
		} `xml:"category"`
	} `xml:"categories"`
}

func parseCaps(body []byte) (*Caps, error) {
	var x capsXML
	if err := xml.Unmarshal(body, &x); err != nil {
		return nil, err
	}
	yes := func(s string) bool { return strings.EqualFold(s, "yes") || s == "1" }
	split := func(s string) []string {
		if s == "" {
			return nil
		}
		parts := strings.Split(s, ",")
		for i := range parts {
			parts[i] = strings.TrimSpace(strings.ToLower(parts[i]))
		}
		return parts
	}
	c := &Caps{
		ServerTitle: x.Server.Title, ServerType: x.Server.Type,
		SearchAvail: yes(x.Searching.Search.Available), SearchParams: split(x.Searching.Search.Params),
		MovieAvail: yes(x.Searching.Movie.Available), MovieParams: split(x.Searching.Movie.Params),
		TVAvail: yes(x.Searching.TV.Available), TVParams: split(x.Searching.TV.Params),
	}
	c.LimitMax, _ = strconv.Atoi(x.Limits.Max)
	c.LimitDefault, _ = strconv.Atoi(x.Limits.Default)
	addCategory := func(id, name string, parentAnime bool) {
		if n, err := strconv.Atoi(id); err == nil {
			c.Categories = append(c.Categories, n)
			if n == animeCategory || parentAnime || isAnimeCategoryName(name) {
				c.AnimeCategories = append(c.AnimeCategories, n)
			}
		}
	}
	for _, cat := range x.Categories.Category {
		addCategory(cat.ID, cat.Name, false)
		for _, sub := range cat.Sub {
			addCategory(sub.ID, sub.Name, isAnimeCategoryName(cat.Name))
		}
	}
	return c, nil
}

func hasParam(params []string, p string) bool {
	if len(params) == 0 {
		return true // unknown; assume supported
	}
	for _, v := range params {
		if v == p {
			return true
		}
	}
	return false
}
