package app

// Scenario tests: behaviours that a front-end caching resolver such as
// AdGuard Home relies on when splitter runs behind it.

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/net/dns/dnsmessage"

	"github.com/starriv/splitter/internal/dnstest"
	"github.com/starriv/splitter/internal/metrics"
	"github.com/starriv/splitter/internal/wire"
)

// TestForwardedReplyIsByteIdentical verifies that a forwarded reply reaches
// the client exactly as the upstream sent it, apart from the ID, including
// EDNS options and record types that the splitter does not decode.
func TestForwardedReplyIsByteIdentical(t *testing.T) {
	e := start(t)
	e.ab.Extra.Store(true)
	for _, network := range []string{"udp", "tcp"} {
		got := dnstest.ExchangeRaw(t, network, e.dns, dnstest.NewQuery(t, 0x5151, "www.google.com.", dnsmessage.TypeA, true))
		sent := *e.ab.Last.Load()
		if wire.ID(got) != 0x5151 {
			t.Fatalf("%s: ID %#x", network, wire.ID(got))
		}
		if !bytes.Equal(got[2:], sent[2:]) {
			t.Fatalf("%s: reply changed in transit\nupstream %x\nclient   %x", network, sent, got)
		}
		if !bytes.Contains(got, []byte("splitter-passthrough")) {
			t.Fatalf("%s: EDNS option lost", network)
		}
	}
}

// TestTruncationRetryOverTCP verifies the truncation path: an oversized UDP
// answer sets TC and carries no records, the client's TCP retry is
// forwarded over TCP, and the complete answer is returned. This matches the
// behaviour of a front-end proxy such as AdGuard Home's dnsproxy.
func TestTruncationRetryOverTCP(t *testing.T) {
	e := start(t)
	e.ab.TruncateUDP.Store(true)
	m := e.ask(t, "udp", "big.example.org.", dnsmessage.TypeA)
	if !m.Header.Truncated || len(m.Answers) != 0 {
		t.Fatalf("udp: want TC with no answers, got %+v", m.Header)
	}
	m = e.ask(t, "tcp", "big.example.org.", dnsmessage.TypeA)
	if m.Header.Truncated || dnstest.FirstA(m) != abroadAddr {
		t.Fatalf("tcp retry: %+v %v", m.Header, dnstest.FirstA(m))
	}
}

