// Package config loads and validates the splitter's YAML configuration.
//
// Validation enforces three safety invariants:
//   - upstreams are IP literals; a hostname would make the resolver depend on
//     resolving its own upstream (a bootstrap loop);
//   - nothing binds a wildcard address, and only loopback addresses may be
//     bound;
//   - the final rule matches every query, so every query has exactly one
//     decision.
//
// All problems are reported together, not only the first one.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"
	"golang.org/x/net/dns/dnsmessage"

	"github.com/starriv/splitter/internal/geosite"
)

// Duration is a time.Duration that decodes from a Go duration string such as
// "2s" or "500ms".
type Duration time.Duration

// UnmarshalYAML decodes a YAML scalar as a Go duration string.
func (d *Duration) UnmarshalYAML(n *yaml.Node) error {
	var s string
	if err := n.Decode(&s); err != nil {
		return err
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return err
	}
	*d = Duration(v)
	return nil
}

// Config is the parsed configuration. Defaults are applied and addresses are
// validated during parsing; validated addresses are kept separately from the
// textual form written in the file.
//
// AdminHosts lists extra host names under which the admin endpoints may be
// reached, typically through a reverse proxy. IP literals and "localhost" are
// always accepted; any other Host header is refused, which stops
// DNS-rebinding pages from reading the admin API.
type Config struct {
	Listen        []string            `yaml:"listen"`        // client addresses to serve DNS on
	Admin         string              `yaml:"admin"`         // address for metrics and health endpoints
	AdminHosts    []string            `yaml:"admin_hosts"`   // extra Host names accepted by the admin endpoints
	Log           Log                 `yaml:"log"`           // logging configuration
	Limits        Limits              `yaml:"limits"`        // concurrency and reload limits
	Observability Observability       `yaml:"observability"` // the optional Prometheus-backed console page
	Upstreams     map[string]Upstream `yaml:"upstreams"`     // named upstream groups
	Sets          map[string]Set      `yaml:"sets"`          // named domain matchers
	Rules         []Rule              `yaml:"rules"`         // rules evaluated in order

	dir        string
	listen     []netip.AddrPort
	admin      netip.AddrPort
	adminHosts []string
}

// Log configures logging.
//
// Dir is the directory the splitter writes its own files to; the query log
// lives there. It defaults to DefaultLogDir. When the query log is enabled and
// Dir cannot be written, startup fails.
type Log struct {
	Level    string   `yaml:"level"`     // debug, info, warn, error
	Queries  bool     `yaml:"queries"`   // one line per query at info level on stderr
	Dir      string   `yaml:"dir"`       // directory for log files
	QueryLog QueryLog `yaml:"query_log"` // every answered query persisted under Dir
}

// QueryLog configures the on-disk query log read by the control page. Enabled
// is a pointer so that an omitted value can default to true.
type QueryLog struct {
	Enabled   *bool    `yaml:"enabled"`     // default true
	MaxSizeMB int      `yaml:"max_size_mb"` // total size of all query log files
	MaxAge    Duration `yaml:"max_age"`     // files older than this are removed
}

// Limits bounds the server's concurrency and the size change that a reload may
// cause.
type Limits struct {
	MaxInflight    int      `yaml:"max_inflight"`     // maximum number of queries processed concurrently
	MaxTCPConns    int      `yaml:"max_tcp_conns"`    // client TCP connections kept open at once
	TCPIdleTimeout Duration `yaml:"tcp_idle_timeout"` // idle time before a client TCP connection is closed
	// MaxSetShrink is the largest share of its entries a set may lose in one
	// reload, in (0, 1]; 1 turns the guard off. Applied on reload, never at
	// startup (there is nothing to compare against).
	MaxSetShrink float64 `yaml:"max_set_shrink"`
}

