# libp2p 集成设计方案

**文档版本**: v2.8  
**创建时间**: 2026-10-14  
**最后更新**: 2026-10-14（v2.8: 修正 Flow 为双向规范化，Stream 为全双工）  
**状态**: 设计中  
**优先级**: 高

## 1. 背景与目标

### 1.1 当前问题

当前 mesh P2P 层存在性能瓶颈：

1. **单链路共享**：每个 peer 只有一条 TCP 连接，所有连接争抢带宽
2. **DRR 公平调度局限**：虽然有多队列，但底层是同一链路，无法真正隔离
3. **无流控隔离**：一个慢连接可能影响其他连接的延迟
4. **手动管理复杂**：需要手动实现队列管理、调度、ACK、心跳等

**实测现象**：
- 通过 mesh P2P 访问 MS10 (SSH) 比通过代理模式（GG SOCKS5）慢很多
- 网络路径相同（都经过 QG-JF-GG），但性能差异显著
- 原因：代理模式每个连接独立，mesh P2P 所有连接共享单链路

### 1.2 设计目标

1. **性能提升**：接近代理模式的性能水平
2. **流控隔离**：每个连接独立的流控，避免相互影响
3. **简化代码**：减少手动队列管理、调度等复杂逻辑
4. **保留路由决策**：基于链路质量的智能路由选择
5. **向后兼容**：不影响现有 mesh 路由、DNS、gVisor 等核心功能

### 1.3 非目标

- 不改变 mesh 路由逻辑（RouteSelector 的决策逻辑不变）
- 不改变 DNS 解析机制（DNSHijacker 保持不变）
- 不改变 gVisor netstack 的使用方式
- 不改变底层代理传输（仍然使用 trojan/SOCKS5/H_Tunnel）

## 2. 架构设计

### 2.1 整体架构

```
┌─────────────────────────────────────────────────────────────┐
│  Application Layer (SSH, HTTP, DNS, etc.)                   │
├─────────────────────────────────────────────────────────────┤
│  Mesh Layer                                                  │
│  ├─ RouteSelector (路由决策，集成 LinkQualityMonitor)        │
│  ├─ DNSHijacker (DNS 解析，不变)                            │
│  ├─ MeshManager (mesh 包处理，不变)                         │
│  └─ LinkQualityMonitor (新增：链路质量监控)                 │
├─────────────────────────────────────────────────────────────┤
│  P2P Layer (libp2p 替换 - One Host Per Node)                │
│  ├─ P2PManager (管理器，持有一个 libp2p.Host)               │
│  ├─ libp2p.Host (单一 host，统一 peer ID)                   │
│  │   ├─ Stream Multiplexing (yamux，自动)                   │
│  │   ├─ Connection Management (自动)                        │
│  │   ├─ Protocol Negotiation (自定义 mesh 协议)             │
│  │   └─ Multiple Transports (trojan, socks5, h_tunnel)      │
│  └─ LinkQualityMonitor (按 connID 跟踪)                     │
├─────────────────────────────────────────────────────────────┤
│  Transport Layer (每种协议一个 transport)                 │
│  ├─ TrojanTransport (管理所有 trojan 代理的连接)          │
│  ├─ SOCKS5Transport (管理所有 socks5 代理的连接)          │
│  └─ H_TunnelTransport (管理所有 h_tunnel 代理的连接)      │
├─────────────────────────────────────────────────────────────┤
│  Proxy Connections (不变)                                    │
│  (trojan/SOCKS5/H_Tunnel over TCP)                          │
└─────────────────────────────────────────────────────────────┘
```

**核心设计决策：One Host Per Node（v2.3）**

每个 phaethon 节点对应一个独立的 libp2p.Host 实例：
- 节点 QG → libp2p.Host (peerID=QG)
- 节点 VM → libp2p.Host (peerID=VM)
- 节点 GG → libp2p.Host (peerID=GG)

**每个 Host 配置每种协议一个 Transport**：
- TrojanTransport（管理所有 trojan 代理的连接，Dial + Listen）
- SOCKS5Transport（管理所有 socks5 代理的连接，Dial + Listen）
- H_TunnelTransport（管理所有 h_tunnel 代理的连接，Dial + Listen）

**优势**：
1. **统一 Peer ID**：一个节点一个 peer ID，简化 gossip 和拓扑管理
2. **符合 libp2p 设计**：libp2p 原生支持一个 host 多个 transport
3. **连接复用**：host 级别的连接管理、peer discovery 可以复用
4. **简化架构**：不需要管理多个 host 实例
5. **保留多链路**：每个代理仍然是一个独立的 connection，链路质量独立跟踪

**与 v2.0-v2.2 的区别**：
- **v2.0-v2.2（One Host Per Proxy）**：每个代理一个 host，peer ID 不同
- **v2.3（One Host Per Node）**：每个节点一个 host，每个代理是一个 connection

### 2.2 核心组件

#### 2.2.0 Multiaddr 扩展：代理寻址（v2.5 新增）

**问题**：标准 libp2p 使用 multiaddr 表示网络地址（如 `/ip4/1.2.3.4/tcp/39999`），但我们的 Dial 需要指定通过哪个代理连接，这不是网络层概念。

**方案**：定义 Phaethon 自定义 multiaddr 协议组件，编码代理信息。

**Multiaddr 格式**：
```
/phaethon/proxy/<proxyName>

示例：
/phaethon/proxy/trojan-to-gg      # 通过 trojan-to-gg 代理连接
/phaethon/proxy/socks5-1          # 通过 socks5-1 代理连接
/phaethon/proxy/htunnel-1         # 通过 htunnel-1 代理连接
```

**协议注册**：
```go
// 在 init() 中注册自定义协议
func init() {
    // 注册 /phaethon/proxy 协议
    multiaddr.AddProtocol(multiaddr.Protocol{
        Name:  "phaethon-proxy",
        Code:  0x0F01,  // 实验性协议范围 (0x0F00-0x0FFF)
        Size:  multiaddr.LengthPrefixedVarSize,
    })
}

// 构造代理地址
func NewProxyMultiaddr(proxyName string) (multiaddr.Multiaddr, error) {
    return multiaddr.NewMultiaddr("/phaethon/proxy/" + proxyName)
}

// 从 multiaddr 提取代理名
func ExtractProxyName(addr multiaddr.Multiaddr) (string, bool) {
    for _, c := range addr.Components() {
        if c.Protocol().Code == 0x0F01 {
            return c.Value(), true
        }
    }
    return "", false
}
```

**Transport 匹配**：
```go
// TrojanTransport.CanDial 检查地址是否匹配
func (t *TrojanTransport) CanDial(addr multiaddr.Multiaddr) bool {
    proxyName, ok := ExtractProxyName(addr)
    if !ok {
        return false
    }
    // 检查是否是本 transport 管理的 trojan 代理
    _, exists := t.proxies[proxyName]
    return exists
}

// SOCKS5Transport.CanDial 同理
func (t *SOCKS5Transport) CanDial(addr multiaddr.Multiaddr) bool {
    proxyName, ok := ExtractProxyName(addr)
    if !ok {
        return false
    }
    _, exists := t.proxies[proxyName]
    return exists
}
```

**虚拟监听地址**：
```
/phaethon/inject/<protocol>

示例：
/phaethon/inject/trojan     # TrojanTransport 的注入监听
/phaethon/inject/socks5     # SOCKS5Transport 的注入监听
/phaethon/inject/htunnel    # H_TunnelTransport 的注入监听
```

**Listen 流程**：
```go
// P2PManager 配置 host 监听虚拟地址
h, err := libp2p.New(
    libp2p.ListenAddrStrings(
        "/phaethon/inject/trojan",
        "/phaethon/inject/socks5",
        "/phaethon/inject/htunnel",
    ),
    // ...
)

// libp2p swarm 会调用对应 transport 的 Listen()
// TrojanTransport.Listen("/phaethon/inject/trojan") → 返回从 connChan 读取的 listener
```

**Peer AddrInfo 构造**：
```go
// 连接到 peer 时，构造包含代理信息的 AddrInfo
// 注意：peer.ID 由 libp2p 自动管理，我们不需要关心
func (m *P2PManager) ConnectViaProxy(ctx context.Context, proxyName string, remotePeerID peer.ID) error {
    // 构造代理地址
    proxyAddr, _ := NewProxyMultiaddr(proxyName)
    
    // 构造 AddrInfo（peer.ID 由 libp2p 内部使用）
    addrInfo := peer.AddrInfo{
        ID:    remotePeerID,
        Addrs: []multiaddr.Multiaddr{proxyAddr},
    }
    
    // libp2p 会根据地址选择合适的 transport
    return m.host.Connect(ctx, addrInfo)
}
```

**注意（v2.7 澄清）**：
- ✅ **peer.ID 是 libp2p 内部概念**：由 libp2p 自动生成和管理，对我们完全透明
- ✅ **我们只关心 NodeID 和 connID**：路由基于 (nodeID, connID)，不依赖 proxyName
- ✅ **LinkNIC 直接持有 connection**：每个 LinkNIC 代表一条连接，是核心抽象
- ✅ **proxyName 仅用于 V2 模式**：主动端记录 proxyName ↔ LinkNIC 关系，用于路由 hook 设置
- ✅ **配置完全复用现有代理架构**：不需要任何新的配置
- ✅ **Stream 按 flow 创建**：每个 flow（TCP/UDP 5-tuple、ICMP identifier 等）一个 stream，实现公平性

#### 2.2.1 多链路架构（v2.5：One Host Per Node + 每种协议一个 Transport）

**当前实现的多链路设计**：

```
节点 A 和节点 B 之间：
- 配置了 3 个代理：trojan-1, socks5-2, htunnel-3
- 建立 3 条独立的 connection（通过同一个 host）：
  ├─ Connection 1 (trojan-1):   A → trojan-1 → B
  ├─ Connection 2 (socks5-2):   A → socks5-2 → B
  └─ Connection 3 (htunnel-3):  A → htunnel-3 → B
  
- 每条 connection 独立管理
- RouteSelector 根据链路质量选择使用哪条 connection
```

**链路标识**：

```
主动端（A）发起连接：
- proxyName: "trojan-1"（主动端才知道用的哪个代理）
- connID: libp2p connection ID（两端可见）
- peerID: B 的 peer ID

被动端（B）接收连接：
- 没有 proxyName（不知道 A 用的哪个代理）
- connID: libp2p connection ID（和 A 端相同）
- peerID: A 的 peer ID

同一条 connection，两端的 connID 是相同的！
A 的视角：到 B 的链路 = (B, connID="abc123")
B 的视角：到 A 的链路 = (A, connID="abc123")
```

**libp2p 集成策略（v2.5: One Host Per Node + 每种协议一个 Transport）**：

1. **每个节点一个 libp2p.Host**：整个节点共享一个 host 实例
2. **每种协议一个 transport**（不是每个 proxy 一个）：
   - TrojanTransport（处理所有 trojan 类型的代理连接）
   - SOCKS5Transport（处理所有 socks5 类型的代理连接）
   - H_TunnelTransport（处理所有 h_tunnel 类型的代理连接）
3. **每个 transport 管理多个同协议代理**：TrojanTransport 内部有 trojan-1, trojan-2 等多个 dialer
4. **统一 peer ID**：整个节点一个 peer ID，简化 gossip 和拓扑管理
5. **LinkQualityMonitor 按 connID 跟踪**：
   - 两端都按 connID 跟踪（connID 是全局唯一的）
   - 主动端额外记录 proxyName（用于配置查找）

