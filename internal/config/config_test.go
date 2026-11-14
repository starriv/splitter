package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// valid is a minimal configuration that passes validation. Each rejection case
// below replaces one fragment of it.
const valid = `
listen: ["127.0.0.1:5336"]
admin: "127.0.0.1:9198"
upstreams:
  cn: {servers: ["223.5.5.5"]}
  abroad: {servers: ["1.1.1.1:53"], timeout: 1500ms}
sets:
  cn: {rules: ["baidu.com"]}
  local: {rules: ["local.example"]}
rules:
  - {name: local, sets: [local], answer: {a: ["192.0.2.10"]}}
  - {name: no-https, qtypes: [HTTPS], reject: nodata}
  - {name: cn, sets: [cn], forward: cn}
  - {name: default, forward: abroad}
`

// TestValid verifies that a well-formed configuration is accepted and that
// defaults, path resolution, and query type parsing behave as documented.
func TestValid(t *testing.T) {
	c, err := Parse([]byte(valid), "/etc/splitter")
	if err != nil {
		t.Fatal(err)
	}
	if got := c.Upstreams["cn"].ServerAddrs()[0].String(); got != "223.5.5.5:53" {
		t.Errorf("default port: %s", got)
	}
	if c.Rules[0].Answer.TTLSeconds() != DefaultAnswerTTL || c.Limits.MaxInflight != DefaultMaxInflight {
		t.Errorf("defaults not applied")
	}
	if c.Rules[1].QTypeCodes()[0] != 65 {
		t.Errorf("HTTPS qtype")
	}
	if c.Path("cn.txt") != "/etc/splitter/cn.txt" || c.Path("/x") != "/x" {
		t.Errorf("Path resolution")
	}
}

