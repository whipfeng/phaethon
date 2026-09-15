# Mesh 网络改进设计

> 版本: v0.5.0
> 日期: 2026-09-15
> 状态: IMPLEMENTED
> 负责人: Phaethon Dev
> 依赖: [mesh_multi_vip_design.md](mesh_multi_vip_design.md) v0.4.1

## 变更记录

| 版本 | 日期 | 变更内容 | 作者 |
|------|------|----------|------|
| v0.1.0 | 2026-09-15 | 初始版本：整合已完成的 mesh 改进（src IP 重写、P2P 协议版本、Auto P2P、地址空间扩大等） | Qoder |
| v0.2.0 | 2026-09-15 | 新增 Mode B Mesh 路由设计（待实现） | Qoder |
| v0.3.0 | 2026-09-15 | MeshDial 不使用 DirectDialer，直接调用全局 netstack 函数；新增 ADR-4 | Qoder |
| v0.4.0 | 2026-09-15 | 清理 DirectDialer 中的 netstack 路径（已无引用，避免误导） | Qoder |
| v0.5.0 | 2026-09-15 | Mode B mesh 路由实现完成：MeshDial、server handler 统一、DNS hijacker 清理 | Qoder |

## 1. 背景与目标

### 1.1 当前问题

Mesh 网络在多 VIP 设计（v0.4.1）基础上，存在以下问题需要解决：

1. **入站数据包 src IP 错误**：跨节点 DNS 响应到达 Windows 后被丢弃
2. **P2P 协议版本不兼容**：旧版本客户端连接后无校验
3. **P2P 连接需要手动启用**：mesh 启用时应自动建立 P2P 连接
4. **TCP 跨节点连接失败**：子网重叠 + localNodeDomain 被规则覆盖
5. **地址空间不足**：硬编码 /16 网络，每节点 /24 只有 252 个 Fake-IP
6. **Mode B 流量未走 mesh**：代理入口流量仍通过 proxy chain 拨号

### 1.2 目标

1. 修复跨节点 DNS 和 TCP 连通性
2. 自动化 P2P 连接管理
3. 扩大地址空间至可配置范围
4. Mode B 流量统一走 mesh 网络

## 2. 已完成的改进

### 2.1 入站数据包 src IP 重写

**问题**：跨节点 DNS 响应到达 Windows 后被丢弃，因为 src IP 是远端 GIP 而非本地 GIP。

**根因**：
- DNS 查询流程：app → localGIP(.3) → NAT(src→VIP) → tryDNSRedirect(dst→remoteGIP) → mesh
- DNS 响应流程：远端 → src=remoteGIP, dst=VIP → 本地 HandleMeshFrame
- NAT reverse 只重写 dst（VIP→hostIP），不重写 src（remoteGIP 保持不变）
- Windows DNS 客户端期望 src=localGIP，收到 src=remoteGIP 后丢弃

**修复**：
- `mesh/mesh.go` VIP 路径区分 DNS 和 TCP 响应：
  - DNS 响应（UDP port 53）：使用 `TranslateInboundWithSrc` 重写 src 为 localGIP
  - TCP 响应：使用 `TranslateInbound` 保持原始 src IP（Fake-IP）
- 新增 `tun/nat.go`：`TranslateInboundWithSrc`、`RewriteSrcIP` 方法

**验证**：跨节点 DNS 解析正常，qg.phn → 100.64.0.9 ✓

### 2.2 P2P 协议版本

**实现**：
- `p2p/p2p.go`：`const P2PProtocolVersion = 1`
- `HelloMsg` 新增 `ProtocolVersion` 字段
- `handleHello()` 校验版本，不匹配则断开

**兼容性**：旧版本无此字段，反序列化为 0，被正确拒绝。

### 2.3 Auto P2P

**问题**：P2P 连接需要手动勾选 checkbox。

**修复**：`main.go` 中 mesh 启用时，自动对所有兼容代理（socks5、trojan、h_tunnel）启动 P2P。

### 2.4 多 P2P 连接共存与负载均衡

**问题**：同一服务器的多个代理各自启动 P2P，导致驱逐循环。

