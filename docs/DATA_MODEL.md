# cf-opt-adguard 数据模型与配置项（DATA_MODEL.md）

> 版本：v0.2 ｜ 状态：migration `0001_init.sql` 已实现定稿（本文已与代码核实对齐）；本文即 schema 权威来源。
> 本文只回答：存什么——SQLite 表结构、状态枚举、配置项全集。接口 HTTP 细节归 [API.md](API.md)，状态流转归 [ARCHITECTURE.md](ARCHITECTURE.md)。
> 相关：[ARCHITECTURE.md](ARCHITECTURE.md)、[conventions.md](conventions.md)。
> 更新时机：表结构 / 迁移 / 配置键或默认值变化时（与代码同一变更交付）。

---

## 1. 设计原则

- SQLite 单文件库（默认 `./data/state.db`，路径可配），只存：域名统计、探测信号、重写托管状态、运行记录。
- 驱动使用纯 Go 的 `modernc.org/sqlite`（免 CGO，D12）；迁移与查询代码基于标准 `database/sql`，不绑定驱动专有 API。
- **查询日志原文不入库**：只存聚合后的域名级统计与探测结论，控制隐私面与库体积。
- 域名一律以**小写 punycode**形态存储；展示时再转 Unicode。
- 时间一律 UTC、RFC3339 字符串存储（与 AGH `older_than` 参数格式兼容）。

## 2. 表结构（已实现：`internal/state/migrations/0001_init.sql`）

### 2.1 domains（域名聚合记录）

```sql
CREATE TABLE domains (
    id           INTEGER PRIMARY KEY,
    domain       TEXT NOT NULL UNIQUE,      -- 精确域名，punycode 小写
    zone         TEXT,                      -- 可注册域（zone 模式聚合时填充，D14 默认填充）
    hit_count    INTEGER NOT NULL DEFAULT 0,-- 最近窗口内命中次数（每次运行重算）
    first_seen   TEXT NOT NULL,             -- 首次出现 RFC3339 UTC
    last_seen    TEXT NOT NULL,             -- 最后出现
    created_at   TEXT NOT NULL,
    updated_at   TEXT NOT NULL
);
CREATE INDEX idx_domains_last_seen ON domains(last_seen);
```

### 2.2 domain_probes（探测信号与结论）

```sql
CREATE TABLE domain_probes (
    domain       TEXT PRIMARY KEY,
    cname_hit    INTEGER NOT NULL DEFAULT 0,-- 0/1
    cidr_hit     INTEGER NOT NULL DEFAULT 0,-- 0/1
    cf_ray_hit   INTEGER NOT NULL DEFAULT 0,-- 0/1
    server_hit   INTEGER NOT NULL DEFAULT 0,-- 0/1
    score        INTEGER NOT NULL DEFAULT 0,-- 加权总分
    confidence   TEXT NOT NULL,             -- confirmed | maybe | not_cf
    cname_chain  TEXT,                      -- 实际 CNAME 链，审计用
    resolved_ips TEXT,                      -- 探测时解析到的 A/AAAA（JSON 数组）
    error        TEXT,                      -- 探测失败原因（成功为 NULL）
    probed_at    TEXT NOT NULL
);
```

### 2.3 rewrites（托管重写状态）

```sql
CREATE TABLE rewrites (
    domain       TEXT NOT NULL,             -- 与 domains.domain 对齐；通配条目带 *. 前缀
    answer       TEXT NOT NULL,             -- 裸 IP（禁止写域名型 CNAME）
    ip_version   INTEGER NOT NULL,          -- 4 | 6
    state        TEXT NOT NULL,             -- active | pending | removed
    agh_present  INTEGER NOT NULL DEFAULT 0,-- 最近一次 list 中 AGH 是否已有该条
    first_synced TEXT,
    last_synced  TEXT,
    last_error   TEXT,
    updated_at   TEXT NOT NULL,
    PRIMARY KEY (domain, answer, ip_version) -- 同域名可同时存在多条答案（v4/v6 互补）
);
CREATE INDEX idx_rewrites_state ON rewrites(state);
```

> AGH rewrite 条目没有"标签 / 归属"字段，托管归属完全靠本表 + AGH list 现状判定；表外域名一律不碰（见 [ARCHITECTURE.md](ARCHITECTURE.md) §4.5）。

### 2.4 runs（运行审计）

```sql
CREATE TABLE runs (
    id           INTEGER PRIMARY KEY,
    mode         TEXT NOT NULL,             -- dry | live
    started_at   TEXT NOT NULL,
    finished_at  TEXT,
    log_entries  INTEGER,                   -- 采集原始条目数
    candidates   INTEGER,                   -- 候选域名数
    confirmed    INTEGER,
    n_add        INTEGER NOT NULL DEFAULT 0,
    n_update     INTEGER NOT NULL DEFAULT 0,
    n_remove     INTEGER NOT NULL DEFAULT 0,
    n_failed     INTEGER NOT NULL DEFAULT 0,
    preferred_ip TEXT,                      -- 本次使用的优选 IP（多值时记录主 IP）
    note         TEXT                       -- 中止原因 / 汇总信息
);
```

