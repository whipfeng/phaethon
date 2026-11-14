# Watchdog 新旧进程交接（2026-11-14）

## 背景

自升级只替换磁盘上的 `phaethon.exe` 并重启 worker，watchdog 进程本身一直运行启动时的旧映像。结果是：

- watchdog 的代码修复不随升级生效；
- 旧映像在升级后变成 `.old`/`.old2`，仍被 watchdog 映射，下一次轮换可能因文件被占用失败。

## 目标

升级完成后让**新 watchdog 进程**接管监督，旧 watchdog 退出，从而：

1. watchdog 代码与磁盘版本一致；
2. 旧映像文件在旧进程退出后解锁，后续轮换不再依赖 `.old.last`；
3. 新 watchdog 启动失败时，旧 watchdog 仍能回滚并继续服务。

## 流程

```
旧 watchdog（版本 A）
  │ 1. 停旧 worker（释放实例锁）
  │ 2. 替换 phaethon.exe 为版本 B
  │ 3. 写 handoff request（token/目标版本）
  │ 4. 启动版本 B 的 watchdog（不带 PHAETHON_WORKER）
  ▼
新 watchdog（版本 B）
  │ 5. 写 result=accepted
  │ 6. 启动新 worker，等待 ready 且 worker 版本 == B
  │ 7. 写 result=ready
  ▼
旧 watchdog
  │ 8. 读到 ready → 写 upgrade succeeded → 退出（释放旧映像）
  ▼
新 watchdog 继续监督
```

失败路径：

```
旧 watchdog 等待 ready 超时 / 新 watchdog 报 failed / 新进程提前退出
  → 写 request=abort
  → 新 watchdog 停 worker 并退出
  → 超时未退出则杀进程树（Windows taskkill /T，Unix pkill + kill）
  → 旧 watchdog 用备份回滚、重启 worker，继续监督
```

## 文件与协议

- `data/state/watchdog-handoff-request.json`：旧 watchdog 写，状态 `starting`/`abort`
- `data/state/watchdog-handoff-result.json`：新 watchdog 写，状态 `accepted`/`ready`/`failed`
- `data/state/watchdog-version`：watchdog 启动时写入当前版本，便于确认实际运行的 watchdog 版本

两个方向用不同文件，避免旧进程的 abort 与新进程的 ready 互相覆盖。

## 关键约束

- 实例锁由 worker 持有，watchdog 不持有；旧 worker 停止后锁自然释放，新 worker 获取。
- 新 watchdog 的环境必须去掉 `PHAETHON_WORKER`，否则会以 worker 身份启动。
- 新 watchdog 只在自己启动的 worker 版本等于自身版本时才报 ready，避免 `.old` 回退导致“版本看起来成功、实际跑旧版”。
- 目标版本低于首个交接版本（`v0.8.0-699`）时走旧的直接启动 worker 路径，保证可以回滚到旧版本。
- 旧 watchdog 退出前必须确认新 watchdog ready；退出后不再触碰新进程。
- 启动器/服务管理器应把“旧 watchdog 退出码 0”视为主动交接，不要重复拉起；即使重复拉起，worker 的实例锁也会让重复实例退出。

## 上线注意

一次性激活：首个包含交接代码的版本（699）仍由旧 watchdog 安装，watchdog 进程不会自己更新。需要：

1. 正常发布 699，确认 worker 为 699；
2. 对每个节点做一次 watchdog 进程重启（服务重启/批处理 stop+start）；
3. 之后再发布新版本，watchdog 才会执行交接。

Linux 可直接 `exec` 自身；当前实现为统一使用 spawn + ready + 旧进程退出，便于跨平台复用同一状态机。

## 验证点

- 升级日志包含 `handoff: new watchdog ready`、`handoff: supervision transferred`；
- 旧 watchdog PID 消失，新 watchdog PID 存活；
- `data/state/watchdog-version` 等于新版本；
- 新 watchdog 启动失败时可回滚，worker 版本回到旧版本且实例锁被正确释放。

## 实机验证（2026-11-14）

1. 发布 699，三个节点 worker 均为 699；
2. 一次性重启 watchdog：
   - QG：`rc-service phaethon restart`（分离式执行）；
   - VM/qgw：分离式批处理 `taskkill` + `start`；
   - 重启后 `data/state/watchdog-version` 均为 `v0.8.0-699-g339fc08`。
3. 发布测试版本 `v0.8.0-700-gfa11bac`：

| 节点 | 结果 | 旧 watchdog | 新 watchdog | 证据 |
| --- | --- | --- | --- | --- |
| QG | succeeded | 24567 消失 | 24677 存活 | `handoff: started/ready/transferred` |
| VM | succeeded | 旧 PID 消失 | 21616 存活 | 同上，`watchdog-version=700` |
| qgw | succeeded | 旧 PID 消失 | 18728 存活 | 同上，`watchdog-version=700` |

4. 验证中发现并修复的竞态：新 watchdog 的 monitor loop 可能在发布 `ready` 之前读到 upgrade-request 并把它清掉，导致旧 watchdog 等不到 ready。已在 `handoffMode && !handoffReadySent` 时跳过 upgrade 请求处理。

结论：watchdog 代码已能随升级更新；旧映像在旧进程退出后解锁，后续轮换不再依赖 `.old.last`。`watchdog-version` 可用于运维核对实际运行的 watchdog 版本。
