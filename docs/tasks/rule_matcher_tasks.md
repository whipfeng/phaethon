# RuleMatcher 实现任务

**创建时间**: 2026-10-23  
**关联设计**: docs/plans/rule_matcher_design.md

## Phase 1: 核心实现

### Task 1.1: 实现 RuleMatcher 结构体和构建逻辑
**文件**: `db/matcher.go`
**描述**: 
- 创建 `RuleMatcher` 结构体
- 实现 `BuildRuleMatcher()` 从数据库加载数据
- 实现 `buildRelationships()` 构建对象关系
- 实现 `ResolveMatch()` 匹配接口

**验收标准**:
- [ ] 能从数据库加载 rules, proxies, proxygroups, subscriptions, mappings, resolvers, mesh node-id
- [ ] 能正确构建 proxyNames, groupNames, subscriptionNames 查找表
- [ ] 能正确注入回调到 ProxyGroup
- [ ] 能正确构建 Matchers
- [ ] 编译通过

**预估时间**: 2-3 小时

---

### Task 1.2: 实现全局缓存和 watcher 机制
**文件**: `db/matcher.go`
**描述**:
- 实现全局 `globalMatcher` 缓存
- 实现 `GetMatcher()` 获取当前 matcher
- 实现 `registerWatcher()` 注册数据库变更监听
- 实现 `rebuildMatcher()` 重建逻辑
- 添加 debounce 机制避免频繁重建
- **实现并发重建控制**：
  - 使用 `rebuildCh` (buffer=1) 串行化重建请求
  - 单 goroutine `rebuildLoop` 处理重建
  - 旧配置继续服务，重建完成后原子替换

**验收标准**:
- [ ] 首次调用 `GetMatcher()` 时构建 matcher
- [ ] 注册 watcher 监听相关 bucket 变更（包括 BucketSubscriptionNodes）
- [ ] 数据变更时自动重建 matcher
- [ ] 重建过程不影响正在进行的匹配（旧配置继续服务）
- [ ] 并发触发时通过 channel 串行化，不会启动多个重建 goroutine
- [ ] 编译通过

**预估时间**: 2-3 小时

---

### Task 1.3: 实现便捷接口
**文件**: `db/matcher.go`
**描述**:
- 实现 `db.ResolveMatch()` 便捷函数
- 内部调用 `GetMatcher()` 获取 matcher 并执行匹配

**验收标准**:
- [ ] `db.ResolveMatch()` 能正确执行匹配
- [ ] 返回 4 个值：(req, proxy, matchResult, err)
- [ ] 编译通过

**预估时间**: 30 分钟

---

## Phase 2: 集成

### Task 2.1: 更新 BaseServer
**文件**: `server/base.go`
**描述**:
- 移除 `RuleConf *config.RuleConfiguration` 字段
- 保留 `Mapping *config.Mapping` 字段

**验收标准**:
- [ ] BaseServer 不再包含 RuleConf
- [ ] 编译通过（可能有其他文件报错，后续修复）

**预估时间**: 10 分钟

---

### Task 2.2: 更新 socks5 服务器
**文件**: `server/socks5.go`
**描述**:
- 更新 `StartSocks5(ruleConf, mapping)` → `StartSocks5(mapping)`
- 替换所有 `r.ruleConf.ResolveMatch()` → `db.ResolveMatch()`
- 添加 `"phaethon/db"` 导入
- 处理新的 4 返回值签名

**验收标准**:
- [ ] StartSocks5 不再接受 ruleConf 参数
- [ ] 所有匹配调用使用 db.ResolveMatch()
- [ ] 编译通过

**预估时间**: 30 分钟

---

### Task 2.3: 更新 trojan 服务器
**文件**: `server/trojan.go`
**描述**:
- 更新 `StartTrojan(ruleConf, mapping)` → `StartTrojan(mapping)`
- 更新 `StartTrojanSNI(ruleConf, mappings, port)` → `StartTrojanSNI(mappings, port)`
- 替换所有匹配调用
- 添加 db 导入

**验收标准**:
- [ ] 所有 Start 函数不再接受 ruleConf 参数
- [ ] 所有匹配调用使用 db.ResolveMatch()
- [ ] 编译通过

**预估时间**: 30 分钟

---

### Task 2.4: 更新 htunnel 服务器
**文件**: `server/htunnel.go`
**描述**:
- 更新 `StartHTunnel(ruleConf, mapping)` → `StartHTunnel(mapping)`
- 替换所有匹配调用
- 添加 db 导入

**验收标准**:
- [ ] StartHTunnel 不再接受 ruleConf 参数
- [ ] 所有匹配调用使用 db.ResolveMatch()
- [ ] 编译通过

**预估时间**: 30 分钟

---

### Task 2.5: 更新 http/https 服务器
**文件**: `server/http.go`
**描述**:
- 更新 `StartHTTP(ruleConf, mapping)` → `StartHTTP(mapping)`
- 更新 `StartHTTPS(ruleConf, mapping)` → `StartHTTPS(mapping)`
- 替换所有匹配调用
- 添加 db 导入

