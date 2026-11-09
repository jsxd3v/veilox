// Package mask replaces detected values with placeholders like <EMAIL_1> and puts them back.
//
// A Mapper lives for one request only. It holds the placeholder ↔ value mapping in memory; nothing in
// this package writes values anywhere.
package mask

import (
	"encoding/json"
	"regexp"
	"strconv"
	"strings"

	"github.com/jsxd3v/veilox/internal/detect"
)

// placeholderRe matches anything shaped like a placeholder. Only issued ones are ever replaced.
var placeholderRe = regexp.MustCompile(`<([A-Z][A-Z0-9_]*)_([0-9]+)>`)

// Finding describes one masked value (used by `veilox scan`).
type Finding struct {
	Start, End  int // byte offsets in the input text
	Type        string
	Placeholder string
	Value       string
}

// Mapper masks and restores the texts of one request. It is not safe for concurrent use.
type Mapper struct {
	engine   *detect.Engine
	byKey    map[string]string // type + "\x00" + key -> placeholder
	values   map[string]string // placeholder -> first spelling seen
	next     map[string]int    // type -> last number used
	reserved map[string]map[int]bool
	counts   map[string]int
	maxLen   int
}

// NewMapper returns an empty mapper.
func NewMapper(e *detect.Engine) *Mapper {
	return &Mapper{
		engine:   e,
		byKey:    map[string]string{},
		values:   map[string]string{},
		next:     map[string]int{},
		reserved: map[string]map[int]bool{},
		counts:   map[string]int{},
	}
}

// Reserve records placeholder-like text that is already in the input, so those numbers are never
// issued. Call it on every text of the request before the first Mask.
func (m *Mapper) Reserve(text string) string {
	for _, sm := range placeholderRe.FindAllStringSubmatch(text, -1) {
		n, err := strconv.Atoi(sm[2])
		if err != nil {
			continue
		}
		if m.reserved[sm[1]] == nil {
			m.reserved[sm[1]] = map[int]bool{}
		}
		m.reserved[sm[1]][n] = true
	}
	return text
}

// Mask replaces every detected value in text with its placeholder.
func (m *Mapper) Mask(text string) string {
	out, _ := m.MaskText(text)
	return out
}

// MaskText is Mask that also returns what was found.
func (m *Mapper) MaskText(text string) (string, []Finding) {
	matches := m.engine.Find(text)
	if len(matches) == 0 {
		return text, nil
	}
	var b strings.Builder
	findings := make([]Finding, 0, len(matches))
	last := 0
	for _, mt := range matches {
		ph := m.placeholder(mt)
		b.WriteString(text[last:mt.Start])
		b.WriteString(ph)
		last = mt.End
		m.counts[mt.Type]++
		findings = append(findings, Finding{Start: mt.Start, End: mt.End, Type: mt.Type, Placeholder: ph, Value: mt.Value})
	}
	b.WriteString(text[last:])
	return b.String(), findings
}

func (m *Mapper) placeholder(mt detect.Match) string {
	k := mt.Type + "\x00" + mt.Key
	if ph, ok := m.byKey[k]; ok {
		return ph
	}
	n := m.next[mt.Type] + 1
	for m.reserved[mt.Type][n] {
		n++
	}
	m.next[mt.Type] = n
	ph := "<" + mt.Type + "_" + strconv.Itoa(n) + ">"
	m.byKey[k] = ph
	m.values[ph] = mt.Value
	if len(ph) > m.maxLen {
		m.maxLen = len(ph)
	}
	return ph
}

// Counts returns how many values were masked, per type.
func (m *Mapper) Counts() map[string]int {
	out := make(map[string]int, len(m.counts))
	for k, v := range m.counts {
		out[k] = v
	}
	return out
}

// Total is the number of values masked.
func (m *Mapper) Total() int {
	n := 0
	for _, v := range m.counts {
		n += v
	}
	return n
}

// Issued reports whether any placeholder was issued.
func (m *Mapper) Issued() bool { return len(m.values) > 0 }

// Restore replaces issued placeholders in text with the real values.
func (m *Mapper) Restore(text string) string { return m.restore(text, false) }

// RestoreJSON is Restore for text that is itself JSON (tool-call arguments): values are inserted
// JSON-escaped so that the JSON stays valid.
func (m *Mapper) RestoreJSON(text string) string { return m.restore(text, true) }

