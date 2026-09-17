# Mesh IPIP 隧道 + 管理面板优化设计

## 概述

本设计文档涵盖三个任务：
1. **Mesh IPIP 隧道**：在 mesh 层实现 IP-in-IP 封装，支持策略路由指定出口节点
2. **管理面板优化**：将日志和活跃连接从仪表盘移到独立页面
3. **SetReadDeadline 调研**：验证 gVisor 的 SetReadDeadline 是否可用，watchdog 是否必要

---

## 任务一：Mesh IPIP 隧道

### 背景与动机

当前 mesh 路由依赖节点通告的路由表。但某些场景需要：
- **策略路由**：在入口节点声明规则，指定流量从哪个出口节点发出
- **免通告路由**：不依赖通告的路由表，直接通过外层 IPIP 头路由
- **流量工程**：负载均衡、故障切换、地理路由等

### 设计目标

1. 在 mesh 层实现 IPIP 封装/解封装
2. 支持规则匹配指定出口节点
3. 利用外层 IPIP 头的路由能力
4. 与现有 TUN/mesh 架构解耦

### 架构设计

#### IP 地址规划

- **VIP**：100.x.x.x（现有，用于 NAT/内部寻址）
- **EIP**：从节点现有网段中预留专用 IP（如 100.0.0.254）
  - 不需要额外通告
  - 每个节点从自己的子网中分配
  - 用于 IPIP 外层头

#### 数据流

**去程（封装）**：
```
本地应用 → TUN 设备 → gVisor netstack → NAT（src=VIP/GIP）
  ↓
规则匹配："dst=X via node Y"
  ↓
Mesh 层封装：
  - 外层 IP 头：src=本地 EIP, dst=出口节点 EIP, protocol=4 (IPIP)
  - 内层 IP 包：原始包（src=NAT'd VIP/GIP, dst=X）
  ↓
Mesh P2P 发送到出口节点
```

**回程（无需特殊处理）**：
```
目标服务器响应 → 回程包（src=X, dst=NAT'd VIP/GIP）
  ↓
出口节点网络栈 → 正常路由回本地节点
```

**出口节点（解封装）**：
```
Mesh P2P 收到 IPIP 包（protocol=4）
  ↓
检查外层 IP 头
  ↓
剥离外层头，暴露内层 IP 包
  ↓
注入本地 gVisor netstack（或 TUN 设备）
  ↓
内层包继续路由到最终目标
```

#### 每层 IP 包源地址

| 层级 | 源地址 | 目标地址 | 说明 |
|------|--------|----------|------|
| 内层包 | NAT'd VIP/GIP | 最终目标 | TUN 层已完成 NAT |
| 外层包 | 本地节点 EIP | 出口节点 EIP | IPIP 封装头 |

### 实现方案

#### 1. EIP 分配

**文件**：`mesh/ipip.go`

EIP 是子网的第 5 个 IP（subnet + 4），例如 100.1.0.0/16 → 100.1.0.4。

**IP 保留方案**（前 10 个 IP）：
- .0 = 网络地址
- .1 = VIP（mesh 节点标识）
- .2 = hostIP（TUN 接口地址）
- .3 = GIP（DNS hijacker）
- .4 = EIP（IPIP 封装用）
- .5-.9 = 预留
- .10+ = Fake-IP 分配

**优势**：
- 避免与 Fake-IP 冲突（Fake-IP 从 .10 开始分配）
- 确定性计算，任何节点都可以从拓扑中已通告的子网计算出其他节点的 EIP
- 不需要额外的 gossip 消息
- 简单易记

```go
// CalculateEIP 计算 EIP
// EIP = subnet + 4 (例如：100.0.0.0/16 → 100.0.0.4)
func CalculateEIP(subnet *net.IPNet) net.IP {
    ip := subnet.IP.To4()
    eip := make(net.IP, 4)
    copy(eip, ip)
    eip[3] = ip[3] + 4
    return eip
}

// MeshManager.getEIPForNode 从拓扑中获取节点子网并计算 EIP
func (m *MeshManager) getEIPForNode(nodeID string) net.IP {
    subnet := m.getSubnetForNode(nodeID)
    return CalculateEIP(subnet)
}
```

#### 2. 封装逻辑（入口节点）

**文件**：`mesh/mesh.go` 或新增 `mesh/tunnel.go`

