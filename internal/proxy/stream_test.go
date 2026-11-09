package proxy

import (
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

// scripted returns an upstream that sends a fixed SSE body.
func scripted(body string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, body)
	})
}

func chunk(delta, finish string) string {
	if finish == "" {
		finish = "null"
	} else {
		finish = `"` + finish + `"`
	}
	return `data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"gpt-test","choices":[{"index":0,"delta":` + delta + `,"finish_reason":` + finish + `}]}` + "\n\n"
}

func streamBody(t *testing.T, e *env, path string, body map[string]any) string {
	t.Helper()
	resp := e.do(path, body, nil)
	b, _ := io.ReadAll(resp.Body)
	return string(b)
}

// The model's text ends in something that looks like the start of a placeholder: it is held back,
// then released unchanged when the choice finishes.
func TestOpenAIStreamFlushAtFinish(t *testing.T) {
	up := scripted(
		chunk(`{"role":"assistant","content":"Bye <NA"}`, "") +
			chunk(`{"content":"ME_1> and <NAME_"}`, "") +
			chunk(`{"tool_calls":[{"index":0,"function":{"arguments":"{\"to\":\"<EMA"}}]}`, "") +
			chunk(`{}`, "stop") +
			"data: [DONE]\n\n")
	e := setup(t, opts{upstream: up})
	out := streamBody(t, e, "/v1/chat/completions", openAIBody(true, nil))
	var content, args string
	for _, ev := range readStream(t, &http.Response{Body: io.NopCloser(strings.NewReader(out))}) {
		c, err := decodeObject([]byte(ev.data))
		if err != nil {
			continue // [DONE]
		}
		d := asMap(asMap(asSlice(c["choices"])[0])["delta"])
		if s, ok := asString(d["content"]); ok {
			content += s
		}
		for _, tc := range asSlice(d["tool_calls"]) {
			a, _ := asString(asMap(asMap(tc)["function"])["arguments"])
			args += a
		}
	}
	if content != "Bye Maria Santos and <NAME_" {
		t.Fatalf("content %q", content)
	}
	if !strings.Contains(out, `"finish_reason":"stop"`) || !strings.Contains(out, `"content":"<NAME_"`) {
		t.Fatalf("held text not released at finish:\n%s", out)
	}
	if args != `{"to":"<EMA` {
		t.Fatalf("args %q", args)
	}
}

// A stream that ends without finish_reason still releases held text before [DONE].
func TestOpenAIStreamFlushAtDone(t *testing.T) {
	up := scripted(chunk(`{"content":"Hi <EMAIL"}`, "") + "data: [DONE]\n\n")
	e := setup(t, opts{upstream: up})
	out := streamBody(t, e, "/v1/chat/completions", openAIBody(true, nil))
	if !strings.Contains(out, `"content":"Hi "`) || !strings.Contains(out, `"content":"<EMAIL"`) || !strings.HasSuffix(out, "data: [DONE]\n\n") {
		t.Fatalf("%s", out)
	}
	if strings.Index(out, `"<EMAIL"`) > strings.Index(out, "[DONE]") {
		t.Fatal("flush must come before [DONE]")
	}
}

// A stream cut off without [DONE]: held text is still delivered.
func TestOpenAIStreamFlushAtEOF(t *testing.T) {
	up := scripted(chunk(`{"content":"Hi <NAME_1"}`, ""))
	e := setup(t, opts{upstream: up})
	out := streamBody(t, e, "/v1/chat/completions", openAIBody(true, nil))
	if !strings.Contains(out, `"content":"<NAME_1"`) {
		t.Fatalf("%s", out)
	}
}

func TestOpenAIStreamErrorAndRefusal(t *testing.T) {
	up := scripted(chunk(`{"refusal":"I can't email <EMAIL_1>"}`, "") +
		`data: {"error":{"message":"bad <NAME_1>","type":"server_error"}}` + "\n\n" +
		"data: not json\n\n")
	e := setup(t, opts{upstream: up})
	out := streamBody(t, e, "/v1/chat/completions", openAIBody(true, nil))
	if !strings.Contains(out, "I can't email maria.santos@example.com") || !strings.Contains(out, "bad Maria Santos") || !strings.Contains(out, "data: not json\n\n") {
		t.Fatalf("%s", out)
	}
	if !strings.Contains(e.logs.String(), `"masked"`) {
		t.Fatal(e.logs.String())
	}
}

