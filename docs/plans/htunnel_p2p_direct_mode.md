# h_tunnel P2P 直发模式：FrameTransport 多态 + MESH 通道

| | |
|---|---|
| 版本 | v0.2.0 |
| 状态 | DRAFT |
| 日期 | 2026-09-30 |
| 关联 | `docs/plans/htunnel_transport_optimization.md`（v1 流平面优化，保留并存）；`docs/plans/p2p_control_frame_congestion.md`（P2P TCP 链路，正交） |

## 版本历史

| 版本 | 日期 | 变更 |
|------|------|------|
| v0.1.0 | 2026-09-17 | 初稿：frame 包抽离、FrameTransport 多态、MESH 直发通道（客户端+服务端） |
| v0.2.0 | 2026-09-30 | 连接池与心跳优化：独立心跳、GET 从 16 减到 2（动态扩展）、帧批次可配置、客户端控制等待时间 |

## 一、背景

P2P 流量经过 h_tunnel 时的封装链：

```
mesh帧/IP包 → P2P帧 → htunnel流(seq纪律) → HTTP POST     （三重封装）
```

问题：

1. **三步握手 + 一跳 splice**：`DialP2P` 走 `dialHTunnel("BIND",...)` 三次 HEAD 建通道，服务端还要把流 splice 到本地 reverse server（多一次 loopback TCP），P2P 才接管。
2. **seq 纪律脆弱**：全局严格 +1（`step != 1 → 410`），任何异常（重试、乱序）杀死整个通道。
3. **心跳 PUT**：htunnel 层 30s PUT 与 P2P 层 10s heartbeat 帧重复保活。
4. **HOL**：单个 keepalive 连接上，控制帧（hello/gossip/heartbeat）必须排在批量 mesh 数据后面。
5. **每拨号新建 http.Client**：无跨连接复用（v1 文档 §1.1）。

## 二、方案总览：P2P 帧传输层多态

不换协议、不加平面。在"P2P 帧传输"抽象出统一接口，htunnel 成为多态成员之一：

```
                    frame.FrameTransport（数据报语义：丢/重/乱序容忍）
                   /                |                \
        streamTransport      htunnelDirectTransport    （未来可扩展）
        (直连TCP/trojan)      (h_tunnel 直发 POST/GET)
        现状零行为变化          新增，替代 BIND 流模式
```

- P2P 会话（runSession/peerWriteLoop）读写走接口，其余逻辑（hello/gossip/heartbeat 帧、mesh 注册、双队列）零改动。
- via 链的 CONN 流（`htunnelConn`）**一行不动**——它不经过 P2P 层。
- DialControl / DialReverse / DialPacket（UDP）不动。

### 2.1 数据报语义的合法性

P2P 会话上只有三种帧，全部容忍丢/重/乱序：

| 帧类型 | 容忍性 |
|--------|--------|
| FrameData（hello/gossip JSON） | 自包含周期性消息，丢失由下个周期（10-15s）补偿 |
| FrameHeartbeat | 空载荷，丢失由下个周期补偿 |
| FrameMeshPacket（overlay IP 包） | 端点 netstack TCP 自带重传/去重/排序 |

注意 FrameUDPChannel（0x04）只在 reverse 连接上流动，不经过 P2P 会话，直发模式无需支持。

## 三、Step 0：抽离 frame 包

`reverse` 包名实为"反向连接"，但统一帧协议（frame.go）历史原因住在里面，P2P 早已共用。抽成独立包，还清命名债：

- 新建 `frame/` 包：`reverse/frame.go` **整体搬移**（帧常量、MaxPayload、ReadFrame/WriteFrame、ReverseFramedConn、bytesBuffer），文件名不变。
- `reverse` 回归纯反向连接（registry/control/clientid），`import "phaethon/frame"`。
- 全局引用更新：`reverse.FrameX` → `frame.FrameX`、`reverse.ReadFrame/WriteFrame` → `frame.ReadFrame/WriteFrame`、`reverse.ReverseFramedConn` → `frame.ReverseFramedConn`、`reverse.MaxPayload` → `frame.MaxPayload`（涉及 p2p、server、dialer、reverse 四包，编译器兜底）。
- BindPort 常量留在 reverse（反向服务器语义）。

选 `frame` 而非 p2p 的原因：p2p → dialer 已有依赖，dialer 要实现接口必须 import p2p → 环；frame 只依赖 util，p2p/dialer/server/reverse 均已依赖 reverse（frame 从其中抽出），零新增环风险。

