package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/structpb"

	"github.com/Silo-Server/silo-plugin-markers-introdb/provider"
	pluginv1 "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginproto/silo/plugin/v1"
)

func testMarkerServer(t *testing.T, handler http.HandlerFunc, apiKey string) *markerServer {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	client := provider.NewClient(apiKey)
	client.SetBaseURL(srv.URL)
	return &markerServer{runtime: &runtimeServer{client: client, provider: provider.NewProvider(client)}}
}

func TestMarkerServerFetchMarkersMapsSegments(t *testing.T) {
	server := testMarkerServer(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{
			"tvdb_id":123,"type":"tv","season":1,"episode":2,
			"intro":[{"start_ms":null,"end_ms":60000},{"start_ms":210000,"end_ms":250000}],
			"credits":[{"start_ms":1450000,"end_ms":1500000},{"start_ms":1570000,"end_ms":null}],
			"recap":[{"start_ms":null,"end_ms":45000}],
			"preview":[{"start_ms":1350000,"end_ms":1400000}]
		}`))
	}, "")

	resp, err := server.FetchMarkers(context.Background(), &pluginv1.FetchMarkersRequest{
		ItemType: "episode",
		ExternalIds: &pluginv1.MarkerExternalIDs{
			TmdbId: "123",
		},
		SeasonNumber:    1,
		EpisodeNumber:   2,
		DurationSeconds: 1800,
	})
	if err != nil {
		t.Fatalf("FetchMarkers: %v", err)
	}
	want := []struct {
		segment string
		start   float64
		end     float64
	}{
		{"intro", 0, 60},
		{"intro", 210, 250},
		{"credits", 1450, 1500},
		{"credits", 1570, 1800},
		{"recap", 0, 45},
		{"preview", 1350, 1400},
	}
	if len(resp.GetMarkers()) != len(want) {
		t.Fatalf("markers = %d, want %d", len(resp.GetMarkers()), len(want))
	}
	for i, expected := range want {
		got := resp.GetMarkers()[i]
		if got.GetSegment() != expected.segment || got.StartSeconds == nil || got.EndSeconds == nil || got.GetStartSeconds() != expected.start || got.GetEndSeconds() != expected.end {
			t.Errorf("marker %d = %+v, want %s %v–%v", i, got, expected.segment, expected.start, expected.end)
		}
		if got.GetConfidence() != 0.9 || got.GetSubmissionCount() != 0 || got.GetAlgorithm() != provider.Algorithm {
			t.Errorf("marker %d metadata = %+v, want default confidence, no submission count, and %q", i, got, provider.Algorithm)
		}
	}
}

func TestMarkerServerFetchMarkersWithEmptyArrays(t *testing.T) {
	server := testMarkerServer(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"tvdb_id":123,"type":"movie","intro":null,"credits":[],"recap":null}`))
	}, "")
	resp, err := server.FetchMarkers(context.Background(), &pluginv1.FetchMarkersRequest{
		ItemType:        "movie",
		ExternalIds:     &pluginv1.MarkerExternalIDs{TmdbId: "123"},
		DurationSeconds: 1800,
	})
	if err != nil {
		t.Fatalf("FetchMarkers: %v", err)
	}
	if len(resp.GetMarkers()) != 0 {
		t.Fatalf("markers = %+v, want none", resp.GetMarkers())
	}
}

func TestRuntimeConfigureSetsAPIKeyForStats(t *testing.T) {
	var gotAuth string
	server := testMarkerServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		_, _ = w.Write([]byte(`{"total":1}`))
	}, "")
	account, err := structpb.NewStruct(map[string]any{"api_key": "secret-key"})
	if err != nil {
		t.Fatalf("NewStruct: %v", err)
	}
	if _, err := server.runtime.Configure(context.Background(), &pluginv1.ConfigureRequest{
		Config: []*pluginv1.ConfigEntry{{Key: "account", Value: account}},
	}); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	if _, err := server.GetMarkerProviderStats(context.Background(), &pluginv1.GetMarkerProviderStatsRequest{}); err != nil {
		t.Fatalf("GetMarkerProviderStats: %v", err)
	}
	if gotAuth != "Bearer secret-key" {
		t.Fatalf("Authorization = %q, want Bearer secret-key", gotAuth)
	}
}

