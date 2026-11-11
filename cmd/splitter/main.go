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
	"encoding/binary"
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
	"syscall"
	"time"

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
	cfg := a.Runtime().Config

	srv := server.New(a, server.Options{
		MaxInflight:    cfg.Limits.MaxInflight,
		MaxTCPConns:    cfg.Limits.MaxTCPConns,
		TCPIdleTimeout: time.Duration(cfg.Limits.TCPIdleTimeout),
		Logger:         log,
		Observer:       m,
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
	a.SetBoundAddrs(srv.Addrs(), cfg.AdminAddr())
	admin := &http.Server{
		Handler:           a.AdminHandler(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    64 << 10,
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
//   - every DNS address the process bound answers a query over UDP. /readyz is
//     served by a different goroutine and socket than DNS, so on its own it
//     stays healthy while the resolver itself is dead.
//
// Only the admin address is read from the configuration file (or taken from
// adminOverride); the DNS addresses come from /readyz. A file that was edited
// but not yet reloaded therefore cannot fail the check of a healthy process,
// and a listen address changed in the file is not probed before the restart
// that binds it.
//
// Budget: Docker allows 3 seconds in total (see Dockerfile), which dictates the
// short timeouts.
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
	perAddr := time.Second / time.Duration(len(addrs))
	for _, ap := range addrs {
		if err := probeDNS(ap, perAddr); err != nil {
			fmt.Fprintf(os.Stderr, "dns %s: %v\n", ap, err)
			return 1
		}
	}
	return 0
}

func healthcheckAdmin(path, override string) (netip.AddrPort, error) {
	if override != "" {
		ap, err := netip.ParseAddrPort(override)
		if err != nil {
			return ap, fmt.Errorf("-admin %q: want ip:port", override)
		}
		return ap, nil
	}
	return config.LoadAdminAddr(path)
}

// probeQuery is a "version.bind CH TXT" query with the ID left at zero. A
// CHAOS-class query is answered locally with REFUSED under every configuration,
// so the probe never reaches an upstream and does not depend on which rules
// exist. It is counted in splitter_queries_total{rule="_invalid"} twice a
// minute.
var probeQuery = []byte{
	0, 0, 0x01, 0x00, 0, 1, 0, 0, 0, 0, 0, 0,
	7, 'v', 'e', 'r', 's', 'i', 'o', 'n', 4, 'b', 'i', 'n', 'd', 0,
	0, 16, 0, 3,
}

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
	return 0
}
