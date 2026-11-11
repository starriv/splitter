package domain

import (
	"fmt"
	"strings"
	"testing"
)

// TestParseRule covers prefix handling, normalization, and rejection of
// malformed rules.
func TestParseRule(t *testing.T) {
	cases := []struct {
		in   string
		want Rule
		err  bool
	}{
		{"baidu.com", Rule{KindDomain, "baidu.com"}, false},
		{"domain:Baidu.COM.", Rule{KindDomain, "baidu.com"}, false},
		{"full:www.qq.com", Rule{KindFull, "www.qq.com"}, false},
		{"keyword:Taobao", Rule{KindKeyword, "taobao"}, false},
		{`regexp:^img\d+\.example\.cn$`, Rule{KindRegexp, `^img\d+\.example\.cn$`}, false},
		{"_dmarc.example.com", Rule{KindDomain, "_dmarc.example.com"}, false},
		{"suffix:baidu.com", Rule{}, true},
		{"domain:", Rule{}, true},
		{"domain:a..b", Rule{}, true},
		{"domain:.baidu.com", Rule{}, true},
		{"domain:bad host", Rule{}, true},
		{"regexp:(", Rule{}, true},
		{"regexp:", Rule{}, true}, // empty pattern would match every name
		{"domain:" + strings.Repeat("a", 64) + ".com", Rule{}, true},
	}
	for _, c := range cases {
		got, err := ParseRule(c.in)
		if (err != nil) != c.err {
			t.Errorf("ParseRule(%q) err = %v, want err %v", c.in, err, c.err)
			continue
		}
		if !c.err && got != c.want {
			t.Errorf("ParseRule(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

// build constructs a Matcher from the given rules, failing the test on invalid
// input.
func build(t *testing.T, rules ...string) *Matcher {
	t.Helper()
	b := NewBuilder()
	for _, r := range rules {
		if err := b.AddString(r); err != nil {
			t.Fatal(err)
		}
	}
	return b.Build()
}

// TestMatch covers the matching semantics of every kind: label-aligned suffix
// matching, exact full matching, keyword substring matching, and regexp
// matching against a normalized name.
func TestMatch(t *testing.T) {
	m := build(t, "domain:baidu.com", "full:www.qq.com", "keyword:taobao", `regexp:^img[0-9]+\.example\.org$`, "domain:cn")
	cases := []struct {
		name string
		kind Kind
		ok   bool
	}{
		{"baidu.com", KindDomain, true},
		{"a.b.baidu.com", KindDomain, true},
		{"xbaidu.com", 0, false}, // suffix matching is label-aligned
		{"baidu.com.evil.org", 0, false},
		{"www.qq.com", KindFull, true},
		{"m.www.qq.com", 0, false}, // full rules match the exact name only
		{"qq.com", 0, false},
		{"world.taobao.com", KindKeyword, true},
		{"img12.example.org", KindRegexp, true},
		{"img.example.org", 0, false},
		{"gov.cn", KindDomain, true}, // a rule for a bare TLD
		{"", 0, false},
	}
	for _, c := range cases {
		k, ok := m.Match(c.name)
		if ok != c.ok || k != c.kind {
			t.Errorf("Match(%q) = %v,%v want %v,%v", c.name, k, ok, c.kind, c.ok)
		}
	}
	if got := m.Counts(); got[KindFull] != 1 || got[KindDomain] != 2 || got[KindKeyword] != 1 || got[KindRegexp] != 1 {
		t.Errorf("Counts() = %v", got)
	}
}

// TestBuilderDedup verifies that duplicate rules are counted once, including
// duplicates that differ only in case before normalization.
func TestBuilderDedup(t *testing.T) {
	m := build(t, "baidu.com", "domain:BAIDU.com", "keyword:x", "keyword:X", "regexp:a", "regexp:a")
	if m.Len() != 3 {
		t.Fatalf("Len() = %d, want 3", m.Len())
	}
}

// TestNormalize covers trailing-dot removal and ASCII-only case folding.
func TestNormalize(t *testing.T) {
	for in, want := range map[string]string{
		"WWW.Baidu.Com.": "www.baidu.com",
		"a.b":            "a.b",
		".":              "",
		"":               "",
		// Only ASCII letters are folded, and the result must not depend on
		// whether the name also contains an ASCII uppercase letter.
		"ÜBER.COM.": "Über.com",
		"über.COM.": "über.com",
	} {
		if got := Normalize(in); got != want {
			t.Errorf("Normalize(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestRegexpCaseInsensitive verifies that a pattern containing ASCII uppercase
// literals matches a lowercase name; silent non-matching would be a regression.
func TestRegexpCaseInsensitive(t *testing.T) {
	m := build(t, `regexp:^IMG[0-9]+\.Example\.CN$`)
	if _, ok := m.Match("img123.example.cn"); !ok {
		t.Fatal("uppercase regexp rule did not match a lowercase name")
	}
	if _, ok := m.Match("img123.example.com"); ok {
		t.Fatal("regexp rule matched a different domain")
	}
}

// TestLoadList covers comment and blank-line skipping, surrounding whitespace,
// a UTF-8 byte-order mark, CRLF line endings, and line-numbered errors.
func TestLoadList(t *testing.T) {
	b := NewBuilder()
	n, err := LoadList(strings.NewReader("# header\n\nbaidu.com\n  full:qq.com  \n"), b)
	if err != nil || n != 2 {
		t.Fatalf("LoadList = %d, %v", n, err)
	}
	if n, err := LoadList(strings.NewReader("\ufeffdomain:baidu.com\r\nfull:qq.com\r\n"), NewBuilder()); err != nil || n != 2 {
		t.Fatalf("BOM/CRLF list = %d, %v", n, err)
	}
	_, err = LoadList(strings.NewReader("baidu.com\nnope:x\n"), NewBuilder())
	if err == nil || !strings.Contains(err.Error(), "line 2") {
		t.Fatalf("want line number in error, got %v", err)
	}

	// domain-list-community source lines: attributes and trailing comments.
	b = NewBuilder()
	src := "full:a.example @ads\nb.example @!cn @ads # note\nregexp:^c\\.example$ #x\nkeyword:dexample\t@cn\n"
	if n, err := LoadList(strings.NewReader(src), b); err != nil || n != 4 {
		t.Fatalf("dlc list = %d, %v", n, err)
	}
	m := b.Build()
	for _, name := range []string{"a.example", "x.b.example", "c.example", "xdexample.org"} {
		if _, ok := m.Match(name); !ok {
			t.Errorf("%s did not match the dlc-format list", name)
		}
	}
	for _, bad := range []string{"include:google\n", "a.example extra\n", "a.example @\n"} {
		_, err := LoadList(strings.NewReader(bad), NewBuilder())
		if err == nil || !strings.Contains(err.Error(), "line 1") {
			t.Errorf("%q: err = %v, want a line-numbered error", bad, err)
		}
	}
}

// benchMatcher builds a matcher with many domain rules and a smaller set of
// full rules for the match benchmarks.
func benchMatcher(b *testing.B) *Matcher {
	bl := NewBuilder()
	for i := range 10000 {
		_ = bl.AddString(fmt.Sprintf("domain:site%d.example%d.cn", i, i%50))
	}
	for i := range 100 {
		_ = bl.AddString(fmt.Sprintf("full:exact%d.example.com", i))
	}
	return bl.Build()
}

// BenchmarkMatchHitDeep measures a domain match against a name with several
// leading labels.
func BenchmarkMatchHitDeep(b *testing.B) {
	m := benchMatcher(b)
	b.ReportAllocs()
	for b.Loop() {
		if _, ok := m.Match("a.b.c.site4321.example21.cn"); !ok {
			b.Fatal("miss")
		}
	}
}

// BenchmarkMatchMiss measures a name that matches no rule.
func BenchmarkMatchMiss(b *testing.B) {
	m := benchMatcher(b)
	b.ReportAllocs()
	for b.Loop() {
		if _, ok := m.Match("www.some-foreign-site.example.com"); ok {
			b.Fatal("hit")
		}
	}
}

// FuzzRule verifies that a parsed rule round-trips through its string form, is
// accepted by the builder, and matches its own value for full and domain rules.
func FuzzRule(f *testing.F) {
	for _, s := range []string{"baidu.com", "full:a.b", "keyword:x", `regexp:^a\.b$`, "domain:a..b", "regexp:(", "\ufeffx"} {
		f.Add(s, "www.baidu.com")
	}
	f.Fuzz(func(t *testing.T, rule, name string) {
		r, err := ParseRule(rule)
		if err != nil {
			return
		}
		// A parsed rule must round-trip and be accepted by the builder.
		r2, err := ParseRule(r.String())
		if err != nil || r2 != r {
			t.Fatalf("round trip %q -> %v -> %v, %v", rule, r, r2, err)
		}
		b := NewBuilder()
		if err := b.Add(r); err != nil {
			t.Fatal(err)
		}
		m := b.Build()
		// Full and domain rules always match their own value.
		if r.Kind == KindFull || r.Kind == KindDomain {
			if _, ok := m.Match(r.Value); !ok {
				t.Fatalf("%v does not match itself", r)
			}
		}
		_, _ = m.Match(Normalize(name))
	})
}
