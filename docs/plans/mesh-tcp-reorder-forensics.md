# mesh 内层 TCP 乱序致发送端 cwnd 塌缩 — 发送端取证插桩（2026-11-06）

| | |
|---|---|
| 状态 | **已完成（2026-11-06）— 假设实锤**：JF 活动窗口差分 fast_retransmit +28 / retransmits +36 / sack_recovery +31 / spurious_recovery +31 / dsack +35（481 segments_sent），有效 cwnd ≈ 1.6 段；Spurious+DSACK 证明祸首为乱序而非丢包。详见 docs/issues/elephant-download-dribble.md v4。附带病理：传输中段硬停滞无 RTO（独立问题） |
| 关联 | [elephant 下载滴流专项](../issues/elephant-download-dribble.md)（v4 研判）、[mesh 大文件慢](../issues/mesh-large-file-slow.md) |

## 背景与假设

elephant 滴流专项 v4 研判：下载方向每内层流稳态 ~100-150KB/s、发送端 cwnd 钉在初始值 ~10（慢启动从未增长）、聚合随流数线性扩展（8 流 1.2MB/s）、与底层路径/对端协议/源节点全部无关；QG 内核侧观测到内层分段持续乱序（rcv_ooopack 173→573，约占 50% 分段）。假设：**mesh 帧乱序 → 内层发送端持续 dup-ACK → 快速重传/cwnd 塌缩**。

## 目标

在 JF（发送端嫌疑节点）直接观测 gVisor TCP 的 `FastRetransmit` / `Retransmits` / `Timeouts` / `FastRecovery` / `SACKRecovery` / `SpuriousRecovery` 计数器，在滴流窗口做差分：
- FastRetransmit 差分 ≫ 0 且与 SegmentsSent 同量级 → 假设成立，进"消除乱序源"方案设计
- FastRetransmit ≈ 0 → 假设推翻，回退到窗口/其他机制排查

## 变更（只读观测，无行为变更）

1. `mesh/netstack.go`：`Netstack.TCPStats()` — 导出 `n.ns.Stats().TCP` 计数器（nil 安全）
2. `mesh/mesh.go`：`MeshManager.GetNetstackTCPStats()` 委托
3. `admin/admin.go`：`apiMesh` 结果新增 `tcpStats` 字段（挂在现有 /api/mesh，不加新路由）

## 部署

1. 先 VM（本机）构建部署验证 API 可用（带版本戳，Makefile ldflags）
2. JF 部署需用户确认（生产节点、手动 nohup、pkill 陷阱），确认后构建 Linux 版经 QG 中继放置，重启后立即采样
3. 采样方案：JF `GET /api/mesh` 取 tcpStats 基线 → QG 持续下载 JF/elephant 大文件 60s → 再取一次做差分

## 回滚

纯增量只读字段，旧二进制不认识该字段也不受影响；回滚 = 退回上次运行版本（既有纪律）。
