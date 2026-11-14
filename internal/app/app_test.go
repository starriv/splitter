package app

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/net/dns/dnsmessage"

	"github.com/starriv/splitter/internal/config"
	"github.com/starriv/splitter/internal/dnstest"
	"github.com/starriv/splitter/internal/domain"
	"github.com/starriv/splitter/internal/metrics"
	"github.com/starriv/splitter/internal/server"
	"github.com/starriv/splitter/internal/wire"
)

var (
	cnAddr     = netip.MustParseAddr("10.0.0.1")
	abroadAddr = netip.MustParseAddr("10.0.0.2")
	localAddr  = netip.MustParseAddr("192.0.2.10")
)

type env struct {
	app    *App
	dns    netip.AddrPort
	cn, ab *dnstest.Upstream
	dir    string
	cfg    string
}

// writeConfig writes the configuration shared by the app tests: the local zone
// is answered locally, the cn set forwards to one upstream group, and every
// other query forwards to another group.
func writeConfig(t testing.TB, dir string, cn, ab netip.AddrPort, cnList string, queryLogOff ...bool) string {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "cn-domains.txt"), []byte(cnList), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := fmt.Sprintf(`
listen: ["127.0.0.1:0"]
admin: "127.0.0.1:0"
log: {dir: logs, query_log: {enabled: %t}}
upstreams:
  aliyun: {servers: ["%s"], timeout: 1s}
  cloudflare: {servers: ["%s"], timeout: 1s}
sets:
  local: {rules: ["domain:local.example"]}
  cn: {files: [cn-domains.txt]}
rules:
  - {name: local-zone, sets: [local], answer: {a: ["192.0.2.10"], ttl: 60}}
  - {name: cn, sets: [cn], forward: aliyun}
  - {name: default, forward: cloudflare}
`, len(queryLogOff) == 0 || !queryLogOff[0], cn, ab)
	p := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(p, []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// start creates a test environment: two fake upstreams, a configuration with
// the shared routing shape, and a running DNS server that is shut down during
// test cleanup.
// start passes queryLogOff to writeConfig.
func start(t *testing.T, queryLogOff ...bool) *env {
	t.Helper()
	e := &env{dir: t.TempDir()}
	e.cn = dnstest.StartUpstream(t, cnAddr)
	e.ab = dnstest.StartUpstream(t, abroadAddr)
	e.cfg = writeConfig(t, e.dir, e.cn.Addr, e.ab.Addr, "# test\ndomain:baidu.com\nfull:www.qq.com\n", queryLogOff...)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	m := metrics.New("test", "test")
	a, err := New(e.cfg, log, new(slog.LevelVar), m)
	if err != nil {
		t.Fatal(err)
	}
	e.app = a
	srv := server.New(a, server.Options{Observer: m})
	if err := srv.Listen([]netip.AddrPort{netip.MustParseAddrPort("127.0.0.1:0")}); err != nil {
		t.Fatal(err)
	}
	srv.Serve()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
		_ = a.Close()
	})
	e.dns = srv.Addrs()[0]
	return e
}

func (e *env) ask(t *testing.T, network, name string, qt dnsmessage.Type) dnsmessage.Message {
	t.Helper()
	return dnstest.Exchange(t, network, e.dns, dnstest.NewQuery(t, 0x1234, name, qt, true))
}

// TestRouting verifies routing decisions over both UDP and TCP: exact and
// subdomain matches, case folding, full-match semantics, and the local zone.
func TestRouting(t *testing.T) {
	e := start(t)
	cases := []struct {
		name string
		want netip.Addr
	}{
		{"www.baidu.com.", cnAddr},
		{"BAIDU.COM.", cnAddr},
		{"www.qq.com.", cnAddr},
		{"m.www.qq.com.", abroadAddr}, // full: does not cover subdomains
		{"www.google.com.", abroadAddr},
		{"home.local.example.", localAddr},
		{"a.b.local.example.", localAddr},
	}
	for _, network := range []string{"udp", "tcp"} {
		for _, c := range cases {
			m := e.ask(t, network, c.name, dnsmessage.TypeA)
			if got := dnstest.FirstA(m); got != c.want {
				t.Errorf("%s %s -> %v, want %v", network, c.name, got, c.want)
			}
			if m.Header.ID != 0x1234 || m.Header.RCode != dnsmessage.RCodeSuccess {
				t.Errorf("%s %s header %+v", network, c.name, m.Header)
			}
		}
	}
}

