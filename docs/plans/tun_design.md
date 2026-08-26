# TUN 路由感知重构计划

## 元数据

- 文档类型：Plan
- 版本：v0.2.0
- 所属项目：phaethon
- 创建日期：2026-08-25
- 变更记录：
  - v0.2.0（2026-08-26）：取消代理服务器 exclusion route 兜底，改为 `BindContext`/socket 绑定作为唯一防环手段；保留 LAN/私网 exclusion route 作为本地网络保护；明确 SOCKS5/Trojan UDP 需自研绑定；不做引擎级内部看门狗。

## 背景

phaethon 当前采用 **TUN 模式** 实现系统级流量拦截：系统 TUN 接口 + gvisor 用户态网络栈 + Fake-IP + DNS 劫持，将所有流量引入 phaethon，再由规则引擎决定直连或进入代理链。

在讨论中曾评估是否转向 Proxifier 风格（Winsock LSP / API Hook / WFP / DLL 注入）实现。结论是 **保持 TUN 模式作为主要演进方向**，原因包括：

- Proxifier 风格依赖进程级过滤/注入，跨平台能力差，且反作弊/安全软件容易检测到注入 DLL。
- Windows 下 WFP 重定向需要处理代理进程自身流量循环；用户态 Hook 需普通代码签名，写 WFP callout driver 才需 EV/WHQL。
- TUN 模式问题（路由、DNS、网口选择）是已知且可逐步修复的；Proxifier 问题是结构性、难以完全消除的。

## 已落地内容（阶段 1–4）

当前 `tun/engine.go` 已经实现以下基础修复，无需重复开发：

1. `readLoop` 按 IP 版本字段（高 4 位）分发 `ipv4` / `ipv6`，非 IP 包丢弃。
2. `readLoop` 每包复制到独立 `make([]byte, n)` 后再注入 netstack，不再复用 2048 字节 buffer。
3. `proxyDesc` 使用 `strings.EqualFold` 判断 DIRECT，大小写不再导致失效。
4. `tun/route.go` 已定义 `DefaultLANExclusions`，覆盖常见 IPv4 私网/本地/组播前缀。
5. `handleUDP` 已改用 `ChainUDPDial` / `directDialPacket()` + `relayUDP()`，保持 UDP 数据报边界。

本次核心工作是 **阶段 5 及以后**。

## 新增/变更目标

1. **取消代理服务器 exclusion route 兜底**：所有出站连接（DIRECT、代理首跳）必须依赖 `BindContext`/socket 绑定防环，不再为已解析的代理服务器 IP 预加系统路由。
2. **保留 LAN/私网 exclusion route**：本地多播、广播、局域网直连不应被吸入 TUN，仍通过更精确的系统路由直接走物理网卡。
3. **路由感知绑定能力下沉到 `dialer` 包**：TUN 只负责注入上下文，各协议 Dialer 统一复用。
4. **不满足的协议改为自研/补齐**：重点是 SOCKS5 / Trojan UDP ASSOCIATE 控制面与数据面绑定；Hysteria2 通过 `ConnFactory` 绑定；VLESS 已自研，只需接入 `DialRouteAware`。
5. **系统 DNS 重定向跨平台补齐**：Windows 已用 `netsh`；Linux 兼容 `systemd-resolved` / `NetworkManager` / `/etc/resolv.conf`；macOS 用 `networksetup`。
6. **统一跨平台清理逻辑**：Linux/macOS 新增 `cleanup_other.go`；Windows 增加 DNS 恢复兜底。
7. **不做引擎级内部看门狗**：当前进程内探测 TUN DNS 路径不可靠，继续依赖外部 `LAYER_WATCHDOG_PID` 进程看门狗 + 强化清理逻辑做崩溃恢复。

## 阶段 5: 路由感知绑定下沉到 dialer 包

