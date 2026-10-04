# gVisor 路由栈演进方向

## 状态：⚠️ 已废弃 - 被 `gvisor_stack_integration_design.md` 取代

**最终方案：gVisor fork 定制（见 `gvisor_stack_integration_design.md` 阶段 2）**

---

## 修订记录

- **2026-10-03 勘误**："核心限制：DNAT 必须在路由决策前"一节的死结结论不成立。经源码逐行核实（pinned v0.0.0-20250428193742）：PREROUTING 在路由判定前执行（ipv4.go:873）、NAT target 全套可用（DNAT/SNAT/Masquerade/Redirect）、conntrack 覆盖 ICMP echo/差错翻译、回程选路可用具体路由按最长前缀匹配解决。"NAT 必须在 TUN 边界（gVisor 外部）"从技术必然降级为当前边界架构的合理选择。详见"修订后的架构决策"一节。
- **2026-10-03 背景注**：本文"充分利用 gVisor 的 NIC 规划"及后续章节描述的多 NIC 架构存在已证实的 Forwarder 回程自环缺陷（FindRoute 连接 id 限定 + 默认路由直接匹配抢占转发兜底），架构方向待新 ADR 定夺；阅读后续章节请结合此背景。

## 结论

**长期演进方向**：将 gVisor netstack 作为主路由栈，减少心智模型复杂度。

## 当前架构痛点

### 心智模型复杂
- 路由逻辑分散在多处：readLoop、writeLoop、mesh forward
- TTL 处理需要手动实现（递减、检查、ICMP 生成）
- 混杂模式绕过 gVisor 正常路由路径
- 调试时需要理解多层路由决策

### 代码重复
- readLoop 和 HandleMeshFrame 都需要处理 TTL
- writeLoop 需要重新分类数据包
- 路由判断逻辑在多个地方出现

## gVisor NAT 能力边界调研（2026-09-17）

### 调研目标

探索能否将自定义 NAT（`mesh/nat.go`，558 行）下沉到 gVisor IPTables/ConnTrack，减少栈外手动实现。

### gVisor IPTables 与 Linux 的关键差异

| 能力 | Linux iptables | gVisor IPTables |
|------|---------------|-----------------|
| 策略路由（ip rule / 多路由表） | ✅ | ❌ 单表 + 仅按目标地址匹配 |
| fwmark / MARK target | ✅ | ❌ 完全没有 fwmark 概念 |
| conntrack zone | ✅ | ❌ |
| `-m conntrack --ctstate` | ✅ | ❌ 完全没有 conntrack match |
| DNAT 允许的 hook | Prerouting, Output | Prerouting, Output（其他 hook 直接 panic） |
| SNAT 允许的 hook | Postrouting, Input | Postrouting, Input（其他 hook 直接 panic） |
| POSTROUTING 接口匹配（-o） | ✅ | ❌ 忽略 -i/-o |
| MASQUERADE | ✅ | Go API 层存在（编程可用，未注册 syscall ABI） |
| ICMP 错误消息内层翻译 | ✅ | ✅（conntrack.go:948-970，等价于 nat.go 的 translateICMPError） |

### 核心限制：DNAT 必须在路由决策前

> **❌ 2026-10-03 勘误：本节"死结"结论不成立。**
>
> 前提"PREROUTING DNAT 在路由判定前执行"**正确**——已源码核实：`network/ipv4/ipv4.go:873`
> 在 `HandlePacket` 入口调用 `CheckPrerouting`，之后才进 `handleValidatedPacket` 做
> 本地交付/转发判定，官方注释明确 "CheckPrerouting can modify the backing storage of
> the packet"（ipv4.go:879）。
>
> 错误出在"无策略路由 → 只能靠默认路由"这一步：忽略了路由表的**最长前缀匹配**。
> 为回程前缀添加具体路由（如 `192.168.0.0/16 → TUN NIC`）即可引导 DNAT 还原后的
> 回程包，无需策略路由。去程（默认路由出 mesh）与回程（具体路由回 TUN）按目标
> 前缀天然分离，下列三个"死循环"分支均不存在。

**问题场景（原文）**：旁路网关流量（源地址任意，如 192.168.1.100）需要 SNAT 成 VIP 从 mesh 发出，回程包 DNAT 还原后需要路由回 TUN。

**gVisor 的死结**：
```
回程包 dst=VIP 到达 NIC 2 (Mesh)
    ↓
PREROUTING DNAT: dst=VIP → dst=192.168.1.100（必须在路由前）
    ↓
路由决策: 目标是 192.168.1.100（任意源地址）
    ↓
无策略路由/fwmark → 只能靠默认路由        ← 错误：忽略了具体路由的最长前缀匹配
    ├─ 默认路由 → NIC 3 (loopback) ❌ 死循环
    ├─ 默认路由 → NIC 1 (TUN) ❌ 去程包也走 TUN，死循环
    └─ 默认路由 → NIC 2 (Mesh) ❌ 回程包又出 mesh，死循环
```

