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
