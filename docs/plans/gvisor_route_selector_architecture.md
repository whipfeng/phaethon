# gVisor 路由架构最终方案：RouteSelector + Link NICs

## 状态：✅ 最终方案（2026-10-05 修订：RouteSelector 4 分支逻辑 + 路由表完全清空 + conntrack 辅助回程）

**本文档整合并取代**：
- `gvisor_routing_evolution.md`（早期调研，已废弃）
- `multi_nic_architecture.md`（v1，已废弃）
- `multi_nic_architecture_v2.md`（手动 NAT 方案，已废弃）
- `gvisor_stack_integration_design.md`（部分结论被本文更新）

**核心变化**（相比 `gvisor_stack_integration_design.md`）：
1. **取消 Tunnel NIC**：IPIP 封装在路由决策时完成，不需要独立的隧道 NIC
2. **RouteSelector 扩展点**：在 FindRoute 过程中做动态路由决策
3. **Link NICs**：每个直连 mesh peer 一个 NIC（而非共享 mesh NIC）

---

## 1. 架构概览

### 1.1 NIC 规划

| NIC | 角色 | 绑定 | 混杂 | 说明 |
|-----|------|------|------|------|
| 1 | TUN | VIP(.1) / GIP(.3) | 是 | 栈↔宿主边界 + 本节点 mesh 服务地址 |
| 2 | Link-VM | 无 | 是 | 直连 VM 的链路（收发） |
| 3 | Link-GG | 无 | 是 | 直连 GG 的链路（收发） |
| 4 | Link-MS9 | 无 | 是 | 直连 MS9 的链路（收发） |
| 100+ | h_tunnel | 无 | 是 | 沿用现状 |

**关键变化**：
- ❌ **没有 Tunnel NIC**：IPIP 封装在转发路径完成
- ✅ **Link NICs**：每个直连 peer 一个 NIC，直接与该 peer 通信（发送 + 入站注入，见 §5.2）
- ✅ **VIP/EIP 不绑定**：只是 NAT/隧道身份地址，从子网算出（EIP 仅用于 IPIP 外层 src 与对端匹配解封装）
- ✅ **GIP 绑 NIC 1**：admin/DNS 监听地址按地址绑定，与 NIC 无关；meshEP 退役后 NIC 2 让位给 Link-VM
- ✅ **fakeIP 不绑定**：经 NIC 1 混杂模式 + Forwarder 拦截（传输层 demuxer）
- ✅ **Link NICs 混杂模式**：入站帧注入后需在 Link NIC 上完成本地交付判定（dst=GIP/VIP 等本地地址不绑在 Link NIC 上，混杂模式使 demuxer 放行）
- ✅ **meshEP 退役**：不再有共享 mesh NIC；入站帧由 HandleMeshFrame 按 fromNodeID 注入对应 Link NIC，IPIP 解封装保留在帧层

### 1.2 路由表

```
路由表: 空（所有路由由 RouteSelector 处理）
```

**注意**：
- **路由表完全清空**：所有路由决策（mesh、非 mesh、本地地址）都由 RouteSelector 处理（§2.3）。路由表不再包含任何条目。
- **Conntrack 管理的回程包例外**：旁路网关回程包（src=VIP, dst=客户端）通过 conntrack 记录的 `originalInputNIC` 直接路由回 NIC 1（patch #5/#6，`FindRouteViaNIC`），跳过 RouteSelector 和路由表。
- **Socket/Forwarder 回程包走 RouteSelector**：DNSHijacker 应答、forwarder 回复等栈内 socket 的回程包仍然走 RouteSelector，由 RouteSelector 根据目标地址返回正确的 EgressNIC。

---

## 2. RouteSelector 扩展点

### 2.1 设计目标

在 gVisor 的 `FindRoute` 过程中注入自定义路由决策逻辑，处理路由表无法表达的复杂场景。

### 2.2 接口定义