// TestLocalZoneNeverLeaks verifies that the local answer rule never reaches
// an upstream for any query type, returns an authoritative NOERROR reply,
// and echoes an OPT record to EDNS queries.
func TestLocalZoneNeverLeaks(t *testing.T) {
	e := start(t)
	before := e.cn.Queries.Load() + e.ab.Queries.Load()
	for _, qt := range []dnsmessage.Type{dnsmessage.TypeA, dnsmessage.TypeAAAA, dnsmessage.TypeTXT, 65} {
		m := e.ask(t, "udp", "nas.local.example.", qt)
		if m.Header.RCode != dnsmessage.RCodeSuccess || !m.Header.Authoritative {
			t.Errorf("type %d: header %+v", qt, m.Header)
		}
		if qt != dnsmessage.TypeA && len(m.Answers) != 0 {
			t.Errorf("type %d: want NODATA, got %d answers", qt, len(m.Answers))
		}
		if len(m.Additionals) != 1 || m.Additionals[0].Header.Type != dnsmessage.TypeOPT {
			t.Errorf("type %d: EDNS query must get an OPT back", qt)
		}
	}
	if after := e.cn.Queries.Load() + e.ab.Queries.Load(); after != before {
		t.Fatalf("local zone reached an upstream (%d queries)", after-before)
	}
}

// TestInvalidQueries verifies the error contract for malformed input:
// FORMERR for a query without a question, no reply to response packets,
// NOTIMP for other opcodes, BADVERS for unsupported EDNS versions, and
// REFUSED for non-IN classes.
func TestInvalidQueries(t *testing.T) {
	e := start(t)
	// Header only, QDCOUNT=0 -> FORMERR.
	m := dnstest.Exchange(t, "udp", e.dns, []byte{0xAB, 0xCD, 1, 0, 0, 0, 0, 0, 0, 0, 0, 0})
	if m.Header.RCode != dnsmessage.RCodeFormatError || m.Header.ID != 0xABCD {
		t.Errorf("no question: %+v", m.Header)
	}
	// Responses get no reply at all, well-formed or not.
	for _, pkt := range [][]byte{
		{0xAB, 0xCD, 0x81, 0x80, 0, 0, 0, 0, 0, 0, 0, 0}, // QR=1, no question (malformed)
		dnstest.NewQuery(t, 0x99, "x.cn.", dnsmessage.TypeA, false),
	} {
		raw := append([]byte(nil), pkt...)
		raw[2] |= 0x80
		if resp := e.app.ServeDNS(context.Background(), raw, 0); resp != nil {
			t.Errorf("answered a response packet: %x", resp)
		}
	}
	// Another opcode returns NOTIMP even when it carries no question (STATUS):
	// FORMATERROR would misdescribe a message whose format is valid but whose
	// opcode is not implemented.
	m = dnstest.Exchange(t, "udp", e.dns, []byte{0xAB, 0xCE, 0x10, 0, 0, 0, 0, 0, 0, 0, 0, 0})
	if m.Header.RCode != dnsmessage.RCodeNotImplemented {
		t.Errorf("STATUS without a question: %+v", m.Header)
	}
	// EDNS version 1 -> BADVERS (extended RCODE 16), for local and forwarded names alike.
	for _, name := range []string{"nas.local.example.", "www.baidu.com."} {
		raw := dnstest.NewQuery(t, 0x77, name, dnsmessage.TypeA, true)
		raw[len(raw)-5] = 1 // OPT TTL = ext-rcode, version, flags(2); then RDLENGTH(2)
		before := e.cn.Queries.Load()
		resp := dnstest.ExchangeRaw(t, "udp", e.dns, raw)
		if rc := wire.EffectiveRCode(resp); rc != 16 {
			t.Errorf("%s with EDNS version 1: rcode %s, want BADVERS", name, wire.RCodeName(rc))
		}
		if e.cn.Queries.Load() != before {
			t.Errorf("%s with EDNS version 1 was forwarded", name)
		}
	}
	// CHAOS class -> REFUSED, never forwarded.
	q := dnsmessage.Message{Header: dnsmessage.Header{ID: 7}, Questions: []dnsmessage.Question{{
		Name: dnsmessage.MustNewName("version.bind."), Type: dnsmessage.TypeTXT, Class: dnsmessage.ClassCHAOS}}}
	raw, _ := q.Pack()
	if m := dnstest.Exchange(t, "udp", e.dns, raw); m.Header.RCode != dnsmessage.RCodeRefused {
		t.Errorf("chaos: %+v", m.Header)
	}
}

