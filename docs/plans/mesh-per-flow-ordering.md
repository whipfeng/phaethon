# mesh 每流有序化改造 — 消除隧道层乱序源（2026-11-06）

| | |
|---|---|
| 状态 | **§3.10（2026-11-06）撤销 h_tunnel 有序模式（§3.1/§3.2/§3.5/§3.7 作废，以 §3.10 为准）+ IPIP 按内层分类，已部署 JF/QG/DESKW（v0.8.0-329），elephant 测速变慢未达标；追加 3.10.6 改动三（去 ackQueue + 写循环合批），代码已实现、单测通过，待部署验收**。历史：已部署（v0.8.0-325-g2361df1）；但**残余乱序未消**（QG rcv_ooopack ~45%），滴流仍在（单流 173~726KB/s）——三钉死代码在场 ≠ 每个环节运行时生效，残余乱序源定位转 [v5 取证计划](mesh-reorder-forensics-v5.md)（2026-11-06 已批准） |
| 关联 | [elephant 下载滴流专项 v4](../issues/elephant-download-dribble.md)（根因实锤）、[发送端取证计划](mesh-tcp-reorder-forensics.md)（已完成） |

## 一、问题（一句话）

mesh v2 隧道层把同一条内层 TCP 流的帧拆到多路并发 POST / 多条 GET recvLoop / 多条路由上无序传输，接收端 ~50% 分段乱序（QG rcv_ooopack 实锤），内层发送端 dup-ACK → 快速重传 → cwnd 塌缩到 1~2 段（JF gVisor 计数器差分实锤：fast_retransmit +28 / spurious +31 / dsack +35 / 481 段）。每流 40~210KB/s 恒定滴流。

## 二、设计原则

**让乱序在构造上不可能，而不是事后重组。** 内层 netstack 交付给 mesh 的帧天然有序；有序管道只是把 mesh 偷走的有序性还回去——不重蹈 mesh 层加重组缓冲越俎代庖的覆辙（用户立场）。

## 三、改动（三处钉死 + 批策略微调，不改协议）

### 3.1 POST 方向：每流在途独占（主要工作量）

**现状**：`p2p.peerWriteLoop`（p2p/p2p.go:614-783）从 `drrOutput` 合批（≤64 帧）→ `transport.SendBatch` → `htunnelDirectTransport.SendBatch`（dialer/htunnel_direct.go:149）整体一个 POST，sendSem=16 路并发。同一内层流的相邻帧落进不同在途批次 → 到达即乱序。

**改法**：
1. 帧入批前取流键（FrameMeshPacket 载荷 = IP 包，键 = srcIP:srcPort:dstIP:dstPort:proto，复用 DRR 已有哈希逻辑，p2p.go drrLoop 的 256 队列即按流分桶）。
2. Peer 维护 `inFlightFlows map[flowKey]int`（引用计数）与"批次完成回调"：`SendBatch` 的异步 POST 完成（htunnel_direct.go:184 goroutine 内 postBatch 返回）时释放批内全部流键。
3. `peerWriteLoop` 合批时跳过在途流的帧（留在队列下轮再取），只从非在途流取帧成批；批发出时登记流键。16 路并发保留，但**同一流同一时刻至多一个批次在途** → 严格 FIFO。
4. 控制/ACK/DNS 快路径（controlCh/dnsQueue/ackQueue，p2p.go:703-749）不动：ACK 乱序无害（累积确认），控制帧低频。

**吞吐核算**：单流隧道速率 = 批次字节数/RTT。内流 cwnd 恢复后单流在途 ~512KB，10ms 级 RTT 上 ≥5MB/s；慢启动初期小批次即发（见 3.4），无鸡生蛋。

### 3.2 GET 方向：每流钉死投递链

**现状**：服务端 `meshDispatchLoop`（server/htunnel_mesh.go:120）把 `meshOut` 帧分发给任意空闲 waiter（dispatchToWaiter 取队首）→ `meshHandleRead` 合批 → GET 响应；客户端 2 条 recvLoop（dialer/htunnel_direct.go:128-131）各自投递同一 `dataRecvCh`。同流帧分上不同 GET，响应完成顺序乱。

**改法**：
1. `meshMsg` 增加流键字段（发送侧 meshChannelTransport.Send/SendBatch，server/htunnel_mesh.go:238-258，解析 IP 头取键，解析工具复用 mesh 包现有逻辑）。
2. `dispatchToWaiter` 改为按流键一致性哈希到 waiter 子集：同一流的帧永远进同一 GET 连接 → 响应天然有序。waiter 超时重建后哈希不变（按索引而非句柄）。
3. 客户端 recvLoop 无需改（有序性由服务端保证）。

### 3.3 路由：每流生命周期钉死单路径

**现状**：mesh 转发逐包选路（bestPaths/Dijkstra），抖动时同流帧可能经不同路径到达 → 路径间时延差制造乱序。

**改法**：转发层加每流路由缓存：五元组 → 下一跳，命中即用；路由表版本号变更时整表作废重选。换路瞬间已发出帧可能与新路帧交错——接受这一瞬态（远好于持续乱序），或实现"换路时等旧路在途 drain"（留作优化项，首版不做）。

### 3.4 批填充策略微调

每流独占后，原"攒批等待"（MinWait 1s/MaxWait 30s 见 dialer/htunnel_direct.go adjustWaitTime 与 server 侧首帧等待）在单流场景会引入不必要延迟。改为：**流的队列抽空即发，有多少发多少**（上限 MaxBatchSize 不变）。聚合吞吐由多流并发提供，不由单流攒批提供。

**实施注记**：3.1 的有序调度器本就在 POST 槽释放即刻扫描发批，无攒批定时器；3.2 的 dispatcher 对每条流首帧即刻投递、只合并已入队帧——"抽空即发"由 3.1/3.2 的构造天然达成，3.4 无需独立代码改动。adjustWaitTime 仅调控空闲轮询频率（408 重试间隔），与批填充无关，保持原样。

### 3.5 配置开关

`HTunnelConfig` 新增 `AllowFlowReorder bool`（默认 false = 有序；true 回滚旧行为），POST 独占与 GET 钉死共用一个开关；路由钉死独立开关 `MeshConfig.AllowFlowRouteChange`（默认 false = 钉死；true 回滚逐包轮转）。回滚 = 置 true 或退回上次运行版本，两档都有。

### 3.6 stripe lane 亲和钉死（验收复测失败后追加，2026-11-06）

**背景**：3.1-3.3 部署后验收仍失败（fast_retransmit +120/68s）。逐层排查曾怀疑 stripe lane 亲和漂移：单流下载期间 JF 侧两条 lane 计数器同时增长。

**结论（二轮复测修正）**：stripe 权重漂移会造成流横跳（本节修复仍有价值，作为加固保留），但**不是本次乱序根因**——零 lane 纯 primary 通道下乱序依旧。真正的根因是 3.2 的 dispatch 模式判定 bug（见 §3.7）。

