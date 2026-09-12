package config

import (
	"errors"
	"time"
)

// BackendServices configures a SMART Backend Services token endpoint
// (client_credentials grant with a private_key_jwt client assertion), used by
// servers that issue their own access tokens (DTR, PAS).
type BackendServices struct {
	// ClientIDs are the registered client ids (assertion iss and sub).
	ClientIDs []string `json:"clientIds" yaml:"clientIds"`
	// JWKSURLs are trusted JWK Set URLs holding client signing keys.
	JWKSURLs []string `json:"jwksUrls" yaml:"jwksUrls"`
	// JWKSFile is an optional local JWK Set checked before JWKSURLs.
	JWKSFile string `json:"jwksFile" yaml:"jwksFile"`
	// TokenTTL is the lifetime of issued access tokens.
	TokenTTL Duration `json:"tokenTtl" yaml:"tokenTtl"`
	// MaxAssertionLifetime bounds how far ahead an assertion exp may be.
	MaxAssertionLifetime Duration `json:"maxAssertionLifetime" yaml:"maxAssertionLifetime"`
}

// DefaultBackendServices returns SMART defaults: 5 minute tokens and assertions.
func DefaultBackendServices() BackendServices {
	return BackendServices{TokenTTL: D(5 * time.Minute), MaxAssertionLifetime: D(5 * time.Minute)}
}

// Validate reports invalid token endpoint settings.
func (b BackendServices) Validate() error {
	var errs []error
	if len(b.ClientIDs) == 0 {
		errs = append(errs, errors.New("backendServices.clientIds is required"))
	}
	if len(b.JWKSURLs) == 0 && b.JWKSFile == "" {
		errs = append(errs, errors.New("backendServices needs jwksUrls or jwksFile"))
	}
	if b.TokenTTL.Duration <= 0 {
		errs = append(errs, errors.New("backendServices.tokenTtl must be positive"))
	}
	if b.MaxAssertionLifetime.Duration <= 0 {
		errs = append(errs, errors.New("backendServices.maxAssertionLifetime must be positive"))
	}
	return errors.Join(errs...)
}
