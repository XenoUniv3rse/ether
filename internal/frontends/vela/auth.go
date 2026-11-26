package vela

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/kit/ids"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"time"
)

func viewerWire(v *frontend.Viewer) object {
	id, _ := ids.GUID(ids.User, v.ID)
	// The viewer value includes current policy; revisions change when those facts do.
	return object{"id": id, "name": frontend.ViewerDisplayName(v), "policyRevision": hashJSON(v)}
}

func (i *instance) viewerAvatar(r *http.Request, v *frontend.Viewer, token string, d frontend.Device) object {
	if i.host.ImageGrants == nil || token == "" {
		return nil
	}
	img, ok := frontend.ViewerAvatarImage(v)
	if !ok {
		return nil
	}
	secret, err := i.host.ImageGrants.Issue(r.Context(), frontend.ImageGrantRequest{
		Credential: token,
		Device:     d,
		Image:      img,
	})
	if err != nil || secret == "" {
		return nil
	}
	return object{
		"id":    hash(img.Path),
		"kind":  frontend.ImagePerson,
		"tag":   img.Tag,
		"sizes": []object{imageSize(img, secret)},
	}
}

// viewerPolicyWire reads the current owner policy for a bootstrap or sign-in.
// Countdown instants come from the same evaluator as playback admission.
func (i *instance) viewerPolicyWire(r *http.Request, v *frontend.Viewer, d frontend.Device, avatar object) (object, error) {
	out := viewerWire(v)
	if avatar != nil {
		out["avatar"] = avatar
	}
	if i.host.ViewerCapabilities == nil {
		return out, nil
	}
	ip, _ := frontend.ClientIP(i.identityContext(r))
	info, err := i.host.ViewerCapabilities(r.Context(), v, d, ip)
	if err != nil {
		return nil, err
	}
	if info.Bedtime != nil {
		out["bedtime"] = info.Bedtime
	}
	if info.BedtimeState != nil {
		out["bedtimeState"] = info.BedtimeState
	}
	out["policyRevision"] = hashJSON(object{"viewer": v, "policy": info.PolicyRevision, "bedtime": info.Bedtime})
	return out, nil
}
func (i *instance) tokens(w http.ResponseWriter, r *http.Request, access, refresh string, d frontend.Device) {
	resolver, ok := i.host.Identity.(frontend.DeviceIdentity)
	if !ok {
		fail(w, frontend.ErrUnauthorized)
		return
	}
	v, expires, err := resolver.ResolveDevice(r.Context(), access, d)
	if err != nil || v == nil {
		fail(w, frontend.ErrUnauthorized)
		return
	}
	i.tokenResult(w, r, access, refresh, d, v, expires)
}
func (i *instance) auth(w http.ResponseWriter, r *http.Request) {
	var b struct {
		Username, Password, RefreshToken, Code, Browser, ExistingToken, UserCode, DeviceCode, Secret, TargetOrigin string
		PlexToken                                                                                                  string `json:"plexToken"`
		SelectProfile                                                                                              bool   `json:"selectProfile"`
		SelectionToken                                                                                             string `json:"selectionToken"`
		ProfileID                                                                                                  string `json:"profileId"`
		PIN                                                                                                        string `json:"pin"`
		Device                                                                                                     device `json:"device"`
	}
	if body(r, &b) != nil {
		problem(w, 400, "request", "invalid_request")
		return
	}
	ctx := i.identityContext(r)
	d := b.Device.neutral()
	path := strings.TrimPrefix(r.URL.Path, "/vela/v0/auth/")
	switch path {
	case "web-code":
		_, access, err := i.viewer(r)
		if err != nil {
			fail(w, err, r)
			return
		}
		if d.ID != requestDevice(r).ID || !i.trustedSecureOrigin(r, b.TargetOrigin) {
			problem(w, 400, "request", "invalid_target_origin")
			return
		}
		issuer, ok := i.host.Identity.(interface {
			IssueOriginWebCode(context.Context, string, frontend.Device, string) (string, error)
		})
		if !ok {
			i.unavailable(w, r)
			return
		}
		code, err := issuer.IssueOriginWebCode(ctx, access, d, b.TargetOrigin)
		if err != nil {
			fail(w, err, r)
			return
		}
		send(w, 200, object{"code": code, "targetOrigin": b.TargetOrigin, "expiresAt": time.Now().Add(time.Minute).UTC().Format(time.RFC3339)})

	case "sign-in":
		if !i.signInMethods().Password {
			i.unavailable(w, r)
			return
		}
		a, e := i.host.Identity.SignIn(ctx, b.Username, b.Password, d)
		if e != nil {
			fail(w, e, r)
			return
		}
		i.tokens(w, r, a, "", d)
	case "plex":
		if !i.signInMethods().Plex {
			i.unavailable(w, r)
			return
		}
		if b.SelectProfile {
			i.plexSignInSelect(w, r, b.PlexToken, d)
			return
		}
		a, e := i.host.Identity.SignInWithPlex(ctx, b.PlexToken, d)
		if e != nil {
			plexAuthFail(w, e)
			return
		}
		i.tokens(w, r, a, "", d)
	case "plex/profile":
		i.plexProfileComplete(w, r, b.SelectionToken, b.ProfileID, b.PIN, d)
	case "plex/profiles/challenge":
		i.plexProfileChallenge(w, r, d)
	case "refresh":
		a, f, e := i.host.Identity.Refresh(ctx, b.RefreshToken, d)
		if e != nil {
			fail(w, e, r)
			return
		}
		i.tokens(w, r, a, f, d)
	case "revoke":
		_, token, e := i.viewer(r)
		if e != nil {
			fail(w, e, r)
			return
		}
		if e = i.host.Identity.Revoke(ctx, token); e != nil {
			fail(w, e, r)
			return
		}
		send(w, 200, object{"accepted": true})
	case "web-code/redeem":
		service, ok := i.host.Identity.(interface {
			RedeemDeviceWebCode(context.Context, string, frontend.Device) (string, error)
		})
		if !ok {
			i.unavailable(w, r)
			return
		}
		var a string
		var e error
		reqOrigin, hasOrigin := i.host.Origin(r)
		secureOrigin := ""
		if hasOrigin && reqOrigin.Scheme == "https" && reqOrigin.Host != "" {
			secureOrigin = "https://" + reqOrigin.Host
		}
		if bound, ok := i.host.Identity.(interface {
			RedeemOriginWebCode(context.Context, string, frontend.Device, string) (string, error)
		}); ok && secureOrigin != "" && i.trustedSecureOrigin(r, secureOrigin) {
			a, e = bound.RedeemOriginWebCode(ctx, b.Code, d, secureOrigin)
		} else {
			a, e = service.RedeemDeviceWebCode(ctx, b.Code, d)
		}
		if e != nil {
			fail(w, e, r)
			return
		}
		i.tokens(w, r, a, "", d)
	case "device", "quick-connect":
		if !i.signInMethods().QuickConnect {
			i.unavailable(w, r)
			return
		}
		if path == "device" && i.host.ViewerPortalURL == nil {
			i.unavailable(w, r)
			return
		}
		code, secret, e := i.host.Identity.QuickConnectInitiate(ctx, d)
		if e != nil {
			fail(w, e, r)
			return
		}
		details, e := i.host.Identity.QuickConnectDetails(ctx, secret)
		if e != nil {
			fail(w, e, r)
			return
		}
		if path == "quick-connect" {
			send(w, 200, object{"secret": secret, "code": code, "expiresAt": details.ExpiresAt.UTC().Format(time.RFC3339), "pollIntervalSeconds": 5})
			return
		}
		if i.host.ViewerPortalURL == nil {
			i.unavailable(w, r)
			return
		}
		url, e := i.host.ViewerPortalURL(ctx, r)
		if e != nil {
			fail(w, e, r)
			return
		}
		send(w, 200, object{"deviceCode": secret, "userCode": code, "verificationUrl": url, "expiresAt": details.ExpiresAt.UTC().Format(time.RFC3339), "pollIntervalSeconds": 5})
	case "device/poll", "quick-connect/poll":
		if !i.signInMethods().QuickConnect {
			i.unavailable(w, r)
			return
		}
		secret := b.Secret
		if path == "device/poll" {
			secret = b.DeviceCode
		}
		service, ok := i.host.Identity.(interface {
			QuickConnectPollDevice(context.Context, string, frontend.Device) (string, error)
		})
		if !ok {
			i.unavailable(w, r)
			return
		}
		a, e := service.QuickConnectPollDevice(ctx, secret, d)
		if errors.Is(e, frontend.ErrQuickConnectPending) {
			send(w, 200, object{"status": "pending"})
			return
		}
		if e != nil {
			fail(w, e, r)
			return
		}
		// Use a recorder so the token response keeps the same authoritative expiry.
		out := httptest.NewRecorder()
		i.tokens(out, r, a, "", d)
		if out.Code != 200 {
			send(w, out.Code, json.RawMessage(out.Body.Bytes()))
			return
		}
		var tokens object
		_ = json.Unmarshal(out.Body.Bytes(), &tokens)
		send(w, 200, object{"status": "approved", "tokens": tokens})
	case "device/approve", "quick-connect/approve":
		v, _, e := i.viewer(r)
		if e != nil {
			fail(w, e, r)
			return
		}
		if v.ID <= 0 || v.Delegated {
			problem(w, http.StatusForbidden, "policy", "approval_not_allowed")
			return
		}
		if !i.signInMethods().QuickConnect {
			i.unavailable(w, r)
			return
		}
		code := strings.TrimSpace(b.Code)
		if path == "device/approve" {
			code = strings.TrimSpace(b.UserCode)
		}
		if code == "" {
			problem(w, http.StatusBadRequest, "request", "invalid_code")
			return
		}
		if e = i.host.Identity.QuickConnectApprove(ctx, v, code); e != nil {
			quickConnectApproveFail(w, e, r)
			return
		}
		send(w, 200, object{"accepted": true})
	}
}

