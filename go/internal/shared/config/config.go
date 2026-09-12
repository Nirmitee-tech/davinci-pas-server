// Package config provides the settings shared by the Nirmitee Da Vinci
// servers (listen address, audit, client JWT trust, outbound calls) and the
// helpers each server uses to load its configuration from a YAML or JSON file
// and environment variables.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// SupportedAlgorithms are the JWT algorithms the shared verifier implements.
var supportedAlgorithms = map[string]bool{"RS256": true, "RS384": true, "ES256": true, "ES384": true}

// SupportedAlgorithm reports whether alg can be verified.
func SupportedAlgorithm(alg string) bool { return supportedAlgorithms[alg] }

// Duration is a time.Duration written as a Go duration string ("5s") in files.
type Duration struct{ time.Duration }

// D wraps a time.Duration.
func D(d time.Duration) Duration { return Duration{d} }

// UnmarshalJSON accepts "5s" style strings or integer milliseconds.
func (d *Duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err == nil {
		v, err := time.ParseDuration(s)
		if err != nil {
			return err
		}
		d.Duration = v
		return nil
	}
	var ms int64
	if err := json.Unmarshal(b, &ms); err != nil {
		return errors.New("duration must be a string such as \"5s\" or integer milliseconds")
	}
	d.Duration = time.Duration(ms) * time.Millisecond
	return nil
}

// MarshalJSON writes the duration as a string.
func (d Duration) MarshalJSON() ([]byte, error) { return json.Marshal(d.String()) }

// UnmarshalYAML accepts "5s" style strings.
func (d *Duration) UnmarshalYAML(n *yaml.Node) error {
	v, err := time.ParseDuration(n.Value)
	if err != nil {
		return err
	}
	d.Duration = v
	return nil
}

// Issuer is a trusted client identity and where its signing keys live.
type Issuer struct {
	Issuer  string `json:"issuer" yaml:"issuer"`
	JWKSURL string `json:"jwksUrl,omitempty" yaml:"jwksUrl,omitempty"`
}

// Rewrite maps an outbound URL prefix to another prefix, for example so a
// containerized server can reach "http://localhost/..." URLs on the host.
type Rewrite struct {
	From string `json:"from" yaml:"from"`
	To   string `json:"to" yaml:"to"`
}

// Auth configures verification of client bearer JWTs.
type Auth struct {
	TrustedIssuers   []Issuer `json:"trustedIssuers" yaml:"trustedIssuers"`
	Algorithms       []string `json:"algorithms" yaml:"algorithms"`
	AudienceBaseURLs []string `json:"audienceBaseUrls" yaml:"audienceBaseUrls"`
	ClockSkew        Duration `json:"clockSkew" yaml:"clockSkew"`
	JWKSCacheTTL     Duration `json:"jwksCacheTtl" yaml:"jwksCacheTtl"`
}

// Outbound configures HTTP calls the server makes (JWKS, client FHIR servers).
type Outbound struct {
	URLRewrites []Rewrite `json:"urlRewrites" yaml:"urlRewrites"`
	Timeout     Duration  `json:"timeout" yaml:"timeout"`
}

// RewriteURL applies the first matching rewrite.
func (o Outbound) RewriteURL(raw string) string {
	for _, r := range o.URLRewrites {
		if r.From != "" && strings.HasPrefix(raw, r.From) {
			return r.To + strings.TrimPrefix(raw, r.From)
		}
	}
	return raw
}

// Common holds the settings every server shares.
type Common struct {
	ListenAddr      string   `json:"listenAddr" yaml:"listenAddr"`
	LogLevel        string   `json:"logLevel" yaml:"logLevel"`
	AuditDir        string   `json:"auditDir" yaml:"auditDir"`
	ShutdownTimeout Duration `json:"shutdownTimeout" yaml:"shutdownTimeout"`
	Auth            Auth     `json:"auth" yaml:"auth"`
	Outbound        Outbound `json:"outbound" yaml:"outbound"`
}

// DefaultCommon returns shared defaults for a server listening on listenAddr.
func DefaultCommon(listenAddr string) Common {
	return Common{
		ListenAddr:      listenAddr,
		LogLevel:        "info",
		AuditDir:        "data/audit",
		ShutdownTimeout: D(10 * time.Second),
		Auth: Auth{
			Algorithms:   []string{"RS384", "ES384", "RS256", "ES256"},
			ClockSkew:    D(60 * time.Second),
			JWKSCacheTTL: D(5 * time.Minute),
		},
		Outbound: Outbound{Timeout: D(3 * time.Second)},
	}
}

// Normalize trims issuers, fills default JWKS URLs ({iss}/jwks.json) and
// removes duplicates.
func (c *Common) Normalize() {
	seen := map[string]bool{}
	var issuers []Issuer
	for _, iss := range c.Auth.TrustedIssuers {
		iss.Issuer = strings.TrimRight(strings.TrimSpace(iss.Issuer), "/")
		if iss.Issuer == "" || seen[iss.Issuer] {
			continue
		}
		seen[iss.Issuer] = true
		if iss.JWKSURL == "" {
			iss.JWKSURL = iss.Issuer + "/jwks.json"
		}
		issuers = append(issuers, iss)
	}
	c.Auth.TrustedIssuers = issuers
	for i, a := range c.Auth.Algorithms {
		c.Auth.Algorithms[i] = strings.ToUpper(strings.TrimSpace(a))
	}
	for i, u := range c.Auth.AudienceBaseURLs {
		c.Auth.AudienceBaseURLs[i] = strings.TrimRight(strings.TrimSpace(u), "/")
	}
}

