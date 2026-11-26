package vela

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/kit/ids"
)

const (
	plexSelectionTTL         = 5 * time.Minute
	plexSelectionMaxPIN      = 5
	plexSelectionMaxRetained = 1000
)

// plexSelection is one in-progress Plex Home profile selection. It lives
// only in memory: the initiating Plex account token is held here so the
// client never sends it again, and it is never logged or persisted.
type plexSelection struct {
	plexToken string // initiating account token; empty for a signed-in switch
	viewerID  int64  // signed-in switch: the current viewer
	oldAccess string // signed-in switch: session to revoke on completion
	isSwitch  bool
	device    frontend.Device
	profiles  []frontend.HouseholdProfile
	expires   time.Time
	attempts  int
	busy      bool
	consumed  bool
}

// plexHomeAuth returns the profile-selection identity only when the host
// both implements it and advertises the feature as fully supported.
func (i *instance) plexHomeAuth() (frontend.HouseholdProfileAuth, bool) {
	auth, ok := i.host.Identity.(frontend.HouseholdProfileAuth)
	if !ok {
		return nil, false
	}
	source, ok := i.host.Identity.(frontend.AuthenticationFeatureSource)
	if !ok || !source.AuthenticationFeatures().HouseholdProfiles {
		return nil, false
	}
	return auth, true
}

func (i *instance) plexProfilesWire(selectionKey string, expires time.Time, profiles []frontend.HouseholdProfile) []object {
	out := make([]object, 0, len(profiles))
	for _, p := range profiles {
		id, err := ids.GUID(ids.User, p.ViewerID)
		if err != nil {
			continue
		}
		profile := object{"id": id, "name": p.Name, "protected": p.Protected}
		if p.Restricted {
			profile["restricted"] = true
		}
		if avatar := i.plexAvatarURL(selectionKey, expires, p); avatar != "" {
			profile["avatarUrl"] = avatar
		}
		out = append(out, profile)
	}
	return out
}

// plexAvatarPrefix marks an images-path secret as a profile-selection
// avatar capability rather than an artwork grant.
const plexAvatarPrefix = "plexavatar-"

const plexAvatarDomain = "vela-plex-avatar"

type plexAvatarClaim struct {
	Selection string `json:"s"`
	ViewerID  int64  `json:"v"`
	Expires   int64  `json:"e"`
}

// plexAvatarURL seals a selection-bound avatar capability into the images
// path. It carries no thumb URL and no credential, and it expires with
// the selection; the bytes are proxied by plexAvatar below. Profiles
// without a plausible absolute thumb URL get no avatarUrl.
func (i *instance) plexAvatarURL(selectionKey string, expires time.Time, p frontend.HouseholdProfile) string {
	if i.seal == nil || selectionKey == "" || p.ViewerID <= 0 {
		return ""
	}
	u, err := url.Parse(strings.TrimSpace(p.Thumb))
	if err != nil || u.Host == "" || u.User != nil || (u.Scheme != "https" && u.Scheme != "http") {
		return ""
	}
	b, err := json.Marshal(plexAvatarClaim{Selection: selectionKey, ViewerID: p.ViewerID, Expires: expires.Unix()})
	if err != nil {
		return ""
	}
	nonce, err := i.cursorNonce(plexAvatarDomain, b)
	if err != nil {
		return ""
	}
	sealed := base64.RawURLEncoding.EncodeToString(i.seal.Seal(nonce, nonce, b, []byte(plexAvatarDomain)))
	return "/vela/v0/images/" + plexAvatarPrefix + sealed
}

func (i *instance) readPlexAvatarClaim(secret string) (plexAvatarClaim, bool) {
	var claim plexAvatarClaim
	token := strings.TrimPrefix(secret, plexAvatarPrefix)
	if token == secret || token == "" || len(token) > 4096 || i.seal == nil {
		return claim, false
	}
	b, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil || len(b) < i.seal.NonceSize() {
		return claim, false
	}
	b, err = i.seal.Open(nil, b[:i.seal.NonceSize()], b[i.seal.NonceSize():], []byte(plexAvatarDomain))
	if err != nil || json.Unmarshal(b, &claim) != nil || claim.Selection == "" || claim.ViewerID <= 0 {
		return plexAvatarClaim{}, false
	}
	return claim, true
}

