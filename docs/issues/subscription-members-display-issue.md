# 订阅节点前端显示问题修复总结

**日期**: 2026-10-26  
**状态**: 修复中

## 问题描述

前端控制台无法显示代理组中的手动节点和订阅节点。

## 根因分析

### 1. Members 字段不持久化

提交 `ef0f966` (Oct 23 14:52) "Phase 3 - eliminate s.runtime cache" 移除了运行时缓存，改为直接查询数据库。但：

- `ProxyGroup.Members` 标记为 `yaml:"-" json:"members,omitempty"`
- 该字段**不存储到数据库**，需要在运行时计算
- `db.GetProxyGroup()` 返回的组，`Members` 为空

### 2. Members 需要运行时计算

```go
// config/config.go
type ProxyGroup struct {
    ManualProxies []string       // 持久化：手动节点名称列表
    Subscription  string         // 持久化：订阅名称
    
    // 运行时字段（不持久化）
    Members       []GroupMember  // 需要 RebuildProxies() 计算
    ManualMembers []GroupMember  // 需要 RebuildProxies() 计算
    SubMembers    []GroupMember  // 需要 RebuildProxies() 计算
}
```

### 3. RebuildProxies() 依赖回调注入

```go
func (g *ProxyGroup) rebuildProxiesLocked() {
    // 需要这些回调才能工作
    if g.ResolveProxy != nil && g.ResolveProxy(name) != nil {
        g.ManualMembers = append(...)
    }
    if g.ResolveSubscription != nil {
        // 从订阅获取节点
    }
}
```

### 4. RuleMatcher 负责注入回调

```go
// db/matcher.go init()
for _, group := range m.groups {
    group.ResolveProxy = func(name string) *config.Proxy {
        return m.proxies[name]
    }
    group.ResolveGroup = func(name string) *config.ProxyGroup {
        return m.groups[name]
    }
    group.ResolveSubscription = func(name string) *config.Subscription {
        return m.subscriptions[name]
    }
}
for _, group := range m.groups {
    group.RebuildProxies()  // 构建 Members
}
```

## 订阅流程简图

```
┌─────────────────────────────────────────────────────────┐
│                    数据库 (BoltDB)                       │
│  ┌──────────────────┐  ┌─────────────────────────────┐ │
│  │ BucketSubscriptions│  │ BucketSubscriptionNodes   │ │
│  │  - 订阅元数据      │  │  - 订阅节点池              │ │
│  │  - URL/间隔        │  │  - map[string]*Proxy       │ │
│  └──────────────────┘  └─────────────────────────────┘ │
│  ┌──────────────────┐  ┌─────────────────────────────┐ │
│  │ BucketProxyGroups│  │ BucketProxies               │ │
│  │  - 组配置          │  │  - 代理配置                 │ │
│  │  - ManualProxies  │  │  - 类型/服务器/端口等        │ │
│  │  - Subscription   │  │                             │ │
│  └──────────────────┘  └─────────────────────────────┘ │
└─────────────────────────────────────────────────────────┘
                            ↓ 启动时加载 + Watch 事件
┌─────────────────────────────────────────────────────────┐
│              RuleMatcher (运行时状态)                    │
│  ┌──────────────────────────────────────────────────┐  │
│  │ 1. 加载所有代理、组、订阅、订阅节点                │  │
│  │ 2. 注入回调：ResolveProxy/Group/Subscription      │  │
│  │ 3. 调用 RebuildProxies() 构建 Members             │  │
│  └──────────────────────────────────────────────────┘  │
│                          ↓                               │
│  ┌──────────────────────────────────────────────────┐  │
│  │ Watch 事件处理：                                   │  │
│  │ - 代理变更 → 重建所有组的 Members                  │  │
│  │ - 组变更 → 注入回调 + RebuildProxies()             │  │
│  │ - 订阅变更 → 重建所有组的 Members                  │  │
│  │ - 订阅节点变更 → 重建引用该订阅的组的 Members       │  │
│  └──────────────────────────────────────────────────┘  │
└─────────────────────────────────────────────────────────┘
                            ↓ GetGroup(name)
┌─────────────────────────────────────────────────────────┐
│              Admin API (前端查询)                        │
│  GET /api/groups/{name}/members                         │
│  → 从 RuleMatcher 获取组（包含完整的 Members）           │
│  → buildGroupMemberList() 格式化                         │
│  → 返回给前端                                            │
└─────────────────────────────────────────────────────────┘
```

## 修复方案

### 已完成的修复

1. **添加 `RuleMatcher.GetGroup()` 方法**
   ```go
   func (m *RuleMatcher) GetGroup(name string) *config.ProxyGroup {
       m.mu.RLock()
       defer m.mu.RUnlock()
       return m.groups[name]
   }
   ```

2. **Admin API 优先从 RuleMatcher 获取组**
   ```go
   // admin/admin.go
   g, err := db.GetProxyGroup(groupName)
   matcher, err := db.GetMatcher()
   if err == nil && matcher != nil {
       if runtimeGroup := matcher.GetGroup(groupName); runtimeGroup != nil {
           g = runtimeGroup  // 使用包含 Members 的运行时版本
       }
   }
   ```

3. **Watch 函数注入回调并重建 Members**
   ```go
   // db/matcher.go onConfigChange()
   case string(BucketProxyGroups):
       group.ResolveProxy = func(name string) *config.Proxy {
           return m.proxies[name]
       }
       group.ResolveGroup = func(name string) *config.ProxyGroup {
           return m.groups[name]
       }
       group.ResolveSubscription = func(name string) *config.Subscription {
           return m.subscriptions[name]
       }
       group.RebuildProxies()
   ```

4. **代理/订阅变更时重建所有组的 Members**
   ```go
   case string(BucketProxies):
       // ... 更新代理 ...
       for _, group := range m.groups {
           group.RebuildProxies()
       }
   ```

### 待验证

- [ ] RuleMatcher 是否正确初始化
- [ ] Watch 事件是否触发
- [ ] Members 是否正确构建
- [ ] 前端 API 是否返回正确的成员列表

## 测试步骤

1. 访问前端控制台
2. 展开包含手动节点的代理组（如 Company、Country）
3. 检查是否显示手动节点
4. 展开包含订阅节点的代理组（如 Starlink-0）
5. 检查是否显示订阅节点

## 相关文件

- `db/matcher.go` - RuleMatcher 实现
- `admin/admin.go` - Admin API 实现
- `config/config.go` - ProxyGroup 结构定义
- `db/runtime_state.go` - 数据库操作
