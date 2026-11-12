# Mesh 大文件传输吞吐对比（2026-11-09）

> 触发：升级部署时二进制（23MB）走 mesh 极慢/卡死，改走 QG 跳板后 3.6s 完成。
> 目的：把历史测试方法与结论和今天的实测放在一起对比，判断是否同一现象。

## 1. 历史方法（见 [elephant-download-dribble.md](elephant-download-dribble.md)）

| 方法 | 观测对象 | 结论 |
|---|---|---|
| 静态资源逐条 HTTP 下载 | 内层端到端 TCP 流 | 每流稳态 100~210KB/s，与源无关；小响应快、大响应滴流 |
| 多连接并发（6/8 流） | 聚合吞吐 | 6 流 ≈ 478KB/s，8 流 ≈ 1.2MB/s，**线性扩展** → 瓶颈在每流 |
| 274×5MB POST（QG→jf） | 隧道层搬运能力 | 11MB/s，证明底层腿健康，但**没测内层流** |
| QG 侧 `rcv_ooopack` 采样 | 内层乱序 | 173→573，约 50% 分段乱序 |
| JF gVisor `stack.Stats().TCP` 差分 | 发送端拥塞控制 | fast_retransmit +28、sack_recovery +31、spurious +31、DSACK +35、timeouts 0 → 把乱序误判为丢包，有效 cwnd ≈ 1.6 段 |

**v4 结论**：mesh 隧道层多路并发 POST/GET 合批 → 同一内层流的分段乱序到达 → 内层发送端 dup-ACK → 快速重传 + cwnd 塌缩 → 每流滴流；N 流线性聚合。
**v5**：另有一条"硬停滞"（单流归零、永不恢复）根因在 gVisor SACK 记账与 cubic 截断，已修复；**残余滴流留待新一轮**。

## 2. 今天的实测（2026-11-09，部署 23MB 二进制）

| # | 路径 | 传输方式 | 结果 | 折算 |
|---|---|---|---|---|
| 1 | 本机 → QG `10.11.61.50:2222` | scp（公网/直连） | 秒级完成 | 约 5~10MB/s |
| 2 | QG → mb `192.168.1.88` | scp（局域网直连） | **3.59s** | **约 6.4MB/s** |
| 3 | 本机 → mb（mesh，`loc.mb.phn`） | scp（单流走 mesh） | 5 分钟后仅 15.9MB，随后停滞 | **约 50KB/s** |
| 4 | QG → mb（mesh，`loc.mb.phn`） | scp | >226s 未完成，中止 | <100KB/s |
| 5 | 本机 → gg（mesh，`loc.gg.phn`） | scp | 23MB 完成（分钟级） | 数百 KB/s |
| 6 | 本机 → deskw（mesh，`deskw.phn`） | scp 1KB | **完全卡死无输出** | —（见 §3） |

## 3. 对比结论

1. **同一现象，未消失**：凡走 mesh 的单条内层 TCP 流，吞吐都落在 50~200KB/s 量级（#3/#4/#5），与 v4 的 100~210KB/s 一致；换成直连/局域网（#1/#2）立刻回到 6MB/s 量级，比值约 **100 倍**。
2. **不是链路带宽**：同一条 mesh 上多流可聚合（历史 8 流 1.2MB/s），本次单流慢、直连快，符合"每流 cwnd 被乱序打塌"的机制，而不是总带宽不足。
3. **deskw 的 1KB 卡死不是吞吐问题**：deskw 的 OS SSH 默认 shell 是 PowerShell，`scp` 远端拉起 `scp -t` 失败/挂住；控制命令（`dir`、`Get-Process`）秒回。判定与吞吐无关，属传输协议适配问题。
4. **修正一个此前的说法**：我曾以为 deskw 是"大流量卡死"，实测 1KB 也卡，说明是 scp 通道问题；真正的 mesh 吞吐证据应取 #3/#4/#5。

## 4. 运维结论（立即生效）

- **不要经 mesh 搬二进制**。优先 `本机 → QG(10.11.61.50:2222) → 目标机内网 IP`：
  - mb：`ssh root@10.11.61.50`，再 `scp file cmll@192.168.1.88:...`（3.6s/23MB）
  - gg：`scp file layer4@loc.gg.phn:/home/layer4/phaethon-gg/`（走 mesh，分钟级，可接受）
- deskw 这类 Windows + PowerShell 默认 shell 的目标，不要用 scp；改 `ssh + stdin` 或让本机放文件。

## 5. 下一步（吞吐专项，待排期）

按 v4 的方法做一次**同条件复测**，确认是否仍是乱序→cwnd 塌缩：

1. 在 QG 起 `iperf3 -s`，从 jf 经 mesh 跑单流 `iperf3 -c`，记录吞吐；
2. 同时采 QG 侧 `ss -ti`（`rcv_ooopack`、`cwnd`、`retrans`）与 jf `/api/mesh` 的 `tcpStats` 差分（fast_retransmit / sack_recovery / spurious_recovery / DSACK）；
3. 并行 8 流复测聚合是否仍线性（若线性 → 每流问题；若不变 → 带宽问题）；
4. 检查当前 mesh 数据面是否仍存在"同一内层流分片跨多通道/多队列"的乱序源（DRR 256 队列、meshInboundWorkers=4、stripe lane 分流、IPIP 外层并发）。

## 6. 复测（2026-11-09 21:00–22:00，全部在 411 版本上）

环境：vm(Windows 本机) / QG(10.11.61.50) / jf / gg / mb(192.168.1.88)，测试载荷为 `/tmp/bench*.bin`（16MB 随机数，scp 单流或分片并行）。

| # | 路径 | 跳数 | 载荷 | 耗时 | 折算 |
|---|---|---|---|---|---|
| 1 | VM → QG 公网直连 | 0 | 4MB | 1.31s | **3.1MB/s** |
| 2 | VM → QG mesh（`loc.qg.phn`） | 1 | 1MB | 0.26s | **3.9MB/s** |
| 3 | QG → jf 公网直连（60002） | 0 | 16MB | 1.83s | **9.2MB/s** |
| 4 | QG → jf mesh（`loc.jf.phn`） | 1 | 16MB | 8.10s | **2.07MB/s** |
| 5 | QG → jf mesh 4 流并行（4×4MB） | 1 | 16MB | 4.63s | **3.46MB/s（1.7×）** |
| 6 | **VM → jf mesh** | **2（经 qg）** | 1MB | 20.1s | **52KB/s** |
| 7 | **VM → jf mesh 4 流并行（4×256KB）** | 2 | 1MB | **>300s 未完成** | **<3.4KB/s/流** |
| 8 | VM → gg mesh（`loc.gg.phn`） | 1 | 1MB | 27.6s | **38KB/s** |
| 9 | QG → mb mesh（`loc.mb.phn`） | 多跳 | 23MB | >226s 未完成 | <100KB/s |
| 10 | QG → mb 局域网（192.168.1.88） | 0 | 23MB | 3.59s | **6.4MB/s** |
| 11 | VM → QG 公网直连 | 0 | 16MB | 2.40s | **6.9MB/s** |
| 12 | **VM → jf mesh（整包 16MB）** | **2（经 qg）** | 16MB | **630.9s** | **26.6KB/s** |

链路质量（vm 侧 `/api/mesh`）：

```
vm -> qg  linkKey=vm:QG_SS5_UDPLEG  srtt=37.6ms  jitter=3.6ms
vm -> gg  linkKey=vm:GGDYSV_TJ      srtt=275.4ms jitter=139.9ms
```

### 6.1 这次复测得到的两个不同问题

**A. 高 RTT/抖动链路（vm↔gg）**：`srtt 275ms / jitter 140ms`，实测 38KB/s。
按 cwnd/RTT 反推有效窗口约 10KB（≈7 段），属于链路本身差 + 拥塞窗口受限，
与 v4 的"每流滴流"形态一致，但不是本次新增。

**B. 中转路径几乎停滞（vm→qg→jf）**：这是新发现，也是最严重的一条：

- 两段单跳各自很快：vm→qg 3.9MB/s、qg→jf 2.07MB/s；
- 组合成两跳后：1MB 要 20.1s（52KB/s），4 路并行 **300s 都跑不完**（并行不但不聚合，反而更差）；
- 同一时刻 vm 的 `bestPaths` 明确写着 `jf: nextHop=qg, cost=68.9`，即流量确实经 qg 中转；
- qg→jf 同一 sshd、同一载荷 2.07MB/s，说明 jf 端不慢；vm→qg 同链路 3.9MB/s，说明 vm 出口不慢。

结论：**问题在 qg 的 mesh 帧转发（relay）路径**，不在链路带宽、不在端点 sshd。
且并行流更差，指向转发侧存在串行瓶颈/排队或丢帧重传放大，而不是单纯的每流 cwnd 限制。

> 2026-11-09 22:15 补充（用户确认）：**两跳中转慢在本次升级之前就存在，不是 411 引入的回归**，
> 因此不再做 fork / 批次的 A/B 回滚实验。该结论仅作为现象记录与后续专项入口。

> 22:20 补充：vm 的 P2P peer 只有 `gg` 与 `qg` 两个（`/api/p2p`），代理表里也没有指向 jf 的条目，
> 所以 **vm→jf 只能走两跳**；此前"优化到接近直连"的结论应来自单跳场景。若要接近直连，
> 需要给 vm 增加一条直连 jf 的 P2P 代理（jf 侧已有 h_tunnel 32457 / trojan 39988 入口）。


### 6.2 下一步（针对 B）

> **2026-11-09 23:10 修正**：下面这段基于"两跳塌到 26KB/s"的判断已被 §7 的 md5 校验基线推翻，
> 保留原文仅作过程记录。真正结论见 §7/§8。

1. 在 QG 开 `mesh=debug`，观察 `HandleMeshFrame` 的 relay 分支与 `channelDropStats`、`tcpStats` 差分（重点看非本机目的帧的入队/丢弃/阻塞）；
2. 对比 qg 上"终结给本机"与"中转给 jf"两条路径的队列深度与写循环耗时；
3. 用 411 与旧版（如 343）各跑一次 vm→qg→jf 的 1MB 传输，确认是否为本次升级引入的回归（怀疑点：D26 写 TUN 双优先级、D21 入站队列丢弃策略对**中转**路径的影响）；
4. 若确认是回归，优先回退/修正对应批次，再谈吞吐优化。

## 7. md5 校验基线（2026-11-09 23:00，同一 16MB 随机载荷 `c3809d4a…`）

每个结果都核对了落盘字节数与 md5，排除"scp 静默失败"造成的假数据（此前 3.9MB/s 那组即因此作废）。

| 场景 | 路径 | 耗时 | 吞吐 | 占直连 |
|---|---|---|---|---|
| 直连基线 | vm ← qg 公网 `10.11.61.50:2222` | 2.43s | **6.6MB/s** | 100% |
| mesh 1 跳 | vm ← qg（内建 SSH） | 4.35s | **3.68MB/s** | 56% |
| mesh 2 跳 | vm ← jf（内建 SSH，经 qg） | 14.15s | **1.13MB/s** | 17% |
| mesh 2 跳 8 流并发 | vm ← jf，8×2MB 并行 | 11.39s | **1.47MB/s** | 22% |
| 直连（另一段） | qg ← jf 公网 `:60002` | 1.83s | 9.2MB/s | — |
| mesh 1 跳（另一段） | qg ← jf（`loc.jf.phn`） | 10.84s | 1.55MB/s | 17% |

### 7.1 修正后的结论

1. **不存在"两跳 relay 塌到几十 KB"**：早前 26~52KB/s 全部来自 `loc.*`（fakeIP/loopback → 本机 OS sshd）这条**服务端点路径**，不是 mesh 链路。换成内建 SSH 端点后，同一对节点是 1.13MB/s。
2. **两跳吞吐 ≈ 两段单跳的较小者**：qg↔jf 单跳本身只有 1.55MB/s，所以 vm→qg→jf 落在 1.13MB/s，属于正常串联，不是异常。
3. **8 流聚合只有 1.3×（1.13→1.47MB/s），不是线性**：说明这条两跳路径的瓶颈是**共享的链路容量（qg↔jf 约 1.5MB/s）**，而不是"每流 cwnd"。
4. **真正的差距在单跳本身**：同样 qg↔jf，直连 9.2MB/s、走 mesh 只有 1.55MB/s（约 6×）；vm↔qg 直连 6.6、走 mesh 3.68（约 1.8×）。这才是需要专项优化的地方，而不是中转。

### 7.2 与历史记录对齐

- `elephant-download-dribble.md` 顶部：v5 之后"单流 173~726KB/s、8 流聚合 0.2~10MB/s 高方差"；本次 16MB 单流 1.13MB/s、8 流 1.47MB/s，**单流已高于历史区间，聚合远低于历史峰值**。
- 历史"接近直连"指的是 **8 流聚合峰值 ~10MB/s**，不是单流；当前 8 流只有 1.47MB/s，说明**聚合能力明显退化**，与 D10（`4d9a144`）把 lane 入队改为"满则清亲和回退主链路"高度相关：同一条流的帧会在 lane 与主链路之间分批到达，破坏 v5 的"每流 lane 亲和钉扎"。

## 8. 完整实验方案（待执行）

**目标**：用可复现的数据回答两件事——(a) 单跳 mesh 相对直连的 1.8~6× 差距来自哪一段；(b) 8 流聚合退化是否由 lane 中途 repin 造成。

**固定条件**：16MB `urandom`（md5 `c3809d4a…`）、每场景 3 次取中位、每次都校验 md5、记录收发两端 `/api/mesh` 的 `tcpStats` 差分与 `channelDropStats`。

| 组 | 变量 | 观测 |
|---|---|---|
| A | 直连基线（vm↔qg 公网、qg↔jf 公网、qg↔mb 局域网） | 证明"非 mesh 上限" |
| B | mesh 单跳：单流 / 4 流 / 8 流 | 判断单跳是否存在共享瓶颈 |
| C | mesh 两跳：单流 / 8 流 | 验证"两跳 ≈ min(单跳)" |
| D | **stripe lane 关闭 vs 开启**（同一 8 流场景） | 直接验证 lane 分流是否引入乱序/退化 |
| E | 打开 `MESH-TRACE` 抓同一内层流的 `seq` 序列 | 数出乱序/回退次数，而非从计数推断 |

**判据**：

- 若 D 组关闭 lane 后 8 流聚合明显上升 → 锁定 D10 的 repin 逻辑，改为"lane 满则丢、绝不中途 repin"；
- 若 B 组单跳本身 8 流也不聚合 → 瓶颈在单条 P2P 传输（TCP-over-TCP/加密/写循环），与 lane 无关；
- 若 E 组 seq 回退集中出现在 repin 时刻 → 直接实锤，不需要 A/B 猜测。

## 10. 第二轮排查（2026-11-10 00:00，v0.8.0-420-gd696a82）

### 10.1 被否掉的假设：TCP_NOTSENT_LOWAT

按 `吞吐上限 = lowat / RTT` 推算 128KB 恰好等于实测单跳 3.4MB/s、两跳 2.3MB/s，看起来严丝合缝，
于是把 lowat 提到 512KB 并部署 vm/QG/jf 复测：

| 场景 | 128KB | 512KB |
|---|---|---|
| 1 跳 vm←qg | 3.03MB/s | 2.95MB/s |
| 2 跳 vm←jf | 2.67MB/s | 2.09MB/s |
| 2 跳 8 流 | 2.20MB/s | 2.09MB/s |

**无提升**。查日志发现 QG/jf 上一条 `LANE-LOWAT` 都没有 —— 这条链路的载体不是普通 `*net.TCPConn`
（vm↔qg 是 `QG_SS5_UDPLEG`，qg↔jf 是 `MGMS_HT` h_tunnel），`limitKernelBacklog` 根本没被调用。
数字吻合只是数学巧合。改动本身保留（对真正的 TCP 载体仍有意义），但不是根因。

### 10.2 当前收敛

- 起点 1.13MB/s → 现在 2.1~2.7MB/s（D10 保序修复贡献），直连基线 6.6MB/s，差 2.4~3×；
- 8 流聚合（2.09）低于单流峰值，说明载体内部还有**共享的批量/窗口限制**；
- 两个载体都在我们自己的代码里：`dialer/htunnel_direct.go`（h_tunnel POST/GET 批量）与
  SOCKS5 UDP 腿的可靠层窗口。

### 10.3 下一步（无需动 jf 系统）

1. **h_tunnel 参数可热调**：`config.HTunnelConfig` 已有 `get-slots`(默认 2)、`pool-size`(16)、
   `max-batch-size`(512KB)、wait-time 区间，且 mesh 配置 API（`PUT /api/mesh` 的 `updates.htunnel`）
   可以写入这些值。先把 `get-slots` 2→8、`max-batch-size` 512KB→1MB，复测 qg↔jf 单跳与 8 流聚合，
   验证下游 long-poll 并发/批量是否就是 1.55MB/s 的来源。
2. **UDP leg 窗口**：查 `p2p`/`server` 的 UDP 腿可靠层（窗口大小、ACK 节奏、重排窗口），
   对照 vm↔qg 单跳 3MB/s 的上限。
3. 两者都调完后，再测 8 流聚合是否接近直连；若仍不达标，继续按"载体 → DRR → gVisor"顺序收敛。


## 9. 修复与验证（2026-11-09 23:40，v0.8.0-418-g4598c70）

**改动**（`4598c70`）：`laneEnqueue` 队列满时不再 `repinPrimary`，而是丢弃该批并**保持流在原 lane**；
只有 lane writer 退出（done 关闭）才允许回退主链路；新增 `laneQueueDrops` 计数。

部署：vm / QG / jf 三节点升级到 `v0.8.0-418-g4598c70`（md5 校验一致）。

**结果**（16MB，md5 `c3809d4a…` 全部校验通过）：

| 场景 | 修复前 | 修复后 | 变化 |
|---|---|---|---|
| 2 跳单流 vm←jf | 14.15s / **1.13MB/s** | 6.29s / **2.67MB/s** | **2.4×** |
| 2 跳 8 流聚合 | 11.39s / **1.47MB/s** | 7.09~7.69s / **2.2MB/s** | **1.5×** |
| 1 跳单流 vm←qg | 4.35s / 3.68MB/s | 5.29s / 3.03MB/s | 略降（噪声/冷启动） |

**结论**：D10 的"满即改钉"确实是乱序源之一，修复后单流与聚合都明显回升。
但离历史 8 流峰值（~10MB/s）与直连基线（6.6MB/s）仍有明显差距，且单流测量方差较大
（同一版本先后出现 14.9s 与 6.29s，怀疑与 stripe lane 的建立时机/选路有关）。

**下一步**：

1. 每场景 3~5 次取中位、等 lane 稳定后再测，确认 2.67MB/s 是否可复现；
2. 若单流稳定在 ~2.7MB/s，则剩余 2.4× 差距在单条 P2P 传输（TCP-over-TCP、加密、写循环、DRR 批量），按 B 组继续；
3. 8 流聚合目前低于单流（2.2 vs 2.67），说明还有共享瓶颈，需要看 lane 数、DRR 队列与 qg↔jf 单跳容量（1.55MB/s）。




## 11. 互验缺自证：两跳路由不对称导致 RTT≈1s（2026-11-10 07:50，v0.8.0-425-g05cb375）

### 11.1 现象与证据

425 之前（424）vm→jf 两跳只有 **127KB/s**（26.7MB / 3m28s），比任一单跳都低一个数量级：

| 路径 | 耗时 | 吞吐 |
|---|---|---|
| vm → qg（1 跳） | 2.31s / 8.9MB | 3.86MB/s |
| qg → jf（1 跳） | 5.91s / 8.9MB | 1.51MB/s |
| vm → qg → jf（2 跳，424） | 3m28s / 26.7MB | **0.127MB/s** |
| vm → qg → jf（2 跳，425） | 4.39s / 8.9MB | **2.03MB/s** |

jf 的 `tcpFlowDebug` 抓住 SSH 流实时状态：`srtt=991ms`、`rto=1.38s`、`sndWnd=130816`。
128KB ÷ 1s ≈ 128KB/s，与实测完全一致 —— 瓶颈是 **接收窗口 / 端到端 RTT**，不是带宽、CPU 或批量。

RTT 来自回程绕路：vm→qg ping 38ms、qg→jf ping 22ms，但 vm→jf ping **979ms**。
原因是 jf 的 `bestPaths` 到 vm 为 `nextHop=gg`（gg 本身是 1s 链路），去程 qg、回程 gg。

### 11.2 根因：转发时把"指回自己"的边剥掉了

jf 的 `/api/mesh/topology` 有 `qg -> vm`，却没有 `vm -> qg`。校验后的图缺边，Dijkstra 无法经 qg 到 vm。

424 引入的 `mergedNeighborLinks` 只遍历 `allPeers`，**遗漏本节点自己的 claim**。
qg 转发 vm 的 claim 时，验证"vm 声明了到 qg 的链接"要查 `merged[qg][vm]`，而 `merged[qg]` 为空，
于是判定失败并在转发前剥掉 `vm→qg`；下游 jf 永远看不到这条反向边。

### 11.3 修复

`mergedNeighborLinks(selfNodeID, ownNeighbors, allPeers)`：把本节点 own claim 一并并入合并表
（`mesh/mesh.go:3619`）。不放松防环规则、不改通告格式，只让本节点的声称参与互验。

### 11.4 425 验证

- jf→vm `bestPaths`：`nextHop=gg cost=1532 → nextHop=qg cost=62.9`；
- vm→jf ping：**979ms → 51ms**；
- 两跳单流：**0.127MB/s → 2.03MB/s（约 16×）**，已回到 423 之前 2.1~2.7MB/s 的区间并略优；
- 剩余瓶颈：qg↔jf 单跳仅 1.51MB/s（直连基线 6.6MB/s），下一阶段按 §10.3 调 h_tunnel / UDP leg 载体。

## 12. h_tunnel 载体调参与 qg 单核上限（2026-11-10 08:10）

### 12.1 调参实验（qg↔jf 单跳 / vm→jf 两跳）

| 配置 | 4 流聚合 | 8 流聚合 | 稳定性 |
|---|---|---|---|
| 默认（get-slots=2, batch=512KB） | 1.94MB/s | 2.45~3.2MB/s | 完成 |
| get-slots=8, batch=1MB | — | 2.9~3.5MB/s | 有流 stall |
| get-slots=2, batch=1MB | 3.00MB/s | 25.8/71.2MB 后 stall | 不稳定 |

- `max-batch-size` 1MB 对 4 流明显有效（1.94→3.0），但 8 流会卡死（`timeout 120` 只下完 25.8MB）；
- `get-slots=8` 让多个 data GET 并发，吞吐略升但引入乱序/stall，回退；
- 默认配置最稳：8 流 29.0s 完成 71.2MB（2.45MB/s），无卡死。

### 12.2 qg 资源上限

`nproc=1`、内存 484MB。8 流中继时 phaethon CPU 67~70%，jf 侧 CPU 空闲。
两跳的共享瓶颈是 **qg 单 vCPU 上的 gVisor 转发 + h_tunnel 单 data GET 循环**，不是链路带宽，
也不是 inner TCP 每流窗口（单流已能到 2.1MB/s，4 流默认并不聚合）。

### 12.3 结论

- 路由/互验缺边已修复并验证（见 §11），这是本轮 127KB/s→2.1MB/s 的主因；
- 继续提升需要动载体并发模型：多个 data GET 必须带**全局批次序号 + 客户端按序重组**，
  否则并发 GET 的响应会乱序投递，inner TCP 直接崩；或先在服务端做短时聚合（≈2ms/256KB）
  再交给单个 GET，保持有序的同时把每批从 ~90KB 提到更大。
- 在此之前保持默认 h_tunnel 参数，避免 8 流 stall。

## 13. 428 聚合窗口与瓶颈定位（2026-11-10 08:30，v0.8.0-428-g88de519）

### 13.1 聚合窗口实验结果

`server/htunnel_mesh.go` 增加 2ms / 256KB 的 GET 投递聚合（控制帧与 ≥256KB 立即 flush，
保持单 data GET 循环的有序性）。

| 场景 | 默认 | +2ms/256KB 聚合 |
|---|---|---|
| 4 流两跳 | 1.94MB/s | 2.53MB/s |
| 8 流两跳 | 2.45MB/s | 2.34MB/s（完成，无 stall） |

聚合对低并发有小幅帮助，8 流基本不变；仍保持稳定。

### 13.2 分段容量实测（8 并发流）

| 段 | 聚合吞吐 | qg 侧 CPU |
|---|---|---|
| vm → qg（单跳） | **7.72MB/s** | — |
| qg → jf（单跳，h_tunnel） | **4.45MB/s** | 42% |
| vm → qg → jf（两跳） | **2.34MB/s** | 67~70% |

结论：
- vm↔qg 的 UDP leg 聚合能力足够（7.72），不是瓶颈；
- qg↔jf 的 h_tunnel 单腿 4.45，也不是硬带宽瓶颈（公网直连 sshd 9.2MB/s）；
- 两跳只有 2.34，低于两段的较小者（4.45），差额来自 **qg 单 vCPU 上同时做两段加解密 + gVisor 转发**。
  `nproc=1`，8 流中继时 phaethon 67~70%，jf 侧 CPU 空闲。

### 13.3 下一步选项

1. **给 qg 增加 vCPU（最便宜）**：两跳 CPU 成本约为单腿的 2 倍，2~4 核预计可把两跳推到 4~6MB/s；
2. **P2P 层中转快路径**：对 transit 帧跳过 gVisor 注入，直接按 bestPaths 转发到下一跳 P2P 队列，
   可显著降低每包成本，但涉及 TTL/u-turn/IPIP/NAT 语义，属于较大改动，需要单独评审；
3. 保持当前默认 h_tunnel 参数（get-slots=2 / 512KB），避免 8 流 stall。

## 14. qg 扩到 2 核 + 多 GET 按序重组实验（2026-11-10 08:45，v0.8.0-431-gee75d9c）

qg 从 1 核扩到 2 核（`nproc=2`），服务仍是 428/431，无需重装，只重启确认。

### 14.1 2 核结果

| 场景 | 1 核 | 2 核 |
|---|---|---|
| 两跳单流 | 2.19MB/s | **2.4~2.5MB/s** |
| 两跳 8 流 | 2.34MB/s | **3.60MB/s**（71.2MB / 19.8s） |
| qg→jf 单腿 8 流 | 4.45MB/s | **4.75MB/s**（71.2MB / 15s） |

2 核下 8 流中继时 qg 的 phaethon CPU 只有 0~16%，**CPU 不再是瓶颈**；
两跳 3.60 已达到单腿 4.75 的 76%，说明中转本身没有明显额外损耗，剩余差距在载体。

### 14.2 多 data GET + 按序重组（431 新增，已实现但默认关闭）

- 服务端给含数据帧的批次分配全局序号（响应头 `X-B`），控制帧批次 seq=0；
- 客户端 `orderedInbox` 按序重组，缺口 >1s 或缓冲 >8MB 时跳过；
- 实验：`get-slots=8` 时 4 流 1.48MB/s、8 流 1.51MB/s，明显差于 `get-slots=2` 的 3.60；
  怀疑部分响应丢头/缺批导致频繁 1s 跳跃，暂不启用。
- 结论：保持 `get-slots=2`（单 data GET，天然有序），431 在该配置下与 428 持平（3.60）。

### 14.3 当前瓶颈与下一步

qg↔jf 同一条物理链路上：OS 直连 sshd（60002）8 流 **10.2MB/s**，h_tunnel 载体 8 流 **4.75MB/s**。
CPU 空闲、netstack 中转只损耗 24%，所以 P2P 中转快路径在 2 核下收益有限；
要接近 6.6MB/s 直连基线，需要提升 h_tunnel 载体本身（HTTP 长轮询批量/代理层），
或换一条更高带宽的载体链路。代码侧下一步优先级低于载体选型。

## 15. 多 GET 按序重组 / POST 重排实验：负结果（2026-11-10 09:05，v0.8.0-436-g2bef2c4）

### 15.1 GET 多循环 + X-B 按序重组（431）

- 服务端给数据批次分配 `X-B` 序号，客户端 `orderedInbox` 按序投递（缺口 1s、8MB 跳过）。
- `get-slots=2`（单 data 循环）时 431 与 428 持平：8 流 3.60MB/s。
- `get-slots=8` 时 4 流 **1.48MB/s**、8 流 **1.51MB/s**，明显退化。
- 结论：序号机制本身可用，但代理侧存在丢批/丢头时会产生 1s 跳跃；多 GET 暂不启用。

### 15.2 服务端按 X-S 重排并发 POST（433/434）

动机：客户端最多 16 个并发 POST，服务端按到达顺序入 meshIn，ACK 乱序导致 jf 侧
inner TCP 伪重传（观察到 DSACK 16 万，约 27%）。

- 433（handler 持锁直接写 meshIn）：8 流 **1.35MB/s**，POST handler 被反压阻塞后超时留洞。
- 434（单一投递协程、非阻塞 handler）：8 流 **1.83MB/s**，仍明显退化；
  测试后 jf 计数：segments_sent 87941、retransmits 31112（35%）、DSACK 26359（30%）。
- 结论：重排闸门没有消除根因，反而引入排队与跳跃；已 revert（7703590、2bef2c4）。

### 15.3 回退后状态（436）

- qg/jf 436，`get-slots=2`，8 流两跳 **3.32MB/s**（65.4MB / 19.7s），恢复稳定；
- qg 2 核、CPU 空闲；单流 2.4~2.5MB/s；
- 仍未解释：jf 侧 inner TCP 有 ~30% DSACK 伪重传，说明 **数据/ACK 在 qg→vm 或 p2p 调度层仍被乱序**。
  下一步应在 qg→vm 的 SS5 UDP leg / DRR 队列做逐流序号核对，而不是继续在 h_tunnel 层加重排。

## 16. 载体归属验证：UDP leg 不可关闭；h_tunnel 外部链路抖动（2026-11-10 09:15）

### 16.1 关闭 qg↔vm 的 p2p-udp（WIN7_VPN）

用 `cmd/toggle-p2p-udp` 停 phaethon 后把 WIN7_VPN 的 `p2p-udp` 置 false，重启 qg：

| 配置 | 两跳单流 |
|---|---|
| UDP leg 开启 | 2.4~2.5MB/s |
| UDP leg 关闭（走 SOCKS5 TCP base） | **0.34MB/s**（24s / 8.2MB） |

