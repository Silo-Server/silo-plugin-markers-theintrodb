# TheIntroDB Marker Plugin for Silo

First-party Silo marker provider for [TheIntroDB](https://theintrodb.org).

The plugin implements `marker_provider.v1` against TheIntroDB API v4 and can
fetch or submit `intro`, `credits`, `recap`, and `preview` markers for movies
and TV episodes, including specials (season 0).

Each request addresses TheIntroDB by one ID, because TheIntroDB reads season
and episode numbers in the space of the ID it receives:

- A movie uses its TVDB ID, then TMDB, then IMDb.
- An episode uses the series ID of the provider whose numbering it follows,
  which Silo reports as `episode_numbering`: the first TVDB or TMDB provider in
  the library's episode chain that supplied the episode. A TVDB-numbered
  episode also sends its TVDB episode ID as `tvdb_episode_id`.
- On a host that predates `episode_numbering`, an episode Silo matched on TVDB
  alone is treated as TVDB-numbered, and any other episode uses the series'
  TMDB ID so TheIntroDB converts TMDB numbering.

Malformed IDs, seasons above 1000, and episodes above 10000 are not looked up.
Lookups ask for TheIntroDB's per-segment confidence and submission count. A
segment without a confidence (or with one outside 0-1) is reported with
confidence 0, so it ranks below any rated marker and Silo uses it only when no
other provider has a marker for that section. A fetch confidence floor above 0
on this provider drops such segments entirely. When
TheIntroDB answers with a release more than five minutes longer or shorter than
the file, its credits and previews are dropped, since they would land in the
wrong place; intros and recaps are kept.

Submissions that TheIntroDB would read as "no segment" (credits or a preview
starting at 0, an intro or recap ending at 0, an empty range) or cannot accept
are refused locally, and Silo records them as permanent refusals. After
TheIntroDB rejects the API key, submissions stop for an hour or until the key
changes or validates.

Each returned segment is preserved, including separate credits ranges around a
mid-credits scene. Lookup responses are cached for up to 24 hours in a bounded
cache; changing credentials or submitting a marker clears that cache. The
lookup rate follows the ceiling TheIntroDB reports in `X-RateLimit-Limit`.
Quota responses include the upstream reset delay so Silo can schedule the next
lookup. Reaching TheIntroDB's daily limit for a single title fails only that
lookup instead of pausing the provider.

## Configuration

The `account` global config accepts an optional `api_key` string from a
theintrodb.org account. Fetches work without a key; a key raises the lookup
budget, and submissions and account statistics require one.

## Development

```sh
GOWORK=off go test ./...
make build
```

## Contributing

Read [CONTRIBUTING.md](CONTRIBUTING.md) before opening a pull request. Changes to
the marker contract must be coordinated with `silo-plugin-sdk` and
`silo-server`.

## License

`silo-plugin-markers-theintrodb` is licensed under `AGPL-3.0-only`. See
[LICENSE](LICENSE).
