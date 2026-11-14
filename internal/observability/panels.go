package observability

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Snapshot is the response of GET /api/v1/observability: the aligned time grid
// and every panel of the observability page, read from one refresh.
type Snapshot struct {
	GeneratedAt   time.Time      `json:"generated_at"`
	WindowSeconds float64        `json:"window_seconds"`
	StartMS       int64          `json:"start_ms"`
	EndMS         int64          `json:"end_ms"`
	StepMS        int64          `json:"step_ms"`
	Prometheus    PrometheusInfo `json:"prometheus"`
	Panels        []Panel        `json:"panels"`
}

// PrometheusInfo describes the data source and, when it could not be read, why.
type PrometheusInfo struct {
	Configured   bool   `json:"configured"`
	URL          string `json:"url,omitempty"`
	Job          string `json:"job,omitempty"`
	DashboardURL string `json:"dashboard_url,omitempty"`
	Reachable    bool   `json:"reachable"`
	Error        string `json:"error,omitempty"`
}

// Panel is one card, chart or table. Kind selects the fields that are set:
// "stat" uses Value/Display/Tone/Context, "series" uses Series (whose sample
// values are aligned with the snapshot grid), and "table" uses Columns/Rows.
type Panel struct {
	ID          string   `json:"id"`
	Section     string   `json:"section"` // status, flow, detail, runtime
	Kind        string   `json:"kind"`    // stat, series or table
	Title       string   `json:"title"`
	Description string   `json:"description"`
	Unit        string   `json:"unit"` // state, count, reqps, percent, ms, duration, number
	Stacked     bool     `json:"stacked,omitempty"`
	Value       *float64 `json:"value,omitempty"`
	Display     string   `json:"display,omitempty"`
	Tone        string   `json:"tone,omitempty"`
	Context     string   `json:"context,omitempty"`
	Series      []Series `json:"series,omitempty"`
	Columns     []Column `json:"columns,omitempty"`
	Rows        []Row    `json:"rows,omitempty"`
	Error       string   `json:"error,omitempty"`
}

// Series is one chart line. Values[i] belongs to the sample at
// StartMS+i*StepMS; nil marks a missing sample.
type Series struct {
	Name   string     `json:"name"`
	Color  string     `json:"color"` // a colour token: teal, sky, violet, amber, orchid, rose, green, muted
	Values []*float64 `json:"values"`
	// Line marks an aggregate drawn over the stack rather than inside it (the
	// dashboard's "总计" / "总体" series). Without it a stacked chart would
	// count the total twice.
	Line bool `json:"line,omitempty"`
}

// Column describes one table column.
type Column struct {
	Key      string `json:"key"`
	Title    string `json:"title"`
	Unit     string `json:"unit"`
	Decimals int    `json:"decimals,omitempty"`
	Numeric  bool   `json:"numeric,omitempty"`
}

// Row is one table row; the key cell holds the series name.
type Row struct {
	Key   string          `json:"key"`
	Cells map[string]Cell `json:"cells"`
}

// Cell is one table cell. Text overrides the formatted Value; Tone colours it.
type Cell struct {
	Value *float64 `json:"value,omitempty"`
	Text  string   `json:"text,omitempty"`
	Tone  string   `json:"tone,omitempty"`
}

// Panel sections, in dashboard order.
const (
	sectionStatus  = "status"
	sectionFlow    = "flow"
	sectionDetail  = "detail"
	sectionRuntime = "runtime"
)

// longWindow is the look-back of the "cumulative" KPI. It matches the Grafana
// dashboard, whose Prometheus keeps 15 days.
const longWindow = 15 * 24 * time.Hour

// Snapshot reads every panel. A nil client (the feature is not configured)
// returns a snapshot with Configured=false and no panels. A Prometheus that
// cannot be reached returns Reachable=false and the reason; the page then shows
// the error instead of stale numbers.
func (c *Client) Snapshot(ctx context.Context, window time.Duration, now time.Time) Snapshot {
	if window < MinWindow || window > MaxWindow {
		window = DefaultWindow
	}
	if now.IsZero() {
		now = time.Now()
	}
	now = now.Truncate(time.Second)
	start := now.Add(-window)
	step := stepFor(window)
	stepMS := step.Milliseconds()
	endMS := now.UnixMilli()
	startMS := start.UnixMilli()
	n := int((endMS-startMS)/stepMS) + 1
	snap := Snapshot{
		GeneratedAt:   now.UTC(),
		WindowSeconds: window.Seconds(),
		StartMS:       startMS,
		EndMS:         endMS,
		StepMS:        stepMS,
		Prometheus:    PrometheusInfo{Configured: c != nil},
		Panels:        []Panel{},
	}
	if c == nil {
		return snap
	}
	snap.Prometheus.URL = c.base.Redacted()
	snap.Prometheus.Job = c.job
	snap.Prometheus.DashboardURL = c.dashboard

	ctx, cancel := context.WithTimeout(ctx, snapshotTimeout)
	defer cancel()
	// A cheap vector query proves that the endpoint speaks the Prometheus API
	// and that the configured address is reachable; individual panels then
	// report their own failures.
	if _, err := c.query(ctx, "vector(1)", now); err != nil {
		snap.Prometheus.Error = err.Error()
		return snap
	}
	snap.Prometheus.Reachable = true

	b := &builder{
		client: c, ctx: ctx, start: start, end: now, step: step,
		startMS: startMS, stepMS: stepMS, n: n,
		windowSecs: int64(window.Seconds()),
	}
	b.srv = b.serverLabel()
	snap.Panels = b.build()
	return snap
}

