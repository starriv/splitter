package upstream

import (
	"context"
	"net/netip"
	"sync"
	"testing"
	"time"

	"golang.org/x/net/dns/dnsmessage"

	"github.com/starriv/splitter/internal/dnstest"
	"github.com/starriv/splitter/internal/wire"
)

// recorder is an Observer that records exchange reasons and counts dropped and
// retransmitted exchanges.
type recorder struct {
	mu        sync.Mutex
	reasons   []string
	dropped   int
	retrans   int
	penalties []bool
}

func (r *recorder) ObservePenalty(_, _ string, p bool) {
	r.mu.Lock()
	r.penalties = append(r.penalties, p)
	r.mu.Unlock()
}

func (r *recorder) ObserveRetransmit(_, _ string) { r.mu.Lock(); r.retrans++; r.mu.Unlock() }

func (r *recorder) ObserveExchange(_, _ string, _ time.Duration, reason string) {
	r.mu.Lock()
	r.reasons = append(r.reasons, reason)
	r.mu.Unlock()
}
func (r *recorder) ObserveDropped(_, _ string) { r.mu.Lock(); r.dropped++; r.mu.Unlock() }

// query builds an A query with a fixed ID and returns its wire form together
// with the parsed question.
func query(t *testing.T, name string) ([]byte, dnsmessage.Question) {
	req := dnstest.NewQuery(t, 0xBEEF, name, dnsmessage.TypeA, true)
	q, err := wire.ParseQuery(req)
	if err != nil {
		t.Fatal(err)
	}
	return req, q.Question
}

// check asserts that resp answers with the given address and restores the
// query ID used by query.
func check(t *testing.T, resp []byte, want netip.Addr) {
	t.Helper()
	var m dnsmessage.Message
	if err := m.Unpack(resp); err != nil {
		t.Fatal(err)
	}
	if m.Header.ID != 0xBEEF {
		t.Errorf("ID not restored: %#x", m.Header.ID)
	}
	if got := dnstest.FirstA(m); got != want {
		t.Errorf("answer %v, want %v", got, want)
	}
}

// TestExchangeUDPAndTCP verifies that both transports complete an exchange and
// restore the caller's query ID.
func TestExchangeUDPAndTCP(t *testing.T) {
	up := dnstest.StartUpstream(t, netip.MustParseAddr("10.0.0.1"))
	g := NewGroup("g", []netip.AddrPort{up.Addr}, time.Second, nil)
	req, q := query(t, "www.baidu.com.")
	for _, p := range []wire.Proto{wire.UDP, wire.TCP} {
		resp, err := g.Exchange(context.Background(), req, q, p)
		if err != nil {
			t.Fatalf("%s: %v", p, err)
		}
		check(t, resp, up.A)
	}
}

// TestForgedResponseDropped verifies that a response whose ID or question does
// not match is dropped and counted, while the genuine response is still
// accepted.
func TestForgedResponseDropped(t *testing.T) {
	up := dnstest.StartUpstream(t, netip.MustParseAddr("10.0.0.1"))
	up.Forge.Store(true)
	rec := &recorder{}
	g := NewGroup("g", []netip.AddrPort{up.Addr}, time.Second, rec)
	req, q := query(t, "www.baidu.com.")
	resp, err := g.Exchange(context.Background(), req, q, wire.UDP)
	if err != nil {
		t.Fatal(err)
	}
	check(t, resp, up.A)
	if rec.dropped != 1 {
		t.Errorf("dropped = %d, want 1", rec.dropped)
	}
}

// TestFallbackToNextServer verifies that a transport failure on the first
// server falls through to the next one and is reported as a timeout.
func TestFallbackToNextServer(t *testing.T) {
	up := dnstest.StartUpstream(t, netip.MustParseAddr("10.0.0.2"))
	rec := &recorder{}
	g := NewGroup("g", []netip.AddrPort{dnstest.BlackHole(t), up.Addr}, 200*time.Millisecond, rec)
	req, q := query(t, "example.org.")
	resp, err := g.Exchange(context.Background(), req, q, wire.UDP)
	if err != nil {
		t.Fatal(err)
	}
	check(t, resp, up.A)
	if len(rec.reasons) != 2 || rec.reasons[0] != "timeout" || rec.reasons[1] != "" {
		t.Errorf("reasons = %q, want [timeout, \"\"]", rec.reasons)
	}
}

