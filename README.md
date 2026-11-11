# splitter

splitter 是 DNS 查询分流服务，部署在具备缓存、过滤或统计能力的前置解析器之后
（例如 AdGuard Home），根据域名规则集决定每条查询发往哪一个上游组。服务本身不承担缓存、
过滤、按设备统计与持久查询日志；这些职责由前置解析器完成。查询的出口路径由网络层根据目的
地址决定，splitter 不参与出口选择。

```text
客户端 → 前置解析器（过滤 / 缓存 / 日志）→ splitter（回环地址）
                                               ├─ 本地域规则      → 本地应答
                                               ├─ 命中域名集合    → 上游组 A
                                               └─ 未命中          → 上游组 B（默认）
```

splitter 以单个静态二进制或 distroless 容器形式交付，配置与域名列表以只读方式挂载。
服务默认仅监听回环地址，适合与前置解析器部署在同一主机或同一网络命名空间中。

## 核心特性

- **域名规则集匹配**：支持 `full:`、`domain:`、`keyword:`、`regexp:` 四种规则，以及
  v2fly / sing-box 生态的 `geosite.dat` 集合；规则自上而下求值，首条命中即生效。
- **本地域应答**：对指定本地域按配置返回 A 或 AAAA 记录，未配置的类型返回 NODATA，
  查询不会转发至任何上游。
- **上游组与按序回退**：每个上游组可配置多台服务器，按顺序尝试；超时、网络错误或 REFUSED
  触发切换到下一台。连续失败 3 次的服务器在 30 秒内排在健康服务器之后，使后续查询无需先
  经历该服务器的超时。
- **应答逐字节透传**：转发应答除恢复 DNS ID 外不做重新编码，EDNS 选项、DNSSEC 记录与
  未知记录类型均原样返回。
- **原子热加载**：配置与全部域名列表在旁路完成加载和校验后原子替换；校验失败时继续使用
  旧配置，不中断查询。
- **可观测性**：提供 Prometheus 指标、liveness / readiness 端点与可解释的路由诊断接口。
- **只读管理面**：内置 JSON API 与单页控制面板，用于状态查看、最近查询观察和手动重载，
  不支持在运行时修改规则或上游。
- **安全默认值**：监听地址仅允许回环；通配地址、局域网地址与 IPv4-mapped 形式在配置校验
  阶段即被拒绝。
- **单一静态二进制**：无 CGO 依赖，distroless 镜像不含 shell 与包管理器，以非 root 用户运行。

## 设计原则与边界

| 不承担的职责 | 原因 |
| --- | --- |
| 缓存 | 前置解析器已提供缓存。两层缓存会叠加 TTL，改变上游记录的有效期语义。 |
| 重新编码上游应答 | 逐字节透传避免 EDNS、OPT 与未知记录类型在重编码过程中被破坏或丢失。 |
| DoH / DoT 上游 | 上游传输保持明文 DNS，由部署环境的网络层（隧道、VPN 或内网）提供链路保护。 |
| 按客户端分流与 ECS | 服务模型假定单一前置解析器作为客户端；按客户端分流与 ECS 由前置解析器或网络层实现。 |
| 运行时修改规则 | 配置是唯一事实来源，容器内配置目录只读挂载；规则变更通过修改配置并重载完成。 |
| 监听 53 端口或对局域网开放 | 服务定位为前置解析器的内部上游，仅监听回环地址。 |

依赖选择遵循最小化原则：DNS 报文解析、Protobuf 与 YAML 使用 Go 团队或基金会维护的库，
指标使用 Prometheus 官方客户端。除这些库外，运行时不依赖第三方服务。

## 请求处理流程

1. 从 UDP 或 TCP 监听地址接收查询，解析 DNS 头部与问题段。
2. 按规则顺序匹配域名：首条命中的规则决定本地应答或目标上游组。
3. 本地应答规则在本地构造响应（A/AAAA 记录或 NODATA），设置 AA 位，不产生上游流量。
4. 转发规则将查询发送到目标上游组；同组服务器按配置顺序尝试（连续失败的服务器后移），
   每次尝试受上游组超时限制。
5. 校验应答的 DNS ID 与问题段，丢弃不匹配的报文；有效应答恢复 ID 后逐字节返回客户端。
6. 记录规则、上游、耗时与响应码相关指标；最近查询进入固定容量的内存环形缓冲。

