# Mesh 网络性能优化方案

**创建时间**: 2026-10-14  
**状态**: 待实施  
**优先级**: 高

## 问题背景

当前 mesh 网络在以下方面存在性能问题：
1. 路由查找效率低（线性扫描）
2. 路由重计算阻塞读操作（锁持有时间长）
3. P2P 队列无优先级区分（控制帧可能被数据包阻塞）
4. 队列满时才丢包（延迟较高）

## 优化目标

1. **路由查找**: O(R) → O(log R)，R 为路由数
2. **路由重计算**: 减少锁持有时间，异步化
3. **优先级队列**: 控制帧优先，避免饿死
4. **RED 风格丢包**: 提前通知 TCP 减速，降低延迟

## 优化方案

### 优化 1：路由查找改用 Trie

**现状**：
- `findRoute` 和 `findNextHops` 线性扫描所有路由
- 时间复杂度 O(R)，R 为路由数

**方案**：
- 使用 IP Trie 结构（类似现有的 `domainTrie`）
- 支持最长前缀匹配（LPM）
- 时间复杂度 O(log R)

**实现要点**：
- 新增 `routeTrie` 结构（IPv4 prefix tree）
- 每个节点存储路由条目（MeshRoute）
- 插入时按前缀长度分层
- 查找时从根到叶，匹配最长前缀

**影响范围**：
- `mesh/mesh.go`: `recomputeRoutes` 构建 trie
- `mesh/mesh.go`: `findRoute` 和 `findNextHops` 使用 trie 查找

### 优化 2：recomputeRoutes 异步化

**现状**：
- `recomputeRoutes` 持有 `m.mu.RLock()` 整个计算过程
- 包括：遍历 peer、构建拓扑图、Dijkstra、构建路由表
- 期间所有读路由操作被阻塞

**方案**：
- 在锁内只做数据快照（快速）
- 释放锁后进行计算（耗时操作）
- 计算完成后原子交换路由表（已有 `atomic.Store`）

**实现要点**：
- 锁内：复制 peers、advertise、domainSuffixes 等
- 锁外：构建拓扑图、运行 Dijkstra、构建路由表
- 原子交换：`m.routeTable.Store(&newRouteTable)`

**影响范围**：
- `mesh/mesh.go`: `recomputeRoutes` 重构

### 优化 3：优先级队列

**现状**：
- 每个 Peer 一个 `writeCh`（数据帧）和一个 `controlCh`（控制帧）
- 控制帧已优先，但数据帧内部无优先级
- 大文件传输可能饿死小数据包

**方案**：
- 数据帧分两级：`highPriorityCh`（小包）和 `bulkCh`（大包）
- 控制帧 > 高优先级 > 普通优先级
- `peerWriteLoop` 按优先级消费

**实现要点**：
- Peer 结构新增 `highPriorityCh` 和 `bulkCh`
- 根据包大小分级（< 500 bytes = high, >= 500 bytes = bulk）
- `peerWriteLoop` 严格按优先级 select

**影响范围**：
- `p2p/p2p.go`: Peer 结构、`enqueueWrite`、`peerWriteLoop`

### 优化 4：RED 风格早期丢包

**现状**：
- 队列满才丢包（Tail Drop）
- TCP 要等到队列满才减速
- 延迟较高

**方案**：
- 设置高水位线（如队列容量 80%）
- 达到高水位时，按概率丢包
- 丢包概率随队列长度线性增加
- 提前通知 TCP 减速

**实现要点**：
- 配置参数：`minThreshold`, `maxThreshold`, `maxProbability`
- 队列长度在 `[minThreshold, maxThreshold]` 时，丢包概率线性增加
- 超过 `maxThreshold` 时 100% 丢包
- 控制帧不受 RED 影响（保证 ACK 不丢）

**影响范围**：
- `p2p/p2p.go`: `enqueueWrite` 添加 RED 逻辑
- 配置：可配置的 RED 参数

## 实施计划

### 阶段 1：路由优化（优化 1 + 2）
1. 实现 IP Trie 结构
2. 修改 `recomputeRoutes` 构建 trie
3. 修改 `findRoute` 和 `findNextHops` 使用 trie
4. 重构 `recomputeRoutes` 异步化
5. 测试验证

### 阶段 2：队列优化（优化 3 + 4）
1. 实现优先级队列
2. 修改 `peerWriteLoop` 按优先级消费
3. 实现 RED 风格丢包
4. 测试验证

### 阶段 3：部署验证
1. 部署到 VM 环境
2. 验证功能正确性
3. 性能测试（路由查找、吞吐量、延迟）
4. 部署到所有环境

## 风险与缓解

**风险 1：Trie 实现复杂**
- 缓解：参考现有 `domainTrie` 实现
- 缓解：先实现简单版本，后续优化

**风险 2：异步化引入竞态**
- 缓解：使用 atomic 操作交换路由表
- 缓解：充分测试并发场景

**风险 3：优先级队列增加内存**
- 缓解：合理设置队列容量
- 缓解：监控内存使用

**风险 4：RED 参数调优**
- 缓解：提供默认值，可配置
- 缓解：根据实际流量调整

## 测试计划

**功能测试**：
- 路由查找正确性（最长前缀匹配）
- 路由重计算并发安全
- 优先级队列行为（控制帧优先）
- RED 丢包行为

**性能测试**：
- 路由查找延迟（对比优化前后）
- 路由重计算时间（锁持有时间）
- 吞吐量（大文件传输）
- 延迟（小包 vs 大包）

**压力测试**：
- 高并发路由查找
- 队列满载行为
- RED 触发频率

## 参考资料

- RED (Random Early Detection): https://en.wikipedia.org/wiki/Random_early_detection
- IP Trie: https://en.wikipedia.org/wiki/Trie#IP_routing
- TCP Congestion Control: https://en.wikipedia.org/wiki/TCP_congestion_control