// TestAllServersFail verifies that exhausting every server in a group returns
// an error.
func TestAllServersFail(t *testing.T) {
	g := NewGroup("g", []netip.AddrPort{dnstest.BlackHole(t)}, 100*time.Millisecond, nil)
	req, q := query(t, "example.org.")
	if _, err := g.Exchange(context.Background(), req, q, wire.UDP); err == nil {
		t.Fatal("want error")
	}
}

// TestTruncatedPassedThrough verifies that a truncated UDP answer reaches the
// client unchanged, so the client retries over TCP.
func TestTruncatedPassedThrough(t *testing.T) {
	up := dnstest.StartUpstream(t, netip.MustParseAddr("10.0.0.1"))
	up.TruncateUDP.Store(true)
	g := NewGroup("g", []netip.AddrPort{up.Addr}, time.Second, nil)
	req, q := query(t, "big.example.org.")
	resp, err := g.Exchange(context.Background(), req, q, wire.UDP)
	if err != nil {
		t.Fatal(err)
	}
	if !wire.Truncated(resp) {
		t.Fatal("TC bit must reach the client so it retries over TCP")
	}
}

// TestCancelAbortsQuickly verifies that cancellation unblocks a pending
// exchange promptly instead of waiting for the full timeout.
func TestCancelAbortsQuickly(t *testing.T) {
	g := NewGroup("g", []netip.AddrPort{dnstest.BlackHole(t)}, 5*time.Second, nil)
	req, q := query(t, "example.org.")
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(50*time.Millisecond, cancel)
	start := time.Now()
	_, _ = g.Exchange(ctx, req, q, wire.UDP)
	if d := time.Since(start); d > time.Second {
		t.Fatalf("cancel took %s", d)
	}
}

// TestExchangeClampsEDNSPayload verifies that the EDNS UDP payload size offered
// upstream is clamped to 1232 bytes, so that a large answer does not require IP
// fragmentation, while the client's own advertisement is left untouched and is
// not part of the forwarded packet.
func TestExchangeClampsEDNSPayload(t *testing.T) {
	up := dnstest.StartUpstream(t, netip.MustParseAddr("10.0.0.1"))
	g := NewGroup("g", []netip.AddrPort{up.Addr}, time.Second, nil)

	var rh dnsmessage.ResourceHeader
	if err := rh.SetEDNS0(4096, 0, false); err != nil {
		t.Fatal(err)
	}
	m := dnsmessage.Message{
		Header:      dnsmessage.Header{ID: 0x0102, RecursionDesired: true},
		Questions:   []dnsmessage.Question{{Name: dnsmessage.MustNewName("www.baidu.com."), Type: dnsmessage.TypeA, Class: dnsmessage.ClassINET}},
		Additionals: []dnsmessage.Resource{{Header: rh, Body: &dnsmessage.OPTResource{}}},
	}
	req, err := m.Pack()
	if err != nil {
		t.Fatal(err)
	}
	pq, err := wire.ParseQuery(req)
	if err != nil {
		t.Fatal(err)
	}
	if pq.UDPSize != 4096 {
		t.Fatalf("test query advertises %d", pq.UDPSize)
	}
	if _, err := g.Exchange(context.Background(), req, pq.Question, wire.UDP); err != nil {
		t.Fatal(err)
	}
	seen := *up.LastQuery.Load()
	if got := wire.EDNSPayloadOf(seen); got != wire.EDNSPayload {
		t.Fatalf("upstream saw EDNS payload size %d, want %d", got, wire.EDNSPayload)
	}
	// Over TCP the payload size is irrelevant and must be left alone.
	if _, err := g.Exchange(context.Background(), req, pq.Question, wire.TCP); err != nil {
		t.Fatal(err)
	}
	if got := wire.EDNSPayloadOf(*up.LastQuery.Load()); got != 4096 {
		t.Fatalf("TCP exchange rewrote the EDNS payload size to %d", got)
	}
}