### Task 5.1: 新增 `dialer.BindContext`

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
func ListenPacketRouteAware(network, laddr string) (net.PacketConn, error)
```

- `BindSocket`：按目的地址查询系统路由表，绑定到查得的出口接口；查询时排除 TUN 接口。
- `BindDefaultInterface`：直接绑定到启动时捕获的默认物理接口；用于无法从调用点获得目的地址的外部库封装 Dialer。
- `DialRouteAware`：带路由感知的通用拨号函数，内部解析 addr 中的目的 IP 并调用 `BindSocket`。
- `ListenPacketRouteAware`：带路由感知的 UDP socket 创建，使用 `net.ListenConfig{Control: ...}`。

### Task 5.2: 迁移平台相关实现

将 `tun/tun_direct_*.go` 中的平台相关逻辑迁移到 `dialer/bind_*.go`：

- `dialer/bind_windows.go`：`GetBestRoute2` + `IP_UNICAST_IF` / `IPV6_UNICAST_IF`，索引按网络字节序处理。
- `dialer/bind_linux.go`：`SO_BINDTODEVICE`；按目的地址解析 `/proc/net/route` 选择接口。
- `dialer/bind_darwin.go`：`SO_IP_BOUND_IF`；按目的地址用 `route -n get` 或回退默认接口。

`tun` 包不再直接处理 socket 绑定细节。

### Task 5.3: `DirectDialer` 接入 `BindContext`

修改 `dialer/direct.go`：

```go
func (d *DirectDialer) Dial(dstAddr string, dstPort int) (net.Conn, error) {
    addr := net.JoinHostPort(dstAddr, strconv.Itoa(dstPort))
    return DialRouteAware("tcp", addr)
}

