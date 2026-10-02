# Multi-NIC 架构设计（gVisor iptables NAT 方案）

## 概述

本方案将 NAT 能力下沉到 gVisor 内部，通过 iptables + conntrack 实现，NIC 3 作为统一分发点处理所有包的流向。

## NIC 定义

| NIC | 名称 | 绑定地址 | 用途 |
|-----|------|----------|------|
| NIC 1 | TUN adapter | **无** | **仅接收** TUN 数据 |
| NIC 2 | Mesh endpoint | GIP (100.1.0.3) | mesh 网络收发 |
| NIC 3 | Loopback endpoint | **无** | **统一分发点** |

## 路由表

| 目标 | NIC | 说明 |
|------|-----|------|
| 100.1.0.0/16 (本地 mesh 网段) | NIC 3 | 本地 mesh 地址（含 VIP、GIP） |
| 100.0.0.0/8 (mesh) | NIC 2 | 其他 mesh 节点 |
| 0.0.0.0/0 (default) | NIC 3 | 默认路由 |

**说明**：
- NIC 1 不绑定任何地址，仅用于接收 TUN 数据
- 本地 mesh 网段 (100.1.0.0/16) 路由到 NIC 3，用于回程包和 IPIP 解封装后的包
- VIP 不需要单独路由（包含在本地 mesh 网段中）

## gVisor iptables 规则

### SNAT 规则（Input hook）

```go
// 所有从 NIC 1 进入的包都做 SNAT
Rule{
    Matcher: IPHeaderFilter{
        InputInterface: "NIC1",
    },
    Target: &SNATTarget{
        Addr: VIP, // 100.1.0.1
    },
}
```

**说明**：
- 不判断 src 是否属于 mesh 网段，所有 NIC 1 的包都转
- conntrack 自动记录映射：VIP:port ↔ 原始src:port

### DNAT（conntrack 自动）

- **回程包**（Forwarder 发出）：Output hook，conntrack 自动 DNAT
- **mesh 回程包**：Prerouting hook，conntrack 自动 DNAT
- 不需要额外配置 DNAT 规则

## NIC 3 统一分发逻辑

NIC 3 是核心分发点，处理所有包的流向：

```
NIC 3 收到包，判断:

1. src ∉ mesh 网段?
   → 送往 TUN（写回 TUN 设备）

2. src ∈ mesh 网段:
   → 是 IPIP 包（外层 dst=本地GIP）?
       → 解封装 IPIP
       → 内层包环回到 Forwarder（跨节点 Forwarder，无 DNAT）
   
   → 普通包:
       → dst 命中通告路由 → IPIP 封装 → NIC 2 (mesh)
       → dst 未命中通告路由 → 环回到 Forwarder
```

### 送往 TUN 的机制

由于 NIC 1 仅接收，需要一个机制把包写回 TUN：
- 在 NIC 3 的分发逻辑中，直接调用 `WriteLoopDevice.Write()` 把包写入 TUN
- 或者保留一个简化的 writeLoop，只负责写 TUN，不做 NAT

## 包流转预演

### 场景 1：普通访问（未命中通告路由）

**去程**：
```
应用: src=192.168.1.100, dst=外部IP
  → NIC 1 readLoop → 注入 gVisor
  → iptables SNAT: src=192.168.1.100 → src=VIP
  → 路由: dst=外部IP → default → NIC 3
  → NIC 3 分发: src=VIP ∈ mesh, dst 未命中通告路由 → 环回到 Forwarder
  → Forwarder 处理连接
```

**回程**：
```
Forwarder: src=外部IP, dst=VIP
  → conntrack DNAT (Output hook): dst=VIP → dst=192.168.1.100
  → 路由: dst=192.168.1.100 → default → NIC 3
  → NIC 3 分发: src=外部IP ∉ mesh → 送往 TUN
  → 应用收到 ✓
```

### 场景 2：IPIP 双向（命中通告路由 + 跨节点 Forwarder）

#### 2.1 出站 IPIP（命中通告路由）

