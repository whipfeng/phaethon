# IPIP隧道协议注册机制实现总结

## 完成的工作

### 1. gVisor Fork修改

#### 1.1 定义TunnelProtocol接口 (`pkg/tcpip/stack/registration.go`)
```go
type TunnelProtocol interface {
    Number() tcpip.TransportProtocolNumber
    ShouldDecapsulate(pkt *PacketBuffer) bool
    Decapsulate(pkt *PacketBuffer) *PacketBuffer
}
```

这个接口定义了网络层隧道协议的通用行为：
- `Number()`: 返回协议号（IPIP=4, GRE=47等）
- `ShouldDecapsulate()`: 判断是否应该解封此包
- `Decapsulate()`: 执行解封，返回内层包

#### 1.2 添加注册机制 (`pkg/tcpip/stack/stack.go`)
- 在Stack结构中添加`tunnelProtocols`注册表
- 提供`RegisterTunnelProtocol()`, `UnregisterTunnelProtocol()`, `GetTunnelProtocol()`方法
- 在Stack初始化时创建空的注册表

#### 1.3 实现分发逻辑 (`pkg/tcpip/network/ipv4/ipv4.go`)
在`handleValidatedPacket()`中添加隧道协议检查：
```go
if tunnelProto := e.protocol.stack.GetTunnelProtocol(h.TransportProtocol()); tunnelProto != nil {
    if tunnelProto.ShouldDecapsulate(pkt) {
        innerPkt := tunnelProto.Decapsulate(pkt)
        // 递归处理内层包
        e.handleValidatedPacket(innerH, innerPkt, inNICName)
        return
    }
}
```

### 2. Phaethon实现

#### 2.1 IPIP隧道协议处理器 (`mesh/ipip_tunnel.go`)
实现了`ipipTunnelProtocol`结构：
- 维护自己的状态：`localEIP`（本地Endpoint IP）
- `ShouldDecapsulate()`: 检查外层目的IP是否匹配localEIP
- `Decapsulate()`: 剥离外层IP头，返回内层包

#### 2.2 注册IPIP处理器 (`mesh/netstack.go`)
在`configureRouteSelector()`中注册IPIP处理器：
```go
ipipHandler := newIPIPTunnelProtocol(tcpip.AddrFrom4Slice(localEIP))
n.ns.RegisterTunnelProtocol(ipipHandler)
```

#### 2.3 清理旧代码
- 删除了`mesh/ipip_protocol.go`（旧的传输层协议实现）
- 移除了gVisor中的硬编码IPIP逻辑

### 3. 设计文档
创建了`docs/plans/ipip_tunnel_registration_design.md`，详细说明：
- 设计背景和动机
- 接口定义和实现细节
- 数据流（封装和解封路径）
- 与早期方案的对比
- 未来扩展性（GRE、VXLAN等）

## 架构优势

### 1. 无侵入性
- gVisor Stack不存储协议特定状态
- 只提供通用的注册和分发框架
- 协议状态由Phaethon代码维护

### 2. 高扩展性
- 可轻松添加新的隧道协议（GRE、VXLAN等）
- 每个协议独立实现，互不干扰
- 符合开闭原则（对扩展开放，对修改封闭）

### 3. 关注点分离
- gVisor：提供通用框架
- Phaethon：实现具体协议逻辑
- 符合单一职责原则

### 4. 符合Linux语义
- 类似于Linux的隧道模块（ipip、gre等）
- 每个模块独立注册到内核
- 内核提供通用的分发机制

## 数据流

### IPIP封装路径（发送）
```
内层包 → RouteSelector Branch 3 → forwardWithIPIPEncapsulation 
→ 外层包(proto=4) → Link NIC → mesh peer
```

### IPIP解封路径（接收）
```
外层包从peer到达 → Link NIC.InjectInbound → gVisor网络层
→ handleValidatedPacket → GetTunnelProtocol(4)
→ ShouldDecapsulate(外层dst==localEIP?) → Decapsulate()
→ 内层包 → 递归handleValidatedPacket → RouteSelector → TCP forwarder
```

## 测试状态

### 已验证
- ✅ Linux amd64编译成功
- ✅ Windows amd64编译成功
- ✅ VM环境部署成功
- ✅ 服务正常启动（watchdog + worker进程）
- ✅ 现有mesh流量正常处理

### 待测试
- ⏳ IPIP封装（需要从QG发送IPIP包到VM）
- ⏳ IPIP解封（VM接收并解封IPIP包）
- ⏳ 端到端连通性（通过IPIP隧道访问VM的advertised routes）

## 下一步

1. **部署到QG环境**：QG需要运行新代码才能发送IPIP包
2. **测试IPIP封装**：从VM ping 172.30.0.2（QG的advertised route）
3. **测试IPIP解封**：从QG ping 10.0.0.2（VM的advertised route）
4. **验证TCP forwarder**：确认解封后的包能正确到达TCP forwarder
5. **清理调试日志**：移除过多的GVISOR-TRACE日志

## 关键文件

### gVisor Fork
- `pkg/tcpip/stack/registration.go` - TunnelProtocol接口定义
- `pkg/tcpip/stack/stack.go` - 注册机制实现
- `pkg/tcpip/network/ipv4/ipv4.go` - 分发逻辑

### Phaethon
- `mesh/ipip_tunnel.go` - IPIP处理器实现
- `mesh/netstack.go` - 注册IPIP处理器
- `docs/plans/ipip_tunnel_registration_design.md` - 设计文档

## 总结

通过引入隧道协议注册机制，我们成功地将IPIP处理从gVisor的硬编码逻辑转变为可扩展的插件式架构。这不仅解决了当前的IPIP需求，还为未来支持更多隧道协议（GRE、VXLAN等）奠定了基础。

这个设计遵循了"框架在底层，实现在上层"的原则，保持了gVisor的通用性，同时满足了Phaethon的特定需求。
