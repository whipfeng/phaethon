# tun_spec.md

## 元数据

- 文档类型：Spec
- 版本：v0.4.0
- 所属项目：phaethon
- 创建日期：2026-07-14

## 变更记录

| 版本 | 日期 | 变更内容 | 作者 |
|------|------|----------|------|
| v0.1.0 | 2026-07-14 | 初始版本：TUN 模式规格 | Claude |
| v0.2.0 | 2026-08-26 | 补充 DIRECT 路由选择、DNS 缓存与解析路由规则 | Claude |
| v0.3.0 | 2026-08-26 | 补充主流实现对比与 dialer BindContext 兼容性矩阵；明确 VLESS/Hysteria2 通过库钩子统一接入 | Claude |
| v0.4.0 | 2026-08-26 | VLESS 接入方式更新为自研 Dialer + DialRouteAware；移除 xray-core / proxyclient 依赖描述 | Claude |

## 1. 概述

TUN 模式通过系统 TUN 接口 + gvisor 用户态网络栈 + Fake-IP 实现系统级流量拦截，将系统全部流量路由到 phaethon 处理。

## 2. 架构

```
┌─────────────┐     ┌──────────────┐     ┌─────────────┐
│  系统应用    │────→│  TUN 接口    │────→│ gvisor 栈   │
└─────────────┘     └──────────────┘     └──────┬──────┘
                                                │
                       ┌────────────────────────┘
                       ↓
              ┌─────────────────┐
              │ DNS Hijacker    │  53 端口 DNS 拦截
              │ TCP Forwarder   │  连接 → RuleConf.Match → ChainDial
              │ UDP Forwarder   │  UDP 包 → NAT → ChainDial
              └─────────────────┘
```

## 3. 核心组件

### 3.1 TUN 接口

- 创建系统 TUN 设备，需要管理员/root 权限。
- 配置路由和 DNS，使系统流量进入 TUN。
- Windows 依赖 `wintun.dll`。

### 3.2 gvisor 网络栈

- 在 `tun/engine.go` 中初始化 netstack。
- 设置 TCP 传输处理器。
- UDP 传输处理器（待完善或已实现，视版本而定）。

### 3.3 Fake-IP

- DNS 查询返回 Fake-IP（私有地址段）。
- 应用连接 Fake-IP 时，gvisor 栈根据 Fake-IP 映射回真实域名/地址。
- 避免 DNS 泄漏，支持域名规则匹配。

### 3.4 DNS Hijacker

- 拦截 53 端口 DNS 请求。
- 根据规则返回 Fake-IP 或转发到真实 DNS。

## 4. 配置

```yaml
tun:
  enabled: true
  name: "phaethon-tun"
  mtu: 1500
  address: 198.18.0.1/16
  dns-hijack: true
```

## 5. Admin API

| 方法 | 路径 | 说明 |
|------|------|------|
| GET | `/api/tun` | 获取 TUN 状态（启用/禁用、接口名、错误信息） |
| POST | `/api/tun` | 启用或禁用 TUN |

请求体示例：

```json
{ "enabled": true }
```

## 6. DIRECT 流量路由规则

TUN 启用后会通过 split-tunnel 路由（`0.0.0.0/1`、`128.0.0.0/1`）把大部分流量引入 gvisor 栈。DIRECT 流量再发回真实网络时，必须避免重新进入 TUN 造成环路。

### 6.1 路由表优先原则

- 对代理服务器 IP、`IP-CIDR,DIRECT` 规则目标、LAN/私有网段，启动时直接添加更精确的 exclusion route 走原物理接口，使其不进入 TUN。
- 对动态 `MATCH,DIRECT` 目标，不能预先加路由，需要在 TUN 内部按目的地址查询系统路由表，选择正确出口接口。

### 6.2 接口选择机制（BindContext）

所有从 TUN 内部发出的出站连接（DIRECT 规则、DIRECT 域名 DNS 查询、代理链首跳），在创建 socket 时必须通过 `dialer.BindContext` 绑定到正确的真实网卡，避免流量回灌 TUN。

`BindContext` 提供两类绑定语义：

- `BindSocket(c syscall.RawConn, dst net.IP)`：按目的地址查询系统路由表，绑定到查得的出口接口。用于 DIRECT 流量和已知目的地址的代理首跳。
- `BindDefaultInterface(c syscall.RawConn)`：直接绑定到启动时捕获的默认物理接口。用于无法从调用点获得目的地址的外部库封装 Dialer（如 xray-core UDP）。

#### 6.2.1 Windows

- 使用 `GetBestRoute2` 查询目的地址对应的最佳路由，查询时排除 phaethon TUN 接口（通过 `tunLUID`）。
- 如果最佳路由指向 TUN 接口，回退到启动时捕获的原始默认接口。
- 使用 `IP_UNICAST_IF`（`IPPROTO_IP` 选项 31）将 socket 绑定到选中的接口索引。
- IPv6 使用 `IPV6_UNICAST_IF`。
- 接口索引必须按平台要求处理字节序。

#### 6.2.2 Linux

