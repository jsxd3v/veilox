# Contributing to veilox

Thanks for helping! Bug reports, detector ideas, docs fixes and pull requests are all welcome.

## Development setup

You need Go 1.26 or newer. No API keys: tests use a fake provider and never call a real API.

```bash
go test -race ./...
golangci-lint run          # https://golangci-lint.run, v2
go build ./cmd/veilox
```

Before opening a pull request, run the same checks as CI:

```bash
go test -race -coverpkg=./internal/... -coverprofile=coverage.out ./...
go tool cover -func=coverage.out | tail -1          # must stay at 80% or more
go test -run WorstCase -v ./internal/detect         # detector speed, without -race
golangci-lint run ./...
go mod tidy -diff
```

If you add or update a dependency, regenerate the notices: `./scripts/third-party-notices.sh`.

## Try it locally without a key

```bash
go run ./examples/fake-provider &          # pretends to be OpenAI and Anthropic on :9999
# point providers.*.base_url in your veilox.yaml at http://127.0.0.1:9999, then:
go run ./cmd/veilox serve
```

The fake provider prints every prompt it receives, so you can see exactly what a provider would see.

## Code map

```
cmd/veilox/          entry point
internal/detect/     detectors and overlap rules
internal/mask/       per-request mapping, restore, stream restorer
internal/proxy/      HTTP server, OpenAI and Anthropic adapters, SSE
internal/config/     veilox.yaml
internal/pricing/    prices.yaml and cost
internal/budget/     today's spend per app
internal/store/      SQLite: request rows, reports
internal/auth/       app keys
internal/cli/        commands
internal/fakeprovider/  fake OpenAI/Anthropic for tests and demos
```

## Adding a detector

1. Add it in `internal/detect/builtin.go` with a type name, and register it in `detect.go` (priority
   order and the `BuiltinNames` map).
2. Add table tests with matches **and** near-misses (wrong checksum, too short, glued to a word, dates,
   IDs). False positives matter: a masked order number confuses the model.
3. Add worst-case inputs to `perf_test.go` if it scans digits or long runs.
4. Document it in `docs/detectors.md` and the README table.

Detectors must be deterministic and must not call out to anything.

## Pull requests

- Keep changes focused; one feature or fix per pull request.
- Add or update tests and docs, and a line in `CHANGELOG.md` under "Unreleased".
- Use made-up data only (`example.com`, test card numbers like `4111 1111 1111 1111`, `DE89 3704 0044 0532 0130 00`).
- Commit messages: short and in plain English.

## Releasing (maintainers)

Update `CHANGELOG.md` (move "Unreleased" to the new version), then tag: `git tag v0.1.0 && git push --tags`.
The release workflow runs the tests, publishes binaries with GoReleaser and pushes the Docker image.
