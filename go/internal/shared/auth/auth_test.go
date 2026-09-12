package auth

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

type testKey struct {
	alg  string
	kid  string
	priv crypto.Signer
}

func (k testKey) jwk() JWK {
	switch pub := k.priv.Public().(type) {
	case *rsa.PublicKey:
		return JWK{Kty: "RSA", Kid: k.kid, Alg: k.alg, N: b64(pub.N.Bytes()), E: b64(big.NewInt(int64(pub.E)).Bytes())}
	case *ecdsa.PublicKey:
		raw, _ := pub.Bytes()
		size := (len(raw) - 1) / 2
		crv := "P-384"
		if size == 32 {
			crv = "P-256"
		}
		return JWK{Kty: "EC", Kid: k.kid, Alg: k.alg, Crv: crv, X: b64(raw[1 : 1+size]), Y: b64(raw[1+size:])}
	}
	return JWK{}
}

func newKey(t *testing.T, alg, kid string) testKey {
	t.Helper()
	var priv crypto.Signer
	var err error
	switch alg {
	case "RS256", "RS384":
		priv, err = rsa.GenerateKey(rand.Reader, 2048)
	case "ES256":
		priv, err = ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	case "ES384":
		priv, err = ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	}
	if err != nil {
		t.Fatal(err)
	}
	return testKey{alg: alg, kid: kid, priv: priv}
}

func sign(t *testing.T, k testKey, header, claims map[string]any) string {
	t.Helper()
	hb, _ := json.Marshal(header)
	cb, _ := json.Marshal(claims)
	input := b64(hb) + "." + b64(cb)
	var digest []byte
	var h crypto.Hash
	switch k.alg {
	case "RS256", "ES256":
		d := sha256.Sum256([]byte(input))
		digest, h = d[:], crypto.SHA256
	default:
		d := sha512.Sum384([]byte(input))
		digest, h = d[:], crypto.SHA384
	}
	var sig []byte
	switch priv := k.priv.(type) {
	case *rsa.PrivateKey:
		s, err := rsa.SignPKCS1v15(rand.Reader, priv, h, digest)
		if err != nil {
			t.Fatal(err)
		}
		sig = s
	case *ecdsa.PrivateKey:
		r, s, err := ecdsa.Sign(rand.Reader, priv, digest)
		if err != nil {
			t.Fatal(err)
		}
		size := (priv.Curve.Params().BitSize + 7) / 8
		sig = append(r.FillBytes(make([]byte, size)), s.FillBytes(make([]byte, size))...)
	}
	return input + "." + b64(sig)
}

type fixture struct {
	srv      *httptest.Server
	issuer   string
	keys     []JWK
	fetches  atomic.Int32
	verifier *Verifier
}

func newFixture(t *testing.T, keys ...testKey) *fixture {
	t.Helper()
	f := &fixture{}
	for _, k := range keys {
		f.keys = append(f.keys, k.jwk())
	}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/jwks.json") {
			http.NotFound(w, r)
			return
		}
		f.fetches.Add(1)
		_ = json.NewEncoder(w).Encode(JWKSet{Keys: f.keys})
	}))
	t.Cleanup(f.srv.Close)
	f.issuer = f.srv.URL + "/custom/crd_server_v221"
	fetcher := NewKeyFetcher(f.srv.Client(), 5*time.Minute, nil)
	f.verifier = NewVerifier([]TrustedIssuer{{Issuer: f.issuer}}, []string{"RS384", "ES384", "ES256", "RS256"}, time.Minute, fetcher)
	return f
}

const aud = "http://host.docker.internal:8081/v221/cds-services/order-sign-crd"

func (f *fixture) claims(overrides map[string]any) map[string]any {
	now := time.Now().Unix()
	c := map[string]any{"iss": f.issuer, "aud": aud, "exp": now + 300, "iat": now, "jti": randomJTI()}
	for k, v := range overrides {
		if v == nil {
			delete(c, k)
		} else {
			c[k] = v
		}
	}
	return c
}

func (f *fixture) header(k testKey) map[string]any {
	return map[string]any{"alg": k.alg, "typ": "JWT", "kid": k.kid, "jku": f.issuer + "/jwks.json"}
}

var jtiCounter atomic.Int64

func randomJTI() string {
	return "jti-" + big.NewInt(jtiCounter.Add(1)).String() + "-" + big.NewInt(time.Now().UnixNano()).String()
}

func TestVerifyAcceptsInfernoStyleTokens(t *testing.T) {
	for _, alg := range []string{"ES384", "RS384", "ES256", "RS256"} {
		t.Run(alg, func(t *testing.T) {
			k := newKey(t, alg, "kid-"+alg)
			f := newFixture(t, k)
			tok := sign(t, k, f.header(k), f.claims(nil))
			p, err := f.verifier.Verify(context.Background(), tok, []string{aud})
			if err != nil {
				t.Fatalf("Verify: %v", err)
			}
			if p.Issuer != f.issuer || p.KeyID != k.kid || p.Alg != alg {
				t.Fatalf("unexpected principal %+v", p)
			}
		})
	}
}

func TestVerifyAcceptsArrayAudienceAndTrailingSlash(t *testing.T) {
	k := newKey(t, "ES384", "k1")
	f := newFixture(t, k)
	tok := sign(t, k, f.header(k), f.claims(map[string]any{"aud": []string{"https://other.example.test", aud + "/"}}))
	if _, err := f.verifier.Verify(context.Background(), tok, []string{aud}); err != nil {
		t.Fatalf("Verify: %v", err)
	}
}

