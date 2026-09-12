package pas

import (
	"context"
	"sync"
	"time"
)

// ClaimRecord is the stored state of one prior authorization request, keyed by
// Claim.identifier[0] system|value within a version (SPEC 4.2).
type ClaimRecord struct {
	Version string
	Key     string
	// Revision increases on every submit; a pending auto finalize only applies to its own revision.
	Revision uint64
	// Base is the FHIR base the request arrived on, used in later notifications.
	Base         string
	FullURL      string
	Claim        map[string]any
	Bundle       map[string]any
	Results      []ItemResult
	LastResponse map[string]any
	UpdatedAt    time.Time
}

// ItemResult is the decision for one Claim.item.
type ItemResult struct {
	Index     int
	Sequence  any
	RuleID    string
	Decision  string
	Finalized bool
}

// SubscriptionRecord is a stored rest-hook Subscription.
type SubscriptionRecord struct {
	Version string
	ID      string
	// Base is the FHIR base the Subscription was created on.
	Base     string
	Resource map[string]any
	// Content is the backport payload content: full-resource or id-only.
	Content string
	// Events counts event notifications since the Subscription started.
	Events int
}

// Store keeps claims, ClaimResponses and Subscriptions. The handler serializes
// every call, so an implementation only needs to be safe for one caller at a
// time. Records returned by a Store may be modified by the handler and are
// written back with the matching Save method. Lookups of missing items return
// nil and no error.
type Store interface {
	SaveClaim(ctx context.Context, rec *ClaimRecord) error
	Claim(ctx context.Context, version, key string) (*ClaimRecord, error)
	// Claims returns the claims of a version in the order they were first saved.
	Claims(ctx context.Context, version string) ([]*ClaimRecord, error)
	SaveClaimResponse(ctx context.Context, version, id string, resource map[string]any) error
	ClaimResponse(ctx context.Context, version, id string) (map[string]any, error)
	SaveSubscription(ctx context.Context, rec *SubscriptionRecord) error
	Subscription(ctx context.Context, version, id string) (*SubscriptionRecord, error)
	// Subscriptions returns the Subscriptions of a version in creation order.
	Subscriptions(ctx context.Context, version string) ([]*SubscriptionRecord, error)
	DeleteSubscription(ctx context.Context, version, id string) (bool, error)
	// Reset removes everything.
	Reset(ctx context.Context) error
}

// MemoryStore is the default in-memory Store. State is lost on restart.
type MemoryStore struct {
	mu             sync.Mutex
	claims         map[string]*ClaimRecord
	claimOrder     []string
	claimResponses map[string]map[string]any
	subs           map[string]*SubscriptionRecord
	subOrder       []string
}

// NewMemoryStore returns an empty MemoryStore.
func NewMemoryStore() *MemoryStore {
	s := &MemoryStore{}
	s.clear()
	return s
}

func (s *MemoryStore) clear() {
	s.claims = map[string]*ClaimRecord{}
	s.claimOrder = nil
	s.claimResponses = map[string]map[string]any{}
	s.subs = map[string]*SubscriptionRecord{}
	s.subOrder = nil
}

func storeKey(version, key string) string { return version + "|" + key }

// SaveClaim implements Store.
func (s *MemoryStore) SaveClaim(_ context.Context, rec *ClaimRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := storeKey(rec.Version, rec.Key)
	if _, ok := s.claims[k]; !ok {
		s.claimOrder = append(s.claimOrder, k)
	}
	s.claims[k] = rec
	return nil
}

// Claim implements Store.
func (s *MemoryStore) Claim(_ context.Context, version, key string) (*ClaimRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.claims[storeKey(version, key)], nil
}

// Claims implements Store.
func (s *MemoryStore) Claims(_ context.Context, version string) ([]*ClaimRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []*ClaimRecord
	for _, k := range s.claimOrder {
		if rec := s.claims[k]; rec != nil && rec.Version == version {
			out = append(out, rec)
		}
	}
	return out, nil
}

// SaveClaimResponse implements Store.
func (s *MemoryStore) SaveClaimResponse(_ context.Context, version, id string, resource map[string]any) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.claimResponses[storeKey(version, id)] = resource
	return nil
}

// ClaimResponse implements Store.
func (s *MemoryStore) ClaimResponse(_ context.Context, version, id string) (map[string]any, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.claimResponses[storeKey(version, id)], nil
}

// SaveSubscription implements Store.
func (s *MemoryStore) SaveSubscription(_ context.Context, rec *SubscriptionRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := storeKey(rec.Version, rec.ID)
	if _, ok := s.subs[k]; !ok {
		s.subOrder = append(s.subOrder, k)
	}
	s.subs[k] = rec
	return nil
}

// Subscription implements Store.
func (s *MemoryStore) Subscription(_ context.Context, version, id string) (*SubscriptionRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.subs[storeKey(version, id)], nil
}

// Subscriptions implements Store.
func (s *MemoryStore) Subscriptions(_ context.Context, version string) ([]*SubscriptionRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []*SubscriptionRecord
	for _, k := range s.subOrder {
		if rec := s.subs[k]; rec != nil && rec.Version == version {
			out = append(out, rec)
		}
	}
	return out, nil
}

// DeleteSubscription implements Store.
func (s *MemoryStore) DeleteSubscription(_ context.Context, version, id string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := storeKey(version, id)
	if _, ok := s.subs[k]; !ok {
		return false, nil
	}
	delete(s.subs, k)
	for i, v := range s.subOrder {
		if v == k {
			s.subOrder = append(s.subOrder[:i:i], s.subOrder[i+1:]...)
			break
		}
	}
	return true, nil
}

// Reset implements Store.
func (s *MemoryStore) Reset(context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.clear()
	return nil
}