## 四、FrameTransport 接口

```go
// frame/transport.go
package frame

// FrameTransport 是 P2P 帧的传输抽象，数据报语义：
// 实现 MAY 丢帧、重复、乱序（消费方全部容忍）。
type FrameTransport interface {
    // Send 发送一帧。实现可异步缓冲（返回 nil ≠ 已上路），
    // 失败通过后续 Send/Recv 返回错误暴露。
    Send(frameType byte, payload []byte) error
    // Recv 阻塞收一帧。存活检测内建于实现
    //（stream: 60s 读 deadline；direct: 长轮询 GET 周期）。
    Recv() (frameType byte, payload []byte, err error)
    Close() error
}

// NewStreamTransport 把可靠有序的 net.Conn 适配为 FrameTransport。
// 完整保留现有 deadline 行为（写 5s/30s 按帧类型、读 60s）。
func NewStreamTransport(conn net.Conn) FrameTransport
```

streamTransport 行为 = 现状原样移植：

- `Send`：`SetWriteDeadline(5s；FrameMeshPacket 30s)` + `WriteFrame` + 清 deadline（p2p/p2p.go writeFrame 的逻辑随迁）。
- `Recv`：`SetReadDeadline(60s)` + `ReadFrame` + 清 deadline（runSession 的逻辑随迁）。

## 五、P2P 会话改造

| 位置 | 现状 | 改造 |
|------|------|------|
| `Peer.conn` | `net.Conn` | → `Peer.transport frame.FrameTransport` |
| `HandleP2PConn(conn, addr)` | 直用 conn | 保留签名（server 侧 BIND 入口），内部 `HandleP2PTransport(NewStreamTransport(conn), addr)` |
| 新增 `HandleP2PTransport(t, addr)` | - | Peer 构造 + session 启动（原 HandleP2PConn 函数体） |
| `StartPeer` :318 | `DialP2P() → net.Conn` | `DialP2P() → FrameTransport`（接口签名变更） |
| `runSession` :394 | `SetReadDeadline + reverse.ReadFrame(peer.conn)` | `peer.transport.Recv()` |
| `peerWriteLoop.writeFrame` | deadline + `reverse.WriteFrame(peer.conn,...)` | `peer.transport.Send(req.frameType, req.data)`（MeshPacket 日志保留在循环里） |
| `sendHeartbeats`/`enqueueWrite`/双队列/mesh 注册 | - | 零改动 |

`P2PDialer` 接口（dialer/dialer.go:101）签名变更：

```go
type P2PDialer interface {
    DialP2P() (frame.FrameTransport, error)
}
```

- `TrojanDialer.DialP2P` / `Socks5Dialer.DialP2P`：现有 conn 用 `frame.NewStreamTransport(conn)` 包装返回。
- `HTunnelDialer.DialP2P`：**切换为直发模式**（第六节）。

## 六、h_tunnel 直发协议（wire 规范）

### 6.1 通道建立：一次 HEAD

```
HEAD {url}/{connSeq=0}
  X-C: MESH
  （无 X-H/X-P —— 无目标地址语义）
→ 200 + X-I: {connectionID}    服务端创建 mesh channel，不拨任何目标
```

无 step2/step3 push/ack，无目标拨号，无 splice。

### 6.2 数据面：POST（客户端 → 服务端）

```
POST {url}/{connectionID}/{seq}      seq 仅用于日志，服务端不校验纪律
  X-I: {connectionID}
  Body: SealBody( 帧批次 )
```

- **帧批次** = 若干帧的 `WriteFrame` 输出（3 字节头 + payload）直接拼接。服务端 `OpenBody` 后用 `frame.ReadFrame` 循环解析。
- **两车道并发**（共享 client 连接池使并发 POST 零成本）：
  - **数据道**（FrameMeshPacket）：批量发送。v1 §3.2 算法移植：in-flight 收集（POST 在飞时新到的帧追加 pending，POST 返回立即发下一批，零定时器）、单批 ≤ 512KB（nginx 默认 `client_max_body_size 1m` 余量）、pending 高水位 1MB（超过则 Send 阻塞背压）。
  - **控制道**（非 FrameMeshPacket）：到达即单独 POST（阻塞 ~1 RTT），**永不排队在数据批后面**——控制帧优先级在 htunnel 层的落实。
  - 两车道并发 → 批次间可能乱序/（失败重连场景）丢失，由第二节的数据报语义兜住。
