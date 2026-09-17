# AlbumShelf・NAS图集馆 项目结构与模块设计

> 版本：v1.0 ｜ 本文档是 AI IDE 开发时的**核心约束文档**：规定代码怎么组织、模块怎么协作、数据如何流动。

---

## 1. 项目总体目录

```
AlbumShelf/
│
├── docs/                  # 项目设计文档
├── backend/               # Go 后端
├── frontend/              # Vue 前端
├── docker/                # Docker 构建文件
├── scripts/               # 开发/维护脚本
├── data/                  # 本地运行数据（cache/thumbs + cache/previews + albumshelf.db）
├── .env.example
├── docker-compose.yml
├── Makefile
└── README.md
```

| 目录 | 职责 |
| --- | --- |
| docs | 项目设计文档 |
| backend | Go 后端 |
| frontend | Vue 前端 |
| docker | Docker 构建文件 |
| scripts | 开发/维护脚本 |
| data | 本地运行数据 |
| docker-compose.yml | 容器编排 |
| Makefile | 常用开发命令 |

## 2. 完整项目结构（最终形态）

> ⚠️ 这是**最终架构**。目录与占位文件已一次性预创建（仅含 `TODO(Sprint N)` 注释），但 Sprint 0 只允许**实现** MVP 精简版（见 §41）对应的文件，后续 Sprint 按需逐步填充，禁止一次性实现全部模块。

