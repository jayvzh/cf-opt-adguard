# cf-opt-adguard 接口契约（API.md）

> 版本：v0.1 ｜ 状态：设计稿。本工具是纯 CLI 客户端，**没有自己的 HTTP API**；本文记录它依赖的外部 HTTP / DNS 契约与它对外提供的 CLI 契约。
> 本文是外部接口相关代码的唯一契约来源：实现中的 URL、参数、响应字段必须与本文一致；与实测不一致时先改本文（并在 [pitfalls.md](pitfalls.md) 登记差异），再改代码。
> 相关：调用机制 [ARCHITECTURE.md](ARCHITECTURE.md)；客户端落位 [PROJECT_STRUCTURE.md](PROJECT_STRUCTURE.md)。
> 更新时机：目标 AGH 版本变化、端点 / 字段 / CLI flags 变化时。

---

## 1. 通用约定与证据来源

### 1.1 取证来源（2026-09-17 核实）

| 事实 | 来源 |
| --- | --- |
| rewrite 载荷为 `{domain, answer, enabled}`，update 为 `{target, update}` | AGH master 源码 `internal/filtering/rewritehttp.go` |
| querylog 新版参数 `limit/offset/older_than/search/reason`，`response_status` 已弃用 | AGH master 源码 `internal/querylog/http.go`、`openapi/openapi.yaml` |
| 登录 `POST /control/login` + Cookie 会话 | vendored 参考脚本 `references/CloudflareSpeedTest-Adguard-Script-main/main.go` |
| CFST 结果文件表头与默认文件名 | vendored 源码 `references/CloudflareSpeedTest-master/utils/csv.go` |

> ⚠️ AGH 接口在版本间有差异（见 [pitfalls.md](pitfalls.md) 第 1、2 条）。`adguard` 客户端必须以**目标实例实测**为准做兼容，禁止只凭本文或网络资料发版。

### 1.2 Base URL 与认证

- Base URL：用户配置的 AGH 根地址（如 `http://192.168.1.2:3000`），所有端点前缀 `/control`。
- 认证二选一（实现时二者都支持，默认 Basic）：
  - HTTP Basic Auth：`Authorization: Basic base64(user:pass)`；
  - 会话：先 `POST /control/login`（body `{"name":"<user>","password":"<pass>"}`），持响应 Cookie 访问后续接口。
- 任何认证失败（401/403）不得重试写操作，直接中止并报错。

## 2. 查询日志：`GET /control/querylog`

### 2.1 请求参数

| 参数 | 版本 | 说明 |
| --- | --- | --- |
| `limit` | 全部 | 单页条数 |
| `offset` | 新版 | 分页偏移；与 `older_than` 建议二选一 |
| `older_than` | 新版 | RFC3339Nano 时间，取早于该时间的记录，向前翻页 |
| `search` | 新版 | 按域名或客户端 IP 过滤 |
| `reason` | 新版 | 按过滤原因筛选，可多次传参；与 `response_status` 互斥 |
| `response_status` | 旧版（新版弃用） | 枚举：`all`/`filtered`/`blocked`/`blocked_safebrowsing`/`blocked_parental`/`whitelisted`/`rewritten`/`safe_search`/`processed` |
| `startTime` / `endTime` | 旧版（v0.107 等） | Unix 毫秒时间窗口 |

采集器策略（P0 仅新版，D11）：用 `reason=NotFilteredNotFound`（可再加 `NotFilteredAllowList`）取正常放行记录，`limit` + `older_than` 向前翻页直至覆盖时间窗口；不实现旧版 `startTime/endTime` 与 `response_status` 降级路径。参数集合格式化只在 `adguard` 客户端一处维护；开工后以用户 `config.yaml` 指向的实例只读实测，把下表字段与响应结构固化回填。

### 2.2 响应（关键字段，以实测为准）

```json
{
  "oldest": "2026-09-10T00:00:00Z",
  "data": [
    {
      "question": { "host": "assets.example.com", "type": "A", "class": "IN" },
      "answer": [
        { "type": "A", "value": "104.16.1.2", "ttl": 60, "name": "assets.example.com" }
      ],
      "reason": "NotFilteredNotFound",
      "client": "192.168.1.50",
      "time": "2026-09-17T08:00:00.000Z",
      "elapsedMs": "12.3"
    }
  ]
}
```

消费约定：

- 只消费 `question.host` / `question.type`（A、AAAA）/ `reason` / `client` / `time`；
- `answer` 不作为 CF 判定依据（它可能已被本工具的重写污染，判定一律走 detector 独立解析）；
- 未知字段一律忽略，保证对 AGH 版本演进向前兼容。

## 3. DNS 重写：`/control/rewrite/*`

重写条目对象（新版含 `enabled`，旧版仅前两字段，客户端按响应原样容忍）：

```json
{ "domain": "assets.example.com", "answer": "104.16.1.1", "enabled": true }
```

