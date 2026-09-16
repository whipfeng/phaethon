# Mesh 网络改进设计

> 版本: v0.11.0
> 日期: 2026-09-16
> 状态: IMPLEMENTED
> 负责人: Phaethon Dev
> 依赖: [mesh_multi_vip_design.md](mesh_multi_vip_design.md) v0.4.1

## 变更记录

| 版本 | 日期 | 变更内容 | 作者 |
|------|------|----------|------|
| v0.1.0 | 2026-09-15 | 初始版本：整合已完成的 mesh 改进（src IP 重写、P2P 协议版本、Auto P2P、地址空间扩大等） | Qoder |
| v0.2.0 | 2026-09-15 | 新增 Mode B Mesh 路由设计（待实现） | Qoder |
| v0.3.0 | 2026-09-15 | MeshDial 不使用 DirectDialer，直接调用全局 netstack 函数；新增 ADR-4 | Qoder |
| v0.4.0 | 2026-09-15 | 清理 DirectDialer 中的 netstack 路径（已无引用，避免误导） | Qoder |
| v0.5.0 | 2026-09-15 | Mode B mesh 路由实现完成：MeshDial、server handler 统一、DNS hijacker 清理 | Qoder |
| v0.6.0 | 2026-09-15 | ~~writeLoop 分支结构修复~~ → 废弃，改用统一 DNS hijacker 方案 | Qoder |
| v0.7.0 | 2026-09-15 | ~~GIP 路径 src IP 重写~~ → 废弃，改用统一 DNS hijacker 方案 | Qoder |
| v0.8.0 | 2026-09-15 | 统一 DNS hijacker 方案：删除 tryDNSRedirect 拦截，DNS hijacker 统一处理 Mode A/B DNS，支持跨节点转发和缓存 | Qoder |
| v0.9.0 | 2026-09-16 | 多链路路由优化：路由表存所有 peer，按跳数排序，同跳数轮询负载均衡 | Qoder |
| v0.10.0 | 2026-09-16 | 自动生成 nodeID.phn DNS 路由条目：内置 .phn 后缀 + claimedSubnets 组合，修复跨节点 nodeID 域名路由 | Qoder |
| v0.11.0 | 2026-09-16 | 统一 domain trie 和路由表结构：trie 改为 peers 数组、本地判断统一用 len(peers)==0、own subnet 加入路由表 | Qoder |

## 1. 背景与目标

### 1.1 当前问题

Mesh 网络在多 VIP 设计（v0.4.1）基础上，存在以下问题需要解决：

1. **入站数据包 src IP 错误**：跨节点 DNS 响应到达 Windows 后被丢弃
2. **P2P 协议版本不兼容**：旧版本客户端连接后无校验
3. **P2P 连接需要手动启用**：mesh 启用时应自动建立 P2P 连接
4. **TCP 跨节点连接失败**：子网重叠 + localNodeDomain 被规则覆盖
5. **地址空间不足**：硬编码 /16 网络，每节点 /24 只有 252 个 Fake-IP
6. **Mode B 流量未走 mesh**：代理入口流量仍通过 proxy chain 拨号

### 1.2 目标

1. 修复跨节点 DNS 和 TCP 连通性
2. 自动化 P2P 连接管理
3. 扩大地址空间至可配置范围
4. Mode B 流量统一走 mesh 网络

## 2. 已完成的改进

### 2.1 入站数据包 src IP 重写

**问题**：跨节点 DNS 响应到达 Windows 后被丢弃，因为 src IP 是远端 GIP 而非本地 GIP。

**根因**：
- DNS 查询流程：app → localGIP(.3) → NAT(src→VIP) → tryDNSRedirect(dst→remoteGIP) → mesh
- DNS 响应流程：远端 → src=remoteGIP, dst=VIP → 本地 HandleMeshFrame
- NAT reverse 只重写 dst（VIP→hostIP），不重写 src（remoteGIP 保持不变）
- Windows DNS 客户端期望 src=localGIP，收到 src=remoteGIP 后丢弃

**修复**：
- `mesh/mesh.go` VIP 路径区分 DNS 和 TCP 响应：
  - DNS 响应（UDP port 53）：使用 `TranslateInboundWithSrc` 重写 src 为 localGIP
  - TCP 响应：使用 `TranslateInbound` 保持原始 src IP（Fake-IP）
- 新增 `tun/nat.go`：`TranslateInboundWithSrc`、`RewriteSrcIP` 方法

**验证**：跨节点 DNS 解析正常，qg.phn → 100.64.0.9 ✓

### 2.2 P2P 协议版本

**实现**：
- `p2p/p2p.go`：`const P2PProtocolVersion = 1`
- `HelloMsg` 新增 `ProtocolVersion` 字段
- `handleHello()` 校验版本，不匹配则断开

**兼容性**：旧版本无此字段，反序列化为 0，被正确拒绝。

### 2.3 Auto P2P

**问题**：P2P 连接需要手动勾选 checkbox。

**修复**：`main.go` 中 mesh 启用时，自动对所有兼容代理（socks5、trojan、h_tunnel）启动 P2P。

### 2.4 多 P2P 连接共存与负载均衡

**问题**：同一服务器的多个代理各自启动 P2P，导致驱逐循环。

**修复**：
- 移除 `handleHello` 中相同 MeshNodeID 的驱逐逻辑
- `mesh/mesh.go` 新增 `findPeers()` 方法，返回同一节点的所有 peer
- 发送时随机选择 peer，实现负载均衡

### 2.5 TCP 跨节点连接修复

**根因 1**：子网地址空间重叠
- VM 配置 `subnet: 100.0.1.0/16` 与 QG `subnet: 100.0.0.0/16` 在 /16 掩码下重叠
- 修复：VM 子网改为 `100.1.0.0/16`

