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
