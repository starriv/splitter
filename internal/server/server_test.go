package server

import (
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"net"
	"net/netip"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/starriv/splitter/internal/dnstest"
	"github.com/starriv/splitter/internal/wire"
)

// echo replies with the request itself after an optional delay, so that a test
// can identify which request produced a reply.
type echo struct {
	delay   time.Duration
	started chan struct{}
	calls   atomic.Int64
}

func (e *echo) ServeDNS(ctx context.Context, req []byte, _ wire.Proto) []byte {
	e.calls.Add(1)
	if e.started != nil {
		select {
		case e.started <- struct{}{}:
		default:
		}
	}
	select {
	case <-time.After(e.delay):
	case <-ctx.Done():
	}
	return req
}

// counter records server observations for assertions.
type counter struct {
	panics   atomic.Int64
	overload atomic.Int64
	conns    atomic.Int64
	writeErr atomic.Int64
}

func (c *counter) Inflight(float64)   {}
func (c *counter) TCPConns(d float64) { c.conns.Add(int64(d)) }
func (c *counter) WriteFailed(string) { c.writeErr.Add(1) }
func (c *counter) Overload(string)    { c.overload.Add(1) }
func (c *counter) Panic()             { c.panics.Add(1) }

// startServer binds a server to a loopback ephemeral port, starts the accept
// loops, and shuts the server down at test cleanup.
func startServer(t *testing.T, h Handler, opt Options) *Server {
	t.Helper()
	s := New(h, opt)
	if err := s.Listen([]netip.AddrPort{netip.MustParseAddrPort("127.0.0.1:0")}); err != nil {
		t.Fatal(err)
	}
	s.Serve()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = s.Shutdown(ctx)
	})
	return s
}

// msg returns a minimal DNS header of wire.HeaderLen bytes with the given ID.
func msg(id uint16) []byte {
	m := make([]byte, 12)
	binary.BigEndian.PutUint16(m, id)
	return m
}

// writeTCP sends one length-prefixed message and fails the test on error.
func writeTCP(t *testing.T, c net.Conn, m []byte) {
	t.Helper()
	if err := writeTCPErr(c, m); err != nil {
		t.Fatal(err)
	}
}

// writeTCPErr sends one length-prefixed message and returns the write error.
func writeTCPErr(c net.Conn, m []byte) error {
	_, err := c.Write(append(binary.BigEndian.AppendUint16(nil, uint16(len(m))), m...))
	return err
}

// readTCP reads one length-prefixed message and fails the test on error.
func readTCP(t *testing.T, c net.Conn) []byte {
	t.Helper()
	var lb [2]byte
	if _, err := io.ReadFull(c, lb[:]); err != nil {
		t.Fatal(err)
	}
	b := make([]byte, binary.BigEndian.Uint16(lb[:]))
	if _, err := io.ReadFull(c, b); err != nil {
		t.Fatal(err)
	}
	return b
}