**改法**：stripeTransport 新增粘性亲和表 `map[uint32]*stripeLane`（flowHash32 流键 → lane）：
1. 选路先查表，命中即用（**不再乘 currentWeight**，权重漂移不撼动已建立流的绑定）；
2. 未命中按带权 rendezvous 解析（质量感知保留给新流）并登记；
3. lane 增删（attachLane/removeLane）时整表作废，全部流重选——换路瞬态接受（同 §3.3 立场）；
4. 表容量上限 65536（超出即作废重建，防内存膨胀）；
5. lane 队列满回退 primary 的路径保留：罕见瞬态，与 §3.3"换路时旧路在途 drain 不做"同一取舍。

`flowHash32` 与 3.1/3.2 的 `frame.FlowKey` 同为五元组哈希（各自带盐、不同盐不影响钉死语义，只影响分布）。

### 3.7 dispatch 模式判定 bug — 3.2 实际未生效（二轮复测实锤，2026-11-06）

**现象**：3.6 部署后验收三度失败（fast_retransmit +170/122s，单流 67KB/s）。复测期间 JF 侧 `transport: tcp`（**零 stripe lane**）——证明乱序与 stripe 无关，存在于单通道内部。

**根因**：`meshDispatchLoop` 在**dispatcher 启动瞬间**用 `legacyDispatch(ch)`（查 `pipeCnt==0`）决定模式，而 dispatcher 随 channel 创建即启动——**永远早于客户端首个携带 X-N 的 GET**。于是所有 channel 终身跑 legacyDispatchLoop，3.2 的 per-pipe 有序分发从未生效，GET 侧维持"任意空闲 waiter 领取"的原生乱序。

**修复**：模式判定改为纯配置——`AllowFlowReorder` → legacy；否则一律 pipeDispatchLoop。X-N 到达前 `pipeCnt==0`，数据帧全走 wildcard 队列（pipeAnyQ/pipeAnyW），行为等价 legacy；X-N 到达后 pipeQ 惰性增长，正序分发自然接管。X-N 前已入 pipeAnyQ 的帧与 X-N 后的 pipe 帧之间可能有短暂交错（仅通道建立初期、量极少），接受。

### 3.8 V2 服务端 GET 投递：控制帧优先 + 按字节合批（v5 取证实锤，2026-11-06，用户已批准）

**现象**：QG↔JF 走 V2 直连时，8 流负载下 3 次中 2 次会话重建（QG 日志 `link dead`：连续 10 个 ping 丢失），重建瞬间 JF pipeQ 积压全丢 → 内层流整窗丢失 → RTO 退避到 30~50s，表现为硬停滞/截断；18:55 QGW↔JF V2 段同样复现。V1（单 FIFO txBuf）无此问题：控制帧只被延迟、不会饿死。

**根因（代码 + 日志实锤）**：
1. p2p 层 pong 走 `isControl=true`（p2p.go handlePing），但 `meshChannelTransport.Send` 丢弃了 isControl，`flowKeyOf` 把一切非 MeshPacket 帧标 `flowKeyAny` → 进 `pipeAnyQ`。
2. `flushPipes` 先让每条 pipe 的 waiter 被本 pipe 数据消费，`pipeAnyQ` 只能等"全部 pipe 数据都投完后剩下的 waiter"。客户端 2 条 GET 都带 X-Q（都是 pipe waiter），负载下 pipe 数据永不排空 → pong **饿死**而非延迟 → 对端 10 次 ping 超时判链路死亡。
3. 每 GET 上限 32 帧（waiter.msg cap 32，~42KB）× 2 条串行 GET = 吞吐天花板，加剧 pipeQ 积压（v5 实测 ~600ms 排队 ≈ 10MB 用户态缓冲即在此）。
4. 附带竞态：handler 超时与 dispatcher 推帧之间无同步，已推入 waiter.msg 的帧在 408 时丢失。

**改法（F1 + F3，仅改 server/htunnel_mesh.go + htChannel 字段，不改协议、不改 p2p 层）**：
1. **控制帧按 isControl 分类（F1）**：`meshMsg` 增 `control` 字段，`Send(..., isControl)` 透传；`SendBatch` 均为数据帧。控制帧进新增的 `ctrlQ`，每次投递**最先**装入批次，任意 waiter（pipe 或 wildcard）可领取。
2. **ping 保持走数据通道**（用户既定设计：ping 用于度量数据路径）：非控制、非 MeshPacket 帧（ping 等）在 `pipeCnt>0` 时按 `pipeRR` 轮转落到某条 pipe 队列尾，与该 pipe 数据同 FIFO 排队；`pipeCnt==0` 时进 `pipeAnyQ`。
3. **批次组装顺序**：pipe waiter = `ctrlQ → pipeAnyQ → pipeQ[p]`；wildcard waiter = `ctrlQ → pipeAnyQ`。pipeAnyQ 只含 X-N 到达前的早期数据，排在 pipe 数据前保持其相对先后。
4. **按字节合批（F3）**：一个 GET 的批次按线上字节（3+len(payload)）累计至 `GetMaxBatchSize()`（默认 512KB，低于 nginx 1m），至少 1 帧；取代"32 帧/GET"。dispatcher 在一次唤醒中先非阻塞排空 meshOut 再 flush，使积压时批次自然做大，空闲时首帧即发（§3.4 语义不变）。
5. **无竞态交接**：waiter 改为 `batch chan []meshMsg`（cap 1，每 waiter 恰投 1 次）。dispatcher 在 `pipeMu` 下检查 `done` 后投递；handler 超时时在 `pipeMu` 下 `close(done)`，随后非阻塞检查 batch——已交接的批次照常返回 200，不丢帧。
6. **legacy 模式（AllowFlowReorder=true）并入同一投递循环**：legacy 下 meshHandleRead 本就忽略 X-N，pipeCnt 恒为 0，数据帧全走 pipeAnyQ 单 FIFO、waiter 全为 wildcard，按字节整批交接；控制帧同样优先（与有序/乱序开关正交，只会更好）。原 `legacyDispatchLoop`/`dispatchToWaiter` 删除。
7. 已出队批次的原队列槽位清零、空队列置 nil，避免底层数组钉住已投递的 payload。

**不在本节（F2，另立方案待批）**：pipeQ 仍为无界队列；单纯加界背压只会把膨胀挪进 p2p DRR 8MB 预算，真正治理需 sojourn-time AQM（CoDel 类丢弃）+ `meshChannelTransport.Send` 监听 peer 停止。F1+F3 先止血（消除会话重建）并提升 GET 吞吐，F2 视复测排队时延再定。

**验收**：QG→JF V2 8 流下载 3 轮，QG/JF 日志零 `link dead`/会话重建；聚合吞吐 ≥ V1 直连基线（3.1~5.1MB/s）。

