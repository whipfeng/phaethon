# TUN 显而易见问题修复计划

## 元数据

- 文档类型：Plan
- 版本：v0.1.0
- 所属项目：phaethon
- 创建日期：2026-08-25

## 背景

phaethon 当前采用 **TUN 模式** 实现系统级流量拦截：系统 TUN 接口 + gvisor 用户态网络栈 + Fake-IP + DNS 劫持，将所有流量引入 phaethon，再由规则引擎决定直连或进入代理链。

在讨论中曾评估是否转向 Proxifier 风格（Winsock LSP / API Hook / WFP / DLL 注入）实现。结论是 **保持 TUN 模式作为主要演进方向**，原因包括：

- Proxifier 风格依赖进程级过滤/注入，跨平台能力差，且反作弊/安全软件容易检测到注入 DLL。
- Windows 下 WFP 重定向需要处理代理进程自身流量循环；用户态 Hook 需普通代码签名，写 WFP callout driver 才需 EV/WHQL。
- TUN 模式问题（路由、DNS、网口选择）是已知且可逐步修复的；Proxifier 问题是结构性、难以完全消除的。

详细结论见本文件第 5 节“代码审查结论汇总”与第 6 节“待办改进项”。

## 1. 显而易见问题修复

` tun/engine.go`、`tun/device_windows.go` 等文件已实现基于 gvisor netstack 的 TUN 流量拦截。代码 review 中发现几个不需要大改架构、但容易出问题的明显缺陷：

1. `readLoop` 把所有包都按 IPv4 注入 netstack，IPv6 包会被错误解析。
2. `readLoop` 复用同一个 2048 字节 buffer，存在数据被覆盖的风险。
3. `proxyDesc` 对 DIRECT 的判断因为大小写不一致而失效。

本次修复只针对这 3 个显而易见的问题，**不开启真实 TUN 进行整机测试**（避免把当前工作网络搞挂）。真实 TUN 拦截测试留到后续在独立 VM/测试机上进行。

## 新增目标

5. 排除 LAN/私网流量，避免 TUN 启用后本地网络中断。
6. 修复 UDP 转发语义，使用 `net.PacketConn` 保持数据报边界，避免把 UDP 当 TCP 流 relay。

## 阶段 1: readLoop 修复

### Task 1.1: IPv4/IPv6 协议识别
- 读取 TUN 包后，根据 IP 头第一个字节的高 4 位判断版本：
  - `0x40` (4) → IPv4
  - `0x60` (6) → IPv6
- 分别调用 `linkEP.InjectInbound(ipv4.ProtocolNumber, pkt)` 或 `linkEP.InjectInbound(ipv6.ProtocolNumber, pkt)`。
- 非 IPv4/IPv6 包直接丢弃。

### Task 1.2: 每包独立 buffer
- 在 `readLoop` 里每次读到数据后，复制到新的 `make([]byte, n)` 再交给 `buffer.MakeWithData`。
- 替代当前复用 `buf := make([]byte, 2048)` 的方案。
- 为减少 GC 压力，可引入 `sync.Pool`（可选，本次先保证正确性）。

## 阶段 2: proxyDesc 大小写修复

### Task 2.1: 统一大小写比较
- `proxyDesc` 中使用 `strings.EqualFold(p.Type, config.ProxyDIRECT)` 替代 `p.Type == config.ProxyDIRECT`。
- 因为配置初始化时 `proxy.Type = strings.ToLower(proxy.Type)`，直接等于大写常量会永远失败。

## 阶段 3: LAN/私网排除

### Task 3.1: 默认 LAN CIDR 排除
- 在 `tun/route.go` 新增 `DefaultLANExclusions`，包含常见 IPv4 私网/本地/组播前缀：
  - `10.0.0.0/8`
  - `172.16.0.0/12`
  - `192.168.0.0/16`
  - `127.0.0.0/8`
  - `169.254.0.0/16`
  - `224.0.0.0/4`
  - `255.255.255.255/32`
- `Engine.Start()` 将 LAN CIDR 与代理服务器 IP 合并后传给 `RouteManager.SetExclusions`。

### Task 3.2: RouteManager 支持 CIDR 排除
- `platformSetup` 遍历 exclusions 时同时处理纯 IP 和 CIDR：
  - Windows: 通过 `parseExclusionCIDR` 区分 `/32` 与具体前缀长度。
  - Linux: 通过 `parseExclusionLinux` 生成 `*net.IPNet`。
  - Darwin: `route -n add -host` / `route -n add -net` 分别处理。
- `deleteExclusionRoute` 同样根据是否含 `/` 删除主机或网络路由。

## 阶段 4: UDP 报文转发

