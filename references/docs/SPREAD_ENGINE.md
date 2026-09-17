# AlbumShelf・NAS图集馆 双页拼凑引擎设计（SPREAD_ENGINE）

> 版本：v1.0 ｜ 双页模式的核心特色功能，与排序引擎并列单独成文。属于 Sprint 6 实现。
> 逻辑参考：NeeView（https://github.com/neelabo/NeeView）源码 `NeeView/PageFrames/`，本地参考副本：`references/NeeView-master/`。

---

## 1. 定位

**双页模式（Spread，NeeView 术语"見開き"）是为了提升连续阅读体验，不是图片对比工具。**

典型场景：漫画、扫描件、连续图片。切换双页模式后**自动布局拼凑**：

- 足够窄的竖图（portrait）→ 两两拼成左右双页，像翻开的书
- 较宽的横图（landscape）→ 自动独占一屏满幅显示，不被拉伸拼页
- 翻页按"帧"推进，单页帧与双页帧混合排列，阅读节奏连贯

## 2. NeeView 逻辑解读（源码依据）

### 2.1 核心结构

| NeeView 概念 | 文件 | 说明 |
| --- | --- | --- |
| `PageMode` | `Book/PageMode.cs` | `SinglePage` / `WidePage`（1 页 / 2 页显示模式） |
| `PageFrame` | `PageFrames/PageFrame.cs` | 一"帧"= 屏幕上一次呈现的内容（1~2 个 PageFrameElement） |
| `PageFrameFactory` | `PageFrames/PageFrameFactory.cs` | **拼帧算法核心**：决定单页帧还是双页帧 |
| `AspectRatioTools` | `PageFramesMath/AspectRatioTools.cs` | 横图判定 |
| `IsStaticWidePage` | `Config/BookConfig.cs` | 静态配对模式开关 |

### 2.2 横图判定

```csharp
// AspectRatioTools.cs
IsLandscape(width, height, wideRatio) => width > height * wideRatio
// BookConfig.WideRatio 默认 1.0，可配置
```

即：**宽高比 > WideRatio 判为横图**。默认 1.0 时正方形算竖图（可拼页）。

### 2.3 拼帧算法（PageFrameFactory.CreatePageFrame，FramePageSize=2 时）

```
取当前页 source1：
├── source1 是横图（IsSupportedWidePage && source1.IsLandscape()）
│      → 单页帧（CreateSinglePageFrame）        # 横图独占
│
├── 静态宽页模式（IsStaticWidePage）且按奇偶判定本位置不需要第二页
│      → 宽填充帧（CreateWideFillPageFrame）
│
├── 取下一页 source2：
│   ├── 无下一页（末尾）
│   │      → 宽填充帧（可插入 Dummy 空白页）
│   ├── source2 是横图
│   │      → 宽填充帧                            # source1 独占
│   ├── source1 或 source2 是封面/封底，且开启 IsSupportedSingleFirstPage/SingleLastPage
│   │      → 宽填充帧                            # 封面封底单页显示
│   └── 其余情况
│          → 双页帧（CreateWidePageFrame）       # 两竖图拼页
│
└── FramePageSize==1 时 → 永远单页帧
```

### 2.4 静态配对模式（IsStaticWidePage）

动态模式下横图会让配对"错位"（横图前后的竖图各自落单）；静态模式改为**按页码奇偶固定配对**（`(index & 1)` 判定，见 `IsNeedSecondPageWhenStaticWidePage`），横图不打乱分组，占一帧但可拉伸填满。

### 2.5 其他相关机制

| 机制 | 说明 |
| --- | --- |
| Dummy Page 插入 | 双页帧缺第二页时插入空白页补位（`CanInsertDummyPage`，可配置首/末页是否插入） |
| WidePageStretch | 双页缩放方式，默认 `UniformHeight`（等高对齐） |
| WidePageVerticalAlignment | 双页垂直对齐（默认居中） |
| ReadOrder | 阅读方向，决定双页左右顺序（日漫 RightLeft / 普通LeftRight） |
| ContentsSpace | 双页间距 |
| DividePage（Phase 2） | 单页模式下横图拆成两半分屏显示（DividePageRate 控制分割点） |
| Panorama（远期） | 帧无缝连接成横向长卷连续滚动 |

## 3. AlbumShelf・NAS图集馆 实现设计

### 3.1 引擎位置：前端纯函数模块

NeeView 是本地应用，直接知道页面尺寸；Web 端由**后端扫描时读取图片文件头**获得宽高（见 §4），随目录列表返回。因此拼帧计算完全可以在前端完成：

