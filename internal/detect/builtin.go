package detect

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// ---- email ----

var emailRe = regexp.MustCompile(`[A-Za-z0-9._%+-]+@[A-Za-z0-9-]+(?:\.[A-Za-z0-9-]+)*\.[A-Za-z]{2,}`)

type emailDetector struct{}

func (emailDetector) kind() string { return Email }

func (emailDetector) find(text string) []Match {
	var out []Match
	for _, loc := range emailRe.FindAllStringIndex(text, -1) {
		s, e := loc[0], loc[1]
		for s < e && text[s] == '.' { // a sentence dot before the address isn't part of it
			s++
		}
		if text[s] == '@' {
			continue
		}
		if e < len(text) && (isASCIIAlnum(text[e]) || text[e] == '_') {
			continue
		}
		v := text[s:e]
		out = append(out, Match{Start: s, End: e, Type: Email, Value: v, Key: strings.ToLower(v)})
	}
	return out
}

// ---- credit card ----

// A run of digit groups separated by a single space or dash.
var digitRunRe = regexp.MustCompile(`[0-9]+(?:[ -][0-9]+)*`)

type cardDetector struct{}

func (cardDetector) kind() string { return CreditCard }

type span struct{ start, end int }

func (cardDetector) find(text string) []Match {
	var out []Match
	for _, loc := range digitRunRe.FindAllStringIndex(text, -1) {
		if loc[0] > 0 && (isASCIIAlnum(text[loc[0]-1]) || text[loc[0]-1] == '_') {
			continue
		}
		if loc[1] < len(text) && (isASCIIAlnum(text[loc[1]]) || text[loc[1]] == '_') {
			continue
		}
		var groups []span
		run := text[loc[0]:loc[1]]
		start := 0
		for i := 0; i <= len(run); i++ {
			if i == len(run) || !isDigit(run[i]) {
				groups = append(groups, span{loc[0] + start, loc[0] + i})
				start = i + 1
			}
		}
		out = appendCards(out, text, groups)
	}
	return out
}

// appendCards picks the longest Luhn-valid stretch of whole groups, then looks left and right of it.
func appendCards(out []Match, text string, groups []span) []Match {
	if len(groups) == 0 {
		return out
	}
	bestA, bestB, bestLen := -1, -1, 0
	for a := range groups {
		n := 0
		for b := a; b < len(groups); b++ {
			n += groups[b].end - groups[b].start
			if n > 19 {
				break
			}
			if n >= 13 && n > bestLen {
				v := text[groups[a].start:groups[b].end]
				if d := digitsOnly(v); luhn(d) && strings.Trim(d, "0") != "" {
					bestA, bestB, bestLen = a, b, n
				}
			}
		}
	}
	if bestA < 0 {
		return out
	}
	out = appendCards(out, text, groups[:bestA])
	s, e := groups[bestA].start, groups[bestB].end
	out = append(out, Match{Start: s, End: e, Type: CreditCard, Value: text[s:e], Key: digitsOnly(text[s:e])})
	return appendCards(out, text, groups[bestB+1:])
}

func luhn(digits string) bool {
	sum := 0
	double := false
	for i := len(digits) - 1; i >= 0; i-- {
		d := int(digits[i] - '0')
		if double {
			d *= 2
			if d > 9 {
				d -= 9
			}
		}
		sum += d
		double = !double
	}
	return sum%10 == 0
}

// ---- IBAN ----

var ibanRe = regexp.MustCompile(`[A-Za-z]{2}[0-9]{2}(?: ?[A-Za-z0-9]){11,32}`)

// IBAN lengths per country (ISO 13616 registry). Other country codes are not treated as IBANs.
var ibanLengths = map[string]int{
	"AD": 24, "AE": 23, "AL": 28, "AT": 20, "AZ": 28, "BA": 20, "BE": 16, "BG": 22, "BH": 22, "BR": 29,
	"CH": 21, "CR": 22, "CY": 28, "CZ": 24, "DE": 22, "DK": 18, "DO": 28, "EE": 20, "ES": 24, "FI": 18,
	"FO": 18, "FR": 27, "GB": 22, "GE": 22, "GI": 23, "GL": 18, "GR": 27, "GT": 28, "HR": 21, "HU": 28,
	"IE": 22, "IL": 23, "IS": 26, "IT": 27, "JO": 30, "KW": 30, "KZ": 20, "LB": 28, "LI": 21, "LT": 20,
	"LU": 20, "LV": 21, "MC": 27, "MD": 24, "ME": 22, "MK": 19, "MR": 27, "MT": 31, "MU": 30, "NL": 18,
	"NO": 15, "PK": 24, "PL": 28, "PS": 29, "PT": 25, "QA": 29, "RO": 24, "RS": 22, "SA": 24, "SE": 24,
	"SI": 19, "SK": 24, "SM": 27, "TN": 24, "TR": 26, "UA": 29, "VG": 24, "XK": 20,
}

