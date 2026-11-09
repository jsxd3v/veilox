package proxy

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jsxd3v/veilox/internal/auth"
	"github.com/jsxd3v/veilox/internal/budget"
	"github.com/jsxd3v/veilox/internal/config"
	"github.com/jsxd3v/veilox/internal/fakeprovider"
	"github.com/jsxd3v/veilox/internal/store"
)

// Values that must never reach the provider or the logs.
var pii = []string{"Maria Santos", "maria.santos@example.com", "+351 912 345 678", "4111 1111 1111 1111", "DE89 3704 0044 0532 0130 00"}

const ticket = "Customer Maria Santos (maria.santos@example.com, +351 912 345 678) says card 4111 1111 1111 1111 was charged twice. IBAN DE89 3704 0044 0532 0130 00."

const appKey = "vx_testkey0000000000000000000000000000000001"

type env struct {
	t        *testing.T
	url      string
	fake     *fakeprovider.Fake
	upstream *httptest.Server
	store    *store.Store
	srv      *Server
	logs     *syncBuffer
	dir      string
}

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

type opts struct {
	extraYAML string // appended to the config
	budget    string // daily_budget_usd value, "" for none
	upstream  http.Handler
	noKey     bool
}

func setup(t *testing.T, o opts) *env {
	t.Helper()
	dir := t.TempDir()
	fake := &fakeprovider.Fake{}
	var h http.Handler = fake
	if o.upstream != nil {
		h = o.upstream
	}
	up := httptest.NewServer(h)
	t.Cleanup(up.Close)

	budgetLine := ""
	if o.budget != "" {
		budgetLine = "\n    daily_budget_usd: " + o.budget
	}
	cfgText := fmt.Sprintf(`
server:
  upstream_timeout: 5s
providers:
  openai: { base_url: %[1]s/v1 }
  anthropic: { base_url: %[1]s }
apps:
  - name: support-bot
    key_sha256: %[2]s%[3]s
storage:
  path: veilox.db
masking:
  names: ["Maria Santos"]
%[4]s`, up.URL, auth.Hash(appKey), budgetLine, o.extraYAML)
	write(t, filepath.Join(dir, "veilox.yaml"), cfgText)
	write(t, filepath.Join(dir, "prices.yaml"), `
models:
  "gpt-test*": { input: 1, output: 4 }
  claude-test: { input: 3, output: 15 }
`)
	cfg, err := config.Load(filepath.Join(dir, "veilox.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	rt, err := NewRuntime(cfg)
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(cfg.Resolve(cfg.Storage.Path))
	if err != nil {
		t.Fatal(err)
	}
	st.StartWriter(func(err error) { t.Errorf("store: %v", err) })
	t.Cleanup(func() { _ = st.Close() })
	bt, err := budget.New(context.Background(), cfg.Location, nil, st.SpendByApp)
	if err != nil {
		t.Fatal(err)
	}
	logs := &syncBuffer{}
	srv := New(rt, st, bt, slog.New(slog.NewJSONHandler(logs, nil)))
	srv.getenv = func(k string) string {
		if o.noKey {
			return ""
		}
		return "provider-secret-" + k
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	e := &env{t: t, url: ts.URL, fake: fake, upstream: up, store: st, srv: srv, logs: logs, dir: dir}
	t.Cleanup(e.checkNoPII)
	return e
}

func write(t *testing.T, path, text string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
}

// checkNoPII fails the test if any known value reached the provider, the logs or the database file.
func (e *env) checkNoPII() {
	e.store.Wait()
	db, _ := os.ReadFile(filepath.Join(e.dir, "veilox.db"))
	wal, _ := os.ReadFile(filepath.Join(e.dir, "veilox.db-wal"))
	places := map[string]string{"logs": e.logs.String(), "database": string(db) + string(wal)}
	for i, b := range e.fake.Bodies() {
		places[fmt.Sprintf("provider request %d", i)] = b
	}
	for where, text := range places {
		for _, v := range pii {
			if strings.Contains(text, v) {
				e.t.Errorf("%q found in %s", v, where)
			}
		}
	}
}

func (e *env) do(path string, body any, headers map[string]string) *http.Response {
	e.t.Helper()
	var r io.Reader
	switch b := body.(type) {
	case string:
		r = strings.NewReader(b)
	default:
		r = bytes.NewReader(encodeJSON(b))
	}
	req, err := http.NewRequest(http.MethodPost, e.url+path, r)
	if err != nil {
		e.t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if _, ok := headers["Authorization"]; !ok {
		if _, ok := headers["x-api-key"]; !ok {
			req.Header.Set("Authorization", "Bearer "+appKey)
		}
	}
	for k, v := range headers {
		if v == "" {
			req.Header.Del(k)
		} else {
			req.Header.Set(k, v)
		}
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	e.t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func readJSON(t *testing.T, resp *http.Response) map[string]any {
	t.Helper()
	b, _ := io.ReadAll(resp.Body)
	m, err := decodeObject(b)
	if err != nil {
		t.Fatalf("not JSON (%d): %s", resp.StatusCode, b)
	}
	return m
}

func (e *env) report() []store.Row {
	e.store.Wait()
	rows, err := e.store.Report(context.Background(), store.Query{From: "2000-01-01", To: "2999-01-01", By: []string{"app", "model"}})
	if err != nil {
		e.t.Fatal(err)
	}
	return rows
}

func openAIBody(stream bool, extra map[string]any) map[string]any {
	b := map[string]any{
		"model":    "gpt-test",
		"messages": []any{map[string]any{"role": "system", "content": "You are a support agent."}, map[string]any{"role": "user", "content": ticket}},
	}
	if stream {
		b["stream"] = true
	}
	for k, v := range extra {
		b[k] = v
	}
	return b
}

func anthropicBody(stream bool, extra map[string]any) map[string]any {
	b := map[string]any{
		"model":      "claude-test",
		"max_tokens": 200,
		"system":     "Support agent for Maria Santos's bank.",
		"messages":   []any{map[string]any{"role": "user", "content": []any{map[string]any{"type": "text", "text": ticket}}}},
	}
	if stream {
		b["stream"] = true
	}
	for k, v := range extra {
		b[k] = v
	}
	return b
}

const wantReply = "Hi Maria Santos, sorry about the double charge on card 4111 1111 1111 1111. The refund is on its way and we'll confirm it at maria.santos@example.com."

var tools = []any{map[string]any{"type": "function", "function": map[string]any{"name": "send_email", "parameters": map[string]any{"type": "object"}}}}

func TestOpenAIJSON(t *testing.T) {
	e := setup(t, opts{})
	resp := e.do("/v1/chat/completions", openAIBody(false, map[string]any{"tools": tools}), map[string]string{"OpenAI-Organization": "org-1", "Cookie": "a=b"})
	if resp.StatusCode != 200 {
		t.Fatal(resp.Status)
	}
	if resp.Header.Get("X-Veilox-Request-Id") == "" {
		t.Error("missing request id")
	}
	got := readJSON(t, resp)
	msg := asMap(asMap(asSlice(got["choices"])[0])["message"])
	if msg["content"] != wantReply {
		t.Fatalf("content: %v", msg["content"])
	}
	args := asMap(asMap(asSlice(msg["tool_calls"])[0])["function"])["arguments"].(string)
	var a map[string]string
	if err := json.Unmarshal([]byte(args), &a); err != nil || a["to"] != "maria.santos@example.com" {
		t.Fatalf("arguments %q: %v", args, err)
	}

	sent := e.fake.Bodies()[0]
	for _, want := range []string{"<NAME_1>", "<EMAIL_1>", "<PHONE_1>", "<CREDIT_CARD_1>", "<IBAN_1>"} {
		if !strings.Contains(sent, want) {
			t.Errorf("provider body lacks %s: %s", want, sent)
		}
	}
	h := e.fake.Headers()[0]
	if h.Get("Authorization") != "Bearer provider-secret-OPENAI_API_KEY" || h.Get("OpenAI-Organization") != "org-1" || h.Get("Cookie") != "" {
		t.Errorf("headers: %v", h)
	}
	rows := e.report()
	if len(rows) != 1 || rows[0].Requests != 1 || rows[0].Masked != 5 || rows[0].CostMicros == 0 || rows[0].Unpriced != 0 {
		t.Fatalf("%+v", rows)
	}
	if !strings.Contains(e.logs.String(), `"masked":{"CREDIT_CARD":1,"EMAIL":1,"IBAN":1,"NAME":1,"PHONE":1}`) {
		t.Errorf("log: %s", e.logs.String())
	}
}

// readStream returns the events of an SSE response.
func readStream(t *testing.T, resp *http.Response) []*event {
	t.Helper()
	br := bufio.NewReader(resp.Body)
	var out []*event
	for {
		ev, err := readEvent(br)
		if ev != nil {
			out = append(out, ev)
		}
		if err != nil {
			return out
		}
	}
}

// openAIStreamText rebuilds content and tool arguments from chunks, checking no chunk shows half a placeholder.
func openAIStreamText(t *testing.T, evs []*event) (content, args string, usageChunks int) {
	t.Helper()
	for _, ev := range evs {
		if ev.data == "[DONE]" {
			continue
		}
		c, err := decodeObject([]byte(ev.data))
		if err != nil {
			t.Fatalf("bad chunk %q", ev.data)
		}
		if c["usage"] != nil {
			usageChunks++
		}
		for _, ch := range asSlice(c["choices"]) {
			d := asMap(asMap(ch)["delta"])
			if s, ok := asString(d["content"]); ok {
				if strings.Contains(s, "<") {
					t.Errorf("chunk shows placeholder text: %q", s)
				}
				content += s
			}
			for _, tc := range asSlice(d["tool_calls"]) {
				if s, ok := asString(asMap(asMap(tc)["function"])["arguments"]); ok {
					args += s
				}
			}
		}
	}
	return
}

func TestOpenAIStream(t *testing.T) {
	e := setup(t, opts{})
	resp := e.do("/v1/chat/completions", openAIBody(true, map[string]any{"tools": tools}), nil)
	if resp.StatusCode != 200 || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		t.Fatal(resp.Status, resp.Header)
	}
	evs := readStream(t, resp)
	content, args, usageChunks := openAIStreamText(t, evs)
	if content != wantReply {
		t.Fatalf("content %q", content)
	}
	if args != `{"subject":"Your refund","to":"maria.santos@example.com"}` {
		t.Fatalf("args %q", args)
	}
	if usageChunks != 0 {
		t.Error("veilox added include_usage, so the usage chunk must not reach the app")
	}
	if evs[len(evs)-1].data != "[DONE]" {
		t.Error("stream must end with [DONE]")
	}
	if !strings.Contains(e.fake.Bodies()[0], `"include_usage":true`) {
		t.Error("include_usage not requested")
	}
	rows := e.report()
	if len(rows) != 1 || rows[0].OutputTokens == 0 || rows[0].CostMicros == 0 {
		t.Fatalf("usage not recorded: %+v", rows)
	}
}

func TestOpenAIStreamAppAskedForUsage(t *testing.T) {
	e := setup(t, opts{})
	resp := e.do("/v1/chat/completions", openAIBody(true, map[string]any{"stream_options": map[string]any{"include_usage": true}}), nil)
	_, _, usageChunks := openAIStreamText(t, readStream(t, resp))
	if usageChunks != 1 {
		t.Fatal("the app asked for usage and should get it")
	}
}

func TestAnthropicJSON(t *testing.T) {
	e := setup(t, opts{})
	body := anthropicBody(false, map[string]any{"tools": tools})
	// History with a thinking block (signed, must be untouched), a tool call and a tool result.
	thinking := map[string]any{"type": "thinking", "thinking": "user is <NAME_1>", "signature": "sig"}
	body["messages"] = append(asSlice(body["messages"]),
		map[string]any{"role": "assistant", "content": []any{thinking, map[string]any{"type": "tool_use", "id": "t1", "name": "lookup", "input": map[string]any{"email": "maria.santos@example.com", "n": 1}}}},
		map[string]any{"role": "user", "content": []any{map[string]any{"type": "tool_result", "tool_use_id": "t1", "content": "Card on file: 4111 1111 1111 1111"}}},
	)
	resp := e.do("/v1/messages", body, map[string]string{"Authorization": "", "x-api-key": appKey, "anthropic-version": "2023-06-01"})
	if resp.StatusCode != 200 {
		t.Fatal(resp.Status)
	}
	got := readJSON(t, resp)
	content := asSlice(got["content"])
	if asMap(content[0])["text"] != wantReply {
		t.Fatalf("text %v", asMap(content[0])["text"])
	}
	if in := asMap(asMap(content[1])["input"]); in["to"] != "maria.santos@example.com" {
		t.Fatalf("tool input %v", in)
	}
	sent := e.fake.Bodies()[0]
	if !strings.Contains(sent, `"thinking":"user is <NAME_1>"`) || !strings.Contains(sent, `"email":"<EMAIL_1>"`) || !strings.Contains(sent, `"content":"Card on file: <CREDIT_CARD_1>"`) {
		t.Fatalf("sent %s", sent)
	}
	h := e.fake.Headers()[0]
	if h.Get("x-api-key") != "provider-secret-ANTHROPIC_API_KEY" || h.Get("Anthropic-Version") != "2023-06-01" || h.Get("Authorization") != "" {
		t.Fatalf("headers %v", h)
	}
	if rows := e.report(); rows[0].Model != "claude-test" || rows[0].CostMicros == 0 {
		t.Fatalf("%+v", rows)
	}
}

func TestAnthropicStream(t *testing.T) {
	e := setup(t, opts{})
	e.fake.ChunkSize = 2
	resp := e.do("/v1/messages", anthropicBody(true, map[string]any{"tools": tools}), nil)
	evs := readStream(t, resp)
	var text, partial string
	names := []string{}
	for _, ev := range evs {
		names = append(names, ev.name)
		d, err := decodeObject([]byte(ev.data))
		if err != nil {
			t.Fatal(ev.data)
		}
		delta := asMap(d["delta"])
		if s, ok := asString(delta["text"]); ok {
			if strings.Contains(s, "<") {
				t.Errorf("half placeholder %q", s)
			}
			text += s
		}
		if s, ok := asString(delta["partial_json"]); ok {
			partial += s
		}
	}
	if text != wantReply {
		t.Fatalf("text %q", text)
	}
	if partial != `{"subject":"Your refund","to":"maria.santos@example.com"}` {
		t.Fatalf("partial %q", partial)
	}
	if names[0] != "message_start" || names[len(names)-1] != "message_stop" {
		t.Fatal(names)
	}
	rows := e.report()
	if rows[0].OutputTokens < 10 || rows[0].InputTokens == 0 {
		t.Fatalf("%+v", rows)
	}
}

func TestPassThroughUnchangedEvents(t *testing.T) {
	up := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, ": comment\nevent: ping\ndata: {\"type\":\"ping\"}\n\n")
		_, _ = io.WriteString(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"model\":\"claude-test\",\"usage\":{\"input_tokens\":5}}}\n\n")
	})
	e := setup(t, opts{upstream: up})
	resp := e.do("/v1/messages", anthropicBody(true, nil), nil)
	b, _ := io.ReadAll(resp.Body)
	want := ": comment\nevent: ping\ndata: {\"type\":\"ping\"}\n\nevent: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"model\":\"claude-test\",\"usage\":{\"input_tokens\":5}}}\n\n"
	if string(b) != want {
		t.Fatalf("%q", b)
	}
}

func TestAuth(t *testing.T) {
	e := setup(t, opts{})
	r := e.do("/v1/chat/completions", openAIBody(false, nil), map[string]string{"Authorization": "Bearer vx_wrong"})
	if r.StatusCode != 401 || asMap(readJSON(t, r)["error"])["type"] != "authentication_error" {
		t.Fatal(r.Status)
	}
	r = e.do("/v1/messages", anthropicBody(false, nil), map[string]string{"Authorization": "", "x-api-key": ""})
	if r.StatusCode != 401 || asMap(readJSON(t, r)["error"])["type"] != "authentication_error" {
		t.Fatal(r.Status)
	}
	if len(e.fake.Bodies()) != 0 {
		t.Fatal("nothing should be forwarded")
	}
	if strings.Contains(e.logs.String(), "vx_wrong") {
		t.Fatal("keys must not be logged")
	}
}

func TestBudget(t *testing.T) {
	e := setup(t, opts{budget: "0.00001"}) // 10 micro-dollars: one request uses it up
	if r := e.do("/v1/messages", anthropicBody(false, nil), nil); r.StatusCode != 200 {
		t.Fatal(r.Status)
	}
	r := e.do("/v1/messages", anthropicBody(false, nil), nil)
	if r.StatusCode != 429 {
		t.Fatal(r.Status)
	}
	if ra := r.Header.Get("Retry-After"); ra == "" || ra == "0" {
		t.Fatalf("Retry-After %q", ra)
	}
	er := asMap(readJSON(t, r)["error"])
	if er["type"] != "rate_limit_error" || !strings.Contains(er["message"].(string), `daily budget of $0.00 for app "support-bot" reached`) {
		t.Fatal(er)
	}
	r = e.do("/v1/chat/completions", openAIBody(false, nil), nil)
	if er := asMap(readJSON(t, r)["error"]); r.StatusCode != 429 || er["type"] != "budget_exceeded" || er["code"] != "veilox_budget_exceeded" {
		t.Fatal(er)
	}
	if len(e.fake.Bodies()) != 1 {
		t.Fatal("blocked requests must not be forwarded")
	}
}

func TestBudgetSurvivesRestart(t *testing.T) {
	e := setup(t, opts{budget: "0.00001"})
	e.do("/v1/messages", anthropicBody(false, nil), nil)
	e.store.Wait()
	rt := e.srv.rt.Load()
	bt, err := budget.New(context.Background(), rt.Cfg.Location, nil, e.store.SpendByApp)
	if err != nil {
		t.Fatal(err)
	}
	e.srv.budgets = bt // a fresh tracker loads today's spend from the database
	if r := e.do("/v1/messages", anthropicBody(false, nil), nil); r.StatusCode != 429 {
		t.Fatal(r.Status)
	}
}

func TestRequestErrors(t *testing.T) {
	e := setup(t, opts{extraYAML: "  "})
	r := e.do("/v1/chat/completions", "{not json", nil)
	if r.StatusCode != 400 {
		t.Fatal(r.Status)
	}
	r = e.do("/v1/chat/completions", `[1,2]`, nil)
	if r.StatusCode != 400 {
		t.Fatal(r.Status)
	}
	r = e.do("/v1/chat/completions", `{"a":1} {"b":2}`, nil)
	if r.StatusCode != 400 {
		t.Fatal(r.Status)
	}
	big := `{"model":"gpt-test","messages":[{"role":"user","content":"` + strings.Repeat("x", 11<<20) + `"}]}`
	r = e.do("/v1/chat/completions", big, nil)
	if r.StatusCode != 413 {
		t.Fatal(r.Status)
	}
	r = e.do("/v1/messages", big, nil)
	if r.StatusCode != 413 || asMap(readJSON(t, r)["error"])["type"] != "request_too_large" {
		t.Fatal(r.Status)
	}
	if len(e.fake.Bodies()) != 0 {
		t.Fatal("nothing should be forwarded")
	}
}

func TestRoutes(t *testing.T) {
	e := setup(t, opts{})
	resp, err := http.Get(e.url + "/healthz")
	if err != nil || resp.StatusCode != 200 {
		t.Fatal(err)
	}
	resp.Body.Close()
	resp, _ = http.Get(e.url + "/v1/chat/completions")
	if resp.StatusCode != 405 {
		t.Fatal(resp.Status)
	}
	resp.Body.Close()
	r := e.do("/v1/embeddings", `{}`, nil)
	if r.StatusCode != 404 || !strings.Contains(asMap(readJSON(t, r)["error"])["message"].(string), "/v1/messages") {
		t.Fatal(r.Status)
	}
}

func TestUnsupportedContent(t *testing.T) {
	img := []any{map[string]any{"type": "text", "text": "Who is Maria Santos?"}, map[string]any{"type": "image_url", "image_url": map[string]any{"url": "data:image/png;base64,AAAA"}}}
	body := openAIBody(false, map[string]any{"messages": []any{map[string]any{"role": "user", "content": img}}})

	e := setup(t, opts{})
	if r := e.do("/v1/chat/completions", body, nil); r.StatusCode != 200 {
		t.Fatal(r.Status)
	}
	if !strings.Contains(e.logs.String(), `"unmasked_parts":1`) || !strings.Contains(e.fake.Bodies()[0], "Who is <NAME_1>?") {
		t.Fatal(e.logs.String())
	}

	e = setup(t, opts{extraYAML: "  unsupported_content: reject\n"})
	body = openAIBody(false, map[string]any{"messages": []any{map[string]any{"role": "user", "content": img}}})
	r := e.do("/v1/chat/completions", body, nil)
	if r.StatusCode != 400 || !strings.Contains(asMap(readJSON(t, r)["error"])["message"].(string), "image_url") {
		t.Fatal(r.Status)
	}
	ab := anthropicBody(false, nil)
	ab["messages"] = []any{map[string]any{"role": "user", "content": []any{map[string]any{"type": "document", "source": map[string]any{}}}}}
	if r := e.do("/v1/messages", ab, nil); r.StatusCode != 400 {
		t.Fatal(r.Status)
	}
	if len(e.fake.Bodies()) != 0 {
		t.Fatal("rejected requests must not be forwarded")
	}
}

func TestProviderErrorsPassThrough(t *testing.T) {
	up := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		msg := "rejected"
		if strings.Contains(string(b), "<EMAIL_1>") {
			msg = "invalid recipient <EMAIL_1>"
		}
		_, _ = fmt.Fprintf(w, `{"error":{"message":%q,"type":"invalid_request_error"}}`, msg)
	})
	e := setup(t, opts{upstream: up})
	r := e.do("/v1/chat/completions", openAIBody(false, nil), nil)
	if r.StatusCode != 400 {
		t.Fatal(r.Status)
	}
	if m := asMap(readJSON(t, r)["error"])["message"]; m != "invalid recipient maria.santos@example.com" {
		t.Fatal(m)
	}
	e.store.Wait()
	if !strings.Contains(e.logs.String(), `"error_type":"invalid_request_error"`) {
		t.Fatal(e.logs.String())
	}
}

