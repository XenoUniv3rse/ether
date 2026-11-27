package agent

import (
	"net/url"
	"regexp"
	"strings"
)

var ownerWebURL = regexp.MustCompile(`https?://[^\s<>"\x60]+`)

type turnWebLinks map[string]bool

func newTurnWebLinks(prompt string) turnWebLinks {
	links := make(turnWebLinks)
	for _, raw := range ownerWebURL.FindAllString(prompt, -1) {
		links.add(raw)
		links.add(strings.TrimRight(raw, ".,!?;:)]}"))
	}
	return links
}

func (links turnWebLinks) add(raw string) {
	u, err := url.Parse(raw)
	if err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Hostname() != "" &&
		u.User == nil && !strings.Contains(raw, "#") {
		links[raw] = true
	}
}

func (links turnWebLinks) allows(value any) bool {
	raw, _ := value.(string)
	return links[strings.TrimSpace(raw)]
}
