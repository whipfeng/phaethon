# h_tunnel v1 重构设计：基于 gVisor Netstack

## 背景

当前 h_tunnel 基于复杂的 HTTP 协议，需要重构为更简洁的架构，利用 gVisor netstack 的 TCP/IP 协议栈能力。

## 核心设计

### 1. 非对称架构

**上行（客户端 → 服务端）**：
- 通过现有 mesh P2P 会话的 `peer.Send()` 发送
- 复用现有 h_tunnel HTTP 协议（`htunnelDirectTransport`）
- IP 包封装为 `FrameMeshPacket` 帧，走现有 POST 通道
- 数据发送为 fire-and-forget（不等 HTTP 响应）

**下行（服务端 → 客户端）**：
- 通过现有 mesh P2P 会话返回
- `transport.Recv → HandleMeshFrame → InjectMeshPacket → netstack`
- 从 mesh endpoint 进入 netstack，IP+port 匹配到正确的 socket

**关键：服务端零改动**。服务端已有 `HandleMeshFrame` 处理 `FrameMeshPacket`，无需新增任何逻辑。

### 2. 统一发送路径

**关键原则**：
- hTunnelEndpoint 不持有自己的 transport
- hTunnelEndpoint 通过 `peer.Send()` 发送，复用 peer 的发送队列
- 所有发往同一 peer 的流量（mesh + hTunnelEndpoint）共享同一个 `peer.writeCh`
- 统一 drop-tail 语义（跟交换机一样）

**架构示意**：
```
hTunnelEndpoint.WritePackets ─┐
                              ├→ peer.Send() → peer.writeCh → peerWriteLoop → transport.Send()
mesh 流量 ────────────────────┘

peer.writeCh (16384 buffer, drop-tail)
      │
      ▼
peerWriteLoop (单 goroutine)
      │
      ▼
transport.Send() (htunnelDirectTransport)
      │
      ▼
fire-and-forget HTTP POST
```

**复用现有基础设施**：
- 复用现有的 mesh netstack（不创建新实例）
- 复用现有 P2P 会话的发送路径（peer.writeCh → peerWriteLoop）
- 复用现有 mesh channel 协议（`FrameMeshPacket` 帧类型）
- 复用现有 P2P 会话的接收路径
- 每个 h_tunnel 代理创建一个 uplink-only endpoint

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
    mu     sync.RWMutex
    mtu    uint32
    nicID  tcpip.NICID
    peer   *p2p.Peer  // 目标 peer（通过 P2P 连接）
    closed bool
}