func (m *Mapper) restore(text string, escape bool) string {
	if len(m.values) == 0 {
		return text
	}
	if !escape {
		if !strings.Contains(text, "<") {
			return text
		}
		return placeholderRe.ReplaceAllStringFunc(text, func(ph string) string {
			if v, ok := m.values[ph]; ok {
				return v
			}
			return ph
		})
	}
	// JSON text may spell < and > as \u003c and \u003e (Go and some providers escape them).
	if !strings.Contains(text, "<") && !strings.Contains(text, `\u003`) {
		return text
	}
	return jsonPlaceholderRe.ReplaceAllStringFunc(text, func(found string) string {
		sm := jsonPlaceholderRe.FindStringSubmatch(found)
		if v, ok := m.values["<"+sm[1]+">"]; ok {
			return jsonEscape(v)
		}
		return found
	})
}

// jsonPlaceholderRe matches a placeholder in JSON text, with < and > written plainly or escaped.
var jsonPlaceholderRe = regexp.MustCompile(`(?:<|\\u003[cC])([A-Z][A-Z0-9_]*_[0-9]+)(?:>|\\u003[eE])`)

func jsonEscape(s string) string {
	var b strings.Builder
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(s) // a string always encodes
	out := strings.TrimSuffix(b.String(), "\n")
	return out[1 : len(out)-1]
}

// RestoreValue walks a decoded JSON value and restores placeholders in every string (map keys too).
func (m *Mapper) RestoreValue(v any) any {
	switch t := v.(type) {
	case string:
		return m.Restore(t)
	case []any:
		for i := range t {
			t[i] = m.RestoreValue(t[i])
		}
		return t
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, val := range t {
			out[m.Restore(k)] = m.RestoreValue(val)
		}
		return out
	default:
		return v
	}
}

// MapStrings applies f to every string value (not keys) inside a decoded JSON value, in place.
func MapStrings(v any, f func(string) string) any {
	switch t := v.(type) {
	case string:
		return f(t)
	case []any:
		for i := range t {
			t[i] = MapStrings(t[i], f)
		}
	case map[string]any:
		for k, val := range t {
			t[k] = MapStrings(val, f)
		}
	}
	return v
}

// Stream restores placeholders in text that arrives in pieces. A placeholder split across pieces
// ("<NA" + "ME_1>") is held back until it is complete, so the output never contains half of one.
type Stream struct {
	m      *Mapper
	escape bool
	held   string
}

// NewStream starts a stream restorer. Use json=true for streamed JSON text (tool-call arguments).
func (m *Mapper) NewStream(json bool) *Stream { return &Stream{m: m, escape: json} }

// Write takes the next piece and returns the text that is safe to emit now.
func (s *Stream) Write(piece string) string {
	text := s.held + piece
	cut := s.holdFrom(text)
	s.held = text[cut:]
	return s.m.restore(text[:cut], s.escape)
}

// Flush returns whatever is still held back. Call it when the stream ends.
func (s *Stream) Flush() string {
	text := s.held
	s.held = ""
	return s.m.restore(text, s.escape)
}

// Pending reports whether text is held back.
func (s *Stream) Pending() bool { return s.held != "" }

// holdFrom returns where the tail that could still become an issued placeholder starts.
func (s *Stream) holdFrom(text string) int {
	if s.m.maxLen == 0 {
		return len(text)
	}
	if !s.escape {
		// Placeholders contain exactly one '<', so only the last '<' can start one.
		i := strings.LastIndexByte(text, '<')
		if i < 0 || len(text)-i >= s.m.maxLen || !s.couldStart(text[i:]) {
			return len(text)
		}
		return i
	}
	// JSON text: the escaped form \u003cNAME_1\u003e is longer and contains several '\', so check
	// every '<' or '\' near the end and hold from the earliest one that could still grow into a placeholder.
	longest := s.m.maxLen + 10 // two 6-byte escapes instead of two 1-byte brackets
	from := max(len(text)-longest+1, 0)
	for i := from; i < len(text); i++ {
		if (text[i] == '<' || text[i] == '\\') && s.couldStart(text[i:]) {
			return i
		}
	}
	return len(text)
}

// couldStart reports whether tail is the unfinished beginning of an issued placeholder.
func (s *Stream) couldStart(tail string) bool {
	for ph := range s.m.values {
		if len(tail) < len(ph) && strings.HasPrefix(ph, tail) {
			return true
		}
		if s.escape {
			inner := ph[1 : len(ph)-1]
			for _, form := range []string{`\u003c` + inner + `\u003e`, `\u003C` + inner + `\u003E`} {
				if len(tail) < len(form) && strings.HasPrefix(form, tail) {
					return true
				}
			}
		}
	}
	return false
}