规则求值结果可通过 `/debug/route` 或 `GET /api/v1/route` 在不发送查询的情况下查询，
用于人工核对分流方向。

## 规则语法与匹配

规则前缀与 v2fly/domain-list-community 一致。列表文件同时接受 domain-list-community 源文件
的写法：规则后的属性标签（如 `@ads`、`@!cn`）不参与匹配，文件中的全部条目均归入该集合；
`#` 之后的行尾注释被忽略。`include:` 不受支持，加载时报错；被包含的列表应作为集合的另一个
文件引用，需要按属性过滤时应使用 geosite 选择器。

| 规则 | 语义 |
| --- | --- |
| `full:example.com` | 仅精确匹配该域名 |
| `domain:example.com` | 匹配该域名及其所有子域，按标签边界对齐（`xbaidu.com` 不匹配 `baidu.com`） |
| `keyword:example` | 域名包含指定子串 |
| `regexp:^ex.*\.com$` | 对规范化后的域名应用 RE2 正则 |

集合也可以由 `sets.<name>.geosite` 从 geosite.dat 构建，选择器支持 `tag`、`tag@attr` 与
`tag@-attr`，并可按链式组合。

匹配成本按规则类型区分：`full` 与 `domain` 为哈希查找（`domain` 每条域名最多按标签逐级
查找），`keyword` 与 `regexp` 为线性扫描。大型列表应优先使用 `full` 与 `domain`。正则使用
Go 的 RE2 引擎，匹配时间与输入长度线性相关，不受灾难性回溯影响。

规则按配置顺序求值，首条命中生效；最后一条规则必须覆盖所有剩余查询，配置校验会拒绝不满足
该条件的配置。

## 配置

配置文件为 YAML，默认路径 `/etc/splitter/config.yaml`，可通过 `-config` 覆盖。完整示例见
[`config.example.yaml`](config.example.yaml)。

配置由以下部分组成：

| 字段 | 说明 |
| --- | --- |
| `listen` | DNS 监听地址列表，仅允许回环地址 |
| `admin` | 管理端点监听地址，仅允许回环地址 |
| `admin_hosts` | 可选。经反向代理访问管理端点时使用的主机名；IP 字面量与 `localhost` 始终允许 |
| `log` | 日志级别与是否记录查询 |
| `limits` | 并发查询数、TCP 连接数、空闲超时与列表收缩阈值 |
| `upstreams` | 上游组定义：服务器列表（仅允许 IP 字面量）与超时 |
| `sets` | 域名集合定义：规则列表、列表文件或 geosite 选择器 |
| `rules` | 有序规则：名称、匹配集合、动作（`forward`、`answer` 或 `reject`）与可选查询类型限制 |

配置校验在执行时强制以下不变量：

- 上游地址必须为 IP 字面量。使用域名会使解析器依赖自身解析上游，形成引导环路。
- 监听地址必须为回环地址；通配地址、局域网地址与 IPv4-mapped 形式均被拒绝。
- 同一集合内不得重复引用列表文件；未被任何规则引用的集合或上游视为配置错误。
- 最后一条规则必须能够匹配所有剩余查询。
- 所有校验错误一次性汇总返回，而非在首个错误处终止。

### 重载

发送 `SIGHUP` 或调用 `POST /api/v1/reload` 触发重载。新配置与全部列表在独立路径完成加载
与校验，成功后原子替换运行时；任一列表加载失败或校验失败时保留旧配置继续服务，并将
`/readyz` 置为 503、`splitter_reloads_total{result="failure"}` 加一。

重载时若某个集合（原条目数不少于 100）一次性减少超过 `limits.max_set_shrink`（默认 0.5），
重载会被拒绝，以避免不完整的列表使域名在无提示的情况下改道。启动时没有可供比较的旧集合，
该检查不生效，因此列表应先通过 `-check` 校验再部署。

## 安装

以下任一方式均可获得 splitter：

```bash
# Go 1.27 或更高版本
go install github.com/starriv/splitter/cmd/splitter@latest

# 容器镜像（linux/amd64、linux/arm64）
docker pull ghcr.io/starriv/splitter:1
docker pull docker.io/starriv/splitter:1
```

