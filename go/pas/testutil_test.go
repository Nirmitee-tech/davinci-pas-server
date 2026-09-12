package pas

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Nirmitee-tech/davinci-pas-server/go/internal/shared/config"
	"github.com/Nirmitee-tech/davinci-pas-server/go/internal/shared/fhir"
)

// repoRoot is the repository root relative to this package directory.
const repoRoot = "../.."

type captured struct {
	header http.Header
	body   object
}

// listener imitates a rest-hook endpoint. It answers with the next queued
// status (default 200) and records every request.
type listener struct {
	mu       sync.Mutex
	got      []captured
	statuses []int
	srv      *httptest.Server
}

func newListener(t *testing.T) *listener {
	t.Helper()
	l := &listener{}
	l.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		v, _ := fhir.DecodeOrdered(raw)
		l.mu.Lock()
		l.got = append(l.got, captured{header: r.Header.Clone(), body: asObj(v)})
		status := http.StatusOK
		if len(l.statuses) > 0 {
			status, l.statuses = l.statuses[0], l.statuses[1:]
		}
		l.mu.Unlock()
		w.WriteHeader(status)
	}))
	t.Cleanup(l.srv.Close)
	return l
}

func (l *listener) all(kind string) []captured {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []captured
	for _, c := range l.got {
		if kind == "" || notificationType(c.body) == kind {
			out = append(out, c)
		}
	}
	return out
}

func (l *listener) waitFor(t *testing.T, kind string, n int) []captured {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if got := l.all(kind); len(got) >= n {
			return got
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("expected %d %s notification(s), got %d", n, kind, len(l.all(kind)))
	return nil
}

func notificationType(b object) string {
	entries := list(b["entry"])
	if len(entries) == 0 {
		return ""
	}
	return parameterValue(asObj(get(entries[0], "resource")), "type", "valueCode")
}

func parameterValue(params object, name, key string) string {
	for _, p := range list(params["parameter"]) {
		if str(p, "name") == name {
			if key == "valueReference" {
				return str(p, "valueReference", "reference")
			}
			return str(p, key)
		}
	}
	return ""
}

func testConfig() Config {
	cfg := DefaultConfig()
	cfg.PendedAutoFinalize = config.D(0)
	cfg.HandshakeDelay = config.D(0)
	cfg.NotificationRetry = config.D(20 * time.Millisecond)
	cfg.NotificationMax = config.D(3 * time.Second)
	cfg.EnableAdmin = true
	cfg.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	if p := filepath.Join(repoRoot, DefaultRulesFile); fileExists(p) {
		cfg.RulesFile = p
	}
	return cfg
}

func fileExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}

func newTestServer(t *testing.T, cfg Config) (*httptest.Server, *Handler) {
	t.Helper()
	handler, err := NewHandler(cfg)
	if err != nil {
		t.Fatal(err)
	}
	h := handler.(*Handler)
	srv := httptest.NewServer(h)
	t.Cleanup(func() {
		srv.Close()
		_ = h.Close()
	})
	return srv, h
}

func loadJSON(t *testing.T, path string) any {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Skipf("test data not available: %v", err)
	}
	v, err := fhir.DecodeOrdered(raw)
	if err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	return v
}

