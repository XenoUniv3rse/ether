package mediabrowser

import (
	"context"
	"encoding/json"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/kit/authhdr"
	"github.com/braedonsaunders/ether/internal/frontend/kit/ids"
	"github.com/braedonsaunders/ether/internal/frontend/kit/route"
	"net/http"
	"strconv"
	"strings"
)

type previewRowArtworkKey struct{}

func withPreviewRowArtwork(r *http.Request, enabled bool) *http.Request {
	if !enabled {
		return r
	}
	return r.WithContext(context.WithValue(r.Context(), previewRowArtworkKey{}, true))
}

func previewRowArtwork(r *http.Request, item map[string]any) {
	if len(rankedRowRanks(r)) > 0 {
		return
	}
	if enabled, _ := r.Context().Value(previewRowArtworkKey{}).(bool); !enabled {
		return
	}
	backdrops, _ := item["BackdropImageTags"].([]string)
	if len(backdrops) == 0 || backdrops[0] == "" {
		return
	}
	tags := map[string]string{}
	switch old := item["ImageTags"].(type) {
	case map[string]string:
		for k, v := range old {
			tags[k] = v
		}
	case map[string]any:
		for k, v := range old {
			if value, ok := v.(string); ok {
				tags[k] = value
			}
		}
	}
	tags["Primary"] = backdrops[0]
	item["ImageTags"] = tags
	item["PrimaryImageAspectRatio"] = float64(16) / 9
	if old, ok := item["ImageBlurHashes"].(map[string]any); ok {
		hashes := map[string]any{}
		for kind, values := range old {
			if kind != "Primary" {
				hashes[kind] = values
			}
		}
		if byTag, ok := old["Backdrop"].(map[string]string); ok && byTag[backdrops[0]] != "" {
			hashes["Primary"] = map[string]string{backdrops[0]: byTag[backdrops[0]]}
		}
		item["ImageBlurHashes"] = hashes
	}
}

type rankedRowKey struct{}

func withRankedRow(r *http.Request, enabled bool, titles []frontend.Title, offset int) *http.Request {
	if !enabled {
		return r
	}
	ranks := map[frontend.RatingKey]int{}
	for n, t := range titles {
		if offset+n < 10 {
			ranks[t.Key] = offset + n + 1
		}
	}
	return r.WithContext(context.WithValue(r.Context(), rankedRowKey{}, ranks))
}
func rankedRowRanks(r *http.Request) map[frontend.RatingKey]int {
	ranks, _ := r.Context().Value(rankedRowKey{}).(map[frontend.RatingKey]int)
	return ranks
}

func (i *instance) gridImagesFor(r *http.Request, titles []frontend.Title) map[frontend.RatingKey]map[string]any {
	return i.gridImagesWithParents(r, titles)
}

// gridListImages signs the artwork a library list carries. Like Jellyfin, a
// list carries every image type's tag unless the app narrows them with
// EnableImageTypes (or turns images off): apps build wide rows and the
// home-screen spotlight from BackdropImageTags and ImageTags.Thumb/Logo,
// and request nothing for a type whose tag is missing. Page grants are
// stateless, so the extra kinds cost response bytes, not writes.
func (i *instance) gridListImages(r *http.Request, titles []frontend.Title) map[frontend.RatingKey]map[string]any {
	kinds := listImageKinds(r)
	if kinds != nil && len(kinds) == 0 {
		return map[frontend.RatingKey]map[string]any{}
	}
	if kinds == nil {
		return i.gridImagesWithParents(r, titles)
	}
	return i.gridImageRows(r, titles, nil, kinds)
}

// Sign a page and its parent images together. Both sets use the same policy
// and all image kinds, so splitting them repeats the authorization and facts.
func (i *instance) gridImagesWithParents(r *http.Request, titles []frontend.Title) map[frontend.RatingKey]map[string]any {
	if i == nil {
		return nil
	}
	if i.protocol != "jellyfin" || r == nil {
		return i.gridImageRows(r, titles, nil, nil)
	}
	cache, _ := r.Context().Value(showArtworkCacheKey{}).(map[frontend.RatingKey]map[string]any)
	if cache == nil {
		return i.gridImageRows(r, titles, nil, nil)
	}
	batch := append([]frontend.Title(nil), titles...)
	seen := map[frontend.RatingKey]bool{}
	for _, title := range titles {
		seen[title.Key] = true
	}
	var parents []frontend.RatingKey
	for _, title := range titles {
		if title.Type != "episode" && title.Type != "season" {
			continue
		}
		show := parentShowKey(title)
		if _, ok := cache[show]; show == 0 || ok || seen[show] {
			continue
		}
		seen[show] = true
		parents = append(parents, show)
		batch = append(batch, frontend.Title{Key: show})
	}
	rows := i.gridImageRows(r, batch, nil, nil)
	for _, key := range parents {
		if art, ok := rows[key]; ok {
			cache[key] = art
		}
	}
	return rows
}

