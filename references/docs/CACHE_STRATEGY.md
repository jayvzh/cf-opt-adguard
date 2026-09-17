# 缓存调用与图片缓存生成策略（CACHE_STRATEGY）

本文是图片缓存体系的调度总纲：双端缓存模型、服务端生成调度、前端预加载调度、
目录空闲预热（DIRWARM）、缩略图加载通道与后台全库预热（CacheWarmer）的分工与优先级。

关联文档：缓存键与失效语义见 [DATA_MODEL.md](DATA_MODEL.md) §3；HTTP 端点契约见
[API.md](API.md) §3.3 / §3.5 / §3.10a；总体架构见 [ARCHITECTURE.md](ARCHITECTURE.md) §4.3。

---

## 1. 双端缓存模型

```
浏览器                                   服务器
┌─────────────────┐   immutable 命中    ┌──────────────────────────┐
│ HTTP 磁盘缓存    │ ◄────────────────► │ /data/cache/{thumb,preview}/
│ (URL 含 v 参数)  │   未命中才回源      │ sha1(路径|mtime|size|变体|桶).jpg
└─────────────────┘                    └──────────────────────────┘
```

- **服务器缓存**：内容寻址——原图 mtime/size 编码进缓存文件名，原图一变键即变、
  必然 miss 重新生成；旧文件成孤儿由清理机制回收。命中判定 = 文件存在（键已编码新鲜度）。
- **浏览器缓存**：preview/thumb 产物响应 `Cache-Control: public, max-age=31536000, immutable`
  + URL 版本参数 `v={mtime}{size}`——重访同 URL 0 网络请求；preview 生成失败回退原图的
  响应 `no-cache` + ETag 再校验（内容随生成恢复而变化，不可长缓存）。
- 预加载一律经 `fetch` 完整读入响应体，确保资源完整落入 HTTP 缓存；显示端 `<img>` 同 URL
  直接命中。前端不自建内存 Map 缓存（HTTP 缓存已覆盖，避免双份内存占用）。

## 2. 服务端生成调度器（backend/internal/thumbnail/scheduler.go）

所有 libvips 生成任务（缩略图/预览图）经全局 Scheduler 排队执行：

- **并发槽**：上限 `THUMB_CONCURRENCY`（默认 2），限制同时执行的大图解码数，
  避免 NAS 磁盘 IO 被打满；`VIPS_CONCURRENCY` 与之配平（乘积 ≈ 可用核数）。
- **双优先级 FIFO**：High = 用户正在看的图；Low = 前端空闲预热与后台预热。取队时
  High 队列清空才轮到 Low。
- **排队中取消**：任务携带请求 ctx，翻页/切目录即丢弃排队任务（已开工的不可中断，
  跑完落盘的结果对后续等待者依然有效）。
- **队列上限 256**：防止异常客户端无限堆积（前端预热已窗口化/限并发，正常远达不到）。

### 生成链路的关键去重（backend/internal/service/thumbnail_service.go）

| 机制 | 语义 |
| --- | --- |
| singleflight（inflight 表） | 同缓存键并发生成只有一个执行者，其余等待后直接读缓存文件 |
| 缓存命中短路 | `IsFresh` 命中直接开文件返回，不入队不生成 |
| 高撞低双跑 | 高优先级请求撞上在飞低优任务时，不等它（等于把用户请求降级到它的队列位置）——以自身优先级双跑一次，多付一次生成换用户即时响应 |
| 负缓存（60s TTL） | 生成失败的同键请求快速失败，不再触发注定失败的解码；取消类错误不入缓存 |
| thumb ← preview 派生 | thumb 生成优先从同源新鲜 preview 派生（小解码，不再读原图）；缺失或失败回退原图生成 |
| 趁热级联 | thumb 从原图生成成功后，异步低优补 preview（原图仍在 page cache，只花 CPU） |

## 3. 前端预加载调度器（frontend/src/utils/imagePreloader.ts）

模块级单例（非 Pinia store，遵守 store 白名单铁律）。URL 级队列，并发上限 4
（服务端生成为瓶颈，4 并发仍在浏览器每主机 6 连接限制内，余量留给 `<img>`）。

### 3.1 优先级阶梯（数值小者先执行；pump 按优先级选队首，同档 FIFO）

| 层级 | 值 | 内容 | 触发 |
| --- | --- | --- | --- |
| 当前帧 | （不经调度器） | 当前帧 preview 由 `<img>` 原生加载（浏览器最高优先级渲染资源，不可取消） | 换帧 |
| ADJACENT | 1 | 跳转目标左右相邻帧 preview，fetch `priority: high` | 换帧 `bumpToTop` |
| WARMUP | 2 | 查看器滑窗 `[i-4, i+40]` preview（>50MP 跳过；宽高缺失不过滤） | 换帧重建 |
| DIRWARM | 3 | 当前目录前缀 preview + thumb（见 §4） | 目录列表就绪 / 查看器开关 / 解锁 |

非 ADJACENT 档请求一律携带 `X-Load-Priority: low` → 服务端低优生成（见 API.md §3.3/§3.5）。

### 3.2 去重与抢占

- **去重**：`completed`（已成功预取的 URL）+ `inflight` + 队列内查重，三重命中即跳过；
  `cancelAll`（切目录 / 关查看器）清空三者，重开可全量预热（immutable 下由浏览器
  磁盘缓存直接命中，无网络请求）。
- **抢占（往后排）**：换帧时 `bumpToTop` 把相邻帧插队首，并 abort 在飞的非目标
  WARMUP/DIRWARM 任务（标记 preempted，结束后按原优先级重排队尾）——服务端排队中
  任务随请求取消丢弃，已开工的照常完成落盘不白做。
