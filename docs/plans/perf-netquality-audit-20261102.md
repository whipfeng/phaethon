# 全局代码审视：性能与网络质量（2026-11-02）

## Context

全环境部署 v0.8.0-239-g8f23e13 完成后，按用户要求对全库做性能/网络质量审视。3 个并行审查角度（数据面热路径 / 定时器与资源泄漏 / 网络质量专项）共产出 19 条候选，**逐条亲验代码后确认 14 条、推翻 1 条**。gvisor-fork 定制单独审视（2 角度 9 候选，全部亲验确认 9 条），结果见文末。

## 确认发现（按严重度）

### 一、网络质量

1. **RED 随机丢内层 TCP 段（TCP-over-TLS 塌陷风险）** `p2p/p2p.go:807-854`，单流队列 cap 32（`:1062`），深度 >50% 起随机丢（最高 80%）。内层 TCP 视之为拥塞丢包 → RTO（200ms–1s）→ 同一条外层连接上重传风暴，与外层拥塞窗口互相放大。单链路 >几 Mbps 批量传输即吞吐塌陷。修法：内层包改阻塞式背压（enqueue 阻塞让 gvisor 自然降窗）。
2. **relayUDP 双向共享同一 buffer（真数据竞争）** `handler/connection_handler.go:553-616`，一个 `buf` 被 netstack→target（`:588`）与 target→netstack（`:605`）两个 goroutine 同时写，双向 UDP（DNS/QUIC）并发时数据报互相践踏。修法：每方向独立 buffer。
3. **Trojan/VLESS TLS 握手无 deadline** `dialer/trojan.go:144`、`dialer/vless.go`。对端 accept 后静默 → 握手永久挂起。连带：健康检查 goroutine（`main.go:2483`）外层 5s 超时返回，但 goroutine 不放信号量槽位（`main.go:2375` cap 8）；8 个挂死 = `checkGroupHealth` 卡在 `sem <- struct{}{}`（`:2378` 无超时发送）永久停摆。修法：握手前 `SetDeadline(15s)` 成功后清零。
4. **无 MSS clamp，IPIP 封装后必然分片**。全库无 MSS 处理；内层 MSS 1460 + IPIP 外层 20B = 1520 > 1500 MTU，每包两分片，单分片丢 = 整段重传。修法：LinkNIC MTU 降 ~1400 或转发路径 clamp SYN MSS option。
5. **WriteFrame 每帧 2 次写 + 每帧 2 次 SetReadDeadline** `frame/frame.go:109-115`、`frame/transport.go:85-87`。3 字节头与 payload 各成一次 TLS 记录/syscall；recv 路径每帧 2 次 deadline syscall。修法：头+payload 拼 buffer 单次 Write（SendBatch 已是正确写法）；deadline 循环外设一次。
6. **"ACK 优先"误判小包数据且仍被 RED 丢** `p2p/p2p.go:935-977`。ACK 标志 + payload<100B 的数据包（HTTP 请求、TLS record、SSH 按键）进共享 FIFO ackQueue（cap 256，跨流 HoL），且 `!isControl` 照样过 RED。修法：ACK 分类限纯 ACK（payload==0），ack/dns 队列豁免 RED。
7. **meshInboundCh 满即丢内层包 → cross-node DNS 5s SERVFAIL** `p2p/p2p.go:1559-1567`（cap 4096，4 worker 消费），转发超时 5s（`mesh/dns.go:50`）。修法：DNS 内层包独立深队列或阻塞入队。

### 二、每包 CPU 浪费

8. **WritePacket 双份 Flatten** `mesh/link_nic.go:183-184` + `:150-151`：为打日志先 Flatten，WritePackets 内部再 Flatten，单包路径两次 1500B 拷贝。
9. **debug 日志参数无条件求值** `link_nic.go:158-161`、`mesh/mesh.go:1160-1167`：每包 `net.IP().To4()` 等分配，日志关闭仍付费。修法：加级别前置守卫。
10. **SelectRoute 每次分配 slice + String() 全部前缀** `mesh/netstack.go:200-204`，只为一条 debug 日志。
11. **u-turn 检测每中转包 `Mask().String()` 做 map key** `mesh/mesh.go:1179`。修法：改 4 字节数组 key。
12. **TUN 出口单 goroutine 逐包 syscall** `mesh/netstack.go:1875`（Windows wintun 写开销大，环满阻塞 = 全局 egress HoL）；**入口每包 make+copy** `tun/manager.go:818-819`。修法：批量写 + sync.Pool。

### 三、慢速累积（周~月级）

13. **DNSCache 过期条目永不删除** `mesh/dns.go:84-102` + **fakeip DB 行只增不减** `mesh/fakeip.go:144-148`（`Release` 仅测试调用）——按域名 churn 周级涨数万条，重启全量加载变慢。修法：sweep tick 顺带清过期 cache；wraparound 驱逐时同步删 DB。
14. **DNS forwarder panic 重启无锁换 channel** `mesh/dns.go:163-170`，与 Stop 竞态泄漏 listener goroutine（仅 panic 时）。

### 推翻

- ~~mesh 入站单消费 goroutine~~：不成立，`p2p/p2p.go:44` `meshInboundWorkers=4` 已是并行消费。

### 核实干净

路由查找（routeTrie+atomic）、DRR 批量出队、frame readBufferPool、所有 NewTicker 均有 Stop、TCP relay 双工双 goroutine+32KB buffer、DNS hijacker 应答路径、各 loop 均有退出条件、用户向 dial 路径全部设了 NoDelay（仅 P2P/reverse bind 控制连接未设，低优先）。

## 建议处理顺序

**2→3**（正确性，改动小）→ **1→4→6→7**（网络质量）→ 8-12 一波清扫（机械改动）→ 13-14 顺手。

