# SSH 经 GG 代理入口访问 VM：批量流量下输入卡顿（2026-11-15）

## 现象

从 GG 的代理入口访问 VM 时，SSH 交互偶尔“输入没反应”。经实机复现，触发条件是**同一条 mesh 链路上有批量流量**（例如经同一入口上传大文件），不是单纯的 idle。

## 复现环境

- GG 本机 → trojan 入口 `127.0.0.1:39988`（SNI `www.dynamictj.com`）→ `172.30.0.2:22`（VM 的 OS sshd）
- 路径：`gg → jf → qg → vm`，三跳，IPIP 通告路由
- 交互流：一个 PTY SSH 会话，每秒发送 `MARK_n` 并等待回显
- 批量流：300MB 上传 `dd if=/dev/zero bs=1M count=300 | ssh ... 'dd of=/dev/null'`

## 测量结果

| 场景 | 交互输入回显延迟 |
| --- | --- |
| 空闲 | ~100ms |
| 300MB 批量上传并发 | min 164ms / p50 498ms / p95 1370ms / max 1385ms |

批量上传期间 `[P2P-QDEPTH]`：

```
peer=5 proxy= queued=0KB drrOut=77 ctrl=0 maxSojourn=1.124s
writes=230 busy=3ms maxWrite=0s rate=129.2KB/s
```

- `maxSojourn=1.124s`：帧从入队到写出等待了 1.1 秒；
- `maxWrite=0s`、`busy=3ms`：实际写 socket 并不慢；
- `queued=0KB`：DRR 数据队列本身没有积压；
- 说明帧卡在 **peerWriteLoop 的在途 credit 门控**，不是网络或 socket 写阻塞。

## 根因

`p2p.peerWriteLoop` 使用 **peer 级**的在途 credit（`stripe.writeCredit()`）。批量流把 credit 用满后，write loop 停止消费 `drrOutput`，所有数据流（包括交互 SSH 的小包）一起等待；等 credit 恢复时，交互包前面已经排着批量流的在途窗口，形成 1 秒级卡顿。控制/DNS 有旁路，但交互数据没有。

## 修复

新增 `smallCh` 小数据优先队列：

1. `isSmallInteractiveData` 识别 SSH（端口 22）的**小数据**帧（≤512B、非纯 ACK）；IPIP 按内层包判断。
2. `drrLoop` 把小数据帧投递到 `smallCh`；队列满时回退 `drrOutput`，不阻塞调度器。
3. `peerWriteLoop`：
   - credit 充足时，批次顺序为 control → DNS → small → bulk；
   - credit 耗尽时，仍可发送 control/DNS/small，并允许 **64KB 保留额度**；额度用完才等待 credit 恢复；
   - 批量数据继续受原 credit 门控，不影响吞吐策略。
4. 纯 ACK 和非 SSH 流量保持原 DRR 路径，避免跨类乱序。

## 预期

批量上传并发时，交互 SSH 的小数据不再排在批量在途窗口之后，p95 应从 1.3s 量级回落到百毫秒级；批量吞吐仍由原 credit/DRR 控制。

## 验证状态

代码 + 单测完成（`p2p` 全部通过），已部署到 **gg / jf / qg / vm**（`v0.8.1-1-g8d30675`），**尚未扩散到其他环境**。

修复后同一批量上传 + 交互 PTY 测试：

| 路径 | 修复前 p95 / max | 修复后 p50 / p95 / max |
| --- | --- | --- |
| GG → VM OS sshd（172.30.0.2） | 1370ms / 1385ms | 457ms / 1858ms / 4912ms |
| GG → VM 内置 SSH（vm.phn） | — | 293ms / 1529ms / 2099ms |

结论：**p50 有改善，但 p95 仍在 1.5~1.9s，问题没有完全解决**。修复后 `[P2P-QDEPTH]` 仍出现 `maxSojourn=13.074s`、`drrOut=454`、`maxWrite=0s`、`rate=96.8KB/s`，说明：

1. peer 级 credit 门控仍在把批量帧压很久；
2. `smallCh` 只覆盖 p2p write loop，交互帧在到达 p2p 之前（gVisor inner TCP / trojan mapping / OS sshd）仍可能被批量流阻塞；
3. 两条测试路径（内置 SSH 和 OS sshd）都出现 1.5s+ 尖峰，瓶颈更可能在 GG 进程内的共享数据面（gVisor/mapping），而不是 OS sshd。

下一步（未完成）：

- 给 `writeReq` 增加 small 类标记，在 QDEPTH 里分别记录 bulk/small 的 maxSojourn，确认 small 帧是否真的被优先；
- 若 small 帧在 p2p 层已低延迟，把优先级上移到 gVisor inner TCP / mapping relay 的共享队列；
- 在未拿到 p95 改善前，不扩散该修复到其他环境。

