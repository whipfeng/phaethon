# Mesh 全网拓扑设计方案

## 背景

当前 Mesh 拓扑图只显示本地节点的直接邻居（一跳范围）。用户希望看到全网视图，了解完整的网络拓扑结构。

## 目标

- 每个节点能够展示完整的 Mesh 网络拓扑
- 显示所有节点及其连接关系
- 支持故障排查（查看路由路径）
- 复用现有 gossip 机制，不引入新的消息类型

## 设计

### 方案：扩展 Gossip 携带拓扑边

在现有 `GossipInfo` 中新增 `TopologyEdges` 字段，每个节点通告自己的直连边，通过 gossip 聚合机制自动传播到全网。

### 数据结构

```go
// GossipTopologyEdge 表示一条拓扑边
type GossipTopologyEdge struct {
    NodeID   string `json:"nodeId"`   // 边的起点
    Neighbor string `json:"neighbor"` // 边的终点
    Hop      int    `json:"hop"`      // 0=自己的直连观察, >1=间接学习
}

// GossipInfo 新增字段
type GossipInfo struct {
    NodeID          string                  `json:"nodeId"`
    Subnet          string                  `json:"subnet"`
    DomainSuffixes  []GossipDomainSuffix    `json:"domainSuffixes,omitempty"`
    Routes          []GossipRoute           `json:"routes,omitempty"`
    ClaimedSubnets  []GossipClaimedSubnet   `json:"claimedSubnets,omitempty"`
    TopologyEdges   []GossipTopologyEdge    `json:"topologyEdges,omitempty"` // 新增
}
```

### 传播机制

复用现有 gossip 聚合逻辑（类似 ClaimedSubnets）：

1. 每个节点观察自己的直连 peer，生成自己的边（hop=0）
2. `broadcastGossip()` 聚合所有已知边，按 split-horizon 过滤后发给每个 peer
3. 接收方存储到拓扑表，hop+1
4. 下一轮 gossip 时转发出去

**示例**（A — B — C 链式拓扑）：

第 1 轮报文：
```
A → B: { topologyEdges: [] }                    ← (A,B)涉及B，split-horizon过滤
B → A: { topologyEdges: [{B, C, hop=0}] }       ← (A,B)涉及A，过滤
B → C: { topologyEdges: [{A, B, hop=0}] }       ← (B,C)涉及C，过滤
C → B: { topologyEdges: [] }                    ← (B,C)涉及B，过滤
```

第 2 轮报文：
```
A → B: { topologyEdges: [{B, C, hop=1}] }       ← 转发从B学来的
B → A: { topologyEdges: [{B, C, hop=0}] }
B → C: { topologyEdges: [{A, B, hop=0}] }
C → B: { topologyEdges: [{A, B, hop=1}] }       ← 转发从B学来的
```

第 2 轮后所有节点收敛：
```
A: [(A,B), (B,C)]  ✓
B: [(A,B), (B,C)]  ✓
C: [(A,B), (B,C)]  ✓
```

**收敛时间** = 网络直径 × gossip 周期（15s）。链式 4 节点约 30-45s 收敛。

### 拓扑表存储

```go
// PeerInfo 新增字段
type PeerInfo struct {
    // ... 现有字段
    TopologyEdges []PeerTopologyEdgeEntry  // 从该 peer 学到的拓扑边
}

type PeerTopologyEdgeEntry struct {
    NodeID   string
    Neighbor string
    Hop      int
}

// Topology 新增全局拓扑边表
type Topology struct {
    mu           sync.RWMutex
    peers        []*PeerInfo
    topologyEdges map[string]map[string]int  // "nodeA|nodeB" -> min hop
}
```

### broadcastGossip 聚合逻辑

```go
// 聚合全局拓扑边表
type globalEdgeEntry struct {
    hop     int
    nextHop *PeerInfo  // nil = 自己的直连观察
    nodeID  string
    neighbor string
}
bestEdges := make(map[string]globalEdgeEntry)  // key: "nodeA|nodeB" (排序后)

// 自己的直连边 (hop=0)
for _, peer := range allPeers {
    key := edgeKey(m.nodeID, peer.NodeID())
    bestEdges[key] = globalEdgeEntry{0, nil, m.nodeID, peer.NodeID()}
}

// 从 peer 学到的边
for _, peer := range allPeers {
    for _, e := range peer.TopologyEdges {
        key := edgeKey(e.NodeID, e.Neighbor)
        if existing, ok := bestEdges[key]; !ok || e.Hop < existing.hop {
            bestEdges[key] = globalEdgeEntry{e.Hop, peer, e.NodeID, e.Neighbor}
        }
    }
}

// 发给每个 peer 时 split-horizon 过滤
for _, peer := range allPeers {
    var edges []GossipTopologyEdge
    for _, e := range bestEdges {
        if e.nextHop == peer { continue }  // split horizon
        // 也过滤涉及该 peer 的边
        if e.nodeID == peer.NodeID() || e.neighbor == peer.NodeID() { continue }
        edges = append(edges, GossipTopologyEdge{
            NodeID: e.nodeID, Neighbor: e.neighbor, Hop: e.hop,
        })
    }
    // ... 构建 GossipInfo 发送
}
```

### Admin API

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
    From string `json:"from"`
    To   string `json:"to"`
}
```

### 前端渲染

拓扑图渲染逻辑：
1. 从 `/api/mesh/topology` 获取全网数据
2. 使用 Canvas 绘制节点和边
3. 本地节点高亮显示

## 实现步骤

1. **协议层**：定义 `GossipTopologyEdge`，扩展 `GossipInfo`
2. **拓扑表**：`PeerInfo` 新增 `TopologyEdges`，`Topology` 新增全局边表
3. **聚合逻辑**：`broadcastGossip()` 中聚合拓扑边，split-horizon 过滤
4. **接收处理**：`UpdateGossip()` 存储拓扑边
5. **API**：新增 `/api/mesh/topology` 端点
6. **前端**：修改拓扑图渲染逻辑

## 状态

- [x] 设计完成
- [ ] 实现
