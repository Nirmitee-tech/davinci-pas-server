package pas

import (
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Nirmitee-tech/davinci-pas-server/go/internal/shared/fhir"
)

// TestReplayTestdata replays testdata/<version> in Inferno suite order
// (SPEC 2 and 14) and checks decisions, structure and notifications.
func TestReplayTestdata(t *testing.T) {
	for _, version := range Versions {
		t.Run(version, func(t *testing.T) { replayVersion(t, version) })
	}
}

func replayVersion(t *testing.T, version string) {
	td := filepath.Join(repoRoot, "testdata", version)
	expected := asObj(loadJSON(t, filepath.Join(td, "expected.json")))
	l := newListener(t)
	srv, _ := newTestServer(t, testConfig())
	base := srv.URL + "/" + version + "/fhir"

	// 1. CapabilityStatement.
	status, _, body := do(t, http.MethodGet, base+"/metadata", nil)
	cs := asObj(body)
	if status != http.StatusOK || fhir.Type(cs) != "CapabilityStatement" {
		t.Fatalf("metadata: status %d %v", status, fhir.Type(cs))
	}
	interactions := map[string]bool{}
	for _, rest := range list(cs["rest"]) {
		for _, res := range list(get(rest, "resource")) {
			if str(res, "type") == "Subscription" {
				for _, i := range list(get(res, "interaction")) {
					interactions[str(i, "code")] = true
				}
			}
		}
	}
	for _, code := range []string{"create", "update", "delete"} {
		if !interactions[code] {
			t.Errorf("metadata: Subscription %s interaction missing", code)
		}
	}

	// 2. Subscription Setup: Inferno sets the endpoint and appends the Authorization header.
	sub := asObj(loadJSON(t, filepath.Join(td, "subscription.json")))
	token := "notify-" + version
	channel := asObj(sub["channel"])
	set(channel, "endpoint", l.srv.URL+"/notification_listener")
	set(channel, "header", []any{"Authorization: Bearer " + token})
	status, hdr, body := do(t, http.MethodPost, base+"/Subscription", sub)
	created := asObj(body)
	subID := fhir.ID(created)
	if status != http.StatusCreated || subID == "" || str(created, "status") != "requested" || str(created, "channel", "type") != "rest-hook" {
		t.Fatalf("subscription create: status %d, body %v", status, canonical(created))
	}
	if hdr.Get("Location") != base+"/Subscription/"+subID {
		t.Errorf("Location %q", hdr.Get("Location"))
	}
	if canonical(created["channel"]) != canonical(sub["channel"]) || canonical(created["_criteria"]) != canonical(sub["_criteria"]) {
		t.Errorf("subscription create: channel and criteria must be returned as sent")
	}
	handshakes := l.waitFor(t, "handshake", 1)
	checkNotification(t, handshakes[0], "handshake", subID, token)
	waitSubscriptionStatus(t, base, subID, "active")

	// 3. Workflows.
	for _, name := range []string{"approval_submit", "denial_submit", "pended_submit"} {
		req := asObj(loadJSON(t, filepath.Join(td, name+".json")))
		status, _, body := do(t, http.MethodPost, base+"/Claim/$submit", req)
		if status != http.StatusOK {
			t.Fatalf("%s: status %d %v", name, status, canonical(body))
		}
		checkResponse(t, name, asObj(body), req, opSubmit)
		expectItems(t, name, asObj(body), get(expected[name], "items"))
	}
	status, _, body = do(t, http.MethodPost, srv.URL+"/admin/pended/finalize", map[string]any{"version": version})
	if status != http.StatusOK || len(list(get(body, "finalized"))) != 1 || seqText(get(body, "notifications")) != "1" {
		t.Fatalf("finalize: status %d %v", status, canonical(body))
	}
	events := l.waitFor(t, "event-notification", 1)
	event := events[0]
	checkNotification(t, event, "event-notification", subID, token)
	second := asObj(list(event.body["entry"])[1])
	if version == V221 {
		focus := asObj(second["resource"])
		if parameterValue(asObj(get(list(event.body["entry"])[0], "resource")), "status", "valueCode") != "active" {
			t.Error("event status must be active")
		}
		if fhir.Type(focus) != "Bundle" || str(second, "fullUrl") != "urn:uuid:"+fhir.ID(focus) {
			t.Fatalf("full-resource: second entry must be the response Bundle with fullUrl urn:uuid:<id>")
		}
		expectItems(t, "notification", focus, get(expected["pended_submit"], "after_finalize"))
		checkNotificationCopy(t, focus)
		checkResponse(t, "notification focus", focus, nil, opSubmit)
	} else {
		if second["resource"] != nil || !strings.Contains(str(second, "fullUrl"), "/ClaimResponse/") || str(second, "request", "method") != "PUT" {
			t.Fatalf("id-only: unexpected second entry %v", canonical(second))
		}
		status, _, body := do(t, http.MethodGet, str(second, "fullUrl"), nil)
		if status != http.StatusOK || fhir.Type(asObj(body)) != "ClaimResponse" {
			t.Fatalf("id-only focus read: status %d", status)
		}
		req := asObj(loadJSON(t, filepath.Join(td, "pended_inquire.json")))
		status, _, body = do(t, http.MethodPost, base+"/Claim/$inquire", req)
		if status != http.StatusOK || fhir.Type(asObj(body)) != "Bundle" {
			t.Fatalf("pended inquire: status %d type %s", status, fhir.Type(asObj(body)))
		}
		checkResponse(t, "pended_inquire", asObj(body), req, opInquire)
		expectItems(t, "pended_inquire", asObj(body), get(expected["pended_inquire"], "items"))
	}
	status, _, body = do(t, http.MethodPost, srv.URL+"/admin/pended/finalize", map[string]any{"version": version})
	if status != http.StatusOK || len(list(get(body, "finalized"))) != 0 {
		t.Errorf("second finalize must be a no-op: %v", canonical(body))
	}
	time.Sleep(100 * time.Millisecond)
	if n := len(l.all("")); n != 2 {
		t.Errorf("expected exactly one handshake and one event notification, got %d requests", n)
	}

	// 4. Claim updates (v2.2.1).
	if version == V221 {
		for _, step := range []string{"claim_update_initial_submit", "claim_update_add_item_submit", "claim_update_modify_cancel_submit", "claim_update_cancel_all_submit"} {
			req := asObj(loadJSON(t, filepath.Join(td, step+".json")))
			status, _, body := do(t, http.MethodPost, base+"/Claim/$submit", req)
			if status != http.StatusOK {
				t.Fatalf("%s: status %d", step, status)
			}
			resp := asObj(body)
			checkResponse(t, step, resp, req, opSubmit)
			expectItems(t, step, resp, get(expected[step], "items"))
			var submitted, returned []string
			for _, it := range list(asObj(get(list(req["entry"])[0], "resource"))["item"]) {
				submitted = append(submitted, seqText(get(it, "sequence")))
			}
			for _, it := range list(firstClaimResponse(resp)["item"]) {
				returned = append(returned, seqText(get(it, "itemSequence")))
			}
			if strings.Join(submitted, ",") != strings.Join(returned, ",") {
				t.Errorf("%s: returned item sequences %v, submitted %v", step, returned, submitted)
			}
		}
	}

	// 5. Must support.
	for i, req := range list(loadJSON(t, filepath.Join(td, "must_support_submit.json"))) {
		label := "must_support_submit[" + seqText(i) + "]"
		status, _, body := do(t, http.MethodPost, base+"/Claim/$submit", req)
		if status != http.StatusOK {
			t.Fatalf("%s: status %d", label, status)
		}
		resp := asObj(body)
		checkResponse(t, label, resp, asObj(req), opSubmit)
		want := asObj(list(expected["must_support_submit"])[i])
		expectItems(t, label, resp, want["items"])
		cr := firstClaimResponse(resp)
		if want["addItem"] == true && cr["addItem"] == nil {
			t.Errorf("%s: addItem expected", label)
		}
		if want["communicationRequest"] == true && cr["communicationRequest"] == nil {
			t.Errorf("%s: communicationRequest expected", label)
		}
		if want["error"] == true && (cr["error"] == nil || str(cr, "outcome") != "partial") {
			t.Errorf("%s: error with outcome partial expected", label)
		}
		if want["task"] == true && !hasResource(resp, "Task") {
			t.Errorf("%s: Task expected", label)
		}
	}
	replaced := false
	for i, req := range list(loadJSON(t, filepath.Join(td, "must_support_inquire.json"))) {
		label := "must_support_inquire[" + seqText(i) + "]"
		status, _, body := do(t, http.MethodPost, base+"/Claim/$inquire", req)
		if status != http.StatusOK {
			t.Fatalf("%s: status %d", label, status)
		}
		wantType := "Bundle"
		if version == V221 {
			wantType = "Parameters"
		}
		if fhir.Type(asObj(body)) != wantType {
			t.Fatalf("%s: response type %s, want %s", label, fhir.Type(asObj(body)), wantType)
		}
		bundles := responseBundles(asObj(body))
		if len(bundles) != 1 {
			t.Fatalf("%s: expected one response Bundle", label)
		}
		checkResponse(t, label, bundles[0], asObj(req), opInquire)
		want := asObj(list(expected["must_support_inquire"])[i])
		expectItems(t, label, bundles[0], want["items"])
		for from, to := range asObj(want["replacedAuthorization"]) {
			if from == fhir.OrderKey {
				continue
			}
			if strings.Contains(canonical(bundles[0]), `"valueString":"`+str(asObj(want["replacedAuthorization"]), from)+`"`) {
				replaced = true
			}
			_ = to
		}
		if want["task"] == true && !hasResource(bundles[0], "Task") {
			t.Errorf("%s: Task expected", label)
		}
	}
	if !replaced {
		t.Errorf("no inquiry response carried a replaced authorization number (spec-47)")
	}

	// 6. Error handling: the kit's empty Bundle.
	empty := `{"resourceType":"Bundle","id":"ReferralAuthorizationBundleExample","type":"collection","timestamp":"2005-05-02T11:01:00+05:00","entry":[]}`
	for _, op := range []string{"$submit", "$inquire"} {
		status, _, body := do(t, http.MethodPost, base+"/Claim/"+op, empty)
		if status != http.StatusBadRequest || fhir.Type(asObj(body)) != "OperationOutcome" {
			t.Errorf("%s with an empty Bundle: status %d %s", op, status, fhir.Type(asObj(body)))
		}
	}

	// Delivery log used by the harness.
	status, _, body = do(t, http.MethodGet, srv.URL+"/admin/notifications", nil)
	found := false
	for _, n := range list(get(body, "notifications")) {
		if str(n, "kind") == "handshake" && get(n, "delivered") == true && str(n, "endpoint_token") == token && str(n, "subscription_id") == subID {
			found = true
		}
	}
	if status != http.StatusOK || !found {
		t.Errorf("admin notifications: status %d, delivered handshake for the token not listed: %v", status, canonical(body))
	}
}

func waitSubscriptionStatus(t *testing.T, base, id, want string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	got := ""
	for time.Now().Before(deadline) {
		_, _, body := do(t, http.MethodGet, base+"/Subscription/"+id, nil)
		if got = str(body, "status"); got == want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("Subscription/%s status %q, want %q", id, got, want)
}

func hasResource(bundle object, typ string) bool {
	for _, en := range list(bundle["entry"]) {
		if fhir.Type(asObj(get(en, "resource"))) == typ {
			return true
		}
	}
	return false
}

// checkNotificationCopy verifies the SPEC 7.4 notification copy rule.
func checkNotificationCopy(t *testing.T, focus object) {
	t.Helper()
	cr := firstClaimResponse(focus)
	check := func(where string, exts any, urls []string) {
		for _, x := range list(exts) {
			for _, u := range urls {
				if str(x, "url") == u {
					t.Errorf("notification ClaimResponse %s must omit %s", where, u)
				}
			}
		}
	}
	check("root", cr["extension"], notificationOmit["root"])
	for _, key := range []string{"item", "addItem"} {
		for _, it := range list(cr[key]) {
			check(key, get(it, "extension"), notificationOmit[key])
		}
	}
}
