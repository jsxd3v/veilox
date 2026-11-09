package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/jsxd3v/veilox/internal/auth"
	"github.com/jsxd3v/veilox/internal/fakeprovider"
	"github.com/jsxd3v/veilox/internal/pricing"
	"github.com/jsxd3v/veilox/internal/store"
)

func run(t *testing.T, stdin string, args ...string) (int, string, string) {
	t.Helper()
	var out, errb bytes.Buffer
	code := Run(args, strings.NewReader(stdin), &out, &errb)
	return code, out.String(), errb.String()
}

func writeFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

const key = "vx_clitest000000000000000000000000000000001"

func configDir(t *testing.T, upstream string) string {
	t.Helper()
	dir := t.TempDir()
	writeFile(t, dir, "veilox.yaml", fmt.Sprintf(`
server: { listen: "127.0.0.1:0" }
providers:
  openai: { base_url: %[1]s }
  anthropic: { base_url: %[1]s }
apps:
  - { name: support-bot, key_sha256: %[2]s, daily_budget_usd: 5 }
  - { name: crm, key_sha256: %[3]s }
masking:
  names_file: names.txt
  rules: [{ name: CUSTOMER_ID, pattern: 'CUS-[0-9]{6}' }]
storage: { path: data/veilox.db }
`, upstream, auth.Hash(key), auth.Hash(key+"2")))
	writeFile(t, dir, "names.txt", "Maria Santos\n")
	writeFile(t, dir, "prices.yaml", "models:\n  gpt-test: { input: 1, output: 4 }\n")
	return dir
}

func TestUsageAndVersion(t *testing.T) {
	if code, _, errs := run(t, ""); code != 2 || !strings.Contains(errs, "Usage:") {
		t.Fatal(code)
	}
	if code, _, errs := run(t, "", "bogus"); code != 2 || !strings.Contains(errs, `unknown command "bogus"`) {
		t.Fatal(code)
	}
	if code, out, _ := run(t, "", "version"); code != 0 || !strings.HasPrefix(out, "veilox dev") {
		t.Fatal(out)
	}
	if code, out, _ := run(t, "", "help"); code != 0 || !strings.Contains(out, "veilox scan") {
		t.Fatal(out)
	}
	if code, _, _ := run(t, "", "report", "--bogus"); code != 2 {
		t.Fatal(code)
	}
	if code, _, _ := run(t, "", "scan", "-h"); code != 0 {
		t.Fatal(code)
	}
}

func TestKeygen(t *testing.T) {
	code, out, _ := run(t, "", "keygen", "support-bot")
	if code != 0 {
		t.Fatal(code)
	}
	var k, h string
	for _, f := range strings.Fields(out) {
		if strings.HasPrefix(f, "vx_") {
			k = f
		}
		if len(f) == 64 {
			h = f
		}
	}
	if k == "" || auth.Hash(k) != h || !strings.Contains(out, "- name: support-bot") {
		t.Fatal(out)
	}
	if code, _, _ := run(t, "", "keygen"); code != 2 {
		t.Fatal(code)
	}
}

