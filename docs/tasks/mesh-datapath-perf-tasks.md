# mesh 数据面吞吐修复任务

> 设计方案：[mesh-datapath-perf-design.md](../plans/mesh-datapath-perf-design.md)
> 分析记录：[mesh-large-file-slow.md](../issues/mesh-large-file-slow.md)

## 阶段一：调参 + 正确性修复（零行为风险）

- [x] 1.1 gVisor TCP 栈级调参：Send/ReceiveBufferSizeRangeOption Max→16MB + CongestionControlOption("cubic")（netstack.go stack.New 后、forwarder 注册前；fork 不动）
- [x] 1.2 fork calculateAdvertisedMSS 钳制上限 1460（endpoint.go:622；MTU 1600 不动；对端旧版兼容性查证后否决单端 MTU 改法）
- [x] 1.3 streamTransport.SendBatch 部分写检测（frame/transport.go:174，io.ErrShortWrite）
- [x] 1.4 fork 删 forwarder.go:78-79 corrID Sprintf + phaethon/util import（fork 仓独立 commit）
- [ ] 1.5 三平台 build + Mac 真机回归（phase_a/b/d、iperf 基线）——build 通过，回归 pending（mesh 链路 flapping）

## 阶段二：可观测性

- [x] 2.1 TUN 写失败计数 tunWriteDrops（netstack.go + GetChannelDropStats 新 key）
- [x] 2.2 fork channel.go 满丢计数 + Endpoint accessor（fork 独立 commit）+ phaethon 采集 NicTxDrops
- [x] 2.3 admin 前端 channelDropStats 分项计数展示（app.js）

## 阶段三：队列与调度

- [x] 3.1 dataQueue 字节预算制（per-peer 8MB 全局预算 + 每队列 512 帧上限，enqueue CAS 记账/出队退款/入队失败退款，统一主动/被动两侧 + 重连重置）
- [x] 3.2 真 DRR：deficit 出队闸门 + quantum 64KB 连续出队 + 空队列 deficit 归零 + 负余结转（单流 batch ~43 帧）
- [x] 3.3 写路径 writev：Send/SendBatch 走 net.Buffers writev（非 TCP 回退单 buffer 单次写；payload 超限校验）
- [ ] 3.4 Mac 真机验证：跨 GG 大文件 DataDrops 趋零、batch 长度 >1、多流公平、内存上限

## 部署验证（用户指令 2026-11-05：所有阶段完成后部署两个环境验证效果）

- [ ] 部署 QG + Mac 两环境正式版，大文件传输实测对比修复前后吞吐、drop 计数全零

## 明确不做

阻塞背压 / ping 改动 / 单 NIC 化 / QUIC 化（见设计文档）
