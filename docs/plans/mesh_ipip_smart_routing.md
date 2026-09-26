# Mesh IPIP 封装与智能选路设计

## 概述

本设计分三阶段优化 mesh 网络的路由机制：
1. **Phase 1**: IPIP 封装 - 源节点明确指定目标 nodeID，中间节点按外层目标转发
2. **Phase 2**: 链路质量监控 - 定期探测 peer 连接的 RTT 和丢包率
3. **Phase 3**: 智能选路 - 基于链路质量做选路决策

## 动机

### 当前问题
- 每一跳都要查 mesh 路由表，复杂度高
- 路由表不一致时可能路由到错误的 nodeID
- 选路策略受限（只按跳数），无法优化链路质量

### 目标
- 中间节点简化：不查 mesh 路由表，按外层目标转发
- 目标 nodeID 明确：不会路由错误
- 支持智能选路：基于链路质量优化

## 架构设计

### 统一路由结构

#### 设计原则

静态路由和动态路由合并为统一结构，通过 `source` 字段区分来源。路由匹配时，nodeID 选择遵循以下规则：
1. **静态优先**：静态配置的 nodeID 优先级高于动态学习的
2. **稳定选择**：同一目标 IP + 同一可用节点列表 → 总是选出同一个 nodeID
3. **故障切换**：只有当选中的 nodeID 掉线时，才重新选择

#### 数据结构

**统一路由表**：
```go
type RouteSource string

const (
    RouteSourceStatic  RouteSource = "static"
    RouteSourceDynamic RouteSource = "dynamic"
)

type MeshRoute struct {
    Prefix  *net.IPNet
    Entries []RouteEntry  // 统一条目列表
}

type RouteEntry struct {
    NodeID string
    Source RouteSource  // static | dynamic
}
```

**统一域名路由**：
```go
type DomainRoute struct {
    Suffix  string       // 如 ".phn"
    Entries []RouteEntry // 统一条目列表
}
```

#### NodeID 选择算法

**目标**：稳定、可预测、故障自动切换

**算法流程**：
```
输入：targetIP, routeEntries (含 source 标记)
输出：selectedNodeID

1. 排序：静态条目排前面，动态条目排后面
   sortedEntries = sort(entries, by=source: static first)

2. 过滤：移除离线节点
   availableEntries = filter(sortedEntries, node.isOnline())

3. 粘性检查：如果上次选中的节点仍在 availableEntries 中，保持不变
   if lastSelectedNodeID != "" && contains(availableEntries, lastSelectedNodeID):
       return lastSelectedNodeID

4. 哈希稳定选择：
   hash = hash(targetIP) % len(availableEntries)
   selectedNodeID = availableEntries[hash].NodeID

5. 记录选择，下次优先复用
   lastSelectedNodeID = selectedNodeID
   return selectedNodeID
```

**稳定性保证**：
- 同一 targetIP + 同一可用节点列表 → 总是选出同一个 nodeID
- 静态节点优先级高，但一旦选中动态节点也会保持稳定
- 只有选中节点掉线时才重新选择

**示例**：
```
目标：10.21.20.65
匹配路由条目：
  - {nodeID: QG, source: static}
  - {nodeID: VM, source: static}
  - {nodeID: GG, source: dynamic}
排序后：[QG(static), VM(static), GG(dynamic)]
假设全部在线，哈希选择：hash(10.21.20.65) % 3 = 1 → 选择 VM
下次：还是 VM（粘性）
如果 VM 掉线：availableEntries=[QG(static), GG(dynamic)]
重新哈希：hash(10.21.20.65) % 2 = 0 → 选择 QG
```

#### 配置格式

```yaml
mesh:
  static-routes:
    - prefix: 10.21.20.0/24
      node-ids: [QG, VM]  # 可以配置多个，优先级高于动态
    - prefix: 8.8.8.0/24
      node-ids: [QG]
  
  static-domain-routes:
    - suffix: ".internal"
      node-ids: [GG]  # 可以配置多个
```

### Phase 1: IPIP 封装

#### 数据平面变化

**当前流程**：
```
源节点 A 发包到目标 D（mesh IP）
  ↓
每一跳：查路由表 → 选下一跳 → P2P 转发
  ↓
A → B → C → D（每跳都查路由）
```

**改成 IPIP 后**：
```
源节点 A 发包到目标 D（mesh IP）
  ↓
A 查拓扑，找到 D 的 nodeID
  ↓
A 封装 IPIP：外层 src=A.EIP, dst=D.EIP, 内层=原始包
  ↓
A 查路由到 D.EIP，选下一跳 peer，发送封装包
  ↓
中间节点 B 收到：
  - 看外层目标 D.EIP
  - 查路由到 D.EIP，选下一跳 peer，转发
  ↓
中间节点 C 收到：
  - 看外层目标 D.EIP
  - 查路由到 D.EIP，选下一跳 peer，转发
  ↓
目标节点 D 收到：
  - 看外层目标 = 自己的 EIP
  - 解封装，得到内层原始包
  - 处理内层包
```

#### 关键设计