// listImageKinds maps EnableImages/EnableImageTypes to image kinds: nil is
// every kind, an empty slice is none.
func listImageKinds(r *http.Request) []string {
	if v := route.Query(r.URL.Query(), "EnableImages"); len(v) == 1 && strings.EqualFold(v[0], "false") {
		return []string{}
	}
	types := route.Query(r.URL.Query(), "EnableImageTypes")
	if len(types) == 0 {
		return nil
	}
	seen := map[string]bool{}
	var kinds []string
	add := func(k ...string) {
		for _, kind := range k {
			if !seen[kind] {
				seen[kind] = true
				kinds = append(kinds, kind)
			}
		}
	}
	for _, value := range types {
		for _, t := range strings.Split(value, ",") {
			switch strings.ToLower(strings.TrimSpace(t)) {
			case "primary":
				add(frontend.ImagePoster, frontend.ImageStill)
			case "backdrop":
				add(frontend.ImageBackdrop)
			case "thumb":
				add(frontend.ImageThumb)
			case "logo":
				add(frontend.ImageLogo)
			case "banner":
				add(frontend.ImageBanner)
			}
		}
	}
	if len(kinds) == 0 {
		// Only types Ether has no artwork for: keep the poster.
		return []string{frontend.ImagePoster, frontend.ImageStill}
	}
	return kinds
}

func (i *instance) gridImageRows(r *http.Request, titles []frontend.Title, keys []frontend.RatingKey, kinds []string) map[frontend.RatingKey]map[string]any {
	out := make(map[frontend.RatingKey]map[string]any)
	token := authhdr.MediaBrowserToken(r)
	if len(titles) > 0 {
		if signer, ok := i.host.ImageGrants.(frontend.ImagePageSigner); ok {
			for start := 0; start < len(titles); start += 50 {
				end := min(start+50, len(titles))
				rows, err := signer.SignPage(r.Context(), frontend.ImagePageRequest{Ranked: rankedRowRanks(r), Credential: token, Device: device(r), Titles: titles[start:end], Kinds: gridKinds(kinds)})
				if err != nil {
					continue
				}
				i.collectGridImages(out, rows)
			}
			return out
		}
		if len(keys) == 0 {
			keys = make([]frontend.RatingKey, len(titles))
			for n, title := range titles {
				keys[n] = title.Key
			}
		}
	}
	batch, ok := i.host.ImageGrants.(frontend.ImageGrantBatch)
	if !ok {
		return out
	}
	for start := 0; start < len(keys); start += 50 {
		end := min(start+50, len(keys))
		rows, err := batch.IssueBatch(r.Context(), frontend.ImageBatchRequest{Credential: token, Device: device(r), Items: keys[start:end], Kinds: gridKinds(kinds)})
		if err != nil {
			continue
		}
		i.collectGridImages(out, rows)
	}
	return out
}

func gridKinds(kinds []string) []string {
	if len(kinds) > 0 {
		return kinds
	}
	return []string{frontend.ImagePoster, frontend.ImageStill, frontend.ImageBackdrop, frontend.ImageLogo, frontend.ImageThumb, frontend.ImageBanner}
}

