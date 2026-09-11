# Mesh 多 VIP 与源 IP 选择设计

## 元数据

- 文档类型：Plan
- 版本：v0.1.0
- 所属项目：phaethon
- 创建日期：2026-09-10

## 变更记录

| 版本 | 日期 | 变更内容 | 作者 |
|------|------|----------|------|
| v0.1.0 | 2026-09-10 | 初始版本：多 VIP 支持、源 IP 选择机制 | Qoder |
| v0.2.0 | 2026-09-11 | 路由简化（PeerSender、prefix 路由）、gateway 转发、无 TUN 模式设计 | Qoder |

## 1. 背景与目标

### 1.1 当前问题

原始 mesh 实现中每个节点只能拥有一个 VIP（100.64.0.0/16 内），限制了以下场景：
- 一个节点需要代表多个 VIP 提供服务
- 需要按用途/协议划分不同的 VIP

同时存在以下 bug：
1. Gossip 只传播主 VIP，不传播额外 VIP
2. 路由计算只为主 VIP 创建路由条目
3. `UpdateFromGossip` 不检测 VIP 变化，导致不触发路由重算

### 1.2 目标

1. 支持每个节点拥有多个 VIP
2. Gossip 正确传播所有 VIP
3. 路由表为所有 VIP 创建条目
4. 明确源 IP 选择机制

## 2. 多 VIP 架构

### 2.1 配置层

`config/config.go` 中 `MeshConfig` 使用 `VIPs []string` 数组：

```yaml
mesh:
  enabled: true
  node-id: "node-a"
  vips:
    - "100.64.1.2"    # 主 VIP
    - "100.64.31.2"   # 额外 VIP
```

`main.go` 解析时第一个为主 VIP，其余为额外 VIP。

### 2.2 拓扑层

`mesh/topology.go` 中：
- `TopoNode.VIPs` 从 `net.IP` 改为 `[]net.IP`
- `TopologyInfo.VIPs` 从 `string` 改为 `[]string`
- 新增 `SetNodeVIPs(nodeID string, vips []net.IP)` 方法
- `GetNodeAllVIPs(nodeID string) []net.IP` 返回节点所有 VIP

### 2.3 Gossip 传播

`mesh/mesh.go` 中 `gossipLoop` 收集 `m.localVIPs` map 中所有 VIP，通过 `TopologyInfo.VIPs []string` 传播。

`UpdateFromGossip` 检测 VIP 变化（长度不同或内容不同），设置 `changed = true` 触发路由重算。

### 2.4 路由计算

`recomputeRoutes` 遍历 `GetNodeAllVIPs(dstNodeID)` 为每个 VIP 创建 `/32` 路由条目：

```go
for _, vip := range dstVIPs {
    prefixRoutes = append(prefixRoutes, PrefixRoute{
        Prefix:  &net.IPNet{IP: vip, Mask: net.CIDRMask(32, 32)},
        NextHop: nextHopVIP,
        Cost:    0,
    })
}
```

### 2.5 TUN 拦截

`tun/engine.go` 中 `localMeshVIPs` 从 `net.IP` 改为 `map[string]bool`，支持多个本地 VIP 匹配。

`main_tun.go` 调用 `SetMeshInterceptor` 时传入所有 VIP。

### 2.6 OS 注册

所有 VIP 都通过 `AddMeshVIPToOS` 注册到操作系统 TUN 接口。

## 3. 源 IP 选择机制

### 3.1 结论

**Windows 和 Linux 源 IP 选择策略不同**：

- **Windows**：自动按最长前缀匹配选择源 IP，无需额外配置
- **Linux**：默认使用接口主 IP，需要配置源路由（`ip route add <prefix> dev <dev> src <srcIP>`）才能根据目标选择正确的源 IP

### 3.2 Windows 验证结果

通过 tcpdump 在 QG 环境抓包验证：

| 目标 IP | 选择的源 IP | 匹配前缀长度 |
|---------|-------------|-------------|
| 100.64.1.1 | 100.64.1.2 | 24-bit (100.64.1.x) |
| 100.64.31.1 | 100.64.31.2 | 24-bit (100.64.31.x) |

尽管路由表中所有条目都是 `/32`，Windows 仍然按接口 IP 与目标 IP 的前缀匹配长度选择源 IP。

### 3.3 Linux 行为（已验证）

**Linux 默认使用接口的主 IP 作为源 IP，需要显式配置源路由才能根据目标选择正确的源 IP。**

QG 环境验证（有 `100.64.1.1/32` 和 `100.64.31.1/32` 两个 IP）：

**无源路由时**：
```bash
$ ip route get 100.64.1.2
100.64.1.2 dev tun0  src 100.64.1.1

$ ip route get 100.64.31.2
100.64.31.2 dev tun0  src 100.64.1.1    # 不是 100.64.31.1！
```

