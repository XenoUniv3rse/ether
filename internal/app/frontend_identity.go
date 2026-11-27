package app

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/identity"
)

type surfaceIdentity struct {
	app         *App
	policy      core.FrontendHostPolicy
	settings    func() frontend.Settings
	hostConfig  func() frontend.HostConfig
	surface     frontend.Protocol
	users       core.Auth
	resolver    *identity.Resolver
	local       *identity.Local
	grants      *identity.Grants
	apiKeys     *identity.APIKeys
	quick       *identity.QuickConnect
	times       *identity.ViewerTimes
	login, poll *identity.LoginLimiter
}

func (s surfaceIdentity) Resolve(ctx context.Context, credential string) (*core.Viewer, error) {
	if s.resolver == nil {
		return nil, core.ErrUnauthorized
	}
	return s.resolver.Resolve(ctx, string(s.surface), credential)
}

func (s surfaceIdentity) ResolveAPIKey(ctx context.Context, key string) (*core.Viewer, frontend.APIKeyInfo, error) {
	if s.apiKeys == nil || (s.surface != "jellyfin" && s.surface != "emby") {
		return nil, frontend.APIKeyInfo{}, core.ErrUnauthorized
	}
	return s.apiKeys.Resolve(ctx, string(s.surface), key)
}

func (s surfaceIdentity) ListAPIKeys(ctx context.Context) ([]frontend.APIKeyInfo, error) {
	if s.apiKeys == nil || (s.surface != "jellyfin" && s.surface != "emby") {
		return nil, core.ErrUnauthorized
	}
	return s.apiKeys.List(ctx, string(s.surface))
}

func (s surfaceIdentity) CreateAPIKey(ctx context.Context, appName string) (frontend.APIKeyInfo, string, error) {
	if s.apiKeys == nil || (s.surface != "jellyfin" && s.surface != "emby") {
		return frontend.APIKeyInfo{}, "", core.ErrUnauthorized
	}
	return s.apiKeys.Create(ctx, string(s.surface), appName)
}

func (s surfaceIdentity) RevokeAPIKey(ctx context.Context, keyOrHandle string) (int64, error) {
	if s.apiKeys == nil || (s.surface != "jellyfin" && s.surface != "emby") {
		return 0, core.ErrUnauthorized
	}
	return s.apiKeys.Revoke(ctx, string(s.surface), keyOrHandle)
}

func (s surfaceIdentity) Accept(ctx context.Context, token string) (*core.Viewer, frontend.CredentialAcceptance, error) {
	if !s.policy.ExternalIdentity || core.IsGrantCredential(token) || core.IsAPIKeyCredential(token) {
		return nil, frontend.CredentialAcceptance{}, core.ErrUnauthorized
	}
	if acc, ok := s.users.(frontend.CredentialAcceptor); ok {
		return acc.Accept(ctx, token)
	}
	u, err := s.Resolve(ctx, token)
	return u, frontend.CredentialAcceptance{}, err
}

func (s surfaceIdentity) ReceiptCurrent(ctx context.Context, r frontend.CredentialAcceptance) error {
	if !s.policy.ExternalIdentity {
		return core.ErrUnauthorized
	}
	if acc, ok := s.users.(frontend.CredentialAcceptor); ok {
		return acc.ReceiptCurrent(ctx, r)
	}
	return core.ErrUnauthorized
}

func (s surfaceIdentity) IssueDelegation(ctx context.Context, userID int64) (string, error) {
	if !s.policy.ExternalIdentity {
		return "", core.ErrUnauthorized
	}
	if del, ok := s.users.(frontend.DelegationAuth); ok {
		return del.IssueDelegation(ctx, userID)
	}
	return "", core.ErrUnauthorized
}

func (s surfaceIdentity) ActiveUser(ctx context.Context, id int64) (*core.User, error) {
	if act, ok := s.users.(frontend.ActiveUserSource); ok {
		return act.ActiveUser(ctx, id)
	}
	return nil, core.ErrUnauthorized
}