// Anthropic: held text is released at content_block_stop, or at the end if the stream is cut.
func TestAnthropicStreamFlush(t *testing.T) {
	ev := func(name, data string) string { return "event: " + name + "\ndata: " + data + "\n\n" }
	up := scripted(
		ev("message_start", `{"type":"message_start","message":{"model":"claude-test","usage":{"input_tokens":3,"cache_read_input_tokens":2,"cache_creation_input_tokens":1}}}`) +
			ev("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":"Dear <NAME_1>"}}`) +
			ev("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":", see <CREDIT_CA"}}`) +
			ev("content_block_stop", `{"type":"content_block_stop","index":0}`) +
			ev("content_block_start", `{"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"t","name":"x","input":{"to":"<EMAIL_1>"}}}`) +
			ev("content_block_delta", `{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"cc\":\"\\u003cEMA"}}`) +
			ev("content_block_delta", `{"type":"content_block_delta","index":7,"delta":{"type":"text_delta","text":"orphan"}}`) +
			ev("message_delta", `{"type":"message_delta","usage":{"output_tokens":9}}`) +
			ev("error", `{"type":"error","error":{"type":"overloaded_error","message":"about <NAME_1>"}}`))
	e := setup(t, opts{upstream: up})
	out := streamBody(t, e, "/v1/messages", anthropicBody(true, nil))
	for _, want := range []string{
		`"text":"Dear Maria Santos"`,
		`"text":", see "`,
		"event: content_block_delta\ndata: {\"delta\":{\"text\":\"<CREDIT_CA\",\"type\":\"text_delta\"},\"index\":0,\"type\":\"content_block_delta\"}\n\nevent: content_block_stop",
		`"input":{"to":"maria.santos@example.com"}`,
		`"partial_json":"{\"cc\":\""`,
		`"partial_json":"\\u003cEMA"`, // held back (could become an escaped placeholder), released at the end
		`"message":"about Maria Santos"`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %s in:\n%s", want, out)
		}
	}
	e.store.Wait()
	if !strings.Contains(e.logs.String(), `"input_tokens":6,"output_tokens":9`) {
		t.Fatal(e.logs.String())
	}
}

func TestReadEvent(t *testing.T) {
	r := strings.NewReader("\n\nid: 1\r\ndata: a\r\ndata: b\r\n\r\ndata: last")
	resp := &http.Response{Body: io.NopCloser(r)}
	evs := readStream(t, resp)
	if len(evs) != 2 || evs[0].data != "a\nb" || evs[1].data != "last" {
		t.Fatalf("%+v", evs)
	}
	evs[0].changed = true
	if got := string(evs[0].bytes()); got != "id: 1\ndata: a\ndata: b\n\n" {
		t.Fatalf("%q", got)
	}
}

func TestJSONHelpers(t *testing.T) {
	if asInt(3.0) != 3 || asInt(7) != 7 || asInt(int64(8)) != 8 || asInt("x") != 0 || asInt(nil) != 0 {
		t.Fatal("asInt")
	}
	m, _ := decodeObject([]byte(`{"a":1.5,"b":12345678901234}`))
	if asInt(m["a"]) != 1 || asInt(m["b"]) != 12345678901234 {
		t.Fatal(m)
	}
	if _, err := decodeObject([]byte(`null`)); err == nil || err.Error() != "body must be one JSON object" {
		t.Fatal(err)
	}
	if string(EncodeJSON(map[string]string{"a": "<b>"})) != `{"a":"<b>"}` || string(encodeJSON(func() {})) != "null" {
		t.Fatal("encode")
	}
	if GuessAPI(map[string]any{"system": "x"}) != "anthropic" || GuessAPI(map[string]any{}) != "openai" {
		t.Fatal("guess")
	}
}

