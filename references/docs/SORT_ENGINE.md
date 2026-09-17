# AlbumShelf・NAS图集馆 排序引擎设计

> 版本：v1.0 ｜ 排序是本项目最有价值的特色功能，单独成文档。属于 Sprint 4/5 实现。

---

## 1. 定位与边界

- 排序**只发生在后端** `internal/sorting/`，独立纯函数模块，不依赖数据库、不依赖 HTTP
- 输入：`[]model.File` + `SortOptions`；输出：排序后的 `[]model.File`
- 排序不修改文件、不落库；只有用户显式"保存设置"时才写入 folder_settings

## 2. 接口定义

```go
// sorter.go
type Sorter interface {
    Sort(files []model.File, options model.SortOptions) []model.File
}

type SortOptions struct {
    Mode      string     // filename | natural | modified_time | created_time | file_size | regex
    Direction string     // asc | desc
    Regex     string     // mode=regex 时使用
    Rules     []SortRule // mode=regex 且多规则时使用
}

type SortRule struct {
    Group     int    // capture group 序号（从 1 开始）
    Type      string // number | string
    Direction string // asc | desc
}
```

## 3. 排序模式

| Mode | 说明 | 示例结果 |
| --- | --- | --- |
| filename | 按文件名字典序 A→Z（direction 可反转） | `1, 10, 100, 11, 2, 20` |
| natural | 自然排序：名字中数字段按数值比较 | `1, 2, 10, 11, 20, 100` |
| modified_time | 修改时间 旧→新 | — |
| created_time | 创建时间 旧→新 | — |
| file_size | 文件大小 小→大 | — |
| regex | 按 Regex 提取的捕获组排序（本项目核心特色） | 见 §5 |

## 4. Natural Sort 算法

```
输入：1.jpg / 10.jpg / 2.jpg
  → 分词（Tokenize）：把文件名切分为 [文本段, 数字段, 文本段...] 序列
  → 逐段比较：数字段按数值比较，文本段按字典序比较
结果：1.jpg < 2.jpg < 10.jpg
```

实现要点：

- 比较函数为分段比较（chunked comparison），不是简单提取第一个数字
- 大小写不敏感（sensitivity: base）
- 等值时回退到字典序保证稳定性（排序结果确定性）

## 5. Regex Sort（核心特色）

### 5.1 执行流程

```
Filename → Regex Match → Capture Groups → Sort Key → Compare → Sorted Result
```

示例：`chapter10_page2.jpg`，Regex `chapter(\d+)_page(\d+)`：

```
Capture → ["10", "2"]
Sort Key（type=number）→ [10, 2]
多规则依次比较：先比 Group 1，相同再比 Group 2
```

### 5.2 多规则排序

规则依序生效（Rule 1 → Rule 2 → ...）：

```json
{
  "regex": "chapter(\\d+)_page(\\d+)",
  "rules": [
    { "group": 1, "type": "number", "direction": "asc" },
    { "group": 2, "type": "number", "direction": "asc" }
  ]
}
```

效果：

```
chapter1_page1
chapter1_page10
chapter2_page1
chapter10_page1
```

### 5.3 单规则简写

MVP 支持简单场景：一个 Regex + 单个 group 配置（`/api/v1/folders?sort=regex&regex=(\d+)`）。

### 5.4 未匹配文件的策略

**MVP 固定策略：**

```
Matched Files   → 参与排序，排前面
Unmatched Files → 保持原序，排最后
```

未来扩展：`first / last / ignore`（配置化）。接口设计上预留 `unmatched_policy` 字段位。

### 5.5 错误处理

- Regex 编译失败 → 返回 `INVALID_REGEX`，前端 RegexEditor 立即提示
- 规则引用的 group 不存在 → 该规则跳过（不报错），在 Preview 响应中体现

## 6. Go 实现结构

```
internal/sorting/
├── sorter.go            # Sorter 接口 + SortEngine 分发
├── filename_sorter.go   # 字典序
├── natural_sorter.go    # 自然排序
├── time_sorter.go       # 修改/创建时间
├── size_sorter.go       # 文件大小
├── regex_sorter.go      # Regex 编译 → Match → Capture → Sort Key → Compare
└── rule_sorter.go       # 多规则组合比较器（链式 less 函数）
```

SortEngine 分发逻辑：

```
SortService → SortEngine.Sort(files, options)
                └── switch options.Mode → 选择对应 Sorter
```

所有 Sorter 必须是**稳定排序**（Go `sort.SliceStable`），保证同 key 文件相对顺序确定。

## 7. 前端 Preview（配合 RegexEditor）

排序本身在后端，但 RegexEditor 需要实时预览：

```
用户输入 Regex → POST /api/v1/sort/preview
→ 显示每个文件的捕获结果：
   chapter10_page2.jpg → Group 1 = 10, Group 2 = 2
→ 显示排序后的文件顺序
→ 用户确认无误后保存为该目录设置
```

**Sort Preview 是 RegexEditor 的必备组成部分**，没有预览用户无法确认 Regex 正确性。

## 8. 测试要求

`internal/sorting/` 必须有单元测试覆盖：

1. Natural：`1, 10, 100, 11, 2, 20 → 1, 2, 10, 11, 20, 100`
2. Regex 多规则：`chapter1_page1 / chapter1_page10 / chapter2_page1 / chapter10_page1`
3. 未匹配文件排最后且保持原序
4. INVALID_REGEX 返回错误
5. DESC 方向反转
6. 稳定性：同 key 输入顺序保持
