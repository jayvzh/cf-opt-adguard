# cf-opt-adguard

> 状态：**设计阶段，尚未开工**（无代码 / 未初始化 Go module）。本文档集 v0.1 完成于 2026-09-17，首个里程碑落地后回填构建与部署说明。

一个本地运行的 **AdGuard Home × Cloudflare 优选 IP 自动编排器**：从 AGH 查询日志挖掘高频域名，独立探测确认 Cloudflare CDN，把优选 IP 增量写为 AGH 的 DNS 重写规则，并持续维护（新站点自动加入、失效站点自动淘汰）。

## 它如何工作

```text
AGH 查询日志 → 采集 → 频次聚合 → CF 探测（独立 DNS + IP 段 + HTTP 头打分）
           → 优选 IP（优选域名 / CloudflareSpeedTest / 静态列表）
           → 增量计划（add/update/remove）→ AGH Rewrite API → DNS 回查验证
```

- 外部独立 CLI，不改 AGH 核心、不做 AGH 插件；
- 默认精确域名重写，支持 dry-run 预览，全程本地运行、不上传查询日志；
- 规划细节见 [docs/PRD.md](docs/PRD.md) 与 [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md)。

## 部署与使用

首个里程碑交付后补充（Go 单二进制，计划支持配置文件 + flags，外部 cron / systemd timer 周期触发）。当前可先阅读文档了解设计，开放决策见 [docs/context.md](docs/context.md)。

## 项目文档

> 三层文档体系：日常任务读根《开发规则》即可；L3 指针位于 `.trae/rules/开发规则.md`（IDE 自动注入）。

| 文档 | 内容 |
| --- | --- |
| [开发规则.md](开发规则.md) | L1 开发第一份规则：文档地图、铁律、常用命令、交付自检 |
| [docs/PRD.md](docs/PRD.md) | 产品需求：做什么 / 不做什么、功能优先级、业务默认值、验收标准 |
| [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md) | 技术架构：流水线机制、探测打分、状态机、同步与失败策略 |
| [docs/PROJECT_STRUCTURE.md](docs/PROJECT_STRUCTURE.md) | 目标 Go 包结构、职责边界、依赖矩阵、阶段 I/O |
| [docs/DATA_MODEL.md](docs/DATA_MODEL.md) | SQLite 表结构、状态枚举、配置项全集、迁移约定 |
| [docs/API.md](docs/API.md) | AGH / Cloudflare / CFST 外部接口契约 + CLI flags 与退出码 |
| [docs/DEVELOPMENT_RULES.md](docs/DEVELOPMENT_RULES.md) | 完整开发总则：红线、分层、验证阶梯、文档维护映射、自检清单 |
| [docs/context.md](docs/context.md) | 背景与现状、参考材料说明、已拍板的 5 项关键决策、下一步、文档导航 |
| [docs/decisions.md](docs/decisions.md) | 重要选型决策记录（ADR） |
| [docs/pitfalls.md](docs/pitfalls.md) | AGH 版本差异等已知坑与绕开姿势（附取证来源） |
| [docs/conventions.md](docs/conventions.md) | 项目专属约定、缩写表、技术债与绕法 |

`references/` 为只读参考材料（文档规范、文档样例、社区同步脚本与 CloudflareSpeedTest 源码），不参与构建。

## 免责声明

本工具修改家庭 / 内网 DNS 解析行为，误判可能影响个别站点访问；`run` 默认即为 dry-run（只打印计划不写入），请核对计划无误后再加 `--apply` 执行，建议先在测试实例验证。
