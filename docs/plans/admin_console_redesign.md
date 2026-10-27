# 控制台菜单重构与 UX 优化设计

**版本**: v1.0  
**日期**: 2026-10-27  
**状态**: 待评审

## 1. 背景与目标

### 1.1 当前问题

1. **菜单结构混乱**
   - 12 个菜单项平铺，缺乏清晰的逻辑分组
   - Runtime / Configuration / Tools 三组划分不合理
   - 订阅（Subscriptions）和代理（Proxies）分离，但用户心智中是一体的

2. **UX 反馈不足**
   - 菜单切换时无加载提示，用户不确定操作是否生效
   - 版本更新触发刷新时无提示，用户困惑
   - 移动端体验差：侧边栏占用空间，Tab 过多

3. **功能 Bug**
   - 活跃连接页面（Connections）无数据显示
   - 订阅组成员展示显示为 0，实际有成员

### 1.2 设计目标

1. **菜单重构**：11 个菜单项，4 个逻辑分组（配置 / 网络 / 监控 / 系统）
2. **响应式设计**：PC 端侧边栏 + 移动端底部导航
3. **UX 优化**：所有异步操作都有明确的加载状态反馈
4. **Bug 修复**：修复连接和订阅成员显示问题

## 2. 菜单结构设计

### 2.1 最终菜单（PC 端侧边栏）

```
📊 Dashboard          概览
─── 配置 ───
🔌 Proxies            代理（含订阅管理）
📜 Rules              规则
🗺️ Mappings          端口映射
─── 网络 ───
🌐 TUN                TUN 隧道
🔷 Mesh               Mesh 网络
↗️ Resolvers          DNS 解析
─── 监控 ───
🔗 Connections        活跃连接
📋 Logs               日志
─── 系统 ───
⚙️ Admin              系统设置
🧙 Reverse            反向连接
📦 Package            部署包
```

### 2.2 移动端底部导航（5 项）

```
[📊 Dashboard] [🔌 Proxies] [📜 Rules] [🔗 Monitor] [⋯ More]
```

**More 抽屉菜单**：
- Mappings
- TUN
- Mesh
- Resolvers
- Admin
- Reverse
- Package

### 2.3 页面调整

#### Proxies 页面（整合订阅）

**布局**：
```
┌─────────────────────────────────────┐
│ 订阅源 [▼ 折叠/展开]                │
│ ┌─────────────────────────────────┐ │
│ │ • 订阅1 (100 节点) [刷新] [编辑]│ │
│ │ • 订阅2 (50 节点)  [刷新] [编辑]│ │
│ │ [+ 添加订阅]                    │ │
│ └─────────────────────────────────┘ │
├─────────────────────────────────────┤
│ 代理列表                            │
│ [筛选: 全部 | 订阅 | 手动]          │
│ ┌─────────────────────────────────┐ │
│ │ • Proxy1 (trojan) [编辑] [删除] │ │
│ │ • Proxy2 (socks5) [编辑] [删除] │ │
│ └─────────────────────────────────┘ │
├─────────────────────────────────────┤
│ 代理分组                            │
│ ┌─────────────────────────────────┐ │
│ │ • Group1 [编辑] [删除]          │ │
│ └─────────────────────────────────┘ │
└─────────────────────────────────────┘
```

**移动端适配**：
- 订阅源区域默认折叠，点击展开
- 代理列表使用卡片式布局
- 筛选器改为下拉菜单

#### Monitor 页面（移动端合并 Connections + Logs）

**PC 端**：保持两个独立菜单项

**移动端**：合并为一个 Monitor 页面
```
┌─────────────────────────────────────┐
│ [连接] [日志]  ← Tab 切换           │
├─────────────────────────────────────┤
│ （当前 Tab 内容）                   │
└─────────────────────────────────────┘
```

## 3. UX 优化设计

### 3.1 菜单切换加载指示器

**触发时机**：
- 用户点击菜单项
- HTMX 发起请求前

**UI 设计**：
```
┌─────────────────────────────────────┐
│ ████████████░░░░░░░░░░░░░░░░░░░░░░░ │ ← 顶部进度条
│                                     │
│         🔄 加载中...                │ ← 页面中央 spinner
│                                     │
└─────────────────────────────────────┘
```

**实现方案**：
1. 使用 HTMX 的 `htmx:before-request` 事件显示加载指示器
2. 使用 `htmx:after-settle` 事件隐藏加载指示器
3. 进度条使用 CSS 动画，不依赖实际进度

