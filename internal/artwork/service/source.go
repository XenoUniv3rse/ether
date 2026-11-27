// Package service owns shared artwork authorization, selection and rendering.
package service

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/braedonsaunders/ether/internal/artwork"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/experience"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/livetv"
	"github.com/braedonsaunders/ether/internal/reqmemo"
)

// Source has no protocol server or protocol wire model dependencies.
type Source struct {
	OwnerEnabled     func() bool
	PosterGen        func() uint64
	OwnerDir         string
	AssetFolder      func() string
	Assets           *artwork.LocalAssets
	LandscapeThumb   bool
	Catalog          core.StoredTitleLookup
	CollectionDevice string
	Collections      core.Collections
	CollectionCards  func(context.Context, *core.User, []core.RatingKey) (map[core.RatingKey]*core.Collection, error)
	Playlists        core.Playlists
	Visibility       experience.Visibility
	LiveTV           interface {
		PermittedChannels(context.Context, *core.User, []string) (map[string]livetv.Channel, error)
	}
	Discovery        *frontend.DiscoveryFeatures
	Images           *RemoteCache
	ImageURL         core.ImageURLFunc
	Palettes         *PaletteCache
	Now              func() time.Time
	Variants         func(*core.Title, string) (*frontend.ArtworkImages, bool)
	Metadata         func(context.Context, frontend.ArtworkRequest, frontend.Image) frontend.Image
	meta             *MetadataQueue
	VirtualSeason    func(context.Context, *core.User, core.RatingKey) (*core.Title, error)
	PluginItem       func(context.Context, *core.User, core.Device, core.RatingKey) (*core.Title, []frontend.Image, error)
	LibraryLiveThumb func(context.Context, *core.Viewer, string, string) (string, error)
	// PluginImage authorizes a plugin-named image (frontend.PluginImagePrefix)
	// for viewer and returns the upstream URL to fetch.
	PluginImage  func(context.Context, *core.Viewer, string, string) (string, error)
	ViewerAvatar func(context.Context, *core.Viewer) ([]byte, string, error)
	URLImages    interface {
		GetUntrustedURL(context.Context, string) ([]byte, string, error)
	}
	Raster   frontend.SVGRasterizer
	Collages *LibraryCollageCache
	// Generated draws Ether's poster for a movie or show with no artwork
	// (*artwork.Renderer). Nil leaves such titles without a poster entry.
	Generated interface {
		Title(context.Context, artwork.TitlePoster) ([]byte, string, error)
	}
}
type artworkVirtualSeasonKey struct{}

func (a Source) now() time.Time {
	if a.Now != nil {
		return a.Now()
	}
	return time.Now()
}
func (a Source) PosterGeneration() uint64 {
	if a.PosterGen != nil {
		if gen := a.PosterGen(); gen > 0 {
			return gen
		}
	}
	if a.Discovery != nil && a.Discovery.Overlays != nil {
		if pg, ok := a.Discovery.Overlays.(interface{ PosterGeneration() uint64 }); ok {
			return pg.PosterGeneration()
		}
	}
	return 0
}
func (a Source) title(ctx context.Context, key core.RatingKey) (*core.Title, error) {
	if t, ok := frontend.ArtworkTitle(ctx, key); ok {
		return t, nil
	}
	if t, ok := ctx.Value(artworkVirtualSeasonKey{}).(*core.Title); ok && t != nil && t.Key == key {
		return t, nil
	}
	if a.Catalog == nil {
		return nil, core.ErrNotAvailable
	}
	return a.Catalog.StoredTitle(ctx, key)
}

type visMemoKey struct{}

type visMemo struct {
	mu   sync.Mutex
	seen map[core.RatingKey]bool
	val  map[core.RatingKey]bool
}

func withVisMemo(ctx context.Context) context.Context {
	if ctx.Value(visMemoKey{}) != nil {
		return ctx
	}
	return context.WithValue(ctx, visMemoKey{}, &visMemo{})
}

func (a Source) visible(ctx context.Context, v *core.User, t *core.Title) bool {
	if t == nil {
		return false
	}
	memo, _ := ctx.Value(visMemoKey{}).(*visMemo)
	if memo != nil {
		memo.mu.Lock()
		if memo.seen[t.Key] {
			ok := memo.val[t.Key]
			memo.mu.Unlock()
			return ok
		}
		memo.mu.Unlock()
	}
	ok := a.judgeVisible(ctx, v, t)
	if memo != nil {
		memo.mu.Lock()
		if memo.seen == nil {
			memo.seen = map[core.RatingKey]bool{}
			memo.val = map[core.RatingKey]bool{}
		}
		memo.seen[t.Key] = true
		memo.val[t.Key] = ok
		memo.mu.Unlock()
	}
	return ok
}

func (a Source) judgeVisible(ctx context.Context, v *core.User, t *core.Title) bool {
	vis := a.Visibility
	vis.GetTitle = a.title
	return vis.Visible(ctx, v, t)
}
func ultraBlurType(t core.MediaType) bool { return t == core.TypeMovie || t == core.TypeShow }

