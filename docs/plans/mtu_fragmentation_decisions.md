# MTU / 分片最终结论（2026-11-27）

状态：已落地（fork `fc4dcb0` + `f7bba12` + `225e98f`，phaethon `6ddc43e`）。

本文件用于终止重复讨论。下面每一条都是最终结论；如无新事实，不要重开。

## 1. 尺寸预算

术语先定死，后文不再混用：

- **内层**：业务/用户的 IP 包，即从 TUN 或 modeb 进入 mesh、由 RouteSelector 处理
  的包。本文件第 7 节讨论的分片/重组**全部发生在内层**，分支 1–4 只是内层包不同
  的去向，不是“内层 vs 外层”的区别。
- **外层**：承载 mesh 链路的 OS 级 TCP/UDP（UDP leg / TLS）。它只负责把内层包
  （必要时加 IPIP 头）搬过去，对 mesh 的尺寸决策透明，自身不参与本文件的推导。

- 内层预算 `mesh.InnerMTU = 1400`，是唯一真源：
  - TUN 适配器 MTU（`tun.OverlayMTU`）取 1400；
  - **所有 mesh 出向路由**（分支 2 直连 LinkNIC、分支 3 IPIP 封装）的 route MTU
    都取 1400（`stack.SetIPIPInnerMTU` + `RouteDecision.MeshEgress`）。内层预算
    与我方节点是否是 egress 端、目标是否走 IPIP 无关：只要包要被交给 LinkNIC，
    就必须 ≤1400。
- 内层包经 IPIP 封装后 = 1400 + 20B IPIP 头 = 1420，可完整放进 1500B 的外层
  UDP leg 路径，不依赖路径分片。
- 通道 link endpoint 的 1600 只是封装头余量，不是端到端尺寸依据。

## 2. 决策一：MSS 广告不再手写

之前 phaethon 在 TUN 入口**手写**钳制 TCP MSS（1360），属于自己实现的一套，
与 gvisor 的尺寸处理重复且只覆盖 TCP。已删除。

现在：gvisor 由 route MTU 原生推导 MSS。判定依据是“route 是否带显式预算”
（`Route.ExplicitMTU() != 0`），而不是 `NeedIPIP`——因为分支 2（直连 LinkNIC）
同样以内层整包 1400 为预算：

- 普通路由（NIC 推导，无显式预算）：`route MTU` 是网络层 MTU，已扣除 IPv4 头
  （1380），`MSS = 1380 - 20B TCP 头 = 1360`。
- 显式内层预算路由（分支 2/3，`route MTU = 1400` 是**内层整包预算**，含 IPv4 头）：
  `MSS = 1400 - 20B IPv4 头 - 20B TCP 头 = 1360`。

同一判定也用于发送侧 `MaxPayloadSize`（`maxPayloadBudget`），否则本机发起的 TCP
（modeb 代理出站等）会按 1380 发段，满段内层包 1420 超预算，到出口再被重切。

单测 `TestAdvertisedMSS` 锁定：普通路由 1380 与显式预算 1400（直连/IPIP）都广告
1360。

## 3. 决策二：分片/重组全部收拢到 gvisor fork

- **入向（TUN 和 modeb）**：IPv4 分片在 `HandlePacket` / `handleLocalPacket`
  中、`parseAndValidate` 之后立即重组（fork patch #15），早于 Prerouting、
  NAT 与 RouteSelector。上游只在本地投递路径重组，导致转发的非首片以空
  `TransportHeader` 进入 Postrouting，被需要传输头的 SNAT 规则误判为
  `RuleDrop`（ICMP 分片全丢就是这么来的）。
- **出向**：所有写向 LinkNIC 的 IPv4 包都受路由显式预算约束
  （`writePacketPostRouting` 用 `Route.ExplicitMTU()` 取分片上限），TCP 由 MSS、
  UDP/ICMP/转发包由分片共同保证内层包 ≤1400；IPIP 分支在封装前另用
  `fragmentation.PacketFragmenter` 按 1400 切内层，外层路径不再产生分片。
- **IPIP 外层包例外**（`PacketBuffer.IsMeshOuter`）：外层包预算是链路 MTU
  （内层 1400 + 20B IPIP 头 = 1420），不套内层 1400 预算。外层包一旦被 IP 分片，
  对端在**帧层**解封装（不重组外层分片），会把非首片当成整包注入 netstack——这是
  致命错误，因此外层路由必须保持 1420 ≤ LinkNIC MTU 且不分片。
