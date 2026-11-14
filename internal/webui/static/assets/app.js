// Control console for splitter. It communicates only with the same-origin
// /api/v1 API (see internal/app/api.go).
//
// Domain names displayed on the page originate from queries issued by devices
// on the network and are therefore untrusted input: all content is assembled
// from textContent and DOM nodes, and this file contains no innerHTML. The
// page CSP also forbids inline scripts and styles.
"use strict";

const STATE_INTERVAL = 5000;
const RECENT_INTERVAL = 3000;
const OBS_INTERVAL = 30000; // matches the Grafana dashboard's refresh=30s
const OBS_DEFAULT_RANGE = "6h";
const RECENT_LIMIT = 50;
const PREVIEW_LIMIT = 8;
const RATE_WINDOW = 60; // seconds over which the query rate and average latency are computed
const LIST_STALE_DAYS = 14; // lists update weekly; 14 days without an update raises a warning
const RULE_TONES = ["teal", "sky", "violet", "amber", "orchid"];
const KIND_LABEL = { full: "完整匹配", domain: "域名及子域", keyword: "关键词", regexp: "正则" };
const ERROR_LABEL = { timeout: "超时", network: "网络错误", mismatch: "应答不匹配", refused: "拒绝（REFUSED）", canceled: "已取消" };
const ACTION_LABEL = { forward: "转发", answer: "本地应答", reject: "拒绝" };
const RCODE_TONE = { NOERROR: "", NXDOMAIN: "amber", REFUSED: "amber", SERVFAIL: "danger" };
const VIEWS = {
  overview: { title: "概况", desc: "查询量、应答构成与上游健康。" },
  rules: { title: "分流规则", desc: "自上而下，第一条命中的生效。" },
  upstreams: { title: "上游", desc: "一组内按顺序尝试，传输失败才换下一台。" },
  sets: { title: "域名集合", desc: "列表文件由周更任务维护。" },
  diagnose: { title: "路由诊断", desc: "查看路由不发送查询。实际解析会向上游发一条真实查询，并计入统计。" },
  queries: { title: "最近查询", desc: "分流器应答过的每一条查询，最新的在前。前置解析器缓存命中的不会到这里。" },
  observability: { title: "可观测性", desc: "与 Grafana 同口径的分流器指标：抓取与配置健康、查询速率、延迟、上游与运行保护。" },
};
const SVG_NS = "http://www.w3.org/2000/svg";

const $ = (id) => document.getElementById(id);

// h creates an element. Keys in attrs other than class are written as
// attributes; children may be nodes or strings.
function h(tag, attrs, ...children) {
  const el = document.createElement(tag);
  for (const [key, value] of Object.entries(attrs || {})) {
    if (value === undefined || value === null || value === false) continue;
    if (key === "class") el.className = value;
    else el.setAttribute(key, value === true ? "" : String(value));
  }
  for (const child of children.flat()) {
    if (child === undefined || child === null || child === false) continue;
    el.append(child instanceof Node ? child : String(child));
  }
  return el;
}

function icon(name, size = 16) {
  const svg = document.createElementNS(SVG_NS, "svg");
  svg.setAttribute("class", "icon");
  svg.setAttribute("width", size);
  svg.setAttribute("height", size);
  svg.setAttribute("aria-hidden", "true");
  const use = document.createElementNS(SVG_NS, "use");
  use.setAttribute("href", "#i-" + name);
  svg.append(use);
  return svg;
}

// The face of a customizable select. The chevron is a real element because the
// page CSP drops data-URI images, which is what made the previous arrow vanish.
function selectFace() {
  const mark = icon("chevron");
  mark.classList.add("select-chevron");
  return h("button", { type: "button" }, h("selectedcontent"), mark);
}

const badge = (text, tone, extra = "") => h("span", { class: `badge sm ${tone || ""} ${extra}`.trim() }, text);

// ── Formatting ───────────────────────────────────────────────────────────────
const intFormat = new Intl.NumberFormat("zh-CN");
const oneDecimal = new Intl.NumberFormat("zh-CN", { maximumFractionDigits: 1 });
const twoDecimals = new Intl.NumberFormat("zh-CN", { maximumFractionDigits: 2 });
const percentFormat = new Intl.NumberFormat("zh-CN", { style: "percent", maximumFractionDigits: 1 });
const clockFormat = new Intl.DateTimeFormat("zh-CN", { hour: "2-digit", minute: "2-digit", second: "2-digit", hour12: false });
const dateTimeFormat = new Intl.DateTimeFormat("zh-CN", { month: "2-digit", day: "2-digit", hour: "2-digit", minute: "2-digit", hour12: false });
const fullFormat = new Intl.DateTimeFormat("zh-CN", { dateStyle: "medium", timeStyle: "medium" });
const relativeFormat = new Intl.RelativeTimeFormat("zh-CN", { numeric: "auto" });

const fmt = {
  int: (n) => intFormat.format(n),
  bytes(n) {
    if (n < 1024 * 1024) return `${oneDecimal.format(n / 1024)} KB`;
    if (n < 1024 * 1024 * 1024) return `${oneDecimal.format(n / 1024 / 1024)} MB`;
    return `${twoDecimals.format(n / 1024 / 1024 / 1024)} GB`;
  },
  ms: (milliseconds) => `${(milliseconds < 10 ? twoDecimals : oneDecimal).format(milliseconds)} ms`,
  percent: (ratio) => percentFormat.format(ratio),
  clock: (date) => clockFormat.format(date),
  dateTime: (date) => dateTimeFormat.format(date),
  full: (date) => fullFormat.format(date),
  duration(seconds) {
    const s = Math.max(0, Math.floor(seconds));
    if (s < 60) return `${s} 秒`;
    if (s < 3600) return `${Math.floor(s / 60)} 分钟`;
    if (s < 48 * 3600) return `${Math.floor(s / 3600)} 小时 ${Math.floor((s % 3600) / 60)} 分`;
    return `${Math.floor(s / 86400)} 天 ${Math.floor((s % 86400) / 3600)} 小时`;
  },
  relative(date, now) {
    const seconds = (date - now) / 1000;
    const abs = Math.abs(seconds);
    if (abs < 60) return relativeFormat.format(Math.round(seconds), "second");
    if (abs < 3600) return relativeFormat.format(Math.round(seconds / 60), "minute");
    if (abs < 86400) return relativeFormat.format(Math.round(seconds / 3600), "hour");
    return relativeFormat.format(Math.round(seconds / 86400), "day");
  },
};

const compactFormat = new Intl.NumberFormat("zh-CN", { notation: "compact", maximumFractionDigits: 1 });
const axisClockFormat = new Intl.DateTimeFormat("zh-CN", { hour: "2-digit", minute: "2-digit", hour12: false });
const axisDayFormat = new Intl.DateTimeFormat("zh-CN", { month: "2-digit", day: "2-digit", hour: "2-digit", minute: "2-digit", hour12: false });

// ── API ──────────────────────────────────────────────────────────────────────
async function api(path, options) {
  let response;
  try {
    response = await fetch(path, { cache: "no-store", ...options });
  } catch (cause) {
    // Abort and timeout errors are control flow, not a dead admin endpoint.
    if (cause && (cause.name === "AbortError" || cause.name === "TimeoutError")) throw cause;
    const error = new Error("连不上分流器的管理端口");
    error.network = true;
    throw error;
  }
  let body = null;
  try { body = await response.json(); } catch (e) { /* a non-JSON body is handled by status code */ }
  if (!response.ok) {
    const error = new Error((body && body.error) || `HTTP ${response.status}`);
    error.status = response.status;
    throw error;
  }
  return body;
}

const postJSON = (path, payload) =>
  api(path, { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify(payload) });

// ── Page state ───────────────────────────────────────────────────────────────
const view = {
  data: null,          // the most recent successful /api/v1/state response
  samples: [],         // samples used for rate calculation, covering RATE_WINDOW seconds
  tones: new Map(),    // rule name to colour; colours follow the rule, not its rank
  connectionError: "", // non-empty when the most recent refresh failed
  actionError: "",     // the reload action failed or did not reach the server
  alertKey: "",
  paused: false,
  ruleOptions: "",
};

let currentView = "overview";
let trendData = [];
let pendingFocus = null;
let closeDrawer = () => {};

function knownView(name) {
  return Object.hasOwn(VIEWS, name) ? name : "";
}

function viewFromLocation() {
  const search = new URLSearchParams(location.search);
  const named = knownView(search.get("view"));
  if (named) return named;
  if (search.get("name")) return "diagnose";
  return "overview";
}

function viewHref(name) {
  const next = new URLSearchParams(location.search);
  next.delete("view");
  // A link to any view opens the newest page of the queries list.
  next.delete("page");
  next.delete("cursor");
  if (name !== "overview") next.set("view", name);
  const query = next.toString();
  return query ? `?${query}` : location.pathname;
}

function updateNavLinks() {
  for (const link of document.querySelectorAll("a[data-view]")) {
    link.href = viewHref(link.dataset.view);
  }
}

function applyRuleParam(rule) {
  const select = $("f-rule");
  const names = [...select.options].map((option) => option.value);
  if (!rule || names.includes(rule)) {
    select.value = rule || "";
    delete select.dataset.pending;
    return;
  }
  select.dataset.pending = rule;
}

// History navigation restores the form from the URL. The inputs are the source
// syncURL writes back, so a restored address is lost if the fields stay stale.
function restoreFromLocation() {
  const search = new URLSearchParams(location.search);
  $("f-q").value = search.get("q") || "";
  applyRuleParam(search.get("rule") || "");
  $("d-name").value = search.get("name") || "";
  const type = (search.get("type") || "A").toUpperCase();
  const typeSelect = $("d-type");
  if ([...typeSelect.options].some((option) => option.value === type)) typeSelect.value = type;
  restorePager(search);
  const range = search.get("range") || "";
  obs.range = OBS_RANGE_RE.test(range) ? range : OBS_DEFAULT_RANGE;
  updateObsControls();
  showDiagnoseError("");
}

function syncURL() {
  const next = new URLSearchParams();
  const name = $("d-name").value.trim();
  if (name) {
    next.set("name", name);
    if ($("d-type").value !== "A") next.set("type", $("d-type").value);
  }
  if ($("f-q").value.trim()) next.set("q", $("f-q").value.trim());
  if ($("f-rule").value) next.set("rule", $("f-rule").value);
  if (obs.range !== OBS_DEFAULT_RANGE || new URLSearchParams(location.search).has("range")) next.set("range", obs.range);
  if (currentView !== "overview") next.set("view", currentView);
  if (currentView === "queries" && pager.page > 1) {
    next.set("page", String(pager.page));
    next.set("cursor", pageCursor(pager.page));
  }
  const query = next.toString();
  history.replaceState(pagerState(), "", query ? `?${query}` : location.pathname);
  updateNavLinks();
}

function applyView(name) {
  const next = knownView(name) || "overview";
  currentView = next;
  document.documentElement.dataset.view = next;
  const meta = VIEWS[next];
  $("page-title").textContent = meta.title;
  $("page-desc").textContent = meta.desc;
  document.title = `${meta.title} · DNS 分流器`;
  for (const link of document.querySelectorAll(".side-nav a[data-view]")) {
    if (link.dataset.view === next) link.setAttribute("aria-current", "page");
    else link.removeAttribute("aria-current");
  }
}

function resetViewScroll() {
  $("main").scrollTop = 0;
  if (document.documentElement.dataset.layout === "narrow") window.scrollTo({ top: 0 });
}

function viewTransitionAllowed() {
  return typeof document.startViewTransition === "function" && !matchMedia("(prefers-reduced-motion: reduce)").matches;
}

function navigate(name, options) {
  const next = knownView(name);
  if (!next) return;
  const changed = next !== currentView;
  const drawerOpen = $("drawer").hasAttribute("aria-modal");
  // A mouse click on the always-visible sidebar leaves focus where it is.
  // Keyboard activation, and any click that closes the drawer, moves focus
  // to the page title: the activating link is either about to be hidden or
  // the view change would otherwise stay silent.
  const moveFocus = !options || options.focus !== false;

  if (changed) {
    const url = new URL(location.href);
    url.searchParams.delete("page");
    url.searchParams.delete("cursor");
    resetPager();
    if (next === "overview") url.searchParams.delete("view");
    else url.searchParams.set("view", next);
    const apply = () => {
      history.pushState({ view: next }, "", url);
      updateNavLinks();
      applyView(next);
      render();
      resetViewScroll();
    };
    const refresh = () => {
      if (next === "overview" || next === "queries") refreshRecent();
      if (next === "observability") enterObservability();
    };
    // Route focus after the new state is in the DOM; with a view transition,
    // wait for the finished promise so the focus ring is not snapshotted.
    const focus = () => {
      if (drawerOpen) {
        pendingFocus = $("page-title");
        closeDrawer({ restoreFocus: false });
      } else if (moveFocus) {
        $("page-title").focus({ preventScroll: true });
      }
    };
    if (!drawerOpen && viewTransitionAllowed()) {
      let transition = null;
      try {
        transition = document.startViewTransition(() => {
          apply();
          refresh();
        });
      } catch (e) {
        // A transition already in flight: fall back to an instant switch.
        apply();
        refresh();
        focus();
        return;
      }
      transition.finished.then(focus, focus);
    } else {
      apply();
      refresh();
      focus();
    }
    return;
  }

  // Same view: unchanged behavior.
  if (drawerOpen) {
    pendingFocus = $("page-title");
    closeDrawer({ restoreFocus: false });
  }
}