```
AlbumShelf/
│
├── docs/
│   ├── PRD.md
│   ├── ARCHITECTURE.md
│   ├── PROJECT_STRUCTURE.md
│   ├── DATA_MODEL.md
│   ├── API.md
│   ├── SORT_ENGINE.md
│   ├── SPREAD_ENGINE.md
│   ├── UI_DESIGN.md
│   ├── SPRINT_PLAN.md
│   ├── DEVELOPMENT_RULES.md
│   └── SPRINT0_TASK.md
│
├── backend/
│   ├── cmd/
│   │   └── server/
│   │       └── main.go
│   ├── internal/
│   │   ├── app/
│   │   │   └── app.go
│   │   ├── config/
│   │   │   └── config.go
│   │   ├── api/
│   │   │   ├── router.go
│   │   │   ├── handler/
│   │   │   │   ├── health_handler.go
│   │   │   │   ├── folder_handler.go
│   │   │   │   ├── image_handler.go
│   │   │   │   ├── thumbnail_handler.go
│   │   │   │   └── settings_handler.go
│   │   │   ├── request/
│   │   │   │   ├── folder_request.go
│   │   │   │   └── settings_request.go
│   │   │   └── response/
│   │   │       ├── folder_response.go
│   │   │       ├── image_response.go
│   │   │       └── error_response.go
│   │   ├── service/
│   │   │   ├── folder_service.go
│   │   │   ├── image_service.go
│   │   │   ├── thumbnail_service.go
│   │   │   └── settings_service.go
│   │   ├── filesystem/
│   │   │   ├── filesystem.go
│   │   │   ├── scanner.go
│   │   │   ├── path.go
│   │   │   └── metadata.go
│   │   ├── sorting/
│   │   │   ├── sorter.go
│   │   │   ├── filename_sorter.go
│   │   │   ├── natural_sorter.go
│   │   │   ├── time_sorter.go
│   │   │   ├── size_sorter.go
│   │   │   ├── regex_sorter.go
│   │   │   └── rule_sorter.go
│   │   ├── thumbnail/
│   │   │   ├── generator.go
│   │   │   ├── cache.go
│   │   │   ├── validator.go
│   │   │   └── libvips.go
│   │   ├── repository/
│   │   │   ├── database.go
│   │   │   ├── folder_repository.go
│   │   │   ├── settings_repository.go
│   │   │   ├── favorite_repository.go
│   │   │   ├── session_repository.go
│   │   │   ├── appsettings_repository.go
│   │   │   └── image_cache_repository.go
│   │   ├── model/
│   │   │   ├── file.go
│   │   │   ├── folder.go
│   │   │   ├── image.go
│   │   │   ├── thumbnail.go
│   │   │   ├── settings.go
│   │   │   ├── sort.go
│   │   │   ├── auth.go
│   │   │   ├── appsettings.go
│   │   │   └── setup.go
│   │   ├── middleware/
│   │   │   ├── error.go
│   │   │   ├── logger.go
│   │   │   └── access.go
│   │   └── auth/
│   │       ├── auth.go
│   │       └── auth_test.go
│   ├── migrations/
│   ├── tests/
│   ├── go.mod
│   └── go.sum
│
├── frontend/
│   ├── public/
│   ├── src/
│   │   ├── main.ts
│   │   ├── App.vue
│   │   ├── router/
│   │   │   └── index.ts
│   │   ├── pages/
│   │   │   ├── BrowserPage.vue
│   │   │   └── SettingsPage.vue
│   │   ├── components/
│   │   │   ├── layout/
│   │   │   │   ├── AppHeader.vue
│   │   │   │   ├── AppSidebar.vue
│   │   │   │   └── AppLayout.vue
│   │   │   ├── folder/
│   │   │   │   ├── FolderTree.vue
│   │   │   │   └── FolderItem.vue
│   │   │   ├── viewer/
│   │   │   │   ├── ImageViewer.vue
│   │   │   │   ├── ViewerCanvas.vue
│   │   │   │   ├── SpreadFrame.vue
│   │   │   │   ├── SpreadControls.vue
│   │   │   │   ├── ViewerToolbar.vue
│   │   │   │   ├── ImageInfo.vue
│   │   │   │   └── LoadingOverlay.vue
│   │   │   ├── filmstrip/
│   │   │   │   ├── Filmstrip.vue
│   │   │   │   └── ThumbnailItem.vue
│   │   │   ├── sorting/
│   │   │   │   ├── SortMenu.vue
│   │   │   │   ├── RegexEditor.vue
│   │   │   │   └── SortRuleEditor.vue
│   │   │   └── common/
│   │   │       ├── AppButton.vue
│   │   │       ├── AppModal.vue
│   │   │       └── EmptyState.vue
│   │   ├── stores/
│   │   │   ├── folder.ts
│   │   │   ├── viewer.ts
│   │   │   ├── spread.ts
│   │   │   ├── settings.ts
│   │   │   ├── auth.ts
│   │   │   └── favorites.ts
│   │   ├── services/
│   │   │   ├── api.ts
│   │   │   ├── folder.service.ts
│   │   │   ├── image.service.ts
│   │   │   ├── settings.service.ts
│   │   │   ├── auth.service.ts
│   │   │   ├── appsettings.service.ts
│   │   │   ├── setup.service.ts
│   │   │   └── favorite.service.ts
│   │   ├── composables/
│   │   │   ├── useViewer.ts
│   │   │   ├── useKeyboard.ts
│   │   │   ├── useZoom.ts
│   │   │   ├── usePan.ts
│   │   │   └── useFilmstrip.ts
│   │   ├── types/
│   │   │   ├── file.ts
│   │   │   ├── image.ts
│   │   │   ├── folder.ts
│   │   │   ├── sort.ts
│   │   │   ├── spread.ts
│   │   │   ├── auth.ts
│   │   │   ├── appsettings.ts
│   │   │   └── setup.ts
│   │   ├── utils/
│   │   │   ├── path.ts
│   │   │   ├── format.ts
│   │   │   └── spread.ts
│   │   └── styles/
│   │       └── main.css
│   ├── package.json
│   └── vite.config.ts
│
├── docker/
│   ├── backend/
│   │   └── Dockerfile
│   └── frontend/
│       └── Dockerfile
│
├── scripts/
│   ├── dev.sh
│   ├── build.sh
│   └── test.sh
│
├── data/
│   ├── cache/
│   │   ├── thumbs/        # 缩略图持久缓存（200px）
│   │   └── previews/      # 预览图持久缓存（长边 1920px）
│   └── albumshelf.db
│
├── .env.example
├── docker-compose.yml
├── Makefile
└── README.md
```

