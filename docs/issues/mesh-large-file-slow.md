# mesh 大文件传输慢 —— 深度研判记录

> 2026-11-04 发起。症状：经 mesh 转发网络偶发特别慢，传输稍大文件时慢得离谱。
> 本文档为持久化分析记录（防上下文压缩丢失），含：实现事实（file:line）、dump 实证、
> 主流实现参考对比、嫌疑排序、验证计划、修复方向草案。
> **状态：方案已出 → docs/plans/mesh-datapath-perf-design.md（2026-11-05，三阶段 + 架构研判 §9），待用户批准。**

## 一、数据面实现事实（三路并行精读结论）

### 1.1 P2P 队列与调度（p2p/p2p.go）

| 点位 | 事实 | 位置 |
|---|---|---|
| 帧格式 | 3 字节头（type+len16）+ payload，MaxPayload 65535，fire-and-forget 无 seq/ack | frame/frame.go:23-41 |
| 入队 | `select{ch<-req; default: drops++; return ErrQueueFull}` —— tail drop 不阻塞；**4 类 drop 均有原子计数**（controlChDrops/dnsQueueDrops/ackQueueDrops/dataQueueDrops[256]，p2p.go:851-857），经 /api/mesh channelDropStats 暴露（但前端 UI 不展示）；drrOutput 阻塞/drrLoop 卡死无计数 | p2p.go:832-861 |
| dataQueues | 256 队列 × **每队列 64**（被动侧 p2p.go:1026）/ **32**（主动侧重连后 p2p.go:1358，"v2: was 256"）——**两侧不一致** | 同上 |
| 优先级队列 | dns 512 / ack 256 / control 512 / drrOutput 512，满即丢 | p2p.go:1211-1229 |
| hashFlow | 五元组 Jenkins+salt mod 256 —— **单条 TCP 流固定落 1 个队列** | p2p.go:947-991 |
| DRRScheduler | quantum=1500；**deficit≤0 只补充、从不阻止出队** —— 实质纯 round-robin，非真 DRR | p2p.go:222-332 |
| drrLoop | NextBatch(64) 一轮扫 256 队列每队列最多取 1 包 → 单流每轮产 1 帧 | p2p.go:748-765 |
| peerWriteLoop | 每 peer 唯一写 goroutine；先非阻塞排空 control→dns→ack 再 select drrOutput（优先级次序正确）；聚合最多 64 帧 → SendBatch | p2p.go:573-742 |
| SendBatch | `make([]byte,total)` 全量 memcpy 后单次 conn.Write；**返回值 n 被丢弃，部分写（无 err）未检测——帧流破坏隐患（正确性 bug）** | frame/transport.go:136-187（:174） |
| 单帧退化 | batch=1 时走 Send → WriteFrame = **2 次 Write syscall**（3 字节头 + payload 分开写） | frame/frame.go:102-118 |
| 写特性 | streamTransport.Send 无写锁、**无写 deadline**，慢写阻塞全 peer | frame/transport.go:115-132 |
| meshInbound | meshInboundCh cap 4096 × 4 worker，满即丢（meshInboundDrops）；DNS 独立通道 4096 × 2 | p2p.go:44-47,467,769-783 |
| ping | sendPing 刻意走数据队列（isControl=false）；enqueue 失败即 **cancelPing(seq) 回滚**（p2p.go:2031-2033，:2030 注释明示），drop 的 ping 不计入 SRTT —— 故意测真实拥塞路径 | p2p.go:2008-2033 |
| transport | 三种 dialer 均返回 NewStreamTransport(conn)，纯 TCP 帧流；UDP leg 仅 p2p-udp 时 | dialer/*_bind.go |

### 1.2 mesh 链路层（mesh/）

| 点位 | 事实 | 位置 |
|---|---|---|
| LinkNIC.WritePackets | 逐包 Flatten 后同步 sender.Send，**无批合并**；发送失败仅 warn+continue（静默） | mesh/link_nic.go:139-181 |
| LinkNIC MTU | 1600 | mesh/link_nic.go:43 |
| NIC1 channel endpoint | `channel.New(8192, 1600)` —— **满即 gVisor 静默丢** | mesh/netstack.go:744 |
| mesh 层队列 | criticalCh cap 256（阻塞）/ normalCh cap 1024（**非阻塞丢**） | mesh/mesh.go:352-353,2409 |
| netstack writeLoop | ReadContext 排空后逐包写 TUN，每包一次 syscall，无 GSO/GRO | mesh/netstack.go:1873-1914 |
| MTU 不一致 | netstack/LinkNIC 1600 vs TUN 设备 1500：>1500B 内层包回程写 TUN 失败仅记日志丢 | mesh/netstack.go:1976；tun/device_*.go |

### 1.3 端到端背压（关键结论）

- 背压链：**对端消费慢 → leg TCP 读慢 → Send 阻塞 → peerWriteLoop 卡 → drrOutput 满 → drrLoop 卡 → 单流 dataQueue（32/64 包 ≈ 48/96KB）满 → tail drop**。
- 帧层无重传 → 内层 TCP 端到端 **RTO**（gVisor 默认 **NewReno**，RTO 即 cwnd=1）→ 吞吐锯齿塌缩。
- pingTracker/rttSampler 只测速，**不参与流控**（p2p.go:1769-1956 无任何 pacing/window 消费点）。
- gVisor TCP 参数全默认、**全仓零调优点**（grep ReceiveBufferSize/CUBIC 零命中，netstack.go:747 仅 keepalive）。
- 单流穿链：TUN readLoop（每包 syscall）→ NIC1(8192) → gVisor 转发 → LinkNIC 逐包 → dataQueue → drrLoop → drrOutput → 单 writeLoop → 单 leg TCP。任何一环串行化都限死单流。

## 二、dump 实证（2026-11-04 VM 留跑取证期产物）

### 2.1 18:30:48 dump（A-B 缺口 106215，"stall"事件）
- `A(tun-read)` 持续增长、`B(netstack-write)` 冻结的签名。
- 栈证据：**全部 peerWriteLoop parked 在 select（p2p.go:694）、全部 drrLoop parked 在 NextBatch（p2p.go:302）、meshInboundLoop 全 parked（p2p.go:771）—— P2P 写路径完全空闲，无背压堆积**。
- netstack writeLoop（goroutine 39）parked 在 channel endpoint `ReadContext`（gvisor-fork channel.go:183）—— **gVisor 栈不再产出出站包**。
- 推断：A-B 缺口包在 TUN readLoop 与 netstack 之间消失 → NIC1 channel endpoint（8192）满后 **InjectInbound 静默丢**（gVisor 语义，无计数）。stall 的根在 netstack 内部冻结（与 11/03 gVisor 调试日志事件同族），但与吞吐问题共享"有界队列+静默丢"模式。
- 注：18:30 与 12:33 两个 dump 版本均为当日 dirty/dev 实验构建，非正式版。

### 2.2 12:33:02 dump（A-B 缺口 9284，uptime 42m）
- `disposition: ping-healthy`（gg/qg peer 全活，srtt 117/37ms，consecLost=0）→ 物理链路活。
- 同 2.1 签名：写路径 idle + netstack 不产出 → **物理活、栈内冻结**分支。

## 三、主流实现参考对比（回答"主不主流"）

### 3.1 gVisor netstack 缓冲与拥塞（fork 实测定论，见第八节）

**fork（../gvisor-fork，基准 upstream v0.0.0-20250428193742-2d800c3129d5）实测值**：
- tcp 缓冲 range = {Min 4096, **Default 1MB, Max 4MB}**（protocol.go:574-583）；**接收/发送 autotuning 均默认开**（moderateReceiveBuffer protocol.go:587；发送侧 computeTCPSendBufferSize endpoint.go:3335-3369）
- 默认拥塞控制 = **Reno**（protocol.go:537-540）；**CUBIC 实现存在于本版本**（cubic.go 含 HyStart），`SetTransportProtocolOption(CongestionControlOption("cubic"))` 一行可切（protocol.go:298-309；可用列表 TCPAvailableCongestionControlOption = "reno cubic"）；**#11632 溢出修复已含于 base**（snd.go:1100-1103 有注记）；无 BBR
- **通告窗口 = 当前接收缓冲的一半**（endpoint.go:67-71 `rcvAdvWndScale=1` 硬编码、:1251-1253 `wndFromSpace=space>>1`）—— 连接初期 = Default/2 = **512KB**，autotuning 顶到 Max 4MB 后 = **2MB 窗口上限**（:1303-1351 只增不减）；**对半折损是硬编码行为，调 Max 才能抬顶**
- 初始接收窗口 ~29KB（InitialCwnd×MSS×2），autotune 爬坡（RTT 内 copied×2+16×amss，慢启动期再×2）
- **SACK 默认开**（protocol.go:584）、**RACK 恢复默认**（:595）、timestamps 实质总是启用（connect.go:354/572）、window scaling 按 maxReceiveBuffer 自动（:3115-3126）
- **MinRTO = 200ms**（snd.go:33 const，但 stack 级 TCPMinRTOOption 可调 protocol.go:346-356）、InitialCwnd=10（硬编码 const 无选项）、MaxRetries=15
- **phaethon 侧零调参**：全仓 grep SetTransportProtocolOption 零命中（netstack.go 仅 keepalive 用 SetSockOptInt）；栈级调参**全部可在 phaethon 侧完成，不用动 fork**；forwarder accept 的连接继承 stack 级设置（newEndpoint endpoint.go:888-921）
- **Coder 案例（5 倍吞吐修复）**：窗口恒停 512KB = Default 缓冲一半；吞吐 = window/RTT——本项目 autotune 后顶到 2MB，但 ÷2 折损与爬坡仍同族问题；2MB ÷ 100ms ≈ 160Mbps 单流硬顶
- **Tailscale 实测表**（issue #21651，~60ms RTT）：kernel TCP 293Mbps / netstack Reno **22Mbps** / netstack CUBIC 68Mbps —— Reno 是主流明确弃用的默认；CUBIC 3 倍于 Reno
- 另有 netstack 对**包重排极敏感**（1% 包延迟 200µs → 0.33 vs 3.58Gbps，Tailscale #21651）
- 结论：**缓冲 range Max 与拥塞控制两项全默认 = 不主流**，是单流跨节点吞吐的第一道硬顶；SACK/RACK/TS 默认已开无需动

### 3.2 DRR 真语义（Shreedhar & Varghese 1995；Linux sch_drr.c / tc-drr(8)）
- 真 DRR：active list + 每类 deficit；出队条件 `head_pkt_len <= deficit`，满足则扣减并连续出队；不满足则 `deficit += quantum` 并移到队尾；**空队列从 active list 移除且 deficit 归零**；quantum ≥ 最大包长保证 O(1)。
- 本项目实现：deficit 从不阻止出队、每轮每队列最多 1 包、空队列靠 256 次 select-default 扫描 —— **不是 DRR，是带 deficit 变量的 round-robin**。fq_codel（Linux 默认队列）用的是改良 DRR + CoDel 丢包，也比这个先进。

### 3.3 TCP-over-TCP 隧道（Honda et al. 2005 "TCP meltdown"；CMU "TCP Tunnels: Avoiding Congestion Collapse"；MPTCP 隧道研究 TCD）
- 内层 TCP + 外层 TCP 双重可靠性不compose：隧道排队延迟 > 内层 RTO → 伪重传 → 外层拥塞回退 → 延迟更大 → 正反馈塌缩。
- 单隧道多流 = HoL 阻塞；隧道可把感知 RTT 抬升 280%。
- 业界解法是 UDP 化（QUIC/WireGuard）或严格限速缓冲 + SACK + BBR。
- 本项目：leg=TCP 且**无任何缓冲调优/SACK 配置**；tail drop 直接把"延迟> RTO"变成"真丢包+RTO"，比经典 meltdown 更糟（meltdown 至少不真丢）。

### 3.4 丢包管理（bufferbloat 共识）
- 浅队列 + tail drop（本项目 32~64 包）是教科书反模式：满即丢造成 TCP 锯齿；主流是 fq_codel/CoDel（按队列延迟丢包）或 BQL（按在途字节限制队列）。
- 主流内核路径队列满 → 背压（阻塞发送方或 NET_TX_BUSY），**而不是丢内层包**。

## 四、嫌疑排序（综合实现+ dump + 参考）

| # | 嫌疑 | 证据 | 主流性判定 |
|---|---|---|---|
| 1 | gVisor 缓冲 range Max 4MB 与拥塞控制 Reno 全默认（SACK/RACK/TS 默认已开） | netstack.go 零 SetTransportProtocolOption；Tailscale Reno 22 vs CUBIC 68Mbps | 不主流，单流硬顶（2MB 窗口 ÷ RTT） |
| 2 | 背压断点：dataQueue 满即 tail drop + 帧层无重传 → 内层 RTO 塌缩 | p2p.go:832-861；link_nic.go:171 静默 continue；dump 显示同模式 | 反模式（主流=背压/CoDel） |
| 3 | 单流队列深度 32/64 包（≈48/96KB），RTT 37~117ms 下吸收 <1.3ms 速率差；两侧不一致 | p2p.go:1026 vs 1358 | 深度无依据，注释自称 "CUBIC depth" 但算法是 Reno |
| 4 | MTU 1600 vs 1500，回程 >1500B 包写 TUN 静默丢 → 随机 RTO | netstack.go:744 vs device_*.go；netstack.go:1976 | 反模式（主流=MSS clamp/统一 MTU） |
| 5 | 假 DRR（deficit 不阻止出队、每轮 1 包、256 空转扫描） | p2p.go:279-287 | 与 sch_drr/fq 语义不符 |
| 6 | 写路径常数开销：单帧 2 syscall、SendBatch 全量 memcpy、LinkNIC 无聚合、TUN 每包 syscall 无 GSO | frame.go:109/113；transport.go:153；link_nic.go:152 | 低于主流（writev/GSO 是标配） |
| 7 | ~~ping 走数据队列可被 drop → SRTT 失真~~ **已推翻**：p2p.go:2031-2033 enqueue 失败即 cancelPing(seq)，drop 的 ping 不计入 SRTT，走数据队列是故意测真实拥塞 | p2p.go:2030-2033 | 非缺陷 |
| 8 | stall 事件（18:30/12:33）：netstack 内部冻结 + NIC1 8192 静默丢，无计数无 dump 触发之外的可见性 | dump 栈证据 | 缺 observability，主流有 drop 计数 |

## 五、验证计划（不动代码）

1. **坐实嫌疑 2/3**：传输大文件期间轮询 Mac/VM `admin /api/mesh` 的 `channelDropStats.peers.<node>.DataDrops`，增长即坐实 tail drop。同时观察对端 TCP 重传（`netstat`/性能计数器）。
2. **坐实嫌疑 1**：在 fork 里临时打日志读出 tcp protocol 实际生效的 DefaultSend/ReceiveBufferSize 与拥塞算法；或 netstat 观测单流吞吐上限是否 ≈ buffer/2÷RTT。
3. **坐实嫌疑 4**：日志 grep "write TUN" 失败记录（netstack.go:1976）在传输期间是否出现。
4. 对比实验：Mac↔QG LAN 直连（RTT ~1ms）传大文件 vs 经 GG（RTT ~100ms），若后者塌缩而前者正常 → RTT 相关，指向 1/2/3。

## 六、修复方向（2026-11-04 经三点质疑逐点研判后定稿，待批准，不实施）

> 研判记录：用户质疑背压/ping/单写者三点，逐一代码查证后修订如下。

### 研判关键事实（查证所得）

1. **背压阻塞点 = phaethon 自己的入站 goroutine**：`LinkNIC.WritePackets` 是 gVisor 栈回调，
   执行上下文是调用 InjectInbound 的 TUN readLoop / meshInboundLoop。naive 无界阻塞会 HoL
   冻结同 goroutine 上所有流/所有 peer 的包——与 18:30 dump netstack writeLoop 停摆同形态。
   **结论：不做阻塞背压。**
2. **sendPing 已有 drop 回滚**：p2p.go:2030 注释 "A RED/enqueue rejection rolls back via
   cancelPing"，:2031-2033 enqueue 失败即 `cancelPing(seq)`，被 drop 的 ping 不计入 SRTT。
   ping 走数据队列是**故意设计**（测真实拥塞路径）；移控制通道 = 探针失明。**G 撤回。**
3. **单写者 = 字节流物理约束 + 公平仲裁点**：一条 TCP conn 不可能并发写（多写者加锁后
   吞吐等价单写者还付调度开销）；公平性来自 DRR 调度，单写者只是执行者。现状 DRR 是
   假的（deficit 从不阻止出队，p2p.go:279-287 实际纯 round-robin），公平与效率双失。
   E 不是"拆开"，是把假 DRR 修成真语义，仲裁点不动。

### 改动清单（按优先级）

| # | 改动 | 研判结论 | 风险面 |
|---|---|---|---|
| A | netstack TCP 调优：Send/ReceiveBufferSize→4~8MB、评估 CUBIC（查证 fork 是否含 #11632 溢出修复）、确认 SACK 生效。全走 SetTransportProtocolOption，**phaethon 侧调参，不动 fork** | 保留，嫌疑 1 头号。Tailscale #21651（Reno 22 vs CUBIC 68Mbps）+ Coder 案例（窗口=buffer/2 现象一致）佐证 | 低，纯调参 |
| B | ~~阻塞背压~~ 改为：**队列深度按 BDP 估算**（跨 GG 100ms×50Mbps÷1500≈400+ 帧 ≫ 现 32/64，丢弃点正常工况事实不可达）+ **tail drop 计数与 admin 可见性**（channel.go NIC1 满丢、dataQueue 满丢当前均无计数） | 改形态保留。先可观测再谈行为改造 | 低-中（内存上限需在方案文档论证） |
| C | 主动/被动队列深度不一致（256×64 vs 256×32）统一 | 并入 B | 低 |
| D | MTU 对齐 1500（netstack/LinkNIC 当前 1600 vs TUN 1500，回程写失败静默丢 netstack.go:1976） | 保留，改动小收益确定 | 低，只改常量 |
| E | 真 DRR（active list + deficit 耗尽连续出队）或换 fq 语义 | 保留但降级为可选，排在 A/B/D 后。单写者仲裁点不动 | 中，动调度行为 |
| F | 写路径 syscall 优化（writev/GSO/聚合写/去双 Flatten） | 保留排最后。低于主流是事实，但属倍率优化非塌缩根因 | 中，动帧边界正确性 |
| G | ~~ping 移回控制通道~~ | **撤回**。已有 cancelPing 回滚（p2p.go:2031-2033），走数据队列故意测拥塞 | — |
| H | stall 可观测性：NIC/queue drop 计数、NIC 满触发自动 dump | 新增保留。12:33/18:30 两次 stall 全靠人工撞见 | 低，纯观测 |
| I | fork forwarder.go:78 每包 Sprintf + ipv4 转发路径 ~10 处 LogModuleDebug 删除 | 新增保留，纯删日志 | 近零 |

### 明确不做

- 阻塞/限额等待式背压（HoL 冻结入站 goroutine，见研判事实 1）
- ping 移控制通道（失明，见研判事实 2）
- 拆单写者（物理约束，见研判事实 3）
- 数据面空闲超时（项目铁律不变）

### 遗留尾巴（不进本清单）

- recordPing(p2p.go:1859) 对 stale pending ping 的 aging 参数是否过短——微调项
- mesh stall 取证：12:33 dump 与 18:30 dump 并排对比 B 段冻结栈（见 mesh-data-plane-stall.md）

## 七、gvisor-fork 专项审计（2026-11-04 补）

> 审计对象 `../gvisor-fork`（go.mod replace 指向），34 个 fork 专属 commit（git log 中文 message 那些），
> 基准 upstream v0.0.0-20250428193742-2d800c3129d5。

### 7.1 fork 改了什么（数据面相关逐条）

| commit | 内容 |
|---|---|
| b5e623a | RouteSelector 接入 FindRoute（每连接一次，非每包） |
| c8492e3/f9203a6/c31defc/5d292d1 | IPIP 转发封装 + 外层 src/dst=EIP |
| d3adc22 / 8aeaf43 / 5670ffa | DeepCopyForForwarding 保 NIC 身份 / 转发 panic 防御 / 转发路径细化 |
| 11a05cd / dbf904c | DNAT 出向走 OutputNIC / 本机 socket 跳过 InputInterface |
| 8dcbc6d / 1bbe401 / d777723 | 本地回环 / LinkNIC promiscuous 语义 |
| bd45411 / 26ac249 / b185bd4 | InputNICName / 转发日志接模块化日志 |
| 7f69f4c | conntrack 仅真实 SNAT 记 originalInputNIC（修 mesh reply 丢包） |
| a481bb0 / 6739c92 | 指定链路 socket 补 IPIP 封装/源地址（改了 tcp/connect.go +10 行） |
| **d7df39b** | **删每包 2-8 次 os.OpenFile 调试写 + demux 每包 RLock 全端点遍历（11/03 卡死根因，已清）** |
| d209f6f | 删收包热路径 corrID 每包 Sprintf（channel 侧） |
| 13c1300 | ipv4.go 30 处 Logf→直调 util.LogModuleDebug |
| 5034b18 | routeDecisionCache 有界 4096+TTL 5min，回调移出 s.mu |
| ab8f48a / 43ba4bb / 465a002 | name→NICID 反查 O(1) / 删 Logf shim |
| 0890425 / 1b68992 | IPIP TTL 递减语义 / ICMP Frag-Needed 写 next-hop MTU |

**fork 对 pkg/tcpip/transport/tcp/ 改动极小**：仅 connect.go(+10)/endpoint.go(+27)/forwarder.go(+13)，
**无拥塞控制/缓冲/发送路径算法改动**——tcp 的慢是全默认 + 无调优点，不是 fork 改坏的。
channel endpoint（channel.go）fork 未改。

### 7.2 fork 层面吞吐相关定论

1. **channel 队列满 = 静默丢，无计数**（channel.go:80-107 non-blocking select 满则 DecRef 返回 ErrNoBufferSpace；
   WritePackets 遇满 break 无计数）。phaethon NIC1=8192——大文件突发可触顶，丢的是 TCP 数据段，直接表现=慢。
2. **遗留分配点**：`tcp/forwarder.go:78` 每包 `fmt.Sprintf(corrID)`（mesh 用 tcp.NewForwarder，握手期/乱序包热路径）；
   connect.go:592 每 SYN 一次（可忽略）。
3. **11/03 同类隐患仍在机制上**：ipv4 转发路径每包 ~10 处 LogModuleDebug 调用点（ipv4.go:814,890,913,947,959,1005,1506…），
   禁用时仅级别检查（廉价），但运行时开 debug + 文件 Logger = 每包格式化+I/O。d7df39b 删的是 unconditional os.OpenFile，
   这条是条件触发的，建议改采样或编译期。
4. **IPIP 封装每包一次外层构造**（routeDecisionCache 已降为每 dst 一次，残留可控）。
5. gonet 零 fork 改动：TCPConn 无内部缓冲，Write 用 bytes.Reader 循环，小写高频逐段进 endpoint。
6. **调优点全部可用，无需改 fork**：`TCPSACKEnabled`/`TCPSendBufferSizeRangeOption`/`TCPReceiveBufferSizeRangeOption`/
   `CongestionControlOption`(reno|cubic)/`TCPModerateReceiveBufferOption` 的 SetTransportProtocolOption 都在
   （protocol.go:260-315）。注意 CongestionControlOption 只影响切换后新建的 endpoint。

### 7.3 fork 审计修正后的关键数字（单流跨节点硬顶估算）

- 默认缓冲 range {Default 1MB, Max 4MB} + autotune → 通告窗口顶 = **Max/2 = 2MB**（初值 Default/2 = 512KB，autotune 只增不减顶到 Max）；RTT 37ms → 2MB/37ms ≈ 432Mbps；RTT 117ms ≈ 137Mbps——**不是带宽不够，是 Reno 爬坡与丢包塌缩让实际远低于此**
- 初始窗口 29KB + moderate 爬坡：传大文件前半段窗口远低于上限
- 遇 tail drop：MinRTO 200ms + Reno cwnd=1，每次丢包损失整数量级时间
- → **即便队列不丢包，Reno 默认下单流也远低于窗口上限；丢包时直接塌缩**，与症状完全吻合
- 注：fork forwarder.go:78-109 诊断日志可安全删除——corrID Sprintf 在 SYN 判断之前、每包无条件执行（唯一无门控热路径开销），4 处使用全是 LogModuleDebug 参数，功能零依赖；删除时一并移除 fork 的 phaethon/util import（a481bb0 引入）

## 八、参考来源

- Coder: How We Improved the Performance of a Userspace TCP Stack in Go by 5X — https://coder.com/blog/delivering-5x-faster-throughput-in-coder-2-12-0
- Tailscale issue #21651（Reno vs CUBIC 实测、重排敏感性）— https://github.com/tailscale/tailscale/issues/21651
- gVisor stack 默认缓冲（pkg.go.dev）— https://pkg.go.dev/gvisor.dev/gvisor/pkg/tcpip/stack
- gVisor #11632（CUBIC cwnd 溢出 panic）— https://github.com/google/gvisor/issues/11632
- GitLab runsc 网络调优 MR — https://gitlab.com/gitlab-com/gl-infra/argocd/apps/-/merge_requests/1197
- DRR 原始论文（Shreedhar & Varghese）— https://web.stanford.edu/class/ee384x/EE384X//papers/DRR.pdf
- tc-drr(8) — https://man7.org/linux/man-pages/man8/tc-drr.8.html
- Deficit round robin（Wikipedia）— https://en.wikipedia.org/wiki/Deficit_round_robin
- TCP-over-TCP meltdown（Honda et al. 2005 摘要）— http://ui.adsabs.harvard.edu/abs/2005SPIE.6011..138H/abstract
- TCP Tunnels: Avoiding Congestion Collapse（CMU）— https://www.cs.cmu.edu/~rajesh/papers/tunnel1-3.PDF
- TCP over QUIC tunnels（arXiv 2504.10054）— https://arxiv.org/abs/2504.10054
- MPTCP 多 WAN 隧道测量（TCD/Leith）— https://www.scss.tcd.ie/doug.leith/pubs/1570723294.pdf

## 九、架构级研判（2026-11-05，回应"组织架构是否合理"的质疑）

### 9.1 per-P2P-link 一个 LinkNIC 是否合理？

**事实**（agent 精读）：栈内 NIC 两类——NIC1 = TUN 侧 channel endpoint（8192 深队列）；
NIC 2..N = 每 peer 一个 LinkNIC（`CreateLinkNIC` netstack.go:267，peer 建立时新建、
断开时 RemoveNIC，ID 单调递增）。gVisor 路由表恒空（`SetRouteTable(nil)` :322），
全部路由决策走 fork 的 RouteSelector 回调（4 分支：本节点子网 / mesh 直连 /
IPIP 外网前缀 / 兜底本地栈）。

关键发现：**LinkNIC 在 gVisor 里不承担队列、ARP/邻居、conntrack、防火墙中的任何一样**——
- 无队列：WritePackets 同步直发 sender.Send（link_nic.go:139-181），排队全在 p2p 层
- 无 ARP：Capabilities=CapabilityNone、ARPHardwareType=None（link_nic.go:116-126）
- conntrack/iptables 栈级单实例共享，与 NIC 数无关

即 per-link NIC 买的只是两样东西：(a) fork 转发路径的出口句柄 (b) 入向帧的源链路
身份（conntrack patch #5 按 pkt.NICID 区分）。这两样都有更便宜的替代（出向查表/
入向传 fromNodeID），且 peer 抖动 = CreateNIC/RemoveNIC 持栈锁 + 决策缓存失效，
fork patch #14（决策自愈）正是为 NIC 抖动打的补丁。

**研判结论：当前形态不是性能瓶颈**（LinkNIC 无队列无额外拷贝，开销只是两个协议
endpoint 结构），但它是**复杂度集中点**——单 NIC 化需要迁移 fork patch #3/#5/#6/#12/#14
的语义（conntrack 入向身份是最大改动面），收益是 peer 生命周期从栈层消失。
**本轮不做**：性能收益不明确、fork 改动面大、风险不对称。记为长期架构候选。

### 9.2 单 TCP 连接承载全 peer 流量的 HoL 问题（真架构级缺陷）

确认：两节点间**一条 P2P 连接**承载全部帧（mesh 数据/DNS/IPIP/gossip/ping），
单 peerWriteLoop 单 goroutine 顺序写（p2p.go:573）。UDP leg 存在但按帧类型分流、
非常驻。后果：

- **TCP 字节流 HoL：外层 TCP 一个丢段 → 重排窗口内所有流全部停顿**，
  与内层流数无关。DRR 公平调度只解决本地排队次序，解决不了外层 HoL。
- **TCP-over-TCP meltdown**（§3.3）：relay 路径（trojan/socks5 本身也是 TCP）
  上内层 Reno + 外层 tail drop = 正反馈塌缩，比经典 meltdown 更糟（真丢包）。
- 主流解法：隧道 UDP 化（QUIC——Tailscale ts2021 协议、WireGuard）或
  MPTCP 多连接。**本项目已有 UDP leg 基建（udpLegTransport），mesh 数据帧
  走 UDP leg 可绕开外层 TCP HoL**——这应是架构演进的第一候选，
  但属协议形态变更，不并入本轮调优。

### 9.3 TUN readLoop 单 goroutine 全栈串行化

TUN 入站唯一 goroutine（tun/manager.go:763，LockOSThread）内同步跑完整 gVisor
栈处理（InjectInbound → DeliverNetworkPacket 全链）。阻塞 = 全部 TUN 入站 HoL。
当前包率下 CPU 非瓶颈（Mac 实测 0%），记录为规模上限隐患，不动。

### 9.4 其他确认合理的设计（不再质疑）

- 控制/dns/ack 三优先队列 + DRR 数据队列的分层：符合主流 QoS 分层
- ping 走数据队列 + cancelPing 回滚：设计正确（测真实拥塞）
- 无写 deadline（让 TCP backpressure 传播）：符合主流
- 每 peer 单写者：字节流物理约束，必然正确

## 十、两环境部署实测（2026-11-05，v0.8.0-301-g040e55f）

部署：QG = linux/amd64 OpenRC（stop→zap→start，回退保险 /root/phaethon.prev-running）；
Mac = darwin/amd64（**该机是 Intel x86_64，arm64 报 Bad CPU type**，回退保险
~/phaethon-test/phaethon-prev-running）。两侧版本戳均为 v0.8.0-301-g040e55f。

测试方法：50MB urandom 文件，scp 双向各 3 次取区间，md5 校验完整性。

| 方向 | 旧版（QG prev-running） | 新版（v301） |
|---|---|---|
| VM→Mac | 28s | 26–79s（持平/噪声） |
| Mac→VM | 110s | 51–81s，中位 69s（**≈1.6x**） |

可观测性结论（全程）：
- tunWriteDrops / nicTxDrops / DataDrops / ControlChDrops 全 0
- 零 QUEUE FULL、零 budget exhausted 日志
- 日志零新增 ERROR

遗留定性：**吞吐分钟级大幅波动 + 偶发长时间卡死为存量问题**——旧版 QG A/B 回退
复现同样症状（首次 Mac→VM 卡死 2.35MB + curl VIP 21s 超时均在旧版复现），
与 GG 链路单点 flapping 相关（见 [[project-gg-path-instability]]）。
架构演进第一候选：UDP leg 常驻化绕开外层 TCP HoL（§9.2）。
