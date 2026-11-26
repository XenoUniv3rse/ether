package mediabrowser

import (
	"context"
	"net/http"
	"sync"
	"time"

	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/kit/ids"
)

// Listed versions: Jellyfin lists every version of a title in a list row's
// MediaSources, and Infuse builds its movie page and version picker from that
// row (it plays without reading /Items/{id} first). A row with one source
// shows no picker. Rows here carry the owner's configured version menu
// (Auto, then each configured slot), the same cheap projection Plex rows
// already carry: no search, no resolve, no per-row offer check or write. A
// slot's MediaSource Id is the id detail and PlaybackInfo would issue for it;
// PlaybackInfo authorizes it for the played item (resolveSourceFor).

type listVersionScopeKey struct{}

type listVersionScope struct {
	once  sync.Once
	scope frontend.VersionScope
	ok    bool
}

func withListVersionScope(ctx context.Context) context.Context {
	if ctx.Value(listVersionScopeKey{}) != nil {
		return ctx
	}
	return context.WithValue(ctx, listVersionScopeKey{}, &listVersionScope{})
}

// listScope builds the version scope once per list request.
func (i *instance) listScope(r *http.Request, viewer *frontend.Viewer) (frontend.VersionScope, bool) {
	memo, _ := r.Context().Value(listVersionScopeKey{}).(*listVersionScope)
	if memo == nil {
		scope, err := i.versionScope(r, viewer)
		return scope, err == nil
	}
	memo.once.Do(func() {
		scope, err := i.versionScope(r, viewer)
		memo.scope, memo.ok = scope, err == nil
	})
	return memo.scope, memo.ok
}

// listedVersionSources returns the configured slots after Auto as list-row
// MediaSources, or nil when the menu is Auto alone. auto is the row's
// existing source; it is renamed for the picker only when slots follow it.
func (i *instance) listedVersionSources(r *http.Request, viewer *frontend.Viewer, auto map[string]any, title frontend.Title) []any {
	if r == nil || viewer == nil || auto == nil || title.Key.IsPluginKey() || (title.Type != "movie" && title.Type != "episode") || mbIsBookItem(title) {
		return nil
	}
	if !title.Released.IsZero() && title.Released.After(time.Now()) {
		return nil
	}
	slots, ok := i.host.Editions.(frontend.VersionSlots)
	if !ok {
		return nil
	}
	listed, ok := i.host.VersionHandles.(frontend.ListedVersionHandles)
	if !ok {
		return nil
	}
	scope, ok := i.listScope(r, viewer)
	if !ok {
		return nil
	}
	caps := scope.Caps
	// The menu describes the source, as on Plex rows; bitrate ceilings
	// belong to the output decision at play time.
	caps.MaxBitrateKbps, caps.DefaultRemoteBitrate = 0, false
	t := title
	versions, err := slots.VersionSlots(i.identityContext(r), frontend.ResolveRequest{Title: &t, Client: caps, User: viewer}, frontend.MaxVariants, false)
	if err != nil || len(versions) < 2 {
		return nil
	}
	out := make([]any, 0, len(versions)-1)
	for _, v := range versions {
		if v.Auto || v.ID == "" {
			continue
		}
		handle := listed.ListedHandle(scope, title.Key, v.ID)
		if handle == "" {
			continue
		}
		source := i.mediaSourceDTO(handle, v.Info)
		source["Name"] = v.Label
		i.mediaSourceDialect(source)
		i.jellyfinMediaSourceDTO(source, v.Info, false)
		jellyfinMediaSourceShape(source, title.Title)
		delete(source, "Chapters")
		i.completeFileSource(r, source, title, "")
		out = append(out, source)
	}
	if len(out) == 0 {
		return nil
	}
	if primary := versions[0]; primary.Auto {
		i.applyVersionMediaFacts(auto, ids.RatingKeyGUID(title.Key), primary.Info)
		if primary.Label != "" {
			auto["Name"] = primary.Label
		}
	}
	return out
}

// resolveSourceFor is resolveSource for a handle played on a known item. A
// configured version a list row named resolves here the first time it is
// played; the offer check runs for that one item.
func (i *instance) resolveSourceFor(r *http.Request, scope frontend.VersionScope, key frontend.RatingKey, handle string) (frontend.RatingKey, string, string, error) {
	target, kind, tag, err := i.resolveSource(r, scope, handle)
	if err == nil || key == 0 {
		return target, kind, tag, err
	}
	if listed, ok := i.host.VersionHandles.(frontend.ListedVersionHandles); ok {
		if t, e := listed.ResolveListed(i.identityContext(r), scope, key, handle); e == nil && t.Item == key {
			return t.Item, "version", t.Release, nil
		}
	}
	return target, kind, tag, err
}
