# elephant 服务下载滴流专项调查（2026-11-05，v4 定稿 2026-11-06，v5 状态更新 2026-11-06）

| | |
|---|---|
| 状态 | **硬停滞（吞吐归零永不恢复）：✅ 已根治（2026-11-06，双层根因，soak 60/60 零冻结）**，见 §3.5。**残余滴流（单流 173~726KB/s、8 流聚合 0.2~10MB/s 高方差）：🔵 进行中** — 机制沿用 v4 实锤（mesh 隧道层乱序 → 内层发送端 dup-ACK → cwnd 压死 1~3），三钉死（每流有序化，c608130/f0fb969）已部署但 JF→QG 方向仍有残余乱序源未钉住（QG rcv_ooopack 1469 ≈ 45% 包乱序），新一轮取证 **2026-11-06 已批准立项**（v5，方向见 §5） |
| 现象 | https://10.193.131.25/elephant/default/ 大资源每连接稳定滴流 40~110KB/s，多 MB 资源需数十秒~2 分钟，浏览器表现为「一直待处理」 |

## 一、症状与复现

- 全部 37 个静态资源 HTTP 200、字节数完整、无 RST、无截断。**排除「提前关闭连接」假设**。
- 每连接速率恒定（速率 ∝ 1/RTT 的窗口形态），多连接并行各跑各的（6 连接并发 46~113KB/s，聚合 478KB/s；8 流 ~1.2MB/s 线性）。**排除共享带宽瓶颈**。
- 小响应（3KB 主文档 0.4s）快，大响应滴流 —— 与体积强相关。
- 传输中段可发生**硬停滞**（停在固定字节数数十秒不增长，见 §3.4）。

## 二、机制实锤时间线

| 版本 | 日期 | 结论 | 结局 |
|---|---|---|---|
| v1 | 11-05 | POST 通道 stop-and-wait 在 deskzq 出口 | 被 v2 推翻 |
| v2/v3 | 11-05 深夜 | deskzq↔jf 底层路径 cwnd 塌缩（mss1260/恒cwnd10/RTT 抖动） | **被 v4 推翻**（当时只见底层腿之果，未见内层流之因） |
| **v4** | **11-06** | **mesh 隧道层乱序 → 内层发送端 dup-ACK → cwnd 塌缩** | **实锤（本文档定稿）** |
| **v5** | **11-06** | **硬停滞（§3.4）双层根因定位并根治**：gVisor #14092 SACK 记账泄漏 + cubic cwnd=0 int 截断 | **✅ 已修复**（fork 5ba8665/e6fd17d，soak 60/60）；残余滴流转 §5 新一轮 |

v2/v3 为何错：当时用 QG→jf 274×5MB POST 11MB/s 证明「底层腿健康」，但 POST 批量上传只测了**隧道搬运能力**（大量独立请求并发，乱序由各跳本地 TCP 各自消化），从未测过**内层端到端 TCP 流**的窗口行为。内层流 sender 的 cwnd 从未被直接观测过，所有「底层路径」证据（cwnd 恒 10、重传 0）实为内层 TCP 症状在底层腿上的投影。

### v4 证据链

1. **每流稳态 100~210KB/s 与源无关**：QG 经 mesh 下载 elephant（Linux 真实栈 sender）、经 socks5 A/B、经 deskzq/jf 不同源，速率带一致。
2. **聚合随流数线性扩展**：8 流 ≈ 1.2MB/s → 瓶颈在每流自己的拥塞控制，不在链路带宽。
3. **接收端内核实锤乱序**：QG 侧内层 socket `rcv_ooopack` 采样 173→573，约 50% 分段乱序到达。
4. **socks5 A/B**：健康底层腿上走 mesh 内层流照样滴流 → 与底层路径无关。
5. **发送端 gVisor 计数器差分**（见 §3）→ 直接实锤 dup-ACK/快速重传/cwnd 塌缩机制。

## 三、发送端取证（2026-11-06，JF gVisor 计数器差分）

插桩：`/api/mesh` 新增 `tcpStats` 字段（导出 gVisor `stack.Stats().TCP`，只读）。测试流：QG 经 mesh 下载 JF 本机软件仓 8MB 包（`jf.phn:443`，sender = JF 的 gVisor netstack，与 elephant 场景的 Linux sender 同受 mesh 乱序机制作用，作为可观测代理）。JF 重启后基线全零。

### 3.1 活动窗口差分（下载前 10s，传输活跃期）

| 计数器 | 差分 | 含义 |
|---|---|---|
| segments_sent | +481 | 窗口发送量 |
| **fast_retransmit** | **+28** | dup-ACK 触发的快速重传（≈ 每 2 个 RTT 一次） |
| retransmits | +36 | 总重传（含上者） |
| **sack_recovery** | **+31** | 进入快速恢复次数 |
| **spurious_recovery** | **+31** | **恢复后发现是虚警**（原段只是迟到 = 乱序铁证） |
| **segments_acked_with_dsack** | **+35** | 对端 ACK 带 DSACK（重复段到达 = 乱序铁证） |
| timeouts | 0 | 无 RTO，纯快速重传主导 |

