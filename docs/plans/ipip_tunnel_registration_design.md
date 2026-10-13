# IPIP隧道协议注册机制设计

## 背景

Phaethon需要支持IPIP隧道封装/解封来实现非mesh通告路由的转发（Branch 3）。早期的实现在gVisor的Stack结构中硬编码存储EIP，并在ipv4.go中硬编码IPIP解封逻辑，这导致：

1. **侵入gVisor内部状态**：在Stack结构中添加协议特定字段（ipipLocalEIP）
2. **缺乏扩展性**：硬编码proto==4检查，无法支持其他隧道协议（GRE、VXLAN等）
3. **违反关注点分离**：gVisor核心代码包含Phaethon特定的业务逻辑

## 设计方案

采用**协议注册机制**，类似于gVisor的传输层协议注册（TCP/UDP），为网络层隧道协议提供通用的注册和分发机制。

### 核心接口

在gVisor fork的`pkg/tcpip/stack/registration.go`中定义：

```go
// TunnelProtocol is the interface that network-layer tunnel protocols (like IPIP,
// GRE, VXLAN) must implement. Unlike TransportProtocol which creates socket endpoints,
// TunnelProtocol decapsulates tunnel packets and returns the inner packet for normal
// network layer processing.
type TunnelProtocol interface {
    // Number returns the IP protocol number for this tunnel protocol
    // (e.g., 4 for IPIP, 47 for GRE).
    Number() tcpip.TransportProtocolNumber

    // ShouldDecapsulate checks if this packet should be decapsulated by this handler.
    // For example, IPIP checks if outer destination matches local EIP.
    // Returns true if the packet should be decapsulated, false otherwise.
    ShouldDecapsulate(pkt *PacketBuffer) bool

    // Decapsulate decapsulates the tunnel packet and returns the inner packet.
    // The caller is responsible for releasing the returned packet buffer.
    // Returns nil if decapsulation fails.
    Decapsulate(pkt *PacketBuffer) *PacketBuffer
}
```

### Stack扩展

在`pkg/tcpip/stack/stack.go`中：

1. **添加隧道协议注册表**：
```go
type Stack struct {
    // ... existing fields ...
    
    // tunnelProtocols registers network-layer tunnel protocol handlers (IPIP, GRE, etc.).
    // These handlers decapsulate tunnel packets and return inner packets for normal processing.
    tunnelProtocols map[tcpip.TransportProtocolNumber]TunnelProtocol `state:"nosave"`
}
```

2. **提供注册/查询方法**：
```go
// RegisterTunnelProtocol registers a network-layer tunnel protocol handler.
func (s *Stack) RegisterTunnelProtocol(proto TunnelProtocol)

// UnregisterTunnelProtocol removes a previously registered tunnel protocol handler.
func (s *Stack) UnregisterTunnelProtocol(number tcpip.TransportProtocolNumber)

// GetTunnelProtocol returns the registered tunnel protocol handler for the given protocol number.
func (s *Stack) GetTunnelProtocol(number tcpip.TransportProtocolNumber) TunnelProtocol
```

### 网络层分发

在`pkg/tcpip/network/ipv4/ipv4.go`的`handleValidatedPacket`中：

```go
// Check for registered tunnel protocol handlers (IPIP, GRE, etc.).
if tunnelProto := e.protocol.stack.GetTunnelProtocol(h.TransportProtocol()); tunnelProto != nil {
    if tunnelProto.ShouldDecapsulate(pkt) {
        innerPkt := tunnelProto.Decapsulate(pkt)
        if innerPkt != nil {
            defer innerPkt.DecRef()
            // Parse inner IP header and recursively process
            innerData := innerPkt.NetworkHeader().Slice()
            if len(innerData) >= 20 {
                innerH := header.IPv4(innerData)
                if innerH.IsValid(len(innerH)) {
                    e.handleValidatedPacket(innerH, innerPkt, inNICName)
                    return
                }
            }
        }
    }
}
// Continue with normal processing...
```

### IPIP处理器实现

在Phaethon的`mesh/ipip_tunnel.go`中实现IPIP隧道协议处理器：

