# Mesh 链路误断与重建后遗症修复

**创建时间**: 2026-11-07
**状态**: 已批准，实施中（F6 于实施中追加，用户已批准）
**来源**: mesh-per-flow-ordering.md §3.11 遗留观察（4 流并发约 13s 全零停顿）+ 排查中顺带发现的问题

## 一、问题与日志证据

### P1 GG 进程崩溃：LinkNIC 删除时的竞态
- GG 今天崩溃 3 次（04:17 后、07:12 后、09:58:36），栈：`LinkNIC.InjectInbound` link_nic.go:90 ← `HandleMeshFrame` mesh.go:1123 ← `meshInboundLoop` p2p.go:798，空指针。
- 原因：link_nic.go:61 判断 `l.dispatcher != nil`，:90 再读一次。期间 `RemoveNIC` 调 `Attach(nil)`（gVisor nic.go:360），第二次读到 nil。
- 触发频率高：VM 每约 30s 重连一次（见 P5），每次都删/建一次 Link NIC。

### P2 链路在大流量下被误判死亡
- 复现（10:34~10:38，4 流并发拉 GG 8MB）：
  - GG 10:36:33 `link dead: 10 consecutive pings without pong for ... nodeID=jf`；
  - JF 10:36:35 对 gg 同样判死（会话已存活 37m42s）；
  - 两边删 Link NIC、2 秒后重建。第 1 轮 4 流都在 120s 内没下完。
- 原因：
  - ping 每 1s 发一次，5s 未回记一次丢。连续 10 次（约 15s 收不到任何 pong）就判死（p2p.go:2163）。
  - GG 发往 JF 的积压约 18MB：应用层队列预算 8MB（p2p.go:241）加内核发送缓冲约 10MB（之前 `ss` 实测 Send-Q 约 10MB）。按 GG 出口约 300KB/s 算，约 60s。
  - GG 的 ping 排在数据队列；pong 虽走优先队列，也要排在内核 10MB 之后。两个方向都超过 15s。
  - 实际上这期间两边都在持续收到对方的数据帧，链路是活的。

### P3 链路重建后，老连接永久发不出去
- 重建后，GG 上老连接每次发送都报 `[TCP-SEND] failed ... nicID=5 ... ErrInvalidEndpointState`。nicID=5 是已删除的旧 Link NIC，新的是 69。
- VM 上 FIN-WAIT1 连接也一直卡在已删的 NIC 37/45/47/51/55 上。
- 原因：gVisor TCP 端点建连时把 route 固定在 `e.route`。NIC 删除后 `Route.WritePacket` 恒返回 ErrInvalidEndpointState（route.go:541），重传也全部失败，直到超时。
- 这就是 §3.11 记录的"13s 全零停顿"：流卡死，不是网速问题。

### P4 gVisor sackedOut 计数漂移（上游代码 bug）
- `enterRecovery`（snd.go:1167）直接 `SackedOut = 0`，但写队列里仍有已 SACK 段（`seg.acked=true`）。这些段之后被累计确认时，snd.go:1796 又减一次，计数变负。
- 单测采样 442 次中 285 次为负，最小 -81。
- 影响：RACK 的 `SackedOut >= 3 则乱序窗口置 0`（rack.go:337）判断失真。
- 同类问题：MSS 调整（snd.go:439）和减计数（snd.go:1796）用 scoreboard 判断，而加计数（snd.go:1446）用 `seg.acked`。RTO 清空 scoreboard（snd.go:671）后两者不一致，计数会只增不减。

### P6 链路重建后老连接恢复极慢（F3 单测发现）
- 单测 `TestRerouteAfterNICRemoval`（gvisorfork/tcp_linkrebuild_test.go）：删 NIC 200ms 后重建，F3 换路由成功（`[TCP-REROUTE] nic 2 -> 3`），但 5 次中 2~4 次 30s 传不完 64MB。
- 采样证据：
  - 接收端 `rcvNxt == SndUna`、乱序队列为空、无 SACK、dupack=0；
  - 每次 RTO 后只前进 1~2 个段，随即进入 sack-recovery，`Outstanding` 跳回约 270，cwnd=5，约 7KB/s 爬行。
