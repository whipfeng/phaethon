# Dialer BindStrategy 抽象

## Context

`h_tunnel/trojan/socks5` 三个 dialer 都有相同的 v1/v2 模式切换：

| 协议       | v1 (P2P=false)                                | v2 (P2P=true)                                | 切换入口                |
|------------|-----------------------------------------------|----------------------------------------------|-------------------------|
| trojan     | BIND PORT=1（control）/PORT=0（reverse）raw tls.Conn | BIND PORT=2 + frame.NewStreamTransport       | `IsP2P()` → StartPeer    |
| socks5     | BIND PORT=1/0 raw net.Conn                    | BIND PORT=2 + frame.NewStreamTransport       | `IsP2P()` → StartPeer    |
| h_tunnel   | "CONN"/"BIND" 三步 HEAD 字节流                | "MESH" 单步 HEAD + htunnelDirectTransport    | `IsP2P()` → StartPeer    |

现在每个协议自己实现 `DialControl/DialP2P/DialReverse` 三个方法（`dialer/trojan.go:67-126`、`dialer/socks5.go:70-114`、`dialer/htunnel.go:234-251`），里面大量重复：
- 同一个 TLS/TCP dial
- 同一个 BIND 命令构造
- 同样的 P2P 模式 frame 包装

加新协议（VMess/VLESS）接入 mesh P2P 时需要重新写三遍 if/else，认知成本高。

## 目标

抽 `BindStrategy` 接口 + 注册表，把「dial proxy + 协议特定 BIND 握手 + P2P 模式包 frame」这一步形式化。新增协议只要：
1. 实现 `BindStrategy`（30-50 行）
2. 在 `init()` 注册到表里
3. 自动获得 v1（control/reverse）和 v2（P2P）两条路径

## 设计

### 核心类型（`dialer/bind_strategy.go`，新文件）

```go
// BindMode selects the BIND semantic. Mirrors reverse.BindPort* values
// so existing server-side protocol handlers (which read DST.PORT) need
// no changes.
type BindMode int

const (
    BindModeData    BindMode = 0  // reverse/data connection
    BindModeControl BindMode = 1  // control/listen for forwarding
    BindModeP2P     BindMode = 2  // mesh P2P transport
)

// BindResult is what a BIND handshake returns.
type BindResult struct {
    Conn  net.Conn              // always set (raw transport, possibly TLS-wrapped)
    Frame frame.FrameTransport  // set only when mode == BindModeP2P and SupportsP2P
    Mode  BindMode
}

// BindStrategy is the per-protocol BIND handshake implementation.
type BindStrategy interface {
    // Bind dials the proxy and performs the BIND handshake.
    // dstAddr is the reverse address (used only for BindModeData).
    Bind(ctx context.Context, proxy *config.Proxy, mode BindMode, dstAddr string) (*BindResult, error)

    // SupportsP2P returns true if the protocol has a v2 (P2P) variant.
    SupportsP2P() bool
}

// Registry.
var bindRegistry = map[string]BindStrategy{}

func RegisterBindStrategy(proxyType string, s BindStrategy) { ... }
func GetBindStrategy(proxyType string) (BindStrategy, bool) { ... }
```

### Per-protocol 实现（`dialer/<proto>_bind.go`，新文件）

每个协议一个文件，把现有 `DialControl/DialP2P/DialReverse` 三个方法的 BIND 握手部分抽出来：

```go
// dialer/trojan_bind.go
type trojanBindStrategy struct{}

func (trojanBindStrategy) Bind(ctx context.Context, p *config.Proxy, mode BindMode, dstAddr string) (*BindResult, error) {
    port := portForMode(mode)  // 0/1/2
    addr := dstAddr
    if mode != BindModeData {
        addr = p.Server
    }
    tlsConn, err := tls.Dial("tcp", net.JoinHostPort(p.Server, strconv.Itoa(p.Port)), p.TLSConfig)
    if err != nil { return nil, err }
    if err := SendTrojanRequestWithCmd(tlsConn, 0x02, addr, port); err != nil { tlsConn.Close(); return nil, err }
    r := &BindResult{Conn: tlsConn, Mode: mode}
    if mode == BindModeP2P {
        r.Frame = frame.NewStreamTransport(tlsConn)
    }
    return r, nil
}

func (trojanBindStrategy) SupportsP2P() bool { return true }

func init() { RegisterBindStrategy(config.ProxyTROJAN, trojanBindStrategy{}) }
```

