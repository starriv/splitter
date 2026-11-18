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
	coalesced int
	panics    int
}

func (r *recorder) ObserveCoalesced(string) { r.mu.Lock(); r.coalesced++; r.mu.Unlock() }

func (r *recorder) ObservePanic(string) { r.mu.Lock(); r.panics++; r.mu.Unlock() }

func (r *recorder) panicCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.panics
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

// withID returns a copy of base with a different message ID, so callers of the
// same logical query have identical bytes apart from the ID.
func withID(base []byte, id uint16) []byte {
	req := append([]byte(nil), base...)
	wire.SetID(req, id)
	return req
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

// waitQueries waits until up has received n queries.
func waitQueries(t *testing.T, up *dnstest.Upstream, n int64) {
	t.Helper()
	for deadline := time.Now().Add(2 * time.Second); up.Queries.Load() < n; {
		if time.Now().After(deadline) {
			t.Fatalf("upstream received %d queries, want %d", up.Queries.Load(), n)
		}
		time.Sleep(time.Millisecond)
	}
}

// TestCoalesceIdenticalQueries verifies that identical queries in flight at
// the same time share one upstream exchange, and that each caller gets the
// answer with its own ID.
func TestCoalesceIdenticalQueries(t *testing.T) {
	up := dnstest.StartUpstream(t, netip.MustParseAddr("10.0.0.1"))
	up.Delay.Store(int64(300 * time.Millisecond))
	rec := &recorder{}
	g := NewGroup("g", []netip.AddrPort{up.Addr}, 5*time.Second, rec)
	_, q := query(t, "www.baidu.com.")

	const n = 5
	var wg sync.WaitGroup
	errs := make([]error, n)
	ids := make([]uint16, n)
	for i := range n {
		wg.Go(func() {
			req := dnstest.NewQuery(t, uint16(0x1000+i), "www.baidu.com.", dnsmessage.TypeA, true)
			resp, err := g.Exchange(context.Background(), req, q, wire.UDP)
			if err == nil {
				ids[i] = wire.ID(resp)
			}
			errs[i] = err
		})
		if i == 0 {
			waitQueries(t, up, 1) // the others join an exchange already in flight
		}
	}
	wg.Wait()
	for i := range n {
		if errs[i] != nil {
			t.Fatalf("query %d: %v", i, errs[i])
		}
		if ids[i] != uint16(0x1000+i) {
			t.Errorf("query %d got ID %#x, want %#x", i, ids[i], 0x1000+i)
		}
	}
	if got := up.Queries.Load(); got != 1 {
		t.Errorf("upstream received %d queries, want 1", got)
	}
	if rec.coalesced != n-1 {
		t.Errorf("coalesced = %d, want %d", rec.coalesced, n-1)
	}

	// Once the exchange is over nothing is kept: the next query goes upstream.
	up.Delay.Store(0)
	req, _ := query(t, "www.baidu.com.")
	if _, err := g.Exchange(context.Background(), req, q, wire.UDP); err != nil {
		t.Fatal(err)
	}
	if got := up.Queries.Load(); got != 2 {
		t.Errorf("upstream received %d queries after the flight, want 2", got)
	}
}

// TestCoalesceKeepsDistinctQueriesApart verifies that queries differing in
// name, flags or transport are not merged.
func TestCoalesceKeepsDistinctQueriesApart(t *testing.T) {
	up := dnstest.StartUpstream(t, netip.MustParseAddr("10.0.0.1"))
	up.Delay.Store(int64(200 * time.Millisecond))
	rec := &recorder{}
	g := NewGroup("g", []netip.AddrPort{up.Addr}, 5*time.Second, rec)

	type call struct {
		name  string
		edns  bool
		proto wire.Proto
	}
	calls := []call{
		{"a.example.org.", true, wire.UDP},
		{"b.example.org.", true, wire.UDP},  // different name
		{"a.example.org.", false, wire.UDP}, // no EDNS
		{"A.example.org.", true, wire.UDP},  // different case
		{"a.example.org.", true, wire.TCP},  // different transport
	}
	var wg sync.WaitGroup
	for _, c := range calls {
		wg.Go(func() {
			req := dnstest.NewQuery(t, 0xBEEF, c.name, dnsmessage.TypeA, c.edns)
			pq, err := wire.ParseQuery(req)
			if err != nil {
				t.Error(err)
				return
			}
			if _, err := g.Exchange(context.Background(), req, pq.Question, c.proto); err != nil {
				t.Errorf("%+v: %v", c, err)
			}
		})
	}
	wg.Wait()
	if got := up.Queries.Load(); got != int64(len(calls)) {
		t.Errorf("upstream received %d queries, want %d", got, len(calls))
	}
	if rec.coalesced != 0 {
		t.Errorf("coalesced = %d, want 0", rec.coalesced)
	}
}

// TestCoalesceCallerCancel verifies that a caller giving up does not abort the
// exchange for the callers still waiting on it, and that the exchange is
// aborted once every caller has gone.
func TestCoalesceCallerCancel(t *testing.T) {
	up := dnstest.StartUpstream(t, netip.MustParseAddr("10.0.0.1"))
	up.Delay.Store(int64(300 * time.Millisecond))
	rec := &recorder{}
	g := NewGroup("g", []netip.AddrPort{up.Addr}, 5*time.Second, rec)
	req, q := query(t, "www.baidu.com.")

	ctx, cancel := context.WithCancel(context.Background())
	leader := make(chan error, 1)
	go func() { _, err := g.Exchange(ctx, req, q, wire.UDP); leader <- err }()
	waitQueries(t, up, 1)
	var fresp []byte
	follower := make(chan error, 1)
	go func() {
		var err error
		fresp, err = g.Exchange(context.Background(), req, q, wire.UDP)
		follower <- err
	}()
	waitWaiting(t, g, req, 2)
	cancel()
	if err := <-leader; err == nil {
		t.Error("canceled caller: want error")
	}
	if err := <-follower; err != nil {
		t.Fatalf("follower: %v", err)
	}
	check(t, fresp, up.A)
	if got := up.Queries.Load(); got != 1 {
		t.Errorf("upstream received %d queries, want 1", got)
	}

	// Every caller gone: the exchange is aborted and unlisted at once, so the
	// next identical query starts its own.
	g2 := NewGroup("g", []netip.AddrPort{dnstest.BlackHole(t)}, 5*time.Second, nil)
	ctx2, cancel2 := context.WithCancel(context.Background())
	time.AfterFunc(50*time.Millisecond, cancel2)
	start := time.Now()
	if _, err := g2.Exchange(ctx2, req, q, wire.UDP); err == nil {
		t.Error("want error")
	}
	if d := time.Since(start); d > time.Second {
		t.Fatalf("cancel took %s", d)
	}
	if n := g2.waiting(req); n != 0 {
		t.Errorf("flight still listed with %d waiters", n)
	}
}

// waiting returns the number of callers waiting on the flight for req over
// UDP, or 0 when none is in progress.
func (g *Group) waiting(req []byte) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	if f := g.flights[flightKey(req, wire.UDP)]; f != nil {
		return f.waiters
	}
	return 0
}

