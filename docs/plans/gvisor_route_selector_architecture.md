# gVisor 路由架构最终方案：RouteSelector + Link NICs

## 状态：✅ 最终方案（2026-10-08 修订：补丁 #10 `LocalLoopback` 出栈栈内环回，取代已回退的补丁 #9 `HandleLocal=true`；RouteSelector 4 分支逻辑 + 路由表完全清空 + conntrack 辅助回程）

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
- **路由表完全清空**：所有路由决策（mesh、非 mesh、本地地址）都由 RouteSelector 处理（§2.3）。路由表不包含 VIP/32、本节点子网 /16、192.168/16、对端子网等任何条目。
- **本地子网与 192.168 的处理**：本地交付（含 fakeIP、GIP、VIP）由 RouteSelector 分支 1 的 `EgressNIC=1 + LocalDelivery=true` 覆盖（出栈经 TUN、入栈由 handleValidatedPacket 拦截）；其他 192.168.x.x 等非栈内地址走分支 4 经 NIC 1 → TUN → OS，由宿主网络栈做下一跳决策。
- **栈 socket 也能找到路由**：分支 1 的 `EgressNIC=1` 保证栈 socket `Connect(fakeIP)` 在 FindRoute 中能构造路由（fork patch #3 只对 `NeedIPIP`/显式 `EgressNIC` 构造路由）。
- **Conntrack 管理的回程包例外**：旁路网关回程包（src=VIP, dst=客户端）通过 conntrack 记录的 `originalInputNIC` 直接路由回 NIC 1（patch #5/#6，`FindRouteViaNIC`），跳过 RouteSelector 和路由表。
- **Socket/Forwarder 回程包走 RouteSelector**：DNSHijacker 应答、forwarder 回复等栈内 socket 的回程包仍然走 RouteSelector，由 RouteSelector 根据目标地址返回正确的 EgressNIC（dst=LAN 客户端经分支 4 走 NIC 1）。

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

**语义双轨**：
- **FindRoute 路径**（stack socket `Connect()` SYN）：RouteSelector 必须给出 `EgressNIC`（或 `NeedIPIP`），否则 FindRoute 不会构造路由，SYN 无法发出。
- **handleValidatedPacket 路径**（NIC 收包，`patch #2b`）：RouteSelector 的 `LocalDelivery=true` 会跳过转发、在 IP 层本地交付（依赖 NIC 1 混杂模式 + `AcquireAssignedAddress`，对 fakeIP/GIP/VIP 等本地地址可达）。

**为什么需要 EgressNIC=1（出栈 TUN 路径）**：
- 栈内 socket `Connect(fakeIP)` 在 gvisor 中走 IP 层 forwardUnicastPacket → FindRoute；没有路由可发包。
- FindRoute 只对 `NeedIPIP` 或显式 `EgressNIC` 构造路由（fork patch #3 line 1674），`LocalDelivery` 单独设置不构造路由。
- 因此 fakeIP/GIP/VIP 等"本地交付"目标在 FindRoute 中也必须附 `EgressNIC=1`——SYN 经 NIC 1 → writeLoop → TUN → OS 识别为 TUN 子网地址 → 路由回 TUN → NIC 1 入站 → handleValidatedPacket 看到 `LocalDelivery=true` → 本地交付给 Forwarder（loopback 单跳，开销可接受）。

