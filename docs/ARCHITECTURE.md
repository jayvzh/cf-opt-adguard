# cf-opt-adguard 技术架构与运行机制（ARCHITECTURE.md）

> 版本：v0.3 ｜ 状态：M1–M6 全部实现（collector → aggregate → detector → ipselector → planner → syncer → verifier → pipeline dry-run / `--apply` live 全链路落地，本文已与代码核实对齐）。
> 本文只回答：怎么跑——分层依赖、流水线机制、探测打分、同步与调度、失败处理。接口字段归 [API.md](API.md)，表结构归 [DATA_MODEL.md](DATA_MODEL.md)，目录落位归 [PROJECT_STRUCTURE.md](PROJECT_STRUCTURE.md)。
> 相关：上游 [PRD.md](PRD.md)；选型理由 [decisions.md](decisions.md)；已知坑 [pitfalls.md](pitfalls.md)。
> 更新时机：流水线阶段、核心机制、技术选型、同步 / 调度 / 失败策略变化时。

---

## 1. 定位：外部编排器

本工具是独立 CLI 进程，对 AGH 只做 API 调用，不修改 AGH、不作为 AGH 插件运行。一次运行就是一条单向流水线：

```text
AGH Query Log API
      │
      ▼
[collector 采集器] 分页拉取 + 条件过滤
      │
      ▼
[aggregate 聚合器] 归一化 / 频次统计 / 阈值过滤（纯函数）
      │
      ▼
候选域名列表
      │
      ▼
[detector 探测器] 独立 DNS + CF 官方 CIDR + HTTP 头 → 加权打分（打分纯函数）
      │
      ▼
confirmed_cf 域名列表（maybe / not 仅落状态库）
      │
      ▼
[ipselector 优选 IP] CloudflareSpeedTest 结果文件（默认）/ domain 解析 / 静态列表
      │
      ▼
[planner 计划器] AGH 现有 rewrite + 本地状态 → add/update/remove 计划（纯函数）
      │
      ▼
[syncer 同步器] 唯一写侧：调用 AGH Rewrite API（仅 --apply live 模式执行；dry-run 空转）
      │
      ▼
[verifier 验证器] 经 AGH 回查解析结果，标记失败条目
      │
      ▼
[state 状态库] SQLite：域名 / 探测信号 / 重写 / 运行记录
```

## 2. 技术选型（计划）

| 层 | 选型 | 理由 |
| --- | --- | --- |
| 语言 / 分发 | Go（本机 go1.27.1；go.mod 版本首个里程碑声明） | 单二进制、部署简单、网络库强、易调用外部命令 |
| DNS 解析 | 独立 resolver（计划 `miekg/dns`），UDP/DoH 可配 | 必须绕开本机 AGH，避免重写结果自我印证 |
| HTTP | 标准库 `net/http`（可配超时、UA、代理） | 探测与 AGH 客户端都只需基础能力 |
| 状态存储 | SQLite（驱动选型首个里程碑定，优先纯 Go 驱动免 CGO） | 零运维、单文件、备份简单 |
| 对外接口 | CLI（无 HTTP 服务、无 UI） | 适配 cron / systemd timer；P1 加内置调度，P2 才考虑 Web UI |
| 日志 | 标准库 `log/slog` 结构化日志 | 级别 / 文件输出、便于审计 |

## 3. 运行形态

- **单次运行**：`cf-opt-adguard run` 跑完流水线即退出，由外部 cron / systemd timer 周期触发（P0 推荐形态）。
- **两种模式**：默认 `dry`（只打印计划）；显式 `--apply` 才进入 `live` 执行写入（D10）。两种模式走完全相同的计划管线，差异仅在 syncer 是否执行写调用。
- 内置定时调度属于 P1；在此之前不允许在进程内常驻循环。
- **部署约定（默认来源 B，D17）**：release 单二进制放入 CloudflareSpeedTest 发布目录（与 `cfst` 可执行文件、`result.csv` 同级）执行，默认直接读取该目录下用户自行测速产出的 `result.csv`；MVP 不调用 CFST 二进制测速。

## 4. 各阶段机制

### 4.1 collector（采集）

- 分页拉取 `GET /control/querylog`，直到覆盖目标时间窗口或无更多数据；P0 只使用新版参数集（`limit` / `older_than` 或 `offset` / `reason`，D11），开工时以用户 `config.yaml` 指向的实例实测固化；不实现旧版 `startTime/endTime` 兼容（版本差异记录见 [pitfalls.md](pitfalls.md) 第 1 条）。
- 只保留：A / AAAA 查询、正常放行响应（reason 白名单：`NotFilteredNotFound` / `NotFilteredAllowList`；旧版按 `response_status=processed` 判定）。
- 支持客户端白名单 / 黑名单（按 client IP / 名字匹配）。
- 排除项：本地域名与内网反向域（`.lan` / `.local` / `.home.arpa` / `.arpa` / 无点单标签）、私网 `in-addr.arpa` / `ip6.arpa` 反向查询、被过滤 / 广告拦截条目、配置的已知非 CF 域名黑名单。
- 分页拉取必须有上限与超时（窗口 30d + 大日志量时不能无限拉）。

