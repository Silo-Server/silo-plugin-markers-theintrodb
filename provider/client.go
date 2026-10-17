package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"mime"
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
	clientUserAgent = "Silo-Server-TheIntroDB-Plugin/1.0 (+https://github.com/Silo-Server/silo-plugin-markers-theintrodb)"
	// Missing and partial responses are deliberately short-lived. A later
	// playback should be able to discover newly contributed intro or credits
	// markers without hammering TheIntroDB on repeated starts.
	defaultIncompleteCacheTTL = 15 * time.Minute
	// A Cloudflare block is not content-specific. Further lookups only add
	// rejected traffic and can prolong an automated block, so the host pauses
	// the provider for this long.
	defaultBlockedCooldown = 5 * time.Minute
)

// TheIntroDB meters each request against a 10-second burst window and reports
// the lookup ceiling in X-RateLimit-Limit: 30 anonymously and, per its docs,
// 120 with an API key. The lookup limiter starts below the anonymous ceiling
// and then follows the reported one, so a key TheIntroDB does not accept
// never raises the rate. Submissions allow 40 per window.
const (
	rateLimitWindow                           = 10 * time.Second
	anonymousLookupRate, anonymousLookupBurst = rate.Limit(2), 5
	writeRate, writeBurst                     = rate.Limit(3), 3
)

const (
	// rejectedKeyBackoff is how long submissions with a key TheIntroDB
	// refused are skipped without a request.
	rejectedKeyBackoff = time.Hour
	// exhaustedUsageBackoff stands in for the daily reset, which TheIntroDB
	// reports as 0, once the daily budget is spent.
	exhaustedUsageBackoff = time.Hour
	// maxRetryDelay bounds a server-supplied delay to just over a day.
	maxRetryDelay = 25 * time.Hour
	// maxErrorReason bounds the response text kept in an error, which Silo
	// stores per item.
	maxErrorReason = 512
)

// Client is an HTTP client for the TheIntroDB v4 API. Each instance has its
// own rate limiters and response cache; concurrent fetches for the same lookup
// key collapse to a single HTTP round trip via the cache.
type Client struct {
	httpClient    *http.Client
	mu            sync.RWMutex
	apiKey        string
	rejectedKey   string
	rejectedUntil time.Time
	baseURL       string
	lookupLimiter *rate.Limiter
	writeLimiter  *rate.Limiter
	cache         *responseCache
}

// NewClient builds a Client with the canonical rate limits and cache TTL.
// The apiKey may be empty — TheIntroDB serves read traffic without a key;
// authenticated reads also include the caller's pending submissions and get
// a larger budget.
func NewClient(apiKey string) *Client {
	c := &Client{
		httpClient:    &http.Client{Timeout: defaultTimeout},
		baseURL:       DefaultBaseURL,
		lookupLimiter: rate.NewLimiter(anonymousLookupRate, anonymousLookupBurst),
		writeLimiter:  rate.NewLimiter(writeRate, writeBurst),
		cache:         newResponseCache(maxCacheEntries),
	}
	c.SetAPIKey(apiKey)
	return c
}

// SetBaseURL overrides the API base URL (used by tests).
func (c *Client) SetBaseURL(u string) {
	c.mu.Lock()
	c.baseURL = u
	c.cache = newResponseCache(maxCacheEntries)
	c.mu.Unlock()
}

// SetAPIKey rotates the bearer token in-place. Safe to call concurrently
// with in-flight requests; subsequent requests use the new key and cache, and
// the lookup rate starts over until responses report the new key's ceiling.
func (c *Client) SetAPIKey(apiKey string) {
	apiKey = strings.TrimSpace(apiKey)
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.apiKey == apiKey {
		return
	}
	c.apiKey = apiKey
	c.rejectedKey, c.rejectedUntil = "", time.Time{}
	c.cache = newResponseCache(maxCacheEntries)
	c.lookupLimiter.SetLimit(anonymousLookupRate)
	c.lookupLimiter.SetBurst(anonymousLookupBurst)
}

// followRateLimit sizes the lookup limiter to the burst ceiling a lookup
// response reports, keeping a fifth of it in reserve.
func (c *Client) followRateLimit(headers http.Header) {
	ceiling, err := strconv.Atoi(strings.TrimSpace(headers.Get("X-RateLimit-Limit")))
	if err != nil || ceiling <= 0 {
		return
	}
	limit := rate.Limit(float64(ceiling*4) / (5 * rateLimitWindow.Seconds()))
	burst := max(1, ceiling/6)
	if c.lookupLimiter.Limit() != limit {
		c.lookupLimiter.SetLimit(limit)
	}
	if c.lookupLimiter.Burst() != burst {
		c.lookupLimiter.SetBurst(burst)
	}
}