```go
func RouteSelector(dst tcpip.Address) RouteDecision {
    // 1. 本地 mesh 子网（fakeIP / GIP / VIP / hostIP）
    //    入栈：handleValidatedPacket 看到 LocalDelivery=true 拦截并本地交付
    //          （给 Forwarder 处理 fakeIP / 给 admin listener 处理 GIP 等）。
    //    出栈：LocalLoopback=true → Route.Loop=PacketLoop，包在栈内直接交付，
    //          不写 NIC（补丁 #10）。唯一例外是 dst==VIP —— 那是 Forwarder /
    //          DNS 劫持器回给入站客户端的包（客户端源已被 Input 链 SNAT 归一
    //          化成 VIP），必须出 NIC 1，由 conntrack 在 Postrouting 反翻译回
    //          LAN 客户端地址。
    if isLocalMeshSubnet(dst) {
        return RouteDecision{
            EgressNIC:     1,                         // 构造 route 所需；环回时不写 NIC
            LocalDelivery: true,                      // 入栈时拦截，不走 forwardUnicastPacket
            LocalLoopback: dst != localVIP,           // 补丁 #10：出栈环回（VIP 除外）
            Cacheable:     true,
        }
    }
    
    // 2. 其他 mesh 子网（直连或非直连）→ 下一跳 Link NIC 直送，无 IPIP
    if isMeshSubnet(dst) {
        nextHopNIC, egressNodeID := selectNextHop(dst)  // Dijkstra 选下一跳 Link NIC
        return RouteDecision{
            EgressNIC: nextHopNIC,
            Cacheable: false,  // 拓扑变化时重新计算
        }
    }
    
    // 3. 通告路由匹配（非 mesh）→ IPIP 封装到出口节点 EIP
    if egressNodeID, found := matchAdvertisedRoute(dst); found {
        nextHopNIC := getNextHopLinkNIC(egressNodeID)
        egressEIP := calculateEIP(getNodeSubnet(egressNodeID))
        return RouteDecision{
            EgressNIC: nextHopNIC,
            NeedIPIP:  true,
            EgressEIP: egressEIP,
            Cacheable: true,
        }
    }
    
    // 4. 兜底（匹配不上任何通告路由的外部 IP）
    //    入栈：LocalDelivery=true 拦截并本地交付给 Forwarder —— 这是 TUN 抓到
    //          的 OS 流量、旁路网关流量的代理入口，同时是补丁 #8 的 panic 防御。
    //    出栈：LocalLoopback=true → 栈内环回给 Forwarder（补丁 #10）。出站命中
    //          Branch 4 的只有 phaethon 自有 socket（NetDial / 栈内 DNS）：
    //          Forwarder 与劫持器的回包 dst 是 VIP，落 Branch 1。
    //    EgressNIC=1 仍保留：FindRoute 需要一个 NIC 来取 address endpoint 构造
    //          route，即使该 route 最终不写 NIC。
    return RouteDecision{
        EgressNIC:     1,
        LocalDelivery: true,
        LocalLoopback: true,                          // 补丁 #10
        Cacheable:     true,
    }
}
```

**关键变化**：
- **所有 FindRoute 都走 RouteSelector**：删除 `id == 0 && localAddr == ""` 限制。RouteSelector 处理所有路由决策（除了 conntrack 管理的回程包，见 patch #5/#6）。
- **本地 mesh 子网优先**：本节点子网（如 VM 的 100.1.0.0/16）直接 LocalDelivery，不走 Link NIC。fakeIP 在本节点子网范围内，由情况 1 覆盖。
- **统一 EgressNIC 语义**：RouteSelector 返回的 EgressNIC 是下一跳的 Link NIC（直连 peer），gVisor 直接使用该 NIC 发送包。
- **非 mesh 一次性算好**：RouteSelector 同时返回 EgressNIC（下一跳）和 EgressEIP（IPIP 外层目标），无需递归查找。
- **兜底 LocalDelivery（补丁 #8 强化）**：匹配不上通告路由的目标**强制** `LocalDelivery=true` 走本地交付给 Forwarder，**禁止**进转发路径。只有匹配到通告路由的非 mesh 目标才走 IPIP 封装。这是 gVisor 不 panic 防御 + 兜底语义的双重保险。
- **LocalDelivery 与 LocalLoopback 是两个方向的语义（补丁 #10）**：`LocalDelivery` 是**入站** flag，只被 `handleValidatedPacket` 消费（"这个 dst 的包从 NIC 进来时别转发，交给 Forwarder"）；`FindRoute` 从不读它。`LocalLoopback` 是**出站** flag，只被 `FindRoute` 消费（"这个本地生成的包在栈内交付，别写 NIC"）。两者互不影响，必须分开。
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
    // LocalDelivery 单独设置在 FindRoute 中**不构造路由**——它是入栈拦截标志，
    // 由 handleValidatedPacket（patch #2b）读取；FindRoute 必须有 EgressNIC 才能
    // 让出栈 SYN 找到出口（设计 §2.3 语义双轨）。
    if decision.NeedIPIP {
        return makeIPIPRoute(decision.EgressNIC, decision.EgressEIP)
    }
    if decision.EgressNIC != 0 {
        return makeRouteViaNIC(decision.EgressNIC, ...)
    }
    // 无 EgressNIC 且无 NeedIPIP → fall through 到路由表（兜底分支 4 的 fallback 行为）
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

### 6.10 补丁 #7：本地 socket 流量跳过 InputInterface 匹配（新增，对齐 Linux 语义）

**问题**：本地 socket → 本地服务的流量（典型：`MeshDial` 通过 `Netstack.ResolveDomain` 创建 UDP endpoint 解析域名，dst=100.0.0.3=GIP）在 gVisor 内部被路由到 NIC 1（dst 所在 NIC），进 `deliverPacketLocally` → `CheckInput`。此时 `inNICName = e.nic.Name() = "tun"`，**命中** Input 链的 `InputInterface=="tun"` SNAT 规则，src 被改写成 VIP。

同时，本地 socket 流量**跳过 Prerouting**（`handleLocalPacket` 设计如此），所以**不建立 conntrack entry**。响应包走到 Prerouting 时 DNAT 无 entry 可查，dst 仍是 VIP（未绑地址），被丢弃或错误转发。