- **提权（重复任务语义）**：用户点击的图若已在低档队列 → 从队列移除后插到队首
  （`bumpToTop` 既有语义），**不是**"先加后去重"（保留旧位置等于没提权）；
  在飞同 URL 不打断（不浪费已下载字节，服务端 singleflight/双跑兜底）。

## 4. 目录空闲预热 DIRWARM（新增）

用户停留在某目录且更高档无排队任务时，以最低档预热当前目录（`syncDirWarm`）：

- **范围**：按当前排序序（目录 API 返回的列表序）——
  - 阶段 A：前 100 张 preview（打开即看的瓶颈资源；>50MP 跳过）；
  - 阶段 B：前 100 张 thumb（服务端从阶段 A 的新鲜 preview 派生，每图只解码一次原图；
    不做像素过滤）。
- **解锁（会话级，内存 Set）**：用户在本目录查看第 101 张及以后（索引 ≥ 100，
  桌面取帧内图片索引 max、移动端帧索引即图片索引）→ 解锁该目录，阶段 A/B 均放开为
  **全量 preview + 全量 thumb**（预览已全量生成，thumb 派生只付小解码，胶片条任意
  位置秒开）。不落 localStorage：库内容会变化，持久化解锁易失真；重新深翻一次即再解锁。
- **重建时机**：目录列表变化 / 收藏夹视图切换 / 查看器开关（后两者伴随 `cancelAll`
  清空，需重挂）。重建语义同 WARMUP：丢弃旧未开始任务按新清单重排，在飞不打断。
- **不启用**：收藏夹虚拟视图（跨目录混合列表，无"当前目录"语义）。
- **与查看器档重叠**：WARMUP 窗口内的 preview 与 DIRWARM 阶段 A 重叠部分由去重集合
  自然跳过。

## 5. 缩略图加载通道（与 preloader 分离）

| 场景 | 通道 | 服务端优先级 |
| --- | --- | --- |
| 胶片条/移动网格可见与近视口（虚拟渲染 ±3 overscan 挂载） | 原生 `<img loading="lazy">` | High（用户可见，浏览器全权调度） |
| 远离视口 | 虚拟渲染不挂载 + lazy 不触发，不加载 | — |
| DIRWARM 阶段 B（投机预热前 100 / 解锁全量） | preloader fetch + `X-Load-Priority: low` | Low |

缩略图的"ADJACENT 等价物"即**虚拟渲染窗口 + 原生 lazy**：可见即高优、不可见不加载。
可见缩略图本就必须由 `<img>` 显示（预取只是提前触发同一 URL），故不纳入 preloader，
职责清晰：**preloader 只管投机预热，浏览器管可见加载**。可见缩略图与 DIRWARM 缩略图
URL 相同（同 dpr 桶 200/300/500），HTTP 缓存天然去重。桌面文件模式网格为纯文字卡片，无缩略图。

## 6. 后台全库预热 CacheWarmer（backend/internal/service/cache_warmer.go）

按 warm 级别（设置页可改、持久化 app_settings、env `THUMB_WARMER=0` 总关）逐目录预热：

| 级别 | 每目录 thumb | 每目录 preview |
| --- | --- | --- |
| off | 0 | 0 |
| minimal | 20 | 5 |
| level1 | 100 | 50 |
| level2 | 300 | 150 |
| full | 不限 | 不限 |

- 目录按修改时间降序（新入库优先）；目录内**先 preview 前缀后 thumb 前缀**
  （thumb 从新鲜 preview 派生，每图一次原图解码）。
- 严格单任务串行（至多占一个生成槽，另一个留给前台）。
- **双静默门槛（30s，共用 `warmIdleThreshold`）**：
  1. 高优先级任务静默 ≥ 30s（用户不在浏览）；
  2. 前端低优流量（`X-Load-Priority: low` 请求，即 WARMUP/DIRWARM）静默 ≥ 30s。
  两者同时满足才推进——同为低优先级会在队列与磁盘 IO 上交错，必须等前端预热完成，
  落实优先级总表的"后台构建垫底"。打点只在 handler 层记录（`NoteFrontendWarmTraffic`），
  不能挂在 `Submit(PriorityLow)` 上：预热器自身也提交低优任务，会把自己挡死。

## 7. 优先级总表

```
用户可见请求（当前帧 <img> / 可见缩略图 lazy <img>）      服务端 High
─────────────────────────────────────────────────────
前端空闲预热（ADJACENT → WARMUP → DIRWARM，fetch low）    服务端 Low
后台全库预热 CacheWarmer（等双静默后推进）                 服务端 Low（垫底）
```

同一 Low 档内的先后由"CacheWarmer 等 DIRWARM 静默"保证：用户停留在目录期间
DIRWARM 持续占用低优流量 → CacheWarmer 暂停（phase=paused）；DIRWARM 完成（或用户
关掉页面）并静默 30s 后，后台构建才推进。用户任何浏览活动（高优请求）随时把
CacheWarmer 打回 paused。

## 8. 已知取舍

- `cancelAll` 清空 `completed` 后重挂 DIRWARM：重复 fetch 由浏览器 immutable 缓存
  直接命中（0 网络），代价可忽略。
- 解锁后超大目录（万张）的 DIRWARM 会持续占用低优槽数小时，期间 CacheWarmer 让位
  ——符合"当前目录优先"的意图；关闭页面即恢复。
- 快速连续切图时在飞 DIRWARM 被反复 abort 重排：服务端排队任务丢弃、已开工完成落盘，
  重取时命中服务端缓存，不白做。
- 服务端队列上限 256 对 DIRWARM 免疫：前端 4 并发限流 → 服务端至多 ~5 个低优任务在队。
