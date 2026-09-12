package auth

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const tokenURL = "http://host.docker.internal:8083/v220/fhir/auth/token"

func assertionClaims(overrides map[string]any) map[string]any {
	now := time.Now().Unix()
	c := map[string]any{"iss": "client-1", "sub": "client-1", "aud": tokenURL, "exp": now + 240, "iat": now, "jti": randomJTI()}
	for k, v := range overrides {
		if v == nil {
			delete(c, k)
		} else {
			c[k] = v
		}
	}
	return c
}

func jwksServer(t *testing.T, keys *[]JWK, hits *atomic.Int32) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		_ = json.NewEncoder(w).Encode(JWKSet{Keys: *keys})
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestClientAssertionAcceptsSMARTAlgorithms(t *testing.T) {
	for _, alg := range []string{"ES384", "RS384"} {
		t.Run(alg, func(t *testing.T) {
			k := newKey(t, alg, "kid-"+alg)
			keys := []JWK{k.jwk()}
			var hits atomic.Int32
			srv := jwksServer(t, &keys, &hits)
			v := NewClientAssertionVerifier(ClientAssertionOptions{
				ClientIDs: []string{"client-1"}, Algorithms: []string{"RS384", "ES384"},
				JWKSURLs:  []string{"http://127.0.0.1:1/unreachable.json", srv.URL + "/jwks.json"},
				ClockSkew: time.Minute, Keys: NewKeyFetcher(srv.Client(), time.Minute, nil),
			})
			tok := sign(t, k, map[string]any{"alg": alg, "kid": k.kid, "typ": "JWT"}, assertionClaims(nil))
			p, err := v.Verify(context.Background(), tok, []string{tokenURL})
			if err != nil {
				t.Fatalf("Verify: %v", err)
			}
			if p.Issuer != "client-1" || p.KeyID != k.kid {
				t.Fatalf("principal %+v", p)
			}
		})
	}
}

func TestClientAssertionRejections(t *testing.T) {
	k := newKey(t, "ES384", "k1")
	impostor := newKey(t, "ES384", "k1")
	rs256 := newKey(t, "RS256", "k2")
	v := NewClientAssertionVerifier(ClientAssertionOptions{
		ClientIDs: []string{"client-1"}, Algorithms: []string{"RS384", "ES384"},
		StaticKeys: []JWK{k.jwk(), rs256.jwk()}, ClockSkew: time.Minute,
	})
	hdr := map[string]any{"alg": "ES384", "kid": "k1", "typ": "JWT"}
	cases := []struct {
		name  string
		token string
		want  string
	}{
		{"malformed", "abc", "compact JWS"},
		{"alg", sign(t, rs256, map[string]any{"alg": "RS256", "kid": "k2"}, assertionClaims(nil)), "unsupported alg"},
		{"missing kid", sign(t, k, map[string]any{"alg": "ES384"}, assertionClaims(nil)), "missing kid"},
		{"unknown kid", sign(t, k, map[string]any{"alg": "ES384", "kid": "zz"}, assertionClaims(nil)), "no trusted key"},
		{"bad signature", sign(t, impostor, hdr, assertionClaims(nil)), "signature verification failed"},
		{"unknown client", sign(t, k, hdr, assertionClaims(map[string]any{"iss": "x", "sub": "x"})), "unknown client"},
		{"sub differs", sign(t, k, hdr, assertionClaims(map[string]any{"sub": "other"})), "sub must equal iss"},
		{"aud", sign(t, k, hdr, assertionClaims(map[string]any{"aud": "https://other.test/token"})), "aud does not match"},
		{"expired", sign(t, k, hdr, assertionClaims(map[string]any{"exp": time.Now().Add(-2 * time.Minute).Unix()})), "expired"},
		{"exp too far", sign(t, k, hdr, assertionClaims(map[string]any{"exp": time.Now().Add(10 * time.Minute).Unix()})), "in the future"},
		{"missing exp", sign(t, k, hdr, assertionClaims(map[string]any{"exp": nil})), "missing exp"},
		{"missing jti", sign(t, k, hdr, assertionClaims(map[string]any{"jti": nil})), "missing jti"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := v.Verify(context.Background(), tc.token, []string{tokenURL})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err %v, want %q", err, tc.want)
			}
		})
	}
	// Replay, and an array aud with a trailing slash.
	tok := sign(t, k, hdr, assertionClaims(map[string]any{"aud": []string{"https://x.test", tokenURL + "/"}}))
	if _, err := v.Verify(context.Background(), tok, []string{tokenURL}); err != nil {
		t.Fatalf("first use: %v", err)
	}
	if _, err := v.Verify(context.Background(), tok, []string{tokenURL}); err == nil || !strings.Contains(err.Error(), "already been used") {
		t.Fatalf("replay: %v", err)
	}
}

