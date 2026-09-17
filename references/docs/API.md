# AlbumShelf・NAS图集馆 API 设计

> 版本：v1.0 ｜ 所有接口前缀：`/api/v1` ｜ 本文档是前后端的**契约文档**，前端 TS 类型必须与此对应。

---

## 1. 通用约定

### 响应格式

成功：

```json
{ "data": {} }
```

错误：

```json
{
  "error": {
    "code": "INVALID_PATH",
    "message": "Invalid image path"
  }
}
```

### Error Code 枚举

| Code | HTTP 状态码 | 说明 |
| --- | --- | --- |
| `INVALID_PATH` | 400 | 路径非法或越出 IMAGE_ROOT |
| `FILE_NOT_FOUND` | 404 | 图片文件不存在 |
| `FOLDER_NOT_FOUND` | 404 | 目录不存在 |
| `UNSUPPORTED_FORMAT` | 415 | 不支持的图片格式 |
| `THUMBNAIL_FAILED` | 500 | 缩略图生成失败 |
| `INVALID_REGEX` | 400 | Regex 编译失败 |
| `DATABASE_ERROR` | 500 | 数据库错误 |
| `INTERNAL_ERROR` | 500 | 其他内部错误 |

### 路径参数约定

- 所有 `path` 参数为**相对于 IMAGE_ROOT 的绝对风格路径**（如 `/Comics/001.jpg`）
- 服务端统一执行：Clean → Resolve → 校验仍在 IMAGE_ROOT 内，否则 `INVALID_PATH`

## 2. Endpoint 总览

| Method | Path | 功能 | Sprint |
| --- | --- | --- | --- |
| GET | `/api/v1/health` | 健康检查 | 0 |
| GET | `/api/v1/folders` | 列出目录内容（子目录 + 图片，支持排序参数） | 1 |
| GET | `/api/v1/image` | 获取图片（variant=original\|preview，含双端缓存头） | 2 |
| GET | `/api/v1/image/info` | 获取图片元信息 | 2 |
| GET | `/api/v1/thumbnail` | 获取缩略图（带缓存） | 3 |
| GET | `/api/v1/folder/settings` | 读取文件夹设置 | 4 |
| PUT | `/api/v1/folder/settings` | 保存文件夹设置 | 4 |
| POST | `/api/v1/sort/preview` | Regex 排序预览 | 5 |
| POST | `/api/v1/auth/login` | 登录（种会话 Cookie） | 7 |
| POST | `/api/v1/auth/logout` | 退出登录（删会话 + 清 Cookie） | 7 |
| GET | `/api/v1/auth/status` | 登录状态探测（`enabled`/`authenticated`，已登录附系统信息） | 7 |
| GET | `/api/v1/protected-folders` | 私有目录列表 | 7 |
| PUT | `/api/v1/protected-folders` | 全量替换私有目录（需登录） | 7 |
| GET | `/api/v1/cache/stats` | 缓存体积统计 | 7 |
| POST | `/api/v1/cache/cleanup/orphan` | 清理孤儿缓存（需登录） | 7 |
| POST | `/api/v1/cache/cleanup/all` | 清理全部缓存（需登录） | 7 |
| POST | `/api/v1/cache/cleanup/variant` | 清空单变体缓存 thumb/preview（需登录） | 10 |
| GET | `/api/v1/cache/warm/status` | 后台预热级别与进度快照（需登录） | 10 |
| PUT | `/api/v1/cache/warm/settings` | 更新预热级别（需登录，即时生效） | 10 |
| GET | `/api/v1/config/export` | 导出配置（附件下载，需登录） | 7 |
| POST | `/api/v1/config/import` | 导入配置（按路径 upsert 合并，需登录） | 7 |
| GET | `/api/v1/setup/status` | 初始化状态检测（公开、只读，见 §3.13） | 8 |
| GET | `/api/v1/favorites` | 收藏列表（收藏时间倒序） | 9 |
| POST | `/api/v1/favorites/toggle` | 收藏/取消收藏切换 | 9 |