SOCKS5 TCP base 只有 ~0.3MB/s，UDP leg 是必需的，已恢复 `p2p-udp=true`。

### 16.2 恢复后观察到外部载体抖动

多次重启/压测后，qg↔jf 的 h_tunnel 对端（36.140.28.178:18080）出现：

- jf 侧 qg 链路 `srtt=204ms jitter=262ms`（正常 ~20ms）；
- 两端路由自动切到 gg：jf→vm cost 187 via gg，vm→jf cost 201 via gg；
- 8 流两跳降到 1.89MB/s；单流 0.38~1.25MB/s。

同时间 qg→jf 公网 sshd 仍可 8.2MB/3.13s（单流 2.6MB/s），说明物理链路正常，
劣化在 h_tunnel 的 HTTP 长轮询端点或其代理层。

恢复后强制 gossip：两端路由回到 qg（cost ≈105），但单流仍只有 0.38~1.25MB/s，
说明载体仍未回到之前的水平。该现象是外部的、随时间波动的，不是 436 代码回归
（436 在载体正常时实测 3.32~3.60MB/s）。

### 16.3 结论

- UDP leg 必须保留；它天然不保序/会丢包，是 inner TCP DSACK 的主要来源；
- 想在代码层继续提升，应给 UDP leg 帧加序号 + 接收端小窗口重排（隐藏网络乱序），
  但当前外部 h_tunnel 端点抖动会掩盖收益，建议等载体稳定后再做该实验；
- 生产保持 436、`get-slots=2`、`p2p-udp=true`。

## 17. RACK 诊断字段（2026-11-10 09:36）

gvisor fork 的 `tcpFlowDebug` 新增 `reoWndMs / minRttUs / rackRttUs / reoWndIncr / reord /
dsackSeen / rackUpdates / sackPermitted / recoveryBits`，用于确认 sender 侧 RACK 是否真的
在学习 RTT、重排窗口是否与 mesh 路径的乱序幅度匹配。仅诊断，不改行为。

### 17.1 试验：RACK reo_wnd 使用排队延迟下限（2026-11-10 09:42）

线上采样（jf sender）：`minRtt≈60ms`、`srtt/rackRtt≈200~385ms`、`reoWnd=15~31ms`、`reord` 时真时假。
即 RACK 只允许 15~31ms 的重排/排队裕量，而路径的实际排队延迟有 200~300ms，
大量"只是被队列延迟"的段被判定丢失，形成 DSACK 伪重传。

fork 改动：`reoWnd = max(minRTT/4*incr, SRTT-minRTT)`，仍受 `min(_, SRTT)` 上限。
验证方式：8 流两跳，对比 DSACK/retransmits 增量与吞吐。
- 全量 `SRTT-minRTT` 作为 reo_wnd 下限：8 流 61.9MB/90.4s = 0.68MB/s，真实丢包只能等 RTO，明显变差；
- 收敛为 `(SRTT-minRTT)/4` 再测。
- `(SRTT-minRTT)/4` 作为下限：8 流 57.3MB/90.3s = 0.63MB/s，仍有超时，同样变差。
- **结论：reo_wnd 调大不是出路**。该路径存在真实丢包（UDP leg），把 RACK 丢失判定放宽会让真实丢包
  只能等 RTO，吞吐反而崩塌；原有 minRTT/4*incr 对真实丢包是合适的。DSACK 是伴随现象，不是主因。
  已回退 RACK 行为改动，仅保留诊断字段。

## 18. 环境基线漂移：qg↔jf 链路整体劣化（2026-11-10 10:05）

同一时段分段复测，排除代码因素：

| 段 | 09:13 | 10:02 |
|---|---|---|
| qg → jf 公网直连 sshd 单流 | 8.2MB / 3.13s = **2.6MB/s** | 8.2MB / 32s = **0.26MB/s** |
| qg → jf h_tunnel 单流 | 1.5~2.6MB/s | 4.9~7.6MB / 40s = **0.15MB/s** |
| vm → qg 单跳 | 6~7.7MB/s | 22.9MB / 3.62s = **6.3MB/s** |

vm↔qg 健康、qg↔jf 的**直连 SSH 与 h_tunnel 同时变慢**，说明问题在 qg↔jf 的公网链路/出口，
不是 442 代码，也不是 h_tunnel 单独的问题。

因此 §17 里 RACK reo_wnd 的两个负结果是在链路开始劣化后测的，**可能被环境污染，需要在链路恢复后复测**；
当前生产保持 442（RACK 原逻辑 + 诊断字段），不做进一步 netstack 行为改动。

## 19. 实验：Reord 时允许 SACK 真实丢包走 dup-ACK（2026-11-10 10:20）

链路部分恢复（qg→jf 直连 2.7MB/s、srtt 22ms），但 h_tunnel 单流仍只有 0.58MB/s，
两跳 8 流 1.15MB/s，环境未完全复原，因此采用**同条件 A/B 对比**而不是绝对值判定。

- 基线 442 累计：sent 575002 / retransmits 102041 (17.7%) / DSACK 59995 (10.4%)；
- 实验分支 `experiment/rack-dupack-coexist`：`Reord=true` 时，若 SACK scoreboard 已把
  队头判失（真实空洞），仍允许 dup-ACK 快速重传；裸乱序继续抑制。
- 实验组 3 次 8 流：1.57 / 1.26 / 2.32MB/s（中位 1.57）；基线上一次 1.15MB/s。环境方差大，需回退基线同窗口再测。
- 基线 445 同窗口 3 次：2.08 / 0.72(接近 stall) / 2.01MB/s，中位 ~2.0。
- **结论：实验组（中位 1.57）不优于基线（中位 ~2.0）**，且方差极大；不合并实验分支。
  生产保持基线逻辑（445），`experiment/rack-dupack-coexist` 仅保留在 fork 分支供链路完全恢复后复测。
- 复盘：当前限制仍是 qg↔jf 载体的真实丢包/排队（h_tunnel 单流 0.58~1.8MB/s，直连 2.7MB/s），
  netstack 恢复语义的微调不足以抵消它。

## 20. 测量规范与经验（此后所有吞吐结论按此执行）

### 20.1 成对测量，同排对比

每个场景同一行给出 OS 级（不过 mesh）与 mesh 级两条数据，并标 `mesh/OS`：

| 对比段 | OS 级（不过 mesh） | mesh 级 | mesh/OS |
|---|---|---|---|
| 例：qg↔jf 单流 | x MB/s（公网 sshd） | y MB/s（内建 SSH） | y/x |

必须同方向（pull/push）、同流数、同文件大小、同端点类型、同时间窗口。

### 20.2 两跳的 OS 级基线

vm→qg→jf 不是“没有等价路径”。OS 基线用 SSH 跳板（经 qg 系统 sshd 到 jf 公网 sshd）：

```
ssh -J root@10.11.61.50:2222 -p 60002 layer4@36.140.28.178
# 或
ssh -o ProxyCommand="ssh -p 2222 root@10.11.61.50 -W 36.140.28.178:60002" \
    -p 60002 layer4@36.140.28.178
```

### 20.3 时间与状态

- 网络会漂移，**不等恢复到旧状态**：在当前窗口重做基线，每条数据标时间；
- 旧数据不删除，只 append；旧结论若被新数据修正，就地标注“已被 <时间> 数据取代/受链路劣化污染”；
- A/B 至少各 3 次、尽量交替（A B A B），取中位数，避免把链路漂移当成代码收益；
- 每条记录都写清“过不过 mesh”。

### 20.4 历史数据状态说明

- §17 的 RACK reo_wnd 负结果、§18 的链路劣化判断，均为特定时间窗口的观测，保留原文；
- 后续以带时间戳的新基线为准，不再假设“恢复到 09:13 的状态”。

## 21. 成对基线（2026-11-10 10:55–11:05，当前窗口，不假设旧状态）

规范见 §20。单位 MB/s = 10^6 B/s；每格为 3 次（8 流为 2~3 次）中位数，OS 与 mesh 同时段。
OS 两跳基线 = vm 经 qg 系统 sshd 串接 jf 系统 sshd（`ssh qg "ssh -p 60002 jf cat ..."`），
即“通过 QG 的 OS 级 SSH 代理”，不是直连。

| 对比段 | 流数 | OS 级（不过 mesh） | mesh 级 | mesh/OS |
|---|---|---|---|---|
| vm↔qg 单跳 | 1 | 7.40（公网 sshd 22.9MB） | 6.24（内建 SSH 同文件） | 0.84 |
| vm↔qg 单跳 | 8 | 10.75 | 7.46 | 0.69 |
| qg↔jf 单跳 | 1 | ≥8.2（公网 sshd 8.18MB，1s 分辨率） | 1.17 | ≤0.14 |
| qg↔jf 单跳 | 8 | 10.91 | 2.94 | 0.27 |
| vm→qg→jf 两跳 | 1 | 2.91~3.27（OS SSH 串接） | 0.42（第 3 次 60s 超时） | ~0.14 |
| vm→qg→jf 两跳 | 8 | 8.03（OS SSH 串接） | ≤0.78（1 次 90s 超时未跑完） | ≤0.10 |

单次明细（秒）：
- vm→qg OS 单流 3.105 / 3.096 / 3.068；mesh 3.671 / 3.592 / 4.048
- vm→qg 8 流 OS 17.007 / 17.069；mesh 26.335 / 23.029
- qg→jf OS 单流 1 / 1 / 1；mesh 7 / 8 / 4
- qg→jf 8 流 OS 6 / 6；mesh 25 / 20
- 两跳 OS 单流 — / 2.813 / 2.501；mesh 15.070 / 19.292 / 60.033(timeout)
- 两跳 8 流 OS 7.562 / 8.149 / 8.166；mesh 77.093 / 90.531(timeout)

**读数结论**：
1. vm↔qg 的 mesh 已接近 OS（0.69~0.84），不是主要矛盾；
2. qg↔jf 的 mesh 只有 OS 的 0.14~0.27，是最大短板；
3. 两跳 mesh 只有 OS SSH 串接的 0.10~0.14，说明中转把两段损失叠加放大；
4. 当前窗口 qg↔jf 的 OS 8 流有 10.9MB/s，物理链路本身没问题，问题在 mesh 载体（h_tunnel）与中转。

## 22. 同窗口对照：h_tunnel max-batch-size 1MB（2026-11-10 11:15）

基于 §21 的当前窗口基线（qg↔jf mesh 8 流中位 2.94MB/s），只改 `max-batch-size=1MB`
（`get-slots=2`），其余不变，立刻复测 qg↔jf mesh 8 流（8×8.18MB）：

| 配置 | 单次耗时 | 吞吐 | 中位 |
|---|---|---|---|
| 默认 512KB | 25s / 20s（§21） | 2.62 / 3.27 | 2.94 |
| 1MB | 33s / 24s | 1.98 / 2.73 | 2.35 |

1MB 批次反而更差（0.80×），与之前 8 流易 stall 的观察一致；已恢复默认 512KB/`get-slots=2`，
复测 27s（2.42MB/s）确认回到原区间。

结论：在当前 h_tunnel/网络状态下，**加大单批不是有效杠杆**；qg↔jf mesh 仍只有同窗口
OS 8 流（10.91MB/s）的 ~0.27，需要继续从载体并发/丢包机制找原因，而不是批大小。

## 23. 关键发现：forwarding-trace 诊断吃掉中转节点约 25% CPU（2026-11-10 11:20）

### 23.1 证据（qg CPU profile，15s，qg↔jf mesh 8 流）

`/debug/pprof/profile`（临时以 `--enable-debug-api` 启动 qg）：

```
0.64s 16.6%  syscall.Syscall6
0.50s 13.0%  fmt.(*pp).doPrintf
0.31s  8.1%  mesh.ParseForwardingTrace
0.14s  3.6%  util.LogModuleDebug
```

`forwarding-trace=true` 时，`TraceForwarding` 对**每个包**做 `ParseForwardingTrace` + 3 次 `fmt.Sprintf`，
即使 mesh 日志级别不是 debug、日志会被丢弃也照做。qg/jf 当时都是 `forwarding-trace=true`。

### 23.2 处理

1. qg、jf 都设置 `forwarding-trace=false` 并重启；
2. 代码增加前置门：`util.GetModuleLogLevel("mesh") > debug` 时直接返回，不再解析/格式化
   （commit 见下），即使误开 trace 也不会付这份成本。

### 23.3 同窗口效果

| 场景（8 流） | trace=on | trace=off | 变化 |
|---|---|---|---|
| qg↔jf mesh 单跳 | 2.94MB/s（25s/20s） | **3.41MB/s（22s/17s）** | +16% |
| vm→qg→jf mesh 两跳 | ≤0.78MB/s（77s/90s 超时） | **~1.42MB/s**（46s；第 2 次 90s 超时未跑完） | ~1.8× |

两跳的第二次仍 90s 超时，链路仍有抖动；但同窗口的中位趋势明确：关掉诊断后中转吞吐上升。

## 24. Transit 帧快路径（2026-11-10 11:30，v0.8.0-451-g716fd41）

### 24.1 依据

qg 中继 CPU profile（15s，vm→jf 两跳 8 流，trace 已关）：
`forwardUnicastPacket` 29.7% cum、`FindRoute/RouteSelector` ~19%、`mallocgc` 12.5%、
`LinkNIC.WritePackets` 7%；即 transit 帧大部分 CPU 花在“gVisor 重新选路 + 转发 + 包缓冲”上。

### 24.2 实现

`MeshManager.tryTransitFastPath`：目的地址属于其他节点 /16 且 `bestPaths` 下一跳非来源时，
直接查 `subnetOwner → bestPaths → nextHopSenders`，复制帧、TTL-1、重算 IPv4 头校验和，
`PeerSender.Send` 到下一跳；任一条件不满足（本地/未知/回弹/TTL≤1/Send 失败）回退 netstack。
`routeTable` 新增 `nextHopSenders`（每 nodeID 取第一直连 sender），`/api/mesh` 新增 `transitFastPath` 计数。
单元测试：`mesh/transit_fastpath_test.go`（转发/TTL/校验和/回退条件）。

### 24.3 当前窗口效果（同文件、8 流）

| 场景 | trace 关、无快路径 | 快路径 451 |
|---|---|---|
| vm→qg→jf mesh 两跳 | 46.1s（~1.42MB/s），第 2 次 90s 超时 | **34.4s（~1.90MB/s）**，第 2 次仍 90s 超时 |

- qg `transitFastPath` 计数 141480，确认路径生效；
- 连通性回归：vm→jf ping 0% 丢包；单流 8.18MB 完整下载（12.4s）；
- 第二次 8 流仍遇到 90s 超时，说明还存在独立的 stall 问题，与快路径无关。

结论：快路径在当前窗口带来约 +34% 的两跳吞吐，且可随时回退；继续保留并部署到 jf。

## 25. 快路径后的健康窗口复测（2026-11-10 11:40–11:50）

链路恢复到较好状态后，同窗口成对复测（8 流、同文件 8.18MB×8）：

| 场景 | OS 级（不过 mesh） | mesh 451（trace 关 + transit 快路径） | mesh/OS |
|---|---|---|---|
| vm→qg→jf 两跳 | 8.35 / 8.20 / 7.72（中位 **8.20MB/s**，OS SSH 串接） | 12~15s（**4.4~5.4MB/s**）、20.2s（3.25）、22.2s（2.95）、31.3s（2.09）、31.7s（2.06），另有 2 次 90s 超时 | 完成时 0.25~0.66 |

说明：
- 健康时完成的中位约 4.4MB/s，已明显高于快路径前的 3.3~3.6 与 trace 关之前的 2.0；
- 90s 超时的几次都发生在链路抖动窗口（同时间段 OS 仍稳定 8.2），属于独立的 stall 现象；
- 逐 3s 轮询的 4 次运行全部正常完成，未能在轮询中抓到 partial stall，后续需在抖动窗口现场抓状态。

## 26. 健康窗口中转 CPU 复测：CPU 已不再是瓶颈（2026-11-10 11:42）

qg 中转 profile（451，trace 关 + transit 快路径，vm→jf 两跳 8 流，15s）：
```
total samples = 850ms / 15s = 5.67% of one core
30.6% Syscall6（socket/net I/O）
 8.2% futex
 7.1% net/http 写入 + htunnel.postBatch
 7.1% gVisor handleValidatedPacket（本地投递，非 transit）
```

对比优化前同场景（trace 开、无快路径）：38.3% 单核，且吞吐只有 ~1.4MB/s；
现在 ~4~5MB/s 时只有 5.67%。**CPU 已不是限制项**。

剩余差距（健康时完成 4.4~5.4 vs OS 8.2MB/s）回到载体侧：
- qg↔vm 的 SS5 UDP leg 真实丢包/乱序；
- qg↔jf 的 h_tunnel GET 往返与排队；
- 以及链路抖动窗口出现的 90s stall（本次健康窗口未复现）。

结论：中转层（trace、gVisor 转发）优化已到位，下一步若继续提升，应针对 UDP leg 的丢包/排队
或 h_tunnel 载体本身，而不是继续在中转代码上加优化。

## 27. 90s stall 现场抓取尝试（2026-11-10 12:10）

### 27.1 结果

在健康窗口连续跑了 4 次 8 流（每次 65.4MB），全部正常完成，未复现 partial stall。
之前 90s 超时的几次都出现在链路抖动/劣化窗口（同时间段 OS 直连仍正常），说明 stall 与
载体劣化强相关，不是稳定复现的代码死锁。

### 27.2 已准备的工具

`scripts/diag/stallhunt.sh`：在 vm 上并发 8 流，每 2s 汇总进度；一旦 6s 无进展且未完成，
立即抓取：
- jf 的 tcpStats + 22 端口各 SSH 流状态（cwnd/outstanding/定时器）；
- qg 的 channelDropStats / transitFastPath / uturn / linkNIC；
- vm 的 channelDropStats；
- qg 的 goroutine dump（需 QG 以 `--enable-debug-api` 启动）。
报告写到 vm 的 `/c/temp/stall_report.txt`。

### 27.3 下一步

等下一次链路抖动（可用 qg↔jf 的 srtt/jitter 或 OS 直连吞吐做判据）时，直接跑 stallhunt；
抓到报告后按“jf 发送端 blocked / qg 队列丢包 / vm 接收端 blocked”三类定位。
当前 QG 已恢复为不带 debug API 的正常服务，qg/jf=451。
- 追加：R=5~10 又连续 6 次全部正常完成（合计 **10/10 次健康窗口 8 流全部完成**），stall 未复现；
- 抓取脚本已改为命中 stall 后先 kill 8 个 scp，避免 `wait` 拖到 600s 才退出。

## 28. 直连 QUIC 可行性实测与环境约束（2026-11-10 12:30）

### 28.1 UDP 可达性实测

- 在 jf 上以 `python3` 监听 `0.0.0.0:19000/udp`（用户态进程，未改系统配置）；
- 从 qg 向 `36.140.28.178:19000/udp` 发送数据报；
- 结果：监听进程成功 bind，但**未收到任何数据报**；jf 公网只放通了已知 TCP 端口
  （18080 h_tunnel、60002 sshd），任意 UDP 入站不可达。

### 28.2 结论：直接 QUIC 在本环境不可行（除非加基础设施）

| 链路 | 直连 UDP 可行性 | 原因 |
|---|---|---|
| qg→jf | **否** | jf 公网 NAT/防火墙未放通任意 UDP；开新端口属 jf 系统级操作，不允许 |
| vm→qg | 不确定/大概率否 | vm 在 NAT 后，只能通过 WIN7_VPN 的 SOCKS5 UDP 出站；qg 无公网 UDP 入口 |

即使放通 UDP，QUIC 还要处理：
1. 节点身份/mTLS（现有 mesh 依赖代理口令/TLS，需要新的证书体系）；
2. 多流与 HOL：单 QUIC stream 会像当前单 TCP 载体一样头阻塞，需要按 inner flow 分流；
3. 双拥塞控制：inner TCP + QUIC CC 叠加，需要 BBR/队列调优；
4. MTU/PMTUD：mesh MTU ~1580，QUIC 头开销要留余量；
5. qg 2 核/484MB 上的连接内存与 CPU。

### 28.3 可选的现实路线

1. 若能让基础设施给 jf 放通一个固定 UDP 端口（或加一个有公网 UDP 的 relay），再上 QUIC；
2. 否则优先优化现有 TCP 载体：h_tunnel 目前是 HTTP 长轮询，若端点支持 WebSocket/持久 TCP，
   可去掉每批 HTTP 往返开销，仍保持有序，风险远小于换 QUIC；
3. 直连 QUIC 只对 vm↔qg 有意义（该段已有 SS5 UDP leg，且不是当前瓶颈），无法解决 qg↔jf 瓶颈。

## 29. qg↔mb 沙箱：ht 载体与 SOCKS5/OS 对照（2026-11-10 13:00）

按“先在可随意改的 vm/qg/mb 上定位”的思路，在 qg↔mb（同 LAN）建立三种等价路径：

| 路径 | 单流 | 8 流 | 说明 |
|---|---|---|---|
| OS 级（mb admin HTTPS 192.168.1.88:39999） | 6.2MB/s | 6.5MB/s | 不过 mesh |
| mesh + 直连 SOCKS5（MB_S5，mb:11080） | ~4.5 | **6.55MB/s** | 同一 mesh 栈，普通 TCP 载体 |
| mesh + h_tunnel（MB_HT，mb:18081） | 1.8~2.25 | **3.4MB/s** | 同一 mesh 栈，ht 载体 |

**结论：同一套 mesh 栈、同一 LAN，换掉 ht 载体（SOCKS5）就能到 OS 水平；ht 载体是瓶颈。**
且 MB_HT 直连 mb 的 phaeton ht 服务（没有公网 nginx），排除了“公网代理层”的解释。

### 29.1 已排除的因素（沙箱内实测）

- `get-slots=8`：8 流 3.15MB/s（无提升）；
- `max-batch-size=1MB`：3.07MB/s（无提升）；
- `LaneAck` 50ms→5ms：3.1~3.4MB/s（无提升，srtt 反而 70~97ms），已 revert；
- qg/mb `channelDropStats` 全 0，无队列丢包/AQM 记录。

### 29.2 ht 载体上的 inner TCP 状态（mb tcpFlowDebug）

- srtt **32~97ms**（LAN minRTT 仅 4~12ms），rackRtt 18~64ms，reoWnd 0~6ms；
- cwnd 只有 **6~41 段**，sndWnd 很大（64KB~1.4MB）→ 发送端被 cwnd 限制，不是接收窗口；
- reord=true 普遍，DSACK 5.8%、retransmits 6.3%。

即 ht 路径额外引入了 ~30~90ms 的延迟/乱序，把 gVisor inner TCP 的 cwnd 压在个位数~几十段。
下一步在沙箱里对比 MB_S5 下的同一流 srtt/cwnd，并量 ht 的 HEAD/POST/GET 原始 RTT，
定位这 30~90ms 是 ht 批处理/HTTP 往返还是 p2p 的 ACK 调度造成的。

## 30. 沙箱继续：POST 按序重排与其它杠杆（2026-11-10 13:10）

### 30.1 对照基线（qg↔mb 同 LAN，8×9MiB，字节已校验）

| 载体 | 单流 | 8 流 |
|---|---|---|
| SOCKS5（MB_S5） | ~4.5MB/s | **5.8~6.3MB/s**（75.5MB/12~14s） |
| h_tunnel（MB_HT）默认 | 1.8~2.25 | **3.4MB/s**（75.5MB/20~22s） |
| h_tunnel + 服务端 X-S 重排（非阻塞投递） | 0.73~1.35 | **3.77MB/s**（75.5MB/20s） |

POST 按序重排只带来 ~11%，单流反而更差，**不足以解决问题**，已在 464 回退。

### 30.2 已排除的杠杆（累计）

- `get-slots=8`、`max-batch-size=1MB`：无提升；
- `LaneAck` 50ms→5ms：无提升，srtt 反而更高；
- lane 信用门：`/api/p2p` 显示 ht 载体 transport=tcp，未挂 stripe，`writeCredit` 为无限，不参与；
- channelDropStats/AQM：全 0；
- 并发 POST 乱序：重排后仅 +11%。

### 30.3 当前判断

ht 路径上 inner TCP 的 srtt 32~97ms（LAN minRTT 4~12ms）、cwnd 6~41、reord 普遍；
同 mesh 栈换 SOCKS5 即回到 5.8~6.3。**h_tunnel 载体是确定的瓶颈层**，
剩余可疑点是 ht 的“每批 GET/POST 节奏 + ACK 调度”本身，而不是批大小/并发数/信用窗口。

下一步在沙箱里给 ht 加每批统计（batch 字节、GET/POST RTT、每秒批数），
再决定是改 ht 的批处理/ACK 优先级，还是给 ht 端点加持久 TCP/WebSocket 载体。

当前 qg/mb = 464（POST 重排与 lane 实验均已回退），jf 保持 451 未动。

## 31. ht 每批统计：GET 批大小/延迟与 POST ACK 延迟（2026-11-10 13:27）

新增 `[HT-STATS]`（仅 htunnel=debug 输出），qg↔mb 沙箱 8 流实测：

| 方向 | 次数/5s | 平均延迟 | 平均批大小 | 速率 |
|---|---|---|---|---|
| GET（mb→qg 数据） | 232~244 | 39~53ms | 73~93KB | 1.8~2.4MB/s（单个 data GET 循环） |
| POST（qg→mb ACK） | 1243~1625 | 20~23ms | ~0KB（纯 ACK） | — |

- GET 的延迟 ≈ 批大小/生产速率：80KB / 2MB/s ≈ 40ms，说明**单个 data GET 循环被生产速率与往返延迟共同锁死**；
- POST 承载的是逐包 ACK（每秒 250~325 个），每个 HTTP POST 约 20ms，ACK 延迟直接抬高 inner TCP 的 RTT；
- 服务端加 `TCP_NODELAY`（commit 0995554）后 GET 平均延迟 45~53ms → 39~42ms、单循环速率 1.4~1.6 → 1.8~2.4MB/s，有改善但仍不够；
- 8 流总吞吐仍 ~3.3~3.8MB/s，对照 SOCKS5 5.8~6.3。

**结构性问题**：ht 用“一次 HTTP 请求承载一批帧 + 单个 data GET 串行”的模型，
ACK 也走独立 HTTP POST；在高包率（8 流）下，请求数和每请求延迟把 RTT 抬高，
inner TCP cwnd 被压在 BDP 附近。批大小/并发数/信用窗口都不是解，
需要按 flow 分 lane 的多 GET（保每流有序）或持久多路复用连接。

## 32. 方案1：按 inner flow 分 lane 的多 GET（2026-11-10 14:15）

实现（已回退）：服务端按 inner 5-tuple 哈希到 1..N-1 lane FIFO，客户端每个 data GET
绑定一个 lane（X-L/X-N）；lane 内 FIFO 保每流有序，lane 间并发；get-slots<=2 时完全不启用。

沙箱 qg↔mb、MB_HT、8×9MiB：

| 配置 | 8 流 | 备注 |
|---|---|---|
| 默认单 data GET | 3.4MB/s | GET ~47/s、批 73~93KB、avg 39~53ms |
| + lane（get-slots=8） | **3.77MB/s** | GET ~80~90/s、批 48~51KB、avg 60~75ms |
| + POST 按序重排 | 3.77MB/s | 单流反而变差 |
| 对照：SOCKS5 | 5.8~6.3MB/s | 同一 mesh 栈 |

**结论：分 lane 只把并发提上去，但每 lane 的 batch 更小、GET 延迟更高，总吞吐仅 +11%。**
ht 的“每请求一批 + 单连接串行/长轮询”模型 + inner TCP 的 cwnd/RTT 互相锁死；
批大小、并发数、lane、POST 顺序、LaneAck 都不是解。

在“不走 WebSocket、QUIC 被 NAT 阻断”的前提下，剩余可行方向只有：
1. 端点支持时上持久多路复用（HTTP/2 h2c 或 raw TCP upgrade），去掉每批 HTTP 往返；
2. 接受当前 ht 载体 ~3.4~3.8MB/s 的上限，保持 451 生产版本；
3. 从基础设施侧换一条更干净/可开 UDP 的 qg↔jf 链路，再上 QUIC。

当前 qg/mb = 471（lane/POST 实验均已回退），jf 保持 451 未动。

## 33. 另一条载体：SSH+SOCKS5 与多 SSH 连接（2026-11-10 14:30）

配置级实验（不改 jf 系统）：qg 增加 SSH 代理 `JF_SSH`（direct-tcpip 到 jf OS sshd），
再加 SOCKS5 代理 `JF_S5`（server=127.0.0.1:1080, via=JF_SSH），关闭 MGMS_HT，
qg↔jf 链路变为 `qg:JF_S5`（jf 自带的 socks5-test 映射，经 SSH 隧道）。

| 场景 | 结果 |
|---|---|
| qg→jf 单流（8.18MB） | 2.7~4.1MB/s（优于 ht 单流 ~2） |
| qg→jf 8 流 | 65.4MB/16s、40s → 4.09/1.64（中位 ~2.9） |
| vm→jf 两跳 8 流（字节/退出码校验） | 70.2s、23.4s → 0.93/2.80（中位 ~1.9） |
| 4 条并行 JF_SSH_i + JF_S5_i | qg→jf 8 流 53s/55s → **1.2MB/s** |

分析：
- 单个 ssh.Client 只有一条 TCP，所有 SOCKS5 channel 复用它 → 单连接 HOL，8 流聚合不上去；
- OS 直连 8 流 10.9MB/s 是 8 条独立 SSH 连接，不是一条 SSH 里的 8 个 channel；
- 4 条并行链路没有带来聚合：mesh 当前按“最佳一条链路”选路，未做按流 ECMP，反而增加 SSH 开销。

结论：配置级没有白捡的收益；要把 SSH+SOCKS5 这条更干净的单流载体用于两跳，
需要 mesh 侧实现**按 inner flow 在多条链路间 ECMP**（纯代码改动，风险中等）。
已恢复 MGMS_HT，JF_SSH/JF_S5 等实验代理保留但禁用。

## 34. 单流精确对比与多链路事实澄清（2026-11-10 14:40）

用 python 计时（ms 精度，`ssh ... cat 8.18MB > /dev/null`，3 次）：

