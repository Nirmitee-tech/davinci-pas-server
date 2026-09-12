package auth

import (
	"bytes"
	"crypto"
	"crypto/ecdsa"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
	"encoding/json"
	"errors"
	"math"
	"math/big"
	"strings"
	"time"
)

type jwsHeader struct {
	Alg  string   `json:"alg"`
	Kid  string   `json:"kid"`
	Jku  string   `json:"jku"`
	Typ  string   `json:"typ"`
	Crit []string `json:"crit"`
}

type audience []string

func (a *audience) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err == nil {
		*a = audience{s}
		return nil
	}
	var list []string
	if err := json.Unmarshal(b, &list); err != nil {
		return errors.New("aud must be a string or an array of strings")
	}
	*a = list
	return nil
}

type jwtClaims struct {
	Iss string       `json:"iss"`
	Sub string       `json:"sub"`
	Aud audience     `json:"aud"`
	Exp *json.Number `json:"exp"`
	Iat *json.Number `json:"iat"`
	Nbf *json.Number `json:"nbf"`
	Jti string       `json:"jti"`
}

type parsedToken struct {
	header       jwsHeader
	claims       jwtClaims
	signingInput []byte
	signature    []byte
}

func numericDate(n *json.Number) (time.Time, bool) {
	if n == nil {
		return time.Time{}, false
	}
	f, err := n.Float64()
	if err != nil || math.IsNaN(f) || math.IsInf(f, 0) {
		return time.Time{}, false
	}
	return time.Unix(int64(f), 0), true
}

func parseCompact(token string) (*parsedToken, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
		return nil, errors.New("token is not a compact JWS")
	}
	hb, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return nil, errors.New("token header is not base64url")
	}
	var h jwsHeader
	if err := json.Unmarshal(hb, &h); err != nil {
		return nil, errors.New("token header is not JSON")
	}
	pb, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, errors.New("token payload is not base64url")
	}
	var c jwtClaims
	dec := json.NewDecoder(bytes.NewReader(pb))
	dec.UseNumber()
	if err := dec.Decode(&c); err != nil {
		return nil, errors.New("token payload is not a valid claims object")
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return nil, errors.New("token signature is not base64url")
	}
	return &parsedToken{
		header:       h,
		claims:       c,
		signingInput: []byte(parts[0] + "." + parts[1]),
		signature:    sig,
	}, nil
}

func verifySignature(alg string, key crypto.PublicKey, input, sig []byte) error {
	switch alg {
	case "RS256", "RS384":
		pub, ok := key.(*rsa.PublicKey)
		if !ok {
			return errors.New("key type does not match " + alg)
		}
		if alg == "RS256" {
			d := sha256.Sum256(input)
			return rsa.VerifyPKCS1v15(pub, crypto.SHA256, d[:], sig)
		}
		d := sha512.Sum384(input)
		return rsa.VerifyPKCS1v15(pub, crypto.SHA384, d[:], sig)
	case "ES256", "ES384":
		pub, ok := key.(*ecdsa.PublicKey)
		if !ok {
			return errors.New("key type does not match " + alg)
		}
		var digest []byte
		size := 32
		if alg == "ES256" {
			d := sha256.Sum256(input)
			digest = d[:]
		} else {
			d := sha512.Sum384(input)
			digest, size = d[:], 48
		}
		if len(sig) != 2*size {
			return errors.New(alg + " signature has the wrong length")
		}
		r := new(big.Int).SetBytes(sig[:size])
		s := new(big.Int).SetBytes(sig[size:])
		if !ecdsa.Verify(pub, digest, r, s) {
			return errors.New("signature verification failed")
		}
		return nil
	}
	return errors.New("unsupported algorithm")
}