## 主流修复（第二轮，2026-11-15）

放弃逐包“大小/端口猜流类型”，改为 qdisc 式公平队列 + 背压：

1. **DRR quantum 64KB → 4KB**：一轮只给单流约一个 MTU 级的字节量，批量流不能占用一整轮 640ms（100KB/s 载体）。
2. **单批上限 16KB**：避免一次 `SendBatch` 形成 ~90KB 的突发。
3. **同步载体关闭 peer 级 credit 门控**：直连 TCP 已设置 `TCP_NOTSENT_LOWAT`，有真实内核背压，交给 DRR + 传输阻塞；异步 h_tunnel POST 仍保留 credit。
4. **自适应 `TCP_NOTSENT_LOWAT` = 2×BDP**，夹在 16KB~512KB：慢链路小窗口限制交互时延，快链路大窗口不压吞吐；由 `stripeTransport` 控制器每 200ms 调整。
5. **`smallCh` 保留但默认关闭**（`enableSmallPriority=false`），只作对照。

实现只增加固定队列/单 transport 字段，没有 per-flow map，不引入生命周期清理问题。

**WS 载体统一（第三轮）**：实测这条 P2P 实际走 h_tunnel WebSocket，`transport: tcp` 只是 leg 形态；上一轮只给 `streamTransport` 实现了 lowat/门控能力，WS 载体被漏掉。已把 `KernelBacklogLimited` / `SetKernelBacklogLimit` 落到 `WSTransport`：

- 客户端在 WS 握手前拿到原始 TCP conn，直接设置 `TCP_NOTSENT_LOWAT`；
- 服务端通过 `http.Server.ConnContext` 把 accepted conn 传进 WS handler；
- WS `Send` 本身是同步写，具备真实背压；lowat 生效后 peer 级 credit 对 WS 也关闭；
- p2p write loop 只按 carrier 接口决策，没有 `if WS` 分支。

验证：待部署后重测 gg→vm 的 300MB 上传 + 交互 PTY，目标 p95 <200ms；未达标不扩散。

## 第四轮：被动侧 tuner 缺失 + srtt 自放大（2026-11-15）

部署 `v0.8.1-9-ga650852` 后重测仍不达标：

| 时间 | 场景 | p50 / p95 / max |
| --- | --- | --- |
| 16:47 | 300MB 上传 + 交互 PTY，GG → VM OS sshd | 518ms / 1472ms / 1785ms |
| 16:53 | 同上（打开 p2p/relay/trojan INFO） | 387ms / 1959ms / 20022ms |

关键日志（GG，路径实际由 GG 被动接收会话）：

```
[LANE-INFLIGHT] passive:5 lane=0 inflight=69..157KB cap=64KB..8192KB
                rtt=64..923ms srtt=51..833ms minRTT=20..21ms rate=398..958KB/s
[LANE-LOWAT] set 512KB remote=...     # 仅创建时一条，从未出现 adaptive
```

两个根因：

1. **被动侧从不调优**：`newStripeTransport` 只在 `cfg.dialLane != nil`（主动侧）时启动
   `controllerLoop`，而 `tuneKernelBacklog` 只在 `controllerTick` 内调用。GG 在本路径上是
   被动侧，carrier 永远保持创建时的 512KB 内核发送窗口；512KB / ~500KB/s ≈ 1s 的
   bufferbloat，正是交互帧看到的 HOL。
2. **公式用 srtt 自放大**：`lowat = 2 × rate × srtt`，而 srtt 是 ack 派生、已包含排队时延。
   队列越满 srtt 越大，lowat 越大，形成正反馈，永远回不到低时延。

修复：

- 新增 `lowatLoop()`，每 200ms 独立于 lane 控制器运行，主/被动侧都启用；从
  `controllerTick` 移除重复调用。
- 公式改用 **minRTT**（10 分钟传播基线，LEDBAT 风格）：`lowat = 2 × rate × minRTT`，
  无基线时回退 srtt，再无则 30ms；仍夹在 16KB~512KB。
- 初始 `laneNotSentLowat` 由 512KB 降为 16KB，先小后大，避免首批数据在调优前灌满内核。

验证：待部署 4 节点后重测 300MB + 交互 PTY，目标 p95 < 200ms；未达标不扩散。

## 第五轮：DRR 输出 FIFO 才是真正的队列（2026-11-15）

第四轮修复部署后（`v0.8.1-10-g670061b`）：

| 方向 | 场景 | p50 / p95 / max |
| --- | --- | --- |
| 上行 GG→VM（300MB 上传） | 交互 PTY | 513ms / 1383ms / 1907ms |
| 下行 VM→GG（300MB 下载） | 交互 PTY | 113ms / 459ms / 989ms |

