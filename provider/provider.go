package provider

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Provider converts TheIntroDB timestamps to playback markers.
type Provider struct {
	client *Client
}

// NewProvider constructs a Provider backed by the supplied client. Pass an
// already-configured *Client (from NewClient) so callers control the API
// key, base URL, and lifecycle.
func NewProvider(client *Client) *Provider {
	return &Provider{client: client}
}

func (p *Provider) ID() string { return ProviderID }

// FetchMarkers looks up and converts all segment occurrences for one item.
// Missing identity or unsupported media kinds return an empty result.
func (p *Provider) FetchMarkers(ctx context.Context, req Request) (Result, error) {
	if p == nil || p.client == nil {
		return Result{}, nil
	}

	tmdbID := strings.TrimSpace(req.ExternalIDs[ExternalIDKeyTMDB])
	tvdbID := strings.TrimSpace(req.ExternalIDs[ExternalIDKeyTVDB])
	imdbID := strings.TrimSpace(req.ExternalIDs[ExternalIDKeyIMDB])
	if tmdbID == "" && tvdbID == "" && imdbID == "" {
		return Result{}, nil
	}

	durationMS := int64(req.Duration / time.Millisecond)

	var resp *mediaResponse
	var err error
	switch req.Kind {
	case ItemKindEpisode:
		if req.SeasonNumber <= 0 || req.EpisodeNumber <= 0 {
			return Result{}, nil
		}
		resp, err = p.client.FetchEpisode(ctx, tmdbID, tvdbID, imdbID, req.SeasonNumber, req.EpisodeNumber, durationMS)
	case ItemKindMovie:
		resp, err = p.client.FetchMovie(ctx, tmdbID, tvdbID, imdbID, durationMS)
	default:
		return Result{}, nil
	}
	if err != nil {
		return Result{}, err
	}
	if resp == nil {
		return Result{}, nil
	}

	result := Result{}
	result.Markers = append(result.Markers, convertMarkers(resp.Intro, MarkerKindIntro, req.Duration)...)
	result.Markers = append(result.Markers, convertMarkers(resp.Credits, MarkerKindCredits, req.Duration)...)
	result.Markers = append(result.Markers, convertMarkers(resp.Recap, MarkerKindRecap, req.Duration)...)
	result.Markers = append(result.Markers, convertMarkers(resp.Preview, MarkerKindPreview, req.Duration)...)
	return result, nil
}

// The API has already selected a release using duration_ms. Each array entry is
// a separate occurrence; in particular, credits may surround a scene to keep.
func convertMarkers(stamps []segmentTimestamps, kind MarkerKind, totalDuration time.Duration) []Marker {
	requireEnd := kind == MarkerKindIntro || kind == MarkerKindRecap
	var markers []Marker
	for _, s := range stamps {
		if !validTimestamp(s.StartMs) || !validTimestamp(s.EndMs) {
			continue
		}
		start := time.Duration(0)
		end := totalDuration
		if s.StartMs != nil {
			start = time.Duration(*s.StartMs) * time.Millisecond
		}
		if s.EndMs != nil {
			end = time.Duration(*s.EndMs) * time.Millisecond
		}
		if requireEnd && s.EndMs == nil {
			continue
		}
		// Zero is TheIntroDB's no-segment sentinel for credits and previews.
		if !requireEnd && (s.StartMs == nil || start == 0) {
			continue
		}
		if end <= start || (totalDuration > 0 && end > totalDuration) {
			continue
		}
		markers = append(markers, Marker{Kind: kind, Start: start, End: end, Confidence: defaultConfidence, Algorithm: Algorithm})
	}
	return markers
}

func validTimestamp(ms *int64) bool {
	const maxMilliseconds = int64(1<<63-1) / int64(time.Millisecond)
	return ms == nil || (*ms >= 0 && *ms <= maxMilliseconds)
}