**代码位置**：
- `admin/static/app.js`: 添加全局 HTMX 事件监听
- `admin/static/style.css`: 添加加载指示器样式

### 3.2 版本更新刷新指示器

**触发时机**：
- 检测到新版本
- 用户确认刷新

**UI 设计**：
```
┌─────────────────────────────────────┐
│ 🔄 正在更新到 v0.8.1...             │
│    请勿关闭页面                     │
│                                     │
│ ████████████░░░░░░░░░░░░░░░░░░░░░░░ │
└─────────────────────────────────────┘
```

**实现方案**：
1. 刷新前显示全屏遮罩层
2. 使用 `beforeunload` 事件阻止用户意外关闭
3. 刷新完成后自动隐藏

**代码位置**：
- `admin/static/app.js`: 修改版本更新逻辑
- `admin/templates/layout.html`: 添加遮罩层 HTML

### 3.3 响应式断点

```css
/* 移动端 */
@media (max-width: 768px) {
  .sidebar { display: none; }
  .bottom-nav { display: flex; }
}

/* PC 端 */
@media (min-width: 769px) {
  .sidebar { display: block; }
  .bottom-nav { display: none; }
}
```

## 4. Bug 修复

### 4.1 活跃连接无数据

**问题分析**：
- 可能原因 1：API 返回数据格式错误
- 可能原因 2：前端解析错误
- 可能原因 3：后端未正确收集连接信息

**诊断步骤**：
1. 检查 `/api/connections` 返回值
2. 检查前端 `fetchConnections()` 函数
3. 检查后端 `handleConnections()` 逻辑

**修复方案**：
- 根据诊断结果修复对应层

### 4.2 订阅组成员显示为 0

**问题分析**：
- 可能原因 1：API 返回的 `members` 字段为空
- 可能原因 2：前端未正确读取 `members` 字段
- 可能原因 3：数据库查询逻辑错误

**诊断步骤**：
1. 检查 `/api/subscriptions` 返回值
2. 检查前端渲染逻辑
3. 检查后端 `ListSubscriptions()` 函数

**修复方案**：
- 根据诊断结果修复对应层

## 5. 实施计划

### 阶段 1：Bug 修复（优先级最高）

**任务 1.1**: 诊断并修复活跃连接无数据  
**任务 1.2**: 诊断并修复订阅组成员显示为 0

**预计工时**: 2-3 小时

### 阶段 2：UX 优化

**任务 2.1**: 实现菜单切换加载指示器  
**任务 2.2**: 实现版本更新刷新指示器

**预计工时**: 2-3 小时

### 阶段 3：菜单重构

**任务 3.1**: 调整菜单分组（layout.html）  
**任务 3.2**: 整合订阅到 Proxies 页面  
**任务 3.3**: 实现移动端底部导航  
**任务 3.4**: 实现 More 抽屉菜单  
**任务 3.5**: 移动端 Monitor 页面合并

**预计工时**: 6-8 小时

### 阶段 4：测试与优化

**任务 4.1**: PC 端功能测试  
**任务 4.2**: 移动端功能测试  
**任务 4.3**: 性能优化  
**任务 4.4**: 文档更新

**预计工时**: 3-4 小时

**总预计工时**: 13-18 小时

## 6. 技术细节

### 6.1 HTMX 加载指示器

```javascript
// admin/static/app.js
document.body.addEventListener('htmx:before-request', function(evt) {
  showLoadingIndicator();
});

document.body.addEventListener('htmx:after-settle', function(evt) {
  hideLoadingIndicator();
});

function showLoadingIndicator() {
  // 显示顶部进度条
  const progressBar = document.getElementById('loading-progress');
  if (progressBar) {
    progressBar.classList.remove('hidden');
    progressBar.style.width = '0%';
    // 动画到 80%
    setTimeout(() => {
      progressBar.style.width = '80%';
    }, 100);
  }
  
  // 显示中央 spinner
  const spinner = document.getElementById('loading-spinner');
  if (spinner) {
    spinner.classList.remove('hidden');
  }
}

function hideLoadingIndicator() {
  // 完成进度条
  const progressBar = document.getElementById('loading-progress');
  if (progressBar) {
    progressBar.style.width = '100%';
    setTimeout(() => {
      progressBar.classList.add('hidden');
    }, 300);
  }
  
  // 隐藏 spinner
  const spinner = document.getElementById('loading-spinner');
  if (spinner) {
    spinner.classList.add('hidden');
  }
}
```

### 6.2 移动端底部导航

