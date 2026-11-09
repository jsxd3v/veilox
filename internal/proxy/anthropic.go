package proxy

import (
	"net/http"
	"sort"

	"github.com/jsxd3v/veilox/internal/config"
	"github.com/jsxd3v/veilox/internal/mask"
	"github.com/jsxd3v/veilox/internal/pricing"
)

// anthropic handles POST /v1/messages.
type anthropic struct{}

func (anthropic) name() string { return config.Anthropic }
func (anthropic) path() string { return "/v1/messages" }

func (anthropic) setAuth(h http.Header, key string) { h.Set("x-api-key", key) }

// walk calls f on the system prompt, text blocks, tool inputs and tool results. Thinking blocks are
// left untouched: they are signed by the provider and must come back exactly as they were sent.
func (anthropic) walk(body map[string]any, f func(string) string) []string {
	var unsupported []string
	switch s := body["system"].(type) {
	case string:
		body["system"] = f(s)
	case []any:
		unsupported = append(unsupported, walkAnthropicBlocks(s, f)...)
	}
	for _, msg := range asSlice(body["messages"]) {
		m := asMap(msg)
		if m == nil {
			continue
		}
		switch c := m["content"].(type) {
		case string:
			m["content"] = f(c)
		case []any:
			unsupported = append(unsupported, walkAnthropicBlocks(c, f)...)
		}
	}
	return unsupported
}

func walkAnthropicBlocks(blocks []any, f func(string) string) []string {
	var unsupported []string
	for _, b := range blocks {
		blk := asMap(b)
		if blk == nil {
			continue
		}
		typ, _ := asString(blk["type"])
		switch typ {
		case "text":
			if s, ok := asString(blk["text"]); ok {
				blk["text"] = f(s)
			}
		case "tool_use":
			blk["input"] = mask.MapStrings(blk["input"], f)
		case "tool_result":
			switch c := blk["content"].(type) {
			case string:
				blk["content"] = f(c)
			case []any:
				unsupported = append(unsupported, walkAnthropicBlocks(c, f)...)
			}
		case "thinking", "redacted_thinking":
			// signed by the provider: never modified
		default:
			unsupported = append(unsupported, typ)
		}
	}
	return unsupported
}

func (anthropic) addHint(body map[string]any, hint string) {
	switch s := body["system"].(type) {
	case string:
		body["system"] = s + "\n\n" + hint
	case []any:
		body["system"] = append(s, map[string]any{"type": "text", "text": hint})
	default:
		body["system"] = hint
	}
}

func (anthropic) prepareStream(map[string]any) bool { return false } // usage is always in the stream

func (anthropic) restore(body map[string]any, m *mask.Mapper) {
	for _, b := range asSlice(body["content"]) {
		restoreAnthropicBlock(asMap(b), m)
	}
	restoreError(body, m)
}

func restoreAnthropicBlock(blk map[string]any, m *mask.Mapper) {
	if blk == nil {
		return
	}
	switch blk["type"] {
	case "text":
		if s, ok := asString(blk["text"]); ok {
			blk["text"] = m.Restore(s)
		}
	case "tool_use":
		if in, ok := blk["input"]; ok {
			blk["input"] = m.RestoreValue(in)
		}
	}
}

func (anthropic) usage(body map[string]any) (string, pricing.Usage, bool) {
	model, _ := asString(body["model"])
	u := asMap(body["usage"])
	if u == nil {
		return model, pricing.Usage{}, false
	}
	return model, anthropicUsage(u, pricing.Usage{}), true
}

// anthropicUsage reads a usage object over prev; fields that are missing keep their old value.
func anthropicUsage(u map[string]any, prev pricing.Usage) pricing.Usage {
	if _, ok := u["input_tokens"]; ok {
		prev.Input = asInt(u["input_tokens"])
	}
	if _, ok := u["output_tokens"]; ok {
		prev.Output = asInt(u["output_tokens"])
	}
	if _, ok := u["cache_read_input_tokens"]; ok {
		prev.CacheRead = asInt(u["cache_read_input_tokens"])
	}
	if _, ok := u["cache_creation_input_tokens"]; ok {
		prev.CacheWrite = asInt(u["cache_creation_input_tokens"])
	}
	return prev
}

