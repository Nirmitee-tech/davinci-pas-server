// Package pas is the Da Vinci Prior Authorization Support (PAS) payer server
// as an embeddable http.Handler. It serves PAS 2.2.1 at {prefix}/v221/fhir and
// PAS STU 2.0.1 at {prefix}/v201/fhir: CapabilityStatement, Claim/$submit,
// Claim/$inquire, rest-hook Subscriptions with handshake and event
// notifications, and ClaimResponse read. Decisions come from a deterministic
// rules file. It keeps no global state, so several handlers (or the CRD and
// DTR servers) can share one process.
package pas

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"runtime/debug"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/Nirmitee-tech/davinci-pas-server/go/internal/shared/audit"
	"github.com/Nirmitee-tech/davinci-pas-server/go/internal/shared/auth"
	"github.com/Nirmitee-tech/davinci-pas-server/go/internal/shared/fhir"
)

const (
	maxBodyBytes    = 16 << 20
	maxAuditBody    = 4 << 20
	tsLayout        = "2006-01-02T15:04:05.000Z07:00"
	fhirContentType = "application/fhir+json; charset=utf-8"
	jsonContentType = "application/json; charset=utf-8"
)

var fhirIDPattern = regexp.MustCompile(`^[A-Za-z0-9\-.]{1,64}$`)

// Handler is the PAS server. Create it with NewHandler; call Close to stop
// background notification delivery and pending auto finalize timers.
type Handler struct {
	cfg      Config
	log      *slog.Logger
	audit    *audit.Logger
	store    Store
	engine   *engine
	notifier *notifier

	mu       sync.Mutex
	revision uint64
	timers   map[string]*time.Timer
	closed   bool
}

// call is the per-request context passed to route functions.
type call struct {
	version string
	base    string
	id      string
	rec     *auditRecord
}

type route struct {
	methods   []string
	operation string
	auth      bool
	serve     func(w http.ResponseWriter, r *http.Request, c *call)
}

// NewHandler loads the adjudication rules and returns the PAS handler (a
// *Handler). Routes are relative to cfg.Prefix.
func NewHandler(cfg Config) (http.Handler, error) {
	cfg.normalize()
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	rules, source, err := cfg.resolveRules()
	if err != nil {
		return nil, err
	}
	log := cfg.Logger
	if log == nil {
		log = slog.Default()
	}
	store := cfg.Store
	if store == nil {
		store = NewMemoryStore()
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	client := cfg.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: cfg.Outbound.Timeout.Duration}
	}
	h := &Handler{
		cfg:    cfg,
		log:    log,
		audit:  cfg.Audit,
		store:  store,
		engine: &engine{rules: rules, now: now, newID: newUUID},
		timers: map[string]*time.Timer{},
	}
	h.notifier = newNotifier(client, cfg.NotificationRetry.Duration, cfg.NotificationMax.Duration,
		cfg.Outbound.RewriteURL, log, cfg.Audit, now)
	log.Info("pas handler ready", "rules", source, "rulesVersion", rules.RulesVersion, "prefix", cfg.Prefix,
		"auth", len(cfg.AuthTokens) > 0, "admin", cfg.EnableAdmin)
	return h, nil
}

// Close stops pending auto finalize timers and notification deliveries.
func (h *Handler) Close() error {
	h.mu.Lock()
	h.closed = true
	h.stopTimersLocked()
	h.mu.Unlock()
	h.notifier.close()
	return nil
}

func (h *Handler) stopTimersLocked() {
	for k, t := range h.timers {
		t.Stop()
		delete(h.timers, k)
	}
}

