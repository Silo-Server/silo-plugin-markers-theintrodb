package provider

import (
	"net/url"
	"testing"
)

func TestNewTargetChoosesTheIDTheNumbersBelongTo(t *testing.T) {
	got := map[string]string{ExternalIDKeyTMDB: "1399", ExternalIDKeyTVDB: "121361", ExternalIDKeyIMDB: "tt0944947"}
	bothIDs := map[string]string{ExternalIDKeyTVDB: "3254641", ExternalIDKeyTMDB: "63056"}
	for _, tt := range []struct {
		name    string
		kind    ItemKind
		ids     map[string]string
		episode Episode
		want    Target
		ok      bool
	}{
		{
			name:    "TVDB numbering uses the series TVDB ID and pins the episode",
			kind:    ItemKindEpisode,
			ids:     got,
			episode: Episode{ExternalIDs: bothIDs, Numbering: "tvdb", Title: "Winter Is Coming"},
			want:    Target{Param: "tvdb_id", ID: "121361", TVDBEpisodeID: "3254641"},
			ok:      true,
		},
		{
			name:    "TMDB numbering lets TheIntroDB convert TMDB numbers",
			kind:    ItemKindEpisode,
			ids:     got,
			episode: Episode{ExternalIDs: bothIDs, Numbering: "tmdb", Title: "Winter Is Coming"},
			want:    Target{Param: "tmdb_id", ID: "1399"},
			ok:      true,
		},
		{
			name:    "numbering is case-insensitive",
			kind:    ItemKindEpisode,
			ids:     got,
			episode: Episode{ExternalIDs: bothIDs, Numbering: " TVDB "},
			want:    Target{Param: "tvdb_id", ID: "121361", TVDBEpisodeID: "3254641"},
			ok:      true,
		},
		{
			name:    "TVDB numbering without the episode's TVDB ID sends the title instead of a pin",
			kind:    ItemKindEpisode,
			ids:     got,
			episode: Episode{Numbering: "tvdb", Title: " Pilot "},
			want:    Target{Param: "tvdb_id", ID: "121361", EpisodeName: "Pilot"},
			ok:      true,
		},
		{
			// TVDB would read TMDB's numbers as its own, naming another
			// episode wherever the numberings differ.
			name:    "TMDB numbering without a series TMDB ID has no target",
			kind:    ItemKindEpisode,
			ids:     map[string]string{ExternalIDKeyTVDB: "121361", ExternalIDKeyIMDB: "tt0944947"},
			episode: Episode{ExternalIDs: bothIDs, Numbering: "tmdb"},
		},
		{
			name:    "TVDB numbering without a series TVDB ID has no target",
			kind:    ItemKindEpisode,
			ids:     map[string]string{ExternalIDKeyTMDB: "1399", ExternalIDKeyIMDB: "tt0944947"},
			episode: Episode{ExternalIDs: bothIDs, Numbering: "tvdb"},
		},
		{
			// A host that predates episode_numbering: only an episode TVDB
			// supplied without TMDB is known to follow TVDB's numbers.
			name:    "older host, episode only TVDB supplied",
			kind:    ItemKindEpisode,
			ids:     got,
			episode: Episode{ExternalIDs: map[string]string{ExternalIDKeyTVDB: "3254641"}},
			want:    Target{Param: "tvdb_id", ID: "121361", TVDBEpisodeID: "3254641"},
			ok:      true,
		},
		{
			name:    "older host, episode both providers supplied",
			kind:    ItemKindEpisode,
			ids:     got,
			episode: Episode{ExternalIDs: bothIDs},
			want:    Target{Param: "tmdb_id", ID: "1399"},
			ok:      true,
		},
		{
			name:    "unknown numbering reads as empty",
			kind:    ItemKindEpisode,
			ids:     got,
			episode: Episode{ExternalIDs: bothIDs, Numbering: "anidb"},
			want:    Target{Param: "tmdb_id", ID: "1399"},
			ok:      true,
		},
		{
			name: "series known only to TVDB",
			kind: ItemKindEpisode,
			ids:  map[string]string{ExternalIDKeyTVDB: "121361", ExternalIDKeyIMDB: "tt0944947"},
			want: Target{Param: "tvdb_id", ID: "121361"},
			ok:   true,
		},
		{
			name:    "IMDb is the last resort when the numbering is unknown, and carries the title",
			kind:    ItemKindEpisode,
			ids:     map[string]string{ExternalIDKeyIMDB: "tt0944947"},
			episode: Episode{Title: "Pilot"},
			want:    Target{Param: "imdb_id", ID: "tt0944947", EpisodeName: "Pilot"},
			ok:      true,
		},
		{
			name: "movie prefers TVDB",
			kind: ItemKindMovie,
			ids:  map[string]string{ExternalIDKeyTMDB: "603", ExternalIDKeyTVDB: "169"},
			want: Target{Param: "tvdb_id", ID: "169"},
			ok:   true,
		},
		{
			name:    "movie ignores episode hints",
			kind:    ItemKindMovie,
			ids:     map[string]string{ExternalIDKeyTMDB: "603"},
			episode: Episode{ExternalIDs: map[string]string{ExternalIDKeyTVDB: "1"}, Numbering: "tvdb", Title: "not an episode"},
			want:    Target{Param: "tmdb_id", ID: "603"},
			ok:      true,
		},
		{
			name: "malformed IDs are skipped instead of drawing a 422",
			kind: ItemKindMovie,
			ids:  map[string]string{ExternalIDKeyTVDB: "abc", ExternalIDKeyTMDB: "99999999999", ExternalIDKeyIMDB: " tt0111161 "},
			want: Target{Param: "imdb_id", ID: "tt0111161"},
			ok:   true,
		},
		{
			name:    "a malformed episode TVDB ID is no pin",
			kind:    ItemKindEpisode,
			ids:     got,
			episode: Episode{ExternalIDs: map[string]string{ExternalIDKeyTVDB: "-4"}, Numbering: "tvdb"},
			want:    Target{Param: "tvdb_id", ID: "121361"},
			ok:      true,
		},
		{
			name: "IDs are canonicalized",
			kind: ItemKindMovie,
			ids:  map[string]string{ExternalIDKeyTVDB: " 0169 "},
			want: Target{Param: "tvdb_id", ID: "169"},
			ok:   true,
		},
		{
			name: "no usable ID",
			kind: ItemKindEpisode,
			ids:  map[string]string{ExternalIDKeyIMDB: "tt1", ExternalIDKeyTVDB: "0"},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			target, ok := NewTarget(tt.kind, tt.ids, tt.episode)
			if ok != tt.ok || target != tt.want {
				t.Fatalf("NewTarget = %+v, %v; want %+v, %v", target, ok, tt.want, tt.ok)
			}
		})
	}
}

func TestTargetQueryNamesOneID(t *testing.T) {
	q := url.Values{}
	Target{Param: "tvdb_id", ID: "121361", TVDBEpisodeID: "3254641", EpisodeName: "Winter Is Coming"}.addTo(q)
	want := url.Values{"tvdb_id": {"121361"}, "tvdb_episode_id": {"3254641"}, "episode_name": {"Winter Is Coming"}}
	if q.Encode() != want.Encode() {
		t.Fatalf("query = %s, want %s", q.Encode(), want.Encode())
	}
}
