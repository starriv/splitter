package app

import (
	"strings"
	"sync"
	"time"

	"golang.org/x/net/dns/dnsmessage"

	"github.com/starriv/splitter/internal/config"
	"github.com/starriv/splitter/internal/domain"
	"github.com/starriv/splitter/internal/wire"
)

// recentCapacity bounds the in-memory ring of routed queries that the control
// page displays. The ring is a live view of the splitter's current decisions,
// not a query log: it is never persisted, and durable per-device history is
// kept by the front-end caching resolver. Only cache misses reach the
// splitter, so 512 entries cover several minutes of traffic.
const recentCapacity = 512

// recentEntry stores raw values so that the hot path performs no formatting.
type recentEntry struct {
	seq    uint64
	at     time.Time
	name   string
	qtype  dnsmessage.Type
	proto  wire.Proto
	rule   string
	set    string
	kind   domain.Kind
	target string
	rcode  int
	tc     bool
	dur    time.Duration
}

// QueryRecord is one entry of GET /api/v1/queries.
type QueryRecord struct {
	Seq        uint64    `json:"seq"`
	Time       time.Time `json:"time"`
	Name       string    `json:"name"`
	Type       string    `json:"type"`
	Proto      string    `json:"proto"`
	Rule       string    `json:"rule"`
	Set        string    `json:"set,omitempty"`
	Match      string    `json:"match,omitempty"`
	Target     string    `json:"target"`
	RCode      string    `json:"rcode"`
	Truncated  bool      `json:"truncated,omitempty"`
	DurationMS float64   `json:"duration_ms"`
}

// recent is a fixed-size ring of the latest routed queries.
type recent struct {
	mu   sync.Mutex
	buf  [recentCapacity]recentEntry
	next uint64 // sequence number of the next entry; also the total ever added
}

func (r *recent) add(e recentEntry) {
	r.mu.Lock()
	e.seq = r.next + 1
	r.buf[r.next%recentCapacity] = e
	r.next++
	r.mu.Unlock()
}

// list returns up to limit entries, newest first, keeping entries whose name
// contains substr and whose rule equals rule when those filters are set.
// total is the number of queries recorded since start.
func (r *recent) list(limit int, substr, rule string) (out []QueryRecord, total uint64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := min(r.next, recentCapacity)
	out = make([]QueryRecord, 0, min(limit, int(n)))
	for i := uint64(1); i <= n && len(out) < limit; i++ {
		e := &r.buf[(r.next-i)%recentCapacity]
		if rule != "" && e.rule != rule {
			continue
		}
		if substr != "" && !strings.Contains(e.name, substr) {
			continue
		}
		rec := QueryRecord{
			Seq: e.seq, Time: e.at.UTC(), Name: e.name, Type: config.QTypeName(e.qtype),
			Proto: e.proto.String(), Rule: e.rule, Set: e.set, Target: e.target,
			RCode: wire.RCodeName(e.rcode), Truncated: e.tc,
			DurationMS: float64(e.dur.Microseconds()) / 1000,
		}
		if e.set != "" {
			rec.Match = e.kind.String()
		}
		out = append(out, rec)
	}
	return out, r.next
}