### 4.2 aggregate（聚合，纯函数）

- 归一化：转小写、去尾点；IDN 统一转 punycode 存储（展示时还原 Unicode）。
- host 级频次统计 + **公共后缀表归并**（`golang.org/x/net/publicsuffix`，随依赖分发、离线可用）：每个 host 记录其可注册域（zone），维护查询次数、首次出现、最后出现（窗口内）。
- 阈值入候选（默认值见 [PRD.md](PRD.md) §6）：24h ≥ 20 次或 7d ≥ 50 次。阈值为配置项，不允许散落在代码中（集中在 config 默认值）。
- **目标条目决策（D14，`ResolveTargets` 纯函数）**：探测结论产出后，按可注册域分组逐 zone 混合决策 rewrite 目标：
  1. zone 内仅 1 个达阈 host → 单条精确（证据不足以通配，含"仅裸域套 CDN"场景）；
  2. zone 内 ≥2 个达阈 host 全部 confirmed 且 `sync.wildcard: true` → `zone` + `*.zone` 两条；
  3. 混入 maybe / not_cf（或禁用通配）→ 该 zone 回退逐 confirmed host 精确；
  4. verdict 缺失按 not_cf 保守处理；`maybe` 一律不入目标。
  - `aggregate.mode: exact` 整体退回逐 host 精确（D6 旧行为）；输出按 Domain 字典序稳定排序。

### 4.3 detector（Cloudflare 探测）

**探测对象**：每个 zone 的裸域 + 每个达阈 host（zone 归并决策依赖裸域结论，D14）。

对每个探测对象采集三类信号：

1. **CNAME 链**：用独立 resolver 追完整 CNAME 链，命中 `cloudflare.net` / `cloudflare.com` / `cdn.cloudflare.net` 后缀。
2. **IP 段**：最终 A / AAAA 是否落在 Cloudflare 官方 CIDR 内。CIDR 列表启动时从官方端点拉取并缓存到 `data/cf_ips.json`（7 天新鲜期，端点见 [API.md](API.md) §5），拉取失败降级用缓存，缓存也无则该信号缺失（不单独决定结论）。
3. **HTTP 头**：HEAD（失败降级 GET 只读头部）请求 `https://<domain>`，检查 `cf-ray`、`server: cloudflare`、`cf-cache-status`。

打分规则（权重为业务护栏，改动需同步 PRD）：

| 信号 | 分值 |
| --- | --- |
| CNAME 命中 CF 后缀 | +2 |
| A / AAAA 命中官方 CIDR | +2 |
| 响应含 `cf-ray` | +3 |
| `server: cloudflare` | +1 |

- 总分 ≥ 4 → `confirmed_cf`；有信号但未达线 → `maybe_cf`（只记录不写入）；无信号 → `not_cf`。
- HTTP 探测失败（超时 / 非 2xx / TLS 失败）按"该信号缺失"处理，不直接判死；DNS 解析失败为 `not_cf` 并记录原因。
- 并发受控（默认 8，可配）、单请求超时（默认 3s）、失败不拖垮整批；探测信号逐项落状态库，保证结论可追溯；探测总量受 `aggregate.max_domains` 预算护栏约束（D15）。

### 4.4 ipselector（优选 IP）

三种来源（配置择一，**默认 B**，D17）：

- **B CloudflareSpeedTest（默认）**：读取 CFST 运行后产出的结果文件（默认 `result.csv`，相对于工作目录；release 约定把本工具二进制放入 CFST 发布目录执行，文件格式见 [API.md](API.md) §6）。CSV 已按丢包 / 延迟、下载速度排序，首数据行即最优 IP。**MVP 只解析文件，不调用 / 拉起 `cfst` 二进制测速**——测速由用户自行运行；自动调用测速为 P1（见 [PRD.md](PRD.md) §5.2）。
- **A 优选域名**：用独立 resolver 解析（如 `cfip.yyyyt.top`）得到 IP 列表；多 IP 时默认取延迟最低，可配轮询。
- **C 静态列表**：读取用户维护的 IP 文件，每行一个 IPv4 / IPv6。

要求：

- 明确区分 IPv4 / IPv6，按配置决定下发哪一族（默认仅 `[4]`；v6 需 CFST 另跑 ipv6 测速后显式开启，AGH rewrite answer 单条只放一个地址）。
- **优选 IP 结果为空时中止本次写入**，保留 AGH 现有规则不动（见 §7）。

### 4.5 planner（计划生成，纯函数）