```go
type ipipTunnelProtocol struct {
    localEIP tcpip.Address  // 处理器维护自己的状态
}

func (p *ipipTunnelProtocol) Number() tcpip.TransportProtocolNumber {
    return IPIPProtocolNumber  // 4
}

func (p *ipipTunnelProtocol) ShouldDecapsulate(pkt *stack.PacketBuffer) bool {
    // 检查外层目的IP是否匹配本地EIP
    outerIP := header.IPv4(pkt.NetworkHeader().Slice())
    dstAddr := outerIP.DestinationAddress()
    return p.localEIP == dstAddr
}

func (p *ipipTunnelProtocol) Decapsulate(pkt *stack.PacketBuffer) *stack.PacketBuffer {
    // 剥离外层IP头，返回内层包
    outerIP := header.IPv4(pkt.NetworkHeader().Slice())
    outerHdrLen := int(outerIP.HeaderLength())
    innerData := pkt.NetworkHeader().Slice()[outerHdrLen:]
    
    innerPkt := stack.NewPacketBuffer(stack.PacketBufferOptions{
        Payload: buffer.MakeWithData(innerData),
    })
    return innerPkt
}
```

### 注册流程

在`mesh/netstack.go`的`configureRouteSelector`中：

```go
// Register IPIP tunnel protocol handler for decapsulation.
ipipHandler := newIPIPTunnelProtocol(tcpip.AddrFrom4Slice(localEIP))
n.ns.RegisterTunnelProtocol(ipipHandler)
```

## 数据流

### IPIP封装（发送路径）

```
内层包 (dst=172.30.0.2)
  ↓
RouteSelector Branch 3: 非mesh通告路由
  ↓
forwardWithIPIPEncapsulation (gVisor fork)
  ↓
外层包 (outer dst=目标节点EIP, proto=4)
  ↓
通过Link NIC发送到mesh peer
```

### IPIP解封（接收路径）

```
外层包从mesh peer到达 (proto=4, outer dst=本地EIP)
  ↓
HandleMeshFrame → Link NIC.InjectInbound
  ↓
gVisor网络层 (ipv4.handleValidatedPacket)
  ↓
检查注册的隧道协议处理器
  ↓
IPIP处理器.ShouldDecapsulate: 外层dst==本地EIP? → 是
  ↓
IPIP处理器.Decapsulate: 剥离外层IP头
  ↓
内层包递归调用handleValidatedPacket
  ↓
RouteSelector决策 → TCP forwarder → Direct拨号目标
```

## 优势

1. **无侵入性**：gVisor Stack结构不存储协议特定状态，只提供注册机制
2. **高扩展性**：未来可轻松添加GRE (proto=47)、VXLAN (UDP封装) 等隧道协议
3. **关注点分离**：
   - gVisor：提供通用的隧道协议注册和分发框架
   - Phaethon：实现具体的隧道协议逻辑（IPIP处理器维护自己的EIP状态）
4. **符合Linux语义**：类似于Linux的隧道模块（ipip、gre等），每个模块独立注册

## 与早期方案的对比

| 方面 | 早期方案（硬编码） | 当前方案（注册机制） |
|------|-------------------|---------------------|
| gVisor侵入性 | 高（添加ipipLocalEIP字段） | 低（只添加通用注册表） |
| 扩展性 | 差（硬编码proto==4） | 好（可注册多种协议） |
| 状态管理 | gVisor Stack存储EIP | 处理器自己维护EIP |
| 代码位置 | ipv4.go中硬编码逻辑 | Phaethon代码中实现处理器 |
| 符合设计原则 | 违反 | 符合 |

## 未来扩展

### GRE隧道 (proto=47)

```go
type greTunnelProtocol struct {
    localAddr tcpip.Address
}

func (p *greTunnelProtocol) Number() tcpip.TransportProtocolNumber {
    return 47  // GRE
}

func (p *greTunnelProtocol) ShouldDecapsulate(pkt *stack.PacketBuffer) bool {
    // GRE有额外的头部，需要解析GRE header
    // 检查目的地址是否匹配
}

func (p *greTunnelProtocol) Decapsulate(pkt *stack.PacketBuffer) *stack.PacketBuffer {
    // 剥离外层IP头 + GRE头
    // 返回内层包
}
```

### VXLAN (UDP封装)

VXLAN比较特殊，它是UDP封装（端口4789），需要在UDP处理器中注册子处理器，或者在UDP层检查端口号后分发到VXLAN处理器。

## 总结

通过引入隧道协议注册机制，我们实现了：
- gVisor fork的最小化修改（只添加通用框架）
- Phaethon代码的模块化（每个隧道协议独立实现）
- 良好的扩展性（支持未来添加更多隧道协议）
- 符合Linux的网络隧道语义

这是一个典型的"框架在底层，实现在上层"的设计模式，既保持了gVisor的通用性，又满足了Phaethon的特定需求。
