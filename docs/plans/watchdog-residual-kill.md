# watchdog 残留进程精确查杀

**创建时间**: 2026-11-07
**状态**: DRAFT（待用户确认）

## 一、问题

`killResidualWorkers`（main_tun_nows.go:99）在 watchdog 启动时扫描 `/proc`，只要 `/proc/<pid>/cmdline` 含 "phaethon" 就杀（排除 supervise-daemon），不分用户、目录、进程类型。

**误杀对象**：
- 部署/验证用的远程 shell：命令行里带 `/home/layer4/phaethon-gg`、`phaethon.log` 等。
- `tail -f phaethon.log`、`vim phaethon.log`、`less data/logs/phaethon.log`。
- 同机另一个实例（GG 上的 `/home/layer4/phaethon`）。
- 任何路径里带 phaethon 的进程（IDE、编译等）。

**证据**：
- 2026-11-07 用新部署脚本部署 JF：验证 shell 的参数里带 `/home/layer4/layer4-deploy … phaethon.log`，新 watchdog 起来 5 秒内这个 shell 就被杀了。改为通过 stdin 传参后，验证正常通过。
- JF 2026-11-02 20:09:26 日志：`watchdog: found 4 residual process(es): [1092 60634 60952 951]`。正常残留最多 2 个（旧 watchdog + 旧 worker），多出的 2 个疑似被误杀（具体是什么进程，当时没有记录）。

## 二、方案

### W1 残留判定收紧（main_tun_nows.go）
只有同时满足下面 3 条的进程才算残留：
1. **同一用户**：`/proc/<pid>/status` 的 Uid 与本进程相同。
2. **同一工作目录**：`readlink /proc/<pid>/cwd` 与本进程 cwd 相同。同目录才会抢同一个数据库和端口；别的目录的实例不是残留。
3. **是 phaethon 程序本身**：`readlink /proc/<pid>/exe` 去掉 ` (deleted)` 后缀后满足以下任一：
   - 与本进程 exe 路径相同；
   - 在 `<cwd>/data/packages/` 下（pkg worker）；
   - 文件名为 `phaethon`、`phaethon-watchdog` 或 `phaethon-v*`。

**不再看 cmdline**。shell、tail、vim 的 exe 是 bash/tail/vim，不会命中。

日志：每个被杀进程记录 `pid exe cwd`，便于审计。

### W2 worker 随 watchdog 退出（Linux）
- `spawnChildProcess` 在 Linux 下设置 `SysProcAttr{Pdeathsig: SIGTERM}`。新增 build-tag 文件 `worker_attr_linux.go` / `worker_attr_other.go`。
- watchdog 被 `kill -9` 时，内核向 worker 发 SIGTERM，worker 走正常退出清理（路由、iptables）。从根上减少孤儿 worker。
- 用 SIGTERM 而不用 SIGKILL：QG 有 bypass-gateway iptables 规则，需要清理。
- darwin 无 Pdeathsig：维持现状（darwin 本来就没有 /proc，W1 也不生效）。

## 三、不做的事
- ❌ pidfile 方案：pid 复用、文件残留，都要额外处理，W1+W2 已够用。
- ❌ Windows：`killResidualWorkers` 在 Windows 本来就是空实现，不动。

## 四、验收
1. GG 上开 `tail -f data/logs/phaethon.log`，再部署 GG：tail 不被杀，日志只出现旧 watchdog 和旧 worker 两个 pid。
2. `kill -9 <watchdog pid>`：worker 在 1 秒内退出，日志有正常退出记录。
3. 用 nohup 直接再起一个实例（模拟重复启动）：旧的两个进程被杀，新实例正常接管。
4. 部署脚本恢复通过命令行传参（可选）也能通过验证。
