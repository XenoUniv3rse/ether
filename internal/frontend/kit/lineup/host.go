package lineup

import (
	"context"
	"fmt"
	"io"

	"github.com/braedonsaunders/ether/internal/frontend"
)

type ViewerSource interface {
	XtreamLineupViewers(context.Context) ([]frontend.Viewer, error)
}

func ViewerKey(v *frontend.Viewer, category string) Key {
	if category == "0" {
		category = ""
	}
	return Key{Viewer: v.ID, Profile: category, Restricted: !v.IsOwner || v.MaxContentRating != ""}
}

func NewForHost(host frontend.Host, build func(context.Context, Key, *frontend.Viewer, io.Writer) error) *Store {
	users, ok := host.Identity.(frontend.UserLookupSource)
	if !ok || host.PageFreshness == nil {
		return nil
	}
	opt := hostOptions(host)
	opt.Build = func(ctx context.Context, key Key, out io.Writer) error {
		viewer, err := users.User(ctx, key.Viewer)
		if err != nil {
			return err
		}
		if viewer == nil || ViewerKey(viewer, key.Profile) != key {
			return frontend.ErrUnauthorized
		}
		return build(ctx, key, viewer, out)
	}
	return New(host.CacheDir, opt)
}

func hostOptions(host frontend.Host) Options {
	return Options{Viewers: func(ctx context.Context) ([]Key, error) {
		source, ok := host.Identity.(ViewerSource)
		if !ok {
			return nil, nil
		}
		viewers, err := source.XtreamLineupViewers(ctx)
		keys := make([]Key, 0, len(viewers))
		for _, viewer := range viewers {
			keys = append(keys, ViewerKey(&viewer, ""))
		}
		return keys, err
	}, Revision: func(key Key) Revision {
		f := host.PageFreshness()
		policy := fmt.Sprint(f.Config, ":", f.Accounts, ":", f.Safety, ":", f.Hidden, ":", f.Libraries, ":", f.KindSync, ":", f.EpisodeOrder, ":", f.Watch, ":", f.Favorite, ":", f.Listed)
		// A change in catalogue facts may reclassify a title for a limited
		// viewer even while the configured policy itself stays the same.
		judged := key.Restricted
		if host.LineupPolicy != nil {
			revision, current := host.LineupPolicy(key.Viewer)
			policy += fmt.Sprint(":", revision)
			judged = judged || current
		}
		if judged {
			policy += fmt.Sprint(":", f.Catalog)
		}
		return Revision{Policy: policy, Content: fmt.Sprint(f.Catalog, ":", f.Availability)}
	}}
}
