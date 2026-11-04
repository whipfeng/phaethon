# mesh 数据面吞吐修复 设计

> 2026-11-05。分析依据：docs/issues/mesh-large-file-slow.md（事实/dump/参考/架构研判 §9）。
> 原则：只改 phaethon 侧能解决的先走；fork 改动最小化且独立提交；队列行为改造放最后；
> 不阻塞背压（HoL 冻结入站 goroutine，§6 研判事实 1）；不动单写者/不动 ping 路径。

## 根因链（修正后的事实）

1. **gVisor TCP 全默认**：Reno + 缓冲 range Max 4MB → 窗口硬顶 Max/2=2MB ÷ RTT
   （117ms ≈ 137Mbps 理论顶，Reno 爬坡实际远低于此）；Tailscale 实测 Reno 22 vs CUBIC 68Mbps。
   phaethon 零调参（SetTransportProtocolOption 全仓零命中）。SACK/RACK/TS 默认已开。
2. **丢包塌缩**：队列链满 → tail drop 内层 IP 包 → 内层 Reno RTO 200ms + cwnd=1。
   单流队列深 32/64 帧（48/96KB），跨 GG BDP ≈ 400+ 帧（~600KB @100ms×50Mbps），
   深度差一个数量级；帧层无重传。
3. **MTU 错配**：NIC1 channel MTU 1600 vs TUN 1500 → 栈产生 >1500B 包写 TUN
   静默丢（netstack.go:1976，无计数）。
4. **正确性 bug**：streamTransport.SendBatch 丢弃 n，部分写未检测（frame/transport.go:174）
   ——可破坏帧流，与性能无关但同属写路径，顺手修。
5. **可观测性缺口**：dataQueue 满丢**已有计数**（p2p.go:851-857，/api/mesh 暴露但
   前端不展示）；**无计数**：NIC1 channel 满丢（fork，完全静默）、TUN 写失败丢、
   drrOutput 阻塞时长、SendBatch 部分写。

## 阶段一：调参 + 正确性修复（零行为风险）

### 1.1 gVisor TCP 栈级调参（仅 phaethon，netstack.go stack.New 后）

```go
// netstack.go stack.New(...) 之后、CreateNIC 之前
s.SetTransportProtocolOption(tcp.ProtocolNumber, &tcpip.TCPSendBufferSizeRangeOption{
    Min: 4096, Default: 1 << 20, Max: 16 << 20})
s.SetTransportProtocolOption(tcp.ProtocolNumber, &tcpip.TCPReceiveBufferSizeRangeOption{
    Min: 4096, Default: 1 << 20, Max: 16 << 20})
s.SetTransportProtocolOption(tcp.ProtocolNumber, &tcpip.CongestionControlOption("cubic"))
```

- 窗口硬顶 2MB → **8MB**（÷2 硬编码不变，autotune 自动顶到新 Max，endpoint.go:1329）
- CUBIC 影响此后新建的 endpoint —— 必须在 forwarder 注册（:1817）前设置；
  #11632 修复已在 base（snd.go:1100 注记），无 panic 顾虑
- 不改 MinRTO（200ms = Linux 默认）、不动 ModerateReceiveBuffer（autotune 保留）
- fork 零改动

### 1.2 通告 MSS 上限钳制 1460（fork 单点钳制，替代 MTU 改动）

核实结论：netstack.go:739-743 注释载明 MTU 1600 的历史原因——对端按 link MTU 1600
通告 MSS 1560，回程包 >1500 写本端 TUN 失败。**只改本端 NIC1=1500 治不了对端旧版
发来的 1560B 包**；而 LinkNIC 必须保持 1600（IPIP 外层 +20B 余量，link MTU 低于
1520 会导致外层包 ErrMessageTooLong）。两条路由（NIC1 本地 1500 / LinkNIC 出口 1600）
无法用单一 MTU 统一。

改法：fork `calculateAdvertisedMSS`（endpoint.go:622）对 maxMSS 加全局上限
**1460**（= 1500 - 40，TUN 端到端安全），一处钳制覆盖全部栈内 TCP endpoint
（forwarder accept / SYN cookie / 主动 connect，SYN-ACK 在内）。fork 单 commit。
- 两端升级后：所有栈内 TCP 包 ≤1500，TUN 写失败路径（:1976）自然消失；
  IPIP 外层 ≤1520 ≤ LinkNIC 1600 余量保持
- 未升级对端仍发 1560 → 本端 TUN 写失败但**有计数**（2.1），不劣于现状
- MTU 1600/1600 保持不动；代价：隧道单包 1560→1460（~6% 包数），可接受
- 顺带把 :739-743 注释更新为本设计 rationale

### 1.3 SendBatch 部分写修复（frame/transport.go:174）

```go
n, err := t.conn.Write(buf)
if err == nil && n != len(buf) {
    return io.ErrShortWrite   // 上层已有失败→Close 路径（p2p.go:640），语义不变
}
```

### 1.4 fork 热路径日志删除（独立 commit，fork 仓）

- 删 `forwarder.go:78-79` corrID Sprintf（SYN 判断前每包无条件执行，4 处使用全为
  LogModuleDebug 参数）；连带删 `phaethon/util` import（a481bb0 为诊断引入）