// waitWaiting waits until the flight for req has n callers waiting on it.
func waitWaiting(t *testing.T, g *Group, req []byte, n int) {
	t.Helper()
	for deadline := time.Now().Add(2 * time.Second); g.waiting(req) < n; {
		if time.Now().After(deadline) {
			t.Fatalf("flight has %d waiters, want %d", g.waiting(req), n)
		}
		time.Sleep(time.Millisecond)
	}
}

// waitPanics waits until obs has recorded n recovered panics. The panic is
// reported after the waiters are released, so this cannot be observed
// synchronously from a caller.
func waitPanics(t *testing.T, obs interface{ panicCount() int }, n int) {
	t.Helper()
	for deadline := time.Now().Add(2 * time.Second); ; {
		if got := obs.panicCount(); got >= n {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("recorded %d panics, want %d", obs.panicCount(), n)
		}
		time.Sleep(time.Millisecond)
	}
}

// TestCoalesceSharedError verifies that a failing exchange reports the same
// error to every caller that joined it.
func TestCoalesceSharedError(t *testing.T) {
	rec := &recorder{}
	g := NewGroup("g", []netip.AddrPort{dnstest.BlackHole(t)}, 300*time.Millisecond, rec)
	req, q := query(t, "www.baidu.com.")

	leader := make(chan error, 1)
	go func() { _, err := g.Exchange(context.Background(), withID(req, 0x2000), q, wire.UDP); leader <- err }()
	waitWaiting(t, g, req, 1)
	follower := make(chan error, 1)
	go func() { _, err := g.Exchange(context.Background(), withID(req, 0x2001), q, wire.UDP); follower <- err }()
	waitWaiting(t, g, req, 2)

	if err := <-leader; err == nil {
		t.Error("leader: want an error")
	}
	if err := <-follower; err == nil {
		t.Error("follower: want the leader's error")
	}
	if rec.coalesced != 1 {
		t.Errorf("coalesced = %d, want 1", rec.coalesced)
	}
	if n := g.waiting(req); n != 0 {
		t.Errorf("flight still listed with %d waiters", n)
	}
}

