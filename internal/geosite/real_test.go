package geosite

// Opt-in comparison against a real dlc.dat and a text list exported from the
// same upstream. SPLITTER_GEOSITE_DAT selects the geosite.dat file and
// SPLITTER_GEOSITE_LIST selects the text list; both must be set to run the
// test. SPLITTER_GEOSITE_LIST may point at the same file the routing-equivalence
// test consumes through SPLITTER_DOMAIN_LIST:
//
//	SPLITTER_GEOSITE_DAT=/path/dlc.dat \
//	  SPLITTER_GEOSITE_LIST=$SPLITTER_DOMAIN_LIST \
//	  go test -run TestRealGeosite -v ./internal/geosite

import (
	"os"
	"sort"
	"testing"

	"github.com/starriv/splitter/internal/domain"
)

// collect loads data through one selector and returns the selected domain and
// full rules as a set, together with a count of selected rules per kind.
func collect(t *testing.T, data []byte, selector string) (map[string]bool, map[domain.Kind]int) {
	t.Helper()
	set := map[string]bool{}
	kinds := map[domain.Kind]int{}
	_, err := Load(data, sel(t, selector), func(r domain.Rule) error {
		kinds[r.Kind]++
		if r.Kind == domain.KindDomain || r.Kind == domain.KindFull {
			set[r.String()] = true
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return set, kinds
}

// TestRealGeosite compares the geosite selector semantics against a text list
// exported from the same upstream. It checks that cn@-!cn and cn@!cn select
// disjoint sets, that together they account for the whole cn list, that known
// !cn entries are excluded, and that both sources select the same names by
// meaning.
func TestRealGeosite(t *testing.T) {
	datPath, listPath := os.Getenv("SPLITTER_GEOSITE_DAT"), os.Getenv("SPLITTER_GEOSITE_LIST")
	if datPath == "" || listPath == "" {
		t.Skip("set SPLITTER_GEOSITE_DAT and SPLITTER_GEOSITE_LIST")
	}
	data, err := os.ReadFile(datPath)
	if err != nil {
		t.Fatal(err)
	}
	all, allKinds := collect(t, data, "cn")
	kept, keptKinds := collect(t, data, "cn@-!cn")
	dropped, droppedKinds := collect(t, data, "cn@!cn")
	t.Logf("cn: %d domain/full %v", len(all), allKinds)
	t.Logf("cn@-!cn: %d %v   cn@!cn: %d %v", len(kept), keptKinds, len(dropped), droppedKinds)
	for r := range dropped {
		if kept[r] {
			t.Errorf("%s is both !cn and not !cn", r)
		}
	}
	if len(kept)+len(dropped) < len(all) {
		t.Errorf("selectors lost entries: %d + %d < %d", len(kept), len(dropped), len(all))
	}
	for _, must := range []string{"domain:tiktok.com", "domain:capcut.com"} {
		if kept[must] {
			t.Errorf("%s must be excluded by @-!cn", must)
		}
	}

	f, err := os.Open(listPath)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	b := domain.NewBuilder()
	list := map[string]bool{}
	if _, err := domain.LoadList(f, b); err != nil {
		t.Fatal(err)
	}
	data2, _ := os.ReadFile(listPath)
	for _, line := range splitLines(string(data2)) {
		if r, err := domain.ParseRule(line); err == nil && line != "" && line[0] != '#' {
			list[r.String()] = true
		}
	}
	// Compare by meaning rather than by text: the dat build drops rules that a
	// parent rule makes redundant (with domain:cn present, domain:12306.cn is
	// redundant), while a text export retains them.
	datB := domain.NewBuilder()
	for r := range kept {
		_ = datB.AddString(r)
	}
	datM, listM := datB.Build(), b.Build()
	uncovered := func(rules map[string]bool, m *domain.Matcher) []string {
		var out []string
		for r := range rules {
			rule, _ := domain.ParseRule(r)
			probes := []string{rule.Value}
			if rule.Kind == domain.KindDomain {
				probes = append(probes, "x7."+rule.Value)
			}
			for _, n := range probes {
				if _, ok := m.Match(n); !ok {
					out = append(out, n)
				}
			}
		}
		sort.Strings(out)
		return out
	}
	notInList := uncovered(kept, listM)
	notInDat := uncovered(list, datM)
	_, cnTLD := kept["domain:cn"]
	t.Logf("text: list %d rules, dat %d rules (dat has domain:cn: %v)", len(list), len(kept), cnTLD)
	t.Logf("meaning: dat names the list misses: %d %v", len(notInList), head(notInList))
	t.Logf("meaning: list names the dat misses: %d %v", len(notInDat), head(notInDat))
	// The inputs are snapshots taken at different times. A small number of
	// differences reflects upstream churn; a larger number indicates differing
	// selector semantics.
	if d := len(notInList) + len(notInDat); d > 20 {
		t.Errorf("%d names route differently: the selector semantics differ from the generated text list", d)
	}
}

func splitLines(s string) []string {
	var out []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			out = append(out, trim(s[start:i]))
			start = i + 1
		}
	}
	return append(out, trim(s[start:]))
}

func trim(s string) string {
	for len(s) > 0 && (s[len(s)-1] == '\r' || s[len(s)-1] == ' ') {
		s = s[:len(s)-1]
	}
	return s
}

func head(s []string) []string {
	if len(s) > 15 {
		return s[:15]
	}
	return s
}