type ibanDetector struct{}

func (ibanDetector) kind() string { return IBAN }

func (ibanDetector) find(text string) []Match {
	var out []Match
	pos := 0
	for pos < len(text) {
		loc := ibanRe.FindStringIndex(text[pos:])
		if loc == nil {
			break
		}
		s, maxEnd := pos+loc[0], pos+loc[1]
		if s > 0 && (isASCIIAlnum(text[s-1]) || text[s-1] == '_') {
			pos = s + 1
			continue
		}
		found := false
		for e := maxEnd; e > s; e-- {
			if text[e-1] == ' ' || (e < len(text) && (isASCIIAlnum(text[e]) || text[e] == '_')) {
				continue
			}
			compact := strings.ToUpper(strings.ReplaceAll(text[s:e], " ", ""))
			if ibanGrouping(text[s:e]) && validIBAN(compact) {
				out = append(out, Match{Start: s, End: e, Type: IBAN, Value: text[s:e], Key: compact})
				pos, found = e, true
				break
			}
		}
		if !found {
			pos = s + 1
		}
	}
	return out
}

// ibanGrouping accepts an IBAN without spaces, or in groups of four (the last group may be shorter).
func ibanGrouping(v string) bool {
	if !strings.Contains(v, " ") {
		return true
	}
	parts := strings.Split(v, " ")
	for i, p := range parts {
		if (i < len(parts)-1 && len(p) != 4) || len(p) == 0 || len(p) > 4 {
			return false
		}
	}
	return true
}

func validIBAN(s string) bool {
	if n, ok := ibanLengths[s[:2]]; !ok || len(s) != n {
		return false
	}
	// Move the first four characters to the end, letters become 10..35, then mod 97 must be 1.
	r := s[4:] + s[:4]
	mod := 0
	for i := 0; i < len(r); i++ {
		c := r[i]
		switch {
		case isDigit(c):
			mod = (mod*10 + int(c-'0')) % 97
		case c >= 'A' && c <= 'Z':
			mod = (mod*100 + int(c-'A'+10)) % 97
		default:
			return false
		}
	}
	return mod == 1
}

// ---- IPv4 ----

var ipv4Re = regexp.MustCompile(`[0-9]{1,3}\.[0-9]{1,3}\.[0-9]{1,3}\.[0-9]{1,3}`)

type ipv4Detector struct{ skipPrivate bool }

func (ipv4Detector) kind() string { return IPv4 }

func (d ipv4Detector) find(text string) []Match {
	var out []Match
	for _, loc := range ipv4Re.FindAllStringIndex(text, -1) {
		s, e := loc[0], loc[1]
		if s > 0 && (isASCIIAlnum(text[s-1]) || text[s-1] == '.' || text[s-1] == '_') {
			continue
		}
		if e < len(text) && (isASCIIAlnum(text[e]) || text[e] == '_') {
			continue
		}
		if e+1 < len(text) && text[e] == '.' && isDigit(text[e+1]) { // 1.2.3.4.5 is a version, not an IP
			continue
		}
		parts := strings.Split(text[s:e], ".")
		var oct [4]int
		ok := true
		for i, p := range parts {
			if len(p) > 1 && p[0] == '0' {
				ok = false
				break
			}
			n, _ := strconv.Atoi(p)
			if n > 255 {
				ok = false
				break
			}
			oct[i] = n
		}
		if !ok || (d.skipPrivate && privateIPv4(oct)) {
			continue
		}
		out = append(out, Match{Start: s, End: e, Type: IPv4, Value: text[s:e], Key: text[s:e]})
	}
	return out
}

func privateIPv4(o [4]int) bool {
	return o[0] == 10 || o[0] == 127 ||
		(o[0] == 172 && o[1] >= 16 && o[1] <= 31) ||
		(o[0] == 192 && o[1] == 168)
}

// ---- phone ----

type phoneDetector struct{}

func (phoneDetector) kind() string { return Phone }

