// Package metrics owns every Prometheus series the splitter exports.
//
// Label sets are bounded: rule, upstream, and set names come from the
// configuration, rcode is a fixed list of mnemonics plus "OTHER" (see
// wire.RCodeLabel; the value is controlled by the upstream), and no label
// carries a query name. Per-name detail belongs in the query log, because a
// label per name would create one series per queried name.
package metrics

import (
	"net/http"
	"slices"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

const ns = "splitter"

// buckets holds the latency histogram buckets, from 0.25 ms to approximately
// 4 s.
var buckets = prometheus.ExponentialBuckets(0.00025, 2, 15)

// Metrics owns the registry and every series the splitter exports. The zero
// value is not usable; create one with New.
type Metrics struct {
	reg             *prometheus.Registry
	version, commit string

	queries     *prometheus.CounterVec
	responses   *prometheus.CounterVec
	duration    *prometheus.HistogramVec
	upRequests  *prometheus.CounterVec
	upErrors    *prometheus.CounterVec
	upDuration  *prometheus.HistogramVec
	upDropped   *prometheus.CounterVec
	upRetrans   *prometheus.CounterVec
	upPenalized *prometheus.GaugeVec
	snapshot    *snapshotCollector
	reloads     *prometheus.CounterVec
	lastReload  prometheus.Gauge
	reloadOK    prometheus.Gauge
	inflight    prometheus.Gauge
	tcpConns    prometheus.Gauge
	overload    *prometheus.CounterVec
	writeErrors *prometheus.CounterVec
	malformed   prometheus.Counter
	panics      prometheus.Counter
	qlWritten   prometheus.Counter
	qlDropped   prometheus.Counter
	qlErrors    prometheus.Counter
}

// upstreamErrorReasons lists every reason that upstream_errors_total can carry
// in its upstream.reason label.
var upstreamErrorReasons = []string{"timeout", "network", "mismatch", "refused", "canceled"}

// SetEntries describes one loaded set for the entries gauge.
type SetEntries struct {
	Set   string // set name
	Kind  string // match kind
	Count int    // number of distinct rules loaded
}

// Source describes one loaded list file for the freshness gauge.
type Source struct {
	Set   string    // set that references the file
	File  string    // path of the list file
	MTime time.Time // file modification time at load
}

// New returns a Metrics with the splitter's collectors registered under the
// "splitter" namespace, together with the Go runtime and process collectors.
// Failure counters are created at zero so that a first failure produces an
// increase rather than a new series.
func New(version, commit string) *Metrics {
	m := &Metrics{reg: prometheus.NewRegistry(), version: version, commit: commit}
	f := func(c prometheus.Collector) { m.reg.MustRegister(c) }

	f(collectors.NewGoCollector())
	f(collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
	build := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: ns, Name: "build_info", Help: "Build information; always 1.",
	}, []string{"version", "commit"})
	build.WithLabelValues(version, commit).Set(1)
	f(build)

	m.queries = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: ns, Name: "queries_total", Help: "Queries by the rule that decided them.",
	}, []string{"rule", "action", "target"})
	m.responses = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: ns, Name: "responses_total", Help: "Responses sent to clients by RCODE.",
	}, []string{"rcode"})
	m.duration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: ns, Name: "request_duration_seconds", Help: "Time from receiving a query to having its response, by rule.", Buckets: buckets,
	}, []string{"rule"})
	m.upRequests = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: ns, Name: "upstream_requests_total", Help: "Exchanges attempted with each upstream server.",
	}, []string{"upstream", "server"})
	m.upErrors = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: ns, Name: "upstream_errors_total", Help: "Failed exchanges by reason (timeout, network, mismatch, refused, canceled). Each failed attempt counts once, including attempts later rescued by the next server.",
	}, []string{"upstream", "server", "reason"})
	m.upDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: ns, Name: "upstream_duration_seconds", Help: "Duration of successful upstream exchanges.", Buckets: buckets,
	}, []string{"upstream", "server"})
	m.upDropped = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: ns, Name: "upstream_dropped_responses_total", Help: "UDP responses discarded because ID or question did not match (late duplicates or spoofing attempts).",
	}, []string{"upstream", "server"})
	m.upRetrans = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: ns, Name: "upstream_retransmits_total", Help: "UDP queries sent a second time because no answer arrived within half the timeout (packet loss on the way out or back).",
	}, []string{"upstream", "server"})
	m.upPenalized = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: ns, Name: "upstream_penalized", Help: "1 while a server is moved behind the healthy servers of its group after consecutive failures, otherwise 0.",
	}, []string{"upstream", "server"})
	m.snapshot = newSnapshotCollector()
	m.reloads = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: ns, Name: "reloads_total", Help: "Configuration loads by result (the initial load counts).",
	}, []string{"result"})
	m.lastReload = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: ns, Name: "last_reload_success_timestamp_seconds", Help: "Time of the last successful configuration load.",
	})
	m.reloadOK = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: ns, Name: "config_last_reload_successful", Help: "1 if the most recent configuration load was accepted, 0 if it was refused (the previous runtime keeps serving).",
	})
	m.inflight = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: ns, Name: "inflight_queries", Help: "Queries being processed.",
	})
	m.tcpConns = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: ns, Name: "tcp_connections", Help: "Client TCP connections currently open.",
	})
	m.overload = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: ns, Name: "overload_dropped_total", Help: "Queries dropped because max_inflight or max_tcp_conns was reached.",
	}, []string{"proto"})
	m.writeErrors = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: ns, Name: "response_write_errors_total", Help: "Responses that could not be written back to the client (a reply was built but never delivered).",
	}, []string{"proto"})
	m.malformed = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: ns, Name: "malformed_queries_total", Help: "Queries that could not be parsed.",
	})
	m.panics = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: ns, Name: "panics_total", Help: "Query handlers that panicked; the query was dropped and the process kept serving. Any value above 0 is a bug.",
	})
	m.qlWritten = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: ns, Name: "query_log_records_total", Help: "Answered queries written to the on-disk query log.",
	})
	m.qlDropped = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: ns, Name: "query_log_dropped_total", Help: "Answered queries not written to the query log because its queue was full (the disk is slower than the query rate).",
	})
	m.qlErrors = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: ns, Name: "query_log_write_errors_total", Help: "Query log write failures plus records lost while writing is suspended after a failure (disk full, permissions); those records are not on disk.",
	})
	for _, c := range []prometheus.Collector{
		m.queries, m.responses, m.duration, m.upRequests, m.upErrors, m.upDuration, m.upDropped, m.upRetrans,
		m.upPenalized, m.snapshot, m.reloads, m.lastReload, m.reloadOK, m.inflight, m.tcpConns, m.overload, m.writeErrors,
		m.malformed, m.panics, m.qlWritten, m.qlDropped, m.qlErrors,
	} {
		f(c)
	}
	// Failure counters must exist at 0 before the first failure: a series that
	// is born with the value 1 has no previous sample, so increase() and rate()
	// report nothing and an alert on the first failed reload never fires.
	for _, r := range []string{"success", "failure"} {
		m.reloads.WithLabelValues(r)
	}
	for _, p := range []string{"udp", "tcp"} {
		m.overload.WithLabelValues(p)
		m.writeErrors.WithLabelValues(p)
	}
	return m
}