// Queries refused before routing (wrong class, bad EDNS version, other
// opcodes, FORMERR) are recorded under this pseudo-rule.
const INVALID_RULE = "_invalid";
const ruleTone = (name) => view.tones.get(name) || "";
const ruleBadge = (name) => name === INVALID_RULE ? badge("无效查询", "", "outline") : badge(name, ruleTone(name));

function rcodeBadge(rcode, count) {
  const tone = rcode in RCODE_TONE ? RCODE_TONE[rcode] : "amber";
  const text = count === undefined ? rcode : `${rcode} ${fmt.int(count)}`;
  return badge(text, tone, tone ? "" : "outline");
}

// ── Status and overview ──────────────────────────────────────────────────────
function renderStatus(status) {
  const el = $("status");
  let tone = "success", text = "运行中";
  if (view.connectionError) { tone = "danger"; text = "连接中断"; }
  else if (!status.ready) { tone = "warning"; text = "重载被拒，仍在用旧配置"; }
  el.className = `badge ${tone}`;
  $("status-text").textContent = text;
}

function versionText(status) {
  return status.commit && status.commit !== "none" ? `${status.version} (${status.commit})` : status.version;
}

function prop(term, value) {
  return h("div", null, h("dt", null, term), h("dd", null, value));
}

// The note is visible text, not a title: hover-only text never reaches touch
// or keyboard users.
function propWithNote(term, value, note) {
  return h("div", null, h("dt", null, term), h("dd", null, value, note ? h("span", { class: "sub" }, note) : null));
}

function renderRuntime(status) {
  const now = new Date(status.now);
  const loaded = new Date(status.loaded_at);
  $("runtime").replaceChildren(
    prop("版本", h("code", null, versionText(status))),
    prop("监听", h("code", null, (status.listen || []).join("、"))),
    prop("管理端口", h("code", null, status.admin)),
    prop("已运行", fmt.duration((now - new Date(status.started_at)) / 1000)),
    prop("配置加载于", h("span", { title: fmt.full(loaded) }, `${fmt.dateTime(loaded)}（${fmt.relative(loaded, now)}）`)),
    propWithNote("日志级别", status.log_level, status.log_queries ? "标准错误输出逐条记录查询" : ""),
    prop("成功重载", fmt.int(status.reloads_ok)),
    propWithNote("被拒重载", fmt.int(status.reloads_failed), status.reloads_failed > 0 ? `自启动以来有 ${fmt.int(status.reloads_failed)} 次重载被拒；详情见进程日志。` : ""),
  );
}

// renderQueryLog shows the on-disk query log as a capacity/health panel. The
// native progress element carries the value to assistive technology; the text
// beside it carries the exact bytes.
function renderQueryLog(log) {
  const host = $("querylog");
  if (!log) {
    host.replaceChildren(h("p", { class: "empty-note" }, "状态里没有查询日志信息。"));
    return;
  }
  if (!log.active) {
    host.replaceChildren(
      h("div", { class: "chips" }, badge("仅内存", "", "outline")),
      h("p", { class: "muted" }, "查询日志已关闭，只在内存里保留最近 512 条。"),
      h("dl", { class: "props" }, prop("目录", h("code", null, log.dir))),
    );
    return;
  }
  const ratio = log.max_size_bytes > 0 ? log.size_bytes / log.max_size_bytes : 0;
  const tone = ratio >= 0.95 ? "danger" : ratio >= 0.8 ? "warning" : "";
  const used = `${fmt.bytes(log.size_bytes)} / ${fmt.bytes(log.max_size_bytes)}`;
  const progress = h("progress", { class: `meter ${tone}`.trim(), max: "100", value: (ratio * 100).toFixed(2), "aria-label": "查询日志磁盘占用" });
  progress.setAttribute("aria-valuetext", `已用 ${used}`);
  const props = [
    prop("目录", h("code", null, log.dir)),
    prop("文件", fmt.int(log.files)),
    prop("保留", retentionAge(log)),
    propWithNote("丢弃", fmt.int(log.dropped), log.dropped > 0 ? "磁盘写入跟不上查询速度，部分查询没有落盘。" : ""),
    propWithNote("写入失败", fmt.int(log.write_errors), log.write_errors > 0 ? `写入查询日志失败${log.last_error ? `：${log.last_error}` : ""}；详情见进程日志。` : ""),
  ];
  if (log.oldest) {
    const oldest = new Date(log.oldest);
    props.splice(3, 0, prop("最早记录", h("span", { title: fmt.full(oldest) }, fmt.dateTime(oldest))));
  }
  host.replaceChildren(
    h("div", { class: "log-usage" },
      h("div", { class: "log-usage-head" }, h("span", null, "磁盘占用"), h("span", { class: "mono" }, used)),
      progress),
    h("dl", { class: "props" }, ...props),
  );
}

function renderMeta(status) {
  $("meta-version").textContent = versionText(status);
  $("meta-listen").textContent = (status.listen || []).join("、");
}

function retentionAge(log) {
  const days = log.max_age_seconds / 86400;
  return days >= 1 && Number.isInteger(days) ? `${days} 天` : `${oneDecimal.format(log.max_age_seconds / 3600)} 小时`;
}

function retentionText(log) {
  return `保留 ${retentionAge(log)}、最多 ${fmt.bytes(log.max_size_bytes)}`;
}

const MIX_TONE = { NOERROR: "success", NXDOMAIN: "sky", REFUSED: "amber", SERVFAIL: "rose", OTHER: "muted" };

// renderRcodes replaces the old chip row with a composition bar plus a readable
// legend. The bar is a single role=img summary; the legend carries every number.
function renderRcodes(rcodes) {
  const entries = Object.entries(rcodes || {}).filter(([, n]) => n > 0).sort((a, b) => b[1] - a[1]);
  const total = entries.reduce((sum, [, n]) => sum + n, 0);
  const totalEl = $("rcode-total");
  const el = $("rcodes");
  if (!total) {
    totalEl.textContent = "—";
    el.replaceChildren(h("p", { class: "empty-note" }, "还没有应答。"));
    return;
  }
  totalEl.textContent = `${fmt.int(total)} 条`;
  const bar = h("div", {
    class: "mix-bar",
    role: "img",
    "aria-label": entries.map(([code, n]) => `${code} ${fmt.percent(n / total)}`).join("，"),
  });
  const rows = [];
  for (const [code, n] of entries) {
    const tone = MIX_TONE[code] || "muted";
    const share = n / total;
    const seg = h("span", { class: `mix-seg mix-tone-${tone}`, title: `${code} ${fmt.int(n)} · ${fmt.percent(share)}` });
    seg.style.width = `${(share * 100).toFixed(2)}%`;
    bar.append(seg);
    rows.push(h("div", null,
      h("dt", null, h("span", { class: `mix-swatch mix-tone-${tone}` }), code),
      h("dd", null, `${fmt.int(n)} · ${fmt.percent(share)}`)));
  }
  el.replaceChildren(bar, h("dl", { class: "mix-legend" }, ...rows));
}

function renderAlerts() {
  const status = view.data && view.data.status;
  const items = [];
  if (view.connectionError) {
    items.push({ tone: "danger", title: "读不到分流器的状态", detail: `${view.connectionError}。页面保留上一次的数据，每 ${STATE_INTERVAL / 1000} 秒自动重试。` });
  }
  if (view.actionError) {
    items.push({ tone: "danger", title: "重新加载没有执行", detail: view.actionError });
  }
  if (status && !status.ready && status.last_reload) {
    items.push({
      tone: "warning",
      title: `${fmt.dateTime(new Date(status.last_reload.at))} 的重新加载被拒绝，分流器仍在用上一份配置`,
      detail: `${status.last_reload.error}\n\n修正仓库里的配置或列表并重新部署后，再点「重新加载配置」。`,
    });
  }
  if (status && status.panics > 0) {
    items.push({ tone: "danger", title: `处理查询时出现过 ${fmt.int(status.panics)} 次 panic`, detail: "对应的查询被丢弃，进程继续服务。这是代码缺陷，日志里有堆栈。" });
  }
  // Rebuild only when the content changes: this region is aria-live, so every
  // rebuild is announced by screen readers.
  const key = JSON.stringify(items);
  if (key === view.alertKey) return;
  view.alertKey = key;
  $("alerts").replaceChildren(...items.map((item) =>
    h("div", { class: `alert ${item.tone}` },
      icon("alert", 18),
      h("div", { class: "alert-copy" },
        h("p", { class: "alert-title" }, item.title),
        h("p", { class: "alert-detail" }, item.detail)))));
}

function upstreamTotals(data) {
  const total = { requests: 0, errors: 0, succeeded: 0, seconds: 0 };
  for (const group of data.upstreams) {
    for (const server of group.servers) {
      total.requests += server.requests;
      total.succeeded += server.succeeded;
      total.seconds += server.duration_seconds;
      for (const n of Object.values(server.errors)) total.errors += n;
    }
  }
  return total;
}

function pushSample(data) {
  const up = upstreamTotals(data);
  const sample = {
    at: Date.parse(data.status.now) / 1000,
    started: data.status.started_at,
    queries: data.status.queries,
    succeeded: up.succeeded,
    seconds: up.seconds,
  };
  // Counters restart at zero when the process restarts, so older samples are
  // discarded.
  if (view.samples.length && view.samples[0].started !== sample.started) view.samples = [];
  view.samples.push(sample);
  while (view.samples.length > 2 && sample.at - view.samples[1].at >= RATE_WINDOW) view.samples.shift();
}

function setValue(id, text, unit) {
  $(id).replaceChildren(text, unit ? h("small", null, unit) : "");
}

function setProgress(id, value, max, text, ratio) {
  const el = $(id);
  if (!el) return;
  const limit = max > 0 ? max : 1;
  el.max = limit;
  el.value = Math.max(0, Math.min(value, limit));
  el.setAttribute("aria-valuetext", text);
  el.className = `meter${ratio >= 0.95 ? " danger" : ratio >= 0.8 ? " warning" : ""}`;
}

// setCardNote writes the KPI's context as visible text under the value.
function setCardNote(id, text) {
  const value = $(id);
  const card = value && value.closest(".card");
  if (!card) return;
  let note = card.querySelector(".card-context");
  if (!note) {
    note = h("p", { class: "card-context" });
    card.append(note);
  }
  note.textContent = text || "";
}