**根本矛盾（已证伪）**：
- ~~去程和回程需要不同的出口 NIC，但两者的目标地址都不在 mesh 网段内，都靠默认路由~~
  → 回程 dst 属于 LAN 前缀，为回程前缀添加具体路由（`192.168.0.0/16 → NIC 1`）即可，
  最长前缀匹配优先于默认路由，去程/回程互不干扰
- ~~gVisor 无策略路由，无法根据"包从哪个 NIC 来"选择路由表~~
  → 本场景无需按入站 NIC 区分，按目标前缀即可完成选路

### 架构决策：NAT 必须在 TUN 边界（gVisor 外部）

> **⚠️ 2026-10-03 修订**：本决策依据的"死结"已证伪（见上节勘误）。栈内 NAT 可行，
> 本决策降级为"**当前边界架构下的合理选择**"——nat.go 已在生产验证、无需 fork，
> 短期架构继续保留；但它不再是技术必然。修订后的分析见下节。

**方案**（原文，仍为当前边界架构的实际实现）：在 TUN 入口/出口做 NAT，保证进入 gVisor 的包源地址已经是 VIP。

```
LAN 机器 (src=192.168.1.100) → TUN
    ↓
readLoop: nat.go.TranslateOutbound (src → VIP)
    ↓
gVisor 收到 src=VIP 的包（不需要 NAT）
    ↓
gVisor 转发到 mesh（从 NIC 2 发出）
    ↓
回程包 dst=VIP 到达 gVisor
    ↓
gVisor 转发到 TUN（从 NIC 1 发出）
    ↓
writeLoop: nat.go.TranslateInbound (dst=VIP → 原始源)
    ↓
TUN → 宿主机 → LAN 机器
```

**优势**：
1. gVisor 完全不需要做 NAT——包进出都是 VIP，路由无歧义
2. NAT 状态管理在 nat.go（栈外），简单直接
3. 不需要 gVisor 的 IPTables/ConnTrack 参与 NAT
4. ~~绕过了"DNAT 必须在路由前 + 路由后无法正确路由"的死结~~（该死结已证伪，见上节勘误）

**本质**：writeLoop + 自定义 NAT 就是一个手写的策略路由器，只是实现位置在 gVisor 外面。gVisor 缺的不是策略路由的"能力"，而是策略路由的"集成点"——它没有暴露让我们注入自定义路由决策的 hook。（2026-10-03 注：本段依据的"死结"已证伪；对 NAT 场景"无集成点"不成立——IPTables 五钩子即集成点，且本场景无需按入站 NIC 区分。集成点缺失仅对 FindRoute 连接 id 限定的选路场景成立。）

### 修订后的架构决策：栈内 NAT 可行（2026-10-03 源码核实）

针对 pinned 版本 v0.0.0-20250428193742-2d800c3129d5 的逐行核实结果：

1. **PREROUTING 在路由判定前执行**：`network/ipv4/ipv4.go:873`（`HandlePacket` 入口）调用
   `CheckPrerouting`，之后才进 `handleValidatedPacket` 判定本地交付/转发；官方注释明确
   允许钩子改写包内容（ipv4.go:879 "CheckPrerouting can modify the backing storage of the
   packet"）。五钩子齐备：Output(:536)、Postrouting(:575)、Forward(:685/:785)、Input(:1228)。
2. **NAT target 全套可用**（`stack/iptables_targets.go`）：DNATTarget(:186，限 Prerouting/Output)、
   SNATTarget(:281)、**MasqueradeTarget**(:381，限 Postrouting，经 snatAction 做端口分配)、
   RedirectTarget(:240)。
3. **conntrack 覆盖 nat.go 的全部状态管理**：动态端口分配、ICMP 差错报文内嵌包还原
   （conntrack.go `getHeaders` 的 `isICMPError` 分支）、ICMP echo ident 跟踪
   （conntrack.go:73-96，`srcPortOrEchoRequestIdent`/`dstPortOrEchoReplyIdent`）。
   `mesh/nat.go` 的 558 行可整体由 IPTables 规则 + conntrack 替代。
4. **回程选路无需策略路由**：DNAT 还原后的 dst 属于 LAN 前缀，路由表添加具体路由
   （如 `192.168.0.0/16 → TUN NIC`），按最长前缀匹配直达 TUN。

**修订后定位**：
- 栈内 NAT **无需 fork**——IPTables 是公开 Go API（`stack.Options.IPTables` / `s.IPTables()`），
  在未修改的 gVisor 上即可配置规则