- SpuriousRecovery ≈ SACKRecovery 且 DSACK 与重传同量级 → **乱序被误判为丢包**，不是真丢包。
- 吞吐反推 cwnd：47.5KB/s ÷ (MSS 1460B / RTT ~50ms) ≈ **有效 cwnd 仅 1.6 段** —— 慢启动从未起来的定量实锤。

### 3.2 机制链（v4 定稿）

```
mesh v2 隧道层：16 路并发无序 POST（sendSem=16）+ 2 条 GET recvLoop 长轮询合批
  → 同一条内层流的帧经多路并发通道传输，无按序保证
  → 接收端 ~50% 分段乱序到达（QG rcv_ooopack 实锤）
  → 内层发送端（gVisor/Linux 真实栈）收到 3 个 dup-ACK → 判定丢包
  → 快速重传 + cwnd 砍半 + 进入快速恢复
  → DSACK/SpuriousRecovery 揭示误判，但 cwnd 已受损，刚恢复又被下一波乱序打回
  → cwnd 钉死在 1~2 段（初始窗口都涨不出去）
  → 每流 40~210KB/s 恒定滴流；N 流聚合线性扩展
```

### 3.3 为何上传方向看起来很快

上传 11MB/s 是 274 个**独立 POST 请求**的隧道层搬运速度：每个请求体由源端本地 TCP 栈发出，乱序/重传由各跳自己的本地 TCP 消化，聚合自然高。它从未考验过任何一条**内层端到端 TCP 流**的拥塞控制 —— 上传下载的 73× 不对称是「测了不同的东西」，不是链路单向病态。

### 3.4 附带病理：传输中段硬停滞（症状记录，已于 §3.5 根治）

同一取证过程中观察到：下载在 **475136 字节**处硬停滞 60s+（此前 elephant 3MB 文件也曾在 917468 字节停滞 40s+），期间 JF 计数器 segments_sent 持续增长（其他内层流）但该连接 0 字节前进、0 RTO。与 §3.2 主机制是**两个独立问题**。⚠️ 原怀疑「隧道腿静默死亡」**已被 §3.5 推翻**：根因在 gVisor TCP 栈内部，与隧道腿无关。

### 3.5 硬停滞双层根因与根治（2026-11-06 定稿，✅ 已修复）

**症状**：单条内层 TCP 流吞吐归零、ESTABLISHED 永不恢复；进程、admin、其他流全部正常（「控制台还能用」是因为只有该流的发送端楔形）。

**取证工具**：fork 新增端点注册表 + `/api/mesh` `tcpFlowDebug` 快照（fork 提交 `90cb5ed`），冻结瞬间抓取发送端 sender 全量状态，两次独立冻结抓到**同一楔形签名**：

```
cwnd: 0, ssthresh: 2, sackedOut: -3 / -1（负值 = 计数器被幻影占用磨穿）
sndUna == sndNxt（全部数据已获 ACK），outstanding: 0
resend/reorder/probe 三定时器全 disabled，unsentBytes 624896~755474
writeListSegs 40~49，congestState: open —— 不是 loss 状态，是纯卡死
```

**第一层根因（幻影占用）**：gVisor TCP 发送端把「SACK 过又被累计 ACK 的」包在**非恢复状态**下漏记 Outstanding（`SetPipe` 只在恢复期做 accounting）→ Outstanding 虚高 → 虚假 RTO/recovery 反复磨 cwnd。上游 issue gvisor.dev/issue/14092，修复 PR #14101，逐行移植进 fork（`5ba8665`）。

**第二层根因（cwnd=0 创生点）**：cubic `getCwnd` 返回值经 `int()` 截断，无 ≥1 地板。RTO 后 cwnd=1、`HandleRTOExpired` 记 WMax=1 → `wtRtt ≈ WMax×Beta = 0.7` 段 → `cwnd = 1 + (0.7-1)/1 = 0.7` → **int 截断为 0**。RFC 5681 要求 cwnd≥1，fork 原本无地板。cwnd=0 后 `sendData` 循环条件 `Outstanding < SndCwnd`（`0<0`）永不放行；恰数据全 ACK（sndUna==sndNxt）→ 三定时器全灭、纯 ACK 不再触发 `cc.Update` → **永久卡死**。fork 修复 `e6fd17d`：`Update`/`updateSlowStart` 两处赋值加 `max(...,1)` 地板，使 cwnd≥1 成为归纳不变量（同时覆盖 `int(WEst)` 与 `int(cwnd)` 两个截断点）。

