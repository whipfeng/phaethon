# mesh 每流有序化改造 — 消除隧道层乱序源（2026-11-06）

| | |
|---|---|
| 状态 | 已部署（v0.8.0-325-g2361df1）；但**残余乱序未消**（QG rcv_ooopack ~45%），滴流仍在（单流 173~726KB/s）——三钉死代码在场 ≠ 每个环节运行时生效，残余乱序源定位转 [v5 取证计划](mesh-reorder-forensics-v5.md)（2026-11-06 已批准） |
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
