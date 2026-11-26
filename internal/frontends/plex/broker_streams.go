package plex

import (
	"context"
	"errors"

	"github.com/braedonsaunders/ether/internal/frontend"
)

func (s *Server) plexStreamAccess(u *frontend.Viewer) frontend.SessionAccess {
	return frontend.SessionAccess{Viewer: u, Protocol: "plex"}
}

func (s *Server) assignStream(ctx context.Context, u *frontend.Viewer, binding frontend.StreamBinding) int64 {
	rt := s.streams()
	if s == nil || rt == nil {
		return 0
	}
	id, err := rt.BindStream(ctx, s.plexStreamAccess(u), binding)
	if err != nil {
		return 0
	}
	return id
}

func (s *Server) resolveStream(ctx context.Context, u *frontend.Viewer, id int64) (frontend.StreamBinding, error) {
	rt := s.streams()
	if s == nil || rt == nil {
		return frontend.StreamBinding{}, frontend.ErrNotFound
	}
	return rt.ResolveStream(ctx, s.plexStreamAccess(u), id)
}

// resolveIssuedStream resolves a stream id a client sent back. The broker
// authorizes it and names the title; when a probe or failover moved that
// id onto another track for this client, the track it names now is
// returned instead of the title-wide binding.
func (s *Server) resolveIssuedStream(ctx context.Context, u *frontend.Viewer, clientID string, id int64) (frontend.StreamBinding, error) {
	binding, err := s.resolveStream(ctx, u, id)
	if err != nil || u == nil || s.playIDs == nil {
		return binding, err
	}
	if t, remapped, known, issuedEst := s.playIDs.resolve(playIdentityKey{userID: u.ID, clientID: clientID, key: binding.Key}, id); remapped {
		binding.Index, binding.Info, binding.Estimated = t.index, t.info, t.estimated
	} else if known {
		binding.Estimated = issuedEst
	}
	return binding, nil
}

func streamMiss(err error) bool {
	return err == nil || errors.Is(err, frontend.ErrNotFound)
}