function renderKPIs(data) {
  const status = data.status;
  const up = upstreamTotals(data);
  const samples = view.samples;
  const first = samples[0];
  const last = samples[samples.length - 1];
  const span = first && last ? last.at - first.at : 0;

  setValue("kpi-queries", fmt.int(status.queries));
  const invalid = status.refused_locally + status.malformed;
  const writeErrors = status.write_errors || 0;
  setCardNote("kpi-queries", `自 ${fmt.dateTime(new Date(status.started_at))} 启动以来` +
    (invalid > 0 ? `；另有 ${fmt.int(invalid)} 条无效查询在本地拒绝` : "") +
    (writeErrors > 0 ? `；${fmt.int(writeErrors)} 次应答未能写回客户端` : ""));

  if (first && span >= 1) {
    setValue("kpi-qps", oneDecimal.format((last.queries - first.queries) / span), "qps");
    setCardNote("kpi-qps", `近 ${Math.round(span)} 秒平均，正在处理 ${fmt.int(status.inflight)} 条`);
  } else {
    setCardNote("kpi-qps", "需要至少两次状态采样");
  }

  const msText = (ms) => (ms < 10 ? twoDecimals : oneDecimal).format(ms);
  const recent = first && last ? last.succeeded - first.succeeded : 0;
  if (span >= 1 && recent > 0) {
    setValue("kpi-latency", msText((last.seconds - first.seconds) / recent * 1000), "ms");
    setCardNote("kpi-latency", `近 ${Math.round(span)} 秒内 ${fmt.int(recent)} 次成功的上游交换`);
  } else if (up.succeeded > 0) {
    setValue("kpi-latency", msText(up.seconds / up.succeeded * 1000), "ms");
    setCardNote("kpi-latency", "启动以来的平均值，最近没有新的上游交换");
  } else {
    setValue("kpi-latency", "—");
    setCardNote("kpi-latency", "还没有转发过查询");
  }

  setValue("kpi-errors", fmt.int(up.errors), "次");
  setCardNote("kpi-errors", up.requests > 0
    ? `启动以来 ${fmt.int(up.requests)} 次上游请求，失败率 ${fmt.percent(up.errors / up.requests)}`
    : "还没有上游请求");

  const servfail = (status.rcodes && status.rcodes.SERVFAIL) || 0;
  const success = status.queries > 0 ? 1 - servfail / status.queries : null;
  const successValue = $("kpi-success");
  if (success === null) {
    setValue("kpi-success", "—");
    successValue.className = "card-value";
    setCardNote("kpi-success", "还没有业务查询");
  } else {
    const pct = success * 100;
    setValue("kpi-success", twoDecimals.format(pct), "%");
    // The verdict is spelled out, not carried by the colour alone.
    const verdict = pct < 95 ? "过低（低于 95%）；" : pct < 99 ? "偏低（低于 99%）；" : "";
    setCardNote("kpi-success", `${verdict}SERVFAIL ${fmt.int(servfail)} 条，共 ${fmt.int(status.queries)} 条业务查询`);
    successValue.className = `card-value tone-${pct < 95 ? "danger" : pct < 99 ? "warning" : "success"}`;
  }

  const overload = status.overload_dropped || 0;
  const overloadNote = overload > 0 ? `；已因并发或连接上限丢弃 ${fmt.int(overload)} 条` : "";

  const inMax = status.max_inflight || 0;
  const inRatio = inMax > 0 ? status.inflight / inMax : 0;
  setValue("kpi-inflight", fmt.int(status.inflight));
  setCardNote("kpi-inflight", `上限 ${fmt.int(inMax)} · 当前占用 ${fmt.percent(inRatio)}${overloadNote}`);
  setProgress("kpi-inflight-meter", status.inflight, inMax, `${fmt.int(status.inflight)} / ${fmt.int(inMax)}`, inRatio);

  const tcpMax = status.max_tcp_conns || 0;
  const tcpRatio = tcpMax > 0 ? status.tcp_connections / tcpMax : 0;
  setValue("kpi-tcp", fmt.int(status.tcp_connections));
  setCardNote("kpi-tcp", `上限 ${fmt.int(tcpMax)} · 当前占用 ${fmt.percent(tcpRatio)}${overloadNote}`);
  setProgress("kpi-tcp-meter", status.tcp_connections, tcpMax, `${fmt.int(status.tcp_connections)} / ${fmt.int(tcpMax)}`, tcpRatio);
}

// renderTrend draws the in-memory query-rate samples as a compact sparkline.
// The raw points are secondary content and live in a details disclosure.
function renderTrend() {
  const points = [];
  const samples = view.samples;
  for (let i = 1; i < samples.length; i += 1) {
    const dt = samples[i].at - samples[i - 1].at;
    if (dt <= 0) continue;
    points.push({ at: samples[i].at, v: Math.max(0, (samples[i].queries - samples[i - 1].queries) / dt) });
  }
  trendData = points;
  const host = $("spark");
  if (points.length < 2) {
    host.replaceChildren(h("p", { class: "empty-note" }, "正在积累采样…"));
    host.title = "正在积累采样…";
    renderTrendTable();
    return;
  }
  const width = 600;
  const height = 132;
  const pad = 6;
  const span = points.at(-1).at - points[0].at;
  const current = points.at(-1).v;
  const peak = Math.max(...points.map((point) => point.v));
  const scale = Math.max(peak, 0.001);
  const t0 = points[0].at;
  const t1 = points.at(-1).at;
  const x = (t) => pad + (t1 === t0 ? 0 : ((t - t0) / (t1 - t0)) * (width - 2 * pad));
  const y = (v) => height - pad - (v / scale) * (height - 2 * pad);
  let line = "";
  points.forEach((point, index) => {
    line += `${index ? "L" : "M"}${x(point.at).toFixed(1)} ${y(point.v).toFixed(1)}`;
  });
  const firstX = x(points[0].at).toFixed(1);
  const area = `M${firstX} ${height - pad} L${firstX} ${y(points[0].v).toFixed(1)} ` +
    points.slice(1).map((point) => `L${x(point.at).toFixed(1)} ${y(point.v).toFixed(1)}`).join(" ") +
    ` L${x(points.at(-1).at).toFixed(1)} ${height - pad} Z`;
  const svg = svgEl("svg", {
    class: "spark-svg",
    viewBox: `0 0 ${width} ${height}`,
    preserveAspectRatio: "none",
    role: "img",
    "aria-label": `近 ${Math.round(span)} 秒查询速率，当前 ${twoDecimals.format(current)} qps，峰值 ${twoDecimals.format(peak)} qps`,
  });
  svg.append(
    svgEl("line", { x1: pad, x2: width - pad, y1: height - pad, y2: height - pad, class: "spark-baseline" }),
    svgEl("path", { d: area, class: "spark-area" }),
    svgEl("path", { d: line, class: "spark-line" }),
  );
  host.replaceChildren(svg);
  host.title = `近 ${Math.round(span)} 秒 · 当前 ${twoDecimals.format(current)} qps · 峰值 ${twoDecimals.format(peak)} qps`;
  renderTrendTable();
}

function renderTrendTable() {
  const details = $("trend-details");
  if (!details || !details.open) return;
  const host = $("trend-table");
  const rows = trendData.slice(-20).reverse();
  // This runs on every state poll. Rebuilding the scrollport would reset its
  // scroll position and drop keyboard focus, so only the rows are replaced.
  let body = host.querySelector("tbody");
  if (!body) {
    body = h("tbody");
    host.replaceChildren(h("div", { class: "table-wrap", tabindex: "0", role: "region", "aria-label": "查询速率数据表" },
      h("table", { class: "table" },
        h("caption", { class: "sr-only" }, "查询速率数据表"),
        h("thead", null, h("tr", null,
          h("th", { scope: "col" }, "时间"),
          h("th", { scope: "col", class: "num" }, "查询速率"))),
        body)));
  }
  if (!rows.length) {
    body.replaceChildren(h("tr", null, h("td", { colspan: 2, class: "empty" }, "正在积累采样…")));
  } else {
    body.replaceChildren(...rows.map((point) => h("tr", null,
      h("td", { class: "time" }, fmt.clock(new Date(point.at * 1000))),
      h("td", { class: "num" }, `${twoDecimals.format(point.v)} qps`))));
  }
}

// renderUpstreamHealth summarises every server in one compact card per group.
// The native progress element carries the success ratio; the text carries the
// exact counts and latency.
function renderUpstreamHealth(data) {
  const host = $("upstream-health");
  if (!data.upstreams.length) {
    host.replaceChildren(h("p", { class: "empty-note" }, "配置里没有上游：所有规则都在本地应答或拒绝。"));
    return;
  }
  host.replaceChildren(...data.upstreams.map((group) => {
    const rows = group.servers.map((server) => {
      const failures = Object.values(server.errors).reduce((sum, n) => sum + n, 0);
      const rate = server.requests > 0 ? server.succeeded / server.requests : null;
      const average = server.succeeded > 0 ? server.duration_seconds / server.succeeded * 1000 : null;
      const tone = rate === null ? "" : rate < 0.9 ? "danger" : rate < 0.99 ? "warning" : "success";
      const progress = h("progress", {
        class: `meter ${tone}`.trim(),
        max: "100",
        value: rate === null ? "0" : (rate * 100).toFixed(1),
        "aria-label": `${server.addr} 成功率`,
      });
      progress.setAttribute("aria-valuetext", rate === null ? "还没有请求" : fmt.percent(rate));
      const meta = [];
      if (rate === null) {
        meta.push("还没有请求");
      } else {
        meta.push(`成功率 ${fmt.percent(rate)}`);
        if (average !== null) meta.push(`平均 ${fmt.ms(average)}`);
        meta.push(`${fmt.int(server.requests)} 次请求`);
        if (failures > 0) meta.push(`失败 ${fmt.int(failures)}`);
        if (server.retransmits > 0) meta.push(`重传 ${fmt.int(server.retransmits)}`);
        if (server.dropped > 0) meta.push(`丢弃 ${fmt.int(server.dropped)}`);
      }
      if (server.penalized) meta.unshift("近期连续失败，暂排在其他服务器之后");
      const penalized = server.penalized ? badge("惩罚中", "warning") : null;
      return h("li", { class: "upstream-row" },
        h("div", { class: "upstream-row-head" },
          h("code", null, server.addr),
          penalized),
        h("p", { class: "upstream-row-meta" }, meta.join(" · ")),
        progress);
    });
    return h("article", { class: "panel upstream-group" },
      h("header", { class: "upstream-group-head" },
        h("h3", null, h("code", null, group.name)),
        h("p", { class: "muted" }, `单次超时 ${oneDecimal.format(group.timeout_seconds)} 秒`)),
      h("ul", { class: "upstream-list" }, ...rows));
  }));
}

// ── Data tables ──────────────────────────────────────────────────────────────
function emptyRow(columns, text) {
  return h("tr", null, h("td", { colspan: columns, class: "empty" }, text));
}

function fillEmpty(id, text) {
  const table = $(id);
  if (!table || !table.tBodies || !table.tHead.rows.length) return;
  table.tBodies[0].replaceChildren(emptyRow(table.tHead.rows[0].cells.length, text));
}

function averageMs(seconds, count) {
  return count > 0 ? fmt.ms(seconds / count * 1000) : "—";
}

function conditionCell(rule) {
  if (!rule.sets.length && !rule.qtypes.length) return "其余全部查询";
  return h("div", { class: "chips" },
    rule.sets.map((set) => badge(`集合 ${set}`, "", "outline")),
    rule.qtypes.length ? badge(`类型 ${rule.qtypes.join(" / ")}`, "", "outline") : null);
}

function actionCell(rule) {
  const detail = rule.action === "answer"
    ? [...rule.answer.a, ...rule.answer.aaaa].join("、")
    : rule.target;
  return h("div", null,
    h("div", { class: "chips" }, badge(ACTION_LABEL[rule.action] || rule.action, "", "outline"), h("code", null, detail)),
    rule.action === "answer" ? h("span", { class: "sub" }, `TTL ${rule.answer.ttl} 秒，其余记录类型回 NODATA`) : null);
}

function shareCell(ratio) {
  const fill = h("div", { class: "share-fill" });
  fill.style.transform = `scaleX(${ratio})`;
  return h("div", { class: "share" },
    h("div", { class: "share-track" }, fill),
    h("span", { class: "share-text" }, fmt.percent(ratio)));
}

function rememberRules(data) {
  view.tones = new Map(data.rules.map((rule, index) => [rule.name, RULE_TONES[index % RULE_TONES.length]]));
  const names = data.rules.map((rule) => rule.name);
  const signature = names.join("\n");
  if (signature === view.ruleOptions) return;
  view.ruleOptions = signature;
  const select = $("f-rule");
  const wanted = select.dataset.pending || select.value;
  delete select.dataset.pending;
  let button = select.querySelector("button");
  if (!button || !button.querySelector(".select-chevron")) button = selectFace();
  select.replaceChildren(button, h("option", { value: "" }, "全部规则"), ...names.map((name) => h("option", { value: name }, name)),
    h("option", { value: INVALID_RULE }, "无效查询"));
  select.value = names.includes(wanted) || wanted === INVALID_RULE ? wanted : "";
}

function renderRules(data) {
  const total = data.status.queries;
  $("rules").tBodies[0].replaceChildren(...data.rules.map((rule, index) =>
    h("tr", null,
      h("td", { class: "num" }, index + 1),
      h("td", null, ruleBadge(rule.name)),
      h("td", null, conditionCell(rule)),
      h("td", null, actionCell(rule)),
      h("td", { class: "num" }, fmt.int(rule.queries)),
      h("td", null, shareCell(total > 0 ? rule.queries / total : 0)),
      h("td", { class: "num" }, averageMs(rule.duration_seconds, rule.queries)))));
}

function renderOverviewRules(data) {
  const total = data.status.queries;
  const rows = data.rules.map((rule, index) =>
    h("tr", null,
      h("td", { class: "num" }, index + 1),
      h("td", null, ruleBadge(rule.name)),
      h("td", { class: "num" }, fmt.int(rule.queries)),
      h("td", null, shareCell(total > 0 ? rule.queries / total : 0))));
  $("overview-rules").tBodies[0].replaceChildren(...(rows.length ? rows : [emptyRow(4, "配置里没有规则。")]));
}

function serverCells(server) {
  const failed = Object.values(server.errors).reduce((sum, n) => sum + n, 0);
  const reasons = Object.entries(server.errors).filter(([, n]) => n > 0)
    .map(([reason, n]) => `${ERROR_LABEL[reason] || reason} ${fmt.int(n)}`).join("，");
  return [
    h("td", { class: "nowrap" }, h("code", null, server.addr),
      server.penalized ? h("span", { class: "sub" }, "连续失败，暂排在其他服务器之后") : null),
    h("td", { class: "num" }, fmt.int(server.requests)),
    h("td", { class: "num" }, fmt.int(failed), reasons ? h("span", { class: "sub" }, reasons) : null),
    h("td", { class: "num" }, fmt.int(server.retransmits)),
    h("td", { class: "num" }, averageMs(server.duration_seconds, server.succeeded)),
  ];
}