实证（QG `/tmp/phaethon-snat-debug.log`）：458 条 `src=100.0.0.1 dst=100.0.0.3 snatDone=true`（请求被 SNAT 改写），`/tmp/phaethon-prerouting-debug.log` 中 0 条 src=100.0.0.1（响应被丢前无 conntrack entry）。

**Linux 语义对照**：
- Linux：本地 socket 发到本机的包**不进 INPUT 链**，只过 OUTPUT/POSTROUTING。`-i tun` 不会命中。
- gVisor：本地 socket 流量走 `handleLocalPacket → handleValidatedPacket → deliverPacketLocally → CheckInput`，inNICName 被错误地设为 dst 所在 NIC 名（"tun"），行为与 Linux 不一致。

**修复**：让 `handleLocalPacket` 把空 inNICName 传给 `handleValidatedPacket`，这样 `CheckInput` 中的 `InputInterface=="tun"` filter 不会命中，Input 链 SNAT 不再误伤本地 socket 流量。

**文件**：`pkg/tcpip/network/ipv4/ipv4.go`
**函数**：`handleLocalPacket`（行 983-1000）

**改动**：
```go
func (e *endpoint) handleLocalPacket(pkt *stack.PacketBuffer, canSkipRXChecksum bool) {
    ...
    e.handleValidatedPacket(h, pkt, "" /* inNICName */)  // 之前是 e.nic.Name()
}
```

**效果**：
- ✅ 本地 socket → 本地服务：Input 链 SNAT 不命中，conntrack 不需要介入，response 正常 loopback 投递
- ✅ 外部 NIC 接收 → 本地服务：仍然走 `HandlePacket` → `handleValidatedPacket(h, pkt, e.nic.Name())`（行 977），inNICName 是真实接收 NIC，Input 链 SNAT 正常命中（仅当接收 NIC 是 "tun" 时）
- ✅ 旁路网关去程 SNAT 路径（NIC 1 接收 → 转发）不受影响（Postrouting 链独立工作）

**心智模型统一**：改完后，gVisor iptables 规则与 Linux iptables 规则一一对应：
- Linux `-i tun -j SNAT` ⇔ gVisor `InputInterface=="tun"` 仅匹配真实从 NIC 1 接收的包
- 本地 socket 流量走 OUTPUT/POSTROUTING（gVisor 同理），不受 INPUT 链影响

### 6.11 补丁 #8：Branch 4 强制 LocalDelivery + handleForwardingError 不 panic（新增）

**问题**（2026-10-08 QG 验证发现）：

Branch 4（兜底，匹配不上任何通告路由的外部 IP）当前实现只设 `EgressNIC=1`、不设 `LocalDelivery`，导致入栈包走 `forwardUnicastPacket` → `FindRoute`（dst 不在路由表、不在 LocalSubnet、本地无 endpoint 匹配）→ 返回 `*tcpip.ErrHostUnreachable` → 在 `forwardPacketWithRoute` 包装为 `&ip.ErrOther{Err: err}` → `handleForwardingError` 落到 `*ip.ErrOther` 内层 default case → **panic**，把整个 phaethon worker 进程搞死，watchdog 每 ~10 分钟拉起一次。

**双重违反设计意图**：
1. 设计 §2.3「兜底 LocalDelivery」明确要求 Branch 4 走本地交付给 Forwarder，而不是进转发路径。
2. gVisor fork `handleForwardingError` 用 `panic` 处理未识别的 forwarding error，是上游防御性 bug——任何新增的 forwarding 错误（iptables reject、conntrack 状态异常、未来 gVisor 升级）都会再次 panic。

**修复**（双管齐下）：

**(a) RouteSelector Branch 4 显式设 `LocalDelivery=true`**

文件：`mesh/route_selector.go`
```go
// Branch 4: 兜底 → 出栈经 TUN 走到 OS 网络栈；入栈时 handleValidatedPacket
// 拦截并本地交付给 Forwarder（同 Branch 1 loopback 路径）。
return stack.RouteDecision{
    EgressNIC:     1,
    LocalDelivery: true,
    Cacheable:     true,
}
```

入栈包走 `deliverPacketLocally` → `AcquireAssignedAddress`（dst 不在本地，nil）→ 落到 `tcp.NewForwarder` 兜底 → phaethon 的 `acceptTCP` 接收 → 走 mesh/Proxy 转发到实际目的地。
出栈（stack-socket Connect）包：`EgressNIC=1` 让 FindRoute 仍构造 NIC 1 路由。~~包经 writeLoop → TUN → OS → 路由回 TUN → NIC 1 接收 → `LocalDelivery=true` → 走 deliverPacketLocally（loopback 单跳）~~ **【已被补丁 #10 取代】** 出栈改由 `LocalLoopback=true` 在栈内直接环回，不再依赖 TUN 绕回，见 §6.13。

