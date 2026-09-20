# Admin Console Mobile Adaptation & Mesh UX Design

> 版本: v0.1.0
> 日期: 2026-09-20
> 状态: DRAFT
> 负责人: Phaethon Dev
> 依赖: [admin_mobile_mesh_spec.md](../specs/admin_mobile_mesh_spec.md)

## 变更记录

| 版本 | 日期 | 变更内容 | 作者 |
|------|------|----------|------|
| v0.1.0 | 2026-09-20 | 初始版本 | Qoder |

## 1. 概述

本设计文档描述如何实现 Admin 控制台的移动端适配和 Mesh 页面交互重构。

## 2. 移动端适配设计

### 2.1 CSS 媒体查询策略

采用移动优先（Mobile First）策略，从最小屏幕开始设计，逐步增强：

```css
/* 基础样式（Mobile < 768px） */
.sidebar { display: none; }
.bottom-nav { display: flex; }

/* Tablet (≥ 768px) */
@media (min-width: 768px) {
    .sidebar { display: block; width: 60px; }
    .bottom-nav { display: none; }
}

/* Desktop (≥ 1024px) */
@media (min-width: 1024px) {
    .sidebar { width: 220px; }
}
```

### 2.2 底部导航栏设计

**结构**:
```html
<nav class="bottom-nav">
    <a href="./" class="bottom-nav-item active">
        <span class="icon">📊</span>
        <span class="label">Dashboard</span>
    </a>
    <a href="./tun" class="bottom-nav-item">
        <span class="icon">🌐</span>
        <span class="label">TUN</span>
    </a>
    <a href="./mesh" class="bottom-nav-item">
        <span class="icon">🔷</span>
        <span class="label">Mesh</span>
    </a>
    <a href="./logs" class="bottom-nav-item">
        <span class="icon">📋</span>
        <span class="label">Logs</span>
    </a>
    <a href="./config" class="bottom-nav-item">
        <span class="icon">⚙️</span>
        <span class="label">Config</span>
    </a>
</nav>
```

**样式**:
- 固定底部，高度 56px
- 5 个等宽项目
- 图标在上，文字在下
- 选中状态高亮

### 2.3 表格响应式

**方案**: 横向滚动 + 关键列固定

```css
.table-responsive {
    overflow-x: auto;
    -webkit-overflow-scrolling: touch;
}

.data-table {
    min-width: 600px; /* 确保表格不会太窄 */
}

/* 第一列和最后一列固定 */
.data-table th:first-child,
.data-table td:first-child {
    position: sticky;
    left: 0;
    background: var(--bg-primary);
    z-index: 1;
}

.data-table th:last-child,
.data-table td:last-child {
    position: sticky;
    right: 0;
    background: var(--bg-primary);
    z-index: 1;
}
```

### 2.4 弹窗响应式

**Mobile**: 全屏弹窗
```css
@media (max-width: 768px) {
    .modal-content {
        width: 100%;
        height: 100%;
        max-width: none;
        max-height: none;
        border-radius: 0;
    }
    
    .modal-header {
        position: sticky;
        top: 0;
        background: var(--bg-primary);
        z-index: 10;
    }
    
    .form-actions {
        position: sticky;
        bottom: 0;
        background: var(--bg-primary);
        z-index: 10;
    }
}
```

### 2.5 表单响应式

```css
/* Mobile: 堆叠 */
.form-row {
    flex-direction: column;
    gap: 1rem;
}

/* Tablet+: 并排 */
@media (min-width: 768px) {
    .form-row {
        flex-direction: row;
        gap: 1rem;
    }
}

/* 触摸优化 */
.btn, input, select, textarea {
    min-height: 44px;
}
```

## 3. Mesh 页面交互重构设计

### 3.1 页面结构