**验收结果（JF 部署 v0.8.0-327-gb1834f9，2026-11-06 19:49）**：✅ 通过。3 轮 × 8 流 × 8.18MB 全部完整下完，聚合 4.54 / 3.90 / 5.55 MB/s（修复前 V2 ~2.65MB/s 或 3 次中 2 次会话重建）；QG 日志（info 级）部署后零 `link dead`/MGMS_HT 会话重建，JF 侧 qg/qgw peer 探测样本连续累积（461/448）、srtt ~22ms。单流仍有 0.49~2.1MB/s 的离散，残余排队/乱序归 F2 与 v5 取证继续跟踪。

### 3.9 代码审查问题修复 + F2 排队治理（2026-11-06，用户已批准"都按合理的修"）

对 §3.1~§3.8 落地代码做审查，发现以下问题，逐项修复（不改协议，混合版本共网兼容）：

**R1. 回滚开关不生效（§3.5 失实）**：`HTunnelConfig` 从未接入任何配置源——服务端 `HTunnelServer.htConfig` 与客户端 `dialP2PDirect` 的 `htConfig` 恒为 nil，`AllowFlowReorder` 永远 false，"置 true 秒级回滚"不成立。
- 改法：`MeshConfig` 增 `HTunnel *HTunnelConfig`（json/yaml `htunnel`，随 mesh-config 桶持久化）；`config` 包持有进程级当前值（`SetHTunnel`/`HTunnel()`，原子指针）。main 启动加载 mesh 配置后、`OnMeshConfigUpdate` 热更新时写入；admin `PUT /api/mesh/config` 接受 `htunnel` 字段（`null` = 清除回默认）。
- 取值时机：客户端每次 `dialP2PDirect` 快照一次、服务端每个 mesh channel 创建时快照一次——同一通道内模式恒定，开关只影响新建通道（存量通道随会话重建自然切换）。

**R2. §3.3 路由钉死落在死代码里**：每流 pin 缓存实现在 `MeshManager.SendToNode`，而该函数全仓无调用方；真实转发路径 `NextHopLink` 直接取 `bestPaths[target]` 的单一 NextHop/LinkNIC，在同一路由快照内对所有流确定不变——§3.3 的"每流生命周期单路径"由现有构造天然满足（快照切换即全体换路，与 §3.3 接受的换路瞬态同义）。
- 改法：删除 `SendToNode`/`findNextHopsForNode` 与 pin 缓存（`flowRoutePin*`、`SetFlowRoutePin`、`dropFlowRoutePins`、`linkSelectIdx`），删除 `MeshConfig.AllowFlowRouteChange` 开关及 main 接线；§3.3/§3.5 原文保留作历史，以本条为准。

**R3. §3.1 有序发送队列无界**：`enqueueOrdered` 只追加不设限，下游 POST 慢时 `ordPending` 可无限膨胀（内存 + 排队时延双失控），且绕开了 p2p 层对 transport 背压的预期。
- 改法：按字节加界（`ordPendingCap` = 4MiB，全通道合计）。超界时 `enqueueOrdered` 在 `sync.Cond` 上阻塞，直到 dispatcher 取走批次释放字节或通道关闭——背压回传 p2p peerWriteLoop/DRR（与其他 transport 的阻塞写同语义）。单次入队允许越界一批，避免大批次永不可入。控制帧不经此队列，不受影响。

**R4. §3.6 lane 满即回落 primary 制造乱序**：stripe lane 队列满时该批直接走 primary，下一批又回到 lane，同一流在两条路径间来回横跳——这正是 §3.6 要消除的乱序。另：亲和表每次重置都 `make(map, 65536)` 预分配，属无谓的 ~MB 级常驻。
- 改法：lane 队列满时**有界等待**（50ms）；仍满或 lane 关闭则走 primary，并把这批涉及的流**改钉到 primary**（亲和表置 nil）——每流至多一次换路瞬态，不再横跳。亲和表重置改为不预分配。

**R5. p2p 入站按流队列无界阻塞**：数据帧入 per-flow worker 队列时无限期阻塞（仅 stop 可解），gVisor 消费慢时会卡住整条会话读循环，连同 pong/控制帧一起停摆 → 心跳超时。
- 改法：先非阻塞投递；满则有界等待 100ms，仍满则丢弃并计 `meshInboundDrops`。100ms 远小于 ping 5s 超时，读循环不会被饿死；持续过载时丢弃是正确的拥塞信号（内层 TCP 降窗），而非让控制面陪葬。

**F2. V2 服务端投递排队治理（§3.8 遗留，本节实施）**：
1. **控制帧独立入口**：新增 `meshCtrlOut`（cap 256），`Send(isControl=true)` 走它，dispatcher 恒读——数据侧被硬上限暂停时控制帧不被堵在 meshOut 里（§3.8 饿死问题的构造性根除）。
2. **硬字节上限**：数据队列（pipeAnyQ + 全部 pipeQ）合计 ≥ 8MiB 时 dispatcher 暂停读取 meshOut，meshOut 填满后 `Send`/`SendBatch` 阻塞 → 背压回传 p2p DRR；GET 取走数据降到界下即恢复。仅在客户端 GET 停滞时触发，正常负载由下条 AQM 控制。
3. **每队列 CoDel（RFC 8289）**：帧入 meshOut 时打单调时间戳；批次组装出队时按 RFC 8289 控制律对 pipeQ[p] / pipeAnyQ 逐帧判定，target 50ms、interval 300ms（GET 链往返 ~20~40ms，留余量）。只丢 `FrameMeshPacket`（ping 等非流帧不丢，保持路径度量），队列剩余 < 2 帧时不丢。丢弃是稀疏单丢——后续段到达产生 dup-ACK/SACK，内层走快速重传而非 RTO——用于把常驻排队（v5 实测 ~600ms）压回 target 附近。丢弃计数入日志（debug，节流）。
4. **会话结束即关通道**：`meshChannelTransport.Close()` 改为关闭所属 channel（`closeChannel`，幂等）。p2p 会话终止（重复会话去重、心跳超时等）后客户端立即收到 410 重拨，不再留僵尸通道（POST 卡在无人读的 meshIn、GET 空转）；`Send`/`Recv` 已监听 `ch.closed`，随之解阻塞。

**验收**：
1. `go vet`/`go test -race`（server/p2p/dialer/mesh/frame/config）通过，新增 CoDel、硬上限、控制旁路、有界入队、lane 改钉单测。
2. QG→JF V2 8 流 × 3 轮：零 `link dead`/会话重建；聚合吞吐不低于 §3.8 结果（3.9~5.6MB/s）。
3. JF `/api/mesh` 回滚开关：PUT `htunnel.allow-flow-reorder=true` 后新建通道走 legacy（日志/行为可见），置回 false 恢复。

### 3.10 撤销 h_tunnel 有序模式 + IPIP 按内层包分类（2026-11-06，用户已批准方向）

#### 3.10.1 背景与证据

