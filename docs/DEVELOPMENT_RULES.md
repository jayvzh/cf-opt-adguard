# cf-opt-adguard 开发总则（DEVELOPMENT_RULES.md）

> 版本：v0.1 ｜ 本文对所有开发任务永久生效。日常任务先执行根目录 [开发规则.md](../开发规则.md)（精简版）；精简版与本文冲突时以本文为准。
> 本文只回答：怎么干活——环境、红线、分层、护栏、验证、自检。需求归 [PRD.md](PRD.md)，机制归 [ARCHITECTURE.md](ARCHITECTURE.md)。
> 相关：[PROJECT_STRUCTURE.md](PROJECT_STRUCTURE.md)、[DATA_MODEL.md](DATA_MODEL.md)、[conventions.md](conventions.md)。
> 更新时机：开发流程 / 红线 / 验证方式 / 自检项变化时。

---

## 1. 环境

- 语言：Go（本机 go1.27.1；go.mod 版本在工程初始化时声明，声明后团队以 go.mod 为准）。
- 工程**已初始化**（module path `cf-opt-adguard`，M1–M4 已实现）；标准命令：`go build ./...`、`go vet ./...`、`go test ./...`、`gofmt -l .`。
- 目标运行环境：用户本地 / NAS（Linux 为主，兼容 macOS / Windows 单二进制）；依赖 AGH 实例与外部网络（或用户配置的 resolver）。
- 敏感信息只允许出现在本地 `config.yaml`（不入库），仓库只提供 `config.example.yaml`。

## 2. 红线（永久生效，违反即返工）

1. **写操作必须经计划**：对 AGH 的任何写调用只能来自 syncer 消费 planner 产出的计划；禁止探测器 / 采集器 / 调试代码直接调 add/update/delete。
2. **测试不许碰真实 AGH / 公网写操作**：自动化测试一律 `httptest` 假服务器 + `testdata/` 夹具；对真实实例只允许只读核对与用户明确授权下的一次 live 验证。
3. **外部契约以实测为准**：AGH / CF / CFST 的字段、参数、返回值编码前先核实（目标实例或官方源码），禁止照抄 PRD、搜索结果或记忆；发现与文档不符，先改 [API.md](API.md) 并登记 [pitfalls.md](pitfalls.md)，再写代码。
4. **answer 只写裸 IP**：禁止把优选域名、`$dnsrewrite=` 语法或 CNAME 目标写进 rewrite answer。
5. **防误伤优先（D14）**：默认 zone 混合归并——zone 内仅 1 个达阈 host 用精确、≥2 全 confirmed 且 `sync.wildcard: true` 才允许通配、混入 maybe/not_cf 自动回退逐 host 精确；`aggregate.mode: exact` 为显式退回项。禁止无条件默认通配。
6. **只增量、只动托管集合**：禁止全量删除再添加；状态库外的用户手工 rewrite 一律不碰。
7. **优选 IP 为空 / 前置阶段失败时不得写 AGH**，尤其不得执行删除。
8. **范围红线**：一次只做 P0 链路；P1（通知、内置调度、回滚、多实例）、P2（Web UI 等）允许预留接口定义，禁止提前实现。

## 3. 分层与依赖

- 依赖方向以 [PROJECT_STRUCTURE.md](PROJECT_STRUCTURE.md) §3 的允许 / 禁止矩阵为唯一依据，代码评审逐条对照。
- `aggregate`、`detector` 打分、`planner` diff 必须是无 IO 纯函数；网络、数据库、时钟全部从外部注入。
- `adguard` 客户端不允许 import 任何业务包；端点 URL / 参数构造只在此包出现，业务包只调类型化方法。

## 4. 网络与外部调用规则

- 所有 HTTP / DNS 调用必须带超时与 `context.Context`；禁止无超时的默认 client。
- 探测并发、AGH 写限速走配置值；重试只用于幂等只读请求与 5xx / 网络错误，4xx（除 401 会话过期一次重登）不重试。
- 探测器使用用户配置的独立 resolver，禁止偷用系统默认 resolver（会被本机 AGH 重写污染结论）。
- User-Agent 标识本工具名称与版本；请求不携带 AGH 日志内容。

## 5. 业务数值护栏

- [PRD.md](PRD.md) §6 与 [DATA_MODEL.md](DATA_MODEL.md) §4 的默认值（窗口、阈值、TTL、确认分数线、并发 / 超时）是产品护栏：
  - 只能在 `internal/config` 默认值处定义一次，禁止散落在业务代码；
  - 调整默认值 = 产品决策，必须同批改 PRD / DATA_MODEL 并在 [decisions.md](decisions.md) 留痕。
