# Multi-NIC 架构设计（手动 NAT 方案）

## 概述

本方案采用多 NIC 架构，NIC 3 作为统一分发点处理所有包的流向。NAT 在 readLoop/writeLoop 中手动实现。

## 为什么不用 gVisor iptables

经过源码调研，gVisor iptables 无法满足需求：

1. **SNATTarget 只支持 Postrouting/Input hook**
   - Prerouting/Output/Forward 会 panic
2. **Postrouting 不支持接口匹配**
   - `iptables_types.go:320-321` 直接 `return true`，忽略 InputInterface/OutputInterface
3. **Input hook 只处理本地包**
   - 从 TUN 进入需要转发的包不会触发 Input hook
4. **无法区分目标**
   - 即使 Postrouting 支持接口匹配，也无法区分目标是 mesh 网段还是外网

**结论**：手动 NAT 在 readLoop/writeLoop 中是正确的方案，因为那时候有完整的上下文信息。

## gVisor Spoofing 机制调研

### Spoofing 检查时机

Spoofing 在**路由创建时**（`FindRoute`）检查，不是在包发出后检查。

```go
// stack.go:1304-1309
func (s *Stack) getAddressEP(nic *nic, localAddr, remoteAddr, srcHint tcpip.Address, netProto tcpip.NetworkProtocolNumber) AssignableAddressEndpoint {
    if localAddr.BitLen() == 0 {
        return nic.primaryEndpoint(netProto, remoteAddr, srcHint)  // 未指定 localAddr，自动选择
    }
    return nic.findEndpoint(netProto, localAddr, CanBePrimaryEndpoint)  // 指定了 localAddr，检查 spoofing
}
```

### Spoofing 检查逻辑

当指定了 `localAddr` 时，`findEndpoint` 会检查 NIC 是否开启 spoofing：

```go
// nic.go:497-499
func (n *nic) findEndpoint(protocol tcpip.NetworkProtocolNumber, address tcpip.Address, peb PrimaryEndpointBehavior) AssignableAddressEndpoint {
    return n.getAddressOrCreateTemp(protocol, address, peb, spoofing)
}

// nic.go:510-518
func (n *nic) getAddressOrCreateTemp(..., tempRef getAddressBehaviour) AssignableAddressEndpoint {
    var spoofingOrPromiscuous bool
    switch tempRef {
    case spoofing:
        spoofingOrPromiscuous = n.Spoofing()  // 检查 NIC 是否开启 spoofing
    }
    return n.getAddressOrCreateTempInner(protocol, address, spoofingOrPromiscuous, peb)
}
```

- **NIC 开启 spoofing**：允许使用任意地址作为源地址
- **NIC 未开启 spoofing**：只能使用绑定到该 NIC 的地址作为源地址

### 本地交付逻辑

当目标是本机地址（绑定到某个 NIC 的地址）时，gVisor 会尝试创建本地路由：

```go
// stack.go:1476-1480
if s.handleLocal && !isMulticast && !isLocalBroadcast {
    if r := s.findLocalRouteRLocked(id, localAddr, remoteAddr, netProto); r != nil {
        return r, nil  // 本地路由，包在协议栈内部环回
    }
}
```

`findLocalRoute` 检查 `localAddr` 和 `remoteAddr` 是否都是本机地址：

```go
// stack.go:1334-1338
func (s *Stack) findLocalRouteFromNICRLocked(localAddressNIC *nic, localAddr, remoteAddr tcpip.Address, ...) *Route {
    localAddressEndpoint := localAddressNIC.getAddressOrCreateTempInner(netProto, localAddr, false, NeverPrimaryEndpoint)
    if localAddressEndpoint == nil {
        return nil  // localAddr 不是该 NIC 绑定的地址，返回 nil
    }
    // ... 检查 remoteAddr 是否也是本机地址
}
```

### 包交付路径

```go
// ipv4.go:561-569
func (e *endpoint) writePacketPostRouting(r *stack.Route, pkt *stack.PacketBuffer, headerIncluded bool) tcpip.Error {
    if r.Loop()&stack.PacketLoop != 0 {
        e.handleLocalPacket(pkt, !headerIncluded)  // 本地交付
    }
    if r.Loop()&stack.PacketOut == 0 {
        return nil  // 不从 NIC 发出
    }
    // ... 从 NIC 发出到物理层
}
```