### Task 4.1: 使用 net.PacketConn 保持数据报语义
- `handleUDP` 不再调用 `dialer.ChainDialWithID` 把 UDP 伪装成 TCP。
- 代理流量改用 `dialer.ChainUDPDial(proxy)` 获取 `net.PacketConn`。
- DIRECT UDP 通过 `directDialPacket()` 创建绑定原物理网卡的 UDP socket，绕过 TUN 路由。
- 新增 `relayUDP()`：
  - 一个方向从 `netstackConn.Read` 读取一个 UDP 数据报，通过 `targetConn.WriteTo` 发给目标。
  - 另一个方向从 `targetConn.ReadFrom` 读取一个数据报，通过 `netstackConn.Write` 写回 netstack。
  - 这样可以保持 UDP 数据报边界，不混淆多个数据包。

## 阶段 5: 路由感知 socket 绑定下沉到 dialer 包

当前 `setDirectSocketOption` 位于 `tun` 包，强依赖 `*Engine` 和 `RouteManager` 的内部字段。这导致：

- `dialer/direct.go` 的 `DirectDialer` 无法复用该能力，TUN 内部不得不单独实现 `directDial`/`directDialPacket`。
- 代理链首跳（SOCKS5/Trojan/HTunnel 等连接自己的服务器）无法做路由感知绑定，只能依赖启动时添加的静态 exclusion route。

本阶段将路由感知绑定能力下沉到 `dialer` 包，让 TUN 只负责提供网络上下文，所有出站连接统一复用。

**兼容性现状**（详见 `tun_spec.md` 第 8 节）：
- 可完全接入：`DirectDialer`、`HTTPDialer`、`ShadowsocksDialer`、`HTunnelDialer`。
- 间接受益/部分接入：`Socks5Dialer`、`TrojanDialer`、`SSHDialer`、`ReverseDialer`、`ControlClient`（TCP 首跳随 next-hop 链的 DirectDialer 接入而改善；UDP 控制面/数据面需额外改造）。
- 外部库封装 Dialer 统一通过库提供的全局钩子接入：`Hysteria2Dialer` 自定义 `ConnFactory`；`VLESSDialer` 已改为自研实现，直接通过 `DialRouteAware` 建立 TCP/TLS 首跳。两类均接入同一 `BindContext`，无需保留 exclusion route 兜底。

### Task 5.1: 新增 dialer.BindContext

在 `dialer/bind.go` 定义：

```go
package dialer

type BindContext struct {
    DefaultIfaceName  string
    DefaultIfaceIndex int
    TUNLUID           uint64   // Windows only
    DefaultGateway    net.IP   // Linux fallback
}

func (b *BindContext) BindSocket(c syscall.RawConn, dst net.IP) error
func (b *BindContext) BindDefaultInterface(c syscall.RawConn) error
func SetGlobalBindContext(bc *BindContext)
func GetGlobalBindContext() *BindContext
func DialRouteAware(network, addr string) (net.Conn, error)
```

- `BindSocket`：按目的地址选择真实网卡并绑定 socket。用于 DIRECT 流量和已知目的地址的代理首跳。
- `BindDefaultInterface`：直接绑定到启动时捕获的默认物理接口。用于无法从调用点获得目的地址的外部库封装 Dialer（如 xray-core UDP）。
- `SetGlobalBindContext` / `GetGlobalBindContext`：TUN 引擎注入/清理上下文；非 TUN 模式为 nil，行为不变。
- `DialRouteAware`：带路由感知的通用拨号函数，内部解析 addr 中的目的 IP 并调用 `BindSocket`。

### Task 5.2: 迁移平台相关实现

将 `tun/tun_direct_*.go` 中的平台相关逻辑迁移到 `dialer/bind_*.go`：

- `dialer/bind_windows.go`：`GetBestRoute2` + `IP_UNICAST_IF`。
- `dialer/bind_linux.go`：`SO_BINDTODEVICE`，先固定绑定默认接口；后续 Task 5.5 补充按目的地址查询。
- `dialer/bind_darwin.go`：`SO_IP_BOUND_IF`（或 `SO_BINDTODEVICE`），同 Linux 逐步实现。

`RouteManager` / `tun` 包中对应的 Windows API 包装（`getBestRouteInterface` 等）一并迁移，或保留 thin wrapper 供 `dialer` 调用。

### Task 5.3: DirectDialer 接入 BindContext

修改 `dialer/direct.go`：

```go
func (d *DirectDialer) Dial(dstAddr string, dstPort int) (net.Conn, error) {
    addr := net.JoinHostPort(dstAddr, strconv.Itoa(dstPort))
    return DialRouteAware("tcp", addr)
}

func (d *DirectDialer) DialPacket() (net.PacketConn, error) {
    // 如果全局 BindContext 存在，创建 UDP socket 时也绑定真实网卡
}
```

### Task 5.4: TUN 内部复用 DirectDialer

`Engine.directDial` 和 `Engine.directDialPacket` 改为直接调用 `dialer.DirectDialer` 或 `dialer.DialRouteAware`，删除 `tun` 包内重复的 socket 绑定代码。

