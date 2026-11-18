// Package upstream forwards queries to a group of plain DNS servers.
//
// The design follows the approach used by unbound and dnsproxy:
//   - Each exchange uses a fresh socket. The operating system selects a random
//     source port and the package selects a random query ID, so an off-path
//     attacker must guess both values (RFC 5452). A connected UDP socket also
//     ignores packets from any other address.
//   - A response is accepted only when its ID and question match. A response
//     that does not match is dropped and counted, and the read continues until
//     the deadline.
//   - A UDP query that has no answer after half the timeout is sent once more
//     on the same socket with the same ID, so an answer to either copy is
//     accepted. Without retransmission, a single lost packet causes a full
//     timeout and a SERVFAIL, because a group usually contains one server and
//     has no failover peer.
//   - The transport mirrors the client: a UDP query is sent over UDP and a
//     truncated (TC) answer is returned unchanged, so the client retries over
//     TCP and that retry is forwarded over TCP. The package never enlarges an
//     answer beyond what the client requested.
//   - Servers in a group are tried in order. The next server is used when the
//     previous one failed at the transport level (timeout or network error) or
//     answered REFUSED, which usually means an access-control mismatch on that
//     server rather than a property of the name. When every server refuses,
//     the last REFUSED answer is returned unchanged. A SERVFAIL from an
//     upstream is a valid answer (often a DNSSEC validation failure) and is
//     passed through.
//   - A server that fails penaltyThreshold times in a row is moved behind the
//     healthy servers for penaltyDuration. Without this, a black-holed first
//     server would cost every query a full timeout before the next server is
//     tried. Once the penalty expires the server is tried first again; one
//     success clears its record, one more failure renews the penalty. A
//     penalized server is still tried when every healthier server failed.
//   - Identical queries in flight at the same time share one exchange. Two
//     queries are identical when their bytes match apart from the ID and they
//     arrive over the same transport, so every flag, EDNS option and the case
//     of the name match too, and the shared answer is a valid answer to each
//     caller's query. Only the ID differs, and it is restored per caller.
//     Nothing is kept after the exchange completes; this is not a cache. The
//     shared exchange is aborted only when every caller waiting on it has given
//     up. A panic inside the shared exchange is contained: every caller
//     receives an error and the failure is counted, so one defective exchange
//     can neither take the process down nor leave callers waiting forever.
package upstream

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"net/netip"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/net/dns/dnsmessage"

	"github.com/starriv/splitter/internal/wire"
)

// Observer receives per-exchange telemetry. The reason argument is empty on
// success and otherwise one of "timeout", "network", "mismatch", "refused", or
// "canceled".
// Implementations are called from multiple goroutines and must be safe for
// concurrent use.
type Observer interface {
	// ObserveExchange records one exchange attempt with its duration and
	// reason.
	ObserveExchange(group, server string, d time.Duration, reason string)
	// ObserveDropped records one UDP response discarded because its ID or
	// question did not match the query.
	ObserveDropped(group, server string)
	// ObserveRetransmit records one UDP retransmission.
	ObserveRetransmit(group, server string)
	// ObservePenalty records that a server was moved behind the healthy
	// servers (true) or that its failure record was cleared (false).
	ObservePenalty(group, server string, penalized bool)
	// ObserveCoalesced records one query that joined an identical query
	// already in flight instead of starting its own exchange.
	ObserveCoalesced(group string)
	// ObservePanic records one panic recovered inside a shared upstream
	// exchange. The waiters received an error instead of the process going
	// down.
	ObservePanic(group string)
}

// nopObserver discards every observation.
type nopObserver struct{}

func (nopObserver) ObserveExchange(string, string, time.Duration, string) {}
func (nopObserver) ObserveDropped(string, string)                         {}
func (nopObserver) ObserveRetransmit(string, string)                      {}
func (nopObserver) ObservePenalty(string, string, bool)                   {}
func (nopObserver) ObserveCoalesced(string)                               {}
func (nopObserver) ObservePanic(string)                                   {}

// errMismatch reports a response whose ID or question does not match the query.
var errMismatch = errors.New("response does not match query")

const (
	// penaltyThreshold is the number of consecutive failed attempts after
	// which a server is moved behind the healthy servers of its group.
	penaltyThreshold = 3
	// penaltyDuration is how long a failing server stays behind the healthy
	// servers before it is tried first again.
	penaltyDuration = 30 * time.Second
	// udpBufSize bounds a UDP response. Queries sent upstream advertise at
	// most wire.EDNSPayload (1232) bytes, so a legitimate answer fits with a
	// wide margin; a datagram that fills the buffer is treated as a failed
	// attempt instead of being parsed truncated.
	udpBufSize = 4096
)