```go
type P2PManager struct {
    // 单一 libp2p.Host（整个节点共享）
    host host.Host
    
    // 代理配置（用于添加到 transport）
    proxies map[string]*config.Proxy  // proxyName -> proxy config
    
    // Connection 跟踪
    connections map[string]*connectionInfo  // connID -> info
    
    // 带宽统计
    bwReporter *metrics.BandwidthCounter
    
    // 链路质量监控
    monitor *LinkQualityMonitor
    
    // mesh 协议处理器
    meshHandler *MeshStreamHandler
    
    ctx    context.Context
    cancel context.CancelFunc
}

type connectionInfo struct {
    proxyName string   // 代理名称（主动端才知道）
    peerID    peer.ID  // 对端 peer ID
    connID    string   // libp2p connection ID
    nodeID    string   // 对端节点 ID（mesh 层概念）
}

// 创建单一 libp2p.Host（配置每种协议一个 transport）
func (m *P2PManager) createHost() (host.Host, error) {
    // 创建每种协议的 transport（全局唯一）
    trojanTransport := NewTrojanTransport()
    socks5Transport := NewSOCKS5Transport()
    htunnelTransport := NewH_TunnelTransport()
    
    // 根据 proxy 配置，添加 dialer 到对应的 transport
    for _, proxy := range m.proxies {
        switch proxy.Type {
        case config.ProxyTROJAN:
            trojanTransport.AddProxy(proxy.Name, proxy)
        case config.ProxySOCKS5:
            socks5Transport.AddProxy(proxy.Name, proxy)
        case config.ProxyH_TUNNEL:
            htunnelTransport.AddProxy(proxy.Name, proxy)
        }
    }
    
    // 注册到现有的 server（如果本节点有对应的服务端）
    if trojanServer := m.getTrojanServer(); trojanServer != nil {
        trojanServer.SetP2PTransport(trojanTransport)
    }
    if socks5Server := m.getSocks5Server(); socks5Server != nil {
        socks5Server.SetP2PTransport(socks5Transport)
    }
    if htunnelServer := m.getHTunnelServer(); htunnelServer != nil {
        htunnelServer.SetP2PTransport(htunnelTransport)
    }
    
    // 创建一个 host，配置所有 transport
    h, err := libp2p.New(
        libp2p.NoTransports,
        libp2p.Transport(trojanTransport),
        libp2p.Transport(socks5Transport),
        libp2p.Transport(htunnelTransport),
        libp2p.DefaultMuxers,  // yamux
        libp2p.DefaultSecurity,
        libp2p.ListenAddrStrings(
            "/phaethon/inject/trojan",
            "/phaethon/inject/socks5",
            "/phaethon/inject/htunnel",
        ),
        libp2p.BandwidthReporter(m.bwReporter),
    )
    return h, err
}

// 通过指定代理连接到 peer
func (m *P2PManager) ConnectViaProxy(ctx context.Context, proxyName string, remotePeerID peer.ID) error {
    // 构造代理地址
    proxyAddr, _ := NewProxyMultiaddr(proxyName)
    
    // 构造 AddrInfo
    addrInfo := peer.AddrInfo{
        ID:    remotePeerID,
        Addrs: []multiaddr.Multiaddr{proxyAddr},
    }
    
    // libp2p 会根据地址选择合适的 transport
    return m.host.Connect(ctx, addrInfo)
}
```

#### 2.2.1.1 P2PManager（管理器，持有一个 libp2p.Host）

**职责**：
- 管理一个 libp2p.Host 实例（整个节点共享）
- 配置每种协议一个 transport（trojan, socks5, h_tunnel）
- 管理 connection 跟踪（每个代理是一个 connection）
- 协调链路质量监控

**接口设计**：
```go
type P2PManager struct {
    // 单一 libp2p.Host（整个节点共享）
    host host.Host
    
    // Transport 实例（每种协议一个）
    trojanTransport  *TrojanTransport
    socks5Transport  *SOCKS5Transport
    htunnelTransport *H_TunnelTransport
    
    // 代理配置
    proxies map[string]*config.Proxy  // proxyName -> proxy config
    
    // Connection 跟踪
    mu          sync.RWMutex
    connections map[string]*connectionInfo  // connID -> info
    
    // 带宽统计
    bwReporter *metrics.BandwidthCounter
    
    // 链路质量监控
    monitor *LinkQualityMonitor
    
    // mesh 协议处理器
    meshHandler *MeshStreamHandler
    
    // 本地节点信息
    localNodeID string
    meshNodeID  string
    meshVIP     string
    
    ctx    context.Context
    cancel context.CancelFunc
}

type connectionInfo struct {
    proxyName string   // 代理名称（主动端才知道）
    peerID    peer.ID  // 对端 peer ID
    connID    string   // libp2p connection ID
    nodeID    string   // 对端节点 ID（mesh 层概念）
}

// 初始化 P2PManager（创建单一 host，配置每种协议一个 transport）
func NewP2PManager(localNodeID string, proxies map[string]*config.Proxy) (*P2PManager, error) {
    mgr := &P2PManager{
        proxies:     proxies,
        connections: make(map[string]*connectionInfo),
        bwReporter:  metrics.NewBandwidthCounter(),
        localNodeID: localNodeID,
    }
    
    // 创建单一 host，配置所有 transport
    h, err := mgr.createHost()
    if err != nil {
        return nil, fmt.Errorf("create host: %w", err)
    }
    mgr.host = h
    
    // 注册 mesh 协议处理器
    h.SetStreamHandler(meshProtocol, mgr.meshHandler.HandleStream)
    
    return mgr, nil
}

// 创建单一 libp2p.Host（配置每种协议一个 transport）
func (m *P2PManager) createHost() (host.Host, error) {
    // 创建每种协议的 transport（全局唯一）
    m.trojanTransport = NewTrojanTransport()
    m.socks5Transport = NewSOCKS5Transport()
    m.htunnelTransport = NewH_TunnelTransport()
    
    // 根据 proxy 配置，添加 dialer 到对应的 transport
    for _, proxy := range m.proxies {
        switch proxy.Type {
        case config.ProxyTROJAN:
            m.trojanTransport.AddProxy(proxy.Name, proxy)
        case config.ProxySOCKS5:
            m.socks5Transport.AddProxy(proxy.Name, proxy)
        case config.ProxyH_TUNNEL:
            m.htunnelTransport.AddProxy(proxy.Name, proxy)
        }
    }
    
    // 注册到现有的 server（如果本节点有对应的服务端）
    if trojanServer := m.getTrojanServer(); trojanServer != nil {
        trojanServer.SetP2PTransport(m.trojanTransport)
    }
    if socks5Server := m.getSocks5Server(); socks5Server != nil {
        socks5Server.SetP2PTransport(m.socks5Transport)
    }
    if htunnelServer := m.getHTunnelServer(); htunnelServer != nil {
        htunnelServer.SetP2PTransport(m.htunnelTransport)
    }
    
    // 创建一个 host，配置所有 transport
    h, err := libp2p.New(
        libp2p.NoTransports,
        libp2p.Transport(m.trojanTransport),
        libp2p.Transport(m.socks5Transport),
        libp2p.Transport(m.htunnelTransport),
        libp2p.DefaultMuxers,  // yamux
        libp2p.DefaultSecurity,
        libp2p.ListenAddrStrings(
            "/phaethon/inject/trojan",
            "/phaethon/inject/socks5",
            "/phaethon/inject/htunnel",
        ),
        libp2p.BandwidthReporter(m.bwReporter),
    )
    return h, err
}

// ConnectViaProxy 通过指定代理连接到 peer
func (m *P2PManager) ConnectViaProxy(ctx context.Context, proxyName string, remotePeerID peer.ID) error {
    // 构造代理地址
    proxyAddr, _ := NewProxyMultiaddr(proxyName)
    
    // 构造 AddrInfo
    addrInfo := peer.AddrInfo{
        ID:    remotePeerID,
        Addrs: []multiaddr.Multiaddr{proxyAddr},
    }
    
    // libp2p 会根据地址自动选择合适的 transport
    err := m.host.Connect(ctx, addrInfo)
    if err != nil {
        return err
    }
    
    // 记录 connection 信息
    conns := m.host.Network().ConnsToPeer(remotePeerID)
    if len(conns) > 0 {
        conn := conns[0]
        m.connections[conn.ID()] = &connectionInfo{
            proxyName: proxyName,
            peerID:    remotePeerID,
            connID:    conn.ID(),
        }
    }
    
    return nil
}

// 关闭 host
func (m *P2PManager) Close() error {
    m.cancel()
    return m.host.Close()
}
```

#### 2.2.1.2 Connect 和 Listen 适配（每种协议一个 Transport）

**核心原则**：
**每种协议一个 Transport 实例**（不是每个 proxy 一个），该 transport 同时处理该协议的 Dial 和 Listen。多个同协议的 server 都注入到同一个 transport。

**架构示意图**：
```
libp2p.Host (一个节点一个)
├─ TrojanTransport (唯一的 trojan transport)
│   ├─ Dial: 通过 trojan-1/trojan-2/... 代理拨号
│   └─ Listen: 接受来自所有 TrojanServer 的 P2P 连接
│       ├─ TrojanServer :39999 注入的 P2P 连接
│       ├─ TrojanServer :39998 注入的 P2P 连接
│       └─ 其他 TrojanServer 注入的 P2P 连接
│
├─ SOCKS5Transport (唯一的 SOCKS5 transport)
│   ├─ Dial: 通过 socks5-1/socks5-2/... 代理拨号
│   └─ Listen: 接受来自所有 Socks5Server 的 P2P 连接
│       ├─ Socks5Server :1080 注入的 P2P 连接
│       └─ 其他 Socks5Server 注入的 P2P 连接
│
└─ H_TunnelTransport (唯一的 H_Tunnel transport)
    ├─ Dial: 通过 htunnel-1/htunnel-2/... 代理拨号
    └─ Listen: 接受来自所有 HTunnelServer 的 P2P 连接
        └─ HTunnelServer :8080 注入的 P2P 连接
```

**现有架构（已实现）**：

1. **代理服务器已经在监听**（`main_engine.go` 启动）：
   ```go
   // server/trojan.go - TrojanServer 已经在监听
   type TrojanServer struct {
       BaseServer
       Password string
   }
   func (s *TrojanServer) Serve(listener net.Listener)  // 已经在 main_engine.go 启动
   func (s *TrojanServer) HandleConn(clientConn net.Conn) {
       // 解析 Trojan 协议，获取 dstPort
       if dstPort == reverse.BindPortP2P {  // line 117
           // TODO: libp2p integration - 这里应该交给 P2P transport 处理
       }
   }
   
   // server/socks5.go - Socks5Server 已经在监听
   func (s *Socks5Server) HandleConn(clientConn net.Conn) {
       // 解析 SOCKS5 协议，获取 dstPort
       if dstPort == reverse.BindPortP2P {  // line 158
           // TODO: libp2p integration - 这里应该交给 P2P transport 处理
       }
   }
   ```

2. **代理客户端已经存在**（用于 Dial）：
   ```go
   // dialer/trojan.go
   type TrojanDialer struct {
       proxy *config.Proxy
   }
   func (d *TrojanDialer) DialP2P() (frame.FrameTransport, error)
   ```

**适配方案（v2.5：每种协议一个 Transport）**：

1. **TrojanTransport（全局唯一，处理所有 trojan 连接）**：
   ```go
   type TrojanTransport struct {
       // 代理配置池（用于 Dial，支持多个 trojan 代理）
       proxies map[string]*dialer.TrojanDialer  // proxyName -> dialer
       
       // 接受连接的通道（从所有 TrojanServer 接收 port=2 连接）
       connChan chan net.Conn
   }
   
   func NewTrojanTransport() *TrojanTransport {
       return &TrojanTransport{
           proxies:  make(map[string]*dialer.TrojanDialer),
           connChan: make(chan net.Conn, 16),  // 缓冲通道
       }
   }
   
   // AddProxy 添加一个 trojan 代理（用于 Dial）
   func (t *TrojanTransport) AddProxy(proxyName string, proxy *config.Proxy) {
       dialer := dialer.NewDialer(proxy).(*dialer.TrojanDialer)
       t.proxies[proxyName] = dialer
   }
   ```

2. **Dial 方法（通过指定代理拨号）**：
   ```go
   func (t *TrojanTransport) Dial(ctx context.Context, addr multiaddr.Multiaddr, p peer.ID) (transport.CapableConn, error) {
       // 从地址中解析出要使用的 proxyName
       proxyName := extractProxyName(addr)
       
       dialer, ok := t.proxies[proxyName]
       if !ok {
           return nil, fmt.Errorf("proxy not found: %s", proxyName)
       }
       
       // 复用现有的 dialer.DialP2P()
       frameTransport, err := dialer.DialP2P()
       if err != nil {
           return nil, fmt.Errorf("trojan dial failed: %w", err)
       }
       
       // 包装成 libp2p CapableConn
       return newTrojanCapableConn(frameTransport, p)
   }
   ```

