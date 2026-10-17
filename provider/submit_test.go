package provider

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
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
		Episode:       Episode{Title: "Ignored For TMDB"},
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
		Kind:          ItemKindEpisode,
		ExternalIDs:   map[string]string{ExternalIDKeyTMDB: "1399", ExternalIDKeyTVDB: "121361"},
		SeasonNumber:  0,
		EpisodeNumber: 3,
		Episode:       Episode{ExternalIDs: map[string]string{ExternalIDKeyTVDB: "4271129", ExternalIDKeyTMDB: "63087"}, Numbering: "tvdb", Title: " A Special "},
		Segment:       MarkerKindCredits,
		Start:         ptrDur(40 * time.Minute),
		End:           ptrDur(50 * time.Minute),
		Duration:      50 * time.Minute,
	})
	if err != nil {
		t.Fatalf("SubmitMarker: %v", err)
	}
	gotBody := (*bodies)[0]
	if gotBody.TvdbID == nil || *gotBody.TvdbID != 121361 || gotBody.TmdbID != nil {
		t.Errorf("identity = tvdb %v tmdb %v, want TVDB 121361 only", gotBody.TvdbID, gotBody.TmdbID)
	}
	// The pin identifies the episode, so the title, which may be localized,
	// is not sent to corroborate it.
	if gotBody.TvdbEpisodeID == nil || *gotBody.TvdbEpisodeID != 4271129 || gotBody.EpisodeName != "" {
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
		Kind:        ItemKindMovie,
		ExternalIDs: map[string]string{ExternalIDKeyTMDB: "603", ExternalIDKeyTVDB: "169"},
		Episode:     Episode{ExternalIDs: map[string]string{ExternalIDKeyTVDB: "1"}, Numbering: "tvdb", Title: "not an episode"},
		Segment:     MarkerKindIntro,
		End:         ptrDur(90 * time.Second),
		Duration:    4 * time.Minute,
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

func TestSubmitMarkerRefusesWhatTheIntroDBReadsAsNoSegment(t *testing.T) {
	for _, tt := range []struct {
		name       string
		segment    MarkerKind
		start, end *time.Duration
	}{
		{name: "credits starting at the first frame", segment: MarkerKindCredits, start: ptrDur(0), end: ptrDur(45 * time.Second)},
		{name: "preview starting within half a millisecond", segment: MarkerKindPreview, start: ptrDur(400 * time.Microsecond), end: ptrDur(45 * time.Second)},
		{name: "credits without a start", segment: MarkerKindCredits, end: ptrDur(45 * time.Second)},
		{name: "intro ending at the first frame", segment: MarkerKindIntro, start: ptrDur(0), end: ptrDur(0)},
		{name: "recap without an end", segment: MarkerKindRecap, start: ptrDur(0)},
		{name: "empty range", segment: MarkerKindPreview, start: ptrDur(90 * time.Second), end: ptrDur(90 * time.Second)},
		{name: "past six hours", segment: MarkerKindCredits, start: ptrDur(6*time.Hour + time.Second), end: ptrDur(6*time.Hour + time.Minute)},
	} {
		t.Run(tt.name, func(t *testing.T) {
			p, bodies, _ := submitCapture(t)
			_, err := p.SubmitMarker(context.Background(), SubmissionRequest{
				Kind:        ItemKindMovie,
				ExternalIDs: map[string]string{ExternalIDKeyTMDB: "603"},
				Segment:     tt.segment,
				Start:       tt.start,
				End:         tt.end,
				Duration:    7 * time.Hour,
			})
			if !errors.Is(err, ErrUnsubmittable) || len(*bodies) != 0 {
				t.Fatalf("err = %v, requests = %d; want a local refusal, not a no-segment vote", err, len(*bodies))
			}
		})
	}
}

func TestSubmitMarkerRoundsToTheNearestMillisecond(t *testing.T) {
	p, bodies, _ := submitCapture(t)
	// 15.1 s and 20.1 s land just below the millisecond after float math.
	start, end := time.Duration(15.1*float64(time.Second)), time.Duration(20.1*float64(time.Second))
	if _, err := p.SubmitMarker(context.Background(), SubmissionRequest{
		Kind:        ItemKindMovie,
		ExternalIDs: map[string]string{ExternalIDKeyTMDB: "603"},
		Segment:     MarkerKindIntro,
		Start:       &start,
		End:         &end,
		Duration:    2 * time.Hour,
	}); err != nil {
		t.Fatal(err)
	}
	if got := (*bodies)[0]; got.StartMs == nil || *got.StartMs != 15100 || got.EndMs == nil || *got.EndMs != 20100 {
		t.Fatalf("bounds = %v-%v, want 15100-20100", got.StartMs, got.EndMs)
	}
}

func TestSubmitFailuresTheHostCanActOn(t *testing.T) {
	type answer struct {
		status      int
		contentType string
		headers     map[string]string
		body        string
	}
	for _, tt := range []struct {
		name      string
		answer    answer
		wantRetry time.Duration
		wantText  string
	}{
		{
			name:      "account check unavailable",
			answer:    answer{503, "application/json", nil, `{"error":"Unable to verify account status, please try again later"}`},
			wantRetry: defaultBlockedCooldown,
			wantText:  "submit HTTP 503: Unable to verify account status",
		},
		{
			name:      "Cloudflare block",
			answer:    answer{403, "text/html", map[string]string{"CF-Ray": "ray-1", "Server": "cloudflare"}, "<html>blocked</html>"},
			wantRetry: defaultBlockedCooldown,
			wantText:  "Cloudflare blocked request (HTTP 403, cf-ray ray-1)",
		},
		{
			// TheIntroDB's own JSON errors also pass through Cloudflare.
			name:     "origin 403 through Cloudflare",
			answer:   answer{403, "application/json; charset=utf-8", map[string]string{"CF-Ray": "ray-2", "Server": "cloudflare"}, `{"error":"Your account has been locked"}`},
			wantText: "introdb: submit HTTP 403: Your account has been locked",
		},
		{
			// The host records "submit HTTP 404" as a permanent refusal,
			// so a missing route must not carry that text.
			name:     "missing route",
			answer:   answer{404, "text/plain", nil, "Cannot POST /v4/submit"},
			wantText: "submit endpoint not found (HTTP 404)",
		},
		{
			name:     "refusal keeps the status the host reads and only the message",
			answer:   answer{400, "application/json", nil, `{"error":"intro duration must not exceed 300000 milliseconds (300 seconds)"}`},
			wantText: "introdb: submit HTTP 400: intro duration must not exceed 300000 milliseconds (300 seconds)",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var requests int
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				requests++
				for k, v := range tt.answer.headers {
					w.Header().Set(k, v)
				}
				w.Header().Set("Content-Type", tt.answer.contentType)
				w.WriteHeader(tt.answer.status)
				_, _ = w.Write([]byte(tt.answer.body))
			}))
			defer srv.Close()
			c := NewClient("secret-key")
			c.SetBaseURL(srv.URL)
			_, err := NewProvider(c).SubmitMarker(context.Background(), SubmissionRequest{
				Kind:        ItemKindMovie,
				ExternalIDs: map[string]string{ExternalIDKeyTMDB: "603"},
				Segment:     MarkerKindIntro,
				End:         ptrDur(60 * time.Second),
			})
			if err == nil || !strings.Contains(err.Error(), tt.wantText) {
				t.Fatalf("error = %v, want text %q", err, tt.wantText)
			}
			var retryErr *RetryAfterError
			if tt.wantRetry > 0 && (!errors.As(err, &retryErr) || retryErr.RetryAfter != tt.wantRetry) {
				t.Fatalf("error = %T %v, want a %s cooldown", err, err, tt.wantRetry)
			}
			if tt.wantRetry == 0 && errors.As(err, &retryErr) {
				t.Fatalf("error = %v, want no cooldown", err)
			}
			if strings.Contains(err.Error(), "<html>") || requests != 1 {
				t.Fatalf("error = %q after %d requests, want one request and no page body", err, requests)
			}
		})
	}
}

