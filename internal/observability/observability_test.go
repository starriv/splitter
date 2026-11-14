package observability

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakePrometheus serves canned Prometheus API responses and records the
// expressions it was asked for. hasExported selects whether the scrape renames
// the splitter's server label to exported_server, as the home deployment does.
type fakePrometheus struct {
	mu          sync.Mutex
	hasExported bool
	queries     []string
}

func (f *fakePrometheus) server(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/query", f.instant)
	mux.HandleFunc("/api/v1/query_range", f.rangeQuery)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func (f *fakePrometheus) record(expr string) {
	f.mu.Lock()
	f.queries = append(f.queries, expr)
	f.mu.Unlock()
}

func (f *fakePrometheus) asked(substr string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, q := range f.queries {
		if strings.Contains(q, substr) {
			return true
		}
	}
	return false
}

// instant returns one canned value per expression the panels use. Unknown
// expressions receive an empty vector, which is not an error.
func (f *fakePrometheus) instant(w http.ResponseWriter, r *http.Request) {
	expr := r.URL.Query().Get("query")
	f.record(expr)
	label := "server"
	srv := "223.5.5.5:53"
	if f.hasExported {
		label = "exported_server"
	}
	vector := func(labels map[string]string, value string) {
		writeProm(w, "vector", []any{map[string]any{"metric": labels, "value": []any{1700000040.0, value}}})
	}
	switch {
	case expr == "vector(1)":
		vector(map[string]string{}, "1")
	case strings.HasPrefix(expr, "min(up{job="):
		vector(map[string]string{}, "1")
	case strings.HasPrefix(expr, "min(splitter_config_last_reload_successful"):
		vector(map[string]string{}, "0")
	case strings.HasPrefix(expr, `sum(increase(splitter_queries_total{rule!="_invalid"}[1296000s]))`):
		vector(map[string]string{}, "12345")
	case strings.HasPrefix(expr, `sum(increase(splitter_queries_total{rule!="_invalid"}[`):
		vector(map[string]string{}, "600")
	case strings.HasPrefix(expr, "sum(rate(splitter_queries_total"):
		vector(map[string]string{}, "0.5")
	case strings.Contains(expr, `splitter_responses_total{rcode="SERVFAIL"}`):
		vector(map[string]string{}, "98.5")
	case strings.Contains(expr, "splitter_request_duration_seconds_sum"):
		vector(map[string]string{}, "12.5")
	case expr == "count by (exported_server) (splitter_upstream_requests_total)":
		if f.hasExported {
			vector(map[string]string{"exported_server": srv}, "1")
			return
		}
		vector(map[string]string{"exported_server": ""}, "1")
	case strings.Contains(expr, "sum by (rule, action, target) (increase("):
		vector(map[string]string{"rule": "cn", "action": "forward", "target": "aliyun"}, "600")
	case strings.Contains(expr, "sum by (rule, action, target) (rate("):
		vector(map[string]string{"rule": "cn", "action": "forward", "target": "aliyun"}, "0.5")
	case strings.Contains(expr, "sum by (upstream,") && strings.Contains(expr, "increase(splitter_upstream_requests_total"):
		vector(map[string]string{"upstream": "aliyun", label: srv}, "300")
	case strings.Contains(expr, "sum by (upstream,") && strings.Contains(expr, "increase(splitter_upstream_errors_total"):
		vector(map[string]string{"upstream": "aliyun", label: srv}, "3")
	case strings.Contains(expr, "sum by (upstream,") && strings.Contains(expr, "splitter_upstream_duration_seconds_bucket"):
		vector(map[string]string{"upstream": "aliyun", label: srv}, "12.5")
	case strings.Contains(expr, "sum by (upstream,") && strings.Contains(expr, "increase(splitter_upstream_retransmits_total"):
		vector(map[string]string{"upstream": "aliyun", label: srv}, "2")
	case strings.Contains(expr, "sum by (upstream,") && strings.Contains(expr, "increase(splitter_upstream_dropped_responses_total"):
		vector(map[string]string{"upstream": "aliyun", label: srv}, "1")
	case strings.HasPrefix(expr, "time() - max(splitter_last_reload_success_timestamp_seconds"):
		vector(map[string]string{}, "3600")
	case expr == "sum by (set) (splitter_set_entries)":
		writeProm(w, "vector", []any{
			map[string]any{"metric": map[string]string{"set": "cn"}, "value": []any{1700000040.0, "1000"}},
			map[string]any{"metric": map[string]string{"set": "local"}, "value": []any{1700000040.0, "2"}},
		})
	case strings.HasPrefix(expr, `sum(increase(splitter_queries_total{rule="_invalid"}`):
		vector(map[string]string{}, "4")
	default:
		vector(map[string]string{}, "0")
	}
}

// rangeQuery returns three aligned samples at start, start+step, start+2*step.
func (f *fakePrometheus) rangeQuery(w http.ResponseWriter, r *http.Request) {
	expr := r.URL.Query().Get("query")
	f.record(expr)
	q := r.URL.Query()
	start, _ := strconv.ParseFloat(q.Get("start"), 64)
	step, _ := strconv.ParseFloat(strings.TrimSuffix(q.Get("step"), "s"), 64)
	values := func(a, b, c float64) []any {
		return []any{
			[]any{start, strconv.FormatFloat(a, 'f', -1, 64)},
			[]any{start + step, strconv.FormatFloat(b, 'f', -1, 64)},
			[]any{start + 2*step, strconv.FormatFloat(c, 'f', -1, 64)},
		}
	}
	series := func(labels map[string]string, a, b, c float64) map[string]any {
		return map[string]any{"metric": labels, "values": values(a, b, c)}
	}
	var result []any
	switch {
	case strings.Contains(expr, "sum by (rule, action)"):
		result = []any{
			series(map[string]string{"rule": "default", "action": "forward"}, 0.2, 0.2, 0.2),
			series(map[string]string{"rule": "cn", "action": "forward"}, 0.5, 0.5, 0.5),
		}
	case strings.Contains(expr, "sum(rate(splitter_queries_total"):
		result = []any{series(map[string]string{}, 0.7, 0.7, 0.7)}
	case strings.Contains(expr, "sum by (rule) (splitter_queries_total"):
		result = []any{
			series(map[string]string{"rule": "cn"}, 1000, 1010, 1020),
			series(map[string]string{"rule": "default"}, 500, 505, 510),
		}
	case strings.Contains(expr, "sum(splitter_queries_total"):
		result = []any{series(map[string]string{}, 1500, 1515, 1530)}
	case strings.Contains(expr, "sum by (rcode)"):
		result = []any{
			series(map[string]string{"rcode": "NOERROR"}, 0.6, 0.6, 0.6),
			series(map[string]string{"rcode": "SERVFAIL"}, 0.1, 0.1, 0.1),
		}
	case strings.Contains(expr, "sum by (rule, le)"):
		result = []any{series(map[string]string{"rule": "cn"}, 5, 6, 7)}
	case strings.Contains(expr, "sum by (le)"):
		result = []any{series(map[string]string{}, 6, 7, 8)}
	case strings.Contains(expr, "sum by (upstream,") && strings.Contains(expr, "le)"):
		result = []any{series(map[string]string{"upstream": "aliyun", "exported_server": "223.5.5.5:53", "server": "home-dns"}, 9, 10, 11)}
	case strings.Contains(expr, "sum by (upstream,") && strings.Contains(expr, "reason)"):
		result = []any{series(map[string]string{"upstream": "aliyun", "exported_server": "223.5.5.5:53", "server": "home-dns", "reason": "timeout"}, 0.1, 0.1, 0.1)}
	case strings.Contains(expr, "splitter_upstream_retransmits_total"):
		result = []any{series(map[string]string{"upstream": "aliyun", "exported_server": "223.5.5.5:53", "server": "home-dns"}, 0.01, 0.01, 0.01)}
	case strings.Contains(expr, "splitter_upstream_dropped_responses_total"):
		result = []any{series(map[string]string{"upstream": "aliyun", "exported_server": "223.5.5.5:53", "server": "home-dns"}, 0.001, 0.001, 0.001)}
	case strings.Contains(expr, "splitter_overload_dropped_total"),
		strings.Contains(expr, "splitter_response_write_errors_total"),
		strings.Contains(expr, "splitter_malformed_queries_total"),
		strings.Contains(expr, "splitter_reloads_total{result=\"failure\"}"),
		strings.Contains(expr, "splitter_inflight_queries"),
		strings.Contains(expr, "splitter_tcp_connections"),
		strings.Contains(expr, "splitter_panics_total"):
		result = []any{series(map[string]string{}, 0.01, 0.02, 0.03)}
	default:
		result = []any{}
	}
	writeProm(w, "matrix", result)
}

func writeProm(w http.ResponseWriter, kind string, result []any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"status": "success",
		"data":   map[string]any{"resultType": kind, "result": result},
	})
}

