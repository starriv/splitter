package app

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/net/dns/dnsmessage"

	"github.com/starriv/splitter/internal/config"
	"github.com/starriv/splitter/internal/metrics"
	"github.com/starriv/splitter/internal/querylog"
	"github.com/starriv/splitter/internal/wire"
)

// TestQueryLogSurvivesRestart verifies the whole write path: answered queries
// reach log.dir, Close flushes them, and a new process reads them back.
func TestQueryLogSurvivesRestart(t *testing.T) {
	e := start(t)
	if e.app.qlog == nil {
		t.Fatal("query log not open")
	}
	for _, n := range []string{"www.baidu.com.", "x.local.example.", "example.org."} {
		e.ask(t, "udp", n, dnsmessage.TypeA)
	}
	if err := e.app.Close(); err != nil {
		t.Fatal(err)
	}
	matches, _ := filepath.Glob(filepath.Join(e.dir, "logs", "queries-*.jsonl"))
	if len(matches) != 1 {
		t.Fatalf("segments: %v", matches)
	}

	a, err := New(e.cfg, slog.New(slog.DiscardHandler), new(slog.LevelVar), metrics.New("t", "t"))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	page, err := a.qlog.Read("", 10, querylog.Filter{})
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, r := range page.Records {
		names = append(names, r.Name+"/"+r.Rule)
	}
	if got := strings.Join(names, " "); got != "example.org/default x.local.example/local-zone www.baidu.com/cn" {
		t.Fatalf("records after restart: %s", got)
	}
}

// TestQueryLogUnwritableDirFails verifies that an unusable log directory
// stops startup, whether it is written in the file or left at the default,
// and that a disabled query log needs no directory.
func TestQueryLogUnwritableDirFails(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "blocker"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, "config.yaml")
	cfg := "listen: [\"127.0.0.1:0\"]\nadmin: \"127.0.0.1:0\"\nlog: {dir: blocker/logs}\nrules:\n  - {name: rest, reject: refused}\n"
	if err := os.WriteFile(p, []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := New(p, slog.New(slog.DiscardHandler), new(slog.LevelVar), metrics.New("t", "t"))
	if err == nil || !strings.Contains(err.Error(), "not writable") {
		t.Fatalf("explicit dir: want a startup error, got %v", err)
	}

	// The default directory: stand in for an unwritable /var/log/splitter
	// without depending on what exists at that path on this machine.
	c, err := config.Parse([]byte("listen: [\"127.0.0.1:0\"]\nadmin: \"127.0.0.1:0\"\nrules:\n  - {name: rest, reject: refused}\n"), dir)
	if err != nil {
		t.Fatal(err)
	}
	c.Log.Dir = filepath.Join(dir, "blocker", "logs")
	a := &App{log: slog.New(slog.DiscardHandler), m: metrics.New("t", "t")}
	if err := a.openQueryLog(c); err == nil {
		t.Fatal("default dir: want a startup error")
	}

	off := false
	c.Log.QueryLog.Enabled = &off
	if err := a.openQueryLog(c); err != nil || a.qlog != nil {
		t.Fatalf("disabled log: err %v, qlog %v", err, a.qlog)
	}
}

// TestQueryLogRecordsInvalidQueries verifies that queries refused before
// routing are recorded under _invalid with their class, while the
// -healthcheck probe is left out.
func TestQueryLogRecordsInvalidQueries(t *testing.T) {
	e := start(t)
	ask := func(name string, class dnsmessage.Class) {
		t.Helper()
		m := dnsmessage.Message{Header: dnsmessage.Header{ID: 7, RecursionDesired: true},
			Questions: []dnsmessage.Question{{Name: dnsmessage.MustNewName(name), Type: dnsmessage.TypeTXT, Class: class}}}
		req, err := m.Pack()
		if err != nil {
			t.Fatal(err)
		}
		if e.app.ServeDNS(context.Background(), req, wire.UDP) == nil {
			t.Fatal("no reply")
		}
	}
	ask("version.bind.", dnsmessage.ClassCHAOS)
	ask(HealthcheckName+".", dnsmessage.ClassCHAOS)
	e.ask(t, "udp", "www.baidu.com.", dnsmessage.TypeA)

	page, err := e.app.qlog.Read("", 10, querylog.Filter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Records) != 2 {
		t.Fatalf("want 2 records (the probe left out), got %+v", page.Records)
	}
	if r := page.Records[1]; r.Name != "version.bind" || r.Rule != invalidRule || r.Class != "CH" || r.Type != "TXT" ||
		r.RCode != "REFUSED" || r.Target != "local" {
		t.Errorf("invalid query record = %+v", r)
	}
	if r := page.Records[0]; r.Class != "" || r.Rule != "cn" {
		t.Errorf("IN query record = %+v", r)
	}
}
