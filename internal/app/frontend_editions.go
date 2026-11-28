package app

import (
	"context"
	"path/filepath"
	"strings"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
)

// surfaceEditions exposes probed local copies and release variants.
type surfaceEditions struct {
	app     *App
	surface string
}

func (s surfaceEditions) Editions(ctx context.Context, _ *core.Viewer, _ core.Device, key core.RatingKey) ([]frontend.Version, error) {
	if !localEditionsEnabled || s.app == nil {
		return nil, nil
	}
	title, err := s.app.catalog.CachedGet(ctx, key)
	if err != nil || title == nil {
		return nil, nil
	}
	variants, err := localEditionVariants(s.app.opts.DataDir, title)
	if err != nil || len(variants) == 0 {
		return nil, err
	}
	out := make([]frontend.Version, 0, len(variants))
	for _, variant := range variants {
		version := frontendVersion(title, variant)
		if name := probedFileName(variant.Label); name != "" {
			version.FileName = name
		}
		out = append(out, version)
	}
	return out, nil
}

// probedFileName accepts a local edition label only when it is the probed
// file's basename. Release titles and indexer names are not file names.
func probedFileName(label string) string {
	if label == "" || strings.ContainsAny(label, `/\`) {
		return ""
	}
	switch strings.ToLower(strings.TrimPrefix(filepath.Ext(label), ".")) {
	case "mkv", "mp4", "m4v", "avi", "mov", "wmv", "webm", "mpg", "mpeg", "ts", "m2ts", "mpegts":
		return label
	default:
		return ""
	}
}

func (s surfaceEditions) EditionsMany(ctx context.Context, _ *core.Viewer, _ core.Device, keys []core.RatingKey) (map[core.RatingKey][]frontend.Version, error) {
	out := map[core.RatingKey][]frontend.Version{}
	if !localEditionsEnabled || s.app == nil || s.app.catalog == nil || len(keys) == 0 {
		return out, nil
	}
	titles, err := s.app.catalog.CachedGetMany(ctx, keys)
	if err != nil {
		return nil, err
	}
	list := make([]core.Title, 0, len(titles))
	for _, key := range keys {
		title, ok := titles[key]
		if ok && title.Key == key {
			list = append(list, title)
		}
	}
	variants, err := localEditionVariantsMany(s.app.opts.DataDir, list)
	if err != nil {
		return nil, err
	}
	for key, copies := range variants {
		title := titles[key]
		versions := make([]frontend.Version, 0, len(copies))
		for _, variant := range copies {
			version := frontendVersion(&title, variant)
			if name := probedFileName(variant.Label); name != "" {
				version.FileName = name
			}
			versions = append(versions, version)
		}
		out[key] = versions
	}
	return out, nil
}

func (s surfaceEditions) Variants(ctx context.Context, req core.ResolveRequest, max int) ([]core.ReleaseVariant, error) {
	if s.app == nil {
		return nil, nil
	}
	if req.Title != nil && req.Title.Key != 0 && req.Title.Key.IsPluginKey() {
		vctx := ctx
		if s.surface != "" {
			vctx = core.WithLiveApp(vctx, s.surface)
		}
		if core.IndexerPurpose(vctx) == core.IndexerPurposeOther {
			vctx = core.WithIndexerPurpose(vctx, core.IndexerPurposeDetail)
		}
		variants, err := s.app.pluginKindVersions(vctx, req.User, req.Title.Key)
		if err != nil || len(variants) == 0 {
			return nil, err
		}
		if s.app.playback != nil {
			variants = s.app.playback.WithCachedVariantInfo(ctx, req.User, req.Title, variants)
		}
		if max > 0 && len(variants) > max {
			variants = variants[:max]
		}
		return variants, nil
	}
	if s.app.resolver == nil {
		return nil, nil
	}
	req.User = s.app.withEffectiveLanguages(ctx, req.User)
	if req.PlayedRelease == "" && s.app.playback != nil && req.User != nil && req.User.ID > 0 && req.Title != nil {
		if _, played, sel, ok := s.app.playback.ResumedVersion(ctx, req.User, req.Title.Key, req.Client); ok {
			req.PlayedRelease = played.ID
			req.PlayedSelector = sel
			relCopy := played
			req.PlayedReleaseRecord = &relCopy
		}
	}
	variants, err := s.app.resolver.Variants(ctx, req, max)
	if err == nil && s.app.playback != nil {
		variants = s.app.playback.WithCachedVariantInfo(ctx, req.User, req.Title, variants)
	}
	return variants, err
}

func (s surfaceEditions) RefillSearchCache(t *core.Title) <-chan struct{} {
	if s.app == nil || s.app.resolver == nil || t == nil || t.Key.IsPluginKey() {
		return nil
	}
	return s.app.resolver.RefillSearchCache(t)
}

func (s surfaceEditions) Available(ctx context.Context, key core.RatingKey) (bool, bool) {
	if s.app == nil || s.app.resolver == nil {
		return false, false
	}
	return s.app.resolver.Available(ctx, key)
}

func (s surfaceEditions) VersionSlots(ctx context.Context, req core.ResolveRequest, max int, labels bool) ([]core.ReleaseVariant, error) {
	if s.app == nil {
		return nil, nil
	}
	// A plugin kind item's version menu is its /resolve sources (Auto +
	// remaining releases), listed through the KindResolver cache. Never call
	// plugin /resolve on list renders (labels == false); only on detail or
	// play intent (labels == true).
	if req.Title != nil && req.Title.Key != 0 && req.Title.Key.IsPluginKey() {
		if !labels {
			return nil, nil
		}
		vctx := ctx
		if s.surface != "" {
			vctx = core.WithLiveApp(vctx, s.surface)
		}
		if core.IndexerPurpose(vctx) == core.IndexerPurposeOther {
			vctx = core.WithIndexerPurpose(vctx, core.IndexerPurposeDetail)
		}
		variants, err := s.app.pluginKindVersions(vctx, req.User, req.Title.Key)
		if err != nil || len(variants) == 0 {
			return nil, err
		}
		if s.app.playback != nil {
			variants = s.app.playback.WithCachedVariantInfo(ctx, req.User, req.Title, variants)
		}
		if max > 0 && len(variants) > max {
			variants = variants[:max]
		}
		return variants, nil
	}
	if req.Title != nil {
		if vs, ok := labMediaVariants(s.app.opts.DataDir, req.Title); ok {
			for i := range vs {
				vs[i].Availability = core.VersionInstant
			}
			return vs, nil
		}
	}
	if s.app.resolver == nil {
		return nil, nil
	}
	if labels {
		req.User = s.app.withEffectiveLanguages(ctx, req.User)
		if req.PlayedRelease == "" && s.app.playback != nil && req.User != nil && req.User.ID > 0 && req.Title != nil {
			if _, played, sel, ok := s.app.playback.ResumedVersion(ctx, req.User, req.Title.Key, req.Client); ok {
				req.PlayedRelease = played.ID
				req.PlayedSelector = sel
				relCopy := played
				req.PlayedReleaseRecord = &relCopy
			}
		}
	}
	versions, err := s.app.resolver.VersionSlots(ctx, req, max, labels)
	if err == nil && labels && s.app.playback != nil {
		versions = s.app.playback.WithCachedVariantInfo(ctx, req.User, req.Title, versions)
	}
	return versions, err
}

func (s surfaceEditions) ViewerChoiceFor(ctx context.Context, viewer *core.Viewer, title *core.Title) (*core.ViewerChoiceSummary, error) {
	if s.app == nil || s.app.resolver == nil {
		return nil, nil
	}
	return s.app.resolver.ViewerChoiceSummary(ctx, viewer, title)
}

func (s surfaceEditions) ClearViewerChoice(ctx context.Context, viewer *core.Viewer, title *core.Title, scope string) (bool, error) {
	if s.app == nil || s.app.resolver == nil || viewer == nil {
		return false, nil
	}
	return s.app.resolver.ClearViewerChoice(ctx, viewer.ID, title, scope)
}
