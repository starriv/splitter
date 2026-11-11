package metrics

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// scrape renders the metrics handler and returns the exposition text.
func scrape(t *testing.T, m *Metrics) string {
	t.Helper()
	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("scrape: %d %s", rec.Code, rec.Body)
	}
	return rec.Body.String()
}

// TestFailureSeriesExistAtZero verifies that counters used by alerts for "did
// this ever fail" exist before the first failure; otherwise the first failure
// creates a new series with no previous sample and no increase to observe.
func TestFailureSeriesExistAtZero(t *testing.T) {
	m := New("v", "c")
	m.InitUpstream("abroad", []string{"1.1.1.1:53"})
	body := scrape(t, m)
	for _, s := range []string{
		`splitter_reloads_total{result="failure"} 0`,
		`splitter_reloads_total{result="success"} 0`,
		`splitter_overload_dropped_total{proto="tcp"} 0`,
		`splitter_overload_dropped_total{proto="udp"} 0`,
		`splitter_response_write_errors_total{proto="udp"} 0`,
		`splitter_panics_total 0`,
		`splitter_upstream_requests_total{server="1.1.1.1:53",upstream="abroad"} 0`,
		`splitter_upstream_errors_total{reason="canceled",server="1.1.1.1:53",upstream="abroad"} 0`,
		`splitter_upstream_errors_total{reason="mismatch",server="1.1.1.1:53",upstream="abroad"} 0`,
		`splitter_upstream_errors_total{reason="network",server="1.1.1.1:53",upstream="abroad"} 0`,
		`splitter_upstream_errors_total{reason="timeout",server="1.1.1.1:53",upstream="abroad"} 0`,
		`splitter_upstream_retransmits_total{server="1.1.1.1:53",upstream="abroad"} 0`,
	} {
		if !strings.Contains(body, s+"\n") {
			t.Errorf("missing %s", s)
		}
	}
}

// TestReloadResultGauge verifies that a successful load sets the reload gauge to
// 1, and that a refused load sets it to 0 and increments the failure counter.
func TestReloadResultGauge(t *testing.T) {
	m := New("v", "c")
	m.Loaded(nil, nil)
	if !strings.Contains(scrape(t, m), "splitter_config_last_reload_successful 1\n") {
		t.Error("not 1 after a successful load")
	}
	m.LoadFailed()
	body := scrape(t, m)
	if !strings.Contains(body, "splitter_config_last_reload_successful 0\n") ||
		!strings.Contains(body, `splitter_reloads_total{result="failure"} 1`+"\n") {
		t.Error("a refused load is not visible")
	}
}

// TestSnapshotScrape verifies that the freshness gauge carries one sample per
// (set, file) and that the collector remains scrapeable when several sets share
// the same file.
func TestSnapshotScrape(t *testing.T) {
	m := New("v", "c")
	at := time.Unix(1700000000, 0)
	m.Loaded([]SetEntries{{Set: "cn", Kind: "domain", Count: 3}, {Set: "ads", Kind: "domain", Count: 1}},
		[]Source{{Set: "cn", File: "/etc/splitter/geosite.dat", MTime: at}, {Set: "ads", File: "/etc/splitter/geosite.dat", MTime: at}})
	body := scrape(t, m)
	for _, s := range []string{
		`splitter_set_entries{kind="domain",set="cn"} 3`,
		`splitter_set_source_modified_timestamp_seconds{file="/etc/splitter/geosite.dat",set="ads"} 1.7e+09`,
	} {
		if !strings.Contains(body, s+"\n") {
			t.Errorf("missing %s", s)
		}
	}
}