> 访问控制（Sprint 7）：`POST /auth/login`、`GET /auth/status`、`GET /protected-folders`、`GET /cache/stats` 无需登录；`PUT /protected-folders`、`POST /cache/cleanup/*`、`GET /config/export`、`POST /config/import` 在 auth enabled 时要求登录（401 `UNAUTHORIZED`）；内容端点 `/folders`、`/image`、`/image/info`、`/thumbnail`、`/folder/settings` 在游客访问私有路径时返回 401 `UNAUTHORIZED`；收藏端点 `/favorites`、`/favorites/toggle` 属个人数据端点，auth enabled 时一律要求登录（401），游客完全不可见（Sprint 9）。auth disabled 时上述拦截全部关闭。公开端点另含 `GET /health`（Sprint 0）与 `GET /setup/status`（Sprint 8，配置有误时未登录仍可访问，否则引导页死锁）。

## 3. Endpoint 详细定义

### 3.1 健康检查

```http
GET /api/v1/health
```

```json
{ "data": { "status": "ok" } }
```

### 3.2 列出目录内容

```http
GET /api/v1/folders?path=/Comics&sort=natural&direction=asc
```

| 参数 | 必填 | 说明 |
| --- | --- | --- |
| `path` | 是 | 目录路径，空或 `/` 表示根目录 |
| `sort` | 否 | filename / natural / modified_time / created_time / file_size / regex，默认 filename |
| `direction` | 否 | asc / desc，默认 asc |
| `regex` | 否 | sort=regex 时必填 |
| `regex_rules` | 否 | sort=regex 且多规则时的 JSON 配置（URL 编码） |

**排序优先级**：请求参数 > 该目录已保存的 folder_settings > 默认（filename asc）。

成功响应：

```json
{
  "data": {
    "path": "/Comics",
    "folders": [
      { "name": "Chapter01", "path": "/Comics/Chapter01" }
    ],
    "images": [
      {
        "name": "001.jpg",
        "path": "/Comics/001.jpg",
        "extension": ".jpg",
        "size": 1234567,
        "width": 1200,
        "height": 1800,
        "modified_at": "2026-09-01T10:00:00Z"
      }
    ]
  }
}
```

> `width` / `height` 为图片像素尺寸，**扫描时读取图片文件头**获得（Go `image.DecodeConfig`，不解码全图）。该字段是前端双页 Spread 自动拼页的输入（见 SPREAD_ENGINE.md §4），MVP Sprint 1 即提供。解析失败时两字段为 `null`，前端按竖图处理。

> `folders` 数组按名称自然排序固定返回；`images` 数组按排序参数返回。

### 3.3 获取图片（原图 / 预览图）

```http
GET /api/v1/image?path=/Comics/001.jpg&variant=preview&v=17345678901234567
```

| 参数 | 说明 |
| --- | --- |
| `path` | 原图路径 |
| `variant` | `original` \| `preview`，**默认 `preview`**。Sprint 2 未实现 preview 生成时，后端回退返回原图并通过 `X-Image-Variant` 响应头标注实际变体 |
| `v` | 版本参数（`{mtime}{size}`，来自图片列表响应），服务端仅用于缓存失效，忽略其值 |

- 返回图片二进制（正确的 Content-Type）
- **必须返回缓存头**：`Cache-Control: public, max-age=31536000, immutable` + `ETag`（mtime+size）。URL 含版本参数 → 原图/预览图修改后 URL 自然变化，浏览器缓存自动失效
- 预览图规格：长边 1920px、JPEG q80（服务器持久缓存，见 ARCHITECTURE.md §4.3）
- 可选请求头 `X-Load-Priority: low`（前端空闲预热：查看器滑窗 WARMUP / 目录预热 DIRWARM）：
  服务端把生成任务降为低优先级（用户正在查看的图恒为高优先级），并打点"前端预热流量"
  ——后台全库预热器在两类流量静默前不推进。缺省 = 高优先级。同源自定义头不改变 URL，
  浏览器 HTTP 缓存键不受影响

