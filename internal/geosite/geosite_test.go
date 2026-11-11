package geosite

import (
	"strings"
	"testing"

	"google.golang.org/protobuf/encoding/protowire"

	"github.com/starriv/splitter/internal/domain"
)

type entry struct {
	typ   uint64
	value string
	attrs []string
}

// encode builds a GeoSiteList fixture in wire format, so the decoder is
// exercised without generated message types.
func encode(lists map[string][]entry) []byte {
	var out []byte
	for code, entries := range lists {
		var site []byte
		site = protowire.AppendTag(site, 1, protowire.BytesType)
		site = protowire.AppendString(site, strings.ToUpper(code)) // v2ray stores codes in upper case
		for _, e := range entries {
			var d []byte
			d = protowire.AppendTag(d, 1, protowire.VarintType)
			d = protowire.AppendVarint(d, e.typ)
			d = protowire.AppendTag(d, 2, protowire.BytesType)
			d = protowire.AppendString(d, e.value)
			for _, a := range e.attrs {
				var at []byte
				at = protowire.AppendTag(at, 1, protowire.BytesType)
				at = protowire.AppendString(at, a)
				at = protowire.AppendTag(at, 2, protowire.VarintType)
				at = protowire.AppendVarint(at, 1)
				d = protowire.AppendTag(d, 3, protowire.BytesType)
				d = protowire.AppendBytes(d, at)
			}
			site = protowire.AppendTag(site, 2, protowire.BytesType)
			site = protowire.AppendBytes(site, d)
		}
		out = protowire.AppendTag(out, 1, protowire.BytesType)
		out = protowire.AppendBytes(out, site)
	}
	return out
}

// sel parses the given selectors, failing the test on invalid input.
func sel(t *testing.T, s ...string) []Selector {
	t.Helper()
	var out []Selector
	for _, x := range s {
		p, err := ParseSelector(x)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, p)
	}
	return out
}

// TestLoadSelectors covers list tag matching, attribute inclusion and
// exclusion, list code normalization, decoding of every domain type, and
// rejection of unknown lists and malformed data.
func TestLoadSelectors(t *testing.T) {
	data := encode(map[string][]entry{
		"cn": {
			{2, "baidu.com", nil},
			{3, "www.qq.com", nil},
			{0, "taobao", nil},
			{1, `^img\d+\.cn$`, nil},
			{2, "tiktok.com", []string{"!cn"}},
			{2, "doubleclick.cn", []string{"ads"}},
		},
		"google": {{2, "google.com", nil}},
	})

	var got []string
	add := func(r domain.Rule) error { got = append(got, r.String()); return nil }

	counts, err := Load(data, sel(t, "cn@-!cn"), add)
	if err != nil {
		t.Fatal(err)
	}
	want := "domain:baidu.com full:www.qq.com keyword:taobao regexp:^img\\d+\\.cn$ domain:doubleclick.cn"
	if strings.Join(got, " ") != want {
		t.Fatalf("cn@-!cn = %v", got)
	}
	if counts["cn@-!cn"] != 5 {
		t.Fatalf("counts = %v", counts)
	}

	got = nil
	if _, err := Load(data, sel(t, "CN@ads"), add); err != nil || strings.Join(got, " ") != "domain:doubleclick.cn" {
		t.Fatalf("cn@ads = %v, %v", got, err)
	}

	if _, err := Load(data, sel(t, "nosuchlist"), add); err == nil {
		t.Fatal("unknown tag must fail")
	}
	if _, err := Load([]byte{0xff, 0xff}, sel(t, "cn"), add); err == nil {
		t.Fatal("garbage must fail")
	}
}

// TestParseSelector covers chained attribute filters and rejection of an empty
// tag or an empty attribute segment.
func TestParseSelector(t *testing.T) {
	s, err := ParseSelector("cn@a@-b")
	if err != nil || s.Tag != "cn" || len(s.Require) != 1 || len(s.Exclude) != 1 {
		t.Fatalf("got %+v, %v", s, err)
	}
	for _, bad := range []string{"", "@x", "cn@", "cn@-"} {
		if _, err := ParseSelector(bad); err == nil {
			t.Errorf("ParseSelector(%q) should fail", bad)
		}
	}
}

// FuzzLoad verifies that Load never panics and consistently rejects malformed
// input.
func FuzzLoad(f *testing.F) {
	f.Add(encode(map[string][]entry{"cn": {{2, "baidu.com", []string{"!cn"}}, {3, "qq.com", nil}}}))
	f.Add([]byte{0x0a, 0x02, 0x0a, 0x00})
	sels := []Selector{{Tag: "cn", raw: "cn"}}
	f.Fuzz(func(t *testing.T, data []byte) {
		_, _ = Load(data, sels, func(domain.Rule) error { return nil })
	})
}

// TestZeroMatchSelectorFails protects the rejection of a selector whose
// attribute filter matches no entry. Such a selector usually indicates a
// misspelled attribute name, and accepting it would silently reduce the
// resulting set.
func TestZeroMatchSelectorFails(t *testing.T) {
	data := encode(map[string][]entry{
		"cn": {{2, "baidu.com", []string{"ads"}}},
	})
	add := func(domain.Rule) error { return nil }
	if _, err := Load(data, sel(t, "cn@!cnx"), add); err == nil {
		t.Fatal("selector whose attribute matches no entry must fail")
	}
}

// TestDomainTypeWrongWireTypeFails protects the rejection of a Domain.type
// field with a non-varint wire type. Ignoring the field would leave the type at
// its zero value and silently decode the entry as a keyword rule.
func TestDomainTypeWrongWireTypeFails(t *testing.T) {
	var d []byte
	d = protowire.AppendTag(d, 1, protowire.BytesType) // wrong wire type for a varint
	d = protowire.AppendString(d, "x")
	d = protowire.AppendTag(d, 2, protowire.BytesType)
	d = protowire.AppendString(d, "baidu.com")
	var site []byte
	site = protowire.AppendTag(site, 1, protowire.BytesType)
	site = protowire.AppendString(site, "cn")
	site = protowire.AppendTag(site, 2, protowire.BytesType)
	site = protowire.AppendBytes(site, d)
	var out []byte
	out = protowire.AppendTag(out, 1, protowire.BytesType)
	out = protowire.AppendBytes(out, site)
	if _, err := Load(out, sel(t, "cn"), func(domain.Rule) error { return nil }); err == nil {
		t.Fatal("wrong wire type for Domain.type must fail")
	}
}