func (i *instance) collectGridImages(out map[frontend.RatingKey]map[string]any, rows []frontend.ImageBatchItem) {
	for _, row := range rows {
		if !row.Authoritative {
			continue
		}
		hasPoster := false
		for _, image := range row.Images {
			if image.Image.Kind == frontend.ImagePoster && image.Secret != "" {
				hasPoster = true
				break
			}
		}
		tags := map[string]string{}
		backdrops := []string{}
		result := map[string]any{}
		primarySelected := false
		for _, img := range row.Images {
			if img.Secret == "" {
				continue
			}
			switch img.Image.Kind {
			case frontend.ImagePoster:
				tags["Primary"] = img.Secret
				primarySelected = true
				i.addImageMetadata(result, "Primary", img.Secret, img.Image)
			case frontend.ImageStill:
				if !primarySelected && !hasPoster {
					tags["Primary"] = img.Secret
					primarySelected = true
					i.addImageMetadata(result, "Primary", img.Secret, img.Image)
				}
			case frontend.ImageBackdrop:
				backdrops = append(backdrops, img.Secret)
				i.addImageMetadata(result, "Backdrop", img.Secret, img.Image)
			case frontend.ImageLogo:
				tags["Logo"] = img.Secret
				i.addImageMetadata(result, "Logo", img.Secret, img.Image)
			case frontend.ImageThumb:
				tags["Thumb"] = img.Secret
				i.addImageMetadata(result, "Thumb", img.Secret, img.Image)
			case frontend.ImageBanner:
				tags["Banner"] = img.Secret
				i.addImageMetadata(result, "Banner", img.Secret, img.Image)
			}
		}
		if len(tags) > 0 {
			result["ImageTags"] = tags
		}
		if len(backdrops) > 0 {
			result["BackdropImageTags"] = backdrops
		}
		out[row.Item] = result
	}
}

func (i *instance) imageTags(r *http.Request, detail frontend.ItemDetail, token string) (map[string]any, []string, map[string]any) {
	tags := map[string]any{}
	backdrops := []string{}
	metadata := map[string]any{}
	if i.host.ImageGrants == nil || !detail.ImagesAuthoritative {
		return tags, backdrops, metadata
	}
	if signer, ok := i.host.ImageGrants.(frontend.ImagePageSigner); ok {
		rows, err := signer.SignPage(r.Context(), frontend.ImagePageRequest{
			Credential: token, Device: device(r), Titles: []frontend.Title{detail.Title},
			Kinds: []string{frontend.ImagePoster, frontend.ImageStill, frontend.ImageBackdrop, frontend.ImageLogo, frontend.ImageThumb, frontend.ImageBanner},
		})
		if err == nil && len(rows) > 0 && rows[0].Authoritative && rows[0].Item == detail.Title.Key {
			images := make([]frontend.Image, len(rows[0].Images))
			secrets := make([]string, len(rows[0].Images))
			for n, granted := range rows[0].Images {
				images[n] = granted.Image
				secrets[n] = granted.Secret
			}
			tags, backdrops, metadata = i.tagGrantedImages(images, secrets)
			return tags, backdrops, metadata
		}
	}
	hasPoster := false
	for _, img := range detail.Images {
		switch strings.ToLower(img.Kind) {
		case "poster", "primary":
			hasPoster = true
		}
	}
	primarySelected := false
	for _, img := range detail.Images {
		kind := ""
		switch strings.ToLower(img.Kind) {
		case "poster", "primary":
			kind = "Primary"
		case "logo":
			kind = "Logo"
		case "thumb":
			kind = "Thumb"
		case "still":
			// An episode still is the primary image when the title has no poster.
			if hasPoster || primarySelected {
				kind = "Thumb"
			} else {
				kind = "Primary"
			}
		case "banner":
			kind = "Banner"
		case "backdrop":
			kind = "Backdrop"
		}
		if kind == "" {
			continue
		}
		if _, exists := tags[kind]; exists && kind != "Backdrop" {
			continue
		}
		grant, err := i.host.ImageGrants.Issue(r.Context(), frontend.ImageGrantRequest{Credential: token, Device: device(r), Item: detail.Title.Key, Image: img})
		if err == nil && grant != "" {
			if kind == "Backdrop" {
				backdrops = append(backdrops, grant)
			} else {
				tags[kind] = grant
			}
			if kind == "Primary" {
				primarySelected = true
			}
			i.addImageMetadata(metadata, kind, grant, img)
		}
	}
	return tags, backdrops, metadata
}