## 3. Backend 架构原则

```
Handler → Service → Domain 模块 → Repository / Filesystem
```

**严格禁止：**

- Handler 直接读文件
- Handler 直接操作 SQLite
- Handler 内做复杂排序 / Regex / 文件扫描 / Thumbnail / SQL

## 4. 启动入口与依赖组装

### cmd/server/main.go

职责只有三步：

```
config.Load() → app.New() → app.Run()
```

### internal/app/app.go

负责全部依赖组装：

```
Config → Database → Repository → Filesystem → Services → Handlers → Router
```

> 重要：`main.go` 不负责创建几十个 Service，所有初始化集中在 `app.go`。

## 5. API 模块（internal/api/）

### router.go

只负责注册路由：

```
/api/v1/health
/api/v1/folders
/api/v1/image
/api/v1/image/info
/api/v1/thumbnail
/api/v1/sort/preview
/api/v1/folder/settings
```

Router 不处理业务逻辑。

### handler/

职责链：

```
HTTP Request → Parse Parameters → Validate → Call Service → Response
```

Handler **禁止做**：复杂排序、Regex、文件扫描、Thumbnail、SQL。

## 6. Service 模块（业务编排层）

以 `FolderService` 为例，职责：

```
获取文件夹 → 读取 Folder Settings → 扫描文件 → 过滤图片 → 排序 → 返回
```

依赖：

```
FolderService
   ├── Filesystem
   ├── SettingsRepository
   └── SortEngine
```

## 7. Filesystem 模块（internal/filesystem/）

这是最重要的边界模块。职责：文件路径、文件扫描、文件 Metadata、目录读取。

### filesystem.go — 统一接口

```go
type Filesystem interface {
    ListDirectory(path string) ([]model.File, []model.Folder, error)
    GetFile(path string) (*model.File, error)
    GetMetadata(path string) (*model.FileMeta, error)
}
```

> 预留扩展：未来支持 Local / NAS / WebDAV / S3 时替换实现。当前 MVP 只实现 `LocalFilesystem`。

### path.go — Path Security（Sprint 1 必须完成）

职责：Path Normalize、Path Validate、Root Restriction。

```
任何用户输入 Path → Clean → Resolve → 确认仍位于 IMAGE_ROOT 内
```

- `/api/image?path=/images/a.jpg` → 允许
- `/api/image?path=../../etc/passwd` → 拒绝，返回 `INVALID_PATH`

### scanner.go

```
Read Directory → 识别 Folder → 识别 Image → 返回 Raw File List
```

Scanner **不负责**排序、缩略图、数据库。

### metadata.go

负责：Filename、Path、Size、Modified Time、Created Time、Extension。未来：Width、Height、EXIF。

## 8. Model 模块（internal/model/）

统一定义核心领域对象：

```go
type File struct {
    Name        string
    Path        string
    Extension   string
    Size        int64
    ModifiedAt  time.Time
}

type Image struct {
    File
    Width  int
    Height int
}

type Folder struct {
    Name string
    Path string
}
```

```go
type SortMode string

const (
    SortFilename     SortMode = "filename"
    SortNatural      SortMode = "natural"
    SortModifiedTime SortMode = "modified_time"
    SortCreatedTime  SortMode = "created_time"
    SortFileSize     SortMode = "file_size"
    SortRegex        SortMode = "regex"
)
```

## 9. Sorting 模块（internal/sorting/）

独立模块，纯函数式：

```
Input ([]model.File + SortOptions) → SortEngine → Output ([]model.File)
```

```go
type Sorter interface {
    Sort(files []model.File, options model.SortOptions) []model.File
}
```

```
SortService → SortEngine
                 ├── FilenameSorter
                 ├── NaturalSorter
                 ├── TimeSorter
                 ├── SizeSorter
                 ├── RegexSorter
                 └── RuleSorter（多规则）
```