1. **EIP（Egress IP）**：每个节点有一个 EIP，用于 IPIP 封装
   - EIP = subnet + 4（如 100.0.0.4 for 100.0.0.0/16）
   - 已实现：`mesh/ipip.go` 中的 `CalculateEIP()`

2. **IPIP 封装格式**：
   ```
   +-------------------+-------------------+
   | 外层 IP Header    | 内层原始包        |
   | src=A.EIP         |                   |
   | dst=D.EIP         |                   |
   | protocol=4 (IPIP) |                   |
   +-------------------+-------------------+
   ```

3. **中间节点转发逻辑**：
   - 检查 protocol == 4（IPIP）
   - 提取外层目标地址（D.EIP）
   - 查路由表找到 D.EIP 对应的 peer
   - 转发封装包

4. **目标节点解封装**：
   - 检查外层目标 == 自己的 EIP
   - 剥离外层 IP header
   - 处理内层原始包

#### 兼容性

- **向后兼容**：非 IPIP 包按现有逻辑处理
- **混合存在**：可以部分流量走 IPIP，部分走原有逻辑
- **静态路由**：已有的 IPIP 静态路由逻辑可以复用

### Phase 2: 链路质量监控

#### 探测机制

**谁探测谁**：每个节点定期探测它直接连接的 peer

**探测什么**：在 P2P 通道里发探测消息（不是 ICMP ping）

**探测流程**：
```
每 10 秒：
  A → B 发 ProbeMsg{Seq: 1, Timestamp: T1}
  B → A 回 ProbeReply{Seq: 1, Timestamp: T1}
  A 收到，计算 RTT = T2 - T1
  
  统计丢包：发了 100 个，收到 95 个，丢包率 = 5%
```

#### 数据结构

```go
type PeerQuality struct {
    rtt        time.Duration  // 平均 RTT（滑动窗口）
    packetLoss float64        // 丢包率（0.0 - 1.0）
    lastUpdate time.Time
}
```

#### 参数配置

```yaml
mesh:
  probe-interval: 10s      # 探测间隔
  probe-timeout: 5s        # 探测超时
  rtt-window-size: 10      # RTT 滑动窗口大小
```

### Phase 3: 智能选路

#### 选路策略

**跳数优先，质量优化**（主流做法）：

```go
// 1. 第一优先级：跳数最少
minHop := route.Peers[0].Hop
candidates := filter(peers, hop == minHop)

// 2. 第二优先级：链路质量最优（同等跳数下）
if len(candidates) > 1 {
    sort(candidates, by=rtt)  // RTT 最小优先
}

// 3. 选择：质量最好的（或 top-N 随机）
selected := candidates[0]  // 或 topN[rand]
```

**为什么跳数优先？**
- 跳数少 = 路径短 = 延迟低（经验法则）
- 主流路由协议（BGP、OSPF、RIP）都是跳数/代价优先
- 简单稳定，避免路由环路

**为什么不加权评分？**
- 量纲不同（跳数无单位，RTT 是毫秒），需要归一化
- 权重难调，没有标准答案
- 质量波动导致路由不稳定
- 复杂度高，收益有限

#### 配置

```yaml
mesh:
  routing-strategy: quality  # quality | hop-count | random
  quality-weight: 0.7        # 质量权重
  hop-weight: 0.3            # 跳数权重
```

## 实施计划

### Phase 1: IPIP 封装（2-3 天）

**任务**：
1. 修改 `HandleOutboundPacket`，所有 mesh 流量封装成 IPIP
2. 修复静态路由查路逻辑：用 subnet 或 EIP 查路由（而不是 VIP）
3. 修改 `HandleMeshFrame`，识别 IPIP 包并转发
4. 目标节点解封装处理
5. 测试验证

**验证环境**：QG + JF

### Phase 2: 链路质量监控（1-2 天）

**任务**：
1. 实现 `PeerQuality` 数据结构
2. 实现探测包发送和接收
3. 实现滑动窗口统计
4. 测试验证

**验证环境**：QG + JF

### Phase 3: 智能选路（1-2 天）

**任务**：
1. 修改选路逻辑，使用质量数据
2. 实现选路策略配置
3. 测试验证

**验证环境**：QG + JF → 所有环境

## 风险与缓解

### 风险 1: IPIP 封装增加开销
- **影响**：每个包增加 20 字节（IP header）
- **缓解**：开销很小（<2%），可接受

### 风险 2: 中间节点需要识别 IPIP 包
- **影响**：需要修改所有节点的代码
- **缓解**：向后兼容，旧节点按原有逻辑处理

### 风险 3: 探测包增加网络开销
- **影响**：每 10 秒每个 peer 一个探测包
- **缓解**：开销很小（<1%），可接受

## 成功标准

1. **Phase 1**：IPIP 封装包能正确转发和解封装
2. **Phase 2**：能准确测量 RTT 和丢包率
3. **Phase 3**：选路能基于质量优化，延迟降低

## 参考资料

- 现有 IPIP 实现：`mesh/ipip.go`
- 现有路由逻辑：`mesh/mesh.go:HandleOutboundPacket`
- 现有 peer 管理：`p2p/p2p.go`
