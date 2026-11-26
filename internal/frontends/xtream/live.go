package xtream

import (
	"encoding/base64"
	"encoding/xml"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/braedonsaunders/ether/internal/frontend"
)

func xtreamOwnerGroupRef(id string) string {
	return "owner\n" + id
}

func (i *instance) channels(r *http.Request, l login) ([]frontend.LiveChannel, error) {
	cs, err := i.host.LiveTV.Channels(r.Context(), l.viewer)
	if err != nil {
		return nil, err
	}
	for start := 0; start < len(cs); start += 250 {
		refs := []string{}
		groups := []string{}
		seen := map[string]bool{}
		for _, c := range cs[start:min(start+250, len(cs))] {
			refs = append(refs, c.ID)
			if len(c.Groups) > 0 {
				for _, g := range c.Groups {
					ref := xtreamOwnerGroupRef(g.ID)
					if !seen[ref] {
						groups = append(groups, ref)
						seen[ref] = true
					}
				}
				continue
			}
			if group := strings.TrimSpace(c.Group); group != "" && !seen[group] {
				groups = append(groups, group)
				seen[group] = true
			}
		}
		allocated, err := i.host.IDs.CompactBatch(r.Context(), "xtream", "channel", refs)
		if err != nil {
			return nil, err
		}
		for ref, id := range allocated {
			l.channels[ref] = id
		}
		if len(groups) > 0 {
			allocated, err := i.host.IDs.CompactBatch(r.Context(), "xtream", "live-group", groups)
			if err != nil {
				return nil, err
			}
			for ref, id := range allocated {
				if id <= 0 || id > 2147483645 {
					return nil, fmt.Errorf("invalid live group identity")
				}
				l.groups[ref] = id + 2 // preserve the existing ungrouped categories 1 and 2
			}
		}
	}
	return cs, nil
}

func liveCategoryInt(l login, c frontend.LiveChannel) int32 {
	if len(c.Groups) > 0 {
		if id := l.groups[xtreamOwnerGroupRef(c.Groups[0].ID)]; id > 0 {
			return id
		}
	}
	if id := l.groups[strings.TrimSpace(c.Group)]; id > 0 {
		return id
	}
	if c.Kind == "iptv" {
		return 2
	}
	return 1
}

func liveCategory(l login, c frontend.LiveChannel) string {
	return strconv.FormatInt(int64(liveCategoryInt(l, c)), 10)
}

func liveCategoryIDs(l login, c frontend.LiveChannel) []int32 {
	if len(c.Groups) > 0 {
		out := make([]int32, 0, len(c.Groups))
		for _, g := range c.Groups {
			if id := l.groups[xtreamOwnerGroupRef(g.ID)]; id > 0 {
				out = append(out, id)
			}
		}
		if len(out) > 0 {
			return out
		}
	}
	return []int32{liveCategoryInt(l, c)}
}

func liveChannelInCategory(l login, c frontend.LiveChannel, cat string) bool {
	if cat == "" || cat == "0" {
		return true
	}
	if len(c.Groups) > 0 {
		for _, g := range c.Groups {
			if id := l.groups[xtreamOwnerGroupRef(g.ID)]; id > 0 && strconv.FormatInt(int64(id), 10) == cat {
				return true
			}
		}
		return false
	}
	return cat == liveCategory(l, c)
}