// ServeHTTP implements http.Handler.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	defer func() {
		if rec := recover(); rec != nil {
			if rec == http.ErrAbortHandler {
				panic(rec)
			}
			h.log.Error("panic", "path", r.URL.Path, "panic", fmt.Sprint(rec), "stack", string(debug.Stack()))
			writeFHIR(w, http.StatusInternalServerError, outcome("fatal", "exception", "Internal server error.", ""))
		}
	}()
	hdr := w.Header()
	hdr.Set("Access-Control-Allow-Origin", "*")
	hdr.Set("Access-Control-Allow-Headers", "Authorization, Content-Type, Accept")
	hdr.Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
	hdr.Set("Access-Control-Expose-Headers", "Location")

	rest, ok := strings.CutPrefix(r.URL.Path, h.cfg.Prefix)
	if !ok || (rest != "" && !strings.HasPrefix(rest, "/")) {
		writeFHIR(w, http.StatusNotFound, noRoute(r))
		return
	}
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if rest == "/health" {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			writeFHIR(w, http.StatusMethodNotAllowed, outcome("error", "not-supported", "Use GET on "+r.URL.Path+".", ""))
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
		return
	}

	start := time.Now()
	rec := &auditRecord{TS: start.UTC().Format(tsLayout), Method: r.Method, Path: r.URL.Path, Remote: r.RemoteAddr}
	rw := &recorder{ResponseWriter: w}
	defer func() {
		rec.Status = rw.status
		rec.DurationMS = time.Since(start).Milliseconds()
		if rec.Response == nil {
			if rw.truncated {
				rec.Response = audit.Body([]byte(`"response body larger than the audit limit"`))
			} else {
				rec.Response = auditBody(rw.body.Bytes())
			}
		}
		if err := h.audit.Write(rec); err != nil {
			h.log.Warn("audit write failed", "error", err)
		}
		h.log.Info("request", "method", r.Method, "path", r.URL.Path, "operation", rec.Operation, "status", rec.Status,
			"durationMs", rec.DurationMS, "outcome", rec.Outcome, "error", rec.Error)
	}()

	if strings.HasPrefix(rest, "/admin/") {
		h.serveAdmin(rw, r, rest, rec)
		return
	}

	version, segs, isFHIR := splitFHIRPath(rest)
	rec.Version = version
	rt, id, known := h.match(segs)
	if !isFHIR || !known {
		rec.Operation, rec.Error = "unknown", "no route"
		writeFHIR(rw, http.StatusNotFound, noRoute(r))
		return
	}
	rec.Operation = rt.operation
	if !slices.Contains(rt.methods, r.Method) {
		rec.Error = "method not allowed"
		rw.Header().Set("Allow", strings.Join(rt.methods, ", "))
		writeFHIR(rw, http.StatusMethodNotAllowed, outcome("error", "not-supported",
			r.Method+" is not supported on "+r.URL.Path+"; use "+strings.Join(rt.methods, " or ")+".", ""))
		return
	}
	if rt.auth && len(h.cfg.AuthTokens) > 0 {
		valid, presented := h.authorized(r)
		if presented {
			rec.Auth = "bearer"
		}
		if !valid {
			challenge := "Bearer"
			rec.Error = "missing bearer token"
			if presented {
				challenge = `Bearer error="invalid_token"`
				rec.Error = "invalid bearer token"
			}
			rw.Header().Set("WWW-Authenticate", challenge)
			writeFHIR(rw, http.StatusUnauthorized, outcome("error", "security", "A valid bearer token is required.", ""))
			return
		}
	}
	rt.serve(rw, r, &call{version: version, base: h.fhirBase(r, version), id: id, rec: rec})
}

func noRoute(r *http.Request) object {
	return outcome("error", "not-found", "No route for "+r.Method+" "+r.URL.Path+".", "")
}

// splitFHIRPath splits "/v221/fhir/Claim/$submit" into ("v221", ["Claim", "$submit"], true).
func splitFHIRPath(rest string) (string, []string, bool) {
	parts := strings.SplitN(strings.TrimPrefix(rest, "/"), "/", 3)
	if len(parts) < 2 || parts[1] != "fhir" || !slices.Contains(Versions, parts[0]) {
		return "", nil, false
	}
	var segs []string
	if len(parts) == 3 {
		if trimmed := strings.Trim(parts[2], "/"); trimmed != "" {
			segs = strings.Split(trimmed, "/")
		}
	}
	return parts[0], segs, true
}