延迟集中在**上行**，说明是正向载体上的排队，不是共享的 gVisor/回程。

`ss -tin` 采样 GG 的上行载体（到 JF）：`cwnd:8`、`Send-Q≈32KB`、
`notsent≈21KB`、`bytes_retrans≈10%`、`minrtt≈21ms`。内核里没有 1.3s
的数据，排队发生在应用层：

- `drrLoop` 每轮取 `NextBatch(64)`，再逐帧塞进容量 **512** 的
  `drrOutput`；写循环每次只收集 16KB。
- 交互包空闲期，调度器把 FIFO 填满批量帧；交互包即使赢得 DRR 轮次，
  进入 FIFO 时仍排在最多 512 帧（≈700KB）之后，500KB/s 载体上就是
  1.3s，与 `LANE-INFLIGHT` 的 `srtt≈1.35s` 完全吻合。
- DRR 的公平顺序被这个中间 FIFO 完全抵消。

修复：

- `drrOutputDepth = 16`（≈1~2 个写批次），超出的帧留在 DRR 的 per-flow
  队列里，由调度器按 4KB quantum 公平选择；
- `drrLoop` 单轮批量 64 → 16，避免批次本身变成第二个 FIFO。

验证：待构建部署 4 节点重测上行 300MB + 交互 PTY。

## 第六轮：剩余 p95 由 GG↔JF 载体丢包/RTO 主导（2026-11-15）

第五轮部署后（`v0.8.1-11-gcac6c68`）上行 300MB + 交互 PTY：

| 指标 | 第五轮前 | 第五轮后 |
| --- | --- | --- |
| min | 159ms | 159ms |
| p50 | 513ms | **348~382ms** |
| p95 | 1383ms | **1438~1454ms** |
| max | 1907ms | 1468ms |

逐探针日志显示 p95 不是稳定排队，而是**周期性突发**：每 15~20s 出现
连续 4~5 个 1.2~1.5s 的探针，随后回到 200~400ms。与以下证据对齐：

- `[P2P-QDEPTH] maxSojourn=1.175s writes=123 busy=1.442s maxWrite=487ms`：
  载体 `Write` 一次阻塞 487ms；
- 上行路径由 mesh 选为 `GG → JF → VM`（`/api/mesh` bestPaths: vm 的
  nextHop=jf），对应 GG→JF:39999 载体；
- 该载体 `ss -tin` 采样：`cwnd:1..14`、`ssthresh:4..14`、
  `rto:220ms`、`lost:6..17`、`bytes_retrans/bytes_sent ≈ 10%`，
  `minrtt≈19ms`、`delivery_rate 3.2~5.3Mbps`；
- 反向（VM→GG 下载 300MB）同窗口：p50 113ms / p95 459ms / max 989ms，
  响应方向无同级别排队。

结论：第五轮之后应用层队列已不是主因；剩余 p95 尖峰来自公网载体
（GG↔JF，约 4Mbps、~10% 重传）的丢包恢复/RTO。单个有序 TCP 载体上，
批量帧的丢包恢复会 HOL 阻塞后续所有帧，包括交互帧。

可选方向（需要决策）：

1. 交互流与批量流分载体（stripe 第二 lane / 独立 WS），彻底隔离 RTO HOL；
2. 载体发送侧按实测 delivery_rate 做 kernel pacing（SO_MAX_PACING_RATE），
   把重传率压下来，牺牲少量吞吐；
3. 接受该环境上限，把验收目标改为“p95 < 500ms”或仅在无损链路验收。

## 第七轮：多 lane 从未激活的修复（2026-11-15）

在 GG/JF 日志中 `[STRIPE] add lane` 计数为 **0**：多 lane 功能从未生效。
两个原因叠加：

1. `gateEngaged := t.primaryFlow.isGated()` 在收到第一个 lane ack 后恒为
   true，直接冻结 `addDue`；而 credit 门控在 kernel-bounded 载体
   （WS/TCP，已启用 NOTSENT_LOWAT）上本来就被 `peerWriteLoop` 旁路，
   并不存在“gate owns congestion”。
2. `addDue` 只看本端 `t.gauge`。GG→JF 方向由 GG（被动侧）发送并拥塞，
   GG 的 gauge 已通过 `FrameLaneGauge`/gossip 送达 JF，但 JF 的控制器
   没有使用 `peerGauge`。

修复：

- gate 冻结改为 `!t.KernelBacklogLimited() && isGated()`，即仅对真正
  启用 credit 门控的异步载体生效；
- `peerHigh = peerGauge >= 0.7*255` 作为一等 add 触发；
- peer 触发采用更快的 5s sustain / 5s interval，动态 lane 上限 2 条，
  避免在丢包 WAN 上堆叠过多冗余载体；