function upstreamSection(group, index) {
  const titleId = `upstream-title-${index}`;
  return h("section", { class: "section", "data-upstream": group.name, "aria-labelledby": titleId },
    h("div", { class: "section-head" },
      h("h2", { id: titleId }, h("code", null, group.name)),
      h("p", { class: "muted" }, `单次超时 ${oneDecimal.format(group.timeout_seconds)} 秒`)),
    h("div", { class: "table-wrap", tabindex: "0", role: "region", "aria-labelledby": titleId },
      h("table", { class: "table" },
        h("caption", { class: "sr-only" }, `上游组 ${group.name} 的服务器`),
        h("thead", null, h("tr", null,
          h("th", { scope: "col" }, "服务器"),
          h("th", { scope: "col", class: "num" }, "请求"),
          h("th", { scope: "col", class: "num" }, "失败"),
          h("th", { scope: "col", class: "num" }, "重发"),
          h("th", { scope: "col", class: "num" }, "平均耗时"))),
        h("tbody", null, group.servers.map((server) => h("tr", null, ...serverCells(server)))))));
}

function renderUpstreams(data) {
  const host = $("upstreams");
  if (!data.upstreams.length) {
    delete host.dataset.groups;
    host.replaceChildren(h("p", { class: "empty-note" }, "配置里没有上游：所有规则都在本地应答或拒绝。"));
    return;
  }
  // Rebuilding the scrollport on every poll drops scroll position and focus.
  // Same group names keep the existing sections and only replace the rows.
  const signature = data.upstreams.map((group) => group.name).join("\n");
  if (host.dataset.groups !== signature) {
    host.dataset.groups = signature;
    host.replaceChildren(...data.upstreams.map(upstreamSection));
    return;
  }
  data.upstreams.forEach((group, index) => {
    const section = host.children[index];
    section.querySelector(".muted").textContent = `单次超时 ${oneDecimal.format(group.timeout_seconds)} 秒`;
    section.querySelector("tbody").replaceChildren(
      ...group.servers.map((server) => h("tr", null, ...serverCells(server))));
  });
}

function sourceLine(source, now) {
  const modified = new Date(source.modified);
  const known = modified.getUTCFullYear() > 1970;
  const days = known ? (now - modified) / 86400000 : 0;
  return h("div", null,
    h("code", { title: source.path }, source.file),
    source.selectors ? h("span", { class: "sub" }, `geosite：${source.selectors.join("、")}`) : null,
    h("span", { class: "sub", title: known ? fmt.full(modified) : null },
      known ? `更新于 ${fmt.relative(modified, now)}` : "更新时间未知"),
    days > LIST_STALE_DAYS ? badge(`已 ${Math.floor(days)} 天没有更新`, "warning") : null);
}

function renderSets(data) {
  const now = new Date(data.status.now);
  $("sets").tBodies[0].replaceChildren(...data.sets.map((set) =>
    h("tr", null,
      h("td", null, h("code", null, set.name)),
      h("td", { class: "num" },
        fmt.int(set.total),
        h("span", { class: "sub" }, Object.entries(set.entries).map(([kind, n]) => `${kind} ${fmt.int(n)}`).join("，"))),
      h("td", null,
        set.sources.map((source) => sourceLine(source, now)),
        set.inline_rules > 0 ? h("div", null, `配置内联 ${fmt.int(set.inline_rules)} 条`) : null),
      h("td", null, h("div", { class: "chips" }, (set.used_by || []).map(ruleBadge))))));
}

function render() {
  const data = view.data;
  $("workspace").toggleAttribute("data-stale", Boolean(view.connectionError));
  renderAlerts();
  if (!data) {
    renderStatus({ ready: true });
    if (view.connectionError) {
      const text = "暂时读不到数据。";
      for (const id of ["rules", "sets", "recent", "overview-rules", "overview-recent"]) fillEmpty(id, text);
      $("upstreams").replaceChildren(h("p", { class: "empty-note" }, text));
      $("runtime").replaceChildren(prop("状态", text));
      for (const id of ["spark", "rcodes", "upstream-health", "querylog", "trend-table"]) {
        $(id).replaceChildren(h("p", { class: "empty-note" }, text));
      }
      $("spark").title = text;
      $("rcode-total").textContent = "—";
    }
    return;
  }
  renderStatus(data.status);
  renderMeta(data.status);
  // The rule palette and the filter options are global chrome.
  rememberRules(data);
  // Only the visible view pays for rebuilding its panels. A poll on the
  // observability page must not rebuild every hidden table.
  switch (currentView) {
    case "overview":
      renderRuntime(data.status);
      renderQueryLog(data.status.query_log);
      renderRcodes(data.status.rcodes);
      renderKPIs(data);
      renderTrend();
      renderUpstreamHealth(data);
      renderOverviewRules(data);
      break;
    case "rules":
      renderRules(data);
      break;
    case "upstreams":
      renderUpstreams(data);
      break;
    case "sets":
      renderSets(data);
      break;
    default:
      break;
  }
}

let stateFetch = null;

function refreshState() {
  // Deduplicate overlapping polls; callers that await this get the in-flight
  // request instead of starting a second one.
  if (stateFetch) return stateFetch;
  stateFetch = (async () => {
    const signal = typeof AbortSignal.timeout === "function" ? AbortSignal.timeout(10000) : undefined;
    try {
      const data = await api("/api/v1/state", { signal, priority: "low" });
      view.data = data;
      view.connectionError = "";
      pushSample(data);
    } catch (error) {
      if (error.name === "TimeoutError") view.connectionError = "读取状态超时";
      else if (error.name !== "AbortError") view.connectionError = error.message;
    }
    render();
  })().finally(() => { stateFetch = null; });
  return stateFetch;
}

// ── Recent queries ───────────────────────────────────────────────────────────
function queryRow(record, compact) {
  const at = new Date(record.time);
  const how = record.set ? `集合 ${record.set}，${KIND_LABEL[record.match] || record.match}` : "未命中任何集合";
  if (compact) {
    return h("tr", null,
      h("td", { class: "time", title: fmt.full(at) }, fmt.clock(at)),
      h("td", { class: "name" }, record.type ? h("code", null, record.name || ".") : "没有可读的问题段"),
      h("td", null, ruleBadge(record.rule)),
      h("td", null, h("div", { class: "chips" }, rcodeBadge(record.rcode), record.truncated ? badge("已截断", "", "outline") : null)),
      h("td", { class: "num" }, fmt.ms(record.duration_ms)));
  }
  return h("tr", null,
    h("td", { class: "time", title: fmt.full(at) }, fmt.clock(at)),
    h("td", { class: "name" }, record.type ? h("code", null, record.name || ".") : "没有可读的问题段"),
    h("td", { class: "nowrap" }, record.class ? `${record.class} ${record.type}` : record.type || "—"),
    h("td", null, ruleBadge(record.rule)),
    h("td", { class: "nowrap", title: how }, h("code", null, record.target)),
    h("td", null, h("div", { class: "chips" }, rcodeBadge(record.rcode), record.truncated ? badge("已截断", "", "outline") : null)),
    h("td", { class: "num" }, fmt.ms(record.duration_ms)));
}

// The queries view is paged with cursors from /api/v1/queries: page 1 is the
// newest records and refreshes every few seconds; page N starts at the cursor
// that page N-1 returned as "next". Cursors point into records that already
// exist, so pages after the first stay put while new queries arrive and do
// not auto-refresh. The page and its cursor live in the URL (back and forward
// move between pages); the cursors of the pages before it live in
// history.state, so "previous" also works after a reload of that entry.
// generation changes whenever the page or the filters change, so a response
// requested for another page is dropped instead of painted.
const pager = { page: 1, cursors: [""], next: "", count: 0, shown: null, generation: 0 };
let recentInFlight = null; // { key, controller, generation }

function pageCursor(page) {
  return page === 1 ? "" : pager.cursors[page - 1];
}

function recentFilters(cursor) {
  const query = new URLSearchParams({ limit: RECENT_LIMIT });
  const q = $("f-q").value.trim();
  const rule = $("f-rule").value;
  if (q) query.set("q", q);
  if (rule) query.set("rule", rule);
  if (cursor) query.set("cursor", cursor);
  return { query, filtered: Boolean(q || rule) };
}

function emptyText(filtered, more) {
  if (more) return "这一段日志里没有符合筛选条件的查询。点「下一页」继续向前查找。";
  if (pager.page > 1) return "这一页没有记录。";
  return filtered
    ? "没有符合筛选条件的查询。清空筛选条件可以看到全部。"
    : "还没有查询经过分流器。在「路由诊断」里做一次实际解析，或等客户端发起查询。";
}

function renderRecentNote(data) {
  const log = view.data && view.data.status.query_log;
  if (data.source === "disk") {
    const where = log ? `查询日志写在 ${log.dir}，${retentionText(log)}。` : "查询日志已落盘。";
    $("recent-note").textContent = `${where}每页 ${RECENT_LIMIT} 条，最新的在第 1 页。AdGuard 缓存命中的查询不会到达分流器。`;
  } else {
    $("recent-note").textContent = `查询日志已关闭（log.query_log.enabled: false），只在内存里保留最近 ${fmt.int(data.capacity)} 条，每页 ${RECENT_LIMIT} 条。AdGuard 缓存命中的查询不会到达分流器。`;
  }
}

// pageHref is the address of a page of the current filters.
function pageHref(page) {
  const next = new URLSearchParams(location.search);
  next.delete("page");
  next.delete("cursor");
  if (page > 1) {
    next.set("page", String(page));
    next.set("cursor", pageCursor(page));
  }
  const query = next.toString();
  return query ? `?${query}` : location.pathname;
}

function pagerState() {
  return { view: currentView, cursors: pager.cursors.slice(0, pager.page) };
}

// setPageLink enables a pager link with its address, or disables it. A link
// that loses its target while focused hands focus to the page indicator, so
// keyboard users are not dropped to <body>.
function setPageLink(link, page) {
  const target = page >= 1 && pageCursor(page) !== undefined ? page : 0;
  if (target) {
    link.href = pageHref(target);
    link.dataset.page = String(target);
    link.removeAttribute("aria-disabled");
    return;
  }
  if (document.activeElement === link) $("p-page").focus();
  link.removeAttribute("href");
  delete link.dataset.page;
  link.setAttribute("aria-disabled", "true");
}

function renderPager(error) {
  const more = Boolean(pager.next);
  $("recent-pager").hidden = pager.page === 1 && !more;
  $("p-page").textContent = `第 ${fmt.int(pager.page)} 页`;
  $("p-first").hidden = pager.page <= 2;
  setPageLink($("p-first"), pager.page > 2 ? 1 : 0);
  setPageLink($("p-prev"), pager.page - 1);
  setPageLink($("p-next"), more ? pager.page + 1 : 0);
  let text = error || "";
  if (!text) {
    const parts = [`本页 ${fmt.int(pager.count)} 条`];
    if (pager.page > 1) parts.push("这一页不自动刷新");
    if (!more && pager.page > 1) parts.push("已经到最早的记录");
    text = parts.join(" · ");
  }
  $("recent-progress").textContent = text;
}

