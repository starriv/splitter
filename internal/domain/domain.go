// Package domain implements the domain-name rule language and its matcher.
//
// Rules use the v2fly/domain-list-community syntax, so a list written for a
// compatible tool can be consumed without translation:
//
//	full:example.com     exact name only
//	domain:example.com   the name and every subdomain (also the default without a prefix)
//	keyword:example      substring of the name
//	regexp:^ex.*\.com$   RE2 regular expression, matched against the lowercase name
//
// Matching cost, from cheapest to most expensive: full and domain rules are
// answered by hash lookups (a domain lookup performs at most one lookup per
// label and allocates nothing); keyword and regexp rules require a scan of
// every rule of their kind, so large lists should use full and domain rules.
// Patterns are compiled with Go's RE2 engine, which runs in linear time and
// cannot be driven into catastrophic backtracking by a hostile pattern or
// query name.
package domain

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
)

// Kind identifies the matching mode of a rule. The zero value is not a valid
// Kind; Matcher.Match returns it when no rule matches.
type Kind uint8

const (
	// KindFull matches an exact name only.
	KindFull Kind = iota + 1
	// KindDomain matches a name and all of its subdomains.
	KindDomain
	// KindKeyword matches a name containing the value as a substring.
	KindKeyword
	// KindRegexp matches a name against a compiled RE2 pattern.
	KindRegexp
)

// Kinds lists every Kind in evaluation order. Callers must not modify the
// slice.
var Kinds = []Kind{KindFull, KindDomain, KindKeyword, KindRegexp}

// String returns the rule name for k, or "none" for an unrecognized kind.
func (k Kind) String() string {
	switch k {
	case KindFull:
		return "full"
	case KindDomain:
		return "domain"
	case KindKeyword:
		return "keyword"
	case KindRegexp:
		return "regexp"
	}
	return "none"
}

// Rule is one parsed rule: a matching kind and its normalized value.
type Rule struct {
	Kind  Kind
	Value string
}

// String renders the rule as kind:value, the form accepted by ParseRule.
func (r Rule) String() string { return r.Kind.String() + ":" + r.Value }

// Normalize lowercases ASCII letters and removes one trailing dot. It does not
// allocate when the name is already normalized.
//
// Only ASCII letters are folded. Non-ASCII bytes are preserved: DNS names on
// the wire use punycode (ASCII) for internationalized domain names, so folding
// Unicode case here would make the result depend on whether the name happened
// to contain an ASCII uppercase letter. That dependence would make matching
// inconsistent.
func Normalize(name string) string {
	name = strings.TrimSuffix(name, ".")
	for i := 0; i < len(name); i++ {
		if c := name[i]; 'A' <= c && c <= 'Z' {
			return asciiLower(name)
		}
	}
	return name
}

func asciiLower(s string) string {
	b := []byte(s)
	for i, c := range b {
		if 'A' <= c && c <= 'Z' {
			b[i] = c + 'a' - 'A'
		}
	}
	return string(b)
}

// ParseRule parses one rule. A value without a recognized prefix is treated as
// a domain rule. It returns an error for an empty rule, an unknown prefix, or an
// invalid name or pattern. Full and domain values are normalized; keyword values
// are lowercased.
func ParseRule(s string) (Rule, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return Rule{}, errors.New("empty rule")
	}
	kind := KindDomain
	if i := strings.IndexByte(s, ':'); i >= 0 {
		switch s[:i] {
		case "full":
			kind = KindFull
		case "domain":
			kind = KindDomain
		case "keyword":
			kind = KindKeyword
		case "regexp":
			kind = KindRegexp
		case "include":
			return Rule{}, errors.New(`"include:" is not supported; list the included file as another source of the set, or select the list from geosite.dat`)
		default:
			return Rule{}, fmt.Errorf("unknown rule type %q (want full, domain, keyword or regexp)", s[:i])
		}
		s = s[i+1:]
	}
	switch kind {
	case KindFull, KindDomain:
		s = Normalize(s)
		if err := validName(s); err != nil {
			return Rule{}, fmt.Errorf("%s:%s: %w", kind, s, err)
		}
	case KindKeyword:
		s = strings.ToLower(s)
		if s == "" {
			return Rule{}, errors.New("empty keyword")
		}
	case KindRegexp:
		if s == "" {
			// regexp.Compile("") succeeds and matches every name, so a single
			// "regexp:" line would silently turn the whole set into a catch-all.
			return Rule{}, errors.New(`empty regexp rule ("regexp:") would match every name`)
		}
		if _, err := compileRegexp(s); err != nil {
			return Rule{}, fmt.Errorf("regexp:%s: %w", s, err)
		}
	}
	return Rule{Kind: kind, Value: s}, nil
}