- 错误语义：数据道 POST 失败异步暴露（writeErr 于后续 Send/Recv 返回）；控制道 POST 失败当场返回。任一失败 → P2P 会话结束 → StartPeer 重连。

### 6.3 数据面：GET 长轮询（服务端 → 客户端）

```
GET {url}/{connectionID}/{seq}
  X-I: {connectionID}
  X-W: {waitTime}                    客户端控制服务端等待时间（v0.2.0 新增）
→ 200 + Body: SealBody(帧批次)     服务端从 outbound 取首批 + 非阻塞凑批（≤maxBatchSize）
→ 408                              waitTime 无帧，客户端重试
→ 410                              通道已关闭
```

**连接池设计（v0.2.0）**：

每个通道维护固定连接池（默认 8 个连接），分配策略：
- **2 条固定 GET 长轮询**：始终用于接收数据，交替工作
- **1 条固定心跳 PUT**：定期发送保活
- **其余连接机动**：用于 POST 发送数据

**GET 动态扩展**：
- 基础：2 条 GET 交替长轮询
- 如果某次 GET 返回数据达到 `maxBatchSize`（说明流量大），增加 GET 数量（最多 8 条）
- 增加滞后机制：连续 3 次满批次才增加，连续 10 次空批次才减少
- 额外的 GET 快速返回（短等待时间），不阻塞

**客户端控制等待时间**：
- 客户端通过 `X-W` 头告诉服务端等待多久（默认 5s）
- 高流量时：短等待（1s），快速返回，降低延迟
- 低流量时：长等待（30s），节省请求
- 自适应算法：根据上次批次大小调整下次等待时间

**帧批次上限（可配置）**：
- 默认 512KB（低于 nginx 1MB 限制）
- 可通过配置调整：`mesh.maxBatchSize`

### 6.4 心跳与通道生命周期

**心跳（v0.2.0 新增）**：
```
PUT {url}/{connectionID}/{seq}
  X-I: {connectionID}
→ 200                              心跳成功
→ 410                              通道已关闭
```

- 独立心跳请求，保持功能分离
- 默认间隔 30 秒，可配置：`mesh.heartbeatInterval`
- 心跳失败 → 通道关闭 → P2P 会话重连

**通道生命周期**：
- **DELETE**（客户端 Close）：关闭通道；未发出的 pending 丢弃（上层重连重建，与现状 conn 死亡等价）。
- **服务端超时**：复用 `resetReqTimeout`（POST/GET/PUT 活动续期）；mesh channel 无 targetConn，初始 10s 超时判定视为 ready。

### 6.5 加密与伪装

与现有协议完全一致：X-* 头加密（SealHeader）、body 整批加密（SealBody，XChaCha20-Poly1305）、URL 形态不变。DPI 视角：普通 HTTP POST/GET 携带不透明 blob，伪装形态无变化。

## 七、实现结构

### 7.1 客户端 `dialer/htunnel_direct.go`

```go
type htunnelDirectTransport struct {
    proxy        *config.Proxy
    connectionID string
    client       *http.Client      // 共享 client（v1 §3.1）
    crypto       *util.HTunnelCrypto

    // 连接池配置（v0.2.0）
    poolSize     int               // 总连接数（默认 8）
    getSlots     int               // 固定 GET 数（默认 2）
    
    // 数据道批量状态（pendMu 保护，v1 §3.2 算法）
    pendMu   sync.Mutex
    pending  []byte   // 明文帧拼接
    sending  bool
    writeErr error
    // cond：背压唤醒 + Close 唤醒

    ctrlMu    sync.Mutex   // 控制道串行
    closed    chan struct{}
    closeOnce sync.Once

    // 读侧（readMu 保护）
    readBuf    []byte      // 最近一次 GET 的解密帧流
    readOffset int
    
    // GET 动态扩展（v0.2.0）
    getMu         sync.Mutex
    activeGETs    int           // 当前 GET 数量
    fullCount     int           // 连续满批次计数
    emptyCount    int           // 连续空批次计数
    waitTime      time.Duration // 当前等待时间（自适应）
    
    // 心跳（v0.2.0）
    heartbeatInterval time.Duration
    heartbeatStop   chan struct{}
}
```

