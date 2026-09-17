# 选型决策记录（decisions.md）

> 格式：日期 + 标题 + 背景 / 决策 / 原因 / 影响。只记录重要、非显而易见、未来可能复用的决策。
> 更新时机：做出新的重要选型或推翻旧决策时（旧条目不删，追加"已被 Dx 取代"标注）。

---

## D1 ｜ 2026-09-17 ｜ 外部编排器，不做 AGH 插件 / 核心修改

- **背景**：需求本质是"按响应 IP 判断 Cloudflare 并自动重写"，AGH 官方不支持该能力。
- **决策**：独立 CLI 进程，只通过 AGH REST API 交互。
- **原因**：不改 AGH 可随官方版本平滑升级；失败面隔离在工具自身；卸载即恢复原状。
- **影响**：必须自行维护托管状态与归属判定（AGH rewrite 无归属标签）；覆盖率受日志窗口限制（写入 PRD 非目标与风险）。

## D2 ｜ 2026-09-17 ｜ Go 单二进制 + CLI + 外部定时器

- **背景**：候选含常驻服务 / Web UI 方案。
- **决策**：P0 只做单次运行退出的 CLI，定时靠系统 cron / systemd timer；内置调度 P1，Web UI P2。
- **原因**：核心逻辑是周期性批处理，无需常驻；单二进制在 NAS / 路由器上部署成本最低；Go 网络库（DNS、HTTP）与调用外部测速二进制都方便。
- **影响**：状态必须持久化在 SQLite 而不是内存；每次运行天然按幂等重放设计。

## D3 ｜ 2026-09-17 ｜ 重写通道选 Rewrite API，不用 filtering user_rules

- **背景**：参考脚本（`references/CloudflareSpeedTest-Adguard-Script-main/main.go`）通过 `POST /control/filtering/set_rules` 提交 `{"rules":[...]}`，该接口**全量覆盖** AGH 用户自定义规则，脚本靠"读出 → 改一条 → 整体写回"工作。
- **决策**：本工具只用 `/control/rewrite/list|add|update|delete` 做条目级增删改。
- **原因**：全量覆盖通道会误伤用户手工规则且并发写互相踩踏；rewrite 条目天然 domain→answer 模型，正合本工具语义，可逐条限速重试。
- **影响**：托管集合与用户规则物理隔离（分属两套 AGH 机制），归属判定只需在 rewrite 范围内做；代价是同域名多地址族要管理多条（domain+answer 为键）。

## D4 ｜ 2026-09-17 ｜ answer 写裸 IP，不写域名型重写

- **背景**：AGH 支持 `$dnsrewrite=<域名>` 之类 CNAME 形式，看似可以省掉"解析优选域名"步骤。
- **决策**：由 ipselector 先把优选域名解析成 IP，rewrite answer 只写 IP 文本。
- **原因**：CNAME 形式把解析结果交回 AGH / 上游，失去"优选"确定性，且无法做 IP 族区分与版本化更新；PRD 明确不推荐。
- **影响**：每次运行都要重解析优选域名，IP 变化即触发批量 update；优选 IP 解析为空时必须中止（ARCHITECTURE §7）。

## D5 ｜ 2026-09-17 ｜ 探测使用独立 resolver，禁止走系统默认

- **背景**：运行本工具的主机往往正把 AGH 当作默认 DNS；一旦已有重写，系统解析结果会被自己改写。
- **决策**：detector 的 CNAME / A 查询只走用户显式配置的独立 resolver（UDP 53 或 DoH）。
- **原因**：否则 CNAME / CIDR 信号是自我印证的循环结论，误判率无法控制。
- **影响**：`detector.resolvers` 成为必填配置；ipselector 解析优选域名同样走该 resolver。

## D6 ｜ 2026-09-17 ｜ 默认精确域名聚合，zone / 通配显式开启（部分被 D14 修订）

- **背景**：按根域重写一条可覆盖全部子域，但同根域下常有非 CF 子域。
- **决策**：默认仅对 confirmed 的精确域名写规则；`aggregate.mode=zone` 与 `sync.wildcard` 都是独立开关，默认关闭。
- **原因**：规则污染的代价（站点解析异常）高于少覆盖几个域名；PRD 将污染列为主要风险。
- **影响**：规则条数更多；planner 的 remove 集合严格限定在本工具写入过的精确域名。
- **2026-09-17 修订（D14）**：默认改为一级域（zone）混合归并 + 证据不足自动回退精确；"防污染优先"的原则保留（回退规则正是该原则的体现），`aggregate.mode: exact` 仍可整体退回逐 host 精确旧行为。

## D7 ｜ 2026-09-17 ｜ 状态用 SQLite，纯函数核心 + 薄 IO 外壳

