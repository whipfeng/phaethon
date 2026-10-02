# 多 NIC 架构设计

> 版本: v1.0.0
> 日期: 2026-10-01
> 状态: IMPLEMENTING (Phase 3 部分完成)
> 整合自: gvisor_routing_evolution.md, multi_nic_implementation_plan.md, mesh_ipip_smart_routing.md

## 概述

将当前单 NIC 架构重构为多 NIC 架构，充分利用 gVisor 的路由能力，减少心智模型复杂度。

## 目标架构

### NIC 规划

| NIC | 地址绑定 | 混杂模式 | Spoofing | 职责 | 状态 |
|-----|----------|----------|----------|------|------|
| **NIC 1 (TUN)** | 无 | **否** | **是** | TUN 适配器 I/O，NAT 在边界（readLoop/writeLoop），VIP 路由用于回程 IP 转发。**Spoofing**：Forwarder 回包从 NIC 1 出去（hostIP 路由），源地址是外部 IP | ✅ 已实现 |
| **NIC 2 (Mesh)** | GIP (.3) | **否** | **否** | mesh 流量入口/出口，DNS/Proxy socket 源地址 | ✅ 已实现 |
| **NIC 3 (Loopback)** | 无 | **是** | **是** | **双重职责**：(1) 默认路由环回 → Forwarder（代理连接），(2) IPIP 封装（智能选路） | ✅ 已实现 |
| **NIC 100+ (h_tunnel)** | 无 | 否 | 是 | h_tunnel 代理出站绑定（SO_BINDTODEVICE） | ✅ 已存在 |

**关键设计**：
- **VIP 不绑定**：VIP 只是 NAT 转换地址（SNAT 源 / DNAT 目标），不绑定到任何 NIC。VIP 路由用于回程流量的 IP 转发（NIC 2 → NIC 1），不是本地交付
- **NIC 2 非混杂**：只接收目标为 GIP 或 mesh 网段的包
- **NIC 3 混杂 + Spoofing + 双重职责**：
  - **混杂模式**：让 NIC 3 接收目标地址不是自己的包（环回包的目标是外部 IP）
  - **Spoofing**：让 Forwarder 用外部 IP 作为源地址回 SYN-ACK（如 SYN 到 219.159.26.41:443，Forwarder 回 SYN-ACK 时 src=219.159.26.41）
  - **职责 1（环回 → Forwarder）**：默认路由的出站包环回到入站，利用混杂模式实现本地交付，触发 TCP/UDP Forwarder（代理连接的核心机制）
  - **职责 2（IPIP 封装）**：匹配静态路由的出站包执行 IPIP 封装，通过 mesh 网络发送到出口节点（智能选路）

### 路由表

```go
s.SetRouteTable([]tcpip.Route{
  {Destination: vipAddr/32, NIC: 1},      // VIP (.1, 单个 IP) → NIC 1 (回程 NAT)
  {Destination: hostIP/32, NIC: 1},       // hostIP (.2, 单个 IP) → NIC 1
  {Destination: "100.64.0.0/10", NIC: 2}, // mesh 网段 → NIC 2
  {Destination: defaultRoute, NIC: 3},    // 0.0.0.0/0 → loopback
})
```

**关键**：
- **VIP (.1) 和 hostIP (.2) 都是单个 IP**（/32），不是子网！
- 这两个 IP 路由到 NIC 1，用于回程流量的 IP 转发
- **100.64.0.0/10** 是整个 mesh 网络，包括所有 fake IP
- **路由优先级**：VIP/hostIP (/32) > mesh (/10) > default (/0)

**当前实现**：
```go
// ❌ 不完整：只有默认路由，缺少 VIP 和 mesh 网段路由
routes := []tcpip.Route{
    {Destination: header.IPv4EmptySubnet, NIC: 3},
    {Destination: header.IPv6EmptySubnet, NIC: 3},
}
```

## 实现阶段

