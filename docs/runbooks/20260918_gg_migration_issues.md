# 2026-09-18 GG 迁移与反向连接问题

## 问题 1: 日志路径变更导致误判

### 现象
QG 重启后，在 `/root/phaethon.log` 中看不到反向客户端的启动日志，误以为反向连接没有自动注册。

### 根因
提交 `ac91cec` (feat: rotating log file support) 将日志输出从 stdout/旧日志文件改到了 `.phaethon/phaethon.log`（rotating log）。

- 旧路径: `/root/phaethon.log`
- 新路径: `/root/.phaethon/phaethon.log`

### 教训
部署新版本后，先确认日志路径是否变化。检查 `.phaethon/` 目录。

---

## 问题 2: BindingStore RemoveByReverseID 过于暴力

### 现象
GG 环境的反向连接向导页面只显示 1 个 binding，而 QG 有 6 个反向客户端全部注册成功。

### 根因
提交 `4673ab6` (fix: allow same reverseID to reclaim ports after restart) 引入了 `RemoveByReverseID` 调用。该函数删除同一 reverseID 的**所有** bindings，不区分 seq。

QG 的 6 个反向客户端共享同一个 instance-level reverseID，靠不同的 seq (1-6) 区分。每次新注册都会清掉之前所有的 binding，只有最后一个存活。

### 修复
将 `RemoveByReverseID(req.ReverseID)` 改为 `Remove(req.ReverseID, req.Seq)`，只删除当前 (reverseID, seq) 的 binding。

### 设计要点
- 一个 instance 可以有多个 reverse config，共享 reverseID，不同 seq
- 端口回收靠 `isPortAvailable` 检查，不需要暴力删除所有 bindings
- `RemoveByReverseID` 函数保留但不再在注册流程中使用

---

## 问题 3: GG 环境 SSH 连接不稳定

### 现象
频繁执行 kill 命令后，GG 的 SSH 连接被重置，无法重新连接。

### 原因
可能是远程主机的 SSH 连接限制或防护机制。

### 教训
- 避免短时间内多次 kill 进程
- 使用单条命令完成 stop + replace + start
- 等待足够时间（30-60秒）再重试连接