func (h *Handler) match(segs []string) (route, string, bool) {
	get, post, put, del := http.MethodGet, http.MethodPost, http.MethodPut, http.MethodDelete
	n := len(segs)
	switch {
	case n == 1 && segs[0] == "metadata":
		return route{[]string{get}, "metadata", false, h.metadata}, "", true
	case n == 2 && segs[0] == "Claim" && segs[1] == "$submit":
		return route{[]string{post}, "submit", true, h.submit}, "", true
	case n == 2 && segs[0] == "Claim" && segs[1] == "$inquire":
		return route{[]string{post}, "inquire", true, h.inquire}, "", true
	case n == 1 && segs[0] == "Subscription":
		return route{[]string{post}, "subscription-create", true, h.subscriptionCreate}, "", true
	case n == 2 && segs[0] == "Subscription" && fhirIDPattern.MatchString(segs[1]):
		return route{[]string{get, put, del}, "subscription", true, h.subscriptionInstance}, segs[1], true
	case n == 3 && segs[0] == "Subscription" && segs[2] == "$status" && fhirIDPattern.MatchString(segs[1]):
		return route{[]string{get}, "subscription-status", true, h.subscriptionStatus}, segs[1], true
	case n == 2 && segs[0] == "ClaimResponse" && fhirIDPattern.MatchString(segs[1]):
		return route{[]string{get}, "claimresponse-read", true, h.claimResponseRead}, segs[1], true
	}
	return route{}, "", false
}

// fhirBase computes the external FHIR base for this request (SPEC 0).
func (h *Handler) fhirBase(r *http.Request, version string) string {
	suffix := h.cfg.Prefix + "/" + version + "/fhir"
	if h.cfg.PublicBaseURL != "" {
		return h.cfg.PublicBaseURL + suffix
	}
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	if p := firstValue(r.Header.Get("X-Forwarded-Proto")); p == "http" || p == "https" {
		scheme = p
	}
	host := r.Host
	if fh := firstValue(r.Header.Get("X-Forwarded-Host")); fh != "" {
		host = fh
	}
	return scheme + "://" + host + suffix
}

func firstValue(v string) string {
	first, _, _ := strings.Cut(v, ",")
	return strings.TrimSpace(first)
}

// authorized checks the bearer token against PAS_AUTH_TOKENS (SPEC 3).
func (h *Handler) authorized(r *http.Request) (valid, presented bool) {
	scheme, token, found := strings.Cut(strings.TrimSpace(r.Header.Get("Authorization")), " ")
	token = strings.TrimSpace(token)
	if !found || !strings.EqualFold(scheme, "Bearer") || token == "" {
		return false, false
	}
	for _, t := range h.cfg.AuthTokens {
		if auth.EqualConstantTime(token, t) {
			valid = true
		}
	}
	return valid, true
}

// ---------------------------------------------------------------- FHIR routes

func (h *Handler) metadata(w http.ResponseWriter, _ *http.Request, c *call) {
	h.respond(w, c, http.StatusOK, h.engine.metadata(c.version, c.base), "")
}

func (h *Handler) submit(w http.ResponseWriter, r *http.Request, c *call) {
	body, problem := h.readBody(w, r, c)
	if problem != nil {
		h.respond(w, c, http.StatusBadRequest, problem, "")
		return
	}
	h.mu.Lock()
	status, resp, rec := h.engine.submit(c.version, c.base, body)
	var err error
	if rec != nil {
		err = h.saveSubmissionLocked(r.Context(), rec)
	}
	h.mu.Unlock()
	if err != nil {
		h.serverError(w, c, err)
		return
	}
	h.respond(w, c, status, resp, summarizeResponse(resp))
}

func (h *Handler) saveSubmissionLocked(ctx context.Context, rec *ClaimRecord) error {
	h.revision++
	rec.Revision = h.revision
	rec.UpdatedAt = h.engine.now()
	if err := h.store.SaveClaim(ctx, rec); err != nil {
		return err
	}
	if cr := firstClaimResponse(rec.LastResponse); cr != nil {
		if err := h.store.SaveClaimResponse(ctx, rec.Version, fhir.ID(cr), cr); err != nil {
			return err
		}
	}
	h.scheduleFinalizeLocked(rec)
	return nil
}

func (h *Handler) scheduleFinalizeLocked(rec *ClaimRecord) {
	key := storeKey(rec.Version, rec.Key)
	if t := h.timers[key]; t != nil {
		t.Stop()
		delete(h.timers, key)
	}
	delay := h.cfg.PendedAutoFinalize.Duration
	if delay <= 0 || h.closed || !h.engine.autoFinalizable(rec) {
		return
	}
	version, claimKey, revision := rec.Version, rec.Key, rec.Revision
	h.timers[key] = time.AfterFunc(delay, func() { h.autoFinalize(version, claimKey, revision) })
}