类似 socks5 和 h_tunnel 各自实现。h_tunnel 特殊点：mode 映射到 "CONN"/"BIND"/"MESH" 字符串命令，dstAddr 直接作为绑定参数。

### 现有接口瘦身

`P2PDialer/ControlDialer/ReverseDialer` 三个接口保留（callers 太多），但实现变成 `BindStrategy` 的薄包装：

```go
// dialer/trojan.go (existing file, modified)
func (d *TrojanDialer) DialP2P() (frame.FrameTransport, error) {
    s, _ := GetBindStrategy(config.ProxyTROJAN)
    r, err := s.Bind(context.Background(), d.Proxy, BindModeP2P, "")
    if err != nil { return nil, err }
    return r.Frame, nil
}

func (d *TrojanDialer) DialControl() (net.Conn, error) {
    s, _ := GetBindStrategy(config.ProxyTROJAN)
    r, err := s.Bind(context.Background(), d.Proxy, BindModeControl, "")
    if err != nil { return nil, err }
    return r.Conn, nil
}

func (d *TrojanDialer) DialReverse(dstAddr string) (net.Conn, error) {
    s, _ := GetBindStrategy(config.ProxyTROJAN)
    r, err := s.Bind(context.Background(), d.Proxy, BindModeData, dstAddr)
    if err != nil { return nil, err }
    return r.Conn, nil
}
```

净结果：每个 dialer 文件少 60-100 行重复代码，新增协议只要 30-50 行 BindStrategy 实现 + 1 行 register。

### 主调用点（`p2p/p2p.go`）保持不变

`p2p.StartPeer` 通过 `P2PDialer.DialP2P()` 接口多态调度，**不需要修改**——抽象对它是透明的。

## 改动量

### BindStrategy 重构（BIND 路径）

| 文件                         | 类型 | 改动量      |
|------------------------------|------|-------------|
| `dialer/bind_strategy.go`    | 新建 | +80 行      |
| `dialer/trojan_bind.go`      | 新建 | +60 行      |
| `dialer/socks5_bind.go`      | 新建 | +60 行      |
| `dialer/htunnel_bind.go`     | 新建 | +80 行      |
| `dialer/trojan.go`           | 修改 | -50 行（瘦身）|
| `dialer/socks5.go`           | 修改 | -50 行（瘦身）|
| `dialer/htunnel.go`          | 修改 | -80 行（瘦身）|

净增 ~100 行，新增协议节省 ~200 行（每协议）。

### Dial() 路径 v2 分发

| 文件 | 改动 |
|------|------|
| `dialer/dialer.go` | +20 行（meshAwareDialer + NewDialer v2 包装）|
| `dialer/socks5.go` Dial() | 零改动 |
| `dialer/trojan.go` Dial() | 零改动 |
| `dialer/htunnel.go` Dial() | 零改动（h_tunnel v2 已经是 mesh）|

净增 ~20 行，**3 个协议 Dial() 零改动**。

## 兼容性

- `BindMode` 数值与现有 `reverse.BindPort*` 常量完全一致（0/1/2），server 端代码无需改动
- `P2PDialer/ControlDialer/ReverseDialer` 接口签名不变，所有 callers（`p2p/p2p.go`、`control_client.go`、`reverse.go` 等）零修改
- 行为零变化：纯重构，输出 byte-for-byte 一致

## v1/v2 dispatch for Dial() 路径

### Context

