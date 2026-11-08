# 未完成事项核实（2026-11-08）

**状态**: 已核实（只读取证，未改代码）
**用途**: 对 docs/ 下"待实施/设计中/实施中"状态的方案做逐项代码核实，避免重复劳动或基于虚假前提判断。

---

## 一、文档状态失真：标"未完成"但代码已落地

| 文档 | 文档声称 | 代码实际 |
|---|---|---|
| `plans/goroutine-leak-l1-l2-disposition.md` | 待实施 | **L1 已修**：`admin/admin.go:891-903` `closeCascadeConn`（Close 级联 `ln.Close()`，`sync.Once` 幂等）；两项测试在 `admin/admin_test.go:12,22` |
| `issues/p2p-v8-mesh-regression.md` | 诊断中 | **10/10 已修**（`establishPeerFromGossip`、`reregisterPeer`、`pingLossWindowSize=20`、版本 10 强制校验、保存即重启等） |
| `plans/timeout-audit-20261103.md` B1 | 待裁决 | **已按删除处置**：`frame/transport.go:123-127`、`:173` 明确无写 deadline，仅保留 60s 读 deadline |
| 网络质量批次 2/3/5 | — | 12 项中 **10 项已落地**（RED 已删、relayUDP 双 buffer、trojan/vless 15s 握手、MTU=1600、双 Flatten 已除、日志守卫、u-turn 改 `[4]byte`、DNS 四项、`uturnCount` 原子） |

## 二、确认未完成

### P0 · gvisor-fork（3 项）

| 项 | 状态 | 证据 |
|---|---|---|
| F3 TCP 端点 NIC 删除后重选路 | 未实现 | `../gvisor-fork/.../tcp/connect.go:848-860` `sendTCP` 只打日志后返回错误，无 reroute；`stack/route.go:512` `isValidForOutgoing` 未导出 |
| F4 `SackedOut` 记账不变量 | 未实现 | `.../tcp/snd.go:1167` 仍 `s.SackedOut = 0`；`:1796` 用 scoreboard 再减；无防负钳制 |
| F6 SetPipe 排除已丢失段 | 未实现 | `.../tcp/snd.go:1257-1305` 只认 scoreboard，无 `seg.lost` / `writeNext` 判断 |

**取证结论：属"代码真没做"，不是文档过期。** 三条独立证据：

1. **符号级**：`reroute` / `IsValidForOutgoing` / `[TCP-REROUTE]` 在 fork 与 phaethon 代码零命中，只存在于 .md
2. **行号级**：文档引用的 8 处 file:line **全部精确落在未打补丁的原始代码上**（若真改过行号必然漂移）
3. **仓库级**：fork 单分支 `main`、工作区干净、无 stash、无相关提交

**"单测 8/8 通过"的说法不成立**：`gvisorfork/` 下确有 8 个测试点，但其中 3 个是无关的 Eifel undo 测试；`TestRerouteAfterNICRemoval` 不引用任何 F3 新符号，不打补丁也能编译，只会 30s 超时失败；`TestSackedOutNeverNegative` 在当前代码上应失败。

**F4 补充**：`5ba8665`（PR #14101 移植）修的是另一个变量 `sackedOutsideRecovery`（Outstanding 泄漏），**不是** `SackedOut` 不变量，两者不是一回事。

### P1（3 项）

| 项 | 缺口 | 证据 |
|---|---|---|
| TUN 出口批量写 | drain 已做，但随后仍逐包 `Write`；`tun.Device` 无批量 API | `mesh/netstack.go:1920-1933`（drain）vs `:1995-1996`（逐包 Write）；`tun/device.go:10-17` 只有单包 `Write` |
| 健康检查 dial 可取消 | **部分完成**：握手 15s deadline ✅、sem 获取超时 ✅；但 dial 无 context/errgroup，`wg.Wait()` 仍可能永久阻塞 | `dialer/trojan.go:144`、`dialer/vless.go:72,103`、`main.go:2413-2420`、`:2494-2502` 已做；`main.go:2528-2596` 未做 |
| watchdog 残留查杀 W1/W2 | cmdline 子串匹配仍在（会误杀 `tail -f phaethon.log`）；无 `Pdeathsig` | `main_tun_nows.go:121-123`；`main.go:971-981` 的 `os.ProcAttr` 只有 Env/Files；全仓无 `worker_attr_*.go` |

### P2（1 项，另 2 项已完成）

| 项 | 状态 |
|---|---|
| 端口范围设置页 | **未实现**：db 只有 `Get*PortRange`（`db/configstore.go:123-137`），无 `Put*`；无专用 API；前端零命中；仅启动期读取（`main.go:815`、`server/control_server.go:114`），无热生效 |
| SSH shell 单测 | ✅ 已完成 `43298d2` |
| 内建控制台端口约束 | ✅ 已完成 `2b6bc0e` |

