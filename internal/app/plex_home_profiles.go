package app

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"sort"
	"strconv"
	"strings"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/plextv"
)

// plexHomeTV is the plex.tv surface Plex Home profile selection needs.
// *plextv.Client implements it; tests drive it through a fake transport.
type plexHomeTV interface {
	HomeUsers(context.Context, string) ([]plextv.HomeUser, error)
	HomeUserToken(context.Context, string, int64, string) (string, error)
	HomeUserAvatar(context.Context, string, string) ([]byte, string, error)
}

// plexHomeUsers is the auth surface Plex Home profile selection needs.
type plexHomeUsers interface {
	plexAccountAuthenticator
	ActivePlexUserByPlexID(int64) *frontend.User
	OwnerPlexID() int64
	HomeMember(int64) bool
	PlexLinkID(context.Context, int64, int64) *int64
}

func (s surfaceIdentity) plexTV() plexHomeTV {
	if s.app == nil || s.app.plextv == nil {
		return nil
	}
	return s.app.plextv
}

func (s surfaceIdentity) plexHomeSource() (plexHomeUsers, plexHomeTV, bool) {
	users, ok := s.users.(plexHomeUsers)
	if !ok {
		return nil, nil, false
	}
	tv := s.plexTV()
	if tv == nil {
		return nil, nil, false
	}
	return users, tv, true
}

// plexHomeProfilesSupported reports whether every piece of the flow works:
// native grants, a configured Plex identity, the plex.tv client, and the
// retained server/owner token the signed-in switch switches within.
func (s surfaceIdentity) plexHomeProfilesSupported() bool {
	if s.policy.ExternalIdentity || s.grants == nil || s.login == nil || !s.plexConfigured() {
		return false
	}
	if _, _, ok := s.plexHomeSource(); !ok {
		return false
	}
	return s.retainedPlexToken() != ""
}

func (s surfaceIdentity) PlexHomeSwitchAvailable(viewer *core.Viewer) (available, known bool) {
	source, ok := s.users.(frontend.HouseholdProfileAvailability)
	if !ok {
		return false, false
	}
	return source.PlexHomeSwitchAvailable(viewer)
}

func (s surfaceIdentity) retainedPlexToken() string {
	if s.hostConfig == nil {
		return ""
	}
	return strings.TrimSpace(s.hostConfig().Plex.Token)
}

func plexHomeProfileName(u *frontend.User, home plextv.HomeUser) string {
	for _, name := range []string{u.Title, u.Username, home.DisplayName()} {
		if strings.TrimSpace(name) != "" {
			return name
		}
	}
	return ""
}

// eligibleHouseholdProfiles intersects a Plex Home's members with active
// Ether viewers. Only active viewers are selectable.
func eligibleHouseholdProfiles(users plexHomeUsers, members []plextv.HomeUser) []frontend.HouseholdProfile {
	out := make([]frontend.HouseholdProfile, 0, len(members))
	seen := map[int64]bool{}
	for _, m := range members {
		if m.ID <= 0 || seen[m.ID] {
			continue
		}
		seen[m.ID] = true
		u := users.ActivePlexUserByPlexID(m.ID)
		if u == nil || u.ID <= 0 {
			continue
		}
		thumb := m.Thumb
		if thumb == "" {
			thumb = u.Thumb
		}
		out = append(out, frontend.HouseholdProfile{
			ViewerID:   u.ID,
			PlexID:     m.ID,
			Name:       plexHomeProfileName(u, m),
			Thumb:      thumb,
			Protected:  m.Protected,
			Restricted: m.Restricted,
		})
	}
	ownerPlexID := users.OwnerPlexID()
	sort.SliceStable(out, func(i, j int) bool {
		if oi, oj := out[i].PlexID == ownerPlexID, out[j].PlexID == ownerPlexID; oi != oj {
			return oi
		}
		ni, nj := strings.ToLower(out[i].Name), strings.ToLower(out[j].Name)
		if ni != nj {
			return ni < nj
		}
		return out[i].ViewerID < out[j].ViewerID
	})
	return out
}