func (anthropic) errorType(body map[string]any) string {
	s, _ := asString(asMap(body["error"])["type"])
	return s
}

func (anthropic) errorBody(status int, _ string, msg string) any {
	typ := "invalid_request_error"
	switch status {
	case http.StatusUnauthorized:
		typ = "authentication_error"
	case http.StatusNotFound:
		typ = "not_found_error"
	case http.StatusRequestEntityTooLarge:
		typ = "request_too_large"
	case http.StatusTooManyRequests:
		typ = "rate_limit_error"
	case http.StatusBadGateway, http.StatusGatewayTimeout, http.StatusServiceUnavailable, http.StatusInternalServerError:
		typ = "api_error"
	}
	return map[string]any{"type": "error", "error": map[string]any{"type": typ, "message": msg}}
}

func (anthropic) newStream(m *mask.Mapper, _ bool) streamer {
	return &anthropicStream{m: m, blocks: map[int64]*anthropicBlock{}}
}

// ---- streaming ----

type anthropicBlock struct {
	st       *mask.Stream
	field    string // "text" or "partial_json"
	deltaTyp string // "text_delta" or "input_json_delta"
}

type anthropicStream struct {
	m         *mask.Mapper
	blocks    map[int64]*anthropicBlock
	model     string
	u         pricing.Usage
	haveUsage bool
}

func (s *anthropicStream) event(ev *event) []*event {
	data, err := decodeObject([]byte(ev.data))
	if err != nil {
		return []*event{ev}
	}
	typ, _ := asString(data["type"])
	idx := asInt(data["index"])
	switch typ {
	case "message_start":
		msg := asMap(data["message"])
		if m, ok := asString(msg["model"]); ok {
			s.model = m
		}
		if u := asMap(msg["usage"]); u != nil {
			s.u, s.haveUsage = anthropicUsage(u, s.u), true
		}
	case "message_delta":
		if u := asMap(data["usage"]); u != nil {
			s.u, s.haveUsage = anthropicUsage(u, s.u), true
		}
	case "content_block_start":
		blk := asMap(data["content_block"])
		switch blk["type"] {
		case "text":
			b := &anthropicBlock{st: s.m.NewStream(false), field: "text", deltaTyp: "text_delta"}
			s.blocks[idx] = b
			if t, ok := asString(blk["text"]); ok && t != "" {
				blk["text"] = b.st.Write(t)
				ev.setData(data)
			}
		case "tool_use":
			s.blocks[idx] = &anthropicBlock{st: s.m.NewStream(true), field: "partial_json", deltaTyp: "input_json_delta"}
			if in := asMap(blk["input"]); len(in) > 0 {
				blk["input"] = s.m.RestoreValue(in)
				ev.setData(data)
			}
		}
	case "content_block_delta":
		b := s.blocks[idx]
		delta := asMap(data["delta"])
		if b == nil || delta == nil {
			break
		}
		if t, ok := asString(delta[b.field]); ok {
			delta[b.field] = b.st.Write(t)
			ev.setData(data)
		}
	case "content_block_stop":
		if out := s.flushBlock(idx); out != nil {
			return []*event{out, ev}
		}
	case "error":
		restoreError(data, s.m)
		ev.setData(data)
	}
	return []*event{ev}
}

// flushBlock returns a delta event with the block's held-back text, or nil.
func (s *anthropicStream) flushBlock(idx int64) *event {
	b := s.blocks[idx]
	if b == nil || !b.st.Pending() {
		return nil
	}
	tail := b.st.Flush()
	return newEvent("content_block_delta", map[string]any{
		"type":  "content_block_delta",
		"index": idx,
		"delta": map[string]any{"type": b.deltaTyp, b.field: tail},
	})
}

func (s *anthropicStream) end() []*event {
	idxs := make([]int64, 0, len(s.blocks))
	for i := range s.blocks {
		idxs = append(idxs, i)
	}
	sort.Slice(idxs, func(a, b int) bool { return idxs[a] < idxs[b] })
	var out []*event
	for _, i := range idxs {
		if ev := s.flushBlock(i); ev != nil {
			out = append(out, ev)
		}
	}
	return out
}

func (s *anthropicStream) usage() (string, pricing.Usage, bool) { return s.model, s.u, s.haveUsage }
