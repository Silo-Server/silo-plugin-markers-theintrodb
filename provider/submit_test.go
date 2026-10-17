package provider

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func ptrDur(d time.Duration) *time.Duration { return &d }

// submitCapture serves POST /submit and records each decoded body.
func submitCapture(t *testing.T) (*Provider, *[]submitRequest, *[]map[string]any) {
	t.Helper()
	var bodies []submitRequest
	var raws []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/submit" || r.Header.Get("Authorization") != "Bearer secret-key" {
			t.Errorf("request %s %s auth %q", r.Method, r.URL.Path, r.Header.Get("Authorization"))
		}
		b, _ := io.ReadAll(r.Body)
		var body submitRequest
		var raw map[string]any
		_ = json.Unmarshal(b, &body)
		_ = json.Unmarshal(b, &raw)
		bodies = append(bodies, body)
		raws = append(raws, raw)
		_, _ = w.Write([]byte(`{"ok":true,"submissions":[{"id":"abc","tvdbId":121361,"status":"pending","weight":1.5}]}`))
	}))
	t.Cleanup(srv.Close)
	c := NewClient("secret-key")
	c.SetBaseURL(srv.URL)
	return NewProvider(c), &bodies, &raws
}

func TestSubmitMarkerSendsExpectedBody(t *testing.T) {
	p, bodies, raws := submitCapture(t)
	res, err := p.SubmitMarker(context.Background(), SubmissionRequest{
		Kind:          ItemKindEpisode,
		ExternalIDs:   map[string]string{ExternalIDKeyTMDB: "1234", ExternalIDKeyIMDB: "tt0944947"},
		SeasonNumber:  1,
		EpisodeNumber: 2,
		EpisodeTitle:  "Ignored For TMDB",
		Segment:       MarkerKindIntro,
		Start:         ptrDur(0),
		End:           ptrDur(60 * time.Second),
		Duration:      30 * time.Minute,
	})
	if err != nil {
		t.Fatalf("SubmitMarker: %v", err)
	}
	if res.ID != "abc" || res.Status != SubmissionStatusPending || res.Weight != 1.5 {
		t.Errorf("result = %+v, want id abc status pending weight 1.5", res)
	}
	gotBody := (*bodies)[0]
	if gotBody.TmdbID == nil || *gotBody.TmdbID != 1234 || gotBody.Type != "tv" || gotBody.Segment != "intro" {
		t.Errorf("body = %+v, want tmdb 1234 type tv segment intro", gotBody)
	}
	// One ID per request: a second one would change the coordinate space
	// TheIntroDB reads the season and episode in.
	for _, key := range []string{"tvdb_id", "imdb_id", "tvdb_episode_id", "episode_name", "mapping"} {
		if _, ok := (*raws)[0][key]; ok {
			t.Errorf("body sent %s: %v", key, (*raws)[0])
		}
	}
	if gotBody.StartMs != nil {
		t.Errorf("intro start_ms = %v, want null (zero start dropped)", *gotBody.StartMs)
	}
	if v, ok := (*raws)[0]["start_ms"]; !ok || v != nil {
		t.Errorf("start_ms = %v (present %v), want explicit null", v, ok)
	}
	if gotBody.EndMs == nil || *gotBody.EndMs != 60000 {
		t.Errorf("end_ms = %v, want 60000", gotBody.EndMs)
	}
	if gotBody.Season == nil || *gotBody.Season != 1 || gotBody.Episode == nil || *gotBody.Episode != 2 {
		t.Errorf("season/episode = %v/%v, want 1/2", gotBody.Season, gotBody.Episode)
	}
	if gotBody.VideoDurationMs == nil || *gotBody.VideoDurationMs != 1_800_000 {
		t.Errorf("video_duration_ms = %v, want 1800000", gotBody.VideoDurationMs)
	}
}

func TestSubmitMarkerAddressesTVDBNumberedEpisodeByTVDB(t *testing.T) {
	p, bodies, raws := submitCapture(t)
	_, err := p.SubmitMarker(context.Background(), SubmissionRequest{
		Kind:               ItemKindEpisode,
		ExternalIDs:        map[string]string{ExternalIDKeyTMDB: "1399", ExternalIDKeyTVDB: "121361"},
		SeasonNumber:       0,
		EpisodeNumber:      3,
		EpisodeExternalIDs: map[string]string{ExternalIDKeyTVDB: "4271129"},
		EpisodeTitle:       " A Special ",
		Segment:            MarkerKindCredits,
		Start:              ptrDur(40 * time.Minute),
		End:                ptrDur(50 * time.Minute),
		Duration:           50 * time.Minute,
	})
	if err != nil {
		t.Fatalf("SubmitMarker: %v", err)
	}
	gotBody := (*bodies)[0]
	if gotBody.TvdbID == nil || *gotBody.TvdbID != 121361 || gotBody.TmdbID != nil {
		t.Errorf("identity = tvdb %v tmdb %v, want TVDB 121361 only", gotBody.TvdbID, gotBody.TmdbID)
	}
	if gotBody.TvdbEpisodeID == nil || *gotBody.TvdbEpisodeID != 4271129 || gotBody.EpisodeName != "A Special" {
		t.Errorf("episode hints = %v %q", gotBody.TvdbEpisodeID, gotBody.EpisodeName)
	}
	if gotBody.Season == nil || *gotBody.Season != 0 || gotBody.Episode == nil || *gotBody.Episode != 3 {
		t.Errorf("season/episode = %v/%v, want the special 0/3", gotBody.Season, gotBody.Episode)
	}
	if v, ok := (*raws)[0]["end_ms"]; !ok || v != nil {
		t.Errorf("credits end_ms = %v (present %v), want null for an end at the file end", v, ok)
	}
}