func (h *Handler) autoFinalize(version, claimKey string, revision uint64) {
	ctx := context.Background()
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return
	}
	rec, err := h.store.Claim(ctx, version, claimKey)
	if err != nil {
		h.log.Error("auto finalize: load claim", "version", version, "claim", claimKey, "error", err)
		return
	}
	if rec == nil || rec.Revision != revision {
		return
	}
	delete(h.timers, storeKey(version, claimKey))
	crID, n, changed, err := h.finalizeLocked(ctx, rec)
	if err != nil {
		h.log.Error("auto finalize", "version", version, "claim", claimKey, "error", err)
	}
	if changed {
		h.log.Info("auto finalize", "version", version, "claim", claimKey, "claimResponse", crID, "notifications", n)
	}
}

// finalizeLocked finalizes one record, stores it and queues event notifications.
func (h *Handler) finalizeLocked(ctx context.Context, rec *ClaimRecord) (string, int, bool, error) {
	resp, changed := h.engine.finalize(rec)
	if !changed {
		return "", 0, false, nil
	}
	rec.UpdatedAt = h.engine.now()
	if err := h.store.SaveClaim(ctx, rec); err != nil {
		return "", 0, true, err
	}
	cr := firstClaimResponse(resp)
	crID := fhir.ID(cr)
	if err := h.store.SaveClaimResponse(ctx, rec.Version, crID, cr); err != nil {
		return crID, 0, true, err
	}
	n, err := h.notifyLocked(ctx, rec, resp)
	return crID, n, true, err
}

// notifyLocked queues one event notification per active or requested
// Subscription of the record's version whose filter matches (SPEC 7.3).
func (h *Handler) notifyLocked(ctx context.Context, rec *ClaimRecord, resp object) (int, error) {
	subs, err := h.store.Subscriptions(ctx, rec.Version)
	if err != nil {
		return 0, err
	}
	requestor := requestorIdentifiers(rec)
	count := 0
	for _, sub := range subs {
		if st := str(sub.Resource, "status"); st != "active" && st != "requested" {
			continue
		}
		if !subscriptionMatches(sub, requestor) {
			continue
		}
		sub.Events++
		if err := h.store.SaveSubscription(ctx, sub); err != nil {
			return count, err
		}
		body, err := fhir.MarshalOrdered(h.engine.eventNotification(sub, resp))
		if err != nil {
			return count, err
		}
		h.notifier.send(notification{
			Version:        sub.Version,
			SubscriptionID: sub.ID,
			Kind:           "event-notification",
			Endpoint:       str(sub.Resource, "channel", "endpoint"),
			Headers:        channelHeaders(sub.Resource),
			Body:           body,
		})
		count++
	}
	return count, nil
}

func (h *Handler) inquire(w http.ResponseWriter, r *http.Request, c *call) {
	body, problem := h.readBody(w, r, c)
	if problem != nil {
		h.respond(w, c, http.StatusBadRequest, problem, "")
		return
	}
	var lookupErr error
	h.mu.Lock()
	status, resp := h.engine.inquire(c.version, c.base, body, func(key string) *ClaimRecord {
		rec, err := h.store.Claim(r.Context(), c.version, key)
		if err != nil {
			lookupErr = err
		}
		return rec
	})
	h.mu.Unlock()
	if lookupErr != nil {
		h.serverError(w, c, lookupErr)
		return
	}
	h.respond(w, c, status, resp, summarizeResponse(resp))
}

func (h *Handler) subscriptionCreate(w http.ResponseWriter, r *http.Request, c *call) {
	body, problem := h.readBody(w, r, c)
	if problem != nil {
		h.respond(w, c, http.StatusBadRequest, problem, "")
		return
	}
	sub, content, status, oo := validateSubscription(body)
	if oo != nil {
		h.respond(w, c, status, oo, "")
		return
	}
	id := h.engine.newID()
	setResourceID(sub, id)
	set(sub, "status", "requested")
	rec := &SubscriptionRecord{Version: c.version, ID: id, Base: c.base, Resource: sub, Content: content}
	endpoint, headers := str(sub, "channel", "endpoint"), channelHeaders(sub)

	h.mu.Lock()
	err := h.store.SaveSubscription(r.Context(), rec)
	var respBody, handshake []byte
	if err == nil {
		respBody, err = fhir.MarshalOrdered(sub)
	}
	if err == nil {
		handshake, err = fhir.MarshalOrdered(h.engine.handshake(rec))
	}
	h.mu.Unlock()
	if err != nil {
		h.serverError(w, c, err)
		return
	}
	version := c.version
	h.notifier.send(notification{
		Version:        version,
		SubscriptionID: id,
		Kind:           "handshake",
		Endpoint:       endpoint,
		Headers:        headers,
		Body:           handshake,
		Delay:          h.cfg.HandshakeDelay.Duration,
		OnDelivered:    func() { h.markActive(version, id) },
	})
	w.Header().Set("Location", c.base+"/Subscription/"+id)
	h.respondBytes(w, c, http.StatusCreated, respBody, "Subscription/"+id+" "+content)
}