// rejectKey remembers that TheIntroDB refused apiKey, unless it has since
// been replaced. Any key change clears it.
func (c *Client) rejectKey(apiKey string) {
	c.mu.Lock()
	if c.apiKey == apiKey {
		c.rejectedKey, c.rejectedUntil = apiKey, time.Now().Add(rejectedKeyBackoff)
	}
	c.mu.Unlock()
}

func (c *Client) acceptKey(apiKey string) {
	c.mu.Lock()
	if c.rejectedKey == apiKey {
		c.rejectedKey, c.rejectedUntil = "", time.Time{}
	}
	c.mu.Unlock()
}

func (c *Client) keyRejected(apiKey string) bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.rejectedKey == apiKey && time.Now().Before(c.rejectedUntil)
}

func (c *Client) invalidateCache() {
	c.mu.Lock()
	c.cache = newResponseCache(maxCacheEntries)
	c.mu.Unlock()
}

// FetchEpisode looks up segment timestamps for a TV episode addressed by
// target (see NewTarget). Season 0 holds specials.
func (c *Client) FetchEpisode(ctx context.Context, target Target, season, episode int, durationMS int64) (*mediaResponse, error) {
	if target.ID == "" {
		return nil, fmt.Errorf("introdb: tvdb_id, tmdb_id, or imdb_id required")
	}
	if !validCoordinates(season, episode) {
		return nil, fmt.Errorf("introdb: episode lookup requires season 0-%d and episode 1-%d (got %d/%d)", maxSeason, maxEpisode, season, episode)
	}
	q := url.Values{}
	target.addTo(q)
	q.Set("season", strconv.Itoa(season))
	q.Set("episode", strconv.Itoa(episode))
	if durationMS > 0 {
		q.Set("duration_ms", strconv.FormatInt(durationMS, 10))
	}
	return c.fetch(ctx, q)
}

// FetchMovie looks up segment timestamps for a movie addressed by target.
func (c *Client) FetchMovie(ctx context.Context, target Target, durationMS int64) (*mediaResponse, error) {
	if target.ID == "" {
		return nil, fmt.Errorf("introdb: tvdb_id, tmdb_id, or imdb_id required")
	}
	q := url.Values{}
	target.addTo(q)
	if durationMS > 0 {
		q.Set("duration_ms", strconv.FormatInt(durationMS, 10))
	}
	return c.fetch(ctx, q)
}

func (c *Client) fetch(ctx context.Context, q url.Values) (*mediaResponse, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	c.mu.RLock()
	baseURL, apiKey, cache := c.baseURL, c.apiKey, c.cache
	c.mu.RUnlock()
	// details adds TheIntroDB's per-segment confidence and submission count.
	q.Set("details", "true")
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
			cache.Set(key, response, responseCacheTTL(response))
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
		resp, err := c.do(ctx, c.lookupLimiter, http.MethodGet, reqURL, nil, apiKey)
		if err != nil {
			return nil, fmt.Errorf("introdb: request failed: %w", err)
		}
		c.followRateLimit(resp.Header)

		// TheIntroDB answers a miss with a JSON 404. Any other 404 means the
		// endpoint itself is gone, which must not read as "no markers".
		if resp.StatusCode == http.StatusNotFound {
			isJSON := isJSONResponse(resp)
			closeResponse(resp)
			if !isJSON {
				return nil, fmt.Errorf("introdb: lookup endpoint not found (HTTP 404)")
			}
			return nil, nil
		}

		if resp.StatusCode == http.StatusTooManyRequests {
			return nil, rateLimitError(resp)
		}

		if resp.StatusCode >= 500 {
			closeResponse(resp)
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

		if resp.StatusCode == http.StatusForbidden && !isJSONResponse(resp) && isCloudflareResponse(resp) {
			closeResponse(resp)
			return nil, cloudflareBlockError(resp)
		}

		if resp.StatusCode >= 400 {
			return nil, fmt.Errorf("introdb: HTTP %d: %s", resp.StatusCode, errorReason(resp))
		}

		var out mediaResponse
		decodeErr := json.NewDecoder(io.LimitReader(resp.Body, maxResponseBody)).Decode(&out)
		closeResponse(resp)
		if decodeErr != nil {
			return nil, fmt.Errorf("introdb: decode response: %w", decodeErr)
		}
		return &out, nil
	}
}

