package proxy

import (
	"net/http"
	"sort"

	"github.com/jsxd3v/veilox/internal/config"
	"github.com/jsxd3v/veilox/internal/mask"
	"github.com/jsxd3v/veilox/internal/pricing"
)

// openAI handles POST /v1/chat/completions (OpenAI and compatible APIs).
type openAI struct{}

func (openAI) name() string { return config.OpenAI }
func (openAI) path() string { return "/v1/chat/completions" }

func (openAI) setAuth(h http.Header, key string) { h.Set("Authorization", "Bearer "+key) }

// walk calls f on every text the provider would read: message content (string or text parts),
// refusals, and tool-call arguments. It returns the types of parts it can't mask (images, audio, files).
func (openAI) walk(body map[string]any, f func(string) string) []string {
	var unsupported []string
	for _, msg := range asSlice(body["messages"]) {
		m := asMap(msg)
		if m == nil {
			continue
		}
		switch c := m["content"].(type) {
		case string:
			m["content"] = f(c)
		case []any:
			for _, part := range c {
				p := asMap(part)
				if p == nil {
					continue
				}
				typ, _ := asString(p["type"])
				switch typ {
				case "text":
					if s, ok := asString(p["text"]); ok {
						p["text"] = f(s)
					}
				case "refusal":
					if s, ok := asString(p["refusal"]); ok {
						p["refusal"] = f(s)
					}
				default:
					unsupported = append(unsupported, typ)
				}
			}
		}
		if s, ok := asString(m["refusal"]); ok {
			m["refusal"] = f(s)
		}
		for _, tc := range asSlice(m["tool_calls"]) {
			fn := asMap(asMap(tc)["function"])
			if s, ok := asString(fn["arguments"]); ok {
				fn["arguments"] = f(s)
			}
		}
		if fn := asMap(m["function_call"]); fn != nil { // legacy function calling
			if s, ok := asString(fn["arguments"]); ok {
				fn["arguments"] = f(s)
			}
		}
	}
	return unsupported
}

func (openAI) addHint(body map[string]any, hint string) {
	msgs := asSlice(body["messages"])
	body["messages"] = append([]any{map[string]any{"role": "system", "content": hint}}, msgs...)
}

// prepareStream asks for token usage in the stream. It returns true if veilox added the option,
// in which case the extra usage chunk is removed before it reaches the app.
func (openAI) prepareStream(body map[string]any) bool {
	so := asMap(body["stream_options"])
	if so == nil {
		so = map[string]any{}
		body["stream_options"] = so
	}
	if v, _ := so["include_usage"].(bool); v {
		return false
	}
	so["include_usage"] = true
	return true
}

func (openAI) restore(body map[string]any, m *mask.Mapper) {
	for _, ch := range asSlice(body["choices"]) {
		msg := asMap(asMap(ch)["message"])
		if msg == nil {
			continue
		}
		restoreMessageFields(msg, m)
	}
	restoreError(body, m)
}

func restoreMessageFields(msg map[string]any, m *mask.Mapper) {
	for _, k := range []string{"content", "refusal"} {
		if s, ok := asString(msg[k]); ok {
			msg[k] = m.Restore(s)
		}
	}
	for _, tc := range asSlice(msg["tool_calls"]) {
		fn := asMap(asMap(tc)["function"])
		if s, ok := asString(fn["arguments"]); ok {
			fn["arguments"] = m.RestoreJSON(s)
		}
	}
	if fn := asMap(msg["function_call"]); fn != nil {
		if s, ok := asString(fn["arguments"]); ok {
			fn["arguments"] = m.RestoreJSON(s)
		}
	}
}

func restoreError(body map[string]any, m *mask.Mapper) {
	if e := asMap(body["error"]); e != nil {
		if s, ok := asString(e["message"]); ok {
			e["message"] = m.Restore(s)
		}
	}
}

func (openAI) usage(body map[string]any) (string, pricing.Usage, bool) {
	model, _ := asString(body["model"])
	u := asMap(body["usage"])
	if u == nil {
		return model, pricing.Usage{}, false
	}
	return model, openAIUsage(u), true
}

func openAIUsage(u map[string]any) pricing.Usage {
	prompt := asInt(u["prompt_tokens"])
	cached := asInt(asMap(u["prompt_tokens_details"])["cached_tokens"])
	if cached > prompt {
		cached = prompt
	}
	return pricing.Usage{Input: prompt - cached, CacheRead: cached, Output: asInt(u["completion_tokens"])}
}

func (openAI) errorType(body map[string]any) string {
	e := asMap(body["error"])
	if s, ok := asString(e["type"]); ok && s != "" {
		return s
	}
	if s, ok := asString(e["code"]); ok {
		return s
	}
	return ""
}

func (openAI) errorBody(status int, code, msg string) any {
	typ := "invalid_request_error"
	switch status {
	case http.StatusUnauthorized:
		typ = "authentication_error"
	case http.StatusTooManyRequests:
		typ = "budget_exceeded"
	case http.StatusBadGateway, http.StatusGatewayTimeout, http.StatusServiceUnavailable, http.StatusInternalServerError:
		typ = "server_error"
	}
	return map[string]any{"error": map[string]any{"message": msg, "type": typ, "code": code}}
}

func (openAI) newStream(m *mask.Mapper, injectedUsage bool) streamer {
	return &openAIStream{m: m, dropUsage: injectedUsage, texts: map[oaKey]*mask.Stream{}}
}

