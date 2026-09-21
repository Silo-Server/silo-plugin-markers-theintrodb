package provider

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
)

func TestFetchEpisodeSendsTVDBWhenNoTMDB(t *testing.T) {
	var gotQuery url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.Query()
		_, _ = w.Write([]byte(`{"type":"episode"}`))
	}))
	defer srv.Close()

	c := NewClient("")
	c.SetBaseURL(srv.URL)
	if _, err := c.FetchEpisode(context.Background(), "", "55555", "tt1234567", 2, 3, 0); err != nil {
		t.Fatalf("FetchEpisode: %v", err)
	}
	if gotQuery.Get("tvdb_id") != "55555" {
		t.Errorf("tvdb_id = %q, want 55555", gotQuery.Get("tvdb_id"))
	}
	if gotQuery.Get("tmdb_id") != "" {
		t.Errorf("tmdb_id = %q, want empty", gotQuery.Get("tmdb_id"))
	}
	if gotQuery.Get("imdb_id") != "" {
		t.Errorf("imdb_id should be omitted when tvdb present, got %q", gotQuery.Get("imdb_id"))
	}
}

func TestFetchEpisodePrefersTMDBOverTVDBAndIMDB(t *testing.T) {
	var gotQuery url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.Query()
		_, _ = w.Write([]byte(`{"type":"episode"}`))
	}))
	defer srv.Close()

	c := NewClient("")
	c.SetBaseURL(srv.URL)
	if _, err := c.FetchEpisode(context.Background(), "111", "222", "tt333", 1, 1, 0); err != nil {
		t.Fatalf("FetchEpisode: %v", err)
	}
	if gotQuery.Get("tmdb_id") != "111" {
		t.Errorf("tmdb_id = %q, want 111", gotQuery.Get("tmdb_id"))
	}
	if gotQuery.Get("tvdb_id") != "" || gotQuery.Get("imdb_id") != "" {
		t.Errorf("only tmdb_id expected, got tvdb=%q imdb=%q", gotQuery.Get("tvdb_id"), gotQuery.Get("imdb_id"))
	}
}

func TestFetchMovieSendsTVDB(t *testing.T) {
	var gotQuery url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.Query()
		_, _ = w.Write([]byte(`{"type":"movie"}`))
	}))
	defer srv.Close()

	c := NewClient("")
	c.SetBaseURL(srv.URL)
	if _, err := c.FetchMovie(context.Background(), "", "888", "", 0); err != nil {
		t.Fatalf("FetchMovie: %v", err)
	}
	if gotQuery.Get("tvdb_id") != "888" {
		t.Errorf("tvdb_id = %q, want 888", gotQuery.Get("tvdb_id"))
	}
}

func TestFetchEpisodeCachesByID(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&hits, 1)
		_, _ = w.Write([]byte(`{"type":"episode"}`))
	}))
	defer srv.Close()

	c := NewClient("")
	c.SetBaseURL(srv.URL)
	for i := 0; i < 3; i++ {
		if _, err := c.FetchEpisode(context.Background(), "", "999", "", 1, 1, 0); err != nil {
			t.Fatalf("FetchEpisode: %v", err)
		}
	}
	if hits != 1 {
		t.Fatalf("server hits = %d, want 1 (cached after first)", hits)
	}
}

// trackedBody records how much of a response body was consumed and whether it
// was closed, so the status branches can be checked without a live server.
type trackedBody struct {
	reader io.Reader
	read   int
	closed bool
}

func (b *trackedBody) Read(p []byte) (int, error) {
	n, err := b.reader.Read(p)
	b.read += n
	return n, err
}

func (b *trackedBody) Close() error {
	b.closed = true
	return nil
}

func TestCloseResponseDrainsAndClosesBody(t *testing.T) {
	body := &trackedBody{reader: strings.NewReader(strings.Repeat("x", 64))}
	closeResponse(&http.Response{Body: body})

	if !body.closed {
		t.Error("response body was not closed")
	}
	if body.read != 64 {
		t.Errorf("drained %d bytes, want 64 so the connection stays reusable", body.read)
	}
}

func TestCloseResponseToleratesMissingBody(t *testing.T) {
	closeResponse(nil)
	closeResponse(&http.Response{})
}
