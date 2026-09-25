package provider

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"golang.org/x/time/rate"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func jsonResponse(status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}
}

func TestFetchSharesConcurrentWorkAcrossCancellation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		client := NewClient("")
		var hits atomic.Int32
		release := make(chan struct{})
		client.httpClient.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
			hits.Add(1)
			<-release
			return jsonResponse(http.StatusOK, `{"tmdb_id":123,"type":"movie"}`), nil
		})
		ctx, cancel := context.WithCancel(t.Context())
		canceled := make(chan error, 1)
		go func() {
			_, err := client.FetchMovie(ctx, "123", "", "", 0)
			canceled <- err
		}()
		synctest.Wait()
		const waiters = 10
		results := make(chan error, waiters)
		for range waiters {
			go func() {
				_, err := client.FetchMovie(t.Context(), "123", "", "", 0)
				results <- err
			}()
		}
		synctest.Wait()
		cancel()
		if err := <-canceled; !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled caller: %v", err)
		}
		close(release)
		for range waiters {
			if err := <-results; err != nil {
				t.Fatalf("shared caller: %v", err)
			}
		}
		if hits.Load() != 1 {
			t.Fatalf("HTTP requests = %d, want 1", hits.Load())
		}
	})
}

func TestCredentialChangeIsolatesInFlightAndCachedResponses(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		client := NewClient("old-key")
		release := make(chan struct{})
		var hits atomic.Int32
		client.httpClient.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
			hits.Add(1)
			switch req.Header.Get("Authorization") {
			case "Bearer old-key":
				<-release
				return jsonResponse(http.StatusOK, `{"tmdb_id":1}`), nil
			case "Bearer new-key":
				return jsonResponse(http.StatusOK, `{"tmdb_id":2}`), nil
			default:
				return jsonResponse(http.StatusNotFound, `{}`), nil
			}
		})
		oldDone := make(chan error, 1)
		go func() {
			_, err := client.FetchMovie(t.Context(), "123", "", "", 0)
			oldDone <- err
		}()
		synctest.Wait()
		client.SetAPIKey("new-key")
		response, err := client.FetchMovie(t.Context(), "123", "", "", 0)
		if err != nil || response == nil || response.TmdbID != 2 {
			t.Fatalf("new credential fetch = %+v, %v", response, err)
		}
		close(release)
		if err := <-oldDone; err != nil {
			t.Fatal(err)
		}
		response, err = client.FetchMovie(t.Context(), "123", "", "", 0)
		if err != nil || response == nil || response.TmdbID != 2 || hits.Load() != 2 {
			t.Fatalf("old response changed new cache: response=%+v error=%v hits=%d", response, err, hits.Load())
		}
		client.SetAPIKey("")
		if response, err := client.FetchMovie(t.Context(), "123", "", "", 0); err != nil || response != nil {
			t.Fatalf("anonymous fetch retained authenticated result: %+v, %v", response, err)
		}
		client.SetAPIKey("new-key")
		if response, err := client.FetchMovie(t.Context(), "123", "", "", 0); err != nil || response == nil {
			t.Fatalf("authenticated fetch retained anonymous negative: %+v, %v", response, err)
		}
	})
}

func TestSubmissionInvalidatesNegativeLookup(t *testing.T) {
	client := NewClient("test-key")
	var submitted atomic.Bool
	client.httpClient.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.Path == "/v3/submit" {
			submitted.Store(true)
			return jsonResponse(http.StatusOK, `{"submissions":[{"id":"123","status":"pending"}]}`), nil
		}
		if submitted.Load() {
			return jsonResponse(http.StatusOK, `{"tmdb_id":123,"intro":[{"end_ms":60000}]}`), nil
		}
		return jsonResponse(http.StatusNotFound, `{}`), nil
	})
	if result, err := client.FetchMovie(t.Context(), "123", "", "", 0); err != nil || result != nil {
		t.Fatalf("initial result = %+v, %v", result, err)
	}
	if _, err := client.submitSegment(t.Context(), submitRequest{TmdbID: 123, Type: "movie", Segment: "intro"}); err != nil {
		t.Fatal(err)
	}
	if result, err := client.FetchMovie(t.Context(), "123", "", "", 0); err != nil || result == nil || len(result.Intro) != 1 {
		t.Fatalf("result after submission = %+v, %v", result, err)
	}
}

func TestFetchReturnsQuotaResetWithoutRetry(t *testing.T) {
	client := NewClient("")
	var hits atomic.Int32
	client.httpClient.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
		hits.Add(1)
		response := jsonResponse(http.StatusTooManyRequests, `{}`)
		response.Header.Set("X-UsageLimit-Reset", "86400")
		return response, nil
	})
	_, err := client.FetchMovie(t.Context(), "123", "", "", 0)
	var quota *RetryAfterError
	if !errors.As(err, &quota) || quota.RetryAfter != 24*time.Hour || hits.Load() != 1 {
		t.Fatalf("error=%v quota=%+v HTTP requests=%d", err, quota, hits.Load())
	}
}

func TestFetchRetriesUseRateLimiter(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		client := NewClient("")
		client.limiter = rate.NewLimiter(rate.Every(5*time.Second), 1)
		var requested []time.Time
		client.httpClient.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
			requested = append(requested, time.Now())
			if len(requested) == 1 {
				return jsonResponse(http.StatusServiceUnavailable, `{}`), nil
			}
			return jsonResponse(http.StatusOK, `{"tmdb_id":123}`), nil
		})
		if _, err := client.FetchMovie(t.Context(), "123", "", "", 0); err != nil {
			t.Fatal(err)
		}
		if len(requested) != 2 || requested[1].Sub(requested[0]) < 5*time.Second {
			t.Fatalf("retry bypassed rate limiter: %v", requested)
		}
	})
}

func TestRetryDelayUsesApplicableReset(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, tt := range []struct {
		name    string
		headers map[string]string
		want    time.Duration
	}{
		{"seconds", map[string]string{"Retry-After": "12"}, 12 * time.Second},
		{"http date", map[string]string{"Retry-After": now.Add(time.Minute).Format(http.TimeFormat)}, time.Minute},
		{"usage reset", map[string]string{"X-UsageLimit-Reset": "86400", "X-UsageLimit-Remaining": "0"}, 24 * time.Hour},
		{"rate reset with remaining daily quota", map[string]string{"X-UsageLimit-Reset": "86400", "X-UsageLimit-Remaining": "400", "X-RateLimit-Reset": "7"}, 7 * time.Second},
		{"all exhausted limits", map[string]string{"Retry-After": "1", "X-UsageLimit-Reset": "86400", "X-UsageLimit-Remaining": "0", "X-RateLimit-Reset": "7"}, 24 * time.Hour},
		{"invalid hints", map[string]string{"Retry-After": "garbage", "X-UsageLimit-Reset": "9223372036854775807", "X-RateLimit-Reset": "-2"}, 10 * time.Second},
	} {
		t.Run(tt.name, func(t *testing.T) {
			headers := make(http.Header)
			for key, value := range tt.headers {
				headers.Set(key, value)
			}
			if got := retryDelay(headers, now); got != tt.want {
				t.Fatalf("delay=%v, want %v", got, tt.want)
			}
		})
	}
}
