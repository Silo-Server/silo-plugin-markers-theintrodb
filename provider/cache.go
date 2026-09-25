package provider

import (
	"container/list"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"
)

type cacheEntry struct {
	key       string
	value     *mediaResponse
	expiresAt time.Time
}

// responseCache bounds both positive and negative lookup results. Each cache
// instance owns its in-flight requests so replacing it also isolates responses
// fetched before a credential change or submission.
type responseCache struct {
	mu      sync.Mutex
	entries map[string]*list.Element
	order   list.List
	limit   int
	flights singleflight.Group
}

func newResponseCache(limit int) *responseCache {
	return &responseCache{entries: make(map[string]*list.Element), limit: limit}
}

func (c *responseCache) Get(key string) (*mediaResponse, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	element, ok := c.entries[key]
	if !ok {
		return nil, false
	}
	entry := element.Value.(cacheEntry)
	if !time.Now().Before(entry.expiresAt) {
		c.remove(element)
		return nil, false
	}
	c.order.MoveToFront(element)
	return entry.value, true
}

func (c *responseCache) Set(key string, value *mediaResponse, ttl time.Duration) {
	if c.limit <= 0 || ttl <= 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	entry := cacheEntry{key: key, value: value, expiresAt: time.Now().Add(ttl)}
	if element, ok := c.entries[key]; ok {
		element.Value = entry
		c.order.MoveToFront(element)
		return
	}
	c.entries[key] = c.order.PushFront(entry)
	if c.order.Len() > c.limit {
		c.remove(c.order.Back())
	}
}

func (c *responseCache) remove(element *list.Element) {
	delete(c.entries, element.Value.(cacheEntry).key)
	c.order.Remove(element)
}