### 2.5 状态枚举（唯一权威定义）

```text
confidence: confirmed | maybe | not_cf
state:      active | pending | removed
mode:       dry | live
ip_version: 4 | 6
```

## 3. 迁移约定

- 迁移文件放 `internal/state/migrations/`，按序号递增（如 `0001_init.sql`），程序启动时自动应用并记录版本。
- 已应用的迁移**只增不改**；需要变更新增迁移文件。
- `0001_init.sql` 已实现**定稿**（四表 + 索引，与 §2 一致）；后续 schema 变更一律新增 0002+ 迁移。

## 4. 配置项全集（已实现，默认值与 `internal/config.Default()` 一致）

> 形态：配置文件（YAML，`-c` 指定）+ 命令行 flags（同名 flag 覆盖文件值）。默认值集中在 `internal/config`，业务护栏默认值与 [PRD.md](PRD.md) §6 保持同义。

| 键 | 说明 | 默认值 |
| --- | --- | --- |
| `adguard.url` | AGH 根地址，如 `http://192.168.1.2:3000` | 无，必填（用户本地 `config.yaml` 提供实测实例） |
| `adguard.username` / `adguard.password` | 登录凭据；密码支持 `${ENV_VAR}` 展开（D13） | 无，必填 |
| `adguard.timeout` | AGH HTTP 超时 | `10s` |
| `querylog.window` | 采集窗口 | `7d`（可选 `24h`/`30d`） |
| `querylog.page_size` | 分页单页条数 | `500` |
| `querylog.max_pages` | 翻页页数上限（护栏：超大日志不能无限拉） | `200` |
| `querylog.fetch_timeout` | 采集阶段总超时 | `5m` |
| `querylog.clients_include` / `clients_exclude` | 客户端白 / 黑名单 | 空（不过滤） |
| `aggregate.mode` | 聚合粒度：`zone` 一级域混合（D14）/ `exact` 逐 host 精确（D6 旧行为） | `zone` |
| `aggregate.min_hits_24h` / `min_hits_7d` | 高频阈值 | `20` / `50` |
| `aggregate.max_domains` | 探测预算 / domains 行数上限（D15） | `1000` |
| `domains.include` / `domains.exclude` | 强制包含 / 排除域名 | 空 |
| `detector.resolvers` | **独立** DNS resolver 列表（P0 支持 `ip[:port]` 形态） | 无，必填至少一个 |
| `detector.concurrency` / `timeout` | 探测并发 / 单请求超时 | `8` / `3s` |
| `detector.http_enabled` | 是否启用 HTTP 头信号 | `true` |
| `detector.score_threshold` | 确认分数线 | `4` |
| `cfip.source` | 优选 IP 来源：`cfst:PATH`（默认）/ `domain:HOST` / `static:PATH` | `cfst:result.csv`（相对于工作目录；release 二进制放 CFST 发布目录执行即零配置，D17） |
| `cfip.strategy` | 多 IP 选择；cfst 来源下 `lowest_latency` 即取 CSV 首数据行（CFST 已按测速排序） | `lowest_latency`（可选 `roundrobin`） |
| `cfip.ip_versions` | 下发地址族 | `[4]`（v6 需 CFST 另跑 ipv6 测速后显式开启） |
| `sync.mode` | 写入通道 | `rewrite-api`（唯一支持值） |
| `sync.wildcard` | 是否允许生成 `*.zone` 通配条目（D14）；`false` 时 zone 组全部降级精确 | `true` |
| `sync.ttl` | pending → removed 淘汰时长 | `720h`（30d） |
| `sync.rate_limit` / `sync.retry` | 写操作限速 / 重试次数（live 模式消费，M5–M6） | `200ms` / `3` |
| `runtime.db_path` | SQLite 路径 | `./data/state.db` |
| `runtime.apply` | 是否真正执行写计划 | `false`（默认 dry-run；M5–M6 前 `--apply` 恒以退出码 2 报错，D10） |
| `runtime.log_level` / `log_file` | 日志级别 / 文件 | `info` / 空（stderr） |
| `runtime.schedule` | 内置调度 cron 表达式（P1） | 空（P0 单次运行退出） |

## 5. 容量护栏（D15）

- 查询日志原文**永不入库**；只存域名级统计与探测结论。
- `domains` ≤ `aggregate.max_domains`（默认 1000）行，超限按 `last_seen` 淘汰最旧并打 slog warn；被淘汰域名再次出现按新记录重新累计。
- `runs` 只保留最近 60 次，启动清理时淘汰最旧。
- `rewrites` / `domain_probes` 与目标集合对齐（confirmed/pending 域名级别），不单独设限。
- 预期库体积 < 5MB（NAS / 路由器小盘设备友好）。
