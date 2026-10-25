package main

import (
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/durationpb"

	"github.com/Silo-Server/silo-plugin-markers-introdb/provider"
	pluginv1 "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginproto/silo/plugin/v1"
	publicmanifest "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginsdk/manifest"
	"github.com/Silo-Server/silo-plugin-sdk/pkg/pluginsdk/runtime"
)

var version string

//go:embed manifest.json
var manifestJSON []byte

type runtimeServer struct {
	pluginv1.UnimplementedRuntimeServer

	manifest *pluginv1.PluginManifest
	provider *provider.Provider
	client   *provider.Client
}

type markerServer struct {
	pluginv1.UnimplementedMarkerProviderServer
	runtime *runtimeServer
}

func (s *runtimeServer) GetManifest(context.Context, *pluginv1.GetManifestRequest) (*pluginv1.GetManifestResponse, error) {
	return &pluginv1.GetManifestResponse{Manifest: s.manifest}, nil
}

func (s *runtimeServer) Configure(_ context.Context, req *pluginv1.ConfigureRequest) (*pluginv1.ConfigureResponse, error) {
	apiKey := apiKeyFromConfig(req.GetConfig())
	s.client.SetAPIKey(apiKey)
	return &pluginv1.ConfigureResponse{}, nil
}

func (s *markerServer) FetchMarkers(ctx context.Context, req *pluginv1.FetchMarkersRequest) (*pluginv1.FetchMarkersResponse, error) {
	res, err := s.runtime.provider.FetchMarkers(ctx, requestFromProto(req))
	if err != nil {
		return nil, providerError(err)
	}
	out := &pluginv1.FetchMarkersResponse{}
	for _, marker := range res.Markers {
		start := marker.Start.Seconds()
		end := marker.End.Seconds()
		out.Markers = append(out.Markers, &pluginv1.MarkerSegment{
			Segment:         markerKindName(marker.Kind),
			StartSeconds:    &start,
			EndSeconds:      &end,
			Confidence:      marker.Confidence,
			SubmissionCount: int32(marker.SubmissionCount),
			Algorithm:       marker.Algorithm,
		})
	}
	return out, nil
}

func (s *markerServer) SubmitMarker(ctx context.Context, req *pluginv1.SubmitMarkerRequest) (*pluginv1.SubmitMarkerResponse, error) {
	res, err := s.runtime.provider.SubmitMarker(ctx, submissionFromProto(req))
	if err != nil {
		return nil, providerError(err)
	}
	return &pluginv1.SubmitMarkerResponse{
		SubmissionId: res.ID,
		Status:       res.Status,
		Weight:       res.Weight,
	}, nil
}

func (s *markerServer) GetMarkerProviderStats(ctx context.Context, _ *pluginv1.GetMarkerProviderStatsRequest) (*pluginv1.MarkerProviderStatsResponse, error) {
	stats, err := s.runtime.provider.FetchUserStats(ctx)
	if err != nil {
		return nil, providerError(err)
	}
	return &pluginv1.MarkerProviderStatsResponse{
		Total:          int32(stats.Total),
		Accepted:       int32(stats.Accepted),
		Pending:        int32(stats.Pending),
		Rejected:       int32(stats.Rejected),
		AcceptanceRate: stats.AcceptanceRate,
		CurrentStreak:  int32(stats.CurrentStreak),
		BestStreak:     int32(stats.BestStreak),
	}, nil
}

func main() {
	manifest, err := loadManifest()
	if err != nil {
		panic(err)
	}
	client := provider.NewClient("")
	rt := &runtimeServer{
		manifest: manifest,
		client:   client,
		provider: provider.NewProvider(client),
	}
	runtime.Serve(runtime.ServeConfig{
		Servers: runtime.CapabilityServers{
			Runtime:        rt,
			MarkerProvider: &markerServer{runtime: rt},
			HttpRoutes:     &httpRoutesServer{runtime: rt},
		},
	})
}

func loadManifest() (*pluginv1.PluginManifest, error) {
	manifest, err := publicmanifest.Load(manifestJSON)
	if err != nil {
		return nil, fmt.Errorf("load embedded manifest: %w", err)
	}
	if version != "" {
		manifest.Version = version
	}
	executablePath, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("resolve executable path: %w", err)
	}
	binaryData, err := os.ReadFile(executablePath)
	if err != nil {
		return nil, fmt.Errorf("read executable %q: %w", executablePath, err)
	}
	checksum := sha256.Sum256(binaryData)
	manifest.Checksum = hex.EncodeToString(checksum[:])

	// Routes are registered here rather than declared in the manifest JSON: the
	// descriptor has to name the handler's path, and keeping both in one place
	// means they cannot drift.
	if err := publicmanifest.RegisterHTTPRoutes(manifest, &pluginv1.HttpRouteDescriptor{
		Id:     "report",
		Method: http.MethodPost,
		Path:   reportRoutePath,
		// Reports are per-account and act on TheIntroDB's data, so the route
		// needs a signed-in caller and is not part of the public surface.
		Access:    "authenticated",
		Navigable: false,
	}); err != nil {
		return nil, fmt.Errorf("register plugin routes: %w", err)
	}
	return manifest, nil
}