// markActive sets a requested Subscription to active after a 2xx handshake.
func (h *Handler) markActive(version, id string) {
	ctx := context.Background()
	h.mu.Lock()
	defer h.mu.Unlock()
	rec, err := h.store.Subscription(ctx, version, id)
	if err != nil || rec == nil {
		return
	}
	if str(rec.Resource, "status") == "requested" {
		set(rec.Resource, "status", "active")
		if err := h.store.SaveSubscription(ctx, rec); err != nil {
			h.log.Error("activate subscription", "version", version, "subscription", id, "error", err)
		}
	}
}

func (h *Handler) subscriptionInstance(w http.ResponseWriter, r *http.Request, c *call) {
	ctx := r.Context()
	switch r.Method {
	case http.MethodGet:
		h.mu.Lock()
		rec, err := h.store.Subscription(ctx, c.version, c.id)
		var b []byte
		if err == nil && rec != nil {
			b, err = fhir.MarshalOrdered(rec.Resource)
		}
		h.mu.Unlock()
		switch {
		case err != nil:
			h.serverError(w, c, err)
		case rec == nil:
			h.respond(w, c, http.StatusNotFound, notFound("Subscription", c.id), "")
		default:
			h.respondBytes(w, c, http.StatusOK, b, "")
		}
	case http.MethodPut:
		body, problem := h.readBody(w, r, c)
		if problem != nil {
			h.respond(w, c, http.StatusBadRequest, problem, "")
			return
		}
		sub, content, status, oo := validateSubscription(body)
		if oo != nil {
			h.respond(w, c, status, oo, "")
			return
		}
		if bodyID := fhir.ID(sub); bodyID != "" && bodyID != c.id {
			h.respond(w, c, http.StatusBadRequest, outcome("error", "invalid", "Subscription.id must match the id in the URL.", "Subscription.id"), "")
			return
		}
		h.mu.Lock()
		rec, err := h.store.Subscription(ctx, c.version, c.id)
		var b []byte
		if err == nil && rec != nil {
			setResourceID(sub, c.id)
			set(sub, "status", str(rec.Resource, "status"))
			rec.Resource, rec.Content = sub, content
			if err = h.store.SaveSubscription(ctx, rec); err == nil {
				b, err = fhir.MarshalOrdered(sub)
			}
		}
		h.mu.Unlock()
		switch {
		case err != nil:
			h.serverError(w, c, err)
		case rec == nil:
			h.respond(w, c, http.StatusNotFound, notFound("Subscription", c.id), "")
		default:
			h.respondBytes(w, c, http.StatusOK, b, "")
		}
	case http.MethodDelete:
		h.mu.Lock()
		deleted, err := h.store.DeleteSubscription(ctx, c.version, c.id)
		h.mu.Unlock()
		switch {
		case err != nil:
			h.serverError(w, c, err)
		case !deleted:
			h.respond(w, c, http.StatusNotFound, notFound("Subscription", c.id), "")
		default:
			h.notifier.cancelSubscription(c.version, c.id)
			c.rec.Outcome = "deleted"
			w.WriteHeader(http.StatusNoContent)
		}
	}
}

func (h *Handler) subscriptionStatus(w http.ResponseWriter, r *http.Request, c *call) {
	h.mu.Lock()
	rec, err := h.store.Subscription(r.Context(), c.version, c.id)
	var b []byte
	if err == nil && rec != nil {
		b, err = fhir.MarshalOrdered(h.engine.statusParameters(rec, "query-status", str(rec.Resource, "status"), ""))
	}
	h.mu.Unlock()
	switch {
	case err != nil:
		h.serverError(w, c, err)
	case rec == nil:
		h.respond(w, c, http.StatusNotFound, notFound("Subscription", c.id), "")
	default:
		h.respondBytes(w, c, http.StatusOK, b, "")
	}
}