// find returns the panel with id.
func find(t *testing.T, snap Snapshot, id string) Panel {
	t.Helper()
	for _, p := range snap.Panels {
		if p.ID == id {
			return p
		}
	}
	t.Fatalf("panel %q not found", id)
	return Panel{}
}

func TestParseWindow(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want time.Duration
		ok   bool
	}{
		{"", DefaultWindow, true},
		{"6h", 6 * time.Hour, true},
		{"3d", 72 * time.Hour, true},
		{"2w", 14 * 24 * time.Hour, true},
		{"90s", 0, false},
		{"1m", 0, false},
		{"60d", 0, false},
		{"nope", 0, false},
	} {
		got, err := ParseWindow(tc.in)
		if tc.ok && (err != nil || got != tc.want) {
			t.Errorf("ParseWindow(%q) = %v, %v; want %v", tc.in, got, err, tc.want)
		}
		if !tc.ok && err == nil {
			t.Errorf("ParseWindow(%q) accepted", tc.in)
		}
	}
}

func TestDisabledClient(t *testing.T) {
	var c *Client
	snap := c.Snapshot(context.Background(), 6*time.Hour, time.Unix(1_700_000_000, 0))
	if snap.Prometheus.Configured || snap.Prometheus.Reachable || len(snap.Panels) != 0 {
		t.Errorf("snapshot = %+v", snap)
	}
	if snap.WindowSeconds != (6*time.Hour).Seconds() || snap.StepMS <= 0 || snap.EndMS == 0 {
		t.Errorf("grid = %+v", snap)
	}
	if New(Settings{}) != nil || New(Settings{URL: "not a url"}) != nil {
		t.Error("invalid or empty settings must disable the client")
	}
}