func TestNonJSONUpstreamBody(t *testing.T) {
	up := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusBadGateway)
		_, _ = io.WriteString(w, "<html>bad gateway</html>")
	})
	e := setup(t, opts{upstream: up})
	r := e.do("/v1/messages", anthropicBody(false, nil), nil)
	b, _ := io.ReadAll(r.Body)
	if r.StatusCode != 502 || string(b) != "<html>bad gateway</html>" {
		t.Fatal(r.Status, string(b))
	}
}

func TestUpstreamFailures(t *testing.T) {
	e := setup(t, opts{noKey: true})
	r := e.do("/v1/chat/completions", openAIBody(false, nil), nil)
	if r.StatusCode != 502 || !strings.Contains(asMap(readJSON(t, r)["error"])["message"].(string), "OPENAI_API_KEY") {
		t.Fatal(r.Status)
	}

	e = setup(t, opts{})
	e.upstream.Close()
	if r := e.do("/v1/messages", anthropicBody(false, nil), nil); r.StatusCode != 502 {
		t.Fatal(r.Status)
	}

	slow := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body) // the server only notices a closed connection once the body is read
		<-r.Context().Done()
	})
	e = setup(t, opts{upstream: slow})
	rt := *e.srv.rt.Load()
	cfg := *rt.Cfg
	cfg.Server.UpstreamTimeout = 100 * time.Millisecond
	rt.Cfg = &cfg
	e.srv.Reload(&rt)
	if r := e.do("/v1/messages", anthropicBody(false, nil), nil); r.StatusCode != 504 {
		t.Fatal(r.Status)
	}
}

