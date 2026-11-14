package app

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/net/dns/dnsmessage"

	"github.com/starriv/splitter/internal/dnstest"
	"github.com/starriv/splitter/internal/observability"
)

// call builds a request and serves it through h, returning the recorded
// response.
func call(t *testing.T, h http.Handler, method, target, body string, hdr map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := adminRequest(method, target, strings.NewReader(body))
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// adminRequest is httptest.NewRequest addressed to the loopback admin
// endpoint; the default Host "example.com" is refused by the rebinding guard.
func adminRequest(method, target string, body io.Reader) *http.Request {
	req := httptest.NewRequest(method, target, body)
	req.Host = "127.0.0.1:9198"
	return req
}

var jsonBody = map[string]string{"Content-Type": "application/json"}

// decode unmarshals the recorded response body into T and fails the test
// when the body is not valid JSON.
func decode[T any](t *testing.T, rec *httptest.ResponseRecorder) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatalf("decode %s: %v", rec.Body, err)
	}
	return v
}

// TestAPIState verifies that the state endpoint reports a consistent
// snapshot: rule evaluation order, per-rule counters, set sources and
// upstream statistics.
func TestAPIState(t *testing.T) {
	e := start(t)
	e.ask(t, "udp", "www.baidu.com.", dnsmessage.TypeA)
	e.ask(t, "udp", "www.baidu.com.", dnsmessage.TypeAAAA)
	e.ask(t, "udp", "www.google.com.", dnsmessage.TypeA)
	e.ask(t, "udp", "home.local.example.", dnsmessage.TypeA)
	e.ask(t, "udp", "version.bind.", dnsmessage.TypeTXT) // IN class: routed like any name

	rec := call(t, e.app.AdminHandler(), "GET", "/api/v1/state", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("state: %d %s", rec.Code, rec.Body)
	}
	st := decode[State](t, rec)

	if !st.Status.Ready || st.Status.Version != "test" || st.Status.Queries != 5 {
		t.Errorf("status = %+v", st.Status)
	}
	if st.Status.RCodes["NOERROR"] != 5 {
		t.Errorf("rcodes = %v", st.Status.RCodes)
	}

	// Rules come back in evaluation order with their own counters.
	want := []struct {
		name, action, target string
		queries              uint64
	}{
		{"local-zone", "answer", "local", 1},
		{"cn", "forward", "aliyun", 2},
		{"default", "forward", "cloudflare", 2},
	}
	if len(st.Rules) != len(want) {
		t.Fatalf("rules = %+v", st.Rules)
	}
	for i, w := range want {
		r := st.Rules[i]
		if r.Name != w.name || r.Action != w.action || r.Target != w.target || r.Queries != w.queries {
			t.Errorf("rule %d = %+v, want %+v", i, r, w)
		}
	}
	if a := st.Rules[0].Answer; a == nil || len(a.A) != 1 || a.A[0] != "192.0.2.10" || a.TTL != 60 {
		t.Errorf("local-zone answer = %+v", st.Rules[0].Answer)
	}
	if got := st.Rules[1].Sets; len(got) != 1 || got[0] != "cn" {
		t.Errorf("cn sets = %v", got)
	}

	if len(st.Upstreams) != 2 || st.Upstreams[0].Name != "aliyun" || st.Upstreams[1].Name != "cloudflare" {
		t.Fatalf("upstreams = %+v", st.Upstreams)
	}
	al := st.Upstreams[0].Servers[0]
	if al.Addr != e.cn.Addr.String() || al.Requests != 2 || al.Succeeded != 2 || al.Errors["timeout"] != 0 {
		t.Errorf("aliyun server = %+v", al)
	}
	if _, ok := al.Errors["timeout"]; !ok {
		t.Errorf("error reasons must be listed at 0: %v", al.Errors)
	}

	if len(st.Sets) != 2 || st.Sets[0].Name != "cn" || st.Sets[1].Name != "local" {
		t.Fatalf("sets = %+v", st.Sets)
	}
	cn := st.Sets[0]
	if cn.Total != 2 || cn.Entries["domain"] != 1 || cn.Entries["full"] != 1 || len(cn.UsedBy) != 1 || cn.UsedBy[0] != "cn" {
		t.Errorf("cn set = %+v", cn)
	}
	if len(cn.Sources) != 1 || cn.Sources[0].File != "cn-domains.txt" || cn.Sources[0].Modified.IsZero() {
		t.Errorf("cn sources = %+v", cn.Sources)
	}
	if st.Sets[1].Inline != 1 || len(st.Sets[1].Sources) != 0 {
		t.Errorf("local set = %+v", st.Sets[1])
	}
}