**根因 2**：localNodeDomain 被代理规则覆盖
- `localNodeDomain` 检查仅在 DIRECT 分支中，proxy 分支忽略
- 修复：`tun/engine.go` 中将 `localNodeDomain` 检查提前到代理匹配之前

### 2.6 Mesh 网络地址空间扩大

**问题**：硬编码 100.64.0.0/16，每节点 /24 只有 252 个 Fake-IP。

**修复**：
- 整体 mesh 网络：可配置（默认 100.64.0.0/10）
- 每节点子网：可配置前缀长度（默认 /18，可设为 /16 获得 65536 个地址）
- 配置示例：

```yaml
mesh:
    enabled: true
    node-id: qg
    network: 100.0.0.0/8      # 整体 mesh 网络
    subnet: 100.0.0.0/16     # 本节点子网
    domain-suffixes:
        - phn
```

**代码改动**：
- `config/config.go`：`Mesh.Network` 字段 + `GetNetwork()`
- `mesh/forward.go`：`SetMeshCIDR()`/`GetMeshCIDR()`
- `mesh/state.go`：`AllocateSubnet` 支持可配置网络范围
- `mesh/mesh.go`：存储 network 和 subnetPrefixLen
- `main.go`：mesh 初始化时设置 meshCIDR、推导 subnetPrefixLen

### 2.7 DNS 调试日志

已在以下位置添加 `[DNS-DEBUG]` 日志：
- `tun/engine.go`：readLoop 中检测 GIP:53 的 DNS 查询
- `tun/engine.go`：tryDNSRedirect 中记录域名和 remoteGIP
- `tun/dns.go`：DNSHijacker 中记录查询域名和来源

### 2.8 多链路路由优化（设计中）

**问题**：当前路由表只存一个 peer（最优路径），但多连接场景下需要二次查找所有同 nodeID 的连接，效率低且选路策略简单（纯随机）。

**现状**：
```go
// 路由计算：只存一个 peer
best[prefix] = globalEntry{hop, peer, subnet}

// 包发送：二次查找
peers := m.findPeers(dstIP)  // 先查路由表，再遍历所有 peer 找同 nodeID
selectedPeer := peers[rand.Intn(len(peers))]  // 纯随机
```

**问题**：
1. 路由表只存一个 peer，但发送时要重新查找所有同 nodeID 的连接 → O(n) 每次发包
2. 纯随机选路 → 不考虑链路质量，可能频繁切换
3. 路由重算后没有状态保持

**新设计**：

#### 2.8.1 路由表结构

路由表直接存储所有可用 peer，按跳数排序：

```go
type MeshRoute struct {
    Prefix  *net.IPNet
    Peers   []PeerWithHop  // 按跳数排序
    lastIdx int            // 轮询索引，路由重算时重置为 0
}

type PeerWithHop struct {
    Peer PeerSender
    Hop  int
}
```

#### 2.8.2 路由计算（recomputeRoutes）

收集所有 peer，按跳数排序：

```go
// 收集所有 peer 到每个前缀
peerMap := make(map[string][]PeerWithHop)
for _, peer := range peers {
    for _, r := range peer.Routes {
        peerMap[r.PrefixStr] = append(peerMap[r.PrefixStr], PeerWithHop{peer.Sender, r.Hop})
    }
}

// 排序：跳数低的在前
for prefix, peers := range peerMap {
    sort.Slice(peers, func(i, j int) bool {
        return peers[i].Hop < peers[j].Hop
    })
    routes = append(routes, MeshRoute{Prefix: prefix, Peers: peers, lastIdx: 0})
}
```

#### 2.8.3 选路策略（HandleOutboundPacket）

跳数低的优先，同跳数轮询：

```go
route := m.findRoute(dstIP)
if route == nil || len(route.Peers) == 0 {
    return false
}

// 找最低跳数
minHop := route.Peers[0].Hop

// 统计同跳数的 peer 数量
count := 0
for _, p := range route.Peers {
    if p.Hop == minHop {
        count++
    } else {
        break  // 已排序，遇到不同跳数就停止
    }
}

// 轮询选择
idx := route.lastIdx % count
selectedPeer := route.Peers[idx].Peer
route.lastIdx++

// 发送
selectedPeer.Send(pkt)
```

#### 2.8.4 路由重算时的状态重置

路由重算时，`lastIdx` 重置为 0：

```go
func (m *MeshManager) recomputeRoutes() {
    // ... 重新计算路由 ...
    // 新路由的 lastIdx 默认为 0
}
```

#### 2.8.5 优势

| 维度 | 旧设计 | 新设计 |
|------|--------|--------|
| **路由表** | 存 1 个 peer | 存所有 peer（按跳数排序） |
| **选路复杂度** | O(n) 每次发包 | O(1) 查路由表 + O(1) 轮询 |
| **负载均衡** | 纯随机 | 同跳数轮询（round-robin） |
| **状态保持** | 无 | lastIdx 记录上次选择 |
| **跳数优先** | 隐式（路由表只存最优） | 显式（排序后选最低跳数组） |

#### 2.8.6 改动文件

| 文件 | 改动 |
|------|------|
| `mesh/mesh.go` | `MeshRoute` 结构改为存所有 peer + lastIdx |
| `mesh/mesh.go` | `recomputeRoutes` 收集所有 peer 并排序 |
| `mesh/mesh.go` | `findPeer` → `findRoute` 返回完整路由 |
| `mesh/mesh.go` | `HandleOutboundPacket` 选路逻辑改为轮询 |
| `mesh/mesh.go` | 删除 `findPeers` 方法（不再需要） |

### 2.9 自动生成 nodeID.phn DNS 路由条目（v0.10.0）

**问题**：跨节点 DNS 路由中，`nodeID.phn` 格式的域名无法正确路由到目标节点。