```go
// gVisor fork 新增
type RouteSelector func(dst tcpip.Address) RouteDecision

type RouteDecision struct {
    // 出口 Link NIC（所有非本地交付的情况）
    // 直连 mesh：直连 peer 的 Link NIC
    // 非直连 mesh：Dijkstra 计算的下一跳 Link NIC
    // 非 mesh：出口节点的下一跳 Link NIC
    EgressNIC tcpip.NICID
    
    // 是否需要 IPIP 封装（仅非 mesh 目标）
    NeedIPIP bool
    
    // IPIP 出口节点 EIP（隧道终点，从子网算出；与帧层解封装条件
    // "外层 dst == 本节点 EIP"（§2.4/§4.5）一致）
    // 仅当 NeedIPIP=true 时有效
    EgressEIP tcpip.Address
    
    // 是否可缓存（动态选路时设为 false）
    Cacheable bool
    
    // 本地交付（环回 forwarder）
    LocalDelivery bool
}
```

### 2.3 决策逻辑

```go
func RouteSelector(dst tcpip.Address) RouteDecision {
    // 1. 本地 mesh 网段（本节点子网）→ 本地交付
    //    （admin API、本地服务、fakeIP 等）
    if isLocalMeshSubnet(dst) {
        return RouteDecision{LocalDelivery: true, Cacheable: true}
    }
    
    // 2. 其他 mesh 目标（直连或非直连）→ 计算下一跳 Link NIC，无 IPIP 封装
    if isMeshSubnet(dst) {
        egressNodeID := selectEgressNode(dst)  // Dijkstra 选出口节点
        nextHopNIC := getNextHopLinkNIC(egressNodeID)  // 下一跳 Link NIC
        return RouteDecision{
            EgressNIC: nextHopNIC,
            Cacheable: false,  // 拓扑变化时重新计算
        }
    }
    
    // 3. 非 mesh → 匹配通告路由
    if egressNodeID, found := matchAdvertisedRoute(dst); found {
        nextHopNIC := getNextHopLinkNIC(egressNodeID)
        egressEIP := calculateEIP(getNodeSubnet(egressNodeID))
        return RouteDecision{
            EgressNIC: nextHopNIC,
            NeedIPIP: true,
            EgressEIP: egressEIP,
            Cacheable: true,
        }
    }
    
    // 4. 匹配不上 → 本地交付（触发 forwarder）
    return RouteDecision{LocalDelivery: true, Cacheable: true}
}
```

**关键变化**：
- **所有 FindRoute 都走 RouteSelector**：删除 `id == 0 && localAddr == ""` 限制。RouteSelector 处理所有路由决策（除了 conntrack 管理的回程包，见 patch #5/#6）。
- **本地 mesh 子网优先**：本节点子网（如 VM 的 100.1.0.0/16）直接 LocalDelivery，不走 Link NIC。fakeIP 在本节点子网范围内，由情况 1 覆盖。
- **统一 EgressNIC 语义**：RouteSelector 返回的 EgressNIC 是下一跳的 Link NIC（直连 peer），gVisor 直接使用该 NIC 发送包。
- **非 mesh 一次性算好**：RouteSelector 同时返回 EgressNIC（下一跳）和 EgressEIP（IPIP 外层目标），无需递归查找。
- **兜底 LocalDelivery**：匹配不上通告路由的目标交给 forwarder 处理。只有匹配到通告路由的非 mesh 目标才走 IPIP 封装。
- **路由表完全清空**：所有路由由 RouteSelector 处理，路由表不再包含任何条目。

**例外**：
- **Conntrack 管理的回程包**：DNAT 反转通过 `pkt.OutputNICName` 直接路由（patch #5/#6，`FindRouteViaNIC`），跳过 RouteSelector 和路由表。

### 2.4 VIP 和 EIP 的计算

**关键结论**：VIP 和 EIP 都从子网算出，不需要映射表。

```go
// 每个节点的子网已知（如 GG: 100.179.0.0/16）
// VIP 和 EIP 都是子网内的固定偏移

func calculateVIP(subnet net.IPNet) net.IP {
    // VIP = subnet + 1 (如 100.179.0.1)
    return incrementIP(subnet.IP, 1)
}

func calculateEIP(subnet net.IPNet) net.IP {
    // EIP = subnet + 4 (如 100.179.0.4)
    return incrementIP(subnet.IP, 4)
}
```

**RouteSelector 使用**：
```go
egressNodeSubnet := getEgressNodeSubnet(egressNodeID)
egressEIP := calculateEIP(egressNodeSubnet)  // 外层封装目标（隧道终点）
```

### 2.5 路由缓存兼容性

**可缓存场景**：
- fakeIP 范围固定 → 可缓存
- 直连 peer 子网固定 → 可缓存
- IPIP 出口节点确定（如静态配置）→ 可缓存

