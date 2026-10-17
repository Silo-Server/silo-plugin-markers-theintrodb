package provider

import (
	"context"
	"fmt"
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

	target, ok := NewTarget(req.Kind, req.ExternalIDs, req.Episode)
	if !ok {
		return Result{}, nil
	}

	durationMS := int64(req.Duration / time.Millisecond)

	var resp *mediaResponse
	var err error
	switch req.Kind {
	case ItemKindEpisode:
		if !validCoordinates(req.SeasonNumber, req.EpisodeNumber) {
			return Result{}, nil
		}
		resp, err = p.client.FetchEpisode(ctx, target, req.SeasonNumber, req.EpisodeNumber, durationMS)
	case ItemKindMovie:
		resp, err = p.client.FetchMovie(ctx, target, durationMS)
	default:
		return Result{}, nil
	}
	if err != nil {
		return Result{}, err
	}
	if resp == nil {
		return Result{}, nil
	}

	endsApply := sameRelease(resp, req.Duration)
	result := Result{}
	result.Markers = append(result.Markers, convertMarkers(resp.Intro, MarkerKindIntro, req.Duration)...)
	if endsApply {
		result.Markers = append(result.Markers, convertMarkers(resp.Credits, MarkerKindCredits, req.Duration)...)
	}
	result.Markers = append(result.Markers, convertMarkers(resp.Recap, MarkerKindRecap, req.Duration)...)
	if endsApply {
		result.Markers = append(result.Markers, convertMarkers(resp.Preview, MarkerKindPreview, req.Duration)...)
	}
	return result, nil
}

// releaseTolerance is how far apart TheIntroDB treats two durations as one
// release.
const releaseTolerance = 5 * time.Minute

// sameRelease reports whether the response's timestamps come from a release
// whose length matches the file. With no release within the tolerance,
// TheIntroDB answers with its most-submitted release instead. Intros and
// recaps, timed from the start, still apply, but credits and previews, which
// sit toward the end, would land in the wrong place.
func sameRelease(resp *mediaResponse, duration time.Duration) bool {
	if resp.GroupDurationMs == nil || *resp.GroupDurationMs <= 0 || duration <= 0 {
		return true
	}
	diff := time.Duration(*resp.GroupDurationMs)*time.Millisecond - duration
	return diff.Abs() <= releaseTolerance
}

// Each array entry is a separate occurrence; in particular, credits may
// surround a scene to keep.
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
		// Credits and previews run to the end of the release they were timed
		// on, so one that ends a little past this file still ends with it.
		// Silo stores whole-second durations, which makes this common.
		if !requireEnd && totalDuration > 0 && start < totalDuration && end > totalDuration && end-totalDuration <= releaseTolerance {
			end = totalDuration
		}
		if end <= start || (totalDuration > 0 && end > totalDuration) {
			continue
		}
		marker := Marker{Kind: kind, Start: start, End: end, Confidence: defaultConfidence, Algorithm: Algorithm}
		if s.Confidence != nil && *s.Confidence >= 0 && *s.Confidence <= 1 {
			marker.Confidence = *s.Confidence
		}
		if s.SubmissionCount != nil && *s.SubmissionCount > 0 {
			marker.SubmissionCount = *s.SubmissionCount
		}
		markers = append(markers, marker)
	}
	return markers
}

func validTimestamp(ms *int64) bool {
	const maxMilliseconds = int64(1<<63-1) / int64(time.Millisecond)
	return ms == nil || (*ms >= 0 && *ms <= maxMilliseconds)
}

// TheIntroDB accepts a video duration from 5 minutes to 6 hours, and no
// timestamp past 6 hours.
const (
	minSubmitVideoDuration = 5 * time.Minute
	maxSubmitVideoDuration = 6 * time.Hour
	maxSubmitTimestamp     = 6 * time.Hour
)