**添加源路由后**：
```bash
$ ip route add 100.64.31.0/24 dev tun0 src 100.64.31.1

$ ip route get 100.64.31.2
100.64.31.2 dev tun0  src 100.64.31.1   # 现在正确了
```

tcpdump 抓包确认：
```
# 无源路由
IP 100.64.1.1 > 100.64.1.2: ICMP echo request
IP 100.64.1.1 > 100.64.31.2: ICMP echo request   # 两个都用 100.64.1.1

# 有源路由
IP 100.64.1.1 > 100.64.1.2: ICMP echo request
IP 100.64.31.1 > 100.64.31.2: ICMP echo request  # 现在用 100.64.31.1
```

### 3.4 平台差异总结

| 平台 | 源 IP 选择策略 | 需要配置 |
|------|---------------|---------|
| Windows | 最长前缀匹配（自动） | 无需额外配置 |
| Linux | 默认用主 IP，需源路由才能按目标选择 | 需要 `ip route add <prefix> dev <dev> src <srcIP>` |

### 3.5 Mesh 层行为

Mesh **不修改**内层 IP 包的源 IP。`mesh/mesh.go:231` 中 `srcVIP := m.vip` 仅用于 mesh 帧头封装（P2P 路由），内层包的源 IP 保持 Windows 选择的原始值。

数据流：
```
Windows 选择源 IP (longest prefix match)
    ↓
内层 IP 包 (src=100.64.1.2, dst=100.64.1.1)
    ↓
Mesh 封装 (frame header: srcVIP, dstVIP 用于 P2P 路由)
    ↓
P2P 传输到目标节点
    ↓
目标节点解封装，取出内层 IP 包
    ↓
内层包源 IP 仍为 100.64.1.2（未被修改）
```

## 4. 关键决策

### 4.1 多 VIP 存储方式

**决策**：使用 `[]net.IP` 数组而非单个 VIP。

**理由**：简单直接，支持任意数量 VIP，与 gossip JSON 序列化兼容。

### 4.2 路由条目粒度

**决策**：每个 VIP 独立创建 `/32` 路由条目。

**理由**：与现有 prefix routing 架构一致，longest prefix match 自然工作。

### 4.3 源 IP 不修改

**决策**：Mesh 层不修改内层包的源 IP。

**理由**：
- 保持端到端透明性
- 目标节点能看到真实的源 VIP
- 响应包能正确路由回源

## 5. gVisor Netstack 用户态网络能力

### 5.1 结论

**gVisor netstack 支持纯用户态网络操作，无需 TUN 设备：**

- 创建出站 TCP/UDP socket
- 产生出站 IP 包（通过回调）
- 注入入站 IP 包
- 完整的用户态 TCP/IP 协议栈

### 5.2 API 验证

**创建出站连接：**

```go
// 创建 TCP endpoint
ep, err := stack.NewEndpoint(tcpip.TCPProtocolNumber, tcpip.IPv4ProtocolNumber, &wq)

// 连接到目标
err = ep.Connect(tcpip.FullAddress{Addr: tcpip.Address(dstIP), Port: 80})

// 发送数据
ep.Write(...)
```

**出站 IP 包回调：**

```go
// 实现自定义 LinkEndpoint
type MyLinkEndpoint struct {
    onOutbound func(pkt []byte)  // 出站包回调
}

// netstack 产生出站包时调用
func (e *MyLinkEndpoint) WritePackets(pkts stack.PacketBufferList) (int, tcpip.Error) {
    for _, pkt := range pkts.AsSlice() {
        e.onOutbound(pkt.AsSlice())  // 回调给用户
    }
    return len(pkts), nil
}
```

**入站 IP 包注入：**

```go
// 从自定义链路收到 IP 包，注入 netstack
pkt := stack.NewPacketBuffer(stack.PacketBufferOptions{Payload: buffer.MakeWithData(data)})
stack.InjectInbound(ipv4.ProtocolNumber, pkt)
```

### 5.3 完整流程

```
应用场景：SOCKS5 代理 → 用户态网络栈 → 自定义链路

1. SOCKS5 收到请求（访问 8.8.8.8:53）
2. stack.NewEndpoint(TCP, IPv4, &wq) → 创建虚拟 socket
3. ep.Connect(8.8.8.8:53) → 发起连接
4. ep.Write(dnsQuery) → 发送数据
5. netstack 产生出站 IP 包 → WritePackets 回调 → 发送到自定义链路
6. 对端处理，响应 IP 包回来
7. 收到响应 IP 包 → InjectInbound → 注入 netstack
8. netstack 处理响应 → socket 收到数据
9. 数据传回 SOCKS5 客户端
```

### 5.4 用途

- **无需 TUN 权限**：在没有 TUN 设备或权限的环境运行
- **灵活的网络拓扑**：IP 包可以通过任意链路传输（mesh、WebSocket、gRPC 等）
- **纯应用层虚拟网络**：完全在用户态实现网络栈

### 5.5 与当前架构的关系