async function refreshRecent() {
  // The overview preview always wants the newest page; the queries view wants
  // the page it is on.
  const onQueries = currentView === "queries";
  const cursor = onQueries ? pageCursor(pager.page) || "" : "";
  const { query, filtered } = recentFilters(cursor);
  const key = query.toString();
  const generation = pager.generation;
  // Polling the same page must not abort its own in-flight request; a page or
  // filter change does supersede the previous one.
  if (recentInFlight) {
    if (recentInFlight.key === key && recentInFlight.generation === generation) return;
    recentInFlight.controller.abort();
  }
  const controller = new AbortController();
  recentInFlight = { key, controller, generation };
  const timeout = typeof AbortSignal.timeout === "function" ? AbortSignal.timeout(10000) : null;
  const signal = timeout && typeof AbortSignal.any === "function"
    ? AbortSignal.any([controller.signal, timeout])
    : controller.signal;
  // Switching pages keeps the previous rows on screen, dimmed, until the new
  // page arrives.
  const wrap = $("recent").closest(".table-wrap");
  const paging = onQueries && pager.shown !== key;
  if (paging) wrap.setAttribute("aria-busy", "true");
  let data;
  try {
    data = await api(`/api/v1/queries?${query}`, { signal, priority: paging ? "high" : "low" });
  } catch (error) {
    if (error.name === "AbortError") return;
    if (paging && generation === pager.generation) wrap.removeAttribute("aria-busy");
    if (!$("recent").dataset.loaded) {
      fillEmpty("recent", "暂时读不到数据。");
      fillEmpty("overview-recent", "暂时读不到数据。");
    } else if (paging && generation === pager.generation) {
      renderPager(`没能读取第 ${fmt.int(pager.page)} 页：${error.message}`);
    }
    return;
  } finally {
    if (recentInFlight && recentInFlight.controller === controller) recentInFlight = null;
  }
  if (generation !== pager.generation) return; // another page or filter meanwhile
  $("recent").dataset.loaded = "true";
  renderRecentNote(data);
  if (!cursor) {
    const preview = data.queries.slice(0, PREVIEW_LIMIT).map((record) => queryRow(record, true));
    if (!preview.length) preview.push(emptyRow(5, filtered ? "没有符合筛选条件的查询。" : "还没有查询经过分流器。"));
    $("overview-recent").tBodies[0].replaceChildren(...preview);
    const note = $("overview-recent-note");
    note.textContent = filtered ? "已按最近查询页的筛选条件显示。" : "";
    note.hidden = !filtered;
  }
  // The overview fetched page 1; it must not overwrite another page's table.
  if (cursor !== (pageCursor(pager.page) || "")) return;

  const rows = data.queries.map((record) => queryRow(record, false));
  if (!rows.length) rows.push(emptyRow(7, emptyText(filtered, Boolean(data.next))));
  $("recent").tBodies[0].replaceChildren(...rows);
  wrap.removeAttribute("aria-busy");
  if (pager.shown !== key) wrap.scrollTop = 0;
  pager.shown = key;
  pager.count = data.queries.length;
  pager.next = data.next || "";
  // Page 1 moves as queries arrive, so the cursors of later pages are rebuilt
  // from the page on screen.
  pager.cursors.length = pager.page;
  if (pager.next) pager.cursors[pager.page] = pager.next;
  renderPager();
}

// goToPage switches the queries view to page, as a new history entry.
function goToPage(page) {
  if (page === pager.page || pageCursor(page) === undefined) return;
  pager.page = page;
  pager.generation++;
  history.pushState(pagerState(), "", pageHref(page));
  updateNavLinks();
  renderPager();
  refreshRecent();
}

// resetPager returns to the newest page, for a filter or view change.
function resetPager() {
  pager.generation++;
  pager.page = 1;
  pager.cursors = [""];
  pager.next = "";
  pager.count = 0;
}

// restorePager reads the page from the URL, and the cursors of the pages
// before it from history.state when this entry saved them.
function restorePager(search) {
  pager.generation++;
  const page = Number.parseInt(search.get("page") || "", 10);
  const cursor = search.get("cursor") || "";
  if (!(page > 1) || !cursor) {
    resetPager();
    return;
  }
  const saved = history.state && Array.isArray(history.state.cursors) ? history.state.cursors : [];
  const cursors = new Array(page).fill(undefined);
  for (let i = 0; i < page - 1 && i < saved.length; i += 1) {
    if (typeof saved[i] === "string") cursors[i] = saved[i];
  }
  cursors[0] = "";
  cursors[page - 1] = cursor;
  pager.page = page;
  pager.cursors = cursors;
  pager.next = "";
  pager.count = 0;
}

function setPaused(paused) {
  view.paused = paused;
  $("f-pause").setAttribute("aria-pressed", String(paused));
  $("f-pause-label").textContent = paused ? "继续刷新" : "暂停刷新";
  $("f-pause-icon").setAttribute("href", paused ? "#i-play" : "#i-pause");
  // Only page 1 refreshes; later pages are fixed and ignore the toggle.
  if (!paused && pager.page === 1) refreshRecent();
}

// ── Observability ────────────────────────────────────────────────────────────
// The page mirrors the splitter panels of the Grafana "DNS 解析与分流"
// dashboard. Prometheus holds the history, so this page asks the splitter's own
// API (/api/v1/observability), which queries Prometheus server-side. Panel
// layout, PromQL and colours live in internal/observability; this file renders
// whatever panels the server returns.
const OBS_SECTIONS = [
  { id: "status", title: "状态与关键指标" },
  { id: "flow", title: "解析质量与流量" },
  { id: "detail", title: "分流与上游明细" },
  { id: "runtime", title: "运行与集合" },
];
const OBS_RANGE_LABEL = { "1h": "1 小时", "3h": "3 小时", "6h": "6 小时", "12h": "12 小时", "24h": "24 小时", "3d": "3 天", "7d": "7 天" };
const OBS_UNIT_LABEL = { reqps: "qps", percent: "%", ms: "ms" };
const OBS_RANGE_RE = /^\d+(?:s|m|h|d|w)$/;
const CHART_HEIGHT = 240;
const CHART_MARGIN = { l: 54, r: 12, t: 12, b: 26 };

const obs = {
  range: OBS_DEFAULT_RANGE,
  paused: false,
  loading: false,
  error: "",          // the most recent fetch failed at the network/HTTP level
  data: null,         // the most recent successful snapshot
  fetchedAt: 0,       // when the last successful snapshot arrived
  stale: false,       // showing older panels while Prometheus is unreachable
  generation: 0,      // discards responses to a superseded request
  nodes: new Map(),   // panel id → { el, update }
  chartDraws: new Map(), // plot element → redraw function
  chartObserver: null,
  abort: null,        // in-flight observability request
};

let chartFrame = 0;
const chartRedraws = new Set();

function svgEl(tag, attrs, text) {
  const el = document.createElementNS(SVG_NS, tag);
  for (const [key, value] of Object.entries(attrs || {})) {
    if (value === undefined || value === null) continue;
    el.setAttribute(key, String(value));
  }
  if (text !== undefined) el.textContent = text;
  return el;
}

const fixedFormats = new Map();

// fixed formats with exactly digits decimals and grouping, like toFixed but
// consistent with the Intl formatting used everywhere else on the page.
function fixed(value, digits) {
  let format = fixedFormats.get(digits);
  if (!format) {
    format = new Intl.NumberFormat("zh-CN", { minimumFractionDigits: digits, maximumFractionDigits: digits });
    fixedFormats.set(digits, format);
  }
  return format.format(value);
}

// formatObsValue renders a number for stat contexts, tables and tooltips.
function formatObsValue(value, unit, decimals) {
  if (value === null || value === undefined || !Number.isFinite(value)) return "—";
  switch (unit) {
    case "percent": return `${fixed(value, decimals ?? 2)}%`;
    case "reqps": return fixed(value, decimals ?? 3);
    case "ms": return `${fixed(value, decimals ?? (Math.abs(value) < 10 ? 2 : 1))} ms`;
    case "count": return intFormat.format(Math.round(value));
    case "duration": return fmt.duration(value);
    case "number": return fixed(value, decimals ?? 3);
    default: return fixed(value, decimals ?? 2);
  }
}

function obsUnitLabel(unit) {
  return OBS_UNIT_LABEL[unit] || "";
}

function rangeSeconds(value) {
  const match = /^(\d+)([smhdw])$/.exec(value || "");
  if (!match) return 0;
  const mult = { s: 1, m: 60, h: 3600, d: 86400, w: 604800 }[match[2]];
  return Number(match[1]) * mult;
}

let helpSeq = 0;

// helpDetails returns a "?" button and its note. The note is an auto popover,
// so it renders in the top layer: no ancestor overflow or content-visibility
// containment can clip it, and Escape or a click outside closes it.
function helpDetails(description) {
  const id = `help-${++helpSeq}`;
  const note = h("p", { class: "help-note", id, popover: "auto" }, description || "");
  const button = h("button", {
    type: "button",
    class: "help-trigger",
    "aria-label": "指标说明",
    "aria-expanded": "false",
    "aria-controls": id,
    popovertarget: id,
  }, "?");
  note.addEventListener("toggle", (event) => {
    const open = event.newState === "open";
    button.setAttribute("aria-expanded", String(open));
    if (open) {
      placeHelp(button, note);
      note.classList.add("help-placed");
    } else {
      note.classList.remove("help-placed");
    }
  });
  return h("span", { class: "panel-help" }, button, note);
}

let openHelp = null;

// placeHelp puts the note under its button, right-aligned to it when there is
// room and clamped to a 16px gutter otherwise; above the button when the space
// below is too short. It follows the button while the page scrolls.
function placeHelp(button, note) {
  openHelp = { button, note };
  const gutter = 16;
  const r = button.getBoundingClientRect();
  const w = note.offsetWidth;
  const hgt = note.offsetHeight;
  // Keep the note inside #main's column. Without this, a right-aligned note
  // next to a left-aligned heading extends over the sidebar; the popover is in
  // the top layer, so it is not clipped, but covering navigation looks broken.
  const main = document.getElementById("main");
  const minLeft = Math.max(gutter, main ? main.getBoundingClientRect().left + gutter : gutter);
  let left = r.right - w;
  if (left < minLeft) left = Math.min(r.left, innerWidth - w - gutter);
  left = Math.max(minLeft, left);
  let top = r.bottom + 6;
  if (top + hgt > innerHeight - gutter && r.top - 6 - hgt >= gutter) top = r.top - 6 - hgt;
  top = Math.min(top, innerHeight - hgt - gutter);
  note.style.left = `${Math.round(left)}px`;
  note.style.top = `${Math.round(Math.max(gutter, top))}px`;
}

function followHelp() {
  if (!openHelp) return;
  if (!openHelp.note.matches(":popover-open")) {
    openHelp = null;
    return;
  }
  // The button scrolled out of view: close instead of floating detached.
  const r = openHelp.button.getBoundingClientRect();
  if (r.bottom < 0 || r.top > innerHeight) openHelp.note.hidePopover();
  else placeHelp(openHelp.button, openHelp.note);
}
addEventListener("scroll", followHelp, { capture: true, passive: true });
addEventListener("resize", followHelp, { passive: true });

function obsNoticeCard(tone, title, detail) {
  return h("div", { class: `alert ${tone}` },
    icon("alert", 18),
    h("div", { class: "alert-copy" },
      h("p", { class: "alert-title" }, title),
      h("p", { class: "alert-detail" }, detail)));
}

function obsSetupCard() {
  const snippet = [
    "observability:",
    "  # Prometheus 的基准地址。配置后本页才启用；未配置时不会发出任何请求。",
    "  prometheus_url: http://127.0.0.1:9090",
    "  # 抓取本实例的 Prometheus job 名，用于「splitter 抓取」面板。",
    "  job: splitter",
    "  # 可选：右上角「在 Grafana 中打开」的链接。",
    "  dashboard_url: https://grafana.example/d/dns-quality/",
  ].join("\n");
  return h("section", { class: "panel obs-setup" },
    h("h2", { class: "panel-title" }, "还没有连接 Prometheus"),
    h("p", { class: "muted" }, "这一页画的是历史曲线，数据来自 Prometheus 中 splitter 自己导出的指标。配置一个地址即可启用；它只读，未配置时不影响其余页面和 DNS 服务。"),
    h("pre", { class: "snippet" }, h("code", null, snippet)),
    h("p", { class: "muted" }, "修改配置后点右上角的「重新加载配置」，或发送 SIGHUP。Prometheus 需要抓取本实例的 /metrics；面板口径与 Grafana 的「DNS 解析与分流」一致。"));
}

// updateObsControls reflects the page state in the toolbar.
function updateObsControls() {
  for (const input of document.querySelectorAll('#obs-range input[name="obs-range"]')) {
    input.checked = input.value === obs.range;
  }
  $("obs-pause").setAttribute("aria-pressed", String(obs.paused));
  $("obs-pause-label").textContent = obs.paused ? "继续刷新" : "暂停刷新";
  $("obs-pause-icon").setAttribute("href", obs.paused ? "#i-play" : "#i-pause");
  const refresh = $("obs-refresh");
  refresh.toggleAttribute("aria-busy", obs.loading);
  // Not .disabled: disabling the focused button drops focus to <body>.
  refresh.setAttribute("aria-disabled", String(obs.loading));

  const info = (obs.data && obs.data.prometheus) || null;
  const link = $("obs-grafana");
  if (info && info.dashboard_url) {
    link.hidden = false;
    link.href = info.dashboard_url;
  } else {
    link.hidden = true;
  }
  const parts = [`范围 ${OBS_RANGE_LABEL[obs.range] || obs.range}`];
  if (info && info.configured) {
    if (!info.reachable) parts.push("Prometheus 不可达");
    else if (obs.paused) parts.push("自动刷新已暂停");
    else parts.push(obs.loading ? "正在刷新" : "30 秒自动刷新");
  }
  if (obs.fetchedAt) parts.push(`更新于 ${fmt.clock(new Date(obs.fetchedAt))}`);
  const meta = $("obs-meta");
  meta.textContent = parts.join(" · ");
  meta.title = info && info.url ? `Prometheus：${info.url}` : "";
}

function resetChartObserver() {
  if (obs.chartObserver) obs.chartObserver.disconnect();
  chartRedraws.clear();
  if (chartFrame) cancelAnimationFrame(chartFrame);
  chartFrame = 0;
  obs.chartDraws.clear();
}

