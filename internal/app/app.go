// Package app assembles the routing runtime from a configuration, answers
// queries with it, swaps runtimes atomically on reload, and serves the admin
// endpoints.
//
// Reload is all-or-nothing: the new configuration and every list are loaded
// and validated in isolation, and only a fully built runtime replaces the
// running one. A malformed list therefore never empties a set while the
// service is running; the attempt is counted in
// splitter_reloads_total{result="failure"} and the previous runtime keeps
// serving.
package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/netip"
	"os"
	"slices"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/net/dns/dnsmessage"

	"github.com/starriv/splitter/internal/config"
	"github.com/starriv/splitter/internal/domain"
	"github.com/starriv/splitter/internal/geosite"
	"github.com/starriv/splitter/internal/metrics"
	"github.com/starriv/splitter/internal/observability"
	"github.com/starriv/splitter/internal/querylog"
	"github.com/starriv/splitter/internal/router"
	"github.com/starriv/splitter/internal/upstream"
	"github.com/starriv/splitter/internal/webui"
	"github.com/starriv/splitter/internal/wire"
)

// Runtime is one immutable, fully loaded configuration: the parsed
// configuration, the router built from it, and the metric subjects published
// for it. Reload builds a new Runtime and swaps it in whole.
type Runtime struct {
	Config   *config.Config
	Router   *router.Router
	Sets     []metrics.SetEntries
	Sources  []metrics.Source
	LoadedAt time.Time
}

// Build loads every list and assembles a Runtime from cfg. Sets and rules are
// validated while building; if any validation fails, the joined error is
// returned and no runtime is exposed. obs may be nil, in which case upstream
// observations are not recorded.
func Build(cfg *config.Config, obs upstream.Observer) (*Runtime, error) {
	rt := &Runtime{Config: cfg, LoadedAt: time.Now()}

	groups := map[string]*upstream.Group{}
	for name, u := range cfg.Upstreams {
		groups[name] = upstream.NewGroup(name, u.ServerAddrs(), time.Duration(u.Timeout), obs)
	}

	setNames := make([]string, 0, len(cfg.Sets))
	for name := range cfg.Sets {
		setNames = append(setNames, name)
	}
	sort.Strings(setNames)
	type geoFile struct {
		data  []byte
		mtime time.Time // zero when the stat failed
	}
	geoCache := map[string]geoFile{}
	sets := map[string]*router.Set{}
	var errs []error
	for _, name := range setNames {
		s := cfg.Sets[name]
		b := domain.NewBuilder()
		// A (set, file) pair may be published only once: a second sample with
		// identical labels fails the entire /metrics scrape. The configuration
		// validator already rejects duplicates; this keeps Build safe for any other
		// caller.
		published := map[string]bool{}
		addSource := func(p string, mtime time.Time) {
			if mtime.IsZero() || published[p] {
				return
			}
			published[p] = true
			rt.Sources = append(rt.Sources, metrics.Source{Set: name, File: p, MTime: mtime})
		}
		for _, f := range s.Files {
			p := cfg.Path(f)
			// Stat before reading: the published mtime must not be newer than the
			// bytes that were loaded, because the list updater replaces files
			// atomically.
			mtime := statMTime(p)
			if _, err := domain.LoadFile(p, b); err != nil {
				errs = append(errs, fmt.Errorf("set %s: %w", name, err))
				continue
			}
			addSource(p, mtime)
		}
		for _, r := range s.Rules {
			if err := b.AddString(r); err != nil {
				errs = append(errs, fmt.Errorf("set %s: rule %q: %w", name, r, err))
			}
		}
		for _, g := range s.Geosite {
			p := cfg.Path(g.File)
			gf, ok := geoCache[p]
			if !ok {
				// Same order as for list files: stat first, then read.
				gf.mtime = statMTime(p)
				var err error
				if gf.data, err = os.ReadFile(p); err != nil {
					errs = append(errs, fmt.Errorf("set %s: %w", name, err))
					continue
				}
				geoCache[p] = gf
			}
			addSource(p, gf.mtime)
			if _, err := geosite.Load(gf.data, g.Parsed(), b.Add); err != nil {
				errs = append(errs, fmt.Errorf("set %s: %s: %w", name, p, err))
			}
		}
		m := b.Build()
		if m.Len() == 0 {
			errs = append(errs, fmt.Errorf("set %s: loaded zero rules; refusing to route with an empty set", name))
		}
		sets[name] = &router.Set{Name: name, Matcher: m}
		counts := m.Counts()
		for _, k := range domain.Kinds {
			rt.Sets = append(rt.Sets, metrics.SetEntries{Set: name, Kind: k.String(), Count: counts[k]})
		}
	}
	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}

	rules := make([]*router.Rule, 0, len(cfg.Rules))
	for _, cr := range cfg.Rules {
		r := &router.Rule{Name: cr.Name, QTypes: cr.QTypeCodes()}
		for _, s := range cr.Sets {
			set, ok := sets[s]
			if !ok {
				errs = append(errs, fmt.Errorf("rule %s: unknown set %q", cr.Name, s))
				continue
			}
			r.Sets = append(r.Sets, set)
		}
		switch {
		case cr.Forward != "":
			g, ok := groups[cr.Forward]
			if !ok {
				errs = append(errs, fmt.Errorf("rule %s: unknown upstream %q", cr.Name, cr.Forward))
			}
			r.Action, r.Forwarder = router.Forward, g
		case cr.Answer != nil:
			v4, v6 := cr.Answer.Addrs()
			r.Action, r.Answer = router.Answer, router.LocalAnswer{A: v4, AAAA: v6, TTL: cr.Answer.TTLSeconds()}
		case cr.Reject != "":
			r.Action, r.RCode = router.Reject, cr.RejectCode()
		default:
			errs = append(errs, fmt.Errorf("rule %s: no action (forward, answer or reject)", cr.Name))
		}
		rules = append(rules, r)
	}
	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	ro, err := router.New(rules)
	if err != nil {
		return nil, err
	}
	rt.Router = ro
	return rt, nil
}