- add lane 日志和事件补充 peerGauge/peerTriggered。

单测：`TestStripePeerGaugeAddsLaneWhileGateEngaged` 覆盖“gate 已 engaged
且载体 kernel-bounded 时，peer 拥塞仍能加 lane”。

验证：待部署 4 节点后重测上行 300MB + 交互 PTY。

首次验证（只修 gate/peerGauge，5s sustain）：JF 在批量开始后约 9s 加
lane1、27s 加 lane2。逐探针结果：前 26s 仍有 1.4s 尖峰，lane2 生效后
（探针 21~60）稳定在 **99~145ms**，p50 从 348~382ms 降到 **120ms**，
但前段尖峰仍把整轮 p95 拉到 1412ms。

补充两项：

- peer 触发的 sustain/interval 由 5s 降到 **1s**，让 lane 在批量开始
  后 1~2s 内出现；
- 新增 **idle-carrier spread**：affinity miss 时，若 rendezvous 选中的
  载体已有 flow 而另一个载体为空，则改选空载体的最高分者。这样前两个
  长流（批量 + 交互）会落在不同 TCP 载体上，而不是靠 hash 撞运气。
  每个载体只增加一个 `pinned` 原子计数，affinity 表整体重置时清零，
  无 per-flow 生命周期清理负担。

单测：`TestStripeIdleSpreadSeparatesFirstFlows`。

## 第八轮：idle-carrier spread 的亲和表生命周期（2026-11-15）

第一次跑通（v14）：

| 指标 | 数值 |
| --- | --- |
| min | 95ms |
| p50 | **116ms** |
| p95 | **177ms** |
| max | 680ms |

除探针 1（PTY 建立 + lane 正在加，680ms）外，探针 2~60 全部 **95~188ms**，
p95 首次进入 200ms 以内。JF 在批量开始后约 6s 通过 peerGauge 加 lane1，
idle spread 把批量流和交互流分到不同载体。

紧接着第二次运行回退到 p50=209ms / p95=1506ms。原因：affinity 表只在
lane 集合变化或容量溢出时整体重置，**流结束后条目仍留在表里**，导致
每个载体的 `pinned` 计数一直大于 0，第二次运行的批量/交互流不再满足
“有空闲载体”条件，只能按 rendezvous 撞运气。

修复（显式生命周期清理）：

- affinity 值改为 `affinityPin{lane, seen}`，命中时刷新 seen；
- 新增 janitor：每 10s 扫描，删除 idle ≥ 30s 的 pin 并递减对应载体计数；
- `repinPrimary` 同步维护 primary/lane 计数；
- 表整体重置时清零所有计数。

单测：`TestStripeAffinityExpiresIdleFlows`。

## 第九轮：最终验证（v0.8.1-15-gb3b6327，2026-11-15）

同一路径 GG→JF→VM，300MB 上传 + 60 次交互 PTY 探针：

| 运行 | min | p50 | p95 | max | 说明 |
| --- | --- | --- | --- | --- | --- |
| 1（worker 重启后首次） | 96ms | 114ms | 1417ms | 1545ms | 前 ~10s 冷启动窗口，探针 3~8 慢，探针 9 起恢复 |
| 2 | 93ms | **112ms** | **160ms** | 179ms | 全部探针 ≤179ms |
| 3 | 97ms | **113ms** | **130ms** | 148ms | 全部探针 ≤148ms |

稳定态验收：**p95 130~160ms < 200ms，达标**。JF 在 peerGauge 触发后
1~2s 内加 lane，idle spread 让批量流与交互流落在不同载体；affinity 的
30s idle 过期保证连续运行不会重新撞到同一载体。

剩余问题（独立于本次改动）：worker/会话刚建立后的首个 300MB 上传，
载体 TCP 处于冷启动，前 ~10s 仍会经历一次链路饱和/丢包恢复窗口
（1.4s 级）。这是单条浅缓冲公网链路上的 TCP 慢启动行为；若要消除，
需要发送侧 kernel pacing（SO_MAX_PACING_RATE）或更小的初始窗口，
属于另一个课题。

扩散状态：仅在 gg / jf / qg / vm 四个验证环境；未扩散到其它环境。

## 第十轮：冷启动窗口攻坚（2026-11-15）

复现方式：kill GG worker（watchdog 自动拉起），6s 后立即跑 300MB + 交互
PTY。结果 p50=117ms / p95=1419ms / max=1581ms，探针 5~15（约前 17s）
间歇出现 0.3~1.6s 尖峰，之后恢复 110ms。JF 的 lane1 直到批量开始后
**11s** 才加入。

两个独立原因与修复：

