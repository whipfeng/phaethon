# P2P 控制帧拥塞韧性与传输缓冲治理

> 版本: v0.3.0（v0.3.0 简化：写侧去掉 deadline 改纯阻塞写，卡死检测全部移交读侧；v0.2.0 曾设计的丢帧/补帧机制取消；v0.1.0 初版）

## 背景

2026-09-23 部署 P2P v6（`p2p_v6_and_mesh_package_distribution.md`）后的运维观察与讨论结论：

1. **JF 重启后 `nslookup jf.phn` 需较长时间恢复**。成因有两部分：
   - 结构性：拓扑多跳传播依赖 15s gossip 周期，JF 的直连邻居是 qg，gg/vm 需等 qg 下一跳，ms9/ms10 再等一跳，最坏 ~30-45s。mesh DNS（`ResolveDomainSubnet`）查实时路由表、无缓存，传播未到即解析失败。
   - 链路层：慢链路上（JF 走 h_tunnel over NAT）控制帧被大流量拖住，进一步拉长传播时间（本方案治理目标）。
2. **VM→GG→MS10 推送 7.9MB 包在 74%（5.9MB）处连接中断**（服务端 `read body failed`）。疑似中继链路级联阻塞后，写 deadline 到期杀连接，内层 TCP 流随之断开。
3. **现有机制盘点**：p2p 已有 `controlCh`(512) / `writeCh`(16384) 双队列，`peerWriteLoop` 每轮开头非阻塞抽干 controlCh——**用户态队列层控制帧严格优先，经分析正确，保留不动**。但该优先级只覆盖用户态，管不到内核。

## 问题分析：控制帧延迟的三层排队点

| 层 | 排队机制 | 控制帧优先级有效？ |
|---|---|---|
| 用户态队列 | `controlCh` vs `writeCh`，fast-path 每轮先抽干控制队列 | ✅ 有效。控制帧最多落后一个"正在写的数据帧"；循环空闲时阻塞在三路 select（stopCh/controlCh/writeCh），不空转 |
| 内核 socket 缓冲 | TCP 按字节序 FIFO 上线 | ❌ 无效。未设置 SO_SNDBUF/SO_RCVBUF，Linux autotune 可达 MB 级；大流量时控制帧字节排在积压数据**队尾**（bufferbloat）。512KB@100KB/s ≈ 5s 队尾等待 |
| 对端零窗口 | TCP 流控，对端不读则本端一字节都发不出 | ❌ 无效。Write syscall 睡死直到 deadline：控制帧 5s、数据帧 30s。**任一超时即 `conn.Close()`，整条链路拓扑状态全重置**，重新传播 hop×15s——比丢一条 gossip 的代价高一个数量级 |

### WriteFrame 帧完整性约束（决定"丢帧保连"的可行性）

`reverse/frame.go:50` 的 WriteFrame = 3 字节头 + payload **两次独立 Write**。写超时后无法撤回已写入流的字节：

- 已写字节数 **n == 0**：流完好，可安全丢弃该帧，后续帧继续正确解析
- **n > 0**（半帧入流）：帧边界错位，接收端后续全部解析错乱，**必须杀连接**（无可挽回）

因此"超时丢帧保连"必须以"确认未写入任何字节"为前提。

## 方案

### 1. P2P 连接 socket 缓冲上限（治 bufferbloat）

- P2P 连接建立处（入站 `HandleP2PConn` 的 conn、主动拨号的 conn）调用 `TCPConn.SetWriteBuffer / SetReadBuffer`
- 默认 **512KB**；新增配置项 `p2p: socket-buffer-kb`，0 = 不设置（走系统默认）
- 效果：控制帧队尾最坏等待 = 缓冲/带宽（512KB @ 100KB/s ≈ 5s，而非 MB 级缓冲的 40s+）
- 权衡：高 BDP 链路（带宽×RTT > 缓冲）吞吐上限被压低。当前全部链路（LAN、h_tunnel over NAT、trojan 中继）BDP 远小于 512KB，无实际影响；配置项保留后路

### 2. 写侧去掉 deadline：纯阻塞写（v0.3.0）

无 deadline 时 `conn.Write` 只有两种出口：整帧进入内核缓冲（完成；Go 内部自动按内核腾出的空间续写），或连接错误（连接已死，帧完整性失去意义）。"部分写"仅在 deadline 截断时出现——去掉 deadline 后半帧场景不存在，v0.2.0 设计的丢帧/补帧机制与 WriteFrameN 全部不需要，`reverse/frame.go` 保持原样。

#### 死链检测矩阵（全部由读侧承担，双侧 60s read deadline 已存在，p2p.go:394）

| 故障 | 检测方 |
|------|--------|
| 对端进程死 / RST | 内核 FIN/RST → 本端阻塞的 Write/Read 报错退出 |
| 对端活着但不发数据 | 本端 60s read deadline |
| 对端 wedged（不读不发） | 对端收不到任何帧 → 对端 60s 关连接 → 本端阻塞 Write 报错；本端读侧 60s 同时判死 |
| 本端出向零窗口卡死 | 对端收不到帧 → 对端 60s 关连接 → 本端阻塞 Write 报错退出 |

#### 与 deadline+丢帧方案（v0.2.0）对比

- 持久卡死：结局相同——对端读侧 60s 判死重连（丢弃帧改变不了对端"收不到字节"的事实）
- 瞬时拥塞恢复：无 deadline 方案把卡死期间排队的帧（含 gossip）全部按序送达；丢帧方案则把卡死期间的帧连同 gossip 一起丢弃
- 实现：减少 WriteFrameN、丢帧分支、补帧循环全部复杂度

### 3. 保留现有用户态队列优先级（不变）

- 双队列 + fast-path 机制正确，无需改动
- 不引入控制/数据分连接：fd、握手、失败模式全翻倍，当前 6 节点规模不划算

## 变更清单

| 文件 | 变更 |
|------|------|
| `p2p/p2p.go` | ① 连接建立处 SetWriteBuffer/SetReadBuffer（读配置）；② peerWriteLoop.writeFrame 去掉 SetWriteDeadline（纯阻塞写），取消 5s/30s 双 deadline |
| `config` | 新增 `p2p: socket-buffer-kb`（默认 512，0=系统默认） |

## 验证

1. **单测**：阻塞写至完成；模拟对端关闭后 Write 报错并正常销毁 session
2. **复现修复前故障**：VM→GG→MS10 推送 7.9MB 包应完整完成（修复前 74% 处断开）
3. **控制面观察**：大流量传输期间，GG console peers lastSeen 无 >15s 间隙，gossip 不中断、链路不闪断
4. **jf.phn 恢复时间**：JF 重启后各节点解析恢复时间不劣于现状（结构性多跳传播时间不变，链路层拖长因素消除）

## 关联

- 上游设计：`p2p_v6_and_mesh_package_distribution.md`
- 关联发现（另行立项，不在本方案范围）：h_tunnel 传输每帧一次完整 HTTP 往返（同步 POST/长轮询 GET），RTT 直接决定吞吐上限，且每次 `dialHTunnel` 新建 Transport 导致逻辑连接间不复用 TCP——是 JF 链路慢的结构性根源，待单独设计改进方案
