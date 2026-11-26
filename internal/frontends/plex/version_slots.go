package plex

import (
	"context"
	"strings"
	"time"

	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontends/plex/model"
)

func versionIdentity(v frontend.ReleaseVariant) string {
	if v.ID != "" {
		return v.ID
	}
	return v.Release.ID
}

type slotMediaRender func(map[frontend.VariantRef]int64) []model.Media

// slotMediaPage collects the exact pairs used by a rendered page, then
// finishes each row with the durable IDs assigned in that batch.
type slotMediaPage struct {
	refs   []frontend.VariantRef
	finish []func(map[frontend.VariantRef]int64)
}

func (s *Server) finishSlotPage(ctx context.Context, pr *Request, page *slotMediaPage) {
	if page == nil || len(page.refs) == 0 || pr.versionErr != nil {
		return
	}
	ids, err := s.variantIDsFor(ctx, page.refs)
	if err != nil {
		pr.versionErr = err
		return
	}
	for _, finish := range page.finish {
		finish(ids)
	}
}

func slotRef(t *frontend.Title, v frontend.ReleaseVariant) frontend.VariantRef {
	return frontend.VariantRef{Key: t.Key, ReleaseID: versionIdentity(v)}
}

func (s *Server) variantRefs(t *frontend.Title, versions []frontend.ReleaseVariant) []frontend.VariantRef {
	var refs []frontend.VariantRef
	if s.variantIDs != nil {
		for _, v := range versions {
			if !v.Auto {
				refs = append(refs, slotRef(t, v))
			}
		}
	}
	return refs
}

func (s *Server) variantMediaID(t *frontend.Title, v frontend.ReleaseVariant, ids map[frontend.VariantRef]int64) int64 {
	if v.Auto || s.variantIDs == nil {
		return int64(t.Key)
	}
	return ids[slotRef(t, v)]
}

func (s *Server) variantIDsFor(ctx context.Context, refs []frontend.VariantRef) (map[frontend.VariantRef]int64, error) {
	if len(refs) == 0 {
		return nil, nil
	}
	return s.variantIDs.shared.IDsFor(ctx, refs)
}

