package main

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"runtime/debug"
	"testing"
	"time"

	"github.com/starriv/splitter/internal/app"
	"github.com/starriv/splitter/internal/metrics"
	"github.com/starriv/splitter/internal/server"
)

// freePort returns a loopback port that was free at the time of the call. The
// generated configuration needs real port numbers because -healthcheck
// discovers the process through the configuration file.
func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

// writeFiles writes a configuration and its list file into dir and returns
// the configuration path.
func writeFiles(t *testing.T, dir string, dns, admin int, list string) string {
	t.Helper()
	cfg := fmt.Sprintf(`
listen: ["127.0.0.1:%d"]
admin: "127.0.0.1:%d"
log: {dir: logs}
upstreams:
  u: {servers: ["192.0.2.1"], timeout: 100ms}
sets:
  cn: {files: [cn.txt]}
rules:
  - {name: cn, sets: [cn], reject: nxdomain}
  - {name: rest, forward: u}
`, dns, admin)
	p := filepath.Join(dir, "config.yaml")
	for name, body := range map[string]string{p: cfg, filepath.Join(dir, "cn.txt"): list} {
		if err := os.WriteFile(name, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return p
}

// TestCheck verifies that -check accepts a valid configuration and rejects
// an empty list and a missing configuration.
func TestCheck(t *testing.T) {
	dir := t.TempDir()
	p := writeFiles(t, dir, 5336, 9198, "example.cn\n")
	if code := runCheck(p); code != 0 {
		t.Errorf("valid config: exit %d", code)
	}
	writeFiles(t, dir, 5336, 9198, "# empty\n")
	if code := runCheck(p); code == 0 {
		t.Error("an empty list passed -check")
	}
	if code := runCheck(filepath.Join(dir, "missing.yaml")); code == 0 {
		t.Error("a missing config passed -check")
	}
}

// TestHealthcheck verifies that -healthcheck fails for a refused reload and
// for a dead DNS listener; the admin endpoint answering alone does not
// constitute health.
func TestHealthcheck(t *testing.T) {
	dir := t.TempDir()
	dnsPort, adminPort := freePort(t), freePort(t)
	p := writeFiles(t, dir, dnsPort, adminPort, "example.cn\n")

	a, err := app.New(p, slog.New(slog.DiscardHandler), new(slog.LevelVar), metrics.New("t", "t"))
	if err != nil {
		t.Fatal(err)
	}
	cfg := a.Runtime().Config
	srv := server.New(a, server.Options{})
	if err := srv.Listen(cfg.ListenAddrs()); err != nil {
		t.Fatal(err)
	}
	srv.Serve()
	adminLn, err := net.Listen("tcp", cfg.AdminAddr().String())
	if err != nil {
		t.Fatal(err)
	}
	admin := &http.Server{Handler: a.AdminHandler()}
	go func() { _ = admin.Serve(adminLn) }()
	t.Cleanup(func() { _ = admin.Close() })
	stopped := false
	stop := func() {
		if stopped {
			return
		}
		stopped = true
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	}
	t.Cleanup(stop)

	if code := runHealthcheck(p, ""); code != 0 {
		t.Fatalf("healthy process: exit %d", code)
	}

	// A refused reload keeps the old runtime serving but reports unhealthy until
	// a valid configuration loads.
	writeFiles(t, dir, dnsPort, adminPort, "# empty\n")
	if err := a.Reload(); err == nil {
		t.Fatal("empty list accepted")
	}
	if code := runHealthcheck(p, ""); code == 0 {
		t.Error("healthy after a refused reload")
	}
	writeFiles(t, dir, dnsPort, adminPort, "example.cn\n")
	if err := a.Reload(); err != nil {
		t.Fatal(err)
	}
	if code := runHealthcheck(p, ""); code != 0 {
		t.Fatalf("recovered process: exit %d", code)
	}

	// A configuration file that was edited but not reloaded does not affect the
	// running process, so it must not fail the check either.
	broken := []byte(fmt.Sprintf("listen: [\"127.0.0.1:%d\"]\nadmin: \"127.0.0.1:%d\"\nrules: []\n", freePort(t), adminPort))
	if err := os.WriteFile(p, broken, 0o644); err != nil {
		t.Fatal(err)
	}
	if code := runHealthcheck(p, ""); code != 0 {
		t.Errorf("unreloaded config edit failed the check: exit %d", code)
	}
	if code := runHealthcheck(filepath.Join(dir, "missing.yaml"), cfg.AdminAddr().String()); code != 0 {
		t.Errorf("-admin override: exit %d", code)
	}

	// DNS listeners are gone while the admin endpoint still answers 200.
	stop()
	if code := runHealthcheck(p, ""); code == 0 {
		t.Error("healthy with the DNS listener down")
	}
}

// TestProbeDNSRejectsNonDNS verifies that an echoed query, whose QR bit is
// clear, is not accepted as a DNS response.
func TestProbeDNSRejectsNonDNS(t *testing.T) {
	c, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	go func() {
		buf := make([]byte, 512)
		for {
			n, from, err := c.ReadFromUDPAddrPort(buf)
			if err != nil {
				return
			}
			_, _ = c.WriteToUDPAddrPort(buf[:n], from) // echo: QR bit not set
		}
	}()
	ap := netip.MustParseAddrPort(c.LocalAddr().String())
	if err := probeDNS(ap, 500*time.Millisecond); err == nil {
		t.Error("an echoed query was accepted as a DNS response")
	}
}

// TestVersionFromBuildInfo verifies the fallback used when version and commit
// were not set at link time, and that link-time values take precedence.
func TestVersionFromBuildInfo(t *testing.T) {
	installed := &debug.BuildInfo{Main: debug.Module{Version: "v1.2.3"}}
	checkout := &debug.BuildInfo{Main: debug.Module{Version: "(devel)"}, Settings: []debug.BuildSetting{
		{Key: "vcs.revision", Value: "9a477360aff02731e34edda693094d7f977f7435"},
		{Key: "vcs.modified", Value: "true"},
	}}
	cases := []struct {
		name         string
		v, c         string
		info         *debug.BuildInfo
		wantV, wantC string
	}{
		{"go install", "dev", "none", installed, "v1.2.3", "none"},
		{"git checkout", "dev", "none", checkout, "dev", "9a477360aff0-dirty"},
		{"link-time values win", "v9.9.9", "abcdef123456", checkout, "v9.9.9", "abcdef123456"},
		{"no build info", "dev", "none", nil, "dev", "none"},
	}
	for _, c := range cases {
		if v, cm := versionFromBuildInfo(c.v, c.c, c.info); v != c.wantV || cm != c.wantC {
			t.Errorf("%s: got %q %q, want %q %q", c.name, v, cm, c.wantV, c.wantC)
		}
	}
}