// builder holds everything one snapshot refresh shares.
type builder struct {
	client     *Client
	ctx        context.Context
	start, end time.Time
	startMS    int64
	stepMS     int64
	step       time.Duration
	n          int    // samples per series
	windowSecs int64  // selected range in seconds, for PromQL
	srv        string // label holding the upstream server address
}

// build runs the panels concurrently and returns them in dashboard order.
func (b *builder) build() []Panel {
	builders := []func() Panel{
		b.scrapeStat, b.reloadStat, b.totalQueriesStat, b.windowQueriesStat,
		b.queryRateStat, b.answerSuccessStat, b.latencyStat,
		b.queryRateChart, b.totalTrendChart, b.rcodeChart, b.durationChart,
		b.rulesPanel, b.upstreamPanel, b.upstreamRTTChart, b.upstreamIssuesChart,
		b.reloadAgeStat, b.setEntriesStat, b.listAgeStat, b.invalidQueriesStat, b.protectionChart,
	}
	panels := make([]Panel, len(builders))
	sem := make(chan struct{}, queryConcurrency)
	var wg sync.WaitGroup
	for i := range builders {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			panels[i] = builders[i]()
		}(i)
	}
	wg.Wait()
	return panels
}

// serverLabel reports which label carries the upstream server address. When the
// Prometheus scrape job sets its own "server" label (the usual deployment), the
// splitter's label is renamed to "exported_server" by Prometheus; otherwise the
// original "server" label survives.
func (b *builder) serverLabel() string {
	vs, err := b.client.query(b.ctx, "count by (exported_server) (splitter_upstream_requests_total)", b.end)
	if err == nil {
		for _, v := range vs {
			if strings.TrimSpace(v.labels["exported_server"]) != "" {
				return "exported_server"
			}
		}
	}
	return "server"
}

// ── Panels: status ───────────────────────────────────────────────────────────

func (b *builder) scrapeStat() Panel {
	p := panel("scrape", sectionStatus, "splitter 抓取",
		"Prometheus 抓取 splitter 的 /metrics。异常表示指标不可达：分流器可能已停止，前置解析器会转发失败。", "state")
	v, ok, err := b.scalar(fmt.Sprintf(`min(up{job=%q})`, b.client.job))
	if err != nil {
		p.Error = err.Error()
		return p
	}
	p.Context = "job " + b.client.job
	if !ok {
		p.Display, p.Tone = "未采集", "muted"
		return p
	}
	p.Value = ptr(v)
	switch {
	case v >= 1:
		p.Display, p.Tone = "正常", "success"
	case v == 0:
		p.Display, p.Tone = "异常", "danger"
	default:
		p.Display, p.Tone = fixed(v, 2), "muted"
	}
	return p
}

func (b *builder) reloadStat() Panel {
	p := panel("reload", sectionStatus, "splitter · 配置加载",
		"最近一次配置/列表加载是否被接受。被拒时继续用旧配置；看「热加载距今」与 splitter 日志。", "state")
	v, ok, err := b.scalar("min(splitter_config_last_reload_successful)")
	if err != nil {
		p.Error = err.Error()
		return p
	}
	if !ok {
		p.Display, p.Tone = "未采集", "muted"
		return p
	}
	p.Value = ptr(v)
	switch {
	case v >= 1:
		p.Display, p.Tone = "配置正常", "success"
	case v == 0:
		p.Display, p.Tone = "加载被拒", "danger"
	default:
		p.Display, p.Tone = fixed(v, 2), "muted"
	}
	return p
}

func (b *builder) totalQueriesStat() Panel {
	p := panel("queries_15d", sectionStatus, "累积总解析量",
		"Prometheus 保留期（15 天）内的业务查询累计（increase），跨重启、排除 _invalid；所选窗口下一定 ≥ 区间解析量。", "count")
	v, ok, err := b.scalar(fmt.Sprintf(`sum(increase(splitter_queries_total{rule!="_invalid"}[%ds]))`, int64(longWindow.Seconds())))
	if err != nil {
		p.Error = err.Error()
		return p
	}
	p.Context = "Prometheus 保留期内累计，跨重启"
	if !ok {
		p.Display, p.Tone = "未采集", "muted"
		return p
	}
	p.Value = ptr(v)
	p.Display = formatStat(v, p.Unit)
	return p
}

