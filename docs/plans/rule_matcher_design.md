# RuleMatcher 设计文档

**创建时间**: 2026-10-23  
**状态**: 设计中  
**优先级**: 高

## 背景

当前系统使用 `RuleConfiguration` 进行规则匹配，存在以下问题：

1. **名字不准确**：`RuleConfiguration` 包含了太多无关的配置（Admin, TUN, Mesh, ReverseConfigs 等）
2. **一次性加载**：启动时加载整个配置到内存，数据变化后不会更新
3. **加载冗余数据**：包括 Subscriptions 的节点池（SubProxies）等运行时不需要的数据

## 目标

设计一个专门的规则匹配器 `RuleMatcher`，满足：

1. **职责单一**：只包含规则匹配相关的数据和逻辑
2. **按需加载**：只加载构建对象关系需要的数据
3. **动态重建**：当底层数据变化时，自动重建对象关系
4. **实时匹配**：每次匹配使用最新的配置数据

## 设计

### 数据结构

```go
// db/matcher.go

// RuleMatcher 是专门的规则匹配器，包含规则匹配所需的所有对象关系
type RuleMatcher struct {
    // 原始数据（从数据库加载）
    rules       []string
    proxies     []*config.Proxy
    groups      []*config.ProxyGroup
    subscriptions []*config.Subscription  // 仅元数据，不含节点池
    mappings    []*config.Mapping
    resolvers   []*config.Resolver
    meshNodeID  string  // 从 Mesh 配置提取
    
    // 构建的对象关系
    proxyNames        map[string]*config.Proxy
    groupNames        map[string]*config.ProxyGroup
    subscriptionNames map[string]*config.Subscription
    matchers          []config.Matcher
    matcherTimeRanges []config.TimeRange
    matcherRules      []string
}
```

### 构建流程

```go
// BuildRuleMatcher 从数据库构建 RuleMatcher
func BuildRuleMatcher() (*RuleMatcher, error) {
    m := &RuleMatcher{}
    
    // 1. 从数据库加载原始数据
    err := db.View(func(tx *bolt.Tx) error {
        // 加载规则匹配需要的数据
        m.rules = loadRules(tx)
        m.proxies = loadProxies(tx)
        m.groups = loadProxyGroups(tx)
        m.subscriptions = loadSubscriptions(tx)  // 仅元数据
        m.mappings = loadMappings(tx)
        m.resolvers = loadResolvers(tx)
        
        // 加载 Mesh 配置的 node-id（用于 resolver 的 src-relative）
        if meshConf := loadMeshConfig(tx); meshConf != nil {
            m.meshNodeID = meshConf.NodeID
        }
        
        return nil
    })
    if err != nil {
        return nil, err
    }
    
    // 2. 构建对象关系（类似 RuleConfiguration.Init()）
    if err := m.buildRelationships(); err != nil {
        return nil, err
    }
    
    return m, nil
}

// buildRelationships 构建对象关系
func (m *RuleMatcher) buildRelationships() error {
    // 1. 构建查找表
    m.proxyNames = make(map[string]*config.Proxy)
    m.groupNames = make(map[string]*config.ProxyGroup)
    m.subscriptionNames = make(map[string]*config.Subscription)
    
    // 注册特殊代理
    m.proxyNames[config.ProxyDIRECT] = config.SingletonProxy(config.ProxyDIRECT)
    m.proxyNames[config.ProxyREJECT] = config.SingletonProxy(config.ProxyREJECT)
    
    // 注册代理
    for _, p := range m.proxies {
        if p.IsEnabled() {
            m.proxyNames[p.Name] = p
        }
    }
    
    // 注册订阅
    for _, s := range m.subscriptions {
        if s.IsEnabled() {
            m.subscriptionNames[s.Name] = s
        }
    }
    
    // 注册代理组
    for _, g := range m.groups {
        if g.IsEnabled() {
            m.groupNames[g.Name] = g
        }
    }
    
    // 2. 注入回调到代理组
    for _, g := range m.groups {
        g.resolveProxy = func(name string) *config.Proxy {
            return m.proxyNames[name]
        }
        g.resolveGroup = func(name string) *config.ProxyGroup {
            return m.groupNames[name]
        }
        g.resolveSubscription = func(name string) *config.Subscription {
            return m.subscriptionNames[name]
        }
    }
    
    // 3. 构建 Matchers
    m.matchers = make([]config.Matcher, 0, len(m.rules))
    m.matcherTimeRanges = make([]config.TimeRange, 0, len(m.rules))
    m.matcherRules = make([]string, 0, len(m.rules))
    
    for _, rule := range m.rules {
        // 解析规则字符串，构建 Matcher
        // ... (复用 config 包的解析逻辑)
    }
    
    return nil
}
```

