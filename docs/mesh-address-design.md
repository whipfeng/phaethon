# Mesh 地址设计与包处理

## 地址角色

每个 mesh 节点在其子网（如 100.64.x.0/24）内有三个特殊地址：

| 地址 | 名称 | 用途 |
|------|------|------|
| .1 | VIP | NAT 转换地址，回程识别用 |
| .2 | hostIP | TUN 接口地址，投递到本地 OS |
| .3 | GIP | DNS 服务地址，投递到 gVisor netstack |

### VIP (.1)
- **仅用于 NAT 转换和回程识别**
- **不配置给 OS**：.1 不是任何网络接口的地址，不应通过 AddMeshVIPToOS 添加到 TUN 设备
- **出站 NAT**：当源地址不是 mesh 地址（如旁路网关模式下的 LAN 客户端），NAT 将源地址转换为 VIP
- **回程识别**：回程包 dst=VIP 时，进行 NAT 反转，还原原始源地址后投递给 OS
- **不直接投递**：VIP 不会直接投递给应用，只用于 NAT 转换

**历史问题**：之前 ping .1 能成功是因为错误地将 .1 添加到了 OS TUN 设备（AddMeshVIPToOS），导致 OS 认为 .1 是本机地址并响应 ICMP。正确实现中，.1 不应配置给 OS，ping .1 不应该成功。

### hostIP (.2)
- **TUN 接口地址**：OS 侧的 TUN 接口 IP
- **本地投递**：dst=.2 的包通过 WriteMeshPacket 写到 TUN 设备，OS 内核投递给应用
- **DNS 查询源地址**：应用发 DNS 查询时，内核根据路由表选择源地址为 .2

### GIP (.3)
- **DNS 服务地址**：DNS hijacker 绑定在此地址
- **netstack 投递**：dst=.3 的包通过 InjectMeshPacket 注入 gVisor netstack，由 DNS hijacker 处理
- **系统 DNS 目标**：OS 的系统 DNS 被重定向到 GIP

## 出站包处理（HandleOutboundPacket / writeLoop）

```
包到达 mesh interceptor
  ↓
src 是 mesh 地址？
  ├─ 是 → 不做 NAT，原样发送
  └─ 否 → NAT 转换 src 为 VIP
  ↓
通过 P2P 发送 FrameMeshPacket
```

## 入站包处理（HandleMeshFrame）

HandleMeshFrame 必须根据 dst 地址精确区分处理路径，不能用 `isLocalNetstackAddr` 统一判断，因为 .2 和 .3 的投递方式不同。

```
收到 mesh frame
  ↓
检查 dst 地址：
  │
  ├─ dst == .1 (VIP)
  │   │
  │   │  NAT 反转：查找 reverse mapping，还原原始 dst
  │   │  例：dst=100.64.1.1 → 还原为 100.64.1.2（原始源地址）
  │   │
  │   └─ WriteMeshPacket → TUN → OS 投递
  │
  ├─ dst == .2 (hostIP)
  │   └─ WriteMeshPacket → TUN → OS 投递
  │
  ├─ dst == .3 (GIP)
  │   └─ InjectMeshPacket → gVisor netstack → DNS hijacker
  │
  ├─ dst 在本地子网（如 fakeIP .4+）
  │   └─ InjectMeshPacket → gVisor netstack → TCP forwarder
  │
  └─ dst 非本地地址
      ├─ 有 peer 路由 → 转发（TTL-1）
      └─ 无 peer 路由 → InjectMeshPacket（本地 gateway 处理）
```

### 地址判断逻辑

```go
// HandleMeshFrame 中的地址判断
switch {
case dst.Equal(vip):           // .1 - NAT 反转后投递
    handleVIP(packet)
case dst.Equal(hostIP):        // .2 - 直接投递 OS
    writeMeshPacket(packet)
case dst.Equal(gip):           // .3 - 投递 netstack
    injectMeshPacket(packet)
case localSubnet.Contains(dst): // 本地子网其他地址
    injectMeshPacket(packet)
default:                        // 非本地 - 转发或本地处理
    forwardOrLocal(packet)
}
```

**注意**：不能用 `isLocalNetstackAddr(dst)` 统一判断 .2 和 .3，因为：
- .2 需要 WriteMeshPacket（投递给 OS）
- .3 需要 InjectMeshPacket（投递给 netstack）
- 两者的处理路径完全不同

## DNS 解析流程（数据面迭代转发）

### 场景：VM 解析 www.feishu.cn（gateway 是 JF）