**不可缓存场景**：
- IPIP 出口节点动态选择（基于延迟/负载）→ 标记 `Cacheable: false`
- 每次 FindRoute 都调用 RouteSelector

**缓存策略**：
```go
if route.Cacheable {
    // 存入 gVisor 路由缓存
    cache.Set(dst, route)
} else {
    // 不缓存，每次重新决策
}
```

### 2.6 SNAT 配置（Input + Postrouting）

**问题**：NIC 1 入站的包不仅需要 SNAT（转发路径），本地交付路径（访问 DNS hijacker、forwarder 等）也需要 SNAT。否则本地服务看到的源地址是客户端 IP（192.168.x），回复时无法正确路由回 NIC 1。

**方案**：在 Input hook 和 Postrouting hook 都配置 SNAT 规则，匹配 `InputInterface="tun"`（NIC 1）。

**gVisor iptables 支持**：
- ✅ **Input hook 支持 SNAT**（`SNATTarget.Action` 支持 `Postrouting, Input`）
- ✅ **Input hook 支持 InputInterface 匹配**（可以匹配 `inNicName`）
- ✅ **Output hook 支持 DNAT**（`DNATTarget.Action` 支持 `Prerouting, Output`）

**SNAT 规则**：
```
# 转发路径 SNAT（已实现）
-t NAT -A POSTROUTING -i tun -j SNAT --to-source <VIP>

# 本地交付路径 SNAT（需添加）
-t NAT -A INPUT -i tun -j SNAT --to-source <VIP>
```

**Conntrack NIC 记录/恢复**（patch #5/#6）：
1. **Input hook SNAT**：conntrack 记录 `originalInputNIC = "tun"`
2. **Output hook DNAT**（回复包）：conntrack 恢复 `pkt.OutputNICName = "tun"`
3. **forwardUnicastPacket**：检查 `pkt.OutputNICName`，使用 `FindRouteViaNIC` 直接路由回 NIC 1，跳过 RouteSelector 和路由表

**流程**：
```
入站（NIC 1 → forwarder）:
  src=192.168.1.100, dst=fakeIP, inNIC=tun
    ↓
  Input hook SNAT: src → VIP, conntrack 记录 originalInputNIC=tun
    ↓
  RouteSelector: dst=fakeIP → LocalDelivery
    ↓
  Forwarder 收到: src=VIP, dst=fakeIP

出站（forwarder → NIC 1）:
  Forwarder 发送回复: src=VIP, dst=VIP (peer 地址)
    ↓
  Output hook: conntrack 反转 SNAT → dst=192.168.1.100, pkt.OutputNICName=tun
    ↓
  forwardUnicastPacket: 检查 pkt.OutputNICName → FindRouteViaNIC(tun, dst)
    ↓
  包从 NIC 1 发出 → 客户端收到回复
```

---

## 3. IPIP 封装路径（无 Tunnel NIC）

### 3.1 封装流程

```
[FindRoute(8.8.8.8)]
    ↓
[RouteSelector 决策]
    - 非 mesh 目标
    - selectEgressNode(8.8.8.8) = GG
    - getNextHopLinkNIC(GG) = Link-GG
    - EgressNIC = Link-GG
    - NeedIPIP = true
    - EgressEIP = 100.179.0.4 (GG EIP)
    ↓
[返回 Route]
    Route{
        NIC: Link-GG,  // RouteSelector 直接指定
        NeedIPIP: true,
        EgressEIP: 100.179.0.4,
    }
    ↓
[转发代码 ipv4.forwardUnicastPacket]
    看到 NeedIPIP=true
    ↓
[IPIP 封装]
    内层: src=QG_VIP, dst=8.8.8.8
    外层: src=QG_EIP, dst=100.179.0.4, proto=4
    ↓
[Link-GG 发送] → GG 节点（无需二次路由，EgressNIC 已指定）
```

### 3.2 关键优势

- ✅ **少一个 NIC**：不需要 Tunnel NIC
- ✅ **封装逻辑靠近决策点**：更内聚
- ✅ **符合 Linux 风格**：隧道是路由的一部分，不是独立 NIC
- ✅ **简化架构**：NIC 数量可控

### 3.3 与 Tunnel NIC 方案的对比

