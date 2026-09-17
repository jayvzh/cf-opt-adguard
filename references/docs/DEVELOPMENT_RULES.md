# AlbumShelf・NAS图集馆 开发规则（DEVELOPMENT_RULES）

> 版本：v1.0 ｜ 本文档对所有 Sprint 永久生效，AI IDE 在任何 Sprint 开发前必须先阅读本文档与 PROJECT_STRUCTURE.md。
> 配套：日常轻量任务按仓库根目录 `开发规则.md`（精简版）执行；本总则与精简版冲突时以本总则为准。

---

## 1. 全局开发节奏规则

1. **一次只开发一个 Sprint**，禁止一次生成整个项目
2. 禁止修改/创建**未来 Sprint** 的模块（如 Sprint 1 不做 Spread/Favorite/Tag/Regex Editor）
3. 允许创建未来需要的**接口定义**（如 `Sorter interface`），但不实现（如 RegexSorter 属于 Sprint 5）
4. 每个 Sprint 结束必须通过验收标准 + 自检清单（见 SPRINT0_TASK.md 的自检格式）
5. **自测走开发模式**：日常功能验证一律用 `./scripts/dev.sh`（本地并行启动前后端，前端 :5160、后端 :8080，`/api` 代理到 8080；非开发模式（Docker 部署）浏览器访问端口默认 `8160`，容器内仍监听 `8080`（compose 端口映射 `${PORT:-8160}:8080`）；子命令：`start|stop|restart|status|logs [backend|frontend]`，改代码后用 `restart` 强制清理端口并重启，`start` 遇端口占用会交互询问；无参运行脚本可查看完整用法）。**禁止为验证功能反复执行 `docker compose up --build` 构建镜像**。Docker 镜像优化、编排完善与最终部署冒烟统一放到收尾 Sprint（Sprint 7）完成，各中间 Sprint 不做 Docker 迭代

## 2. Backend 分层与依赖规则

```
Handler → Service → Domain 模块（Filesystem/Sorting/Thumbnail） → Repository / Filesystem
```

### 允许的依赖

```
Handler → Service
Service → Filesystem
Service → Sorting
Service → Thumbnail
Service → Repository
```

### 禁止的依赖

```
Handler → Repository        （Handler 禁止直接操作 SQLite）
Handler → Filesystem        （Handler 禁止直接读文件）
Sorting → Repository        （排序是纯函数，不碰数据库）
Filesystem → API            （文件层禁止反向依赖接口层）
```

### Handler 职责边界

Handler 只做：Parse Parameters → Validate → Call Service → Response。

**Handler 禁止**：复杂排序、Regex、文件扫描、Thumbnail 生成、SQL。

## 3. Backend 结构规则

- `main.go` 只做 `config.Load() → app.New() → app.Run()`，禁止直接创建 Service
- 所有依赖组装集中在 `internal/app/app.go`
- `router.go` 只注册路由，不含业务逻辑
- 领域对象定义集中在 `internal/model/`，其他包不得重复定义冲突结构

## 4. 路径安全规则（永久生效）

所有接收 `path` 参数的接口必须经过 `filesystem/path.go`：

```
用户输入 Path → Clean → Resolve → 确认仍位于 IMAGE_ROOT 内
```

- 越界（`../`、符号链接逃逸、绝对路径越权）→ 返回 `INVALID_PATH`
- 该规则任何 Sprint 都不允许绕过或"临时放宽"

## 5. Frontend 规则

### 依赖方向

```
允许：Page → Component → Store → Service → API
禁止：Component 直接 fetch() API
```

### Store 规则

- 仅允许 4 个 Store：FolderStore / ViewerStore / SpreadStore / SettingsStore
- **禁止创建管理一切的 AppStore**
- Zoom/Pan（scale、panX、panY）是 UI Runtime State，放 useViewer composable，**不进 Store**

### 数据类型规则

- 前端 TS 类型必须来自 API.md §4 契约定义（API Contract → Frontend Type）
- 禁止前端自行发明不一致的数据结构

### 组件复用规则

- 双页 Spread 是**帧渲染**：`ViewerCanvas → SpreadFrame(1~2 张图)`，禁止做成两个独立画布；**不做任何图片对比工具**（A/B 对比、Wipe、Difference）
- 拼帧算法必须收敛在 `utils/spread.ts` 纯函数中，组件内禁止内联拼页逻辑
- 键盘事件统一走 `useKeyboard → Viewer Action → Viewer Store`，禁止键盘直接操作组件 DOM

## 6. API 规则

- 所有接口遵循 API.md 的响应格式：成功 `{"data": ...}`，错误 `{"error": {"code", "message"}}`
- 错误码只用 API.md 枚举表中的值
- 排序一律在后端完成；前端只调 `POST /api/v1/sort/preview` 做编辑器预览

## 7. 性能规则

- 禁止"打开目录 → 加载全部原图"；**Viewer 默认加载预览图（preview）**，原图仅用户点击"加载原图"时按需加载；Filmstrip 用缩略图懒加载
- Filmstrip 必须虚拟渲染，10000+ 目录不得一次渲染全部节点
- Filmstrip 可见缩略图数量必须按视口宽度、缩略图宽度（等高不等宽）与间距动态计算，**禁止硬编码数量**（算法见 ARCHITECTURE.md §4.6）
- 缩略图/预览图必须走服务器持久缓存（hash 命名 + mtime/size 失效校验，`/data/cache/`）
- **所有图片类响应（image/thumbnail）必须带 `v={mtime}{size}` 版本参数 URL + `Cache-Control: immutable` + `ETag`**；禁止发无缓存头的图片响应（双端缓存策略见 ARCHITECTURE.md §4.3）
- 前端禁止为图片自建内存 Map 缓存（HTTP 缓存已覆盖）

## 8. 数据规则

- 图片文件永远不入数据库；数据库只存配置、缓存索引、用户数据
- 数据库迁移放 `backend/migrations/`，按序号递增，禁止修改已应用的迁移

## 9. 代码风格

- Go：标准 gofmt；错误必须显式处理，禁止 `_ = err`
- Vue：`<script setup lang="ts">` + TypeScript strict
- 命名：后端文件 `snake_case.go`，前端组件 `PascalCase.vue`，composable `useXxx.ts`

## 10. 每 Sprint 交付前自检清单

```text
[ ] Build 通过（go build / pnpm build）
[ ] Test 通过（新增模块有单元测试：sorting/path/thumbnail 必测）
[ ] 功能自测经开发模式完成（scripts/dev.sh restart，前端 http://localhost:5160），未为验证功能反复构建 Docker 镜像
[ ] Lint 通过（golangci-lint / eslint）
[ ] API 行为与 API.md 一致（响应格式、错误码）
[ ] 新文件都在本 Sprint 允许清单内
[ ] 未触碰未来 Sprint 模块
[ ] 文档无过时（若接口有变，同步更新 API.md）
```