`proxy.IsP2P()=true` 时，`p2p.StartPeer` 通过 `DialP2P()`（BindStrategy v2 路径）已经在跑，已经为该代理建立了一条 mesh IPIP 隧道到远端节点。但是 `dialer.NewDialer(proxy).Dial(dstAddr, dstPort)` 这条**直连拨号路径**仍然走协议握手（socks5 CONNECT / trojan CONNECT），把数据推到远端 SOCKS5/Trojan 服务端。

正确的数据流应该是：
- v2（p2p=true + v2=true）：本地 netstack → IPIP 隧道 → 远端 netstack → 远端内核 socket
- v1（其他情况）：本地 → SOCKS5/Trojan 协议握手 → 远端代理服务端 → 目标

链路建立（BindStrategy/DialP2P）协议相关，但**外层数据通道（mesh IPIP 隧道）应当统一**。
h_tunnel 的 `DialP2P()` 走 v2 MESH（用于 mesh gossip），但 `Dial()`（数据拨号）仍然走 h_tunnel 协议 CONN——和 socks5/trojan 一样需要被替换。

### 设计决策（2026-10-12 更新）

**v2 开关模型**：在代理配置上新增独立的 `v2` 布尔字段，默认 `false`。

- **必须同时满足 `p2p: true` 且 `v2: true` 才走 v2 路径**
- 只有 `p2p: true` 而 `v2: false`（默认）：仍走 v1 协议握手（现有稳定基线）
- `p2p: false`：无论 `v2` 如何，都走 v1（v2 依赖 mesh P2P 链路）

**v2 数据路径**：把目标地址做 IPIP 封装，直接送入对应 peer 链路的 mesh 网络，不经过代理服务器的协议握手转发。

**h_tunnel 特殊处理**：当 `p2p: true` 且 `v2: true` 时，h_tunnel 不走 v1 的 BIND 三步握手，切换到专为 mesh 准备的升级协议（`dialHTunnelMeshDirect` / `X-C: MESH` 单步 HEAD 路径）。

**trojan/socks5**：当 `p2p: true` 且 `v2: true` 时，同样走 IPIP 封装入 mesh，不需要额外的协议升级，只是数据面从代理 server 切换到 mesh 链路。

### 设计（2026-10-12 修正）

**入口：`dialer.NewDialer(proxy)` 加 v2 包装**（不变）

```go
func NewDialer(proxy *config.Proxy) Dialer {
    if proxy == nil { return &DirectDialer{} }
    var d Dialer
    switch strings.ToUpper(proxy.Type) {
    case config.ProxySOCKS5:
        d = &Socks5Dialer{BaseDialer: BaseDialer{Proxy: proxy}}
    // ... 现有 switch ...
    default:
        return &stubDialer{name: proxy.Type}
    }
    
    // v2 dispatch: 必须 p2p=true 且 v2=true 才走 mesh 直连
    if proxy.IsP2P() && proxy.V2 {
        return &meshAwareDialer{proxy: proxy}
    }
    return d
}
```

**`meshAwareDialer`：v2 包装，绕开协议握手**

```go
type meshAwareDialer struct {
    proxy *config.Proxy
}

func (d *meshAwareDialer) Dial(dstAddr string, dstPort int) (net.Conn, error) {
    return MeshDial(dstAddr, dstPort, "", "PROXY:"+d.proxy.Name, nil)
}

func (d *meshAwareDialer) ServerAddr() (string, int) { return "", 0 }
```

### IPIP 封装与路由回调

#### 核心问题

v2 的目标 IP 可能不在任何通告路由中（如外部 IP `8.8.8.8`），RouteSelector 无法决策。但链路已经由 proxy 配置选定，不需要 RouteSelector。

#### 现有 RouteSelector 如何选 NIC 和 EIP

当前 `SelectRoute` 回调（`mesh/netstack.go:174-248`）是一个闭包，直接访问 `meshMgr` 内部方法：

