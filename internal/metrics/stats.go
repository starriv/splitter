package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
)

// Stats is a point-in-time copy of the counters exposed by the admin API. It is
// read back from the same collectors that /metrics exports, so the control page
// and Prometheus cannot disagree about a number.
type Stats struct {
	Rules       map[string]Timing      // by rule name; "_invalid" holds locally refused queries
	RCodes      map[string]uint64      // responses by RCODE label
	Servers     map[ServerKey]Upstream // by upstream group and server
	Reloads     map[string]uint64      // "success" / "failure"
	Overload    uint64                 // queries dropped at max_inflight or max_tcp_conns, both transports
	WriteErrors uint64                 // replies built but not delivered, both transports
	Malformed   uint64                 // queries that could not be parsed
	Panics      uint64                 // handler panics
	Inflight    float64                // queries being processed
	TCPConns    float64                // client TCP connections currently open
}

// Timing is a count and the sum of the durations behind it.
type Timing struct {
	Count      uint64  // number of observations
	SumSeconds float64 // sum of the observed durations, in seconds
}

// ServerKey identifies one server within one upstream group. Upstream is the
// group name and Server is the server address as configured.
type ServerKey struct{ Upstream, Server string }

// Upstream holds the counters of one upstream server.
type Upstream struct {
	Requests    uint64            // exchanges attempted
	Errors      map[string]uint64 // by reason
	Dropped     uint64            // responses discarded as non-matching
	Retransmits uint64            // UDP retransmissions
	OK          Timing            // successful exchanges
	Penalized   bool              // moved behind the healthy servers after consecutive failures
}

// BuildInfo returns the version and commit strings passed to New.
func (m *Metrics) BuildInfo() (version, commit string) { return m.version, m.commit }

// Stats reads the current values. It collects only the splitter's own series,
// not the Go runtime and process collectors that a full Gather would also run.
func (m *Metrics) Stats() Stats {
	s := Stats{
		Rules:   map[string]Timing{},
		RCodes:  map[string]uint64{},
		Servers: map[ServerKey]Upstream{},
		Reloads: map[string]uint64{},
	}
	server := func(l map[string]string, update func(*Upstream)) {
		k := ServerKey{Upstream: l["upstream"], Server: l["server"]}
		u := s.Servers[k]
		if u.Errors == nil {
			u.Errors = map[string]uint64{}
		}
		update(&u)
		s.Servers[k] = u
	}
	// The duration histogram carries the same count as queries_total and also
	// provides the sum. It has one series per rule, whereas queries_total has
	// one per rule/action/target and therefore splits a rule whose target
	// changed on reload.
	each(m.duration, func(l map[string]string, d *dto.Metric) {
		s.Rules[l["rule"]] = timing(d)
	})
	each(m.responses, func(l map[string]string, d *dto.Metric) {
		s.RCodes[l["rcode"]] = count(d)
	})
	each(m.upRequests, func(l map[string]string, d *dto.Metric) {
		server(l, func(u *Upstream) { u.Requests = count(d) })
	})
	each(m.upErrors, func(l map[string]string, d *dto.Metric) {
		server(l, func(u *Upstream) { u.Errors[l["reason"]] = count(d) })
	})
	each(m.upDropped, func(l map[string]string, d *dto.Metric) {
		server(l, func(u *Upstream) { u.Dropped = count(d) })
	})
	each(m.upRetrans, func(l map[string]string, d *dto.Metric) {
		server(l, func(u *Upstream) { u.Retransmits = count(d) })
	})
	each(m.upPenalized, func(l map[string]string, d *dto.Metric) {
		server(l, func(u *Upstream) { u.Penalized = d.GetGauge().GetValue() > 0 })
	})
	each(m.upDuration, func(l map[string]string, d *dto.Metric) {
		server(l, func(u *Upstream) { u.OK = timing(d) })
	})
	each(m.reloads, func(l map[string]string, d *dto.Metric) {
		s.Reloads[l["result"]] = count(d)
	})
	each(m.overload, func(_ map[string]string, d *dto.Metric) { s.Overload += count(d) })
	each(m.writeErrors, func(_ map[string]string, d *dto.Metric) { s.WriteErrors += count(d) })
	each(m.malformed, func(_ map[string]string, d *dto.Metric) { s.Malformed = count(d) })
	each(m.panics, func(_ map[string]string, d *dto.Metric) { s.Panics = count(d) })
	each(m.inflight, func(_ map[string]string, d *dto.Metric) { s.Inflight = d.GetGauge().GetValue() })
	each(m.tcpConns, func(_ map[string]string, d *dto.Metric) { s.TCPConns = d.GetGauge().GetValue() })
	return s
}

// count returns the value of a counter metric.
func count(d *dto.Metric) uint64 { return uint64(d.GetCounter().GetValue()) }

// timing returns the sample count and sample sum of a histogram metric.
func timing(d *dto.Metric) Timing {
	h := d.GetHistogram()
	return Timing{Count: h.GetSampleCount(), SumSeconds: h.GetSampleSum()}
}

// each calls fn once for every series that c currently holds. The collector is
// drained on a separate goroutine because Collect blocks on an unbuffered
// channel.
func each(c prometheus.Collector, fn func(labels map[string]string, d *dto.Metric)) {
	ch := make(chan prometheus.Metric)
	go func() {
		c.Collect(ch)
		close(ch)
	}()
	for metric := range ch {
		var d dto.Metric
		if metric.Write(&d) != nil {
			continue
		}
		labels := make(map[string]string, len(d.GetLabel()))
		for _, lp := range d.GetLabel() {
			labels[lp.GetName()] = lp.GetValue()
		}
		fn(labels, &d)
	}
}