**GET 动态扩展算法**：
```go
func (t *htunnelDirectTransport) adjustGETCount(batchSize int) {
    t.getMu.Lock()
    defer t.getMu.Unlock()
    
    maxBatch := t.config.MaxBatchSize
    
    if batchSize >= maxBatch {
        t.fullCount++
        t.emptyCount = 0
        if t.fullCount >= 3 && t.activeGETs < t.poolSize-2 {
            t.activeGETs++
            go t.recvLoop(false)  // 启动额外 GET
        }
    } else if batchSize == 0 {
        t.emptyCount++
        t.fullCount = 0
        if t.emptyCount >= 10 && t.activeGETs > t.getSlots {
            t.activeGETs--
            // GET 自然退出（不重启）
        }
    } else {
        t.fullCount = 0
        t.emptyCount = 0
    }
}
```

**等待时间自适应**：
```go
func (t *htunnelDirectTransport) adjustWaitTime(batchSize int) {
    t.getMu.Lock()
    defer t.getMu.Unlock()
    
    maxBatch := t.config.MaxBatchSize
    
    if batchSize >= maxBatch {
        t.waitTime = 1 * time.Second   // 高流量：短等待
    } else if batchSize > maxBatch/2 {
        t.waitTime = 3 * time.Second
    } else if batchSize == 0 {
        t.waitTime = 30 * time.Second  // 低流量：长等待
    } else {
        t.waitTime = 5 * time.Second   // 默认
    }
}
```

### 7.2 服务端 `server/htunnel_mesh.go`

```go
// htChannel 增加字段：isMesh bool, meshIn/meshOut chan meshMsg
type meshMsg struct { frameType byte; payload []byte }

// 桥接：服务端侧 FrameTransport（喂给本地 P2P manager）
type meshChannelTransport struct { ch *htChannel }
// Send → meshOut（阻塞，背压）；Recv → meshIn（阻塞，通道关闭返回错误）

// POST handler: OpenBody → ReadFrame 循环 → meshIn（有界 4096，满则阻塞）

// GET handler（v0.2.0 更新）:
//   1. 读取 X-W 头获取客户端指定的等待时间（默认 5s）
//   2. 等 meshOut 首帧（waitTime 超时 408）
//   3. 非阻塞凑批（≤maxBatchSize）
//   4. 返回帧批次

// PUT handler（v0.2.0 新增）:
//   1. resetReqTimeout（续期）
//   2. 返回 200
```

通道建立后 `go handleP2PTransport(meshChannelTransport{ch}, "mesh:"+connID)`（server/p2p_server.go，直调 p2p 包，跳过 reverse server splice）。

### 7.3 共享 http.Client（v1 §3.1，随本变更落地）

`sync.Map` 按 `proxy.Name` 缓存 + sig（server/port/url/Next 指针）失效重建；池参数 `MaxIdleConnsPerHost=16`、`MaxIdleConns=64`、`IdleConnTimeout=120s`（> 心跳间隔，虽然直发模式无心跳，仍大于 GET 周期）。legacy `htunnelConn` 一并切换到共享 client。

### 7.4 配置参数（v0.2.0 新增）

```yaml
# config.yaml
mesh:
  # 连接池大小（默认 8）
  poolSize: 8
  
  # 固定 GET 长轮询数（默认 2）
  getSlots: 2
  
  # 帧批次上限（字节，默认 512KB）
  maxBatchSize: 524288
  
  # 心跳间隔（默认 30s）
  heartbeatInterval: 30s
  
  # GET 等待时间范围
  minWaitTime: 1s
  maxWaitTime: 30s
  defaultWaitTime: 5s
```

## 八、兼容与部署

**隔离原则（v0.2.0 强调）**：
- ✅ **传统模式零影响**：所有改动仅针对 mesh 模式（`isMesh=true`）
- ✅ **代码路径分离**：mesh 和传统模式使用不同的 handler 和 transport
- ✅ **配置独立**：`mesh.*` 配置只影响 mesh 模式

**具体隔离措施**：
| 组件 | 传统模式 | Mesh 模式 | 隔离方式 |
|------|---------|-----------|---------|
| PUT 心跳 | handleHeartbeat（序列号控制） | meshHandleHeartbeat（简单续期） | `if ch.isMesh` 分支 |
| GET 长轮询 | 序列号 + leaderOrWait | waiter 队列 + X-W 等待时间 | `if ch.isMesh` 分支 |
| POST 写入 | 序列号 + 写入 targetConn | 帧批次 → meshIn 队列 | `if ch.isMesh` 分支 |
| Transport | htunnelConn（BIND 流） | htunnelDirectTransport | 不同类，零交集 |