**(b) handleForwardingError 防御性不 panic**

文件：`gvisor-fork/pkg/tcpip/network/ipv4/ipv4.go`（同 ipv6.go）
函数：`handleForwardingError`

将 `*ip.ErrOther` 内层 default case 从 `panic(...)` 改为 `log.Errorf + stats.Forwarding.Errors.Increment()` 静默丢弃；外层 default case 同理。任何未识别的 forwarding 错误（含未来 gVisor 升级引入的新错误）只记日志、不再杀进程。

**效果**：
- ✅ QG panic 循环立即停止（消除 ssh/外部 IP 访问根因）
- ✅ 非 mesh 外部 IP（dst=106.13.183.103 等）正常进入 Forwarder，由 Proxy 链转发
- ✅ 防御性兜底：未来任何未知 forwarding 错误不再拖垮整个进程

### 6.12 补丁 #9：`stack.Options.HandleLocal = true`（❌ 已回退，由 §6.13 补丁 #10 取代）

> **回退原因**：`HandleLocal` 是全局开关，除了想要的自寻址 loopback，还附带打开 `HandlePacket` 里"源地址是本机地址就丢包"的检查（`ipv4.go:955-964`），在 NIC 1 混杂 + 转发 + Input SNAT 组合下把外部 DNS 查询（`192.168.1.7 → 100.0.0.3:53`）静默丢弃（任务 #428 单变量测试确认）。且它只覆盖 `localAddr == remoteAddr`，覆盖不到 Branch 4。以下为原始记录，保留作决策依据。

**问题**（VM 部署后实测发现）：

gVisor `stack.Options.HandleLocal` 默认为 `false`。在 phaethon 的 2-NIC 拓扑里，这意味着：

- DNS 解析（`ResolveDomain` → `mesh/netstack.go:1117`）创建的 UDP socket，src 由 gVisor 自动选为 GIP（NIC 1 绑定的本地地址），dst=GIP:53（同机 DNS hijacker）。
- 拨号（`dialTCP` / `NetDialWithPreConnect` → `mesh/netstack.go:766/903`）创建的 TCP socket，src=GIP，dst=fakeIP。
- 这些"自寻址"包走 `FindRoute → makeRoute`（`gvisor-fork/pkg/tcpip/stack/route.go:201-216`）：
  ```go
  loop := PacketOut
  if !outgoingNIC.IsLoopback() {
      if handleLocal && localAddr != (tcpip.Address{}) && remoteAddr == localAddr {
          loop = PacketLoop
      }
      // ...
  }
  ```
- 因为 `handleLocal=false`，`loop = PacketOut`，包被写出 NIC → TUN → OS 网络栈。
- **OS（Windows / Linux）不会把 TUN 设备自己发出去的包再 loopback 回 TUN 设备**——这是 OS 网络层的物理事实，不依赖任何路由配置。
- 结果：DNS 30 秒超时（SOCKS5 报 "connection refused"）、TCP 拨号永久卡在 SYN 阶段。

**实测证据**（VM phaethon-stdout.log，2026-10-08 19:55:59）：

```
[DIAG-DEBUG] initStack: HandleLocal=false
[DIAG-DEBUG] ResolveDomain udp src=100.1.0.3:18060 dst=100.1.0.3:53 HandleLocal=false
[DIAG-DEBUG] GIP-touch pkt#127: 100.1.0.3:18060 -> 100.1.0.3:53 (proto=17 len=62, MeshSubnetContains=true)
[DIAG-DEBUG] GIP-touch pkt#127 WROTE to TUN: 100.1.0.3:18060 -> 100.1.0.3:53
```

包**确实**被 gVisor 写到了 TUN（"WROTE to TUN" 日志）。但 30 秒后没有响应——OS 层 loopback 失败。

**修复**：

文件：`mesh/netstack.go`
函数：`initStack`

```go
s := stack.New(stack.Options{
    NetworkProtocols:   []stack.NetworkProtocolFactory{ipv4.NewProtocol, ipv6.NewProtocol},
    TransportProtocols: []stack.TransportProtocolFactory{tcp.NewProtocol, udp.NewProtocol, func(s *stack.Stack) stack.TransportProtocol { return newIPIPProtocol(s, linkEP) }},
    HandleLocal:        true,  // 让 gVisor 内部 loopback 自寻址包（GIP→GIP、VIP→VIP、EIP→EIP）
})
```

**为什么是 `HandleLocal: true`，而不是依赖 OS loopback**：

