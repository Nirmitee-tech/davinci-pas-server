// Package auth verifies CDS Hooks client JWTs ("Trusting CDS Clients").
package auth

import (
	"context"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Error is returned for any token that must be rejected with HTTP 401.
type Error struct {
	Reason string
}

func (e *Error) Error() string { return "invalid bearer token: " + e.Reason }

func reject(reason string) error { return &Error{Reason: reason} }

// Principal describes a verified client token.
type Principal struct {
	Issuer  string
	Subject string
	JTI     string
	KeyID   string
	Alg     string
}

// TrustedIssuer pairs an issuer with its configured JWKS URL.
type TrustedIssuer struct {
	Issuer  string
	JWKSURL string
}

// Verifier validates signature, issuer, audience, time claims and jti replay.
type Verifier struct {
	issuers map[string]string
	algs    map[string]bool
	skew    time.Duration
	keys    *KeyFetcher
	replay  *ReplayCache
	now     func() time.Time
}

// NewVerifier builds a verifier for the given issuers and algorithms.
func NewVerifier(issuers []TrustedIssuer, algs []string, skew time.Duration, keys *KeyFetcher) *Verifier {
	v := &Verifier{
		issuers: map[string]string{},
		algs:    map[string]bool{},
		skew:    skew,
		keys:    keys,
		replay:  NewReplayCache(),
		now:     time.Now,
	}
	for _, iss := range issuers {
		name := strings.TrimRight(iss.Issuer, "/")
		jwks := iss.JWKSURL
		if jwks == "" {
			jwks = name + "/jwks.json"
		}
		v.issuers[name] = jwks
	}
	for _, a := range algs {
		v.algs[a] = true
	}
	return v
}

// Verify checks token and requires its aud to match one of audiences.
func (v *Verifier) Verify(ctx context.Context, token string, audiences []string) (*Principal, error) {
	tok, err := parseCompact(token)
	if err != nil {
		return nil, reject(err.Error())
	}
	h, c := tok.header, tok.claims
	if !v.algs[h.Alg] {
		return nil, reject("unsupported alg " + quote(h.Alg))
	}
	if len(h.Crit) > 0 {
		return nil, reject("unsupported crit header")
	}
	iss := strings.TrimRight(c.Iss, "/")
	jwksURL, trusted := v.issuers[iss]
	if c.Iss == "" || !trusted {
		return nil, reject("untrusted issuer " + quote(c.Iss))
	}
	if h.Jku != "" {
		if h.Jku != jwksURL && !strings.HasPrefix(h.Jku, iss+"/") {
			return nil, reject("jku is not under the trusted issuer")
		}
		jwksURL = h.Jku
	}

	if err := v.verifyWithKeys(ctx, tok, jwksURL); err != nil {
		return nil, err
	}

	now := v.now()
	exp, ok := numericDate(c.Exp)
	if !ok {
		return nil, reject("missing exp")
	}
	if now.After(exp.Add(v.skew)) {
		return nil, reject("token expired")
	}
	iat, ok := numericDate(c.Iat)
	if !ok {
		return nil, reject("missing iat")
	}
	if iat.After(now.Add(v.skew)) {
		return nil, reject("iat is in the future")
	}
	if nbf, ok := numericDate(c.Nbf); ok && nbf.After(now.Add(v.skew)) {
		return nil, reject("token not yet valid")
	}
	if !audienceMatches(c.Aud, audiences) {
		return nil, reject("aud does not match the service URL")
	}
	if c.Jti == "" {
		return nil, reject("missing jti")
	}
	if !v.replay.Store(iss+"|"+c.Jti, exp.Add(v.skew), now) {
		return nil, reject("jti has already been used")
	}
	return &Principal{Issuer: iss, Subject: c.Sub, JTI: c.Jti, KeyID: h.Kid, Alg: h.Alg}, nil
}

func (v *Verifier) verifyWithKeys(ctx context.Context, tok *parsedToken, jwksURL string) error {
	try := func(keys []JWK) (matched bool, err error) {
		for _, k := range keys {
			if tok.header.Kid != "" && k.Kid != tok.header.Kid {
				continue
			}
			if !k.usableFor(tok.header.Alg) {
				continue
			}
			pub, perr := k.PublicKey()
			if perr != nil {
				continue
			}
			matched = true
			if verifySignature(tok.header.Alg, pub, tok.signingInput, tok.signature) == nil {
				return true, nil
			}
		}
		if matched {
			return true, reject("signature verification failed")
		}
		return false, nil
	}

	keys, err := v.keys.Keys(ctx, jwksURL, false)
	if err != nil {
		return reject("unable to load issuer keys")
	}
	matched, verr := try(keys)
	if matched {
		return verr
	}
	// Unknown kid: the issuer may have rotated keys.
	keys, err = v.keys.Keys(ctx, jwksURL, true)
	if err != nil {
		return reject("unable to load issuer keys")
	}
	matched, verr = try(keys)
	if matched {
		return verr
	}
	return reject("no issuer key matches the token kid and alg")
}

func quote(s string) string { return "\"" + s + "\"" }

// NormalizeURL lowercases scheme and host, drops default ports and trailing slashes.
func NormalizeURL(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" {
		return strings.TrimRight(raw, "/")
	}
	scheme := strings.ToLower(u.Scheme)
	host := strings.ToLower(u.Hostname())
	port := u.Port()
	if (scheme == "http" && port == "80") || (scheme == "https" && port == "443") {
		port = ""
	}
	if port != "" {
		host += ":" + port
	}
	return scheme + "://" + host + strings.TrimRight(u.EscapedPath(), "/")
}

func audienceMatches(aud []string, expected []string) bool {
	for _, a := range aud {
		na := NormalizeURL(a)
		for _, e := range expected {
			if na == NormalizeURL(e) {
				return true
			}
		}
	}
	return false
}

// ReplayCache remembers jti values until the token expires.
type ReplayCache struct {
	mu      sync.Mutex
	seen    map[string]time.Time
	inserts int
}

// NewReplayCache returns an empty cache.
func NewReplayCache() *ReplayCache {
	return &ReplayCache{seen: map[string]time.Time{}}
}

// Store records key until the given time. It returns false if key is already
// present and unexpired.
func (c *ReplayCache) Store(key string, until, now time.Time) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if exp, ok := c.seen[key]; ok && now.Before(exp) {
		return false
	}
	c.seen[key] = until
	c.inserts++
	if c.inserts%256 == 0 {
		for k, exp := range c.seen {
			if !now.Before(exp) {
				delete(c.seen, k)
			}
		}
	}
	return true
}