func statMTime(p string) time.Time {
	st, err := os.Stat(p)
	if err != nil {
		return time.Time{}
	}
	return st.ModTime()
}

// shrinkGuardMinEntries is the set size below which the shrink guard does not
// apply. Small sets are edited by hand, and removing two of three inline rules
// is a deliberate edit rather than evidence of a truncated download.
const shrinkGuardMinEntries = 100

// checkShrink refuses a reload in which a set loses more than the configured
// share of its entries. A set with zero rules is already refused by Build;
// this guard covers a list that is non-empty but truncated, whether by a
// partial write, an interrupted download, or an upstream format change that
// drops most lines. Without the guard the reload would silently reroute every
// entry the list no longer contains.
func checkShrink(old, next *Runtime) error {
	limit := next.Config.Limits.MaxSetShrink
	if limit >= 1 {
		return nil
	}
	before, after := summary(old.Sets), summary(next.Sets)
	names := make([]string, 0, len(before))
	for name := range before {
		names = append(names, name)
	}
	sort.Strings(names)
	var errs []error
	for _, name := range names {
		was := before[name]
		now, kept := after[name]
		if !kept || was < shrinkGuardMinEntries || now >= was {
			continue
		}
		if lost := float64(was-now) / float64(was); lost > limit {
			errs = append(errs, fmt.Errorf("set %s: %d -> %d entries (lost %.0f%%, limits.max_set_shrink allows %.0f%%); "+
				"if this is intended, raise the limit for one reload or restart", name, was, now, lost*100, limit*100))
		}
	}
	return errors.Join(errs...)
}

// reloadStatus records the outcome of the most recent load attempt so that the
// admin endpoints can expose it, in addition to the metrics.
type reloadStatus struct {
	OK  bool
	Err string
	At  time.Time
}

