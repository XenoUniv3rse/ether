package vela

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/kit/reqmemo"
)

// Keep wire grants comfortably inside the row cursor (10m), row visit (15m)
// and receipt (30m) lifetimes. Lazy eviction needs no idle worker.
const bootstrapAttempts = 3

// bootstrapChangedError is private: clients only see the bounded retry contract.
type bootstrapChangedError struct{}

func (*bootstrapChangedError) Error() string { return "bootstrap changed during assembly" }

const bootstrapTTL = 2 * time.Minute
const bootstrapBytes = 8 << 20

type bootstrapEntry struct {
	ready     chan struct{}
	revision  frontend.HomeRevision
	freshness frontend.PageFreshness
	expires   time.Time
	payload   json.RawMessage
	rows      []frontend.RowViewPageRequest
}

type bootstrapDeadlineKey struct{}
type bootstrapDeadline struct {
	expires time.Time
	rows    []frontend.RowViewPageRequest
	partial bool
}

func logDroppedHomeRow(r *http.Request, rowID, stage string, err error) {
	if lease, ok := r.Context().Value(bootstrapDeadlineKey{}).(*bootstrapDeadline); ok {
		lease.partial = true
	}
	// Source errors can contain provider URLs and credentials.
	slog.WarnContext(r.Context(), "Vela row omitted", "row", rowID, "stage", stage, "error_class", fmt.Sprintf("%T", err))
}

func noteBootstrapDeadline(r *http.Request, expiry time.Time) {
	if lease, ok := r.Context().Value(bootstrapDeadlineKey{}).(*bootstrapDeadline); ok && !expiry.IsZero() && expiry.Before(lease.expires) {
		lease.expires = expiry
	}
}

func noteBootstrapRow(r *http.Request, req frontend.RowViewPageRequest) {
	if lease, ok := r.Context().Value(bootstrapDeadlineKey{}).(*bootstrapDeadline); ok {
		lease.rows = append(lease.rows, req)
	}
}

func (i *instance) validateBootstrapRows(ctx context.Context, rows []frontend.RowViewPageRequest, expires time.Time) error {
	if len(rows) > 0 {
		if source, ok := i.host.Experience.(frontend.RowViewValidator); ok {
			if err := source.ValidateRowViews(ctx, rows); err != nil {
				if errors.Is(err, frontend.ErrNotFound) {
					return frontend.ErrRowViewStale
				}
				return err
			}
		}
		i.rowMu.Lock()
		defer i.rowMu.Unlock()
		for _, row := range rows {
			if _, ok := i.rowMeta[row.ID]; !ok {
				return frontend.ErrRowViewStale
			}
		}
	}
	if !time.Now().Before(expires) {
		return frontend.ErrRowViewStale
	}
	return nil
}

type homeRevisionSource interface {
	HomeRevision(context.Context, *frontend.Viewer) (frontend.HomeRevision, bool, error)
}

func (i *instance) viewerSpoilerFree(ctx context.Context, v *frontend.Viewer) (bool, error) {
	if i.host.LiveEvents == nil || v == nil {
		return false, nil
	}
	sf, err := i.host.LiveEvents.SpoilerFree(ctx, v)
	if errors.Is(err, frontend.ErrNotAvailable) {
		return false, nil
	}
	return sf, err
}