### 3.4 获取图片元信息

```http
GET /api/v1/image/info?path=/Comics/001.jpg
```

```json
{
  "data": {
    "name": "image_001.jpg",
    "path": "/Comics/001.jpg",
    "resolution": { "width": 3840, "height": 2160 },
    "size": 5033165,
    "modified_at": "2026-09-01T10:00:00Z"
  }
}
```

### 3.5 获取缩略图

```http
GET /api/v1/thumbnail?path=/Comics/001.jpg&width=300
```

| 参数 | 说明 |
| --- | --- |
| `path` | 原图路径 |
| `width` | 目标宽度（px），默认 300，允许 200/300/500 |

流程：Cache Check（mtime/size 验证）→ 命中直接返回；未命中 libvips 生成 → 写缓存 → 返回。

- URL 同样携带 `v={mtime}{size}` 版本参数
- **必须返回缓存头**：`Cache-Control: public, max-age=31536000, immutable` + `ETag`
- 可选请求头 `X-Load-Priority: low`（语义同 §3.3）：目录预热 DIRWARM 的缩略图取此通道，
  服务端低优生成并打点前端预热流量；缺省 = 高优先级（胶片条/网格懒加载的用户可见缩略图）
- 目标：重访同目录时缩略图全部命中浏览器磁盘缓存，0 网络请求

### 3.6 文件夹设置

```http
GET /api/v1/folder/settings?path=/Comics
```

```json
{
  "data": {
    "path": "/Comics",
    "sort_mode": "regex",
    "sort_direction": "asc",
    "regex_pattern": "chapter(\\d+)_page(\\d+)",
    "regex_config": "{\"rules\":[{\"group\":1,\"type\":\"number\",\"direction\":\"asc\"},{\"group\":2,\"type\":\"number\",\"direction\":\"asc\"}]}",
    "page_mode": "spread",
    "read_order": "right_to_left",
    "wide_ratio": 1.0,
    "single_first_page": true,
    "single_last_page": false
  }
}
```

```http
PUT /api/v1/folder/settings
```

```json
{
  "path": "/Comics",
  "sort_mode": "regex",
  "sort_direction": "asc",
  "regex_pattern": "chapter(\\d+)_page(\\d+)",
  "regex_config": "{\"rules\":[{\"group\":1,\"type\":\"number\",\"direction\":\"asc\"},{\"group\":2,\"type\":\"number\",\"direction\":\"asc\"}]}",
  "page_mode": "spread",
  "read_order": "right_to_left",
  "wide_ratio": 1.0,
  "single_first_page": true,
  "single_last_page": false
}
```

Spread 相关字段说明（详见 SPREAD_ENGINE.md）：

| 字段 | 取值 | 说明 |
| --- | --- | --- |
| `page_mode` | `single` / `spread` | 单页 / 双页自动拼页 |
| `read_order` | `left_to_right` / `right_to_left` | 阅读方向（日漫右开本） |
| `wide_ratio` | number（默认 1.0） | 横图判定阈值：`width > height × wide_ratio` 判为横图（独占一屏） |
| `single_first_page` | bool | 封面单页显示 |
| `single_last_page` | bool | 封底单页显示 |

> Spread 五字段均可为 `null`（未保存）；GET 未保存目录返回 `null`，前端回落默认值。
> PUT 时字段非法或缺省由后端容错规范化（page_mode→single、read_order→left_to_right、wide_ratio→1.0、single_first_page→true、single_last_page→false）。
> `view_mode` 为数据库保留列，API 暂不接入（Sprint 6 决策），预留给阅读模式记忆扩展。

### 3.7 Regex 排序预览（供 RegexEditor 实时预览）