**修复历程**：只移植 PR #14101 后复测，soak 第 1 轮仍冻（1.5MB @16:18）→ 再次冻结瞬间快照仍 cwnd=0 → 排除幻影占用（第一层已修）后唯二可能：`getCwnd` 截断 / 其他负路径，逐行推 cubic 公式实锤截断。两层都修后 **soak 60/60 轮零冻结**（每轮 -m 120s 下满 8MB 包，冻结判据=字节数 6×5s 不变；修复前 1~2 轮必冻）。

**部署状态**：VM/QG/JF 全部运行 v0.8.0-325-g2361df1（linux md5 d4be74cf…，windows 738684bd…），fork @ e6fd17d，phaethon @ 2361df1，均已推送 origin。

## 四、排除项记录（累计）

- ❌ 提前关闭响应 / VM wintun 专属 / 共享带宽瓶颈 / TCP 连不上 / mesh 层队列丢包（全零）/ ICMP 大包黑洞 / deskzq 旧版本 / p2p 队列层瓶颈 / 方向会话状态保温（131s 反向灌流 1.9GB 不改善）/ v2 htunnel 代码缺陷（QG↔jf 同代码 11MB/s）/ MGMS_HT 开 p2p-udp（IsP2PUDP 仅 SOCKS5）
- ❌ **v2/v3：deskzq↔jf 底层路径 cwnd 塌缩**（v4 推翻：该腿只是投影，内层流在健康腿上同病；POST 11MB/s 只证明隧道搬运能力）
- ❌ mesh VIP 作为目的地址的访问**不是合法用法**（用户权威纠正）
- ✅ 实锤：mesh 隧道层乱序 → 内层发送端 dup-ACK → 快速重传/cwnd 塌缩（§3.1 差分数据）
- ✅ **已根治：传输中段硬停滞**（§3.5：双层根因 gVisor #14092 + cubic cwnd=0 int 截断；soak 60/60 零冻结）。原「隧道腿静默死亡」假设排除
- 🔵 进行中：残余滴流 = 三钉死后仍存的乱序源（v5 新一轮，§5）

## 五、下一步

1. **消除乱序源（🔵 v5 进行中，2026-11-06 已批准）**：三钉死（每流 lane 亲和钉扎 + mesh 入站按流钉队列 + htunnel 有序模式，c608130/f0fb969）已部署，cwnd 不再归零，但最新抓取 QG 内核 rcv_ooopack 1469（~45% 包乱序）证明 JF→QG 方向仍有乱序源未钉住；真实乱序→dupACK→恢复循环仍把 cwnd 压 1~3 → 滴流。新一轮取证方向：逐环节排查 JF→QG 方向每帧发送顺序（mesh 出站 → htunnel lane → 物理网卡），重点嫌疑 stripe 并行 lane 多路下发导致帧重排（lane 间未钉序）、某段帧未走有序通道。方案文档见 docs/plans/。
2. ~~硬停滞探活（独立小项）~~ **✅ 不需要了**：硬停滞根因已在 §3.5 根治（栈内 bug，非隧道腿死亡），探活机制无的放矢。
3. XHR API 挂死问题（浏览器 F12 pending，用户自理待办）。
4. JF_SOCKS5 测试代理（p2p 已关、代理保留）清理时机由用户定。

## 六、历史研判存档（v2/v3，已被 v4 取代，保留备查）

<details>
<summary>v2/v3 原文（11-05 深夜）</summary>

**v3 机制链（当时结论）**：deskzq↔jf 底层（10.149.237.34↔10.140.51.2，mss1260 隧道，base RTT ~29ms，抖动至 90ms+）→ 每条 TCP 连接窗口被钳在 ~12.6KB 量级（cwnd 恒 10）→ JF 全程零重传/零 DSACK → 候选限速因子：RTT 抖动放大 ACK 时钟、隧道中段中间盒、小窗口 × HTTP/1.1 串行乘积 → 5~6 连接聚合 ~500KB/s。

**v4 修正**：该「cwnd 恒 10」实为**内层 TCP**（经 mesh 隧道）的症状投影在底层腿上；底层腿本身健康（同代码同型腿 11MB/s）。真凶是 mesh 隧道层给内层流制造的乱序。

**v2 数据存档**：

| 方向 | 路径形态 | 实测 |
|---|---|---|
| 上传 VM→elephant（5MB POST） | IPIP 直达（外层 vm-EIP→deskzq-EIP） | **12MB/s（0.42s）** |
| 下载 elephant→VM（1.87MB） | 逐跳普通中转 deskzq→jf→qg→vm | **57~88KB/s（21~33s）** |
| Mac→VM 20MB（mb→qg→vm） | 普通中转（不过 jf/deskzq） | 532KB/s |

- deskzq 以 10.149.237.34 源地址与 JF:32457 建立 4-6 条长连接，全部 mss=1260、cwnd 恒 10、ssthresh 7~9、rtt min 29ms 实测 30~91ms。
- 用户补充：有时单向流量是通的（与路径质量波动自洽）。
- 同一时期 deskzq→vm 方向能扛 ~4000 小帧/s（ACK），扛不住 ~38-65 大帧/s。

</details>
