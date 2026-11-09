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

### 6.2 下一步（针对 B）

1. 在 QG 开 `mesh=debug`，观察 `HandleMeshFrame` 的 relay 分支与 `channelDropStats`、`tcpStats` 差分（重点看非本机目的帧的入队/丢弃/阻塞）；
2. 对比 qg 上"终结给本机"与"中转给 jf"两条路径的队列深度与写循环耗时；
3. 用 411 与旧版（如 343）各跑一次 vm→qg→jf 的 1MB 传输，确认是否为本次升级引入的回归（怀疑点：D26 写 TUN 双优先级、D21 入站队列丢弃策略对**中转**路径的影响）；
4. 若确认是回归，优先回退/修正对应批次，再谈吞吐优化。