// App serves queries from the current runtime. It owns the atomic runtime
// pointer, reload serialization, the reload status and the in-memory ring of
// recent queries.
type App struct {
	path     string
	log      *slog.Logger
	level    *slog.LevelVar
	m        *metrics.Metrics
	rt       atomic.Pointer[Runtime]
	queryLog atomic.Bool
	// obs reads the observability page's data from Prometheus. It is nil when
	// observability.prometheus_url is unset and is replaced on every reload.
	obs      atomic.Pointer[observability.Client]
	reloadMu sync.Mutex
	status   atomic.Pointer[reloadStatus]
	started  time.Time
	recent   recent // latest answered queries, for the control page

	// The query log is opened once at startup from the first configuration;
	// qlogSettings records what it was opened with so that a reload can report
	// a change that needs a restart. qlog is nil only when the log is disabled.
	qlog         *querylog.Log[recentEntry]
	qlogSettings config.QueryLogSettings

	boundMu     sync.Mutex
	boundListen []netip.AddrPort
	boundAdmin  netip.AddrPort
	boundKnown  bool
}

// New loads the configuration at path, opens the query log, and returns an
// App. level is adjusted on every load and reload. Call Close when done.
func New(path string, log *slog.Logger, level *slog.LevelVar, m *metrics.Metrics) (*App, error) {
	a := &App{path: path, log: log, level: level, m: m, started: time.Now()}
	if err := a.Reload(); err != nil {
		return nil, err
	}
	if err := a.openQueryLog(a.rt.Load().Config); err != nil {
		return nil, err
	}
	return a, nil
}

// openQueryLog opens the on-disk query log. When the log is enabled, a log
// directory that cannot be created or written stops startup: silently running
// without the history the operator expects is worse than not starting.
func (a *App) openQueryLog(cfg *config.Config) error {
	s := cfg.QueryLogSettings()
	a.qlogSettings = s
	if !s.Enabled {
		return nil
	}
	l, err := querylog.Open(s.Dir, (*recentEntry).record, querylog.Options{
		MaxBytes: s.MaxBytes, MaxAge: s.MaxAge, Observer: a.m,
	})
	if err != nil {
		return fmt.Errorf("log.dir %s is not writable (set log.dir to a writable directory, or log.query_log.enabled: false): %w", s.Dir, err)
	}
	a.qlog = l
	a.log.Info("query log", "dir", s.Dir, "max_bytes", s.MaxBytes, "max_age", s.MaxAge)
	return nil
}

// Close flushes and closes the query log. Call it after the DNS server has
// stopped, so that the last answered queries are written.
func (a *App) Close() error {
	if a.qlog == nil {
		return nil
	}
	return a.qlog.Close()
}

// Runtime returns the currently active runtime.
func (a *App) Runtime() *Runtime { return a.rt.Load() }

// SetBoundAddrs records the addresses the process bound. Reload compares the
// configuration against these addresses instead of against the previous
// configuration, so the restart warning persists for as long as the mismatch
// exists.
func (a *App) SetBoundAddrs(listen []netip.AddrPort, admin netip.AddrPort) {
	a.boundMu.Lock()
	defer a.boundMu.Unlock()
	a.boundListen = slices.Clone(listen)
	a.boundAdmin = admin
	a.boundKnown = true
}

func (a *App) boundAddrs() (listen []netip.AddrPort, admin netip.AddrPort, known bool) {
	a.boundMu.Lock()
	defer a.boundMu.Unlock()
	return a.boundListen, a.boundAdmin, a.boundKnown
}