func (i *instance) tagGrantedImages(images []frontend.Image, secrets []string) (map[string]any, []string, map[string]any) {
	tags := map[string]any{}
	backdrops := []string{}
	metadata := map[string]any{}
	hasPoster := false
	for _, img := range images {
		switch strings.ToLower(img.Kind) {
		case "poster", "primary":
			hasPoster = true
		}
	}
	primarySelected := false
	for n, img := range images {
		kind := ""
		switch strings.ToLower(img.Kind) {
		case "poster", "primary":
			kind = "Primary"
		case "logo":
			kind = "Logo"
		case "thumb":
			kind = "Thumb"
		case "still":
			if hasPoster || primarySelected {
				kind = "Thumb"
			} else {
				kind = "Primary"
			}
		case "banner":
			kind = "Banner"
		case "backdrop":
			kind = "Backdrop"
		}
		if kind == "" || n >= len(secrets) || secrets[n] == "" {
			continue
		}
		if _, exists := tags[kind]; exists && kind != "Backdrop" {
			continue
		}
		if kind == "Backdrop" {
			backdrops = append(backdrops, secrets[n])
		} else {
			tags[kind] = secrets[n]
		}
		if kind == "Primary" {
			primarySelected = true
		}
		i.addImageMetadata(metadata, kind, secrets[n], img)
	}
	return tags, backdrops, metadata
}

// addImageMetadata attaches facts only after the matching grant was issued.
func (i *instance) addImageMetadata(dto map[string]any, kind, tag string, img frontend.Image) {
	if tag == "" {
		return
	}
	if kind == "Primary" && img.Width > 0 && img.Height > 0 {
		dto["PrimaryImageAspectRatio"] = float64(img.Width) / float64(img.Height)
	}
	if i.protocol != "jellyfin" || img.BlurHash == "" {
		return
	}
	hashes, _ := dto["ImageBlurHashes"].(map[string]any)
	if hashes == nil {
		hashes = map[string]any{}
		dto["ImageBlurHashes"] = hashes
	}
	byTag, _ := hashes[kind].(map[string]string)
	if byTag == nil {
		byTag = map[string]string{}
		hashes[kind] = byTag
	}
	byTag[tag] = img.BlurHash
}
func (i *instance) userImage(w http.ResponseWriter, r *http.Request) {
	if i.host.ImageGrants == nil {
		mediaBrowserError(w, r, http.StatusNotFound)
		return
	}
	if raw := r.PathValue("index"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n != 0 {
			mediaBrowserError(w, r, http.StatusNotFound)
			return
		}
	}
	tags := route.Query(r.URL.Query(), "tag")
	if len(tags) > 1 {
		mediaBrowserError(w, r, http.StatusNotFound)
		return
	}
	if len(tags) == 1 && tags[0] != "" && i.serveImageTag(w, r, tags[0]) {
		return
	}
	v, token, err := i.authenticate(r)
	if err != nil || v == nil {
		mediaBrowserError(w, r, http.StatusNotFound)
		return
	}
	r = r.WithContext(context.WithValue(r.Context(), principalKey{}, principal{viewer: v, token: token}))
	if v, token, err = i.viewer(r); err != nil || v == nil {
		mediaBrowserError(w, r, http.StatusNotFound)
		return
	}
	target := r.PathValue("userId")
	if target == "" {
		if qs := route.Query(r.URL.Query(), "userId"); len(qs) == 1 {
			target = qs[0]
		} else if len(qs) > 1 {
			mediaBrowserError(w, r, http.StatusNotFound)
			return
		}
	}
	if target != "" {
		uid, err := ids.ParseGUID(ids.User, target)
		if err != nil || uid != v.ID {
			mediaBrowserError(w, r, http.StatusNotFound)
			return
		}
	}
	img, ok := frontend.ViewerAvatarImage(v)
	if !ok {
		mediaBrowserError(w, r, http.StatusNotFound)
		return
	}
	grant, err := i.host.ImageGrants.Issue(r.Context(), frontend.ImageGrantRequest{
		Credential: token,
		Device:     device(r),
		Image:      img,
	})
	if err != nil || grant == "" {
		mediaBrowserError(w, r, http.StatusNotFound)
		return
	}
	i.host.ImageGrants.Serve(w, r, grant)
}

func (i *instance) image(w http.ResponseWriter, r *http.Request) {
	if strings.EqualFold(r.PathValue("imageType"), "Chapter") || i.host.ImageGrants == nil {
		mediaBrowserError(w, r, http.StatusNotFound)
		return
	}
	tags := route.Query(r.URL.Query(), "tag")
	if len(tags) == 1 && tags[0] != "" && i.serveImageTag(w, r, tags[0]) {
		return
	}
	if len(tags) > 1 {
		mediaBrowserError(w, r, http.StatusNotFound)
		return
	}
	// Jellyfin treats tag as an optional cache-buster, and clients such as
	// Moonfin omit it (#405). Serve those requests in serveTaglessImage.
	i.serveTaglessImage(w, r)
}