// TestUpstreamDownGivesServfail verifies that a timed-out upstream produces
// SERVFAIL instead of a dropped query.
func TestUpstreamDownGivesServfail(t *testing.T) {
	e := start(t)
	writeConfig(t, e.dir, e.cn.Addr, dnstest.BlackHole(t), "domain:baidu.com\n")
	if err := e.app.Reload(); err != nil {
		t.Fatal(err)
	}
	m := e.ask(t, "udp", "www.google.com.", dnsmessage.TypeA)
	if m.Header.RCode != dnsmessage.RCodeServerFailure {
		t.Fatalf("rcode %v, want SERVFAIL", m.Header.RCode)
	}
}

// TestReload verifies that a reload applies new lists, that a malformed list
// is rejected while the previous runtime keeps serving, and that an empty
// list is rejected.
func TestReload(t *testing.T) {
	e := start(t)
	if got := dnstest.FirstA(e.ask(t, "udp", "www.bilibili.com.", dnsmessage.TypeA)); got != abroadAddr {
		t.Fatalf("before reload: %v", got)
	}
	writeConfig(t, e.dir, e.cn.Addr, e.ab.Addr, "domain:baidu.com\ndomain:bilibili.com\n")
	if err := e.app.Reload(); err != nil {
		t.Fatal(err)
	}
	if got := dnstest.FirstA(e.ask(t, "udp", "www.bilibili.com.", dnsmessage.TypeA)); got != cnAddr {
		t.Fatalf("after reload: %v", got)
	}

	// A broken list is rejected and the previous runtime keeps serving.
	writeConfig(t, e.dir, e.cn.Addr, e.ab.Addr, "domain:baidu.com\nnot a rule\n")
	if err := e.app.Reload(); err == nil {
		t.Fatal("broken list accepted")
	}
	if got := dnstest.FirstA(e.ask(t, "udp", "www.bilibili.com.", dnsmessage.TypeA)); got != cnAddr {
		t.Fatalf("after failed reload: %v", got)
	}
	// An empty list is rejected too: an empty cn set would send everything abroad.
	writeConfig(t, e.dir, e.cn.Addr, e.ab.Addr, "# nothing\n")
	if err := e.app.Reload(); err == nil || !strings.Contains(err.Error(), "zero rules") {
		t.Fatalf("empty list: %v", err)
	}
}

