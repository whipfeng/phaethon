# gVisor 栈内集成方案：路由 / NAT / IPIP 下沉设计

## 状态：⚠️ 部分过时 - 被 `gvisor_route_selector_architecture.md` 更新

**本文档的前期调研和 ADR 仍然有效，但架构方案已被更新。**

**关键更新**（见 `gvisor_route_selector_architecture.md`）：
1. ❌ **取消 Tunnel NIC**：IPIP 封装在路由决策时完成，不需要独立的隧道 NIC
2. ✅ **RouteSelector 扩展点**：在 FindRoute 过程中做动态路由决策
3. ✅ **Link NICs**：每个直连 mesh peer 一个 NIC（而非共享 mesh NIC）

**本文档仍然有效的部分**：
- §2 gVisor 扩展点盘点（除 Tunnel NIC 相关）
- §3 关键调研详情（NAT、IPIP 解封装、混杂模式）
- §7 讨论脉络纪要（历史讨论记录）

**已过时的部分**：
- §4 D2（每出口节点一个隧道 NIC）→ 改为路由时封装
- §5.1 NIC 规划（隧道 NIC 201+）→ 改为 Link NICs
- §5.2 路由表（通告前缀 → 隧道 NIC）→ 改为 RouteSelector 决策
- §5.3 数据流（隧道 NIC 相关）→ 改为转发路径封装

---

**建议**：直接阅读 `gvisor_route_selector_architecture.md` 获取最终方案。

---

本文汇总 2026-09 至 2026-10 的架构讨论、源码调研与结论。
源码引用均基于锁定版本 `gvisor.dev/gvisor v0.0.0-20250428193742-2d800c3129d5`

---

## 1. 背景与问题

### 1.1 当前阻塞问题：TCP 回程自环（任务 #353）

v2 多 NIC 架构（已实现）存在 TCP 回程自环：Forwarder 的 SYN-ACK 被
FindRoute 的"直配抢占"送回栈内默认路由 NIC，形成 RST 循环。
根因分析见 §3.4。

### 1.2 前版结论勘误（2026-10-03）

| 文档 | 原结论 | 勘误 |
|------|--------|------|
| gvisor_routing_evolution.md | "DNAT 必须在路由决策前 → 无策略路由 → 死结，NAT 必须留在 TUN 边界" | 前提正确（Prerouting DNAT 确在路由前，ipv4.go:873），错误在"只能靠默认路由"——忽略了最长前缀明确路由（如 192.168.0.0/16 → TUN NIC）。**栈内 NAT 可行，无需 fork** |
| multi_nic_architecture_v2.md | "gVisor iptables 无法满足需求，手动 NAT 是正确方案" | 已过时。NAT 能力全覆盖（§3.1），且 v2 实现已实际用上 iptables SNAT（任务 #348） |

### 1.3 当前实现状态（v2，遗留自环）

`mesh/netstack.go` 现状：

| NIC | 角色 | 绑定 | 混杂 | Spoofing | 路由 |
|-----|------|------|------|----------|------|
| 1 | TUN | VIP /32（netstack.go:545） | 否 | 是 | VIP /32 → NIC 1 |
| 2 | mesh | GIP | 否 | 是 | mesh /8 → NIC 2 |
| 3 | IPIP 入口 | 无 | 是 | 否 | EIP /32 → NIC 3 |
| 4 | 统一分发 | 无 | 是 | 是 | 默认 → NIC 4 |
| 100+ | h_tunnel | 无 | 是 | 是 | — |

- 转发全开（v4/v6，netstack.go:349-350）
- mesh 出站拦截器保留（tun/engine.go meshOutboundCh）
- NAT：iptables SNAT（#348）+ 手动 NAT 移除（#350），nat.go 有未提交修改
- 默认路由指向 NIC 4 + 转发全开，正是自环的温床（§3.4）

---

## 2. gVisor 扩展点盘点（全部源码核实）

