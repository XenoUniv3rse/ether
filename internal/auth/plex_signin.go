package auth

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/flight"
)

var (
	// ErrPlexNotShared identifies a valid plex.tv account that does not have
	// active shared access to this server.
	ErrPlexNotShared = errors.New("auth: server not shared with Plex account")

	// ErrPlexUnreachable indicates plex.tv validation could not be completed.
	ErrPlexUnreachable = errors.New("auth: plex.tv validation unavailable")
)

type negEntry struct {
	expires   time.Time
	plexID    int64
	notShared bool
}

// AuthenticatePlexAccount validates a plex.tv account token using the same
// plex.tv validation and caching path as Service.Accept, resolving only to the
// server owner or an active shared/home Plex user. Server tokens, delegation
// tokens, owner console sessions and Ether grant credentials are rejected.
func (s *Service) AuthenticatePlexAccount(ctx context.Context, token string) (*core.User, int64, error) {
	if s == nil {
		return nil, 0, core.ErrUnauthorized
	}
	token = strings.TrimSpace(token)
	if token == "" || core.IsGrantCredential(token) || core.IsAPIKeyCredential(token) || strings.HasPrefix(token, delegationPrefix) || IsOwnerSession(token) {
		return nil, 0, core.ErrUnauthorized
	}
	h := HashToken(token)
	now := s.o.Now()

	s.mu.RLock()
	if s.ownerPlexID == 0 || s.serverTokens[h] {
		s.mu.RUnlock()
		return nil, 0, core.ErrUnauthorized
	}
	if id, ok := s.tokens[h]; ok {
		u := s.activePlexUser(id)
		var inactivePlexID int64
		if u == nil {
			if r := s.users[id]; r != nil && r.Origin != "dlna_guest" {
				inactivePlexID = r.PlexID
			}
		}
		s.mu.RUnlock()
		if u == nil {
			if inactivePlexID != 0 {
				return nil, inactivePlexID, ErrPlexNotShared
			}
			return nil, 0, core.ErrUnauthorized
		}
		s.touch(u.ID, now)
		return u, u.PlexID, nil
	}
	if e, ok := s.pos[h]; ok && now.Before(e.expires) {
		u := s.activePlexUser(e.userID)
		s.mu.RUnlock()
		if u == nil {
			return nil, 0, core.ErrUnauthorized
		}
		s.touch(u.ID, now)
		return u, u.PlexID, nil
	}
	if neg, ok := s.neg[h]; ok && now.Before(neg.expires) {
		s.mu.RUnlock()
		if neg.notShared {
			return nil, neg.plexID, ErrPlexNotShared
		}
		return nil, 0, core.ErrUnauthorized
	}
	s.mu.RUnlock()

	if s.o.Validator == nil && s.unknownTokenRefresher() == nil {
		return nil, 0, core.ErrUnauthorized
	}
	out, err := s.validateResult(ctx, token, h)
	s.mu.RLock()
	isServer := s.serverTokens[h]
	s.mu.RUnlock()
	if isServer {
		return nil, 0, core.ErrUnauthorized
	}
	if err == nil && out.user != nil {
		return out.user, out.user.PlexID, nil
	}
	if out.notShared {
		return nil, out.plexID, ErrPlexNotShared
	}
	if errors.Is(err, core.ErrUnauthorized) {
		return nil, 0, core.ErrUnauthorized
	}
	if ctx.Err() != nil && errors.Is(err, context.Canceled) {
		return nil, 0, err
	}
	return nil, 0, ErrPlexUnreachable
}

// ActivePlexUserByPlexID returns the active Ether viewer mapped to a
// plex.tv account id, or nil when none is active. DLNA guests never match.
func (s *Service) ActivePlexUserByPlexID(plexID int64) *core.User {
	if s == nil || plexID <= 0 {
		return nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	id, ok := s.byPlex[plexID]
	if !ok {
		return nil
	}
	return s.activePlexUser(id)
}

// PlexHomeSwitchAvailable reports availability from the cached server
// membership. An owner row alone does not establish a Home roster.
func (s *Service) PlexHomeSwitchAvailable(viewer *core.Viewer) (available, known bool) {
	if viewer == nil {
		return false, false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.ownerPlexID <= 0 || (!s.hasCachedHome && s.homeSyncedOwnerID != s.ownerPlexID) {
		return false, false
	}
	r := s.users[viewer.ID]
	return s.activeHomeProfiles >= 2 && r.isActivePlexHomeProfile() && r.PlexID == viewer.PlexID, true
}

func (r *row) isActivePlexHomeProfile() bool {
	return r != nil && r.Active && r.PlexID > 0 && r.Origin != "dlna_guest" && (r.IsOwner || r.Home)
}

// OwnerPlexID returns the server owner's plex.tv account id, or 0.
func (s *Service) OwnerPlexID() int64 {
	if s == nil {
		return 0
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.ownerPlexID
}

// PlexLinkID returns the verified identity_links id for a viewer's Plex account,
// or nil when none is recorded.
func (s *Service) PlexLinkID(ctx context.Context, viewerID, plexID int64) *int64 {
	if s == nil || s.db == nil || viewerID <= 0 || plexID <= 0 {
		return nil
	}
	var id int64
	if err := s.db.QueryRowContext(ctx, `SELECT id FROM identity_links WHERE viewer_id=? AND provider='plex' AND subject=?`, viewerID, strconv.FormatInt(plexID, 10)).Scan(&id); err != nil || id <= 0 {
		return nil
	}
	return &id
}

func (s *Service) validateResult(ctx context.Context, token, h string) (validation, error) {
	if err := ctx.Err(); err != nil {
		return validation{}, err
	}
	// Detached, so one impatient client does not fail the others waiting
	// on the same token. Waiters share that one acceptance receipt.
	out, err := s.inflight.DoDetachedLimit(ctx, h, s.o.MaxConcurrentValidations, func(ctx context.Context) (validation, error) {
		ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
		defer cancel()
		return s.validateOnce(ctx, token, h)
	})
	// Only the direct admission refusal takes this path; validation errors may
	// wrap other causes and retain their existing stale-fallback semantics.
	if err == flight.ErrCapacity {
		u, acc, staleErr := s.stale(h, errors.New("auth: token validation capacity reached"))
		var plexID int64
		if u != nil {
			plexID = u.PlexID
		}
		return validation{user: u, acc: acc, plexID: plexID}, staleErr
	}
	return out, err
}
