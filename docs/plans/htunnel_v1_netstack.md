# h_tunnel v1 重构设计：基于 gVisor Netstack

## 背景

当前 h_tunnel 基于复杂的 HTTP 协议，需要重构为更简洁的架构，利用 gVisor netstack 的 TCP/IP 协议栈能力。

## 核心设计

### 1. 非对称架构

**上行（客户端 → 服务端）**：
- 通过现有 mesh channel 的 `FrameTransport.Send(FrameMeshPacket, data)` 发送
- 复用现有 h_tunnel HTTP 协议（`htunnelDirectTransport`）
- IP 包封装为 `FrameMeshPacket` 帧，走现有 POST 通道
- 数据发送为 fire-and-forget（不等 HTTP 响应）

**下行（服务端 → 客户端）**：
- 通过现有 mesh P2P 会话返回
- `transport.Recv → HandleMeshFrame → InjectMeshPacket → netstack`
- 从 mesh endpoint 进入 netstack，IP+port 匹配到正确的 socket

**关键：服务端零改动**。服务端已有 `HandleMeshFrame` 处理 `FrameMeshPacket`，无需新增任何逻辑。

### 2. 复用现有基础设施

**关键原则**：
- 复用现有的 mesh netstack（不创建新实例）
- 复用现有 `FrameTransport` 接口（多态）
- 复用现有 mesh channel 协议（`FrameMeshPacket` 帧类型）
- 复用现有 P2P 会话的接收路径
- 每个 h_tunnel 代理创建一个 uplink-only endpoint

**架构示意**：
```
现有 Mesh Netstack
├── Mesh Endpoint (GIP: 100.x.x.3)
│   ├── 上行：meshOutboundCh → peer.Send → transport.Send
│   └── 下行：transport.Recv → HandleMeshFrame → InjectMeshPacket
├── h_tunnel Endpoint 1 (uplink only)
│   └── 上行：WritePackets → transport.Send(FrameMeshPacket, data)
│   └── 下行：经由 Mesh Endpoint（IP+port 匹配）
├── h_tunnel Endpoint 2 (uplink only)
│   └── 同上
└── ...
```

### 3. IP 地址策略

**源 IP**：
- 所有 h_tunnel endpoint 使用 GIP（.3 地址）作为源 IP
- GIP 已经分配给 mesh endpoint
- 不需要为 h_tunnel 分配新的 IP

**回程目标 IP**：
- 服务端响应包的目标 IP = 客户端 GIP
- 通过 mesh 网络路由回客户端
- 从 mesh endpoint 进入 netstack

### 4. Endpoint 设计

**每个 h_tunnel 代理一个 uplink-only endpoint**：
```go
type hTunnelEndpoint struct {
    mu        sync.RWMutex
    mtu       uint32
    nicID     tcpip.NICID
    transport frame.FrameTransport  // 多态：htunnelDirectTransport
    closed    bool
}

// WritePackets: netstack 调用，将 IP 包封装为 FrameMeshPacket 发送
func (e *hTunnelEndpoint) WritePackets(pkts stack.PacketBufferList) (int, tcpip.Error) {
    for _, pkt := range pkts.AsSlice() {
        data := pkt.ToBuffer().Flatten()
        e.transport.Send(frame.FrameMeshPacket, data)  // fire-and-forget
    }
}

// Attach: no-op（不需要下行，下行经由 mesh endpoint）
func (e *hTunnelEndpoint) Attach(stack.NetworkDispatcher) {}
func (e *hTunnelEndpoint) IsAttached() bool { return true }
```

### 5. 数据流

**客户端发送（上行）**：
```
应用 → netstack → hTunnelEndpoint.WritePackets
    → transport.Send(FrameMeshPacket, ipPacket)
    → htunnelDirectTransport: fire-and-forget HTTP POST
    → h_tunnel 服务端
```

**服务端接收（已有逻辑，零改动）**：
```
HTTP POST body → frame.ReadFrame → HandleMeshFrame
    → InjectMeshPacket → netstack → 目标应用
```

**服务端响应（下行）**：
```
应用 → netstack → mesh endpoint → peer.Send(FrameMeshPacket)
    → transport.Send → HTTP POST → 客户端
```