**去程**：
```
应用: src=192.168.1.100, dst=10.11.61.50 (命中通告路由)
  → NIC 1 readLoop → 注入 gVisor
  → iptables SNAT: src=192.168.1.100 → src=VIP
  → 路由: dst=10.11.61.50 → default → NIC 3
  → NIC 3 分发: 
      src=VIP ∈ mesh ✓
      dst=10.11.61.50 命中通告路由 ✓
      → IPIP 封装:
          外层: src=本地GIP, dst=出口节点GIP
          内层: src=VIP, dst=10.11.61.50
      → NIC 2 (mesh) → mesh 网络
```

**出口节点处理**：
```
  → NIC 2 收到 IPIP 包
  → 解封装 IPIP
  → 内层: src=VIP, dst=10.11.61.50
  → 访问目标: src=出口节点IP, dst=10.11.61.50
  → 目标回包: src=10.11.61.50, dst=出口节点IP
  → 通过 mesh 回传（无需 IPIP）: src=10.11.61.50, dst=VIP
```

**回程**：
```
  → mesh 网络 → 本地 NIC 2 接收
  → 包: src=10.11.61.50, dst=VIP (非 IPIP)
  → 路由: dst=VIP ∈ 100.1.0.0/16 → NIC 3
  → NIC 3 分发: src=10.11.61.50 ∉ mesh → 送往 TUN
  → 应用收到 ✓
```

**说明**：回程包无需 IPIP 封装，mesh 网络可以直接路由 dst=VIP。

#### 2.2 入站 IPIP（跨节点 Forwarder）

**其他节点访问本地网络**：
```
其他节点应用: src=其他节点应用IP, dst=192.168.1.100 (本地网络)
  → 其他节点做 IPIP 封装:
      外层: src=其他节点GIP, dst=本地GIP
      内层: src=其他节点VIP, dst=192.168.1.100
  → mesh 网络 → 本地 NIC 2 接收
  → NIC 2 传递给 NIC 3（不解封装）
  → NIC 3 分发:
      src=其他节点GIP ∈ mesh ✓
      判断: 这是 IPIP 包（外层 dst=本地GIP）
      → 解封装 IPIP
      → 内层: src=其他节点VIP, dst=192.168.1.100
      → 环回到 Forwarder（无 DNAT，跨节点执行 Forwarder）
  → Forwarder 访问 192.168.1.100
  → 回包通过 Forwarder → mesh → 其他节点 ✓
```

**说明**：IPIP 的用途是跨节点执行 Forwarder，解封装后直接环回到 Forwarder，无 DNAT。

## 关键设计点

1. **NIC 1 仅接收**：不做 writeLoop，所有回程数据通过 NIC 3 分发后送往 TUN
2. **gVisor iptables NAT**：SNAT 在 Input hook，DNAT 由 conntrack 自动处理
3. **NIC 3 统一分发**：先判断 src 是否 mesh，再判断 IPIP 和通告路由
4. **IPIP 双向**：
   - 出站：src ∈ mesh 且 dst 命中通告路由时封装，发送到出口节点
   - 入站：src ∈ mesh 且外层 dst=本地GIP 时解封装，环回到 Forwarder
5. **回程无 IPIP**：mesh 网络可以直接路由 dst=VIP，无需 IPIP 封装

## 实现要点

1. **移除 NIC 1 的 writeLoop**：改为在 NIC 3 分发逻辑中直接写 TUN
2. **配置 gVisor iptables**：Input hook SNAT 规则
3. **实现 NIC 3 分发逻辑**（按顺序判断）：
   - 判断 src 是否 ∈ mesh 网段
   - src ∈ mesh 时：判断是否 IPIP 包（外层 dst=本地GIP）
   - src ∈ mesh 且非 IPIP 时：判断 dst 是否命中通告路由
4. **IPIP 封装/解封装**：在 NIC 3 统一处理

## 与当前架构的对比

| 项目 | 当前架构 | 新架构 |
|------|----------|--------|
| NAT 位置 | readLoop/writeLoop | gVisor iptables + conntrack |
| NIC 1 用途 | 收发 | 仅接收 |
| 回程路径 | NIC 1 writeLoop | NIC 3 分发 → 送往 TUN |
| 分发逻辑 | 分散 | NIC 3 统一 |
| IPIP 解封装 | NIC 2 | NIC 3 |