// discoveryIdentity preserves artwork-v1 hash material. It is never exposed as
// a route or credential; protocol image URLs remain adapter-owned.
func discoveryIdentity(key core.RatingKey, version, kind, lang string) string {
	suffix := "?lang=" + url.QueryEscape(lang)
	if kind != "" {
		suffix = "?kind=" + kind + "&lang=" + url.QueryEscape(lang)
	}
	return fmt.Sprintf("/library/metadata/%s/discoveryPoster/%s%s", key.String(), version, suffix)
}

type frontendArtworkEntry struct {
	image                         frontend.Image
	ownerKind                     string
	element, personKind, personID string
	subject                       string
	discoveryKind                 string
	discovery                     bool
	remotePath                    string
	immutable                     bool
	channel                       bool
	programmePath                 string
	channelLogo                   string
	channelNumber, channelName    string
	ultra                         *Palette
	virtualSeason                 *core.Title
	generated                     *artwork.TitlePoster
}

// Absolute artwork paths are passed to the guarded URL image store by remote.
func validRemoteArtwork(path string) bool {
	return artwork.ValidPath(path) || strings.HasPrefix(path, "https://") || strings.HasPrefix(path, "http://")
}

func artworkReference(req frontend.ArtworkRequest, kind, source string) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%d\x00%d\x00%s\x00%s\x00%s\x00%s", req.Viewer.ID, req.Item, req.Device.Language, kind, source, "artwork-v1")))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

type channelArtworkBatchKey struct{}

type channelArtworkBatch struct {
	viewer   int64
	app      string
	channels map[string]livetv.Channel
}

// PrepareChannelArtwork loads and authorizes the selected channels for a
// page of logo grants. Fetches continue to authorize each image independently.
func (a Source) PrepareChannelArtwork(ctx context.Context, viewer *core.Viewer, ids []string) (context.Context, error) {
	if a.LiveTV == nil || len(ids) == 0 {
		return ctx, nil
	}
	if policy, ok := a.Visibility.Policy.(core.UserPolicySource); ok {
		viewer = policy.UserPolicy(viewer)
	}
	if viewer == nil {
		return ctx, core.ErrUnauthorized
	}
	channels, err := a.LiveTV.PermittedChannels(ctx, viewer, ids)
	if err != nil {
		return ctx, err
	}
	batch := channelArtworkBatch{viewer: viewer.ID, app: core.LiveApp(ctx), channels: make(map[string]livetv.Channel, len(ids))}
	for _, id := range ids {
		batch.channels[id] = channels[id]
	}
	return context.WithValue(ctx, channelArtworkBatchKey{}, batch), nil
}

// oneChannel authorizes a single guide row. Home asks for a logo per
// channel; scanning every channel for each logo held the request for seconds.
func (a Source) oneChannel(ctx context.Context, viewer *core.User, id string) (livetv.Channel, error) {
	if a.LiveTV == nil || viewer == nil || id == "" {
		return livetv.Channel{}, core.ErrNotFound
	}
	if batch, ok := ctx.Value(channelArtworkBatchKey{}).(channelArtworkBatch); ok && batch.viewer == viewer.ID && batch.app == core.LiveApp(ctx) {
		if channel, checked := batch.channels[id]; checked {
			if channel.ID == "" {
				return livetv.Channel{}, core.ErrNotFound
			}
			return channel, nil
		}
	}
	channels, err := a.LiveTV.PermittedChannels(ctx, viewer, []string{id})
	if err != nil {
		return livetv.Channel{}, err
	}
	channel, ok := channels[id]
	if !ok {
		return livetv.Channel{}, core.ErrNotFound
	}
	return channel, nil
}

// ChannelImage describes a channel already filtered through AllowedChannels.
// The reference is not an authorization credential; List rechecks policy.
func (a Source) ChannelImage(viewer *core.Viewer, channel livetv.Channel) frontend.Image {
	if viewer == nil {
		return frontend.Image{}
	}
	req := frontend.ArtworkRequest{Viewer: viewer, Channel: channel.ID}
	ref := artworkReference(req, frontend.ImageChannel, channel.ID+"\x00"+channel.Logo+"\x00"+channel.Name+"\x00"+channel.Number)
	return frontend.Image{Kind: frontend.ImageChannel, Path: ref, Tag: ref}
}