// authenticatePlexAccountForHome mirrors SignInWithPlex's admission gates
// (client IP, login limiter, server-token refusal) without issuing a grant.
func (s surfaceIdentity) authenticatePlexAccountForHome(ctx context.Context, plexToken, appName string) (*frontend.User, int64, error) {
	if s.policy.ExternalIdentity || s.grants == nil || s.login == nil || s.users == nil {
		return nil, 0, core.ErrUnauthorized
	}
	ip, ok := frontend.ClientIP(ctx)
	if !ok || ip.IsUnspecified() || ip.IsMulticast() {
		return nil, 0, core.ErrUnauthorized
	}
	exceeded, err := s.login.IPExceeded(ctx, ip)
	if err != nil {
		return nil, 0, err
	}
	if exceeded {
		return nil, 0, s.allowApp(ctx, s.login, ip.String(), appName)
	}
	if cfgTok := s.retainedPlexToken(); cfgTok != "" && subtle.ConstantTimeCompare([]byte(strings.TrimSpace(plexToken)), []byte(cfgTok)) == 1 {
		if err := s.allowApp(ctx, s.login, ip.String(), appName); err != nil {
			return nil, 0, err
		}
		return nil, 0, core.ErrUnauthorized
	}
	plexAuth, ok := s.users.(plexAccountAuthenticator)
	if !ok {
		if err := s.allowApp(ctx, s.login, ip.String(), appName); err != nil {
			return nil, 0, err
		}
		return nil, 0, core.ErrUnauthorized
	}
	user, plexID, authErr := plexAuth.AuthenticatePlexAccount(ctx, plexToken)
	key := ip.String()
	if plexID != 0 {
		key = "plex:" + strconv.FormatInt(plexID, 10)
	}
	if err := s.allowApp(ctx, s.login, key, appName); err != nil {
		return nil, plexID, err
	}
	if authErr != nil {
		switch {
		case errors.Is(authErr, frontend.ErrPlexNotShared):
			return nil, plexID, frontend.ErrPlexNotShared
		case errors.Is(authErr, frontend.ErrPlexUnreachable):
			return nil, plexID, frontend.ErrPlexUnreachable
		case errors.Is(authErr, context.Canceled):
			return nil, plexID, authErr
		default:
			return nil, plexID, core.ErrUnauthorized
		}
	}
	if user == nil || user.ID <= 0 {
		return nil, plexID, core.ErrUnauthorized
	}
	return user, plexID, nil
}

// HouseholdProfiles authenticates the initiating token, then lists the
// active Ether viewers in that account's Plex Home. When plex.tv offers no
// Home listing for the account (a shared friend with no Home of their own
// on this server), the only eligible profile is the initiating viewer, so
// callers fall back to an ordinary sign-in.
func (s surfaceIdentity) HouseholdProfiles(ctx context.Context, plexToken string) ([]frontend.HouseholdProfile, error) {
	users, tv, ok := s.plexHomeSource()
	if !ok {
		return nil, frontend.ErrIdentityUnavailable
	}
	initiator, plexID, err := s.authenticatePlexAccountForHome(ctx, plexToken, "")
	if err != nil {
		return nil, err
	}
	members, err := tv.HomeUsers(ctx, plexToken)
	if err != nil {
		if errors.Is(err, core.ErrUnauthorized) || errors.Is(err, core.ErrNotFound) {
			return []frontend.HouseholdProfile{{
				ViewerID: initiator.ID, PlexID: plexID, Thumb: initiator.Thumb,
				Name: plexHomeProfileName(initiator, plextv.HomeUser{}),
			}}, nil
		}
		return nil, frontend.ErrPlexUnreachable
	}
	s.syncHomeThumbs(ctx, members)
	profiles := eligibleHouseholdProfiles(users, members)
	if len(profiles) == 0 {
		// The initiating viewer authenticated, so it is eligible even when
		// plex.tv's Home listing does not name it (defensive; keeps the
		// one-profile shortcut honest instead of failing a valid sign-in).
		profiles = []frontend.HouseholdProfile{{
			ViewerID: initiator.ID, PlexID: plexID, Thumb: initiator.Thumb,
			Name: plexHomeProfileName(initiator, plextv.HomeUser{}),
		}}
	}
	return profiles, nil
}

// HouseholdProfilesForViewer lists the profiles a signed-in viewer may switch
// to. Switch is offered only inside the server owner's Plex Home — the one
// Home Ether holds a retained account link for — so the viewer must be the
// owner or an active Home member of it.
func (s surfaceIdentity) HouseholdProfilesForViewer(ctx context.Context, viewer *core.Viewer) ([]frontend.HouseholdProfile, error) {
	users, tv, ok := s.plexHomeSource()
	if !ok || viewer == nil || viewer.ID <= 0 {
		return nil, frontend.ErrIdentityUnavailable
	}
	if s.policy.ExternalIdentity || s.grants == nil || s.login == nil {
		return nil, core.ErrUnauthorized
	}
	if err := s.allow(ctx, s.login, "plex-home-switch"); err != nil {
		return nil, err
	}
	// Re-read the viewer so a paused or unshared viewer cannot switch.
	current := users.ActivePlexUserByPlexID(viewer.PlexID)
	if viewer.PlexID <= 0 || current == nil || current.ID != viewer.ID {
		return nil, frontend.ErrHouseholdProfileIneligible
	}
	if !current.IsOwner && !users.HomeMember(current.ID) {
		return nil, frontend.ErrHouseholdProfileIneligible
	}
	// The retained link must exist for this viewer's Plex account.
	if link := users.PlexLinkID(ctx, current.ID, current.PlexID); link == nil {
		return nil, frontend.ErrHouseholdProfileIneligible
	}
	token := s.retainedPlexToken()
	if token == "" {
		return nil, frontend.ErrIdentityUnavailable
	}
	members, err := tv.HomeUsers(ctx, token)
	if err != nil {
		return nil, frontend.ErrPlexUnreachable
	}
	s.syncHomeThumbs(ctx, members)
	return eligibleHouseholdProfiles(users, members), nil
}

