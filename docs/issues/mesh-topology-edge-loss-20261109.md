# Mesh 拓扑丢边（gg--vm 在 deskw 视角缺失）

> 发现日期: 2026-11-09
> 状态: 现象已消失（jf 重启后自愈），根因未定论，已加埋点待复现
> 相关: [full-code-audit-20261109.md](full-code-audit-20261109.md)

## 1. 现象

在 deskw 节点控制台看拓扑图，`vm — gg` 这条边缺失；其它节点（vm / jf / qg / qgw / gg / mb）都正常显示。

- 缺失持续 **≥ 2 小时**（14:58 与 16:58 两次抓取都缺），不是瞬时抖动。
- deskw 只有 jf 一个直连 peer（`/api/mesh` 中 `peers=1`），它看到的"学到的边"全部来自 jf 的 gossip。
- jf 自己的 `/api/mesh/topology` 里该边**双向都在**，seq 互为反向。

## 2. 观测数据

同一时刻各节点的边集合（`/api/mesh/topology`）：

| 节点 | gg--vm | 边上的 seq 对 |
|---|---|---|
| vm | 有 | `gg(11/2)`、`vm(2/11)` |
| jf | 有 | `gg(11/2)`、`vm(2/11)` |
| qg | 有 | 同上 |
| qgw | 有 | 同上 |
| gg | 有 | 同上 |
| mb | 有 | 同上 |
| **deskw** | **无** | — |

（vm 重启前 seq 为 `gg(10/1)`、`vm(1/10)`，当时同样只有 deskw 缺失。）

## 3. 时间线

| 时间 | 事件 | deskw 的 gg--vm |
|---|---|---|
| 14:58 | 首次抓取 | 缺 |
| 16:53 | vm 升级并重启（seq 变为 2/11） | — |
| 16:58 | 再次抓取（**jf 尚未改动**） | 仍缺 |
| 17:07:14 | jf 短暂运行新版（带埋点） | — |
| 17:07:26 | 新版在 jf 上被 SIGTERM（见 §6） | — |
| 17:13:47 | jf 回滚到旧版 `v0.8.0-343-ga01befa` 并重启 | — |
| 17:16 | 抓取 | **有（自愈）** |

结论：**自愈发生在 jf 重启时，且 jf 跑的是旧版** ⇒ 与版本无关，是重启重建了 jf 的某个内存态。

## 4. 已排除 / 已推翻

- **不是 deskw 渲染问题**：`GetFullTopology()` 直接使用各 peer 收到的原始 `ClaimedSubnets` 拼边（有意保留最原始状态便于观察），deskw 缺边说明它**收到/保存**的 claim 里就没有。
- **不是链路或 seq 错乱**：所有节点的 seq 对都双向对称。
- **不是"jf 出向被互验裁掉"**（我最初的判断，已撤回）：jf 原始视图双边齐全，反向 seq 能对上，互验条件应当通过。
- **不是版本问题**：自愈时 jf 已回滚到旧版。

## 5. 当前假设（待验证）

拓扑图是**把所有 peer 通告的 claim 合并**后画的；而 gossip 转发用的是 `bestClaims`——**按 subnet 只保留跳数最小的那一份**。

于是可能出现口径分裂：

- 展示：`('gg', 11/2)` 这条边可能来自 qg 转发的副本，`('vm', 2/11)` 来自另一条路径的副本 → jf 视角"完整"；
- 转发：对 gg 的 subnet 只取 **gg 直连 hop=1 那一份**（`mesh/mesh.go` `bestClaims` 选择处）→ 若那一份的邻居列表里当时没有 vm → 发给 deskw 的 gossip 就缺这条边；
- jf 重启 → 所有 claim 重新拉取 → gg 那份也刷新 → 恢复。

这条路径能同时解释「jf 显示完整」和「deskw 缺边」，也不违反互验本身的有效性（互验防外部成环的作用保留）。

## 6. 期间暴露的另一个真实 bug：Linux worker 被误杀

为抓 gossip 日志把带埋点的新版部署到 jf，启动 12 秒后 worker 收到 SIGTERM 退出：

```
17:07:14 P2PManager initialized (version=v0.8.0-404-gdd01268-dirty, platform=linux/amd64)
17:07:26 Received signal terminated, shutting down...
```

原因：`worker_attr_linux.go` 使用 `PR_SET_PDEATHSIG`。该信号绑定的是 **fork 出子进程的那个线程**，而 Go 会回收/复用线程——watchdog 还活着，worker 却被内核杀掉。

**修复**：弃用 `PR_SET_PDEATHSIG`，改用已有的 worker→watchdog 心跳管道做父进程存活检测——watchdog 死则管道读端关闭，worker 下一次心跳写入失败即退出（`main_engine.go` `emitProtocolMsg` 返回写失败）。

- 事件驱动、无轮询、无线程语义依赖，跨平台一致（Unix EPIPE / Windows broken pipe）
- 检测延迟 ≤ 心跳间隔 10s，与 watchdog 侧 30s 判活对称

## 7. 已加的埋点（`mesh=debug` 时输出）

| 日志 | 位置 | 内容 |
|---|---|---|
| `[GOSSIP-IN] from=<peer> claims=...` | `mesh/topology.go` `UpdateGossip` | 收到的原始 claim：`owner@subnet(hN){neighbor(localSeq/remoteSeq) ...}` |
| `[GOSSIP-OUT] to=<peer> claims=...` | `mesh/mesh.go` `broadcastGossip` | 实际转发出去的 claim（经互验与 split horizon 之后） |
| `[GOSSIP-SEL] take/skip subnet=...` | `mesh/mesh.go` bestClaims 选择处 | 同一 subnet 多份副本的选择与丢弃，含 hop 与邻居数 |

开启方式：`PUT /api/log/modules {"modules":{"mesh":"debug"}}`，或环境变量 `PHAETHON_LOG_MODULES=mesh=debug`。

## 8. 下次复现时怎么取证

1. 对 **所有节点** 立刻抓 `/api/mesh/topology`（原始视图）与 `/api/mesh`，记录边集合 + seq 对，确认缺失范围（是单节点还是整条路径）。
2. 在 **中转节点**（本例 jf）开 `mesh=debug`，抓 1–2 个 gossip 周期的三类日志，比对：
   - `[GOSSIP-IN]` 里 owner 自己的那份有没有该邻居；
   - `[GOSSIP-SEL]` 选中了哪一份（hop / 邻居数）；
   - `[GOSSIP-OUT] to=deskw` 里最终有没有。
3. 若 `[GOSSIP-IN]` 有、`[GOSSIP-OUT]` 无 → 转发路径丢（§5 假设成立）；
   若 `[GOSSIP-IN]` 无 → 上游未发，需再往上追一跳。

## 9. 当前状态

- 6 个节点拓扑一致，`gg--vm` 正常；deskw 已补上 VM 的 4 把公钥（含本机），mesh SSH 可用。
- jf 运行 `v0.8.0-343-ga01befa`（回滚后状态），备份 `phaethon.bak-20261109-170713`。
- 带埋点 + pdeathsig 修复的新版源码已就绪（`v0.8.0-404-gdd01268-dirty`），Linux 产物已构建，待隔离冒烟后升级到 jf 以便下次复发留证。
