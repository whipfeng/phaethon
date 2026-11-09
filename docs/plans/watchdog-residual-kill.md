# watchdog 职责边界与单实例清理

**创建时间**: 2026-11-07
**状态**: 已落地（2026-11-09）——原"扫描 /proc 精确查杀残留"方案被**单实例锁**替代，见 §二

> 本文原名为"watchdog 残留进程精确查杀"，方案是收紧 `/proc` 扫描条件（W1）+ `Pdeathsig`（W2）。
> 2026-11-09 复核后改为：watchdog 不杀任何非自己子进程的进程，单实例由 worker 持有的文件锁表达，
> 残留清理权归锁持有者。W2 保留并修正了线程语义。

## 一、职责边界（冻结，后续尽量不改）

watchdog 是 boot 角色，只做这 5 件事：

1. 选择 worker 二进制（pkg 比 self 新才用 pkg，崩溃未 ready 回退 last-good）
2. spawn 子进程（`PHAETHON_WORKER=1`，stdout 管道）
3. 读子进程 stdout 的 `{"ready":true}` / `{"heartbeat":true}`
4. 退出/ready 超时/心跳超时 → 重启（冷却 + 退避 + 上限）
5. 转发 SIGINT/SIGTERM

**不做**：

- ❌ 扫描 `/proc`、按 cmdline/exe/uid/cwd 猜"哪些进程是我的"
- ❌ 杀任何不是自己 fork 的进程
- ❌ 清理 TUN 设备、路由、iptables、DNS（这些属于 worker 启动自检）
- ❌ 理解业务语义；它只认"进程活着/死了/卡住"

平台差异只允许存在于进程原语：`processExists` / `reapChild` / `gracefulExitSignal`。

## 二、单实例：锁，不扫进程

`killResidualWorkers` 已整体删除（含 Linux 的 `/proc` 扫描与 Windows 空实现）。

替代方案：**worker 在 `db.Init` 之前获取 `<data>/instance.lock`**（`instance_lock*.go`）。

| 平台 | 原语 |
|---|---|
| Linux / Darwin | `flock(LOCK_EX|LOCK_NB)` |
| Windows | `LockFileEx(EXCLUSIVE|FAIL_IMMEDIATELY)` |
| 其他 | 空实现（无跨进程保护，watchdog 仍工作） |

- 拿不到锁：记录持有者信息（锁文件内的 `pid=... since=...`），以退出码 `3` 退出。
- watchdog 见到退出码 `3`：**不再重启，自身退出**，避免两个实例互踩端口/DB/TUN。
- 拿到锁 = "本 data 目录没有别的活实例" → 此时启动清理**必然**是在清理自己的前任残留，不需要任何进程匹配。

这把"谁是残留"从"猜进程身份"变成"谁持有资源"，两个问题（进程清扫、残留清理）用同一条规则解决。

### `--takeover`（Linux / macOS）

裸脚本部署需要"一条命令替换旧实例"时使用。流程：

1. 读锁文件里的 `pid/exe`；
2. 校验该 pid 确属本部署（Linux 读 `/proc/<pid>/exe`；无 `/proc` 的平台退化为锁文件记录 + 同目录同名）；
3. **先 SIGTERM watchdog，再 SIGTERM worker**：先杀 watchdog 可以避免它重启新 worker 来抢锁；
4. 最多等 15s 拿锁；超时或校验失败则报错退出，**不做 SIGKILL**。

默认不带 `--takeover` 时启动路径永不杀进程。Windows 明确不支持，需先停服务再启动。

## 三、停机判定：内存状态，不用 marker 文件

原 `data/state/stopped` 磁盘 marker 已删除。原因：重启场景（watchdog SIGTERM 子进程）会让 worker 写 marker，
残留文件会让后续一次真实崩溃被误判为"优雅退出"，watchdog 直接退出、服务不再重启。

现在只有一处判断依据：watchdog 内存里的 `stopping atomic.Bool`，仅在收到停机信号时置位。
子进程退出时：`stopping == true` → 清理并退出；否则一律按崩溃重启。

### worker 与 watchdog 的契约（冻结）

| 方向 | 契约 |
|---|---|
| worker → watchdog stdout | `{"ready":true}`、`{"heartbeat":true}`（其它行原样转发） |
| worker 退出码 `0` | 正常退出；watchdog 仅在自身 `stopping` 时退出，否则重启 |
| worker 退出码 `3` | 单实例冲突；watchdog 不重启并退出 |
| 其它退出码 | 崩溃，重启 |

新增能力只能**加字段/加退出码**，不得改变上述语义。

## 四、父死信号（W2，已修正）

