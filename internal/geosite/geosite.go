// Package geosite decodes geosite.dat files in the v2ray format published by
// v2fly/domain-list-community and Loyalsoldier/v2ray-rules-dat.
//
// The protobuf wire format is decoded directly with protowire rather than
// through generated code for v2ray-core: the schema consists of four small
// messages, and importing a complete proxy implementation for them would add
// considerably more code and dependencies than generated types would save.
//
//	message GeoSiteList { repeated GeoSite entry = 1; }
//	message GeoSite     { string country_code = 1; repeated Domain domain = 2; }
//	message Domain      { Type type = 1; string value = 2; repeated Attribute attribute = 3; }
//	message Attribute   { string key = 1; oneof { bool bool_value = 2; int64 int_value = 3; } }
//	enum Type           { Plain = 0; Regex = 1; RootDomain = 2; Full = 3; }
//
// Selectors use the domain-list-community attribute syntax:
//
//	cn            every entry of the cn list
//	cn@ads        only entries carrying the "ads" attribute
//	cn@-!cn       entries NOT carrying the "!cn" attribute
//
// Several attribute filters may be chained (cn@a@-b); all must hold.
package geosite

import (
	"errors"
	"fmt"
	"strings"

	"google.golang.org/protobuf/encoding/protowire"

	"github.com/starriv/splitter/internal/domain"
)

// Selector selects entries from one geosite list. An entry is selected when its
// list tag equals Tag and its attributes satisfy every Require entry and no
// Exclude entry. A Selector created by ParseSelector retains its original text,
// which Load uses as the key in its result map.
type Selector struct {
	Tag     string
	Require []string
	Exclude []string
	raw     string
}

// String returns the selector text as it was parsed.
func (s Selector) String() string { return s.raw }

// ParseSelector parses "tag", "tag@attr" or "tag@-attr"; attribute filters may
// be chained with @. The tag and the attribute names are lowercased. An empty
// tag or an empty attribute segment is an error.
func ParseSelector(s string) (Selector, error) {
	parts := strings.Split(strings.TrimSpace(s), "@")
	sel := Selector{Tag: strings.ToLower(parts[0]), raw: s}
	if sel.Tag == "" {
		return Selector{}, fmt.Errorf("selector %q: empty tag", s)
	}
	for _, a := range parts[1:] {
		a = strings.ToLower(a)
		switch {
		case strings.HasPrefix(a, "-") && len(a) > 1:
			sel.Exclude = append(sel.Exclude, a[1:])
		case a != "" && a != "-":
			sel.Require = append(sel.Require, a)
		default:
			return Selector{}, fmt.Errorf("selector %q: empty attribute", s)
		}
	}
	return sel, nil
}

func (s Selector) accepts(attrs []string) bool {
	has := func(k string) bool {
		for _, a := range attrs {
			if a == k {
				return true
			}
		}
		return false
	}
	for _, k := range s.Require {
		if !has(k) {
			return false
		}
	}
	for _, k := range s.Exclude {
		if has(k) {
			return false
		}
	}
	return true
}

