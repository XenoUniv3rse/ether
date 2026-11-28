//go:build !scorecard

package app

import (
	"context"
	"github.com/braedonsaunders/ether/internal/core"
)

func scorecardVariants(ctx context.Context, a *App, v *core.Viewer, t *core.Title, caps core.ClientCaps) ([]core.ReleaseVariant, error) {
	if vs, ok := labMediaVariants(a.opts.DataDir, t); ok {
		return vs, nil
	}
	req := core.ResolveRequest{Title: t, User: a.withEffectiveLanguages(ctx, v), Client: caps}
	// After a play, Auto is named after the release that played (the one a
	// resume opens), as Plex shows it — unless the viewer explicitly picked a
	// non-Auto version, which stays behind its own entry.
	if a.playback != nil && v != nil && v.ID > 0 {
		if _, played, sel, ok := a.playback.ResumedVersion(ctx, v, t.Key, caps); ok {
			req.PlayedRelease = played.ID
			req.PlayedSelector = sel
			relCopy := played
			req.PlayedReleaseRecord = &relCopy
		}
	}
	variants, err := a.resolver.Variants(ctx, req, core.MaxVariants)
	if err == nil && a.playback != nil {
		variants = a.playback.WithCachedVariantInfo(ctx, v, t, variants)
	}
	return variants, err
}
