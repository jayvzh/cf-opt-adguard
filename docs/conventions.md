# 项目专属约定与技术债（conventions.md）

> 分三组：目录与模块 / 命名与缩写 / 技术债与绕法。只写本项目独有的约定，通用 Go 常识不写。
> 更新时机：形成新约定、发现新技术债绕法时。

---

## 1. 目录与模块

- `references/` 是**只读参考材料**：不参与构建、禁止 Go 代码 import / `go:embed`；其中两份上游仓库各自保留独立 LICENSE，本项目代码不从其中拷贝大段实现。
- 权威文档只放 `docs/`（git 管理路径）；`.trae/rules/` 只放可秒重建的指针文件（见 [开发规则.md](../开发规则.md) 与规范）。
- 测试夹具统一放各包 `testdata/`；假服务器基于 `net/http/httptest`，JSON 夹具命名 `*_response.json`。
- 本地运行产物放 `./data/`（SQLite、CIDR 缓存），目录整体加入忽略，不入库；CFST 的 `result.csv` 不是本工具产物——release 约定在 CFST 发布目录读取（D17），开发期真实样例作只读夹具保留在 `references/` 或拷贝进各包 `testdata/`。
- `config.example.yaml` 与配置结构同批改（新增配置键必须补示例与注释）。

## 2. 命名与缩写

| 缩写 / 术语 | 含义 |
| --- | --- |
| AGH | AdGuard Home |
| CF | Cloudflare |
| CFST | CloudflareSpeedTest（外部测速二进制） |
| CIDR | IP 网段表示，CF 官方网段用于探测信号 |
| IDN | 国际化域名；存储统一 punycode，展示还原 Unicode |
| zone | 可注册域（如 `example.com`），相对于精确域名 |
| 托管集合 | 本工具状态库中登记过 rewrite 的域名集合，是唯一允许增删改的范围 |
| 计划（plan） | planner 输出的 add / update / remove 不可变数据结构 |

- Go 包名按 PROJECT_STRUCTURE §1 表中的名字（collector / aggregate / detector / ipselector / planner / syncer / verifier / adguard / state / pipeline / config），不另造同义词包。
- 项目 / 二进制名已定 `cf-opt-adguard`（D9）；Go module path 在 `go mod init` 前不得写死，以实际仓库地址为准。
- 日志字段键固定：`run_id`、`domain`、`answer`、`state`、`stage`；错误包装前缀用包名（`adguard: ...`）。
- 域名文本统一小写 punycode；时间统一 UTC RFC3339；配置中的时长用 Go duration 文本（`7d` 这类窗口在 config 层解析，业务层只收 `time.Duration` / 时间点）。

## 3. 技术债与绕法

- **`cdncheck` 暂不集成**：PRD 提到可作辅助，P0 用 CNAME + CIDR + HTTP 头三类自研信号即可达到确认线；需要时以可选依赖形式接入，不得让它成为判定硬依赖。
- **AGH 版本策略**：P0 只支持新版 querylog 参数（D11），无版本分支；开工以用户 `config.yaml` 实例实测固化字段。未来若加旧版兼容，分支只允许存在于 `adguard` 客户端内部，业务包看到的必须是统一结构。
- **内置调度 / 通知 / 回滚 / Web UI 为 P1/P2**：现在只允许在 config 键与包注释中预留位置，禁止提前建包实现（红线第 8 条）。
- **公共后缀数据**：zone 聚合**默认开启**（D14，`aggregate.mode: zone`）；PSL 随依赖 `golang.org/x/net/publicsuffix` 离线分发，不做运行时在线拉取，无需单独维护静态副本。
- **SQLite 驱动已定**：使用纯 Go `modernc.org/sqlite`（D12，免 CGO）；数据访问走标准 `database/sql`，不引入驱动专有 API。
