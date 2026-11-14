package app

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"mime"
	"net/http"
	"net/netip"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"golang.org/x/net/dns/dnsmessage"

	"github.com/starriv/splitter/internal/config"
	"github.com/starriv/splitter/internal/domain"
	"github.com/starriv/splitter/internal/metrics"
	"github.com/starriv/splitter/internal/observability"
	"github.com/starriv/splitter/internal/querylog"
	"github.com/starriv/splitter/internal/router"
	"github.com/starriv/splitter/internal/wire"
)

// This file implements the JSON API used by the control page (internal/webui).
// Every value is read from the running process: the active runtime, the same
// collectors that /metrics exports, and the query log (on disk under log.dir,
// or the in-memory ring when log.query_log.enabled is false).
// The API cannot edit the configuration; the repository remains the only place
// where rules are written. Its two write actions are "re-read what is on disk"
// and "send one query".
//
//	GET  /api/v1/state                     status, rules, upstreams and sets with their counters
//	GET  /api/v1/queries?limit=&q=&rule=&cursor=
//	                                       answered queries, newest first; "next" pages
//	                                       further back when the query log is on disk
//	GET  /api/v1/route?name=&type=         how a name would be routed (nothing is sent)
//	POST /api/v1/resolve {name,type}       route and resolve one query
//	POST /api/v1/reload                    the SIGHUP path: reload configuration and lists
//	GET  /api/v1/observability?range=6h    splitter panels read back from Prometheus

// State is the GET /api/v1/state response: status, rules, upstreams and sets
// from a single runtime snapshot.
type State struct {
	Status    Status          `json:"status"`
	Rules     []RuleState     `json:"rules"`
	Upstreams []UpstreamState `json:"upstreams"`
	Sets      []SetState      `json:"sets"`
}

// Status is the status section of the state response.
type Status struct {
	Version       string            `json:"version"`
	Commit        string            `json:"commit"`
	Now           time.Time         `json:"now"`
	StartedAt     time.Time         `json:"started_at"`
	LoadedAt      time.Time         `json:"loaded_at"`
	Ready         bool              `json:"ready"` // false: the newest config was refused, the previous one is serving
	LastReload    *ReloadState      `json:"last_reload,omitempty"`
	Listen        []string          `json:"listen"`
	Admin         string            `json:"admin"`
	LogLevel      string            `json:"log_level"`
	LogQueries    bool              `json:"log_queries"`
	QueryLog      QueryLogState     `json:"query_log"`
	MaxInflight   int               `json:"max_inflight"`
	MaxTCPConns   int               `json:"max_tcp_conns"`
	Inflight      int               `json:"inflight"`
	TCPConns      int               `json:"tcp_connections"`
	Queries       uint64            `json:"queries"`          // answered by a rule
	Refused       uint64            `json:"refused_locally"`  // non-IN class, bad EDNS version, other opcodes, FORMERR
	Malformed     uint64            `json:"malformed"`        // could not be parsed
	Overload      uint64            `json:"overload_dropped"` // dropped at max_inflight / max_tcp_conns
	WriteErrors   uint64            `json:"write_errors"`
	Panics        uint64            `json:"panics"`
	ReloadsOK     uint64            `json:"reloads_ok"`
	ReloadsFailed uint64            `json:"reloads_failed"`
	RCodes        map[string]uint64 `json:"rcodes"`
}

// QueryLogState describes the on-disk query log. Settings are those the log
// was opened with at startup; a reloaded configuration does not change them.
type QueryLogState struct {
	Enabled       bool       `json:"enabled"` // log.query_log.enabled
	Active        bool       `json:"active"`  // queries are being written to Dir
	Dir           string     `json:"dir"`     // resolved log.dir
	Files         int        `json:"files"`
	SizeBytes     int64      `json:"size_bytes"`
	MaxSizeBytes  int64      `json:"max_size_bytes"`
	MaxAgeSeconds float64    `json:"max_age_seconds"`
	Oldest        *time.Time `json:"oldest,omitempty"` // first stored record
	Dropped       uint64     `json:"dropped"`          // not written: queue full
	WriteErrors   uint64     `json:"write_errors"`
	LastError     string     `json:"last_error,omitempty"` // most recent write error
}

