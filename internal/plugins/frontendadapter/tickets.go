package frontendadapter

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"strings"
	"sync"
	"time"
)

// Stream tickets (spec §6.5). A plugin's adapter response points media at
// Ether, not at the plugin: the SDK helper builds
// /ether/v1/kind-media/{ticket}, where the ticket is a short-lived grant
// bound to (viewer, item, adapter) and an optional zero-based part index
// and/or resolver source id. Ether mints and validates it; bytes then come
// from KindMedia, off the plugin path even for remote plugins.
//
// The grant is scoped: a ticket for one viewer and one item (or one part
// and/or source of that item) is rejected for any other viewer, item, part,
// source or adapter, and it expires.

// DefaultStreamTicketTTL is the lifetime of a minted stream ticket.
const DefaultStreamTicketTTL = 2 * time.Minute

// ErrTicketInvalid means the ticket is unknown, expired or out of scope.
var ErrTicketInvalid = errors.New("frontendadapter: stream ticket invalid")

// StreamGrant is the scope a stream ticket authorizes.
type StreamGrant struct {
	ViewerID  int64
	ItemID    string
	Adapter   string
	PartIndex *int
	Source    string
	Wait      time.Duration
}

// TicketIssuer mints and validates scoped, short-lived stream tickets for
// one adapter. It is safe for concurrent use. App wiring mounts Validate at
// the kind-media endpoint; the SDK helper builds the URL.
type TicketIssuer struct {
	mu      sync.Mutex
	tickets map[string]ticket
	ttl     time.Duration
	now     func() time.Time
	waits   mediaWaitSlots
}

type ticket struct {
	grant   StreamGrant
	expires time.Time
}

// NewTicketIssuer builds an issuer with the default TTL.
func NewTicketIssuer() *TicketIssuer {
	return &TicketIssuer{
		tickets: make(map[string]ticket),
		ttl:     DefaultStreamTicketTTL,
		now:     time.Now,
	}
}

// SetTTL overrides the ticket lifetime (tests use a short TTL).
func (t *TicketIssuer) SetTTL(ttl time.Duration) {
	t.mu.Lock()
	t.ttl = ttl
	t.mu.Unlock()
}

func cloneGrant(g StreamGrant) StreamGrant {
	if g.PartIndex != nil {
		v := *g.PartIndex
		g.PartIndex = &v
	}
	return g
}

func samePartIndex(a, b *int) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

// ValidateSource checks an optional resolver source id scope: empty, or a
// non-blank id without control characters.
func ValidateSource(source string) error {
	if source == "" {
		return nil
	}
	if strings.TrimSpace(source) == "" {
		return errors.New("frontendadapter: blank stream grant source")
	}
	for _, r := range source {
		if r < 0x20 || r == 0x7f {
			return errors.New("frontendadapter: invalid stream grant source")
		}
	}
	return nil
}

// Issue mints a ticket for the given grant and returns its opaque token.
func (t *TicketIssuer) Issue(grant StreamGrant) (string, error) {
	if grant.ViewerID == 0 || grant.ItemID == "" || grant.Adapter == "" {
		return "", errors.New("frontendadapter: incomplete stream grant")
	}
	if grant.PartIndex != nil && *grant.PartIndex < 0 {
		return "", errors.New("frontendadapter: negative stream grant part index")
	}
	if err := ValidateSource(grant.Source); err != nil {
		return "", err
	}
	var raw [24]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	token := hex.EncodeToString(raw[:])
	t.mu.Lock()
	t.tickets[token] = ticket{grant: cloneGrant(grant), expires: t.now().Add(t.ttl)}
	t.mu.Unlock()
	return token, nil
}

// Validate checks that token is live and matches the presented item scope. It
// does not consume the ticket: a media URL may be ranged many times within
// its short lifetime. An expired or out-of-scope ticket returns
// ErrTicketInvalid and is forgotten.
func (t *TicketIssuer) Validate(token string, viewerID int64, itemID, adapter string) (StreamGrant, error) {
	return t.ValidateScope(token, viewerID, itemID, adapter, nil, "")
}

// ValidatePart checks that token is live and matches the presented viewer,
// item, adapter and optional part scope.
func (t *TicketIssuer) ValidatePart(token string, viewerID int64, itemID, adapter string, partIndex *int) (StreamGrant, error) {
	return t.ValidateScope(token, viewerID, itemID, adapter, partIndex, "")
}

// ValidateScope checks that token is live and matches the presented viewer,
// item, adapter, optional part index, and optional resolver source scope.
func (t *TicketIssuer) ValidateScope(token string, viewerID int64, itemID, adapter string, partIndex *int, source string) (StreamGrant, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	tk, ok := t.tickets[token]
	if !ok {
		return StreamGrant{}, ErrTicketInvalid
	}
	if !t.now().Before(tk.expires) {
		delete(t.tickets, token)
		return StreamGrant{}, ErrTicketInvalid
	}
	if tk.grant.ViewerID != viewerID || tk.grant.ItemID != itemID || tk.grant.Adapter != adapter || !samePartIndex(tk.grant.PartIndex, partIndex) || tk.grant.Source != source {
		return StreamGrant{}, ErrTicketInvalid
	}
	return cloneGrant(tk.grant), nil
}

// StreamURL mints a ticket for grant and returns the Ether stream URL the
// plugin should embed in its adapter response. The URL is served by Ether
// from KindMedia, so bytes stay off the plugin path even for remote
// plugins.
func (t *TicketIssuer) StreamURL(grant StreamGrant) (string, error) {
	token, err := t.Issue(grant)
	if err != nil {
		return "", err
	}
	return "/ether/v1/kind-media/" + token, nil
}

// Redeem returns the grant a live ticket authorizes, or ErrTicketInvalid if
// it is unknown or expired. The kind-media endpoint uses it: the ticket
// itself is the proof (short-lived and unguessable), so the request carries
// no separate credential. The grant's viewer, item and optional partIndex
// scope what may be opened.
func (t *TicketIssuer) Redeem(token string) (StreamGrant, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	tk, ok := t.tickets[token]
	if !ok {
		return StreamGrant{}, ErrTicketInvalid
	}
	if !t.now().Before(tk.expires) {
		delete(t.tickets, token)
		return StreamGrant{}, ErrTicketInvalid
	}
	return cloneGrant(tk.grant), nil
}

// Sweep drops expired tickets; the host may call it periodically.
func (t *TicketIssuer) Sweep() {
	t.mu.Lock()
	now := t.now()
	for token, tk := range t.tickets {
		if !now.Before(tk.expires) {
			delete(t.tickets, token)
		}
	}
	t.mu.Unlock()
}
