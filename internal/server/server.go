// Package server runs the UDP and TCP listeners and passes each query to a
// Handler. The package is independent of routing: it moves bytes, bounds
// concurrency, and shuts down gracefully.
package server

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"runtime/debug"
	"sync"
	"sync/atomic"
	"time"

	"github.com/starriv/splitter/internal/wire"
)

// Handler answers one query. Returning nil instructs the server to send no
// response. The context is canceled when the server shuts down.
type Handler interface {
	ServeDNS(ctx context.Context, req []byte, proto wire.Proto) []byte
}

// Observer receives server-level telemetry. Implementations are called from
// multiple goroutines and must be safe for concurrent use.
type Observer interface {
	// Inflight records the change in the number of queries being processed.
	Inflight(delta float64)
	// TCPConns records the change in the number of open client TCP
	// connections.
	TCPConns(delta float64)
	// Overload records one query dropped because a concurrency limit was
	// reached.
	Overload(proto string)
	// WriteFailed records one response that could not be written to the
	// client.
	WriteFailed(proto string)
	// Panic records one panic in a query handler.
	Panic()
}

// nopObserver discards every observation.
type nopObserver struct{}

func (nopObserver) Inflight(float64)   {}
func (nopObserver) TCPConns(float64)   {}
func (nopObserver) Overload(string)    {}
func (nopObserver) WriteFailed(string) {}
func (nopObserver) Panic()             {}

// Options configures a Server. Zero or negative limits are replaced with
// defaults.
type Options struct {
	MaxInflight    int           // concurrent queries; beyond this new queries are dropped
	MaxTCPConns    int           // client TCP connections kept open at once
	TCPIdleTimeout time.Duration // idle time before a client TCP connection is closed
	Logger         *slog.Logger  // logger for server events; nil discards them
	Observer       Observer      // telemetry receiver; nil disables telemetry
}

// Server owns the bound listeners, the concurrency limits, and the lifecycle of
// query handling. Create one with New, bind addresses with Listen, start the
// accept loops with Serve, and stop with Shutdown.
type Server struct {
	h    Handler
	opt  Options
	sem  chan struct{} // in-flight queries
	cns  chan struct{} // open client TCP connections
	log  *slog.Logger
	obs  Observer
	udp  []*net.UDPConn
	tcp  []*net.TCPListener
	addr []netip.AddrPort

	ctx    context.Context // canceled to abort in-flight upstream exchanges
	cancel context.CancelFunc
	wg     sync.WaitGroup

	// closing is set under mu when Shutdown starts. TCP connections re-arm
	// their read deadline under mu after checking it, so the "deadline = now"
	// installed by Shutdown can never be overwritten by a longer idle deadline.
	closing atomic.Bool
	mu      sync.Mutex
	conns   map[net.Conn]struct{}
}