- 先拉 `GET /control/rewrite/list` 获取 AGH 现状。
- **目标条目 = aggregate.ResolveTargets 的粒度决策产物**（§4.2，D14）：通配条目以 `*.` 前缀域名形态（如 `*.example.com`）与精确条目同键参与 diff。
- 托管归属判定：只以"本工具状态库中存在且处于 active/pending 的域名集合"为准（本轮 persist 后新登记的 pending 目标即入集合）；**集合之外的用户手工 rewrite 绝不触碰**。
- 对目标集合计算三类操作：
  - **add**：目标域名在 AGH 中不存在；
  - **update**：目标域名存在但现有答案不含本次优选 IP；
  - **remove**：托管域名在 AGH 中的条目，答案与本次期望不符且属同一 IP 族（异族答案互补共存不清理）；本阶段 remove 只打印不执行。
- 计划是一份不可变数据结构，默认 dry 模式打印与 `--apply` 后的 live 执行消费同一份计划。

### 4.6 syncer（同步，唯一写侧，已实现）

- 全工程只有本包允许调用 AGH rewrite 写端点（add / update / delete）；仅消费 planner 计划，不自产条目。
- 仅在 `--apply`（live）模式执行；dry 模式下本阶段空转。逐条执行：限速（`sync.rate_limit`，默认 200ms）、指数退避重试（`sync.retry`，默认 3 次，401/403 鉴权错误不重试）；单条最终失败记录到状态库并继续下一条，不中断整批。
- 严格幂等：以 list 现状为准（包内维护 current 视图，随每次写成功同步更新，保证批内后续判断准确）；add 已存在则降级为 update 判断；delete 已不存在视为成功。
- 禁止"全量删除 + 全量添加"；v4/v6 异族答案互补共存，update 只替换同族答案。
- 成功后写状态库：`state=active`、`agh_present=true`；`first_synced` 只在首次成功写入时填定，此后不变（D18）。

### 4.7 verifier（验证，已实现）

- 同步完成后向 AGH（作为 DNS server，`host:53`，从 `--agh-url` 主机推导）发起 UDP DNS 查询（复用 miekg/dns），v4 查 A / v6 查 AAAA，比对应答是否等于目标 answer。
- wildcard 条目用测试子域 `cf-opt-verify.<zone>` 回查，不污染真实子域。
- 失败条目标记 `last_error`（保留原状态）并计入运行汇总；自动回滚为 P1（MVP 至少保证 dry-run 可预览、失败可见）。

## 5. 托管状态机

```text
                 首次 confirmed
  (无记录) ───────────────────────▶ active
     ▲                              │ 窗口内未达阈值 / 本次未确认
     │ 重新 confirmed               ▼
     └────────────────────────── pending
                                    │ TTL（默认 30d）到期仍未恢复
                                    ▼
                                 removed ──(syncer 删除 AGH 规则后)──▶ (清除/归档)
```

- `active`：本次确认 CF，AGH 中应有对应 rewrite。
- `pending`：暂不动规则，连续观察；超过 TTL 才进入删除计划。
- `removed`：已生成删除计划；删除成功后记录归档。状态枚举全集见 [DATA_MODEL.md](DATA_MODEL.md)。

## 6. 调度与增量策略

| 项 | 默认 |
| --- | --- |
| 触发方式 | 外部 cron / systemd timer，建议每 1~6 小时 |
| 查询窗口 | 7d（24h / 7d / 30d 可配） |
| 增量依据 | AGH rewrite list 现状 + 本地状态库 |
| 优选 IP 变化 | 读取最新 `result.csv`（默认 B）/ 重新解析优选域名后批量 update 受影响条目 |
| 淘汰 | 30d 未出现：active → pending → remove |

每次运行都是幂等重放：即使上一次中途失败，重跑只会补齐差异，不产生重复规则。

## 7. 失败处理与安全护栏

- **默认 dry-run**：不带 `--apply` 的运行绝不写 AGH；任何新配置首次运行先不带 `--apply` 核对计划（计划含具体 domain → answer 清单），确认后再带 `--apply` 执行。
- **前置失败不写入**：采集窗口为空之外的异常、优选 IP 为空、无法获取 rewrite list，直接中止，不做任何删除动作。
- **局部失败隔离**：单域名探测失败、单条写失败均不阻断其余条目；失败数写入运行汇总与退出码（见 [API.md](API.md) §7）。
- **AGH 限流 / 5xx**：退避重试；连续失败超过阈值时提前结束同步阶段并保留已完成结果。
- **审计**：每次运行落 runs 记录（模式、各阶段计数、失败明细）；通知（Webhook / Telegram / 邮件）为 P1。

## 8. 隐私边界

- 查询日志只在本地读取、本地聚合、本地落盘（SQLite 与日志文件），不向任何第三方发送日志内容。
- 探测器仅对候选域名本身发起必要的 DNS 查询（到用户配置的 resolver）与 HTTP HEAD 请求；HTTP 请求携带常规 UA，不附带 AGH 数据。
- AGH 密码等敏感配置不得写进日志；配置文件不入库（提供 `config.example.yaml`）。
