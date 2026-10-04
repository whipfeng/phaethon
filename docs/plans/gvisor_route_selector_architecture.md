# gVisor 路由架构最终方案：RouteSelector + Link NICs

## 状态：✅ 最终方案（2026-10-04 讨论定稿）

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
路由表:
  100.1.0.1/32      → NIC 1 (VIP，本地交付)
  100.1.0.0/16      → NIC 1 (本节点子网，TUN 宿主回程)
  192.168.0.0/16    → NIC 1 (宿主网段，旁路网关客户端回程)
  100.2.0.0/16      → Link-VM (VM 子网，直连)
  100.179.0.0/16    → Link-GG (GG 子网，直连)
  100.189.0.0/16    → Link-MS9 (MS9 子网，直连)
  <通告路由前缀>    → RouteSelector 决策（见下文）
```

**注意**：
- **本节点子网入表（修订）**：原"不入表（避免 fakeIP 被转发）"的顾虑由补丁 #2b 消除——handleValidatedPacket 在转发前先查 `RouteSelectorLocalDelivery`，fakeIP/本地地址在转发路径消费路由表之前已被本地交付。入表必要性：TUN 宿主（src=hostIP .2）与 forwarder accept 路径需要到本节点子网地址的**普通出栈路由**（findLocalRoute 返回 PacketLoop 环回路由，不能用于 forwarder 回程）。
- **宿主网段入表**：旁路网关客户端（src=192.168.x）与 DNS 应答的回程路由。补丁 #6（FindRouteViaNIC）只覆盖 conntrack DNAT 回程；栈内 socket 应答（DNSHijacker `Write(To:)`，FindRoute(1, GIP, client)）必须依赖路由表。
- 默认路由不存在（由 RouteSelector 兜底）。

---

## 2. RouteSelector 扩展点

### 2.1 设计目标

在 gVisor 的 `FindRoute` 过程中注入自定义路由决策逻辑，处理路由表无法表达的复杂场景。

### 2.2 接口定义

```go
// gVisor fork 新增
type RouteSelector func(dst tcpip.Address) RouteDecision

type RouteDecision struct {
    // 是否需要 IPIP 封装
    NeedIPIP bool
    
    // IPIP 出口节点 VIP（从子网算出）
    EgressVIP tcpip.Address
    
    // 是否可缓存（动态选路时设为 false）
    Cacheable bool
    
    // 本地交付（环回 forwarder）
    LocalDelivery bool
}
```

### 2.3 决策逻辑

```go
func RouteSelector(dst tcpip.Address) RouteDecision {
    // 1. fakeIP / 本地 GIP / 本地 VIP → 本地交付
    //    （GIP=admin/DNS 监听；VIP=本节点服务地址；conntrack 回程已在
    //     Prerouting DNAT 改写 dst，不会走到这里）
    if isFakeIP(dst) || isLocalGIP(dst) || isLocalVIP(dst) {
        return RouteDecision{LocalDelivery: true, Cacheable: true}
    }
    
    // 2. mesh 网段 → 查路由表（直连 peer 有明确路由）
    if isMeshSubnet(dst) {
        // 路由表会处理，RouteSelector 不介入
        return RouteDecision{}  // 空决策，继续普通路由
    }
    
    // 3. 非 mesh 目标 → IPIP 封装
    egressVIP := selectEgressNode(dst)  // 选择出口节点
    return RouteDecision{
        NeedIPIP: true,
        EgressVIP: egressVIP,  // 从子网算出，不是查表
        Cacheable: true,       // 如果选路是确定性的
    }
}
```

**生效范围（补丁 #3 修订）**：RouteSelector 只在 `id == 0 && localAddr == ""` 的 FindRoute 调用中生效——即仅 IP 转发路径（forwardUnicastPacket：无接收 NIC、无本地地址上下文）。其余调用全部跳过：

- TCP accept/RST：`FindRoute(inNIC, pktDst, pktSrc)`，id≠0 且 localAddr≠""。若不跳过，forwarder 回程（dst=LAN 客户端）会被误判 NeedIPIP，SYN-ACK 被错误封装发往 mesh。
- UDP Forwarder Connect：`FindRoute(inNIC, "", client)`，id≠0 但 localAddr==""（endpoint 尚未 bind）。若不跳过，DNS/UDP 应答路由会被 selector 按 advertise 前缀误判（如 LAN 段被通告时回程被封装）。应答必须经接收 NIC 直出。
- 栈内 socket 单播应答（如 DNSHijacker 未连接 socket 的 `Write(To:)`）：`FindRoute(0, boundGIP, client)`，localAddr≠""。这类流量走路由表（宿主网段/本节点子网 → NIC 1）。
- 栈内跨 NIC 拨号（forwardToRemote：`FindRoute(1, GIP, remoteGIP)`）：id≠0，走路由表 + chosenRoute 兜底（本地地址在 NIC 1、出口为 Link NIC）。

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
egressVIP := calculateVIP(egressNodeSubnet)  // 外层封装目标
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

---

## 3. IPIP 封装路径（无 Tunnel NIC）

### 3.1 封装流程

```
[FindRoute(8.8.8.8)]
    ↓
