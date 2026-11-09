# Changelog

All notable changes are listed here. The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/)
and the project uses [Semantic Versioning](https://semver.org/).

## [Unreleased]

## [0.1.0] - 2026-11-09

First release.

### Added
- Gateway for OpenAI-compatible `POST /v1/chat/completions` and Anthropic `POST /v1/messages`, JSON and
  streaming (SSE). Apps only change their base URL and key.
- Masking before the request leaves: `EMAIL`, `PHONE`, `CREDIT_CARD` (Luhn), `IBAN` (mod-97, registry
  lengths), `IPV4`, a list of known names, and custom RE2 rules (optionally one capture group).
  Deterministic overlap rules; the same value gets the same placeholder across a request.
- Restoring real values in answers, tool-call arguments (JSON-escaped, also when brackets arrive as
  `<…>`) and provider error messages. In streams, placeholders split across chunks are held
  back until complete.
- Per-app keys (SHA-256 hashes in `veilox.yaml`, constant-time lookup); provider keys stay in veilox's
  environment.
- Cost tracking in SQLite (pure Go, no CGO) from an editable `prices.yaml` with prefix matching and cache
  token prices. Daily budgets per app with a `429`, `Retry-After` and an error in the SDK's format.
- `masking.unsupported_content` (forward or reject images and files), `masking.system_hint`,
  `pricing.unknown_model`.
- `veilox setup [app]`: creates `.env` with a new app key (`VEILOX_KEY`) and an example `prices.yaml`
  (or fills in a copied `.env.example`).
  veilox reads `.env` itself, and `veilox.yaml` is optional, so one app needs no YAML at all.
- Commands: `serve` (graceful shutdown, reload on `SIGHUP`, daily retention cleanup), `report` (table, CSV,
  JSON), `scan` (dry run on text or a request body), `keygen`, `check`, `version`.
- One JSON log line per request with counts per type, never values.
- Binaries for macOS, Linux and Windows (amd64, arm64) and a distroless, non-root Docker image on
  `ghcr.io/jsxd3v/veilox`.

[Unreleased]: https://github.com/jsxd3v/veilox/compare/v0.1.0...HEAD
[0.1.0]: https://github.com/jsxd3v/veilox/releases/tag/v0.1.0