func (s surfaceIdentity) Users(ctx context.Context) ([]core.User, error) {
	if s.users == nil {
		return nil, nil
	}
	return s.users.Users(ctx)
}

func (s surfaceIdentity) User(ctx context.Context, id int64) (*core.User, error) {
	if src, ok := s.users.(frontend.UserLookupSource); ok {
		return src.User(ctx, id)
	}
	return nil, core.ErrNotFound
}

func (s surfaceIdentity) KnownUsers(ctx context.Context) ([]core.User, error) {
	if src, ok := s.users.(frontend.KnownUsersSource); ok {
		return src.KnownUsers(ctx)
	}
	return s.Users(ctx)
}

func (s surfaceIdentity) ViewerTimes(ctx context.Context, credential string, target int64) (frontend.ViewerTimes, error) {
	if s.times == nil || target <= 0 {
		return frontend.ViewerTimes{}, core.ErrUnauthorized
	}
	var (
		viewer *core.Viewer
		err    error
	)
	if core.IsAPIKeyCredential(credential) {
		viewer, _, err = s.ResolveAPIKey(frontend.WithQuietIdentity(ctx), credential)
	} else {
		viewer, err = s.Resolve(ctx, credential)
	}
	if err != nil {
		return frontend.ViewerTimes{}, err
	}
	if viewer == nil {
		return frontend.ViewerTimes{}, core.ErrUnauthorized
	}
	return s.times.Get(ctx, viewer.ID, target)
}

// ResolveDevice binds validation to the stored surface and device without writes.
func (s surfaceIdentity) ResolveDevice(ctx context.Context, credential string, device core.Device) (*core.Viewer, time.Time, error) {
	if s.surface == "" || s.policy.ExternalIdentity || s.grants == nil {
		return nil, time.Time{}, core.ErrUnauthorized
	}
	return s.grants.ResolveDevice(ctx, string(s.surface), credential, device)
}

func (s surfaceIdentity) ResolveGrantDevice(ctx context.Context, credential, claim string) (*core.Viewer, core.Device, time.Time, error) {
	if s.surface == "" || s.policy.ExternalIdentity || s.grants == nil {
		return nil, core.Device{}, time.Time{}, core.ErrUnauthorized
	}
	return s.grants.ResolveGrantDevice(ctx, string(s.surface), credential, claim)
}

func (s surfaceIdentity) surfaceAppName(device core.Device) string {
	if device.Product != "" {
		return device.Product
	}
	if device.Client != "" {
		return device.Client
	}
	if device.DeviceName != "" {
		return device.DeviceName
	}
	return surfaceDisplayName(string(s.surface))
}

func surfaceDisplayName(surface string) string {
	switch surface {
	case "jellyfin":
		return "Jellyfin"
	case "emby":
		return "Emby"
	case "silo":
		return "Silo"
	case "vela":
		return "Vela"
	case "xtream":
		return "Xtream"
	case "plex":
		return "Plex"
	case "stremio":
		return "Stremio"
	default:
		return surface
	}
}

func (s surfaceIdentity) allowApp(ctx context.Context, limiter *identity.LoginLimiter, key, app string) error {
	ip, ok := frontend.ClientIP(ctx)
	if !ok || ip.IsUnspecified() || ip.IsMulticast() {
		return core.ErrUnauthorized
	}
	if app == "" {
		app = surfaceDisplayName(string(s.surface))
	}
	allowed, retry, err := limiter.AllowWithRetry(identity.WithRateLimitApp(ctx, app), key, ip)
	if err != nil {
		return err
	}
	if !allowed {
		return &frontend.IdentityRateLimitError{RetryAfter: retry}
	}
	return nil
}

func (s surfaceIdentity) allow(ctx context.Context, limiter *identity.LoginLimiter, key string) error {
	return s.allowApp(ctx, limiter, key, "")
}

