# TheIntroDB Marker Plugin for Silo

First-party Silo marker provider for [TheIntroDB](https://theintrodb.org).

The plugin implements `marker_provider.v1` against TheIntroDB API v4 and can
fetch or submit `intro`, `credits`, `recap`, and `preview` markers for movies
and TV episodes, including specials (season 0).

Each request addresses TheIntroDB by one ID, because TheIntroDB reads season
and episode numbers in the space of the ID it receives:

- A movie uses its TVDB ID, then TMDB, then IMDb.
- An episode whose row in Silo came from TVDB alone (a TVDB episode ID and no
  TMDB episode ID) uses TVDB's numbers, so it is addressed by the series' TVDB
  ID with `tvdb_episode_id` and the episode title as hints.
- Every other episode may follow TMDB's numbering, since Silo matches
  providers' episodes by number alone. It uses the series' TMDB ID, so
  TheIntroDB converts TMDB numbering, then TVDB, then IMDb.

Seasons above 1000 and episodes above 10000, which TheIntroDB refuses, are
not looked up, and submissions it can never accept are reported to Silo as
permanent refusals.

Malformed IDs are skipped. Lookups ask for TheIntroDB's per-segment confidence
and submission count and pass them to Silo. Lookups work without an account;
submissions and account statistics need an API key from theintrodb.org.

Each returned segment is preserved, including separate credits ranges around a
mid-credits scene. Lookup responses are cached for up to 24 hours in a bounded
cache; changing credentials or submitting a marker clears that cache. Quota
responses include the upstream reset delay so Silo can schedule the next
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