// SubmitMarker contributes a single segment to TheIntroDB via POST /v3/submit.
// A TMDB id is required (the submission endpoint is keyed on TMDB). Per the
// TheIntroDB convention, intro/recap submissions drop a zero start (= from the
// beginning) and credits/preview drop an end at file duration (= to the end).
func (p *Provider) SubmitMarker(ctx context.Context, req SubmissionRequest) (SubmissionResult, error) {
	if p == nil || p.client == nil {
		return SubmissionResult{}, fmt.Errorf("introdb: provider not configured")
	}
	tmdbID, err := strconv.Atoi(strings.TrimSpace(req.ExternalIDs[ExternalIDKeyTMDB]))
	if err != nil || tmdbID <= 0 {
		return SubmissionResult{}, fmt.Errorf("introdb: submit requires a TMDB id")
	}
	segment, err := segmentName(req.Segment)
	if err != nil {
		return SubmissionResult{}, err
	}

	body := submitRequest{
		TmdbID:  tmdbID,
		ImdbID:  strings.TrimSpace(req.ExternalIDs[ExternalIDKeyIMDB]),
		Segment: segment,
	}
	switch req.Kind {
	case ItemKindEpisode:
		if req.SeasonNumber <= 0 || req.EpisodeNumber <= 0 {
			return SubmissionResult{}, fmt.Errorf("introdb: episode submit requires season and episode")
		}
		body.Type = "tv"
		season, episode := req.SeasonNumber, req.EpisodeNumber
		body.Season = &season
		body.Episode = &episode
	case ItemKindMovie:
		body.Type = "movie"
	default:
		return SubmissionResult{}, fmt.Errorf("introdb: unsupported item kind")
	}
	if req.Duration > 0 {
		durMs := int64(req.Duration / time.Millisecond)
		body.VideoDurationMs = &durMs
	}

	start, end := req.Start, req.End
	switch req.Segment {
	case MarkerKindIntro, MarkerKindRecap:
		if start != nil && *start <= 0 {
			start = nil // beginning of file
		}
	case MarkerKindCredits, MarkerKindPreview:
		if end != nil && req.Duration > 0 && *end >= req.Duration {
			end = nil // runs to end of file
		}
	}
	if start != nil {
		ms := int64(*start / time.Millisecond)
		body.StartMs = &ms
	}
	if end != nil {
		ms := int64(*end / time.Millisecond)
		body.EndMs = &ms
	}

	resp, err := p.client.submitSegment(ctx, body)
	if err != nil {
		return SubmissionResult{}, err
	}
	if resp == nil || len(resp.Submissions) == 0 {
		return SubmissionResult{Status: SubmissionStatusPending}, nil
	}
	s := resp.Submissions[0]
	return SubmissionResult{ID: s.ID, Status: s.Status, Weight: s.Weight}, nil
}

// FetchUserStats validates the configured key and returns contribution stats.
func (p *Provider) FetchUserStats(ctx context.Context) (UserStats, error) {
	if p == nil || p.client == nil {
		return UserStats{}, fmt.Errorf("introdb: provider not configured")
	}
	resp, err := p.client.fetchUserStats(ctx)
	if err != nil {
		return UserStats{}, err
	}
	return UserStats{
		Total:          resp.Total,
		Accepted:       resp.Accepted,
		Pending:        resp.Pending,
		Rejected:       resp.Rejected,
		AcceptanceRate: resp.AcceptanceRate,
		CurrentStreak:  resp.CurrentStreak,
		BestStreak:     resp.BestStreak,
	}, nil
}

func segmentName(kind MarkerKind) (string, error) {
	switch kind {
	case MarkerKindIntro:
		return "intro", nil
	case MarkerKindCredits:
		return "credits", nil
	case MarkerKindRecap:
		return "recap", nil
	case MarkerKindPreview:
		return "preview", nil
	default:
		return "", fmt.Errorf("introdb: unknown segment kind %d", kind)
	}
}