- 真正需要 fork 的只有两处：
  ① 选路语义——FindRoute 连接 id 限定 + 默认路由直接匹配抢占转发兜底（TCP 回程自环故障根因）；
  ② 转发封装框架——IPIP 等隧道封装无上游抽象（可用自定义 Target 或路由级封装回调补齐）

**PoC 时需运行时验证（当前均为源码阅读结论）**：
- MasqueradeTarget 的源地址取自出 NIC 的 `AcquireOutgoingPrimaryAddress`——若用 masquerade，
  VIP 需作为 primary 地址绑定在出 NIC；改用 SNATTarget 直接指定 VIP 则无此要求
- conntrack/NAT 规则需显式配置（默认空表全放行，无副作用）
- loopback 类型 NIC 跳过 PREROUTING（ipv4.go:866 "Loopback traffic skips the prerouting chain"），
  环回注入路径如需 NAT 需注意此行为

### gVisor 包处理流程源码分析（2026-09-17）

#### 两次地址检查

gVisor 在包处理时有两次地址检查，语义不同：

**第一次检查（per-NIC）**：`handleValidatedPacket` (ipv4.go:1171)
```go
if addressEndpoint := e.AcquireAssignedAddress(dstAddr, e.nic.Promiscuous(), ...); addressEndpoint != nil {
    e.deliverPacketLocally(h, pkt, inNICName)  // 本地交付
} else if e.Forwarding() {
    e.handleForwardingError(e.forwardUnicastPacket(pkt))  // 转发
}
```
- 检查目标地址是否属于**当前 NIC**
- 第二个参数 `e.nic.Promiscuous()` 决定是否允许临时 endpoint
- 如果当前 NIC 是混杂模式，接受任何目标地址

**第二次检查（全局）**：`forwardUnicastPacket` (ipv4.go:782)
```go
if ep := e.protocol.findEndpointWithAddress(dstAddr); ep != nil {
    ep.handleValidatedPacket(h, pkt, e.nic.Name())  // 转发到目标 NIC
    return nil
}
// 否则外部路由
r, err := stk.FindRoute(...)
```
- 检查目标地址是否属于**任何 NIC**
- 只在开启转发时执行
- `findEndpointWithAddress` 调用 `AcquireAssignedAddress(addr, false /* allowTemp */, ...)`
- **关键**：`allowTemp=false`，**不考虑混杂模式**

#### 混杂模式的作用范围

**混杂模式只影响入站包（从链路层到达），不影响转发包**：

1. **入站包**（从链路层到达）：
   - `HandlePacket` → `handleValidatedPacket`
   - 检查混杂模式（`e.nic.Promiscuous()`）
   - 如果混杂模式，接受任何目标地址

2. **转发包**（从其他 NIC 转发过来）：
   - `forwardUnicastPacket` → `FindRoute` 找到输出 NIC
   - `forwardPacketWithRoute` → `forwardToEp.writePacketPostRouting`
   - 直接调用输出 NIC 的链路层 `WritePackets`
   - **不经过** `handleValidatedPacket`，**不检查**混杂模式

#### 关键发现

- `findEndpointWithAddress` 使用 `allowTemp=false`，只检查显式分配的地址
- 混杂模式不影响转发路径，只影响从链路层直接到达的包
- NIC 3 是混杂模式不会导致转发包被错误接收

#### 设计影响

NIC 3（Loopback）设计为混杂模式是安全的：
- 从 NIC 2 转发的包到 NIC 3，走 `writePacketPostRouting` → `WritePackets`
- 不会被 NIC 3 的混杂模式当做本地包接收
- 混杂模式让 NIC 3 能接受从链路层直接到达的任意目标地址包（用于环回场景）

### 结论（2026-10-03 修订）

**短期保留 nat.go 现状**。它虽然在栈外手写，但工作在正确的执行层（TUN 边界），协议覆盖完整（含 ICMP 错误消息翻译），且已在生产验证。当前边界架构（单 NIC / 2-NIC 方向）继续使用。

**但"gVisor 的能力边界到此为止"已被证伪**（见上节勘误）：栈内 NAT 能力齐备（五钩子 + conntrack + 全套 NAT target，含 ICMP echo/差错翻译），"NAT 必须在 TUN 边界"是当时路由分析疏漏导致的误判，并非 gVisor 的能力限制。gVisor 真正缺失的只有两处：

1. **选路语义**：FindRoute 连接 id 限定 + 默认路由直接匹配抢占转发兜底（TCP 回程自环故障根因，需 fork 修补）
2. **转发封装框架**：IPIP 等隧道封装无上游抽象（可用自定义 Target/封装回调补齐）

其余能力（NAT、conntrack、ICMP 翻译）上游均已提供。

