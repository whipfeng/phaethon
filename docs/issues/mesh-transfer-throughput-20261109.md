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
