package auth

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"
)

// minRefreshInterval limits forced JWKS refetches when an unknown kid arrives.
const minRefreshInterval = 15 * time.Second

// KeyFetcher downloads and caches JWK Sets by URL.
type KeyFetcher struct {
	client  *http.Client
	ttl     time.Duration
	rewrite func(string) string
	now     func() time.Time

	mu      sync.Mutex
	entries map[string]jwksEntry
}

type jwksEntry struct {
	keys    []JWK
	fetched time.Time
}

// NewKeyFetcher returns a cache with the given TTL. rewrite may be nil.
func NewKeyFetcher(client *http.Client, ttl time.Duration, rewrite func(string) string) *KeyFetcher {
	if client == nil {
		client = &http.Client{Timeout: 3 * time.Second}
	}
	if rewrite == nil {
		rewrite = func(s string) string { return s }
	}
	return &KeyFetcher{client: client, ttl: ttl, rewrite: rewrite, now: time.Now, entries: map[string]jwksEntry{}}
}

// Keys returns the keys published at url, fetching when the cache is empty,
// stale, or refresh is requested (rate limited).
func (f *KeyFetcher) Keys(ctx context.Context, url string, refresh bool) ([]JWK, error) {
	f.mu.Lock()
	entry, ok := f.entries[url]
	f.mu.Unlock()
	now := f.now()
	if ok {
		age := now.Sub(entry.fetched)
		if (!refresh && age < f.ttl) || (refresh && age < minRefreshInterval) {
			return entry.keys, nil
		}
	}
	keys, err := f.fetch(ctx, url)
	if err != nil {
		if ok {
			// Serve stale keys rather than failing when the issuer is briefly unavailable.
			return entry.keys, nil
		}
		return nil, err
	}
	f.mu.Lock()
	f.entries[url] = jwksEntry{keys: keys, fetched: now}
	f.mu.Unlock()
	return keys, nil
}

func (f *KeyFetcher) fetch(ctx context.Context, url string) ([]JWK, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, f.rewrite(url), nil)
	if err != nil {
		return nil, fmt.Errorf("jwks %s: %w", url, err)
	}
	req.Header.Set("Accept", "application/json")
	resp, err := f.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("jwks %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("jwks %s: HTTP %d", url, resp.StatusCode)
	}
	var set JWKSet
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&set); err != nil {
		return nil, fmt.Errorf("jwks %s: %w", url, err)
	}
	return set.Keys, nil
}