- **`loop & PacketLoop != 0`**：调用 `handleLocalPacket`，在协议栈内部本地交付
- **`loop & PacketOut == 0`**：不从 NIC 发出到物理层

### 关键结论

1. **如果 `localAddr` 是本机地址，`remoteAddr` 也是本机地址**：
   - `findLocalRoute` 成功，创建本地路由
   - `loop = PacketLoop`
   - **包在协议栈内部本地交付，不从 NIC 发出**

2. **如果 `localAddr` 不是本机地址，`remoteAddr` 是本机地址**：
   - `findLocalRoute` 失败（`localAddr` 不是任何 NIC 绑定的地址）
   - 继续走正常路由路径
   - `loop = PacketOut`（因为 `remoteAddr != localAddr`）
   - **包从 NIC 发出到物理层，不会本地交付**

3. **如果 `remoteAddr` 不是本机地址**：
   - 走正常路由路径
   - 检查 spoofing
   - **包从 NIC 发出**

### 对我们架构的影响

在我们的架构中：
- **gVisor 本机地址**：只有 GIP (100.x.0.3)，绑定在 NIC 2
- **不是本机地址**：VIP (100.x.0.1)、EIP (100.x.0.4)，没有绑定到任何 NIC

可能出现的包：
1. **本地应用发出的包**：`src=本地IP, dst=任意`
   - `localAddr=本地IP` 不是本机地址
   - 检查 spoofing，NIC 1 需要开启 spoofing

2. **Forwarder 发出的回程包**：`src=外部IP, dst=VIP`
   - `localAddr=外部IP` 不是本机地址
   - `remoteAddr=VIP` 不是本机地址
   - 检查 spoofing，NIC 1 需要开启 spoofing
   - 包从 NIC 1 发出，被 writeLoop 读取

3. **目标是 GIP 的包**：`src=任意, dst=GIP`
   - 如果 `src` 不是本机地址，`findLocalRoute` 失败
   - 包从 NIC 2 发出到 mesh 网络，**不会本地交付**
   - 但在我们的架构中，这种情况不会发生（不会有包 `src=非本机地址, dst=本地GIP`）

## NIC 定义

| NIC | 名称 | 绑定地址 | Promiscuous | Spoofing | 用途 |
|-----|------|----------|-------------|----------|------|
| NIC 1 | TUN adapter | **无** | false | true | 接收 TUN + writeLoop 写回外部 NAT 回程 |
| NIC 2 | Mesh endpoint | GIP (100.x.0.3) | false | true | mesh 网络收发 |
| NIC 3 | IPIP endpoint | **无** | true | false | IPIP 解封后环回 |
| NIC 4 | Loopback endpoint | **无** | true | **true** | 统一分发：IPIP 封包 or 环回 Forwarder |

**配置说明**：

- **Promiscuous（混杂模式）**：NIC 接收所有包，不检查 dst 是否匹配绑定地址
  - NIC 1/2：false，只接收 dst 匹配绑定地址的包
  - NIC 3/4：true，需要接收 dst=EIP 或任意地址的包

- **Spoofing（地址欺骗）**：允许发出的包 src 不是 NIC 绑定的地址
  - NIC 1/2：true，NIC 1 发出的包 src 是本地应用的原始 IP，NIC 2 发出的包 src 是本地 GIP
  - NIC 3：false，不需要发出包（只做环回）
  - NIC 4：**true**，Forwarder 创建 endpoint 时需要 `FindRoute(NIC4, srcIP, dstIP)`，srcIP 可能是任意地址（如外部 IP），需要 spoofing 才能创建路由

## 路由表

| 目标 | NIC | 说明 |
|------|-----|------|
| VIP (100.x.0.1) | NIC 1 | 外部 NAT 回程 → writeLoop 写回 TUN |
| 100.0.0.0/8 (mesh) | NIC 2 | 其他 mesh 节点 |
| EIP (100.x.0.4) | NIC 3 | IPIP 解封入口 |
| 0.0.0.0/0 (default) | NIC 4 | 默认路由 |