用户曾质疑"Mode B 借助 OS 路由环回是错误实现"——这个质疑在 TUN 边界场景下**成立**。OS 永远不会把 TUN 设备自己发出去的包 loopback 回来（TUN 不是 lo 接口），所以靠 OS loopback 是不可行的实现。`HandleLocal=true` 让 gVisor **在协议栈内部**就完成 loopback：

1. `FindRoute` 命中 `findLocalRouteRLocked`（`stack.go:1835`，被 `s.handleLocal` 门控）
3. `makeRoute` 在 `handleLocal && localAddr == remoteAddr` 时设 `loop = PacketLoop`
4. `writePacketPostRouting` 走 `PacketLoop` 分支 → `handleLocalPacket` → `deliverPacketLocally` → DNS hijacker / TCP Forwarder

整个过程**完全在 gVisor 内**，不经过 TUN 写回、不依赖 OS 任何行为。

**不需要 fork**：这是公开的 `stack.Options` 字段，运行时通过 `s.HandleLocal()` 可读。

**验证方法**：

- `s.HandleLocal()` 返回 `true`
- `ResolveDomain` 不再 timeout
- `dialTCP` 的 SYN 在 gVisor 内完成 loopback，connect 成功
- DNS hijacker 日志显示 `from=100.1.0.1:<ephemeral>`（OS-NAT 视角的 source，因为包根本没走 TUN）—— 注意：从 gVisor 看 src 仍是 100.1.0.3，只是 loopback 在栈内完成

**潜在影响**：

- ✅ DNS 解析正常（30s → <100ms）
- ✅ MeshDial 拨号正常（避免 timeout 卡死）
- ✅ 本地 socket 互访正常（如 phaethon admin 调 DNS hijacker 自身）
- ⚠️ 所有"自寻址"流量改走栈内 loopback，**不再写 TUN 也不走 OS NAT**——这是预期行为，不会破坏 NAT（NAT 设计是给外部 LAN 主机用的，loopback 流量无需 NAT）
- ⚠️ gVisor 内部额外开销：每次自寻址包多走一次 `handleLocalPacket`，可忽略

### 6.13 补丁 #10：`LocalLoopback` 出栈栈内环回（新增，2026-10-08，取代补丁 #9）

**补丁 #9（`HandleLocal=true`）已回退**：它是 `stack.Options` 的全局开关，除了想要的"自寻址包栈内 loopback"，还附带打开了 `HandlePacket` 里的"源地址是本机地址就丢包"检查（`ipv4.go:955-964`），在 NIC 1 混杂 + 转发 + Input SNAT 的组合下把外部 DNS 查询（`192.168.1.7 → 100.0.0.3:53`）静默丢弃（任务 #428 单变量测试确认）。而且它只覆盖 `localAddr == remoteAddr` 的自寻址包，覆盖不到 Branch 4。

**问题**（JF / GG 实测）：

Branch 1（非 VIP）和 Branch 4 的**本地交付**原先是靠**出栈绕一圈 TUN** 实现的：

```
EgressNIC=1 → writeLoop → TUN → OS 路由 → 回 TUN → readLoop → InjectInbound
            → NIC 1 → handleValidatedPacket(LocalDelivery=true) → Forwarder
```

在 TUN 关闭的节点（JF / GG / MS9 / MS10）上 `tun.Engine.Write` 返回 `TUN device not available`，`mesh/netstack.go` 的 `writeLoop` 静默丢包：

- GG：`100.179.0.3:44471 → 192.168.1.88:22` SYN 重传 5 次后 `context deadline exceeded` → 反向代理（Direct / HTTP / Reverse / SOCKS5 / Trojan）全挂
- JF：`GIP → GIP:53` 栈内 DNS 丢包 544 次
- Branch 2/3（Link NIC / IPIP）不受影响 → mesh 路由与**入站** mesh admin 正常，所以故障表现为"选择性失效"

**修复**：把环回做在 gVisor 内部，不依赖 TUN 设备是否存在。

`writePacketPostRouting`（`ipv4.go:576-585`）本来就有这个能力：

```go
if r.Loop()&stack.PacketLoop != 0 {
    e.handleLocalPacket(pkt, !headerIncluded /* canSkipRXChecksum */)
}
if r.Loop()&stack.PacketOut == 0 {
    return nil                      // 不写 NIC，也不走 Postrouting
}
```

只要让 route 带上 `Loop = PacketLoop`（且清掉 `PacketOut`），包就在栈内交付。

**为什么不重演补丁 #9 的回归**：`handleLocalPacket`（`ipv4.go:980-1005`）**不含** `HandleLocal()` 那段"源地址是本机就丢"的检查——那段只在 `HandlePacket`（外部收包路径）里。所以 `PacketLoop` 环回拿到了好处，碰不到那个坑。

