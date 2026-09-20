# Admin Console Mobile Adaptation & Mesh UX Specification

> 版本: v0.1.0
> 日期: 2026-09-20
> 状态: DRAFT
> 依赖: [admin_mobile_and_mesh_ux.md](../inputs/admin_mobile_and_mesh_ux.md)

## 1. 概述

本规格定义 Admin 控制台的两项改进：
1. 移动端响应式适配
2. Mesh 页面域名后缀路由和 Advertise CIDR 的交互重构

## 2. 移动端适配

### 2.1 断点定义

| 断点 | 宽度 | 布局 |
|------|------|------|
| Desktop | > 1024px | 侧边栏 + 主内容区 |
| Tablet | 768px - 1024px | 折叠侧边栏（图标） + 主内容区 |
| Mobile | < 768px | 底部导航栏 + 全屏主内容区 |

### 2.2 侧边栏行为

**Desktop (> 1024px)**:
- 侧边栏固定左侧，宽度 220px
- 显示图标 + 文字
- 可拖拽调整宽度

**Tablet (768px - 1024px)**:
- 侧边栏固定左侧，宽度 60px
- 只显示图标，文字隐藏
- 悬停时显示 tooltip

**Mobile (< 768px)**:
- 侧边栏隐藏，改为底部导航栏
- 底部导航栏固定，高度 56px
- 显示 5 个主要入口：Dashboard、TUN、Mesh、Logs、Config
- 其他页面通过顶部面包屑或页面内链接访问

### 2.3 表格适配

**Desktop**:
- 标准表格布局
- 所有列可见

**Tablet/Mobile**:
- 表格容器支持横向滚动（`overflow-x: auto`）
- 关键列固定左侧（如名称、状态）
- 操作列固定右侧
- 可选：卡片式布局（每个记录一张卡片）

### 2.4 表单适配

**Desktop**:
- 表单字段可并排（`flex-row`）
- 标签在左侧

**Tablet/Mobile**:
- 表单字段堆叠（`flex-column`）
- 标签在上方
- 输入框宽度 100%
- 按钮宽度 100% 或并排两个

### 2.5 弹窗适配

**Desktop**:
- 居中弹窗，最大宽度 600px
- 可关闭

**Mobile**:
- 全屏弹窗或底部抽屉
- 从底部滑入
- 顶部固定标题栏，底部固定操作按钮

### 2.6 触摸优化

- 按钮最小高度 44px（Apple HIG）
- 输入框最小高度 44px
- 点击区域不小于 44x44px
- 禁用 hover 效果，改用 active 状态

## 3. Mesh 页面交互重构

### 3.1 当前问题

- Domain Suffixes 和 Advertise 使用逗号分隔的文本输入框
- 只有一个 Save 按钮保存所有配置
- 无法单独增删改查每条记录
- 与 Rules、Proxies 等页面的交互模式不一致

### 3.2 目标交互

改为标准 CRUD 模式：
- 表格展示所有记录
- 每条记录有 Edit、Delete 按钮
- 顶部 Add 按钮添加新记录
- 弹窗编辑单条记录

### 3.3 Domain Suffix Routes 重构

**表格结构**:

| Domain Suffix | Actions |
|---------------|---------|
| example.com | Edit / Delete |
| test.org | Edit / Delete |

**Add/Edit 弹窗**:
- 字段：Domain Suffix（必填）
- 验证：不能为空，不能包含通配符

**API**:
- `GET /api/mesh/domain-suffixes` - 获取列表
- `POST /api/mesh/domain-suffixes` - 添加 `{ "suffix": "example.com" }`
- `PUT /api/mesh/domain-suffixes/:index` - 编辑
- `DELETE /api/mesh/domain-suffixes/:index` - 删除

### 3.4 Advertise Routes 重构

**表格结构**:

| CIDR | Actions |
|------|---------|
| 192.168.1.0/24 | Edit / Delete |
| 10.0.0.0/8 | Edit / Delete |

**Add/Edit 弹窗**:
- 字段：CIDR（必填）
- 验证：必须是有效的 IPv4 CIDR

**API**:
- `GET /api/mesh/advertise` - 获取列表
- `POST /api/mesh/advertise` - 添加 `{ "cidr": "192.168.1.0/24" }`
- `PUT /api/mesh/advertise/:index` - 编辑
- `DELETE /api/mesh/advertise/:index` - 删除

### 3.5 保留功能

- Force Gossip 按钮保留在页面顶部
- Config 区域改为两个独立的表格区域（Domain Suffixes、Advertise）
- 每个区域有自己的 Add 按钮和表格

### 3.6 向后兼容

- 保留 `PATCH /api/mesh/config` API（批量更新）
- 新增的 CRUD API 是对现有 API 的补充
- 前端使用新的 CRUD API，不再使用文本输入框

## 4. 实现约束

### 4.1 技术栈

- 纯 HTML + CSS + JavaScript（无框架）
- 使用 HTMX 进行异步请求
- 使用现有 CSS 变量和组件样式

### 4.2 文件改动

| 文件 | 改动 |
|------|------|
| `admin/static/style.css` | 添加移动端响应式样式 |
| `admin/templates/mesh.html` | 重构 Domain Suffixes 和 Advertise 区域 |
| `admin/static/app.js` | 添加 CRUD 交互逻辑 |
| `admin/admin.go` | 添加 CRUD API 端点 |

### 4.3 国际化

- 所有新增文本使用 `data-i18n` 属性
- 在 `admin/static/i18n.js` 中添加翻译

## 5. 验收标准

### 5.1 移动端适配

- [ ] 在 iPhone SE (375px) 上可用
- [ ] 在 iPad (768px) 上可用
- [ ] 在 Desktop (1920px) 上保持原有体验
- [ ] 表格可横向滚动或卡片式展示
- [ ] 弹窗在移动端全屏或底部抽屉
- [ ] 按钮和输入框大小适合触摸

### 5.2 Mesh 页面交互

- [ ] Domain Suffixes 显示为表格
- [ ] 可以添加、编辑、删除 Domain Suffix
- [ ] Advertise 显示为表格
- [ ] 可以添加、编辑、删除 Advertise CIDR
- [ ] 弹窗编辑单条记录
- [ ] 操作后自动刷新列表
- [ ] 与 Rules 页面交互风格一致

## 6. 优先级

1. **P0**: Mesh 页面交互重构（用户明确提到的痛点）
2. **P1**: 移动端基础适配（表格滚动、表单堆叠、弹窗全屏）
3. **P2**: 移动端高级适配（底部导航栏、卡片式布局）
