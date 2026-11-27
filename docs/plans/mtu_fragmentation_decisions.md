# MTU / 分片最终结论（2026-11-27）

状态：已落地（fork `fc4dcb0` + `f7bba12`，phaethon `6ddc43e`）。

本文件用于终止重复讨论。下面每一条都是最终结论；如无新事实，不要重开。

## 1. 尺寸预算

- 内层预算 `mesh.InnerMTU = 1400`，是唯一真源：
  - TUN 适配器 MTU（`tun.OverlayMTU`）取 1400；
  - IPIP 路由 MTU 取 1400（`stack.SetIPIPInnerMTU`）。
- 外层 = 内层 + 20B IPIP 头 = 1420，可完整放进 1500B 的 UDP leg 路径，不依赖
  路径分片。
- 通道 link endpoint 的 1600 只是封装头余量，不是端到端尺寸依据。

## 2. 决策一：MSS 广告不再手写

之前 phaethon 在 TUN 入口**手写**钳制 TCP MSS（1360），属于自己实现的一套，
与 gvisor 的尺寸处理重复且只覆盖 TCP。已删除。

现在：gvisor 由 route MTU 原生推导 MSS（`route MTU - 40`），IPIP 路由 MTU 就是
内层预算 1400，因此广告 MSS 自然落在 1360，且对 TCP 之外同样成立。

## 3. 决策二：分片/重组全部收拢到 gvisor fork

- **入向（TUN 和 modeb）**：IPv4 分片在 `HandlePacket` / `handleLocalPacket`
  中、`parseAndValidate` 之后立即重组（fork patch #15），早于 Prerouting、
  NAT 与 RouteSelector。上游只在本地投递路径重组，导致转发的非首片以空
  `TransportHeader` 进入 Postrouting，被需要传输头的 SNAT 规则误判为
  `RuleDrop`（ICMP 分片全丢就是这么来的）。
- **出向**：IPIP 封装前用 `fragmentation.PacketFragmenter` 按路由 MTU 切内层，
  外层路径不再产生分片。
- **forwarder 返回方向**：不做任何手工切片，交给 gvisor 的内层重组/分片保证外层
  不超 MTU。
- **零拷贝**：重组沿用 gvisor `fragmentation` 实现（引用计数 + 视图拼接），
  没有在 phaethon 侧复制/重写包体。

## 4. 不做什么

- 不再在 phaethon 侧手写 MSS/MTU/分片逻辑。
- 不监控/告警 UDP leg 的 `legFragmentLimit=1472` 超限计数：UDP 无 MSS，截断会
  破坏数据报语义；该计数只作为“出现即异常”的指标。
- 不为大包场景强制应用层切片；分片是网络正常能力，由 gvisor 负责。

## 5. 日志

fork 内保留模块级 debug 日志（`gvisor` 模块）：

- `ipv4 reassembly buffered` — 收到分片、尚未凑齐；
- `ipv4 reassembly complete` — 重组完成（含 id/src/dst/proto/len）；
- `ipv4 reassembly error` — 重组失败（含原因）。

默认不开 debug；排查时按模块开启即可。

## 6. 验证顺序

单测（`mesh` / `tun`）→ vm 基础功能 → qg / jf / gg。