当前 TUN 模式：
```
OS 应用 → TUN 设备 → readLoop → InjectInbound → netstack → Forwarder → proxy
```

用户态模式（未来可能）：
```
SOCKS5/HTTP 代理 → 用户态 socket → netstack → WritePackets 回调 → 自定义链路
```

两者可以共存，根据部署环境选择。

## 6. 已修复的 Bug

| Bug | 原因 | 修复 |
|-----|------|------|
| Gossip 只传播一个 VIP | `AddDirectLink` 只设置主 VIP | 新增 `SetNodeVIPs`，在 `RegisterPeer` 中调用 |
| Gossip 数据格式错误 | `BroadcastMeshGossip` 发送原始数据，接收端期望 JSON 命令 | 包装为 `{"cmd":"mesh_gossip","payload":...}` |
| 路由不更新 | `UpdateFromGossip` 不检测 VIP 变化 | 添加 VIP 变化检测，设置 `changed = true` |
| 额外 VIP 无路由 | `recomputeRoutes` 只处理主 VIP | 遍历 `GetNodeAllVIPs` 为所有 VIP 创建路由 |

## 7. Mesh v2 架构：基于能力通告的路由

### 7.1 核心变化

**从"基于内外层封装的 overlay"到"基于能力通告的纯 IP 路由"**

| 维度 | Mesh v1（当前） | Mesh v2（目标） |
|------|----------------|----------------|
| 节点标识 | VIP（注册到 OS） | VIP（不注册 OS，纯 mesh 标识） |
| NodeID | 无 | 展示用（字符串，方便人识别） |
| TUN 依赖 | 必须 | 可选 |
| 路由依据 | VIP 前缀 | 能力通告（IP/域名 + fakeIP） |
| 访问方式 | TUN only | TUN + 普通代理 |
| 包结构 | 内外层封装 | **纯 IP 包，无封装** |

### 7.2 节点标识与子网分配

**地址空间**：使用 100.64.0.0/10（CGNAT 段，RFC 6598）。此网段统一用于 mesh 和非 mesh 模式。

**子网分配**：人工指定，每个节点配置一个 /20 子网（4096 个地址，4093 个可用 fakeIP）。因为 node-id 本身也是人工指定的，如果 node-id 重复则无解，所以子网也采用人工指定，简单可靠。

```
100.64.0.0/10 (100.64.0.0 ~ 100.127.255.255)

示例:
Node A: 100.64.0.0/20
  ├── .0.1       → VIP（mesh 路由 + TUN NAT 源）
  ├── .0.2       → hostIP（TUN 适配器，注册 OS）
  ├── .0.3       → GIP（DNSHijacker，注册 netstack）
  └── .0.4~.15.254 → fakeIP 池（4093 个）

Node B: 100.64.16.0/20
  ├── .16.1   → VIP
  ├── .16.2   → hostIP
  ├── .16.3   → GIP
  └── .16.4~.31.254 → fakeIP 池
```

**三个地址的职责**：

| 地址 | 角色 | 注册 OS | 注册 netstack | 用途 |
|------|------|---------|--------------|------|
| .1 VIP | mesh 节点标识 | 否 | 否 | mesh 路由、TUN 流量 NAT 源 |
| .2 hostIP | TUN 适配器 | 是 | 否 | OS 侧适配器地址，不能注册 netstack（否则响应回环） |
| .3 GIP | netstack 内部 | 否 | 是 | DNSHijacker 绑定、代理模式 socket 源地址 |
| .4~末尾 | fakeIP 池 | 否 | 否 | DNS 解析分配，流量在子网内天然路由到该节点 |

**两种流量模式的源地址**：

```
TUN 流量:   NAT 后 src=VIP (.1)    → mesh → gateway
代理流量:   netstack socket src=GIP (.3) → mesh → gateway
```

gateway 回程时根据 dst 自动分流：

```
响应 dst=VIP (.1)  → 反向 NAT → TUN → LAN Host
响应 dst=GIP (.3)  → InjectInbound → netstack → 虚拟 socket → Client
```

**VIP（4 字节）**：
- 子网 .1 地址
- 节点在 mesh 中的标识
- 不注册到 OS 和 netstack，纯 mesh 内部使用
- TUN 旁路网关模式下作为 NAT 源地址
- 也是 DNS 查询的目标（能力通告中域名对应的 gateway VIP）

**NodeID（字符串）**：
- 配置文件、日志、Admin API 中展示
- 方便人识别，如 "gateway-cn"、"node-a"

```yaml
mesh:
  node-id: "gateway-cn"      # 展示用
  vip: "100.64.0.1"          # 子网 .1，节点标识（人工指定）
  # 自动推导:
  #   子网 = 100.64.0.0/20
  #   hostIP = 100.64.0.2
  #   GIP = 100.64.0.3
  #   fakeIP 池 = 100.64.0.4 ~ 100.64.15.254
```

### 7.3 能力通告