// TestAdminEndpoints verifies the legacy route endpoint, the metric series
// that exist before any failure occurs, and the health endpoint body.
func TestAdminEndpoints(t *testing.T) {
	e := start(t)
	e.ask(t, "udp", "www.baidu.com.", dnsmessage.TypeA)
	h := e.app.AdminHandler()

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, adminRequest("GET", "/debug/route?name=WWW.Baidu.com.&type=aaaa", nil))
	var ri RouteInfo
	if err := json.Unmarshal(rec.Body.Bytes(), &ri); err != nil {
		t.Fatal(err)
	}
	want := RouteInfo{Name: "www.baidu.com", Type: "AAAA", Rule: "cn", Action: "forward", Target: "aliyun", Set: "cn", Match: "domain"}
	if ri != want {
		t.Errorf("route = %+v", ri)
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, adminRequest("GET", "/metrics", nil))
	body := rec.Body.String()
	for _, s := range []string{
		`splitter_queries_total{action="forward",rule="cn",target="aliyun"} 1`,
		`splitter_upstream_requests_total{server="` + e.cn.Addr.String() + `",upstream="aliyun"} 1`,
		`splitter_set_entries{kind="domain",set="cn"} 1`,
		`splitter_set_entries{kind="full",set="cn"} 1`,
		`splitter_reloads_total{result="success"} 1`,
		`splitter_responses_total{rcode="NOERROR"} 1`,
		// Failure series exist at zero before anything fails, so the first
		// failure is an increase that alerting can observe.
		`splitter_reloads_total{result="failure"} 0`,
		`splitter_config_last_reload_successful 1`,
		`splitter_overload_dropped_total{proto="udp"} 0`,
		`splitter_response_write_errors_total{proto="tcp"} 0`,
		`splitter_upstream_errors_total{reason="timeout",server="` + e.ab.Addr.String() + `",upstream="cloudflare"} 0`,
		`splitter_upstream_retransmits_total{server="` + e.cn.Addr.String() + `",upstream="aliyun"} 0`,
		`splitter_panics_total 0`,
	} {
		if !strings.Contains(body, s) {
			t.Errorf("metrics missing %s", s)
		}
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, adminRequest("GET", "/healthz", nil))
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"cn":2`) {
		t.Errorf("healthz %d %s", rec.Code, rec.Body)
	}
}

// domainList returns the path named by SPLITTER_DOMAIN_LIST and whether that
// file exists. The opt-in tests that use it are skipped otherwise.
func domainList() (string, bool) {
	p := os.Getenv("SPLITTER_DOMAIN_LIST")
	if p == "" {
		return "", false
	}
	if _, err := os.Stat(p); err != nil {
		return "", false
	}
	return p, true
}

// TestDomainListEquivalence loads a full domain list when one is available
// and verifies the matching semantics the splitter relies on: every domain:
// entry matches itself and its subdomains, and every full: entry matches
// itself only.
//
// The repository ships only a small fixture. Set SPLITTER_DOMAIN_LIST to a
// full list, as an absolute path or a path relative to this package
// directory.
func TestDomainListEquivalence(t *testing.T) {
	path, ok := domainList()
	if !ok {
		t.Skip("SPLITTER_DOMAIN_LIST is not set to an existing file")
	}
	f, err := os.Open(path)
	if err != nil {
		t.Skipf("domain list not available: %v", err)
	}
	defer f.Close()
	b := domain.NewBuilder()
	if _, err := domain.LoadFile(path, b); err != nil {
		t.Fatal(err)
	}
	cn := b.Build()

	var domains, fulls int
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		r, err := domain.ParseRule(sc.Text())
		if err != nil {
			continue // comments and blanks
		}
		switch r.Kind {
		case domain.KindDomain:
			domains++
			for _, n := range []string{r.Value, "x." + r.Value, "a.b." + r.Value} {
				if _, ok := cn.Match(n); !ok {
					t.Errorf("%s does not match %s", r, n)
				}
			}
		case domain.KindFull:
			fulls++
			if k, ok := cn.Match(r.Value); !ok {
				t.Errorf("%s does not match itself", r)
			} else if k != domain.KindFull && k != domain.KindDomain {
				t.Errorf("%s matched as %s", r, k)
			}
		}
	}
	if domains < 1000 || fulls == 0 {
		t.Fatalf("list looks wrong: %d domain, %d full", domains, fulls)
	}
	for _, n := range []string{"google.com", "www.youtube.com", "github.com", "openai.com"} {
		if _, ok := cn.Match(n); ok {
			t.Errorf("foreign domain %s is in the cn set", n)
		}
	}
	t.Logf("checked %d domain and %d full rules", domains, fulls)
}

// TestExampleConfig verifies that the shipped example configuration remains
// loadable and buildable.
func TestExampleConfig(t *testing.T) {
	dir := t.TempDir()
	listPath, ok := domainList()
	if !ok {
		t.Skip("SPLITTER_DOMAIN_LIST is not set to an existing file")
	}
	for src, dst := range map[string]string{
		filepath.Join("..", "..", "config.example.yaml"): "config.yaml",
		listPath: "cn-domains.txt",
	} {
		data, err := os.ReadFile(src)
		if err != nil {
			t.Skipf("%v", err)
		}
		if err := os.WriteFile(filepath.Join(dir, dst), data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	cfg, err := config.Load(filepath.Join(dir, "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Build(cfg, nil); err != nil {
		t.Fatal(err)
	}
}

// BenchmarkServeDNS measures the per-query cost of the local-answer path.
func BenchmarkServeDNS(b *testing.B) {
	dir := b.TempDir()
	cn := dnstest.StartUpstream(b, cnAddr)
	ab := dnstest.StartUpstream(b, abroadAddr)
	var list strings.Builder
	for i := range 10000 {
		fmt.Fprintf(&list, "domain:site%d.cn\n", i)
	}
	p := writeConfig(b, dir, cn.Addr, ab.Addr, list.String())
	a, err := New(p, slog.New(slog.DiscardHandler), new(slog.LevelVar), metrics.New("b", "b"))
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = a.Close() })
	req := dnstest.NewQuery(b, 1, "x.local.example.", dnsmessage.TypeA, true)
	b.ReportAllocs()
	for b.Loop() {
		if a.ServeDNS(context.Background(), req, 0) == nil {
			b.Fatal("nil")
		}
	}
}

// TestReadyzReflectsFailedReload verifies that readiness reports a refused
// reload while liveness stays green: the process keeps serving the previous
// runtime, but a deploy or healthcheck must observe that the newly shipped
// configuration was rejected.
func TestReadyzReflectsFailedReload(t *testing.T) {
	e := start(t)
	h := e.app.AdminHandler()
	get := func(path string) (int, map[string]any) {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, adminRequest("GET", path, nil))
		var body map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("%s body: %v", path, err)
		}
		return rec.Code, body
	}

	if code, body := get("/readyz"); code != http.StatusOK || body["last_reload_ok"] != true {
		t.Fatalf("ready after start: %d %v", code, body)
	}
	if err := os.WriteFile(e.cfg, []byte("listen: [\"0.0.0.0:53\"]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := e.app.Reload(); err == nil {
		t.Fatal("broken config accepted")
	}
	code, body := get("/readyz")
	if code != http.StatusServiceUnavailable {
		t.Fatalf("readiness = %d, want 503", code)
	}
	if body["last_reload_ok"] != false || body["last_reload_error"] == nil {
		t.Fatalf("readyz body: %v", body)
	}
	if code, _ := get("/healthz"); code != http.StatusOK {
		t.Fatalf("healthz = %d, want 200 (the process is alive)", code)
	}
	// A valid configuration restores readiness.
	writeConfig(t, e.dir, e.cn.Addr, e.ab.Addr, "domain:baidu.com\n")
	if err := e.app.Reload(); err != nil {
		t.Fatal(err)
	}
	if code, _ := get("/readyz"); code != http.StatusOK {
		t.Fatalf("readiness did not recover: %d", code)
	}
}

// TestBuildFailsClosed verifies that Build, a public entry point also used by
// -check, fails closed instead of inserting a nil set or inventing a reject
// action.
func TestBuildFailsClosed(t *testing.T) {
	dir := t.TempDir()
	text := `