type phoneGroup struct {
	digits string
	end    int  // byte offset after the group (after ')' if in parentheses)
	paren  bool // written as (555)
	sep    byte // separator before this group, 0 for the first or after ')'
}

func (phoneDetector) find(text string) []Match {
	var out []Match
	for i := 0; i < len(text); {
		c := text[i]
		startable := isDigit(c) || ((c == '+' || c == '(') && i+1 < len(text) && isDigit(text[i+1]))
		if !startable || (i > 0 && phoneBlockedBefore(text[i-1])) {
			i++
			continue
		}
		plus, groups := scanPhone(text, i)
		matched := false
		if dottedQuad(plus, groups) || totalDigits(groups) > 15 {
			// An IP address, or a longer number (a card, an account): never cut into a phone.
			i = endOfNumber(text, groups[len(groups)-1].end)
			continue
		}
		for k := len(groups); k >= 1; k-- {
			end := groups[k-1].end
			if k == len(groups) && end < len(text) && (isASCIIAlnum(text[end]) || text[end] == '_') {
				break // glued to a word: not a phone
			}
			if validPhone(plus, groups[:k]) {
				v := text[i:end]
				key := digitsOnly(v)
				if plus {
					key = "+" + key
				}
				out = append(out, Match{Start: i, End: end, Type: Phone, Value: v, Key: key})
				i, matched = end, true
				break
			}
		}
		if !matched {
			// Skip the rest of this number so we don't match its tail on its own.
			j := i + 1
			for j < len(text) && isDigit(text[j]) {
				j++
			}
			i = j
		}
	}
	return out
}

// endOfNumber returns where a run of digits and phone separators starting at i ends.
func endOfNumber(text string, i int) int {
	for i < len(text) {
		c := text[i]
		switch {
		case isDigit(c) || c == ')':
			i++
		case (c == ' ' || c == '-' || c == '.' || c == '(') && i+1 < len(text) && (isDigit(text[i+1]) || text[i+1] == '('):
			i++
		default:
			return i
		}
	}
	return i
}

func phoneBlockedBefore(b byte) bool {
	return isASCIIAlnum(b) || b == '_' || b == '.' || b == '+' || b == '-' || b == '/' || b == ','
}

func scanPhone(text string, i int) (bool, []phoneGroup) {
	pos := i
	plus := false
	if text[pos] == '+' {
		plus = true
		pos++
	}
	var groups []phoneGroup
	var sep byte
	digits := 0
	for pos < len(text) && digits <= 15 { // a phone has at most 15 digits: stop reading after that
		paren := false
		if text[pos] == '(' {
			paren = true
			pos++
		}
		ds := pos
		for pos < len(text) && isDigit(text[pos]) {
			pos++
		}
		if pos == ds || (paren && (pos >= len(text) || text[pos] != ')')) {
			break // not a group: the number ends before it
		}
		g := phoneGroup{digits: text[ds:pos], paren: paren, sep: sep}
		digits += pos - ds
		if paren {
			pos++
		}
		g.end = pos
		groups = append(groups, g)
		// Separator: one of " -." followed by a digit or '(' ; after ')' a digit may follow directly.
		sep = 0
		if pos+1 < len(text) && (text[pos] == ' ' || text[pos] == '-' || text[pos] == '.') &&
			(isDigit(text[pos+1]) || text[pos+1] == '(') {
			sep = text[pos]
			pos++
			continue
		}
		if paren && pos < len(text) && isDigit(text[pos]) {
			continue
		}
		break
	}
	return plus, groups
}

func totalDigits(groups []phoneGroup) int {
	n := 0
	for _, g := range groups {
		n += len(g.digits)
	}
	return n
}

func validPhone(plus bool, groups []phoneGroup) bool {
	n := totalDigits(groups)
	if n < 9 || n > 15 {
		return false
	}
	if plus {
		return true
	}
	// Without a leading +, the number must be written in a phone-like layout.
	if len(groups) < 2 {
		return false
	}
	for i, g := range groups {
		l := len(g.digits)
		if i == 0 {
			// Long area codes start with 0 (0207, 01632); 2026-... is a year.
			if l > 5 || (l >= 4 && !g.paren && g.digits[0] != '0') {
				return false
			}
		} else if l < 2 || l > 4 {
			return false
		}
	}
	// Dates like 2026-11-09 or 09.11.2026 followed by more digits.
	if len(groups) >= 3 && (groups[1].sep == '-' || groups[1].sep == '.') {
		a, b, c := len(groups[0].digits), len(groups[1].digits), len(groups[2].digits)
		if (a == 4 && b == 2 && c == 2) || (a == 2 && b == 2 && c == 4) {
			return false
		}
	}
	return true
}