```
dst → 遍历路由表 rt.Routes
    → route.Prefix.Contains(dst)?        匹配 prefix
    → SelectEgressNodeID(dst, entries)    选出目标节点 targetNodeID
    → NextHopLink(targetNodeID)           Dijkstra 下一跳 → *LinkNIC
    → linkNIC.nicID                       作为 EgressNIC

Branch 3 额外：
    → GetEIPForNode(targetNodeID)         targetNodeID 的 subnet+4 → EgressEIP
    → NeedIPIP = true
```

关键：**LinkNIC 来自 Dijkstra 下一跳 peer 的 `*LinkNIC` 对象**，**EIP = 目标出口节点的 subnet+4**。

#### v2 路由回调：匹配现有模式

v2 跳过路由表和 Dijkstra，直接从 proxy 配置找到 peer 的 LinkNIC：

```
proxyName → p2p.proxyPeers[proxyName] → *Peer
    → peer.linkNIC.nicID               作为 EgressNIC（和 NextHopLink 返回的同一个东西）
    → CalculateEIP(peer.peerSubnet)     peer subnet+4 → EgressEIP（和 GetEIPForNode 返回的同一个东西）
    → NeedIPIP = true
```

返回类型和现有 RouteSelector **完全一致**（`stack.RouteDecision`），下游 IPIP 封装零改动。

#### 回调创建与函数注入

回调闭包在 `mesh` 包内创建（和现有 `SelectRoute` 一样在 `mesh/netstack.go`），直接访问 `mesh`/`p2p` 内部结构。但回调需要 `proxyName`（来自 `dialer` 层），而 `mesh` 包不能直接访问 `p2p.proxyPeers`。

解决方案：**函数注入**（和 `GlobalNetstackDialWithModeBFunc` 相同模式）。

```go
// p2p/p2p.go — 函数注入变量（不是公开方法）
var LookupProxyPeerRoute func(proxyName string) (linkNICID tcpip.NICID, peerSubnet *net.IPNet, ok bool)

// main.go 初始化时注入实现
p2p.LookupProxyPeerRoute = func(proxyName string) (tcpip.NICID, *net.IPNet, bool) {
    peer, ok := globalP2PManager.proxyPeers[proxyName]
    if !ok || peer.linkNIC == nil {
        return 0, nil, false
    }
    return peer.linkNIC.nicID, peer.peerSubnet, true
}
```

回调闭包在 `mesh` 包内创建，调用注入函数并转换为 `stack.RouteDecision`：

```go
// mesh/netstack.go — 创建 v2 路由回调
func CreateV2RouteFunc(proxyName string) func() stack.RouteDecision {
    return func() stack.RouteDecision {
        linkNICID, peerSubnet, ok := p2p.LookupProxyPeerRoute(proxyName)
        if !ok {
            return stack.RouteDecision{}
        }
        eip := CalculateEIP(peerSubnet) // subnet+4，已有函数
        return stack.RouteDecision{
            EgressNIC: linkNICID,
            EgressEIP: tcpip.AddrFrom4Slice(eip),
            NeedIPIP:  true,
            Cacheable: true,
        }
    }
}
```

#### gVisor fork — per-endpoint RouteFunc

当前路由是 stack 级的（`RouteSelector`），不是 per-endpoint。v2 需要 per-endpoint 路由回调，因为不同 socket 可能对应不同 proxy/peer。

**fork 补丁 #13：per-endpoint RouteFunc**

```go
// pkg/tcpip/transport/tcp/endpoint.go — tcp.Endpoint 新增字段
type Endpoint struct {
    ...
    // RouteFunc is an optional per-endpoint route callback.
    // When set, connect() calls it instead of stack.FindRoute().
    // Returns a RouteDecision (same type as RouteSelector).
    RouteFunc func() RouteDecision
}
```

```go
// pkg/tcpip/transport/tcp/endpoint.go — connect() 中检查
// 在现有 e.stack.FindRoute() 调用之前（line 2442）：
if e.RouteFunc != nil {
    decision := e.RouteFunc()
    r, err := e.stack.BuildRouteFromDecision(decision, netProto)
    if err != nil {
        return err
    }
    e.route = r
    // 跳过 e.stack.FindRoute()，继续后续握手
} else {
    r, err := e.stack.FindRoute(nicID, localAddr, remoteAddr, netProto, false)
    // ... 现有逻辑 ...
}
```