```html
<!-- admin/templates/layout.html -->
<nav class="bottom-nav">
  <a href="./" class="nav-item {{if eq .Title "Dashboard"}}active{{end}}">
    <span class="nav-icon">📊</span>
    <span class="nav-label">Dashboard</span>
  </a>
  <a href="./proxies" class="nav-item {{if eq .Title "Proxies"}}active{{end}}">
    <span class="nav-icon">🔌</span>
    <span class="nav-label">Proxies</span>
  </a>
  <a href="./rules" class="nav-item {{if eq .Title "Rules"}}active{{end}}">
    <span class="nav-icon">📜</span>
    <span class="nav-label">Rules</span>
  </a>
  <a href="./monitor" class="nav-item {{if eq .Title "Monitor"}}active{{end}}">
    <span class="nav-icon">🔗</span>
    <span class="nav-label">Monitor</span>
  </a>
  <button onclick="toggleMoreMenu()" class="nav-item">
    <span class="nav-icon">⋯</span>
    <span class="nav-label">More</span>
  </button>
</nav>

<!-- More 抽屉菜单 -->
<div id="more-menu" class="more-menu hidden">
  <div class="more-menu-header">
    <h3>更多功能</h3>
    <button onclick="toggleMoreMenu()" class="close-btn">×</button>
  </div>
  <div class="more-menu-items">
    <a href="./mappings">🗺️ 端口映射</a>
    <a href="./tun">🌐 TUN 隧道</a>
    <a href="./mesh">🔷 Mesh 网络</a>
    <a href="./resolvers">↗️ DNS 解析</a>
    <a href="./admin">⚙️ 系统设置</a>
    <a href="./reverse">🧙 反向连接</a>
    <a href="./package">📦 部署包</a>
  </div>
</div>
```

### 6.3 Proxies 页面整合订阅

```html
<!-- admin/templates/proxies.html -->
<div class="card">
  <div class="card-header">
    <h3>📡 订阅源</h3>
    <button onclick="toggleSubscriptions()" class="btn btn-sm btn-outline">
      <span id="subscriptions-toggle-icon">▼</span>
    </button>
  </div>
  <div id="subscriptions-section" class="card-body">
    <!-- 订阅列表 -->
    {{range .Subscriptions}}
    <div class="subscription-item">
      <span>{{.Name}} ({{.NodeCount}} 节点)</span>
      <button onclick="refreshSubscription({{.ID}})" class="btn btn-sm">刷新</button>
      <button onclick="editSubscription({{.ID}})" class="btn btn-sm">编辑</button>
    </div>
    {{end}}
    <button onclick="addSubscription()" class="btn btn-primary btn-sm">+ 添加订阅</button>
  </div>
</div>

<div class="card">
  <div class="card-header">
    <h3>🔌 代理列表</h3>
  </div>
  <div class="card-body">
    <!-- 代理列表 -->
  </div>
</div>
```

## 7. 验收标准

### 7.1 功能验收

- [ ] 菜单分组正确（配置 / 网络 / 监控 / 系统）
- [ ] Proxies 页面包含订阅管理
- [ ] 移动端底部导航正常工作
- [ ] More 抽屉菜单正常工作
- [ ] 活跃连接页面显示数据
- [ ] 订阅组成员显示正确

### 7.2 UX 验收

- [ ] 菜单切换时显示加载指示器
- [ ] 版本更新刷新时显示提示
- [ ] PC 端侧边栏正常显示
- [ ] 移动端侧边栏隐藏，底部导航显示
- [ ] 所有按钮触控区域足够大（≥44px）

### 7.3 性能验收

- [ ] 页面切换时间 < 500ms
- [ ] 加载指示器不卡顿
- [ ] 移动端滚动流畅

## 8. 风险与缓解

### 8.1 风险

1. **移动端兼容性问题**：不同浏览器对 CSS Grid/Flexbox 支持不同
2. **HTMX 事件冲突**：加载指示器可能与现有 HTMX 逻辑冲突
3. **数据迁移问题**：整合订阅到 Proxies 页面可能需要调整数据结构

### 8.2 缓解措施

1. 使用 CSS 前缀和 fallback
2. 充分测试 HTMX 事件
3. 保持后端 API 不变，只调整前端展示

## 9. 后续优化

1. **深色模式**：支持深色主题
2. **快捷键**：PC 端支持键盘快捷键导航
3. **离线缓存**：移动端支持 PWA
4. **手势支持**：移动端支持左右滑动切换页面