func TestSnapshot(t *testing.T) {
	fake := &fakePrometheus{hasExported: true}
	srv := fake.server(t)
	now := time.Unix(1_700_000_040, 0)
	c := New(Settings{URL: srv.URL, Job: "splitter", DashboardURL: "https://grafana.example/d/x", Timeout: 2 * time.Second})
	snap := c.Snapshot(context.Background(), 5*time.Minute, now)
	if !snap.Prometheus.Configured || !snap.Prometheus.Reachable || snap.Prometheus.Error != "" {
		t.Fatalf("prometheus = %+v", snap.Prometheus)
	}
	if snap.Prometheus.Job != "splitter" || snap.Prometheus.DashboardURL != "https://grafana.example/d/x" {
		t.Errorf("prometheus info = %+v", snap.Prometheus)
	}
	if len(snap.Panels) != 20 {
		t.Fatalf("panels = %d", len(snap.Panels))
	}
	for _, p := range snap.Panels {
		if p.ID == "" || p.Title == "" || p.Kind == "" || p.Description == "" {
			t.Errorf("incomplete panel %+v", p)
		}
		if p.Error != "" {
			t.Errorf("%s: %s", p.ID, p.Error)
		}
	}
	if got := find(t, snap, "scrape"); got.Display != "正常" || got.Tone != "success" {
		t.Errorf("scrape = %+v", got)
	}
	if got := find(t, snap, "reload"); got.Display != "加载被拒" || got.Tone != "danger" {
		t.Errorf("reload = %+v", got)
	}
	if got := find(t, snap, "queries_window"); got.Display != "600" {
		t.Errorf("queries_window = %+v", got)
	}
	if got := find(t, snap, "qps"); got.Display != "0.500" {
		t.Errorf("qps = %+v", got)
	}
	if got := find(t, snap, "answer_success"); got.Display != "98.50" || got.Tone != "warning" {
		t.Errorf("answer_success = %+v", got)
	}
	if got := find(t, snap, "set_entries"); got.Display != "1,002" || !strings.Contains(got.Context, "cn 1,000") {
		t.Errorf("set_entries = %+v", got)
	}

	// The rule table is an outer join of increase and rate by labels.
	rules := find(t, snap, "rules")
	if rules.Kind != "table" || len(rules.Rows) != 1 {
		t.Fatalf("rules = %+v", rules)
	}
	row := rules.Rows[0]
	if row.Key != "cn · forward · aliyun" {
		t.Errorf("rule key = %q", row.Key)
	}
	if c := row.Cells["queries"]; c.Value == nil || *c.Value != 600 {
		t.Errorf("rule queries cell = %+v", c)
	}
	if c := row.Cells["share"]; c.Value == nil || *c.Value != 100 {
		t.Errorf("rule share cell = %+v", c)
	}

	// The upstream table uses the exported_server label and computes the
	// success rate from attempts and failures.
	up := find(t, snap, "upstream_table")
	if up.Kind != "table" || len(up.Rows) != 1 {
		t.Fatalf("upstream = %+v", up)
	}
	upRow := up.Rows[0]
	if upRow.Key != "aliyun · 223.5.5.5:53" {
		t.Errorf("upstream key = %q", upRow.Key)
	}
	if c := upRow.Cells["success"]; c.Value == nil || *c.Value != 99 || c.Tone != "success" {
		t.Errorf("upstream success = %+v", c)
	}
	if c := upRow.Cells["p95"]; c.Value == nil || *c.Value != 12.5 {
		t.Errorf("upstream p95 = %+v", c)
	}
	if !fake.asked("sum by (upstream, exported_server, le)") {
		t.Error("upstream queries did not select the exported_server label")
	}

	// Chart series are aligned with the snapshot grid and coloured by name.
	rate := find(t, snap, "query_rate")
	if rate.Kind != "series" || !rate.Stacked || len(rate.Series) != 3 {
		t.Fatalf("query_rate = %+v", rate)
	}
	wantNames := []string{"cn · forward", "default · forward", "总计"}
	for i, name := range wantNames {
		if rate.Series[i].Name != name {
			t.Errorf("series %d = %q, want %q", i, rate.Series[i].Name, name)
		}
	}
	if rate.Series[0].Color != "green" || rate.Series[1].Color != "amber" || rate.Series[2].Color != "violet" {
		t.Errorf("colors = %v, %v, %v", rate.Series[0].Color, rate.Series[1].Color, rate.Series[2].Color)
	}
	if rate.Series[2].Line != true || rate.Series[0].Line {
		t.Error("the total series must be drawn over the stack, not inside it")
	}
	if got := len(rate.Series[0].Values); got != int((snap.EndMS-snap.StartMS)/snap.StepMS)+1 {
		t.Errorf("series has %d values, want one per grid step (%d)", got, snap.StepMS)
	}
	for i, want := range []float64{0.5, 0.5, 0.5} {
		if v := rate.Series[0].Values[i]; v == nil || *v != want {
			t.Errorf("value %d = %v, want %v", i, v, want)
		}
	}

	// The protection chart merges the fixed-name gauges.
	protection := find(t, snap, "protection")
	if len(protection.Series) != 7 {
		t.Errorf("protection series = %d", len(protection.Series))
	}
}