**说明**：
- NIC 1 不绑定任何地址，但 VIP 路由指向 NIC 1，让外部 NAT 回程包（dst=VIP）从 NIC 1 writeLoop 写回 TUN
- 本地 mesh 网段路由已移除，VIP 单独路由到 NIC 1
- EIP 路由到 NIC 3，用于 IPIP 解封
- 默认路由到 NIC 4，作为统一分发点

## 手动 NAT 实现

### SNAT（readLoop 中）

```go
// tun/engine.go readLoop
// 所有从 TUN 进入的 IPv4 包都做 SNAT，替换 src IP 为 VIP
if e.natTable != nil && n >= 20 && pktBuf[0]>>4 == 4 {
    if natPkt := e.natTable.TranslateOutbound(pktBuf); natPkt != nil {
        pktBuf = natPkt
    }
}
```

**说明**：
- 在 readLoop 中，包刚从 TUN 进来，知道来源是本地应用
- NATTable 自动记录映射：VIP:port ↔ 原始src:port
- 可以根据目标地址决定 SNAT 策略（mesh 网段 vs 外网）

### DNAT（writeLoop 中）

```go
// tun/engine.go writeLoop（或 NIC 3 分发逻辑中写 TUN 前）
// 回程包需要做 DNAT，替换 dst IP 为原始 src
if e.natTable != nil {
    if natPkt := e.natTable.TranslateInbound(pktBuf); natPkt != nil {
        pktBuf = natPkt
    }
}
```

**说明**：
- 回程包在送往 TUN 前做 DNAT
- NATTable 自动查找映射，还原原始 src IP

## NIC 3 IPIP 解封逻辑

NIC 3 专门处理 IPIP 解封：

```
NIC 3 收到 IPIP 包（外层 dst=本地EIP）：
  → 解封装 IPIP
  → 内层包环回到 Forwarder（跨节点 Forwarder，无 DNAT）
```

## NIC 4 统一分发逻辑

NIC 4 是核心分发点，处理所有包的流向：

```
NIC 4 收到包，判断:

1. dst 命中通告路由？
   → IPIP 封装 → NIC 2 (mesh)

2. 否则
   → 环回到 Forwarder
```

**说明**：
- NIC 4 不再判断 src 是否 mesh 网段
- 外部 NAT 回程包（dst=VIP）走 VIP 路由到 NIC 1 writeLoop，不经过 NIC 4
- IPIP 入站包（dst=EIP）走 EIP 路由到 NIC 3，不经过 NIC 4

## 包流转预演

### 场景 1：普通访问（未命中通告路由）

**去程**：
```
应用: src=192.168.1.100, dst=外部IP
  → NIC 1 readLoop → 手动 SNAT: src=192.168.1.100 → src=VIP
  → 注入 gVisor
  → 路由: dst=外部IP → default → NIC 4
  → NIC 4 分发: dst 未命中通告路由 → 环回到 Forwarder
  → Forwarder 处理连接
```

**回程**：
```
Forwarder: src=外部IP, dst=VIP
  → 路由: dst=VIP → NIC 1
  → NIC 1 writeLoop: 手动 DNAT: dst=VIP → dst=192.168.1.100
  → 写回 TUN
  → 应用收到 ✓
```

### 场景 2：IPIP 双向（命中通告路由 + 跨节点 Forwarder）

#### 2.1 出站 IPIP（命中通告路由）

**去程**：
```
应用: src=192.168.1.100, dst=10.11.61.50 (命中通告路由)
  → NIC 1 readLoop → 手动 SNAT: src=192.168.1.100 → src=VIP
  → 注入 gVisor
  → 路由: dst=10.11.61.50 → default → NIC 4
  → NIC 4 分发: 
      dst=10.11.61.50 命中通告路由 ✓
      → IPIP 封装:
          外层: src=本地GIP, dst=出口节点GIP
          内层: src=VIP, dst=10.11.61.50
      → NIC 2 (mesh) → mesh 网络
```