- **决策**：域名 / 探测 / 重写 / 运行记录落 SQLite；聚合、打分、diff 设计为无 IO 纯函数，网络与时钟注入。
- **原因**：CLI 每次退出，状态必须外存；纯函数核心让最容易出错的统计 / 判定 / 差异逻辑可用表驱动测试覆盖，不依赖网络与真实 AGH。
- **影响**：包结构按"阶段纯逻辑 + adguard/state 外壳"切分（PROJECT_STRUCTURE）；时间相关逻辑必须注入时钟。

## D8 ｜ 2026-09-17 ｜ CloudflareSpeedTest 只做外部依赖，不重造测速（部分被 D17 修订）

- **背景**：CFST 已成熟实现延迟 / 丢包 / 下载测速与 IP 段下载。
- **决策**：来源 B 通过调用外部二进制 + 解析结果文件（CSV）集成，本仓库内嵌的 CFST 源码仅作格式参考。
- **原因**：测速算法维护成本高且非本工具核心价值；CSV 是稳定的外部契约。
- **影响**：来源 B 需要用户自行提供二进制；文件缺失 / 结果为空按"优选 IP 为空"安全中止。
- **2026-09-17 修订（D17）**：来源 B 升格为**默认来源**；"调用外部二进制测速"环节在 MVP 不实现，本工具只读用户自行测速后产出的 `result.csv`，自动调用推迟到 P1。

## D9 ｜ 2026-09-17 ｜ 定名 cf-opt-adguard

- **决策**：项目 / 二进制名称定为 `cf-opt-adguard`（备选 cf-agh-rewriter / AdGuard-CF-AutoRewrite 弃用）。
- **原因**：与仓库目录同名，文档已统一使用，零返工；语义（CF 优化 × AdGuard）清晰且命令长度适中。
- **影响**：Go module path 留到 `git init` 时按实际仓库地址确定；cmd 目录为 `cmd/cf-opt-adguard/`。

## D10 ｜ 2026-09-17 ｜ 默认 dry-run，显式 --apply 才写入

- **背景**：原 PRD 设计为默认 live 执行、靠 `--dry-run` 预览；误配置或首次运行就会改动真实 AGH 规则。
- **决策**：`run` 默认只产出计划（dry），必须显式 `--apply` 才进入 live 写操作。
- **原因**：本工具操作家庭 / 内网 DNS 解析，误写影响面大；cron 任务由用户主动写上 `--apply`，安全边界明确。
- **影响**：PRD §6 增加"默认模式"护栏；CLI 契约（API.md §7）改为 `--apply`；配置键 `runtime.apply` 默认 false；两种模式仍消费同一份计划。

## D11 ｜ 2026-09-17 ｜ P0 只支持新版 AGH 参数，实测对象为用户实例

- **决策**：querylog 只实现新版参数集（`limit` / `offset` 或 `older_than` / `reason`），不做旧版 `startTime/endTime`、`response_status` 兼容层。
- **原因**：双版本适配让 P0 工作量与测试面翻倍；用户自有 AGH 实例为新版，可直接覆盖真实需求。
- **影响**：开工后第一件事是用用户 `config.yaml` 指向的实例做只读实测，把 API.md 中"以实测为准"的字段固化；旧版兼容作为未来需求再排期。

## D12 ｜ 2026-09-17 ｜ SQLite 选用纯 Go 驱动 modernc.org/sqlite

- **决策**：使用 `modernc.org/sqlite`（纯 Go，无 CGO）。
- **原因**：单二进制需在 Linux / macOS / Windows 乃至 NAS / 路由器交叉编译，免 C 工具链；本库写入量极小，性能差异可忽略。
- **影响**：go.mod 引入 modernc.org/sqlite；构建命令保持 `go build ./...` 无特殊环境要求。

## D13 ｜ 2026-09-17 ｜ 配置形态：YAML + flags + ENV

- **决策**：`config.yaml` 承载主体配置，同名 CLI flag 覆盖文件值，密码支持 `${ENV_VAR}` 从环境变量展开。
- **原因**：多 resolver / 过滤列表等复杂结构用文件表达；flags 便于 cron 临时覆盖；密码不入库且不必明文落盘。
- **影响**：仓库只提供 `config.example.yaml`，真实 `config.yaml` 忽略；用户提供的 config.yaml 是 D11 的实测来源。

## D14 ｜ 2026-09-17 ｜ 域名归并默认一级域混合 + 证据不足自动回退精确（部分修订 D6）

