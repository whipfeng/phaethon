# 全局 deadline/timeout 审视（2026-11-03）

## Context

用户命题：phaethon 定位是中转设备（middlebox），传输语义应对端到端透明。TCP 自己有 RTO/keepalive，应用层自己有协议级心跳。**数据面在途连接不应由我们设空闲超时**。只有两类合理：① 握手/建连阶段（对端静默握着我们不放的资源）；② 我们自己发起的探测/查询。本文档是对全库 deadline/timeout 的逐点亲验分类与处置意见。

## 资源泄漏研判（删除空闲杀后的死连接清理机制）

删除 A 类空闲杀的前提是死连接有兜底清理。逐条验证：

- **服务器入向 accepted conn**：`server/base.go:90-94 configureTCPConn` 已设 OS TCP keepalive（30s period）→ socks5/http/trojan/direct 入向死客户端由 OS 在 ~60s 内检测。
- **gVisor 内层 TCP**：`mesh/netstack.go` applyTCPKeepalive 应用于 5 处端点，MeshTCPKeepalive 默认 30s idle/10s interval/3 count → TUN/mesh 内层死连接 ~60s 检测。
- **dialer 出站 conn**：`dialer/bind.go:144/228` KeepAlive 30s → 上游死服务端有兜底。
- **服务器入向数据泵本就没有空闲杀**：`util.Relay/RelayWithRateLimit`（direct/http/reverse/socks5/trojan 五处共用）纯 io.Copy，无超时——A 类删除后与入向语义统一。
- **UDP（A2）无 keepalive 可用**：泄漏面=客户端整机崩溃且未 close socket，每次 2 goroutine；NAT 绑定过期后该流本就永久无声。低频、低量，接受。
- **trojan UDP association（A3）**：TLS 控制连接走 AcceptLoop → keepalive 30s 生效，死客户端有兜底。

结论：A1/A2/A3 删除/改正后无资源泄漏缺口、无语义变化（仅"idle 被杀"变为"idle 存活"，即目标行为本身）。B1 删除后死/卡死对端由读侧心跳检测（p2p.go:1414，30s 无帧杀 transport，Close 唤醒写方）兜底；活但慢的对端=TCP 背压自然传播，正确。

## 分类与处置

### A. 数据面空闲杀连接（违反中转哲学 — 建议删除/改正）

**A1. TCP relay 5 分钟空闲看门狗** `handler/connection_handler.go:479-498`
`relay()` 被全部 TCP 路径调用（本地 admin 回环 :201、mesh dial :338、direct/proxy dial :338），即**每一条经 phaethon 中继的 TCP 连接**空闲超 5 分钟即被强杀（conn.Close()+target.Close()）。idle SSH、HTTP keepalive、数据库长连接、MQTT 全部受害——OS/TCP keepalive 才是正确机制，不是应用层拍脑袋的 5 分钟。
**处置：删除看门狗。** 死连接清理交给 TCP 协议自身（对端 FIN/RST、keepalive）；NAT 映射过期是客户端自己的责任。

**A2. UDP relay 30 秒空闲看门狗** `handler/connection_handler.go:561-578`
`relayUDP()`（全部 TUN/mesh UDP 的唯一中继点 :456）空闲 30s 杀。idle QUIC、低频 DNS（>30s 间隔的查询每次都被杀）。
**处置：删除看门狗。** 注意代价：UDP 无 FIN 语义，若客户端不 close 且永远不来包，goroutine 滞留。缓解：NAT 绑定本身 30-60s 过期，真死流不会再有包；netstack UDP association 由客户端 socket 生命周期决定。滞留风险真实但量级低，接受。

**A3. Trojan UDP association 60s 空闲读超时杀** `server/trojan.go:260-263`
主循环 `SetReadDeadline(60s)`，`err != nil` 即 return——客户端 60s 无 UDP 包则整条 association（TLS 控制连接）被拆。同文件 :210/:385 的 UDP 回复读超时是 timeout→continue（存活），此处语义不一致。
**处置：改为 timeout→continue**（与同文件 :210 及 socks5 :354 对齐），association 由客户端决定寿命。

### B. 写 deadline 杀链路（判断项 — 建议放宽，待裁决）

**B1. frame 传输 30s 写 deadline** `frame/transport.go:120`（控制帧 5s）/`:146`
Send/SendBatch 写超时→error→调用方拆链路。拥塞时对端 TCP 接收缓冲满 30s = 杀整条 mesh 链路/_BIND 通道，内层所有流陪葬，而非背压传播。
**处置意见：删除写 deadline**——TLS/TCP 本身有重传，写阻塞即天然背压；死对端由读侧心跳检测（见 D1）兜底。控制帧 5s 同理删。

### C. 握手阶段 deadline（合理 — 保留）

trojan/vless 15s（dialer 出站握手，本次批次 2 已加）、sshHandshakeTimeout、dialer/trojan_bind.go:78、server/http.go:28 / server/socks5.go:35 请求头解析 30s（解析完即清零）、server/reverse.go:280 首帧 10s（读完清零）、server/reverse.go:218 匹配等待 60s、main.go:2537 健康检查 5s、dialer/control_client.go:84。均为"资源在我手、对端可能静默"阶段，符合哲学 ①。

### D. 死对端检测（合理 — 保留）

**D1. mesh 链路 30s 心跳超时** `p2p/p2p.go:1414-1444`：1s ping 定时器 + 任何入帧刷新 lastSeen，30s 无帧=对端真死才杀。帧传输 60s 读 deadline（frame/transport.go:85）是其二级兜底。
**D2. reverse 协议心跳**：双方 25-30s 心跳帧 < 各处 60-70s drain deadline，正常。

### E. drain-loop deadline 当可中断 sleep（无害 — 保留）

reverse.go / htunnel / trojan UDP 回复 / socks5 UDP 多处 `SetReadDeadline(60-70s)` + `timeout→continue`：仅为周期性检查 closed 标志，超时不杀连接。风格上像 sleep 但无害。

### F. 自建协议 scoped timeout（合理 — 保留）

`mesh/netstack.go:1545` QueryInternalDNS 2s（自己发起的内部查询）；`server/htunnel.go:657` readSource = long-poll 窗口 scoped 于单次请求；`mesh/stall_monitor.go:451` admin dump 连接。

## 验收

部署后 VM 验证：idle SSH 挂 10 分钟不断线；低频 DNS（间隔 >30s）正常解析；iperf 吞吐不退化。