// serveTaglessImage answers an image request with no tag. A tag is a
// short-lived capability minted for one viewer after that viewer was
// shown the item. Two tag-less shapes exist in the wild:
//
//   - The request carries the client's own live credential (header or
//     api_key). Resolve the item and image exactly as that viewer, then
//     mint the same scoped grant in-process and serve through it. Grant
//     Issue and Serve re-authorize against current policy, so this path
//     only returns artwork the credential holder could already have
//     obtained a tag for.
//   - The request is fully anonymous. Some clients (Moonfin on Smart
//     TVs) build image URLs with no tag and load them with a plain
//     <img>, so no credential can ever arrive; Jellyfin itself serves
//     these. Ether serves them only via servePublicImage: title artwork
//     for titles every active viewer may see. Anything hidden from
//     anyone — and every unresolvable id — stays the same 404, so the
//     anonymous path is not an oracle for restricted titles.
//
// A tag that is expired, invalid or forged falls back here once Serve
// declines it. The tag carries no authority of its own, so the request is
// authorized exactly as the same URL without a tag would be.
func (i *instance) serveTaglessImage(w http.ResponseWriter, r *http.Request) {
	wireID, imageType := r.PathValue("itemId"), r.PathValue("imageType")
	index := 0
	if raw := r.PathValue("index"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 0 {
			mediaBrowserError(w, r, http.StatusNotFound)
			return
		}
		index = n
	}
	v, token, err := i.authenticate(r)
	if err != nil || v == nil {
		i.servePublicImage(w, r, wireID, imageType, index)
		return
	}
	if frontend.IsAPIKeyCredential(token) {
		i.servePublicImage(w, r, wireID, imageType, index)
		return
	}
	r = r.WithContext(context.WithValue(r.Context(), principalKey{}, principal{viewer: v, token: token}))
	if v, token, err = i.viewer(r); err != nil || v == nil {
		mediaBrowserError(w, r, http.StatusNotFound)
		return
	}
	primaryOnly := strings.EqualFold(imageType, "Primary") && index == 0
	if _, err := ids.ParseGUID(ids.Channel, wireID); err == nil {
		if !primaryOnly || i.host.LiveTV == nil {
			mediaBrowserError(w, r, http.StatusNotFound)
			return
		}
		ch, err := i.host.LiveTV.ResolveChannelGUID(r.Context(), v, wireID)
		if err != nil || ch.ID == "" {
			mediaBrowserError(w, r, http.StatusNotFound)
			return
		}
		for _, img := range ch.Images {
			if img.Kind != frontend.ImageChannel {
				continue
			}
			grant, err := i.host.ImageGrants.Issue(r.Context(), frontend.ImageGrantRequest{Credential: token, Device: device(r), Channel: ch.ID, Image: img})
			if err == nil && grant != "" {
				i.host.ImageGrants.Serve(w, r, grant)
				return
			}
			break
		}
		mediaBrowserError(w, r, http.StatusNotFound)
		return
	}
	if viewNum, err := ids.ParseGUID(ids.View, wireID); err == nil {
		if !primaryOnly {
			mediaBrowserError(w, r, http.StatusNotFound)
			return
		}
		secret, _, ok := i.libraryCollageGrant(r, v, viewNum, wireID)
		if !ok || secret == "" {
			mediaBrowserError(w, r, http.StatusNotFound)
			return
		}
		i.host.ImageGrants.Serve(w, r, secret)
		return
	}
	item, images, portrait, err := i.imageTarget(r, v, wireID)
	if err != nil {
		mediaBrowserError(w, r, http.StatusNotFound)
		return
	}
	img, ok := taglessImage(images, imageType, index)
	if !ok && portrait && primaryOnly {
		for _, candidate := range images {
			if candidate.Kind == frontend.ImagePerson && candidate.Path != "" {
				img, ok = candidate, true
				break
			}
		}
	}
	if !ok {
		mediaBrowserError(w, r, http.StatusNotFound)
		return
	}
	grant, err := i.host.ImageGrants.Issue(r.Context(), frontend.ImageGrantRequest{Credential: token, Device: device(r), Item: item, Image: img})
	if err != nil || grant == "" {
		mediaBrowserError(w, r, http.StatusNotFound)
		return
	}
	i.host.ImageGrants.Serve(w, r, grant)
}

