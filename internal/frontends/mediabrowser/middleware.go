package mediabrowser

import (
	"context"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5/middleware"

	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/kit/authhdr"
	"github.com/braedonsaunders/ether/internal/frontend/kit/reqmemo"
	"github.com/braedonsaunders/ether/internal/frontend/kit/route"
	"github.com/braedonsaunders/ether/internal/frontend/kit/support"
)

// anonymousStreamKey marks a request the middleware admitted without a
// credential so the handler can serve it from a recent session.
type anonymousStreamKey struct{}

// These exact method/pattern pairs are pre-login endpoints. Jellyfin's
// System/Endpoint requires authentication, but Emby clients use Ether's
// existing anonymous response before sign-in (see public_test.go).
// Media and image routes instead verify their own unguessable scoped grants.
var publicRoutesAllowlist = map[route.Route]bool{
	{Method: "POST", Pattern: "/Ether/WebSignIn"}:                           true,
	{Method: "GET", Pattern: "/Branding/Css"}:                               true,
	{Method: "GET", Pattern: "/Branding/Css.css"}:                           true,
	{Method: "GET", Pattern: "/System/Info/Public"}:                         true,
	{Method: "GET", Pattern: "/System/Endpoint"}:                            true,
	{Method: "GET", Pattern: "/System/Ping"}:                                true,
	{Method: "POST", Pattern: "/System/Ping"}:                               true,
	{Method: "GET", Pattern: "/Branding/Configuration"}:                     true,
	{Method: "GET", Pattern: "/web"}:                                        true,
	{Method: "GET", Pattern: "/web/{path...}"}:                              true,
	{Method: "GET", Pattern: "/"}:                                           true,
	{Method: "GET", Pattern: "/Users/Public"}:                               true,
	{Method: "POST", Pattern: "/Users/AuthenticateByName"}:                  true,
	{Method: "POST", Pattern: "/Users/AuthenticateWithQuickConnect"}:        true,
	{Method: "GET", Pattern: "/QuickConnect/Enabled"}:                       true,
	{Method: "POST", Pattern: "/QuickConnect/Initiate"}:                     true,
	{Method: "GET", Pattern: "/QuickConnect/Connect"}:                       true,
	{Method: "GET", Pattern: "/GetUtcTime"}:                                 true,
	{Method: "GET", Pattern: "/Items/{itemId}/Images/{imageType}"}:          true,
	{Method: "HEAD", Pattern: "/Items/{itemId}/Images/{imageType}"}:         true,
	{Method: "GET", Pattern: "/Items/{itemId}/Images/{imageType}/{index}"}:  true,
	{Method: "HEAD", Pattern: "/Items/{itemId}/Images/{imageType}/{index}"}: true,
	{Method: "GET", Pattern: "/Users/{userId}/Images/Primary"}:              true,
	{Method: "HEAD", Pattern: "/Users/{userId}/Images/Primary"}:             true,
	{Method: "GET", Pattern: "/Users/{userId}/Images/Primary/{index}"}:      true,
	{Method: "HEAD", Pattern: "/Users/{userId}/Images/Primary/{index}"}:     true,
	{Method: "GET", Pattern: "/UserImage"}:                                  true,
	{Method: "HEAD", Pattern: "/UserImage"}:                                 true,
	{Method: "GET", Pattern: "/sessions/lazy/{path...}"}:                    true,
	{Method: "HEAD", Pattern: "/sessions/lazy/{path...}"}:                   true,
	{Method: "GET", Pattern: "/sessions/media/{path...}"}:                   true,
	{Method: "HEAD", Pattern: "/sessions/media/{path...}"}:                  true,
}

// anonymousStreamRoutes are the media stream aliases a client may request
// without any credential. Real Jellyfin serves these anonymously
// (VideosController.GetVideoStream has no [Authorize]); Findroid and the
// Jellyfin Kotlin SDK build /Videos/{id}/stream URLs and hand them to
// ExoPlayer with no token. The handler admits such a request only when it
// matches a playback session an authenticated viewer just opened from the
// same client IP for the same item (see anonIndex). An authenticated
// request on these routes still flows through the normal path.
var anonymousStreamRoutes = map[route.Route]bool{
	{Method: "GET", Pattern: "/Videos/{id}/{name}"}:  true,
	{Method: "HEAD", Pattern: "/Videos/{id}/{name}"}: true,
	{Method: "GET", Pattern: "/Audio/{id}/{name}"}:   true,
	{Method: "HEAD", Pattern: "/Audio/{id}/{name}"}:  true,
}

func validStreamAlias(id, name string) bool {
	name = strings.ToLower(name)
	if themeParentID(id) != "" {
		return name == "stream" || name == "stream.m4a" || name == "stream.mp3"
	}
	switch {
	case name == "stream", strings.HasPrefix(name, "stream."), name == "universal", name == "master.m3u8", name == "main.m3u8":
		return true
	default:
		return false
	}
}