// Every API response passes through Cloudflare, so the Server header cannot
// distinguish a block from an origin error. The origin reports errors as JSON;
// a Cloudflare block is an HTML page.
func isJSONResponse(resp *http.Response) bool {
	// A malformed parameter (e.g. a bare "; charset") still yields the media
	// type alongside ErrInvalidMediaParameter, so only an empty type is fatal.
	mediaType, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if mediaType == "" {
		return false
	}
	return strings.EqualFold(mediaType, "application/json") || strings.HasSuffix(strings.ToLower(mediaType), "+json")
}

// isCloudflareResponse reports whether the response passed through Cloudflare.
// Without that provenance a non-JSON 403 is an ordinary error and keeps its
// body instead of pausing the provider.
func isCloudflareResponse(resp *http.Response) bool {
	return strings.TrimSpace(resp.Header.Get("CF-Ray")) != "" ||
		strings.Contains(strings.ToLower(resp.Header.Get("Server")), "cloudflare")
}

// cloudflareBlockError asks the host to pause the provider instead of letting
// every queued lookup reach the block. The HTML block page is dropped; the
// CF-Ray value is enough to diagnose it.
func cloudflareBlockError(resp *http.Response) error {
	message := fmt.Sprintf("introdb: Cloudflare blocked request (HTTP %d); retry after %s", resp.StatusCode, defaultBlockedCooldown)
	if ray := strings.TrimSpace(resp.Header.Get("CF-Ray")); ray != "" {
		message = fmt.Sprintf("introdb: Cloudflare blocked request (HTTP %d, cf-ray %s); retry after %s", resp.StatusCode, ray, defaultBlockedCooldown)
	}
	return &RetryAfterError{RetryAfter: defaultBlockedCooldown, Message: message}
}

// responseCacheTTL keeps a response for the full TTL only when it has a
// usable intro and credits. TheIntroDB marks a missing credits segment with a
// zero-start entry that convertMarkers discards, so counting array entries
// would cache that miss for a day. The file duration isn't known here, so an
// open-ended segment counts as usable.
func responseCacheTTL(response *mediaResponse) time.Duration {
	unbounded := time.Duration(math.MaxInt64)
	if response == nil ||
		len(convertMarkers(response.Intro, MarkerKindIntro, unbounded)) == 0 ||
		len(convertMarkers(response.Credits, MarkerKindCredits, unbounded)) == 0 {
		return defaultIncompleteCacheTTL
	}
	return defaultCacheTTL
}

// closeResponse releases a response body that the caller is not returning. The
// status branches report the upstream status rather than a read failure, so the
// close error has nowhere to go; draining a bounded amount first keeps the
// connection reusable instead of forcing the transport to drop it.
func closeResponse(resp *http.Response) {
	if resp == nil || resp.Body == nil {
		return
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxResponseBody))
	_ = resp.Body.Close()
}

// submitSegment contributes a single segment via POST /v4/submit. The API key
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
	// Each candidate would draw the same refusal, so skip the request until
	// the key changes or the backoff passes.
	if c.keyRejected(apiKey) {
		return nil, fmt.Errorf("introdb: submit skipped: TheIntroDB rejected this API key; use a key from theintrodb.org")
	}

	payload, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("introdb: marshal submit: %w", err)
	}
	resp, err := c.do(ctx, c.writeLimiter, http.MethodPost, baseURL+"/submit", bytes.NewReader(payload), apiKey)
	if err != nil {
		return nil, fmt.Errorf("introdb: submit request failed: %w", err)
	}
	defer closeResponse(resp)

	if resp.StatusCode >= 400 {
		return nil, c.accountError("submit", resp, apiKey)
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
// via GET /v4/user/stats.
func (c *Client) fetchUserStats(ctx context.Context) (*userStatsResponse, error) {
	c.mu.RLock()
	baseURL := c.baseURL
	apiKey := c.apiKey
	c.mu.RUnlock()
	if apiKey == "" {
		return nil, fmt.Errorf("introdb: user stats require an API key")
	}
	resp, err := c.do(ctx, c.writeLimiter, http.MethodGet, baseURL+"/user/stats", nil, apiKey)
	if err != nil {
		return nil, fmt.Errorf("introdb: stats request failed: %w", err)
	}
	defer closeResponse(resp)

	if resp.StatusCode >= 400 {
		return nil, c.accountError("stats", resp, apiKey)
	}

	var out userStatsResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxResponseBody)).Decode(&out); err != nil {
		return nil, fmt.Errorf("introdb: decode stats response: %w", err)
	}
	if out.Error != "" {
		return nil, fmt.Errorf("introdb: stats error: %s", out.Error)
	}
	c.acceptKey(apiKey)
	return &out, nil
}