// TestCoalesceTCP verifies that in-flight coalescing also applies to the TCP
// transport, and that each caller gets its own ID back.
func TestCoalesceTCP(t *testing.T) {
	up := dnstest.StartUpstream(t, netip.MustParseAddr("10.0.0.1"))
	up.Delay.Store(int64(200 * time.Millisecond))
	rec := &recorder{}
	g := NewGroup("g", []netip.AddrPort{up.Addr}, 5*time.Second, rec)
	_, q := query(t, "www.baidu.com.")

	const n = 3
	var wg sync.WaitGroup
	errs := make([]error, n)
	ids := make([]uint16, n)
	for i := range n {
		wg.Go(func() {
			req := dnstest.NewQuery(t, uint16(0x3000+i), "www.baidu.com.", dnsmessage.TypeA, true)
			resp, err := g.Exchange(context.Background(), req, q, wire.TCP)
			if err == nil {
				ids[i] = wire.ID(resp)
			}
			errs[i] = err
		})
		if i == 0 {
			waitQueries(t, up, 1) // the others join an exchange already in flight
		}
	}
	wg.Wait()
	for i := range n {
		if errs[i] != nil {
			t.Fatalf("query %d: %v", i, errs[i])
		}
		if ids[i] != uint16(0x3000+i) {
			t.Errorf("query %d got ID %#x, want %#x", i, ids[i], 0x3000+i)
		}
	}
	if got := up.Queries.Load(); got != 1 {
		t.Errorf("upstream received %d TCP queries, want 1", got)
	}
	if rec.coalesced != n-1 {
		t.Errorf("coalesced = %d, want %d", rec.coalesced, n-1)
	}
}

// panicOnceObserver panics on its first exchange observation. A panic in this
// path must be contained by the flight goroutine, not take the process down.
type panicOnceObserver struct {
	recorder
	once sync.Once
}

func (o *panicOnceObserver) ObserveExchange(_ string, _ string, _ time.Duration, _ string) {
	o.once.Do(func() { panic("boom from observer") })
}