func (s surfaceIdentity) resetIdentity(ctx context.Context, key string) {
	if s.login == nil || key == "" {
		return
	}
	if ip, ok := frontend.ClientIP(ctx); ok && ip.IsValid() && !ip.IsUnspecified() && !ip.IsMulticast() {
		_ = s.login.ResetIdentity(ctx, key, ip)
	}
}

// PublicViewers returns only active local sign-in names when the owner enables the picker.
func (s surfaceIdentity) PublicViewers(ctx context.Context) ([]frontend.PublicViewer, error) {
	if s.policy.ExternalIdentity || s.settings == nil || s.local == nil {
		return []frontend.PublicViewer{}, nil
	}
	enabled, _ := s.settings()["publicViewerPicker"].(bool)
	if !enabled {
		return []frontend.PublicViewer{}, nil
	}
	if err := s.allow(ctx, s.login, "public-viewers"); err != nil {
		return nil, err
	}
	return s.local.PublicViewers(ctx)
}
func (s surfaceIdentity) SignIn(ctx context.Context, username, password string, device core.Device) (string, error) {
	if s.policy.ExternalIdentity {
		return "", core.ErrUnauthorized
	}
	remoteIP := ""
	if ip, ok := frontend.ClientIP(ctx); ok && ip.IsValid() {
		remoteIP = ip.String()
	}
	clientName := device.Product
	if clientName == "" {
		clientName = device.Client
		if clientName == "" {
			clientName = device.DeviceName
		}
	}
	appName := s.surfaceAppName(device)
	origUsername := username
	if err := s.allowApp(ctx, s.login, origUsername, appName); err != nil {
		s.app.emitLoginFailed(username, "password", "rate_limited", clientName, string(s.surface), remoteIP)
		return "", err
	}
	if core.IsGrantCredential(password) {
		if s.grants == nil {
			s.app.emitLoginFailed(username, "grant", "unavailable", clientName, string(s.surface), remoteIP)
			return "", core.ErrUnauthorized
		}
		tok, err := s.grants.SignInWithGrant(ctx, string(s.surface), username, password, device)
		if err != nil {
			s.app.emitLoginFailed(username, "grant", "invalid_credentials", clientName, string(s.surface), remoteIP)
			return "", err
		}
		s.resetIdentity(ctx, origUsername)
		var viewerID int64
		if v, rerr := s.Resolve(ctx, tok); rerr == nil && v != nil {
			viewerID = v.ID
			if v.Username != "" {
				username = v.Username
			}
		}
		s.app.emitLoginSucceeded(viewerID, username, "grant", clientName, string(s.surface), remoteIP)
		return tok, nil
	}
	viewer, link, err := s.local.Authenticate(ctx, username, password)
	if err != nil {
		s.app.emitLoginFailed(username, "password", "invalid_credentials", clientName, string(s.surface), remoteIP)
		return "", err
	}
	// The grant is the sign-in. Login timestamps are recorded after the
	// response so a large catalogue database cannot hold the client open.
	tok, err := s.grants.Issue(ctx, viewer, &link, string(s.surface), device)
	if err != nil {
		s.app.emitLoginFailed(username, "password", "grant_issue_failed", clientName, string(s.surface), remoteIP)
		return "", err
	}
	s.resetIdentity(ctx, origUsername)
	s.app.emitLoginSucceeded(viewer, username, "password", clientName, string(s.surface), remoteIP)
	return tok, nil
}
func (s surfaceIdentity) SignInWithPlex(ctx context.Context, plexToken string, device core.Device) (string, error) {
	if s.policy.ExternalIdentity || s.grants == nil || s.login == nil || s.users == nil {
		return "", core.ErrUnauthorized
	}
	ip, ok := frontend.ClientIP(ctx)
	if !ok || ip.IsUnspecified() || ip.IsMulticast() {
		return "", core.ErrUnauthorized
	}
	remoteIP := ip.String()
	clientName := device.Product
	if clientName == "" {
		clientName = device.Client
		if clientName == "" {
			clientName = device.DeviceName
		}
	}
	appName := s.surfaceAppName(device)
	exceeded, err := s.login.IPExceeded(ctx, ip)
	if err != nil {
		return "", err
	}
	if exceeded {
		s.app.emitLoginFailed("", "plex", "rate_limited", clientName, string(s.surface), remoteIP)
		return "", s.allowApp(ctx, s.login, ip.String(), appName)
	}
	if s.hostConfig != nil {
		if cfgTok := strings.TrimSpace(s.hostConfig().Plex.Token); cfgTok != "" && subtle.ConstantTimeCompare([]byte(strings.TrimSpace(plexToken)), []byte(cfgTok)) == 1 {
			if err := s.allowApp(ctx, s.login, ip.String(), appName); err != nil {
				s.app.emitLoginFailed("", "plex", "rate_limited", clientName, string(s.surface), remoteIP)
				return "", err
			}
			s.app.emitLoginFailed("", "plex", "invalid_credentials", clientName, string(s.surface), remoteIP)
			return "", core.ErrUnauthorized
		}
	}
	plexAuth, ok := s.users.(plexAccountAuthenticator)
	if !ok {
		if err := s.allowApp(ctx, s.login, ip.String(), appName); err != nil {
			return "", err
		}
		s.app.emitLoginFailed("", "plex", "unavailable", clientName, string(s.surface), remoteIP)
		return "", core.ErrUnauthorized
	}
	user, plexID, authErr := plexAuth.AuthenticatePlexAccount(ctx, plexToken)
	key := ip.String()
	if plexID != 0 {
		key = "plex:" + strconv.FormatInt(plexID, 10)
	}
	if err := s.allowApp(ctx, s.login, key, appName); err != nil {
		s.app.emitLoginFailed("", "plex", "rate_limited", clientName, string(s.surface), remoteIP)
		return "", err
	}
	if authErr != nil {
		if errors.Is(authErr, frontend.ErrPlexNotShared) {
			s.app.emitLoginFailed("", "plex", "not_shared", clientName, string(s.surface), remoteIP)
			return "", frontend.ErrPlexNotShared
		}
		if errors.Is(authErr, frontend.ErrPlexUnreachable) {
			s.app.emitLoginFailed("", "plex", "plex_unreachable", clientName, string(s.surface), remoteIP)
			return "", frontend.ErrPlexUnreachable
		}
		if errors.Is(authErr, context.Canceled) {
			return "", authErr
		}
		s.app.emitLoginFailed("", "plex", "invalid_credentials", clientName, string(s.surface), remoteIP)
		return "", core.ErrUnauthorized
	}
	if user == nil || user.ID <= 0 {
		s.app.emitLoginFailed("", "plex", "invalid_credentials", clientName, string(s.surface), remoteIP)
		return "", core.ErrUnauthorized
	}
	var linkID *int64
	if linker, ok := s.users.(interface {
		PlexLinkID(context.Context, int64, int64) *int64
	}); ok {
		linkID = linker.PlexLinkID(ctx, user.ID, plexID)
	}
	tok, err := s.grants.Issue(ctx, user.ID, linkID, string(s.surface), device)
	if err != nil {
		s.app.emitLoginFailed(user.Username, "plex", "grant_issue_failed", clientName, string(s.surface), remoteIP)
		return "", err
	}
	if plexID != 0 {
		s.resetIdentity(ctx, key)
	}
	s.app.emitLoginSucceeded(user.ID, user.Username, "plex", clientName, string(s.surface), remoteIP)
	return tok, nil
}
func (s surfaceIdentity) IssueRefresh(ctx context.Context, access string, device core.Device) (string, error) {
	return s.grants.IssueRefresh(ctx, string(s.surface), access, device)
}
func (s surfaceIdentity) Refresh(ctx context.Context, token string, device core.Device) (string, string, error) {
	if err := s.allowApp(ctx, s.poll, "refresh", s.surfaceAppName(device)); err != nil {
		return "", "", err
	}
	access, refresh, err := s.grants.Refresh(ctx, string(s.surface), token, device)
	if err == nil {
		_, _ = s.Resolve(ctx, access)
	}
	return access, refresh, err
}
func (s surfaceIdentity) QuickConnectInitiate(ctx context.Context, device core.Device) (string, string, error) {
	if s.policy.ExternalIdentity {
		return "", "", core.ErrUnauthorized
	}
	if err := s.allowApp(ctx, s.login, "quickconnect-init", s.surfaceAppName(device)); err != nil {
		return "", "", err
	}
	return s.quick.Initiate(ctx, string(s.surface), device)
}
func (s surfaceIdentity) QuickConnectPoll(ctx context.Context, secret string) (string, error) {
	if s.policy.ExternalIdentity {
		return "", core.ErrUnauthorized
	}
	if err := s.allow(ctx, s.poll, secret); err != nil {
		return "", err
	}
	grant, err := s.quick.PollSurface(ctx, string(s.surface), secret)
	if errors.Is(err, identity.ErrQuickConnectPending) {
		return "", frontend.ErrQuickConnectPending
	}
	return grant, err
}