`Engine.resolveDirect` 中的 DNS 查询 socket 也改用 `dialer.DialRouteAware`。

### Task 5.5: 代理链首跳接入 BindContext

修改各协议 Dialer 连接自身服务器的首跳逻辑：

- `dialer/socks5.go`：建立到 SOCKS5 服务器控制连接时调用 `DialRouteAware`。
- `dialer/trojan.go`：建立到 Trojan 服务器 TCP 连接时调用 `DialRouteAware`。
- `dialer/htunnel.go`：建立到 h_tunnel 服务器连接时调用 `DialRouteAware`。
- `dialer/ssh.go`：建立 SSH 服务器连接时调用 `DialRouteAware`。
- UDP 控制面（SOCKS5/Trojan UDP ASSOCIATE）同样调用带绑定的拨号。

实现方式：在 `NewDialer` 或各 Dialer 内部统一使用 `DialRouteAware` 替代裸 `net.DialTimeout`。

**外部库 Dialer 接入**：
- `Hysteria2Dialer`：自定义 `ConnFactory`，在 `New(addr)` 中创建 UDP socket 后调用 `BindContext.BindSocket(addr)`。quic-go 复用该 socket 建立 QUIC 连接，因此 TCP/UDP 统一绑定。
- `VLESSDialer`：已改为自研 VLESS 客户端实现，通过 `DialRouteAware` 建立到 VLESS 服务器的 TCP/TLS 首跳，不再使用 xray-core `RegisterDialerController`。

**无兜底分支**：上述改造完成后，所有 Dialer 均接入同一 `BindContext`，不再为任何代理类型保留静态 `exclusion route`。

### Task 5.6: TUN 引擎注入 BindContext

在 `tun/engine.go` 的 `Start()` 中，路由配置完成后：

```go
bc := &dialer.BindContext{
    DefaultIfaceName:  e.routeMgr.DefaultIfaceName,
    DefaultIfaceIndex: e.routeMgr.DefaultIfaceIndex,
    TUNLUID:           e.routeMgr.tunLUID,
    DefaultGateway:    e.routeMgr.originalGateway,
}
dialer.SetGlobalBindContext(bc)
```

在 `Stop()` 中清理：

```go
dialer.SetGlobalBindContext(nil)
```

### Task 5.7: Linux DIRECT 路由感知

在 `dialer/bind_linux.go` 中实现按目的地址查询路由表：

- 候选方案：netlink `RouteGet(dst)` 或解析 `/proc/net/route`。
- 根据查询结果使用 `SO_BINDTODEVICE` 绑定到正确接口，而非固定默认接口。

### Task 5.8: DIRECT DNS 路由感知（Linux）

`Engine.resolveDirect` 中 DNS 查询 socket 在 Linux 上根据 DNS 服务器地址选择接口。依赖 Task 5.7 的路由查询函数。

### Task 5.9: 路由查询缓存

在 `dialer` 包中对 `(目的 IP, 协议) → 出口接口索引` 做短期缓存（TTL 30s）。缓存失效或 miss 时回退到路由表查询。

优先保证正确性，再考虑性能优化。

### Task 5.10: IPv6 DIRECT 支持

- Windows：补充 `IPV6_UNICAST_IF` 绑定。
- Linux/macOS：补充 IPv6 目的地址路由查询，`SO_BINDTODEVICE` 对 IPv6 socket 同样适用。

## 阶段 6: 验证

### Task 6.1: 单元测试
- `go test ./tun -v` 必须全部通过。
- `go test ./dialer -v` 必须全部通过。
- `go test ./...` 无回归。

### Task 6.2: 代码静态检查
- `go build ./...` 成功。
- `go vet ./tun ./dialer` 无新增告警。

## 验收标准

- `readLoop` 正确处理 IPv4/IPv6。
- `readLoop` 不再复用单 buffer。
- `proxyDesc` 对 DIRECT 显示为 `"DIRECT"`。
- LAN/私网流量默认绕过 TUN。
- UDP 转发保持数据报语义，不再当 TCP 流处理。
- `dialer` 包存在可注入的 `BindContext`，TUN 启动时正确注入。
- `DirectDialer` 和各协议 Dialer 首跳复用路由感知绑定；`VLESSDialer` 已改为自研实现并通过 `DialRouteAware` 接入，`Hysteria2Dialer` 通过 `ConnFactory` 接入同一 `BindContext`，不再保留 exclusion route 兜底。
- TUN 内部 `directDial`/`directDialPacket`/`resolveDirect` 不再重复实现 socket 绑定。
- Linux DIRECT 能根据目的地址选择正确接口。
- IPv6 DIRECT 在至少一个平台可用。
- `go test ./...` 通过，`go build ./...` 成功。
- 不在当前工作主机上启用真实 TUN。