1. **高 gauge 只在水位跨越时通告**：冷启动时第一次 high 可能早于对端
   gossip 握手就绪，急件丢失后要等下一次 low→high 跨越（实测 11s）。
   改为 high 期间每 1s 限频重发 `FrameLaneGauge`。
2. **TCP_NOTSENT_LOWAT 只管“未发出”字节**：冷启动时拥塞窗口仍可把
   in-flight 数据堆到浅缓冲链路里。Linux/darwin 的 `setNotSentLowat`
   现在同时设置 `SO_SNDBUF = n`，从创建时（16KB，内核翻倍约 32KB）
   就限制 unsent+unacked 总量；tuner 有速率样本后再按 2×BDP 放大。

验证：待部署 4 节点后按同一“重启 worker → 立即压测”配方复测。

第一版（v17：SNDBUF=n + gauge 重发）冷启动复测：p50=109ms /
p95=274ms / max=516ms（此前 1419/1581）。只剩探针 1~5（前 ~3s）
偏高，之后 ~100ms。

ss 采样定位到原因：冷启动前 4s 内核 cwnd 从 16 涨到 41，
unacked 25 段（~32KB）、notsent 64KB；第 5 秒出现 lost:34，
随后 5s 的丢包恢复。问题是 tuner 用初始突发的 acked rate 放大了窗口
（首 1~3s acked rate ~1MB/s，实际链路 ~0.5MB/s）。

追加两处修正：

- Linux 的 SO_SNDBUF 传 n/2（内核翻倍后有效值约等于 n），初始有效窗口约 16KB；
- tuner 增加“突发前 5s 保持创建窗口”保护：按 tx 空闲间隔 >=1s 识别
  新突发，burstAt 后 5s 不放大窗口。慢链路上 16KB 约 1.6xBDP，不压吞吐。

## 第十一轮：冷启动最终状态与失败实验（2026-11-15）

最终保留版本 `v0.8.1-28-g4b9d1ea`：

| 场景 | min | p50 | p95 | max |
| --- | --- | --- | --- | --- |
| 温态（fresh session，无前置批量） | 91ms | 113ms | 149ms | 578ms |
| 冷启动（kill worker 后立即压测） | 95ms | 110ms | 376ms | 695ms |

冷启动从 p95 1419ms / max 1581ms 降到 p95 376ms / max 695ms；前 6 个探针
仍有 263~695ms，之后回到 ~100ms。

本轮验证为无效并已回退的三条路（重要教训）：

1. 把 Linux SO_SNDBUF 钉在约 lowat（2xBDP）：冷启动首窗变小，但温态从
   p50 112/p95 160 退化到 p50 258/p95 1425。内核自调发送缓冲不能随意钉小。
2. 冷启动窗口内强制数据全部走 lane（v24/v25）：冷启动 p95 141~159 达标，
   但紧接着的温态 p50 482~649/p95 690~831；lane 弱于 primary 时整体变慢。
3. 1xBDP/8KB 窗口：温态仍差，不是主因，已回退到 2xBDP/16KB。

保留的修复：gauge 高位 1s 重发、idle-spread 选择进锁、affinity 30s idle
过期、peerGauge 触发加 lane、DRR 输出 16 帧、被动侧 lowat 调优、
2xBDP/16KB、突发前 15s 保持创建窗口。

结论：温态验收（p95<200ms）达标；冷启动已降一个数量级但未达标。残余尖峰
的结构性根因是 peer write loop 对 primary 载体是同步写：primary 的丢包
恢复会阻塞唯一的写协程，已经投递到健康 lane 的交互帧仍在队列等待。彻底
解决需要 per-carrier writer（每个载体独立写协程 + 控制帧优先级队列），
属于下一阶段改造，不要再靠调窗口/临时改路由去逼近。

## 第十二轮：per-carrier writer 落地（2026-11-15）

结构性改造：primary 也有独立写协程 + 有界队列（数据 2 批、控制 256 帧），
`Send/SendBatch` 只入队；控制帧优先且不丢，数据队列满尾丢弃。新增
`TestStripeBlockedPrimaryDoesNotBlockLane` 证明 primary 写阻塞不再阻塞 lane。
同时 idle-spread 优先把前几个流放到空闲动态 lane，避免落在 primary 的
数据队列上。

验证（gg→jf→vm，300MB + 60 探针）：

| 场景 | min | p50 | p95 | max |
| --- | --- | --- | --- | --- |
| 温态 v28（同步 primary） | 91 | 113 | 149 | 578 |
| 温态 v32（per-carrier writer） | 91 | 115 | 256 | 477 |
| 冷启动 v28 | 95 | 110 | 376 | 695 |
| 冷启动 v32 | 98 | 134 | 303 | 646 |

结论：