function clearObsPanels() {
  resetChartObserver();
  obs.nodes.clear();
  const host = $("obs-panels");
  host.replaceChildren();
  host.hidden = true;
  delete host.dataset.structure;
}

function renderObservability() {
  updateObsControls();
  const data = obs.data;
  const notice = $("obs-notice");
  const host = $("obs-panels");
  // Range and refresh controls mean nothing until a Prometheus is configured.
  $("obs-toolbar").hidden = Boolean(data) && !data.prometheus.configured;

  if (!data) {
    host.hidden = false;
    if (!host.dataset.structure) {
      host.replaceChildren(h("div", { class: "panel" },
        h("p", { class: "empty-note" }, obs.error ? "暂时读不到数据。" : "正在从 Prometheus 读取指标…")));
    }
    notice.replaceChildren(...(obs.error ? [obsNoticeCard("danger", "读不到可观测性数据", obs.error)] : []));
    return;
  }

  const info = data.prometheus;
  if (!info.configured) {
    clearObsPanels();
    notice.replaceChildren(obsSetupCard());
    return;
  }
  if (obs.error) {
    notice.replaceChildren(obsNoticeCard("danger", "读取可观测性数据失败", obs.error));
  } else if (!info.reachable) {
    notice.replaceChildren(obsNoticeCard("danger", "Prometheus 不可达",
      `${info.error || "请求失败"}\n\n地址：${info.url || "未配置"}${info.job ? ` · job：${info.job}` : ""}\n页面保留上一次的数据；每 30 秒自动重试。`));
  } else {
    notice.replaceChildren();
  }

  if (!data.panels || !data.panels.length) {
    clearObsPanels();
    return;
  }
  host.hidden = false;
  host.toggleAttribute("data-stale", obs.stale || !info.reachable || Boolean(obs.error));
  renderObsPanels(data);
}

function renderObsPanels(data) {
  const host = $("obs-panels");
  const signature = data.panels.map((panel) => panel.id).join(",");
  if (host.dataset.structure !== signature) {
    resetChartObserver();
    obs.nodes.clear();
    const ordered = [];
    const targets = new Map(); // panel id → grid element
    for (const section of OBS_SECTIONS) {
      const members = data.panels.filter((panel) => panel.section === section.id);
      const stats = members.filter((panel) => panel.kind === "stat");
      const rest = members.filter((panel) => panel.kind !== "stat");
      const grids = [];
      if (stats.length) {
        const grid = h("div", { class: "obs-grid obs-grid-stats" });
        grids.push(grid);
        for (const panel of stats) targets.set(panel.id, grid);
      }
      if (rest.length) {
        // One chart alone (the runtime section) reads better across the page.
        const grid = h("div", { class: "obs-grid" + (rest.length === 1 ? " obs-grid-single" : "") });
        grids.push(grid);
        for (const panel of rest) targets.set(panel.id, grid);
      }
      ordered.push(h("section", { class: "obs-section", "data-section": section.id, "aria-labelledby": `obs-${section.id}-title` },
        h("div", { class: "section-head" }, h("h2", { id: `obs-${section.id}-title` }, section.title)),
        ...grids));
    }
    host.replaceChildren(...ordered);
    for (const panel of data.panels) {
      const node = createObsPanel(panel);
      obs.nodes.set(panel.id, node);
      const grid = targets.get(panel.id);
      if (grid) grid.append(node.el);
    }
    host.dataset.structure = signature;
  }
  for (const panel of data.panels) {
    const node = obs.nodes.get(panel.id);
    if (node) node.update(panel, data);
  }
}

function createObsPanel(panel) {
  if (panel.kind === "series") return createObsChart(panel);
  if (panel.kind === "table") return createObsTable(panel);
  return createObsStat(panel);
}

function createObsStat(panel) {
  const value = h("p", { class: "card-value" });
  const context = h("p", { class: "card-context" });
  const help = helpDetails(panel.description);
  const el = h("article", { class: "card obs-stat", "data-panel": panel.id },
    h("div", { class: "chart-head" }, h("h3", { class: "card-label" }, panel.title), help),
    value,
    context);
  return {
    el,
    update(next) {
      value.replaceChildren(next.display || "—", h("small", { hidden: !obsUnitLabel(next.unit) }, obsUnitLabel(next.unit)));
      value.className = "card-value" + (next.tone ? ` tone-${next.tone}` : "");
      context.textContent = next.error ? next.error : (next.context || "");
      context.classList.toggle("tone-danger", Boolean(next.error));
      help.querySelector("p").textContent = next.description || "";
    },
  };
}

function createObsTable(panel) {
  const caption = h("caption", { class: "sr-only" }, panel.title);
  const thead = h("thead");
  const tbody = h("tbody");
  const table = h("table", { class: "table" }, caption, thead, tbody);
  const wrap = h("div", { class: "table-wrap", tabindex: "0", role: "region", "aria-label": panel.title }, table);
  const el = h("section", { class: "section obs-table", "data-panel": panel.id },
    h("div", { class: "section-head" },
      h("h3", { class: "panel-title" }, panel.title),
      helpDetails(panel.description)),
    wrap);
  return {
    el,
    update(next) {
      caption.textContent = next.title;
      thead.replaceChildren(h("tr", null, ...next.columns.map((column) =>
        h("th", { scope: "col", class: column.numeric ? "num" : null }, column.title))));
      if (!next.rows || !next.rows.length) {
        tbody.replaceChildren(h("tr", null, h("td", { colspan: next.columns.length, class: "empty" },
          next.error || "所选区间没有数据。")));
        return;
      }
      tbody.replaceChildren(...next.rows.map((row) => {
        const cells = next.columns.slice(1).map((column) => {
          const cell = row.cells[column.key] || {};
          const text = cell.text !== undefined ? cell.text : formatObsValue(cell.value, column.unit, column.decimals);
          return h("td", { class: `${column.numeric ? "num" : ""} ${cell.tone ? `tone-${cell.tone}` : ""}`.trim() }, text);
        });
        return h("tr", null, h("td", { class: "name" }, h("code", null, row.key)), ...cells);
      }));
    },
  };
}

function createObsChart(panel) {
  const title = h("h3", { class: "panel-title" }, panel.title);
  const help = helpDetails(panel.description);
  const plot = h("div", { class: "chart-plot" });
  const svg = svgEl("svg", { class: "chart-svg", role: "img", focusable: "false" });
  const tip = h("div", { class: "chart-tip", "aria-hidden": "true" });
  tip.hidden = true;
  plot.append(svg, tip);
  const legend = h("div", { class: "chart-legend", role: "group", "aria-label": "图例", tabindex: "-1" });
  const dataBody = h("div");
  const details = h("details", { class: "chart-data" }, h("summary", null, "数据表"), dataBody);
  details.addEventListener("toggle", () => {
    if (details.open && state.dirtyData) renderDataTable();
  });
  const el = h("figure", { class: "panel chart", "data-panel": panel.id },
    h("figcaption", { class: "chart-head" }, title, help), plot, legend, details);
  const state = { panel, snap: null, hidden: new Set(), layout: null, hoverLine: null, suspended: false, visibilityBound: false, dirtyData: true };

  const visibleSeries = () => state.panel.series.filter((series) => !state.hidden.has(series.name));
  const pointCount = () => (state.panel.series.length ? state.panel.series[0].values.length : 0);

  function draw() {
    const snap = state.snap;
    if (!snap || !el.isConnected || state.suspended) return;
    const width = Math.max(320, Math.round(plot.clientWidth || 640));
    const { l, r, t, b } = CHART_MARGIN;
    const pw = Math.max(10, width - l - r);
    const ph = Math.max(10, CHART_HEIGHT - t - b);
    svg.setAttribute("viewBox", `0 0 ${width} ${CHART_HEIGHT}`);
    svg.setAttribute("width", String(width));
    svg.setAttribute("height", String(CHART_HEIGHT));
    svg.replaceChildren();
    state.layout = null;
    state.hoverLine = null;
    tip.hidden = true;

    const n = pointCount();
    const visible = visibleSeries();
    const x = (i) => l + (n <= 1 ? pw / 2 : (i / (n - 1)) * pw);
    let max = 0;
    let hasData = false;
    if (state.panel.stacked) {
      // The "总计" series is drawn over the stack, never inside it: adding the
      // aggregate to the band sum would draw every query twice.
      const sums = new Array(n).fill(0);
      for (const series of visible) {
        if (series.line) continue;
        for (let i = 0; i < n; i += 1) {
          const v = series.values[i];
          if (v !== null && v !== undefined) {
            sums[i] += v;
            hasData = true;
          }
        }
      }
      for (const v of sums) if (v > max) max = v;
    }
    for (const series of visible) {
      for (const v of series.values) {
        if (v !== null && v !== undefined) {
          hasData = true;
          if (v > max) max = v;
        }
      }
    }
    if (!n || !visible.length || !hasData) {
      const empty = state.panel.error
        ? "读取这个面板失败，展开「数据表」查看原因"
        : visible.length ? "这段时间没有数据" : "未选择任何系列";
      svg.append(svgEl("text", { x: width / 2, y: CHART_HEIGHT / 2, "text-anchor": "middle", class: "chart-empty" }, empty));
      svg.setAttribute("aria-label", `${state.panel.title}：没有数据`);
      return;
    }

    const step = niceStep(max > 0 ? max / 4 : 1);
    const yMax = Math.max(step, Math.ceil(max / step) * step);
    const y = (v) => t + ph - (v / yMax) * ph;

    for (let v = 0; v <= yMax + step * 1e-6; v += step) {
      const yy = y(v);
      svg.append(svgEl("line", { x1: l, x2: width - r, y1: yy, y2: yy, class: "chart-grid" }));
      svg.append(svgEl("text", { x: l - 8, y: yy + 4, "text-anchor": "end", class: "chart-axis" }, axisValue(v, state.panel.unit)));
    }
    const ticks = n > 4 ? 4 : Math.max(0, n - 1);
    for (let k = 0; k <= ticks; k += 1) {
      const i = Math.round((k / Math.max(ticks, 1)) * (n - 1));
      const label = axisTime(snap.start_ms + i * snap.step_ms, snap.end_ms - snap.start_ms);
      svg.append(svgEl("text", {
        x: x(i), y: CHART_HEIGHT - 6, class: "chart-axis",
        "text-anchor": k === 0 ? "start" : k === ticks ? "end" : "middle",
      }, label));
    }

    if (state.panel.stacked) {
      let base = new Array(n).fill(0);
      for (const series of visible) {
        if (series.line) continue;
        const top = new Array(n);
        for (let i = 0; i < n; i += 1) {
          const v = series.values[i];
          top[i] = base[i] + (v === null || v === undefined ? 0 : v);
        }
        svg.append(svgEl("path", { d: bandPath(base, top, x, y, n), class: `chart-area c-${series.color}` }));
        svg.append(svgEl("path", { d: linePath(top, x, y, n), class: `chart-line c-${series.color}` }));
        base = top;
      }
      for (const series of visible) {
        if (!series.line) continue;
        const d = linePath(series.values, x, y, n);
        if (d) svg.append(svgEl("path", { d, class: `chart-line chart-line-strong c-${series.color}` }));
      }
    } else {
      for (const series of visible) {
        const d = linePath(series.values, x, y, n);
        if (d) svg.append(svgEl("path", { d, class: `chart-line c-${series.color}` }));
      }
    }

    const hover = svgEl("line", { class: "chart-hover", x1: 0, x2: 0, y1: t, y2: t + ph, visibility: "hidden" });
    svg.append(hover);
    state.hoverLine = hover;
    state.layout = { l, r, t, b, pw, ph, width, n, x, y, snap };
    const latest = visible.map((series) => `${series.name} ${formatObsValue(latestValue(series.values), state.panel.unit)}`);
    svg.setAttribute("aria-label", `${state.panel.title}：${visible.length} 条曲线。${latest.slice(0, 4).join("；")}`);
  }

  function pointerIndex(clientX) {
    const layout = state.layout;
    if (!layout) return -1;
    const rect = svg.getBoundingClientRect();
    if (!rect.width) return -1;
    const px = ((clientX - rect.left) / rect.width) * layout.width;
    if (px < layout.l - 4 || px > layout.width - layout.r + 4) return -1;
    const ratio = Math.min(1, Math.max(0, (px - layout.l) / layout.pw));
    return Math.round(ratio * (layout.n - 1));
  }

  function hideTip() {
    if (state.hoverLine) state.hoverLine.setAttribute("visibility", "hidden");
    tip.hidden = true;
  }

  function showTip(event) {
    const i = pointerIndex(event.clientX);
    if (i < 0) {
      hideTip();
      return;
    }
    const layout = state.layout;
    const xx = layout.x(i);
    state.hoverLine.setAttribute("x1", xx);
    state.hoverLine.setAttribute("x2", xx);
    state.hoverLine.setAttribute("visibility", "visible");
    const at = new Date(layout.snap.start_ms + i * layout.snap.step_ms);
    tip.replaceChildren(
      h("p", { class: "tip-time" }, fmt.full(at)),
      ...visibleSeries().map((series) => h("div", { class: `tip-row c-${series.color}` },
        h("span", { class: "swatch" }),
        h("span", { class: "tip-name" }, series.name),
        h("span", { class: "tip-value" }, formatObsValue(series.values[i], state.panel.unit)))),
    );
    tip.hidden = false;
    const plotRect = plot.getBoundingClientRect();
    const rel = event.clientX - plotRect.left;
    const tipWidth = tip.offsetWidth;
    let left = rel + 12;
    if (left + tipWidth > plotRect.width - 4) left = rel - tipWidth - 12;
    tip.style.left = `${Math.max(4, left)}px`;
    tip.style.top = "4px";
  }

  plot.addEventListener("pointermove", (event) => showTip(event));
  plot.addEventListener("pointerdown", (event) => showTip(event));
  plot.addEventListener("pointerleave", hideTip);

  // The legend is rebuilt only when the set of series changes. A refresh with
  // the same series updates the values in place, so a focused legend button
  // keeps its focus across the 30-second poll.
  const legendItems = new Map(); // series name → { button, value }
  let legendSignature = "";

  function renderLegend() {
    const many = state.panel.series.length > 8;
    legend.dataset.many = String(many);
    legend.tabIndex = many ? 0 : -1;
    legend.setAttribute("aria-label", many ? "图例（可滚动）" : "图例");
    const signature = state.panel.series.map((series) => `${series.name}\u0000${series.color}`).join("\n");
    if (signature === legendSignature) {
      for (const series of state.panel.series) {
        const item = legendItems.get(series.name);
        if (!item) continue;
        item.value.textContent = formatObsValue(latestValue(series.values), state.panel.unit);
        item.button.setAttribute("aria-pressed", String(!state.hidden.has(series.name)));
      }
      return;
    }
    legendSignature = signature;
    legendItems.clear();
    legend.replaceChildren(...state.panel.series.map((series) => {
      const value = h("span", { class: "legend-value" }, formatObsValue(latestValue(series.values), state.panel.unit));
      const button = h("button", {
        type: "button",
        class: `legend-item c-${series.color}`,
        "aria-pressed": String(!state.hidden.has(series.name)),
        title: series.name,
      },
      h("span", { class: "swatch" }),
      h("span", { class: "legend-name" }, series.name),
      value);
      legendItems.set(series.name, { button, value });
      button.addEventListener("click", () => {
        if (state.hidden.has(series.name)) state.hidden.delete(series.name);
        else state.hidden.add(series.name);
        button.setAttribute("aria-pressed", String(!state.hidden.has(series.name)));
        draw();
      });
      return button;
    }));
  }

  let dataTbody = null; // kept across refreshes so the scrollport survives

  function renderDataTable() {
    state.dirtyData = false;
    if (state.panel.error) {
      dataTbody = null;
      dataBody.replaceChildren(h("p", { class: "muted chart-error" }, state.panel.error));
      return;
    }
    const rows = state.panel.series.map((series) => {
      let min = null, max = null, sum = 0, count = 0, latest = null;
      for (const v of series.values) {
        if (v === null || v === undefined) continue;
        if (min === null || v < min) min = v;
        if (max === null || v > max) max = v;
        sum += v;
        count += 1;
        latest = v;
      }
      return h("tr", null,
        h("td", { class: "nowrap" }, h("span", { class: `swatch c-${series.color}` }), " ", series.name),
        h("td", { class: "num" }, formatObsValue(latest, state.panel.unit)),
        h("td", { class: "num" }, formatObsValue(min, state.panel.unit)),
        h("td", { class: "num" }, formatObsValue(max, state.panel.unit)),
        h("td", { class: "num" }, formatObsValue(count ? sum / count : null, state.panel.unit)));
    });
    if (!rows.length) rows.push(h("tr", null, h("td", { colspan: 5, class: "empty" }, "所选区间没有数据。")));
    if (dataTbody && dataTbody.isConnected) {
      dataTbody.replaceChildren(...rows);
      return;
    }
    const body = h("tbody", null, ...rows);
    dataTbody = body;
    dataBody.replaceChildren(h("div", { class: "table-wrap", tabindex: "0", role: "region", "aria-label": `${state.panel.title} 数据表` },
      h("table", { class: "table" },
        h("caption", { class: "sr-only" }, `${state.panel.title} 数据表`),
        h("thead", null, h("tr", null,
          h("th", { scope: "col" }, "系列"),
          h("th", { scope: "col", class: "num" }, "最新"),
          h("th", { scope: "col", class: "num" }, "最小"),
          h("th", { scope: "col", class: "num" }, "最大"),
          h("th", { scope: "col", class: "num" }, "平均"))),
        body)));
  }

  return {
    el,
    update(next, snap) {
      state.panel = next;
      state.snap = snap;
      title.textContent = next.title;
      el.toggleAttribute("data-error", Boolean(next.error));
      const helpText = help.querySelector("p");
      if (helpText) helpText.textContent = next.description || "";
      if (!state.visibilityBound) {
        const section = el.closest(".obs-section");
        if (section) {
          section.addEventListener("contentvisibilityautostatechange", (event) => {
            state.suspended = Boolean(event.skipped);
            if (!event.skipped) draw();
          });
          state.visibilityBound = true;
        }
      }
      renderLegend();
      state.dirtyData = true;
      if (details.open) renderDataTable();
      draw();
      obs.chartDraws.set(plot, draw);
      getChartObserver().observe(plot);
    },
  };
}

