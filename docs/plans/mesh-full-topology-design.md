# Mesh 全网拓扑设计方案

## 背景

当前 Mesh 拓扑图只显示本地节点的直接邻居（一跳范围）。用户希望看到全网视图，了解完整的网络拓扑结构。

## 目标

- 每个节点能够展示完整的 Mesh 网络拓扑
- 显示所有节点及其连接关系
- 支持故障排查（查看路由路径）
- 保持协议简单，避免过度复杂

## 设计思路

### 方案：链路状态泛洪（Link-State Flooding）

类似 OSPF 的简化版本，每个节点广播自己的邻居列表，其他节点收集并泛洪，最终每个节点都有全网拓扑信息。

### 核心数据结构

```go
// 拓扑通告 - 每个节点定期广播
type TopologyAnnouncement struct {
    NodeID    string   `json:"nodeId"`    // 通告来源节点
    Neighbors []string `json:"neighbors"` // 该节点的直接邻居
    SeqNum    uint64   `json:"seqNum"`    // 序列号，用于去重
    Timestamp int64    `json:"timestamp"` // 时间戳，用于过期清理
}

// 全网拓扑表 - 每个节点维护
type TopologyTable struct {
    mu       sync.RWMutex
    entries  map[string]*TopologyEntry  // nodeId -> entry
}

type TopologyEntry struct {
    NodeID    string
    Neighbors []string
    SeqNum    uint64
    LastSeen  time.Time
}
```

### 协议扩展

#### 1. Gossip 消息类型扩展

当前 gossip 消息：
```go
type GossipMessage struct {
    NodeID     string
    VIP        string
    Subnet     string
    // ... 其他字段
}
```

扩展后：
```go
type GossipMessage struct {
    NodeID     string
    VIP        string
    Subnet     string
    // 新增：拓扑通告
    Topology   *TopologyAnnouncement `json:"topology,omitempty"`
}
```

#### 2. 拓扑泛洪逻辑

```go
func (m *MeshManager) handleTopologyAnnouncement(ann *TopologyAnnouncement, fromNode string) {
    // 1. 检查序列号，如果已收到更新的，忽略
    entry := m.topologyTable.Get(ann.NodeID)
    if entry != nil && entry.SeqNum >= ann.SeqNum {
        return
    }
    
    // 2. 更新本地拓扑表
    m.topologyTable.Update(ann)
    
    // 3. 泛洪给其他邻居（除了来源）
    m.broadcastTopology(ann, fromNode)
}

func (m *MeshManager) broadcastTopology(ann *TopologyAnnouncement, excludeNode string) {
    for _, peer := range m.peers {
        if peer.NodeID == excludeNode {
            continue
        }
        m.sendGossip(peer, &GossipMessage{Topology: ann})
    }
}
```

#### 3. 本地拓扑通告生成

```go
func (m *MeshManager) generateTopologyAnnouncement() *TopologyAnnouncement {
    m.mu.RLock()
    defer m.mu.RUnlock()
    
    neighbors := make([]string, 0, len(m.peers))
    for _, peer := range m.peers {
        neighbors = append(neighbors, peer.NodeID)
    }
    
    return &TopologyAnnouncement{
        NodeID:    m.localNodeID,
        Neighbors: neighbors,
        SeqNum:    m.topologySeqNum,
        Timestamp: time.Now().Unix(),
    }
}
```

### Admin API 扩展

新增 `/api/mesh/topology` 端点：

```go
type FullTopology struct {
    Nodes []TopologyNode `json:"nodes"`
    Edges []TopologyEdge `json:"edges"`
}

type TopologyNode struct {
    NodeID string `json:"nodeId"`
    VIP    string `json:"vip"`
    Subnet string `json:"subnet"`
}

type TopologyEdge struct {
    From   string `json:"from"`
    To     string `json:"to"`
    Direct bool   `json:"direct"` // 是否是直连（vs 通过路由表推断）
}
```

### 前端渲染

拓扑图渲染逻辑：
1. 从 `/api/mesh/topology` 获取全网数据
2. 使用 Canvas 或 SVG 绘制节点和边
3. 本地节点高亮显示
4. 直连边用实线，推断边用虚线

### 时序与清理

- 每个节点每 30 秒广播一次拓扑通告
- 序列号每次广播递增
- 拓扑表条目 90 秒未更新则删除（节点离线）

## 实现步骤

1. **协议层**
   - 定义 `TopologyAnnouncement` 结构
   - 扩展 `GossipMessage` 添加拓扑字段
   - 实现拓扑表 `TopologyTable`
   - 实现泛洪逻辑

2. **本地通告**
   - 定期生成并广播本地拓扑通告
   - 处理收到的拓扑通告

3. **API 层**
   - 新增 `/api/mesh/topology` 端点
   - 从拓扑表生成全网视图

4. **前端**
   - 修改拓扑图渲染逻辑
   - 支持显示全网节点和边

## 复杂度评估

| 模块 | 工作量 | 风险 |
|------|--------|------|
| 协议扩展 | 中 | 低 |
| 泛洪逻辑 | 中 | 中（需防止泛洪风暴） |
| 拓扑表维护 | 低 | 低 |
| API | 低 | 低 |
| 前端渲染 | 中 | 低 |

**总计**：约 2-3 天开发时间

## 待讨论

1. **泛洪频率**：30 秒是否合适？太频繁增加带宽，太慢收敛慢
2. **过期时间**：90 秒是否合适？需要考虑网络抖动
3. **大规模网络**：100+ 节点时，拓扑表可能很大，是否需要优化
4. **安全性**：是否需要签名防止伪造拓扑通告

## 替代方案

### 方案 B：中心化拓扑收集

指定一个节点作为拓扑收集器，所有节点向它报告邻居，由它计算全网拓扑。

优点：简单，无泛洪风暴
缺点：单点故障，需要选举机制

### 方案 C：按需查询

不维护全网拓扑，admin 页面请求时实时查询所有节点。

优点：数据实时
缺点：延迟高，需要多跳查询

**推荐方案 A（链路状态泛洪）**，去中心化，收敛快，适合中小规模网络。