// do sends a request and decodes the JSON response (nil for an empty body).
func do(t *testing.T, method, url string, body any, headers ...string) (int, http.Header, any) {
	t.Helper()
	var rdr io.Reader
	switch b := body.(type) {
	case nil:
	case []byte:
		rdr = bytes.NewReader(b)
	case string:
		rdr = strings.NewReader(b)
	default:
		raw, err := fhir.MarshalOrdered(b)
		if err != nil {
			raw, err = json.Marshal(b)
		}
		if err != nil {
			t.Fatal(err)
		}
		rdr = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, url, rdr)
	if err != nil {
		t.Fatal(err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/fhir+json")
	}
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var v any
	if len(bytes.TrimSpace(raw)) > 0 {
		if v, err = fhir.DecodeOrdered(raw); err != nil {
			t.Fatalf("%s %s: response is not JSON: %s", method, url, raw)
		}
	}
	return resp.StatusCode, resp.Header, v
}

// responseBundles returns the Bundles of a $submit or $inquire response body.
func responseBundles(body object) []object {
	if fhir.Type(body) == "Bundle" {
		return []object{body}
	}
	var out []object
	for _, p := range list(body["parameter"]) {
		if str(p, "name") == "return" {
			if r := asObj(get(p, "resource")); fhir.Type(r) == "Bundle" {
				out = append(out, r)
			}
		}
	}
	return out
}

// itemDecisions maps ClaimResponse.item.itemSequence to the review action code.
func itemDecisions(bundle object) map[string]string {
	out := map[string]string{}
	cr := firstClaimResponse(bundle)
	for _, it := range list(cr["item"]) {
		for _, adj := range list(get(it, "adjudication")) {
			for _, x := range list(get(adj, "extension")) {
				for _, s := range list(get(x, "extension")) {
					if str(s, "url") == pasSD+"extension-reviewActionCode" {
						out[seqText(get(it, "itemSequence"))] = firstCode(get(s, "valueCodeableConcept"))
					}
				}
			}
		}
	}
	return out
}

func expectItems(t *testing.T, label string, bundle object, want any) {
	t.Helper()
	got := itemDecisions(bundle)
	w := asObj(want)
	if len(got) != len(orderedKeys(w)) {
		t.Errorf("%s: item decisions %v, want %v", label, got, w)
		return
	}
	for _, k := range orderedKeys(w) {
		if got[k] != str(w, k) {
			t.Errorf("%s: item %s decision %q, want %q (all %v)", label, k, got[k], str(w, k), got)
		}
	}
}

// canonical renders JSON without the key-order marker, with sorted keys.
func canonical(v any) string {
	var strip func(any) any
	strip = func(v any) any {
		switch t := v.(type) {
		case map[string]any:
			m := map[string]any{}
			for k, x := range t {
				if k != fhir.OrderKey {
					m[k] = strip(x)
				}
			}
			return m
		case []any:
			out := make([]any, len(t))
			for i, x := range t {
				out[i] = strip(x)
			}
			return out
		}
		return v
	}
	b, _ := json.Marshal(strip(v))
	return string(b)
}

var absoluteRef = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9+.-]*:`)

// checkResponse mirrors the kit's response structure rules (SPEC 4.3) and the
// validator context rules confirmed live (SPEC 4.4, 4.5).
func checkResponse(t *testing.T, label string, bundle, request object, op string) {
	t.Helper()
	entries := list(bundle["entry"])
	if len(entries) == 0 {
		t.Fatalf("%s: response Bundle has no entries", label)
	}
	if fhir.Type(bundle) != "Bundle" || str(bundle, "type") != "collection" || str(bundle, "identifier", "system") != "urn:ietf:rfc:3986" {
		t.Errorf("%s: response must be a collection Bundle with a urn:ietf:rfc:3986 identifier", label)
	}
	cr := asObj(get(entries[0], "resource"))
	if fhir.Type(cr) != "ClaimResponse" {
		t.Fatalf("%s: first entry is %s, want ClaimResponse", label, fhir.Type(cr))
	}
	counts := map[string]int{}
	byURL := map[string]object{}
	for _, en := range entries {
		counts[str(en, "fullUrl")]++
		byURL[str(en, "fullUrl")] = asObj(get(en, "resource"))
		if fhir.Type(asObj(get(en, "resource"))) == "Claim" {
			t.Errorf("%s: the response must not echo a Claim", label)
		}
	}
	base := extractBaseURL(str(entries[len(entries)-1], "fullUrl"))
	visited := map[string]bool{}
	var walk func(v any, depth int)
	walk = func(v any, depth int) {
		if depth > 60 {
			t.Errorf("%s: reference traversal too deep", label)
			return
		}
		switch x := v.(type) {
		case []any:
			for _, y := range x {
				walk(y, depth+1)
			}
		case map[string]any:
			if ref, ok := x["reference"].(string); ok && ref != "" {
				abs := ref
				if base != "" && !strings.HasPrefix(ref, "urn:uuid:") && !absoluteRef.MatchString(ref) {
					abs = base + "/" + ref
				}
				if counts[abs] != 1 {
					t.Errorf("%s: referenced %s appears %d times in the Bundle, want exactly once", label, abs, counts[abs])
				}
				if !visited[abs] && byURL[abs] != nil {
					visited[abs] = true
					walk(byURL[abs], depth+1)
				}
				return
			}
			typ := fhir.Type(x)
			for _, k := range orderedKeys(x) {
				if strings.HasPrefix(k, "_") || (k == "request" && typ == "ClaimResponse") || (k == "related" && typ == "Claim") {
					continue
				}
				walk(x[k], depth+1)
			}
		}
	}
	walk(cr, 0)

	if op == opSubmit && request != nil {
		for _, re := range entries {
			rr := asObj(get(re, "resource"))
			for _, q := range list(request["entry"]) {
				qr := asObj(get(q, "resource"))
				if fhir.Type(qr) != fhir.Type(rr) {
					continue
				}
				sameURL := str(q, "fullUrl") != "" && str(q, "fullUrl") == str(re, "fullUrl")
				sameID := fhir.ID(qr) != "" && fhir.ID(qr) == fhir.ID(rr)
				sameIdent := present(qr["identifier"]) && canonical(qr["identifier"]) == canonical(rr["identifier"])
				if !(sameURL || sameID || sameIdent) {
					continue
				}
				if !(str(q, "fullUrl") == str(re, "fullUrl") && fhir.ID(qr) == fhir.ID(rr) && canonical(qr["identifier"]) == canonical(rr["identifier"])) {
					t.Errorf("%s: echoed %s/%s changed its fullUrl, id or identifier", label, fhir.Type(rr), fhir.ID(rr))
				}
				if canonical(qr) != canonical(rr) {
					t.Errorf("%s: echoed %s/%s differs from the request", label, fhir.Type(rr), fhir.ID(rr))
				}
				break
			}
		}
	}

	req := asObj(get(cr, "request"))
	hasDAR := false
	for _, x := range list(req["extension"]) {
		if str(x, "url") == darURL && str(x, "valueCode") != "" {
			hasDAR = true
		}
	}
	if str(req, "reference") == "" && !hasDAR {
		t.Errorf("%s: ClaimResponse.request needs a reference or a data-absent-reason extension (spec-44)", label)
	}
	for _, adj := range list(cr["adjudication"]) {
		for _, x := range list(get(adj, "extension")) {
			for _, s := range list(get(x, "extension")) {
				if str(s, "url") == pasSD+"extension-reviewActionCode" {
					t.Errorf("%s: header adjudication must not carry extension-reviewActionCode", label)
				}
			}
		}
	}
	for _, key := range []string{"item", "addItem"} {
		for _, it := range list(cr[key]) {
			for _, adj := range list(get(it, "adjudication")) {
				for _, x := range list(get(adj, "extension")) {
					for _, s := range list(get(x, "extension")) {
						if str(s, "url") != "reasonCode" {
							continue
						}
						for _, c := range fhir.Codings(get(s, "valueCodeableConcept")) {
							if str(c, "system") != "https://codesystem.x12.org/external/886" {
								t.Errorf("%s: reviewAction reasonCode system %q, want https://codesystem.x12.org/external/886", label, str(c, "system"))
							}
						}
					}
				}
			}
			for _, x := range list(get(it, "extension")) {
				if str(x, "url") != pasSD+"extension-itemAuthorizedDetail" {
					continue
				}
				for _, s := range list(get(x, "extension")) {
					if q := asObj(get(s, "valueQuantity")); q != nil && str(q, "unit") == "" && str(q, "code") == "" {
						t.Errorf("%s: itemAuthorizedDetail quantity needs unit or code", label)
					}
				}
			}
		}
	}
}

// checkNotification mirrors the kit's notification rules (SPEC 7.2, 7.4).
func checkNotification(t *testing.T, c captured, kind, subscriptionID, token string) {
	t.Helper()
	b := c.body
	if str(b, "type") != "history" {
		t.Errorf("%s: Bundle.type %q, want history", kind, str(b, "type"))
	}
	entries := list(b["entry"])
	if len(entries) == 0 {
		t.Fatalf("%s: empty notification", kind)
	}
	for i, en := range entries {
		if get(en, "request") == nil || get(en, "response") == nil {
			t.Errorf("%s: entry %d lacks request or response", kind, i+1)
		}
	}
	if !strings.HasSuffix(str(entries[0], "request", "url"), "Subscription/"+subscriptionID+"/$status") {
		t.Errorf("%s: status request.url %q", kind, str(entries[0], "request", "url"))
	}
	params := asObj(get(entries[0], "resource"))
	if fhir.Type(params) != "Parameters" {
		t.Fatalf("%s: first entry is not Parameters", kind)
	}
	wantStatus := "active"
	if kind == "handshake" {
		wantStatus = "requested"
	}
	if got := parameterValue(params, "type", "valueCode"); got != kind {
		t.Errorf("type parameter %q, want %q", got, kind)
	}
	if got := parameterValue(params, "status", "valueCode"); got != wantStatus {
		t.Errorf("%s: status parameter %q, want %q", kind, got, wantStatus)
	}
	ref := parameterValue(params, "subscription", "valueReference")
	if parts := strings.Split(ref, "/"); parts[len(parts)-1] != subscriptionID {
		t.Errorf("%s: subscription reference %q does not end with the id", kind, ref)
	}
	if got := c.header.Get("Authorization"); got != "Bearer "+token {
		t.Errorf("%s: Authorization header %q", kind, got)
	}
	if !strings.HasPrefix(c.header.Get("Content-Type"), "application/fhir+json") {
		t.Errorf("%s: Content-Type %q", kind, c.header.Get("Content-Type"))
	}
}