- 打分权重只在 detector 打分函数与 ARCHITECTURE §4.3 两处保持同义；算法变了两处一起改。

## 6. 状态与迁移

- SQLite 迁移只增不改已应用文件，序号递增（见 [DATA_MODEL.md](DATA_MODEL.md) §3）。
- 状态枚举（confidence / state / mode）以 DATA_MODEL §2.5 为唯一权威，代码中不得自造新值。
- 不入库查询日志原文；日志输出对密码 / 完整 Cookie 脱敏。

## 7. 代码风格

- `gofmt` 格式化、`go vet` 干净；错误显式处理，禁止 `_ = err`；错误向上返回时带阶段前缀（如 `collector: ...`）。
- 包名短小单数、文件名 `snake_case.go`；对外类型放各包根文件，测试与实现同包。
- 结构化日志用 `log/slog`；阶段内用统一键名（`domain`、`answer`、`run_id`）。
- 不引重型框架；新增第三方依赖前在 [decisions.md](decisions.md) 说明理由与替代方案。

## 8. 验证阶梯（无 UI 项目，逐级进行，上一级能证明就不做下一级）

```text
1. go build ./... + go vet ./... + gofmt -l .
2. go test ./...（纯函数表驱动单测；adguard/syncer 用 httptest 夹具）
3. go run ./cmd/cf-opt-adguard run -c config.yaml（默认 dry 模式，对真实实例只读：拉日志/list 不拉写）
4. 用户明确授权时，对用户 config.yaml 指向的 AGH 实例执行一次 run --apply 并核对结果，随后清理测试域名
```

- 禁止为"试一下"对真实 AGH 执行写操作；第 4 级永远不允许在未授权的生产实例上进行。
- 探测器对真实公网域名的联网验证属于第 2→3 级之间的一次性人工抽查，不进自动化测试。

## 9. 文档维护（代码变更 → 文档同步）

| 代码变更 | 必须同步 |
| --- | --- |
| CLI flags / 退出码 / 计划输出变化 | [API.md](API.md) §7、README（使用示例） |
| AGH / CF / CFST 端点、字段、版本兼容变化 | [API.md](API.md)、[pitfalls.md](pitfalls.md) |
| 表结构 / 配置键 / 默认值变化 | [DATA_MODEL.md](DATA_MODEL.md)（必要时 PRD §6） |
| 流水线 / 状态机 / 失败策略变化 | [ARCHITECTURE.md](ARCHITECTURE.md)、[PROJECT_STRUCTURE.md](PROJECT_STRUCTURE.md)、[context.md](context.md) |
| 包结构 / 依赖方向变化 | [PROJECT_STRUCTURE.md](PROJECT_STRUCTURE.md) |
| 重要选型、新增依赖 | [decisions.md](decisions.md) |
| 踩坑、版本差异、实测与文档不一致 | [pitfalls.md](pitfalls.md) |
| 新约定 / 缩写 / 技术债绕法 | [conventions.md](conventions.md) |

- 文档随代码同一变更交付，不接受"以后补"。
- 新增 docs 文件必须三处登记：根 [开发规则.md](../开发规则.md) 文档地图、README 索引、[context.md](context.md) 导航。
- 删除 / 重命名文档前全仓搜索旧名，引用与被引文件同批修改。

## 10. 交付自检清单

```text
[ ] go build / go vet / gofmt 检查通过
[ ] 新增纯函数（聚合 / 打分 / diff）有表驱动单测；adguard / syncer 测试只用 httptest 夹具
[ ] 没有任何对真实 AGH / 公网的写操作出现在测试或调试代码中
[ ] 所有写调用都经 planner 计划且只出现在 syncer
[ ] answer 全部是裸 IP；通配仅在 zone 全 confirmed 且 wildcard 显式开启时出现（D14）
[ ] 外部接口字段与实测一致，差异已记入 pitfalls
[ ] 默认 dry 模式确认无任何写调用；只有显式 --apply 才走 syncer 写路径
[ ] 优选 IP 为空 / 前置失败路径已确认不会触发写或删
[ ] 默认值只在 config 定义一次；改动的护栏已同步 PRD / DATA_MODEL / decisions
[ ] 未实现 P1 / P2 范围（无 notify / 调度 / Web UI 代码）
[ ] 配置文件无真实密码；config.example.yaml 同步更新
[ ] 受影响文档已随代码同批更新，链接可达、无悬空引用
```