// SubmitMarker contributes a single segment to TheIntroDB via POST /v4/submit,
// addressing the item the same way a lookup does (see NewTarget). Per the
// TheIntroDB convention, intro/recap submissions drop a zero start (= from the
// beginning) and credits/preview drop an end at file duration (= to the end).
func (p *Provider) SubmitMarker(ctx context.Context, req SubmissionRequest) (SubmissionResult, error) {
	if p == nil || p.client == nil {
		return SubmissionResult{}, fmt.Errorf("introdb: provider not configured")
	}
	target, ok := NewTarget(req.Kind, req.ExternalIDs, req.Episode)
	if !ok {
		return SubmissionResult{}, fmt.Errorf("%w: submit requires a TVDB, TMDB, or IMDb id", ErrUnsubmittable)
	}
	segment, err := segmentName(req.Segment)
	if err != nil {
		return SubmissionResult{}, err
	}

	body := submitRequest{Segment: segment}
	target.applyTo(&body)
	switch req.Kind {
	case ItemKindEpisode:
		if !validCoordinates(req.SeasonNumber, req.EpisodeNumber) {
			return SubmissionResult{}, fmt.Errorf("%w: season %d episode %d is outside TheIntroDB's range", ErrUnsubmittable, req.SeasonNumber, req.EpisodeNumber)
		}
		body.Type = "tv"
		season, episode := req.SeasonNumber, req.EpisodeNumber
		body.Season = &season
		body.Episode = &episode
	case ItemKindMovie:
		body.Type = "movie"
	default:
		return SubmissionResult{}, fmt.Errorf("%w: unsupported item kind", ErrUnsubmittable)
	}
	if req.Duration >= minSubmitVideoDuration && req.Duration <= maxSubmitVideoDuration {
		durMs := int64(req.Duration / time.Millisecond)
		body.VideoDurationMs = &durMs
	}

	body.StartMs, body.EndMs, err = submissionBounds(req, segment)
	if err != nil {
		return SubmissionResult{}, err
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

// submissionBounds converts a segment to TheIntroDB's millisecond bounds. A
// null start means the beginning of the file (intro, recap) and a null end the
// end of it (credits, preview). TheIntroDB reads a zero intro or recap end, a
// zero credits or preview start, and an empty range as a vote that the segment
// is absent, so those are refused rather than sent.
func submissionBounds(req SubmissionRequest, segment string) (start, end *int64, err error) {
	start, end = roundedMilliseconds(req.Start), roundedMilliseconds(req.End)
	switch req.Segment {
	case MarkerKindIntro, MarkerKindRecap:
		if start != nil && *start <= 0 {
			start = nil
		}
		if end == nil || *end <= 0 {
			return nil, nil, fmt.Errorf("%w: %s needs an end after the start of the file", ErrUnsubmittable, segment)
		}
	case MarkerKindCredits, MarkerKindPreview:
		if durationMs := req.Duration.Round(time.Millisecond).Milliseconds(); end != nil && durationMs > 0 && *end >= durationMs {
			end = nil
		}
		if start == nil || *start <= 0 {
			return nil, nil, fmt.Errorf("%w: %s needs a start after the beginning of the file", ErrUnsubmittable, segment)
		}
	}
	if start != nil && end != nil && *end <= *start {
		return nil, nil, fmt.Errorf("%w: %s ends before it starts", ErrUnsubmittable, segment)
	}
	for _, bound := range []*int64{start, end} {
		if bound != nil && *bound > maxSubmitTimestamp.Milliseconds() {
			return nil, nil, fmt.Errorf("%w: %s runs past %s", ErrUnsubmittable, segment, maxSubmitTimestamp)
		}
	}
	return start, end, nil
}

// roundedMilliseconds rounds to the nearest millisecond: Silo's seconds pass
// through floating point, and truncating could turn 5 s into 4999 ms.
func roundedMilliseconds(d *time.Duration) *int64 {
	if d == nil {
		return nil
	}
	ms := d.Round(time.Millisecond).Milliseconds()
	return &ms
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
		return "", fmt.Errorf("%w: unknown segment kind %d", ErrUnsubmittable, kind)
	}
}
