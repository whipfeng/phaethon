# Admin SPA 改造需求

> 日期: 2026-09-01
> 来源: 用户反馈

## 背景

Admin 面板当前是多页面架构（MPA），每次菜单切换都会整页刷新。

## 问题

1. **PiP 日志窗口被销毁**：Document Picture-in-Picture 窗口绑定在创建它的页面上，菜单切换导致页面导航，PiP 窗口被销毁
2. **SSE 连接每次都要重新建立**：每次页面刷新都会断开并重建 SSE 连接
3. **页面状态无法保持**：切换菜单后页面状态丢失

## 需求

将 admin 改为 SPA（单页应用），实现：
1. PiP 日志窗口跨菜单保持
2. SSE 连接持久化
3. 菜单切换无刷新

## 方案选择

经对比 React/Vue SPA、HTMX + Go 模板、原生 JS SPA、Go Admin 框架等方案，选择 **HTMX 风格**（Go 模板片段 + HTMX 属性驱动）。

理由：
- 与现有 Go 模板架构最接近，迁移成本最低
- 不需要 Node.js 构建链
- 页面模板几乎不需要改动
- PiP/SSE 持久化只需保持 layout 不刷新即可

## 设计文档

- Plan: [admin_spa_design.md](../plans/admin_spa_design.md)
- Tasks: [admin-spa-tasks.md](../tasks/admin-spa-tasks.md)
