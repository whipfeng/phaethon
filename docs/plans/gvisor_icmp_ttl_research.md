# gVisor ICMP Time Exceeded 支持调研

## 调研日期
2026-09-30

## 调研问题
gVisor netstack 是否支持在收到 TTL=0 的 IPv4 包时自动生成 ICMP Time Exceeded 响应？

## 调研结论

**gVisor 支持 ICMP Time Exceeded 生成，但 phaethon 的配置导致该功能不工作。**

### gVisor 的实现

gVisor 的 netstack 确实有完整的 ICMP Time Exceeded 生成逻辑：

**位置**: `gvisor/pkg/tcpip/network/ipv4/ipv4.go`

- **Lines 748-772** (`forwardUnicastPacket`): 检查 TTL=0 并生成 ICMP
  ```go
  ttl := h.TTL()
  if ttl == 0 {
      // As per RFC 792 page 6, Time Exceeded Message, ...
      _ = e.protocol.returnError(&icmpReasonTTLExceeded{}, pkt, false /* deliveredLocally */)
      return &ip.ErrTTLExceeded{}
  }
  ```

- **Lines 717-718** in `icmp.go`: 映射到正确的 ICMP type/code
  ```go
  case *icmpReasonTTLExceeded:
      return header.ICMPv4TimeExceeded, header.ICMPv4TTLExceeded, sent.timeExceeded, 0
  ```

- **Lines 609-806** in `icmp.go` (`returnError`): 完整的 ICMP 错误构造，包括查找回源路由、构建 ICMP payload、发送等

### 根本原因：混杂模式绕过了转发路径

**问题代码**: `mesh/netstack.go:327`
```go
s.SetPromiscuousMode(1, true)
```

**gVisor 的包处理决策** (`ipv4.go:1171-1179`):
```go
if addressEndpoint := e.AcquireAssignedAddress(dstAddr, e.nic.Promiscuous(), ...); addressEndpoint != nil {
    e.deliverPacketLocally(h, pkt, inNICName)  // <-- 所有包都走这里
} else if e.Forwarding() {
    e.handleForwardingError(e.forwardUnicastPacket(pkt))  // <-- 永远不会到达
}
```

**混杂模式的影响**:
- 当 `promiscuousMode=true` 时，`AcquireAssignedAddress` 会为任何目标地址创建临时 endpoint
- 这导致**所有包都被认为是本地交付的**
- 本地交付路径 (`deliverPacketLocally`) **没有 TTL=0 检查**
- 因此 ICMP Time Exceeded 生成代码永远不会被执行

### 包处理流程对比

**当前配置（混杂模式开启）**:
```
注入 TTL=0 包 
  → gVisor 收到 
  → 混杂模式匹配任何目标地址 
  → deliverPacketLocally（本地交付）
  → 没有 TTL 检查 
  → 包被静默消费
  → ❌ 没有 ICMP 生成
```

**如果禁用混杂模式**:
```
注入 TTL=0 包 
  → gVisor 收到 
  → 检查是否本地地址 
  → 不是本地地址 
  → 进入转发路径 (forwardUnicastPacket)
  → 检查 TTL=0 
  → ✅ 生成 ICMP Time Exceeded
```

### 转发已启用但无效

**代码**: `mesh/netstack.go:329`
```go
_ = s.SetForwardingDefaultAndAllNICs(ipv4.ProtocolNumber, true)
```

转发已启用，但由于混杂模式导致所有包在到达转发检查之前就被分类为本地交付，所以转发逻辑永远不会执行。

### 注入点分析

有两个注入 TTL=0 包的地方，都有相同的问题：

1. **TUN readLoop** (`tun/engine.go:1071-1083`): 
   - TTL=0 后 fall through 到 line 1111 的 `InjectInbound`
   - 同样的混杂模式问题

2. **HandleMeshFrame** (`mesh/mesh.go:1122-1141`): 
   - TTL=0 后调用 `m.tun.InjectMeshPacket(pkt)` (line 1133)
   - 同样的混杂模式问题

### 现有但未使用的代码

**文件**: `mesh/forward.go`, lines 89-165

存在完整的 `GenerateICMPTimeExceeded` 函数，手动构造 ICMP Time Exceeded 包（type=11, code=0），包含正确的 IP/ICMP checksum 和原始包作为 payload。但**这个函数在代码库中从未被调用**。

### ICMP 速率限制（次要问题）

即使转发路径可达，`ICMPv4TimeExceeded` 也在速率限制列表中（`ipv4.go:1965-1969`）。`allowICMPReply` 函数会检查 `stack.AllowICMPMessage()`，可能导致高负载时某些 ICMP Time Exceeded 消息被丢弃。但这是次要问题，因为主要问题是 ICMP 生成根本不会被尝试。

### gVisor 的双重角色

gVisor 的 netstack 同时支持终端主机和路由器模式：
- **终端主机模式**: 本地交付包，处理传输层协议（TCP/UDP）
- **路由器模式**（转发启用）: 在 NIC 之间转发包，为传输失败生成 ICMP 错误

phaethon 的 netstack 启用了转发，但由于混杂模式，实际上对所有包都表现为终端主机模式。

## 解决方案选项

### 方案 1：禁用混杂模式
- **优点**: 利用 gVisor 内置的 ICMP 生成能力
- **缺点**: 可能影响现有的包处理逻辑，需要全面测试
- **风险**: 不确定禁用混杂模式后，现有的 mesh 路由、fakeIP、本地服务访问等功能是否正常工作