3. **Listen 方法（从所有 server 接收连接）**：
   ```go
   func (t *TrojanTransport) Listen(addr multiaddr.Multiaddr) (transport.Listener, error) {
       // 不创建自己的 listener！
       // 返回一个假的 listener，实际连接从 connChan 获取
       return &trojanListener{
           transport: t,
           connChan:  t.connChan,
       }, nil
   }
   
   // InjectConnection 供所有 TrojanServer 调用，注入 port=2 连接
   func (t *TrojanTransport) InjectConnection(conn net.Conn) {
       select {
       case t.connChan <- conn:
           util.LogModuleInfo("P2P", "Injected P2P connection to TrojanTransport")
       default:
           util.LogModuleError("P2P", "P2P connection channel full, dropping connection")
           conn.Close()
       }
   }
   ```

4. **Listener 实现（从通道接受连接）**：
   ```go
   type trojanListener struct {
       transport *TrojanTransport
       connChan  chan net.Conn
   }
   
   func (l *trojanListener) Accept() (transport.CapableConn, error) {
       // 从通道获取连接（由所有 TrojanServer 注入）
       conn := <-l.connChan
       return newTrojanCapableConn(conn, "")
   }
   
   func (l *trojanListener) Close() error {
       // 不关闭任何 listener（因为没有自己的 listener）
       return nil
   }
   
   func (l *trojanListener) Addr() net.Addr {
       // 返回假地址（实际不监听任何端口）
       addr, _ := net.ResolveTCPAddr("tcp", "0.0.0.0:0")
       return addr
   }
   ```

5. **修改 server.TrojanServer.HandleConn()（注入 P2P 连接）**：
   ```go
   // server/trojan.go
   func (s *TrojanServer) HandleConn(clientConn net.Conn) {
       // ... 解析 Trojan 协议 ...
       
       if dstPort == reverse.BindPortP2P {
           // 不是自己处理，而是交给 P2P transport
           if p2pTransport := s.GetP2PTransport(); p2pTransport != nil {
               p2pTransport.InjectConnection(clientConn)
               return  // 连接已交给 P2P，这里不关闭
           }
           // fallback: 旧的 P2P 处理逻辑（如果 P2P 未启用）
           s.handleP2PConnection(clientConn)
           return
       }
       
       // ... 处理普通 Trojan 连接 ...
   }
   ```

6. **SOCKS5 和 H_Tunnel 同理**：
   ```go
   // SOCKS5Transport（全局唯一，处理所有 SOCKS5 连接）
   type SOCKS5Transport struct {
       proxies  map[string]*dialer.Socks5Dialer
       connChan chan net.Conn  // 从所有 Socks5Server 接收连接
   }
   
   func (t *SOCKS5Transport) InjectConnection(conn net.Conn) {
       t.connChan <- conn
   }
   
   // H_TunnelTransport（全局唯一，处理所有 H_Tunnel 连接）
   type H_TunnelTransport struct {
       proxies  map[string]*dialer.HTunnelDialer
       connChan chan net.Conn
   }
   
   func (t *H_TunnelTransport) InjectConnection(conn net.Conn) {
       t.connChan <- conn
   }
   ```

7. **P2PManager 配置（创建每种协议一个 transport）**：
   ```go
   func (m *P2PManager) createHost() (host.Host, error) {
       // 创建每种协议的 transport（全局唯一）
       trojanTransport := NewTrojanTransport()
       socks5Transport := NewSOCKS5Transport()
       htunnelTransport := NewH_TunnelTransport()
       
       // 根据 proxy 配置，添加 dialer 到对应的 transport
       for _, proxy := range m.proxies {
           switch proxy.Type {
           case config.ProxyTROJAN:
               trojanTransport.AddProxy(proxy.Name, proxy)
           case config.ProxySOCKS5:
               socks5Transport.AddProxy(proxy.Name, proxy)
           case config.ProxyH_TUNNEL:
               htunnelTransport.AddProxy(proxy.Name, proxy)
           }
       }
       
       // 注册到现有的 server（如果本节点有对应的服务端）
       if trojanServer := m.getTrojanServer(); trojanServer != nil {
           trojanServer.SetP2PTransport(trojanTransport)
       }
       if socks5Server := m.getSocks5Server(); socks5Server != nil {
           socks5Server.SetP2PTransport(socks5Transport)
       }
       if htunnelServer := m.getHTunnelServer(); htunnelServer != nil {
           htunnelServer.SetP2PTransport(htunnelTransport)
       }
       
       // 创建 host，配置所有 transport（不监听任何端口！）
       h, err := libp2p.New(
           libp2p.NoTransports,
           libp2p.Transport(trojanTransport),
           libp2p.Transport(socks5Transport),
           libp2p.Transport(htunnelTransport),
           libp2p.DefaultMuxers,
           libp2p.DefaultSecurity,
           libp2p.ListenAddrStrings(),  // 空字符串 = 不监听
           libp2p.BandwidthReporter(m.bwReporter),
       )
       return h, err
   }
   ```

8. **配置示例（复用现有 config.Proxy 和 config.Mapping）**：
   ```yaml
   # QG 节点（既是客户端也是服务端）
   proxies:
     # 作为客户端连接到 GG（trojan-1）
     - name: trojan-to-gg
       type: trojan
       server: 106.13.183.103
       port: 39999
       password: xxx
       
     # 作为客户端连接到 JF（trojan-2）
     - name: trojan-to-jf
       type: trojan
       server: 36.140.28.178
       port: 18080
       password: yyy
       
   mappings:
     # 作为服务端接受连接（TrojanServer 已经在监听 :39999）
     - name: trojan-server-1
       type: trojan
       listen: :39999
       password: zzz
       
     # 作为服务端接受连接（TrojanServer 已经在监听 :39998）
     - name: trojan-server-2
       type: trojan
       listen: :39998
       password: www
   
   # VM 节点（只有客户端）
   proxies:
     # 只作为客户端连接到 QG
     - name: trojan-to-qg
       type: trojan
       server: 10.11.61.41
       port: 39999
       password: xxx
   ```

**数据流（被动端接收连接）**：
```
1. 多个 TrojanServer 已经在监听（:39999, :39998, ...）
   ↓
2. 客户端连接到达任意一个 TrojanServer，HandleConn() 解析协议
   ↓
3. 检测到 dstPort == BindPortP2P (2)
   ↓
4. 调用 trojanTransport.InjectConnection(conn)（所有 server 都注入到同一个 transport）
   ↓
5. 连接进入 trojanTransport.connChan
   ↓
6. trojanListener.Accept() 从 connChan 获取连接
   ↓
7. libp2p.Host 处理连接（yamux 多路复用）
   ↓
8. MeshStreamHandler.HandleStream() 处理 mesh 包
```

**总结**：
- ✅ **每种协议一个 Transport**：TrojanTransport、SOCKS5Transport、H_TunnelTransport 各一个实例
- ✅ **Transport 处理 Dial 和 Listen**：Dial 通过代理客户端，Listen 从 connChan 接受注入的连接
- ✅ **多个 Server 注入同一个 Transport**：所有 TrojanServer 检测到 port=2 后都注入到同一个 TrojanTransport
- ✅ **不创建新的 listener**：复用现有 server 的监听，transport 只接受注入的连接
- ✅ **支持多代理**：一个 transport 可以管理多个同协议的代理（trojan-1, trojan-2, ...）
- ✅ **符合现有实现**：server/trojan.go:117 和 server/socks5.go:158 已经有 port=2 检测逻辑

#### 2.2.2 LinkQualityMonitor（新增）

**职责**：
- 测量到直接邻居的链路质量（RTT、Jitter、错误率、吞吐量）
- **按链路跟踪**：主动端按 (proxyName, localSeq)，被动端按 localSeq
- 通过 gossip 共享链路质量信息
- 为 RouteSelector 提供链路质量数据

**核心设计决策（v2.2：利用 libp2p 原生能力）**：

1. **RTT 和 Jitter**：使用 libp2p 内置 ping 协议测量
   - ping 协议提供精确的 RTT 测量
   - 通过连续 ping 的 RTT 差异计算 jitter（平均绝对偏差）
   
2. **吞吐量**：使用 libp2p 的 BandwidthCounter
   - 每个 libp2p.Host 配置 BandwidthReporter
   - BandwidthCounter 自动跟踪每个 peer 的 RateIn/RateOut
   - 无需手动统计，libp2p 在底层自动收集
   
3. **错误率**：通过 ping 失败次数计算
   - TotalPings 和 FailedPings 统计
   - ErrorRate = FailedPings / TotalPings

**优势**：
- ✅ 减少手动统计代码
- ✅ 利用 libp2p 已经实现的精确测量
- ✅ 吞吐量统计更准确（底层网络层直接统计）
- ✅ 保留 ping 用于 RTT/Jitter（这些无法从 BandwidthCounter 获取）

**核心结构**：
```go
type LinkQualityMonitor struct {
    mu     sync.RWMutex
    p2pMgr *P2PManager  // 引用 P2PManager（持有单一 host 和 bwReporter）
    localID string
    
    // 按 connID 跟踪（connID 是全局唯一的链路标识）
    links map[string]*LinkMetrics  // connID -> metrics
    
    // 配置
    probeInterval time.Duration  // ping 探测间隔（默认 5s）
    probeTimeout  time.Duration  // ping 超时（默认 3s）
    
    // 链路质量数据库（自己的测量 + gossip 获取的）
    linkDatabase map[string]map[string]*LinkInfo  // from -> to -> quality
}

type LinkMetrics struct {
    // 直接测量
    RTT         float64       // 平均 RTT (ms) - 通过 ping 测量
    RTTMin      float64       // 最小 RTT
    RTTMax      float64       // 最大 RTT
    Jitter      float64       // RTT jitter (ms) - 平均绝对偏差，通过连续 ping 计算
    ErrorRate   float64       // 错误率 (0-1) - FailedPings / TotalPings
    Throughput  int64         // 吞吐量 (bytes/sec) - 从 BandwidthCounter 获取
    
    // 统计
    TotalPings  uint64        // 总 ping 次数
    FailedPings uint64        // 失败 ping 次数
    
    // 综合质量分数 (0-100)
    QualityScore float64
    
    // 最后更新时间
    LastUpdated time.Time
    
    // 内部状态
    lastRTT float64           // 上次 RTT，用于计算 jitter
}
```

**P2PManager 集成 BandwidthCounter**：
```go
type P2PManager struct {
    host host.Host  // 单一 host
    proxies map[string]*config.Proxy
    peerInfo map[peer.ID]*peerInfo
    
    // libp2p 带宽统计
    bwReporter *metrics.BandwidthCounter  // 单一 host 使用
    
    monitor *LinkQualityMonitor
    // ...
}

func NewP2PManager(localNodeID string) *P2PManager {
    mgr := &P2PManager{
        proxies: make(map[string]*config.Proxy),
        peerInfo: make(map[peer.ID]*peerInfo),
        bwReporter: metrics.NewBandwidthCounter(),  // 创建带宽统计器
    }
    // ...
}
```

**获取链路质量统计**：
```go
// GetLinkQualityStats 获取链路质量统计（实现 mesh.P2PTransport 接口）
// 链路全局标识：nodeID + connID
func (m *P2PManager) GetLinkQualityStats(nodeID string) (srtt, rto, jitter time.Duration, lossRate float64, sampleCount int) {
    // 通过 nodeID 查找 peer
    for _, info := range m.peerInfo {
        if info.nodeID == nodeID && info.connID != "" {
            // 从 monitor 获取 RTT、Jitter、ErrorRate（底层用 connID 查找）
            metrics := m.monitor.GetLinkQuality(info.connID)
            if metrics != nil {
                srtt = time.Duration(metrics.RTT) * time.Millisecond
                rto = srtt * 2
                jitter = time.Duration(metrics.Jitter) * time.Millisecond
                lossRate = metrics.ErrorRate
                sampleCount = int(metrics.TotalPings)
                
                // 从 BandwidthCounter 获取吞吐量
                bwStats := m.bwReporter.GetBandwidthForPeer(info.peerID)
                metrics.Throughput = int64((bwStats.RateIn + bwStats.RateOut) / 2)
            }
            return
        }
    }
    return 0, 0, 0, 0.0, 0
}

// GetLinkQualityStatsByProxy 按代理名查询（多链路场景）
func (m *P2PManager) GetLinkQualityStatsByProxy(proxyName string) (srtt, rto, jitter time.Duration, lossRate float64, sampleCount int)

// GetLinkQualityStatsByLinkID 按连接 ID 查询（精确到具体链路）
func (m *P2PManager) GetLinkQualityStatsByLinkID(linkID string) (srtt, rto, jitter time.Duration, lossRate float64, sampleCount int)
```

