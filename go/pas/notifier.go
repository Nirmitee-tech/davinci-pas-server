package pas

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/Nirmitee-tech/davinci-pas-server/go/internal/shared/audit"
)

// notification is one rest-hook POST (handshake or event notification).
type notification struct {
	Version        string
	SubscriptionID string
	Kind           string
	Endpoint       string
	Headers        [][2]string
	Body           []byte
	Delay          time.Duration
	// OnDelivered runs once after the first 2xx answer.
	OnDelivered func()
}

// deliveryEntry is one line of GET {prefix}/admin/notifications (SPEC 11).
type deliveryEntry struct {
	Version        string  `json:"version"`
	Kind           string  `json:"kind"`
	SubscriptionID string  `json:"subscription_id"`
	EndpointToken  *string `json:"endpoint_token"`
	Attempts       int     `json:"attempts"`
	Status         *int    `json:"status"`
	Delivered      bool    `json:"delivered"`
	QueuedAt       string  `json:"queued_at"`
}

const maxDeliveryLog = 1000

// notificationAudit is the audit line written for each delivery attempt.
type notificationAudit struct {
	TS             string          `json:"ts"`
	Version        string          `json:"version"`
	Operation      string          `json:"operation"`
	Kind           string          `json:"kind"`
	SubscriptionID string          `json:"subscriptionId"`
	Method         string          `json:"method"`
	Endpoint       string          `json:"endpoint"`
	Attempt        int             `json:"attempt"`
	Status         int             `json:"status,omitempty"`
	DurationMS     int64           `json:"durationMs"`
	Error          string          `json:"error,omitempty"`
	Request        json.RawMessage `json:"request,omitempty"`
}

// notifier delivers notifications in the background with the retry contract
// of SPEC 7.3: retry every interval while the answer is not 2xx, stop after the
// first 2xx or when the window ends. It never sends heartbeats.
type notifier struct {
	client  *http.Client
	retry   time.Duration
	window  time.Duration
	rewrite func(string) string
	log     *slog.Logger
	audit   *audit.Logger
	now     func() time.Time

	mu      sync.Mutex
	ctx     context.Context
	cancel  context.CancelFunc
	subs    map[string]context.CancelFunc
	subCtx  map[string]context.Context
	entries []*deliveryEntry
	closed  bool
	wg      sync.WaitGroup
}

func newNotifier(client *http.Client, retry, window time.Duration, rewrite func(string) string, log *slog.Logger, auditLog *audit.Logger, now func() time.Time) *notifier {
	n := &notifier{client: client, retry: retry, window: window, rewrite: rewrite, log: log, audit: auditLog, now: now}
	n.ctx, n.cancel = context.WithCancel(context.Background())
	n.subs = map[string]context.CancelFunc{}
	n.subCtx = map[string]context.Context{}
	if n.rewrite == nil {
		n.rewrite = func(s string) string { return s }
	}
	return n
}

// send queues a notification.
func (n *notifier) send(msg notification) {
	n.mu.Lock()
	if n.closed {
		n.mu.Unlock()
		return
	}
	key := storeKey(msg.Version, msg.SubscriptionID)
	ctx := n.subCtx[key]
	if ctx == nil {
		var cancel context.CancelFunc
		ctx, cancel = context.WithCancel(n.ctx)
		n.subCtx[key], n.subs[key] = ctx, cancel
	}
	entry := &deliveryEntry{
		Version:        msg.Version,
		Kind:           msg.Kind,
		SubscriptionID: msg.SubscriptionID,
		QueuedAt:       n.now().UTC().Format(instantLayout),
	}
	if token, ok := bearerToken(msg.Headers); ok {
		entry.EndpointToken = &token
	}
	n.entries = append(n.entries, entry)
	if len(n.entries) > maxDeliveryLog {
		n.entries = append([]*deliveryEntry(nil), n.entries[len(n.entries)-maxDeliveryLog:]...)
	}
	n.wg.Add(1)
	n.mu.Unlock()
	go n.deliver(ctx, msg, entry)
}