每个节点通告自己**能访问什么**：

```go
type CapabilityInfo struct {
    NodeID  string   `json:"nodeId"`
    VIP     string   `json:"vip"`       // 子网第一个 IP
    Subnet  string   `json:"subnet"`    // 子网 CIDR，如 "100.64.0.0/20"
    IPs     []string `json:"ips,omitempty"`      // IP 或 CIDR
    Domains []string `json:"domains,omitempty"`  // 域名后缀
}
```

**域名后缀通告**：只通告域名后缀，不使用通配符，不使用前导点，按**最长后缀匹配**原则查找。

**域名匹配规则**（与 Clash、dnsmasq 等一致）：
- 配置 `github.com`，匹配 `github.com` 和所有子域名（如 `api.github.com`）
- 不需要写 `.github.com` 或 `*.github.com`
- 配置 `com.github` 不会匹配 `github.com`（必须完整后缀）

**示例配置：**

```yaml
mesh:
  node-id: "gateway-cn"
  subnet: "100.64.0.0/20"
  advertise:
    - "0.0.0.0/0"           # 能访问所有 IP（互联网出口）
  domain-suffixes:
    - "github.com"          # 能解析 github.com 及其所有子域名
    - "google.com"
```

**最长后缀匹配示例：**

```
Gateway A 通告: "github.com"
Gateway B 通告: "api.github.com"

查询 "cdn.api.github.com"
  → 最长后缀匹配: "api.github.com" → Gateway B

查询 "raw.github.com"
  → 最长后缀匹配: "github.com" → Gateway A
```

**查找结构**：使用 trie，按域名标签倒序建树：

```
com
 └── github
      ├── (Gateway A)
      └── api
           └── (Gateway B)
```

查找时从根往下遍历，最深匹配节点即为结果。O(k) 复杂度，k 为域名标签数。

**好处：**
- 不用写通配符，通告就是纯后缀
- 层级自然形成，不同 gateway 可以负责不同层级
- 查找快，trie 遍历即可
- 可以精细控制：某些子域名走不同 gateway

### 7.4 能力路由表

每个节点维护能力路由表，根据 **dstIP** 决定下一跳：

```go
type CapabilityRoute struct {
    Pattern  string  // IP CIDR 或子网 CIDR
    NextHop  string  // 下一跳节点的 VIP
    Cost     int     // 总代价
}

// 路由优先级：
// 1. 精确 IP 匹配（如 8.8.8.8/32）
// 2. 最长前缀匹配（如 10.0.0.0/8）
// 3. 子网匹配（如 100.64.16.0/20 → Gateway B，用于 fakeIP 路由）
// 4. 默认路由（0.0.0.0/0）
```

### 7.5 DNS 解析流程

**机制**：系统 DNS 指向 TUN IP，DNSHijacker 在 netstack 内部监听 UDP 53（内嵌 DNS 服务器），不是从 TUN readLoop 拦截端口。

**完整流程：**

```
① 应用调用 getaddrinfo("api.github.com")
② OS 向系统 DNS 发送查询 → 系统 DNS 指向 GIP → 包进入 netstack
③ DNSHijacker（netstack 内 UDP 53，绑定 GIP）收到查询
④ 解析域名: api.github.com
⑤ 查能力路由表（trie 最长后缀匹配）:
   "api.github.com" → Gateway B (VIP=100.64.16.1)
⑥ 封装 DNS 查询包，通过 mesh 发给 Gateway B
   src=100.64.0.1, dst=100.64.16.1, payload=DNS query
⑦ Gateway B 收到 mesh 包，dst=VIP 是本地的
   → InjectInbound 注入 netstack
   → netstack 投递给 DNSHijacker（绑定 GIP，监听 UDP 53）
⑧ Gateway B 从自己子网分配 fakeIP: 100.64.0.5
   记录映射: 100.64.0.5 ↔ api.github.com
   （此时不解析真实 IP）
⑨ DNS 响应通过 mesh 返回: api.github.com = 100.64.0.5
⑩ 客户端 DNSHijacker 返回响应给应用
⑪ 应用连接 100.64.0.5:443
⑫ TUN readLoop 拦截 IP 包，查能力路由表:
   100.64.0.5 匹配 100.64.0.0/20 → Gateway B
⑬ 通过 mesh 发给 Gateway B
⑭ Gateway B 收到 dst=100.64.0.5
   查映射: 100.64.0.5 → api.github.com
   此时才解析真实 IP，创建出站连接
```

**关键设计点：**

1. **DNS 查询路由**：谁通告了域名后缀能力，DNS 查询就发给谁的 VIP
2. **Gateway 接收 DNS**：mesh 包到达后 InjectInbound 注入 netstack，由 DNSHijacker 处理（和普通 DNS 查询走同一路径）
3. **fakeIP 分配**：gateway 从自己 /20 子网分配，不需要预绑定
4. **延迟解析**：DNS 阶段只分配 fakeIP，不解析真实 IP；真实 IP 在流量到达 gateway 时才解析
5. **fakeIP 路由**：fakeIP 在 gateway 子网内（如 100.64.0.5 在 100.64.0.0/20），天然路由到该 gateway