// queryLogState reports the query log for the state response.
func (a *App) queryLogState() QueryLogState {
	s := a.qlogSettings
	out := QueryLogState{Enabled: s.Enabled, Active: a.qlog != nil, Dir: s.Dir,
		MaxSizeBytes: s.MaxBytes, MaxAgeSeconds: s.MaxAge.Seconds()}
	if a.qlog == nil {
		return out
	}
	st := a.qlog.Stats()
	out.Files, out.SizeBytes = st.Files, st.Bytes
	out.Dropped, out.WriteErrors, out.LastError = st.Dropped, st.Errors, st.LastError
	if !st.Oldest.IsZero() {
		t := st.Oldest.UTC()
		out.Oldest = &t
	}
	return out
}

// QueryPage is the GET /api/v1/queries response. Source is "disk" when the
// records come from the query log and "memory" when they come from the ring.
// Next is the cursor of the following (older) page, empty on the last page;
// Capacity is set only for memory.
type QueryPage struct {
	Queries  []QueryRecord `json:"queries"`
	Total    uint64        `json:"total"` // queries recorded since start
	Source   string        `json:"source"`
	Next     string        `json:"next,omitempty"`
	Capacity int           `json:"capacity,omitempty"`
}

// queries answers GET /api/v1/queries. substr is a normalized name fragment
// and rule an exact rule name; either may be empty.
func (a *App) queries(cursor string, limit int, substr, rule string) (QueryPage, error) {
	if a.qlog == nil {
		before, err := parseMemoryCursor(cursor)
		if err != nil {
			return QueryPage{}, err
		}
		list, more, total := a.recent.page(before, limit, substr, rule)
		page := QueryPage{Queries: list, Total: total, Source: "memory", Capacity: recentCapacity}
		if more && len(list) > 0 {
			page.Next = memoryCursorPrefix + strconv.FormatUint(list[len(list)-1].Seq, 10)
		}
		return page, nil
	}
	var f querylog.Filter
	if rule != "" {
		f.Contains = append(f.Contains, []byte(`"rule":`+strconv.Quote(rule)))
	}
	if substr != "" {
		if jsonPlain(substr) {
			f.Contains = append(f.Contains, []byte(substr))
		}
		f.Keep = func(r *querylog.Record) bool { return strings.Contains(r.Name, substr) }
	}
	page, err := a.qlog.Read(cursor, limit, f)
	if err != nil {
		return QueryPage{}, err
	}
	_, total := a.recent.list(0, "", "")
	return QueryPage{Queries: page.Records, Total: total, Source: "disk", Next: page.Next}, nil
}

// memoryCursorPrefix marks a cursor into the in-memory ring: "m<seq>" pages
// to the entries older than seq. The on-disk log's cursors have the form
// "<segment>@<offset>", so neither source accepts the other's cursor.
const memoryCursorPrefix = "m"

func parseMemoryCursor(cursor string) (uint64, error) {
	if cursor == "" {
		return 0, nil
	}
	seq, err := strconv.ParseUint(strings.TrimPrefix(cursor, memoryCursorPrefix), 10, 64)
	if !strings.HasPrefix(cursor, memoryCursorPrefix) || err != nil || seq == 0 {
		return 0, errors.New("invalid cursor")
	}
	return seq, nil
}

// jsonPlain reports whether s is encoded verbatim inside a JSON string, so a
// raw-byte search finds it.
func jsonPlain(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c < 0x20 || c >= 0x7f || c == '"' || c == '\\' || c == '<' || c == '>' || c == '&' {
			return false
		}
	}
	return true
}

// ReloadState describes the outcome of the most recent reload.
type ReloadState struct {
	OK    bool      `json:"ok"`
	At    time.Time `json:"at"`
	Error string    `json:"error,omitempty"`
}

