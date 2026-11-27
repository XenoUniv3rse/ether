package mediabrowser

import (
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/kit/ids"
	"github.com/braedonsaunders/ether/internal/frontend/kit/route"
)

func (i *instance) mutate(w http.ResponseWriter, r *http.Request, kind string) {
	v, _, err := i.viewer(r)
	if err != nil || v == nil {
		http.Error(w, "Unauthorized", 401)
		return
	}
	if kind == "played" && frontend.IsCatchUpGUID(r.PathValue("itemId")) {
		i.catchUpPlayed(w, r, v, r.PathValue("itemId"))
		return
	}
	if i.host.Experience == nil {
		http.Error(w, "Unavailable", 503)
		return
	}
	key, err := ids.ParseRatingKeyGUID(r.PathValue("itemId"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	switch kind {
	case "played":
		if r.Method == "POST" {
			err = i.host.Experience.SetPlayed(r.Context(), v, key)
		} else {
			err = i.host.Experience.SetUnplayed(r.Context(), v, key)
		}
	case "likes":
		var likes *bool
		if r.Method == "POST" {
			values := route.Query(r.URL.Query(), "likes")
			if len(values) > 1 {
				http.Error(w, "Bad Request", 400)
				return
			}
			if len(values) == 1 {
				val, e := strconv.ParseBool(values[0])
				if e != nil {
					http.Error(w, "Bad Request", 400)
					return
				}
				likes = &val
			}
		}
		err = i.host.Experience.SetLikes(r.Context(), v, key, likes)
	case "favorite":
		err = i.host.Experience.SetFavorite(r.Context(), v, key, r.Method == "POST")
	}
	if err != nil {
		if errors.Is(err, frontend.ErrNotFound) {
			http.NotFound(w, r)
			return
		}
		http.Error(w, "Unavailable", 503)
		return
	}
	data, err := i.host.Experience.UserData(r.Context(), v, []frontend.RatingKey{key})
	if err != nil {
		http.Error(w, "Unavailable", 503)
		return
	}
	st := data[key]
	id := ids.RatingKeyGUID(key)
	dto := i.userDataDTO(id, st)
	// Jellyfin omits a zero percentage on the played-item body. Item listings
	// still carry the key when the watch row has progress.
	if i.protocol == "jellyfin" {
		if pct, ok := dto["PlayedPercentage"].(float64); ok && pct == 0 {
			delete(dto, "PlayedPercentage")
		}
	}
	jsonReply(w, dto)
}
func (i *instance) mutationRoutes() error {
	for _, path := range []string{"/UserItems/{itemId}/UserData", "/Users/{userId}/Items/{itemId}/UserData"} {
		if err := i.router.Handle(http.MethodPost, path, http.HandlerFunc(i.updateUserItemData)); err != nil {
			return err
		}
	}
	for _, path := range []struct{ pattern, kind string }{{"/UserItems/{itemId}/Rating", "likes"}, {"/Users/{userId}/Items/{itemId}/Rating", "likes"}, {"/UserPlayedItems/{itemId}", "played"}, {"/Users/{userId}/PlayedItems/{itemId}", "played"}, {"/UserFavoriteItems/{itemId}", "favorite"}, {"/Users/{userId}/FavoriteItems/{itemId}", "favorite"}} {
		for _, method := range []string{"POST", "DELETE"} {
			kind := path.kind
			if err := i.router.Handle(method, path.pattern, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { i.mutate(w, r, kind) })); err != nil {
				return err
			}
		}
	}
	return nil
}

func (i *instance) updateUserItemData(w http.ResponseWriter, r *http.Request) {
	v, _, err := i.viewer(r)
	if errors.Is(err, errViewerScope) {
		http.Error(w, http.StatusText(http.StatusForbidden), http.StatusForbidden)
		return
	}
	if err != nil || v == nil {
		http.Error(w, http.StatusText(http.StatusUnauthorized), http.StatusUnauthorized)
		return
	}

	var body *struct {
		PlaybackPositionTicks     *int64
		Played, IsFavorite, Likes *bool
		PlayCount                 *int
		Rating                    *float64
		LastPlayedDate            *time.Time
	}
	if err := decode(r, &body); err != nil || body == nil ||
		(body.PlaybackPositionTicks != nil && (*body.PlaybackPositionTicks < 0 || *body.PlaybackPositionTicks > (1<<63-1)/100)) ||
		(body.PlayCount != nil && (*body.PlayCount < 0 || *body.PlayCount > (1<<31-1))) ||
		(body.Rating != nil && (*body.Rating < 0 || *body.Rating > 10)) ||
		(body.LastPlayedDate != nil && body.LastPlayedDate.Before(time.Unix(0, 0))) {
		http.Error(w, http.StatusText(http.StatusBadRequest), http.StatusBadRequest)
		return
	}
	patch := frontend.ItemPlaybackPatch{Played: body.Played, PlayCount: body.PlayCount, LastPlayedAt: body.LastPlayedDate}
	if body.PlaybackPositionTicks != nil {
		position := time.Duration(*body.PlaybackPositionTicks) * 100
		patch.Position = &position
	}
	id := r.PathValue("itemId")
	if frontend.IsCatchUpGUID(id) {
		i.patchCatchUpUserData(w, r, v, id, frontend.CatchUpUserDataPatch{ItemPlaybackPatch: patch, Favorite: body.IsFavorite, Likes: body.Likes, Rating: body.Rating})
		return
	}

	exp := i.host.Experience
	if exp == nil {
		writeUserDataError(w, r, frontend.ErrNotAvailable)
		return
	}
	key, err := ids.ParseRatingKeyGUID(id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	ctx := frontend.WithWatchOrigin(i.identityContext(r), frontend.WatchOrigin{Surface: i.protocol, ClientID: device(r).ID})
	if !patch.Empty() {
		if writer, ok := exp.(frontend.ItemPlaybackWriter); ok {
			err = writer.PatchItemPlayback(ctx, v, key, patch)
		} else {
			err = frontend.ErrNotAvailable
		}
	}
	if err == nil && body.IsFavorite != nil {
		err = exp.SetFavorite(ctx, v, key, *body.IsFavorite)
	}
	if err == nil && body.Rating != nil {
		err = exp.SetRating(ctx, v, key, *body.Rating)
	}
	if err == nil && body.Likes != nil {
		err = exp.SetLikes(ctx, v, key, body.Likes)
	}
	if err != nil {
		writeUserDataError(w, r, err)
		return
	}
	data, err := exp.UserData(ctx, v, []frontend.RatingKey{key})
	if err != nil {
		writeUserDataError(w, r, err)
		return
	}
	status, ok := data[key]
	if !ok {
		http.NotFound(w, r)
		return
	}
	i.updatedUserData(w, r, ids.RatingKeyGUID(key), status)
}

func (i *instance) updatedUserData(w http.ResponseWriter, r *http.Request, id string, status frontend.WatchStatus) {
	if i.protocol == "emby" && r.PathValue("userId") != "" {
		w.WriteHeader(http.StatusOK)
		return
	}
	jsonReply(w, i.userDataDTO(id, status))
}

func writeUserDataError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, frontend.ErrNotFound):
		http.NotFound(w, r)
	case errors.Is(err, frontend.ErrUnauthorized):
		http.Error(w, http.StatusText(http.StatusUnauthorized), http.StatusUnauthorized)
	default:
		http.Error(w, http.StatusText(http.StatusServiceUnavailable), http.StatusServiceUnavailable)
	}
}