// TestValidationInvariants verifies the validation invariants: each case replaces one
// fragment of the valid configuration and expects a specific error message.
func TestValidationInvariants(t *testing.T) {
	cases := []struct{ old, new, want string }{
		{`["223.5.5.5"]`, `["dns.alidns.com"]`, "must be IP literals"},
		{`["127.0.0.1:5336"]`, `["0.0.0.0:53"]`, "wildcard"},
		{`["127.0.0.1:5336"]`, `["[::]:53"]`, "wildcard"},
		{`"127.0.0.1:9198"`, `":9198"`, "admin"},
		{"  - {name: default, forward: abroad}\n", "", "last rule must match everything"},
		{"{name: cn, sets: [cn], forward: cn}", "{name: cn, forward: cn}", "unreachable"},
		{"forward: abroad}", "forward: nope}", `unknown upstream "nope"`},
		{"sets: [cn], forward: cn", "sets: [cnn], forward: cn", `unknown set "cnn"`},
		{"reject: nodata", "reject: drop", "reject"},
		{"reject: nodata", "reject: nodata, forward: cn", "exactly one"},
		{`answer: {a: ["192.0.2.10"]}`, `answer: {a: ["::1"]}`, "not an IPv4"},
		{"qtypes: [HTTPS]", "qtypes: [BOGUS]", "unknown query type"},
		{"timeout: 1500ms", "timeout: 1m", "timeout"},
		{"timeout: 1500ms", "timeout: 1ms", "timeout"},
		{"admin:", "admni:", "field admni not found"},
		{`["1.1.1.1:53"], timeout`, `["127.0.0.1:5336"], timeout`, "forwarding loop"},
		{`"127.0.0.1:9198"`, `"127.0.0.1:5336"`, "also a DNS listen address"},
		{`answer: {a: ["192.0.2.10"]}`, `answer: {ttl: 60}`, "at least one address"},
		{`answer: {a: ["192.0.2.10"]}`, `answer: {a: ["192.0.2.10"], ttl: 99999999}`, "answer.ttl"},
		// Only loopback addresses may be bound, and IPv4-mapped spellings must
		// not bypass the wildcard, LAN, or self-forward checks.
		{`["127.0.0.1:5336"]`, `["192.0.2.10:5336"]`, "only loopback"},
		{`["127.0.0.1:5336"]`, `["[::ffff:0.0.0.0]:53"]`, "wildcard"},
		{`["127.0.0.1:5336"]`, `["[::ffff:8.8.8.8]:53"]`, "only loopback"},
		{`"127.0.0.1:9198"`, `"192.0.2.10:9198"`, "only loopback"},
		{`["1.1.1.1:53"], timeout`, `["[::ffff:127.0.0.1]:5336"], timeout`, "forwarding loop"},
		{`["1.1.1.1:53"], timeout`, `["1.1.1.1:53", "1.1.1.1:53"], timeout`, "listed twice"},
		{"qtypes: [HTTPS]", "qtypes: [0]", "reserved"},
		{"cn: {rules: [\"baidu.com\"]}", "cn: {files: [\"../outside.txt\"]}", "inside the config"},
		// The same file twice in a set would export duplicate freshness samples
		// and fail the entire /metrics scrape.
		{"cn: {rules: [\"baidu.com\"]}", "cn: {files: [\"a.txt\", \"./a.txt\"]}", "same file"},
		{"cn: {rules: [\"baidu.com\"]}", "cn: {geosite: [{file: g.dat, selectors: [cn]}, {file: g.dat, selectors: [google]}]}", "same file"},
		// Defined but unreferenced: the rule that should reference this entry
		// is missing.
		{"forward: abroad}", "reject: refused}", "upstreams.abroad: not used"},
		{"sets: [cn], forward: cn", "qtypes: [MX], forward: cn", "sets.cn: not used"},
		{"listen:", "limits: {max_set_shrink: 1.5}\nlisten:", "max_set_shrink"},
		{"listen:", "limits: {max_set_shrink: -0.1}\nlisten:", "max_set_shrink"},
		{"listen:", "limits: {max_inflight: 20000}\nlisten:", "max_inflight"},
		{"listen:", "admin_hosts: [\"dns.example:9198\"]\nlisten:", "admin_hosts[0]"},
		{"listen:", "admin_hosts: [\"https://dns.example\"]\nlisten:", "admin_hosts[0]"},
	}
	for _, c := range cases {
		cfg := strings.Replace(valid, c.old, c.new, 1)
		if cfg == valid {
			t.Fatalf("case %q did not change the config", c.old)
		}
		_, err := Parse([]byte(cfg), "/")
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%q -> %q: err = %v, want %q", c.old, c.new, err, c.want)
		}
	}
}

// TestMultiDocumentRejected verifies that a second YAML document is rejected: a
// merge or copy that leaves two configurations in one file must fail explicitly
// instead of being ignored.
func TestMultiDocumentRejected(t *testing.T) {
	if _, err := Parse([]byte(valid+"---\nlisten: [\"127.0.0.1:6000\"]\n"), "/"); err == nil {
		t.Fatal("a second YAML document was accepted")
	}
}

// TestExampleConfigValid verifies that the example configuration shipped with
// the repository always validates.
func TestExampleConfigValid(t *testing.T) {
	data, err := os.ReadFile("../config.example.yaml")
	if err != nil {
		t.Skipf("example config not readable: %v", err)
	}
	if _, err := Parse(data, ".."); err != nil {
		t.Fatalf("config.example.yaml does not validate: %v", err)
	}
}

// TestParseQType verifies that mnemonics and numeric codes are accepted and
// that values outside the 16-bit range are rejected.
func TestParseQType(t *testing.T) {
	for in, want := range map[string]uint16{"a": 1, "AAAA": 28, "https": 65, "99": 99} {
		got, err := ParseQType(in)
		if err != nil || uint16(got) != want {
			t.Errorf("ParseQType(%q) = %d, %v", in, got, err)
		}
	}
	if _, err := ParseQType("65536"); err == nil {
		t.Error("overflow accepted")
	}
}