```html
<!-- Domain Suffix Routes 区域 -->
<div class="mesh-section">
    <div class="mesh-section-header">
        <h4>Domain Suffix Routes</h4>
        <button onclick="showAddDomainSuffix()" class="btn btn-primary btn-sm">+ Add</button>
    </div>
    <table class="data-table">
        <thead>
            <tr>
                <th>Domain Suffix</th>
                <th>Actions</th>
            </tr>
        </thead>
        <tbody id="domain-suffixes-tbody">
            <!-- 动态生成 -->
        </tbody>
    </table>
</div>

<!-- Advertise Routes 区域 -->
<div class="mesh-section">
    <div class="mesh-section-header">
        <h4>Advertise Routes</h4>
        <button onclick="showAddAdvertise()" class="btn btn-primary btn-sm">+ Add</button>
    </div>
    <table class="data-table">
        <thead>
            <tr>
                <th>CIDR</th>
                <th>Actions</th>
            </tr>
        </thead>
        <tbody id="advertise-tbody">
            <!-- 动态生成 -->
        </tbody>
    </table>
</div>
```

### 3.2 后端 API 设计

**Domain Suffixes**:

```go
// GET /api/mesh/domain-suffixes
func (s *AdminServer) apiMeshDomainSuffixesGet(w http.ResponseWriter, r *http.Request) {
    s.mu.RLock()
    suffixes := []string{}
    if s.conf.Mesh != nil {
        suffixes = s.conf.Mesh.DomainSuffixes
    }
    s.mu.RUnlock()
    jsonResponse(w, suffixes)
}

// POST /api/mesh/domain-suffixes
func (s *AdminServer) apiMeshDomainSuffixesPost(w http.ResponseWriter, r *http.Request) {
    var req struct {
        Suffix string `json:"suffix"`
    }
    if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
        httpError(w, "parse fail", http.StatusBadRequest)
        return
    }
    // 验证 suffix
    if req.Suffix == "" {
        httpError(w, "suffix required", http.StatusBadRequest)
        return
    }
    
    s.mu.Lock()
    if s.conf.Mesh != nil {
        s.conf.Mesh.DomainSuffixes = append(s.conf.Mesh.DomainSuffixes, req.Suffix)
        if err := s.saveConfigLocked(); err != nil {
            s.mu.Unlock()
            httpError(w, "save fail", http.StatusInternalServerError)
            return
        }
        // 更新 mesh manager
        mesh.GlobalMeshManager.UpdateConfig(s.conf.Mesh.DomainSuffixes, s.conf.Mesh.Advertise)
    }
    s.mu.Unlock()
    
    jsonResponse(w, map[string]interface{}{"ok": true})
}

// DELETE /api/mesh/domain-suffixes/:index
func (s *AdminServer) apiMeshDomainSuffixesDelete(w http.ResponseWriter, r *http.Request) {
    indexStr := strings.TrimPrefix(r.URL.Path, "/api/mesh/domain-suffixes/")
    index, err := strconv.Atoi(indexStr)
    if err != nil {
        httpError(w, "invalid index", http.StatusBadRequest)
        return
    }
    
    s.mu.Lock()
    if s.conf.Mesh != nil && index >= 0 && index < len(s.conf.Mesh.DomainSuffixes) {
        s.conf.Mesh.DomainSuffixes = append(
            s.conf.Mesh.DomainSuffixes[:index],
            s.conf.Mesh.DomainSuffixes[index+1:]...,
        )
        if err := s.saveConfigLocked(); err != nil {
            s.mu.Unlock()
            httpError(w, "save fail", http.StatusInternalServerError)
            return
        }
        mesh.GlobalMeshManager.UpdateConfig(s.conf.Mesh.DomainSuffixes, s.conf.Mesh.Advertise)
    }
    s.mu.Unlock()
    
    jsonResponse(w, map[string]interface{}{"ok": true})
}
```

**Advertise**: 类似逻辑，验证 CIDR 格式。

### 3.3 前端 JavaScript