```go
// 在 mesh 发送包前检查是否需要 IPIP 封装
func (m *MeshManager) maybeEncapsulate(dstIP net.IP, packet []byte) ([]byte, error) {
    // 1. 检查规则：是否有 "via node X" 的规则
    // 2. 如果有，查找出口节点的 EIP
    // 3. 构造 IPIP 包：
    //    - 外层 IP 头：src=本地 EIP, dst=出口节点 EIP, protocol=4
    //    - 内层：原始 packet
    // 4. 返回封装后的包
}
```

#### 3. 解封装逻辑（出口节点）

**文件**：`mesh/mesh.go`

```go
// 在 mesh 收到包时检查是否是 IPIP
func (m *MeshManager) handleIPIPPacket(data []byte) error {
    // 1. 检查 IP 头的 protocol 字段是否为 4
    // 2. 剥离外层 IP 头
    // 3. 内层包注入 gVisor netstack
    //    - 调用 e.netstack.InjectInbound(innerPacket)
}
```

#### 4. 规则配置

**文件**：`config/config.go`

静态路由配置（与通告路由同层概念）：
```yaml
mesh:
  # 动态路由（通过 gossip 通告）
  advertise: ["10.0.0.0/8"]
  domain-suffixes: ["test.jf.local"]
  
  # 静态路由（本地配置，不依赖通告）
  static-routes:
    - dst: "10.0.0.0/8"
      via: "jf"  # 指定出口节点，使用 IPIP 封装
  static-domain-suffixes:
    - suffix: "internal.company.com"
      via: "jf"  # 指定出口节点，从该节点子网分配 Fake-IP
```

**设计原则**：
- **静态 IP 路由**：去程 IPIP 封装，回程正常路由（不对称，但不影响连接）
- **静态域名后缀**：DNS 解析时从目标节点子网分配 Fake-IP，正常 mesh 路由（无需 IPIP）

#### 5. 字节操作

IPIP 封装/解封装都是纯字节操作：
- **封装**：手动构造 IP 头字节（20 字节，protocol=4），拼接内层包
- **解封装**：解析外层 IP 头，剥离前 20 字节，内层包注入 netstack
- **传输**：通过现有 P2P 连接（TCP/UDP 流）发送，不需要原始 IP 能力

### 关键技术点

1. **IPIP 协议号**：IP 头中 protocol=4
2. **EIP 分配策略**：从节点子网中预留，无需通告
3. **封装位置**：mesh 层，在 P2P 发送前
4. **解封装位置**：mesh 层，在 P2P 接收后
5. **与 TUN 解耦**：TUN 层不感知 IPIP，只负责 NAT 和规则匹配

---

## 任务二：日志和连接页面拆分

### 背景

仪表盘仍然承载了太多内容：TUN 摘要、Mesh 摘要、活跃连接、日志、安全、快速操作等。需要将日志和活跃连接移到独立页面。

### 设计

#### 导航结构

```
📊 Dashboard
Runtime
  🌐 TUN
  🔷 Mesh
  📋 Logs (新)
  🔗 Connections (新)
Configuration
  📡 Subscriptions, 🔗 Proxies, 📋 Rules, 🔌 Mappings, ↗ Resolvers
Tools
  🧙 Reverse Wizard
```

#### 新增页面

1. **Logs 页面** (`/logs`)
   - 从仪表盘移入完整日志卡片
   - 独立页面，更大的显示区域
   - 支持 PiP 窗口

2. **Connections 页面** (`/connections`)
   - 从仪表盘移入活跃连接列表
   - 独立页面，支持排序/过滤
   - 支持 PiP 窗口

#### 实现

**文件变更**：
- `admin/admin.go`：新增路由和 handler
- `admin/templates/logs.html`：新建
- `admin/templates/connections.html`：新建
- `admin/templates/dashboard.html`：移除日志和连接卡片，替换为摘要
- `admin/templates/layout.html`：新增导航项
- `admin/static/app.js`：新增 PAGE_TITLES 条目
- `admin/static/i18n.js`：新增翻译键

---

## 任务三：SetReadDeadline 调研

### 背景

当前 TCP 和 UDP relay 使用 watchdog 方式实现空闲超时，代码注释说明是"avoid gVisor's SetReadDeadline lock contention that previously caused CreateEndpoint timeouts"。

