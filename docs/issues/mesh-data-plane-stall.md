# Mesh 数据面卡死：轻则断流、重则整机网络栈挂死

**状态**: ✅ 根因已定位（2026-11-01，静态审查代码实锤），待讨论修复方案
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

### 拓扑异常（待确认是否为诱因）

- VM 侧 `RECOMPUTE-ROUTES peers=[qg qg]`（两条 qg 会话）；QG 侧 p2p 列表 id 7/8 均为 vm、均 upToDate。
- QG 侧重复 jf 会话有明确配置来源（`MGMS_HT` + `MGMS_HT (copy)` 两个 h_tunnel 代理）；vm 的两条 Peer proxyName 为空（被动接入），成因待查（见根因链条③：RegisterPeer 无去重 + 重连/双代理均会产生同 nodeID 多会话）。
- 症状"偶发能通一次 + 之后全挂"符合**流量被分摊到两条会话、其中一条已死**的竞态特征。

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
3. **双会话无去重 + 同跳 round-robin → 流量对半摊到死活两条链路**
   - `mesh/topology.go:137` `RegisterPeer` 直接 append，无 nodeID 去重
   - `mesh/mesh.go:1051` `SendToNode` 对同节点同跳会话 round-robin；`peer.Send` 仅验证入队成功（永不被卡），failover 分支（mesh.go:1059）永不触发
   - 完全解释"偶发能通一次、之后全挂"与控制面看似正常（gossip 走另一条活会话）
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
| ④ 双会话去重 | **撤销** | 同跳多会话 round-robin 是设计 §2.8 多链路负载均衡特性，非 bug；问题从来不在重复，而在死会话不被发现 |

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

## 下一步

- **治本 = 找到并修复 B**（触发源）。前置条件：阶段二取证
- **阶段二停滞 dump（优先开工）**：见 docs/tasks/mesh-data-plane-stall-tasks.md 阶段二。原理：关键循环的进展足迹（现有 atomic 计数器：TUN readPackets / WritePackets / peer lastSeenNano）由独立 monitor goroutine 旁观；卡死=部分冻结签名（有的计数器在走、有的冻结 60s），满足即全量 goroutine 栈写 logs/ 文件。断网可取证：不侵入阻塞调用、不拿业务锁，分析在事后读文件，无需在线、无需网络
- 复现验证用跨平台 Win↔Linux 全链路（含 wintun/tun，不绕开 TUN 层）