- worker 侧 `PR_SET_PDEATHSIG=SIGTERM`（`worker_attr_linux.go`）
- `PR_SET_PDEATHSIG` 绑定的是 fork 该进程的**父线程**，因此 watchdog `runWatchdogMode` 开头
  `runtime.LockOSThread()`，保证 fork 线程常驻
- worker 设置 prctl 前后各检查一次 `os.Getppid() == 1`：父进程在窗口期已死则立即退出，
  避免"父进程已死却永远收不到信号"的孤儿
- Windows 无等价原语；重启用 `os.Kill`（`child_signal_*.go`），因为 `os.Process.Signal` 不支持 SIGTERM

## 五、TUN / 路由冲突：属于 worker，不属于 watchdog

前提：锁保证同一 data 目录单实例。剩下的冲突只来自**崩溃残留**与**同机其他 VPN/实例**。

现状（`tun/cleanup_*.go`、`tun/route_*.go`）：

| 平台 | 启动清理依据 | 主要缺口 |
|---|---|---|
| Linux | `tun*` 接口持有 `tunIP` 时删 0/1 路由与地址；备份/state 文件恢复 DNS、sysctl、iptables | iptables 无 comment/专用 chain；路由无 proto 标记；`cleanup_linux.go` 硬编码 `tun0`；非 bypass 模式会把 `rp_filter`/`ip_forward` 误写 0；排除路由不清理 |
| Windows | 删除 `phaethontun` 适配器 + 残留 0/1 路由 | `removeAdapterAPI` 只按 WINTUN 实例 ID 取**第一个**；`deleteResidualRoutesAPI` 全表删 `0/0 + next-hop 0`，不校验 LUID；`StopTUN` 不删适配器，关闭 TUN 后永久残留 |
| Darwin | `utun*` 持有 `tunIP` 时删地址 | DNS 只在内存保存，SIGKILL 后污染会被下次启动当成"原始值"固化；`preemptForeignSplits` 快照仅在内存，SIGKILL 后别人的 0/1 路由永久丢失 |

加固原则（后续任务，按优先级）：

1. **给自建对象打标记，清理只认标记**
   - Linux：0/1、128/1 与排除路由设 `Protocol=200`（或固定 table）；iptables 用专用 chain `PHAETHON` + `-m comment`
   - Windows：只用**本适配器 LUID** 匹配路由；`removeAdapterAPI` 必须按名称 + LUID 精确匹配
   - Darwin：路由带 `RTF_STATIC` + 本 ifindex；DNS 原始值持久化到 `data/state/`
2. **清理权 = 锁持有者**：`CleanupResidual` 只在拿到实例锁之后执行（已在 `db.Init` 之后、`startEngine` 之前）
3. **TUN 禁用/不可用也要清理一次**：当前 `main_engine.go` 仅在 `enabled && Available()` 时清理，
   关掉 TUN 后残留（尤其 Windows 适配器）无人处理
4. **部分失败要回滚**：`StartTUN` 中途失败只 `dev.Close()`，`StopTUN` 因 `tunRunning=false` 直接返回；
   Windows 的 TUN IP、weak-host、首条 /1 路由会留到下次启动
5. **不要抢占其他 VPN**：Linux/Windows 遇到 EEXIST 直接失败是可接受的；Darwin 的
   `preemptForeignSplits` 会删别人的 0/1 路由，应改为"检测到外来 split 路由 → 报错放弃 TUN"，
   或至少把快照持久化以便崩溃后恢复

## 六、历史结论（勿重复开发）

- 原 W1"收紧 `/proc` 判定"**未采用**，随 `killResidualWorkers` 一并删除
- 原 W2 `Pdeathsig` **已采用**（worker 侧 prctl + watchdog 线程固定 + getppid 检查）
- 文档早期写的 worker 路径 `data/packages/` 是错的，实际 pkg worker 在 `<cwd>/data/worker/`
  （`main.go` `findLatestPkgAndExtract`）

## 七、验收

1. 同一 data 目录启动第二个实例：第二个 worker 退出码 3，其 watchdog 退出，第一个实例不受影响
2. 开 `tail -f phaethon.log` 再部署：tail 不被杀（watchdog 不再扫描进程）
3. `kill -9 <watchdog pid>`：worker 收到 SIGTERM 并走正常清理（Linux）
4. 正常 SIGTERM 停止：watchdog 退出，不重启
5. 崩溃（非 0/3 退出码）：watchdog 重启；反复崩溃遵守冷却与上限
6. 删掉 `data/state/stopped` 相关代码后，任意一次崩溃都不会被误判为优雅退出