// imageTarget resolves the same edition and person IDs the browse DTOs emit.
// Artwork-only reads avoid detail enrichment (release lists and user state).
func (i *instance) imageTarget(r *http.Request, v *frontend.Viewer, wireID string) (frontend.RatingKey, []frontend.Image, bool, error) {
	key, err := ids.ParseRatingKeyGUID(wireID)
	if err != nil && i.host.VersionHandles != nil {
		if scope, e := i.versionScope(r, v); e == nil {
			if target, e := i.host.VersionHandles.Resolve(i.identityContext(r), scope, wireID); e == nil && target.Item != 0 {
				key, err = target.Item, nil
			}
		}
	}
	if err != nil {
		if people, ok := i.host.Experience.(frontend.PeopleReader); ok {
			person, e := people.Person(i.identityContext(r), v, device(r), wireID, frontend.PersonReadOptions{})
			if e == nil && person.ArtworkItem != 0 {
				return person.ArtworkItem, person.Images, true, nil
			}
		}
		return 0, nil, false, ids.ErrInvalid
	}
	if artwork, ok := i.host.Experience.(frontend.ItemArtwork); ok {
		images, complete, e := artwork.ItemArtwork(frontend.WithoutArtworkMetadata(i.identityContext(r)), v, device(r), key)
		if e != nil {
			return 0, nil, false, e
		}
		if !complete {
			return 0, nil, false, frontend.ErrNotFound
		}
		return key, images, false, nil
	}
	if i.host.Experience == nil {
		return 0, nil, false, frontend.ErrNotFound
	}
	detail, e := i.host.Experience.Item(frontend.WithNoContainerExpand(i.identityContext(r)), frontend.ItemRequest{Viewer: v, Device: device(r), Key: key})
	if e != nil || !detail.ImagesAuthoritative {
		return 0, nil, false, frontend.ErrNotFound
	}
	return key, detail.Images, false, nil
}

// servePublicImage serves title artwork to a fully anonymous tag-less
// request. Only plain title items are eligible (person, channel, user
// and library-view images stay grant-only), the title must be visible
// to every active viewer (frontend.PublicArtwork), and the bytes are
// served through the grant service's public path, which re-checks all
// of that before writing (frontend.PublicImageServer).
func (i *instance) servePublicImage(w http.ResponseWriter, r *http.Request, wireID, imageType string, index int) {
	public, experienceOK := i.host.Experience.(frontend.PublicArtwork)
	server, grantsOK := i.host.ImageGrants.(frontend.PublicImageServer)
	if !experienceOK || !grantsOK {
		mediaBrowserError(w, r, http.StatusNotFound)
		return
	}
	key, err := ids.ParseRatingKeyGUID(wireID)
	if err != nil {
		mediaBrowserError(w, r, http.StatusNotFound)
		return
	}
	images, _, visible, err := public.PublicArtwork(frontend.WithoutArtworkMetadata(i.identityContext(r)), device(r), key)
	if err != nil || !visible {
		mediaBrowserError(w, r, http.StatusNotFound)
		return
	}
	img, ok := taglessImage(images, imageType, index)
	if !ok {
		mediaBrowserError(w, r, http.StatusNotFound)
		return
	}
	server.ServePublic(w, r, device(r), key, img)
}

// taglessImage picks the index-th descriptor for a Jellyfin/Emby image
// type, in the same priority imageTags uses when it mints tags: a poster
// is Primary, an episode still stands in when there is no poster, and a
// still is also the Thumb fallback.
func taglessImage(images []frontend.Image, imageType string, index int) (frontend.Image, bool) {
	var kinds []string
	switch strings.ToLower(imageType) {
	case "primary":
		kinds = []string{frontend.ImagePoster, "primary", frontend.ImageStill}
	case "art", "backdrop":
		kinds = []string{frontend.ImageBackdrop}
	case "logo":
		kinds = []string{frontend.ImageLogo}
	case "thumb":
		kinds = []string{frontend.ImageThumb, frontend.ImageStill}
	case "banner":
		kinds = []string{frontend.ImageBanner}
	default:
		return frontend.Image{}, false
	}
	var candidates []frontend.Image
	for _, kind := range kinds {
		for _, img := range images {
			if strings.EqualFold(img.Kind, kind) && img.Path != "" {
				candidates = append(candidates, img)
			}
		}
	}
	if index >= len(candidates) {
		return frontend.Image{}, false
	}
	return candidates[index], true
}

