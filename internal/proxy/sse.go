package proxy

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"strings"
)

// event is one server-sent event. Events that veilox doesn't change are written back byte for byte.
type event struct {
	lines   []string // raw lines, without line endings
	name    string   // "event:" field
	data    string   // "data:" fields joined with \n
	changed bool
}

// readEvent reads the next event. It returns io.EOF after the last one.
func readEvent(br *bufio.Reader) (*event, error) {
	ev := &event{}
	var data []string
	done := func() *event {
		ev.data = strings.Join(data, "\n")
		return ev
	}
	for {
		raw, err := br.ReadString('\n')
		if raw != "" {
			line := strings.TrimRight(raw, "\r\n")
			if line == "" {
				if len(ev.lines) > 0 {
					return done(), nil
				}
			} else {
				ev.lines = append(ev.lines, line)
				field, value, _ := strings.Cut(line, ":")
				value = strings.TrimPrefix(value, " ")
				switch field {
				case "event":
					ev.name = value
				case "data":
					data = append(data, value)
				}
			}
		}
		if err != nil {
			if err == io.EOF && len(ev.lines) > 0 {
				return done(), nil // last event without a closing blank line
			}
			return nil, err
		}
	}
}

// newEvent builds an event that veilox adds to a stream.
func newEvent(name string, v any) *event {
	return &event{name: name, data: string(encodeJSON(v)), changed: true}
}

// setData replaces the event's data with v.
func (e *event) setData(v any) {
	e.data = string(encodeJSON(v))
	e.changed = true
}

func (e *event) bytes() []byte {
	var b bytes.Buffer
	if !e.changed {
		for _, l := range e.lines {
			b.WriteString(l)
			b.WriteByte('\n')
		}
		b.WriteByte('\n')
		return b.Bytes()
	}
	for _, l := range e.lines { // keep id:, retry: and comments
		if f, _, _ := strings.Cut(l, ":"); f != "event" && f != "data" {
			b.WriteString(l)
			b.WriteByte('\n')
		}
	}
	if e.name != "" {
		b.WriteString("event: " + e.name + "\n")
	}
	for _, d := range strings.Split(e.data, "\n") {
		b.WriteString("data: " + d + "\n")
	}
	b.WriteByte('\n')
	return b.Bytes()
}

// ---- JSON helpers ----

// decodeObject decodes a JSON object, keeping numbers exact.
func decodeObject(data []byte) (map[string]any, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var v map[string]any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	if v == nil {
		return nil, errNotObject
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, errNotObject
	}
	return v, nil
}

type jsonError string

func (e jsonError) Error() string { return string(e) }

const errNotObject = jsonError("body must be one JSON object")

// encodeJSON encodes without escaping <, > and & (placeholders stay readable).
func encodeJSON(v any) []byte {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return []byte("null")
	}
	return bytes.TrimSuffix(b.Bytes(), []byte("\n"))
}

func asMap(v any) map[string]any {
	m, _ := v.(map[string]any)
	return m
}

func asSlice(v any) []any {
	s, _ := v.([]any)
	return s
}

func asString(v any) (string, bool) {
	s, ok := v.(string)
	return s, ok
}

func asInt(v any) int64 {
	switch n := v.(type) {
	case json.Number:
		if i, err := n.Int64(); err == nil {
			return i
		}
		if f, err := n.Float64(); err == nil {
			return int64(f)
		}
	case float64:
		return int64(n)
	case int:
		return int64(n)
	case int64:
		return n
	}
	return 0
}