```
utils/spread.ts（SpreadEngine 纯函数）
输入：images[]（含 width/height）+ SpreadOptions
输出：frames[]（每帧 = { images: [1~2 张], type: 'single' | 'spread' }）
     + imageIndex → frameIndex 映射（供 Filmstrip / 键盘翻页使用）
```

纯函数 + 无副作用 → 可单元测试（对齐 SORT_ENGINE 的测试要求）。

### 3.2 SpreadOptions（文件夹设置，可保存复用）

```typescript
interface SpreadOptions {
  pageMode: 'single' | 'spread'
  wideRatio: number            // 横图判定阈值，默认 1.0
  readOrder: 'left_to_right' | 'right_to_left'
  singleFirstPage: boolean     // 封面单页显示，默认 true
  singleLastPage: boolean      // 封底单页显示，默认 false
  insertDummyPage: boolean     // 缺页补白，Phase 2
  staticWidePage: boolean      // 静态奇偶配对，Phase 2
}
```

### 3.3 动态拼帧算法（MVP 实现）

```
frames = []
i = 0
while i < images.length:
    page = images[i]
    if isLandscape(page):                  # width > height * wideRatio
        frames.push([page]); i += 1
    else:
        next = images[i+1]
        if next 不存在:
            frames.push([page]); i += 1    # （Phase 2: 可插 Dummy 补位）
        elif isLandscape(next):
            frames.push([page]); i += 1    # 下一页是横图，本页独占
        elif (i == 0 && singleFirstPage) || (i+1 == last && singleLastPage && next 无配对):
            frames.push([page]); i += 1    # 封面/封底单页
        else:
            frames.push([page, next]); i += 2   # 双页帧
```

按 readOrder 决定帧内左右顺序：`right_to_left` 时帧内两图反转（第一页在右）。

### 3.4 渲染模型：单画布帧渲染（架构关键变化）

双页不再是"两个独立 ViewerCanvas"，而是**一个帧容器渲染 1~2 张图**：

```
ViewerCanvas（唯一画布，Zoom/Pan 作用于整个帧）
└── SpreadFrame
    ├── Image A（flex 布局左/右）
    └── Image B（flex 布局右/左）
```

- 等高对齐：帧内两图按高度统一缩放（`scale = min(hA, hB) 归一化`），垂直居中
- Zoom / Pan / Fit 作用于整帧（阅读体验优先，不提供帧内单图独立缩放）
- 单页帧 = SpreadFrame 只含一张图的退化情形，渲染逻辑统一

### 3.5 翻页与 Filmstrip 联动

- ViewerStore 以 **frameIndex** 为翻页单位（`next()` / `previous()` 移动一帧）
- Filmstrip 仍以图片为单位：当前帧包含的所有图片缩略图同时高亮
- 点击 Filmstrip 某缩略图 → 通过 `imageIndex → frameIndex` 映射定位到所在帧
- 切换 Single/Spread 模式时保持**当前图片**不跳变（用映射反查，而非保留帧号）

## 4. 后端配合：图片尺寸获取

自动拼页依赖每张图的宽高。方案：

- **后端目录扫描时解析图片文件头**（JPEG SOF marker / PNG IHDR / WebP VP8X 等，Go 标准库 `image.DecodeConfig` 即可，只读几十字节，不解码全图）
- 结果随 `GET /api/v1/folders` 的 images 数组返回 `width` / `height` 字段
- 性能：单次目录扫描按需读取 header，10k 文件级别可接受；后续可缓存进 thumbnail_cache 表复用 mtime/size 失效逻辑（Phase 2）
- 尺寸缺失（解析失败）→ 该图按竖图处理（可拼页），保证不中断布局

## 5. Sprint 归属与边界

| 内容 | Sprint |
| --- | --- |
| 图片尺寸读取（DecodeConfig）随列表返回 | Sprint 1 |
| SpreadEngine 纯函数 + 单元测试 | Sprint 6 |
| SpreadFrame 渲染、模式切换、ReadOrder、封面封底单页 | Sprint 6 |
| Dummy Page 补位、静态奇偶配对 | Phase 2 |
| 单页横图拆屏（DividePage） | Phase 2 |
| Panorama 连续滚动 | 远期 |

## 6. 测试要求

`utils/spread.ts` 单元测试必须覆盖：

1. 全竖图序列：两两配对 `[1,2],[3,4],[5,6]`
2. 横图打断：竖,竖,横,竖,竖 → `[1,2],[3],[4,5]`
3. 末尾落单竖图 → 独占一帧
4. 封面单页开启：第一张竖图不与第二张拼页
5. WideRatio=1.2 时正方形图判为竖图；WideRatio=0.8 时判为横图
6. right_to_left：双页帧内图片顺序反转
7. imageIndex→frameIndex 映射正确；切换模式后当前图片保持不跳变