// plexAvatar proxies one profile avatar for a live selection. It is
// usable before the client has a session: the sealed capability, bound
// to the selection and expiring with it, is the only credential. The
// upstream thumb URL and any Plex token stay server-side.
func (i *instance) plexAvatar(w http.ResponseWriter, r *http.Request, secret string) {
	claim, ok := i.readPlexAvatarClaim(secret)
	if !ok {
		problem(w, http.StatusNotFound, "not-found", "not_found")
		return
	}
	if claim.Expires <= time.Now().Unix() {
		plexSelectionGone(w, false)
		return
	}
	i.plexSelMu.Lock()
	sel := i.plexSels[claim.Selection]
	if sel == nil {
		i.plexSelMu.Unlock()
		plexSelectionGone(w, false)
		return
	}
	if time.Now().After(sel.expires) {
		delete(i.plexSels, claim.Selection)
		i.plexSelMu.Unlock()
		plexSelectionGone(w, false)
		return
	}
	if sel.consumed {
		i.plexSelMu.Unlock()
		plexSelectionGone(w, true)
		return
	}
	var profile *frontend.HouseholdProfile
	for idx := range sel.profiles {
		if sel.profiles[idx].ViewerID == claim.ViewerID {
			p := sel.profiles[idx]
			profile = &p
			break
		}
	}
	plexToken, viewerID := sel.plexToken, sel.viewerID
	i.plexSelMu.Unlock()
	if profile == nil || strings.TrimSpace(profile.Thumb) == "" {
		problem(w, http.StatusNotFound, "not-found", "not_found")
		return
	}
	auth, ok := i.plexHomeAuth()
	if !ok {
		problem(w, http.StatusNotFound, "not-found", "not_found")
		return
	}
	body, contentType, err := auth.FetchHouseholdAvatar(i.identityContext(r), frontend.HouseholdAvatarRequest{
		PlexToken: plexToken,
		ViewerID:  viewerID,
		Profile:   *profile,
	})
	if err != nil || len(body) == 0 || !strings.HasPrefix(strings.ToLower(contentType), "image/") {
		problem(w, http.StatusBadGateway, "transient", "avatar_unavailable")
		return
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.Header().Set("Cache-Control", "private, no-store")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}

func (i *instance) newPlexSelection(sel *plexSelection) (string, bool) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", false
	}
	token := hex.EncodeToString(raw)
	sel.expires = time.Now().Add(plexSelectionTTL)
	i.plexSelMu.Lock()
	defer i.plexSelMu.Unlock()
	if i.plexSels == nil {
		i.plexSels = map[string]*plexSelection{}
	}
	now := time.Now()
	for k, old := range i.plexSels {
		if now.After(old.expires) {
			delete(i.plexSels, k)
		}
	}
	if len(i.plexSels) >= plexSelectionMaxRetained {
		return "", false
	}
	i.plexSels[hash(token)] = sel
	return token, true
}

func (i *instance) sendPlexSelection(w http.ResponseWriter, sel *plexSelection) {
	token, ok := i.newPlexSelection(sel)
	if !ok {
		problem(w, http.StatusServiceUnavailable, "capacity", "selection_unavailable")
		return
	}
	send(w, http.StatusOK, object{
		"selectionToken": token,
		"expiresAt":      sel.expires.UTC().Format(time.RFC3339),
		"profiles":       i.plexProfilesWire(hash(token), sel.expires, sel.profiles),
	})
}

// plexProfileFail maps identity errors from the profile flow onto the wire.
// The Plex-specific statuses reuse plexAuthFail's shapes.
func plexProfileFail(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, frontend.ErrHouseholdProfilePIN):
		send(w, http.StatusUnauthorized, object{"error": object{
			"class": "auth", "code": "invalid_pin",
			"message": "incorrect PIN", "retry": object{"allowed": true},
		}})
	case errors.Is(err, frontend.ErrHouseholdProfileIneligible):
		send(w, http.StatusForbidden, object{"error": object{
			"class": "policy", "code": "profile_ineligible",
			"message": "this profile cannot sign in here", "retry": object{"allowed": false},
		}})
	default:
		plexAuthFail(w, err)
	}
}

func plexSelectionGone(w http.ResponseWriter, consumed bool) {
	if consumed {
		send(w, http.StatusConflict, object{"error": object{
			"class": "conflict", "code": "selection_consumed",
			"message": "this profile selection was already used", "retry": object{"allowed": false},
		}})
		return
	}
	send(w, http.StatusGone, object{"error": object{
		"class": "conflict", "code": "selection_expired",
		"message": "this profile selection expired", "retry": object{"allowed": false},
	}})
}

// plexSignInSelect handles POST /auth/plex with selectProfile: true.
func (i *instance) plexSignInSelect(w http.ResponseWriter, r *http.Request, plexToken string, d frontend.Device) {
	ctx := i.identityContext(r)
	auth, ok := i.plexHomeAuth()
	if !ok {
		// Not fully supported here: keep today's ordinary behaviour.
		a, e := i.host.Identity.SignInWithPlex(ctx, plexToken, d)
		if e != nil {
			plexAuthFail(w, e)
			return
		}
		i.tokens(w, r, a, "", d)
		return
	}
	profiles, err := auth.HouseholdProfiles(ctx, plexToken)
	if err != nil {
		plexProfileFail(w, err)
		return
	}
	if len(profiles) == 0 {
		plexProfileFail(w, frontend.ErrHouseholdProfileIneligible)
		return
	}
	if len(profiles) == 1 {
		a, e := i.host.Identity.SignInWithPlex(ctx, plexToken, d)
		if e != nil {
			plexProfileFail(w, e)
			return
		}
		i.tokens(w, r, a, "", d)
		return
	}
	i.sendPlexSelection(w, &plexSelection{plexToken: plexToken, device: d, profiles: profiles})
}