// Reload re-reads the configuration and all lists, then swaps the runtime
// atomically. Listen and admin addresses change only on restart.
func (a *App) Reload() error {
	a.reloadMu.Lock()
	defer a.reloadMu.Unlock()
	cfg, err := config.Load(a.path)
	var rt *Runtime
	if err == nil {
		rt, err = Build(cfg, a.m)
	}
	old := a.rt.Load()
	if err == nil && old != nil {
		err = checkShrink(old, rt)
	}
	if err != nil {
		a.m.LoadFailed()
		a.status.Store(&reloadStatus{OK: false, Err: err.Error(), At: time.Now()})
		return err
	}
	if old != nil {
		if listen, admin, known := a.boundAddrs(); known {
			if !slices.Equal(listen, cfg.ListenAddrs()) || admin != cfg.AdminAddr() {
				a.log.Warn("listen/admin address changes need a restart; the bound sockets differ from the config",
					"bound", listen, "admin_bound", admin, "config", cfg.ListenAddrs(), "admin_config", cfg.AdminAddr())
			}
		}
		// The server is built from the startup limits and is never rebuilt, so a
		// reloaded limits.* block would otherwise be ignored silently. The mismatch
		// is reported instead.
		if o, n := old.Config.Limits, cfg.Limits; o.MaxInflight != n.MaxInflight ||
			o.MaxTCPConns != n.MaxTCPConns || o.TCPIdleTimeout != n.TCPIdleTimeout {
			a.log.Warn("limits changes need a restart; the running semaphore and idle timeout stay as they were",
				"running", old.Config.Limits, "config", cfg.Limits)
		}
		if s := cfg.QueryLogSettings(); s != a.qlogSettings {
			a.log.Warn("log.dir and log.query_log changes need a restart; the query log keeps its startup settings",
				"running", a.qlogSettings, "config", s)
		}
	}
	a.level.Set(parseLevel(cfg.Log.Level))
	a.queryLog.Store(cfg.Log.Queries)
	a.rt.Store(rt)
	a.obs.Store(observability.New(observability.Settings{
		URL:          cfg.Observability.PrometheusURL,
		Job:          cfg.Observability.Job,
		DashboardURL: cfg.Observability.DashboardURL,
		Timeout:      time.Duration(cfg.Observability.Timeout),
	}))
	a.status.Store(&reloadStatus{OK: true, At: time.Now()})
	for name, u := range cfg.Upstreams {
		labels := make([]string, 0, len(u.ServerAddrs()))
		for _, s := range u.ServerAddrs() {
			labels = append(labels, s.String())
		}
		a.m.InitUpstream(name, labels)
	}
	a.m.Loaded(rt.Sets, rt.Sources)
	a.log.Info("config loaded", "rules", len(rt.Router.Rules()), "sets", summary(rt.Sets))
	return nil
}

func parseLevel(s string) slog.Level {
	var l slog.Level
	_ = l.UnmarshalText([]byte(s))
	return l
}

func summary(sets []metrics.SetEntries) map[string]int {
	out := map[string]int{}
	for _, s := range sets {
		out[s.Set] += s.Count
	}
	return out
}

