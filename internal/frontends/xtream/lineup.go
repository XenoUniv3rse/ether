package xtream

import (
	"bufio"
	"context"
	"io"
	"net/http"
	"net/url"

	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/kit/lineup"
)

func (i *instance) openLineups() *lineup.Store {
	return lineup.NewForHost(i.host, func(ctx context.Context, key lineup.Key, viewer *frontend.Viewer, out io.Writer) error {
		r := (&http.Request{URL: &url.URL{RawQuery: url.Values{"category_id": {key.Profile}}.Encode()}}).WithContext(frontend.WithExportTitlesOnly(frontend.WithRequestMemo(ctx)))
		buf := bufio.NewWriterSize(out, 64<<10)
		l := login{viewer: viewer, device: frontend.Device{Client: "IPTV player apps", Product: "Xtream"}, username: lineup.UserMarker, password: lineup.PassMarker}.detached()
		if err := i.writeLineup(r, l, buf, lineup.BaseMarker); err != nil {
			return err
		}
		return buf.Flush()
	})
}

func (i *instance) WaitLineup(ctx context.Context, viewer *frontend.Viewer) error {
	return i.lineups.Wait(ctx, lineup.ViewerKey(viewer, ""))
}

func (i *instance) writeLineup(r *http.Request, l login, out io.Writer, base string) error {
	return lineup.WriteVOD(r.Context(), out, base, l.username, l.password, pageSize,
		func(kind string, visit func(context.Context, frontend.Title, int32) error) error {
			return i.walk(r, l, kind, false, func(r *http.Request, l login, title frontend.Title, _ string) error {
				return visit(r.Context(), title, l.ids[title.Key])
			})
		}, func(ctx context.Context, key frontend.RatingKey, visit func(frontend.Title) error) error {
			return i.children(r.WithContext(ctx), l, key, false, func(_ login, title frontend.Title) error { return visit(title) })
		}, func(ctx context.Context, refs []string) (map[string]int32, error) {
			return i.host.IDs.CompactBatch(ctx, "xtream", "item", refs)
		})
}