func TestCheck(t *testing.T) {
	dir := configDir(t, "https://example.invalid")
	getenv = func(k string) string {
		if k == "OPENAI_API_KEY" {
			return "set"
		}
		return ""
	}
	defer func() { getenv = os.Getenv }()
	code, out, errs := run(t, "", "check", "--config", filepath.Join(dir, "veilox.yaml"))
	if code != 0 {
		t.Fatal(errs)
	}
	for _, want := range []string{"is valid", "support-bot ($5.00/day), crm (no budget)", "CUSTOMER_ID, NAME (1 names)", "1 models from ./prices.yaml", "ANTHROPIC_API_KEY is not set"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	bad := writeFile(t, dir, "bad.yaml", "apps: [{name: x, key_sha256: nope}]\n")
	if code, _, errs := run(t, "", "check", "--config", bad); code != 1 || !strings.Contains(errs, "64 hex") {
		t.Fatal(code, errs)
	}
	empty := writeFile(t, dir, "empty.yaml", "pricing: { file: none.yaml }\n")
	if code, out, _ := run(t, "", "check", "--config", empty); code != 0 || !strings.Contains(out, "no apps") || !strings.Contains(out, "no prices") {
		t.Fatal(out)
	}
}

func TestScanText(t *testing.T) {
	dir := configDir(t, "https://example.invalid")
	cfg := filepath.Join(dir, "veilox.yaml")
	file := writeFile(t, dir, "ticket.txt", "From: Maria Santos <maria@example.com>\nCustomer CUS-004211, order 88213\nAlso <EMAIL_1> literally")
	code, out, _ := run(t, "", "scan", file, "--config", cfg)
	if code != 0 {
		t.Fatal(code)
	}
	for _, want := range []string{
		"From: <NAME_1> <<EMAIL_2>>\nCustomer <CUSTOMER_ID_1>, order 88213\nAlso <EMAIL_1> literally\n",
		"3 values would be masked", "1:7       NAME", "2:10      CUSTOMER_ID",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}

	code, out, _ = run(t, "nothing here", "scan", "-", "--json", "--config", cfg)
	var res struct {
		Masked   string
		Findings []any
	}
	if code != 0 || json.Unmarshal([]byte(out), &res) != nil || res.Masked != "nothing here" || len(res.Findings) != 0 {
		t.Fatal(out)
	}
	code, out, _ = run(t, "x\nmail a@b.co", "scan", "-", "--json", "--config", cfg)
	if code != 0 || !strings.Contains(out, `"line": 2`) || !strings.Contains(out, `"column": 6`) {
		t.Fatal(out)
	}
	if code, out, _ := run(t, "hello", "scan", "-", "--config", cfg); code != 0 || !strings.Contains(out, "Nothing to mask") {
		t.Fatal(out)
	}
}

func TestScanWithoutConfig(t *testing.T) {
	t.Chdir(t.TempDir())
	code, out, errs := run(t, "Maria Santos a@b.co", "scan", "-")
	if code != 0 || !strings.Contains(errs, "built-in detectors only") || !strings.Contains(out, "Maria Santos <EMAIL_1>") {
		t.Fatal(out, errs)
	}
	if code, _, errs := run(t, "x", "scan", "-", "--config", "missing.yaml"); code != 1 || errs == "" {
		t.Fatal("an explicit missing config should fail")
	}
	if code, _, _ := run(t, "", "scan"); code != 2 {
		t.Fatal(code)
	}
	if code, _, _ := run(t, "", "scan", "nofile.txt"); code != 1 {
		t.Fatal(code)
	}
}

func TestScanRequest(t *testing.T) {
	dir := configDir(t, "https://example.invalid")
	cfg := filepath.Join(dir, "veilox.yaml")
	openai := `{"model":"m","messages":[{"role":"user","content":[{"type":"text","text":"I am Maria Santos"},{"type":"image_url","image_url":{"url":"x"}}]}]}`
	code, out, _ := run(t, openai, "scan", "-", "--request", "--config", cfg)
	if code != 0 || !strings.Contains(out, "openai request") || !strings.Contains(out, `"text": "I am <NAME_1>"`) || !strings.Contains(out, "Not masked (veilox can't read these parts): image_url") {
		t.Fatal(out)
	}
	anthropic := `{"model":"m","system":"Agent for maria@example.com","messages":[{"role":"user","content":"hi"}]}`
	code, out, _ = run(t, anthropic, "scan", "-", "--request", "--json", "--config", cfg)
	if code != 0 || !strings.Contains(out, `"system": "Agent for <EMAIL_1>"`) {
		t.Fatal(out)
	}
	if code, _, errs := run(t, "not json", "scan", "-", "--request", "--config", cfg); code != 1 || !strings.Contains(errs, "not a JSON request body") {
		t.Fatal(errs)
	}
	if code, _, _ := run(t, "{}", "scan", "-", "--request", "--api", "gemini", "--config", cfg); code != 1 {
		t.Fatal(code)
	}
}

func ptr(v int64) *int64 { return &v }

func TestReport(t *testing.T) {
	dir := configDir(t, "https://example.invalid")
	cfg := filepath.Join(dir, "veilox.yaml")
	if code, _, errs := run(t, "", "report", "--config", cfg); code != 1 || !strings.Contains(errs, "no database") {
		t.Fatal(errs)
	}
	st, err := store.Open(filepath.Join(dir, "data", "veilox.db"))
	if err != nil {
		t.Fatal(err)
	}
	now = func() time.Time { return time.Date(2026, 11, 9, 15, 0, 0, 0, time.UTC) }
	defer func() { now = time.Now }()
	for _, r := range []store.Record{
		{Day: "2026-11-09", App: "support-bot", Provider: "anthropic", Model: "claude-x", Status: 200, Usage: pricing.Usage{Input: 1_200_000, Output: 300_000}, CostMicros: ptr(4_980_000), Masked: map[string]int{"EMAIL": 6000, "NAME": 412}},
		{Day: "2026-11-09", App: "crm", Provider: "openai", Model: "gpt-x", Status: 200, Usage: pricing.Usage{Input: 800}, CostMicros: ptr(1_000)},
		{Day: "2026-11-09", App: "crm", Provider: "openai", Model: "local", Status: 500, Usage: pricing.Usage{Input: 5}},
		{Day: "2026-11-01", App: "crm", Provider: "openai", Model: "gpt-x", Status: 200, CostMicros: ptr(2_000_000)},
	} {
		r.Time = now()
		if err := st.Insert(context.Background(), r); err != nil {
			t.Fatal(err)
		}
	}
	st.Close()

	code, out, errs := run(t, "", "report", "--config", cfg)
	if code != 0 {
		t.Fatal(errs)
	}
	want := `DAY         APP          MODEL     REQUESTS  ERRORS  INPUT TOK  OUTPUT TOK    COST  MASKED
2026-11-09  support-bot  claude-x         1       0  1,200,000     300,000   $4.98   6,412
2026-11-09  crm          gpt-x            1       0        800           0  <$0.01       0
2026-11-09  crm          local            1       1          5           0  $0.00*       0
TOTAL                                     3       1  1,200,805     300,000  $4.98*   6,412

* 1 requests to models without a price, not included in COST: local
  Add them to prices.yaml.
`
	if out != want {
		t.Fatalf("got:\n%s\nwant:\n%s", out, want)
	}

	code, out, _ = run(t, "", "report", "--config", cfg, "--since", "all", "--by", "app", "--format", "csv")
	if code != 0 || out != "app,requests,errors,input_tokens,output_tokens,cost_usd,unpriced_requests,masked\nsupport-bot,1,0,1200000,300000,4.980000,0,6412\ncrm,3,1,805,0,2.001000,1,0\n" {
		t.Fatal(out)
	}
	code, out, _ = run(t, "", "report", "--db", filepath.Join(dir, "data", "veilox.db"), "--config", "nope.yaml", "--since", "2026-11-01", "--until", "2026-11-01", "--app", "crm", "--by", "provider", "--format", "json")
	var rows []map[string]any
	if code != 0 || json.Unmarshal([]byte(out), &rows) != nil || len(rows) != 1 || rows[0]["provider"] != "openai" || rows[0]["cost_usd"] != 2.0 {
		t.Fatal(out)
	}
	if code, out, _ := run(t, "", "report", "--config", cfg, "--since", "2020-01-01", "--until", "2020-01-02"); code != 0 || !strings.Contains(out, "No requests") {
		t.Fatal(out)
	}
	for _, bad := range [][]string{{"--since", "yesterday"}, {"--until", "11/09"}, {"--format", "xml"}, {"--by", "color"}, {"--since", "0d"}} {
		if code, _, _ := run(t, "", append([]string{"report", "--config", cfg}, bad...)...); code != 1 {
			t.Errorf("%v: code %d", bad, code)
		}
	}
}

func TestCommasAndMoney(t *testing.T) {
	for in, want := range map[string]string{"0": "0", "999": "999", "1000": "1,000", "1234567.89": "1,234,567.89"} {
		if commas(in) != want {
			t.Errorf("commas(%s) = %s", in, commas(in))
		}
	}
	if money(12_345_678_900, 0) != "$12,345.68" || money(9_999, 0) != "<$0.01" || money(0, 2) != "$0.00*" {
		t.Fatal("money")
	}
}

func TestServe(t *testing.T) {
	fake := &fakeprovider.Fake{}
	up := httptest.NewServer(fake)
	defer up.Close()
	dir := configDir(t, up.URL)
	cfg := filepath.Join(dir, "veilox.yaml")
	t.Setenv("OPENAI_API_KEY", "sk-test")
	t.Setenv("ANTHROPIC_API_KEY", "")

	addr := make(chan string, 1)
	sigs := make(chan chan<- os.Signal, 1)
	onListen = func(a string) { addr <- a }
	serveSignals = func(c chan<- os.Signal) { sigs <- c }
	defer func() {
		onListen = func(string) {}
		serveSignals = func(chan<- os.Signal) {}
	}()

	var logs syncWriter
	done := make(chan int, 1)
	go func() { done <- Run([]string{"serve", "--config", cfg}, nil, io.Discard, &logs) }()
	base := "http://" + <-addr
	sig := <-sigs

	req, _ := http.NewRequest(http.MethodPost, base+"/v1/chat/completions", strings.NewReader(`{"model":"gpt-test","messages":[{"role":"user","content":"I am Maria Santos"}]}`))
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || !strings.Contains(string(body), "Hi Maria Santos") {
		t.Fatal(resp.Status, string(body))
	}

	// Reload with a broken file keeps the old config; a valid change is picked up.
	writeFile(t, dir, "prices.yaml", "models: [")
	sig <- syscall.SIGHUP
	waitFor(t, &logs, "reload failed")
	writeFile(t, dir, "prices.yaml", "models:\n  gpt-test: { input: 1, output: 4 }\n  other: { input: 1, output: 1 }\n")
	sig <- syscall.SIGHUP
	waitFor(t, &logs, `"priced_models":2`)

	sig <- syscall.SIGTERM
	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("exit %d: %s", code, logs.String())
		}
	case <-time.After(10 * time.Second):
		t.Fatal("serve did not stop")
	}
	out := logs.String()
	for _, want := range []string{"veilox listening", "ANTHROPIC_API_KEY", `"masked":{"NAME":1}`, "shutting down"} {
		if !strings.Contains(out, want) {
			t.Errorf("log lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "Maria") {
		t.Error("PII in logs")
	}
	if code, _, errs := run(t, "", "report", "--config", cfg); code != 0 || errs != "" {
		t.Fatal(errs)
	}
}

func TestServeErrors(t *testing.T) {
	dir := t.TempDir()
	noApps := writeFile(t, dir, "a.yaml", "")
	if code, _, errs := run(t, "", "serve", "--config", noApps); code != 1 || !strings.Contains(errs, "no apps") {
		t.Fatal(errs)
	}
	if code, _, _ := run(t, "", "serve", "--config", filepath.Join(dir, "missing.yaml")); code != 1 {
		t.Fatal(code)
	}
	busy := configDir(t, "https://example.invalid")
	ln, err := net.Listen("tcp", "127.0.0.1:0") // holds the port so serve can't use it
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	t.Setenv("VEILOX_LISTEN", ln.Addr().String())
	if code, _, errs := run(t, "", "serve", "--config", filepath.Join(busy, "veilox.yaml")); code != 1 || !strings.Contains(errs, "address already in use") {
		t.Fatal(errs)
	}
}

func waitFor(t *testing.T, w *syncWriter, s string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(w.String(), s) {
		if time.Now().After(deadline) {
			t.Fatalf("log never contained %q:\n%s", s, w.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

type syncWriter struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (w *syncWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.b.Write(p)
}

func (w *syncWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.b.String()
}