| # | 能力 | 扩展点 | 公开 API？ | 结论 |
|---|------|--------|-----------|------|
| 1 | 自定义 NIC | LinkEndpoint + CreateNIC | ✅ | 隧道 NIC / TUN / mesh NIC 全用它 |
| 2 | NAT | IPTables 5 hook + Target | ✅ | SNAT/DNAT/Masquerade 齐备（§3.1） |
| 3 | NAT 状态 | conntrack（内建） | ✅ | ICMP echo ID + 错误消息翻译全覆盖 |
| 4 | IPIP 解壳 | TransportProtocol 注册（构造时） | ✅ | proto=4 处理器（§3.2） |
| 5 | 解壳后备挂载 | SetTransportProtocolHandler | ✅ | stack.go:517，defaultHandler 优先于 UnknownDestination |
| 6 | 解壳后重注入 | InjectableLinkEndpoint.InjectInbound | ✅ | registration.go:1256，channel.Endpoint 实现 |
| 7 | 自定义网络协议 | — | ❌ 无此扩展点 | network/ 仅 arp/ipv4/ipv6，proto=4 **不能**作为网络协议监听 |
| 8 | 最长前缀选路 | FindRoute | ⚠️ 缺陷 | 直配抢占，需 fork 补丁 #1 |
| 9 | 转发优先语义 | handleValidatedPacket | ⚠️ 缺陷 | 本地交付优先于转发，需 fork 补丁 #2 |

关键事实：**封装、解封装、NAT 三项全部零 fork 可做**；fork 只剩两个小补丁。

---

## 3. 关键调研详情

### 3.1 IPTables / conntrack 覆盖 nat.go 全部能力

nat.go（558 行）手工实现的能力与 gVisor 内建能力对照：

| nat.go 能力 | gVisor 对应 | 证据 |
|-------------|------------|------|
| SNAT（src → VIP + 端口） | SNATTarget（显式地址）/ MasqueradeTarget | iptables_targets.go:281 / :381 |
| DNAT（回程还原） | DNATTarget（Prerouting） | iptables_targets.go:186 |
| 端口分配与映射表 | conntrack 表 | stack/conntrack.go |
| 5 分钟空闲清理 | conntrack TTL | 内建 |
| ICMP echo ID 翻译 | conntrack ident 跟踪 | conntrack.go:73-96 |
| ICMP 错误消息内层翻译 | conntrack unwrap | conntrack.go:948-970 |

Hook 位置（network/ipv4/ipv4.go）：Prerouting :873（路由前）、Input :1228、
Forward :685/:785、Output :536、Postrouting :575。

注意点：

- MasqueradeTarget 取 NIC 主地址（AcquireOutgoingPrimaryAddress）；VIP 不绑任何
  NIC，因此用 **SNATTarget 显式地址池**，不用 Masquerade
- conntrack 需要显式规则才能建立状态
- loopback NIC 跳过 PREROUTING（ipv4.go:866）——TUN NIC 非 loopback，不受影响
- DNAT/SNAT 允许的 hook 与 Linux 相同（Prerouting/Output、Postrouting/Input），
  其他 hook 配置会 panic——写规则时注意

### 3.2 IPIP 封装 / 解封装的正确位置

**Linux 参照模型**（讨论澄清：封装从来不是"协议监听"，监听是入站概念）：

```
出站: 路由表 → ipip0 设备 xmit → 套壳 → 直落 underlay 设备
入站: 物理网卡 → proto=4 协议模块 → 剥壳 → 重新入栈
```

**我们的对应设计：**

TX（封装）——隧道 NIC 的 xmit：

```
路由表: <通告前缀> → 隧道 NIC 201
   ↓
NIC WritePackets: 套壳 (src=本机EIP, dst=出口EIP, proto=4)
   ↓
直落 mesh 链路（hop 表选路），不回栈二次路由
```

外层包**不回栈重新路由**——那会引入 EIP 网段路由歧义和绕环风险，Linux 也不这么干
（ip_tunnel_xmit 在 xmit 内解析 outer dst 后直落 underlay 设备队列）。

RX（解封装）——proto=4 协议处理器，**零 fork**：