// health is the failure record of one server. It is updated by concurrent
// exchanges, so every field is atomic.
type health struct {
	fails     atomic.Int32 // consecutive failed attempts
	downUntil atomic.Int64 // UnixNano until which the server is penalized; 0 = healthy
}

// Group is an ordered list of DNS servers that share a per-attempt timeout.
// Its configuration is immutable after construction; the per-server failure
// record and the in-flight exchange table change as queries run. A Group may be
// used concurrently. There is no explicit Close: a shared exchange ends when it
// completes or when its last caller leaves.
type Group struct {
	name    string
	servers []netip.AddrPort
	labels  []string
	timeout time.Duration
	obs     Observer
	health  []health
	now     func() time.Time // replaced in tests

	mu      sync.Mutex
	flights map[string]*flight // exchanges in progress, by flightKey
}

// flight is one upstream exchange shared by every identical query that arrives
// while it is in progress. resp and err are written once by the deferred
// function in run, before done is closed; waiters is guarded by Group.mu.
type flight struct {
	done    chan struct{}
	resp    []byte
	err     error
	waiters int
	cancel  context.CancelFunc
}

// flightKey identifies identical queries: the transport followed by the query
// bytes with the ID zeroed. Everything else that can change the answer (flags,
// EDNS payload size and options, the case of the name) is part of the bytes.
func flightKey(req []byte, proto wire.Proto) string {
	k := make([]byte, 1+len(req))
	k[0] = byte(proto)
	copy(k[1:], req)
	k[1], k[2] = 0, 0
	return string(k)
}

// NewGroup returns a group with the given name, ordered servers, and
// per-attempt timeout. A nil obs is replaced with an observer that discards
// every event.
func NewGroup(name string, servers []netip.AddrPort, timeout time.Duration, obs Observer) *Group {
	if obs == nil {
		obs = nopObserver{}
	}
	labels := make([]string, len(servers))
	for i, s := range servers {
		labels[i] = s.String()
	}
	return &Group{name: name, servers: servers, labels: labels, timeout: timeout, obs: obs,
		health: make([]health, len(servers)), now: time.Now, flights: map[string]*flight{}}
}

// Name returns the group name.
func (g *Group) Name() string { return g.name }

// Servers returns the configured server addresses formatted as strings, in the
// order they are tried.
func (g *Group) Servers() []string { return g.labels }

// Penalized reports whether the server at index i is currently moved behind
// the healthy servers of the group.
func (g *Group) Penalized(i int) bool {
	return g.health[i].downUntil.Load() > g.now().UnixNano()
}

// order returns the server indexes in the order this exchange tries them:
// healthy servers first, then penalized ones, each in configured order.
func (g *Group) order() []int {
	out := make([]int, 0, len(g.servers))
	var down []int
	for i := range g.servers {
		if g.Penalized(i) {
			down = append(down, i)
			continue
		}
		out = append(out, i)
	}
	return append(out, down...)
}

func (g *Group) markOK(i int) {
	h := &g.health[i]
	if h.fails.Swap(0) >= penaltyThreshold {
		h.downUntil.Store(0)
		g.obs.ObservePenalty(g.name, g.labels[i], false)
	}
}

func (g *Group) markFailed(i int) {
	h := &g.health[i]
	if h.fails.Add(1) < penaltyThreshold {
		return
	}
	was := h.downUntil.Swap(g.now().Add(penaltyDuration).UnixNano())
	if was == 0 {
		g.obs.ObservePenalty(g.name, g.labels[i], true)
	}
}