func TestPanicRecovery(t *testing.T) {
	e := setup(t, opts{})
	h := e.srv.recoverPanics(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic("boom") }))
	rec := &recorder{header: http.Header{}}
	h.ServeHTTP(rec, &http.Request{URL: mustURL("/x")})
	if rec.status != 500 || !strings.Contains(e.logs.String(), "internal error") {
		t.Fatal(rec.status)
	}
}

func TestOpenAILegacyAndErrorTypes(t *testing.T) {
	m := newMapperFor(t, "Maria Santos")
	body := map[string]any{"messages": []any{
		map[string]any{"role": "assistant", "content": nil, "refusal": "no, Maria Santos", "function_call": map[string]any{"arguments": `{"n":"Maria Santos"}`}},
		map[string]any{"role": "user", "content": []any{map[string]any{"type": "refusal", "refusal": "Maria Santos"}, "junk"}},
		"junk",
	}}
	(openAI{}).walk(body, m.Mask)
	s := string(encodeJSON(body))
	if strings.Contains(s, "Maria") {
		t.Fatal(s)
	}
	resp := map[string]any{"choices": []any{map[string]any{"message": map[string]any{"refusal": "<NAME_1>", "function_call": map[string]any{"arguments": `{"n":"<NAME_1>"}`}}}, map[string]any{}}}
	(openAI{}).restore(resp, m)
	if !strings.Contains(string(encodeJSON(resp)), `{\"n\":\"Maria Santos\"}`) {
		t.Fatal(string(encodeJSON(resp)))
	}
	if (openAI{}).errorType(map[string]any{"error": map[string]any{"code": "rate_limit"}}) != "rate_limit" {
		t.Fatal("code fallback")
	}
	if (anthropic{}).errorType(map[string]any{"error": map[string]any{"type": "overloaded_error"}}) != "overloaded_error" {
		t.Fatal("anthropic type")
	}
	for _, st := range []int{401, 404, 413, 429, 500} {
		if asMap((anthropic{}).errorBody(st, "", "x").(map[string]any)["error"])["type"] == "" {
			t.Fatal(st)
		}
	}
}

// A stream that lasts longer than the body-read deadline is not cut off.
func TestLongStreamOutlivesBodyDeadline(t *testing.T) {
	up := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		for i := range 6 {
			_, _ = io.WriteString(w, chunk(`{"content":"part`+string(rune('0'+i))+` "}`, ""))
			w.(http.Flusher).Flush()
			time.Sleep(100 * time.Millisecond)
		}
		_, _ = io.WriteString(w, chunk(`{}`, "stop")+"data: [DONE]\n\n")
	})
	e := setup(t, opts{upstream: up})
	e.srv.bodyTimeout = 150 * time.Millisecond
	out := streamBody(t, e, "/v1/chat/completions", openAIBody(true, nil))
	if !strings.Contains(out, "part5") || !strings.HasSuffix(out, "data: [DONE]\n\n") {
		t.Fatalf("stream was cut:\n%s", out)
	}
}

// A client that sends its body too slowly is cut off by the body deadline.
func TestSlowBodyIsCutOff(t *testing.T) {
	e := setup(t, opts{})
	e.srv.bodyTimeout = 100 * time.Millisecond
	pr, pw := io.Pipe()
	go func() {
		_, _ = io.WriteString(pw, `{"model":"gpt-test",`)
		time.Sleep(500 * time.Millisecond)
		_, _ = io.WriteString(pw, `"messages":[]}`)
		_ = pw.Close()
	}()
	req, _ := http.NewRequest(http.MethodPost, e.url+"/v1/chat/completions", pr)
	req.Header.Set("Authorization", "Bearer "+appKey)
	resp, err := http.DefaultClient.Do(req)
	if err == nil {
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatal(resp.Status)
		}
	}
	if len(e.fake.Bodies()) != 0 {
		t.Fatal("nothing should be forwarded")
	}
}