场景：Mac → QG 旁路网关 → mesh → JF → DESKW → `10.193.131.25`（elephant，DESKW 通告路由，走 IPIP）。浏览器访问极慢。

**A/B 实测**（同一路径，curl 单连接，QG 局域网侧抓包；切换方式：QG、DESKW 的 mesh 配置 `htunnel.allow-flow-reorder=true` + 重启 JF 重建通道）：

| 指标 | 有序模式（§3.1/§3.2） | 允许乱序 |
|---|---|---|
| app.js | 20.5s | 12.1s |
| chunk-vendors.js | 98s | 72.5s |
| 内层 RTT p50/p90/max | 72/103/158ms | 56/75/120ms |
| 接收侧乱序到达 | 0 | 26% |
| elephant 重传 | 71 | 64 |
| 其中误重传（重传到达前接收端已累计确认 + DSACK） | 70/71 | 64/64 |
| 重传触发：定时器（RTO/TLP） | 71 | 33 |
| 重传触发：乱序（≥3 个带 SACK 空洞的重复 ACK） | 0 | 17（另 14 个 1~2 个空洞，可能由 RACK 触发） |

重传确认是发送端协议栈生成（TSval 均比原包新），不是隧道复制。

**结论**：
1. §3.1"每流同一时刻至多一个 POST 在途"在每跳每批引入 0~1 个 HTTP RTT 的等待。QG POST、JF→DESKW GET、DESKW POST、JF→QG GET 四处叠加，形成内层 RTT 抖动，超过 elephant 约 130ms 的 RTO，导致误超时重传和 cwnd 塌缩。代价远大于消除乱序的收益。
2. 乱序本身的代价有限：现代 TCP（RACK-TLP、DSACK 撤销）能扛住大部分。即使完美保序，也只能去掉约 17~31 次乱序类误重传。
3. 修订 §二"让乱序在构造上不可能"的立场：**对 h_tunnel 多 POST/多 GET 不再强制保序**。保留零延迟成本的"按流哈希钉路径"措施，它们不会让包等待。
4. 撤销前（c608130 之前）接收端约 50% 乱序，主要来源是 4 个 worker 抢一条共享入站队列。该问题已由 f0fb969 的按流钉 worker 队列消除，予以保留。当前允许乱序模式下剩余的 26% 来自 ht 多 POST/多 GET 并发。

**IPIP 分类缺陷（代码实锤）**：以下分类函数只看外层 IP 头。IPIP（协议号 4）的外层都是 QG↔DESKW，于是所有经通告路由的连接被当成同一条流、同一种包，且每跳（QG、JF 中继、DESKW）都会重复：

| 位置 | 后果 |
|---|---|
| `p2p.flowHash32` → DRR `hashFlow`（p2p/p2p.go:881、1035） | 所有 IPIP 连接挤进 256 个 DRR 队列中的同一个（深度 512 帧、每轮 64KB 配额），互相排队 |
| `isACKPacket`（p2p/p2p.go:973） | IPIP 里的纯 ACK 不进 ackQueue 优先通道，排在数据之后 |
| `isDNSPacket`（p2p/p2p.go:939） | IPIP 里的 DNS 不进 dnsQueue |
| `frame.FlowKey` → 入站 worker（p2p/p2p.go:1789） | 4 个 worker 只用到 1 个 |
| stripe 亲和（`flowHash32`） | 有 lane 时，所有 IPIP 流落同一条 lane |

浏览器并发约 6 条连接时，它们挤在同一个 DRR 队列里，ACK 也被数据延迟，正好会加剧上表中的定时器类误重传。curl 单连接测试会低估这一影响。

#### 3.10.2 改动一：撤销 h_tunnel 有序模式（手动改，不做 git revert）

**删除**：

- 客户端 `dialer/htunnel_direct.go`：
  - 删除 `enqueueOrdered`、`orderedDispatchLoop`、`ordRelease`、`ordFlowBuf`，以及全部 `ord*` 字段、`maxOrdBatchFrames`/`maxOrdBatchBytes`/`ordPendingCap`，`Close` 里的 `ordClosed` 处理一并删除。
  - `Send`：数据帧直接走 `sendFrame`。
  - `SendBatch`：恒走"整批一个 POST、sendSem 16 路并发"路径（即现 allow-flow-reorder 分支）。
  - `recvLoop`：不再发送 X-Q/X-N 头。
- `dialer/htunnel.go`：删除 `HeaderPipeIndex`/`HeaderPipeCount` 常量。
- 服务端 `server/htunnel.go` + `server/htunnel_mesh.go`：
  - 删除 X-N/X-Q 解析，删除 `pipeCnt`/`pipeQ`/`pipeWait`/`pipeRR` 与按流分管道逻辑（`routeFrameLocked` 的 pipe 分支、pipe waiter）。
  - 删除 `meshMsg.flowKey`/`flowKeyOf`/`flowKeyAny`。
  - 数据帧只剩一条 FIFO 数据队列，所有 GET waiter 平等领取。
- `config.HTunnelConfig`：删除 `AllowFlowReorder` 字段与 `GetAllowFlowReorder()`。
  - `HTunnel` 配置接线（§3.9 R1）保留，pool-size、get-slots 等仍在用。
  - 通道启动日志去掉 `allowFlowReorder=`。
- 测试：
  - 删除 `TestMeshDispatch_PingsRoundRobinOnPipes`、`TestMeshDispatch_PipeIsolation`、`TestMeshDispatch_WildcardBeforePipeCount`。
  - 其余 dispatch/CoDel 测试改为单队列语义。

**明确保留（与保序开关无关，或属于零成本的按流哈希）**：

- §3.8 F1/F3：控制帧 `ctrlQ` 最优先、按字节合批、无竞态交接。ping 仍走数据队列，同 FIFO 排队，保持"ping 度量数据路径"的语义。
- §3.9 F2：`meshCtrlOut` 控制旁路、8MiB 硬上限背压、CoDel（作用于单一数据队列）、`Close` 关通道。
- §3.9 R2（删除死代码 `SendToNode`）的结论不变。
- §3.6 / §3.9 R4：stripe 按流亲和与 lane 满改钉 primary。
- f0fb969 / §3.9 R5：入站每个 worker 一条队列、按流钉分配、有界等待。
- `frame.FlowKey`：入站 worker 仍在用。

**与旧版本混跑**：
- 旧客户端发来的 X-N/X-Q 头，新服务端忽略，按 FIFO 处理。
- 新客户端不发头，旧服务端 `pipeCnt==0`，走 wildcard 队列。
- 两个方向都兼容，可逐节点升级。
- 未升级的旧客户端（默认有序）仍保持 POST 停等，升级后消除。

**数据库残留**：QG、DESKW 的 mesh 配置里有 `htunnel: {"allow-flow-reorder": true}`。字段删除后 JSON 解码会忽略它，不影响运行。部署后 PUT `{"htunnel": null}` 清理。

#### 3.10.3 改动二：IPIP 按内层包分类