// RuleState is one routing rule with its own counters.
type RuleState struct {
	Name            string       `json:"name"`
	Sets            []string     `json:"sets"`
	QTypes          []string     `json:"qtypes"`
	Action          string       `json:"action"`
	Target          string       `json:"target"`
	Answer          *AnswerState `json:"answer,omitempty"`
	Queries         uint64       `json:"queries"`
	DurationSeconds float64      `json:"duration_seconds"` // sum over Queries
}

// AnswerState is the address set of an answer rule.
type AnswerState struct {
	A    []string `json:"a"`
	AAAA []string `json:"aaaa"`
	TTL  uint32   `json:"ttl"`
}

// UpstreamState is one upstream group with its servers.
type UpstreamState struct {
	Name           string        `json:"name"`
	TimeoutSeconds float64       `json:"timeout_seconds"`
	Servers        []ServerState `json:"servers"`
}

// ServerState is one server of an upstream group.
type ServerState struct {
	Addr            string            `json:"addr"`
	Requests        uint64            `json:"requests"`
	Errors          map[string]uint64 `json:"errors"`
	Dropped         uint64            `json:"dropped"`
	Retransmits     uint64            `json:"retransmits"`
	Succeeded       uint64            `json:"succeeded"`
	DurationSeconds float64           `json:"duration_seconds"` // sum over Succeeded
	Penalized       bool              `json:"penalized"`        // moved behind the healthy servers after consecutive failures
}

// SetState is one domain set with its entry counts and sources.
type SetState struct {
	Name    string         `json:"name"`
	Total   int            `json:"total"`
	Entries map[string]int `json:"entries"` // by match kind, zero kinds left out
	Inline  int            `json:"inline_rules"`
	Sources []SourceState  `json:"sources"`
	UsedBy  []string       `json:"used_by"`
}

// SourceState is one file that contributes entries to a set.
type SourceState struct {
	File      string    `json:"file"`
	Path      string    `json:"path"`
	Modified  time.Time `json:"modified"`
	Selectors []string  `json:"selectors,omitempty"` // geosite sources only
}