需要调研：
1. gVisor 的 SetReadDeadline 是否真的有问题？
2. 是 bug（已修复？）还是使用方式不对？
3. Watchdog 是否真的必要？

### 调研计划

#### 1. 查看 gVisor 源码和 issue

- 搜索 gVisor 仓库关于 SetReadDeadline 的 issue
- 查看 gVisor netstack 的 SetReadDeadline 实现
- 确认是否有已知的锁竞争问题

#### 2. 测试 SetReadDeadline

在测试环境中：
- 使用 SetReadDeadline 替代 watchdog
- 压测观察是否有 CreateEndpoint 超时
- 监控锁竞争情况

#### 3. 决策

**如果 SetReadDeadline 可用**：
- 简化代码，移除 watchdog goroutine
- 减少 goroutine 和 channel 开销

**如果 SetReadDeadline 确实有问题**：
- 保持 watchdog 方式
- 在代码注释中详细说明原因

### 相关文件

- `tun/engine.go:1266` - `relayWithIdleTimeout`（TCP watchdog）
- `tun/engine.go:1483` - `relayUDP`（UDP watchdog）
- 注释：line 1264-1265

---

## 任务四：网络间歇性卡顿问题修复

### 背景

网络出现间歇性卡顿，表现为所有连接短暂挂起。分析发现多个潜在阻塞点。

### 问题分析与修复方案

| # | 位置 | 问题 | 修复方案 | 状态 |
|---|------|------|---------|------|
| 1 | `tun/engine.go:1000`<br>readLoop + LockOSThread | `device.Read()` 阻塞时 OS 线程挂起 | 已知 TUN Read Stall bug，需超时检测+设备重建 | 暂不修复（已有文档） |
| 2 | `tun/engine.go:1120`<br>单 writeLoop | `device.Write()` 阻塞时全部出站停滞 | 正常阻塞行为，除非 Write 长时间阻塞 | 暂不修复 |
| 3+7 | `mesh/mesh.go:1347,1388`<br>路由表读写锁 | `recomputeRoutes` 写锁与 `findRoute` 读锁竞争，peer 变动时阻塞 readLoop | **路由表 atomic.Value 无锁化**：读操作直接读指针，写操作在副本计算后 atomic swap | ✅ 待实现 |
| 4 | `tun/engine.go:1230`<br>TCP forwarder 回调 | 回调中同步执行 `handleConn`（DNS+拨号+relay），阻塞 gVisor worker | **handleConn 异步化**：回调只做 `CreateEndpoint` + `r.Complete()`，然后 `go handleConn()` | ✅ 待实现 |
| 5 | `dialer/ssh.go:76`<br>SSH reconnMu | SSH 重连 15s 阻塞同代理并发连接 | 设计选择，保持现状 | 不修复 |
| 6 | `mesh/dns.go:168,220`<br>DNS serveLoop | 单 goroutine 循环，`forwardToRemote` 同步阻塞 5s | **事件驱动异步化**：共享 UDP 连接 + pending queries map + 独立接收 goroutine | ✅ 待实现 |
| 8 | `tun/engine.go:1099`<br>InjectInbound | channel.Endpoint 容量 512，高负载丢包 | **扩容到 2048** | ✅ 待实现 |
| 9 | `reverse/registry.go:243`<br>Registry.Match 60s | 反向连接匹配超时 60s | 设计正确，保持现状 | 不修复 |
| 10 | `mesh/fakeip.go:85`<br>FakeIPPool 写锁 | 每次 DNS 查询获取写锁 | 纯本地计算，锁持有时间短，影响小 | 不修复 |

### 修复详细设计

#### 1. 路由表无锁化（#3+#7）

**当前问题**：
```go
// recomputeRoutes 持有写锁
m.routesMu.Lock()
m.routes = newRoutes
m.routesMu.Unlock()

// findRoute 持有读锁
m.routesMu.RLock()
route := m.routes
m.routesMu.RUnlock()
```

**修复方案**：
```go
// 使用 atomic.Value 存储路由表
type MeshManager struct {
    routes atomic.Value // 存储 *[]MeshRoute
}

// 读操作无锁
func (m *MeshManager) findRoute(dst net.IP) *MeshRoute {
    routes := m.routes.Load().(*[]MeshRoute)
    // 直接遍历，无锁
}

// 写操作在副本上计算，完成后 atomic swap
func (m *MeshManager) recomputeRoutes() {
    newRoutes := computeRoutes() // 在副本上计算
    m.routes.Store(&newRoutes)   // atomic swap
}
```