**修复**：
- 移除 `handleHello` 中相同 MeshNodeID 的驱逐逻辑
- `mesh/mesh.go` 新增 `findPeers()` 方法，返回同一节点的所有 peer
- 发送时随机选择 peer，实现负载均衡

### 2.5 TCP 跨节点连接修复

**根因 1**：子网地址空间重叠
- VM 配置 `subnet: 100.0.1.0/16` 与 QG `subnet: 100.0.0.0/16` 在 /16 掩码下重叠
- 修复：VM 子网改为 `100.1.0.0/16`

**根因 2**：localNodeDomain 被代理规则覆盖
- `localNodeDomain` 检查仅在 DIRECT 分支中，proxy 分支忽略
- 修复：`tun/engine.go` 中将 `localNodeDomain` 检查提前到代理匹配之前

### 2.6 Mesh 网络地址空间扩大

**问题**：硬编码 100.64.0.0/16，每节点 /24 只有 252 个 Fake-IP。

**修复**：
- 整体 mesh 网络：可配置（默认 100.64.0.0/10）
- 每节点子网：可配置前缀长度（默认 /18，可设为 /16 获得 65536 个地址）
- 配置示例：

```yaml
mesh:
    enabled: true
    node-id: qg
    network: 100.0.0.0/8      # 整体 mesh 网络
    subnet: 100.0.0.0/16     # 本节点子网
    domain-suffixes:
        - phn
```

**代码改动**：
- `config/config.go`：`Mesh.Network` 字段 + `GetNetwork()`
- `mesh/forward.go`：`SetMeshCIDR()`/`GetMeshCIDR()`
- `mesh/state.go`：`AllocateSubnet` 支持可配置网络范围
- `mesh/mesh.go`：存储 network 和 subnetPrefixLen
- `main.go`：mesh 初始化时设置 meshCIDR、推导 subnetPrefixLen

### 2.7 DNS 调试日志

已在以下位置添加 `[DNS-DEBUG]` 日志：
- `tun/engine.go`：readLoop 中检测 GIP:53 的 DNS 查询
- `tun/engine.go`：tryDNSRedirect 中记录域名和 remoteGIP
- `tun/dns.go`：DNSHijacker 中记录查询域名和来源

## 3. Mode B Mesh 路由设计（待实现）

### 3.1 问题

当前 Mode B（代理入口）流量仍通过 proxy chain 拨号，未走 mesh 网络。

### 3.2 设计原则

1. **Mesh 始终启用**：不需要判断 `IsMeshEnabled()`，所有流量直接走 mesh
2. **不匹配规则**：Mode B 不做规则匹配，不获取 proxy，直接拨号目标
3. **统一抽象**：所有 server handler 共用同一个拨号函数 `MeshDial()`
4. **DNS 转发**：在 writeLoop 的 `tryDNSRedirect` 中拦截，不在 DNS hijacker 中

### 3.3 数据流对比

#### Mode A（TUN 入口）

```
Windows 应用
  ↓
Wintun → readLoop → InjectInbound → gVisor netstack
                                        ↓
                              TCP/UDP Forwarder
                                        ↓
                              handleConn（规则匹配）
                                        ↓
                              chainDial（proxy chain）
                                        ↓
                              目标服务器

DNS 查询路径：
  readLoop → tryDNSRedirect（重写 dst→远端GIP）→ mesh → 远端节点
                                                      ↓
  应用 ← TUN ← writeLoop ← 重写 src←本地GIP ← DNS 响应 ← DNS hijacker
```

#### Mode B（代理入口）

```
客户端 → SOCKS5/Trojan/HTTP/HTunnel server
              ↓
         MeshDial（直接拨号，不匹配规则）
              ↓
         netstack socket（DNS 解析 + TCP 连接）
              ↓
         writeLoop 拦截 outbound 包
              ↓
         ┌─ TCP：直接 mesh 路由到目标
         └─ UDP:53：tryDNSRedirect（重写 dst→远端GIP）→ mesh → 远端节点
                                                                ↓
         netstack socket ← writeLoop ← 重写 src←本地GIP ← DNS 响应 ← DNS hijacker
              ↓
         用 Fake-IP 建立 TCP 连接 → writeLoop → mesh 路由
```

