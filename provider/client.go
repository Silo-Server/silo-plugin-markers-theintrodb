package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/time/rate"
)

const (
	maxRetries      = 3
	maxResponseBody = 1 << 20 // 1 MB
	defaultTimeout  = 15 * time.Second
	defaultCacheTTL = 24 * time.Hour
	maxCacheEntries = 1024
)

// Client is an HTTP client for the TheIntroDB /v3/media endpoint. Each
// instance has its own rate limiter and response cache; concurrent fetches
// for the same lookup key collapse to a single HTTP round trip via the cache.
type Client struct {
	httpClient *http.Client
	mu         sync.RWMutex
	apiKey     string
	baseURL    string
	limiter    *rate.Limiter
	cache      *responseCache
}

// NewClient builds a Client with the canonical rate limit and cache TTL.
// The apiKey may be empty — TheIntroDB serves read traffic without a key,
// authenticated reads also include the caller's pending submissions.
func NewClient(apiKey string) *Client {
	return &Client{
		httpClient: &http.Client{Timeout: defaultTimeout},
		apiKey:     strings.TrimSpace(apiKey),
		baseURL:    DefaultBaseURL,
		// Reads allow 30 requests / 10 seconds per IP or authenticated account.
		limiter: rate.NewLimiter(2, 5),
		cache:   newResponseCache(maxCacheEntries),
	}
}

// SetBaseURL overrides the API base URL (used by tests).
func (c *Client) SetBaseURL(u string) {
	c.mu.Lock()
	c.baseURL = u
	c.cache = newResponseCache(maxCacheEntries)
	c.mu.Unlock()
}

// SetAPIKey rotates the bearer token in-place. Safe to call concurrently
// with in-flight requests; subsequent requests use the new key and cache.
func (c *Client) SetAPIKey(apiKey string) {
	apiKey = strings.TrimSpace(apiKey)
	c.mu.Lock()
	if c.apiKey != apiKey {
		c.apiKey = apiKey
		c.cache = newResponseCache(maxCacheEntries)
	}
	c.mu.Unlock()
}

func (c *Client) invalidateCache() {
	c.mu.Lock()
	c.cache = newResponseCache(maxCacheEntries)
	c.mu.Unlock()
}

// FetchEpisode looks up segment timestamps for a TV episode.
// At least one of tmdbID, tvdbID, or imdbID must be non-empty. When several are
// present the preference order is tmdb → tvdb → imdb (matching TheIntroDB's own
// clients).
func (c *Client) FetchEpisode(ctx context.Context, tmdbID, tvdbID, imdbID string, season, episode int, durationMS int64) (*mediaResponse, error) {
	if tmdbID == "" && tvdbID == "" && imdbID == "" {
		return nil, fmt.Errorf("introdb: tmdb_id, tvdb_id, or imdb_id required")
	}
	if season <= 0 || episode <= 0 {
		return nil, fmt.Errorf("introdb: episode lookup requires season and episode > 0 (got %d/%d)", season, episode)
	}
	q := url.Values{}
	setPreferredID(q, tmdbID, tvdbID, imdbID)
	q.Set("season", strconv.Itoa(season))
	q.Set("episode", strconv.Itoa(episode))
	if durationMS > 0 {
		q.Set("duration_ms", strconv.FormatInt(durationMS, 10))
	}
	return c.fetch(ctx, q)
}

// FetchMovie looks up segment timestamps for a movie.
// At least one of tmdbID, tvdbID, or imdbID must be non-empty.
func (c *Client) FetchMovie(ctx context.Context, tmdbID, tvdbID, imdbID string, durationMS int64) (*mediaResponse, error) {
	if tmdbID == "" && tvdbID == "" && imdbID == "" {
		return nil, fmt.Errorf("introdb: tmdb_id, tvdb_id, or imdb_id required")
	}
	q := url.Values{}
	setPreferredID(q, tmdbID, tvdbID, imdbID)
	if durationMS > 0 {
		q.Set("duration_ms", strconv.FormatInt(durationMS, 10))
	}
	return c.fetch(ctx, q)
}

// setPreferredID writes exactly one id query parameter, preferring tmdb, then
// tvdb, then imdb. At least one is assumed non-empty by the callers.
func setPreferredID(q url.Values, tmdbID, tvdbID, imdbID string) {
	switch {
	case tmdbID != "":
		q.Set("tmdb_id", tmdbID)
	case tvdbID != "":
		q.Set("tvdb_id", tvdbID)
	default:
		q.Set("imdb_id", imdbID)
	}
}

func (c *Client) fetch(ctx context.Context, q url.Values) (*mediaResponse, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	c.mu.RLock()
	baseURL, apiKey, cache := c.baseURL, c.apiKey, c.cache
	c.mu.RUnlock()
	key := q.Encode()
	if cached, ok := cache.Get(key); ok {
		return cached, nil
	}

	result := cache.flights.DoChan(key, func() (any, error) {
		if cached, ok := cache.Get(key); ok {
			return cached, nil
		}
		// A canceled waiter must not cancel another caller's shared lookup.
		// Bound the shared work even when all callers have stopped waiting.
		fetchCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), defaultTimeout)
		defer cancel()
		response, err := c.fetchMedia(fetchCtx, baseURL+"/media?"+key, apiKey)
		if err == nil {
			cache.Set(key, response, defaultCacheTTL)
		}
		return response, err
	})
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case fetched := <-result:
		if fetched.Err != nil {
			return nil, fetched.Err
		}
		return fetched.Val.(*mediaResponse), nil
	}
}