- **背景**：D6 的"默认逐 host 精确"在真实日志下规则条数爆炸（同一站点十几个子域各一条）；而全量 zone 通配又有污染风险。
- **决策**：`aggregate.mode` 默认 `zone`：按公共后缀表把达阈 host 归并到可注册域后**逐 zone 混合决策**（`aggregate.ResolveTargets` 纯函数）：
  1. zone 内仅 1 个达阈 host → 单条精确（裸域或子域；单 host 证据不足以断言整个 zone，且避免通配误伤未观测子域如 `mx`）；
  2. zone 内 ≥2 个达阈 host **全部** confirmed 且 `sync.wildcard: true` → `zone` + `*.zone` 两条；
  3. 混入 maybe / not_cf 达阈 host（或禁用通配）→ 该 zone 回退为逐 confirmed host 精确条目；
  4. verdict 缺失按 not_cf 保守处理；`maybe` 一律不入目标；`aggregate.mode: exact` 整体退回逐 host 精确。
- **原因**：归并只在"同一 zone 下多个独立 host 都被探测确认"时发生，等于用探测证据给通配背书；任何证据混杂立即回退，防污染原则与 D6 一致。
- **影响**：修订 D6 结论（D6 的 exact 路径保留为 `aggregate.mode: exact`）；目标条目输出按 Domain 字典序稳定排序；通配条目以 `*.` 前缀域名参与 planner diff 与 AGH rewrite（真实实例行为待实测，见计划 §7）。

## D15 ｜ 2026-09-17 ｜ 状态库容量护栏：domains ≤1000、runs ≤60，超限按 last_seen 淘汰最旧

- **背景**：CLI 长期无人值守运行，状态库若无限增长会在 NAS / 路由器小盘设备上出问题。
- **决策**：查询日志原文永不入库；`domains` 上限 1000 行（`aggregate.max_domains`，兼作探测预算上限）、`runs` 只留最近 60 次；rewrites / probes 与目标集合对齐不单独设限。超限按 `last_seen` 淘汰最旧行并打 slog warn。
- **原因**：域名统计和运行审计是仅有的两张"只增"表，last_seen 淘汰语义自然（久未出现的域名本就该被遗忘）；库体积预期 <5MB。
- **影响**：清理逻辑集中在 state 包启动时执行；被淘汰域名若再次出现按新域名重新累计。

## D16 ｜ 2026-09-17 ｜ CF 判定自研三信号，cdncheck 不进依赖、仅作人工核对工具

- **背景**：`cdncheck`（projectdiscovery）内置大量 CDN 网段可作现成判定库，但引入后判定逻辑黑盒化且依赖膨胀。
- **决策**：CF IP 段判定用官方端点自研（CIDR 信号 ~40 行，打分设计不变）；cdncheck 二进制仅作人工交叉核对工具，不作为流水线组件、不进 go.mod。
- **原因**：本工具只判 CF 一家，官方 `client/v4/ips` 端点 + 本地缓存足够且可审计；三信号打分是核心逻辑，必须自己掌控。
- **影响**：go.mod 不出现 cdncheck；需要交叉核对时手动运行其二进制比对结果，差异登 pitfalls。

## D17 ｜ 2026-09-17 ｜ 优选 IP 默认来源 B（只读 CFST 结果文件），release 二进制随 CFST 目录部署，MVP 不做自动测速

- **背景**：原设计默认来源 A（解析优选域名），来源 B 含"调用外部二进制测速 + 读结果文件"两步；实际使用习惯是用户自行在 CFST 发布目录手动测速，且自动测速耗时长、参数与 IP 段文件管理复杂。
- **决策**：
  1. `cfip.source` 默认由 A 改为 **B**：默认值 `cfst:result.csv`（相对于进程工作目录）；A 优选域名 / C 静态列表保留，但必须显式配置才启用。
  2. **部署约定**：用户把 release 的 `cf-opt-adguard` 二进制放入 CFST 发布目录（与 `cfst` 可执行文件、`result.csv` 同级，如 `references/CloudflareSpeedTest-master/releases/cfst_linux_amd64/` 的目录形态）执行；测速由用户自行运行 CFST 产出 `result.csv`，本工具只读取。
  3. **MVP 不实现自动测速**：不 exec / 拉起 `cfst` 二进制，不拼测速参数；自动调用测速 / 定时调度测速仍为 P1（PRD §5.2）。
- **原因**：同目录部署使默认路径零配置；读 CSV 是稳定、可离线夹具测试的外部契约；自动测速与本工具核心链路解耦，推迟不影响 MVP 价值。
- **影响**：部分修订 D8；PRD F4 / §6、ARCHITECTURE §3 / §4.4、API §6 / §7.2、DATA_MODEL §4（`cfip.source` 默认值）同步；`config.Default()` 的 `cfip.source` 需在实现时设为 `cfst:result.csv`；`result.csv` 缺失 / 只有表头仍按"优选 IP 为空"安全中止（退出码 2，不写不删）；开发期以 `references/` 下真实 `result.csv` 作只读夹具，拷贝进各包 `testdata/` 使用。