// requestLog records only registered templates: concrete paths can contain
// scoped media grants, and query strings and headers can contain account tokens.
func (i *instance) requestLog(rt route.Route, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if i.host.Log == nil || !i.host.Log.Enabled(r.Context(), slog.LevelDebug) {
			next.ServeHTTP(w, r)
			return
		}
		start := time.Now()
		recorder := middleware.NewWrapResponseWriter(w, r.ProtoMajor)
		defer func() {
			status := recorder.Status()
			if status == 0 {
				status = http.StatusOK
			}
			i.host.Log.DebugContext(r.Context(), "request", "frontend", i.protocol,
				"method", safeHTTPMethod(r.Method), "route", rt.Pattern,
				"status", status, "duration", time.Since(start))
		}()
		next.ServeHTTP(recorder, r)
	})
}

func (i *instance) routeMiddleware(rt route.Route, next http.Handler) http.Handler {
	inner := i.authenticatedRoute(rt, next)
	return i.requestLog(rt, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet || r.Method == http.MethodHead {
			ctx := r.Context()
			if !anonymousStreamRoutes[rt] && !strings.HasPrefix(rt.Pattern, "/sessions/") {
				ctx = frontend.WithQuietIdentity(ctx)
			}
			if !reqmemo.Active(ctx) {
				ctx = reqmemo.With(ctx)
			}
			if frontend.RequestMemoFrom(ctx) == nil {
				ctx = frontend.WithRequestMemo(ctx)
			}
			r = r.WithContext(ctx)
		}
		inner.ServeHTTP(w, r)
	}))
}

func isSensitiveAuthRoute(pattern string) bool {
	switch pattern {
	case "/Auth/Keys", "/Auth/Keys/{key}", "/Auth/Keys/{key}/Delete",
		"/Users/New", "/Users/{userId}/Password", "/Users/Password":
		return true
	}
	return false
}

func (i *instance) authenticatedRoute(rt route.Route, next http.Handler) http.Handler {
	// Never sample hand-off request or response secrets in support recordings.
	if rt.Pattern == "/Ether/WebSignIn" {
		return next
	}
	sensitive := isSensitiveAuthRoute(rt.Pattern)
	if !sensitive && i.pages != nil && i.host.PageFreshness != nil {
		original := next
		cached := i.pages.wrap(next, i.host.PageFreshness)
		next = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !publicRoutesAllowlist[rt] {
				if _, _, err := i.viewer(r); err != nil {
					original.ServeHTTP(w, r)
					return
				}
			}
			cached.ServeHTTP(w, r)
		})
	}
	if !sensitive {
		next = support.Wrap(i.host.Support, string(i.protocol), func(r *http.Request) (*frontend.Viewer, frontend.Device) {
			if _, ok := i.host.Identity.(frontend.GrantDeviceIdentity); !ok {
				return nil, frontend.Device{}
			}
			v, _, err := i.viewer(r)
			if err != nil {
				return nil, frontend.Device{}
			}
			return v, device(r)
		}, func(path string) string { return path }, next)
	}
	if publicRoutesAllowlist[rt] {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if anonymousStreamRoutes[rt] && !validStreamAlias(r.PathValue("id"), r.PathValue("name")) {
			mediaBrowserError(w, r, http.StatusNotFound)
			return
		}
		if i.protocol != "jellyfin" && (rt.Pattern == "/Items/{id}" || rt.Pattern == "/Users/{userId}/Items/{id}") && strings.EqualFold(r.PathValue("id"), "Root") {
			mediaBrowserError(w, r, http.StatusNotFound)
			return
		}
		v, token, err := i.authenticate(r)
		if err != nil || v == nil {
			// A stream alias with no credential at all is answered by the
			// handler against a just-opened session; anything carrying a bad
			// or foreign credential is still rejected here.
			if anonymousStreamRoutes[rt] && !authhdr.MediaBrowserCredentialSupplied(r) {
				r = r.WithContext(context.WithValue(r.Context(), anonymousStreamKey{}, true))
				next.ServeHTTP(w, r)
				return
			}
			if i.protocol == "emby" && anonymousStreamRoutes[rt] && queryBool(r, "Static", false) {
				name := strings.ToLower(r.PathValue("name"))
				if name == "stream" || strings.HasPrefix(name, "stream.") {
					w = &embyStaticResponse{ResponseWriter: w}
				}
			}
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
		r = r.WithContext(context.WithValue(r.Context(), principalKey{}, principal{viewer: v, token: token, apiKey: frontend.IsAPIKeyCredential(token)}))
		if _, _, err := i.viewer(r); err == nil {
			if i.host.ClientSeen != nil {
				i.host.ClientSeen(device(r).ID)
			}
			if !sensitive {
				if _, ok := i.host.Identity.(frontend.GrantDeviceIdentity); ok {
					support.Seen(r, i.host.Support, string(i.protocol), v, device(r))
				}
			}
		}
		next.ServeHTTP(w, r)
	})
}
