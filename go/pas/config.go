package pas

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Nirmitee-tech/davinci-pas-server/go/internal/shared/audit"
	"github.com/Nirmitee-tech/davinci-pas-server/go/internal/shared/config"
)

// EnvPrefix prefixes the PAS environment variables (PAS_PORT, ...).
const EnvPrefix = "PAS"

// DefaultPort is the standalone listen port (SPEC section 1).
const DefaultPort = 8085

// Config configures the PAS handler. Runtime collaborators (Rules, Store,
// Logger, Audit, HTTPClient, Now) are optional and never read from the environment.
type Config struct {
	config.Common `yaml:",inline"`

	// Prefix is prepended to every route, e.g. "/pas" when several Da Vinci
	// servers share one host. FHIR bases are {prefix}/v221/fhir and {prefix}/v201/fhir.
	Prefix string `json:"prefix" yaml:"prefix"`
	// PublicBaseURL fixes the external origin (e.g. a tunnel URL) instead of
	// deriving it from the request. It is used in notifications and resource URLs.
	PublicBaseURL string `json:"publicBaseUrl" yaml:"publicBaseUrl"`
	// RulesFile is the adjudication rules file. Empty means spec/adjudication-rules.json
	// (or ../spec/adjudication-rules.json) when present, else the bundled copy.
	RulesFile string `json:"rulesFile" yaml:"rulesFile"`
	// AuthTokens are the accepted bearer tokens for FHIR routes other than GET /metadata.
	// Empty disables authentication.
	AuthTokens []string `json:"authTokens" yaml:"authTokens"`
	// PendedAutoFinalize finalizes auto-finalizable pended items this long after the submit; 0 disables it.
	PendedAutoFinalize config.Duration `json:"pendedAutoFinalize" yaml:"pendedAutoFinalize"`
	// NotificationRetry is the interval between delivery attempts of one notification.
	NotificationRetry config.Duration `json:"notificationRetry" yaml:"notificationRetry"`
	// NotificationMax stops retrying a notification after this long.
	NotificationMax config.Duration `json:"notificationMax" yaml:"notificationMax"`
	// HandshakeDelay is the pause between the Subscription create response and the handshake.
	HandshakeDelay config.Duration `json:"handshakeDelay" yaml:"handshakeDelay"`
	// EnableAdmin serves the test-only control routes {prefix}/admin/pended/finalize,
	// {prefix}/admin/reset and {prefix}/admin/notifications. Off by default. Only the
	// PAS_ADMIN_ENABLED environment variable sets it; a config file cannot.
	EnableAdmin bool `json:"-" yaml:"-"`
	// DataDir holds the audit log ({DataDir}/audit/YYYY-MM-DD.jsonl).
	DataDir string `json:"dataDir" yaml:"dataDir"`

	Rules      *Rules           `json:"-" yaml:"-"`
	Store      Store            `json:"-" yaml:"-"`
	Logger     *slog.Logger     `json:"-" yaml:"-"`
	Audit      *audit.Logger    `json:"-" yaml:"-"`
	HTTPClient *http.Client     `json:"-" yaml:"-"`
	Now        func() time.Time `json:"-" yaml:"-"`
}

// DefaultConfig returns the built-in configuration (SPEC section 1).
func DefaultConfig() Config {
	c := Config{
		Common:             config.DefaultCommon(fmt.Sprintf(":%d", DefaultPort)),
		PendedAutoFinalize: config.D(10 * time.Second),
		NotificationRetry:  config.D(3 * time.Second),
		NotificationMax:    config.D(280 * time.Second),
		HandshakeDelay:     config.D(time.Second),
		DataDir:            "data",
	}
	c.AuditDir = filepath.Join(c.DataDir, "audit")
	c.Outbound.Timeout = config.D(10 * time.Second)
	return c
}