func (i *instance) live(w http.ResponseWriter, r *http.Request, l login, action string) {
	channels, err := i.channels(r, l)
	if err != nil {
		http.Error(w, "Unavailable", 503)
		return
	}
	if action == "get_live_categories" {
		out := []map[string]any{}
		seen := map[string]bool{}
		var ownerGroups []frontend.LiveGroup
		if lg, ok := i.host.LiveTV.(frontend.LiveTVGroups); ok {
			ownerGroups, err = lg.Groups(r.Context(), l.viewer)
			if err != nil {
				http.Error(w, "Unavailable", 503)
				return
			}
		} else {
			seenOwner := map[string]bool{}
			for _, c := range channels {
				for _, g := range c.Groups {
					if !seenOwner[g.ID] {
						seenOwner[g.ID] = true
						ownerGroups = append(ownerGroups, g)
					}
				}
			}
		}
		for _, g := range ownerGroups {
			if id := l.groups[xtreamOwnerGroupRef(g.ID)]; id > 0 {
				catID := strconv.FormatInt(int64(id), 10)
				if !seen[catID] {
					seen[catID] = true
					out = append(out, map[string]any{"category_id": catID, "category_name": g.Name, "parent_id": 0})
				}
			}
		}
		for _, c := range channels {
			if len(c.Groups) > 0 {
				continue
			}
			id := liveCategory(l, c)
			if !seen[id] {
				seen[id] = true
				name := "On Air"
				if id == "2" {
					name = "IPTV"
				}
				if group := strings.TrimSpace(c.Group); group != "" {
					name = group
				}
				out = append(out, map[string]any{"category_id": id, "category_name": name, "parent_id": 0})
			}
		}
		jsonResponse(w, out)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_, _ = w.Write([]byte("["))
	first := true
	num := 0
	catFilter := r.URL.Query().Get("category_id")
	for _, c := range channels {
		if !liveChannelInCategory(l, c, catFilter) {
			continue
		}
		if !first {
			_, _ = w.Write([]byte(","))
		}
		first = false
		num++
		archive := 0
		if c.ArchiveDays > 0 {
			archive = 1
		}
		_ = jsonEncoder(w, map[string]any{"num": num, "name": c.Name, "stream_type": "live", "stream_id": l.channels[c.ID], "stream_icon": i.image(r, l, 0, c.ID, c.Images), "epg_channel_id": c.ID, "added": "0", "category_id": liveCategory(l, c), "category_ids": liveCategoryIDs(l, c), "custom_sid": "", "tv_archive": archive, "tv_archive_duration": c.ArchiveDays, "direct_source": ""})
	}
	_, _ = w.Write([]byte("]"))
}

func (i *instance) findChannel(r *http.Request, l login, id string) (frontend.LiveChannel, bool) {
	cs, err := i.channels(r, l)
	if err != nil {
		return frontend.LiveChannel{}, false
	}
	for _, c := range cs {
		if strconv.FormatInt(int64(l.channels[c.ID]), 10) == id {
			return c, true
		}
	}
	return frontend.LiveChannel{}, false
}

func (i *instance) liveNow() time.Time {
	if i.host.Now != nil {
		return i.host.Now().UTC()
	}
	return time.Now().UTC()
}

func (i *instance) programmes(r *http.Request, l login, c frontend.LiveChannel, from, until time.Time) ([]frontend.LiveProgramme, error) {
	return i.host.LiveTV.Guide(r.Context(), l.viewer, frontend.LiveGuideQuery{Device: l.device, Channels: []string{c.ID}, From: from, Until: until, Limit: 10000})
}

func (i *instance) programmeIcon(r *http.Request, l login, p frontend.LiveProgramme) string {
	return frontend.ProgrammeIconURL(r.Context(), i.host.ImageGrants, i.base(r), p, l.credential, l.device)
}

func (i *instance) epg(w http.ResponseWriter, r *http.Request, l login, action string) {
	c, ok := i.findChannel(r, l, r.URL.Query().Get("stream_id"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	now := i.liveNow()
	from := now.Add(-6 * time.Hour)
	until := now.Add(48 * time.Hour)
	if action == "get_simple_data_table" && c.ArchiveDays > 0 {
		from = now.AddDate(0, 0, -min(c.ArchiveDays, 30))
	}
	ps, err := i.programmes(r, l, c, from, until)
	if err != nil {
		http.Error(w, "Unavailable", 503)
		return
	}
	limit := 10000
	if action == "get_short_epg" {
		limit = 4
		if n, e := strconv.Atoi(r.URL.Query().Get("limit")); e == nil && n > 0 {
			limit = min(n, 1000)
		}
	}
	out := []map[string]any{}
	for _, p := range ps {
		if p.ChannelID != c.ID || !p.End.After(p.Start) || action == "get_short_epg" && !p.End.After(now) {
			continue
		}
		playing := 0
		if !p.Start.After(now) && p.End.After(now) {
			playing = 1
		}
		hasArchive := 0
		if ended, _ := frontend.ArchiveFlags(c.ArchiveDays, p.Start, p.End, now); ended {
			hasArchive = 1
		}
		row := map[string]any{"id": p.ID, "epg_id": strconv.FormatInt(int64(l.channels[c.ID]), 10), "channel_id": c.ID, "title": base64.StdEncoding.EncodeToString([]byte(p.Title)), "description": base64.StdEncoding.EncodeToString([]byte(p.Summary)), "lang": "", "start": p.Start.UTC().Format("2006-01-02 15:04:05"), "end": p.End.UTC().Format("2006-01-02 15:04:05"), "start_timestamp": strconv.FormatInt(p.Start.Unix(), 10), "stop_timestamp": strconv.FormatInt(p.End.Unix(), 10), "now_playing": playing, "has_archive": hasArchive}
		if icon := i.programmeIcon(r, l, p); icon != "" {
			row["icon"] = icon
		}
		out = append(out, row)
		if len(out) >= limit {
			break
		}
	}
	jsonResponse(w, map[string]any{"epg_listings": out})
}

type programmeXMLIcon struct {
	Source string `xml:"src,attr"`
}

func (i *instance) xmltv(w http.ResponseWriter, r *http.Request, l login) {
	cs, err := i.channels(r, l)
	if err != nil {
		http.Error(w, "Unavailable", 503)
		return
	}
	w.Header().Set("Content-Type", "application/xml; charset=utf-8")
	_, _ = w.Write([]byte(xml.Header + "<tv>"))
	e := xml.NewEncoder(w)
	for _, c := range cs {
		_ = e.Encode(struct {
			XMLName xml.Name `xml:"channel"`
			ID      string   `xml:"id,attr"`
			Name    string   `xml:"display-name"`
		}{ID: c.ID, Name: c.Name})
	}
	now := i.liveNow()
	for _, c := range cs {
		ps, err := i.programmes(r, l, c, now.Add(-6*time.Hour), now.Add(48*time.Hour))
		if err != nil {
			return
		}
		for _, p := range ps {
			if p.ChannelID != c.ID || !p.End.After(p.Start) {
				continue
			}
			var icon *programmeXMLIcon
			if src := i.programmeIcon(r, l, p); src != "" {
				icon = &programmeXMLIcon{Source: src}
			}
			_ = e.Encode(struct {
				XMLName xml.Name          `xml:"programme"`
				Channel string            `xml:"channel,attr"`
				Start   string            `xml:"start,attr"`
				Stop    string            `xml:"stop,attr"`
				Title   string            `xml:"title"`
				Desc    string            `xml:"desc"`
				Icon    *programmeXMLIcon `xml:"icon,omitempty"`
			}{Icon: icon, Channel: c.ID, Start: p.Start.UTC().Format("20060102150405 -0700"), Stop: p.End.UTC().Format("20060102150405 -0700"), Title: p.Title, Desc: p.Summary})
		}
	}
	_ = e.Flush()
	_, _ = w.Write([]byte("</tv>"))
}

func cleanM3U(s string) string {
	return strings.NewReplacer("\r", " ", "\n", " ", "\"", "'").Replace(s)
}

func (i *instance) playlist(w http.ResponseWriter, r *http.Request, l login) {
	base := i.base(r)
	if base == "" {
		http.Error(w, "Unavailable", 503)
		return
	}
	cs, err := i.channels(r, l)
	if err != nil {
		http.Error(w, "Unavailable", 503)
		return
	}
	w.Header().Set("Content-Type", "audio/x-mpegurl; charset=utf-8")
	_, _ = fmt.Fprintln(w, "#EXTM3U")
	liveExt := "m3u8"
	if strings.EqualFold(r.URL.Query().Get("output"), "ts") || strings.EqualFold(r.URL.Query().Get("output"), "mpegts") {
		liveExt = "ts"
	}
	for _, c := range cs {
		group := strings.TrimSpace(c.Group)
		if len(c.Groups) > 0 {
			group = strings.TrimSpace(c.Groups[0].Name)
		}
		if group == "" {
			group = "Live TV"
		}
		_, _ = fmt.Fprintf(w, "#EXTINF:-1 tvg-id=\"%s\" tvg-logo=\"%s\" group-title=\"%s\",%s\n%s/live/%s/%s/%d.%s\n", cleanM3U(c.ID), cleanM3U(i.image(r, l, 0, c.ID, c.Images)), cleanM3U(group), cleanM3U(c.Name), base, l.username, l.password, l.channels[c.ID], liveExt)
	}
	// Movies are emitted a page at a time; exporting never accumulates the catalogue.
	if err := i.walk(r, l, "movie", func(l login, t frontend.Title, cat string) error {
		_, err := fmt.Fprintf(w, "#EXTINF:-1 group-title=\"Movies\",%s\n%s/movie/%s/%s/%d.mkv\n", cleanM3U(t.Title), base, l.username, l.password, l.ids[t.Key])
		return err
	}); err != nil {
		return
	}
	_ = i.walk(r, l, "show", func(_ login, show frontend.Title, _ string) error {
		return i.children(r, l, show.Key, func(season frontend.Title) error {
			if string(season.Type) != "season" {
				return nil
			}
			return i.children(r, l, season.Key, func(ep frontend.Title) error {
				if string(ep.Type) != "episode" {
					return nil
				}
				_, err := fmt.Fprintf(w, "#EXTINF:-1 group-title=\"Series\",%s S%02dE%02d %s\n%s/series/%s/%s/%d.mkv\n", cleanM3U(show.Title), ep.Season, ep.Episode, cleanM3U(ep.Title), base, l.username, l.password, l.ids[ep.Key])
				return err
			})
		})
	})
}