**根因分析**：

当前 domain trie 只存储用户配置的后缀（如 `phn`），不区分同一后缀下不同 nodeID 的归属。

以 "vm.phn" 从 JF 查询为例：
```
JF: ResolveDomainSubnet("vm.phn")
  → trie 匹配 "phn" → nextHop=QG → 转发到 QG
QG: 收到 "vm.phn"
  → trie 匹配 "phn" → nextHop=nil（自己的后缀）→ 从本地池分配 Fake-IP
  → 流量到达 QG，但 QG 的 localNodeDomain 检查 "vm.phn" ≠ "qg.phn" → 不匹配
  → 走普通代理链 → 失败
```

QG 不知道 "vm" 是另一个 mesh node，把 "vm.phn" 当成普通域名处理。

**设计**：

两个独立的 DNS 路由机制共存：

1. **自动生成**（内置）：`.phn` 是软件内置的默认后缀。每个节点从 claimedSubnets 中已知的 nodeID 自动生成 `nodeID.phn` 条目，插入 domain trie。
2. **用户配置**：`domain-suffixes` 中的自定义后缀（如 `test.jf.local`、`httpbin.org`）照常通告、照常插入 trie。

#### 2.9.1 自动生成逻辑

在 `recomputeRoutes` 构建 domain trie 时，除了插入用户配置的后缀和 peer 通告的后缀，还自动为每个已知节点生成 `nodeID.phn` 条目：

```go
const defaultMeshSuffix = "phn"

// 自动生成 nodeID.phn 条目
for _, cs := range allClaimedSubnets {
    domain := cs.NodeID + "." + defaultMeshSuffix  // e.g. "vm.phn"
    if cs.NodeID == m.nodeID {
        // 自己的节点 → 本地
        trie.Insert(domain, nil, nil, 0)
    } else {
        // 其他节点 → 找到对应的 peer 作为 nextHop
        peer, hop := findNextHopForNode(cs.NodeID, peers)
        if peer != nil {
            trie.Insert(domain, peer, cs.Subnet, hop)
        }
    }
}
```

#### 2.9.2 效果

以 QG 的 domain trie 为例（已知节点：qg、vm、jf）：

```
自动生成：
  qg.phn → nextHop=nil (本地), subnet=nil
  vm.phn → nextHop=VM peer, subnet=100.1.0.0/16, hop=1
  jf.phn → nextHop=JF peer, subnet=100.2.0.0/16, hop=1

用户配置（如有）：
  test.jf.local → nextHop=JF peer, subnet=100.2.0.0/16
  httpbin.org → nextHop=JF peer, subnet=100.2.0.0/16
```

查询 "vm.phn" 时：
- JF: trie 匹配 "vm.phn" → nextHop=VM, subnet=100.1.0.0/16 → 转发到 VM
- VM: trie 匹配 "vm.phn" → nextHop=nil → 本地解析 → localNodeDomain 匹配 → 拨号本地 ✓

#### 2.9.3 Gossip 变化

自动生成的 `nodeID.phn` 条目**不需要通过 gossip 通告**。每个节点都从 claimedSubnets（已通过 gossip 同步）本地生成相同的条目。

用户配置的 `domain-suffixes` 仍通过 gossip 正常通告。

#### 2.9.4 改动文件

| 文件 | 改动 |
|------|------|
| `mesh/mesh.go` | `recomputeRoutes` 中自动生成 `nodeID.phn` 条目插入 domain trie |
| `mesh/mesh.go` | 新增 `defaultMeshSuffix` 常量 |
| `mesh/mesh.go` | 新增辅助函数：根据 nodeID 查找 nextHop peer |

### 2.10 统一 domain trie 和路由表结构（v0.11.0）

**问题**：domain trie 和路由表结构不一致，本地/远端判断逻辑不统一。

1. **domain trie** 存单个 `nextHop *PeerInfo`，路由表存 `Peers []PeerWithHop`
2. domain trie 用 `subnet == nil` 判断本地（隐含约定，容易出错）
3. 路由表把 own subnet 排除在外，`HandleOutboundPacket` 单独用 `m.subnet.Contains()` 判断本地
4. DNS 转发无法利用多链路负载均衡

**设计**：统一为同一套数据结构，同一个判断逻辑。

#### 2.10.1 统一数据结构

Domain trie 和路由表都使用 `[]PeerWithHop` 数组：

```go
// domain trie 节点
type trieNode struct {
    children map[string]*trieNode
    hasEntry bool
    peers    []PeerWithHop   // 改为 peer 数组，和路由表一致
    subnet   *net.IPNet
}
```

**本地判断**：`len(peers) == 0` → 本地，`len(peers) > 0` → 远端。

不再依赖 `subnet == nil` 或 `nextHop == nil` 等隐含约定。

#### 2.10.2 Domain Trie 改动

**Insert**：接受 `[]PeerWithHop`，按 hop 排序存储。同一 suffix 多次 Insert 时合并 peers（去重，保留最低 hop）。

**Lookup**：返回 `([]PeerWithHop, *net.IPNet, int)` 而不是 `(*PeerInfo, *net.IPNet, int)`。

**纯最长匹配**：去掉 `foundOwn` 特殊逻辑。遍历标签时，任何 `hasEntry` 节点只要 `suffixLen > bestLen` 就更新。不再区分 own/remote。

```go
func (t *DomainTrie) Lookup(domain string) ([]PeerWithHop, *net.IPNet, int) {
    // ...
    if node.hasEntry {
        suffixLen := accumulated + len(label)
        if suffixLen > bestLen {
            bestLen = suffixLen
            bestPeers = node.peers
            bestSubnet = node.subnet
        }
    }
    // ...
}
```

#### 2.10.3 ResolveDomainSubnet 改动