**设计说明**：
- **链路全局标识**：(nodeID, connID) 组合唯一标识一条链路
- **三种查询方式**：
  - `GetLinkQualityStats(nodeID)`：最常用，按节点 ID 查询
  - `GetLinkQualityStatsByProxy(proxyName)`：多链路场景，按代理名查询
  - `GetLinkQualityStatsByLinkID(linkID)`：精确查询，按连接 ID 查询
- **底层实现**：所有方法最终都通过 connID 从 monitor 获取指标

**RTT 和 Jitter 测量**：
```go
// 使用 libp2p 内置 ping 协议（单一 host 探测所有 peer）
func (lqm *LinkQualityMonitor) probeRTT() {
    ticker := time.NewTicker(lqm.probeInterval)
    defer ticker.Stop()
    
    for {
        select {
        case <-ticker.C:
            // 遍历单一 host 连接的所有 peer
            for _, conn := range lqm.p2pMgr.host.Network().Conns() {
                peerID := conn.RemotePeer()
                connID := conn.ID()
                go func(cid string, host host.Host, pid peer.ID) {
                    // libp2p 内置 ping
                    result := <-ping.Ping(lqm.ctx, host, pid)
                    if result.Error != nil {
                        lqm.recordPingFailure(cid, pid)
                    } else {
                        lqm.recordRTT(cid, pid, result.RTT)
                    }
                }(connID, lqm.p2pMgr.host, peerID)
            }
        case <-lqm.stopCh:
            return
        }
    }
}

// 记录 RTT 并计算 Jitter
func (lqm *LinkQualityMonitor) recordRTT(connID string, peerID peer.ID, rtt time.Duration) {
    lqm.mu.Lock()
    defer lqm.mu.Unlock()
    
    metrics := lqm.getOrCreateMetrics(connID)
    metrics.TotalPings++
    metrics.updateRTT(float64(rtt.Milliseconds()))
    metrics.LastUpdated = time.Now()
    metrics.QualityScore = calculateQualityScore(metrics)
}

// 更新 RTT 统计并计算 Jitter
func (m *LinkMetrics) updateRTT(rttMs float64) {
    // 计算 jitter（平均绝对偏差）
    if m.lastRTT > 0 {
        deviation := math.Abs(rttMs - m.lastRTT)
        if m.Jitter == 0 {
            m.Jitter = deviation
        } else {
            m.Jitter = m.Jitter*0.8 + deviation*0.2  // EMA 平滑
        }
    }
    m.lastRTT = rttMs
    
    // 更新 RTT 统计（EMA 平滑）
    if m.RTT == 0 {
        m.RTT = rttMs
        m.RTTMin = rttMs
        m.RTTMax = rttMs
    } else {
        m.RTT = m.RTT*0.8 + rttMs*0.2
        if rttMs < m.RTTMin {
            m.RTTMin = rttMs
        }
        if rttMs > m.RTTMax {
            m.RTTMax = rttMs
        }
    }
}
```

**质量分数计算（v2.2：包含 Jitter）**：
```go
func calculateQualityScore(m *LinkMetrics) float64 {
    // RTT 分数：越低越好（基准：100ms = 0 分，10ms = 100 分）
    rttScore := math.Max(0, 100-(m.RTT-10))
    
    // Jitter 分数：越低越好（基准：50ms = 0 分，0ms = 100 分）
    jitterScore := math.Max(0, 100-m.Jitter*2)
    
    // 错误率分数：越低越好
    errorScore := (1 - m.ErrorRate) * 100
    
    // 吞吐量分数：越高越好（对数刻度）
    throughputScore := math.Min(100, math.Log10(float64(m.Throughput)/1000+1)*20)
    
    // 加权平均（v2.2：加入 Jitter 权重）
    score := rttScore*0.3 + jitterScore*0.2 + errorScore*0.3 + throughputScore*0.2
    
    return math.Max(0, math.Min(100, score))
}
```

**指标来源总结**：

| 指标 | 来源 | 说明 |
|------|------|------|
| RTT | libp2p ping | 精确的往返时间测量 |
| Jitter | 连续 ping 计算 | RTT 的平均绝对偏差 |
| ErrorRate | ping 统计 | FailedPings / TotalPings |
| Throughput | BandwidthCounter | libp2p 底层自动统计的收发速率 |
| QualityScore | 综合计算 | 加权平均上述指标 |

**为什么保留 ping**：
- ✅ RTT 和 Jitter 无法从 BandwidthCounter 获取
- ✅ ping 协议轻量级，开销小（默认 5s 一次）
- ✅ 提供精确的延迟和稳定性测量
- ✅ 错误率统计需要 ping 探测

**为什么使用 BandwidthCounter**：
- ✅ 吞吐量统计更准确（底层网络层直接统计）
- ✅ 无需手动跟踪每个包的字节数
- ✅ libp2p 已经实现，无需重复造轮子
- ✅ 自动按 peer 维度统计，符合我们的需求

#### 2.2.3 MeshStreamHandler（新增）

**职责**：
- 处理 mesh 协议的 stream
- 替代现有的 frame 处理逻辑
- 将 mesh 包注入 gVisor netstack

**接口设计**：
```go
type MeshStreamHandler struct {
    meshManager *MeshManager
    monitor     *LinkQualityMonitor
}

// 处理入站 stream
func (h *MeshStreamHandler) HandleStream(stream network.Stream) {
    defer stream.Close()
    
    peerID := stream.Conn().RemotePeer()
    
    // 读取 mesh 包
    for {
        // 读取长度前缀的消息
        msg, err := readMeshMessage(stream)
        if err != nil {
            h.monitor.RecordError(peerID, err)
            return
        }
        
        // 处理 mesh 包
        h.meshManager.HandleMeshPacket(peerID, msg)
    }
}

// 发送 mesh 包到 peer
func (h *MeshStreamHandler) SendPacket(peerID peer.ID, packet *MeshPacket) error {
    // 创建或复用 stream
    stream, err := h.getOrCreateStream(peerID)
    if err != nil {
        return err
    }
    
    // 写入消息
    return writeMeshMessage(stream, packet)
}
```

### 2.3 数据流

#### 2.3.1 发送 mesh 包

```
Application (SSH/HTTP)
    ↓
gVisor netstack (TCP/UDP)
    ↓
RouteSelector (路由决策，选择 proxyName)
    ↓
MeshManager.HandleMeshPacket()
    ↓
P2PManager.NewStream(proxyName, peerID)
    ↓
libp2p.Host → stream (yamux multiplexing)
    ↓
TrojanTransport / SOCKS5Transport / H_TunnelTransport
    ↓
Proxy connection (trojan/SOCKS5/H_Tunnel over TCP)
    ↓
Network
```

#### 2.3.2 接收 mesh 包

```
Network
    ↓
Proxy connection (trojan/SOCKS5/H_Tunnel over TCP)
    ↓
TrojanTransport / SOCKS5Transport / H_TunnelTransport
    ↓
libp2p.Host → stream (yamux demultiplexing)
    ↓
MeshStreamHandler.HandleStream()
    ↓
MeshManager.HandleMeshPacket()
    ↓
InjectMeshPacket() → gVisor netstack
    ↓
RouteSelector (路由决策)
    ↓
TCP/UDP Forwarder → Application
```

#### 2.3.3 链路质量监控

```
LinkQualityMonitor
    ↓ (定期 ping)
libp2p ping protocol
    ↓ (测量 RTT)
更新 LinkMetrics
    ↓ (定期广播)
Gossip: LinkQualityAdvertisement
    ↓ (共享给邻居)
其他节点的 LinkQualityMonitor
    ↓ (存储到 linkDatabase)
RouteSelector 查询链路质量
    ↓ (计算多跳路径质量)
选择最佳路径
```

#### 2.2.4 Gossip 数据结构演进

**v1 版本（当前实现）**：使用 LocalSeq/RemoteSeq 标识链路

```go
type GossipLink struct {
    LocalSeq     uint16  `json:"localSeq"`               // 本端声明的 seq
    RemoteSeq    uint16  `json:"remoteSeq"`              // 对端声明的 seq
    FriendlyName string  `json:"friendlyName,omitempty"` // 友好名称（代理名，仅展示）
    SRTT         float64 `json:"srtt,omitempty"`
    LossRate     float64 `json:"lossRate,omitempty"`
    Jitter       float64 `json:"jitter,omitempty"`
}
```

**问题**：
1. LocalSeq/RemoteSeq 是主动端概念，被动端无法直接看到
2. 需要双向通告才能匹配链路，增加复杂度
3. proxyName 也是主动端概念，不适合做全局标识

**v2 版本（libp2p 集成后）**：使用 Connection ID 标识链路

```go
type GossipLink struct {
    ConnID   string  `json:"connId"`             // libp2p connection ID（两端可见）
    SRTT     float64 `json:"srtt,omitempty"`
    LossRate float64 `json:"lossRate,omitempty"`
    Jitter   float64 `json:"jitter,omitempty"`
}
```

**优势**：
1. **两端可见**：libp2p connection ID 对连接两端都可见，无需额外通告
2. **全局唯一**：(NodeID, ConnID) 组合在整个网络中唯一标识一条链路
   - NodeID：链路所属节点（从 GossipNeighbor.NodeID 获取）
   - ConnID：该节点上的连接标识（从 GossipLink.ConnID 获取）
3. **简化匹配**：反向验证时比较 (NodeID, ConnID) 是否匹配
4. **解耦 proxyName**：proxyName 仅用于配置查找，不参与链路标识

**示例**：
```
节点 A 和节点 B 之间有一条连接：
- A 端看到：ConnID="abc123"，对端 NodeID="B"
- B 端看到：ConnID="abc123"（同一个），对端 NodeID="A"
- 全局标识：(A, abc123) 或 (B, abc123)，取决于从哪端看

Gossip 通告结构：
GossipClaimedSubnet {
    NodeID: "A"  // subnet 所有者
    Neighbors: [
        GossipNeighbor {
            NodeID: "B"  // 邻居节点
            Links: [
                GossipLink {
                    ConnID: "abc123"  // A-B 之间的连接 ID
                    SRTT: 10.5,
                    ...
                }
            ]
        }
    ]
}
// 全局链路标识：(A, abc123) 表示节点 A 上的连接 abc123
```

**迁移策略**：
- PeerSender 接口新增 `GetConnID() string` 方法
- QualityTracker 新增 `GetByConnID(connID string)` 方法
- 保留 `GetLocalSeq()`/`GetRemoteSeq()` 用于向后兼容（过渡期）
- gossip 广播和接收都使用 ConnID
- 链路匹配逻辑从 `LocalSeq/RemoteSeq 对匹配` 简化为 `ConnID 相等匹配`

#### 2.2.5 Gossip 协议简化

**v1 版本（当前实现）**：hello + gossip 双协议

```go
type GossipInfo struct {
    // Protocol fields (only used by hello)
    Cmd             string `json:"cmd,omitempty"`             // "hello" or "gossip"
    ProtocolVersion int    `json:"protocolVersion,omitempty"` // only in hello
    Seq             uint16 `json:"seq,omitempty"`             // 本端生成的序列号（hello 协商用）
    FriendlyName    string `json:"friendlyName,omitempty"`    // 友好名称（代理名，hello 协商用）

    // Topology information (both hello and gossip carry this)
    DomainSuffixes []GossipDomainSuffix  `json:"domainSuffixes,omitempty"`
    Routes         []GossipRoute         `json:"routes,omitempty"`
    ClaimedSubnets []GossipClaimedSubnet `json:"claimedSubnets,omitempty"`
}
```

**问题**：
1. hello 握手用于协商 seq、交换 FriendlyName，增加复杂度
2. libp2p 已经处理连接建立和协议协商，不需要自定义握手
3. Cmd 字段区分 hello/gossip 增加状态机复杂度

**v2 版本（libp2p 集成后）**：纯 gossip 协议（保留展示字段）

```go
type GossipInfo struct {
    // Protocol fields (for display/versioning, not for connection identification)
    ProtocolVersion int    `json:"protocolVersion,omitempty"` // 协议版本
    Seq             uint16 `json:"seq,omitempty"`             // 序列号（控制台展示用，非链路标识）
    FriendlyName    string `json:"friendlyName,omitempty"`    // 友好名称（主动端传递，控制台展示用）

    // Topology information
    DomainSuffixes []GossipDomainSuffix  `json:"domainSuffixes,omitempty"`
    Routes         []GossipRoute         `json:"routes,omitempty"`
    ClaimedSubnets []GossipClaimedSubnet `json:"claimedSubnets,omitempty"`
}
```

