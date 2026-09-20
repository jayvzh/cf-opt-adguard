# 开发文档（DEVELOPMENT.md）

> 面向开发者的工程入口：环境、命令、结构、文档地图、红线摘要。用户向说明见 [README.md](README.md)。

## 项目一句话

Go 单二进制 CLI（module path `cf-opt-adguard`）：从 AdGuard Home 查询日志发现高频 Cloudflare 域名，独立探测确认后把优选 IP 增量写成 AGH DNS 重写，并持续维护。当前 P0 全链路（M1–M6：dry-run + `--apply` live 写入与验证）已实现。

## 环境

- Go ≥ go.mod 声明版本（本机 go1.27.1），无 CGO 依赖。
- 目标运行环境：用户本地 / NAS（Linux 为主），依赖 AGH 实例与外部网络。
- 敏感信息只在本地 `config.yaml`（不入库），仓库仅提供 `config.example.yaml`。

## 常用命令

```bash
gofmt -l .                                  # 格式检查
go build ./...                              # 编译
go vet ./...                                # 静态检查
go test ./...                               # 单测（httptest 假服务器，不依赖网络）
go run ./cmd/cf-opt-adguard run -c config.yaml        # 冒烟：默认 dry-run，对真实实例只读
go run ./cmd/cf-opt-adguard run -c config.yaml --apply # 用户明确授权后的 live 写入
```

- CLI flags / 退出码以 `--help` 与 [docs/API.md](docs/API.md) §7 为准；改动后同步更新 API.md 与 README。
- 发布构建：`bash scripts/build-release.sh [版本号]`（linux amd64/arm64 双架构打包到 `release/`）。
- 安装脚本：`scripts/install.sh`（交互向导 + 非交互子命令，详见 README）。

## 目录结构概览

```text
cmd/cf-opt-adguard/   # 入口：run / version / export 子命令装配与退出码
internal/
  config/             # YAML + flags 覆盖 + ${ENV} 展开 + 默认值（唯一默认值定义处）
  pipeline/           # 阶段编排（dry-run / apply）
  collector/          # querylog 分页采集
  aggregate/          # 归一化 + zone 归并 + 通配/精确决策（纯函数）
  detector/           # CF 探测：resolver / cidr / httphead + 打分（纯函数）
  ipselector/         # 优选 IP 三来源（默认读 CFST result.csv）
  planner/            # 对比现状生成 add/update/remove 计划（纯函数）
  syncer/             # 全工程唯一 AGH 写侧（仅 --apply 时消费计划）
  verifier/           # 经 AGH 回查 DNS 验证
  adguard/            # AGH REST 类型化客户端（auth / querylog / rewrite）
  state/              # SQLite 仓储 + 迁移 + 容量护栏
scripts/              # build-release.sh / install.sh
docs/                 # 设计与规则文档（见下）
```

包职责边界与依赖方向矩阵的唯一权威：[docs/PROJECT_STRUCTURE.md](docs/PROJECT_STRUCTURE.md)。

## AI开发 文档地图

| 任务 | 打开 |
| --- | --- |
| 判定需求范围 / 非目标 / 默认护栏 | [docs/PRD.md](docs/PRD.md) |
| 流水线 / 探测打分 / 状态机 / 失败策略 | [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md) |
| 包放哪 / 依赖方向 / 阶段 I/O | [docs/PROJECT_STRUCTURE.md](docs/PROJECT_STRUCTURE.md) |
| 表结构 / 配置键 / 枚举 | [docs/DATA_MODEL.md](docs/DATA_MODEL.md) |
| AGH / CF / CFST 接口字段 / CLI flags | [docs/API.md](docs/API.md) |
| 背景现状 / 关键决策结论 / 下一步 | [docs/context.md](docs/context.md) |
| 选型理由 / 新依赖 | [docs/decisions.md](docs/decisions.md) |
| AGH 版本差异 / 已知坑 | [docs/pitfalls.md](docs/pitfalls.md) |
| 缩写 / 目录约定 / 技术债绕法 | [docs/conventions.md](docs/conventions.md) |
| 完整开发规则（总则） | [docs/DEVELOPMENT_RULES.md](docs/DEVELOPMENT_RULES.md) |
| 日常精简规则（AI 第一份规则） | [开发规则.md](开发规则.md) |

## AI开发 红线摘要（完整版见 docs/DEVELOPMENT_RULES.md §2）

1. 对 AGH 的写调用只能来自 syncer 消费 planner 计划；禁止全量删除再添加，不碰托管集合之外的用户手工规则。
2. 测试一律 `httptest` + 夹具；对真实实例只允许 dry-run 只读，live 写入需用户明确授权。
3. 外部接口字段以实测为准，禁止照抄 PRD / 搜索结果；差异先改 API.md + 记 pitfalls.md 再写代码。
4. rewrite answer 只写裸 IP；默认 zone 混合归并防误伤（D14），通配仅 zone 内全 confirmed 且 `sync.wildcard: true`。
5. 探测只用用户配置的独立 resolver，禁止偷用系统默认。
6. 默认 dry-run，显式 `--apply` 才写入；优选 IP 为空或前置阶段失败时禁止任何写 / 删。
7. 业务默认值只在 `internal/config` 定义一次；`references/` 只读。
8. 只做 P0；P1（通知 / 内置调度 / 回滚 / 多实例）、P2（Web UI）禁止提前实现。

## 验证阶梯（上一级能证明就不做下一级）

```text
1. go build ./... + go vet ./... + gofmt -l .
2. go test ./...（纯函数表驱动单测；adguard/syncer 用 httptest 夹具）
3. go run ./cmd/cf-opt-adguard run -c config.yaml（dry-run，对真实实例只读）
4. 用户明确授权后对目标实例 run --apply 并核对，随后清理测试域名
```

## 代码变更 → 文档同步

| 代码变更 | 必须同步 |
| --- | --- |
| CLI flags / 退出码 / 计划输出 | docs/API.md §7、README |
| AGH / CF / CFST 端点与字段 | docs/API.md、docs/pitfalls.md |
| 表结构 / 配置键 / 默认值 | docs/DATA_MODEL.md（必要时 PRD §6） |
| 流水线 / 状态机 / 失败策略 | docs/ARCHITECTURE.md、docs/PROJECT_STRUCTURE.md、docs/context.md |