**(a) fork：`RouteDecision` 加出站专用字段**

文件：`gvisor-fork/pkg/tcpip/stack/stack.go`

```go
type RouteDecision struct {
    EgressNIC     tcpip.NICID
    NeedIPIP      bool
    EgressEIP     tcpip.Address
    Cacheable     bool
    LocalDelivery bool          // 入站语义：handleValidatedPacket 消费（补丁 #2b/#8）
    LocalLoopback bool          // 出站语义：FindRoute 消费（补丁 #10）
}
```

`LocalDelivery` 与 `LocalLoopback` **必须分开**：`FindRoute` 从不读 `LocalDelivery`（`stack.go:1629` 注释已明确"Don't act on it here"），`handleValidatedPacket` 从不读 `LocalLoopback`。

**(b) fork：`FindRoute` 单点覆写**

现有 `FindRoute` body 原样改名 `findRouteInner`，外面套一层：

```go
func (s *Stack) FindRoute(id tcpip.NICID, localAddr, remoteAddr tcpip.Address, netProto tcpip.NetworkProtocolNumber, multicastLoop bool) (*Route, tcpip.Error) {
    r, err := s.findRouteInner(id, localAddr, remoteAddr, netProto, multicastLoop)
    if err != nil || r == nil {
        return r, err
    }
    if s.RouteSelectorLocalLoopback(remoteAddr) {
        r.setLocalLoopback()   // routeInfo.Loop = PacketLoop
    }
    return r, nil
}
```

选单点覆写而不是把参数穿过 7 个 `makeRoute` 调用点：`FindRoute` 内部 return 分支太多（IPIP / EgressNIC / 路由表 / 本地路由 / 兜底），改一处比改七处安全。`RouteSelectorLocalLoopback` 复用已有的 dst 决策缓存（`decisionForSelectorRLocked`）。

`setLocalLoopback()` 的连带效果都是正确语义：
- `makeRoute` 在 `Loop()&PacketOut == 0` 时 early-return（`route.go:223`），跳过网关/链路解析——环回包本来就不需要
- `local()`（`route.go:471`）变 true → `RequiresTXTransportChecksum()` 返回 false、`isResolutionRequiredRLocked()` 跳过 ARP——与 gVisor 自己的 `makeLocalRoute` 一致

`ipv4.go` / `ipv6.go` **不改**。

**(c) phaethon：RouteSelector 三分支各加一行**

文件：`mesh/route_selector.go`

| 分支 | `LocalLoopback` | 理由 |
|---|---|---|
| 1 本地 mesh 子网 | `dst != cfg.LocalVIP` | VIP 是回包目的地，见下 |
| 2/3 mesh、已通告 | `false` | 走 Link NIC / IPIP，不变 |
| 4 兜底 | `true` | 出站命中 Branch 4 的只有自有 socket |

**判据为什么是 `dst != VIP`（关键，2026-10-08 QG 实测确认）**：

Input 链的 SNAT 规则（`mesh/netstack.go:606-620`，`InputInterface=="tun"` → src 改写为 VIP）把所有从 NIC 进来、本地交付的客户端源地址**归一化成 VIP**。因此 Forwarder 与 DNS 劫持器只认得 VIP，回包在 `FindRoute` 时的 `remoteAddr` 就是 **VIP**（落 Branch 1），LAN 客户端地址是之后由 conntrack 在 Postrouting 反翻译出来的——**它从不到达 RouteSelector**。

实测证据（QG `/root/data/logs/phaethon.log`）：

```
[TCP-DEBUG] tcp forwarder called local=100.0.0.14:443   remote=100.0.0.1:54806
[TCP-DEBUG] tcp forwarder called local=8.212.124.35:443 remote=100.0.0.1:54815
[DNS-DEBUG] DNSHijacker: query domain=qg.phn from=100.0.0.1:13616
```

`remote` / `from` **全部**是 `100.0.0.1` = VIP，无一例外。writeLoop 里看到的 `100.0.0.12 -> 192.168.1.88`、`100.0.0.3 -> 192.168.1.7` 都是 **Postrouting 反翻译之后**的地址，不能拿来推断选路时的 dst。

于是出站方向在 `FindRoute` 处的全集是：

| 发送者 | remoteAddr | Branch | 应做 |
|---|---|---|---|
| Forwarder 回包（TUN / 旁路客户端） | **VIP** | 1 | 出 NIC 1 → TUN → Postrouting 反翻译 |
| DNS 劫持回包（TUN / 旁路客户端） | **VIP** | 1 | 出 NIC 1 → TUN |
| Forwarder 回包（mesh peer，Link NIC 入站不过 SNAT） | peer mesh 地址 | 2/3 | 出 Link NIC |
| NetDial SYN → 非 mesh 未通告目标 | 真实目标 | 4 | **环回** |
| NetDial → fakeIP / GIP | Branch 1 地址 | 1 | **环回** |
| `ResolveDomain` 栈内 DNS | GIP | 1 | **环回** |

