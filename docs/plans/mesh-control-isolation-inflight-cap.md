# Mesh 链路：控制面隔离 + 在途量上限

**状态**: DRAFT（待用户确认）
**创建**: 2026-11-07
**问题来源**: `docs/issues/vm-gg-ssh-proxy-link.md`
**相关方案**: `mesh-multilink-stripe-design.md`（stripe lane）、`htunnel_p2p_direct_mode.md`（HT v2）

## 1. 背景

VM→GG（trojan via SSH 跳板）压测证据：
- VM 进程内队列 <256KB、SSH keepalive <1s、GG Recv-Q=0，瓶颈在跳板→GG 段（≈46KB/s，丢包重）
- VM 与 GG 之间压住 ≈2MB+（sshd channel 窗口 + 跳板内核缓冲），全在我们进程外面
- 同一条连接上的 ping/DNS/ACK 跟着排几十秒 → `link dead held`、DNS 5s 超时、链路被判死重建

问题本质是两个：
1. **控制面和大流量挤同一条连接**（控制帧排在别人的缓冲后面，进程内优先级无效）
2. **进程外积压不受控**（内层 TCP 只看到 RTT 涨到几十秒，不丢包，不会降速，反而超时重传浪费带宽）

## 2. A：控制面隔离（专用数据 lane）

### 2.1 规则
- 满足触发条件的链路，主动方会话建立后**立即**拨一条**专用数据 lane**（复用现有 `dialStripeLane` + StripeAttach，被动方无需改协议）
- 专用 lane 存在时：所有可拆分数据帧（`splittable` = 非 DNS 的 FrameMeshPacket）**只走数据 lane**，主连接只走控制帧、DNS、ping/pong
- 动态 lane（拥塞控制器加的）照旧参与分流，但主连接不再参与分流（rendezvous 候选集去掉 primary）

### 2.2 触发条件（自动，无配置）
主连接的底层 carrier **没能设上 TCP_NOTSENT_LOWAT**（`frame.limitKernelBacklog` 走到 skipped 分支）：
- 经代理/中继的 carrier（SSH channel、via 链、非 TCP socket）
- Windows 上所有链路（不支持 NOTSENT_LOWAT）

能设上的（Linux 直连 TCP）积压本来就留在进程内 DRR，控制帧已能优先，不开。
实现：`limitKernelBacklog` 返回 bool，dialer 侧把结果挂到 transport 上供 p2p 读取。

### 2.3 专用 lane 的生命周期
- 专用 lane **不受**降级剔除（`srtt≥1s / loss≥50%` 那条）和 5 分钟空闲回收约束
- 专用 lane 写/读失败被移除 → 数据回落主连接（现有行为），控制器 5s 后重拨；重拨失败指数退避（5s→60s 封顶）
- `laneEnqueue` 满等待 50ms 后改走主连接的回落：专用 lane **不回落**（回落就又混到一起了），改为由 §3 的在途门控在 DRR 前拦住

### 2.4 ping 语义
ping 仍按 Phase C 进数据 DRR 队列（测进程内数据面排队），出 DRR 后走主连接。
§3 把积压拉回进程内后，ping 的 SRTT 仍能反映数据面拥塞，路由代价计算不受影响。

### 2.5 不覆盖
HT v2（POST/GET 池）不走 stripe（p2p.go:1468 明确排除），由 §4 单独处理。

## 3. B：在途量上限（所有 TCP 类链路通用）

### 3.1 计量
- 发送方：每条 lane（主连接 laneID=0，其余用 stripeID）累计 `tx` = 成功交给 transport 写出的帧 payload 字节
- 接收方：每条 lane 累计 `rx` = 从该 lane 读到的帧 payload 字节
- **在途量** = `tx − ackedRx`，就是压在 carrier 缓冲、跳板、网络里的数据

### 3.2 回报帧与能力协商

- 新帧类型 `FrameLaneAck = 0x17`，二进制：重复 `{laneID uint32, rx uint64}`（12B/条）
- **逐链路请求协商**（取代原 gossip `lack:true` 全局通告）：
  - 发送方在第一次发 MeshPacket 时懒发送一个空的 FrameLaneAck（payload 为空）作为"请回报"
  - 接收方看到空 payload 就设置 ackRequested，之后每 50ms 检查 rx 变化并回报
  - 旧 peer 收到未知帧 0x17 会 log 一次 Info 并忽略，不影响会话
  - 发送方收到首个 ack 才对该 lane 启用门控 → 新老版本混跑时行为与现在一致
- 回报走主连接、control 优先级；最多 64 个条目（8MB 上限）

### 3.3 上限（延迟驱动公式）

