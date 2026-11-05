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

- [x] 1.1 新帧类型 0x16 StripeAttach（{nodeIDLen, nodeID, seq:2, stripeID:4}，会话 key=(nodeID,Seq)）+ 帧编解码与单测 — f032852/c407ebd
- [x] 1.2 `stripeTransport` 包装器骨架：实现 FrameTransport 接口，1 lane = 纯转发（行为零差异基线）— b10fb91
- [x] 1.3 主动端：同 proxy 重复 DialP2P + 首帧 StripeAttach 绑定（`stripeAttachLane`，触发器在阶段 2）— 07d9543
- [x] 1.4 被动端：HandleP2PTransport 首帧 peek 识别 StripeAttach → 按 (NodeID, RemoteSeq) 查会话 attachLane（`firstFrameTransport` 注入首帧 gossip 回会话路径）— 07d9543
- [x] 1.5 per-lane 写循环（独立队列 + writer goroutine）+ SendBatch 内按 5-tuple 分流；控制/DNS 帧恒走 primary lane；队列满/死 lane 回退 primary — 07d9543
- [x] 1.6 per-lane readLoop + fan-in 单 Recv 通道；lane 上收到控制/gossip 帧 = 协议错误 warn 丢弃 — 07d9543
- [x] 1.7 判据：frame+p2p 全量单测通过（12 个新增）；peer 写路径零改动 — 07d9543

## 阶段 2：动态控制器（代码，包装器内部）

- [x] 2.1 信号采集：primary SendBatch sojourn EMA / 50ms 归一化（空闲 5s 衰减）；per-lane PingInfo 探测（lane 内 ping→自动回 pong、pong→匹配记 srtt/超时计 loss，不上会话层）— 3c1aa00
- [x] 2.2 加线逻辑：gauge ≥0.7 持续 30s → +1（间隔 ≥10s，cap 4）— 3c1aa00
- [x] 2.3 减线逻辑：gauge <0.7 持续 5min → -1（减最新 lane）— 3c1aa00
- [x] 2.4 UDP leg FSM 状态订阅：Active 停止加线（highSince 清零）+ 存量靠清空规则自然回收；Backoff 后需重新持续 30s — 3c1aa00
- [x] 2.5 控制器事件日志：加减线原因原文 ring buffer(32) + 日志 — 3c1aa00
- [x] 2.6 拥塞信号自归一化（方案修订，用户"走1"批准）：sojourn 滑动 min 滤波基线（10s 窗口 + 1ms 噪声地板）+ 无量纲 gauge = clamp((EMA−base)/(max(base,地板)×margin=1.0),0,1)；持续满拥塞逃逸（gauge 钉 ≥0.95 过窗口 → EMA 重建基线 + 事件）；空闲衰减改 EMA 向基线回拉；lane 权重改相对自身 srtt min 基线 w=1/(1+rel+2×loss)；删除 50ms/50ms 两个绝对尺度 — 本次

## 阶段 3：散流决策（代码，包装器内部）

- [x] 3.1 rendezvous 加权哈希（流 key = 内层 5-tuple，权重 = lane 本地探测质量，linkCost 形态）— 6e56526
- [x] 3.2 候选集 = primary lane ∪ 动态 lanes（**单 proxy 会话内，永不跨 proxy**——§4.3 决策：proxy 间是异构 LinkNIC 隔离域，跨域切换归路由）— 6e56526
- [x] 3.3 权重劣化主动失效重哈希（probe ≥8 且 loss ≥50% 或 srtt ≥1s → 移除 + 事件）— 6e56526
- [x] 3.4 lane 数增减时 rendezvous 最小重映射验证（存量候选互不迁移，测试锁定）— 6e56526

## 阶段 4：验证（部署门控后）

- [x] 4.1 admin badge/计数扩展（stripe×N、per-stripe srtt/loss/权重、发送计数；rendezvous 无状态无亲和表）— e222dbe
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