## 充分利用 gVisor 的 NIC 规划

基于以上调研结论，设计充分利用 gVisor 能力的多 NIC 架构。

### NIC 规划

| NIC | 地址绑定 | 混杂模式 | 职责 |
|-----|----------|----------|------|
| **NIC 1 (TUN)** | 无 | 否 | TUN 适配器 I/O，NAT 在边界（readLoop/writeLoop） |
| **NIC 2 (Mesh)** | GIP (.3) | **否** | mesh 流量 + DNS/Proxy socket 源地址 |
| **NIC 3 (Loopback)** | 无 | **是** | 默认路由环回 → Forwarder |
| **NIC 100+ (h_tunnel)** | 无 | 否 | h_tunnel 代理出站绑定（SO_BINDTODEVICE） |

**关键设计**：
- **VIP 不绑定**：VIP 只是 NAT 转换地址（SNAT 源 / DNAT 目标），不绑定到任何 NIC，避免 gVisor 认为是本地包
- **NIC 2 非混杂**：只接收目标为 GIP 或 mesh 网段的包，其他包走路由
- **NIC 3 混杂 + 手动实现**：环回 endpoint，收到出站包后环回到入站，让 Forwarder 处理
- **h_tunnel NIC**：uplink-only，仅用于 SO_BINDTODEVICE，不接收入站流量

### 路由表

```go
s.SetRouteTable([]tcpip.Route{
  {Destination: vipAddr, NIC: 1},         // VIP → NIC 1 (回程 NAT)
  {Destination: "10.0.0.0/8", NIC: 2},    // mesh 网段 → NIC 2
  {Destination: defaultRoute, NIC: 3},    // 0.0.0.0/0 → loopback
})
```

### 关键配置

```go
// 1. 开启 IP 路由转发
s.SetForwardingDefaultAndAllNICs(ipv4.ProtocolNumber, true)

// 2. NIC 1 (TUN): 不绑定地址，不开混杂
s.CreateNIC(1, tunEndpoint)
// 不 AddProtocolAddress，不 SetPromiscuousMode

// 3. NIC 2 (Mesh): 绑定 GIP，不开混杂
s.CreateNIC(2, meshEndpoint)
s.AddProtocolAddress(2, gipAddr, ...)  // GIP = .3

// 4. NIC 3 (Loopback): 不绑定地址，开混杂，手动实现
s.CreateNIC(3, loopbackEndpoint)
s.SetPromiscuousMode(3, true)

// 5. NIC 100+ (h_tunnel): 不绑定地址，开 spoofing
s.CreateNIC(100+i, htunnelEndpoint)
s.SetSpoofing(100+i, true)
```

### Loopback Endpoint 实现

手动实现环回 endpoint，负责：
1. **IPIP 封装**：非 mesh 流量（通告路由）进行 IPIP 封装
2. **环回**：封装后的包或本地交付的包环回到入站

```go
type LoopbackEndpoint struct {
    mu             sync.RWMutex
    dispatcher     stack.NetworkDispatcher
    mtu            uint32
    meshEndpoint   *MeshEndpoint      // 持有 mesh endpoint 引用
    ipipTunnel     *IPIPTunnel        // IPIP 隧道处理器
    staticRoutes   []StaticRoute      // 静态路由（决定哪些目标需要 IPIP）
}

func (e *LoopbackEndpoint) WritePackets(pkts stack.PacketBufferList) (int, tcpip.Error) {
    e.mu.RLock()
    d := e.dispatcher
    e.mu.RUnlock()
    
    if d == nil {
        return 0, &tcpip.ErrInternal{}
    }
    
    for _, pkt := range pkts.AsSlice() {
        data := pkt.ToBuffer().Flatten()
        dstIP := getDstIP(data)
        
        // 1. 检查是否需要 IPIP 封装（匹配静态路由）
        if targetEIP := e.getTargetEIP(dstIP); targetEIP != nil {
            // 非 mesh 流量：IPIP 封装后通过 mesh endpoint 发送
            localEIP := e.ipipTunnel.GetLocalEIP()
            encapsulated, err := e.ipipTunnel.Encapsulate(localEIP, targetEIP, data)
            if err != nil {
                continue
            }
            // 直接调用 mesh endpoint 发送，不环回
            e.meshEndpoint.SendRawPacket(encapsulated)
            continue
        }
        
        // 2. 不需要封装：环回到入站（让 Forwarder 或本地 socket 处理）
        pkt.IncRef()
        d.DeliverNetworkPacket(pkt.NetworkProtocolNumber, pkt)
    }
    return pkts.Len(), nil
}

// getTargetEIP 检查目标地址是否匹配静态路由，返回出口节点的 EIP
func (e *LoopbackEndpoint) getTargetEIP(dstIP net.IP) net.IP {
    for _, route := range e.staticRoutes {
        if route.Contains(dstIP) {
            return route.EgressEIP
        }
    }
    return nil
}
```