// QuickConnectPollDevice redeems or replays a secret for one device.
func (s surfaceIdentity) QuickConnectPollDevice(ctx context.Context, secret string, device core.Device) (string, error) {
	if s.policy.ExternalIdentity {
		return "", core.ErrUnauthorized
	}
	if err := s.allowApp(ctx, s.poll, secret, s.surfaceAppName(device)); err != nil {
		return "", err
	}
	grant, err := s.quick.PollSurfaceDevice(ctx, string(s.surface), secret, device)
	if errors.Is(err, identity.ErrQuickConnectPending) {
		return "", frontend.ErrQuickConnectPending
	}
	return grant, err
}

// NoteSignIn records a sign-in after the client has the token.
func (s surfaceIdentity) NoteSignIn(ctx context.Context, viewerID int64) error {
	if s.times == nil {
		return nil
	}
	return s.times.RecordSignIn(ctx, viewerID)
}

func (s surfaceIdentity) QuickConnectStatus(ctx context.Context, secret string) (bool, error) {
	if s.policy.ExternalIdentity {
		return false, core.ErrUnauthorized
	}
	if err := s.allow(ctx, s.poll, secret); err != nil {
		return false, err
	}
	return s.quick.StatusSurface(ctx, string(s.surface), secret)
}

func (s surfaceIdentity) QuickConnectDetails(ctx context.Context, secret string) (frontend.QuickConnectDetails, error) {
	if s.policy.ExternalIdentity {
		return frontend.QuickConnectDetails{}, core.ErrUnauthorized
	}
	if err := s.allow(ctx, s.poll, secret); err != nil {
		return frontend.QuickConnectDetails{}, err
	}
	details, err := s.quick.DetailsSurface(ctx, string(s.surface), secret)
	if err != nil {
		return frontend.QuickConnectDetails{}, err
	}
	return frontend.QuickConnectDetails{Approved: details.Approved, Code: details.Code, Secret: secret, Device: details.Device, CreatedAt: details.CreatedAt, ExpiresAt: details.ExpiresAt}, nil
}
func (s surfaceIdentity) QuickConnectApprove(ctx context.Context, viewer *core.Viewer, code string) error {
	if s.policy.ExternalIdentity || s.quick == nil || s.login == nil {
		return frontend.ErrIdentityUnavailable
	}
	if viewer == nil || viewer.ID <= 0 || viewer.Delegated {
		return core.ErrUnauthorized
	}
	if err := s.allow(ctx, s.login, "quickconnect-approve"); err != nil {
		return err
	}
	err := s.quick.ApproveSurface(ctx, string(s.surface), code, viewer.ID)
	switch {
	case errors.Is(err, identity.ErrQuickConnectExpired):
		return frontend.ErrQuickConnectExpired
	case errors.Is(err, identity.ErrQuickConnectUsed):
		return frontend.ErrQuickConnectUsed
	default:
		return err
	}
}
func (s surfaceIdentity) Revoke(ctx context.Context, credential string) error {
	if s.policy.ExternalIdentity {
		return core.ErrUnauthorized
	}
	viewer, err := s.Resolve(ctx, credential)
	if err != nil {
		return err
	}
	hash := sha256.Sum256([]byte(credential))
	return s.grants.RevokeGrant(ctx, viewer.ID, hex.EncodeToString(hash[:]))
}
func (surfaceIdentity) SetupComplete() bool { return identity.ProtocolSetupComplete() }