// compileRegexp compiles a rule pattern for matching against a normalized
// (lowercase) name. The case-insensitive flag allows a pattern written with
// ASCII uppercase to match; because every name handed to a matcher is already
// lowercased, the flag cannot broaden what a lowercase pattern matches.
func compileRegexp(pattern string) (*regexp.Regexp, error) {
	return regexp.Compile("(?i)" + pattern)
}

// validName enforces the DNS name limits and the character set accepted for
// full and domain rules: a name is at most 253 bytes, a label at most 63 bytes,
// and labels consist of lowercase ASCII letters, digits, hyphens and
// underscores. Underscores are accepted because service labels such as _dmarc
// occur in published lists.
func validName(s string) error {
	if s == "" {
		return errors.New("empty name")
	}
	if len(s) > 253 {
		return errors.New("name longer than 253 bytes")
	}
	label := 0
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '.':
			if label == 0 {
				return errors.New("empty label")
			}
			label = 0
			continue
		case 'a' <= c && c <= 'z', '0' <= c && c <= '9', c == '-', c == '_':
		default:
			return fmt.Errorf("invalid character %q", c)
		}
		label++
		if label > 63 {
			return errors.New("label longer than 63 bytes")
		}
	}
	if label == 0 {
		return errors.New("empty label")
	}
	return nil
}

// Matcher evaluates a normalized name against a compiled rule set. It is
// immutable after Build and safe for concurrent use.
type Matcher struct {
	full     map[string]struct{}
	suffix   map[string]struct{}
	keywords []string
	regexps  []*regexp.Regexp
}

// Match reports whether name matches any rule and, if so, which kind matched.
// name must already be normalized (see Normalize). Rules are evaluated in the
// order full, domain, keyword, regexp, and Match stops at the first match.
func (m *Matcher) Match(name string) (Kind, bool) {
	if _, ok := m.full[name]; ok {
		return KindFull, true
	}
	if len(m.suffix) > 0 {
		for s := name; ; {
			if _, ok := m.suffix[s]; ok {
				return KindDomain, true
			}
			i := strings.IndexByte(s, '.')
			if i < 0 {
				break
			}
			s = s[i+1:]
		}
	}
	for _, k := range m.keywords {
		if strings.Contains(name, k) {
			return KindKeyword, true
		}
	}
	for _, re := range m.regexps {
		if re.MatchString(name) {
			return KindRegexp, true
		}
	}
	return 0, false
}

// Counts returns the number of distinct rules per kind. The returned map is
// newly allocated and may be modified by the caller.
func (m *Matcher) Counts() map[Kind]int {
	return map[Kind]int{
		KindFull:    len(m.full),
		KindDomain:  len(m.suffix),
		KindKeyword: len(m.keywords),
		KindRegexp:  len(m.regexps),
	}
}

// Len returns the total number of distinct rules.
func (m *Matcher) Len() int {
	return len(m.full) + len(m.suffix) + len(m.keywords) + len(m.regexps)
}

