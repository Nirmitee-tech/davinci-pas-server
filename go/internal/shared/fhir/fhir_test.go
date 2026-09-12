package fhir

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestOrderedRoundTripKeepsKeyOrderAndNumbers(t *testing.T) {
	in := `{"resourceType":"Appointment","id":"a1","participant":[{"type":[{"coding":[{"system":"s","code":"PPRF"}]}],"actor":{"reference":"Location/x"}},{"actor":{"reference":"Patient/p"},"status":"accepted"}],"priority":5,"q":{"value":1.50,"unit":"<mg>&"}}`
	v, err := DecodeOrdered([]byte(in))
	if err != nil {
		t.Fatal(err)
	}
	out, err := MarshalOrdered(v)
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != in {
		t.Fatalf("round trip changed the document:\n got %s\nwant %s", out, in)
	}
}

func TestOrderedAppendsNewKeysAfterOriginalOrder(t *testing.T) {
	v, _ := DecodeOrdered([]byte(`{"resourceType":"ServiceRequest","status":"draft","id":"s1"}`))
	m := CloneResource(v.(map[string]any))
	m["extension"] = []any{map[string]any{"url": "u"}}
	out, _ := MarshalOrdered(m)
	want := `{"resourceType":"ServiceRequest","status":"draft","id":"s1","extension":[{"url":"u"}]}`
	if string(out) != want {
		t.Fatalf("got %s want %s", out, want)
	}
}

func TestDecodeOrderedRejectsTrailingData(t *testing.T) {
	if _, err := DecodeOrdered([]byte(`{} {}`)); err == nil {
		t.Fatal("expected error")
	}
}

func TestParseReference(t *testing.T) {
	cases := map[string][2]string{
		"ServiceRequest/abc":                                      {"ServiceRequest", "abc"},
		"http://localhost/fhir/DeviceRequest/d-1":                 {"DeviceRequest", "d-1"},
		"https://x.test/fhir/Patient/p1/_history/3":               {"Patient", "p1"},
		"Organization/org1?foo=bar":                               {"Organization", "org1"},
		"http://localhost/custom/crd_server_v221/fhir/Coverage/c": {"Coverage", "c"},
	}
	for in, want := range cases {
		typ, id, ok := ParseReference(in)
		if !ok || typ != want[0] || id != want[1] {
			t.Errorf("ParseReference(%q) = %q %q %v", in, typ, id, ok)
		}
	}
	for _, bad := range []string{"", "#contained", "urn:uuid:1", "patient/1", "justid"} {
		if _, _, ok := ParseReference(bad); ok {
			t.Errorf("ParseReference(%q) should fail", bad)
		}
	}
}

func TestClientGet(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/fhir/Patient/p1":
			if r.Header.Get("Authorization") != "Bearer tok" || r.Header.Get("Accept") != "application/fhir+json" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			_, _ = w.Write([]byte(`{"resourceType":"Patient","id":"p1"}`))
		case r.URL.Path == "/fhir/Coverage":
			if r.URL.Query().Get("patient") != "p1" {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			_, _ = w.Write([]byte(`{"resourceType":"Bundle","entry":[{"resource":{"resourceType":"Coverage","id":"c1"}}]}`))
		case r.URL.Path == "/fhir/Slow/1":
			time.Sleep(200 * time.Millisecond)
		case r.URL.Path == "/fhir/Broken/1":
			_, _ = w.Write([]byte(`not json`))
		default:
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte("Unable to find test run"))
		}
	}))
	defer srv.Close()

	c := &Client{HTTP: srv.Client(), Timeout: 50 * time.Millisecond}
	ep := Endpoint{BaseURL: srv.URL + "/fhir/", AccessToken: "tok", TokenType: "bearer"}
	ctx := context.Background()

	if r := c.Get(ctx, ep, "Patient/p1", nil); r.Status != 200 || ID(r.Resource) != "p1" || r.Err != nil {
		t.Fatalf("read: %+v", r)
	}
	if r := c.Get(ctx, ep, "Coverage", url.Values{"patient": {"p1"}}); r.Status != 200 || len(BundleResources(r.Resource)) != 1 {
		t.Fatalf("search: %+v", r)
	}
	if r := c.Get(ctx, ep, "Other/1", nil); r.Status != 500 || r.Resource != nil {
		t.Fatalf("500: %+v", r)
	}
	if r := c.Get(ctx, ep, "Slow/1", nil); r.Status != 0 || r.Err == nil || !strings.Contains(r.Err.Error(), "deadline") {
		t.Fatalf("timeout: %+v", r)
	}
	if r := c.Get(ctx, ep, "Broken/1", nil); r.Status != 200 || r.Err == nil {
		t.Fatalf("unparseable: %+v", r)
	}
}
