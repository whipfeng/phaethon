# 流量路径分析

## 状态：✅ 已整合到 `gvisor_route_selector_architecture.md`

**本文档的流量路径分析已整合到新架构文档中，包含更详细的 IP 示例和 RouteSelector 决策流程。**

**建议**：直接阅读 `gvisor_route_selector_architecture.md` §4 完整流量路径。

---

**以下内容为历史分析，仅供参考。**

## 旁路网关完整流程（具体 IP 示例）

### 场景：LAN(<IP_QGW_LAN>) 访问外网 8.8.8.8

#### 去程（LAN → 外网）

```
[<IP_QGW_LAN>] 
      │ src=<IP_QGW_LAN>, dst=8.8.8.8
      ↓
[QG OS iptables] → [TUN/NIC1 进入 gVisor]
      │
      │ 路由决策: FindRoute(8.8.8.8) → 默认路由 → NIC3 (隧道)
      ↓
{Postrouting SNAT}
      │ src: <IP_QGW_LAN> → <IP_OTHER_023> (QG VIP)
      │ conntrack 记录: (<IP_QGW_LAN>, 8.8.8.8) ↔ (<IP_OTHER_023>, 8.8.8.8)
      ↓
[NIC3 WritePackets - 隧道 NIC]
      │ 查 mesh 路由: 0.0.0.0/0 → 出口节点 GG (EIP=<IP_OTHER_043>)
      │ IPIP 封装:
      │   outer: src=<IP_OTHER_015> (QG EIP), dst=<IP_OTHER_043> (GG EIP)
      │   inner: src=<IP_OTHER_023>, dst=8.8.8.8
      ↓
[NIC2 发送] → [mesh P2P] → [GG NIC2 进入]
                                │
                           {IPIP 协议处理器解封装}
                                │ 剥壳得到 inner: src=<IP_OTHER_023>, dst=8.8.8.8
                                ↓
                           [GG gVisor 转发] → [GG 外网接口] → [8.8.8.8]
```

#### 回程（外网 → LAN）

```
[8.8.8.8]
      │ src=8.8.8.8, dst=GG外网接口IP
      ↓
[GG gVisor]
      │ conntrack DNAT: dst → <IP_OTHER_023> (QG VIP)
      │ 目标是 mesh VIP，直接 mesh P2P 发送（无需 IPIP）
      ↓
[mesh P2P] → [QG NIC2 进入]
                  │
             {Prerouting DNAT (conntrack)}
                  │ dst: <IP_OTHER_023> → <IP_QGW_LAN>
                  ↓
             {路由决策}
                  │ FindRoute(<IP_QGW_LAN>) → 默认路由 → NIC1
                  ↓
             [NIC1 发送] → [TUN] → [QG OS] → [LAN <IP_QGW_LAN>]
```

### 核心问题：默认路由方向冲突

**去程**：SNAT 后需要 IPIP 封装 → 需要路由到隧道 NIC（NIC3）
```
0.0.0.0/0 → NIC3 (隧道，用于 IPIP 封装)
```

**回程**：DNAT 后目标是 LAN IP → 需要路由到 TUN（NIC1）
```
0.0.0.0/0 → NIC1 (TUN，返回给 LAN)
```

**冲突**：默认路由只能有一个方向！

---

## 当前架构（2-NIC）

```
NIC 1: TUN (本地应用入口)
NIC 2: Mesh (mesh 流量出口)

路由表:
  <IP_OTHER_023>/32 → NIC 1 (VIP)
  100.0.0.0/8  → NIC 2 (mesh)
```

## 流量路径

### 1. 本地应用 → Fake-IP → forwarder

```
[Windows App] 
    ↓ src=<IP_OTHER_023>, dst=100.64.0.x (Fake-IP)
[TUN NIC 1]
    ↓
[gVisor NIC 1 进入]
    ↓
{路由决策}
    ↓ FindRoute(100.64.0.x)
    ↓ 匹配 100.0.0.0/8 → NIC 2
    ↓
[转发路径] ❌ 错误！应该本地交付
    ↓
[NIC 2 发送] → mesh → ???
```

**问题**：转发优先下，Fake-IP 被转发到 NIC 2，不会被 forwarder 处理。

**期望路径**：
```
[gVisor NIC 1 进入]
    ↓
{本地交付} ✅
    ↓
[TCP/UDP forwarder]
    ↓
[代理连接] → 外网
```

### 2. 本地应用 → mesh VIP

```
[Windows App]
    ↓ src=<IP_OTHER_023>, dst=<IP_GG_VIP> (GG VIP)
[TUN NIC 1]
    ↓
[gVisor NIC 1 进入]
    ↓
{路由决策}
    ↓ FindRoute(<IP_GG_VIP>)
    ↓ 匹配 100.0.0.0/8 → NIC 2
    ↓
[转发路径] ✅
    ↓
[NIC 2 发送] → mesh P2P → GG ✅
```

### 3. 旁路网关 → 外网

```
[LAN <IP_QGW_LAN>]
    ↓ src=<IP_QGW_LAN>, dst=8.8.8.8
[QG OS iptables]
    ↓
[TUN NIC 1]
    ↓
[gVisor NIC 1 进入]
    ↓
{路由决策}
    ↓ FindRoute(8.8.8.8)
    ↓ 无匹配 → ErrHostUnreachable ❌
    ↓
[包被丢弃] ❌
```

**期望路径**：
```
[gVisor NIC 1 进入]
    ↓
{路由决策}
    ↓ 默认路由 → NIC 2
    ↓
[Postrouting SNAT]
    ↓ src → <IP_OTHER_023>
    ↓
[NIC 2 发送] → mesh → 出口节点 → 8.8.8.8 ✅
```

