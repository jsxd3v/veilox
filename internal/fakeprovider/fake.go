// Package fakeprovider is a stand-in for the OpenAI and Anthropic APIs, used by tests and the demo.
// It records every request body it receives and answers using the placeholders it was sent.
package fakeprovider

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"sync"
)

var placeholderRe = regexp.MustCompile(`<[A-Z][A-Z0-9_]*_[0-9]+>`)

// Fake is an http.Handler serving /v1/chat/completions and /v1/messages.
type Fake struct {
	mu     sync.Mutex
	bodies [][]byte
	heads  []http.Header

	// Reply builds the answer from the prompt text. Default: DefaultReply.
	Reply func(prompt string) string
	// ChunkSize is how many bytes of text go in each stream event. Default 3, so placeholders get split.
	ChunkSize int
	// Model overrides the model name in responses.
	Model string
	// OnRequest, if set, is called with each decoded request (e.g. to print what the provider sees).
	OnRequest func(path string, prompt string)
}

// Bodies returns copies of all request bodies received.
func (f *Fake) Bodies() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.bodies))
	for i, b := range f.bodies {
		out[i] = string(b)
	}
	return out
}

// Headers returns the request headers received.
func (f *Fake) Headers() []http.Header {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]http.Header(nil), f.heads...)
}

// DefaultReply answers like a support assistant, using the placeholders found in the prompt.
func DefaultReply(prompt string) string {
	has := func(p string) bool { return strings.Contains(prompt, p) }
	var b strings.Builder
	if has("<NAME_1>") {
		b.WriteString("Hi <NAME_1>, ")
	} else {
		b.WriteString("Hi, ")
	}
	b.WriteString("sorry about the double charge")
	if has("<CREDIT_CARD_1>") {
		b.WriteString(" on card <CREDIT_CARD_1>")
	}
	b.WriteString(". The refund is on its way")
	if has("<EMAIL_1>") {
		b.WriteString(" and we'll confirm it at <EMAIL_1>")
	}
	b.WriteString(".")
	return b.String()
}

type request struct {
	Model    string            `json:"model"`
	Stream   bool              `json:"stream"`
	System   json.RawMessage   `json:"system"`
	Messages []json.RawMessage `json:"messages"`
	Tools    []json.RawMessage `json:"tools"`
}

// promptText joins every string in the request's system prompt and messages.
func promptText(req request) string {
	var parts []string
	var collect func(v any)
	collect = func(v any) {
		switch t := v.(type) {
		case string:
			parts = append(parts, t)
		case []any:
			for _, x := range t {
				collect(x)
			}
		case map[string]any:
			keys := make([]string, 0, len(t))
			for k := range t {
				if k != "role" && k != "type" && k != "id" && k != "tool_use_id" && k != "name" && k != "signature" {
					keys = append(keys, k)
				}
			}
			sort.Strings(keys)
			for _, k := range keys {
				collect(t[k])
			}
		}
	}
	var sys any
	_ = json.Unmarshal(req.System, &sys)
	collect(sys)
	for _, m := range req.Messages {
		var v any
		_ = json.Unmarshal(m, &v)
		collect(v)
	}
	return strings.Join(parts, "\n")
}

func (f *Fake) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	f.mu.Lock()
	f.bodies = append(f.bodies, body)
	f.heads = append(f.heads, r.Header.Clone())
	f.mu.Unlock()

	var req request
	if err := json.Unmarshal(body, &req); err != nil {
		http.Error(w, `{"error":{"message":"bad json","type":"invalid_request_error"}}`, http.StatusBadRequest)
		return
	}
	prompt := promptText(req)
	if f.OnRequest != nil {
		f.OnRequest(r.URL.Path, prompt)
	}
	reply := DefaultReply
	if f.Reply != nil {
		reply = f.Reply
	}
	text := reply(prompt)
	model := req.Model
	if f.Model != "" {
		model = f.Model
	}
	var args string
	if len(req.Tools) > 0 {
		email := "<EMAIL_1>"
		if m := placeholderRe.FindString(prompt); m != "" && !strings.Contains(prompt, email) {
			email = m
		}
		a, _ := json.Marshal(map[string]string{"to": email, "subject": "Your refund"})
		args = string(a)
	}
	in, out := len(prompt)/4+1, len(text)/4+1

	switch r.URL.Path {
	case "/v1/chat/completions":
		if req.Stream {
			f.openAIStream(w, model, text, args, in, out, body)
		} else {
			f.openAIJSON(w, model, text, args, in, out)
		}
	case "/v1/messages":
		if req.Stream {
			f.anthropicStream(w, model, text, args, in, out)
		} else {
			f.anthropicJSON(w, model, text, args, in, out)
		}
	default:
		http.NotFound(w, r)
	}
}