**DNS 查询转发实现：**

```go
// DNSHijacker.serveLoop 中
domain := parseDNSQueryDomain(packet)

// 查能力路由表（trie 最长后缀匹配）
gateway := capabilityTrie.LongestMatch(domain)
if gateway != nil && gateway != self {
    // 通过 mesh 转发 DNS 查询给 gateway
    meshForwardDNSQuery(gateway.VIP, packet)
    // 等待 gateway 返回 fakeIP
    fakeIP := <-dnsResponseCh
    // 返回 DNS 响应给应用
    return buildDNSResponse(packet, fakeIP)
}

// 本地处理（自己就是 gateway 或无匹配）
fakeIP := allocateFromOwnSubnet(domain)
return buildDNSResponse(packet, fakeIP)
```

### 7.6 Mesh 包结构：纯 IP 包

**关键结论：不需要内外层封装，mesh 链路传输的就是普通 IP 包。**

```
+------------------------------------------+
| IP Header                                |
| srcIP = 源节点 VIP（NAT 后）              |
| dstIP = 目标地址（真实 IP 或 fakeIP）     |
+------------------------------------------+
| Payload (TCP/UDP data)                   |
+------------------------------------------+
```

**路由方式**：
- 中间节点根据 dstIP 查能力路由表，决定下一跳
- 到达目标节点后，该节点处理并转发到真实目标

### 7.7 NAT 机制

**问题**：源节点可能来自 LAN（如 192.168.1.100），这个 IP 在 mesh 中不可路由。

**解决**：源节点做 NAT，将真实源 IP 替换为自己的 VIP。

```
旁路网关场景：

LAN Host (192.168.1.100) 访问 8.8.8.8:8080
    ↓
Node A (旁路网关, VIP=100.64.0.1) 拦截
    ↓
NAT: src=192.168.1.100 → src=100.64.0.1 (VIP)
    ↓
Mesh 包: src=100.64.0.1, dst=8.8.8.8
    ↓
能力路由: 8.8.8.8 → Node B (VIP=100.64.16.1)
    ↓
Node B 收到，转发到真实 8.8.8.8
    ↓
响应: src=8.8.8.8, dst=100.64.0.1
    ↓
路由回 Node A
    ↓
Node A 反向 NAT: dst=100.64.0.1 → dst=192.168.1.100
    ↓
发给 LAN Host
```

**NAT 表维护**：
- 旁路网关/代理节点需要维护 NAT 表
- 类似 Linux iptables MASQUERADE

### 7.8 两种访问模式（统一流量图）

同一节点可同时支持 TUN 旁路网关和普通代理两种模式，两种流量在能力路由表处汇合，走同一条 mesh 链路。