详细算法见 SORT_ENGINE.md。

## 10. Thumbnail 模块（internal/thumbnail/）

```
Request → ThumbnailService → Cache Check → Valid?
                                            ├── Yes → Return Cache
                                            └── No → libvips Generate → Save Cache → Return
```

- **generator.go**：Image Resize、Quality、Format（MVP 用 JPEG，后续 WebP）
- **cache.go**：Cache Path、Cache Directory、Cache Lookup。缓存文件名用 `hash(source_path + mtime + size + variant + 长度)`（如 `8d923ab2.jpg`），variant ∈ `thumb | preview`；目录 `/data/cache/{thumbs,previews}/`，**持久化**（容器重启不丢）
- **validator.go**：判断 Original Exists? / Original Modified? / Thumbnail Exists?（source mtime > 缓存记录 mtime → Regenerate）

## 11. Repository 模块

只负责数据库访问（SettingsRepository、FavoriteRepository 等）。禁止包含复杂业务逻辑。

## 12. Frontend 页面与组件职责

### BrowserPage（pages/BrowserPage.vue）

只负责**页面组合**：

```
AppLayout
├── FolderTree
├── Viewer
├── Filmstrip
└── SortPanel
```

不包含复杂 Zoom / 复杂 API / 复杂排序逻辑。

### Layout 模块

- **AppLayout**：Header + Sidebar + Main Content 布局
- **AppHeader**：Logo、Current Folder、Sort、PageMode（单页/双页切换）、Settings 入口
- **AppSidebar**：包含 FolderTree

### Folder 模块

- **FolderTree**：目录导航，发出 `select(folder)` 事件 → `FolderStore.openFolder()`

### Viewer 模块（前端核心）

```
ImageViewer
├── ViewerToolbar
├── ViewerCanvas
│   └── SpreadFrame（帧内容：1~2 张图）
├── SpreadControls
└── ImageInfo
```

- **ImageViewer.vue**：加载当前帧、Viewer State、Controls
- **ViewerCanvas.vue**：真正执行图片 Transform（scale / translateX / translateY），支持 Zoom / Pan / Fit / Reset。Zoom/Pan 作用于**整个帧**
- **SpreadFrame.vue**：帧内容渲染，包含 1 张图（单页/横图独占）或 2 张图（双页等高对齐并排）
- **SpreadControls.vue**：单页/双页切换、阅读方向（左开本/右开本）、WideRatio 调整

### Zoom/Pan 状态归属（重要设计）

- **Store（全局业务状态）**：currentImage、currentIndex、Viewer Mode
- **useViewer() Composable（UI Runtime State）**：scale、panX、panY

理由：Zoom/Pan 属于 UI Runtime State，不是全局业务状态。

### Filmstrip 模块

- **Filmstrip.vue**：Image List、Virtual Render、Horizontal Scroll、Current Item Auto Center
- **ThumbnailItem.vue**：只负责 Thumbnail 显示、Selected State、Click。不负责导航/API/排序
- **useFilmstrip.ts**：动态布局与虚拟窗口计算——ResizeObserver 测量视口宽度；等高不等宽（宽度按宽高比缩放 + clamp）下用前缀和偏移数组 + 二分查找确定可见区间；可见数量**由视口宽度、缩略图宽度和间距动态推导，禁止硬编码**（算法见 ARCHITECTURE.md §4.6）

数据流：

```
FolderStore → images[] → Filmstrip → ThumbnailItem
点击：ThumbnailItem → select(index) → ViewerStore.currentIndex → ImageViewer
```

### Spread 双页模块（关键架构设计）

双页是**帧渲染**，不是双画布对比（对比工具明确不做）：

```
Single 模式：   ViewerCanvas → SpreadFrame(1 张图)
Spread 双页：   ViewerCanvas → SpreadFrame(2 张图，等高并排)
```