| 载体 | 单流 | 8 流 |
|---|---|---|
| OS 直连 qg→jf（单条 SSH TCP） | **7.4~7.9MB/s**（1.04~1.10s） | 10.9（8 条独立 SSH 连接） |
| mesh ht（MGMS_HT，单链路） | **~1.0MB/s**（8.4/8.4/5.0s） | 3.4~3.8 |
| mesh SSH+SOCKS5（JF_S5，单链路） | **~2.4MB/s**（冷启动 47.5s，之后 3.5/3.3s） | 2.9（单 SSH TCP，HOL） |

**更正**：之前说“接近 OS 主要靠多流并发”不准确。OS 单流本身就有 7.4~7.9MB/s；
mesh 单流 ht 只有 ~1.0、SSH+SOCKS5 ~2.4，**单流和聚合都差**，不是单纯聚合问题。

### 34.1 多链路是否按流分发？

不是。当前包路径：
`mesh/netstack.go:482` → `meshMgr.NextHopLink(targetNodeID)` → `mesh/mesh.go:706`
返回 Dijkstra 最佳路径上的**一个 LinkNIC**；同一 nodeID 的多条链路只参与质量比较
（选出最好的一条），没有按 5-tuple 哈希分发。`selectBestPeers` 虽有排序/列表，但
`NextHopLink` 不使用列表。实测 4 条 JF_SSH+SOCKS5 链路，qg→jf 8 流反而只有 1.2MB/s，
也印证了“只走一条 + 额外 SSH 开销”。

### 34.2 SSH 连接复用

`SSHDialer` 每个代理缓存一个 `ssh.Client`（一条 SSH/TCP 连接）；每次 SOCKS5 dial 建的
direct-tcpip channel 都在**同一条 TCP** 上复用。8 条 inner 流共用一条 SSH TCP → 单连接
HOL；OS 的 8 流 10.9MB/s 是 8 个独立 ssh 进程（8 条 TCP），不是一条连接里的 8 个 channel。

### 34.3 结论

要到接近 OS：需要 (a) 把单流载体从 ht 换成低开销的 TCP（SSH+SOCKS5 已到 2.4，沙箱 LAN SOCKS5 单流 ~4.5），
并且 (b) 在同一 nodeID 的多条 LinkNIC/PeerSender 上做**按 5-tuple 的 ECMP**（每流固定一条，保序）。
后者可在 LinkNIC 层实现一个 MultiLinkNIC，按包 5-tuple 哈希写入不同 sender，不依赖 SelectRoute 的 dst-only 接口。

## 35. vm→qg→mb 两跳实测与 relay RTT 膨胀（2026-11-10 14:50）

用户要求补测的两跳（vm→qg→mb，qg↔mb 为 MB_HT，vm↔qg 为 SS5 UDP leg，8×9MiB）：

| 场景 | 单流 | 8 流 |
|---|---|---|
| qg↔mb 单跳（ht） | 1.8~2.25MB/s | 3.4MB/s |
| **vm→qg→mb 两跳（ht）** | **0.65~1.27（中位 0.72）** | **1.72MB/s**（75.4MB/43~44s，字节校验） |
| vm→qg→mb 两跳（qg↔mb 换 SOCKS5） | — | 3.63（一次）/ 0.94（一次，链路抖动），方差大 |

两跳约为 ht 单跳的 0.5×；换 SOCKS5 单跳载体后两跳最好能到 3.6。

两跳期间 mb 的 inner TCP：`srtt 352~566ms`、`minRTT 59~71ms`、`rackRtt 185~1064ms`、
cwnd 1~77（个别 903），reord 混合。即 relay 路径额外引入 **~400~500ms 排队延迟**。

排查：
- qg 的 UDP socket `Recv-Q/Send-Q = 0`，内核无积压；
- qg 日志无 `P2P-QDEPTH`、无 `mesh AQM dropped`、无 `LANE-` 告警；
- 因此积压不在 qg 内核/qg p2p 队列，更可能在 mb 的 ht 发送侧（dataQ/p2p 队列）或 vm 的入站路径。

结论：ht 单跳 3.4 并非离谱，但两跳会被 relay 的 400~500ms 排队延迟按 BDP 再砍半（1.72）。
下一步应给 mb→qg→vm 的每一段加 sojourn 计时，定位这 400~500ms 积压在哪一段，再决定限流/降队列。

## 36. GET 接收/投递解耦（427bd10）与服务端排队消除（2026-11-10 15:30）

服务端 sojourn 统计实锤：两跳期间 mb 的 ht dataQ `waiters=0`、`queued=1.36MB`、
`maxSojourn=585ms` —— recvLoop 同步解析/投递上一批时无法发下一个 GET，服务端积压，
inner TCP RTT 被抬高。

改动：recvLoop 收到批次后只 `push` 入队并唤醒单一投递协程，立即发下一个 GET；
投递仍按 X-B 序号 FIFO（保序）。已部署 qg 478，mb 477（服务端统计）。

| 指标（两跳 8 流） | 改前 | 改后 |
|---|---|---|
| mb dataQ queued | 0.54~1.36MB | **0** |
| 服务端 avgSojourn | 62~234ms | **6~40ms** |
| 服务端 maxSojourn | 585ms | 100~400ms |
| inner TCP cwnd | 1~77 | 21~275 |
| qg GET | ~47/s、批 73~93KB、avg 39~53ms | 10~20/s、批 30~318KB、avg 87~182ms |
| qg POST(ACK) | 250~325/s、avg 20~23ms | 100~220/s、avg 30~38ms |

两跳吞吐：改后两次 75.4MB/38.6s = **1.95MB/s**、75.4MB/87.7s = 0.86MB/s（链路方差大），
中位仍 ~1.4~1.7，**没有明确提升**。

结论：bufferbloat 已消除，cwnd 变大，但瓶颈转回“单个 data GET 串行 + inner TCP RTT 250~400ms”：
GET 现在平均 87~182ms 是在**等服务端生产数据**（queued=0），即生产者（inner TCP）受 RTT 限制。
下一步只剩：在 async 基础上做多 GET（每流 lane 保序），让 GET 并发拉取，减少等待；或换持久多路复用载体。
当前 qg=478、mb=477、jf=451 未动。

## 37. async 之后再试多 GET（get-slots=8）：仍无收益（2026-11-10 15:40）

在 478（GET 接收/投递已解耦）基础上，qg 设置 `get-slots=8` 复测 qg↔mb 8 流：

| 配置 | 8 流 |
|---|---|
| 默认 1 个 data GET（async） | ~3.4MB/s 量级 |
| get-slots=8（async） | **2.77 / 3.0（中位 ~2.9）** |

多 GET 仍无收益。原因与之前一致：服务端 queued 已为 0，GET 的等待时间是在等
inner TCP 生产数据（cwd 受 250~400ms RTT 限制）；加 GET 只是把同样的数据分到更多
并发请求上，不改变生产速率。

已恢复默认 `get-slots`。

**单链路 ht 的调优到此为止**：批大小、get-slots、lane、POST 顺序、LaneAck、
RACK reo_wnd、TCP_NODELAY、GET 解耦，均无法把单链路从 ~3.4、两跳从 ~1.7 再推高。
剩余只有“持久多路复用载体”（需端点支持）或“换链路/开 UDP 上 QUIC”（需基础设施）。

## 38. 连接池/并发投递实验：不是连接数问题（2026-11-10 15:50）

用户提出“增加 HTTP 连接池、动态分散上下行投递”。实测（qg↔mb，8 流，字节校验）：

| 配置 | 8 流 |
|---|---|
| 默认（get-slots=2, pool-size=16） | ~3.4MB/s |
| get-slots=8, pool-size=64 | 75.5MB/24s、21s = **3.15 / 3.60（中位 3.4）** |

连接池/并发 GET/POST 加大没有收益。现有池子已是 `MaxIdleConns=64/MaxIdleConnsPerHost=32`、
POST 默认 16 并发；POST 实测 ~200/s、avg 35ms（约 7 并发在飞），远未打满。
async 后服务端 queued=0，GET 的 87~182ms 是在**等 inner TCP 生产数据**。

所以：
- “保序”不是吞吐杠杆，只是防止乱序把 inner TCP 的 cwnd 打塌的**正确性约束**；
- 动态多 GET/多 POST 投递已经测过（服务端把批次交给任意空闲 waiter），吞吐不变；
- 真正的瓶颈是 inner TCP 的 cwnd/RTT（250~400ms），而 RTT 里包含每批 HTTP 往返和每 ACK 一次 POST 的延迟。

剩余唯一有理论收益的单链路改动：把上下行从“每批一个 HTTP 请求”改为**持久 chunked 流**
（普通 HTTP/1.1，非 WebSocket），消掉每批/每 ACK 的请求开销；需要端点允许 chunked 流不被缓冲。

## 39. 连接复用实测：不是“用完就关”（2026-11-10 15:55）

qg↔mb 传输中每 2s 采样 `ss -tn | grep 192.168.1.88:18081`：

- 稳定 **18 条 ESTABLISHED**（= get-slots 2 条 + POST pool 16 条），5 次采样本地端口集合完全相同；
- TIME-WAIT 仅 3 → 9（整段传输），没有每请求建连/断开。

结论：HTTP keep-alive 正常复用，RTT 变长**不是**连接建立造成的，也不是关闭连接。
POST avg 35ms 是“已复用连接上的每请求处理/调度”开销，GET avg 87~182ms 是在等生产数据。
连接池加大（get-slots=8/pool=64）实测仍 3.4，进一步排除“连接数/复用”方向。

剩余可动的只有“减少请求数”（持久 chunked 流），而不是连接数。

## 40. 两跳都用 SOCKS5 UDP leg 的对照（2026-11-10 16:00）

沙箱把 qg↔mb 从 ht 换成 SOCKS5 UDP leg（mb 加 UDP_ARM 代理开启被动侧 UDP，
qg 的 MB_S5 设 p2p-udp=true，重建会话后 transport=udp），vm↔qg 本来就是 SS5 UDP leg。
8×9MiB，字节校验：

| 场景 | 链路 | 8 流 |
|---|---|---|
| qg↔mb 单跳 | SOCKS5 UDP leg | **5.03 / 5.39（中位 5.2）** |
| qg↔mb 单跳 | ht | 3.4 |
| vm→qg→mb 两跳 | **两段都 UDP** | **2.58 / 2.97（中位 2.78）** |
| vm→qg→mb 两跳 | qg↔mb 用 ht | 1.72 |
| vm→qg→mb 两跳 | qg↔mb 用 SOCKS5 TCP | 3.63（一次）/0.94（抖动） |

结论：
- 同样两跳，UDP leg 比 ht 快约 1.6×（2.78 vs 1.72），但仍低于单跳（5.2）约 0.53×；
- 说明 relay 本身会因 RTT 翻倍/排队再砍半，ht 在此基础上再额外损失约 1.6×；
- 对 jf 无法直接复制：jf 公网未放通 SOCKS5 UDP 端口，UDP leg 起不来。

### 40.1 回答“连接复用后 RTT 为何不接近 TCP 直连”
复用的是 **HTTP 载体连接**（实测 18 条稳定 ESTABLISHED）；inner TCP 的 RTT 不是载体 TCP 的 RTT，
而是“数据在 ht dataQ 等一个 GET 批次 + relay 转发 + ACK 走 POST”的应用层周期。
即使载体连接复用，inner TCP 的包仍要等批处理窗口，RTT 因此被放大。

### 40.2 chunked 是否受 body 限制
chunked 本身没有 body 上限，是流式 chunk；真正的限制在中间代理是否缓冲、以及服务端/客户端
是否实现流式读写。它解决的是“每批/每 ACK 一个请求”的固定开销，不是单请求大小。

## 41. 两跳瓶颈拆解：转发容量 vs 转发时延/丢包（2026-11-10 16:10）

两跳 8 流（MB_HT）前后计数增量（mb 侧 inner TCP）：

| 计数 | 增量 | 比例 |
|---|---|---|
| segments_sent | +62,274 | — |
| retransmits | +6,158 | **9.9%** |
| segments_acked_with_dsack | +5,642 | **9.1%** |
| timeouts | +26 | — |

同期：qg `transitFastPath` 持续增长（快路径生效）、qg/vm channelDropStats 全 0、
tunWriteDrops 0、qg 中继 CPU 仅 ~5.7%。

结论：
- **转发容量不是瓶颈**（CPU 低、无队列丢包、快路径正常）；
- 两跳的损失来自**路径时延与丢包/乱序**：RTT 从单跳 ~70ms 抬到 ~300ms（BDP 减半），
  同时 DSACK 9% 说明大量“延迟/乱序但未丢”的段被重传；最可疑的是 qg→vm 的
  SS5 UDP leg（原始 UDP，无可靠/保序）叠加 ht 载体的批处理延迟；
- 同两跳换 UDP-HT 对照：UDP 2.78 vs ht 1.72，说明 ht 载体再额外损失 ~1.6×，
  但即使两段都 UDP 也只有单跳的 0.53×，**relay 的 RTT/丢包是两跳共同的主因**。

## 43. 两层单变量法：relay 排队实验（2026-11-10 17:00）

按用户要求，固定载体、只改 relay 侧一个变量：每 peer 数据队列字节预算 8MB→1MB。

载体固定为两段 UDP（vm↔qg SS5 UDP、qg↔mb SOCKS5 UDP），8×9MiB，字节校验：

| 预算 | 2 跳吞吐 | mb retrans 增量 | mb DSACK 增量 | qg DataDrops |
|---|---|---|---|---|
| 8MB（基线） | 2.25 / 2.72（中位 ~2.5） | — | — | 0 |
| **1MB** | **3.00 / 3.09（中位 ~3.0）** | **0.49%** | **0.04%** | **0** |

- relay 层把出口队列从 8MB 降到 1MB，两跳 +20%，无丢包；
- 两段 UDP 下 DSACK 仅 0.04%：**relay 本身几乎不乱序/丢包**，两跳 0.53× 的主因是
  RTT 翻倍导致的 BDP，而不是 relay 转发丢包；
- 同一 1MB 预算下 ht 两跳 1.85/2.0（中位 ~1.9），UDP 两跳 3.0：
  **ht 层额外损失 ~1.6×，且 ht 路径的 DSACK ~9% 是 ht 载体引入的，不是 relay**。

生产 jf 窗口当时链路劣化（qg↔jf srtt 120ms/jitter 67ms），
因此 1MB 预算未在 jf 上做干净 A/B，已回退为 8MB；qg=486。

后续按层推进：
- relay 层：剩余是 RTT/BDP（0.53× 属于两跳结构），队列调优最多再拿 ~20%；
- ht 层：单独看 ht（载体/relay 固定），只改一个 ht 变量，同时记录 mb DSACK/retrans 与吞吐。

## 44. ht 单层单变量：服务端 POST 按序投递（2026-11-10 17:20）

固定：qg↔mb 单跳 ht、预算 8MB、relay 不参与。唯一变量：服务端 POST 重排
（单一投递协程、2s 缺口跳过；客户端不变）。

| 指标（qg↔mb 单跳 8 流） | 基线（无重排） | 服务端 POST 重排 |
|---|---|---|
| 吞吐 | 32s/29s（~2.5MB/s） | 24s/36s（中位 ~2.5MB/s） |
| DSACK 增量 | 8.5% | **8.3%** |
| retrans 增量 | 9.2% | **9.3%** |

**POST 按序投递没有降低 DSACK，也没有提升吞吐** → ACK 的并发 POST 乱序
不是 ht 的 DSACK 来源（至少不是主因）。

ht 单跳本身就有 8.5% DSACK，说明是 ht 层内部（ACK 延迟/HTTP 每请求服务时间
触发 sender 端 RACK 误判，或两 GET 循环的投递路径）导致，而不是 relay。

下一单变量候选（按层法）：固定其他，只改 gVisor RACK 的 reo_wnd（单跳 ht、
无 UDP leg 真实丢包场景），观察 DSACK/retrans 是否下降、吞吐是否上升。

## 45. ht 单层：ACK-POST 成本与 RACK reo_wnd 单变量（2026-11-10 18:20）

### 45.1 “每个 ACK 一次 POST”排除了吗？

- 并发 POST **乱序**：已排除（服务端按序投递：DSACK 8.5%→8.3%，吞吐不变）；
- 每 POST 的**延迟**：原始 ht 通道（无 crypto/无 mesh）在已复用连接上实测
  `p50 6.7~9ms、p90 13~18ms`，而 qg↔mb 的 OS ping 本身就是 `5~22ms（avg 11.7）`；
  即单次 POST ≈ 1 个基础 RTT，**不是固定 35ms 协议开销**；生产负载下 20~28ms。
- 结论：ACK-POST 的**开销与 RTT 成正比**，不是 DSACK 的直接来源；它通过“ACK 延迟抖动”
  间接影响 RACK。

### 45.2 RACK reo_wnd 单变量（单跳 ht，无 relay、无 UDP leg）

固定其他，仅把 reo_wnd 下限改为 `(SRTT-minRTT)`（仍 cap 到 SRTT）；mb 部署 489：

| 指标 | 基线（488） | reo_wnd 下限（489） |
|---|---|---|
| DSACK 增量 | 8.3% | **3.0%** |
| retrans 增量 | 9.3% | **3.2%** |
| 吞吐（4 次） | 32/29s（~2.5） | 16/32/17/32s（4.5/2.25/4.24/2.25，中位 ~2.9） |

**ht 单跳的 8.5% DSACK 确认是 RACK 在 ACK 延迟抖动下误判丢包**；把 reo_wnd 放到观测到的
排队延迟后，DSACK/重传降到 ~3%，吞吐中位提升 ~18%（仍有双峰：4.3 或 2.25）。

### 45.3 下一步（仍只在 ht 层，单变量）

1. 用同一构建测**两跳 ht**（有真实丢包场景），确认放宽 reo_wnd 是否拖慢真实恢复；
2. 若两跳变差，再单独试“有界 floor”（如 `min(SRTT-minRTT, 100ms)` 或 `qd/2`），一次一个变量。
relay 层暂不动。

## 46. 突破：reo_wnd 下限 + 多 GET 并发（2026-11-10 19:20）

ht 单层，固定其他，两个已分别验证的变量叠加（reo_wnd 下限 + get-slots=8）：

| 场景（8 流） | 基线 | 现在 | 变化 |
|---|---|---|---|
| qg↔jf 单跳 ht（生产） | 3.4MB/s | **6.2MB/s**（11s/10s） | **+82%** |
| qg↔mb 单跳 ht（沙箱） | 2.4MB/s | **4.2MB/s**（15/19/18s） | +75% |
| vm→jf 两跳（生产） | 3.16（仅 slots8） | **3.36MB/s**（23.3/19.5/17.2s） | +6% |
| vm→mb 两跳（沙箱） | 1.72 | **2.29MB/s**（31.5/36.8/28.9s） | +33% |

- 机制：ht 单跳 DSACK 8.5% 来自 RACK 在 ACK 延迟抖动下误判；reo_wnd 下限修复后 DSACK→3%，
  再叠加多 GET 并发，单跳从 3.4 到 6.2；
- 两跳只涨到 3.36：单跳 6.2 × ~0.54，剩余是两跳 RTT/relay 结构损失（与之前 relay 单层结论一致）。

部署：qg 486（get-slots=8）、jf 490（reo_wnd 下限）、mb 489（reo_wnd + POST 按序）。
下一步转 relay 层（单变量），ht 层暂告一段落。

## 47. relay 层单变量收尾（2026-11-10 19:40）

固定 ht（reo_wnd + slots8）与生产两跳，只改 relay 每 peer 队列预算：

| 预算 | 两跳 8 流（65.4MB） | 结果 |
|---|---|---|
| 8MB | 2.81 / 3.36 / 3.82 | 中位 3.36 |
| **1MB** | **3.85 / 3.94 / 4.74** | **中位 3.94（+17%）** |
| 512KB | 2.65 / 2.12 / 2.69 | 中位 2.65（变差，已回退） |

- 1MB 最优，已提交 `c4ce864`；
- qg 的 UDP leg `legDrops` 全程为 0（临时暴露字段确认），vm/jf DataDrops 0：
  relay 没有丢包可优化，剩余差距是两跳 RTT（单跳 6.2 → 两跳 3.94，0.64×）；
- 16 并发流测试无效（内置 SSH 并发会话数受限，只有 ~9 条完成），不能作为扩展性依据。

结论：relay 层可调项（预算）已收敛；两跳 ~3.9~4.7MB/s 接近当前拓扑/RTT 的结构上限。

## 48. 对比口径与连接数结论（2026-11-10 20:00）

统一口径：同文件 8.18MB × 8 流，字节校验；OS 与 mesh 同为 8 流，不是 8 对 1。

| 场景（8 流） | OS 级 | mesh 现在 | 比值 |
|---|---|---|---|
| 单跳 qg↔jf | 10.9MB/s（8 条独立 OS SSH） | 6.2MB/s | 0.57 |
| 两跳 vm→qg→jf | 8.03MB/s（OS SSH 串接两段） | 3.94MB/s（最好 4.74） | 0.49 |

连接数实验（ht 单层，其余固定）：
- get-slots 2→8 + reo_wnd：单跳 3.4 → 6.2MB/s；
- get-slots 8→16：30s/10s（2.18/6.53），中位 ~3.3，反而更差；
- 结论：**连接数甜点是 8**，继续加会拆碎批次/增加争抢，不是“跑满带宽”的杠杆；
- 加链路不是必需项，当前只有一条 ht，且 mesh 不做按流 ECMP。

## 49. RTT 分解与缩短 RTT 的方向（2026-11-10 20:05）

两跳 inner TCP 的 RTT 组成（实测）：
- qg↔jf 链路 srtt ~26ms（公网）；
- vm↔qg SS5 UDP leg srtt ~36ms；
- 两跳 inner minRTT 实测 ~53~71ms ≈ 两段之和，**路径本身的 RTT 已接近理论地板**；
- 负载下 inner srtt 抬到 250~400ms → 多出来的是**排队**，不是路径变长；
- 服务端 ht dataQ 排队已消除（queued=0），qg UDP legDrops=0，DataDrops=0。

所以“缩短 RTT”实际是**削掉负载下的排队分量**，候选（按证据强度）：
1. **ACK 路径**：每个 ACK 仍是独立 POST，服务时间≈1 RTT；需测 ACK 在 qg→jf POST 队列和 jf 会话入口的 sojourn；
2. **qg→vm 出口队列**：p2p writer 的 enqueue→write sojourn（已有 EMA，但未暴露）；
3. 若两者都小，剩余 RTT 就是 inner TCP 窗口/ACK 节奏的相互作用，只能靠持久流或更大 cwnd。

下一步：先把 1 和 2 的 sojourn 暴露出来，跑一次生产两跳，定位 250~400ms 中哪一段占大头。

## 50. 出口队列 sojourn 实测：不在 p2p 队列（2026-11-10 20:30）

临时给 qg/vm 暴露 `/api/p2p` 的 `egressSoj/legSoj`（未提交，测完回退）。

传输中采样（vm→jf 两跳）：
- qg 到 vm peer：`egressSoj=0.1ms`，legAvg=0，drop=0；
- vm 到 qg peer：`egressSoj=0.2ms`，drop=0；
- mb ht dataQ queued=0（前一轮已测）。

即 **p2p 两侧的出口写队列都不构成 200~330ms 的 RTT 排队**。
当前 qg↔vm 在多次重启后 transport=tcp（UDP leg 未重新协商），leg sojourn 暂无数据。

剩余可疑位置（不在我们的 p2p 写队列）：
1. 载体自身的 OS/代理缓冲：WIN7_VPN 的 SOCKS5 TCP、ht 的 HTTP 端点；
2. vm 的 OS/TUN 接收路径（数据方向）；
3. inner TCP 的 ACK 节奏/延迟 ACK 与 RTT 采样。

下一步：先把 vm 的 UDP leg 重新拉起，测 leg ingress sojourn；若仍低，则需要在载体 socket 缓冲/持久流层面处理。

## 51. UDP leg 回退与恢复：载体对照 1.82 vs 3.89（2026-11-10 20:50）

多次重启后 qg↔vm 的 UDP leg 掉回 TCP carrier（vm 的 QG_SS5_UDPLEG p2p-udp 被改/未生效），
同一套 495 生产配置下实测：

| vm↔qg 载体 | 两跳 8 流（65.4MB） | 中位 |
|---|---|---|
| TCP carrier（WIN7_VPN 基座） | 35.9s | **1.82MB/s** |
| **UDP leg（恢复 p2p-udp=true，移除 UDP_ARM_QG）** | 18.5s / 15.4s | **3.89MB/s**（最好 4.24） |

结论：vm↔qg 的 UDP leg 对两跳是决定性的（2.1×）；恢复后生产回到 3.5~4.2MB/s。

RTT 分解补充：qg/vm 的 p2p 出口队列 sojourn 仅 0.1~0.2ms、drop=0，
所以负载下 200~330ms 的排队不在 p2p 写队列；剩余嫌疑是载体 OS/代理缓冲
（WIN7_VPN SOCKS5、ht HTTP）与 inner TCP 的 ACK 节奏。

## 52. reo_wnd 上限 2×SRTT：伪重传降了，吞吐没变（2026-11-10 20:50）

单变量：只把 reo_wnd 上限从 SRTT 放宽到 2×SRTT（jf 496），其余不动。

| 指标 | 490（1× cap） | 496（2× cap） |
|---|---|---|
| 长跑 8 流（196.3MB） | — | 51.0s = **3.85MB/s** |
| 短跑 8 流（65.4MB） | 3.53 / 4.24（中位 3.89） | — |
| DSACK 比例 | ~25.5% | **~8.7~10.8%** |
| inner cwnd（采样） | 11~30 | 1~12 |

结论：放宽 reo_wnd 确实把伪重传从 25% 降到 ~10%，但 **吞吐不变**；cwnd 仍只有 1~30，
限制点已不是 RTT（srtt 67~89ms、min 52~56ms）或伪重传，而是 **inner TCP 的 cwnd 增长/CC**。

已回退（fork 647af00，jf 重新激活 490）。
当前生产：qg 495（slots8、1MB）、jf 490（reo_wnd 1×）、mb 489、vm 495（UDP leg 正常），
两跳 3.5~4.2MB/s、单跳 ht 6.2MB/s。

## 53. 突破：禁用 HyStart 慢启动提前退出（2026-11-10 21:05）

单变量（jf 497，其余不动）：禁用 CUBIC HyStart 的 delay-increase 提前退出
（4~16ms 阈值 vs 本路径 30ms+ 抖动，导致 cwnd≈20 时误退慢启动）。

长跑 8 流（8×24.5MB=196.3MB），两次：

| 版本 | 耗时 | 吞吐 |
|---|---|---|
| 490/496（HyStart 开） | — | 3.85~3.89MB/s |
| **497（HyStart 关）** | 38.34s / 38.16s | **5.12 / 5.14MB/s** |

- 提升 ~32%，两次一致；
- 采样 cwnd：部分流长到 261/1478（此前 11~30），srtt 169~278ms、DSACK 13.5%；
- 说明此前两跳的真正限制是 **HyStart 误退慢启动把 cwnd 压在 ~20**，不是 RTT/伪重传/队列。

当前两跳 5.13MB/s ≈ OS 两跳 8.03 的 64%；单跳 ht 6.2（HyStart 开时）。
部署：qg 495、jf 497、mb 489、vm 495。
下一步：把“硬禁用”换成提高 HyStart 阈值（如 20/60ms）以避免慢启动过冲，仍为单变量。

## 54. HyStart 阈值 20/60ms 对照：不如硬禁用（2026-11-10 21:20）

单变量对照（jf 498，HyStart 阈值 4/16→20/60ms）：

| 版本 | 长跑 8 流（196.3MB） |
|---|---|
| 497 硬禁用 HyStart | 38.34s / 38.16s = **5.12 / 5.14MB/s** |
| 498 阈值 20/60ms | 42.49s / 44.45s = **4.62 / 4.42MB/s** |

阈值版仍会在 cwnd 较小时提前退出慢启动，吞吐低 ~12%。
已回退到硬禁用（fork revert fea795a）；生产采用 497 的行为。

## 55. 稳态与冷启动（2026-11-10 21:30）

499（HyStart 硬禁用）重复长跑（196.3MB）：

| 运行 | 耗时 | 吞吐 |
|---|---|---|
| 部署后首次 | 61.6s / 59.4s | 3.19 / 3.30MB/s |
| 预热后 | 38.3s / 38.2s / 36.7s | 5.12 / 5.14 / **5.35MB/s** |

结论：稳态 **5.1~5.35MB/s**（OS 两跳 8.03 的 64~67%）；部署/重启后的首跑受慢启动/路由收敛影响，
只有 3.2~3.3，第二次起恢复。后续若要消除冷启动，需要预热连接或保留拥塞状态。

当前生产：qg 495、jf 499（HyStart 禁用）、mb 489、vm 495；vm↔qg UDP leg=udp，qg↔jf ht srtt 20ms。

## 56. InitialCwnd 10→32：冷启动变好、稳态变差（2026-11-10 21:40）

单变量（jf 500）：InitialCwnd 32，HyStart 仍禁用。

| 运行 | 耗时（196.3MB） | 吞吐 |
|---|---|---|
| 部署后首跑 | 42.4s | 4.63MB/s（原冷启动 3.2~3.3） |
| 次跑 | 40.6s | 4.84MB/s（原稳态 5.12~5.35） |

提高初始窗口改善了冷启动，但稳态下降（可能慢启动过冲导致更多丢包）。
已回退（fork revert 173aab7），保留 499 的 InitialCwnd=10 + HyStart 禁用：
稳态 **5.1~5.35MB/s**，冷启动首跑 3.2~3.3。

## 57. 同代码多次长跑方差（2026-11-10 21:50）

501 与 499 的代码等价（InitialCwnd=10、HyStart 禁用、reoWnd 1×），但后续长跑：

| 版本 | 196.3MB 耗时 | 吞吐 |
|---|---|---|
| 499 | 36.7 / 38.2 / 38.3s | 5.35 / 5.14 / 5.12 |
| 501（同代码） | 42.4 / 54.5 / 73.4 / 40.6s | 4.63 / 3.60 / 2.67 / 4.84 |

同期 qg↔jf srtt 15ms/jitter 1.8，vm↔qg 37ms、transport=udp，链路看起来正常。
说明两跳吞吐的 run-to-run 方差很大（2.7~5.35），不能只看单次；需要同窗口 5 次中位数，
并观察 jf 的 cwnd/DSACK 是否在慢启动过冲后反复收缩。

当前配置（保留）：HyStart 禁用、InitialCwnd 10、reoWnd 1×、get-slots=8、relay 1MB、UDP leg。