func TestClientClosesStream(t *testing.T) {
	release := make(chan struct{})
	up := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"model\":\"claude-test\",\"usage\":{\"input_tokens\":7}}}\n\n")
		w.(http.Flusher).Flush()
		select {
		case <-r.Context().Done():
		case <-release:
		}
	})
	e := setup(t, opts{upstream: up})
	defer close(release)
	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, e.url+"/v1/messages", bytes.NewReader(encodeJSON(anthropicBody(true, nil))))
	req.Header.Set("x-api-key", appKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = bufio.NewReader(resp.Body).ReadString('\n')
	cancel()
	resp.Body.Close()
	deadline := time.Now().Add(3 * time.Second)
	for !strings.Contains(e.logs.String(), `"status":499`) {
		if time.Now().After(deadline) {
			t.Fatalf("no 499 logged: %s", e.logs.String())
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !strings.Contains(e.logs.String(), `"input_tokens":7`) {
		t.Fatal("partial usage should be recorded")
	}
}

func TestReservedPlaceholderInPrompt(t *testing.T) {
	e := setup(t, opts{})
	e.fake.Reply = func(prompt string) string { return prompt }
	body := openAIBody(false, map[string]any{"messages": []any{map[string]any{"role": "user", "content": "Docs mention <NAME_1>. Real: Maria Santos"}}})
	got := readJSON(t, e.do("/v1/chat/completions", body, nil))
	content := asMap(asMap(asSlice(got["choices"])[0])["message"])["content"]
	if content != "Docs mention <NAME_1>. Real: Maria Santos" {
		t.Fatal(content)
	}
	if !strings.Contains(e.fake.Bodies()[0], "Docs mention <NAME_1>. Real: <NAME_2>") {
		t.Fatal(e.fake.Bodies()[0])
	}
}

func TestSystemHint(t *testing.T) {
	e := setup(t, opts{extraYAML: "  system_hint: true\n"})
	e.do("/v1/chat/completions", openAIBody(false, nil), nil)
	e.do("/v1/messages", anthropicBody(false, nil), nil)
	ab := anthropicBody(false, nil)
	ab["system"] = []any{map[string]any{"type": "text", "text": "Be brief."}}
	e.do("/v1/messages", ab, nil)
	delete(ab, "system")
	e.do("/v1/messages", ab, nil)
	nothing := openAIBody(false, map[string]any{"messages": []any{map[string]any{"role": "user", "content": "hello"}}})
	e.do("/v1/chat/completions", nothing, nil)
	b := e.fake.Bodies()
	for i := range 4 {
		if !strings.Contains(b[i], "write each placeholder exactly") {
			t.Errorf("request %d has no hint: %s", i, b[i])
		}
	}
	if strings.Contains(b[4], "placeholder") {
		t.Error("no hint when nothing was masked")
	}
}

func TestUnknownModel(t *testing.T) {
	e := setup(t, opts{})
	e.do("/v1/chat/completions", openAIBody(false, map[string]any{"model": "mystery"}), nil)
	e.do("/v1/chat/completions", openAIBody(false, map[string]any{"model": "mystery"}), nil)
	if rows := e.report(); rows[0].Unpriced != 2 || rows[0].CostMicros != 0 {
		t.Fatalf("%+v", rows)
	}
	if n := strings.Count(e.logs.String(), "model has no price"); n != 1 {
		t.Fatalf("warned %d times", n)
	}

	e = setup(t, opts{extraYAML: "pricing:\n  unknown_model: reject\n"})
	r := e.do("/v1/chat/completions", openAIBody(false, map[string]any{"model": "mystery"}), nil)
	if r.StatusCode != 400 || len(e.fake.Bodies()) != 0 {
		t.Fatal(r.Status)
	}
}

func TestDatedModelUsesRequestedPrice(t *testing.T) {
	e := setup(t, opts{})
	e.fake.Model = "claude-test-20261101"
	e.do("/v1/messages", anthropicBody(false, nil), nil)
	if rows := e.report(); rows[0].Model != "claude-test-20261101" || rows[0].Unpriced != 0 || rows[0].CostMicros == 0 {
		t.Fatalf("%+v", rows)
	}
}