// ---- streaming ----

// oaKey identifies one text stream: the content of a choice, or the arguments of one of its tool calls.
type oaKey struct {
	choice int64
	tool   int64 // -1 for content
}

type openAIStream struct {
	m         *mask.Mapper
	dropUsage bool
	texts     map[oaKey]*mask.Stream
	template  map[string]any // id, created, model… of the last chunk, for chunks veilox adds
	model     string
	u         pricing.Usage
	haveUsage bool
}

func (s *openAIStream) get(k oaKey) *mask.Stream {
	st, ok := s.texts[k]
	if !ok {
		st = s.m.NewStream(k.tool >= 0)
		s.texts[k] = st
	}
	return st
}

func (s *openAIStream) event(ev *event) []*event {
	if ev.data == "[DONE]" {
		return append(s.flushAll(), ev)
	}
	chunk, err := decodeObject([]byte(ev.data))
	if err != nil {
		return []*event{ev}
	}
	s.template = map[string]any{}
	for _, k := range []string{"id", "object", "created", "model", "system_fingerprint"} {
		if v, ok := chunk[k]; ok {
			s.template[k] = v
		}
	}
	if m, ok := asString(chunk["model"]); ok && m != "" {
		s.model = m
	}
	changed := false
	if u := asMap(chunk["usage"]); u != nil {
		s.u, s.haveUsage = openAIUsage(u), true
		if s.dropUsage {
			if len(asSlice(chunk["choices"])) == 0 {
				return s.flushAll() // the usage-only chunk the app didn't ask for
			}
			delete(chunk, "usage")
			changed = true
		}
	}
	if e := asMap(chunk["error"]); e != nil {
		restoreError(chunk, s.m)
		changed = true
	}

	for _, c := range asSlice(chunk["choices"]) {
		ch := asMap(c)
		if ch == nil {
			continue
		}
		idx := asInt(ch["index"])
		delta := asMap(ch["delta"])
		if delta != nil {
			if text, ok := asString(delta["content"]); ok {
				delta["content"] = s.get(oaKey{idx, -1}).Write(text)
				changed = true
			}
			if text, ok := asString(delta["refusal"]); ok {
				delta["refusal"] = s.m.Restore(text)
				changed = true
			}
			for _, tc := range asSlice(delta["tool_calls"]) {
				t := asMap(tc)
				fn := asMap(t["function"])
				if args, ok := asString(fn["arguments"]); ok {
					fn["arguments"] = s.get(oaKey{idx, asInt(t["index"])}).Write(args)
					changed = true
				}
			}
		}
		if ch["finish_reason"] != nil {
			if delta == nil {
				delta = map[string]any{}
				ch["delta"] = delta
			}
			if s.flushChoice(idx, delta) {
				changed = true
			}
		}
	}
	if changed {
		ev.setData(chunk)
	}
	return []*event{ev}
}

// flushChoice moves held-back text of one choice into delta. It reports whether there was any.
func (s *openAIStream) flushChoice(idx int64, delta map[string]any) bool {
	flushed := false
	for _, k := range s.sortedKeys() {
		if k.choice != idx || !s.texts[k].Pending() {
			continue
		}
		flushed = true
		tail := s.texts[k].Flush()
		if k.tool < 0 {
			prev, _ := asString(delta["content"])
			delta["content"] = prev + tail
			continue
		}
		var target map[string]any
		for _, tc := range asSlice(delta["tool_calls"]) {
			if t := asMap(tc); t != nil && asInt(t["index"]) == k.tool {
				target = t
			}
		}
		if target == nil {
			target = map[string]any{"index": k.tool, "function": map[string]any{}}
			delta["tool_calls"] = append(asSlice(delta["tool_calls"]), target)
		}
		fn := asMap(target["function"])
		if fn == nil {
			fn = map[string]any{}
			target["function"] = fn
		}
		prev, _ := asString(fn["arguments"])
		fn["arguments"] = prev + tail
	}
	return flushed
}

// flushAll returns a chunk carrying any text still held back (normally there is none, because
// each choice is flushed at its finish_reason).
func (s *openAIStream) flushAll() []*event {
	var choices []any
	seen := map[int64]bool{}
	for _, k := range s.sortedKeys() {
		if !s.texts[k].Pending() || seen[k.choice] {
			continue
		}
		seen[k.choice] = true
		delta := map[string]any{}
		s.flushChoice(k.choice, delta)
		choices = append(choices, map[string]any{"index": k.choice, "delta": delta, "finish_reason": nil})
	}
	if len(choices) == 0 {
		return nil
	}
	chunk := map[string]any{"object": "chat.completion.chunk"}
	for k, v := range s.template {
		chunk[k] = v
	}
	chunk["choices"] = choices
	return []*event{newEvent("", chunk)}
}

func (s *openAIStream) sortedKeys() []oaKey {
	keys := make([]oaKey, 0, len(s.texts))
	for k := range s.texts {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].choice != keys[j].choice {
			return keys[i].choice < keys[j].choice
		}
		return keys[i].tool < keys[j].tool
	})
	return keys
}

func (s *openAIStream) end() []*event { return s.flushAll() }

func (s *openAIStream) usage() (string, pricing.Usage, bool) { return s.model, s.u, s.haveUsage }
