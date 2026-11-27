package sessions

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
)

// EnableLazyDirectTickets defers admission of direct-file capabilities to
// the first media GET. The surface must use a persistent, cluster-wide key.
func EnableLazyDirectTickets(issuer frontend.LazyStreams, key []byte) error {
	s, ok := issuer.(*lazyStreams)
	if !ok || len(key) == 0 {
		return errors.New("sessions: missing direct ticket dependency")
	}
	sum := sha256.Sum256(key)
	block, err := aes.NewCipher(sum[:])
	if err != nil {
		return err
	}
	s.directTickets, err = cipher.NewGCMWithRandomNonce(block)
	return err
}

// EnableMediaBrowserDirectTickets also binds the item-scoped version resolver
// for exact versions offered on quiet metadata reads.
func EnableMediaBrowserDirectTickets(issuer frontend.LazyStreams, versions frontend.VersionHandles, key []byte) error {
	listed, ok := versions.(frontend.ListedVersionHandles)
	if !ok {
		return core.ErrNotAvailable
	}
	if err := EnableLazyDirectTickets(issuer, key); err != nil {
		return err
	}
	issuer.(*lazyStreams).listedTicketVersions = listed
	return nil
}

const lazyDirectPrefix = "lt1_"

type lazyDirectTicket struct {
	Viewer    int64
	Reference string
	Request   json.RawMessage
	Expires   int64
}

func (s *lazyStreams) sealDirectTicket(ticket lazyDirectTicket) (string, error) {
	raw, err := json.Marshal(ticket)
	if err != nil || len(raw) > 80<<10 {
		return "", core.ErrNotAvailable
	}
	sealed := s.directTickets.Seal(nil, nil, raw, []byte(s.protocol))
	return lazyDirectPrefix + base64.RawURLEncoding.EncodeToString(sealed), nil
}

func (s *lazyStreams) openDirectTicket(token string) (lazyDirectTicket, error) {
	var ticket lazyDirectTicket
	token = strings.TrimPrefix(token, lazyDirectPrefix)
	if len(token) > base64.RawURLEncoding.EncodedLen((80<<10)+s.directTickets.Overhead()) {
		return ticket, core.ErrNotFound
	}
	sealed, err := base64.RawURLEncoding.Strict().DecodeString(token)
	if err != nil || base64.RawURLEncoding.EncodeToString(sealed) != token {
		return ticket, core.ErrNotFound
	}
	raw, err := s.directTickets.Open(nil, nil, sealed, []byte(s.protocol))
	if err != nil || json.Unmarshal(raw, &ticket) != nil || ticket.Viewer <= 0 || ticket.Reference == "" {
		return lazyDirectTicket{}, core.ErrNotFound
	}
	return ticket, nil
}