func (b *builder) windowQueriesStat() Panel {
	p := panel("queries_window", sectionStatus, "区间解析量",
		"所选时间范围内的业务查询累计（increase），排除 rule=\"_invalid\" 的健康检查与畸形查询；与累计总量使用同一计数口径。", "count")
	v, ok, err := b.scalar(fmt.Sprintf(`sum(increase(splitter_queries_total{rule!="_invalid"}[%ds]))`, b.windowSecs))
	if err != nil {
		p.Error = err.Error()
		return p
	}
	if !ok {
		p.Display, p.Tone = "未采集", "muted"
		return p
	}
	p.Value = ptr(v)
	p.Display = formatStat(v, p.Unit)
	return p
}

func (b *builder) queryRateStat() Panel {
	p := panel("qps", sectionStatus, "当前查询速率",
		"splitter 最近 5 分钟业务查询速率（不含 _invalid）。", "reqps")
	v, ok, err := b.scalar(`sum(rate(splitter_queries_total{rule!="_invalid"}[5m]))`)
	if err != nil {
		p.Error = err.Error()
		return p
	}
	p.Context = "最近 5 分钟平均"
	if !ok {
		p.Display, p.Tone = "未采集", "muted"
		return p
	}
	p.Value = ptr(v)
	p.Display = formatStat(v, p.Unit)
	return p
}

func (b *builder) answerSuccessStat() Panel {
	p := panel("answer_success", sectionStatus, "应答成功率",
		"1 - SERVFAIL / 业务查询；REFUSED、NXDOMAIN 都是有效应答，不算失败。分母排除 _invalid 健康检查。", "percent")
	expr := fmt.Sprintf(`100 * (1 - sum(increase(splitter_responses_total{rcode="SERVFAIL"}[%ds])) / clamp_min(sum(increase(splitter_queries_total{rule!="_invalid"}[%ds])), 1))`,
		b.windowSecs, b.windowSecs)
	v, ok, err := b.scalar(expr)
	if err != nil {
		p.Error = err.Error()
		return p
	}
	p.Context = "1 − SERVFAIL / 业务查询"
	if !ok {
		p.Display, p.Tone = "未采集", "muted"
		return p
	}
	p.Value = ptr(v)
	p.Display = formatStat(v, p.Unit)
	p.Tone = toneAt(v, threshold{0, "danger"}, threshold{95, "warning"}, threshold{99, "success"})
	return p
}

func (b *builder) latencyStat() Panel {
	p := panel("latency", sectionStatus, "平均耗时",
		"端到端平均处理耗时（含上游等待，排除 _invalid）；P95 见「处理耗时 P95」。", "ms")
	v, ok, err := b.scalar(`1000 * sum(rate(splitter_request_duration_seconds_sum{rule!="_invalid"}[5m])) / clamp_min(sum(rate(splitter_request_duration_seconds_count{rule!="_invalid"}[5m])), 0.000001)`)
	if err != nil {
		p.Error = err.Error()
		return p
	}
	p.Context = "最近 5 分钟平均，含上游等待"
	if !ok {
		p.Display, p.Tone = "未采集", "muted"
		return p
	}
	p.Value = ptr(v)
	p.Display = formatStat(v, p.Unit)
	p.Tone = toneAt(v, threshold{0, "success"}, threshold{250, "warning"}, threshold{1000, "danger"})
	return p
}

func (b *builder) reloadAgeStat() Panel {
	p := panel("reload_age", sectionRuntime, "热加载距今",
		"最近一次成功加载配置/列表距今；周更列表正常时 < 7 天，> 14 天要查列表定时器。", "duration")
	v, ok, err := b.scalar("time() - max(splitter_last_reload_success_timestamp_seconds)")
	if err != nil {
		p.Error = err.Error()
		return p
	}
	p.Context = "最近一次成功加载（任一实例）"
	if !ok {
		p.Display, p.Tone = "未采集", "muted"
		return p
	}
	p.Value = ptr(v)
	p.Display = formatStat(v, p.Unit)
	p.Tone = toneAt(v, threshold{0, "success"}, threshold{7 * 86400, "warning"}, threshold{14 * 86400, "danger"})
	return p
}

