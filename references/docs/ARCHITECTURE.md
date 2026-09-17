# AlbumShelf・NAS图集馆 技术架构设计

> 版本：v1.0 ｜ 依赖文档：PRD.md

---

## 1. 技术选型（已锁定）

| 层 | 技术 | 理由 |
| --- | --- | --- |
| Backend | **Go 1.24+** | 单二进制、NAS 友好、性能好、文件扫描简单、Docker 简单 |
| Web 框架 | **Gin** | 成熟、生态丰富、稳定、AI IDE 熟悉 |
| Frontend | **Vue 3 + TypeScript + Vite** | Viewer/Filmstrip/Split View 类 UI 开发体验好 |
| 状态管理 | **Pinia** | Vue3 官方推荐 |
| 样式 | **Tailwind CSS + Headless UI** | 图片浏览器需要高度自定义，不绑定重型组件库 |
| 图片处理 | **libvips（Go 绑定：govips）** | 大量图片缩略图生成性能/内存占用最优（LibrePhotos 同方案） |
| 数据库 | **SQLite** | 无需独立数据库服务、Docker 简单、NAS 友好、备份简单 |
| 部署 | **Docker / docker compose** | NAS 标准部署方式 |

> MVP 第一阶段甚至可以不依赖数据库（直接扫描目录 + 返回 API），SQLite 从 Sprint 4（文件夹设置）起启用。

## 2. 总体架构

```
                Browser
                   │
                   ▼
            Vue 3 Frontend
                   │
                REST API
                   │
                   ▼
               Go Backend
                   │
      ┌────────────┼────────────┐
      ▼            ▼            ▼
 File Service   Sort Engine   Thumbnail
      │            │            │
      │            │         libvips
      │            │            │
      └────────────┼────────────┘
                   ▼
               Storage
                   │
      ┌────────────┼────────────┐
      ▼            ▼            ▼
   Images     Thumbnails      SQLite
```

## 3. 三层架构原则

整个后端明确分为三层，模块只能向下依赖：

```
Layer 3 交互层：API（Handler/Router） + 前端 Vue/Store/Component
Layer 2 业务层：Service、Sorting（排序引擎）、Thumbnail（缩略图）
Layer 1 文件层：Filesystem（扫描、Metadata、路径安全） + Repository（SQLite）
```

- 未来增加 WebDAV / S3 / NAS Remote Storage → 只扩展 Filesystem Layer
- 未来增加 Spread（双页）/ Favorite / Tag / AI Search → 不影响 Filesystem Layer

## 4. 核心设计决策

### 4.1 图片文件不入数据库

数据库只保存：配置、缓存索引、用户数据。图片始终留在文件系统（NAS 目录）。

### 4.2 三级图片加载策略

```
Original（原图）    → 100% 查看、高倍缩放、按需加载（用户点击"加载原图"）
Preview（预览图）    → Viewer 默认显示（长边约 1920px，JPEG q80）
Thumbnail（缩略图）  → Filmstrip（200px）
```

典型场景：同一目录 20-300 张图、多次频繁访问，且原图常有十几 MB 不适合直接网页浏览。因此：

- **Viewer 默认加载 Preview，禁止默认加载原图**；原图仅在用户点击"加载原图"时按需加载
- Preview 与 Thumbnail 共用 govips 生成管线，**Preview 提前进 MVP**（Sprint 3，与 Thumbnail 同 Sprint 实现）
- Preview 采用**渐进式 JPEG**（libvips `Interlace`，SOF2 码流）：弱网/大图首屏边传边显；Thumbnail 保持 baseline（小图渐进编码体积反增）
- 翻页/重访时缩略图与预览图优先命中缓存（见 §4.3）；原图永远不自动加载，仅在用户点击"加载原图"时按需请求（重复点击同一张直接命中浏览器缓存）

### 4.3 双端缓存策略（服务器持久缓存 + 浏览器 HTTP 缓存）

**服务器端（持久化，DATA_DIR，容器/进程重启不丢）：**

- 缓存文件名：`hash(source_path + mtime + size + variant + 长度)`，variant ∈ `thumb | preview`
- 存放于 `/data/cache/`（thumb / preview 分子目录），SQLite cache 表做索引与失效判断
- 失效验证：原图 mtime/size 与缓存记录不一致 → 重新生成
- 原图不做服务器缓存（直接文件流）

**浏览器端（HTTP 缓存，URL 版本化）：**