func (a Source) entries(ctx context.Context, req frontend.ArtworkRequest) ([]frontendArtworkEntry, bool, error) {
	if frontend.CollectionCardArtwork(ctx) && req.Item.Kind() == core.KindSmart && req.Viewer != nil {
		return a.collectionCardEntries(ctx, req)
	}
	if a.LandscapeThumb {
		// Public cache tags must resolve to the same descriptor for every viewer.
		req.Device.Language = ""
	}
	ctx = withVisMemo(ctx)
	if req.Viewer == nil || ((req.Item == 0) == (req.Channel == "")) {
		return nil, false, core.ErrUnauthorized
	}
	s := a
	if policy, ok := s.Visibility.Policy.(core.UserPolicySource); ok {
		viewer := policy.UserPolicy(req.Viewer)
		if viewer == nil || viewer.ID != req.Viewer.ID {
			return nil, false, core.ErrUnauthorized
		}
		req.Viewer = viewer
	}

	if req.Channel != "" {
		if req.Programme != "" {
			return a.programmeEntries(ctx, req)
		}
		channel, err := s.oneChannel(ctx, req.Viewer, req.Channel)
		if err != nil {
			return nil, false, err
		}
		return []frontendArtworkEntry{{image: a.ChannelImage(req.Viewer, channel), channel: true, channelLogo: channel.Logo, channelNumber: channel.Number, channelName: channel.Name}}, true, nil
	}
	if req.Item.IsPluginKey() {
		return s.pluginEntries(ctx, req)
	}
	var title *core.Title
	var virtualSeason *core.Title
	var collectionVersion string
	switch req.Item.Kind() {
	case core.KindCollection, core.KindSmart:
		collection, err := s.collection(ctx, req)
		if err != nil {
			return nil, false, err
		}
		collectionVersion = collectionStamp(collection)
		if collection != nil {
			if _, bound := frontend.ArtworkCollection(ctx, req.Item); !bound {
				col := *collection
				if col.PosterPath == "" && s.Collections != nil {
					if p, err := s.Collections.Items(ctx, req.Viewer, req.Item, 0, 20); err == nil {
						for i := range p.Items {
							if p.Items[i].PosterPath != "" && s.visible(ctx, req.Viewer, &p.Items[i]) {
								col.PosterPath = p.Items[i].PosterPath
								break
							}
						}
					}
				}
				ctx = frontend.WithArtworkCollection(ctx, &col)
			}
		}
	case core.KindPlaylist:
		if s.Playlists == nil {
			return nil, false, core.ErrNotFound
		}
		if reader, ok := s.Playlists.(interface {
			Exists(context.Context, int64, core.RatingKey) (bool, error)
		}); ok {
			exists, err := reader.Exists(ctx, req.Viewer.ID, req.Item)
			if err != nil {
				return nil, false, err
			}
			if !exists {
				return nil, false, core.ErrNotFound
			}
		} else if _, err := s.Playlists.Get(ctx, req.Viewer.ID, req.Item); err != nil {
			return nil, false, err
		}
	default:
		if s.Catalog == nil {
			return nil, false, core.ErrNotFound
		}
		var err error
		title, err = s.title(ctx, req.Item)
		if errors.Is(err, core.ErrNotFound) && a.VirtualSeason != nil {
			title, err = a.VirtualSeason(ctx, req.Viewer, req.Item)
			if err == nil && title != nil {
				virtualSeason = title
				ctx = context.WithValue(ctx, artworkVirtualSeasonKey{}, title)
			}
		}
		if err != nil || title == nil || title.Key != req.Item || (virtualSeason == nil && !title.IsReleased(s.now()) && !s.undatedSearchHit(ctx, title.Key)) || !s.visible(ctx, req.Viewer, title) {
			return nil, false, core.ErrNotFound
		}
	}
	var entries []frontendArtworkEntry
	posterGen := a.PosterGeneration()
	primaryOnly := frontend.PrimaryArtworkOnly(ctx)
	var haveKind map[string]bool
	if primaryOnly {
		haveKind = map[string]bool{}
	}
	add := func(kind, source string, entry frontendArtworkEntry) {
		if !frontend.ArtworkKindWanted(ctx, kind) {
			return
		}
		if primaryOnly {
			if haveKind[kind] {
				return
			}
			haveKind[kind] = true
		}
		if posterGen > 0 && (kind == frontend.ImagePoster || (kind == frontend.ImageThumb && !a.LandscapeThumb) || entry.ownerKind != "") {
			source += "\x00pgen:" + strconv.FormatUint(posterGen, 10)
		}
		identityReq := req
		if a.LandscapeThumb {
			viewer := *req.Viewer
			viewer.ID = 0
			identityReq.Viewer = &viewer
		}
		ref := artworkReference(identityReq, kind, source)
		if entry.immutable || artwork.ValidPath(entry.remotePath) {
			ref = frontend.ImmutableArtworkPrefix + ref
		}
		entry.image = frontend.Image{Kind: kind, Path: ref, Tag: ref, Subject: entry.subject}
		entry.virtualSeason = virtualSeason
		entries = append(entries, entry)
	}
	var selected frontend.DiscoveryArtworkSelection
	if title != nil && virtualSeason == nil && s.Discovery != nil && experience.TitleAllowed(s.Visibility.Policy, req.Viewer, title) {
		selected = s.Discovery.SelectArtwork(ctx, req.Viewer, title, req.Device.Language)
	}
	for _, item := range []struct{ kind, element string }{{frontend.ImagePoster, "poster"}, {frontend.ImageThumb, "thumb"}, {frontend.ImageBackdrop, "art"}, {frontend.ImageLogo, "logo"}, {frontend.ImageBanner, "banner"}} {
		if !frontend.ArtworkKindWanted(ctx, item.kind) {
			// A page signer seals only its wanted kinds.
			continue
		}
		ownerKind := item.kind
		if ownerKind == frontend.ImageThumb {
			ownerKind = frontend.ImagePoster
		}
		if _, stamp := a.ownerImage(ctx, req.Item, ownerKind); stamp != "" {
			add(item.kind, stamp, frontendArtworkEntry{ownerKind: ownerKind})
			continue
		}
		if a.LandscapeThumb && item.kind == frontend.ImageThumb {
			if title != nil {
				path := title.BackdropPath
				if a.Variants != nil {
					if variants, ready := a.Variants(title, req.Device.Language); ready && variants != nil {
						for _, file := range variants.Backdrops {
							if !validRemoteArtwork(file.FilePath) || file.Width <= file.Height {
								continue
							}
							if path == "" {
								path = file.FilePath
							}
							if file.ISO639 != "" && file.ISO639 == title.Language {
								path = file.FilePath
								break
							}
						}
					}
				}
				if validRemoteArtwork(path) {
					add(item.kind, "landscape\x00"+path, frontendArtworkEntry{remotePath: path})
				}
			}
			continue
		}
		path, _, err := s.ArtPath(ctx, req, item.element)
		generated := req.Item.Kind() == core.KindSmart && (item.element == "thumb" || item.element == "poster")
		version, kind := "", ""
		switch item.kind {
		case frontend.ImagePoster, frontend.ImageThumb:
			version = selected.PosterVersion
		case frontend.ImageBackdrop:
			version = selected.BackdropVersion
			kind = "backdrop"
		case frontend.ImageLogo:
			version = selected.LogoVersion
			kind = "logo"
		}
		if version != "" {
			add(item.kind, discoveryIdentity(req.Item, version, kind, req.Device.Language), frontendArtworkEntry{discovery: true, discoveryKind: kind})
			continue
		}
		if (err != nil || path == "") && !generated {
			if p, ok := a.generatedPoster(title, virtualSeason, item.kind); ok {
				add(item.kind, "generated\x00"+p.Stamp(), frontendArtworkEntry{generated: &p})
			}
			continue
		}
		add(item.kind, item.element+"\x00"+path+"\x00"+collectionVersion, frontendArtworkEntry{element: item.element, immutable: title != nil && artwork.ValidPath(path)})
	}
	complete := true
	if title != nil {
		if s.Palettes != nil && ultraBlurType(title.Type) && title.BackdropPath != "" && frontend.ArtworkKindWanted(ctx, frontend.ImageBackdrop) && (!primaryOnly || !haveKind[frontend.ImageBackdrop]) {
			if colors := s.Palettes.Lookup(title.Key, title.BackdropPath); colors != nil {
				stamp := colors.TopLeft + colors.TopRight + colors.BottomLeft + colors.BottomRight
				add(frontend.ImageBackdrop, "ultrablur\x00"+title.BackdropPath+"\x00"+stamp, frontendArtworkEntry{ultra: colors})
			}
		}
		if validRemoteArtwork(title.StillPath) {
			add(frontend.ImageStill, "still\x00"+title.StillPath, frontendArtworkEntry{remotePath: title.StillPath})
		}
		if virtualSeason == nil && a.Variants != nil {
			needVariants := !primaryOnly
			if primaryOnly {
				for _, k := range []string{frontend.ImagePoster, frontend.ImageBackdrop, frontend.ImageLogo, frontend.ImageStill} {
					if frontend.ArtworkKindWanted(ctx, k) && !haveKind[k] {
						needVariants = true
						break
					}
				}
			}
			if needVariants {
				variants, ready := a.Variants(title, req.Device.Language)
				if ready && variants != nil {
					for _, group := range []struct {
						kind  string
						files []frontend.ArtworkImageFile
					}{{frontend.ImagePoster, variants.Posters}, {frontend.ImageBackdrop, variants.Backdrops}, {frontend.ImageLogo, variants.Logos}, {frontend.ImageStill, variants.Still}} {
						if !frontend.ArtworkKindWanted(ctx, group.kind) || (primaryOnly && haveKind[group.kind]) {
							continue
						}
						for _, file := range group.files {
							if validRemoteArtwork(file.FilePath) {
								add(group.kind, "variant\x00"+file.FilePath, frontendArtworkEntry{remotePath: file.FilePath})
								if primaryOnly {
									break
								}
							}
						}
					}
				}
			}
		}
		if frontend.ArtworkKindWanted(ctx, frontend.ImagePerson) {
			for _, portrait := range creditedPortraits(*title) {
				add(frontend.ImagePerson, portrait.source, frontendArtworkEntry{
					personKind: portrait.role,
					personID:   base64.RawURLEncoding.EncodeToString([]byte(portrait.thumb)),
					subject:    portrait.subject,
				})
			}
		}
	}
	return entries, complete, nil
}