func apiKeyFromConfig(entries []*pluginv1.ConfigEntry) string {
	for _, entry := range entries {
		if entry == nil || entry.GetKey() != "account" || entry.GetValue() == nil {
			continue
		}
		if raw, ok := entry.GetValue().AsMap()["api_key"].(string); ok {
			return strings.TrimSpace(raw)
		}
	}
	return ""
}

func requestFromProto(req *pluginv1.FetchMarkersRequest) provider.Request {
	return provider.Request{
		Kind:          itemKind(req.GetItemType()),
		ExternalIDs:   externalIDs(req.GetExternalIds()),
		SeasonNumber:  int(req.GetSeasonNumber()),
		EpisodeNumber: int(req.GetEpisodeNumber()),
		Episode:       episodeFromProto(req.GetEpisodeExternalIds(), req.GetEpisodeNumbering(), req.GetEpisodeTitle()),
		Duration:      secondsDuration(req.GetDurationSeconds()),
	}
}

func submissionFromProto(req *pluginv1.SubmitMarkerRequest) provider.SubmissionRequest {
	out := provider.SubmissionRequest{
		Kind:          itemKind(req.GetItemType()),
		ExternalIDs:   externalIDs(req.GetExternalIds()),
		SeasonNumber:  int(req.GetSeasonNumber()),
		EpisodeNumber: int(req.GetEpisodeNumber()),
		Episode:       episodeFromProto(req.GetEpisodeExternalIds(), req.GetEpisodeNumbering(), req.GetEpisodeTitle()),
		Segment:       markerKind(req.GetSegment()),
		Duration:      secondsDuration(req.GetDurationSeconds()),
	}
	if req.StartSeconds != nil {
		start := secondsDuration(req.GetStartSeconds())
		out.Start = &start
	}
	if req.EndSeconds != nil {
		end := secondsDuration(req.GetEndSeconds())
		out.End = &end
	}
	return out
}

func episodeFromProto(ids *pluginv1.MarkerExternalIDs, numbering, title string) provider.Episode {
	return provider.Episode{
		ExternalIDs: externalIDs(ids),
		Numbering:   strings.TrimSpace(numbering),
		Title:       strings.TrimSpace(title),
	}
}

func externalIDs(ids *pluginv1.MarkerExternalIDs) map[string]string {
	out := map[string]string{}
	if ids == nil {
		return out
	}
	setID(out, provider.ExternalIDKeyTMDB, ids.GetTmdbId())
	setID(out, provider.ExternalIDKeyIMDB, ids.GetImdbId())
	setID(out, provider.ExternalIDKeyTVDB, ids.GetTvdbId())
	if providerIDs := ids.GetProviderIds(); providerIDs != nil {
		for key, value := range providerIDs.AsMap() {
			if s, ok := value.(string); ok {
				setID(out, strings.ToLower(strings.TrimSpace(key)), s)
			}
		}
	}
	return out
}

func setID(out map[string]string, key, value string) {
	if strings.TrimSpace(key) != "" && strings.TrimSpace(value) != "" {
		out[key] = strings.TrimSpace(value)
	}
}

func itemKind(kind string) provider.ItemKind {
	switch strings.ToLower(strings.TrimSpace(kind)) {
	case "episode":
		return provider.ItemKindEpisode
	case "movie":
		return provider.ItemKindMovie
	default:
		return 0
	}
}

func markerKind(kind string) provider.MarkerKind {
	switch strings.ToLower(strings.TrimSpace(kind)) {
	case "intro":
		return provider.MarkerKindIntro
	case "credits":
		return provider.MarkerKindCredits
	case "recap":
		return provider.MarkerKindRecap
	case "preview":
		return provider.MarkerKindPreview
	default:
		return 0
	}
}

func markerKindName(kind provider.MarkerKind) string {
	switch kind {
	case provider.MarkerKindIntro:
		return "intro"
	case provider.MarkerKindCredits:
		return "credits"
	case provider.MarkerKindRecap:
		return "recap"
	case provider.MarkerKindPreview:
		return "preview"
	default:
		return ""
	}
}

func secondsDuration(seconds float64) time.Duration {
	return time.Duration(seconds * float64(time.Second))
}

func providerError(err error) error {
	// InvalidArgument tells the host to record a permanent refusal rather
	// than retry a submission that cannot succeed.
	if errors.Is(err, provider.ErrUnsubmittable) {
		return status.Error(codes.InvalidArgument, err.Error())
	}
	var retryErr *provider.RetryAfterError
	if !errors.As(err, &retryErr) || retryErr == nil {
		return err
	}
	st := status.New(codes.ResourceExhausted, retryErr.Error())
	if retryErr.RetryAfter <= 0 {
		return st.Err()
	}
	withDetails, detailErr := st.WithDetails(&errdetails.RetryInfo{
		RetryDelay: durationpb.New(retryErr.RetryAfter),
	})
	if detailErr != nil {
		return st.Err()
	}
	return withDetails.Err()
}

