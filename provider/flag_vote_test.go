package provider

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// The viewer's client never holds the API key: this call is made by the plugin, with
// the key the administrator configured, which is why the request shape matters.
func TestFlagVoteAddressesTheIntroDBWithTheConfiguredKey(t *testing.T) {
	var gotPath, gotAuth string
	var gotBody map[string]any

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"message":"Media flagged successfully"}`))
	}))
	t.Cleanup(srv.Close)

	client := NewClient("configured-key")
	client.SetBaseURL(srv.URL)

	seriesID, season, episode := 76475, 1, 4
	result, err := client.FlagVote(context.Background(), FlagRequest{
		TVDBID: &seriesID, Type: "tv", Season: &season, Episode: &episode,
	})
	if err != nil {
		t.Fatalf("FlagVote: %v", err)
	}

	if gotPath != "/flag-vote" {
		t.Fatalf("path = %q, want /flag-vote", gotPath)
	}
	if gotAuth != "Bearer configured-key" {
		t.Fatalf("Authorization = %q, want the configured key as a bearer token", gotAuth)
	}
	if gotBody["tvdb_id"] != float64(seriesID) || gotBody["type"] != "tv" ||
		gotBody["season"] != float64(season) || gotBody["episode"] != float64(episode) {
		t.Fatalf("body = %v", gotBody)
	}
	if result.Message != "Media flagged successfully" {
		t.Fatalf("message = %q", result.Message)
	}
}

// A repeat flag is a settled outcome and not a failure: TheIntroDB keys one vote
// per account per media, so the caller is told the segment is already known about.
// A provider-wide block is the opposite — its delay has to survive on the typed
// contract — and the two are checked here not to be confused with one another.
func TestFlagVoteMapsRepeatFlagsAndProviderBlocks(t *testing.T) {
	t.Run("a repeat flag is a settled outcome, not a failure", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusConflict)
			_, _ = w.Write([]byte(`{"message":"You have already flagged this media"}`))
		}))
		t.Cleanup(srv.Close)

		client := NewClient("configured-key")
		client.SetBaseURL(srv.URL)
		id := 76475

		_, err := client.FlagVote(context.Background(), FlagRequest{TVDBID: &id, Type: "movie"})

		var conflict *FlagConflictError
		if !errors.As(err, &conflict) {
			t.Fatalf("err = %v, want FlagConflictError", err)
		}
		if !strings.Contains(conflict.Message, "already flagged") {
			t.Fatalf("message = %q, want TheIntroDB's own wording", conflict.Message)
		}
	})

	t.Run("a provider block keeps the delay it asked for", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Retry-After", "90")
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"error":"maintenance"}`))
		}))
		t.Cleanup(srv.Close)

		client := NewClient("configured-key")
		client.SetBaseURL(srv.URL)
		id := 76475

		_, err := client.FlagVote(context.Background(), FlagRequest{TVDBID: &id, Type: "movie"})

		var retry *RetryAfterError
		if !errors.As(err, &retry) {
			t.Fatalf("err = %v, want RetryAfterError", err)
		}
		if retry.RetryAfter != 90*time.Second {
			t.Fatalf("retry after = %v, want the 90s TheIntroDB asked for", retry.RetryAfter)
		}
	})
}