### P3（1 项）

- **P2P Manager 队列化重构**：完全未开工。`p2p/p2p.go:92-101` 仍 `sync.Mutex` + 直接操作 map，全包无 `cmdCh`/`peerCommand`/快照。

---

## 三、内建服务端口分派（已实现，方案 B）

`handler/connection_handler.go` 的 `localNodeDomain` 分支按端口显式分派：

| 端口 | 服务 |
|---|---|
| 22 | 内建 SSH |
| 443 | 内建控制台（规范入口） |
| 39999 | 内建控制台（保留别名，兼容既有脚本/文档） |
| 其他 | 拒绝 + WARN，为未来内建服务预留 |

handler 未注入时仍回退本地拨号。第二处 `localNodeDomain` 分支（rule matching 之后）经确认是不可达死代码，未改动。

## 四、维护提示

- 本文档的行号基于 2026-11-08 的 `master`，后续提交会导致漂移，修改前请重新定位
- 判断某项是否完成，以代码为准，不以 plan 的状态字段为准

---

# 五、2026-11-08 决策结论（逐项定案）

> 本节的目的是**避免重复决策**。每项都记录了最终选择、以及精读代码后发现的关键约束。
> 若后续有人想推翻，请先读完"约束"部分再讨论。

## 5.1 F3（TCP 端点 NIC 删除后重选路）— 修

**决策**：修，允许改 `../gvisor-fork`。

**精读发现（必须按此实现，不能照文档）**：

1. **不能在 `(*Endpoint).sendTCP` 失败后重发同一个包**：`buildTCPHdr` 已经 `Push` 过 TCP 头（`connect.go:864`），重发会叠加第二个头；checksum 基于旧 route 的 pseudo-header 也算错。
2. **正确落点**：在 `sendRaw`（`connect.go:1037`）与 `sendSynTCP`（`connect.go:835`）**最前面**做"route 有效性校验 + 自愈"，再做 `MaxHeaderLength`/TTL/GSO 等 route 派生计算。因为 NIC 删除后 `nic.remove` 已 `ep.Close()` 网络端点，在失效 route 上调这些函数本身就有未定义行为。
3. **`e.route` 不能置 nil**：`sendRaw` 在调用 sendTCP 前已解引用 `e.route`（`connect.go:1044-1046`）。
4. **旧 route 必须 `Release()` 一次**；新 route 直接接管 `FindRoute`/`BuildRouteFromDecision` 返回的引用，不要重复 `Acquire`。
5. **需要 stack 导出** `Route.IsValidForOutgoing()`（当前 `isValidForOutgoing` 未导出，`route.go:512`）。
6. **必须加退避**：`RouteFunc → V2RouteLookup` 会取 p2p `mgr.mu` 并打日志，每包重试会造成锁竞争与日志风暴。建议在 endpoint 加 `nextRouteHealAttempt` 字段，失败后 `now + min(RTO, 1s)`。
7. **accepted socket 的 `boundNICID` 是入向 NIC**（`accept.go:202`），该 NIC 删除后用严格 nicID 重试永远失败 —— 需要决定是否退化为 `nicID=0` 让 RouteSelector 重选。
8. **不能更新 `MaxPayloadSize`/调用 `sendData`**（`snd.go:448-449` 会递归进入发送路径）。

**推荐策略**：自愈后**丢弃当前包**，交给 RTO/SYN 重传，而不是重发当前包。

**验收**：`TestRerouteAfterNICRemoval` 通过；无 `ErrInvalidEndpointState` 刷屏；VM 手工验证链路重建后流可恢复。

## 5.2 F4（`SackedOut` 记账）— 修

**决策**：修，允许改 fork。

**精读发现（关键：不能只删一行）**：

1. 增/减存在 **6 处不对称**：闸门不对称（`+` 只在 RACK 开启时的 `walkSACK`，`-` 在累计 ACK 路径无条件）、判据不对称（`+` 用 `seg.acked` latch，`-` 用实时 `scoreboard.IsSACKED`）、覆盖粒度不对称、recovery 清零破坏对称、RTO 不清 `seg.acked`、split/merge/trim 不调整。
2. **`seg.acked` 目前是"只置不清"的一次性 latch**（全仓无清除点），不能直接当统一判据。
3. **RTO 只清 scoreboard，不清 `SackedOut` 也不清 `seg.acked`**（`snd.go:655-672`）。
4. **restore 会恢复 `SackedOut` 但重置 scoreboard**（`endpoint_state.go:204-206`），状态天然不一致。
5. **`enterRecovery` 的注释是错的**：`5ba8665` 写的 "SetPipe owns all SACK accounting"，但 `SetPipe` 只改 `Outstanding`，完全不碰 `SackedOut`。