// Builder accumulates rules into a Matcher. Full and domain rules are
// deduplicated by value; keyword and regexp rules are deduplicated by their
// parsed form. A Builder is not safe for concurrent use.
type Builder struct {
	m    Matcher
	seen map[Rule]struct{}
}

// NewBuilder returns an empty Builder.
func NewBuilder() *Builder {
	return &Builder{
		m:    Matcher{full: map[string]struct{}{}, suffix: map[string]struct{}{}},
		seen: map[Rule]struct{}{},
	}
}

// Add adds an already parsed rule. It returns an error for an invalid rule kind
// or a regexp pattern that does not compile.
func (b *Builder) Add(r Rule) error {
	switch r.Kind {
	case KindFull:
		b.m.full[r.Value] = struct{}{}
	case KindDomain:
		b.m.suffix[r.Value] = struct{}{}
	case KindKeyword, KindRegexp:
		if _, dup := b.seen[r]; dup {
			return nil
		}
		b.seen[r] = struct{}{}
		if r.Kind == KindKeyword {
			b.m.keywords = append(b.m.keywords, r.Value)
			return nil
		}
		re, err := compileRegexp(r.Value)
		if err != nil {
			return err
		}
		b.m.regexps = append(b.m.regexps, re)
	default:
		return fmt.Errorf("invalid rule kind %d", r.Kind)
	}
	return nil
}

// AddString parses s with ParseRule and adds the resulting rule.
func (b *Builder) AddString(s string) error {
	r, err := ParseRule(s)
	if err != nil {
		return err
	}
	return b.Add(r)
}

// Build returns the Matcher for the rules accumulated so far. The Builder must
// not be used afterwards; the returned Matcher shares its internal state.
func (b *Builder) Build() *Matcher {
	m := b.m
	b.seen = nil
	return &m
}

// LoadList reads rules, one per line. Blank lines and lines whose first
// non-space character is '#' are skipped, and a UTF-8 byte-order mark on the
// first line is ignored. It returns the number of rule lines read and aborts at
// the first invalid rule; that error includes the line number. A single line is
// limited to 1 MiB.
//
// Lines in the domain-list-community source format are accepted as well: the
// rule may be followed by attribute tags such as "@ads" or "@!cn", which are
// ignored (every entry of the file belongs to the set, as with an unfiltered
// geosite selector), and by a comment starting with '#'. "include:" lines are
// rejected with an explanation; use several files or a geosite selector.
func LoadList(r io.Reader, b *Builder) (int, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	n, line := 0, 0
	for sc.Scan() {
		line++
		s := sc.Text()
		if line == 1 {
			s = strings.TrimPrefix(s, "\ufeff") // strip the UTF-8 BOM some editors prepend
		}
		s = strings.TrimSpace(s)
		if s == "" || s[0] == '#' {
			continue
		}
		s, err := listRule(s)
		if err != nil {
			return n, fmt.Errorf("line %d: %w", line, err)
		}
		if err := b.AddString(s); err != nil {
			return n, fmt.Errorf("line %d: %w", line, err)
		}
		n++
	}
	return n, sc.Err()
}

// listRule returns the rule of one non-empty list line, dropping trailing
// attribute tags and a trailing comment.
func listRule(line string) (string, error) {
	fields := strings.Fields(line)
	for _, f := range fields[1:] {
		switch {
		case f[0] == '#':
			return fields[0], nil
		case f[0] == '@' && len(f) > 1:
		default:
			return "", fmt.Errorf("unexpected %q after the rule %q (only @attribute tags and a # comment may follow)", f, fields[0])
		}
	}
	return fields[0], nil
}

// LoadFile opens path and loads its rules with LoadList. Errors returned by
// LoadList are prefixed with path.
func LoadFile(path string, b *Builder) (int, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	n, err := LoadList(f, b)
	if err != nil {
		return n, fmt.Errorf("%s: %w", path, err)
	}
	return n, nil
}
