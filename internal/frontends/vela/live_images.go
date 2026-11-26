package vela

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/kit/reqmemo"
)

// liveImages rewrites the image URLs a plugin put in live-event payloads
// (league logos, competitor and entity crests) into signed Ether image grants,
// and channel logos on watch targets into the channel's own grant. A raw
// upstream URL never reaches the client: a logo Ether cannot sign is dropped.
//
// One signer serves one response (or one stream), so a crest repeated across a
// screen is signed once.
type liveImages struct {
	i     *instance
	r     *http.Request
	token string
	// mu guards issued: one signer can be shared by concurrent readers of one
	// response (screen data sources resolve in parallel).
	mu           sync.Mutex
	issued       map[string]string
	viewer       *frontend.Viewer
	channelCards *streamChannelCards
}

type channelCardPage struct {
	order []string
	cards map[string]any
}

type streamChannelCards struct {
	mu                 sync.Mutex
	revision           frontend.LiveChannelRevision
	nextCheck, expires time.Time
	valid              bool
	pages              map[string]channelCardPage
}

const channelCardRevisionInterval = 2 * time.Second

func (i *instance) newLiveImages(r *http.Request, token string) *liveImages {
	// One access decision per plugin and host for everything this signer
	// signs (fetching an image still checks again).
	r = r.WithContext(frontend.WithPluginImageMemo(r.Context()))
	return &liveImages{i: i, r: r, token: token, issued: map[string]string{}}
}

func (s *liveImages) plugin(pluginID, raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	if strings.HasPrefix(raw, "/vela/v0/images/") {
		return raw
	}
	key := "p\x00" + pluginID + "\x00" + raw
	s.mu.Lock()
	defer s.mu.Unlock()
	if got, ok := s.issued[key]; ok {
		return got
	}
	out := ""
	if img, ok := frontend.PluginImage(pluginID, raw); ok && s.i.host.ImageGrants != nil {
		secret, err := s.i.host.ImageGrants.Issue(s.r.Context(), frontend.ImageGrantRequest{Credential: s.token, Device: requestDevice(s.r), Image: img})
		if err == nil && secret != "" {
			out = "/vela/v0/images/" + secret
		}
	}
	s.issued[key] = out
	return out
}

// Plugin and Channel make the signer the screens' image signer.
func (s *liveImages) Plugin(pluginID, raw string) string { return s.plugin(pluginID, raw) }

// Channel signs a channel's own logo.
func (s *liveImages) Channel(channelID string) string { return s.channel(channelID) }

func (s *liveImages) channel(channelID string) string {
	channelID = strings.TrimSpace(channelID)
	if channelID == "" {
		return ""
	}
	key := "c\x00" + channelID
	s.mu.Lock()
	defer s.mu.Unlock()
	if got, ok := s.issued[key]; ok {
		return got
	}
	out := ""
	if issuer, ok := s.i.host.ImageGrants.(frontend.ChannelLogoIssuer); ok {
		secret, err := issuer.IssueChannelLogo(s.r.Context(), s.token, requestDevice(s.r), channelID)
		if err == nil && secret != "" {
			out = "/vela/v0/images/" + secret
		}
	}
	s.issued[key] = out
	return out
}

// sign marshals v and rewrites every "logo" in it. pluginID is the plugin the
// payload belongs to when the payload itself does not say (a screen's
// top-level pluginId, an event's namespaced id or an entity's key say so).
func (s *liveImages) sign(v any, pluginID string) (json.RawMessage, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return s.signRaw(b, pluginID)
}