var _ frontend.SurfaceIdentity = surfaceIdentity{}

var _ frontend.DeviceIdentity = surfaceIdentity{}

// appLoginSurface reports whether the surface authenticates with dedicated
// app logins: Xtream, and plugin frontend adapters (only adapters declaring
// query credentials are ever issued one; grant lookups are surface-scoped).
func appLoginSurface(surface frontend.Protocol) bool {
	return surface == "xtream" || strings.HasPrefix(string(surface), "plugin:")
}

// AuthenticateAppPassword never accepts a household password or issues a new grant.
func (s surfaceIdentity) AuthenticateAppPassword(ctx context.Context, username, password string) (*core.Viewer, error) {
	if !appLoginSurface(s.surface) || s.login == nil {
		return nil, core.ErrUnauthorized
	}
	if ip, ok := frontend.ClientIP(ctx); !ok || ip.IsUnspecified() || ip.IsMulticast() {
		return nil, core.ErrUnauthorized
	}
	// Valid app passwords authenticate each API/Range request. Charging those
	// against the interactive sign-in budget would lock out a normal startup.
	credential, short := frontend.AppLoginCredential(username, password)
	if !short {
		credential = password
	}
	if short || s.surface == "xtream" && len(username) <= 128 && len(password) <= 128 && core.IsGrantCredential(password) && username == frontend.XtreamUsername(password) {
		viewer, err := s.Resolve(ctx, credential)
		if err == nil && viewer != nil {
			return viewer, nil
		}
	}
	if err := s.allow(ctx, s.login, string(s.surface)); err != nil {
		return nil, err
	}
	return nil, core.ErrUnauthorized
}