原方案 `cap=clamp(2×rate×max(baseRTT,50ms),64KB,4MB)` 有两个发散问题：
- baseRTT 在我们自己的队列膨胀时会虚高，导致 cap 越涨越大
- 纯速率上限在 RTT 超过目标时会崩溃

改用 **LEDBAT-like 延迟驱动公式**：
```
minRTT      = 过去 10 分钟每分钟的最小 RTT（滑动窗口）
qd          = rtt − minRTT           // 排队延迟
target      = 200ms                  // 目标排队延迟

if qd > target:                      // 拥塞：减速
    cap *= max(0.5, target/qd)
elif qd < target/2 and busy:         // 空闲：加速（busy = inflight ≥ cap/2 自上次调整）
    cap *= 1.25

cap = clamp(cap, 64KB, 8MB)         // 边界
initial_cap = 256KB                  // 初始值
```

- **停滞防护**：ack 无进展 10s 且 inflight > 0 → 解除门控并告警 `[LANE-STALLED]`
- ×1.25 加速步长保守（同 LEDBAT off-target gain），避免震荡
- 快链路（10MB/s、20ms）→ qd ≈ 0，cap 维持在 8MB；VM→GG（46KB/s、rtt≈55ms）→ qd≈55ms，cap 降至 ~256KB，进程外排队 ≈5.5s（现状 45s+）

### 3.4 门控位置（关键）
在 `peerWriteLoop` 出队处，不在 transport 写入处：
- 数据可写额度不足时，`peerWriteLoop` **暂停从 drrOutput 取数据**，但继续取 controlCh / dnsQueue
- 额度 = 数据 lane 们的总在途 < 总 cap（有专用 lane 时只算数据 lane；否则算主连接）
- ack 到达 / 写完成时通知 `peerWriteLoop` 重新检查
- 效果：多出的数据留在 DRR 队列（公平调度、8MB 预算、满了丢包 → 内层 TCP 正常降速），控制帧永不被门控卡住

### 3.5 不覆盖
UDP 数据腿（不经 TCP 缓冲，自带机制）；HT v2 本期不接门控（§4 先解决控制面混排）。

## 4. C：HT v2 控制帧与数据帧分开

### 4.1 现状问题（Agent 审查结果）

**主动端（客户端 QG）**：
- `Send/SendBatch` 忽略 `isControl`，控制帧和数据塞同一个 POST body，排在 16 个 POST 名额后面
- `sendSem` 16 槽并发，新 pong/gossip 等所有 16 槽满时阻塞
- POST 超时 10s，失败静默丢失（仅 debug log），无重试
- GET `dataRecvCh`（64）满了会阻塞整个读循环，同一 GET body 后面的控制帧跟着等
- `io.ReadAll` 错误被忽略，512KB body 整体 AEAD 解密，慢链路下头帧延迟可达 ~20s（46KB/s）
- GET 超时基于 wait + 10s，慢 body 截断后解密失败直接拆会话

**被动端（服务端 JF）**：
- POST 入向 `meshIn` 单 FIFO（4096），`Recv` 无控制优先
- 16 个 POST handler 并发写入，帧重排序
- egress `meshOut`（4096）+ `meshCtrlOut`（256），但 peerWriteLoop 在 8MB gate 关闭时阻塞，pong 排队
- CoDel 几乎无效：每批最多丢一帧，sojourn 时间不含 meshOut 排队和传输时间
- DNS 走数据 FIFO，可被 CoDel 丢弃

**根因**：
1. **GET 队头阻塞**：512KB body 整体解密，头帧（如 pong）不可见直到整个 body 到达
2. **POST 重排序**：16 槽并发 + handler 交错写入
3. **控制帧混排**：无隔离通道，pong/DNS 排在大数据后面
4. **缓冲失控**：~1.5MB（POST）+ 6MB（GET）+ 8MB（gate）进程外积压

### 4.2 修复方案（按优先级）

#### P1：客户端控制 POST（立即可做，兼容旧服务端）
- `isControl=true` 或 batch 中有 Control 帧 → 单独小 POST
- 专用信号量 1–2 槽（不占 `sendSem` 16 槽）
- 超时 5s，失败上报给 session（触发链路重建）
- wire format 不变（3B header + payload，整体 XChaCha20）

#### P2：服务端入站分流（server-only，兼容旧客户端）
- `meshHandleWrite` 中路由非 FrameMeshPacket 和 DNS 到 `meshCtrlIn`
- `Recv` 先服务 `meshCtrlIn`，再服务 `meshIn`
- handler 阻塞时限（如 2s），超时丢弃并告警