```http
POST /api/v1/sort/preview
Content-Type: application/json
```

```json
{
  "files": ["chapter10_page2.jpg", "chapter1_page1.jpg"],
  "mode": "regex",
  "regex": "chapter(\\d+)_page(\\d+)",
  "rules": [
    { "group": 1, "type": "number", "direction": "asc" },
    { "group": 2, "type": "number", "direction": "asc" }
  ]
}
```

响应：

```json
{
  "data": {
    "sorted": ["chapter1_page1.jpg", "chapter10_page2.jpg"],
    "matches": [
      {
        "filename": "chapter10_page2.jpg",
        "groups": ["10", "2"]
      },
      {
        "filename": "chapter1_page1.jpg",
        "groups": ["1", "1"]
      }
    ],
    "unmatched": []
  }
}
```

Regex 非法时返回 `INVALID_REGEX`。

> 历史备注：早期设计中有独立的 `POST /api/sort` 端点，已合并为「folders 排序参数 + sort/preview」两个入口，不再单独保留。

### 3.8 认证（Sprint 7）

会话载体见 §1「认证载体」。`AUTH_PASSWORD` 未设置（登录系统关闭）时三个端点仍可用，但登录会返回 `400 UNAUTHORIZED`。

```http
POST /api/v1/auth/login
Content-Type: application/json
```

```json
{
  "username": "admin",
  "password": "secret"
}
```

响应（`200`，同时下发 `Set-Cookie: session_token=…; HttpOnly; SameSite=Lax; Path=/; Max-Age=604800`）：

```json
{
  "data": {
    "authenticated": true,
    "username": "admin"
  }
}
```

错误：凭证错误 → `401 INVALID_CREDENTIALS`；登录系统未启用 → `400 UNAUTHORIZED`。

```http
POST /api/v1/auth/logout
```

响应（`200`，删除 sessions 表记录并清除 Cookie）：

```json
{
  "data": {
    "authenticated": false
  }
}
```

```http
GET /api/v1/auth/status
```

响应（`200`；`username` / `image_root` / `data_dir` 三项**仅已登录时返回**，游客不泄露服务器路径）：

```json
{
  "data": {
    "enabled": true,
    "authenticated": true,
    "username": "admin",
    "image_root": "/data/images",
    "data_dir": "/data/albumshelf"
  }
}
```

### 3.9 私有目录（Sprint 7）

```http
GET /api/v1/protected-folders
```

响应：

```json
{
  "data": {
    "paths": ["/Comics/Adult", "/Private"]
  }
}
```

```http
PUT /api/v1/protected-folders
```

请求（**全量替换语义**：以请求列表为准覆盖旧配置）：

```json
{
  "paths": ["/Comics/Adult", "/Private"]
}
```

响应：同 GET（返回规范化后的列表）。

| 项 | 说明 |
| --- | --- |
| 访问控制 | GET 免登录；PUT 需登录（`401 UNAUTHORIZED`） |
| 错误 | 路径非法 / 不存在 / 越出 IMAGE_ROOT → `400 INVALID_PATH` |
| 生效 | 保存后游客的目录列表立即过滤这些前缀，直接访问返回 `401 UNAUTHORIZED` |

### 3.10 缓存管理（Sprint 7）

```http
GET /api/v1/cache/stats
```

响应（thumb / preview 两个变体的文件数与磁盘占用）：

```json
{
  "data": {
    "thumb":   { "count": 123, "bytes": 4567890 },
    "preview": { "count": 45,  "bytes": 1234567 }
  }
}
```

```http
POST /api/v1/cache/cleanup/orphan
```

```http
POST /api/v1/cache/cleanup/all
```

响应（两类清理同形）：

```json
{
  "data": {
    "removed_files": 10,
    "removed_bytes": 123456
  }
}
```