**正确做法（原子改动集，约 30-50 行）**：

- 删除 `snd.go:1167` 的 `SackedOut = 0`
- RTO（`snd.go:671` 后）与 restore（`endpoint_state.go:204-206` 后）统一 `SackedOut = 0` + 清 `writeList` 的 `acked`
- MSS 调整（`snd.go:439-443`）与累计 ACK（`snd.go:1793`）判据统一为 `seg.acked`
- 建议抽 `resetSACKStateLocked()` 供 RTO/restore 共用，避免再次打补丁造成不对称

**风险**：删掉清零后，退出 recovery 时 `SackedOut` 可能仍 `>=3`，`rack.go:337` 会更激进地把 `ReoWnd` 压到 0 → 可能增加伪丢包。需在乱序链路压测。

**验收**：`TestSackedOutNeverNegative` 四子用例通过；`gvisorfork/` 8 个测试无回归；VM 大文件吞吐不退步。

## 5.3 F6（`SetPipe` 排除已丢失段）— 修

**决策**：修，允许改 fork。

**精读发现（关键：只改 SetPipe 基本无效）**：

1. **RTO 路径完全不调用 SetPipe**。SetPipe 调用点：`snd.go:545`、`:1347`、`:1676`、`:1844`、`sack_recovery.go:36`。
2. `rc.detectLoss` 在两次 SetPipe **之后**才置 `seg.lost`（`snd.go:1863-1874`），所以"发现丢包的那个 ACK"上 SetPipe 看不到新 lost。
3. **必须补调用时机**：在 `rc.detectLoss` 返回 `numLost > 0` 后、`DoRecovery` 前调用 `s.SetPipe()`；`rack.go:404-416` 同理。
4. **不要用 `writeNext` 替代 `SndNxt`**：`writeNext` 会回退（RTO 时指向 `SndUna`），还可能为 nil；`SndNxt` 才是 RFC6675 的 HighData 语义。
5. 建议只改 (a) 规则：`if !s1.lost && !IsRangeLost(sb) { pipe++ }`，保留 (b) 的 HighRxt 语义，避免与 RFC6675 冲突。
6. **GSO 粒度不一致**：`seg.lost` 是段级，SetPipe 按 SMSS 子区间计数，部分丢失时会低估 pipe → cwnd 过宽。建议 `walkSACK` 命中 SACK 时同步 `seg.lost=false`。
7. 与 F4 强耦合：F4 改完后 RACK 更易置 lost，F6 的 pipe 低估效应会被放大。

**验收**：新增 `TestSetPipeExcludesLost`；8 个测试无回归；VM 大文件 RTO 后吞吐能恢复（不再卡 ~7KB/s）。

## 5.4 TUN 出口批量写 — 做，但仅"接口批量、实现循环"（A3）

**决策**：A3。**注意：原方案（Linux/Darwin 真批量）技术上不成立。**

**精读发现**：

- **Linux TUN 一次 `write()` 只能写一个 IP 包**。`tun_chr_write_iter` 会把整个 iov 拼成一个 skb，用 `writev` 传 N 包 = 内核收到一个超 MTU 的畸形包被丢弃。
- Darwin utun 同理（一次 write 视为一帧）。
- 本仓用 `IFF_TUN|IFF_NO_PI`，无 `IFF_VNET_HDR`/`TUNSETOFFLOAD`，**没有批量路径**。
- 真批量需要多队列 TUN（`IFF_MULTI_QUEUE`）或 vnet_hdr+GSO，属另一个工程。
- Windows wintun 的 `AllocateSendPacket`/`SendPacket` 必须成对，也只能循环。

**实际做法**：

- `mesh/netstack.go` 的 `WriteLoopDevice` 增加可选批量接口 + 类型断言
- `tun.Manager` 实现批量方法，内部循环调用 `dev.Write`
- 可顺带优化 Darwin 的每包 `append(hdr, payload...)` 分配（用 `writev` 消掉这次拷贝）
- 批量上限（建议 32/64 包），避免 drain 无界放大 egress 延迟
- 写失败时按批聚合日志（现在是每包一条 Warn，批量会放大）

**收益**：代码形态统一 + Darwin 省一次拷贝；**Windows 无性能收益**。

## 5.5 健康检查 dial 可取消 — 做，层次二 + 类型多态（B2）

**决策**：B2 层次二（真打断）+ `DialTimeoutAware` 按类型多态。

**精读发现**：