func firstNonEmptyPath(paths ...string) string {
	for _, p := range paths {
		if trimmed := strings.TrimSpace(p); trimmed != "" {
			return trimmed
		}
	}
	return ""
}

func (a Source) pluginEntries(ctx context.Context, req frontend.ArtworkRequest) ([]frontendArtworkEntry, bool, error) {
	if a.PluginItem == nil {
		return nil, false, core.ErrNotFound
	}
	title, rawImages, err := a.PluginItem(ctx, req.Viewer, req.Device, req.Item)
	if err != nil || title == nil || title.Key != req.Item {
		if err == nil {
			err = core.ErrNotFound
		}
		return nil, false, err
	}
	byKind := make(map[string]frontend.Image, len(rawImages))
	for _, img := range rawImages {
		p := strings.TrimSpace(img.Path)
		if p == "" {
			continue
		}
		kind := img.Kind
		if kind == "" {
			kind = frontend.ImagePoster
		}
		if _, exists := byKind[kind]; !exists {
			img.Kind = kind
			img.Path = p
			byKind[kind] = img
		}
	}
	var entries []frontendArtworkEntry
	posterGen := a.PosterGeneration()
	add := func(kind, source string, entry frontendArtworkEntry) {
		if !frontend.ArtworkKindWanted(ctx, kind) {
			return
		}
		if posterGen > 0 && entry.ownerKind != "" {
			source += "\x00pgen:" + strconv.FormatUint(posterGen, 10)
		}
		identityReq := req
		if a.LandscapeThumb {
			viewer := *req.Viewer
			viewer.ID = 0
			identityReq.Viewer = &viewer
		}
		ref := artworkReference(identityReq, kind, source)
		entry.image = frontend.Image{
			Kind:    kind,
			Path:    ref,
			Tag:     ref,
			Subject: entry.subject,
			Width:   entry.image.Width,
			Height:  entry.image.Height,
		}
		entries = append(entries, entry)
	}
	emittedPrimary := make(map[string]string, 6)
	for _, item := range []struct{ kind, element string }{
		{frontend.ImagePoster, "poster"},
		{frontend.ImageThumb, "thumb"},
		{frontend.ImageBackdrop, "art"},
		{frontend.ImageLogo, "logo"},
		{frontend.ImageBanner, "banner"},
	} {
		if !frontend.ArtworkKindWanted(ctx, item.kind) {
			continue
		}
		ownerKind := item.kind
		if ownerKind == frontend.ImageThumb {
			ownerKind = frontend.ImagePoster
		}
		if _, stamp := a.ownerImage(ctx, req.Item, ownerKind); stamp != "" {
			add(item.kind, stamp, frontendArtworkEntry{ownerKind: ownerKind})
			continue
		}
		if a.LandscapeThumb && item.kind == frontend.ImageThumb {
			raw := byKind[frontend.ImageBackdrop]
			path := firstNonEmptyPath(raw.Path, title.BackdropPath)
			if path == "" {
				raw = byKind[frontend.ImageThumb]
				path = raw.Path
			}
			if path != "" {
				emittedPrimary[item.kind] = path
				add(item.kind, "landscape\x00"+path, frontendArtworkEntry{
					image:      frontend.Image{Width: raw.Width, Height: raw.Height},
					remotePath: path,
				})
			}
			continue
		}
		var raw frontend.Image
		var path string
		switch item.kind {
		case frontend.ImagePoster:
			raw = byKind[frontend.ImagePoster]
			if title.LandscapeCard && frontend.ArtworkKindWanted(ctx, frontend.ImageThumb) {
				path = raw.Path
			} else {
				path = firstNonEmptyPath(raw.Path, title.PosterPath, byKind[frontend.ImageThumb].Path, title.StillPath)
				if raw.Path == "" {
					raw = byKind[frontend.ImageThumb]
				}
			}
		case frontend.ImageThumb:
			raw = byKind[frontend.ImageThumb]
			if title.LandscapeCard {
				path = firstNonEmptyPath(raw.Path, title.ThumbPath, byKind[frontend.ImageBackdrop].Path, title.BackdropPath)
				if raw.Path == "" {
					raw = byKind[frontend.ImageBackdrop]
				}
			} else if title.Type == core.TypeEpisode {
				path = firstNonEmptyPath(raw.Path, title.StillPath, byKind[frontend.ImagePoster].Path, title.PosterPath)
				if raw.Path == "" {
					raw = byKind[frontend.ImagePoster]
				}
			} else {
				path = firstNonEmptyPath(raw.Path, byKind[frontend.ImagePoster].Path, title.PosterPath)
				if raw.Path == "" {
					raw = byKind[frontend.ImagePoster]
				}
			}
		case frontend.ImageBackdrop:
			raw = byKind[frontend.ImageBackdrop]
			path = firstNonEmptyPath(raw.Path, title.BackdropPath)
		case frontend.ImageLogo:
			raw = byKind[frontend.ImageLogo]
			path = firstNonEmptyPath(raw.Path, title.LogoPath)
		case frontend.ImageBanner:
			raw = byKind[frontend.ImageBanner]
			path = raw.Path
		}
		if path == "" {
			if p, ok := a.generatedPoster(title, nil, item.kind); ok {
				add(item.kind, "generated\x00"+p.Stamp(), frontendArtworkEntry{generated: &p})
			}
			continue
		}
		emittedPrimary[item.kind] = path
		add(item.kind, "plugin\x00"+item.element+"\x00"+path, frontendArtworkEntry{
			image:      frontend.Image{Width: raw.Width, Height: raw.Height},
			remotePath: path,
		})
	}
	stillRaw := byKind[frontend.ImageStill]
	if stillPath := firstNonEmptyPath(stillRaw.Path, title.StillPath); stillPath != "" {
		emittedPrimary[frontend.ImageStill] = stillPath
		add(frontend.ImageStill, "still\x00"+stillPath, frontendArtworkEntry{
			image:      frontend.Image{Width: stillRaw.Width, Height: stillRaw.Height},
			remotePath: stillPath,
		})
	}
	for _, img := range rawImages {
		p := strings.TrimSpace(img.Path)
		if p == "" {
			continue
		}
		kind := img.Kind
		if kind == "" {
			kind = frontend.ImagePoster
		}
		if emittedPrimary[kind] == p {
			continue
		}
		add(kind, "plugin-extra\x00"+kind+"\x00"+p, frontendArtworkEntry{
			image:      frontend.Image{Width: img.Width, Height: img.Height},
			remotePath: p,
		})
	}
	for _, portrait := range creditedPortraits(*title) {
		add(frontend.ImagePerson, portrait.source, frontendArtworkEntry{
			personKind: portrait.role,
			personID:   base64.RawURLEncoding.EncodeToString([]byte(portrait.thumb)),
			subject:    portrait.subject,
		})
	}
	return entries, true, nil
}