## gvisor-fork 定制审视

范围：`3a3c088..HEAD` 23 提交、16 文件、+1390/-70。两个并行角度（性能 / 正确性）共产出 9 条候选，逐条对照 fork 代码亲验，**9 条全部确认**。定制面区分方式：`3a3c088` 是 go.mod replace 指向的上游基线（v0.0.0-20250428193742），该 commit 之后的提交即全部定制。

### 确认发现（按严重度）

1. **每包同步文件 I/O ×15 处（fork 中最严重）**。全库 `grep os.OpenFile` 得 15 处调试日志，三档：
   - **每个 UDP 包无条件 2 次**：`iptables.go:361-369`（CheckPrerouting）、`iptables_targets.go:353-368`（natAction，每个有 conntrack tuple 的 NAT 包，且未判协议，IPv6 包被当 IPv4 解析）。
   - **每个 DNS(53) 包又 8 次**：`conntrack.go:577-597`（命中+未命中双路径）、`transport_demuxer.go:166/187/205`（handlePacket 三处）、`udp/endpoint.go:902/924/938/966/1002`（HandlePacket 四处，bufferFull 分支覆盖全丢弃路径）、`iptables.go:422`（SNAT）。
   - 一次性 `sync.Once`（`ipv4.go:1128/1629`）无害。
   折算 DNS 应答接收路径单包约 10 次 open/write/close syscall——mesh 内 DNS 延迟劣化与跨节点 5s SERVFAIL 的重大叠加嫌疑。修法：全部删除（调试期已过）或原子开关门控。
2. **IPIP 内层 TTL 入端不递减** `ipv4.go:913-915`：NeedIPIP 分支在 `forwardPacketWithRoute`（TTL 递减在 :779）之前分流进 `forwardWithIPIPEncapsulation`，内层包头原样入隧。隧道入端不算一跳 → TTL≤1 的包多穿节点、traceroute 少计跳数、与 decapsulate 后 HandlePacket 全量重处理叠加使环路包多活近一倍跳数。修法：封装前对内层做 TTL 递减与 ≤1 检查。
3. **routeDecisionCache 无界增长、无过期** `stack.go:668-695`：key 仅 dst，无容量/TTL，且被收包热路径副作用填充（`ipv4.go:1501` 每收包调 `RouteSelectorLocalStack`）。扫描/探测遍历大量 dst 时内存只增不减。修法：加上限 + 过期 sweep。
4. **patch #14 自愈重查不回写缓存** `stack.go:1716`：cached NIC 失效时 `freshDecision := s.routeSelector(remoteAddr)` 只赋局部变量，永不回写——该 dst 每次 FindRoute 都现跑 selector（Dijkstra），直到全局 ClearRouteDecisionCache；同段还有硬编码 `s.nics[1]` fallback（:~1728）。恰在链路 flapping 窗口（GG 场景）每包全量选路。
5. **routeSelector 回调在 s.mu RLock 下执行** `stack.go:683`（经 findRouteInner:1675 与 RouteSelectorLocalStack:701）：回调若调用任何取 `s.mu.Lock` 的 Stack API 即自死锁，且拉长读锁持有阻塞写者。当前 phaethon 回调（`mesh/route_selector.go:85`）只调自己的 SelectRoute，**未触发，属潜在风险**。修法：锁内只取回调指针，锁外调用。
6. **DNAT 回程 NIC 名查找持锁线性扫**：`GetNICByName`（stack.go:1083）持 **s.mu 写锁**扫 NIC 表，被每个 DNAT 出站包调（ipv4.go:559 区域）——写锁阻塞全部并发读路径，回程突发即全局毛刺；`FindNICIDFromName`（:2721）RLock 版被每个 DNAT 转发回复包调（ipv4.go:866 区域）。修法：改 RLock + name→NICID 预解析缓存。
7. **corrID 每收包分配 ×2** `ipv4.go:1508` + `:1786-1790`：`phaethonCorrID` 每包 `fmt.Sprintf`（net.IP×2 转字符串），handleValidatedPacket 与 DeliverTransportPacket 前各算一遍；`LogModuleDebug` 虽有级别守卫（util/util.go:436）但 corrID 字符串本身无条件构造。修法：改 lazy 或包级缓存键（4 字节数组）。
8. **IPIP 双重全包拷贝 + 每外层包全量 FindRoute**：`pkt.ToView()`（ipv4.go:967）整包拷贝 → `newIPIPOuterPacket` 内 `buffer.MakeWithData`（:1041）再拷一遍；write 路径 :1064 同构。两路径每外层包各跑一次全量 `FindRoute` 路由表遍历（:1005、:1097）。修法：PacketBuffer 直接拼接外层 header（ReserveHeaderBytes 已有先例），外层路由按 egressEIP 缓存。
9. **demux 每 DNS 包无条件 RLock 全端点 map 遍历** `transport_demuxer.go:644-651`：遍历 `eps.endpoints` 在 `LogModuleDebug` 之外，info 级别照跑。修法：移入级别守卫内或删除。

### 核对干净的关键路径

conntrack Input 钩子 fullChecksum=true 是增量校验和更新而非校验丢弃；`originalInputNIC` 写持 cn.mu、读持 RLock，conn 条目走既有 bucket 超时清理无新增泄漏；`DeepCopyForForwarding` 保留 NICID/InputNICName/OutputNICName 正确支撑 Postrouting `-i` 匹配；IPIP 外层包直走 forwardPacketWithRoute 无递归封装/ping-pong 环路；patch #12 v4（ipv4.go:1500）与 v6（ipv6.go:1402）转发优先分支语义对齐；RouteDecision cache 独立 RWMutex 设计正确；patch #8 panic→log+drop 正确。
