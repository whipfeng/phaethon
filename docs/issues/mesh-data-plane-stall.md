# Mesh 数据面卡死：轻则断流、重则整机网络栈挂死

**状态**: 🔍 排查中（2026-11-01）
**严重级别**: 高（曾导致整机网络栈挂死，OS 被迫重启）
**首次发现**: 2026-11-01 ~10:13（VM 经 QG 出口 curl exit=28）

## 症状（两级，疑似同一根因）

1. **轻症：数据面卡死**
   - VM↔QG 之间所有新建 TCP 连接无法完成数据交换（双向）
   - 控制面（gossip/路由/DNS trie）完全正常
   - QG↔GG 等其他节点对的数据面正常
   - QG 本机出口正常
2. **重症：整机网络栈挂死**
   - 2026-11-01 ~12:09 前，VM（Windows 本机）整体挂死，外部 SSH 无法连接，重启操作系统才恢复
   - `LastBootUpTime = 2026-11-01 12:09:27`

## 时间线（2026-11-01）

| 时间 | 事件 |
|---|---|
| ~10:13 | QG 当日早前重启后，首次发现 VM 经 QG 出口 exit=28 |
| ~12:0x | VM 整机挂死，SSH 不可达 |
| 12:09:27 | Windows 强制重启恢复 |
| 11:25:58 / 12:52:44 / 13:07:19 | VM phaethon 多次重启（TUN idx 10→7→7） |
| 12:56:19 | QG 部署 v0.8.0-226-g9231944 |
| 13:09:34 | QG 收到 VM 发来的 7890 SYN，完成握手，但 socks5 数据 30s 无流量 |
| 13:10:43 | VM→QG:32458 偶发成功（telnet 连通） |
| 13:11:07 | VM→QG:7890 连接 52123：`CreateEndpoint` **永久挂起**（>20 分钟未返回） |

**关键判定：干净重启（12:09）+ 全新 226 双端，exit=28 依然复现** → 排除脏状态，是现行代码真实 bug。

## 实测证据链

### 排除项（已证伪的方向）

1. **与 226 提交无关**：6d440c6 / 217 / 225 / 226 多版本组合均复现。
2. **非 VIP/路由认知问题**：历史测试地址 `100.107.0.1` 已不存在（QG 现 subnet `100.0.0.0/16`）；正确服务入口是 hostIP `100.0.0.2`（VIP `.1` 按 route_selector.go 设计是 conntrack/SNAT 中转地址，`LocalStack=false` 非服务地址）。用正确地址测试症状相同。
3. **非 Windows 路由丢失**：split-tunnel `0.0.0.0/1` + `128.0.0.0/1` 经 TUN 路由实际存在（此前 `route print` 受 GBK 编码干扰误判）。
4. **非 netstack 全面死锁**：QG 累计 5737 个 forwarder `CALLBACK entered`，5736 个进入 `HANDLECONN-GOROUTINE`——仅 1 个挂起。数据面对其他节点（GG）正常。

### 卡死点实锤

- QG netstack 收到 VM 的 SYN（forwarder 打日志），`CreateEndpoint`（gVisor `performHandshake`：发 SYN-ACK 等 ACK）**20+ 分钟不返回**。
- 正常情况 SYN-ACK 重传超时应在数分钟内返回错误；永久挂起说明卡死在比重传定时器更深的位置（如：发送路径阻塞在满队列/锁上，定时器无法触发或报文无法入队）。
- VM 重启后 worker PID 14120 无日志静默更换为 10832；Windows 事件查看器无崩溃记录。

### 拓扑异常（待确认是否为诱因）

- VM 侧 `RECOMPUTE-ROUTES peers=[qg qg]`（两条 qg 会话）；QG 侧 p2p 列表 id 7/8 均为 vm、均 upToDate。
- QG 侧重复 jf 会话有明确配置来源（`MGMS_HT` + `MGMS_HT (copy)` 两个 h_tunnel 代理）；vm 的两条 Peer proxyName 为空（被动接入），成因待查。
- 症状"偶发能通一次 + 之后全挂"符合**流量被分摊到两条会话、其中一条已死**的竞态特征。

## 嫌疑点（按可能性排序）

1. **p2p 数据面写路径阻塞**：转发数据帧经数据队列（`enqueueWrite`）写入 htunnel TCP 通道；若写阻塞（对端不读/读循环死亡），队列积压直至死锁。gossip 走独立控制路径所以表现正常。
2. **Wintun 驱动/读循环层死锁**：重症（整机网络栈挂死）指向驱动层。历史上 `tun-read-stall`（ReceivePacket 永久阻塞）已修，但驱动层死锁可将整个 Windows 网络栈拖死。
3. **gVisor fork 握手/注入路径**：`performHandshake` 内发送 SYN-ACK 即阻塞（参见 gvisor-fork `pkg/tcpip/transport/tcp/forwarder.go`）。

## 复现途径（受限讨论）

- ❌ **本机双实例压测不可行**：Wintun 设备具有排他性，单适配器只能被一个进程打开（用户指出）。
- 备选：
  - 纯 p2p 传输层双实例互测（不经 netstack/TUN，只验证数据队列与 htunnel 通道）
  - 跨平台组合：Windows 实例 + Linux/Mac 实例互连
  - WSL2/Docker Linux 实例 + Windows 实例（各自独占本机 Wintun）
- 铁律约束：禁止在 VM/QG 环境做网络层劣化实验；压测只在本地隔离环境。

## 下一步（待讨论定方案）

1. 可观测性：加 pprof 或按需 goroutine dump，下次卡死直接抓挂死栈（一锤定音）。
2. 按实锤指向修复 p2p 写路径 / Wintun 读循环 / gVisor 握手。
3. 双会话拓扑去重或回包选路修复（若为诱因）。