分界只有 `dst == VIP` 一条。phaethon 自有 socket **从不**主动拨 VIP（`CalculateVIP` 只用于 RouteSelector 配置，`GetVIP()` 只喂 `SetMeshConfig`，全代码无拨号），所以不会误判。

**环回不会被 NAT 干扰（已核对代码）**：

- `handleLocalPacket` 调 `handleValidatedPacket(h, pkt, "" /* inNICName */)`（补丁 #7）→ `deliverPacketLocally` → `CheckInput(pkt, "")` → `InputInterface=="tun"` **不匹配** → 落 Input catch-all accept → **不改源地址**。环回的 SYN 保持 `src=GIP`，Forwarder 回包 `dst=GIP`（≠VIP）→ 同样环回 → 回到 NetDial endpoint，全程不出栈。
- `Loop=PacketLoop` 且无 `PacketOut` 时，`writePacketPostRouting` 在 `ipv4.go:582` 直接 return，**跳过 Postrouting**（`pkt.InputNICName` 在 `ipv4.go:591` 才赋值），SNAT 碰不到。

**改完后的包路径**：

```
NetDial(GIP:port → 192.168.1.88:22)
  → FindRoute → Branch 4 → LocalLoopback=true → Loop=PacketLoop
  → writePacketPostRouting: handleLocalPacket() 后 return，不写 NIC
  → handleValidatedPacket(inNICName="")
      → CheckInput("") 不匹配 "tun" → 不 SNAT，src 保持 GIP
      → RouteSelectorLocalDelivery(dst)=true → 跳过转发（补丁 #2b）
      → AcquireAssignedAddress(promiscuous) → deliverPacketLocally
  → TCP Forwarder → acceptTCP（Mode B 匹配）→ handleConn（规则 / 代理链）
  → 回包 dst=GIP ≠ VIP → Branch 1 LocalLoopback=true → 同样环回 → NetDial endpoint
```

TUN 有没有、能不能写，全程无关。

**⚠️ 必须守住的耦合**：`dst != VIP` 这条判据**隐式依赖 Input 链 SNAT 把入站客户端源归一化成 VIP**。谁改了 / 删了那条 SNAT 规则，Forwarder 与劫持器的回包就会掉进环回分支，QG 的 TUN 客户端与旁路网关当场全断。修改 `mesh/netstack.go` 的 NAT 配置时必须同时复核本节。

**不动的东西**：`dialTCP` / `dialUDP` 仍绑 GIP、DNS 劫持器仍绑 GIP:53、Input/Postrouting SNAT 配置、`writeLoop`、地址分配（**不新增保留地址**）。

### 6.14 补丁 #11：Input 链 conntrack 补记输入 NIC（新增，2026-10-09）

**现象**（QG 实测，旁路网关 DNS 查询）：DNS 劫持器对 Branch 1（`dst==VIP`，§6.3 判据表第一行）的回包，`h.udpEP.Write()` 本身成功（无错误），但回包**有时**（与 `recomputeRoutes` 周期性重算在时间上相关）不经 `writeLoop`/TUN 直接出站，而是源地址被改写成 QG 物理 LAN 口地址（`192.168.1.101`）+ 随机临时端口后才到达客户端——不是补丁 #9 那种全丢，是**回包被二次误路由后由 `tun/engine.go handleUDP`（Forwarder 的直连 UDP 转发器）当成新连接转发出去**，日志实证：`handleUDP invoked: src=100.0.0.3 dst=192.168.1.88:PORT` 紧跟在 `h.udpEP.Write()` 成功之后出现，且这类误路由发生时完全没有 `writeLoop`/`wrote pktN to TUN` 日志。

**根因**：补丁 #5（§6.5）引入的 `conntrackEntry.OriginalInputNIC`（fork 实际字段名 `cn.originalInputNIC`）只在 **Postrouting 链**路径被正确记录——`performNAT`（`conntrack.go:825-827`）读取的 `pkt.InputNICName` 只在 `writePacketPostRouting`（`ipv4.go:591`，`ipv6.go:864`）里被赋值一次，这是**转发流量**专属路径。但 Input 链 SNAT（`mesh/netstack.go:603-616`，命中条件 `InputInterface=="tun"`，用于 DNS 劫持器 / Forwarder 本机交付流量）命中时走的是 `deliverPacketLocally → CheckInput(pkt, inNICName)`（`ipv4.go:1349`），`inNICName`只是本地变量，从未写回 `pkt.InputNICName`。

