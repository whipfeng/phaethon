# Mesh 数据面卡死：轻则断流、重则整机网络栈挂死

**状态**: 部分闭环（2026-11-02）：gossip 竞态 panic 已修复（e77ff5f）；VM↔QG 链路修复经 patch #15（6739c92）验证；dump `goroutine-dump-20261102-135925` 证明 rule 0 可合法触发且 VM 内部 goroutine 全健康；B 触发源（数据面停滞）仍未实锤，精准 dump 阶梯已实现待部署取证
**严重级别**: 高（曾导致整机网络栈挂死，OS 被迫重启）
**首次发现**: 2026-11-01 ~10:13（VM 经 QG 出口 curl exit=28）

## 症状（两级，疑似同一根因）

1. **轻症：数据面卡死**
   - VM↔QG 之间所有新建 TCP 连接无法完成数据交换（双向）
   - 控制面（gossip/路由/DNS trie）完全正常
   - QG↔GG 等其他节点对的数据面正常
   - QG 本机出口正常
2. **重症：整机网络栈挂死**
   - 2026-11-01 ~12:09 前，VM（Windows 本机）整体挂死，外部 SSH 无法连接，重启操作系统才恢复
   - `LastBootUpTime = 2026-11-01 12:09:27`

## 时间线（2026-11-01）

| 时间 | 事件 |
|---|---|
| ~10:13 | QG 当日早前重启后，首次发现 VM 经 QG 出口 exit=28 |
| ~12:0x | VM 整机挂死，SSH 不可达 |
| 12:09:27 | Windows 强制重启恢复 |
| 11:25:58 / 12:52:44 / 13:07:19 | VM phaethon 多次重启（TUN idx 10→7→7） |
| 12:56:19 | QG 部署 v0.8.0-226-g9231944 |
| 13:09:34 | QG 收到 VM 发来的 7890 SYN，完成握手，但 socks5 数据 30s 无流量 |
| 13:10:43 | VM→QG:32458 偶发成功（telnet 连通） |
| 13:11:07 | VM→QG:7890 连接 52123：`CreateEndpoint` **永久挂起**（>20 分钟未返回） |

**关键判定：干净重启（12:09）+ 全新 226 双端，exit=28 依然复现** → 排除脏状态，是现行代码真实 bug。

## 实测证据链

### 排除项（已证伪的方向）

1. **与 226 提交无关**：6d440c6 / 217 / 225 / 226 多版本组合均复现。
2. **非 VIP/路由认知问题**：历史测试地址 `100.107.0.1` 已不存在（QG 现 subnet `100.0.0.0/16`）；正确服务入口是 hostIP `100.0.0.2`（VIP `.1` 按 route_selector.go 设计是 conntrack/SNAT 中转地址，`LocalStack=false` 非服务地址）。用正确地址测试症状相同。
3. **非 Windows 路由丢失**：split-tunnel `0.0.0.0/1` + `128.0.0.0/1` 经 TUN 路由实际存在（此前 `route print` 受 GBK 编码干扰误判）。
4. **非 netstack 全面死锁**：QG 累计 5737 个 forwarder `CALLBACK entered`，5736 个进入 `HANDLECONN-GOROUTINE`——仅 1 个挂起。数据面对其他节点（GG）正常。

### 卡死点实锤

- QG netstack 收到 VM 的 SYN（forwarder 打日志），`CreateEndpoint`（gVisor `performHandshake`：发 SYN-ACK 等 ACK）**20+ 分钟不返回**。
- 正常情况 SYN-ACK 重传超时应在数分钟内返回错误；永久挂起说明卡死在比重传定时器更深的位置（如：发送路径阻塞在满队列/锁上，定时器无法触发或报文无法入队）。
- VM 重启后 worker PID 14120 无日志静默更换为 10832；Windows 事件查看器无崩溃记录。

### 双 qg 会话定性（2026-11-02 用户确认）