#### P3：GET 控制隔离
- **Server-only 第一步**：ctrlQ 非空时批次限制为控制 + ~16KB 数据
- **后续**：专用控制 GET，新请求头标记；旧服务端忽略头，保持现状；新服务端仅在客户端发过头时才切换

#### P4：客户端 recv 修复
- 检查 `ReadAll` 错误，非 200 重试 transient 5xx（指数退避）
- 基于 body 进度设超时（而非 wait 时间）
- 解析完整个 body 后先交付控制帧，再交付数据

#### P5：按字节限大小
- GET 批次自适应：`clamp(rate × 200ms, 16KB, 512KB)`
- POST 按在途字节限流，减少并发槽数（2–4 大槽代替 16 小槽）降重排序

#### P6：DNS 优先级（服务端）
- `meshChannelTransport.Send` 中用 `isDNSPacket` 分类
- 新增 DNS 队列，在控制之后、数据之前服务
- 不被 CoDel 丢弃

#### P7：HT v2 作为 laneID 0 纳入 LaneAck 门控
- HT v2 单链路视为 laneID 0，用 §3 的计量和门控覆盖
- ack 必须走隔离控制路径（P1–P3），否则 ack 延迟会节流链路
- 损失记账：发送方需计入已知损失（失败 POST 字节、服务端 CoDel 丢弃），否则 `tx − ackedRx` 泄漏导致门控楔住
- 规则：ack 持续到达但 `ackedRx` 不动超过 max(4×baseRTT, 2s) → 重置 baseline
- gossip 通告 `lack:true`，旧 peer 不收到未知帧
- 一旦门控生效，服务端批次自然 ≤ cap，512KB 队头问题解决；8MB cap 和 CoDel 成为后备

#### P8：CoDel 对称化
- 门控生效后，AQM 移到发送端 DRR 队列（两端）
- 若保留服务端 CoDel，需在帧进入 meshOut 时打时间戳，逐帧用真实时间评估

### 4.3 兼容性
- 保持 body format 不变（3B header + payload，整体 XChaCha20）
- 新行为通过请求头键控（旧服务端忽略）或 gossip flag
- 逐 chunk 密封需要 HEAD 请求中协商版本

### 4.4 与原设计的差距
原设计 `htunnel_p2p_direct_mode.md:147-151` 指定了独立控制通道，但未实现。本方案补齐该缺口，并解决 GET 侧队头阻塞、超时 teardown、DNS 优先级和门控损失记账问题。

## 5. 实施顺序
1. B 的计量 + 回报帧 + 能力协商（两端都要新版本才生效）
2. B 的门控
3. A 专用数据 lane
4. C HT v2
每步单测 + 本地编译通过后统一部署 VM/GG 验证，再部署其他节点。

## 6. 验收
- Mac→vm.phn 4 路并发下载 3 分钟：
  - VM/GG 不再出现 `link dead held` / `link dead`
  - 压测期间 `vm.phn` DNS 解析 <1s；对 vm.phn 的交互请求（`/api/version`）<3s
  - 总吞吐不低于现状（≈27KB/s）
- GG↔JF（直连 TCP，Linux）64MB 吞吐不低于现状
- QG→JF（HT v2）压测期间无 `link dead`，ping SRTT 不随数据量线性涨
- 新老版本混跑（只升一端）链路正常、无报错

## 7. 新增诊断日志
- `[LANE-INFLIGHT]` 每 5s（有门控时）：lane、inflight、cap、rate、baseRTT、门控累计等待时长
- `[STRIPE] dedicated data lane` 拨号/移除/重拨

## 8. 实施交接说明（B1 已完成，待 B2/A/C）

### 8.1 当前进度

**✅ B1 LaneAck 计量框架** (commit 61a80ae) 已完成并推送：
- `frame/frame.go`: FrameLaneAck = 0x17, KernelBacklogLimited interface
- `frame/notsent_lowat.go`: limitKernelBacklog 返回 bool
- `frame/transport.go`: streamTransport.lowat + KernelBacklogLimited() 方法
- `p2p/lane_flow.go`: laneFlow（tx/rx/onSent/onRecv/onAck），minRTT 桶，rate EMA，encode/decode
- `p2p/stripe_transport.go`: primaryFlow/lane flow, handleLaneAck, flowLoop, requestLaneAcks, sendControl/name 字段
- `p2p/p2p.go`: passive/active side sendControl/name 传入，FrameLaneAck 静默忽略
- 编译验证: `go vet ./p2p/` 通过

### 8.2 待实现任务（按优先级顺序）

**🔴 B2: peerWriteLoop 在途门控 (§3.4)** — 必须先完成才能做 A