func (d *DirectDialer) DialPacket() (net.PacketConn, error) {
    return ListenPacketRouteAware("udp", "")
}
```

当全局 `BindContext` 为 nil 时，两个函数回退到标准 `net.DialTimeout` / `ListenUDP`，保证非 TUN 模式行为不变。

### Task 5.4: TUN 内部复用 `DirectDialer`

`Engine.directDial`、`Engine.directDialPacket`、`Engine.resolveDirect` 改为直接调用 `dialer.DirectDialer` 或 `dialer.DialRouteAware` / `ListenPacketRouteAware`，删除 `tun` 包内重复的 socket 绑定代码。

### Task 5.5: 各协议 Dialer 接入 `BindContext`

| Dialer | 接入方式 |
|---|---|
| `HTTPDialer` | 首跳直接调用 `DialRouteAware` |
| `ShadowsocksDialer` | TCP 首跳 `DialRouteAware`；UDP `ListenPacketRouteAware` |
| `HTunnelDialer` | `http.Transport.DialContext` 注入 `DialRouteAware` |
| `Socks5Dialer` | TCP 首跳经 `nextDialer` 自动改善；**自研/补齐 UDP ASSOCIATE 控制面 + relay socket 绑定** |
| `TrojanDialer` | TCP 首跳经 `nextDialer` 自动改善；**补齐 UDP 控制面/数据面绑定** |
| `SSHDialer` | 只有 TCP，间接受益 |
| `ReverseDialer` | TCP 是 registry 已有连接，不在 BindContext 范围；UDP 数据面 socket 绑定 |
| `ControlClient` | 控制面经 `ChainDial` 间接受益 |
| `Hysteria2Dialer` | `ConnFactory.New` 中创建 UDP socket 并调用 `BindSocket(serverAddr)` |
| `VLESSDialer` | 已自研，把连接 VLESS server 的 `nextDialer.Dial` 路径改为 `DialRouteAware` |

**无兜底分支**：所有 Dialer 均接入 `BindContext`；不再为任何代理类型保留静态 exclusion route。

### Task 5.6: TUN 引擎注入 `BindContext`

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

- 解析 `/proc/net/route`，找到目的地址最长匹配的非 TUN 路由条目。
- 根据条目使用 `SO_BINDTODEVICE` 绑定到正确接口。
- 避免引入 netlink 外部依赖。

### Task 5.8: DIRECT DNS 路由感知（Linux）

`Engine.resolveDirect` 中 DNS 查询 socket 改用 `dialer.DialRouteAware()` 创建，复用路由感知绑定。

### Task 5.9: 路由查询缓存

在 `dialer` 包中对 `(目的 IP, 协议) → 出口接口索引` 做短期缓存（TTL 30s）。缓存失效或 miss 时回退到路由表查询。

### Task 5.10: IPv6 DIRECT 支持

- Windows：补充 `IPV6_UNICAST_IF` 绑定。
- Linux/macOS：IPv6 socket 同样适用 `SO_BINDTODEVICE` / `SO_IP_BOUND_IF`。

## 阶段 6: 系统 DNS 重定向

### Task 6.1: Windows

沿用现有 `netsh interface ip set dns name=devName static <tun-ip>`，停止时恢复 DHCP 或原 DNS。

### Task 6.2: Linux

新增 `tun/dns_system_linux.go`：

- 检测 `systemd-resolved`：`resolvectl dns devName <tun-ip>`。
- 检测 `NetworkManager`：`nmcli connection modify ... ipv4.dns ...`。
- 回退：备份 `/etc/resolv.conf`，写入 `nameserver <tun-ip>`；停止时恢复备份。

### Task 6.3: macOS

新增 `tun/dns_system_darwin.go`：

- `networksetup -listnetworkserviceorder` 找活跃服务。
- `networksetup -setdnsservers <service> <tun-ip>`；恢复时用 `Empty` 表示 DHCP。

## 阶段 7: 跨平台清理与可用性

### Task 7.1: 启用 Linux/macOS TUN

修改 `tun/avail_other.go`：

- Linux 运行时检查 `/dev/net/tun` 是否可打开。
- Darwin 运行时检查 `com.apple.net.utun_control` 是否可连接。
- 失败时记录 warn 并返回 `false`，不再编译期硬编码不可用。

### Task 7.2: macOS 接口配置

在 `tun/route_darwin.go` 的 `platformSetup` 中：

1. `ifconfig <dev> inet <tun-ip> <tun-ip> netmask <mask> up`
2. 保存 `DefaultIfaceName` / `DefaultIfaceIndex`。

`platformTeardown` 中 `ifconfig <dev> down`。

### Task 7.3: 统一清理逻辑

- 新建 `tun/cleanup_other.go`：
  - Linux：删除 `0.0.0.0/1`、`128.0.0.0/1` 路由；`ip link set devName down`。
  - Darwin：删除 `0.0.0.0/1`、`128.0.0.0/1` 路由；`ifconfig devName down`。
- 更新 `tun/cleanup_windows.go`：增加删除系统 DNS 静态配置（恢复 DHCP）作为兜底。

### Task 7.4: 看门狗策略

- **不做引擎级内部看门狗**：进程内探测 TUN DNS 路径不可靠，容易误触发。
- 保留/强化外部 `LAYER_WATCHDOG_PID` 进程看门狗：父进程退出后自动清理路由/DNS。
- 依靠 `CleanupResidual` 在各平台启动时兜底，确保崩溃/强制杀进程后能恢复网络。

## 阶段 8: 验证

### Task 8.1: 单元测试

- `go test ./tun -v`
- `go test ./dialer -v`
- `go test ./...` 无回归

### Task 8.2: 静态检查

- `go build ./...` 成功（Windows/Linux/macOS 交叉编译）。
- `go vet ./tun ./dialer` 无新增告警。

### Task 8.3: 不开启真实 TUN

不在当前工作主机运行会调用 `CreateDevice` / `Setup` 的集成测试；真实 TUN 功能到独立 VM/测试机验证。

## 验收标准

- `readLoop` 正确处理 IPv4/IPv6；不再复用单 buffer。
- `proxyDesc` 对 DIRECT 显示为 `"DIRECT"`。
- LAN/私网流量默认绕过 TUN。
- UDP 转发保持数据报语义。
- `dialer` 包存在可注入的 `BindContext`，TUN 启动时正确注入、停止时清理。
- `DirectDialer` 和各协议 Dialer 首跳复用路由感知绑定；Socks5/Trojan UDP 已完成绑定改造；Hysteria2 通过 `ConnFactory` 接入；VLESS 已自研并接入。
- TUN 内部 `directDial` / `directDialPacket` / `resolveDirect` 不再重复实现 socket 绑定。
- Linux DIRECT 能根据目的地址选择正确接口。
- IPv6 DIRECT 在至少一个平台可用。
- Windows/Linux/macOS 均能在支持环境下 `Available() == true`。
- Linux/macOS 系统 DNS 可重定向到 TUN IP 并恢复。
- 崩溃/异常退出后 `CleanupResidual` 能恢复网络。
- `go test ./...` 通过，`go build ./...` 成功。
- 不在当前工作主机启用真实 TUN。

## 风险与缓解

| 风险 | 缓解 |
|---|---|
| 取消代理服务器 exclusion route 后某 Dialer 漏绑导致环路 | 每个 Dialer 改造后必须编译通过 + 代码审查；LAN exclusion 保留保护本地网络 |
| 改系统 DNS 后网络断掉 | 外部 watchdog 进程监控父进程退出并清理；`CleanupResidual` 启动兜底 |
| Linux 发行版 DNS 管理方式不同 | 按 `systemd-resolved` > `NetworkManager` > `/etc/resolv.conf` 优先级检测 |
| macOS `ifconfig` / Linux `resolv.conf` 需要 root | `Engine.Start` 已调用 `EnsureAdminPrivileges` |
| 路由查询频繁影响性能 | 30s TTL 路由查询缓存 |
