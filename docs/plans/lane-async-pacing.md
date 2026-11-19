# Lane 级异步整形（async pacing）设计与 A/B 结果

> 状态：**已实现并 A/B，负结果（2026-11-19）**。默认关闭，代码保留在历史提交
> `2a74ec0`；deskzq 已回退到 `v0.8.3-19-gba787fa`，不随生产部署。
> 目标：解决 4 个满速大文件并发时单流落后，同时不重演同步 pacer 阻塞写循环的失败
> （见 `docs/issues/mesh-transfer-throughput-20261109.md` §110）。

## 0. A/B 结果（deskzq→jf，4 × 9.4MB 并发）

| 模式 | 各流耗时 | 速率 |
|---|---|---|
| pacing off | 11.2 / 13.4 / 26.0 / 26.9s | 843 / 708 / 364 / 351 KB/s |
| pacing on | 96.9 / 108.0 / 109.7 / 111.7s | 98 / 88 / 86 / 85 KB/s |

- 单流 pacing off：1.43–1.65MB/s；pacing on 后四流全部骤降。
- 结论：**lane writer 上的应用层整形是负结果**。WS/TCP 本身已经在按 cwnd/发送缓冲
  排队，app 层再按“实测 inner delivery rate”sleep，等于在 TCP 的节拍上再串一层延迟，
  吞吐直接掉一个数量级；速率反馈也形成了“越慢越限”的闭环。
- 该形态不能上生产；默认关闭，未扩散。剩余的四流尾部是共享外层队列/公平调度问题，
  不是应用层突发问题。

## 1. 问题

- 页面/常规多资源加载已正常；4 个并发 9.4MB 下载时出现 3 条约 14s、1 条约 41s。
- 原因是流亲和把每条内层 TCP 流固定到一条 lane；lane 数少于并发流数时，共享 lane 的
  两条流互相排队，外层共享路径的浅队列还会放大拥塞。
- 单纯加 lane（warm3）已实测为负结果：4 条流全部降到 ~200–240KB/s，首屏变慢。

## 2. 为什么放在 lane writer

- 每条动态 lane 已有独立 `laneWriter` goroutine 和队列（`stripe_transport.go:888`）。
  在这里等待只延迟该 lane 的数据，不会阻塞 `peerWriteLoop`、控制帧或 ACK。
- 之前 `frame.PacedTransport` 失败的原因是在通用 transport 上同步 sleep，直接卡住
  peer write loop；本方案不复用它。

## 3. 设计

### 3.1 每 lane 一个令牌桶

- 令牌桶参数：`burst = 16KB`（可配置常量），令牌按 `rate`（bytes/s）补充。
- 数据批写入前 `wait(bytes)`；令牌不足时用 timer 等待，`select` 同时监听 lane.done /
  session.closed，可随时退出。
- 控制帧（`frame.IsControlFrame`）直通；批次里含控制帧则整批不 pacing，保证优先。
- 单批大于 burst 时允许一次“透支”到该批大小，避免永远等不到足够令牌。

### 3.2 速率来源

- 目标速率 = 该 lane 的实测 delivery rate（`laneFlow.snapshot().rate`）。
- 下限：`lanePaceFloor = 512KB/s`，上限：`lanePaceCeil = 8MB/s`。
- 速率样本为 0 或尚未建立时不 pacing（避免 0.9×0 把数据掐死）。
- 速率每 200ms 更新一次；rate 下降时逐步跟随，不瞬间把令牌桶降到 0。

### 3.3 开关与灰度

- 默认关闭。通过 `PHAETHON_LAB=1` + `PHAETHON_LANE_PACE=1` 开启（仅实验构建/节点）。
- 不进入用户配置、不进入控制台。
- 先在 deskzq→jf 单链路 A/B，通过后再评估是否默认开启。

## 4. A/B 验收

同一窗口、同一路径，至少 3 轮：

| 场景 | 指标 | 通过标准 |
|---|---|---|
| 4 × 9.4MB 并发 | 每条流耗时、max/min 比、总聚合 | max/min 明显下降，聚合不低于基线 |
| 单流 9.4MB | 耗时/速率 | 不低于基线 -10% |
| elephant 首屏/二次加载 | lastResourceEnd | 不劣于当前（首屏 ~6s、缓存 ~3.7–4.6s） |
| 30 分钟 soak | 无 stall、无连接重置、内存稳定 | fail=0 |

失败即关闭实验开关并记录数据，不回退现有自适应 lowat/warm lane。

## 5. 实现步骤

1. 新增 `p2p/lane_pacer.go`：令牌桶 + 可中断 wait + 单测（令牌补充、控制帧旁路、零速率）。
2. `stripeLane` 增加 `pacer *lanePacer`，`attachLane` 创建；`laneWriter` 在 SendBatch 前
   对数据批调用 `wait`。
3. 每 200ms 从 `laneFlow.snapshot().rate` 更新 pacer 速率（复用 lowatLoop 或 controllerLoop）。
4. 构建 lab 包，deskzq 自升级并设置 lab 开关，按 §4 验收。
5. 结论写入 `docs/issues/elephant-download-dribble.md`，代码保留在历史记录中。