// Warm schedules the cache-only variant source for a visible row or grid item.
func (a Source) Warm(title core.Title, device core.Device) {
	if a.Palettes != nil && ultraBlurType(title.Type) && title.BackdropPath != "" {
		_ = a.Palettes.Lookup(title.Key, title.BackdropPath)
	}
	if a.Variants != nil {
		_, _ = a.Variants(&title, device.Language)
	}
}

// BindMetadata keeps the queue so fixture ingest can store hashes before a page is served.
func (a *Source) BindMetadata(q *MetadataQueue) {
	if a != nil {
		a.meta = q
	}
}

// StoreMetadata hashes each title's images and waits until those rows are stored.
// A later page joins the rows and does not wait on the hash.
func (a *Source) StoreMetadata(ctx context.Context, viewer *core.User, titles []core.Title) error {
	if a == nil || a.meta == nil || viewer == nil {
		return nil
	}
	// The queue holds a bounded number of jobs. Repeat until each title's
	// images have been stored; a stored row returns without new work.
	for pass := 0; pass < 4; pass++ {
		for _, title := range titles {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			_, _, _ = a.List(ctx, frontend.ArtworkRequest{Viewer: viewer, Item: title.Key})
		}
		if err := a.meta.WaitIdle(ctx); err != nil {
			return err
		}
	}
	return nil
}