- 功能零依赖，排障等效信息由 phaethon forwarder 回调 per-连接日志替代

## 阶段二：可观测性（只加计数，不改行为）

### 2.1 TUN 写失败计数（phaethon）

- netstack.go:1986 default 分支加 `tunWriteDrops.Add(1)`；Netstack 结构体加字段；
  并入 `GetChannelDropStats()`（mesh.go:2931）新 key `TunWriteDrops`

### 2.2 NIC1 channel 满丢计数（fork，最小改动）

- fork `pkg/tcpip/link/channel/channel.go` `queue.Write` default 分支（:94 DecRef 处）
  加包级原子计数器，Endpoint 加导出 accessor（如 `DroppedPackets()`）
- phaethon writeLoop 周期采集进 channelDropStats 新 key `NicTxDrops`
- 依据：该丢弃完全静默（WritePackets 部分写吞错返回 nil，栈统计也不增长，
  channel.go:278-291 + nic.go:449-467），不改 fork 无从观测

### 2.3 前端展示（admin）

- app.js mesh 页 channelDropStats 面板：DataDrops/ControlChDrops/meshInboundDrops/
  TunWriteDrops/NicTxDrops 全量展示（数据早已在 API，仅 UI 缺）

## 阶段三：队列深度与调度（有行为变化，最后做、单独验证）

### 3.1 数据队列：帧数上限 → 字节预算上限

现状：每队列帧数上限 32/64（两侧不一致），单流固定落 1 队列 → 单流缓冲 48/96KB
≪ BDP（跨 GG ~600KB）；满即 tail drop → 内层 RTO。

方案（对齐 fq/BQL 按字节限额的主流做法）：
- per-peer 数据面全局**字节预算**（初值 8MB，常量集中定义可调），
  取代每队列固定帧数作为丢弃判据
- 每队列保留帧数上限 512（防单流爆炸），enqueue 判据：
  `队列满 || 全局预算耗尽` → drop + 计数
- 记账：enqueue 时 `budget -= len(data)`，drrLoop 出队时 `budget += len(data)`
  （出队点 p2p.go:285-287，单点归还无泄漏面）；peer 断开重建队列时预算清零
- 两侧（主动/被动）统一此逻辑，消除 32/64 不一致
- 内存上界：8MB/peer 显式有界（现状满帧理论 1GB 反而无界于 pool 切片），更可控

### 3.2 真 DRR（deficit 成为出队闸门）

现状：deficit 只记账不闸门，单流每轮 1 帧 → batch 恒退化为单帧写
（2 syscall/帧）。修法（对齐 sch_drr）：
- 出队条件 `len(req.data) <= deficits[idx]`，满足则扣减并**连续出队**至不满足
- `deficits[idx] += quantum` 时机保持每轮一次；quantum 1500 → **64KB**
  （单流一轮可出 ~43 个 1500B 帧 → SendBatch 真正聚合）
- 空队列跳过（现状已是非阻塞 try，无 active list 需求——256 固定扫可接受）
- 单写者/drrOutput/drrLoop 结构不动；hashFlow/盐不动

### 3.3 写路径 syscall 优化（与 3.2 联动才有意义）

- `NewStreamTransport` 保留 `*net.TCPConn`；SendBatch 改 `net.Buffers`
  （Linux 走 writev 免聚合 memcpy），非 TCPConn 回退现有路径
- 单帧 Send 用 `net.Buffers{hdr[:], payload}` 合并为单次写（2 syscall → 1）
- 不改帧格式/不重排语义；partial-write 修复（1.3）先行兜底

## 明确不做（本轮）

- 阻塞/限额等待式背压（冻结 TUN readLoop/meshInboundLoop，HoL 全线停摆）
- ping 任何改动（cancelPing 回滚已正确，控制通道失明）
- per-link LinkNIC 单 NIC 化（issue §9.1：非瓶颈，fork patch 迁移面大，长期候选）
- QUIC/多连接/UDP leg 常驻化（issue §9.2：真架构演进候选，独立议题）
- TUN readLoop 多 goroutine 化（CPU 非瓶颈，规模隐患记录待议）

## 验证计划

每阶段独立验证、独立提交，前阶段回归通过再做下一阶段：

1. **阶段一**：三平台 build；Mac 真机开关 TUN + phase_a/b/d 回归；
   LAN iperf3 经 mesh 单流吞吐对比（基线先测）；跨 GG 传大文件吞吐对比；
   `ifconfig utun7` 确认 MTU 1500 后无 EMSGSIZE 类 warn
2. **阶段二**：人为制造对端慢消费（限其网卡或挂起进程），确认四类 drop 计数
   在 /api/mesh 可见、前端可见；正常传输时全零
3. **阶段三**：单流大文件跨 GG 传输：DataDrops 趋零、SendBatch 平均 batch 长度
   >1（打日志或计数）、吞吐不低于阶段一；多流并发公平性抽查
   （两条流各应得 ≈50%）；内存 profile 确认预算生效（上限 8MB/peer）
4. 全程：CPU 观测（Mac 0% 基线不回退）、错误日志归零
