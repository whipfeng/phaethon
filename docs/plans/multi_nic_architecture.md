# 多 NIC 架构设计

> 版本: v1.0.0
> 日期: 2026-10-01
> 状态: IMPLEMENTING (Phase 3 部分完成)
> 整合自: gvisor_routing_evolution.md, multi_nic_implementation_plan.md, mesh_ipip_smart_routing.md

## 概述

将当前单 NIC 架构重构为多 NIC 架构，充分利用 gVisor 的路由能力，减少心智模型复杂度。

## 目标架构

### NIC 规划

| NIC | 地址绑定 | 混杂模式 | 职责 | 状态 |
|-----|----------|----------|------|------|
| **NIC 1 (TUN)** | 无 | **否** | TUN 适配器 I/O，NAT 在边界（readLoop/writeLoop） | ❌ 当前错误开启混杂+spoofing |
| **NIC 2 (Mesh)** | GIP (.3) | **否** | mesh 流量 + DNS/Proxy socket 源地址 | ❌ **未实现** |
| **NIC 3 (Loopback)** | 无 | **是** | 默认路由环回 → Forwarder，IPIP 封装 | ✅ 已实现 |
| **NIC 100+ (h_tunnel)** | 无 | 否 | h_tunnel 代理出站绑定（SO_BINDTODEVICE） | ✅ 已存在 |

**关键设计**：
- **VIP 不绑定**：VIP 只是 NAT 转换地址（SNAT 源 / DNAT 目标），不绑定到任何 NIC
- **NIC 2 非混杂**：只接收目标为 GIP 或 mesh 网段的包
- **NIC 3 混杂 + 手动实现**：环回 endpoint，收到出站包后环回到入站或 IPIP 封装

### 路由表

```go
s.SetRouteTable([]tcpip.Route{
  {Destination: vipAddr, NIC: 1},         // VIP → NIC 1 (回程 NAT)
  {Destination: "100.64.0.0/10", NIC: 2}, // mesh 网段 → NIC 2
  {Destination: defaultRoute, NIC: 3},    // 0.0.0.0/0 → loopback
})
```

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

### Phase 4: 实现 NIC 2 (Mesh Endpoint) ❌ 待实现

**目标**：创建独立的 Mesh Endpoint 处理 mesh 流量

**实现内容**：
1. 创建 `MeshEndpoint` 结构，实现 `stack.LinkEndpoint`
2. 绑定 GIP (.3) 到 NIC 2
3. 关闭 NIC 1 的混杂模式和 spoofing
4. 实现 mesh 包的接收和发送
5. 更新路由表：添加 mesh 网段和 VIP 路由

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
    {Destination: vipSubnet, NIC: 1},      // VIP → NIC 1
    {Destination: meshSubnet, NIC: 2},     // mesh → NIC 2
    {Destination: defaultRoute, NIC: 3},   // default → NIC 3
})
```

**验证**：
- mesh 流量走 NIC 2
- VIP 回程走 NIC 1
- 默认流量走 NIC 3（loopback/IPIP）

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

## 数据流

### 出站（mesh 内部流量，无 IPIP）

```
TUN (src=任意, dst=100.x.x.x) → InjectInbound NIC 1
  ↓
gVisor 路由: dst=100.x.x.x → NIC 2
  ↓
mesh endpoint → hop 表 → P2P 链路
```

### 出站（通告路由，需要 IPIP 封装）

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

### 回程（mesh 响应，dst=VIP）

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

1. **缺少 NIC 2 (Mesh Endpoint)**：导致 mesh 网络不通
2. **NIC 1 配置错误**：开启了混杂模式和 spoofing（应关闭）
3. **路由表不完整**：缺少 VIP 和 mesh 网段路由

**根本原因**：Phase 4 未实现，只有 NIC 1 和 NIC 3，没有独立的 Mesh Endpoint。

## 下一步

1. **实现 Phase 4**：创建 MeshEndpoint，绑定 GIP，关闭 NIC 1 混杂模式
2. **完善路由表**：添加 VIP → NIC 1，mesh → NIC 2 路由
3. **测试 mesh 连通性**：验证 NIC 2 工作正常
4. **完成 Phase 5-6**：TUN 独立和 QGT 验证

## 参考文档

- [gVisor ICMP TTL 调研](./gvisor_icmp_ttl_research.md)
- [Mesh Traceroute 设计](./mesh_traceroute_design.md)
- [TUN 设计](./tun_design.md)