- primary→lane 的写循环 HOL 已从结构上消除（单测覆盖），冷启动继续
  改善（p95 376→303）。
- 但温态 p95 从 149 升到 256，冷启动仍未进 200。剩余尾延迟不再是
  “同一 TCP 队列的 HOL”，而是**共享 WAN 瓶颈本身**：两条载体走同一条
  ~0.5MB/s、~10% 丢包的链路，批量流的 RTO/重传突发仍会占用瓶颈队列，
  交互 lane 的包也会排队。
- 在这一层之下继续调 writer/窗口收益已很小；要再进 200ms 需要限制
  批量流对瓶颈的占用（内核 pacing / 发送速率上限）或换更大带宽的链路。

## 第十三轮：内核 BBR 参照实验（2026-11-16）

在 GG 上临时加载 tcp_bbr（默认 CC 仍 cubic，测完已 rmmod），用
PHAETHON_CARRIER_CC=bbr 对载体 socket 设 BBR，跑同一路径的 30MB goodput
和 300MB+交互探针：

| 指标 | cubic | bbr |
| --- | --- | --- |
| 30MB 上传耗时 | **103.4s（~0.29MB/s）** | 142.5s（~0.21MB/s，-28%） |
| 载体重传 | ~8~10% | ~15% |
| 交互 p50/p95 | — | 114ms / 681ms |
| BBR 带宽估计 | — | 12.9Mbps（实际链路 2~4Mbps） |

`ss` 同时显示接收侧 `rcv_rtt` 10~43s、`rcv_space` 100KB 级：**JF 的载体
读循环被下游（JF→VM）背压阻塞，socket 没有被及时 drain**，ACK 流变得
稀疏/突发，BBR 的 delivery-rate 采样因此严重高估，随后维持更大窗口、
重传更多、goodput 反而下降。

结论：

- **内核 BBR 在这条路径上是负收益，已回退并 rmmod**；不要把它作为方案。
- 瓶颈不只在发送侧 CC，**接收侧读循环与下游转发的耦合**是更大的问题。
  应用层 pacer 如果拿同样的 ACK 信号，也会犯同样的高估错误。
- 下一步结构性工作应是**读侧解耦**：载体读循环只入有界队列（满则尾丢弃/
  异步转发），不直接阻塞在下游；socket 持续 drain 后，CC 才能看到真实
  delivery_rate。读侧解耦完成后再评估 pacing，否则没有意义。

## 附：拓扑图链路丢包率为什么恒为 0%

拓扑边上的 loss 来自 `mesh.PeerQuality` 的 ACK/ping 统计：

- 判定丢包要等 `pingTimeout=5s`；
- 超时判定后还有 `pingLateGrace=30s`，迟到的 pong 会把该样本翻回
  `acked=true`；
- 载体是 TCP，链路丢包由 TCP 重传隐藏，控制帧几乎总能送达。

所以 0% 是"控制帧 ping 丢包率"的真实值，不是链路丢包率，也不是没统计。
要反映真实链路质量，应在拓扑边上增加 **TCP 重传率**（TCP_INFO 的
`bytes_retrans/bytes_sent` 或 `total_retrans`），并与控制帧丢包率分开展示。

## 第十四轮：v29~v32 回退记录与噪声复核（2026-11-16）

回退（保留历史，全部为新的 revert commit）：

| 回退提交 | 原提交 | 内容 |
| --- | --- | --- |
| f46173f | 9ef74d6 | idle-spread 优先动态 lane |
| f804c69 | 3fe7ebd | primary 数据队列 4->2 |
| d0e2072 | dbe12a1 | per-carrier writer |

相关实验保留在标签：exp/w32-percarrier、exp/bbr-carrier-cc、
exp/bbr-negative-result。回退后部署 v0.8.1-38-gd0e2072 到 gg/jf/qg/vm。

重要复核：同一份回退后的二进制连续跑 3 轮温态：

| 轮次 | p50 | p95 | max |
| --- | --- | --- | --- |
| 1 | 220 | 1378 | 1440 |
| 2 | 107 | 134 | 190 |
| 3 | 367 | 1359 | 1451 |

同一二进制 p95 可在 134 和 1378 之间跳变，远大于 v28(149) 与 v32(256)
的差异。因此之前“v32 温态退化”的单次结论不成立，版本差异被链路 RTO
风暴的噪声淹没。后续版本比较必须用多轮中位数，且先解决周期性尖峰本身。

结论与下一步：
- 回退保留（生产先用 v28 行为），不要再用单次 p95 判断版本优劣；
- 真正支配因素是周期性 RTO 尖峰 + 接收侧 rcv_rtt 偏大；
- 先做观测：拓扑边增加 TCP 重传率（bytes_retrans/bytes_sent），与 ping
  丢包率分开展示；