**客户端接收（已有逻辑，零改动）**：
```
HTTP GET body → frame.ReadFrame → HandleMeshFrame
    → InjectMeshPacket → mesh endpoint → netstack
    → IP+port 匹配 → 原始 socket
```

### 6. TCP 连接匹配

**关键机制**：
- TCP 连接追踪基于 `(src_ip, src_port, dst_ip, dst_port)`
- 不基于 NIC/endpoint
- 回程包从 mesh endpoint 进入，只要 IP+port 匹配，socket 就能收到

### 7. Fire-and-forget 数据发送

`htunnelDirectTransport.sendData()` 对 `FrameMeshPacket` 采用 fire-and-forget 策略：

```go
func (t *htunnelDirectTransport) sendData(frameType byte, payload []byte) error {
    // 序列化帧
    var buf bytes.Buffer
    frame.WriteFrame(&buf, frameType, payload)

    // 异步发送，不等响应
    go func() {
        // HTTP POST（带加密、序号等现有逻辑）
        t.postBatch(buf.Bytes())
    }()
    return nil  // 立即返回
}
```

**理由**：
- `FrameMeshPacket` 承载的是 IP 包，上层 TCP 有重传机制
- 丢包可接受（TCP 重传恢复）
- 不需要 pending buffer、flushLoop、backpressure 等复杂机制
- 多 socket 并发时 `WritePackets` 不阻塞

**控制帧（heartbeat/hello/gossip）保持同步**：
- 控制帧需要确认送达
- 使用 `sendControl()` 同步 POST

## 实现要点

### 0. h_tunnel 强制启用 P2P

**规则**：
- h_tunnel 类型的代理必须启用 P2P
- `config.Proxy.IsP2P()` 对 h_tunnel 始终返回 true
- Admin API 拒绝禁用 h_tunnel 的 P2P

**原因**：
- h_tunnel 的下行（回程）依赖 mesh P2P 网络
- 如果 P2P 禁用，回程包无法通过 mesh 网络返回

### 1. Engine 管理 hTunnelEndpoint

```go
// Engine 新增字段
htunnelMu        sync.RWMutex
htunnelEndpoints map[string]*hTunnelEndpoint // proxy name → endpoint
htunnelNextNICID atomic.Uint64

// 方法
func (e *Engine) AddHTunnelEndpoint(proxyName string, transport frame.FrameTransport) error
func (e *Engine) RemoveHTunnelEndpoint(proxyName string)
func (e *Engine) HTunnelEndpoint(proxyName string) *hTunnelEndpoint
```

### 2. 集成（main.go）

```go
// h_tunnel 代理连接成功后，创建 endpoint
transport := ... // htunnelDirectTransport (FrameTransport)
engine.AddHTunnelEndpoint(proxy.Name, transport)

// 路由：netstack 需要知道哪些目标走 hTunnelEndpoint
// （待实现：路由规则配置）
```

## 优势

1. **零服务端改动**：复用现有 mesh channel 协议
2. **简洁**：hTunnelEndpoint 仅做 uplink，下行完全复用现有路径
3. **多态**：使用 FrameTransport 接口，不耦合具体实现
4. **高效**：fire-and-forget 发送，不阻塞 netstack
5. **清晰**：上行/下行分离，职责明确

## 文件变更清单

| 文件 | 变更 |
|------|------|
| `tun/htunnel_endpoint.go` | 新增 hTunnelEndpoint（uplink-only） |
| `tun/htunnel_endpoint_test.go` | 单元测试 |
| `tun/engine.go` | 新增 hTunnelEndpoint 管理方法 |
| `config/config.go` | h_tunnel 强制 P2P |
| `admin/admin.go` | 拒绝禁用 h_tunnel P2P |
| `dialer/htunnel_direct.go` | sendData 改为 fire-and-forget |
| `main.go` | 集成：创建 endpoint、路由配置 |
| `docs/plans/htunnel_v1_netstack.md` | 本设计文档 |

## 待讨论：背压策略

fire-and-forget 简化了发送路径，但缺少背压机制。高并发场景下可能产生大量 in-flight HTTP POST。需要讨论：
- 是否需要限制并发 POST 数量？
- 是否需要发送队列 + 批量打包？
- 背压阈值如何确定？