func (b *builder) setEntriesStat() Panel {
	p := panel("set_entries", sectionRuntime, "集合条目",
		"splitter 加载的域名集合条目数（domain + full + keyword + regexp 之和）；列表周更后应保持同量级。", "count")
	vs, err := b.client.query(b.ctx, "sum by (set) (splitter_set_entries)", b.end)
	if err != nil {
		p.Error = err.Error()
		return p
	}
	if len(vs) == 0 {
		p.Display, p.Tone = "未采集", "muted"
		return p
	}
	type setCount struct {
		name string
		n    float64
	}
	sets := make([]setCount, 0, len(vs))
	total := 0.0
	for _, v := range vs {
		sets = append(sets, setCount{name: v.labels["set"], n: v.value})
		total += v.value
	}
	sort.Slice(sets, func(i, j int) bool { return sets[i].name < sets[j].name })
	parts := make([]string, 0, len(sets))
	for _, s := range sets {
		name := s.name
		if name == "" {
			name = "其他"
		}
		parts = append(parts, name+" "+groupInt(int64(math.Round(s.n))))
	}
	p.Value = ptr(total)
	p.Display = formatStat(total, p.Unit)
	p.Context = strings.Join(parts, " · ")
	return p
}

func (b *builder) listAgeStat() Panel {
	p := panel("list_age", sectionRuntime, "列表更新距今",
		"splitter 加载的列表文件最近一次修改距今；周更列表正常时 < 7 天，> 14 天要查更新任务。时间取最近一次成功加载时文件的 mtime。", "duration")
	v, ok, err := b.scalar("time() - max(splitter_set_source_modified_timestamp_seconds)")
	if err != nil {
		p.Error = err.Error()
		return p
	}
	p.Context = "最近一次列表文件修改（任一集合）"
	if !ok {
		p.Display, p.Tone = "未采集", "muted"
		return p
	}
	p.Value = ptr(v)
	p.Display = formatStat(v, p.Unit)
	p.Tone = toneAt(v, threshold{0, "success"}, threshold{7 * 86400, "warning"}, threshold{14 * 86400, "danger"})
	return p
}

func (b *builder) invalidQueriesStat() Panel {
	p := panel("invalid", sectionRuntime, "无效查询 · 所选区间",
		"rule=\"_invalid\" 的本地拒绝：健康检查 CH TXT + 畸形查询。它们不计入业务累计，保留在这里便于识别异常扫描。", "count")
	v, ok, err := b.scalar(fmt.Sprintf(`sum(increase(splitter_queries_total{rule="_invalid"}[%ds]))`, b.windowSecs))
	if err != nil {
		p.Error = err.Error()
		return p
	}
	p.Context = "本地拒绝，不计入业务累计"
	if !ok {
		p.Display, p.Tone = "未采集", "muted"
		return p
	}
	p.Value = ptr(v)
	p.Display = formatStat(v, p.Unit)
	return p
}

// ── Panels: charts ───────────────────────────────────────────────────────────

func (b *builder) queryRateChart() Panel {
	p := panel("query_rate", sectionFlow, "查询速率 · 按规则与动作",
		"每条规则决定了多少查询（5 分钟速率，不含 _invalid）。answer 是本地域应答，forward 是转发上游，reject 是本地拒绝。", "reqps")
	p.Stacked = true
	raw, err := b.rangeSeries(`sum by (rule, action) (rate(splitter_queries_total{rule!="_invalid"}[5m]))`,
		func(l map[string]string) string { return joinNonEmpty(" · ", l["rule"], l["action"]) },
		func(l map[string]string) string { return l["rule"] })
	if err != nil {
		p.Error = err.Error()
		return p
	}
	total, err := b.rangeSeries(`sum(rate(splitter_queries_total{rule!="_invalid"}[5m]))`,
		func(map[string]string) string { return "总计" },
		func(map[string]string) string { return "总计" })
	if err != nil {
		p.Error = err.Error()
		return p
	}
	return withSeries(p, append(raw, total...))
}

func (b *builder) totalTrendChart() Panel {
	p := panel("query_total", sectionFlow, "查询总量 · 当前进程计数趋势",
		"当前 splitter 进程的原始累计计数器（不含 _invalid），按规则拆分；进程重启处归零。15 天跨重启总量看顶部 KPI，增长斜率看「查询速率」。", "count")
	raw, err := b.rangeSeries(`sum by (rule) (splitter_queries_total{rule!="_invalid"})`,
		func(l map[string]string) string { return l["rule"] },
		func(l map[string]string) string { return l["rule"] })
	if err != nil {
		p.Error = err.Error()
		return p
	}
	total, err := b.rangeSeries(`sum(splitter_queries_total{rule!="_invalid"})`,
		func(map[string]string) string { return "总计" },
		func(map[string]string) string { return "总计" })
	if err != nil {
		p.Error = err.Error()
		return p
	}
	return withSeries(p, append(raw, total...))
}