```
mesh 帧 → 原样注入 NIC 2
   ↓ 栈: 地址匹配命中（EIP 绑在 NIC 2）
   ↓ 栈: DeliverTransportPacket 按协议号分发
proto=4 处理器: 剥壳
   ↓
channel endpoint InjectInbound 重注入 NIC 1（TUN，混杂模式）
   ↓
NIC 1 混杂接受内层包（dst=8.8.8.8 等非本地地址也被接受）
   ↓
本地交付 → Forwarder 拦截（无监听）→ 代理拨号器 → mesh/internet 出口
```

**关键**：重新注入 NIC 1（而非 NIC 2）是为了利用 NIC 1 的混杂模式，
让 transit traffic（dst=互联网/mesh）被接受为"本地"，然后被 Forwarder
拦截走代理路径——与单 NIC 时期逻辑一致（commit 7df7f48：writeLoop 分类
other → re-inject → 混杂接受 → Forwarder）。

源码证据链：

1. `stack.Options.TransportProtocols` 支持构造时注册任意协议号
   （stack.go:187-191, 198-199, 420；TransportProtocol 接口 registration.go:262）
2. 本地交付对无 endpoint 的包调 `HandleUnknownDestinationPacket`
   （stack/nic.go:893）——剥壳逻辑挂载点；`SetTransportProtocolHandler`
   （stack.go:517）为等价备选
3. `channel.Endpoint` 实现 `InjectableLinkEndpoint.InjectInbound`
   （registration.go:1253-1264）——重注入出口
4. IPv4 Parse 链：ipv4.go:1789-1813（parseAndValidate）、:1835（hasTransportHdr
   = !More && FragmentOffset==0）、stack.go:2309（ParsePacketBufferTransport 调
   协议自己的 Parse）

实现注意点：

- `DeliverTransportPacket` 要求 TransportHeader 非空（nic.go:853 附近检查）——
  协议的 `Parse` 塞 4 字节假端口头即可（ParsePorts 读它返回假端口）
- 分片的外层包不走此路径（hasTransportHdr=false）——mesh 链路 MTU 自控，
  外层不分片，无影响
- 本版本无运行时 RegisterTransportProtocol（只能 New 时经 Options 注册）——
  不影响：netstack 初始化本就在我们手里

**概念澄清**（讨论中"套壳在外面，解壳在里面？"的疑问）：
两者都在栈框架的扩展点上，**决策全在栈内**（选路归栈、分发归栈），
我们只实现扩展点上的"动手"代码。与旧的栈外拦截（readLoop 绕过栈）有本质区别。

### 3.3 混杂模式作用范围（沿用既有结论）

混杂模式只影响入站接受（handleValidatedPacket 的地址检查），不影响转发路径
（forwardUnicastPacket 用 allowTemp=false 查找，不看混杂位）。
详见 gvisor_routing_evolution.md「gVisor 包处理流程源码分析」。

### 3.4 FindRoute 直配抢占缺陷（fork 补丁 #1）

`Stack.FindRoute`（stack.go:1461-1610）的实际逻辑：

```
按表序遍历路由表（非最长前缀序）:
  if 目标 ⊄ 该路由前缀 → continue
  if (id==0 || id==route.NIC) 且 getAddressEP 成功:
      立即返回                       ← 直配抢占：不看后续更长前缀
  else if 转发开启且 chosenRoute 未记录:
      chosenRoute = 此路由, 继续     ← 兜底只记第一条
遍历结束 → 用 chosenRoute（id!=0 时还要求请求 NIC 有地址）
```

缺陷：

1. **表序即优先级**：SetRouteTable 的写入顺序决定匹配优先级，前缀长度不参与
2. **直配短路**：请求 NIC（或 id=0）名下第一条包含目标的路由直接返回，
   即使其他 NIC 有更长前缀
3. **兜底条件苛刻**：转发兜底要求 forwarding 开启；id!=0 时还要求请求 NIC
   自持地址，否则 ErrHostUnreachable

自环场景：Forwarder SYN-ACK（dst=宿主机 IP）查询时，默认路由 NIC（NIC 4）
直配命中即返回 → 包回灌栈内 → RST 循环。