// New returns a server that calls h for every query. Zero or negative values in
// opt are replaced with defaults.
func New(h Handler, opt Options) *Server {
	if opt.MaxInflight <= 0 {
		opt.MaxInflight = 1024
	}
	if opt.MaxTCPConns <= 0 {
		opt.MaxTCPConns = 256
	}
	if opt.TCPIdleTimeout <= 0 {
		opt.TCPIdleTimeout = 10 * time.Second
	}
	if opt.Logger == nil {
		opt.Logger = slog.New(slog.DiscardHandler)
	}
	if opt.Observer == nil {
		opt.Observer = nopObserver{}
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &Server{
		h: h, opt: opt, sem: make(chan struct{}, opt.MaxInflight), cns: make(chan struct{}, opt.MaxTCPConns),
		log: opt.Logger, obs: opt.Observer, ctx: ctx, cancel: cancel,
		conns: map[net.Conn]struct{}{},
	}
}

// Listen binds UDP and TCP on every address in addrs. A port of 0 selects a
// free UDP port and binds TCP on the same number (used by tests). On error,
// every listener already bound is closed.
func (s *Server) Listen(addrs []netip.AddrPort) (err error) {
	defer func() {
		if err != nil {
			s.closeListeners()
		}
	}()
	for _, ap := range addrs {
		u, t, bound, err := listenPair(ap)
		if err != nil {
			return err
		}
		s.udp = append(s.udp, u)
		s.tcp = append(s.tcp, t)
		s.addr = append(s.addr, bound)
	}
	return nil
}

// listenPair binds UDP and TCP on the same address. With port 0 the kernel
// selects the UDP port, and that number may already be in use for TCP on a busy
// host, so the attempt is retried with a fresh port.
func listenPair(ap netip.AddrPort) (*net.UDPConn, *net.TCPListener, netip.AddrPort, error) {
	for attempt := 0; ; attempt++ {
		u, err := net.ListenUDP("udp", net.UDPAddrFromAddrPort(ap))
		if err != nil {
			return nil, nil, ap, fmt.Errorf("listen udp %s: %w", ap, err)
		}
		bound := netip.AddrPortFrom(ap.Addr(), u.LocalAddr().(*net.UDPAddr).AddrPort().Port())
		t, err := net.ListenTCP("tcp", net.TCPAddrFromAddrPort(bound))
		if err == nil {
			return u, t, bound, nil
		}
		_ = u.Close()
		if ap.Port() != 0 || attempt == 20 {
			return nil, nil, ap, fmt.Errorf("listen tcp %s: %w", bound, err)
		}
	}
}

// Addrs returns the bound addresses. Each address serves both UDP and TCP.
func (s *Server) Addrs() []netip.AddrPort { return s.addr }

// Serve starts the accept loops and returns immediately.
func (s *Server) Serve() {
	for _, u := range s.udp {
		s.wg.Add(1)
		go s.serveUDP(u)
	}
	for _, t := range s.tcp {
		s.wg.Add(1)
		go s.serveTCP(t)
	}
}

// Shutdown stops accepting queries, allows in-flight queries to finish until
// ctx is done, then aborts the remainder.
//
// UDP sockets stay open until the in-flight handlers have written their
// replies, because closing them first would drop every reply in flight. Their
// read loops are stopped by setting a read deadline in the past instead.
func (s *Server) Shutdown(ctx context.Context) error {
	s.mu.Lock()
	s.closing.Store(true)
	for c := range s.conns {
		_ = c.SetReadDeadline(time.Now()) // wake idle readers; in-flight replies still go out
	}
	s.mu.Unlock()
	for _, t := range s.tcp {
		_ = t.Close()
	}
	for _, u := range s.udp {
		_ = u.SetReadDeadline(time.Now())
	}
	done := make(chan struct{})
	go func() { s.wg.Wait(); close(done) }()
	var err error
	select {
	case <-done:
	case <-ctx.Done():
		err = ctx.Err()
		s.cancel() // abort upstream exchanges
		s.mu.Lock()
		for c := range s.conns {
			_ = c.Close() // unblock writers stuck on a slow client
		}
		s.mu.Unlock()
		<-done
	}
	s.cancel()
	for _, u := range s.udp {
		_ = u.Close()
	}
	return err
}

func (s *Server) closeListeners() {
	for _, u := range s.udp {
		_ = u.Close()
	}
	for _, t := range s.tcp {
		_ = t.Close()
	}
}

func (s *Server) acquire(proto string) bool {
	select {
	case s.sem <- struct{}{}:
		s.obs.Inflight(1)
		return true
	default:
		s.obs.Overload(proto)
		return false
	}
}

func (s *Server) release() {
	<-s.sem
	s.obs.Inflight(-1)
}

// acquireConn reserves a server-side slot for a client TCP connection. The
// connection semaphore is separate from the query semaphore: an idle keep-alive
// connection holds no query slot but still consumes a goroutine and a file
// descriptor, so it must be bounded independently or a slow client can exhaust
// file descriptors and take the admin endpoints down with it.
func (s *Server) acquireConn() bool {
	select {
	case s.cns <- struct{}{}:
		s.obs.TCPConns(1)
		return true
	default:
		return false
	}
}

func (s *Server) releaseConn() {
	<-s.cns
	s.obs.TCPConns(-1)
}

// handle runs the handler for one query and contains a panic to that query. A
// single packet that triggers a defect must cost one answer, not the resolver
// process. ok is false after a panic: nothing is sent, and a TCP connection is
// closed because its stream position can no longer be trusted.
func (s *Server) handle(req []byte, proto wire.Proto) (resp []byte, ok bool) {
	defer func() {
		if r := recover(); r != nil {
			s.obs.Panic()
			s.log.Error("panic in query handler; query dropped", "proto", proto.String(),
				"panic", r, "stack", string(debug.Stack()))
			resp, ok = nil, false
		}
	}()
	return s.h.ServeDNS(s.ctx, req, proto), true
}

func (s *Server) serveUDP(c *net.UDPConn) {
	defer s.wg.Done()
	buf := make([]byte, 65535)
	for {
		n, from, err := c.ReadFromUDPAddrPort(buf)
		if err != nil {
			if s.closing.Load() || errors.Is(err, net.ErrClosed) {
				return
			}
			s.log.Warn("udp read", "err", err)
			time.Sleep(10 * time.Millisecond) // avoid a busy loop on a persistent error
			continue
		}
		if n < wire.HeaderLen || !s.acquire("udp") {
			continue
		}
		req := make([]byte, n)
		copy(req, buf[:n])
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			defer s.release()
			if resp, _ := s.handle(req, wire.UDP); resp != nil {
				if _, err := c.WriteToUDPAddrPort(resp, from); err != nil && !errors.Is(err, net.ErrClosed) {
					// A reply that does not reach the client is counted; an
					// answer that exceeds the path MTU fails at this point.
					s.obs.WriteFailed("udp")
					s.log.Debug("udp write", "to", from, "err", err)
				}
			}
		}()
	}
}

