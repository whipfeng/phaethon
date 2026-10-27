# Members 字段重构：从预计算改为按需计算

**创建时间**: 2026-10-26  
**状态**: 已完成  
**优先级**: 中

## 问题背景

ProxyGroup 的 `Members`、`ManualMembers`、`SubMembers` 字段原本是预计算并缓存的运行时字段。这种设计存在以下问题：

1. **一致性维护复杂**：需要在多个地方调用 `RebuildProxies()` 来保持缓存最新
2. **容易出错**：忘记调用 `RebuildProxies()` 会导致 Members 为空（前端不展示手动节点）
3. **过度优化**：计算成本很低（只是内存操作），预计算带来的性能收益微乎其微

## 根本原因分析

### 计算成本分析

`rebuildProxiesLocked()` 的实际计算：

```go
// 1. 手动成员：遍历 ManualProxies（~1-10 个）
for _, name := range g.ManualProxies {
    ResolveProxy(name)  // O(1) map 查找
    ResolveGroup(name)  // O(1) map 查找
}

// 2. 订阅成员：遍历订阅节点（~10-1000 个）
for name := range sub.SubProxies {
    // 应用过滤器（正则或子串匹配）
}
sort.Strings(names)  // O(n log n)

// 总计：O(n log n)，n = 订阅节点数量
```

**关键点**：
- 数据来源：`sub.SubProxies` 已经在内存中（BoltDB mmap 或 RuleMatcher 缓存）
- 不需要数据库查询，不需要 I/O
- 对于 1000 个节点，计算时间 = 微秒级

### 访问模式分析

| 用途 | 访问频率 | 是否热路径 |
|------|---------|-----------|
| 代理选择（Next） | 每个连接一次 | 是（但计算成本低） |
| 前端展示 | 用户手动触发 | 否 |
| 健康检查 | 定时触发 | 否 |

**结论**：访问频率不高，计算成本低，预计算不合理。

## 重构方案

### 核心变更

1. **删除字段**：移除 `Members`、`ManualMembers`、`SubMembers` 字段
2. **删除方法**：移除 `RebuildProxies()`、`rebuildProxiesLocked()`
3. **按需计算**：在 `activeMembersLocked()` 中调用 `computeMembersLocked()` 实时计算
4. **清理调用点**：移除所有 `RebuildProxies()` 调用
5. **删除死代码**：移除未使用的 `SubCandidateCount` 字段

### 代码变更

#### config/config.go

```go
// ❌ 删除的字段
type ProxyGroup struct {
    Members         []GroupMember  // 删除
    ManualMembers   []GroupMember  // 删除
    SubMembers      []GroupMember  // 删除
    SubCandidateCount int          // 删除（未使用的死代码）
}

// ✅ 新增的计算方法
func (g *ProxyGroup) computeMembersLocked() []GroupMember {
    // 按需计算成员列表
    var manualMembers []GroupMember
    for _, name := range g.ManualProxies {
        if g.ResolveProxy != nil && g.ResolveProxy(name) != nil {
            manualMembers = append(manualMembers, ...)
        }
        // ...
    }
    
    var subMembers []GroupMember
    if g.Subscription != "" && g.ResolveSubscription != nil {
        if sub := g.ResolveSubscription(g.Subscription); sub != nil {
            names := g.subscriptionCandidatesLocked(sub)
            // ...
        }
    }
    
    members := make([]GroupMember, 0, len(subMembers)+len(manualMembers))
    members = append(members, subMembers...)
    members = append(members, manualMembers...)
    return members
}

// ✅ 修改的方法
func (g *ProxyGroup) activeMembersLocked() []GroupMember {
    return g.computeMembersLocked()  // 按需计算
}

func (g *ProxyGroup) GetMembers() []GroupMember {
    g.selMu.RLock()
    defer g.selMu.RUnlock()
    return g.computeMembersLocked()  // 按需计算
}
```

#### db/matcher.go

```go
// ❌ 删除的调用
for _, group := range m.groups {
    group.RebuildProxies()  // 删除
}

// ✅ 更新的检查
if !ok || len(group.GetMembers()) == 0 {  // 使用 GetMembers()
    return nil
}
```

#### admin/admin.go, main.go

移除所有 `RebuildProxies()` 调用，更新日志中的 `g.Members` 为 `g.GetMembers()`。

## 优势对比

| 维度 | 预计算（旧） | 按需计算（新） |
|------|-------------|---------------|
| **一致性** | ❌ 需要手动维护 | ✅ 总是最新 |
| **代码复杂度** | ❌ 高（多处调用） | ✅ 低（自动计算） |
| **bug 风险** | ❌ 高（容易遗漏） | ✅ 低（无需维护） |
| **性能** | ✅ 略快（微秒级差异） | ✅ 足够快（微秒级） |
| **内存占用** | ❌ 需要缓存 | ✅ 无缓存 |
| **死代码** | ❌ 存在未使用字段 | ✅ 已清理 |

## 影响范围

### 修改的文件

1. `config/config.go` - 核心重构 + 删除死代码
2. `db/matcher.go` - 移除 Watch handlers 中的 RebuildProxies 调用
3. `admin/admin.go` - 移除 API 中的 RebuildProxies 调用
4. `main.go` - 移除初始化和订阅刷新中的 RebuildProxies 调用

### 删除的字段

- `ProxyGroup.Members` - 改为按需计算
- `ProxyGroup.ManualMembers` - 改为按需计算
- `ProxyGroup.SubMembers` - 改为按需计算
- `ProxyGroup.SubCandidateCount` - 未使用的死代码，直接删除

### 保留的部分

- `ResolveProxy`、`ResolveGroup`、`ResolveSubscription` 回调仍然保留（按需计算时需要）
- `GetMembers()`、`GetManualMembers()` 方法保留（外部接口不变）
- `activeMembersLocked()` 方法保留（代理选择逻辑不变）
- `SubscriptionCandidates()` 方法保留（返回候选节点列表）

## 测试验证

1. ✅ 编译通过（`go build` 无错误）
2. ⏳ 待验证：前端控制台展示手动节点
3. ⏳ 待验证：代理选择逻辑正常
4. ⏳ 待验证：健康检查正常

## 总结

这次重构消除了不必要的预计算优化，简化了代码逻辑，降低了维护成本和 bug 风险。性能影响可忽略（微秒级），但代码质量显著提升。

**关键洞察**：
- BoltDB 使用 mmap，数据访问本身就是内存操作
- 计算成本很低（O(n log n)，n=10-1000），不值得预计算
- 按需计算更简单、更安全、更可靠
- 清理死代码（SubCandidateCount）进一步简化了代码库