// slotMedia is the single projection for configured menus, including hubs.
// Settings choose the identities; cache/probe state can change their labels
// and stream facts but never the shape. No release search occurs here.
func (s *Server) slotMedia(ctx context.Context, pr *Request, t *frontend.Title, versions []frontend.ReleaseVariant, ids map[frontend.VariantRef]int64, info frontend.MediaInfo, ps *frontend.PlaySession, resumed resumedVersionResult, stamp int64, streams, batchDetail bool, selectedChapterMedia *frontend.MediaInfo) []model.Media {
	ik := s.playIdentityFor(pr, t.Key)
	now := s.now()
	issuePlayID := !batchDetail || ps != nil || s.playIDs.has(ik)
	streamIK := ik
	if !issuePlayID {
		streamIK = playIdentityKey{}
	}
	var pin pinnedVariants
	if issuePlayID {
		pin = s.playIDs.pinned(ik, ps != nil, now)
	}
	selected := pin.selected
	var (
		resumedInfo     = resumed.info
		resumedRel      = resumed.rel
		resumedSelector = resumed.selector
		hasResumed      = resumed.ok
		slotInfo        = info
		infoTarget      int64
	)
	if ps != nil {
		if selected == 0 {
			selected = int64(t.Key)
			if ps.Resolution != nil {
				for _, v := range versions {
					if (ps.Resolution.Release.ID != "" && v.Release.ID == ps.Resolution.Release.ID) ||
						(!frontend.IsAutoVersionSelector(ps.Resolution.VersionSelector) && v.ID == ps.Resolution.VersionSelector) {
						selected = s.variantMediaID(t, v, ids)
						break
					}
				}
			}
		}
		infoTarget = selected
	} else {
		if hasResumed {
			slotInfo = resumedInfo
			explicitSlot := frontend.IsExplicitVersionSlotSelector(resumedSelector)
			if len(versions) > 0 && versions[0].Remembered && (versions[0].Release.ID == resumedRel.ID || !explicitSlot) {
				infoTarget = int64(t.Key)
			} else {
				for _, v := range versions {
					if resumedRel.ID != "" && v.Release.ID == resumedRel.ID {
						infoTarget = s.variantMediaID(t, v, ids)
						break
					}
				}
				if infoTarget == 0 && explicitSlot {
					for _, v := range versions {
						if v.ID == resumedSelector {
							infoTarget = s.variantMediaID(t, v, ids)
							break
						}
					}
				}
				if infoTarget == 0 && !explicitSlot {
					infoTarget = int64(t.Key)
				}
			}
		} else if !info.Estimated && info.HasPlayableMedia() {
			infoTarget = int64(t.Key)
		}
		if selected == 0 {
			if infoTarget != 0 && !(len(versions) > 0 && versions[0].Remembered) {
				selected = infoTarget
			} else {
				selected = int64(t.Key)
			}
		}
	}
	out := make([]model.Media, 0, len(versions))
	caps := s.clientCapsForKey(pr, t.Key)
	stored := selection{}
	if streams {
		stored = s.selections.get(pr.User.ID, int64(t.Key))
	}
	for _, v := range versions {
		id := s.variantMediaID(t, v, ids)
		claimed := v.Info
		appliedSlotProbe := false
		// A stopped session's persisted probe belongs only to the version
		// that played; never stamp an explicit non-Auto pick onto Auto.
		if id == infoTarget && (ps != nil || (!slotInfo.Estimated && slotInfo.HasPlayableMedia())) {
			v.Info = slotInfo
			appliedSlotProbe = !slotInfo.Estimated && slotInfo.Video() != nil
			if ps != nil && ps.Resolution != nil && (ps.Resolution.Release.Name != "" || ps.Resolution.Release.Info.Height > 0) {
				v.Release = ps.Resolution.Release
			} else if ps == nil && hasResumed && (resumedRel.Name != "" || resumedRel.Info.Height > 0) {
				if v.Release.ID == "" || strings.HasPrefix(v.Release.ID, frontend.VersionSelectorPrefix) || v.Release.ID == resumedRel.ID {
					v.Release = resumedRel
				}
			}
		} else if ps == nil && v.Auto && v.Info.Estimated && infoTarget != int64(t.Key) {
			if wp, ok := s.playback().(frontend.WarmProber); ok && pr != nil && pr.User != nil {
				if warm, ok := wp.WarmProbeInfo(pr.User, caps, t.Key); ok && !warm.Estimated && warm.HasPlayableMedia() {
					v.Info = warm
					appliedSlotProbe = true
				}
			}
		}
		if !v.Info.Estimated && v.Info.Video() != nil {
			if v.Info.Size > 0 && v.Release.Size <= 0 {
				v.Release.Size = v.Info.Size
			}
			bareLabel := v.Label == "" || !strings.Contains(v.Label, " · ")
			if appliedSlotProbe || bareLabel || probeDisagrees(claimed, v.Info) {
				v.Label = probedVariantTitle(v.Auto, v.Release, claimed, v.Info)
			}
		}
		v.Info = variantInfoForClient(v.Info, caps)
		m := buildMediaVariant(t, v, stamp, id)
		if !streams && v.Info.Estimated {
			// A list row: a picked release reports what its name and size
			// claim; a cold slot (no release yet) claims nothing.
			var claim *frontend.Release
			if v.Release.ID != "" && !strings.HasPrefix(v.Release.ID, frontend.VersionSelectorPrefix) {
				claim = &v.Release
			}
			unprobedListMedia(&m, t, claim)
		}
		if streams {
			if issuePlayID {
				m.Part[0].Key = s.playIDs.partKey(ik, m.ID, m.Part[0].Key, now)
			}
			facts := s.withCachedSidecars(ctx, pr.User, t, v.Info)
			sel := s.preferredSelection(ctx, pr.User, t.Key, facts, t.Language, resolveSelection(stored, facts))
			m.Part[0].Stream = s.issuedStreams(t.Key, streamIK, m.ID, facts, sel, pr.User, pr)
		}
		if id == selected {
			m.Selected = model.Ptr(true)
			m.Part[0].Selected = model.Ptr(true)
			if selectedChapterMedia != nil && !v.Info.Estimated && len(v.Info.Chapters) > 0 {
				*selectedChapterMedia = v.Info
			}
		}
		out = append(out, m)
	}
	// A settings list is issued only by detail/queue. Hubs and batch lookups
	// project the same configured slots without creating per-client flow rows.
	if streams && issuePlayID {
		s.playIDs.issueConfiguredSlots(ik, versions, now)
	}
	return out
}

// Configured slots are not discovery snapshots. Their stable identities come
// from settings, while labels and current source facts may be refreshed.
func (p *playIdentities) issueConfiguredSlots(k playIdentityKey, vs []frontend.ReleaseVariant, now time.Time) {
	if !k.valid() {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	e := p.entryLocked(k, now)
	if e == nil {
		return
	}
	e.variants = cloneVariants(vs)
	e.resolved = true
	e.at = now
}