```
1. App 发 DNS 查询
   src=100.64.1.2(.2), dst=100.64.1.3(.3 GIP)
   ↓
2. TUN 捕获，注入 gVisor
   ↓
3. DNS hijacker 在 .3 收到查询
   - 检查域名匹配 mesh suffix: feishu.cn → gateway=jf
   - 修改 dst 为 gateway GIP: 100.64.2.3
   - src 保持 100.64.1.2
   - 注入 gVisor netstack
   ↓
4. writeLoop 读出，mesh interceptor 处理
   - src=100.64.1.2 是 mesh 地址 → 不做 NAT
   - 通过 P2P 发送: src=100.64.1.2, dst=100.64.2.3
   ↓
5. Mesh 路由多跳传输（VM → QG → JF）
   ↓
6. JF 的 HandleMeshFrame 收到
   - dst=100.64.2.3 是本地 GIP
   - InjectMeshPacket → gVisor netstack
   ↓
7. JF 的 DNS hijacker 在 .3 收到
   - 分配 fakeIP: 100.64.2.4
   - 构造响应: src=100.64.2.3, dst=100.64.1.2
   - 注入 gVisor netstack
   ↓
8. JF 的 writeLoop 读出，通过 mesh 发送
   ↓
9. Mesh 路由多跳传输（JF → QG → VM）
   ↓
10. VM 的 HandleMeshFrame 收到
    - dst=100.64.1.2 是本地 hostIP (.2)
    - WriteMeshPacket → TUN → OS
    ↓
11. OS 内核投递给 App（匹配 dst port）
```

### 关键点

1. **DNS 走数据面**：DNS 查询是普通 UDP 包，通过 mesh IP 路由传输，不走 P2P 控制面
2. **一级转发**：DNS hijacker 只负责改 dst 为 gateway GIP，多跳传输由 mesh 路由处理
3. **无需 pending 记录**：响应直接回到发起方（src 保持原样），hijacker 不跟踪查询
4. **UDP 源地址不检查**：DNS 客户端用非连接 socket，接受任何源 IP 的响应（只要 queryID 匹配）
5. **NAT 不介入**：src 是 mesh 地址时不做 NAT，回程也不需要 NAT 反转

## Mode B（代理入口）架构

### 关键发现：gVisor 无内部环回

经测试验证，gVisor netstack 的 `channel.Endpoint` 不支持内部环回。所有出站包（即使目标是本地 IP）都走 `linkEP.Read()` → writeLoop，不会投递给本地 forwarder 或 hijacker endpoint。

### 解决方案：为 netstack 添加 loopback NIC

在 gVisor netstack 上添加一个 loopback NIC，将本地地址的出站包路由到 loopback 而非 channel.Endpoint。包经过 loopback 回到传输层，forwarder/hijacker 自然兜底。

gVisor 自带 `loopback.New()` 实现：`WritePackets` 将出站包重新注入为入站包（`DeliverNetworkPacket`），无需自行实现。

```
initStack() 中：
  1. 创建 channel.Endpoint（现有，用于 TUN/mesh 出站）
  2. 创建 loopback endpoint（新增，gVisor 内置 loopback.New()）
  3. 创建两个 NIC：
     - NIC 1 (tunNICID): channel.Endpoint
     - NIC 2 (loNICID): loopback endpoint
  4. dnsAddr 注册在 loNICID 上（让 netstack 认为 dnsAddr 是本地地址）
  5. 路由表：
     - 整个 fakeIP/mesh 段 → NIC 2 (loopback)   ← 覆盖整个网段，含 dnsAddr
     - 默认路由 → NIC 1 (channel)
```

> 注：dnsAddr 本身在 fakeIP/mesh 段内，无需单独 /32 路由。整个网段一条路由即可。

### 数据流（统一后）

```
Mode B 收到请求 (domain:port)
  ↓
1. DirectDialer.Dial(domain, port)
   - 域名 → ResolveDomain → fakeIP
   - IP → 直接用
   - 都走 NetDial(ip:port) → netstack
  ↓
2. netstack 路由表决定去向：
   - fakeIP/mesh 段 → loNIC (loopback)
   - 其他 → tunNIC (channel)
  ↓
3. writeLoop 从 tunNIC 读出包，做最终路由：
   ├─ dst 是 remote mesh VIP → meshInterceptor → mesh 链路
   ├─ dst 是 hostIP (.2) → TUN → OS（终止于 OS）
   └─ dst 是其他（外部 IP）→ TUN → OS → 物理出口
      注：外部 IP 从 tunNIC 出去后，OS 路由决定走物理接口还是 mesh
  ↓
4. loopback 路径（fakeIP）：
   - loopback 环回 → forwarder/hijacker 拦截
   - forwarder: 查回域名 → 规则匹配 → DialRouteAware (OS socket) → 真实目标
   - 注：forwarder 用 OS socket dial，不再走 netstack，避免死循环
```