```
  LAN Host / Client                  Node A (VIP=100.64.0.1)                Node B (VIP=100.64.16.1)             Internet
       │                                     │                                     │                                │
  ┌────┴──────┐                              │                                     │                                │
  │ LAN Host  │                              │                                     │                                │
  │192.168.   │                              │                                     │                                │
  │ 1.100     │                              │                                     │                                │
  │ 或 Client │                              │                                     │                                │
  │ (SOCKS5)  │                              │                                     │                                │
  └────┬──────┘                              │                                     │                                │
       │ ① IP包 / SOCKS5请求                  │                                     │                                │
       ├────────────────────────────────────►│                                     │                                │
       │                                     │                                     │                                │
       │                              ┌──────┴──────────────────────┐              │                                │
       │                              │ 入口处理                     │              │                                │
       │                              │                              │              │                                │
       │                              │ 模式A (TUN旁路网关):          │              │                                │
       │                              │  ② TUN readLoop 拦截         │              │                                │
       │                              │  ③ NAT: src→100.64.0.1(VIP) │              │                                │
       │                              │                              │              │                                │
       │                              │ 模式B (SOCKS5代理):           │              │                                │
       │                              │  ② gVisor 虚拟socket         │              │                                │
       │                              │  ③ netstack产生IP包           │              │                                │
       │                              │     src=100.64.0.3(GIP)      │              │                                │
       │                              │                              │              │                                │
       │                              │ 两种模式汇合:                  │              │                                │
       │                              │  ④ 查能力路由表                │              │                                │
       │                              │    dst→nextHop 100.64.16.1   │              │                                │
       │                              └──────┬──────────────────────┘              │                                │
       │                                     │                                     │                                │
       │                                     │ ⑤ IP包: src=VIP或GIP, dst=8.8.8.8   │                                │
       │                                     ├────────────────────────────────────►│                                │
       │                                     │         P2P Mesh (纯IP包)           │                                │
       │                                     │                                     │                                │
       │                                     │                              ┌──────┴──────────────────────┐        │
       │                                     │                              │ ⑥ 收到 mesh IP包             │        │
       │                                     │                              │    dst=8.8.8.8               │        │
       │                                     │                              │ ⑦ netstack 处理              │        │
       │                                     │                              │    Forwarder 创建出站连接     │        │
       │                                     │                              │ ⑧ 转发到真实目标             │        │
       │                                     │                              └──────┬──────────────────────┘        │
       │                                     │                                     │                                │
       │                                     │                                     │ ⑨ TCP/UDP 到真实目标          │
       │                                     │                                     ├───────────────────────────────►│
       │                                     │                                     │                                │
       │                                     │                                     │          ┌──────────┐        │
       │                                     │                                     │          │ Internet │        │
       │                                     │                                     │          │ 8.8.8.8  │        │
       │                                     │                                     │          └────┬─────┘        │
       │                                     │                                     │               │              │
       │                                     │                                     │ ⑩ 响应         │              │
       │                                     │                                     │◄──────────────┘              │
       │                                     │                              ┌──────┴──────────────────────┐        │
       │                                     │                              │ ⑪ netstack 收到响应          │        │
       │                                     │                              │ ⑫ 封装回程 IP包              │        │
       │                                     │                              │    src=8.8.8.8, dst=源地址    │        │
       │                                     │                              └──────┬──────────────────────┘        │
       │                                     │                                     │                                │
       │                                     │ ⑬ 回程 IP包                         │                                │
       │                                     │◄────────────────────────────────────┤                                │
       │                                     │         P2P Mesh (纯IP包)           │                                │
       │                                     │                                     │                                │
       │                              ┌──────┴──────────────────────┐              │                                │
       │                              │ 回程处理（按 dst 分流）       │              │                                │
       │                              │                              │              │                                │
       │                              │ dst=VIP (.1):                │              │                                │
       │                              │  ⑭ 反向NAT: dst→192.168.1.100│              │                                │
       │                              │  ⑮ 写回 TUN → OS → LAN Host │              │                                │
       │                              │                              │              │                                │
       │                              │ dst=GIP (.3):                │              │                                │
       │                              │  ⑭ InjectInbound → netstack │              │                                │
       │                              │  ⑮ 虚拟socket→代理→Client   │              │                                │
       │                              └──────┬──────────────────────┘              │                                │
       │◄────────────────────────────────────┤                                     │                                │
       │ ⑮ 响应到达 LAN Host / Client        │                                     │                                │
```

**两种模式对比**：

| 维度 | 模式 A：TUN 旁路网关 | 模式 B：普通代理 |
|------|---------------------|-----------------|
| 入口 | LAN Host 的 IP 包（OS 内核） | SOCKS5/HTTP 代理请求（应用层） |
| 源 IP 处理 | NAT：真实 LAN IP → VIP (.1) | gVisor 虚拟 socket，源 IP = GIP (.3)，无需 NAT |
| 目标地址 | 真实 IP（如 8.8.8.8） | fakeIP（如 100.64.16.5 → api.github.com） |
| 需要 TUN | 是 | 否 |
| 需要 NAT | 是（LAN IP 不可路由） | 否（GIP 已是 mesh 可路由地址） |
| Gateway 处理 | netstack Forwarder 直接转发到真实 IP | netstack 先查 fakeIP → 域名，再连接真实目标 |
| 回程分流 | dst=VIP → 反向 NAT → TUN → OS → LAN Host | dst=GIP → InjectInbound → netstack → 虚拟 socket → Client |

### 7.9 Gateway 侧处理

Gateway 节点收到 mesh 包后：

1. **dstIP 是真实 IP**（如 8.8.8.8）：
   - 用 netstack Forwarder 处理
   - 创建出站连接到真实目标
   - 响应通过 netstack 回来，转发回 mesh

2. **dstIP 是 fakeIP**（如 100.64.16.5，属于 Node A 的 /20 子网）：
   - 查表：100.64.16.5 → api.github.com
   - 解析域名获取真实 IP
   - 创建出站连接
   - 响应转发回 mesh

**回程路由**：
- Gateway 的 netstack 需要配置路由，让 mesh VIP 段走 mesh link endpoint
- 或动态添加已知节点 VIP 的 /32 路由到 mesh link

### 7.10 实现阶段

1. **Phase 1**: 子网分配与能力通告
   - 每个节点分配 /20 子网（第一个 IP = VIP，第二个 = hostIP，第三个 = GIP，其余 = fakeIP 池）
   - 定义 `CapabilityInfo` 结构（含 Subnet、域名后缀列表）
   - 修改 gossip 传播能力信息（含子网和域名后缀）

2. **Phase 2**: 能力路由表 + 域名 trie
   - 实现 IP 能力路由表（按 dstIP 最长前缀匹配）
   - 实现域名 trie（按后缀倒序建树，最长后缀匹配）
   - 子网路由：fakeIP 在 gateway 子网内，天然路由到该 gateway

