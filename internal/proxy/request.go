package proxy

import (
	"fmt"

	"github.com/jsxd3v/veilox/internal/config"
)

// WalkRequest calls f on every text of an API request body that the gateway masks, exactly as the
// gateway does. api is "openai" or "anthropic". It returns the types of parts that can't be masked.
func WalkRequest(api string, body map[string]any, f func(string) string) ([]string, error) {
	switch api {
	case config.OpenAI:
		return openAI{}.walk(body, f), nil
	case config.Anthropic:
		return anthropic{}.walk(body, f), nil
	}
	return nil, fmt.Errorf("unknown API %q (use openai or anthropic)", api)
}

// GuessAPI says which API a request body is for: a top-level "system" field means Anthropic.
func GuessAPI(body map[string]any) string {
	if _, ok := body["system"]; ok {
		return config.Anthropic
	}
	return config.OpenAI
}

// DecodeRequest parses a request body the way the gateway does.
func DecodeRequest(data []byte) (map[string]any, error) { return decodeObject(data) }

// EncodeJSON encodes like the gateway (no HTML escaping).
func EncodeJSON(v any) []byte { return encodeJSON(v) }