// TestUDPRetransmitAfterLoss verifies that one lost packet costs at most half
// of the timeout: the copy sent at the retransmit point carries the same ID, so
// its answer is accepted.
func TestUDPRetransmitAfterLoss(t *testing.T) {
	up := dnstest.StartUpstream(t, netip.MustParseAddr("10.0.0.1"))
	up.DropUDP.Store(1)
	rec := &recorder{}
	g := NewGroup("g", []netip.AddrPort{up.Addr}, 400*time.Millisecond, rec)
	req, q := query(t, "www.baidu.com.")
	start := time.Now()
	resp, err := g.Exchange(context.Background(), req, q, wire.UDP)
	if err != nil {
		t.Fatalf("a single lost packet failed the exchange: %v", err)
	}
	check(t, resp, up.A)
	if d := time.Since(start); d < 150*time.Millisecond || d > 390*time.Millisecond {
		t.Errorf("took %s, want about half of the 400ms timeout", d)
	}
	if rec.retrans != 1 || up.Queries.Load() != 2 {
		t.Errorf("retransmits = %d, upstream saw %d queries; want 1 and 2", rec.retrans, up.Queries.Load())
	}
	if len(rec.reasons) != 1 || rec.reasons[0] != "" {
		t.Errorf("reasons = %q, want one success", rec.reasons)
	}
}

// TestUDPRetransmitBounds verifies that exactly one retransmission is
// attempted, that the attempt still ends at its own deadline, and that a
// canceled caller is not delayed by the retransmission bookkeeping.
func TestUDPRetransmitBounds(t *testing.T) {
	up := dnstest.StartUpstream(t, netip.MustParseAddr("10.0.0.1"))
	up.DropUDP.Store(100)
	rec := &recorder{}
	g := NewGroup("g", []netip.AddrPort{up.Addr}, 200*time.Millisecond, rec)
	req, q := query(t, "www.baidu.com.")
	start := time.Now()
	if _, err := g.Exchange(context.Background(), req, q, wire.UDP); err == nil {
		t.Fatal("answer from an upstream that drops everything")
	}
	if d := time.Since(start); d < 180*time.Millisecond || d > 600*time.Millisecond {
		t.Errorf("took %s, want the 200ms timeout", d)
	}
	if rec.retrans != 1 || up.Queries.Load() != 2 || rec.reasons[0] != "timeout" {
		t.Errorf("retransmits = %d, queries = %d, reasons = %q", rec.retrans, up.Queries.Load(), rec.reasons)
	}

	slow := NewGroup("g", []netip.AddrPort{up.Addr}, 5*time.Second, rec)
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(50*time.Millisecond, cancel)
	start = time.Now()
	if _, err := slow.Exchange(ctx, req, q, wire.UDP); err == nil {
		t.Fatal("answer after cancel")
	}
	if d := time.Since(start); d > time.Second {
		t.Errorf("cancel took %s to unblock the exchange", d)
	}
}

