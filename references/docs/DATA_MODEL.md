# AlbumShelf・NAS图集馆 数据模型设计

> 版本：v1.0 ｜ 数据库：SQLite ｜ 依赖文档：ARCHITECTURE.md

---

## 1. 设计原则（重要）

**图片文件不入数据库。** 数据库只保存：

```
配置 + 缓存索引 + 用户数据
```

图片始终留在 NAS 文件系统，由 Filesystem 模块实时扫描。这意味着：

- 不需要全量图片索引任务
- 文件系统变化天然实时反映
- 数据库极小，备份和迁移成本低

## 2. 表结构

### 2.1 folders（出现过的文件夹）

```sql
CREATE TABLE folders (
    id         INTEGER PRIMARY KEY,
    path       TEXT UNIQUE NOT NULL,
    created_at DATETIME,
    updated_at DATETIME
);
```

> 说明：该表随浏览行为懒创建（首次打开某目录时 upsert），不需要预先导入目录树。

### 2.2 folder_settings（每目录设置，F009）

```sql
CREATE TABLE folder_settings (
    id                 INTEGER PRIMARY KEY,
    folder_id          INTEGER NOT NULL,
    sort_mode          TEXT,
    sort_direction     TEXT,
    regex_pattern      TEXT,
    regex_config       TEXT,        -- JSON 字符串
    view_mode          TEXT,
    page_mode          TEXT,        -- single | spread（双页自动拼页）
    read_order         TEXT,        -- left_to_right | right_to_left（阅读方向）
    wide_ratio         REAL,        -- 横图判定阈值，默认 1.0
    single_first_page  INTEGER,     -- 封面单页显示
    single_last_page   INTEGER,     -- 封底单页显示
    updated_at         DATETIME
);
```

> Spread 相关字段语义见 SPREAD_ENGINE.md。Dummy Page 补位、静态奇偶配对为 Phase 2 扩展字段，暂不建列。

**sort_mode 取值：**

```
filename | natural | modified_time | created_time | file_size | regex
```

**regex_config JSON 结构（多规则排序）：**

```json
{
  "rules": [
    {
      "group": 1,
      "type": "number",
      "direction": "asc"
    },
    {
      "group": 2,
      "type": "number",
      "direction": "asc"
    }
  ]
}
```

字段说明见 SORT_ENGINE.md §3。

### 2.3 favorites（收藏，Sprint 9 实现）

```sql
CREATE TABLE favorites (
    id         INTEGER PRIMARY KEY,
    file_path  TEXT UNIQUE,
    created_at DATETIME
);
```

- `file_path` 唯一约束支撑收藏去重（一张图至多一条收藏）
- `created_at`：收藏时刻（`CURRENT_TIMESTAMP`），列表按其倒序返回；配置导入合并时不保留原导出时间，入库记为导入时刻
- 生命周期：删除/移动源文件后条目**惰性清理**——`GET /favorites` 列出时发现即移除，无后台清理任务
- API：`GET /api/v1/favorites`、`POST /api/v1/favorites/toggle`（API.md §3.14）；纳入配置导入导出（§3.11/§3.12 合并语义）

### 2.4 tags / image_tags（Phase 2）

```sql
CREATE TABLE tags (
    id   INTEGER PRIMARY KEY,
    name TEXT UNIQUE
);

CREATE TABLE image_tags (
    image_path TEXT,
    tag_id     INTEGER
);
```

### 2.5 image_cache（缩略图/预览图缓存索引）

```sql
CREATE TABLE image_cache (
    id             INTEGER PRIMARY KEY,
    source_path    TEXT,
    variant        TEXT,        -- 'thumb' | 'preview'
    cache_path     TEXT,        -- /data/cache/{variant}/hash.jpg
    source_mtime   INTEGER,
    source_size    INTEGER,
    width          INTEGER,
    height         INTEGER,
    created_at     DATETIME
);
```

> 同一张原图可产生 thumb 与 preview 两条缓存记录；失效验证按 (source_mtime, source_size) 逐条判断。

### 2.6 sessions（会话表，Sprint 7）

```sql
CREATE TABLE IF NOT EXISTS sessions (
    token      TEXT PRIMARY KEY,
    created_at DATETIME NOT NULL,
    expires_at DATETIME NOT NULL
);
```

> 单管理员 Cookie 会话的持久化存储：登录成功生成 token（`crypto/rand` 32 字节 hex）写入本表，并以 `session_token` Cookie 下发（HttpOnly + SameSite=Lax）。每次请求由 `middleware/access.go` 查表校验，**过期的记录在校验时惰性删除**（固定过期、不滑动续期）；登出删除对应记录。重启后端不丢登录态。

### 2.7 protected_folders（私有目录，Sprint 7）

```sql
CREATE TABLE IF NOT EXISTS protected_folders (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    path       TEXT NOT NULL UNIQUE,
    created_at DATETIME NOT NULL
);
```

> 私有目录前缀列表（设置页维护，全量替换语义保存）。登录系统启用时，游客请求的目录列表按前缀过滤这些条目，直接访问私有路径返回 `401 UNAUTHORIZED`；登录系统关闭时表不参与任何过滤。

## 3. 缓存失效验证

```
原图 mtime = 123   vs   缓存记录 source_mtime = 120
→ mtime 不一致（或原图 size 变化）→ Thumbnail Invalid → Regenerate
```

判定规则（validator.go）：

1. 原图不存在 → 缓存记录删除
2. `source_mtime` 或 `source_size` 与现值不一致 → 重新生成并更新记录
3. 缩略图文件丢失 → 重新生成

## 4. 后端领域模型（与表结构的对应关系）

领域对象（model/ 包）**不直接等于表结构**，API 返回的图片数据永远来自文件系统实时扫描：

```go
// 文件系统扫描产物
type File struct {
    Name       string    `json:"name"`
    Path       string    `json:"path"`
    Extension  string    `json:"extension"`
    Size       int64     `json:"size"`
    Width      int       `json:"width"`   // 读图片文件头获得（image.DecodeConfig），失败为 0
    Height     int       `json:"height"`  // 双页 Spread 拼页依赖此字段（SPREAD_ENGINE.md §4）
    ModifiedAt time.Time `json:"modified_at"`
}

type Image struct {
    File    // 已含 Width/Height（文件头读取）
}

type Folder struct {
    Name string `json:"name"`
    Path string `json:"path"`
}
```

## 5. 前端 TypeScript 类型（必须与 API Contract 一致）

> 规则：**API Contract → Frontend Type**，禁止前端自行发明不一致的数据结构。权威定义见 API.md §4。

```typescript
export interface FolderItem {
  name: string
  path: string
}

export interface ImageFile {
  name: string
  path: string
  extension: string
  size: number
  width: number | null    // 文件头读取，失败为 null
  height: number | null
  modifiedAt: string   // ISO 8601
}

export interface FolderResponse {
  path: string
  folders: FolderItem[]
  images: ImageFile[]
}
```

## 6. 迁移策略

- 使用 `backend/migrations/` 目录存放 SQL 迁移文件，按序号命名：`0001_init.sql`
- MVP 启动时自动执行未应用的迁移
- Phase 2 新增收藏/标签表时通过新迁移文件扩展，禁止修改已应用的迁移
