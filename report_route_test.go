package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/Silo-Server/silo-plugin-markers-introdb/provider"
	pluginv1 "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginproto/silo/plugin/v1"
)

// reportRouteFor points the report route at handler and counts every request that
// reaches it, so a refused body can be shown to stop before the round trip rather
// than after it.
func reportRouteFor(t *testing.T, handler http.HandlerFunc, apiKey string) (*httpRoutesServer, *int) {
	t.Helper()
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		handler(w, r)
	}))
	t.Cleanup(srv.Close)

	client := provider.NewClient(apiKey)
	client.SetBaseURL(srv.URL)
	return &httpRoutesServer{
		runtime: &runtimeServer{client: client, provider: provider.NewProvider(client)},
	}, &calls
}

func reportCall(body string) *pluginv1.HandleHTTPRequest {
	return &pluginv1.HandleHTTPRequest{
		Method: http.MethodPost,
		Path:   reportRoutePath,
		Body:   []byte(body),
	}
}

// This route takes input from any authenticated account, so an identifier the plugin
// would have discarded is refused here. Forwarding it would spend a round trip and come
// back as TheIntroDB's refusal instead of as the caller's mistake.
func TestReportRouteRefusesIdentifiersTheProviderWouldDiscard(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{"empty imdb id", `{"type":"movie","imdb_id":""}`, "imdb_id must be an identifier like tt0133093"},
		{"blank imdb id", `{"type":"movie","imdb_id":"   "}`, "imdb_id must be an identifier like tt0133093"},
		{"malformed imdb id", `{"type":"movie","imdb_id":"tt"}`, "imdb_id must be an identifier like tt0133093"},
		{"zero tvdb id", `{"type":"movie","tvdb_id":0}`, "tvdb_id must be an identifier TheIntroDB accepts"},
		{"negative tvdb id", `{"type":"movie","tvdb_id":-1}`, "tvdb_id must be an identifier TheIntroDB accepts"},
		{"oversized tvdb id", `{"type":"movie","tvdb_id":100000001}`, "tvdb_id must be an identifier TheIntroDB accepts"},
		{"oversized tmdb id", `{"type":"movie","tmdb_id":10000001}`, "tmdb_id must be an identifier TheIntroDB accepts"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			route, calls := reportRouteFor(t, func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusOK)
			}, "configured-key")

			resp, err := route.Handle(context.Background(), reportCall(tc.body))
			if err != nil {
				t.Fatalf("Handle: %v", err)
			}
			if got := resp.GetStatusCode(); got != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 (body %s)", got, resp.GetBody())
			}
			if !strings.Contains(string(resp.GetBody()), tc.want) {
				t.Fatalf("body = %s, want it to mention %q", resp.GetBody(), tc.want)
			}
			if *calls != 0 {
				t.Fatalf("TheIntroDB was called %d time(s); the identifier should have been refused first", *calls)
			}
		})
	}
}

// A provider-wide block belongs to the host, not to this reply: the delay has to
// survive on the typed contract so every consumer shares one cooldown instead of each
// rediscovering it from a status.
func TestReportRouteReportsProviderBlocksThroughTheTypedContract(t *testing.T) {
	route, _ := reportRouteFor(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Retry-After", "90")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"error":"maintenance"}`))
	}, "configured-key")

	resp, err := route.Handle(context.Background(), reportCall(`{"type":"movie","tvdb_id":76475}`))
	if resp != nil {
		t.Fatalf("a provider block produced an HTTP reply instead of the typed error: %+v", resp)
	}

	st, ok := status.FromError(err)
	if !ok || st.Code() != codes.ResourceExhausted {
		t.Fatalf("err = %v, want a ResourceExhausted status", err)
	}

	var delay time.Duration
	for _, detail := range st.Details() {
		if info, isRetry := detail.(*errdetails.RetryInfo); isRetry {
			delay = info.GetRetryDelay().AsDuration()
		}
	}
	if delay != 90*time.Second {
		t.Fatalf("retry delay = %v, want the 90s TheIntroDB asked for", delay)
	}
}

// An unknown path is still a 404 rather than the report handling swallowing it.
func TestReportRouteIgnoresOtherPaths(t *testing.T) {
	route, calls := reportRouteFor(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}, "configured-key")

	resp, err := route.Handle(context.Background(), &pluginv1.HandleHTTPRequest{
		Method: http.MethodPost,
		Path:   "/something-else",
		Body:   []byte(`{}`),
	})
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if resp.GetStatusCode() != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.GetStatusCode())
	}
	if *calls != 0 {
		t.Fatalf("TheIntroDB was called %d time(s) for an unknown path", *calls)
	}
}