[RouteSelector 决策]
    - 非 mesh 目标
    - NeedIPIP = true
    - EgressVIP = 100.179.0.1 (GG)
    ↓
[返回 Route]
    Route{
        NIC: ?,  // 还不知道
        Gateway: 100.179.0.1,  // 外层目标
        NeedIPIP: true,
    }
    ↓
[转发代码 ipv4.forwardUnicastPacket]
    看到 NeedIPIP=true
    ↓
[IPIP 封装]
    内层: src=QG, dst=8.8.8.8
    外层: src=QG_EIP, dst=100.179.0.1, proto=4
    ↓
[FindRoute(100.179.0.1)] ← 外层包重新路由
    ↓
[路由表匹配]
    100.179.0.0/16 → NIC 3 (Link-GG)
    ↓
[NIC 3 发送] → GG 节点
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
  src=192.168.1.100, dst=100.1.0.5
    ↓
[Prerouting SNAT]
  src: 192.168.1.100 → 100.1.0.1 (QG VIP)
  conntrack: (192.168.1.100, 100.1.0.5) ↔ (100.1.0.1, 100.1.0.5)
    ↓
[FindRoute(100.1.0.5)]
    ↓
[RouteSelector]
  100.1.0.5 是 fakeIP → LocalDelivery=true
    ↓
[本地交付] → TCP/UDP Forwarder
    ↓
[域名还原: 100.1.0.5 → www.google.com]
    ↓
[代理拨号器] → mesh/internet 出口 → www.google.com
```

### 4.2 旁路网关 → 外网 raw IP

```
LAN(192.168.1.100) → 8.8.8.8

[NIC1 TUN 进入]
  src=192.168.1.100, dst=8.8.8.8
    ↓
[Prerouting SNAT]
  src → 100.1.0.1 (QG VIP)
    ↓
[FindRoute(8.8.8.8)]
    ↓
[RouteSelector]
  - 非 mesh 目标
  - NeedIPIP=true
  - EgressVIP=100.179.0.1 (GG)
    ↓
[转发代码看到 NeedIPIP=true]
    ↓
[IPIP 封装]
  内层: src=100.1.0.1, dst=8.8.8.8
  外层: src=100.1.0.4 (QG EIP), dst=100.179.0.1 (GG VIP), proto=4
    ↓
[FindRoute(100.179.0.1)]
  匹配: 100.179.0.0/16 → NIC 3 (Link-GG)
    ↓
[NIC3 发送] → GG 节点
    ↓
[GG 解封装] → 8.8.8.8
```

### 4.3 本地应用 → mesh VIP（直连）

```
QG 本地应用 → VM (100.2.0.1, 直连)

[NIC1 TUN 进入]
  src=100.1.0.1, dst=100.2.0.1
    ↓
[FindRoute(100.2.0.1)]
  匹配: 100.2.0.0/16 → NIC 2 (Link-VM)
    ↓
[RouteSelector]
  mesh 网段，不介入（空决策）
    ↓
[NIC2 发送] → VM 节点（直连 P2P）
```

### 4.4 本地应用 → mesh VIP（非直连）

```
QG 本地应用 → MS9 (100.189.0.1, 非直连)

[NIC1 TUN 进入]
  src=100.1.0.1, dst=100.189.0.1
    ↓