### 8.2 Gossip 链路标识变更（v0.2.0）

**变更内容**：
- 移除 `GossipLink.LinkID` 字段
- 新增 `GossipLink.LocalSeq` 和 `GossipLink.RemoteSeq` 字段
- `GetFriendlyName()` 不再拼接 seq（从 `"MGMS_HT#3"` 改为 `"MGMS_HT"`）

**新格式**：
```json
{
  "localSeq": 3,        // 本端声明的 seq
  "remoteSeq": 5,       // 对端声明的 seq
  "friendlyName": "MGMS_HT",
  "srtt": 18.5,
  "lossRate": 0.02
}
```

**理由**：
1. **透明性**：直接暴露底层 seq 编号，调试更直观
2. **灵活性**：匹配逻辑按需组合，不受固定格式限制
3. **对称性**：两端各自声明 seq，对等关系更清晰
4. **简洁性**：友好名称不再拼接 seq，显示更干净

**匹配逻辑**：
- 需要链路匹配时，组合为 `linkId = fmt.Sprintf("%d-%d", localSeq, remoteSeq)`
- 列表展示时，分开显示 `localSeq` 和 `remoteSeq`

**影响范围**：
- `mesh/topology.go`：GossipLink 结构定义
- `mesh/mesh.go`：broadcastGossip 填充逻辑
- `p2p/p2p.go`：GetFriendlyName 方法
- Admin UI：链路展示逻辑（如有）

**部署兼容性**：
- 需要所有节点同时升级（gossip 格式变更）
- 旧节点无法解析新格式，会丢失链路质量信息

### 8.3 P2P 开关语义（v0.2.0 讨论结论）

**配置示例**：
```yaml
- name: MGMS_HT
  type: h_tunnel
  url: http://36.140.28.178:18080/ui/resdata/
  password: "${MGMS_HT_PASSWORD}"
  p2p: false  # 禁用 P2P，回退到传统 BIND 模式
```

**行为定义**：
| P2P 开关 | 模式 | 链路可见性 | 中继能力 | 效率 |
|---------|------|-----------|---------|------|
| `p2p: true`（默认） | Mesh 直发模式 | 链路信息公开，参与 gossip 广播 | 可为其他节点中继 | 高效（HEAD + POST/GET/PUT） |
| `p2p: false` | 传统 BIND 模式 | 链路私有，不广播 | 仅服务本节点 | 较低（BIND 握手 + 目标拨号） |

**设计讨论**：

曾考虑"私有 Mesh 模式"：`p2p: false` 时仍使用 Mesh 直发模式，但不广播链路信息。

**结论**：不采用。理由：
1. **权衡清晰**：当前设计让用户在"效率"和"隐私"间明确选择
   - 要效率 → 开 P2P（接受链路公开和中继）
   - 要隐私 → 关 P2P（接受传统模式的低效）
2. **语义简单**：`p2p: false` 的含义一致——不参与 P2P mesh 网络
3. **实现已完整**：`main.go:393` 和 `main.go:2038` 已检查 `IsP2P()`，禁用时不调用 `StartPeer`
4. **无额外复杂度**：不需要"private"标记或 gossip 过滤逻辑

**代码路径**：
- `config.go:86-98`：`IsP2P()` 检查 `p.P2P` 字段，h_tunnel 可配置
- `main.go:393-396`：启动时检查，禁用时跳过 `StartPeer`
- `main.go:2038`：配置热重载时检查，动态启停 P2P peer
- `dialer/htunnel.go:132`：`Dial()` 方法始终可用（BIND 模式，传统代理流量）
- `dialer/htunnel.go:241`：`DialP2P()` 仅在 P2P 启用时被调用（Mesh 直发模式）

**部署兼容性**：
- **服务端双模式并存**：MESH channel（新）与 BIND 流 channel（现有代码路径原样保留）共存。
- **部署顺序**：JF 先升（旧 QG 客户端走 BIND 不受影响）→ QG/VM 升级后自动切 MESH。**无客户端回落逻辑**。
- 升级窗口内功能零损失（旧路径持续工作）。
- 全部环境升级后，可另行移除 h_tunnel 的 BIND P2P 路径（不在本变更范围）。

## 九、范围外（明确不做）

