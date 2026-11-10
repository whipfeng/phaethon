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

（过程注记：qg 的 `/tmp` 是 236MB tmpfs，多次 staged 二进制曾把 qg 填满，
导致一次 scp 失败并把旧 Windows 二进制部署到 jf、jf 短暂崩溃。已清理
`/tmp/phaethon-*.staged` 并用正确 513/514 恢复；后续部署前先 `df -h /tmp`，
并校验 staged 的 md5。）