- 再做读侧解耦/背压的结构性验证，而不是继续在版本间来回切。

## 第十五轮：读侧解耦 + 拓扑重传率 + 路由代价（2026-11-16）

已完成并部署 v0.8.1-41-gbc22f76 到 gg/jf/qg/vm：

1. 读侧解耦：stripe 的 baseReadLoop/laneReadLoop 对数据帧改为非阻塞派发，
   满队列尾丢弃并归还缓冲，载体会持续 drain；丢弃计数并入 Peer.DataDrops。
2. TCP 重传率：frame 读取 Linux TCP_INFO（Segs_out/Total_retrans），经
   transport/stripe/udpLeg/p2p 透出；mesh 每 10s 差分 + 0.7/0.3 EWMA；
   拓扑 JSON 增加 retransRate，mesh 页面显示 "retrans X%"。
3. 路由代价：lossEff = 1-(1-pingLoss)(1-retransRate)(1-dropRate)，
   cost = (SRTT+Sojourn)/denom/confidence；retransRate 随 GossipLink 广播
   （omitempty，老节点/无 TCP_INFO 平台按 0），远端多跳路径也参与。
   未探测链路（SRTT=0）仍兜底 cost=1000。

实测（v41 温态 3 轮）：

| 轮次 | p50 | p95 | max |
| --- | --- | --- | --- |
| 1 | 119 | 508 | 20020 |
| 2 | 286 | 1385 | 1464 |
| 3 | 111 | 146 | 172 |

第 1 轮同时采样拓扑：gg→jf 的 retrans 从 0% 升到 **6.3%**，SRTT 23→89ms，
cost 23→89；qg→jf 1.1%、jf→qg 0.9%（gossip 传播可见），而 ping loss
始终为 0%。这直接证明旧的“丢包率”指标看不到真实链路重传，新指标能
对齐风暴窗口。

路由稳定性：gg→jf 代价升到 89 时，直连 gg→vm 代价仍在 146~217，JF 仍是
最优下一跳，未观察到路由切换/抖动。

结论：读侧解耦消除了“消费者慢→停止读 socket”的一类阻塞，但没有消除
周期性风暴本身；风暴现在有直接可观测签名（gg→jf 载体 retrans 尖峰 +
SRTT/cost 同步跳变）。下一步应针对风暴的初始触发（载体 RTO/内层 gVisor
TCP 恢复），用该指标做单变量 A/B，而不是再调整路由公式。

## 第十六轮：内层 RTO 下限实验（否定结果，2026-11-16）

根据第十五轮的采样，风暴期间外层载体（GG→JF TCP）并未进入 RTO backoff
（backoff 始终为空），bytes_retrans 平稳增长；而内层 gVisor TCP 出现
100~180 段/秒的重传突发，外层在此期间反而“空闲”（bytes_retrans 不变）。
由此假设：外层 RTO≈223ms 略高于内层 MinRTO=200ms，每次外层重传触发
内层伪 RTO，随后指数退避造成数秒停顿。

加入 `PHAETHON_INNER_MIN_RTO_MS`（默认 200ms，不设置时行为不变），在 GG
上设为 500ms 跑 3 轮温态：

| 轮次 | p50 | p95 | max |
| --- | --- | --- | --- |
| 1 | 317 | 1389 | 1571 |
| 2 | 379 | 2298 | 6648 |
| 3 | 112 | 131 | 161 |

对比默认 200ms 的 3 轮（p50 119/286/111，p95 508/1385/146），**没有改善**，
风暴仍出现（max 6.6s）。结论：内层 MinRTO 不是触发点，单纯把内层 RTO
抬高不能消除风暴；内层重传突发更像是内层 flow 在某个更长的停顿后恢复
（或 gVisor 内部 stall）的结果，而不是 223ms 外层重传直接触发。

已验证并排除的路径：内层 MinRTO 200→500ms。下一步方案调整：
1. 优先验证 UDP leg（绕过外层 TCP 的 RTO/丢失恢复）是否是风暴触发点；
2. 若 UDP leg 也复现，则问题在内层 gVisor（flow stall/link rebuild），
   需要做 per-flow 状态取证，而不是继续调 RTO/窗口。

## 第十七轮：风暴期间的内层 flow 取证（2026-11-17）

使用 admin `/api/mesh` 已有的 `tcpFlowDebug`（gVisor per-flow JSON）在
风暴窗口逐秒采样，结果：

- 批量 SSH flow：cwnd 84~128、sndBufUsed 0.6~1MB、unsent 0.5~1MB、
  outstanding 91~128、srtt 73→750ms、rto 200ms→2.7s。内层 flow 本身健康，
  但在持续喂一个慢的外层载体。