// TestAPIQueries verifies the queries endpoint from both sources:
// newest-first order, substring and rule filters, and validation of the limit
// parameter.
func TestAPIQueries(t *testing.T) {
	for _, source := range []string{"disk", "memory"} {
		t.Run(source, func(t *testing.T) {
			e := start(t, source == "memory")
			h := e.app.AdminHandler()
			for _, n := range []string{"www.baidu.com.", "www.google.com.", "home.local.example.", "tieba.baidu.com."} {
				e.ask(t, "udp", n, dnsmessage.TypeA)
			}

			all := decode[QueryPage](t, call(t, h, "GET", "/api/v1/queries", "", nil))
			if all.Total != 4 || all.Source != source || len(all.Queries) != 4 || all.Next != "" {
				t.Fatalf("queries = %+v", all)
			}
			if (source == "memory") != (all.Capacity == recentCapacity) {
				t.Errorf("capacity %d for %s", all.Capacity, source)
			}
			// Newest first.
			wantSeq := uint64(4)
			if source == "disk" {
				wantSeq = 0 // sequence numbers are not stored on disk
			}
			if q := all.Queries[0]; q.Name != "tieba.baidu.com" || q.Rule != "cn" || q.Set != "cn" || q.Match != "domain" ||
				q.Target != "aliyun" || q.RCode != "NOERROR" || q.Type != "A" || q.Proto != "udp" || q.Seq != wantSeq {
				t.Errorf("newest = %+v", q)
			}
			if q := all.Queries[2]; q.Name != "www.google.com" || q.Rule != "default" || q.Set != "" || q.Match != "" {
				t.Errorf("catch-all entry = %+v", q)
			}

			byName := decode[QueryPage](t, call(t, h, "GET", "/api/v1/queries?q=BAIDU", "", nil))
			if len(byName.Queries) != 2 {
				t.Errorf("q=BAIDU -> %+v", byName.Queries)
			}
			// "cn" occurs in other fields of every cn record; only names count.
			if got := decode[QueryPage](t, call(t, h, "GET", "/api/v1/queries?q=aliyun", "", nil)); len(got.Queries) != 0 {
				t.Errorf("q=aliyun matched a non-name field: %+v", got.Queries)
			}
			byRule := decode[QueryPage](t, call(t, h, "GET", "/api/v1/queries?rule=local-zone&limit=1", "", nil))
			if len(byRule.Queries) != 1 || byRule.Queries[0].Name != "home.local.example" {
				t.Errorf("rule=local-zone -> %+v", byRule.Queries)
			}
			for _, bad := range []string{"0", "-1", "x", "513"} {
				if rec := call(t, h, "GET", "/api/v1/queries?limit="+bad, "", nil); rec.Code != http.StatusBadRequest {
					t.Errorf("limit=%s: %d", bad, rec.Code)
				}
			}
			if rec := call(t, h, "GET", "/api/v1/queries?cursor=../../etc/passwd@0", "", nil); rec.Code != http.StatusBadRequest {
				t.Errorf("bad cursor: %d", rec.Code)
			}
		})
	}
}