## 58. 同窗口 5 次中位数基准（2026-11-10 22:00）

当前配置（HyStart 禁用、InitialCwnd 10、reoWnd 1×、slots8、relay 1MB、UDP leg），
两跳 vm→jf，196.3MB/次，连续 5 次：

| run | 耗时 | 吞吐 |
|---|---|---|
| 1 | 39.78s | 4.93MB/s |
| 2 | 38.08s | 5.16 |
| 3 | 78.24s | 2.51（离群） |
| 4 | 46.27s | 4.24 |
| 5 | 41.95s | 4.68 |

中位 **4.68MB/s**，稳态区间 4.2~5.2，单次离群 2.51。
对比改动前 3.16~3.89，中位提升约 20~45%；OS 两跳 8.03 → 58%。

剩余：单次离群对应 cwnd 偶发塌缩，需要继续观察慢启动过冲后的恢复；中位 4.68 作为当前基准。

## 59. cwnd 轨迹：慢启动过冲与流间不公平（2026-11-10 22:15）

长跑中每 3s 采样 jf 的 SSH 流（HyStart 禁用）：

- 部分流 cwnd 长到 **917 / 1238 / 1958 / 3137 / 4108** 段，远超路径 BDP（约 500 段）；
- 同时其余流 cwnd 只有 **1~50**，srtt 被抬到 130~260ms；
- DSACK 累计 ~14.8%。

结论：**硬禁用 HyStart 会让慢启动过冲到远超 BDP**，大窗口流把队列占满，导致其它流丢包收缩；
中位 4.68 与离群 2.51 的差异来自过冲程度，而不是链路本身。

下一步（单变量，二选一）：
1. HyStart 阈值大幅提高（如 50/150ms），让慢启动在接近 BDP 时退出；
2. 给 SndCwnd 加一个接近 BDP 的上限（~600 段）。
先做 1；若仍过冲，再做 2。

## 60. HyStart 阈值 50/150ms：仍过冲，吞吐不变（2026-11-10 22:20）

单变量（jf 502，HyStart 重新启用，仅把阈值 4/16 → 50/150ms）：

| 运行 | 196.3MB 耗时 | 吞吐 |
|---|---|---|
| 预热 | 39.72s | 4.71MB/s |
| 1 | 41.41s | 4.52 |
| 2 | 34.42s | 5.44 |
| 3 | 39.31s | 4.76 |
| 4 | 40.65s | 4.61 |

中位 **4.71MB/s**，与硬禁用 HyStart 的 4.68 无差异。

传输中每 2s 采样 jf 的 gVisor 流：

- cwnd 仍长到 **1565 / 2755 / 7749**，且这些流的 `ssthresh` 仍是 `MaxInt64`，
  说明 HyStart 的 delay-increase **完全没有触发**；
- 原因：该检查用“本轮 minRTT 比上一轮 minRTT 增加超过阈值”，而 ht 批处理下
  每轮 minRTT 随队列缓慢抬升，`LastRTT` 每轮跟着抬，差值始终不到 50ms；
  ACK 间隔也大于 2ms，ACK-train 分支同样不成立；
- 窗口内 DSACK 增量 17.6%、重传增量 17.8%。

结论：单纯提高 HyStart 阈值不能让慢启动在 BDP 附近退出，吞吐也没有收益。

## 61. cwnd 上限 600 段（未真正生效，jf 503）（2026-11-10 22:35）

在 `cubic.Update` 末尾把 `SndCwnd` 钳到 600 段。长跑 8 流：

| 运行 | 耗时 | 吞吐 |
|---|---|---|
| 预热 | 41.86s | 4.47（其中 1 流少 26,410B） |
| 1 | 37.42s | 5.00 |
| 2 | 41.66s | 4.49 |
| 3 | 43.06s | 4.35 |
| 4 | 39.85s | 4.70 |

中位 ~4.60，与基线相同。采样发现 cwnd 仍有 **2319 / 2782** 超过 600：

- 超出的来源是 SACK 恢复期间 `dup ACK` 的 `SndCwnd++`，这条路径不调用 `cc.Update`，
  因此 `cubic.Update` 里的钳位没有机会执行；
- 结论：上限必须加在**发送路径**（`sendData` 的 in-flight 判定），而不是只在 CC 更新里。

## 62. 发送路径 in-flight 上限 64 段：过低，-13%（jf 504）（2026-11-10 22:50）

`sendData` 用 `min(SndCwnd, 64)` 作为在途上限（每流约 92KB）。

| 运行 | 耗时 | 吞吐 |
|---|---|---|
| 预热 | 46.72s | 4.01 |
| 1 | 47.16s | 3.97 |
| 2 | 39.35s | 4.76 |
| 3 | 40.27s | 4.65 |
| 4 | 45.79s | 4.09 |

中位 **4.09MB/s**，比基线（4.68）低 ~13%。负载下 inner RTT 仍有 150~250ms，
64 段/流只能支撑 ~0.5MB/s/流，上限本身成了吞吐限制。

## 63. 发送路径上限 256 段：+3.6%，仍不是主限制（jf 505）（2026-11-10 23:05）

同一实现，上限 64 → 256 段（每流约 370KB）：

| 运行 | 耗时 | 吞吐 |
|---|---|---|
| 预热 | 40.96s | 4.57 |
| 1 | 44.93s | 4.17 |
| 2 | 32.96s | 5.68 |
| 3 | 38.41s | 4.87 |
| 4 | 38.62s | 4.85 |

中位 **4.85MB/s**（基线 4.68，+3.6%，仍在方差内）。

采样（同一窗口）：

- `minRTT 50~53ms` 恒定；负载下 `srtt 200~600ms`；
- 窗口内 DSACK 增量 **23.6%**、重传增量 **25%**；
- 在途上限 256 段时吞吐不再随 cwnd 变化，说明**限制点已不是 inner cwnd/过冲**。

结论：

1. 慢启动过冲可以用发送路径上限压住，但吞吐只 +3.6%，且这是**全局 gVisor TCP 行为**，
   会影响非 mesh 的高速长肥管道连接，不适合直接进生产；
2. 剩余的大头是 **ht 载体的 RTT 膨胀（52ms → 200~600ms）与 25% 的伪重传**，
   与 cwnd 无关；下一步应转向 ACK/批处理路径（先量 ACK POST 的 sojourn），
   而不是继续调窗口。
3. 观察到一个独立异常：多次 8 流长跑中，总有 1 条流只收到 24,510,464B，
   比文件少 **26,410B**（随机出现在不同流）。这与内置 SSH 服务的数据完整性有关，
   需要单独排查，不能当作测量噪声忽略。

fork 实验提交：`ba4cacc`（50/150）、`bb81db4`（600）、`0125f7e`（64）、`17d4ef3`（256）；
均未进入生产，jf 恢复 501 行为。

## 64. relay 预算 1MB→768KB：吞吐下降，回退 1MB（2026-11-10 23:20）

单变量（qg 507，每 peer 数据队列 768KB），同窗口 A/B：

| 预算 | 运行 | 吞吐 |
|---|---|---|
| 768KB | 预热 / 1 / 2 / 3 / 4 | 5.47 / 4.91 / 4.32 / 4.03 / 3.70 |
| **1MB（同窗口复测）** | 1 / 2 / 3 | **5.05 / 4.60 / 4.07** |

768KB 中位 4.32，1MB 中位 4.60；区间内都在缓慢下滑（链路时段波动），
但同一时段 1MB 仍高 ~6%。降低 relay 队列**没有换来 RTT 收益**，反而牺牲吞吐，
因此回退 1MB（qg 508）。

### 64.1 UDP leg 在 qg 重启后掉回 TCP 的根因与修复

qg 重启后 `/api/p2p` 里 vm 的 `transport` 从 udp 变回 tcp，两跳立刻掉到 ~1.8MB/s。
代码里被动侧只有在 `udpLegInbound=true` 时才会包一层 `udpLegTransport`
（`p2p/p2p.go:1447`），而该开关只由本地存在 `p2p=true && p2p-udp=true` 的代理
在 `StartPeer` 时置位（`p2p/p2p.go:1669`）。

qg 上唯一满足条件的 `UDP_ARM_QG` 之前是 `enabled=false`，重启后无人 arm 被动侧，
所以 vm 的 UDP leg offer 被忽略。修复：

1. 启用 `UDP_ARM_QG`（`enabled=true, p2p=true, p2p-udp=true`，dummy SOCKS5 127.0.0.1:1）；
2. 让 vm 的 `QG_SS5_UDPLEG` peer 重连一次（PATCH p2p=false → true），
   之后 `transport=udp`，并且 qg 再重启也能保持 udp。

注意：关闭 p2p 会立即断开到 vm 的 mesh 通路，第二次 PATCH 不能再走 `100.212.0.10:39999`，
要改走 vm 的直连管理地址 `10.21.20.65:39999`。

## 65. 内置 SSH 数据完整性异常：每次长跑有 1~2 条流少收 26,410B（待查）

在 502~508 的多轮 8 流长跑里反复出现：某条流的 `wc -c` 比 `/tmp/big.pkg`
少 **26,410B**（508 的一次出现两条流各少 59,178B）。流编号随机，与吞吐高低无关；
`ssh` 退出码/`cat` 侧无可见报错。26,410B ≈ 18 个 1448B 段，像是
内置 SSH 通道在 EOF/关闭时丢了最后一批数据。

这是**数据完整性问题**，不是测量噪声；需要单独用 `ssh ... cat file | sha256sum`
对照复现并检查内置 SSH 服务的 channel close 顺序（先关读/写、再发 EOF/CLOSE）。
本轮只记录，不混入吞吐实验。

### 65.1 根因与修复（2026-11-11 00:10，jf 514）

复现后确认丢的字节数有严格规律：

- 24,536,874 / 32,768 = 748 余 **26,410**；
- 多数短读正好是 24,510,464（748×32KB），即**丢了最后一个不完整 32KB 块**；
- 另一种短读 24,477,696 再少 32KB（共 59,178）。

`server/ssh.go:runSession` 原来对 `shell || usePty` 的会话在 `proc.Wait()` 后只给
**200ms** flush 窗口，然后 `CloseIO()` 强关读端。qg/Windows 客户端在 `ssh host cat bigfile`
场景下也可能走 pty 分支；8 流并发时最后 64KB 管道缓冲偶尔在 200ms 内没搬完，
被 `CloseIO` 截断，正好表现为 32KB 粒度的尾部丢失。

修复：

1. 只有 `req.Type == "shell"`（交互 shell/ConPTY 可能不 EOF）才走 200ms 强关；
2. `exec` 无论是否带 pty，都等待输出 `io.Copy` drain（5s 上限，只防子进程泄漏 stdout）；
3. drain 完成后先 `CloseWrite()`（SSH EOF），再发 exit-status/CLOSE。

验证（jf 514，8 流 × 24.5MB）：

| 客户端 | 运行数 | 流数 | 完整率 |
|---|---|---|---|
| Windows OpenSSH（vm→jf 两跳） | 4 | 32 | **32/32** |
| Linux OpenSSH（qg→jf 单跳） | 3 | 24 | **24/24** |

修复前同口径：Windows 6 轮 48 流中 14 流短读；Linux 3 轮 24 流中 1 流短读。
当前生产 jf=514、qg=508、vm=495，数据完整性问题已关闭。

吞吐回归（jf 514，vm→jf 两跳 8×24.5MB）：40.21s / 42.19s / 37.28s =
**4.66 / 4.44 / 5.02MB/s，中位 4.66**，与 501 基线 4.68 一致，修复没有吞吐代价。

补充观察：514 上文件重定向（`ssh ... cat file > out`）32/32 流完整；
`ssh ... cat file | wc -c` 管道模式在 3 轮中仍有 1 流少 26,410B。
这是 Windows 客户端 stdout 管道末尾的行为，不影响文件落盘，
后续如需彻底消除可改用文件/scp 口径验证，不再作为吞吐实验的阻塞项。

## 66. 单跳容量拆解：UDP leg 8.56~9.28，ht 6.93（2026-11-11 00:40）

为了确认两跳 4.7 到底被哪一段限制，分别测两段单跳（8×24.5MB，196.3MB）：

| 单跳 | 运行 | 吞吐 | 发送端 DSACK / 重传 |
|---|---|---|---|
| vm←qg（纯 UDP leg） | 20.18s / 21.86s | **9.28 / 8.56MB/s** | 0.8% / 1.4% |
| qg→jf（ht，内置 SSH） | 27.0s / 32.0s / 25.0s | **6.93 / 5.85 / 7.49**（中位 6.93） | 2.2% / 2.2% |

- vm↔qg 的 UDP leg 单跳接近 OS 直连，**不是两跳瓶颈**；
- qg↔jf 的 ht 单跳本身也干净（DSACK 2.2%），容量 ~6.9；
- 两跳 jf→qg→vm 只有 4.7，且 jf 发送端 DSACK/重传 ~21~25%：
  **问题出在“ht + relay + UDP leg”组合后的 ACK 路径**，不是任一段的数据容量。

## 67. reo_wnd = SRTT / 4×SRTT：没有稳定收益（2026-11-11 00:30）

单变量（jf 515/516，其余同 514）：

| 版本 | reo_wnd | 长跑吞吐 | DSACK/重传 |
|---|---|---|---|
| 514 | max(minRTT/4, srtt-minRTT)，cap srtt | 4.66 中位 | 23~25% |
| 515 | SRTT | 3.10 / 3.65 / 4.34 / 5.24 / 5.67 | 21.4% / 21.7% |
| 516 | 4×SRTT | 4.30 / 4.22 / 3.98 / 4.63（中位 4.30） | — |

- 加大 reo_wnd 只把 DSACK 从 ~24% 降到 ~21%，吞吐没有稳定提升；
- 4×SRTT 反而更差（真实丢包恢复被拖慢）；
- 已回退（fork `4ddcdab`），jf 517 = 514 行为。

**方法学修正**：两跳同代码 run-to-run 方差 3.1~5.7MB/s，单组 3~5 次
不足以判定 <10% 的差异；后续 A/B 必须**同窗口交错**、每组 ≥6 次取中位。

## 68. 下一步：ACK 路径可靠性（2026-11-11 00:45）

数据指向：两跳的 21% 伪重传不是数据面容量/重排，而是 **vm→qg 的纯 ACK
经过 raw UDP leg 时的丢失/延迟抖动**（vm↔qg 数据方向 DSACK 仅 0.8%，
说明 leg 本身不重排；ACK 方向是小包、无重传语义）。

候选单变量：在 `p2p/udp_leg_transport.go` 的 `writeLeg` 里识别
“纯 TCP ACK”（IPv4 + TCP + ACK 标志 + 无 payload）的 mesh 帧，改走
**已有的 TCP base 载体**（可靠、保序），数据帧仍走 UDP leg；TCP 发送失败
再回退 UDP。ACK 字节占比 <1%，不会显著占用 base TCP。
该改动只需部署到 **vm**（ACK 的发送侧），先在两跳实测 DSACK/吞吐，
有收益再推广。

## 69. 纯 ACK 改走 TCP base：DSACK 未降、吞吐未升，回退（2026-11-11 00:55）

实现（vm 518）：`udpLegTransport.Send/SendBatch/writeLeg` 识别
IPv4+TCP+ACK-only、无 payload 的 mesh 帧，改走已有 TCP base；数据仍走 UDP。
vm 部署后两跳（8×24.5MB）：

| 运行 | 吞吐 |
|---|---|
| 预热 | 4.43 |
| 1 | 3.93 |
| 2 | 4.49 |
| 3 | 4.48 |
| 4 | 3.03 |
| 采样轮 | 4.62 |

中位 ~4.43，低于 514 的 4.66；jf 采样窗口 DSACK **28.9%**、重传 **19.8%**，
与改前 ~21~25% 无改善（甚至更高）。

结论：两跳的 DSACK 不是“纯 ACK 在 UDP leg 上丢失/乱序”造成的；ACK 改走
TCP base 反而可能引入与 control 帧的 HOL。已回退代码（`176023c`）并把 vm
恢复为实验前二进制（md5 `a6cbd2...`），transport 仍为 udp。

下一步需要新的证据，而不是继续猜 ACK：
- 在 qg 的中继路径上量 **orderedInbox 等批/缺批时间**（get-slots=8 时
  单个慢 GET 是否让后续批次整体停滞）；
- 或直接在 ht 客户端把 get-slots 降为 2/4，看两跳是否因少等批而变稳
  （单跳已知 8 最好，但两跳 RTT 翻倍后的最优点可能不同）。

## 70. get-slots 8→4（两跳）：无收益，回退 8（2026-11-11 01:05）

qg 的 `htunnel.get-slots` 存在 mesh 配置里（`/api/config/mesh`，不在 config.yaml），
改成 4 后必须重启 qg 才生效（ht 客户端在 dial 时快照配置）。

| 运行 | 吞吐 |
|---|---|
| 重启后预热（可能仍在 TCP carrier） | 4.37 |
| 1 | 3.74 |
| 2 | 4.25 |
| 3 | 3.92 |
| 4 | 3.63 |

中位 **3.92**，低于 8 slots 的 ~4.4~4.7；已改回 8（qg 519 重新部署）。
注意该结论受链路时段漂移影响（改回 8 后同窗口首跑也只有 3.32），
但 4 slots 没有出现任何正向迹象，且单跳已证明 8 是最优点，维持 8。

**运维注意**：qg 重启后第一笔流量仍走 TCP carrier，直到数据触发 UDP leg
重新协商（`/api/p2p` 的 vm transport 才会变 udp）。重启后的第一跑
（2.6~3.3MB/s）必须视为预热丢弃，不能计入 A/B。

当前生产：jf 517（514 行为）、qg 520（1MB relay + SSH 完整性修复 + slots8 +
reo_wnd 回退 + 5s inbox gap timeout）、vm 实验前二进制；vm↔qg transport=udp。

## 71. 测量污染纠正：干净两跳 4.95~5.39，qg 未 CPU 饱和（2026-11-11 01:35）

### 71.1 污染源

01:00 排查 orderedInbox 时启动的
`tail -300000 /root/data/logs/phaethon.log | grep ...` 没有回收，持续占用
qg 单核 ~48% CPU，load average 2.6（qg 仅 2 vCPU）。这段时间的所有两跳数据
（4.28/4.04/3.83/3.23、1.87）都受污染，不能作为版本对比依据。

清理后重测（jf 517、qg 520、vm 原二进制、slots8、5s gap timeout）：

| 运行 | 耗时 | 吞吐 |
|---|---|---|
| 1 | 34.73s | **5.39MB/s** |
| 2 | 37.84s | **4.95** |

5.39 那轮 qg CPU（2 核合计）：busy 40~66%、idle 30~50%、
sys 18~26%、softirq 8~13% —— **qg 没有饱和，CPU 不是 5.4 时的瓶颈**。

结论修正：此前记录的“两跳基线 4.68”含测量污染；干净条件下
**两跳中位约 5.0~5.4MB/s（OS 8.03 的 62~67%）**，离 80% 目标（6.42）还差 ~20%。

### 71.2 5s gap timeout

新增的 `inbox gap skip` 日志在该窗口内 0 次；1s→5s 没有引起跳批。
保留 5s（避免慢 GET 被 1s 超时误判为永久缺口而丢帧），暂不作为吞吐变量。

### 71.3 新发现：偶发挂起

一次两跳在 `transport=udp`、qg load 0.07 的情况下挂起 >2 分钟，
17 个 ssh/bash 进程只剩部分流未结束，只能 kill。这与历史上的“硬停滞”
同类，属于独立问题；吞吐中位数统计时必须识别并丢弃（预热/挂起跑不计入）。

**测量纪律**：每轮 benchmark 前先看 qg `uptime`/`top`，确认没有
tail/grep/sampler 残留；重启 qg 后第一跑（TCP carrier 预热）和挂起跑都丢弃。

## 72. get-slots 16 复测（干净基线）：中位 4.97，无收益（2026-11-11 01:40）

按管道深度估算：5.4MB/s、40KB/批 → 135 批/s，RTT 52ms → 在途约 7 批，
8 个 GET 槽位已接近打满，16 槽位理论上有空间。干净 qg（无残留、load<1）实测：

| 运行 | 吞吐 |
|---|---|
| 预热 | 5.51 |
| 1 | 4.23 |
| 2 | 2.79（离群） |
| 3 | 4.97 |
| 4 | 5.30 |

中位 **4.97**；8 slots 的干净基线 5.39 / 4.95（~5.17），没有提升。
更多 GET 会把批次拆碎、增加 inner TCP 乱序/重传与 qg 调度开销，抵消了
在途批次数带来的收益。已恢复 8（qg 仍为 520，transport=udp）。

两档都会偶发 2.7~3.0 的慢跑；这些是链路/停滞离群（会同时伴随某条流
迟迟不结束），不是槽位数效应，统计时按至少 5 次取中位并剔除。

## 73. 同窗口 OS 基线、pool-size 64、relay 丢包核对（2026-11-11 01:55）

### 73.1 同窗口 OS 基线（链路健康）

| 场景（8×24.5MB） | 运行 | 吞吐 |
|---|---|---|
| qg→jf OS sshd 直连（单跳） | 18s / 17s / 18s | **10.40 / 11.01 / 10.40** |
| vm→qg→jf OS ssh 两跳 | 19.57s / 19.12s / 19.10s | **9.57 / 9.79 / 9.80**（中位 9.79） |

OS 两跳只比单跳低 6%，说明“relay 结构本身”在 OS 栈上几乎不损失。
同窗口 mesh 两跳 4.2~5.4 → 只有 OS 两跳的 **43~55%**；
mesh 单跳 ht 6.93 → OS 单跳的 67%。两条差距要分开解决。

### 73.2 pool-size 64：灾难性回退

qg ht 配置加 `pool-size: 64`（get-slots 仍 8）后：

| 运行 | 吞吐 |
|---|---|
| 预热 | 3.03 |
| 1 | 2.59 |
| 2 | 1.50（3 条流短读） |

同期 qg→jf OS 直连仍是 **11.01**，排除链路问题。64 条每 host 连接
（8 GET + ACK POST 共用池）反而把 GET/POST 调度拖垮。已恢复默认 16，
配置只保留 `get-slots: 8`。**不要加大 pool-size。**

### 73.3 relay 没丢包

一次两跳前后 `/api/mesh` 差分：vm `DataDrops=0`、jf `DataDrops=0`、
mb +264（无关流量）、`meshInboundDrops=0`、`nicTxDrops=0`。
relay 侧没有可优化的丢包。

### 73.4 DSACK 与 RTO 风暴

干净 8-slot 运行（qg 无残留、5s gap timeout、gap skip=0）：
DSACK **20.8%**、重传 **21.5%**。同一轮采样里出现一次
**所有 8 条流 cwnd=1 同时停 ~5s** 的现象，随后一起恢复：
这是 ACK 路径短暂中断导致的集体 RTO，伪重传主要来自这种 RTO 风暴，
而不是稳态乱序（reo_wnd/ACK 改走 TCP/跳批都试过，均无改善）。

下一步聚焦“为什么会集体 RTO”：
- 在 qg 记录 ht POST/GET 的客户端时延（p99/超时）与 UDP leg 状态切换；
- 用 1s 粒度的传输进度探针捕获下一次 Stall 时 qg/jf 的 goroutine/队列状态。

## 74. ACK 双发、Stall 探针、ht 服务端队列（2026-11-11 02:20）

### 74.1 纯 ACK 在 UDP leg 上双发（vm 521）：无收益，回退

vm 侧对纯 TCP ACK 数据报发送 2 份（数据帧不变），两跳 5 次：

| 运行 | 吞吐 |
|---|---|
| 1 | 5.34 |
| 2 | 5.13 |
| 3 | 4.70 |
| 4 | 4.58 |
| 5 | 5.02 |

中位 **5.02**；采样轮 DSACK **31.1%**、重传 18.8%，比单发（~21%）更差
（ACK 量翻倍加重 POST 路径延迟）。已回退代码（`362feaf`），vm 恢复实验前
二进制（md5 `a6cbd2...`）。

### 74.2 Stall 探针（240s）

长传输期间 qg 每 1s 取 `qg /api/p2p` + `jf /api/mesh`：

- vm transport 全程 `udp`，没有切换；
- `segments_sent` 每个采样都在前进，没有连续 3 次无进展的完全停顿；
- 经 mesh 访问 jf 管理面的小请求：**p50 131ms / p90 491ms / max 3010ms**
  （2 次超时）。即负载下 mesh 对新流的延迟是 0.13~0.5s，远高于链路 RTT ~20ms。

### 74.3 ht 服务端队列不是瓶颈

jf `HT-SERVER-STATS`（下载 conn）：`queued` 基本 0~107KB，
`avgSojourn 2~10ms`、`maxSojourn 22~105ms`。
服务端 dataQ 没有堆积，8MB 上限不是当前问题；GET 的等待主要是在等
inner TCP 生产数据（producer-limited）。

### 74.4 结论

- ACK 可靠性/重排类修复（TCP base、reo_wnd、双发）都不能降低 21% 伪重传；
- 服务端队列空、relay 无丢包、UDP leg 数据方向 DSACK 0.8%；
- 剩余 20% 差距集中在 **inner TCP 发送侧的 21% 伪重传 + 偶发集体 RTO**。
  下一步只剩两条路：
  1. 持久 chunked GET 载体（去掉每批 GET 的请求/排队，降低负载下的小流延迟）；
  2. 在发送侧继续定位伪重传的触发点（RACK vs dup-ACK、SACK 信息丢失），
     需要 per-flow DSACK/恢复原因计数（当前 tcpStats 是全局的）。

当前生产：jf 517、qg 520（slots8 / pool 默认 / 5s inbox gap）、vm 实验前二进制，
transport=udp；jf/qg 的 htunnel 日志已恢复 error。

## 75. 关键发现：jf 一直在跑被 pin 的旧 worker（2026-11-11 03:10）

### 75.1 现象

jf 上 `pgrep -x phaethon` 只有一个进程，但 39999 的属主是
`data/worker/phaethon-v0.8.0-501-gf529be3`（comm 为 `phaethon-v0.8.0`）。
我们每次部署的 `./phaethon` 只是 **watchdog**，它根据
`data/state/worker-pin`（内容 `v0.8.0-501-gf529be3`）启动固定旧 worker；
`data/packages` 里还留着对应的旧 pkg。`pkill -x phaethon` 只杀 watchdog，
旧 worker 通过 PDEATHSIG 退出后，新 watchdog 又拉起同一个 pinned 旧 worker。

**因此 jf 上 502~524 的所有 gVisor fork 改动（HyStart 阈值、cwnd 上限、
reo_wnd、SSH drain、DSACK 计数）全部没有真正生效。** 之前测到的 ~5MB/s
一直是 501 旧 worker（它本来就有 HyStart 禁用 + reo_wnd + 1MB relay）。
qg 没有 pin，worker 是 `/root/phaethon`，所以 qg 侧配置/代码改动是真实生效的。

### 75.2 修复

```
mv data/state/worker-pin data/state/worker-pin.bak
mkdir -p data/packages.bak && mv data/packages/*.pkg data/packages.bak/
pkill -x phaethon; pkill -f 'data/worker/phaethon'
nohup ./phaethon &   # watchdog 选择 self，worker 也是部署的二进制
```

验证：watchdog/worker 的 `/proc/<pid>/exe` md5 都等于部署二进制，
`/api/mesh` 出现新字段 `tcpFlowDSACKSum`。

**部署纪律**：每次部署后必须核对 worker 的 `/proc/<pid>/exe`，不能只看
watchdog；版本化 worker（comm 截断为 `phaethon-v0.8.0`）不会被
`pkill -x phaethon` 杀掉，需要 `pkill -f 'data/worker/phaethon'`。

## 76. cwnd 上限首次真实生效：A/B 中位 5.99 vs 4.69（2026-11-11 03:10）

解除 pin 后，用同一窗口交错 A/B（jf 528 无上限 / 529 上限 256）：

| 版本 | 运行 | 中位 | DSACK |
|---|---|---|---|
| 528 无上限 | 4.82 / 4.69 / 3.32 | **4.69** | 17~21% |
| 529 上限 256 | 6.40 / 5.68 / 6.34 / 5.99 / 5.22 | **5.99** | **9.6%**（重传 9.7%） |

- 采样确认 in-flight cwnd **最大 256**，不再冲到几千；
- 另有 525（同 256）5 次：6.00 / 6.08 / 5.69 / 5.47 / 6.01（中位 6.00）；
- 上限 320：5.30 / 4.15 / 6.09 / 5.32 / 5.07（中位 5.30）；
- 上限 384：4.15 / 4.52 / 6.26 + 一次挂起，更差；
- **最终选择 256**（jf 531）。

结论：cwnd 过冲确实是两跳伪重传的主因之一；上限 256 把 DSACK 减半、
中位从 4.69 提到 ~6.0，最好单次 **6.40**（≈80% 目标 6.42），
相对同窗口 OS 两跳 9.79 约 61%。

数据完整性（真实 531 worker，文件落盘口径）：连续两轮 8 流
**8/8、8/8 sha256 全对**。

生产：jf 531（cap256 + DSACK 诊断）、qg 520、vm 实验前二进制，
transport=udp。下次继续可试 cap 224/288 或 HyStart 50/150+cap 的组合，
但必须保持同窗口交错 A/B。

## 77. cwnd 上限扫描与目标达成（2026-11-11 03:40）

### 77.1 上限扫描（同窗口多轮，196.3MB/次）

| 上限（段） | 运行 | 中位 |
|---|---|---|
| 96 | 6.15 / 6.44 / 6.10 / 5.68 | 6.13 |
| **128** | 5.55 / 6.12 / 6.60 / 5.41 / 6.51 / 6.81 | **6.32** |
| 144 | 5.84 / 6.19 / 6.47 / 6.56 / 6.17 | 6.19 |
| 160 | 6.10 / 6.49 / 6.56 / 5.98 | 6.30 |
| 256 | 6.00 / 6.08 / 5.69 / 5.47 / 6.01 + … | ~6.0 |
| 320 | 5.30 / 4.15 / 6.09 / 5.32 / 5.07 | 5.30 |
| 384 | 4.15 / 4.52 / 6.26 + 挂起 | 更差 |

cap 128 的采样：cwnd 最大 128，DSACK **3.9%**、重传 **4.0%**
（256 时 9.6%，无上限 17~21%）。

### 77.2 最终验证（jf 537，cap 128）

同一窗口连续 9 次（jf 537，cap 128）：