// TestTCPPipelinedQueriesAnsweredInOrder verifies that pipelined queries on one
// connection are answered in the order in which they were received.
func TestTCPPipelinedQueriesAnsweredInOrder(t *testing.T) {
	s := startServer(t, &echo{}, Options{})
	c, err := net.Dial("tcp", s.Addrs()[0].String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	for id := uint16(1); id <= 5; id++ {
		writeTCP(t, c, msg(id))
	}
	_ = c.SetDeadline(time.Now().Add(2 * time.Second))
	for id := uint16(1); id <= 5; id++ {
		if got := binary.BigEndian.Uint16(readTCP(t, c)); got != id {
			t.Fatalf("reply %d, want %d", got, id)
		}
	}
}

// TestTCPIdleTimeoutClosesConnection verifies that a client connection that
// stays idle longer than TCPIdleTimeout is closed.
func TestTCPIdleTimeoutClosesConnection(t *testing.T) {
	s := startServer(t, &echo{}, Options{TCPIdleTimeout: 100 * time.Millisecond})
	c, err := net.Dial("tcp", s.Addrs()[0].String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := c.Read(make([]byte, 1)); err == nil {
		t.Fatal("idle connection was not closed")
	}
}

// TestUDPOverloadDrops verifies that when MaxInflight is reached, a new UDP
// query is dropped and counted as overload while the in-flight query still
// receives its reply.
func TestUDPOverloadDrops(t *testing.T) {
	h := &echo{delay: 300 * time.Millisecond, started: make(chan struct{}, 1)}
	obs := &counter{}
	s := startServer(t, h, Options{MaxInflight: 1, Observer: obs})
	c, err := net.Dial("udp", s.Addrs()[0].String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_, _ = c.Write(msg(1))
	<-h.started
	_, _ = c.Write(msg(2)) // arrives while the only slot is busy
	_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 512)
	n, err := c.Read(buf)
	if err != nil || binary.BigEndian.Uint16(buf[:n]) != 1 {
		t.Fatalf("first reply: %v", err)
	}
	if obs.overload.Load() != 1 || h.calls.Load() != 1 {
		t.Fatalf("overload=%d calls=%d", obs.overload.Load(), h.calls.Load())
	}
}

// TestShortUDPPacketsIgnored verifies that packets shorter than a DNS header are
// ignored and do not reach the handler.
func TestShortUDPPacketsIgnored(t *testing.T) {
	h := &echo{}
	s := startServer(t, h, Options{})
	c, _ := net.Dial("udp", s.Addrs()[0].String())
	defer c.Close()
	_, _ = c.Write([]byte{1, 2, 3})
	_, _ = c.Write(msg(9))
	_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 512)
	if n, err := c.Read(buf); err != nil || binary.BigEndian.Uint16(buf[:n]) != 9 {
		t.Fatalf("reply: %v", err)
	}
	if h.calls.Load() != 1 {
		t.Fatalf("handler saw %d messages, want 1", h.calls.Load())
	}
}

// TestShutdownDrainsInflight verifies that a query in flight when shutdown
// starts still receives its reply.
func TestShutdownDrainsInflight(t *testing.T) {
	h := &echo{delay: 200 * time.Millisecond, started: make(chan struct{}, 1)}
	s := New(h, Options{})
	if err := s.Listen([]netip.AddrPort{netip.MustParseAddrPort("127.0.0.1:0")}); err != nil {
		t.Fatal(err)
	}
	s.Serve()
	c, _ := net.Dial("udp", s.Addrs()[0].String())
	defer c.Close()
	_, _ = c.Write(msg(7))
	<-h.started
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := s.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	_ = c.SetReadDeadline(time.Now().Add(time.Second))
	buf := make([]byte, 512)
	if n, err := c.Read(buf); err != nil || binary.BigEndian.Uint16(buf[:n]) != 7 {
		t.Fatalf("in-flight reply lost: %v", err)
	}
}

// TestShutdownWithOpenTCPConnections verifies that Shutdown does not wait for
// idle client TCP connections to expire: a front-end resolver may keep
// connections open, and an orchestrator typically allows only a few seconds for
// a graceful stop.
func TestShutdownWithOpenTCPConnections(t *testing.T) {
	h := &echo{delay: 100 * time.Millisecond, started: make(chan struct{}, 4)}
	s := New(h, Options{TCPIdleTimeout: 30 * time.Second})
	if err := s.Listen([]netip.AddrPort{netip.MustParseAddrPort("127.0.0.1:0")}); err != nil {
		t.Fatal(err)
	}
	s.Serve()
	var conns []net.Conn
	for range 3 {
		c, err := net.Dial("tcp", s.Addrs()[0].String())
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		conns = append(conns, c)
	}
	// One connection is mid-query when shutdown starts; the others are idle.
	writeTCP(t, conns[0], msg(1))
	<-h.started

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_ = conns[0].SetReadDeadline(time.Now().Add(2 * time.Second))
		if got := binary.BigEndian.Uint16(readTCP(t, conns[0])); got != 1 {
			t.Errorf("reply %d", got)
		}
	}()
	start := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := s.Shutdown(ctx); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
	if d := time.Since(start); d > time.Second {
		t.Fatalf("shutdown took %s", d)
	}
	wg.Wait()
}

// TestShutdownForcedReturnsPromptly verifies that Shutdown aborts the remaining
// work and returns promptly once the grace period expires.
func TestShutdownForcedReturnsPromptly(t *testing.T) {
	h := &echo{delay: time.Hour, started: make(chan struct{}, 1)}
	s := New(h, Options{TCPIdleTimeout: 30 * time.Second})
	if err := s.Listen([]netip.AddrPort{netip.MustParseAddrPort("127.0.0.1:0")}); err != nil {
		t.Fatal(err)
	}
	s.Serve()
	c, _ := net.Dial("tcp", s.Addrs()[0].String())
	defer c.Close()
	writeTCP(t, c, msg(1))
	<-h.started
	start := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_ = s.Shutdown(ctx)
	if d := time.Since(start); d > time.Second {
		t.Fatalf("forced shutdown took %s", d)
	}
}