function getChartObserver() {
  if (!obs.chartObserver) {
    obs.chartObserver = new ResizeObserver((entries) => {
      for (const entry of entries) {
        const redraw = obs.chartDraws.get(entry.target);
        if (redraw) chartRedraws.add(redraw);
      }
      if (!chartRedraws.size || chartFrame) return;
      chartFrame = requestAnimationFrame(() => {
        chartFrame = 0;
        const redraws = [...chartRedraws];
        chartRedraws.clear();
        for (const redraw of redraws) redraw();
      });
    });
  }
  return obs.chartObserver;
}

function latestValue(values) {
  for (let i = values.length - 1; i >= 0; i -= 1) {
    if (values[i] !== null && values[i] !== undefined) return values[i];
  }
  return null;
}

function linePath(values, x, y, n) {
  let d = "";
  let pen = false;
  for (let i = 0; i < n; i += 1) {
    const v = values[i];
    if (v === null || v === undefined) {
      pen = false;
      continue;
    }
    d += `${pen ? "L" : "M"}${x(i).toFixed(1)} ${y(v).toFixed(1)}`;
    pen = true;
  }
  return d;
}

function bandPath(base, top, x, y, n) {
  let d = "";
  for (let i = 0; i < n; i += 1) d += `${i ? "L" : "M"}${x(i).toFixed(1)} ${y(top[i]).toFixed(1)}`;
  for (let i = n - 1; i >= 0; i -= 1) d += `L${x(i).toFixed(1)} ${y(base[i]).toFixed(1)}`;
  return `${d}Z`;
}

function niceStep(raw) {
  if (!(raw > 0)) return 1;
  const base = Math.pow(10, Math.floor(Math.log10(raw)));
  const fraction = raw / base;
  const mult = fraction <= 1 ? 1 : fraction <= 2 ? 2 : fraction <= 5 ? 5 : 10;
  return mult * base;
}

function axisValue(value, unit) {
  if (value === 0) return "0";
  switch (unit) {
    case "count": return compactFormat.format(value);
    case "percent": return `${Math.round(value)}%`;
    case "ms": return value >= 100 ? String(Math.round(value)) : trimNumber(value, value < 10 ? 1 : 0);
    case "reqps": return Math.abs(value) >= 1000 ? compactFormat.format(value) : Math.abs(value) >= 10 ? String(Math.round(value)) : trimNumber(value, value < 1 ? 2 : 1);
    default: return trimNumber(value, 3);
  }
}

// trimNumber formats with at most digits decimals and drops trailing zeros, so
// an axis reads "5" rather than "5.0".
function trimNumber(value, digits) {
  return Number(value.toFixed(digits)).toString();
}

function axisTime(ms, span) {
  return (span <= 86400000 ? axisClockFormat : axisDayFormat).format(new Date(ms));
}

async function refreshObservability(manual = false) {
  const range = obs.range;
  const generation = ++obs.generation;
  // Superseded range changes or manual refreshes must not finish after the
  // newer request and paint stale numbers.
  if (obs.abort) obs.abort.abort();
  const controller = new AbortController();
  obs.abort = controller;
  const timeout = typeof AbortSignal.timeout === "function" ? AbortSignal.timeout(25000) : null;
  const signal = timeout && typeof AbortSignal.any === "function"
    ? AbortSignal.any([controller.signal, timeout])
    : controller.signal;
  obs.loading = true;
  updateObsControls();
  try {
    const data = await api(`/api/v1/observability?range=${encodeURIComponent(range)}`, {
      signal,
      priority: manual ? "high" : "low",
    });
    if (generation !== obs.generation) return;
    if (data.prometheus.configured && !data.prometheus.reachable && obs.data && obs.data.panels.length) {
      // Keep the last panels visible, but say why they are frozen.
      obs.data = { ...obs.data, prometheus: data.prometheus };
      obs.stale = true;
    } else {
      obs.data = data;
      obs.stale = false;
      if (!data.prometheus.configured || data.prometheus.reachable) obs.fetchedAt = Date.now();
    }
    obs.error = "";
  } catch (error) {
    if (generation !== obs.generation) return;
    if (error.name === "AbortError") return;
    if (error.name === "TimeoutError") obs.error = "读取超时，稍后自动重试。";
    else obs.error = error.network ? `${error.message}，稍后自动重试。` : error.message;
    obs.stale = true;
  } finally {
    if (generation === obs.generation) {
      obs.loading = false;
      obs.abort = null;
      updateObsControls();
      // Do not touch hidden panels; entering the view renders the cached data.
      if (currentView === "observability") renderObservability();
    }
  }
}

function enterObservability() {
  const seconds = rangeSeconds(obs.range);
  const loaded = obs.data && Math.round(obs.data.window_seconds) === seconds;
  const fresh = obs.fetchedAt && Date.now() - obs.fetchedAt < OBS_INTERVAL;
  updateObsControls();
  if (!loaded || !fresh) {
    refreshObservability();
    return;
  }
  renderObservability();
}

function setObsRange(value) {
  if (!OBS_RANGE_RE.test(value) || value === obs.range) return;
  obs.range = value;
  updateObsControls();
  syncURL();
  refreshObservability(true);
}

function setObsPaused(paused) {
  obs.paused = paused;
  updateObsControls();
  if (!paused) refreshObservability(true);
}


// ── Route diagnosis ──────────────────────────────────────────────────────────
function matchText(route) {
  if (route.set) return `集合 ${route.set}（${route.match}：${KIND_LABEL[route.match] || route.match}）`;
  return "没有命中任何集合，落到这条规则";
}

function resultGrid(route, resolved) {
  const pairs = [
    ["查询", h("code", null, `${route.name || "."} ${route.type}`)],
    ["命中规则", ruleBadge(route.rule)],
    ["匹配依据", matchText(route)],
    ["去向", h("span", null, `${ACTION_LABEL[route.action] || route.action} `, h("code", null, route.target))],
  ];
  if (resolved) {
    pairs.push(["结果", h("div", { class: "chips" }, rcodeBadge(resolved.rcode), resolved.truncated ? badge("已截断", "", "outline") : null)]);
    pairs.push(["耗时", fmt.ms(resolved.duration_ms)]);
  }
  return h("dl", { class: "result-grid" }, pairs.map(([term, value]) => h("div", null, h("dt", null, term), h("dd", null, value))));
}

function answersTable(answers) {
  return h("div", { class: "table-wrap", tabindex: "0", role: "region", "aria-label": "应答记录" },
    h("table", { class: "table" },
      h("caption", { class: "sr-only" }, "应答记录"),
      h("thead", null, h("tr", null,
        h("th", { scope: "col" }, "名称"), h("th", { scope: "col" }, "类型"),
        h("th", { scope: "col", class: "num" }, "TTL"), h("th", { scope: "col" }, "数据"))),
      h("tbody", null, answers.map((answer) => h("tr", null,
        h("td", { class: "name" }, h("code", null, answer.name || ".")),
        h("td", null, answer.type),
        h("td", { class: "num" }, `${fmt.int(answer.ttl)} 秒`),
        h("td", { class: "name" }, h("code", null, answer.data)))))));
}

function showDiagnoseError(message) {
  const error = $("d-error");
  error.textContent = message;
  error.hidden = !message;
  $("d-name").setAttribute("aria-invalid", message ? "true" : "false");
}

let diagnosing = false;