// ServeDNS implements server.Handler. It routes one raw query and returns the
// raw reply, or nil when no reply may be sent.
func (a *App) ServeDNS(ctx context.Context, req []byte, proto wire.Proto) []byte {
	start := time.Now()
	q, err := wire.ParseQuery(req)
	q.Proto = proto
	switch {
	case errors.Is(err, wire.ErrNoHeader):
		a.m.Malformed()
		return nil
	case q.Header.Response:
		// A response is never answered, not even a malformed one with FORMERR: two
		// servers that did so would exchange packets indefinitely.
		a.m.Malformed()
		return nil
	case q.Header.OpCode != 0:
		// Checked before the format test: other opcodes use a different message
		// layout (STATUS carries no question, UPDATE carries a zone section), so
		// FORMATERROR is not a correct diagnosis for them.
		return a.local(q, invalidRule, dnsmessage.RCodeNotImplemented, nil, start)
	case err != nil:
		a.m.Malformed()
		return a.local(q, invalidRule, dnsmessage.RCodeFormatError, nil, start)
	case q.EDNS && q.EDNSVersion != 0:
		// Only EDNS0 is supported (RFC 6891 §6.1.3), regardless of which rule
		// would match.
		return a.local(q, invalidRule, wire.RCodeBadVers, nil, start)
	case q.Question.Class != dnsmessage.ClassINET:
		return a.local(q, invalidRule, dnsmessage.RCodeRefused, nil, start)
	}

	d := a.rt.Load().Router.Route(q.Name, q.Question.Type)
	rule := d.Rule
	var resp []byte
	switch rule.Action {
	case router.Forward:
		resp, err = rule.Forwarder.Exchange(ctx, req, q.Question, proto)
		if err != nil {
			a.log.Debug("upstream failed", "name", q.Name, "upstream", rule.Target(), "err", err)
			resp, _ = wire.Reply(q, dnsmessage.RCodeServerFailure, false, nil)
		}
	case router.Answer:
		resp, _ = wire.Reply(q, dnsmessage.RCodeSuccess, true, localRecords(q, rule.Answer))
	case router.Reject:
		resp, _ = wire.Reply(q, rule.RCode, false, nil)
	}
	if resp == nil {
		// No reply is sent, so nothing is counted as a response.
		return nil
	}
	rc := wire.EffectiveRCode(resp)
	dur := time.Since(start)
	a.m.ObserveQuery(rule.Name, rule.Action.String(), rule.Target(), wire.RCodeLabel(rc), dur)
	e := recentEntry{at: start, name: q.Name, qtype: q.Question.Type, class: q.Question.Class, proto: proto, rule: rule.Name,
		set: d.Set, kind: d.Kind, target: rule.Target(), rcode: rc, tc: wire.Truncated(resp), dur: dur}
	a.recent.add(e)
	if a.qlog != nil {
		a.qlog.Append(e)
	}
	if a.queryLog.Load() {
		a.log.Info("query", "name", q.Name, "type", config.QTypeName(q.Question.Type), "proto", proto.String(),
			"rule", rule.Name, "set", d.Set, "match", d.Kind.String(), "target", rule.Target(),
			"rcode", wire.RCodeName(rc), "tc", wire.Truncated(resp), "ms", float64(dur.Microseconds())/1000)
	}
	return resp
}

func (a *App) local(q wire.Query, rule string, rc dnsmessage.RCode, ans []dnsmessage.Resource, start time.Time) []byte {
	resp, err := wire.Reply(q, rc, false, ans)
	if err != nil {
		return nil
	}
	// The target label records where a query is sent; locally answered queries
	// never leave the process. The RCODE is recorded in the rcode label, so the
	// target label keeps a single meaning for every rule.
	dur := time.Since(start)
	a.m.ObserveQuery(rule, "reject", "local", wire.RCodeLabel(int(rc)), dur)
	if isHealthcheckProbe(q) {
		return resp
	}
	e := recentEntry{at: start, name: q.Name, proto: q.Proto, rule: rule, target: "local", rcode: int(rc), dur: dur}
	if q.HasQuestion {
		e.qtype, e.class = q.Question.Type, q.Question.Class
	}
	a.recent.add(e)
	if a.qlog != nil {
		a.qlog.Append(e)
	}
	return resp
}

// HealthcheckName is the name of the query that splitter -healthcheck sends
// in class CH. It is answered locally with REFUSED like any other non-IN
// query, but it is left out of the query history: Docker sends one every 30
// seconds, and the history is for client queries.
const HealthcheckName = "healthcheck.splitter.invalid"

func isHealthcheckProbe(q wire.Query) bool {
	return q.HasQuestion && q.Question.Class == dnsmessage.ClassCHAOS && q.Name == HealthcheckName
}

func localRecords(q wire.Query, ans router.LocalAnswer) []dnsmessage.Resource {
	hdr := dnsmessage.ResourceHeader{Name: q.Question.Name, Class: dnsmessage.ClassINET, TTL: ans.TTL}
	var out []dnsmessage.Resource
	switch q.Question.Type {
	case dnsmessage.TypeA:
		for _, a := range ans.A {
			out = append(out, dnsmessage.Resource{Header: hdr, Body: &dnsmessage.AResource{A: a.As4()}})
		}
	case dnsmessage.TypeAAAA:
		for _, a := range ans.AAAA {
			out = append(out, dnsmessage.Resource{Header: hdr, Body: &dnsmessage.AAAAResource{AAAA: a.As16()}})
		}
	}
	return out
}