func TestMarkerServerSubmitRateLimitMapsRetryInfo(t *testing.T) {
	server := testMarkerServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-UsageLimit-Reset", "90")
		w.WriteHeader(http.StatusTooManyRequests)
	}, "secret-key")

	_, err := server.SubmitMarker(context.Background(), &pluginv1.SubmitMarkerRequest{
		ItemType:        "episode",
		ExternalIds:     &pluginv1.MarkerExternalIDs{TmdbId: "123"},
		SeasonNumber:    1,
		EpisodeNumber:   2,
		Segment:         "intro",
		DurationSeconds: 1800,
		EndSeconds:      floatPtr(60),
	})
	if err == nil {
		t.Fatal("expected rate limit error")
	}
	st, ok := status.FromError(err)
	if !ok || st.Code() != codes.ResourceExhausted {
		t.Fatalf("status = %v/%v, want ResourceExhausted", st.Code(), err)
	}
	for _, detail := range st.Details() {
		if retry, ok := detail.(*errdetails.RetryInfo); ok {
			if retry.GetRetryDelay().AsDuration().Seconds() != 90 {
				t.Fatalf("retry delay = %v, want 90s", retry.GetRetryDelay().AsDuration())
			}
			return
		}
	}
	t.Fatal("missing RetryInfo detail")
}

func floatPtr(v float64) *float64 { return &v }

func TestMarkerServerPassesEpisodeIdentityAndDetails(t *testing.T) {
	var gotQuery url.Values
	server := testMarkerServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.Query()
		_, _ = w.Write([]byte(`{"tvdb_id":121361,"type":"tv","season":1,"episode":1,
			"intro":[{"start_ms":436000,"end_ms":535000,"confidence":0.67,"submission_count":5}]}`))
	}, "")

	resp, err := server.FetchMarkers(context.Background(), &pluginv1.FetchMarkersRequest{
		ItemType:           "episode",
		ExternalIds:        &pluginv1.MarkerExternalIDs{TmdbId: "1399", TvdbId: "121361"},
		SeasonNumber:       1,
		EpisodeNumber:      1,
		DurationSeconds:    3695,
		EpisodeExternalIds: &pluginv1.MarkerExternalIDs{TvdbId: "3254641", TmdbId: "63056"},
		EpisodeNumbering:   "tvdb",
		EpisodeTitle:       "Winter Is Coming",
	})
	if err != nil {
		t.Fatalf("FetchMarkers: %v", err)
	}
	if gotQuery.Get("tvdb_id") != "121361" || gotQuery.Get("tmdb_id") != "" ||
		gotQuery.Get("tvdb_episode_id") != "3254641" || gotQuery.Has("episode_name") {
		t.Fatalf("query = %s, want the TVDB-numbered episode addressed by TVDB", gotQuery.Encode())
	}
	if len(resp.GetMarkers()) != 1 {
		t.Fatalf("markers = %+v", resp.GetMarkers())
	}
	if m := resp.GetMarkers()[0]; m.GetConfidence() != 0.67 || m.GetSubmissionCount() != 5 || m.GetAlgorithm() != provider.Algorithm {
		t.Fatalf("marker = %+v, want TheIntroDB's confidence and submission count", m)
	}
}

func TestProviderErrorMarksUnsubmittableAsInvalidArgument(t *testing.T) {
	err := providerError(fmt.Errorf("%w: submit requires a TVDB, TMDB, or IMDb id", provider.ErrUnsubmittable))
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("code = %v, want InvalidArgument so the host records a permanent refusal", status.Code(err))
	}
	if status.Code(providerError(fmt.Errorf("introdb: submit HTTP 500: unavailable"))) == codes.InvalidArgument {
		t.Fatal("a server error must stay retryable")
	}
}
