# libp2p 集成设计方案

**文档版本**: v1.0  
**创建时间**: 2026-10-14  
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
┌─────────────────────────────────────────────────────┐
│  Application Layer (SSH, HTTP, DNS, etc.)           │
├─────────────────────────────────────────────────────┤
│  Mesh Layer                                          │
│  ├─ RouteSelector (路由决策，集成 LinkQualityMonitor)│
│  ├─ DNSHijacker (DNS 解析，不变)                    │
│  ├─ MeshManager (mesh 包处理，不变)                 │
│  └─ LinkQualityMonitor (新增：链路质量监控)         │
├─────────────────────────────────────────────────────┤
│  P2P Layer (libp2p 替换)                            │
│  ├─ libp2p.Host (替代 P2PManager)                   │
│  ├─ Stream Multiplexing (yamux，自动)               │
│  ├─ Connection Management (自动)                    │
│  └─ Protocol Negotiation (自定义 mesh 协议)         │
├─────────────────────────────────────────────────────┤
│  Transport Layer (不变)                              │
│  ├─ TrojanTransport (自定义 libp2p.Transport)       │
│  ├─ SOCKS5Transport (自定义 libp2p.Transport)       │
│  └─ H_TunnelTransport (自定义 libp2p.Transport)     │
├─────────────────────────────────────────────────────┤
│  Proxy Connections (不变)                            │
│  (trojan/SOCKS5/H_Tunnel over TCP)                  │
└─────────────────────────────────────────────────────┘
```

### 2.2 核心组件

#### 2.2.0 多链路架构（重要）

**当前实现的多链路设计**：

```
节点 A 和节点 B 之间：
- 配置了 3 个代理：trojan-1, socks5-2, htunnel-3
- 建立 3 条独立的 P2P 链路：
  ├─ Link 1 (trojan-1):   A → trojan-1 → B
  ├─ Link 2 (socks5-2):   A → socks5-2 → B
  └─ Link 3 (htunnel-3):  A → htunnel-3 → B
  
- 每条链路独立管理
- RouteSelector 根据链路质量选择使用哪条链路
```

**链路标识**：

```
主动端（A）发起连接：
- proxyName: "trojan-1"（主动端才知道用的哪个代理）
- localSeq: 12345（A 自己生成的序列号）
- peerID: B

被动端（B）接收连接：
- 没有 proxyName（不知道 A 用的哪个代理）
- localSeq: 67890（B 自己生成的序列号，和 A 的不同！）
- peerID: A

同一条物理链路，两端的 localSeq 是不同的！
A 的视角：到 B 的链路 = (B, localSeq=12345)
B 的视角：到 A 的链路 = (A, localSeq=67890)
```

**libp2p 集成策略**：

1. **保留 localSeq 机制**：我们自己管理序列号，不依赖 libp2p 内部实现
2. **建立映射关系**：localSeq ↔ libp2p conn ID
3. **每个 proxyName = 一个 libp2p 连接**：不会自动扩缩（也不需要）
4. **LinkQualityMonitor 按链路跟踪**：
   - 主动端：按 (proxyName, localSeq) 跟踪
   - 被动端：按 localSeq 跟踪

```go
type Libp2pHost struct {
    host libp2p.Host
    
    // localSeq → libp2p conn 的映射
    localSeqToConn map[uint32]network.Conn
    
    // libp2p conn → localSeq 的反向映射
    connToLocalSeq map[string]uint32
    
    // 主动端：proxyName → localSeq 列表
    proxyToLinks map[string][]uint32
}
```

#### 2.2.1 libp2p.Host（替代 P2PManager）

**职责**：
- 管理 peer 连接
- 提供 stream 多路复用
- 内置连接管理（心跳、重连、健康检查）

**接口设计**：
```go
type Libp2pHost struct {
    host    libp2p.Host
    monitor *LinkQualityMonitor
    
    // mesh 协议处理器
    meshHandler *MeshStreamHandler
}

// 启动 libp2p host
func NewLibp2pHost(config *P2PConfig) (*Libp2pHost, error)

// 连接到 peer
func (h *Libp2pHost) Connect(peerID peer.ID, addr multiaddr.Multiaddr) error

// 创建 stream 到 peer
func (h *Libp2pHost) NewStream(ctx context.Context, peerID peer.ID, protocol protocol.ID) (network.Stream, error)

// 注册 mesh 协议处理器
func (h *Libp2pHost) SetStreamHandler(protocol protocol.ID, handler network.StreamHandler)

