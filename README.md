# splitter

DNS 分流服务。部署在 AdGuard Home 这类前置解析器之后，按域名规则把每条查询发往指定的上游组，
并把应答过的每条查询写入查询日志。缓存、过滤和按设备统计由前置解析器负责。

```text
客户端 → 前置解析器（过滤 / 缓存）→ splitter（回环地址）
                                      ├─ 本地域规则    → 本地应答
                                      ├─ 命中域名集合  → 上游组 A
                                      └─ 未命中        → 上游组 B（默认）
```

- **规则匹配**：支持 `full:`、`domain:`、`keyword:`、`regexp:` 和 `geosite.dat`，自上而下求值，第一条命中的规则生效。
- **上游回退**：组内服务器按顺序尝试，超时、网络错误或 REFUSED 时换下一台；连续失败 3 次的服务器 30 秒内排到最后。
- **逐字节透传**：转发的应答只恢复 DNS ID，EDNS、DNSSEC 和未知记录类型原样返回。
- **原子热加载**：新配置和列表全部校验通过才替换，失败时继续用旧配置。
- **查询日志**：每条查询以 JSON Lines 写入磁盘，写入不阻塞应答；控制面板可以分页回看。
- **可观测性页**：可选接入 Prometheus，在控制台内按 Grafana 的口径查看查询速率、RCODE、耗时 P95、上游与运行保护。
- **安全默认值**：只允许监听回环地址，上游只接受 IP 字面量；静态二进制，distroless 镜像以非 root 用户运行。

## 快速开始

```bash
# 容器（linux/amd64、linux/arm64）
docker run --rm --network host \
  -v /path/to/config:/etc/splitter:ro \
  -v splitter-logs:/var/log/splitter \
  ghcr.io/starriv/splitter:1

# 或二进制：go install，或从 GitHub Releases 下载（附 sha256sums.txt）
go install github.com/starriv/splitter/cmd/splitter@latest
splitter -check -config config.yaml    # 校验配置和全部列表后退出
splitter -config config.yaml           # 启动
```

控制面板在 `http://127.0.0.1:9198/`。监听地址只能是回环地址，所以容器必须使用宿主网络，或与前置解析器共享网络命名空间。

| 命令 / 信号 | 作用 |
| --- | --- |
| `-check` | 校验配置并加载全部列表，打印规则摘要 |
| `-healthcheck` | 请求 `/readyz`，再向每个 DNS 监听地址发一条探测查询（容器 HEALTHCHECK 使用） |
| `-version` | 打印版本号和提交号 |
| `SIGHUP` | 热加载配置和列表 |
| `SIGTERM` / `SIGINT` | 优雅退出，先处理完在途查询 |

## 配置

完整示例见 [`config.example.yaml`](config.example.yaml)，默认路径 `/etc/splitter/config.yaml`。

| 字段 | 说明 |
| --- | --- |
| `listen` / `admin` | DNS 和管理端点的监听地址，只允许回环地址 |
| `admin_hosts` | 经反向代理访问管理端点时使用的主机名 |
| `log` | 日志级别、日志目录和查询日志（见下文） |
| `limits` | 并发查询数、TCP 连接数、空闲超时、重载时列表的最大收缩比例 |
| `observability` | 可选。配置 Prometheus 地址后启用「可观测性」页，见下文 |
| `upstreams` | 上游组：服务器（IP 字面量）和单次尝试超时 |
| `sets` | 域名集合：内联规则、列表文件或 geosite 选择器 |
| `rules` | 有序规则：匹配的集合和查询类型，动作为 `forward`、`answer` 或 `reject` 之一；最后一条必须匹配全部查询 |

配置中的所有错误一次报告完。未被任何规则引用的集合或上游也算错误。

### 规则语法

| 写法 | 匹配 |
| --- | --- |
| `full:example.com` | 只匹配这个域名 |
| `domain:example.com` | 这个域名及其子域，按标签边界匹配（`xbaidu.com` 不匹配 `baidu.com`） |
| `keyword:example` | 包含该子串 |
| `regexp:^ex.*\.com$` | RE2 正则 |