// TestRefusedUpstreamFailsOverFast verifies that a refused port (ICMP
// unreachable) triggers failover immediately instead of waiting for the
// upstream timeout.
func TestRefusedUpstreamFailsOverFast(t *testing.T) {
	e := start(t)
	c, _ := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	dead := c.LocalAddr().(*net.UDPAddr).AddrPort()
	c.Close() // nothing listens there now
	cfg := fmt.Sprintf(`
listen: ["127.0.0.1:0"]
admin: "127.0.0.1:0"
upstreams:
  aliyun: {servers: ["%s"], timeout: 1s}
  cloudflare: {servers: ["%s", "%s"], timeout: 3s}
sets:
  cn: {files: [cn-domains.txt]}
rules:
  - {name: cn, sets: [cn], forward: aliyun}
  - {name: default, forward: cloudflare}
`, e.cn.Addr, dead, e.ab.Addr)
	if err := os.WriteFile(e.cfg, []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := e.app.Reload(); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	m := e.ask(t, "udp", "www.google.com.", dnsmessage.TypeA)
	if dnstest.FirstA(m) != abroadAddr {
		t.Fatalf("no failover: %+v", m.Header)
	}
	if d := time.Since(start); d > time.Second {
		t.Fatalf("failover waited %s; ICMP unreachable should short-circuit", d)
	}
}

// TestReloadUnderLoad verifies that continuous traffic survives repeated
// reloads from a scheduled list update without a failed query, and that
// names outside the changed entry route identically on every attempt.
func TestReloadUnderLoad(t *testing.T) {
	e := start(t)
	lists := []string{"domain:baidu.com\nfull:www.qq.com\n", "domain:baidu.com\nfull:www.qq.com\ndomain:bilibili.com\n"}
	stop := make(chan struct{})
	var reloads atomic.Int64
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			p := filepath.Join(e.dir, "cn-domains.txt")
			tmp := p + ".tmp"
			_ = os.WriteFile(tmp, []byte(lists[i%2]), 0o644)
			_ = os.Rename(tmp, p) // the list updater replaces files atomically
			if err := e.app.Reload(); err != nil {
				t.Errorf("reload: %v", err)
				return
			}
			reloads.Add(1)
			time.Sleep(5 * time.Millisecond)
		}
	}()

	names := []struct {
		name string
		want netip.Addr
	}{
		{"www.baidu.com.", cnAddr},
		{"www.qq.com.", cnAddr},
		{"www.google.com.", abroadAddr},
		{"home.local.example.", localAddr},
	}
	var failures atomic.Int64
	var clients sync.WaitGroup
	for w := range 16 {
		clients.Add(1)
		go func() {
			defer clients.Done()
			network := "udp"
			if w%4 == 0 {
				network = "tcp"
			}
			for i := range 60 {
				n := names[(w+i)%len(names)]
				if got := dnstest.FirstA(e.ask(t, network, n.name, dnsmessage.TypeA)); got != n.want {
					failures.Add(1)
					t.Errorf("%s %s -> %v", network, n.name, got)
				}
				// The changed entry may route either way, but every query must be answered.
				if got := dnstest.FirstA(e.ask(t, network, "www.bilibili.com.", dnsmessage.TypeA)); got != cnAddr && got != abroadAddr {
					failures.Add(1)
					t.Errorf("bilibili -> %v", got)
				}
			}
		}()
	}
	clients.Wait()
	close(stop)
	wg.Wait()
	if reloads.Load() < 5 {
		t.Fatalf("only %d reloads happened during the run", reloads.Load())
	}
	t.Logf("%d queries, %d reloads, %d failures", 16*60*2, reloads.Load(), failures.Load())
}

// FuzzServeDNS exercises the full request path with a configuration that
// never touches the network.
func FuzzServeDNS(f *testing.F) {
	dir := f.TempDir()
	cfg := `
listen: ["127.0.0.1:0"]
admin: "127.0.0.1:0"
log: {query_log: {enabled: false}}
sets:
  local: {rules: ["domain:local.example", "keyword:ads", "regexp:^x[0-9]+\\."]}
rules:
  - {name: local, sets: [local], answer: {a: ["192.0.2.10"], aaaa: ["fd00::11"]}}
  - {name: rest, reject: refused}
`
	p := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(p, []byte(cfg), 0o644); err != nil {
		f.Fatal(err)
	}
	a, err := New(p, slog.New(slog.DiscardHandler), new(slog.LevelVar), metrics.New("f", "f"))
	if err != nil {
		f.Fatal(err)
	}
	f.Add(dnstest.NewQuery(f, 1, "a.local.example.", dnsmessage.TypeA, true))
	f.Add(dnstest.NewQuery(f, 2, "x12.example.", dnsmessage.TypeAAAA, false))
	f.Add(dnstest.NewQuery(f, 3, "www.google.com.", 65, true))
	f.Add([]byte{0, 1, 0x80, 0, 0, 1, 0, 0, 0, 0, 0, 0})
	f.Fuzz(func(t *testing.T, req []byte) {
		resp := a.ServeDNS(context.Background(), req, wire.UDP)
		if resp == nil {
			return
		}
		var m dnsmessage.Message
		if err := m.Unpack(resp); err != nil {
			t.Fatalf("we produced an unparseable reply: %v", err)
		}
		if !m.Header.Response || m.Header.ID != wire.ID(req) {
			t.Fatalf("bad reply header %+v", m.Header)
		}
	})
}