**关键设计**：
- IPIP 封装在 NIC 3 完成，**不在 TUN 入口拦截**
- 封装后直接调用 `meshEndpoint.SendRawPacket`，不经过环回
- 只有需要本地交付的包才环回（`DeliverNetworkPacket`）
- NIC 3 统一处理所有出站逻辑（IPIP 封装 + 环回）

### IPIP 出口节点选择

IPIP 封装前需要选择出口节点（egress node），算法如下：

```go
func selectEgressNodeID(targetIP net.IP, entries []RouteEntry) string {
    // 1. 排序：静态路由优先，动态路由在后
    sort.SliceStable(sorted, func(i, j int) bool {
        return sorted[i].Source == RouteSourceStatic && sorted[j].Source == RouteSourceDynamic
    })
    
    // 2. 排除自身节点
    available := filter(entries, e => e.NodeID != m.nodeID)
    
    // 3. Sticky 缓存：如果之前选过的节点还可用，继续使用
    if cachedNodeID, ok := m.stickyCache[targetIP]; ok {
        if contains(available, cachedNodeID) {
            return cachedNodeID
        }
    }
    
    // 4. Hash 稳定选择：基于 targetIP 的 hash 选择节点
    hash := hashIP(targetIP)
    idx := hash % len(available)
    selectedNodeID := available[idx].NodeID
    
    // 5. 缓存选择结果
    m.stickyCache[targetIP] = selectedNodeID
    return selectedNodeID
}
```

**算法特点**：
- **静态优先**：静态配置的出口节点优先于动态通告的
- **Sticky**：同一目标 IP 尽量使用同一出口节点（会话保持）
- **稳定**：基于目标 IP hash，相同目标总是选择相同节点（除非节点不可用）
- **排除自身**：不会选择自己作为出口节点

**路由来源**：
- **静态路由**：配置文件中指定的 `mesh.static_routes`
- **动态路由**：mesh 网络中其他节点通告的路由前缀

**示例配置**：
```yaml
mesh:
  static_routes:
    - prefix: "8.8.8.0/24"
      node_ids: ["gg", "qg"]  # 优先使用 gg，gg 不可用时用 qg
```

### 数据流

**出站（旁路网关，需要 NAT + IPIP 封装）**：
```
LAN 机器 (src=192.168.1.100, dst=8.8.8.8) → TUN
  ↓
readLoop: nat.go.TranslateOutbound (src → VIP)
  ↓
InjectInbound NIC 1 (src=VIP, dst=8.8.8.8)
  ↓
gVisor 路由: dst=8.8.8.8 → 默认路由 → NIC 3
  ↓
NIC 3 WritePackets:
  - 匹配静态路由 → IPIP 封装 (outer: src=localEIP, dst=egressEIP)
  - 调用 meshEndpoint.SendRawPacket(封装后的包)
  ↓
mesh endpoint → P2P 链路 → 出口节点
  ↓
出口节点 decapsulate → 转发到 8.8.8.8
```

**出站（mesh 内部流量，无 IPIP）**：
```
TUN (src=任意, dst=100.x.x.x) → InjectInbound NIC 1
  ↓
gVisor 路由: dst=100.x.x.x → NIC 2
  ↓
mesh endpoint → hop 表 → P2P 链路
```

**出站（通告路由，需要 IPIP 封装）**：
```
本地应用 (src=GIP, dst=8.8.8.8) → socket → gVisor
  ↓
gVisor 路由: dst=8.8.8.8 → 默认路由 → NIC 3
  ↓
NIC 3 WritePackets:
  - 匹配静态路由 → IPIP 封装
  - meshEndpoint.SendRawPacket(封装后的包)
  ↓
mesh P2P 链路 → 出口节点 → decapsulate → 8.8.8.8
```

**回程（mesh 响应，dst=GIP）**：
```
mesh 响应 (dst=GIP) → NIC 2 入站
  ↓
gVisor 路由: dst=GIP (绑定在 NIC 2) → 本地交付
  ↓
Forwarder → socket → 响应
```

**回程（mesh 响应，dst=VIP）**：
```
mesh 响应 (dst=VIP) → NIC 2 入站
  ↓
gVisor 检查: VIP 未绑定，查路由表
  ↓
路由: VIP → NIC 1
  ↓
转发到 NIC 1 → writeLoop 读取
  ↓
writeLoop: nat.go.TranslateInbound (dst=VIP → 原始源)
  ↓
写 TUN → 宿主机 → LAN 机器
```

