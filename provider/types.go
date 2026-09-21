// Package provider implements TheIntroDB marker lookup and contribution logic.
package provider

import (
	"fmt"
	"time"
)

// ProviderID is the canonical identifier stored in
// media_files.*_markers_provider for markers sourced from TheIntroDB.
const ProviderID = "introdb"

// Algorithm is the algorithm tag written alongside markers. The version
// suffix lets us invalidate or refresh markers if the upstream contract
// changes.
const Algorithm = "introdb:v3"

// defaultConfidence is Silo's confidence policy for this provider. TheIntroDB
// does not publish per-segment confidence or submission counts.
const defaultConfidence = 0.9

// DefaultBaseURL is the production TheIntroDB v3 endpoint. Overridable
// in tests via Client.SetBaseURL.
const DefaultBaseURL = "https://api.theintrodb.org/v3"

const (
	ExternalIDKeyTMDB = "tmdb"
	ExternalIDKeyIMDB = "imdb"
	ExternalIDKeyTVDB = "tvdb"
)

type ItemKind int

const (
	ItemKindEpisode ItemKind = iota + 1
	ItemKindMovie
)

type MarkerKind int

const (
	MarkerKindIntro MarkerKind = iota + 1
	MarkerKindCredits
	MarkerKindRecap
	MarkerKindPreview
)

type Request struct {
	Kind          ItemKind
	ExternalIDs   map[string]string
	SeasonNumber  int
	EpisodeNumber int
	Duration      time.Duration
}

type Result struct {
	Markers []Marker
}

type Marker struct {
	Kind       MarkerKind
	Start      time.Duration
	End        time.Duration
	Confidence float64
	Algorithm  string
}

type SubmissionRequest struct {
	Kind          ItemKind
	ExternalIDs   map[string]string
	SeasonNumber  int
	EpisodeNumber int
	Segment       MarkerKind
	Start         *time.Duration
	End           *time.Duration
	Duration      time.Duration
}

const (
	SubmissionStatusPending  = "pending"
	SubmissionStatusAccepted = "accepted"
	SubmissionStatusRejected = "rejected"
)

type SubmissionResult struct {
	ID     string
	Status string
	Weight float64
}

type UserStats struct {
	Total          int
	Accepted       int
	Pending        int
	Rejected       int
	AcceptanceRate float64
	CurrentStreak  int
	BestStreak     int
}

type RetryAfterError struct {
	RetryAfter time.Duration
	Message    string
}

func (e *RetryAfterError) Error() string {
	if e == nil {
		return ""
	}
	if e.Message != "" {
		return e.Message
	}
	return fmt.Sprintf("retry after %s", e.RetryAfter)
}

// mediaResponse mirrors the JSON shape returned by GET /v3/media.
// Each segment kind is an array of zero or more entries; absent fields
// are decoded as empty slices via Go's zero-value semantics.
type mediaResponse struct {
	TmdbID  int                 `json:"tmdb_id"`
	Type    string              `json:"type"`
	Season  *int                `json:"season,omitempty"`
	Episode *int                `json:"episode,omitempty"`
	Intro   []segmentTimestamps `json:"intro,omitempty"`
	Recap   []segmentTimestamps `json:"recap,omitempty"`
	Credits []segmentTimestamps `json:"credits,omitempty"`
	Preview []segmentTimestamps `json:"preview,omitempty"`
}

// segmentTimestamps is the per-occurrence shape returned by TheIntroDB.
// Either bound may be nil — for intro/recap, start may be omitted (segment
// begins at file start); for credits/preview, end may be omitted (segment
// runs to file end).
type segmentTimestamps struct {
	StartMs *int64 `json:"start_ms,omitempty"`
	EndMs   *int64 `json:"end_ms,omitempty"`
}

// submitRequest is the POST /v3/submit body. tmdb_id is required; start_ms and
// end_ms are sent as explicit null (no omitempty) when the segment begins at
// the start (intro/recap) or runs to the end (credits/preview).
type submitRequest struct {
	TmdbID          int    `json:"tmdb_id"`
	ImdbID          string `json:"imdb_id,omitempty"`
	Type            string `json:"type"`
	Segment         string `json:"segment"`
	Season          *int   `json:"season,omitempty"`
	Episode         *int   `json:"episode,omitempty"`
	VideoDurationMs *int64 `json:"video_duration_ms,omitempty"`
	StartMs         *int64 `json:"start_ms"`
	EndMs           *int64 `json:"end_ms"`
}

// submitResponse mirrors the POST /v3/submit success body.
type submitResponse struct {
	Submissions []submissionRecord `json:"submissions"`
}

type submissionRecord struct {
	ID     string  `json:"id"`
	Status string  `json:"status"` // pending | accepted | rejected
	Weight float64 `json:"weight"`
}

// userStatsResponse mirrors GET /v3/user/stats. A non-empty Error (or a non-2xx
// status) means the key is invalid.
type userStatsResponse struct {
	Total          int     `json:"total"`
	Accepted       int     `json:"accepted"`
	Pending        int     `json:"pending"`
	Rejected       int     `json:"rejected"`
	AcceptanceRate float64 `json:"acceptance_rate"`
	CurrentStreak  int     `json:"current_streak"`
	BestStreak     int     `json:"best_streak"`
	Error          string  `json:"error,omitempty"`
}