func (f *Fake) chunks(s string) []string {
	n := f.ChunkSize
	if n <= 0 {
		n = 3
	}
	var out []string
	for len(s) > n {
		out = append(out, s[:n])
		s = s[n:]
	}
	if s != "" {
		out = append(out, s)
	}
	return out
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func sse(w http.ResponseWriter, event string, v any) {
	b, _ := json.Marshal(v)
	if event != "" {
		fmt.Fprintf(w, "event: %s\n", event)
	}
	fmt.Fprintf(w, "data: %s\n\n", b)
	if fl, ok := w.(http.Flusher); ok {
		fl.Flush()
	}
}

func (f *Fake) openAIJSON(w http.ResponseWriter, model, text, args string, in, out int) {
	msg := map[string]any{"role": "assistant", "content": text}
	finish := "stop"
	if args != "" {
		msg["tool_calls"] = []any{map[string]any{"id": "call_1", "type": "function", "function": map[string]any{"name": "send_email", "arguments": args}}}
		finish = "tool_calls"
	}
	writeJSON(w, map[string]any{
		"id": "chatcmpl-fake", "object": "chat.completion", "created": 1762646400, "model": model,
		"choices": []any{map[string]any{"index": 0, "message": msg, "finish_reason": finish}},
		"usage":   map[string]any{"prompt_tokens": in, "completion_tokens": out, "total_tokens": in + out, "prompt_tokens_details": map[string]any{"cached_tokens": 0}},
	})
}

func (f *Fake) openAIStream(w http.ResponseWriter, model, text, args string, in, out int, body []byte) {
	var opts struct {
		StreamOptions struct {
			IncludeUsage bool `json:"include_usage"`
		} `json:"stream_options"`
	}
	_ = json.Unmarshal(body, &opts)
	w.Header().Set("Content-Type", "text/event-stream")
	base := func(choices []any) map[string]any {
		return map[string]any{"id": "chatcmpl-fake", "object": "chat.completion.chunk", "created": 1762646400, "model": model, "choices": choices}
	}
	sse(w, "", base([]any{map[string]any{"index": 0, "delta": map[string]any{"role": "assistant", "content": ""}, "finish_reason": nil}}))
	for _, c := range f.chunks(text) {
		sse(w, "", base([]any{map[string]any{"index": 0, "delta": map[string]any{"content": c}, "finish_reason": nil}}))
	}
	finish := "stop"
	if args != "" {
		finish = "tool_calls"
		sse(w, "", base([]any{map[string]any{"index": 0, "delta": map[string]any{"tool_calls": []any{map[string]any{"index": 0, "id": "call_1", "type": "function", "function": map[string]any{"name": "send_email", "arguments": ""}}}}, "finish_reason": nil}}))
		for _, c := range f.chunks(args) {
			sse(w, "", base([]any{map[string]any{"index": 0, "delta": map[string]any{"tool_calls": []any{map[string]any{"index": 0, "function": map[string]any{"arguments": c}}}}, "finish_reason": nil}}))
		}
	}
	sse(w, "", base([]any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": finish}}))
	if opts.StreamOptions.IncludeUsage {
		u := base([]any{})
		u["usage"] = map[string]any{"prompt_tokens": in, "completion_tokens": out, "total_tokens": in + out}
		sse(w, "", u)
	}
	fmt.Fprint(w, "data: [DONE]\n\n")
}

func (f *Fake) anthropicJSON(w http.ResponseWriter, model, text, args string, in, out int) {
	content := []any{map[string]any{"type": "text", "text": text}}
	stop := "end_turn"
	if args != "" {
		var input map[string]any
		_ = json.Unmarshal([]byte(args), &input)
		content = append(content, map[string]any{"type": "tool_use", "id": "toolu_1", "name": "send_email", "input": input})
		stop = "tool_use"
	}
	writeJSON(w, map[string]any{
		"id": "msg_fake", "type": "message", "role": "assistant", "model": model, "content": content,
		"stop_reason": stop, "usage": map[string]any{"input_tokens": in, "output_tokens": out, "cache_read_input_tokens": 0, "cache_creation_input_tokens": 0},
	})
}

func (f *Fake) anthropicStream(w http.ResponseWriter, model, text, args string, in, out int) {
	w.Header().Set("Content-Type", "text/event-stream")
	sse(w, "message_start", map[string]any{"type": "message_start", "message": map[string]any{
		"id": "msg_fake", "type": "message", "role": "assistant", "model": model, "content": []any{},
		"usage": map[string]any{"input_tokens": in, "output_tokens": 1, "cache_read_input_tokens": 0, "cache_creation_input_tokens": 0},
	}})
	sse(w, "content_block_start", map[string]any{"type": "content_block_start", "index": 0, "content_block": map[string]any{"type": "text", "text": ""}})
	sse(w, "ping", map[string]any{"type": "ping"})
	for _, c := range f.chunks(text) {
		sse(w, "content_block_delta", map[string]any{"type": "content_block_delta", "index": 0, "delta": map[string]any{"type": "text_delta", "text": c}})
	}
	sse(w, "content_block_stop", map[string]any{"type": "content_block_stop", "index": 0})
	stop := "end_turn"
	if args != "" {
		stop = "tool_use"
		sse(w, "content_block_start", map[string]any{"type": "content_block_start", "index": 1, "content_block": map[string]any{"type": "tool_use", "id": "toolu_1", "name": "send_email", "input": map[string]any{}}})
		for _, c := range f.chunks(args) {
			sse(w, "content_block_delta", map[string]any{"type": "content_block_delta", "index": 1, "delta": map[string]any{"type": "input_json_delta", "partial_json": c}})
		}
		sse(w, "content_block_stop", map[string]any{"type": "content_block_stop", "index": 1})
	}
	sse(w, "message_delta", map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": stop}, "usage": map[string]any{"output_tokens": out}})
	sse(w, "message_stop", map[string]any{"type": "message_stop"})
}