// AuthenticateAppToken verifies an app login proven by md5(password + salt).
// Like AuthenticateAppPassword it never accepts a household password, and only
// failed proofs charge the login limiter.
func (s surfaceIdentity) AuthenticateAppToken(ctx context.Context, username, token, salt string) (*core.Viewer, error) {
	if !appLoginSurface(s.surface) || s.login == nil || s.grants == nil {
		return nil, core.ErrUnauthorized
	}
	if ip, ok := frontend.ClientIP(ctx); !ok || ip.IsUnspecified() || ip.IsMulticast() {
		return nil, core.ErrUnauthorized
	}
	if credential, ok := s.grants.AppLoginTokenCredential(ctx, string(s.surface), username, token, salt); ok {
		viewer, err := s.Resolve(ctx, credential)
		if err == nil && viewer != nil {
			return viewer, nil
		}
	}
	if err := s.allow(ctx, s.login, string(s.surface)); err != nil {
		return nil, err
	}
	return nil, core.ErrUnauthorized
}

var _ frontend.AppLoginTokenIdentity = surfaceIdentity{}

// RedeemWebCode is available only on the Jellyfin surface and shares its normal
// sign-in rate limits and device grant lifecycle.
func (s surfaceIdentity) RedeemWebCode(ctx context.Context, code, browser, existing string) (string, int64, error) {
	if s.surface != "jellyfin" || s.grants == nil || s.login == nil {
		return "", 0, core.ErrUnauthorized
	}
	if err := s.allowApp(ctx, s.login, "ether-web", browser); err != nil {
		return "", 0, err
	}
	return s.grants.RedeemWebCode(ctx, code, browser, existing)
}

