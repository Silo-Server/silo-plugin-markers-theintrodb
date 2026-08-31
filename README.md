# TheIntroDB Marker Plugin for Silo

First-party Silo marker provider for [TheIntroDB](https://theintrodb.org).

The plugin implements `marker_provider.v1` and can fetch or submit `intro`,
`credits`, `recap`, and `preview` markers for movies and TV episodes.

The manifest asks Silo for a TMDB external ID. The fetch implementation can
address TheIntroDB with a TMDB, TVDB, or IMDb ID and works without an account;
submissions specifically require a TMDB ID and API key. Account statistics also
require an API key.

## Configuration

The `account` global config accepts an optional `api_key` string. Fetches work
without a key; submissions and account statistics require one.

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
