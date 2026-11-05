# mesh 多链路散流任务清单

> 设计方案：[mesh-multilink-stripe-design.md](../plans/mesh-multilink-stripe-design.md)
> 前置结论：UDP 腿（已有机制）优先；散流只覆盖无 UDP 腿的 TCP 流；路由层不动。

## 阶段 0：UDP 腿点亮与普查（运营，零代码）

- [ ] 0.1 普查：链路 × UDP 就绪矩阵（各节点 p2p_udp 配置 / 被动端开关 / admin badge / legDrops）
- [ ] 0.2 QG↔Mac 开 p2p_udp 点亮 UDP 腿（VM 留跑取证期不动），admin 确认 FSM Active、badge=udp
- [ ] 0.3 trojan 腿 UDP relay 可用性验证（DialPacket 已有，验服务端）
- [ ] 0.4 点亮后 50MB scp 复测，对比 v301 基线（Mac→VM 中位 69s / VM→Mac 26-38s）
- [ ] 0.5 观测 UDP 腿 IP 分片丢包率，评估 MSS/MTU 是否需要回议（v3.2 定了不钳，实测超标再议）

## 阶段 1：stripe 骨架（代码）

> 核心决策：同 proxy 多链路**封装在传输层**（`stripeTransport` 包装器，形态对齐 udp_leg_transport / ht v2 连接池），对路由/gossip/会话层呈现**单一链路**。DRR/peerWriteLoop/enqueueWriteReq/预算全部不动。

- [ ] 1.1 新帧类型 0x16 StripeAttach（{sessionID, stripeID}）+ 帧编解码与单测
- [ ] 1.2 `stripeTransport` 包装器骨架：实现 FrameTransport 接口，1 lane = 纯转发（行为零差异基线）
- [ ] 1.3 主动端：同 proxy 重复 DialP2P + 首帧 StripeAttach 绑定
- [ ] 1.4 被动端：HandleP2PTransport 识别 StripeAttach → 登记 extra data leg（复用 UDP 腿归属机制）
- [ ] 1.5 per-lane 写循环（独立 writev）+ SendBatch 内按 5-tuple 分流；控制/DNS 帧恒走 primary lane
- [ ] 1.6 per-lane readLoop + fan-in 单 Recv 通道（udp_leg dataCh 模式）；lane 上收到控制帧 = 协议错误 warn 丢弃
- [ ] 1.7 判据：全部现有单测/e2e 通过；peer 写路径零改动

## 阶段 2：动态控制器（代码，包装器内部）

- [ ] 2.1 信号采集：包装器发送队列积压、per-lane 内部探测（lane 内 LinkPing/LinkPong，不上 gossip）
- [ ] 2.2 加线逻辑：积压持续 30s 超 70% → +1（间隔 ≥10s，cap 4）
- [ ] 2.3 减线逻辑：积压清空持续 5min → -1
- [ ] 2.4 UDP leg FSM 状态订阅：Active 停止加线/存量到期回收；Backoff 恢复
- [ ] 2.5 控制器事件日志（加/减线原因原文）

## 阶段 3：散流决策（代码，包装器内部）

- [ ] 3.1 rendezvous 加权哈希（流 key = 内层 5-tuple，权重 = lane 本地探测质量，linkCost 形态）
- [ ] 3.2 候选集 = primary lane ∪ 动态 lanes（跨 proxy 并集在阶段 4 后按实测再议）
- [ ] 3.3 权重劣化主动失效重哈希
- [ ] 3.4 lane 数增减时 rendezvous 最小重映射验证

## 阶段 4：验证（部署门控后）

- [ ] 4.1 admin badge/计数扩展（stripe×N、per-stripe srtt/loss/权重、亲和表大小）
- [ ] 4.2 Mac↔VM 50MB scp 三取向对比 v301 / UDP 腿数据
- [ ] 4.3 多 proxy 环境双链路利用率 ≈ 权重比
- [ ] 4.4 kill 单 stripe 流平滑迁移验证
- [ ] 4.5 HoL 注入：单 stripe 劣化吞吐损失 ≤1/N；长稳 24h
- [ ] 4.6 阈值实测校正（cap/积压阈值），常量化定稿

## 明确不做

QUIC / 直连 UDP 监听 / ARQ / 跨 stripe 重排序 / MPTCP 调度 / stripe 跨 proxy / 路由层改动（见设计文档 §二）

## 独立线（不阻塞本清单）

- stall 取证：12:33 / 18:30 / 03:05 三 dump 并排分析（留跑取证期）
- d7df39b（gVisor 调试日志拖垮数据面）滚动部署 + /tmp 清理
