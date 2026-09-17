# 背景与现状（context.md）

> 版本：v0.1 ｜ 本文只回答"这是什么项目、到哪一步"，变化快、有时效性。架构机制只放链接，不复述。
> 更新时机：每完成一个里程碑、阶段判断变化时。

---

## 1. 这是什么项目

`cf-opt-adguard` 是一个本地运行的外部编排器：从 AdGuard Home 查询日志挖高频域名 → 独立探测确认 Cloudflare CDN → 取优选 IP → 通过 AGH Rewrite API 增量写入 DNS 重写。完整需求见 [PRD.md](PRD.md)，运行机制见 [ARCHITECTURE.md](ARCHITECTURE.md)。

## 2. 当前状态（2026-09-17）

- **阶段：未开工 / 文档骨架刚完成**。仓库内无任何 Go 代码，尚未 `go mod init`，尚未初始化 git。
- 本机环境：Go 1.27.1（linux/amd64）。
- 已有材料：
  - [PRD.md](PRD.md)：v0.2，已按文档规范重构为纯产品视角；
  - L2 设计文档一套（架构 / 结构 / 数据模型 / API / 开发总则 / 四件套），均为**设计稿**，实现时以代码核实回填；
  - `references/`：文档规范样板、AlbumShelf 文档样例、两份参考代码（见下）。
- 接口关键事实已于 2026-09-17 对 AGH master 源码 + OpenAPI 与 vendored CFST 源码取证，见 [API.md](API.md) §1.1。

## 3. references/ 目录的角色（只读，不参与构建）

| 路径 | 是什么 | 怎么用 |
| --- | --- | --- |
| `references/开发文档规范标准.md` | 三层文档体系规范 | 维护文档时遵守 |
| `references/docs/` | AlbumShelf 项目的文档落地样例 | 格式参考，内容与本项目无关 |
| `references/CloudflareSpeedTest-Adguard-Script-main/` | 社区单域名同步脚本（Go） | AGH 登录 / 旧 user_rules 通道的事实参考，**其方案不采用**（见 [decisions.md](decisions.md) D3） |
| `references/CloudflareSpeedTest-master/` | CloudflareSpeedTest 源码 | 来源 B：结果文件格式与调用方式参考 |

## 4. 下一步（首个里程碑建议顺序）

1. 定名 + `git init` + `go mod init`；
2. `config` 与 `adguard` 客户端（先只读：status / querylog / rewrite list，配 httptest 夹具）；
3. `aggregate` + `detector` 纯函数核心与单测（先跑通 dry-run 全链路）；
4. `ipselector`（先只做来源 A）+ `planner` + `state`；
5. 最后接 `syncer` / `verifier` 的 live 写入；
6. 里程碑收尾：把设计稿中的"待核实"逐项与实测核对，更新文档版本号去掉"设计稿"标注。

## 5. 开放问题（2026-09-17 已全部拍板，详见 [decisions.md](decisions.md) D9~D13）

1. ~~项目名称~~ → 定名 **cf-opt-adguard**；Go module path 留到 git init 时按仓库地址确定（D9）。
2. ~~安全默认~~ → **默认 dry-run，显式 `--apply` 才写入**（D10）。
3. ~~AGH 版本范围~~ → **P0 只支持新版参数**（`limit/older_than/reason`）；开工时以用户 `config.yaml` 中配置的真实 AGH 实例实测固化字段（D11）。
4. ~~SQLite 驱动~~ → 纯 Go **modernc.org/sqlite**，免 CGO（D12）。
5. ~~配置形态~~ → **YAML + flags 覆盖 + `${ENV}` 取密码**（D13）。

> 测试约定：用户会在本地放置 `config.yaml`（不入库）指向其真实 AGH 服务器，作为开工后的联调实测对象；自动化测试仍只用 httptest 夹具。

## 6. 文档导航

| 想知道 | 打开 |
| --- | --- |
| 做什么 / 不做什么 / 默认护栏 | [PRD.md](PRD.md) |
| 流水线怎么跑、打分 / 状态机 / 失败策略 | [ARCHITECTURE.md](ARCHITECTURE.md) |
| 包怎么放、谁能依赖谁 | [PROJECT_STRUCTURE.md](PROJECT_STRUCTURE.md) |
| 表 / 配置项 / 枚举 | [DATA_MODEL.md](DATA_MODEL.md) |
| AGH / CF / CFST 接口字段、CLI flags | [API.md](API.md) |
| 红线、验证阶梯、交付自检 | [DEVELOPMENT_RULES.md](DEVELOPMENT_RULES.md) |
| 选型决策记录 | [decisions.md](decisions.md) |
| 已知坑 / 版本差异 | [pitfalls.md](pitfalls.md) |
| 约定、缩写、技术债绕法 | [conventions.md](conventions.md) |