func (s *Server) serveTCP(l *net.TCPListener) {
	defer s.wg.Done()
	for {
		c, err := l.AcceptTCP()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			s.log.Warn("tcp accept", "err", err)
			time.Sleep(10 * time.Millisecond)
			continue
		}
		if !s.acquireConn() {
			// Shed immediately without starting a goroutine: linger 0 sends an
			// RST so the client retries at once instead of waiting for a
			// timeout.
			s.obs.Overload("tcp")
			_ = c.SetLinger(0)
			_ = c.Close()
			continue
		}
		s.mu.Lock()
		s.conns[c] = struct{}{}
		s.mu.Unlock()
		s.wg.Add(1)
		go s.serveConn(c)
	}
}

// serveConn handles queries on one connection sequentially. RFC 7766 permits
// pipelining, and answering pipelined queries in order is a valid subset of
// that behaviour.
func (s *Server) serveConn(c *net.TCPConn) {
	defer s.wg.Done()
	defer s.releaseConn()
	defer func() {
		s.mu.Lock()
		delete(s.conns, c)
		s.mu.Unlock()
		_ = c.Close()
	}()
	var lb [2]byte
	for {
		s.mu.Lock()
		if s.closing.Load() {
			s.mu.Unlock()
			return
		}
		_ = c.SetReadDeadline(time.Now().Add(s.opt.TCPIdleTimeout))
		s.mu.Unlock()
		if _, err := io.ReadFull(c, lb[:]); err != nil {
			return
		}
		n := int(binary.BigEndian.Uint16(lb[:]))
		if n < wire.HeaderLen {
			return
		}
		req := make([]byte, n)
		if _, err := io.ReadFull(c, req); err != nil {
			return
		}
		if !s.acquire("tcp") {
			return
		}
		resp, ok := s.handle(req, wire.TCP)
		s.release()
		if !ok {
			return
		}
		if resp == nil {
			// Nothing to send for this query (a response-shaped packet, for
			// example), but the connection and the queries pipelined behind it
			// are still valid.
			continue
		}
		out := make([]byte, 2+len(resp))
		binary.BigEndian.PutUint16(out, uint16(len(resp)))
		copy(out[2:], resp)
		_ = c.SetWriteDeadline(time.Now().Add(s.opt.TCPIdleTimeout))
		if _, err := c.Write(out); err != nil {
			if !errors.Is(err, net.ErrClosed) {
				s.obs.WriteFailed("tcp")
			}
			return
		}
	}
}