#### 2. TCP forwarder 异步化（#4）

**当前问题**：
```go
fwd := tcp.NewForwarder(e.ns, 0, 1024, func(r *tcp.ForwarderRequest) {
    ep, err := r.CreateEndpoint(&wq)  // 同步
    r.Complete(false)
    e.handleConn(conn, dstAddr, dstPort)  // 同步阻塞（DNS+拨号+relay）
})
```

**修复方案**：
```go
fwd := tcp.NewForwarder(e.ns, 0, 1024, func(r *tcp.ForwarderRequest) {
    ep, err := r.CreateEndpoint(&wq)  // 保持同步（通常很快）
    r.Complete(false)
    go e.handleConn(conn, dstAddr, dstPort)  // 异步，立即释放 worker
})
```

#### 3. DNS 事件驱动异步化（#6）

**当前问题**：
```go
func (h *DNSHijacker) serveLoop() {
    for {
        query := h.udpEP.Read()
        remoteIP, ttl, err := h.forwardToRemote(...)  // 阻塞 5s
        resp := buildDNSResponse(remoteIP)
        h.udpEP.Write(resp)
    }
}
```

**修复方案**：
```go
type DNSHijacker struct {
    remoteConn     *gonet.UDPConn
    pendingQueries map[uint16]pendingQuery  // txID → callback
    pendingMu      sync.Mutex
}

type pendingQuery struct {
    srcAddr tcpip.FullAddress
    callback func(net.IP, time.Duration)
}

func (h *DNSHijacker) init() {
    go h.receiveResponses()  // 独立接收 goroutine
}

func (h *DNSHijacker) serveLoop() {
    for {
        query := h.udpEP.Read()
        txID := extractTxID(query)
        
        // 注册 pending
        h.pendingMu.Lock()
        h.pendingQueries[txID] = pendingQuery{
            srcAddr: srcAddr,
            callback: func(ip net.IP, ttl time.Duration) {
                resp := buildDNSResponse(query, ip)
                h.udpEP.Write(resp, srcAddr)
            },
        }
        h.pendingMu.Unlock()
        
        // 发送查询后立即返回，不阻塞
        h.remoteConn.Write(query)
    }
}

func (h *DNSHijacker) receiveResponses() {
    for {
        resp := h.remoteConn.Read()
        txID := extractTxID(resp)
        
        h.pendingMu.Lock()
        if pending, ok := h.pendingQueries[txID]; ok {
            ip, ttl := parseDNSResponse(resp)
            go pending.callback(ip, ttl)  // 异步回调
            delete(h.pendingQueries, txID)
        }
        h.pendingMu.Unlock()
    }
}
```

#### 4. InjectInbound 扩容（#8）

**当前**：`channel.NewEndpoint(512, ...)`

**修复**：`channel.NewEndpoint(2048, ...)`

### 实施顺序

1. 路由表无锁化（#3+#7）
2. TCP forwarder 异步化（#4）
3. DNS 事件驱动异步化（#6）
4. InjectInbound 扩容（#8）

### 验证

1. 部署到 VM 环境
2. 高并发测试（100+ 并发连接）
3. 模拟 peer 变动（注册/注销）时观察是否卡顿
4. DNS 查询压力测试
5. 监控延迟和丢包率

### 异步设计原则（后续优化方向）

#### 核心原则

**原则一：不能因为自己的阻塞而阻塞别人**
- DNS 解析、TCP 连接、IP 包传输都应该独立执行
- 如果有阻塞，考虑用队列代替锁或无节制创建协程

**原则二：超时设计**

**讨论历程**：

1. **初始想法**：超时由外部程序控制
   - forwarder 和 mode B 接收端的 DNS/TCP 超时应该是"无限"的
   - 用 context 传递外部程序的取消信号
   - 外部程序断开 → cancel context → DNS/拨号取消

2. **反思**：纯代理转发逻辑也没有这种机制
   - relay 循环自然处理断开：一边关，另一边也关
   - 不需要复杂的 context 传递或监控 goroutine

3. **问题**：DNS/拨号阻塞期间客户端断开怎么办？
   - 会浪费内部资源（DNS 解析器、拨号 goroutine）
   - 但这是优化，不是必须
   - 过度设计（监控 goroutine、poll/epoll）得不偿失

