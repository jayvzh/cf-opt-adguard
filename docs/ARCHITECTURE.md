# cf-opt-adguard 技术架构与运行机制（ARCHITECTURE.md）

> 版本：v0.1 ｜ 状态：设计稿（项目尚未开工；本文源自 PRD 与对 AGH 官方源码 / 参考仓库的取证，实现后必须以代码核实并回填实际差异）
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
[ipselector 优选 IP] domain 解析 / CloudflareSpeedTest 结果 / 静态列表
      │
      ▼
[planner 计划器] AGH 现有 rewrite + 本地状态 → add/update/remove 计划（纯函数）
      │
      ▼
[syncer 同步器] 唯一写侧：调用 AGH Rewrite API（dry-run 时只打印不执行）
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

## 4. 各阶段机制

### 4.1 collector（采集）

- 分页拉取 `GET /control/querylog`，直到覆盖目标时间窗口或无更多数据；P0 只使用新版参数集（`limit` / `older_than` 或 `offset` / `reason`，D11），开工时以用户 `config.yaml` 指向的实例实测固化；不实现旧版 `startTime/endTime` 兼容（版本差异记录见 [pitfalls.md](pitfalls.md) 第 1 条）。
- 只保留：A / AAAA 查询、正常放行响应（reason 白名单：`NotFilteredNotFound` / `NotFilteredAllowList`；旧版按 `response_status=processed` 判定）。
- 支持客户端白名单 / 黑名单（按 client IP / 名字匹配）。
- 排除项：本地域名与内网反向域（`.lan` / `.local` / `.home.arpa` / `.arpa` / 无点单标签）、私网 `in-addr.arpa` / `ip6.arpa` 反向查询、被过滤 / 广告拦截条目、配置的已知非 CF 域名黑名单。
- 分页拉取必须有上限与超时（窗口 30d + 大日志量时不能无限拉）。

### 4.2 aggregate（聚合，纯函数）

- 归一化：转小写、去尾点；IDN 统一转 punycode 存储（展示时还原 Unicode）。
- 两级粒度：
  - **精确域名**（默认）：`assets.example.com` 独立计数；
  - **可注册域**（需显式开启）：借助公共后缀表归并到 `example.com`；公共后缀表数据作为静态资源随程序分发。
- 每条记录维护：查询次数、首次出现、最后出现（窗口内）。
- 阈值入候选（默认值见 [PRD.md](PRD.md) §6）：24h ≥ 20 次或 7d ≥ 50 次。阈值为配置项，不允许散落在代码中（集中在 config 默认值）。

### 4.3 detector（Cloudflare 探测）

对每个候选域名采集三类信号：

1. **CNAME 链**：用独立 resolver 追完整 CNAME 链，命中 `cloudflare.net` / `cloudflare.com` / `cdn.cloudflare.net` 后缀。
2. **IP 段**：最终 A / AAAA 是否落在 Cloudflare 官方 CIDR 内。CIDR 列表启动时从官方端点拉取并本地缓存（端点见 [API.md](API.md) §5），缓存文件可离线复用，拉取失败时用缓存继续。
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
- 并发受控（可配上限，默认保守值）、单请求超时、失败不拖垮整批；探测信号逐项落状态库，保证结论可追溯。

### 4.4 ipselector（优选 IP）

三种来源（配置择一，默认 A）：

- **A 优选域名**：用独立 resolver 解析（如 `cfip.yyyyt.top`）得到 IP 列表；多 IP 时默认取延迟最低，可配轮询。
- **B CloudflareSpeedTest**：调用外部二进制测速后读取其结果文件（文件格式见 [API.md](API.md) §6）；本工具不重新实现测速。
- **C 静态列表**：读取用户维护的 IP 文件，每行一个 IPv4 / IPv6。

要求：

- 明确区分 IPv4 / IPv6，按配置决定下发哪一族（默认两族都下，AGH rewrite answer 单条只放一个地址）。
- **优选 IP 结果为空时中止本次写入**，保留 AGH 现有规则不动（见 §7）。

### 4.5 planner（计划生成，纯函数）

- 先拉 `GET /control/rewrite/list` 获取 AGH 现状。
- 托管归属判定：只以"本工具状态库中存在且处于 active/pending 的域名集合"为准；**集合之外的用户手工 rewrite 绝不触碰**。
- 对目标集合计算三类操作：
  - **add**：confirmed 且 AGH 中不存在；
  - **update**：AGH 中存在但 answer 与当前优选 IP 不同；
  - **remove**：状态转为 removed（TTL 到期）或本次重新判定为非 CF 的托管条目。
- 计划是一份不可变数据结构，默认 dry 模式打印与 `--apply` 后的 live 执行消费同一份计划。

### 4.6 syncer（同步，唯一写侧）

- 全工程只有本包允许调用 AGH rewrite 写端点（add / update / delete）。
- 仅在 `--apply`（live）模式执行；dry 模式下本阶段空转。逐条执行：限速、指数退避重试（可配次数）；单条最终失败记录到状态库并继续下一条，不中断整批。
- 严格幂等：执行前以 list 现状为准；add 已存在则降级为 update 判断；delete 已不存在视为成功。
- 禁止"全量删除 + 全量添加"。

### 4.7 verifier（验证）

- 同步完成后向 AGH（指定 AGH 为 server）发起 DNS 查询，比对应答是否等于目标 answer。
- 失败条目标记 `last_error` 并计入运行汇总；自动回滚为 P1（MVP 至少保证 dry-run 可预览、失败可见）。

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
| 优选 IP 变化 | 重新解析后批量 update 受影响条目 |
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
