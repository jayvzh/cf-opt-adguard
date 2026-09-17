# 踩坑与绕开姿势（pitfalls.md）

> 格式：标题 + 现象 / 原因 / 解决 / 备注。只收非显而易见、可能再踩的坑；通用常识不写。
> M1–M4 已实现（对真实 AGH 实例的 live 联调尚未开始），以下条目来自官方源码 / 参考仓库的**一手取证**（非实跑踩坑），M5–M6 联调时必须逐条复核并补充实测现象。
> 更新时机：实测与文档不一致、踩了新坑时。

---

## 1. AGH querylog 分页参数随版本变化

- **现象（取证）**：AGH master（2026-09 核实）`GET /control/querylog` 用 `limit` + `offset` 或 `older_than`（RFC3339Nano）分页，过滤参数是可重复的 `reason`，旧的 `response_status` 标记为 deprecated 且与 `reason` 互斥；而 v0.107 等旧版用 `startTime` / `endTime`（Unix 毫秒）+ `response_status`。
- **原因**：AGH 查询日志在版本迭代中重写过搜索参数模型。
- **解决**：P0 只实现新版参数（D11），不做能力探测与旧版分支；翻页用 `older_than` 向前取数，终止条件为"空页或已早于窗口起点"。参数构造集中在 `adguard` 客户端一处。
- **备注**：来源 `internal/querylog/http.go` 与 `openapi/openapi.yaml`；已决策 P0 只支持新版（D11），不做旧版兼容层；开工时用用户 `config.yaml` 指向的实例只读实测并固化本文件字段。

## 2. rewrite 条目新版带 `enabled`，update 是 target/update 两段式

- **现象（取证）**：master 版条目 JSON 为 `{domain, answer, enabled}`；修改端点 `POST /control/rewrite/update` 体为 `{"target":{...},"update":{...}}`，不是整条覆盖；旧版条目只有 `{domain, answer}`。
- **原因**：AGH 后续版本给 rewrite 增加了启停开关。
- **解决**：结构体对 `enabled` 做可选容忍（响应缺省按 true 处理）；update 严格按 target/update 组包；list 到的原始对象在状态库存档，便于差异排查。
- **备注**：来源 `internal/filtering/rewritehttp.go`。

## 3. `/control/filtering/set_rules` 是全量覆盖，不能当增量接口用

- **现象（取证）**：参考脚本读 `/control/filtering/status` 的 `user_rules` 数组 → 改其中一条 → `POST /control/filtering/set_rules` 整包写回。
- **原因**：该接口语义是"用传入数组替换全部用户自定义规则"。
- **解决**：本工具完全不走此通道，只用条目级 rewrite API（决策 D3）；评审时看到任何 `set_rules` 调用一律打回。
- **备注**：来源 vendored `CloudflareSpeedTest-Adguard-Script-main/main.go`。两套机制在 AGH 内可能同时生效，若用户手工在 user_rules 里写过 hosts 式 `ip domain` 规则，排查"为什么有两条答案"时先查这里。

## 4. 登录是会话 Cookie，不是 Bearer Token

- **现象**：参考脚本先 `POST /control/login`（body 键名是 `name` 不是 `username`）拿到 Cookie 再请求后续接口。
- **原因**：AGH 管理 API 使用自有会话机制。
- **解决**：优先直接用 HTTP Basic Auth（AGH 同样接受，实现最简单）；走登录时记住 CookieJar，401 只允许重登一次，且仅重试只读请求。
- **备注**：来源参考脚本 main.go；两种方式在目标实例上各做一次实测确认。

## 5. CFST 结果文件是带中文表头的 CSV，首数据行才是最优 IP

- **现象（取证）**：默认输出 `result.csv`，表头固定为 `IP 地址,已发送,已接收,丢包率,平均延迟,下载速度(MB/s),地区码`；数据先按丢包率 / 延迟排序再按下载速度排序。参考脚本用 `-o result.txt` 自定义文件名并读第二行第一列。
- **原因**：CFST 排序后直接导表，不带机器可读的"最优 IP"字段。
- **解决**：解析器跳过表头、按列索引 0 取 IP，不硬依赖文件名与中文表头内容；只有表头无数据 = 优选为空，安全中止本次写入。
- **备注**：来源 vendored `utils/csv.go`。MVP 不调用 `cfst` 二进制，用户需先自行运行测速产出 `result.csv`；推荐把本工具 release 二进制放入 CFST 发布目录执行，默认读 `./result.csv`（D17）。文件缺失（典型原因：放进目录后还没跑过测速）同样按"优选为空"中止并提示用户先测速；自动调用测速为 P1。

## 6. 探测走系统 resolver 会被自己的重写污染

- **现象**：若主机默认 DNS 指向正在被本工具管理的 AGH，CNAME / A 结果可能已是 rewrite 后的优选 IP。
- **原因**：循环印证——用自己写入的结果证明域名属于 CF。
- **解决**：detector / ipselector 一律走配置的独立 resolver（D5）；`detector.resolvers` 缺失时配置校验直接报错，不回退系统默认。
- **备注**：源自 PRD §4 的原始设计约束。

## 7. 配置文件里的明文密码

- **现象**：参考仓库的 `config.yaml` 直接写明 `username/password: admin/admin`。
- **风险**：照抄习惯会把 AGH 凭据提交进库。
- **解决**：仓库只提供 `config.example.yaml`；真实 `config.yaml` 加入忽略（git init 时配 `.gitignore`）；支持 `${ENV_VAR}` 展开从环境取密码；日志对密码与 Cookie 脱敏。