**简化内容**：
- 删除 `Cmd` 字段：不再区分 hello/gossip，统一为 gossip
- 保留 `ProtocolVersion`：用于协议版本控制
- 保留 `Seq`：用于控制台友好展示，**但不作为链路标识**（链路标识用 ConnID）
- 保留 `FriendlyName`：主动端传递，用于控制台友好展示

**关键区别**：
- **链路标识**：使用 ConnID（两端可见，全局唯一）
- **展示字段**：Seq/FriendlyName 仅用于控制台显示，不参与链路匹配和路由决策

**优势**：
1. **简化状态机**：不需要 hello/gossip 状态转换
2. **减少开销**：每个 gossip 包少传 4 个字段
3. **对齐 libp2p**：连接建立由 libp2p 处理，应用层只做拓扑通告

#### 2.2.6 删除 ResendHelloToAll 方法（v2.2 决策）

**背景**：
在 v2.1 之前的实现中，`P2PTransport` 接口包含 `ResendHelloToAll()` 方法，用于在路由表变化时重新发送 hello 消息给所有 peer。

**原始实现**：
```go
// mesh/mesh.go - 旧版本
if m.checkSubnetConflict() {
    m.recomputeRoutes()
    if m.p2p != nil {
        m.p2p.ResendHelloToAll()  // 重新发送 hello
    }
    m.broadcastGossip()  // 广播 gossip
}
```

**问题分析**：
1. **冗余**：hello 握手的主要目的是协商 localSeq 和交换 FriendlyName
2. **libp2p 已处理连接管理**：连接断开/重连由 libp2p 自动处理，不需要手动重发 hello
3. **gossip 已包含拓扑信息**：路由表变化后，`broadcastGossip()` 已经会广播最新的拓扑信息
4. **增加复杂度**：维护 hello 重发逻辑增加状态机复杂度

**v2.2 决策**：
- ❌ 删除 `ResendHelloToAll()` 方法
- ✅ 路由表变化后只调用 `broadcastGossip()`
- ✅ 依赖 libp2p 的连接管理和 gossip 的拓扑通告

**更新后的实现**：
```go
// mesh/mesh.go - v2.2
if m.checkSubnetConflict() {
    m.recomputeRoutes()
    m.broadcastGossip()  // 只广播 gossip，不重发 hello
}
```

**影响范围**：
- `mesh/mesh.go`：删除调用点
- `mesh.P2PTransport` 接口：删除方法定义
- `p2p/p2p_manager.go`：删除实现（如果有的话）

**理由总结**：
- libp2p 的连接管理已经足够，不需要应用层手动重发 hello
- gossip 协议已经承担拓扑信息广播的职责
- 简化代码，减少不必要的复杂度

#### 2.2.7 P2PTransport 接口实现（v2.2：完整实现）

**背景**：
`mesh.P2PTransport` 接口是 mesh 层与 P2P 层的桥梁。在 v2.2 中，我们完成了所有接口方法的完整实现。

**接口定义**（`mesh/mesh.go`）：
```go
type P2PTransport interface {
    // Gossip 广播
    BroadcastMeshGossip(data []byte) error
    SendMeshGossipTo(peerNodeID string, data []byte) error
    SendMeshGossipToAll(data []byte)
    ListMeshPeerIDs() []string
    
    // Peer 管理
    SetMeshInfo(nodeID, vip string)
    StopPeerByProxyName(proxyName string)
    StopPeerByLinkID(linkID string)
    
    // 链路质量统计（三种查询方式）
    GetLinkQualityStats(nodeID string) (srtt, rto, jitter time.Duration, lossRate float64, sampleCount int)
    GetLinkQualityStatsByProxy(proxyName string) (srtt, rto, jitter time.Duration, lossRate float64, sampleCount int)
    GetLinkQualityStatsByLinkID(linkID string) (srtt, rto, jitter time.Duration, lossRate float64, sampleCount int)
    
    // Channel 丢包统计（libp2p 架构下不适用，返回空）
    GetChannelDropStats() (peerStats map[string]struct {
        DataDrops      uint64
        ControlChDrops uint64
    }, meshInboundDrops uint64)
}
```

**设计说明**：
- **链路全局标识**：(nodeID, connID) 组合唯一标识一条链路
- **三种查询方式**：适应不同场景（按节点、按代理、按连接）
- **GetChannelDropStats**：libp2p + yamux + TCP 架构下，丢包由底层处理，此方法返回空统计

**核心实现**（`p2p/p2p_manager.go`）：

```go
// peerInfo 跟踪 peer 的连接信息
type peerInfo struct {
    proxyName string    // 代理名称
    nodeID    string    // mesh 节点 ID
    connID    string    // libp2p connection ID（链路全局标识的一部分）
    peerID    peer.ID   // libp2p peer ID
}

type P2PManager struct {
    host       host.Host  // 单一 host
    proxies    map[string]*config.Proxy
    peerInfo   map[peer.ID]*peerInfo  // peer 信息跟踪
    bwReporter *metrics.BandwidthCounter
    monitor    *LinkQualityMonitor
    // ...
}

// ConnectViaProxy 通过指定代理连接到 peer
func (m *P2PManager) ConnectViaProxy(ctx context.Context, proxyName string, remotePeerID peer.ID) error {
    // 构造代理地址
    proxyAddr, _ := NewProxyMultiaddr(proxyName)
    
    // 构造 AddrInfo
    addrInfo := peer.AddrInfo{
        ID:    remotePeerID,
        Addrs: []multiaddr.Multiaddr{proxyAddr},
    }
    
    // libp2p 会根据地址自动选择合适的 transport
    return m.host.Connect(ctx, addrInfo)
}

// StopPeerByProxyName 停止指定代理的连接
func (m *P2PManager) StopPeerByProxyName(proxyName string) {
    // 查找该代理对应的连接并关闭
    for peerID, info := range m.peerInfo {
        if info.proxyName == proxyName {
            conns := m.host.Network().ConnsToPeer(peerID)
            for _, conn := range conns {
                conn.Close()
            }
            delete(m.peerInfo, peerID)
        }
    }
}

// StopPeerByLinkID 按连接 ID 停止 peer
func (m *P2PManager) StopPeerByLinkID(linkID string) {
    for peerID, info := range m.peerInfo {
        if info.connID == linkID {
            conns := m.host.Network().ConnsToPeer(peerID)
            for _, conn := range conns {
                conn.Close()
            }
            delete(m.peerInfo, peerID)
            return
        }
    }
}

// BroadcastMeshGossip 广播 gossip 到所有 peer
func (m *P2PManager) BroadcastMeshGossip(data []byte) error {
    for _, info := range m.peerInfo {
        if err := m.meshHandler.SendPacket(m.ctx, info.proxyName, info.peerID, data); err != nil {
            util.LogModuleError("P2P", "Failed to send gossip to peer %s: %v", info.peerID, err)
        }
    }
    return nil
}

// SendMeshGossipTo 发送 gossip 到指定节点
func (m *P2PManager) SendMeshGossipTo(peerNodeID string, data []byte) error {
    for _, info := range m.peerInfo {
        if info.nodeID == peerNodeID {
            return m.meshHandler.SendPacket(m.ctx, info.proxyName, info.peerID, data)
        }
    }
    return fmt.Errorf("peer not found: %s", peerNodeID)
}
```

**实现要点**：
1. **peerInfo 跟踪**：维护 peerID → (proxyName, nodeID, connID) 的映射
2. **链路全局标识**：(nodeID, connID) 组合唯一标识一条链路
3. **BandwidthCounter 集成**：通过 `bwReporter.GetBandwidthForPeer()` 获取吞吐量
4. **自动 ping 探测**：LinkQualityMonitor 启动后自动开始 RTT/Jitter 测量
5. **三种查询方式**：按 nodeID、proxyName、connID 查询链路质量
6. **错误处理**：所有方法都有完整的错误处理和日志记录
7. **GetChannelDropStats**：libp2p 架构下不适用，返回空统计

**与 mesh 层的集成**：
- mesh 层通过 `p2p.GlobalP2PManager` 访问 P2P 层
- 所有接口方法都通过 `mesh.P2PTransport` 接口调用
- mesh 层不直接依赖 libp2p，保持解耦

#### 2.2.8 连接包装机制（v2.6 新增）

**问题**：TrojanServer/Socks5Server/HTunnelServer 检测到 `dstPort == reverse.BindPortP2P` (port=2) 后，需要将 `net.Conn` 注入到 Transport。但 libp2p 需要的是 `transport.CapableConn` 接口，不是原始的 `net.Conn`。

**关键问题**：
1. **协议状态**：当 TrojanServer.HandleConn() 检测到 port=2 时，trojan 协议握手是否已完成？
2. **包装层次**：需要在哪一层进行包装？

**答案**：
- ✅ **协议握手已完成**：TrojanServer.HandleConn() 在解析 trojan 协议头后才检测到 port=2
- ✅ **包装应用层数据流**：Transport 收到的是已解密的 trojan 应用层数据流
- ✅ **直接包装为 CapableConn**：不需要再处理 trojan 协议

**包装实现**：

```go
// TrojanTransport.InjectConnection 实现
func (t *TrojanTransport) InjectConnection(conn net.Conn) {
    // conn 已经是解密后的 trojan 应用层数据流
    // 需要包装成 libp2p CapableConn
    
    // 1. 创建 capableConn 包装
    capableConn := &trojanCapableConn{
        Conn: conn,
        // 实现 transport.CapableConn 接口需要的方法
        // 大部分方法可以委托给底层 conn
    }
    
    // 2. 送入 connChan
    select {
    case t.connChan <- capableConn:
        util.LogModuleInfo("P2P", "Injected P2P connection to TrojanTransport")
    default:
        util.LogModuleError("P2P", "P2P connection channel full, dropping connection")
        conn.Close()
    }
}

// trojanCapableConn 包装 net.Conn 为 transport.CapableConn
type trojanCapableConn struct {
    net.Conn
    // 需要实现的额外字段
    localAddr  multiaddr.Multiaddr
    remoteAddr multiaddr.Multiaddr
}

// 实现 transport.CapableConn 接口
func (c *trojanCapableConn) LocalMultiaddr() multiaddr.Multiaddr {
    return c.localAddr
}

func (c *trojanCapableConn) RemoteMultiaddr() multiaddr.Multiaddr {
    return c.remoteAddr
}

// 其他接口方法（Stat, Scope, Close 等）委托给底层 conn
// ...
```

**修改 TrojanServer.HandleConn()**：

```go
// server/trojan.go
func (s *TrojanServer) HandleConn(clientConn net.Conn) {
    defer clientConn.Close()
    
    // 1. 解析 trojan 协议头（加密、认证）
    // ... 现有逻辑 ...
    
    // 2. 获取目标端口
    dstPort := ...  // 从 trojan 协议头解析
    
    // 3. 检测 P2P 连接
    if dstPort == reverse.BindPortP2P {
        // 连接已解密，是应用层数据流
        if p2pTransport := s.GetP2PTransport(); p2pTransport != nil {
            // 注入到 P2P transport（不关闭 conn）
            p2pTransport.InjectConnection(clientConn)
            return  // 连接已交给 P2P，这里不关闭
        }
        // fallback: 旧的 P2P 处理逻辑
        s.handleP2PConnection(clientConn)
        return
    }
    
    // 4. 处理普通 trojan 连接
    // ... 现有逻辑 ...
}
```

**注意事项**：
1. **不要重复加密/解密**：conn 已经是解密后的数据流，Transport 不要再处理 trojan 协议
2. **不要关闭 conn**：InjectConnection 后，TrojanServer 不要 close(conn)，由 Transport 管理生命周期
3. **错误处理**：如果 connChan 满了，需要 close(conn) 防止资源泄漏

#### 2.2.9 协议版本控制（v2.6 新增）

**问题**：如何管理 mesh 协议的版本兼容性？

**方案**：

1. **协议版本号**：
   ```go
   const MeshProtocolVersion = 2  // 当前版本
   
   // Gossip 消息携带版本号
   type GossipInfo struct {
       ProtocolVersion int    `json:"protocolVersion"`
       // ... 其他字段
   }
   ```

2. **版本协商**：
   - 连接建立时，双方交换支持的协议版本
   - 使用双方都支持的最高版本
   - 如果版本不兼容，关闭连接并记录日志

