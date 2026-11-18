// Command splitter is a DNS split resolver. It normally runs behind a caching
// front-end resolver such as AdGuard Home: it matches each query name against
// configured domain sets and forwards the query to the upstream group named
// by the matching rule.
//
//	splitter -config /etc/splitter/config.yaml        run
//	splitter -config ... -check                       validate config and lists, then exit
//	kill -HUP <pid>                                   reload config and lists without dropping queries
package main

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"golang.org/x/net/dns/dnsmessage"

	"github.com/starriv/splitter/internal/app"
	"github.com/starriv/splitter/internal/config"
	"github.com/starriv/splitter/internal/metrics"
	"github.com/starriv/splitter/internal/server"
)

func main() { os.Exit(run()) }

func run() int {
	cfgPath := flag.String("config", "/etc/splitter/config.yaml", "config file")
	check := flag.Bool("check", false, "validate the config and load every list, then exit")
	showVersion := flag.Bool("version", false, "print version and exit")
	healthcheck := flag.Bool("healthcheck", false, "probe the local admin /readyz endpoint and every bound DNS address, then exit (Docker HEALTHCHECK)")
	adminFlag := flag.String("admin", "", "with -healthcheck: admin address to probe instead of the one in the config file")
	flag.Parse()
	resolveVersion()

	if *showVersion {
		fmt.Printf("splitter %s (%s)\n", version, commit)
		return 0
	}
	if *check {
		return runCheck(*cfgPath)
	}
	if *healthcheck {
		return runHealthcheck(*cfgPath, *adminFlag)
	}

	level := new(slog.LevelVar)
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
	m := metrics.New(version, commit)

	// Install the handler before anything can take traffic, so a SIGTERM received
	// during startup still follows the graceful path. Separate channels prevent a
	// SIGHUP queued behind a slow reload from displacing a SIGTERM: os/signal
	// drops a signal when the channel is full.
	hup := make(chan os.Signal, 1)
	term := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)
	signal.Notify(term, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(hup)
	defer signal.Stop(term)

	a, err := app.New(*cfgPath, log, level, m)
	if err != nil {
		log.Error("load config", "err", err)
		return 1
	}
	// Runs after the DNS listeners have shut down (below), so the last answered
	// queries reach the query log.
	defer func() {
		if err := a.Close(); err != nil {
			log.Warn("close query log", "err", err)
		}
	}()
	cfg := a.Runtime().Config
	// The semaphore is created once, from the limits the process starts with.
	// Reload does not rebuild it. POST /api/v1/resolve takes a slot from the
	// same channel the DNS listeners use.
	a.UseInflight(cfg.Limits.MaxInflight)

	srv := server.New(a, server.Options{
		MaxInflight:    cfg.Limits.MaxInflight,
		MaxTCPConns:    cfg.Limits.MaxTCPConns,
		TCPIdleTimeout: time.Duration(cfg.Limits.TCPIdleTimeout),
		Logger:         log,
		Observer:       m,
		Sem:            a.InflightSem(),
	})
	if err := srv.Listen(cfg.ListenAddrs()); err != nil {
		log.Error("listen", "err", err)
		return 1
	}
	adminLn, err := net.Listen("tcp", cfg.AdminAddr().String())
	if err != nil {
		log.Error("listen admin", "err", err)
		return 1
	}
	adminBound := cfg.AdminAddr()
	if ap, err := netip.ParseAddrPort(adminLn.Addr().String()); err == nil {
		adminBound = ap
	}
	a.SetBoundAddrs(srv.Addrs(), adminBound)
	if err := writeBoundFile(*cfgPath, adminBound, srv.Addrs()); err != nil {
		// The health check falls back to the config file. At startup that
		// file still names these sockets.
		log.Warn("write bound address file", "err", err)
	}
	// Canceled on shutdown so a resolve still inside ServeDNS stops before
	// the query log is closed. Shutdown itself does not cancel handler
	// contexts.
	handlerCtx, stopHandlers := context.WithCancel(context.Background())
	defer stopHandlers()
	admin := &http.Server{
		Handler:           a.AdminHandler(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		// Go arms this before the handler. Observability and resolve extend
		// it per request; 30s covers the 25s page wait and a single upstream
		// timeout without that extension.
		WriteTimeout:   30 * time.Second,
		IdleTimeout:    60 * time.Second,
		MaxHeaderBytes: 64 << 10,
		BaseContext:    func(net.Listener) context.Context { return handlerCtx },
	}
	adminErr := make(chan error, 1)
	go func() {
		if err := admin.Serve(adminLn); err != nil && !errors.Is(err, http.ErrServerClosed) {
			adminErr <- err
		}
	}()
	srv.Serve()
	log.Info("serving", "version", version, "dns", srv.Addrs(), "admin", cfg.AdminAddr())

	exitCode := 0
loop:
	for {
		select {
		case <-hup:
			if err := a.Reload(); err != nil {
				log.Error("reload rejected; still serving the previous config", "err", err)
			}
		case s := <-term:
			log.Info("signal", "signal", s.String())
			break loop
		case err := <-adminErr:
			// Metrics and health endpoints are the observation surface of the service;
			// if the admin listener fails, the process is unobservable and must be
			// restarted.
			log.Error("admin server died", "err", err)
			exitCode = 1
			break loop
		}
	}
	log.Info("shutting down")
	// Shutdown budgets are separate: draining the admin endpoint must not consume
	// the time the DNS listeners need to flush their in-flight replies.
	// Cancel handler contexts first so a resolve aborts and its query-log
	// append happens before Close, which runs after this function returns.
	stopHandlers()
	adminCtx, adminCancel := context.WithTimeout(context.Background(), 2*time.Second)
	_ = admin.Shutdown(adminCtx)
	adminCancel()
	dnsCtx, dnsCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer dnsCancel()
	if err := srv.Shutdown(dnsCtx); err != nil {
		log.Warn("shutdown", "err", err)
		exitCode = 1
	}
	return exitCode
}

// runHealthcheck implements the Docker HEALTHCHECK; the distroless image
// provides no shell or curl for this purpose. It verifies two conditions:
//
//   - /readyz reports whether the newest configuration was accepted. A rejected
//     reload makes the container report unhealthy without restarting it; the
//     previous runtime keeps serving.
//   - every DNS address the process bound answers a query over UDP and accepts
//     a TCP handshake. /readyz is served by a different goroutine and socket
//     than DNS, so on its own it stays healthy while the resolver itself is dead.
//
// The admin address comes from -admin, else the bound-address file written
// when the process listened, else the admin key in the config file. DNS
// addresses still come from /readyz, so an unreloaded listen edit is not
// probed. The bound file is keyed by the config path: the probe and the
// process must be started with the same path.
//
// Budget: Docker allows 3 seconds in total (see Dockerfile). The HTTP client
// gets 1.5s and the DNS probes share about 1.2s, with a 150ms floor per
// address. Many listen addresses need a longer HEALTHCHECK than 3s.
func runHealthcheck(path, adminOverride string) int {
	admin, err := healthcheckAdmin(path, adminOverride)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	client := &http.Client{Timeout: 1500 * time.Millisecond}
	resp, err := client.Get("http://" + admin.String() + "/readyz")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer resp.Body.Close()
	var body struct {
		Listen []string `json:"listen"`
	}
	decodeErr := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&body)
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintf(os.Stderr, "readyz: %s (the newest config was refused)\n", resp.Status)
		return 1
	}
	if decodeErr != nil || len(body.Listen) == 0 {
		fmt.Fprintf(os.Stderr, "readyz: no bound DNS addresses in the response (%v)\n", decodeErr)
		return 1
	}
	addrs := make([]netip.AddrPort, 0, len(body.Listen))
	for _, s := range body.Listen {
		ap, err := netip.ParseAddrPort(s)
		if err != nil {
			fmt.Fprintf(os.Stderr, "readyz: listen address %q: %v\n", s, err)
			return 1
		}
		addrs = append(addrs, ap)
	}
	// Many listen addresses need a longer Docker HEALTHCHECK than the default
	// 3s: the floor keeps a single address from being given a few milliseconds,
	// and the probes then run one address after another.
	perAddr := healthDNSBudget / time.Duration(len(addrs))
	if perAddr < healthProbeFloor {
		perAddr = healthProbeFloor
	}
	for _, ap := range addrs {
		if err := probeListen(ap, perAddr); err != nil {
			fmt.Fprintf(os.Stderr, "listen %s: %v\n", ap, err)
			return 1
		}
	}
	return 0
}