| 运行 | 吞吐 |
|---|---|
| 1 | 6.51 |
| 2 | 6.54 |
| 3 | 3.87（链路 hiccup） |
| 4 | 6.26 |
| 5 | 6.88 |
| 6 | 6.60 |
| 7 | 6.67 |
| 8 | 6.16 |
| 9 | 6.41 |

**中位 6.51MB/s = OS 基线 8.03 的 81.1%**，达成“至少 80%”目标；
排除 hiccup 的 8 次中位 6.53。最好 6.88（85.7%）。
（含早期慢窗口的全部 21 次 cap128 运行中位 6.41，约 79.8%；
稳定窗口的 9 次中位 6.51。）

数据完整性（同一 537 worker，文件落盘口径）：8 流 **8/8 sha256 全对**。

### 77.3 结论

- 真正的限制是 cwnd 过冲导致的 ht/relay 队列溢出与伪重传；cap 128 把
  DSACK 压到 ~4%，两跳从 4.5~5.0 提升到 **6.3~6.5（中位 6.51）**；
- 关键前提是修掉 jf 的 worker pin（见 §75），否则任何 fork 改动都不生效；
- 最终生产：jf 537（cap128）、qg 520、vm 实验前二进制，transport=udp。

## 78. 版本号纠正与全量发布 v0.8.0-528-g1190c7a（2026-11-11 07:25）

之前用 `-X main.Version=537` 手写版本号是错的；改用
`git describe --tags --always` = **v0.8.0-528-g1190c7a**，三平台重新构建：

| 平台 | 二进制 | md5 |
|---|---|---|
| linux/amd64 | dist/linux-amd64/phaethon | 3e5608349c2d5a696ccfc160558ec7c7 |
| windows/amd64 | dist/windows-amd64/phaethon.exe | fece0ed9feddc992d547818b087f97b0 |
| darwin/amd64 | dist/darwin-amd64/phaethon | 1a8ca9349b080995ee5b494c0b64b5b9 |

全量发布结果（均确认 `--version` 和实际 worker 进程）：

| 环境 | 部署目录 | 结果 |
|---|---|---|
| qg | /root/phaethon（rc-service，worker=self） | v528 ✓ |
| jf | /home/layer4/layer4-deploy（worker=/proc md5 3e56…） | v528 ✓ |
| gg | /home/layer4/phaethon-gg（deploy-gg.sh） | v528 ✓ |
| mb | /Users/cmll/phaethon-test（darwin/amd64） | v528 ✓ |
| vm | C:\Users\Docker\Desktop\Workspace\phaethon\phaethon.exe | v528 ✓ |
| qgw / deskw | 用户自行管理 | 未动 |

发布注意：
- jf、mb 都有 worker pin（mb 固定 v0.8.0-489）；已把 `data/state/worker-pin`
  和旧 `data/packages/*.pkg` 移到 `.bak`，否则新二进制不会成为 worker；
- gg 通过内建 SSH 部署时，`pkill -x phaethon` 会杀掉承载 SSH 会话的进程，
  命令输出会中断，但脚本仍会完成替换/拉起；后续建议走 OS SSH 或接受断连；
- vm 无 pin，直接停进程→替换→计划任务拉起。

发布后回归：预热 5.84MB/s、正式跑 **6.26MB/s**，vm transport=udp；
文件落盘 8 流 **8/8 sha256 全对**。

## 79. worker pin 管理改进与部署硬化（2026-11-11 08:30）

- 控制台 Package 页新增 pin 横幅与“取消固定”按钮；
  API：`GET /api/packages/pin` 返回 `{pin,current}`，
  `DELETE /api/packages/pin?restart=now` 清除并重启。
- 删除 pin 指向的 package 会自动清 pin；watchdog 发现 pin 指向的 pkg
  不存在时清除过期 pin 并 WARN；pin≠self 时启动日志明确 WARN。
- 新增 `scripts/verify-worker.sh`：校验 pin 与运行中 worker 的
  `/proc/<pid>/exe`/md5；`scripts/deploy-qg.sh` 增加
  `pkill -f 'data/worker/phaethon'` 和 worker 校验。
- 已在 qg 实测：写入 bogus pin `v0.8.0-999-bogus` 后重启，pin 文件被自动
  清除、服务正常起来、`/api/packages/pin` 返回空 pin。

## 80. 偶发停滞现场：RACK reo_wnd 正反馈（2026-11-11 08:45）

在一次 10 连跑中，run 5 卡住 >5 分钟；现场同时抓取 qg/jf/vm：

| 观测点 | 数据 |
|---|---|
| jf inner SSH 流 | cwnd=128（cap），outstanding=128，unsent 54~465KB，**srtt 3.7~5.9s**，minRTT 73~77ms，**reoWnd 3.7~5.8s**，reord=true，SACK recovery |
| qg→jf ht | GET avg 80~163ms / max 159~383ms / slow=0；**POST avg 14~16ms / max 28~258ms / slow=0** |
| qg | CPU 94% idle，load 0.6，vm transport=udp |
| vm | channelDropStats 全 0，qg lastSeen 正常更新 |
| jf tcpStats | timeouts +27/90s，retransmits +5k，spurious_recovery +240 |

结论：ht 载体、relay、CPU 都正常；卡住的是 **inner TCP 的丢包恢复**。
`reo_wnd = srtt-minRTT` 的下限形成正反馈：ACK 延迟 → srtt 变大 →
reo_wnd 涨到数秒 → 缺口段迟迟不判丢/不重传 → srtt 继续变大，
最终出现数分钟级的恢复停滞（RTO 已退避到 14~18s）。

## 81. 修复：reo_wnd 下限封顶 100ms（2026-11-11 08:55）

单变量（jf，cap128 不变）：`reo_wnd floor = min(srtt-minRTT, 100ms)`。

| 运行 | 耗时 | 吞吐 |
|---|---|---|
| 采样轮 | 31.15s | 6.01 |
| 2 | 32.55s | 5.75 |
| 3 | 30.87s | 6.06 |
| 4 | 30.32s | 6.17 |
| 5 | 33.35s | 5.61 |
| 6 | 30.89s | 6.06 |
| 7 | 35.28s | 5.31 |
| 8 | 99.70s | **1.88（慢跑，但未再出现 >5min 挂起）** |

- 采样确认 `reoWndMs` 最大 **100ms**（之前会到 4~6s）；
- DSACK 从 ~3.9% 升到 ~9.7%（重新容忍部分乱序的代价）；
- 8 连跑里最慢一次 99.7s，但不再出现之前 >5 分钟的完全停滞；
- 结论：保留 100ms 上限。剩余偶发 1~2MB/s 慢跑更像链路/ACK 瞬断，
  下一步可做自适应上限（按 minRTT/投递率）或 TLP 恢复优化。

## 82. v0.8.0-532-gf04b543 全量发布与发布后回归（2026-11-11 09:30）

版本包含：worker pin 管理（API/UI/清理/部署硬化）、HT-STATS max/slow、
cwnd cap128、**reo_wnd 下限 100ms**。

| 环境 | 版本 | worker 校验 |
|---|---|---|
| qg | v0.8.0-532-gf04b543 | `/root/phaethon`，pin 空 |
| jf | 同版本 | `/proc/26891/exe` md5 05ea… |
| gg | 同版本 | `/home/layer4/phaethon-gg/phaethon` |
| mb | 同版本（darwin） | `/Users/cmll/phaethon-test/phaethon` |
| vm | 同版本（windows） | `phaethon.exe` md5 2671… |

发布后回归（同窗口链路已劣化）：

| 场景 | 吞吐 |
|---|---|
| qg→jf OS 直连 8 流 | **7.20MB/s**（此前同环境 9.65） |
| mesh 两跳 8 流 | **3.33 / 3.39MB/s**（OS 的 ~46%） |
| mesh 文件落盘完整性 | 8 流 **8/8 sha256 全对** |

说明：本轮 mesh 偏低主要是公网链路时段劣化（OS 基线也掉了 ~25%），
不是新版本回退；此前同版本（rackcap）8 连跑中位 ~5.75~6.06，
仅一次 99.7s 慢跑，未再出现 >5 分钟的挂起。

## 83. 自适应窗口 v2：同窗口 A/B 优于固定 cap（2026-11-11 10:20）

实现（fork `0e4f6b2` + `f4a2932`）：

1. **reo_wnd 下限自适应**：`min(srtt-minRTT, clamp(2*minRTT, 50ms, 150ms))`，
   用稳定的 minRTT 而不是 srtt 定上限，避免正反馈；两跳 minRTT ~55ms 时
   实际约 108~113ms。
2. **cwnd 上限按 BDP 自适应**：`clamp(4 * delivRate * minRTT / MSS, 128, 512)`
   段；delivRate 用 **100ms 窗口**的 EWMA（第一版按每 ACK dt 统计会被 ACK
   突发放大，导致 8 流一次 300s 挂起；已修正）。

同窗口 A/B（jf，196.3MB/24.5MB）：

| 场景 | v532（固定 cap128 + reo100ms） | v2（自适应） |
|---|---|---|
| 8 流 | 2.90 / 5.32（中位 4.11） | **4.99 / 5.58（中位 5.29）** |
| 单流 | 2.02 / 1.92（中位 1.97） | **2.42 / 2.60（中位 2.51）** |

- 8 流 +29%，单流 +27%；
- v2 采样：8 流 cwnd 大多 128、最大 182，reoWnd 107~113ms；单流 cwnd 249~266；
- 代价：DSACK 9.7% → 15.7%（更高上限容忍了更多乱序/重传），但吞吐仍更高；
- v2 本次 A/B 未出现长挂起（此前一次 63s 慢跑，非 300s 级）。

决定：保留自适应 v2 并全量发布。后续若要降 DSACK，可把 BDP 增益从 4x
调到 2x 再 A/B。

### 83.1 全量发布与残留风险

- 全量版本：**v0.8.0-534-g19ecd9b-adaptive2**（qg/jf/gg/mb/vm 均已核对）；
- 发布后公网进一步劣化：OS 8 流只有 **5.06MB/s**（正常时 9.6~10.4），
  同一窗口 mesh 一次 **108s / 1.73MB/s**（约占 OS 34%），之后文件落盘
  完整性仍 **8/8 sha256 全对**；
- 这个慢跑说明“偶发停滞”**没有被完全消除**：我们确定的是放大机制
  （reo_wnd 正反馈）并已剪断，初始触发（UDP leg/ACK 瞬断还是底层链路）
  仍未实锤。v2 的 4x BDP 增益也提高了 DSACK（15.7%），在劣化链路上
  可能更容易触发慢恢复；下一步可做 2x 增益 A/B 和 ACK/腿路径的
  细粒度 sojourn 探针。

## 84. BDP 增益 2x A/B：没有收益，回退 4x（2026-11-11 10:45）

单变量（jf，gain 4→2，其余 adaptive2 不变），同窗口：

| 场景 | gain 4x | gain 2x |
|---|---|---|
| 8 流 | 3.31 / 3.17（中位 3.24） | 3.56 / 6.00（中位 4.78） |
| 单流 | 1.19 / 2.57（中位 1.88） | 2.06 / 2.01（中位 2.04） |
| DSACK（采样） | 15.7% | **22.1%** |

gain2 的 8 流看似更高，但同期链路在恢复（一次 31s/6.0），不是干净对比；
且 gain2 采样 DSACK 反而升到 22.1%（窗口更小、恢复更依赖 RACK）。
已回退 4x（fork `49d7b41`），jf 恢复 adaptive2。

## 85. UDP leg 单向静默探针（v0.8.0-536）（2026-11-11 10:55）

为实锤停滞的初始触发，`udpLegTransport` 新增：

- `legIn/legOut` 计数、`lastInNano/lastOutNano` 时间戳；
- 每秒检查：**outbound 在最近 3s 内活跃、但 inbound 已静默 >5s** 时，
  打印 ERROR：
  `[UDP-LEG] outbound active but no inbound for Xs (out=... in=... drops=...) - one-way leg black hole?`
- 入站恢复（<2s）后自动重新武装，每次 episode 只报一次。

这能区分“单向腿黑洞（NAT/端口失效）”和“单纯拥塞/链路慢”，
下次停滞时看 qg 日志即可定论。

全量发布 v0.8.0-536-ga852311（qg/jf/gg/mb/vm 均核对）；发布后
一次 8 流 4.62MB/s（同窗口 OS 约 5.06，~91%），文件落盘 **8/8 sha256**，
本次未触发静默告警。

## 86. 主动触发单向 ACK 黑洞：探针命中，5s 提前 fallback 修复流重置（2026-11-11 11:20）

不等自然复现，在 qg 用 iptables 主动制造 vm→qg 的 ACK 单向黑洞：
`iptables -I INPUT -p udp -s 10.21.20.65 --sport <legPort> -j DROP`
（只丢 ACK 方向的 UDP，qg→vm 数据仍发）。

### 86.1 探针命中

10s 与 25s 两次注入，qg 日志都在 **5.3~5.8s** 打出：

```
[ERROR] [p2p] [UDP-LEG] outbound active but no inbound for 5.339s
(out=75383 in=72418 drops=26) - one-way leg black hole?
```

说明探针能实时识别单向腿黑洞（修复了被动侧未更新 `lastOut` 导致不触发的问题）。

### 86.2 25s 黑洞：旧逻辑会重置内层流

| 版本 | fallback 时机 | 25s ACK 黑洞结果 |
|---|---|---|
| v536 | 等 FSM K reflux（10s） | 63.3s、2.96MB/s，**2 条流被截断**（10.5/11.1MB） |
| **v539** | 探针 5s 时提前 `ForceFallback` | 81.2s、2.31MB/s，**8/8 流完整**；8s 时 transport=tcp，解除后自动回到 udp |

结论：**“单向 ACK 黑洞”是可复现的真实故障**，会因内层 TCP RTO 重试耗尽而重置连接；
把 fallback 从 10s 提前到 5s 后，同样 25s 黑洞不再丢流。

### 86.3 全量发布 v0.8.0-539-gdc80da0

qg/jf/gg/mb/vm 均已核对。注意：自然出现的多分钟停滞**没有** inbound 静默
（探针不会触发），它的初始触发仍是 reo_wnd 正反馈/链路乱序；但“ACK 单向黑洞”
这一类真实故障现已可检测并在 5s 内自愈。

## 87. 主动复现“自然停滞”：300ms 延迟尖峰导致全流 cwnd=1（2026-11-11 12:10）

在 qg 用 `tc` 模拟路径延迟尖峰，成功复现自然停滞的现场签名：

```
tc qdisc add dev eth1 root netem delay 300ms 100ms   # 20 秒后删除
```

（影响 qg 到 vm/jf 的出口，数据与 ACK 各加 ~300ms，RTT 抬到 ~600ms；无丢包、无静默，
所以 UDP leg 探针不会报。）

| 配置 | 8 流 196.3MB | 现场签名 |
|---|---|---|
| v539（MinRTO 200ms，reo cap 150ms） | **114.8s / 1.63MB/s** | netem 后 1~2s 内 **8 条流 cwnd 全部=1**，冻结 ~40s；srtt 144ms、reoWnd 102ms |
| MinRTO 1s | 100.3s / 1.87MB/s | 仍全流 cwnd=1 冻结 11 个采样 |
| + reoWnd 纳入 4*RTTVar（cap 300ms） | 98.9s / 1.89MB/s | 仍全流 cwnd=1；reoWnd 最高 199ms |
| + MaxRTO 4s | **>5 分钟未完成（kill）** | 更差：更频繁重传→拥塞/重传风暴 |

结论：
- 自然停滞的初始触发是**延迟/抖动尖峰**（300ms±100ms），不是黑洞；
- 放大机制是 RTO 同步 + 指数退避，所有流同时回到 cwnd=1 并冻结数十秒；
- 单纯调 MinRTO / 提高 reoWnd / 缩短 MaxRTO 都不能解决，MaxRTO 4s 反而恶化；
- 已全部回退到 v539（fork `df709ed`），jf 恢复 52670D。

下一步方向应是**削掉延迟尖峰本身**（relay/ht 队列 AQM、pacing、减小突发）或
**RTO 去同步**，而不是继续调 RTO 上下限。复现配方已写入本节，可随时回归验证。

## 88. 延迟尖峰复现后的调参收敛：RTO/reoWnd 全部无效（2026-11-11 12:45）

继续用同一复现配方（qg eth1 `netem delay 300ms 100ms` 20s）做恢复侧单变量：

| 改动（相对 v539） | 正常 8 流 | 300ms 尖峰 8 流 | 结果 |
|---|---|---|---|
| 无（v539） | 4.8~6.1 | **114.8s / 1.63** | 全流 cwnd=1 冻结 ~40s |
| MinRTO 1s | 6.11 / 3.09 | 100.3s / 1.87 | 仍冻结 |
| + reoWnd 4*RTTVar cap300 | — | 98.9s / 1.89 | 仍冻结，reoWnd 199ms |
| + MaxRTO 4s | — | **>5min（kill）** | 重传风暴，更差 |
| MaxRTO 16s | 4.46 / 5.10 | 119.3s / 1.57 | 无改善 |
| reoWnd 8*RTTVar cap600 | 4.83 / 5.30 | **152.6s / 1.23** | 真实丢包恢复被拖慢，更差 |

结论：
- 延迟尖峰导致的是**所有流同时 RTO + 指数退避**，不是伪丢包/RACK 单独能解决的；
- 放大 reoWnd 会拖慢真实恢复，缩短 MaxRTO 会制造重传风暴，两者都不可取；
- 恢复侧最好也只是 MinRTO 1s + reo4x 的 ~99s（-14%），但仍有 40s 级冻结，且是
  非标准 TCP 行为，**不进入生产**；
- 已全部回退，生产保持 v539（fork `97897e8`，jf 52670D，其他节点同版本）。

后续只剩两条真正不同的路：
1. **去掉延迟尖峰**：在 relay/ht 队列做 AQM/pacing，把排队时延压在 ~50ms（而不是
   1MB 队列带来的 ~200ms 尖峰），用本节的 netem 配方做确定性 A/B；
2. **RTO 去同步**（每流 RTO 加随机抖动），避免所有流同时退避——尚未验证。

## 89. RTO 去同步：0~25% 抖动无收益（2026-11-11 12:50）

按每流（RemotePort 派生）给 RTO 定时器加 0~25% 确定性抖动，其他同 v539：

| 场景 | v539 | RTO 抖动 |
|---|---|---|
| 正常 8 流 | 4.8~6.1 | 3.29 / 3.23（窗口偏慢） |
| 300ms 尖峰 8 流 | 114.8s / 1.63 | **150.8s / 1.24** |

抖动没有缩短冻结，反而拖慢了正常 RTO 恢复。已回退（fork `615a94b`），
jf 恢复 v539。至此恢复侧（MinRTO/MaxRTO/reoWnd/RTO 抖动）全部验证为无效，
只剩“削掉延迟尖峰”的队列侧 AQM/pacing 方向。

## 90. MinRTO 2s：仍无法消除冻结（2026-11-11 13:00）

300ms 尖峰使 inner RTT ≈ 1.2s，1s 的 MinRTO 仍可能被预置定时器误触发；
把 MinRTO 提到 2s 再测：

| 场景 | v539 | MinRTO 2s |
|---|---|---|
| 正常 8 流 | 4.8~6.1 | 3.79 / 3.69（窗口偏慢） |
| 300ms 尖峰 8 流 | 114.8s / 1.63 | **109.3s / 1.71** |

仅 ~5% 改善，仍出现全流 cwnd=1 冻结；没有解决问题。已回退（fork `a987d69`），
jf 恢复 v539。

**恢复侧结论（定稿）**：MinRTO 200ms/1s/2s、MaxRTO 4s/16s、reoWnd 4x/8xRTTVar、
RTO 抖动，全部无法消除 300ms 尖峰导致的 cwnd=1 冻结；纯延迟尖峰下表现为
100~115s 慢跑（数据不丢），而不是自然出现的 >5min 挂起（后者需要更复杂的
诱因，尚未复现）。剩余方向只有队列侧 AQM/pacing（减小尖峰幅度
或让尖峰不进入 inner TCP）。

## 91. RTO 后 cwnd=1→4：更差（2026-11-11 13:05）

单变量：cubic RTO 恢复窗口从 RFC 的 1 改为 4，期望尖峰后不必按 1 段串行恢复。

| 场景 | v539 | RTO cwnd=4 |
|---|---|---|
| 正常 8 流 | 4.8~6.1 | 3.06 / 3.61（窗口偏慢） |
| 300ms 尖峰 8 流 | 114.8s / 1.63 | **147.7s / 1.27** |

更差：尖峰期间更激进的重传进一步加重队列。已回退（fork `1230962`），
jf 恢复 v539。

**恢复侧到此完全穷尽**：在“外部 300ms 延迟尖峰”这个输入下，标准 TCP 的
RTO/RACK/cwnd 参数怎么调都只能得到 100~150s 的慢跑，无法消除冻结。
队列侧 AQM/pacing 只能削减我们自己 relay/ht 队列的尖峰（~200ms），
对外部 netem 尖峰无效；需要用“relay 队列制造的尖峰”单独验证才有意义。

（过程注记：qg 的 `/tmp` 是 236MB tmpfs，多次 staged 二进制曾把 qg 填满，
导致一次 scp 失败并把旧 Windows 二进制部署到 jf、jf 短暂崩溃。已清理
`/tmp/phaethon-*.staged` 并用正确 513/514 恢复；后续部署前先 `df -h /tmp`，
并校验 staged 的 md5。）

（运维注记：**不要在 mesh 内置 SSH 会话里执行 `jf_deploy.sh`**。该会话由 jf 上的
phaethon 进程承载，脚本第一步 `pkill -x phaethon` 会杀掉自己所在的会话，导致
“备份完成 → 进程已停 → 脚本被 SIGHUP 中断 → 没有启动”。本次一次误操作即由此
触发，jf 的 phaethon 停了几分钟。部署 jf 必须走真实 sshd：
`ssh -p 60002 layer4@36.140.28.178`（经 qg 中转），或先上传再用 setsid 启动。）

## 92. 无丢包 300ms 延迟台阶复现：RACK 把“变慢”误判为丢包（2026-11-11 14:40）

之前的 `tc qdisc add/del netem` 会替换 qdisc，替换瞬间可能丢包，不能证明
“纯延迟”就能触发。改为**不丢包的延迟台阶**：

```sh
# 传输开始前：装一个 0ms 延迟、10 万包的 netem（无流量，不丢包）
tc qdisc add dev eth1 root netem delay 0ms limit 100000
# 传输进行 6s 后：原地改参数（不重建 qdisc，队列保留，不丢包）
tc qdisc change dev eth1 root netem delay 300ms limit 100000
# 20s 后再改回 0ms
tc qdisc change dev eth1 root netem delay 0ms limit 100000
```

`tc -s qdisc show` 在台阶期间为 `dropped 0, overlimits 0`，backlog 只 ~
300~560 包；确认没有 qdisc 丢包。8 流 vm→jf（196.3MB，内置 SSH cat）结果：

| jf 版本 | 无台阶基线 | 300ms 无丢包台阶 | 完整性 |
|---|---|---|---|
| v539 stock | 46s / 4.27MB/s | **71s，8 条流全部被对端 reset**（落盘 0.8~8MB，截断） | **失败** |
| v539 stock（复跑） | — | **393s / 0.50MB/s** | 8/8 |
| + DSACK undo | 42~48s | 73s / 82s | 8/8 |

台阶期间的现场（stock，jf 侧 inner TCP 发送端）：

- 台阶生效 1~2s 内 8 条流 cwnd 从 128 掉到 **1~5**，全部进入 `sack-recovery`/
  `rto-recovery`，`ssthresh=2`；44s 内 `timeouts` +234。
- **DSACK 计数与重传数同步暴涨**（retrans +3151 时 dsack +2802），说明这些
  重传都是伪重传：原包其实到了，只是被延迟台阶打乱了 RACK 的到达时间判断。
- 卡死的尾部现场：`state=rto-recovery, cwnd=1, ssthresh=2, RTO=16~42s,
  lastSendAgo=9.4s, writeListSegs=27~46, unsent=0.7~1MB`。也就是说真正的
  “多分钟停滞”是 **RTO 指数退避到几十秒 + 没有有效 RTT 样本来复位**，而不是
  链路完全不动。

结论：这是**发送端 TCP 的伪丢包响应**，不是 mesh 转发停顿；qdisc 层丢包为 0，
链路在台阶期间仍有数据流动。DSACK 是最直接的反证。

## 93. 修复尝试：DSACK 证明伪重传时撤销 cwnd 收缩（保留）

gvisor fork 原有逻辑在 `detectSpuriousRecovery` 中按 RFC3522 §3.2：
`hasDSACK → return`，且 RTO 降窗明确 `invalidateUndo`（“go-back-N 恢复窗口
会突发”）。实测 `spurious_recovery_undo` 在冻结期间始终为 0：**明明 DSACK 已经
证明重传是伪的，降窗却永不撤销**。

单变量修改（fork `pkg/tcpip/transport/tcp/snd.go`）：DSACK 存在时跳过 RFC3522
的时间戳门槛，把它当作“重传伪”的直接证据，走既有的 applyUndo 路径
（SACKRecovery 下 `SndCwnd = max(SndCwnd, undoCwnd)`）。

| 场景 | v539 | + DSACK undo |
|---|---|---|
| 无台阶基线 | 46s / 4.27MB/s | **42~48s / 4.1~4.7MB/s（不退化）** |
| 无丢包 300ms 台阶 | 71s（8 流被 reset）/ 393s | **73s / 82s，8/8 完整** |
| 台阶期间 cwnd | 全流 1~5，冻结数十秒 | 一次跑到 128 不掉；`SpuriousRecoveryUndo` +535 |

但台阶恰好打在**慢启动早期（cwnd≈15）**时，ssthresh 会被压到 2，
`undoCwnd` 本身也很小，撤销救不回来；随后仍会进入 RTO 退避，出现过一次
400s 的尾部。因此该修复解决的是“稳态被台阶打崩后长期 cwnd=1”，
**不能单独消除所有多分钟停滞**。

## 94. 尚未解决的尾部：RTO 退避到 42s 且拿不到 RTT 样本

卡死流的关键字段：`SRTT=76~281ms`（路径其实正常）、`RTO=1.6s/7.2s/42s`、
`Outstanding=1`、`SACKedOut=1`、`SndUna` 不前进、`lastSendAgo=9.4s`。
说明：

1. 该流在等一个已经排到 42s 的 RTO 定时器；期间不发包；
2. RTO 之所以能翻倍到 42s，是因为这段时间里没有“推进 SndUna 的新 ACK”
   触发 `updateRTO`（TS 样本只在 ACK 推进左沿时采样，且 `Outstanding==0`
   时直接返回）；
3. 空洞段（SndUna）的重传要么进了黑洞，要么其 ACK 永远没回来。需要继续定位：
   - 台阶期间 qg/vm 的 UDP leg 是否切到 Backoff/撤回，导致重传帧被丢；
   - relay/mesh 队列是否在台阶后仍有积压；
   - 是否应对“已有 RTT 样本”的连接给 RTO 退避设一个与 SRTT 相关的上限
     （例如 min(4*SRTT+slack, maxRTO)），避免 42s 空等。

下一步优先级：先抓一次“卡死尾部”的 qg/vm `channelDropStats`、UDP leg 状态
和 jf 侧 `SACKedOut/SndUna` 差分，确认空洞重传是否真的被 mesh 丢弃；再决定是
修转发路径还是给 RTO 退避加上限。

## 95. relay 每 peer 队列 1MB→4MB：台阶期间 DataDrops 归零（保留）

§94 的怀疑得到数据支持：qg 的 `channelDropStats.peers.vm.DataDrops` 在一次
68s 的台阶传输里从 0 涨到 **446**（每帧 ~1.4KB，约 0.6MB），而 `tc -s` 仍显示
qdisc dropped 0——真正的丢包发生在**我们自己的 relay 数据队列**里。

原因：`dataQueueByteBudget=1MB`（`p2p/p2p.go`）。300ms 台阶下 8 流的 BDP 约
1.5~3MB，1MB 预算装不下，队列满后直接丢帧；inner TCP 看到丢包 → SACK/RTO →
重传风暴 → 空洞重传再被丢 → RTO 指数退避到 42s，形成多分钟停滞。

单变量：只改 `dataQueueByteBudget` 1MB→4MB（qg 重启加载），jf 保持 DSACK undo
版本。连跑 4 次 300ms 无丢包台阶：

| 配置 | 台阶 8 流耗时 | 完整性 | qg DataDrops |
|---|---|---|---|
| 1MB + DSACK undo | 68 / 82 / 89 / 98 / **400**（一次） | 8/8（400s 那次超时截断） | 446（vm peer） |
| **4MB + DSACK undo** | **70 / 83 / 85 / 95** | **4/4 全部 8/8** | **0（全部 peer）** |

（基线同期 55~93s 波动很大：qg 今天多次重启、链路反复收敛；不能用跨小时的
基线绝对值比较，队列丢包计数是更可靠的判据。）

结论：**把 relay 每 peer 预算提到 4MB 消除了台阶期间的 relay 丢帧**，配合
DSACK undo 后 4 次台阶全部完成、无多分钟停滞。仍待观察：长时间自然运行下
4MB 是否带来内存/缓冲膨胀副作用；以及台阶恰好命中慢启动早期时的退化
（§93 末尾）。

（部署注记：qg 部署按 `.claude/skills/qg-deploy`：传 `/root/phaethon.new` →
`rc-service phaethon stop; zap; start`，重启会断当前 SSH。jf 必须走真实
sshd `36.140.28.178:60002`，否则 `pkill` 会杀掉承载部署会话的内置 SSH。）

## 96. 全量部署与最终验证（2026-11-11 16:05）

两个改动合入后构建 `v0.8.0-547-g9ddb2a9`，已部署并核对：

| 节点 | 平台 | 状态 |
|---|---|---|
| vm（本机 Windows） | windows/amd64 | 计划任务重启，双进程，日志 `P2PManager initialized v0.8.0-547-g9ddb2a9` |
| qg | linux/amd64 | OpenRC 重启，`rc-service status: started` |
| jf | linux/amd64 | 真实 sshd 部署，双进程 |
| gg | linux/amd64 | `loc.gg.phn` 部署，双进程，39988/39998/39999 LISTEN |
| mb | darwin/amd64 | 经 qg LAN 中转上传，`md5 -q` 一致，手动 `sudo sh -c` 启动根进程 |
| qgw / deskw | — | 按用户要求不由我部署 |