- 所有图片 URL 携带版本参数 `v={mtime}{size}`（如 `/api/v1/image?path=...&variant=preview&v=17345678901234567`），服务端忽略该参数，仅用于缓存失效
- 全部图片响应返回 `Cache-Control: public, max-age=31536000, immutable` + `ETag`（mtime+size）
- 效果：首次访问后缩略图/预览图进入浏览器磁盘缓存，**重访同目录时 0 网络请求直接显示**；原图**不自动显示**——每次会话 Viewer 一律默认预览图，仅当用户点击"加载原图"时才请求原图，若该图本次浏览器会话中已加载过则直接命中磁盘缓存、无重新下载；原图被修改 → mtime 变 → 列表返回新 URL → 浏览器自动视为新资源
- 兜底链：浏览器缓存被驱逐 → 重新请求 → 服务器持久缓存秒回（thumb/preview）；原图仅重新下载用户主动查看的那几张

**禁止**：对不带版本参数的图片 URL 发 immutable 头；前端为缩略图/预览图自建内存 Map 缓存（HTTP 缓存已覆盖，避免双份内存占用）。

### 4.4 排序在后端完成

排序统一由后端 Sort Engine 执行（`GET /api/v1/folders?path=...&sort=...`），理由：万级文件目录下前端排序不可持续，且后端统一负责 Filter / Sort / Metadata 更合理。前端只保留 Regex Editor 的实时 Preview（调 `POST /api/v1/sort/preview`）。

### 4.5 Viewer 自研封装而非套用成熟库

核心功能（Transform / Scale / Pan / Fullscreen）自研 Viewer Layer：

- 双页 Spread 自动拼帧（等高对齐、横图独占、阅读方向）需要精细控制布局
- 成熟图片组件（PhotoSwipe 等）反而会限制架构

结构上抽出底层 `ViewerCanvas` 组件，Single 模式与 Spread 双页模式复用同一画布，双页只是"帧内容 = 1~2 张图"的差异（详见 SPREAD_ENGINE.md §3.4）。

### 4.6 Filmstrip 动态布局 + 虚拟渲染

Filmstrip 使用虚拟渲染（自研或 vue-virtual-scroller），只渲染视口附近缩略图，支撑 10000+ 图片目录。

**可见数量永远动态计算，禁止硬编码固定数量。** 由于缩略图"等高不等宽"（宽度按原始宽高比缩放），不能用"固定项宽 × 索引"定位，必须用前缀和偏移：

```
显示高度 THUMB_H（常量，如 96px，与生成尺寸 200px 解耦）
itemWidth(i)  = clamp(THUMB_H × (w_i / h_i), MIN_W, MAX_W)
GAP（常量，如 8px）
offset(i)     = Σ (itemWidth(k) + GAP), k < i     // 前缀和数组，滚动 O(1)、构建 O(n)
```

- 可见区间 = 在 `offset[]` 上二分查找 `scrollLeft` 与 `scrollLeft + viewportWidth` 覆盖的项（± overscan）
- `viewportWidth` 由 **ResizeObserver** 实时测量容器，窗口缩放/侧栏折叠自动重算可见数量
- 自动居中：`scrollTo(offset(i) - (viewportWidth - itemWidth(i)) / 2)`
- 纯计算逻辑放 `useFilmstrip.ts`（或独立纯函数），配合 utils 可单测

### 4.7 双页 Spread 引擎（前端纯函数）

双页拼凑（Spread）是阅读体验功能，不是对比工具，逻辑参考 NeeView 見開き模式（详见 SPREAD_ENGINE.md）：

- 横图判定：`width > height × WideRatio`（默认 1.0）→ 横图独占一帧
- 竖图与相邻竖图两两拼双页，等高对齐
- 引擎是前端纯函数 `utils/spread.ts`：输入图片列表（含尺寸）→ 输出帧序列 + 图片↔帧索引映射
- 翻页以帧为单位；Filmstrip 仍以图片为单位联动高亮
- 依赖图片尺寸：**后端扫描时读取图片文件头**（Go `image.DecodeConfig`，不解码全图），随目录列表返回 `width/height` —— 因此尺寸在 MVP Sprint 1 就提供（原 Phase 2 计划提前）

### 4.8 明确不做图片对比

不做 A/B 对比、Wipe 拉帘、像素 Difference 等对比功能；双图能力仅以双页阅读形式存在。

### 4.9 单管理员认证与私有目录（Sprint 7）

- 凭证来自环境变量 `AUTH_USERNAME` / `AUTH_PASSWORD`；**未设置 `AUTH_PASSWORD` 时登录系统整体关闭**（`/auth/status` 返回 `enabled: false`），Sprint 0-6 行为零回归。
- 会话持久化于 SQLite `sessions` 表：token 为 `crypto/rand` 32 字节 hex，固定过期不滑动续期，校验时惰性删除过期记录，重启不丢登录态；Cookie `session_token`（HttpOnly + SameSite=Lax）承载。
- 访问控制三层：
  1. **路由级 `RequireAuth`**：写操作与配置端点（protected-folders PUT、cache cleanup、config 导入导出）未登录直接 `401`；
  2. **`ProtectedAccess`**：内容端点（folders/image/thumbnail/folder-settings）在登录启用时校验访问的路径是否命中私有前缀，命中且未登录返回 `401`；
  3. **目录列表过滤**：游客请求 `/folders` 时，私有前缀的目录直接从列表隐藏（而非置灰）。