- 拼帧算法在 `utils/spread.ts`（纯函数 SpreadEngine，见 SPREAD_ENGINE.md）：横图独占一帧、竖图两两拼页、封面封底单页
- ViewerStore 翻页以 frameIndex 为单位；Filmstrip 仍以图片为单位（当前帧的所有图片同时高亮）
- 切换 Single/Spread 时通过 imageIndex↔frameIndex 映射保持当前图片不跳变
- 这样 Zoom / Pan / Fit 逻辑天然复用，Spread 后期扩展（Dummy Page、静态配对、Panorama）只动帧层

### Sorting UI 模块

- **SortMenu**：Filename / Natural / Date / Size / Regex 选择
- **RegexEditor**：Regex Input、Capture Group 配置、**Sort Preview（必须实现）**
  - 输入 `chapter(\d+)_page(\d+)` → 实时显示 `chapter10_page2.jpg → Group1=10, Group2=2`
  - 用户必须能直观确认 Regex 是否正确

## 13. Store 设计

允许且仅允许 6 个 Store（Sprint 7 新增 AuthStore，Sprint 9 新增 FavoritesStore）：

| Store | 状态 | Actions |
| --- | --- | --- |
| FolderStore | currentFolder、folders、images、loading、error | openFolder()、refresh()、changeSort() |
| ViewerStore | currentFrameIndex、currentImage、mode（single/spread） | next()、previous()、select()、nextFrame()、previousFrame() |
| SpreadStore | pageMode、wideRatio、readOrder、singleFirstPage、singleLastPage、frames、imageToFrame | buildFrames()、togglePageMode()、setReadOrder() |
| SettingsStore | folderSettings、globalSettings | — |
| AuthStore | enabled、authenticated、username、loaded | fetchStatus()、login()、logout() |
| FavoritesStore | images、paths（Set，O(1) 判定）、loaded、loading、error；getters：count、has()、available | fetchAll()、toggle()、clear() |

> AuthStore 理由（Sprint 7）：认证是**全局横切状态**——路由守卫、AppHeader 登录态、设置页账户区块三处共享，不属于任何单一页面，故作为第 5 个 Store 转正。`enabled=false`（未设置 AUTH_PASSWORD）时 `authenticated` 恒为 false，守卫与 UI 均以 `enabled` 为准。
> FavoritesStore 理由（Sprint 9）：收藏是**全局横切数据**——FolderTree 收藏夹入口、AppHeader「只看收藏」、BrowserPage 网格角标与筛选、ImageViewer 心形按钮四处共享同一份状态，不属于任何单一页面，故作为第 6 个 Store 转正。`available` getter 统一门控：auth enabled 且未登录时为 false（入口完全隐藏、不发起请求）。

**禁止**创建管理一切的 AppStore。

## 14. Composables

| Composable | 职责 |
| --- | --- |
| useViewer.ts | Fit、Reset、Zoom、Fullscreen |
| useZoom.ts | Zoom In / Out、Wheel Zoom |
| usePan.ts | Mouse Drag、Touch Drag |
| useKeyboard.ts | Arrow Left/Right、Space、F、0、1 |
| useFilmstrip.ts | 动态布局（视口测量/前缀和偏移/可见区间二分）、虚拟滚动、自动居中 |

> 重要：键盘**不直接操作组件**，链路为 `Keyboard → Viewer Action → Viewer Store`。

## 15. 前端 API Layer（services/）

**原则：组件禁止直接 `fetch()`。** 链路必须是：

```
Component → Store → Service → API
```

- **api.ts**：统一 Base URL、Error、Timeout、Request
- **folder.service.ts**：`getFolder(path)`、`getImages(path)`
- **image.service.ts**：`getImageURL(path)`、`getThumbnailURL(path)`
- **settings.service.ts**：文件夹设置读写

## 16. 核心数据流（7 条 Flow）

### Flow 1：打开文件夹

```
Click Folder → FolderTree → FolderStore.openFolder()
→ FolderService.getFolder() → GET /api/v1/folders
→ FolderHandler → FolderService → Filesystem.Scanner → SortEngine
→ 返回 {folders, images} → FolderStore → Filmstrip / Viewer
```