// Observability configures the control console's observability page, which
// reads the splitter's own series back from a Prometheus server. Prometheus is
// the only component that stores history, so the page asks it for the range
// charts instead of keeping a second time-series store in the resolver.
//
// The block is optional. With PrometheusURL empty the page explains how to
// enable it and makes no network requests.
type Observability struct {
	// PrometheusURL is the base URL of the Prometheus server, for example
	// "http://127.0.0.1:9090" or "https://prometheus.example/prom".
	PrometheusURL string `yaml:"prometheus_url"`
	// Job is the scrape job name under which Prometheus scrapes this splitter,
	// used by the scrape-health panel. Defaults to "splitter".
	Job string `yaml:"job"`
	// DashboardURL is an optional link to the Grafana dashboard the page
	// reproduces, shown as "在 Grafana 中打开".
	DashboardURL string `yaml:"dashboard_url"`
	// Timeout bounds one Prometheus API request.
	Timeout Duration `yaml:"timeout"`
}

// Upstream is a named group of DNS servers. Servers are tried in order, and
// Timeout applies to each attempt.
type Upstream struct {
	Servers []string `yaml:"servers"` // "ip" or "ip:port", tried in order
	Timeout Duration `yaml:"timeout"` // per attempt

	addrs []netip.AddrPort
}

// Set is a named collection of domain rules loaded from list files, inline
// rules, or geosite selections.
type Set struct {
	Files   []string  `yaml:"files"`   // rule lists, one rule per line
	Rules   []string  `yaml:"rules"`   // inline rules
	Geosite []Geosite `yaml:"geosite"` // v2ray geosite.dat selections
}

// Geosite selects entries from a v2ray geosite data file.
type Geosite struct {
	File      string   `yaml:"file"`      // path to the geosite data file
	Selectors []string `yaml:"selectors"` // e.g. "cn", "cn@-!cn"

	parsed []geosite.Selector
}

// Rule maps matching queries to exactly one action.
type Rule struct {
	Name    string   `yaml:"name"`    // rule name used in logs and in duplicate-name checks
	Sets    []string `yaml:"sets"`    // any of these sets matches
	QTypes  []string `yaml:"qtypes"`  // and the query type is one of these
	Forward string   `yaml:"forward"` // name of the upstream group to forward to
	Answer  *Answer  `yaml:"answer"`  // fixed records to answer with
	Reject  string   `yaml:"reject"`  // nxdomain, nodata, refused, servfail

	qtypes []dnsmessage.Type
}

// Answer holds fixed address records returned locally.
type Answer struct {
	A    []string `yaml:"a"`    // IPv4 addresses
	AAAA []string `yaml:"aaaa"` // IPv6 addresses
	TTL  *uint32  `yaml:"ttl"`  // record TTL in seconds; unset means DefaultAnswerTTL, 0 is allowed

	a, aaaa []netip.Addr
}

const (
	// DefaultMaxInflight is the default limit on concurrent queries.
	DefaultMaxInflight = 1024
	// DefaultMaxTCPConns is the default limit on open client TCP connections.
	DefaultMaxTCPConns = 256
	// DefaultTCPIdleTimeout is the default idle timeout for client TCP
	// connections.
	DefaultTCPIdleTimeout = 10 * time.Second
	// DefaultMaxSetShrink is the default maximum fraction of entries a set may
	// lose in one reload.
	DefaultMaxSetShrink = 0.5
	// DefaultTimeout is the default per-attempt upstream timeout.
	DefaultTimeout = 2 * time.Second
	// DefaultAnswerTTL is the default TTL, in seconds, of locally answered
	// records.
	DefaultAnswerTTL = 60
	// DefaultLogDir is the default log directory. The container image creates
	// it, owned by the runtime user.
	DefaultLogDir = "/var/log/splitter"
	// DefaultQueryLogMaxSizeMB is the default size cap of the query log.
	DefaultQueryLogMaxSizeMB = 4096
	// DefaultQueryLogMaxAge is the default retention of the query log.
	DefaultQueryLogMaxAge = 30 * 24 * time.Hour
	// DefaultObservabilityJob is the default Prometheus scrape job name of the
	// splitter.
	DefaultObservabilityJob = "splitter"
	// DefaultObservabilityTimeout is the default deadline for one Prometheus
	// API request made by the control page.
	DefaultObservabilityTimeout = 8 * time.Second

	// Each in-flight query may hold one 4 KiB upstream receive buffer, so this
	// bounds that memory at 64 MiB.
	maxInflightLimit = 16384
	maxTCPConnsLimit = 65536
	minTimeout       = 10 * time.Millisecond
	maxTimeout       = 10 * time.Second
	minIdleTimeout   = time.Second
	maxIdleTimeout   = 5 * time.Minute
	maxAnswerTTL     = 7 * 24 * 60 * 60
	maxQueryLogMB    = 65536
	minQueryLogAge   = time.Hour
	maxQueryLogAge   = 366 * 24 * time.Hour
	minObservability = time.Second
	maxObservability = 60 * time.Second
	// The job label is embedded in a PromQL matcher. Restricting it to
	// unquoted, printable characters keeps the query construction independent
	// of PromQL escaping rules.
	maxObservabilityJobLength = 128
)

