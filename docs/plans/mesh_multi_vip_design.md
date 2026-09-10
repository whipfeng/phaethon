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

**从"基于 VIP 的 overlay 网络"到"基于能力通告的任意拓扑路由"**

| 维度 | Mesh v1（当前） | Mesh v2（目标） |
|------|----------------|----------------|
| 节点标识 | VIP (100.64.x.x) | NodeID |
| TUN 依赖 | 必须 | 可选 |
| 路由依据 | VIP 前缀 | 能力通告（IP/域名） |
| 访问方式 | TUN only | TUN + 普通代理 |

### 7.2 能力通告

每个节点通告自己**能访问什么**：

```go
type CapabilityInfo struct {
    NodeID   string   `json:"nodeId"`
    IPs      []string `json:"ips,omitempty"`      // IP 或 CIDR，如 "8.8.8.8", "192.168.0.0/16"
    Domains  []string `json:"domains,omitempty"`  // 域名模式，如 "*.google.com", "api.github.com"
    Cost     int      `json:"cost"`               // 访问代价（可选）
}
```

**示例配置：**

```yaml
mesh:
  node-id: "gateway-cn"
  capabilities:
    ips:
      - "0.0.0.0/0"           # 能访问所有 IP（互联网出口）
      - "10.0.0.0/8"          # 能访问内网
    domains:
      - "*.google.com"
      - "*.github.com"
```

### 7.3 邻居学习与路径计算

**Gossip 传播能力通告：**

```
节点 A 通告: {nodeId: "A", ips: ["0.0.0.0/0"], domains: ["*.google.com"]}
节点 B 通告: {nodeId: "B", ips: ["10.0.0.0/8"], domains: ["*.internal.com"]}
```

**每个节点维护能力路由表：**

```go
type CapabilityRoute struct {
    Pattern  string   // IP CIDR 或域名模式
    NextHop  string   // 下一跳 NodeID
    Cost     int      // 总代价
}

// 路由表按优先级排序
// 1. 精确 IP 匹配（如 8.8.8.8/32）
// 2. 最长前缀匹配（如 10.0.0.0/8）
// 3. 域名精确匹配（如 api.github.com）
// 4. 域名通配符匹配（如 *.github.com）
// 5. 默认路由（0.0.0.0/0）
```

### 7.4 两种访问模式

#### 7.4.1 TUN 模式

```
OS 应用 → TUN 设备 → readLoop 读取 IP 包
    ↓
检查目标 IP/域名是否匹配能力路由表
    ↓
匹配：封装 mesh 包（外层=目标 NodeID，内层=原始 IP 包）
    ↓
通过 P2P 链路发送到下一跳
    ↓
目标节点收到 → 解封装 → 注入 netstack 或 OS → 访问目标
```

#### 7.4.2 普通代理模式（无 TUN）

```
SOCKS5/HTTP 代理收到请求（访问 api.github.com:443）
    ↓
检查目标是否匹配能力路由表
    ↓
匹配：创建 gVisor 虚拟 socket
    ↓
stack.NewEndpoint() → ep.Connect() → ep.Write()
    ↓
gVisor 产生出站 IP 包 → WritePackets 回调
    ↓
封装 mesh 包（外层=目标 NodeID，内层=IP 包）
    ↓
通过 P2P 链路发送到下一跳
    ↓
目标节点收到 → 解封装 → 注入 netstack → 访问目标
    ↓
响应包反向传回 → 虚拟 socket 收到数据 → 代理返回给客户端
```

### 7.5 Mesh 链路包结构（待细化）

**基本结构：**

```
+------------------+------------------------+
| 外层 Header      | 内层 Payload           |
| - 目标 NodeID    | - IP 包                |
| - 源 NodeID      | - 或 域名 + 数据       |
| - 包类型         |                        |
+------------------+------------------------+
```

**问题：域名传输效率**

每个包都带完整域名太浪费（如 `api.github.com` 16 字节 × 每个包）。

**候选方案：**

1. **Session ID 映射**
   - 首包：`{session: 0x1234, domain: "api.github.com", data: ...}`
   - 后续包：`{session: 0x1234, data: ...}`
   - 接收方维护 session → domain 映射表

2. **域名压缩**
   - 常见域名预定义短 ID（如 `google.com` = 0x01）
   - 或使用字典压缩

3. **混合模式**
   - IP 目标：直接传 IP 包（固定 20 字节头）
   - 域名目标：首包传域名，后续用 session ID

**待讨论确定具体方案。**

### 7.6 实现阶段

1. **Phase 1**: 能力通告基础设施
   - 定义 `CapabilityInfo` 结构
   - 修改 gossip 传播能力信息
   - 实现能力路由表

2. **Phase 2**: 去 VIP 化
   - NodeID 作为主要标识
   - 移除 VIP 相关逻辑（或作为可选兼容）

3. **Phase 3**: 普通代理模式
   - 集成 gVisor 用户态网络栈
   - 实现虚拟 socket → mesh 链路

4. **Phase 4**: 包结构优化
   - 确定域名传输方案
   - 实现 session 管理

### 7.7 与 v1 的兼容性

考虑渐进式迁移：
- v2 节点可以识别 v1 的 VIP 包
- v1 节点不识别 v2 的能力通告（忽略）
- 混合部署期间，v2 节点可以同时支持两种模式

## 8. 验收标准

- [x] 配置多个 VIP 后，所有 VIP 注册到 OS TUN 接口
- [x] Gossip 传播所有 VIP（通过日志确认）
- [x] 路由表包含所有远端 VIP 的条目（通过 admin API 确认）
- [x] Ping 不同 VIP 使用对应的源 IP（tcpdump 验证）
- [x] Mesh 内部互通正常（回归测试）
