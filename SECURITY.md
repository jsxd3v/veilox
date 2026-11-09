# Security policy

## Supported versions

Security fixes are released for the latest minor version.

## Reporting a vulnerability

Please **do not** open a public issue. Report it privately through GitHub:
[Security → Report a vulnerability](https://github.com/jsxd3v/veilox/security/advisories/new).

Include the version (`veilox version`), how to reproduce it and the impact you see. Use made-up data in
your report, never real personal data. You'll get an answer within a few days, and credit in the release
notes if you'd like.

A detector that misses a value (a phone format, a name not on your list) is a limitation, not a
vulnerability: please open a normal issue with a made-up example. A way to make veilox send a value it
*did* detect to the provider, write it to disk or logs, or leak it to another request is a vulnerability.

## How veilox handles personal data

- **Mapping in memory, per request.** The placeholder ↔ value mapping is created for one request and
  dropped when it ends. It is never written to disk, logs, the database or error messages, and never
  shared between requests.
- **Fail closed.** A request veilox can't parse is refused, never forwarded unmasked.
- **No values at rest.** The database and logs hold counts per type (`{"EMAIL":2}`), token counts and cost.
  No prompt text, answer text, detected value, header or key. Provider error messages, which can echo the
  prompt, are not logged; only their type is. The integration tests check that known values never reach
  the fake provider, the logs or the database file.
- **Not masked:** images, audio, files and PDFs inside requests (`unsupported_content: reject` refuses
  them), Anthropic thinking blocks (signed by the provider), and anything the detectors don't recognise.
  See the README's "What veilox does NOT protect against".

## How veilox protects itself

- App keys are stored as SHA-256 hashes; lookups hash the presented key and compare it with every app in
  constant time. Keys are never logged.
- Provider keys are read from environment variables only.
- Request bodies are limited (`server.max_body_bytes`, default 10 MiB), non-streaming provider responses
  to 32 MiB. Header, body-read, idle and per-request upstream timeouts are set; a closed client connection
  cancels the provider request.
- Custom rules use Go's RE2 engine: matching time is linear, so a rule can't freeze the gateway.
- Panics are recovered without logging request data.
- The Docker image is distroless and runs as a non-root user; it works with a read-only root filesystem.
- veilox serves plain HTTP. Keep it on a private network, or put it behind a TLS proxy (Caddy, Nginx, a
  load balancer).
- Releases include SHA-256 checksums. CI runs `govulncheck` and `gosec` (through golangci-lint).