// ListPage lists artwork for titles already loaded by the caller. Overlay
// settings, saved variants, cached ratings, and viewer overlay facts are read
// once for the page.
func (a Source) ListPage(ctx context.Context, viewer *core.Viewer, device core.Device, titles []core.Title) (map[core.RatingKey][]frontend.Image, error) {
	if viewer == nil {
		return nil, core.ErrUnauthorized
	}
	if !reqmemo.Active(ctx) {
		ctx = reqmemo.With(ctx)
	}
	pageViewer := viewer
	if policy, ok := a.Visibility.Policy.(core.UserPolicySource); ok {
		resolved := policy.UserPolicy(viewer)
		if resolved == nil || resolved.ID != viewer.ID {
			return nil, core.ErrUnauthorized
		}
		pageViewer = resolved
	}
	ctx = withVisMemo(ctx)
	memo, _ := ctx.Value(visMemoKey{}).(*visMemo)
	if memo != nil {
		memo.mu.Lock()
		if memo.seen == nil {
			memo.seen = map[core.RatingKey]bool{}
			memo.val = map[core.RatingKey]bool{}
		}
		for i := range titles {
			memo.seen[titles[i].Key] = true
			memo.val[titles[i].Key] = true
		}
		memo.mu.Unlock()
	}
	var catalogTitles []core.Title
	for i := range titles {
		if !titles[i].Key.IsPluginKey() {
			catalogTitles = append(catalogTitles, titles[i])
		}
	}
	if len(catalogTitles) > 0 && a.Discovery != nil && a.Discovery.Overlays != nil {
		next, err := a.Discovery.Overlays.WithTitlePage(ctx, pageViewer, catalogTitles)
		if err != nil {
			return nil, err
		}
		ctx = next
	}
	// An episode or season missing a wanted image falls back to its
	// series' (ArtPath). Read those series in one batch rather than one
	// title per series as each row is listed.
	if len(catalogTitles) > 0 {
		if pre, ok := a.Catalog.(interface {
			PreloadStored(context.Context, []core.RatingKey)
		}); ok {
			if parents := fallbackParents(ctx, catalogTitles); len(parents) > 0 {
				pre.PreloadStored(ctx, parents)
			}
		}
	}
	type listedTitle struct {
		idx     int
		req     frontend.ArtworkRequest
		entries []frontendArtworkEntry
	}
	listed := make([]listedTitle, 0, len(titles))
	withMetadata := a.Metadata != nil && frontend.ArtworkMetadataWanted(ctx)
	var lookups []artworkMetadataLookup
	for i := range titles {
		req := frontend.ArtworkRequest{Viewer: viewer, Device: device, Item: titles[i].Key}
		entries, _, err := a.entries(frontend.WithArtworkTitle(ctx, &titles[i]), req)
		if errors.Is(err, core.ErrNotFound) {
			continue
		}
		if err != nil {
			return nil, err
		}
		listed = append(listed, listedTitle{idx: i, req: req, entries: entries})
		if withMetadata && a.meta != nil {
			for _, e := range entries {
				lookups = append(lookups, artworkMetadataLookup{request: req, image: e.image})
			}
		}
	}
	if withMetadata && a.meta != nil && len(lookups) > 0 {
		ctx = a.meta.Preload(ctx, lookups)
	}
	out := make(map[core.RatingKey][]frontend.Image, len(listed))
	for _, item := range listed {
		titleCtx := frontend.WithArtworkTitle(ctx, &titles[item.idx])
		images := make([]frontend.Image, 0, len(item.entries))
		for _, e := range item.entries {
			img := e.image
			if withMetadata {
				img = a.Metadata(titleCtx, item.req, img)
			}
			images = append(images, img)
		}
		out[item.req.Item] = images
	}
	return out, nil
}

