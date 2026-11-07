# Mesh 链路：控制面隔离 + 在途量上限

**状态**: DRAFT（待用户确认）
**创建**: 2026-11-07
**问题来源**: `docs/issues/vm-gg-ssh-proxy-link.md`
**相关方案**: `mesh-multilink-stripe-design.md`（stripe lane）、`htunnel_p2p_direct_mode.md`（HT v2）

## 1. 背景

VM→GG（trojan via SSH 跳板）压测证据：
- VM 进程内队列 <256KB、SSH keepalive <1s、GG Recv-Q=0，瓶颈在跳板→GG 段（≈46KB/s，丢包重）
- VM 与 GG 之间压住 ≈2MB+（sshd channel 窗口 + 跳板内核缓冲），全在我们进程外面
- 同一条连接上的 ping/DNS/ACK 跟着排几十秒 → `link dead held`、DNS 5s 超时、链路被判死重建

问题本质是两个：
1. **控制面和大流量挤同一条连接**（控制帧排在别人的缓冲后面，进程内优先级无效）
2. **进程外积压不受控**（内层 TCP 只看到 RTT 涨到几十秒，不丢包，不会降速，反而超时重传浪费带宽）

## 2. A：控制面隔离（专用数据 lane）

### 2.1 规则
- 满足触发条件的链路，主动方会话建立后**立即**拨一条**专用数据 lane**（复用现有 `dialStripeLane` + StripeAttach，被动方无需改协议）
- 专用 lane 存在时：所有可拆分数据帧（`splittable` = 非 DNS 的 FrameMeshPacket）**只走数据 lane**，主连接只走控制帧、DNS、ping/pong
- 动态 lane（拥塞控制器加的）照旧参与分流，但主连接不再参与分流（rendezvous 候选集去掉 primary）

### 2.2 触发条件（自动，无配置）
主连接的底层 carrier **没能设上 TCP_NOTSENT_LOWAT**（`frame.limitKernelBacklog` 走到 skipped 分支）：
- 经代理/中继的 carrier（SSH channel、via 链、非 TCP socket）
- Windows 上所有链路（不支持 NOTSENT_LOWAT）

能设上的（Linux 直连 TCP）积压本来就留在进程内 DRR，控制帧已能优先，不开。
实现：`limitKernelBacklog` 返回 bool，dialer 侧把结果挂到 transport 上供 p2p 读取。

### 2.3 专用 lane 的生命周期
- 专用 lane **不受**降级剔除（`srtt≥1s / loss≥50%` 那条）和 5 分钟空闲回收约束
- 专用 lane 写/读失败被移除 → 数据回落主连接（现有行为），控制器 5s 后重拨；重拨失败指数退避（5s→60s 封顶）
- `laneEnqueue` 满等待 50ms 后改走主连接的回落：专用 lane **不回落**（回落就又混到一起了），改为由 §3 的在途门控在 DRR 前拦住

### 2.4 ping 语义
ping 仍按 Phase C 进数据 DRR 队列（测进程内数据面排队），出 DRR 后走主连接。
§3 把积压拉回进程内后，ping 的 SRTT 仍能反映数据面拥塞，路由代价计算不受影响。

### 2.5 不覆盖
HT v2（POST/GET 池）不走 stripe（p2p.go:1468 明确排除），由 §4 单独处理。

## 3. B：在途量上限（所有 TCP 类链路通用）

### 3.1 计量
- 发送方：每条 lane（主连接 laneID=0，其余用 stripeID）累计 `tx` = 成功交给 transport 写出的帧 payload 字节
- 接收方：每条 lane 累计 `rx` = 从该 lane 读到的帧 payload 字节
- **在途量** = `tx − ackedRx`，就是压在 carrier 缓冲、跳板、网络里的数据