// plexProfileChallenge handles the authenticated switch-profile start.
func (i *instance) plexProfileChallenge(w http.ResponseWriter, r *http.Request, d frontend.Device) {
	auth, ok := i.plexHomeAuth()
	if !ok {
		i.unavailable(w, r)
		return
	}
	v, access, err := i.viewer(r)
	if err != nil {
		fail(w, err, r)
		return
	}
	if d.ID != requestDevice(r).ID {
		problem(w, http.StatusBadRequest, "request", "invalid_device")
		return
	}
	profiles, err := auth.HouseholdProfilesForViewer(i.identityContext(r), v)
	if err != nil {
		if errors.Is(err, frontend.ErrIdentityUnavailable) {
			i.unavailable(w, r)
			return
		}
		plexProfileFail(w, err)
		return
	}
	if len(profiles) < 2 {
		problem(w, http.StatusConflict, "conflict", "no_other_profiles")
		return
	}
	i.sendPlexSelection(w, &plexSelection{
		viewerID: v.ID, oldAccess: access, isSwitch: true, device: d, profiles: profiles,
	})
}

// plexProfileComplete handles POST /auth/plex/profile.
func (i *instance) plexProfileComplete(w http.ResponseWriter, r *http.Request, selectionToken, profileID, pin string, d frontend.Device) {
	auth, ok := i.plexHomeAuth()
	if !ok {
		i.unavailable(w, r)
		return
	}
	if selectionToken == "" || profileID == "" {
		problem(w, http.StatusBadRequest, "request", "invalid_request")
		return
	}
	key := hash(selectionToken)
	i.plexSelMu.Lock()
	sel := i.plexSels[key]
	if sel == nil {
		i.plexSelMu.Unlock()
		plexSelectionGone(w, false)
		return
	}
	if time.Now().After(sel.expires) {
		delete(i.plexSels, key)
		i.plexSelMu.Unlock()
		plexSelectionGone(w, false)
		return
	}
	if sel.consumed || sel.busy {
		i.plexSelMu.Unlock()
		plexSelectionGone(w, true)
		return
	}
	if sel.device.ID != d.ID {
		i.plexSelMu.Unlock()
		send(w, http.StatusForbidden, object{"error": object{
			"class": "policy", "code": "selection_device_mismatch",
			"message": "this profile selection belongs to another device", "retry": object{"allowed": false},
		}})
		return
	}
	viewerID, err := ids.ParseGUID(ids.User, profileID)
	var profile *frontend.HouseholdProfile
	if err == nil {
		for idx := range sel.profiles {
			if sel.profiles[idx].ViewerID == viewerID {
				p := sel.profiles[idx]
				profile = &p
				break
			}
		}
	}
	if profile == nil {
		i.plexSelMu.Unlock()
		plexProfileFail(w, frontend.ErrHouseholdProfileIneligible)
		return
	}
	if profile.Protected {
		if sel.attempts >= plexSelectionMaxPIN {
			sel.consumed = true
			i.plexSelMu.Unlock()
			w.Header().Set("Retry-After", "300")
			send(w, http.StatusTooManyRequests, object{"error": object{
				"class": "capacity", "code": "rate_limited",
				"message": "too many PIN attempts; start again",
				"retry":   object{"allowed": true, "afterSeconds": 300, "maxAttempts": 1},
			}})
			return
		}
		sel.attempts++
	}
	sel.busy = true
	snapshot := *sel
	i.plexSelMu.Unlock()

	access, signErr := auth.SignInWithHouseholdProfile(i.identityContext(r), frontend.HouseholdProfileSignIn{
		PlexToken: snapshot.plexToken,
		ViewerID:  snapshot.viewerID,
		Profile:   *profile,
		PIN:       pin,
		Device:    d,
	})

	i.plexSelMu.Lock()
	if cur := i.plexSels[key]; cur != nil {
		cur.busy = false
		if signErr == nil {
			cur.consumed = true
			cur.plexToken = ""
		}
	}
	i.plexSelMu.Unlock()

	if signErr != nil {
		plexProfileFail(w, signErr)
		return
	}
	if snapshot.isSwitch && snapshot.oldAccess != "" && snapshot.oldAccess != access {
		// Replace the old session; a failed revoke must not strand the
		// viewer without the new session they just proved.
		_ = i.host.Identity.Revoke(i.identityContext(r), snapshot.oldAccess)
	}
	i.tokens(w, r, access, "", d)
}
