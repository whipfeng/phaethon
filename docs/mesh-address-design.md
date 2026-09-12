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

在 gVisor netstack 上添加一个 loopback NIC，将出站包路由到 loopback 而非 channel.Endpoint。包经过 loopback 回到传输层，forwarder/hijacker 自然兜底。

gVisor 自带 `loopback.New()` 实现：`WritePackets` 将出站包重新注入为入站包（`DeliverNetworkPacket`），无需自行实现。

### 两个 NIC 的职责

| NIC | 常量 | 链路层 | 职责 |
|---|---|---|---|
| 出站 NIC | `outNICID = 1` | channel.Endpoint | 出站统一分发点：writeLoop 从此读取，分发到 mesh 链路或 TUN 设备 |
| 环回 NIC | `loNICID = 2` | loopback.New() | 出站包环回为入站，forwarder/hijacker 兜底 |

> `outNICID` 不与 TUN 绑定。TUN 未启用时，outNIC 仍用于 mesh 拦截和 VIP 回程分发。

### 路由表（目标设计）

```
路由表：
  1. VIP（mesh 本机 VIP）→ outNIC    ← TUN 回程流量（NAT 后 src=VIP，回程 dst=VIP）
  2. meshSubnet → outNIC             ← writeLoop 拦截 → meshInterceptor → mesh 链路
  3. 默认 → loNIC (loopback)         ← 所有新请求出站 → forwarder/hijacker
```

### 回程处理

两种入口的 forwarder 都通过 gonet.Conn.Write() 写回数据。回程包的目标地址就是出站时的源地址：

- **TUN 入口**：NAT 把 src 替换为 VIP → 回程 dst=VIP → outNIC → TUN → OS → 客户端
- **Mode B 入口**：gVisor netstack socket 源地址为 GIP → 回程 dst=GIP → 精确匹配客户端 socket → 直接投递

```
TUN 入口:
  客户端真实IP → NAT src=VIP → forwarder dial (OS socket)
  回程: dst=VIP → outNIC → TUN → OS → 客户端

Mode B 入口:
  DirectDialer → gVisor netstack socket (src=GIP) → loopback → forwarder dial (OS socket)
  回程: dst=GIP → 精确匹配客户端 gVisor netstack socket → 直接投递
```

NAT 的作用（TUN 入口）：将客户端真实 IP 替换为 VIP，使回程目标 = VIP，路由表走 outNIC → TUN。
Mode B 不需要额外 NAT：gVisor netstack socket 源地址本身就是 GIP，回程通过精确匹配投递。

### 数据流（统一后）

```
出站包路由决策（路由表）：
  ├─ dst = VIP → outNIC → writeLoop → TUN → OS（TUN 回程）
  ├─ dst = mesh 远端 VIP → outNIC → writeLoop → meshInterceptor → mesh 链路
  └─ dst = 其他 → loNIC → loopback → forwarder/hijacker

writeLoop 逻辑不变：
  1. 从 outNIC 的 channel.Endpoint 读包
  2. mesh 拦截（dst 是远端 mesh VIP → meshInterceptor）
  3. 写 TUN 设备（VIP 回程 + 其他）
```

### 两个入口完全统一

| | TUN 入口 | Mode B 入口 |
|---|---|---|
| 包进入方式 | readLoop → InjectInbound | gonet.DialTCP/UDP（outbound → loopback） |
| forwarder 拦截 | inbound 包直接命中 | outbound 包经 loopback 变为 inbound |
| 出站源地址 | VIP（NAT 替换） | GIP（gVisor netstack socket 天然使用） |
| 回程路径 | dst=VIP → outNIC → TUN → OS | dst=GIP → 精确匹配客户端 socket → 直接投递 |
| DNS 解析 | OS 系统 DNS → TUN 劫持 → hijacker | gVisor netstack socket → loopback → hijacker |
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
| UDP 出站非本地地址 → channel → outNIC | ✅ |

> 注：UDP loopback 投递是同步的，waiter 必须在 Write 之前注册。

### 待实现

- [x] loopback NIC 创建 + 路由配置（已实现于 engine.go initStack）
- [x] 验证 TCP/UDP loopback 投递
- [x] 验证非本地地址走 channel
- [x] DNS hijacker 绑定 NIC 0（任意网卡），接收来自 TUN 和 loopback 的 DNS 查询
- [x] Mode B (SOCKS5) 通过 gVisor netstack socket 进行 DNS 解析和连接建立
- [x] forwarder 排除 TUN 接口（BindContext + DialRouteAware）
- [x] tunNICID 重命名为 outNICID（不与 TUN 绑定）
- [x] TUN 入口 NAT 标记（src → VIP，readLoop 中实现）
- [x] TUN 出口反向 NAT（dst=VIP → hostIP，writeLoop 中实现）
- [x] 路由表更新（VIP → outNIC, meshSubnet → outNIC, 默认 → loNIC）
- [x] DirectDialer 域名走 netstack（解析为 fakeIP → loopback → forwarder）
- [x] loopbackRouting 标志（mesh 启用时设置，非 mesh TUN 模式用默认路由）

> **DirectDialer 说明**：域名走 netstack（resolve → fakeIP → loopback → forwarder），直接 IP 走 OS socket（DialRouteAware）。直接 IP 不能走 netstack，否则会死循环（directIP → netstack → loopback → forwarder → DirectDialer → netstack → ...）。

> **非 mesh TUN 模式**：当 mesh 未启用时，`loopbackRouting=false`，路由表为 `默认 → outNIC`（旧行为）。此时 DirectDialer 的 netstack 回调不会被设置，所有连接走 OS socket。

## 已完成的待实现

- [x] 删除 AddMeshVIPToOS（.1 不应配置给 OS）
- [x] DNS hijacker 支持 mesh 域名转发（改 dst 为 gateway GIP，注入 netstack）
- [x] HandleMeshFrame 区分 .1/.2/.3 的处理逻辑
- [x] 废弃 P2P 控制面 DNS 协议（SendMeshDNSQuery）