- 使用 `SO_BINDTODEVICE` 绑定到出口接口。
- DIRECT 流量按目的地址查询路由表（netlink `RouteGet` 或 `/proc/net/route`）。
- 代理首跳与无法获得目的地址的 UDP socket 绑定默认物理接口。

#### 6.2.3 macOS

- 使用 `SO_IP_BOUND_IF` 绑定到出口接口索引。
- 与 Linux 相同，DIRECT 按目的地址查询路由表，代理首跳绑定默认接口。

#### 6.2.4 禁止行为

禁止写死启动时捕获的单一默认接口作为所有流量的唯一出口，否则 VPN 的 split-tunnel 精确路由会被忽略。

### 6.3 路由查询缓存

为降低重复查询开销，对 (目的 IP, 协议) → 出口接口索引 做短期缓存（TTL 建议 30s）。缓存失效或 miss 时回退到路由表查询。

## 7. 代理服务器出网规则

### 7.1 当前实现

TUN 启动时通过 `resolveProxyIPs()` 解析所有代理服务器 IP，并添加 exclusion route 使其走原物理接口。代理链内部各协议 Dialer 通过 `net.Dial` 等标准接口连接服务器，依赖系统路由。

### 7.2 建议实现

代理服务器首跳连接应当复用 DIRECT 流量的路由感知绑定机制（`BindContext`），原因：

- 静态 exclusion route 无法应对代理服务器域名解析结果变化、网络切换、VPN 多网卡等场景。
- 代理首跳目标地址已知，完全可以在 socket 创建时按目的地址查询路由表并绑定真实网卡。
- 对于外部库封装的 Dialer（如 xray-core、quic-go），虽然无法从调用点传入目的地址，但可以通过全局 controller / 自定义 conn factory 统一绑定默认物理接口，同样避免进入 TUN。
- 复用同一套 `BindContext` 机制后，可减少启动阶段对系统路由表的修改，降低路由残留和环路风险。

### 7.3 下沉到 dialer 包

路由感知绑定能力应从 `tun` 包下沉到 `dialer` 包：

- `dialer` 包维护一个可注入的 `BindContext`（含默认接口名/索引、TUN LUID、默认网关等）。
- TUN 引擎在启动时调用 `dialer.SetGlobalBindContext(bc)`，停止时清理。
- `DirectDialer` 和各协议 Dialer 的首跳连接统一通过 `dialer.DialRouteAware()` 或等效绑定函数创建，自动应用绑定。
- 外部库封装 Dialer（`VLESSDialer`、`Hysteria2Dialer`）通过 xray-core 全局 `RegisterDialerController` / Hysteria2 `ConnFactory` 接入同一 `BindContext`，不再依赖静态 exclusion route。
- `tun` 包不再直接处理 socket 绑定细节，只负责提供网络上下文。

## 7. DNS 解析规则

### 7.1 Fake-IP 阶段

DNS Hijacker 对应用查询返回 Fake-IP，真实域名记录到 Fake-IP 映射表。

### 7.2 DIRECT 域名解析

规则命中 DIRECT 的域名，需要在 TUN 内部解析真实 IP。解析行为：

1. 先查 TUN 内部 DNS 缓存；命中则直接复用真实 IP。
2. 缓存未命中时，使用路由表感知的 Resolver 发起真实 DNS 查询（查询本身也要按 DNS 服务器地址查路由表选择接口，不能写死默认接口）。
3. 解析结果写入缓存并返回。

DNS 查询 socket 同样应通过 `dialer.DialRouteAware()` 创建，使其复用路由感知绑定。

### 7.3 DNS 缓存策略

- 缓存键：域名 + 查询类型（A/AAAA）。
- TTL：取 DNS 响应中的 TTL，但不超过上限（例如 300s）。
- 失败缓存：NXDOMAIN / 超时结果也缓存较短时间（例如 5s），避免反复查询失效域名。

## 8. 主流实现与 dialer 兼容性

### 8.1 主流代理工具做法

开启 TUN 后，虚拟网卡通常成为系统默认路由，所有 IP 层流量被吸入 TUN。主流工具的防环路策略通常是**系统路由排除 + 套接字级接口绑定**两者叠加：

| 工具 | DIRECT / 代理首跳处理方式 | socket 绑定平台选项 |
|---|---|---|
| mihomo (Clash.Meta) | `auto-detect-interface` + 直连 outbound 绑定物理接口 | Windows `IP_UNICAST_IF` / Linux `SO_BINDTODEVICE` |
| Clash Premium | 主要依靠 `auto-route` + 系统路由/规则排除 | 公开文档未明确 socket 级绑定 |
| sing-box | outbound `bind_interface` + route `auto_detect_interface` / `default_interface` + `strict_route` | Linux `SO_BINDTODEVICE`、macOS `IP_BOUND_IF` |
| Xray / v2ray | TUN inbound `autoOutboundsInterface` 或 outbound `sockopt.interface` / `sendThrough` | Linux `SO_BINDTODEVICE`、Windows `IP_UNICAST_IF` |
| Surge | `tun-excluded-routes` + `direct` 策略 `interface=` | macOS/iOS `IP_BOUND_IF` |

