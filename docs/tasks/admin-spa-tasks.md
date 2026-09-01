# Admin SPA 改造任务

- 目标：将 admin 从 MPA 改为 HTMX SPA，实现 PiP 跨菜单保持、SSE 持久化
- 创建日期：2026-09-01
- 依赖计划：[admin_spa_design.md](../plans/admin_spa_design.md)

## 阶段 1: 文档与准备

### Task 1.1: 创建 MDD 文档
- [x] 创建 `docs/inputs/20260901_admin_spa_requirement.md`
- [x] 创建 `docs/plans/admin_spa_design.md`
- [x] 创建 `docs/tasks/admin-spa-tasks.md`（本文件）

## 阶段 2: Go 后端

### Task 2.1: render() 添加 HTMX 片段渲染
- [x] `admin/admin.go` - `render()` 检测 `HX-Request: true` 头
- [x] HTMX 请求时调用 `t.ExecuteTemplate(w, "content", data)` 只渲染 content 块
- [x] 普通请求保持 `t.Execute(w, data)` 渲染完整页面

### Task 2.2: 下载 HTMX 库
- [x] 下载 `htmx.min.js`（v2.0.4）到 `admin/static/`

## 阶段 3: 前端改造

### Task 3.1: layout.html 改造
- [x] `<head>` 引入 `<script src="./static/htmx.min.js?v={{.Version}}"></script>`
- [x] 内容区域包裹 `<div id="main-content">`（top-bar 保持在外部）
- [x] 侧边栏 nav links 添加 `hx-get`、`hx-target="#main-content"`、`hx-swap="innerHTML"`、`hx-push-url="true"`
- [x] Raw Config 链接同样改为 HTMX 风格
- [x] `<h2>` 添加 `id="page-title"` 用于客户端更新标题

### Task 3.2: app.js 改造
- [x] 移除 nav-link click 上的 teardown 监听
- [x] 保留 beforeunload/pagehide 上的 teardown
- [x] 添加 `htmx:afterSettle` 处理器：
  - [x] 重新执行注入内容中的 `<script>` 标签（clone-and-replace）
  - [x] 调用 `applyI18n()`
  - [x] 调用 `updateActiveNav()`
  - [x] 调用 `updatePageTitle()`
- [x] 添加 `reloadPage()` 辅助函数（封装 `htmx.ajax`）
- [x] 添加 `PAGE_TITLES` 映射、`updateActiveNav()`、`updatePageTitle()` 函数

### Task 3.3: 替换 location.reload()
- [x] `dashboard.html` - 1 处
- [x] `subscriptions.html` - 3 处
- [x] `mappings.html` - 3 处
- [x] `reverse-wizard.html` - 1 处
- [x] `rules.html` - 4 处
- [x] `proxies.html` - 4 处

### Task 3.4: 修复 DOMContentLoaded 兼容性
- [x] `mappings.html` - `DOMContentLoaded` → 直接调用
- [x] `reverse-wizard.html` - `DOMContentLoaded` → 直接调用

## 阶段 4: 验证与收尾

### Task 4.1: 本地验证
- [x] `go build` 成功
- [ ] 菜单切换无页面刷新（Network 无 document 请求）— 需运行时验证
- [ ] PiP 日志窗口跨菜单保持 — 需运行时验证
- [ ] SSE 连接不中断 — 需运行时验证
- [ ] 浏览器前进/后退正常 — 需运行时验证
- [ ] 每个 URL 可直接访问 — 需运行时验证
- [ ] i18n 导航后正常 — 需运行时验证
- [ ] 所有 CRUD 功能无回归 — 需运行时验证

### Task 4.2: 更新文档
- [x] 更新 `docs/specs/admin_spec.md` 至 v0.2.0
- [x] 标记所有 task 为 [x]