```go
// pkg/tcpip/stack/stack.go — 新增 BuildRouteFromDecision
// 复用 findRouteInner 中已有的 RouteDecision → Route 构建逻辑
func (s *Stack) BuildRouteFromDecision(decision RouteDecision, netProto tcpip.NetworkProtocolNumber) (*Route, tcpip.Error) {
    // 和 findRouteInner 中 decision.NeedIPIP / decision.EgressNIC 分支相同的路由构建
}
```

#### 数据流

```
TUN forwarder
  → dialer.NewDialer(proxy).Dial(dst, port)
    → meshAwareDialer.Dial(dst, port, "", "PROXY:"+proxyName, nil)
      → MeshDial → GlobalNetstackDialWithModeBFunc
        → gVisor netstack 创建 TCP endpoint
        → endpoint.RouteFunc = mesh.CreateV2RouteFunc(proxyName)
        → endpoint.connect()
          → fork 检查 e.RouteFunc != nil
            → 调用 RouteFunc() → RouteDecision{EgressNIC=linkNIC, EgressEIP=peer EIP, NeedIPIP=true}
            → BuildRouteFromDecision → Route（带 IPIP 字段）
          → TCP 握手（SYN → SYN-ACK → ACK）
            → 发送时检查 route.NeedIPIP
              → forwardWithIPIPEncapsulation()（已有逻辑）
                → 外层: src=本地 EIP, dst=peer EIP, proto=4
                → 内层: 原始 TCP 包（dst=目标 IP）
              → 从 Link NIC 发出 → P2P 链路
```

#### 优点

- **回调跟随 socket 生命周期**，零维护（无额外表，无清理逻辑）
- **返回 `stack.RouteDecision`**，和现有 RouteSelector 完全一致，IPIP 封装零改动
- **函数注入**，不新增 P2PManager 公开方法，和现有 `GlobalNetstackDialWithModeBFunc` 模式一致
- **未设回调时完全走现有 RouteSelector**，零影响
- **proxy → peer → LinkNIC 映射**复用已有 `proxyPeers` map，无需新数据结构

### 影响范围

| 文件 | 改动 |
|------|------|
| `config/config.go` | +1 字段（`V2 bool`），YAML tag `v2` |
| `dialer/dialer.go` | 启用 v2 dispatch 条件（`proxy.IsP2P() && proxy.V2`） |
| `dialer/htunnel_bind.go` | BindModeP2P 条件加 `p.V2`，启用 MESH direct |
| `p2p/p2p.go` | +1 函数注入变量 `LookupProxyPeerRoute` |
| `mesh/netstack.go` | +1 函数 `CreateV2RouteFunc(proxyName)` |
| gVisor fork `tcp.Endpoint` | +1 字段 `RouteFunc`（补丁 #13） |
| gVisor fork `connect()` | +1 if 分支（检查 RouteFunc） |
| gVisor fork `stack.go` | +1 方法 `BuildRouteFromDecision` |
| `main.go` | 注入 `LookupProxyPeerRoute` 实现 |
| `dialer/socks5.go` Dial() | **零改动** |
| `dialer/trojan.go` Dial() | **零改动** |
| `dialer/htunnel.go` Dial() | **零改动** |

所有 `dialer.NewDialer(p).Dial(...)` 调用点（TUN forwarder）零改动——v1 和 v2 都在 Dialer 接口背后自动切换。三个协议完全统一。

### 兼容性

- 新增 `v2` 字段默认 `false`，现有配置零影响——升级后所有代理行为不变
- `proxy.IsP2P()` 已经存在：`socks5/trojan/h_tunnel` 默认 true，其他类型 false
- TUN forwarder 调用 `dialer.NewDialer(proxy).Dial()` 不变
- h_tunnel v1 协议 CONN 保留作 `v2=false` 的 fallback
- 服务端（远端 SOCKS5/Trojan/h_tunnel server）**完全不动**——v2 数据流不经过协议服务端
- gVisor fork 补丁 #13 向后兼容：`RouteFunc` 为 nil 时行为与当前完全一致