1. 健康检查有两条路径：`testURL == ""` → `checkProxyTCPHealth`（只 TCP 可达）；否则 → `checkProxyHealth`（`ChainDial` 完整握手）。
2. **5s 超时判死是既有行为**（`main.go:2548-2549`、`:2593-2594` 直接 `return false`），**不是 ctx 引入的新问题**。
3. 真正会**无限**挂死的只有两处：SOCKS5 握手（`socks5.go:147-195` 无 deadline）、SSH channel open（`ssh.go:74` 无超时）。
4. 底层 `net.Dialer{Timeout: 30s}`（`bind.go:153-160`）保证 TCP connect 有界。

**实现**：

- `bind.go` 增加 `DialRouteAwareContext`（保留 `DialRouteAware` 作为 `context.Background()` 包装）
- 统一 helper `withCtxDeadline(ctx, conn)`：按 `ctx.Deadline()` 设 deadline + `context.AfterFunc` 取消时打断阻塞读；返回前清除
- 12 个 `Dial` 实现各加 2 行调用 helper（这是"真打断"的必要成本，无法靠一个包装函数替代）
- 新增可选接口 `DialTimeoutAware { MaxDialTimeout() time.Duration }`
- 健康检查 ctx 超时按类型取：默认 5s，**htunnel 声明 35s**（3 次串行 10s HEAD + 余量）

**注意**：

- `SetDeadline` 不能设在共享缓存连接上（SSH client 池、hysteria2 pool、htunnel shared client），只能对 per-call conn 设
- 现有后台 goroutine 模式（`main.go:2534-2543`）要一并去掉，否则仍泄漏

**验收**：`go test ./...` 无回归；健康检查不再出现 goroutine 无限滞留；htunnel 类代理在配了 HealthCheckURL 时不再被 5s 误判。

## 5.6 watchdog 残留查杀 — 做 W1 + W2（仅 Linux）

**决策**：W1 精确判定 + W2 `Pdeathsig`。

**精读发现**：

1. **文档的路径写错了**：pkg worker 二进制在 `<cwd>/data/worker/phaethon-<version>`（`main.go:1093-1094`），**不是** `data/packages/`（那里只有 `.pkg`）。按文档实现会漏杀。
2. 二进制名可能是 `phaethon-0.7.3-61-ga31b3b5`（不带 `v`），不要写死 `phaethon-v*`。
3. **`Pdeathsig` 是线程级**：fork 子进程的 OS 线程退出时才会发信号。必须 `runtime.LockOSThread()` 且该 goroutine **不能提前返回**（返回会销毁线程 → 立刻误杀 worker）。
4. 三条件取法：uid 读 `/proc/<pid>/status` 的 `Uid:`；cwd 读 `Readlink(/proc/<pid>/cwd)`；exe 读 `Readlink(/proc/<pid>/exe)`（去掉 `" (deleted)"`）。**权限失败时放行不杀**（保守）。
5. **杀序要改**：先杀 watchdog（让 Pdeathsig 带走 worker），1s 后再兜底扫 worker。否则「杀 worker → 旧 watchdog 重启新 worker」会产生新残留。
6. **cwd 过严会漏杀**：旧实例若以不同 cwd 启动（`cd /`、systemd 变更），同 exe 同 uid 也清不掉。建议 cwd 不匹配时降级为「exe 完全相同才杀」并打 Warn。

**验收**：`tail -f phaethon.log` 不再被误杀；watchdog 被 kill -9 后 worker 自动退出；正常关闭流程无副作用。

## 5.7 端口范围设置页 — 做

**决策**：UDP + TCP 都热生效 + Admin 界面。

**要点**：

- db 增加 `PutUDPPortRange` / `PutTCPPortRange`（bucket 名实际是 `port:config`，不是文档写的 `port-range-config`）
- admin 增加 GET/PUT 端点
- 前端在 Admin 设置页加「网络端口范围」区块，格式 `min-max`，留空用默认
- 校验：1–65535、`min ≤ max`、格式非法拒绝
- UDP/TCP 允许区间重叠
- **TCP 热生效会重建反连通道（短暂断开）**，界面需提示

## 5.8 P2P Manager 队列化 — 已否决（不实施）

见 `docs/plans/p2p-manager-queue-refactor.md`（状态已改为"已否决"）。

## 5.9 实施顺序建议

1. **F4 → F6**（F6 依赖 F4 的 RTO/acked 清理语义），**F3 可并行**
2. fork 三项合并为一次 `-a` 重建 + 一次四环境部署
3. **5.5（健康检查）排在 fork 之后**，避免两组大改动同时上难以定位
4. 5.4（TUN）、5.6（watchdog）、5.7（端口范围）与 fork 无交集，可并行开发

## 5.10 联合回归要求

F3 的反复重传会放大 F4/F6 在 RTO 路径上的问题，因此必须跑联合场景：

- 乱序（reorder）
- LinkNIC 删除重连
- RTO

三者叠加的测试是必要的。