// ListCollectionPage builds images from policy-checked collection headers and
// preloads metadata for every image on the page together.
func (a Source) ListCollectionPage(ctx context.Context, viewer *core.Viewer, device core.Device, cols []core.Collection) (map[core.RatingKey][]frontend.Image, error) {
	posters, err := a.collectionPagePosters(ctx, viewer, cols)
	if err != nil {
		return nil, err
	}
	type listedCollection struct {
		req     frontend.ArtworkRequest
		ctx     context.Context
		entries []frontendArtworkEntry
	}
	withMetadata := !frontend.CollectionCardArtwork(ctx) && a.Metadata != nil && frontend.ArtworkMetadataWanted(ctx)
	listed := make([]listedCollection, 0, len(cols))
	var lookups []artworkMetadataLookup
	for i := range cols {
		c := &cols[i]
		req := frontend.ArtworkRequest{Viewer: viewer, Device: device, Item: c.Key}
		listCtx := ctx
		if poster, ok := posters[c.Key]; ok {
			withPoster := *c
			withPoster.PosterPath = poster
			listCtx = frontend.WithArtworkCollection(ctx, &withPoster)
		} else if frontend.CollectionCardArtwork(ctx) || c.Key.Kind() == core.KindCollection || (!c.Upcoming && c.PosterPath != "") {
			listCtx = frontend.WithArtworkCollection(ctx, c)
		}
		entries, _, err := a.entries(listCtx, req)
		if errors.Is(err, core.ErrNotFound) || errors.Is(err, core.ErrUnauthorized) {
			continue
		}
		if err != nil {
			return nil, err
		}
		listed = append(listed, listedCollection{req: req, ctx: listCtx, entries: entries})
		if withMetadata && a.meta != nil {
			for _, e := range entries {
				lookups = append(lookups, artworkMetadataLookup{request: req, image: e.image})
			}
		}
	}
	if len(lookups) > 0 {
		ctx = a.meta.Preload(ctx, lookups)
	}
	out := make(map[core.RatingKey][]frontend.Image, len(listed))
	for _, item := range listed {
		images := make([]frontend.Image, 0, len(item.entries))
		for _, e := range item.entries {
			img := e.image
			if withMetadata {
				metadataCtx := ctx
				if col, ok := frontend.ArtworkCollection(item.ctx, item.req.Item); ok {
					metadataCtx = frontend.WithArtworkCollection(ctx, col)
				}
				img = a.Metadata(metadataCtx, item.req, img)
			}
			images = append(images, img)
		}
		out[item.req.Item] = images
	}
	return out, nil
}

