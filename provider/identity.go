package provider

import (
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

// TheIntroDB's documented ID and coordinate ranges. A value outside them is
// refused, so it is treated as missing instead of spending a request.
const (
	maxTVDBID  = 100_000_000
	maxTMDBID  = 10_000_000
	maxSeason  = 1000
	maxEpisode = 10000
)

// validCoordinates reports whether TheIntroDB accepts an episode's numbers.
// Season 0 holds specials. TheIntroDB also accepts episode 0, but Silo sends
// 0 for an episode it could not number, so episode 0 is refused here.
func validCoordinates(season, episode int) bool {
	return season >= 0 && season <= maxSeason && episode > 0 && episode <= maxEpisode
}

var imdbIDPattern = regexp.MustCompile(`^tt[0-9]{7,8}$`)

// Target is the one media ID a request addresses TheIntroDB by, plus the
// optional hints that refine a TV episode. TheIntroDB reads season and episode
// numbers in the space of the ID it is given: TVDB's default order for
// tvdb_id, TMDB's numbering for tmdb_id. TVDBEpisodeID is documented to pin
// the record on submissions; lookups ignore it and EpisodeName today.
type Target struct {
	Param         string // "tvdb_id", "tmdb_id" or "imdb_id"
	ID            string
	TVDBEpisodeID string
	EpisodeName   string
}

// NewTarget chooses how to address an item; ok is false when it has no usable
// ID.
//
// v4 keys media by TVDB and resolves TMDB and IMDb IDs server-side, so a movie,
// which has no numbering, prefers its TVDB ID. An episode is addressed by the
// series ID of the provider whose numbering it follows: Silo reports it in
// episode.Numbering. Without that provider's series ID the episode has no
// target, since another ID would name a different episode wherever the
// numberings differ. A host that predates that field leaves it empty, and then
// only an episode TVDB supplied without TMDB is known to use TVDB's numbers.
// Any other episode may follow TMDB's numbering, so tmdb_id lets TheIntroDB
// convert it, with TVDB and then IMDb as fallbacks.
func NewTarget(kind ItemKind, ids map[string]string, episode Episode) (Target, bool) {
	tvdb := validNumericID(ids[ExternalIDKeyTVDB], maxTVDBID)
	tmdb := validNumericID(ids[ExternalIDKeyTMDB], maxTMDBID)
	imdb := strings.TrimSpace(ids[ExternalIDKeyIMDB])
	if !imdbIDPattern.MatchString(imdb) {
		imdb = ""
	}

	tvdbNumbered := false
	episodeTVDB := ""
	if kind == ItemKindEpisode {
		episodeTVDB = validNumericID(episode.ExternalIDs[ExternalIDKeyTVDB], 0)
		switch strings.ToLower(strings.TrimSpace(episode.Numbering)) {
		case ExternalIDKeyTVDB:
			if tvdb == "" {
				return Target{}, false
			}
			tvdbNumbered = true
		case ExternalIDKeyTMDB:
			if tmdb == "" {
				return Target{}, false
			}
		default:
			// An older host, or a numbering this plugin does not know.
			tvdbNumbered = episodeTVDB != "" && strings.TrimSpace(episode.ExternalIDs[ExternalIDKeyTMDB]) == ""
		}
	}

	var t Target
	preferTVDB := kind == ItemKindMovie || tvdbNumbered
	switch {
	case tvdb != "" && (preferTVDB || tmdb == ""):
		t = Target{Param: "tvdb_id", ID: tvdb}
	case tmdb != "":
		t = Target{Param: "tmdb_id", ID: tmdb}
	case imdb != "":
		t = Target{Param: "imdb_id", ID: imdb}
	default:
		return Target{}, false
	}
	if kind == ItemKindEpisode {
		// The pin names a record in TVDB's numbering, so it goes only with a
		// tvdb_id request for a TVDB-numbered episode. The title only
		// corroborates an episode the request cannot pin, and TheIntroDB
		// ignores it for tmdb_id.
		if t.Param == "tvdb_id" && tvdbNumbered {
			t.TVDBEpisodeID = episodeTVDB
		}
		if t.TVDBEpisodeID == "" && t.Param != "tmdb_id" {
			t.EpisodeName = strings.TrimSpace(episode.Title)
		}
	}
	return t, true
}

// addTo writes the target as GET /media query parameters.
func (t Target) addTo(q url.Values) {
	q.Set(t.Param, t.ID)
	if t.TVDBEpisodeID != "" {
		q.Set("tvdb_episode_id", t.TVDBEpisodeID)
	}
	if t.EpisodeName != "" {
		q.Set("episode_name", t.EpisodeName)
	}
}

// applyTo sets the target's fields on a POST /submit body.
func (t Target) applyTo(body *submitRequest) {
	switch t.Param {
	case "tvdb_id":
		body.TvdbID = parsedID(t.ID)
	case "tmdb_id":
		body.TmdbID = parsedID(t.ID)
	case "imdb_id":
		body.ImdbID = t.ID
	}
	body.TvdbEpisodeID = parsedID(t.TVDBEpisodeID)
	body.EpisodeName = t.EpisodeName
}

// validNumericID returns the canonical form of a positive integer ID no
// larger than limit (0 means no limit), or "" when value is not one.
func validNumericID(value string, limit int64) string {
	id, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
	if err != nil || id <= 0 || (limit > 0 && id > limit) {
		return ""
	}
	return strconv.FormatInt(id, 10)
}

func parsedID(value string) *int64 {
	if value == "" {
		return nil
	}
	id, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		return nil
	}
	return &id
}