func TestSubmitSkipsAKeyTheIntroDBRejected(t *testing.T) {
	var submits, stats int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/user/stats" {
			stats++
			_, _ = w.Write([]byte(`{"total":1}`))
			return
		}
		submits++
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"Invalid or expired token"}`))
	}))
	defer srv.Close()
	c := NewClient("wrong-site-key")
	c.SetBaseURL(srv.URL)
	p := NewProvider(c)
	submit := func() error {
		_, err := p.SubmitMarker(context.Background(), SubmissionRequest{
			Kind: ItemKindMovie, ExternalIDs: map[string]string{ExternalIDKeyTMDB: "603"},
			Segment: MarkerKindIntro, End: ptrDur(60 * time.Second),
		})
		return err
	}
	if err := submit(); err == nil || !strings.Contains(err.Error(), "submit HTTP 401: Invalid or expired token") {
		t.Fatalf("first submit error = %v", err)
	}
	for range 3 {
		if err := submit(); err == nil || !strings.Contains(err.Error(), "rejected this API key") {
			t.Fatalf("later submit error = %v", err)
		}
	}
	if submits != 1 {
		t.Fatalf("submit requests = %d, want 1 until the key changes", submits)
	}
	// A key check that succeeds clears the rejection.
	if _, err := p.FetchUserStats(context.Background()); err != nil || stats != 1 {
		t.Fatalf("stats err = %v, requests = %d", err, stats)
	}
	_ = submit()
	if submits != 2 {
		t.Fatalf("submit requests after a good key check = %d, want 2", submits)
	}
	// So does a new key, and returning to a rejected key tries it again.
	_ = submit()
	c.SetAPIKey("theintrodb-key")
	_ = submit()
	if submits != 3 {
		t.Fatalf("submit requests after a key change = %d, want 3", submits)
	}
	c.SetAPIKey("wrong-site-key")
	_ = submit()
	if submits != 4 {
		t.Fatalf("submit requests after restoring a rejected key = %d, want 4", submits)
	}
}

func TestSubmitErrorTextIsBounded(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte(strings.Repeat("x", 4096)))
	}))
	defer srv.Close()
	c := NewClient("secret-key")
	c.SetBaseURL(srv.URL)
	_, err := NewProvider(c).SubmitMarker(context.Background(), SubmissionRequest{
		Kind: ItemKindMovie, ExternalIDs: map[string]string{ExternalIDKeyTMDB: "603"},
		Segment: MarkerKindIntro, End: ptrDur(60 * time.Second),
	})
	if err == nil || len(err.Error()) > maxErrorReason+64 {
		t.Fatalf("error length = %d, want at most about %d", len(err.Error()), maxErrorReason)
	}
}
