package adapters

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/kindstream"
	"github.com/braedonsaunders/ether/internal/newznab"
	"github.com/braedonsaunders/ether/internal/nzb"
	"github.com/braedonsaunders/ether/internal/pluginitems"
)

// PluginNZBStore is Ether's shared indexer and vault path.
type PluginNZBStore interface {
	PluginReleaseReferences(context.Context, []core.Release) ([]string, []string)
	RequestPluginNZB(context.Context, string, core.Release) ([]byte, string, error)
	PluginLegacyReferences(context.Context, []core.Release) []string
	ResolvePluginTorrent(context.Context, core.Release) (core.Release, error)
}

func (r *KindResolver) fetchPluginNZB(ctx context.Context, rel core.Release) ([]byte, error) {
	if r.pluginNZBs == nil {
		return r.indexer.FetchNZB(ctx, rel)
	}
	data, _, err := r.pluginNZBs.RequestPluginNZB(ctx, rel.NZBRef, rel)
	return data, err
}

func (r *KindResolver) canReuseVaultedSource(src core.PlayableSource) bool {
	if r.pluginNZBs == nil || src.BlobRef != "" {
		return false
	}
	src = kindstream.NormalizeNZBSource(src)
	if !strings.EqualFold(strings.TrimSpace(src.Pattern), kindstream.PatternRelease) {
		return false
	}
	rel, err := releaseFromSource(src, "", "")
	return err == nil && rel.Protocol == core.ProtocolUsenet && (rel.NZBRef != "" || rel.NZBURL != "") &&
		!r.usenetPlaybackFailed(src)
}

func (r *KindResolver) openSavedKindSource(ctx context.Context, rec pluginitems.KindRecord, lib core.Library, item core.KindItem, partIdx *int, sourceID string) (*frontend.KindMedia, error) {
	if r.pluginNZBs == nil {
		return nil, nil
	}
	src, ok, _, err := r.items.GetPartResolutionForOpen(ctx, item.Ref, partIdx)
	if err != nil || !ok || !r.canReuseVaultedSource(src) {
		return nil, nil
	}
	src.ExpiresAt = 0
	if _, matches := matchPlayableSource([]core.PlayableSource{src}, item.Title, sourceID, r.now().Unix()); !matches {
		return nil, nil
	}
	openCtx, cancel := withHardTimeout(newznab.WithVaultedPluginNZB(ctx), r.candidateOpenTimeout)
	defer cancel()
	media, err := r.openSingleSource(openCtx, rec, lib, item, partIdx, src, false, nil)
	if ctx.Err() == nil && errors.Is(err, core.ErrSourceUnavailable) {
		return nil, nil
	}
	return media, err
}

// publicReleaseLinks mirrors opaque references into the deprecated link fields.
func (r *KindResolver) publicReleaseLinks(c core.SourceCandidate) core.SourceCandidate {
	if r.pluginNZBs == nil {
		return c
	}
	c.NZBRef = candidateNZBRef(c)
	if c.TorrentRef == "" {
		if ref, ok := strings.CutPrefix(c.TorrentURL, core.PluginTorrentRefPrefix); ok {
			c.TorrentRef = ref
		}
	}
	c.NZBURL, c.TorrentURL = "", ""
	if c.NZBRef != "" {
		c.NZBURL = core.PluginNZBRefPrefix + c.NZBRef
	}
	if c.TorrentRef != "" {
		c.TorrentURL = core.PluginTorrentRefPrefix + c.TorrentRef
	}
	return c
}

// publicLegacyCandidate retains the reference for callers that submitted a
// saved raw locator to inspect. Successful probes never echo the old URL.
func (r *KindResolver) publicLegacyCandidate(ctx context.Context, c core.SourceCandidate) core.SourceCandidate {
	if r.pluginNZBs != nil && candidateNZBRef(c) == "" && c.NZBURL != "" {
		c.NZBRef = r.pluginNZBs.PluginLegacyReferences(ctx, []core.Release{{
			NZBURL: c.NZBURL, IndexerID: c.IndexerID,
		}})[0]
	}
	return r.publicReleaseLinks(c)
}

