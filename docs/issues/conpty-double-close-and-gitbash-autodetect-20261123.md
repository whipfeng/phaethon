# 内置 SSH：Git Bash 自动探测与 ConPTY 二次关闭崩溃（2026-11-23）

## 现象

1. `ssh Docker@loc.vm.phn`（系统 OpenSSH）交互式 PTY 会话秒断，非交互命令正常。
   根因是系统 OpenSSH 的 `DefaultShell` 配置为 Git Bash，而内置 SSH 的自动探测
   仍优先 pwsh，两者行为不一致。
2. VM 宿主上的 worker 多次在无升级、无 watchdog 退出日志的情况下重启
   （2026-11-23 19:54–19:55 两次），此前一直未能定位。

## 根因

### 1. 自动探测顺序与系统 ssh 不一致

Windows `autoDetectShell()` 旧顺序为 `pwsh → powershell → cmd`，没有 Git Bash。
系统 OpenSSH 已把 `DefaultShell` 设为 Git Bash 的节点上，两种 ssh 入口拿到的
shell 不同，PTY 行为也就不同（Git Bash 在 ConPTY 下可稳定交互，且原生 UTF-8，
无需 chcp 注入）。

### 2. ConPTY 被二次关闭，库内堆损坏

`conptyProc.CloseIO()`（`runSession` 快速关闭灰度的兜底路径）与延迟的
`conptyProc.Kill()` 都会调用 `conpty.ConPty.Close()`。该库的 `Close()` 不是幂等的：
第二次 `ClosePseudoConsole` + 句柄关闭会在库内破坏堆，进程以
`0xC0000374 (STATUS_HEAP_CORRUPTION)` 直接退出，不产生 Go panic 或应用退出日志。

复现证据（同一台 Windows 12 核机器）：

| 场景 | 结果 |
|---|---|
| 单次 `ConPty.Close()` | 进程存活，正常运行 |
| `Kill()` 后 `CloseIO()` | 第二次调用时进程崩溃 |
| `CloseIO()` 后 `Kill()` | 第二次调用时进程崩溃 |
| 修复后两种顺序 | 均存活，测试通过 |

因此任何一次交互式 SSH 会话在“灰度关闭”路径下结束，都可能直接带崩整个
worker；这与此前 VM worker 无日志重启的特征一致。

## 修复（commit 5dbbe0b）

1. `autoDetectShell()` 顺序改为 **Git Bash → pwsh → powershell → cmd**。
   - Git Bash 依次探测：注册表 `SOFTWARE\GitForWindows\InstallPath`（HKLM/HKCU）
     → `ProgramFiles` / `ProgramFiles(x86)` / `ProgramW6432` / `LOCALAPPDATA\Programs`
     下的 `Git\bin\bash.exe` → PATH 上的 `bash`。
   - 排除 `System32\bash.exe`（WSL 启动器，拉起的是 Linux shell，不是 Git Bash）。
   - 用户显式配置的 shell 优先级不变，仍高于自动探测。
2. `conptyProc` 增加 `mu/closed`，`Close()` 幂等；已关闭的会话不再按 PID
   `TerminateProcess`，避免 PID 复用误杀其他进程。
3. 新增单测 `ssh_session_windows_test.go`（探测结果必须是真实存在的 POSIX shell、
   优先 Git Bash、WSL 识别），PTY 手动探针 `ssh_pty_probe_test.go` 改为校验
   命令真实输出行，不再匹配 shell 回显。

## 验证

- 本机（VM）PTY 探针：`PHAE_PTY_PROBE=C:\Program Files\Git\bin\bash.exe`，
  `echo PHAE_PROBE_OK` 真实回显，测试通过。
- `go build ./...`、`go test ./server` 全通过。
- VM 部署 `v0.8.3-51-g5dbbe0b`：
  - 进程稳定（watchdog 11084 / worker 11636，30s 无重启）；
  - 内置 ssh exec：`ssh Docker@vm.phn 'uname -s'` → `MINGW64_NT`；
  - 内置 ssh 交互 PTY：`echo PTY_OK` → 正常回显、`exit` 正常断开；
  - 多轮会话结束后 worker PID 不变（二次关闭崩溃已消除）；
  - QG → VM mesh 可达（`ping 100.212.0.2` 0% 丢包），VM 控制台 302 正常。

## 备注

- 本次为 VM 手工替换二进制部署；`data/state/worker-version` 与 `upgrade-status`
  仍保留上一次自升级的 `v0.8.3-47` 记录（仅状态展示提示，不影响运行）。
- deskw 的内置 ssh 当前 exec 仍走 PowerShell（旧自动探测），部署新版本后会自动
  切到 Git Bash；deskw 由用户自行部署。qgw/gg/mb 未在本次范围内。