- 配置导出文件即导入文件（round-trip 同形）：`GET /config/export` 是唯一不带 `data` 信封的 200 响应。

## 5. Backend 模块划分

```
backend/
├── cmd/server/main.go        # 入口：加载 Config → app.New() → app.Run()
└── internal/
    ├── app/                  # 依赖组装（所有初始化集中于此，main 不直接建 Service）
    ├── config/               # 配置加载（env）
    ├── api/                  # router.go + handler/ + request/ + response/
    ├── service/              # 业务编排层（folder/image/thumbnail/settings）
    ├── filesystem/           # 文件层：scanner / path 安全 / metadata（含图片尺寸，读文件头）
    ├── sorting/              # 排序引擎（filename/natural/time/size/regex/rule）
    ├── thumbnail/            # generator / cache / validator / libvips
    ├── repository/           # SQLite 访问（settings/favorite）
    ├── model/                # 核心领域对象（File/Image/Folder/Sort...）
    └── middleware/           # error / logger
```

调用链约束：

```
Handler → Service → Domain 模块（Filesystem / Sorting / Thumbnail） → Repository / Filesystem
```

严格禁止：Handler 直接读文件、Handler 直接操作 SQLite、Handler 内做排序/Regex/缩略图。详细模块职责见 PROJECT_STRUCTURE.md。

## 6. Frontend 模块划分

```
frontend/src/
├── router/         # vue-router（/ 、/settings、/login + 认证守卫，Sprint 7）
├── pages/          # BrowserPage（页面组合，不含复杂逻辑）、SettingsPage、LoginPage
├── components/     # layout / folder / viewer(含 SpreadFrame) / filmstrip / sorting / settings / common
├── stores/         # folder / viewer / spread / settings / auth（禁止万能 AppStore）
├── services/       # api.ts + 各领域 service（组件禁止直接 fetch）
├── composables/    # useViewer / useZoom / usePan / useKeyboard / useFilmstrip
├── types/          # 与 API Contract 一一对应的 TS 类型
├── utils/          # path / format / spread（SpreadEngine 纯函数）
```

关键状态归属：

- **Store（全局业务状态）**：currentFrame / currentFrameIndex（双页模式按帧翻页）、currentImage、images[]、mode
- **Composable（UI Runtime State）**：scale、panX、panY（useViewer 管理，不进 Store）

## 7. 配置项（环境变量）

| 变量 | 说明 | 默认 |
| --- | --- | --- |
| `IMAGE_ROOT` | 允许浏览的图片根目录（唯一来源） | `/images` |
| `DATA_DIR` | SQLite + 缩略图缓存目录 | `/data` |
| `PORT` | 后端监听端口 | `8080`（宿主对外端口默认 `8160`，由 compose 端口映射 `${PORT:-8160}:8080` 决定） |

## 8. 部署形态

```
docker-compose.yml（image 形态，Sprint 8）
└── albumshelf   （jayvzh/albumshelf:latest，后端单容器托管前端静态文件）
    ├── ports：${PORT:-8160}:8080（左侧宿主端口默认 8160 可改，容器内固定 8080）
    ├── volumes：图片目录只读(:ro) + DATA_DIR 读写
    └── 6 个环境变量透传：IMAGE_ROOT / DATA_DIR / PORT / AUTH_USERNAME / AUTH_PASSWORD / SESSION_MAX_AGE
```

- **镜像构建**（Sprint 8）：`make docker-build` / `docker-push`。镜像基于多阶段构建：builder 阶段启用 `CGO_ENABLED=1` 并安装 `libvips-dev` + `pkg-config`（govips 为 cgo 包）；runtime 阶段含 `curl` 并内置 **HEALTHCHECK**（探测 `/api/v1/health`，`docker ps` 显示 `(healthy)`）。
- **初始化检测**（Sprint 8）：前端路由守卫启动时先请求公开端点 `GET /api/v1/setup/status`（不挂 RequireAuth / ProtectedAccess），`initialized=false` 时重定向 `/setup` 引导页——纯诊断只读，仅展示配置修复指引，禁止任何配置写入；检测请求失败视为就绪放行，避免后端不可达时全站锁死。

MVP 采用**后端直接托管前端静态文件**的单容器方案，简单可靠；多容器拆分留到需要时再做。