| 端点 | 语义 | 访问控制 |
| --- | --- | --- |
| `GET /cache/stats` | 只读统计 | 免登录 |
| `POST /cache/cleanup/orphan` | 仅清理**孤儿缓存**（源文件已删除的缩略图 / 预览图） | 需登录 |
| `POST /cache/cleanup/all` | **清空全部**缩略图与预览图缓存 | 需登录 |

### 3.10a 缓存预热管理（Sprint 10）

```http
GET /api/v1/cache/warm/status
```

响应（预热器状态快照；phase ∈ idle / warming / paused——paused = 前台浏览中自动暂停）：

```json
{
  "data": {
    "level": "minimal",
    "phase": "warming",
    "dirs_total": 120,
    "dirs_done": 35,
    "current_dir": "/APS/xxx",
    "images_planned": 2000,
    "images_done": 800,
    "last_pass_end": 0,
    "enabled": true
  }
}
```

```http
PUT /api/v1/cache/warm/settings
```

请求体（level ∈ off / minimal / level1 / level2 / full，非法值 400 INVALID_REQUEST）：

```json
{ "level": "level1" }
```

```http
POST /api/v1/cache/cleanup/variant
```

请求体（variant ∈ thumb / preview，清空该变体全部缓存文件与索引行；响应同两类清理）：

```json
{ "variant": "preview" }
```

级别语义（每目录按文件名序处理前缀，档位按写真/COS 图库典型分布设计）：

| 级别 | 缩略图 | 预览图 |
| --- | --- | --- |
| `off` | 不构建 | 不构建 |
| `minimal`（默认） | 前 20 张 | 前 5 张 |
| `level1` | 前 100 张 | 前 50 张 |
| `level2` | 前 300 张 | 前 150 张 |
| `full` | 全部 | 全部 |

三个端点均需登录。级别持久化于 `app_settings` 表，变更即时生效（触发按新级别重新扫描）；env `THUMB_WARMER=0` 可彻底关闭预热运行（status 中 `enabled=false`）。

### 3.11 配置导出（Sprint 7）

```http
GET /api/v1/config/export
```

响应为**附件下载**：`Content-Disposition: attachment; filename=albumshelf-config-YYYYMMDD.json`，响应体是裸 JSON payload（**本端点是唯一不带 `data` 信封的 200 响应**，下载文件即导入文件，round-trip 依赖同形）：

```json
{
  "version": 1,
  "exported_at": "2026-09-09T12:00:00Z",
  "folder_settings": [
    {
      "path": "/Comics",
      "sort_mode": "regex",
      "sort_direction": "asc",
      "regex_pattern": "chapter(\\d+)_page(\\d+)",
      "regex_config": "{\"rules\":[…]}",
      "page_mode": "spread",
      "read_order": "right_to_left",
      "wide_ratio": 1.0,
      "single_first_page": true,
      "single_last_page": false
    }
  ],
  "protected_folders": ["/Comics/Adult"],
  "favorites": [
    { "path": "/Comics/001.jpg", "created_at": "2026-09-08T20:00:00Z" }
  ]
}
```

> `folder_settings` 中未保存的字段序列化为 `null`。`favorites` 为可选字段（`omitempty`，Sprint 9 起导出）：旧配置文件无此字段不受影响。需登录（`401 UNAUTHORIZED`）。

### 3.12 配置导入（Sprint 7）

```http
POST /api/v1/config/import
Content-Type: application/json
```

请求体即 §3.11 导出文件的原文（round-trip）。导入语义为**upsert 合并**：按 `path` 覆盖已有目录设置，新增不存在的目录；私有目录与现有列表**合并**（去重）；收藏按 `path` **合并**（`INSERT OR IGNORE`，已存在则跳过，不覆盖不删除现有收藏；`created_at` 不保留原导出时间，入库记为导入时刻；仅做路径安全校验、不要求文件存在——迁移到新 NAS 时文件可能暂缺，收藏列表的惰性清理会兜底）。