// Exchange sends req, a complete DNS message whose question is q, and returns
// the response with the caller's original ID restored. proto selects the
// transport, which is mirrored upstream. Servers are tried healthy first (see
// the package documentation) until one completes the exchange with an answer
// other than REFUSED; the attempt against each server is limited by the group
// timeout, and cancellation of ctx aborts the remaining attempts. When every
// server that answered refused, the last REFUSED answer is returned.
//
// A query identical to one already in flight (see flightKey) waits for that
// exchange instead of starting its own. Cancellation of ctx releases only this
// caller; the shared exchange is aborted when its last caller has gone.
func (g *Group) Exchange(ctx context.Context, req []byte, q dnsmessage.Question, proto wire.Proto) ([]byte, error) {
	if len(req) < wire.HeaderLen {
		return nil, wire.ErrNoHeader
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	key := flightKey(req, proto)
	g.mu.Lock()
	f, joined := g.flights[key]
	if joined {
		f.waiters++
	} else {
		// The exchange outlives any single caller, so it runs on a context that
		// keeps ctx's values but is canceled only through f.cancel.
		fctx, cancel := context.WithCancel(context.WithoutCancel(ctx))
		f = &flight{done: make(chan struct{}), waiters: 1, cancel: cancel}
		g.flights[key] = f
		// The exchange may still read the query after this caller has returned
		// and its buffer has been reused, so it works on a private copy.
		go g.run(fctx, f, key, append([]byte(nil), req...), q, proto)
	}
	g.mu.Unlock()
	// Registered before any observer call so a defective observer cannot leak
	// this caller's slot in the flight.
	defer g.leave(key, f)
	if joined {
		g.obs.ObserveCoalesced(g.name)
	}

	select {
	case <-f.done:
		if f.err != nil {
			return nil, f.err
		}
		resp := make([]byte, len(f.resp))
		copy(resp, f.resp)
		wire.SetID(resp, wire.ID(req))
		return resp, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// leave records that one caller stopped waiting on f. The last caller out
// removes the flight and aborts the exchange. The flight is unlisted before
// the cancel, so a query arriving now starts a fresh exchange instead of
// joining one that is being aborted.
func (g *Group) leave(key string, f *flight) {
	g.mu.Lock()
	if f.waiters--; f.waiters == 0 {
		if g.flights[key] == f {
			delete(g.flights, key)
		}
		f.cancel()
	}
	g.mu.Unlock()
}

// run performs the shared exchange of f and publishes its result. A panic in
// the exchange is recovered here and published as an error, so every caller
// returns instead of waiting forever on a done channel that is never closed.
func (g *Group) run(ctx context.Context, f *flight, key string, req []byte, q dnsmessage.Question, proto wire.Proto) {
	var (
		resp []byte
		err  error
	)
	defer func() {
		r := recover()
		if r != nil {
			resp, err = nil, fmt.Errorf("panic in upstream exchange: %v", r)
		}
		g.mu.Lock()
		if g.flights[key] == f {
			delete(g.flights, key)
		}
		g.mu.Unlock()
		f.cancel()
		f.resp, f.err = resp, err
		close(f.done)
		if r != nil {
			// Reported after the waiters are released: an observer defect must
			// not be able to keep them from returning.
			g.obs.ObservePanic(g.name)
		}
	}()
	resp, err = g.exchange(ctx, req, q, proto)
}

// exchange tries the servers of the group in order for one query; see
// Exchange.
func (g *Group) exchange(ctx context.Context, req []byte, q dnsmessage.Question, proto wire.Proto) ([]byte, error) {
	origID := wire.ID(req)
	var (
		errs    []error
		refused []byte
	)
	for _, i := range g.order() {
		if ctx.Err() != nil {
			break
		}
		start := time.Now()
		resp, err := g.exchangeOne(ctx, i, req, q, proto)
		why := reason(ctx, err)
		if err == nil && wire.RCode(resp) == int(dnsmessage.RCodeRefused) {
			why = "refused"
		}
		g.obs.ObserveExchange(g.name, g.labels[i], time.Since(start), why)
		switch why {
		case "":
			g.markOK(i)
			wire.SetID(resp, origID)
			return resp, nil
		case "refused":
			g.markFailed(i)
			refused = resp
			errs = append(errs, fmt.Errorf("%s: REFUSED", g.labels[i]))
		case "canceled":
			// The caller gave up; that says nothing about the server.
			errs = append(errs, fmt.Errorf("%s: %w", g.labels[i], err))
		default:
			g.markFailed(i)
			errs = append(errs, fmt.Errorf("%s: %w", g.labels[i], err))
		}
	}
	if refused != nil && ctx.Err() == nil {
		wire.SetID(refused, origID)
		return refused, nil
	}
	if len(errs) == 0 {
		return nil, ctx.Err()
	}
	return nil, errors.Join(errs...)
}

// reason classifies an exchange error for telemetry. A response mismatch is
// reported as "mismatch", caller cancellation as "canceled", and deadline
// expiry as "timeout"; every other error is reported as "network".
func reason(ctx context.Context, err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, errMismatch):
		return "mismatch"
	case errors.Is(ctx.Err(), context.Canceled):
		return "canceled"
	case errors.Is(err, os.ErrDeadlineExceeded), errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	}
	return "network"
}

// bufPool recycles UDP receive buffers so that an exchange does not allocate a
// buffer for every attempt.
var bufPool = sync.Pool{New: func() any { b := make([]byte, udpBufSize); return &b }}

// errOversize reports a UDP datagram that filled the receive buffer, so its
// tail may have been cut off by the kernel.
var errOversize = fmt.Errorf("udp response of %d bytes or more", udpBufSize)

func (g *Group) exchangeOne(ctx context.Context, i int, req []byte, q dnsmessage.Question, proto wire.Proto) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, g.timeout)
	defer cancel()
	deadline, _ := ctx.Deadline()

	out := make([]byte, len(req))
	copy(out, req)
	id := uint16(rand.Uint32())
	wire.SetID(out, id)
	// Clamp the EDNS UDP payload size offered upstream to the DNS Flag Day
	// 2020 recommendation of 1232 bytes. The client's own advertisement is
	// retained for the reply sent back, but requesting a larger UDP payload
	// upstream risks a fragmented response; a lost fragment becomes a timeout
	// and a SERVFAIL without TC, so the client never retries over TCP.
	if proto != wire.TCP {
		wire.ClampEDNS(out, wire.EDNSPayload)
	}

	network := "udp"
	if proto == wire.TCP {
		network = "tcp"
	}
	var d net.Dialer
	conn, err := d.DialContext(ctx, network, g.servers[i].String())
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	if err := conn.SetDeadline(deadline); err != nil {
		return nil, err
	}
	// Cancellation of the caller (shutdown) unblocks reads immediately.
	stop := context.AfterFunc(ctx, func() { _ = conn.SetDeadline(time.Now()) })
	defer stop()

	if proto == wire.TCP {
		return g.tcp(conn, out, id, q)
	}
	return g.udp(ctx, conn, out, id, q, i, deadline)
}