| 方案 | NIC 数量 | 封装位置 | 复杂度 |
|------|---------|---------|--------|
| Tunnel NIC | 5 个 | Tunnel NIC 读取 Gateway | 中 |
| **路由时封装（本文）** | 4 个 | forwardUnicastPacket 内 | **低** |

---

## 4. 完整流量路径（具体 IP 示例）

### 4.1 旁路网关 → 外网域名

```
LAN(192.168.1.100) → www.google.com (fakeIP=100.1.0.5)

[NIC1 TUN 进入 gVisor]
  src=192.168.1.100, dst=100.1.0.5, inNIC=tun
    ↓
[Input hook SNAT]
  src: 192.168.1.100 → 100.1.0.1 (QG VIP)
  conntrack: (192.168.1.100, 100.1.0.5, inNIC=tun) ↔ (100.1.0.1, 100.1.0.5)
    ↓
[handleValidatedPacket — patch #2b]
  RouteSelectorLocalDelivery(100.1.0.5)?
  → RouteSelector 情况 1: isLocalMeshSubnet(100.1.0.5) = true（fakeIP 在本节点子网内）
  → LocalDelivery = true → 跳过转发，走本地交付
    ↓
[本地交付] → TCP/UDP Forwarder
  Forwarder 收到: src=VIP(100.1.0.1), dst=fakeIP(100.1.0.5)
    ↓
[域名还原: 100.1.0.5 → www.google.com]
    ↓
[代理拨号器] → mesh/internet 出口 → www.google.com
```

### 4.2 旁路网关 → 外网 raw IP

```
LAN(192.168.1.100) → 8.8.8.8

[NIC1 TUN 进入]
  src=192.168.1.100, dst=8.8.8.8, inNIC=tun
    ↓
[Input hook SNAT]
  src: 192.168.1.100 → 100.1.0.1 (QG VIP)
  conntrack: 记录 originalInputNIC=tun
    ↓
[handleValidatedPacket — patch #2b]
  RouteSelectorLocalDelivery(8.8.8.8)?
  → RouteSelector 判定: 非本地 mesh → 非 mesh → matchAdvertisedRoute(8.8.8.8)
  → 匹配到出口节点通告路由 → LocalDelivery=false
    ↓
[FindRoute(8.8.8.8)]
    ↓
[RouteSelector — 情况 3: 匹配通告路由]
  - matchAdvertisedRoute(8.8.8.8) → 匹配到通告路由
  - getNextHopLinkNIC(GG) = Link-GG
  - EgressNIC = Link-GG
  - NeedIPIP = true
  - EgressEIP = 100.179.0.4 (GG EIP)
    ↓
[FindRoute 构造路由]
  NIC = Link-GG, needIPIP = true, egressEIP = 100.179.0.4
    ↓
[forwardUnicastPacket 看到 NeedIPIP=true]
    ↓
[IPIP 封装]
  内层: src=100.1.0.1 (QG VIP), dst=8.8.8.8
  外层: src=100.1.0.4 (QG EIP), dst=100.179.0.4 (GG EIP), proto=4
    ↓
[Link-GG 发送] → GG 节点（无需二次路由，EgressNIC 已指定）
    ↓
[GG 解封装: 外层 dst == 本节点 EIP（§2.4）] → 内层包 → 代理拨号 → 8.8.8.8
```

### 4.3 本地应用 → mesh VIP（直连）

```
QG 本地应用 → VM (100.2.0.1, 直连)

[NIC1 TUN 进入]
  src=100.1.0.1, dst=100.2.0.1
    ↓
[FindRoute(100.2.0.1)]
    ↓
[RouteSelector — 情况 2: mesh 目标]
  - isMeshSubnet(100.2.0.1) = true
  - selectEgressNode(100.2.0.1) = VM
  - getNextHopLinkNIC(VM) = Link-VM（直连 peer）
  - EgressNIC = Link-VM
  - NeedIPIP = false
    ↓
[FindRoute 构造路由]
  NIC = Link-VM
    ↓
[Link-VM 发送] → VM 节点（直连 P2P，无 IPIP）
```

### 4.4 本地应用 → mesh VIP（非直连）