async function diagnose(resolve) {
  if (diagnosing) return;
  const name = $("d-name").value.trim();
  const type = $("d-type").value;
  if (!name) {
    showDiagnoseError("先填一个域名，例如 www.baidu.com。");
    $("d-name").focus();
    return;
  }
  showDiagnoseError("");
  syncURL();
  const buttons = [$("d-route"), $("d-resolve")];
  const active = resolve ? $("d-resolve") : $("d-route");
  const idle = active.textContent;
  // aria-disabled, not disabled: disabling the focused button drops focus.
  diagnosing = true;
  buttons.forEach((button) => { button.setAttribute("aria-disabled", "true"); });
  active.setAttribute("aria-busy", "true");
  active.textContent = resolve ? "正在解析…" : "正在查看…";
  try {
    const result = $("d-result");
    if (resolve) {
      const resolved = await postJSON("/api/v1/resolve", { name, type });
      result.replaceChildren(
        resultGrid(resolved.route, resolved),
        resolved.answers.length
          ? answersTable(resolved.answers)
          : h("p", { class: "result-note" }, resolved.truncated
            ? "应答被截断（TC），没有带回记录；真实客户端会改走 TCP 重试。"
            : "应答里没有记录。"));
      refreshState();
      if (!view.paused) refreshRecent();
    } else {
      const route = await api(`/api/v1/route?${new URLSearchParams({ name, type })}`);
      result.replaceChildren(resultGrid(route, null), h("p", { class: "result-note" }, "只是按当前规则推演，没有发出查询。"));
    }
    result.hidden = false;
  } catch (error) {
    $("d-result").hidden = true;
    if (error.network) showDiagnoseError(`${error.message}，稍后再试。`);
    else if (error.status === 400) showDiagnoseError(`分流器不接受这个输入：${error.message}`);
    else showDiagnoseError(`${resolve ? "解析" : "查看路由"}失败：${error.message}`);
    $("d-name").focus();
  } finally {
    diagnosing = false;
    buttons.forEach((button) => { button.removeAttribute("aria-disabled"); });
    active.removeAttribute("aria-busy");
    active.textContent = idle;
  }
}

// ── Reload, theme and toasts ─────────────────────────────────────────────────
function toast(message) {
  const el = h("div", { class: "toast" }, icon("check", 18), message);
  $("toasts").append(el);
  setTimeout(() => el.remove(), 4000);
}

async function reloadConfig() {
  const button = $("reload");
  if (button.getAttribute("aria-disabled") === "true") return;
  button.setAttribute("aria-disabled", "true");
  button.setAttribute("aria-busy", "true");
  $("reload-label").textContent = "正在重新加载…";
  view.actionError = "";
  try {
    await postJSON("/api/v1/reload", {});
    toast("已重新加载配置和列表");
  } catch (error) {
    // 422: the new configuration was refused. The reason is reported through the
    // state (last_reload) and shown in the banner, so it is not repeated here.
    if (error.status !== 422) view.actionError = error.network ? `${error.message}，请求没有送达。` : error.message;
  } finally {
    button.removeAttribute("aria-disabled");
    button.removeAttribute("aria-busy");
    $("reload-label").textContent = "重新加载配置";
  }
  await refreshState();
  // A reload can enable, disable or repoint observability.prometheus_url.
  if (currentView === "observability") refreshObservability(true);
}

const THEME_COLORS = { light: "#ffffff", dark: "#141414" }; // mirrors --background; also in theme.js

function applyThemeControl() {
  const dark = document.documentElement.dataset.theme === "dark";
  $("theme-icon").setAttribute("href", dark ? "#i-moon" : "#i-sun");
  $("theme").setAttribute("aria-label", dark ? "切换为浅色" : "切换为深色");
  const meta = $("theme-color");
  if (meta) meta.setAttribute("content", dark ? THEME_COLORS.dark : THEME_COLORS.light);
}

function toggleTheme() {
  const next = document.documentElement.dataset.theme === "dark" ? "light" : "dark";
  document.documentElement.dataset.theme = next;
  try { localStorage.setItem("splitter-theme", next); } catch (e) { /* if the preference cannot be stored, it applies to this session only */ }
  applyThemeControl();
}

// ── Navigation drawer ────────────────────────────────────────────────────────
// Below 960px the sidebar is a popover drawer. Wide layout removes the popover
// and the dialog role so the same element is a normal sidebar.
function setupDrawer() {
  const drawer = $("drawer");
  const scroller = drawer.querySelector(".drawer-scroller");
  const sheet = drawer.querySelector(".drawer-sheet");
  const openBtn = $("nav-open");
  const closeBtn = $("nav-close");
  const workspace = $("workspace");
  const popoverOK = Object.hasOwn(HTMLElement.prototype, "popover");
  const layoutQuery = matchMedia("(min-width: 960px)");
  let suppressClose = false;
  let closeTimer = 0;
  let restoreFocus = true;

  function wide() { return layoutQuery.matches; }

  function setBackgroundInert(on) {
    workspace.inert = on;
    $("skip").inert = on;
    $("toasts").inert = on;
  }

  function finishClose() {
    if (!drawer.hasAttribute("aria-modal")) return;
    drawer.removeAttribute("aria-modal");
    clearTimeout(closeTimer);
    suppressClose = false;
    if (popoverOK && drawer.matches(":popover-open")) drawer.hidePopover();
    setBackgroundInert(false);
    openBtn.setAttribute("aria-expanded", "false");
    const target = pendingFocus;
    pendingFocus = null;
    if (target) target.focus({ preventScroll: true });
    else if (restoreFocus) openBtn.focus({ preventScroll: true });
  }

  function syncLayout() {
    if (wide()) {
      if (drawer.hasAttribute("aria-modal")) finishClose();
      if (popoverOK && drawer.matches(":popover-open")) drawer.hidePopover();
      drawer.removeAttribute("popover");
      drawer.removeAttribute("role");
      drawer.removeAttribute("aria-label");
      document.documentElement.dataset.layout = "wide";
      return;
    }
    document.documentElement.dataset.layout = "narrow";
    if (!popoverOK) return;
    if (!drawer.hasAttribute("popover")) drawer.setAttribute("popover", "manual");
    drawer.setAttribute("role", "dialog");
    drawer.setAttribute("aria-label", "控制台导航");
  }

  async function openDrawer() {
    if (wide() || !popoverOK) return;
    syncLayout();
    suppressClose = true;
    restoreFocus = true;
    drawer.showPopover();
    setBackgroundInert(true);
    drawer.setAttribute("aria-modal", "true");
    openBtn.setAttribute("aria-expanded", "true");
    if (!CSS.supports("scroll-initial-target", "nearest")) {
      scroller.scrollTo({ left: scroller.offsetWidth, behavior: "instant" });
      await new Promise((resolve) => requestAnimationFrame(() => requestAnimationFrame(resolve)));
    }
    scroller.scrollTo({ left: 0, behavior: "auto" });
    closeBtn.focus({ preventScroll: true });
  }

  closeDrawer = function (options) {
    if (!drawer.hasAttribute("aria-modal")) return;
    restoreFocus = !options || options.restoreFocus !== false;
    scroller.scrollTo({ left: scroller.offsetWidth, behavior: "auto" });
    clearTimeout(closeTimer);
    closeTimer = setTimeout(() => {
      if (drawer.hasAttribute("aria-modal")) finishClose();
    }, 700);
  };

  const visibleThreshold = 1 / Math.max(window.innerWidth, 1);
  const observer = new IntersectionObserver((entries) => {
    if (wide()) return;
    const entry = entries.at(-1);
    if (!entry) return;
    if (entry.intersectionRatio === 1) {
      suppressClose = false;
      openBtn.setAttribute("aria-expanded", "true");
      return;
    }
    if (suppressClose || !drawer.hasAttribute("aria-modal")) return;
    if (entry.intersectionRatio < visibleThreshold) finishClose();
  }, { root: drawer, threshold: [Math.min(visibleThreshold, 0.99), 1] });
  observer.observe(sheet);

  if (!CSS.supports("animation-timeline", "scroll()")) {
    scroller.addEventListener("scroll", () => {
      const width = sheet.offsetWidth || 1;
      const ratio = 1 - scroller.scrollLeft / width;
      drawer.style.setProperty("--drawer-backdrop", String(Math.min(1, Math.max(0, ratio))));
    }, { passive: true });
  }

  openBtn.addEventListener("click", openDrawer);
  closeBtn.addEventListener("click", () => closeDrawer());
  drawer.addEventListener("click", (event) => {
    if (!sheet.contains(event.target)) closeDrawer();
  });
  document.addEventListener("keydown", (event) => {
    if (event.key === "Escape" && drawer.hasAttribute("aria-modal")) {
      event.preventDefault();
      closeDrawer();
    }
  });
  layoutQuery.addEventListener("change", syncLayout);
  syncLayout();
}

// ── Startup ──────────────────────────────────────────────────────────────────
function start() {
  setupDrawer();
  applyThemeControl();
  applyView(viewFromLocation());
  updateNavLinks();
  $("theme").addEventListener("click", toggleTheme);
  $("reload").addEventListener("click", reloadConfig);
  const systemTheme = matchMedia("(prefers-color-scheme: dark)");
  systemTheme.addEventListener("change", () => {
    let stored = null;
    try { stored = localStorage.getItem("splitter-theme"); } catch (e) { /* apply for this session */ }
    if (stored === "light" || stored === "dark") return;
    document.documentElement.dataset.theme = systemTheme.matches ? "dark" : "light";
    applyThemeControl();
  });

  document.addEventListener("click", (event) => {
    const link = event.target.closest("a[data-view]");
    if (!link || event.defaultPrevented || event.button !== 0) return;
    if (event.metaKey || event.ctrlKey || event.shiftKey || event.altKey) return;
    event.preventDefault();
    navigate(link.dataset.view, { focus: event.detail === 0 });
  });
  addEventListener("popstate", () => {
    if ($("drawer").hasAttribute("aria-modal")) {
      pendingFocus = $("page-title");
      closeDrawer({ restoreFocus: false });
    }
    restoreFromLocation();
    updateNavLinks();
    const next = viewFromLocation();
    applyView(next);
    render();
    if (next === "overview" || next === "queries") refreshRecent();
    if (next === "observability") enterObservability();
    if (next === "diagnose" && $("d-name").value.trim()) diagnose(false);
    else $("d-result").hidden = true;
  });

  $("diagnose").addEventListener("submit", (event) => { event.preventDefault(); diagnose(false); });
  $("d-resolve").addEventListener("click", () => diagnose(true));
  $("d-name").addEventListener("input", () => showDiagnoseError(""));

  let debounce = 0;
  $("f-q").addEventListener("input", () => {
    clearTimeout(debounce);
    debounce = setTimeout(() => { resetPager(); syncURL(); refreshRecent(); }, 250);
  });
  $("f-rule").addEventListener("change", () => { resetPager(); syncURL(); refreshRecent(); });
  $("filters").addEventListener("submit", (event) => { event.preventDefault(); resetPager(); syncURL(); refreshRecent(); });
  $("f-pause").addEventListener("click", () => setPaused(!view.paused));
  $("recent-pager").addEventListener("click", (event) => {
    const link = event.target.closest("a.pager-link");
    if (!link || event.defaultPrevented || event.button !== 0) return;
    if (event.metaKey || event.ctrlKey || event.shiftKey || event.altKey) return;
    event.preventDefault();
    if (link.dataset.page) goToPage(Number(link.dataset.page));
  });
  $("trend-details").addEventListener("toggle", () => {
    if ($("trend-details").open) renderTrendTable();
  });

  $("obs-range").addEventListener("change", (event) => {
    const input = event.target.closest('input[name="obs-range"]');
    if (input) setObsRange(input.value);
  });
  $("obs-refresh").addEventListener("click", () => { if (!obs.loading) refreshObservability(true); });
  $("obs-pause").addEventListener("click", () => setObsPaused(!obs.paused));

  // Filters and diagnosis input are stored in the URL so that the view can be
  // bookmarked or shared. A name without an explicit view opens the diagnosis.
  restoreFromLocation();

  refreshState().then(() => {
    refreshRecent();
    if (currentView === "observability") enterObservability();
    if ($("d-name").value && currentView === "diagnose") diagnose(false);
  });
  setInterval(() => { if (!document.hidden) refreshState(); }, STATE_INTERVAL);
  setInterval(() => {
    if (document.hidden || view.paused) return;
    if (currentView === "overview" || (currentView === "queries" && pager.page === 1)) refreshRecent();
  }, RECENT_INTERVAL);
  setInterval(() => {
    if (document.hidden || obs.paused) return;
    if (currentView === "observability") refreshObservability();
  }, OBS_INTERVAL);
  document.addEventListener("visibilitychange", () => {
    if (document.hidden) return;
    refreshState();
    if (!view.paused) refreshRecent();
    if (!obs.paused && currentView === "observability") refreshObservability();
  });
}

start();
