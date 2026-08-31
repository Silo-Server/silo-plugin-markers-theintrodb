# Contributing to TheIntroDB Marker Plugin

The [Silo contribution guide](https://github.com/Silo-Server/.github/blob/main/CONTRIBUTING.md)
covers project-wide coordination, focused changes, evidence, AI disclosure, and
pull request expectations. Those requirements apply here; this guide adds the
plugin-specific workflow.

## Before you start

Open an [issue](https://github.com/Silo-Server/silo-plugin-markers-theintrodb/issues)
before changing marker semantics, submission behavior, configuration, or the
advertised capability. This repository owns the TheIntroDB adapter; contract
changes belong in
[`silo-plugin-sdk`](https://github.com/Silo-Server/silo-plugin-sdk), while host
marker orchestration belongs in
[`silo-server`](https://github.com/Silo-Server/silo-server).

## Development setup

Use the Go version declared in `go.mod`. A local `go.work` may point at a sibling
SDK checkout while developing both repositories, but committed code and CI must
resolve the tagged SDK dependency with `GOWORK=off`. Never commit a local
filesystem `replace` directive or an API key.

## Validate your change

```sh
GOWORK=off go test ./...
GOWORK=off go vet ./...
GOWORK=off go build ./...
gofmt -l .
```

`gofmt -l .` should print nothing. If it reports unrelated pre-existing drift,
none of the Go files touched by your change may appear in the output; do not add
to the output, and report what remains. Add focused coverage for marker
conversion, validation, upstream error mapping, and authenticated submissions
when those behaviors change.

## Open the pull request

Use a Conventional Commit title, explain any compatibility or marker-accuracy
risk, and paste the actual validation results. Read the
[AI-assisted contribution policy](https://github.com/Silo-Server/silo-server/blob/main/docs/ai-contributions.md)
and include its disclosure block.