```
QG 本地应用 → MS9 (100.189.0.1, 非直连，经 GG 中继)

[NIC1 TUN 进入]
  src=100.1.0.1, dst=100.189.0.1
    ↓
[FindRoute(100.189.0.1)]
    ↓
[RouteSelector — 情况 2: mesh 目标]
  isMeshSubnet(100.189.0.1) = true
  selectEgressNode(100.189.0.1) = MS9
  getNextHopLinkNIC(MS9) = Link-GG（Dijkstra 下一跳）
    ↓
[返回 RouteDecision]
  EgressNIC = Link-GG
  NeedIPIP = false
    ↓
[FindRoute 构造路由]
  NIC = Link-GG, dst = 100.189.0.1
    ↓
[QG → GG → MS9]（mesh 内部中继，无 IPIP）
```

**关键**：非直连 mesh 节点由 RouteSelector 计算下一跳 Link NIC（Link-GG），无 IPIP 封装，mesh 内部中继。

### 4.5 回程流量（旁路网关）

```
8.8.8.8 → LAN(192.168.1.100)

[GG 响应]
  src=8.8.8.8, dst=100.1.0.1 (QG VIP)
    ↓
[mesh P2P] → QG Link-GG 进入
    ↓
[Prerouting DNAT] (conntrack)
  查询 conntrack by dst=VIP → 找到 originalInputNIC=tun
  pkt.OutputNICName = "tun"  ← patch #6
  dst: 100.1.0.1 → 192.168.1.100
    ↓
[forwardUnicastPacket]
  检查 pkt.OutputNICName == "tun" → FindRouteViaNIC(NIC1, 192.168.1.100)
  跳过 RouteSelector + 路由表，直连路由经 NIC 1
    ↓
[NIC1 发送] → TUN → OS → LAN
```

**关键**：
1. **入站注入**：HandleMeshFrame 收到外层帧 → 帧层解封装（外层 dst == 本节点 EIP）→ 内层包按 fromNodeID 注入 Link-GG → netstack 正常走 Prerouting（conntrack 得到 InputInterface=Link-GG）
2. **DNAT + conntrack 辅助路由**：DNAT 时从 conntrack 取出 `originalInputNIC=tun`，存入 `pkt.OutputNICName`。`forwardUnicastPacket` 检测到后调用 `FindRouteViaNIC`，跳过 RouteSelector 和路由表，直接构造经 NIC 1 的直连路由。无需在路由表中配置 LAN 网段条目。

---

## 5. Link NICs 设计

### 5.1 设计原则

**每个直连 mesh peer 一个 NIC**，而非共享 mesh NIC。

**优势**：
- ✅ 每个 NIC 直接和链路通信，无需外部判断逻辑
- ✅ RouteSelector 明确：子网 → Link NIC（EgressNIC）
- ✅ 符合"路由内化"原则

### 5.2 Link NIC 的职责

Link NIC 是"哑"的，职责是收发直连：
- 不做路由决策（路由在 FindRoute 完成）
- 不做 IPIP 封装/解封装（封装在转发路径、解封装在帧层完成）
- 发送：链路层发送交给 mesh hop 表选路（非直连目标经中继时同样由 hop 表决定下一跳）
- 接收：HandleMeshFrame 收到帧后按 fromNodeID 找到对应 Link NIC 调用 InjectInbound；
  conntrack 依赖该入口标识记录 OriginalInputNIC（补丁 #5），DNAT 回程靠它选出口 NIC（补丁 #6）

### 5.3 与共享 mesh NIC 的对比

| 方案 | NIC 数量 | 路由决策位置 | 复杂度 |
|------|---------|------------|--------|
| 共享 mesh NIC | 2 个 | 外部判断逻辑 | 高 |
| **Link NICs（本文）** | N+1 个 | RouteSelector | **低** |

---

## 6. gVisor Fork 补丁

### 6.1 补丁 #1：FindRoute 最长前缀匹配

**文件**：`pkg/tcpip/stack/stack.go`
**函数**：`FindRoute`

**问题**：直配路由抢占，不看后续更长前缀。

**修复**：跨 NIC 收集最长前缀再决出（约 100 行）。

### 6.2 补丁 #2：转发优先语义

**文件**：`pkg/tcpip/network/ipv4/ipv4.go`
**函数**：`handleValidatedPacket`

**问题**：本地交付优先于转发。

**修复**：交换顺序——先尝试转发，失败再本地交付（约 30 行）。

### 6.3 补丁 #3：RouteSelector 扩展点（新增）

**文件**：`pkg/tcpip/stack/stack.go`
**函数**：`FindRoute`

