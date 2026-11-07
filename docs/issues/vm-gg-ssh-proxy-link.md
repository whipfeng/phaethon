# VM→GG 经 SSH 跳板的链路：周期断开 + 几乎不可用

**创建时间**: 2026-11-07
**状态**: 诊断中（瓶颈已定位，修复方案待用户决策）
**优先级**: 中

## 问题描述
VM 到 GG 的 mesh 链路走代理 `GGDYSV_TJ`（trojan → 106.13.183.103:39999），via `LAOYU_SSH_TEST`（ssh aigametest@47.238.79.7:22，阿里云香港）。
用户反馈：连一段时间就被强制断开；即使立即重连，链路也几乎不可用。

## 核心流程（正确理解）
```
VM phaethon ──(SSH TCP)──> 跳板 sshd 47.238.79.7 ──(新 TCP, direct-tcpip)──> GG:39999 trojan
     段①                                 段②（香港→北京，跨境）
```
- VM 写 trojan 数据 = 写 SSH channel；channel 受 sshd 给的窗口（OpenSSH 默认 2MB）限制
- sshd 把数据写到段② socket，段② 内核发送缓冲也能存数 MB
- mesh 的 ping（Phase C 设计：走数据队列）、DNS、TCP ACK 都和大流量挤同一条 lane

## 待确认问题清单

### 问题1：周期断开是谁断的
**结论**: ✅ 已验证 —— 旧版本自己断的。旧 keepalive 5s×1 次不回就关 SSH，每 25~45s 断一次；GG 侧同时看到 47.238.79.7 的 EOF/RST。
F5（SSH keepalive 容忍）部署后（VM v0.8.0-344 起）没再出现 SSH 断开。

### 问题2：积压在哪一段
**需要验证**:
- [x] VM 进程内发送队列：`[P2P-QDEPTH]`（b346a3b）压测 3 分钟**一条都没触发** → 队列 <256KB、单次写 <1s、排队 <1s ✅ 不在 VM 进程内
- [x] 段①（VM→跳板）：SSH keepalive 每 5s 一次，和数据走同一条 TCP；压测期间无 `[SSH-KA-RTT]`（回复都 <1s） ✅ 不在段①
- [x] GG 接收侧：`ss -tni` 采样 136s，GG 对 :41686 的 Recv-Q 始终为 0 ✅ GG 读得及时
- [x] 段②（跳板→GG）：GG 收速稳定 **≈46KB/s**（30~59KB/s），每 10s 有约 100 个乱序包（约占收到段数的 30%，说明丢包重），rtt≈55ms ✅ 瓶颈在段②

**当前状态**: ✅ 已验证

### 问题3：为什么"几乎不可用"
带宽只有 ≈46KB/s，但 VM 与 GG 之间能压住 ≈2MB+（sshd 窗口 + 跳板内核缓冲）。
2MB ÷ 46KB/s ≈ 45s 以上的排队 —— 同 lane 上的 ping/DNS/ACK 都得排这么久：
- VM 侧持续 `link dead held`（ping 10+ 次无 pong，但入向帧一直在）
- 第二轮下载 5s 就失败（vm.phn 的 DNS 转发被积压卡住）
- 15:05:29 GG 判 link dead（入向静默 >10s）关链路，2s 后重建
- Mac 侧有效吞吐仅 ≈27KB/s（线上 46KB/s，差额推测是内层 TCP 在超长排队下的重传 + 封装开销，**未实锤**）

## 诊断日志位置
- VM: `C:\Users\Docker\Desktop\Workspace\phaethon\data\logs\phaethon.log`，标签 `[P2P-QDEPTH]`、`link dead held`、`[SSH-KA-RTT]`
- GG: `/home/layer4/phaethon-gg/data/logs/phaethon.log`；`ss -tni state established "( dst 47.238.79.7 )"`

## 结论与修复
瓶颈是物理链路段②（香港跳板→北京 GG，≈46KB/s、重丢包），不是重连不及时。
程序层面的问题是**积压放大**：大流量把 2MB 级缓冲灌满，导致控制面/DNS/交互流量排队几十秒，看起来"整条链路不可用"。
修复方向待用户决策（需要先改方案文档）。

## 更新日志
- 2026-11-07：创建；完成三段排除，定位瓶颈在跳板→GG 段