func TestSubmitMarkerMovieOmitsEpisodeFields(t *testing.T) {
	p, bodies, raws := submitCapture(t)
	_, err := p.SubmitMarker(context.Background(), SubmissionRequest{
		Kind:               ItemKindMovie,
		ExternalIDs:        map[string]string{ExternalIDKeyTMDB: "603", ExternalIDKeyTVDB: "169"},
		EpisodeExternalIDs: map[string]string{ExternalIDKeyTVDB: "1"},
		EpisodeTitle:       "not an episode",
		Segment:            MarkerKindIntro,
		End:                ptrDur(90 * time.Second),
		Duration:           4 * time.Minute,
	})
	if err != nil {
		t.Fatalf("SubmitMarker: %v", err)
	}
	if gotBody := (*bodies)[0]; gotBody.Type != "movie" || gotBody.TvdbID == nil || *gotBody.TvdbID != 169 || gotBody.TmdbID != nil {
		t.Errorf("movie body = %+v, want type movie addressed by TVDB 169", gotBody)
	}
	// A movie must not carry episode fields, and TheIntroDB refuses a video
	// duration under five minutes.
	for _, key := range []string{"season", "episode", "tvdb_episode_id", "episode_name", "video_duration_ms"} {
		if _, ok := (*raws)[0][key]; ok {
			t.Errorf("movie body sent %s: %v", key, (*raws)[0])
		}
	}
}

func TestSubmitMarkerRequiresUsableID(t *testing.T) {
	p, bodies, _ := submitCapture(t)
	_, err := p.SubmitMarker(context.Background(), SubmissionRequest{
		Kind:        ItemKindMovie,
		ExternalIDs: map[string]string{ExternalIDKeyIMDB: "tt1", ExternalIDKeyTMDB: "not-a-number"},
		Segment:     MarkerKindIntro,
		End:         ptrDur(60 * time.Second),
	})
	if !errors.Is(err, ErrUnsubmittable) || len(*bodies) != 0 {
		t.Fatalf("err = %v, requests = %d; want a permanent local refusal without an API call", err, len(*bodies))
	}
	if _, err := p.SubmitMarker(context.Background(), SubmissionRequest{
		Kind:        ItemKindMovie,
		ExternalIDs: map[string]string{ExternalIDKeyIMDB: "tt0111161"},
		Segment:     MarkerKindIntro,
		End:         ptrDur(60 * time.Second),
	}); err != nil || (*bodies)[0].ImdbID != "tt0111161" {
		t.Fatalf("IMDb-only submit: err = %v body = %+v", err, *bodies)
	}
}

func TestSubmitMarkerRequiresKey(t *testing.T) {
	p := NewProvider(NewClient("")) // no key
	_, err := p.SubmitMarker(context.Background(), SubmissionRequest{
		Kind:          ItemKindEpisode,
		ExternalIDs:   map[string]string{ExternalIDKeyTMDB: "1"},
		SeasonNumber:  1,
		EpisodeNumber: 1,
		Segment:       MarkerKindIntro,
		End:           ptrDur(60 * time.Second),
	})
	if err == nil {
		t.Fatal("expected error when API key missing")
	}
}

func TestFetchUserStatsParses(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"total":10,"accepted":7,"pending":2,"rejected":1,"acceptance_rate":0.7,"current_streak":3,"best_streak":5}`))
	}))
	defer srv.Close()

	c := NewClient("secret-key")
	c.SetBaseURL(srv.URL)
	p := NewProvider(c)
	stats, err := p.FetchUserStats(context.Background())
	if err != nil {
		t.Fatalf("FetchUserStats: %v", err)
	}
	if stats.Total != 10 || stats.Accepted != 7 || stats.AcceptanceRate != 0.7 || stats.BestStreak != 5 {
		t.Errorf("stats = %+v", stats)
	}
}

func TestSubmitMarkerReturnsRetryAfterOnUsageLimit(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-UsageLimit-Reset", "120")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()

	c := NewClient("secret-key")
	c.SetBaseURL(srv.URL)
	p := NewProvider(c)

	_, err := p.SubmitMarker(context.Background(), SubmissionRequest{
		Kind:          ItemKindEpisode,
		ExternalIDs:   map[string]string{ExternalIDKeyTMDB: "1234"},
		SeasonNumber:  1,
		EpisodeNumber: 2,
		Segment:       MarkerKindIntro,
		End:           ptrDur(60 * time.Second),
		Duration:      30 * time.Minute,
	})
	if err == nil {
		t.Fatal("expected usage-limit error")
	}
	var retryErr *RetryAfterError
	if !errors.As(err, &retryErr) {
		t.Fatalf("error = %T %v, want RetryAfterError", err, err)
	}
	if retryErr.RetryAfter != 120*time.Second {
		t.Fatalf("retry after = %v, want 120s", retryErr.RetryAfter)
	}
}

func TestSubmitMarkerRefusesCoordinatesTheIntroDBRefuses(t *testing.T) {
	p, bodies, _ := submitCapture(t)
	_, err := p.SubmitMarker(context.Background(), SubmissionRequest{
		Kind:          ItemKindEpisode,
		ExternalIDs:   map[string]string{ExternalIDKeyTVDB: "121361"},
		SeasonNumber:  2019,
		EpisodeNumber: 4,
		Segment:       MarkerKindIntro,
		End:           ptrDur(60 * time.Second),
	})
	if !errors.Is(err, ErrUnsubmittable) || len(*bodies) != 0 {
		t.Fatalf("err = %v, requests = %d; want a permanent local refusal", err, len(*bodies))
	}
}