func TestVerifyRejections(t *testing.T) {
	k := newKey(t, "ES384", "k1")
	other := newKey(t, "ES384", "k1")
	f := newFixture(t, k)
	now := time.Now().Unix()

	cases := []struct {
		name   string
		token  func() string
		reason string
	}{
		{"garbage", func() string { return "not-a-jwt" }, "compact JWS"},
		{"alg none", func() string {
			h := f.header(k)
			h["alg"] = "none"
			return strings.TrimSuffix(sign(t, k, h, f.claims(nil)), "") // signature ignored, alg rejected first
		}, "unsupported alg"},
		{"HS256", func() string {
			h := f.header(k)
			h["alg"] = "HS256"
			return sign(t, k, h, f.claims(nil))
		}, "unsupported alg"},
		{"untrusted issuer", func() string {
			return sign(t, k, f.header(k), f.claims(map[string]any{"iss": "https://evil.example.test"}))
		}, "untrusted issuer"},
		{"jku outside issuer", func() string {
			h := f.header(k)
			h["jku"] = "https://evil.example.test/jwks.json"
			return sign(t, k, h, f.claims(nil))
		}, "jku"},
		{"bad signature", func() string { return sign(t, other, f.header(other), f.claims(nil)) }, "signature"},
		{"wrong audience", func() string {
			return sign(t, k, f.header(k), f.claims(map[string]any{"aud": "http://host.docker.internal:8081/v221/cds-services/order-select-crd"}))
		}, "aud"},
		{"expired", func() string {
			return sign(t, k, f.header(k), f.claims(map[string]any{"exp": now - 3600, "iat": now - 4000}))
		}, "expired"},
		{"missing exp", func() string { return sign(t, k, f.header(k), f.claims(map[string]any{"exp": nil})) }, "exp"},
		{"iat in future", func() string {
			return sign(t, k, f.header(k), f.claims(map[string]any{"iat": now + 3600}))
		}, "iat"},
		{"missing jti", func() string { return sign(t, k, f.header(k), f.claims(map[string]any{"jti": nil})) }, "jti"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := f.verifier.Verify(context.Background(), tc.token(), []string{aud})
			if err == nil {
				t.Fatal("expected rejection")
			}
			var ae *Error
			if !asAuthError(err, &ae) || !strings.Contains(ae.Reason, tc.reason) {
				t.Fatalf("got %v, want reason containing %q", err, tc.reason)
			}
		})
	}
}

func asAuthError(err error, target **Error) bool {
	e, ok := err.(*Error)
	if ok {
		*target = e
	}
	return ok
}

func TestVerifyRejectsReplayedJTI(t *testing.T) {
	k := newKey(t, "RS384", "k1")
	f := newFixture(t, k)
	tok := sign(t, k, f.header(k), f.claims(map[string]any{"jti": "same-jti"}))
	if _, err := f.verifier.Verify(context.Background(), tok, []string{aud}); err != nil {
		t.Fatalf("first use: %v", err)
	}
	if _, err := f.verifier.Verify(context.Background(), tok, []string{aud}); err == nil || !strings.Contains(err.Error(), "jti") {
		t.Fatalf("replay should be rejected, got %v", err)
	}
}

func TestVerifyRefetchesJWKSOnUnknownKid(t *testing.T) {
	k1 := newKey(t, "ES384", "old")
	f := newFixture(t, k1)
	if _, err := f.verifier.Verify(context.Background(), sign(t, k1, f.header(k1), f.claims(nil)), []string{aud}); err != nil {
		t.Fatal(err)
	}
	k2 := newKey(t, "ES384", "rotated")
	f.keys = append(f.keys, k2.jwk())
	f.verifier.keys.now = func() time.Time { return time.Now().Add(time.Minute) }
	if _, err := f.verifier.Verify(context.Background(), sign(t, k2, f.header(k2), f.claims(nil)), []string{aud}); err != nil {
		t.Fatalf("rotated key: %v", err)
	}
	if got := f.fetches.Load(); got != 2 {
		t.Fatalf("expected 2 JWKS fetches, got %d", got)
	}
}

func TestVerifyWithoutKidTriesMatchingKeys(t *testing.T) {
	k := newKey(t, "ES384", "k1")
	f := newFixture(t, newKey(t, "RS384", "rsa"), k)
	h := f.header(k)
	delete(h, "kid")
	delete(h, "jku")
	if _, err := f.verifier.Verify(context.Background(), sign(t, k, h, f.claims(nil)), []string{aud}); err != nil {
		t.Fatalf("Verify: %v", err)
	}
}

func TestNormalizeURL(t *testing.T) {
	cases := map[string]string{
		"HTTP://Host.Docker.Internal:8081/v221/cds-services/x/": "http://host.docker.internal:8081/v221/cds-services/x",
		"https://abc.ngrok-free.app:443/v221/cds-services/x":    "https://abc.ngrok-free.app/v221/cds-services/x",
		"http://localhost:80/a":                                 "http://localhost/a",
	}
	for in, want := range cases {
		if got := NormalizeURL(in); got != want {
			t.Errorf("NormalizeURL(%q) = %q, want %q", in, got, want)
		}
	}
}