func dottedQuad(plus bool, groups []phoneGroup) bool {
	if plus || len(groups) != 4 {
		return false
	}
	for i, g := range groups {
		if g.paren || len(g.digits) > 3 || (i > 0 && g.sep != '.') {
			return false
		}
	}
	return true
}

// ---- names ----

// nameDetector matches listed names case-insensitively, as whole words. Names are indexed by their
// first word, so the cost doesn't grow with the length of the list.
type nameDetector struct {
	byFirst map[string][]nameEntry // lower-cased first token -> names, longest first
}

type nameEntry struct {
	key   string
	words []string
}

func newNameDetector(names []string) (*nameDetector, error) {
	d := &nameDetector{byFirst: map[string][]nameEntry{}}
	seen := map[string]bool{}
	for _, n := range names {
		words := strings.Fields(n)
		if len(words) == 0 {
			continue
		}
		if r, _ := utf8.DecodeRuneInString(words[0]); !isWordRune(r) {
			return nil, fmt.Errorf("name %q must start with a letter or digit", n)
		}
		key := strings.ToLower(strings.Join(words, " "))
		if seen[key] {
			continue
		}
		seen[key] = true
		first := strings.ToLower(leadingToken(words[0]))
		d.byFirst[first] = append(d.byFirst[first], nameEntry{key: key, words: words})
	}
	if len(d.byFirst) == 0 {
		return nil, nil
	}
	for _, list := range d.byFirst {
		sort.Slice(list, func(i, j int) bool {
			li, lj := utf8.RuneCountInString(list[i].key), utf8.RuneCountInString(list[j].key)
			if li != lj {
				return li > lj
			}
			return list[i].key < list[j].key
		})
	}
	return d, nil
}

func (*nameDetector) kind() string { return Name }

func (d *nameDetector) find(text string) []Match {
	var out []Match
	for i := 0; i < len(text); {
		r, size := utf8.DecodeRuneInString(text[i:])
		if !isWordRune(r) || !wordBoundaryBefore(text, i) {
			i += size
			continue
		}
		tok := leadingToken(text[i:])
		end := -1
		for _, n := range d.byFirst[strings.ToLower(tok)] {
			if e, ok := matchWords(text, i, n.words); ok && wordBoundaryAfter(text, e) {
				end = e
				break
			}
		}
		if end < 0 {
			i += len(tok)
			continue
		}
		v := text[i:end]
		out = append(out, Match{Start: i, End: end, Type: Name, Value: v, Key: strings.ToLower(strings.Join(strings.Fields(v), " "))})
		i = end
	}
	return out
}

// leadingToken returns the run of word runes at the start of s.
func leadingToken(s string) string {
	for i, r := range s {
		if !isWordRune(r) {
			return s[:i]
		}
	}
	return s
}

// matchWords matches words at text[i:], case-insensitively, with one or more spaces between words.
func matchWords(text string, i int, words []string) (int, bool) {
	for k, w := range words {
		if k > 0 {
			j := i
			for j < len(text) {
				r, size := utf8.DecodeRuneInString(text[j:])
				if !unicode.IsSpace(r) {
					break
				}
				j += size
			}
			if j == i {
				return 0, false
			}
			i = j
		}
		for _, wr := range w {
			if i >= len(text) {
				return 0, false
			}
			tr, size := utf8.DecodeRuneInString(text[i:])
			if tr != wr && !equalFoldRune(tr, wr) {
				return 0, false
			}
			i += size
		}
	}
	return i, true
}

func equalFoldRune(a, b rune) bool {
	for r := unicode.SimpleFold(a); r != a; r = unicode.SimpleFold(r) {
		if r == b {
			return true
		}
	}
	return false
}

func isWordRune(r rune) bool {
	return unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' || unicode.Is(unicode.Mn, r)
}

func wordBoundaryBefore(text string, i int) bool {
	if i == 0 {
		return true
	}
	r, _ := utf8.DecodeLastRuneInString(text[:i])
	return !isWordRune(r)
}

func wordBoundaryAfter(text string, i int) bool {
	if i >= len(text) {
		return true
	}
	r, _ := utf8.DecodeRuneInString(text[i:])
	return !isWordRune(r)
}
