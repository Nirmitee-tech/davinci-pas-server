package auth

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"strings"
	"time"
)

// ClientAssertionType is the only client_assertion_type SMART Backend
// Services accepts (RFC 7523 section 2.2).
const ClientAssertionType = "urn:ietf:params:oauth:client-assertion-type:jwt-bearer"

// ClientAssertionOptions configures a ClientAssertionVerifier.
type ClientAssertionOptions struct {
	// ClientIDs are the registered client ids; iss and sub must both equal one.
	ClientIDs []string
	// Algorithms are the accepted JWS algorithms, e.g. RS384 and ES384.
	Algorithms []string
	// StaticKeys are checked before any JWKS URL (for example a local JWKS file).
	StaticKeys []JWK
	// JWKSURLs are trusted JWK Set locations, tried in order.
	JWKSURLs []string
	// ClockSkew is the leeway applied to exp, nbf and iat.
	ClockSkew time.Duration
	// MaxLifetime bounds how far in the future exp may be (SMART: 5 minutes).
	MaxLifetime time.Duration
	// Keys fetches and caches the JWKS URLs.
	Keys *KeyFetcher
}

// ClientAssertionVerifier validates SMART Backend Services client assertions
// (private_key_jwt): signature from a trusted key, iss = sub = a registered
// client id, aud = the token endpoint, exp bounds and jti replay.
type ClientAssertionVerifier struct {
	clientIDs   map[string]bool
	algs        map[string]bool
	static      []JWK
	jwksURLs    []string
	skew        time.Duration
	maxLifetime time.Duration
	keys        *KeyFetcher
	replay      *ReplayCache
	now         func() time.Time
}

// NewClientAssertionVerifier builds a verifier from opts.
func NewClientAssertionVerifier(opts ClientAssertionOptions) *ClientAssertionVerifier {
	v := &ClientAssertionVerifier{
		clientIDs:   map[string]bool{},
		algs:        map[string]bool{},
		static:      opts.StaticKeys,
		jwksURLs:    opts.JWKSURLs,
		skew:        opts.ClockSkew,
		maxLifetime: opts.MaxLifetime,
		keys:        opts.Keys,
		replay:      NewReplayCache(),
		now:         time.Now,
	}
	if v.keys == nil {
		v.keys = NewKeyFetcher(nil, 5*time.Minute, nil)
	}
	if v.maxLifetime <= 0 {
		v.maxLifetime = 5 * time.Minute
	}
	for _, id := range opts.ClientIDs {
		v.clientIDs[id] = true
	}
	for _, a := range opts.Algorithms {
		v.algs[a] = true
	}
	return v
}

// Verify checks a client assertion whose aud must match one of tokenURLs.
func (v *ClientAssertionVerifier) Verify(ctx context.Context, assertion string, tokenURLs []string) (*Principal, error) {
	tok, err := parseCompact(strings.TrimSpace(assertion))
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
	if h.Kid == "" {
		return nil, reject("missing kid header")
	}
	if c.Iss == "" || !v.clientIDs[c.Iss] {
		return nil, reject("unknown client " + quote(c.Iss))
	}
	if c.Sub != c.Iss {
		return nil, reject("sub must equal iss")
	}
	if err := v.verifySignature(ctx, tok); err != nil {
		return nil, err
	}

	now := v.now()
	exp, ok := numericDate(c.Exp)
	if !ok {
		return nil, reject("missing exp")
	}
	if now.After(exp.Add(v.skew)) {
		return nil, reject("assertion expired")
	}
	if exp.After(now.Add(v.maxLifetime + v.skew)) {
		return nil, reject("exp is more than " + v.maxLifetime.String() + " in the future")
	}
	if iat, ok := numericDate(c.Iat); ok && iat.After(now.Add(v.skew)) {
		return nil, reject("iat is in the future")
	}
	if nbf, ok := numericDate(c.Nbf); ok && nbf.After(now.Add(v.skew)) {
		return nil, reject("assertion not yet valid")
	}
	if !audienceMatches(c.Aud, tokenURLs) {
		return nil, reject("aud does not match the token endpoint")
	}
	if c.Jti == "" {
		return nil, reject("missing jti")
	}
	if !v.replay.Store(c.Iss+"|"+c.Jti, exp.Add(v.skew), now) {
		return nil, reject("jti has already been used")
	}
	return &Principal{Issuer: c.Iss, Subject: c.Sub, JTI: c.Jti, KeyID: h.Kid, Alg: h.Alg}, nil
}

// verifySignature tries the static keys, then each JWKS URL from cache, then
// each URL again with a forced refresh (the client may have rotated keys).
func (v *ClientAssertionVerifier) verifySignature(ctx context.Context, tok *parsedToken) error {
	try := func(keys []JWK) (matched bool, err error) {
		for _, k := range keys {
			if k.Kid != tok.header.Kid || !k.usableFor(tok.header.Alg) {
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
	if matched, err := try(v.static); matched {
		return err
	}
	urls := v.jwksURLs
	if tok.header.Jku != "" && slices.Contains(urls, tok.header.Jku) {
		urls = append([]string{tok.header.Jku}, slices.DeleteFunc(slices.Clone(urls), func(u string) bool { return u == tok.header.Jku })...)
	}
	for _, refresh := range []bool{false, true} {
		for _, u := range urls {
			keys, err := v.keys.Keys(ctx, u, refresh)
			if err != nil {
				continue // an unreachable JWKS host must not hide keys served by another
			}
			if matched, err := try(keys); matched {
				return err
			}
		}
	}
	return reject("no trusted key matches kid " + quote(tok.header.Kid) + " and alg " + quote(tok.header.Alg))
}

// ClaimsSummary returns iss, kid and jti from an unverified JWT for logging.
// It never returns key material or the signature.
func ClaimsSummary(token string) (iss, kid, jti string) {
	tok, err := parseCompact(strings.TrimSpace(token))
	if err != nil {
		return "", "", ""
	}
	return tok.claims.Iss, tok.header.Kid, tok.claims.Jti
}

// LoadJWKSFile reads a JWK Set document from path.
func LoadJWKSFile(path string) ([]JWK, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("jwks file: %w", err)
	}
	var set JWKSet
	if err := json.Unmarshal(data, &set); err != nil {
		return nil, fmt.Errorf("jwks file %s: %w", path, err)
	}
	if len(set.Keys) == 0 {
		return nil, fmt.Errorf("jwks file %s: no keys", path)
	}
	return set.Keys, nil
}