**新增**：
```go
// 在 FindRoute 中调用 RouteSelector（所有 FindRoute 调用都走，§6.8 已废弃条件限制）
if s.routeSelector != nil {
    decision := s.routeSelector(remoteAddr)
    if decision.LocalDelivery {
        return makeLocalRoute(...)
    }
    if decision.NeedIPIP {
        return makeIPIPRoute(decision.EgressNIC, decision.EgressEIP)
    }
    // 有 EgressNIC 但无 NeedIPIP（mesh 目标）→ 经该 Link NIC 的直连路由
    if decision.EgressNIC != 0 {
        return makeRouteViaNIC(decision.EgressNIC, ...)
    }
}
```

### 6.4 补丁 #4：Postrouting 接口匹配（新增）

**文件**：`pkg/tcpip/stack/iptables_types.go:320-321`

**问题**：Postrouting hook 不支持 InputInterface 匹配，直接 `return true`。

**修复**：
```go
func (r *Rule) checkPostrouting(pkt *Packet, ...) bool {
    if r.InputInterface != "" {
        return pkt.InputInterfaceName == r.InputInterface
    }
    return true
}
```

**用途**：SNAT 只对从 NIC1 进入的旁路网关流量生效。

### 6.5 补丁 #5：Conntrack 记录输入接口（新增）

**文件**：`pkg/tcpip/stack/conntrack.go`

**新增**：
```go
type conntrackEntry struct {
    // ... 现有字段
    OriginalInputNIC tcpip.NICID  // 原始输入接口
}

// 创建 conntrack 条目时记录
func (ct *conntrack) createEntry(pkt *PacketBuffer, ...) *conntrackEntry {
    entry := &conntrackEntry{
        OriginalInputNIC: pkt.NICID,  // 记录包从哪个 NIC 进入
    }
    return entry
}
```

### 6.6 补丁 #6：DNAT 辅助路由（新增）

**文件**：`pkg/tcpip/network/ipv4/ipv4.go` + `pkg/tcpip/stack/stack.go`

**问题**：DNAT 回程包的目标是任意 LAN IP，路由表无法覆盖所有可能。

**解决**：DNAT 时查询 conntrack 获取原始输入接口，存入包元数据，FindRoute 使用该接口路由。

**实现**：
```go
// ipv4.go Prerouting DNAT 后
if pkt.NATType == DNAT {
    if entry := stack.conntrack.LookupByDestination(pkt.DstAddr); entry != nil {
        pkt.OutputNIC = entry.OriginalInputNIC  // 存入包元数据
    }
}

// stack.go FindRoute
func (s *Stack) FindRoute(...) (*Route, error) {
    // 优先使用 DNAT 设置的输出接口
    if pkt.OutputNIC != 0 {
        return makeRoute(pkt.OutputNIC, ...)
    }
    // 正常路由逻辑
    // ...
}
```

**优势**：支持任意源 IP 回程，无需配置 LAN 网段路由。

### 6.7 补丁 #2b：本地交付优先级（新增）

**文件**：`pkg/tcpip/network/ipv4/ipv4.go`（+ ipv6.go）
**函数**：`handleValidatedPacket`

**问题**：补丁 #2 的转发优先是排他的——转发开启时本地交付永不发生。但 RouteSelector 标记 `LocalDelivery` 的目标（fakeIP、GIP、本地 VIP）必须在 IP 层本地交付（经传输层 demuxer 到达 Forwarder/GIP 监听），否则 fakeIP 包会被转发路径送回 TUN 形成自环。

**修复**：转发前先查 RouteSelector；`LocalDelivery=true` 的目标跳过转发、走本地交付：

```go
if e.Forwarding() && !e.protocol.stack.RouteSelectorLocalDelivery(dstAddr) {
    e.handleForwardingError(e.forwardUnicastPacket(pkt))
    return
}
// 本地交付（AcquireAssignedAddress + deliverPacketLocally，原有路径）
```

`RouteSelectorLocalDelivery(dst)` 与 FindRoute 共用 §2.5 的决策缓存。

### 6.8 补丁 #3 修订：RouteSelector 生效范围（已废弃）

**原问题**：补丁 #3 在所有 FindRoute 调用前执行，包括传输层 accept 路径的建连路由（`tcp/accept.go: FindRoute(inNIC, pktDst, pktSrc)`）。forwarder 回程的 remote 是 TUN 客户端（非 mesh 目标），会被误判为 NeedIPIP，SYN-ACK 被错误封装发往 mesh。