// SignInWithHouseholdProfile completes a selection. For an initiating-token
// selection it re-authenticates that token; for a signed-in switch it
// re-checks the current viewer's Home membership. A target other than the
// initiating account is verified through the plex.tv Home switch, which is
// where a protected profile's PIN is checked.
func (s surfaceIdentity) SignInWithHouseholdProfile(ctx context.Context, req frontend.HouseholdProfileSignIn) (string, error) {
	users, tv, ok := s.plexHomeSource()
	if !ok {
		return "", frontend.ErrIdentityUnavailable
	}
	if s.policy.ExternalIdentity || s.grants == nil || s.login == nil {
		return "", core.ErrUnauthorized
	}
	if req.Profile.ViewerID <= 0 || req.Profile.PlexID <= 0 {
		return "", frontend.ErrHouseholdProfileIneligible
	}
	if req.Profile.Protected && req.PIN == "" {
		return "", frontend.ErrHouseholdProfilePIN
	}
	// The selected viewer must still be the active viewer for that Plex id.
	target := users.ActivePlexUserByPlexID(req.Profile.PlexID)
	if target == nil || target.ID != req.Profile.ViewerID {
		return "", frontend.ErrHouseholdProfileIneligible
	}

	appName := s.surfaceAppName(req.Device)
	var switchToken string
	var initiatorPlexID int64
	if strings.TrimSpace(req.PlexToken) != "" {
		initiator, plexID, err := s.authenticatePlexAccountForHome(ctx, req.PlexToken, appName)
		if err != nil {
			return "", err
		}
		_ = initiator
		initiatorPlexID = plexID
		switchToken = req.PlexToken
		// The target must be in the initiating account's Home right now.
		members, err := tv.HomeUsers(ctx, req.PlexToken)
		if err != nil {
			if req.Profile.PlexID != initiatorPlexID {
				return "", frontend.ErrPlexUnreachable
			}
		} else if !plexHomeContains(members, req.Profile.PlexID) {
			return "", frontend.ErrHouseholdProfileIneligible
		} else {
			s.syncHomeThumbs(ctx, members)
		}
	} else {
		// Signed-in switch: the current viewer must still belong to the
		// owner's Home, and the target must be listed in that Home.
		if req.ViewerID <= 0 {
			return "", core.ErrUnauthorized
		}
		if err := s.allowApp(ctx, s.login, "plex-home-switch", appName); err != nil {
			return "", err
		}
		current, err := s.ActiveUser(ctx, req.ViewerID)
		if err != nil || current == nil || current.PlexID <= 0 {
			return "", frontend.ErrHouseholdProfileIneligible
		}
		if !current.IsOwner && !users.HomeMember(current.ID) {
			return "", frontend.ErrHouseholdProfileIneligible
		}
		if link := users.PlexLinkID(ctx, current.ID, current.PlexID); link == nil {
			return "", frontend.ErrHouseholdProfileIneligible
		}
		initiatorPlexID = current.PlexID
		switchToken = s.retainedPlexToken()
		if switchToken == "" {
			return "", frontend.ErrIdentityUnavailable
		}
		members, err := tv.HomeUsers(ctx, switchToken)
		if err != nil {
			return "", frontend.ErrPlexUnreachable
		}
		if !plexHomeContains(members, req.Profile.PlexID) {
			return "", frontend.ErrHouseholdProfileIneligible
		}
		s.syncHomeThumbs(ctx, members)
	}

	if req.Profile.PlexID != initiatorPlexID {
		// Verify through plex.tv. HomeUserToken confirms the switched
		// account is exactly the selected Plex user; the returned account
		// token is used only for that confirmation and never stored.
		if _, err := tv.HomeUserToken(ctx, switchToken, req.Profile.PlexID, req.PIN); err != nil {
			switch {
			case errors.Is(err, plextv.ErrHomePINIncorrect), errors.Is(err, plextv.ErrHomePINRequired):
				return "", frontend.ErrHouseholdProfilePIN
			case errors.Is(err, plextv.ErrPlexRefused):
				if req.Profile.Protected {
					return "", frontend.ErrHouseholdProfilePIN
				}
				return "", frontend.ErrHouseholdProfileIneligible
			default:
				return "", frontend.ErrPlexUnreachable
			}
		}
	} else if req.PIN != "" && req.Profile.Protected {
		// Selecting the initiating protected account itself still proves
		// the PIN through plex.tv rather than trusting possession alone
		// when the client supplied one.
		if _, err := tv.HomeUserToken(ctx, switchToken, req.Profile.PlexID, req.PIN); err != nil {
			if errors.Is(err, plextv.ErrHomePINIncorrect) || errors.Is(err, plextv.ErrHomePINRequired) {
				return "", frontend.ErrHouseholdProfilePIN
			}
			// A switch-to-self refusal is not fatal: the initiating token
			// already authenticated as this viewer above.
		}
	}

	linkID := users.PlexLinkID(ctx, target.ID, target.PlexID)
	tok, err := s.grants.Issue(ctx, target.ID, linkID, string(s.surface), req.Device)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(req.PlexToken) != "" && initiatorPlexID != 0 {
		s.resetIdentity(ctx, "plex:"+strconv.FormatInt(initiatorPlexID, 10))
	}
	if s.app != nil {
		remoteIP := ""
		if ip, ok := frontend.ClientIP(ctx); ok && ip.IsValid() {
			remoteIP = ip.String()
		}
		clientName := req.Device.Product
		if clientName == "" {
			clientName = req.Device.Client
			if clientName == "" {
				clientName = req.Device.DeviceName
			}
		}
		s.app.emitLoginSucceeded(target.ID, target.Username, "plex", clientName, string(s.surface), remoteIP)
	}
	return tok, nil
}