列表文件兼容 domain-list-community 写法：属性标签（`@cn`）会被忽略，不支持 `include:`。`full` 和 `domain` 是哈希查找，`keyword` 和 `regexp` 是逐条扫描，大列表应尽量用前两种。geosite 选择器支持 `tag`、`tag@attr`、`tag@-attr`。

### 查询日志

```yaml
log:
  dir: /var/log/splitter   # 默认值；相对路径按配置文件所在目录解析
  query_log:
    enabled: true          # 默认开启
    max_size_mb: 4096      # 所有日志文件的总上限
    max_age: 720h          # 保留 30 天
```

- **记什么**：splitter 应答过的每条查询，包括在规则求值前就被本地拒绝的无效查询，这类查询的规则记为 `_invalid`。不记的有三类：AdGuard 缓存命中的查询（到不了 splitter）、无法解析或不回复的报文，以及 `-healthcheck` 的探测查询。记录里没有客户端地址。
- **存储**：`queries-<UTC 时间>.jsonl`。单个文件写满 16 MiB 或写满 24 小时后换新文件，超过总上限或保留时长的旧文件自动删除。
- **目录不可写**：查询日志开启时，`log.dir` 无法创建或不可写，进程拒绝启动。直接运行二进制时，把 `log.dir` 指向可写目录，或者关闭查询日志。
- **写入方式**：写入走后台队列，DNS 应答不等磁盘。磁盘跟不上或写入失败时记录会丢失，并计入指标、在控制面板提示。
- **关闭后**：`enabled: false` 时只在内存里保留最近 512 条。
- **生效时机**：`log.dir` 和 `log.query_log` 改动需要重启才生效。

### 热加载

发送 `SIGHUP` 或调用 `POST /api/v1/reload` 触发。加载失败时继续用旧配置，`/readyz` 返回 503。如果某个不少于 100 条的集合一次缩水超过 `limits.max_set_shrink`（默认 50%），这次加载会被拒绝，防止残缺的列表悄悄改变分流。启动时不做这项检查，所以部署前先跑 `-check`。

## 管理接口

默认 `127.0.0.1:9198`，没有登录机制，靠网络边界控制访问。所有请求都校验 `Host` 头，只接受 IP、`localhost` 和 `admin_hosts` 里的主机名，防止 DNS rebinding。两个 POST 接口要求 JSON 请求体，并拒绝跨站请求。

| 路径 | 说明 |
| --- | --- |
| `/` | 控制面板 |
| `/metrics` | Prometheus 指标 |
| `/healthz`、`/readyz` | 存活检查；就绪检查在最近一次重载被拒绝时返回 503 |
| `GET /api/v1/state` | 运行状态、规则、上游、集合和查询日志的计数 |
| `GET /api/v1/queries?limit=&q=&rule=&cursor=` | 查询记录，最新的在前。`q` 按域名子串筛选，`rule` 按规则名筛选；把响应里的 `next` 作为 `cursor` 传回即可往前翻页 |
| `GET /api/v1/route?name=&type=` | 查看路由结果，不发送查询（旧路径 `/debug/route` 也可用） |
| `POST /api/v1/resolve` | `{"name":"…","type":"A"}`，走完整处理路径实际解析一次 |
| `POST /api/v1/reload` | 等同于 `SIGHUP`；新配置被拒绝时返回 422 |
| `GET /api/v1/observability?range=6h` | 「可观测性」页的数据：从 Prometheus 读回的 splitter 指标面板。未配置时返回 `configured: false` |

### 可观测性页

控制台里的「可观测性」页把 Grafana「DNS 解析与分流」面板中与 splitter 有关的部分搬进控制台：
抓取与配置健康、累计/区间查询量、按规则与动作的速率、RCODE 分布、处理耗时 P95、
按规则和按服务器的明细、上游 RTT / 失败 / 重传 / 丢弃、热加载与集合、过载与保护。

历史数据只有 Prometheus 有，所以这一页由 splitter 服务端转查 Prometheus：浏览器只访问控制台的
同源 API，CSP 不需要放宽，Prometheus 也不必暴露给浏览器。功能默认关闭。

