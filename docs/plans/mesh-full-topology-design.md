# Mesh Gossip 协议优化与全网拓扑设计方案

## 背景

### 原始需求
当前 Mesh 拓扑图只显示本地节点的直接邻居（一跳范围）。用户希望看到全网视图，了解完整的网络拓扑结构。

### 发现的问题
在实现全网拓扑功能时，发现了现有 gossip 协议的严重问题：

1. **过时路由游荡问题**：当节点停止通告某个路由/subnet后，其他节点存储的旧信息不会被清除，继续在网络中传播，每次 hop+1，导致 hop count 累积到 400+
2. **字段冗余问题**：GossipInfo 中存在多个冗余字段，增加了协议复杂性和报文大小

## 目标

- 解决过时路由游荡问题
- 消除协议冗余，简化设计
- 每个节点能够展示完整的 Mesh 网络拓扑
- 显示所有节点及其连接关系
- 支持故障排查（查看路由路径）
- 复用现有 gossip 机制，不引入新的消息类型

## 问题分析

### 过时路由游荡问题

**场景**：节点A原来有subnet X，后来改成Y

1. A通告X给B（hop=0）
2. B存储并传播给C（hop=1）
3. C存储并传播给B（hop=2，但split horizon阻止回传给B）
4. A改成Y，停止通告X
5. B更新A.PeerInfo，不再包含X
6. **但是** B的bestClaims还会从C.PeerInfo中包含X（hop=2）
7. B继续传播X给其他节点，hop count不断累积

**根本原因**：当前协议没有路由老化机制，一旦学到某个路由就会一直保留并传播。

### 字段冗余分析

| 字段 | 冗余？ | 原因 |
|------|--------|------|
| NodeID | ✅ 冗余 | 发送方身份已通过PeerSender识别 |
| Subnet | ✅ 冗余 | 可从ClaimedSubnets中hop=0的条目推导 |
| Routes.Hop | ✅ 冗余 | 路由所有者的距离已在ClaimedSubnets中维护 |
| DomainSuffixes.Hop | ✅ 冗余 | 域名后缀所有者的距离已在ClaimedSubnets中维护 |
| DomainSuffixes.Subnet | ✅ 冗余 | 域名后缀所有者的subnet已在ClaimedSubnets中维护 |
| TopologyEdges | ✅ 冗余 | 可通过在ClaimedSubnets中添加Neighbors字段表达 |

## 设计方案

### 核心思路

1. **简化GossipInfo**：移除冗余字段
2. **引用式设计**：Routes和DomainSuffixes引用nodeID，hop count从ClaimedSubnets推导
3. **合并拓扑信息**：在ClaimedSubnets中添加Neighbors字段，替代TopologyEdges

### 新的数据结构

```go
// GossipInfo 简化后的结构
type GossipInfo struct {
    // 移除 NodeID 字段（通过PeerSender识别）
    // 移除 Subnet 字段（从ClaimedSubnets推导）
    
    DomainSuffixes []GossipDomainSuffix    `json:"domainSuffixes,omitempty"`
    Routes         []GossipRoute           `json:"routes,omitempty"`
    ClaimedSubnets []GossipClaimedSubnet   `json:"claimedSubnets,omitempty"`
    // 移除 TopologyEdges 字段（通过ClaimedSubnets.Neighbors表达）
}

// GossipRoute 简化：只引用所有者，不维护hop count
type GossipRoute struct {
    Prefix string `json:"prefix"`
    NodeID string `json:"nodeId"`  // 路由所有者
}

// GossipDomainSuffix 简化：只引用所有者，不维护hop count和subnet
type GossipDomainSuffix struct {
    Suffix string `json:"suffix"`
    NodeID string `json:"nodeId"`  // 域名后缀所有者
}

// GossipClaimedSubnet 增强：添加Neighbors字段表达拓扑
type GossipClaimedSubnet struct {
    Subnet    string   `json:"subnet"`
    NodeID    string   `json:"nodeId"`
    Hop       int      `json:"hop"`
    Neighbors []string `json:"neighbors,omitempty"`  // 该节点直连的节点列表
}
```

### 报文示例

```json
{
  "claimedSubnets": [
    {
      "subnet": "100.179.0.0/16",
      "nodeId": "gg",
      "hop": 0,
      "neighbors": ["ms9", "ms10"]
    },
    {
      "subnet": "100.189.0.0/16",
      "nodeId": "ms9",
      "hop": 1,
      "neighbors": ["gg", "x"]
    },
    {
      "subnet": "100.96.0.0/16",
      "nodeId": "ms10",
      "hop": 1,
      "neighbors": ["gg"]
    }
  ],
  "routes": [
    {"prefix": "192.168.1.0/24", "nodeId": "gg"},
    {"prefix": "10.10.0.0/16", "nodeId": "ms9"}
  ],
  "domainSuffixes": [
    {"suffix": ".internal", "nodeId": "gg"},
    {"suffix": ".ms9.internal", "nodeId": "ms9"}
  ]
}
```

