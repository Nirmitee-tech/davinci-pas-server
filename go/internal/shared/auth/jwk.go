package auth

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rsa"
	"encoding/base64"
	"errors"
	"fmt"
	"math/big"
	"slices"
	"strings"
)

// JWK is the subset of RFC 7517 fields needed for signature verification.
type JWK struct {
	Kty    string   `json:"kty"`
	Kid    string   `json:"kid,omitempty"`
	Alg    string   `json:"alg,omitempty"`
	Use    string   `json:"use,omitempty"`
	KeyOps []string `json:"key_ops,omitempty"`
	Crv    string   `json:"crv,omitempty"`
	X      string   `json:"x,omitempty"`
	Y      string   `json:"y,omitempty"`
	N      string   `json:"n,omitempty"`
	E      string   `json:"e,omitempty"`
}

// JWKSet is a JSON Web Key Set document.
type JWKSet struct {
	Keys []JWK `json:"keys"`
}

func decodeB64URL(s string) ([]byte, error) {
	return base64.RawURLEncoding.DecodeString(strings.TrimRight(s, "="))
}

// usableFor reports whether the key may verify a signature made with alg.
func (k JWK) usableFor(alg string) bool {
	if k.Alg != "" && k.Alg != alg {
		return false
	}
	if k.Use != "" && k.Use != "sig" {
		return false
	}
	if len(k.KeyOps) > 0 && !slices.Contains(k.KeyOps, "verify") {
		return false
	}
	switch alg {
	case "RS256", "RS384":
		return k.Kty == "RSA"
	case "ES256":
		return k.Kty == "EC" && k.Crv == "P-256"
	case "ES384":
		return k.Kty == "EC" && k.Crv == "P-384"
	}
	return false
}

// PublicKey converts the JWK into an *rsa.PublicKey or *ecdsa.PublicKey.
func (k JWK) PublicKey() (crypto.PublicKey, error) {
	switch k.Kty {
	case "RSA":
		nb, err := decodeB64URL(k.N)
		if err != nil || len(nb) == 0 {
			return nil, errors.New("jwk: invalid RSA modulus")
		}
		eb, err := decodeB64URL(k.E)
		if err != nil || len(eb) == 0 || len(eb) > 4 {
			return nil, errors.New("jwk: invalid RSA exponent")
		}
		e := 0
		for _, b := range eb {
			e = e<<8 | int(b)
		}
		pub := &rsa.PublicKey{N: new(big.Int).SetBytes(nb), E: e}
		if pub.N.BitLen() < 2048 {
			return nil, errors.New("jwk: RSA key shorter than 2048 bits")
		}
		return pub, nil
	case "EC":
		var curve elliptic.Curve
		var size int
		switch k.Crv {
		case "P-256":
			curve, size = elliptic.P256(), 32
		case "P-384":
			curve, size = elliptic.P384(), 48
		default:
			return nil, fmt.Errorf("jwk: unsupported curve %q", k.Crv)
		}
		x, errX := decodeB64URL(k.X)
		y, errY := decodeB64URL(k.Y)
		if errX != nil || errY != nil || len(x) == 0 || len(y) == 0 || len(x) > size || len(y) > size {
			return nil, errors.New("jwk: invalid EC coordinates")
		}
		buf := make([]byte, 1+2*size)
		buf[0] = 4
		copy(buf[1+size-len(x):1+size], x)
		copy(buf[1+2*size-len(y):], y)
		pub, err := ecdsa.ParseUncompressedPublicKey(curve, buf)
		if err != nil {
			return nil, fmt.Errorf("jwk: %w", err)
		}
		return pub, nil
	}
	return nil, fmt.Errorf("jwk: unsupported key type %q", k.Kty)
}