// NormalizeStoredReleaseLinks resolves legacy locators on demand in the JSON
// blobs that plugins use for saved releases and item hints. Leave other KV
// values alone and keep source IDs derived from the same links in sync.
func (r *KindResolver) NormalizeStoredReleaseLinks(ctx context.Context, raw []byte) ([]byte, error) {
	if r.pluginNZBs == nil || (!bytes.Contains(raw, []byte("nzbUrl")) &&
		!bytes.Contains(raw, []byte("torrentUrl")) && !bytes.Contains(raw, []byte("nzb:http")) &&
		!bytes.Contains(raw, []byte("magnet:"))) {
		return raw, nil
	}
	var value any
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if !json.Valid(raw) || dec.Decode(&value) != nil {
		return raw, nil
	}

	type locator struct {
		link     string
		protocol string
	}
	known := make(map[locator]int)
	aliases := make(map[string]string)
	var releases []core.Release
	collect := func(key, value string) {
		if (key == "magnet" || key == "torrentUrl") && strings.HasPrefix(value, "magnet:") {
			if bare := newznab.BareMagnet(newznab.InfoHashFromMagnet(value)); bare != value {
				aliases[value] = bare
				aliases["mag:"+value[:min(len(value), 80)]] = "mag:" + bare
			}
			return
		}
		var protocol string
		switch {
		case key == "nzbUrl", strings.HasPrefix(value, "nzb:http"):
			protocol = core.ProtocolUsenet
		case key == "torrentUrl":
			protocol = core.ProtocolTorrent
		default:
			return
		}
		link := strings.TrimPrefix(value, "nzb:")
		scheme, _, _ := strings.Cut(link, "://")
		if !strings.EqualFold(scheme, "http") && !strings.EqualFold(scheme, "https") {
			return
		}
		loc := locator{link, protocol}
		if _, exists := known[loc]; !exists {
			known[loc] = len(releases)
			releases = append(releases, core.Release{Protocol: protocol, NZBURL: link})
		}
	}
	var visit func(any)
	visit = func(node any) {
		switch node := node.(type) {
		case string:
			collect("", node)
		case []any:
			for _, v := range node {
				visit(v)
			}
		case map[string]any:
			for key, v := range node {
				collect("", key)
				if s, ok := v.(string); ok {
					collect(key, s)
				} else {
					visit(v)
				}
			}
		}
	}
	visit(value)
	if len(releases) == 0 && len(aliases) == 0 {
		return raw, nil
	}
	refs := r.pluginNZBs.PluginLegacyReferences(ctx, releases)
	for loc, i := range known {
		if refs[i] == "" {
			aliases[loc.link] = ""
			aliases["nzb:"+loc.link] = ""
			continue
		}
		prefix := core.PluginNZBRefPrefix
		if loc.protocol == core.ProtocolTorrent {
			prefix = core.PluginTorrentRefPrefix
		}
		aliases[loc.link] = prefix + refs[i]
		aliases["nzb:"+loc.link] = "nzb:" + prefix + refs[i]
	}
	return json.Marshal(rewriteStoredReleaseAliases(value, aliases))
}

func rewriteStoredReleaseAliases(node any, aliases map[string]string) any {
	switch node := node.(type) {
	case string:
		if replacement, ok := aliases[node]; ok {
			return replacement
		}
	case []any:
		for i, v := range node {
			node[i] = rewriteStoredReleaseAliases(v, aliases)
		}
	case map[string]any:
		out := make(map[string]any, len(node))
		for key, v := range node {
			// Music's staged candidate maps also key entries by release ID.
			if replacement, ok := aliases[key]; ok {
				if replacement == "" {
					continue
				}
				key = replacement
			}
			out[key] = rewriteStoredReleaseAliases(v, aliases)
		}
		return out
	}
	return node
}

func candidateNZBRef(c core.SourceCandidate) string {
	if c.NZBRef != "" {
		return strings.TrimPrefix(c.NZBRef, core.PluginNZBRefPrefix)
	}
	locator := strings.TrimPrefix(c.NZBURL, "nzb:")
	if ref, ok := strings.CutPrefix(locator, core.PluginNZBRefPrefix); ok {
		return ref
	}
	return ""
}

func candidateNZBKey(c core.SourceCandidate) string {
	if ref := candidateNZBRef(c); ref != "" {
		return "ref:" + hashNZBBytes([]byte(ref))
	}
	return normalizeNZBKey(c.NZBURL)
}

func (r *KindResolver) requestPluginNZB(ctx context.Context, q core.SourceQuery) ([]core.SourceCandidate, error) {
	if r.pluginNZBs == nil {
		return nil, core.ErrNotAvailable
	}
	var rel core.Release
	if q.NZBIdentity != nil {
		rel.IndexerID = strings.TrimSpace(q.NZBIdentity.IndexerID)
		rel.GUID = strings.TrimSpace(q.NZBIdentity.GUID)
	}
	if len(rel.IndexerID) > 256 || len(rel.GUID) > 4096 {
		return nil, core.ErrReleaseRejected
	}
	data, storeRef, err := r.pluginNZBs.RequestPluginNZB(ctx, strings.TrimSpace(q.NZBRef), rel)
	if err != nil {
		return nil, err
	}
	doc, err := nzb.Parse(data)
	if err != nil {
		return nil, core.ErrReleaseRejected
	}
	files := make([]core.ReleaseFile, 0, min(len(doc.Files), 2000))
	for i, file := range doc.Files[:cap(files)] {
		files = append(files, core.ReleaseFile{Index: i, Path: file.StableName(), Size: file.EstimatedDecodedBytes(), SizeApproximate: true})
	}
	return []core.SourceCandidate{r.publicReleaseLinks(core.SourceCandidate{NZBRef: storeRef, Files: files, FileCount: len(doc.Files)})}, nil
}