3. **Phase 3**: DNS 解析流程改造
   - DNSHijacker 增加 mesh DNS 转发能力
   - 查域名 trie 找到对应 gateway，通过 mesh 转发 DNS 查询
   - Gateway 从自己子网分配 fakeIP，记录 fakeIP ↔ 域名映射
   - 延迟解析：DNS 阶段不解析真实 IP，流量到达时才解析

4. **Phase 4**: 去内外层封装
   - Mesh 链路直接传 IP 包
   - 移除 encodeMeshFrame/decodeMeshFrame
   - 实现基于 dstIP 的能力路由转发

5. **Phase 5**: NAT 机制
   - 旁路网关实现 NAT 表
   - 源节点做 NAT（真实 IP ↔ VIP）

6. **Phase 6**: 普通代理模式
   - 集成 gVisor 用户态网络栈（虚拟 socket，src=VIP 无需 NAT）
   - Gateway 侧 fakeIP → 域名映射 → 解析真实 IP → 出站连接

### 7.11 与 v1 的兼容性

考虑渐进式迁移：
- v2 节点可以识别 v1 的封装包（兼容期）
- v1 节点不识别 v2 的纯 IP 包
- 混合部署期间，建议统一升级

## 8. 验收标准

- [x] 配置多个 VIP 后，所有 VIP 注册到 OS TUN 接口
- [x] Gossip 传播所有 VIP（通过日志确认）
- [x] 路由表包含所有远端 VIP 的条目（通过 admin API 确认）
- [x] Ping 不同 VIP 使用对应的源 IP（tcpdump 验证）
- [x] Mesh 内部互通正常（回归测试）

## 9. Mesh 路由简化方案（最终结论）

### 9.1 核心原则

1. **Gossip 只通告必要信息**，其余全部本地计算
2. **路由表直接指向直连 peer/link**，不经过 nodeID 中转
3. **nodeID 只是给人看的标识**，不参与路由决策
4. **VIP 从子网推导**（.1 地址），不需要通告

### 9.2 Gossip 内容

每个节点通告 4 个字段：

```json
{
  "nodeId": "QG",
  "subnet": "100.64.0.0/24",
  "domainSuffixes": ["phn"],
  "routes": ["0.0.0.0/0"]
}
```

| 字段 | 含义 | 示例 |
|------|------|------|
| nodeId | 人可读标识（不参与路由） | "QG" |
| subnet | 节点自己的 mesh 子网，用于推导 .1/.2/.3 地址 | "100.64.0.0/24" |
| domainSuffixes | 能解析的域名后缀（不带前导点） | ["phn", "github.com"] |
| routes | 能到达的**额外**路由网段（不含 subnet 本身） | ["0.0.0.0/0", "192.168.1.0/24"] |

**subnet 不合并到 routes 的原因**：收到 gossip 的节点需要从 subnet 计算 gateway 的 .3 地址（GIP），用于 DNS 查询发送目标。

**地址推导规则**（所有节点算法一致）：
- .1 = VIP（mesh 标识、NAT 源）
- .2 = hostIP（TUN 适配器，注册 OS）
- .3 = GIP（DNSHijacker 绑定，netstack 内部）

**路由构建**：subnet 和 routes 都作为路由条目，subnet 本身也是一条路由。

### 9.3 路由表

路由表直接指向直连 peer/link 对象：

```go
type MeshRoute struct {
    Prefix *net.IPNet  // 匹配前缀
    Peer   *Peer       // 直连 peer 对象（不是 nodeID）
}
```

**构建规则**：

```
收到 peer 的 gossip:
  subnet "100.64.0.0/24"  → 添加路由 100.64.0.0/24 → 该 peer
  routes ["0.0.0.0/0"]    → 添加路由 0.0.0.0/0    → 该 peer
```

subnet 本身也是一条路由，和 routes 字段中的条目同等对待。

**查找规则**：Longest prefix match，找到对应的 peer，直接发送。

### 9.4 路由示例

**两节点直连**：

```
QG: subnet=100.64.0.0/24, routes=["0.0.0.0/0"]
VM: subnet=100.64.1.0/24, routes=[]

VM 的路由表:
  100.64.0.0/24 → peer QG 的链路   (来自 QG 的 subnet)
  0.0.0.0/0     → peer QG 的链路   (来自 QG 的 routes)

QG 的路由表:
  100.64.1.0/24 → peer VM 的链路   (来自 VM 的 subnet)
```

**三节点多跳**：

```
A: subnet=100.64.0.0/24, 直连 B
B: subnet=100.64.1.0/24, 直连 A 和 C
C: subnet=100.64.2.0/24, routes=["0.0.0.0/0"]

A 的路由表:
  100.64.1.0/24 → peer B 的链路   (B 的 subnet)
  100.64.2.0/24 → peer B 的链路   (C 的 subnet，经 B 转发)
  0.0.0.0/0     → peer B 的链路   (C 的 routes，经 B 转发)
```

