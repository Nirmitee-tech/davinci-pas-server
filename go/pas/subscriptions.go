package pas

import (
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/Nirmitee-tech/davinci-pas-server/go/internal/shared/fhir"
)

const (
	contentFullResource = "full-resource"
	contentIDOnly       = "id-only"
)

// validateSubscription checks a posted Subscription (SPEC 7.1). It returns the
// Subscription and its payload content, or a status and OperationOutcome.
func validateSubscription(body any) (object, string, int, object) {
	sub, ok := body.(map[string]any)
	if !ok || fhir.Type(sub) != "Subscription" {
		return nil, "", http.StatusBadRequest, outcome("error", "structure", "The request body must be a Subscription.", "Subscription")
	}
	content := payloadContent(sub)
	var problems []string
	if str(sub, "criteria") != pasTopic {
		problems = append(problems, "Subscription.criteria must be the PAS SubscriptionTopic canonical "+pasTopic)
	}
	if str(sub, "channel", "type") != "rest-hook" {
		problems = append(problems, "Subscription.channel.type must be rest-hook")
	}
	if endpoint := str(sub, "channel", "endpoint"); endpoint == "" {
		problems = append(problems, "Subscription.channel.endpoint is required")
	} else if u, err := url.Parse(endpoint); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		problems = append(problems, "Subscription.channel.endpoint must be an absolute http or https URL")
	}
	if content != contentFullResource && content != contentIDOnly {
		problems = append(problems, "the backport-payload-content extension on Subscription.channel.payload must be full-resource or id-only")
	}
	if len(problems) > 0 {
		return nil, "", http.StatusUnprocessableEntity, outcome("error", "business-rule", strings.Join(problems, "; "), "")
	}
	return sub, content, 0, nil
}

// payloadContent returns the backport payload content code of a Subscription.
func payloadContent(sub object) string {
	for _, x := range list(get(sub, "channel", "_payload", "extension")) {
		if str(x, "url") == backportSD+"backport-payload-content" {
			return str(x, "valueCode")
		}
	}
	return ""
}

// channelHeaders returns the HTTP headers for notifications: Content-Type from
// channel.payload, then every channel.header entry split on the first ": ".
func channelHeaders(sub object) [][2]string {
	contentType := str(sub, "channel", "payload")
	if contentType == "" {
		contentType = "application/fhir+json"
	}
	headers := [][2]string{{"Content-Type", contentType}}
	for _, h := range list(get(sub, "channel", "header")) {
		s, _ := h.(string)
		name, value, found := strings.Cut(s, ": ")
		if !found {
			name, value, found = strings.Cut(s, ":")
			value = strings.TrimSpace(value)
		}
		name = strings.TrimSpace(name)
		if !found || name == "" {
			continue
		}
		replaced := false
		for i := range headers {
			if strings.EqualFold(headers[i][0], name) {
				headers[i] = [2]string{name, value}
				replaced = true
			}
		}
		if !replaced {
			headers = append(headers, [2]string{name, value})
		}
	}
	return headers
}

// bearerToken returns the token of an Authorization: Bearer header, if any.
func bearerToken(headers [][2]string) (string, bool) {
	for _, h := range headers {
		if strings.EqualFold(h[0], "Authorization") {
			if scheme, token, ok := strings.Cut(h[1], " "); ok && strings.EqualFold(scheme, "Bearer") {
				return strings.TrimSpace(token), true
			}
		}
	}
	return "", false
}

// setResourceID sets id and places it right after resourceType.
func setResourceID(res object, id string) {
	keys := orderedKeys(res)
	res["id"] = id
	order := make([]string, 0, len(keys)+1)
	placed := false
	for _, k := range keys {
		if k == "id" {
			continue
		}
		order = append(order, k)
		if k == "resourceType" {
			order = append(order, "id")
			placed = true
		}
	}
	if !placed {
		order = append([]string{"id"}, order...)
	}
	res[fhir.OrderKey] = order
}

// orgFilters returns the org-identifier values of the backport filter criteria.
func orgFilters(sub object) []string {
	var out []string
	for _, x := range list(get(sub, "_criteria", "extension")) {
		if str(x, "url") != backportSD+"backport-filter-criteria" {
			continue
		}
		name, value, ok := strings.Cut(str(x, "valueString"), "=")
		if ok && strings.TrimSpace(name) == "org-identifier" && strings.TrimSpace(value) != "" {
			out = append(out, strings.TrimSpace(value))
		}
	}
	return out
}