3. **版本兼容策略**：
   - **v1 → v2**：不兼容，强制全量升级（当前决策）
   - **v2 → v3**：向后兼容（未来需要实现）

4. **版本检查**：
   ```go
   func (h *MeshStreamHandler) handleStream(stream network.Stream) {
       // 读取对端版本号
       remoteVersion := readProtocolVersion(stream)
       
       // 检查兼容性
       if !isCompatible(remoteVersion, MeshProtocolVersion) {
           util.LogModuleError("P2P", "Incompatible protocol version: remote=%d, local=%d",
               remoteVersion, MeshProtocolVersion)
           stream.Reset()
           return
       }
       
       // 继续处理...
   }
   ```

#### 2.2.10 可观测性设计（v2.6 新增）

**目标**：确保 libp2p 架构下的问题可诊断、可追踪。

**日志分类**：

1. **连接生命周期**：
   ```go
   util.LogModuleInfo("P2P", "Connection established: peer=%s, node=%s, connID=%s, proxy=%s",
       peerID, nodeID, connID, proxyName)
   util.LogModuleInfo("P2P", "Connection closed: peer=%s, connID=%s, reason=%s",
       peerID, connID, reason)
   ```

2. **Transport 层事件**：
   ```go
   util.LogModuleInfo("P2P", "TrojanTransport.Dial: proxy=%s, peer=%s", proxyName, peerID)
   util.LogModuleInfo("P2P", "TrojanTransport.InjectConnection: conn=%s", conn.RemoteAddr())
   util.LogModuleError("P2P", "TrojanTransport.Dial failed: proxy=%s, err=%v", proxyName, err)
   ```

3. **Stream 事件**：
   ```go
   util.LogModuleInfo("P2P", "Stream created: peer=%s, streamID=%d", peerID, stream.ID())
   util.LogModuleInfo("P2P", "Stream closed: peer=%s, streamID=%d, reason=%s",
       peerID, stream.ID(), reason)
   ```

4. **链路质量监控**：
   ```go
   util.LogModuleInfo("P2P", "Link quality: node=%s, connID=%s, RTT=%v, jitter=%v, loss=%.2f%%",
       nodeID, connID, rtt, jitter, lossRate*100)
   ```

**指标收集**：

```go
type P2PMetrics struct {
    // 连接指标
    ConnectionsActive  prometheus.Gauge
    ConnectionsTotal   prometheus.Counter
    ConnectionErrors   prometheus.Counter
    
    // Transport 指标
    DialAttempts       prometheus.Counter
    DialSuccesses      prometheus.Counter
    DialFailures       prometheus.Counter
    InjectedConns      prometheus.Counter
    
    // Stream 指标
    StreamsActive      prometheus.Gauge
    StreamsTotal       prometheus.Counter
    
    // 链路质量指标
    LinkRTT            *prometheus.GaugeVec  // by nodeID
    LinkJitter         *prometheus.GaugeVec
    LinkLossRate       *prometheus.GaugeVec
    
    // yamux 指标
    YamuxWindowUsage   *prometheus.GaugeVec
    YamuxFlowDelay     *prometheus.GaugeVec
}
```

**Admin API 扩展**：

```go
// GET /api/p2p/metrics
func (s *Server) handleGetP2PMetrics() {
    return json.Marshal(map[string]interface{}{
        "connections": s.p2pManager.GetConnectionStats(),
        "transports": map[string]interface{}{
            "trojan": s.p2pManager.trojanTransport.GetStats(),
            "socks5": s.p2pManager.socks5Transport.GetStats(),
            "htunnel": s.p2pManager.htunnelTransport.GetStats(),
        },
        "linkQuality": s.p2pManager.monitor.GetLinkQualityStats(),
    })
}
```

#### 2.2.11 错误处理和恢复策略（v2.6 新增）

**错误分类**：

1. **可恢复错误**：
   - 连接超时 → 自动重连
   - Stream 异常关闭 → 重建 stream
   - Transport Dial 失败 → 重试或切换到其他代理

2. **不可恢复错误**：
   - libp2p Host 启动失败 → 记录日志，退出程序
   - 配置错误（如 proxy 不存在）→ 记录日志，跳过该 proxy
   - 协议版本不兼容 → 关闭连接，记录日志

**恢复策略**：

```go
// 连接断开后自动重连
func (m *P2PManager) handleConnectionClosed(conn network.Conn) {
    peerID := conn.RemotePeer()
    nodeID, _ := m.GetNodeID(peerID)
    
    util.LogModuleInfo("P2P", "Connection closed: peer=%s, node=%s", peerID, nodeID)
    
    // 如果是重要节点，尝试重连
    if m.shouldReconnect(nodeID) {
        go func() {
            time.Sleep(5 * time.Second)  // 等待一段时间
            m.reconnectToNode(nodeID)
        }()
    }
}

// Stream 异常后重建
func (h *MeshStreamHandler) handleStreamError(stream network.Stream, err error) {
    peerID := stream.Conn().RemotePeer()
    util.LogModuleError("P2P", "Stream error: peer=%s, err=%v", peerID, err)
    
    // 重置 stream
    stream.Reset()
    
    // 下次发送时会创建新 stream
    h.removeStream(peerID)
}
```

**背压处理**：

```go
// 连接注入通道满时的处理
func (t *TrojanTransport) InjectConnection(conn net.Conn) {
    select {
    case t.connChan <- conn:
        // 成功注入
    case <-time.After(5 * time.Second):
        // 超时，记录指标
        t.metrics.InjectionTimeouts.Inc()
        util.LogModuleError("P2P", "Connection injection timeout, dropping")
        conn.Close()
    }
}
```

#### 2.2.12 Stream 管理策略（v2.6 新增，v2.6.2/v2.7/v2.8 修正）

**问题**：libp2p 的 stream 应该按什么维度创建？

**设计决策（v2.8 最终版）**：**每个 flow 一个 stream（双向规范化，全双工）**

**Flow 定义（v2.8 修正：双向规范化）**：

Flow 是网络流量的最小独立单元，**双向规范化**，让 A→B 和 B→A 映射到同一个 FlowKey：

| 协议类型 | Flow 定义 | 规范化方式 | 说明 |
|---------|----------|-----------|------|
| TCP | 双向 5-tuple | {(IP1, Port1), (IP2, Port2), proto=6} | 较小的 IP:Port 作为 endpoint1 |
| UDP | 双向 5-tuple | {(IP1, Port1), (IP2, Port2), proto=17} | 较小的 IP:Port 作为 endpoint1 |
| ICMP | 双向 4-tuple | {IP1, IP2, identifier, proto=1} | 较小的 IP 作为 endpoint1 |
| 其他 | 双向 3-tuple | {IP1, IP2, proto} | 较小的 IP 作为 endpoint1 |

**为什么需要双向规范化？**

```
TCP 连接示例：
  A:54321 → B:22 (SYN)
  B:22 → A:54321 (SYN-ACK)
  A:54321 → B:22 (DATA)
  
单向五元组（错误）：
  Flow 1: (A, 54321, B, 22, TCP)  ← A→B 方向
  Flow 2: (B, 22, A, 54321, TCP)  ← B→A 方向
  ❌ 两个 flow？不对！这是同一个 TCP 连接

双向规范化（正确）：
  Flow: {(A, 54321), (B, 22), TCP}  ← 双向，集合表示
  ✅ 一个 flow，对应一个 stream
```

**FlowKey 数据结构（v2.8：双向规范化）**：

```go
type FlowKey struct {
    // 规范化的端点（较小的 IP:Port 作为 endpoint1）
    endpoint1IP   netip.Addr
    endpoint1Port uint16
    endpoint2IP   netip.Addr
    endpoint2Port uint16
    protocol      uint8
    identifier    uint16  // ICMP identifier
}

func extractFlowKey(packet *IPPacket) FlowKey {
    key := FlowKey{protocol: packet.Protocol}
    
    // 提取端口/identifier
    var port1, port2 uint16
    switch packet.Protocol {
    case 6:  // TCP
        tcp := packet.Payload.(*TCPPacket)
        port1 = tcp.SrcPort
        port2 = tcp.DstPort
    case 17:  // UDP
        udp := packet.Payload.(*UDPPacket)
        port1 = udp.SrcPort
        port2 = udp.DstPort
    case 1:  // ICMP
        icmp := packet.Payload.(*ICMPPacket)
        key.identifier = icmp.Identifier
    }
    
    // 双向规范化：让较小的 IP:Port 作为 endpoint1
    // 这样 A→B 和 B→A 会得到相同的 FlowKey
    ep1IP, ep1Port, ep2IP, ep2Port := normalizeEndpoints(
        packet.SrcIP, packet.DstIP, port1, port2)
    
    key.endpoint1IP = ep1IP
    key.endpoint1Port = ep1Port
    key.endpoint2IP = ep2IP
    key.endpoint2Port = ep2Port
    
    return key
}

func normalizeEndpoints(ip1, ip2 netip.Addr, port1, port2 uint16) (netip.Addr, uint16, netip.Addr, uint16) {
    // 比较 IP
    cmp := bytes.Compare(ip1.AsSlice(), ip2.AsSlice())
    if cmp < 0 {
        return ip1, port1, ip2, port2
    } else if cmp > 0 {
        return ip2, port2, ip1, port1
    }
    // IP 相同，比较端口
    if port1 <= port2 {
        return ip1, port1, ip2, port2
    }
    return ip2, port2, ip1, port1
}
```

**Stream 是全双工的（v2.8 澄清）**：

```
Stream (full-duplex, 全双工)
  ├─ A→B 方向：独立的发送窗口、拥塞控制
  └─ B→A 方向：独立的发送窗口、拥塞控制

yamux 为每个 stream 维护：
  ├─ Send Window (本端→对端)：独立流控
  └─ Receive Window (对端→本端)：独立流控

两个方向的带宽完全独立，互不影响！
```

**示例**：

```
SSH 连接（TCP，全双工）：
  A→B: 用户输入命令 (100 KB/s)
  B→A: 终端输出 (500 KB/s)
  ✅ 两个方向同时传输，互不影响

文件下载（TCP，全双工）：
  A→B: ACK + 请求 (10 KB/s)
  B→A: 文件数据 (10 MB/s)
  ✅ 两个方向同时传输，带宽独立

DNS 查询（UDP，双向规范化）：
  A:12345 → B:53 (查询)
  B:53 → A:12345 (响应)
  ✅ 规范化为同一个 flow: {(A, 12345), (B, 53), UDP}

ICMP ping（双向规范化）：
  A → B (Echo Request, id=1234)
  B → A (Echo Reply, id=1234)
  ✅ 规范化为同一个 flow: {A, B, ICMP, id=1234}
```

**理由**：
- ✅ **双向规范化**：A→B 和 B→A 映射到同一个 flow，对应同一个 stream
- ✅ **全双工**：stream 两个方向带宽独立，互不影响
- ✅ **公平性**：每个 flow（应用连接）独立流控，互不影响
- ✅ **libp2p 的核心价值**：yamux 多路复用 + 独立流控 + 全双工
- ✅ **协议兼容**：正确处理所有 IP 协议类型（TCP/UDP/ICMP/其他）

**错误设计（v2.6 初版）**：
- ❌ 一个 connection 一个 stream → 所有流量共享，无公平性
- ❌ libp2p 的价值完全没有体现

**错误设计（v2.6.2 修正版）**：
- ⚠️ 说"每个应用连接一个 stream"但未定义 flow 的具体含义
- ⚠️ 假设所有 IP 包都有 5-tuple（实际只有 TCP/UDP 有端口）

**错误设计（v2.7 修正版）**：
- ⚠️ Flow 定义为单向五元组（A→B 和 B→A 会映射到不同 flow）
- ⚠️ 未说明 stream 是全双工的

**正确设计（v2.8 最终版）**：
- ✅ 一个 connection 多个 stream（每个 flow 一个）
- ✅ Flow 双向规范化（A→B 和 B→A 映射到同一个 flow）
- ✅ Stream 全双工（两个方向带宽独立）
- ✅ yamux 自动调度，公平共享带宽

**架构层次**：

```
应用层（SSH、HTTP、DNS 等）
   ↓ 每个 flow（双向规范化，TCP/UDP 连接、ICMP 会话等）
Stream（全双工，独立流控，yamux 调度）
   ├─ A→B 方向：独立带宽
   └─ B→A 方向：独立带宽
   ↓ 多个 stream 多路复用
Connection（通过代理到目标节点）
   ↓
代理（trojan/socks5/h_tunnel）
```