补丁方案：直配阶段不短路，改为**跨 NIC 收集最长前缀**再决出（约 100 行）。
修复后表序不再敏感，消除一整类路由陷阱。

### 3.5 转发优先语义（fork 补丁 #2）

gVisor 现状：本地交付优先，非本地才转发（findEndpointWithAddress 先行）。

我们需要的规则（讨论中收敛）：

```
长于默认前缀的路由命中 → 转发
默认路由 / 无路由      → 本地交付（Forwarder / socket）
```

原因：

- 默认路由触发转发会让宿主机 raw-IP 流量在 TUN ↔ 栈之间死循环
- fakeIP ⊂ mesh 100.64.0.0/10，整个 /10 入表会让 fakeIP 流量被转发而非
  交付 → **按远端子网逐 /16 入表，本段子网不入表**

补丁方案：TUN 入站判定价于 handleValidatedPacket 的地址检查处
（约 30 行）。

---

## 4. 设计决策（讨论结论，ADR 候选）

### D1 子网 = 路由表项，不是 NIC

- 讨论起因："每个 mesh 子网段是不是一个 NIC？"
- 结论：NIC 是链路抽象，子网是路由数据。Linux 不为每个子网建网卡。
- 主流做法：BGP → FIB，控制面同步进路由表，不逐包匹配通告表。

### D2 每出口节点一个隧道 NIC

- 通告路由经 IPIP 送出口节点；隧道 NIC 等价 Linux ipip0，per-node
  （outer dst = 该节点 EIP，固定三元组：src=本机EIP / dst=节点EIP / proto=4）
- **隧道 NIC 是"哑"的**：创建时绑死该节点 EIP，数据面只做机械套壳，
  不查询、不选 nodeid（选路在控制面，见 D7）
- 内部先例：h_tunnel NIC 100+（uplink-only，SO_BINDTODEVICE）
- 出口节点选择（静态优先 / sticky / 排除自身）沿用
  `mesh_ipip_smart_routing.md`，本文不重复

### D3 原始 mesh 子网流量走共享 mesh NIC 2

- 讨论起因："一个链路要两张 NIC？"——不用。
- 远端子网流量 RAW 不封装（**线上协议不变式**：mesh 帧格式不变，
  旧节点互通不受影响），只有通告路由流量才 IPIP。
- P2P 子链路是 L2 不透明管道：帧 = IP 包，hop 表不看内容。

### D4 封装在隧道 NIC xmit，直落链路

- 见 §3.2 TX。outer dst 的路径解析在 xmit 内完成 = 查 hop 表，
  与 Linux ip_tunnel_xmit 结构一致。

### D5 解封装在 proto=4 协议处理器，零 fork

- 见 §3.2 RX。收端骨架归零：mesh 收帧后唯一动作 = 原样注入 NIC 2，
  不再有"outer.dst==本机EIP && proto==4"的写死分支。
- **重新注入目标 = NIC 1（TUN，混杂模式）**：让内层 transit traffic
  （dst=互联网/mesh）被混杂模式接受为"本地"，然后被 Forwarder 拦截
  走代理路径——与单 NIC 时期逻辑一致（commit 7df7f48）。
- 若 gVisor 上游将来出现 ipip 网络协议包，处理器代码平移即可。

### D6 EIP 与 GIP 一起绑定 mesh NIC 2

- D5 的前提：解壳的"dst==本机EIP"地址匹配由栈完成，不再手写。

### D7 通告路由经控制面同步进 FIB

- mesh 通告变化 → 去抖动重建 SetRouteTable（栈内查表即可区分
  "raw → NIC 2" 还是 "IPIP → 隧道 NIC"）。
- 讨论起因："通告路由也直接加入路由？"——是。但**不是原样全加**：
  控制面先按出口选择算法把每个前缀解析为 1 个最佳节点，FIB 每个
  前缀只落一条（指向胜者节点的隧道 NIC）。多节点通告同一前缀 =
  控制面的候选集；节点故障 / sticky 切换 = 控制面重新同步改指次优，
  数据路径全程无感知。