**需要**：添加默认路由 `0.0.0.0/0 → NIC 2`

### 4. 返回流量（旁路网关）

```
[8.8.8.8]
    ↓ src=8.8.8.8, dst=<IP_OTHER_023> (出口节点 EIP)
[mesh P2P]
    ↓
[gVisor NIC 2 进入]
    ↓
[Prerouting DNAT]
    ↓ dst → <IP_QGW_LAN>
    ↓
{路由决策}
    ↓ FindRoute(<IP_QGW_LAN>)
    ↓ 无匹配 → ErrHostUnreachable ❌
    ↓
[包被丢弃] ❌
```

**期望路径**：
```
[Prerouting DNAT]
    ↓ dst → <IP_QGW_LAN>
    ↓
{路由决策}
    ↓ 默认路由 → NIC 1
    ↓
[NIC 1 发送] → TUN → OS → LAN ✅
```

**需要**：默认路由 `0.0.0.0/0 → NIC 1`

**冲突**：旁路网关出方向需要默认路由 → NIC 2，返回方向需要默认路由 → NIC 1！

### 5. mesh 节点互访（GG → VM）

```
[GG App]
    ↓ src=<IP_GG_VIP>, dst=<IP_OTHER_023>
[mesh P2P]
    ↓
[gVisor NIC 2 进入]
    ↓
{路由决策}
    ↓ FindRoute(<IP_OTHER_023>)
    ↓ 匹配 <IP_OTHER_023>/32 → NIC 1
    ↓
[转发路径]
    ↓
[NIC 1 发送] → TUN → OS → ??? ❌
```

**期望路径**：
```
[gVisor NIC 2 进入]
    ↓
{本地交付} ✅ (目标是本地 VIP)
    ↓
[admin handler / 应用] ✅
```

**问题**：VIP 路由指向 NIC 1，但包从 NIC 2 进入，应该本地交付。

### 6. 通告路由（VM → MS9 背后的 <IP_OTHER_101>/24）

```
[VM App]
    ↓ src=<IP_OTHER_023>, dst=<IP_OTHER_102>
[TUN NIC 1]
    ↓
[gVisor NIC 1 进入]
    ↓
{路由决策}
    ↓ FindRoute(<IP_OTHER_102>)
    ↓ 无匹配 → ErrHostUnreachable ❌
    ↓
[包被丢弃] ❌
```

**期望路径**：
```
[gVisor NIC 1 进入]
    ↓
{路由决策}
    ↓ 匹配通告路由 → 隧道 NIC 201
    ↓
[隧道 NIC WritePackets]
    ↓ IPIP 封装 (outer dst: MS9 EIP)
    ↓
[mesh P2P] → MS9
    ↓
[MS9 解封装] → <IP_OTHER_102> ✅
```

**需要**：为通告路由添加路由条目 `<IP_OTHER_101>/24 → 隧道 NIC 201`

## 核心冲突

### 冲突 1：默认路由方向

```
旁路网关出方向: 0.0.0.0/0 → NIC 2 (mesh)
返回包方向:     0.0.0.0/0 → NIC 1 (TUN)
```

**无法用单一默认路由解决！**

### 冲突 2：Fake-IP 处理

```
转发优先: Fake-IP → 转发到 NIC 2 ❌
期望:     Fake-IP → 本地交付给 forwarder ✅
```

### 冲突 3：VIP 互访

```
VIP 路由: <IP_OTHER_023>/32 → NIC 1
包从 NIC 2 进入，应该本地交付，但路由指向 NIC 1 → 转发出去 ❌
```

## 解决方案分析

### 方案 A：策略路由（假设支持）

```
路由表 100 (本地应用):
  0.0.0.0/0 → local (本地交付)

路由表 200 (旁路网关):
  0.0.0.0/0 → NIC 2 (转发)

策略规则:
  from <IP_OTHER_023> (VIP) → 路由表 100
  from <IP_QG_LAN_NET>/24 (LAN) → 路由表 200
```

**问题**：
- 返回包 DNAT 后目标是 LAN IP，需要额外规则
- gVisor 不支持策略路由

### 方案 B：3-NIC 架构

```
NIC 1: TUN (本地应用入口)
NIC 2: Mesh (VIP 互访)
NIC 3: 隧道 (旁路网关入口 + 通告路由出口)

路由表:
  100.0.0.0/8 → NIC 2 (mesh VIP)
  0.0.0.0/0   → NIC 3 (默认)
```

**本地应用**：
```
NIC 1 进入 → 本地交付 → forwarder ✅
```

**旁路网关**：
```
NIC 3 进入 → 路由 → NIC 3 → SNAT → mesh ✅
```

**返回包**：
```
NIC 2 进入 → DNAT → 路由 → NIC 3 → TUN → LAN ✅
```

**优势**：本地应用和旁路网关从不同 NIC 进入，天然隔离。

### 方案 C：WritePackets 判断

```
NIC 1 WritePackets:
  if src == VIP:
    # 本地应用，环回给 forwarder
    InjectInbound(pkt)
  else:
    # 旁路网关，SNAT + 转发到 NIC 2
    SNAT(pkt)
    NIC 2.Send(pkt)
```

**问题**：需要在 WritePackets 中实现复杂逻辑，可能影响性能。

## 结论

**3-NIC 架构最清晰**：
- 本地应用和旁路网关从不同 NIC 进入
- 不需要策略路由
- 路由表简单稳定
- 职责分离明确
