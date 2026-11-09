# Prepares the demo recorded in docs/demo.gif. Run from the repo root: source docs/demo/setup.sh
# Starts the fake provider and veilox in a temporary folder and adds a week of sample spend.
# Everything is made up. `demo_stop` stops both servers.

DEMO_ROOT=$(pwd)
DEMO=$(mktemp -d)
mkdir -p "$DEMO/bin" "$DEMO/data"
go build -o "$DEMO/bin/veilox" ./cmd/veilox || return 1
go build -o "$DEMO/bin/fake-provider" ./examples/fake-provider || return 1
cp examples/prices.yaml examples/names.txt docs/demo/ticket.txt docs/demo/request.json "$DEMO/"
cd "$DEMO" || return 1
export PATH="$DEMO/bin:$PATH"

VEILOX_KEY=$(veilox keygen support-bot | grep -o 'vx_[A-Za-z0-9]*')
CRM_KEY=$(veilox keygen crm-summaries | grep -o 'vx_[A-Za-z0-9]*')
export VEILOX_KEY
cat > veilox.yaml <<YAML
server:
  listen: 127.0.0.1:8080
providers:
  openai: { base_url: http://127.0.0.1:9999 }
  anthropic: { base_url: http://127.0.0.1:9999 }
apps:
  - { name: support-bot, key_sha256: $(printf %s "$VEILOX_KEY" | shasum -a 256 | cut -d' ' -f1), daily_budget_usd: 5 }
  - { name: crm-summaries, key_sha256: $(printf %s "$CRM_KEY" | shasum -a 256 | cut -d' ' -f1), daily_budget_usd: 20 }
masking:
  names_file: names.txt
  rules:
    - { name: CUSTOMER_ID, pattern: 'CUS-[0-9]{6}' }
storage: { path: data/veilox.db }
YAML

fake-provider --listen 127.0.0.1:9999 > provider.log 2>/dev/null &
FAKE_PID=$!
OPENAI_API_KEY=demo ANTHROPIC_API_KEY=demo veilox serve > veilox.log 2>&1 &
VEILOX_PID=$!
until curl -sf http://127.0.0.1:8080/healthz >/dev/null; do sleep 0.2; done

# A week of made-up traffic: support-bot on Claude, crm-summaries on GPT. Cost uses the example prices.
seed() { # day app provider model requests input_per_req output_per_req input_price output_price masked_per_req
  sqlite3 data/veilox.db "WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM n WHERE i < $5)
    INSERT INTO requests (ts, day, app, provider, model, stream, status, input_tokens, output_tokens, cache_read_tokens,
      cache_write_tokens, cost_micros, latency_ms, ttfb_ms, masked, masked_total, unmasked_parts, error_type)
    SELECT '$1T12:00:00Z', '$1', '$2', '$3', '$4', 1, CASE WHEN i % 97 = 0 THEN 529 ELSE 200 END,
      $6 + (i % 7) * 11, $7 + (i % 5) * 9, 0, 0,
      CAST(ROUND(($6 + (i % 7) * 11) * $8 + ($7 + (i % 5) * 9) * $9) AS INTEGER),
      900, 300, '{\"EMAIL\":1,\"NAME\":1}', ${10}, 0, '' FROM n;"
}
for d in 1 2 3 4 5 6; do
  day=$(date -u -v-"${d}"d +%Y-%m-%d 2>/dev/null || date -u -d "-$d day" +%Y-%m-%d)
  seed "$day" support-bot anthropic claude-sonnet-5-5 $((380 + d * 7)) 740 215 1.00 5.00 $((2 + d % 2))
  seed "$day" crm-summaries openai gpt-6.1-terra $((70 + d * 5)) 1380 340 1.00 4.00 3
done
# Restart so veilox loads the seeded spend into its budget totals.
kill "$VEILOX_PID"; wait "$VEILOX_PID" 2>/dev/null
OPENAI_API_KEY=demo ANTHROPIC_API_KEY=demo veilox serve > veilox.log 2>&1 &
VEILOX_PID=$!
until curl -sf http://127.0.0.1:8080/healthz >/dev/null; do sleep 0.2; done
: > provider.log

demo_stop() { kill "$VEILOX_PID" "$FAKE_PID" 2>/dev/null; wait 2>/dev/null; cd "$DEMO_ROOT" || return; rm -rf "$DEMO"; }