- VM 侧 `RECOMPUTE-ROUTES peers=[qg qg]`（两条 qg 会话）、QG 侧 p2p 列表两条 vm 会话，是**正常配置**，不是拓扑异常——就是启动了两个代理各自建立的 p2p。
- 多会话**不是 round-robin**：每条链路独立测量质量、独立参与最优路径计算，最优路径选定后会话折叠为一条（QG 侧 jf 双会话同理）。
- 旧表述"流量被分摊到两条会话、其中一条已死的竞态"作废；死链的正确处理依赖 pingTracker ~10s 自判关链 + 最优路径重算（见根因链条③修正）。

## 根因（静态审查实锤，2026-11-01）

### 根因链条

1. **htunnel 服务端 Send 无超时 + 客户端心跳无限续命 → peerWriteLoop 永久卡死**
   - `server/htunnel_mesh.go:238` `meshChannelTransport.Send/SendBatch` 阻塞在 `ch.meshOut`（4096 帧），唯一解除途径是 `ch.closed`，无任何超时
   - `server/htunnel.go:967` `resetReqTimeout`：任何请求到达都重置 30s 关闭定时器
   - `dialer/htunnel_direct.go:502` VM 侧 `heartbeatLoop` 周期性 PUT 心跳——即使 VM 的 GET 读侧已死，心跳照样发，QG 通道永不超时关闭
   - 结果：QG 的 `peerWriteLoop` 永久阻塞在 Send，无错误、无 warn 日志、会话不清理不重连
   - 注：p2p 层 `enqueueWrite`（p2p/p2p.go:769）本身非阻塞（select/default + RED 丢弃），不背锅；唯一阻塞点在 transport 层
2. **gVisor fork 被动握手 SYN-ACK 无限重传 → `CreateEndpoint` 永不返回**
   - `gvisor-fork/pkg/tcpip/transport/tcp/connect.go:610` `retransmitHandlerLocked`：被动握手等 final-ACK 时无 maxRetries 上限，指数退避（上限 60s）后无限循环
   - SYN-ACK 写入死队列后握手永远等不到 ACK，"卡死 >20 分钟"完全解释（定时器正常，只是设计上就不放弃）
3. **双会话无去重 + 每条链路独立参与最优路径计算 → 死链未被及时发现**
   - `mesh/topology.go:137` `RegisterPeer` 直接 append，无 nodeID 去重
   - 同 nodeID 多会话是**正常配置**（如双代理各自建立的 p2p），**不是 round-robin 负载分摊**：每条链路独立测量、独立参与最优路径计算（设计 §2.8 多链路特性）
   - ping/pong 数据面探针（1s 间隔 × 阈值 10）会在 ~10s 内自判死链并自动关链触发重算——死链本应快速自愈，"偶发能通一次、之后全挂"指向死链发现/切链环节的问题，而非"流量被对半摊到死活两条链路"的 round-robin 竞态（旧表述已修正）
4. **客户端写失败被静默吞掉**
   - `dialer/htunnel_direct.go:218` `postBatch` 失败仅 debug 日志（Send 立即返回 nil，实际 POST 在 goroutine 里），上层永远收不到错误

### 触发源（B 链审查，2026-11-01 下午）

B 链 = VM 停止发 GET 的原因链：`recvLoop → routeFrame → readLoop(Recv) → HandleMeshFrame → LinkNIC.InjectInbound → gVisor DeliverNetworkPacket`，及 TUN 层。

**已排除（代码级无永久阻塞）**：
- `client.Do` 有 ctx 超时（waitTime+10s）
- `meshInboundCh` 入队非阻塞（满了丢帧，p2p/p2p.go:1559）
- `criticalCh`（256）仅在 criticalWorker 卡死时阻塞，criticalWorker 各 handler 纯计算
- TUN 写出：`channel.New(8192)` 缓冲满丢帧不阻塞（netstack.go:662）——writeLoop 卡死不拖垮 netstack