### 3.4 DNS 转发设计

**关键**：DNS 转发不在 DNS hijacker 中实现，而是在 writeLoop 的 `tryDNSRedirect` 中处理。

#### 去程（outbound）

```
writeLoop 拦截 DNS 查询包（dst=本地GIP:53）
  ↓
tryDNSRedirect：
  1. 解析域名
  2. 调用 meshGatewayResolver(domain) 获取远端 GIP
  3. 重写 dst IP：本地GIP → 远端GIP
  4. 通过 meshInterceptor 发送到远端节点
```

#### 远端节点处理

```
收到 DNS 查询（dst=自己的GIP:53）
  ↓
DNS hijacker 从本地 Fake-IP 池分配地址
  ↓
返回 DNS 响应（src=远端GIP, dst=查询方）
```

#### 回程（inbound）

```
响应包到达本地（src=远端GIP, dst=本地VIP/GIP）
  ↓
mesh.HandleMeshFrame → NAT reverse：
  - DNS 响应（UDP:53）：重写 src 为本地GIP
  - TCP 响应：保持原始 src（Fake-IP）
  ↓
writeLoop 投递给 netstack socket
  ↓
原始查询方收到响应（src=本地GIP，符合预期）
```

### 3.5 MeshDial 的实现

`MeshDial()` 直接使用全局 netstack 函数，不经过 `DirectDialer`（避免与规则匹配中的"DIRECT 直连"混淆）：

```go
// MeshDial dials destination through mesh network.
// Always uses netstack path: DNS resolution → Fake-IP → mesh routing.
func MeshDial(dstAddr string, dstPort int) (net.Conn, error) {
    if GlobalNetstackDialFunc == nil || GlobalDNSResolverFunc == nil {
        return nil, fmt.Errorf("mesh not initialized")
    }

    var targetAddr string
    if ip := net.ParseIP(dstAddr); ip == nil {
        // 域名：通过 netstack DNS 解析获取 Fake-IP
        fakeIP, err := GlobalDNSResolverFunc(dstAddr)
        if err != nil {
            return nil, fmt.Errorf("mesh dns resolve %s: %w", dstAddr, err)
        }
        targetAddr = net.JoinHostPort(fakeIP.String(), strconv.Itoa(dstPort))
    } else {
        // IP：直接使用
        targetAddr = net.JoinHostPort(dstAddr, strconv.Itoa(dstPort))
    }

    // 通过 netstack 拨号，经过 writeLoop → mesh 路由
    conn, err := GlobalNetstackDialFunc("tcp", targetAddr)
    if err != nil {
        return nil, err
    }
    util.SetTCPNoDelay(conn)
    return conn, nil
}
```

**两种情况**：

1. **域名**：
   - 调用 `GlobalDNSResolverFunc(domain)` → `engine.ResolveDomain`
   - `ResolveDomain` 通过 netstack 发送 DNS 查询到 dnsAddr:53
   - DNS 查询经过 writeLoop → `tryDNSRedirect` 拦截 → mesh 转发到远端
   - 远端 DNS hijacker 响应 → 回程重写 src → 返回 Fake-IP
   - 用 Fake-IP 通过 `GlobalNetstackDialFunc` 建立 TCP 连接

2. **IP 地址**：
   - 直接通过 `GlobalNetstackDialFunc("tcp", ip:port)` 拨号
   - TCP 连接经过 writeLoop → mesh 路由到目标节点

### 3.6 代码调整

#### 需要回退的错误实现（commit 27d9b41）

1. **dialer/bind.go**：
   - 删除 `IsMeshEnabled()` 函数
   - 删除 `ModeBMeshDial()` 函数

2. **tun/dns.go**：
   - 删除 `meshGatewayResolver` 字段
   - 删除 `SetMeshGatewayResolver()` 方法
   - 删除 `forwardDNSQuery()` 方法
   - 删除 serveLoop 中的 mesh gateway 检查逻辑
   - 删除 `time` import（如果只被 forwardDNSQuery 使用）

