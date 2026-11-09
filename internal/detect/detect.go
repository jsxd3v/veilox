// Package detect finds personal data in text. Detection is deterministic: the same text and the same
// options always give the same matches.
package detect

import (
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strings"
)

// Built-in detector types.
const (
	Email      = "EMAIL"
	Phone      = "PHONE"
	CreditCard = "CREDIT_CARD"
	IBAN       = "IBAN"
	IPv4       = "IPV4"
	Name       = "NAME"
)

// BuiltinNames maps config names to types, in the default order.
var BuiltinNames = map[string]string{
	"email":       Email,
	"phone":       Phone,
	"credit_card": CreditCard,
	"iban":        IBAN,
	"ipv4":        IPv4,
}

// DefaultBuiltins is the list used when the config doesn't name detectors.
var DefaultBuiltins = []string{"email", "phone", "credit_card", "iban", "ipv4"}

// Match is one finding. Start and End are byte offsets into the text.
type Match struct {
	Start, End int
	Type       string
	Value      string
	// Key is the normalised value: two matches with the same Type and Key are the same thing
	// (e.g. the same email in different case).
	Key string
}

// Rule is a custom regex rule from the config.
type Rule struct {
	Name    string
	Pattern string
	Group   int
}

// Options configure an Engine.
type Options struct {
	Builtins        []string // config names, e.g. "email"; nil means DefaultBuiltins
	SkipPrivateIPv4 bool
	Names           []string
	Rules           []Rule
}

type detector interface {
	kind() string
	find(text string) []Match
}

// Engine runs all configured detectors and resolves overlaps. It is safe for concurrent use.
type Engine struct {
	detectors []detector
	priority  map[string]int
}

var ruleNameRe = regexp.MustCompile(`^[A-Z][A-Z0-9_]*$`)

// New builds an Engine.
func New(opts Options) (*Engine, error) {
	builtins := opts.Builtins
	if builtins == nil {
		builtins = DefaultBuiltins
	}
	enabled := map[string]bool{}
	for _, b := range builtins {
		t, ok := BuiltinNames[b]
		if !ok {
			return nil, fmt.Errorf("unknown detector %q (known: email, phone, credit_card, iban, ipv4)", b)
		}
		enabled[t] = true
	}

	e := &Engine{priority: map[string]int{}}
	// Fixed priority for equal-length overlaps: lower wins.
	order := []string{CreditCard, IBAN, Email, IPv4, Phone}
	for i, t := range order {
		e.priority[t] = i
		if !enabled[t] {
			continue
		}
		switch t {
		case CreditCard:
			e.detectors = append(e.detectors, cardDetector{})
		case IBAN:
			e.detectors = append(e.detectors, ibanDetector{})
		case Email:
			e.detectors = append(e.detectors, emailDetector{})
		case IPv4:
			e.detectors = append(e.detectors, ipv4Detector{skipPrivate: opts.SkipPrivateIPv4})
		case Phone:
			e.detectors = append(e.detectors, phoneDetector{})
		}
	}

	seen := map[string]bool{}
	for i, r := range opts.Rules {
		if !ruleNameRe.MatchString(r.Name) {
			return nil, fmt.Errorf("rule %q: name must be upper case letters, digits and _ (e.g. CUSTOMER_ID)", r.Name)
		}
		if _, builtin := e.priority[r.Name]; builtin || r.Name == Name {
			return nil, fmt.Errorf("rule %q: name is used by a built-in detector", r.Name)
		}
		if seen[r.Name] {
			return nil, fmt.Errorf("rule %q: defined twice", r.Name)
		}
		seen[r.Name] = true
		re, err := regexp.Compile(r.Pattern)
		if err != nil {
			return nil, fmt.Errorf("rule %q: invalid pattern: %w", r.Name, err)
		}
		if r.Group < 0 || r.Group > re.NumSubexp() {
			return nil, fmt.Errorf("rule %q: group %d does not exist in the pattern", r.Name, r.Group)
		}
		e.priority[r.Name] = len(order) + i
		e.detectors = append(e.detectors, ruleDetector{name: r.Name, re: re, group: r.Group})
	}
	e.priority[Name] = len(order) + len(opts.Rules)

	if len(opts.Names) > 0 {
		nd, err := newNameDetector(opts.Names)
		if err != nil {
			return nil, err
		}
		if nd != nil {
			e.detectors = append(e.detectors, nd)
		}
	}
	return e, nil
}

// Types returns the types this engine can produce, in priority order.
func (e *Engine) Types() []string {
	out := make([]string, 0, len(e.detectors))
	for _, d := range e.detectors {
		out = append(out, d.kind())
	}
	return out
}

// Find returns non-overlapping matches sorted by position.
//
// Overlaps are resolved by: longer match first, then type priority, then earlier start.
func (e *Engine) Find(text string) []Match {
	var all []Match
	for _, d := range e.detectors {
		all = append(all, d.find(text)...)
	}
	if len(all) == 0 {
		return nil
	}
	sort.SliceStable(all, func(i, j int) bool {
		a, b := all[i], all[j]
		if la, lb := a.End-a.Start, b.End-b.Start; la != lb {
			return la > lb
		}
		if pa, pb := e.priority[a.Type], e.priority[b.Type]; pa != pb {
			return pa < pb
		}
		return a.Start < b.Start
	})
	var kept []Match
	for _, m := range all {
		overlaps := false
		for _, k := range kept {
			if m.Start < k.End && k.Start < m.End {
				overlaps = true
				break
			}
		}
		if !overlaps {
			kept = append(kept, m)
		}
	}
	slices.SortFunc(kept, func(a, b Match) int { return a.Start - b.Start })
	return kept
}

// ---- helpers ----

func isDigit(b byte) bool { return b >= '0' && b <= '9' }

func isASCIIAlnum(b byte) bool {
	return isDigit(b) || (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z')
}

func digitsOnly(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if isDigit(s[i]) {
			b.WriteByte(s[i])
		}
	}
	return b.String()
}

// ---- custom rules ----

type ruleDetector struct {
	name  string
	re    *regexp.Regexp
	group int
}

func (r ruleDetector) kind() string { return r.name }

func (r ruleDetector) find(text string) []Match {
	var out []Match
	for _, loc := range r.re.FindAllStringSubmatchIndex(text, -1) {
		s, e := loc[2*r.group], loc[2*r.group+1]
		if s < 0 || e <= s {
			continue
		}
		out = append(out, Match{Start: s, End: e, Type: r.name, Value: text[s:e], Key: text[s:e]})
	}
	return out
}