// TestListenFailureReleasesSockets verifies that a failure to bind TCP releases
// the UDP socket already bound for the same address.
func TestListenFailureReleasesSockets(t *testing.T) {
	taken, err := net.ListenTCP("tcp", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer taken.Close()
	port := taken.Addr().(*net.TCPAddr).Port
	s := New(&echo{}, Options{})
	ap := netip.AddrPortFrom(netip.MustParseAddr("127.0.0.1"), uint16(port))
	if err := s.Listen([]netip.AddrPort{ap}); err == nil {
		t.Fatal("expected TCP bind failure")
	}
	// The UDP socket bound before the failure must have been released.
	u, err := net.ListenUDP("udp", net.UDPAddrFromAddrPort(ap))
	if err != nil {
		t.Fatalf("udp port still held: %v", err)
	}
	u.Close()
}

// TestTCPConnCapSheds verifies that a flood of idle keep-alive connections is
// bounded by MaxTCPConns: connections beyond the limit are shed immediately
// with an RST instead of consuming a goroutine and a file descriptor each until
// the process runs out of resources.
func TestTCPConnCapSheds(t *testing.T) {
	obs := &counter{}
	s := startServer(t, &echo{}, Options{MaxTCPConns: 2, Observer: obs})
	addr := s.Addrs()[0].String()

	var held []net.Conn
	defer func() {
		for _, c := range held {
			_ = c.Close()
		}
	}()
	for i := 0; i < 2; i++ {
		c, err := net.Dial("tcp", addr)
		if err != nil {
			t.Fatal(err)
		}
		held = append(held, c)
	}
	deadline := time.Now().Add(time.Second)
	for obs.conns.Load() != 2 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if got := obs.conns.Load(); got != 2 {
		t.Fatalf("open connections = %d, want 2", got)
	}

	// The third connection is accepted by the kernel and then reset.
	c, err := net.Dial("tcp", addr)
	if err != nil {
		return // kernel refused outright: also acceptable
	}
	defer c.Close()
	var lb [2]byte
	_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
	// The shed path sends an RST (linger 0). Depending on scheduling the RST
	// can arrive before this write, so the write itself may fail with EPIPE or
	// ECONNRESET; that is the same "not served" outcome as a failed read. Only
	// a well-formed response would mean the limit was bypassed.
	if err := writeTCPErr(c, msg(1)); err == nil {
		if _, err := io.ReadFull(c, lb[:]); err == nil {
			t.Fatal("connection beyond max_tcp_conns was served")
		}
	}
	if obs.overload.Load() == 0 {
		t.Fatal("shed connection was not counted as overload")
	}
	// The shed path must not leak a slot: closing a held connection must let a
	// new one in.
	_ = held[0].Close()
	held = held[1:]
	deadline = time.Now().Add(2 * time.Second)
	for {
		c2, err := net.Dial("tcp", addr)
		if err != nil {
			t.Fatal(err)
		}
		writeTCP(t, c2, msg(2))
		_ = c2.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
		if _, err := io.ReadFull(c2, lb[:]); err == nil {
			_ = c2.Close()
			break
		}
		_ = c2.Close()
		if time.Now().After(deadline) {
			t.Fatal("a freed connection slot was never reused")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// panicky panics for requests whose first byte is 0xFF and echoes the request
// otherwise.
type panicky struct{}

func (panicky) ServeDNS(_ context.Context, req []byte, _ wire.Proto) []byte {
	if req[0] == 0xFF {
		panic("boom")
	}
	return req
}

// TestPanicIsContained verifies that a handler panic costs only the affected
// query: the panic is counted, no response is sent, and the listeners keep
// answering.
func TestPanicIsContained(t *testing.T) {
	obs := &counter{}
	s := startServer(t, panicky{}, Options{Observer: obs})
	addr := s.Addrs()[0]

	u, err := net.Dial("udp", addr.String())
	if err != nil {
		t.Fatal(err)
	}
	defer u.Close()
	if _, err := u.Write(msg(0xFF01)); err != nil {
		t.Fatal(err)
	}
	_ = u.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
	if n, err := u.Read(make([]byte, 64)); err == nil {
		t.Fatalf("got %d bytes back for a query whose handler panicked", n)
	}

	c, err := net.Dial("tcp", addr.String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	writeTCP(t, c, msg(0xFF02))
	_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := c.Read(make([]byte, 1)); err != io.EOF {
		t.Errorf("tcp after panic: %v, want the connection closed", err)
	}
	if got := obs.panics.Load(); got != 2 {
		t.Errorf("panics counted = %d, want 2", got)
	}
	if got := obs.conns.Load(); got != 0 {
		t.Errorf("tcp connection slots leaked: %d", got)
	}

	for _, network := range []string{"udp", "tcp"} {
		if got := dnstest.ExchangeRaw(t, network, addr, msg(0x0102)); !bytes.Equal(got, msg(0x0102)) {
			t.Errorf("%s after a panic: %x", network, got)
		}
	}
}