const (
	healthDNSBudget  = 1200 * time.Millisecond
	healthProbeFloor = 150 * time.Millisecond
)

func healthcheckAdmin(path, override string) (netip.AddrPort, error) {
	if override != "" {
		ap, err := netip.ParseAddrPort(override)
		if err != nil {
			return ap, fmt.Errorf("-admin %q: want ip:port", override)
		}
		return ap, nil
	}
	if ap, ok := readBoundAdmin(path); ok {
		return ap, nil
	}
	return config.LoadAdminAddr(path)
}

// boundFile is the address record written at listen time. The health check
// reads it so an unreloaded edit of the admin key cannot mark a serving
// process unhealthy. The file is not removed on shutdown; the next start
// overwrites it.
type boundFile struct {
	Admin  string   `json:"admin"`
	Listen []string `json:"listen"`
}

func boundFilePath(configPath string) (string, error) {
	abs, err := filepath.Abs(configPath)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte(abs))
	name := "splitter-bound-" + hex.EncodeToString(sum[:8]) + ".json"
	return filepath.Join(os.TempDir(), name), nil
}

func writeBoundFile(configPath string, admin netip.AddrPort, listen []netip.AddrPort) error {
	path, err := boundFilePath(configPath)
	if err != nil {
		return err
	}
	info := boundFile{Admin: admin.String(), Listen: make([]string, len(listen))}
	for i, ap := range listen {
		info.Listen[i] = ap.String()
	}
	body, err := json.Marshal(info)
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, body, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func readBoundAdmin(configPath string) (netip.AddrPort, bool) {
	path, err := boundFilePath(configPath)
	if err != nil {
		return netip.AddrPort{}, false
	}
	body, err := os.ReadFile(path)
	if err != nil {
		return netip.AddrPort{}, false
	}
	var info boundFile
	if json.Unmarshal(body, &info) != nil {
		return netip.AddrPort{}, false
	}
	ap, err := netip.ParseAddrPort(info.Admin)
	if err != nil {
		return netip.AddrPort{}, false
	}
	return ap, true
}

// probeQuery is a CH TXT query for app.HealthcheckName with the ID left at
// zero. A CHAOS-class query is answered locally with REFUSED under every
// configuration, so the probe never reaches an upstream and does not depend on
// which rules exist. It is counted in splitter_queries_total{rule="_invalid"}
// twice a minute but kept out of the query log.
var probeQuery = func() []byte {
	m := dnsmessage.Message{
		Header: dnsmessage.Header{RecursionDesired: true},
		Questions: []dnsmessage.Question{{
			Name: dnsmessage.MustNewName(app.HealthcheckName + "."), Type: dnsmessage.TypeTXT, Class: dnsmessage.ClassCHAOS,
		}},
	}
	b, err := m.Pack()
	if err != nil {
		panic(err)
	}
	return b
}()

// probeDNS sends probeQuery over UDP and accepts any response carrying its ID.
func probeDNS(ap netip.AddrPort, timeout time.Duration) error {
	conn, err := net.DialTimeout("udp", ap.String(), timeout)
	if err != nil {
		return err
	}
	defer conn.Close()
	if err := conn.SetDeadline(time.Now().Add(timeout)); err != nil {
		return err
	}
	req := append([]byte(nil), probeQuery...)
	id := uint16(time.Now().UnixNano())
	binary.BigEndian.PutUint16(req, id)
	if _, err := conn.Write(req); err != nil {
		return err
	}
	buf := make([]byte, 512)
	n, err := conn.Read(buf)
	if err != nil {
		return err
	}
	if n < 12 || binary.BigEndian.Uint16(buf) != id || buf[2]&0x80 == 0 {
		return errors.New("reply is not a DNS response to the probe")
	}
	return nil
}

// probeListen requires both a DNS answer and a TCP handshake. The two run
// together and share one budget.
func probeListen(ap netip.AddrPort, timeout time.Duration) error {
	errc := make(chan error, 2)
	go func() { errc <- probeDNS(ap, timeout) }()
	go func() { errc <- probeTCP(ap, timeout) }()
	var err error
	for range 2 {
		if e := <-errc; e != nil && err == nil {
			err = e
		}
	}
	return err
}

// probeTCP checks that the bound address accepts a connection. The handshake
// is enough: the DNS exchange is the UDP probe's job.
func probeTCP(ap netip.AddrPort, timeout time.Duration) error {
	conn, err := net.DialTimeout("tcp", ap.String(), timeout)
	if err != nil {
		return err
	}
	return conn.Close()
}

// runCheck loads the configuration and every list, then prints a summary of
// the rules and sets.
func runCheck(path string) int {
	cfg, err := config.Load(path)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	rt, err := app.Build(cfg, nil)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	fmt.Printf("ok: %d rules, listen %v, admin %s\n", len(rt.Router.Rules()), cfg.ListenAddrs(), cfg.AdminAddr())
	for _, s := range rt.Sets {
		if s.Count > 0 {
			fmt.Printf("  set %-12s %-8s %d\n", s.Set, s.Kind, s.Count)
		}
	}
	for _, r := range rt.Router.Rules() {
		fmt.Printf("  rule %-12s %-8s -> %s\n", r.Name, r.Action, r.Target())
	}
	if s := cfg.QueryLogSettings(); s.Enabled {
		fmt.Printf("  query log %s (up to %d MiB, kept %s)\n", s.Dir, s.MaxBytes>>20, s.MaxAge)
	} else {
		fmt.Println("  query log off")
	}
	return 0
}