// TestAPIQueriesPaging verifies that "next" walks the on-disk log back to the
// first query without repeating or skipping one, with and without a filter.
func TestAPIQueriesPaging(t *testing.T) {
	e := start(t)
	h := e.app.AdminHandler()
	const n = 25
	for i := range n {
		e.ask(t, "udp", fmt.Sprintf("p%02d.baidu.com.", i), dnsmessage.TypeA)
		e.ask(t, "udp", fmt.Sprintf("p%02d.example.org.", i), dnsmessage.TypeA)
	}
	walk := func(query string) []string {
		var names []string
		cursor := ""
		for range 100 {
			page := decode[QueryPage](t, call(t, h, "GET", "/api/v1/queries?limit=4&"+query+"&cursor="+url.QueryEscape(cursor), "", nil))
			for _, r := range page.Queries {
				names = append(names, r.Name)
			}
			if page.Next == "" {
				return names
			}
			cursor = page.Next
		}
		t.Fatal("paging does not end")
		return nil
	}
	all := walk("")
	if len(all) != 2*n || all[0] != "p24.example.org" || all[len(all)-1] != "p00.baidu.com" {
		t.Fatalf("all pages: %d records, %v … %v", len(all), all[:1], all[len(all)-1:])
	}
	cn := walk("rule=cn")
	if len(cn) != n || cn[0] != "p24.baidu.com" || cn[n-1] != "p00.baidu.com" {
		t.Fatalf("rule=cn pages: %v", cn)
	}
}

// TestAPIStateQueryLog verifies the query_log section of the state response.
func TestAPIStateQueryLog(t *testing.T) {
	e := start(t)
	h := e.app.AdminHandler()
	e.ask(t, "udp", "www.baidu.com.", dnsmessage.TypeA)
	st := decode[State](t, call(t, h, "GET", "/api/v1/state", "", nil)).Status.QueryLog
	if !st.Enabled || !st.Active || st.Dir != filepath.Join(e.dir, "logs") || st.Files != 1 || st.SizeBytes == 0 ||
		st.MaxSizeBytes != 4096<<20 || st.MaxAgeSeconds != (30*24*time.Hour).Seconds() || st.Oldest == nil {
		t.Errorf("query_log = %+v", st)
	}
}

// TestRecentRingWraps verifies that the ring retains the newest
// recentCapacity entries and reports the total number of queries ever
// recorded.
func TestRecentRingWraps(t *testing.T) {
	var r recent
	for i := 0; i < recentCapacity+10; i++ {
		r.add(recentEntry{name: "n", rule: "r"})
	}
	list, total := r.list(recentCapacity, "", "")
	if total != recentCapacity+10 || len(list) != recentCapacity {
		t.Fatalf("total %d, len %d", total, len(list))
	}
	if list[0].Seq != recentCapacity+10 || list[len(list)-1].Seq != 11 {
		t.Errorf("seq range %d..%d", list[0].Seq, list[len(list)-1].Seq)
	}
}

// TestRecentPages verifies that paging the ring with the Seq of the last
// record visits every matching entry once, newest first, and that the last
// page reports no more.
func TestRecentPages(t *testing.T) {
	var r recent
	for i := range 25 {
		rule := "a"
		if i%2 == 1 {
			rule = "b"
		}
		r.add(recentEntry{name: "n", rule: rule})
	}
	var seen []uint64
	before := uint64(0)
	for pages := 0; ; pages++ {
		if pages > 10 {
			t.Fatal("paging did not end")
		}
		list, more, _ := r.page(before, 4, "", "a")
		for _, rec := range list {
			seen = append(seen, rec.Seq)
		}
		if !more {
			break
		}
		before = list[len(list)-1].Seq
	}
	// Entries 1, 3, …, 25 carry rule "a": 13 of them, newest first.
	if len(seen) != 13 || seen[0] != 25 || seen[12] != 1 {
		t.Errorf("seen %v", seen)
	}
	for i := 1; i < len(seen); i++ {
		if seen[i] >= seen[i-1] {
			t.Fatalf("not strictly descending: %v", seen)
		}
	}
	// A full last page must not claim more.
	if _, more, _ := r.page(0, 13, "", "a"); more {
		t.Error("exact page reported more")
	}
}