不管几跳，路由表始终指向**自己的直连链路**。

### 9.5 发包流程

```
ping 100.64.0.5
  → longest prefix match: 100.64.0.5 匹配 100.64.0.0/24
  → 找到 peer QG 的链路
  → peer.Send(data)  // 直接用 peer 连接发送，不经过 nodeID
```

### 9.6 域名路由

域名路由独立于 IP 路由，用 trie 最长后缀匹配：

```
查询 "test.phn"
  → trie 匹配 "phn" → 找到对应 peer
  → 通过该 peer 的链路发送 DNS 查询
```

域名后缀不带前导点：配置 "phn" 匹配 "phn" 和 "*.phn"。

### 9.7 与现有代码的差异

| 维度 | 现有实现 | 简化方案 |
|------|---------|---------|
| Gossip 字段 | nodeId, VIPs, subnet, domainSuffixes, links, routes | nodeId, subnet, domainSuffixes, routes |
| 路由表 | prefix → nextHop VIP (net.IP) | prefix → peer 对象 |
| 路由计算 | Dijkstra + VIP 查找 + prefix 排序 | 直接从 gossip 构建（subnet + routes），longest prefix match |
| 发包 | SendMeshPacketByVIP(nextHopVIP, data) | peer.Send(data) |
| nodeID 用途 | 路由中转 | 仅展示 |
| VIP 传播 | gossip 通告 | 从 subnet 本地计算 (.1) |
| subnet 用途 | 仅配置 | 配置 + gossip 通告（接收方用于推导 .1/.3 和路由） |

### 9.8 实现步骤

1. **简化 TopologyInfo**：移除 VIPs、Links 字段，保留 nodeId、subnet、domainSuffixes、routes
2. **简化路由表**：`PrefixRoute` 从 `NextHop net.IP` 改为 `Peer *Peer`（或 peer 引用）
3. **简化 recomputeRoutes**：直接从 gossip 的 subnet + routes 构建路由表，不需要 Dijkstra
4. **简化发包**：用 peer 直接发送，不需要先查 nodeID 再查连接
5. **清理冗余代码**：移除 VIP 传播、邻居学习、Dijkstra 等不再需要的逻辑

## 10. Gateway 无 TUN 模式

### 10.1 动机

某些节点充当 mesh gateway，通告外部路由（如 `0.0.0.0/0`、`10.0.0.0/8`），但不需要本地 TUN 拦截。典型场景：机房服务器做互联网出口、企业内网节点通告内部网段。

这些节点需要 gVisor netstack 处理 mesh 收到的包（Forwarder/proxy 转发），但不需要 TUN 设备。

### 10.2 核心结论

**gVisor netstack 不依赖 TUN**。gVisor 是用户态 TCP/IP 协议栈，TUN 只是给它喂包的一种方式。当前代码把 TUN 和 gVisor 绑在 `Engine.Start()` 里一起启停，这是代码耦合，不是架构限制。

### 10.3 改动

把 `Engine.Start()` 里的两步拆开：

1. **gVisor netstack 初始化**（始终执行）：创建 stack、注册协议、创建 LinkEndpoint、启动 Forwarder
2. **TUN 设备初始化**（可选）：打开 Wintun/libtun、启动 readLoop/writeLoop、设置系统 DNS、添加 OS 路由

```go
func (e *Engine) Start() error {
    // 1. 始终初始化 gVisor netstack
    e.initNetstack()

    // 2. 仅在 tun.enabled=true 时打开 TUN 设备
    if e.tunEnabled {
        e.initTunDevice()
        e.setupSystemDNS()
        e.setupOSRoutes()
    }

    return nil
}
```

TUN 关闭时，gVisor 照常运行。mesh 收到的包通过 `InjectInbound` 进入 gVisor，gVisor 通过 Forwarder/proxy 处理后走正常出站路径。

### 10.4 配置

```yaml
mesh:
  enabled: true
  node-id: "gateway-cn"
  subnet: "100.64.0.0/24"
  advertise:
    - "0.0.0.0/0"

tun:
  enabled: false    # TUN 关闭，gVisor 仍然启动
```

### 10.5 与现有架构的关系

```
                        ┌─────────────────────────────────┐
                        │         gVisor netstack          │
                        │  (TCP/IP 协议栈、Forwarder)      │
                        └──────────┬──────────────────────┘
                                   │
                    ┌──────────────┼──────────────┐
                    │                             │
              ┌─────┴─────┐               ┌──────┴──────┐
              │ TUN 模式   │               │ mesh 收包    │
              │            │               │             │
              │ readLoop   │               │ InjectInbound│
              │ writeLoop  │               │             │
              │ DNS 拦截   │               │             │
              └────────────┘               └─────────────┘
```

两种入口共享同一个 gVisor netstack，出站路径统一。TUN 只是其中一种入口，不是必须的。