| 项 | 说明 |
|------|------|
| via 链 CONN 流（`htunnelConn`） | 一行不动；其批量优化按 v1 文档 §3.2 另行跟进 |
| DialControl / DialReverse | 保持 BIND 流模式 |
| DialPacket（UDP over htunnel） | 不动 |
| p2p v0.3.0（socket buffer + 去 deadline） | 独立变更，在 streamTransport 落地后实施 |
| 并发数据道 POST（>1 in-flight 数据批） | 本版单 in-flight；乱序容忍已内建，未来可直接放开 |

## 十、文件变更清单

| 文件 | 变更 |
|------|------|
| `frame/frame.go` | 新增（自 reverse/frame.go 整体搬移，包名 frame） |
| `frame/transport.go` | 新增：FrameTransport 接口 + NewStreamTransport |
| `reverse/*.go` | 删 frame.go；import frame，引用更新 |
| `p2p/p2p.go` | Peer.conn → transport；HandleP2PTransport 新增；runSession/peerWriteLoop 走接口；StartPeer 适配；**修复 RestartPeer 竞态** |
| `dialer/dialer.go` | P2PDialer.DialP2P 返回 FrameTransport |
| `dialer/trojan.go` `dialer/socks5.go` | DialP2P 包装 NewStreamTransport |
| `dialer/htunnel.go` | DialP2P 切直发；共享 client（§7.3） |
| `dialer/htunnel_direct.go` | 新增：htunnelDirectTransport；**v0.2.0：连接池、GET 动态扩展、心跳、自适应等待** |
| `server/htunnel.go` | X-C: MESH 分支；channel 增加 isMesh/meshIn/meshOut；POST/GET/PUT 按类型分发 |
| `server/htunnel_mesh.go` | 新增：mesh channel handler + meshChannelTransport；**v0.2.0：GET 读取 X-W 头、PUT 心跳处理** |
| `server/p2p_server.go` | 新增 handleP2PTransport |
| `config/config.go` | **v0.2.0：新增 MeshConfig 配置结构** |
| `mesh/topology.go` | **v0.2.0：GossipLink 移除 linkId，改为 localSeq + remoteSeq** |
| `mesh/mesh.go` | **v0.2.0：broadcastGossip 使用 localSeq/remoteSeq** |
| `p2p/p2p.go` | **v0.2.0：GetFriendlyName 不再拼接 seq** |
| `docs/index.md` | 登记本文档 |

## 十一、验证方案

1. **构建与既有测试**：`go build ./...`、`go vet ./...`、现有单测 + e2e（reverse/dialer）零回归——重点覆盖 StreamTransport 与旧路径等价性。
2. **P2P over trojan/direct 回归**：StreamTransport 包装后，现有 P2P 会话（hello/gossip/mesh 数据/心跳）行为不变。
3. **直发模式联调（VM + QG）**：
   - VM/QG 建立 P2P（经 h_tunnel）→ 日志确认 `X-C: MESH` 一次 HEAD 建通道、无三步握手、PUT 心跳正常、路由立即建立（hello 即时）。
   - mesh 数据传输（pkg 推送）经直发链路，吞吐对比旧 BIND 流模式。
   - 控制帧不被数据批阻塞：批量传输中观察 gossip/heartbeat 帧延迟。
4. **v0.2.0 新特性验证**：
   - **连接池**：确认只有 2 条 GET 长轮询（基础），日志显示连接池状态。
   - **心跳独立**：确认 PUT 心跳定期发送（默认 30s），与 GET/POST 分离。
   - **GET 动态扩展**：高流量场景下观察 GET 数量增加（最多 8 条），低流量时减少回 2 条。
   - **自适应等待**：高流量时等待时间短（1s），低流量时长（30s）。
   - **帧批次上限**：确认批次不超过 maxBatchSize（默认 512KB）。
5. **竞态修复验证**：
   - 多次重启 P2P peer，确认不会出现重复连接。
   - 配置变更触发 RestartPeer，确认旧连接正确清理。
6. **韧性**：中途断网/杀服务端 → 客户端 GET/POST 失败 → 会话结束 → 自动重连；DELETE 后服务端通道清理。
7. **滚动升级**：JF 升级后旧 QG（BIND 模式）P2P 正常；QG 升级后日志切换到 MESH 模式。
8. **伪装**：抓包确认 POST/GET/PUT 形态与现有流量一致（URL 模式、加密 body）。