// 关闭
func (h *Libp2pHost) Close() error
```

#### 2.2.2 LinkQualityMonitor（新增）

**职责**：
- 测量到直接邻居的链路质量（RTT、错误率、吞吐量）
- **按链路跟踪**：主动端按 (proxyName, localSeq)，被动端按 localSeq
- 通过 gossip 共享链路质量信息
- 为 RouteSelector 提供链路质量数据

**核心结构**：
```go
type LinkQualityMonitor struct {
    mu     sync.RWMutex
    host   *Libp2pHost
    localID string
    
    // 主动端：按 (proxyName, localSeq) 跟踪
    outboundLinks map[string]map[uint32]*LinkMetrics  // proxyName -> localSeq -> metrics
    
    // 被动端：按 localSeq 跟踪
    inboundLinks map[uint32]*LinkMetrics  // localSeq -> metrics
    
    // 配置
    probeInterval time.Duration  // ping 探测间隔
    probeTimeout  time.Duration  // ping 超时
    
    // 链路质量数据库（自己的测量 + gossip 获取的）
    linkDatabase map[string]map[string]*LinkInfo  // from -> to -> quality
}

type LinkMetrics struct {
    // 直接测量
    RTT         float64       // 平均 RTT (ms)
    RTTMin      float64       // 最小 RTT
    RTTMax      float64       // 最大 RTT
    ErrorRate   float64       // 错误率 (0-1)
    Throughput  int64         // 吞吐量 (bytes/sec)
    
    // 统计
    TotalPings  uint64        // 总 ping 次数
    FailedPings uint64        // 失败 ping 次数
    
    // 综合质量分数 (0-100)
    QualityScore float64
    
    // 最后更新时间
    LastUpdated time.Time
}
```

**核心方法**：
```go
// 主动端：记录特定链路的 RTT
func (lqm *LinkQualityMonitor) RecordOutboundRTT(proxyName string, localSeq uint32, rtt time.Duration) {
    lqm.mu.Lock()
    defer lqm.mu.Unlock()
    
    if lqm.outboundLinks[proxyName] == nil {
        lqm.outboundLinks[proxyName] = make(map[uint32]*LinkMetrics)
    }
    
    metrics := lqm.outboundLinks[proxyName][localSeq]
    if metrics == nil {
        metrics = &LinkMetrics{}
        lqm.outboundLinks[proxyName][localSeq] = metrics
    }
    
    // 更新 RTT 统计
    metrics.updateRTT(rtt)
}

// 被动端：记录特定链路的 RTT
func (lqm *LinkQualityMonitor) RecordInboundRTT(localSeq uint32, rtt time.Duration) {
    lqm.mu.Lock()
    defer lqm.mu.Unlock()
    
    metrics := lqm.inboundLinks[localSeq]
    if metrics == nil {
        metrics = &LinkMetrics{}
        lqm.inboundLinks[localSeq] = metrics
    }
    
    // 更新 RTT 统计
    metrics.updateRTT(rtt)
}

// 获取链路质量（主动端）
func (lqm *LinkQualityMonitor) GetOutboundLinkQuality(proxyName string, localSeq uint32) *LinkMetrics {
    lqm.mu.RLock()
    defer lqm.mu.RUnlock()
    return lqm.outboundLinks[proxyName][localSeq]
}

// 获取链路质量（被动端）
func (lqm *LinkQualityMonitor) GetInboundLinkQuality(localSeq uint32) *LinkMetrics {
    lqm.mu.RLock()
    defer lqm.mu.RUnlock()
    return lqm.inboundLinks[localSeq]
}
```

**RTT 测量**：
```go
// 使用 libp2p 内置 ping 协议
func (lqm *LinkQualityMonitor) probeRTT() {
    ticker := time.NewTicker(lqm.probeInterval)
    defer ticker.Stop()
    
    for {
        select {
        case <-ticker.C:
            for peerID := range lqm.directLinks {
                go func(pid peer.ID) {
                    // libp2p 内置 ping
                    result := <-ping.Ping(lqm.ctx, lqm.host, pid)
                    if result.Error != nil {
                        lqm.recordPingFailure(pid)
                    } else {
                        lqm.recordRTT(pid, result.RTT)
                    }
                }(peerID)
            }
        case <-lqm.stopCh:
            return
        }
    }
}
```

**质量分数计算**：
```go
func (lqm *LinkQualityMonitor) calculateQualityScore(m *LinkMetrics) float64 {
    // RTT 分数：越低越好（基准：100ms = 0 分，10ms = 100 分）
    rttScore := math.Max(0, 100 - (m.RTT - 10))
    
    // 错误率分数：越低越好
    errorScore := (1 - m.ErrorRate) * 100
    
    // 吞吐量分数：越高越好（对数刻度）
    throughputScore := math.Min(100, math.Log10(float64(m.Throughput)/1000) * 20)
    
    // 加权平均
    score := rttScore*0.4 + errorScore*0.4 + throughputScore*0.2
    
    return math.Max(0, math.Min(100, score))
}
```

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

#### 2.2.4 Custom Transport（新增）

**职责**：
- 实现 libp2p.Transport 接口
- 封装现有的代理连接（trojan/SOCKS5/H_Tunnel）

**TrojanTransport 示例**：
```go
type TrojanTransport struct {
    proxyConfig *TrojanProxyConfig
}