[FindRoute(100.189.0.1)]
  匹配: 100.189.0.0/16 → NIC 4 (Link-MS9)
    ↓
[但 MS9 非直连！]
    ↓
[mesh 路由层处理]
  Link-MS9 NIC → mesh hop 表 → 选择中继路径
    ↓
[QG → GG → MS9]（mesh 内部中继，无 IPIP）
```

**关键**：非直连 mesh 节点走 mesh 中继，不需要 IPIP 封装。

### 4.5 回程流量（旁路网关）

```
8.8.8.8 → LAN(192.168.1.100)

[GG 响应]
  src=8.8.8.8, dst=100.1.0.1 (QG VIP)
    ↓
[mesh P2P] → QG NIC3 (Link-GG) 进入
    ↓
[Prerouting DNAT] (conntrack)
  dst: 100.1.0.1 → 192.168.1.100
    ↓
[FindRoute(192.168.1.100)]
  匹配: 192.168.0.0/16 → NIC 1 (TUN)
    ↓
[NIC1 发送] → TUN → OS → LAN
```

**关键**：
1. **入站注入**：HandleMeshFrame 收到外层帧 → 帧层解封装（外层 dst == 本节点 EIP）→ 内层包按 fromNodeID 注入 NIC 3 (Link-GG) → netstack 正常走 Prerouting（conntrack 得到 InputInterface=NIC3）
2. DNAT 后目标是 LAN IP，路由表有明确路由（192.168.0.0/16 → NIC 1），不需要策略路由

---

## 5. Link NICs 设计

### 5.1 设计原则

**每个直连 mesh peer 一个 NIC**，而非共享 mesh NIC。

**优势**：
- ✅ 每个 NIC 直接和链路通信，无需外部判断逻辑
- ✅ 路由表明确：子网 → Link NIC
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
| **Link NICs（本文）** | N+1 个 | gVisor 路由表 | **低** |

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
// 在路由表查询前/后调用 RouteSelector
if s.routeSelector != nil {
    decision := s.routeSelector(remoteAddr)
    if decision.LocalDelivery {
        return makeLocalRoute(...)
    }
    if decision.NeedIPIP {
        return makeIPIPRoute(decision.EgressVIP)
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

### 6.8 补丁 #3 修订：RouteSelector 生效范围（新增）

**问题**：补丁 #3 在所有 FindRoute 调用前执行，包括传输层 accept 路径的建连路由（`tcp/accept.go: FindRoute(inNIC, pktDst, pktSrc)`）。forwarder 回程的 remote 是 TUN 客户端（非 mesh 目标），会被误判为 NeedIPIP，SYN-ACK 被错误封装发往 mesh。

**修复**：RouteSelector 仅在 `id == 0 || localAddr == ""` 时介入（无显式本地上下文的选路）。accept/RST 等带接收 NIC + 本地地址的调用直接走 NIC 出栈早退分支。

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

// 3. 路由表（与 §1.2 一致，见其修订说明）
routeTable := []tcpip.Route{
    {Destination: vipAddr, NIC: 1},            // VIP → NIC1
    {Destination: "<本节点子网>", NIC: 1},      // 本节点子网（TUN 宿主回程）
    {Destination: "<宿主网段>", NIC: 1},        // 宿主网段（栈内 socket 应答，如 DNSHijacker）
    {Destination: "<peer 子网>", NIC: 2..N},   // 各 peer 子网 → 对应 Link NIC
}
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
| 封装后外层包怎么路由？ | 重新 FindRoute，匹配 Link NIC |
| SNAT 怎么只针对旁路网关？ | **补丁 #4**：Postrouting InputInterface 匹配 |
| DNAT 回程怎么路由？ | **补丁 #5+#6**：Conntrack 辅助路由，支持任意源 IP |
| 需要配置 LAN 网段路由吗？ | **不需要**，conntrack 自动处理 |

---

## 9. 参考文档

- `gvisor_stack_integration_design.md` —— 前期调研（部分结论被本文更新）
- `mesh_ipip_smart_routing.md` —— IPIP 出口节点选择算法
- `traffic_flow_analysis.md` —— 流量路径分析（本文整合了其结论）
