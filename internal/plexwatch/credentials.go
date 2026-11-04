package plexwatch

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/md5"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"
)

// serviceCredentials never persist bearer credentials. The server identity is
// part of the cache key so reclaiming the server cannot reuse another claim.
type serviceCredentials struct {
	mu     sync.Mutex
	server string
	tokens map[int64]serviceCredential
}
type serviceCredential struct {
	token string
	until time.Time
}

// unwrapServiceToken implements Plex's service-token envelope (not media DRM).
// The fixed protocol seed is not an account credential. The server issues the
// enclosed signed JWT only after authenticating the claimed server. This was
// independently implemented from the PMS wire/loader behavior; no PMS code is used.
func unwrapServiceToken(encoded string) (string, time.Time, error) {
	invalid := errors.New("plexwatch: invalid service credential envelope")
	if len(encoded) > 16384 {
		return "", time.Time{}, invalid
	}
	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil || len(raw) == 0 || len(raw)%aes.BlockSize != 0 {
		return "", time.Time{}, invalid
	}
	seed := []byte("Your move now intern, good luck!")
	for i := range seed {
		seed[i] ^= 42
	}
	var material, previous []byte
	for len(material) < 48 {
		h := md5.New()
		h.Write(previous)
		h.Write(seed)
		previous = h.Sum(nil)
		material = append(material, previous...)
	}
	block, err := aes.NewCipher(material[:32])
	if err != nil {
		return "", time.Time{}, invalid
	}
	cipher.NewCBCDecrypter(block, material[32:48]).CryptBlocks(raw, raw)
	// PMS uses arbitrary padding bytes, with only the final byte giving length.
	pad := int(raw[len(raw)-1])
	if pad < 1 || pad > 16 || pad >= len(raw) {
		return "", time.Time{}, invalid
	}
	token := string(raw[:len(raw)-pad])
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return "", time.Time{}, invalid
	}
	for _, p := range parts {
		if p == "" {
			return "", time.Time{}, invalid
		}
		if _, e := base64.RawURLEncoding.DecodeString(p); e != nil {
			return "", time.Time{}, invalid
		}
	}
	payload, _ := base64.RawURLEncoding.DecodeString(parts[1])
	var claims struct {
		Exp int64 `json:"exp"`
	}
	if json.Unmarshal(payload, &claims) != nil || claims.Exp == 0 {
		return "", time.Time{}, invalid
	}
	// Claims only bound the cache lifetime. The Plex service verifies the JWT.
	return token, time.Unix(claims.Exp, 0), nil
}

func (c *Client) stateToken(ctx context.Context, viewerToken string, plexID int64) (string, error) {
	if c.serverIdentity == nil {
		// BaseURL is the explicit offline transport used by the journal tests.
		if c.plex.Host == c.scrobble.Host {
			return viewerToken, nil
		}
		return "", ErrCredentialForbidden
	}
	server, id := c.serverIdentity()
	if server == "" || id != c.clientID {
		return "", ErrCredentialForbidden
	}
	c.credentials.mu.Lock()
	defer c.credentials.mu.Unlock()
	if c.credentials.server != server {
		c.credentials.server = server
		c.credentials.tokens = make(map[int64]serviceCredential)
	}
	if cached := c.credentials.tokens[plexID]; c.now().Before(cached.until) {
		return cached.token, nil
	}
	var users []struct {
		ID       int64 `json:"id"`
		Services []struct {
			Identifier string `json:"identifier"`
			Endpoint   string `json:"endpoint"`
			Token      string `json:"token"`
			Status     string `json:"status"`
		} `json:"services"`
	}
	if _, err := c.call(ctx, http.MethodGet, c.plex, "/api/v2/server/users/services", server, nil, &users, http.StatusOK); err != nil {
		if errors.Is(err, ErrRevoked) {
			return "", ErrCredentialForbidden
		}
		return "", err
	}
	for _, u := range users {
		if u.ID != plexID {
			continue
		}
		for _, s := range u.Services {
			if s.Identifier != "scrobbling" || s.Endpoint != scrobbleOrigin || s.Status != "online" {
				continue
			}
			token, expiry, err := unwrapServiceToken(s.Token)
			if err != nil {
				return "", err
			}
			until := c.now().Add(5 * time.Minute)
			if expiry.Add(-time.Minute).Before(until) {
				until = expiry.Add(-time.Minute)
			}
			if !c.now().Before(until) {
				return "", ErrCredentialForbidden
			}
			c.credentials.tokens[plexID] = serviceCredential{token, until}
			return token, nil
		}
	}
	return "", ErrCredentialForbidden
}

func (c *Client) stateCall(ctx context.Context, method, path, viewerToken string, plexID int64, body []byte, dest any, ok ...int) (int, error) {
	token, err := c.stateToken(ctx, viewerToken, plexID)
	if err != nil {
		return 0, &credentialLookupError{err}
	}
	code, err := c.call(ctx, method, c.scrobble, path, token, body, dest, ok...)
	if code == http.StatusUnauthorized && c.serverIdentity != nil {
		c.credentials.mu.Lock()
		delete(c.credentials.tokens, plexID)
		c.credentials.mu.Unlock()
		// A refused service JWT does not revoke the independent viewer PIN token.
		// Never replay a POST automatically; journal admission owns retry decisions.
		return code, &StatusError{Status: http.StatusForbidden, Op: "service credential"}
	}
	return code, err
}

// A credential lookup failure proves no state request was sent.
type credentialLookupError struct{ error }

func (e *credentialLookupError) Unwrap() error { return e.error }