// requestorIdentifiers returns identifier values (and system|value) of the Claim.provider entry.
func requestorIdentifiers(rec *ClaimRecord) map[string]bool {
	out := map[string]bool{}
	ref := str(rec.Claim, "provider", "reference")
	if ref == "" {
		return out
	}
	for _, en := range list(rec.Bundle["entry"]) {
		u := str(en, "fullUrl")
		if u != ref && !strings.HasSuffix(u, "/"+ref) {
			continue
		}
		for _, id := range list(get(en, "resource", "identifier")) {
			if v := str(id, "value"); v != "" {
				out[v] = true
				if s := str(id, "system"); s != "" {
					out[s+"|"+v] = true
				}
			}
		}
	}
	return out
}

// subscriptionMatches applies the org-identifier filter; no filter matches everything.
func subscriptionMatches(sub *SubscriptionRecord, requestor map[string]bool) bool {
	filters := orgFilters(sub.Resource)
	if len(filters) == 0 {
		return true
	}
	for _, f := range filters {
		if requestor[f] {
			return true
		}
	}
	return false
}

// statusParameters builds a SubscriptionStatus Parameters resource (SPEC 7.2).
func (e *engine) statusParameters(sub *SubscriptionRecord, kind, status, focus string) object {
	params := []any{
		obj("name", "subscription", "valueReference", obj("reference", sub.Base+"/Subscription/"+sub.ID)),
		obj("name", "topic", "valueCanonical", pasTopic),
		obj("name", "status", "valueCode", status),
		obj("name", "type", "valueCode", kind),
		obj("name", "events-since-subscription-start", "valueString", strconv.Itoa(sub.Events)),
	}
	if focus != "" {
		params = append(params, obj("name", "notification-event", "part", []any{
			obj("name", "event-number", "valueString", strconv.Itoa(sub.Events)),
			obj("name", "timestamp", "valueInstant", e.instant()),
			obj("name", "focus", "valueReference", obj("reference", focus)),
		}))
	}
	return obj("resourceType", "Parameters", "id", e.newID(), "parameter", params)
}

func (e *engine) statusEntry(sub *SubscriptionRecord, params object) object {
	return obj(
		"fullUrl", "urn:uuid:"+fhir.ID(params),
		"resource", params,
		"request", obj("method", "GET", "url", sub.Base+"/Subscription/"+sub.ID+"/$status"),
		"response", obj("status", "200"),
	)
}

func (e *engine) historyBundle(entries []any) object {
	return obj("resourceType", "Bundle", "id", e.newID(), "type", "history", "timestamp", e.instant(), "entry", entries)
}

// handshake builds the handshake notification (SPEC 7.2).
func (e *engine) handshake(sub *SubscriptionRecord) object {
	params := e.statusParameters(sub, "handshake", "requested", "")
	return e.historyBundle([]any{e.statusEntry(sub, params)})
}

// eventNotification builds a full-resource or id-only event notification for a
// finalized response Bundle (SPEC 7.4). sub.Events must already count this event.
func (e *engine) eventNotification(sub *SubscriptionRecord, response object) object {
	if sub.Content == contentFullResource {
		focus := "urn:uuid:" + fhir.ID(response)
		params := e.statusParameters(sub, "event-notification", "active", focus)
		return e.historyBundle([]any{
			e.statusEntry(sub, params),
			obj("fullUrl", focus, "resource", notificationCopy(response),
				"request", obj("method", "POST", "url", sub.Base+"/Bundle"),
				"response", obj("status", "201")),
		})
	}
	crID := fhir.ID(firstClaimResponse(response))
	focus := sub.Base + "/ClaimResponse/" + crID
	params := e.statusParameters(sub, "event-notification", "active", focus)
	return e.historyBundle([]any{
		e.statusEntry(sub, params),
		obj("fullUrl", focus,
			"request", obj("method", "PUT", "url", "ClaimResponse/"+crID),
			"response", obj("status", "200")),
	})
}