```go
func (m *MeshManager) ResolveDomainSubnet(domain string) *net.IPNet {
    peers, subnet, suffixLen := trie.Lookup(domain)
    if suffixLen == 0 || len(peers) == 0 {
        return nil  // 无匹配 或 本地 → 不转发
    }
    return subnet
}
```

用 `len(peers) == 0` 判断本地，不再依赖 `subnet == nil`。

#### 2.10.4 路由表改动

Own subnet 加入路由表，peers 为空：

```go
// recomputeRoutes 中
// 不再排除 ownPrefixes，改为加入路由表
routes = append(routes, MeshRoute{
    Prefix:  ownSubnet,
    Peers:   nil,  // 空 → 本地
    lastIdx: 0,
})
```

#### 2.10.5 HandleOutboundPacket 改动

统一走 `findRoute`，去掉单独的 `m.subnet.Contains()` 检查：

```go
func (m *MeshManager) HandleOutboundPacket(dstIP net.IP, pkt []byte) bool {
    route := m.findRoute(dstIP)
    if route == nil {
        return false  // 无路由
    }
    if len(route.Peers) == 0 {
        // 本地路由 → 投递给 netstack
        return m.deliverLocal(dstIP, pkt)
    }
    // 远端路由 → mesh 转发（轮询选 peer）
    // ...
}
```

#### 2.10.6 DNS 转发多链路

DNS 转发时，从 trie 返回的 peers 数组中轮询选择，和 IP 路由一样：

```go
peers, subnet, _ := trie.Lookup(domain)
if len(peers) > 0 {
    // 轮询选 peer
    idx := lastIdx % len(peers)
    peer := peers[idx]
    lastIdx++
    // 通过 peer 转发 DNS 查询
}
```

#### 2.10.7 自动生成 nodeID.phn 条目

自动生成的 own 条目带 subnet 值（用于环路排除），peers 为空：

```go
bestNodes[m.nodeID] = nodeClaim{nil, ownSubnet, 0}
// Insert 时：peers=nil（空），subnet=ownSubnet
```

#### 2.10.8 改动文件

| 文件 | 改动 |
|------|------|
| `mesh/domain_trie.go` | trieNode 改为 `peers []PeerWithHop`；Insert/Lookup 签名改为 peers 数组；Lookup 改为纯最长匹配 |
| `mesh/domain_trie_test.go` | 更新测试适配新签名 |
| `mesh/mesh.go` | `ResolveDomainSubnet` 改用 `len(peers)==0` 判断本地 |
| `mesh/mesh.go` | `recomputeRoutes` 中 own subnet 加入路由表（peers 为空） |
| `mesh/mesh.go` | `HandleOutboundPacket` 统一走 `findRoute`，去掉 `m.subnet.Contains` 检查 |
| `mesh/mesh.go` | 自动生成 nodeID.phn 条目带 ownSubnet |

## 3. Mode B Mesh 路由设计（待实现）

### 3.1 问题

当前 Mode B（代理入口）流量仍通过 proxy chain 拨号，未走 mesh 网络。

### 3.2 设计原则

1. **Mesh 始终启用**：不需要判断 `IsMeshEnabled()`，所有流量直接走 mesh
2. **不匹配规则**：Mode B 不做规则匹配，不获取 proxy，直接拨号目标
3. **统一抽象**：所有 server handler 共用同一个拨号函数 `MeshDial()`
4. **DNS 转发**：在 writeLoop 的 `tryDNSRedirect` 中拦截，不在 DNS hijacker 中

### 3.3 数据流对比

#### Mode A（TUN 入口）

```
Windows 应用
  ↓
Wintun → readLoop → InjectInbound → gVisor netstack
                                        ↓
                              TCP/UDP Forwarder
                                        ↓
                              handleConn（规则匹配）
                                        ↓
                              chainDial（proxy chain）
                                        ↓
                              目标服务器

DNS 查询路径：
  readLoop → tryDNSRedirect（重写 dst→远端GIP）→ mesh → 远端节点
                                                      ↓
  应用 ← TUN ← writeLoop ← 重写 src←本地GIP ← DNS 响应 ← DNS hijacker
```

#### Mode B（代理入口）

```
客户端 → SOCKS5/Trojan/HTTP/HTunnel server
              ↓
         MeshDial（直接拨号，不匹配规则）
              ↓
         netstack socket（DNS 解析 + TCP 连接）
              ↓
         writeLoop 拦截 outbound 包
              ↓
         ┌─ TCP：直接 mesh 路由到目标
         └─ UDP:53：tryDNSRedirect（重写 dst→远端GIP）→ mesh → 远端节点
                                                                ↓
         netstack socket ← writeLoop ← 重写 src←本地GIP ← DNS 响应 ← DNS hijacker
              ↓
         用 Fake-IP 建立 TCP 连接 → writeLoop → mesh 路由
```

### 3.4 DNS 转发设计

**关键**：DNS 转发不在 DNS hijacker 中实现，而是在 writeLoop 的 `tryDNSRedirect` 中处理。

#### 去程（outbound）

```
writeLoop 拦截 DNS 查询包（dst=本地GIP:53）
  ↓
tryDNSRedirect：
  1. 解析域名
  2. 调用 meshGatewayResolver(domain) 获取远端 GIP
  3. 重写 dst IP：本地GIP → 远端GIP
  4. 通过 meshInterceptor 发送到远端节点
```

#### 远端节点处理

```
收到 DNS 查询（dst=自己的GIP:53）
  ↓
DNS hijacker 从本地 Fake-IP 池分配地址
  ↓
返回 DNS 响应（src=远端GIP, dst=查询方）
```

#### 回程（inbound）