### 处理逻辑

#### 发送方（broadcastGossip）

1. **构建ClaimedSubnets**：
   - 自己的条目：`{subnet: m.subnetStr, nodeID: m.nodeID, hop: 0, neighbors: [所有直连peer的nodeID]}`
   - 从peer学到的条目：保留原hop count和neighbors，hop+1

2. **构建Routes**：
   - 自己的路由：`{prefix: r, nodeID: m.nodeID}`
   - 从peer学到的路由：`{prefix: r.Prefix, nodeID: r.NodeID}`（保留原nodeID）

3. **构建DomainSuffixes**：
   - 自己的后缀：`{suffix: s, nodeID: m.nodeID}`
   - 从peer学到的后缀：`{suffix: entry.Suffix, nodeID: entry.NodeID}`（保留原nodeID）

4. **Split horizon过滤**：
   - 对每个peer，过滤掉nextHop==该peer的条目
   - 使用nodeID比较而非指针比较，更可靠

#### 接收方（UpdateGossip）

1. **解析ClaimedSubnets**：
   - 建立nodeID到hop count的映射
   - 建立nodeID到neighbors的映射
   - 建立nodeID到subnet的映射

2. **解析Routes**：
   - 查找route.NodeID的hop count
   - 路由的实际hop = route.NodeID的hop count

3. **解析DomainSuffixes**：
   - 查找suffix.NodeID的hop count和subnet
   - 后缀的实际hop = suffix.NodeID的hop count
   - 后缀的subnet = suffix.NodeID的subnet

4. **构建全网拓扑**：
   - 从所有ClaimedSubnets的Neighbors字段推导边
   - 例如：A.neighbors=[B,C] → 边(A,B), (A,C)

### 解决过时路由游荡问题

**原理**：当节点A停止通告subnet X时：
1. A的ClaimedSubnets不再包含X
2. B收到A的新gossip，更新A.PeerInfo.ClaimedSubnets，不再包含X
3. B构建bestClaims时，X不再从A.PeerInfo中来
4. 如果其他peer的ClaimedSubnets也不包含X，则X从bestClaims中消失
5. X不再被传播，自动清理

**关键**：Routes和DomainSuffixes引用nodeID，当nodeID对应的ClaimedSubnet消失时，相关的Routes和DomainSuffixes也自动失效。

### 全网拓扑推导

从ClaimedSubnets推导拓扑边：

```go
// 从ClaimedSubnets推导拓扑
edgeSet := make(map[string]FullTopologyEdge)

for _, cs := range allClaimedSubnets {
    for _, neighbor := range cs.Neighbors {
        key := edgeKey(cs.NodeID, neighbor)
        edgeSet[key] = FullTopologyEdge{From: cs.NodeID, To: neighbor}
    }
}
```

**示例**：
- gg.neighbors=[ms9, ms10] → 边(gg,ms9), (gg,ms10)
- ms9.neighbors=[gg, x] → 边(ms9,gg), (ms9,x)
- ms10.neighbors=[gg] → 边(ms10,gg)

去重后得到全网拓扑。

## 实现步骤

1. **协议层**：
   - 修改GossipInfo结构，移除NodeID和Subnet字段
   - 修改GossipRoute和GossipDomainSuffix，移除Hop字段，添加NodeID字段
   - 修改GossipClaimedSubnet，添加Neighbors字段
   - 移除GossipTopologyEdge结构

2. **发送逻辑**：
   - 修改broadcastGossip()，构建新的报文格式
   - 自己的ClaimedSubnet条目包含Neighbors
   - Routes和DomainSuffixes引用nodeID

3. **接收逻辑**：
   - 修改UpdateGossip()，解析新格式
   - 建立nodeID到hop/subnet/neighbors的映射
   - Routes和DomainSuffixes的hop从映射中查找

4. **拓扑展示**：
   - 修改GetFullTopology()，从ClaimedSubnets.Neighbors推导边
   - 移除对TopologyEdges的依赖

5. **测试验证**：
   - 验证过时路由自动清理
   - 验证全网拓扑正确展示
   - 验证split horizon正常工作

## 优势

1. **解决游荡问题**：过时路由自动清理，不会累积到400+ hop
2. **消除冗余**：减少字段重复，简化协议
3. **减小报文**：移除冗余字段，减少网络开销
4. **逻辑清晰**：引用式设计，关系明确
5. **自动一致性**：节点信息变更时，相关路由和拓扑自动更新

## 状态

- [x] 问题分析完成
- [x] 冗余分析完成
- [x] 新设计完成
- [ ] 实现