func (c *Client) fetchMedia(ctx context.Context, reqURL, apiKey string) (*mediaResponse, error) {
	for attempt := 0; ; attempt++ {
		resp, err := c.do(ctx, http.MethodGet, reqURL, nil, apiKey)
		if err != nil {
			return nil, fmt.Errorf("introdb: request failed: %w", err)
		}

		if resp.StatusCode == http.StatusNotFound {
			resp.Body.Close()
			return nil, nil
		}

		if resp.StatusCode == http.StatusTooManyRequests {
			resp.Body.Close()
			return nil, rateLimitError(resp)
		}

		if resp.StatusCode >= 500 {
			resp.Body.Close()
			if attempt == maxRetries {
				return nil, fmt.Errorf("introdb: server error %d after %d retries", resp.StatusCode, maxRetries)
			}
			backoff := time.Duration(1<<attempt) * time.Second
			select {
			case <-time.After(backoff):
			case <-ctx.Done():
				return nil, ctx.Err()
			}
			continue
		}

		if resp.StatusCode >= 400 {
			body, _ := io.ReadAll(io.LimitReader(resp.Body, maxResponseBody))
			resp.Body.Close()
			return nil, fmt.Errorf("introdb: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
		}

		var out mediaResponse
		decodeErr := json.NewDecoder(io.LimitReader(resp.Body, maxResponseBody)).Decode(&out)
		resp.Body.Close()
		if decodeErr != nil {
			return nil, fmt.Errorf("introdb: decode response: %w", decodeErr)
		}
		return &out, nil
	}
}

// submitSegment contributes a single segment via POST /v3/submit. The API key
// is required (submissions are credited to that account); returns an error if
// none is configured. Submissions are not cached. On 429 the usage-limit reset
// is surfaced in the error so callers can back off.
func (c *Client) submitSegment(ctx context.Context, body submitRequest) (*submitResponse, error) {
	c.mu.RLock()
	baseURL := c.baseURL
	apiKey := c.apiKey
	c.mu.RUnlock()
	if apiKey == "" {
		return nil, fmt.Errorf("introdb: submit requires an API key")
	}

	payload, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("introdb: marshal submit: %w", err)
	}
	resp, err := c.do(ctx, http.MethodPost, baseURL+"/submit", bytes.NewReader(payload), apiKey)
	if err != nil {
		return nil, fmt.Errorf("introdb: submit request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusTooManyRequests {
		return nil, rateLimitError(resp)
	}
	if resp.StatusCode >= 400 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, maxResponseBody))
		return nil, fmt.Errorf("introdb: submit HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}

	// Submissions affect authenticated lookups immediately. Replacing the cache
	// prevents an older in-flight fetch from repopulating it with stale data.
	c.invalidateCache()
	var out submitResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxResponseBody)).Decode(&out); err != nil {
		return nil, fmt.Errorf("introdb: decode submit response: %w", err)
	}
	return &out, nil
}

// fetchUserStats validates the configured key and returns contribution stats
// via GET /v3/user/stats.
func (c *Client) fetchUserStats(ctx context.Context) (*userStatsResponse, error) {
	c.mu.RLock()
	baseURL := c.baseURL
	apiKey := c.apiKey
	c.mu.RUnlock()
	if apiKey == "" {
		return nil, fmt.Errorf("introdb: user stats require an API key")
	}
	resp, err := c.do(ctx, http.MethodGet, baseURL+"/user/stats", nil, apiKey)
	if err != nil {
		return nil, fmt.Errorf("introdb: stats request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusTooManyRequests {
		return nil, rateLimitError(resp)
	}
	if resp.StatusCode >= 400 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, maxResponseBody))
		return nil, fmt.Errorf("introdb: stats HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}

	var out userStatsResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxResponseBody)).Decode(&out); err != nil {
		return nil, fmt.Errorf("introdb: decode stats response: %w", err)
	}
	if out.Error != "" {
		return nil, fmt.Errorf("introdb: stats error: %s", out.Error)
	}
	return &out, nil
}

func (c *Client) do(ctx context.Context, method, endpoint string, body io.Reader, apiKey string) (*http.Response, error) {
	if err := c.limiter.Wait(ctx); err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "Silo-Server/markers")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	return c.httpClient.Do(req)
}

func rateLimitError(resp *http.Response) error {
	after := retryDelay(resp.Header, time.Now())
	return &RetryAfterError{RetryAfter: after, Message: fmt.Sprintf("introdb: rate or usage limited; retry after %s", after)}
}

// Daily usage headers accompany successful requests too. Only honor a reset
// for an exhausted quota, or when the response omits its remaining count.
func retryDelay(headers http.Header, now time.Time) time.Duration {
	after := retrySeconds(headers.Get("Retry-After"))
	if date, err := http.ParseTime(headers.Get("Retry-After")); err == nil {
		after = max(after, date.Sub(now))
	}
	for _, prefix := range []string{"X-UsageLimit-", "X-RateLimit-"} {
		if remaining, err := strconv.ParseInt(headers.Get(prefix+"Remaining"), 10, 64); err == nil && remaining > 0 {
			continue
		}
		after = max(after, retrySeconds(headers.Get(prefix+"Reset")))
	}
	if after <= 0 {
		return 10 * time.Second
	}
	return after
}

func retrySeconds(value string) time.Duration {
	seconds, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
	if err != nil || seconds <= 0 || seconds > int64(1<<63-1)/int64(time.Second) {
		return 0
	}
	return time.Duration(seconds) * time.Second
}
