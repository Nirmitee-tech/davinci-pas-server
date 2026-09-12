package pas

import (
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Nirmitee-tech/davinci-pas-server/go/internal/shared/audit"
	"github.com/Nirmitee-tech/davinci-pas-server/go/internal/shared/config"
	"github.com/Nirmitee-tech/davinci-pas-server/go/internal/shared/fhir"
)

func envMap(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestLoadConfigDefaultsAndAdminFlag(t *testing.T) {
	cfg, err := LoadConfig(envMap(nil))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ListenAddr != ":8085" || cfg.EnableAdmin || len(cfg.AuthTokens) != 0 {
		t.Errorf("defaults: listen %q admin %v tokens %v", cfg.ListenAddr, cfg.EnableAdmin, cfg.AuthTokens)
	}
	if cfg.PendedAutoFinalize.Duration != 10*time.Second || cfg.NotificationRetry.Duration != 3*time.Second || cfg.NotificationMax.Duration != 280*time.Second {
		t.Errorf("timing defaults: %v %v %v", cfg.PendedAutoFinalize, cfg.NotificationRetry, cfg.NotificationMax)
	}
	for value, want := range map[string]bool{"true": true, "TRUE": false, "1": false, "yes": false, "": false} {
		cfg, err := LoadConfig(envMap(map[string]string{"PAS_ADMIN_ENABLED": value}))
		if err != nil {
			t.Fatal(err)
		}
		if cfg.EnableAdmin != want {
			t.Errorf("PAS_ADMIN_ENABLED=%q: admin %v, want %v", value, cfg.EnableAdmin, want)
		}
	}
	cfg, err = LoadConfig(envMap(map[string]string{"PAS_ENABLE_ADMIN": "true"}))
	if err != nil || cfg.EnableAdmin {
		t.Errorf("the replaced PAS_ENABLE_ADMIN must not enable control routes")
	}
	for name, body := range map[string]string{"cfg.json": `{"enableAdmin": true}`, "cfg.yaml": "enableAdmin: true\n"} {
		path := filepath.Join(t.TempDir(), name)
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		cfg, err = LoadConfig(envMap(map[string]string{"PAS_CONFIG_FILE": path}))
		if err != nil || cfg.EnableAdmin {
			t.Errorf("%s: a config file must not enable control routes (err %v, admin %v)", name, err, cfg.EnableAdmin)
		}
	}
	cfg, err = LoadConfig(envMap(map[string]string{
		"PAS_PORT": "8185", "PAS_PREFIX": "pas/", "PAS_AUTH_TOKENS": " a, b ,",
		"PAS_PENDED_AUTO_FINALIZE_SECONDS": "0", "PAS_PUBLIC_BASE_URL": "https://example.test/",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ListenAddr != ":8185" || cfg.Prefix != "/pas" || !reflect.DeepEqual(cfg.AuthTokens, []string{"a", "b"}) ||
		cfg.PendedAutoFinalize.Duration != 0 || cfg.PublicBaseURL != "https://example.test" {
		t.Errorf("env overrides not applied: %+v", cfg)
	}
	if _, err := LoadConfig(envMap(map[string]string{"PAS_PUBLIC_BASE_URL": "ftp://x"})); err == nil {
		t.Error("a non-http public base URL must be rejected")
	}
}

func TestEmbeddedRulesMatchSpec(t *testing.T) {
	path := filepath.Join(repoRoot, DefaultRulesFile)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Skipf("spec rules not available: %v", err)
	}
	spec, err := fhir.DecodeOrdered(raw)
	if err != nil {
		t.Fatal(err)
	}
	bundled, err := fhir.DecodeOrdered(embeddedRules)
	if err != nil {
		t.Fatal(err)
	}
	if canonical(spec) != canonical(bundled) {
		t.Error("pas/adjudication-rules.json differs from spec/adjudication-rules.json; copy the spec file")
	}
	if _, err := EmbeddedRules(); err != nil {
		t.Fatal(err)
	}
}

func TestAdminRoutesDisabledAndProxied(t *testing.T) {
	cfg := testConfig()
	cfg.EnableAdmin = false
	srv, _ := newTestServer(t, cfg)
	for _, r := range []struct{ method, path string }{
		{http.MethodPost, "/admin/pended/finalize"}, {http.MethodPost, "/admin/reset"}, {http.MethodGet, "/admin/notifications"},
	} {
		status, _, body := do(t, r.method, srv.URL+r.path, nil)
		if status != http.StatusNotFound || str(body, "issue") != "" && fhir.Type(asObj(body)) != "OperationOutcome" {
			t.Errorf("disabled %s %s: status %d", r.method, r.path, status)
		}
	}
	if status, _, _ := do(t, http.MethodGet, srv.URL+"/health", nil); status != http.StatusOK {
		t.Errorf("health must stay available: %d", status)
	}

	enabled, _ := newTestServer(t, testConfig())
	status, _, _ := do(t, http.MethodGet, enabled.URL+"/admin/notifications", nil, "X-Forwarded-For", "203.0.113.9")
	if status != http.StatusForbidden {
		t.Errorf("proxied admin call: status %d, want 403", status)
	}
	status, _, _ = do(t, http.MethodPost, enabled.URL+"/admin/pended/finalize", `{"version":"v999"}`)
	if status != http.StatusBadRequest {
		t.Errorf("unknown version: status %d, want 400", status)
	}
	if status, _, _ := do(t, http.MethodGet, enabled.URL+"/admin/reset", nil); status != http.StatusMethodNotAllowed {
		t.Errorf("GET reset: status %d, want 405", status)
	}
}

func TestRoutingErrors(t *testing.T) {
	srv, _ := newTestServer(t, testConfig())
	base := srv.URL + "/v221/fhir"
	cases := []struct {
		method, url string
		body        any
		status      int
		code        string
	}{
		{http.MethodGet, srv.URL + "/v999/fhir/metadata", nil, 404, "not-found"},
		{http.MethodGet, base + "/Patient/1", nil, 404, "not-found"},
		{http.MethodGet, base + "/Claim/$submit", nil, 405, "not-supported"},
		{http.MethodPost, base + "/Claim/$submit", "not json", 400, "structure"},
		{http.MethodPost, base + "/Claim/$submit", `{"resourceType":"Bundle","type":"batch","entry":[{}]}`, 400, "invalid"},
		{http.MethodPost, base + "/Claim/$inquire", `{"resourceType":"Bundle","type":"collection","entry":[]}`, 400, "required"},
		{http.MethodPost, base + "/Subscription", `{"resourceType":"Subscription","criteria":"x","channel":{"type":"email"}}`, 422, "business-rule"},
		{http.MethodGet, base + "/Subscription/unknown", nil, 404, "not-found"},
		{http.MethodGet, base + "/ClaimResponse/unknown", nil, 404, "not-found"},
	}
	for _, tc := range cases {
		status, hdr, body := do(t, tc.method, tc.url, tc.body)
		oo := asObj(body)
		if status != tc.status || fhir.Type(oo) != "OperationOutcome" || str(list(oo["issue"])[0], "code") != tc.code {
			t.Errorf("%s %s: status %d body %s, want %d %s", tc.method, tc.url, status, canonical(body), tc.status, tc.code)
		}
		if !strings.HasPrefix(hdr.Get("Content-Type"), "application/fhir+json") {
			t.Errorf("%s %s: Content-Type %q", tc.method, tc.url, hdr.Get("Content-Type"))
		}
	}
}

func TestBearerAuth(t *testing.T) {
	cfg := testConfig()
	cfg.AuthTokens = []string{"secret-token"}
	srv, _ := newTestServer(t, cfg)
	base := srv.URL + "/v221/fhir"
	if status, _, _ := do(t, http.MethodGet, base+"/metadata", nil); status != http.StatusOK {
		t.Errorf("metadata must not require auth: %d", status)
	}
	empty := `{"resourceType":"Bundle","type":"collection","entry":[]}`
	status, hdr, body := do(t, http.MethodPost, base+"/Claim/$submit", empty)
	if status != http.StatusUnauthorized || hdr.Get("WWW-Authenticate") == "" || str(list(get(body, "issue"))[0], "code") != "security" {
		t.Errorf("missing token: status %d %q", status, hdr.Get("WWW-Authenticate"))
	}
	if status, _, _ := do(t, http.MethodPost, base+"/Claim/$submit", empty, "Authorization", "Bearer wrong"); status != http.StatusUnauthorized {
		t.Errorf("wrong token: status %d", status)
	}
	if status, _, _ := do(t, http.MethodPost, base+"/Claim/$submit", empty, "Authorization", "Bearer secret-token"); status != http.StatusBadRequest {
		t.Errorf("valid token must reach the operation (400 for the empty Bundle): status %d", status)
	}
}

// subscribe creates a Subscription pointing at l and returns its id.
func subscribe(t *testing.T, base string, l *listener, version, token string) string {
	t.Helper()
	sub := asObj(loadJSON(t, filepath.Join(repoRoot, "testdata", version, "subscription.json")))
	set(asObj(sub["channel"]), "endpoint", l.srv.URL+"/hook")
	set(asObj(sub["channel"]), "header", []any{"Authorization: Bearer " + token})
	status, _, body := do(t, http.MethodPost, base+"/Subscription", sub)
	if status != http.StatusCreated {
		t.Fatalf("subscription create: %d %s", status, canonical(body))
	}
	return fhir.ID(asObj(body))
}

func TestAutoFinalizeAndRetryUntilDelivered(t *testing.T) {
	cfg := testConfig()
	cfg.PendedAutoFinalize = config.D(50 * time.Millisecond)
	srv, _ := newTestServer(t, cfg)
	base := srv.URL + "/v221/fhir"
	l := newListener(t)
	id := subscribe(t, base, l, V221, "retry-token")
	l.waitFor(t, "handshake", 1)
	waitSubscriptionStatus(t, base, id, "active")

	// Inferno answers 500 while no test waits; the server must retry and stop after the first 2xx.
	l.mu.Lock()
	l.statuses = []int{500, 500}
	l.mu.Unlock()
	req := loadJSON(t, filepath.Join(repoRoot, "testdata", V221, "pended_submit.json"))
	if status, _, _ := do(t, http.MethodPost, base+"/Claim/$submit", req); status != http.StatusOK {
		t.Fatalf("pended submit: %d", status)
	}
	events := l.waitFor(t, "event-notification", 3)
	time.Sleep(150 * time.Millisecond)
	if n := len(l.all("event-notification")); n != 3 {
		t.Errorf("event notification attempts %d, want 3 (two 500 answers, then one 200)", n)
	}
	if n := len(l.all("handshake")); n != 1 {
		t.Errorf("handshake must not be resent after delivery: %d", n)
	}
	checkNotification(t, events[2], "event-notification", id, "retry-token")
	if canonical(events[0].body) != canonical(events[2].body) {
		t.Error("retries must resend the same notification")
	}
}

func TestSubscriptionUpdateDeleteAndStatus(t *testing.T) {
	srv, _ := newTestServer(t, testConfig())
	base := srv.URL + "/v201/fhir"
	l := newListener(t)
	id := subscribe(t, base, l, V201, "crud-token")
	l.waitFor(t, "handshake", 1)
	waitSubscriptionStatus(t, base, id, "active")

	status, _, body := do(t, http.MethodGet, base+"/Subscription/"+id+"/$status", nil)
	if status != http.StatusOK || parameterValue(asObj(body), "type", "valueCode") != "query-status" {
		t.Errorf("$status: %d %s", status, canonical(body))
	}
	_, _, current := do(t, http.MethodGet, base+"/Subscription/"+id, nil)
	updated := asObj(current)
	set(updated, "reason", "updated reason")
	status, _, body = do(t, http.MethodPut, base+"/Subscription/"+id, updated)
	if status != http.StatusOK || str(body, "reason") != "updated reason" || str(body, "status") != "active" {
		t.Errorf("PUT: %d %s", status, canonical(body))
	}
	set(updated, "id", "other")
	if status, _, _ := do(t, http.MethodPut, base+"/Subscription/"+id, updated); status != http.StatusBadRequest {
		t.Errorf("PUT with mismatched id: %d", status)
	}
	if status, _, _ := do(t, http.MethodDelete, base+"/Subscription/"+id, nil); status != http.StatusNoContent {
		t.Errorf("DELETE: %d", status)
	}
	if status, _, _ := do(t, http.MethodGet, base+"/Subscription/"+id, nil); status != http.StatusNotFound {
		t.Errorf("GET after DELETE: %d", status)
	}
}

func TestPrefixAndPublicBaseURL(t *testing.T) {
	cfg := testConfig()
	cfg.Prefix = "/pas"
	cfg.PublicBaseURL = "https://public.example.test"
	srv, _ := newTestServer(t, cfg)
	if status, _, _ := do(t, http.MethodGet, srv.URL+"/v221/fhir/metadata", nil); status != http.StatusNotFound {
		t.Errorf("unprefixed route: %d, want 404", status)
	}
	status, _, body := do(t, http.MethodGet, srv.URL+"/pas/v221/fhir/metadata", nil)
	if status != http.StatusOK || str(body, "implementation", "url") != "https://public.example.test/pas/v221/fhir" {
		t.Errorf("prefixed metadata: %d %q", status, str(body, "implementation", "url"))
	}
	l := newListener(t)
	id := subscribe(t, srv.URL+"/pas/v221/fhir", l, V221, "prefix-token")
	hs := l.waitFor(t, "handshake", 1)[0]
	ref := parameterValue(asObj(get(list(hs.body["entry"])[0], "resource")), "subscription", "valueReference")
	if ref != "https://public.example.test/pas/v221/fhir/Subscription/"+id {
		t.Errorf("handshake subscription reference %q", ref)
	}
}

func TestAuditRedactsBearerTokens(t *testing.T) {
	dir := t.TempDir()
	logger, err := audit.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	cfg := testConfig()
	cfg.Audit = logger
	srv, h := newTestServer(t, cfg)
	l := newListener(t)
	subscribe(t, srv.URL+"/v221/fhir", l, V221, "very-secret-notification-token")
	l.waitFor(t, "handshake", 1)
	_ = h.Close()
	_ = logger.Close()
	files, _ := filepath.Glob(filepath.Join(dir, "*.jsonl"))
	if len(files) == 0 {
		t.Fatal("no audit file written")
	}
	raw, _ := os.ReadFile(files[0])
	if strings.Contains(string(raw), "very-secret-notification-token") {
		t.Error("audit log contains the bearer token")
	}
	if !strings.Contains(string(raw), "[redacted]") || !strings.Contains(string(raw), `"operation":"notification"`) {
		t.Error("audit log should record the redacted request and the notification attempt")
	}
}