func (i *instance) bootstrap(w http.ResponseWriter, r *http.Request) {
	if local, ok := i.host.Experience.(interface {
		NoProviderWait(context.Context) context.Context
	}); ok {
		r = r.WithContext(local.NoProviderWait(r.Context()))
	}
	source, hasRevision := i.host.Experience.(homeRevisionSource)
	refresh := false
	request := r
	for attempt := 0; attempt < bootstrapAttempts && request.Context().Err() == nil; attempt++ {
		// A rejected visit's authorization and catalogue reads must not
		// survive into the replacement visit.
		ctx := frontend.WithFreshRequestMemo(reqmemo.With(request.Context()))
		r := request.WithContext(ctx)
		v, token, err := i.viewer(r)
		if err != nil {
			fail(w, err, r)
			return
		}
		downloads := i.downloadsFeature(r.Context(), v)
		r = r.WithContext(context.WithValue(r.Context(), downloadCapabilityKey{}, downloads))
		dev := requestDevice(r)
		avatar := i.viewerAvatar(r, v, token, dev)
		spoilerFree, err := i.viewerSpoilerFree(r.Context(), v)
		if err != nil {
			if retryBootstrap(err) {
				refresh = true
				continue
			}
			fail(w, err, r)
			return
		}
		// Credentials scope the image grants and cursor scope includes the complete
		// viewer policy. Do not reuse wire responses between refresh families/devices,
		// certificate-region preferences, or viewer spoiler-free preferences.
		scope := hashJSON([]any{v, dev, token, avatar, spoilerFree, downloads, r.URL.RequestURI(), i.appearanceDocument().etag, i.livePreviewMode(), i.liveEventsAvailable(), i.extrasAvailable(), i.extraPlaybackAvailable(), i.previewAvailable(), i.themeAvailable(), i.capabilitiesRevision(), i.signInMethods(), i.preferredRatingRegions(r), i.screensGeneration(), screenClient(r), frontend.ScreenPreviewFrom(r.Context())})
		freshness := i.bootstrapFreshness()
		var before frontend.HomeRevision
		cacheable := false
		if hasRevision {
			before, cacheable, err = source.HomeRevision(r.Context(), v)
			if retryBootstrap(err) {
				refresh = true
				continue
			}
			if err != nil {
				fail(w, bootstrapTransientError(err), r)
				return
			}
		}
		var pending *bootstrapEntry
		if cacheable {
			i.bootstrapMu.Lock()
			entry := i.bootstraps[scope]
			if entry != nil && entry.payload == nil {
				i.bootstrapMu.Unlock()
				select {
				case <-entry.ready:
				case <-r.Context().Done():
					return
				}
				// Re-read the revision after the builder commits, within the same bounded budget.
				continue
			}
			if entry != nil && time.Now().Before(entry.expires) && entry.revision == before && entry.freshness == freshness {
				i.bootstrapMu.Unlock()
				// The wire cache can outlive a replaced snapshot even when the
				// viewer revision and grant deadlines still match.
				err := i.validateBootstrapRows(r.Context(), entry.rows, entry.expires)
				if err == nil {
					varyLanguage(w)
					cache(w, r, entry.payload)
					return
				}
				if !retryBootstrap(err) {
					fail(w, bootstrapTransientError(err), r)
					return
				}
				i.bootstrapMu.Lock()
				if i.bootstraps[scope] != entry {
					i.bootstrapMu.Unlock()
					continue
				}
				delete(i.bootstraps, scope)
			}
			if i.bootstraps == nil {
				i.bootstraps = make(map[string]*bootstrapEntry)
			}
			total := 0
			for key, old := range i.bootstraps {
				if old.payload != nil && !time.Now().Before(old.expires) {
					delete(i.bootstraps, key)
					continue
				}
				total += len(old.payload)
			}
			// Admission bounds both pending builders and retained responses. A full
			// cache does not block other viewers or start background work.
			if len(i.bootstraps) < 32 && total < bootstrapBytes {
				pending = &bootstrapEntry{ready: make(chan struct{}), revision: before, freshness: freshness, expires: time.Now().Add(bootstrapTTL)}
				i.bootstraps[scope] = pending
			} else {
				delete(i.bootstraps, scope)
			}
			i.bootstrapMu.Unlock()
		}
		lease := &bootstrapDeadline{expires: time.Now().Add(bootstrapTTL)}
		buildRequest := r.WithContext(context.WithValue(r.Context(), bootstrapDeadlineKey{}, lease))
		if cacheable {
			if source, ok := i.host.Experience.(interface {
				BeginBootstrapRead(context.Context, *frontend.Viewer, frontend.HomeRevision) context.Context
			}); ok {
				buildRequest = buildRequest.WithContext(source.BeginBootstrapRead(buildRequest.Context(), v, before))
			}
		}
		out, buildErr := i.buildBootstrap(buildRequest, v, token, avatar, spoilerFree, refresh)
		var payload json.RawMessage
		if buildErr == nil {
			payload, buildErr = json.Marshal(out)
		}
		if buildErr == nil && cacheable {
			after, available, revisionErr := source.HomeRevision(request.Context(), v)
			afterSpoilerFree, sfErr := i.viewerSpoilerFree(request.Context(), v)
			if revisionErr != nil {
				buildErr = revisionErr
			} else if sfErr != nil {
				buildErr = sfErr
			} else if !available || before != after || freshness != i.bootstrapFreshness() || afterSpoilerFree != spoilerFree {
				buildErr = frontend.ErrRowViewStale
			}
		}
		if buildErr == nil {
			// Use the original context so the final check reads current
			// policy rather than the proof memoized during assembly.
			buildErr = i.validateBootstrapRows(request.Context(), lease.rows, lease.expires)
		}
		if pending != nil {
			i.bootstrapMu.Lock()
			total := len(payload)
			for key, old := range i.bootstraps {
				if key != scope {
					total += len(old.payload)
				}
			}
			if buildErr == nil && !lease.partial && total <= bootstrapBytes {
				pending.payload = payload
				pending.expires = lease.expires
				pending.rows = lease.rows
			} else {
				delete(i.bootstraps, scope)
			}
			close(pending.ready)
			i.bootstrapMu.Unlock()
		}
		if retryBootstrap(buildErr) {
			refresh = true
			continue
		}
		if buildErr != nil {
			fail(w, bootstrapTransientError(buildErr), r)
			return
		}
		varyLanguage(w)
		cache(w, r, payload)
		return
	}
	if err := request.Context().Err(); err != nil {
		fail(w, err, request)
		return
	}
	fail(w, &bootstrapChangedError{}, r)
}

func retryBootstrap(err error) bool {
	return errors.Is(err, frontend.ErrRowViewStale) || errors.Is(err, frontend.ErrLiveGuideStale)
}

// bootstrapTransientError prevents transient unavailability during bootstrap
// assembly from answering 409 not_available with retry.allowed: false; clients
// receive 503 + Retry-After so they wait and recover automatically.
func bootstrapTransientError(err error) error {
	if errors.Is(err, frontend.ErrNotAvailable) && !errors.Is(err, frontend.ErrPreparing) {
		return &frontend.PreparingError{RetryAfter: 2 * time.Second, Err: err}
	}
	return err
}

// Catalogue enrichment is a soft refresh (bounded by TTL). Policy revocations
// use HomeRevision.Policy; owner settings, grant rotations and library changes
// are hard inputs even when they do not write the shared policy epoch.
func (i *instance) bootstrapFreshness() frontend.PageFreshness {
	if i.host.PageFreshness == nil {
		return frontend.PageFreshness{}
	}
	f := i.host.PageFreshness()
	return frontend.PageFreshness{Config: f.Config, Layout: f.Layout, Grants: f.Grants, Accounts: f.Accounts, Safety: f.Safety, Libraries: f.Libraries, Images: f.Images, KindSync: f.KindSync, Screens: f.Screens}
}