后果：Input 链 SNAT 建立的 conntrack 连接，`originalInputNIC` 永远是空，回包的 `pkt.OutputNICName`（conntrack 在 `conntrack.go:996-1000` 回填）也永远是空。`(*endpoint).writePacket`（`ipv4.go:531-574`）在目的地址被 DNAT 还原后发现 `dstAddr != newDstAddr`，本该走 `pkt.OutputNICName != ""` 的快速路径（直接复用原 Route 写 NIC，不二次路由，`ipv4.go:555-563`），但因为该字段恒为空，只能落入 fallback（`ipv4.go:564-570`，`ep.handleLocalPacket`），用 DNAT 还原后的新目的地址（真实 LAN 客户端 IP）**重新跑一次 RouteSelector**（`handleValidatedPacket` → `RouteSelectorLocalDelivery`，`ipv4.go:1272`）。真实 LAN 地址不在 mesh 子网/VIP/GIP/fakeIP 里，落 Branch 4 兜底（`LocalDelivery:true`），被当成"查无主的新连接"扔进 `deliverPacketLocally` 的传输层 demux fallback，也就是 phaethon 注册的 UDP Forwarder（`handleUDP`），从而在物理网卡上开新 socket 转发——这就是观测到的错误源地址。是否命中 fallback 的"误打误撞对了"分支（`findEndpointWithAddress(newDstAddr)` 精确匹配，`ipv4.go:194-205`）取决于当时 NIC 1 混杂模式下临时端点的存在状态，随 `SyncLinkNICs`/路由重算的时机波动，解释了现象的非确定性。

**修复**：对齐补丁 #5 原本的设计意图（"记录包从哪个 NIC 进入"应覆盖**所有**本地交付路径，不止转发路径），在 Input 链交付路径补上 `pkt.InputNICName` 的赋值，使其与 Postrouting 链路径对称：

```go
// pkg/tcpip/network/ipv4/ipv4.go deliverPacketLocally（及调用链上 CheckInput 之前）
// 补丁 #11：对齐补丁 #5，Input 链本地交付也要记录输入 NIC，
// 否则该连接的 conntrack 条目 originalInputNIC 永远为空，
// DNAT 回包的 OutputNICName 填不上，writePacket 只能走
// 二次 RouteSelector 的 fallback，被误判为新连接扔给 Forwarder。
if pkt.InputNICName == "" {
    pkt.InputNICName = inNICName // e.nic.Name()，deliverPacketLocally 调用处已有
}
```

`pkg/tcpip/network/ipv6/ipv6.go` 对应位置同步镜像修改（与补丁 #5/#7 的 ipv4/ipv6 双修惯例一致）。

**影响范围**：只影响 Input 链 SNAT 命中的连接（即 TUN NIC 1 混杂模式下本地交付的流量：DNS 劫持器 GIP:53、TCP/UDP Forwarder 本机监听）的 conntrack `originalInputNIC` 记录，不改变 RouteSelector 判据、不改变 NAT 规则配置、不改变 Postrouting 链已有行为（它已经是对称正确的）。

**验证方式**：QG 旁路网关场景下用独立 DNS 探针脚本反复查询 mesh 域名触发 `forwardToRemote` 超时走 SERVFAIL 分支，确认回包源地址稳定为 GIP:53（而不是物理 LAN 口地址+随机端口），且 `writeLoop` 日志里稳定出现 `wrote pktN to TUN`，不再出现 `handleUDP invoked` 介入同一条回包。

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
3. ✅ 补丁 #3：RouteSelector 扩展点
4. ✅ 补丁 #4：Postrouting InputInterface 匹配
5. ✅ 补丁 #5：Conntrack 记录输入接口
6. ✅ 补丁 #6：DNAT 辅助路由
7. ✅ 补丁 #7：本地 socket 流量跳过 InputInterface 匹配
8. ✅ 补丁 #8：Branch 4 强制 LocalDelivery + handleForwardingError 不 panic
9. ❌ 补丁 #9：`stack.Options.HandleLocal=true`（**已回退**，引入外部 DNS 丢包回归，见 §6.12）
10. ✅ 补丁 #10：`RouteDecision.LocalLoopback` + `FindRoute` 单点覆写 → 出栈栈内环回（取代 #9，见 §6.13）
11. ✅ 补丁 #11：Input 链 conntrack 补记输入 NIC，修复 DNS 劫持器/Forwarder 回包源地址误改写（见 §6.14）

**架构实现**：
1. ✅ Link NICs：每个直连 peer 一个 NIC
2. ✅ IPIP 封装内化：在转发路径完成
3. ✅ NAT 规则配置：SNAT + DNAT + conntrack
4. ✅ 拦截器退役

### 阶段 3：验证

- [x] DNS 解析（HandleLocal=true 修复，补丁 #9）✅
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