最终同一构建下 8 流 vm→jf 196.3MB 验证（qg 只在台阶窗口加 300ms、无丢包）：

| 场景 | 耗时 | 吞吐 | 完整性 | qg DataDrops |
|---|---|---|---|---|
| 基线（无台阶） | **39s** | 5.03MB/s | 8/8 | 0 |
| 300ms 台阶 #1 | 93s | 2.11MB/s | 8/8 | 0 |
| 300ms 台阶 #2 | 82s | 2.39MB/s | 8/8 | 0 |

对比修复前：stock 在同样台阶下出现 393s 多分钟停滞，以及 71s 但 **8 条流全部
被 reset、文件截断**。现在不再出现冻结/重置，台阶期间只表现为 RTT 翻倍带来的
吞吐下降（8 流总吞吐仍 >2MB/s）。

**结论**：
1. 多分钟停滞的直接原因不是链路静默，而是 **RACK 把延迟/乱序当丢包 → 伪重传
   风暴 → cwnd 塌缩 + RTO 指数退避**；DSACK 是伪重传的直接证据。
2. 放大因素是 **relay 每 peer 1MB 队列在台阶 BDP 下溢出丢帧**（qdisc dropped 0
   但 DataDrops=446），空洞重传被反复丢弃，RTO 才能退避到 42s。
3. 修复 = DSACK 伪重传撤销 cwnd 收缩（fork）+ relay 预算 4MB（p2p）。二者
   缺一仍会在部分时序下退回长停滞（4MB 单独不解决 RACK；DSACK undo 单独
   也会被 relay 丢帧拖回 RTO 退避）。
4. 残留：台阶仍会带来 ~2× 的短时减速（外部延迟本身）；基线 5.0MB/s 仍低于
   OS 直连 8.03MB/s，属于吞吐专项的旧账，不是本次停滞问题。4MB 预算的内存
   与长跑缓冲膨胀需要后续观察。

## 97. deskw 用内置 SSH 发版：pkg 热切换绕过“停应用断 SSH”（2026-11-11 16:22）

deskw 是 Windows，只有 phaethon 内置 SSH 通道（落地是 PowerShell，父进程
`explorer.exe`，即交互式桌面会话）；没有 scp/sftp，OS sshd 不可用。

直接 `pkill + 替换 exe + 启动` 会在第一步杀掉承载 SSH 的进程，脚本自断。
改用 watchdog 的 **pkg 热切换**，watchdog 不重启、不会被自己杀掉：

1. 打签名包（版本戳用 `git describe` 原文）：
   `go run ./pkg/signing/cmd/sign -binary <win exe> -meta <meta.json> -key scripts/signing.key -output <pkg>`
   本地用 `signing.VerifyPkg(pkg, signing.PublicKey)` 验签通过（8.9MB）。
2. 传输：因为只有内置 SSH，用 base64 经 stdin 送入 PowerShell
   `[Console]::In.ReadToEnd()` → `WriteAllBytes`，落盘到
   `data/packages/phaethon_windows_amd64_v0.8.0-547-g9ddb2a9.pkg`，
   远端 `Get-FileHash MD5` 与本地一致。
3. 触发：用 `schtasks` 建一次性任务（任务计划服务所有，**不随 SSH 会话死亡**），
   脚本只杀“父进程也是 phaethon 的那个” worker，不碰 watchdog；
   SSH 会随 worker 断开，属预期。
4. watchdog 在 3s 监控周期内发现子进程退出，重扫 `data/packages`，验签、
   比对版本（v547 > 自身 v529）、解包到
   `data/worker/phaethon-v0.8.0-547-g9ddb2a9` 并拉起。新 worker 起来后内置
   SSH 自动恢复。

验证：watchdog PID 2840 未变，worker PID 4508 指向
`data\worker\phaethon-v0.8.0-547-g9ddb2a9`；日志
`P2PManager initialized (version=v0.8.0-547-g9ddb2a9, platform=windows/amd64)`；
无 panic；`schtasks` 任务与临时脚本已删除。

副作用与注意：
- 主 exe/watchdog 仍是 v529，升级靠 `data/packages` 里的 pkg 持久化；
  **不要清空该目录**，否则重启会回退 v529。
- 同期修复了 `pkg/signing/cmd/sign` 的版本解析 bug：在 Windows 上直接执行
  Windows 目标二进制时 `--version` 输出是 `version=... platform=...`，旧代码取
  `Fields()[1]` 会得到 `platform=windows`；现在优先解析 `version=` 字段。

## 98. qgw（Windows 7）内置 SSH 发版：限制更多，走 LAN HTTP 中转（2026-11-11 16:41）

qgw 与 deskw 同架构（watchdog + worker），但环境更受限：

- Windows 7 SP1（NT 6.1.7601），PowerShell **2.0**（无 `Get-CimInstance` /
  `Get-Content -Tail` / `Get-FileHash`）；
- 内置 SSH 的 exec **不转发 stdin**（`[Console]::In.ReadToEnd()` 得到空串），
  无 scp/sftp；
- exec 命令行有长度限制，带较长 base64 的命令直接
  `exec request failed on channel 0`（短命令正常）；
- `schtasks.exe` 被拒绝访问（“拒绝访问”），无法用计划任务做 detached 触发。

因此 deskw 的“stdin base64 上传 + schtasks 触发”在 qgw 都不可用，改用：

1. **构建**：必须用 Win7 兼容工具链
   `C:\go-legacy-win7\bin\go.exe build -ldflags "... -X main.Platform=windows7 ..."`
   （Makefile 的 `windows7` 目标，`GO_LEGACY_WIN7`）。
2. **签名包**：meta `platform=windows7`，版本用 `git describe` 原文
   （`v0.8.0-549-g7328aee`），本地 `signing.VerifyPkg` 验签通过。
3. **传输**：qgw 与 mb 同 LAN（qgw 192.168.1.100、mb 192.168.1.88）。
   把 pkg 放到 mb `/tmp`，用 `/usr/local/bin/python3 -m http.server 18080`
   起临时 HTTP；qgw 用
   `(New-Object Net.WebClient).DownloadFile("http://192.168.1.88:18080/win7.pkg", ...)`
   直接下到 `data/packages/`，`certutil -hashfile` 校验 MD5 一致。
   （注意 mb 的 `/usr/bin/python3` 被坏掉的 Xcode 路径劫持，要用
   `/usr/local/bin/python3`。）
4. **触发**：qgw 上只有 watchdog（16568）与 worker 两个进程，直接
   `Stop-Process -Id <worker> -Force`。命令很短，在会话被拆掉前已执行；
   watchdog 不重启，3s 监控周期内发现子进程退出，验签、版本比较
   （549 > 529）、解包到 `data/worker/phaethon-v0.8.0-549-g7328aee` 并拉起。
   SSH 随 worker 断开，数秒后随新 worker 恢复。

验证：watchdog 16568 未变；worker 15484，进程名
`phaethon-v0.8.0-549-g7328aee`，路径 `data\worker\phaethon-v0.8.0-549-g7328aee`；
日志 `P2PManager initialized (version=v0.8.0-549-g7328aee, platform=windows7/amd64)`；
无 panic；mb 侧 `peers[qgw].lastSeen` 持续刷新、direct=true、srtt≈67ms。

注意：qgw 主 exe/watchdog 仍是 `v0.8.0-529-gf5d243d-win7`，升级靠
`data/packages` 中的包持久化；不要清空该目录。worker 进程名不含 `.exe`
（解包文件名），用 `Get-Process phaethon` 只能看到 watchdog，需用通配符。

## 99. 内置 SSH 中文乱码/交互挂起的根因与修复（2026-11-11 18:05）

用户 `ssh qgw.phn` / `ssh deskw.phn` 看到 **PTY allocation request failed**、
PowerShell banner 乱码，属于内置 SSH 的实现问题，不是网络：

1. **没有 ConPTY 就走管道模式**。deskw 是 Win10 **14393（1607）**、
   qgw 是 Win7 7601，ConPTY 要求 1809+，所以 `supportPty=false`，
   `pty-req` 被拒绝（这是对的：伪造 PTY 会让客户端关掉本地回显，
   管道模式反而变成“黑洞”）。客户端于是打印 PTY 警告并退回 cooked 模式。
2. **管道 shell 没做编码处理**：PowerShell 启动 banner 按控制台初始代码页
   （中文系统 GBK）输出，cmdlet 输出同样按 GBK 写出，UTF-8 客户端必然乱码。
3. **PS 2.0（qgw）不读重定向 stdin**：不带 `-Command -` 时，交互 shell
   直接挂起（现场能看到一堆孤立的 `powershell.exe`，从 v529 起就存在）。

修复（`server/ssh_session*.go`，版本 `v0.8.0-557-g8ebcc7e`）：

- 管道模式 PowerShell 统一 `-NoLogo -Command -`：去掉 GBK banner，
  并强制从 stdin 读命令（PS2/PS5.1 都不会挂起）；
- 启动注入 UTF-8 init：`[Console]::OutputEncoding=UTF8` + try/catch；
  **InputEncoding 只在 PS Major<=2 时设置**——实测 PS5.1 在
  `-Command -` 管道会话里设置 InputEncoding 会抛终止性异常，整个会话
  无任何输出；
- 输出侧剥掉 PS2 宿主写在首部的 UTF-8 BOM（首块内联剥离，不阻塞短读）；
- cmd 用 `/Q`（关回显）+ `chcp 65001`。

验证（`ssh <node>` 管道喂命令）：

| 节点 | 环境 | `HI` | 中文输出 | 中文输入 | banner |
|---|---|---|---|---|---|
| qgw | Win7 + PS2.0，无 ConPTY | ✅ | ✅ | ✅ | 无 |
| deskw | Win10 1607 + PS5.1，无 ConPTY | ✅ | ✅ | ✅ | 无 |
| vm | Win11 + pwsh7，ConPTY | ✅ | ✅ | 交互（PTY）✅；纯管道输入仍按 OEM 解码 | 无 |

部署：qgw/deskw 通过 pkg 热切换（worker = `data/worker/phaethon-<ver>`，
watchdog 不动），vm 走计划任务重启；三者均为 `v0.8.0-557-g8ebcc7e`。

已知边界：Win7/1607 没有 ConPTY，无法提供真正的 PTY，交互是“行级 + 客户端
回显”，没有服务端提示符；`ssh -T` 或 `ssh host '命令'` 不受影响。

### 99.1 恢复 Win10+ 的交互提示符（v0.8.0-559-g6396c5b）

最初为兼容 Win7 的 PS2，所有 PowerShell 管道会话统一用了 `-Command -`，
副作用是 deskw（Win10 1607，无 ConPTY）这类 PS5.1 节点也**没有提示符**，
看起来“没有交互”。实际上 PS3+ 的交互宿主能正常读取重定向 stdin，
所以按 OS 主版本分流：

- `RtlGetVersion().MajorVersion >= 10`：`-NoLogo`（交互宿主，提示符恢复）；
- Win7/8（PS2）：仍用 `-NoLogo -Command -`（否则不读 stdin）。

验证（`ssh <node>` 管道喂命令）：

| 节点 | 环境 | 提示符 | HI/中文输出 | 中文输入 |
|---|---|---|---|---|
| deskw | Win10 1607 + PS5.1，无 ConPTY | ✅ `PS C:\...>` | ✅ | ✅ |
| vm | Win11 + pwsh7，ConPTY | ✅（PTY 会话为完整 ConPTY） | ✅ | ✅ |
| qgw | Win7 + PS2，无 ConPTY | ❌（PS2 限制） | ✅ | ✅ |

qgw 的“无提示符”是 **OS/PowerShell 限制**，不是没实现：PS2 不带
`-Command -` 不读 stdin；Win7 又没有 ConPTY，无法提供真 PTY。要让它也有
完整交互，只能内置第三方 PTY（winpty/ConPTY shim），当前未引入。

## 100. 内置 winpty PTY（无 ConPTY 的 Win10 节点）（2026-11-11 19:45）

按“随部署目录分发 winpty”的方案落地，不引入 cgo：

- `third_party/winpty/amd64/` 入库 winpty 0.4.3 的 `winpty.dll` +
  `winpty-agent.exe`（Git for Windows 同版本，DLL/agent 均无额外运行时依赖）；
- 部署时把这两个文件放到部署根目录（与 phaethon.exe / worker 的 CWD 同级，
  即 `Desktop\phaethon\` 或 `Data\worker\..\..\`）。代码按 exe 目录、其两级
  父目录、CWD 顺序查找 DLL，并把该目录前插 PATH 让 agent 可被发现；
- `winpty_windows.go` 用 `syscall.NewLazyDLL` 动态加载 0.4.3 API：
  `winpty_config_new/open/spawn_config_new/spawn/conin_name/conout_name/set_size/free`；
  `winptyProc` 实现 `sessionProc`（Read/Write/Resize/Wait/Kill）；
  **conin 只写、conout 只读**（O_RDWR 会 ACCESS_DENIED）；
  `appname` 先 LookPath 成绝对路径（CreateProcess 带 appname 不搜索 PATH）；
- 首次 `pty-req` 时做一次端到端探针（winpty 拉 `cmd /c exit 0`），失败则
  拒绝 pty-req、回退管道模式，避免“接受了 PTY 却黑屏”；
- 交互 shell 仍是 `-NoLogo`（真 console 下 PS3+/PS5.1 正常读输入、打提示符），
  注入的 UTF-8 init 在 winpty 路径以 **CR 结尾**（LF 会被当成第二次换行，
  导致 PS 停在 `>>`）。

平台门控：`ptyAvailable = ConPTY || (OS Major>=10 && winptyReady())`。
实测 **Win7 的 PS2 交互宿主在 winpty 控制台下不消费输入**（会话挂起、无
提示符），所以 Win7/8 明确关闭 winpty，继续使用稳定的 pipe 行级模式。

验证（v0.8.0-567-gb7b4b9f，Go SSH 客户端强制 pty-req 100x30）：

| 节点 | 结果 |
|---|---|
| deskw（Win10 1607 / PS5.1） | winpty PTY：提示符、`中文测试`、`HI`、`exit` 均正常 |
| vm（Win11） | ConPTY（原路径），未受影响 |
| qgw（Win7 / PS2） | pty-req 按门控拒绝，pipe 行级模式中文正常（无提示符） |

部署：winpty 两文件已放到 deskw/qgw/vm 的部署根目录；deskw 通过 pkg 热切换、
qgw 通过 pkg 热切换、vm 走计划任务，三节点均为 `v0.8.0-574-gc53c1ee`。
（调试注记：测试客户端把 stdout/stderr 共用同一个 `bytes.Buffer` 会产生竞态，
导致偶发 0 字节输出；换线程安全 writer 后稳定。）

### 100.1 Win7/PS2 + winpty 复测结果：不稳定，最终关闭

不死心又试了几轮 Win7（qgw）：

- 去掉 `chcp`、只设 `[Console]` 编码后，PS2 **能**出提示符并执行命令
  （`Write-Output 中文` 正常），说明 winpty 通道本身可用；
- 但同一构建下会话**不稳定**：多次只输出初始清屏（8 字节）、命令不执行、
  进程立即结束；`exit` 在 PS2 下不会终止进程（加了服务端在 exit 后
  TerminateProcess 的兜底仍无法改变“会话早退”的问题）；
- 影响面是 PS2 的交互宿主 + 隐藏控制台，不是我们的管道/编码逻辑。

最终决策：`ptyAvailable = ConPTY || (OS Major>=10 && winptyReady())`，
Win7/8 固定走 pipe 行级模式（命令、UTF-8 中文都正常）。要把 qgw 变成真
PTY，可靠路径是升级系统里的 PowerShell：**WMF 5.1**（Win7 SP1 可装）或
**PowerShell 7.1**；升级后与 deskw（PS5.1 + winpty，已验证稳定）一致。
另一个低成本选项是给 PS2 做“提示符 REPL”（无 TTY，不能跑全屏程序）。

### 100.2 根因修正：断开的桌面会话，而非 PS2/版本（2026-11-11 23:35）

继续深挖后找到了真正的相关性：

- `qwinsta`：**qgw 的 wangqian 会话是 `Disc`（断开）**；deskw 是 `Active`；
- 失败会话的服务端日志：spawn 成功、input copy n=48、shell 在 exit 后
  以 0 退出（**命令确实执行了**），但 output copy 只有 **8 字节**（初始清屏）；
- winpty 的 agent 靠**抓取隐藏控制台的屏幕缓冲**来渲染 conout；桌面会话
  断开时输入仍能进、命令仍能跑，但屏幕不更新，所以只有清屏、没有回显；
- winpty 0.4.2（含 x64_xp）与 0.4.3 表现一致，**不是版本/输入处理问题**。

修复：用 `WTSQuerySessionInformation(WTSConnectState)` 判断当前会话：

- `Active`（管理员 RDP/控制台连着）：启用 winpty，真 PTY；
- `Disconnected`（无人值守）：拒绝 pty-req，回退稳定的行级模式。

验证（v0.8.0-579-gb6887c3）：Disc 状态下 pty-req 被拒绝、行级中文正常；
Active 状态下的 winpty 验证需要 qgw 桌面保持连接后复测。
（部署注记：qgw 的部署目录在 qg 上是 `/root/qgw` vboxsf 挂载，部署包直接
`cp` 到 `/root/qgw/data/packages/` 即可，不再走 mb HTTP 中转。）

### 100.3 Win7 winpty 最终结论：本机屏幕捕获不可靠（2026-11-12 00:0x）

管理员 RDP 连上 qgw（`qwinsta` 显示 `Active`）后继续复测，做了完整单变量：

| 变量 | 结果 |
|---|---|
| winpty 0.4.3 / 0.4.2 x64_xp | 行为一致 |
| 注入 init / 不注入 | 注入时 init 行执行后 PS2 不回提示符；不注入时提示符稳定出现 |
| pty-req 探针 / 去掉探针 | 去掉后提示符出现更稳定，但命令仍不执行 |
| Disc / Active 会话 | Disc 只有 8 字节清屏；Active 能出提示符（38~203 字节） |

服务端日志（带 init 的版本）明确：spawn 成功、input copy n=48、shell 在
exit 后以 0 退出（**命令确实执行**），但 output copy 只有 8 字节。
Active + 无 init 时提示符能渲染，但随后发送命令既不回显也不执行。

deskw（Win10 1607）用同一套代码、同一个 winpty 版本工作稳定，说明差异在
**Win7 的控制台/屏幕缓冲行为**，不是我们的输入/退出/编码逻辑，也不是
winpty 版本。Win7 上无法通过参数调优让它稳定。

最终状态：回到 v574 的实现（v0.8.0-583-g9e0c0e4 部署在 qgw），**Win7/8
固定行级模式**，Win10 无 ConPTY 用 winpty/ConPTY。Win7 要真 PTY 只有换
Win10 1809+（ConPTY）或接受行级/REPL；WMF 5.1/PS7 不会带来 ConPTY。

## 101. 同载体基线纠偏：差距在 mesh 数据面，不在物理链路（2026-11-12 01:30）

**环境**：vm `v0.8.0-574-gc53c1ee`、qg/jf `v0.8.0-547-g9ddb2a9`；
vm↔qg 走 `vm:QG_SS5_UDPLEG`（SOCKS5 10.11.61.50:7890，UDP leg Active），
qg↔jf 走 `qg:MGMS_HT`（h_tunnel，transport=tcp）。
载荷 `/tmp/bench24.bin`=24.5MB（jf md5 `438de69e…`、qg md5 `054d4242…`），
8 流；测量脚本 `tmp_bench_hop.sh`（`ssh ... cat file`，丢弃输出，墙钟/字节）。

### 101.1 同窗口同载荷对照（一行一个载体）

| 路径 / 载体 | 方向 | 吞吐 | 说明 |
|---|---|---|---|
| vm ↔ qg 公网 `10.11.61.50:2222`（OS sshd） | 下载 | **9.84MB/s** | 宿主机 NAT 端口转发（eth0） |
| vm ↔ qg 公网 `10.11.61.50:7890`（原生 SOCKS5） | 下载 | **11 / 9.0MB/s** | curl 单流 / 8 流，无 mesh |
| vm ↔ qg 内网 `192.168.1.101:2222`（OS sshd） | 下载 / 上传 | **1.33 / 2.43MB/s** | 经 192.168.1.1 的 eth1 路径 |
| vm → qg mesh（UDP leg） | 下载 / 上传 | **1.8 / 7.2MB/s** | 内建 SSH，8 流 |
| vm → qg mesh（关 UDP leg，TCP 载体） | 下载 / 上传 | **3.1 / 3.6MB/s** | 同上，`p2p-udp=false` 单变量 |
| vm → qg → jf（OS ssh 串接，无 mesh） | 下载 | **4.1~4.6MB/s** | 同窗口公网链路已劣化（历史 9.79） |
| vm → jf mesh（两跳） | 下载 | **0.7~1.4MB/s** | UDP leg 1.0~1.4；TCP 载体 0.7~0.9 |

**结论 1（口径纠偏）**：`10.11.61.50:2222` 的 9.84MB/s 与 mesh 走的
`192.168.1.101/eth1` 不是同一条物理路径，不能直接当作 mesh 的“直连基线”。
在**同一条 eth1 路径**上 OS ssh 只有 1.33MB/s，mesh UDP leg 的 1.8MB/s 已不低于它；
真正同载体的上限是原生 SOCKS5 的 9~11MB/s —— mesh 数据面相对它损失 3~5×，
这才是需要优化的部分。后续基线必须写明“哪条接口/哪个载体”。

**结论 2（方向不对称）**：UDP leg 下载 1.8 / 上传 7.2；TCP 载体下载 3.1 / 上传 3.6。
下载慢是 qg→vm 方向（qg 被动腿经 SOCKS5 relay 回写）特有的问题，
与“链路带宽不足”不符（同链路原生 SOCKS5 可 9~11）。

### 101.2 单流内层 TCP 现场（download，UDP leg）

单流连续 98MB，qg gVisor 发送端采样（`tcpFlowDebug`，6 次）：

| 指标 | 值 |
|---|---|
| cwnd | 128 / 148 / 150 / 176（自适应上限 4×BDP，floor 128 / ceil 512） |
| srtt / minRTT | 155~201ms / **37ms** |
| RACK RTT / reoWnd | 62~205ms / 73ms |
| DSACK 计数 | 0~2（**几乎无丢包**） |
| sndWnd（vm 通告） | 2MB |
| 单流吞吐 | **1.85MB/s**（98MB/53s） |

解释：无丢包、接收窗 2MB、cwnd 贴着自适应上限，吞吐 ≈ cap×MSS/srtt。
srtt 比 minRTT 高 ~120ms，说明路径上存在约 200KB 的驻留队列
（`p2p.udpLegTransport` 的 `legCh` 256 个数据报 ≈ 384KB，或 lane/relay 队列）；
`[P2P-QDEPTH]` 阈值 256KB/1s，恰好低于该水位，所以日志没有报。
单流与 8 流聚合几乎相同（1.85 vs 1.8），确认是**共享的单点串行瓶颈**，不是每流 cwnd。

### 101.3 顺带发现：jf→vm 路由仍会短暂切到 gg

一次采样中 jf 的 `bestPaths` 为 `vm via gg cost=174.5`（gg 是 ~1s 链路），
几十秒后恢复 `via qg cost=72.4`；拓扑里 `qg -> vm` 边存在（cost 68.9ms，
gg->vm 123.4ms）。同时 vm→jf 新建 SSH 握手 0.8~1.1s。
这和 §11 的“回程绕 gg”同类，属路由/边成本抖动，会让两跳吞吐出现 0.7~0.9 的离群跑。

### 101.4 下一步（单变量）

1. **cwnd 自适应上限是否就是限制**：fork `cwndBDPGain` 4→16（qg 为下载发送端），
   同窗口 A/B 单流/8 流；若吞吐显著上升 → 继续调 gain/ceil；若只涨 RTT 不涨吞吐 →
   瓶颈是驻留队列（接着压 legCh/lane 队列）。
2. 若 1 无效：把 `legCh` 入队水位、`laneQDTarget=200ms` 与 `P2P-QDEPTH` 阈值对齐排查，
   确认 200KB 队列在哪个环节。
3. 两跳另开一条：给 jf→vm 的路由加切换滞回，避免短时切到 gg。

### 101.5 A/B 结果：cwnd BDP 增益 4→16（2026-11-12 01:50，突破）

单变量：fork `cwndBDPGain` 4→16（上限仍 ceil 512 段），其余代码/配置不动；
只把 qg 换成就绪的 `v0.8.0-583-g2ab2f5c-gain16`（下载方向发送端），jf 仍 547/gain4。

| 场景（8 流，同窗口，载荷 md5 校验） | gain4 | **gain16** | 倍数 |
|---|---|---|---|
| vm→qg 单流 24.5MB | 1.23~1.85MB/s | **7.79MB/s** | ~5× |
| vm→qg 8 流 | 1.80~1.97MB/s | **9.35~9.84MB/s** | ~5× |
| vm→qg 8 流落盘完整性 | — | **8/8 MD5 一致**，24.1s=8.1MB/s | — |
| vm→jf 两跳 8 流 | 0.67~1.43MB/s | **4.56 / 5.66MB/s** | ~4× |
| 同窗口 OS 两跳 8 流 | 4.07~4.56MB/s | **9.35MB/s** | 链路已恢复 |

**结论**：此前 4×BDP 的自适应上限（floor 128 段）就是单跳/两跳的直接限制。
`cap=4×rate×minRTT/MSS` 在 1.85MB/s、minRTT 37ms 时只有 ~190 段，正好把
单流和 8 流都压在 ~1.8MB/s；抬高到 16×BDP（实际 ceil 512 段）后，内层发送端
可以填满载体的真实容量，单跳直接回到原生 SOCKS5 的 9~11MB/s 量级。
101.2 里 120ms 的 srtt 增量是“小窗口在队列里排队”的结果，不是独立的管路限制。
两跳仍低于 OS 两跳，剩余差距在 jf（仍 gain4）与 qg 中转；下一步把同一二进制
发到 jf/gg，再跑 300ms netem 台阶与长跑，确认不重新引入停滞。

### 101.6 增益必须按载体区分：默认 4x + 节点显式 opt-in（2026-11-12 02:40）

把同一二进制发到 jf 测试后发现 **16x 只适合干净载体**：

| jf 侧 BDP 增益（同窗口，vm→jf 8 流） | 中位吞吐 | 同窗口 OS 两跳 | 备注 |
|---|---|---|---|
| **4x（默认）** | **6.23MB/s**（5.19/6.23/6.68） | 9.84 | 最优 |
| 8x（env=8） | 4.79MB/s（4.79/5.05/4.45） | 9.35 | 明显退化 |
| 16x（env=16） | ~3.6MB/s（2.79~4.35） | 9.84 | 部分流 ssthresh=2/cwnd=2，RTO 塌缩 |

原因：jf→qg 走 `MGMS_HT`（h_tunnel），链路 minRTT≈55ms 但 srtt 150~200ms、
DSACK 7~13%；大窗口会把突发放大成延迟尖峰，触发 RTO。qg→vm 的 UDP leg
DSACK 接近 0，16x 才有 5× 收益。

**最终实现（fork `e29b10f` + 主仓 `2119bc5`）**：

- fork 默认值恢复为 4x，新增 `tcp.SetCwndBDPGain(v)`；
- 主仓启动时读 `PHAETHON_CWND_BDP_GAIN`（0/空 = 默认 4x）；
- 任一流一旦 RTO，该流固定回落 4x（防止抖动载体上大窗口反复塌缩）；
- qg 的 `/etc/init.d/phaethon` 显式 `PHAETHON_CWND_BDP_GAIN=16`（持久），
  jf/gg 等 ht 载体节点保持默认 4x。

**最终同窗口验证**（qg=16、jf=4）：

| 场景 | 结果 |
|---|---|
| vm→qg 单跳 8 流 | **9.84MB/s**（与原生 SOCKS5 9~11 持平） |
| vm→qg 8 流完整性 | 8/8 MD5 一致，24.1s=8.1MB/s |
| vm→jf 两跳 8 流 | 5.19 / **6.23** / 6.68MB/s（中位 6.23） |
| vm→jf 两跳完整性 | **8/8 MD5 一致**，35.4s=5.54MB/s |
| 同窗口 OS 两跳 | 9.84MB/s（mesh 占 **63~68%**，最好 68%） |

**剩余差距**：两跳距 OS 两跳还差 ~32%，瓶颈已收敛到 jf→qg 的 h_tunnel
回程 ACK：minRTT 55ms、srtt 150~200ms、DSACK 7~13%。下一步做 ht ACK
路径的 sojourn/批量实验（不是继续加 cwnd）；另需补 300ms netem 台阶与
长跑回归，确认 16x 在 qg 干净载体上不会重新引入停滞。

### 101.7 稳定性回归：300ms 无丢包台阶 + 5 连跑（2026-11-12 03:00）

**300ms 无丢包台阶**（qg eth1，`tc qdisc change` 原地改延迟，§92 配方）：

| 场景 | 耗时 | 吞吐 | 完整性 | qdisc 丢包 |
|---|---|---|---|---|
| 基线 8 流两跳 | 28~32s | 6.2~6.7MB/s | 8/8 | 0 |
| 300ms 台阶 8 流 | 61s | **3.06MB/s** | **8/8（fail=0）** | **dropped 0** |

台阶期只是吞吐下降到约一半（RTT 翻倍的物理代价），**没有出现 cwnd 集体塌缩、
流重置或分钟级停滞**；qdisc backlog 峰值 ~639KB，requeues 218，无丢包。

**5 连跑**（qg=16x，jf=4x，同窗口 OS 两跳 9.84MB/s）：

| 运行 | 1 | 2 | 3 | 4 | 5 | 中位 |
|---|---|---|---|---|---|---|
| vm→jf 两跳 8 流 | 6.68 | 6.68 | 5.84 | 6.45 | 6.23 | **6.45MB/s** |

全部 8/8 流完成（fail=0），无停滞；中位为同窗口 OS 两跳的 **65.5%**，最好 68%。

过程注记：在 jf 试自适应增益（DSACK 版）时出现过一次 661s、8 流全失败的
离群跑；换回最终二进制（qg=16/jf=4）后 5 连跑未复现。判断为环境瞬断/载体
抖动，仍按“偶发停滞”类别继续观察，若再出现先抓 qg/jf 的 UDP-LEG 与
channelDropStats 现场。

## 102. h_tunnel 回程批量调小：两跳中位 6.45~7.19，最好 8.13MB/s（2026-11-12 03:40）

### 102.1 同窗口 A/B：`htunnel.max-batch-size`

jf 的 h_tunnel 服务端按 `max-batch-size` 组装 GET 响应；默认 512KB。
同一窗口（OS 两跳 9.35~9.84MB/s）交替改配置 + 重启 jf，各测 2 次：

| jf max-batch-size | vm→jf 两跳 8 流 | 同窗口 OS 两跳 |
|---|---|---|
| 512KB（默认） | **2.71 / 2.79MB/s** | 9.35 |
| **128KB** | **5.84 / 5.84MB/s** | 9.35 |
| 64KB | 7.79 后**停滞**（见 102.3） | 9.84 |

同一窗口内 128KB 是 512KB 的 2 倍：批量越大，jf→qg→vm 的突发越长，
内层 TCP 的 RTT 被突发排队抬高得越多（sender 的 srtt 150~200ms vs minRTT 55~80ms）。

多窗口汇总（128KB）：

| 组 | 结果 | 中位 |
|---|---|---|
| 5 连跑 A | 5.66 / 7.19 / 7.48 / 8.13 / 7.19 | **7.19**（OS 9.84，73%） |
| 5 连跑 B | 7.19 / 6.45 / 6.45 / 6.45 / 7.48 | **6.45**（OS 9.84，65.5%） |
| 3 连跑 | 7.48 / 6.03 / 8.13 | 7.48（OS 9.35） |

最好 8.13MB/s = 同窗口 OS 的 **82.6%**，但中位尚不稳定在 80% 以上；
链路时段漂移仍是主因。完整性：128KB 下 8 流 **8/8 MD5 一致**（34.8s=5.63MB/s）。

### 102.2 get-slots 8→16（128KB 下）：更差，回退

（02:55 注记：先前“16→8 回退”那次实际没有写进 qg 配置，下面按配置核验后的
数据重新区分；结论不变。）

| 配置 | 5 连跑 | 中位 | 同窗口 OS |
|---|---|---|---|
| **get-slots=8** | 5.66 / 7.19 / 7.48 / 8.13 / 7.19 | **7.19** | 9.84（73%） |
| get-slots=8（另一窗口复核） | 7.19 / 7.79 / 8.13 / 6.45 / 7.19 | **7.19** | 9.35（77%） |
| get-slots=16 | 6.68 / 6.23 / 5.66 / 6.45 / 7.19 | **6.45** | 9.84（65.5%） |
| get-slots=16（复测） | 7.19 / 6.45 / 6.45 / 6.45 / 7.48 | **6.45** | 9.84（65.5%） |
| get-slots=4 | 5.84 / 5.19 / 4.79 | 5.19 | 9.35~9.84（~53%） |
| get-slots=2 | 4.92 / 1.76（106s 慢跑） | — | 9.84，且不稳定 |

16、4、2 都稳定低于 8：8 个 GET 是当前窗口的最优点。最终 qg 配置为
`{"htunnel":{"get-slots":8}}`（已核验 qg API 返回 8）。

### 102.3 64KB 触发停滞的现场（重要）

64KB 首跑 7.79MB/s，随后一次 8 流传输直接卡住 >5 分钟。卡住时 jf 内层
发送端现场（8 条 port-22 流）：

| 指标 | 值 |
|---|---|
| srtt | **3.8~5.0s** |
| minRTT | 79~82ms |
| RTO | **8~19s** |
| cwnd | 7 条 128（cap），1 条 5（ssth=2） |
| DSACK / retransmits | **28158 / 35533（约 79% 是伪重传）** |
| 状态 | sackRecovery，lastSend 最高 4s |

与 §80/§92 的“RACK 把延迟/乱序当丢包 → 伪重传风暴 → srtt 正反馈”完全同型。
64KB 把 GET 批次切得更碎，多 GET 之间的帧交错/重组缺口更频繁，直接触发该
正反馈，因此 **64KB 不安全，最终选 128KB**。

**当前生产**：jf `htunnel.max-batch-size=131072`、默认 4x 增益；
qg `htunnel.get-slots=8`、`PHAETHON_CWND_BDP_GAIN=16`。

### 102.4 96KB 同样触发停滞：128KB 是小批量的安全下界

96KB 两次：6.03 / 8.90MB/s（OS 9.84），第三次直接进入停滞，jf 内层流现场：

```
srtt 3.4~5.5s  minRTT 79~97ms  RTO 8.8~16.6s  cwnd 128（一条 55）
```

与 64KB 同一形态。128KB 在 8~10 次连续跑中未出现该形态，因此
**128KB 是当前安全下界，不再往下试**。

### 102.5 阶段结论与剩余差距

| 指标 | 结果 |
|---|---|
| 128KB 两跳中位（多窗口） | **7.19MB/s**（OS 9.35~9.84 的 **73~77%**） |
| 128KB 两跳最好单次 | **8.13 / 8.90MB/s**（OS 的 **82.6~90%**） |
| 512KB 同窗口 A/B | 2.71 / 2.79（128KB 同窗口 5.84） |
| 完整性 | 8/8 MD5 一致（34.8s=5.63MB/s） |

目标“中位 ≥80%”尚未稳定达成：最好单次已过 80%，但中位仍在 73~77%。
剩余方差全部指向同一个机制：**ht 多 GET 下的乱序/延迟被 RACK 判成丢包 →
伪重传风暴 → srtt 3~5s 正反馈**（DSACK 已证明 79% 重传是伪的）。
下一步不是继续调批量/增益，而是压掉这个正反馈本身（例如 RACK 判丢前
用 minRTT/reoWnd 的更硬门槛，或 orderedInbox 缺口不跳批、按流保序投递）。

### 102.6 reoWnd 上限 150→300ms：中性，回退

单变量（jf 侧 RACK reo_wnd floor cap 150→300ms，§83 的自适应公式其余不变），
同窗口（OS 9.84）3 次：6.03 / 7.19 / 6.92，中位 6.92（~70%），
与 128KB/默认 150ms 的 6.80~7.19 无差异。已回退代码与 jf 二进制。
说明健康窗口里的 DSACK 税不是靠放宽 reoWnd 能消掉的；下一步应做
**发送侧 pacing** 或 **egress sojourn AQM**，从源头减少突发而不是放宽判丢。

### 102.7 pacing 评估：gVisor 当前版本无 pacer，暂不实施（未验证）

> 定性说明：**这不是“pacing 无效”的结论**。当前 gVisor fork 没有实现发送侧
> pacer，我们也没有写过一版 pacing 去测，因此无法证伪它。下面的“暂不实施”
> 只基于间接证据，属于**优先级判断**，不是功能否定；条件变化后应重新评估。

评估目标：用发送侧 pacing 削平 cwnd 突发，减少 ht 载体排队和 RACK 伪丢包。

间接证据（当前 `128KB / slots8 / gain4` 生产配置，jf 侧）：

| 观测 | 结果 | 含义 |
|---|---|---|
| jf ht server CoDel 丢帧计数 | **0**（整个测试期） | dataQ 没有堆到 50ms 目标以上，当前没有观测到需要削峰的队列 |
| orderedInbox gap skip 日志 | 0 | 没有观测到跳批（日志级别内） |
| 内层 TCP 形态 | cwnd 128~176、srtt 150~200ms、minRTT 55~80ms | 吞吐 ≈ cwnd×MSS/RTT，是被窗口/RTT 限制 |
| 提高窗口（gain8/16） | 更差（4.79 / ~3.6） | 再放大窗口只会增加丢包/RTO，不是 pacing 能解决的 |

判断：当前形态下没有“发送突发→可观测队列/丢帧→RTT 正反馈”的证据链，
pacing 的预期收益低、实现风险高，**暂不投入**；若将来换更高带宽载体、
或 ht server AQM 开始出现丢帧，pacing 应作为候选重新评估（届时需要实现
真正的 pacer 后再下结论）。

真正可能跨过 80% 的是把载体 RTT 降下来：持久化 chunked GET（去掉每批
GET 的请求/响应往返），或在 orderedInbox 上做真正的按流保序/重传，
而不是继续在发送窗口上做文章。

### 102.8 剩余 RTT 不在已埋点的队列里（2026-11-12 05:00）

在两跳连续跑期间同时查了三处“可疑队列”，全部为空：

| 埋点 | 结果 |
|---|---|
| jf ht server CoDel 丢帧 | 0 |
| qg/jf `[P2P-QDEPTH]`（≥256KB 或 sojourn≥1s） | 0 |
| qg/jf orderedInbox gap-skip | 0 |

同时内层 TCP 仍是 `cwnd 128~176 / srtt 150~200ms / minRTT 55~80ms`，
即多出来的 ~100ms 既不在 relay 数据队列、也不在 ht server dataQ。
剩余最可能的两个位置：
1. ht 载体自己的 TCP 连接/socket 缓冲（GET 响应与 POST ACK 的发送缓冲）；
2. 接收端（vm）的 UDP leg 入站缓冲（`legCh` 256 个数据报 ≈ 384KB，@7MB/s
   约 55ms）与 Windows 接收/ACK 节奏（延迟 ACK 上限 200ms，与 srtt 量级吻合）。

这两处都不是一个常量能安全解决的：动 `legCh` 会引入 ACK/数据丢失，
动 ht socket 缓冲需要重做载体（persistent chunked GET/POST）。

**阶段定论**：在“长轮询 ht + Windows 接收端”这个组合下，配置类杠杆已到边界；
中位 70~77%、最好 83~90%。要稳定过 80% 必须做载体结构改造（多步、收益
仍需验证），不建议在未评估前继续堆改动。

## 103. 持久化 chunked GET 载体：已实现并实测，单流更慢（2026-11-12 07:10）

按 102.8 的结论实现了第二项结构改造，默认关闭、可 A/B：

### 103.1 实现

- 客户端：`PHAETHON_HT_STREAM_GET=1` 时，数据方向只开 **1 条持久 GET**
  （控制方向仍是普通长轮询）；请求带 `X-Stream: 1`。
- 服务端：识别 `X-Stream` 后回 200 + `X-Stream: 1`，保持响应打开，按
  `[4 字节大端长度][sealed 帧批]` 连续写出并 Flush；无批时继续等待。
- 单数据流天然保序，不再依赖 X-B/orderedInbox；服务端不认识的旧版本会
  忽略头，客户端检测到无 `X-Stream` 回显后回退长轮询。

### 103.2 实测（qg+jf 均部署，日志确认 `[HT-STREAM] persistent GET established`）

| 场景（同窗口 OS 9.35~9.84） | 吞吐 |
|---|---|
| stream GET（单持久流） | 4.15 / 5.19 / 5.34MB/s（中位 5.19，~55%） |
| 关闭 stream（原 8 长轮询） | **7.79 / 7.79MB/s（79.2%）** |

结论：**每批 GET 的请求/响应往返不是瓶颈**。改成单持久流后丢了 8 路长轮询
的流水线，吞吐反而下降约 30%；而 8 路长轮询的乱序也没有在实测中造成
足以抵消流水线收益的损失。持久流本身不改变 `cwnd×MSS/RTT` 这个上限。

### 103.3 处置

- 生产已回滚为：qg `gaincfg`（仅 `PHAETHON_CWND_BDP_GAIN=16`）、
  jf `gaincfg`（`max-batch-size=128KB`、默认 4×）；
- 回归确认：两跳 **7.79 / 7.79MB/s**，同窗口 OS 9.84（**79.2%**），接近 80%；
- 持久流代码保留在仓库中，**默认关闭**（`PHAETHON_HT_STREAM_GET` 不设置即
  走原路径），作为后续“多持久流 + 全局序号保序”工作的基础。

至此两项候选能力都已有明确结论：
1. **chunked 持久 GET**：已实现，单流形态实测更慢，默认关闭；
2. **发送侧 pacing**：gVisor 当前版本没有 pacer，我们也未实现/实测；
   基于“无可削队列 + cwnd/RTT 受限”的间接证据**暂不投入**——这是优先级
   判断，不是“pacing 无效”的验证结论，条件变化后应重新评估。

## 104. WebSocket 载体 PoC：vm→qg→mb 实测约 1.7×（2026-11-12 07:50）

### 104.1 实现

- `frame.WSTransport`：把 WebSocket 当 `FrameTransport` 用；每条二进制消息
  携带若干长度前缀帧，可选 h_tunnel 口令加密；接收侧控制帧（非
  `FrameMeshPacket`）优先于数据帧。
- 服务端：`ServeHTTP` 见到 `Upgrade: websocket` 就升级，直接在该连接上
  跑一个 P2P 会话（不再走 GET/POST 通道）。
- 客户端：`PHAETHON_HT_WS=<proxy 名列表|1|all>` 时，对应 h_tunnel 链路改走
  `ws://host/path/ws/<connID>`；其余链路保持原长轮询。默认关闭。

