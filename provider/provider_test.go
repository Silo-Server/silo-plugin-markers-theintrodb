package provider

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"
)

func newProvider(t *testing.T, body string) *Provider {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	c := NewClient("")
	c.SetBaseURL(srv.URL)
	return NewProvider(c)
}

func episodeReq(ids map[string]string) Request {
	return Request{
		Kind:          ItemKindEpisode,
		ExternalIDs:   ids,
		SeasonNumber:  1,
		EpisodeNumber: 1,
		Duration:      30 * time.Minute,
	}
}

func TestProviderResolvesTVDBOnly(t *testing.T) {
	p := newProvider(t, `{"type":"episode","intro":[{"end_ms":60000}]}`)
	res, err := p.FetchMarkers(context.Background(), episodeReq(map[string]string{ExternalIDKeyTVDB: "777"}))
	if err != nil {
		t.Fatalf("FetchMarkers: %v", err)
	}
	if len(res.Markers) != 1 || res.Markers[0].Kind != MarkerKindIntro {
		t.Fatalf("expected one intro marker, got %+v", res.Markers)
	}
}

func TestProviderPreservesSegmentOccurrences(t *testing.T) {
	p := newProvider(t, `{
		"tmdb_id":123,"type":"tv","season":1,"episode":1,
		"intro":[{"start_ms":null,"end_ms":60000},{"start_ms":210000,"end_ms":250000}],
		"credits":[{"start_ms":1450000,"end_ms":1500000},{"start_ms":1570000,"end_ms":null}],
		"recap":[{"start_ms":null,"end_ms":45000}],
		"preview":[{"start_ms":1350000,"end_ms":1400000}]
	}`)
	res, err := p.FetchMarkers(context.Background(), episodeReq(map[string]string{ExternalIDKeyTMDB: "123"}))
	if err != nil {
		t.Fatalf("FetchMarkers: %v", err)
	}
	want := []Marker{
		{Kind: MarkerKindIntro, End: 60 * time.Second, Confidence: defaultConfidence, Algorithm: Algorithm},
		{Kind: MarkerKindIntro, Start: 210 * time.Second, End: 250 * time.Second, Confidence: defaultConfidence, Algorithm: Algorithm},
		{Kind: MarkerKindCredits, Start: 1450 * time.Second, End: 1500 * time.Second, Confidence: defaultConfidence, Algorithm: Algorithm},
		{Kind: MarkerKindCredits, Start: 1570 * time.Second, End: 1800 * time.Second, Confidence: defaultConfidence, Algorithm: Algorithm},
		{Kind: MarkerKindRecap, End: 45 * time.Second, Confidence: defaultConfidence, Algorithm: Algorithm},
		{Kind: MarkerKindPreview, Start: 1350 * time.Second, End: 1400 * time.Second, Confidence: defaultConfidence, Algorithm: Algorithm},
	}
	if !reflect.DeepEqual(res.Markers, want) {
		t.Fatalf("markers = %+v, want %+v", res.Markers, want)
	}
}