### Phase 1: LoopbackEndpoint 基础框架 ✅ 已完成

**文件**：`mesh/loopback_endpoint.go`

**实现内容**：
- 实现 `stack.LinkEndpoint` 接口
- `WritePackets`：接收出站包，环回到入站
- 添加统计信息收集

**验证**：NIC 3 创建成功，混杂模式配置正确

### Phase 2: Netstack 初始化更新 ✅ 已完成

**改动**：
- 在 `initStack` 中创建 NIC 3 (LoopbackEndpoint)
- 配置混杂模式
- 添加 `LoopbackEP()` getter 方法

**问题**：NIC 1 错误地开启了混杂模式和 spoofing（应关闭）

### Phase 3: IPIP 封装迁移到 NIC 3 ✅ 已完成

**改动**：
- 在 `LoopbackEndpoint.WritePackets` 中添加 IPIP 封装逻辑
- 实现 `extractDstIP`、`needsIPIPEncapsulation`、`encapsulatePacket`、`sendViaMesh` 方法
- 添加 `SetIPIPConfig` 方法配置 IPIP 参数
- 在 `MeshManager` 添加公共方法：`SelectEgressNodeIDForIP`、`SendEncapsulatedPacket`

**实现细节**：
- 使用 `pkt.ToBuffer().Flatten()` 获取原始包数据
- 使用 `config.MeshStaticRoute` 匹配静态路由
- 调用 `meshMgr.SelectEgressNodeIDForIP` 选择出口节点和 EIP
- 调用 `tunnel.Encapsulate` 执行 IPIP 封装
- 调用 `meshMgr.SendEncapsulatedPacket` 发送封装后的包

**修复的问题**：
- "consume twice" panic：创建新 packet buffer 避免重复消费

**当前状态**：
- ✅ 默认路由已改到 NIC 3
- ✅ IPIP 封装逻辑已实现并激活
- ✅ 代码编译通过
- ❌ **缺少 NIC 2，mesh 网络不通**

### Phase 4: 实现 NIC 2 (Mesh Endpoint) ✅ 已完成

**目标**：创建独立的 Mesh Endpoint 处理 mesh 流量

**实现内容**：
1. ✅ 创建 `MeshEndpoint` 结构，实现 `stack.LinkEndpoint`
2. ✅ 绑定 GIP (.3) 到 NIC 2
3. ✅ 关闭 NIC 1 的混杂模式和 spoofing
4. ✅ 实现 mesh 包的接收和发送（`DeliverNetworkPacket` 和 `SendRawPacket`）
5. ✅ 更新路由表：添加 mesh 网段路由（VIP 路由待后续完善）

**关键配置**：
```go
// NIC 1 (TUN): 关闭混杂和 spoofing
s.CreateNIC(1, tunEndpoint)
// 不 SetPromiscuousMode, 不 SetSpoofing

// NIC 2 (Mesh): 绑定 GIP，关闭混杂
s.CreateNIC(2, meshEndpoint)
s.AddProtocolAddress(2, gipAddr, ...)  // GIP = .3

// 路由表
s.SetRouteTable([]tcpip.Route{
    {Destination: meshSubnet, NIC: 2},     // mesh → NIC 2
    {Destination: defaultRoute, NIC: 3},   // default → NIC 3
})
```

**验证**：
- ✅ 代码编译通过
- ⏳ mesh 流量走 NIC 2（待测试）
- ⏳ 默认流量走 NIC 3（loopback/IPIP）（待测试）

### Phase 5: TUN NIC 独立 ❌ 待实现

**改动**：
- 将 channel.Endpoint 改为 TUN 设备 I/O
- NAT 保持在 TUN 边界

**验证**：
- TUN 功能正常
- NAT 正确

### Phase 6: QGT 环境验证 ❌ 待实现

**部署**：
- 在 QG 服务器上创建 QGT 环境
- 独立 subnet (100.65.0.0/16)
- 禁用 TUN（避免冲突）