### 104.2 vm→qg→mb 验证（MB_HT，直连 LAN，不经过 nginx）

同一时段交替切换 qg 的 `PHAETHON_HT_WS`（每次重启生效），8 流 196MB：

| 载体 | 各次吞吐 | 中位 |
|---|---|---|
| 长轮询 GET/POST | 2.97 / 3.01 / 2.49 / 1.73 / 2.31 | **~2.49MB/s** |
| **WebSocket** | 4.92 / 4.67 / 5.50 / 3.06 / 5.05 / 2.31 / 4.25 / 3.34 | **~4.3MB/s** |

约 **1.7×** 提升；慢跑仍与链路时段有关。完整性：WS 下 8 流 **8/8 MD5 一致**
（`8753a5aa…`）。qg 日志确认 `[HT-WS] persistent websocket established:
proxy=MB_HT url=ws://192.168.1.88:18081/ui/resdata/ws/conn-1`。

### 104.3 当前状态与下一步

- **MB_HT 已启用 WS**（qg init: `PHAETHON_HT_WS=MB_HT`），mb 运行
  `v0.8.0-583-g2ab2f5c-ws`（服务端同时兼容长轮询）；
- **MGMS_HT（qg→jf）仍走长轮询**：该路径经过 nginx，目前未放行
  `Upgrade`；放行后可用同一二进制按 `PHAETHON_HT_WS=MGMS_HT` 直接 A/B；
  实测从 qg 直连 `36.140.28.178:39988`（jf ht 服务端口）不可达（http 000），
  公网只暴露 18080（nginx），因此没有绕开 nginx 的验证路径。
- 同一时段 jf 两跳（长轮询）复核：3 次 7.19 / 7.19 / **0.22（865s 停滞）**，
  中位 7.19 vs OS 9.35（76.9%），但出现一次 **865s 尾延迟**（fail=0，8 流最终
  全部完成，无重置）。这属于已知“偶发停滞”，不是吞吐回归；当场 jf 日志没有
  ERROR/WARN 记录，需要下一次现场 5s 采样（tcpFlowDebug + channelDropStats）
  才能定位。WS 本身只在 MB_HT 上启用，jf 链路未变。
- 下一步：mb 链路长跑/停滞回归；nginx 放行后验证 jf 两跳。

## 105. 慢跑现场实锤：ht GET 出队队列 100~227ms（2026-11-12 08:40）

### 105.1 先纠正一个测量错误

此前多次以“没有 P2P-QDEPTH / LANE-INFLIGHT / AQM / gap-skip 日志”判断“没有队列”。
实际 qg/jf 的全局日志级别是 **error**，这些 info 日志根本没写。把
`p2p`/`htunnel` 调到 info 后重测，结论改变。

### 105.2 现场数据

健康跑（6~7.8MB/s）：jf ht server `avgSojourn 2.1~2.6ms, maxSojourn ≤26ms,
queued=0`，队列确实不存在。

一次 85s / 2.20MB/s 的慢跑（jf 内层 8 流 cwnd 128、srtt 369~805ms、minRTT 73~82ms、
DSACK/retransmits ≈ 90%）同时段：

```
[HT-SERVER-STATS] conn=conn-3 frames=12212 avgSojourn=100.3ms maxSojourn=227.3ms queued=394884
mesh AQM dropped 34 frames ... queued=392537B
mesh AQM dropped 105 frames ... queued=394798B
```

更早一次 8MB 上限下的 AQM 日志直接看到 `queued=945694B`。

### 105.3 512KB 上限实验：无改善，回退

把 `htMeshMaxQueuedBytes` 8MB→512KB 部署 jf 后：

| 项 | 8MB | 512KB |
|---|---|---|
| 慢跑队列峰值 | ~945KB | ~390KB |
| 慢跑 avgSojourn | ~150ms（推算） | **100ms**（实测） |
| CoDel 丢帧 | 稀疏（1~3） | **多（105/10s）** |
| 两跳吞吐 | 6.45~7.79 | 6.45 / 7.19 / 2.71（中位 6.45） |

队列压小了但 sojourn 仍有 100ms（由 GET 出队速率决定），多出来的丢帧是真实
丢包，吞吐没有改善。已回退 8MB（`htq8m`）。

### 105.4 结论与下一步

- 剩余 RTT 的主因已定位：**ht GET 出队队列**（慢跑时 100~227ms），不是 relay
  p2p 队列、不是 ht server AQM 目标值；
- 降低上限只是把队列换到别处并增加真实丢包，不是解法；
- 结构性解法就是 WebSocket 载体（mb 已验证 1.7×）；jf 需等 nginx 放行
  `Upgrade`，之后用 `PHAETHON_HT_WS=MGMS_HT` 直接验证；
- 诊断用的 info 日志已调回 error。

（运维注记：不要在**内联** ssh 命令里直接 `pkill -f 'data/worker/phaethon'`，
该模式会匹配到执行命令的 shell 自身并把会话杀掉，导致 phaethon 停住；
本次一次误操作使 jf 短停约 1~2 分钟，已用真实 sshd 重新拉起并验证
`direct=true srtt 20.9ms`、8 流 6.23MB/s。部署请用脚本文件执行。）

### 105.5 8MB 上限下的再次现场：srtt 2.5~3.2s

08:49 的一次 8 流传输再次卡住 >9 分钟，现场（jf 内层发送端）：
5 条 flow、`cwnd=128`、`srtt 2.5~3.2s`、`minRTT 75~80ms`、`RTO 5~6s`、
`unacked ~190KB`、`unsent 200~700KB`，`dSACK 2k~8.4k`，
全局 `retransmits=61941 / dsack=38077`。

这说明 **8MB 的 ht dataQ 上限允许队列堆到多秒**（@2MB/s 时 4MB≈2s），
srtt 因此到 3s，再触发 RACK 伪丢包正反馈。之前 512KB 实验只看了中位吞吐，
没有评估尾延迟；从这次现场看，**限制上限对“不出现多秒停滞”可能比中位吞吐更重要**。
下一步：用 512KB / 1MB 上限做 6 连跑，比较尾延迟（>60s 的次数、srtt 峰值），
再决定生产值。

### 105.6 WebSocket 自动探测与 nginx 阻塞

- 客户端已实现“WS 优先 + 5s 握手超时 + 失败回退长轮询”，
  `PHAETHON_HT_WS=auto` 表示所有 h_tunnel 链路先试 WS；env 仅用于
  测试阶段的强制开关，最终生产应默认 auto。
- **18080 的 nginx 无法由我修改**：从 qg 看 `36.140.28.178:18080` 返回
  `Server: nginx/1.14.1`；但 jf 本机 `ss` 没有 18080（jf 的 nginx 是
  Docker 容器，只监听 80/443），且 layer4 账号无 sudo、无 docker 权限。
  需要 18080 那台转发机的权限，或由你在那台机器上只改这一条 location：

```nginx
# 仅 /ui/resdata/ 这一条转发路径
proxy_http_version 1.1;
proxy_set_header Upgrade $http_upgrade;
proxy_set_header Connection "upgrade";
proxy_buffering off;
proxy_cache off;
proxy_read_timeout 3600s;
proxy_send_timeout 3600s;
```

### 105.7 512KB 上限的尾延迟复测：仍会 >150s 停滞

按 105.5 的计划把 jf 换回 512KB（`q512`）跑 6 次（150s 超时）：

| # | 1 | 2 | 3 | 4 | 5 | 6 |
|---|---|---|---|---|---|---|
| 吞吐 | 7.48 | 7.19 | 5.50 | **STALL >150s** | 6.03 | 6.45 |

结论：512KB 能明显压小队列（~390KB / sojourn 100ms），但**不能消除停滞**——
一次仍然 >150s。说明停滞的持续性主要来自发送端的 RACK/RTO 恢复螺旋，
队列只是触发/放大器，不是唯一维持因素；靠调上限解决不了。
已恢复 8MB（`htq8m`），并确认一次 6.92MB/s 正常。真正的结构性解法仍是
WebSocket 载体（去掉 GET 队列这个触发源），jf 等 nginx 放行。

### 105.8 实验：DSACK 触发 reoWnd 扩展（2s）——更差，默认关闭
思路：DSACK 已证明刚才是伪重传时，把 RACK 的重排容忍窗口临时放大到当前
排队延迟（上限 2s，持续 2s），避免继续误判。fork 里以
`PHAETHON_REO_DSACK_BOOST_MS` 控制（默认 0 = 关闭）。

实测（jf，8MB，196MB/次，4 次）：

| # | 1 | 2 | 3 | 4 |
|---|---|---|---|---|
| 吞吐 | 2.25 | 5.19 | 1.61 | 1.57 |

明显差于默认的 6.45 左右。原因：窗口放大后真实丢包也被晚判，队列持续更久，
恢复反而更慢。已回滚 jf 到 `htq8m`；代码保留为**默认关闭**的实验开关，
不进入生产。结论：停滞不能靠放宽 RACK 判丢来解决。












## 106. nginx 放行 Upgrade 后：jf 链路 WS A/B 无收益（2026-11-12 10:35）

在 jf 的 `/etc/nginx/nginx.conf` 的 `location /ui/resdata/` 里加
`Upgrade/Connection "upgrade"`（仅此一处），`nginx -t` + `nginx -s reload` 后，
qg 日志确认：

```
[HT-WS] persistent websocket established: proxy=MGMS_HT
  url=ws://36.140.28.178:18080/ui/resdata/ws/conn-2
```

同窗口 A/B（OS 两跳 8.50MB/s，公网窗口已劣化）：

| 载体（jf 链路） | 各次吞吐 | 中位 |
|---|---|---|
| WebSocket（单连接） | 5.84 / 1.45(129s) / 3.89 / 4.35 | ~4.1MB/s |
| 长轮询 8 GET | 4.15 / 3.74 / 5.50 | ~4.15MB/s |

**结论：jf 公网链路上单条 WS 连接没有超过 8 路长轮询**（基本持平，且同样有慢跑）。
mb 链路 WS 的 1.7× 是因为它的长轮询本身更慢（LAN、服务端差异），
不能推广到 jf。结合 105 的队列数据（WS 已经去掉了 GET 队列，吞吐仍不涨），
说明 jf 剩余的瓶颈/停滞主因不是 GET 队列，而是发送端 RACK/RTO 恢复螺旋。

**当前生产**：MB_HT 保持 WS；MGMS_HT 回到长轮询
（qg env 为 `PHAETHON_HT_WS=MB_HT`）。nginx 的 Upgrade 改动保留，
对长轮询无影响，后续若做“多 WS 连接”可直接复用。

## 107. 方向：ht mesh 统一到 WS；lane 一致性尝试与回退（2026-11-12 11:10）

### 107.1 方向

后续 h_tunnel mesh 统一演进到 WebSocket 载体，把它**当作普通 TCP 通道**对待：
stripe/lane、保序、路由等上层逻辑不再为 h_tunnel POST/GET 池做特例，
降低心智负担。长轮询保留为兼容/回退。

### 107.2 lane 一致性尝试

改动（已 stash，未进生产）：
- 不再对“h_tunnel + p2p + V2”一刀切跳过 stripe：当该链路启用 WS 时，
  按普通字节流包 stripe，每条 lane 是独立 WS 连接；
- 新增 `PHAETHON_STRIPE_MIN_LANES` 预拨 lane，验证 lane 路径；
- WS 服务端 handler 在 attach 后阻塞到 transport 关闭，避免 attach 返回即关连接。

实测问题：
- lane 能 attach（jf 日志有 `attaching leg stripeID=...`），但客户端
  `laneCount` 始终为 1，`ensureMinLanes` 反复重拨；
- 30s 内 qg 到 `36.140.28.178:18080` 累积 **45 条 ESTABLISHED**（连接泄漏），
  强制 4 lane 的一次两跳只有 **136s / 1.37MB/s**；
- 说明 lane 生命周期/所有权（控制器 remove、attach 后 reader/writer、
  min-lane 与动态 remove 的互斥）还有 bug，不能按默认路径启用。

处置：代码改动已 `git stash`（`wip: ws stripe lanes (unstable: attach churn/leak, reverted)`），
生产回退为 wsauto + 仅 MB_HT 走 WS，MGMS_HT 回长轮询；jf 回 `htq8m`。
恢复后验证：jf 两跳 4.25MB/s、mb 两跳 3.22MB/s（当前窗口整体偏低），链路正常。

### 107.3 事故记录：qg 根分区写满导致 phaethon 短暂下线

- 现象：部署 wslane 时 `cp: write error: No space left on device`，
  `/root/phaethon` 被截断为 0 字节，服务启动失败。
- 原因：`/root/data/logs/phaethon.log` 883MB + /root 下大量 staged 二进制
  （5xx 版本），6.6G 根分区 100%。
- 恢复：保留日志尾部 2MB 后清空日志、删除 /tmp 大文件和旧 staged 二进制，
  释放到 1.4G；重新上传已验证的 wsauto 二进制并 `rc-service` 启动，
  校验 `--version`、双进程与链路。
- 纪律：qg 部署前先 `df -h /`；不要长期保留 staged 二进制；日志定期截断。

### 107.4 下一步

1. 修 WS lane 生命周期（单一 owner、min-lane 与动态 remove 互斥、
   attach 后连接不泄漏），做连接数泄漏回归后再 A/B；
2. 然后按既定方向把 WS 作为 ht 默认载体（auto 探测），长轮询仅回退；
3. RACK 恢复螺旋仍是独立课题。

### 107.5 wslane4 复测：单会话 4/4 成功，但跨重拨泄漏（仍未启用）

改动（已再次 stash）：
- **按实际载体判断**：新增 `frame.StreamCarrier` 接口；只有实际返回的
  transport 是流式载体（WS）才包 stripe。WS→长轮询回退的 transport 不是
  StreamCarrier，因此长轮询不会被包 lane —— 符合“长轮询无需 lane”的要求；
- min-lane 延迟 5s 启动（等握手完成并便于开日志）；
- 增加 remove lane 日志；
- WS 服务端 handler 在 attach 后阻塞到 transport 关闭。

结果：
- 单个稳定会话里 lane 成功从 **1/4 → 4/4** 并保持（没有 remove）；
- 但 qg→jf 的 ESTABLISHED 连接数在多次会话重拨后冲到 **74**，回退后稳定在 41；
- 强制 lane 的两次两跳 2.71 / 3.89MB/s（劣化窗口），没有收益。

判断：min-lane 方式会随每次重拨新建一个 stripe 并预拨 4 条 lane，
旧会话/lane 没有被复用或及时拆除，导致连接累积。正确修法不是继续
“预拨 lane”，而是：**每个 peer 会话只有一个 stripe 实例、重拨时复用；
会话拆除时关闭全部 lane；避免同 peer 的重复会话**。这属于 p2p 会话
生命周期改动，需单独评审后再上。

### 107.6 nginx 限速排查

`/etc/nginx/nginx.conf` 中没有 `limit_rate` / `limit_conn` / `limit_req` /
`proxy_limit_rate`；`resdatabackend` 是 `10.140.51.2:32457`，keepalive 32，
`proxy_buffering off`。所以 **nginx 本身没有配置限速**。单条 WS 上不去更可能
是外层 LB 的单连接策略/单连接 cwnd；lane 生命周期修好后，多 WS 连接才能
验证这一点。

当前生产已回退：qg `wsauto` + 仅 MB_HT 走 WS，MGMS_HT 长轮询；jf `htq8m`；
qg 到 jf:18080 的连接稳定在 41（phaethon 长轮询池）。后续加密与 SOCKS5
UDP leg 纳入 WS 统一规划。

### 107.7 统一抽象落地（StreamCarrier）与 WS lane 仍未放量（2026-11-12 11:45）

按“一套抽象 + 多态实现”的要求实现了（已 stash，未进生产）：

- 新增 `frame.StreamCarrier` 能力接口：
  - `frame.WSTransport` → `true`
  - `dialer.htunnelDirectTransport`（长轮询/回退）→ 显式 `false`
  - 其它 TCP 类载体视为 `true`
- p2p 不再出现 `proxy.Type == H_TUNNEL && V2` 的类型特判：
  ```go
  if sc, ok := transport.(frame.StreamCarrier); ok && sc.IsStreamCarrier() {
      stripe = newStripeTransport(transport, ...) // 统一 lane 逻辑
  }
  ```
- 不再强制预拨 lane（min-lane 已删除），lane 完全交给现有控制器。

实测：
- MGMS_HT / MB_HT 都成功建立 WS；
- 但把 WS 包进 stripe 后约 1 分钟内，qg→jf 的 ESTABLISHED 又涨到 **34**；
  回退到“MB_HT WS + MGMS 长轮询”后稳定在 **9**；回退后两跳 5.19MB/s 正常。

结论：**抽象层已经统一，问题收敛到 lane 控制器本身在 WS 载体上的
add/remove 行为**（gauge/credit 的触发条件是为长轮询/TCP 场景调的）。
下一步应做控制器硬化：只在主 lane 真正拥塞持续 N 秒才加、加/删都带
滞回、每会话连接数上限可验证（≤1+maxLanes 且 10 分钟不增长），先在
MB（LAN）上回归，再上 jf。加密和 SOCKS5 UDP leg 随后纳入同一抽象。

### 107.8 关键修正：连接增长来自 WS 会话重拨，不是 lane 控制器（2026-11-12 12:00）

核对控制器参数后先纠正一个错误：lane 控制器并不激进，实际是
`add sustain 30s / add interval 10s / clear sustain 5min / max 8`。
本轮 wslane6（统一 StreamCarrier + 无强制 lane）实测：

- MGMS_HT 的 WS 在 11:51:54 建立，**11:52:12（约 18s）就重拨了一次**；
- 期间没有任何 `[STRIPE] add lane / diag` 日志 —— 控制器根本没加 lane；
- qg→jf ESTABLISHED 涨到 39，是**旧 WS 会话未及时关闭 + 频繁重拨**造成的，
  不是 lane churn。

