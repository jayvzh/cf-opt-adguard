# Sprint 0 开发任务书（AI IDE Task Specification）

> 用法：将本文档整体复制给 AI IDE（Cursor / Claude Code / Windsurf 等）作为 Sprint 0 的任务输入。
> 后续每个 Sprint 按此模板生成对应任务书。

---

## 1. 任务上下文

在开始编码前，AI 必须先完整阅读以下文档：

```
docs/PRD.md
docs/ARCHITECTURE.md
docs/PROJECT_STRUCTURE.md
docs/API.md
docs/DEVELOPMENT_RULES.md
docs/SPRINT_PLAN.md（本 Sprint 部分）
开发规则.md（根目录精简版：命令与自测约定）
```

## 2. 本 Sprint 目标

**Project Foundation**：搭建可运行的项目骨架，打通前后端 + Docker 最小闭环。

具体任务：

1. Go Backend 最小骨架
2. Vue3 Frontend 最小骨架
3. Docker 构建与编排（仅最小可跑骨架；镜像优化/部署完善统一放 Sprint 7）
4. Config 加载
5. Health Check API

## 3. 允许创建的文件（精确清单）

> **注：项目骨架已预先创建**。§2「完整项目结构」中的目录与占位文件均已存在（内容仅含 `TODO(Sprint N)` 注释，含各自的目标 Sprint 编号）。AI 的职责是**实现下方清单中本 Sprint 对应文件的完整内容**；其余占位文件保持原样，禁止删除、禁止提前填充。

**超出此清单的文件一律禁止创建。**

```
AlbumShelf/
├── backend/
│   ├── cmd/
│   │   └── server/
│   │       └── main.go
│   ├── internal/
│   │   ├── app/
│   │   │   └── app.go
│   │   ├── config/
│   │   │   └── config.go
│   │   └── api/
│   │       ├── router.go
│   │       ├── handler/
│   │       │   └── health_handler.go
│   │       └── response/
│   │           └── error_response.go
│   ├── go.mod
│   └── go.sum
│
├── frontend/
│   ├── public/
│   ├── src/
│   │   ├── main.ts
│   │   ├── App.vue
│   │   └── styles/
│   │       └── main.css
│   ├── package.json
│   ├── vite.config.ts
│   └── tsconfig.json
│
├── docker/
│   ├── backend/
│   │   └── Dockerfile
│   └── frontend/
│       └── Dockerfile
│
├── scripts/
│   ├── dev.sh
│   └── build.sh
│
├── .env.example
├── docker-compose.yml
├── Makefile
└── README.md
```

## 4. 禁止开发（未来功能）

本 Sprint **禁止**创建或实现：

```
filesystem / scanner / path 安全模块   （Sprint 1）
folder / image / thumbnail Handler     （Sprint 1-3）
sorting 任何 Sorter 实现               （Sprint 4-5）
spread / compare / favorite / tag 相关任何代码（Sprint 6+，compare 已从产品范围移除）
SQLite / repository / migrations       （Sprint 4）
Vue Router 页面、FolderTree、Viewer 等组件（Sprint 1-2）
```

前端本 Sprint 只需 App.vue 显示 "AlbumShelf・NAS图集馆" 标题即可，不引入路由。

## 5. 技术要求

### Backend（Go 1.24+ / Gin）

- `main.go` 只做三件事：`config.Load() → app.New() → app.Run()`，逻辑写在 `app.go`
- Config 从环境变量读取：

```
IMAGE_ROOT  默认 /images
DATA_DIR    默认 /data
PORT        默认 8080
```

- 路由：`GET /api/v1/health` → `{"data":{"status":"ok"}}`
- 错误响应结构按 API.md（error_response.go 中定义 Error 结构，本 Sprint 只需结构定义 + health 一个 handler）

### Frontend（Vue 3 + TypeScript + Vite + Pinia + Tailwind）

- Vite 创建 Vue3 + TS 模板
- 引入 Pinia 与 Tailwind（只安装配置，不建业务组件）
- dev 模式 proxy `/api` 到 `http://localhost:8080`
- dev server 固定端口 5160（`server.port=5160` + `strictPort`，见 vite.config.ts），日常自测用 `./scripts/dev.sh start`

### Docker

- backend Dockerfile：多阶段构建（golang builder → 运行镜像需包含 libvips 运行库）
- frontend Dockerfile：node build → nginx 或由 backend 托管静态文件（推荐后者，MVP 单容器）
- docker-compose.yml：单服务（backend 托管前端产物），volume 挂载 `IMAGE_ROOT`（只读）与 `DATA_DIR`

## 6. 验收标准

**开发模式（默认验证方式，本 Sprint 及后续 Sprint 均按此执行）**：

```text
./scripts/dev.sh start
  ↓
浏览器访问 http://localhost:5160（前端 Vite dev，/api 代理到后端 :8080）
  ↓
页面显示 "AlbumShelf・NAS图集馆"
  ↓
GET /api/v1/health 返回 {"data":{"status":"ok"}}
```

> **Docker 说明**：本 Sprint 仅需产出最小可运行的 Docker 骨架文件，可用 `make docker-up` 冒烟一次（可选）；镜像优化、编排完善与最终部署冒烟统一放在 **Sprint 7（部署收尾）**。**禁止为验证每轮功能反复 `docker compose up --build`**。

## 7. AI 自检清单（完成后必须逐项确认）

```text
[ ] go build ./... 通过
[ ] pnpm build 通过
[ ] 开发模式自测通过：./scripts/dev.sh start 后前端 http://localhost:5160 显示 "AlbumShelf・NAS图集馆"，/api/v1/health 返回正确 JSON
[ ] 创建的文件与 §3 清单完全一致（无多余文件）
[ ] 未创建 §4 禁止清单中的任何模块
[ ] main.go 中无 Service 创建逻辑（全部在 app.go）
[ ] .env.example 包含 IMAGE_ROOT / DATA_DIR / PORT 三项
[ ] README.md 包含：项目简介、本地开发步骤、Docker 启动步骤
[ ] （可选，仅骨架冒烟一次）make docker-up 能启动容器；完整 Docker 部署完善归 Sprint 7
```

## 8. 交付物

- 满足 §3 清单的全部代码文件
- 自检结果汇报（逐项打勾）
- 运行验证截图或命令输出（health API 响应）