// FetchHouseholdAvatar fetches the selected profile's avatar bytes through
// the plex.tv client, which confines the thumb URL to Plex avatar hosts.
// The credential mirrors completion: the initiating token when present,
// otherwise the retained server/owner token for a signed-in switch. The
// profile must still be the active viewer for its Plex account.
func (s surfaceIdentity) FetchHouseholdAvatar(ctx context.Context, req frontend.HouseholdAvatarRequest) ([]byte, string, error) {
	users, _, ok := s.plexHomeSource()
	if !ok {
		return nil, "", frontend.ErrIdentityUnavailable
	}
	if s.policy.ExternalIdentity || req.Profile.ViewerID <= 0 || req.Profile.PlexID <= 0 || strings.TrimSpace(req.Profile.Thumb) == "" {
		return nil, "", frontend.ErrHouseholdProfileIneligible
	}
	target := users.ActivePlexUserByPlexID(req.Profile.PlexID)
	if target == nil || target.ID != req.Profile.ViewerID {
		return nil, "", frontend.ErrHouseholdProfileIneligible
	}
	token := strings.TrimSpace(req.PlexToken)
	if token == "" {
		token = s.retainedPlexToken()
	}
	if token == "" {
		return nil, "", frontend.ErrIdentityUnavailable
	}
	return s.app.cachedPlexAvatar(ctx, token, req.Profile.Thumb)
}

type plexThumbSyncer interface {
	SyncPlexThumbs(context.Context, map[int64]string) error
}

func (s surfaceIdentity) syncHomeThumbs(ctx context.Context, members []plextv.HomeUser) {
	syncer, ok := s.users.(plexThumbSyncer)
	if !ok || len(members) == 0 {
		return
	}
	thumbs := make(map[int64]string, len(members))
	for _, m := range members {
		if m.ID > 0 && strings.TrimSpace(m.Thumb) != "" {
			thumbs[m.ID] = m.Thumb
		}
	}
	if len(thumbs) == 0 {
		return
	}
	_ = syncer.SyncPlexThumbs(ctx, thumbs)
}

// cachedPlexAvatar fetches one Plex account or Plex Home profile avatar once
// through the plex.tv client and retains it in Ether's shared image cache.
func (a *App) cachedPlexAvatar(ctx context.Context, token, thumb string) ([]byte, string, error) {
	if a == nil || a.plextv == nil {
		return nil, "", frontend.ErrIdentityUnavailable
	}
	thumb = strings.TrimSpace(thumb)
	if thumb == "" {
		return nil, "", core.ErrNotFound
	}
	load := func(fctx context.Context) ([]byte, string, error) {
		body, ct, err := a.plextv.HomeUserAvatar(fctx, token, thumb)
		if err != nil {
			return nil, "", frontend.ErrPlexUnreachable
		}
		return body, ct, nil
	}
	if a.images == nil {
		return load(ctx)
	}
	sum := sha256.Sum256([]byte("plex-avatar-v1\x00" + thumb))
	return a.images.GetLoaded(ctx, "plex-avatar/"+hex.EncodeToString(sum[:]), load)
}

func plexHomeContains(members []plextv.HomeUser, plexID int64) bool {
	for _, m := range members {
		if m.ID == plexID {
			return true
		}
	}
	return false
}

var _ frontend.HouseholdProfileAuth = surfaceIdentity{}