**验收标准**:
- [ ] 所有 Start 函数不再接受 ruleConf 参数
- [ ] 所有匹配调用使用 db.ResolveMatch()
- [ ] 编译通过

**预估时间**: 20 分钟

---

### Task 2.6: 更新 direct 服务器
**文件**: `server/direct.go`
**描述**:
- 更新 `StartDirect(ruleConf, mapping)` → `StartDirect(mapping)`
- 替换所有匹配调用
- 添加 db 导入

**验收标准**:
- [ ] StartDirect 不再接受 ruleConf 参数
- [ ] 所有匹配调用使用 db.ResolveMatch()
- [ ] 编译通过

**预估时间**: 20 分钟

---

### Task 2.7: 更新 reverse 服务器
**文件**: `server/reverse.go`
**描述**:
- 更新 `StartReverse(ruleConf, mapping, handler)` → `StartReverse(mapping, handler)`
- 更新 `StartReverseMapping(ruleConf, mapping)` → `StartReverseMapping(mapping)`
- 更新 `handleReverseUDPChannel(ruleConf, mapping, ...)` → `handleReverseUDPChannel(mapping, ...)`
- 更新 `handleReverseGeneric(ruleConf, mapping, ...)` → `handleReverseGeneric(mapping, ...)`
- 替换所有匹配调用
- 添加 db 导入

**验收标准**:
- [ ] 所有函数不再接受 ruleConf 参数
- [ ] 所有匹配调用使用 db.ResolveMatch()
- [ ] 编译通过

**预估时间**: 1 小时

---

### Task 2.8: 更新 tun/engine.go
**文件**: `tun/engine.go`
**描述**:
- 更新 `NewEngine(ruleConf)` → `NewEngine()`
- 移除 `ruleConf` 字段
- 替换所有 `e.ruleConf.ResolveMatch()` → `db.ResolveMatch()`
- 添加 db 导入

**验收标准**:
- [ ] NewEngine 不再接受 ruleConf 参数
- [ ] Engine 结构体不再包含 ruleConf 字段
- [ ] 所有匹配调用使用 db.ResolveMatch()
- [ ] 编译通过

**预估时间**: 1 小时

---

### Task 2.9: 更新 main.go
**文件**: `main.go`
**描述**:
- 更新所有服务器启动调用，移除 `conf` 参数
- 更新 `startEngine(conf, ...)` → `startEngine(...)`
- 简化或移除 `getRuleConf()`（如果不再需要）
- 更新 `refreshSubscription(conf, ...)` 使用 `db.ResolveMatch()`

**验收标准**:
- [ ] 所有服务器启动不再传递 conf
- [ ] 编译通过
- [ ] 程序能正常启动

**预估时间**: 1 小时

---

## Phase 3: 清理和测试

### Task 3.1: 编译验证
**描述**:
- 执行 `go build` 确保编译通过
- 修复所有编译错误

**验收标准**:
- [ ] 编译通过，无错误

**预估时间**: 1-2 小时

---

### Task 3.2: 单元测试
**描述**:
- 为 `RuleMatcher` 编写单元测试
- 测试构建逻辑
- 测试匹配逻辑
- 测试 watcher 重建机制

**验收标准**:
- [ ] 所有测试通过
- [ ] 覆盖主要场景

**预估时间**: 2-3 小时

---

### Task 3.3: 集成测试
**描述**:
- 在 VM 环境部署测试
- 验证规则匹配功能正常
- 验证数据变更后 matcher 自动重建
- 验证性能无明显下降

**验收标准**:
- [ ] 所有功能正常
- [ ] 性能可接受

**预估时间**: 2-3 小时

---

### Task 3.4: 文档更新
**描述**:
- 更新 agents.md 中的架构说明
- 更新相关代码注释
- 记录 watcher 机制的使用方式

**验收标准**:
- [ ] 文档完整准确

**预估时间**: 1 小时

---

## 总计

- **Phase 1**: 4-6 小时
- **Phase 2**: 6-8 小时
- **Phase 3**: 6-9 小时
- **总计**: 16-23 小时

## 风险

1. **watcher 性能**：频繁重建可能影响性能
   - 缓解：使用 debounce（500ms）+ channel 串行化
2. **并发问题**：重建过程中的并发访问
   - 缓解：旧配置继续服务，重建完成后原子替换指针
3. **数据一致性**：部分更新导致的不一致
   - 缓解：在单个事务中读取所有数据
4. **回归风险**：大范围重构可能引入 bug
   - 缓解：充分测试，分阶段部署
5. **节点池变更触发**：订阅节点池变更需要触发重建
   - 缓解：监控 BucketSubscriptionNodes，确保节点池更新后重建

## 依赖

- 无外部依赖
- 需要现有的 db.Subscribe() 机制正常工作