func (n *notifier) deliver(ctx context.Context, msg notification, entry *deliveryEntry) {
	defer n.wg.Done()
	deadline := time.Now().Add(n.window)
	if !sleepContext(ctx, msg.Delay) {
		return
	}
	for attempt := 1; ; attempt++ {
		start := time.Now()
		status, err := n.post(ctx, msg)
		delivered := err == nil && status >= 200 && status < 300
		n.mu.Lock()
		entry.Attempts = attempt
		entry.Delivered = delivered
		entry.Status = nil
		if err == nil {
			s := status
			entry.Status = &s
		}
		n.mu.Unlock()
		n.record(msg, attempt, status, err, time.Since(start), attempt == 1 || delivered)
		if delivered {
			n.log.Info("notification delivered", "version", msg.Version, "kind", msg.Kind,
				"subscription", msg.SubscriptionID, "attempt", attempt, "status", status)
			if msg.OnDelivered != nil {
				msg.OnDelivered()
			}
			return
		}
		if ctx.Err() != nil {
			return
		}
		if time.Now().Add(n.retry).After(deadline) {
			n.log.Warn("notification abandoned", "version", msg.Version, "kind", msg.Kind,
				"subscription", msg.SubscriptionID, "attempts", attempt, "status", status, "error", errString(err))
			return
		}
		if !sleepContext(ctx, n.retry) {
			return
		}
	}
}

func (n *notifier) post(ctx context.Context, msg notification) (int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, n.rewrite(msg.Endpoint), bytes.NewReader(msg.Body))
	if err != nil {
		return 0, err
	}
	for _, h := range msg.Headers {
		req.Header.Set(h[0], h[1])
	}
	resp, err := n.client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	return resp.StatusCode, nil
}

func (n *notifier) record(msg notification, attempt, status int, err error, took time.Duration, withBody bool) {
	rec := notificationAudit{
		TS:             n.now().UTC().Format(tsLayout),
		Version:        msg.Version,
		Operation:      "notification",
		Kind:           msg.Kind,
		SubscriptionID: msg.SubscriptionID,
		Method:         http.MethodPost,
		Endpoint:       msg.Endpoint,
		Attempt:        attempt,
		Status:         status,
		DurationMS:     took.Milliseconds(),
		Error:          errString(err),
	}
	if withBody {
		rec.Request = auditBody(msg.Body)
	}
	if werr := n.audit.Write(rec); werr != nil {
		n.log.Warn("audit write failed", "error", werr)
	}
	if !(err == nil && status >= 200 && status < 300) {
		n.log.Info("notification attempt", "version", msg.Version, "kind", msg.Kind, "subscription", msg.SubscriptionID,
			"attempt", attempt, "status", status, "error", errString(err))
	}
}

// snapshot returns a copy of the delivery log.
func (n *notifier) snapshot() []deliveryEntry {
	n.mu.Lock()
	defer n.mu.Unlock()
	out := make([]deliveryEntry, len(n.entries))
	for i, e := range n.entries {
		out[i] = *e
	}
	return out
}

// cancelSubscription stops pending deliveries for a deleted Subscription.
func (n *notifier) cancelSubscription(version, id string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	key := storeKey(version, id)
	if cancel := n.subs[key]; cancel != nil {
		cancel()
	}
	delete(n.subs, key)
	delete(n.subCtx, key)
}

// reset stops every pending delivery and clears the log.
func (n *notifier) reset() {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.cancel()
	n.ctx, n.cancel = context.WithCancel(context.Background())
	n.subs = map[string]context.CancelFunc{}
	n.subCtx = map[string]context.Context{}
	n.entries = nil
}

// close stops deliveries and waits for their goroutines.
func (n *notifier) close() {
	n.mu.Lock()
	n.closed = true
	n.cancel()
	n.mu.Unlock()
	n.wg.Wait()
}

func sleepContext(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