| 方法 / 路径 | 请求体 | 用途 |
| --- | --- | --- |
| `GET /control/rewrite/list` | — | 获取全部重写，响应为对象数组 |
| `POST /control/rewrite/add` | `{domain, answer, enabled?}` | 新增单条 |
| `POST /control/rewrite/update` | `{"target":{domain,answer},"update":{domain,answer,enabled?}}` | 修改单条（按 target 定位） |
| `POST /control/rewrite/delete` | `{domain, answer}` | 删除单条 |

调用约定：

- `answer` 只允许写**解析后的裸 IP**（IPv4 / IPv6 文本）；禁止写优选域名或 `$dnsrewrite=` 语法（见 [decisions.md](decisions.md) D4）。
- 同域名不同地址族 = 不同条目（domain+answer 共同标识一条）。
- 写操作限速、重试、单条失败隔离，机制见 [ARCHITECTURE.md](ARCHITECTURE.md) §4.6。
- **不使用** `/control/filtering/set_rules` 通道（它全量覆盖用户规则，原因见 [pitfalls.md](pitfalls.md) 第 3 条）。

## 4. 登录：`POST /control/login`

```json
{ "name": "admin", "password": "********" }
```

成功后通过 `Set-Cookie` 建立会话。仅当未使用 Basic Auth 时调用；会话过期（401）允许重新登录一次后重试原只读请求，写请求不自动重试。

## 5. Cloudflare 官方 IP 段

| 端点 | 响应 |
| --- | --- |
| `GET https://api.cloudflare.com/client/v4/ips` | JSON：`result.ipv4_cidrs[]` / `result.ipv6_cidrs[]` |
| `GET https://www.cloudflare.com/ips-v4` | 纯文本，每行一个 CIDR（备用） |
| `GET https://www.cloudflare.com/ips-v6` | 纯文本，每行一个 CIDR（备用） |

- 启动时拉取，落本地缓存文件；拉取失败且缓存存在时用缓存，两者皆无则 detector 的 CIDR 信号降级为缺失（不单独决定结论）。

## 6. CloudflareSpeedTest 集成契约

- 本工具不实现测速：按需调用外部 `CloudflareSpeedTest` 二进制（路径 / 参数可配），等待退出后读取结果文件。
- 默认输出 `result.csv`（vendored 源码取证），首行中文表头，其后按丢包率 / 延迟、下载速度排序，**第二行第一列即最优 IP**：

```text
IP 地址,已发送,已接收,丢包率,平均延迟,下载速度(MB/s),地区码
1.0.0.1,4,4,0.00,12.34,15.67,LAX
```

- 解析器必须跳过表头、按列索引取 IP；文件不存在 / 只有表头视为"优选 IP 为空"，按 [ARCHITECTURE.md](ARCHITECTURE.md) §7 中止写入。
- 参考脚本用 `-o result.txt` 自定义输出名，默认名与列以实际 CFST 版本 `--help` 为准。

## 7. CLI 契约（本工具对外接口）

> 设计稿，实现后以 `cf-opt-adguard --help` 实测回填；flags 与 [DATA_MODEL.md](DATA_MODEL.md) §4 配置键一一对应。

### 7.1 子命令

| 命令 | 说明 |
| --- | --- |
| `run` | 执行一次完整流水线（P0 唯一核心命令） |
| `version` | 打印版本 |

### 7.2 `run` 主要 flags

```bash
# 默认即 dry-run：只读采集 + 出计划，不写 AGH（新配置首次运行、定时任务核对都用它）
cf-opt-adguard run \
  -c config.yaml \
  --agh-url http://192.168.1.2:3000 \
  --agh-user admin \
  --agh-pass '********' \      # 或配置中 ${AGH_PASSWORD} 环境变量展开
  --window 7d \
  --min-hits 20 \
  --cfip-source domain:cfip.yyyyt.top \
  --log-level info

# 核对计划无误后，显式 --apply 才真正写入
cf-opt-adguard run -c config.yaml --apply
```

- 同名 flag 覆盖配置文件值（D13）。
- **默认 dry 模式**：只产出计划，绝不调写接口；显式 `--apply` 进入 live 模式执行写入（D10）。cron / systemd timer 任务必须显式带 `--apply`。
- 计划输出固定包含：采集条数、候选数、confirmed 数、优选 IP、add/update/remove 计数与逐条 `domain → answer` 清单，以及 `[DRY-RUN]` / `[APPLY]` 模式标识。

### 7.3 退出码（计划）

| 码 | 含义 |
| --- | --- |
| 0 | 成功（live 全部条目成功；dry-run 正常产出计划） |
| 2 | 配置 / 参数错误（含必填缺失、优选 IP 为空前置失败） |
| 3 | 采集 / 探测阶段失败导致未进入同步（AGH 不可达等），未做任何写操作 |
| 4 | 同步执行但存在失败条目（部分成功，详情见 runs 记录与日志） |