- 外层载体（GG→JF TCP）：cwnd **5~9**、lost **5~23**、srtt 400~750ms，
  处于持续丢包恢复；这是真正的瓶颈。
- 交互 SSH flow：cwnd 128、outstanding=0、unsent=0、lastSend 最长 4.8s。
  说明请求已经发出，卡在**回程**（VM→GG）等待响应，而不是请求方向排队。
- 同期所有 app 级 drop 计数（meshInbound/dataBudget/lane/readQueue/nicTx/
  tunWrite）**全为 0**。

结论：剩余风暴不是 app 队列丢包、不是路由、也不是内层 MinRTO；是外层
载体的 WAN 丢包/RTO 同时影响两个方向（去程批量 + 回程交互响应）。内层
cwnd 很大、unsent 很深的组合会把外层丢包恢复放大成秒级停顿。

下一步（方案调整）：
1. 做 UDP leg 对照实验（绕过外层 TCP 的 RTO/丢失恢复），多轮中位数，
   观察风暴是否消失；
2. 若 UDP leg 也复现，再按方向拆分（去程/回程 carrier）做 per-direction
   取证，不再盲调 RTO/窗口。

## 第十八轮：UDP leg A/B（突破，2026-11-17）

在 JF 主动侧把 `GGSV_TJ` 的 `p2p-udp` 打开（toggle-p2p-udp 改 DB 后重启），
`/api/p2p` 显示 GG peer：`transport=udp`、`legPhase=active`。同一路径
GG→VM，各跑 3 轮：

| 载体 | p50（3 轮） | p95（3 轮） | max（3 轮） |
| --- | --- | --- | --- |
| TCP | 163 / 133 / 111 | 461 / 403 / 137 | 11163 / 576 / 173 |
| **UDP leg** | **101 / 100 / 103** | **121 / 118 / 133** | **292 / 142 / 256** |

结论：

- 周期性风暴的触发点就是 **GG↔JF 外层 TCP 载体的丢包/RTO**；换成 UDP leg
  后 3 轮全部干净，p95 稳定在 **118~133ms**，max ≤292ms，达到 <200ms 目标。
- 内层 gVisor/应用队列不是触发点；之前的 MinRTO、pacing、per-carrier writer
  等尝试之所以效果有限，是因为没有绕开外层 TCP 的丢失恢复。
- UDP leg 的开关是 per-proxy `p2p-udp`（主动侧打开，被动侧自动 wrap 并
  通过 gossip 协商）；本次 p2p-key 为空，leg 为明文，生产需按需配置密钥。

后续：评估是否把 UDP leg 作为该链路的默认载体（或在控制台明确展示/
可切换）；其它主动侧路径（VM/QG）按同样方式验证；保留 TCP 作为回退。

## 第十九轮：外层载体突发与 pacing A/B（2026-11-17，事1）

风暴窗口逐秒采样（GG→JF 外层载体）：

```
t+0  cwnd=41  ssthresh=5.8MB
t+1  cwnd=57
t+2  cwnd=64  backoff=1  lost=6  ssthresh=133   ← 丢包
t+3  cwnd=19
t+4  cwnd=18
t+5  cwnd=9
t+11 内层 TCP retrans=73、timeouts=2
t+12..t+35 内层 segments_sent≈0~1/s（停顿约 24s），外层 bytes_retrans 不变
t+36 内层恢复发送
```

即：批量开始时外层内核慢启动从 cwnd 41 冲到 64（初始 ssthresh 5.8MB），
把浅缓冲打满后在 t+2 丢包，cwnd 塌到 9；随后内层 TCP 进入约 24s 的
RTO 停顿。外层突发是触发点之一，内层 RTO 退避是放大器。

单变量 A/B（3 轮，原配置 TCP 载体）：

| 变量 | p50 | p95 | max |
| --- | --- | --- | --- |
| 基线 | 163/133/111 | 461/403/137 | 11163/576/173 |
| `tcp_slow_start_after_idle=0` | 158/502/177 | 11618/618/12099 | 14295/2302/20021 |
| `SO_MAX_PACING_RATE=500KB/s` | 237/122/265 | 986/467/2823 | 10933/13748/17036 |

两者都没有消除 10s+ 停顿；500KB/s 已接近链路容量，说明只限制外层速率
不足以阻止内层 RTO 放大。实验后已恢复 GG 原配置（env 清空、sysctl 还原），
UDP leg 保持关闭。

结论：事1 目标（原配置 TCP 下 p95<200ms）尚未达成。下一步在 TCP 体系内
做隔离：给交互流单独一条 TCP lane（按流固定），让批量流的 carrier 丢包/
RTO 不阻塞交互流；这与 UDP leg 的隔离原理相同，但不改用户配置语义。