```javascript
// 加载 Domain Suffixes
async function loadDomainSuffixes() {
    const res = await fetch('./api/mesh/domain-suffixes');
    const suffixes = await res.json();
    const tbody = document.getElementById('domain-suffixes-tbody');
    tbody.innerHTML = suffixes.map((suffix, idx) => `
        <tr>
            <td>${escapeHtml(suffix)}</td>
            <td>
                <button onclick="editDomainSuffix(${idx})" class="btn btn-sm btn-outline">Edit</button>
                <button onclick="deleteDomainSuffix(${idx})" class="btn btn-sm btn-danger">Delete</button>
            </td>
        </tr>
    `).join('');
}

// 添加 Domain Suffix
function showAddDomainSuffix() {
    document.getElementById('domain-suffix-modal-title').textContent = 'Add Domain Suffix';
    document.getElementById('domain-suffix-edit-idx').value = '-1';
    document.getElementById('domain-suffix-form').reset();
    document.getElementById('domain-suffix-modal').classList.remove('hidden');
}

// 保存 Domain Suffix
async function saveDomainSuffix(e) {
    e.preventDefault();
    const idx = parseInt(document.getElementById('domain-suffix-edit-idx').value);
    const suffix = document.getElementById('domain-suffix-input').value.trim();
    
    if (idx >= 0) {
        // Edit
        await fetch(`./api/mesh/domain-suffixes/${idx}`, {
            method: 'PUT',
            headers: {'Content-Type': 'application/json'},
            body: JSON.stringify({suffix})
        });
    } else {
        // Add
        await fetch('./api/mesh/domain-suffixes', {
            method: 'POST',
            headers: {'Content-Type': 'application/json'},
            body: JSON.stringify({suffix})
        });
    }
    
    closeDomainSuffixModal();
    loadDomainSuffixes();
}

// 删除 Domain Suffix
async function deleteDomainSuffix(idx) {
    if (!confirm('Delete this domain suffix?')) return;
    await fetch(`./api/mesh/domain-suffixes/${idx}`, {method: 'DELETE'});
    loadDomainSuffixes();
}
```

### 3.4 弹窗 HTML

```html
<div id="domain-suffix-modal" class="modal hidden">
    <div class="modal-content modal-sm">
        <div class="modal-header">
            <h3 id="domain-suffix-modal-title">Add Domain Suffix</h3>
            <button onclick="closeDomainSuffixModal()" class="close-btn">&times;</button>
        </div>
        <form id="domain-suffix-form" onsubmit="saveDomainSuffix(event)">
            <input type="hidden" id="domain-suffix-edit-idx" value="-1">
            <div class="form-group">
                <label>Domain Suffix *</label>
                <input type="text" id="domain-suffix-input" required placeholder="example.com">
            </div>
            <div class="form-actions">
                <button type="button" onclick="closeDomainSuffixModal()" class="btn btn-outline">Cancel</button>
                <button type="submit" class="btn btn-primary">Save</button>
            </div>
        </form>
    </div>
</div>
```

## 4. 实现步骤

### Phase 1: Mesh 页面交互重构（P0）

1. 后端：添加 Domain Suffixes CRUD API
2. 后端：添加 Advertise CRUD API
3. 前端：重构 mesh.html 页面结构
4. 前端：实现 CRUD JavaScript 逻辑
5. 前端：添加弹窗组件
6. 测试：验证增删改查功能

### Phase 2: 移动端基础适配（P1）

1. CSS：添加媒体查询断点
2. CSS：实现底部导航栏
3. CSS：表格横向滚动
4. CSS：表单堆叠布局
5. CSS：弹窗全屏
6. 测试：iPhone SE、iPad、Desktop

### Phase 3: 移动端高级适配（P2）

1. CSS：卡片式表格布局（可选）
2. CSS：底部抽屉弹窗（可选）
3. 测试：触摸交互优化

## 5. 风险与缓解

| 风险 | 缓解措施 |
|------|----------|
| 移动端浏览器兼容性 | 使用标准 CSS，避免实验性特性 |
| 横向滚动性能 | 使用 `-webkit-overflow-scrolling: touch` |
| 弹窗高度溢出 | 使用 `overflow-y: auto` 限制内容区域 |
| API 向后兼容 | 保留 `PATCH /api/mesh/config`，新增 CRUD API |

## 6. 测试计划

### 6.1 功能测试

- [ ] Domain Suffix 增删改查
- [ ] Advertise CIDR 增删改查
- [ ] 配置持久化（重启后保留）
- [ ] Mesh manager 同步更新

### 6.2 响应式测试

- [ ] iPhone SE (375x667)
- [ ] iPhone 12 (390x844)
- [ ] iPad (768x1024)
- [ ] iPad Pro (1024x1366)
- [ ] Desktop (1920x1080)

### 6.3 浏览器测试

- [ ] Chrome (iOS/Android/Desktop)
- [ ] Safari (iOS/Desktop)
- [ ] Firefox (Desktop)
