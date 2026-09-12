package auth

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"sync"
	"time"
)

// IssuedToken describes an opaque access token handed out by a token endpoint.
type IssuedToken struct {
	ClientID string
	Scope    string
	// Audience names what the token was issued for, e.g. a FHIR base or version.
	Audience string
	Expires  time.Time
}

// TokenStore keeps opaque access tokens in memory until they expire.
type TokenStore struct {
	now func() time.Time

	mu      sync.Mutex
	tokens  map[string]IssuedToken
	inserts int
}

// NewTokenStore returns an empty store.
func NewTokenStore() *TokenStore {
	return &TokenStore{now: time.Now, tokens: map[string]IssuedToken{}}
}

// Issue stores t under a new random token (32 hex characters) and returns it.
func (s *TokenStore) Issue(t IssuedToken) (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	token := hex.EncodeToString(buf)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tokens[token] = t
	s.inserts++
	if s.inserts%256 == 0 {
		now := s.now()
		for k, v := range s.tokens {
			if !now.Before(v.Expires) {
				delete(s.tokens, k)
			}
		}
	}
	return token, nil
}

// Lookup returns the token details when the token exists and has not expired.
func (s *TokenStore) Lookup(token string) (IssuedToken, bool) {
	if token == "" {
		return IssuedToken{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.tokens[token]
	if !ok || !s.now().Before(t.Expires) {
		return IssuedToken{}, false
	}
	return t, true
}

// EqualConstantTime compares two secrets without leaking timing information.
func EqualConstantTime(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}