func (b *builder) rcodeChart() Panel {
	p := panel("rcodes", sectionFlow, "响应 RCODE · 速率",
		"回给前置解析器的 RCODE 分布（堆叠高度 = 总应答速率）。SERVFAIL 上升通常意味着上游超时/链路问题；NXDOMAIN 是正常「域名不存在」，REFUSED 多来自本地拒绝的无效查询。", "reqps")
	p.Stacked = true
	raw, err := b.rangeSeries(`sum by (rcode) (rate(splitter_responses_total[5m]))`,
		func(l map[string]string) string { return l["rcode"] },
		func(l map[string]string) string { return l["rcode"] })
	if err != nil {
		p.Error = err.Error()
		return p
	}
	return withSeries(p, raw)
}

func (b *builder) durationChart() Panel {
	p := panel("duration_p95", sectionFlow, "处理耗时 P95",
		"端到端处理耗时（收到查询到生成应答）的 P95，含上游等待，按规则拆分；排除 _invalid 健康检查。", "ms")
	raw, err := b.rangeSeries(`1000 * histogram_quantile(0.95, sum by (rule, le) (rate(splitter_request_duration_seconds_bucket{rule!="_invalid"}[5m])))`,
		func(l map[string]string) string { return l["rule"] },
		func(l map[string]string) string { return l["rule"] })
	if err != nil {
		p.Error = err.Error()
		return p
	}
	overall, err := b.rangeSeries(`1000 * histogram_quantile(0.95, sum by (le) (rate(splitter_request_duration_seconds_bucket{rule!="_invalid"}[5m])))`,
		func(map[string]string) string { return "总体" },
		func(map[string]string) string { return "总体" })
	if err != nil {
		p.Error = err.Error()
		return p
	}
	return withSeries(p, append(raw, overall...))
}

func (b *builder) upstreamRTTChart() Panel {
	p := panel("upstream_rtt", sectionDetail, "上游 RTT P95",
		"每台上游成功交换的 P95 往返耗时（5 分钟窗口）。P95 抬到 1–2 秒且重传同时上升时，先看隧道/链路。失败尝试在「上游失败 · 重传 · 丢弃」里，不混进 RTT。", "ms")
	raw, err := b.rangeSeries(fmt.Sprintf(`1000 * histogram_quantile(0.95, sum by (upstream, %s, le) (rate(splitter_upstream_duration_seconds_bucket[5m])))`, b.srv),
		func(l map[string]string) string { return joinNonEmpty(" · ", l["upstream"], l[b.srv]) },
		func(l map[string]string) string { return l["upstream"] })
	if err != nil {
		p.Error = err.Error()
		return p
	}
	return withSeries(p, raw)
}

func (b *builder) upstreamIssuesChart() Panel {
	p := panel("upstream_issues", sectionDetail, "上游失败 · 重传 · 丢弃",
		"失败原因：timeout / network / mismatch / canceled。mismatch 持续出现要怀疑伪造应答或上游串包；重传说明去程/回程 UDP 丢包；丢弃是 ID/问题不匹配。", "reqps")
	failures, err := b.rangeSeries(fmt.Sprintf(`sum by (upstream, %s, reason) (rate(splitter_upstream_errors_total[5m]))`, b.srv),
		func(l map[string]string) string {
			return "失败 · " + joinNonEmpty(" · ", l["upstream"], l[b.srv], l["reason"])
		},
		func(l map[string]string) string { return l["reason"] })
	if err != nil {
		p.Error = err.Error()
		return p
	}
	retrans, err := b.rangeSeries(fmt.Sprintf(`sum by (upstream, %s) (rate(splitter_upstream_retransmits_total[5m]))`, b.srv),
		func(l map[string]string) string { return "重传 · " + joinNonEmpty(" · ", l["upstream"], l[b.srv]) },
		func(map[string]string) string { return "重传" })
	if err != nil {
		p.Error = err.Error()
		return p
	}
	dropped, err := b.rangeSeries(fmt.Sprintf(`sum by (upstream, %s) (rate(splitter_upstream_dropped_responses_total[5m]))`, b.srv),
		func(l map[string]string) string { return "丢弃 · " + joinNonEmpty(" · ", l["upstream"], l[b.srv]) },
		func(map[string]string) string { return "丢弃" })
	if err != nil {
		p.Error = err.Error()
		return p
	}
	return withSeries(p, append(append(failures, retrans...), dropped...))
}

func (b *builder) protectionChart() Panel {
	p := panel("protection", sectionRuntime, "运行与保护 · 丢弃 / 失败 / 并发",
		"过载丢弃、应答写失败、畸形查询、热加载失败、panic 都是速率；inflight/tcp 是瞬时值。任何 panic > 0 都是缺陷；写失败 = 应答已生成但没送达客户端。", "number")
	queries := []struct {
		expr, name string
	}{
		{`sum(rate(splitter_overload_dropped_total[5m]))`, "过载丢弃"},
		{`sum(rate(splitter_response_write_errors_total[5m]))`, "应答写失败"},
		{`sum(rate(splitter_malformed_queries_total[5m]))`, "畸形查询"},
		{`sum(rate(splitter_reloads_total{result="failure"}[5m]))`, "热加载失败"},
		{`sum(splitter_inflight_queries)`, "处理中查询"},
		{`sum(splitter_tcp_connections)`, "客户端 TCP 连接"},
		{`sum(rate(splitter_panics_total[5m]))`, "panic"},
	}
	var raw []rawSeries
	for _, q := range queries {
		name := q.name
		got, err := b.rangeSeries(q.expr,
			func(map[string]string) string { return name },
			func(map[string]string) string { return name })
		if err != nil {
			p.Error = err.Error()
			return p
		}
		raw = append(raw, got...)
	}
	return withSeries(p, raw)
}

