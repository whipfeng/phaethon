# 内建 SSH 会话继承 worker 环境 & QG 内存耗尽

> 发现日期: 2026-11-09
> 状态: 均已修复并随 `v0.8.0-411-g6a5c5b9` 上线（vm / QG / jf）

## 1. 内建 SSH 会话会派生"没有 watchdog 的 worker"

### 现象

jf 升级完成后只剩 1 个 phaeton 进程：

```
24242  ./phaethon        # environ: PHAETHON_WORKER=1
       ppid = 1
       fd/1 -> .../phaethon-service.log   # 不是 watchdog 管道
```

没有 watchdog、父进程变成 1、stdout 指向日志文件。心跳写永远不会失败，
所以当时的心跳式父进程检测完全失效，孤儿 worker 会一直存活且失去自动重启能力。

### 根因

内建 SSH 服务（`nodeID.phn:22`）运行在 **worker 进程内部**，用户会话由它 fork 外壳，
因此继承了 worker 的全部环境变量，包括 `PHAETHON_WORKER=1`。
在该会话里执行 `./phaethon`，程序按 `main()` 的判断直接进入 worker 模式，
跳过了 watchdog；又因为 stdout 被重定向成文件，心跳管道检测自然不成立。

### 修复（`6a5c5b9`）

- `server/ssh_session.go` 新增 `sessionEnv()`：启动用户会话前剔除 `PHAETHON_WORKER`；
  Unix 的 pty/pipe、Windows 的 pipe/ConPTY 四条路径全部使用它。
- `worker_attr_linux.go` 增加兜底：stdout 是命名管道时仍走心跳写失败检测；
  否则（被直接拉起）每 5s 检查 `getppid()`，父进程消失即退出。

### 运维提醒

不要在内建 SSH 会话所在目录直接执行 `./phaethon` 来"手工重启"：
即使环境已清理，直接前台启动也不会经过 watchdog。请用服务管理器或
`env -u PHAETHON_WORKER setsid nohup ./phaethon ...` 并确认进程数应为 2。

## 2. QG "panic" 实为内存耗尽 + swap 抖动

### 现象

QG 突然表现为"卡死/像 panic"：SSH 连上后 banner 超时、管理口偶发不可达，
但进程仍在、服务状态 started，日志无 panic。

实测（473MB 内存的机器）：

```
Mem:  473MB total, 422MB used, free 9MB
Swap: 945MB total, 892MB used
phaethon worker: VmRSS 260MB + VmSwap 805MB
```

worker 实际占用约 1.06GB，几乎全部压在 swap 上，进程在抖动而无法及时响应。

### 处理

升级到 `v0.8.0-411-g6a5c5b9`（含批次 2 的 D6 包内存修复：出站 `buf.Release()`、
入站零拷贝）并重启后：

```
Mem used: 409MB -> 120MB
Swap used: 915MB -> 119MB
```

### 后续观察

- `/api/mesh` 的 `channelDropStats`、进程 RSS 与 swap 占用应保持平稳；
- 若再次持续增长，按 §8/§10 的取证方式记录增长曲线（哪类流量驱动）。

## 3. 顺带修正

- `scripts/.env` 中 `QG_HOST` 由过期的 `10.11.61.40` 改为 `10.11.61.50`。