// TestMemoryCursor verifies the cursor syntax of the in-memory source.
func TestMemoryCursor(t *testing.T) {
	if seq, err := parseMemoryCursor("m42"); err != nil || seq != 42 {
		t.Errorf("m42: %d, %v", seq, err)
	}
	for _, bad := range []string{"42", "m", "m0", "m-1", "seg@0", "mx"} {
		if _, err := parseMemoryCursor(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

// TestAPIRoute verifies that both route endpoint spellings explain a name
// without sending a query.
func TestAPIRoute(t *testing.T) {
	e := start(t)
	h := e.app.AdminHandler()
	want := RouteInfo{Name: "www.baidu.com", Type: "AAAA", Rule: "cn", Action: "forward", Target: "aliyun", Set: "cn", Match: "domain"}
	// The legacy spelling is retained for compatibility with existing
	// operational scripts.
	for _, p := range []string{"/api/v1/route", "/debug/route"} {
		rec := call(t, h, "GET", p+"?name=WWW.Baidu.com.&type=aaaa", "", nil)
		if got := decode[RouteInfo](t, rec); rec.Code != http.StatusOK || got != want {
			t.Errorf("%s = %d %+v", p, rec.Code, got)
		}
		if rec := call(t, h, "GET", p, "", nil); rec.Code != http.StatusBadRequest {
			t.Errorf("%s without name: %d", p, rec.Code)
		}
		if rec := call(t, h, "GET", p+"?name=a.com&type=NOPE", "", nil); rec.Code != http.StatusBadRequest {
			t.Errorf("%s bad type: %d", p, rec.Code)
		}
	}
	// Explaining a route sends no query.
	if e.cn.Queries.Load() != 0 || e.ab.Queries.Load() != 0 {
		t.Errorf("route reached an upstream: cn=%d ab=%d", e.cn.Queries.Load(), e.ab.Queries.Load())
	}
}

// TestAPIResolve verifies that resolve runs one real query through the
// routing pipeline, including local answers, NODATA results, rejection of
// invalid names, and accounting in the recent ring.
func TestAPIResolve(t *testing.T) {
	e := start(t)
	h := e.app.AdminHandler()

	rec := call(t, h, "POST", "/api/v1/resolve", `{"name":"WWW.Baidu.com","type":"a"}`, jsonBody)
	if rec.Code != http.StatusOK {
		t.Fatalf("resolve: %d %s", rec.Code, rec.Body)
	}
	res := decode[ResolveResult](t, rec)
	if res.RCode != "NOERROR" || res.Route.Rule != "cn" || res.Route.Target != "aliyun" ||
		len(res.Answers) != 1 || res.Answers[0].Data != cnAddr.String() || res.Answers[0].Type != "A" {
		t.Errorf("resolve = %+v", res)
	}
	if e.cn.Queries.Load() != 1 || e.ab.Queries.Load() != 0 {
		t.Errorf("hits cn=%d ab=%d", e.cn.Queries.Load(), e.ab.Queries.Load())
	}

	// The local zone is answered here; AAAA has no record, so NODATA.
	res = decode[ResolveResult](t, call(t, h, "POST", "/api/v1/resolve", `{"name":"home.local.example"}`, jsonBody))
	if res.Route.Rule != "local-zone" || len(res.Answers) != 1 || res.Answers[0].Data != localAddr.String() {
		t.Errorf("local = %+v", res)
	}
	res = decode[ResolveResult](t, call(t, h, "POST", "/api/v1/resolve", `{"name":"home.local.example","type":"AAAA"}`, jsonBody))
	if res.RCode != "NOERROR" || len(res.Answers) != 0 {
		t.Errorf("local AAAA = %+v", res)
	}

	// The request is a real query and therefore appears in the recent list.
	list, total := e.app.recent.list(10, "", "")
	if total != 3 || list[2].Name != "www.baidu.com" {
		t.Errorf("recent after resolve: total %d %+v", total, list)
	}

	long := strings.Repeat("a", 64) + ".com"
	for _, body := range []string{`{"name":""}`, `{"name":"` + long + `"}`, `{"name":"a.com","type":"NOPE"}`, `not json`} {
		if rec := call(t, h, "POST", "/api/v1/resolve", body, jsonBody); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: %d %s", body, rec.Code, rec.Body)
		}
	}
}

// TestAPIResolveUpstreamDown verifies that resolve reports SERVFAIL as data
// when the upstream does not answer, rather than an HTTP error.
func TestAPIResolveUpstreamDown(t *testing.T) {
	e := start(t)
	writeConfig(t, e.dir, e.cn.Addr, dnstest.BlackHole(t), "domain:baidu.com\n")
	if err := e.app.Reload(); err != nil {
		t.Fatal(err)
	}
	rec := call(t, e.app.AdminHandler(), "POST", "/api/v1/resolve", `{"name":"www.google.com"}`, jsonBody)
	res := decode[ResolveResult](t, rec)
	if rec.Code != http.StatusOK || res.RCode != "SERVFAIL" {
		t.Errorf("resolve with upstream down = %d %+v", rec.Code, res)
	}
}

// TestAPIReload verifies the reload endpoint: it applies a valid list,
// refuses a broken one with 422 while the previous runtime keeps serving,
// and records the failure in the state.
func TestAPIReload(t *testing.T) {
	e := start(t)
	h := e.app.AdminHandler()

	list := filepath.Join(e.dir, "cn-domains.txt")
	if err := os.WriteFile(list, []byte("domain:baidu.com\nfull:www.qq.com\ndomain:taobao.com\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	rec := call(t, h, "POST", "/api/v1/reload", `{}`, jsonBody)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"ok":true`) {
		t.Fatalf("reload: %d %s", rec.Code, rec.Body)
	}
	if got := e.app.Explain("www.taobao.com", dnsmessage.TypeA).Rule; got != "cn" {
		t.Errorf("after reload taobao -> %s", got)
	}

	// A broken list is refused with 422; the previous runtime keeps serving and
	// the state reports the reason.
	if err := os.WriteFile(list, []byte("regexp:(\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	rec = call(t, h, "POST", "/api/v1/reload", `{}`, jsonBody)
	if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), `"ok":false`) {
		t.Fatalf("bad reload: %d %s", rec.Code, rec.Body)
	}
	if got := e.app.Explain("www.taobao.com", dnsmessage.TypeA).Rule; got != "cn" {
		t.Errorf("a refused reload changed routing: taobao -> %s", got)
	}
	st := decode[State](t, call(t, h, "GET", "/api/v1/state", "", nil))
	if st.Status.Ready || st.Status.LastReload == nil || st.Status.LastReload.OK || st.Status.LastReload.Error == "" ||
		st.Status.ReloadsFailed != 1 {
		t.Errorf("status after refused reload = %+v", st.Status)
	}
}

// TestAPIWriteGuard verifies that requests from another origin in a browser
// cannot trigger the POST endpoints of the admin API.
func TestAPIWriteGuard(t *testing.T) {
	e := start(t)
	h := e.app.AdminHandler()
	before := e.app.Runtime()
	cases := []struct {
		name string
		hdr  map[string]string
		want int
	}{
		{"no content type (plain cross-site form/fetch)", nil, http.StatusUnsupportedMediaType},
		{"form content type", map[string]string{"Content-Type": "application/x-www-form-urlencoded"}, http.StatusUnsupportedMediaType},
		{"text/plain", map[string]string{"Content-Type": "text/plain"}, http.StatusUnsupportedMediaType},
		{"cross-site", map[string]string{"Content-Type": "application/json", "Sec-Fetch-Site": "cross-site"}, http.StatusForbidden},
		{"same-site but other origin", map[string]string{"Content-Type": "application/json", "Sec-Fetch-Site": "same-site"}, http.StatusForbidden},
	}
	for _, c := range cases {
		for _, p := range []string{"/api/v1/reload", "/api/v1/resolve"} {
			if rec := call(t, h, "POST", p, `{"name":"www.baidu.com"}`, c.hdr); rec.Code != c.want {
				t.Errorf("%s %s: %d, want %d", p, c.name, rec.Code, c.want)
			}
		}
	}
	if e.app.Runtime() != before {
		t.Error("a refused request reloaded the config")
	}
	if e.cn.Queries.Load() != 0 {
		t.Error("a refused request reached an upstream")
	}
	for _, site := range []string{"same-origin", "none"} {
		hdr := map[string]string{"Content-Type": "application/json; charset=utf-8", "Sec-Fetch-Site": site}
		if rec := call(t, h, "POST", "/api/v1/reload", `{}`, hdr); rec.Code != http.StatusOK {
			t.Errorf("Sec-Fetch-Site %s: %d %s", site, rec.Code, rec.Body)
		}
	}
	// Reads stay GET-only and writes POST-only.
	if rec := call(t, h, "GET", "/api/v1/reload", "", nil); rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET reload: %d", rec.Code)
	}
	if rec := call(t, h, "POST", "/api/v1/state", `{}`, jsonBody); rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST state: %d", rec.Code)
	}
	big := `{"name":"` + strings.Repeat("a", 8<<10) + `"}`
	if rec := call(t, h, "POST", "/api/v1/resolve", big, jsonBody); rec.Code != http.StatusBadRequest {
		t.Errorf("oversized body: %d", rec.Code)
	}
}

// TestAdminHostGuard verifies that a request carrying a foreign Host header,
// as a DNS-rebinding page would send, is refused on every admin path, while
// loopback names and an absent Host are accepted. Names from admin_hosts are
// covered by the config package tests.
func TestAdminHostGuard(t *testing.T) {
	e := start(t)
	h := e.app.AdminHandler()
	for _, p := range []string{"/", "/metrics", "/healthz", "/api/v1/state", "/api/v1/queries"} {
		req := httptest.NewRequest("GET", p, nil)
		req.Host = "rebind.attacker.example:9198"
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusMisdirectedRequest {
			t.Errorf("GET %s with a foreign Host: %d, want 421", p, rec.Code)
		}
	}
	req := httptest.NewRequest("POST", "/api/v1/reload", strings.NewReader("{}"))
	req.Host = "rebind.attacker.example:9198"
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusMisdirectedRequest {
		t.Errorf("rebinding reload: %d, want 421", rec.Code)
	}
	for _, host := range []string{"localhost:9198", "[::1]:9198", ""} {
		req := httptest.NewRequest("GET", "/healthz", nil)
		req.Host = host
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Errorf("Host %q: %d, want 200", host, rec.Code)
		}
	}
}

