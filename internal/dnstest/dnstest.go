// Package dnstest provides a fake upstream DNS server and a minimal DNS client
// for tests.
//
// Upstream answers queries over UDP and TCP on the same port and can simulate
// packet loss, forged IDs, truncation and unknown EDNS options, so the
// resolver transport can be tested without external services. Started
// resources are released through testing.TB cleanup.
package dnstest

import (
	"encoding/binary"
	"io"
	"net"
	"net/netip"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/net/dns/dnsmessage"
)

// Upstream is a fake DNS server: it answers A queries with A and AAAA queries
// with AAAA when configured, over UDP and TCP on the same port.
type Upstream struct {
	Addr    netip.AddrPort
	A       netip.Addr
	AAAA    netip.Addr   // answered for AAAA queries when valid
	Queries atomic.Int64 // queries received, both transports

	// Forge makes the next UDP reply be preceded by a reply with a wrong ID.
	Forge atomic.Bool
	// DropUDP discards that many UDP queries without answering, simulating
	// packet loss.
	DropUDP atomic.Int64
	// TruncateUDP sets the TC bit on UDP replies and omits the answers.
	TruncateUDP atomic.Bool
	// Refuse answers every query with REFUSED and no records.
	Refuse atomic.Bool
	// Extra adds an EDNS option (code 65001) and a record of unknown type, so
	// tests can verify that forwarded replies reach the client byte for byte.
	Extra atomic.Bool
	// Last holds the last reply sent, after Forge and TruncateUDP handling.
	Last atomic.Pointer[[]byte]
	// LastQuery holds the last query received, before answering.
	LastQuery atomic.Pointer[[]byte]

	udp *net.UDPConn
	tcp *net.TCPListener
}

// StartUpstream starts a fake upstream on 127.0.0.1; it stops with the
// test.
func StartUpstream(t testing.TB, a netip.Addr) *Upstream { return StartUpstreamAt(t, 0, a) }

// StartUpstreamAt starts a fake upstream on a fixed port; 0 selects any free
// port.
func StartUpstreamAt(t testing.TB, fixed int, a netip.Addr) *Upstream {
	t.Helper()
	u := &Upstream{A: a}
	for i := 0; ; i++ {
		uc, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: fixed})
		if err != nil {
			t.Fatal(err)
		}
		port := uc.LocalAddr().(*net.UDPAddr).Port
		tl, err := net.ListenTCP("tcp", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: port})
		if err != nil {
			uc.Close()
			if i < 10 && fixed == 0 {
				continue
			}
			t.Fatal(err)
		}
		u.udp, u.tcp = uc, tl
		u.Addr = netip.AddrPortFrom(netip.MustParseAddr("127.0.0.1"), uint16(port))
		break
	}
	go u.serveUDP()
	go u.serveTCP()
	t.Cleanup(func() { u.udp.Close(); u.tcp.Close() })
	return u
}

func (u *Upstream) answer(req []byte, truncate bool) []byte {
	var m dnsmessage.Message
	if err := m.Unpack(req); err != nil || len(m.Questions) != 1 {
		return nil
	}
	q := m.Questions[0]
	m.Header.Response, m.Header.RecursionAvailable = true, true
	m.Answers, m.Authorities = nil, nil
	if u.Refuse.Load() {
		m.Header.RCode = dnsmessage.RCodeRefused
	} else if truncate {
		m.Header.Truncated = true
	} else if q.Type == dnsmessage.TypeA {
		m.Answers = []dnsmessage.Resource{{
			Header: dnsmessage.ResourceHeader{Name: q.Name, Type: dnsmessage.TypeA, Class: dnsmessage.ClassINET, TTL: 300},
			Body:   &dnsmessage.AResource{A: u.A.As4()},
		}}
	} else if q.Type == dnsmessage.TypeAAAA && u.AAAA.IsValid() {
		m.Answers = []dnsmessage.Resource{{
			Header: dnsmessage.ResourceHeader{Name: q.Name, Type: dnsmessage.TypeAAAA, Class: dnsmessage.ClassINET, TTL: 300},
			Body:   &dnsmessage.AAAAResource{AAAA: u.AAAA.As16()},
		}}
	}
	if u.Extra.Load() {
		m.Answers = append(m.Answers, dnsmessage.Resource{
			Header: dnsmessage.ResourceHeader{Name: q.Name, Type: 65280, Class: dnsmessage.ClassINET, TTL: 42},
			Body:   &dnsmessage.UnknownResource{Type: 65280, Data: []byte{0xde, 0xad, 0xbe, 0xef}},
		})
		for i, r := range m.Additionals {
			if opt, ok := r.Body.(*dnsmessage.OPTResource); ok {
				opt.Options = append(opt.Options, dnsmessage.Option{Code: 65001, Data: []byte("splitter-passthrough")})
				m.Additionals[i].Body = opt
			}
		}
	}
	out, err := m.Pack()
	if err != nil {
		return nil
	}
	u.Last.Store(&out)
	return out
}