// RouteInfo is the response of /api/v1/route and its legacy alias /debug/route.
type RouteInfo struct {
	Name   string `json:"name"`
	Type   string `json:"type"`
	Rule   string `json:"rule"`
	Action string `json:"action"`
	Target string `json:"target"`
	Set    string `json:"set,omitempty"`
	Match  string `json:"match,omitempty"`
}

// Explain reports how a name would be routed, without sending a query.
func (a *App) Explain(name string, qtype dnsmessage.Type) RouteInfo {
	n := domain.Normalize(name)
	d := a.rt.Load().Router.Route(n, qtype)
	ri := RouteInfo{Name: n, Type: config.QTypeName(qtype), Rule: d.Rule.Name,
		Action: d.Rule.Action.String(), Target: d.Rule.Target(), Set: d.Set}
	if d.Set != "" {
		ri.Match = d.Kind.String()
	}
	return ri
}

// health is the shared response body of /healthz and /readyz. "listen" holds
// the DNS addresses bound by the process; the -healthcheck probe uses them
// instead of the configuration file, which may have been edited since.
func (a *App) health() map[string]any {
	rt := a.rt.Load()
	lists := map[string]string{}
	for _, s := range rt.Sources {
		lists[s.File] = s.MTime.UTC().Format(time.RFC3339)
	}
	listen, _, known := a.boundAddrs()
	if !known {
		listen = rt.Config.ListenAddrs()
	}
	bound := make([]string, 0, len(listen))
	for _, ap := range listen {
		bound = append(bound, ap.String())
	}
	out := map[string]any{
		"status":         "ok",
		"listen":         bound,
		"loaded_at":      rt.LoadedAt.UTC().Format(time.RFC3339),
		"sets":           summary(rt.Sets),
		"list_modified":  lists,
		"last_reload_ok": true,
	}
	if st := a.status.Load(); st != nil {
		out["last_reload_ok"] = st.OK
		out["last_reload_at"] = st.At.UTC().Format(time.RFC3339)
		if !st.OK {
			// The previous runtime is still serving, so liveness is unaffected, but
			// the newest configuration was refused: readiness and /healthz both report
			// the failure.
			out["status"] = "reload_failed"
			out["last_reload_error"] = st.Err
		}
	}
	return out
}

// AdminHandler serves /metrics, /healthz, /readyz and /debug/route, the JSON API
// under /api/v1/ (see api.go), and the control page at / (see internal/webui).
//
// Every request must carry an acceptable Host header (see
// config.Config.AdminHostAllowed). The admin port is loopback-only, but a page
// that rebinds its own name to 127.0.0.1 would otherwise be same-origin with
// the API in the browser and could read the recent queries or call the write
// endpoints. The allowed names follow the running configuration, so a reload
// that adds admin_hosts takes effect at once.
func (a *App) AdminHandler() http.Handler {
	mux := a.adminMux()
	return gzipHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !a.rt.Load().Config.AdminHostAllowed(r.Host) {
			writeError(w, http.StatusMisdirectedRequest,
				fmt.Sprintf("host %q is not an admin address; add it to admin_hosts if a reverse proxy publishes the admin endpoint", r.Host))
			return
		}
		mux.ServeHTTP(w, r)
	}))
}

func (a *App) adminMux() *http.ServeMux {
	mux := http.NewServeMux()
	mux.Handle("GET /metrics", a.m.Handler())
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, a.health())
	})
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		code := http.StatusOK
		if st := a.status.Load(); st != nil && !st.OK {
			code = http.StatusServiceUnavailable
		}
		writeJSON(w, code, a.health())
	})
	mux.HandleFunc("GET /debug/route", a.handleRoute)
	a.registerAPI(mux)
	webui.Register(mux)
	return mux
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