### gVisor 承担的职责

**充分利用的能力**：
- ✅ IP 路由转发（NIC 间转发）
- ✅ TCP/UDP Forwarder（代理连接）
- ✅ ICMP 处理（TTL 递减、Time Exceeded 生成）
- ✅ Socket 管理（DNS/Proxy socket）
- ✅ 路由表管理（按目标地址路由）

**保留在 gVisor 外部**：
- ❌ NAT（在 TUN 边界，readLoop/writeLoop）
- ❌ 策略路由（gVisor 不支持，用多 NIC + 路由表模拟）
- ❌ mesh hop 表路由（在 mesh endpoint 内部）

### 与 h_tunnel 的兼容性

h_tunnel NIC（NIC 100+）与此架构完全兼容：
- h_tunnel NIC 是 uplink-only（Attach 是 no-op）
- 下行流量通过主 mesh NIC（NIC 2）返回
- h_tunnel NIC 仅用于 SO_BINDTODEVICE，确保出站流量走正确的 h_tunnel 代理
- 不干扰 NIC 1/2/3 的路由和转发逻辑

## 演进目标架构

### 核心理念
所有路由决策集中在 gVisor 路由表，而不是分散在代码各处。

### 架构设计
```
readLoop (从 TUN 读取)
   ↓
全部注入 gVisor netstack
   ↓
gVisor 路由表决策
   ├─ TUN NIC → 回操作系统（本地交付）
   ├─ mesh NIC → mesh endpoint → P2P 链路
   └─ proxy NIC → proxy endpoint → 代理转发
   ↓
writeLoop (从 gVisor 读取)
   ↓
根据 NIC 类型发送到对应目标
```

### 关键组件

#### 1. 多个 NIC 定义
- **TUN NIC**：与操作系统交互，接收/发送本地数据包
- **mesh NIC**：mesh 网络接口，处理 P2P 路由、中继、failover
- **proxy NIC**：代理接口，处理 TCP/UDP 转发

#### 2. 动态路由表配置
根据 mesh 链路通告动态配置路由规则：
```go
// 示例：mesh 链路通告
{
  "destination": "100.179.0.0/16",
  "gateway": "100.179.0.1",  // GG 节点
  "metric": 1,
  "interface": "mesh0"
}

// gVisor 路由表自动更新
s.SetRouteTable([]tcpip.Route{
  {Destination: subnet, Gateway: gateway, NIC: meshNICID},
})
```

#### 3. 自定义 Endpoint
对于 mesh 特有的逻辑，需要自定义 endpoint：
- P2P 选路算法
- 中继逻辑
- Failover 机制
- 链路质量评估

### 收益

#### 心智模型简化
1. **TTL 自动处理**：gVisor 自动递减 TTL、生成 ICMP Time Exceeded
2. **路由集中化**：所有路由决策在 gVisor 路由表，不需要在代码多处判断
3. **标准 IP 行为**：符合标准网络协议，可以用标准工具调试
4. **减少代码重复**：不需要在 readLoop、writeLoop、mesh forward 重复路由逻辑

#### 可维护性提升
1. **调试更容易**：可以用 `ip route` 等工具查看路由表
2. **测试更简单**：路由逻辑由 gVisor 保证正确性
3. **扩展更容易**：新增路由规则只需要更新路由表

### 成本与风险

#### 实现成本
- **预估工期**：2-4 周
- **主要工作**：
  1. 定义多个 NIC 和 endpoint（1 周）
  2. 实现动态路由表配置（3-5 天）
  3. 实现 mesh endpoint（P2P 选路、中继、failover）（1 周）
  4. 迁移现有逻辑到 endpoint（3-5 天）
  5. 测试和调试（3-5 天）

#### 风险
1. **迁移风险**：现有系统稳定运行，迁移可能引入新问题
2. **性能风险**：gVisor 路由性能需要验证
3. **兼容性风险**：需要确保现有功能（代理、TUN、mesh）全部正常

### 迁移策略

#### 阶段 1：并行运行（1 周）
- 保留现有路由逻辑
- 新增 gVisor 路由表配置
- 双写模式：同时走新旧路径
- 对比结果，验证正确性

#### 阶段 2：逐步切换（2 周）
- 先切换简单场景（本地路由）
- 再切换 mesh 路由
- 最后切换代理转发
- 每个阶段充分测试

#### 阶段 3：清理旧代码（1 周）
- 删除旧的路由逻辑
- 更新文档
- 性能优化

## 网络设计洞察：TX/RX 逻辑分离

### 本质

网络通信从逻辑设计上，**双向就是分开的**：

```
发送方向（TX）：
  应用 → 传输层 → 网络层 → 链路层 → 物理层 → 线缆

接收方向（RX）：
  线缆 → 物理层 → 链路层 → 网络层 → 传输层 → 应用
```