// quickConnectApproveFail translates approval errors after the caller's bearer
// token has already been verified. Bad, expired, or already-used codes must
// never return 401/auth, which Vela's transport treats as an expired session.
func quickConnectApproveFail(w http.ResponseWriter, err error, r *http.Request) {
	switch {
	case errors.Is(err, frontend.ErrQuickConnectExpired):
		send(w, http.StatusGone, object{
			"error": object{
				"class":   "conflict",
				"code":    "code_expired",
				"message": "code expired",
				"retry":   object{"allowed": false},
			},
		})
	case errors.Is(err, frontend.ErrQuickConnectUsed):
		send(w, http.StatusConflict, object{
			"error": object{
				"class":   "conflict",
				"code":    "code_used",
				"message": "code already used",
				"retry":   object{"allowed": false},
			},
		})
	case errors.Is(err, frontend.ErrUnauthorized):
		send(w, http.StatusBadRequest, object{
			"error": object{
				"class":   "request",
				"code":    "invalid_code",
				"message": "invalid code",
				"retry":   object{"allowed": false},
			},
		})
	case errors.Is(err, frontend.ErrRateLimited):
		seconds := 1
		var limited *frontend.IdentityRateLimitError
		if errors.As(err, &limited) && limited.RetryAfter > 0 {
			seconds = max(1, min(900, int((limited.RetryAfter+time.Second-1)/time.Second)))
		}
		w.Header().Set("Retry-After", strconv.Itoa(seconds))
		send(w, http.StatusTooManyRequests, object{
			"error": object{
				"class":   "capacity",
				"code":    "rate_limited",
				"message": "rate limited",
				"retry":   object{"allowed": true, "afterSeconds": seconds, "maxAttempts": 2},
			},
		})
	default:
		fail(w, err, r)
	}
}