func (h *Handler) claimResponseRead(w http.ResponseWriter, r *http.Request, c *call) {
	h.mu.Lock()
	cr, err := h.store.ClaimResponse(r.Context(), c.version, c.id)
	var b []byte
	if err == nil && cr != nil {
		b, err = fhir.MarshalOrdered(cr)
	}
	h.mu.Unlock()
	switch {
	case err != nil:
		h.serverError(w, c, err)
	case cr == nil:
		h.respond(w, c, http.StatusNotFound, notFound("ClaimResponse", c.id), "")
	default:
		h.respondBytes(w, c, http.StatusOK, b, "")
	}
}

func notFound(resourceType, id string) object {
	return outcome("error", "not-found", resourceType+"/"+id+" was not found.", "")
}

// readBody reads the request body. It returns the decoded JSON (nil when the
// body is not JSON, which the operation reports as a structure problem) or an
// OperationOutcome when the body cannot be read.
func (h *Handler) readBody(w http.ResponseWriter, r *http.Request, c *call) (any, object) {
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	c.rec.Request = auditBody(raw)
	if err != nil {
		return nil, outcome("error", "too-long", "The request body could not be read or exceeds 16 MiB.", "")
	}
	v, err := fhir.DecodeOrdered(raw)
	if err != nil {
		return nil, nil
	}
	return v, nil
}

func (h *Handler) respond(w http.ResponseWriter, c *call, status int, body object, summary string) {
	c.rec.Outcome = summary
	if status >= 400 && fhir.Type(body) == "OperationOutcome" {
		c.rec.Error = outcomeText(body)
	}
	writeFHIR(w, status, body)
}

func (h *Handler) respondBytes(w http.ResponseWriter, c *call, status int, body []byte, summary string) {
	c.rec.Outcome = summary
	w.Header().Set("Content-Type", fhirContentType)
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

func (h *Handler) serverError(w http.ResponseWriter, c *call, err error) {
	h.log.Error("request failed", "operation", c.rec.Operation, "error", err)
	c.rec.Error = err.Error()
	writeFHIR(w, http.StatusInternalServerError, outcome("error", "exception", "The server could not complete the request.", ""))
}

// ---------------------------------------------------------------- control routes

func (h *Handler) serveAdmin(w http.ResponseWriter, r *http.Request, rest string, rec *auditRecord) {
	rec.Operation = "admin"
	if !h.cfg.EnableAdmin {
		rec.Error = "control routes disabled"
		writeFHIR(w, http.StatusNotFound, noRoute(r))
		return
	}
	if r.Header.Get("X-Forwarded-For") != "" || r.Header.Get("Forwarded") != "" {
		rec.Error = "proxied control request"
		writeFHIR(w, http.StatusForbidden, outcome("error", "forbidden", "Control endpoints are not available through a proxy.", ""))
		return
	}
	var method string
	var serve func()
	switch rest {
	case "/admin/pended/finalize":
		rec.Operation, method, serve = "admin-finalize", http.MethodPost, func() { h.adminFinalize(w, r, rec) }
	case "/admin/reset":
		rec.Operation, method, serve = "admin-reset", http.MethodPost, func() { h.adminReset(w, r, rec) }
	case "/admin/notifications":
		rec.Operation, method, serve = "admin-notifications", http.MethodGet, func() {
			writeJSON(w, http.StatusOK, map[string]any{"notifications": h.notifier.snapshot()})
		}
	default:
		rec.Error = "no route"
		writeFHIR(w, http.StatusNotFound, noRoute(r))
		return
	}
	if r.Method != method {
		rec.Error = "method not allowed"
		w.Header().Set("Allow", method)
		writeFHIR(w, http.StatusMethodNotAllowed, outcome("error", "not-supported", "Use "+method+" on "+r.URL.Path+".", ""))
		return
	}
	serve()
}

func (h *Handler) adminFinalize(w http.ResponseWriter, r *http.Request, rec *auditRecord) {
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
	rec.Request = auditBody(raw)
	var req struct {
		Version string `json:"version"`
	}
	if err == nil && len(bytes.TrimSpace(raw)) > 0 {
		err = json.Unmarshal(raw, &req)
	}
	selected := Versions
	if err == nil && req.Version != "" {
		if !slices.Contains(Versions, req.Version) {
			err = fmt.Errorf("unknown version %q", req.Version)
		}
		selected = []string{req.Version}
	}
	if err != nil {
		rec.Error = err.Error()
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": `the body must be empty or {"version": "v221"} or {"version": "v201"}`})
		return
	}
	ctx := r.Context()
	finalized := []string{}
	total := 0
	var failure error
	h.mu.Lock()
	for _, v := range selected {
		claims, err := h.store.Claims(ctx, v)
		if err != nil {
			failure = err
			break
		}
		for _, c := range claims {
			id, n, changed, err := h.finalizeLocked(ctx, c)
			if err != nil {
				failure = err
			}
			if changed {
				finalized = append(finalized, id)
				total += n
			}
		}
	}
	h.mu.Unlock()
	if failure != nil {
		h.log.Error("finalize failed", "error", failure)
		rec.Error = failure.Error()
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "finalize failed"})
		return
	}
	rec.Outcome = fmt.Sprintf("finalized %d, notifications %d", len(finalized), total)
	writeJSON(w, http.StatusOK, struct {
		Finalized     []string `json:"finalized"`
		Notifications int      `json:"notifications"`
	}{finalized, total})
}