// 实现 libp2p.Transport 接口
func (t *TrojanTransport) Dial(ctx context.Context, addr multiaddr.Multiaddr, peerID peer.ID) (transport.CapableConn, error) {
    // 通过 trojan 代理建立连接
    dialer := trojan.NewDialer(t.proxyConfig)
    conn, err := dialer.DialP2P()
    if err != nil {
        return nil, err
    }
    
    // 包装成 libp2p 连接
    return t.wrapConn(conn, peerID)
}

func (t *TrojanTransport) Listen(addr multiaddr.Multiaddr) (transport.Listener, error) {
    // trojan 是客户端协议，不支持监听
    return nil, errors.New("trojan transport does not support listening")
}
```

### 2.3 数据流

#### 2.3.1 发送 mesh 包

```
Application (SSH/HTTP)
    ↓
gVisor netstack (TCP/UDP)
    ↓
RouteSelector (路由决策)
    ↓
MeshManager.HandleMeshPacket()
    ↓
MeshStreamHandler.SendPacket()
    ↓
libp2p stream (yamux multiplexing)
    ↓
TrojanTransport / SOCKS5Transport
    ↓
Proxy connection (trojan/SOCKS5 over TCP)
    ↓
Network
```

#### 2.3.2 接收 mesh 包

```
Network
    ↓
Proxy connection (trojan/SOCKS5 over TCP)
    ↓
TrojanTransport / SOCKS5Transport
    ↓
libp2p stream (yamux demultiplexing)
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

## 3. 详细设计

### 3.1 删除的组件

以下组件将被 libp2p 替代，需要删除：

| 组件 | 文件 | 行数 | 说明 |
|------|------|------|------|
| `P2PManager` | `p2p/p2p.go` | ~500 | 由 `Libp2pHost` 替代 |
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
| `Libp2pHost` | `p2p/libp2p_host.go` | ~300 | libp2p host 封装 |
| `LinkQualityMonitor` | `p2p/link_quality.go` | ~400 | 链路质量监控 |
| `MeshStreamHandler` | `p2p/mesh_stream.go` | ~200 | mesh stream 处理 |
| `TrojanTransport` | `p2p/transport_trojan.go` | ~150 | trojan transport |
| `SOCKS5Transport` | `p2p/transport_socks5.go` | ~150 | SOCKS5 transport |
| `H_TunnelTransport` | `p2p/transport_htunnel.go` | ~150 | H_Tunnel transport |
| **总计** | | **~1350** | |

**净变化**：+100 行（但复杂度大幅降低）

### 3.3 修改的组件

| 组件 | 文件 | 修改内容 |
|------|------|---------|
| `RouteSelector` | `mesh/route_selector.go` | 集成 `LinkQualityMonitor`，查询链路质量 |
| `MeshManager` | `mesh/mesh.go` | 使用 `MeshStreamHandler` 发送/接收 mesh 包 |
| `main_engine.go` | `main_engine.go` | 初始化 `Libp2pHost` 替代 `P2PManager` |

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
    peers := s.p2pManager.GetPeers()  // 从 P2PManager 获取
    // ...
}

// 修改后
func (s *Server) handleGetPeers() {
    peers := s.libp2pHost.GetPeers()  // 从 Libp2pHost 获取
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

**阶段 2：P2P 层重构（2 周）**
- [ ] 实现 `MeshStreamHandler`
- [ ] 移除 `P2PManager`、`DRRScheduler`、`writeLoop`、`readLoop`
- [ ] 集成 `Libp2pHost` 到 `main_engine.go`
- [ ] 集成测试：单节点 mesh 包收发

**阶段 3：链路质量监控（1 周）**
- [ ] 实现 `LinkQualityMonitor`
- [ ] 实现 RTT 探测（libp2p ping）
- [ ] 实现错误率统计
- [ ] 实现链路质量广播（gossip）
- [ ] 集成测试：链路质量测量

**阶段 4：路由决策集成（1 周）**
- [ ] `RouteSelector` 集成 `LinkQualityMonitor`
- [ ] 实现多跳路径质量计算
- [ ] 优化路由决策算法
- [ ] 集成测试：路由决策基于链路质量

**阶段 5：测试与调优（2 周）**
- [ ] 多节点 mesh 测试（QG、VM、GG、JF、MS9、MS10）
- [ ] 性能基准测试（对比当前实现）
- [ ] 稳定性测试（长时间运行）
- [ ] 兼容性测试（新旧版本混合运行）

**总工期**：7 周（1 人全职）

### 4.2 兼容性策略

**问题**：新旧版本无法直接通信（协议不同）

**方案**：强制全量升级

**理由**：
1. 环境数量有限（6 个节点），全量升级可控
2. 新旧协议兼容会增加复杂度
3. 性能提升显著，值得一次性升级

**升级顺序**：
1. 先在 VM 环境测试（开发环境）
2. 验证通过后，部署到 QG、GG、JF
3. 最后部署到 MS9、MS10

### 4.3 回滚方案

如果新版本出现严重问题：
1. 保留旧版本二进制
2. 快速回滚到旧版本（5 分钟内）
3. 分析问题，修复后重新部署

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