// ── Panels: tables ───────────────────────────────────────────────────────────

func (b *builder) rulesPanel() Panel {
	p := tablePanel(panel("rules", sectionDetail, "解析详情 · 按规则",
		"所选区间的业务查询累计、当前速率与占比；rule/action/target 均来自配置。rule=\"_invalid\" 已排除，其计数见「运行与集合」。逐条查询见「最近查询」页。", "count"))
	incExpr := fmt.Sprintf(`sum by (rule, action, target) (increase(splitter_queries_total{rule!="_invalid"}[%ds]))`, b.windowSecs)
	inc, err := b.client.query(b.ctx, incExpr, b.end)
	if err != nil {
		p.Error = err.Error()
		return p
	}
	rate, err := b.client.query(b.ctx, `sum by (rule, action, target) (rate(splitter_queries_total{rule!="_invalid"}[5m]))`, b.end)
	if err != nil {
		p.Error = err.Error()
		return p
	}
	type ruleRow struct {
		key        string
		queries    float64
		queriesSet bool
		rate       float64
		rateSet    bool
	}
	rows := map[string]*ruleRow{}
	get := func(l map[string]string) *ruleRow {
		key := joinNonEmpty(" · ", l["rule"], l["action"], l["target"])
		r := rows[key]
		if r == nil {
			r = &ruleRow{key: key}
			rows[key] = r
		}
		return r
	}
	total := 0.0
	for _, v := range inc {
		r := get(v.labels)
		r.queries, r.queriesSet = v.value, true
		total += v.value
	}
	for _, v := range rate {
		r := get(v.labels)
		r.rate, r.rateSet = v.value, true
	}
	ordered := make([]*ruleRow, 0, len(rows))
	for _, r := range rows {
		ordered = append(ordered, r)
	}
	sort.Slice(ordered, func(i, j int) bool {
		if ordered[i].queries != ordered[j].queries {
			return ordered[i].queries > ordered[j].queries
		}
		return ordered[i].key < ordered[j].key
	})
	p.Columns = []Column{
		{Key: "key", Title: "规则 · 动作 · 目标"},
		{Key: "queries", Title: "累计查询", Unit: "count", Numeric: true},
		{Key: "rate", Title: "当前速率", Unit: "reqps", Decimals: 3, Numeric: true},
		{Key: "share", Title: "占比", Unit: "percent", Decimals: 1, Numeric: true},
	}
	for _, r := range ordered {
		cells := map[string]Cell{
			"queries": cellValue(r.queriesSet, r.queries),
			"rate":    cellValue(r.rateSet, r.rate),
		}
		share := 0.0
		if total > 0 {
			share = r.queries / total * 100
		}
		cells["share"] = cellValue(true, share)
		p.Rows = append(p.Rows, Row{Key: r.key, Cells: cells})
	}
	return p
}