**关键设计：**
- DirectDialer 总是用 NetDial，不区分域名/IP
- writeLoop 是统一的路由决策点（mesh / hostIP / 其他）
- forwarder dial 用 OS socket（DialRouteAware），绕过 netstack，避免死循环
- TUN 的 exclusion routes 确保 OS socket 能到达物理出口

TUN 入口数据流不变（inbound 包从 TUN 进入，经 readLoop 注入 netstack，forwarder 兜底）。

### 两个入口完全统一

| | TUN 入口 | Mode B 入口 |
|---|---|---|
| 包进入方式 | readLoop → InjectInbound | gonet.DialTCP/UDP（outbound → loopback） |
| forwarder 拦截 | inbound 包直接命中 | outbound 包经 loopback 变为 inbound |
| DNS 解析 | OS 系统 DNS → TUN 劫持 → hijacker | netstack socket → loopback → hijacker |
| 规则匹配 | **共享** handleConn | **共享** handleConn |
| 连接建立 | **共享** handleConn | **共享** handleConn |

### Stack 启动条件

Hijacker 始终存在。Stack 在以下情况启动：
- TUN 启用 → StartStack() + StartTUN()
- Mesh 启用（无 TUN）→ StartStack()（Mode B 通过 loopback 使用 hijacker/forwarder）
- 都未启用 → 不启动（无 mesh 场景下 Mode B 完全使用 OS 网络）

### loopback 验证结果

测试程序 `tun/netstack_internal_test.go:TestNetstackLoopback` 验证了三个场景：

| 场景 | 结果 |
|---|---|
| TCP 出站 → loopback → forwarder 拦截 | ✅ |
| UDP 出站 → loopback → hijacker endpoint 收到 | ✅ |
| UDP 出站非本地地址 → channel → tunNIC | ✅ |

> 注：UDP loopback 投递是同步的，waiter 必须在 Write 之前注册。

### 待实现

- [x] loopback NIC 创建 + 路由配置（已实现于 engine.go initStack）
- [x] 验证 TCP/UDP loopback 投递
- [x] 验证非本地地址走 channel
- [x] DNS hijacker 绑定 NIC 0（任意网卡），接收来自 TUN 和 loopback 的 DNS 查询
- [x] Mode B (SOCKS5) 通过 netstack socket 进行 DNS 解析和连接建立
- [x] forwarder 排除 TUN 接口（BindContext + DialRouteAware）
- [ ] DirectDialer 总是用 NetDial（域名和 IP 都走 netstack）
- [ ] **待解决**：netstack 出站包区分新请求 vs 回程流量

### 待解决问题：出站包路由歧义

**问题：**
Netstack 出站包（从 tunNIC 出来）包括：
1. **新请求**（Mode B 发起的连接）→ 应该环回到 forwarder
2. **回程包**（forwarder dial 出去后，OS 返回的回复）→ 应该去 TUN → OS

如果统一环回，回程包也会被环回到 forwarder，破坏连接。

**示例：**
```
Forwarder dial 8.8.8.8 (OS socket) → OS → 物理出口
8.8.8.8 回复 → OS → TUN → readLoop → netstack
Netstack 处理回复 → writeLoop (outbound from netstack perspective)
  → 如果环回 → forwarder (错误！应该去 TUN → OS)
```

**可能的解决方案（待讨论）：**
1. 只环回 SYN / 首次 UDP（需要检查包内容）
2. 用不同地址段区分（fakeIP 环回，real IP 去 TUN）
3. 在 forwarder dial 时标记连接，writeLoop 根据标记决定
4. 其他？

**当前实现：**
- fakeIP/mesh → loNIC (loopback)
- 其他 → tunNIC (writeLoop → TUN)
- DirectDialer 对域名用 NetDial，对 IP 用 OS socket（避免回程问题）

## 已完成的待实现

- [x] 删除 AddMeshVIPToOS（.1 不应配置给 OS）
- [x] DNS hijacker 支持 mesh 域名转发（改 dst 为 gateway GIP，注入 netstack）
- [x] HandleMeshFrame 区分 .1/.2/.3 的处理逻辑
- [x] 废弃 P2P 控制面 DNS 协议（SendMeshDNSQuery）