// TestAPIObservability verifies the wiring of the observability endpoint: it is
// disabled without a Prometheus URL, rejects an out-of-range window, and picks
// up the configured Prometheus after a reload.
func TestAPIObservability(t *testing.T) {
	e := start(t)
	h := e.app.AdminHandler()

	rec := call(t, h, "GET", "/api/v1/observability", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("observability: %d %s", rec.Code, rec.Body)
	}
	snap := decode[observability.Snapshot](t, rec)
	if snap.Prometheus.Configured || snap.Prometheus.Reachable || len(snap.Panels) != 0 {
		t.Fatalf("disabled snapshot = %+v", snap)
	}
	if snap.WindowSeconds != (6 * time.Hour).Seconds() {
		t.Errorf("default window = %v", snap.WindowSeconds)
	}

	if rec := call(t, h, "GET", "/api/v1/observability?range=1m", "", nil); rec.Code != http.StatusBadRequest {
		t.Errorf("short range: %d %s", rec.Code, rec.Body)
	}

	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		kind := "vector"
		if strings.HasSuffix(r.URL.Path, "query_range") {
			kind = "matrix"
		}
		fmt.Fprintf(w, `{"status":"success","data":{"resultType":%q,"result":[]}}`, kind)
	}))
	defer fake.Close()

	cfg, err := os.ReadFile(e.cfg)
	if err != nil {
		t.Fatal(err)
	}
	cfg = append(cfg, []byte("\nobservability:\n  prometheus_url: "+fake.URL+"\n  job: splitter\n")...)
	if err := os.WriteFile(e.cfg, cfg, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := e.app.Reload(); err != nil {
		t.Fatal(err)
	}
	rec = call(t, h, "GET", "/api/v1/observability?range=1h", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("observability after reload: %d %s", rec.Code, rec.Body)
	}
	snap = decode[observability.Snapshot](t, rec)
	if !snap.Prometheus.Configured || !snap.Prometheus.Reachable || snap.Prometheus.Error != "" {
		t.Fatalf("prometheus = %+v", snap.Prometheus)
	}
	if snap.WindowSeconds != time.Hour.Seconds() || snap.Prometheus.Job != "splitter" {
		t.Errorf("snapshot = %+v", snap)
	}
	if len(snap.Panels) != 20 {
		t.Errorf("panels = %d", len(snap.Panels))
	}
}
