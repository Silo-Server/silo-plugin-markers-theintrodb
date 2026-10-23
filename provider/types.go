// Package provider implements TheIntroDB marker lookup and contribution logic.
package provider

import (
	"errors"
	"fmt"
	"time"
)

// ProviderID is the canonical identifier stored in
// media_files.*_markers_provider for markers sourced from TheIntroDB.
const ProviderID = "introdb"

// Algorithm is the algorithm tag written alongside markers. The version
// suffix names the upstream API contract the markers came from.
const Algorithm = "introdb:v4"

// defaultConfidence stands in when a response omits TheIntroDB's own
// per-segment confidence or sends one outside 0-1. Silo picks the most
// confident online marker, so such a segment is used only when no rated
// marker competes with it.
const defaultConfidence = 0

// DefaultBaseURL is the production TheIntroDB v4 endpoint. Overridable
// in tests via Client.SetBaseURL.
const DefaultBaseURL = "https://api.theintrodb.org/v4"

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

// Request describes one item to look up. For an episode, ExternalIDs names
// the series, SeasonNumber/EpisodeNumber place the episode in Silo's
// numbering (season 0 holds specials), and Episode describes it further.
type Request struct {
	Kind          ItemKind
	ExternalIDs   map[string]string
	SeasonNumber  int
	EpisodeNumber int
	Episode       Episode
	Duration      time.Duration
}

// Episode is what Silo knows about an episode beyond its series' IDs. All
// fields are empty for a movie and on hosts that predate them.
type Episode struct {
	// ExternalIDs are the provider episode records Silo matched at the
	// episode's numbers. Silo matches each provider's episodes by number
	// alone, so two providers' IDs can name different episodes.
	ExternalIDs map[string]string
	// Numbering is the external ID key ("tvdb" or "tmdb") of the provider
	// whose numbering the episode follows, or empty when Silo cannot tell.
	Numbering string
	// Title is in the library's metadata language.
	Title string
}

type Result struct {
	Markers []Marker
}

type Marker struct {
	Kind            MarkerKind
	Start           time.Duration
	End             time.Duration
	Confidence      float64
	SubmissionCount int
	Algorithm       string
}

// SubmissionRequest describes one segment to contribute. The item fields
// follow Request.
type SubmissionRequest struct {
	Kind          ItemKind
	ExternalIDs   map[string]string
	SeasonNumber  int
	EpisodeNumber int
	Episode       Episode
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

// ErrUnsubmittable marks a submission TheIntroDB cannot accept however often
// it is retried, such as an item without a usable ID.
var ErrUnsubmittable = errors.New("introdb: unsubmittable")

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

// mediaResponse mirrors the JSON shape returned by GET /v4/media. Each
// segment kind is an array of zero or more entries; absent fields are decoded
// as empty slices via Go's zero-value semantics. The episode-list fallback
// TheIntroDB sends for media without submissions decodes to no segments.
type mediaResponse struct {
	TvdbID int    `json:"tvdb_id"`
	Type   string `json:"type"`
	// GroupDurationMs is the duration of the release whose timestamps the
	// response carries; present with details when that duration is known.
	GroupDurationMs *int64              `json:"group_duration_ms,omitempty"`
	Season          *int                `json:"season,omitempty"`
	Episode         *int                `json:"episode,omitempty"`
	Intro           []segmentTimestamps `json:"intro,omitempty"`
	Recap           []segmentTimestamps `json:"recap,omitempty"`
	Credits         []segmentTimestamps `json:"credits,omitempty"`
	Preview         []segmentTimestamps `json:"preview,omitempty"`
}

// segmentTimestamps is the per-occurrence shape returned by TheIntroDB.
// Either bound may be nil — for intro/recap, start may be omitted (segment
// begins at file start); for credits/preview, end may be omitted (segment
// runs to file end). Confidence and SubmissionCount are present because
// lookups ask for details.
type segmentTimestamps struct {
	StartMs         *int64   `json:"start_ms,omitempty"`
	EndMs           *int64   `json:"end_ms,omitempty"`
	Confidence      *float64 `json:"confidence,omitempty"`
	SubmissionCount *int     `json:"submission_count,omitempty"`
}

// submitRequest is the POST /v4/submit body. Exactly one of TvdbID, TmdbID and
// ImdbID is set (see Target). start_ms and end_ms are sent as explicit null
// (no omitempty) when the segment begins at the start (intro/recap) or runs to
// the end (credits/preview).
type submitRequest struct {
	TvdbID          *int64 `json:"tvdb_id,omitempty"`
	TmdbID          *int64 `json:"tmdb_id,omitempty"`
	ImdbID          string `json:"imdb_id,omitempty"`
	Type            string `json:"type"`
	Segment         string `json:"segment"`
	Season          *int   `json:"season,omitempty"`
	Episode         *int   `json:"episode,omitempty"`
	TvdbEpisodeID   *int64 `json:"tvdb_episode_id,omitempty"`
	EpisodeName     string `json:"episode_name,omitempty"`
	VideoDurationMs *int64 `json:"video_duration_ms,omitempty"`
	StartMs         *int64 `json:"start_ms"`
	EndMs           *int64 `json:"end_ms"`
}

// submitResponse mirrors the POST /v4/submit success body.
type submitResponse struct {
	OK          bool               `json:"ok"`
	Submissions []submissionRecord `json:"submissions"`
}

type submissionRecord struct {
	ID     string  `json:"id"`
	Status string  `json:"status"` // pending | accepted | rejected
	Weight float64 `json:"weight"`
}

// userStatsResponse mirrors GET /v4/user/stats. A non-empty Error (or a non-2xx
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
