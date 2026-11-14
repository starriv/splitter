package app

import (
	"fmt"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/dns/dnsmessage"

	"github.com/starriv/splitter/internal/config"
	"github.com/starriv/splitter/internal/domain"
	"github.com/starriv/splitter/internal/querylog"
	"github.com/starriv/splitter/internal/wire"
)

// recentCapacity bounds the in-memory ring of answered queries. The ring
// serves GET /api/v1/queries when the on-disk query log is disabled. Only
// cache misses reach the splitter, so 512 entries cover several minutes of
// traffic.
const recentCapacity = 512

// recentEntry stores raw values so that the hot path performs no formatting.
type recentEntry struct {
	seq    uint64
	at     time.Time
	name   string
	qtype  dnsmessage.Type
	class  dnsmessage.Class // zero when the query had no readable question
	proto  wire.Proto
	rule   string
	set    string
	kind   domain.Kind
	target string
	rcode  int
	tc     bool
	dur    time.Duration
}

// QueryRecord is one entry of GET /api/v1/queries. The in-memory ring and the
// on-disk query log share the type, so the API returns one shape from either
// source; Seq is set only by the ring.
type QueryRecord = querylog.Record

// record formats an entry for the API and the query log.
func (e *recentEntry) record() QueryRecord {
	rec := QueryRecord{
		Seq: e.seq, Time: e.at.UTC(), Name: e.name,
		Proto: e.proto.String(), Rule: e.rule, Set: e.set, Target: e.target,
		RCode: wire.RCodeName(e.rcode), Truncated: e.tc,
		DurationMS: float64(e.dur.Microseconds()) / 1000,
	}
	if e.class != 0 {
		rec.Type = config.QTypeName(e.qtype)
	}
	if e.class != 0 && e.class != dnsmessage.ClassINET {
		rec.Class = className(e.class)
	}
	if e.set != "" {
		rec.Match = e.kind.String()
	}
	return rec
}

// className names a DNS class for display.
func className(c dnsmessage.Class) string {
	switch c {
	case dnsmessage.ClassCHAOS:
		return "CH"
	case dnsmessage.ClassHESIOD:
		return "HS"
	case dnsmessage.ClassCSNET:
		return "CS"
	case dnsmessage.ClassANY:
		return "ANY"
	case 254:
		return "NONE"
	}
	return fmt.Sprint(uint16(c))
}

// recent is a fixed-size ring of the latest answered queries.
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
	out, _, total = r.page(0, limit, substr, rule)
	return out, total
}

// page is list restricted to entries with a sequence number below before (0
// means no bound), so a client can page backwards through the ring with the
// Seq of the last record it received. more reports whether an older entry
// that passes the filters remains in the ring. Pages are stable while new
// queries arrive: they only ever take sequence numbers above every cursor.
func (r *recent) page(before uint64, limit int, substr, rule string) (out []QueryRecord, more bool, total uint64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := min(r.next, recentCapacity)
	out = make([]QueryRecord, 0, min(limit, int(n)))
	for i := uint64(1); i <= n; i++ {
		e := &r.buf[(r.next-i)%recentCapacity]
		if before != 0 && e.seq >= before {
			continue
		}
		if rule != "" && e.rule != rule {
			continue
		}
		if substr != "" && !strings.Contains(e.name, substr) {
			continue
		}
		if len(out) == limit {
			more = true
			break
		}
		out = append(out, e.record())
	}
	return out, more, r.next
}