// TestErrorsAreStable verifies that the error report for a broken configuration
// is deterministic and does not depend on map iteration order.
func TestErrorsAreStable(t *testing.T) {
	cfg := strings.NewReplacer(`["223.5.5.5"]`, `["a.example"]`, `["1.1.1.1:53"]`, `["b.example"]`).Replace(valid)
	_, first := Parse([]byte(cfg), "/")
	if first == nil {
		t.Fatal("broken config accepted")
	}
	for range 20 {
		if _, err := Parse([]byte(cfg), "/"); err.Error() != first.Error() {
			t.Fatalf("unstable report:\n%v\n---\n%v", first, err)
		}
	}
}

// TestNoUpstreamNeededWithoutForward verifies that a configuration that only
// answers and rejects requires no upstream group.
func TestNoUpstreamNeededWithoutForward(t *testing.T) {
	cfg := `
listen: ["127.0.0.1:5336"]
admin: "127.0.0.1:9198"
rules:
  - {name: all, reject: refused}
`
	if _, err := Parse([]byte(cfg), "/"); err != nil {
		t.Fatal(err)
	}
}

// TestAnswerTTLZeroKept verifies that an explicit ttl: 0 is kept instead of
// being replaced by the default, while an omitted ttl gets the default.
func TestAnswerTTLZeroKept(t *testing.T) {
	cfg := strings.Replace(valid, `answer: {a: ["192.0.2.10"]}`, `answer: {a: ["192.0.2.10"], ttl: 0}`, 1)
	c, err := Parse([]byte(cfg), "/")
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range c.Rules {
		if r.Answer != nil && r.Answer.TTLSeconds() != 0 {
			t.Errorf("ttl: 0 became %d", r.Answer.TTLSeconds())
		}
	}
}

// TestAdminHostAllowed verifies the Host check that protects the admin
// endpoints against DNS rebinding.
func TestAdminHostAllowed(t *testing.T) {
	cfg := strings.Replace(valid, "listen:", "admin_hosts: [DNS.Example.]\nlisten:", 1)
	c, err := Parse([]byte(cfg), "/")
	if err != nil {
		t.Fatal(err)
	}
	for host, want := range map[string]bool{
		"":                        true,
		"127.0.0.1:9198":          true,
		"[::1]:9198":              true,
		"localhost:9198":          true,
		"LOCALHOST":               true,
		"dns.example":             true,
		"dns.example:443":         true,
		"rebind.attacker.io":      false,
		"rebind.attacker.io:9198": false,
		"localhost.attacker.io":   false,
	} {
		if got := c.AdminHostAllowed(host); got != want {
			t.Errorf("AdminHostAllowed(%q) = %v, want %v", host, got, want)
		}
	}
}