// TestCoalescePanicContained verifies that a panic inside the shared exchange
// is recovered, reported, and turned into an error for every waiter; the flight
// is unlisted so the next identical query runs its own exchange.
func TestCoalescePanicContained(t *testing.T) {
	up := dnstest.StartUpstream(t, netip.MustParseAddr("10.0.0.1"))
	up.Delay.Store(int64(200 * time.Millisecond))
	obs := &panicOnceObserver{}
	g := NewGroup("g", []netip.AddrPort{up.Addr}, 5*time.Second, obs)
	req, q := query(t, "www.baidu.com.")

	leader := make(chan error, 1)
	go func() { _, err := g.Exchange(context.Background(), withID(req, 0x4000), q, wire.UDP); leader <- err }()
	waitQueries(t, up, 1)
	follower := make(chan error, 1)
	go func() { _, err := g.Exchange(context.Background(), withID(req, 0x4001), q, wire.UDP); follower <- err }()
	waitWaiting(t, g, req, 2)

	if err := <-leader; err == nil {
		t.Error("leader: want an error after the panic")
	}
	if err := <-follower; err == nil {
		t.Error("follower: want an error after the panic")
	}
	waitPanics(t, obs, 1)
	if n := g.waiting(req); n != 0 {
		t.Errorf("flight still listed with %d waiters", n)
	}

	// The panic was contained, so the group remains usable.
	resp, err := g.Exchange(context.Background(), req, q, wire.UDP)
	if err != nil {
		t.Fatalf("query after the panic: %v", err)
	}
	check(t, resp, up.A)
	if got := up.Queries.Load(); got != 2 {
		t.Errorf("upstream received %d queries, want 2", got)
	}
}

// TestCoalesceResponseBuffersIndependent verifies that coalesced callers do not
// share the returned slice: one caller mutating its answer cannot corrupt
// another's.
func TestCoalesceResponseBuffersIndependent(t *testing.T) {
	up := dnstest.StartUpstream(t, netip.MustParseAddr("10.0.0.1"))
	up.Delay.Store(int64(200 * time.Millisecond))
	rec := &recorder{}
	g := NewGroup("g", []netip.AddrPort{up.Addr}, 5*time.Second, rec)
	req, q := query(t, "www.baidu.com.")

	var (
		wg     sync.WaitGroup
		r0, r1 []byte
		e0, e1 error
	)
	for i := 0; i < 2; i++ {
		wg.Go(func() {
			resp, err := g.Exchange(context.Background(), withID(req, uint16(0x5000+i)), q, wire.UDP)
			if i == 0 {
				r0, e0 = resp, err
			} else {
				r1, e1 = resp, err
			}
		})
		if i == 0 {
			waitQueries(t, up, 1)
		}
	}
	wg.Wait()
	if e0 != nil || e1 != nil {
		t.Fatalf("errs = %v, %v", e0, e1)
	}
	if rec.coalesced != 1 {
		t.Fatalf("coalesced = %d, want 1; the buffers were not shared", rec.coalesced)
	}
	for i := range r0 {
		r0[i] = 0 // damage the first caller's copy
	}
	var m dnsmessage.Message
	if err := m.Unpack(r1); err != nil {
		t.Fatal(err)
	}
	if m.Header.ID != 0x5001 {
		t.Errorf("second caller's ID = %#x, want 0x5001", m.Header.ID)
	}
	if got := dnstest.FirstA(m); got != up.A {
		t.Errorf("second caller's answer = %v, want %v", got, up.A)
	}
}

// TestCoalesceAbortThenRetry verifies that a flight aborted because every
// caller gave up is not reused: the next identical query runs its own exchange.
func TestCoalesceAbortThenRetry(t *testing.T) {
	up := dnstest.StartUpstream(t, netip.MustParseAddr("10.0.0.1"))
	up.Delay.Store(int64(200 * time.Millisecond))
	g := NewGroup("g", []netip.AddrPort{up.Addr}, 5*time.Second, nil)
	req, q := query(t, "www.baidu.com.")

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { _, err := g.Exchange(ctx, req, q, wire.UDP); done <- err }()
	waitQueries(t, up, 1)
	cancel()
	if err := <-done; err == nil {
		t.Error("canceled caller: want an error")
	}
	if n := g.waiting(req); n != 0 {
		t.Fatalf("aborted flight still listed with %d waiters", n)
	}

	resp, err := g.Exchange(context.Background(), req, q, wire.UDP)
	if err != nil {
		t.Fatalf("query after the abort: %v", err)
	}
	check(t, resp, up.A)
	if got := up.Queries.Load(); got != 2 {
		t.Errorf("upstream received %d queries, want 2", got)
	}
}