func (b *builder) upstreamPanel() Panel {
	p := tablePanel(panel("upstream_table", sectionDetail, "上游详情 · 按服务器",
		"按上游服务器聚合；成功率 = 1 - 失败尝试/总尝试（每次失败尝试都计数，含被下一台救回的）。P95 只统计成功交换；重传 = 半超时未答重发；丢弃 = ID/问题不匹配的迟到或伪造应答。", "count"))
	srv := b.srv
	exprs := map[string]string{
		"attempts": fmt.Sprintf(`sum by (upstream, %s) (increase(splitter_upstream_requests_total[%ds]))`, srv, b.windowSecs),
		"failures": fmt.Sprintf(`sum by (upstream, %s) (increase(splitter_upstream_errors_total[%ds]))`, srv, b.windowSecs),
		"p95":      fmt.Sprintf(`1000 * histogram_quantile(0.95, sum by (upstream, %s, le) (rate(splitter_upstream_duration_seconds_bucket[5m])))`, srv),
		"retrans":  fmt.Sprintf(`sum by (upstream, %s) (increase(splitter_upstream_retransmits_total[%ds]))`, srv, b.windowSecs),
		"dropped":  fmt.Sprintf(`sum by (upstream, %s) (increase(splitter_upstream_dropped_responses_total[%ds]))`, srv, b.windowSecs),
	}
	type upstreamRow struct {
		key    string
		values map[string]float64
	}
	rows := map[string]*upstreamRow{}
	for _, field := range []string{"attempts", "failures", "p95", "retrans", "dropped"} {
		vs, err := b.client.query(b.ctx, exprs[field], b.end)
		if err != nil {
			p.Error = err.Error()
			return p
		}
		for _, v := range vs {
			key := joinNonEmpty(" · ", v.labels["upstream"], v.labels[srv])
			r := rows[key]
			if r == nil {
				r = &upstreamRow{key: key, values: map[string]float64{}}
				rows[key] = r
			}
			r.values[field] = v.value
		}
	}
	ordered := make([]*upstreamRow, 0, len(rows))
	for _, r := range rows {
		ordered = append(ordered, r)
	}
	sort.Slice(ordered, func(i, j int) bool {
		if ordered[i].values["attempts"] != ordered[j].values["attempts"] {
			return ordered[i].values["attempts"] > ordered[j].values["attempts"]
		}
		return ordered[i].key < ordered[j].key
	})
	p.Columns = []Column{
		{Key: "key", Title: "上游 · 服务器"},
		{Key: "attempts", Title: "尝试", Unit: "count", Numeric: true},
		{Key: "failures", Title: "失败", Unit: "count", Numeric: true},
		{Key: "success", Title: "成功率", Unit: "percent", Decimals: 2, Numeric: true},
		{Key: "p95", Title: "P95", Unit: "ms", Decimals: 1, Numeric: true},
		{Key: "retrans", Title: "重传", Unit: "count", Numeric: true},
		{Key: "dropped", Title: "丢弃", Unit: "count", Numeric: true},
	}
	for _, r := range ordered {
		cells := map[string]Cell{
			"attempts": cellValue(true, r.values["attempts"]),
			"failures": cellValue(true, r.values["failures"]),
			"retrans":  cellValue(true, r.values["retrans"]),
			"dropped":  cellValue(true, r.values["dropped"]),
		}
		if p95, ok := r.values["p95"]; ok {
			cells["p95"] = cellValue(true, p95)
		} else {
			cells["p95"] = Cell{Text: "—"}
		}
		if attempts := r.values["attempts"]; attempts > 0 {
			// Same denominator as the dashboard: an increase below one attempt
			// is divided by one, so a single failed attempt cannot dominate.
			den := attempts
			if den < 1 {
				den = 1
			}
			success := 100 * (1 - r.values["failures"]/den)
			if success < 0 {
				success = 0
			}
			if success > 100 {
				success = 100
			}
			cells["success"] = Cell{
				Value: ptr(success),
				Tone:  toneAt(success, threshold{0, "danger"}, threshold{95, "warning"}, threshold{99, "success"}),
			}
		} else {
			cells["success"] = Cell{Text: "—"}
		}
		p.Rows = append(p.Rows, Row{Key: r.key, Cells: cells})
	}
	return p
}

// ── Series assembly ──────────────────────────────────────────────────────────

// rawSeries is one query result before a colour is assigned.
type rawSeries struct {
	name     string
	colorKey string
	values   []*float64
}

// rangeSeries runs a range query and returns its series sorted by name, so that
// palette colours and legend order stay stable between refreshes.
func (b *builder) rangeSeries(expr string, name, colorKey func(map[string]string) string) ([]rawSeries, error) {
	ms, err := b.client.queryRange(b.ctx, expr, b.start, b.end, b.step)
	if err != nil {
		return nil, err
	}
	out := make([]rawSeries, 0, len(ms))
	for _, m := range ms {
		out = append(out, rawSeries{name: name(m.labels), colorKey: colorKey(m.labels), values: b.align(m.points)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].name < out[j].name })
	return out, nil
}

// align places Prometheus samples on the snapshot grid. A sample that falls
// outside the grid or is duplicated is dropped; a missing step stays nil.
func (b *builder) align(points []point) []*float64 {
	out := make([]*float64, b.n)
	for _, pt := range points {
		idx := int(math.Round(float64(pt.at-b.startMS) / float64(b.stepMS)))
		if idx < 0 || idx >= b.n {
			continue
		}
		v := pt.value
		out[idx] = &v
	}
	return out
}

// withSeries assigns colours and attaches the series to p.
func withSeries(p Panel, raw []rawSeries) Panel {
	p.Kind = "series"
	// Palette colours a fixed series already uses in this panel are skipped,
	// so two series never share a colour unless both are overflow.
	used := map[string]bool{}
	for _, r := range raw {
		if c := fixedColors[r.colorKey]; c != "" {
			used[c] = true
		}
	}
	free := make([]string, 0, len(palette))
	for _, c := range palette {
		if !used[c] {
			free = append(free, c)
		}
	}
	next := 0
	for _, r := range raw {
		color := fixedColors[r.colorKey]
		if color == "" {
			color = overflowColor
			if next < len(free) {
				color = free[next]
			}
			next++
		}
		p.Series = append(p.Series, Series{Name: r.name, Color: color, Values: r.values})
	}
	for i := range p.Series {
		if p.Series[i].Name == "总计" || p.Series[i].Name == "总体" {
			p.Series[i].Line = true
		}
	}
	return p
}