// collectionPagePosters mirrors the single-image fallback for a whole page,
// checking title policy and availability once for the combined candidates.
func (a Source) collectionPagePosters(ctx context.Context, viewer *core.Viewer, cols []core.Collection) (map[core.RatingKey]string, error) {
	batch, ok := a.Collections.(interface {
		CollectionPosterCandidates(context.Context, *core.User, []core.RatingKey) (map[core.RatingKey][]core.Title, error)
	})
	if !ok || viewer == nil || frontend.CollectionCardArtwork(ctx) {
		return nil, nil
	}
	var keys []core.RatingKey
	for _, c := range cols {
		if c.Key.Kind() == core.KindSmart && !c.Upcoming && c.PosterPath == "" {
			keys = append(keys, c.Key)
		}
	}
	if len(keys) == 0 {
		return nil, nil
	}
	if policy, ok := a.Visibility.Policy.(core.UserPolicySource); ok {
		current := policy.UserPolicy(viewer)
		if current == nil || current.ID != viewer.ID {
			return nil, nil
		}
		viewer = current
	}
	ctx = core.WithCatalogViewer(ctx, viewer)
	candidates, err := batch.CollectionPosterCandidates(ctx, viewer, keys)
	if err != nil {
		return nil, err
	}
	var titles []core.Title
	seen := make(map[core.RatingKey]bool)
	for _, key := range keys {
		for _, title := range candidates[key] {
			if !seen[title.Key] && a.Visibility.TitleAllowed(viewer, &title) {
				seen[title.Key] = true
				titles = append(titles, title)
			}
		}
	}
	visible := make(map[core.RatingKey]bool)
	if len(titles) > 0 {
		vis := a.Visibility
		vis.GetTitle = a.title
		for _, title := range vis.Available(ctx, viewer, titles) {
			visible[title.Key] = vis.Playable(ctx, viewer, &title)
		}
	}
	out := make(map[core.RatingKey]string, len(candidates))
	for key, titles := range candidates {
		out[key] = ""
		for _, title := range titles {
			if visible[title.Key] {
				out[key] = title.PosterPath
				break
			}
		}
	}
	return out, nil
}

func (a Source) List(ctx context.Context, req frontend.ArtworkRequest) ([]frontend.Image, bool, error) {
	entries, complete, err := a.entries(ctx, req)
	if err != nil {
		return nil, false, err
	}
	withMetadata := !frontend.CollectionCardArtwork(ctx) && a.Metadata != nil && frontend.ArtworkMetadataWanted(ctx)
	if withMetadata && a.meta != nil && len(entries) > 1 {
		lookups := make([]artworkMetadataLookup, len(entries))
		for i, e := range entries {
			lookups[i] = artworkMetadataLookup{request: req, image: e.image}
		}
		ctx = a.meta.Preload(ctx, lookups)
	}
	out := make([]frontend.Image, 0, len(entries))
	for _, e := range entries {
		img := e.image
		if withMetadata {
			img = a.Metadata(ctx, req, img)
		}
		out = append(out, img)
	}
	return out, complete, nil
}

var _ frontend.Artwork = Source{}

func (a Source) generatedPoster(t, virtualSeason *core.Title, kind string) (artwork.TitlePoster, bool) {
	if a.Generated == nil || virtualSeason != nil || (kind != frontend.ImagePoster && kind != frontend.ImageThumb) {
		return artwork.TitlePoster{}, false
	}
	return artwork.ForTitle(t)
}

// undatedSearchHit reports a long-tail search hit whose release date is not
// known yet (catalog.UndatedSearchHit): search shows it, so its artwork is
// served although it is not known to be released.
func (a Source) undatedSearchHit(ctx context.Context, key core.RatingKey) bool {
	u, ok := a.Catalog.(interface {
		UndatedSearchHit(context.Context, core.RatingKey) bool
	})
	return ok && u.UndatedSearchHit(ctx, key)
}

// fallbackParents lists the series ArtPath reads for titles: those of
// episodes and seasons without their own image of a wanted kind.
func fallbackParents(ctx context.Context, titles []core.Title) []core.RatingKey {
	poster := frontend.ArtworkKindWanted(ctx, frontend.ImagePoster) || frontend.ArtworkKindWanted(ctx, frontend.ImageThumb)
	backdrop := frontend.ArtworkKindWanted(ctx, frontend.ImageBackdrop)
	logo := frontend.ArtworkKindWanted(ctx, frontend.ImageLogo)
	var out []core.RatingKey
	for i := range titles {
		t := &titles[i]
		if t.Key.IsPluginKey() || (t.Type != core.TypeEpisode && t.Type != core.TypeSeason) {
			continue
		}
		ownPoster := t.PosterPath != "" || (t.Type == core.TypeEpisode && t.StillPath != "")
		if (!poster || ownPoster) && (!backdrop || t.BackdropPath != "") && (!logo || t.LogoPath != "") {
			continue
		}
		pk := t.ShowKey
		if pk == 0 {
			pk = t.Key.Parent()
			if t.Type == core.TypeEpisode {
				pk = pk.Parent()
			}
		}
		if pk != 0 && !pk.IsPluginKey() {
			out = append(out, pk)
		}
	}
	return out
}

// WithLandscapeThumb returns a scoped copy with the adapter-selected thumb shape.
func (a Source) WithLandscapeThumb(landscape bool) frontend.Artwork {
	a.LandscapeThumb = landscape
	return &a
}

// WithPluginItem returns a scoped copy with a surface-bound plugin item lookup.
func (a Source) WithPluginItem(fn func(context.Context, *core.User, core.Device, core.RatingKey) (*core.Title, []frontend.Image, error)) frontend.Artwork {
	a.PluginItem = fn
	return &a
}