- **forwarder 返回方向**：不做任何手工切片，交给 gvisor 的内层重组/分片保证外层
  不超 MTU。
- **零拷贝**：重组沿用 gvisor `fragmentation` 实现（引用计数 + 视图拼接），
  没有在 phaethon 侧复制/重写包体。

## 4. 不做什么

- 不再在 phaethon 侧手写 MSS/MTU/分片逻辑。
- 不监控/告警 UDP leg 的 `legFragmentLimit=1472` 超限计数：UDP 无 MSS，截断会
  破坏数据报语义；该计数只作为“出现即异常”的指标。
- 不为大包场景强制应用层切片；分片是网络正常能力，由 gvisor 负责。
- 不做“无 NAT 直通碎片”快车道：入口统一重组是 NAT 需要传输头的**正确性**需求，
  不是尺寸问题（`tun.OverlayMTU == mesh.InnerMTU == 1400` 早已对齐）。按流判定
  “是否要 L4”一旦判错会静默破坏端口 SNAT，且只影响超大 UDP（TCP 因 MSS≤1400
  零成本），不划算。

## 5. 日志

fork 内保留模块级 debug 日志（`gvisor` 模块）：

- `ipv4 reassembly buffered` — 收到分片、尚未凑齐；
- `ipv4 reassembly complete` — 重组完成（含 id/src/dst/proto/len）；
- `ipv4 reassembly error` — 重组失败（含原因）。

默认不开 debug；排查时按模块开启即可。

## 6. 验证顺序

单测（`mesh` / `tun`）→ vm 基础功能 → qg / jf / gg。

## 7. 路由分支与分片/重组边界（2026-11-27 补）

本节全部是**内层**行为；外层（OS 级 TCP/UDP 承载）不参与，也不会在这里被分片。
不变式一句话：**入向重组、出向分片。** 重组发生在任何内层 IPv4 包进入 netstack
的入口（`HandlePacket` / `handleLocalPacket` 的 1217/1288 门控）；分片发生在任何
超内层预算的 IPv4 包写出前（普通出口 626 / IPIP 封装前 1033、1150）。

RouteSelector 四分支（route_selector.go），四者都是内层包的不同去向：

| 分支 | 场景 | IPIP 封装 | 内层分片/重组 |
|---|---|---|---|
| 1 | 本地 mesh /16 | 无 | 入口重组（来料已碎时） |
| 2 | 其它 mesh /16 | **无**（LinkNIC 直连） | 入口重组；出口按内层预算 1400 切（route MTU=1400） |
| 3 | 非 mesh 宣告前缀 | **有**（20B IPIP 头） | 入口重组；封装前按 1400 切内层，加头后 1420 在外层 MTU 内 |
| 4 | 默认回退 NIC1 | 无 | 入口重组（来料已碎时） |

- **IPIP 封装头只出现在分支 3**（src=本机 EIP、dst=对端 EIP、proto=4），它是内层
  路径上的 +20B 开销，不是本文件所称的“外层”。分支 1/2/4 的内层包直接交给链路
  承载，没有额外封装头。
- **分支 2 与分支 3 共用同一个内层预算（1400）**：区别只是分支 3 多 20B IPIP 头。
  分支 2 不再回落到 LinkNIC MTU（1600）——那是本次修复的根因：直连链路若是 UDP
  leg 承载，1600 帧会超 `legFragmentLimit=1472` 被外层分片，中间设备丢非首片，
  TCP/大 UDP 卡死。现在分支 2 的 route MTU=1400，TCP MSS=1360，UDP/ICMP 写出口
  按 1400 分片。
- **外层（IPIP）包不套内层预算**：`IsMeshOuter` 标记的包按链路 MTU 分片，正常
  最大 1420，不会被拆成外层分片（见第 3 节）。
- **中继节点不碰内层**：`tryTransitFastPath` 在进 gvisor 前按外层 dst 透明转发，
  既不解封装也不重组，只做 TTL-1 + 校验和。
- **尺寸对齐已到位**：入口重组不是尺寸问题，而是 NAT 需要传输头（非首片没有
  L4）。尺寸由 `InnerMTU` 统一控制。
- **外层不进 fork 逻辑**：mesh 链路用的 OS 级 UDP/TCP 只搬运封装后的内层包，
  其分片（若有）由 OS 按链路 MTU 处理，不属于本文件的决策范围。

结论：内层策略**不再改代码**。当前“入口重组、出口分片、中继透明、封装后不超
外层 MTU”自洽且偏保守，v103 按此定型。