func TestClientAssertionKeyRotation(t *testing.T) {
	old := newKey(t, "ES384", "old")
	rotated := newKey(t, "ES384", "new")
	keys := []JWK{old.jwk()}
	var hits atomic.Int32
	srv := jwksServer(t, &keys, &hits)
	fetcher := NewKeyFetcher(srv.Client(), time.Hour, nil)
	v := NewClientAssertionVerifier(ClientAssertionOptions{
		ClientIDs: []string{"client-1"}, Algorithms: []string{"ES384"}, JWKSURLs: []string{srv.URL}, Keys: fetcher,
	})
	if _, err := v.Verify(context.Background(), sign(t, old, map[string]any{"alg": "ES384", "kid": "old"}, assertionClaims(nil)), []string{tokenURL}); err != nil {
		t.Fatal(err)
	}
	keys = []JWK{rotated.jwk()}
	// Force the refresh interval to have elapsed.
	fetcher.now = func() time.Time { return time.Now().Add(time.Minute) }
	if _, err := v.Verify(context.Background(), sign(t, rotated, map[string]any{"alg": "ES384", "kid": "new"}, assertionClaims(nil)), []string{tokenURL}); err != nil {
		t.Fatalf("rotated key: %v", err)
	}
	if hits.Load() != 2 {
		t.Fatalf("jwks fetches = %d, want 2", hits.Load())
	}
}

func TestLoadJWKSFileAndSummary(t *testing.T) {
	k := newKey(t, "ES384", "file-key")
	path := filepath.Join(t.TempDir(), "jwks.json")
	b, _ := json.Marshal(JWKSet{Keys: []JWK{k.jwk()}})
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
	keys, err := LoadJWKSFile(path)
	if err != nil || len(keys) != 1 || keys[0].Kid != "file-key" {
		t.Fatalf("LoadJWKSFile: %v %v", keys, err)
	}
	if err := os.WriteFile(path, []byte(`{"keys":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadJWKSFile(path); err == nil {
		t.Fatal("empty key set accepted")
	}
	tok := sign(t, k, map[string]any{"alg": "ES384", "kid": "file-key"}, assertionClaims(map[string]any{"jti": "j-1"}))
	if iss, kid, jti := ClaimsSummary(tok); iss != "client-1" || kid != "file-key" || jti != "j-1" {
		t.Fatalf("summary %s %s %s", iss, kid, jti)
	}
}

func TestTokenStore(t *testing.T) {
	s := NewTokenStore()
	tok, err := s.Issue(IssuedToken{ClientID: "c", Scope: "system/*.rs", Audience: "v220", Expires: time.Now().Add(time.Minute)})
	if err != nil || len(tok) != 32 {
		t.Fatalf("Issue: %q %v", tok, err)
	}
	if got, ok := s.Lookup(tok); !ok || got.ClientID != "c" || got.Audience != "v220" {
		t.Fatalf("Lookup: %+v %v", got, ok)
	}
	if _, ok := s.Lookup("unknown"); ok {
		t.Fatal("unknown token accepted")
	}
	s.now = func() time.Time { return time.Now().Add(2 * time.Minute) }
	if _, ok := s.Lookup(tok); ok {
		t.Fatal("expired token accepted")
	}
	if !EqualConstantTime("abc", "abc") || EqualConstantTime("abc", "abd") {
		t.Fatal("EqualConstantTime")
	}
}