**示例**：

```
Connection to VM (via trojan-to-vm, 带宽 10 Mbps)
  ├─ Stream 1: SSH session (全双工)
  │   ├─ A→B: 用户输入 (100 KB/s)
  │   └─ B→A: 终端输出 (500 KB/s)
  │   Flow: {(10.0.0.1, 54321), (10.0.0.2, 22), TCP}
  │
  ├─ Stream 2: HTTP request (全双工)
  │   ├─ A→B: HTTP 请求 (50 KB/s)
  │   └─ B→A: HTTP 响应 (5 Mbps)
  │   Flow: {(10.0.0.1, 54322), (10.0.0.2, 80), TCP}
  │
  ├─ Stream 3: DNS query (双向规范化)
  │   ├─ A→B: DNS 查询 (10.0.0.1:12345 → 10.0.0.2:53)
  │   └─ B→A: DNS 响应 (10.0.0.2:53 → 10.0.0.1:12345)
  │   Flow: {(10.0.0.1, 12345), (10.0.0.2, 53), UDP}
  │
  ├─ Stream 4: ICMP ping (双向规范化)
  │   ├─ A→B: Echo Request (id=1234)
  │   └─ B→A: Echo Reply (id=1234)
  │   Flow: {10.0.0.1, 10.0.0.2, ICMP, id=1234}
  │
  └─ Stream 5: File download (全双工)
      ├─ A→B: ACK + 请求 (10 KB/s)
      └─ B→A: 文件数据 (9 Mbps)
      Flow: {(10.0.0.1, 54324), (10.0.0.2, 80), TCP}
  
yamux 自动调度，公平共享 10 Mbps 带宽！
每个 stream 全双工，两个方向带宽独立！
```

**Flow 表管理**：

```go
type FlowTable struct {
    mu     sync.RWMutex
    flows  map[FlowKey]*FlowEntry
    stream network.Stream
}

type FlowEntry struct {
    key       FlowKey
    stream    network.Stream
    createdAt time.Time
    lastSeen  time.Time
}

func (ft *FlowTable) GetOrCreateFlow(key FlowKey) (*FlowEntry, bool, error) {
    ft.mu.RLock()
    entry, exists := ft.flows[key]
    ft.mu.RUnlock()
    
    if exists {
        entry.lastSeen = time.Now()
        return entry, false, nil  // 已存在，复用 stream
    }
    
    // 不存在，创建新 stream
    ft.mu.Lock()
    defer ft.mu.Unlock()
    
    // 双重检查
    if entry, exists := ft.flows[key]; exists {
        entry.lastSeen = time.Now()
        return entry, false, nil
    }
    
    // 在这个 connection 上创建新的 stream
    stream, err := ft.stream.Conn().NewStream()
    if err != nil {
        return nil, false, fmt.Errorf("create stream: %w", err)
    }
    
    entry = &FlowEntry{
        key:       key,
        stream:    stream,
        createdAt: time.Now(),
        lastSeen:  time.Now(),
    }
    ft.flows[key] = entry
    
    util.LogModuleInfo("P2P", "Flow created: flow=%v, streamID=%d", 
        key, stream.ID())
    
    return entry, true, nil  // 新建
}

func (ft *FlowTable) CleanupIdleFlows(timeout time.Duration) {
    ft.mu.Lock()
    defer ft.mu.Unlock()
    
    now := time.Now()
    for key, entry := range ft.flows {
        if now.Sub(entry.lastSeen) > timeout {
            entry.stream.Close()
            delete(ft.flows, key)
            util.LogModuleInfo("P2P", "Flow timeout: flow=%v", key)
        }
    }
}
```

**Stream 生命周期**：

```go
// 当数据包到达时
func (m *P2PManager) handlePacket(dstNodeID string, packet *IPPacket) error {
    // 1. 提取 flow key
    flowKey := extractFlowKey(packet)
    
    // 2. 获取到目标节点的 connection
    link := m.getLinkToNode(dstNodeID)
    if link == nil {
        return fmt.Errorf("no connection to node: %s", dstNodeID)
    }
    
    // 3. 获取或创建 flow 的 stream
    flowEntry, isNew, err := link.flowTable.GetOrCreateFlow(flowKey)
    if err != nil {
        return fmt.Errorf("get/create flow: %w", err)
    }
    
    if isNew {
        // 新 flow，启动转发 goroutine
        go m.forwardFlow(flowEntry, link)
    }
    
    // 4. 在 flow 的 stream 上发送数据包
    _, err = flowEntry.stream.Write(packet.Bytes())
    if err != nil {
        // Stream 错误，清理 flow
        link.flowTable.RemoveFlow(flowKey)
        return fmt.Errorf("write to stream: %w", err)
    }
    
    return nil
}

// 在 stream 上转发数据（从远端 stream 读取，注入本地 netstack）
func (m *P2PManager) forwardFlow(flowEntry *FlowEntry, link *Link) {
    defer func() {
        flowEntry.stream.Close()
        link.flowTable.RemoveFlow(flowEntry.key)
    }()
    
    buf := make([]byte, 65536)
    for {
        n, err := flowEntry.stream.Read(buf)
        if err != nil {
            if err != io.EOF {
                util.LogModuleError("P2P", "Stream read error: flow=%v, err=%v", 
                    flowEntry.key, err)
            }
            return
        }
        
        // 解析 IP 包并注入 netstack
        packet, err := ParseIPPacket(buf[:n])
        if err != nil {
            util.LogModuleError("P2P", "Parse packet error: %v", err)
            continue
        }
        
        m.meshManager.InjectMeshPacket(packet)
    }
}
```

**yamux 的流控机制**：

```
yamux 为每个 stream 维护：
  - 独立的发送窗口（flow control window）
  - 独立的接收缓冲区
  - 独立的拥塞控制

调度策略：
  - 轮询调度（round-robin）或加权公平队列（WFQ）
  - 自动适应网络条件
  - 保证公平性
```

**公平性保证**：

```
场景：Connection 带宽 = 10 Mbps

Stream 1 (SSH): 需要 3 Mbps
Stream 2 (HTTP): 需要 6 Mbps
Stream 3 (DNS): 需要 1 Mbps

yamux 调度结果：
  Stream 1: 3 Mbps ✓
  Stream 2: 6 Mbps ✓
  Stream 3: 1 Mbps ✓

如果 Stream 2 突然需要 10 Mbps：
  Stream 1: 2 Mbps（减少）
  Stream 2: 7 Mbps（增加，但不独占）
  Stream 3: 1 Mbps（保证最低）

yamux 自动调度，公平共享！
```

**优势**：
- ✅ **公平性**：每个应用连接独立流控，互不影响
- ✅ **隔离性**：一个慢 stream 不会阻塞其他 stream
- ✅ **高效**：多个 stream 共享一个 connection，减少连接数
- ✅ **自动调度**：yamux 自动公平调度，无需手动管理

**与当前设计的对比**：

| 特性 | 当前设计（无 libp2p） | libp2p + yamux |
|------|---------------------|----------------|
| **连接数** | 每个 proxy 一个 connection | 每个 proxy 一个 connection |
| **流控** | 所有流量共享，DRR 调度 | 每个 stream 独立流控 |
| **公平性** | 有限（DRR 队列） | 完全（yamux 自动调度） |
| **隔离性** | 差（一个慢连接影响所有） | 好（stream 独立） |
| **复杂度** | 高（手动 DRR、ACK、心跳） | 低（libp2p 自动处理） |

**总结**：
- ✅ **每个 flow 一个 stream**：这是 libp2p 的核心价值
- ✅ **Flow 双向规范化**：A→B 和 B→A 映射到同一个 flow，对应同一个 stream
- ✅ **Stream 全双工**：两个方向带宽独立，互不影响
- ✅ **yamux 自动调度**：公平共享带宽，无需手动管理
- ✅ **独立流控**：每个 stream 互不影响，隔离性好
- ✅ **减少连接数**：多个 stream 共享一个 connection

## 3. 详细设计

### 3.1 删除的组件

以下组件将被 libp2p 替代，需要删除：

| 组件 | 文件 | 行数 | 说明 |
|------|------|------|------|
| `P2PManager` | `p2p/p2p.go` | ~500 | 由新的 `P2PManager`（单一 host + 每种协议一个 transport）替代 |
| `dataQueues[16]` | `p2p/p2p.go` | ~100 | 由 yamux stream 替代 |
| `DRRScheduler` | `p2p/drr.go` | ~200 | 由 yamux 自动调度替代 |
| `writeLoop` | `p2p/p2p.go` | ~150 | 由 libp2p stream 替代 |
| `readLoop` | `p2p/p2p.go` | ~150 | 由 libp2p stream 替代 |
| 手动 ACK 机制 | `p2p/ack.go` | ~100 | 由 TCP + yamux 替代 |
| batch coalescing | `p2p/p2p.go` | ~50 | 由 libp2p 自动优化 |
| **总计** | | **~1250** | |

### 3.2 新增的组件

| 组件 | 文件 | 行数 | 说明 |
|------|------|------|------|
| `P2PManager` (新) | `p2p/p2p_manager.go` | ~350 | 单一 host 管理器 |
| `LinkQualityMonitor` | `p2p/link_quality.go` | ~400 | 链路质量监控 |
| `MeshStreamHandler` | `p2p/mesh_stream.go` | ~200 | mesh stream 处理 |
| `TrojanTransport` | `p2p/transport_trojan.go` | ~150 | trojan transport |
| `SOCKS5Transport` | `p2p/transport_socks5.go` | ~150 | SOCKS5 transport |
| `H_TunnelTransport` | `p2p/transport_htunnel.go` | ~150 | H_Tunnel transport |
| **总计** | | **~1400** | |

**净变化**：+100 行（但复杂度大幅降低）

### 3.3 修改的组件

| 组件 | 文件 | 修改内容 |
|------|------|---------|
| `RouteSelector` | `mesh/route_selector.go` | 集成 `LinkQualityMonitor`，查询链路质量 |
| `MeshManager` | `mesh/mesh.go` | 使用新的 `P2PManager` 发送/接收 mesh 包 |
| `main_engine.go` | `main_engine.go` | 初始化新的 `P2PManager`（单一 host）替代旧的 |

### 3.4 不变的组件

| 组件 | 文件 | 说明 |
|------|------|------|
| gVisor netstack | `mesh/netstack.go` | 完全不变 |
| RouteSelector 决策逻辑 | `mesh/route_selector.go` | 决策逻辑不变，只是数据来源变了 |
| DNSHijacker | `mesh/dns.go` | 完全不变 |
| InjectMeshPacket | `mesh/netstack.go` | 完全不变 |
| Proxy dialers | `dialer/trojan.go`, `dialer/socks5.go` | 底层代理连接不变 |

### 3.5 Admin API 适配

**影响范围**：
- ✅ 拓扑图展示：不受影响（依赖 mesh 层路由信息）
- ✅ 节点列表：不受影响（从 MeshManager 获取）
- ✅ 路由信息：不受影响（RouteSelector 提供）
- ⚠️ Peer 详情：需要适配（内部数据结构变化）
- ⚠️ 链路质量：需要适配（数据来源变化）
- ⚠️ 连接统计：需要适配（统计方式变化）
- ❌ 前端 UI：不受影响（API 接口保持不变）

**需要修改的文件**：
```
server/admin.go          // Admin API 路由
server/admin_mesh.go     // Mesh 相关 API 实现
```

**需要适配的 API**：
```go
// 修改前
func (s *Server) handleGetPeers() {
    peers := s.p2pManager.GetPeers()  // 从旧 P2PManager 获取
    // ...
}

// 修改后
func (s *Server) handleGetPeers() {
    peers := s.p2pManager.GetPeers()  // 从新 P2PManager（多 host）获取
    // 内部实现：遍历所有 host，聚合 peer 信息
    // ...
}
```

**工作量估计**：~8 小时（1 天）

### 3.6 依赖管理

**新增依赖**：
```go
require (
    github.com/libp2p/go-libp2p v0.36.0
    github.com/libp2p/go-libp2p/p2p/transport/tcp v0.0.0
    github.com/multiformats/go-multiaddr v0.13.0
)
```

**二进制大小影响**：
- 当前：~20MB
- 新增 libp2p 最小化依赖：+10MB
- 预计：~30MB（增长 50%）