新增 `frame.InnerPacket(pkt []byte) []byte`：

1. 外层不是 IPv4，或协议号不是 4 → 返回 `pkt` 本身。
2. 外层是分片（MF=1 或片偏移≠0）→ 返回 `pkt` 本身（非首片没有内层头）。
3. 外层 IHL 非法，或长度不足以容纳内层 IPv4 头 → 返回 `pkt` 本身。
4. 内层不是 IPv4 → 返回 `pkt` 本身。
5. 否则返回 `pkt[外层IHL:]`。只解一层，不递归。

以下函数入口先调用 `InnerPacket` 再按原逻辑解析：

| 函数 | 改后效果 |
|---|---|
| `frame.FlowKey` | IPIP 按内层五元组分 worker |
| `p2p.flowHash32` | IPIP 按内层五元组分 DRR 队列和 stripe lane |
| `p2p.isACKPacket` | IPIP 里的纯 ACK 进 ackQueue（已被 3.10.6 改动三取代：ackQueue 与 `isACKPacket` 删除） |
| `p2p.isDNSPacket` | IPIP 里的 DNS 进 dnsQueue |

不改封装/解封装、不改 netstack、不改路由，只改发送侧和接收侧的分类。

**单测**：
- 内层五元组不同的两个 IPIP 包，`FlowKey`/`flowHash32` 结果不同。
- 同一内层流的多个 IPIP 包结果相同。
- IPIP 内层纯 ACK → `isACKPacket=true`；IPIP 内层带载荷 → false。
- IPIP 内层 UDP 53 → `isDNSPacket=true`。
- 外层分片、截断包、内层非 IPv4 → 回退到外层分类，且不越界。

#### 3.10.4 入站 worker 数（评估，本节不改）

- `meshInboundWorkers=4` 来自 885699d（2026-10-20）的经验值：1 个 worker 时出现 `channelDrops=161`，改为 4 个后归零。它不是按 CPU 核数或单包耗时推导的。
- 每个 worker 的工作：`HandleMeshFrame` → 注入 gVisor（TCP 段入端点队列，由 gVisor 自己的协程处理）。中继转发出口是非阻塞入队，单包开销小。
- 2026-11-06 实测：QG、JF、DESKW 的 `meshInboundDrops` 与各 peer DataDrops 均为 0。按当前 ht 链路几 MB/s 的量级，4 个 worker 不是瓶颈。
- 已知局限：
  - 任意两条流有 1/4 概率共用同一个 worker，消费阻塞时会互相队头阻塞。当前无消费阻塞证据。
  - IPIP 聚到 1 个 worker 的问题由改动二解决。
- 结论：维持 4 个。若以后出现入站丢包，或要按核数自适应（如 `clamp(NumCPU, 2, 8)`），另行立项。

#### 3.10.5 验收

1. `go vet` 与 `go test -race`（frame/p2p/server/dialer/config/mesh）通过，含 3.10.3 新增单测。
2. 部署 JF、QG、DESKW（按部署纪律，各环境操作前与用户确认），确认版本号。验证 `/api/mesh` 中 `channelDropStats` 全 0，日志零 `link dead`。
3. curl 单连接取 elephant 静态资源：app.js ≤ 12.1s、chunk-vendors.js ≤ 72.5s（不劣于允许乱序模式基线）。同时抓包统计误重传与触发分类，与 64 次基线对比。
4. 用户浏览器访问 `https://10.193.131.25/elephant/default/#/login`，主观加载明显改善。
5. QG→JF V2 8 流 × 3 轮回归：零会话重建，聚合吞吐不低于 §3.8 结果（3.9~5.6MB/s）。

**回滚**：开关已删除，只能按纪律退回上次运行的二进制。

#### 3.10.6 改动三：去掉 ackQueue + 写循环合批（2026-11-06 追加，用户批准）

**证据**（改动一、二部署 JF/QG/DESKW 后，v0.8.0-329，curl 单连接，排除浏览器并发干扰）：

| 指标 | 基线（允许乱序，旧版） | 改动一+二 |
|---|---|---|
| app.js | 12.1s | 23.7s / 26.5s / 19.4s |
| 重传 / 其中定时器触发 | 64 / 33 | 102 / 82、113 / 102 |
| 在途段 p50 | 3 | 2 |
| 内层 RTT p50 | 56ms | 55ms |

- 允许乱序模式下 HEAD 本就不发 X-N/X-Q、不走有序队列，改动一对该场景行为不变；行为差异来自改动二。
- 改动二让 IPIP 内的纯 ACK 进入 ackQueue。`peerWriteLoop` 对 controlCh/dnsQueue/ackQueue 逐帧 `writeSingleFrame` → `Send`，在 h_tunnel 上即一个 ACK 一个 POST：QG→JF 测试期间 80~115 POST/s（空闲 6~10/s），而改动二之前 IPIP ACK 在 DRR 里与数据合批。
- QG→JF POST 往返 p50 16ms、p99 19ms，sendSem 16 未打满，单帧 POST 是否为变慢主因未实锤；本改动恢复改动二之前的 ACK 发送方式，同时消除逐帧 POST，部署后复测验证。

**3a 去掉 ackQueue**（在 p2p 写路径，对所有承载协议生效：直连 TCP、trojan 等流式代理、h_tunnel、udp-leg、stripe）：
- 删除 `Peer.ackQueue`、`ackQueueDrops`（含三处初始化、`peerWriteLoop` 两处 select 分支、`channelPriorityName` 分支、丢包合计）。
- 删除 `isACKPacket` 及其单测；纯 ACK 与其它数据一样按 `flowHash32` 进 DRR。
- 理由：ACK 与数据方向相反、哈希到不同 DRR 队列；ACK 流稀疏，DRR 每轮都会服务到它，天然低延迟（同 fq_codel 的稀疏流原理）。共享 ackQueue（256 深 FIFO）反而跨流队头阻塞，且只能逐帧发送。

**3b 写循环合批（控制/DNS/数据同批，优先者排前）**：
- `peerWriteLoop` 阻塞等到任一帧后，非阻塞取尽当时已就绪的 controlCh、dnsQueue、drrOutput，按"控制 → DNS → 数据"顺序组成一批，一次 `SendBatch` 发出；上限沿用 `maxBatchFrames=64`（按优先序填，满即止）。优先级只体现在取帧顺序和批内位置，不再为控制帧/DNS 单独发一次 POST。批内仅 1 帧时仍走 `Send`（带原 isControl）。
- `frame.Frame` 新增 `Control bool`，写循环按 `req.isControl` 填写，使批次保留控制语义：
  - `server.meshChannelTransport.SendBatch`：`Control` 帧进 `meshCtrlOut`（`meshMsg.control=true`），保持 §3.8 控制帧优先投递（pong 不饿死）；其余进 `meshOut`。
  - `htunnelDirectTransport`、`streamTransport`：整批按批内顺序写出（控制帧已在最前），无需区分。
  - `udpLegTransport`：已按帧类型逐帧分流（gossip 恒 TCP，ping/pong、mesh 数据按腿状态），混合批次行为与逐帧 `Send` 一致；`stripeTransport`：非 mesh 包本就不可拆分、走主通道。