### 匹配接口

```go
// ResolveMatch 执行规则匹配
func (m *RuleMatcher) ResolveMatch(req *config.AddrRequest, mappingName string) (
    *config.AddrRequest, *config.Proxy, *config.MatchResult, error) {
    
    // 1. 应用 Resolvers 重写地址
    req = m.applyResolvers(req)
    
    // 2. 查找 mapping
    var mapping *config.Mapping
    if mappingName != "" {
        mapping = m.findMapping(mappingName)
    }
    
    // 3. 遍历规则匹配
    return m.matchRules(req, mapping)
}
```

### 全局缓存与动态重建

```go
// 全局 RuleMatcher 实例
var (
    globalMatcher *RuleMatcher
    matcherMu     sync.RWMutex
    matcherBuilt  bool
    
    // 重建控制
    rebuildCh = make(chan string, 1)  // 触发通道，buffer=1
)

// init 启动重建循环（单 goroutine 串行处理）
func init() {
    go rebuildLoop()
}

// rebuildLoop 串行处理重建请求
func rebuildLoop() {
    for triggerBucket := range rebuildCh {
        // Debounce: 等待静默期
        time.Sleep(500 * time.Millisecond)
        
        // 排空通道中的待处理触发
        for len(rebuildCh) > 0 {
            <-rebuildCh
        }
        
        // 执行重建（串行，不会并发）
        doRebuildMatcher(triggerBucket)
    }
}

// GetMatcher 获取当前的 RuleMatcher（如果未构建则构建）
func GetMatcher() (*RuleMatcher, error) {
    matcherMu.RLock()
    m := globalMatcher
    matcherMu.RUnlock()
    
    if m != nil {
        return m, nil
    }
    
    // 首次构建
    matcherMu.Lock()
    defer matcherMu.Unlock()
    
    if globalMatcher == nil {
        m, err := BuildRuleMatcher()
        if err != nil {
            return nil, err
        }
        globalMatcher = m
        // 注册 watcher，数据变化时重建
        registerWatcher()
    }
    
    return globalMatcher, nil
}

// registerWatcher 注册数据库变更监听器
func registerWatcher() {
    // 监听相关 bucket 的变更
    db.Subscribe(db.BucketRules, rebuildMatcher)
    db.Subscribe(db.BucketProxies, rebuildMatcher)
    db.Subscribe(db.BucketProxyGroups, rebuildMatcher)
    db.Subscribe(db.BucketSubscriptions, rebuildMatcher)
    db.Subscribe(db.BucketSubscriptionNodes, rebuildMatcher)  // 节点池变更
    db.Subscribe(db.BucketMappings, rebuildMatcher)
    db.Subscribe(db.BucketResolvers, rebuildMatcher)
    db.Subscribe(db.BucketConfig, rebuildMatcher)  // Mesh 配置变化
}

// rebuildMatcher 由 watcher 调用，触发重建（非阻塞）
func rebuildMatcher(event db.ChangeEvent) {
    bucket := string(event.Bucket)
    
    // 检查是否需要重建
    if !shouldRebuild(bucket) {
        return
    }
    
    // 非阻塞发送触发信号
    select {
    case rebuildCh <- bucket:
        util.LogModuleInfo("db", "triggered rebuild for %s", bucket)
    default:
        // 已有重建待处理，跳过
        util.LogModuleInfo("db", "rebuild already pending, skipping %s", bucket)
    }
}

// doRebuildMatcher 执行重建（旧配置继续服务）
func doRebuildMatcher(triggerBucket string) {
    util.LogModuleInfo("db", "rebuilding RuleMatcher due to %s change", triggerBucket)
    
    // 1. 构建新 matcher（不持锁，旧 matcher 继续服务）
    m, err := BuildRuleMatcher()
    if err != nil {
        util.LogModuleError("db", "rebuild RuleMatcher failed: %v", err)
        return
    }
    
    // 2. 原子替换（短暂持锁）
    matcherMu.Lock()
    globalMatcher = m
    matcherMu.Unlock()
    
    util.LogModuleInfo("db", "RuleMatcher rebuilt successfully")
}
```