**优化措施**：
- 使用 `-ldflags="-s -w"` 压缩
- 只引入必要的 libp2p 组件（core, swarm, yamux, ping）
- 不引入 DHT、mDNS 等不需要的功能

## 4. 迁移策略

### 4.1 分阶段实施

**阶段 1：基础设施搭建（1 周）**
- [ ] 引入 libp2p 依赖
- [ ] 实现 `TrojanTransport`、`SOCKS5Transport`、`H_TunnelTransport`
- [ ] 实现 `Libp2pHost` 基础框架
- [ ] 单元测试：transport 层连接测试
- **验证门控**：TrojanTransport 能 Dial 和 Accept 连接（本地测试）

**阶段 2：P2P 层重构（2 周）**
- [ ] 实现 `MeshStreamHandler`
- [ ] 移除 `P2PManager`、`DRRScheduler`、`writeLoop`、`readLoop`
- [ ] 集成 `Libp2pHost` 到 `main_engine.go`
- [ ] 集成测试：单节点 mesh 包收发
- **验证门控**：VM 环境能发送/接收 mesh 包（ping 测试到 QG）

**阶段 3：链路质量监控（1 周）**
- [ ] 实现 `LinkQualityMonitor`
- [ ] 实现 RTT 探测（libp2p ping）
- [ ] 实现错误率统计
- [ ] 实现链路质量广播（gossip）
- [ ] 集成测试：链路质量测量
- **验证门控**：LinkQualityMonitor 能测量 RTT 和吞吐量，数据正确

**阶段 4：路由决策集成（1 周）**
- [ ] `RouteSelector` 集成 `LinkQualityMonitor`
- [ ] 实现多跳路径质量计算
- [ ] 优化路由决策算法
- [ ] 集成测试：路由决策基于链路质量
- **验证门控**：RouteSelector 能基于链路质量选择路径（traceroute 验证）

**阶段 5：试点部署（1 周）** ⭐ **v2.6 新增**
- [ ] 部署到 VM 环境（开发环境）
- [ ] 验证基本功能（DNS、mesh 路由、admin API）
- [ ] 部署到 QG 环境（生产环境）
- [ ] 验证 VM ↔ QG ↔ GG 互通
- [ ] 监控 24 小时稳定性
- **验证门控**：
  - VM 和 QG 运行 24 小时无故障
  - mesh 路由正常（traceroute 到所有节点）
  - admin API 正常（拓扑图、节点列表）
  - 性能指标正常（RTT、吞吐量）

**阶段 6：全量部署（1 周）**
- [ ] 部署到 GG、JF 环境
- [ ] 部署到 MS9、MS10 环境
- [ ] 全节点 mesh 测试
- [ ] 性能基准测试（对比当前实现）
- [ ] 稳定性测试（72 小时长时间运行）
- **验证门控**：
  - 所有节点运行 72 小时无故障
  - 性能提升达到预期（70-90%）
  - 无内存泄漏、无连接泄漏

**总工期**：8 周（1 人全职）

### 4.2 兼容性策略

**问题**：新旧版本无法直接通信（协议不同）

**方案**：强制全量升级（分阶段试点）

**理由**：
1. 环境数量有限（6 个节点），全量升级可控
2. 新旧协议兼容会增加复杂度
3. 性能提升显著，值得一次性升级
4. **v2.6 新增**：通过分阶段试点降低风险

**升级顺序**：
1. **试点阶段**：先在 VM 环境测试（开发环境）
2. **扩展阶段**：验证通过后，部署到 QG（生产环境）
3. **试点验证**：VM + QG 运行 24 小时无故障
4. **全量部署**：部署到 GG、JF、MS9、MS10

### 4.3 回滚方案（v2.6 优化）

**回滚触发条件**：
1. **严重故障**：mesh 网络完全不可用（所有节点无法通信）
2. **性能退化**：性能比旧版本差 30% 以上
3. **稳定性问题**：24 小时内出现 3 次以上进程崩溃
4. **数据损坏**：路由表或拓扑信息错误

**回滚流程**：
1. **保留旧版本二进制**：每个环境保留上一版本的备份
2. **快速回滚**：5 分钟内完成回滚（使用预存的旧版本）
3. **验证回滚**：确认旧版本正常运行
4. **问题分析**：收集日志、指标，分析根因
5. **修复后重新部署**：修复问题后，重新走部署流程

**回滚命令示例**：
```bash
# QG 环境回滚
ssh -p 2222 root@10.11.61.41 "rc-service phaethon stop"
ssh -p 2222 root@10.11.61.41 "cp /root/phaethon.backup /root/phaethon"
ssh -p 2222 root@10.11.61.41 "rc-service phaethon start"

# VM 环境回滚
ssh -p 39022 Docker@10.21.20.65 "powershell -Command 'Stop-Process -Name phaethon -Force'"
ssh -p 39022 Docker@10.21.20.65 "powershell -Command 'Copy-Item -Force \"C:\Users\Docker\Desktop\Workspace\phaethon\phaethon.old.exe\" \"C:\Users\Docker\Desktop\Workspace\phaethon\phaethon.exe\"'"
ssh -p 39022 Docker@10.21.20.65 "powershell -Command 'Start-Process -FilePath \"C:\Users\Docker\Desktop\Workspace\phaethon\phaethon.exe\" -WorkingDirectory \"C:\Users\Docker\Desktop\Workspace\phaethon\"'"
```

**回滚后处理**：
1. 确认所有节点回滚成功
2. 验证 mesh 网络恢复正常
3. 分析问题根因
4. 修复后重新走部署流程（从阶段 5 开始）

## 5. 测试计划

### 5.1 单元测试

- [ ] Transport 层连接测试
- [ ] libp2p stream 多路复用测试
- [ ] LinkQualityMonitor RTT 测量测试
- [ ] 链路质量分数计算测试

### 5.2 集成测试

- [ ] 单节点 mesh 包收发测试
- [ ] 多节点 mesh 路由测试
- [ ] 链路质量监控测试
- [ ] 路由决策测试（基于链路质量）

### 5.3 性能测试

**测试场景**：
1. 单连接吞吐量测试
2. 多连接并发测试
3. 长延迟链路测试
4. 高丢包率链路测试

**对比基准**：
- 当前实现（P2PManager + DRR）
- 代理模式（GG SOCKS5）
- 新实现（libp2p + yamux）

**预期结果**：
- 多连接并发性能提升 70-90%
- 接近代理模式的性能水平
- 单连接性能持平或略有提升

### 5.4 稳定性测试

- [ ] 长时间运行测试（72 小时）
- [ ] 连接断开重连测试
- [ ] 节点重启测试
- [ ] 网络抖动测试

## 6. 风险与缓解

### 6.1 高风险

| 风险 | 影响 | 缓解措施 |
|------|------|---------|
| **二进制体积增长** | 部署时间增加，内存占用增加 | 选择性引入 libp2p 组件，使用压缩标志 |
| **性能不达预期** | 无法解决性能问题 | 先做 PoC 验证，基准测试对比 |
| **兼容性问题** | 新旧版本无法通信 | 强制全量升级，保留回滚方案 |

### 6.2 中风险

| 风险 | 影响 | 缓解措施 |
|------|------|---------|
| **调试复杂度增加** | 问题定位困难 | 增加详细日志和 metrics |
| **学习曲线** | 开发效率短期下降 | 提前学习 libp2p 文档 |
| **依赖管理** | 构建复杂度增加 | 使用 Go modules，锁定版本 |

### 6.3 低风险

| 风险 | 影响 | 缓解措施 |
|------|------|---------|
| **gVisor 兼容性** | 影响核心功能 | libp2p 不影响 gVisor，架构隔离 |
| **路由决策失效** | 网络性能下降 | 保留旧的路由决策逻辑作为 fallback |

## 7. 成功标准

### 7.1 功能标准

- [ ] 所有 mesh 节点正常连接
- [ ] mesh 包正常收发
- [ ] 链路质量监控正常工作
- [ ] 路由决策基于链路质量

### 7.2 性能标准

- [ ] 多连接并发性能提升 70% 以上
- [ ] SSH 访问 MS10 延迟降低 50% 以上
- [ ] 接近代理模式的性能水平（差距 < 30%）

### 7.3 稳定性标准

- [ ] 长时间运行（72 小时）无崩溃
- [ ] 连接断开后自动重连
- [ ] 节点重启后自动恢复

## 8. 附录

### 8.1 参考资料

- [libp2p 文档](https://docs.libp2p.io/)
- [yamux 协议](https://github.com/hashicorp/yamux/blob/master/spec.md)
- [go-libp2p GitHub](https://github.com/libp2p/go-libp2p)

### 8.2 术语表

| 术语 | 说明 |
|------|------|
| **libp2p** | 模块化 P2P 网络库 |
| **yamux** | 多路复用协议，一个连接上多个 stream |
| **stream** | libp2p 的逻辑连接，共享底层物理连接 |
| **RTT** | Round-Trip Time，往返延迟 |
| **gossip** | 节点间广播信息的机制 |
| **RouteSelector** | 路由决策器，选择最佳路径 |

### 8.3 更新日志

| 日期 | 版本 | 更新内容 | 作者 |
|------|------|---------|------|
| 2026-10-14 | v1.0 | 初始版本 | Phaethon Team |
| 2026-10-14 | v2.0 | 架构重构：One Host Per Proxy，每个代理一个独立的 libp2p.Host 实例 | Phaethon Team |
| 2026-10-14 | v2.1 | Gossip 结构改用 ConnID 标识链路，简化 gossip 协议（删除 hello/gossip 双协议） | Phaethon Team |
| 2026-10-14 | v2.2 | 集成 libp2p BandwidthCounter 测量吞吐量；保留 ping 用于 RTT/Jitter；删除冗余的 ResendHelloToAll 方法；完善 P2PTransport 接口实现；改进链路质量评分公式（加入 Jitter） | Phaethon Team |
| 2026-10-14 | v2.3 | **架构重构：One Host Per Node**，每个节点一个 libp2p.Host，每个代理是一个 connection；支持 connect 和 listen；统一 peer ID | Phaethon Team |
| 2026-10-14 | v2.4 | **Connect/Listen 适配修正**：P2P transport 不创建自己的 listener，而是 hook 到现有代理服务器（server.TrojanServer/Socks5Server/HTunnelServer）的 `bind port=2` 处理流程；server 检测到 dstPort==BindPortP2P 后，调用 transport.InjectConnection() 注入连接 | Phaethon Team |
| 2026-10-14 | v2.5 | **Transport 架构修正**：每种协议一个 Transport 实例（不是每个 proxy 一个），该 transport 同时处理该协议的 Dial 和 Listen；多个同协议的 server 都注入到同一个 transport 的 connChan；transport 内部维护多个同协议代理的 dialer 池 | Phaethon Team |
| 2026-10-14 | v2.6 | **完善实现细节**：补充 peer.ID ↔ NodeID 双向映射机制（标准做法，分离加密身份和应用身份）；补充连接包装机制（net.Conn → CapableConn）；补充协议版本控制；补充可观测性设计（日志、指标、Admin API）；补充错误处理和恢复策略；优化迁移策略（增加试点阶段、验证门控、回滚触发条件） | Phaethon Team |
| 2026-10-14 | v2.6.1 | **修正 peer.ID 理解**：peer.ID 是 libp2p 内部概念（公钥哈希），对本设计完全透明，无需配置或感知；我们只关心 NodeID 和 connID；配置完全复用现有代理架构（proxies、mappings） | Phaethon Team |
| 2026-10-14 | v2.6.2 | **修正 stream 粒度**：从"一个 connection 一个 stream"改为"每个应用连接一个 stream"，体现 libp2p yamux 多路复用的核心价值 | Phaethon Team |
| 2026-10-14 | v2.7 | **澄清核心概念 + 修正 flow 定义**：澄清 peer.ID/proxyName/LinkNIC 关系（peer.ID 透明、proxyName 仅用于 V2 模式路由 hook、LinkNIC 直接持有 connection）；修正 flow 定义按协议类型区分（TCP/UDP: 5-tuple, ICMP: identifier, 其他: 3-tuple），正确处理所有 IP 协议类型 | Phaethon Team |
| 2026-10-14 | v2.8 | **修正 Flow 为双向规范化 + Stream 全双工**：Flow 双向规范化（A→B 和 B→A 映射到同一个 flow），Stream 全双工（两个方向带宽独立），yamux 自动管理流控 | Phaethon Team |