func TestProviderValidatesSegmentBounds(t *testing.T) {
	for _, tt := range []struct {
		name     string
		segment  string
		stamp    string
		duration time.Duration
		start    time.Duration
		end      time.Duration
		valid    bool
	}{
		{name: "intro from file start", segment: "intro", stamp: `{"start_ms":null,"end_ms":60000}`, duration: 30 * time.Minute, end: time.Minute, valid: true},
		{name: "recap omitted start", segment: "recap", stamp: `{"end_ms":45000}`, duration: 30 * time.Minute, end: 45 * time.Second, valid: true},
		{name: "credits to file end", segment: "credits", stamp: `{"start_ms":1500000,"end_ms":null}`, duration: 30 * time.Minute, start: 25 * time.Minute, end: 30 * time.Minute, valid: true},
		{name: "preview omitted end", segment: "preview", stamp: `{"start_ms":1500000}`, duration: 30 * time.Minute, start: 25 * time.Minute, end: 30 * time.Minute, valid: true},
		{name: "explicit range without duration", segment: "intro", stamp: `{"start_ms":1000,"end_ms":60000}`, start: time.Second, end: time.Minute, valid: true},
		{name: "intro missing end", segment: "intro", stamp: `{"start_ms":1000}`, duration: 30 * time.Minute},
		{name: "recap missing end", segment: "recap", stamp: `{"start_ms":1000,"end_ms":null}`, duration: 30 * time.Minute},
		{name: "credits missing start", segment: "credits", stamp: `{"end_ms":1800000}`, duration: 30 * time.Minute},
		{name: "preview missing start", segment: "preview", stamp: `{"start_ms":null,"end_ms":1800000}`, duration: 30 * time.Minute},
		{name: "no credits with null end", segment: "credits", stamp: `{"start_ms":0,"end_ms":null}`, duration: 30 * time.Minute},
		{name: "no credits with omitted end", segment: "credits", stamp: `{"start_ms":0}`, duration: 30 * time.Minute},
		{name: "no credits with zero end", segment: "credits", stamp: `{"start_ms":0,"end_ms":0}`, duration: 30 * time.Minute},
		{name: "no preview with null end", segment: "preview", stamp: `{"start_ms":0,"end_ms":null}`, duration: 30 * time.Minute},
		{name: "no preview with omitted end", segment: "preview", stamp: `{"start_ms":0}`, duration: 30 * time.Minute},
		{name: "no preview with zero end", segment: "preview", stamp: `{"start_ms":0,"end_ms":0}`, duration: 30 * time.Minute},
		{name: "negative start", segment: "intro", stamp: `{"start_ms":-1,"end_ms":60000}`, duration: 30 * time.Minute},
		{name: "negative end", segment: "recap", stamp: `{"start_ms":0,"end_ms":-1}`, duration: 30 * time.Minute},
		{name: "overflowing start", segment: "intro", stamp: `{"start_ms":9223372036854775807,"end_ms":60000}`, duration: 30 * time.Minute},
		{name: "overflowing end", segment: "intro", stamp: `{"start_ms":0,"end_ms":9223372036854775807}`, duration: 30 * time.Minute},
		{name: "reversed range", segment: "intro", stamp: `{"start_ms":60000,"end_ms":1000}`, duration: 30 * time.Minute},
		{name: "empty range", segment: "intro", stamp: `{"start_ms":60000,"end_ms":60000}`, duration: 30 * time.Minute},
		{name: "end exceeds duration", segment: "intro", stamp: `{"start_ms":0,"end_ms":1800001}`, duration: 30 * time.Minute},
		{name: "start exceeds duration", segment: "credits", stamp: `{"start_ms":1800001,"end_ms":1900000}`, duration: 30 * time.Minute},
		{name: "unknown file end", segment: "credits", stamp: `{"start_ms":1500000,"end_ms":null}`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			p := newProvider(t, fmt.Sprintf(`{"type":"tv",%q:[%s]}`, tt.segment, tt.stamp))
			req := episodeReq(map[string]string{ExternalIDKeyTMDB: "123"})
			req.Duration = tt.duration
			res, err := p.FetchMarkers(context.Background(), req)
			if err != nil {
				t.Fatalf("FetchMarkers: %v", err)
			}
			if !tt.valid {
				if len(res.Markers) != 0 {
					t.Fatalf("invalid bounds produced markers: %+v", res.Markers)
				}
				return
			}
			if len(res.Markers) != 1 {
				t.Fatalf("markers = %+v, want one marker", res.Markers)
			}
			if got := res.Markers[0]; got.Start != tt.start || got.End != tt.end {
				t.Fatalf("range = %v–%v, want %v–%v", got.Start, got.End, tt.start, tt.end)
			}
		})
	}
}