func (h *Handler) adminReset(w http.ResponseWriter, r *http.Request, rec *auditRecord) {
	h.mu.Lock()
	h.stopTimersLocked()
	err := h.store.Reset(r.Context())
	h.notifier.reset()
	h.mu.Unlock()
	if err != nil {
		h.log.Error("reset failed", "error", err)
		rec.Error = err.Error()
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "reset failed"})
		return
	}
	rec.Outcome = "reset"
	w.WriteHeader(http.StatusNoContent)
}

// ---------------------------------------------------------------- audit and responses

// auditRecord is one JSONL audit line. Bearer tokens are redacted from bodies and never logged.
type auditRecord struct {
	TS         string          `json:"ts"`
	Version    string          `json:"version,omitempty"`
	Operation  string          `json:"operation"`
	Method     string          `json:"method"`
	Path       string          `json:"path"`
	Status     int             `json:"status"`
	DurationMS int64           `json:"durationMs"`
	Remote     string          `json:"remote,omitempty"`
	Auth       string          `json:"auth,omitempty"`
	Outcome    string          `json:"outcome,omitempty"`
	Error      string          `json:"error,omitempty"`
	Request    json.RawMessage `json:"request,omitempty"`
	Response   json.RawMessage `json:"response,omitempty"`
}

var bearerPattern = regexp.MustCompile(`(?i)(bearer\s+)[^"\\\s,]+`)

// redact replaces bearer token values (for example in Subscription.channel.header).
func redact(b []byte) []byte { return bearerPattern.ReplaceAll(b, []byte("${1}[redacted]")) }

// auditBody returns a redacted body for the audit log, or a note when it is too large.
func auditBody(b []byte) json.RawMessage {
	if len(b) > maxAuditBody {
		return audit.Body([]byte(fmt.Sprintf("%q", fmt.Sprintf("body of %d bytes omitted", len(b)))))
	}
	return audit.Body(redact(b))
}

type recorder struct {
	http.ResponseWriter
	status    int
	body      bytes.Buffer
	truncated bool
}

func (r *recorder) WriteHeader(status int) {
	if r.status == 0 {
		r.status = status
	}
	r.ResponseWriter.WriteHeader(status)
}

func (r *recorder) Write(b []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	if r.body.Len()+len(b) <= maxAuditBody {
		r.body.Write(b)
	} else {
		r.truncated = true
	}
	return r.ResponseWriter.Write(b)
}

func writeFHIR(w http.ResponseWriter, status int, body object) {
	b, err := fhir.MarshalOrdered(body)
	if err != nil {
		status = http.StatusInternalServerError
		b = []byte(`{"resourceType":"OperationOutcome","issue":[{"severity":"fatal","code":"exception","diagnostics":"response encoding failed"}]}`)
	}
	w.Header().Set("Content-Type", fhirContentType)
	w.WriteHeader(status)
	_, _ = w.Write(b)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		status = http.StatusInternalServerError
		buf.Reset()
		buf.WriteString(`{"error":"response encoding failed"}`)
	}
	w.Header().Set("Content-Type", jsonContentType)
	w.WriteHeader(status)
	_, _ = w.Write(bytes.TrimRight(buf.Bytes(), "\n"))
}