**验证清单**：
- [ ] mesh 连通性
- [ ] 多 NIC 路由
- [ ] IPIP 封装
- [ ] h_tunnel 兼容性

## IPIP 智能选路

### 出口节点选择算法

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
- **稳定**：基于目标 IP hash，相同目标总是选择相同节点
- **排除自身**：不会选择自己作为出口节点

### 统一路由结构

静态路由和动态路由合并为统一结构：

```go
type RouteSource string

const (
    RouteSourceStatic  RouteSource = "static"
    RouteSourceDynamic RouteSource = "dynamic"
)

type RouteEntry struct {
    NodeID string
    Source RouteSource  // static | dynamic
}
```

**示例配置**：
```yaml
mesh:
  static_routes:
    - prefix: "8.8.8.0/24"
      node_ids: ["gg", "qg"]  # gg 优先，gg 不可用时用 qg
```

## 完整数据流

### 场景 1：出站 - mesh 内部流量（无 IPIP）

**场景描述**：访问 mesh 网络内的其他节点（如 gg.phn → 100.179.0.1）

```
TUN (src=任意, dst=100.x.x.x) → readLoop → InjectInbound NIC 1
  ↓
gVisor 路由: dst=100.x.x.x → mesh 网段路由 → NIC 2
  ↓
NIC 2 (MeshEndpoint) → hop 表 → P2P 链路 → 目标节点
```

**关键点**：
- 目标地址是 mesh 网段（100.64.0.0/10）
- 直接通过 NIC 2 发送，不经过 NIC 3
- 不需要 IPIP 封装

### 场景 2：出站 - 代理连接（外部流量，环回 → Forwarder）

**场景描述**：应用通过 SOCKS5/HTTP 代理访问外部网站（目标可以是域名、真实 IP 或 fake IP）

```
应用 → SOCKS5/HTTP 代理 → gVisor TCP Forwarder 创建 socket
  ↓
gVisor socket (src=GIP, dst=外部地址) → 出站
  ↓
gVisor 路由: dst=外部地址 → 默认路由 → NIC 3
  ↓
NIC 3 (LoopbackEndpoint) WritePackets:
  - 不匹配静态路由 → 环回到入站路径
  - dispatcher.DeliverNetworkPacket() 重新注入
  ↓
NIC 3 混杂模式 → 接收所有包 → 本地交付
  ↓
TCP/UDP Forwarder 接管 → 代理连接（通过 dialer）
  ↓
真实连接 → 外部服务器
```

**关键点**：
- 目标地址是外部地址（非 mesh 网段，非静态路由）
- 通过 NIC 3 环回触发 Forwarder
- NIC 3 的混杂模式确保环回的包能被本地交付
- 这是**最常见的代理场景**

### 场景 3：出站 - 通告路由（IPIP 封装）

**场景描述**：访问匹配静态路由的外部地址，需要通过特定出口节点（如 8.8.8.8 通过 GG 出口）

```
本地应用 (src=GIP, dst=8.8.8.8) → socket → gVisor
  ↓
gVisor 路由: dst=8.8.8.8 → 默认路由 → NIC 3
  ↓
NIC 3 (LoopbackEndpoint) WritePackets:
  - 匹配静态路由 → IPIP 封装
  - 选择出口节点（egress node）
  - meshEndpoint.SendRawPacket(封装后的包)
  ↓
mesh P2P 链路 → 出口节点 → decapsulate → 8.8.8.8
```

**关键点**：
- 目标地址匹配 `config.MeshStaticRoute` 中的静态路由
- NIC 3 执行 IPIP 封装而不是简单环回
- 通过 mesh 网络发送到出口节点

### 场景 4：出站 - h_tunnel 代理（NIC 100+）

**场景描述**：使用 h_tunnel 类型的代理（通过 WebSocket/TCP 隧道）