// Handler serves the registry in the Prometheus exposition format.
func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.reg, promhttp.HandlerOpts{Registry: m.reg})
}

// Registry exposes the underlying registry, for tests.
func (m *Metrics) Registry() *prometheus.Registry { return m.reg }

// ObserveExchange implements upstream.Observer and records one upstream
// exchange attempt.
func (m *Metrics) ObserveExchange(group, server string, d time.Duration, reason string) {
	m.upRequests.WithLabelValues(group, server).Inc()
	if reason != "" {
		m.upErrors.WithLabelValues(group, server, reason).Inc()
		return
	}
	m.upDuration.WithLabelValues(group, server).Observe(d.Seconds())
}

// ObserveDropped implements upstream.Observer and records one discarded UDP
// response.
func (m *Metrics) ObserveDropped(group, server string) {
	m.upDropped.WithLabelValues(group, server).Inc()
}

// ObserveRetransmit implements upstream.Observer and records one UDP
// retransmission.
func (m *Metrics) ObserveRetransmit(group, server string) {
	m.upRetrans.WithLabelValues(group, server).Inc()
}

// ObservePenalty implements upstream.Observer and records whether a server is
// currently moved behind the healthy servers of its group.
func (m *Metrics) ObservePenalty(group, server string, penalized bool) {
	v := 0.0
	if penalized {
		v = 1
	}
	m.upPenalized.WithLabelValues(group, server).Set(v)
}

// InitUpstream creates the per-server series of a configured upstream at 0, for
// the same reason the failure counters in New are pre-created. A reload builds
// new groups with a clean failure record, so the penalty gauge is reset too.
func (m *Metrics) InitUpstream(group string, servers []string) {
	for _, s := range servers {
		m.upPenalized.WithLabelValues(group, s).Set(0)
		m.upRequests.WithLabelValues(group, s)
		m.upDropped.WithLabelValues(group, s)
		m.upRetrans.WithLabelValues(group, s)
		for _, r := range upstreamErrorReasons {
			m.upErrors.WithLabelValues(group, s, r)
		}
	}
}