**出口节点处理**：
```
  → NIC 2 收到 IPIP 包
  → 传递给 NIC 3（外层 dst=本地EIP）
  → NIC 3: 解封装 IPIP
  → 内层: src=VIP, dst=10.11.61.50
  → 访问目标: src=出口节点IP, dst=10.11.61.50
  → 目标回包: src=10.11.61.50, dst=出口节点IP
  → 通过 mesh 回传（无需 IPIP）: src=10.11.61.50, dst=VIP
```

**回程**：
```
  → mesh 网络 → 本地 NIC 2 接收
  → 包: src=10.11.61.50, dst=VIP (非 IPIP)
  → 路由: dst=VIP → NIC 1
  → NIC 1 writeLoop: 手动 DNAT → 写回 TUN
  → 应用收到 ✓
```

**说明**：回程包无需 IPIP 封装，mesh 网络可以直接路由 dst=VIP。

#### 2.2 入站 IPIP（跨节点 Forwarder）

**其他节点访问本地网络**：
```
其他节点应用: src=其他节点应用IP, dst=192.168.1.100 (本地网络)
  → 其他节点做 IPIP 封装:
      外层: src=其他节点GIP, dst=本地EIP
      内层: src=其他节点VIP, dst=192.168.1.100
  → mesh 网络 → 本地 NIC 2 接收
  → 路由: dst=本地EIP → NIC 3
  → NIC 3:
      解封装 IPIP
      → 内层: src=其他节点VIP, dst=192.168.1.100
      → 环回到 Forwarder（无 DNAT，跨节点执行 Forwarder）
  → Forwarder 访问 192.168.1.100
  → 回包通过 Forwarder → mesh → 其他节点 ✓
```

**说明**：IPIP 的用途是跨节点执行 Forwarder，解封装后直接环回到 Forwarder，无 DNAT。

## 关键设计点

1. **NIC 1 恢复 writeLoop**：外部 NAT 回程包（dst=VIP）通过 VIP 路由到 NIC 1，writeLoop 做 DNAT 后写回 TUN
2. **手动 NAT**：在 readLoop/writeLoop 中实现，有完整上下文信息
3. **NIC 3 专门 IPIP 解封**：只处理 IPIP 解封后环回，职责单一
4. **NIC 4 统一分发**：判断 dst 是否命中通告路由，决定 IPIP 封包或环回 Forwarder
5. **IPIP 双向**：
   - 出站：NIC 4 判断 dst 命中通告路由时封装，发送到 NIC 2
   - 入站：NIC 3 解封装，环回到 Forwarder
6. **回程无 IPIP**：mesh 网络可以直接路由 dst=VIP，通过 NIC 1 writeLoop 写回 TUN

## 实现要点

1. **恢复 NIC 1 的 writeLoop**：处理外部 NAT 回程包，做 DNAT 后写回 TUN
2. **手动 NAT**：readLoop 中 TranslateOutbound (SNAT)，writeLoop 中 TranslateInbound (DNAT)
3. **新增 NIC 3 (IPIP endpoint)**：
   - 不绑定 IP，开启 Promiscuous
   - 接收 IPIP 包（外层 dst=本地EIP）
   - 解封装后环回到 Forwarder
4. **修改 NIC 4 (Loopback endpoint)**：
   - 不绑定 IP，开启 Promiscuous
   - 判断 dst 是否命中通告路由
   - 命中则 IPIP 封装 → NIC 2
   - 否则环回到 Forwarder
5. **更新路由表**：
   - VIP → NIC 1
   - 100.0.0.0/8 → NIC 2
   - EIP → NIC 3
   - default → NIC 4

## 与旧架构的对比

| 项目 | 旧架构 | 新架构 |
|------|----------|--------|
| NAT 位置 | readLoop/writeLoop | readLoop/writeLoop（保持不变） |
| NIC 1 用途 | 仅接收 | 接收 + writeLoop 写回 |
| NIC 3 用途 | 统一分发 | IPIP 解封 |
| NIC 4 用途 | - | 统一分发 |
| 回程路径 | NIC 3 分发 → 送往 TUN | NIC 1 writeLoop |
| IPIP 解封装 | NIC 2 | NIC 3 |
| IPIP 封装 | NIC 3 | NIC 4 |