- 出口健康计数（`framesSent`、sojourn）只统计非控制帧，与现逐帧路径一致。
- **GET 方向已是同一规则，无需改**：服务端 `buildBatchLocked`（server/htunnel_mesh.go:251）每个 GET 响应先取 ctrlQ 全部控制帧，再按字节上限取 dataQ，一次响应带回（§3.8）。3b 之后 JF 写循环的批次按"控制 → DNS → 数据"进入 meshCtrlOut/meshOut，DNS 在 dataQ 中位于同批数据之前。

**单测**：
- 纯 ACK 入队后进入 DRR 数据队列（不再有 ackQueue）。
- 写循环：控制、DNS、数据同时就绪时合成一次 `SendBatch`，顺序为控制 → DNS → 数据，控制帧 `Control=true`。
- `meshChannelTransport.SendBatch`：`Control` 帧进 `meshCtrlOut`，其余进 `meshOut`。

**验收**：同 3.10.5 第 1~3、5 条（curl app.js ≤ 12.1s、chunk-vendors.js ≤ 72.5s，抓包统计重传并与 64 次基线对比；QG→JF POST 速率回落）。部署 JF/QG/DESKW 前与用户确认。

**验收记录（v0.8.0-330-g156a2d3，2026-11-06，QG eth1 抓包，重传 = 同 seq 重复出现）**：

elephant 路径（Mac → QG → JF(h_tunnel) → DESKW(h_tunnel) → 10.193.131.25）：

| 指标 | 允许乱序基线 | v329 | v330 |
|---|---|---|---|
| app.js（1.78MB） | 12.1s，重传 64 | 19~26s，重传 100~113 | 17.4s / 17.4s，重传 81 / 96 |
| chunk-vendors.js（7.1MB） | 72.5s | — | 60.5s，重传 290 |

测试期间混入一条无 SYN 的外部连接（约 1.1MB，疑似浏览器），结果有干扰。

GG 替代测试环境（不依赖 DESKW 部署）：GG 复用已通告的 `172.18.0.1/32`，在该地址起临时 HTTP 服务（`python3 -m http.server 18999 --bind 172.18.0.1`，目录 `/tmp/phtest`，随机内容 f2m.bin 2MB / f8m.bin 8MB）。路径 Mac → QG → JF(h_tunnel) → GG(trojan GGSV_TJ)。经 JF linkNICStats 差分确认实际经 JF（下载 2MB 期间 JF 的 gg NIC +2.18MB、qg NIC +121KB）。QG 的 bestPaths 显示经 mb，与实际转发不一致，未深究。

| 指标 | GG v319（QG/JF v330） | GG v330 |
|---|---|---|
| 2MB ×2 | 9.2s / 7.5s，重传 64 / 46 | 5.0s / 5.8s，重传 64 / 21 |
| 8MB | 33.9s，重传 259 | 24.6s，重传 117 |

**全局审视发现的问题（2026-11-07，用户确认后的结论）**：

1. **sojourn 测量失真（h_tunnel 异步 POST）**：`SendBatch` 拿到 sendSem 后启动 goroutine 就返回，`peerWriteLoop` 记录的 sojourn 不是真实发送时间。但 sojourn 只用于观测和 stripe lane 决策，h_tunnel V2 不适用 stripe，所以影响不大。**暂不修复**，后续可在 goroutine 里回传真实时间戳修正观测数据。

2. **Control 标志冗余**：客户端设置 `Frame.Control` 标记 ping/pong/gossip，但这些帧的 `FrameType` 本身就是控制类型，服务端靠 `FrameType` 就能分流。Control 标志只在本地内存里有意义，跨节点后丢失，但不影响功能。**不是问题，不修复**。

3. **DRR 调度开销**：O(N) 遍历 256 队列 + deficit 计算，CPU 开销存在但可接受。elephant 测试显示乱序代价有限（基线 26% 乱序，重传 64 次），DRR 的公平性收益可能大于性能损失。**暂不动，先 profile 实锤 CPU 占比**。瓶颈可能在带宽利用率而非 DRR。

4. **合批粒度错配**：客户端按帧数合批（maxBatchFrames=64），服务端按字节合批（GetMaxBatchSize() 默认 512KB）。~~"细发粗回"可能导致服务端堆积~~。**撤销（2026-11-07）**：两端都是"取当时积压，不等待"，上限只在积压多时起作用，不会造成堆积。服务端维持 512KB。客户端也不改成 512KB：服务端 meshHandleWrite 用 io.ReadAll 收完整个 POST 才解析，大 POST 会让首帧等整批上传完；而且 64 帧上限在通用写循环里，trojan、UDP 腿也共用。

5. **stripe lane 不适用于 h_tunnel V2**：h_tunnel V2 是 datagram 语义（POST/GET 池），本就不该用 stripe lane。V1（trojan/socks5）继续适用。**已修复（2026-11-07）**：主动侧判定条件与 dialer/htunnel_bind.go:52 一致（类型为 h_tunnel，且 `IsP2P() && V2`），满足时不套 stripeTransport，p2p_udp 腿直接包底层 transport。被动侧不区分来源，仍套 stripe（0 lane 时为直通）。

6. **bestPaths 和实际转发不一致**：可能是 gossip 广播延迟，多观察。**暂不修复，继续观察**。

**修复计划**：
- 先修 4（合批粒度）和 5（stripe V2 排除）。
- 修完后 profile DRR 调度 CPU 占比，实锤是否是瓶颈。
- 如果 DRR 不是瓶颈，重点查带宽利用率低的原因（内层 TCP 拥塞控制、HTTP 开销、代理链路延迟等）。

**修正与实测（2026-11-07，待用户确认，代码未提交）**：

- 第 4 条前提不成立：服务端 `buildBatchLocked` 不等待凑批，批是"恰好无人消费时积压"出来的。把默认值改为 64KB 只会限制单个 GET 的排空量，不解决任何问题。建议撤回，恢复 512KB。
- 第 5 条条件写错：`IsV2()` 对 trojan/socks5 开了 v2 也成立。正确条件应与 dialer/htunnel_bind.go:52 一致（类型为 h_tunnel 且 `IsP2P() && V2`）。另外，V2 且开启 p2p_udp 时 `newUDPLegTransport` 会拿到 nil 的 stripe，应改用底层 transport。本条只影响结构整洁，对性能无作用。
- 更正事实：DESKW 的 DESK_TJ 类型为 H_TUNNEL，v2=true，经 JF 32457 接入（DESKW /api/proxies 实测）。
- DRR 基准（p2p/drr_bench_test.go，i5-8257U）：一次 NextBatch（256 队列、64 帧）约 25.5µs，其中分配主要来自基准自身的补帧。按实测速率换算，CPU 占比可以忽略，**DRR 不是瓶颈**。