关键共性：
- 路由排除用于把“不该进 TUN”的流量挡在外面。
- socket 绑定用于把代理核心自己的出站连接（代理首跳）钉死在物理网卡，防止环路。
- 多宿主 / VPN 共存 / 不同用户态协议栈（gvisor vs system）是最常见踩坑点。

### 8.2 phaethon 各 Dialer 对 BindContext 的支持

`BindContext` / `DialRouteAware` 的目标是把 TUN 内部的路由感知 socket 绑定能力下沉到 `dialer` 包，使所有出站连接复用同一机制。对于外部库封装的 Dialer，通过库暴露的 controller / factory 钩子接入，不再保留 exclusion route 兜底。

| Dialer | TCP 首跳 | UDP/Packet | 支持程度 | 接入方式 |
|---|---|---|---|---|
| `DirectDialer` | `DialRouteAware` | UDP socket 绑定物理网卡 | ✅ 完全支持 | 所有 `nil`/`DIRECT` next-hop 的归宿，按目的地址绑定 |
| `HTTPDialer` | `DialRouteAware` | - | ✅ 完全支持 | 替换裸 `net.DialTimeout` |
| `ShadowsocksDialer` | `DialRouteAware` | `ListenUDP` 绑定 | ✅ 完全支持 | TCP/UDP 均按目的地址绑定 |
| `HTunnelDialer` | `http.Transport.DialContext` 注入 `DialRouteAware` | - | ✅ 完全支持 | transport 的 dial context 替换 |
| `Socks5Dialer` | 经 `nextDialer.Dial` | UDP ASSOCIATE 控制面 + relay UDP socket | ⚠️ 部分支持 | TCP 首跳在 `next == nil` 时由 `DirectDialer` 自动改善；UDP 控制面/数据面需改造 |
| `TrojanDialer` | 经 `nextDialer.Dial` | 同上 | ⚠️ 部分支持 | TCP 间接受益于 `DirectDialer`；UDP 控制面/数据面需改造 |
| `SSHDialer` | 经 `nextDialer.Dial` | 无独立 UDP | ⚠️ 部分支持 | TCP 间接受益于 `DirectDialer` |
| `ReverseDialer` | `registry.Match` 拿到已有反向连接 | `targetConn` / `chainConn` 可绑定 | ⚠️ 部分支持 | TCP 是 registry 已有连接，不在 BindContext 范围；UDP 按目的地址绑定 |
| `ControlClient` | 经 `ChainDial` 走代理链 | - | ⚠️ 部分支持 | 控制面间接受益于链首 Dialer 改造 |
| `Hysteria2Dialer` | quic-go 复用 `ConnFactory` 创建的 UDP socket | 自定义 `ConnFactory.New` | ✅ 可支持 | `ConnFactory.New(serverAddr)` 中创建 UDP socket 并按目的地址绑定 |
| `VLESSDialer` | 自研 Dialer 使用 `DialRouteAware` 建立到 VLESS 服务器的 TCP/TLS 首跳 | 同 TCP | ✅ 完全支持 | 不再依赖 xray-core / `cnlangzi/proxyclient`；TLS fingerprint 通过 `refraction-networking/utls` 实现；REALITY 暂时未实现 |

### 8.3 实施建议

1. **纯自研 Dialer 直接接入**：`DirectDialer`、`HTTPDialer`、`ShadowsocksDialer`、`HTunnelDialer` 全部使用 `DialRouteAware` / `BindSocket`。
2. **链式 Dialer 间接受益**：只要 `DirectDialer` 接入，`SOCKS5` / `Trojan` / `SSH` 在 `next == nil` 或 DIRECT 时的 TCP 首跳自动获得路由感知能力。
3. **外部库 Dialer 统一通过全局钩子接入**：
   - `Hysteria2Dialer` 自定义 `ConnFactory`，在创建 UDP socket 时调用 `BindSocket(serverAddr)`。
   - `VLESSDialer` 自研 VLESS 客户端实现，直接通过 `DialRouteAware` 建立到服务器的 TCP/TLS 首跳，不再依赖 xray-core 的 `RegisterDialerController`。
4. **无兜底分支**：所有 Dialer 都接入 `BindContext`，不再为任何代理类型保留静态 `exclusion route`。TUN 启动时只需为 LAN/私有网段和已解析的代理服务器 IP 添加 exclusion route 作为辅助保护，代理首跳自身已具备路由感知绑定。
5. **UDP 统一改造 `ListenUDP`**：当前 `dialer.ListenUDP()` 不带绑定参数，需要新增路由感知版本供 UDP relay / ASSOCIATE 使用。

## 9. 限制与注意事项

- 需要管理员/root 权限。
- Windows 需要 `wintun.dll`。
- TUN 启用后会修改系统路由/DNS，请谨慎操作。
- 当前 UDP 转发能力取决于实现版本。
- Linux DIRECT 路由感知尚未实现按目的地址动态查询，当前固定绑定默认接口。

## 10. 相关链接

- [admin_spec.md](admin_spec.md)
- [protocol_spec.md](protocol_spec.md)
- [tun_design.md](../plans/tun_design.md)