// TestFailingServerPenalized verifies that a black-holed first server stops
// costing every query a full timeout once it has failed penaltyThreshold times
// in a row, and that it is tried first again after the penalty expires.
func TestFailingServerPenalized(t *testing.T) {
	up := dnstest.StartUpstream(t, netip.MustParseAddr("10.0.0.2"))
	rec := &recorder{}
	g := NewGroup("g", []netip.AddrPort{dnstest.BlackHole(t), up.Addr}, 100*time.Millisecond, rec)
	now := time.Now()
	g.now = func() time.Time { return now }
	req, q := query(t, "example.org.")
	for i := 0; i < penaltyThreshold; i++ {
		if _, err := g.Exchange(context.Background(), req, q, wire.UDP); err != nil {
			t.Fatal(err)
		}
	}
	if !g.Penalized(0) || g.Penalized(1) {
		t.Fatalf("penalized = %v/%v after %d failures, want true/false", g.Penalized(0), g.Penalized(1), penaltyThreshold)
	}
	if len(rec.penalties) != 1 || !rec.penalties[0] {
		t.Errorf("penalty observations = %v, want [true]", rec.penalties)
	}

	start := time.Now()
	resp, err := g.Exchange(context.Background(), req, q, wire.UDP)
	if err != nil {
		t.Fatal(err)
	}
	check(t, resp, up.A)
	if d := time.Since(start); d >= 100*time.Millisecond {
		t.Errorf("penalized server still tried first: exchange took %s", d)
	}

	// After the penalty the black hole is tried first again; one more failure
	// renews the penalty without a second observation.
	now = now.Add(penaltyDuration + time.Second)
	if g.Penalized(0) {
		t.Fatal("penalty did not expire")
	}
	before := len(rec.reasons)
	if _, err := g.Exchange(context.Background(), req, q, wire.UDP); err != nil {
		t.Fatal(err)
	}
	if got := rec.reasons[before:]; len(got) != 2 || got[0] != "timeout" {
		t.Errorf("reasons after expiry = %q, want [timeout, \"\"]", got)
	}
	if !g.Penalized(0) || len(rec.penalties) != 1 {
		t.Errorf("penalty not renewed: penalized=%v observations=%v", g.Penalized(0), rec.penalties)
	}
}

// TestPenaltyClearedBySuccess verifies that one success clears a server's
// failure record.
func TestPenaltyClearedBySuccess(t *testing.T) {
	up := dnstest.StartUpstream(t, netip.MustParseAddr("10.0.0.1"))
	rec := &recorder{}
	g := NewGroup("g", []netip.AddrPort{up.Addr}, time.Second, rec)
	for i := 0; i < penaltyThreshold; i++ {
		g.markFailed(0)
	}
	if !g.Penalized(0) {
		t.Fatal("not penalized")
	}
	req, q := query(t, "example.org.")
	resp, err := g.Exchange(context.Background(), req, q, wire.UDP)
	if err != nil {
		t.Fatal(err)
	}
	check(t, resp, up.A)
	if g.Penalized(0) || g.health[0].fails.Load() != 0 {
		t.Error("success did not clear the failure record")
	}
	if len(rec.penalties) != 2 || rec.penalties[1] {
		t.Errorf("penalty observations = %v, want [true false]", rec.penalties)
	}
}

// TestRefusedFallsThrough verifies that REFUSED moves on to the next server,
// and that the REFUSED answer itself is returned when every server refuses.
func TestRefusedFallsThrough(t *testing.T) {
	refusing := dnstest.StartUpstream(t, netip.MustParseAddr("10.0.0.1"))
	refusing.Refuse.Store(true)
	good := dnstest.StartUpstream(t, netip.MustParseAddr("10.0.0.2"))
	rec := &recorder{}
	g := NewGroup("g", []netip.AddrPort{refusing.Addr, good.Addr}, time.Second, rec)
	req, q := query(t, "example.org.")
	resp, err := g.Exchange(context.Background(), req, q, wire.UDP)
	if err != nil {
		t.Fatal(err)
	}
	check(t, resp, good.A)
	if len(rec.reasons) != 2 || rec.reasons[0] != "refused" || rec.reasons[1] != "" {
		t.Errorf("reasons = %q, want [refused, \"\"]", rec.reasons)
	}

	only := NewGroup("g", []netip.AddrPort{refusing.Addr}, time.Second, nil)
	resp, err = only.Exchange(context.Background(), req, q, wire.TCP)
	if err != nil {
		t.Fatalf("all servers refused: %v, want the REFUSED answer", err)
	}
	if wire.RCode(resp) != int(dnsmessage.RCodeRefused) || wire.ID(resp) != 0xBEEF {
		t.Errorf("rcode %d id %#x, want REFUSED with the caller's ID", wire.RCode(resp), wire.ID(resp))
	}
}