// accountError reads and closes a failed response from an endpoint that needs
// the API key. Its text keeps the "introdb: <op> HTTP <status>: <reason>" form
// Silo reads to tell refusals (400, 409, 422) from failures it retries.
func (c *Client) accountError(op string, resp *http.Response, apiKey string) error {
	if resp.StatusCode == http.StatusTooManyRequests {
		return rateLimitError(resp)
	}
	isJSON := isJSONResponse(resp)
	if !isJSON && isCloudflareResponse(resp) && (resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusServiceUnavailable) {
		closeResponse(resp)
		return cloudflareBlockError(resp)
	}
	status := resp.StatusCode
	reason := errorReason(resp)
	switch {
	case status == http.StatusNotFound:
		// v4 answers 404 only when the route itself is gone, which says
		// nothing about the item.
		return fmt.Errorf("introdb: %s endpoint not found (HTTP 404)", op)
	case status == http.StatusServiceUnavailable:
		// TheIntroDB could not check the account and asks to retry later.
		after := min(retrySeconds(resp.Header.Get("Retry-After")), maxRetryDelay)
		if after <= 0 {
			after = defaultBlockedCooldown
		}
		return &RetryAfterError{RetryAfter: after, Message: fmt.Sprintf("introdb: %s HTTP 503: %s; retry after %s", op, reason, after)}
	case isJSON && (status == http.StatusUnauthorized || status == http.StatusForbidden):
		c.rejectKey(apiKey)
	}
	return fmt.Errorf("introdb: %s HTTP %d: %s", op, status, reason)
}

// errorReason reads and closes an error response, returning TheIntroDB's JSON
// error message or a bounded excerpt of any other body.
func errorReason(resp *http.Response) string {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxResponseBody))
	closeResponse(resp)
	var parsed struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(body, &parsed) == nil && strings.TrimSpace(parsed.Error) != "" {
		body = []byte(parsed.Error)
	}
	reason := strings.TrimSpace(string(body))
	if len(reason) > maxErrorReason {
		reason = strings.ToValidUTF8(reason[:maxErrorReason], "") + "…"
	}
	return reason
}

func (c *Client) do(ctx context.Context, limiter *rate.Limiter, method, endpoint string, body io.Reader, apiKey string) (*http.Response, error) {
	if err := limiter.Wait(ctx); err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", clientUserAgent)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	return c.httpClient.Do(req)
}

// rateLimitError reads and closes a 429 response. Most limits are per account
// or IP, so the host pauses the provider. TheIntroDB also caps lookups of a
// single title per day; that limit leaves every other title available, so it
// is reported as an ordinary failure the host retries for this item alone.
func rateLimitError(resp *http.Response) error {
	var body struct {
		Code string `json:"code"`
	}
	_ = json.NewDecoder(io.LimitReader(resp.Body, maxResponseBody)).Decode(&body)
	closeResponse(resp)
	after := retryDelay(resp.Header, time.Now())
	if body.Code == "specific_media_usage_limit_exceeded" {
		return fmt.Errorf("introdb: daily lookup limit for this title reached; retry after %s", after)
	}
	return &RetryAfterError{RetryAfter: after, Message: fmt.Sprintf("introdb: rate or usage limited; retry after %s", after)}
}

// Daily usage headers accompany successful requests too. Only honor a reset
// for an exhausted quota, or when the response omits its remaining count.
// TheIntroDB reports the daily reset as 0, so a spent daily budget without a
// usable reset or Retry-After waits exhaustedUsageBackoff.
func retryDelay(headers http.Header, now time.Time) time.Duration {
	after := retrySeconds(headers.Get("Retry-After"))
	if date, err := http.ParseTime(headers.Get("Retry-After")); err == nil {
		after = max(after, date.Sub(now))
	}
	for _, prefix := range []string{"X-UsageLimit-", "X-RateLimit-"} {
		remaining, err := strconv.ParseInt(headers.Get(prefix+"Remaining"), 10, 64)
		if err == nil && remaining > 0 {
			continue
		}
		reset := retrySeconds(headers.Get(prefix + "Reset"))
		if reset <= 0 && prefix == "X-UsageLimit-" && err == nil && remaining == 0 {
			reset = exhaustedUsageBackoff
		}
		after = max(after, reset)
	}
	if after <= 0 {
		return 10 * time.Second
	}
	return min(after, maxRetryDelay)
}

func retrySeconds(value string) time.Duration {
	seconds, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
	if err != nil || seconds <= 0 || seconds > int64(1<<63-1)/int64(time.Second) {
		return 0
	}
	return time.Duration(seconds) * time.Second
}