**解决方案**：删除条件限制，所有 FindRoute 都走 RouteSelector。原问题通过以下机制解决：
1. **所有 NIC1 入站包都被 SNAT**：Input hook SNAT 将 src 改为 VIP，本地服务回包目标是 VIP（不是 LAN 客户端）
2. **Output hook DNAT 还原**：conntrack 反转 SNAT，将 dst 还原为 LAN 客户端，设置 `pkt.OutputNICName`
3. **FindRouteViaNIC 跳过 RouteSelector**：`forwardUnicastPacket` 检测到 `pkt.OutputNICName` 后调用 `FindRouteViaNIC`，直接构造经 NIC1 的直连路由，不经过 RouteSelector

因此 accept/RST 路径的回程包根本不会走到 RouteSelector，无需条件限制。

### 6.9 补丁 #6 完成：FindRouteViaNIC（新增）

**问题**：DNAT 回程（dst=LAN 客户端）走 `forwardUnicastPacket → FindRoute(OutputNIC, "", dst)`，但路由表没有 LAN 网段（设计 §8.4 明确不配），路由表+本地路由都失配 → 回程被丢弃。

**修复**：forwardUnicastPacket 在 `pkt.OutputNICName != ""` 时改调 `stack.FindRouteViaNIC(nicID, remoteAddr)`：跳过 RouteSelector、跳过路由表，直接构造经该 NIC 的直连路由（gateway 为空，与 FindRoute 早退分支同构）。这使 conntrack 辅助路由真正闭环：去程记录 OriginalInputNIC → 回程 DNAT 时恢复输出 NIC → 直连路由出栈。

---

## 7. 实施计划

### 阶段 1：已完成（零 fork）

- ✅ 2-NIC 拓扑（TUN + mesh）
- ✅ iptables SNAT
- ✅ 删除手动 NAT（nat.go）

### 阶段 2：Fork 定制（当前）

**Fork 补丁**：
1. ✅ 补丁 #1：FindRoute 最长前缀匹配
2. ✅ 补丁 #2：转发优先语义
3. ⏳ 补丁 #3：RouteSelector 扩展点
4. ⏳ 补丁 #4：Postrouting InputInterface 匹配
5. ⏳ 补丁 #5：Conntrack 记录输入接口
6. ⏳ 补丁 #6：DNAT 辅助路由

**架构实现**：
1. ⏳ Link NICs：每个直连 peer 一个 NIC
2. ⏳ IPIP 封装内化：在转发路径完成
3. ⏳ NAT 规则配置：SNAT + DNAT + conntrack
4. ⏳ 拦截器退役

### 阶段 3：验证

- [ ] 旁路网关 NAT（域名、raw IP）
- [ ] mesh 路由（直连、非直连）
- [ ] IPIP 封装/解封装
- [ ] DNAT 回程（任意源 IP）
- [ ] 长连接 TCP（无 RST 循环）
- [ ] UDP/QUIC

---

## 8. NAT 完整解决方案

### 8.1 核心问题

**旁路网关 NAT**：
- 去程：LAN IP → SNAT → VIP → mesh → 外网
- 回程：外网 → VIP → DNAT → LAN IP → 回程路由

**难点**：
1. SNAT 只对从 NIC1 进入的流量生效（不能 SNAT 所有流量）
2. DNAT 回程包目标是任意 LAN IP，路由表无法覆盖

### 8.2 解决方案

**补丁 #4**：Postrouting 支持 InputInterface 匹配
- SNAT 规则：`InputInterface=NIC1` → 只对从 NIC1 进入的流量 SNAT

**补丁 #5 + #6**：Conntrack 辅助路由
- SNAT 时记录 OriginalInputNIC 到 conntrack
- DNAT 时查询 conntrack，取出 OriginalInputNIC 存入包
- FindRoute 使用包的 OutputNIC 路由

### 8.3 完整流量路径

#### 去程（旁路网关 → 外网）