```
应用 → h_tunnel 代理 → gVisor TCP Forwarder 创建 socket
  ↓
gVisor socket bind to NIC 100+ (SO_BINDTODEVICE)
  ↓
NIC 100+ (HTunnelEndpoint) → WebSocket/TCP 连接 → h_tunnel 服务器
  ↓
h_tunnel 服务器 → 目标地址
```

**关键点**：
- h_tunnel 代理使用独立的 NIC 100+
- 通过 SO_BINDTODEVICE 绑定到特定 NIC
- 不经过 NIC 1/2/3 的路由逻辑

### 场景 5：回程 - mesh 响应（dst=VIP）

**场景描述**：mesh 内部流量的响应包（如访问 gg.phn 的响应）

```
mesh 响应 (src=100.179.0.x, dst=VIP) → NIC 2 入站
  ↓
gVisor 检查: VIP 未绑定到任何 NIC，查路由表
  ↓
路由: VIP 子网 → NIC 1
  ↓
IP 转发到 NIC 1 → writeLoop 读取
  ↓
writeLoop: nat.go.TranslateInbound (dst=VIP → 原始源 IP)
  ↓
写 TUN → 宿主机 → LAN 机器
```

**关键点**：
- 目标地址是 VIP（本机 TUN 的 NAT 地址）
- VIP 路由用于 IP 转发，不是本地交付
- writeLoop 在 TUN 边界做 DNAT

### 场景 6：回程 - 代理响应（dst=GIP）

**场景描述**：代理连接的响应包（如访问 google.com 的响应）

```
外部响应 (src=google.com, dst=GIP) → dialer 接收
  ↓
gVisor socket (已建立连接) → 入站
  ↓
gVisor: GIP 绑定到 NIC 2 → 本地交付
  ↓
TCP/UDP Forwarder → 应用
```

**关键点**：
- 目标地址是 GIP（.3，绑定到 NIC 2）
- 直接本地交付给 Forwarder
- 不需要路由转发

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

## gVisor 能力边界

### 充分利用的能力

- ✅ IP 路由转发（NIC 间转发）
- ✅ TCP/UDP Forwarder（代理连接）
- ✅ ICMP 处理（TTL 递减、Time Exceeded 生成）
- ✅ Socket 管理（DNS/Proxy socket）
- ✅ 路由表管理（按目标地址路由）

### 保留在 gVisor 外部

- ❌ NAT（在 TUN 边界，readLoop/writeLoop）
- ❌ 策略路由（gVisor 不支持，用多 NIC + 路由表模拟）
- ❌ mesh hop 表路由（在 mesh endpoint 内部）

### 关键限制

gVisor 无策略路由（ip rule / 多路由表），无法根据"包从哪个 NIC 来"选择路由表。因此 NAT 必须在 TUN 边界完成，保证进入 gVisor 的包源地址已经是 VIP。

## 当前问题

1. **VIP 路由缺失**：路由表中没有 VIP → NIC 1 的路由（回程 NAT 可能受影响）
2. **需要测试验证**：Phase 4 刚完成，需要在实际环境测试 mesh 连通性

**已完成**：
- ✅ Phase 1-4 代码实现完成
- ✅ NIC 2 (Mesh Endpoint) 已创建并绑定 GIP
- ✅ NIC 1 混杂模式和 spoofing 已关闭
- ✅ 路由表已更新（mesh subnet → NIC 2）
- ✅ InjectMeshPacket 改为注入到 NIC 2

## 下一步

1. **实现 Phase 4**：创建 MeshEndpoint，绑定 GIP，关闭 NIC 1 混杂模式
2. **完善路由表**：添加 VIP → NIC 1，mesh → NIC 2 路由
3. **测试 mesh 连通性**：验证 NIC 2 工作正常
4. **完成 Phase 5-6**：TUN 独立和 QGT 验证

## 参考文档

- [gVisor ICMP TTL 调研](./gvisor_icmp_ttl_research.md)
- [Mesh Traceroute 设计](./mesh_traceroute_design.md)
- [TUN 设计](./tun_design.md)
