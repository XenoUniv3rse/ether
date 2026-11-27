package livetv

import (
	"net/url"
	"strings"
	"sync/atomic"
	"unicode"

	"github.com/braedonsaunders/ether/internal/core"
)

func providerHost(raw string) string {
	if u, err := url.Parse(raw); err == nil && (u.Scheme == "http" || u.Scheme == "https") {
		return u.Hostname()
	}
	return ""
}

func safeProviderLabel(label string) string {
	label = strings.TrimSpace(label)
	if len(label) > 200 || strings.ContainsAny(label, "/\\?&#=@%:") || strings.IndexFunc(label, unicode.IsControl) >= 0 {
		return ""
	}
	return label
}

func providerLoginLabel(src *Source, candidate ProviderLogin) string {
	if len(src.Logins) == 0 {
		return ""
	}
	raw, username := candidate.PlaylistURL, candidate.Username
	xtream := src.Auth != nil && src.Auth.Type == "xtream"
	if candidate.ID == "primary" {
		raw = src.PlaylistURL
		if xtream {
			username = src.Auth.Username
		}
	}
	if label := providerNickname(candidate); label != "" {
		return label
	}
	if !xtream {
		return providerHost(raw)
	}
	name := []rune(username)
	// Even a one- or two-character username must have a hidden suffix.
	prefix := string(name[:min(2, max(0, len(name)-1))])
	return safeProviderLabel(prefix) + "***"
}

func providerNickname(login ProviderLogin) string {
	label := safeProviderLabel(login.Label)
	if label == "" || containsProviderSecret(label, login.Username, login.Password) ||
		containsURLCredentials(label, login.PlaylistURL) {
		return ""
	}
	return label
}

func containsProviderSecret(label string, secrets ...string) bool {
	for _, secret := range secrets {
		if secret != "" && strings.Contains(label, secret) {
			return true
		}
	}
	return false
}

func containsURLCredentials(label, raw string) bool {
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	if pw, ok := u.User.Password(); ok && pw != "" && strings.Contains(label, pw) {
		return true
	}
	if user := u.User.Username(); user != "" && strings.Contains(label, user) {
		return true
	}
	for _, values := range u.Query() {
		if containsProviderSecret(label, values...) {
			return true
		}
	}
	// Playlist URLs can put an account or token in a path segment.
	return containsProviderSecret(label, strings.Split(u.Path, "/")...)
}

func providerSourceID(c Channel, src StreamSource) string {
	if src.AuthSourceID == "" && (src.ID == stableID(c.ID, "playlist") || len(c.StreamSources) <= 1) {
		return c.SourceID
	}
	return src.AuthSourceID
}

type providerLoginKey struct{ source, login string }

type servingProvider struct {
	sourceID string
	provider core.LiveProvider
	loginKey providerLoginKey
}

func providerForInput(entry authEntry, src StreamSource, lease *loginLease) servingProvider {
	name := safeProviderLabel(entry.name)
	if name == "" && entry.name == "" {
		name = safeProviderLabel(src.Name)
	}
	if auth := entry.auth; auth != nil {
		if containsProviderSecret(name, auth.Username, auth.Password, auth.Token) {
			name = ""
		}
		for _, value := range auth.Headers {
			if containsProviderSecret(name, value) {
				name = ""
				break
			}
		}
	}
	if name == "" {
		name = entry.playlistHost
	}
	if name == "" || containsURLCredentials(name, src.URL) {
		name = providerHost(src.URL)
	}
	out := servingProvider{sourceID: src.ID, provider: core.LiveProvider{
		LiveProviderIdentity: core.LiveProviderIdentity{SourceName: name},
	}}
	if lease != nil {
		out.provider.LoginID = safeProviderLabel(lease.accountID)
		out.provider.LoginLabel = lease.label
		out.provider.LoginNumber = lease.number
		out.provider.LoginSlot = lease.slot + 1
		out.provider.LoginLimit = lease.limit
		out.loginKey = providerLoginKey{lease.source, lease.login}
	}
	return out
}

type providerChange struct {
	active   servingProvider
	previous *core.LiveProviderIdentity
}

// providerTracker publishes immutable snapshots. Each producer has one writer.
type providerTracker struct {
	current atomic.Pointer[providerChange]
}

func (t *providerTracker) set(active servingProvider) {
	old := t.current.Load()
	next := providerChange{active: active}
	if old != nil {
		if old.active == active {
			return
		}
		next.previous = old.previous
		if old.active.sourceID != active.sourceID || old.active.loginKey != active.loginKey {
			if old.active.provider.SourceName != "" {
				next.previous = &old.active.provider.LiveProviderIdentity
			}
		}
	}
	t.current.Store(&next)
}

func (s *Service) localLoginUsage() map[providerLoginKey]int {
	p := &s.loginPool
	if !p.mu.TryLock() {
		return nil
	}
	defer p.mu.Unlock()
	counts := make(map[providerLoginKey]int)
	for _, l := range p.live {
		if !l.lost.Load() {
			counts[providerLoginKey{l.source, l.login}]++
		}
	}
	return counts
}

func (p *providerChange) snapshot(usage map[providerLoginKey]int) (*core.LiveProvider, *core.LiveProviderIdentity) {
	if p == nil || p.active.provider.SourceName == "" {
		return nil, nil
	}
	out := p.active.provider
	if out.LoginLimit > 0 && usage != nil {
		count := usage[p.active.loginKey]
		out.LocalInUse = &count
	}
	return &out, p.previous
}

func (p *providerChange) attachSession(info *SessionInfo, owner bool, usage map[providerLoginKey]int) {
	if p != nil && p.active.sourceID != "" {
		info.ActiveSourceID = p.active.sourceID
	}
	if owner {
		info.Provider, info.PreviousProvider = p.snapshot(usage)
	}
}

func (ss *session) servingProviderLocked() *providerChange {
	if ss.catchUp != nil {
		return ss.catchUp.provider
	}
	if ss.broadcast != nil {
		return ss.broadcast.servingProvider()
	}
	if ss.outputProvider != nil {
		return ss.outputProvider
	}
	if ss.owned != nil && ss.owned.provider != nil {
		return ss.owned.provider()
	}
	return nil
}

// recordOutputProviderLocked captures the grant used by non-HLS outputs.
func (s *Service) recordOutputProviderLocked(ss *session) {
	input := ss.sharedInput.Load()
	if input == nil {
		input = ss
	}
	if g := s.sessionGrant(input); g != nil {
		ss.outputProvider = &providerChange{active: g.provider}
	}
}

func (l *broadcastLease) servingProvider() *providerChange {
	if p, ok := l.broadcast.planner.(*providerBroadcastPlanner); ok {
		return p.provider.current.Load()
	}
	return nil
}

func steadyProvider(f *steadyFeed, before *providerChange) *providerChange {
	if f != nil {
		if next := f.lease.servingProvider(); next != nil {
			if before != nil && next.previous == nil && next.active != before.active {
				return &providerChange{active: next.active, previous: &before.active.provider.LiveProviderIdentity}
			}
			return next
		}
	}
	return before
}