```
LAN(192.168.1.100) → 8.8.8.8

[NIC1 TUN 进入] pkt.NICID = 1
  src=192.168.1.100, dst=8.8.8.8
    ↓
[Conntrack 创建条目]
  entry.OriginalInputNIC = 1  ← 补丁 #5
    ↓
[Prerouting] 无 DNAT（新连接）
    ↓
[路由] dst=8.8.8.8 → RouteSelector → IPIP 封装
    ↓
[Postrouting]
  InputInterface = NIC1 ✓  ← 补丁 #4
  SNAT: src → VIP (100.1.0.1)
    ↓
[发送] src=VIP, dst=8.8.8.8 (IPIP 内层)
```

#### 回程（外网 → 旁路网关）

```
8.8.8.8 → LAN(192.168.1.100)

[Link NIC 进入 (如 NIC3 Link-GG)]
  src=8.8.8.8, dst=VIP (100.1.0.1)
    ↓
[Prerouting DNAT] (conntrack)
  查询 conntrack by dst=VIP
  找到 entry.OriginalInputNIC = 1
  pkt.OutputNIC = 1  ← 补丁 #6
  DNAT: dst → 192.168.1.100
    ↓
[FindRoute(192.168.1.100)]
  pkt.OutputNIC = 1 ✓  ← 补丁 #6
  返回路由: NIC1
    ↓
[Postrouting]
  InputInterface = NIC3 (Link-GG) ✗
  不 SNAT ✅
    ↓
[NIC1 发送] src=8.8.8.8, dst=192.168.1.100 → LAN
```

**关键**：
- 去程：InputInterface=NIC1 → SNAT
- 回程：InputInterface=Link NIC（非 NIC1）→ 不 SNAT
- 回程：DNAT 时从 conntrack 取出 OriginalInputNIC=1 → 路由到 NIC1

### 8.4 NAT 规则配置

```go
// mesh/netstack.go initStack

// 1. Prerouting DNAT（conntrack 自动处理）
// 无需显式规则，conntrack 自动建立映射

// 2. Postrouting SNAT（补丁 #4）
natTable.AddRule(iptables.Rule{
    InputInterface: "NIC1",  // 只对从 NIC1 进入的流量
    Target: &iptables.SNATTarget{
        Addresses: []tcpip.Address{vipAddr},
    },
})

// 3. Input SNAT（本地交付路径）
natTable.AddRule(iptables.Rule{
    InputInterface: "NIC1",
    Target: &iptables.SNATTarget{
        Addresses: []tcpip.Address{vipAddr},
    },
    Hook: iptables.Input,
})

// 4. 路由表（与 §1.2 一致：完全清空）
// 所有路由由 RouteSelector 处理，路由表不包含任何条目。
// Conntrack 管理的回程包通过 FindRouteViaNIC 绕过路由表。
```

### 8.5 优势

- ✅ **支持任意源 IP**：不需要配置 LAN 网段路由
- ✅ **精确 SNAT**：只对从 NIC1 进入的流量 SNAT
- ✅ **自动回程**：conntrack 辅助路由，零配置
- ✅ **符合 Linux 语义**：InputInterface 匹配、conntrack 状态跟踪

---

## 9. 关键讨论结论

| 问题 | 结论 |
|------|------|
| 需要 Tunnel NIC 吗？ | **不需要**，IPIP 封装在路由决策时完成 |
| RouteSelector 在哪调用？ | FindRoute 过程中，路由表查询前/后 |
| VIP/EIP 怎么获取？ | 从子网算出，不需要映射表 |
| 非直连 mesh 节点需要 IPIP 吗？ | **不需要**，走 mesh 中继 |
| 路由缓存会被破坏吗？ | 不会，RouteSelector 可标记 `Cacheable` |
| 每个直连 peer 一个 NIC？ | **是**，Link NICs 设计 |
| 封装后外层包怎么路由？ | RouteSelector 直接指定 EgressNIC，无需二次 FindRoute |
| SNAT 怎么只针对旁路网关？ | **补丁 #4**：Postrouting InputInterface 匹配 |
| DNAT 回程怎么路由？ | **补丁 #5+#6**：Conntrack 辅助路由，支持任意源 IP |
| 需要配置 LAN 网段路由吗？ | **不需要**，conntrack 自动处理 |

---

## 10. 参考文档

- `gvisor_stack_integration_design.md` —— 前期调研（部分结论被本文更新）
- `mesh_ipip_smart_routing.md` —— IPIP 出口节点选择算法
- `traffic_flow_analysis.md` —— 流量路径分析（本文整合了其结论）