```yaml
observability:
  prometheus_url: https://prometheus.example   # 省略则关闭本页，不发出任何请求
  job: splitter                                # 抓取本实例的 job 名
  dashboard_url: https://grafana.example/d/... # 可选：「在 Grafana 中打开」
  timeout: 8s                                  # 单次 Prometheus 请求超时（1s–60s）
```

- **前提**：Prometheus 已抓取本实例的 `/metrics`（job 与 `job` 一致），且保留期覆盖所选范围。按钮最长 7 天，API 的 `range` 最长 30 天。
- **范围与刷新**：可选 1 小时到 7 天；每 30 秒自动刷新，可暂停。范围保存在 URL 的 `range` 参数里（例如 `?view=observability&range=3d`）。
- **地址校验**：只接受 http/https，不能内嵌账号密码，也不能带 query 或 fragment；`dashboard_url` 允许 Grafana 常见的 `?from=…&to=…`。
- **多实例**：同时抓取多个 splitter 时按 Grafana 的口径聚合（`sum`/`min`）。上游服务器地址取 Prometheus 的 `exported_server` 标签；抓取任务没有覆盖 `server` 标签时自动回退到 `server`。
- **未配置或 Prometheus 不可达**：页面显示启用说明或错误与地址，不清空其余页面；DNS 服务不受影响。

### 告警建议

所有指标以 `splitter_` 为前缀，含义见 `/metrics` 的 HELP 文本。标签都是有限集合，不包含查询域名。

| 指标 | 关注点 |
| --- | --- |
| `config_last_reload_successful` | 为 0 表示新配置被拒绝 |
| `upstream_errors_total{reason}`、`upstream_penalized` | 上游失败和被临时排到最后的服务器 |
| `set_source_modified_timestamp_seconds` | 列表多久没更新 |
| `overload_dropped_total` | 达到并发上限后丢弃的查询 |
| `query_log_dropped_total`、`query_log_write_errors_total` | 没能写入查询日志的记录 |
| `panics_total` | 非零即缺陷 |

## DNS 行为

- 本地域返回配置的 A/AAAA 记录并设置 AA 位，其他类型返回 NODATA，查询不会转发出去。
- 上游返回的 SERVFAIL 原样透传，不换服务器；组内服务器全部失败时返回 SERVFAIL。
- 非 IN 类查询返回 REFUSED，EDNS 版本不为 0 返回 BADVERS，非 QUERY 操作码返回 NOTIMP。带 QR 位的报文和畸形报文一律不回复。
- 发往上游的 UDP 查询通告 1232 字节的 EDNS 载荷；等到超时的一半仍无应答时重发一次。客户端用 TCP 时，上游也用 TCP；TC 位原样返回。

## 开发

```bash
make test          # gofmt、race 测试、vet、示例配置校验
make bench         # 基准测试
make build         # 生成 bin/splitter
```

本地打开控制面板：

```bash
tmp=$(mktemp -d) && cp config.example.yaml testdata/cn-domains.txt "$tmp"/
mv "$tmp/config.example.yaml" "$tmp/config.yaml"
sed -i.bak "s|dir: /var/log/splitter|dir: $tmp/logs|" "$tmp/config.yaml"
go run ./cmd/splitter -config "$tmp/config.yaml"
```

依赖完整数据的测试需要设置环境变量：`SPLITTER_DOMAIN_LIST`、`SPLITTER_GEOSITE_DAT`、`SPLITTER_GEOSITE_LIST`。

## 发布

推送 `v*` 标签会触发 `release.yml`：构建四个平台的二进制并发布到 GitHub Releases，同时把多架构镜像推送到 `ghcr.io/starriv/splitter` 和 `docker.io/starriv/splitter`。

- 镜像标签有 `1.0.0`、`1.0`、`1`、`latest`。预发布标签（如 `v1.1.0-rc.1`）只推送完整版本号。
- 推送 Docker Hub 需要配置 secret `DOCKERHUB_USERNAME` 和 `DOCKERHUB_TOKEN`，没配置时只推 ghcr.io。仓库名可以用仓库变量 `DOCKERHUB_REPOSITORY` 覆盖。

## 许可证

[GPL-3.0](LICENSE)。内嵌字体 Geist、Geist Mono、Inter 以 SIL OFL 1.1 发布，许可证见 `internal/webui/static/assets/fonts/OFL.txt`。
