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
- v2（p2p=true）：本地 netstack → IPIP 隧道 → 远端 netstack → 远端内核 socket
- v1（p2p=false）：本地 → SOCKS5/Trojan 协议握手 → 远端代理服务端 → 目标

链路建立（BindStrategy/DialP2P）协议相关，但**外层数据通道（mesh IPIP 隧道）应当统一**。
h_tunnel 的 `DialP2P()` 走 v2 MESH（用于 mesh gossip），但 `Dial()`（数据拨号）仍然走 h_tunnel 协议 CONN——和 socks5/trojan 一样需要被替换。

### 设计

**入口：`dialer.NewDialer(proxy)` 加 v2 包装**

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
    
    // v2 dispatch: IsP2P() 对不支持的协议（http/ssh/hysteria2/vless/...）返回 false
    if proxy.IsP2P() {
        return &meshAwareDialer{proxy: proxy}
    }
    return d
}
```

**`meshAwareDialer`：v2 包装，绕开协议握手**

```go
// meshAwareDialer 替换协议握手，把数据直送 mesh。
// v2 数据通道统一走 MeshDial，不需要协议层的 ConnID/log。
// 注：BaseDialer.ConnID 是 v1 协议握手日志用的字段，v2 由 connlog 用 inbound 标识，
// 所以这里不继承 BaseDialer。
type meshAwareDialer struct {
    proxy *config.Proxy
}

func (d *meshAwareDialer) Dial(dstAddr string, dstPort int) (net.Conn, error) {
    return MeshDial(dstAddr, dstPort, "", "PROXY:"+d.proxy.Name, nil)
}

func (d *meshAwareDialer) ServerAddr() (string, int) { return "", 0 }
```

### 影响范围

| 文件 | 改动 |
|------|------|
| `dialer/dialer.go` | +20 行（meshAwareDialer + NewDialer 加 if） |
| `dialer/socks5.go` Dial() | **零改动**（v1 路径保留，p2p=true 时被 NewDialer 替换） |
| `dialer/trojan.go` Dial() | **零改动**（同上） |
| `dialer/htunnel.go` Dial() | **零改动**（同上，htunnel v1 CONN 保留作 p2p=false fallback）|

所有 `dialer.NewDialer(p).Dial(...)` 调用点（TUN forwarder）零改动——v1 和 v2 都在 Dialer 接口背后自动切换。三个协议完全统一。

### 兼容性

- `proxy.IsP2P()` 已经存在：`socks5/trojan/h_tunnel` 默认 true，其他类型 false（`config/config.go:IsP2P`）
- TUN forwarder 调用 `dialer.NewDialer(proxy).Dial()` 不变
- h_tunnel v1 协议 CONN 保留作 `p2p=false` 的 fallback
- 服务端（远端 SOCKS5/Trojan/h_tunnel server）**完全不动**——v2 数据流不经过协议服务端

### 验证

1. `go build ./...` 编译通过
2. VM 环境：
   - SOCKS5_7890（p2p=true）访问外部 IP（mtalk.google.com / baidu.com）成功，**不再报 `socks5: command failed, status: 5`**
   - 走 mesh IPIP 隧道，日志显示 `[MESH-DIAL]` 而非 `[SOCKS5-DIAL]`
3. SOCKS5_7890（p2p=false 或 http/ssh 协议）仍然走原协议握手（v1 路径不变）
4. 现有 5 个并发 SSH 到 GG 仍正常（BIND 路径未受影响）
5. QG 环境：部署后 SSH 到外部 IP 仍正常（与 patch #8 修复路径一致）

## 不在范围内

- 不改 h_tunnel v1/v2 内部实现（`htunnel.go` + `htunnel_direct.go` 的 transport 细节）
- 不动 UDP dialer（`socks5_udp.go` 走 SOCKS5 UDP ASSOCIATE，不是 BIND）