// Panic implements server.Observer and counts one handler panic.
func (m *Metrics) Panic() { m.panics.Inc() }

// ObserveQuery records one answered query in the query, response, and duration
// series.
func (m *Metrics) ObserveQuery(rule, action, target, rcode string, d time.Duration) {
	m.queries.WithLabelValues(rule, action, target).Inc()
	m.responses.WithLabelValues(rcode).Inc()
	m.duration.WithLabelValues(rule).Observe(d.Seconds())
}

// QueryLogWritten implements querylog.Observer.
func (m *Metrics) QueryLogWritten() { m.qlWritten.Inc() }

// QueryLogDropped implements querylog.Observer.
func (m *Metrics) QueryLogDropped() { m.qlDropped.Inc() }

// QueryLogWriteError implements querylog.Observer.
func (m *Metrics) QueryLogWriteError() { m.qlErrors.Inc() }

// Malformed counts one query that could not be parsed.
func (m *Metrics) Malformed() { m.malformed.Inc() }

// Inflight implements server.Observer and adjusts the in-flight query gauge.
func (m *Metrics) Inflight(delta float64) { m.inflight.Add(delta) }

// Overload implements server.Observer and counts one query dropped at a
// concurrency limit.
func (m *Metrics) Overload(proto string) { m.overload.WithLabelValues(proto).Inc() }

// Loaded records a successful configuration load and atomically replaces the
// set and file freshness snapshot, so a concurrent scrape observes either the
// previous snapshot or the new one, never an empty or partially filled label
// set.
func (m *Metrics) Loaded(sets []SetEntries, sources []Source) {
	m.snapshot.set(sets, sources)
	m.reloads.WithLabelValues("success").Inc()
	m.lastReload.SetToCurrentTime()
	m.reloadOK.Set(1)
}

// TCPConns implements server.Observer and adjusts the open connection gauge.
func (m *Metrics) TCPConns(delta float64) { m.tcpConns.Add(delta) }

// WriteFailed implements server.Observer and counts one response that could not
// be written to the client.
func (m *Metrics) WriteFailed(proto string) { m.writeErrors.WithLabelValues(proto).Inc() }

// LoadFailed records a refused configuration load.
func (m *Metrics) LoadFailed() {
	m.reloads.WithLabelValues("failure").Inc()
	m.reloadOK.Set(0)
}

// snapshotCollector emits the set and source gauges from one immutable
// snapshot. A GaugeVec that is reset and repopulated on reload would allow a
// concurrent scrape to observe an empty or partially filled label set.
type snapshotCollector struct {
	cur        atomic.Pointer[setSnapshot]
	descSet    *prometheus.Desc
	descSource *prometheus.Desc
}

// setSnapshot is the immutable state published by Loaded.
type setSnapshot struct {
	sets    []SetEntries
	sources []Source
}

func newSnapshotCollector() *snapshotCollector {
	return &snapshotCollector{
		descSet: prometheus.NewDesc(ns+"_set_entries",
			"Distinct rules loaded per set and match kind.", []string{"set", "kind"}, nil),
		descSource: prometheus.NewDesc(ns+"_set_source_modified_timestamp_seconds",
			"Modification time of each list file at the last successful load.", []string{"set", "file"}, nil),
	}
}

// set publishes a copy of the arguments so that the caller cannot mutate the
// live snapshot.
func (c *snapshotCollector) set(sets []SetEntries, sources []Source) {
	c.cur.Store(&setSnapshot{sets: slices.Clone(sets), sources: slices.Clone(sources)})
}

// Describe implements prometheus.Collector.
func (c *snapshotCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.descSet
	ch <- c.descSource
}

// Collect implements prometheus.Collector and emits one gauge per loaded set
// and per loaded source file.
func (c *snapshotCollector) Collect(ch chan<- prometheus.Metric) {
	s := c.cur.Load()
	if s == nil {
		return
	}
	for _, e := range s.sets {
		ch <- prometheus.MustNewConstMetric(c.descSet, prometheus.GaugeValue, float64(e.Count), e.Set, e.Kind)
	}
	for _, src := range s.sources {
		ch <- prometheus.MustNewConstMetric(c.descSource, prometheus.GaugeValue, float64(src.MTime.Unix()), src.Set, src.File)
	}
}