// TestLoadAdminAddr verifies that the health check can find the admin
// address in a file whose other sections no longer validate.
func TestLoadAdminAddr(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "config.yaml")
	broken := strings.Replace(valid, "forward: abroad}", "forward: nope}", 1)
	if err := os.WriteFile(p, []byte(broken), 0o644); err != nil {
		t.Fatal(err)
	}
	ap, err := LoadAdminAddr(p)
	if err != nil || ap.String() != "127.0.0.1:9198" {
		t.Errorf("LoadAdminAddr = %v, %v", ap, err)
	}
	if err := os.WriteFile(p, []byte(valid+"rules: [\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if ap, err := LoadAdminAddr(p); err != nil || ap.String() != "127.0.0.1:9198" {
		t.Errorf("LoadAdminAddr with a YAML syntax error = %v, %v", ap, err)
	}
	if err := os.WriteFile(p, []byte("admin: \"10.0.0.1:9198\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadAdminAddr(p); err == nil {
		t.Error("non-loopback admin address accepted")
	}
}

// TestLogDefaults verifies the log directory and query log defaults and that
// relative directories resolve against the configuration directory.
func TestLogDefaults(t *testing.T) {
	c, err := Parse([]byte(valid), "/etc/splitter")
	if err != nil {
		t.Fatal(err)
	}
	got := c.QueryLogSettings()
	want := QueryLogSettings{Enabled: true, Dir: DefaultLogDir, MaxBytes: DefaultQueryLogMaxSizeMB << 20, MaxAge: DefaultQueryLogMaxAge}
	if got != want {
		t.Fatalf("defaults: %+v", got)
	}

	c, err = Parse([]byte(valid+"log:\n  dir: logs\n  query_log: {enabled: false, max_size_mb: 8, max_age: 24h}\n"), "/etc/splitter")
	if err != nil {
		t.Fatal(err)
	}
	got = c.QueryLogSettings()
	want = QueryLogSettings{Enabled: false, Dir: "/etc/splitter/logs", MaxBytes: 8 << 20, MaxAge: 24 * time.Hour}
	if got != want {
		t.Fatalf("explicit: %+v", got)
	}
}

// TestLogRejects verifies that out-of-range query log settings are reported.
func TestLogRejects(t *testing.T) {
	for _, tc := range []struct{ frag, want string }{
		{"log: {query_log: {max_size_mb: -1}}", "max_size_mb"},
		{"log: {query_log: {max_size_mb: 100000}}", "max_size_mb"},
		{"log: {query_log: {max_age: 1m}}", "max_age"},
		{"log: {query_log: {max_age: 9000h}}", "max_age"},
		{`log: {dir: " /tmp"}`, "log.dir"},
	} {
		_, err := Parse([]byte(valid+tc.frag+"\n"), "/etc/splitter")
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: got %v, want %q", tc.frag, err, tc.want)
		}
	}
}

// TestObservability verifies the optional observability block: URL validation,
// defaults, and the requirement that a Grafana link only exists when the page
// can be enabled.
func TestObservability(t *testing.T) {
	parse := func(block string) (*Config, error) {
		return Parse([]byte(valid+"\nobservability:\n"+block), "/etc/splitter")
	}

	c, err := parse("  prometheus_url: http://127.0.0.1:9090/prom\n")
	if err != nil {
		t.Fatal(err)
	}
	if c.Observability.PrometheusURL != "http://127.0.0.1:9090/prom" {
		t.Errorf("url = %q", c.Observability.PrometheusURL)
	}
	if c.Observability.Job != DefaultObservabilityJob {
		t.Errorf("default job = %q", c.Observability.Job)
	}
	if got := time.Duration(c.Observability.Timeout); got != DefaultObservabilityTimeout {
		t.Errorf("default timeout = %s", got)
	}

	c, err = parse("  prometheus_url: https://prometheus.example\n  job: splitter-prod\n  dashboard_url: https://grafana.example/d/x?from=now-6h\n  timeout: 2s\n")
	if err != nil {
		t.Fatal(err)
	}
	if c.Observability.Job != "splitter-prod" || time.Duration(c.Observability.Timeout) != 2*time.Second {
		t.Errorf("observability = %+v", c.Observability)
	}
	if c.Observability.DashboardURL != "https://grafana.example/d/x?from=now-6h" {
		t.Errorf("dashboard url = %q", c.Observability.DashboardURL)
	}

	for _, tc := range []struct{ block, want string }{
		{"  prometheus_url: ftp://prometheus\n", "scheme must be http or https"},
		{"  prometheus_url: http://user:pass@prometheus\n", "credentials must not be embedded"},
		{"  prometheus_url: http://prometheus/x?y=1\n", "must not carry a query"},
		{"  prometheus_url: prometheus\n", "scheme must be http or https"},
		{"  dashboard_url: https://grafana.example/d/x\n", "requires observability.prometheus_url"},
		{"  prometheus_url: http://prometheus\n  job: \"two words\"\n", "observability.job"},
		{"  prometheus_url: http://prometheus\n  timeout: 500ms\n", "observability.timeout"},
		{"  prometheus_url: http://prometheus\n  timeout: 2h\n", "observability.timeout"},
		{"  prometheus_url: http://prometheus\n  unknown: x\n", "field unknown not found"},
	} {
		if _, err := parse(tc.block); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("block %q: error = %v, want %q", tc.block, err, tc.want)
		}
	}
}
