# 残余滴流乱序源取证（v5，2026-11-06）

| | |
|---|---|
| 状态 | **已批准立项（2026-11-06），Phase A 进行中** |
| 关联 | [elephant 下载滴流专项 v4/v5](../issues/elephant-download-dribble.md)（机制实锤 + 硬停滞已根治 §3.5）、[mesh 每流有序化改造](mesh-per-flow-ordering.md)（三钉死已部署）、[mesh 多链路 stripe 设计](mesh-multilink-stripe-design.md) |

## 一、问题

硬停滞根治（gVisor #14092 + cubic cwnd=0，soak 60/60）后，残余滴流未消：单流 173~726KB/s，8 流聚合 0.2~10MB/s 高方差。最新抓取 QG 内核侧内层 socket `rcv_ooopack` 1469（~45% 分段乱序）——**三钉死（每流在途独占 c608130、服务端 pipe 派发 + 客户端每流有序 POST f0fb969、stripe 每流 lane 亲和）代码全部在部署版本（v0.8.0-325-g2361df1）内，但乱序依旧**。本轮目标：定位 JF→QG 方向上残余乱序的确切环节。

## 二、嫌疑环节清单（按嫌疑度排序）

### S1. 运行时态未核验——三钉死是否真的在生效

部署 ≠ 生效，已有前科：`meshDispatchLoop` 曾以「通道创建时 pipeCnt==0」判定 legacy，导致 3.2 部署后滴流不变（htunnel_mesh.go:137-142 注释实锤，f0fb969 改为纯配置选择）。同类风险本轮逐一核验：

1. **服务端 dispatcher 模式**：`GetAllowFlowReorder()` 在 JF/QF 实际生效值（配置在数据库，bool 零值=false=ordered，但需实测确认没有显式打开）。
2. **客户端每流有序 POST**：`htunnelDirectTransport.Send` 走 `enqueueOrdered` 还是 `sendFrame`（htunnel_direct.go:178-183），取决于同一配置。
3. **stripe lane 亲和**：测试流经的 p2p session 是否包了 stripeTransport、测试期间 lane 数（`snapshotInfo`/admin 可见，`transportForm`）。注意：滴流期吞吐低、sojourn 低，gauge 大概率不触发加 lane——若 lane 恒 0，S3 直接排除，省一轮。

### S2. JF→QG 会话拓扑未摸清

下载流：QG `curl jf.phn:443`（100.2.0.10）→ JF gVisor 发送 → JF mesh 出站 → **会话传输** → QG mesh 入站 → QG TUN → 内核。该会话到底是 `htunnelDirectTransport`（POST/GET 经 server）还是直接 p2p session（stripe over stream）、对端是谁、是否过境 GG（GG flapping 史：[[project-gg-path-instability]]）——决定哪些嫌疑环节在路径上。**Phase A 第一件事就是把这条链路的实际形态画出来。**

### S3. stripe lane 中流失效路径（若在路径上）

`stripe_transport.go` 的每流 FIFO 有三个破口：
1. **队列满/ lane 死 → 落回 primary lane**（Send:398-404、SendBatch:444-450）：流的前帧在 lane 队列/在途，后帧走 primary → 跨路径乱序。
2. **lane add/remove → `dropAffinity` 整表作废**（attachLane:193、removeLane:225）：流改绑新 lane，旧 lane 在途帧与新 lane 帧交错（注释自称 accepted transient，但 flapping 时可能是持续源）。
3. `Send` 单帧路径与 `SendBatch` 路径并行存在，同一流两路径都可达。

### S4. QG 入站 recvLoop 合批竞态（若有序管道未生效）

2 条 recvLoop 共用 `dataRecvCh`（htunnel_direct.go:128-131），channel 保序只限于单发送方；多 GET 上的同流帧若靠 legacy 派发即在此竞态重排。

### S5. 换路瞬态（3.3 已知缺口）

路由表版本变更整表作废重选，换路瞬间旧路在途帧与新路帧交错。若 JF→QG 路径抖动（含 GG flapping 触发的中继切换），等效持续乱序源。

## 三、Phase A：运行时态核验 + 复测基线（只观测，不改代码）

1. **链路拓扑成像**：分别查 JF、QG 的 /api/mesh（会话列表、transportForm、对端），确定下载流经的会话与传输类型；顺带确认该会话是否过境 GG。
2. **有序模式实测**：两边 admin 或日志确认 `AllowFlowReorder` 生效值与 dispatcher 实际模式；必要时用一次单流下载 + JF tcpStats 差分验证 recovery 循环是否仍在（fast_retransmit/spurious_recovery 应仍同量级上涨——滴流未消的旁证）。
3. **stripe 在场性**：下载期间轮询 stripe `snapshotInfo`，记录 lane 数与事件环；若 lane 恒 0，S3 排除。
4. **rcv_ooopack 定向复测**：QG 侧对测试 socket 做 rcv_ooopack 差分（内核级乱序铁锤），同时记录本次的会话形态——与 S1-S3 结论交叉。

产出：链路拓扑图 + 各嫌疑环节在场/排除表 → 若某环节直接实锤（如 dispatcher 仍在 legacy、或 lane fallback 日志出现），直接进修复方案，跳过 Phase B。

## 四、Phase B：逐环节 (flow, innerSeq) 打点定位（只加观测代码）

仅当 Phase A 全部排除后实施。在内层 IP 包上取 (五元组, TCP seq) 作天然序号，在嫌疑环节埋环形缓冲打点：

1. **JF 出站**：peerWriteLoop 成批后（SendBatch 入口）、stripe 每 lane 实际写出前。
2. **QG 入站**：每帧到达 recvLoop 时、投递 TUN 前。
3. 打点 (flow, seq, hop, mono-ts) 入 per-node 环形缓冲（容量 1e5 级，内存可控），暴露于现有 /debug/dump 或 /api/mesh。
4. 单流下载 60s 后两端取缓冲，离线按 flow 分组对 seq 排序比对：**第一个出现 seq 逆序的 hop 就是乱序引入点**。

注意：打点只读 IP 头/TCP 头，不 copy payload；环形缓冲覆盖式丢弃，无泄漏顾虑（gVisor 调试日志拖垮数据面的教训：绝不做逐包同步写盘，dump 时才导出）。

## 五、判据与回滚

- Phase A 实锤环节 → 出定向修复方案（新一轮 plan），不再动观测代码。
- Phase B 定位到 hop → 修复该 hop；若所有 hop 内有序、只有跨 hop 乱序 → 结论收敛为「在途交错」（时延差/多路径），回到 3.3 换路 drain 或单在途机制上修。
- 观测代码全部增量、可整体删除；回滚 = 退回上次运行版本（既有纪律）。

## 六、不做的事

- 不在 mesh 层加按序重组缓冲（用户立场：先消除自己引入的乱序，netstack 本职不越俎代庖）。
- 不动 gVisor fork（本轮与它无关；fork 修复已 soak 定案）。
- 不做网络层劣化实验（netem/tc 禁令）。
