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

- [ ] 1.1 新帧类型 0x16 StripeAttach（{sessionID, stripeID}）+ 帧编解码与单测
- [ ] 1.2 主动端：同 proxy 重复 DialP2P + 首帧 StripeAttach 绑定
- [ ] 1.3 被动端：HandleP2PTransport 识别 StripeAttach → 登记 extra data leg（复用 UDP 腿归属机制）
- [ ] 1.4 per-stripe 发送队列 + per-stripe writeLoop（独立 writev）
- [ ] 1.5 per-stripe readLoop（只收 mesh/ping/pong，直通现有入向）
- [ ] 1.6 固定 1 stripe 默认行为：全部现有单测/e2e 通过，行为零差异
- [ ] 1.7 per-stripe 预算分账方案定型（共享+水线 vs 配额）

## 阶段 2：动态控制器（代码）

- [ ] 2.1 信号采集：DRR 出队积压、budget 消耗速率、per-stripe ping 统计
- [ ] 2.2 加线逻辑：积压持续 30s 超 70% → +1（间隔 ≥10s，cap 4）
- [ ] 2.3 减线逻辑：积压清空持续 5min → -1
- [ ] 2.4 UDP leg FSM 状态订阅：Active 停止加线/存量到期回收；Backoff 恢复
- [ ] 2.5 控制器事件日志（加/减线原因原文）

## 阶段 3：散流决策（代码）

- [ ] 3.1 流亲和表（flowHash → 连接指针，60s 空闲过期）
- [ ] 3.2 rendezvous 加权哈希（权重 = 本地 per-conn ping，linkCost 形态）
- [ ] 3.3 候选并集 = 跨 proxy 连接 ∪ 同 proxy stripes
- [ ] 3.4 权重劣化主动失效重哈希
- [ ] 3.5 控制/DNS 帧恒走 primary 的护栏（stripe 上收到视为协议错误）

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
