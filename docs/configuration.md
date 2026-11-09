# Configuration

## The quick way: `.env` only

For one app you don't need any YAML. `veilox setup` writes a `.env` like this (or copy
[`.env.example`](../.env.example) and run `veilox setup` to fill in the key):

```bash
OPENAI_API_KEY=sk-...          # fill in the provider key you use
ANTHROPIC_API_KEY=
VEILOX_KEY=vx_...              # the key your app uses to call veilox
```

| Variable | Default | |
|---|---|---|
| `OPENAI_API_KEY` / `ANTHROPIC_API_KEY` | none | Provider keys (names can be changed with `providers.*.api_key_env`) |
| `VEILOX_KEY` | none | One app key. At least 20 characters; `veilox setup` makes a random 43-character one |
| `VEILOX_APP` | `default` | That app's name in logs and reports (`veilox setup llmvero` sets it) |
| `VEILOX_LISTEN` | `:8080` | Address to listen on |
| `VEILOX_DB` | `./data/veilox.db` | Database file |
| `VEILOX_CONFIG` | `./veilox.yaml` | Path of the optional YAML config |

veilox reads `.env` from the current folder (and from the folder of `veilox.yaml`, if that's elsewhere).
Lines are `KEY=value`; `#` comments, `export` and quotes are allowed. Variables already set in the
environment win over `.env`. veilox warns if other users can read the file (`chmod 600 .env`).

`VEILOX_KEY` is stored as plain text in `.env`, so keep that file out of Git. With `veilox.yaml`, keys are
stored as SHA-256 hashes instead.

## veilox.yaml

For several apps, budgets, names, custom rules or another provider URL, add `veilox.yaml` next to `.env`.
Both work together: apps from `veilox.yaml` and the one from `VEILOX_KEY`. Validate everything with
`veilox check`.

- The config path is `--config`, or `VEILOX_CONFIG`, or `./veilox.yaml`. Without `--config`, a missing
  file is fine: veilox runs on the defaults below.
- Relative paths inside the config (`names_file`, `storage.path`, `pricing.file`) are resolved from the
  folder that holds `veilox.yaml`, not from where you start veilox.
- Unknown keys are an error, so a typo can't silently turn masking off.
- `SIGHUP` reloads both files while veilox runs. If a file is broken, veilox logs the error and keeps
  the running config. `server.listen`, `server.timezone` and `storage.path` need a restart.

## Full reference

```yaml
server:
  listen: ":8080"               # env VEILOX_LISTEN overrides it
  max_body_bytes: 10485760      # 10 MiB; larger requests get 413
  read_header_timeout: 10s
  upstream_timeout: 10m         # the whole provider call, streams included
  timezone: UTC                 # IANA name; when a budget day and a report day start

providers:
  openai:
    base_url: https://api.openai.com
    api_key_env: OPENAI_API_KEY
  anthropic:
    base_url: https://api.anthropic.com
    api_key_env: ANTHROPIC_API_KEY

apps:
  - name: support-bot
    key_sha256: 3b1f…           # from `veilox keygen support-bot`
    daily_budget_usd: 5         # optional; omit for no limit

masking:
  detectors: [email, phone, credit_card, iban, ipv4]   # default: all five
  ipv4:
    skip_private: false         # true leaves 10/8, 172.16/12, 192.168/16, 127/8 unmasked
  names: ["Maria Santos"]
  names_file: names.txt         # one name per line, # comments allowed
  rules:
    - name: CUSTOMER_ID
      pattern: 'CUS-[0-9]{6}'
    - name: POLICY_NO
      pattern: 'policy no\. ([A-Z]{2}[0-9]{8})'
      group: 1                  # mask only this capture group
  unsupported_content: forward  # forward | reject
  system_hint: false

storage:
  path: ./data/veilox.db        # env VEILOX_DB overrides it
  retention_days: 90            # 0 keeps everything

pricing:
  file: ./prices.yaml
  unknown_model: allow          # allow | reject

log:
  level: info                   # debug | info | warn | error
  format: json                  # json | text
```

## Providers

veilox serves two endpoints and forwards each to one provider:

| veilox endpoint | Forwarded to |
|---|---|
| `POST /v1/chat/completions` | `providers.openai.base_url` + `/v1/chat/completions` |
| `POST /v1/messages` | `providers.anthropic.base_url` + `/v1/messages` |

`providers.openai` works with any API that speaks OpenAI Chat Completions: OpenRouter
(`https://openrouter.ai/api`), Groq (`https://api.groq.com/openai`), Together, vLLM, Ollama
(`http://localhost:11434`)… A trailing `/v1` in `base_url` is accepted and ignored, so you can paste
the URL from an SDK example.

Provider keys are read from the environment variable named in `api_key_env`. They never appear in the
config, logs or responses. A missing key makes calls to that provider fail with `502`.

## Apps and keys

Each app gets its own key. veilox stores only its SHA-256, so `veilox.yaml` can live in Git.

```bash
$ veilox keygen support-bot
App key for "support-bot" (shown once; give it to the app, keep it out of Git):

  vx_7Qm2…

Add this to the apps list in veilox.yaml (only the hash is stored):

  - name: support-bot
    key_sha256: 3b1f…
```

Apps send the key the way their SDK already does: `Authorization: Bearer vx_…` (OpenAI SDKs) or
`x-api-key: vx_…` (Anthropic SDKs). Both headers work on both endpoints.

To rotate a key, run `keygen` again, replace the hash and reload with `SIGHUP`.

## Budgets

`daily_budget_usd` limits what an app can spend per calendar day in `server.timezone`.

- Before forwarding, veilox compares the app's spend today with its budget. Once it's reached, every
  request gets `429` with `Retry-After` (seconds until midnight) and an error in the format the app's SDK
  expects.
- It is a **soft limit**: a request that starts under budget is allowed to finish, and several requests
  in flight at once can push the total a little over.
- Spend is kept in memory and loaded from the database at startup, so a restart doesn't reset it.
- Requests to models without a price don't count (their cost is unknown). Set
  `pricing.unknown_model: reject` if every call must be counted.
- Budgets are per veilox instance. Several instances don't share a budget yet.

## Prices

```yaml
# prices.yaml: USD per 1 million tokens
currency: USD
models:
  gpt-6.1-terra:
    input: 1.00
    cached_input: 0.25          # cached prompt tokens (defaults to input)
    output: 4.00
  "claude-sonnet-5-5*":         # trailing * = prefix: matches dated versions too
    input: 1.00
    cache_write: 1.25           # writing to the prompt cache (defaults to input)
    cache_read: 0.10            # same meaning as cached_input
    output: 5.00
```

**The example file ships with made-up round numbers. Check your provider's pricing page and edit it.**
veilox never fetches prices.

- Lookup: exact name first, then the longest `*` prefix. veilox tries the model name in the provider's
  response first, then the one the app asked for.
- Token counts come from the provider's `usage` field. For OpenAI streams veilox asks for usage
  (`stream_options.include_usage`) and removes that extra chunk again if your app didn't ask for it.
- Cost is stored in micro-dollars (1 USD = 1,000,000), so sums don't drift.
- A model missing from the table is still proxied. Its cost is recorded as unknown, `veilox report`
  marks it with `*` and lists it, and veilox logs a warning once per model per day.

## Masking

See [detectors.md](detectors.md) for the exact rules of each detector and how overlaps are resolved.

- `detectors` turns built-in detectors on or off. `names` and `rules` are always used when present.
- `names` and `names_file` are combined. Names are matched case-insensitively and as whole words.
- `rules` use Go's RE2 syntax. RE2 runs in linear time, so no pattern can freeze the gateway. It has no
  lookarounds or backreferences. Rule names must be upper case (`CUSTOMER_ID`) and become the placeholder
  type (`<CUSTOMER_ID_1>`).
- `unsupported_content`: images, audio, files and PDFs inside a request can't be masked.
  `forward` sends them unchanged and logs `unmasked_parts`; `reject` refuses the request with `400`.
- `system_hint: true` adds one line to the system prompt when something was masked, asking the model to
  write placeholders exactly as given. It helps models that would otherwise write `<Name_1>`.

## Storage and logs

One row per request in SQLite: time, app, provider, model, status, token counts, cost, latency, and how
many values were masked per type. **No prompt text, answer text or detected value is stored or
logged.** Rows older than `retention_days` are deleted at startup and then daily.

Each request also writes one log line to stderr:

```json
{"level":"INFO","msg":"request","request_id":"6eaa70cd59ac2ac2","app":"support-bot","provider":"openai",
 "model":"gpt-6.1-terra","status":200,"stream":false,"latency_ms":812,"ttfb_ms":790,"input_tokens":41,
 "output_tokens":38,"cost_usd":0.000193,"masked":{"CREDIT_CARD":1,"EMAIL":1,"NAME":1,"PHONE":1}}
```

`request_id` is also sent back as the `X-Veilox-Request-Id` response header.

## Reports

```bash
veilox report                                  # last 7 days by day, app and model
veilox report --since 30d --by app             # one line per app
veilox report --since 2026-11-01 --until 2026-11-30 --app support-bot --by model
veilox report --format csv > spend.csv         # or --format json
```

`--since` takes `Nd` (the last N days, today included), a date, or `all`. `ERRORS` counts requests that
ended with status 400 or higher, including `499` (the app closed the connection).