// RedeemDeviceWebCode exchanges a console handoff for the bound surface.
func (s surfaceIdentity) RedeemDeviceWebCode(ctx context.Context, code string, device core.Device) (string, error) {
	if s.grants == nil || s.policy.ExternalIdentity {
		return "", core.ErrUnauthorized
	}
	if err := s.allowApp(ctx, s.login, "ether-web", s.surfaceAppName(device)); err != nil {
		return "", err
	}
	token, _, err := s.grants.RedeemDeviceWebCode(ctx, code, string(s.surface), device, "")
	return token, err
}

func (s surfaceIdentity) GrantScope(ctx context.Context, token string, device core.Device) (string, error) {
	if s.grants == nil || s.policy.ExternalIdentity {
		return "", core.ErrUnauthorized
	}
	return s.grants.GrantScope(ctx, string(s.surface), token, device)
}
func (s surfaceIdentity) GrantScopeCurrent(ctx context.Context, scope string, device core.Device) (*core.Viewer, error) {
	if s.grants == nil || s.policy.ExternalIdentity {
		return nil, core.ErrUnauthorized
	}
	return s.grants.GrantScopeCurrent(ctx, string(s.surface), scope, device)
}
func (s surfaceIdentity) plexConfigured() bool {
	if s.users == nil {
		return false
	}
	if owner, err := s.users.Owner(context.Background()); err == nil && owner != nil && owner.PlexID != 0 {
		return true
	}
	if s.hostConfig != nil {
		p := s.hostConfig().Plex
		if p.Claimed || p.OwnerPlexID != 0 {
			return true
		}
	}
	return false
}
func (s surfaceIdentity) AuthenticationFeatures() frontend.AuthenticationFeatures {
	native := !s.policy.ExternalIdentity && s.grants != nil && s.login != nil
	return frontend.AuthenticationFeatures{
		Password:          native && s.local != nil,
		QuickConnect:      native && s.quick != nil && s.poll != nil,
		PlexAccountSignIn: native && s.plexConfigured(),
		HouseholdProfiles: native && s.plexHomeProfilesSupported(),
	}
}

// IssueOriginWebCode is invoked only after the adapter validates a published
// HTTPS destination and proves the caller's current device grant.
func (s surfaceIdentity) IssueOriginWebCode(ctx context.Context, access string, device core.Device, origin string) (string, error) {
	if s.surface != "vela" || s.grants == nil || s.policy.ExternalIdentity {
		return "", core.ErrUnauthorized
	}
	if err := s.allowApp(ctx, s.login, "ether-web", s.surfaceAppName(device)); err != nil {
		return "", err
	}
	v, _, err := s.grants.ResolveDevice(ctx, string(s.surface), access, device)
	if err != nil {
		return "", err
	}
	return s.grants.IssueOriginWebCode(ctx, v.ID, string(s.surface), origin)
}
func (s surfaceIdentity) RedeemOriginWebCode(ctx context.Context, code string, device core.Device, origin string) (string, error) {
	if s.surface != "vela" || s.grants == nil || s.policy.ExternalIdentity {
		return "", core.ErrUnauthorized
	}
	if err := s.allowApp(ctx, s.login, "ether-web", s.surfaceAppName(device)); err != nil {
		return "", err
	}
	token, _, err := s.grants.RedeemOriginWebCode(ctx, code, string(s.surface), device, origin)
	return token, err
}

// plexAccountAuthenticator validates a plex.tv account token for native
// surface sign-in without accepting server, delegation or grant credentials.
type plexAccountAuthenticator interface {
	AuthenticatePlexAccount(context.Context, string) (*frontend.User, int64, error)
}

var _ frontend.APIKeyIdentity = surfaceIdentity{}