// LoadConfig applies environment variables over the defaults. Variables from
// SPEC section 1: PAS_PORT, PAS_PREFIX, PAS_PUBLIC_BASE_URL, PAS_RULES_FILE,
// PAS_AUTH_TOKENS, PAS_PENDED_AUTO_FINALIZE_SECONDS, PAS_NOTIFICATION_RETRY_SECONDS,
// PAS_NOTIFICATION_MAX_SECONDS, PAS_ADMIN_ENABLED (test-only control routes,
// enabled only by the exact value "true"), PAS_DATA_DIR.
// Extras: PAS_LISTEN (full listen address), PAS_CONFIG_FILE, PAS_LOG_LEVEL, PAS_URL_REWRITES.
func LoadConfig(getenv func(string) string) (Config, error) {
	if getenv == nil {
		getenv = os.Getenv
	}
	c := DefaultConfig()
	env := config.NewEnv(EnvPrefix, getenv)
	if path := env.Get("CONFIG_FILE"); path != "" {
		if err := config.LoadFile(path, &c); err != nil {
			return Config{}, err
		}
	}
	port := 0
	env.Int("PORT", &port)
	if port < 0 || port > 65535 {
		return Config{}, fmt.Errorf("%s: must be a TCP port", env.Name("PORT"))
	}
	if port > 0 {
		c.ListenAddr = fmt.Sprintf(":%d", port)
	}
	env.String("LISTEN", &c.ListenAddr)
	env.String("PREFIX", &c.Prefix)
	env.String("PUBLIC_BASE_URL", &c.PublicBaseURL)
	env.String("RULES_FILE", &c.RulesFile)
	env.List("AUTH_TOKENS", &c.AuthTokens)
	env.Scaled("PENDED_AUTO_FINALIZE_SECONDS", &c.PendedAutoFinalize, time.Second)
	env.Scaled("NOTIFICATION_RETRY_SECONDS", &c.NotificationRetry, time.Second)
	env.Scaled("NOTIFICATION_MAX_SECONDS", &c.NotificationMax, time.Second)
	// SPEC 1.2.0 section 11: only the exact value "true" enables the test-only control routes.
	c.EnableAdmin = getenv(env.Name("ADMIN_ENABLED")) == "true"
	dataDir := c.DataDir
	env.String("DATA_DIR", &c.DataDir)
	if c.DataDir != dataDir || c.AuditDir == "" {
		c.AuditDir = filepath.Join(c.DataDir, "audit")
	}
	env.String("LOG_LEVEL", &c.LogLevel)
	env.Rewrites("URL_REWRITES", &c.Outbound.URLRewrites)
	if err := env.Err(); err != nil {
		return Config{}, err
	}
	c.normalize()
	if err := c.Validate(); err != nil {
		return Config{}, err
	}
	return c, nil
}

func (c *Config) normalize() {
	c.Common.Normalize()
	c.Prefix = strings.TrimRight(strings.TrimSpace(c.Prefix), "/")
	if c.Prefix != "" && !strings.HasPrefix(c.Prefix, "/") {
		c.Prefix = "/" + c.Prefix
	}
	c.PublicBaseURL = strings.TrimRight(strings.TrimSpace(c.PublicBaseURL), "/")
	var tokens []string
	for _, t := range c.AuthTokens {
		if t = strings.TrimSpace(t); t != "" {
			tokens = append(tokens, t)
		}
	}
	c.AuthTokens = tokens
}

// Validate reports invalid settings.
func (c Config) Validate() error {
	errs := []error{c.Common.Validate(false)}
	if c.PublicBaseURL != "" && !strings.HasPrefix(c.PublicBaseURL, "http://") && !strings.HasPrefix(c.PublicBaseURL, "https://") {
		errs = append(errs, errors.New("publicBaseUrl must start with http:// or https://"))
	}
	if c.NotificationRetry.Duration <= 0 {
		errs = append(errs, errors.New("notification retry interval must be positive"))
	}
	if c.NotificationMax.Duration < 0 || c.PendedAutoFinalize.Duration < 0 || c.HandshakeDelay.Duration < 0 {
		errs = append(errs, errors.New("notification and finalize durations must not be negative"))
	}
	return errors.Join(errs...)
}

// resolveRules returns cfg.Rules, the configured file, the default file on
// disk, or the bundled copy, with a description of the source.
func (c Config) resolveRules() (*Rules, string, error) {
	if c.Rules != nil {
		return c.Rules, "config", nil
	}
	if c.RulesFile != "" {
		r, err := LoadRulesFile(c.RulesFile)
		return r, c.RulesFile, err
	}
	for _, p := range []string{DefaultRulesFile, filepath.Join("..", DefaultRulesFile)} {
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			r, err := LoadRulesFile(p)
			return r, p, err
		}
	}
	r, err := EmbeddedRules()
	return r, "bundled", err
}