// Load decodes data and calls add for every entry selected by at least one
// selector. It returns the number of rules each selector produced, keyed by the
// selector's original text.
//
// Load fails if the data is malformed, if a selector names a list that does not
// exist, or if a selector matches no entry. A selector that matches no entry is
// rejected because it usually indicates a misspelled attribute name, and
// accepting it would remove rules from the resulting set without an error.
func Load(data []byte, selectors []Selector, add func(domain.Rule) error) (map[string]int, error) {
	byTag := map[string][]int{}
	for i, s := range selectors {
		byTag[s.Tag] = append(byTag[s.Tag], i)
	}
	counts := make(map[string]int, len(selectors))
	found := map[string]bool{}

	err := eachField(data, func(num protowire.Number, v []byte) error {
		if num != 1 {
			return nil
		}
		code, domains, err := parseGeoSite(v)
		if err != nil {
			return err
		}
		idx := byTag[code]
		if len(idx) == 0 {
			return nil
		}
		found[code] = true
		for _, d := range domains {
			r, attrs, err := parseDomain(d)
			if err != nil {
				return fmt.Errorf("list %s: %w", code, err)
			}
			for _, i := range idx {
				if !selectors[i].accepts(attrs) {
					continue
				}
				if err := add(r); err != nil {
					return fmt.Errorf("list %s: %s: %w", code, r, err)
				}
				counts[selectors[i].raw]++
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	for _, s := range selectors {
		if !found[s.Tag] {
			return nil, fmt.Errorf("geosite list %q not found", s.Tag)
		}
	}
	// A tag that exists but whose attribute filter matches nothing usually
	// indicates a misspelled attribute name. Accepting it would silently drop
	// those rules while the set still loads from its other sources.
	for _, s := range selectors {
		if counts[s.raw] == 0 {
			return nil, fmt.Errorf("geosite selector %q matched no entries in list %q (check the attribute name)", s.raw, s.Tag)
		}
	}
	return counts, nil
}

// eachField calls fn for every length-delimited field; other wire types are
// skipped.
func eachField(b []byte, fn func(protowire.Number, []byte) error) error {
	for len(b) > 0 {
		num, typ, n := protowire.ConsumeTag(b)
		if n < 0 {
			return protowire.ParseError(n)
		}
		b = b[n:]
		if typ == protowire.BytesType {
			v, n := protowire.ConsumeBytes(b)
			if n < 0 {
				return protowire.ParseError(n)
			}
			if err := fn(num, v); err != nil {
				return err
			}
			b = b[n:]
			continue
		}
		n = protowire.ConsumeFieldValue(num, typ, b)
		if n < 0 {
			return protowire.ParseError(n)
		}
		b = b[n:]
	}
	return nil
}

// parseGeoSite decodes one GeoSite message. The list code is lowercased so that
// selectors match it without regard to case.
func parseGeoSite(b []byte) (code string, domains [][]byte, err error) {
	err = eachField(b, func(num protowire.Number, v []byte) error {
		switch num {
		case 1:
			code = strings.ToLower(string(v))
		case 2:
			domains = append(domains, v)
		}
		return nil
	})
	return code, domains, err
}

// parseDomain decodes one Domain message into a rule and its lowercased
// attribute keys.
func parseDomain(b []byte) (domain.Rule, []string, error) {
	var (
		typ   uint64
		value string
		attrs []string
	)
	for len(b) > 0 {
		num, wt, n := protowire.ConsumeTag(b)
		if n < 0 {
			return domain.Rule{}, nil, protowire.ParseError(n)
		}
		b = b[n:]
		switch {
		case num == 1:
			// A type field with a non-varint wire type must be rejected.
			// Skipping it would leave typ at its zero value and silently decode
			// the entry as a keyword rule.
			if wt != protowire.VarintType {
				return domain.Rule{}, nil, fmt.Errorf("domain type field has wire type %d, want varint", wt)
			}
			v, n := protowire.ConsumeVarint(b)
			if n < 0 {
				return domain.Rule{}, nil, protowire.ParseError(n)
			}
			typ, b = v, b[n:]
		case num == 2 || num == 3:
			if wt != protowire.BytesType {
				return domain.Rule{}, nil, fmt.Errorf("domain field %d has wire type %d, want bytes", num, wt)
			}
			v, n := protowire.ConsumeBytes(b)
			if n < 0 {
				return domain.Rule{}, nil, protowire.ParseError(n)
			}
			b = b[n:]
			if num == 2 {
				value = string(v)
				break
			}
			key, err := attributeKey(v)
			if err != nil {
				return domain.Rule{}, nil, err
			}
			attrs = append(attrs, strings.ToLower(key))
		default:
			n := protowire.ConsumeFieldValue(num, wt, b)
			if n < 0 {
				return domain.Rule{}, nil, protowire.ParseError(n)
			}
			b = b[n:]
		}
	}
	prefix, ok := map[uint64]string{0: "keyword:", 1: "regexp:", 2: "domain:", 3: "full:"}[typ]
	if !ok {
		return domain.Rule{}, nil, fmt.Errorf("unknown domain type %d for %q", typ, value)
	}
	r, err := domain.ParseRule(prefix + value)
	return r, attrs, err
}

// attributeKey returns the key of an Attribute message and rejects an attribute
// that carries none, because such an attribute can never satisfy a selector.
func attributeKey(b []byte) (string, error) {
	var key string
	err := eachField(b, func(num protowire.Number, v []byte) error {
		if num == 1 {
			key = string(v)
		}
		return nil
	})
	if err == nil && key == "" {
		err = errors.New("attribute without key")
	}
	return key, err
}