// Validate reports invalid shared settings. requireIssuers is true when the
// server enforces client authentication.
func (c Common) Validate(requireIssuers bool) error {
	var errs []error
	if c.ListenAddr == "" {
		errs = append(errs, errors.New("listenAddr is required"))
	}
	if len(c.Auth.Algorithms) == 0 {
		errs = append(errs, errors.New("at least one JWT algorithm is required"))
	}
	for _, a := range c.Auth.Algorithms {
		if !SupportedAlgorithm(a) {
			errs = append(errs, fmt.Errorf("unsupported JWT algorithm %q", a))
		}
	}
	if requireIssuers && len(c.Auth.TrustedIssuers) == 0 {
		errs = append(errs, errors.New("auth.trustedIssuers is required when authentication is enabled"))
	}
	if c.ShutdownTimeout.Duration <= 0 {
		errs = append(errs, errors.New("shutdownTimeout must be positive"))
	}
	if c.Auth.JWKSCacheTTL.Duration <= 0 {
		errs = append(errs, errors.New("auth.jwksCacheTtl must be positive"))
	}
	if c.Auth.ClockSkew.Duration < 0 {
		errs = append(errs, errors.New("auth.clockSkew must not be negative"))
	}
	if c.Outbound.Timeout.Duration <= 0 {
		errs = append(errs, errors.New("outbound.timeout must be positive"))
	}
	return errors.Join(errs...)
}

// LoadFile decodes a YAML (default) or JSON (.json) file into dst. Fields
// absent from the file keep their current values.
func LoadFile(path string, dst any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("config file: %w", err)
	}
	if strings.EqualFold(filepath.Ext(path), ".json") {
		err = json.Unmarshal(data, dst)
	} else {
		err = yaml.Unmarshal(data, dst)
	}
	if err != nil {
		return fmt.Errorf("config file %s: %w", path, err)
	}
	return nil
}

// Env reads environment variables named PREFIX_NAME (or NAME with an empty
// prefix) and collects parse errors.
type Env struct {
	prefix string
	getenv func(string) string
	errs   []error
}

// NewEnv returns an Env; getenv defaults to os.Getenv.
func NewEnv(prefix string, getenv func(string) string) *Env {
	if getenv == nil {
		getenv = os.Getenv
	}
	return &Env{prefix: prefix, getenv: getenv}
}

// Name returns the full variable name.
func (e *Env) Name(name string) string {
	if e.prefix == "" {
		return name
	}
	return e.prefix + "_" + name
}

// Get returns the trimmed value.
func (e *Env) Get(name string) string { return strings.TrimSpace(e.getenv(e.Name(name))) }

func (e *Env) fail(name string, err error) {
	e.errs = append(e.errs, fmt.Errorf("%s: %w", e.Name(name), err))
}

// String sets dst when the variable is non-empty.
func (e *Env) String(name string, dst *string) {
	if v := e.Get(name); v != "" {
		*dst = v
	}
}

// Bool sets dst when the variable is non-empty and parses as a boolean.
func (e *Env) Bool(name string, dst *bool) {
	if v := e.Get(name); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			e.fail(name, err)
			return
		}
		*dst = b
	}
}

// Int sets dst when the variable parses as an integer.
func (e *Env) Int(name string, dst *int) {
	if v := e.Get(name); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			e.fail(name, err)
			return
		}
		*dst = n
	}
}

// Duration sets dst when the variable parses as a Go duration ("5s").
func (e *Env) Duration(name string, dst *Duration) {
	if v := e.Get(name); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			e.fail(name, err)
			return
		}
		dst.Duration = d
	}
}

// Scaled sets dst from a non-negative integer count of unit.
func (e *Env) Scaled(name string, dst *Duration, unit time.Duration) {
	if v := e.Get(name); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			e.fail(name, errors.New("must be a non-negative integer"))
			return
		}
		dst.Duration = time.Duration(n) * unit
	}
}

// List replaces dst with a comma-separated list when the variable is set.
func (e *Env) List(name string, dst *[]string) {
	if v := e.Get(name); v != "" {
		*dst = SplitList(v)
	}
}

// Pairs parses "k=v;k2=v2" style values. ok is false when the variable is unset.
func (e *Env) Pairs(name, pairSep, kvSep string) (map[string]string, bool) {
	v := e.Get(name)
	if v == "" {
		return nil, false
	}
	out := map[string]string{}
	for _, item := range strings.Split(v, pairSep) {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		k, val, found := strings.Cut(item, kvSep)
		if !found {
			e.fail(name, fmt.Errorf("entry %q must be key%svalue", item, kvSep))
			continue
		}
		out[strings.TrimSpace(k)] = strings.TrimSpace(val)
	}
	return out, true
}

// Rewrites parses "from|to" entries separated by commas.
func (e *Env) Rewrites(name string, dst *[]Rewrite) {
	v := e.Get(name)
	if v == "" {
		return
	}
	var list []Rewrite
	for _, item := range SplitList(v) {
		from, to, ok := strings.Cut(item, "|")
		if !ok {
			e.fail(name, fmt.Errorf("entry %q must be from|to", item))
			continue
		}
		list = append(list, Rewrite{From: strings.TrimSpace(from), To: strings.TrimSpace(to)})
	}
	*dst = list
}

// Err returns every parse error.
func (e *Env) Err() error { return errors.Join(e.errs...) }

// SplitList splits a comma-separated list, trimming and dropping empty items.
func SplitList(v string) []string {
	var out []string
	for _, p := range strings.Split(v, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