```
响应包到达本地（src=远端GIP, dst=本地VIP/GIP）
  ↓
mesh.HandleMeshFrame → NAT reverse：
  - DNS 响应（UDP:53）：重写 src 为本地GIP
  - TCP 响应：保持原始 src（Fake-IP）
  ↓
writeLoop 投递给 netstack socket
  ↓
原始查询方收到响应（src=本地GIP，符合预期）
```

### 3.5 MeshDial 的实现

`MeshDial()` 直接使用全局 netstack 函数，不经过 `DirectDialer`（避免与规则匹配中的"DIRECT 直连"混淆）：

```go
// MeshDial dials destination through mesh network.
// Always uses netstack path: DNS resolution → Fake-IP → mesh routing.
func MeshDial(dstAddr string, dstPort int) (net.Conn, error) {
    if GlobalNetstackDialFunc == nil || GlobalDNSResolverFunc == nil {
        return nil, fmt.Errorf("mesh not initialized")
    }

    var targetAddr string
    if ip := net.ParseIP(dstAddr); ip == nil {
        // 域名：通过 netstack DNS 解析获取 Fake-IP
        fakeIP, err := GlobalDNSResolverFunc(dstAddr)
        if err != nil {
            return nil, fmt.Errorf("mesh dns resolve %s: %w", dstAddr, err)
        }
        targetAddr = net.JoinHostPort(fakeIP.String(), strconv.Itoa(dstPort))
    } else {
        // IP：直接使用
        targetAddr = net.JoinHostPort(dstAddr, strconv.Itoa(dstPort))
    }

    // 通过 netstack 拨号，经过 writeLoop → mesh 路由
    conn, err := GlobalNetstackDialFunc("tcp", targetAddr)
    if err != nil {
        return nil, err
    }
    util.SetTCPNoDelay(conn)
    return conn, nil
}
```

**两种情况**：

1. **域名**：
   - 调用 `GlobalDNSResolverFunc(domain)` → `engine.ResolveDomain`
   - `ResolveDomain` 通过 netstack 发送 DNS 查询到 dnsAddr:53
   - DNS 查询经过 writeLoop → `tryDNSRedirect` 拦截 → mesh 转发到远端
   - 远端 DNS hijacker 响应 → 回程重写 src → 返回 Fake-IP
   - 用 Fake-IP 通过 `GlobalNetstackDialFunc` 建立 TCP 连接

2. **IP 地址**：
   - 直接通过 `GlobalNetstackDialFunc("tcp", ip:port)` 拨号
   - TCP 连接经过 writeLoop → mesh 路由到目标节点

### 3.6 代码调整

#### 需要回退的错误实现（commit 27d9b41）

1. **dialer/bind.go**：
   - 删除 `IsMeshEnabled()` 函数
   - 删除 `ModeBMeshDial()` 函数

2. **tun/dns.go**：
   - 删除 `meshGatewayResolver` 字段
   - 删除 `SetMeshGatewayResolver()` 方法
   - 删除 `forwardDNSQuery()` 方法
   - 删除 serveLoop 中的 mesh gateway 检查逻辑
   - 删除 `time` import（如果只被 forwardDNSQuery 使用）

3. **main_tun.go**：
   - 删除 DNS hijacker 的 `SetMeshGatewayResolver` 调用和相关日志