4. **最终方案**：调整为国际惯例超时
   - DNS 解析：30 秒（RFC 建议 5 秒重试，总共约 30 秒）
   - TCP 拨号：120 秒（常见 HTTP 客户端默认值）
   - 即使外部愿意等，也不能让其无节制占用资源
   - 简单合理，不过度设计

**当前超时调整**：
| 位置 | 当前超时 | 调整为 |
|------|----------|--------|
| `tun/engine.go:447` `resolveForDirect` | 5 秒 | **30 秒** |
| `tun/engine.go:1656` `DialRouteAware` | 30 秒 | **120 秒** |
| `tun/engine.go:207` `ResolveDomain` | 5 秒 | **30 秒** |

#### 待优化项总表

| # | 位置 | 问题 | 方案 | 优先级 |
|---|------|------|------|--------|
| A1 | `tun/engine.go:1099`<br>readLoop 中 `meshInterceptor` | 同步调用，阻塞所有 TUN 包处理 | **队列化**：入 `meshOutboundCh`，独立 `meshOutboundLoop` 处理 | 高 |
| A2 | `p2p/p2p.go:369`<br>P2P 接收循环中 `HandleMeshFrame` | 同步调用，阻塞该 peer 的所有包接收 | **队列化**：入 `meshInboundCh`，独立 `meshInboundLoop` 处理 | 高 |
| A3 | `mesh/dns.go:168-261`<br>`serveLoop` 顺序处理 | 单 goroutine 顺序处理，一个慢查询阻塞其他 | **并行处理**：每个查询启动 goroutine 或 worker 池 | 高 |
| A4 | `mesh/fakeip.go:85-115`<br>池锁 + `onChange` 回调 | 锁持有期间调用 `onChange`，嵌套锁延迟 | **待讨论** | 中 |
| A5 | `mesh/dns.go:224-242`<br>远程 DNS 转发 | 每个查询启动 goroutine，高并发时资源耗尽 | **待讨论** | 中 |
| A6 | `tun/engine.go:1243`<br>TCP forwarder 回调 | `CreateEndpoint` 可能阻塞 gVisor | **快速路径**：确保不获取长时间锁 | 低 |
| B1 | `mesh/nat.go:77,159`<br>NAT 表写锁 | 每个包获取写锁更新 `LastSeen` | **`LastSeen` 用 `atomic.Int64`**：无锁更新 | 高 |
| B2 | `mesh/mesh.go:651,836,868`<br>WriteMeshPacket | 直接写 TUN，可能阻塞调用者 | **队列化写入**：`meshWriteCh` + `meshWriteLoop` | 中 |
| B3 | `mesh/mesh.go:651,701,753`<br>peer.Send() goroutine 包装 | `Send()` 已非阻塞，goroutine 多余 | **去掉 goroutine 包装** | 低 |
| B4 | `connlog/activeconn.go:62-81`<br>嵌套锁 | `activeMu` + `mu` 嵌套获取 | **无锁化**：`atomic.Int64` + `sync.Map` | 低 |
| C1 | `tun/engine.go:447,1656`<br>DNS/TCP 超时 | 代理强加 5s/30s 超时 | **调整为国际惯例**：DNS 30s，TCP 120s | 中 |

#### WriteMeshPacket 队列化设计

**当前设计**（可能阻塞）：
```
HandleMeshFrame → WriteMeshPacket → dev.Write()  // 直接写 TUN
```

**优化设计**（队列化）：
```
HandleMeshFrame → meshWriteCh ← meshWriteLoop → dev.Write()
     ↓                    ↓
  入 channel          消费并写入 TUN
```

**实现要点**：
1. Engine 添加 `meshWriteCh chan []byte`（缓冲 2048）
2. 启动 `meshWriteLoop()` goroutine 从 channel 消费，调用 `dev.Write()`
3. `WriteMeshPacket` 改为非阻塞入 channel：
   ```go
   select {
   case e.meshWriteCh <- data:
   default:
       return fmt.Errorf("mesh write queue full")
   }
   ```
4. channel 满时丢包或返回错误（可配置）

**好处**：
- 解耦 mesh 包处理和 TUN I/O
- 自然背压（channel 缓冲）
- 单写者避免竞争
- 类似 P2P 的 `writeCh` + `peerWriteLoop` 模式

