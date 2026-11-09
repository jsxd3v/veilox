// Package pricing reads prices.yaml and turns token counts into a cost.
package pricing

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// Price is USD per 1 million tokens. Nil fields fall back to Input.
type Price struct {
	Input       float64  `yaml:"input"`
	Output      float64  `yaml:"output"`
	CachedInput *float64 `yaml:"cached_input"` // OpenAI wording
	CacheRead   *float64 `yaml:"cache_read"`   // Anthropic wording; same meaning
	CacheWrite  *float64 `yaml:"cache_write"`
}

// Table is a loaded price file.
type Table struct {
	exact    map[string]Price
	prefixes []prefix // longest first
}

type prefix struct {
	p     string
	price Price
}

type file struct {
	Currency string           `yaml:"currency"`
	Models   map[string]Price `yaml:"models"`
}

// Load reads a price file. A missing file gives an empty table, so every model is unpriced.
func Load(path string) (*Table, error) {
	data, err := os.ReadFile(path) // #nosec G304 -- path comes from the operator's config
	if errors.Is(err, os.ErrNotExist) {
		return &Table{exact: map[string]Price{}}, nil
	}
	if err != nil {
		return nil, err
	}
	return Parse(data)
}

// Parse reads a price file's content.
func Parse(data []byte) (*Table, error) {
	var f file
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&f); err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	if f.Currency != "" && f.Currency != "USD" {
		return nil, fmt.Errorf("currency %q: only USD is supported", f.Currency)
	}
	t := &Table{exact: map[string]Price{}}
	for name, p := range f.Models {
		for field, v := range map[string]*float64{"input": &p.Input, "output": &p.Output, "cached_input": p.CachedInput, "cache_read": p.CacheRead, "cache_write": p.CacheWrite} {
			if v != nil && (*v < 0 || math.IsNaN(*v) || math.IsInf(*v, 0)) {
				return nil, fmt.Errorf("model %q: %s must be a positive number", name, field)
			}
		}
		if base, ok := strings.CutSuffix(name, "*"); ok {
			if strings.Contains(base, "*") {
				return nil, fmt.Errorf("model %q: * is only allowed at the end", name)
			}
			t.prefixes = append(t.prefixes, prefix{base, p})
		} else {
			if strings.Contains(name, "*") {
				return nil, fmt.Errorf("model %q: * is only allowed at the end", name)
			}
			t.exact[name] = p
		}
	}
	sort.Slice(t.prefixes, func(i, j int) bool {
		if len(t.prefixes[i].p) != len(t.prefixes[j].p) {
			return len(t.prefixes[i].p) > len(t.prefixes[j].p)
		}
		return t.prefixes[i].p < t.prefixes[j].p
	})
	return t, nil
}

// Lookup finds the price for a model: exact name first, then the longest matching prefix.
func (t *Table) Lookup(model string) (Price, bool) {
	if model == "" {
		return Price{}, false
	}
	if p, ok := t.exact[model]; ok {
		return p, true
	}
	for _, p := range t.prefixes {
		if strings.HasPrefix(model, p.p) {
			return p.price, true
		}
	}
	return Price{}, false
}

// Len is the number of entries.
func (t *Table) Len() int { return len(t.exact) + len(t.prefixes) }

// Usage is a request's token counts. Input excludes cached tokens.
type Usage struct {
	Input      int64
	Output     int64
	CacheRead  int64
	CacheWrite int64
}

// Cost returns the cost in micro-dollars (1 USD = 1,000,000).
//
// Prices are per million tokens, so tokens × price is already in micro-dollars.
func (p Price) Cost(u Usage) int64 {
	read := p.Input
	if p.CacheRead != nil {
		read = *p.CacheRead
	} else if p.CachedInput != nil {
		read = *p.CachedInput
	}
	write := p.Input
	if p.CacheWrite != nil {
		write = *p.CacheWrite
	}
	c := float64(u.Input)*p.Input + float64(u.Output)*p.Output + float64(u.CacheRead)*read + float64(u.CacheWrite)*write
	return int64(math.Round(c))
}