### Flow 2：点击图片

```
Click Thumbnail → ThumbnailItem → Filmstrip → ViewerStore.select(index)
→ imageToFrame 映射定位到所在帧 → currentFrameIndex → ImageViewer → ViewerCanvas → 加载 /api/v1/image
```

### Flow 3：Next（键盘翻页，按帧推进）

```
Arrow Right → useKeyboard → ViewerStore.nextFrame() → currentFrameIndex++
→ 当前帧（1~2 张图）→ ImageViewer Update → Filmstrip 高亮当前帧所有图片并 Auto Center
```

### Flow 4：获取缩略图

```
Filmstrip → ThumbnailItem → GET /api/v1/thumbnail
→ ThumbnailHandler → ThumbnailService → Cache
→ 命中 Return / 未命中 Generator(libvips) → Save Cache → Return
```

### Flow 5：排序（后端排序）

```
Sort Menu → Natural → FolderStore.changeSort()
→ GET /api/v1/folders?path=/Comics&sort=natural
→ FolderService → Scanner → SortEngine → 返回排序结果
```

### Flow 6：Regex Sort Preview

```
RegexEditor 输入 chapter(\d+)_page(\d+) → POST /api/v1/sort/preview
→ RegexSorter → 返回 {matches: [{filename, groups}]}
→ 前端显示 chapter10_page2.jpg → Group 1 = 10, Group 2 = 2
```

### Flow 7：保存 Folder Setting

```
Save → SettingsStore → SettingsService → PUT /api/v1/folder/settings
→ SettingsHandler → SettingsService → SettingsRepository → SQLite
```

## 17. 图片加载策略（必须遵守）

```
禁止：打开目录 → 加载全部原图

必须：Folder Open → Load Metadata
      Filmstrip → Load Thumbnail（懒加载）
      仅 Current Image → Load Preview（默认）/ Original（用户点击"加载原图"时）
```

三级图片体系：Original（原图，**按需加载**——用户点击"加载原图"）/ Preview（Viewer 默认显示，长边 1920px）/ Thumbnail（Filmstrip 用，200px）。

双端缓存（详见 ARCHITECTURE.md §4.3）：服务器持久缓存 `/data/cache/` + 浏览器 HTTP 缓存（URL 版本参数 `v={mtime}{size}` + `Cache-Control: immutable`）——重访同目录缩略图/预览图 0 网络请求；原图不自动显示，同一张图重复点击"加载原图"直接命中浏览器缓存。

## 18. AI IDE 开发边界（每个 Sprint 必须遵守）

### 不允许

修改/创建未来 Sprint 的模块。例如 Sprint 1（文件浏览）禁止创建 Spread 双页、Favorite、Tag、Regex Editor。

### 允许

创建未来需要的**接口定义**（如 `type Sorter interface`），但不实现（如 RegexSorter 属于 Sprint 5）。

## 19. 模块依赖规则摘要

**Backend 允许：** Handler→Service、Service→Filesystem/Sorting/Repository
**Backend 禁止：** Handler→Repository、Sorting→Repository、Filesystem→API

**Frontend 允许：** Page→Component、Component→Store、Store→Service、Service→API
**Frontend 禁止：** Component 直接 Fetch API

## 20. MVP 初始目录（Sprint 0 精简版）

Sprint 0 **不创建全部文件**，只创建：

```
AlbumShelf/
├── docs/
├── backend/
│   ├── cmd/server/main.go
│   └── internal/
│       ├── app/
│       ├── config/
│       └── api/
├── frontend/
│   └── src/
│       ├── App.vue
│       └── main.ts
├── docker/
└── docker-compose.yml
```

后续逐步扩展：Sprint 1 +filesystem/folder；Sprint 2 +viewer；Sprint 3 +thumbnail；Sprint 5 +sorting；Sprint 6 +spread。