### 3.2 回报帧
- 新帧类型 `FrameLaneAck = 0x17`，二进制：重复 `{laneID uint32, rx uint64}`（12B/条）
- 接收方每 50ms 检查一次，任一 lane 的 rx 自上次回报涨了就发；走主连接、control 优先级
- 能力协商：gossip 加 `lack:true`（同 `udp:true` 模式）。只对声明了能力的对端发 ack；发送方收到首个 ack 才对该 lane 启用门控 → 新老版本混跑时行为与现在一致（老版本收到未知帧只记 Info 丢弃，p2p.go:1784，也不会出错）

### 3.3 上限
```
deliveryRate = ackedRx 增量 / 时间（EMA，α=0.25）
baseRTT      = 交付 RTT 的 10s 滑动最小值（记录 (时间, tx) 采样点，ack 越过该点时得 RTT）
cap          = clamp(2 × deliveryRate × max(baseRTT, 50ms), 64KB, 4MB)
```
- 未拿到速率样本前 cap=4MB（不限慢启动）
- ×2 留给速率上探（同 BBR cwnd_gain）
- 快链路（10MB/s、20ms）≈400KB → 不卡吞吐；这条 VM→GG（46KB/s）→ 64KB 下限，进程外排队 ≈1.4s（现状 45s+）

### 3.4 门控位置（关键）
在 `peerWriteLoop` 出队处，不在 transport 写入处：
- 数据可写额度不足时，`peerWriteLoop` **暂停从 drrOutput 取数据**，但继续取 controlCh / dnsQueue
- 额度 = 数据 lane 们的总在途 < 总 cap（有专用 lane 时只算数据 lane；否则算主连接）
- ack 到达 / 写完成时通知 `peerWriteLoop` 重新检查
- 效果：多出的数据留在 DRR 队列（公平调度、8MB 预算、满了丢包 → 内层 TCP 正常降速），控制帧永不被门控卡住

### 3.5 不覆盖
UDP 数据腿（不经 TCP 缓冲，自带机制）；HT v2 本期不接门控（§4 先解决控制面混排）。

## 4. C：HT v2 控制帧与数据帧分开

现状（与 `htunnel_p2p_direct_mode.md:147-151` 原设计不符——原设计就有独立控制通道，未实现）：
- 客户端 `Send/SendBatch` 忽略 `isControl`，控制帧和数据塞同一个 POST body，排在 16 个 POST 名额后面
- 服务端 POST 入向 `meshIn` 单 FIFO（4096），`Recv` 无控制优先
- 客户端 GET 读循环 `dataRecvCh`（64）满了会阻塞，同一 GET body 后面的控制帧跟着等

改法（按原设计补齐）：
1. 客户端：控制帧单独立即发一个 POST，**不占** `sendSem` 名额；数据帧照旧批量
2. 服务端：`meshIn` 拆成 ctrl/data 两个队列，`Recv` 控制优先（与 `FrameTransport.Recv` 约定一致）
3. 客户端 GET：先把整个 body 解析完，控制帧先投 `ctrlRecvCh`，再投数据；数据通道满时不挡控制帧

## 5. 实施顺序
1. B 的计量 + 回报帧 + 能力协商（两端都要新版本才生效）
2. B 的门控
3. A 专用数据 lane
4. C HT v2
每步单测 + 本地编译通过后统一部署 VM/GG 验证，再部署其他节点。

## 6. 验收
- Mac→vm.phn 4 路并发下载 3 分钟：
  - VM/GG 不再出现 `link dead held` / `link dead`
  - 压测期间 `vm.phn` DNS 解析 <1s；对 vm.phn 的交互请求（`/api/version`）<3s
  - 总吞吐不低于现状（≈27KB/s）
- GG↔JF（直连 TCP，Linux）64MB 吞吐不低于现状
- QG→JF（HT v2）压测期间无 `link dead`，ping SRTT 不随数据量线性涨
- 新老版本混跑（只升一端）链路正常、无报错

## 7. 新增诊断日志
- `[LANE-INFLIGHT]` 每 5s（有门控时）：lane、inflight、cap、rate、baseRTT、门控累计等待时长
- `[STRIPE] dedicated data lane` 拨号/移除/重拨