4. **server/*.go**（6 个文件）：
   - 删除所有 `if dialer.IsMeshEnabled()` 判断
   - 删除所有 `dialer.ModeBMeshDial()` 调用
   - 删除所有规则匹配逻辑（`RuleConf.Match`）

#### 正确实现

1. **dialer/bind.go** - 新增 `MeshDial()`：
   - 直接使用 `GlobalDNSResolverFunc` 和 `GlobalNetstackDialFunc`
   - 不经过 `DirectDialer`（详见 3.5 节完整实现）

2. **server handler 统一改为**（6 个文件）：

```go
// 删除：规则匹配、proxy 变量、IsMeshEnabled 判断
// 直接：
targetConn, err := dialer.MeshDial(dstAddr, dstPort)
```

3. **main_tun.go** - 保持不变：
   - `dialer.GlobalNetstackDialFunc = engine.NetDial` ✓
   - `dialer.GlobalDNSResolverFunc = engine.ResolveDomain` ✓

### 3.7 实现步骤

1. **实现 MeshDial 函数**（dialer/bind.go）
   - 删除 `IsMeshEnabled()`、`ModeBMeshDial()`
   - 新增 `MeshDial(dstAddr, dstPort)` 函数
   - 直接使用 `GlobalDNSResolverFunc` 和 `GlobalNetstackDialFunc`（详见 3.5 节）

2. **清理 DirectDialer**（dialer/direct.go）
   - 删除 `Dial()` 中的 netstack 路径（`GlobalNetstackDialFunc`/`GlobalDNSResolverFunc` 分支）
   - 只保留 OS socket 路径（`DialRouteAware`）

3. **回退 DNS hijacker 错误实现**（tun/dns.go, main_tun.go）
   - 删除 `meshGatewayResolver` 相关代码
   - DNS hijacker 只负责从本地池分配 Fake-IP

4. **修改所有 server handler**
   - socks5.go、trojan.go、http.go、htunnel.go、direct.go、reverse.go
   - 删除规则匹配和 proxy chain 逻辑
   - 统一调用 `dialer.MeshDial()`

### 3.8 统一 DNS hijacker 方案（替代 tryDNSRedirect 拦截）

**背景**：之前尝试在 writeLoop 中用 `tryDNSRedirect` 拦截 DNS 查询实现跨节点转发（v0.6.0/v0.7.0），但遇到以下问题：
- Mode B 的 writeLoop 分支结构导致 `tryDNSRedirect` 被跳过
- 修复分支结构后，DNS 响应回程需要 GIP 路径 src IP 重写 + checksum 重算
- 链路太长：writeLoop 拦截 → mesh 转发 → 远端 hijacker → 响应 → mesh 回程 → GIP 路径重写 → netstack
- Mode A/B 需要不同的处理路径，难以统一

**新方案**：DNS hijacker 统一处理所有 DNS 查询（Mode A/B 不再区分），删除 writeLoop 拦截机制。

#### 3.8.1 设计原则

1. **DNS hijacker 是唯一的 DNS 处理入口** — 所有 DNS 查询（无论来自 Mode A 的 TUN 还是 Mode B 的 netstack socket）都到达 DNS hijacker
2. **删除 tryDNSRedirect** — 不再在 writeLoop 或 InjectMeshPacket 中拦截 DNS 查询
3. **删除 GIP 路径 src IP 重写** — 不再需要，因为 DNS 响应由 hijacker 直接返回
4. **DNS hijacker 支持跨节点转发** — 本地域名直接分配 Fake-IP，远端域名转发到对应节点的 DNS hijacker
5. **DNS 缓存** — hijacker 缓存远端返回的域名→Fake-IP 映射

#### 3.8.2 数据流

**本地域名查询**（域名后缀匹配本节点）：
```
查询方 → DNS hijacker（本地 GIP:53）
  → 匹配本地域名后缀
  → 从本地 Fake-IP 池分配地址
  → 返回响应（src=localGIP, dst=查询方）
```

**远端域名查询**（域名后缀匹配远端节点）：
```
查询方 → 本地 DNS hijacker（localGIP:53）
  → 匹配远端域名后缀
  → 检查缓存：有则直接返回
  → 无缓存：转发到远端 DNS hijacker（通过 mesh）
  → 远端 hijacker 分配 Fake-IP，返回
  → 本地 hijacker 缓存映射，返回给查询方
```

**Mode A（TUN 入口）流程**：
```
Windows 应用 → TUN → readLoop → NAT(src→VIP) → netstack → DNS hijacker
  → hijacker 判断域名归属
  → 本地：分配 Fake-IP，返回
  → 远端：转发到远端 hijacker，等待响应，返回
  → 响应 src=localGIP, dst=VIP → NAT reverse → TUN → 应用
```

**Mode B（代理入口）流程**：
```
SOCKS5 handler → MeshDial → ResolveDomain → netstack DNS endpoint → DNS hijacker
  → hijacker 判断域名归属
  → 本地：分配 Fake-IP，返回
  → 远端：转发到远端 hijacker，等待响应，返回
  → 响应 src=localGIP, dst=GIP → netstack endpoint → ResolveDomain 返回 Fake-IP
  → MeshDial 用 Fake-IP 建立 TCP 连接 → mesh 路由到目标
```

#### 3.8.3 域名后缀通告增加网段字段

当前 gossip 通告格式：
```go
type GossipDomainSuffix struct {
    Suffix string `json:"suffix"`
    Hop    int    `json:"hop"`
}
```

新格式（增加网段字段）：
```go
type GossipDomainSuffix struct {
    Suffix string `json:"suffix"`
    Subnet string `json:"subnet"` // 该节点的 Fake-IP 网段，如 "100.2.0.0/16"
    Hop    int    `json:"hop"`
}
```

DNS hijacker 转发时，根据网段字段知道远端节点的 Fake-IP 范围，用于：
1. 构造转发查询的目标地址（远端 GIP:53）
2. 缓存时记录 Fake-IP 属于哪个网段

#### 3.8.4 P2P 协议版本升级

由于 gossip 格式变化（`GossipDomainSuffix` 增加 `subnet` 字段），需要升级 P2P 协议版本：
- 当前：`P2PProtocolVersion = 1`
- 升级：`P2PProtocolVersion = 2`
- 旧版本（version=0 或 1）会被拒绝连接

#### 3.8.5 DNS hijacker 转发实现

DNS hijacker 新增以下能力：

1. **域名后缀匹配**：根据 gossip 学到的 `suffix → subnet` 映射，判断域名归属
2. **跨节点转发**：远端域名通过 gvisor netstack UDP socket 转发到远端节点的 DNS hijacker
3. **缓存**：缓存远端返回的 `domain → Fake-IP` 映射，带 TTL

**转发方式**：DNS hijacker 创建 gvisor UDP socket，Connect 到远端 GIP:53，发送原始 DNS 查询。数据包经过 netstack → writeLoop → meshInterceptor → mesh 路由到远端节点 → 远端 HandleMeshFrame → GIP 路径 → InjectMeshPacket → 远端 DNS hijacker。响应沿原路返回。

```go
// DNSHijacker 新增字段
type DNSHijacker struct {
    // ... 现有字段 ...
    
    // 跨节点 DNS 转发
    domainSuffixes  *DomainTrie       // suffix → subnet 映射（从 gossip 学习）
    cache           *DNSCache         // DNS 缓存
    
    // 本节点信息
    localSubnet     *net.IPNet        // 本节点 Fake-IP 网段
}

// 转发示例（在 serveLoop 中）
func (h *DNSHijacker) forwardToRemote(subnet *net.IPNet, query []byte) (net.IP, error) {
    // 从 subnet 推导远端 GIP
    remoteGIP := DeriveGIPFromSubnet(subnet)
    
    // 创建 gvisor UDP socket，发送到远端 GIP:53
    var wq waiter.Queue
    ep, err := h.ns.NewEndpoint(udp.ProtocolNumber, ipv4.ProtocolNumber, &wq)
    // ... bind, connect to remoteGIP:53 ...
    
    // 发送 DNS 查询
    ep.Write(&slicePayload{data: query}, tcpip.WriteOptions{})
    
    // 等待响应
    // ... read response, parse Fake-IP ...
}
```

**优势**：
- 不需要额外的 mesh DNS 转发接口
- 复用现有的 netstack → writeLoop → mesh 路径
- 远端 DNS hijacker 收到的查询和普通 DNS 查询一样处理

#### 3.8.7 需要回退的拦截点（完整清单）

**tun/engine.go**（8 处）：
1. Line 115: `meshGatewayResolver` 字段声明
2. Lines 150-153: `SetMeshGatewayResolver()` 方法
3. Lines 351-357: `InjectMeshPacket` 中的 `tryDNSRedirect` 调用
4. Lines 1076-1093: readLoop 中的 DNS debug 日志
5. Lines 1095-1101: readLoop 中的 `tryDNSRedirect` 调用
6. Lines 1127-1197: `tryDNSRedirect` 函数本身
7. Lines 1306-1313: writeLoop 中的 `tryDNSRedirect` 调用
8. Line 1776-1777: `queryInternalDNS` 注释引用 tryDNSRedirect

**tun/dns.go**（1 处）：
9. Line 139: `[DNS-DEBUG]` 日志

**mesh/mesh.go**（5 处）：
10. Lines 403-432: `GetGatewayGIPForDomain` 方法
11. Lines 434-438: `ResolveGatewayGIP` 方法
12. Lines 587-607: VIP 路径中 DNS 响应特殊处理（isDNS 检测 + `TranslateInboundWithSrc`）— 统一 DNS hijacker 后 DNS 响应走 GIP 路径，VIP 路径不再需要 DNS 分支
13. Lines 621-659: VIP NAT reverse 失败时的 fallback 代码（注释引用 tryDNSRedirect）— 同上，不再需要
14. Lines 620-627（原 #12）: VIP 路径中引用 tryDNSRedirect 的注释

**main_tun.go**（1 处）：
15. Line 105: `engine.SetMeshGatewayResolver(meshMgr.ResolveGatewayGIP)` 调用

#### 3.8.8 实现步骤

1. **回退所有拦截代码**（15 处，见 3.8.7 清单）
   - tun/engine.go：删除 tryDNSRedirect 函数、meshGatewayResolver 字段、所有调用点
   - mesh/mesh.go：删除 GetGatewayGIPForDomain、ResolveGatewayGIP、VIP 路径 DNS 特殊处理及 fallback
   - main_tun.go：删除 SetMeshGatewayResolver 调用

2. **升级 P2P 协议版本**
   - p2p/p2p.go：`P2PProtocolVersion = 2`

3. **扩展 gossip 格式**
   - mesh/topology.go：`GossipDomainSuffix` 增加 `Subnet` 字段
   - `PeerDomainSuffixEntry` 增加 `Subnet` 字段
   - 更新 gossip 序列化/反序列化

4. **DNS hijacker 增加跨节点转发能力**
   - tun/dns.go：新增 domainSuffixes trie、cache
   - serveLoop 中判断域名归属：本地直接分配，远端用 gvisor socket 转发
   - 从 gossip 更新 domainSuffixes

5. **DNS 缓存**
   - tun/dns.go：实现简单的 DNS 缓存（domain → Fake-IP + TTL）
   - 缓存命中时直接返回，不转发

6. **部署验证**（需同时部署所有节点）
   - 验证 Mode A DNS（TUN 入口）
   - 验证 Mode B DNS（SOCKS5 入口）
   - 验证 DNS 缓存生效

## 4. 关键设计决策

### 4.1 ADR-1: Mode B 不做规则匹配

**决策**：Mode B（代理入口）流量不做规则匹配，直接通过 mesh 路由。

**理由**：
- Mode B 的流量已经进入 mesh 网络，目标是到达 mesh 内的其他节点
- 规则匹配用于决定流量是否走代理，但 Mode B 本身就是代理入口
- 简化实现，避免不必要的复杂性

### 4.2 ADR-2: DNS 转发在 DNS hijacker 而非 writeLoop

**决策**：跨节点 DNS 转发在 DNS hijacker 中实现，不在 writeLoop 中拦截。

**理由**：
- DNS hijacker 是所有 DNS 查询的统一入口（Mode A/B 不再区分）
- writeLoop 拦截需要复杂的地址重写（src IP + checksum），且 Mode A/B 路径不同
- DNS hijacker 可以直接判断域名归属、做缓存、转发到远端 hijacker
- 简化数据流：查询 → hijacker → （本地分配 | 远端转发） → 响应
- ~~v0.6.0/v0.7.0 的 tryDNSRedirect 方案已废弃~~

### 4.3 ADR-3: MeshDial 统一抽象

**决策**：所有 server handler 共用 `MeshDial()` 函数，不各自实现拨号逻辑。

**理由**：
- 避免代码重复（6 个 handler 文件）
- 统一行为：无规则匹配、无 proxy chain、直接 mesh 路由
- 便于维护和修改

### 4.4 ADR-4: MeshDial 不使用 DirectDialer，DirectDialer 清理 netstack 路径

**决策**：
1. `MeshDial()` 直接调用 `GlobalDNSResolverFunc` 和 `GlobalNetstackDialFunc`，不经过 `DirectDialer`
2. `DirectDialer` 中的 netstack 路径（`GlobalNetstackDialFunc`/`GlobalDNSResolverFunc` 分支）删除，只保留 OS socket 路径

**理由**：
- `DirectDialer` 是 DIRECT 代理规则专用的拨号器，走 OS socket 直连目标
- netstack 路径是之前为 Mode B 加的，现在 `MeshDial` 直接调用全局函数，不再经过 `DirectDialer`
- 留着 netstack 路径会误导开发者以为 `DirectDialer` 有两条路径，实际上 DIRECT 规则只需要 OS socket
- 清理后 `DirectDialer.Dial()` 简化为只调用 `DialRouteAware`

## 5. 变更文件清单

| 文件 | 变更 | 状态 |
|------|------|------|
| `mesh/mesh.go` | VIP 路径简化 + 可配置 network + 多链路路由优化（路由表存所有 peer、按跳数排序、同跳数轮询） | ✓ v0.8 已完成 / v0.9 待实现 |
| `mesh/forward.go` | meshCIDR 可配置（SetMeshCIDR/GetMeshCIDR） | ✓ 已完成 |
| `mesh/state.go` | AllocateSubnet 支持可配置网络范围和子网前缀 | ✓ 已完成 |
| `mesh/topology.go` | GossipDomainSuffix 增加 Subnet 字段 | ✓ 已完成 |
| `mesh/domain_trie.go` | Insert/Lookup 增加 subnet 参数 | ✓ 已完成 |
| `tun/nat.go` | TranslateInboundWithSrc、RewriteSrcIP 方法 | ✓ 已完成 |
| `tun/dns.go` | DNS hijacker 跨节点转发（gvisor socket）+ 缓存 + TTL | ✓ 已完成 |
| `tun/engine.go` | 删除 tryDNSRedirect、localNodeDomain 提前、SetDNSDomainResolver | ✓ 已完成 |
| `p2p/p2p.go` | P2P 协议版本升级到 2 + 多连接共存 | ✓ 已完成 |
| `dialer/bind.go` | GetLocalIPForDial + Auto P2P 支持 + MeshDial | ✓ 已完成 |
| `dialer/direct.go` | 删除 netstack 路径，只保留 OS socket | ✓ 已完成 |
| `config/config.go` | Mesh.Network 字段 + GetNetwork() | ✓ 已完成 |
| `main.go` | mesh 初始化 + Auto P2P | ✓ 已完成 |
| `main_tun.go` | 删除 SetMeshGatewayResolver，新增 SetDNSDomainResolver | ✓ 已完成 |
| `server/socks5.go` | 删除规则匹配，改用 MeshDial | ✓ 已完成 |
| `server/trojan.go` | 同上 | ✓ 已完成 |
| `server/http.go` | 同上 | ✓ 已完成 |
| `server/htunnel.go` | 同上 | ✓ 已完成 |
| `server/direct.go` | 同上 | ✓ 已完成 |
| `server/reverse.go` | 同上 | ✓ 已完成 |

## 6. 风险与回退

| 风险 | 影响 | 缓解 |
|------|------|------|
| P2P 版本升级导致不兼容 | 高 | 需同时部署所有节点（QG、VM、JF） |
| DNS hijacker 转发增加延迟 | 中 | DNS 缓存减少跨节点查询 |
| DNS hijacker 转发失败 | 中 | 缓存 + 超时回退到本地解析 |
| 多 P2P 连接增加资源消耗 | 低 | 负载均衡提升可靠性，可接受 |

## 7. 验收标准

### 已完成项

- [x] 跨节点 DNS 解析正常（qg.phn → Fake-IP）
- [x] 跨节点 TCP 连接正常（SSH qg.phn:2222）
- [x] P2P 协议版本校验生效
- [x] Mesh 启用时自动建立 P2P 连接
- [x] 多 P2P 连接共存无驱逐循环
- [x] 地址空间可配置（/8 网络，/16 子网）

### 已完成项（统一 DNS hijacker 方案）

- [x] 删除 tryDNSRedirect 及相关代码
- [x] 删除 GIP 路径 DNS src 重写
- [x] P2P 协议版本升级到 2
- [x] GossipDomainSuffix 增加 Subnet 字段
- [x] DNS hijacker 支持跨节点转发（gvisor socket）
- [x] DNS hijacker 支持缓存（TTL 从响应中提取）
- [x] 跨节点 DNS 解析验证（VM→QG、VM→JF、QG→JF）
- [x] 跨节点 TCP 连接验证

### 待实现项（多链路路由优化）

- [ ] MeshRoute 改为存所有 peer + lastIdx
- [ ] recomputeRoutes 收集所有 peer 并按跳数排序
- [ ] findPeer → findRoute 返回完整路由
- [ ] HandleOutboundPacket 选路改为同跳数轮询
- [ ] 删除 findPeers 方法
- [ ] 路由重算时 lastIdx 重置为 0

### 待验证项

- [x] Mode A DNS（TUN 入口）跨节点解析正常（VM→QG: qg.phn→100.0.0.7 ✓）
- [x] Mode B DNS（SOCKS5 入口）跨节点解析正常（QG SOCKS5 → www.google.com→100.0.0.6 ✓）
- [x] DNS 缓存命中时不转发（QG 日志显示 cached ✓）
- [x] P2P 版本不匹配时拒绝连接（WIN7_VPN peer=1 local=2 被拒绝 ✓）
- [x] VM→JF 跨节点 DNS（test.jf.local→100.2.0.4 ✓）
- [x] 多跳 mesh 路由（VM→QG→JF ✓）
- [ ] 多链路轮询负载均衡（待 v0.9 实现后验证）

### 待实现项（自动生成 nodeID.phn）

- [x] 新增 defaultMeshSuffix 常量（已有 MeshDomainSuffix = "phn"）
- [x] recomputeRoutes 中从 claimedSubnets 自动生成 nodeID.phn 条目
- [x] 过滤裸 "phn" 后缀（recomputeRoutes + broadcastGossip）

### 待验证项（自动生成 nodeID.phn）

- [x] JF 查询 vm.phn → 100.1.0.29 (remote, forwarded to VM ✓)
- [x] JF 查询 qg.phn → 100.0.0.13 (remote, forwarded to QG ✓)
- [x] JF SOCKS5 通过 vm.phn 访问 VM 服务（SOCKS5 request granted, TCP via MESH ✓）
- [x] 用户自定义后缀（httpbin.org）仍正常工作（JF SOCKS5 → httpbin.org/get ✓）