**未排除（需 goroutine dump 定罪）**：
- `DeliverNetworkPacket` 同步执行整个 gVisor 包处理，会拿栈内锁——任何持锁卡死的 goroutine 都会拖住 meshInboundLoop worker
- Windows 网络栈层面异常（OS 级，代码不可见）

**12:09 重症闭环**：TUN 读循环卡死（wintun ReceivePacket 永久阻塞，`tun-read-stall` 同款）+ /1 路由黑洞 → 本机所有流量黑洞 → 外部看整机死亡。此机制不依赖驱动层死锁假设，但本地控制台当时未验证。

**结论**：静态审查已尽，B 的确切触发源只能靠运行时实锤（pprof dump）。

### 与症状对照（全部自洽）

- 干净重启仍复现：结构性 bug，与脏状态无关
- 单向卡死（VM→QG 通、QG→VM 死）与 SYN 能到、回包永不回一致
- 5737 回调仅 1 挂起：只有 VM 那个 peer 的 writeLoop 卡死，其余正常
- gossip/控制面正常：控制面走 VM→QG 入向（通畅）+ 双会话中活的那条出向

## 修复候选 —— 最终研判（2026-11-01，按"治本不治标"标准）

| 项 | 决策 | 理由 |
|---|---|---|
| ① Send 超时 | **撤销** | 与②同类（检测到停滞→杀会话重连），检测信号是拍脑袋的秒数 |
| ② htunnel 通道出站停滞检测 | **撤销** | 与①同类，信号更本质但本质相同：不治病，只把永久静默变重试循环。用户论断"心跳足够，不来取是别的原因（B）所致"成立——杀会话重连治不了 B，B 还在新通道照样卡。风险记录在案：未来若出现新的未知触发源致客户端永久不读，只能人工重启 |
| ③ egress 健康度选路 | **撤销** | 绕开死会话而非让死会话去死，且会掩盖配置错误（如重复代理配置） |
| ④ 双会话去重 | **撤销** | 同 nodeID 多会话是正常配置（双代理各建一条 p2p），不是 round-robin 分摊；每条链路独立参与最优路径计算是设计 §2.8 多链路特性，非 bug；问题从来不在重复，而在死会话不被发现 |

**四项修复全部撤销。治本全部押在 B**（触发源：VM 停止取 GET 的真实原因）。B 的实锤依赖阶段二停滞 dump。

## 卡死复发时的第一动作：物理通道分叉判据（2026-11-01 新增）

用户提出关键质疑："SSH 也连不上——SSH 是被动监听的 socket server，其回程包会路由到 TUN 吗？"经研判与实测修正，结论如下。

**实测（VM 健康态）**：
- VM `172.30.0.2` ↔ 客户端 `10.11.61.50` 不同网段，SSH established 会话活着，ping 1ms，TUN 计数器零增量——**SSH 回程物理直连，不进 TUN**。
- 机制（推断，无官方文档）：split-tunnel 的 `0.0.0.0/1`+`128.0.0.0/1` 路由虽在路由表，但 Windows 源地址感知路由查找下，sshd accept 的连接绑定本地地址 `172.30.0.2`，出站查找带源约束，跳过挂 phaethontun 的 /1，落物理默认路由。佐证：Find-NetRoute（不带源参数）选 TUN，而实际绑源 socket 不走 TUN。
- 推论：若 SSH 回程真进 TUN，须走 RouteSelector 分支4（`LocalStack:true`，栈内消费"local proxy entries"），该分支无法把回程包交回物理网卡上的既有连接——将不可达，与"平时 SSH 通"矛盾，反证回程没进 TUN。

**分叉判据（B 排查的关键分野）**：

| 卡死时物理通道 | 结论 | 下一步 |
|---|---|---|
| 物理通（能 ping/SSH 172.30.0.2）、mesh 死 | phaethon 数据面内部问题 | 查 `data/logs/goroutine-dump-*.log`，按计数器签名定位 B 在 recvLoop/InjectInbound/TUN 读循环哪一段 |
| 物理也死 | **OS/驱动层嫌疑**（wintun 驱动状态、Windows TCP/IP 栈、系统事件日志、内存/句柄泄漏） | 本机控制台现场取证：驱动状态、事件查看器、进程句柄数；dump 文件的 Stats 快照辅助 |