### 验证

1. `go build ./...` 编译通过
2. 默认行为不变：所有代理 `v2` 未配置时走 v1 协议握手
3. VM 环境：手动配置 `p2p: true, v2: true` 的代理，验证走 mesh IPIP 隧道
4. 仅 `p2p: true, v2: false` 的代理仍走 v1 协议握手
5. h_tunnel `p2p: true, v2: true` 走 MESH direct（`X-C: MESH` 单步 HEAD）
6. 现有 v1 代理（SSH 到 GG 等）不受影响
7. 目标为外部 IP（如 `8.8.8.8`）的 v2 代理能正确通过 IPIP 封装送达

## 不在范围内

- 不改 h_tunnel v1 内部实现（`htunnel.go` 的 v1 CONN/BIND 传输细节）
- 不动 UDP dialer（`socks5_udp.go` 走 SOCKS5 UDP ASSOCIATE，不是 BIND）
- 不改 mesh IPIP 封装/转发路径（已有的 Link NIC + IPIP 架构不变）
- 不改 BindStrategy / DialP2P 链路建立逻辑

## V2 语义与热响应（2026-10-13 讨论确认）

### V2 是传输通道选择，不是 Peer 生命周期事件

三种协议（socks5/trojan/h_tunnel）的 v1 和 v2 **目标相同**：数据都到达 mesh P2P 网络，绕过代理服务器的业务逻辑。区别仅在传输通道：

| 协议 | v1 通道 | v2 通道 |
|------|---------|---------|
| socks5 | BIND PORT=2 raw `net.Conn` + `frame.StreamTransport` | Link NIC IPIP 封装（`meshAwareDialer`） |
| trojan | BIND PORT=2 raw `tls.Conn` + `frame.StreamTransport` | Link NIC IPIP 封装（`meshAwareDialer`） |
| h_tunnel | BIND PORT=2 stream（`X-C: BIND`） | MESH 通道（`X-C: MESH` 单步 HEAD + `htunnelDirectTransport`） |

### V2 是 per-socket/per-connection 决策

- **socks5/trojan**：`meshAwareDialer.Dial()` 每次调用时读取 `proxy.V2`，决定走 IPIP 还是 v1 BIND
- **h_tunnel**：`BindModeP2P` 每次调用时读取 `p.IsP2P() && p.V2`，决定走 MESH 通道还是 v1 BIND stream
- 每次新建连接时独立决策，已有连接不受影响

### 切换 V2 不需要重启 Peer

Peer 连接（hello/heartbeat/LinkNIC 建立）独立于数据通道选择：
- Peer 在 v1 和 v2 模式下都存在
- 切换 V2 只影响新数据连接的通道选择
- `mesh.CreateV2RouteFunc(proxyName)` 每次 Dial 时查询 `p2p.LookupProxyPeerRoute()`，运行时获取 LinkNIC 信息

### V2 字段独立存储，`IsV2()` 控制生效

```go
// config.Proxy
V2 bool  // 用户设置，独立存储

func (p *Proxy) IsV2() bool {
    if !p.IsP2P() { return false }  // P2P=false 时 V2 不生效
    return p.V2
}
```

- V2 字段保留用户配置意图，不随 P2P 开关自动重置
- 用户临时关 P2P 调试，再开时 V2 自动恢复生效
- 控制台表格显示 `IsV2()` 的有效状态（P2P=false 时显示 ❌）

### 控制台热响应

- **V2 切换**：保存配置即生效，新连接立即使用新路径。`OnIncrementalUpdate` 无需特殊处理
- **P2P 切换**：`OnIncrementalUpdate` 通过集合差比较检测变化，调用 `StopPeerByProxyName` / `StartPeer`
- **P2P 关闭时**：V2 配置保留，表格显示 ❌（`IsV2()` 返回 false），Peer 连接应被停止