预编译二进制（linux/amd64、linux/arm64、darwin/amd64、darwin/arm64）及 `sha256sums.txt`
见 [GitHub Releases](https://github.com/starriv/splitter/releases)。下载后使用
`sha256sum -c sha256sums.txt` 校验。

## 运行

```bash
# 校验配置与全部列表，打印规则摘要后退出
splitter -check -config /etc/splitter/config.yaml

# 正常启动
splitter -config /etc/splitter/config.yaml

# 探测管理端点与每个 DNS 监听地址（容器 HEALTHCHECK 使用）
splitter -healthcheck -config /etc/splitter/config.yaml

# 打印版本后退出
splitter -version
```

`-version` 输出版本号与提交号。发布的二进制与镜像在构建时注入两者；`go install …@vX.Y.Z`
安装的二进制使用 Go 记录的模块版本；在 git 工作区中直接 `go build` 时使用 Go 记录的版本控制
信息（工作区有未提交修改时带 `dirty` 标记）。

进程信号：

| 信号 | 行为 |
| --- | --- |
| `SIGHUP` | 原子重载配置与列表 |
| `SIGTERM` / `SIGINT` | 优雅退出：停止接收新查询，等待在途查询完成，关闭监听套接字 |

`-healthcheck` 先请求管理端点的 `/readyz`，再向响应中列出的每个已绑定 DNS 地址发送一条
`version.bind CH TXT` 查询。该查询在任何合法配置下均由本地返回 REFUSED，不会到达上游。
管理端点与全部 DNS 地址均正常响应时，健康检查才通过。

健康检查仅从配置文件读取 `admin` 地址（可由 `-admin ip:port` 覆盖），DNS 地址取自运行中的
进程；尚未重载的配置改动不影响检查结果。容器 HEALTHCHECK 每 30 秒执行一次该探测，相关查询
计入 `splitter_queries_total{rule="_invalid"}`。

## 管理接口

管理端点仅监听回环地址，默认 `127.0.0.1:9198`。

| 路径 | 用途 |
| --- | --- |
| `/metrics` | Prometheus 指标 |
| `/healthz` | liveness：始终返回 200，并包含加载时间、集合条目数、列表修改时间与最近一次重载结果 |
| `/readyz` | readiness：最近一次重载被拒绝时返回 503（此时仍以旧配置提供服务） |
| `/debug/route?name=example.com&type=A` | 不发送查询，返回将命中的规则、集合、匹配方式与目标上游组 |

### JSON API

| 路径 | 说明 |
| --- | --- |
| `GET /api/v1/state` | 一次快照：运行状态、规则（按求值顺序，含查询数与耗时）、上游（每台服务器的请求数、失败原因、重传与耗时）、集合（条目数、来源文件、修改时间及引用它的规则）。数值与 `/metrics` 来自同一批采集器。 |
| `GET /api/v1/queries?limit=&q=&rule=` | 最近经过规则求值的查询，按时间倒序；`q` 按域名子串过滤，`rule` 按规则名过滤；`limit` 取值范围 1–512，默认 50。 |
| `GET /api/v1/route?name=&type=` | 与 `/debug/route` 等价，不发送查询。 |
| `POST /api/v1/resolve` | 请求体 `{"name":"…","type":"A"}`，执行完整处理路径并返回路由、RCODE、耗时与应答记录。 |
| `POST /api/v1/reload` | 等价于 `SIGHUP`。成功返回 200；新配置被拒绝时返回 422 及原因，旧配置继续服务。 |

最近查询保存在固定 512 条的内存环形缓冲中，不落盘、重启即清空。该缓冲只包含经过规则
求值的查询：前置解析器缓存命中的查询不会到达 splitter，`-healthcheck` 探测与在本地拒绝的
无效查询不进入该列表。持久查询日志应由前置解析器提供。

所有管理路径校验 `Host` 请求头：仅接受 IP 字面量、`localhost` 与 `admin_hosts` 中列出的
主机名，其余请求返回 421，用于阻止将自身域名解析到 127.0.0.1 的 DNS rebinding 页面访问
管理端点。经反向代理以主机名发布管理端点时，应将该主机名加入 `admin_hosts`，或由代理将
`Host` 改写为回环地址。

两个 POST 接口要求 `Content-Type: application/json`，携带 `Sec-Fetch-Site` 的浏览器请求
必须为 `same-origin`，请求体上限 4 KiB。服务不提供登录机制；访问控制依赖网络边界，管理
端点应仅通过回环地址或受控的反向代理访问。

控制面板由 `internal/webui/static/` 下的静态资源经 `go:embed` 嵌入二进制，运行时无构建
依赖。页面内容全部由 DOM 节点与 `textContent` 构造，不使用 `innerHTML`；响应头包含
`Content-Security-Policy`，禁止内联脚本与样式。

## 指标

| 指标 | 标签 | 说明 |
| --- | --- | --- |
| `splitter_queries_total` | `rule`, `action`, `target` | 每条规则处理并决定的查询数 |
| `splitter_responses_total` | `rcode` | 返回给客户端的 RCODE 分布。无对应助记符的响应码（含扩展位共 4096 种）统一记为 `OTHER` |
| `splitter_request_duration_seconds` | `rule` | 端到端处理耗时直方图 |
| `splitter_upstream_requests_total` | `upstream`, `server` | 每台上游服务器的尝试次数 |
| `splitter_upstream_errors_total` | `upstream`, `server`, `reason` | 上游失败原因：`timeout`、`network`、`mismatch`、`refused`、`canceled`。每次失败尝试均计数，包括随后由下一台上游恢复的查询 |
| `splitter_upstream_duration_seconds` | `upstream`, `server` | 成功上游交换的耗时直方图 |
| `splitter_upstream_dropped_responses_total` | `upstream`, `server` | 因 ID 或问题段不匹配被丢弃的 UDP 应答（迟到、重复或伪造） |
| `splitter_upstream_retransmits_total` | `upstream`, `server` | 超时一半时仍未收到应答而重发的 UDP 查询 |
| `splitter_upstream_penalized` | `upstream`, `server` | 连续失败后被排到健康服务器之后时为 1，否则为 0；重载后归零 |
| `splitter_set_entries` | `set`, `kind` | 每个集合按匹配方式统计的条目数 |
| `splitter_set_source_modified_timestamp_seconds` | `set`, `file` | 列表文件的修改时间，用于判断列表新鲜度 |
| `splitter_reloads_total` | `result` | 重载结果计数 |
| `splitter_last_reload_success_timestamp_seconds` | — | 最近一次成功重载的时间戳 |
| `splitter_config_last_reload_successful` | — | 最近一次配置加载被接受为 1，被拒绝为 0。告警应基于该指标 |
| `splitter_inflight_queries` | — | 正在处理的查询数 |
| `splitter_tcp_connections` | — | 当前打开的客户端 TCP 连接数 |
| `splitter_overload_dropped_total` | `proto` | 超过 `max_inflight` 或 `max_tcp_conns` 被丢弃的查询或连接 |
| `splitter_response_write_errors_total` | `proto` | 已生成但写入客户端失败的应答，例如超过 MTU 的 UDP 报文。此类应答不计入 `responses_total` |
| `splitter_malformed_queries_total` | — | 无法解析的查询数 |
| `splitter_panics_total` | — | 处理查询时发生 panic 的次数。该查询被丢弃，进程继续服务；非零值表示存在缺陷 |

失败类计数器在启动或加载时即以 0 值存在，避免首次失败时序列初值为 1 导致 `increase()`
无法观察到增量。所有指标标签均为有界集合，不包含查询域名。

## DNS 行为约定

- `domain:` 按标签边界匹配自身与所有子域；`full:` 仅匹配自身。
- 本地域查询按配置返回 A/AAAA 地址并设置 AA 位；未配置的类型返回 NODATA；本地域查询不会转发。
- 上游返回 REFUSED 时切换到同组下一台服务器；全部服务器均拒绝时，原样返回最后一个 REFUSED
  应答。上游的 SERVFAIL 视为有效应答并透传（常见原因是 DNSSEC 校验失败），不触发切换。
- 同一服务器连续失败 3 次（超时、网络错误、应答不匹配或 REFUSED）后，30 秒内排在健康服务器
  之后，仅在其他服务器均失败时尝试。惩罚到期后恢复原有顺序：一次成功即清除失败记录，再次
  失败则重新惩罚。
- 上游组内全部服务器失败时返回 SERVFAIL。
- 非 IN 类查询（如 `version.bind CH TXT`）由本地返回 REFUSED，不转发。
- EDNS 版本不为 0 的查询由本地返回 BADVERS（RFC 6891），不转发；非 QUERY 操作码返回 NOTIMP。
- 带 QR 位的报文（应答）不返回任何响应，畸形报文亦不返回 FORMERR，以避免反射与放大风险。
- 出站 UDP 查询通告的 EDNS 载荷大小固定为 1232（DNS Flag Day 2020），避免上游返回必须
  分片的大报文；分片丢失时客户端不会收到 TC 位，只能等待超时。
- UDP 上游交换在超时一半时以相同套接字和 ID 重发一次；上游可能因此收到重复查询。
- 客户端使用 TCP 时上游亦使用 TCP；UDP 应答的 TC 位原样返回，由客户端决定是否改用 TCP。
- 热加载期间查询不中断；空列表、无效列表或被判定为截断的列表会被拒绝，继续使用旧列表。
- TCP 监听支持连接流水线（pipelining）与空闲超时；关闭进程时不会等待客户端长连接空闲。

## 性能与资源

- `full` 与 `domain` 规则使用哈希查找，匹配成本不随列表规模增长；内存占用与条目数线性相关。
- `regexp` 使用 RE2，匹配时间与输入长度线性相关。
- 通过 `limits.max_inflight` 与 `limits.max_tcp_conns` 限制并发；超限查询被丢弃，
  TCP 超限连接立即重置，并计入 `splitter_overload_dropped_total`。
- 服务不缓存应答，内存占用不随查询量增长；最近查询缓冲固定为 512 条。
- 每个在途的 UDP 上游交换占用 4 KiB 接收缓冲；`max_inflight` 上限为 16384，对应缓冲
  内存约 64 MiB。填满缓冲的 UDP 应答按失败处理并切换服务器。

## 测试

| 层次 | 内容 | 位置 |
| --- | --- | --- |
| 单元测试 | 规则解析与匹配、geosite 选择器、报文解析与校验、规则顺序、配置不变量 | 各包 `*_test.go` |
| 服务器测试 | TCP 连接复用、空闲超时、过载丢弃、优雅退出 | `internal/server` |
| 场景测试 | 应答逐字节透传、TC 到 TCP 重试、端口拒绝后切换上游、重载与并发查询 | `internal/app/scenario_test.go` |
| 配置校验 | 示例配置与测试夹具通过 `-check` | `make check-config` |
| 模糊测试 | 报文解析、完整请求路径、规则解析、geosite 解码 | `go test -fuzz …` |
| 真实 geosite | 使用 v2fly 发布的 `dlc.dat` 对比集合构建结果 | `TestRealGeosite`，需 `SPLITTER_GEOSITE_DAT` / `SPLITTER_GEOSITE_LIST` |
| 完整列表等价性 | 对完整域名列表逐条验证匹配语义 | `TestDomainListEquivalence`，需 `SPLITTER_DOMAIN_LIST` |
| geosite 路由等价性 | 同一列表与 geosite `cn@-!cn` 选择器的路由决策一致性 | `TestGeositeRoutingEquivalence`，需 `SPLITTER_GEOSITE_DAT` / `SPLITTER_DOMAIN_LIST` |

## 构建

```bash
make test                  # gofmt 检查、race 测试、vet、示例配置校验
make check-config          # 使用 config.example.yaml 与测试夹具执行 -check
make bench                 # 匹配与请求处理路径基准测试
make build                 # bin/splitter（本机架构）
make build-linux           # bin/splitter-linux-amd64
make build-platform GOOS=darwin GOARCH=arm64   # 任意平台交叉编译
```

本地运行控制面板：

```bash
tmp=$(mktemp -d)
cp config.example.yaml "$tmp/config.yaml"
cp testdata/cn-domains.txt "$tmp/cn-domains.txt"
go run ./cmd/splitter -config "$tmp/config.yaml"
# 浏览器访问 http://127.0.0.1:9198/
```

## 容器与发布

容器镜像基于 distroless 静态运行时，以非 root 用户运行，配置目录只读挂载：

```bash
docker run --rm \
  --network host \
  -v /path/to/config:/etc/splitter:ro \
  ghcr.io/starriv/splitter:1
```

由于监听地址仅限回环，容器必须与前置解析器共享网络命名空间，或使用宿主网络
（`--network host`）。端口映射无法把流量转发到容器内的回环地址。

镜像标签：

| 标签 | 含义 |
| --- | --- |
| `1.0.0` | 指定版本，内容不再变化 |
| `1.0`、`1` | 对应系列的最新版本 |
| `latest` | 最新的正式版本，与该版本的 `1.0.0` 等标签为同一镜像 |

每次正式发布同时推送以上全部标签；发布工作流在推送后校验每个标签与版本标签解析到同一镜像。
预发布版本（如 `v1.1.0-rc.1`）只推送完整版本标签，不更新 `latest`、`1.1` 与 `1`。

`docker pull ghcr.io/starriv/splitter:latest` 即可获得最新正式版本；需要可复现部署时应固定完整
版本号或镜像摘要。镜像同时发布到 `ghcr.io/starriv/splitter` 与
`docker.io/starriv/splitter`，两者内容一致。

## 发布与 CI

| 工作流 | 触发条件 | 内容 |
| --- | --- | --- |
| `.github/workflows/ci.yml` | push 到 main、pull request | 格式检查、`go vet`、`go test -race`、示例配置 `-check`、模糊测试冒烟；构建 linux/amd64 镜像并自检，不发布 |
| `.github/workflows/release.yml` | `v*` 标签 | 完整测试；四平台二进制、`sha256sums.txt` 与 LICENSE 发布到 GitHub Releases；linux/amd64 + linux/arm64 镜像推送到 ghcr.io 与 Docker Hub |

发布新版本：

```bash
git tag -a v1.1.0 -m "splitter 1.1.0"
git push origin v1.1.0
```

版本号遵循[语义化版本](https://semver.org/lang/zh-CN/)；带 `-` 的标签（如 `v1.1.0-rc.1`）
作为预发布版本，不更新 `latest`。ghcr.io 使用工作流自带的 `GITHUB_TOKEN` 推送；Docker Hub
需要在仓库设置中配置 `DOCKERHUB_USERNAME` 与 `DOCKERHUB_TOKEN` 两个 secret，未配置时只发布到
ghcr.io 并在工作流中给出警告。Docker Hub 仓库名默认为 `<owner>/splitter`，可通过仓库变量
`DOCKERHUB_REPOSITORY` 覆盖。

## 目录结构

```text
cmd/splitter/           进程入口：命令行参数、信号处理、-check 与 -healthcheck
internal/
├── domain/             规则解析与匹配引擎，不依赖 DNS
├── geosite/            geosite.dat 解码，仅依赖 domain 与 protobuf
├── wire/               DNS 报文解析、应答校验与本地响应构造
├── upstream/           上游组：UDP/TCP 交换、超时、按序回退、应答校验
├── router/             规则到决策的求值，首条命中且可解释
├── config/             YAML 加载与配置校验
├── metrics/            Prometheus 指标
├── server/             UDP/TCP 监听、并发限制、优雅退出
├── app/                运行时装配、热加载、管理端点、JSON API、最近查询
├── webui/              控制面板静态资源，嵌入二进制
└── dnstest/            测试用上游服务器与客户端
.github/workflows/      CI 与发布工作流
config.example.yaml     示例配置
testdata/               测试夹具
Dockerfile              容器镜像定义
Makefile                构建与测试入口
```

依赖方向为单向：`domain` ← `geosite` / `router` / `wire` ← `upstream` ← `app` → `server`。
`server` 仅依赖 `Handler` 接口，`upstream` 仅依赖 `Observer` 接口；`metrics` 实现这些接口。

## 设计参考

| 来源 | 参考内容 |
| --- | --- |
| v2fly / sing-box | `full:`、`domain:`、`keyword:`、`regexp:` 规则语法与 `geosite.dat` 格式 |
| unbound / RFC 5452 | 每次交换使用新套接字与随机 ID；丢弃 ID 或问题段不匹配的应答 |
| unbound | UDP 查询在超时一半时重发一次 |
| AdGuard dnsproxy | 传输协议跟随客户端；TC 位原样返回 |
| CoreDNS | 规则首条命中；`-check` 校验后退出；`SIGHUP` 原子重载 |
| Prometheus | 有界标签集合，不对查询域名打标签 |

## 许可证

splitter 以 [GNU General Public License v3.0](LICENSE) 发布。控制面板内嵌的 Geist、Geist Mono
与 Inter 字体以 SIL Open Font License 1.1 发布，许可证文本见
`internal/webui/static/assets/fonts/OFL.txt`。