**带宽利用率低的根因（GG 测试路径实测，8MB 单流，v330）**：

| 观测点 | 结果 | 结论 |
|---|---|---|
| GG→JF 外层 trojan 套接字（ss 每 0.4s 采样） | Send-Q 0~15KB，unacked ≤ 25 段 < cwnd 49~145，RTT 稳定 21.6ms，能承载约 3MB/s，实际 250~500KB/s | 外层链路没打满，未积压 |
| JF→nginx（:32457 GET 响应）套接字 | 8 条上游连接合计约 9.3MB，Send-Q ≤ 3.3KB | 未积压 |
| 内层接收端 Mac 通告窗口（QG 抓包） | 216KB | 不受接收窗口限制 |
| 内层发送端 GG gVisor（/api/mesh tcpFlowDebug，每 0.5s） | 发送缓冲始终有 0.5~1MB 未发数据，cwnd 被压在 5~20 段，ssthresh 每约 0.5s 被砍一次，srtt 55~75ms | **受拥塞窗口限制**：吞吐 ≈ 10 段 × 1448B ÷ 65ms ≈ 220KB/s，与实测 8MB/33s 吻合 |
| GG gVisor 计数器（单次下载差分） | sack_recovery +53，其中 spurious_recovery +52（98%）；DSACK +212；timeouts +0 | **几乎全部"丢包恢复"都是乱序误判** |
| QG 抓包内层到达乱序 | 首次到达乱序 437 个，乱序距离 p50 7KB、最大 20KB（v330）；v319 最大 115KB | 乱序距离不超过一个 cwnd，符合"批次之间先后颠倒" |

**gVisor 源码确认（../gvisor-fork/pkg/tcpip/transport/tcp/snd.go:1472）**：`detectSpuriousRecovery` 只按 RFC 3522 识别误恢复并计数，**没有实现撤销**（RFC 4015 Eifel 响应 / Linux DSACK undo），被砍的 cwnd/ssthresh 不会恢复。RACK（rack.go:304）会在收到 DSACK 后放大乱序窗口，但起点只有 minRTT/4（约 8ms），而两条并发 GET 响应的先后差常大于此。因此 §3.10.1 第 2 条"现代 TCP（RACK-TLP、DSACK 撤销）能扛住大部分"对 gVisor 发送端不成立。

**附带发现**：tcpFlowDebug 中 `sackedOut` 长期为负值（-1 ~ -9），属于 SACK 记账异常，可能影响 pipe 估算，待另行核查。

**架构层面的结论**：路由器之所以快且公平，是因为每条链路都是先进先出的，同一条流不会被拆到多条并行路径上（ECMP 按流哈希就是为了这个）。端点用的是带乱序容忍和撤销能力的内核 TCP。我们的 h_tunnel V2 把同一条内层流拆到 16 路并发 POST 和 2 路并发 GET 上，等于一条"不保序的多路径链路"。端点又是不会撤销误判的 gVisor TCP，每次乱序都会让 cwnd 永久打七折。之前的有序模式用"每流至多一个在途请求"的停等方式保序，引入了 RTT 抖动，方向是对的、手段错了。之前"外层隐藏缓冲吸收了队列"的假设，已被上表实测否定。

### 3.11 gVisor 发送端误恢复撤销（方案 A，2026-11-07 用户选定，待确认设计）

**依据**：见上节实测。单流下载期间 sack_recovery +53，其中 spurious +52，timeouts +0。gVisor 已能按 RFC 3522 识别误恢复，但不撤销降窗，cwnd 长期被压在 5~20 段。

**范围**：只改 ../gvisor-fork/pkg/tcpip/transport/tcp，只处理快速/SACK 恢复的误判。RTO 误判不处理（实测 timeouts 为 0；RTO 后是回退重传，贸然恢复大窗口会突发重发，风险大）。不改 RACK 乱序窗口（方案 B），也不改隧道层。

**改法**（参照 Linux `tcp_undo_cwnd_reduction` / RFC 4015 Eifel 响应）：

1. **降窗前存快照**：新增 `sender.reduceForLoss()`，先保存 `SndCwnd`、`Ssthresh`，以及拥塞控制自身的状态（cubic 的 `WMax/WLastMax/K/T/numCongestionEvents`），再调用 `cc.HandleLossDetected()`。现有 4 处 `cc.HandleLossDetected()`（snd.go:1313、snd.go:1805、rack.go:270、rack.go:410）都改为调用它。拥塞控制通过一个可选接口 `undoer{ saveUndo(); undo() }` 保存和恢复自身状态，cubic 实现该接口，reno 不需要。
2. **识别到误判时**（`detectSpuriousRecovery` 置 `spuriousRecovery=true` 处）：如果当前处于 SACK 恢复且快照有效，就置 `undoPending=true`，并立即令 `SndCwnd = max(SndCwnd, 快照cwnd)`，让剩余恢复期内就能按原速发新数据。非 SACK 的快速恢复靠 cwnd 膨胀记账，只置标记，不改 cwnd。
3. **退出恢复时**（`leaveRecovery`，唯一出口）：如果 `undoPending` 为真，就不做"cwnd 收缩到 ssthresh"和 `PostRecovery`，改为：
   - `SndCwnd = max(Ssthresh, 快照cwnd)`；
   - `Ssthresh = max(Ssthresh, 快照ssthresh)`，快照值可能是无穷大，表示回到原来的慢启动状态，与 Linux 一致；
   - 调用 `cc.undo()` 恢复 cubic 曲线；
   - 清除标记和快照。
   正常（非误判）退出时行为不变。
4. **快照失效**：RTO 触发（`retransmitTimerExpired`）时先清除快照和 `undoPending`，再走原逻辑，保证 RTO 降窗不会被误撤销。TLP 路径（rack.go:270 进入后立即退出）不会被判为误恢复，存快照无副作用。
5. **观测**：新增计数器 `SpuriousRecoveryUndo`，并把 `undoPending` 加入 tcpFlowDebug 导出，便于验收。

**不变的东西**：丢包检测、重传逻辑、RACK、SACK 记账、RTO 行为全部不变。真丢包（不带 DSACK、TSEcr 不早于重传时间戳）不会触发撤销。

**单测**（gVisor fork 内）：
- 构造乱序：进入 SACK 恢复后，收到回显原始发送时间戳的确认 → 退出恢复后 cwnd/ssthresh 恢复到降窗前的值，计数器 +1。
- 真丢包：恢复期间只收到对重传包的确认 → 正常降窗，不撤销。
- 恢复期间发生 RTO → 不撤销，cwnd=1。