// reportRoutePath is the path the report route is served on, relative to the
// plugin's mount on the host. The descriptor in loadManifest names it too, so
// the two cannot disagree.
const reportRoutePath = "/report"

// reportRequestBody is what a client posts to the report route. It mirrors the
// addressing the plugin's markers arrive with: whichever identifiers the host
// knows the title by, plus the season and episode for a series.
type reportRequestBody struct {
	Type    string  `json:"type"`
	TVDBID  *int    `json:"tvdb_id,omitempty"`
	TMDBID  *int    `json:"tmdb_id,omitempty"`
	IMDBID  *string `json:"imdb_id,omitempty"`
	Season  *int    `json:"season,omitempty"`
	Episode *int    `json:"episode,omitempty"`
	Mapping string  `json:"mapping,omitempty"`
}

// httpRoutesServer serves the routes this plugin exposes to the host. They are
// reached through the host rather than directly by the browser, which is what
// keeps the configured API key on the server: the viewer's client calls Silo,
// Silo calls this, and this process is the only thing that ever holds the key.
type httpRoutesServer struct {
	pluginv1.UnimplementedHttpRoutesServer
	runtime *runtimeServer
}

func (s *httpRoutesServer) Handle(ctx context.Context, req *pluginv1.HandleHTTPRequest) (*pluginv1.HandleHTTPResponse, error) {
	if strings.EqualFold(req.GetMethod(), http.MethodPost) && req.GetPath() == reportRoutePath {
		return s.handleReport(ctx, req)
	}
	return jsonRouteResponse(http.StatusNotFound, map[string]string{
		"error": "no such plugin route",
	})
}

// handleReport forwards a viewer's report to TheIntroDB.
//
// The body is validated here as well as on TheIntroDB's side: this route takes
// input from any authenticated account, and checking it before the round trip
// turns a remote refusal into a message the viewer can act on.
func (s *httpRoutesServer) handleReport(ctx context.Context, req *pluginv1.HandleHTTPRequest) (*pluginv1.HandleHTTPResponse, error) {
	var body reportRequestBody
	if err := json.Unmarshal(req.GetBody(), &body); err != nil {
		return jsonRouteResponse(http.StatusBadRequest, map[string]string{
			"error": "the request body must be a JSON object",
		})
	}

	mediaType := strings.ToLower(strings.TrimSpace(body.Type))
	if mediaType != "movie" && mediaType != "tv" {
		return jsonRouteResponse(http.StatusBadRequest, map[string]string{
			"error": `type must be "movie" or "tv"`,
		})
	}
	if body.TVDBID == nil && body.TMDBID == nil && body.IMDBID == nil {
		return jsonRouteResponse(http.StatusBadRequest, map[string]string{
			"error": "one of tvdb_id, tmdb_id or imdb_id is required",
		})
	}
	if mediaType == "tv" && (body.Season == nil || body.Episode == nil) {
		return jsonRouteResponse(http.StatusBadRequest, map[string]string{
			"error": "season and episode are required to report an episode",
		})
	}

	result, err := s.runtime.client.FlagVote(ctx, provider.FlagRequest{
		TVDBID:  body.TVDBID,
		TMDBID:  body.TMDBID,
		IMDBID:  body.IMDBID,
		Type:    mediaType,
		Season:  body.Season,
		Episode: body.Episode,
		Mapping: strings.TrimSpace(body.Mapping),
	})
	if err != nil {
		// A repeat report is a settled outcome: the viewer is told the segment
		// is already known about rather than shown a failure.
		var conflict *provider.FlagConflictError
		if errors.As(err, &conflict) {
			return jsonRouteResponse(http.StatusConflict, map[string]string{"message": conflict.Message})
		}
		return jsonRouteResponse(statusForFlagError(err), map[string]string{"error": err.Error()})
	}

	message := result.Message
	if strings.TrimSpace(message) == "" {
		message = "Thanks — TheIntroDB will review this segment."
	}
	return jsonRouteResponse(http.StatusOK, map[string]string{"message": message})
}

// statusForFlagError maps TheIntroDB's failure modes onto the status the host
// should hand back, so a client can tell "try later" from "this will not work".
func statusForFlagError(err error) int {
	var retryErr *provider.RetryAfterError
	if errors.As(err, &retryErr) {
		return http.StatusTooManyRequests
	}
	// No key configured is the server's gap, not something the caller did.
	if strings.Contains(err.Error(), "requires an API key") {
		return http.StatusServiceUnavailable
	}
	return http.StatusBadGateway
}

// jsonRouteResponse encodes a route reply, which is how the host receives it.
func jsonRouteResponse(status int, payload any) (*pluginv1.HandleHTTPResponse, error) {
	encoded, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	return &pluginv1.HandleHTTPResponse{
		StatusCode: int32(status),
		Headers:    map[string]string{"Content-Type": "application/json"},
		Body:       encoded,
	}, nil
}
