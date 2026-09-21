package provider

import (
	"testing"
	"time"
)

func TestResponseCacheBoundsAndExpiresEntries(t *testing.T) {
	cache := newResponseCache(2)
	cache.Set("a", &mediaResponse{TmdbID: 1}, time.Hour)
	cache.Set("b", nil, time.Hour)
	if _, ok := cache.Get("a"); !ok {
		t.Fatal("missing first entry")
	}
	cache.Set("c", nil, time.Hour)
	if _, ok := cache.Get("b"); ok {
		t.Fatal("least recently used entry survived capacity eviction")
	}
	if value, ok := cache.Get("c"); !ok || value != nil {
		t.Fatal("negative lookup was not cached")
	}
	cache.Set("a", &mediaResponse{TmdbID: 2}, time.Hour)
	if len(cache.entries) != 2 {
		t.Fatalf("cache has %d entries, want 2", len(cache.entries))
	}
	element := cache.entries["a"]
	entry := element.Value.(cacheEntry)
	entry.expiresAt = time.Now().Add(-time.Second)
	element.Value = entry
	if _, ok := cache.Get("a"); ok {
		t.Fatal("expired entry returned")
	}
	if len(cache.entries) != 1 {
		t.Fatal("expired entry was not removed")
	}
}