listen: ["127.0.0.1:0"]
admin: "127.0.0.1:0"
upstreams:
  u: {servers: ["1.1.1.1:53"]}
sets:
  s: {rules: ["baidu.com"]}
rules:
  - {name: s, sets: [s], forward: u}
  - {name: default, forward: u}
`
	cfg, err := config.Parse([]byte(text), dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Build(cfg, nil); err != nil {
		t.Fatalf("valid config: %v", err)
	}

	cfg.Rules[0].Sets = []string{"missing"}
	if _, err := Build(cfg, nil); err == nil {
		t.Fatal("unknown set accepted")
	}

	cfg2, err := config.Parse([]byte(text), dir)
	if err != nil {
		t.Fatal(err)
	}
	cfg2.Rules[0].Forward = ""
	if _, err := Build(cfg2, nil); err == nil {
		t.Fatal("rule without an action accepted")
	}
}

// TestReloadRefusesShrunkenSet verifies that a non-empty but truncated list
// does not replace a valid one: every entry it no longer contains would be
// rerouted.
func TestReloadRefusesShrunkenSet(t *testing.T) {
	e := start(t)
	var big strings.Builder
	for i := range 200 {
		fmt.Fprintf(&big, "domain:d%d.example.cn\n", i)
	}
	writeConfig(t, e.dir, e.cn.Addr, e.ab.Addr, big.String())
	if err := e.app.Reload(); err != nil {
		t.Fatal(err)
	}

	// 200 -> 60 entries, the last line cut in the middle of a name.
	lines := strings.SplitAfter(big.String(), "\n")
	writeConfig(t, e.dir, e.cn.Addr, e.ab.Addr, strings.Join(lines[:59], "")+"domain:d59.exa")
	err := e.app.Reload()
	if err == nil || !strings.Contains(err.Error(), "max_set_shrink") {
		t.Fatalf("truncated list: err = %v", err)
	}
	if got := e.app.Explain("d150.example.cn", dnsmessage.TypeA).Rule; got != "cn" {
		t.Errorf("after the refused reload d150.example.cn routes to %q, want cn", got)
	}
	rec := httptest.NewRecorder()
	e.app.AdminHandler().ServeHTTP(rec, adminRequest("GET", "/readyz", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("readyz after a refused reload: %d", rec.Code)
	}
	rec = httptest.NewRecorder()
	e.app.AdminHandler().ServeHTTP(rec, adminRequest("GET", "/metrics", nil))
	if !strings.Contains(rec.Body.String(), "splitter_config_last_reload_successful 0") {
		t.Error("config_last_reload_successful did not drop to 0")
	}

	// A moderate reduction (200 -> 150 entries) and growth are accepted.
	writeConfig(t, e.dir, e.cn.Addr, e.ab.Addr, strings.Join(lines[:150], ""))
	if err := e.app.Reload(); err != nil {
		t.Fatalf("25%% smaller list refused: %v", err)
	}
	// The limit is the escape hatch for a deliberate reduction.
	cfg, err := os.ReadFile(e.cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(e.dir, "cn-domains.txt"), []byte(strings.Join(lines[:20], "")), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := e.app.Reload(); err == nil {
		t.Fatal("150 -> 20 accepted with the default limit")
	}
	if err := os.WriteFile(e.cfg, append(cfg, []byte("limits: {max_set_shrink: 1}\n")...), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := e.app.Reload(); err != nil {
		t.Fatalf("max_set_shrink: 1 did not disable the guard: %v", err)
	}
}

// TestMetricsScrapeIsValid verifies that Build never hands the collector two
// samples with identical labels, even for a configuration the validator did
// not see, because that fails the whole scrape.
func TestMetricsScrapeIsValid(t *testing.T) {
	e := start(t)
	rt := e.app.Runtime()
	seen := map[string]bool{}
	for _, s := range rt.Sources {
		if k := s.Set + "\x00" + s.File; seen[k] {
			t.Errorf("source published twice: set %s file %s", s.Set, s.File)
		} else {
			seen[k] = true
		}
	}
	rec := httptest.NewRecorder()
	e.app.AdminHandler().ServeHTTP(rec, adminRequest("GET", "/metrics", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("/metrics: %d %s", rec.Code, rec.Body)
	}
}