- 原因：
  - RTO 后首个 ACK 到达，RACK 把其余旧在途段标为丢失（`seg.lost`）并进入恢复；
  - `SetPipe`（snd.go）只按 SACK 记分板判断丢失，不认 `seg.lost`；RTO 已清空记分板，于是约 270 个丢失段仍算作在途；
  - RACK 的 DoRecovery 只重传 writeNext 之前的段，RTO 回退后 writeNext 之后的旧段也不会被它重传。
  - Linux 在途量 = packets_out − sacked − lost + retrans，丢失段不计入。
- 影响不限于重建：任何"RTO 后再进恢复"的流都会这样爬行。

### P5 VM 每约 30s 断一次 GG 会话
- VM 经 SSH 跳板 `LAOYU_SSH_TEST`（47.238.79.7）连 GG trojan。
- VM 日志：每次 `session ending ... EOF` 同一秒都有 `[ssh] keepalive timed out, closing stale connection`。两端同时 EOF，是 VM 自己关了 SSH 连接。
- 原因：dialer/ssh.go:276，keepalive 回复只等 5s，超时一次就关闭整条 SSH 连接。
- 附带：该处日志格式串少 `%s`，输出 `%!(EXTRA string=...)`。

## 二、修复方案

### F1 LinkNIC 分发器加锁（修 P1）
- 照搬 gVisor `channel.Endpoint` 写法（channel.go:217-232）：
  - `LinkNIC` 增加 `dispMu sync.RWMutex`；
  - `Attach` 与 `IsAttached` 加锁；
  - `InjectInbound` 在 RLock 下取一次 `d := l.dispatcher` 后解锁，判空后用 `d` 投递。
- 不在锁内投递，避免与 `RemoveNIC` 持有 `s.mu` 形成死锁。
- 投递给正在删除的 NIC 与上游 channel 行为相同，安全。

### F2 ping 判死增加"入站静默"条件 + 限制内核发送积压（修 P2）
1. **判死条件**（p2p.go:2163）：
   - 改为"连续 10 个 ping 无 pong **且** 最近 10s 未收到任何入站帧（`peer.lastSeenNano`）"才判死。
   - 有入站数据说明对端活着、入方向通。若是本端出方向坏了，对端收不到帧，它的 30s heartbeat 会关闭连接，本端收到 EOF，同样能断开。
   - 判死被入站活跃挡下时，记一条限频 Info 日志。
   - 同步修改 p2p_v8_simplified_protocol.md §4.4。
2. **内核发送积压上限**：
   - 外层 lane 的 TCP socket 设置 `TCP_NOTSENT_LOWAT = 128KB`（Linux/darwin），只限"未发出"的字节，不限在途窗口，不影响吞吐。
   - 积压回到应用层后，由现有 DRR 公平调度与 8MB 预算管理，ping/pong 不再排在 10MB 内核缓冲之后。
   - 取底层 socket：逐层解包（`NetConn()` 接口，如 tls.Conn），拿到 `*net.TCPConn` 才设置；取不到（h_tunnel 流、SSH 通道等）或 Windows 平台则跳过，行为不变。
   - 在 lane 建立处统一设置，记一条 Info 日志（成功/跳过原因）。

### F3 NIC 删除后 TCP 端点重新选路（修 P3，gVisor fork）
- `stack.Route` 新增导出方法 `IsValidForOutgoing()`（包装现有 `isValidForOutgoing`）。
- `Endpoint.sendTCP`（connect.go:848）失败且 `r == e.route && !r.IsValidForOutgoing()` 时，调用新增的 `e.rerouteLocked()`。调用方均已持有 `e.mu`：
  - `e.RouteFunc != nil` → `BuildRouteFromDecision(e.RouteFunc(), ...)`（与 connect 一致）；
  - 否则 → `FindRoute(0, 本端地址, 对端地址, 协议)`，走 RouteSelector。NIC id 用 0，因为旧 NIC 已不存在。
  - 成功：释放旧 route，替换 `e.route`，记 Info 日志 `[TCP-REROUTE] old nic → new nic`。
  - 失败（对端已不可达）：保持现状，下次重传再试。