func plexAuthFail(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, frontend.ErrUnauthorized):
		problem(w, http.StatusUnauthorized, "auth", "invalid_credentials")
	case errors.Is(err, frontend.ErrPlexNotShared):
		send(w, http.StatusForbidden, object{
			"error": object{
				"class":   "policy",
				"code":    "not_shared",
				"message": "this server is not shared with this Plex account; ask the server owner to share access",
				"retry":   object{"allowed": false},
			},
		})
	case errors.Is(err, frontend.ErrRateLimited):
		seconds := 1
		var limited *frontend.IdentityRateLimitError
		if errors.As(err, &limited) && limited.RetryAfter > 0 {
			seconds = max(1, min(900, int((limited.RetryAfter+time.Second-1)/time.Second)))
		}
		w.Header().Set("Retry-After", strconv.Itoa(seconds))
		send(w, http.StatusTooManyRequests, object{
			"error": object{
				"class":   "capacity",
				"code":    "rate_limited",
				"message": "rate limited",
				"retry":   object{"allowed": true, "afterSeconds": seconds, "maxAttempts": 2},
			},
		})
	case errors.Is(err, frontend.ErrIdentityUnavailable):
		problem(w, http.StatusForbidden, "policy", "identity_unavailable")
	case errors.Is(err, context.Canceled):
		problem(w, http.StatusRequestTimeout, "request", "request_canceled")
	default:
		send(w, http.StatusServiceUnavailable, object{
			"error": object{
				"class":   "transient",
				"code":    "plex_unreachable",
				"message": "plex.tv validation unavailable",
				"retry":   object{"allowed": false},
			},
		})
	}
}

func (i *instance) trustedSecureOrigin(r *http.Request, origin string) bool {
	if i.host.SecureOrigins == nil {
		return false
	}
	for _, candidate := range i.host.SecureOrigins(r.Context(), r) {
		if candidate == origin {
			return true
		}
	}
	return false
}