func (u *Upstream) serveUDP() {
	buf := make([]byte, 65535)
	for {
		n, from, err := u.udp.ReadFromUDPAddrPort(buf)
		if err != nil {
			return
		}
		u.Queries.Add(1)
		clone := append([]byte(nil), buf[:n]...)
		u.LastQuery.Store(&clone)
		if u.DropUDP.Load() > 0 && u.DropUDP.Add(-1) >= 0 {
			continue
		}
		resp := u.answer(buf[:n], u.TruncateUDP.Load())
		if resp == nil {
			continue
		}
		if u.Forge.CompareAndSwap(true, false) {
			forged := append([]byte(nil), resp...)
			binary.BigEndian.PutUint16(forged, binary.BigEndian.Uint16(forged)+1)
			_, _ = u.udp.WriteToUDPAddrPort(forged, from)
		}
		_, _ = u.udp.WriteToUDPAddrPort(resp, from)
	}
}

func (u *Upstream) serveTCP() {
	for {
		c, err := u.tcp.Accept()
		if err != nil {
			return
		}
		go func() {
			defer c.Close()
			var lb [2]byte
			if _, err := io.ReadFull(c, lb[:]); err != nil {
				return
			}
			req := make([]byte, binary.BigEndian.Uint16(lb[:]))
			if _, err := io.ReadFull(c, req); err != nil {
				return
			}
			u.Queries.Add(1)
			clone := append([]byte(nil), req...)
			u.LastQuery.Store(&clone)
			resp := u.answer(req, false)
			out := binary.BigEndian.AppendUint16(nil, uint16(len(resp)))
			_, _ = c.Write(append(out, resp...))
		}()
	}
}

// BlackHole returns a UDP address that accepts packets and never answers.
func BlackHole(t testing.TB) netip.AddrPort {
	t.Helper()
	c, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return c.LocalAddr().(*net.UDPAddr).AddrPort()
}

// NewQuery packs a recursive query, optionally with an EDNS OPT record.
func NewQuery(t testing.TB, id uint16, name string, qt dnsmessage.Type, edns bool) []byte {
	t.Helper()
	m := dnsmessage.Message{
		Header:    dnsmessage.Header{ID: id, RecursionDesired: true},
		Questions: []dnsmessage.Question{{Name: dnsmessage.MustNewName(name), Type: qt, Class: dnsmessage.ClassINET}},
	}
	if edns {
		var rh dnsmessage.ResourceHeader
		if err := rh.SetEDNS0(1232, dnsmessage.RCodeSuccess, false); err != nil {
			t.Fatal(err)
		}
		m.Additionals = []dnsmessage.Resource{{Header: rh, Body: &dnsmessage.OPTResource{}}}
	}
	b, err := m.Pack()
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// Exchange sends raw over network ("udp" or "tcp") and returns the parsed reply.
func Exchange(t testing.TB, network string, addr netip.AddrPort, raw []byte) dnsmessage.Message {
	t.Helper()
	var m dnsmessage.Message
	if err := m.Unpack(ExchangeRaw(t, network, addr, raw)); err != nil {
		t.Fatalf("unpack reply: %v", err)
	}
	return m
}

// ExchangeRaw is Exchange without parsing the reply.
func ExchangeRaw(t testing.TB, network string, addr netip.AddrPort, raw []byte) []byte {
	t.Helper()
	c, err := net.DialTimeout(network, addr.String(), 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(3 * time.Second))
	var resp []byte
	if network == "tcp" {
		if _, err := c.Write(append(binary.BigEndian.AppendUint16(nil, uint16(len(raw))), raw...)); err != nil {
			t.Fatal(err)
		}
		var lb [2]byte
		if _, err := io.ReadFull(c, lb[:]); err != nil {
			t.Fatal(err)
		}
		resp = make([]byte, binary.BigEndian.Uint16(lb[:]))
		if _, err := io.ReadFull(c, resp); err != nil {
			t.Fatal(err)
		}
	} else {
		if _, err := c.Write(raw); err != nil {
			t.Fatal(err)
		}
		buf := make([]byte, 65535)
		n, err := c.Read(buf)
		if err != nil {
			t.Fatal(err)
		}
		resp = buf[:n]
	}
	return resp
}

// FirstA returns the first A record of m, or the zero Addr.
func FirstA(m dnsmessage.Message) netip.Addr {
	for _, r := range m.Answers {
		if a, ok := r.Body.(*dnsmessage.AResource); ok {
			return netip.AddrFrom4(a.A)
		}
	}
	return netip.Addr{}
}