func TestSnapshotWithoutExportedServer(t *testing.T) {
	fake := &fakePrometheus{hasExported: false}
	srv := fake.server(t)
	c := New(Settings{URL: srv.URL, Timeout: 2 * time.Second})
	snap := c.Snapshot(context.Background(), 5*time.Minute, time.Unix(1_700_000_040, 0))
	if !snap.Prometheus.Reachable {
		t.Fatalf("prometheus = %+v", snap.Prometheus)
	}
	if !fake.asked("sum by (upstream, server)") || fake.asked("sum by (upstream, exported_server") {
		t.Error("the fallback must use the splitter's own server label")
	}
}

func TestSnapshotUnreachable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()
	c := New(Settings{URL: srv.URL, Timeout: 2 * time.Second})
	snap := c.Snapshot(context.Background(), time.Hour, time.Unix(1_700_000_000, 0))
	if !snap.Prometheus.Configured || snap.Prometheus.Reachable || snap.Prometheus.Error == "" {
		t.Fatalf("prometheus = %+v", snap.Prometheus)
	}
	if len(snap.Panels) != 0 {
		t.Errorf("panels = %d", len(snap.Panels))
	}
}

// TestLivePrometheus is an optional integration check against a real
// Prometheus: SPLITTER_OBSERVABILITY_URL=https://prometheus.example go test ./internal/observability -run TestLive
func TestLivePrometheus(t *testing.T) {
	url := os.Getenv("SPLITTER_OBSERVABILITY_URL")
	if url == "" {
		t.Skip("set SPLITTER_OBSERVABILITY_URL to query a real Prometheus")
	}
	c := New(Settings{URL: url, Job: "splitter", Timeout: 10 * time.Second})
	snap := c.Snapshot(context.Background(), 6*time.Hour, time.Now())
	if !snap.Prometheus.Reachable {
		t.Fatalf("prometheus = %+v", snap.Prometheus)
	}
	for _, p := range snap.Panels {
		t.Logf("%-14s %-8s %s %s", p.ID, p.Kind, p.Display, p.Error)
	}
}