3. **main_tun.go**：
   - 删除 DNS hijacker 的 `SetMeshGatewayResolver` 调用和相关日志

4. **server/*.go**（6 个文件）：
   - 删除所有 `if dialer.IsMeshEnabled()` 判断
   - 删除所有 `dialer.ModeBMeshDial()` 调用
   - 删除所有规则匹配逻辑（`RuleConf.Match`）

#### 正确实现

1. **dialer/bind.go** - 新增 `MeshDial()`：
   - 直接使用 `GlobalDNSResolverFunc` 和 `GlobalNetstackDialFunc`
   - 不经过 `DirectDialer`（详见 3.5 节完整实现）

2. **server handler 统一改为**（6 个文件）：

```go
// 删除：规则匹配、proxy 变量、IsMeshEnabled 判断
// 直接：
targetConn, err := dialer.MeshDial(dstAddr, dstPort)
```

3. **main_tun.go** - 保持不变：
   - `dialer.GlobalNetstackDialFunc = engine.NetDial` ✓
   - `dialer.GlobalDNSResolverFunc = engine.ResolveDomain` ✓

### 3.7 实现步骤

1. **实现 MeshDial 函数**（dialer/bind.go）
   - 删除 `IsMeshEnabled()`、`ModeBMeshDial()`
   - 新增 `MeshDial(dstAddr, dstPort)` 函数
   - 直接使用 `GlobalDNSResolverFunc` 和 `GlobalNetstackDialFunc`（详见 3.5 节）

2. **清理 DirectDialer**（dialer/direct.go）
   - 删除 `Dial()` 中的 netstack 路径（`GlobalNetstackDialFunc`/`GlobalDNSResolverFunc` 分支）
   - 只保留 OS socket 路径（`DialRouteAware`）

3. **回退 DNS hijacker 错误实现**（tun/dns.go, main_tun.go）
   - 删除 `meshGatewayResolver` 相关代码
   - DNS hijacker 只负责从本地池分配 Fake-IP

4. **修改所有 server handler**
   - socks5.go、trojan.go、http.go、htunnel.go、direct.go、reverse.go
   - 删除规则匹配和 proxy chain 逻辑
   - 统一调用 `dialer.MeshDial()`

5. **验证 tryDNSRedirect 覆盖 Mode B**
   - writeLoop 已有 `tryDNSRedirect` 调用
   - 确认 Mode B 的 DNS 查询经过 writeLoop 时被正确拦截

## 4. 关键设计决策

### 4.1 ADR-1: Mode B 不做规则匹配

**决策**：Mode B（代理入口）流量不做规则匹配，直接通过 mesh 路由。

**理由**：
- Mode B 的流量已经进入 mesh 网络，目标是到达 mesh 内的其他节点
- 规则匹配用于决定流量是否走代理，但 Mode B 本身就是代理入口
- 简化实现，避免不必要的复杂性

### 4.2 ADR-2: DNS 转发在 writeLoop 而非 DNS hijacker

**决策**：跨节点 DNS 转发在 writeLoop 的 `tryDNSRedirect` 中实现，不在 DNS hijacker 中。

**理由**：
- writeLoop 是所有 netstack 出站包的统一拦截点
- `tryDNSRedirect` 已经实现了域名解析 + GIP 重写 + mesh 转发的完整流程
- 在 DNS hijacker 中转发需要额外的 endpoint 创建和超时管理，复杂且容易出错

### 4.3 ADR-3: MeshDial 统一抽象

**决策**：所有 server handler 共用 `MeshDial()` 函数，不各自实现拨号逻辑。

**理由**：
- 避免代码重复（6 个 handler 文件）
- 统一行为：无规则匹配、无 proxy chain、直接 mesh 路由
- 便于维护和修改

### 4.4 ADR-4: MeshDial 不使用 DirectDialer，DirectDialer 清理 netstack 路径

**决策**：
1. `MeshDial()` 直接调用 `GlobalDNSResolverFunc` 和 `GlobalNetstackDialFunc`，不经过 `DirectDialer`
2. `DirectDialer` 中的 netstack 路径（`GlobalNetstackDialFunc`/`GlobalDNSResolverFunc` 分支）删除，只保留 OS socket 路径

**理由**：
- `DirectDialer` 是 DIRECT 代理规则专用的拨号器，走 OS socket 直连目标
- netstack 路径是之前为 Mode B 加的，现在 `MeshDial` 直接调用全局函数，不再经过 `DirectDialer`
- 留着 netstack 路径会误导开发者以为 `DirectDialer` 有两条路径，实际上 DIRECT 规则只需要 OS socket
- 清理后 `DirectDialer.Dial()` 简化为只调用 `DialRouteAware`

## 5. 变更文件清单

| 文件 | 变更 | 状态 |
|------|------|------|
| `mesh/mesh.go` | VIP 路径 DNS/TCP 区分 + src IP 重写 + 可配置 network + findPeers | ✓ 已完成 |
| `mesh/forward.go` | meshCIDR 可配置（SetMeshCIDR/GetMeshCIDR） | ✓ 已完成 |
| `mesh/state.go` | AllocateSubnet 支持可配置网络范围和子网前缀 | ✓ 已完成 |
| `tun/nat.go` | TranslateInboundWithSrc、RewriteSrcIP 方法 | ✓ 已完成 |
| `p2p/p2p.go` | P2P 协议版本校验 + 多连接共存 | ✓ 已完成 |
| `dialer/bind.go` | GetLocalIPForDial + Auto P2P 支持 | ✓ 已完成 |
| `tun/engine.go` | localNodeDomain 提前到代理匹配之前 | ✓ 已完成 |
| `config/config.go` | Mesh.Network 字段 + GetNetwork() | ✓ 已完成 |
| `main.go` | mesh 初始化 + Auto P2P | ✓ 已完成 |
| `dialer/bind.go` | 删除 IsMeshEnabled/ModeBMeshDial，新增 MeshDial | ✓ 已完成 |
| `dialer/direct.go` | 删除 netstack 路径（GlobalNetstackDialFunc/GlobalDNSResolverFunc 分支），只保留 OS socket | ✓ 已完成 |
| `tun/dns.go` | 删除 meshGatewayResolver 相关代码 | ✓ 已完成 |
| `main_tun.go` | 删除 DNS hijacker SetMeshGatewayResolver 调用 | ✓ 已完成 |
| `server/socks5.go` | 删除规则匹配，改用 MeshDial | ✓ 已完成 |
| `server/trojan.go` | 同上 | ✓ 已完成 |
| `server/http.go` | 同上 | ✓ 已完成 |
| `server/htunnel.go` | 同上 | ✓ 已完成 |
| `server/direct.go` | 同上 | ✓ 已完成 |
| `server/reverse.go` | 同上 | ✓ 已完成 |

## 6. 风险与回退

| 风险 | 影响 | 缓解 |
|------|------|------|
| Mode B 删除规则匹配后无法回退 | 高 | Git 保留历史，可随时 revert |
| DNS 转发依赖 writeLoop tryDNSRedirect | 中 | 已有 Mode A 验证，逻辑相同 |
| 多 P2P 连接增加资源消耗 | 低 | 负载均衡提升可靠性，可接受 |

## 7. 验收标准

### 已完成项

- [x] 跨节点 DNS 解析正常（qg.phn → Fake-IP）
- [x] 跨节点 TCP 连接正常（SSH qg.phn:2222）
- [x] P2P 协议版本校验生效
- [x] Mesh 启用时自动建立 P2P 连接
- [x] 多 P2P 连接共存无驱逐循环
- [x] 地址空间可配置（/8 网络，/16 子网）

### 待验证项

- [ ] Mode B 流量通过 mesh 路由（需部署后验证）
- [ ] 从 QG SOCKS5 代理访问 VM 服务正常
- [ ] DNS 查询通过 writeLoop tryDNSRedirect 转发
- [x] 无 IsMeshEnabled/ModeBMeshDial 残留代码