- **选路发生在哪个面**（讨论澄清，防止误解为"扩展点里选 nodeid"）：

  | 选什么 | 在哪 | 时机 |
  |--------|------|------|
  | 出口节点（去哪儿上网） | 控制面（路由同步） | 按前缀，通告变化时 |
  | 链路（怎么到那个节点） | mesh hop 表 | 逐包，不变（D8） |

  隧道 NIC 扩展点零选择——套壳即走。
- 同构关系：隧道 NIC = ipip0（配置时定 remote，xmit 不选路）；
  控制面 = BGP daemon（选路装 FIB，每前缀一条最优）；hop 表 =
  IGP/链路层解析。

### D8 hop 表留在 mesh 层，职责不变

- 链路/路径选择永远在 mesh 层（TX 时隧道 NIC 套壳后交 hop 表选链路；
  raw 流量 NIC 2 写出同样交 hop 表）。
- hop 表在 TX 侧的位置 = Linux 隧道 xmit 里那次 outer 路由解析。

### D9 NAT 下沉 IPTables + conntrack，删除 nat.go

- 见 §3.1 对照表。NAT 状态管理、ICMP 翻译全部交给栈。
- 回程路径：PREROUTING DNAT（conntrack 还原）→ 最长前缀路由回 TUN NIC
  ——不需要策略路由（勘误后的核心结论）。

### D10 fakeIP = NIC 地址 + TCP Forwarder 兜底

- fakeIP 子网绑 NIC 1（TUN），无监听端口 → Forwarder → 域名还原 →
  ServeConn（mesh admin 链路，端口无关，沿用现状）。

### D11 转发语义规则（配合补丁 #2）

- 见 §3.5。核心：**默认路由不触发转发**。

### D12 fork 范围限定为两个补丁

- 补丁 #1：FindRoute 最长前缀（§3.4，约 100 行）
- 补丁 #2：TUN 入站转发优先（§3.5，约 30 行）
- 风险评估：pkg/tcpip 3.3MB 纯 Go，windows7/go-legacy-win7 构建矩阵
  不受 fork 影响；锁定版本 + 补丁 patch 文件管理。

---

## 5. 目标架构（阶段 2 完成态）

### 5.1 NIC 规划

| NIC | 角色 | 绑定 | 混杂 | Spoofing | 说明 |
|-----|------|------|------|----------|------|
| 1 | TUN | fakeIP 子网 / 宿主网段 | 是 | 否 | 栈↔宿主边界；**混杂模式让 transit traffic（IPIP 内层包 dst=互联网/mesh）被接受为本地，然后被 Forwarder 拦截走代理路径** |
| 2 | mesh | GIP + EIP | 否 | 否 | raw mesh + IPIP 解壳入口 |
| 201+ | 隧道（每出口节点） | 无 | 否 | 否 | IPIP 封装，uplink-only |
| 100+ | h_tunnel | 无 | 是 | 是 | 沿用现状 |

**VIP 不绑定任何 NIC**：VIP 只是 NAT 转换地址（PREROUTING DNAT 的
匹配键），绑定会让栈误判为本地交付。

### 5.2 路由表

```
{宿主网段 192.168.0.0/16 → NIC 1}     // 栈→宿主回程
{远端 mesh 子网 /16 逐条 → NIC 2}      // raw（协议不变式）
{通告前缀 → 隧道 NIC 201..}            // IPIP
（无默认路由；本段子网不入表——见 D11）
```

回程 NAT：dst=VIP 的响应包在 PREROUTING 被 conntrack DNAT 还原为
宿主机 IP → 命中 /16 路由 → NIC 1。**无需 VIP 路由**；conntrack 未命中
（如重启后残留包）自然丢弃。

### 5.3 数据流