### 并发安全与同步机制

**问题场景**：
- 500ms 内多次数据变更 → 可能启动多个重建 goroutine
- 重建过程中有匹配请求 → 需要保证旧配置继续服务

**解决方案**：

1. **Channel 串行化**：
   - 使用 `rebuildCh` (buffer=1) 作为触发通道
   - 单 goroutine `rebuildLoop` 串行处理重建请求
   - 非阻塞 `select` 发送，避免阻塞 watcher

2. **旧配置继续服务**：
   - 重建时不持锁，读操作使用旧 matcher
   - 构建完成后短暂持写锁替换指针

3. **Debounce 合并**：
   - Sleep 500ms 等待静默期
   - 排空通道中待处理的触发，合并为一次重建

**时序图**：
```
T0: 变更 A → rebuildCh <- A
T1: rebuildLoop 收到 A → sleep 500ms
T2: 变更 B → rebuildCh <- B (非阻塞)
T3: 变更 C → rebuildCh 已满，跳过
T4: sleep 结束 → 排空通道 → 执行重建
T5: 重建完成 → globalMatcher 原子替换
```

### 便捷接口

```go
// ResolveMatch 从数据库实时匹配（使用全局缓存的 RuleMatcher）
func ResolveMatch(req *config.AddrRequest, mappingName string) (
    *config.AddrRequest, *config.Proxy, *config.MatchResult, error) {
    
    m, err := GetMatcher()
    if err != nil {
        return nil, nil, nil, err
    }
    
    return m.ResolveMatch(req, mappingName)
}
```

## 实现计划

### Phase 1: 核心实现
1. 创建 `db/matcher.go`，实现 `RuleMatcher` 结构体
2. 实现 `BuildRuleMatcher()` 从数据库加载数据并构建关系
3. 实现 `ResolveMatch()` 匹配接口
4. 实现全局缓存和 watcher 机制

### Phase 2: 集成
1. 更新所有服务器启动函数，移除 `ruleConf` 参数
2. 更新所有匹配调用，使用 `db.ResolveMatch()`
3. 移除 `getRuleConf()` 或简化为只加载全局设置

### Phase 3: 清理
1. 移除旧的 `RuleConfiguration` 相关代码（如果不再使用）
2. 更新测试用例
3. 文档更新

## 优势

1. **职责清晰**：`RuleMatcher` 专门用于规则匹配，不包含无关配置
2. **性能优化**：只加载匹配需要的数据，不加载冗余数据
3. **实时更新**：通过 watcher 机制，数据变化后自动重建对象关系
4. **向后兼容**：保留 `config.RuleConfiguration` 用于其他场景（如配置导入导出）

## 风险

1. **watcher 性能**：频繁的数据变更可能导致频繁重建
   - 缓解：使用 debounce 机制，合并短时间内的多次变更
2. **并发安全**：重建过程中可能有匹配请求
   - 缓解：使用读写锁，重建时替换指针，不影响正在进行的匹配
3. **数据一致性**：重建时读取的数据可能不一致（部分更新）
   - 缓解：在单个数据库事务中读取所有数据