目标：在 `peerWriteLoop` 出队处拦住超额数据，让多余数据留在 DRR 队列（公平调度、8MB 预算、满了丢包 → 内层 TCP 正常降速），控制帧永不被卡住。

关键步骤：
1. 在 `laneFlow` 添加 `cap int64` 字段和 `adjustCap(now)` 方法（LEDBAT-like 公式：qd > 200ms → cap ×= max(0.5, 200ms/qd)；qd < 100ms 且繁忙 → ×1.25；边界 64KB–8MB）
2. 在 `flowLoop` 中每 200ms 调用 `adjustCap`
3. 修改 `peerWriteLoop`：计算 `credit = sum(caps) - sum(inflights)`，credit ≤ 0 时不选 drrOutput，但仍服务 controlCh/dnsQueue
4. 停滞防护：10s 无 ack 进展且 inflight > 0 → 解除门控并告警 `[LANE-STALLED]`

关键文件：`p2p/p2p.go:peerWriteLoop` (~line 1650), `collectWriteBatch` (~line 774), `p2p/lane_flow.go`

验收：压测 VM→GG 时 `[LANE-INFLIGHT]` 显示 inflight 稳定在 64–256KB，不再出现 `link dead held`

---

**🔴 A: 专用数据 lane (§2)** — 必须在 B2 完成后才能实现

目标：当主连接无法设上 TCP_NOTSENT_LOWAT 时（代理/中继/Windows），主动方拨一条专用数据 lane，所有可拆分数据帧只走该 lane，主连接只走控制/DNS/ping。

关键步骤：
1. dial 后检查 `frame.KernelBacklogLimited`，未实现或返回 false → 触发专用 lane
2. 3s 延迟后拨号，固定 stripeID = 0xFFFFFFFF，失败退避 5s→60s
3. `pickLanePinned` 排除 primary（候选集只剩专用 lane + 动态 lanes）
4. 专用 lane 豁免降级剔除和 5 分钟空闲回收
5. `laneEnqueue` 满时不回落主连接（由 B2 门控保证不会塞满）

关键文件：`p2p/p2p.go` dial path (~line 1480), `p2p/stripe_transport.go` attachLane/removeLane/controllerTick/pickLane

验收：VM→GG 链路建立后 ~3s 看到 `[STRIPE] dedicated data lane dialed`，压测期间主连接 Recv-Q ≈ 0

---

**🟡 C-P1/P2: HT v2 控制帧分离 (§4)** — 可独立于 B2/A 实施

**P1: 客户端控制 POST**
- isControl 帧单独小 POST，专用信号量 1–2 槽（不占 sendSem 16 槽），超时 5s
- 关键文件：`dialer/htunnel_direct.go`

**P2: 服务端入站分流**
- meshHandleWrite 中非 FrameMeshPacket 和 DNS 进 meshCtrlIn，Recv 先服务 meshCtrlIn
- handler 阻塞时限 2s，超时丢弃并告警
- 关键文件：`server/htunnel_mesh.go`

验收：QG→JF 压测期间 ping SRTT 不随数据量涨，服务端日志无 handler 阻塞 >2s

---

### 8.3 实施顺序与依赖

```
B2 (gate) → A (dedicated lane) → C-P1/P2 (HT v2 control separation)
```

原因：A 的专用 lane 队列（256 批次）需要 B2 的门控来防止进程内积压。C 可以独立实施，但建议等 B2 稳定后再做。

### 8.4 已知风险

1. **B2 cap 公式可能过于保守**：快链路可能被限流，需通过压测调整参数（target=200ms, ×1.25 加速步长）
2. **A 专用 lane 重拨风暴**：连续失败后退避到 60s 仍可能影响体验，考虑增加"放弃专用 lane"逻辑
3. **HT v2 兼容性**：旧服务端不认识 `X-P2P-Control` 头，控制帧仍会排队，需通过 gossip 协商能力
4. **laneFlow.cap 并发访问**：adjustCap（写）和 peerWriteLoop（读）之间需要原子访问或加锁，考虑用 atomic.Int64
5. **停滞防护误触发**：慢链路（46KB/s）10s 无 ack 可能是正常的，需区分"慢但健康"和"真正停滞"

### 8.5 参考文档

- 问题跟踪: `docs/issues/vm-gg-ssh-proxy-link.md`
- Stripe 设计: `docs/plans/mesh-multilink-stripe-design.md`
- HT v2 设计: `docs/plans/htunnel_p2p_direct_mode.md`

**最后更新**: 2026-11-07 | **下一个接手者**: 请阅读本节 + §2/§3/§4，按优先级顺序实施