这是两条**完全独立**的数据通路：
- 各自的缓冲区
- 各自的处理流程
- 各自的状态机

### 硬件体现

网卡从一开始就有独立的：
- TX DMA / TX 队列 / TX 描述符环
- RX DMA / RX 队列 / RX 描述符环
- 独立的中断、独立的流控

### 物理介质

即使物理上共享介质，逻辑上也是分离的：
- **以太网电口**：RJ45 网线 8 根线，1000BASE-T 用 4 对线，每对全双工
- **光纤 duplex**：一根光纤跳线里实际是两根纤芯，TX 和 RX 分开
- **特殊场景**：单向网关（data diode）物理上分离 TX/RX，用于高安全场景

### 设计启示

物理网卡的入站和出站可以设计成**两个独立逻辑链路**：

```
物理 mesh 链路（P2P）
    ↓
┌─────────────────────────────────┐
│  逻辑上拆成两个独立链路：         │
│                                 │
│  NIC 2 (入站/RX)：              │
│    - 从 P2P 接收包              │
│    - 注入 gVisor                │
│    - GIP 绑定在此，本地交付      │
│                                 │
│  NIC 3 (出站/TX)：              │
│    - gVisor 路由到 NIC 3        │
│    - IPIP 封装（如需要）        │
│    - 调用 mesh endpoint 发送    │
│    - 通过 P2P 发出              │
└─────────────────────────────────┘
    ↓
同一个物理 P2P 链路
```

### 优势

1. **职责分离**：入站和出站可以有不同的处理逻辑
2. **灵活路由**：入站和出站可以走不同的路径
3. **独立扩展**：可以对入站和出站分别做优化、监控、限流
4. **符合本质**：网络通信本来就是两个单工通道拼成全双工

### 实际案例

这种设计在很多网络设备中已有应用：
- Linux 的 `ifb` (Intermediate Functional Block) 用于入站流量控制
- 策略路由可以基于入站/出站接口做不同决策
- 防火墙规则区分 `-i` (入站接口) 和 `-o` (出站接口)

## 测试环境

### 现有环境

| 环境 | 地址 | 角色 | Mesh VIP | 用途 |
|------|------|------|----------|------|
| **GG** | 106.13.183.103 | mesh 节点 | 100.179.0.1 | 主节点，IPIP 出口测试 |
| **QG** | 10.11.61.40 | mesh 节点 | 100.64.0.1 | TUN + 旁路网关（生产） |
| **VM** | 10.21.20.65 | mesh 节点 | 100.64.1.1 | Windows TUN 测试 |
| **JF** | 36.140.28.178 | h_tunnel 服务端 | 100.2.0.1 | h_tunnel 测试 |
| **MS9** | 10.161.88.9 | mesh 节点 | 100.189.0.1 | 多节点路由测试 |
| **MS10** | 10.161.88.10 | mesh 节点 | 100.96.0.1 | 多节点路由测试 |

### QGT 环境（多 NIC 架构验证专用）

**定位**：QG 的测试版本，专门用于验证多 NIC 架构升级。

**为什么需要 QGT**：
- 多 NIC 架构是内部重构，节点间 mesh 协议**完全不变**
- 只需要验证内部路由、NAT、IPIP 封装逻辑正确
- QGT 验证通过后，可以安全升级到 QG 生产环境

**QGT 环境配置**：
```yaml
# QGT 环境（独立于 QG 生产，同一台机器）
node_id: "qgt"
mesh:
  subnet: "100.65.0.0/16"  # 独立子网，避免与 QG 冲突
  vip: "100.65.0.1"
  gip: "100.65.0.3"
  eip: "100.65.0.4"

tun:
  enabled: false  # ⚠️ 不能启用 TUN，避免与 QG 冲突
  bypass_gateway: false

# 多 NIC 架构配置
experimental:
  multi_nic: true  # 启用多 NIC 架构
```

**⚠️ 重要限制**：
- QGT 和 QG 在同一台物理机（10.11.61.40）上
- **不能启用 TUN**，否则会冲突（TUN 设备、iptables 规则）
- 因此**不能测试旁路网关 NAT**
- 只能验证多 NIC 架构的内部逻辑（路由、IPIP 封装、mesh 转发）

**验证目标**：

1. **基础连通性**：
   - [ ] QGT ↔ GG mesh 连通
   - [ ] QGT ↔ MS9/MS10 mesh 连通
   - [ ] QGT mesh DNS 解析正常

2. **多 NIC 路由**：
   - [ ] mesh 流量：socket → NIC 2 → P2P
   - [ ] IPIP 流量：socket → NIC 3 → 封装 → mesh endpoint → P2P
   - [ ] 回程流量：P2P → NIC 2 → gVisor 路由 → socket