func (g *Group) udp(ctx context.Context, conn net.Conn, out []byte, id uint16, q dnsmessage.Question, i int, deadline time.Time) ([]byte, error) {
	if _, err := conn.Write(out); err != nil {
		return nil, err
	}
	// First wait: until the retransmit point. Setting a deadline here could
	// overwrite the "now" deadline installed by the cancellation hook, so the
	// context is re-checked after every deadline that this function sets.
	retransmitted := false
	if err := conn.SetReadDeadline(time.Now().Add(g.timeout / 2)); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	bp := bufPool.Get().(*[]byte)
	defer bufPool.Put(bp)
	buf := *bp
	for {
		n, err := conn.Read(buf)
		if err != nil {
			if retransmitted || !errors.Is(err, os.ErrDeadlineExceeded) || ctx.Err() != nil {
				return nil, err
			}
			retransmitted = true
			if err := conn.SetDeadline(deadline); err != nil {
				return nil, err
			}
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			if _, err := conn.Write(out); err != nil {
				return nil, err
			}
			g.obs.ObserveRetransmit(g.name, g.labels[i])
			continue
		}
		if n >= len(buf) {
			return nil, errOversize
		}
		if !wire.ResponseMatches(buf[:n], id, q) {
			g.obs.ObserveDropped(g.name, g.labels[i])
			continue
		}
		resp := make([]byte, n)
		copy(resp, buf[:n])
		return resp, nil
	}
}

// tcp performs one exchange over a length-prefixed TCP stream and applies the
// same ID and question validation as the UDP path.
func (g *Group) tcp(conn net.Conn, out []byte, id uint16, q dnsmessage.Question) ([]byte, error) {
	msg := make([]byte, 2+len(out))
	binary.BigEndian.PutUint16(msg, uint16(len(out)))
	copy(msg[2:], out)
	if _, err := conn.Write(msg); err != nil {
		return nil, err
	}
	var lb [2]byte
	if _, err := io.ReadFull(conn, lb[:]); err != nil {
		return nil, err
	}
	n := int(binary.BigEndian.Uint16(lb[:]))
	if n < wire.HeaderLen {
		return nil, fmt.Errorf("%w: %d-byte message", errMismatch, n)
	}
	resp := make([]byte, n)
	if _, err := io.ReadFull(conn, resp); err != nil {
		return nil, err
	}
	if !wire.ResponseMatches(resp, id, q) {
		return nil, errMismatch
	}
	return resp, nil
}