- 本次发送失败的包不在原地重发，由 TCP 重传定时器正常重发，新包走新路由。
- 本端/对端地址不变，连接四元组不变，对端无感知。
- 范围只限 TCP（本次证据都是 TCP），UDP 暂不处理。

### F4 sackedOut 记账统一（修 P4，gVisor fork）
- 不变量：`SackedOut = 写队列中 seg.acked=true 的段数之和`。
- 删除 `enterRecovery` 里的 `s.SackedOut = 0`。Linux 进入恢复也不清 `sacked_out`。
- snd.go:439 和 snd.go:1796 处 SackedOut 的增减条件，改为 `seg.acked`，与 snd.go:1446 对称。`Outstanding` 记账仍按 scoreboard 判断，不改。
- RTO 路径不动 `seg.acked`，也不动 SackedOut，按不变量自然一致。
- 单测：复用 sackedOut 采样框架，跑乱序 + 丢包传输，断言全程 `SackedOut >= 0` 且等于实际 acked 段数。

### F5 SSH 跳板 keepalive 容忍（修 P5）
- dialer/ssh.go keepAlive：
  - 单次回复超时 5s → 15s；
  - 连续 2 次超时才关闭（最长约 30s，与 P2P heartbeat 一致）；
  - 收到回复即清零。
- 修正格式串：ssh.go 全部 9 处日志都把 proxy 名作首参却没有对应 `%s`，统一加 `[%s] ` 前缀。
- 第 1 次超时记 Warn `keepalive reply late`，不新发请求，继续等同一个回复（SSH 全局请求按序应答）。

### F6 SetPipe 不计入已丢失段（修 P6，gVisor fork）
- `SetPipe` 规则 (a)：以下段不再计入 pipe：
  - RACK 已标丢失（`seg.lost`）的段；
  - 位于 `writeNext` 及之后的段（RTO 回退后尚未重传，等同 Linux `tcp_enter_loss` 全部标丢）。
- 正常恢复中 writeNext 指向未发送数据（≥ SndNxt），循环在此之前已退出，不受影响。
- 段被重传时 `seg.lost` 会被清除，重传段照常计入。
- 效果：重建单测 8/8 通过（约 2.5s）；其余 fork 回归单测全部通过。

## 三、不做的事
- ❌ 重连时复用原 Link NIC（用户否决）。
- ❌ 改 ping 走优先队列（ping 走数据队列是有意设计，用于测数据面排队，§4.4 Phase C）。
- ❌ 改 8MB 应用层预算（F2 后再观察；如 ping RTT 仍过大，另立项）。

## 四、实施与验收

**顺序**：
1. F1、F4、F3（fork 改动，单独提交）；
2. F2、F5（phaethon 侧）；
3. 本机 `go test`：fork tcp/stack 包、p2p、dialer。

**部署**：`-a` 强制重编，先 GG，再 JF，再 QG。VM 需更新到含 F5 的版本（VM 当前跑的是 v0.8.0-325）。

**验收**：
1. GG 复测 4 流 × 2 轮拉 8MB：
   - 不再出现 `link dead`；
   - GG/JF Link NIC 不重建；
   - 4 流全部下完。
2. 人为触发一次重连（重启 JF）：
   - GG 出现 `[TCP-REROUTE]`；
   - 不再有指向旧 nicID 的 `[TCP-SEND] failed` 刷屏；
   - 已有 ssh 会话恢复。
3. VM：不再出现每 30s 的 `keepalive timed out`，GG 侧 vm 会话持续 > 10 分钟。
4. GG 运行 24h 无 link_nic.go panic。
5. fork 单测：SackedOut 全程非负；`TestRerouteAfterNICRemoval` 稳定通过。

**回滚**：fork 与 phaethon 各自单独提交，按提交回退、重编即可。