// State assembles the snapshot from a single runtime, so rules, sets and
// upstreams always belong to the same configuration even while a reload swaps
// the runtime.
func (a *App) State() State {
	rt := a.rt.Load()
	cfg := rt.Config
	st := a.m.Stats()
	version, commit := a.m.BuildInfo()

	status := Status{
		Version: version, Commit: commit,
		Now: time.Now().UTC(), StartedAt: a.started.UTC(), LoadedAt: rt.LoadedAt.UTC(),
		Ready:    true,
		LogLevel: cfg.Log.Level, LogQueries: cfg.Log.Queries, QueryLog: a.queryLogState(),
		MaxInflight: cfg.Limits.MaxInflight, MaxTCPConns: cfg.Limits.MaxTCPConns,
		Inflight: int(st.Inflight), TCPConns: int(st.TCPConns),
		Refused: st.Rules[invalidRule].Count, Malformed: st.Malformed,
		Overload: st.Overload, WriteErrors: st.WriteErrors, Panics: st.Panics,
		ReloadsOK: st.Reloads["success"], ReloadsFailed: st.Reloads["failure"],
		RCodes: st.RCodes,
	}
	// Report the bound sockets instead of the configuration: a changed listen
	// address takes effect only after a restart, and the page must show where
	// queries arrive.
	listen, admin, known := a.boundAddrs()
	if !known {
		listen, admin = cfg.ListenAddrs(), cfg.AdminAddr()
	}
	status.Admin = admin.String()
	for _, ap := range listen {
		status.Listen = append(status.Listen, ap.String())
	}
	if rs := a.status.Load(); rs != nil {
		status.Ready = rs.OK
		status.LastReload = &ReloadState{OK: rs.OK, At: rs.At.UTC(), Error: rs.Err}
	}

	out := State{Rules: []RuleState{}, Upstreams: []UpstreamState{}, Sets: []SetState{}}
	usedBy := map[string][]string{}
	for i, r := range rt.Router.Rules() {
		cr := cfg.Rules[i]
		rs := RuleState{
			Name: r.Name, Sets: append([]string{}, cr.Sets...), QTypes: []string{},
			Action: r.Action.String(), Target: r.Target(),
			Queries: st.Rules[r.Name].Count, DurationSeconds: st.Rules[r.Name].SumSeconds,
		}
		for _, t := range r.QTypes {
			rs.QTypes = append(rs.QTypes, config.QTypeName(t))
		}
		if r.Action == router.Answer {
			rs.Answer = &AnswerState{A: addrStrings(r.Answer.A), AAAA: addrStrings(r.Answer.AAAA), TTL: r.Answer.TTL}
		}
		for _, s := range cr.Sets {
			usedBy[s] = append(usedBy[s], r.Name)
		}
		status.Queries += rs.Queries
		out.Rules = append(out.Rules, rs)
	}
	out.Status = status

	for _, name := range sortedNames(cfg.Upstreams) {
		u := cfg.Upstreams[name]
		us := UpstreamState{Name: name, TimeoutSeconds: time.Duration(u.Timeout).Seconds(), Servers: []ServerState{}}
		for _, ap := range u.ServerAddrs() {
			s := st.Servers[metrics.ServerKey{Upstream: name, Server: ap.String()}]
			if s.Errors == nil {
				s.Errors = map[string]uint64{}
			}
			us.Servers = append(us.Servers, ServerState{
				Addr: ap.String(), Requests: s.Requests, Errors: s.Errors, Dropped: s.Dropped,
				Retransmits: s.Retransmits, Succeeded: s.OK.Count, DurationSeconds: s.OK.SumSeconds,
				Penalized: s.Penalized,
			})
		}
		out.Upstreams = append(out.Upstreams, us)
	}

	mtimes := map[[2]string]time.Time{}
	for _, src := range rt.Sources {
		mtimes[[2]string{src.Set, src.File}] = src.MTime
	}
	for _, name := range sortedNames(cfg.Sets) {
		cs := cfg.Sets[name]
		ss := SetState{Name: name, Entries: map[string]int{}, Inline: len(cs.Rules), Sources: []SourceState{}, UsedBy: usedBy[name]}
		for _, e := range rt.Sets {
			if e.Set == name && e.Count > 0 {
				ss.Entries[e.Kind] = e.Count
				ss.Total += e.Count
			}
		}
		source := func(file string, selectors []string) {
			p := cfg.Path(file)
			ss.Sources = append(ss.Sources, SourceState{
				File: filepath.Base(p), Path: p, Modified: mtimes[[2]string{name, p}].UTC(), Selectors: selectors,
			})
		}
		for _, f := range cs.Files {
			source(f, nil)
		}
		for _, g := range cs.Geosite {
			source(g.File, g.Selectors)
		}
		out.Sets = append(out.Sets, ss)
	}
	return out
}

func addrStrings(addrs []netip.Addr) []string {
	out := make([]string, 0, len(addrs))
	for _, a := range addrs {
		out = append(out, a.String())
	}
	return out
}