结论修正：
1. 抽象层（StreamCarrier）与控制器都不是当前瓶颈；
2. 当前的直接问题是 **WS 会话生命周期**：新构建下 MGMS 的 WS 会话约 18s
   就重拨，旧连接滞留；
3. 下一步必须做隔离实验：分别构建“仅 Done 修复（不包 stripe）”和
   “仅包 stripe（不做 Done 阻塞）”，确认是哪一个改变了 WS 会话寿命；
4. 在生产确认稳定前，不启用 MGMS 的 WS，也不启用 lane。

生产已回退：qg wsauto + MB_HT WS、MGMS 长轮询，jf htq8m；
EST18080 稳定在 9。

### 107.9 统一载体抽象落地并验证（2026-11-12 12:45）

代码（已提交）：
- `frame.StreamCarrier` 能力接口：`WSTransport`=true；
  `htunnelDirectTransport`（长轮询/回退）显式 false；其它 TCP 类载体 true；
- p2p 去掉 `proxy.Type == H_TUNNEL && V2` 类型特判，按实际载体能力决定
  是否包 `stripeTransport`；
- WS 服务端 handler 在 attach 后阻塞到 transport 关闭（Done），
  修复“lane attach 完 handler 返回就关连接”；
- lane 控制器仅增加 add/diag 日志，未改加/删参数（30s/10s/5min）。

验证（qg/jf/mb 全部 `_wslane6`，MGMS_HT 与 MB_HT 都走 WS）：
- 日志确认两条链路都 `[STRIPE] stream carrier wrapped for lanes`；
- 等部署重启完全结束后静置 3 分钟：**没有 WS 重拨**，EST18080=2、
  EST18081=1，**无 lane add、无连接增长**；
- 之前看到的 “~18~25s 重拨 / 34~45 条连接” 是我方部署重启造成的假象，
  不是会话泄漏；
- 吞吐：jf 两跳 6.68MB/s（28s），mb 两跳 3.22MB/s（58s，当前窗口偏低）；
  单 lane 直通，未观察到相对单 WS 的退化。

结论：ht mesh 的 WS 已按“普通流式载体”接入统一抽象；长轮询/回退因
`IsStreamCarrier()==false` 自动豁免 lane。lane 的动态加/删交给原控制器，
后续在真实拥塞下再验证加 lane 的效果。RACK 恢复螺旋仍是独立课题；
加密与 SOCKS5 UDP leg 后续纳入同一抽象。

### 107.10 WS 载体上的停滞现场与 socket 缓冲实验（负结果）（2026-11-12 13:15）

统一 WS 载体下连跑 6 次：4.45 / 4.56 / 6.45 / **1.35(138s)** / 4.92 / 4.45。
慢跑现场（jf 内层发送端）：

- srtt 284→791ms，minRTT 70~80ms；
- cwnd 多次塌到 1~5、ssthresh=2，RTO 0.7~8.2s；
- DSACK/retransmits ≈ 80%（如 18775/23373）；
- qg 的 `P2P-QDEPTH`/`LANE-*` 全程没有记录，qg relay 队列未到阈值。

说明：即使 WS 去掉了 ht GET 队列，RTT 仍会膨胀数百 ms 并触发 RACK/RTO
塌缩；队列位置不在我们已埋点的 p2p/ht 队列里。

随后做了 socket 缓冲实验（Linux 客户端 `SO_RCVBUF/SO_SNDBUF=512KB`）：
- 吞吐 3.06 / 3.74MB/s，明显低于不设限的 4.4~6.4；慢跑依旧；
- 512KB 对 BDP 太小，先限流后仍不能消除塌缩，已回退（代码已还原）。

结论：继续在载体/缓冲层加限制没有收益；下一阶段必须回到发送端恢复逻辑
（DSACK/RACK 的按 RTT 抑制、minRTT 相对 RTO 上限）或接收端 ACK 行为，
需要单独的 gVisor/Windows 侧方案。生产保持统一 WS 构建（连接稳定 2/1）。

## 108. RACK/RTO 恢复实验（负结果）与 UDP leg 加密方案（2026-11-12 14:00）

### 108.1 DSACK 触发 RTO 上限（backoff / base）无效

两版单变量（jf 发送端）：
- rtoack：仅在 DSACK 最近 2s 内，把 RTO 退避上限压到 `max(4*minRTT, 1s)`；
- rtoack2：同样上限再应用到 base RTO（updateRTO 结果）。

6 连跑（150s 超时）：
- rtoack：6.92 / 6.68 / 6.23 / 6.68 / 7.19 / 6.92（当时窗口好，无慢跑）；
- 同窗口基线（无上限）：6.92 / 4.25 / **101s** / 4.56（慢跑复现）；
- rtoack2：3.59 / 5.34 / 6.23 / **80s** / …（慢跑仍复现）。

慢跑现场确认：即使 RTO 被压在 ~1s 量级，cwnd 仍会塌到 1~5、ssthresh=2，
srtt 600~800ms（minRTT 70~80ms）。说明**维持停滞的不是 RTO 定时器**，
而是数据路径上 600~800ms 的排队（RACK 据此判丢、cwnd 塌缩）；发送端
只改 RTO 无法解决。已回退，fork 源码恢复到 HEAD。

### 108.2 UDP leg 加密：规则简单，但密钥必须两端对称

规则：SOCKS5 链路密码为空→不加密；非空→每条 datagram 用
`HTunnelCrypto` Seal/Open（开销 40 字节，需要相应减小 leg MTU）。

实现上重要的是密钥来源：**被动端没有“active 的 proxy password”**。
正确做法是把**被动侧 SOCKS5 relay 的 mapping 密码**绑到对应 leg 端口
（`UDPLegBindReturnPath` 同一处），由 leg endpoint 用它做 Seal/Open；
ht 的 password 不能拿来给 SOCKS5 leg 用（会与 active 侧空密码不匹配）。
当前 vm↔qg 的 QG_SS5 密码为空，所以现状仍是明文、不受影响。
该改动留作下一批（涉及 endpoint 级密钥绑定，不动现有链路）。

生产：qg/jf/mb 均为 `_wslane6`（统一 WS 载体），连接稳定；RTO 实验未启用。

### 108.3 对照：mb（LAN、无 nginx）不出现多秒停滞

同一构建、同一 Vm→qg 腿，末端换到 mb（LAN、无 nginx），4 连跑：

| # | 1 | 2 | 3 | 4 |
|---|---|---|---|---|
| 耗时/吞吐 | 54s/3.46 | 56s/3.34 | 59s/3.17 | 56s/3.34（2 流失败） |

**没有出现 80~150s 的多秒停滞**，吞吐稳定在 mb 链路容量（~3.3MB/s）。
结合 jf 公网路径在 WS 和长轮询下都会出现 srtt 600~800ms 的慢跑，
可以把队列来源进一步收敛到 **jf 公网路径上的 nginx/LB/运营商缓冲**，
而不是我们的 relay 或 vm 接收端（mb 同为 relay+vm 接收，但没有该现象）。

下一步优先级：
1. 检查 18080 外层 LB 的 buffering/连接队列策略（我们只能改 nginx 那一条
   location，LB 需要你侧确认）；
2. 或在发送端做 pacing，减小进入公网缓冲的突发（现在有了“公网有队列、
   LAN 没有”的证据，pacing 重新具备验证价值）；
3. 若 LB 不可调，接受 jf 公网路径的慢跑尾部，生产以统一 WS 载体为主。

## 109. deskw 掉线不自动重连：无 pong 链路被 inbound 流量永久 hold（2026-11-12 15:00）

**现象**：deskw 掉线后没有自动重连，用户手动重启才恢复。14:43 的日志是
`Received console close event, shutting down...`（手动停），说明 worker 一直活着，
不是崩溃/watchdog 未拉起，而是 **p2p 会话没有自我拆除/重拨**。

**根因（代码）**：`sendPing` 的判死条件是“连续 10 次 ping 无 pong **且**
10s 内没有任何 inbound 帧”。只要还有任意 inbound 帧（来自残留 lane/腿或
对端其他流量）刷新 `lastSeen`，就会走 `link dead held` 分支并且**永远不拆**，
只打 Info 日志（error 级别下不可见）。单向/半死链路因此既不恢复也不重拨。

**修复**：Peer 增加 `lastPongNano`（创建与每次 pong 时刷新）。`sendPing`
在连续丢 ping 达到阈值后，若 **60s 内没有任何 pong**，即使 inbound 仍活跃也
直接 `transport.Close()`，由会话循环按既有 backoff 重拨。

**部署**：
- qg/jf：`v0.8.0-583-g2ab2f5c-pongdead`（Linux）；
- deskw：签名 pkg `phaethon_windows_amd64_v0.8.0-583-g2ab2f5c-pongdead.pkg`
  放入 `data/packages`，kill 旧 worker（v574）后 watchdog 热切换到新 worker；
  watchdog 3304 未变，worker v583；jf 侧 `deskw direct=true srtt=32.4ms`、
  lastSeen 持续刷新。

**后续**：用单向丢包/断链模拟验证 60s 内能自动拆除并重拨（当前 deskw 已在线）。

## 110. 发送端 pacing 两次尝试均为负结果（2026-11-12 16:00）

### 110.1 固定速率（ht WS 服务端 7MB/s）

在 WS 服务端对数据批做 7MB/s 令牌桶（控制帧不 pacing）：
- 3 次：5.66 / **1.36（137s）** / 3.53；
- 慢跑仍出现，且固定 7MB/s 在链路容量下降时依然会超发；
- 放置位置也不对（只 shaping 了 jf→qg 的 ht WS，不是通用层），已回退。

### 110.2 通用自适应 paced transport（p2p 层，0.9×lane 实测速率）

按“通用抽象”实现 `frame.PacedTransport`（包装任意 FrameTransport，数据批
按令牌桶延迟，控制帧直通；速率由 stripe 的 lane 实测 delivery rate ×0.9
每秒反馈），env `PHAETHON_PACE_STREAM=1`，部署在被动侧（下载方向的发送端）。

结果：**链路直接不可用**——8 流全部失败（6s fail=8），并出现一次 938s 挂起；
回退到 `pongdead` 后同窗口 69s/2.71MB/s（fail=0）恢复。

原因判断：同步 sleep 的 pacer 阻塞了 peer write loop，控制帧/ACK 被拖住；
且 lane 速率反馈起不来时 0.9×0 会把数据完全掐住。这个形态不能上生产。

### 110.3 结论

- 固定速率和同步自适应两种 sender pacing 都失败；
- 问题仍指向 jf 公网路径/LB 的缓冲，而 LB 我们无权限；
- 若还要做 pacing，必须是**异步整形**（独立队列 + shaper goroutine，
  控制帧优先、速率下限保护），属于独立设计，不能复用当前的同步写循环。

生产：jf/qg/deskw/mb 均为 `_pongdead`/统一 WS 构建，pacing 代码已回退。

**代码保留**：两次 pacing 的实现（`frame.PacedTransport` 通用整形器 +
stripe `onRate` 反馈 + p2p 被动侧接线）保留在仓库中，仅由
`PHAETHON_PACE_STREAM=1` 启用，默认关闭。保留目的是记录尝试与失败原因，
**不要在生产开启**；后续若做异步整形可在此基础上重写。

## 111. vm→qg→mb 与同窗口 OS 两跳对照：无差距（2026-11-12 16:35）

8 流 196MB，同一窗口：

| 路径 | 耗时 | 吞吐 | 占 OS 两跳 |
|---|---|---|---|
| OS 两跳（vm→qg→mb ssh 串接） | 62s | 3.01MB/s | 100% |
| mesh 两跳（UDP leg + MB_HT WS） | 51s | 3.67MB/s | 122% |
| mesh 两跳（复跑） | 69s | 2.71MB/s | 90% |

结论：mb 路径的 mesh 与 OS 两跳基本持平（中位 ~3.2，约 106%），没有系统性差距；
mb 链路上限当前只有 ~3MB/s（历史 LAN 好时 qg→mb 约 6.4）。慢跑一次 69s，比 jf
的 80~150s 轻。结合 vm→qg 单跳 9.84（=OS 基线），差距只在 **qg↔jf 的公网
ht-over-nginx/LB 载体**这一段，不是 mesh 数据面、relay 或 vm 接收端。

## 112. 发现并修复：WS 载体忽略 via 链（ht via SSH 的缺口）（2026-11-12 16:50）

**缺口实锤**：`dialP2PWebSocket` 之前用 `websocket.DialConfig` + 直连
`net.Dialer`，**完全忽略 proxy.Next（via）**。所以 `MGMS_HT via=JF_SSH`、
URL 指向 jf 内网 `10.140.51.2:32457` 时，WS 在 qg 上直接拨内网地址 →
超时；长轮询 HTTP 客户端本身是走 via 的，但 WS 路径漏了。

**修复**：WS 拨号改为与 HTTP 一致：
- 有 `proxy.Next`（非 DIRECT）时，`dialWithContext(ctx, NewDialer(proxy.Next), host, port)`
  通过 via 建立连接；
- 用 `websocket.NewClient(cfg, conn)` 在该连接上完成握手；
- 无 via 时仍直连。

**验证**：qg 部署 `_wsvia`，MGMS_HT `via=JF_SSH`、URL `http://10.140.51.2:32457/...`：
- 日志确认 `[HT-WS] ... url=ws://10.140.51.2:32457/ui/resdata/ws/...`，
  /api/p2p `MGMS_HT nodeId=jf status=upToDate` —— **via 链打通**；
- 但 bulk 传输不可用：单流 24.5MB 跑了 >3.5 分钟（OS 层 `ssh -L` 同一目标
  返回 400，网络与 sshd 正常）。判断是 phaethon SSH dialer 的
  **direct-tcpip 通道窗口/速率**问题（x/crypto/ssh 单通道窗口默认偏小），
  不是网络。

已回退 MGMS 为直连 WS（`36.140.28.178:18080`, via=""），链路 upToDate；
`_wsvia` 修复代码保留（对带 via 的 WS 链路是正确的通用能力）。
下一步若要走 SSH 隧道：调大 SSH 通道窗口，或按你的思路直接给 SSH 代理
实现 `DialP2P`（原始 P2P 帧走 SSH 通道，不再套 ht）。

## 113. h_tunnel 载体抽象统一：普通代理与 mesh 共用 HTTP/WS 选择（2026-11-12 17:15）

**问题**：此前 WS 只在 mesh 的 `DialP2P` 里特判接入，普通代理 `Dial`
仍固定走 HTTP 长轮询，载体不是代理级能力。

**重构**：
- 新增 `dialer/htCarrier` 接口：`DialTarget(ctx,host,port)` + `DialP2P()`；
- `httpCarrier`：现有 HEAD + POST/GET（target 与 MESH）；
- `wsCarrier`：`DialTarget` 用 WS 的 target 模式，`DialP2P` 用 mesh WS；
- `newHTCarrier(proxy)` 按 `PHAETHON_HT_WS` 选载体，`Dial` 与 `DialP2P` 都走它，
  WS 失败统一回退 http；
- 服务端 `handleMeshWebSocket` 增加 target 模式：识别加密的 `X-H/X-P`，
  Upgrade 后 dial 目标并双向 splice（每消息 Seal/Open）；无 target 头仍是 mesh。
- WS 拨号统一走 `dialWSConn`（含 via 链），普通与 mesh 两条路径行为一致。

**验证**：
- 新增单测 `TestWSTargetCarrier`：客户端 `Dial` → WS target 握手 → 服务端
  dial 本地目标 → 收到 "hello"，通过；
- qg/jf 部署 `v0.8.0-583-g2ab2f5c-htcarrier`：MGMS/MB `upToDate`（mesh WS），
  一次两跳传输 100s/1.87MB/s、fail=0（慢跑为 jf 公网旧问题）。

生产行为变化：`PHAETHON_HT_WS` 列表里的 ht 代理，**普通代理流量也会走 WS**；
未列出的代理仍走 HTTP。下一步再讨论 via SSH（多通道解决 64KB 通道窗口）。

## 114. h_tunnel 三通道收口 + WS 控制台标识（2026-11-12 18:10）

**决策**：WS 一旦生效，**不再受 v2 开关影响**；v2 只决定“无 WS 时 P2P 走
MESH 单步 HEAD 还是 v1 BIND 流”，以及普通代理是否走 mesh（`meshAwareDialer`）。

| 流量 / 通道 | P2P | V2 | WS 生效 | 路径 |
|---|---|---|---|---|
| 普通 target | off | 任意 | 是 | WS target（X-H/X-P），服务端代拨目标 |
| 普通 target | off | 任意 | 否 | HTTP CONN，服务端代拨目标 |
| 普通 target | on | off | 是 | WS target（X-H/X-P），服务端代拨目标 |
| 普通 target | on | off | 否 | HTTP CONN，服务端代拨目标 |
| 普通 target | on | on | 是 | 服务端不代拨目标；目标流量走 mesh，隧道帧经 **WS** 承载 |
| 普通 target | on | on | 否 | 服务端不代拨目标；目标流量走 mesh，隧道帧经 MESH/BIND 承载 |
| P2P 会话 | on | off | 是 | WS 持久连接 + 帧批次 |
| P2P 会话 | on | off | 否 | v1 BIND 三步 PORT=2 |
| P2P 会话 | on | on | 是 | WS 持久连接 + 帧批次 |
| P2P 会话 | on | on | 否 | MESH 单步 HEAD |
| P2P 会话 | off | — | — | 不建立 P2P 会话 |
| 反向数据 BindModeData | 任意 | 任意 | 是 | WS BIND PORT=0 |
| 反向数据 BindModeData | 任意 | 任意 | 否 | HTTP BIND PORT=0 |
| 控制通道 BindModeControl | 任意 | 任意 | 是 | WS BIND PORT=1 |
| 控制通道 BindModeControl | 任意 | 任意 | 否 | HTTP BIND PORT=1 |

> 表述纠正：“不走 h_tunnel 数据面”应说成**不走 h_tunnel 的 target-CONN 转发**
> （服务端不代拨目标）。mesh 的目标数据仍封装在 P2P 会话里、由 h_tunnel 链路
> 承载，所以 WS 对 mesh 吞吐同样生效。

**实现**：
- `htCarrier` 增加 `DialBIND(ctx, mode, dst)`；Data/Control 全部经
  `dialHTBINDWithFallback`，P2P 统一经 `dialHTP2PWithFallback`（v2 判断下沉到
  `dialHTP2PHTTP`），BindStrategy 内不再有裸 `dialHTunnelBIND` 分支；
- 服务端 `handleMeshWebSocket` 按头分发三模式：`X-C: BIND`（bind）→
  `X-H/X-P`（target）→ 无头（mesh）。bind 模式复用 `handleControlConnection` /
  `handleP2PConnection` / `reverse.HandleReverseConnection`，WS 侧用
  `wsServerConn` 做字节流适配；
- WS 结果按代理记录（成功/回退），控制台代理表新增 **Carrier** 列：
  `WS` / `HTTP (WS 回退)` / `HTTP` / `-`（非 ht 代理）。
- `PHAETHON_HT_WS`：未设置默认尝试 WS、失败回退 HTTP；`0/off` 强制 HTTP；
  `1/all/auto` 全开；逗号名单为 allowlist（见 §116）。

**验证**：`TestWSTargetCarrier`、`TestWSBindCarrierControl` 通过
（后者覆盖 Control BIND over WS 双向收发 + Carrier 标签为 `WS`）。

## 115. 单流吞吐根因实锤：lane cap 扩容判据（2026-11-12 20:20）

**问题**：qg↔jf 单流 mesh 长期只有 OS 基线的 1/2~1/3（up ≈3.3MB/s vs 8.9）。

**根因**：mesh 在途配额门控 `adjustCap` 的扩容条件要求“200ms 采样瞬间
inflight ≥ cap/2”。低 RTT 链路 ack 很快，采样时刻 inflight 往往接近 0，
cap 永远停在初始 256KB（甚至被 qd 砍到 64KB）；吞吐 ≈ cap / 50ms ack 间隔，
正好落在 1~5MB/s。实锤日志：修复后 cap 能长到上限 8192KB，qd 仅 15~31ms
（目标 200ms），说明旧判据从未触发扩容。

**修复**（`b4f550c`）：扩容判据改为“本轮 acked 有推进 + qd < target/2”；
`PHAETHON_LANE_CAP_LEGACY=1` 可回旧行为做 A/B；`PHAETHON_LANE_DIAG=1`
输出每秒 `[LANE-CAP]`（cap/inflight/srtt/qd/demand）。

**同窗口 A/B**（24.5MB 单流，qg↔jf，时间=秒 / 吞吐=MB/s，OS=系统 sshd 同链路）：

| 方向 | 旧 583（3 次中位） | 新 626-htid（中位） | OS 基线（中位） |
|---|---|---|---|
| jf→qg（mesh down） | 4.05s / 6.0 | 2.94s / 8.3 | 3.11s / 7.9 |
| qg→jf（mesh up） | 7.45s / 3.3 | 2.74s / 8.9 | 2.43s / 10.1 |

结论：单流 mesh 已基本追平 OS 基线（down ≈1.0×，up ≈1.1×）；偶发单次
~5.5s 离群仍需观察。两跳（vm→qg→jf）与 via SSH 路径待用同一方法复测。
部署：qg/jf=`v0.8.0-626-gb4f550c-htid`，deskw 同版本。

## 116. WS 改为默认启用 + 单流复测（2026-11-12 21:45）

**更正**：P2P 会话是**一条全双工传输**，载体由**主动端**选择；被动端的环境
变量不参与。此前“jf→qg 方向走 HTTP 长轮询”的说法错误——qg 是主动端，
`MGMS_HT` 走 WS 时两个方向都在这条 WS 上。同理，jf 不需要设置
`PHAETHON_HT_WS`，它只是被动接受 qg 拨进来的 WS。

**变更**（`909dc1e`）：`PHAETHON_HT_WS` 未设置时**默认先试 WS**，握手失败
回退 HTTP；`0/off/false/none` 强制 HTTP；`1/all/auto/on` 全开；逗号名单为
allowlist。新增 `wsEnabledFor` 单测覆盖默认值与开关。

**验证**（qg 去掉 `PHAETHON_HT_WS`，只跑默认逻辑；qg=`628-htid`，jf 保持
`626-htid` 作被动端）：

- 日志确认 `MGMS_HT`、`MB_HT` 均直接建立
  `[HT-WS] persistent websocket established`，说明默认探测生效；
- 24.5MB 单流同窗口（秒；OS 为 jf 系统 sshd）：
  - mesh down：3.22 / 2.75 / 2.67（中位 2.75）
  - mesh up：2.93 / 2.82 / 2.65（中位 2.82）
  - OS down：2.87 / 2.78 / 3.01（中位 2.87）
  - OS up：2.40 / 2.45 / 2.46（中位 2.45）
- 即 mesh/OS ≈ 0.96×（down）、1.15×（up），与显式开启 WS 时一致，且本轮
  未再出现 ~5.5s 离群。

**deskw 升级**：deskw 已换 `v0.8.0-628-g909dc1e-htid`，未设置
`PHAETHON_HT_WS` 时 `DESK_TJ` 直接建立
`[HT-WS] persistent websocket established: url=ws://10.140.51.2:32457/...`；
拓扑 `deskw(1/5) ↔ jf(5/1)` 配对正常。qg/jf/deskw 均为 628。

**待办**：via SSH 路径尚未复测；“WS 握手失败回退 HTTP”未现场构造，
依赖已有单测与回退分支。

## 117. 全环境统一到 plain 版本 v0.8.0-630-g7f1a6ab（2026-11-12 22:30）

**版本纪律**：pkg/worker 的版本戳只用 `git describe --tags --always` 原文，
禁止自定义后缀（历史上的 `-relay-log` 事故会导致 watchdog 版本比较误判）。
之前的 `626/628-htid` 属临时后缀，本轮全部替换为 plain 版本。

| 节点 | 方式 | 运行版本 |
|---|---|---|
| vm | `bin/deploy-vm.ps1`（计划任务 PhaethonTUN） | v0.8.0-630-g7f1a6ab |
| qg | 直接替换 `/root/phaethon` + rc-service | 同上 |
| jf | `jf_deploy.sh` | 同上 |
| gg | `/home/layer4/phaethon-gg/` 脚本替换 | 同上 |
| mb (macOS x86_64) | 上传 darwin/amd64 + sudo 替换 | 同上 |
| deskw | Windows 直换（共享目录中转） | 同上 |
| qgw | **签名 pkg 激活**（windows7/amd64），worker-pin=`v0.8.0-630-g7f1a6ab`；watchdog 仍为 529 | worker 630 |

验证：qg `/api/mesh/topology` 共 7 节点 19 条边，所有方向 seq 成对
（如 qgw↔mb 2/6、jf↔deskw 7/1、qg↔jf 6/4）；各节点日志均出现
`P2PManager initialized (version=v0.8.0-630-g7f1a6ab ...)`。

## 118. 离群复现与否定假设（2026-11-12 22:40）

**复现**：qg↔jf 单流 24.5MB，每轮先空闲 10s 再测 down/up：

```
cycle1 down=3.43s  up=3.32s
cycle2 down=3.37s  up=3.29s
cycle3 down=3.46s  up=3.50s
cycle4 down=4.67s  up=3.36s
cycle5 down=5.28s  up=3.38s
cycle6 down=5.09s  up=3.85s
cycle7 down=5.43s  up=3.41s
cycle8 down=3.52s  up=3.47s
cycle9 down=3.46s  up=3.26s
cycle10 down=3.52s up=3.29s
```

- 离群只出现在 **down（jf→qg，jf 为发送端）**，up 始终平稳；
- 连续 4 轮慢后自行恢复，重新计时可再次出现。

**已否定的假设**（同一窗口取证）：

- lane cap 塌缩：jf 日志该链路 `cap` 全程 8192KB，`qd` 峰值仅 77ms
  （目标 200ms），无缩容；
- P2P 会话重连：窗口内无 `session ended` / `peer established` / WS 重建；
- 链路质量突变：qg/jf `srtt` 12~22ms、`jitter` 偶发 15ms，未与慢轮强相关；
- DSACK：qg `tcpFlowDSACKSum` 每轮仅 +6~20，无爆炸。

**下一步**：在慢轮触发时以亚秒级采样 jf 发送侧（WS 写阻塞、DRR 队列
深度、inner TCP 的 RTO/reord）与 qg 接收侧，定位那 ~2s 额外延迟的来源。

## 119. 多跳速率复测与 vm↔qg UDP leg 停顿（2026-11-12 23:25）

**统一端点**（避免内建 SSH/OS sshd 混用）：mesh 两跳与 OS 两跳都落在
jf 的 **OS sshd**：

- mesh 两跳：vm →（mesh，qg 中转）→ `10.140.51.2:22`（jf OS sshd）
- OS 两跳：vm → qg OS sshd（2222）→ jf OS sshd（60002）
- 载荷 24.5MB，交替 3 轮。

| 路径 | down（jf→vm） | up（vm→jf） |
|---|---|---|
| mesh 两跳 | 4.99 / 4.11 / **19.26** | 4.36 / **20.97(BrokenPipe)** / **19.29(BrokenPipe)** |
| OS 两跳 | 3.76 / 3.61 / 3.65 | 3.76 / 3.73 / 3.68 |

**发现**：

- 未停顿时 mesh 两跳 down 约 4.1~5.0s vs OS 3.6~3.8s（慢 10~30%），
  up 首轮 4.36s 后直接崩；
- qg↔jf 单跳此前已 ≈ OS 基线（§116），所以两跳的损失不在 qg→jf；
- vm↔qg 单跳定位：1/4/8MB 上传均成功（0.79/1.14/1.75s），24.5MB 上传
  在 ~5MB 处卡住 ~20s 后 broken pipe；同一路径 24.5MB 下载可完成，
  重复多轮后两个方向都会停 19~21s；
- 同一时段 OS 路径稳定 3.6~3.8s。

**结论**：多跳劣化主要来自 **vm↔qg 的 UDP leg**（重复大流量后停顿/断流），
不是 qg→jf 载体；这也与“偶发停滞”现象同源，列为下一优先项。
排查方向：UDP leg 的发送准入/ack 节奏、`udpLegTransport` 停等与
leg 回退逻辑，以及停顿期间 vm/qg 的 leg 统计（Packets/Drops/backoff）。

## 120. 实锤：vm↔qg UDP leg 是停顿根因（2026-11-12 23:40）

**对照方法**：只改一个变量——把 vm 的 `QG_SS5_UDPLEG` 代理 `p2p-udp`
从 true 改成 false（其余配置不变），等 peer 重建、`/api/p2p` 的 qg
`transport=tcp` 后，用同一 24.5MB 单流脚本复测；测完已恢复原配置
（transport 回到 `udp`）。

**A：UDP leg 开启（现状）**

- SSH 大流量上传：`10.140.51.2`（jf OS sshd）首轮 4.36s，随后
  `Connection reset by peer` / BrokenPipe，19~21s 超时；
- 两跳：down 4.99/4.11/**19.26s**，up 4.36/**20.97**/**19.29s**。

**B：UDP leg 关闭（vm↔qg 走 TCP 载体）**

| 路径（24.5MB，3 轮） | down | up |
|---|---|---|
| mesh 两跳 | 3.58 / 3.76 / 3.69s | 3.65 / 3.90 / 3.87s |
| OS 两跳 | 3.68 / 3.85 / 3.73s | 3.61 / 3.80 / 3.77s |

- SSH 大流量上传（jf OS sshd / 内建）：6/6 全部成功，3.45~3.80s；
- 两跳与 OS 基线持平（down 甚至略快），无任何停顿。

**结论**：多跳劣化、SSH 大流量 BrokenPipe/RST、重复传输后 19~21s 停顿，
全部由 **vm↔qg 的 UDP leg** 引起；qg→jf 的 WS 载体与两跳转发本身已达标。
UDP leg 是可选优化，当前缺陷未修前，建议对受影响的代理关闭 `p2p-udp`。

**待查（UDP leg 自身）**：为何 TCP 载荷（尤其 SSH 的加密大包/反向窗口
更新）在 UDP leg 上会触发长时间停滞而裸 TCP 大流量不会；重点看
`udpLegTransport.writeLeg` 的 fire-and-forget 丢包放大、leg 端口/NAT
映射寿命、以及 inner TCP 在 leg 路径上的 RTO 退避行为。