3. **IPIP 封装**：
   - [ ] 通告路由（如 8.8.8.0/24）触发 IPIP 封装
   - [ ] 出口节点选择正确（static 优先、sticky）
   - [ ] GG 正确 decapsulate

4. **h_tunnel 兼容性**：
   - [ ] h_tunnel 代理正常工作
   - [ ] SO_BINDTODEVICE 绑定正确
   - [ ] 下行流量通过 mesh NIC 返回

**⚠️ 无法在 QGT 验证的功能**：
- ❌ TUN 相关功能（与 QG 冲突）
- ❌ 旁路网关 NAT（需要 TUN + iptables）
- ❌ 这些功能需要在独立环境（如 VM）验证

**部署步骤**：

```bash
# 1. 在 QG 服务器上新建 QGT 目录
ssh root@10.11.61.40 "mkdir -p /root/qgt && cp /root/config.yaml /root/qgt/"

# 2. 修改 QGT 配置（独立 subnet、node_id）
# 编辑 /root/qgt/config.yaml

# 3. 编译多 NIC 架构版本
make linux

# 4. 上传到 QGT
scp dist/linux-amd64/phaethon root@10.11.61.40:/root/qgt/phaethon

# 5. 启动 QGT（独立进程，不影响 QG 生产）
ssh root@10.11.61.40 "cd /root/qgt && nohup ./phaethon > phaethon.log 2>&1 &"
```

**验证命令**：

```bash
# 在 QGT 上测试
ssh root@10.11.61.40 "cd /root/qgt && ./phaethon --config config.yaml"

# 测试 mesh 连通性
ping 100.179.0.1  # GG
ping 100.189.0.1  # MS9

# 测试 IPIP 封装（如果有通告路由）
curl --interface 100.65.0.3 http://8.8.8.8

# 查看日志
tail -f /root/qgt/phaethon.log | grep -E "NIC|IPIP|NAT"
```

**升级路径**：

```
QGT 验证通过
  ↓
合并多 NIC 架构到 master
  ↓
QG 生产环境升级（停机窗口）
  ↓
QG 验证通过
  ↓
其他环境（VM、MS9、MS10）逐步升级
```

**风险控制**：
- QGT 和 QG 生产完全独立，互不影响
- 节点间协议不变，QGT 可以与其他环境的节点正常通信
- 如果 QGT 验证失败，不影响 QG 生产
- 回滚简单：停止 QGT 进程，恢复 QG 即可

### IPIP 测试场景

**场景 1：GG 作为 IPIP 出口**
```
QG (src=VIP, dst=8.8.8.8) → mesh → GG
  ↓
GG 收到 IPIP 包 (outer dst=GG EIP)
  ↓
GG decapsulate → 转发到 8.8.8.8
```

**场景 2：多出口节点选择**
```yaml
# QG 配置
mesh:
  static_routes:
    - prefix: "8.8.8.0/24"
      node_ids: ["gg", "ms9"]  # gg 优先，ms9 备用
```

**测试命令**：
```bash
# 在 QG 上测试 IPIP 路由
curl --interface 100.64.0.3 http://8.8.8.8  # 通过 mesh VIP 发起请求

# 查看 IPIP 封装日志
tail -f /root/phaethon.log | grep "IPIP encapsulated"

# 在 GG 上查看 decapsulate 日志
ssh layer4@106.13.183.103 "tail -f /home/layer4/phaethon-gg/phaethon.log | grep IPIP"
```

### 验证点

1. **IPIP 封装正确性**：
   - outer src = 本地 EIP（subnet + 4）
   - outer dst = 出口节点 EIP
   - inner packet 保持不变

2. **出口节点选择**：
   - 静态路由优先
   - Sticky 缓存生效
   - 节点故障时自动切换

3. **路由路径**：
   - mesh 流量：NIC 1 → NIC 2 → P2P
   - IPIP 流量：NIC 1 → NIC 3 → IPIP 封装 → mesh endpoint → P2P
   - 回程流量：P2P → NIC 2 → gVisor 路由 → NIC 1

## 当前优先级

**短期**：手动实现 traceroute（1-2 天）
- 在 readLoop 和 HandleMeshFrame 手动递减 TTL
- TTL=0 时手动生成 ICMP Time Exceeded
- 快速解决 traceroute 功能

**长期**：gVisor 路由栈演进（2-4 周）
- 作为架构优化项目推进
- 需要充分测试和验证
- 建议在功能稳定期实施

## 参考文档

- [gVisor ICMP TTL 调研](./gvisor_icmp_ttl_research.md)
- [Mesh Traceroute 设计](./mesh_traceroute_design.md)