func sortedNames[V any](m map[string]V) []string {
	names := make([]string, 0, len(m))
	for name := range m {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// ResolveResult is the POST /api/v1/resolve response: the routing decision,
// the outcome and the decoded answer section.
type ResolveResult struct {
	Route      RouteInfo      `json:"route"`
	RCode      string         `json:"rcode"`
	Truncated  bool           `json:"truncated"`
	DurationMS float64        `json:"duration_ms"`
	Answers    []AnswerRecord `json:"answers"`
}

// AnswerRecord is one answer record rendered for display.
type AnswerRecord struct {
	Name string `json:"name"`
	Type string `json:"type"`
	TTL  uint32 `json:"ttl"`
	Data string `json:"data"`
}

// errBadName marks a name that cannot be encoded as a DNS query; callers map
// it to a 400 response.
var errBadName = errors.New("not a valid domain name")

// Resolve sends one query through the same pipeline a client query follows
// (routing rules, upstream exchange, metrics, recent ring) and decodes the
// reply. The query is issued over UDP with EDNS, matching the behaviour of a
// front-end caching resolver.
func (a *App) Resolve(ctx context.Context, name string, qtype dnsmessage.Type) (ResolveResult, error) {
	n := domain.Normalize(name)
	fq, err := dnsmessage.NewName(n + ".")
	if err != nil || n == "" {
		return ResolveResult{}, errBadName
	}
	var id [2]byte
	_, _ = rand.Read(id[:])
	m := dnsmessage.Message{
		Header:    dnsmessage.Header{ID: binary.BigEndian.Uint16(id[:]), RecursionDesired: true},
		Questions: []dnsmessage.Question{{Name: fq, Type: qtype, Class: dnsmessage.ClassINET}},
	}
	var opt dnsmessage.ResourceHeader
	if err := opt.SetEDNS0(wire.EDNSPayload, dnsmessage.RCodeSuccess, false); err != nil {
		return ResolveResult{}, err
	}
	m.Additionals = []dnsmessage.Resource{{Header: opt, Body: &dnsmessage.OPTResource{}}}
	req, err := m.Pack()
	if err != nil {
		return ResolveResult{}, errBadName // a label over 63 bytes fails here
	}

	res := ResolveResult{Route: a.Explain(n, qtype), Answers: []AnswerRecord{}}
	start := time.Now()
	resp := a.ServeDNS(ctx, req, wire.UDP)
	res.DurationMS = float64(time.Since(start).Microseconds()) / 1000
	if resp == nil {
		return res, errors.New("the splitter produced no response")
	}
	res.RCode = wire.RCodeName(wire.EffectiveRCode(resp))
	res.Truncated = wire.Truncated(resp)
	res.Answers = decodeAnswers(resp)
	return res, nil
}

// decodeAnswers renders the answer section for display. Record types that are
// not decoded explicitly are listed with their size and are never dropped.
func decodeAnswers(resp []byte) []AnswerRecord {
	out := []AnswerRecord{}
	var p dnsmessage.Parser
	if _, err := p.Start(resp); err != nil {
		return out
	}
	if err := p.SkipAllQuestions(); err != nil {
		return out
	}
	for {
		h, err := p.AnswerHeader()
		if err != nil {
			return out
		}
		rec := AnswerRecord{Name: domain.Normalize(h.Name.String()), Type: config.QTypeName(h.Type), TTL: h.TTL}
		switch h.Type {
		case dnsmessage.TypeA:
			r, err := p.AResource()
			if err != nil {
				return out
			}
			rec.Data = netip.AddrFrom4(r.A).String()
		case dnsmessage.TypeAAAA:
			r, err := p.AAAAResource()
			if err != nil {
				return out
			}
			rec.Data = netip.AddrFrom16(r.AAAA).String()
		case dnsmessage.TypeCNAME:
			r, err := p.CNAMEResource()
			if err != nil {
				return out
			}
			rec.Data = domain.Normalize(r.CNAME.String())
		case dnsmessage.TypeTXT:
			r, err := p.TXTResource()
			if err != nil {
				return out
			}
			rec.Data = strings.Join(r.TXT, " ")
		default:
			if err := p.SkipAnswer(); err != nil {
				return out
			}
			rec.Data = fmt.Sprintf("%d bytes", h.Length)
		}
		out = append(out, rec)
	}
}

const (
	invalidRule     = "_invalid" // pseudo-rule under which locally refused queries are counted
	maxQueriesLimit = recentCapacity
	defaultQueries  = 50
)

func (a *App) registerAPI(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/v1/state", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, a.State())
	})
	mux.HandleFunc("GET /api/v1/queries", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		limit := defaultQueries
		if s := q.Get("limit"); s != "" {
			n, err := strconv.Atoi(s)
			if err != nil || n < 1 || n > maxQueriesLimit {
				writeError(w, http.StatusBadRequest, fmt.Sprintf("limit must be a number in [1, %d]", maxQueriesLimit))
				return
			}
			limit = n
		}
		page, err := a.queries(q.Get("cursor"), limit, domain.Normalize(strings.TrimSpace(q.Get("q"))), q.Get("rule"))
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, page)
	})
	mux.HandleFunc("GET /api/v1/route", a.handleRoute)
	mux.HandleFunc("GET /api/v1/observability", func(w http.ResponseWriter, r *http.Request) {
		window, err := observability.ParseWindow(r.URL.Query().Get("range"))
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		// A nil client (no observability.prometheus_url) reports
		// configured=false and makes no network request.
		writeJSON(w, http.StatusOK, a.obs.Load().Snapshot(r.Context(), window, time.Now()))
	})
	mux.HandleFunc("POST /api/v1/resolve", guardWrite(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Name string `json:"name"`
			Type string `json:"type"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeError(w, http.StatusBadRequest, `body must be JSON such as {"name":"www.baidu.com","type":"A"}`)
			return
		}
		qt := dnsmessage.TypeA
		if body.Type != "" {
			var err error
			if qt, err = config.ParseQType(body.Type); err != nil {
				writeError(w, http.StatusBadRequest, err.Error())
				return
			}
		}
		res, err := a.Resolve(r.Context(), body.Name, qt)
		switch {
		case errors.Is(err, errBadName):
			writeError(w, http.StatusBadRequest, fmt.Sprintf("%q is not a valid domain name", body.Name))
		case err != nil:
			writeError(w, http.StatusBadGateway, err.Error())
		default:
			writeJSON(w, http.StatusOK, res)
		}
	}))
	mux.HandleFunc("POST /api/v1/reload", guardWrite(func(w http.ResponseWriter, r *http.Request) {
		a.log.Info("reload requested through the admin API", "remote", r.RemoteAddr)
		if err := a.Reload(); err != nil {
			a.log.Error("reload rejected; still serving the previous config", "err", err)
			// The request was understood but the new configuration was refused; the
			// previous runtime keeps serving, as after a failed SIGHUP.
			writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"ok": false, "error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "loaded_at": a.rt.Load().LoadedAt.UTC()})
	}))
}

// handleRoute serves /api/v1/route and its legacy spelling /debug/route.
func (a *App) handleRoute(w http.ResponseWriter, r *http.Request) {
	name := r.URL.Query().Get("name")
	if name == "" {
		writeError(w, http.StatusBadRequest, "name is required, e.g. "+r.URL.Path+"?name=www.baidu.com&type=A")
		return
	}
	qt := dnsmessage.TypeA
	if t := r.URL.Query().Get("type"); t != "" {
		var err error
		if qt, err = config.ParseQType(t); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
	}
	writeJSON(w, http.StatusOK, a.Explain(name, qt))
}

// guardWrite protects the two POST endpoints against cross-site requests
// triggered from another page open in a browser on the network. The admin port
// is loopback-only, but a reverse proxy may publish it under a host name.
//
//   - The body must be declared as JSON. A page on another origin cannot send
//     that content type without a CORS preflight, and no handler here answers
//     preflights, so the browser never sends the request.
//   - Browsers that send Sec-Fetch-Site must report a same-origin or
//     user-initiated request.
//
// The body is also capped: no endpoint here accepts more than 4 KiB.
func guardWrite(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if site := r.Header.Get("Sec-Fetch-Site"); site != "" && site != "same-origin" && site != "none" {
			writeError(w, http.StatusForbidden, "cross-site requests are not accepted")
			return
		}
		if mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || mt != "application/json" {
			writeError(w, http.StatusUnsupportedMediaType, "Content-Type must be application/json")
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, 4<<10)
		next(w, r)
	}
}

func writeError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}
