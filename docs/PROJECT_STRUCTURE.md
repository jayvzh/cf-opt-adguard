# cf-opt-adguard 目录职责与模块边界（PROJECT_STRUCTURE.md）

> 版本：v0.1 ｜ 状态：目标结构设计稿（代码尚未创建）。首个里程碑只建 P0 链路所需文件，禁止预实现 P1 / P2 模块。
> 本文只回答：放哪——目录职责、包边界、阶段间数据流。运行机制见 [ARCHITECTURE.md](ARCHITECTURE.md)，表结构见 [DATA_MODEL.md](DATA_MODEL.md)。
> 相关：[ARCHITECTURE.md](ARCHITECTURE.md)、[DEVELOPMENT_RULES.md](DEVELOPMENT_RULES.md)。
> 更新时机：新增 / 重命名 / 移动包，或阶段间数据流变化时（同步检查本文与依赖矩阵）。

---

## 1. 目标目录结构

```text
cf-opt-adguard/
├── cmd/
│   └── cf-opt-adguard/
│       └── main.go              # 仅 config.Load() → pipeline.Run()，禁止写业务
├── internal/
│   ├── config/                  # 配置加载：flags + config 文件 + 默认值
│   ├── pipeline/                # run 编排：按序调用各阶段、汇总运行结果
│   ├── collector/               # F1：querylog 分页拉取与过滤
│   ├── aggregate/               # F2：归一化 + 频次统计（纯函数）
│   ├── detector/                # F3：resolver / cidr / httphead / 打分（打分纯函数）
│   ├── ipselector/              # F4：优选 IP 三来源
│   ├── planner/                 # F6：现状对比，产出 add/update/remove 计划（纯函数）
│   ├── syncer/                  # F5：执行 AGH rewrite 写操作（全工程唯一写侧）
│   ├── verifier/                # F8：经 AGH 回查 DNS 验证
│   ├── adguard/                 # AGH HTTP 客户端：auth / querylog / rewrite 端点封装
│   ├── state/                   # SQLite 仓储、迁移、状态枚举
│   └── (notify/ P1 再建，MVP 不允许出现)
├── config.example.yaml          # 配置模板（不含真实密码）
├── scripts/                     # 按需建立：构建 / 发布辅助（非必需不预置）
├── docs/                        # 本目录
├── references/                  # 只读参考材料（规范、上游样例、CFST 源码），不参与构建
└── README.md
```

> 项目 / 二进制名已定：`cf-opt-adguard`（D9），即 `cmd/cf-opt-adguard/`；Go module path 在 `go mod init` 时按实际仓库地址确定，之后包导入路径一次对齐。

## 2. 包职责边界

| 包 | 只负责 | 禁止 |
| --- | --- | --- |
| `main` | 装配入口、退出码 | 任何业务逻辑、直接 new 各阶段客户端 |
| `config` | 解析 flags / 配置文件 / 环境变量、校验必填项、持有默认值 | 发网络请求、访问状态库 |
| `pipeline` | 阶段顺序编排、上下文取消、运行汇总 | 实现具体采集 / 探测 / 同步逻辑 |
| `collector` | querylog 分页、过滤、翻页终止条件 | 频次统计、探测、任何写操作 |
| `aggregate` | 域名归一化、频次 / 阈值计算（纯函数） | IO、网络、状态库写入 |
| `detector` | CNAME / CIDR / HTTP 信号采集与打分 | 写状态库之外的副作用、调用 AGH 写 API |
| `ipselector` | 三来源取 IP、多 IP 选择、v4/v6 区分 | 改写域名集合、直接写 AGH |
| `planner` | 对比现状生成计划（纯函数） | 发 HTTP、执行计划 |
| `syncer` | 执行计划、限速重试、逐条结果回传 | 自行决定"哪些域名该写"（只消费计划） |
| `verifier` | 经 AGH 做 DNS 回查 | 修改任何数据 |
| `adguard` | AGH REST 端点的类型化客户端 | 业务判断（过滤 / 打分 / diff） |
| `state` | SQLite CRUD 与迁移 | 网络访问、业务规则 |

## 3. 依赖规则

```text
允许：
  main        → config, pipeline
  pipeline    → collector / aggregate / detector / ipselector / planner / syncer / verifier / state
  collector   → adguard
  syncer      → adguard, state
  verifier    → adguard（或独立 DNS client）
  各业务包    → state（写自己的记录）、config（只读配置值）

禁止：
  collector / aggregate / detector / ipselector / planner → AGH 写端点（只有 syncer 能写）
  adguard     → 任何业务包（客户端不得反向依赖）
  planner     → adguard / 网络（计划由纯函数对入参计算）
  aggregate   → detector / 下游包（上游阶段不得感知下游）
  任何包      → references/（参考材料只读、不引用、不 go:embed）
```

## 4. 核心数据流（阶段 I/O）

| 阶段 | 输入 | 输出 |
| --- | --- | --- |
| collector | AGH 连接配置、时间窗口、客户端 / 域名过滤配置 | 原始查询条目流（host、类型、时间、client、reason） |
| aggregate | 原始条目、聚合粒度、阈值 | 候选域名集合（domain、zone、hits、first/last seen） |
| detector | 候选集合、resolver / CIDR 缓存、HTTP 配置 | 探测结果（逐信号明细 + 三态结论 + 分数） |
| ipselector | 来源配置（domain / cfst / static） | 优选 IP 列表（区分 v4/v6）；为空则中止 |
| planner | confirmed 集合 + 状态库托管集合 + AGH rewrite 现状 + 优选 IP | 不可变计划：add[] / update[] / remove[] |
| syncer | 计划、dry-run 标志 | 逐条执行结果（成功 / 失败 + 原因） |
| verifier | 已执行计划 | 每域名验证结果 |
| state | 各阶段结果 | domains / probes / rewrites / runs 落库 |

## 5. 入口与装配约束

- `main.go` 固定三步：`config.Load() → pipeline.New(依赖).Run(ctx) → 按退出码退出`。
- 依赖组装集中在 `pipeline`（或首个里程碑设立的 `internal/app`）中完成，避免 main 膨胀。
- CLI 用标准库 `flag` 或保持子命令解析轻量；命令契约以 [API.md](API.md) §7 为准。

## 6. 可测试性约束

- `aggregate`（归一化 / 阈值）、`detector` 的打分函数、`planner` 的 diff 必须是无 IO 纯函数，表驱动单测覆盖。
- `adguard` 客户端与 `syncer` 的测试一律用 `httptest` 假服务器 + 固定 JSON 夹具，禁止依赖真实 AGH 实例（夹具放在各包 `testdata/`）。
- 时间相关逻辑（窗口、TTL）注入时钟，禁止在业务函数里直取 `time.Now()`。