12:09 重症（整机挂死+SSH 不可达）按此判据属于右列——这是"OS 自身问题"假设的正向证据，也解释了为什么 stall monitor 的计数器冻结签名在重症形态下可能整体不可用（dump 触发依赖 writeLoop 活着写探针）。

**重症形态的取证现实（用户指正后明确）**：整机挂死时进程内巡检/上报全部失效（任务就跑在死机上），用户只能重启。因此重症形态**唯一可靠的证据是重启前已落盘的东西**，重启后按以下清单逐项查：

1. `data/logs/goroutine-dump-*.log`——StallMonitor 死前若触发过，B 的定位证据在里面（含 Stats 快照与探活结果）
2. Windows 事件查看器——以 LastBootUpTime 往前推死前时间窗，查系统/驱动错误（wintun、TCPIP、NDIS 相关来源）
3. phaethon 自身日志的断尾——data/logs/ 下最后一条日志是什么，卡在哪一步
4. 若有 1 或 2 的实锤 → OS/驱动层方向（驱动状态、句柄泄漏、系统资源）；全空 → 倾向纯数据面重症，回到 dump 分析

## 根因更新：gossip 竞态 panic = 重启循环真凶（2026-11-02 实锤并修复）

留跑巡检期间 watchdog 日志（2026-11-01 23:04 起落盘）把 worker 自发重启全部定罪为 **exit code=2 = Go panic**，panic 原文 `panic: JSON decoder out of sync - data changing underfoot?`。VM 单日 6 次（04:54/05:08/05:35/06:08/06:42/07:25），间隔随 gossip 负载收紧到 25-30 分钟；stdout 历史日志共 113 次。**13:09:01 的 panic 与原始卡死时间线（13:09:34 QG 收 VM SYN）重合——worker panic 重启即原始症状的触发点候选。**

竞态链（三个文件串起来）：

1. `p2p/p2p.go` recvLoop：`peer.transport.Recv()` 返回**池化缓冲区**切片（issue 10 buffer pool，专属于数据面 FrameMeshPacket 的借还协议）
2. `p2p/p2p.go` handleGossip：Unmarshal 校验后把**同一块池化切片**原样转发 mesh 层（issue 7 注释，避免 marshal 往返）
3. `mesh/mesh.go` HandleTopologyGossip → `criticalCh <- meshEvent{data: data}`：**裸塞阻塞队列无拷贝**，函数立即返回，recvLoop 下一轮 Recv() 复用缓冲区
4. `mesh/mesh.go` criticalWorker 取出后 `json.Unmarshal`——解析到一半底层字节被下一帧覆写 → panic

**修复（commit e77ff5f，平台无关，所有 mesh 节点同病）**：p2p 已解析的 `GossipInfo` 值拷贝穿通道；mesh 预检 Unmarshal 与 criticalWorker 第三次 Unmarshal 一并删除（原路径同一份 JSON 解析三次）。线上帧格式不变，新旧版本互发完全兼容；数据面零拷贝路径（ReturnBuffer 协议）不受影响。

**待办**：其他节点（QG/JF/GG/MS9/MS10）日志 grep `data changing underfoot` 坐实影响面——其他环境 watchdog 日志在修复前不落盘/不解析 PHAETHON_LOG_MODULES，历史 panic 可能完全静默。

## dump 135925 运行时证据（2026-11-02）

VM 留跑期间 StallMonitor rule 0 触发，写 `goroutine-dump-20261102-135925.log`（445KB）。分析结论：

