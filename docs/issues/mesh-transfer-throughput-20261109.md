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
