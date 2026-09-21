package main

import (
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"errors"
	"fmt"
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
			Segment:      markerKindName(marker.Kind),
			StartSeconds: &start,
			EndSeconds:   &end,
			Confidence:   marker.Confidence,
			Algorithm:    marker.Algorithm,
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
		Duration:      secondsDuration(req.GetDurationSeconds()),
	}
}

func submissionFromProto(req *pluginv1.SubmitMarkerRequest) provider.SubmissionRequest {
	out := provider.SubmissionRequest{
		Kind:          itemKind(req.GetItemType()),
		ExternalIDs:   externalIDs(req.GetExternalIds()),
		SeasonNumber:  int(req.GetSeasonNumber()),
		EpisodeNumber: int(req.GetEpisodeNumber()),
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