- **rule 0 是合法触发，非误报**：D（peer 入向 dst=VIP 帧）窗口内流动 212≥minTargetActivity 60，随后冻结 90s+，A（TUN readPackets）持续在走——outbound-blackhole 签名成立。
- **dump 内 VM 内部 goroutine 全健康**：TUN readLoop、writeLoop、meshInboundLoop、criticalWorker、各 peer loop 无一持锁卡死——B 触发源本次未现身（或已自愈）。
- **嫌疑指向 qg 链路侧**：当时 jf↔qg 单点 flapping（GG 方向时通时不通，见 GG 链路不稳定诊断），符合 dead-link-suspect 判态——对端死链自愈场景 dump 价值低。
- **暴露的取证缺口**：dump 前没有任何"哪一环死了"的判态，445KB 全靠人工分析。精准 dump 阶梯（docs/plans/stall-monitor-precise-dump-ladder.md）已实现：L1 KB 级快照行 + 三判态 → L2 双腿探测（gvisor socket→邻居 GIP:53 / 宿主 socket→自己 GIP:53）→ 确认嫌疑才写全量 dump；rule 2 + host 腿活 → 跳过 dump。待部署 VM 后生效。

## 下一步

- **治本 = 找到并修复 B**（触发源）。前置条件：阶段二取证
- **阶段二停滞 dump 已实现**（精准 dump 阶梯，2026-11-02，见 docs/plans/stall-monitor-precise-dump-ladder.md）：关键循环进展足迹（TUN readPackets / WritePackets / peer 入向计数器）由 StallMonitor 旁观；卡死=部分冻结签名（freezeWindow=60s）触发 rule 0/1/2 → L1 KB 级快照行 + 判态 → L2 双腿主动探测 → 确认 phaethon 侧嫌疑才写全量 dump（rule 2 host 腿活 = host-silent 良性直接跳过）。断网可取证：不侵入阻塞调用、不拿业务锁。**待部署 VM 生效**，部署后 `POST /debug/stall-probe` 自检
- 复现验证用跨平台 Win↔Linux 全链路（含 wintun/tun，不绕开 TUN 层）

## 2026-11-03 iperf3 吞吐验证未遂：轻症形态复测记录

深度验证第一项（iperf3 mesh 链路吞吐）未遂，**复测证据与本文档轻症症状完全一致**，记录备查：

- **测试方法**：VM（Windows，winget iperf3 3.22）↔ QG（apk iperf3），QG server 绑 100.0.0.2，双向各试一次
- **结果（双向均失败）**：
  - VM→100.0.0.2:22 SYN 到不了 QG（QG tun0 tcpdump 实证无包）
  - QG→100.212.0.2 无主机路由（mesh-only 中继节点 host 腿结构性不通，符合预期）
  - VM→GG VIP（100.179.0.3:39998）、VM→自己 VIP（100.212.0.1:39999）、VM→QG LAN 192.168.1.100（IPIP 分支 3）全部不通
  - fakeIP 域名（qgw.phn）与域名路由（httpbin.org，owner=jf）超时
- **同期正常项**：VM admin 活性、控制面（gossip 2-5s 路由刷新、bestPaths 齐全、channelDropStats 全 0）、互联网直连代理（1.1.1.1 可达、活跃 DIRECT 连接）
- **判别**：非今日提交引入——今日 5 个提交仅 4b7b40e 碰 mesh（GetRouteTree nil 守卫，纯只读导出路径）+ 4bf4bd9 转发选路修复（症状出现时不相关），其余全为 admin 前端。症状 = 文档"轻症：数据面卡死"原样复现（VM↔QG 双向数据死、控制面正常）
- **残留清理**：QG iperf3 进程+包已删、VM iperf3 进程已杀（winget 包 ar51an.iPerf3 保留备下次）、C:/tmp/iperf3.zip 已删；VM 侧 e2e 脚本（C:/tmp/phn_e2e_tree.py）保留
- **吞吐基线仍缺**：iperf3 验证待数据面恢复后重开；当时 GG linkNIC 仅 39 包（近零数据流量），qg linkNIC ~75 包/10s（纯 gossip 量级）