### 方案 2：手动生成 ICMP
- **优点**: 不影响现有配置，使用已有的 `GenerateICMPTimeExceeded` 函数
- **缺点**: 需要手动管理 ICMP 包的路由和发送
- **风险**: 需要确保 ICMP 包能正确路由回源地址

### 方案 3：混合方案
- 对特定目标地址（mesh VIP）禁用混杂模式
- 其他地址保持混杂模式
- **优点**: 最小化影响
- **缺点**: 实现复杂，需要修改 gVisor 的地址管理逻辑

## 待讨论

1. 禁用混杂模式会影响哪些现有功能？
2. mesh 网络为什么需要混杂模式？
3. 是否有方法在保持混杂模式的同时，让特定包走转发路径？

---

## 混杂模式影响分析

### 为什么启用混杂模式？

从代码注释和架构来看，混杂模式用于以下场景：

**writeLoop 的包分类逻辑** (`mesh/netstack.go:1069-1226`):
```
writeLoop 从 NIC 读取出站包，根据目标地址分类：
  - VIP/hostIP → reverse NAT + 写入 TUN（旁路网关返回路径）
  - mesh（非本地）→ mesh 拦截器（通过 mesh 链路发送）
  - 其他 → 重新注入进行本地交付（forwarder/hijacker 通过混杂模式接收）
```

**关键依赖**:
1. **TCP/UDP Forwarder**: gVisor 的 `tcp.NewForwarder` 和 `udp.NewForwarder` 用于处理入站连接
2. **DNS Hijacker**: DNS 劫持器监听特定地址，需要接收发往该地址的包
3. **本地服务访问**: 访问本节点上的服务（如 admin API、trojan 等）

### 禁用混杂模式的潜在影响

#### 1. Forwarder 可能无法接收包

**当前行为**（混杂模式开启）:
- 所有包都被认为是本地交付
- TCP/UDP forwarder 可以接收发往任何地址的包
- Forwarder 调用回调函数处理连接

**禁用后**:
- 只有发往 gVisor 已知地址（assigned addresses）的包才会本地交付
- 发往未知地址的包会进入转发路径
- Forwarder 可能无法接收到发往这些地址的包

**影响范围**:
- TCP forwarder 处理的连接（`tun/engine.go` 中设置的回调）
- UDP forwarder 处理的连接
- 可能影响所有通过 TUN 的 TCP/UDP 流量

#### 2. DNS Hijacker 可能失效

**当前行为**:
- DNS hijacker 监听特定地址（如 192.0.2.3:53）
- 混杂模式确保发往该地址的包被本地交付给 hijacker

**禁用后**:
- 如果 192.0.2.3 没有被添加为 assigned address，包会进入转发路径
- DNS hijacker 无法接收 DNS 请求

**解决方案**:
- 需要确保 DNS hijacker 监听的地址被添加为 assigned address
- 使用 `AddMeshVIP` 或类似函数注册地址

#### 3. 本地服务访问可能受影响

**当前行为**:
- 访问本节点的 admin API（如 100.0.0.1:39999）
- 混杂模式确保包被本地交付给服务

**禁用后**:
- 如果 100.0.0.1 没有被添加为 assigned address，包会进入转发路径
- 服务无法接收请求

**解决方案**:
- 确保所有本地服务的 IP 被添加为 assigned address
- 使用 `AddMeshVIP` 注册 mesh VIP

#### 4. Mesh 路由可能受影响

**当前行为**:
- 发往远程 mesh 节点的包由 writeLoop 拦截并通过 mesh 链路发送

**禁用后**:
- 这些包可能进入 gVisor 的转发路径
- gVisor 尝试转发，但没有正确的路由
- 包可能被丢弃或产生错误的 ICMP

**解决方案**:
- 需要在 gVisor 中配置 mesh 网络的路由
- 或者在 writeLoop 拦截之前处理

### 可能的解决方案

#### 方案 A：选择性禁用混杂模式

只对特定地址范围禁用混杂模式，其他地址保持混杂：
- 为 mesh 网络配置路由，让发往 mesh 的包走转发路径
- 其他地址保持混杂模式，确保 forwarder/hijacker 工作

**优点**: 最小化影响
**缺点**: 实现复杂，需要修改 gVisor 的地址管理和路由配置

#### 方案 B：手动生成 ICMP（推荐）

保持混杂模式不变，在 readLoop 和 HandleMeshFrame 手动生成 ICMP：
- 使用已有的 `GenerateICMPTimeExceeded` 函数
- 生成 ICMP 后注入 gVisor 或直接写入 TUN

**优点**: 不影响现有功能，实现简单
**缺点**: 需要手动管理 ICMP 包的路由

#### 方案 C：注册所有需要的地址

禁用混杂模式，但确保所有需要的地址都被注册：
- 使用 `AddMeshVIP` 注册所有 mesh VIP
- 注册 DNS hijacker 地址
- 注册所有本地服务地址

**优点**: 利用 gVisor 内置功能
**缺点**: 需要全面梳理所有需要的地址，维护成本高

### 建议

**推荐方案 B（手动生成 ICMP）**，原因：
1. 不影响现有架构和功能
2. 实现简单，风险低
3. 已有现成的 `GenerateICMPTimeExceeded` 函数
4. 可以快速验证 traceroute 功能

**长期考虑**：
- 如果需要利用 gVisor 的内置 ICMP 生成，需要重新设计地址管理和路由
- 这需要全面的测试和验证，确保不影响现有功能
- 可以作为后续优化任务