**部署与验收**：
- 下载方向的发送端是"通告路由的节点"的 gVisor，所以先部署 GG（测试路径，我可自行部署），用 `-a` 强制重编。
- GG 路径 8MB 单流 3 次：spurious_recovery 仍可出现，但 `SpuriousRecoveryUndo` 与之接近；tcpFlowDebug 中 cwnd 不再长期低于 20 段；耗时对比 v330（27.8s / 30.2s / 33.2s / 40.4s）明显缩短。
- 通过后部署 JF/QG，再由用户部署 DESKW，复测 elephant（app.js ≤ 12.1s、chunk-vendors.js ≤ 72.5s）和 8 流聚合（不劣于 §3.8 验收的 3.90~5.55MB/s）。

**回滚**：仅 fork 内改动，单独提交；回退该提交、重编即可。

**GG 实测（2026-11-07，v0.8.0-330-dirty，Mac → QG → JF(h_tunnel) → GG 172.18.0.1:18999/f8m.bin）**：

| 轮次 | 耗时 | spurious / undo | sack_recovery | timeouts | cwnd 峰值 |
|---|---|---|---|---|---|
| 1 | 33.9s | 40 / 37 | 41 | 3（一次 RTO 停顿约 8s） | 303 |
| 2 | 23.4s | 21 / 20 | 21 | 0 | 1023 |
| 3 | 24.4s | 22 / 22 | 22 | 0 | 790 |
| 第 4 轮（采 ss） | 22.5s | — | — | — | — |

- 撤销机制达标：undo ≈ spurious，cwnd 不再卡在 5~20（升到 300~1000）。
- 耗时只缩短约 17%（均值 27.2s vs 32.9s）：**瓶颈已移到外层**。内层 srtt 随 cwnd 从 90ms 涨到 1.9s，属排队。
- 定位（GG `ss -tni` 每 0.5s 采样）：GG→JF 有 2 条外层 lane（JF 主动拨入），该单流**全部走 56698**，另一条 56974 闲置（stripe 按流哈希选 lane，`p2p/stripe_transport.go:321`）。56698 外层重传约 10%（≈40KB/s rtx / 400KB/s），内核 cubic cwnd 在 1~26 间跳，吞吐封顶约 400KB/s，Send-Q 从 0 积到 815KB → 正是内层 RTT 膨胀的来源。
- GG 内核只有 reno/cubic（无 bbr），layer4 用户无权加载模块。
- **裸带宽对照（绕过 phaethon）**：GG 直接 `ssh cat f8m.bin` 到 Mac：单流 8MB/20.9s ≈ 375KB/s；4 流并发 32MB/94.5s ≈ 331KB/s。经 mesh 4 流并发同样约 300KB/s（且 4 条流全落在同一条 lane）。突发时单 lane 能到 1250KB/s，随即出现大量重传 → 符合云主机出口限速器（约 3Mbps）特征。
- 结论：GG 路径的上限是 **GG 出口带宽（约 3Mbps）**，与 phaethon 无关；方案 A 后单流 22.5s ≈ 8MB/355KB/s，已基本跑满。单流跨 lane 分发 / 外层 BBR 在这条路径上**没有收益**，不做。GG 路径不适合做吞吐验收，吞吐验收以 DESKW 路径（elephant / 8 流）为准。
- 遗留观察：4 流并发时出现过约 13s 全零吞吐停顿（GG `ss` 采样 648~661s），待 DESKW 部署后看是否复现。

## 四、不做的事

- ❌ mesh 层重组缓冲/序号重排（netstack 的本职，越俎代庖）。
- ❌ HTTP/2 升级（约束内理论最优但协议重写；本方案效果等价，若批聚合延迟成新瓶颈再立项）。
- ❌ QUIC（项目既定约束：数据面 UDP leg 裸传 datagram，QUIC 类已否）。
- ❌ UDP leg 有序化：裸 datagram 本就无序，内流 UDP/UDP-leg 场景不在本方案范围（列为后续观察项）。
- ❌ 硬停滞探活（475136B 事件，独立病理，另行立项）。

## 五、实施步骤

1. **3.1 POST 独占**：p2p.go 合批逻辑 + htunnel_direct.go 完成回调（约 2 处文件）。
2. **3.2 GET 钉死**：htunnel_mesh.go meshMsg/dispatch（约 1 处文件）。
3. **3.3 路由钉死**：mesh.go 转发选路缓存（约 1 处文件）。
4. **3.4/3.5** 批策略 + config。
5. **3.6 stripe 亲和钉死**（追加）：stripe_transport.go 粘性亲和表（1 处文件）。
6. **3.7 dispatch 判定修复**（追加）：meshDispatchLoop 模式判定改纯配置（1 处文件）。
7. **3.8 V2 服务端控制帧优先 + 按字节合批**（追加）：htunnel_mesh.go 投递重写 + htChannel 字段（2 处文件），JF 部署需用户确认。
8. **3.9 审查修复 + F2**（追加）：config/main/admin 开关接线、mesh.go 死代码删除、htunnel_direct.go 有界有序队列、stripe_transport.go 有界等待 + 改钉、p2p.go 入站有界等待、htunnel_mesh.go 控制旁路 + 硬上限 + CoDel + Close 关通道。
5. 本机构建（版本戳）→ VM 部署自测 mesh 起连、控制台正常。
6. QG 部署（中转节点，观察环路/转发指标无回归）。
7. JF + deskzq 部署（用户确认后，生产节点纪律：备份 prev-running、安全 pkill、无参启动、验证版本）。
8. 验收采样（见六）。

混合版本共网无风险：未升级对端表现为现状（无序），不劣于今天；升级两端后生效。

## 六、验收标准

复用取证管线（`/api/mesh` tcpStats + QG 内核 rcv_ooopack）：

1. QG 经 mesh 下载 JF 8MB 包（`jf.phn:443`）：JF tcpStats 60s 差分 **fast_retransmit ≤ 2**、spurious_recovery ≤ 2（基线噪声级）。
2. 单流速率 ≥ 1MB/s（现状 47KB/s），8 流聚合 ≥ 6MB/s。
3. QG 侧内层 socket rcv_ooopack 采样增量 ≈ 0（乱序消除的接收端印证）。
4. 既有功能无回归：mesh 管理面、域名路由、deskzq 控制台、上传 11MB/s 不复测下跌。

## 七、风险与回滚

| 风险 | 缓解 |
|---|---|
| 在途独占导致队头阻塞（某流大批次占住 POST 槽） | 16 槽跨流共享，单流最多占 1 槽；DRR 公平调度兜底 |
| 完成回调丢失（POST goroutine panic/泄漏）→ 流键永不释放 | defer 释放 + 连接关闭时清空 inFlightFlows |
| 路由钉死后单路径劣化无法绕行 | 路由表版本变更即整表重选；仅钉"生命周期内"，不钉"永久" |
| GET 哈希钉死导致单 GET 过载 | 按流均匀分布；waiter 池 64 深，远超 2 条实际并发 |

回滚：配置开关置 false（秒级，无需重启二进制替换）或按纪律退回上次运行版本二进制。