func (s *liveImages) signRaw(b []byte, pluginID string) (json.RawMessage, error) {
	if !bytes.Contains(b, []byte(`"logo"`)) {
		return b, nil
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	var root any
	if err := dec.Decode(&root); err != nil {
		return nil, err
	}
	root = s.walk(root, pluginID, "")
	return json.Marshal(root)
}

func (s *liveImages) walk(node any, pluginID, parentKey string) any {
	switch n := node.(type) {
	case []any:
		for idx, child := range n {
			n[idx] = s.walk(child, pluginID, parentKey)
		}
		return n
	case map[string]any:
		pluginID = objectPlugin(n, pluginID)
		watchTarget := parentKey == "watch" || parentKey == "alternates"
		for k, child := range n {
			if k == "logo" {
				raw, _ := child.(string)
				var signed string
				if strings.HasPrefix(raw, "/vela/v0/images/") {
					// Already a grant (signed where the data was built).
					signed = raw
				} else if watchTarget {
					// A watch target's logo is its channel's; anything else
					// (an add-on or plugin target) carries no logo.
					channelID, _ := n["channelId"].(string)
					if raw != "" {
						signed = s.channel(channelID)
					}
				} else if pluginID != "" {
					signed = s.plugin(pluginID, raw)
				}
				if signed == "" {
					delete(n, k)
				} else {
					n[k] = signed
				}
				continue
			}
			n[k] = s.walk(child, pluginID, k)
		}
		return n
	default:
		return node
	}
}

// objectPlugin reads the owning plugin from an object that names it: a
// resolved screen's pluginId, an entity's favourites key, or a live event's
// namespaced id ("pluginID:feedID:eventID").
func objectPlugin(n map[string]any, fallback string) string {
	if p, ok := n["pluginId"].(string); ok && strings.TrimSpace(p) != "" {
		return strings.TrimSpace(p)
	}
	if key, ok := n["key"].(string); ok {
		// "pluginID:feedID:entityKind:entityID"
		if parts := strings.SplitN(key, ":", 4); len(parts) == 4 && parts[0] != "" && parts[3] != "" {
			return parts[0]
		}
	}
	if id, ok := n["id"].(string); ok && strings.Count(id, ":") >= 2 {
		_, hasCompetitors := n["competitors"]
		_, hasStatus := n["status"]
		if hasCompetitors || hasStatus {
			if p, _, ok := strings.Cut(id, ":"); ok && p != "" {
				return p
			}
		}
	}
	return fallback
}

// TitleCards and ChannelCards make the signer the screens' card projection:
// titles and channels in a plugin screen are Vela's own cards, authorized
// for this viewer (anything the viewer may not see is left out).
func (s *liveImages) viewerOf() *frontend.Viewer {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.viewer == nil {
		if v, _, err := s.i.viewer(s.r); err == nil {
			s.viewer = v
		}
	}
	return s.viewer
}

// TitleCards projects keys into Vela title cards, keyed by rating key.
func (s *liveImages) TitleCards(ctx context.Context, keys []frontend.RatingKey) map[frontend.RatingKey]any {
	out := map[frontend.RatingKey]any{}
	v := s.viewerOf()
	batch, ok := s.i.host.Experience.(frontend.TitleBatchReader)
	if v == nil || !ok || len(keys) == 0 {
		return out
	}
	if len(keys) > 50 {
		keys = keys[:50]
	}
	titles, err := batch.BatchTitles(ctx, v, keys)
	if err != nil || len(titles) == 0 {
		return out
	}
	cards, err := s.i.titles(s.r.WithContext(ctx), v, s.token, titles)
	if err != nil {
		return out
	}
	for n, t := range titles {
		if n < len(cards) {
			out[t.Key] = cards[n]
		}
	}
	return out
}

// ChannelCards projects the viewer's Live TV channels (by id, or one owner
// group) into Vela channel cards; order is the request's (or the lineup's
// for a group).
func (s *liveImages) ChannelCards(ctx context.Context, ids []string, group string) ([]string, map[string]any) {
	if len(ids) == 0 && group == "" {
		return nil, nil
	}
	selection, _ := s.i.host.LiveTV.(frontend.LiveChannelSelection)
	memo := s.channelCards
	if memo == nil || selection == nil {
		page, _ := s.readChannelCards(ctx, selection, ids, group)
		return page.order, page.cards
	}
	memo.mu.Lock()
	defer memo.mu.Unlock()
	now := s.i.serverNow()
	// Durable writes are checked at a bounded rate. Broadcast schedules and
	// transient input health can also change without any database write.
	if !now.Before(memo.nextCheck) || !now.Before(memo.expires) {
		revision, err := selection.ChannelSelectionRevision(ctx)
		if err != nil && ctx.Err() != nil {
			return nil, nil
		}
		if err != nil || !memo.valid || memo.revision != revision || !now.Before(memo.expires) {
			memo.pages = make(map[string]channelCardPage)
			memo.expires = now.Add(liveStreamHeartbeatSec)
		}
		memo.valid = err == nil
		memo.revision = revision
		memo.nextCheck = now.Add(channelCardRevisionInterval)
	}
	if !memo.valid {
		return nil, nil
	}
	key := hashJSON([]any{ids, group})
	if page, ok := memo.pages[key]; ok {
		return page.order, page.cards
	}
	fresh := frontend.WithFreshRequestMemo(reqmemo.With(ctx))
	page, ok := s.readChannelCards(fresh, selection, ids, group)
	if ok && len(memo.pages) < 32 {
		memo.pages[key] = page
	}
	return page.order, page.cards
}

func (s *liveImages) readChannelCards(ctx context.Context, selection frontend.LiveChannelSelection, ids []string, group string) (channelCardPage, bool) {
	out := map[string]any{}
	v := s.viewerOf()
	if v == nil || s.i.host.LiveTV == nil {
		return channelCardPage{}, false
	}
	var channels []frontend.LiveChannel
	var err error
	if selection != nil {
		selected := ids
		if group != "" {
			selected = nil
		}
		channels, err = selection.ReadChannelSelection(ctx, v, selected)
	} else {
		channels, err = s.i.host.LiveTV.Channels(ctx, v)
	}
	if err != nil {
		return channelCardPage{}, false
	}
	want := map[string]bool{}
	for _, id := range ids {
		want[id] = true
	}
	var order []string
	memo := newLiveArtCache()
	complete := true
	for _, c := range channels {
		inGroup := false
		if group != "" {
			for _, g := range c.Groups {
				inGroup = inGroup || g.ID == group || strings.EqualFold(g.Name, group)
			}
			inGroup = inGroup || strings.EqualFold(c.Group, group)
		}
		if !want[c.ID] && !inGroup {
			continue
		}
		art, err := s.i.liveArtworkCached(s.r.WithContext(ctx), s.token, 0, c.ID, c.Images, memo)
		if err != nil {
			complete = false
			continue
		}
		out[c.ID] = object{"id": c.ID, "number": c.Number, "name": c.Name, "kind": c.Kind, "artwork": art, "favorite": c.Favorite}
		if group != "" {
			order = append(order, c.ID)
		}
		if len(out) >= 50 {
			break
		}
	}
	if group == "" {
		for _, id := range ids {
			if _, ok := out[id]; ok {
				order = append(order, id)
			}
		}
	}
	return channelCardPage{order: order, cards: out}, complete
}