```
① 域名/fakeIP（代理路径）
TUN → NIC1(混杂接受) → Forwarder → 代理拨号(src=GIP)

② mesh 子网（raw）
TUN → NIC1 → 转发 → NIC2 → hop表 → P2P
（旁路网关直访 mesh 节点；Postrouting SNAT → GIP）

③ 通告路由（IPIP）
TUN → NIC1 → 转发 → 隧道NIC201
   → 套壳(src=本机EIP, dst=出口EIP, proto=4)
   → 直落 mesh 链路（hop表选路）

④ mesh 帧（收端，零分支）
P2P → 帧原样注入 NIC2 → 栈分发：
   proto=4  → 剥壳 → 内层重注入 NIC1(混杂) → Forwarder → 代理拨号
   GIP/EIP  → 本地交付（admin / socket）
   fakeIP   → Forwarder → 域名还原 → ServeConn
   其他     → 混杂接受 → Forwarder → 代理拨号（transit traffic）
```

**关键**：NIC 1 混杂模式让所有非本地 dst（互联网/mesh）也被接受，
然后被 Forwarder 拦截走代理路径——transit traffic 不经直连互联网出口，
而是通过代理拨号器（规则引擎）决定怎么出去（mesh 到另一节点 / 本地互联网）。

⑤ 本机 socket 出站
socket(src=GIP) → Output → 路由 → NIC2 raw / 隧道NIC 套壳
```

### 5.4 职责划分

| 层 | 职责 |
|----|------|
| gVisor 栈内 | 选路、转发、TTL、NAT（iptables+conntrack）、TCP/UDP 终结（Forwarder，含 transit traffic 经混杂模式接受后拦截）、IPIP 解壳、ICMP 生成 |
| mesh 层（栈外） | hop 表链路选择、P2P 连接管理、通告收发、帧收发（收帧→注入 NIC 2，一个动作） |
| 栈外其余 | 拦截器（仅阶段 1）→ 阶段 2 退役 |

**协议骨架 vs 策略**：收端的协议骨架（地址匹配 / 协议分发 / 注入）写死
是正确的（Linux 同样如此，写一次永不改）；策略类判断（isMeshDest、
通告表逐包匹配、出口选择、域名还原）全部消灭，转化为数据
（路由表、地址绑定、hop 表）。

---

## 6. 实施阶段

### 阶段 1（零 fork）：拓扑 B，修自环

**目标**：消除 TCP 回程自环（#353），落地 NAT 下沉，不动 mesh 协议。

1. 删除 NIC 3（IPIP）与 NIC 4（loopback 默认路由）——直配抢占失去载体
2. 栈内不开转发；mesh 出站仍走拦截器（栈内每个目标单一合法出口，
   FindRoute 直配不会误判）
3. fakeIP / 宿主网段地址绑定 TUN NIC 1；NIC 1 开混杂接受 raw-IP 入站
4. NAT 按需走栈内 iptables（规则矩阵在任务拆解时定，见 §8）
5. 删除 nat.go 与 writeLoop 残留

**验收**：
- [ ] 本地经 QG 旁路网关：域名、mesh 域名（https://gg.phn/api）、raw IP 全通
- [ ] 长连接 TCP（SSH over mesh、trojan 映射）无 RST 循环
- [ ] UDP / QUIC 流量正常
- [ ] 部署顺序：VM 先行验证 → QG（铁律）

### 阶段 2（fork 演进）：路由语义 + IPIP 隧道

**目标**：拦截器退役，收端骨架归零，通告路由 IPIP 全栈化。

1. fork 仓库 + 补丁 #1（FindRoute 最长前缀）+ 补丁 #2（转发优先）
2. EIP 绑 NIC 2；proto=4 协议处理器 + channel 重注入（§3.2）
3. per-node 隧道 NIC + 通告路由 FIB 同步（D2/D7）
4. 拦截器退役：mesh 收帧全部原样注入 NIC 2（零分支）
5. 出口节点选择接入（mesh_ipip_smart_routing.md）

**验收**：
- [ ] QGT 环境验证（不可测 TUN/旁路 NAT 的限制见 gvisor_routing_evolution.md）
- [ ] IPIP 封装/解封装正确性（outer 三元组、内层不动）
- [ ] 通告路由触发 IPIP；静态优先 / sticky / 故障切换
- [ ] 阶段 1 全部验收项回归

**回退**：两阶段独立可回退；阶段 2 出问题退回阶段 1 拓扑（协议不变，
互操作无影响）。

---

## 7. 讨论脉络纪要

按讨论顺序记录关键问题与收敛点，供评审追溯：

| 问题 | 结论 |
|------|------|
| gVisor 可否 fork 定制？ | 可（Apache 2.0，纯 Go，win7 构建不受影响） |
| 定制后 4-NIC 还有意义吗？ | 无意义。NAT/路由/封装下沉，拓扑简化 |
| 路由下沉留外部多态接口？ | 上游已有扩展点（NIC/Target/协议注册），无需自造 |
| DNAT 死结成立吗？ | 不成立（勘误，§1.2） |
| 每子网一个 NIC？ | 不。子网=路由表项（D1） |
| 通告路由逐包匹配？ | 不。FIB 同步（D7，主流做法） |
| 通告路由都进 FIB、扩展点里选 nodeid？ | 不。控制面选好节点装 FIB（每前缀一条）；扩展点零选择，套壳即走（D2/D7） |
| 封装解封装在哪做？ | 套壳=隧道 NIC xmit，解壳=proto=4 处理器（D4/D5） |
| 一个链路两张 NIC？ | 不。raw 走共享 mesh NIC（D3） |
| 到对端还要写死判断吗？ | 收端写死只剩协议骨架（合法）；策略全消灭（§5.4） |
| 定制改造意义大吗？ | 意义收敛为两个补丁（D12），其余全用公开扩展点 |
| IPIP 是入栈协议监听吗？ | 解壳理想是；上游无 ipip 包 → 但 TransportProtocol 注册即可，零 fork（§3.2） |
| 套壳在外解壳在内？ | 都在栈框架扩展点上，决策全在栈内（§3.2 澄清） |
| IPIP 解封后送 Forwarder 执行代理能力？ | 是。重新注入 NIC1(混杂) → Forwarder 拦截 transit traffic → 代理拨号（与单 NIC 时期逻辑一致，commit 7df7f48） |
| 封装直接交链路吗？Linux 也这样？ | 是。xmit 内解析 outer dst 后直落 underlay，不回栈（D4） |
| fork 了解壳能进栈吗？ | 能，且验证后发现零 fork 也能进栈（§3.2 证据链） |

---

## 8. 风险与未决问题

1. **fork 维护税**：两个补丁需随版本升级重放。缓解：锁定版本 + patch 文件 +
   补丁尽量小（合计约 130 行）。
2. **iptables 规则矩阵待细化**（阶段 1 任务拆解时定）：
   - SNAT 作用面（哪些流需要 SNAT → GIP/VIP，哪些走 socket 源地址）
   - conntrack 显式规则的 hook 布点
   - 与拦截器的先后关系（拦截器出口的包不进栈，天然不碰 NAT 规则）
3. **阶段 1 地址绑定细节**：fakeIP 子网以何种 AddressProperties 绑 NIC 1、
   Forwarder CreateEndpoint 的 id 传参（id=0 或 NIC 1）需在实现时验证。
4. **IPIP inner 包的 Postrouting SNAT**：旁路网关流量经隧道发出时 inner src
   的改写位置（隧道 NIC Postrouting vs 出口节点）待定。
5. **通告路由规模**：SetRouteTable 全量重建在路由条目大时的开销——
   去抖动 + 增量对比，量级预期 < 百条，风险低。
6. ~~**UDP raw 流量路径**~~（已解决）：NIC 1 混杂模式接受所有入站包，
   UDP raw traffic（dst 非本地）被 Forwarder 拦截走代理拨号路径，
   与 TCP 行为一致。

---

## 9. 参考文档

- `gvisor_routing_evolution.md` —— gVisor NAT 能力边界调研（含 2026-10-03 勘误）
- `multi_nic_architecture_v2.md` —— v2 架构（本文取代其结论）
- `mesh_ipip_smart_routing.md` —— IPIP 出口节点选择算法
- `gvisor_icmp_ttl_research.md` —— TTL/ICMP 调研
- `mesh_traceroute_design.md` —— traceroute 设计