// observabilityJobRE constrains observability.job: a Prometheus job name is
// written into the up{job="..."} matcher, so it stays unquoted and printable.
var observabilityJobRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.:-]*$`)

// qtypeCodes maps the query type mnemonics accepted by ParseQType to their type
// codes. Types without a dnsmessage constant are written as numbers.
var qtypeCodes = map[string]dnsmessage.Type{
	"A": dnsmessage.TypeA, "NS": dnsmessage.TypeNS, "CNAME": dnsmessage.TypeCNAME,
	"SOA": dnsmessage.TypeSOA, "PTR": dnsmessage.TypePTR, "MX": dnsmessage.TypeMX,
	"TXT": dnsmessage.TypeTXT, "AAAA": dnsmessage.TypeAAAA, "SRV": dnsmessage.TypeSRV,
	"NAPTR": 35, "DS": 43, "DNSKEY": 48, "SVCB": 64, "HTTPS": 65,
	"ANY": dnsmessage.TypeALL, "CAA": 257,
}

// ParseQType maps a mnemonic such as "AAAA" or a numeric code such as "65" to a
// query type. The reserved type 0 is rejected.
func ParseQType(s string) (dnsmessage.Type, error) {
	s = strings.ToUpper(strings.TrimSpace(s))
	if t, ok := qtypeCodes[s]; ok {
		return t, nil
	}
	var n uint16
	if _, err := fmt.Sscanf(s, "%d", &n); err == nil && fmt.Sprint(n) == s {
		if n == 0 {
			return 0, fmt.Errorf("query type 0 is reserved and cannot be matched")
		}
		return dnsmessage.Type(n), nil
	}
	return 0, fmt.Errorf("unknown query type %q", s)
}

// QTypeName is the inverse of ParseQType for display.
func QTypeName(t dnsmessage.Type) string {
	for k, v := range qtypeCodes {
		if v == t {
			return k
		}
	}
	return fmt.Sprint(uint16(t))
}

// rejectCodes maps the reject mnemonics accepted by the configuration to
// response codes. The nodata mnemonic maps to RCodeSuccess because a NODATA
// answer is an empty NOERROR response.
var rejectCodes = map[string]dnsmessage.RCode{
	"nxdomain": dnsmessage.RCodeNameError,
	"nodata":   dnsmessage.RCodeSuccess,
	"refused":  dnsmessage.RCodeRefused,
	"servfail": dnsmessage.RCodeServerFailure,
}

// nameRE constrains upstream, set, and rule names; these names are used in
// error reports and as metric labels.
var nameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)

// Load reads, applies defaults to, and validates a configuration file. Relative
// paths inside the file are resolved against the file's directory.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	return Parse(data, filepath.Dir(abs))
}

// Parse is Load operating on bytes. dir anchors relative paths.
func Parse(data []byte, dir string) (*Config, error) {
	var c Config
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	// The file must contain exactly one document. A second document would be
	// ignored without notice, for example after a merge or copy leaves two
	// configurations in the file.
	var extra yaml.Node
	switch err := dec.Decode(&extra); {
	case err == nil:
		return nil, errors.New("parse config: more than one YAML document; keep exactly one")
	case !errors.Is(err, io.EOF):
		return nil, fmt.Errorf("parse config: %w", err)
	}
	c.dir = dir
	c.defaults()
	if err := c.validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

func (c *Config) defaults() {
	if c.Log.Level == "" {
		c.Log.Level = "info"
	}
	if c.Log.Dir == "" {
		c.Log.Dir = DefaultLogDir
	}
	if c.Log.QueryLog.Enabled == nil {
		on := true
		c.Log.QueryLog.Enabled = &on
	}
	if c.Log.QueryLog.MaxSizeMB == 0 {
		c.Log.QueryLog.MaxSizeMB = DefaultQueryLogMaxSizeMB
	}
	if c.Log.QueryLog.MaxAge == 0 {
		c.Log.QueryLog.MaxAge = Duration(DefaultQueryLogMaxAge)
	}
	if c.Limits.MaxInflight == 0 {
		c.Limits.MaxInflight = DefaultMaxInflight
	}
	if c.Limits.MaxTCPConns == 0 {
		c.Limits.MaxTCPConns = DefaultMaxTCPConns
	}
	if c.Limits.TCPIdleTimeout == 0 {
		c.Limits.TCPIdleTimeout = Duration(DefaultTCPIdleTimeout)
	}
	if c.Limits.MaxSetShrink == 0 {
		c.Limits.MaxSetShrink = DefaultMaxSetShrink
	}
	c.Observability.PrometheusURL = strings.TrimSpace(c.Observability.PrometheusURL)
	c.Observability.DashboardURL = strings.TrimSpace(c.Observability.DashboardURL)
	if c.Observability.Job == "" {
		c.Observability.Job = DefaultObservabilityJob
	}
	if c.Observability.Timeout == 0 {
		c.Observability.Timeout = Duration(DefaultObservabilityTimeout)
	}
	for k, u := range c.Upstreams {
		if u.Timeout == 0 {
			u.Timeout = Duration(DefaultTimeout)
		}
		c.Upstreams[k] = u
	}
	for i := range c.Rules {
		if a := c.Rules[i].Answer; a != nil && a.TTL == nil {
			ttl := uint32(DefaultAnswerTTL)
			a.TTL = &ttl
		}
	}
}

// normalizeAddr canonicalizes an address so that equivalent spellings compare
// equal and the binding checks cannot be bypassed with an IPv4-mapped IPv6
// form: [::ffff:0.0.0.0] is the wildcard, [::ffff:127.0.0.1] is loopback, and
// netip's IsUnspecified/IsLoopback do not look through the mapping.
func normalizeAddr(ap netip.AddrPort) netip.AddrPort {
	return netip.AddrPortFrom(ap.Addr().Unmap(), ap.Port())
}

// parseBind parses a listen or admin address and enforces the binding
// invariant: the splitter binds loopback addresses only. Port 0 (kernel
// assigned, used by tests) is still loopback and is accepted.
func parseBind(s string) (netip.AddrPort, error) {
	ap, err := netip.ParseAddrPort(s)
	if err != nil {
		return ap, fmt.Errorf("%q: want ip:port with an IP literal", s)
	}
	ap = normalizeAddr(ap)
	if ap.Addr().IsUnspecified() {
		return ap, fmt.Errorf("%q: wildcard addresses are not allowed, bind a specific IP", s)
	}
	if !ap.Addr().IsLoopback() {
		return ap, fmt.Errorf("%q: only loopback addresses may be bound (the resolver must never be exposed)", s)
	}
	return ap, nil
}

// parseServer parses an upstream server address. A bare IP literal defaults to
// port 53; unspecified addresses and port 0 are rejected.
func parseServer(s string) (netip.AddrPort, error) {
	ap, err := netip.ParseAddrPort(s)
	if err != nil {
		a, err2 := netip.ParseAddr(s)
		if err2 != nil {
			return ap, fmt.Errorf("%q: upstreams must be IP literals (ip or ip:port); hostnames create a bootstrap loop", s)
		}
		ap = netip.AddrPortFrom(a, 53)
	}
	ap = normalizeAddr(ap)
	if ap.Addr().IsUnspecified() || ap.Port() == 0 {
		return ap, fmt.Errorf("%q: not a usable server address", s)
	}
	return ap, nil
}

func (c *Config) validate() error {
	var errs []error
	fail := func(format string, a ...any) { errs = append(errs, fmt.Errorf(format, a...)) }

	if len(c.Listen) == 0 {
		fail("listen: at least one address is required")
	}
	seen := map[netip.AddrPort]bool{}
	for _, s := range c.Listen {
		ap, err := parseBind(s)
		if err != nil {
			fail("listen: %v", err)
			continue
		}
		if seen[ap] {
			fail("listen: %s listed twice", ap)
		}
		seen[ap] = true
		c.listen = append(c.listen, ap)
	}
	if c.Admin == "" {
		fail("admin: address is required (metrics and health)")
	} else if ap, err := parseBind(c.Admin); err != nil {
		fail("admin: %v", err)
	} else if seen[ap] && ap.Port() != 0 { // port 0 = pick a free port (tests)
		fail("admin: %s is also a DNS listen address (both need TCP on it)", ap)
	} else {
		c.admin = ap
	}

	for i, h := range c.AdminHosts {
		n := strings.TrimSuffix(strings.ToLower(strings.TrimSpace(h)), ".")
		if n == "" || strings.ContainsAny(n, ":/[] \t") {
			fail("admin_hosts[%d]: %q must be a bare host name without port or scheme", i, h)
			continue
		}
		c.adminHosts = append(c.adminHosts, n)
	}

	switch c.Log.Level {
	case "debug", "info", "warn", "error":
	default:
		fail("log.level: %q is not one of debug, info, warn, error", c.Log.Level)
	}
	if strings.TrimSpace(c.Log.Dir) != c.Log.Dir {
		fail("log.dir: %q has leading or trailing spaces", c.Log.Dir)
	}
	if n := c.Log.QueryLog.MaxSizeMB; n < 1 || n > maxQueryLogMB {
		fail("log.query_log.max_size_mb: %d outside [1, %d]", n, maxQueryLogMB)
	}
	if d := time.Duration(c.Log.QueryLog.MaxAge); d < minQueryLogAge || d > maxQueryLogAge {
		fail("log.query_log.max_age: %s outside [%s, %s]", d, minQueryLogAge, maxQueryLogAge)
	}
	if c.Limits.MaxInflight < 1 || c.Limits.MaxInflight > maxInflightLimit {
		fail("limits.max_inflight: must be in [1, %d]", maxInflightLimit)
	}
	if c.Limits.MaxTCPConns < 1 || c.Limits.MaxTCPConns > maxTCPConnsLimit {
		fail("limits.max_tcp_conns: must be in [1, %d]", maxTCPConnsLimit)
	}
	if d := time.Duration(c.Limits.TCPIdleTimeout); d < minIdleTimeout || d > maxIdleTimeout {
		fail("limits.tcp_idle_timeout: %s outside [%s, %s]", d, minIdleTimeout, maxIdleTimeout)
	}

	if s := c.Limits.MaxSetShrink; !(s > 0 && s <= 1) {
		fail("limits.max_set_shrink: %v outside (0, 1] (1 disables the guard)", s)
	}

	if u := c.Observability.PrometheusURL; u != "" {
		if err := checkHTTPURL(u, false); err != nil {
			fail("observability.prometheus_url: %v", err)
		}
	} else if c.Observability.DashboardURL != "" {
		// The Grafana link is only meaningful when the page is enabled; an
		// orphan link is a configuration mistake, not a reason to refuse.
		fail("observability.dashboard_url: requires observability.prometheus_url (the page is disabled without it)")
	}
	if u := c.Observability.DashboardURL; u != "" {
		if err := checkHTTPURL(u, true); err != nil {
			fail("observability.dashboard_url: %v", err)
		}
	}
	if job := c.Observability.Job; !observabilityJobRE.MatchString(job) || len(job) > maxObservabilityJobLength {
		fail("observability.job: %q must be 1-%d characters from [A-Za-z0-9_.:-], starting with a letter or digit", job, maxObservabilityJobLength)
	}
	if d := time.Duration(c.Observability.Timeout); d < minObservability || d > maxObservability {
		fail("observability.timeout: %s outside [%s, %s]", d, minObservability, maxObservability)
	}

	// No upstream at all is valid for a configuration that only answers and
	// rejects; a forward rule that names a missing group is reported later.
	// Keys are processed in sorted order so that a given broken configuration
	// always produces the same report.
	for _, name := range sortedKeys(c.Upstreams) {
		u := c.Upstreams[name]
		if !nameRE.MatchString(name) {
			fail("upstreams.%s: name must match %s", name, nameRE)
		}
		if len(u.Servers) == 0 {
			fail("upstreams.%s: no servers", name)
		}
		seenServers := map[netip.AddrPort]bool{}
		for _, s := range u.Servers {
			ap, err := parseServer(s)
			if err != nil {
				fail("upstreams.%s: %v", name, err)
				continue
			}
			if seen[ap] {
				fail("upstreams.%s: %s is this splitter's own listen address (forwarding loop)", name, ap)
			}
			if seenServers[ap] {
				fail("upstreams.%s: %s is listed twice", name, ap)
			}
			seenServers[ap] = true
			u.addrs = append(u.addrs, ap)
		}
		if t := time.Duration(u.Timeout); t < minTimeout || t > maxTimeout {
			fail("upstreams.%s: timeout %s outside [%s, %s]", name, t, minTimeout, maxTimeout)
		}
		c.Upstreams[name] = u
	}

	for _, name := range sortedKeys(c.Sets) {
		s := c.Sets[name]
		if !nameRE.MatchString(name) {
			fail("sets.%s: name must match %s", name, nameRE)
		}
		if len(s.Files)+len(s.Rules)+len(s.Geosite) == 0 {
			fail("sets.%s: no files, rules or geosite sources", name)
		}
		// The same file twice in one set loads nothing new and would export two
		// freshness samples with identical labels, which fails the whole scrape.
		sources := map[string]string{}
		dup := func(where, f string) {
			p := c.Path(f)
			if first, ok := sources[p]; ok {
				fail("sets.%s.%s: %q is the same file as %s; list each file once per set (put all selectors of one geosite file in one entry)", name, where, f, first)
				return
			}
			sources[p] = where
		}
		for i, f := range s.Files {
			if err := checkListPath(f); err != nil {
				fail("sets.%s.files[%d]: %v", name, i, err)
				continue
			}
			dup(fmt.Sprintf("files[%d]", i), f)
		}
		for i := range s.Geosite {
			g := &s.Geosite[i]
			if g.File == "" || len(g.Selectors) == 0 {
				fail("sets.%s.geosite[%d]: file and selectors are required", name, i)
			} else if err := checkListPath(g.File); err != nil {
				fail("sets.%s.geosite[%d]: %v", name, i, err)
			} else {
				dup(fmt.Sprintf("geosite[%d]", i), g.File)
			}
			for _, sel := range g.Selectors {
				p, err := geosite.ParseSelector(sel)
				if err != nil {
					fail("sets.%s.geosite[%d]: %v", name, i, err)
					continue
				}
				g.parsed = append(g.parsed, p)
			}
		}
		c.Sets[name] = s
	}

	if len(c.Rules) == 0 {
		fail("rules: at least one rule is required")
	}
	ruleNames := map[string]bool{}
	usedSets, usedUpstreams := map[string]bool{}, map[string]bool{}
	for i := range c.Rules {
		r := &c.Rules[i]
		where := fmt.Sprintf("rules[%d]", i)
		if r.Name != "" {
			where = fmt.Sprintf("rules[%d] (%s)", i, r.Name)
		}
		if !nameRE.MatchString(r.Name) {
			fail("%s: name must match %s", where, nameRE)
		}
		if ruleNames[r.Name] {
			fail("%s: duplicate rule name", where)
		}
		ruleNames[r.Name] = true
		for _, s := range r.Sets {
			if _, ok := c.Sets[s]; !ok {
				fail("%s: unknown set %q", where, s)
			}
			usedSets[s] = true
		}
		for _, t := range r.QTypes {
			qt, err := ParseQType(t)
			if err != nil {
				fail("%s: %v", where, err)
				continue
			}
			r.qtypes = append(r.qtypes, qt)
		}
		actions := 0
		if r.Forward != "" {
			actions++
			if _, ok := c.Upstreams[r.Forward]; !ok {
				fail("%s: unknown upstream %q", where, r.Forward)
			}
			usedUpstreams[r.Forward] = true
		}
		if r.Answer != nil {
			actions++
			for _, s := range r.Answer.A {
				a, err := netip.ParseAddr(s)
				if err != nil || !a.Is4() {
					fail("%s: answer.a: %q is not an IPv4 address", where, s)
					continue
				}
				r.Answer.a = append(r.Answer.a, a)
			}
			for _, s := range r.Answer.AAAA {
				a, err := netip.ParseAddr(s)
				if err != nil || !a.Is6() || a.Is4In6() {
					fail("%s: answer.aaaa: %q is not an IPv6 address", where, s)
					continue
				}
				r.Answer.aaaa = append(r.Answer.aaaa, a)
			}
			if len(r.Answer.A)+len(r.Answer.AAAA) == 0 {
				fail("%s: answer needs at least one address (use reject: nodata to answer nothing)", where)
			}
			if *r.Answer.TTL > maxAnswerTTL {
				fail("%s: answer.ttl %d outside [0, %d]", where, *r.Answer.TTL, maxAnswerTTL)
			}
		}
		if r.Reject != "" {
			actions++
			if _, ok := rejectCodes[r.Reject]; !ok {
				fail("%s: reject %q is not one of nxdomain, nodata, refused, servfail", where, r.Reject)
			}
		}
		if actions != 1 {
			fail("%s: exactly one of forward, answer, reject is required", where)
		}
		catchAll := len(r.Sets) == 0 && len(r.QTypes) == 0
		last := i == len(c.Rules)-1
		if catchAll && !last {
			fail("%s: matches everything, so the rules after it are unreachable", where)
		}
		if !catchAll && last {
			fail("%s: the last rule must match everything (no sets, no qtypes)", where)
		}
	}
	// An unused set or upstream usually indicates a rule that was meant to
	// reference it (or a rule that still uses an old name), and an unused set
	// still costs a full list load on every reload.
	for _, name := range sortedKeys(c.Sets) {
		if !usedSets[name] {
			fail("sets.%s: not used by any rule", name)
		}
	}
	for _, name := range sortedKeys(c.Upstreams) {
		if !usedUpstreams[name] {
			fail("upstreams.%s: not used by any rule", name)
		}
	}
	return errors.Join(errs...)
}

// checkHTTPURL validates an HTTP(S) URL. Credentials are refused so that they
// cannot end up in logs or the console page. A query is only allowed on a link
// (the Grafana dashboard), never on the base URL the splitter calls.
func checkHTTPURL(raw string, allowQuery bool) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("%q is not a URL: %w", raw, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("%q: scheme must be http or https", raw)
	}
	if u.Host == "" {
		return fmt.Errorf("%q: a host is required", raw)
	}
	if u.User != nil {
		return fmt.Errorf("%q: credentials must not be embedded; publish the endpoint without auth or use a local address", raw)
	}
	if !allowQuery && (u.RawQuery != "" || u.Fragment != "") {
		return fmt.Errorf("%q: must not carry a query or fragment", raw)
	}
	return nil
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// checkListPath rejects a relative list path that escapes the configuration
// file's directory. Absolute paths remain allowed so that a caller can
// reference a shared list outside the deployment directory, but relative paths
// that traverse upward are rejected rather than silently resolved.
func checkListPath(p string) error {
	if filepath.IsAbs(p) {
		return nil
	}
	clean := filepath.Clean(p)
	if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return fmt.Errorf("%q: relative list paths must stay inside the config directory", p)
	}
	return nil
}

// Path resolves p against the configuration file's directory. Absolute paths
// are returned unchanged.
func (c *Config) Path(p string) string {
	if filepath.IsAbs(p) {
		return p
	}
	return filepath.Join(c.dir, p)
}

// LogDir returns the log directory, resolved against the configuration file's
// directory when relative.
func (c *Config) LogDir() string { return c.Path(c.Log.Dir) }

// QueryLogEnabled reports whether answered queries are persisted under LogDir.
func (c *Config) QueryLogEnabled() bool { return *c.Log.QueryLog.Enabled }

// QueryLogSettings is the part of the configuration that fixes the query log
// for the life of the process; comparing two values detects a change that
// needs a restart.
type QueryLogSettings struct {
	Enabled  bool
	Dir      string
	MaxBytes int64
	MaxAge   time.Duration
}

// QueryLogSettings returns the query log settings after defaults.
func (c *Config) QueryLogSettings() QueryLogSettings {
	return QueryLogSettings{
		Enabled:  c.QueryLogEnabled(),
		Dir:      c.LogDir(),
		MaxBytes: int64(c.Log.QueryLog.MaxSizeMB) << 20,
		MaxAge:   time.Duration(c.Log.QueryLog.MaxAge),
	}
}

// ListenAddrs returns the validated listen addresses.
func (c *Config) ListenAddrs() []netip.AddrPort { return c.listen }

// AdminAddr returns the validated admin address.
func (c *Config) AdminAddr() netip.AddrPort { return c.admin }

// AdminHostAllowed reports whether an HTTP Host header (with or without a
// port) names the admin endpoint: an empty Host (HTTP/1.0 tools), any IP
// literal, "localhost", or a name listed in admin_hosts. A DNS-rebinding page
// always sends its own host name, which none of these match.
func (c *Config) AdminHostAllowed(host string) bool {
	if host == "" {
		return true
	}
	h := host
	if hh, _, err := net.SplitHostPort(host); err == nil {
		h = hh
	}
	h = strings.TrimSuffix(strings.ToLower(strings.Trim(h, "[]")), ".")
	if _, err := netip.ParseAddr(h); err == nil {
		return true
	}
	return h == "localhost" || slices.Contains(c.adminHosts, h)
}

// adminLineRE matches a top-level "admin: ip:port" line, optionally quoted and
// followed by a comment.
var adminLineRE = regexp.MustCompile(`(?m)^admin:[ \t]*["']?([^"'\s#]+)["']?[ \t]*(?:#.*)?\r?$`)

// LoadAdminAddr reads only the admin address from the configuration file.
// The health check uses it so that an edited but not yet reloaded
// configuration elsewhere in the file cannot make a running process look
// unhealthy; everything else is taken from the running process.
func LoadAdminAddr(path string) (netip.AddrPort, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return netip.AddrPort{}, err
	}
	var c struct {
		Admin string `yaml:"admin"`
	}
	if err := yaml.Unmarshal(data, &c); err != nil {
		// A syntax error elsewhere in the file (an edit in progress) must not
		// hide the admin address: fall back to the top-level "admin:" line.
		m := adminLineRE.FindSubmatch(data)
		if m == nil {
			return netip.AddrPort{}, fmt.Errorf("parse config: %w", err)
		}
		c.Admin = string(m[1])
	}
	if c.Admin == "" {
		return netip.AddrPort{}, errors.New("admin: address is required")
	}
	ap, err := parseBind(c.Admin)
	if err != nil {
		return netip.AddrPort{}, fmt.Errorf("admin: %w", err)
	}
	return ap, nil
}

// ServerAddrs returns a group's validated server addresses.
func (u Upstream) ServerAddrs() []netip.AddrPort { return u.addrs }

// Parsed returns the validated selectors.
func (g Geosite) Parsed() []geosite.Selector { return g.parsed }

// QTypeCodes returns the validated query types.
func (r Rule) QTypeCodes() []dnsmessage.Type { return r.qtypes }

// RejectCode returns the response code for a reject rule.
func (r Rule) RejectCode() dnsmessage.RCode { return rejectCodes[r.Reject] }

// TTLSeconds returns the record TTL after defaults were applied.
func (a *Answer) TTLSeconds() uint32 {
	if a.TTL == nil {
		return DefaultAnswerTTL
	}
	return *a.TTL
}

// Addrs returns the validated IPv4 and IPv6 answer addresses.
func (a *Answer) Addrs() (v4, v6 []netip.Addr) { return a.a, a.aaaa }