// mediaBrowserError uses the ProblemDetails shape returned by Jellyfin.
func mediaBrowserError(w http.ResponseWriter, r *http.Request, status int) {
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(status)
	if r.Method != http.MethodHead {
		_ = json.NewEncoder(w).Encode(map[string]any{"type": "about:blank", "title": http.StatusText(status), "status": status})
	}
}

// itemImages advertises only artwork with the same scoped tags as the item DTO.
func (i *instance) itemImages(w http.ResponseWriter, r *http.Request) {
	v, token, err := i.viewer(r)
	if err != nil || v == nil {
		mediaBrowserError(w, r, 401)
		return
	}
	key, err := ids.ParseRatingKeyGUID(r.PathValue("itemId"))
	if err != nil && i.host.VersionHandles != nil {
		if scope, e := i.versionScope(r, v); e == nil {
			if target, e := i.host.VersionHandles.Resolve(i.identityContext(r), scope, r.PathValue("itemId")); e == nil && target.Item != 0 {
				key, err = target.Item, nil
			}
		}
	}
	if err != nil {
		mediaBrowserError(w, r, 404)
		return
	}
	if i.host.Experience == nil {
		mediaBrowserError(w, r, 503)
		return
	}
	detail, err := i.host.Experience.Item(frontend.WithNoContainerExpand(i.identityContext(r)), frontend.ItemRequest{Viewer: v, Device: device(r), Key: key})
	if err != nil {
		mediaBrowserError(w, r, 404)
		return
	}
	// Container details carry their key outside Title. Bind their images to
	// the authorized item, as the detail endpoint does.
	detail.Title.Key = key
	tags, backdrops, _ := i.imageTags(r, detail, token)
	images := []map[string]any{}
	for _, kind := range []string{"Primary", "Logo", "Thumb", "Banner"} {
		if tag, ok := tags[kind].(string); ok && tag != "" {
			images = append(images, map[string]any{"ImageType": kind, "ImageIndex": 0, "ImageTag": tag, "Size": 0, "Width": 0, "Height": 0})
		}
	}
	for index, tag := range backdrops {
		images = append(images, map[string]any{"ImageType": "Backdrop", "ImageIndex": index, "ImageTag": tag, "Size": 0, "Width": 0, "Height": 0})
	}
	for _, info := range images {
		if img, ok := taglessImage(detail.Images, info["ImageType"].(string), info["ImageIndex"].(int)); ok {
			info["Width"], info["Height"] = img.Width, img.Height
		}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(images)
}

// serveImageTag serves a tagged image request through its grant and reports
// whether the grant answered. Jellyfin treats tag as a cache-buster: clients
// cache item rows (and their tags) for hours, so an expired, invalid or
// forged tag is not a refusal. The caller then authorizes the request as
// the tag-less URL.
func (i *instance) serveImageTag(w http.ResponseWriter, r *http.Request, tag string) bool {
	miss := &grantMissWriter{ResponseWriter: w}
	i.host.ImageGrants.Serve(miss, r, tag)
	if !miss.missed {
		return true
	}
	for _, key := range []string{"Content-Type", "Content-Length", "X-Content-Type-Options", "Cache-Control"} {
		w.Header().Del(key)
	}
	return false
}

// grantMissWriter passes a grant's response through unless the grant answers
// 404, which it swallows so the caller can fall back.
type grantMissWriter struct {
	http.ResponseWriter
	missed, wrote bool
}

func (w *grantMissWriter) WriteHeader(code int) {
	if !w.wrote && code == http.StatusNotFound {
		w.missed = true
		return
	}
	w.wrote = true
	w.ResponseWriter.WriteHeader(code)
}

func (w *grantMissWriter) Write(b []byte) (int, error) {
	if w.missed {
		return len(b), nil
	}
	w.wrote = true
	return w.ResponseWriter.Write(b)
}

func (w *grantMissWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