#### 已正确缓解的阻塞点

| 位置 | 机制 |
|------|------|
| `p2p/p2p.go:172-181` | `enqueueWrite` 非阻塞，channel 满时丢包 |
| `tun/engine.go:1257` | TCP 连接各自独立 goroutine，互不影响 |
| `mesh/mesh.go` 多处 | 包发送用 goroutine 包装，异步执行 |

#### 队列化设计示例

**当前**（阻塞）：
```go
// readLoop
if e.meshInterceptor(dstIP, pktBuf) {
    continue
}
```

**优化**（队列化）：
```go
// Engine 添加
meshOutboundCh chan meshPacket  // 缓冲 4096

type meshPacket struct {
    dstIP net.IP
    data  []byte
}

// readLoop 改为
select {
case e.meshOutboundCh <- meshPacket{dstIP: dstIP, data: pktBuf}:
default:
    util.LogWarn("mesh outbound queue full, dropping packet")
}

// 独立 goroutine
func (e *Engine) meshOutboundLoop() {
    for pkt := range e.meshOutboundCh {
        e.meshInterceptor(pkt.dstIP, pkt.data)
    }
}
```

#### 全链路异步 DNS 设计（A3 + A4 + A5 统一解决）

**当前设计**（同步阻塞）：
```
serveLoop: UDP 收查询 → 处理（可能阻塞 5s）→ UDP 发响应 → 收下一个
```

**问题**：一个慢查询卡住整个循环，后续查询全部等待。

**优化设计**（全链路异步）：
```
receiveLoop:  UDP 收查询 → 入 queryCh → 继续收（不阻塞）
                    ↓
processLoop:  从 queryCh 取查询
                    ↓
              缓存命中？→ 直接拿 FakeIP → 入 responseCh
              本地域名？→ 分配 FakeIP（纯内存）→ 入 responseCh
              远端域名？→ 入 forwardCh（不阻塞等返回）
                    ↓
forwardLoop:  从 forwardCh 取 → 发送到远端 → 等响应 → 入 responseCh
                    ↓
respondLoop:  从 responseCh 取 → UDP 发回响应
```

**核心思路**：每一步都不阻塞等返回，全部通过 channel 解耦。

**实现要点**：

1. **queryCh**（缓冲 1024）：接收 UDP 查询
   ```go
   type dnsQuery struct {
       packet []byte
       domain string
       src    tcpip.FullAddress
   }
   ```

2. **responseCh**（缓冲 1024）：待发送的响应
   ```go
   type dnsResponse struct {
       packet []byte
       dst    tcpip.FullAddress
   }
   ```

3. **forwardCh**（缓冲 256）：远端转发请求
   ```go
   type dnsForward struct {
       query    dnsQuery
       subnet   *net.IPNet
       resultCh chan dnsResponse  // 响应回此 channel
   }
   ```

4. **FakeIP 分配无锁化**：
   - `onChange` 改为异步通知（channel 或 goroutine）
   - 锁只保护 map 操作，不保护回调

5. **远端转发有界化**：
   - 固定数量 forwardLoop worker（如 4 个）
   - forwardCh 满时返回 SERVFAIL

**好处**：
- 收查询不阻塞：receiveLoop 只做读取和入队
- 处理不阻塞：processLoop 纯内存操作或入队
- 远端转发不阻塞：入 forwardCh 就返回，响应异步回来
- 发响应不阻塞：respondLoop 只做发送

**统一解决 A3 + A4 + A5**：
- A3（serveLoop 顺序处理）→ 拆分为多个 loop，并行处理
- A4（FakeIP 池锁 + onChange）→ onChange 异步化
- A5（远端转发 goroutine 无界）→ 有界 worker 池

---

## 实施顺序

### 阶段一：调研与准备

1. **SetReadDeadline 调研**（1-2 天）
   - 查看 gVisor 源码/issue
   - 编写测试用例
   - 决策是否保留 watchdog

### 阶段二：管理面板优化

2. **日志和连接页面拆分**（2-3 天）
   - 新建 logs.html 和 connections.html
   - 从仪表盘移入相关代码
   - 更新导航和路由
   - 部署验证

### 阶段三：Mesh IPIP 隧道