// WritePackets: netstack 调用，将 IP 包封装为 FrameMeshPacket 发送
func (e *hTunnelEndpoint) WritePackets(pkts stack.PacketBufferList) (int, tcpip.Error) {
    for _, pkt := range pkts.AsSlice() {
        data := pkt.ToBuffer().Flatten()
        e.peer.Send(data)  // 复用 peer 的发送队列
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
    → peer.Send(FrameMeshPacket, ipPacket)
    → peer.writeCh (drop-tail)
    → peerWriteLoop → transport.Send()
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
    → peer.writeCh → peerWriteLoop → transport.Send()
    → HTTP POST → 客户端
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
        t.postBatch(buf.Bytes(), seq)
    }()
    return nil  // 立即返回
}
```

**理由**：
- `FrameMeshPacket` 承载的是 IP 包，上层 TCP 有重传机制
- 丢包可接受（TCP 重传恢复）
- 不需要 pending buffer、flushLoop、backpressure 等复杂机制
- peerWriteLoop 不阻塞，可以继续消费 peer.writeCh

**优势：允许包乱序发送**：
- 每个包独立 POST，不等待前一个完成
- 网络层可以并行处理多个 POST
- 上层 TCP 处理乱序和重传

**控制帧（heartbeat/hello/gossip）保持同步**：
- 控制帧需要确认送达
- 使用 `sendControl()` 同步 POST

### 8. 队列与背压（类比交换机）

**现有队列**：
| Channel | 容量 | 位置 | 语义 |
|---------|------|------|------|
| `meshOutboundCh` | 65536 | `tun/engine.go` | TUN 入口队列，drop-tail |
| `peer.writeCh` | 16384 | `p2p/p2p.go` | peer 链路队列，drop-tail |
| `peer.controlCh` | 512 | `p2p/p2p.go` | 控制帧优先队列，drop-tail |
| `meshWriteCh` | ? | `tun/engine.go` | TUN 写入队列，drop-tail |

**类比交换机**：
- 普通交换机：输入端口 → 输入缓冲区 → 交换 → 输出缓冲区 → 输出端口
- 缓冲区满：drop-tail（丢包），靠 TCP 重传恢复
- 没有 PAUSE 帧背压（只有网管型交换机支持）

**我们的设计**：
- 所有队列都是 drop-tail（`select` + `default`）
- 跟普通交换机一样，简单、经过验证
- TCP 的拥塞控制会处理丢包

**为什么不用背压**：
- 从 TUN 进入的包是原始 IP 包，没有 TCP 连接上下文
- 无法做 per-connection 背压
- 转发的流量也是 IP 层，没有 TCP 连接上下文
- 只有本地 TCP 连接的返回流量有连接上下文，但占比小
- drop-tail 足够，TCP 会处理

**潜在问题：fire-and-forget 的 goroutine 堆积**：
- peerWriteLoop 调用 `transport.Send()` 是 fire-and-forget
- 每次调用起一个 goroutine 做 HTTP POST
- 如果网络慢，goroutine 会堆积
- 需要讨论：是否限制并发 POST 数量？

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
func (e *Engine) AddHTunnelEndpoint(proxyName string, peer *p2p.Peer) error
func (e *Engine) RemoveHTunnelEndpoint(proxyName string)
func (e *Engine) HTunnelEndpoint(proxyName string) *hTunnelEndpoint
```

### 2. 集成（main.go）

```go
// h_tunnel 代理连接成功后，获取 peer 并创建 endpoint
peer := p2pManager.GetPeer(proxy.Name)
engine.AddHTunnelEndpoint(proxy.Name, peer)

// 路由：netstack 需要知道哪些目标走 hTunnelEndpoint
// （待实现：路由规则配置）
```

## 优势

1. **零服务端改动**：复用现有 mesh channel 协议
2. **统一路径**：hTunnelEndpoint 和 mesh 流量共享 peer 发送队列
3. **简洁**：hTunnelEndpoint 仅做 uplink，下行完全复用现有路径
4. **高效**：fire-and-forget 发送，允许包乱序，peerWriteLoop 不阻塞
5. **清晰**：上行/下行分离，职责明确

## 文件变更清单

| 文件 | 变更 |
|------|------|
| `tun/htunnel_endpoint.go` | 修改：使用 peer 而非 transport |
| `tun/htunnel_endpoint_test.go` | 修改：适配新接口 |
| `tun/engine.go` | 修改：AddHTunnelEndpoint 参数改为 peer |
| `config/config.go` | h_tunnel 强制 P2P |
| `admin/admin.go` | 拒绝禁用 h_tunnel P2P |
| `dialer/htunnel_direct.go` | sendData 改为 fire-and-forget |
| `main.go` | 集成：创建 endpoint、路由配置 |
| `docs/plans/htunnel_v1_netstack.md` | 本设计文档 |

## 待讨论

### 1. Fire-and-forget 的 goroutine 堆积

peerWriteLoop 调用 `transport.Send()` 是 fire-and-forget，每次起一个 goroutine 做 HTTP POST。如果网络慢，goroutine 会堆积。

**选项**：
- A. 保持现状，依赖 TCP 拥塞控制（简单，但 goroutine 可能很多）
- B. 限制并发 POST 数量（semaphore，超过就阻塞 peerWriteLoop）
- C. 改回同步 POST（peerWriteLoop 阻塞等响应，但失去乱序优势）

### 2. 路由配置

netstack 需要知道哪些目标走 hTunnelEndpoint，哪些走 mesh endpoint。需要设计路由规则。
