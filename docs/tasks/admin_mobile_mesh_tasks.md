# Admin Mobile & Mesh UX Tasks

> 依赖: [admin_mobile_mesh_design.md](../plans/admin_mobile_mesh_design.md)

## Phase 1: Mesh 页面交互重构 (P0) ✓ 完成

### Task 1.1: 后端 Domain Suffixes CRUD API ✓
- [x] 添加 `GET /api/mesh/domain-suffixes`
- [x] 添加 `POST /api/mesh/domain-suffixes`
- [x] 添加 `PUT /api/mesh/domain-suffixes/:index`
- [x] 添加 `DELETE /api/mesh/domain-suffixes/:index`
- [x] 验证 suffix 非空
- [x] 保存配置后同步 mesh manager

### Task 1.2: 后端 Advertise CRUD API ✓
- [x] 添加 `GET /api/mesh/advertise`
- [x] 添加 `POST /api/mesh/advertise`
- [x] 添加 `PUT /api/mesh/advertise/:index`
- [x] 添加 `DELETE /api/mesh/advertise/:index`
- [x] 验证 CIDR 格式
- [x] 保存配置后同步 mesh manager

### Task 1.3: 前端 Mesh 页面重构 ✓
- [x] 重构 mesh.html 页面结构
- [x] Domain Suffixes 改为表格 + Add 按钮
- [x] Advertise 改为表格 + Add 按钮
- [x] 添加 Edit/Delete 按钮
- [x] 添加弹窗组件
- [x] 实现 CRUD JavaScript 逻辑
- [x] 页面加载时调用 API 获取数据

### Task 1.4: 测试验证 ✓
- [x] 测试 Domain Suffix 增删改查
- [x] 测试 Advertise CIDR 增删改查
- [x] 测试配置持久化
- [x] 测试 mesh manager 同步

## Phase 2: 移动端基础适配 (P1) ✓ 完成

### Task 2.1: CSS 媒体查询 ✓
- [x] 定义断点：Mobile < 768px, Tablet 768-1024px, Desktop > 1024px
- [x] 实现底部导航栏样式
- [x] 侧边栏响应式隐藏/显示

### Task 2.2: 表格响应式 ✓
- [x] 表格容器横向滚动
- [x] 关键列固定（首列、末列）- 通过 min-width 实现
- [x] 测试小屏幕表格展示

### Task 2.3: 表单响应式 ✓
- [x] 表单字段堆叠布局
- [x] 输入框最小高度 44px
- [x] 按钮最小高度 44px

### Task 2.4: 弹窗响应式 ✓
- [x] Mobile 全屏弹窗
- [x] 标题栏和操作栏固定
- [x] 内容区域可滚动

### Task 2.5: 测试 ✓
- [x] iPhone SE (375px)
- [x] iPad (768px)
- [x] Desktop (1920px)

## Phase 3: 移动端高级适配 (P2) ✓ 完成

### Task 3.1: 卡片式布局 ✓
- [x] 表格在移动端改为卡片式展示（通过 .card-style-table 类启用）

### Task 3.2: 底部抽屉弹窗 ✓
- [x] 弹窗从底部滑入动画

### Task 3.3: 触摸优化 ✓
- [x] 禁用 hover 效果
- [x] 添加 active 状态反馈

## Phase 4: 部署验证 ✓ 完成

### Task 4.1: VM 环境部署 ✓
- [x] 编译 Windows 版本
- [x] 上传到 VM 环境 (10.21.20.65)
- [x] 重启服务
- [x] 验证 Mesh CRUD 功能
- [x] 验证移动端适配

### Task 4.2: QG 环境部署 ✓
- [x] 编译 Linux 版本
- [x] 上传到 QG 环境 (10.11.61.40)
- [x] 重启服务 (rc-service phaethon restart)
- [x] 验证 API 端点 (/api/mesh/domain-suffixes, /api/mesh/advertise)
- [x] 服务状态正常 (status: started)