3. **EIP 分配与规则配置**（2 天）
   - 设计 EIP 分配策略
   - 扩展规则配置支持 `via: node:X`

4. **IPIP 封装/解封装**（3-5 天）
   - 实现封装逻辑（入口节点）
   - 实现解封装逻辑（出口节点）
   - 集成测试

5. **端到端测试**（2 天）
   - 在 QG/VM/JF 环境测试
   - 验证策略路由
   - 性能测试

---

## 验证标准

### Mesh IPIP 隧道

1. 在入口节点配置规则：`dst:10.0.0.0/8 via node:jf`
2. 从本地访问 10.0.0.0/8 的流量
3. 验证流量通过 JF 节点出口
4. 抓包确认 IPIP 封装（外层 protocol=4）
5. 回程流量正常返回

**测试验证（已实现）**：

配置示例（VM 节点）：
```yaml
mesh:
    node-id: vm
    subnet: 100.1.0.0/16
    static-routes:
        - dst: 8.8.8.0/24
          via: jf
```

测试命令：
```bash
ping 8.8.8.8
```

预期结果：
- 回复来自 8.8.8.8（不是 100.1.0.3 gVisor）
- TTL 值合理（如 103，表示经过多跳）
- 日志显示 `[IPIP] Static route matched: dst=8.8.8.8 via=jf`

数据流：
1. VM ping 8.8.8.8 → TUN
2. HandleOutboundPacket 匹配静态路由 8.8.8.0/24 via jf
3. 计算 JF 的 EIP：100.2.255.254（从 JF 子网 100.2.0.0/16 计算）
4. IPIP 封装：外层 src=100.1.255.254 (VM EIP), dst=100.2.255.254 (JF EIP)
5. 通过 mesh 路由发送到 JF
6. JF 解封装，发送原始包到 8.8.8.8
7. 8.8.8.8 直接回复（不对称回程，无需 IPIP）

**调试日志**：
- `CheckStaticRoute` 对 8.8.8.0/24 范围的包记录 INFO 级别日志
- 便于验证静态路由匹配是否触发

**IPIP 回归排查日志**（任务四修复后 IPIP 失效）：
- `tun/engine.go` readLoop：对 8.8.8.x 包记录 INFO 级别日志，确认包是否到达 mesh 拦截点
- `mesh/mesh.go` HandleOutboundPacket：对 8.8.8.x 包记录 INFO 级别日志，确认拦截器是否被调用
- 用于定位卡顿修复后 IPIP 静态路由未触发的根因

**调试日志洪泛修复**（任务四修复后三环境卡顿）：
- 问题：`[MESH-DEBUG]`、`[TCP-DEBUG]`、`[DNS-DEBUG]`、`logTCPPacket` 使用 `LogInfo` 级别，每个包都打日志
- 影响：日志洪泛导致 CPU 占用高、响应延迟，三个环境都出现严重卡顿
- 修复：将所有高频调试日志降级为 `LogDebug` 级别，仅在需要调试时启用

### 管理面板

1. 仪表盘只显示摘要卡片
2. `/logs` 页面显示完整日志
3. `/connections` 页面显示活跃连接
4. PiP 窗口正常工作
5. SSE 实时更新正常

### SetReadDeadline

1. 明确 gVisor SetReadDeadline 的状态
2. 如果可用，简化代码
3. 如果不可用，文档化原因

---

## 风险与缓解

### Mesh IPIP 隧道

**风险**：
- IPIP 封装增加延迟和开销
- 与现有路由逻辑冲突

**缓解**：
- 性能测试
- 规则优先级设计

### 管理面板

**风险**：
- 页面拆分影响现有功能
- PiP 窗口逻辑复杂

**缓解**：
- 逐步拆分，先测试再部署
- 保留回滚能力

### SetReadDeadline

**风险**：
- 调研结论不明确
- 改动引入新 bug

**缓解**：
- 充分测试
- 灰度发布

---

## 附录

### 相关代码位置

- Mesh 路由：`mesh/topology.go`
- P2P 通信：`p2p/p2p.go`
- TUN 引擎：`tun/engine.go`
- 规则匹配：`config/config.go`
- 管理面板：`admin/admin.go`, `admin/templates/`

### 参考资料

- IPIP 协议：RFC 2003
- gVisor netstack：https://gvisor.dev/
- Phaethon mesh 设计：`docs/plans/mesh-full-topology-design.md`