响应：

```json
{
  "data": {
    "folder_settings": 3,
    "protected_folders": 1,
    "favorites": 2
  }
}
```

错误：`version` 不符或 JSON 解析失败 → `400 CONFIG_INVALID`；引用了非法 / 不存在路径 → `400 INVALID_PATH`。需登录。

### 3.13 初始化状态检测（Sprint 8）

```http
GET /api/v1/setup/status
```

**公开端点**：不挂 RequireAuth / ProtectedAccess——未登录 + 配置有误时必须仍可访问，否则引导页死锁。全部字段由只读检测实时得出，**不落盘、不缓存、无任何写操作**。

响应：

```json
{
  "data": {
    "image_root_configured": true,
    "image_root_exists": false,
    "image_root_readable": false,
    "auth_enabled": true,
    "initialized": false
  }
}
```

| 字段 | 语义 |
| --- | --- |
| `image_root_configured` | 环境变量 IMAGE_ROOT 已配置（非空） |
| `image_root_exists` | 图片目录存在且为目录（`os.Stat`） |
| `image_root_readable` | 目录真实可读（`os.Open` + `Readdirnames(1)`，空目录的 io.EOF 视为可读；覆盖 NAS 卷权限错误场景） |
| `auth_enabled` | 登录系统是否启用（`AUTH_PASSWORD` 是否设置；仅布尔，不泄露路径） |
| `initialized` | 前三项逻辑与（`auth_enabled` 不参与判定——游客模式同样可用） |

> 前端契约：未初始化时全局守卫自动跳转 `/setup` 引导页（见 SPRINT8_TASK.md §5.4）；检测请求失败时放行导航，不锁死全站。

### 3.14 收藏（Sprint 9）

个人数据端点（PRD F010 收藏部分）：auth enabled 时一律要求登录（401 `UNAUTHORIZED`，游客完全隐藏）；auth disabled 时免登录开放。

#### GET /api/v1/favorites

```http
GET /api/v1/favorites
```

响应 `data.images` 与 `GET /folders` 的 `images` **同构**（含尺寸元信息），按**收藏时间倒序**：

```json
{
  "data": {
    "images": [
      {
        "name": "001.jpg",
        "path": "/Comics/001.jpg",
        "extension": ".jpg",
        "size": 1024000,
        "width": 1200,
        "height": 1800,
        "modified_at": "2026-09-01T10:00:00Z"
      }
    ]
  }
}
```

> 已删除/移动文件的失效收藏**惰性清理**：列出时发现即记日志并从库中移除，不阻断响应，不出现在列表中。数据库失败 → `500 DATABASE_ERROR`。

#### POST /api/v1/favorites/toggle

```http
POST /api/v1/favorites/toggle
Content-Type: application/json
```

请求体：

```json
{ "path": "/Comics/001.jpg" }
```

校验路径安全（Resolve 防目录穿越）且文件存在、格式受支持后切换收藏状态，响应 `data` 为**切换后**的状态：

```json
{ "data": { "path": "/Comics/001.jpg", "favorited": true } }
```

错误：请求体缺失 / `path` 为空 / 路径非法 → `400 INVALID_PATH`；文件不存在 → `404 FILE_NOT_FOUND`；格式不支持 → `400 UNSUPPORTED_FORMAT`；数据库失败 → `500 DATABASE_ERROR`。

## 4. Frontend TypeScript 类型（契约对应）

