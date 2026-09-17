# cf-opt-adguard 数据模型与配置项（DATA_MODEL.md）

> 版本：v0.1 ｜ 状态：设计稿（尚无 migration；建表 SQL 实现时以此为基线，偏差必须回写本文）
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

## 2. 表结构（计划）

### 2.1 domains（域名聚合记录）

```sql
CREATE TABLE domains (
    id           INTEGER PRIMARY KEY,
    domain       TEXT NOT NULL UNIQUE,      -- 精确域名，punycode 小写
    zone         TEXT,                      -- 可注册域（仅 zone 模式聚合时填）
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
    domain       TEXT PRIMARY KEY,          -- 与 domains.domain 对齐
    answer       TEXT NOT NULL,             -- 裸 IP（禁止写域名型 CNAME）
    ip_version   INTEGER NOT NULL,          -- 4 | 6
    state        TEXT NOT NULL,             -- active | pending | removed
    agh_present  INTEGER NOT NULL DEFAULT 0,-- 最近一次 list 中 AGH 是否已有该条
    first_synced TEXT,
    last_synced  TEXT,
    last_error   TEXT,
    updated_at   TEXT NOT NULL
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
- P0 阶段表结构在首个 migration 定稿前可以迭代；一旦随版本发布即冻结。

## 4. 配置项全集（计划）

> 形态：配置文件（YAML，`-c` 指定）+ 命令行 flags（同名 flag 覆盖文件值）。默认值集中在 `internal/config`，业务护栏默认值与 [PRD.md](PRD.md) §6 保持同义。

| 键 | 说明 | 默认值 |
| --- | --- | --- |
| `adguard.url` | AGH 根地址，如 `http://192.168.1.2:3000` | 无，必填（用户本地 `config.yaml` 提供实测实例） |
| `adguard.username` / `adguard.password` | 登录凭据；密码支持 `${ENV_VAR}` 展开（D13） | 无，必填 |
| `adguard.timeout` | AGH HTTP 超时 | `10s` |
| `querylog.window` | 采集窗口 | `7d`（可选 `24h`/`30d`） |
| `querylog.page_size` | 分页拉取上限 | 实现时按 AGH 版本能力定值 |
| `querylog.clients_include` / `clients_exclude` | 客户端白 / 黑名单 | 空（不过滤） |
| `aggregate.mode` | 聚合粒度 | `exact`（可选 `zone`） |
| `aggregate.min_hits_24h` / `min_hits_7d` | 高频阈值 | `20` / `50` |
| `domains.include` / `domains.exclude` | 强制包含 / 排除域名 | 空 |
| `detector.resolvers` | **独立** DNS resolver 列表（IP/DoH URL） | 无，必填至少一个 |
| `detector.concurrency` / `timeout` | 探测并发 / 单请求超时 | 保守默认（实现时定值） |
| `detector.http_enabled` | 是否启用 HTTP 头信号 | `true` |
| `detector.score_threshold` | 确认分数线 | `4` |
| `cfip.source` | 优选 IP 来源：`domain:HOST` / `cfst:PATH` / `static:PATH` | 无，必填 |
| `cfip.strategy` | 多 IP 选择 | `lowest_latency`（可选 `roundrobin`） |
| `cfip.ip_versions` | 下发地址族 | `[4, 6]` |
| `sync.mode` | 写入通道 | `rewrite-api`（唯一支持值） |
| `sync.wildcard` | 是否允许通配重写 | `false` |
| `sync.ttl` | pending → removed 淘汰时长 | `30d` |
| `sync.rate_limit` / `sync.retry` | 写操作限速 / 重试次数 | 实现时定值 |
| `runtime.db_path` | SQLite 路径 | `./data/state.db` |
| `runtime.apply` | 是否真正执行写计划 | `false`（默认 dry-run；CLI `--apply` 置真，D10） |
| `runtime.log_level` / `log_file` | 日志级别 / 文件 | `info` / 空（stdout） |
| `runtime.schedule` | 内置调度 cron 表达式（P1） | 空（P0 单次运行退出） |
