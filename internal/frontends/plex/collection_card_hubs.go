package plex

import (
	"context"
	"net/http"
	"net/url"
	"strings"

	"github.com/braedonsaunders/ether/internal/frontend"
	kitimages "github.com/braedonsaunders/ether/internal/frontend/kit/images"
	"github.com/braedonsaunders/ether/internal/frontends/plex/model"
)

type collectionCardHubSource interface {
	CollectionCardHubs(context.Context, *frontend.User, frontend.HubContext, int) []frontend.Hub
}

func (s *Server) renderCollectionCardHub(ctx context.Context, pr *Request, hub frontend.Hub) model.Hub {
	out := model.Hub{Title: hub.Title, Type: "collection", HubIdentifier: hub.ID, Style: "shelf", Promoted: model.Ptr(true)}
	vis := s.visibility()
	for _, collection := range hub.Collections {
		if vis.CollectionAllowed(pr.User, &collection) {
			dir := s.collectionDirectory(ctx, &collection)
			if collection.PosterPath != "" {
				dir.Thumb += "?collectionCard=1"
			} else {
				dir.Thumb = ""
			}
			if dir.Art != "" {
				dir.Art += "?collectionCard=1"
			}
			dir.Extra = append(dir.Extra, model.A("guid", "collection://"+collection.Key.String()), model.A("summary", collection.Summary))
			out.Directory = append(out.Directory, dir)
		}
	}
	out.Size = len(out.Directory)
	out.More = hub.More && out.Size > 0
	if out.More {
		out.Key = "/hubs/items?identifier=" + url.QueryEscape(hub.ID)
	}
	return out
}

func (s *Server) writeCollectionCardPage(w http.ResponseWriter, r *http.Request, pr *Request, id string) bool {
	if !strings.HasPrefix(id, "home.") || !frontend.ValidRowID(strings.TrimPrefix(id, "home.")) {
		return false
	}
	source, ok := s.recommender().(collectionCardHubSource)
	if !ok {
		return false
	}
	ctx := r.Context()
	hc := s.hubContext(pr, 0)
	for _, hub := range source.CollectionCardHubs(ctx, pr.User, hc, frontend.MaxCollectionCards) {
		if hub.ID != id {
			continue
		}
		row := s.renderCollectionCardHub(ctx, pr, hub)
		facts, neutral := homeLayoutFacts(row)
		rows := s.arrangeHomeRows(ctx, pr, hc, "home", []model.Hub{row}, []frontend.Hub{facts}, []frontend.NeutralHomeRow{neutral})
		if len(rows) == 0 {
			return false
		}
		mc := hubContainer()
		dirs := rows[0].Directory
		start, end := pr.window(len(dirs))
		mc.Directory = dirs[start:end]
		mc.Size = len(mc.Directory)
		mc.SetPage(start, len(dirs))
		s.write(w, r, mc)
		return true
	}
	return false
}

// A card image follows the viewer-scoped artwork authorization without opening
// the collection or invoking its full poster renderer.
func (s *Server) serveCardArtwork(w http.ResponseWriter, r *http.Request, key frontend.RatingKey, element string, width int) {
	art := s.getHost().Artwork
	if art == nil {
		writeHTTPError(w, http.StatusNotFound)
		return
	}
	kind := frontend.ImagePoster
	if element == "art" || element == "background" {
		kind = frontend.ImageBackdrop
	}
	ctx := frontend.WithCollectionCardArtwork(r.Context())
	r = r.WithContext(ctx)
	req := frontend.ArtworkRequest{Viewer: FromContext(ctx).User, Item: key}
	list, _, err := art.List(ctx, req)
	if err != nil {
		writeHTTPError(w, http.StatusNotFound)
		return
	}
	for _, img := range list {
		if img.Kind != kind {
			continue
		}
		if _, height := photoDimensions(r); width <= 0 && height == 0 {
			width = unsizedImageWidth(element, 500)
		}
		data, ct, err := art.Read(ctx, req, img, frontend.ArtworkTransform{Width: width})
		if err != nil {
			break
		}
		data, ct, err = s.transformPhoto(r, data, ct)
		if err != nil {
			break
		}
		w.Header().Add("Vary", "X-Plex-Token")
		w.Header().Add("Vary", "X-Plex-Product")
		writeArtworkBytes(w, r, data, ct, kitimages.Revalidate)
		return
	}
	writeHTTPError(w, http.StatusNotFound)
}