```typescript
// types/folder.ts
export interface FolderItem {
  name: string
  path: string
}

// types/file.ts
export interface ImageFile {
  name: string
  path: string
  extension: string
  size: number
  width: number | null   // 图片尺寸（读文件头获得），解析失败为 null
  height: number | null
  modified_at: string // ISO 8601（与 §3.2 响应 JSON 字段一致）
}

// types/folder.ts
export interface FolderResponse {
  path: string
  folders: FolderItem[]
  images: ImageFile[]
}

// types/sort.ts
export type SortMode =
  | 'filename'
  | 'natural'
  | 'modified_time'
  | 'created_time'
  | 'file_size'
  | 'regex'

export type SortDirection = 'asc' | 'desc'

export interface SortRule {
  group: number
  type: 'number' | 'string'
  direction: SortDirection
}

export interface SortPreviewRequest {
  files: string[]
  mode: SortMode
  regex?: string
  rules?: SortRule[]
}

export interface SortPreviewResponse {
  sorted: string[]
  matches: { filename: string; groups: string[] }[]
  unmatched: string[]
}

// types/spread.ts（双页 Spread，见 SPREAD_ENGINE.md）
export type PageMode = 'single' | 'spread'
export type ReadOrder = 'left_to_right' | 'right_to_left'

export interface SpreadOptions {
  pageMode: PageMode
  wideRatio: number
  readOrder: ReadOrder
  singleFirstPage: boolean
  singleLastPage: boolean
}

export interface SpreadFrame {
  images: ImageFile[]          // 1 张（单页/横图独占）或 2 张（双页）
  type: 'single' | 'spread'
}

// types/auth.ts（Sprint 7，对应 §3.8）
export interface AuthStatus {
  enabled: boolean          // 登录系统是否启用（AUTH_PASSWORD 是否设置）
  authenticated: boolean    // 当前会话是否已登录
  username?: string         // 仅已登录返回
  image_root?: string       // 仅已登录返回
  data_dir?: string         // 仅已登录返回
}

export interface LoginRequest {
  username: string
  password: string
}

export interface LoginResult {
  authenticated: boolean
  username: string
}

// types/appsettings.ts（Sprint 7，对应 §3.9-§3.12）
export interface VariantCacheStats {
  count: number
  bytes: number
}

export interface CacheStats {
  thumb: VariantCacheStats
  preview: VariantCacheStats
}

export interface CleanupResult {
  removed_files: number
  removed_bytes: number
}

// 导出文件中的单条目录设置（未保存字段序列化为 null）
export interface ConfigFolderEntry {
  path: string
  sort_mode: string | null
  sort_direction: string | null
  regex_pattern: string | null
  regex_config: string | null
  page_mode: string | null
  read_order: string | null
  wide_ratio: number | null
  single_first_page: boolean | null
  single_last_page: boolean | null
}

// 导出文件中的单条收藏（Sprint 9）
export interface ConfigFavoriteEntry {
  path: string
  created_at?: string // 可选：旧配置无此字段导入不受影响
}

export interface ConfigPayload {
  version: number
  exported_at: string
  folder_settings: ConfigFolderEntry[]
  protected_folders: string[]
  favorites?: ConfigFavoriteEntry[] // 可选（omitempty）
}

export interface ConfigImportResult {
  folder_settings: number
  protected_folders: number
  favorites: number
}

// types/setup.ts（Sprint 8，对应 §3.13）
export interface SetupStatus {
  image_root_configured: boolean
  image_root_exists: boolean
  image_root_readable: boolean
  auth_enabled: boolean
  initialized: boolean
}

// types/favorite.ts（Sprint 9，对应 §3.14）
export interface FavoriteListResponse {
  images: ImageFile[] // 与 GET /folders 的 images 同构，收藏时间倒序
}

export interface FavoriteToggleResponse {
  path: string
  favorited: boolean
}
```

## 5. API 数据流总览

```
USER → Vue UI → Store → API Service → Go Handler → Go Service
                                          │
                    ┌─────────────────────┼─────────────────────┐
                    ▼                     ▼                     ▼
               Filesystem              Sorting             Thumbnail
                    │                                          │
                    │                                        libvips
                    └─────────────────────┬────────────────────┘
                                          ▼
                                       Storage
                              ┌──────────┴──────────┐
                              ▼                     ▼
                         Image Files            Cache / SQLite
```
