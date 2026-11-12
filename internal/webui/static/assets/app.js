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
  overview: { title: "概况", desc: "查询量、上游健康，以及需要处理的事项。" },
  rules: { title: "分流规则", desc: "自上而下，第一条命中的生效。" },
  upstreams: { title: "上游", desc: "一组内按顺序尝试，传输失败才换下一台。" },
  sets: { title: "域名集合", desc: "列表文件由周更任务维护。" },
  diagnose: { title: "路由诊断", desc: "查看路由不发送查询。实际解析会向上游发一条真实查询，并计入统计。" },
  queries: { title: "最近查询", desc: "只保留经过规则求值的查询。前置解析器缓存命中的不会到这里。" },
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

// ── API ──────────────────────────────────────────────────────────────────────
async function api(path, options) {
  let response;
  try {
    response = await fetch(path, { cache: "no-store", ...options });
  } catch (cause) {
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
  if (currentView !== "overview") next.set("view", currentView);
  const query = next.toString();
  history.replaceState(null, "", query ? `?${query}` : location.pathname);
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

function navigate(name, options) {
  const next = knownView(name);
  if (!next) return;
  const changed = next !== currentView;
  const drawerOpen = $("drawer").hasAttribute("aria-modal");
  if (changed) {
    const url = new URL(location.href);
    if (next === "overview") url.searchParams.delete("view");
    else url.searchParams.set("view", next);
    history.pushState({ view: next }, "", url);
    updateNavLinks();
    applyView(next);
    if (next === "overview" || next === "queries") refreshRecent();
  }
  // A mouse click on the always-visible sidebar leaves focus where it is.
  // Keyboard activation, and any click that closes the drawer, moves focus
  // to the page title: the activating link is either about to be hidden or
  // the view change would otherwise stay silent.
  const moveFocus = !options || options.focus !== false;
  if (drawerOpen) {
    pendingFocus = $("page-title");
    closeDrawer({ restoreFocus: false });
  } else if (changed && moveFocus) {
    $("page-title").focus({ preventScroll: true });
  }
}

const ruleTone = (name) => view.tones.get(name) || "";
const ruleBadge = (name) => badge(name, ruleTone(name));

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

function renderRuntime(status) {
  const now = new Date(status.now);
  const loaded = new Date(status.loaded_at);
  const version = versionText(status);
  const listen = (status.listen || []).join("、");
  $("runtime").replaceChildren(
    prop("版本", h("code", null, version)),
    prop("监听", h("code", null, listen)),
    prop("管理端口", h("code", null, status.admin)),
    prop("已运行", fmt.duration((now - new Date(status.started_at)) / 1000)),
    prop("配置加载于", h("span", { title: fmt.full(loaded) }, `${fmt.dateTime(loaded)}（${fmt.relative(loaded, now)}）`)),
    prop("日志", status.log_queries ? `${status.log_level}，记录查询` : `${status.log_level}，不记录查询`),
    prop("在途查询", `${fmt.int(status.inflight)} / ${fmt.int(status.max_inflight)}`),
    prop("TCP 连接", `${fmt.int(status.tcp_connections)} / ${fmt.int(status.max_tcp_conns)}`),
    prop("本地拒绝", fmt.int(status.refused_locally)),
    prop("畸形查询", fmt.int(status.malformed)),
    prop("成功重载", fmt.int(status.reloads_ok)),
    prop("被拒重载", fmt.int(status.reloads_failed)),
  );
  $("meta-version").textContent = version;
  $("meta-listen").textContent = listen;
}

function renderRcodes(rcodes) {
  const entries = Object.entries(rcodes || {}).filter(([, n]) => n > 0).sort((a, b) => b[1] - a[1]);
  const el = $("rcodes");
  if (!entries.length) {
    el.hidden = true;
    el.replaceChildren();
    return;
  }
  el.hidden = false;
  el.replaceChildren(
    h("span", { class: "rcodes-label" }, "应答码"),
    ...entries.map(([code, n]) => rcodeBadge(code, n)));
}

function attentionRow(tone, label, text) {
  return h("div", { class: "attn" }, badge(label, tone), h("p", null, text));
}

function renderAttention(data) {
  const items = [];
  const now = new Date(data.status.now);
  for (const group of data.upstreams) {
    for (const server of group.servers) {
      if (!server.penalized) continue;
      items.push(attentionRow("warning", "惩罚中", `${group.name} 的 ${server.addr} 连续失败，暂排在其他服务器之后。`));
    }
  }
  for (const set of data.sets) {
    for (const source of set.sources) {
      const modified = new Date(source.modified);
      if (modified.getUTCFullYear() <= 1970) continue;
      const days = (now - modified) / 86400000;
      if (days > LIST_STALE_DAYS) {
        items.push(attentionRow("warning", "列表过期", `${set.name} 的 ${source.file} 已 ${Math.floor(days)} 天没有更新。`));
      }
    }
  }
  if (data.status.overload_dropped > 0) {
    items.push(attentionRow("danger", "过载", `丢弃了 ${fmt.int(data.status.overload_dropped)} 条查询或连接，已达到并发上限。`));
  }
  if (data.status.write_errors > 0) {
    items.push(attentionRow("warning", "写回失败", `${fmt.int(data.status.write_errors)} 次应答已生成但没有写回客户端。`));
  }
  $("attention").replaceChildren(...(items.length ? items : [h("p", { class: "empty-note" }, "没有需要处理的事项。")]));
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

function renderKPIs(data) {
  const status = data.status;
  const up = upstreamTotals(data);
  const first = view.samples[0];
  const last = view.samples[view.samples.length - 1];
  const span = last.at - first.at;

  setValue("kpi-queries", fmt.int(status.queries));
  $("kpi-queries-context").textContent = `自 ${fmt.dateTime(new Date(status.started_at))} 启动以来` +
    (status.refused_locally + status.malformed > 0 ? `，另有 ${fmt.int(status.refused_locally + status.malformed)} 条无效查询在本地拒绝` : "");

  if (span >= 1) {
    setValue("kpi-qps", oneDecimal.format((last.queries - first.queries) / span));
    $("kpi-qps-context").textContent = `近 ${Math.round(span)} 秒平均，正在处理 ${fmt.int(status.inflight)} 条`;
  }

  const msText = (ms) => (ms < 10 ? twoDecimals : oneDecimal).format(ms);
  const recent = last.succeeded - first.succeeded;
  if (span >= 1 && recent > 0) {
    setValue("kpi-latency", msText((last.seconds - first.seconds) / recent * 1000), "ms");
    $("kpi-latency-context").textContent = `近 ${Math.round(span)} 秒内 ${fmt.int(recent)} 次成功的上游交换`;
  } else if (up.succeeded > 0) {
    setValue("kpi-latency", msText(up.seconds / up.succeeded * 1000), "ms");
    $("kpi-latency-context").textContent = "启动以来的平均值，最近没有新的上游交换";
  } else {
    setValue("kpi-latency", "—");
    $("kpi-latency-context").textContent = "还没有转发过查询";
  }

  setValue("kpi-errors", fmt.int(up.errors), "次");
  $("kpi-errors-context").textContent = up.requests > 0
    ? `启动以来 ${fmt.int(up.requests)} 次上游请求，失败率 ${fmt.percent(up.errors / up.requests)}`
    : "还没有上游请求";
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
  select.replaceChildren(button, h("option", { value: "" }, "全部规则"), ...names.map((name) => h("option", { value: name }, name)));
  select.value = names.includes(wanted) ? wanted : "";
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
      $("attention").replaceChildren(h("p", { class: "empty-note" }, text));
    }
    return;
  }
  renderStatus(data.status);
  renderRuntime(data.status);
  renderRcodes(data.status.rcodes);
  renderAttention(data);
  renderKPIs(data);
  rememberRules(data);
  renderRules(data);
  renderOverviewRules(data);
  renderUpstreams(data);
  renderSets(data);
}

async function refreshState() {
  try {
    const data = await api("/api/v1/state");
    view.data = data;
    view.connectionError = "";
    pushSample(data);
  } catch (error) {
    view.connectionError = error.message;
  }
  render();
}

// ── Recent queries ───────────────────────────────────────────────────────────
function queryRow(record, compact) {
  const at = new Date(record.time);
  const how = record.set ? `集合 ${record.set}，${KIND_LABEL[record.match] || record.match}` : "未命中任何集合";
  if (compact) {
    return h("tr", null,
      h("td", { class: "time", title: fmt.full(at) }, fmt.clock(at)),
      h("td", { class: "name" }, h("code", null, record.name || ".")),
      h("td", null, ruleBadge(record.rule)),
      h("td", null, h("div", { class: "chips" }, rcodeBadge(record.rcode), record.truncated ? badge("已截断", "", "outline") : null)),
      h("td", { class: "num" }, fmt.ms(record.duration_ms)));
  }
  return h("tr", null,
    h("td", { class: "time", title: fmt.full(at) }, fmt.clock(at)),
    h("td", { class: "name" }, h("code", null, record.name || ".")),
    h("td", null, record.type),
    h("td", null, ruleBadge(record.rule)),
    h("td", { class: "nowrap", title: how }, h("code", null, record.target)),
    h("td", null, h("div", { class: "chips" }, rcodeBadge(record.rcode), record.truncated ? badge("已截断", "", "outline") : null)),
    h("td", { class: "num" }, fmt.ms(record.duration_ms)));
}

async function refreshRecent() {
  const query = new URLSearchParams({ limit: RECENT_LIMIT });
  const q = $("f-q").value.trim();
  const rule = $("f-rule").value;
  if (q) query.set("q", q);
  if (rule) query.set("rule", rule);
  let data;
  try {
    data = await api(`/api/v1/queries?${query}`);
  } catch (error) {
    if (!$("recent").dataset.loaded) {
      fillEmpty("recent", "暂时读不到数据。");
      fillEmpty("overview-recent", "暂时读不到数据。");
    }
    return;
  }
  $("recent").dataset.loaded = "true";
  const rows = data.queries.map((record) => queryRow(record, false));
  const preview = data.queries.slice(0, PREVIEW_LIMIT).map((record) => queryRow(record, true));
  if (!rows.length) {
    const empty = q || rule
      ? "没有符合筛选条件的查询。清空筛选条件可以看到全部。"
      : "还没有查询经过分流器。在「路由诊断」里做一次实际解析，或等客户端发起查询。";
    rows.push(emptyRow(7, empty));
    preview.push(emptyRow(5, q || rule ? "没有符合筛选条件的查询。" : "还没有查询经过分流器。"));
  }
  $("recent").tBodies[0].replaceChildren(...rows);
  $("overview-recent").tBodies[0].replaceChildren(...preview);
  $("recent-note").textContent = `只在内存里保留最近 ${fmt.int(data.capacity)} 条，这里显示最新的 ${RECENT_LIMIT} 条。AdGuard 缓存命中的查询不会到达分流器。`;
  const filtered = Boolean(q || rule);
  const note = $("overview-recent-note");
  note.textContent = filtered ? "已按最近查询页的筛选条件显示。" : "";
  note.hidden = !filtered;
}

function setPaused(paused) {
  view.paused = paused;
  $("f-pause").setAttribute("aria-pressed", String(paused));
  $("f-pause-label").textContent = paused ? "继续刷新" : "暂停刷新";
  $("f-pause-icon").setAttribute("href", paused ? "#i-play" : "#i-pause");
  if (!paused) refreshRecent();
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

async function diagnose(resolve) {
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
  buttons.forEach((button) => { button.disabled = true; });
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
    buttons.forEach((button) => { button.disabled = false; });
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
  button.disabled = true;
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
    button.disabled = false;
    button.removeAttribute("aria-busy");
    $("reload-label").textContent = "重新加载配置";
  }
  await refreshState();
}

function applyThemeControl() {
  const dark = document.documentElement.dataset.theme === "dark";
  $("theme-icon").setAttribute("href", dark ? "#i-moon" : "#i-sun");
  $("theme").setAttribute("aria-label", dark ? "切换为浅色" : "切换为深色");
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
    if (next === "overview" || next === "queries") refreshRecent();
    if (next === "diagnose" && $("d-name").value.trim()) diagnose(false);
    else $("d-result").hidden = true;
  });

  $("diagnose").addEventListener("submit", (event) => { event.preventDefault(); diagnose(false); });
  $("d-resolve").addEventListener("click", () => diagnose(true));
  $("d-name").addEventListener("input", () => showDiagnoseError(""));

  let debounce = 0;
  $("f-q").addEventListener("input", () => {
    clearTimeout(debounce);
    debounce = setTimeout(() => { syncURL(); refreshRecent(); }, 250);
  });
  $("f-rule").addEventListener("change", () => { syncURL(); refreshRecent(); });
  $("filters").addEventListener("submit", (event) => { event.preventDefault(); syncURL(); refreshRecent(); });
  $("f-pause").addEventListener("click", () => setPaused(!view.paused));

  // Filters and diagnosis input are stored in the URL so that the view can be
  // bookmarked or shared. A name without an explicit view opens the diagnosis.
  restoreFromLocation();

  refreshState().then(() => {
    refreshRecent();
    if ($("d-name").value && currentView === "diagnose") diagnose(false);
  });
  setInterval(() => { if (!document.hidden) refreshState(); }, STATE_INTERVAL);
  setInterval(() => {
    if (document.hidden || view.paused) return;
    if (currentView === "overview" || currentView === "queries") refreshRecent();
  }, RECENT_INTERVAL);
  document.addEventListener("visibilitychange", () => {
    if (document.hidden) return;
    refreshState();
    if (!view.paused) refreshRecent();
  });
}

start();