// ── Helpers ──────────────────────────────────────────────────────────────────

// scalar runs an instant query whose result is a single series and returns its
// value. ok is false when Prometheus has no series for it (nothing scraped yet
// or all samples NaN).
func (b *builder) scalar(expr string) (float64, bool, error) {
	vs, err := b.client.query(b.ctx, expr, b.end)
	if err != nil {
		return 0, false, err
	}
	if len(vs) == 0 {
		return 0, false, nil
	}
	return vs[0].value, true, nil
}

// panel builds the common part of a panel.
func panel(id, section, title, description, unit string) Panel {
	return Panel{ID: id, Section: section, Kind: "stat", Title: title, Description: description, Unit: unit}
}

// tablePanel marks a panel as a table.
func tablePanel(p Panel) Panel { p.Kind = "table"; return p }

// cellValue returns a numeric cell, or a dash when the query had no sample.
func cellValue(ok bool, v float64) Cell {
	if !ok {
		return Cell{Text: "—"}
	}
	return Cell{Value: ptr(v)}
}

func ptr(v float64) *float64 { return &v }

// joinNonEmpty joins the non-empty parts with sep.
func joinNonEmpty(sep string, parts ...string) string {
	out := parts[:0]
	for _, p := range parts {
		if p != "" {
			out = append(out, p)
		}
	}
	return strings.Join(out, sep)
}

// threshold maps a value to a tone: the last entry with at <= v wins.
type threshold struct {
	at   float64
	tone string
}

func toneAt(v float64, steps ...threshold) string {
	tone := ""
	for _, s := range steps {
		if v >= s.at {
			tone = s.tone
		}
	}
	return tone
}

// formatStat renders a stat value for display.
func formatStat(v float64, unit string) string {
	switch unit {
	case "count":
		return groupInt(int64(math.Round(v)))
	case "reqps":
		return fixed(v, 3)
	case "percent":
		return fixed(v, 2)
	case "ms":
		if math.Abs(v) < 10 {
			return fixed(v, 2)
		}
		return fixed(v, 1)
	case "duration":
		return humanDuration(v)
	default:
		return fixed(v, 3)
	}
}

// fixed formats v with exactly decimals places.
func fixed(v float64, decimals int) string {
	return strconv.FormatFloat(v, 'f', decimals, 64)
}

// groupInt writes an integer with thousands separators.
func groupInt(n int64) string {
	neg := n < 0
	if neg {
		n = -n
	}
	s := strconv.FormatInt(n, 10)
	var b strings.Builder
	if neg {
		b.WriteByte('-')
	}
	for i, c := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(c)
	}
	return b.String()
}

// humanDuration renders a number of seconds as the console's durations do.
func humanDuration(seconds float64) string {
	s := int64(math.Round(seconds))
	if s < 0 {
		s = 0
	}
	switch {
	case s < 60:
		return fmt.Sprintf("%d 秒", s)
	case s < 3600:
		return fmt.Sprintf("%d 分钟", s/60)
	case s < 48*3600:
		return fmt.Sprintf("%d 小时 %d 分", s/3600, (s%3600)/60)
	default:
		return fmt.Sprintf("%d 天 %d 小时", s/86400, (s%86400)/3600)
	}
}

// fixedColors keeps the colours of the dashboard's best-known series stable.
var fixedColors = map[string]string{
	"local-zone": "sky", "cn": "green", "default": "amber", "总计": "violet", "总体": "violet",
	"NOERROR": "green", "NXDOMAIN": "sky", "REFUSED": "amber", "SERVFAIL": "rose", "OTHER": "muted",
	"aliyun": "sky", "cloudflare": "amber",
	"timeout": "amber", "network": "rose", "mismatch": "violet", "refused": "sky", "canceled": "muted",
	"重传": "teal", "丢弃": "muted",
	"过载丢弃": "rose", "应答写失败": "amber", "畸形查询": "amber", "热加载失败": "violet",
	"处理中查询": "sky", "客户端 TCP 连接": "green", "panic": "rose",
}

// palette is the colour order for series without a fixed colour. Rose and
// green are left out: they read as danger and success, and the status colours
// are not reused for arbitrary series.
var palette = []string{"teal", "sky", "violet", "amber", "orchid"}

// overflowColor is shared by every series past the palette, instead of cycling
// back to a colour that already means another series. The legend, tooltip and
// data table still name each one.
const overflowColor = "muted"
