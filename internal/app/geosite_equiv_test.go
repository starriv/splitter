package app

// Opt-in comparison between a geosite-based cn set and a generated text list.
// The list is not part of this repository; set SPLITTER_DOMAIN_LIST to its
// path (absolute, or relative to this package directory):
//
//	SPLITTER_GEOSITE_DAT=/path/dlc.dat SPLITTER_DOMAIN_LIST=/path/cn-domains.txt \
//	  go test -run TestGeositeRoutingEquivalence -v ./internal/app
//
// The test reports whether switching sets.cn from files: to geosite: changes
// routing, without sending a single query.

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/net/dns/dnsmessage"

	"github.com/starriv/splitter/internal/config"
	"github.com/starriv/splitter/internal/domain"
)

// TestGeositeRoutingEquivalence compares routing decisions for a
// geosite-based cn set and a generated text list over a corpus of real-world
// names.
func TestGeositeRoutingEquivalence(t *testing.T) {
	dat := os.Getenv("SPLITTER_GEOSITE_DAT")
	if dat == "" {
		t.Skip("set SPLITTER_GEOSITE_DAT to a dlc.dat (v2fly domain-list-community)")
	}
	listPath, ok := domainList()
	if !ok {
		t.Skip("SPLITTER_DOMAIN_LIST is not set to an existing file")
	}
	list, err := os.ReadFile(listPath)
	if err != nil {
		t.Fatal(err)
	}

	// Two configurations that differ only in how the cn set is built.
	build := func(setBlock, dir string) *Runtime {
		t.Helper()
		text := fmt.Sprintf(`
listen: ["127.0.0.1:0"]
admin: "127.0.0.1:0"
upstreams:
  aliyun: {servers: ["223.5.5.5:53"]}
  cloudflare: {servers: ["1.1.1.1:53"]}
sets:
  local: {rules: ["domain:local.example"]}
  cn:
%s
rules:
  - {name: local-zone, sets: [local], answer: {a: ["192.0.2.10"], ttl: 60}}
  - {name: cn, sets: [cn], forward: aliyun}
  - {name: default, forward: cloudflare}
`, setBlock)
		cfg, err := config.Parse([]byte(text), dir)
		if err != nil {
			t.Fatal(err)
		}
		rt, err := Build(cfg, nil)
		if err != nil {
			t.Fatal(err)
		}
		return rt
	}

	filesDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(filesDir, "cn-domains.txt"), list, 0o644); err != nil {
		t.Fatal(err)
	}
	rtFiles := build(`    files: ["cn-domains.txt"]`, filesDir)

	geoDir := t.TempDir()
	data, err := os.ReadFile(dat)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(geoDir, "dlc.dat"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	rtGeo := build(`    geosite:
      - file: dlc.dat
        selectors: ["cn@-!cn"]`, geoDir)

	// Corpus: every rule of the domain list (itself, a subdomain, and a
	// name that tests label alignment), every real-world scenario name, and
	// the names that must never reach the CN upstream.
	var corpus []string
	add := func(n string) {
		if n != "" {
			corpus = append(corpus, n)
		}
	}
	f, err := os.Open(listPath)
	if err != nil {
		t.Fatal(err)
	}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		r, err := domain.ParseRule(sc.Text())
		if err != nil {
			continue
		}
		add(r.Value)
		add("www." + r.Value)
		if r.Kind == domain.KindDomain {
			add("x9.y8." + r.Value)
		}
		if len(r.Value) > 1 {
			add("zz" + r.Value)
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	f.Close()
	corpus = append(corpus, scenarioNames...)
	mustAbroad := []string{"google.com", "github.com", "youtube.com", "facebook.com",
		"tiktok.com", "tiktokv.com", "capcut.com", "byteoversea.com", "api.telegram.org"}
	corpus = append(corpus, mustAbroad...)

	route := func(rt *Runtime, name string) (rule, action, target string) {
		d := rt.Router.Route(domain.Normalize(name), dnsmessage.TypeA)
		return d.Rule.Name, d.Rule.Action.String(), d.Rule.Target()
	}

	type diff struct{ name, files, geo string }
	var diffs []diff
	geoCN, filesCN := 0, 0
	for _, n := range corpus {
		fr, fa, ft := route(rtFiles, n)
		gr, ga, gt := route(rtGeo, n)
		if fa == "forward" && ft == "aliyun" {
			filesCN++
		}
		if ga == "forward" && gt == "aliyun" {
			geoCN++
		}
		if fr != gr || fa != ga || ft != gt {
			diffs = append(diffs, diff{n, fr + "/" + fa + "/" + ft, gr + "/" + ga + "/" + gt})
		}
	}

	// A name routed to CN by the text list must never be routed abroad by the
	// geosite set, and vice versa: such a direction would produce a poisoned
	// answer, so the count must be zero.
	var dangerous []diff
	for _, d := range diffs {
		if (d.files == "cn/forward/aliyun" && d.geo != "cn/forward/aliyun") ||
			(d.geo == "cn/forward/aliyun" && d.files != "cn/forward/aliyun") {
			dangerous = append(dangerous, d)
		}
	}
	for _, n := range mustAbroad {
		if _, _, target := route(rtGeo, n); target == "aliyun" {
			t.Errorf("geosite routed %s to the CN upstream", n)
		}
	}
	for i, d := range diffs {
		if i == 20 {
			break
		}
		t.Logf("diff %-40s files=%-22s geosite=%s", d.name, d.files, d.geo)
	}
	t.Logf("corpus=%d files→cn=%d geosite→cn=%d diffs=%d dangerous=%d (%.4f%%)",
		len(corpus), filesCN, geoCN, len(diffs), len(dangerous), 100*float64(len(diffs))/float64(len(corpus)))
	if len(dangerous) > 0 {
		t.Fatalf("%d name(s) change side between the two list sources", len(dangerous))
	}
}
